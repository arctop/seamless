package main

// seam call -- any MCP tool by name, its JSON result on stdout.
//
// The other commands each wrap one tool and render it for a person. This is
// the rest of the surface for a script or an agent with a shell: the same dial
// every command uses (dialWith -- the bearer key or the peer_auth socket, and
// the X-Seamless-Host and X-Seamless-Agent-Process headers that bind the call
// to the agent's session), the tool's result text printed exactly as the
// daemon returned it, and the tool's error as the command's.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// callTools is the tool surface `seam call --help` lists, in mcp.Catalog()'s
// order. It is a literal because cmd/seam must not import internal/mcp: that
// package drags in internal/store and the whole SQLite driver, in a binary whose
// job is one HTTP call (expectedTools in doctor.go keeps its count the same way).
// TestCallTools_MatchTheCatalog pins it to the catalog, from a test binary that
// may import what this one may not.
//
// It is help text, never a gate: a tool missing from it is still called, since
// a newer daemon may serve tools this CLI predates, and the daemon is the one
// that refuses a name it does not know. So it lists tools a given daemon may
// not serve: an optional feature's while it is off, and access_request, which
// only a daemon that locks a project registers. The help
// says both.
var callTools = []string{
	"session_start", "session_update", "session_end",
	"memory_write", "memory_append", "memory_edit", "memory_read", "memory_delete",
	"recall",
	"notes_create", "notes_read", "notes_update", "notes_edit", "notes_append", "notes_delete",
	"project_list", "project_create",
	"tasks_add", "tasks_update", "tasks_ready", "tasks_list", "tasks_claim", "tasks_release",
	"lab_open", "trial_record", "trial_query",
	"gardener_proposals", "gardener_request", "gardener_split", "gardener_apply",
	"capture_url", "usage_summary",
	"favorite_set",
	"access_request",
}

var callCmd = spec("call", groupCall, "call any MCP tool by name, print its JSON result",
	between(1, 2, "tool", "json").withHint(`arguments are one JSON object, inline or "-" for stdin`),
	bindNoOpts, runCall).
	withLong(callLong())

// callLong is call's per-command help, with the tool list wrapped to the page.
func callLong() string {
	var b strings.Builder
	b.WriteString(`Calls one MCP tool over the same connection every other seam command uses,
and prints its result to stdout exactly as the daemon returned it: a JSON
object, for jq. A tool error exits 1 with the tool's message on stderr.

The arguments are one JSON object, given inline or as "-" to read it from
stdin; none means {}. Anything that is not a JSON object is refused before
connecting. Each tool's arguments are documented in the MCP tool reference.

  seam call recall '{"query":"lease grace","scope":"notes","limit":50}' | jq -r '.hits[].name'
  echo '{"name":"lease-steal-window"}' | seam call memory_read -

Run by an agent through its shell, a call is bound to that agent's session, as
its MCP calls are; run from a terminal it is unbound, so pass "project" (or
"session") in the arguments.

Tools (an optional feature that is off serves none of its tools, and only a
daemon that locks a project serves access_request):
`)
	line := " "
	for _, t := range callTools {
		if len(line)+1+len(t) > 78 {
			b.WriteString(line + "\n")
			line = " "
		}
		line += " " + t
	}
	b.WriteString(line)
	return b.String()
}

func runCall(ctx context.Context, e *env, _ *noOpts, pos []string) error {
	tool := pos[0]
	if tool == "" {
		return errors.New("tool name is empty")
	}
	args, err := callArgs(e.stdin, pos[1:])
	if err != nil {
		return err
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	text, err := callToolRaw(ctx, cli, tool, args)
	if err != nil {
		return err
	}
	return writeRaw(e.stdout, []byte(text))
}

// callArgs reads call's arguments: none is {}, "-" is stdin, anything else is
// the JSON itself. Whatever arrives must be exactly one JSON object -- an array,
// a string, null, two objects, trailing text and an empty stdin are all present
// but uninterpretable, so each is refused here, before a connection is opened,
// rather than defaulted to {} or left for the daemon to guess at.
//
// Numbers are kept as written (UseNumber): a float64 round trip would turn an
// id-sized integer into a different one on the way to the daemon.
func callArgs(stdin io.Reader, rest []string) (map[string]any, error) {
	if len(rest) == 0 {
		return map[string]any{}, nil
	}
	src, from := []byte(rest[0]), "arguments"
	if rest[0] == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read arguments from stdin: %w", err)
		}
		src, from = b, "arguments on stdin"
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s are empty: pass a JSON object, e.g. {}", from)
		}
		return nil, fmt.Errorf("%s are not valid JSON: %w", from, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must be one JSON object, but more follows it", from)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON object, not %s", from, jsonKind(v))
	}
	return obj, nil
}

// jsonKind names a decoded JSON value's type for a refusal.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "an array"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	default:
		return fmt.Sprintf("%T", v)
	}
}
