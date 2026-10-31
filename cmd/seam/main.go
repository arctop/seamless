// Command seam is the headless Seamless CLI. In P2 it speaks JSON-RPC to a
// running seamlessd over /api/mcp for the minimal loop: prime (session_start +
// briefing), remember (memory_write), recall, and status. It loads the same
// config as the server, so it targets the same address and static key.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/config"
)

func main() {
	os.Exit(dispatch(context.Background(), newEnv(), os.Args[1:]))
}

// dispatch resolves argv and returns the process exit code.
//
// Every command is in the table now, so this is the whole of it. The exit codes
// are the parse/execute split the table made free:
//
//	--help, -h, help, <cmd> --help   0, to stdout
//	success                          0
//	handler error                    1
//	unparseable command line         c.usageExit() -- 2, except hook (see spec.go)
//
// --help reaching stdout at 0 is itself a fix: flag.ErrHelp used to fall through
// the same funnel as a real failure, so `seam capture --help` printed
// "error: flag: help requested" and exited 1.
func dispatch(ctx context.Context, e *env, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(e.stderr, helpText())
		return 2
	}
	switch argv[0] {
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, helpText())
		return 0
	case "-v", "--version":
		// The flag spellings are rewritten to the command rather than handled
		// here, so all three reach one handler: seamlessd accepts the same three
		// (main.go's "version", "-v", "--version"), and a second implementation
		// is how the two binaries would drift back apart.
		argv = append([]string{"version"}, argv[1:]...)
	}

	c, _, ok := lookup(commands(), argv)
	if !ok {
		// unknownCommand already names a family's members ("task bogus" lists
		// them), so only a word that belongs to no family earns the whole page.
		fmt.Fprintln(e.stderr, "error:", unknownCommand(commands(), argv))
		if len(familyOf(commands(), argv[0])) == 0 {
			fmt.Fprint(e.stderr, helpText())
		}
		return 2
	}
	p, err := parse(commands(), argv)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(e.stdout, commandHelp(*c))
		return 0
	case err != nil:
		fmt.Fprintln(e.stderr, "error:", err)
		fmt.Fprintf(e.stderr, "usage: %s\n", synopsis(*c))
		return c.usageExit()
	}
	if err := p.cmd.run(ctx, e, p.opts, p.pos); err != nil {
		fmt.Fprintln(e.stderr, "error:", err)
		return 1
	}
	return 0
}

// hostHeader carries this machine's name to the daemon on every MCP request.
// It mirrors internal/mcp.HostHeader; the CLI keeps its own literal for the same
// reason it keeps its own hook client enum -- importing that package would drag
// SQLite into a binary whose job is one HTTP call -- and a test pins the two.
const hostHeader = "X-Seamless-Host"

// agentProcessHeader names the agent process (agentproc.Anchor) that launched
// this CLI. The SessionStart hook names the same process on the ambient session
// it creates, which is how the daemon binds this connection to that session with
// no session_start call. It mirrors internal/mcp.AgentProcessHeader, pinned by a
// test like hostHeader.
const agentProcessHeader = "X-Seamless-Agent-Process"

// mcpHeaders is the header set every MCP client in this CLI sends: the bearer
// key, plus the machine the caller is on and the agent process that launched it.
// One constructor so `seam mcp-headers` and dial cannot drift apart; the proxy
// bridge sets the same two identity headers per request.
func mcpHeaders(cfg config.Config) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + cfg.MCP.APIKey}
	if host := config.Hostname(); host != "" {
		h[hostHeader] = host
	}
	if proc, ok := agentproc.Anchor(); ok {
		h[agentProcessHeader] = proc
	}
	return h
}

// dial loads config and returns an initialized MCP client plus the base URL.
func dial(ctx context.Context) (*mcpclient.Client, config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cfg, err
	}
	// No whole-request deadline: a tool call may be LLM-backed (recall
	// embeddings, gardener_request) and legitimately slow. The dial timeout
	// inside httpClient is what makes a down daemon fail fast.
	hc, err := cfg.HTTPClient(0)
	if err != nil {
		return nil, cfg, err
	}
	cli, err := mcpclient.NewStreamableHttpClient(cfg.ServerURL()+"/api/mcp",
		transport.WithHTTPHeaders(mcpHeaders(cfg)),
		transport.WithHTTPBasicClient(hc))
	if err != nil {
		return nil, cfg, err
	}
	if err := cli.Start(ctx); err != nil {
		return nil, cfg, err
	}
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "seam-cli", Version: "0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		return nil, cfg, fmt.Errorf("connect to seamlessd at %s: %w", cfg.DaemonAddress(), err)
	}
	return cli, cfg, nil
}

// callTool invokes a tool and returns its decoded JSON result.
func callTool(ctx context.Context, cli *mcpclient.Client, name string, args map[string]any) (map[string]any, error) {
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	if err != nil {
		return nil, err
	}
	text := firstText(res)
	if res.IsError {
		return nil, fmt.Errorf("%s", text)
	}
	var out map[string]any
	if text != "" {
		// Every successful tool result is marshalled JSON, so unreadable text means
		// something other than seamlessd answered. Propagate rather than returning a
		// nil map, which callers would render as a confident empty result.
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, fmt.Errorf("unreadable %s response from seamlessd: %w", name, err)
		}
	}
	return out, nil
}

func firstText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}
