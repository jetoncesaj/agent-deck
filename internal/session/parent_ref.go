package session

// ParentRef returns inst's parent session id and that parent's title, as
// `list --json` and `session show --json` report them (parent_id,
// parent_title), so a consumer can build the parent chain without guessing
// from titles. Both are "" without a parent; the title is "" when the parent
// is not among instances (removed, or listed in another profile).
func ParentRef(inst *Instance, instances []*Instance) (id, title string) {
	if inst == nil || inst.ParentSessionID == "" {
		return "", ""
	}
	for _, candidate := range instances {
		if candidate != nil && candidate.ID == inst.ParentSessionID {
			return inst.ParentSessionID, candidate.GetTitleThreadSafe()
		}
	}
	return inst.ParentSessionID, ""
}
