package main

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// [parent_wakes] mode = "escalation": the turn-start fleet snapshot is
// status delivered into the parent's conversation, so it leaves those
// children out; "all" keeps listing them.
func TestParentWakes_FleetSnapshotLeavesEscalationChildrenOut(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		listed bool
	}{
		{session.ParentWakesAll, true},
		{session.ParentWakesEscalation, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cliInboxTestHome(t)
			registerIssue2834Fleet(t, "parent-pw")
			t.Cleanup(session.SetParentWakePolicyForTest(session.ParentWakePolicy{Mode: tc.mode}))
			got := renderChildrenContextSummary("parent-pw")
			if listed := strings.HasPrefix(got, "[agent-deck fleet] 1 children:"); listed != tc.listed {
				t.Fatalf("listed = %v, want %v: %q", listed, tc.listed, got)
			}
		})
	}
}
