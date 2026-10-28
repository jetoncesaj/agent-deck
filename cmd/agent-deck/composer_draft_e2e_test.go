package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end tests for text left in a Claude input box and for /clear,
// on the delivery harness (real binary, isolated HOME/XDG/tmux, the
// Claude-like fake in the pane):
//
//   - a draft in the box no longer blocks `session send`: the draft is moved
//     aside (Ctrl+U, never Ctrl-C), the message is submitted once, and the
//     draft is typed back without Enter; before, every send was refused
//     with composer_blocked and a queued send waited behind the draft;
//   - `session send <id> /clear` is reported delivered when Claude's
//     SessionStart(source=clear) hook records the new conversation; before,
//     it exited 1 "send dropped silently" although the /clear ran;
//   - `session composer` reads the box, and `composer clear` empties it only
//     on an exact match.

// newDraftEnv is newDeliveryEnv with the project root resolved, so the
// path agent-deck derives the transcript from is the one the fake writes to
// (macOS temp dirs are symlinks), and turn tracking can confirm a send.
func newDraftEnv(t *testing.T) *deliveryEnv {
	t.Helper()
	e := newDeliveryEnv(t)
	root, err := filepath.EvalSymlinks(e.root)
	if err != nil {
		t.Fatal(err)
	}
	e.root = root
	return e
}

// fakeState is the fake's own record of its composer.
func (f *fakeTarget) fakeState(t *testing.T) (draft string, ctrlC int) {
	t.Helper()
	var st struct {
		Draft string `json:"draft"`
		CtrlC int    `json:"ctrl_c"`
	}
	b, _ := os.ReadFile(f.transcript + ".input-state.json")
	_ = json.Unmarshal(b, &st)
	return st.Draft, st.CtrlC
}

// typeDraft types text into the fake's composer the way a person would.
func (e *deliveryEnv) typeDraft(f *fakeTarget, text string) {
	e.t.Helper()
	keys := exec.Command("tmux", "send-keys", "-t", f.tmux, "-l", text)
	keys.Env = e.env
	if out, err := keys.CombinedOutput(); err != nil {
		e.t.Fatalf("type the draft: %v %s", err, out)
	}
	issue2530WaitFor(e.t, 5*time.Second, "the draft in the box", func() bool {
		d, _ := f.fakeState(e.t)
		return d == text
	})
}

// armClearHook makes the fake fire SessionStart(source=clear) through the
// real hook handler when it runs /clear, as Claude Code does.
func (e *deliveryEnv) armClearHook(f *fakeTarget) {
	e.t.Helper()
	f.phaseN++
	id := fmt.Sprintf("idle-hook-%d", f.phaseN)
	ctl, _ := json.Marshal(map[string]any{"id": id, "phase": "idle", "clear_hook": []string{e.bin, "-p", deliveryE2EProfile, "hook-handler"}})
	writeFile(e.t, f.transcript+".input-control.json", string(ctl), 0o600)
	issue2530WaitFor(e.t, 5*time.Second, "fake control "+id, func() bool {
		var st struct{ ID string }
		b, _ := os.ReadFile(f.transcript + ".input-state.json")
		return json.Unmarshal(b, &st) == nil && st.ID == id
	})
}

// rolloverID is the conversation the instance's rollover marker names.
func (e *deliveryEnv) rolloverID(f *fakeTarget) string {
	b, err := os.ReadFile(filepath.Join(e.root, "data", "agent-deck", "hooks", f.id+".rollover"))
	if err != nil {
		return ""
	}
	var m struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(b, &m)
	return m.SessionID
}

type sendReply struct {
	Delivery      string `json:"delivery"`
	SavedDraft    string `json:"saved_draft"`
	DraftRestored bool   `json:"draft_restored"`
}

func TestComposerE2E_ClearSendIsConfirmedBySessionStartHook(t *testing.T) {
	parallelSubprocessTest(t)
	e := newDraftEnv(t)
	f := e.startFake("clear-target", "idle", "")
	e.setPhase(f, "idle")
	e.armClearHook(f)
	before := e.rolloverID(f)

	start := time.Now()
	out, errOut, code := e.run("", nil, "session", "send", f.id, "/clear")
	if code != 0 {
		t.Fatalf("send /clear: exit %d after %s\n%s%s\nwant exit 0: the SessionStart(clear) hook recorded the new conversation (rollover %q -> %q)",
			code, time.Since(start).Round(time.Second), out, errOut, before, e.rolloverID(f))
	}
	if after := e.rolloverID(f); after == "" || after == before {
		t.Fatalf("the fake's /clear did not fire SessionStart(clear): rollover %q -> %q", before, after)
	}
}

func TestComposerE2E_DraftIsMovedAsideAndPutBack(t *testing.T) {
	parallelSubprocessTest(t)
	e := newDraftEnv(t)
	f := e.startFake("draft-target", "idle", "")
	e.setPhase(f, "idle")
	e.armClearHook(f)
	draft := deliveryToken("HELD_PROMPT") + " the prompt a hook put back"
	e.typeDraft(f, draft)

	// A message: delivered once, the draft back in the box, never submitted.
	msg := deliveryToken("ONTO_DRAFT")
	out, errOut, code := e.run("", nil, "session", "send", f.id, msg, "--no-wait", "--json")
	var res sendReply
	_ = json.Unmarshal([]byte(out), &res)
	if code != 0 || res.SavedDraft != draft || !res.DraftRestored {
		t.Fatalf("send onto a draft: exit %d %+v (%s %s); want delivered with the draft moved aside and restored", code, res, out, errOut)
	}
	issue2530WaitFor(t, 5*time.Second, "the message to reach the target", func() bool { return f.received(t, msg) == 1 })
	if got, ctrlC := f.fakeState(t); got != draft || ctrlC != 0 {
		t.Fatalf("box holds %q (Ctrl-C x%d) after the send, want the draft %q back and no Ctrl-C", got, ctrlC, draft)
	}

	// /clear: runs, is confirmed, and the draft comes back after it.
	before := e.rolloverID(f)
	out, errOut, code = e.run("", nil, "session", "send", f.id, "/clear", "--no-wait", "--json")
	res = sendReply{}
	_ = json.Unmarshal([]byte(out), &res)
	if code != 0 || !res.DraftRestored {
		t.Fatalf("send /clear onto a draft: exit %d %+v (%s %s)", code, res, out, errOut)
	}
	if after := e.rolloverID(f); after == "" || after == before {
		t.Fatalf("/clear did not run: rollover %q -> %q", before, after)
	}
	if got, _ := f.fakeState(t); got != draft {
		t.Fatalf("box holds %q after /clear, want the draft %q back", got, draft)
	}

	// A queued send lands once with the draft still there afterwards.
	queued := deliveryToken("QUEUED_ONTO_DRAFT")
	sendID := e.queue(f, queued)
	e.waitSettled(f, sendID, queued, 30*time.Second)
	issue2530WaitFor(t, 5*time.Second, "the draft back after the queued send", func() bool {
		d, _ := f.fakeState(t)
		return d == draft
	})
	if n := f.received(t, draft); n != 0 {
		t.Fatalf("the draft was submitted %d time(s); it must only ever be typed back", n)
	}
	if _, ctrlC := f.fakeState(t); ctrlC != 0 {
		t.Fatalf("Ctrl-C reached the pane %d time(s)", ctrlC)
	}
}

// A guarded send (--require-input-prompt, the macOS app) keeps its contract:
// no key reaches a box holding text.
func TestComposerE2E_GuardedSendStillRefusesOntoDraft(t *testing.T) {
	parallelSubprocessTest(t)
	e := newDraftEnv(t)
	f := e.startFake("guarded-target", "idle", "")
	e.setPhase(f, "idle")
	draft := deliveryToken("APP_DRAFT") + " typed in the app"
	e.typeDraft(f, draft)
	out, errOut, code := e.run("", nil, "session", "send", f.id, "must not land", "--no-wait", "--require-input-prompt", "--json")
	var res sendReply
	_ = json.Unmarshal([]byte(out), &res)
	if code == 0 || res.Delivery != deliveryComposerBlocked {
		t.Fatalf("guarded send onto a draft: exit %d %+v (%s %s); want composer_blocked", code, res, out, errOut)
	}
	if got, _ := f.fakeState(t); got != draft {
		t.Fatalf("box holds %q, want the untouched draft %q", got, draft)
	}
}

func TestComposerE2E_ComposerCommand(t *testing.T) {
	parallelSubprocessTest(t)
	e := newDraftEnv(t)
	f := e.startFake("composer-target", "idle", "")
	e.setPhase(f, "idle")

	read := func() composerReadResult {
		t.Helper()
		out, errOut, code := e.run("", nil, "session", "composer", f.id, "--json")
		var r composerReadResult
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("composer --json: exit %d %v\n%s%s", code, err, out, errOut)
		}
		return r
	}
	if r := read(); r.State != "empty" {
		t.Fatalf("empty box read as %+v", r)
	}

	draft := "line one of a held prompt"
	e.typeDraft(f, draft)
	if r := read(); r.State != "text" || r.Text != draft {
		t.Fatalf("draft read as %+v, want text %q", r, draft)
	}

	_, errOut, code := e.run("", nil, "session", "composer", "clear", f.id, "--expect", "some other text")
	if code != composerExitDiffers || !strings.Contains(errOut, draft) {
		t.Fatalf("clear --expect wrong: exit %d stderr %q; want exit 2 printing what is there", code, errOut)
	}
	if got, _ := f.fakeState(t); got != draft {
		t.Fatalf("box holds %q after a refused clear, want %q", got, draft)
	}

	expectFile := filepath.Join(e.root, "expected.txt")
	writeFile(t, expectFile, "  line one of a\nheld   prompt\n", 0o600)
	out, errOut, code := e.run("", nil, "session", "composer", "clear", f.id, "--expect-file", expectFile, "--json")
	var cr composerClearResult
	_ = json.Unmarshal([]byte(out), &cr)
	if code != 0 || cr.Outcome != "cleared" || cr.After.State != "empty" {
		t.Fatalf("clear --expect-file: exit %d %+v (%s %s)", code, cr, out, errOut)
	}
	if got, ctrlC := f.fakeState(t); got != "" || ctrlC != 0 {
		t.Fatalf("box holds %q (Ctrl-C x%d) after the clear, want empty and no Ctrl-C", got, ctrlC)
	}

	if _, _, code := e.run("", nil, "session", "composer", "clear", f.id, "--expect", "anything"); code != 0 {
		t.Fatalf("clear of an empty box: exit %d, want 0 (already empty)", code)
	}

	// A running turn: unknown, and clear refuses without a key.
	e.typeDraft(f, draft)
	e.setPhase(f, "busy")
	if r := read(); r.State != "unknown" || r.Reason != "agent_busy" {
		t.Fatalf("busy box read as %+v, want unknown agent_busy", r)
	}
	if _, _, code := e.run("", nil, "session", "composer", "clear", f.id, "--any"); code != composerExitUnsafe {
		t.Fatalf("clear --any while busy: exit %d, want 3", code)
	}
	if got, _ := f.fakeState(t); got != draft {
		t.Fatalf("box holds %q after a refused busy clear, want %q", got, draft)
	}

	if _, _, code := e.run("", nil, "session", "composer", "clear", f.id); code != composerExitError {
		t.Fatalf("clear without --expect/--any: exit %d, want 1", code)
	}
}
