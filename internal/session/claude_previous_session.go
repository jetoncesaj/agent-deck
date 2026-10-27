package session

import "encoding/json"

// toolDataClaudePreviousSessionIDKey is the tool_data key a hook rebind
// writes the replaced conversation id under (statedb
// WriteClaudeSessionRebinding): the pre-/clear or pre-compaction
// conversation. It is outside the typed tool_data schema, so
// MergeToolDataExtras carries it across full saves; no save path writes it.
const toolDataClaudePreviousSessionIDKey = "claude_previous_session_id"

// ReadClaudePreviousSessionIDFromToolData extracts claude_previous_session_id
// from the blob. Returns "" for missing, malformed, or legacy rows. It is
// display data (`session show`), never a source of conversation identity.
func ReadClaudePreviousSessionIDFromToolData(td json.RawMessage) string {
	if len(td) == 0 {
		return ""
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(td, &blob); err != nil {
		return ""
	}
	var id string
	if raw, ok := blob[toolDataClaudePreviousSessionIDKey]; ok {
		_ = json.Unmarshal(raw, &id)
	}
	return id
}
