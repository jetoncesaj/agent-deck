package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// `session send --wait` must read the target's own bound transcript, like
// `session output`: in a project directory shared by several sessions the
// newest transcript there can be a sibling's reply, so it is never an answer.
func TestWaitForFreshOutput_ReadsOnlyOwnBoundTranscript(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	setFastFreshOutputConfig(t, 300*time.Millisecond)

	projectPath := "/test/strict-shared-cwd"
	projectsDir := filepath.Join(configDir, "projects", session.ConvertToClaudeDirName(projectPath))
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const siblingID, targetID = "aaaaaaaa-0000-4000-8000-00000000000a", "bbbbbbbb-0000-4000-8000-00000000000b"
	// The sibling's transcript is the newest in the shared folder and fresh.
	writeClaudeJSONL(t, projectsDir, siblingID, "q", "SIBLING REPLY", "2026-06-01T12:00:05Z")

	target := session.NewInstance("target", projectPath)
	target.Tool = "claude"
	target.ClaudeSessionID = targetID
	target.LastStartedAt = time.Now().Add(-time.Hour)
	sibling := session.NewInstance("sibling", projectPath)
	sibling.Tool = "claude"
	sibling.ClaudeSessionID = siblingID
	peers := []*session.Instance{target, sibling}
	sentAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Own transcript missing: an error naming the bound id, never the sibling.
	resp, err := waitForFreshOutput(target, sentAt, peers)
	if err == nil {
		t.Fatalf("expected an error without the target's own transcript, got %+v", resp)
	}
	if resp != nil && strings.Contains(resp.Content, "SIBLING REPLY") {
		t.Fatal("send --wait returned the sibling's reply")
	}
	if !strings.Contains(err.Error(), targetID) {
		t.Fatalf("error %q should name the bound claude_session_id", err)
	}
	if target.ClaudeSessionID != targetID {
		t.Fatalf("ClaudeSessionID moved to %q", target.ClaudeSessionID)
	}

	// Own transcript present (older than the sibling's): that is the answer.
	writeClaudeJSONL(t, projectsDir, targetID, "q", "TARGET REPLY", "2026-06-01T12:00:01Z")
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(projectsDir, targetID+".jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	resp, err = waitForFreshOutput(target, sentAt, peers)
	if err != nil {
		t.Fatalf("waitForFreshOutput: %v", err)
	}
	if resp.Content != "TARGET REPLY" {
		t.Fatalf("send --wait = %q, want the target's own reply", resp.Content)
	}
}

// `session show` prints the conversation id the /clear rebind replaced, in
// text and in --json (claude_previous_session_id).
func TestSessionShow_PrintsPreviousClaudeSessionID(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	id := sessionMoveAddSession(t, home, filepath.Join(home, "proj"), "rebound")
	const oldID, newID = "b74814e8-0000-4000-8000-000000000001", "e7b13f82-0000-4000-8000-000000000002"

	db, err := statedb.Open(filepath.Join(home, ".local", "share", "agent-deck", "profiles", "ch_support_test", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteClaudeSessionRebinding(id, newID, oldID, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	stdout, stderr, code := runAgentDeck(t, home, "session", "show", id, "--json")
	if code != 0 {
		t.Fatalf("session show --json failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var shown map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
		t.Fatalf("parse show: %v\n%s", err, stdout)
	}
	if shown["claude_session_id"] != newID || shown["claude_previous_session_id"] != oldID {
		t.Fatalf("show --json claude ids = %v / %v, want %s / %s", shown["claude_session_id"], shown["claude_previous_session_id"], newID, oldID)
	}

	stdout, stderr, code = runAgentDeck(t, home, "session", "show", id)
	if code != 0 {
		t.Fatalf("session show failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "previous_session_id="+oldID) {
		t.Fatalf("session show text lacks the previous id:\n%s", stdout)
	}
}
