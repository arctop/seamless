package main

// seam recall -- hybrid search over durable knowledge and the work record.

import (
	"context"
	"flag"
	"fmt"
	"strings"
)

// recallScopes is the accepted --scope set, declared here rather than imported.
// Its home is retrieve.RecallScopes, but cmd/seam must not import
// internal/retrieve: that pulls internal/store and with it 30 modernc.org
// packages (the whole SQLite driver) into a binary that today pulls zero -- the
// same reasoning doctor.go documents for not importing internal/mcp.
//
// Duplicating three constants at the surface is house style, not a compromise:
// console/search.go, console/sessions.go and console/notes.go each declare their
// own accepted-value slice for exactly this reason. The server still validates.
var recallScopes = []string{"all", "memories", "notes", "tasks", "trials", "sessions"}

// recall takes its query as an unbounded tail, so the words need no quoting.
// Until now it hand-rolled its own parser to accept flags on either side of that
// tail -- the permuting loop gives every command that, so the exception is gone.
var recallCmd = spec("recall", groupAgentLoop, "search knowledge and the work record",
	atLeast(1, "word"), bindRecall, runRecall).
	withLong("A query word starting with \"-\" needs the terminator:\n" +
		"  seam recall -- -foo")

type recallOpts struct {
	scope   *string
	project *string
	limit   *int
	asJSON  *bool
}

func bindRecall(fs *flag.FlagSet) *recallOpts {
	return &recallOpts{
		scope:   enumFlag(fs, "scope", "all", "what to search: `SCOPE`", recallScopes),
		project: fs.String("project", "", "project `SLUG` (default: server's binding)"),
		limit:   posIntFlag(fs, "limit", 10, "maximum results: `N`"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

func runRecall(ctx context.Context, e *env, o *recallOpts, pos []string) error {
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		// Reachable only from whitespace-only words ("seam recall ' '"): atLeast(1)
		// has already rejected an empty tail.
		return fmt.Errorf("query is empty")
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	args := map[string]any{
		"query": query, "scope": *o.scope, "project": *o.project, "limit": *o.limit,
	}
	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "recall", args)
	}
	out, err := callTool(ctx, cli, "recall", args)
	if err != nil {
		return err
	}
	hits, _ := out["hits"].([]any)
	if len(hits) == 0 {
		fmt.Fprintln(e.stdout, "no results")
		return nil
	}
	for _, h := range hits {
		m, _ := h.(map[string]any)
		// Every knowledge hit has a name (memory name, note slug); a work-record
		// hit may not (a task outside a plan carries an empty plan slug), so the
		// title is the fallback label rather than printing a blank row.
		label := str(m["name"])
		if label == "" {
			label = str(m["title"])
		}
		status := ""
		if s := str(m["status"]); s != "" {
			status = " " + s
		}
		fmt.Fprintf(e.stdout, "[%s%s] %s (%s, %s %.3f)\n    %s\n",
			str(m["kind"]), status, label, str(m["age"]), str(m["source"]), num(m["score"]), str(m["description"]))
	}
	return nil
}
