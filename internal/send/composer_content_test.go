package send

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Captures for the composer content reader. The idle and busy frames are
// the real Claude 2.1.296 captures in testdata with the composer row
// replaced; the wrapped draft is a real Claude 2.1.287 capture.

const wrappedDraftText = `REAL-WRAP-BUSY-98306dfa62: Run exactly one Bash command: python3 -c "import time; time.sleep(240)". Set Bash timeout to300000 ms and do not run it in the background. Wait for it to finish. Then reply BUSY-DONE. For every later REAL-WRAP message reply only its marker and OK. Do not read other files or use any other tools.`

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// withComposer replaces the empty composer row ("❯ ") of a real
// capture with rows (the first after "❯ ", the rest indented two columns).
func withComposer(t *testing.T, fixture string, rows ...string) string {
	t.Helper()
	raw := readFixture(t, fixture)
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasSuffix(lines[i], "❯ ") || strings.HasSuffix(lines[i], "❯ \x1b[39m") {
			repl := []string{"\x1b[39m❯ " + rows[0]}
			for _, r := range rows[1:] {
				if r == "" {
					repl = append(repl, "")
					continue
				}
				repl = append(repl, "  "+r)
			}
			out := append(append(append([]string{}, lines[:i]...), repl...), lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	t.Fatalf("%s has no empty composer row", fixture)
	return ""
}

func TestReadClaudeComposerStates(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		state  string
		text   string
		reason string
	}{
		{"empty after /clear", readFixture(t, "after-clear-2.1.296.ansi"), ComposerStateEmpty, "", ""},
		{"ghost suggestion is empty", readFixture(t, "ghost-2.1.296.ansi"), ComposerStateEmpty, "", ""},
		{"single-line draft", withComposer(t, "after-clear-2.1.296.ansi", "held prompt: summarize the diff"), ComposerStateText, "held prompt: summarize the diff", ""},
		{"real wrapped draft", readFixture(t, "wrapped-draft-2.1.287.ansi"), ComposerStateText, wrappedDraftText, ""},
		{"multi-paragraph draft read in full", withComposer(t, "after-clear-2.1.296.ansi", "first paragraph", "", "second paragraph"), ComposerStateText, "first paragraph second paragraph", ""},
		{"busy turn", withComposer(t, "busy-tool-running-2.1.296.ansi", "typed while busy"), ComposerStateUnknown, "", ComposerCodeBusy},
		{"permission dialog", "⏺ Bash(make test)\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No, and tell Claude what to do differently\n\n Esc to cancel\n", ComposerStateUnknown, "", ComposerCodeDialog},
		{"no composer", "$ ls\nREADME.md\n$ ", ComposerStateUnknown, "", ComposerCodeNoPrompt},
		{"paste marker hides content", withComposer(t, "after-clear-2.1.296.ansi", "see [Pasted text #1 +40 lines]"), ComposerStateUnknown, "", ComposerReasonHiddenContent},
		{"foreign row inside the frame", strings.Replace(withComposer(t, "after-clear-2.1.296.ansi", "draft", "MENU ROW"), "  MENU ROW", "MENU ROW", 1), ComposerStateUnknown, "", ComposerReasonUnrecognized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadClaudeComposer(tc.raw)
			if got.State != tc.state || got.Text != tc.text || got.Reason != tc.reason {
				t.Fatalf("got %+v, want state=%s text=%q reason=%q", got, tc.state, tc.text, tc.reason)
			}
		})
	}
}

// ComposerDraft stops at the first blank row; the full read must not, or a
// draft whose first paragraph equals the expected text would be cleared.
func TestReadClaudeComposerDoesNotTruncateAtBlankRow(t *testing.T) {
	raw := withComposer(t, "after-clear-2.1.296.ansi", "renew now", "", "and keep this note")
	if d, _ := ComposerDraft(raw, tmux.StripANSI); d != "renew now" {
		t.Fatalf("fixture shape changed: ComposerDraft=%q", d)
	}
	c := ReadClaudeComposer(raw)
	if ComposerTextMatches(c, "renew now") {
		t.Fatalf("first paragraph matched the whole box: %+v", c)
	}
	if !ComposerTextMatches(c, "renew now\n\nand keep this note") {
		t.Fatalf("full text did not match: %+v", c)
	}
}

func TestReadClaudeComposerDuringTurnReadsBusyBox(t *testing.T) {
	c := ReadClaudeComposerDuringTurn(withComposer(t, "busy-tool-running-2.1.296.ansi", "typed while busy"))
	if c.State != ComposerStateText || c.Text != "typed while busy" {
		t.Fatalf("got %+v", c)
	}
}

func TestComposerTextMatches(t *testing.T) {
	wrapped := ReadClaudeComposer(readFixture(t, "wrapped-draft-2.1.287.ansi"))
	midWord := ComposerContent{State: ComposerStateText, Rows: []string{"supercalifragi", "listic and more"}}
	para := ComposerContent{State: ComposerStateText, Rows: []string{"one", "", "two"}}
	cases := []struct {
		name string
		c    ComposerContent
		want string
		ok   bool
	}{
		{"real wrap at a space", wrapped, wrappedDraftText, true},
		{"real wrap, expected with line breaks and extra spaces", wrapped, strings.Replace(wrappedDraftText, " Then ", "\n  Then\n", 1) + "\n", true},
		{"real wrap, one character differs", wrapped, strings.Replace(wrappedDraftText, "BUSY-DONE", "BUSY-DONF", 1), false},
		{"real wrap, expected is only the first row", wrapped, wrappedDraftText[:strings.Index(wrappedDraftText, " reply")], false},
		{"real wrap, expected longer than the box", wrapped, wrappedDraftText + " extra", false},
		{"hard wrap inside a word", midWord, "supercalifragilistic and more", true},
		{"hard wrap, expected with a space at the wrap", midWord, "supercalifragi listic and more", true},
		{"blank row needs whitespace", para, "onetwo", false},
		{"blank row with a line break", para, "one\n\ntwo", true},
		{"case matters", ComposerContent{State: ComposerStateText, Rows: []string{"Hello"}}, "hello", false},
		{"empty expected never matches", ComposerContent{State: ComposerStateText, Rows: []string{"x"}}, "  ", false},
		{"not text", ComposerContent{State: ComposerStateEmpty}, "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComposerTextMatches(tc.c, tc.want); got != tc.ok {
				t.Fatalf("ComposerTextMatches(%q, %q) = %v, want %v", tc.c.Rows, tc.want, got, tc.ok)
			}
		})
	}
}

// fakeBox is a Claude composer behind a real 2.1.296 frame. Ctrl+U deletes
// to the start of the current line, or the line break before it, as Claude
// does; hooks let a test model a person typing, a stuck key or a turn.
type fakeBox struct {
	t        *testing.T
	draft    string
	busy     bool
	presses  int
	onPress  func(b *fakeBox)
	keyErr   error
	ctrlUNop bool
	reads    int
	onRead   func(b *fakeBox)
}

func (b *fakeBox) CapturePaneFresh() (string, error) {
	b.reads++
	if b.onRead != nil {
		b.onRead(b)
	}
	fixture := "after-clear-2.1.296.ansi"
	if b.busy {
		fixture = "busy-tool-running-2.1.296.ansi"
	}
	if b.draft == "" {
		return readFixture(b.t, fixture), nil
	}
	return withComposer(b.t, fixture, strings.Split(b.draft, "\n")...), nil
}

func (b *fakeBox) SendCtrlU() error {
	if b.keyErr != nil {
		return b.keyErr
	}
	b.presses++
	if !b.ctrlUNop {
		if strings.HasSuffix(b.draft, "\n") {
			b.draft = b.draft[:len(b.draft)-1]
		} else {
			b.draft = b.draft[:strings.LastIndex(b.draft, "\n")+1]
		}
	}
	if b.onPress != nil {
		b.onPress(b)
	}
	return nil
}

func noSleep(time.Duration) {}

func expectMatch(want string) func(ComposerContent) bool {
	return func(c ComposerContent) bool { return ComposerTextMatches(c, want) }
}

func TestClearClaudeComposer(t *testing.T) {
	cases := []struct {
		name    string
		box     *fakeBox
		match   func(ComposerContent) bool
		outcome string
		reason  string
		presses int
		left    string
	}{
		{"equal: cleared", &fakeBox{draft: "held prompt"}, expectMatch("held prompt"), ClearOutcomeCleared, "", 1, ""},
		{"multi-line equal: cleared line by line", &fakeBox{draft: "line one\nline two\n\nline four"}, expectMatch("line one line two\n\nline four"), ClearOutcomeCleared, "", 6, ""},
		{"different: nothing typed", &fakeBox{draft: "a person's note"}, expectMatch("held prompt"), ClearOutcomeDiffers, "", 0, "a person's note"},
		{"empty: nothing typed", &fakeBox{}, expectMatch("held prompt"), ClearOutcomeAlreadyEmpty, "", 0, ""},
		{"busy: nothing typed", &fakeBox{draft: "held prompt", busy: true}, expectMatch("held prompt"), ClearOutcomeUnsafe, ComposerCodeBusy, 0, "held prompt"},
		{"any: clears other text", &fakeBox{draft: "a person's note"}, nil, ClearOutcomeCleared, "", 1, ""},
		{"text changing before the first key", &fakeBox{draft: "held", onRead: func(b *fakeBox) { b.draft += "x" }}, nil, ClearOutcomeUnsafe, ClearReasonChanging, 0, "heldxx"},
		{"someone types while clearing", &fakeBox{draft: "one\ntwo", onPress: func(b *fakeBox) { b.draft += "NEW" }}, nil, ClearOutcomeUnsafe, ClearReasonChanged, 1, "one\nNEW"},
		{"Ctrl+U removes nothing", &fakeBox{draft: "held prompt", ctrlUNop: true}, nil, ClearOutcomeUnsafe, ClearReasonNoProgress, 2, "held prompt"},
		{"turn starts while clearing", &fakeBox{draft: "one\ntwo", onPress: func(b *fakeBox) { b.busy = true }}, nil, ClearOutcomeUnsafe, ComposerCodeBusy, 1, "one\n"},
		{"key fails", &fakeBox{draft: "held prompt", keyErr: errors.New("tmux gone")}, nil, ClearOutcomeUnsafe, ClearReasonKeyFailed, 0, "held prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.box.t = t
			res := ClearClaudeComposer(tc.box, ComposerClearOptions{Match: tc.match, Sleep: noSleep})
			if res.Outcome != tc.outcome || res.Reason != tc.reason {
				t.Fatalf("outcome=%s reason=%s, want %s %s (result %+v)", res.Outcome, res.Reason, tc.outcome, tc.reason, res)
			}
			if tc.box.presses != tc.presses || res.Presses != tc.presses {
				t.Fatalf("pressed Ctrl+U %d time(s) (reported %d), want %d", tc.box.presses, res.Presses, tc.presses)
			}
			if tc.box.draft != tc.left {
				t.Fatalf("box holds %q, want %q", tc.box.draft, tc.left)
			}
			if res.Outcome == ClearOutcomeCleared && res.After.State != ComposerStateEmpty {
				t.Fatalf("cleared without an empty final read: %+v", res.After)
			}
		})
	}
}
