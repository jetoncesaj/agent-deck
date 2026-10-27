package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// /clear tracking, hook side: Claude Code's SessionStart(source="clear")
// carries the new conversation id. The handler must keep that declaration
// (status file "source" plus the rollover marker) so the binding can follow
// the new conversation; a later startup/resume SessionStart removes it.
func TestHookHandler_SessionStartClearWritesRolloverMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_JOB_DIR", "")
	t.Setenv("AGENTDECK_HOOK_GENERATION", "")
	t.Setenv("AGENTDECK_NO_CHILDREN_CONTEXT", "1")
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	const inst = "inst-clear"
	t.Setenv("AGENTDECK_INSTANCE_ID", inst)

	const newID = "e7b13f82-e006-4844-9ba5-942823049491"
	runHookHandlerWith(t, fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"clear","cwd":%q}`, newID, home))

	data, err := os.ReadFile(filepath.Join(getHooksDir(), inst+".json"))
	if err != nil {
		t.Fatalf("status file: %v", err)
	}
	var sf hookStatusFile
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	if sf.Source != "clear" || sf.SessionID != newID {
		t.Fatalf("status file = %+v, want source=clear session_id=%s", sf, newID)
	}
	marker, err := os.ReadFile(session.ClaudeRolloverMarkerPath(inst))
	if err != nil {
		t.Fatalf("SessionStart(clear) must write the rollover marker: %v", err)
	}
	var m struct {
		SessionID string `json:"session_id"`
		Source    string `json:"source"`
	}
	if err := json.Unmarshal(marker, &m); err != nil || m.SessionID != newID || m.Source != "clear" {
		t.Fatalf("marker = %s (err %v), want session_id=%s source=clear", marker, err, newID)
	}

	// Non-SessionStart events leave it alone and carry no source.
	runHookHandlerWith(t, fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":%q,"cwd":%q,"prompt":"hi"}`, newID, home))
	if _, err := os.Stat(session.ClaudeRolloverMarkerPath(inst)); err != nil {
		t.Fatalf("UserPromptSubmit must not remove the marker: %v", err)
	}

	// A fresh launch or /resume removes it.
	runHookHandlerWith(t, fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"resume","cwd":%q}`, "b74814e8-bcd9-4b44-87b6-b1eb2d888e5a", home))
	if _, err := os.Stat(session.ClaudeRolloverMarkerPath(inst)); !os.IsNotExist(err) {
		t.Fatalf("SessionStart(resume) must remove the marker, stat err = %v", err)
	}
}
