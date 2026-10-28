package main

import (
	"encoding/json"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Projects identify conductors by is_conductor and conductor_name, never by
// group or title: list --json and session show --json carry both for a
// conductor (renamed, in any group) and neither for a worker whose title
// merely looks like a conductor's.
func TestConductorIdentityJSON(t *testing.T) {
	conductor := &session.Instance{ID: "c1", Title: "ops (renamed)", GroupPath: "team-a", Tool: "claude", IsConductor: true, ConductorName: "ops"}
	worker := &session.Instance{ID: "w1", Title: "conductor-ops", GroupPath: "conductor", Tool: "claude"}
	instances := []*session.Instance{conductor, worker}

	out, err := buildListJSON("_test", instances, map[*session.Instance]bool{conductor: true, worker: true})
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}
	if r := byID["c1"]; r["is_conductor"] != true || r["conductor_name"] != "ops" {
		t.Fatalf("list conductor row: is_conductor=%v conductor_name=%v", r["is_conductor"], r["conductor_name"])
	}
	if r := byID["w1"]; r["is_conductor"] != nil || r["conductor_name"] != nil {
		t.Fatalf("list worker row carries conductor identity: %v", r)
	}

	show := map[string]interface{}{}
	addConductorJSON(show, conductor)
	if show["is_conductor"] != true || show["conductor_name"] != "ops" {
		t.Fatalf("show conductor: %v", show)
	}
	show = map[string]interface{}{}
	addConductorJSON(show, worker)
	if len(show) != 0 {
		t.Fatalf("show worker carries conductor identity: %v", show)
	}
}
