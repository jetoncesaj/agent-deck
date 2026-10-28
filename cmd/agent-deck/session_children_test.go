package main

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestChildrenOfFiltersByParent(t *testing.T) {
	a := &session.Instance{ID: "p"}
	b := &session.Instance{ID: "c1", ParentSessionID: "p"}
	c := &session.Instance{ID: "c2", ParentSessionID: "other"}
	d := &session.Instance{ID: "c3", ParentSessionID: "p"}
	got := childrenOf("p", []*session.Instance{a, b, c, d})
	if len(got) != 2 {
		t.Fatalf("expected 2 children, got %d", len(got))
	}
	if got[0].ID != "c1" || got[1].ID != "c3" {
		t.Fatalf("unexpected children: %v %v", got[0].ID, got[1].ID)
	}
}

// childrenOf stays direct-only: a grandchild is not a child.
func TestChildrenOfSkipsGrandchildren(t *testing.T) {
	all := []*session.Instance{
		{ID: "p"},
		{ID: "c", ParentSessionID: "p"},
		{ID: "g", ParentSessionID: "c"},
	}
	if got := childrenOf("p", all); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("childrenOf(p) = %v, want only the direct child c", got)
	}
}

// --descendants rows carry each session's direct parent and its depth below
// the listed session; the direct listing leaves both out.
func TestBuildDescendantRowsCarryParentAndDepth(t *testing.T) {
	all := []*session.Instance{
		{ID: "p", Title: "p"},
		{ID: "c", Title: "c", ParentSessionID: "p"},
		{ID: "g", Title: "g", ParentSessionID: "c"},
		{ID: "c2", Title: "c2", ParentSessionID: "p"},
	}
	rows := buildDescendantRows(session.Descendants("p", all, 0), cachedChildStatus)
	want := []struct {
		id, parent string
		depth      int
	}{{"c", "p", 1}, {"g", "c", 2}, {"c2", "p", 1}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i].ID != w.id || rows[i].ParentID != w.parent || rows[i].Depth != w.depth {
			t.Fatalf("row %d = %+v, want id=%s parent_id=%s depth=%d", i, rows[i], w.id, w.parent, w.depth)
		}
	}
	for _, r := range buildChildRows(childrenOf("p", all), cachedChildStatus) {
		if r.ParentID != "" || r.Depth != 0 {
			t.Fatalf("a direct-children row %+v carries parent_id/depth", r)
		}
	}
}
