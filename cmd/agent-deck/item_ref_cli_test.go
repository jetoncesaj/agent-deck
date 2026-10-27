package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// `session set <id> item <ref>` persists the ref; list --json and session
// show --json carry it as "item" ("" when none), show prints it, and an
// empty value clears it.
func TestSessionSetItem_ShownInListAndShow(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	id := sessionMoveAddSession(t, home, filepath.Join(home, "proj"), "worker")
	other := sessionMoveAddSession(t, home, filepath.Join(home, "proj2"), "no-item")

	if stdout, stderr, code := runAgentDeck(t, home, "session", "set", id, "item", "ONAZA-737"); code != 0 {
		t.Fatalf("session set item failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	listItems := func() map[string]interface{} {
		t.Helper()
		stdout, stderr, code := runAgentDeck(t, home, "list", "--json")
		if code != 0 {
			t.Fatalf("list --json failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		var rows []map[string]interface{}
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("parse list: %v\n%s", err, stdout)
		}
		items := map[string]interface{}{}
		for _, r := range rows {
			v, ok := r["item"]
			if !ok {
				t.Fatalf("row %v lacks item", r["id"])
			}
			items[r["id"].(string)] = v
		}
		return items
	}
	items := listItems()
	if items[id] != "ONAZA-737" || items[other] != "" {
		t.Fatalf("list items = %v", items)
	}

	stdout, stderr, code := runAgentDeck(t, home, "session", "show", id, "--json")
	if code != 0 {
		t.Fatalf("show --json failed (exit %d)\n%s\n%s", code, stdout, stderr)
	}
	var shown map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil || shown["item"] != "ONAZA-737" {
		t.Fatalf("show --json item = %v (%v)\n%s", shown["item"], err, stdout)
	}
	stdout, _, _ = runAgentDeck(t, home, "session", "show", id)
	if !strings.Contains(stdout, "Item:    ONAZA-737") {
		t.Fatalf("show text lacks the item:\n%s", stdout)
	}

	if _, stderr, code := runAgentDeck(t, home, "session", "set", id, "item", "bad\nref"); code == 0 {
		t.Fatalf("multi-line item accepted\n%s", stderr)
	}
	if _, stderr, code := runAgentDeck(t, home, "session", "set", id, "item", ""); code != 0 {
		t.Fatalf("clearing item failed: %s", stderr)
	}
	if items := listItems(); items[id] != "" {
		t.Fatalf("item not cleared: %v", items)
	}
}
