package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMultiLevelParentsE2E builds top -> mid -> leaf with the real binary,
// a real (isolated) tmux server and the Claude-like fake agent, then checks
// that the grandchild's NEED: line and its done line reach its own parent,
// the middle layer, and never the top; that `session children` lists direct
// children by default and the whole tree with --descendants; that an
// automatic parent nests under the calling sub-session and flattens at the
// depth cap; that a cycle is refused; and that archiving the middle layer
// moves the grandchild up to the top.
func TestMultiLevelParentsE2E(t *testing.T) {
	parallelSubprocessTest(t)
	e := newDeliveryEnv(t)
	top := e.startFake("top", "idle", "")
	mid := e.startFake("mid", "idle", top.id) // add --parent <top-level>
	leaf := e.startFake("leaf", "busy", mid.id) // add --parent <sub-session>

	var shown struct {
		ParentID string `json:"parent_id"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("session", "show", leaf.id, "--json")), &shown); err != nil || shown.ParentID != mid.id {
		t.Fatalf("leaf parent_id = %q (err %v), want mid %s", shown.ParentID, err, mid.id)
	}

	// session children: direct children by default, the subtree on request.
	var direct struct {
		Children []childRow `json:"children"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("session", "children", top.id, "--json")), &direct); err != nil {
		t.Fatal(err)
	}
	if len(direct.Children) != 1 || direct.Children[0].ID != mid.id || direct.Children[0].Depth != 0 {
		t.Fatalf("children of top = %+v, want only mid, without depth", direct.Children)
	}
	var all struct {
		Children []childRow `json:"children"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("session", "children", top.id, "--descendants", "--json")), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Children) != 2 || all.Children[0].ID != mid.id || all.Children[0].Depth != 1 ||
		all.Children[1].ID != leaf.id || all.Children[1].Depth != 2 || all.Children[1].ParentID != mid.id {
		t.Fatalf("descendants of top = %+v, want mid at depth 1 and leaf at depth 2 under mid", all.Children)
	}

	// The grandchild's NEED: turn.
	e.setPhase(leaf, "busy")
	e.mustRun("notify-daemon", "--once") // baseline: the leaf is running
	need := deliveryToken("LEAF_NEED")
	leaf.reply(t, "Two ways to land it.\nNEED: "+need+" merge or hold?")
	e.setPhase(leaf, "idle")
	e.mustRun("notify-daemon", "--once")
	assertOnlyMidHears(t, e, top, mid, leaf, need)

	// The grandchild's done line.
	e.setPhase(leaf, "busy")
	e.mustRun("notify-daemon", "--once")
	done := deliveryToken("LEAF_DONE")
	leaf.reply(t, "===AGENTDECK_DONE=== status=ok summary="+done)
	e.setPhase(leaf, "idle")
	e.mustRun("notify-daemon", "--once")
	assertOnlyMidHears(t, e, top, mid, leaf, done)

	// An automatic parent: a session added from inside leaf (depth 2) nests
	// under leaf; one added from inside that helper (depth 3, the default
	// cap) goes under the helper's parent with a note.
	helperID := addFromInside(t, e, leaf.id, "helper")
	if p := parentOf(t, e, helperID); p != leaf.id {
		t.Fatalf("helper parent = %q, want the calling sub-session leaf %s", p, leaf.id)
	}
	deepID, note := addFromInsideWithNote(t, e, helperID, "deeper")
	if p := parentOf(t, e, deepID); p != leaf.id {
		t.Fatalf("session started from the helper at the cap: parent = %q, want leaf %s", p, leaf.id)
	}
	if !strings.Contains(note, "depth limit") {
		t.Fatalf("no depth-limit note on stderr: %q", note)
	}

	// A cycle is refused.
	out, errOut, code := e.run("", nil, "session", "set-parent", top.id, leaf.id, "--json")
	if code == 0 || !strings.Contains(out+errOut, "own ancestor") {
		t.Fatalf("set-parent top under its grandchild: exit %d %s %s, want refused as a cycle", code, out, errOut)
	}

	// Archiving the middle layer moves its live child up to the top.
	var archived struct {
		Relinked []struct {
			ID       string `json:"id"`
			ParentID string `json:"parent_id"`
		} `json:"relinked_children"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("session", "archive", mid.id, "--json")), &archived); err != nil {
		t.Fatal(err)
	}
	if len(archived.Relinked) != 1 || archived.Relinked[0].ID != leaf.id || archived.Relinked[0].ParentID != top.id {
		t.Fatalf("relinked_children = %+v, want leaf moved under top", archived.Relinked)
	}
	if p := parentOf(t, e, leaf.id); p != top.id {
		t.Fatalf("after archiving mid, leaf parent = %q, want top %s", p, top.id)
	}
	if p := parentOf(t, e, helperID); p != leaf.id {
		t.Fatalf("the helper must move with leaf: parent = %q", p)
	}
}

// assertOnlyMidHears checks the middle layer's inbox holds a record from the
// leaf carrying token, and the top's inbox holds nothing from the leaf.
func assertOnlyMidHears(t *testing.T, e *deliveryEnv, top, mid, leaf *fakeTarget, token string) {
	t.Helper()
	recs := e.pendingFrom(mid.id, leaf.id)
	found := false
	for _, r := range recs {
		if raw, _ := json.Marshal(r); strings.Contains(string(raw), token) {
			found = true
			if r["target_session_id"] != mid.id {
				t.Fatalf("record targets %v, want mid %s: %v", r["target_session_id"], mid.id, r)
			}
		}
	}
	if !found {
		t.Fatalf("mid's inbox has no record from leaf carrying %s: %v", token, recs)
	}
	if fromLeaf := e.pendingFrom(top.id, leaf.id); len(fromLeaf) != 0 {
		t.Fatalf("top's inbox holds %d record(s) from the grandchild, want none (a layer was skipped): %v", len(fromLeaf), fromLeaf)
	}
}

func addFromInside(t *testing.T, e *deliveryEnv, callerID, title string) string {
	t.Helper()
	id, _ := addFromInsideWithNote(t, e, callerID, title)
	return id
}

// addFromInsideWithNote runs `add` as if from inside the session callerID
// (its identity in the environment, no --parent) and returns the new id and
// stderr.
func addFromInsideWithNote(t *testing.T, e *deliveryEnv, callerID, title string) (string, string) {
	t.Helper()
	out, errOut, code := e.run("", []string{"AGENTDECK_INSTANCE_ID=" + callerID}, "add", "-t", title, "-c", "claude", "--json", e.root)
	var added struct {
		ID string `json:"id"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &added) != nil || added.ID == "" {
		t.Fatalf("add %s from %s: exit %d %s %s", title, callerID, code, out, errOut)
	}
	return added.ID, errOut
}

func parentOf(t *testing.T, e *deliveryEnv, id string) string {
	t.Helper()
	var shown struct {
		ParentID string `json:"parent_id"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("session", "show", id, "--json")), &shown); err != nil {
		t.Fatalf("session show %s: %v", id, err)
	}
	return shown.ParentID
}
