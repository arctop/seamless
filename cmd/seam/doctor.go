package main

// seam doctor -- client-side reachability + tool-count check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
)

// expectedTools mirrors mcp.ToolCount without importing the mcp server package
// into the CLI (which would pull its whole dependency tree). doctor asserts the
// running server exposes this many tools via tools/list.
//
// It is the REGISTERED count, which is not the same as the exposed count:
// optional features (internal/features) ship OFF, and a disabled feature's tools
// are hidden from tools/list. What a given daemon should be exposing is
// expectedTools minus the tools of ITS disabled features, which is why doctor
// reads the effective feature state from the console before judging the number
// (mcpToolsCheck below).
//
// seamlessd's own doctor asserts its compiled-in mcp.ToolCount against
// REGISTRATION on a SERVER install, which is feature-independent and correctly
// stays a bare equality; on a CLIENT install it asks the same live question this
// command does, through the same features.ToolCountVerdict. The two checks
// measure different things on purpose.
const expectedTools = 33

// lockedOnlyTool is the one tool a daemon registers only while peer_auth locks
// a project (mcp.LockedOnlyTools): a live tools/list that carries it is judged
// against one more registered tool, as seamlessd's doctor does with
// mcp.ToolCountWith.
const lockedOnlyTool = "access_request"

var doctorCmd = spec("doctor", groupObservability, "reachability + key + tool-count check",
	noArgs(), bindNoOpts, runDoctor)

func runDoctor(ctx context.Context, e *env, _ *noOpts, _ []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	base := cfg.ServerURL()

	var failed int
	report := func(ok bool, name, detail string) {
		label := "ok"
		if !ok {
			label = "FAIL"
			failed++
		}
		fmt.Fprintf(e.stdout, "  [%-4s] %s: %s\n", label, name, detail)
	}

	// Which file answered comes first: every line below describes the server
	// that file names. A ./seamless.yaml this command did not read is named
	// too, since seamlessd still reads one and the two would then disagree.
	report(true, "config", configSourceDetail(cfg))
	if note := ignoredWorkingDirConfig(cfg); note != "" {
		fmt.Fprintf(e.stdout, "  [%-4s] %s: %s\n", "note", "config", note)
	}

	// Health.
	client, cerr := cfg.HTTPClient(healthTimeout)
	if cerr != nil {
		return cerr
	}
	resp, herr := client.Get(base + "/healthz")
	if herr != nil {
		report(false, "server", "unreachable at "+cfg.DaemonAddress()+": "+herr.Error())
		if failed > 0 {
			return fmt.Errorf("doctor: %d check(s) failed", failed)
		}
	} else {
		var hz map[string]any
		derr := json.NewDecoder(resp.Body).Decode(&hz)
		_ = resp.Body.Close()
		if derr != nil {
			// Without this the check still fails, but reports a blank status and no reason.
			report(false, "server", "unreadable health response from "+cfg.DaemonAddress()+": "+derr.Error())
		} else {
			report(str(hz["status"]) == "ok", "server", fmt.Sprintf("%s (%s)", str(hz["status"]), cfg.DaemonAddress()))
		}
	}

	// Key + tool count via MCP tools/list.
	cli, _, derr := e.dial(ctx)
	if derr != nil {
		report(false, "mcp", "connect failed: "+derr.Error())
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	defer func() { _ = cli.Close() }()

	tools, terr := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if terr != nil {
		report(false, "mcp_tools", "tools/list failed (bad key?): "+terr.Error())
	} else {
		locked := slices.ContainsFunc(tools.Tools, func(t mcp.Tool) bool { return t.Name == lockedOnlyTool })
		ok, detail := toolsCheck(cfg, len(tools.Tools), locked)
		report(ok, "mcp_tools", detail)
	}

	out, perr := callTool(ctx, cli, "project_list", nil)
	if perr != nil {
		report(false, "projects", perr.Error())
	} else {
		ps, _ := out["projects"].([]any)
		report(true, "projects", fmt.Sprintf("%d registered", len(ps)))
	}

	if failed > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	return nil
}

// configSourceDetail names the config file seam loaded, or that it found none.
func configSourceDetail(cfg config.Config) string {
	if src := cfg.SourcePath(); src != "" {
		return "loaded from " + src
	}
	return "defaults + env (no $SEAMLESS_CONFIG and no ~/.config/seamless/seamless.yaml)"
}

// ignoredWorkingDirConfig is the note for a seamless.yaml in the working
// directory that seam did not load: config.LoadClient never reads one, though
// seamlessd's search still does, and a developer running ./bin/seam beside the
// pre-install layout's config would otherwise see the defaults with no reason
// given. "" when there is no such file, or it is the one loaded.
func ignoredWorkingDirConfig(cfg config.Config) string {
	here, err := os.Stat(config.WorkingDirConfig)
	if err != nil {
		return ""
	}
	if src := cfg.SourcePath(); src != "" {
		if loaded, lerr := os.Stat(src); lerr == nil && os.SameFile(here, loaded) {
			return ""
		}
	}
	abs, err := filepath.Abs(config.WorkingDirConfig)
	if err != nil {
		abs = config.WorkingDirConfig
	}
	return fmt.Sprintf("%s is not read: seam reads $SEAMLESS_CONFIG, then ~/.config/seamless/seamless.yaml, never the working directory's; "+
		"to use it, set SEAMLESS_CONFIG=%s", abs, abs)
}

// toolsCheck judges the live tools/list count and renders the mcp_tools detail.
// It reads the daemon's effective optional-feature state first, because with
// optional features defaulting to OFF a fresh install exposes FEWER tools than
// are registered, and that is the common case rather than the exception.
//
// The judging itself is features.ToolCountVerdict, shared with the client-role
// `seamlessd doctor`, which asks this same question from the other binary. Only
// the registered count differs between them: seamlessd reads mcp.ToolCount
// directly, this CLI mirrors it in expectedTools above. locked says the live
// list carries lockedOnlyTool, which a daemon locking a project registers too.
func toolsCheck(cfg config.Config, live int, locked bool) (bool, string) {
	feats, ferr := consoleFeatures(cfg)
	why := ""
	if ferr != nil {
		why = ferr.Error()
	}
	registered := expectedTools
	if locked {
		registered++
	}
	return features.ToolCountVerdict(registered, live, feats, why)
}

// consoleFeatures reads the daemon's effective optional-feature state from the
// console settings JSON -- the state the MCP tool gate actually resolves, file
// and env base plus the console's stored override, rather than whatever this
// machine's config file happens to say.
//
// The field is a POINTER on purpose: a daemon that predates the features
// contract answers without it, and decoding an absent object into a value would
// yield "every optional feature off" -- a plausible dummy indistinguishable from
// a real answer, and one that would lower the expected tool count on a daemon
// exposing all of them. Absent has to stay distinguishable from off.
func consoleFeatures(cfg config.Config) (*config.Features, error) {
	var data struct {
		FeaturesConfig *config.Features `json:"featuresConfig"`
	}
	if err := consoleJSON(cfg, "/console/settings?format=json", &data); err != nil {
		return nil, err
	}
	if data.FeaturesConfig == nil {
		return nil, errors.New("settings JSON carries no featuresConfig")
	}
	return data.FeaturesConfig, nil
}
