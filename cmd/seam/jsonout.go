package main

// --json: the daemon's own answer on stdout, for jq.
//
// Every read command renders text by default, and that text is unchanged (the
// opt-in rule: nobody sees a difference without asking for one). --json is the
// asking. It prints the tool result text or the console route body the daemon
// returned, byte for byte -- never a struct this CLI decoded and re-marshalled,
// which would silently drop every field the CLI does not know about. MCP
// clients cap how much of a tool result an agent sees; a shell pipe does not,
// which is the whole reason to reach for this instead of the MCP tool.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	mcpclient "github.com/mark3labs/mcp-go/client"

	"github.com/arctop/seamless/internal/config"
)

// jsonUsage is --json's help line on a command that prints one daemon answer.
const jsonUsage = "print the daemon's JSON answer as-is (for jq) instead of the text rendering"

// jsonFlag registers --json with the given help line.
func jsonFlag(fs *flag.FlagSet, usage string) *bool {
	return fs.Bool("json", false, usage)
}

// writeRaw prints a JSON answer as received, ending in exactly the newline a
// line-oriented pipe expects: the console's encoder already ends its body with
// one, a tool result's text never does.
func writeRaw(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}

// printToolJSON calls a tool and prints its result text as-is. A result that is
// not JSON is refused, as callTool refuses it: something other than seamlessd
// answered, and a pipe into jq must not receive it as if it were the answer.
// An empty result is refused for the same reason -- every tool answers with a
// JSON object, and printing nothing would read downstream as "no results".
func printToolJSON(ctx context.Context, e *env, cli *mcpclient.Client, name string, args map[string]any) error {
	text, err := callToolRaw(ctx, cli, name, args)
	if err != nil {
		return err
	}
	if !json.Valid([]byte(text)) {
		return fmt.Errorf("unreadable %s response from seamlessd: not JSON", name)
	}
	return writeRaw(e.stdout, []byte(text))
}

// printConsoleJSON fetches a console route and prints its body as-is.
func printConsoleJSON(e *env, cfg config.Config, path string) error {
	body, err := consoleRaw(cfg, path)
	if err != nil {
		return err
	}
	return writeRaw(e.stdout, body)
}
