package panehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/panenav"
)

// fakeTmux is a Runner that answers from canned output by command name and
// records every call.
type fakeTmux struct {
	mu      sync.Mutex
	calls   [][]string
	answers map[string]string
	fail    map[string]bool
}

func (f *fakeTmux) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.fail[args[0]] {
		return "", errors.New("tmux " + args[0] + ": failed")
	}
	return f.answers[args[0]], nil
}

func (f *fakeTmux) called(name string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

func TestRunClickCarriesClient(t *testing.T) {
	master, tty := testTTY(t)
	in := filepath.Join(t.TempDir(), "in.log")
	var client atomic.Value
	client.Store("/dev/ttys009")
	sigs := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-stop; cancel() }()
		_, err := Run(ctx, Options{
			Command:     "exec cat > " + in,
			TTY:         tty,
			ClickClient: func() string { return client.Load().(string) },
			Grace:       300 * time.Millisecond,
			Signals:     make(chan os.Signal),
		})
		sigs <- err
	}()
	defer func() { close(stop); <-sigs }()
	waitFor(t, "hello", 3*time.Second, func() bool { return hasLine(in, "hello 2") })

	if _, err := master.Write([]byte("\x1b[<0;5;3M\x1b[<2;7;1M")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "click with client, rclick without", 3*time.Second, func() bool {
		return hasLine(in, "click 4 2 /dev/ttys009") && hasLine(in, "rclick 6 0")
	})
	// A name that would break the line is dropped, the click still goes.
	client.Store("bad name")
	if _, err := master.Write([]byte("\x1b[<0;2;2M")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "plain click", 3*time.Second, func() bool { return hasLine(in, "click 1 1") })
}

func TestParseNavControl(t *testing.T) {
	cases := []struct {
		line    string
		ok, bad bool
		want    Control
	}{
		{"goto 1a2b3c4d-1761600000", true, false, Control{"goto", "1a2b3c4d-1761600000"}},
		{"  goto   abc  ", true, false, Control{"goto", "abc"}},
		{"open https://plane.home/x/OPS-9/", true, false, Control{"open", "https://plane.home/x/OPS-9/"}},
		{"goto", true, true, Control{}},
		{"goto a b", true, true, Control{}},
		{"goto $(reboot)", true, true, Control{}},
		{"goto -t", true, true, Control{}},
		{"open file:///etc/passwd", true, true, Control{}},
		{"open javascript:alert(1)", true, true, Control{}},
		{"open https://x.y/ extra", true, true, Control{}},
		{"open", true, true, Control{}},
		{"notify info hi", false, false, Control{}},
		{"watch a b", false, false, Control{}},
		{"", false, false, Control{}},
	}
	for _, c := range cases {
		got, ok, err := ParseNavControl(c.line)
		if ok != c.ok || (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("ParseNavControl(%q) = %+v ok=%v err=%v; want %+v ok=%v bad=%v", c.line, got, ok, err, c.want, c.ok, c.bad)
		}
	}
}

func TestInstallClickBinding(t *testing.T) {
	// tmux's default binding is replaced.
	f := &fakeTmux{answers: map[string]string{"list-keys": "bind-key    -T root MouseDown1Pane select-pane -t = \\; send-keys -M\n"}}
	if !InstallClickBinding(f.run) {
		t.Fatal("default binding not replaced")
	}
	b := f.called("bind-key")
	if len(b) != 1 || strings.Join(b[0], " ") != strings.Join(clickBindingArgs, " ") {
		t.Fatalf("bind-key calls = %q", b)
	}
	// Ours already: nothing to do.
	f = &fakeTmux{answers: map[string]string{"list-keys": "bind-key -T root MouseDown1Pane if-shell -F -t = \"#{@agentdeck_pane}\" \"set-option -p -F -t = @agentdeck_pane_client '#{client_name}'\" \\; select-pane -t = \\; send-keys -M\n"}}
	if !InstallClickBinding(f.run) || len(f.called("bind-key")) != 0 {
		t.Fatalf("rebound an installed binding: %q", f.calls)
	}
	// The user's own binding is left alone.
	f = &fakeTmux{answers: map[string]string{"list-keys": "bind-key -T root MouseDown1Pane select-pane -t = \\; display-message hi\n"}}
	if InstallClickBinding(f.run) || len(f.called("bind-key")) != 0 {
		t.Fatalf("replaced a user binding: %q", f.calls)
	}
}

func TestReadClickClient(t *testing.T) {
	f := &fakeTmux{answers: map[string]string{"display-message": "/dev/ttys012\n"}}
	if got := ReadClickClient(f.run, "%3"); got != "/dev/ttys012" {
		t.Fatalf("ReadClickClient = %q", got)
	}
	if c := f.called("display-message"); len(c) != 1 || c[0][3] != "%3" || c[0][4] != "#{@agentdeck_pane_client}" {
		t.Fatalf("display-message call = %q", c)
	}
	f.answers["display-message"] = "\n"
	if got := ReadClickClient(f.run, "%3"); got != "" {
		t.Fatalf("ReadClickClient(unset) = %q", got)
	}
}

// navFixture: this pane is on tmux session "w1"; clients on it are a
// terminal (/dev/ttys001, pid 11) and a web terminal (/dev/ttys002, pid 22);
// /dev/ttys003 (pid 33) is on another session.
func navFixture() (*Navigator, *fakeTmux, *[]panenav.Event, *[]string) {
	f := &fakeTmux{answers: map[string]string{
		"list-clients": "/dev/ttys001|11|w1|100\n/dev/ttys002|22|w1|200\n/dev/ttys003|33|other|300\n",
	}}
	var webGot []panenav.Event
	var opened []string
	n := &Navigator{
		Run:         f.run,
		TmuxSession: "w1",
		Socket:      "",
		Lookup: func(id string) (string, string, bool) {
			switch id {
			case "s2":
				return "w2", "", true
			case "far":
				return "f1", "othersock", true
			case "stopped":
				return "", "", true
			}
			return "", "", false
		},
		Web: func(ev panenav.Event) (panenav.Reply, error) {
			if ev.ClientPID != 22 {
				return panenav.Reply{}, nil
			}
			webGot = append(webGot, ev)
			return panenav.Reply{Owned: true, OK: true}, nil
		},
		OpenLocal: func(u string) error { opened = append(opened, u); return nil },
	}
	return n, f, &webGot, &opened
}

func TestNavigatorGotoSwitchesTerminalClient(t *testing.T) {
	n, f, web, _ := navFixture()
	n.Clicked("/dev/ttys001")
	if ok, err := n.Handle("goto s2"); !ok || err != nil {
		t.Fatalf("Handle(goto s2) = %v, %v", ok, err)
	}
	sw := f.called("switch-client")
	if len(sw) != 1 || strings.Join(sw[0], " ") != "switch-client -c /dev/ttys001 -t =w2" {
		t.Fatalf("switch-client calls = %q", sw)
	}
	if len(*web) != 0 {
		t.Fatalf("a terminal client went to the web: %+v", *web)
	}
}

func TestNavigatorGotoWebClientGoesToPage(t *testing.T) {
	n, f, web, _ := navFixture()
	n.Clicked("/dev/ttys002")
	if err := n.Goto("s2"); err != nil {
		t.Fatal(err)
	}
	if len(f.called("switch-client")) != 0 {
		t.Fatal("a web client was switched with tmux")
	}
	if len(*web) != 1 || (*web)[0] != (panenav.Event{ClientPID: 22, Action: "goto", SessionID: "s2"}) {
		t.Fatalf("web events = %+v", *web)
	}
	// A stopped session: the page can still show it.
	if err := n.Goto("stopped"); err != nil || len(*web) != 2 {
		t.Fatalf("goto stopped for web = %v, %+v", err, *web)
	}
}

func TestNavigatorGotoRefusals(t *testing.T) {
	n, f, _, _ := navFixture()
	n.Clicked("/dev/ttys001")
	for _, id := range []string{"nope", "stopped", "far", "a;b"} {
		if err := n.Goto(id); err == nil {
			t.Errorf("Goto(%q) succeeded", id)
		}
	}
	// The clicker moved to another session: not followed there.
	n.Clicked("/dev/ttys003")
	if err := n.Goto("s2"); err == nil {
		t.Error("Goto for a client that left the session succeeded")
	}
	if sw := f.called("switch-client"); len(sw) != 0 {
		t.Fatalf("switch-client ran: %q", sw)
	}
}

func TestNavigatorFallsBackToMostActiveClient(t *testing.T) {
	n, f, web, _ := navFixture()
	// No click known: the most recently active client on w1 is the web
	// terminal (activity 200).
	if err := n.Goto("s2"); err != nil {
		t.Fatal(err)
	}
	if len(*web) != 1 || len(f.called("switch-client")) != 0 {
		t.Fatalf("fallback went to web=%+v switch=%q", *web, f.called("switch-client"))
	}
	// A click from a client that has since detached falls back the same way.
	n.Clicked("/dev/ttys999")
	if err := n.Goto("s2"); err != nil || len(*web) != 2 {
		t.Fatalf("fallback after detach = %v, %+v", err, *web)
	}
}

func TestNavigatorOpen(t *testing.T) {
	n, _, web, opened := navFixture()
	n.Clicked("/dev/ttys001")
	if ok, err := n.Handle("open https://example.com/a"); !ok || err != nil {
		t.Fatalf("open for terminal = %v, %v", ok, err)
	}
	if len(*opened) != 1 || (*opened)[0] != "https://example.com/a" || len(*web) != 0 {
		t.Fatalf("terminal open: local=%q web=%+v", *opened, *web)
	}
	n.Clicked("/dev/ttys002")
	if err := n.Open("https://example.com/b"); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 1 || len(*web) != 1 || (*web)[0].URL != "https://example.com/b" || (*web)[0].Action != "open" {
		t.Fatalf("web open: local=%q web=%+v", *opened, *web)
	}
	if ok, err := n.Handle("open file:///etc/passwd"); !ok || err == nil {
		t.Fatalf("open file URL = %v, %v", ok, err)
	}
	if len(*opened) != 1 || len(*web) != 1 {
		t.Fatal("a refused URL was opened")
	}
}

func TestNavigatorWebRefusalIsNotRetriedOnTmux(t *testing.T) {
	n, f, _, _ := navFixture()
	n.Web = func(ev panenav.Event) (panenav.Reply, error) {
		return panenav.Reply{Owned: true, Error: "unknown session"}, nil
	}
	n.Clicked("/dev/ttys002")
	if err := n.Goto("s2"); err == nil {
		t.Fatal("a page refusal was reported as success")
	}
	if len(f.called("switch-client")) != 0 {
		t.Fatal("a refused web goto fell back to switch-client")
	}
}

// Reopening slot 1 while slot 2 is still open puts slot 1 back on top of
// the column (split -b above slot 2) and gives the column the configured
// width again, instead of stacking slot 1 under slot 2 at whatever width
// the column drifted to.
func TestOpenSlot1AboveRemainingSlot2(t *testing.T) {
	// Window 160 wide; the agent pane %0 and slot 2 (%2) at 82 columns.
	row := func(pane, pid, w, slot, cmd string) string {
		return strings.Join([]string{"@1", pane, pid, w, "40", "0", "160", "0", "36%", "100", "30", "", slot, "0", "sess-1", cmd}, "|")
	}
	f := &fakeTmux{answers: map[string]string{
		"list-panes":   row("%0", "10", "77", "", "") + "\n" + row("%2", "12", "82", "2", "two") + "\n",
		"split-window": "%3|13\n",
		"list-keys":    defaultMouseDown1Pane + "\n",
	}}
	hp, created, err := Open(f.run, OpenSpec{
		TmuxSession: "agent", SessionID: "sess-1", Command: "one",
		Layout:      LayoutSpec{Width: "36%"},
		HostCommand: func(string, int) string { return "host" },
	})
	if err != nil || !created || hp.Slot != 1 || hp.PaneID != "%3" {
		t.Fatalf("Open = %+v created=%v err=%v", hp, created, err)
	}
	sp := f.called("split-window")
	if len(sp) != 1 {
		t.Fatalf("split-window calls = %q", sp)
	}
	got := strings.Join(sp[0], " ")
	if !strings.Contains(got, " -v ") || !strings.Contains(got, " -b ") || !strings.Contains(got, " -t %2 ") {
		t.Fatalf("split = %q, want a vertical split above slot 2 (-v -b -t %%2)", got)
	}
	var resized bool
	for _, c := range f.called("resize-pane") {
		if strings.Join(c, " ") == "resize-pane -t %3 -x 36%" {
			resized = true
		}
	}
	if !resized {
		t.Fatalf("column not set back to the configured width; resize-pane calls = %q", f.called("resize-pane"))
	}

	// Opening slot 2 below an open slot 1 stays a plain split below it.
	f = &fakeTmux{answers: map[string]string{
		"list-panes":   row("%0", "10", "100", "", "") + "\n" + row("%1", "11", "57", "1", "one") + "\n",
		"split-window": "%4|14\n",
		"list-keys":    defaultMouseDown1Pane + "\n",
	}}
	if _, _, err := Open(f.run, OpenSpec{TmuxSession: "agent", SessionID: "sess-1", Command: "two", HostCommand: func(string, int) string { return "host" }}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.called("split-window")[0], " "); strings.Contains(got, " -b ") || !strings.Contains(got, " -t %1 ") {
		t.Fatalf("slot 2 split = %q, want below slot 1", got)
	}
}
