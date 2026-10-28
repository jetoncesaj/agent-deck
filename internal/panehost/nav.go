package panehost

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/panenav"
)

// defaultMouseDown1Pane is tmux's own MouseDown1Pane binding, as list-keys
// prints it (whitespace collapsed).
const defaultMouseDown1Pane = `bind-key -T root MouseDown1Pane select-pane -t = \; send-keys -M`

// clickBindingArgs is the MouseDown1Pane binding the host installs: tmux's
// default, preceded on a host pane by recording the clicking client in the
// pane's @agentdeck_pane_client option. Other panes behave exactly as before.
var clickBindingArgs = []string{
	"bind-key", "-T", "root", "MouseDown1Pane",
	"if-shell", "-F", "-t", "=", "#{" + optSlot + "}",
	"set-option -p -F -t = " + optClient + " '#{client_name}'",
	`\;`, "select-pane", "-t", "=",
	`\;`, "send-keys", "-M",
}

// InstallClickBinding makes tmux record which client clicks in a host pane,
// so a click line can carry it. It replaces only tmux's default
// MouseDown1Pane binding (or an earlier copy of its own): a binding the user
// made is left alone, and clicks then carry no client.
func InstallClickBinding(run Runner) bool {
	out, err := run("list-keys", "-T", "root", "MouseDown1Pane")
	if err != nil {
		return false
	}
	cur := strings.Join(strings.Fields(out), " ")
	if strings.Contains(cur, optClient) {
		return true
	}
	if cur != defaultMouseDown1Pane {
		return false
	}
	_, err = run(clickBindingArgs...)
	return err == nil
}

// ReadClickClient returns the tmux client that last clicked in pane, as
// recorded by the click binding ("" when none is known).
func ReadClickClient(run Runner, pane string) string {
	out, err := run("display-message", "-p", "-t", pane, "#{"+optClient+"}")
	if err != nil {
		return ""
	}
	c := strings.TrimSpace(out)
	if !validClientName(c) {
		return ""
	}
	return c
}

// validClientName accepts a tmux client name that fits on a protocol line:
// a tty path such as /dev/ttys012, or client-<pid>.
func validClientName(c string) bool {
	if c == "" || len(c) > 256 {
		return false
	}
	for _, r := range c {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Control is one parsed navigation line from a provider's control fd.
type Control struct {
	Verb string // "goto" or "open"
	Arg  string // the session id or the URL
}

// ParseNavControl parses "goto <session-id>" and "open <url>". ok is false
// for any other verb (the caller handles or ignores it); err is set when the
// verb is a navigation verb with a bad argument.
func ParseNavControl(line string) (c Control, ok bool, err error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return Control{}, false, nil
	}
	switch f[0] {
	case panenav.ActionGoto:
		if len(f) != 2 || !panenav.ValidSessionID(f[1]) {
			return Control{}, true, fmt.Errorf("goto wants one session id, got %q", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0])))
		}
		return Control{Verb: f[0], Arg: f[1]}, true, nil
	case panenav.ActionOpen:
		if len(f) != 2 {
			return Control{}, true, errors.New("open wants one http(s) URL")
		}
		u, err := panenav.ValidURL(f[1])
		if err != nil {
			return Control{}, true, err
		}
		return Control{Verb: f[0], Arg: u}, true, nil
	}
	return Control{}, false, nil
}

// Client is one tmux client as list-clients reports it.
type Client struct {
	Name     string
	PID      int
	Session  string
	Activity int64
}

// ListClients lists the server's clients.
func ListClients(run Runner) ([]Client, error) {
	out, err := run("list-clients", "-F", "#{client_name}|#{client_pid}|#{client_session}|#{client_activity}")
	if err != nil {
		return nil, err
	}
	var cs []Client
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(l, "|", 4)
		if len(f) < 4 || f[0] == "" {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		act, _ := strconv.ParseInt(f[3], 10, 64)
		cs = append(cs, Client{Name: f[0], PID: pid, Session: f[2], Activity: act})
	}
	return cs, nil
}

// Navigator carries out a provider's goto and open lines for the tmux client
// that last clicked in the pane. A client the web terminal attached is
// handed to its web server (the page follows); any other client is a
// terminal, switched with tmux or served by the local URL opener.
type Navigator struct {
	Run         Runner
	TmuxSession string // the pane's own tmux session
	Socket      string // the tmux -L socket Run talks to ("" = default)

	// Lookup resolves a session id of the profile to its tmux session and
	// socket ("" when it is not running). ok is false for an unknown id.
	Lookup func(id string) (tmuxSession, socket string, ok bool)
	// Web offers ev to the web server that owns the client. A zero Reply
	// (Owned false) means no web server does. Nil: no web delivery.
	Web func(ev panenav.Event) (panenav.Reply, error)
	// OpenLocal opens a URL on this machine (open / xdg-open).
	OpenLocal func(url string) error

	mu     sync.Mutex
	client string
	at     time.Time
}

// Clicked records the client of the latest click ("" leaves it unchanged).
func (n *Navigator) Clicked(client string) {
	if client == "" {
		return
	}
	n.mu.Lock()
	n.client, n.at = client, time.Now()
	n.mu.Unlock()
}

// Handle runs a goto or open control line. ok is false when line is not a
// navigation line.
func (n *Navigator) Handle(line string) (ok bool, err error) {
	c, ok, err := ParseNavControl(line)
	if !ok || err != nil {
		return ok, err
	}
	if c.Verb == panenav.ActionGoto {
		return true, n.Goto(c.Arg)
	}
	return true, n.Open(c.Arg)
}

// resolveClient picks the client to act for: the one that last clicked when
// it is still on this pane's session; when no click is known (or that client
// is gone), the most recently active client on the session. A clicker that
// has since moved to another session is not followed there.
func (n *Navigator) resolveClient() (Client, error) {
	cs, err := ListClients(n.Run)
	if err != nil {
		return Client{}, err
	}
	n.mu.Lock()
	want := n.client
	n.mu.Unlock()
	if want != "" {
		for _, c := range cs {
			if c.Name != want {
				continue
			}
			if c.Session != n.TmuxSession {
				return Client{}, fmt.Errorf("client %s has left this session", c.Name)
			}
			return c, nil
		}
	}
	var best Client
	found := false
	for _, c := range cs {
		if c.Session == n.TmuxSession && (!found || c.Activity > best.Activity) {
			best, found = c, true
		}
	}
	if !found {
		return Client{}, errors.New("no client is attached to this session")
	}
	return best, nil
}

// web offers ev to the web servers; true when one of them owns the client
// (its error, if the page could not take it, is returned with it).
func (n *Navigator) web(ev panenav.Event) (bool, error) {
	if n.Web == nil || ev.ClientPID <= 0 {
		return false, nil
	}
	rep, err := n.Web(ev)
	if err != nil || !rep.Owned {
		return false, nil
	}
	if !rep.OK {
		return true, fmt.Errorf("web page refused %s: %s", ev.Action, rep.Error)
	}
	return true, nil
}

// Goto takes the clicking client to session id: the web page selects it, a
// terminal client is switched to its tmux session.
func (n *Navigator) Goto(id string) error {
	if !panenav.ValidSessionID(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	if n.Lookup == nil {
		return errors.New("no session lookup")
	}
	target, socket, ok := n.Lookup(id)
	if !ok {
		return fmt.Errorf("unknown session %s", id)
	}
	c, err := n.resolveClient()
	if err != nil {
		return err
	}
	if owned, err := n.web(panenav.Event{ClientPID: c.PID, Action: panenav.ActionGoto, SessionID: id}); owned {
		return err
	}
	if target == "" {
		return fmt.Errorf("session %s is not running", id)
	}
	if socket != n.Socket {
		return fmt.Errorf("session %s is on another tmux server", id)
	}
	_, err = n.Run("switch-client", "-c", c.Name, "-t", "="+target)
	return err
}

// Open opens url for the clicking client: in the browser of its web page, or
// with the local opener for a terminal (or when no client is known).
func (n *Navigator) Open(url string) error {
	u, err := panenav.ValidURL(url)
	if err != nil {
		return err
	}
	if c, err := n.resolveClient(); err == nil {
		if owned, err := n.web(panenav.Event{ClientPID: c.PID, Action: panenav.ActionOpen, URL: u}); owned {
			return err
		}
	}
	if n.OpenLocal == nil {
		return errors.New("no local opener")
	}
	return n.OpenLocal(u)
}
