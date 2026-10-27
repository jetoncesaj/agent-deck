package statedb

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A /clear rebind keeps the replaced conversation id in tool_data as
// claude_previous_session_id, and a later full save from a writer that does
// not model the key does not drop it.
func TestWriteClaudeSessionRebindingKeepsPreviousID(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.SaveInstance(&InstanceRow{
		ID: "sess", Title: "conductor", Tool: "claude", Status: "running", CreatedAt: time.Unix(1000, 0),
		ToolData: json.RawMessage(`{"claude_session_id":"b74814e8-old","claude_detected_at":1500}`),
	}))

	require.NoError(t, db.WriteClaudeSessionRebinding("sess", "e7b13f82-new", "b74814e8-old", time.Unix(2000, 0)))

	readToolData := func() map[string]any {
		rows, err := db.LoadInstances()
		require.NoError(t, err)
		require.Len(t, rows, 1)
		var td map[string]any
		require.NoError(t, json.Unmarshal(rows[0].ToolData, &td))
		return td
	}
	td := readToolData()
	require.Equal(t, "e7b13f82-new", td["claude_session_id"])
	require.Equal(t, "b74814e8-old", td["claude_previous_session_id"])

	// Full save from a snapshot that knows only the typed keys.
	require.NoError(t, db.SaveInstance(&InstanceRow{
		ID: "sess", Title: "conductor", Tool: "claude", Status: "waiting", CreatedAt: time.Unix(1000, 0),
		ToolData: json.RawMessage(`{"claude_session_id":"e7b13f82-new","claude_detected_at":2000}`),
	}))
	td = readToolData()
	require.Equal(t, "e7b13f82-new", td["claude_session_id"])
	require.Equal(t, "b74814e8-old", td["claude_previous_session_id"], "a full save must carry the previous id forward")

	// No previous id (cold bind) writes no key.
	require.NoError(t, db.SaveInstance(&InstanceRow{
		ID: "cold", Title: "worker", Tool: "claude", Status: "running", CreatedAt: time.Unix(1000, 0),
		ToolData: json.RawMessage(`{}`),
	}))
	require.NoError(t, db.WriteClaudeSessionRebinding("cold", "11111111-new", "", time.Unix(2000, 0)))
	rows, err := db.LoadInstances()
	require.NoError(t, err)
	for _, r := range rows {
		if r.ID == "cold" {
			require.NotContains(t, string(r.ToolData), "claude_previous_session_id")
		}
	}
}
