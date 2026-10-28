package send

import (
	"strings"
	"time"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Composer content: what a Claude input box holds right now, read in full,
// compared against an expected text, and cleared with proof.
//
// ComposerDraft answers "is there operator text?" for the delivery guard and
// stops at the first blank row, which is enough to refuse a send. Clearing
// needs more: every row of the box, so a draft whose first paragraph equals
// the expected text is never taken for that text. ReadClaudeComposer is that
// full read; it builds on ComposerDraft (suggestion, placeholder and hint
// handling) and on the readiness checks ClaudeComposerReady uses (dialog,
// running turn), and reports unknown whenever any part of the box cannot be
// attributed to typed text.

// Composer states reported by ReadClaudeComposer.
const (
	ComposerStateEmpty   = "empty"   // nothing typed (a suggestion or idle hint may show)
	ComposerStateText    = "text"    // typed text, read in full
	ComposerStateUnknown = "unknown" // not safe to read; Reason says why
)

// Reasons for ComposerStateUnknown, beyond the ComposerCode* readiness codes
// (agent_busy, dialog_open, no_prompt), which keep their meaning.
const (
	ComposerReasonUnframed      = "unframed"       // a prompt glyph without the composer's borders around it
	ComposerReasonUnrecognized  = "unrecognized"   // a row inside the box that is not prompt text
	ComposerReasonHiddenContent = "hidden_content" // a [Pasted text …] or [Image …] marker stands for content not on screen
	ComposerReasonCaptureFailed = "capture_failed" // the pane could not be captured
)

// ComposerContent is one read of a Claude composer.
type ComposerContent struct {
	State string `json:"state"`
	// Text is the box's content with whitespace normalized: rows joined
	// by one space, runs of whitespace collapsed, ends trimmed
	// (NormalizePromptText). Empty unless State is text.
	Text string `json:"text,omitempty"`
	// Rows are the box's visual rows, prompt glyph and indent removed and
	// ends trimmed; "" is a blank row (a line break in the draft with
	// nothing after it on that line).
	Rows []string `json:"rows,omitempty"`
	// Reason is set for unknown: a ComposerCode* or ComposerReason* value.
	Reason string `json:"reason,omitempty"`
	// Ghost reports that an empty composer shows a suggestion or hint.
	Ghost bool `json:"ghost,omitempty"`
}

// ReadClaudeComposer reads the Claude composer in raw, a pane capture with
// ANSI attributes intact (CapturePaneFresh): the dim attribute is what tells
// a suggestion apart from typed text.
//
// It reports unknown, in this order, for a selection dialog, a running
// turn, a pane with no composer, a composer without both borders, a row
// inside the box that is neither the prompt row nor an indented
// continuation, and a paste or image marker (its content is not on screen).
func ReadClaudeComposer(raw string) ComposerContent {
	return readClaudeComposer(raw, false)
}

// ReadClaudeComposerDuringTurn is ReadClaudeComposer without the running-turn
// refusal, for a caller that only types into the box (no Enter) and must see
// it while the turn its own send started is running.
func ReadClaudeComposerDuringTurn(raw string) ComposerContent {
	return readClaudeComposer(raw, true)
}

func readClaudeComposer(raw string, busyOK bool) ComposerContent {
	content := tmux.StripANSI(raw)
	if DialogOpen(content) {
		return ComposerContent{State: ComposerStateUnknown, Reason: ComposerCodeDialog}
	}
	if !busyOK && turnRunning(content) {
		return ComposerContent{State: ComposerStateUnknown, Reason: ComposerCodeBusy}
	}
	draft, visible := ComposerDraft(raw, tmux.StripANSI)
	if !visible {
		return ComposerContent{State: ComposerStateUnknown, Reason: ComposerCodeNoPrompt}
	}
	if draft == "" {
		body, _ := CurrentComposerPrompt(content)
		return ComposerContent{State: ComposerStateEmpty, Ghost: body != ""}
	}
	rows, reason := claudeComposerRows(composerSearchLines(content))
	if reason != "" {
		return ComposerContent{State: ComposerStateUnknown, Reason: reason}
	}
	text := joinComposerRows(rows)
	if text == "" {
		return ComposerContent{State: ComposerStateEmpty}
	}
	if strings.Contains(text, "[Pasted text #") || strings.Contains(text, "[Image #") {
		return ComposerContent{State: ComposerStateUnknown, Reason: ComposerReasonHiddenContent}
	}
	return ComposerContent{State: ComposerStateText, Text: text, Rows: rows}
}

// claudeComposerRows returns every row between the composer's borders, or
// the reason they cannot all be read as typed text.
func claudeComposerRows(lines []string) ([]string, string) {
	top, bottom := ComposerBounds(lines)
	if top < 0 || top+1 >= bottom {
		return nil, ComposerReasonUnframed
	}
	var rows []string
	seenPrompt := false
	for _, line := range lines[top+1 : bottom] {
		line = strings.ReplaceAll(strings.TrimRight(line, " \t\r"), " ", " ")
		if !seenPrompt {
			trimmed := strings.TrimLeft(line, " ")
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "❯") {
				return nil, ComposerReasonUnrecognized
			}
			seenPrompt = true
			rows = append(rows, strings.TrimSpace(strings.TrimPrefix(trimmed, "❯")))
			continue
		}
		if strings.TrimSpace(line) == "" {
			rows = append(rows, "")
			continue
		}
		// Claude indents wrapped and continued lines two columns, under
		// the text after "❯ ". Anything else in the box is not input.
		if !strings.HasPrefix(line, "  ") {
			return nil, ComposerReasonUnrecognized
		}
		rows = append(rows, strings.TrimSpace(line))
	}
	if !seenPrompt {
		return nil, ComposerReasonUnframed
	}
	// Trailing blank rows stay: they are line breaks at the end of the
	// draft, which a clear must remove too (and count as progress).
	return rows, ""
}

func joinComposerRows(rows []string) string {
	return NormalizePromptText(strings.Join(rows, " "))
}

// ComposerTextMatches reports whether a composer read holds exactly
// expected, under the comparison rule documented for
// `agent-deck session composer clear --expect`:
//
//   - Whitespace is normalized on both sides: NBSP is a space, every run
//     of spaces, tabs and line breaks is one space, and the ends are
//     trimmed. "a  b\n c" and "a b c" are the same text.
//   - The box is compared row by row. A wrapped row may end at a space
//     (Claude wraps at word boundaries and drops that space) or inside a
//     word too long for the row, so between two consecutive rows the
//     expected text may hold one space or nothing.
//   - A blank row in the box is a blank line in the draft, so the expected
//     text must hold whitespace at that point.
//   - Everything else, case and punctuation included, must be identical.
//
// The only difference the rule cannot see is a line break typed inside a
// word ("foo⏎bar" against "foobar").
func ComposerTextMatches(c ComposerContent, expected string) bool {
	if c.State != ComposerStateText {
		return false
	}
	return rowsMatch(c.Rows, NormalizePromptText(expected))
}

func rowsMatch(rows []string, want string) bool {
	if want == "" {
		return false
	}
	pos := 0
	first := true
	hardBreak := false
	for _, row := range rows {
		row = NormalizePromptText(row)
		if row == "" {
			hardBreak = true
			continue
		}
		if !first {
			if pos < len(want) && want[pos] == ' ' {
				pos++
			} else if hardBreak {
				return false
			}
		}
		if !strings.HasPrefix(want[pos:], row) {
			return false
		}
		pos += len(row)
		first, hardBreak = false, false
	}
	return !first && pos == len(want)
}

// ComposerClearTarget is the pane surface ClearClaudeComposer needs.
// *tmux.Session satisfies it.
type ComposerClearTarget interface {
	CapturePaneFresh() (string, error)
	SendCtrlU() error
}

// Clear outcomes reported by ClearClaudeComposer.
const (
	ClearOutcomeCleared      = "cleared"       // the box held matching text and is now verified empty
	ClearOutcomeAlreadyEmpty = "already_empty" // nothing to clear; no key was sent
	ClearOutcomeDiffers      = "differs"       // the box holds other text; no key was sent
	ClearOutcomeUnsafe       = "unsafe"        // the box could not be read safely, or changed; see Reason
)

// Reasons for ClearOutcomeUnsafe that are not composer read reasons.
const (
	ClearReasonChanging   = "changing"    // the box changed between two reads before any key: someone is typing
	ClearReasonChanged    = "changed"     // text that was not in the box appeared while clearing
	ClearReasonNoProgress = "no_progress" // Ctrl+U stopped removing text
	ClearReasonIncomplete = "incomplete"  // the press budget ran out with text left
	ClearReasonKeyFailed  = "key_failed"  // the Ctrl+U keystroke could not be sent
)

// ComposerClearOptions tunes ClearClaudeComposer.
type ComposerClearOptions struct {
	// Match decides whether the box's content may be cleared. nil clears
	// whatever text the box holds (`--any`).
	Match func(ComposerContent) bool
	// Settle is the wait after each keystroke and between the two reads
	// that must agree before the first keystroke. Defaults to 150ms.
	Settle time.Duration
	// Sleep replaces time.Sleep in tests.
	Sleep func(time.Duration)
}

// ComposerClearResult reports what ClearClaudeComposer saw and did.
type ComposerClearResult struct {
	Outcome string          `json:"outcome"`
	Reason  string          `json:"reason,omitempty"`
	Before  ComposerContent `json:"before"`
	After   ComposerContent `json:"after"`
	Presses int             `json:"presses"`
}

// ClearClaudeComposer empties a Claude composer that holds text Match
// accepts, and proves it is empty afterwards.
//
// No key is sent unless two reads one Settle apart show the same text,
// with no turn running and no dialog open. The keystroke is Ctrl+U, the
// erase `session command` already uses: it deletes back to the start of the
// line (at a line start, the line break before it) and never interrupts a
// turn or exits Claude, unlike Ctrl+C. After every press the box is read
// again: it must hold only what is left of the text being cleared (each
// remaining character in the original order), and less of it. Text that was
// not there before, a turn, a dialog or an unreadable frame stops the clear
// at once and is reported unsafe with what the box now holds. Success needs
// two empty reads one Settle apart.
func ClearClaudeComposer(t ComposerClearTarget, opts ComposerClearOptions) ComposerClearResult {
	settle := opts.Settle
	if settle <= 0 {
		settle = 150 * time.Millisecond
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	read := func() ComposerContent {
		raw, err := t.CapturePaneFresh()
		if err != nil {
			return ComposerContent{State: ComposerStateUnknown, Reason: ComposerReasonCaptureFailed}
		}
		return ReadClaudeComposer(raw)
	}

	res := ComposerClearResult{Before: read()}
	res.After = res.Before
	switch res.Before.State {
	case ComposerStateEmpty:
		res.Outcome = ClearOutcomeAlreadyEmpty
		return res
	case ComposerStateUnknown:
		res.Outcome, res.Reason = ClearOutcomeUnsafe, res.Before.Reason
		return res
	}
	if opts.Match != nil && !opts.Match(res.Before) {
		res.Outcome = ClearOutcomeDiffers
		return res
	}

	sleep(settle)
	again := read()
	res.After = again
	switch {
	case again.State == ComposerStateEmpty:
		res.Outcome = ClearOutcomeAlreadyEmpty
		return res
	case again.State == ComposerStateUnknown:
		res.Outcome, res.Reason = ClearOutcomeUnsafe, again.Reason
		return res
	case !sameRows(again.Rows, res.Before.Rows):
		res.Outcome, res.Reason = ClearOutcomeUnsafe, ClearReasonChanging
		return res
	}

	prev := again
	stalls := 0
	budget := 2*len(res.Before.Rows) + 2
	for res.Presses < budget {
		if err := t.SendCtrlU(); err != nil {
			res.Outcome, res.Reason = ClearOutcomeUnsafe, ClearReasonKeyFailed
			return res
		}
		res.Presses++
		sleep(settle)
		now := read()
		res.After = now
		switch now.State {
		case ComposerStateUnknown:
			res.Outcome, res.Reason = ClearOutcomeUnsafe, now.Reason
			return res
		case ComposerStateEmpty:
			sleep(settle)
			confirm := read()
			res.After = confirm
			if confirm.State == ComposerStateEmpty {
				res.Outcome = ClearOutcomeCleared
				return res
			}
			if confirm.State == ComposerStateUnknown {
				res.Outcome, res.Reason = ClearOutcomeUnsafe, confirm.Reason
				return res
			}
			now = confirm
		}
		if !isRuneSubsequence(compactSpace(now.Text), compactSpace(res.Before.Text)) {
			res.Outcome, res.Reason = ClearOutcomeUnsafe, ClearReasonChanged
			return res
		}
		if len(compactSpace(now.Text)) >= len(compactSpace(prev.Text)) && len(now.Rows) >= len(prev.Rows) {
			// A frame may lag one keystroke; a second press that still
			// removes nothing means Ctrl+U cannot reach the rest.
			stalls++
			if stalls >= 2 {
				res.Outcome, res.Reason = ClearOutcomeUnsafe, ClearReasonNoProgress
				return res
			}
		} else {
			stalls = 0
		}
		prev = now
	}
	res.Outcome, res.Reason = ClearOutcomeUnsafe, ClearReasonIncomplete
	return res
}

func sameRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compactSpace drops every whitespace rune: wrapping moves spaces around,
// so progress and provenance are judged on the other characters.
func compactSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// isRuneSubsequence reports whether every rune of sub appears in s, in order.
func isRuneSubsequence(sub, s string) bool {
	rs := []rune(s)
	i := 0
	for _, r := range sub {
		for i < len(rs) && rs[i] != r {
			i++
		}
		if i == len(rs) {
			return false
		}
		i++
	}
	return true
}
