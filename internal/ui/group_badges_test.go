package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// A collapsed group row counts the sessions that ask the person something
// and those in error, across its subgroups: "2 ask you · 1 error". An
// expanded group shows its rows instead, so it carries no badges.
func TestRenderGroupItem_CollapsedAttentionBadges(t *testing.T) {
	h := newTintHome([]*session.Instance{
		{ID: "a", Title: "a", GroupPath: "team"},
		{ID: "b", Title: "b", GroupPath: "team/sub"},
		{ID: "c", Title: "c", GroupPath: "team/sub", Status: session.StatusError},
		{ID: "d", Title: "d", GroupPath: "team"},
		{ID: "z", Title: "z", GroupPath: "team", Status: session.StatusError, ArchivedAt: archivedAt()},
	})
	snap := map[string]sessionRenderState{
		"a": {status: session.StatusWaiting, askKind: "pick"},
		"b": {status: session.StatusIdle, askKind: "decision"},
		"c": {status: session.StatusError},
		"d": {status: session.StatusRunning},
	}
	stats := h.buildGroupRenderStats(snap)
	if got := stats["team"]; got.asks != 2 || got.errored != 1 {
		t.Fatalf("team stats asks=%d errored=%d, want 2 and 1", got.asks, got.errored)
	}
	if got := stats["team/sub"]; got.asks != 1 || got.errored != 1 {
		t.Fatalf("sub stats asks=%d errored=%d, want 1 and 1", got.asks, got.errored)
	}

	render := func(path string, width int) string {
		var b strings.Builder
		h.renderGroupItem(&b, session.Item{Type: session.ItemTypeGroup, Group: h.groupTree.Groups[path], Path: path, Level: strings.Count(path, "/")},
			false, 0, stats, width)
		return ansi.Strip(b.String())
	}
	h.groupTree.Groups["team"].Expanded = false
	if row := render("team", 80); !strings.Contains(row, "2 ask you · 1 error") {
		t.Errorf("collapsed row %q lacks the badges", row)
	}
	if row := render("team", 20); strings.Contains(row, "ask you") {
		t.Errorf("badges must drop when they do not fit: %q", row)
	}
	h.groupTree.Groups["team"].Expanded = true
	if row := render("team", 80); strings.Contains(row, "ask you") || strings.Contains(row, "error") {
		t.Errorf("expanded row %q carries badges", row)
	}
	// Nothing to show: no badge text at all.
	quiet := newTintHome([]*session.Instance{{ID: "q", Title: "q", GroupPath: "calm"}})
	quiet.groupTree.Groups["calm"].Expanded = false
	var b strings.Builder
	quiet.renderGroupItem(&b, session.Item{Type: session.ItemTypeGroup, Group: quiet.groupTree.Groups["calm"], Path: "calm"},
		false, 0, quiet.buildGroupRenderStats(map[string]sessionRenderState{}), 80)
	if row := ansi.Strip(b.String()); strings.Contains(row, "ask you") || strings.Contains(row, "error") {
		t.Errorf("quiet row %q carries badges", row)
	}
}
