package panehost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// testTTY gives Run a real terminal: the slave side of a pty sized 90x30.
// The master is drained so provider output never blocks.
func testTTY(t *testing.T) (master, tty *os.File) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	if err := pty.Setsize(s, &pty.Winsize{Cols: 90, Rows: 30}); err != nil {
		t.Fatalf("setsize: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, m) }()
	t.Cleanup(func() { _ = s.Close(); _ = m.Close() })
	return m, s
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func hasLine(path, want string) bool {
	for _, l := range readLines(path) {
		if l == want {
			return true
		}
	}
	return false
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, "pid file "+filepath.Base(path), 3*time.Second, func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid > 0
	})
	return pid
}

func TestRunForwardsEventsToProviderStdin(t *testing.T) {
	master, tty := testTTY(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.log")

	var mu sync.Mutex
	fields := map[string]string{"status": "running", "title": "w1"}
	var attached atomic.Int32
	var controls []string
	sigs := make(chan os.Signal, 4)

	done := make(chan int, 1)
	go func() {
		code, err := Run(context.Background(), Options{
			Command: "echo 'notify urgent PR 12 can land' >&3; cat > " + in,
			TTY:     tty,
			Fields: func() map[string]string {
				mu.Lock()
				defer mu.Unlock()
				cp := map[string]string{}
				for k, v := range fields {
					cp[k] = v
				}
				return cp
			},
			FieldsPeriod: 30 * time.Millisecond,
			Probe:        func() Probe { return Probe{Attached: int(attached.Load())} },
			ProbePeriod:  30 * time.Millisecond,
			OnControl: func(l string) {
				mu.Lock()
				controls = append(controls, l)
				mu.Unlock()
			},
			Signals: sigs,
			Grace:   300 * time.Millisecond,
		})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- code
	}()

	waitFor(t, "hello and size", 3*time.Second, func() bool {
		return hasLine(in, "hello 1") && hasLine(in, "resize 90 30")
	})

	// What tmux writes to a pane that asked for SGR mouse reports.
	if _, err := master.Write([]byte("\x1b[<0;5;3M\x1b[<0;5;3m\x1b[<65;2;2Mj")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "click, scroll and key", 3*time.Second, func() bool {
		return hasLine(in, "click 4 2") && hasLine(in, "scroll down 1 1") && hasLine(in, "key j")
	})

	mu.Lock()
	fields["status"] = "waiting"
	fields["item"] = "ONAZA-12"
	mu.Unlock()
	waitFor(t, "refresh lines", 3*time.Second, func() bool {
		return hasLine(in, "refresh status waiting") && hasLine(in, "refresh item ONAZA-12")
	})

	attached.Store(1)
	waitFor(t, "attach", 3*time.Second, func() bool { return hasLine(in, "attach") })

	if err := pty.Setsize(tty, &pty.Winsize{Cols: 50, Rows: 20}); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGWINCH
	waitFor(t, "resize", 3*time.Second, func() bool { return hasLine(in, "resize 50 20") })

	sigs <- syscall.SIGUSR1
	waitFor(t, "manual refresh", 3*time.Second, func() bool { return hasLine(in, "refresh manual") })

	mu.Lock()
	gotCtl := append([]string(nil), controls...)
	mu.Unlock()
	if len(gotCtl) != 1 || gotCtl[0] != "notify urgent PR 12 can land" {
		t.Fatalf("control lines = %q", gotCtl)
	}

	sigs <- syscall.SIGHUP
	select {
	case code := <-done:
		if code != -1 {
			t.Fatalf("host-stopped exit code = %d, want -1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after SIGHUP")
	}
}

// A provider that ignores SIGHUP, with a child that ignores it too, is
// SIGKILLed after the grace period: nothing outlives the pane.
func TestRunStopKillsWholeGroupAfterGrace(t *testing.T) {
	_, tty := testTTY(t)
	dir := t.TempDir()
	provPID := filepath.Join(dir, "prov.pid")
	childPID := filepath.Join(dir, "child.pid")
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	start := time.Now()
	var stoppedAt time.Time
	go func() {
		defer close(done)
		_, _ = Run(context.Background(), Options{
			Command: "trap '' HUP; echo $$ > " + provPID + "; sleep 60 & echo $! > " + childPID + "; while :; do sleep 0.05; done",
			TTY:     tty,
			Signals: sigs,
			Grace:   300 * time.Millisecond,
		})
	}()
	p := readPID(t, provPID)
	c := readPID(t, childPID)
	stoppedAt = time.Now()
	sigs <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if d := time.Since(stoppedAt); d < 250*time.Millisecond {
		t.Fatalf("returned after %v: the grace period was skipped", d)
	}
	waitFor(t, "provider and child dead", 3*time.Second, func() bool { return !pidAlive(p) && !pidAlive(c) })
	_ = start
}

// A provider that exits on its own returns its exit code, and a child it
// left running is reaped with it.
func TestRunProviderExitReapsLeftovers(t *testing.T) {
	_, tty := testTTY(t)
	dir := t.TempDir()
	childPID := filepath.Join(dir, "child.pid")
	code, err := Run(context.Background(), Options{
		Command: "sleep 60 & echo $! > " + childPID + "; exit 3",
		TTY:     tty,
		Signals: make(chan os.Signal),
		Grace:   300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	c := readPID(t, childPID)
	waitFor(t, "leftover child dead", 3*time.Second, func() bool { return !pidAlive(c) })
}

// The agent pane going away ends the host and its provider.
func TestRunEndsWhenAgentPaneGone(t *testing.T) {
	_, tty := testTTY(t)
	dir := t.TempDir()
	provPID := filepath.Join(dir, "prov.pid")
	var gone atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(context.Background(), Options{
			Command:     "echo $$ > " + provPID + "; exec sleep 60",
			TTY:         tty,
			Signals:     make(chan os.Signal),
			Probe:       func() Probe { return Probe{Gone: gone.Load()} },
			ProbePeriod: 30 * time.Millisecond,
			Grace:       300 * time.Millisecond,
		})
	}()
	p := readPID(t, provPID)
	gone.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the agent pane went away")
	}
	waitFor(t, "provider dead", 3*time.Second, func() bool { return !pidAlive(p) })
}

func TestRunRejectsMissingCommandOrTTY(t *testing.T) {
	if _, err := Run(context.Background(), Options{Command: "true"}); err == nil {
		t.Fatal("Run without a terminal must fail")
	}
	_, tty := testTTY(t)
	if _, err := Run(context.Background(), Options{TTY: tty, Command: "  "}); err == nil {
		t.Fatal("Run without a command must fail")
	}
}
