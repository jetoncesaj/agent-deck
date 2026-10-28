package panehost

import (
	"errors"
	"fmt"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/panenav"
)

// MaxWatch bounds how many sessions one pane may watch.
const MaxWatch = 200

// ParseWatch parses "watch ID ID ..." (replace the watched set) and
// "watch none" (clear it). ok is false for any other verb; err is set for a
// watch line with a bad id or too many ids. Duplicates are dropped, order
// kept.
func ParseWatch(line string) (ids []string, ok bool, err error) {
	f := strings.Fields(line)
	if len(f) == 0 || f[0] != "watch" {
		return nil, false, nil
	}
	if len(f) == 1 {
		return nil, true, errors.New(`watch wants session ids, or "none"`)
	}
	if len(f) == 2 && f[1] == "none" {
		return []string{}, true, nil
	}
	seen := map[string]bool{}
	for _, id := range f[1:] {
		if !panenav.ValidSessionID(id) {
			return nil, true, fmt.Errorf("watch: invalid session id %q", id)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) > MaxWatch {
		return nil, true, fmt.Errorf("watch: %d sessions, at most %d", len(ids), MaxWatch)
	}
	return ids, true, nil
}

// watchState is the pane's watched set and the last value sent for each.
type watchState struct {
	ids  []string
	last map[string]string
}

// replace sets the watched set and returns the ids that are new to it (they
// get their current value at once). Ids that left the set are forgotten.
func (w *watchState) replace(ids []string) (added []string) {
	next := make(map[string]string, len(ids))
	for _, id := range ids {
		if v, ok := w.last[id]; ok {
			next[id] = v
		} else {
			added = append(added, id)
		}
	}
	w.ids, w.last = ids, next
	return added
}

// changes records now and returns the lines for every watched session whose
// value differs from the last one sent (for ids, or all watched ids when
// ids is nil), in watch order. A session missing from now is left as it
// was (a failed read).
func (w *watchState) changes(now map[string]string, ids []string) []string {
	if ids == nil {
		ids = w.ids
	}
	var out []string
	for _, id := range ids {
		v, ok := now[id]
		if !ok {
			continue
		}
		v = oneLine(v)
		if prev, seen := w.last[id]; seen && prev == v {
			continue
		}
		w.last[id] = v
		out = append(out, strings.TrimRight("refresh session "+id+" "+v, " "))
	}
	return out
}
