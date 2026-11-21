package main

// seam ready -- the actionable queue (tasks_ready).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

var readyCmd = spec("ready", groupTasks, "list the actionable (unblocked) tasks",
	noArgs(), bindReady, runReady).
	withLong("--json prints tasks_ready's answer, which always carries both the ready\n" +
		"and the blocked lists, so --blocked changes nothing there; gated and gates\n" +
		"appear whenever some work waits on people.")

type readyOpts struct {
	project     *string
	showBlocked *bool
	plan        *string
	who         *string
	agent       *bool
	asJSON      *bool
}

func bindReady(fs *flag.FlagSet) *readyOpts {
	return &readyOpts{
		project:     fs.String("project", "", "project `SLUG` (defaults to the server binding)"),
		showBlocked: fs.Bool("blocked", false, "also list blocked tasks and their blockers, and the work waiting on people"),
		plan:        fs.String("plan", "", "show the ready/blocked step tasks of plan `SLUG` instead of the default queue"),
		who:         fs.String("who", "", "list only the gates and gated tasks waiting on `WHO` (owner, reviewer:<name>, ...)"),
		agent:       fs.Bool("agent", false, "list only the tasks an agent can work now"),
		asJSON:      jsonFlag(fs, jsonUsage),
	}
}

func runReady(ctx context.Context, e *env, o *readyOpts, _ []string) error {
	if *o.who != "" && (*o.agent || *o.showBlocked) {
		return errors.New("--who lists the work waiting on one person: drop --agent and --blocked")
	}
	if *o.agent && *o.showBlocked {
		return errors.New("--agent lists only the tasks an agent can work: drop --blocked")
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	args := map[string]any{"project": *o.project, "plan": *o.plan}
	if *o.who != "" {
		args["who"] = *o.who
	}
	if *o.agent {
		args["agent"] = true
	}
	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "tasks_ready", args)
	}
	out, err := callTool(ctx, cli, "tasks_ready", args)
	if err != nil {
		return err
	}
	gates, _ := out["gates"].([]any)
	gated, _ := out["gated"].([]any)
	if *o.who != "" {
		if len(gates) == 0 && len(gated) == 0 {
			fmt.Fprintf(e.stdout, "nothing waits on %s\n", *o.who)
			return nil
		}
		printHeld(e.stdout, "gates", gates)
		printHeld(e.stdout, "gated", gated)
		return nil
	}
	ready, _ := out["ready"].([]any)
	if len(ready) == 0 {
		fmt.Fprintln(e.stdout, "ready: (nothing actionable)")
	} else {
		fmt.Fprintf(e.stdout, "ready (%d):\n", len(ready))
		for _, t := range ready {
			m, _ := t.(map[string]any)
			fmt.Fprintf(e.stdout, "  %s  %s\n", shortID(str(m["id"])), str(m["title"]))
		}
	}
	if *o.showBlocked {
		blocked, _ := out["blocked"].([]any)
		fmt.Fprintf(e.stdout, "blocked (%d):\n", len(blocked))
		for _, t := range blocked {
			m, _ := t.(map[string]any)
			fmt.Fprintf(e.stdout, "  %s  %s\n", shortID(str(m["id"])), str(m["title"]))
			printBlockers(e.stdout, m)
		}
		if len(gated) > 0 {
			printHeld(e.stdout, "gated", gated)
		}
		if len(gates) > 0 {
			printHeld(e.stdout, "gates", gates)
		}
		return nil
	}
	if len(gates) > 0 || len(gated) > 0 {
		fmt.Fprintf(e.stdout, "waiting on people: %d gated, %d gates (--blocked lists them, --who WHO filters)\n",
			len(gated), len(gates))
	}
	return nil
}

// printHeld lists tasks_ready's gated or gates entries: each task, what its
// own gate waits on, and its unfinished blockers.
func printHeld(w io.Writer, label string, list []any) {
	fmt.Fprintf(w, "%s (%d):\n", label, len(list))
	for _, t := range list {
		m, _ := t.(map[string]any)
		fmt.Fprintf(w, "  %s  %s\n", shortID(str(m["id"])), str(m["title"]))
		if g, ok := m["gate"].(map[string]any); ok && g["open"] == true {
			fmt.Fprintf(w, "      waits on %s: %s\n", str(g["who"]), str(g["what"]))
			if d := str(g["decision"]); d != "" {
				fmt.Fprintf(w, "      decision: %s\n", d)
			}
		}
		printBlockers(w, m)
	}
}

// printBlockers prints an entry's unfinished blockers: a gate by what it
// waits on, any other by its status.
func printBlockers(w io.Writer, m map[string]any) {
	bl, ok := m["blockers"].([]any)
	if !ok {
		return
	}
	for _, b := range bl {
		bm, _ := b.(map[string]any)
		// A blocker from a project this connection may not read
		// comes without title or status, only the server's words.
		if wh := str(bm["withheld"]); wh != "" {
			fmt.Fprintf(w, "      %s\n", wh)
			continue
		}
		if g, ok := bm["gate"].(map[string]any); ok && str(bm["kind"]) == "gate" {
			fmt.Fprintf(w, "      blocked by gate %s (%s: %s)\n", str(bm["title"]), str(g["who"]), str(g["what"]))
			continue
		}
		fmt.Fprintf(w, "      blocked by %s (%s)\n", str(bm["title"]), str(bm["status"]))
	}
}
