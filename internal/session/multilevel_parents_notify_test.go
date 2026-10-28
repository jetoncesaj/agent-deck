package session

import (
	"strings"
	"testing"
)

// threeLevelFixture puts a top-level session above the turn fixture, so the
// fixture's parent becomes the middle layer: top -> middle (f.parent) ->
// worker (f.child). The worker's turns are what the tests produce.
func threeLevelFixture(t *testing.T) (*turnTestFixture, *Instance) {
	t.Helper()
	f := newTurnTestFixture(t)
	top := NewInstanceWithTool("conductor-top", f.parent.ProjectPath, "claude")
	top.ID = "top-multilevel"
	top.Status = StatusWaiting
	f.parent.ParentSessionID = top.ID
	storage, err := NewStorageWithProfile("default")
	if err != nil {
		t.Fatalf("NewStorageWithProfile: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.SaveWithGroups([]*Instance{top, f.parent, f.child}, nil); err != nil {
		t.Fatalf("SaveWithGroups: %v", err)
	}
	f.byID[top.ID] = top
	return f, top
}

// A worker's done line and its NEED: line reach its direct parent, the
// middle layer, and never the top: no layer is skipped, in either
// [parent_wakes] mode.
func TestMultiLevelParents_WorkerTurnsReachTheMiddleLayerOnly(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"done line", "===AGENTDECK_DONE=== status=ok summary=item landed", "item landed"},
		{"NEED line", "Two options.\nNEED: merge or hold?", "NEED: merge or hold?"},
	} {
		for _, mode := range []string{ParentWakesAll, ParentWakesEscalation} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				f, top := threeLevelFixture(t)
				useParentWakes(t, mode)
				f.appendTurn(t, fxHuman("u0", "[agent-deck from:"+f.parent.ID+"] go"), fxAssistantText("a0", tc.text))
				var woken []string
				f.d.notifier.wake.send = func(target *Instance, _, _ string) error {
					woken = append(woken, target.ID)
					return nil
				}
				// Only the worker's turn is observed in this pass, so anything
				// that reaches the top came from the worker.
				statuses := map[string]string{f.child.ID: "waiting"}
				f.d.recordTerminalTurns("default", f.byID, statuses, nil)

				got := f.inboxRecords(t)
				if len(got) != 1 {
					t.Fatalf("middle layer inbox = %d records, want 1: %+v", len(got), got)
				}
				rec := got[0]
				if rec.TargetSessionID != f.parent.ID || rec.ChildSessionID != f.child.ID {
					t.Fatalf("record target=%s child=%s, want the middle layer %s and the worker %s", rec.TargetSessionID, rec.ChildSessionID, f.parent.ID, f.child.ID)
				}
				if !strings.Contains(rec.Text+" "+rec.DoneSummary, tc.want) {
					t.Fatalf("record %+v lacks %q", rec, tc.want)
				}
				if len(woken) != 1 || woken[0] != f.parent.ID {
					t.Fatalf("woken %v, want the middle layer %s once", woken, f.parent.ID)
				}
				if topRecs, err := ReadInboxEvents(top.ID); err != nil || len(topRecs) != 0 {
					t.Fatalf("top inbox = %+v (err %v), want nothing: a layer was skipped", topRecs, err)
				}
			})
		}
	}
}

// The middle layer's own escalation goes one step up, to the top.
func TestMultiLevelParents_MiddleLayerEscalatesToTheTop(t *testing.T) {
	f, top := threeLevelFixture(t)
	byID := f.byID
	if p := resolveParentNotificationTarget(f.parent, byID); p == nil || p.ID != top.ID {
		t.Fatalf("middle layer's target = %v, want the top", p)
	}
	if p := resolveParentNotificationTarget(f.child, byID); p == nil || p.ID != f.parent.ID {
		t.Fatalf("worker's target = %v, want the middle layer", p)
	}
	// A conductor-titled middle layer with a parent is delivered, not
	// suppressed as a top-level conductor would be.
	if isSelfSuppressedConductor(f.parent) {
		t.Fatal("a parented conductor-titled middle layer must not be self-suppressed")
	}
	if !escalationCandidate(f.parent) {
		t.Fatal("the middle layer can escalate to the top")
	}
}
