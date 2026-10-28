package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/desknotify"
)

// "Asks you": set, parse, clear, JSON-facing state, notification dedupe and
// the rule that tells the person's message from agent-deck's relays.

type askFrames struct {
	mu   sync.Mutex
	got  []string
	data []any
}

func captureAskFrames(t *testing.T) *askFrames {
	t.Helper()
	f := &askFrames{}
	prev := publishAskFrame
	publishAskFrame = func(profile, kind, sessionID string, data any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.got = append(f.got, kind+"@"+sessionID)
		f.data = append(f.data, data)
	}
	t.Cleanup(func() { publishAskFrame = prev })
	return f
}

func (f *askFrames) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func TestAsks_ParseMarker(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		want AsksMarkerLine
	}{
		{"===AGENTDECK_ASKS=== kind=pick question=Ship layout A or B?", true,
			AsksMarkerLine{Ask: PersonAsk{Kind: "pick", Question: "Ship layout A or B?"}}},
		{"**===AGENTDECK_ASKS=== kind=Approval item=ITEM-12 question=Merge the migration?**", true,
			AsksMarkerLine{Ask: PersonAsk{Kind: "approval", Item: "ITEM-12", Question: "Merge the migration?"}}},
		{"- ===AGENTDECK_ASKS=== kind=fix question=  The   staging key   expired ", true,
			AsksMarkerLine{Ask: PersonAsk{Kind: "fix", Question: "The staging key expired"}}},
		{"===AGENTDECK_ASKS=== clear", true, AsksMarkerLine{Clear: true}},
		{"===AGENTDECK_ASKS=== kind=whim question=?", false, AsksMarkerLine{}},
		{"===AGENTDECK_ASKS=== kind=decision", false, AsksMarkerLine{}},
		{"===AGENTDECK_ASKS=== kind=decision question=", false, AsksMarkerLine{}},
		{"print ===AGENTDECK_ASKS=== kind=pick question=x when you need me", false, AsksMarkerLine{}},
		{"ASK: which branch?", false, AsksMarkerLine{}},
	}
	for _, tc := range cases {
		got, ok := ParseAsksMarker(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%q: got %+v %v, want %+v %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
	last, ok := ScanAsksMarker("x\n===AGENTDECK_ASKS=== kind=pick question=A?\nmore\n===AGENTDECK_ASKS=== kind=decision question=B?")
	if !ok || last.Ask.Kind != "decision" || last.Ask.Question != "B?" {
		t.Fatalf("last marker wins: %+v", last)
	}
}

// An ask's question never reads as a question to the parent, and the marker
// cannot be taken for a NEED: line; a NEED: printed alongside still is one.
func TestAsks_MarkerIsNotAParentQuestion(t *testing.T) {
	if textAsksParent("Two layouts are ready.\n===AGENTDECK_ASKS=== kind=pick question=Ship A or B?") {
		t.Fatal("an asks-you line must not count as a question to the parent")
	}
	if textHasNeedMarker("===AGENTDECK_ASKS=== kind=pick question=Ship A or B?") {
		t.Fatal("the marker is not a NEED: line")
	}
	if !textAsksParent("===AGENTDECK_ASKS=== kind=fix question=Key expired\nNEED: conductor, pause the queue") {
		t.Fatal("a NEED: line next to the ask still asks the parent")
	}
}

func TestAsks_SetClearAndEvents(t *testing.T) {
	reviewTestHome(t, "default")
	frames := captureAskFrames(t)

	if _, open := LoadAsk("w1"); open {
		t.Fatal("no ask yet")
	}
	if _, _, err := SetAsk("default", "w1", PersonAsk{Kind: "nope", Question: "x"}); err == nil {
		t.Fatal("unknown kind must be refused")
	}
	if _, _, err := SetAsk("default", "w1", PersonAsk{Kind: "pick", Question: "  "}); err == nil {
		t.Fatal("empty question must be refused")
	}
	a, changed, err := SetAsk("default", "w1", PersonAsk{Kind: "Pick", Question: "Ship A\nor B?", Item: "ITEM-12"})
	if err != nil || !changed || a.Kind != "pick" || a.Question != "Ship A or B?" || a.Since.IsZero() || a.Source != AskSourceCLI {
		t.Fatalf("set: %+v %v %v", a, changed, err)
	}
	// The same ask again is no change: same since, no second frame.
	again, changed, _ := SetAsk("default", "w1", PersonAsk{Kind: "pick", Question: "Ship A or B?", Item: "ITEM-12"})
	if changed || !again.Since.Equal(a.Since) {
		t.Fatalf("repeat set must be a no-op: %+v %v", again, changed)
	}
	got, open := LoadAsk("w1")
	if !open || *got != a {
		t.Fatalf("load: %+v", got)
	}
	if open := OpenAsks(); len(open) != 1 || open["w1"] == nil {
		t.Fatalf("open asks: %+v", open)
	}
	if got.PaneValue() != "pick: Ship A or B?" {
		t.Fatalf("pane value %q", got.PaneValue())
	}

	cleared, err := ClearAsk("default", "w1", AskClearedByCLI)
	if err != nil || !cleared {
		t.Fatalf("clear: %v %v", cleared, err)
	}
	if cleared, _ := ClearAsk("default", "w1", AskClearedByCLI); cleared {
		t.Fatal("second clear has nothing to clear")
	}
	if _, open := LoadAsk("w1"); open {
		t.Fatal("ask still open")
	}
	if k := frames.kinds(); strings.Join(k, ",") != "session.asks.set@w1,session.asks.cleared@w1" {
		t.Fatalf("frames: %v", k)
	}
}

// A marker is applied once per turn: seeing the same turn again after the
// person answered does not reopen the ask.
func TestAsks_MarkerAppliedOncePerTurn(t *testing.T) {
	reviewTestHome(t, "default")
	captureAskFrames(t)
	text := "Done with the draft.\n===AGENTDECK_ASKS=== kind=decision question=Publish now?"
	if !ApplyAsksMarker("default", "w2", "turn-a", text) {
		t.Fatal("marker must open the ask")
	}
	a, open := LoadAsk("w2")
	if !open || a.Source != AskSourceMarker || a.Kind != "decision" {
		t.Fatalf("ask: %+v", a)
	}
	if ApplyAsksMarker("default", "w2", "turn-a", text) {
		t.Fatal("same turn again is no change")
	}
	if !ClearAskOnPrompt("default", "w2", "yes, publish") {
		t.Fatal("the person's answer clears it")
	}
	if ApplyAsksMarker("default", "w2", "turn-a", text) {
		t.Fatal("re-observing the answered turn must not reopen the ask")
	}
	if _, open := LoadAsk("w2"); open {
		t.Fatal("reopened")
	}
	if !ApplyAsksMarker("default", "w2", "turn-b", "===AGENTDECK_ASKS=== kind=fix question=Token expired") {
		t.Fatal("a new turn's marker opens a new ask")
	}
	if !ApplyAsksMarker("default", "w2", "turn-c", "Fixed it myself.\n===AGENTDECK_ASKS=== clear") {
		t.Fatal("the clear marker clears it")
	}
	if _, open := LoadAsk("w2"); open {
		t.Fatal("clear marker left the ask open")
	}
}

func TestAsks_NotifiedOncePerAskAndChannel(t *testing.T) {
	reviewTestHome(t, "default")
	captureAskFrames(t)
	a, _, _ := SetAsk("default", "w3", PersonAsk{Kind: "approval", Question: "Merge?"})
	if !MarkAskNotified("w3", &a, "push") {
		t.Fatal("first push claim")
	}
	if MarkAskNotified("w3", &a, "push") {
		t.Fatal("second push claim must fail (dedupe)")
	}
	if !MarkAskNotified("w3", &a, "desktop") {
		t.Fatal("desktop is its own channel")
	}
	// A new ask (cleared and raised again) is notified again.
	_, _ = ClearAsk("default", "w3", AskClearedByCLI)
	time.Sleep(2 * time.Millisecond)
	b, _, _ := SetAsk("default", "w3", PersonAsk{Kind: "approval", Question: "Merge?"})
	if b.Key() == a.Key() {
		t.Fatal("a new ask must have a new key")
	}
	if !MarkAskNotified("w3", &b, "push") {
		t.Fatal("new ask must be notified")
	}
	title, body := AskNotificationText("worker", &b)
	if title != "worker" || body != "asks you (approval): Merge?" {
		t.Fatalf("text: %q %q", title, body)
	}
}

func TestAsks_PersonVersusRelayed(t *testing.T) {
	reviewTestHome(t, "default")
	captureAskFrames(t)
	_, _, _ = SetAsk("default", "w4", PersonAsk{Kind: "pick", Question: "A or B?"})
	RecordAskRelay("w4", "conductor-7", "go with A")
	RecordAskRelay("w4", "", "typed from a plain shell") // no sender: not a relay

	cases := []struct {
		prompt string
		person bool
	}{
		{"B, and rename it", true},
		{"typed from a plain shell", true},
		{"[agent-deck from:conductor-7]\nuse A", false},
		{"[INBOX] urgent · lead (947c): waiting", false},
		{"[HEARTBEAT] [ops] Status: 0 waiting", false},
		{"Stop hook feedback:\n[agent-deck inbox] 1 urgent", false},
		{"<task-notification>\n<task-id>x</task-id>", false},
		{"<system-reminder>ctx</system-reminder>", false},
		{"/compact", false},
		{"  go   with A ", false}, // a relayed send's text, whitespace aside
		{"", false},
	}
	for _, tc := range cases {
		if got := PromptIsFromPerson("w4", tc.prompt); got != tc.person {
			t.Errorf("%q: person = %v, want %v", tc.prompt, got, tc.person)
		}
	}

	if ClearAskOnPrompt("default", "w4", "[agent-deck from:conductor-7]\nuse A") {
		t.Fatal("a relayed send must not clear the ask")
	}
	if ClearAskOnPrompt("default", "w4", "go with A") {
		t.Fatal("an untagged relay must not clear the ask")
	}
	if !ClearAskOnPrompt("default", "w4", "B") {
		t.Fatal("the person's message clears the ask")
	}
	// With no open ask a relay is not even recorded.
	RecordAskRelay("w4", "conductor-7", "later")
	if askRelayMatches("w4", "later") {
		t.Fatal("relay recorded without an open ask")
	}
}

// The daemon raises the ask from a finished turn's marker, which is never
// relayed to the parent: in both parent-wake modes the parent's inbox holds
// at most the routine record, never a question, and no wake fires.
func TestAsks_MarkerTurnNeverEscalates(t *testing.T) {
	for _, mode := range []string{ParentWakesAll, ParentWakesEscalation} {
		t.Run(mode, func(t *testing.T) {
			f := newTurnTestFixture(t)
			useParentWakes(t, mode)
			captureAskFrames(t)
			f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "Two layouts are ready.\n===AGENTDECK_ASKS=== kind=pick item=ITEM-9 question=Ship A or B?"))
			f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil)
			a, open := LoadAsk(f.child.ID)
			if !open || a.Kind != "pick" || a.Item != "ITEM-9" || a.Source != AskSourceMarker {
				t.Fatalf("ask: %+v %v", a, open)
			}
			for _, rec := range f.inboxRecords(t) {
				if rec.Question || rec.Tier == TurnTierUrgent {
					t.Fatalf("the ask reached the parent as an escalation: %+v", rec)
				}
			}
			if *f.sends != 0 {
				t.Fatalf("parent woken %d times", *f.sends)
			}
		})
	}
}

// An open ask means the person owes the child an answer: no stall.
func TestAsks_OpenAskHoldsTheStallTimer(t *testing.T) {
	f := newTurnTestFixture(t)
	useParentWakes(t, ParentWakesEscalation)
	captureAskFrames(t)
	f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "===AGENTDECK_ASKS=== kind=approval question=Deploy?"))
	f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil)
	_, at := progressStamp(f.child, nil)
	f.d.sweepEscalations("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil, true, at.Add(3*DefaultEscalateAfter))
	if got := f.inboxRecords(t); len(got) != 0 || *f.sends != 0 {
		t.Fatalf("stalled while the person owes the answer: %+v", got)
	}
}

// The daemon raises one desktop notification per new ask, never a second
// one for the same ask, whatever the number of passes or daemons.
func TestAsks_DesktopNotificationDeduped(t *testing.T) {
	writeDesktopNotifyConfig(t, "desktop = true")
	captureAskFrames(t)
	_, _, _ = SetAsk("default", "w5", PersonAsk{Kind: "decision", Question: "Ship it?"})
	byID := map[string]*Instance{"w5": {ID: "w5", Title: "worker"}}
	rec := &recordingBackend{}
	for i := 0; i < 3; i++ {
		d := &TransitionDaemon{deskNotifier: desknotify.NewWithBackends(rec)}
		d.notifyAsks("default", byID)
		d.waitDesktopNotifications()
	}
	if len(rec.titles) != 1 || rec.titles[0] != "worker" || rec.bodies[0] != "asks you (decision): Ship it?" {
		t.Fatalf("notifications: %v %v", rec.titles, rec.bodies)
	}
}
