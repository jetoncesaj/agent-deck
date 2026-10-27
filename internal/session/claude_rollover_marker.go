package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude rollover marker sidecar (/clear tracking).
//
// When a person types /clear (or Claude Code auto-compacts into a new
// conversation), Claude Code fires SessionStart with source "clear" (or
// "compact") carrying the NEW session_id. The hook status file is overwritten
// by every later event, so the source is gone by the time the binding logic
// sees the conversation's first turn. The hook handler therefore records the
// declared successor in <hooks>/<instance>.rollover, written in the same
// generation-locked critical section as the hook session anchor, and removes
// it on any other SessionStart (startup, resume) so a stale marker can never
// steer a later rebind.
//
// The marker is evidence for exactly one decision: when a candidate id equals
// the marker's id, UpdateHookStatus skips the #661/#856 size-and-mtime
// heuristic, whose only job is to tell a restart flap from a user-initiated
// new conversation. Every other identity guard (terminal events, foreign
// cwd, no conversation data, background workers, hook generation) still runs.

// claudeRolloverMarker is the on-disk form of the sidecar.
type claudeRolloverMarker struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
	Timestamp int64  `json:"ts"`
}

// IsClaudeRolloverSource reports whether a SessionStart source means Claude
// Code started a new conversation inside the same live process (the pane's
// own /clear or compaction), as opposed to a fresh launch or a /resume of an
// arbitrary conversation.
func IsClaudeRolloverSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "clear", "compact":
		return true
	}
	return false
}

// ClaudeRolloverMarkerPath returns the sidecar path for one instance.
func ClaudeRolloverMarkerPath(instanceID string) string {
	return filepath.Join(GetHooksDir(), instanceID+".rollover")
}

// WriteClaudeRolloverMarker records that SessionStart(source) declared
// sessionID as this instance's new conversation. A non-rollover source clears
// the marker instead.
func WriteClaudeRolloverMarker(instanceID, sessionID, source string) {
	instanceID = strings.TrimSpace(instanceID)
	sessionID = strings.TrimSpace(sessionID)
	if instanceID == "" || filepath.Base(instanceID) != instanceID {
		return
	}
	if sessionID == "" || !IsClaudeRolloverSource(source) {
		ClearClaudeRolloverMarker(instanceID)
		return
	}
	data, err := json.Marshal(claudeRolloverMarker{
		SessionID: sessionID,
		Source:    strings.ToLower(strings.TrimSpace(source)),
		Timestamp: time.Now().Unix(),
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(GetHooksDir(), 0o700); err != nil {
		return
	}
	path := ClaudeRolloverMarkerPath(instanceID)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmpPath, path)
}

// ClearClaudeRolloverMarker removes the sidecar.
func ClearClaudeRolloverMarker(instanceID string) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" || filepath.Base(instanceID) != instanceID {
		return
	}
	_ = os.Remove(ClaudeRolloverMarkerPath(instanceID))
}

// readClaudeRolloverMarker returns the declared successor id and its source,
// or empty strings when there is no readable marker.
func readClaudeRolloverMarker(instanceID string) (sessionID, source string) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" || filepath.Base(instanceID) != instanceID {
		return "", ""
	}
	data, err := readStatusFileNoFollow(ClaudeRolloverMarkerPath(instanceID))
	if err != nil || len(data) == 0 {
		return "", ""
	}
	var m claudeRolloverMarker
	if json.Unmarshal(data, &m) != nil || !IsClaudeRolloverSource(m.Source) {
		return "", ""
	}
	return strings.TrimSpace(m.SessionID), m.Source
}

// claudeRolloverDeclared reports whether this instance's own Claude process
// declared candidate as the conversation that replaced the bound one via
// SessionStart(clear|compact), returning that source.
func (i *Instance) claudeRolloverDeclared(candidate string) (string, bool) {
	if i == nil || candidate == "" {
		return "", false
	}
	id, source := readClaudeRolloverMarker(i.ID)
	if id == "" || id != candidate {
		return "", false
	}
	return source, true
}
