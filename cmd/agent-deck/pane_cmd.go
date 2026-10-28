package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/term"

	"github.com/asheshgoplani/agent-deck/internal/childenv"
	"github.com/asheshgoplani/agent-deck/internal/desknotify"
	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/panehost"
	"github.com/asheshgoplani/agent-deck/internal/panenav"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

var paneLog = logging.ForComponent(logging.CompSession)

func printPaneUsage() {
	fmt.Println("Usage: agent-deck pane <open|close|status|refresh> <id|title> [options]")
	fmt.Println()
	fmt.Println("Side panes: a provider command Agent Deck runs beside a session and owns.")
	fmt.Println("The command comes from [groups.\"<group>\".pane] or [pane.sessions.\"<id|title>\"]")
	fmt.Println("in config.toml (none by default), or from --command.")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  open <session>      Open the configured pane(s), or --command")
	fmt.Println("  close <session>     Close the session's panes (--slot 1|2 for one)")
	fmt.Println("  status <session>    Show the configured spec and the open panes")
	fmt.Println("  refresh <session>   Send \"refresh manual\" (or --field F --value V) to the panes; --all for every session")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  agent-deck pane open worker-12")
	fmt.Println("  agent-deck pane open worker-12 --command 'deck-kit pictures' --graphics")
	fmt.Println("  agent-deck pane status worker-12 --json")
	fmt.Println("  agent-deck pane refresh worker-12 --field plane --value \"OPS-9 state\"")
}

func handlePane(profile string, args []string) {
	if len(args) == 0 || helpRequested(args[:1]) || args[0] == "help" {
		printPaneUsage()
		return
	}
	switch args[0] {
	case "open":
		handlePaneOpen(profile, args[1:])
	case "close":
		handlePaneClose(profile, args[1:])
	case "status":
		handlePaneStatus(profile, args[1:])
	case "refresh":
		handlePaneRefresh(profile, args[1:])
	case "run":
		handlePaneRun(profile, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown pane command %q\n", args[0])
		printPaneUsage()
		exitCLI(1)
	}
}

// paneSession is the session a pane command acts on.
type paneSession struct {
	inst    *session.Instance
	tmux    string
	socket  string
	profile string
}

func resolvePaneSession(profile, identifier string, out *CLIOutput) paneSession {
	_, instances, _, err := loadSessionData(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		exitCLI(1)
	}
	inst, errMsg, errCode := ResolveSession(identifier, instances)
	if inst == nil {
		out.Error(errMsg, errCode)
		if errCode == ErrCodeNotFound {
			exitCLI(2)
		}
		exitCLI(1)
		return paneSession{}
	}
	ps := paneSession{inst: inst, profile: session.GetEffectiveProfile(profile), socket: inst.TmuxSocketName}
	if ts := inst.GetTmuxSession(); ts != nil {
		ps.tmux = ts.Name
		ps.socket = ts.SocketName
	}
	return ps
}

func paneLayoutSpec(cfg *session.UserConfig, width string) panehost.LayoutSpec {
	l := panehost.LayoutSpec{Width: width}
	if cfg != nil {
		l.NarrowCols = cfg.Pane.NarrowCols
		l.MinCols = cfg.Pane.MinCols
	}
	return l
}

// paneHostCommand is the shell command tmux runs in a new host pane: this
// binary's hidden `pane run`.
func paneHostCommand(exe, profile, sessionID, socket, command string, graphics bool) func(agentPane string, slot int) string {
	return func(agentPane string, slot int) string {
		parts := []string{"exec", shellescape.Quote(exe)}
		if profile != "" {
			parts = append(parts, "-p", shellescape.Quote(profile))
		}
		parts = append(parts, "pane", "run",
			"--session", shellescape.Quote(sessionID),
			"--slot", fmt.Sprint(slot),
			"--agent-pane", shellescape.Quote(agentPane),
			"--tmux-socket", shellescape.Quote(socket),
			"--command", shellescape.Quote(command))
		if graphics {
			parts = append(parts, "--graphics")
		}
		return strings.Join(parts, " ")
	}
}

// paneOpenRequest is one pane to open.
type paneOpenRequest struct {
	command  string
	graphics bool
}

// openSessionPanes opens reqs beside the session (skipping ones already
// open) and returns the panes and whether each was created now.
func openSessionPanes(profile, sessionID, tmuxName, socket string, layout panehost.LayoutSpec, reqs []paneOpenRequest) ([]panehost.HostPane, []bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locating agent-deck binary: %w", err)
	}
	run := panehost.SocketRunner(socket)
	var panes []panehost.HostPane
	var created []bool
	for _, r := range reqs {
		hp, c, err := panehost.Open(run, panehost.OpenSpec{
			TmuxSession: tmuxName,
			SessionID:   sessionID,
			Command:     r.command,
			Graphics:    r.graphics,
			Layout:      layout,
			HostCommand: paneHostCommand(exe, profile, sessionID, socket, r.command, r.graphics),
		})
		if err != nil {
			return panes, created, err
		}
		panes = append(panes, hp)
		created = append(created, c)
	}
	return panes, created, nil
}

func configuredPaneRequests(r session.ResolvedPane) []paneOpenRequest {
	var reqs []paneOpenRequest
	if r.Command != "" {
		reqs = append(reqs, paneOpenRequest{r.Command, r.Graphics})
		if r.Stack != "" {
			reqs = append(reqs, paneOpenRequest{r.Stack, r.StackGraphics})
		}
	}
	return reqs
}

func handlePaneOpen(profile string, args []string) {
	fs := flag.NewFlagSet("pane open", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	command := fs.String("command", "", "Provider command to run instead of the configured one")
	graphics := fs.Bool("graphics", false, "With --command: let the provider's terminal graphics through (kitty graphics)")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck pane open <id|title> [--command <cmd> [--graphics]] [--json]")
		fmt.Println()
		fmt.Println("Open the session's side pane(s). A second pane stacks below the first; two at most.")
		fs.PrintDefaults()
	}
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	out := NewCLIOutput(*jsonOutput, false)
	ps := resolvePaneSession(profile, fs.Arg(0), out)
	cfg, _ := session.LoadUserConfig()
	resolved := cfg.ResolvePane(ps.inst.ID, ps.inst.Title, ps.inst.GroupPath)
	reqs := configuredPaneRequests(resolved)
	if strings.TrimSpace(*command) != "" {
		reqs = []paneOpenRequest{{strings.TrimSpace(*command), *graphics}}
	}
	if len(reqs) == 0 {
		out.Error(fmt.Sprintf("no pane command for '%s': set [groups.%q.pane] command or [pane.sessions.%q] command in config.toml, or pass --command", ps.inst.Title, ps.inst.GroupPath, ps.inst.Title), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	if ps.tmux == "" || !ps.inst.Exists() {
		out.Error(fmt.Sprintf("session '%s' is not running", ps.inst.Title), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	panes, created, err := openSessionPanes(ps.profile, ps.inst.ID, ps.tmux, ps.socket, paneLayoutSpec(cfg, resolved.Width), reqs)
	if err != nil {
		out.Error(fmt.Sprintf("opening pane: %v", err), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	var lines []string
	for i, p := range panes {
		verb := "opened"
		if !created[i] {
			verb = "already open"
		}
		lines = append(lines, fmt.Sprintf("pane %d %s (%s): %s", p.Slot, verb, p.PaneID, p.Command))
	}
	out.Success(strings.Join(lines, "\n"), map[string]interface{}{
		"success": true, "id": ps.inst.ID, "title": ps.inst.Title, "panes": panes, "created": created,
	})
}

func handlePaneClose(profile string, args []string) {
	fs := flag.NewFlagSet("pane close", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	slot := fs.Int("slot", 0, "Close only this slot (1 or 2); default all")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck pane close <id|title> [--slot 1|2] [--json]")
		fmt.Println()
		fmt.Println("Close the session's side panes. Each provider gets SIGHUP, then SIGKILL after 1s.")
		fs.PrintDefaults()
	}
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	out := NewCLIOutput(*jsonOutput, false)
	ps := resolvePaneSession(profile, fs.Arg(0), out)
	if ps.tmux == "" || !ps.inst.Exists() {
		out.Success(fmt.Sprintf("session '%s' is not running; no panes", ps.inst.Title), map[string]interface{}{"success": true, "id": ps.inst.ID, "closed": []panehost.HostPane{}})
		return
	}
	closed, err := panehost.Close(panehost.SocketRunner(ps.socket), ps.tmux, *slot, 3*time.Second)
	if err != nil {
		out.Error(fmt.Sprintf("closing panes: %v", err), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	if closed == nil {
		closed = []panehost.HostPane{}
	}
	out.Success(fmt.Sprintf("closed %d pane(s) of %s", len(closed), ps.inst.Title), map[string]interface{}{
		"success": true, "id": ps.inst.ID, "title": ps.inst.Title, "closed": closed,
	})
}

func handlePaneStatus(profile string, args []string) {
	fs := flag.NewFlagSet("pane status", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck pane status <id|title> [--json]")
		fmt.Println()
		fmt.Println("Show the pane spec that applies to the session and its open panes.")
		fs.PrintDefaults()
	}
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	out := NewCLIOutput(*jsonOutput, false)
	ps := resolvePaneSession(profile, fs.Arg(0), out)
	cfg, _ := session.LoadUserConfig()
	r := cfg.ResolvePane(ps.inst.ID, ps.inst.Title, ps.inst.GroupPath)
	panes := []panehost.HostPane{}
	zoomed := false
	running := ps.tmux != "" && ps.inst.Exists()
	if running {
		if st, err := panehost.Inspect(panehost.SocketRunner(ps.socket), ps.tmux); err == nil {
			panes = append(panes, st.Panes...)
			zoomed = st.HostZoomed
		}
	}
	autoOpen := cfg == nil || cfg.Pane.AutoOpenEnabled()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", ps.inst.Title)
	if r.Command == "" {
		fmt.Fprintf(&b, "  configured: none\n")
	} else {
		fmt.Fprintf(&b, "  configured: %s (from %s)\n", r.Command, r.Source)
		if r.Stack != "" {
			fmt.Fprintf(&b, "  stacked:    %s\n", r.Stack)
		}
		fmt.Fprintf(&b, "  auto open:  %v\n", autoOpen)
	}
	if r.Role != "" {
		fmt.Fprintf(&b, "  role:       %s (override)\n", r.Role)
	}
	if !running {
		fmt.Fprintf(&b, "  open:       session not running\n")
	} else if len(panes) == 0 {
		fmt.Fprintf(&b, "  open:       none\n")
	}
	for _, p := range panes {
		fmt.Fprintf(&b, "  pane %d:     %s %dx%d pid %d  %s\n", p.Slot, p.PaneID, p.Width, p.Height, p.PID, p.Command)
	}
	if zoomed {
		fmt.Fprintf(&b, "  layout:     narrow window, agent pane zoomed\n")
	}
	out.Success(strings.TrimRight(b.String(), "\n"), map[string]interface{}{
		"id": ps.inst.ID, "title": ps.inst.Title, "running": running,
		"configured": map[string]interface{}{
			"command": r.Command, "graphics": r.Graphics, "stack": r.Stack, "stack_graphics": r.StackGraphics,
			"role": r.Role, "width": r.Width, "source": r.Source, "auto_open": autoOpen,
		},
		"panes": panes, "zoomed": zoomed,
	})
}

func handlePaneRefresh(profile string, args []string) {
	fs := flag.NewFlagSet("pane refresh", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	all := fs.Bool("all", false, "Every open pane of the profile")
	field := fs.String("field", "", "Send \"refresh FIELD VALUE\" instead of \"refresh manual\"")
	value := fs.String("value", "", "With --field: the value (one line)")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck pane refresh <id|title>|--all [--field F --value V] [--json]")
		fmt.Println()
		fmt.Println("Send \"refresh manual\" to the session's open panes, or \"refresh F V\" with --field.")
		fmt.Println("The host's own fields (status, title, item, pr, session, manual) cannot be sent.")
		fs.PrintDefaults()
	}
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	out := NewCLIOutput(*jsonOutput, false)
	f := strings.TrimSpace(*field)
	if f == "" && *value != "" {
		out.Error("--value needs --field", ErrCodeInvalidOperation)
		exitCLI(1)
	}
	if f != "" {
		if err := panehost.ValidRefreshField(f); err != nil {
			out.Error(err.Error(), ErrCodeInvalidOperation)
			exitCLI(1)
		}
	}
	if *all == (fs.Arg(0) != "") {
		out.Error("give one session or --all", ErrCodeInvalidOperation)
		exitCLI(1)
	}

	var (
		panes []panehost.HostPane
		data  = map[string]interface{}{"success": true}
		label string
	)
	if *all {
		_, instances, _, err := loadSessionData(profile)
		if err != nil {
			out.Error(err.Error(), ErrCodeNotFound)
			exitCLI(1)
		}
		panes = profileHostPanes(instances)
		label = "profile " + session.GetEffectiveProfile(profile)
		data["all"] = true
	} else {
		ps := resolvePaneSession(profile, fs.Arg(0), out)
		if ps.tmux != "" && ps.inst.Exists() {
			if st, err := panehost.Inspect(panehost.SocketRunner(ps.socket), ps.tmux); err == nil {
				panes = st.Panes
			}
		}
		label = ps.inst.Title
		data["id"] = ps.inst.ID
	}
	n := 0
	for _, p := range panes {
		if refreshHostPane(p.PID, f, *value) {
			n++
		}
	}
	data["panes"] = n
	if f != "" {
		data["field"] = f
		data["value"] = panehost.RefreshValue(*value)
	}
	out.Success(fmt.Sprintf("refreshed %d pane(s) of %s", n, label), data)
}

// profileHostPanes lists the open host panes of the profile's sessions,
// one list-panes call per tmux server the profile uses.
func profileHostPanes(instances []*session.Instance) []panehost.HostPane {
	ids := map[string]bool{}
	sockets := map[string]bool{}
	for _, inst := range instances {
		ids[inst.ID] = true
		socket := inst.TmuxSocketName
		if ts := inst.GetTmuxSession(); ts != nil {
			socket = ts.SocketName
		}
		sockets[socket] = true
	}
	var panes []panehost.HostPane
	for socket := range sockets {
		hosts, err := panehost.ListHostPanes(panehost.SocketRunner(socket))
		if err != nil {
			continue
		}
		for _, h := range hosts {
			if ids[h.SessionID] {
				panes = append(panes, h)
			}
		}
	}
	return panes
}

// refreshHostPane queues one refresh for the host process pid (field ""
// means manual) and signals it. A host without a spool (an older binary)
// still gets the signal, which it reads as "refresh manual".
func refreshHostPane(pid int, field, value string) bool {
	if pid <= 0 {
		return false
	}
	if dir, err := panehost.SpoolDir(pid); err == nil {
		if err := panehost.Enqueue(dir, field, value); err != nil && field != "" {
			paneLog.Warn("pane_refresh_field_unsupported", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	}
	return syscall.Kill(pid, syscall.SIGUSR1) == nil
}

// paneProviderDrop are the identity variables a pane inherits from the
// agent's tmux session and must not pass to the provider.
var paneProviderDrop = []string{
	"AGENTDECK_INSTANCE_ID", "AGENTDECK_TOOL_SESSION_ID", "AGENTDECK_TITLE",
	"CLAUDE_SESSION_ID", "CODEX_SESSION_ID", "GEMINI_SESSION_ID", "OPENCODE_SESSION_ID",
}

// handlePaneRun is the host itself, run by tmux in the side pane (hidden;
// `pane open` builds its command line).
func handlePaneRun(profile string, args []string) {
	fs := flag.NewFlagSet("pane run", flag.ContinueOnError)
	sessionID := fs.String("session", "", "Agent Deck session id")
	slot := fs.Int("slot", 1, "pane slot")
	agentPane := fs.String("agent-pane", "", "tmux pane id of the agent")
	socket := fs.String("tmux-socket", "", "tmux -L socket of the session")
	command := fs.String("command", "", "provider command")
	_ = fs.Bool("graphics", false, "provider uses terminal graphics")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck pane run --session <id> --agent-pane <%id> --command <cmd> (internal: run by tmux in a side pane)")
		fs.PrintDefaults()
	}
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	if *sessionID == "" || *agentPane == "" || strings.TrimSpace(*command) == "" || !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "Error: pane run is started by `agent-deck pane open` inside a tmux pane")
		exitCLI(1)
	}
	code := runPaneHost(session.GetEffectiveProfile(profile), *sessionID, *slot, *agentPane, *socket, *command)
	exitCLI(code)
}

func runPaneHost(profile, sessionID string, slot int, agentPane, socket, command string) int {
	self := os.Getenv("TMUX_PANE")
	run := panehost.SocketRunner(socket)

	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-deck pane: opening profile %s: %v\n", profile, err)
		time.Sleep(3 * time.Second)
		return 1
	}
	defer storage.Close()
	all, _, err := storage.LoadLite()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-deck pane: loading sessions: %v\n", err)
		time.Sleep(3 * time.Second)
		return 1
	}
	cfg, _ := session.LoadUserConfig()
	var title, group string
	for _, d := range all {
		if d.ID == sessionID {
			title, group = d.Title, d.GroupPath
		}
	}
	resolved := cfg.ResolvePane(sessionID, title, group)
	target, ok := session.BuildPaneTarget(profile, sessionID, all, resolved.Role)
	if !ok {
		fmt.Fprintf(os.Stderr, "agent-deck pane: session %s not found in profile %s\n", sessionID, profile)
		time.Sleep(3 * time.Second)
		return 1
	}
	extra := append(target.Env(),
		session.PaneEnvPrefix+"SLOT="+fmt.Sprint(slot),
		session.PaneEnvPrefix+"AGENT_PANE="+agentPane,
		session.PaneEnvPrefix+"CONTROL_FD=3",
		session.PaneEnvPrefix+"PROTOCOL="+fmt.Sprint(panehost.ProtocolVersion),
	)
	env := childenv.ForPaneProvider(paneProviderDrop, session.PaneEnvPrefix, extra)
	dir := target.Path
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir, _ = os.UserHomeDir()
	}

	notifyMode := "both"
	if cfg != nil {
		notifyMode = cfg.Pane.NotifyMode()
	}
	notifier := newPaneNotifier(notifyMode, target.Title, target.Conductor, profile)
	nav := newPaneNavigator(run, socket, target.TmuxSession, storage)
	// One storage read serves the pane's own fields and its watched
	// sessions on the same tick.
	load := memoLoad(storage, 500*time.Millisecond)

	// `pane refresh` requests arrive as files here plus SIGUSR1.
	spool, err := panehost.SpoolDir(os.Getpid())
	if err == nil {
		if err := panehost.PrepareSpool(spool); err != nil {
			spool = ""
		} else {
			defer os.RemoveAll(spool)
		}
	} else {
		spool = ""
	}

	code, err := panehost.Run(context.Background(), panehost.Options{
		Command: command,
		Dir:     dir,
		Env:     env,
		TTY:     os.Stdin,
		Fields: func() map[string]string {
			now, err := load()
			if err != nil {
				return nil
			}
			return session.PaneFields(sessionID, now)
		},
		Statuses: func(ids []string) map[string]string {
			now, err := load()
			if err != nil {
				return nil
			}
			return session.PaneWatchStatuses(ids, now)
		},
		Probe:  func() panehost.Probe { return panehost.ProbeAgent(run, agentPane) },
		Layout: func() { panehost.ApplyLayoutAs(run, target.TmuxSession, self) },
		OnControl: func(line string) {
			if ok, err := nav.Handle(line); ok {
				if err != nil {
					paneLog.Warn("pane_nav_refused", slog.String("session", sessionID), slog.String("line", logging.SanitizeValue(line)), slog.String("error", logging.SanitizeValue(err.Error())))
				}
				return
			}
			notifier.handle(line)
		},
		Refreshes: func() []string {
			if spool == "" {
				return nil
			}
			return panehost.Drain(spool)
		},
		ClickClient: func() string {
			c := panehost.ReadClickClient(run, self)
			nav.Clicked(c)
			return c
		},
		Logf: func(format string, a ...any) {
			paneLog.Warn("pane_host", slog.String("session", sessionID), slog.String("msg", fmt.Sprintf(format, a...)))
		},
	})
	notifier.wait()
	if target.TmuxSession != "" {
		panehost.Cleanup(run, target.TmuxSession, self)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-deck pane: %v\n", err)
		time.Sleep(3 * time.Second)
		return 1
	}
	if code > 0 {
		// Leave the reason on screen for a moment before the pane closes.
		fmt.Fprintf(os.Stderr, "\r\npane command exited with status %d\r\n", code)
		time.Sleep(3 * time.Second)
	}
	return 0
}

// memoLoad returns a LoadLite that reuses a read younger than ttl.
func memoLoad(storage *session.Storage, ttl time.Duration) func() ([]*session.InstanceData, error) {
	var (
		mu   sync.Mutex
		at   time.Time
		last []*session.InstanceData
	)
	return func() ([]*session.InstanceData, error) {
		mu.Lock()
		defer mu.Unlock()
		if last != nil && time.Since(at) < ttl {
			return last, nil
		}
		all, _, err := storage.LoadLite()
		if err != nil {
			return nil, err
		}
		last, at = all, time.Now()
		return all, nil
	}
}

// newPaneNavigator wires a provider's goto and open lines: sessions are
// looked up in the profile's storage, web terminal clients are handed to
// their web server through internal/panenav, and URLs for a terminal client
// go to the machine's opener.
func newPaneNavigator(run panehost.Runner, socket, tmuxSession string, storage *session.Storage) *panehost.Navigator {
	return &panehost.Navigator{
		Run:         run,
		TmuxSession: tmuxSession,
		Socket:      socket,
		Lookup: func(id string) (string, string, bool) {
			all, _, err := storage.LoadLite()
			if err != nil {
				return "", "", false
			}
			for _, d := range all {
				if d.ID == id {
					return d.TmuxSession, d.TmuxSocketName, true
				}
			}
			return "", "", false
		},
		Web: func(ev panenav.Event) (panenav.Reply, error) {
			dir, err := panenav.Dir()
			if err != nil {
				return panenav.Reply{}, err
			}
			return panenav.Deliver(dir, ev, time.Second)
		},
		OpenLocal: openURLLocally,
	}
}

// openURLLocally opens an already validated http(s) URL with the platform
// opener, the URL as its only argument (never through a shell).
func openURLLocally(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("no URL opener on this machine: %w", err)
	}
	// #nosec G204 -- fixed opener; the URL was checked by panenav.ValidURL
	// (http/https, no whitespace, so never an option).
	cmd := exec.Command(path, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// paneNotifier delivers a provider's "notify" control lines through Agent
// Deck's existing paths: the desktop notifier and the human outbox of the
// conductor nearest the human (which the conductor bridge forwards to the
// phone). At most one notification per paneNotifyEvery.
type paneNotifier struct {
	mode, title, conductor, profile string
	desk                            *desknotify.Notifier
	mu                              sync.Mutex
	last                            time.Time
	wg                              sync.WaitGroup
	appendOutbox                    func(conductor, tier, text string) error
}

const paneNotifyEvery = 5 * time.Second

func newPaneNotifier(mode, title, conductor, profile string) *paneNotifier {
	return &paneNotifier{
		mode: mode, title: title, conductor: conductor, profile: profile,
		desk: desknotify.New(),
		appendOutbox: func(c, tier, text string) error {
			_, _, err := session.AppendHumanOutbox(c, tier, text)
			return err
		},
	}
}

// parsePaneNotify reads "notify [urgent|info] TEXT".
func parsePaneNotify(line string) (tier, text string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), "notify ")
	if !found {
		return "", "", false
	}
	tier = session.TurnTierUrgent
	if t, r, found := strings.Cut(rest, " "); found && (t == session.TurnTierUrgent || t == session.TurnTierInfo) {
		tier, rest = t, r
	}
	text = strings.Join(strings.Fields(rest), " ")
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300])
	}
	return tier, text, text != ""
}

func (n *paneNotifier) handle(line string) {
	tier, text, ok := parsePaneNotify(line)
	if !ok || n.mode == "none" {
		return
	}
	n.mu.Lock()
	if time.Since(n.last) < paneNotifyEvery {
		n.mu.Unlock()
		paneLog.Info("pane_notify_rate_limited", slog.String("title", logging.SanitizeValue(n.title)))
		return
	}
	n.last = time.Now()
	n.mu.Unlock()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if n.mode == "desktop" || n.mode == "both" {
			n.desk.NotifyText(n.title, text)
		}
		if (n.mode == "outbox" || n.mode == "both") && n.conductor != "" {
			// Labelled as automated pane output: data for the human, not an
			// instruction to any agent.
			if err := n.appendOutbox(n.conductor, tier, "[pane "+n.title+"] "+text); err != nil {
				paneLog.Warn("pane_notify_outbox_failed", slog.String("error", err.Error()))
			}
		}
	}()
}

func (n *paneNotifier) wait() { n.wg.Wait() }

// paneAutoOpen opens the configured panes after a session starts or
// restarts ([pane].auto_open, default on). It never fails the start: a pane
// that cannot open is logged.
func paneAutoOpen(profile string) func(inst *session.Instance) {
	return func(inst *session.Instance) {
		if inst == nil {
			return
		}
		cfg, err := session.LoadUserConfig()
		if err != nil || cfg == nil || !cfg.Pane.AutoOpenEnabled() {
			return
		}
		r := cfg.ResolvePane(inst.ID, inst.Title, inst.GroupPath)
		reqs := configuredPaneRequests(r)
		if len(reqs) == 0 {
			return
		}
		ts := inst.GetTmuxSession()
		if ts == nil || ts.Name == "" {
			return
		}
		if _, _, err := openSessionPanes(session.GetEffectiveProfile(profile), inst.ID, ts.Name, ts.SocketName, paneLayoutSpec(cfg, r.Width), reqs); err != nil && !errors.Is(err, panehost.ErrFull) {
			paneLog.Warn("pane_auto_open_failed", slog.String("session", inst.ID), slog.String("error", err.Error()))
		}
	}
}
