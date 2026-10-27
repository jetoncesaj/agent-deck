package panehost

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Pane and window options the host sets. They are how a host pane is told
// apart from the agent pane and from panes someone split by hand.
const (
	optSlot      = "@agentdeck_pane"         // pane: slot number, 1 or 2
	optSession   = "@agentdeck_pane_session" // pane: Agent Deck session id
	optCommand   = "@agentdeck_pane_command" // pane: provider command
	optGraphics  = "@agentdeck_pane_graphics"
	optAgentPane = "@agentdeck_agent_pane"   // window: the agent pane id
	optWidth     = "@agentdeck_pane_width"   // window: column width ("36%")
	optZoomed    = "@agentdeck_pane_zoomed"  // window: 1 while the host zoomed the agent pane
	optNarrow    = "@agentdeck_pane_narrow"  // window: narrow threshold in columns
	optMinCols   = "@agentdeck_pane_mincols" // window: hide the column below this width

	// focusHook is the window hook slot the host owns. It sends focus back
	// to the agent pane when a host pane becomes active (a click selects
	// the pane under the mouse), so keys, `session send` and status reads
	// keep targeting the agent.
	focusHook = "window-pane-changed[4242]"
)

// MaxPanes is how many host panes one session can have (stacked in one column).
const MaxPanes = 2

// Layout defaults.
const (
	DefaultWidth      = "36%"
	DefaultNarrowCols = 100
	DefaultMinCols    = 30
)

// Runner runs one tmux command and returns its stdout.
type Runner func(args ...string) (string, error)

// SocketRunner runs tmux on the given -L socket ("" = tmux's default
// resolution, which inside a pane is the pane's own server).
func SocketRunner(socket string) Runner {
	return func(args ...string) (string, error) {
		out, err := tmux.Exec(socket, args...).Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				return string(out), fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
			}
			return string(out), fmt.Errorf("tmux %s: %w", args[0], err)
		}
		return string(out), nil
	}
}

// Layout settings for a window.
type LayoutSpec struct {
	Width      string // percent of the window, e.g. "36%"
	NarrowCols int    // below this window width the agent pane is zoomed
	MinCols    int    // a column narrower than this is hidden (zoom)
}

func (l LayoutSpec) withDefaults() LayoutSpec {
	if strings.TrimSpace(l.Width) == "" {
		l.Width = DefaultWidth
	}
	if l.NarrowCols <= 0 {
		l.NarrowCols = DefaultNarrowCols
	}
	if l.MinCols <= 0 {
		l.MinCols = DefaultMinCols
	}
	return l
}

// widthPercent parses "36%" or "36" into 36; out of range falls back.
func widthPercent(w string) int {
	w = strings.TrimSuffix(strings.TrimSpace(w), "%")
	n, err := strconv.Atoi(w)
	if err != nil || n < 5 || n > 90 {
		n, _ = strconv.Atoi(strings.TrimSuffix(DefaultWidth, "%"))
	}
	return n
}

// OpenSpec describes one host pane to open.
type OpenSpec struct {
	TmuxSession string // the Agent Deck session's tmux session name
	SessionID   string // Agent Deck session id
	Command     string // provider command, recorded on the pane
	Graphics    bool   // allow terminal passthrough (kitty graphics)
	Layout      LayoutSpec
	// HostCommand builds the shell command tmux runs in the new pane (the
	// supervisor), given the agent pane id and the slot.
	HostCommand func(agentPane string, slot int) string
}

// HostPane is one open host pane.
type HostPane struct {
	PaneID    string `json:"pane_id"`
	Slot      int    `json:"slot"`
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	Command   string `json:"command"`
	Graphics  bool   `json:"graphics"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Dead      bool   `json:"dead,omitempty"`
}

// WindowState is the primary window of a session as the host sees it.
type WindowState struct {
	WindowID  string
	AgentPane string
	Width     int
	Zoomed    bool
	Panes     []HostPane
}

const listFormat = "#{window_id}|#{pane_id}|#{pane_pid}|#{pane_width}|#{pane_height}|#{pane_dead}|#{window_width}|#{window_zoomed_flag}|#{@agentdeck_pane}|#{@agentdeck_pane_graphics}|#{@agentdeck_pane_session}|#{@agentdeck_pane_command}"

// Inspect reads the session's primary window: the agent pane (the first pane
// that is not a host pane) and the host panes in slot order.
func Inspect(run Runner, tmuxSession string) (WindowState, error) {
	out, err := run("list-panes", "-t", tmuxSession+":^", "-F", listFormat)
	if err != nil {
		return WindowState{}, err
	}
	var st WindowState
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "|", 12)
		if len(f) < 12 {
			continue
		}
		st.WindowID = f[0]
		st.Width, _ = strconv.Atoi(f[6])
		st.Zoomed = f[7] == "1"
		slot, _ := strconv.Atoi(f[8])
		if slot <= 0 {
			if st.AgentPane == "" {
				st.AgentPane = f[1]
			}
			continue
		}
		pid, _ := strconv.Atoi(f[2])
		w, _ := strconv.Atoi(f[3])
		h, _ := strconv.Atoi(f[4])
		st.Panes = append(st.Panes, HostPane{
			PaneID: f[1], Slot: slot, PID: pid, Width: w, Height: h,
			Dead: f[5] == "1", Graphics: f[9] == "1", SessionID: f[10], Command: f[11],
		})
	}
	if st.WindowID == "" {
		return st, fmt.Errorf("tmux session %q has no window", tmuxSession)
	}
	sortPanes(st.Panes)
	return st, nil
}

func sortPanes(p []HostPane) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Slot < p[j-1].Slot; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// ErrFull is returned when the session already has MaxPanes host panes.
var ErrFull = errors.New("session already has two side panes")

// Open opens a host pane in the session's primary window: a column to the
// right of the agent pane, or below the first host pane when one is open.
// Opening a command that is already open returns the existing pane.
func Open(run Runner, spec OpenSpec) (HostPane, bool, error) {
	if spec.HostCommand == nil {
		return HostPane{}, false, errors.New("panehost: no host command")
	}
	lay := spec.Layout.withDefaults()
	st, err := Inspect(run, spec.TmuxSession)
	if err != nil {
		return HostPane{}, false, err
	}
	if st.AgentPane == "" {
		return HostPane{}, false, fmt.Errorf("tmux session %q has no agent pane", spec.TmuxSession)
	}
	for _, p := range st.Panes {
		if p.Command == spec.Command {
			return p, false, nil
		}
	}
	if len(st.Panes) >= MaxPanes {
		return HostPane{}, false, ErrFull
	}
	slot := 1
	for _, p := range st.Panes {
		if p.Slot == slot {
			slot = 2
		}
	}
	hostCmd := spec.HostCommand(st.AgentPane, slot)
	var args []string
	if len(st.Panes) == 0 {
		args = []string{"split-window", "-d", "-h", "-t", st.AgentPane, "-l", fmt.Sprintf("%d%%", widthPercent(lay.Width)),
			"-P", "-F", "#{pane_id}|#{pane_pid}", hostCmd}
	} else {
		args = []string{"split-window", "-d", "-v", "-t", st.Panes[0].PaneID, "-l", "50%",
			"-P", "-F", "#{pane_id}|#{pane_pid}", hostCmd}
	}
	out, err := run(args...)
	if err != nil {
		return HostPane{}, false, err
	}
	f := strings.SplitN(strings.TrimSpace(out), "|", 2)
	paneID := f[0]
	pid := 0
	if len(f) == 2 {
		pid, _ = strconv.Atoi(f[1])
	}
	graphics := "0"
	if spec.Graphics {
		graphics = "1"
	}
	set := []string{
		"set-option", "-p", "-t", paneID, optSlot, strconv.Itoa(slot), ";",
		"set-option", "-p", "-t", paneID, optSession, spec.SessionID, ";",
		"set-option", "-p", "-t", paneID, optCommand, spec.Command, ";",
		"set-option", "-p", "-t", paneID, optGraphics, graphics, ";",
		"set-option", "-w", "-t", st.WindowID, optAgentPane, st.AgentPane, ";",
		"set-option", "-w", "-t", st.WindowID, optWidth, fmt.Sprintf("%d%%", widthPercent(lay.Width)), ";",
		"set-option", "-w", "-t", st.WindowID, optNarrow, strconv.Itoa(lay.NarrowCols), ";",
		"set-option", "-w", "-t", st.WindowID, optMinCols, strconv.Itoa(lay.MinCols), ";",
		// Focus goes back to the agent pane whenever a host pane is
		// selected; the click itself still reaches the host pane (the
		// default MouseDown1Pane binding forwards it with send-keys -M).
		"set-hook", "-w", "-t", st.WindowID, focusHook,
		fmt.Sprintf("if -F '#{%s}' 'select-pane -t %s'", optSlot, st.AgentPane),
	}
	if spec.Graphics {
		// Kitty graphics and other passthrough sequences from this pane
		// reach the outer terminal.
		set = append(set, ";", "set-option", "-p", "-t", paneID, "allow-passthrough", "on")
	}
	if _, err := run(set...); err != nil {
		_, _ = run("kill-pane", "-t", paneID)
		return HostPane{}, false, err
	}
	hp := HostPane{PaneID: paneID, Slot: slot, PID: pid, SessionID: spec.SessionID, Command: spec.Command, Graphics: spec.Graphics}
	ApplyLayout(run, spec.TmuxSession)
	return hp, true, nil
}

// Close ends the session's host panes (slot 0 = all). Each host is asked to
// stop with SIGTERM, which runs its SIGHUP/SIGKILL teardown of the provider;
// a pane still there after wait is killed.
func Close(run Runner, tmuxSession string, slot int, wait time.Duration) ([]HostPane, error) {
	st, err := Inspect(run, tmuxSession)
	if err != nil {
		return nil, err
	}
	var closed []HostPane
	for _, p := range st.Panes {
		if slot != 0 && p.Slot != slot {
			continue
		}
		if p.PID > 0 {
			_ = syscall.Kill(p.PID, syscall.SIGTERM)
		}
		closed = append(closed, p)
	}
	if len(closed) == 0 {
		return nil, nil
	}
	deadline := time.Now().Add(wait)
	for {
		left := 0
		now, err := Inspect(run, tmuxSession)
		if err != nil {
			break // session gone: nothing left to close
		}
		for _, p := range now.Panes {
			for _, c := range closed {
				if p.PaneID == c.PaneID {
					left++
				}
			}
		}
		if left == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, c := range closed {
		_, _ = run("kill-pane", "-t", c.PaneID)
	}
	Cleanup(run, tmuxSession, "")
	return closed, nil
}

// Cleanup removes the host's window hook and options, and undoes its zoom,
// when no host pane other than except is left in the window. A host calls
// it on its way out with its own pane id.
func Cleanup(run Runner, tmuxSession, except string) {
	st, err := Inspect(run, tmuxSession)
	if err != nil {
		return
	}
	for _, p := range st.Panes {
		if p.PaneID != except {
			return
		}
	}
	zoomed, _ := run("show-options", "-wqv", "-t", st.WindowID, optZoomed)
	if strings.TrimSpace(zoomed) == "1" && st.Zoomed {
		_, _ = run("resize-pane", "-Z", "-t", st.AgentPane)
	}
	_, _ = run(
		"set-hook", "-uw", "-t", st.WindowID, focusHook, ";",
		"set-option", "-wqu", "-t", st.WindowID, optZoomed, ";",
		"set-option", "-wqu", "-t", st.WindowID, optAgentPane, ";",
		"set-option", "-wqu", "-t", st.WindowID, optWidth, ";",
		"set-option", "-wqu", "-t", st.WindowID, optNarrow, ";",
		"set-option", "-wqu", "-t", st.WindowID, optMinCols,
	)
}

// LayoutDecision is what ApplyLayout does for a window.
type LayoutDecision int

const (
	LayoutNone    LayoutDecision = iota
	LayoutZoom                   // narrow: zoom the agent pane
	LayoutRestore                // wide again: unzoom and restore the column width
)

// decideLayout is the pure narrow/wide rule. The host only undoes a zoom it
// made itself, so a zoom the person chose on a wide window is left alone.
func decideLayout(windowWidth int, zoomed, hostZoomed bool, l LayoutSpec) LayoutDecision {
	l = l.withDefaults()
	col := windowWidth * widthPercent(l.Width) / 100
	narrow := windowWidth < l.NarrowCols || col < l.MinCols
	switch {
	case narrow && !zoomed:
		return LayoutZoom
	case !narrow && hostZoomed:
		return LayoutRestore
	}
	return LayoutNone
}

// ApplyLayout applies the phone layout to the session's primary window: a
// narrow window (a phone through the web app) zooms the agent pane so it
// gets the full width and the host panes are hidden; when the window is wide
// again the zoom is undone and the column gets its width back. Windows with
// no host pane are left alone. Safe to call from several hosts at once: the
// zoom toggle is guarded inside tmux.
func ApplyLayout(run Runner, tmuxSession string) LayoutDecision {
	st, err := Inspect(run, tmuxSession)
	if err != nil || len(st.Panes) == 0 || st.AgentPane == "" {
		return LayoutNone
	}
	opts, _ := run("display-message", "-p", "-t", st.AgentPane,
		"#{"+optWidth+"}|#{"+optNarrow+"}|#{"+optMinCols+"}|#{"+optZoomed+"}")
	f := strings.SplitN(strings.TrimSpace(opts), "|", 4)
	for len(f) < 4 {
		f = append(f, "")
	}
	narrow, _ := strconv.Atoi(f[1])
	minCols, _ := strconv.Atoi(f[2])
	l := LayoutSpec{Width: f[0], NarrowCols: narrow, MinCols: minCols}
	d := decideLayout(st.Width, st.Zoomed, f[3] == "1", l)
	switch d {
	case LayoutZoom:
		_, _ = run(
			"if-shell", "-F", "-t", st.AgentPane, "#{?window_zoomed_flag,,1}",
			"resize-pane -Z -t "+st.AgentPane+" ; set-option -w -t "+st.WindowID+" "+optZoomed+" 1")
	case LayoutRestore:
		_, _ = run(
			"if-shell", "-F", "-t", st.AgentPane, "#{window_zoomed_flag}",
			"resize-pane -Z -t "+st.AgentPane, ";",
			"set-option", "-wqu", "-t", st.WindowID, optZoomed, ";",
			"resize-pane", "-t", st.Panes[0].PaneID, "-x", fmt.Sprintf("%d%%", widthPercent(l.Width)))
	}
	return d
}

// ProbeAgent looks at the agent pane: gone (closed or dead) and how many
// clients are attached to its session.
func ProbeAgent(run Runner, agentPane string) Probe {
	out, err := run("display-message", "-p", "-t", agentPane, "#{pane_id}|#{pane_dead}|#{session_attached}")
	if err != nil {
		return Probe{Gone: true}
	}
	f := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(f) < 3 || f[0] != agentPane {
		return Probe{Gone: true}
	}
	n, _ := strconv.Atoi(f[2])
	return Probe{Gone: f[1] == "1", Attached: n}
}

// Signal sends sig to the host process of every host pane in the session.
func Signal(run Runner, tmuxSession string, sig syscall.Signal) int {
	st, err := Inspect(run, tmuxSession)
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range st.Panes {
		if p.PID > 0 && syscall.Kill(p.PID, sig) == nil {
			n++
		}
	}
	return n
}
