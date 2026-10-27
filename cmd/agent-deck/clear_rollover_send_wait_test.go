package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// `session send --wait` sent seconds after the person typed /clear. The
// SessionStart(source=clear) hook has written the rollover marker naming the
// new conversation, but the guarded rebind commits only once that
// conversation has data, which is this send's own prompt. The reply lands in
// the NEW transcript only; reading the bound (pre-/clear) transcript timed
// out with "turn identity not established".

type clearSendFixture struct {
	inst                 *session.Instance
	peers                []*session.Instance
	oldID, newID         string
	projectsDir, oldPath string
}

func newClearSendFixture(t *testing.T) clearSendFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	inst := session.NewInstanceWithTool("clear-send", project, "claude")
	fx := clearSendFixture{
		inst:  inst,
		oldID: "b74814e8-0000-4000-8000-00000000c001",
		newID: "e7b13f82-0000-4000-8000-00000000c002",
	}
	fx.projectsDir = filepath.Join(session.GetClaudeConfigDirForInstance(inst), "projects", session.ConvertToClaudeDirName(project))
	if err := os.MkdirAll(fx.projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fx.oldPath = filepath.Join(fx.projectsDir, fx.oldID+".jsonl")
	appendJSONL(t, fx.oldPath,
		clearUser(fx.oldID, "old-u", time.Now().Add(-time.Minute), "before /clear"),
		clearAssistant(fx.oldID, "old-a", "old reply"),
	)
	inst.ClaudeSessionID = fx.oldID
	fx.peers = []*session.Instance{inst}
	t.Cleanup(func() { session.ClearClaudeRolloverMarker(inst.ID) })
	return fx
}

func clearUser(sid, uuid string, ts time.Time, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`,
		uuid, sid, ts.UTC().Format(time.RFC3339Nano), text)
}

func clearAssistant(sid, uuid, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"sessionId":%q,"timestamp":%q,"message":{"id":"m-%s","role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":"end_turn"}}`,
		uuid, sid, time.Now().UTC().Format(time.RFC3339Nano), uuid, text)
}

// sendWait replays handleSessionSend's Claude --wait path: the pre-send
// transcript resolution, the post-send path wait when none existed, then the
// identity/completion/reply helper. claudeWrites stands in for the pane.
func (fx clearSendFixture) sendWait(t *testing.T, prompt string, budget time.Duration, claudeWrites func(sentAt time.Time)) (*session.ResponseOutput, error) {
	t.Helper()
	rolloverID := followClaudeRollover(fx.inst, fx.peers)
	turnPath, err := fx.inst.GetJSONLPathChecked(fx.peers)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if turnPath != "" {
		if cursor, err = session.TranscriptCursor(turnPath); err != nil {
			t.Fatal(err)
		}
	}
	pathKnownBeforeSend := turnPath != ""
	sentAt := time.Now()
	deadline := sentAt.Add(budget)
	go claudeWrites(sentAt)
	if turnPath == "" {
		if turnPath, rolloverID, err = awaitTranscriptPath(fx.inst, fx.inst.ID, "_test", fx.peers, deadline); err != nil {
			return nil, err
		}
	}
	q := session.TurnQuery{Path: turnPath, Prompt: prompt, Cursor: cursor, SessionID: rolloverID}
	if !pathKnownBeforeSend {
		q.NotBefore = sentAt
	}
	resp, _, identityErr, completionErr, responseErr := awaitClaudeWaitReply(q, deadline, func(time.Duration) (string, error) { return "waiting", nil })
	for _, e := range []error{identityErr, completionErr, responseErr} {
		if e != nil {
			return nil, e
		}
	}
	return resp, nil
}

func TestClearRollover_SendWaitFollowsDeclaredSuccessor(t *testing.T) {
	fx := newClearSendFixture(t)
	const prompt = "first message after /clear"
	session.WriteClaudeRolloverMarker(fx.inst.ID, fx.newID, "clear")

	resp, err := fx.sendWait(t, prompt, 5*time.Second, func(time.Time) {
		time.Sleep(50 * time.Millisecond)
		newPath := filepath.Join(fx.projectsDir, fx.newID+".jsonl")
		appendJSONL(t, newPath, clearUser(fx.newID, "new-u", time.Now(), prompt))
		time.Sleep(30 * time.Millisecond)
		appendJSONL(t, newPath, clearAssistant(fx.newID, "new-a", "REPLY IN THE NEW CONVERSATION"))
	})
	if err != nil {
		t.Fatalf("send --wait after /clear: %v", err)
	}
	if resp.Content != "REPLY IN THE NEW CONVERSATION" || resp.SessionID != fx.newID {
		t.Fatalf("resp = %+v, want the new conversation's reply", resp)
	}
}

// A marker naming an id another session is bound to is not this pane's
// successor: the send stays on its own bound transcript and never reads the
// other conversation, so the wait fails rather than return a foreign reply.
func TestClearRollover_SendWaitIgnoresMarkerForUnrelatedID(t *testing.T) {
	fx := newClearSendFixture(t)
	const prompt = "message to the right session"
	peer := session.NewInstanceWithTool("peer", fx.inst.ProjectPath, "claude")
	peer.ClaudeSessionID = fx.newID
	fx.peers = append(fx.peers, peer)
	session.WriteClaudeRolloverMarker(fx.inst.ID, fx.newID, "clear")

	_, err := fx.sendWait(t, prompt, 600*time.Millisecond, func(time.Time) {
		appendJSONL(t, filepath.Join(fx.projectsDir, fx.newID+".jsonl"),
			clearUser(fx.newID, "foreign-u", time.Now(), prompt),
			clearAssistant(fx.newID, "foreign-a", "FOREIGN REPLY"))
	})
	if err == nil || !strings.Contains(err.Error(), "turn identity not established") {
		t.Fatalf("err = %v, want the bound transcript's identity timeout, never the peer's reply", err)
	}
	if fx.inst.ClaudeSessionID != fx.oldID {
		t.Fatalf("ClaudeSessionID = %q, want the bound %q", fx.inst.ClaudeSessionID, fx.oldID)
	}
}

// Following the declaration is no newest-transcript scan: a reply that lands
// in some other transcript of the same folder is never returned.
func TestClearRollover_SendWaitNeverReadsNewestTranscript(t *testing.T) {
	fx := newClearSendFixture(t)
	const prompt = "message after /clear"
	session.WriteClaudeRolloverMarker(fx.inst.ID, fx.newID, "clear")

	_, err := fx.sendWait(t, prompt, 600*time.Millisecond, func(time.Time) {
		appendJSONL(t, filepath.Join(fx.projectsDir, "0a0a0a0a-0000-4000-8000-00000000c0ff.jsonl"),
			clearUser("0a0a0a0a-0000-4000-8000-00000000c0ff", "sib-u", time.Now(), prompt),
			clearAssistant("0a0a0a0a-0000-4000-8000-00000000c0ff", "sib-a", "SIBLING REPLY"))
	})
	if err == nil {
		t.Fatal("send --wait returned a sibling transcript's reply")
	}
}
