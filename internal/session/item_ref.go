package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// toolDataItemRefKey stores the session's item ref (`session set <id> item
// <ref>`) in the tool_data extras zone, like favorite: no schema change, and
// a legacy binary preserves the key on save.
const toolDataItemRefKey = "item_ref"

// MaxItemRefLen bounds an item ref. A ref names one tracker item (an issue
// key, a URL); it is a routing cache, not a description.
const MaxItemRefLen = 512

// NormalizeItemRef trims ref and rejects one that is too long or carries
// control characters (newlines included): a ref is shown on one line and
// matched verbatim by `inbox post`. "" is valid and clears the item.
func NormalizeItemRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if len(ref) > MaxItemRefLen {
		return "", fmt.Errorf("item ref is %d bytes; at most %d", len(ref), MaxItemRefLen)
	}
	for _, r := range ref {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("item ref %q contains a control character", ref)
		}
	}
	return ref, nil
}

// ItemRef returns the tracker item this session works on (any tracker's
// ref, free text), or "" when none is set. It is a routing cache set by a
// project's launch command; the tracker holds the truth. Safe to call from
// any goroutine (the side-pane host reads it while the TUI may set it).
func (i *Instance) ItemRef() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.itemRef
}

// SetItemRef sets the item ref; "" clears it. The ref is expected to be
// normalized already (NormalizeItemRef).
func (i *Instance) SetItemRef(ref string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.itemRefCleared = i.itemRef != "" && ref == ""
	i.itemRef = ref
}

// itemRefForSave returns the ref and whether a clear must be written
// explicitly (see WriteItemRefToToolData).
func (i *Instance) itemRefForSave() (ref string, cleared bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.itemRef, i.itemRefCleared
}

// WriteItemRefToToolData records the item ref in the blob. An empty ref
// removes the key, so rows that never had an item keep their bytes. cleared
// writes an explicit "" instead: MergeToolDataExtras carries an absent key
// forward from the stored row, so a bare removal would silently revert a
// clear (same protocol as favorite).
func WriteItemRefToToolData(td json.RawMessage, ref string, cleared bool) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	switch {
	case ref != "":
		raw, err := json.Marshal(ref)
		if err != nil {
			return td
		}
		m[toolDataItemRefKey] = raw
	case cleared:
		m[toolDataItemRefKey] = json.RawMessage(`""`)
	default:
		delete(m, toolDataItemRefKey)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return td
	}
	return out
}

// ReadItemRefFromToolData returns the item ref; "" for legacy rows.
func ReadItemRefFromToolData(td json.RawMessage) string {
	if len(td) == 0 {
		return ""
	}
	var blob struct {
		ItemRef string `json:"item_ref"`
	}
	_ = json.Unmarshal(td, &blob)
	return blob.ItemRef
}
