package panehost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultGrace is how long a provider gets between SIGHUP and SIGKILL.
const DefaultGrace = time.Second

// lineQueueSize bounds the events waiting for a provider that is not reading
// its stdin. Events are hints, so a full queue drops the newest.
const lineQueueSize = 256

// Probe is one look at the agent pane, taken by Options.Probe.
type Probe struct {
	// Gone is true when the agent pane no longer exists or its process
	// exited (a dead pane kept by remain-on-exit counts as gone).
	Gone bool
	// Attached is the number of clients attached to the session.
	Attached int
	// WindowWidth is the agent window's width; a change triggers Layout.
	// 0 means unknown (Layout then runs on every probe).
	WindowWidth int
}

// Options configures Run.
type Options struct {
	// Command is the provider, run by /bin/sh -c.
	Command string
	Dir     string
	// Env is the provider's full environment.
	Env []string

	// TTY is the pane's terminal. The host reads input from it and the
	// provider's stdout and stderr are attached to it. Required.
	TTY *os.File

	// Fields returns the watched session fields (status, title, item, pr).
	// A change in any value sends "refresh FIELD VALUE". Nil: no polling.
	Fields       func() map[string]string
	FieldsPeriod time.Duration // default 2s

	// Probe looks at the agent pane. Gone ends the host; a rise in
	// Attached sends "attach". Nil: never probed.
	Probe       func() Probe
	ProbePeriod time.Duration // default 1s

	// Layout is called on start, on every resize and every probe period;
	// it applies the narrow/wide layout. Nil: no layout handling.
	Layout func()

	// OnControl receives each line the provider writes to its control fd.
	OnControl func(line string)

	// Grace between SIGHUP and SIGKILL. Default DefaultGrace.
	Grace time.Duration

	// Signals, when set, replaces the process signal subscription (tests).
	Signals <-chan os.Signal

	// Logf reports host-side problems. Nil: discarded.
	Logf func(format string, args ...any)
}

// Run starts the provider beside the session and supervises it until the
// provider exits, the agent pane goes away, the host is signalled (SIGHUP,
// SIGTERM, SIGINT) or ctx ends. On every one of those paths the provider's
// whole process group gets SIGHUP, then SIGKILL after Grace, so no viewer is
// left behind. It returns the provider's exit code (or -1 when it was
// stopped by the host).
func Run(ctx context.Context, o Options) (int, error) {
	if o.TTY == nil {
		return -1, errors.New("panehost: no terminal")
	}
	if strings.TrimSpace(o.Command) == "" {
		return -1, errors.New("panehost: no pane command")
	}
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	if o.FieldsPeriod <= 0 {
		o.FieldsPeriod = 2 * time.Second
	}
	if o.ProbePeriod <= 0 {
		o.ProbePeriod = time.Second
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	sigs := o.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 16)
		signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGWINCH, syscall.SIGUSR1)
		defer signal.Stop(ch)
		sigs = ch
		// A provider in the background process group must still be able
		// to set terminal modes on the pane: an ignored SIGTTOU survives
		// exec, so it is ignored here before the provider starts.
		signal.Ignore(syscall.SIGTTOU)
	}

	fd := int(o.TTY.Fd())
	if saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios); err == nil {
		mode := *saved
		// No echo, no line editing, no signals from keys: every byte goes
		// to the parser. Output processing stays on so a provider's "\n"
		// still starts a new line.
		mode.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
		mode.Iflag &^= unix.IXON | unix.ICRNL | unix.INLCR
		mode.Cc[unix.VMIN] = 1
		mode.Cc[unix.VTIME] = 0
		if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &mode); err != nil {
			logf("set terminal mode: %v", err)
		}
		defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, saved) }()
	}
	writeTTY := func(s string) { _, _ = io.WriteString(o.TTY, s) }
	writeTTY(mouseOn)
	defer writeTTY(mouseOff)

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return -1, fmt.Errorf("creating pane stdin pipe: %w", err)
	}
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return -1, fmt.Errorf("creating pane control pipe: %w", err)
	}

	cmd := exec.Command("/bin/sh", "-c", o.Command)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stdin = stdinR
	cmd.Stdout = o.TTY
	cmd.Stderr = o.TTY
	cmd.ExtraFiles = []*os.File{ctlW}
	// Own process group, so teardown reaches every process the provider
	// started (the orphaned-viewer pile-up came from children that outlived
	// the pane).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = ctlR.Close()
		_ = ctlW.Close()
		return -1, fmt.Errorf("starting pane command: %w", err)
	}
	_ = stdinR.Close()
	_ = ctlW.Close()
	pgid := cmd.Process.Pid

	var wg sync.WaitGroup
	done := make(chan struct{}) // closed when Run returns

	// Writer: one goroutine owns the provider's stdin.
	lines := make(chan string, lineQueueSize)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stdinW.Close()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return
				}
				if _, err := io.WriteString(stdinW, l+"\n"); err != nil {
					// Provider closed stdin: keep draining so senders
					// never block.
					continue
				}
			case <-done:
				return
			}
		}
	}()
	send := func(l string) {
		select {
		case lines <- l:
		default:
			logf("provider is not reading; dropped %q", l)
		}
	}

	// Control lines from the provider.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer ctlR.Close()
		sc := bufio.NewScanner(ctlR)
		sc.Buffer(make([]byte, 4096), 64*1024)
		for sc.Scan() {
			if o.OnControl != nil {
				o.OnControl(sc.Text())
			}
		}
	}()

	// Terminal input. The read blocks until the tty is closed or input
	// arrives; Run does not wait for this goroutine (a tty read cannot be
	// interrupted portably), it only stops forwarding.
	input := make(chan []string, 64)
	go func() {
		var p InputParser
		buf := make([]byte, 4096)
		for {
			n, err := o.TTY.Read(buf)
			if n > 0 {
				if evs := p.Feed(buf[:n]); len(evs) > 0 {
					select {
					case input <- evs:
					case <-done:
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Count the clients before the provider hears anything, so an attach
	// that lands while it starts up is still announced.
	attached, width := -1, 0
	if o.Probe != nil {
		pr := o.Probe()
		attached, width = pr.Attached, pr.WindowWidth
	}
	cols, rows := winsize(fd)
	send(fmt.Sprintf("hello %d", ProtocolVersion))
	send(fmt.Sprintf("resize %d %d", cols, rows))
	if o.Layout != nil {
		o.Layout()
	}

	var fields map[string]string
	if o.Fields != nil {
		fields = o.Fields()
	}
	fieldsTick := time.NewTicker(o.FieldsPeriod)
	defer fieldsTick.Stop()
	probeTick := time.NewTicker(o.ProbePeriod)
	defer probeTick.Stop()

	var exitErr error
	stopped := false
loop:
	for {
		select {
		case err := <-exited:
			exitErr = err
			break loop
		case <-ctx.Done():
			stopped = true
			break loop
		case evs := <-input:
			for _, l := range evs {
				send(l)
			}
		case s := <-sigs:
			switch s {
			case syscall.SIGWINCH:
				c, r := winsize(fd)
				if c != cols || r != rows {
					cols, rows = c, r
					send(fmt.Sprintf("resize %d %d", cols, rows))
				}
				// The provider is not in the terminal's foreground group,
				// so the kernel does not tell it; the host does.
				_ = syscall.Kill(-pgid, syscall.SIGWINCH)
				writeTTY(mouseOn)
				if o.Layout != nil {
					o.Layout()
				}
			case syscall.SIGUSR1:
				send("refresh manual")
			default:
				stopped = true
				break loop
			}
		case <-fieldsTick.C:
			if o.Fields == nil {
				continue
			}
			now := o.Fields()
			for _, k := range changedKeys(fields, now) {
				send(strings.TrimRight("refresh "+k+" "+oneLine(now[k]), " "))
			}
			if now != nil {
				fields = now
			}
		case <-probeTick.C:
			if o.Probe != nil {
				pr := o.Probe()
				if pr.Gone {
					stopped = true
					break loop
				}
				if attached >= 0 && pr.Attached > attached {
					writeTTY(mouseOn)
					send("attach")
				}
				attached = pr.Attached
				// A hidden (zoomed-away) pane gets no SIGWINCH when the
				// window widens again, so the window width is watched
				// here; layout runs only when it moved.
				if o.Layout != nil && (pr.WindowWidth == 0 || pr.WindowWidth != width) {
					o.Layout()
				}
				width = pr.WindowWidth
			} else if o.Layout != nil {
				o.Layout()
			}
		}
	}

	close(done)
	// Unblock a writer stuck on a provider that stopped reading, and the
	// control reader if something outside the group still holds its pipe.
	_ = stdinW.SetWriteDeadline(time.Now())
	code := -1
	if stopped {
		exitErr = stopGroup(pgid, o.Grace, exited)
	} else {
		// The provider exited on its own; anything it left behind in its
		// group goes the same way.
		reapGroup(pgid, o.Grace)
	}
	_ = ctlR.SetReadDeadline(time.Now())
	wg.Wait()
	if !stopped {
		code = exitCode(exitErr)
	}
	return code, nil
}

// stopGroup sends SIGHUP to the provider's group, waits up to grace for the
// provider and the rest of its group to exit, then SIGKILLs what is left.
func stopGroup(pgid int, grace time.Duration, exited <-chan error) error {
	_ = syscall.Kill(-pgid, syscall.SIGHUP)
	deadline := time.After(grace)
	var err error
	select {
	case err = <-exited:
		reapGroup(pgid, grace)
		return err
	case <-deadline:
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case err = <-exited:
	case <-time.After(2 * time.Second):
	}
	return err
}

// reapGroup ends a process group whose leader is gone: SIGHUP, a grace
// period while any member lives, then SIGKILL.
func reapGroup(pgid int, grace time.Duration) {
	if !groupAlive(pgid) {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGHUP)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func winsize(fd int) (cols, rows int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// changedKeys lists the keys whose value differs between a and b, sorted.
// A nil b (a failed read) reports nothing.
func changedKeys(a, b map[string]string) []string {
	if b == nil {
		return nil
	}
	var keys []string
	for k, v := range b {
		if old, ok := a[k]; !ok || old != v {
			if !ok && v == "" {
				continue
			}
			keys = append(keys, k)
		}
	}
	for k, v := range a {
		if _, ok := b[k]; !ok && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// oneLine keeps a value on one protocol line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
