package main

// seam hook-usage -- a live Claude Code session's usage, under peer_auth, as
// a process of its own. `seam hook user-prompt-submit` starts it, detached
// from the agent (spawnDetached), when the daemon's answer to a prompt asks
// for it (harvest.UsageWantedHeader: the Diary is on, and the daemon's own
// throttle says the session's ledger is due), and returns at once: reading a
// long session's transcripts takes a moment the prompt must not wait for. It
// reads the transcript the prompt's payload named, its subagents' transcripts
// beside it included, with the daemon's own parser (harvest.ClaudeUsage), and
// sends the rows to harvest.UsageEndpoint within harvest.UsagePushTimeout. No
// installer wires it, and nobody runs it by hand; its stdio is the null
// device, and what became of the rows is the session's usage ledger.
//
// The rule of every client-side capture holds (hook_capture.go): it sends
// only to the peer_auth socket, and only what a daemon's answer asked for.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/harvest"
)

// hookUsageOpts carries the flags for `seam hook-usage`.
type hookUsageOpts struct {
	config    string // --config, as the hook had it
	client    string // --client, as the hook had it; "" is Claude Code
	sessionID string // --session-id: the agent client's own session id
	path      string // --path: the session's transcript, as the prompt named it

	// httpClient builds the client the push is sent through; nil is
	// cfg.HTTPClient(0). Not a flag: tests pass one.
	httpClient func(cfg config.Config) (*http.Client, error)
}

func bindHookUsage(fs *flag.FlagSet) *hookUsageOpts {
	o := &hookUsageOpts{}
	fs.StringVar(&o.config, "config", "", "path to seamless.yaml, as the hook was given it")
	fs.Var(&enumValue{val: &o.client, valid: hookClients}, "client",
		"agent CLI the session is from (`CLIENT`); valid: "+strings.Join(hookClients, ", ")+"; default Claude Code")
	fs.StringVar(&o.sessionID, "session-id", "", "the agent client's own session id (`ID`)")
	fs.StringVar(&o.path, "path", "", "the session's transcript (`PATH`)")
	return o
}

var hookUsageCmd = spec("hook-usage", groupHooks,
	"send a live session's usage, detached (started by seam hook)",
	noArgs(), bindHookUsage, runHookUsage).
	withLong(`seam hook starts this, detached from the agent, when the daemon's answer to
a Claude Code prompt asks for the session's usage (the Diary is on); no
installer wires it, and it is not run by hand. It reads the transcript, the
transcripts of the subagents it launched included, and sends the session's
usage ledger rows over the peer_auth socket only. It gives up after ` + harvest.UsagePushTimeout.String() + `.`)

func runHookUsage(ctx context.Context, e *env, o *hookUsageOpts, _ []string) error {
	if o.config != "" {
		if err := os.Setenv("SEAMLESS_CONFIG", o.config); err != nil {
			fmt.Fprintln(e.stderr, "seam hook-usage: set config path:", err)
			return nil
		}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-usage: load config:", err)
		return nil
	}
	if !pushesCaptures(cfg) || o.sessionID == "" || o.path == "" || (o.client != "" && o.client != hookClientClaudeCode) {
		return nil // only to the peer_auth socket, only a Claude Code session, and only what a prompt asked for
	}
	ctx, cancel := context.WithTimeout(ctx, harvest.UsagePushTimeout)
	defer cancel()
	totals, rows, ok := harvest.ClaudeUsage(o.path)
	if !ok || len(rows) > harvest.MaxUsageRows {
		return nil // nothing harvested yet, or more rows than a push carries: the session's end sends them
	}
	push := harvest.Push{Version: harvest.PushVersion, Tokens: &totals, Usage: rows}
	if err := push.Validate(); err != nil {
		// A transcript that parsed into something the daemon would refuse
		// whole (a negative count in a malformed line).
		fmt.Fprintln(e.stderr, "seam hook-usage: usage not sent:", err)
		return nil
	}
	body, err := json.Marshal(map[string]any{"session_id": o.sessionID, harvest.Field: push})
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-usage: usage not sent:", err)
		return nil
	}
	newClient := o.httpClient
	if newClient == nil {
		newClient = func(cfg config.Config) (*http.Client, error) { return cfg.HTTPClient(0) }
	}
	client, err := newClient(cfg)
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-usage:", err)
		return nil
	}
	q := url.Values{}
	if o.client != "" {
		q.Set(clientQueryParam, o.client)
	}
	if host := config.Hostname(); host != "" {
		q.Set("host", host)
	}
	target := cfg.ServerURL() + harvest.UsageEndpoint
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-usage:", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if key := cfg.MCP.APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-usage: usage not sent:", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var answer harvest.UsagePushAnswer
	//nolint:errcheck // best-effort read of the answer for its message; the status says what happened
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&answer)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(e.stderr, "seam hook-usage: usage not kept:", cmp.Or(answer.Error, resp.Status))
	}
	return nil
}
