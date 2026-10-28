package ui

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// multiLevelRenderHome builds top -> epic -> {w1 -> h1, w2}, then "last" as
// a second top-level row, all in one group.
func multiLevelRenderHome() *Home {
	h := seamBNewHome()
	h.parentChildrenCollapsed = make(map[string]bool)
	mk := func(id, parent string) *session.Instance {
		inst := session.NewInstanceWithTool(id, "/tmp/"+id, "claude")
		inst.ID = id
		inst.GroupPath = "alpha"
		inst.Status = session.StatusIdle
		if parent != "" {
			inst.SetParent(parent)
		}
		return inst
	}
	h.groupTree = session.NewGroupTree([]*session.Instance{
		mk("top", ""), mk("epic", "top"), mk("w1", "epic"), mk("h1", "w1"), mk("w2", "epic"), mk("last", ""),
	})
	h.width, h.height = 100, 30
	h.rebuildFlatItems()
	return h
}

// rowPrefix is the text of the row for title up to the title itself.
func rowPrefix(t *testing.T, frame, title string) string {
	t.Helper()
	for _, line := range strings.Split(frame, "\n") {
		if i := strings.Index(line, " "+title+" "); i >= 0 {
			return line[:i]
		}
		if strings.HasSuffix(line, " "+title) {
			return strings.TrimSuffix(line, title)
		}
	}
	t.Fatalf("no row for %q in\n%s", title, frame)
	return ""
}

// Every layer indents one step under its parent, and each ancestor that
// still has siblings below draws its continuation line in its own column.
func TestRenderSessionList_ThreeLevelsIndentAndContinue(t *testing.T) {
	h := multiLevelRenderHome()
	frame := stripAnsi(h.renderSessionList(100, 30))

	top := rowPrefix(t, frame, "top")
	epic := rowPrefix(t, frame, "epic")
	w1 := rowPrefix(t, frame, "w1")
	h1 := rowPrefix(t, frame, "h1")
	w2 := rowPrefix(t, frame, "w2")
	width := func(s string) int { return len([]rune(s)) }
	if !(width(top) < width(epic) && width(epic) < width(w1) && width(w1) < width(h1)) {
		t.Fatalf("rows do not indent one step per layer:\ntop  %q\nepic %q\nw1   %q\nh1   %q\n%s", top, epic, w1, h1, frame)
	}
	if width(w2) != width(w1) {
		t.Fatalf("siblings w1 and w2 sit at different depths: %q vs %q", w1, w2)
	}
	// h1 sits under w1, under epic, under top. top has a sibling below
	// ("last") and w1 has one (w2), so both columns carry a │; epic is the
	// only child of top, so its column is blank; h1 closes w1's list.
	if got := strings.Count(h1, "│"); got != 2 {
		t.Fatalf("h1 prefix %q has %d continuation lines, want 2 (top and w1)\n%s", h1, got, frame)
	}
	if !strings.Contains(h1, "└─") {
		t.Fatalf("h1 is w1's last child but its prefix %q has no └─", h1)
	}
	if !strings.Contains(w2, "└─") || !strings.Contains(w1, "├─") {
		t.Fatalf("epic's children connectors: w1 %q, w2 %q", w1, w2)
	}
}

// Folding the middle layer hides its whole subtree in the rendered list.
func TestRenderSessionList_FoldedMiddleLayerHidesItsSubtree(t *testing.T) {
	h := multiLevelRenderHome()
	h.parentChildrenCollapsed["epic"] = true
	h.rebuildFlatItems()
	frame := stripAnsi(h.renderSessionList(100, 30))
	for _, hidden := range []string{" w1", " h1", " w2"} {
		if strings.Contains(frame, hidden+" ") || strings.Contains(frame, hidden+"\n") {
			t.Fatalf("folded epic still shows%s:\n%s", hidden, frame)
		}
	}
	if !strings.Contains(frame, "epic") || !strings.Contains(frame, "last") {
		t.Fatalf("fold hid more than the subtree:\n%s", frame)
	}
}
