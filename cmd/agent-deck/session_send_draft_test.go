package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// draftBox is a Claude pane whose composer behaves like Claude's for the
// keys a send uses: typed text lands in the box, Enter submits and empties
// it, Ctrl+U deletes to the start of the line (or the line break before it),
// Ctrl-C wipes it. It counts every key so a test can prove what reached it.
type draftBox struct {
	mu        sync.Mutex
	box       string
	busy      bool
	submitted []string
	ctrlC     int
	ctrlU     int
	// swallowEnter models a TUI that drops Enter: the message stays typed.
	swallowEnter bool
	// onCapture runs before every capture (someone typing, a turn starting).
	onCapture func(b *draftBox)
}

func (b *draftBox) frame() string {
	div := strings.Repeat("─", 80)
	head := "⏺ All four steps ran.\n\n✻ Sautéed for 45s · done 10:44 AM\n\n"
	if b.busy {
		head = busyClaudeHead
	}
	rows := strings.Split(b.box, "\n")
	body := "❯ " + rows[0]
	for _, r := range rows[1:] {
		body += "\n  " + r
	}
	return head + div + "\n" + body + "\n" + div + "\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
}

func (b *draftBox) CapturePaneFresh() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.onCapture != nil {
		b.onCapture(b)
	}
	return b.frame(), nil
}

func (b *draftBox) GetStatus() (string, error) { return "waiting", nil }

func (b *draftBox) type_(s string) { b.box += s }

func (b *draftBox) enter() {
	if b.swallowEnter {
		return
	}
	if strings.TrimSpace(b.box) != "" {
		b.submitted = append(b.submitted, b.box)
	}
	b.box = ""
}

func (b *draftBox) SendKeysAndEnter(keys string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.type_(keys)
	b.enter()
	return nil
}

func (b *draftBox) SendKeysAndEnterChecked(keys string, capture func() (string, error), check tmux.PostPasteCheck) error {
	b.mu.Lock()
	b.type_(keys)
	b.mu.Unlock()
	if check != nil {
		if ok, err := check(capture()); !ok {
			return err
		}
	}
	return b.SendEnter()
}

func (b *draftBox) SendEnter() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enter()
	return nil
}

func (b *draftBox) SendCtrlC() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctrlC++
	b.box = ""
	return nil
}

func (b *draftBox) SendCtrlU() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctrlU++
	if strings.HasSuffix(b.box, "\n") {
		b.box = b.box[:len(b.box)-1]
	} else {
		b.box = b.box[:strings.LastIndex(b.box, "\n")+1]
	}
	return nil
}

func (b *draftBox) SendKeysChunked(s string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.type_(s)
	return nil
}

func (b *draftBox) gotSubmitted(msg string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.submitted {
		if s == msg {
			return true
		}
	}
	return false
}

// draftTuning is a fast executeSend tuning whose verification loop ends on
// the message's own submission (turn advancement), as on a session whose
// transcript is tracked.
func draftTuning(b *draftBox, msg string) sendExecTuning {
	tun := testGuardTuning(sendRetryOptions{maxRetries: 5, checkDelay: time.Millisecond, verifyDelivery: true})
	tun.retry.turnAdvanced = func() bool { return b.gotSubmitted(msg) }
	tun.draftStableWait, tun.draftClearSettle, tun.draftRestoreWait = time.Millisecond, time.Millisecond, 100*time.Millisecond
	return tun
}

func TestExecuteSendMovesDraftAsideAndPutsItBack(t *testing.T) {
	const draft = "HELD PROMPT: summarize the open PRs"
	for _, msg := range []string{"hello", "/clear"} {
		t.Run(msg, func(t *testing.T) {
			b := &draftBox{box: draft}
			res, err := executeSend(b, "claude", msg, false, draftTuning(b, msg))
			if err != nil || res.delivery != deliverySubmitted {
				t.Fatalf("delivery=%q err=%v, want submitted", res.delivery, err)
			}
			if len(b.submitted) != 1 || b.submitted[0] != msg {
				t.Fatalf("submitted %q, want only %q (never merged with the draft)", b.submitted, msg)
			}
			if b.box != draft {
				t.Fatalf("box holds %q after the send, want the draft %q back", b.box, draft)
			}
			if b.ctrlC != 0 || b.ctrlU == 0 {
				t.Fatalf("Ctrl-C x%d, Ctrl+U x%d; want the draft erased with Ctrl+U only", b.ctrlC, b.ctrlU)
			}
			if res.draftSaved != draft || !res.draftCleared || !res.draftRestored || res.draftRestoreFailed {
				t.Fatalf("result draft fields %+v", res)
			}
		})
	}
}

func TestExecuteSendMovesMultiLineDraftAndKeepsItsText(t *testing.T) {
	const draft = "first line of the note\nsecond line\n\nlast paragraph"
	b := &draftBox{box: draft}
	res, err := executeSend(b, "claude", "hello", false, draftTuning(b, "hello"))
	if err != nil || res.delivery != deliverySubmitted || len(b.submitted) != 1 || b.submitted[0] != "hello" {
		t.Fatalf("delivery=%q err=%v submitted=%q", res.delivery, err, b.submitted)
	}
	back := send.ReadClaudeComposer(b.frame())
	if !send.ComposerTextMatches(back, draft) || !res.draftRestored {
		t.Fatalf("box holds %q after the send, want the draft's text back (restored=%v)", b.box, res.draftRestored)
	}
}

// The message stays in the box (Enter dropped): typing the draft back would
// merge it into our message, so it is reported instead.
func TestExecuteSendNeverTypesDraftOntoAStuckMessage(t *testing.T) {
	const draft = "a person's half-written note"
	b := &draftBox{box: draft, swallowEnter: true}
	res, _ := executeSend(b, "claude", "hello", false, draftTuning(b, "hello"))
	if b.box != "hello" {
		t.Fatalf("box holds %q, want only the stuck message (no merge)", b.box)
	}
	if res.draftSaved != draft || res.draftRestored || !res.draftRestoreFailed {
		t.Fatalf("result %+v: the draft must be reported for recovery", res)
	}
	if f := res.jsonFields(); f["saved_draft"] != draft || f["draft_restore_failed"] != true {
		t.Fatalf("--json fields %v do not carry the draft", f)
	}
}

func TestExecuteSendLeavesDraftAloneWhenItCannotMoveIt(t *testing.T) {
	cases := []struct {
		name string
		box  *draftBox
		tun  func(sendExecTuning) sendExecTuning
	}{
		{"someone is typing", &draftBox{box: "typing", onCapture: func(b *draftBox) { b.box += "." }}, nil},
		{"a turn is running", &draftBox{box: "queued thought", busy: true}, nil},
		{"guarded send (--require-input-prompt)", &draftBox{box: "app draft"}, func(t sendExecTuning) sendExecTuning { t.requireInputPrompt = true; return t }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tun := draftTuning(tc.box, "hello")
			if tc.tun != nil {
				tun = tc.tun(tun)
			}
			before := tc.box.box
			res, err := executeSend(tc.box, "claude", "hello", false, tun)
			if err == nil || res.delivery != deliveryComposerBlocked {
				t.Fatalf("delivery=%q err=%v, want composer_blocked", res.delivery, err)
			}
			if tc.box.ctrlU != 0 || tc.box.ctrlC != 0 || len(tc.box.submitted) != 0 {
				t.Fatalf("keys reached the box: Ctrl+U x%d Ctrl-C x%d submitted %q", tc.box.ctrlU, tc.box.ctrlC, tc.box.submitted)
			}
			if tc.name != "someone is typing" && tc.box.box != before {
				t.Fatalf("box holds %q, want %q untouched", tc.box.box, before)
			}
		})
	}
}

// Codex keeps the hold-and-refuse guard: its composer reader is not safe
// enough to erase from.
func TestExecuteSendDoesNotMoveCodexDraft(t *testing.T) {
	b := &draftBox{box: "codex draft"}
	_, _ = executeSend(b, "codex", "hello", false, draftTuning(b, "hello"))
	if b.ctrlU != 0 || b.ctrlC != 0 {
		t.Fatalf("keys erased a Codex draft: Ctrl+U x%d Ctrl-C x%d", b.ctrlU, b.ctrlC)
	}
}

func TestIsClaudeClearCommand(t *testing.T) {
	for msg, want := range map[string]bool{"/clear": true, "  /clear  ": true, "/reset": true, "/new": true, "/CLEAR": true,
		"/clearance": false, "/compact": false, "clear": false, "": false, "please /clear": false} {
		if got := isClaudeClearCommand(msg); got != want {
			t.Errorf("isClaudeClearCommand(%q) = %v, want %v", msg, got, want)
		}
	}
}
