package session

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// chainFixture: top -> mid -> leaf, top -> sib, and a loner.
func chainFixture() (top, mid, leaf, sib, loner *Instance, all []*Instance) {
	mk := func(id, parent string) *Instance {
		inst := &Instance{ID: id, Title: id}
		inst.ParentSessionID = parent
		return inst
	}
	top = mk("top", "")
	mid = mk("mid", "top")
	leaf = mk("leaf", "mid")
	sib = mk("sib", "top")
	loner = mk("loner", "")
	return top, mid, leaf, sib, loner, []*Instance{top, mid, leaf, sib, loner}
}

func chainIDs(insts []*Instance) string {
	var out []string
	for _, i := range insts {
		out = append(out, i.ID)
	}
	return strings.Join(out, ",")
}

func TestParentChain_WalksUpOneHopAtATime(t *testing.T) {
	top, mid, leaf, _, loner, all := chainFixture()
	if got := chainIDs(ParentChain(leaf, all)); got != "mid,top" {
		t.Fatalf("chain of leaf = %q, want mid,top (direct parent first)", got)
	}
	if got := chainIDs(ParentChain(mid, all)); got != "top" {
		t.Fatalf("chain of mid = %q", got)
	}
	if len(ParentChain(top, all)) != 0 || len(ParentChain(loner, all)) != 0 {
		t.Fatal("a top-level session has an empty chain")
	}
	for inst, want := range map[*Instance]int{top: 0, mid: 1, leaf: 2, loner: 0} {
		if got := ParentDepth(inst, all); got != want {
			t.Fatalf("depth of %s = %d, want %d", inst.ID, got, want)
		}
	}
}

func TestParentChain_StopsAtAMissingParentAndACycle(t *testing.T) {
	dangling := &Instance{ID: "d", Title: "d", ParentSessionID: "gone"}
	if got := ParentDepth(dangling, []*Instance{dangling}); got != 0 {
		t.Fatalf("a link to a missing parent counts %d, want 0", got)
	}
	a := &Instance{ID: "a", ParentSessionID: "b"}
	b := &Instance{ID: "b", ParentSessionID: "a"}
	self := &Instance{ID: "s", ParentSessionID: "s"}
	all := []*Instance{a, b, self}
	if got := chainIDs(ParentChain(a, all)); got != "b" {
		t.Fatalf("chain through a cycle = %q, want it cut at b", got)
	}
	if len(ParentChain(self, all)) != 0 {
		t.Fatal("a self-link is not a parent")
	}
	if d := Descendants("a", all, 0); len(d) != 1 || d[0].Instance != b {
		t.Fatalf("descendants through a cycle = %v, want only b", d)
	}
}

func TestDescendants_DepthFirstWithDepthsAndLimit(t *testing.T) {
	_, _, _, _, _, all := chainFixture()
	var got []string
	for _, d := range Descendants("top", all, 0) {
		got = append(got, d.Instance.ID+":"+string(rune('0'+d.Depth)))
	}
	if strings.Join(got, ",") != "mid:1,leaf:2,sib:1" {
		t.Fatalf("descendants of top = %v, want mid:1,leaf:2,sib:1 (each child followed by its subtree)", got)
	}
	if d := Descendants("top", all, 1); len(d) != 2 || d[0].Instance.ID != "mid" || d[1].Instance.ID != "sib" {
		t.Fatalf("maxDepth 1 = %v, want the direct children only", d)
	}
	top, mid, leaf, _, _, _ := chainFixture()
	all = []*Instance{top, mid, leaf}
	if h := SubtreeHeight(top, all); h != 2 {
		t.Fatalf("height of top = %d, want 2", h)
	}
	if h := SubtreeHeight(leaf, all); h != 0 {
		t.Fatalf("height of a leaf = %d, want 0", h)
	}
}

func TestValidateParentLink_RefusesSelfAndCycles(t *testing.T) {
	top, mid, leaf, sib, loner, all := chainFixture()
	var ple *ParentLinkError
	if err := ValidateParentLink(top, top, all, 0); !errors.As(err, &ple) || ple.Kind != ParentLinkSelf {
		t.Fatalf("self link: %v, want the self error", err)
	}
	for _, under := range []*Instance{mid, leaf} {
		err := ValidateParentLink(top, under, all, 0)
		if !errors.As(err, &ple) || ple.Kind != ParentLinkCycle || !strings.Contains(err.Error(), "its own ancestor") {
			t.Fatalf("top under its descendant %s: %v, want the cycle error", under.ID, err)
		}
	}
	// Moving a subtree sideways or up is fine, and so is the old refusal
	// case: a session that already has children becoming a child.
	for _, tc := range [][2]*Instance{{leaf, sib}, {mid, loner}, {mid, sib}, {leaf, top}} {
		if err := ValidateParentLink(tc[0], tc[1], all, 0); err != nil {
			t.Fatalf("%s under %s: %v, want allowed", tc[0].ID, tc[1].ID, err)
		}
	}
}

func TestValidateParentLink_DepthCapCountsTheMovedSubtree(t *testing.T) {
	top, mid, leaf, sib, loner, all := chainFixture()
	var ple *ParentLinkError
	// New sessions (child nil): under leaf (depth 2) a new one is at 3, the
	// default cap; under a session at 3 it would be 4.
	if err := ValidateParentLink(nil, leaf, all, 0); err != nil {
		t.Fatalf("new session under depth 2 with the default cap: %v", err)
	}
	deep := &Instance{ID: "deep", Title: "deep", ParentSessionID: "leaf"}
	all2 := append(append([]*Instance{}, all...), deep)
	err := ValidateParentLink(nil, deep, all2, 0)
	if !errors.As(err, &ple) || ple.Kind != ParentLinkDepth || !strings.Contains(err.Error(), "max_parent_depth = 3") {
		t.Fatalf("new session at depth 4: %v, want the depth error naming the cap", err)
	}
	// mid has one level below it (leaf): under sib (depth 1) mid lands at 2
	// and leaf at 3: allowed by 3, refused by 2.
	if err := ValidateParentLink(mid, sib, all, 3); err != nil {
		t.Fatalf("mid under sib with cap 3: %v", err)
	}
	err = ValidateParentLink(mid, sib, all, 2)
	if !errors.As(err, &ple) || ple.Kind != ParentLinkDepth || !strings.Contains(err.Error(), "down to depth 3") {
		t.Fatalf("mid under sib with cap 2: %v, want the depth error naming the subtree's depth", err)
	}
	// Cap 1 is the old single-level rule: children only.
	if err := ValidateParentLink(loner, top, all, 1); err != nil {
		t.Fatalf("loner under top with cap 1: %v", err)
	}
	if err := ValidateParentLink(loner, mid, all, 1); err == nil {
		t.Fatal("loner under a sub-session with cap 1 must be refused")
	}
	if err := ValidateParentLink(mid, loner, all, 1); err == nil {
		t.Fatal("a session with children under another with cap 1 must be refused")
	}
	if !CanTakeChild(top, all, 1) || CanTakeChild(mid, all, 1) {
		t.Fatal("CanTakeChild disagrees with the cap")
	}
}

func TestResolveMaxParentDepth(t *testing.T) {
	for in, want := range map[int]int{0: DefaultMaxParentDepth, -1: DefaultMaxParentDepth, 1: 1, 7: 7} {
		if got := ResolveMaxParentDepth(in); got != want {
			t.Fatalf("ResolveMaxParentDepth(%d) = %d, want %d", in, got, want)
		}
	}
	if DefaultMaxParentDepth < 3 {
		t.Fatalf("the default cap %d must allow at least three levels below a top-level session", DefaultMaxParentDepth)
	}
}

func TestRelinkChildrenOfArchived_MovesLiveChildrenUpOneLayer(t *testing.T) {
	top, mid, _, _, _, all := chainFixture()
	top.ProjectPath = "/proj/top"
	leaf2 := &Instance{ID: "leaf2", Title: "leaf2", ParentSessionID: "mid"}
	done := &Instance{ID: "done", Title: "done", ParentSessionID: "mid"}
	done.SetArchivedAtThreadSafe(time.Now())
	grand := &Instance{ID: "grand", Title: "grand", ParentSessionID: "leaf"}
	all = append(all, leaf2, done, grand)

	mid.SetArchivedAtThreadSafe(time.Now())
	moved := RelinkChildrenOfArchived(mid, all)
	if chainIDs(moved) != "leaf,leaf2" {
		t.Fatalf("moved %q, want the live direct children leaf,leaf2", chainIDs(moved))
	}
	for _, c := range moved {
		if c.ParentSessionID != top.ID || c.ParentProjectPath != "/proj/top" {
			t.Fatalf("%s now under %q (%q), want top", c.ID, c.ParentSessionID, c.ParentProjectPath)
		}
	}
	if done.ParentSessionID != "mid" {
		t.Fatal("an archived child keeps its link")
	}
	if grand.ParentSessionID != "leaf" || ParentDepth(grand, all) != 2 {
		t.Fatalf("a grandchild moves with its parent: parent=%q depth=%d", grand.ParentSessionID, ParentDepth(grand, all))
	}
}

func TestRelinkChildrenOfArchived_TopLevelKeepsItsChildren(t *testing.T) {
	top, mid, _, sib, _, all := chainFixture()
	if moved := RelinkChildrenOfArchived(top, all); len(moved) != 0 {
		t.Fatalf("a top-level session's children moved: %v", chainIDs(moved))
	}
	if mid.ParentSessionID != "top" || sib.ParentSessionID != "top" {
		t.Fatal("children of an archived top-level session keep their link")
	}
	if got := chainIDs(LiveChildren(top, all)); got != "mid,sib" {
		t.Fatalf("LiveChildren(top) = %q", got)
	}
}
