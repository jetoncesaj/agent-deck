package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/core"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// "Asks you" in list --json: the legacy builder and core.SessionRow carry
// the same "asks" object (TestCoreRegistryMatchesLegacyHandlers compares
// bytes), absent for a session that asks nothing.
func TestListJSON_Asks(t *testing.T) {
	cliInboxTestHome(t)
	instances := []*session.Instance{
		{ID: "asking-1", Title: "worker", Tool: "claude"},
		{ID: "quiet-1", Title: "other", Tool: "claude"},
	}
	ask, _, err := session.SetAsk("_test", "asking-1", session.PersonAsk{Kind: "pick", Question: "A or B?", Item: "ITEM-7"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := buildListJSON("_test", instances, map[*session.Instance]bool{instances[0]: true, instances[1]: true})
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
	got, ok := byID["asking-1"]["asks"].(map[string]any)
	if !ok || got["kind"] != "pick" || got["question"] != "A or B?" || got["item"] != "ITEM-7" || got["since"] == "" || got["source"] != "cli" {
		t.Fatalf("asks = %v", byID["asking-1"]["asks"])
	}
	if _, ok := byID["quiet-1"]["asks"]; ok {
		t.Fatalf("a session that asks nothing carries asks: %v", byID["quiet-1"])
	}

	// core.SessionRow marshals the same object.
	row := core.SessionRow{Asks: &ask}
	data, _ := json.Marshal(row)
	var back map[string]any
	_ = json.Unmarshal(data, &back)
	if a, _ := back["asks"].(map[string]any); a["question"] != "A or B?" {
		t.Fatalf("core row asks: %s", data)
	}
}

// `session ask` sets and clears the state end to end; session show --json
// carries it as "asks" while open and drops it once cleared.
func TestSessionAskCLI_SetShowClear(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	id := sessionMoveAddSession(t, home, filepath.Join(home, "proj"), "asker")
	run := func(args ...string) string {
		t.Helper()
		stdout, stderr, code := runAgentDeck(t, home, args...)
		if code != 0 {
			t.Fatalf("%v failed (exit %d)\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
		}
		return stdout
	}
	showAsks := func() (map[string]any, bool) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(run("session", "show", id, "--json")), &m); err != nil {
			t.Fatal(err)
		}
		a, ok := m["asks"].(map[string]any)
		return a, ok
	}

	if _, code := func() (string, int) {
		o, _, c := runAgentDeck(t, home, "session", "ask", id, "--kind", "whim", "x?")
		return o, c
	}(); code == 0 {
		t.Fatal("an unknown kind must be refused")
	}
	run("session", "ask", id, "--kind", "approval", "--item", "ITEM-3", "Merge the migration?")
	a, ok := showAsks()
	if !ok || a["kind"] != "approval" || a["question"] != "Merge the migration?" || a["item"] != "ITEM-3" {
		t.Fatalf("show asks = %v", a)
	}
	run("session", "ask", id, "--clear")
	if a, ok := showAsks(); ok {
		t.Fatalf("asks still shown after --clear: %v", a)
	}
}
