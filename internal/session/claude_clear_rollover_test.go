package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// /clear tracking. When a person types /clear, Claude Code starts a new
// conversation in the same process and fires SessionStart with source
// "clear" and the new session_id. The hook handler records that declaration
// in the rollover marker; the binding must then follow the new conversation
// even inside the #661 flap window (old transcript written moments ago, new
// one smaller), where the size/mtime proxy alone keeps the old id. Observed
// in the field: the live conversation was e7b13f82-... while the pane's
// CLAUDE_SESSION_ID and DB record stayed at the pre-/clear b74814e8-....

type clearRolloverFixture struct {
	inst         *Instance
	oldID, newID string
}

// setupClearRollover seeds a rich pre-/clear transcript written 1s ago and a
// small post-/clear transcript written now: inside clearRebindMtimeGrace, so
// without the marker the existing heuristic rejects the candidate.
func setupClearRollover(t *testing.T) clearRolloverFixture {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmpHome, ".claude"))
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)

	projectPath := filepath.Join(tmpHome, "project")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	inst := NewInstanceWithTool("clear-rollover", projectPath, "claude")
	fx := clearRolloverFixture{
		inst:  inst,
		oldID: "b74814e8-0000-4000-8000-000000000001",
		newID: "e7b13f82-0000-4000-8000-000000000002",
	}
	oldPath := seedClaudeJSONL(t, inst, fx.oldID, 200, 1024)
	newPath := seedClaudeJSONL(t, inst, fx.newID, 1, 8)
	now := time.Now()
	oldMtime := now.Add(-time.Second)
	if err := os.Chtimes(oldPath, oldMtime, oldMtime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	if err := os.Chtimes(newPath, now, now); err != nil {
		t.Fatalf("chtimes new: %v", err)
	}
	inst.ClaudeSessionID = fx.oldID
	return fx
}

func (fx clearRolloverFixture) hook(event, sessionID string) {
	fx.inst.UpdateHookStatus(&HookStatus{
		Status:    "waiting",
		SessionID: sessionID,
		Event:     event,
		UpdatedAt: time.Now(),
	})
}

func TestClearRollover_SessionStartClearRebindsAndKeepsPrevious(t *testing.T) {
	fx := setupClearRollover(t)
	WriteClaudeRolloverMarker(fx.inst.ID, fx.newID, "clear")

	fx.hook("Stop", fx.newID)

	if fx.inst.ClaudeSessionID != fx.newID {
		t.Fatalf("ClaudeSessionID = %q, want the post-/clear %q: a SessionStart(source=clear) from this pane must move the binding", fx.inst.ClaudeSessionID, fx.newID)
	}
	if fx.inst.PreviousClaudeSessionID != fx.oldID {
		t.Fatalf("PreviousClaudeSessionID = %q, want %q (the conversation /clear left)", fx.inst.PreviousClaudeSessionID, fx.oldID)
	}
	found := false
	for _, ev := range readLifecycleEvents(t) {
		if ev.Action == "rebind" && ev.OldID == fx.oldID && ev.NewID == fx.newID && ev.Reason == "session_start_clear" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected rebind lifecycle event old=%s new=%s reason=session_start_clear; got %+v", fx.oldID, fx.newID, readLifecycleEvents(t))
	}
}

// Control: the same shape without the declaration is the #661 flap and must
// keep rejecting. The marker relaxes nothing else.
func TestClearRollover_WithoutMarkerStillRejectsFlap(t *testing.T) {
	fx := setupClearRollover(t)
	fx.hook("Stop", fx.newID)
	if fx.inst.ClaudeSessionID != fx.oldID {
		t.Fatalf("ClaudeSessionID = %q, want %q kept: no SessionStart(clear) was declared", fx.inst.ClaudeSessionID, fx.oldID)
	}
	if !hasRejectReason(readLifecycleEvents(t), "candidate_has_less_conversation_data") {
		t.Fatal("expected candidate_has_less_conversation_data reject")
	}
}

// A marker naming some other id is no evidence for this candidate.
func TestClearRollover_MarkerForOtherIDDoesNotRebind(t *testing.T) {
	fx := setupClearRollover(t)
	WriteClaudeRolloverMarker(fx.inst.ID, "00000000-0000-4000-8000-0000000000ff", "clear")
	fx.hook("Stop", fx.newID)
	if fx.inst.ClaudeSessionID != fx.oldID {
		t.Fatalf("ClaudeSessionID = %q, want %q kept", fx.inst.ClaudeSessionID, fx.oldID)
	}
}

// The no-conversation-data guard (foreign `claude -p` ephemerals) is kept:
// a declared successor without a transcript yet waits for its first turn.
func TestClearRollover_NoConversationDataGuardKept(t *testing.T) {
	fx := setupClearRollover(t)
	emptyID := "e7b13f82-0000-4000-8000-0000000000ee"
	WriteClaudeRolloverMarker(fx.inst.ID, emptyID, "clear")
	fx.hook("SessionStart", emptyID)
	if fx.inst.ClaudeSessionID != fx.oldID {
		t.Fatalf("ClaudeSessionID = %q, want %q kept until the new conversation has data", fx.inst.ClaudeSessionID, fx.oldID)
	}
	if !hasRejectReason(readLifecycleEvents(t), "candidate_has_no_conversation_data") {
		t.Fatal("expected candidate_has_no_conversation_data reject")
	}
}

// startup/resume SessionStarts clear the marker; only clear/compact write it.
func TestClaudeRolloverMarker_Lifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const inst = "marker-inst"
	WriteClaudeRolloverMarker(inst, "11111111-1111-4111-8111-111111111111", "clear")
	if id, src := readClaudeRolloverMarker(inst); id != "11111111-1111-4111-8111-111111111111" || src != "clear" {
		t.Fatalf("after clear: id=%q src=%q", id, src)
	}
	WriteClaudeRolloverMarker(inst, "22222222-2222-4222-8222-222222222222", "resume")
	if id, _ := readClaudeRolloverMarker(inst); id != "" {
		t.Fatalf("a resume SessionStart must remove the marker, still %q", id)
	}
	WriteClaudeRolloverMarker(inst, "33333333-3333-4333-8333-333333333333", "compact")
	if id, src := readClaudeRolloverMarker(inst); id != "33333333-3333-4333-8333-333333333333" || src != "compact" {
		t.Fatalf("after compact: id=%q src=%q", id, src)
	}
	WriteClaudeRolloverMarker(inst, "44444444-4444-4444-8444-444444444444", "startup")
	if id, _ := readClaudeRolloverMarker(inst); id != "" {
		t.Fatalf("a startup SessionStart must remove the marker, still %q", id)
	}
}

// A send right after /clear: SessionStart(clear) declared the successor, but
// its transcript has no data yet, so the guarded rebind has not committed.
// The sender follows the declaration for its turn; the rebind itself still
// waits for the hook path.
func TestPendingClaudeRollover_FollowsDeclaredSuccessorOnly(t *testing.T) {
	fx := setupClearRollover(t)
	pendingID := "e7b13f82-0000-4000-8000-0000000000aa"
	peer := NewInstanceWithTool("peer", fx.inst.ProjectPath, "claude")
	peer.ClaudeSessionID = "0f0f0f0f-0000-4000-8000-0000000000bb"
	peers := []*Instance{fx.inst, peer}

	if id, ok := fx.inst.PendingClaudeRollover(peers); ok {
		t.Fatalf("no marker: followed %q", id)
	}

	WriteClaudeRolloverMarker(fx.inst.ID, pendingID, "clear")
	if id, ok := fx.inst.PendingClaudeRollover(peers); !ok || id != pendingID {
		t.Fatalf("PendingClaudeRollover = %q,%v; want the declared %q", id, ok, pendingID)
	}
	if fx.inst.ClaudeSessionID != fx.oldID {
		t.Fatalf("PendingClaudeRollover must not rebind; ClaudeSessionID = %q", fx.inst.ClaudeSessionID)
	}

	// Unrelated ids are never followed.
	for name, write := range map[string]func(){
		"bound id itself":          func() { WriteClaudeRolloverMarker(fx.inst.ID, fx.oldID, "clear") },
		"id bound to another peer": func() { WriteClaudeRolloverMarker(fx.inst.ID, peer.ClaudeSessionID, "clear") },
		"not a file name":          func() { WriteClaudeRolloverMarker(fx.inst.ID, "../"+pendingID, "clear") },
		"glob id":                  func() { WriteClaudeRolloverMarker(fx.inst.ID, "e7b13f82-*", "clear") },
		"resume clears it":         func() { WriteClaudeRolloverMarker(fx.inst.ID, pendingID, "resume") },
		"another instance's marker": func() {
			ClearClaudeRolloverMarker(fx.inst.ID)
			WriteClaudeRolloverMarker(peer.ID, pendingID, "clear")
		},
	} {
		write()
		if id, ok := fx.inst.PendingClaudeRollover(peers); ok {
			t.Errorf("%s: followed %q", name, id)
		}
	}
}

// TurnQuery.SessionID: only a record Claude stamped with the followed
// conversation is adopted.
func TestTurnIdentity_SessionIDGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"user","uuid":"u-other","sessionId":"other","timestamp":"2026-10-27T12:00:00Z","message":{"role":"user","content":"hi"}}
{"type":"user","uuid":"u-none","timestamp":"2026-10-27T12:00:01Z","message":{"role":"user","content":"hi"}}
{"type":"user","uuid":"u-mine","sessionId":"mine","timestamp":"2026-10-27T12:00:02Z","message":{"role":"user","content":"hi"}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := AwaitTurnIdentity(TurnQuery{Path: path, Prompt: "hi", SessionID: "mine"}, 200*time.Millisecond, 10*time.Millisecond)
	if err != nil || id.UUID != "u-mine" {
		t.Fatalf("id=%+v err=%v; want u-mine", id, err)
	}
	if _, err := AwaitTurnIdentity(TurnQuery{Path: path, Prompt: "hi", SessionID: "absent"}, 100*time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("a record of another conversation was adopted")
	}
}
