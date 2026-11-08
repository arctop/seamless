package main

// seam ready -- the actionable queue (tasks_ready).

import (
	"context"
	"flag"
	"fmt"
)

var readyCmd = spec("ready", groupTasks, "list the actionable (unblocked) tasks",
	noArgs(), bindReady, runReady).
	withLong("--json prints tasks_ready's answer, which always carries both the ready\n" +
		"and the blocked lists, so --blocked changes nothing there.")

type readyOpts struct {
	project     *string
	showBlocked *bool
	plan        *string
	asJSON      *bool
}

func bindReady(fs *flag.FlagSet) *readyOpts {
	return &readyOpts{
		project:     fs.String("project", "", "project `SLUG` (defaults to the server binding)"),
		showBlocked: fs.Bool("blocked", false, "also list blocked tasks and their blockers"),
		plan:        fs.String("plan", "", "show the ready/blocked step tasks of plan `SLUG` instead of the default queue"),
		asJSON:      jsonFlag(fs, jsonUsage),
	}
}

func runReady(ctx context.Context, e *env, o *readyOpts, _ []string) error {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	args := map[string]any{"project": *o.project, "plan": *o.plan}
	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "tasks_ready", args)
	}
	out, err := callTool(ctx, cli, "tasks_ready", args)
	if err != nil {
		return err
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
			if bl, ok := m["blockers"].([]any); ok {
				for _, b := range bl {
					bm, _ := b.(map[string]any)
					// A blocker from a project this connection may not read
					// comes without title or status, only the server's words.
					if w := str(bm["withheld"]); w != "" {
						fmt.Fprintf(e.stdout, "      %s\n", w)
						continue
					}
					fmt.Fprintf(e.stdout, "      blocked by %s (%s)\n", str(bm["title"]), str(bm["status"]))
				}
			}
		}
	}
	return nil
}
