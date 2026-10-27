package panehost

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/creack/pty"
)

// testServer starts a private tmux server (-L <unique>, -f /dev/null) under
// the isolated TMUX_TMPDIR from TestMain, with one session "agent" whose
// only pane plays the agent.
func testServer(t *testing.T) (socket string, run Runner) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	socket = fmt.Sprintf("adpane%d%d", os.Getpid(), time.Now().UnixNano()%100000)
	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "agent", "-x", "160", "-y", "40", "sleep 600")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting private tmux server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	run = SocketRunner(socket)
	if _, err := run("set-option", "-g", "mouse", "on"); err != nil {
		t.Fatal(err)
	}
	return socket, run
}

// openTestHost opens a host pane whose host is this test binary (TestMain's
// helper mode) running provider.
func openTestHost(t *testing.T, socket string, run Runner, provider, ctlLog string, graphics bool) HostPane {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	hp, created, err := Open(run, OpenSpec{
		TmuxSession: "agent",
		SessionID:   "sess-1",
		Command:     provider,
		Graphics:    graphics,
		HostCommand: func(agentPane string, slot int) string {
			return strings.Join([]string{
				hostHelperEnv + "=1",
				"PANEHOST_TEST_SOCKET=" + shellescape.Quote(socket),
				"PANEHOST_TEST_SESSION=agent",
				"PANEHOST_TEST_AGENT=" + shellescape.Quote(agentPane),
				"PANEHOST_TEST_CMD=" + shellescape.Quote(provider),
				"PANEHOST_TEST_CONTROL_LOG=" + shellescape.Quote(ctlLog),
				"PANEHOST_TEST_PROBE_LOG=" + shellescape.Quote(ctlLog+".probe"),
				"exec", shellescape.Quote(exe),
			}, " ")
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !created {
		t.Fatalf("Open reported an existing pane for a new command")
	}
	return hp
}

func display(t *testing.T, run Runner, target, format string) string {
	t.Helper()
	out, err := run("display-message", "-p", "-t", target, format)
	if err != nil {
		t.Fatalf("display %s: %v", format, err)
	}
	return strings.TrimSpace(out)
}

func TestTmuxOpenLayoutFocusAndClose(t *testing.T) {
	socket, run := testServer(t)
	dir := t.TempDir()
	in1 := filepath.Join(dir, "in1.log")
	pid1 := filepath.Join(dir, "p1.pid")
	prov1 := "echo $$ > " + pid1 + "; exec cat > " + in1

	hp := openTestHost(t, socket, run, prov1, "", false)
	if hp.Slot != 1 {
		t.Fatalf("first pane slot = %d", hp.Slot)
	}
	p1 := readPID(t, pid1)
	waitFor(t, "hello", 3*time.Second, func() bool { return hasLine(in1, "hello 1") })

	st, err := Inspect(run, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Panes) != 1 || st.Panes[0].Command != prov1 || st.Panes[0].SessionID != "sess-1" {
		t.Fatalf("Inspect panes = %+v", st.Panes)
	}
	// 36% of 160 columns, give or take the border.
	if w := st.Panes[0].Width; w < 55 || w > 59 {
		t.Fatalf("host column width = %d, want about 57", w)
	}
	if active := display(t, run, "agent", "#{pane_id}"); active != st.AgentPane {
		t.Fatalf("active pane after open = %s, want the agent pane %s", active, st.AgentPane)
	}

	// Opening the same command again is a no-op.
	again, created, err := Open(run, OpenSpec{TmuxSession: "agent", Command: prov1, HostCommand: func(string, int) string { return "false" }})
	if err != nil || created || again.PaneID != hp.PaneID {
		t.Fatalf("reopen = %+v created=%v err=%v", again, created, err)
	}

	// Selecting the host pane sends focus straight back to the agent.
	if _, err := run("select-pane", "-t", hp.PaneID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "focus back on agent", 2*time.Second, func() bool {
		return display(t, run, "agent", "#{pane_id}") == st.AgentPane
	})

	// Phone width: the agent pane is zoomed and the column hidden.
	if _, err := run("resize-window", "-t", "agent", "-x", "70", "-y", "40"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "zoom on narrow window", 3*time.Second, func() bool {
		return display(t, run, st.AgentPane, "#{window_zoomed_flag}#{@agentdeck_pane_zoomed}") == "11"
	})
	waitFor(t, "resize line", 3*time.Second, func() bool {
		for _, l := range readLines(in1) {
			if strings.HasPrefix(l, "resize ") && l != "resize 57 40" && l != "resize 57 39" {
				return true
			}
		}
		return false
	})
	// Wide again: zoom undone and the column back at its width.
	if _, err := run("resize-window", "-t", "agent", "-x", "160", "-y", "40"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restore on wide window", 3*time.Second, func() bool {
		return display(t, run, st.AgentPane, "#{window_zoomed_flag}#{@agentdeck_pane_zoomed}") == "0"
	})
	waitFor(t, "column width restored", 3*time.Second, func() bool {
		w, _ := strconv.Atoi(display(t, run, hp.PaneID, "#{pane_width}"))
		return w >= 55 && w <= 59
	})

	// A second pane stacks below the first; a third is refused.
	in2 := filepath.Join(dir, "in2.log")
	pid2 := filepath.Join(dir, "p2.pid")
	prov2 := "echo $$ > " + pid2 + "; exec cat > " + in2
	hp2 := openTestHost(t, socket, run, prov2, "", true)
	if hp2.Slot != 2 {
		t.Fatalf("second pane slot = %d", hp2.Slot)
	}
	p2 := readPID(t, pid2)
	if got := display(t, run, hp2.PaneID, "#{pane_left}|#{allow-passthrough}"); got != display(t, run, hp.PaneID, "#{pane_left}")+"|on" {
		t.Fatalf("second pane left/passthrough = %s, want same column as the first and passthrough on", got)
	}
	if _, _, err := Open(run, OpenSpec{TmuxSession: "agent", Command: "third", HostCommand: func(string, int) string { return "true" }}); err != ErrFull {
		t.Fatalf("third pane err = %v, want ErrFull", err)
	}

	// Close slot 2: its provider ends, slot 1 stays.
	closed, err := Close(run, "agent", 2, 3*time.Second)
	if err != nil || len(closed) != 1 || closed[0].PaneID != hp2.PaneID {
		t.Fatalf("Close(2) = %+v, %v", closed, err)
	}
	waitFor(t, "slot 2 provider dead", 3*time.Second, func() bool { return !pidAlive(p2) })
	if !pidAlive(p1) {
		t.Fatal("closing slot 2 killed slot 1's provider")
	}

	// Killing the session (what `session stop` does) leaves no provider.
	if _, err := run("kill-session", "-t", "agent"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "slot 1 provider dead after kill-session", 4*time.Second, func() bool { return !pidAlive(p1) })
}

// The agent pane exiting ends the host too, so a side pane never keeps a
// finished session's tmux session alive.
func TestTmuxAgentPaneExitEndsHost(t *testing.T) {
	socket, run := testServer(t)
	dir := t.TempDir()
	pid1 := filepath.Join(dir, "p1.pid")
	hp := openTestHost(t, socket, run, "echo $$ > "+pid1+"; exec sleep 600", "", false)
	p1 := readPID(t, pid1)
	st, err := Inspect(run, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run("kill-pane", "-t", st.AgentPane); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "provider dead after agent pane exit", 4*time.Second, func() bool { return !pidAlive(p1) })
	waitFor(t, "host pane closed", 4*time.Second, func() bool {
		_, err := run("display-message", "-p", "-t", hp.PaneID, "#{pane_id}")
		return err != nil
	})
}

// A real client: mouse reports from the outer terminal reach the provider
// as protocol lines through tmux's default bindings, the click leaves focus
// on the agent, an attach is announced, and a provider's control line
// arrives at the host.
func TestTmuxMouseThroughAttachedClient(t *testing.T) {
	socket, run := testServer(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.log")
	ctl := filepath.Join(dir, "ctl.log")
	hp := openTestHost(t, socket, run, "echo 'notify info hello' >&3; exec cat > "+in, ctl, false)
	waitFor(t, "hello", 3*time.Second, func() bool { return hasLine(in, "hello 1") })
	waitFor(t, "control line", 3*time.Second, func() bool { return hasLine(ctl, "notify info hello") })

	client := exec.Command("tmux", "-L", socket, "attach", "-t", "agent")
	client.Env = append(os.Environ(), "TERM=xterm-256color")
	master, err := pty.StartWithSize(client, &pty.Winsize{Cols: 160, Rows: 40})
	if err != nil {
		t.Skipf("cannot attach a client: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, master) }()
	t.Cleanup(func() {
		_ = client.Process.Signal(syscall.SIGTERM)
		_ = master.Close()
		_ = client.Wait()
	})
	waitFor(t, "attach line", 4*time.Second, func() bool { return hasLine(in, "attach") })
	if pb, _ := os.ReadFile(ctl + ".probe"); len(pb) == 0 {
		t.Fatal("the host never probed the agent pane")
	}

	left, _ := strconv.Atoi(display(t, run, hp.PaneID, "#{pane_left}"))
	top, _ := strconv.Atoi(display(t, run, hp.PaneID, "#{pane_top}"))
	st, _ := Inspect(run, "agent")
	// Cell (4,2) of the host pane, in the outer terminal's 1-based cells.
	x, y := left+1+4, top+1+2
	if _, err := fmt.Fprintf(master, "\x1b[<0;%d;%dM\x1b[<0;%d;%dm", x, y, x, y); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "click line", 4*time.Second, func() bool { return hasLine(in, "click 4 2") })
	waitFor(t, "focus back on agent after click", 2*time.Second, func() bool {
		return display(t, run, "agent", "#{pane_id}") == st.AgentPane
	})
	if _, err := fmt.Fprintf(master, "\x1b[<64;%d;%dM", x, y); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "scroll line", 4*time.Second, func() bool { return hasLine(in, "scroll up 4 2") })
}
