package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// handleSessionAsk implements `session ask`: a session declares that it
// needs the person ("asks you"), or clears that. The ask is never sent to
// the session's parent; the person sees it in list/show JSON, the side pane,
// a phone notification (web push) and a desktop notification, steps into the
// session and answers there. Their message clears it.
func handleSessionAsk(profile string, args []string) {
	fs := flag.NewFlagSet("session ask", flag.ContinueOnError)
	self := fs.Bool("self", false, "Ask for the current session")
	kind := fs.String("kind", "", "decision, pick, approval or fix")
	item := fs.String("item", "", "Tracker item the ask is about (optional)")
	clear := fs.Bool("clear", false, "Clear the session's open ask")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	quiet := fs.Bool("quiet", false, "Minimal output")
	quietShort := fs.Bool("q", false, "Minimal output (short)")

	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session ask <session|--self> --kind <decision|pick|approval|fix> \"<question>\"")
		fmt.Println("       agent-deck session ask <session|--self> --clear")
		fmt.Println()
		fmt.Println("Mark a session as needing the person (\"asks you\"). The ask shows in")
		fmt.Println("list/show JSON (\"asks\"), the side pane and one phone and desktop")
		fmt.Println("notification. It is never relayed to the session's parent; print a")
		fmt.Println("NEED: line as well when the parent must act. The person's next message")
		fmt.Println("to the session clears it, as does --clear.")
		fmt.Println()
		fmt.Println("An agent can print the same as a line of its reply instead:")
		fmt.Println("  " + session.AsksMarker + " kind=<kind> [item=<ref>] question=<one line>")
		fmt.Println("  " + session.AsksMarker + " clear")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  agent-deck session ask --self --kind pick \"Ship layout A or B?\"")
		fmt.Println("  agent-deck session ask worker --kind approval --item ITEM-12 \"Merge the migration?\"")
		fmt.Println("  agent-deck session ask --self --clear")
	}

	if err := parseCLIFlags(fs, normalizeArgs(fs, args)); err != nil {
		exitCLI(1)
	}
	out := NewCLIOutput(*jsonOutput, *quiet || *quietShort)

	rest := fs.Args()
	ref := ""
	if !*self {
		if len(rest) == 0 {
			fs.Usage()
			exitCLI(1)
		}
		ref, rest = rest[0], rest[1:]
	}
	question := strings.TrimSpace(strings.Join(rest, " "))
	if *clear == (question != "" || *kind != "") {
		out.Error("give either --kind and a question, or --clear", ErrCodeInvalidOperation)
		exitCLI(1)
	}

	_, instances, _, err := loadSessionData(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		exitCLI(1)
	}
	if *self {
		id, err := resolveSelfSessionID()
		if err != nil {
			out.Error(err.Error(), ErrCodeNotFound)
			exitCLI(1)
		}
		ref = id
	}
	inst, errMsg, errCode := ResolveSession(ref, instances)
	if inst == nil {
		out.Error(errMsg, errCode)
		exitCLI(2)
		return
	}
	effective := session.GetEffectiveProfile(profile)

	if *clear {
		cleared, err := session.ClearAsk(effective, inst.ID, session.AskClearedByCLI)
		if err != nil {
			out.Error(fmt.Sprintf("failed to clear: %v", err), ErrCodeInvalidOperation)
			exitCLI(1)
		}
		msg := fmt.Sprintf("'%s' asks you nothing now", inst.Title)
		if !cleared {
			msg = fmt.Sprintf("'%s' had no open ask", inst.Title)
		}
		out.Success(msg, map[string]any{"success": true, "session_id": inst.ID, "cleared": cleared})
		return
	}

	ask, changed, err := session.SetAsk(effective, inst.ID, session.PersonAsk{Kind: *kind, Question: question, Item: *item})
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		exitCLI(1)
	}
	out.Success(fmt.Sprintf("'%s' asks you (%s): %s", inst.Title, ask.Kind, ask.Question), map[string]any{
		"success": true, "session_id": inst.ID, "asks": ask, "changed": changed,
	})
}
