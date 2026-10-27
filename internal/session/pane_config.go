package session

import (
	"context"
	"encoding/json"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// PaneSettings is the [pane] table: the side-pane host (fork plan, item 7).
// No pane opens unless a group or session names a command, so a config
// without pane entries behaves exactly like upstream.
//
//	[pane]
//	width = "36%"        # column width, percent of the window (default 36%)
//	narrow_cols = 100    # narrower window: the agent pane is zoomed (default 100)
//	min_cols = 30        # a column thinner than this is hidden (default 30)
//	auto_open = true     # open on session start/restart (default true)
//	notify = "both"      # provider notifications: desktop, outbox, both, none
//
//	[pane.sessions."conductor-ops"]   # by session id or title
//	command = "deck-kit pane fleet"
//
//	[groups."onaza".pane]             # inherited by subgroups
//	command = "deck-kit pane"
//	stack = "deck-kit pictures"       # optional second pane below the first
//	stack_graphics = true
type PaneSettings struct {
	// Width is the host column width as a percent of the window ("36%").
	Width string `toml:"width,omitempty"`
	// NarrowCols: a window narrower than this zooms the agent pane and hides
	// the host panes (a phone through the web app). Default 100.
	NarrowCols int `toml:"narrow_cols,omitzero"`
	// MinCols: a host column narrower than this is hidden the same way.
	// Default 30.
	MinCols int `toml:"min_cols,omitzero"`
	// AutoOpen opens the configured panes when a session starts or restarts.
	// Default true (nil).
	AutoOpen *bool `toml:"auto_open,omitempty"`
	// Notify picks where a provider's "notify" control line goes: "desktop"
	// (the desktop notifier), "outbox" (the human outbox of the nearest
	// conductor up the parent chain, which the conductor bridge forwards to
	// the phone), "both" (default) or "none".
	Notify string `toml:"notify,omitempty"`
	// Sessions holds per-session pane specs keyed by session id or title.
	Sessions map[string]PaneSpec `toml:"sessions,omitempty"`
}

// PaneSpec names the pane command(s) for a group or a session.
type PaneSpec struct {
	// Command is the provider command, run by /bin/sh -c in the pane.
	Command string `toml:"command,omitempty"`
	// Graphics lets the provider's terminal passthrough sequences (kitty
	// graphics) reach the outer terminal.
	Graphics bool `toml:"graphics,omitempty"`
	// Stack is an optional second provider, stacked below the first.
	Stack string `toml:"stack,omitempty"`
	// StackGraphics is Graphics for the stacked pane.
	StackGraphics bool `toml:"stack_graphics,omitempty"`
	// Role overrides the derived role (worker, epic-conductor,
	// project-conductor, ops, other).
	Role string `toml:"role,omitempty"`
	// Width overrides [pane].width.
	Width string `toml:"width,omitempty"`
	// Disabled turns off a pane inherited from a parent group.
	Disabled bool `toml:"disabled,omitempty"`
}

// AutoOpenEnabled reports [pane].auto_open (default true).
func (p PaneSettings) AutoOpenEnabled() bool {
	return p.AutoOpen == nil || *p.AutoOpen
}

// NotifyMode returns [pane].notify normalized; unknown values mean "both".
func (p PaneSettings) NotifyMode() string {
	switch m := strings.ToLower(strings.TrimSpace(p.Notify)); m {
	case "desktop", "outbox", "none":
		return m
	}
	return "both"
}

// ResolvedPane is the pane configuration that applies to one session.
type ResolvedPane struct {
	Command, Stack          string
	Graphics, StackGraphics bool
	Role                    string // override, "" = derive
	Width                   string
	Source                  string // where Command came from: "session" or "group <path>"
}

// ResolvePane finds the pane spec for a session: [pane.sessions] by id, then
// by title, then [groups."<path>".pane] walking up the group path. The first
// entry that sets a command (or disables it) decides the command; role and
// width are taken from the first entry that sets them.
func (c *UserConfig) ResolvePane(id, title, groupPath string) ResolvedPane {
	var r ResolvedPane
	if c == nil {
		return r
	}
	type entry struct {
		spec   PaneSpec
		source string
	}
	var chain []entry
	if s, ok := c.Pane.Sessions[id]; ok && id != "" {
		chain = append(chain, entry{s, "session"})
	}
	if s, ok := c.Pane.Sessions[title]; ok && title != "" && title != id {
		chain = append(chain, entry{s, "session"})
	}
	for p := groupPath; p != ""; p = getParentPath(p) {
		if g, ok := c.Groups[p]; ok {
			chain = append(chain, entry{g.Pane, "group " + p})
		}
	}
	decided := false
	for _, e := range chain {
		if !decided && (e.spec.Command != "" || e.spec.Disabled) {
			decided = true
			if !e.spec.Disabled {
				r.Command = strings.TrimSpace(e.spec.Command)
				r.Graphics = e.spec.Graphics
				r.Stack = strings.TrimSpace(e.spec.Stack)
				r.StackGraphics = e.spec.StackGraphics
				r.Source = e.source
			}
		}
		if r.Role == "" {
			r.Role = strings.TrimSpace(e.spec.Role)
		}
		if r.Width == "" {
			r.Width = strings.TrimSpace(e.spec.Width)
		}
	}
	if r.Width == "" {
		r.Width = c.Pane.Width
	}
	return r
}

// PaneAfterStart, when set, runs after a session starts or restarts. The CLI
// sets it to open the session's configured side panes; nil (tests, library
// use) does nothing.
var PaneAfterStart func(*Instance)

func runPaneAfterStart(i *Instance) {
	if h := PaneAfterStart; h != nil {
		h(i)
	}
}

// Pane roles.
const (
	PaneRoleWorker           = "worker"
	PaneRoleEpicConductor    = "epic-conductor"
	PaneRoleProjectConductor = "project-conductor"
	PaneRoleOps              = "ops"
	PaneRoleOther            = "other"
)

// PaneParent is one session up the parent chain.
type PaneParent struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// PaneTarget is everything the side-pane host tells a provider about the
// session it sits beside. Optional values are empty when absent.
type PaneTarget struct {
	ID, Title, Profile, Group, Path, Role string
	Status                                string
	Parents                               []PaneParent // root first, direct parent last
	Item                                  string
	Branch, Worktree, Repo, PR            string
	TmuxSession, TmuxSocket               string
	// Conductor is the conductor name notifications go to ("" = none).
	Conductor string
}

// paneItemRef returns the session's item ref (any tracker's reference, set
// with `session set <id> item <ref>`); "" when none is set.
func paneItemRef(d *InstanceData) string {
	return d.ItemRef
}

// panePRNumber returns the session's PR number. Agent Deck stores no PR for
// a session yet, so it is always absent; a provider finds the PR from the
// branch and repo with its own collector.
func panePRNumber(d *InstanceData) string {
	_ = d
	return ""
}

func findInstanceData(id string, all []*InstanceData) *InstanceData {
	for _, d := range all {
		if d != nil && d.ID == id {
			return d
		}
	}
	return nil
}

// paneConductorName is the conductor identity of a stored session, "" when
// it is not a conductor (mirrors Instance.ResolveConductorName without the
// lazy adoption).
func paneConductorName(d *InstanceData) string {
	if d == nil {
		return ""
	}
	if n := strings.TrimSpace(d.ConductorName); n != "" {
		return n
	}
	if !d.IsConductor {
		return ""
	}
	if n := ConductorNameFromPath(d.ProjectPath); n != "" {
		return n
	}
	return ConductorNameFromTitle(d.Title)
}

// paneParents walks ParentSessionID up to the root, guarding against cycles.
func paneParents(d *InstanceData, all []*InstanceData) []*InstanceData {
	var chain []*InstanceData
	seen := map[string]bool{d.ID: true}
	for p := findInstanceData(d.ParentSessionID, all); p != nil && !seen[p.ID]; p = findInstanceData(p.ParentSessionID, all) {
		seen[p.ID] = true
		chain = append([]*InstanceData{p}, chain...)
	}
	return chain
}

// derivePaneRole derives the role from conductor and parent data: a
// conductor named "ops" is ops, a conductor below another conductor is an
// epic conductor, any other conductor a project conductor; a session with a
// parent is a worker; anything else is other.
func derivePaneRole(d *InstanceData, parents []*InstanceData) string {
	if name := paneConductorName(d); name != "" {
		if name == "ops" {
			return PaneRoleOps
		}
		for _, p := range parents {
			if paneConductorName(p) != "" {
				return PaneRoleEpicConductor
			}
		}
		return PaneRoleProjectConductor
	}
	if d.ParentSessionID != "" {
		return PaneRoleWorker
	}
	return PaneRoleOther
}

// PaneStatus is the session's status for the pane: the stored status,
// refined by the hook status file when the stored one is a live state (the
// hook file is written on every turn; the stored status only when a poller
// last ran).
func PaneStatus(d *InstanceData) string {
	if d == nil {
		return ""
	}
	st := string(d.Status)
	switch d.Status {
	case StatusRunning, StatusWaiting, StatusIdle:
		if hs := readHookStatusFile(d.ID); hs != nil {
			switch hs.Status {
			case "running", "waiting", "idle":
				return hs.Status
			}
		}
	}
	return st
}

// PaneFields returns the fields whose change sends a refresh: status,
// title, item and pr. Nil when the session is not in all.
func PaneFields(id string, all []*InstanceData) map[string]string {
	d := findInstanceData(id, all)
	if d == nil {
		return nil
	}
	return map[string]string{
		"status": PaneStatus(d),
		"title":  d.Title,
		"item":   paneItemRef(d),
		"pr":     panePRNumber(d),
	}
}

// BuildPaneTarget assembles the provider's view of session id from stored
// data. roleOverride replaces the derived role when set. Git values are read
// from the session's path only when it is a git checkout.
func BuildPaneTarget(profile, id string, all []*InstanceData, roleOverride string) (PaneTarget, bool) {
	d := findInstanceData(id, all)
	if d == nil {
		return PaneTarget{}, false
	}
	parents := paneParents(d, all)
	t := PaneTarget{
		ID: d.ID, Title: d.Title, Profile: profile, Group: d.GroupPath, Path: d.ProjectPath,
		Status: PaneStatus(d), Item: paneItemRef(d), PR: panePRNumber(d),
		TmuxSession: d.TmuxSession, TmuxSocket: d.TmuxSocketName,
	}
	if d.WorktreePath != "" {
		t.Path = d.WorktreePath
	}
	for _, p := range parents {
		t.Parents = append(t.Parents, PaneParent{ID: p.ID, Title: p.Title})
	}
	t.Role = strings.TrimSpace(roleOverride)
	if t.Role == "" {
		t.Role = derivePaneRole(d, parents)
	}
	// Notifications go to the conductor nearest the human: the topmost
	// conductor in the chain, else the session itself when it is one.
	for _, p := range parents {
		if n := paneConductorName(p); n != "" {
			t.Conductor = n
			break
		}
	}
	if t.Conductor == "" {
		t.Conductor = paneConductorName(d)
	}
	g := probePaneGit(t.Path)
	t.Branch, t.Repo = g.branch, g.repo
	switch {
	case d.WorktreePath != "":
		t.Worktree = d.WorktreePath
		if d.WorktreeBranch != "" {
			t.Branch = d.WorktreeBranch
		}
	case g.linked:
		t.Worktree = g.top
	}
	return t, true
}

// PaneEnvPrefix prefixes every variable the host sets for a provider.
const PaneEnvPrefix = "AGENTDECK_PANE_"

// Env returns the provider's AGENTDECK_PANE_* variables. Optional values
// (parents, item, branch, worktree, repo, PR) are left out when absent.
func (t PaneTarget) Env() []string {
	env := []string{
		PaneEnvPrefix + "SESSION_ID=" + t.ID,
		PaneEnvPrefix + "TITLE=" + t.Title,
		PaneEnvPrefix + "ROLE=" + t.Role,
		PaneEnvPrefix + "PROFILE=" + t.Profile,
		PaneEnvPrefix + "GROUP=" + t.Group,
		PaneEnvPrefix + "PATH=" + t.Path,
		PaneEnvPrefix + "STATUS=" + t.Status,
	}
	add := func(k, v string) {
		if v != "" {
			env = append(env, PaneEnvPrefix+k+"="+v)
		}
	}
	if n := len(t.Parents); n > 0 {
		ids := make([]string, n)
		for i, p := range t.Parents {
			ids[i] = p.ID
		}
		chain, _ := json.Marshal(t.Parents)
		add("PARENT_ID", t.Parents[n-1].ID)
		add("PARENT_TITLE", t.Parents[n-1].Title)
		add("PARENT_IDS", strings.Join(ids, " "))
		add("PARENT_CHAIN", string(chain))
	}
	add("ITEM", t.Item)
	add("BRANCH", t.Branch)
	add("WORKTREE", t.Worktree)
	add("REPO", t.Repo)
	add("PR", t.PR)
	return env
}

type paneGit struct {
	branch, repo, top string
	linked            bool
}

// probePaneGit reads branch, checkout top, linked-worktree flag and the
// origin repo of dir. Everything is optional: a directory that is not a git
// checkout, a detached HEAD or a missing remote just leaves values empty.
func probePaneGit(dir string) paneGit {
	var g paneGit
	if strings.TrimSpace(dir) == "" {
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// #nosec G204 -- fixed git subcommand, dir is a separate argv element.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD", "--show-toplevel", "--absolute-git-dir").Output()
	if err != nil {
		return g
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return g
	}
	if b := strings.TrimSpace(lines[0]); b != "HEAD" {
		g.branch = b
	}
	g.top = strings.TrimSpace(lines[1])
	gitDir := strings.TrimSpace(lines[2])
	g.linked = strings.Contains(gitDir, "/worktrees/")
	// #nosec G204 -- fixed git subcommand.
	if u, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output(); err == nil {
		g.repo = normalizeRepoURL(strings.TrimSpace(string(u)))
	}
	return g
}

// normalizeRepoURL turns a git remote URL into host/path without scheme,
// credentials, port or ".git": git@github.com:o/r.git, https://github.com/o/r
// and ssh://git@github.com:22/o/r.git all become github.com/o/r. A local path
// or an unparseable URL gives "".
func normalizeRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.Scheme == "file" {
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else if at := strings.Index(raw, ":"); at > 0 && !strings.HasPrefix(raw, "/") {
		// scp-like: [user@]host:path
		host = raw[:at]
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		path = raw[at+1:]
	} else {
		return ""
	}
	path = strings.Trim(strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git"), "/")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + path
}
