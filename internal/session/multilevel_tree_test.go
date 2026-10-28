package session

import (
	"fmt"
	"strings"
	"testing"
)

// treeRows renders a flat item list as "title@level/depth[gutter]" per
// session row, so a test can compare the whole shape at once.
func treeRows(items []Item) string {
	var parts []string
	for _, it := range items {
		if it.Type != ItemTypeSession {
			continue
		}
		g := ""
		for _, last := range it.TreeGutter {
			if last {
				g += "_"
			} else {
				g += "|"
			}
		}
		parts = append(parts, fmt.Sprintf("%s@%d/%d[%s]", it.Session.Title, it.Level, it.TreeDepth, g))
	}
	return strings.Join(parts, " ")
}

func multiLevelTree() *GroupTree {
	return NewGroupTree([]*Instance{
		{ID: "top", Title: "top", GroupPath: "proj"},
		{ID: "epic", Title: "epic", GroupPath: "proj", ParentSessionID: "top"},
		{ID: "w1", Title: "w1", GroupPath: "proj", ParentSessionID: "epic"},
		{ID: "w2", Title: "w2", GroupPath: "proj", ParentSessionID: "epic"},
		{ID: "h1", Title: "h1", GroupPath: "proj", ParentSessionID: "w1"},
		{ID: "solo", Title: "solo", GroupPath: "proj", ParentSessionID: "top"},
		{ID: "other", Title: "other", GroupPath: "proj"},
	})
}

// Flatten nests at every depth: each row follows its parent, one Level and
// one TreeDepth step per layer, with a gutter column per ancestor.
func TestFlatten_NestsAtEveryDepth(t *testing.T) {
	got := treeRows(multiLevelTree().Flatten())
	want := "top@1/0[] epic@2/1[|] w1@3/2[||] h1@4/3[|||] w2@3/2[||] solo@2/1[|] other@1/0[]"
	if got != want {
		t.Fatalf("Flatten =\n  %s\nwant\n  %s", got, want)
	}
}

// RecomputeTreeConnectors on the visible list sets the gutter from the
// visible siblings: w2 closes epic's list, solo closes top's, other closes
// the group, so their columns go blank below them.
func TestRecomputeTreeConnectors_MultiLevelGutter(t *testing.T) {
	items := RecomputeTreeConnectors(multiLevelTree().Flatten())
	got := treeRows(items)
	want := "top@1/0[] epic@2/1[|] w1@3/2[||] h1@4/3[|||] w2@3/2[||] solo@2/1[|] other@1/0[]"
	if got != want {
		t.Fatalf("connectors =\n  %s\nwant\n  %s", got, want)
	}
	last := map[string]bool{}
	for _, it := range items {
		if it.Type == ItemTypeSession {
			last[it.Session.ID] = it.IsLastSubSession
		}
	}
	for id, want := range map[string]bool{"epic": false, "w1": false, "h1": true, "w2": true, "solo": true} {
		if last[id] != want {
			t.Fatalf("IsLastSubSession(%s) = %v, want %v", id, last[id], want)
		}
	}

	// Drop "other": top is now the group's last top-level row, so the first
	// column goes blank for every row below it.
	var trimmed []Item
	for _, it := range multiLevelTree().Flatten() {
		if it.Session == nil || it.Session.ID != "other" {
			trimmed = append(trimmed, it)
		}
	}
	got = treeRows(RecomputeTreeConnectors(trimmed))
	want = "top@1/0[] epic@2/1[_] w1@3/2[_|] h1@4/3[_||] w2@3/2[_|] solo@2/1[_]"
	if got != want {
		t.Fatalf("connectors without other =\n  %s\nwant\n  %s", got, want)
	}
}

// A row whose parent is not on screen (filtered out) draws at the depth of
// the nearest visible ancestor chain, moving left with its subtree.
func TestRecomputeTreeConnectors_FilteredParentLiftsTheSubtree(t *testing.T) {
	var items []Item
	for _, it := range multiLevelTree().Flatten() {
		if it.Session == nil || it.Session.ID != "epic" {
			items = append(items, it)
		}
	}
	got := treeRows(RecomputeTreeConnectors(items))
	want := "top@1/0[] w1@1/0[] h1@2/1[|] w2@1/0[] solo@2/1[|] other@1/0[]"
	if got != want {
		t.Fatalf("connectors without epic =\n  %s\nwant\n  %s", got, want)
	}
}

// Folding a nested row hides its whole subtree and nothing else; the hidden
// rows do not come back as orphans.
func TestFlattenWithParentFolds_NestedFold(t *testing.T) {
	tree := multiLevelTree()
	if got, want := treeRows(tree.FlattenWithParentFolds(map[string]bool{"epic": true})),
		"top@1/0[] epic@2/1[|] solo@2/1[|] other@1/0[]"; got != want {
		t.Fatalf("fold epic =\n  %s\nwant\n  %s", got, want)
	}
	if got, want := treeRows(tree.FlattenWithParentFolds(map[string]bool{"w1": true})),
		"top@1/0[] epic@2/1[|] w1@3/2[||] w2@3/2[||] solo@2/1[|] other@1/0[]"; got != want {
		t.Fatalf("fold w1 =\n  %s\nwant\n  %s", got, want)
	}
	if got, want := treeRows(tree.FlattenWithParentFolds(map[string]bool{"top": true})),
		"top@1/0[] other@1/0[]"; got != want {
		t.Fatalf("fold top =\n  %s\nwant\n  %s", got, want)
	}
}

// A parent in another group leaves the row at top level here, with its own
// subtree still nested under it; a parent cycle inside the group drops no
// row.
func TestFlatten_OrphanSubtreesAndCyclesKeepEveryRow(t *testing.T) {
	tree := NewGroupTree([]*Instance{
		{ID: "lead", Title: "lead", GroupPath: "a"},
		{ID: "o", Title: "o", GroupPath: "b", ParentSessionID: "lead"},
		{ID: "oc", Title: "oc", GroupPath: "b", ParentSessionID: "o"},
		{ID: "x", Title: "x", GroupPath: "b", ParentSessionID: "y"},
		{ID: "y", Title: "y", GroupPath: "b", ParentSessionID: "x"},
	})
	var b []Item
	for _, it := range tree.Flatten() {
		if it.Path == "b" {
			b = append(b, it)
		}
	}
	got := treeRows(b)
	if !strings.HasPrefix(got, "o@1/0[] oc@2/1[") {
		t.Fatalf("orphan subtree = %s, want o at top level with oc under it", got)
	}
	for _, id := range []string{"x@", "y@"} {
		if !strings.Contains(got, id) {
			t.Fatalf("cycle row %s dropped: %s", id, got)
		}
	}
	if n := strings.Count(got, "@"); n != 4 {
		t.Fatalf("group b has %d rows, want 4: %s", n, got)
	}
}
