package main

// seam memory read|list (and, in note.go, seam note read|list) -- the read half
// of the knowledge surface, as thin clients of the MCP tools that already serve
// it.
//
// They exist so an agent with a shell never has a reason to open files under
// the data dir: a script over ~/.seamless skips the project fences, misses
// everything that lives only in the database, and stops working under
// peer_auth, where the data dir belongs to the daemon's account. These go
// through the same tools, the same fences and the same limits as an MCP client;
// --json hands the answer to jq untouched.
//
// Listing goes through recall, the one search tool. memory list is recall's own
// browse mode (kind with no query: that kind, newest first) or a memories-only
// search; note list is a notes-only search, because no tool lists notes without
// a query -- recall's browse mode is memory kinds only.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// --- memory read ---

var memoryReadCmd = spec("memory read", groupKnowledge, "read one memory by name, or by --id",
	between(0, 1, "name").withHint("one memory per call: a name, or --id ID"),
	bindMemoryRead, runMemoryRead).
	withLong(`Calls memory_read. A name resolves in the project (--project, else the session's)
and falls back to a global memory of the same name; a superseded memory is still
returned, with a warning naming its replacement. --id reads by ULID from any scope
the caller may read, so it takes no --project.`)

type memoryReadOpts struct {
	id      *string
	project *string
	asJSON  *bool
}

func bindMemoryRead(fs *flag.FlagSet) *memoryReadOpts {
	return &memoryReadOpts{
		id:      fs.String("id", "", "read by memory `ID` (ULID) instead of by name"),
		project: fs.String("project", "", "project `SLUG` the name resolves in (default: the session's)"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

func runMemoryRead(ctx context.Context, e *env, o *memoryReadOpts, pos []string) error {
	args, err := readArgs("memory", "name", pos, *o.id, *o.project)
	if err != nil {
		return err
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	if *o.asJSON {
		return printToolJSON(ctx, e, cli, "memory_read", args)
	}
	out, err := callTool(ctx, cli, "memory_read", args)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "%s  [%s]  %s\n", str(out["name"]), str(out["kind"]), orDash(str(out["project"])))
	printField(e.stdout, "id", str(out["id"]))
	printField(e.stdout, "description", str(out["description"]))
	printField(e.stdout, "tags", strings.Join(strs(out["tags"]), ", "))
	printField(e.stdout, "model", str(out["model"]))
	printField(e.stdout, "warning", str(out["warning"]))
	printBody(e.stdout, str(out["body"]))
	return nil
}

// --- memory list ---

var memoryListCmd = spec("memory list", groupKnowledge, "list one kind of memory newest first, or search memories",
	atLeast(0, "word"), bindMemoryList, runMemoryList).
	withLong(`Calls recall, scoped to memories. --kind with no words lists that kind newest
first (recall's browse mode); words search memories, within --kind when it is
given. One of the two is required: there is no unfiltered listing.

The limits are recall's own: at most 100 hits (--limit), inside the server's
recall token budget.`)

type memoryListOpts struct {
	kind    *string
	project *string
	limit   *int
	asJSON  *bool
}

func bindMemoryList(fs *flag.FlagSet) *memoryListOpts {
	return &memoryListOpts{
		kind:    enumFlag(fs, "kind", "", "only memories of `KIND`; with no words, list that kind newest first", enumOf(core.MemoryKinds)),
		project: fs.String("project", "", "project `SLUG` (default: the session's; global memories are included)"),
		limit:   posIntFlag(fs, "limit", 10, "maximum results: `N`"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

func runMemoryList(ctx context.Context, e *env, o *memoryListOpts, pos []string) error {
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" && *o.kind == "" {
		return errors.New("pass --kind to list one kind newest first, or words to search memories")
	}
	args := map[string]any{"scope": "memories", "limit": *o.limit}
	setIf(args, "query", query)
	setIf(args, "kind", *o.kind)
	setIf(args, "project", *o.project)
	return listHits(ctx, e, args, *o.asJSON, "(no memories)", func(m map[string]any) {
		fmt.Fprintf(e.stdout, "%s (%s, %s)\n", str(m["name"]), orDash(str(m["project"])), str(m["age"]))
		if d := str(m["description"]); d != "" {
			fmt.Fprintf(e.stdout, "    %s\n", d)
		}
	})
}

// --- shared with note.go ---

// readArgs builds a memory_read / notes_read call from "exactly one of a
// positional or --id". Both refusals happen before dialing: two references is
// two reads the tool would refuse, none is nothing to read, and --project
// beside --id is a scope the id path never consults -- accepting it would be
// the silently-ignored flag the CLI's parser exists to rule out.
func readArgs(noun, key string, pos []string, id, project string) (map[string]any, error) {
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	}
	switch {
	case ref != "" && id != "":
		return nil, fmt.Errorf("pass a %s %s or --id, not both", noun, key)
	case ref == "" && id == "":
		return nil, fmt.Errorf("nothing to read: pass a %s %s, or --id ID", noun, key)
	case id != "" && project != "":
		return nil, fmt.Errorf("--project scopes a %s lookup; an --id read takes none", key)
	case id != "":
		return map[string]any{"id": id}, nil
	}
	args := map[string]any{key: ref}
	setIf(args, "project", project)
	return args, nil
}

// listHits runs one recall and prints its hits with row, or empty when there
// are none; with asJSON it prints recall's answer as-is instead.
func listHits(ctx context.Context, e *env, args map[string]any, asJSON bool, empty string, row func(map[string]any)) error {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	if asJSON {
		return printToolJSON(ctx, e, cli, "recall", args)
	}
	out, err := callTool(ctx, cli, "recall", args)
	if err != nil {
		return err
	}
	hits, _ := out["hits"].([]any)
	if len(hits) == 0 {
		fmt.Fprintln(e.stdout, empty)
		return nil
	}
	for _, h := range hits {
		m, _ := h.(map[string]any)
		row(m)
	}
	return nil
}

// setIf adds a string argument only when it carries a value, so an absent flag
// reaches the tool as an absent key rather than as "".
func setIf(args map[string]any, key, v string) {
	if v != "" {
		args[key] = v
	}
}
