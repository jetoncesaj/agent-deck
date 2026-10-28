package session

// RecomputeTreeConnectors computes IsLastInGroup, IsLastSubSession and
// ParentIsLastInGroup from scratch on the final visible item list — the
// values GroupTree.Flatten set are stale by the time archived/status
// filtering and view-mode partitioning are done with the list (see the
// Item field comments in groups.go). Call it once, right before injecting
// tmux-window rows, on the list produced by PartitionByViewMode.
//
// Scope: only ItemTypeGroup and ItemTypeSession rows are read. Every other
// row type (dividers, remote rows, windows — none of which reach this call
// today) passes through with its fields untouched.
func RecomputeTreeConnectors(items []Item) []Item {
	segStart := 0
	haveLastPath := false
	var lastPath string

	flush := func(end int) {
		if end > segStart {
			recomputeSegment(items, segStart, end)
		}
	}

	for i := range items {
		switch items[i].Type {
		case ItemTypeGroup:
			// A group header always starts a fresh segment for its Path.
			flush(i)
			segStart = i + 1
			haveLastPath = false
		case ItemTypeSession:
			// No header separates consecutive rows of a default group, and a
			// subgroup header can be filtered out of the visible list — in
			// both cases a bare change in Path must also close the prior
			// segment, or its last row loses its closing state.
			if haveLastPath && items[i].Path != lastPath {
				flush(i)
				segStart = i
			}
			lastPath = items[i].Path
			haveLastPath = true
		}
	}
	flush(len(items))

	return items
}

// recomputeSegment assigns the tree-connector flags for the ItemTypeSession
// rows in items[start:end), which all share one Path and are not split by a
// group header. A row nests under its parent only when the parent has a row
// of its own in this segment; its render depth is then its parent's plus
// one, at any depth. A row whose parent has no row here (a structural
// orphan, a row whose parent was filtered out, or one separated from its
// parent by view-mode partitioning) draws at top level. Two passes: the
// first finds render depths, the last top-level row and each parent's last
// nested child; the second assigns IsLastInGroup, IsLastSubSession,
// ParentIsLastInGroup, TreeDepth, TreeGutter and Level from them.
func recomputeSegment(items []Item, start, end int) {
	// sessionIdx maps a session's ID to its row index, scoped to this segment
	// only: a parent that exists elsewhere in the flat list but not in this
	// segment must be treated as absent (D6, R2).
	sessionIdx := make(map[string]int, end-start)
	for i := start; i < end; i++ {
		it := &items[i]
		if it.Type != ItemTypeSession || it.Session == nil {
			continue
		}
		sessionIdx[it.Session.ID] = i
	}

	// parentRow is the row index of it's parent in this segment, or -1.
	parentRow := func(i int) int {
		it := &items[i]
		if !it.IsSubSession || it.Session == nil {
			return -1
		}
		p, ok := sessionIdx[it.Session.ParentSessionID]
		if !ok || p == i {
			return -1
		}
		return p
	}
	// depth memoizes each row's render depth; -2 marks a row being resolved,
	// so a parent cycle among visible rows resolves to top level instead of
	// recursing forever.
	depth := make(map[int]int, end-start)
	var renderDepth func(i int) int
	renderDepth = func(i int) int {
		if d, ok := depth[i]; ok {
			if d == -2 {
				return -1
			}
			return d
		}
		depth[i] = -2
		d := 0
		if p := parentRow(i); p >= 0 {
			if pd := renderDepth(p); pd >= 0 {
				d = pd + 1
			}
		}
		depth[i] = d
		return d
	}

	lastTopLevelIdx := -1
	lastSubIdxByParent := make(map[string]int, end-start)
	for i := start; i < end; i++ {
		it := &items[i]
		if it.Type != ItemTypeSession || it.Session == nil {
			continue
		}
		if renderDepth(i) == 0 {
			lastTopLevelIdx = i
			continue
		}
		lastSubIdxByParent[it.Session.ParentSessionID] = i
	}
	// isLastAmongSiblings reports, for a row at render depth d, whether it
	// closes its sibling list: the last top-level row at depth 0, its
	// parent's last nested child below that.
	isLastAmongSiblings := func(i int) bool {
		if renderDepth(i) == 0 {
			return i == lastTopLevelIdx
		}
		return i == lastSubIdxByParent[items[i].Session.ParentSessionID]
	}

	for i := start; i < end; i++ {
		it := &items[i]
		if it.Type != ItemTypeSession || it.Session == nil {
			continue
		}
		d := renderDepth(i)
		// Level follows the render depth: a row that Flatten nested but that
		// now draws higher (its parent filtered out) moves left with it.
		it.Level += d - it.TreeDepth
		it.TreeDepth = d

		if d == 0 {
			it.IsLastInGroup = i == lastTopLevelIdx
			it.IsLastSubSession = false
			it.ParentIsLastInGroup = false
			it.TreeGutter = nil
			if it.IsSubSession {
				// Two independent defects share this branch, both gated on
				// IsSubSession in renderSessionItem — do not "simplify" this
				// into just one of the two assignments below.
				//
				// 1. Connector glyph: a structural orphan or a child cut off
				//    from its parent by view-mode partitioning still renders
				//    at top-level indent, but renderSessionItem branches on
				//    IsSubSession first and never reads IsLastInGroup for a
				//    sub-session row — its glyph comes from IsLastSubSession.
				it.IsLastSubSession = i == lastTopLevelIdx
				// 2. Gutter: the same branch reads ParentIsLastInGroup to
				//    decide the leading space vs. "│" continuation. This row's
				//    parent has no row in this segment at all, so there is
				//    nothing to continue a line from — the gutter must stay
				//    empty (D6) regardless of whether this particular row is
				//    the last one. Unconditional, not derived from the index.
				it.ParentIsLastInGroup = true
			}
			continue
		}

		// Ancestor rows from the parent up to the top-level one; the gutter
		// lists them top-level first.
		gutter := make([]bool, d)
		for a, k := parentRow(i), d-1; a >= 0 && k >= 0; a, k = parentRow(a), k-1 {
			gutter[k] = isLastAmongSiblings(a)
		}
		it.IsLastInGroup = false
		it.IsLastSubSession = isLastAmongSiblings(i)
		it.ParentIsLastInGroup = gutter[0]
		it.TreeGutter = gutter
	}
}
