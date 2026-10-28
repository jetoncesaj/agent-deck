package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// "Asks you": a session that asks the person something shows "?<kind>" on
// its row; one that asks nothing shows no marker.
func TestRenderSessionItem_AsksMarker(t *testing.T) {
	h := NewHome()
	h.width, h.height = 240, 40
	row := func(inst *session.Instance, askKind string) string {
		var b strings.Builder
		snap := map[string]sessionRenderState{inst.ID: {status: inst.Status, tool: "claude", title: inst.Title, askKind: askKind}}
		h.renderSessionItem(&b, session.Item{Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "work", IsLastInGroup: true}, false, snap, 240)
		return ansi.Strip(b.String())
	}
	inst := &session.Instance{ID: "a", Title: "asker", Tool: "claude", Status: session.StatusWaiting}
	if got := row(inst, "pick"); !strings.Contains(got, "asker") || !strings.Contains(got, " ?pick") {
		t.Errorf("asking row lacks the ?pick marker: %q", got)
	}
	if got := row(inst, ""); strings.Contains(got, "?") {
		t.Errorf("a row that asks nothing carries a marker: %q", got)
	}
}

// The render snapshot reads every open ask in one pass.
func TestRefreshSessionRenderSnapshot_CarriesAskKind(t *testing.T) {
	t.Cleanup(func() { session.RemoveAsk("asker-1") })
	if _, _, err := session.SetAsk("_test", "asker-1", session.PersonAsk{Kind: "approval", Question: "Merge?"}); err != nil {
		t.Fatal(err)
	}
	h := NewHome()
	h.refreshSessionRenderSnapshot([]*session.Instance{
		{ID: "asker-1", Title: "asker", Tool: "claude"},
		{ID: "quiet-1", Title: "quiet", Tool: "claude"},
	})
	snap := h.getSessionRenderSnapshot()
	if snap["asker-1"].askKind != "approval" || snap["quiet-1"].askKind != "" {
		t.Fatalf("askKind: asker=%q quiet=%q", snap["asker-1"].askKind, snap["quiet-1"].askKind)
	}
}
