// Package panenav carries a side pane's navigation request ("go to session
// S", "open URL U") to the web server that owns the tmux client which made
// the request, so the page in the browser follows it.
//
// A pane host (internal/panehost) acts for the tmux client that last clicked
// in its pane. A terminal client is switched with tmux itself; a client the
// web terminal attached is a page in a browser, and only that page can change
// what it shows or open a link on the phone. Every web server listens on one
// socket in Dir() and keeps a route per tmux client it attached (keyed by the
// client's pid, which tmux reports as #{client_pid}). The host asks each
// socket in turn; the server that owns the client answers Owned and hands the
// event to that page.
//
// Both ends validate: a session id is a short token, a URL is http or https.
// Nothing in an event is ever run.
package panenav

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/agentpaths"
)

// Actions.
const (
	ActionGoto = "goto"
	ActionOpen = "open"
)

// Event is one navigation request for the page behind a tmux client.
type Event struct {
	ClientPID int    `json:"client_pid"`
	Action    string `json:"action"`
	SessionID string `json:"session_id,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Reply is a server's answer. Owned means the client is one of this
// server's; OK means the page was sent the event.
type Reply struct {
	Owned bool   `json:"owned"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// MaxURLLen bounds a URL a pane may open.
const MaxURLLen = 2048

// ValidSessionID reports whether id has the shape of an Agent Deck session
// id. Whether it names a real session is for the caller to check.
func ValidSessionID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '-' {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ValidURL returns the URL when it is an absolute http or https URL with a
// host and no whitespace or control characters; otherwise an error.
func ValidURL(raw string) (string, error) {
	if raw == "" || len(raw) > MaxURLLen {
		return "", errors.New("url is empty or too long")
	}
	for _, c := range raw {
		if c <= ' ' || c == 0x7f {
			return "", errors.New("url contains whitespace or control characters")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url does not parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" || u.Opaque != "" {
		return "", errors.New("url has no host")
	}
	return raw, nil
}

// Validate checks an event's shape.
func (e Event) Validate() error {
	if e.ClientPID <= 0 {
		return errors.New("no client pid")
	}
	switch e.Action {
	case ActionGoto:
		if !ValidSessionID(e.SessionID) {
			return fmt.Errorf("invalid session id %q", e.SessionID)
		}
	case ActionOpen:
		if _, err := ValidURL(e.URL); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown action %q", e.Action)
	}
	return nil
}

// Dir is where web servers put their sockets: <data dir>/runtime/panenav.
func Dir() (string, error) {
	return agentpaths.EffectiveDataPath(filepath.Join("runtime", "panenav"), "runtime")
}

// Router keeps a web server's routes from tmux client pid to the page that
// attached the client, and serves the socket hosts ask.
type Router struct {
	mu     sync.Mutex
	routes map[int]route
	seq    int

	ln   net.Listener
	path string
	wg   sync.WaitGroup
}

type route struct {
	id      int
	deliver func(Event) error
}

// NewRouter returns a router with no socket; Listen adds one.
func NewRouter() *Router {
	return &Router{routes: map[int]route{}}
}

// Register routes events for the tmux client clientPID to deliver until the
// returned function is called. A later registration for the same pid (a
// reused pid) replaces the earlier one, and the earlier unregister then
// leaves it alone.
func (r *Router) Register(clientPID int, deliver func(Event) error) (unregister func()) {
	if r == nil || clientPID <= 0 || deliver == nil {
		return func() {}
	}
	r.mu.Lock()
	r.seq++
	id := r.seq
	r.routes[clientPID] = route{id: id, deliver: deliver}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if cur, ok := r.routes[clientPID]; ok && cur.id == id {
			delete(r.routes, clientPID)
		}
		r.mu.Unlock()
	}
}

// Route hands ev to the page that owns its client. A client this router does
// not know is answered Owned=false, so the host may act for it itself.
func (r *Router) Route(ev Event) Reply {
	if r == nil {
		return Reply{}
	}
	r.mu.Lock()
	rt, ok := r.routes[ev.ClientPID]
	r.mu.Unlock()
	if !ok {
		return Reply{}
	}
	if err := ev.Validate(); err != nil {
		return Reply{Owned: true, Error: err.Error()}
	}
	if err := rt.deliver(ev); err != nil {
		return Reply{Owned: true, Error: err.Error()}
	}
	return Reply{Owned: true, OK: true}
}

// socketName is this process's socket file name in Dir().
func socketName(pid int) string { return "web-" + strconv.Itoa(pid) + ".sock" }

// Listen serves the router on dir/web-<pid>.sock (dir is created 0700, the
// socket is 0600). A stale socket of a dead process with the same pid is
// replaced.
func (r *Router) Listen(dir string) error {
	if r == nil {
		return errors.New("panenav: nil router")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	path := filepath.Join(dir, socketName(os.Getpid()))
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	r.mu.Lock()
	r.ln, r.path = ln, path
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.serve(c)
			}()
		}
	}()
	return nil
}

// serve answers one request: one JSON event line in, one JSON reply line out.
func (r *Router) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(c, 8<<10)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var ev Event
	reply := Reply{}
	if err := json.Unmarshal(line, &ev); err != nil {
		reply.Error = "bad request"
	} else {
		reply = r.Route(ev)
	}
	b, _ := json.Marshal(reply)
	_, _ = c.Write(append(b, '\n'))
}

// Close stops serving and removes the socket.
func (r *Router) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	ln, path := r.ln, r.path
	r.ln, r.path = nil, ""
	r.mu.Unlock()
	if ln == nil {
		return
	}
	_ = ln.Close()
	_ = os.Remove(path)
	r.wg.Wait()
}

// Deliver asks every web server socket in dir to take ev. It returns the
// reply of the server that owns the client (Owned=true), or a zero Reply
// when none does. Sockets nobody listens on are skipped.
func Deliver(dir string, ev Event, timeout time.Duration) (Reply, error) {
	if err := ev.Validate(); err != nil {
		return Reply{}, err
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Reply{}, nil
		}
		return Reply{}, err
	}
	var socks []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "web-") && strings.HasSuffix(n, ".sock") && e.Type()&os.ModeSocket != 0 {
			socks = append(socks, filepath.Join(dir, n))
		}
	}
	sort.Strings(socks)
	b, err := json.Marshal(ev)
	if err != nil {
		return Reply{}, err
	}
	b = append(b, '\n')
	for _, s := range socks {
		rep, err := ask(s, b, timeout)
		if err != nil {
			continue
		}
		if rep.Owned {
			return rep, nil
		}
	}
	return Reply{}, nil
}

func ask(sock string, req []byte, timeout time.Duration) (Reply, error) {
	c, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return Reply{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(req); err != nil {
		return Reply{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(c, 8<<10)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Reply{}, err
	}
	var rep Reply
	if err := json.Unmarshal(line, &rep); err != nil {
		return Reply{}, err
	}
	return rep, nil
}
