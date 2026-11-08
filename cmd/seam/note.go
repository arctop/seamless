package main

// seam note read|list -- notes_read, and recall scoped to notes. The shared
// plumbing (readArgs, listHits, setIf) and the reason these verbs exist are in
// memory.go.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// --- note read ---

var noteReadCmd = spec("note read", groupKnowledge, "read one note by slug, or by --id",
	between(0, 1, "slug").withHint("one note per call: a slug, or --id ID"),
	bindNoteRead, runNoteRead).
	withLong(`Calls notes_read. A slug resolves in the project (--project, else the session's)
and falls back to a global note of the same slug; --id reads by ULID from any
scope the caller may read, so it takes no --project.`)

type noteReadOpts struct {
	id      *string
	project *string
	asJSON  *bool
}

func bindNoteRead(fs *flag.FlagSet) *noteReadOpts {
	return &noteReadOpts{
		id:      fs.String("id", "", "read by note `ID` (ULID) instead of by slug"),
		project: fs.String("project", "", "project `SLUG` the slug resolves in (default: the session's)"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

func runNoteRead(ctx context.Context, e *env, o *noteReadOpts, pos []string) error {
	args, err := readArgs("note", "slug", pos, *o.id, *o.project)
	if err != nil {
		return err
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "notes_read", args)
	}
	out, err := callTool(ctx, cli, "notes_read", args)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s  %s\n", str(out["slug"]), orDash(str(out["project"])))
	printField(e.stdout, "title", str(out["title"]))
	printField(e.stdout, "id", str(out["id"]))
	printField(e.stdout, "description", str(out["description"]))
	printField(e.stdout, "tags", strings.Join(strs(out["tags"]), ", "))
	printField(e.stdout, "source", str(out["source_url"]))
	printField(e.stdout, "model", str(out["model"]))
	printField(e.stdout, "provenance", writtenBy(str(out["account"])))
	printBody(e.stdout, str(out["body"]))
	return nil
}

// --- note list ---

var noteListCmd = spec("note list", groupKnowledge, "search notes (recall, scoped to notes)",
	atLeast(1, "word"), bindNoteList, runNoteList).
	withLong(`Calls recall with scope notes. No tool lists notes without a query -- recall's
browse mode is for memory kinds -- so this takes words, as recall does, and
returns the best matches first.

The limits are recall's own: at most 100 hits (--limit), inside the server's
recall token budget.`)

type noteListOpts struct {
	project *string
	limit   *int
	asJSON  *bool
}

func bindNoteList(fs *flag.FlagSet) *noteListOpts {
	return &noteListOpts{
		project: fs.String("project", "", "project `SLUG` (default: the session's; global notes are included)"),
		limit:   posIntFlag(fs, "limit", 10, "maximum results: `N`"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

func runNoteList(ctx context.Context, e *env, o *noteListOpts, pos []string) error {
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		// Reachable only from whitespace-only words: atLeast(1) has already
		// rejected an empty tail.
		return errors.New("query is empty")
	}
	args := map[string]any{"query": query, "scope": "notes", "limit": *o.limit}
	setIf(args, "project", *o.project)
	return listHits(ctx, e, args, *o.asJSON, "(no notes)", func(m map[string]any) {
		fmt.Fprintf(e.stdout, "%s  %s (%s, %s)\n", str(m["name"]), str(m["title"]), orDash(str(m["project"])), str(m["age"]))
		if d := str(m["description"]); d != "" {
			fmt.Fprintf(e.stdout, "    %s\n", d)
		}
	})
}
