package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/asheshgoplani/agent-deck/internal/panenav"
)

// fakeBridgePIDs makes every terminal attach succeed with a fake bridge
// whose tmux client pid is pids[tmuxSession].
func fakeBridgePIDs(t *testing.T, pids map[string]int) {
	t.Helper()
	orig := attachTmuxPTYBridge
	attachTmuxPTYBridge = func(tmuxSession, _ string, sessionID string, w *wsConnWriter) (*tmuxPTYBridge, error) {
		return &tmuxPTYBridge{tmuxSession: tmuxSession, sessionID: sessionID, writer: w, clientPID: pids[tmuxSession], done: make(chan struct{})}, nil
	}
	t.Cleanup(func() { attachTmuxPTYBridge = orig })
}

// openPage dials a session's terminal socket and reads until the terminal
// is attached, as the page does.
func openPage(t *testing.T, base, sessionID string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(base, "/ws/session/"+sessionID), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		var msg wsServerMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read %s: %v", sessionID, err)
		}
		if msg.Type == "status" && msg.Event == "terminal_attached" {
			return conn
		}
	}
}

// nextPaneNav reads the next pane_nav message, or fails after wait.
func nextPaneNav(conn *websocket.Conn, wait time.Duration) (wsServerMessage, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	for {
		var msg wsServerMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return wsServerMessage{}, false
		}
		if msg.Type == "pane_nav" {
			return msg, true
		}
	}
}

func paneNavServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := NewServer(Config{ListenAddr: "127.0.0.1:0", Profile: "work"})
	srv.menuData = &fakeMenuDataLoader{snapshot: &MenuSnapshot{
		Profile: "work",
		Items: []MenuItem{
			{Type: MenuItemTypeSession, Session: &MenuSession{ID: "sess-a", TmuxSession: "tm_a"}},
			{Type: MenuItemTypeSession, Session: &MenuSession{ID: "sess-b", TmuxSession: "tm_b"}},
			{Type: MenuItemTypeSession, Session: &MenuSession{ID: "sess-c", TmuxSession: "tm_c"}},
		},
	}}
	fakeBridgePIDs(t, map[string]int{"tm_a": 4101, "tm_b": 4102})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts.URL
}

// A pane's goto for one tmux client reaches the page that attached that
// client, and no other page.
func TestPaneNavRoutesGotoToTheOwningPage(t *testing.T) {
	srv, base := paneNavServer(t)
	pageA := openPage(t, base, "sess-a")
	pageB := openPage(t, base, "sess-b")

	rep := srv.paneNav.Route(panenav.Event{ClientPID: 4102, Action: panenav.ActionGoto, SessionID: "sess-c"})
	if !rep.Owned || !rep.OK {
		t.Fatalf("Route = %+v", rep)
	}
	msg, ok := nextPaneNav(pageB, 3*time.Second)
	if !ok || msg.Event != "goto" || msg.Target != "sess-c" || msg.SessionID != "sess-b" {
		t.Fatalf("page B got %+v ok=%v", msg, ok)
	}
	if msg, ok := nextPaneNav(pageA, 300*time.Millisecond); ok {
		t.Fatalf("page A also got %+v", msg)
	}
}

func TestPaneNavOpenAndRefusals(t *testing.T) {
	srv, base := paneNavServer(t)
	pageA := openPage(t, base, "sess-a")

	rep := srv.paneNav.Route(panenav.Event{ClientPID: 4101, Action: panenav.ActionOpen, URL: "https://example.com/item/9"})
	if !rep.OK {
		t.Fatalf("open Route = %+v", rep)
	}
	if msg, ok := nextPaneNav(pageA, 3*time.Second); !ok || msg.Event != "open" || msg.URL != "https://example.com/item/9" {
		t.Fatalf("page A got %+v ok=%v", msg, ok)
	}
	// A session this server does not show is refused, owned (so the host
	// does not switch the web client with tmux behind the page's back).
	rep = srv.paneNav.Route(panenav.Event{ClientPID: 4101, Action: panenav.ActionGoto, SessionID: "elsewhere"})
	if !rep.Owned || rep.OK {
		t.Fatalf("unknown session Route = %+v", rep)
	}
	rep = srv.paneNav.Route(panenav.Event{ClientPID: 4101, Action: panenav.ActionOpen, URL: "javascript:alert(1)"})
	if !rep.Owned || rep.OK {
		t.Fatalf("bad URL Route = %+v", rep)
	}
	if msg, ok := nextPaneNav(pageA, 300*time.Millisecond); ok {
		t.Fatalf("a refused event reached the page: %+v", msg)
	}
	// A client no page here attached is not ours.
	if rep := srv.paneNav.Route(panenav.Event{ClientPID: 9999, Action: panenav.ActionGoto, SessionID: "sess-a"}); rep.Owned {
		t.Fatalf("foreign client Route = %+v", rep)
	}
}

func TestPaneNavRouteEndsWithThePage(t *testing.T) {
	srv, base := paneNavServer(t)
	pageA := openPage(t, base, "sess-a")
	_ = pageA.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rep := srv.paneNav.Route(panenav.Event{ClientPID: 4101, Action: panenav.ActionGoto, SessionID: "sess-b"}); !rep.Owned {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the route outlived its page")
}
