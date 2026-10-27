package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func printInboxPostUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: agent-deck inbox post [--project <name>] [--dry-run] [--json] < envelope.json")
	fmt.Fprintln(w, "Deliver one planner event envelope (contract v1) read from stdin to the durable")
	fmt.Fprintln(w, "inbox of the session whose item matches the envelope's item, else to the")
	fmt.Fprintln(w, "project's conductor; item.ready always goes to the conductor. A repeated id")
	fmt.Fprintln(w, "is dropped (exit 0). No session and no conductor: exit 1, nothing delivered.")
	fmt.Fprintln(w, "The record is labelled PLANNER EVENT; its text is data, never instructions.")
	fmt.Fprintln(w, "No network and no planner call: the envelope is the whole input.")
	fmt.Fprintln(w, "  --project   must equal the envelope's project when given")
	fmt.Fprintln(w, "  --dry-run   print the routing decision; deliver and record nothing")
	fmt.Fprintln(w, "  --json      print the decision as JSON")
}

// runInboxPost is `agent-deck inbox post`: the planner doorbell
// (session.PostPlannerEvent). stdin carries the envelope.
func runInboxPost(stdin io.Reader, stdout io.Writer, args []string, explicitProfile string) error {
	fs := flag.NewFlagSet("inbox post", flag.ContinueOnError)
	project := fs.String("project", "", "Project name; must equal the envelope's project")
	dryRun := fs.Bool("dry-run", false, "Print the routing decision without delivering")
	asJSON := fs.Bool("json", false, "Print the decision as JSON")
	fs.SetOutput(io.Discard)
	fs.Usage = func() { printInboxPostUsage(stdout) }
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		fs.Usage()
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("inbox post takes no arguments; the envelope is read from stdin")
	}
	env, err := session.ParsePlannerEnvelope(stdin, *project)
	if err != nil {
		return fmt.Errorf("Error: %w. Nothing was delivered.", err)
	}
	storage, instances, _, err := loadSessionData(explicitProfile)
	if err != nil {
		return fmt.Errorf("Error: %w. Nothing was delivered.", err)
	}
	profile := storage.Profile()
	_ = storage.Close()

	res, postErr := session.PostPlannerEvent(profile, env, instances, *dryRun)
	if postErr != nil && !res.Delivered {
		return fmt.Errorf("Error: %w. Nothing was delivered.", postErr)
	}
	status := "delivered"
	switch {
	case res.Duplicate:
		status = "duplicate"
	case *dryRun:
		status = "dry_run"
	}
	if *asJSON {
		out := map[string]interface{}{
			"status":  status,
			"id":      env.ID,
			"project": env.Project,
			"kind":    env.Kind,
			"item":    env.Item,
			"profile": profile,
		}
		if res.Route.Target != nil {
			out["route"] = res.Route.Kind
			out["target_id"] = res.Route.Target.ID
			out["target_title"] = res.Route.Target.GetTitleThreadSafe()
			out["reason"] = res.Route.Reason
		}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			return err
		}
	} else {
		switch {
		case res.Duplicate:
			fmt.Fprintf(stdout, "Duplicate: event %s (project %s) was already delivered; dropped.\n", env.ID, env.Project)
		default:
			verb := "Delivered"
			if *dryRun {
				verb = "Would deliver"
			}
			fmt.Fprintf(stdout, "%s %s %s (item %s, id %s) to %s %q (%s): %s.\n",
				verb, session.PlannerEventTitle, env.Kind, env.Item, env.ID,
				strings.ReplaceAll(res.Route.Kind, "-", " "), res.Route.Target.GetTitleThreadSafe(), res.Route.Target.ID, res.Route.Reason)
		}
	}
	// Delivered but the id could not be recorded: say so and fail, the
	// record itself is in the inbox.
	return postErr
}
