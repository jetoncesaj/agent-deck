package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/desknotify"
)

func TestParsePaneNotify(t *testing.T) {
	tests := []struct {
		line, tier, text string
		ok               bool
	}{
		{"notify PR 12 can land", "urgent", "PR 12 can land", true},
		{"notify urgent PR 12 can land", "urgent", "PR 12 can land", true},
		{"notify info  digest   line ", "info", "digest line", true},
		{"notify ", "", "", false},
		{"notify info", "urgent", "info", true},
		{"zoom", "", "", false},
	}
	for _, tt := range tests {
		tier, text, ok := parsePaneNotify(tt.line)
		if ok != tt.ok || (ok && (tier != tt.tier || text != tt.text)) {
			t.Errorf("parsePaneNotify(%q) = %q %q %v, want %q %q %v", tt.line, tier, text, ok, tt.tier, tt.text, tt.ok)
		}
	}
	long := "notify " + strings.Repeat("é", 400)
	if _, text, _ := parsePaneNotify(long); len([]rune(text)) != 300 {
		t.Errorf("long text kept %d runes, want 300", len([]rune(text)))
	}
}

type paneFakeBackend struct {
	mu    sync.Mutex
	calls []string
}

func (f *paneFakeBackend) Name() string    { return "fake" }
func (f *paneFakeBackend) Available() bool { return true }
func (f *paneFakeBackend) Notify(_ context.Context, title, body string) error {
	f.mu.Lock()
	f.calls = append(f.calls, title+": "+body)
	f.mu.Unlock()
	return nil
}

func TestPaneNotifierRoutesAndRateLimits(t *testing.T) {
	for _, tc := range []struct {
		mode, conductor    string
		wantDesk, wantOutb int
	}{
		{"both", "onaza", 1, 1},
		{"desktop", "onaza", 1, 0},
		{"outbox", "onaza", 0, 1},
		{"both", "", 1, 0}, // no conductor up the chain: desktop only
		{"none", "onaza", 0, 0},
	} {
		t.Run(tc.mode+"/"+tc.conductor, func(t *testing.T) {
			desk := &paneFakeBackend{}
			var outbox []string
			n := newPaneNotifier(tc.mode, "w1", tc.conductor, "personal")
			n.desk = desknotify.NewWithBackends(desk)
			var mu sync.Mutex
			n.appendOutbox = func(c, tier, text string) error {
				mu.Lock()
				outbox = append(outbox, c+"|"+tier+"|"+text)
				mu.Unlock()
				return nil
			}
			n.handle("notify info PR 12 entered LAND")
			n.handle("notify second within the rate limit")
			n.wait()
			if len(desk.calls) != tc.wantDesk || len(outbox) != tc.wantOutb {
				t.Fatalf("desktop %q outbox %q", desk.calls, outbox)
			}
			if tc.wantDesk == 1 && desk.calls[0] != "w1: PR 12 entered LAND" {
				t.Fatalf("desktop call %q", desk.calls[0])
			}
			if tc.wantOutb == 1 && outbox[0] != "onaza|info|[pane w1] PR 12 entered LAND" {
				t.Fatalf("outbox record %q", outbox[0])
			}
		})
	}
}

func TestPaneHostCommandQuoting(t *testing.T) {
	cmd := paneHostCommand("/opt/a deck/agent-deck", "per'sonal", "id-1", "sock", `deck-kit pane --title "it's"`, true)("%3", 2)
	if !strings.HasPrefix(cmd, "exec ") {
		t.Fatalf("host command must exec: %q", cmd)
	}
	// What the shell sees after exec: print each argument on its own line.
	out, err := exec.Command("/bin/sh", "-c", "printf '%s\\n' "+strings.TrimPrefix(cmd, "exec ")).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/a deck/agent-deck", "-p", "per'sonal", "pane", "run", "--session", "id-1", "--slot", "2",
		"--agent-pane", "%3", "--tmux-socket", "sock", "--command", `deck-kit pane --title "it's"`, "--graphics"}
	if got := strings.Split(strings.TrimRight(string(out), "\n"), "\n"); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func paneE2EAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func paneE2EPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

func paneE2EWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The real binary on a private tmux socket: a group's pane command opens on
// `session start`, gets its AGENTDECK_PANE_* environment without the agent's
// identity, is reported by `pane status`, closes and reopens by CLI, and is
// gone after `session stop`.
func TestPaneCLIAutoOpenStatusCloseAndStop(t *testing.T) {
	skipIfNoTmuxBinaryCLI(t)
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	home := t.TempDir()
	project := t.TempDir()
	socket := fmt.Sprintf("adpane-e2e-%d", time.Now().UnixNano()%1000000)
	envFile := filepath.Join(home, "pane-env.txt")
	pidFile := filepath.Join(home, "prov.pid")
	inLog := filepath.Join(home, "pane-in.log")
	provider := fmt.Sprintf("env > %s; echo $$ > %s; exec cat > %s", envFile, pidFile, inLog)
	cfgPath := filepath.Join(home, ".agent-deck", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[tmux]\nsocket_name = %q\n\n[groups.\"panes\".pane]\ncommand = %q\n", socket, provider)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })

	stdout, stderr, code := runAgentDeck(t, home, "-p", "_test", "add", "-t", "paned", "-g", "panes", "-cmd", "sleep 600", "--no-parent", "--json", project)
	if code != 0 {
		t.Fatalf("add: %s %s", stdout, stderr)
	}
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &added); err != nil || added.ID == "" {
		t.Fatalf("add output %q: %v", stdout, err)
	}
	if stdout, stderr, code = runAgentDeck(t, home, "-p", "_test", "session", "start", added.ID); code != 0 {
		t.Fatalf("start: %s %s", stdout, stderr)
	}
	pid := paneE2EPID(t, pidFile)
	paneE2EWait(t, "hello line", func() bool {
		b, _ := os.ReadFile(inLog)
		return strings.HasPrefix(string(b), "hello 2\n")
	})

	env, _ := os.ReadFile(envFile)
	for _, want := range []string{
		"AGENTDECK_PANE_SESSION_ID=" + added.ID, "AGENTDECK_PANE_TITLE=paned", "AGENTDECK_PANE_ROLE=other",
		"AGENTDECK_PANE_PROFILE=_test", "AGENTDECK_PANE_GROUP=panes", "AGENTDECK_PANE_SLOT=1", "AGENTDECK_PANE_CONTROL_FD=3",
	} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("provider env lacks %s", want)
		}
	}
	for _, absent := range []string{"AGENTDECK_INSTANCE_ID=", "AGENTDECK_PANE_PARENT_ID=", "AGENTDECK_PANE_ITEM=", "AGENTDECK_PANE_PR="} {
		if strings.Contains("\n"+string(env), "\n"+absent) {
			t.Errorf("provider env has %s", absent)
		}
	}

	type statusOut struct {
		Running    bool `json:"running"`
		Configured struct {
			Command string `json:"command"`
			Source  string `json:"source"`
		} `json:"configured"`
		Panes []struct {
			Slot    int    `json:"slot"`
			Command string `json:"command"`
		} `json:"panes"`
	}
	status := func() statusOut {
		t.Helper()
		out, errOut, c := runAgentDeck(t, home, "-p", "_test", "pane", "status", added.ID, "--json")
		if c != 0 {
			t.Fatalf("pane status: %s %s", out, errOut)
		}
		var s statusOut
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatalf("pane status json %q: %v", out, err)
		}
		return s
	}
	s := status()
	if !s.Running || s.Configured.Command != provider || s.Configured.Source != "group panes" || len(s.Panes) != 1 || s.Panes[0].Slot != 1 {
		t.Fatalf("pane status = %+v", s)
	}

	// Opening again is a no-op.
	out, errOut, c := runAgentDeck(t, home, "-p", "_test", "pane", "open", added.ID)
	if c != 0 || !strings.Contains(out, "already open") {
		t.Fatalf("pane open again: %d %s %s", c, out, errOut)
	}

	if out, errOut, c = runAgentDeck(t, home, "-p", "_test", "pane", "close", added.ID); c != 0 {
		t.Fatalf("pane close: %s %s", out, errOut)
	}
	paneE2EWait(t, "provider gone after pane close", func() bool { return !paneE2EAlive(pid) })
	if s := status(); len(s.Panes) != 0 {
		t.Fatalf("panes after close: %+v", s.Panes)
	}

	_ = os.Remove(pidFile)
	if out, errOut, c = runAgentDeck(t, home, "-p", "_test", "pane", "open", added.ID); c != 0 || !strings.Contains(out, "opened") {
		t.Fatalf("pane reopen: %d %s %s", c, out, errOut)
	}
	pid = paneE2EPID(t, pidFile)

	// A restart respawns the agent pane: the side pane and its provider stay.
	if out, errOut, c = runAgentDeck(t, home, "-p", "_test", "session", "restart", added.ID); c != 0 {
		t.Fatalf("session restart: %s %s", out, errOut)
	}
	time.Sleep(1500 * time.Millisecond) // more than one probe period of the host
	if s := status(); len(s.Panes) != 1 || !paneE2EAlive(pid) {
		t.Fatalf("after restart: panes %+v, provider alive %v", s.Panes, paneE2EAlive(pid))
	}

	if out, errOut, c = runAgentDeck(t, home, "-p", "_test", "session", "stop", added.ID); c != 0 {
		t.Fatalf("session stop: %s %s", out, errOut)
	}
	paneE2EWait(t, "provider gone after session stop", func() bool { return !paneE2EAlive(pid) })
}

// Without pane configuration a session gets no pane and `pane open` says
// why: the default is upstream behaviour.
func TestPaneCLINoConfigNoPane(t *testing.T) {
	skipIfNoTmuxBinaryCLI(t)
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	home := t.TempDir()
	project := t.TempDir()
	socket := fmt.Sprintf("adpane-none-%d", time.Now().UnixNano()%1000000)
	cfgPath := filepath.Join(home, ".agent-deck", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[tmux]\nsocket_name = %q\n", socket)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	stdout, stderr, code := runAgentDeck(t, home, "-p", "_test", "add", "-t", "plain", "-cmd", "sleep 600", "--no-parent", "--json", project)
	if code != 0 {
		t.Fatalf("add: %s %s", stdout, stderr)
	}
	var added struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(stdout), &added)
	if stdout, stderr, code = runAgentDeck(t, home, "-p", "_test", "session", "start", added.ID); code != 0 {
		t.Fatalf("start: %s %s", stdout, stderr)
	}
	show, _, _ := runAgentDeck(t, home, "-p", "_test", "session", "show", added.ID, "--json")
	var shown struct {
		Tmux string `json:"tmux_session"`
	}
	_ = json.Unmarshal([]byte(show), &shown)
	tmuxName := shown.Tmux
	if tmuxName == "" {
		t.Fatalf("no tmux session in show output %q", show)
	}
	out, err := exec.Command("tmux", "-L", socket, "list-panes", "-t", tmuxName+":^", "-F", "#{pane_id}").Output()
	if err != nil || len(strings.Fields(string(out))) != 1 {
		t.Fatalf("panes of an unconfigured session: %q %v", out, err)
	}
	_, errOut, c := runAgentDeck(t, home, "-p", "_test", "pane", "open", added.ID)
	if c == 0 || !strings.Contains(errOut, "no pane command") {
		t.Fatalf("pane open without config: %d %s", c, errOut)
	}
}
