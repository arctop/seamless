package main

// seam status -- server health + project count.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

var statusCmd = spec("status", groupObservability, "server health + project count",
	noArgs(), bindStatus, runStatus).
	withLong(`--json prints the two answers status is made of, each as the daemon sent it:

  {"healthz": <GET /healthz>, "project_list": <the project_list tool result>}

A half that could not be read is null, and the command still exits 1 naming it.`)

type statusOpts struct {
	asJSON *bool
}

func bindStatus(fs *flag.FlagSet) *statusOpts {
	return &statusOpts{asJSON: jsonFlag(fs, "print /healthz and project_list as one JSON object, each as-is")}
}

// runStatus prints what it can reach and fails if anything it needed was
// unreachable.
//
// It used to print `projects: (mcp unavailable: ...)` and return nil: the command
// whose whole purpose is answering "is this thing up?" exited 0 when the answer
// was no, which makes it useless as a scripted health gate. The two halves are not
// in tension -- the status lines are the command's OUTPUT (they are what the
// caller asked for, and a partial answer beats none), while the failure is its
// RESULT. Nor is it an AGENTS.md "log or return, never both" violation: the lines
// are the product, not a log of the error, and the error is returned once and
// printed once by the caller.
//
// The shape is doctor's -- print every check, count the failures, aggregate at the
// end -- and the precedent for "server down = error" was already established
// inside this very function: the /healthz branch below has always returned an
// error. It was the MCP branches that were the inconsistency.
//
// Exit 1, not 2: the command line was fine, the server wasn't.
func runStatus(ctx context.Context, e *env, o *statusOpts, _ []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if *o.asJSON {
		return statusJSON(ctx, e, cfg)
	}
	base := cfg.ServerURL()

	var failed int

	// Health via the unauthenticated /healthz endpoint.
	client, err := cfg.HTTPClient(healthTimeout)
	if err != nil {
		return err
	}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return fmt.Errorf("server unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	var hz map[string]any
	if derr := json.NewDecoder(resp.Body).Decode(&hz); derr != nil {
		// Degrade like the mcp branches below rather than printing a blank status:
		// something answered on the port, but it was not seamlessd.
		fmt.Fprintf(e.stdout, "server:   (unreadable health response: %v)\n", derr)
		failed++
	} else {
		fmt.Fprintf(e.stdout, "server:   %s (%s)\n", str(hz["status"]), cfg.DaemonAddress())
		fmt.Fprintf(e.stdout, "version:  %s\n", str(hz["version"]))
		if line := updateAvailableLine(hz); line != "" {
			fmt.Fprintf(e.stdout, "update:   %s\n", strings.TrimPrefix(line, "update available: "))
		}
	}
	// A client install writes nothing here: no database, no corpus, no data dir.
	// The value would still be non-empty (config.Defaults supplies ~/.seamless
	// when the client config omits data_dir), so printing it would name a
	// directory this install does not own -- and on a box converted from a
	// server, one whose contents are no longer what this `status` is describing.
	if !cfg.IsClient() {
		fmt.Fprintf(e.stdout, "data dir: %s\n", cfg.DataDir)
	}

	// Project count via MCP (also proves the static key works).
	cli, _, err := e.dial(ctx)
	if err != nil {
		fmt.Fprintf(e.stdout, "projects: (mcp unavailable: %v)\n", err)
		return statusErr(failed + 1)
	}
	defer func() { _ = cli.Close() }()
	out, err := callTool(ctx, cli, "project_list", nil)
	if err != nil {
		fmt.Fprintf(e.stdout, "projects: (error: %v)\n", err)
		return statusErr(failed + 1)
	}
	ps, _ := out["projects"].([]any)
	slugs := make([]string, 0, len(ps))
	for _, p := range ps {
		if m, ok := p.(map[string]any); ok {
			slugs = append(slugs, str(m["slug"]))
		}
	}
	fmt.Fprintf(e.stdout, "projects: %d [%s]\n", len(slugs), strings.Join(slugs, " "))
	return statusErr(failed)
}

// statusJSON is runStatus for --json: the same two checks, the same exit status,
// with each answer embedded as received. The object is assembled from the raw
// bytes rather than marshalled from a struct, since encoding/json would compact
// and re-escape the embedded answers on the way through.
//
// The data dir line has no place here: it is this machine's configuration, not
// anything the daemon answered.
func statusJSON(ctx context.Context, e *env, cfg config.Config) error {
	client, err := cfg.HTTPClient(healthTimeout)
	if err != nil {
		return err
	}
	resp, err := client.Get(cfg.ServerURL() + "/healthz")
	if err != nil {
		return fmt.Errorf("server unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	body, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	var failed []string
	healthz := []byte("null")
	switch {
	case rerr != nil:
		failed = append(failed, fmt.Sprintf("server: unreadable health response: %v", rerr))
	case !json.Valid(body):
		failed = append(failed, "server: unreadable health response: not JSON")
	default:
		healthz = bytes.TrimSpace(body)
	}

	projects := []byte("null")
	if text, perr := statusProjects(ctx, e); perr != nil {
		failed = append(failed, "projects: "+perr.Error())
	} else {
		projects = text
	}

	fmt.Fprintf(e.stdout, "{\"healthz\":%s,\"project_list\":%s}\n", healthz, projects)
	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", statusErr(len(failed)), strings.Join(failed, "; "))
	}
	return nil
}

// statusProjects is the project_list half of statusJSON, phrased as the text
// rendering phrases its failures.
func statusProjects(ctx context.Context, e *env) ([]byte, error) {
	cli, _, err := e.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp unavailable: %w", err)
	}
	defer func() { _ = cli.Close() }()
	text, err := callToolRaw(ctx, cli, "project_list", nil)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)
	}
	if !json.Valid([]byte(text)) {
		return nil, fmt.Errorf("unreadable project_list response from seamlessd: not JSON")
	}
	return []byte(text), nil
}

// statusErr aggregates the run into the command's result, in doctor's phrasing.
func statusErr(failed int) error {
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("status: %d check(s) failed", failed)
}
