package main

import (
	"fmt"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
)

// Moving a Claude composer draft aside for a send.
//
// Text left in the input box (a person's unsent draft, or the prompt Claude
// Code puts back when a UserPromptSubmit hook blocks it) used to block every
// tmux-path send to that session: the #1409 guard held, then refused with
// composer_blocked, and a queued send retried behind the draft until its
// deadline. executeSend now takes a stable draft out of the box, sends,
// and types the draft back without Enter once the box is empty again, the
// save-clear-send-restore that `session context-verify` promises.
//
// The draft is never merged and never dropped silently:
//   - it is only taken when two reads draftStableWait apart show the same
//     complete text with no turn running and no dialog open (someone still
//     typing, a busy pane or an unreadable box keep the old hold-and-refuse);
//   - the erase is send.ClearClaudeComposer (Ctrl+U, verified row by row),
//     never Ctrl+C, which can exit Claude (#2104);
//   - it is typed back only into a box read empty, and confirmed there;
//     otherwise the send result carries it (saved_draft,
//     draft_restore_failed) and stderr prints it in full.
//
// Guarded sends (--require-input-prompt, the macOS app) keep refusing: their
// contract is that no key reaches a box holding text.

const (
	defaultDraftStableWait  = time.Second
	defaultDraftClearSettle = 150 * time.Millisecond
	defaultDraftRestoreWait = 3 * time.Second
	draftRestorePoll        = 100 * time.Millisecond
)

// movedDraft is a draft executeSend took out of the composer.
type movedDraft struct {
	content send.ComposerContent
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// moveDraftAside empties a Claude composer holding a stable, fully readable
// draft. It returns (nil, nil) when there is nothing it may move (empty box,
// text still changing, busy, dialog, unreadable, or a target without Ctrl+U):
// the caller's guard then decides exactly as before. A non-nil error means
// the clear started and did not finish: the box may hold part of the draft,
// nothing may be sent, and the returned draft must be reported.
func moveDraftAside(target sendRetryTarget, tun sendExecTuning) (*movedDraft, error) {
	clr, ok := target.(send.ComposerClearTarget)
	if !ok {
		return nil, nil
	}
	raw, err := target.CapturePaneFresh()
	if err != nil {
		return nil, nil
	}
	first := send.ReadClaudeComposer(raw)
	if first.State != send.ComposerStateText {
		return nil, nil
	}
	time.Sleep(durationOr(tun.draftStableWait, defaultDraftStableWait))
	res := send.ClearClaudeComposer(clr, send.ComposerClearOptions{
		Match: func(c send.ComposerContent) bool {
			return c.Text == first.Text && len(c.Rows) == len(first.Rows)
		},
		Settle: durationOr(tun.draftClearSettle, defaultDraftClearSettle),
	})
	switch {
	case res.Outcome == send.ClearOutcomeCleared:
		return &movedDraft{content: res.Before}, nil
	case res.Presses == 0:
		return nil, nil
	}
	return &movedDraft{content: first}, fmt.Errorf(
		"message not sent: the draft in the input box could not be moved aside cleanly (%s after %d Ctrl+U); the box may hold part of it",
		res.Reason, res.Presses)
}

// restoreMovedDraft types d back into the composer, without Enter, once the
// box reads empty, and confirms it is there. It reports whether it is back.
func restoreMovedDraft(target sendRetryTarget, d *movedDraft, tun sendExecTuning) bool {
	wait := durationOr(tun.draftRestoreWait, defaultDraftRestoreWait)
	if !awaitComposerState(target, wait, func(c send.ComposerContent) bool {
		return c.State == send.ComposerStateEmpty
	}) {
		// Our own message still in the box, a dialog, or an unreadable
		// pane: typing now would merge or land in the wrong place.
		return false
	}
	if err := target.SendKeysChunked(d.content.Text); err != nil {
		return false
	}
	return awaitComposerState(target, wait, func(c send.ComposerContent) bool {
		return send.ComposerTextMatches(c, d.content.Text)
	})
}

// awaitComposerState polls the composer (a running turn allowed: our own
// send may have started one) until ok holds or wait runs out.
func awaitComposerState(target sendRetryTarget, wait time.Duration, ok func(send.ComposerContent) bool) bool {
	deadline := time.Now().Add(wait)
	for {
		if raw, err := target.CapturePaneFresh(); err == nil && ok(send.ReadClaudeComposerDuringTurn(raw)) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(draftRestorePoll)
	}
}

// warnUnrestoredDraft prints a draft that was taken out of the input box and
// is not confirmed back in it, so it is never lost silently. --json callers
// also get it in saved_draft with draft_restore_failed.
func warnUnrestoredDraft(res sendDeliveryResult) {
	if res.draftSaved == "" || !res.draftRestoreFailed {
		return
	}
	fmt.Fprintf(os.Stderr,
		"Warning: the text in the input box was moved aside for this send and is not confirmed back in the box. It was:\n%s\n",
		res.draftSaved)
}
