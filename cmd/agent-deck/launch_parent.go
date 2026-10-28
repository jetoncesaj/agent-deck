package main

import (
	"errors"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// launchParentError is what selectLaunchParent returns instead of exiting, so
// each call site (launch, add, the capacity pre-check) keeps its own surface.
// UnresolvedID is set only for a caller id that resolves to nothing.
type launchParentError struct {
	Code         string
	Message      string
	UnresolvedID string
}

func (e *launchParentError) Error() string { return e.Message }

// launchParentErrorParts splits a selector error into message and error code.
func launchParentErrorParts(err error) (message, code string) {
	var lpe *launchParentError
	if errors.As(err, &lpe) {
		return lpe.Message, lpe.Code
	}
	return err.Error(), ErrCodeInvalidOperation
}

// launchParentRule is the [launch] config the parent selector follows.
type launchParentRule struct {
	// NestUnderParent is [launch] nest_under_parent: from a sub-session
	// caller, link one hop up (under the caller's own parent) instead of
	// under the caller.
	NestUnderParent bool
	// MaxDepth is [launch] max_parent_depth, resolved
	// (session.ResolveMaxParentDepth); 0 means the default.
	MaxDepth int
}

// selectLaunchParent picks the session a new session is linked to. It is the
// one place launch, add and the capacity pre-check decide this, so they cannot
// drift apart again.
//
// Sub-sessions may be parents (multi-level trees), within the depth cap
// rule.MaxDepth ([launch] max_parent_depth):
//
//   - A parent the user names (explicit) is used as is, top-level or not. A
//     parent already at the cap is refused with the depth error; the user
//     asked for that exact parent, so nothing is picked for them instead.
//   - A parent agent-deck picks itself (the launching session) is the caller,
//     top-level or not. Two things replace the caller with one of its
//     ancestors, with a one-line note and launchedBy set to the caller:
//     rule.NestUnderParent ([launch] nest_under_parent = true, the old
//     one-hop flattening, now asked for explicitly), and the cap (a caller at
//     the cap links its session under the nearest ancestor that still has
//     room). A caller whose parent link points at a session that no longer
//     exists starts the session top-level with a note, as --no-parent does.
//
// noParent means top-level and is honoured from every caller; with
// nest_under_parent on, --no-parent from a sub-session notes where the
// session would otherwise have landed. A caller id that resolves to nothing
// is an error, except with noParent.
func selectLaunchParent(explicit string, noParent bool, rule launchParentRule, instances []*session.Instance) (parent, launchedBy *session.Instance, note string, err error) {
	if explicit != "" {
		named, message, _ := ResolveSession(explicit, instances)
		if named == nil {
			return nil, nil, "", &launchParentError{Code: ErrCodeNotFound, Message: message}
		}
		if err := session.ValidateParentLink(nil, named, instances, rule.MaxDepth); err != nil {
			return nil, nil, "", &launchParentError{Code: ErrCodeInvalidOperation, Message: err.Error()}
		}
		return named, nil, "", nil
	}
	if noParent && !rule.NestUnderParent {
		return nil, nil, "", nil
	}

	caller, unresolved := resolveAutoParentInstanceChecked(instances)
	if noParent {
		if caller == nil || !caller.IsSubSession() {
			return nil, nil, "", nil
		}
		grandparent := findInstanceByID(instances, caller.ParentSessionID)
		if grandparent == nil {
			return nil, nil, "", nil
		}
		return nil, nil, fmt.Sprintf("--no-parent given from sub-session %s; started top-level instead of under its parent %s", caller.Title, grandparent.Title), nil
	}
	if caller == nil {
		if unresolved != "" {
			return nil, nil, "", &launchParentError{
				Code:         ErrCodeNotFound,
				UnresolvedID: unresolved,
				Message: fmt.Sprintf("automatic parent %q could not be resolved; use --parent with a valid session or --no-parent for an intentional top-level session",
					unresolved),
			}
		}
		return nil, nil, "", nil
	}
	if !caller.IsSubSession() {
		return caller, nil, "", nil
	}
	if !rule.NestUnderParent && session.CanTakeChild(caller, instances, rule.MaxDepth) {
		return caller, nil, "", nil
	}
	return underAncestor(caller, rule, instances)
}

// underAncestor returns the ancestor of sub-session inst that a session inst
// launches is linked to when it does not go under inst itself: inst's own
// parent, or, when that one is at the depth cap too, the nearest ancestor
// above it with room. inst is the launcher. A broken chain (a parent that no
// longer exists) gives a top-level session and a note.
func underAncestor(inst *session.Instance, rule launchParentRule, instances []*session.Instance) (parent, launchedBy *session.Instance, note string, err error) {
	parent = findInstanceByID(instances, inst.ParentSessionID)
	if parent == nil {
		return nil, nil, fmt.Sprintf("parent %s of sub-session %s (%s) does not exist; started top-level as --no-parent does", inst.ParentSessionID, inst.Title, inst.ID), nil
	}
	atCap := !session.CanTakeChild(inst, instances, rule.MaxDepth)
	maxDepth := session.ResolveMaxParentDepth(rule.MaxDepth)
	for _, candidate := range session.ParentChain(inst, instances) {
		if !session.CanTakeChild(candidate, instances, rule.MaxDepth) {
			continue
		}
		switch {
		case candidate != parent:
			note = fmt.Sprintf("%s and its parent %s are at the depth limit ([launch] max_parent_depth = %d); linked under their ancestor %s", inst.Title, parent.Title, maxDepth, candidate.Title)
		case atCap:
			note = fmt.Sprintf("%s is at the depth limit ([launch] max_parent_depth = %d); linked under its parent %s", inst.Title, maxDepth, candidate.Title)
		default: // [launch] nest_under_parent = true
			note = fmt.Sprintf("parent %s is a sub-session; linked under its parent %s", inst.Title, candidate.Title)
		}
		return candidate, inst, note, nil
	}
	// Unreachable while the cap is at least 1: the top of the chain is a
	// top-level session, which always has room. Kept for a chain whose top
	// was removed mid-walk.
	return nil, nil, fmt.Sprintf("no ancestor of sub-session %s (%s) has room under [launch] max_parent_depth; started top-level", inst.Title, inst.ID), nil
}

// launchParentRuleFromConfig reads [launch] nest_under_parent and
// max_parent_depth. A config that cannot be read counts as both unset.
func launchParentRuleFromConfig() launchParentRule {
	cfg, err := session.LoadUserConfig()
	if err != nil || cfg == nil {
		return launchParentRule{}
	}
	return launchParentRule{NestUnderParent: cfg.Launch.NestUnderParent, MaxDepth: cfg.Launch.MaxParentDepth}
}

// launchNestUnderParent reports whether [launch] nest_under_parent is on. A
// config that cannot be read counts as off.
func launchNestUnderParent() bool {
	return launchParentRuleFromConfig().NestUnderParent
}

// launchMaxParentDepth is the [launch] max_parent_depth cap in force.
func launchMaxParentDepth() int {
	return session.ResolveMaxParentDepth(launchParentRuleFromConfig().MaxDepth)
}

func findInstanceByID(instances []*session.Instance, id string) *session.Instance {
	for _, inst := range instances {
		if inst.ID == id {
			return inst
		}
	}
	return nil
}
