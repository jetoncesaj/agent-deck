package session

import (
	"fmt"
	"strings"
)

// Parent chains (multi-level sub-sessions).
//
// A session's parent link (ParentSessionID) means two things only: who the
// session escalates to and who may give it orders. A sub-session may itself
// be a parent, so links form trees: a top-level session at depth 0, its
// children at depth 1, their children at depth 2, and so on. Every consumer
// that follows links (the notifier, `session children`, the tree in the TUI
// and the web, the pane breadcrumb) acts on the direct parent or walks the
// chain one hop at a time; nothing skips a layer.
//
// Two rules keep the trees sane, and every surface that creates or moves a
// link checks them through ValidateParentLink:
//
//   - no cycles: a session cannot become its own ancestor;
//   - a depth cap: no session sits deeper than [launch] max_parent_depth
//     below its top-level session (DefaultMaxParentDepth when unset).
//
// The walkers below are defensive against links written before these rules
// existed (or by hand): a cycle or a missing parent ends the walk instead of
// looping or failing.

// DefaultMaxParentDepth is the depth cap when [launch] max_parent_depth is
// unset: a top-level session may have children, grandchildren and great-
// grandchildren, so a session sits at most three links below its top-level
// session.
//
// Three covers the deepest layering in use with one spare level: a project
// conductor (depth 0) runs an Epic conductor (1), the Epic conductor runs its
// workers (2), and a worker may still start a helper of its own (3). Deeper
// trees are rare on purpose; they usually mean a session that keeps launching
// sessions, and the cap stops that from running away. Raise it in config when
// a setup really needs more layers.
const DefaultMaxParentDepth = 3

// parentIndex maps session id to instance for chain walks.
func parentIndex(instances []*Instance) map[string]*Instance {
	byID := make(map[string]*Instance, len(instances))
	for _, inst := range instances {
		if inst != nil {
			byID[inst.ID] = inst
		}
	}
	return byID
}

// ParentChain returns inst's ancestors, its direct parent first and its
// top-level session last. The walk stops at a parent that is not among
// instances (removed, or in another profile) and at a cycle, so it always
// ends. A top-level session has an empty chain.
func ParentChain(inst *Instance, instances []*Instance) []*Instance {
	return parentChainIndexed(inst, parentIndex(instances))
}

func parentChainIndexed(inst *Instance, byID map[string]*Instance) []*Instance {
	if inst == nil {
		return nil
	}
	var chain []*Instance
	seen := map[string]bool{inst.ID: true}
	for cur := inst; ; {
		pid := strings.TrimSpace(cur.ParentSessionID)
		if pid == "" || seen[pid] {
			return chain
		}
		p := byID[pid]
		if p == nil {
			return chain
		}
		seen[pid] = true
		chain = append(chain, p)
		cur = p
	}
}

// ParentDepth is the number of links between inst and its top-level session:
// 0 for a top-level session, 1 for its child, 2 for a grandchild. Only links
// to sessions among instances count (see ParentChain).
func ParentDepth(inst *Instance, instances []*Instance) int {
	return len(ParentChain(inst, instances))
}

// childIndex maps a parent id to its direct children, in instances order.
func childIndex(instances []*Instance) map[string][]*Instance {
	kids := make(map[string][]*Instance)
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if pid := strings.TrimSpace(inst.ParentSessionID); pid != "" && pid != inst.ID {
			kids[pid] = append(kids[pid], inst)
		}
	}
	return kids
}

// DescendantEntry is one session below a parent, with its depth relative to
// that parent (1 = direct child).
type DescendantEntry struct {
	Instance *Instance
	Depth    int
}

// Descendants returns every session below parentID, depth first: each child
// is followed by its own subtree before the next child, children in
// instances order. maxDepth limits how far down it goes (1 = direct children
// only); 0 or less means no limit. A cycle is cut where it closes.
func Descendants(parentID string, instances []*Instance, maxDepth int) []DescendantEntry {
	kids := childIndex(instances)
	var out []DescendantEntry
	seen := map[string]bool{parentID: true}
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if maxDepth > 0 && depth > maxDepth {
			return
		}
		for _, k := range kids[id] {
			if seen[k.ID] {
				continue
			}
			seen[k.ID] = true
			out = append(out, DescendantEntry{Instance: k, Depth: depth})
			walk(k.ID, depth+1)
		}
	}
	walk(parentID, 1)
	return out
}

// SubtreeHeight is how many levels sit below inst: 0 for a session with no
// children, 1 when it has children but no grandchildren, and so on.
func SubtreeHeight(inst *Instance, instances []*Instance) int {
	if inst == nil {
		return 0
	}
	h := 0
	for _, d := range Descendants(inst.ID, instances, 0) {
		if d.Depth > h {
			h = d.Depth
		}
	}
	return h
}

// IsAncestor reports whether ancestorID is on inst's parent chain.
func IsAncestor(ancestorID string, inst *Instance, instances []*Instance) bool {
	for _, p := range ParentChain(inst, instances) {
		if p.ID == ancestorID {
			return true
		}
	}
	return false
}

// ResolveMaxParentDepth turns a configured [launch] max_parent_depth into the
// cap in force: a positive value as is, anything else the default.
func ResolveMaxParentDepth(configured int) int {
	if configured > 0 {
		return configured
	}
	return DefaultMaxParentDepth
}

// ParentLinkError is why a parent link was refused. Kind is one of the
// ParentLink* constants so callers can tell a cycle from the depth cap.
type ParentLinkError struct {
	Kind    string
	Message string
}

func (e *ParentLinkError) Error() string { return e.Message }

const (
	ParentLinkSelf  = "self"
	ParentLinkCycle = "cycle"
	ParentLinkDepth = "depth"
)

// ValidateParentLink checks that child may be linked under parent: not
// itself, not one of its own descendants (that would make it its own
// ancestor), and not so deep that the child, or the deepest session of the
// child's own subtree, would sit more than maxDepth links below its
// top-level session. child is nil for a session that does not exist yet
// (launch, add). maxDepth is the cap in force (ResolveMaxParentDepth).
func ValidateParentLink(child, parent *Instance, instances []*Instance, maxDepth int) error {
	if parent == nil {
		return nil
	}
	maxDepth = ResolveMaxParentDepth(maxDepth)
	if child != nil {
		if child.ID == parent.ID {
			return &ParentLinkError{Kind: ParentLinkSelf, Message: "cannot set session as its own parent"}
		}
		if IsAncestor(child.ID, parent, instances) {
			return &ParentLinkError{Kind: ParentLinkCycle, Message: fmt.Sprintf(
				"cannot link '%s' under '%s': '%s' is below '%s', and a session cannot become its own ancestor",
				child.Title, parent.Title, parent.Title, child.Title)}
		}
	}
	// The child is about to leave its current place, so its own chain does
	// not count; the parent's chain is the new one.
	depth := ParentDepth(parent, instances) + 1
	height := 0
	if child != nil {
		height = SubtreeHeight(child, instances)
	}
	if depth+height > maxDepth {
		what := fmt.Sprintf("the new session would sit at depth %d", depth)
		if child != nil {
			what = fmt.Sprintf("'%s' would sit at depth %d", child.Title, depth)
			if height > 0 {
				what += fmt.Sprintf(" and its own sub-sessions down to depth %d", depth+height)
			}
		}
		return &ParentLinkError{Kind: ParentLinkDepth, Message: fmt.Sprintf(
			"cannot link under '%s': %s, deeper than [launch] max_parent_depth = %d (depth 0 is a top-level session)",
			parent.Title, what, maxDepth)}
	}
	return nil
}

// CanTakeChild reports whether a new session may be linked under parent
// without passing the depth cap.
func CanTakeChild(parent *Instance, instances []*Instance, maxDepth int) bool {
	return ValidateParentLink(nil, parent, instances, maxDepth) == nil
}

// RelinkChildrenOfArchived moves the live children of a session being
// archived one layer up, under the archived session's own parent, so their
// escalations keep reaching a session that reads them: an archived session
// is stopped and never drains its inbox. The layer that went away is the
// only one skipped; nothing else changes.
//
//   - Only direct children that are not archived themselves move (an
//     archived child keeps its history). Their own subtrees move with them.
//   - A top-level session being archived has no layer above it: its children
//     keep their link, and the archive output names them so they can be
//     re-linked by hand.
//   - Unarchiving does not move them back (`session set-parent` does).
//
// The children are changed in memory and returned; the caller persists them.
func RelinkChildrenOfArchived(archived *Instance, instances []*Instance) (relinked []*Instance) {
	if archived == nil {
		return nil
	}
	chain := ParentChain(archived, instances)
	if len(chain) == 0 {
		return nil
	}
	grandparent := chain[0]
	for _, child := range Descendants(archived.ID, instances, 1) {
		inst := child.Instance
		if inst.IsArchived() {
			continue
		}
		inst.SetParentWithPath(grandparent.ID, grandparent.ProjectPath)
		relinked = append(relinked, inst)
	}
	return relinked
}

// LiveChildren returns the direct children of parent that are not archived.
func LiveChildren(parent *Instance, instances []*Instance) []*Instance {
	if parent == nil {
		return nil
	}
	var out []*Instance
	for _, d := range Descendants(parent.ID, instances, 1) {
		if !d.Instance.IsArchived() {
			out = append(out, d.Instance)
		}
	}
	return out
}
