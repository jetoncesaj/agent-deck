package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

// Attention levels of a driver-set row state (`session set <id> state`).
const (
	// AttentionNeedsYou: the person's turn (asks you, your approval, an
	// error, needs your read). A folded parent row counts it.
	AttentionNeedsYou = "needs-you"
	// AttentionMoving: work is under way (working, landing).
	AttentionMoving = "moving"
	// AttentionQuiet: nothing is happening (idle, stopped).
	AttentionQuiet = "quiet"
)

// Bounds on the row state's text, in terminal cells: a word is a short
// phrase on one row ("your approval"), a role one word ("review").
const (
	MaxRowStateWordCells = 24
	MaxRowStateRoleCells = 16
)

// toolDataRowStateKey stores the row state in the tool_data extras zone,
// like item_ref: no schema change, and a legacy binary preserves the key.
const toolDataRowStateKey = "row_state"

// RowState is what a driver (a conductor loop, a script) says about a
// session's row: one symbol that replaces the deck's status icon, a short
// word, an attention level that picks the colour, and an optional role
// word. It is the driver's claim, not the deck's detection; unset, the row
// shows the deck's own status. The JSON shape is the one `session set <id>
// state` takes and `list --json` / `session show --json` return as "state".
type RowState struct {
	Symbol    string `json:"symbol"`
	Attention string `json:"attention"`
	Word      string `json:"word,omitempty"`
	Role      string `json:"role,omitempty"`
}

// ParseRowState reads the value of `session set <id> state`: a JSON object
// with symbol and attention (required) and word and role (optional). An
// empty value, "null" or "{}" clears the state (nil, no error). Unknown keys
// are refused so a typo cannot silently drop a field.
func ParseRowState(value string) (*RowState, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" || value == "{}" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(value)))
	dec.DisallowUnknownFields()
	var st RowState
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf(`state must be a JSON object {"symbol","attention","word","role"}: %w`, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("state must be one JSON object")
	}
	st.Symbol = strings.TrimSpace(st.Symbol)
	st.Word = strings.TrimSpace(st.Word)
	st.Attention = strings.TrimSpace(st.Attention)
	st.Role = strings.TrimSpace(st.Role)
	if err := st.validate(); err != nil {
		return nil, err
	}
	return &st, nil
}

func (st *RowState) validate() error {
	switch st.Attention {
	case AttentionNeedsYou, AttentionMoving, AttentionQuiet:
	default:
		return fmt.Errorf("state attention %q: expected %s, %s or %s", st.Attention, AttentionNeedsYou, AttentionMoving, AttentionQuiet)
	}
	if uniseg.GraphemeClusterCount(st.Symbol) != 1 {
		return fmt.Errorf("state symbol %q: expected exactly one symbol", st.Symbol)
	}
	if w := uniseg.StringWidth(st.Symbol); w < 1 || w > 2 {
		return fmt.Errorf("state symbol %q is %d cells wide; expected 1 or 2", st.Symbol, w)
	}
	for name, text := range map[string]string{"symbol": st.Symbol, "word": st.Word, "role": st.Role} {
		for _, r := range text {
			if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
				return fmt.Errorf("state %s %q contains a control character", name, text)
			}
		}
	}
	if w := uniseg.StringWidth(st.Word); w > MaxRowStateWordCells {
		return fmt.Errorf("state word is %d cells; at most %d", w, MaxRowStateWordCells)
	}
	if w := uniseg.StringWidth(st.Role); w > MaxRowStateRoleCells {
		return fmt.Errorf("state role is %d cells; at most %d", w, MaxRowStateRoleCells)
	}
	return nil
}

// RowState returns a copy of the driver-set row state, or nil when unset.
// Safe from any goroutine (the TUI's render snapshot reads it).
func (i *Instance) RowState() *RowState {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.rowState == nil {
		return nil
	}
	st := *i.rowState
	return &st
}

// SetRowState sets the row state; nil clears it. The state is expected to
// be validated already (ParseRowState).
func (i *Instance) SetRowState(st *RowState) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rowStateCleared = i.rowState != nil && st == nil
	if st == nil {
		i.rowState = nil
		return
	}
	cp := *st
	i.rowState = &cp
}

// rowStateForSave returns the state and whether a clear must be written
// explicitly (see WriteRowStateToToolData).
func (i *Instance) rowStateForSave() (st *RowState, cleared bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.rowState, i.rowStateCleared
}

// WriteRowStateToToolData records the row state in the blob. A nil state
// removes the key, so rows that never had one keep their bytes; cleared
// writes an explicit null instead, because MergeToolDataExtras carries an
// absent key forward from the stored row (same protocol as item_ref).
func WriteRowStateToToolData(td json.RawMessage, st *RowState, cleared bool) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	switch {
	case st != nil:
		raw, err := json.Marshal(st)
		if err != nil {
			return td
		}
		m[toolDataRowStateKey] = raw
	case cleared:
		m[toolDataRowStateKey] = json.RawMessage(`null`)
	default:
		delete(m, toolDataRowStateKey)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return td
	}
	return out
}

// ReadRowStateFromToolData returns the row state; nil for legacy rows, a
// cleared state, or one that no longer validates.
func ReadRowStateFromToolData(td json.RawMessage) *RowState {
	if len(td) == 0 {
		return nil
	}
	var blob struct {
		RowState *RowState `json:"row_state"`
	}
	if err := json.Unmarshal(td, &blob); err != nil || blob.RowState == nil {
		return nil
	}
	if blob.RowState.validate() != nil {
		return nil
	}
	return blob.RowState
}

// RowStateString is the state as `session set` and `session show` print it:
// its JSON, or "" when unset.
func RowStateString(st *RowState) string {
	if st == nil {
		return ""
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return string(raw)
}
