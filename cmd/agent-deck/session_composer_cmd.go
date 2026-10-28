package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// `agent-deck session composer`: read, and clear on an exact match, the text
// sitting in a session's input box. Agent Deck owns the pane, so a caller that
// needs the box empty (a hook about to send /clear, a script whose earlier
// draft was put back) asks for it here instead of sending raw keys.

// Exit codes of `session composer` and `session composer clear`.
const (
	composerExitOK      = 0 // read; or cleared, or already empty
	composerExitError   = 1 // usage, session not found or not running
	composerExitDiffers = 2 // clear: the box holds other text; nothing was typed
	composerExitUnsafe  = 3 // unknown or unsafe: busy, dialog, unreadable, unsupported tool, changed while clearing
)

// composerPane is the pane surface the composer commands use.
// *tmux.Session satisfies it.
type composerPane interface {
	CapturePaneFresh() (string, error)
	SendCtrlU() error
}

type composerReadResult struct {
	Session string `json:"session"`
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	send.ComposerContent
}

type composerClearResult struct {
	Session string `json:"session"`
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Mode    string `json:"mode"` // expect or any
	OK      bool   `json:"ok"`
	send.ComposerClearResult
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
}

func printComposerHelp() {
	fmt.Println("Usage: agent-deck session composer [<id|title|current>] [--json]")
	fmt.Println("       agent-deck session composer clear [<id|title|current>] (--expect TEXT | --expect-file FILE | --any) [--json]")
	fmt.Println()
	fmt.Println("Read what sits in a session's input box now: empty, text, or unknown when it")
	fmt.Println("cannot be read safely (a turn running, a menu or dialog open, a paste marker")
	fmt.Println("hiding its content, no composer on screen, or a tool without a reader).")
	fmt.Println()
	fmt.Println("clear empties the box only when it holds exactly the expected text. It types")
	fmt.Println("nothing when the box holds anything else, a turn is running or the box cannot")
	fmt.Println("be read, and it reports success only after reading the box empty.")
	fmt.Println()
	fmt.Println("Comparison: whitespace is normalized on both sides (every run of spaces, tabs")
	fmt.Println("and line breaks is one space, ends trimmed); where the box wraps a row the")
	fmt.Println("expected text may have a space or nothing; a blank row in the box needs")
	fmt.Println("whitespace in the expected text. Everything else must be identical.")
	fmt.Println()
	fmt.Println("  --expect TEXT       clear only if the box holds TEXT")
	fmt.Println("  --expect-file FILE  clear only if the box holds the content of FILE")
	fmt.Println("  --any               DANGER: clear WHATEVER is in the box. This destroys a")
	fmt.Println("                      person's unsent draft with no copy kept. Use it only when")
	fmt.Println("                      you know nobody is typing in that session.")
	fmt.Println("  --json              print the result as JSON")
	fmt.Println()
	fmt.Println("Exit codes: 0 read, cleared or already empty; 2 the box holds other text")
	fmt.Println("(printed, nothing typed); 3 unknown or not safe (busy, dialog, unreadable,")
	fmt.Println("unsupported tool, or the box changed while clearing); 1 any other error.")
	fmt.Println()
	fmt.Println("Claude sessions only. Codex, Pi and other tools report \"unsupported for <tool>\".")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  agent-deck session composer worker-3 --json")
	fmt.Println("  agent-deck session composer clear worker-3 --expect-file held-prompt.txt")
}

func handleSessionComposer(profile string, args []string) {
	if len(args) > 0 && args[0] == "clear" {
		handleSessionComposerClear(profile, args[1:])
		return
	}
	fs := flag.NewFlagSet("session composer", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = printComposerHelp
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(composerExitError)
	}
	out := NewCLIOutput(*jsonOutput, false)
	if fs.NArg() > 1 {
		fs.Usage()
		out.Error("expected at most one session", ErrCodeInvalidOperation)
		exitCLI(composerExitError)
	}
	inst, pane := resolveComposerSession(out, profile, fs.Arg(0))
	res := composerReadResult{Session: inst.Title, ID: inst.ID, Tool: inst.Tool}
	res.ComposerContent = readSessionComposer(inst.Tool, pane)
	if *jsonOutput {
		out.printJSON(res)
	} else {
		switch res.State {
		case send.ComposerStateEmpty:
			fmt.Printf("%s: empty\n", inst.Title)
		case send.ComposerStateText:
			fmt.Printf("%s: text\n%s\n", inst.Title, res.Text)
		default:
			fmt.Printf("%s: unknown (%s)\n", inst.Title, res.Reason)
		}
	}
	if res.State == send.ComposerStateUnknown {
		exitCLI(composerExitUnsafe)
	}
}

func handleSessionComposerClear(profile string, args []string) {
	fs := flag.NewFlagSet("session composer clear", flag.ContinueOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	expect := fs.String("expect", "", "Clear only if the box holds this text")
	expectFile := fs.String("expect-file", "", "Clear only if the box holds the content of this file")
	anyText := fs.Bool("any", false, "DANGER: clear whatever is in the box, a person's draft included")
	fs.Usage = printComposerHelp
	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(composerExitError)
	}
	out := NewCLIOutput(*jsonOutput, false)
	modes := 0
	for _, set := range []bool{*expect != "", *expectFile != "", *anyText} {
		if set {
			modes++
		}
	}
	if modes != 1 || fs.NArg() > 1 {
		fs.Usage()
		out.Error("give exactly one of --expect TEXT, --expect-file FILE or --any, and at most one session", ErrCodeInvalidOperation)
		exitCLI(composerExitError)
	}
	want, mode := *expect, "expect"
	if *expectFile != "" {
		data, err := os.ReadFile(*expectFile)
		if err != nil {
			out.Error(fmt.Sprintf("cannot read --expect-file: %v", err), ErrCodeInvalidOperation)
			exitCLI(composerExitError)
		}
		want = string(data)
	}
	if *anyText {
		mode = "any"
	} else if send.NormalizePromptText(want) == "" {
		out.Error("the expected text is empty; an empty box needs no clear", ErrCodeInvalidOperation)
		exitCLI(composerExitError)
	}

	inst, pane := resolveComposerSession(out, profile, fs.Arg(0))
	res := clearSessionComposer(inst, pane, mode, want, 0)
	if *jsonOutput {
		out.printJSON(res)
	} else {
		printComposerClear(res)
	}
	if res.ExitCode != composerExitOK {
		exitCLI(res.ExitCode)
	}
}

// resolveComposerSession resolves ref ("" or "current" is the session owning
// this tmux pane) to a running session with a pane, or exits 1.
func resolveComposerSession(out *CLIOutput, profile, ref string) (*session.Instance, composerPane) {
	if ref == "current" {
		ref = ""
	}
	_, _, _, inst, _, msg, code := resolveCommandTarget(profile, ref)
	if inst == nil {
		out.Error(msg, code)
		exitCLI(composerExitError)
	}
	if !inst.Exists() {
		out.Error(fmt.Sprintf("session '%s' is not running", inst.Title), ErrCodeInvalidOperation)
		exitCLI(composerExitError)
	}
	tmuxSess := inst.GetTmuxSession()
	if tmuxSess == nil {
		out.Error(fmt.Sprintf("session '%s' has no tmux pane", inst.Title), ErrCodeInvalidOperation)
		exitCLI(composerExitError)
	}
	if tmuxSess.VimMode {
		return inst, vimComposerPane{tmuxSess}
	}
	return inst, tmuxSess
}

// vimComposerPane marks a Claude vim-mode composer: it can be read, but Ctrl+U
// in normal mode is not an erase, so clearing refuses.
type vimComposerPane struct{ *tmux.Session }

// readSessionComposer reads the box of a session running tool.
func readSessionComposer(tool string, pane composerPane) send.ComposerContent {
	if !session.IsClaudeCompatible(tool) {
		return send.ComposerContent{State: send.ComposerStateUnknown, Reason: "unsupported for " + tool}
	}
	raw, err := pane.CapturePaneFresh()
	if err != nil {
		return send.ComposerContent{State: send.ComposerStateUnknown, Reason: send.ComposerReasonCaptureFailed}
	}
	return send.ReadClaudeComposer(raw)
}

// clearSessionComposer runs the clear for one session and maps its outcome to
// the result and exit code. mode is "expect" (want is the expected text) or
// "any". settle 0 uses ClearClaudeComposer's default.
func clearSessionComposer(inst *session.Instance, pane composerPane, mode, want string, settle time.Duration) composerClearResult {
	res := composerClearResult{Session: inst.Title, ID: inst.ID, Tool: inst.Tool, Mode: mode}
	if !session.IsClaudeCompatible(inst.Tool) {
		res.Outcome, res.Reason = send.ClearOutcomeUnsafe, "unsupported for "+inst.Tool
		res.Error = fmt.Sprintf("session '%s' runs %s: unsupported for %s", inst.Title, inst.Tool, inst.Tool)
		res.ExitCode = composerExitUnsafe
		return res
	}
	if _, vim := pane.(vimComposerPane); vim {
		res.Before = readSessionComposer(inst.Tool, pane)
		res.After = res.Before
		res.Outcome, res.Reason = send.ClearOutcomeUnsafe, "unsupported_vim_mode"
		res.Error = "the session uses the Claude vim composer, where Ctrl+U is not an erase; nothing typed"
		res.ExitCode = composerExitUnsafe
		return res
	}
	opts := send.ComposerClearOptions{Settle: settle}
	if mode == "expect" {
		opts.Match = func(c send.ComposerContent) bool { return send.ComposerTextMatches(c, want) }
	}
	res.ComposerClearResult = send.ClearClaudeComposer(pane, opts)
	switch res.Outcome {
	case send.ClearOutcomeCleared, send.ClearOutcomeAlreadyEmpty:
		res.OK, res.ExitCode = true, composerExitOK
	case send.ClearOutcomeDiffers:
		res.ExitCode = composerExitDiffers
		res.Error = "the box holds other text; nothing typed"
	default:
		res.ExitCode = composerExitUnsafe
		if res.Presses == 0 {
			res.Error = fmt.Sprintf("not safe to clear (%s); nothing typed", res.Reason)
		} else {
			res.Error = fmt.Sprintf("stopped after %d Ctrl+U (%s); the box may be partly cleared", res.Presses, res.Reason)
		}
	}
	return res
}

func printComposerClear(res composerClearResult) {
	switch res.Outcome {
	case send.ClearOutcomeCleared:
		fmt.Printf("%s: cleared\n", res.Session)
	case send.ClearOutcomeAlreadyEmpty:
		fmt.Printf("%s: already empty\n", res.Session)
	case send.ClearOutcomeDiffers:
		fmt.Fprintf(os.Stderr, "Error: %s: %s. The box holds:\n%s\n", res.Session, res.Error, res.Before.Text)
	default:
		fmt.Fprintf(os.Stderr, "Error: %s: %s\n", res.Session, res.Error)
		if res.Before.Text != "" {
			fmt.Fprintf(os.Stderr, "Before: %s\n", res.Before.Text)
		}
		if res.Presses > 0 {
			fmt.Fprintf(os.Stderr, "Now (%s): %s\n", res.After.State, strings.TrimSpace(res.After.Text))
		}
	}
}
