package session

import (
	"strings"
	"testing"
	"time"
)

// [parent_wakes]: in "escalation" mode only a question to the parent, the
// completion sentinel, an error that outlived its grace and a stall reach
// the parent; "all" keeps the upstream behaviour. These tests drive every
// producer path in both modes against a real transcript and the real inbox,
// then every consumer path (prompt drain, Stop-hook drain, `inbox drain`,
// info digest) against what landed.

func useParentWakes(t *testing.T, mode string) {
	t.Helper()
	t.Cleanup(SetParentWakePolicyForTest(ParentWakePolicy{
		Mode: mode, EscalateAfter: DefaultEscalateAfter, ErrorGrace: DefaultErrorGrace,
	}))
}

func TestParentWakePolicyFor_Resolution(t *testing.T) {
	cfg := &UserConfig{
		ParentWakes: ParentWakesSettings{Mode: "all", EscalateAfter: "45m"},
		Profiles: map[string]ProfileSettings{
			"personal": {ParentWakes: ParentWakesSettings{Mode: "escalation"}},
		},
		Groups: map[string]GroupSettings{
			"roona":          {ParentWakes: ParentWakesSettings{ErrorGrace: "1m"}},
			"roona/epic-437": {ParentWakes: ParentWakesSettings{Mode: "all", EscalateAfter: "0"}},
			"health":         {ParentWakes: ParentWakesSettings{Mode: "ESCALATION", EscalateAfter: "nonsense", ErrorGrace: "-5m"}},
		},
	}
	cases := []struct {
		name, profile, group string
		want                 ParentWakePolicy
	}{
		{"global only", "work", "", ParentWakePolicy{ParentWakesAll, 45 * time.Minute, DefaultErrorGrace}},
		{"profile overrides global mode", "personal", "", ParentWakePolicy{ParentWakesEscalation, 45 * time.Minute, DefaultErrorGrace}},
		{"group sets one key, profile the other", "personal", "roona", ParentWakePolicy{ParentWakesEscalation, 45 * time.Minute, time.Minute}},
		{"nearest group wins, key by key", "personal", "roona/epic-437", ParentWakePolicy{ParentWakesAll, 0, time.Minute}},
		{"subgroup with no entry inherits", "personal", "roona/epic-512", ParentWakePolicy{ParentWakesEscalation, 45 * time.Minute, time.Minute}},
		{"bad values read as defaults", "work", "health", ParentWakePolicy{ParentWakesEscalation, DefaultEscalateAfter, DefaultErrorGrace}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.ParentWakePolicyFor(tc.profile, tc.group); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := (*UserConfig)(nil).ParentWakePolicyFor("x", "y"); got.Mode != ParentWakesAll || got.EscalateAfter != DefaultEscalateAfter {
		t.Fatalf("nil config: %+v", got)
	}
	if got := parseParentWakesDuration("off", time.Hour); got != 0 {
		t.Fatalf("off: %v", got)
	}
}

func TestParentWakes_EscalationKinds(t *testing.T) {
	cases := []struct {
		name string
		ev   TransitionNotificationEvent
		want string
	}{
		{"progress", TransitionNotificationEvent{ToStatus: "waiting", Tier: TurnTierInfo}, ""},
		{"error flip", TransitionNotificationEvent{ToStatus: "error", Tier: TurnTierUrgent}, ""},
		{"question", TransitionNotificationEvent{ToStatus: "waiting", Question: true}, EscalationNeed},
		{"done", TransitionNotificationEvent{Kind: transitionKindFinished, DoneStatus: "ok"}, EscalationDone},
		{"failed", TransitionNotificationEvent{Kind: transitionKindFinished, DoneStatus: "fail"}, EscalationDone},
		{"daemon error", TransitionNotificationEvent{Kind: transitionKindEscalation, Escalation: EscalationError}, EscalationError},
		{"daemon stall", TransitionNotificationEvent{Kind: transitionKindEscalation, Escalation: EscalationStalled}, EscalationStalled},
	}
	for _, tc := range cases {
		if got := ParentEscalation(tc.ev); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	esc := ParentWakePolicy{Mode: ParentWakesEscalation}
	all := ParentWakePolicy{Mode: ParentWakesAll}
	qs := []struct {
		name  string
		facts TurnFacts
		p     ParentWakePolicy
		want  bool
	}{
		{"all: person's question", TurnFacts{Question: true, Trigger: TurnTriggerHuman, Text: "A or B?"}, all, true},
		{"escalation: person's question stays with the person", TurnFacts{Question: true, Trigger: TurnTriggerHuman, Text: "A or B?"}, esc, false},
		{"escalation: explicit NEED in a person's turn", TurnFacts{Question: true, Trigger: TurnTriggerHuman, Text: "Blocked.\n**NEED:** a staging key"}, esc, true},
		{"escalation: question after the parent's send", TurnFacts{Question: true, Trigger: TurnTriggerSend, Text: "merge or hold?"}, esc, true},
		{"escalation: question after a background task", TurnFacts{Question: true, Trigger: TurnTriggerTask, Text: "retry?"}, esc, true},
		{"no question", TurnFacts{Trigger: TurnTriggerSend, Text: "ok"}, esc, false},
	}
	for _, tc := range qs {
		if got := questionEscalates(tc.facts, tc.p); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParentWakes_ProducerPaths drives one turn per case through the daemon's
// turn producer (emitTurn via recordTerminalTurns) in both modes and checks
// what reached the parent's inbox and whether it was woken.
func TestParentWakes_ProducerPaths(t *testing.T) {
	type want struct {
		records, wakes int
	}
	cases := []struct {
		name          string
		to            string
		lines         func() []string
		noQuestions   bool // [inbox] question_wakes = false
		all, escalate want
	}{
		{"progress after a background task", "waiting",
			func() []string {
				return []string{fxTaskNotification("u0"), fxAssistantText("a0", "Lane C merged; verifier running.")}
			},
			false, want{1, 0}, want{0, 0}},
		{"plain reply to the parent's send", "waiting",
			func() []string {
				return []string{fxHuman("u0", "[agent-deck from:parent-2469] status?"), fxAssistantText("a0", "Round 2 saved.")}
			},
			false, want{1, 0}, want{0, 0}},
		{"NEED after the parent's send", "waiting",
			func() []string {
				return []string{fxHuman("u0", "[agent-deck from:parent-2469] go"), fxAssistantText("a0", "Two options.\nNEED: merge or hold?")}
			},
			false, want{1, 1}, want{1, 1}},
		{"NEED with question_wakes off", "waiting",
			func() []string {
				return []string{fxHuman("u0", "[agent-deck from:parent-2469] go"), fxAssistantText("a0", "NEED: the prod key")}
			},
			true, want{1, 0}, want{1, 1}},
		{"question to the person who stepped in", "waiting",
			func() []string {
				return []string{fxHuman("u0", "rename it"), fxAssistantText("a0", "Rename to Foo or FooBar?")}
			},
			false, want{1, 1}, want{0, 0}},
		{"NEED in the person's turn", "waiting",
			func() []string {
				return []string{fxHuman("u0", "rename it"), fxAssistantText("a0", "NEED: conductor must free the slot")}
			},
			false, want{1, 1}, want{1, 1}},
		{"done sentinel", "waiting",
			func() []string {
				return []string{fxTaskNotification("u0"), fxAssistantText("a0", "===AGENTDECK_DONE=== status=ok summary=12 merged")}
			},
			false, want{1, 1}, want{1, 1}},
		{"failed sentinel", "waiting",
			func() []string {
				return []string{fxTaskNotification("u0"), fxAssistantText("a0", "===AGENTDECK_DONE=== status=fail summary=tests red")}
			},
			false, want{1, 1}, want{1, 1}},
		{"error flip", "error",
			func() []string {
				return []string{fxHuman("u0", "go"), fxAssistantText("a0", "API Error: 529 overloaded")}
			},
			false, want{1, 1}, want{0, 0}},
	}
	for _, tc := range cases {
		for _, mode := range []string{ParentWakesAll, ParentWakesEscalation} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				f := newTurnTestFixture(t)
				useParentWakes(t, mode)
				if tc.noQuestions {
					off := false
					inboxConfigOverride = &InboxConfig{QuestionWakes: &off}
				}
				f.appendTurn(t, tc.lines()...)
				f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: tc.to, f.parent.ID: "waiting"}, nil)
				w := tc.all
				if mode == ParentWakesEscalation {
					w = tc.escalate
				}
				if got := f.inboxRecords(t); len(got) != w.records {
					t.Fatalf("inbox records = %d, want %d: %+v", len(got), w.records, got)
				}
				if *f.sends != w.wakes {
					t.Fatalf("wakes = %d, want %d", *f.sends, w.wakes)
				}
				// Either way the turn is journaled: `session children` and
				// the stall check read it.
				if j, _ := ReadTurnJournal(f.child.ID, 0); len(j) != 1 {
					t.Fatalf("journal = %+v", j)
				}
				if mode == ParentWakesEscalation && w.records == 0 {
					if st, _ := ReadInboxStats(f.parent.ID); st.RoutineHeld != 1 {
						t.Fatalf("routine_held = %d, want 1", st.RoutineHeld)
					}
				}
			})
		}
	}
}

// TestParentWakes_ConsumerPaths: whatever the producer kept out of the inbox,
// no consumer path (prompt drain, Stop-hook drain, `inbox drain`, the info
// digest) can carry to the parent.
func TestParentWakes_ConsumerPaths(t *testing.T) {
	routine := func(f *turnTestFixture, t *testing.T) {
		f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "progress one"))
		f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil)
		f.appendTurn(t, fxTaskNotification("u1"), fxAssistantText("a1", "progress two"))
		f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil)
	}
	paths := []struct {
		name    string
		carries func(t *testing.T, parentID string) bool
	}{
		{"prompt drain", func(t *testing.T, id string) bool {
			text, _, err := DrainForPrompt(id)
			if err != nil {
				t.Fatal(err)
			}
			return strings.Contains(text, "progress two")
		}},
		{"stop-hook drain (blocks or leaves pending)", func(t *testing.T, id string) bool {
			_, blocked, err := DrainForStopHook(id, false)
			if err != nil {
				t.Fatal(err)
			}
			return blocked || InboxHasPending(id)
		}},
		{"inbox drain", func(t *testing.T, id string) bool {
			evs, _, err := DrainInboxForParentReport(id)
			if err != nil {
				t.Fatal(err)
			}
			return len(evs) > 0
		}},
		{"info digest", func(t *testing.T, id string) bool {
			due, _, _ := DigestDue(id, time.Minute, time.Now().Add(time.Hour))
			return due
		}},
	}
	for _, p := range paths {
		for _, mode := range []string{ParentWakesAll, ParentWakesEscalation} {
			t.Run(p.name+"/"+mode, func(t *testing.T) {
				f := newTurnTestFixture(t)
				useParentWakes(t, mode)
				routine(f, t)
				if got, want := p.carries(t, f.parent.ID), mode == ParentWakesAll; got != want {
					t.Fatalf("carries routine progress = %v, want %v", got, want)
				}
			})
		}
	}
}

// A reply to a tagged send from a session other than the parent still
// reaches that sender in escalation mode; the parent gets nothing.
func TestParentWakes_ReplyToThirdSenderStillDelivered(t *testing.T) {
	f := newTurnTestFixture(t)
	useParentWakes(t, ParentWakesEscalation)
	peer := NewInstanceWithTool("peer", f.child.ProjectPath, "claude")
	peer.ID = "peer-2469"
	storage, err := NewStorageWithProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if err := storage.SaveWithGroups([]*Instance{f.parent, f.child, peer}, nil); err != nil {
		t.Fatal(err)
	}
	f.byID[peer.ID] = peer
	f.appendTurn(t, fxHuman("u0", "[agent-deck from:peer-2469] which port?"), fxAssistantText("a0", "Port 8443."))
	f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil)
	if got := f.inboxRecords(t); len(got) != 0 {
		t.Fatalf("parent inbox: %+v", got)
	}
	got, _ := ReadInboxEvents(peer.ID)
	if len(got) != 1 || got[0].TargetKind != InboxTargetKindReply || got[0].Text != "Port 8443." {
		t.Fatalf("sender inbox: %+v", got)
	}
}

// TestParentWakes_DaemonSweep drives the daemon's escalation sweep with fake
// sessions: an error that recovers within its grace raises nothing, one that
// does not raises one record and one wake carrying the held text, a stall
// raises once, nothing is raised while an answer is owed, and a restarted
// daemon does not raise the same episode again.
func TestParentWakes_DaemonSweep(t *testing.T) {
	sweep := func(f *turnTestFixture, d *TransitionDaemon, status string, at time.Time, extra map[string]string) {
		statuses := map[string]string{f.child.ID: status, f.parent.ID: "waiting"}
		for k, v := range extra {
			statuses[k] = v
		}
		d.sweepEscalations("default", f.byID, statuses, nil, true, at)
	}
	stampAt := func(t *testing.T, f *turnTestFixture) time.Time {
		_, at := progressStamp(f.child, nil)
		if at.IsZero() {
			t.Fatal("no progress stamp from the transcript")
		}
		return at
	}

	t.Run("error recovered within grace", func(t *testing.T) {
		f := newTurnTestFixture(t)
		useParentWakes(t, ParentWakesEscalation)
		f.appendTurn(t, fxHuman("u0", "go"), fxAssistantText("a0", "API Error: 529 overloaded"))
		f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "error", f.parent.ID: "waiting"}, nil)
		t0 := stampAt(t, f)
		sweep(f, f.d, "error", t0, nil)
		sweep(f, f.d, "waiting", t0.Add(time.Minute), nil)
		sweep(f, f.d, "waiting", t0.Add(5*time.Minute), nil)
		if got := f.inboxRecords(t); len(got) != 0 || *f.sends != 0 {
			t.Fatalf("recovered error escalated: %+v sends=%d", got, *f.sends)
		}
		if f.d.notifier.heldErrorTextOf(f.child.ID) != "" {
			t.Fatal("held error text must be forgotten after recovery")
		}
	})

	t.Run("error past grace escalates once, also after a daemon restart", func(t *testing.T) {
		f := newTurnTestFixture(t)
		useParentWakes(t, ParentWakesEscalation)
		f.appendTurn(t, fxHuman("u0", "go"), fxAssistantText("a0", "API Error: 401 Please run /login"))
		f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "error", f.parent.ID: "waiting"}, nil)
		t0 := stampAt(t, f)
		sweep(f, f.d, "error", t0, nil)
		sweep(f, f.d, "error", t0.Add(DefaultErrorGrace-time.Second), nil)
		if *f.sends != 0 {
			t.Fatal("escalated inside the grace")
		}
		sweep(f, f.d, "error", t0.Add(DefaultErrorGrace), nil)
		sweep(f, f.d, "error", t0.Add(DefaultErrorGrace+time.Minute), nil)
		got := f.inboxRecords(t)
		if len(got) != 1 || *f.sends != 1 {
			t.Fatalf("want one record and one wake: %+v sends=%d", got, *f.sends)
		}
		rec := got[0]
		if rec.Kind != transitionKindEscalation || rec.Escalation != EscalationError || rec.Tier != TurnTierUrgent ||
			!strings.Contains(rec.Text, "did not recover") || !strings.Contains(rec.Text, "Please run /login") {
			t.Fatalf("escalation record: %+v", rec)
		}
		if h := NudgeHeadline(rec); !strings.Contains(h, "error (error)") {
			t.Fatalf("headline: %q", h)
		}

		// A restarted daemon re-raises the episode under the same identity:
		// still one record, no second wake.
		d2 := &TransitionDaemon{notifier: f.d.notifier}
		sweep(f, d2, "error", t0.Add(10*time.Minute), nil)
		sweep(f, d2, "error", t0.Add(10*time.Minute+DefaultErrorGrace), nil)
		if got := f.inboxRecords(t); len(got) != 1 || *f.sends != 1 {
			t.Fatalf("restart re-raised: %+v sends=%d", got, *f.sends)
		}
	})

	t.Run("stall escalates once", func(t *testing.T) {
		f := newTurnTestFixture(t)
		useParentWakes(t, ParentWakesEscalation)
		f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "Waiting for the build."))
		f.d.recordTerminalTurns("default", f.byID, map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}, nil)
		t0 := stampAt(t, f)
		sweep(f, f.d, "waiting", t0.Add(DefaultEscalateAfter-time.Minute), nil)
		if *f.sends != 0 {
			t.Fatal("stalled before escalate_after")
		}
		sweep(f, f.d, "waiting", t0.Add(DefaultEscalateAfter), nil)
		sweep(f, f.d, "waiting", t0.Add(2*DefaultEscalateAfter), nil)
		got := f.inboxRecords(t)
		if len(got) != 1 || got[0].Escalation != EscalationStalled || *f.sends != 1 {
			t.Fatalf("want one stall record and one wake: %+v sends=%d", got, *f.sends)
		}
	})

	owed := []struct {
		name  string
		setup func(t *testing.T, f *turnTestFixture) map[string]string
	}{
		{"its question is unanswered", func(t *testing.T, f *turnTestFixture) map[string]string {
			f.appendTurn(t, fxHuman("u0", "[agent-deck from:parent-2469] go"), fxAssistantText("a0", "NEED: which branch?"))
			return nil
		}},
		{"it reported done", func(t *testing.T, f *turnTestFixture) map[string]string {
			f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "===AGENTDECK_DONE=== status=ok summary=done"))
			return nil
		}},
		{"its own worker is running", func(t *testing.T, f *turnTestFixture) map[string]string {
			f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "Worker started."))
			w := NewInstanceWithTool("worker", f.child.ProjectPath, "claude")
			w.ID = "grandchild-2469"
			w.ParentSessionID = f.child.ID
			f.byID[w.ID] = w
			return map[string]string{w.ID: "running"}
		}},
	}
	for _, tc := range owed {
		t.Run("no stall while "+tc.name, func(t *testing.T) {
			f := newTurnTestFixture(t)
			useParentWakes(t, ParentWakesEscalation)
			extra := tc.setup(t, f)
			statuses := map[string]string{f.child.ID: "waiting", f.parent.ID: "waiting"}
			f.d.recordTerminalTurns("default", f.byID, statuses, nil)
			before := len(f.inboxRecords(t))
			wakes := *f.sends
			t0 := stampAt(t, f)
			sweep(f, f.d, "waiting", t0.Add(3*DefaultEscalateAfter), extra)
			if got := f.inboxRecords(t); len(got) != before || *f.sends != wakes {
				t.Fatalf("stall raised: %+v", got)
			}
		})
	}

	t.Run("escalate_after 0 and mode all raise nothing", func(t *testing.T) {
		for _, p := range []ParentWakePolicy{
			{Mode: ParentWakesEscalation, EscalateAfter: 0, ErrorGrace: DefaultErrorGrace},
			{Mode: ParentWakesAll, EscalateAfter: DefaultEscalateAfter, ErrorGrace: 0},
		} {
			f := newTurnTestFixture(t)
			restore := SetParentWakePolicyForTest(p)
			f.appendTurn(t, fxTaskNotification("u0"), fxAssistantText("a0", "Idle."))
			t0 := stampAt(t, f)
			sweep(f, f.d, "waiting", t0.Add(5*DefaultEscalateAfter), nil)
			sweep(f, f.d, "error", t0.Add(6*DefaultEscalateAfter), nil)
			sweep(f, f.d, "error", t0.Add(7*DefaultEscalateAfter), nil)
			restore()
			if p.Mode == ParentWakesEscalation {
				// error_grace still applies: the error escalates, the stall does not.
				if got := f.inboxRecords(t); len(got) != 1 || got[0].Escalation != EscalationError {
					t.Fatalf("%+v: %+v", p, got)
				}
				continue
			}
			if got := f.inboxRecords(t); len(got) != 0 || *f.sends != 0 {
				t.Fatalf("%+v raised: %+v", p, got)
			}
		}
	})
}
