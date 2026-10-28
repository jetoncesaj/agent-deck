package panenav

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidURL(t *testing.T) {
	for _, ok := range []string{
		"https://example.com/a?b=c#d",
		"http://localhost:8080/x",
		"https://plane.home/personal/browse/OPS-9/",
	} {
		if _, err := ValidURL(ok); err != nil {
			t.Errorf("ValidURL(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"mailto:a@b.c",
		"https://",
		"https:example.com",
		"//example.com/x",
		"example.com",
		"https://example.com/a b",
		"https://example.com/\x07",
		"https://example.com/\n; rm -rf /",
		"-a https://example.com",
		"https://example.com/" + string(make([]byte, MaxURLLen)),
	} {
		if _, err := ValidURL(bad); err == nil {
			t.Errorf("ValidURL(%q) accepted", bad)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	for _, ok := range []string{"a1b2c3", "8f14e45f-ceea-467f-a7c6-1b2f6a2c0d11", "x_y"} {
		if !ValidSessionID(ok) {
			t.Errorf("ValidSessionID(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "a b", "a;b", "../x", "-t", "a\nb", string(make([]byte, 129))} {
		if ValidSessionID(bad) {
			t.Errorf("ValidSessionID(%q) = true", bad)
		}
	}
}

func TestEventValidate(t *testing.T) {
	cases := []struct {
		ev Event
		ok bool
	}{
		{Event{ClientPID: 1, Action: ActionGoto, SessionID: "abc"}, true},
		{Event{ClientPID: 1, Action: ActionOpen, URL: "https://x.y/"}, true},
		{Event{ClientPID: 0, Action: ActionGoto, SessionID: "abc"}, false},
		{Event{ClientPID: 1, Action: ActionGoto, SessionID: "a b"}, false},
		{Event{ClientPID: 1, Action: ActionOpen, URL: "file:///x"}, false},
		{Event{ClientPID: 1, Action: "exec", URL: "https://x.y/"}, false},
	}
	for _, c := range cases {
		if err := c.ev.Validate(); (err == nil) != c.ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", c.ev, err, c.ok)
		}
	}
}

func TestRouterRoutesToTheOwningPage(t *testing.T) {
	r := NewRouter()
	var got1, got2 []Event
	un1 := r.Register(101, func(ev Event) error { got1 = append(got1, ev); return nil })
	r.Register(102, func(ev Event) error { got2 = append(got2, ev); return nil })

	rep := r.Route(Event{ClientPID: 102, Action: ActionGoto, SessionID: "s2"})
	if !rep.Owned || !rep.OK {
		t.Fatalf("Route to 102 = %+v", rep)
	}
	if len(got1) != 0 || len(got2) != 1 || got2[0].SessionID != "s2" {
		t.Fatalf("delivered to 101=%v 102=%v", got1, got2)
	}
	// Unknown client: not ours, the host acts itself.
	if rep := r.Route(Event{ClientPID: 999, Action: ActionGoto, SessionID: "s2"}); rep.Owned {
		t.Fatalf("Route to unknown client = %+v", rep)
	}
	// Ours but invalid: owned, refused, never delivered.
	if rep := r.Route(Event{ClientPID: 101, Action: ActionOpen, URL: "file:///etc/passwd"}); !rep.Owned || rep.OK || rep.Error == "" {
		t.Fatalf("Route invalid = %+v", rep)
	}
	// A page that cannot take it.
	r.Register(103, func(Event) error { return errors.New("page gone") })
	if rep := r.Route(Event{ClientPID: 103, Action: ActionGoto, SessionID: "s"}); !rep.Owned || rep.OK {
		t.Fatalf("Route to failing page = %+v", rep)
	}
	un1()
	if rep := r.Route(Event{ClientPID: 101, Action: ActionGoto, SessionID: "s"}); rep.Owned {
		t.Fatalf("Route after unregister = %+v", rep)
	}
}

func TestRouterReusedPidKeepsNewestRoute(t *testing.T) {
	r := NewRouter()
	var newer int
	unOld := r.Register(7, func(Event) error { return nil })
	r.Register(7, func(Event) error { newer++; return nil })
	unOld() // the old page closing must not drop the new page's route
	if rep := r.Route(Event{ClientPID: 7, Action: ActionGoto, SessionID: "s"}); !rep.OK || newer != 1 {
		t.Fatalf("Route = %+v newer=%d", rep, newer)
	}
}

// shortDir is a temp dir whose socket paths stay under the 104-byte limit.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestDeliverOverTheSocket(t *testing.T) {
	dir := shortDir(t)
	// No server yet: nobody owns anything.
	if rep, err := Deliver(dir, Event{ClientPID: 5, Action: ActionGoto, SessionID: "s1"}, time.Second); err != nil || rep.Owned {
		t.Fatalf("Deliver with no server = %+v, %v", rep, err)
	}
	r := NewRouter()
	if err := r.Listen(dir); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	st, err := os.Stat(filepath.Join(dir, socketName(os.Getpid())))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket stat = %v, %v", st, err)
	}
	// A stale socket nobody listens on is skipped.
	if err := os.WriteFile(filepath.Join(dir, "web-1.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := make(chan Event, 1)
	r.Register(5, func(ev Event) error { got <- ev; return nil })

	rep, err := Deliver(dir, Event{ClientPID: 5, Action: ActionOpen, URL: "https://example.com/x"}, time.Second)
	if err != nil || !rep.Owned || !rep.OK {
		t.Fatalf("Deliver = %+v, %v", rep, err)
	}
	if ev := <-got; ev.URL != "https://example.com/x" || ev.Action != ActionOpen {
		t.Fatalf("delivered %+v", ev)
	}
	if rep, _ := Deliver(dir, Event{ClientPID: 6, Action: ActionGoto, SessionID: "s1"}, time.Second); rep.Owned {
		t.Fatalf("Deliver for another client = %+v", rep)
	}
	// The host refuses to send an invalid event at all.
	if _, err := Deliver(dir, Event{ClientPID: 5, Action: ActionOpen, URL: "file:///x"}, time.Second); err == nil {
		t.Fatal("Deliver sent an invalid URL")
	}
	r.Close()
	if _, err := os.Stat(filepath.Join(dir, socketName(os.Getpid()))); !os.IsNotExist(err) {
		t.Fatalf("socket left after Close: %v", err)
	}
}
