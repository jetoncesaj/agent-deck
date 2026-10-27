package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// list --json carries parent_id and parent_title on every row, "" when the
// session has no parent (or the parent is not listed).
func TestBuildListJSON_ParentIDAndTitle(t *testing.T) {
	instances := []*session.Instance{
		{ID: "p1", Title: "epic-conductor", Tool: "claude"},
		{ID: "c1", Title: "worker", Tool: "claude", ParentSessionID: "p1"},
		{ID: "o1", Title: "orphan", Tool: "claude", ParentSessionID: "gone"},
	}
	cached := map[*session.Instance]bool{instances[0]: true, instances[1]: true, instances[2]: true}
	out, err := buildListJSON("_test", instances, cached)
	if err != nil {
		t.Fatalf("buildListJSON: %v", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	want := map[string][2]string{"p1": {"", ""}, "c1": {"p1", "epic-conductor"}, "o1": {"gone", ""}}
	for _, r := range rows {
		id := r["id"].(string)
		gotID, okID := r["parent_id"]
		gotTitle, okTitle := r["parent_title"]
		if !okID || !okTitle {
			t.Fatalf("row %s lacks parent_id/parent_title: %v", id, r)
		}
		if gotID != want[id][0] || gotTitle != want[id][1] {
			t.Errorf("row %s parent = %v/%v, want %q/%q", id, gotID, gotTitle, want[id][0], want[id][1])
		}
	}
}

// session show --json carries parent_id and parent_title too.
func TestSessionShowJSON_ParentIDAndTitle(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	parentID := sessionMoveAddSession(t, home, filepath.Join(home, "parent"), "the-parent")
	stdout, stderr, code := runAgentDeck(t, home, "add", "-t", "the-child", "-c", "claude", "--parent", parentID, "--json", filepath.Join(home, "parent"))
	if code != 0 {
		t.Fatalf("add child failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &added); err != nil || added.ID == "" {
		t.Fatalf("parse add: %v\n%s", err, stdout)
	}
	show := func(id string) map[string]interface{} {
		t.Helper()
		stdout, stderr, code := runAgentDeck(t, home, "session", "show", id, "--json")
		if code != 0 {
			t.Fatalf("session show failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &m); err != nil {
			t.Fatalf("parse show: %v\n%s", err, stdout)
		}
		return m
	}
	child := show(added.ID)
	if child["parent_id"] != parentID || child["parent_title"] != "the-parent" {
		t.Fatalf("child parent = %v/%v, want %s/the-parent", child["parent_id"], child["parent_title"], parentID)
	}
	parent := show(parentID)
	if v, ok := parent["parent_id"]; !ok || v != "" {
		t.Fatalf("parent row parent_id = %v (present %v), want \"\"", v, ok)
	}
	if v, ok := parent["parent_title"]; !ok || v != "" {
		t.Fatalf("parent row parent_title = %v (present %v), want \"\"", v, ok)
	}
}
