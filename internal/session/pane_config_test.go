package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

const paneConfigTOML = `
[pane]
width = "40%"
notify = "outbox"

[pane.sessions."conductor-ops"]
command = "deck-kit pane fleet"
role = "ops"

[pane.sessions."id-quiet"]
disabled = true

[groups."onaza".pane]
command = "deck-kit pane"
stack = "deck-kit pictures"
stack_graphics = true

[groups."onaza/epics".pane]
role = "epic-conductor"
width = "30%"

[groups."plain"]
default_path = "/tmp"
`

func TestResolvePaneFromTOML(t *testing.T) {
	var cfg UserConfig
	if _, err := toml.Decode(paneConfigTOML, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tests := []struct {
		name               string
		id, title, group   string
		wantCmd, wantStack string
		wantRole, wantW    string
		wantSrc            string
	}{
		{"no config, no pane", "x", "x", "plain", "", "", "", "40%", ""},
		{"ungrouped session", "x", "x", "", "", "", "", "40%", ""},
		{"group command", "w", "worker", "onaza", "deck-kit pane", "deck-kit pictures", "", "40%", "group onaza"},
		{"subgroup inherits command, sets role and width", "e", "epic", "onaza/epics/e1", "deck-kit pane", "deck-kit pictures", "epic-conductor", "30%", "group onaza"},
		{"session by title wins over group", "c", "conductor-ops", "onaza", "deck-kit pane fleet", "", "ops", "40%", "session"},
		{"session by id disables the group's pane", "id-quiet", "q", "onaza", "", "", "", "40%", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := cfg.ResolvePane(tt.id, tt.title, tt.group)
			if r.Command != tt.wantCmd || r.Stack != tt.wantStack || r.Role != tt.wantRole || r.Width != tt.wantW || r.Source != tt.wantSrc {
				t.Fatalf("ResolvePane = %+v, want cmd=%q stack=%q role=%q width=%q src=%q", r, tt.wantCmd, tt.wantStack, tt.wantRole, tt.wantW, tt.wantSrc)
			}
		})
	}
	if !cfg.ResolvePane("w", "worker", "onaza").StackGraphics {
		t.Fatal("stack_graphics not carried")
	}
	if cfg.Pane.NotifyMode() != "outbox" || !cfg.Pane.AutoOpenEnabled() {
		t.Fatalf("notify=%q auto_open=%v", cfg.Pane.NotifyMode(), cfg.Pane.AutoOpenEnabled())
	}
	var nilCfg *UserConfig
	if r := nilCfg.ResolvePane("a", "b", "c"); r.Command != "" {
		t.Fatalf("nil config resolved %+v", r)
	}
}

func paneFleet() []*InstanceData {
	return []*InstanceData{
		{ID: "ops1", Title: "conductor-ops", IsConductor: true, ConductorName: "ops", Status: StatusIdle},
		{ID: "pc1", Title: "conductor-onaza", IsConductor: true, ConductorName: "onaza", GroupPath: "onaza", Status: StatusRunning},
		{ID: "ec1", Title: "epic-12", IsConductor: true, ConductorName: "onaza-epic-12", ParentSessionID: "pc1", GroupPath: "onaza/epics", Status: StatusRunning},
		{ID: "w1", Title: "ONAZA-13 worker", ParentSessionID: "ec1", GroupPath: "onaza/epics", ProjectPath: "/nonexistent/pane-test", Status: StatusWaiting},
		{ID: "solo", Title: "scratch", Status: StatusStopped},
		{ID: "loopA", Title: "a", ParentSessionID: "loopB"},
		{ID: "loopB", Title: "b", ParentSessionID: "loopA"},
	}
}

func TestDerivePaneRole(t *testing.T) {
	all := paneFleet()
	for id, want := range map[string]string{
		"ops1": PaneRoleOps, "pc1": PaneRoleProjectConductor, "ec1": PaneRoleEpicConductor,
		"w1": PaneRoleWorker, "solo": PaneRoleOther, "loopA": PaneRoleWorker,
	} {
		tgt, ok := BuildPaneTarget("personal", id, all, "")
		if !ok {
			t.Fatalf("%s not found", id)
		}
		if tgt.Role != want {
			t.Errorf("role(%s) = %q, want %q", id, tgt.Role, want)
		}
	}
	if tgt, _ := BuildPaneTarget("personal", "w1", all, "ops"); tgt.Role != "ops" {
		t.Fatalf("override ignored: %q", tgt.Role)
	}
	if _, ok := BuildPaneTarget("personal", "missing", all, ""); ok {
		t.Fatal("missing session reported found")
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestPaneTargetEnvWorkerChainAndAbsentValues(t *testing.T) {
	tgt, _ := BuildPaneTarget("personal", "w1", paneFleet(), "")
	env := envMap(tgt.Env())
	want := map[string]string{
		"AGENTDECK_PANE_SESSION_ID":   "w1",
		"AGENTDECK_PANE_TITLE":        "ONAZA-13 worker",
		"AGENTDECK_PANE_ROLE":         "worker",
		"AGENTDECK_PANE_PROFILE":      "personal",
		"AGENTDECK_PANE_GROUP":        "onaza/epics",
		"AGENTDECK_PANE_PATH":         "/nonexistent/pane-test",
		"AGENTDECK_PANE_STATUS":       "waiting",
		"AGENTDECK_PANE_PARENT_ID":    "ec1",
		"AGENTDECK_PANE_PARENT_TITLE": "epic-12",
		"AGENTDECK_PANE_PARENT_IDS":   "pc1 ec1",
		"AGENTDECK_PANE_PARENT_CHAIN": `[{"id":"pc1","title":"conductor-onaza"},{"id":"ec1","title":"epic-12"}]`,
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// No git checkout, no item, no PR: those variables are absent, not empty.
	for _, k := range []string{"ITEM", "BRANCH", "WORKTREE", "REPO", "PR"} {
		if _, ok := env["AGENTDECK_PANE_"+k]; ok {
			t.Errorf("AGENTDECK_PANE_%s present without a value", k)
		}
	}
	if len(env) != len(want) {
		t.Errorf("env has %d vars, want %d: %v", len(env), len(want), env)
	}
	if tgt.Conductor != "onaza" {
		t.Errorf("notify conductor = %q, want the topmost conductor onaza", tgt.Conductor)
	}
	solo, _ := BuildPaneTarget("personal", "solo", paneFleet(), "")
	if _, ok := envMap(solo.Env())["AGENTDECK_PANE_PARENT_ID"]; ok || solo.Conductor != "" {
		t.Errorf("top-level session got parent vars or a conductor: %+v", solo)
	}
}

func TestPaneTargetCarriesItemRef(t *testing.T) {
	fleet := paneFleet()
	for _, d := range fleet {
		if d.ID == "w1" {
			d.ItemRef = "ONAZA-13"
		}
	}
	tgt, _ := BuildPaneTarget("personal", "w1", fleet, "")
	if got := envMap(tgt.Env())["AGENTDECK_PANE_ITEM"]; got != "ONAZA-13" {
		t.Errorf("AGENTDECK_PANE_ITEM = %q, want ONAZA-13", got)
	}
	if got := PaneFields("w1", fleet)["item"]; got != "ONAZA-13" {
		t.Errorf("PaneFields item = %q, want ONAZA-13", got)
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestPaneTargetGitValues(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	gitT(t, repo, "remote", "add", "origin", "git@github.com:jetoncesaj/deck-kit.git")
	wt := filepath.Join(filepath.Dir(repo), "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "fork/pane", wt)

	all := []*InstanceData{
		{ID: "main", Title: "m", ProjectPath: repo},
		{ID: "linked", Title: "l", ProjectPath: wt},
		{ID: "managed", Title: "g", ProjectPath: wt, WorktreePath: wt, WorktreeBranch: "fork/pane"},
	}
	m, _ := BuildPaneTarget("p", "main", all, "")
	env := envMap(m.Env())
	if env["AGENTDECK_PANE_BRANCH"] != "main" || env["AGENTDECK_PANE_REPO"] != "github.com/jetoncesaj/deck-kit" {
		t.Fatalf("main checkout env: %v", env)
	}
	if _, ok := env["AGENTDECK_PANE_WORKTREE"]; ok {
		t.Fatalf("main checkout reported as a worktree: %v", env)
	}
	for _, id := range []string{"linked", "managed"} {
		tgt, _ := BuildPaneTarget("p", id, all, "")
		env := envMap(tgt.Env())
		gotWT, _ := filepath.EvalSymlinks(env["AGENTDECK_PANE_WORKTREE"])
		wantWT, _ := filepath.EvalSymlinks(wt)
		if env["AGENTDECK_PANE_BRANCH"] != "fork/pane" || gotWT != wantWT {
			t.Fatalf("%s env: %v", id, env)
		}
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:o/r.git":               "github.com/o/r",
		"https://github.com/o/r":               "github.com/o/r",
		"https://user:tok@GitHub.com/o/r.git/": "github.com/o/r",
		"ssh://git@gitlab.example:2222/g/s/r":  "gitlab.example/g/s/r",
		"/srv/git/r.git":                       "",
		"file:///srv/git/r.git":                "",
		"":                                     "",
	} {
		if got := normalizeRepoURL(in); got != want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPaneFields(t *testing.T) {
	f := PaneFields("w1", paneFleet())
	if f["status"] != "waiting" || f["title"] != "ONAZA-13 worker" || f["item"] != "" || f["pr"] != "" {
		t.Fatalf("PaneFields = %v", f)
	}
	if PaneFields("missing", paneFleet()) != nil {
		t.Fatal("missing session must give nil fields")
	}
}

func TestPaneWatchStatuses(t *testing.T) {
	all := append(paneFleet(), &InstanceData{ID: "held1", Title: "held", Status: StatusError})
	if err := writeAuthHoldRecord(AuthHoldRecord{InstanceID: "held1", Reason: AuthHoldReasonLive, Evidence: "401", Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearAuthHoldRecord("held1") })
	// A healthy session with a stale hold sidecar reports no substate.
	if err := writeAuthHoldRecord(AuthHoldRecord{InstanceID: "pc1", Reason: AuthHoldReasonLive, Evidence: "old", Timestamp: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearAuthHoldRecord("pc1") })

	got := PaneWatchStatuses([]string{"w1", "solo", "held1", "pc1", "gone"}, all)
	want := map[string]string{"w1": "waiting", "solo": "stopped", "held1": "error auth-401", "pc1": "running", "gone": "missing"}
	if len(got) != len(want) {
		t.Fatalf("PaneWatchStatuses = %v", got)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("%s = %q, want %q", id, got[id], v)
		}
	}
}

// Four layers: a worker's own helper is a worker too, its breadcrumb lists
// every layer root first, and notifications still go to the topmost
// conductor in the chain.
func TestBuildPaneTarget_FourLayerChain(t *testing.T) {
	all := append(paneFleet(), &InstanceData{ID: "h1", Title: "helper", ParentSessionID: "w1", GroupPath: "onaza/epics", ProjectPath: "/nonexistent/pane-test"})
	tgt, ok := BuildPaneTarget("personal", "h1", all, "")
	if !ok {
		t.Fatal("helper not found")
	}
	if tgt.Role != PaneRoleWorker {
		t.Fatalf("role %q, want worker", tgt.Role)
	}
	var chain []string
	for _, p := range tgt.Parents {
		chain = append(chain, p.ID)
	}
	if strings.Join(chain, ",") != "pc1,ec1,w1" {
		t.Fatalf("breadcrumb %v, want pc1,ec1,w1 (root first, direct parent last)", chain)
	}
	if tgt.Conductor != "onaza" {
		t.Fatalf("conductor %q, want the topmost conductor onaza", tgt.Conductor)
	}
}
