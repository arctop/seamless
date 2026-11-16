package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const seamlessMCPName = "seamless"

// mcpCommandTimeout bounds each client CLI invocation so a hung codex cannot
// stall install, doctor, or uninstall. It is a variable only so tests that
// exec the fake Codex script can widen it: even a trivial /bin/sh spawn can
// exceed a bound tuned for an idle machine when the whole suite runs under
// the race detector, and the fake always exits on its own.
var mcpCommandTimeout = 5 * time.Second

// codexMCPState is the machine-readable part of `codex mcp get --json` that
// affects whether the reserved Seamless registration is operational. Unknown
// JSON fields are deliberately ignored by parseCodexMCPState so a Codex update
// can add metadata without breaking installation or doctor.
type codexMCPState struct {
	Name              string            `json:"name"`
	Enabled           bool              `json:"enabled"`
	DisabledReason    *string           `json:"disabled_reason"`
	Transport         codexMCPTransport `json:"transport"`
	EnabledTools      []string          `json:"enabled_tools"`
	DisabledTools     []string          `json:"disabled_tools"`
	StartupTimeoutSec *float64          `json:"startup_timeout_sec"`
	ToolTimeoutSec    *float64          `json:"tool_timeout_sec"`
}

// codexMCPTransport represents both transport variants emitted by Codex. The
// installer desires stdio, but retaining the HTTP authentication fields lets
// the classifier distinguish the documented direct-HTTP alternative from an
// owned stale stdio bridge without exposing any field values in diagnostics.
type codexMCPTransport struct {
	Type              string            `json:"type"`
	Command           string            `json:"command"`
	Args              []string          `json:"args"`
	Env               map[string]string `json:"env"`
	EnvVars           []json.RawMessage `json:"env_vars"`
	CWD               *string           `json:"cwd"`
	URL               string            `json:"url"`
	BearerTokenEnvVar *string           `json:"bearer_token_env_var"`
	HTTPHeaders       map[string]string `json:"http_headers"`
	EnvHTTPHeaders    map[string]string `json:"env_http_headers"`
}

// parseCodexMCPState validates the known v0.144.6 get contract without using
// DisallowUnknownFields: missing and wrong-type required fields are errors, but
// newly added fields remain forward-compatible.
func parseCodexMCPState(data []byte) (codexMCPState, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return codexMCPState{}, fmt.Errorf("parse Codex MCP JSON: %w", err)
	}
	if fields == nil {
		return codexMCPState{}, errors.New("parse Codex MCP JSON: expected object")
	}

	var state codexMCPState
	if err := requiredJSONField(fields, "name", &state.Name, false); err != nil {
		return codexMCPState{}, err
	}
	if state.Name == "" {
		return codexMCPState{}, errors.New(`parse Codex MCP JSON: field "name" is empty`)
	}
	if err := requiredJSONField(fields, "enabled", &state.Enabled, false); err != nil {
		return codexMCPState{}, err
	}
	if err := requiredJSONField(fields, "disabled_reason", &state.DisabledReason, true); err != nil {
		return codexMCPState{}, err
	}
	if err := requiredJSONField(fields, "enabled_tools", &state.EnabledTools, true); err != nil {
		return codexMCPState{}, err
	}
	if err := requiredJSONField(fields, "disabled_tools", &state.DisabledTools, true); err != nil {
		return codexMCPState{}, err
	}
	if err := requiredJSONField(fields, "startup_timeout_sec", &state.StartupTimeoutSec, true); err != nil {
		return codexMCPState{}, err
	}
	if err := requiredJSONField(fields, "tool_timeout_sec", &state.ToolTimeoutSec, true); err != nil {
		return codexMCPState{}, err
	}

	var transportFields map[string]json.RawMessage
	if err := requiredJSONField(fields, "transport", &transportFields, false); err != nil {
		return codexMCPState{}, err
	}
	if transportFields == nil {
		return codexMCPState{}, errors.New(`parse Codex MCP JSON: field "transport" must be an object`)
	}
	if err := requiredJSONField(transportFields, "type", &state.Transport.Type, false); err != nil {
		return codexMCPState{}, fmt.Errorf("parse Codex MCP transport: %w", err)
	}
	if state.Transport.Type == "" {
		return codexMCPState{}, errors.New(`parse Codex MCP JSON: field "transport.type" is empty`)
	}

	switch state.Transport.Type {
	case "stdio":
		if err := requiredJSONField(transportFields, "command", &state.Transport.Command, false); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP stdio transport: %w", err)
		}
		if state.Transport.Command == "" {
			return codexMCPState{}, errors.New(`parse Codex MCP JSON: field "transport.command" is empty`)
		}
		if err := requiredJSONField(transportFields, "args", &state.Transport.Args, false); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP stdio transport: %w", err)
		}
		if err := requiredJSONField(transportFields, "env", &state.Transport.Env, true); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP stdio transport: %w", err)
		}
		if err := requiredJSONField(transportFields, "env_vars", &state.Transport.EnvVars, true); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP stdio transport: %w", err)
		}
		if err := requiredJSONField(transportFields, "cwd", &state.Transport.CWD, true); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP stdio transport: %w", err)
		}
	case "streamable_http":
		if err := requiredJSONField(transportFields, "url", &state.Transport.URL, false); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP HTTP transport: %w", err)
		}
		if state.Transport.URL == "" {
			return codexMCPState{}, errors.New(`parse Codex MCP JSON: field "transport.url" is empty`)
		}
		if err := requiredJSONField(transportFields, "bearer_token_env_var", &state.Transport.BearerTokenEnvVar, true); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP HTTP transport: %w", err)
		}
		if err := requiredJSONField(transportFields, "http_headers", &state.Transport.HTTPHeaders, true); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP HTTP transport: %w", err)
		}
		if err := requiredJSONField(transportFields, "env_http_headers", &state.Transport.EnvHTTPHeaders, true); err != nil {
			return codexMCPState{}, fmt.Errorf("parse Codex MCP HTTP transport: %w", err)
		}
	}

	return state, nil
}

func requiredJSONField(fields map[string]json.RawMessage, name string, dst any, nullable bool) error {
	raw, ok := fields[name]
	if !ok {
		return fmt.Errorf("parse Codex MCP JSON: missing required field %q", name)
	}
	if !nullable && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("parse Codex MCP JSON: required field %q is null", name)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("parse Codex MCP JSON field %q: %w", name, err)
	}
	return nil
}

// desiredCodexMCPState derives the comparator input from the same argv used by
// installation. This keeps the registration command, installer comparison, and
// doctor comparison from acquiring three hand-transcribed definitions.
func desiredCodexMCPState(seamBin, configPath string) (codexMCPState, error) {
	args := codexMCPAddArgs(seamBin, configPath)
	separator := slices.Index(args, "--")
	if len(args) < 5 || separator != 3 || args[0] != "mcp" || args[1] != "add" || args[2] == "" {
		return codexMCPState{}, errors.New("build desired Codex MCP state: invalid add arguments")
	}
	if separator+1 >= len(args) || args[separator+1] == "" {
		return codexMCPState{}, errors.New("build desired Codex MCP state: missing bridge command")
	}
	if !portableAbsolutePath(args[separator+1]) {
		return codexMCPState{}, fmt.Errorf("build desired Codex MCP state: bridge command %q is not absolute", args[separator+1])
	}
	if configPath != "" && !portableAbsolutePath(configPath) {
		return codexMCPState{}, fmt.Errorf("build desired Codex MCP state: config path %q is not absolute", configPath)
	}
	return codexMCPState{
		Name:    args[2],
		Enabled: true,
		Transport: codexMCPTransport{
			Type:    "stdio",
			Command: args[separator+1],
			Args:    slices.Clone(args[separator+2:]),
			EnvVars: make([]json.RawMessage, 0),
		},
	}, nil
}

// portableAbsolutePath recognizes the host OS's absolute paths plus Windows
// drive and UNC paths so fixture/adapter tests can round-trip Windows argv on a
// non-Windows builder.
func portableAbsolutePath(value string) bool {
	if filepath.IsAbs(value) || strings.HasPrefix(value, `\\`) || strings.HasPrefix(value, "//") {
		return true
	}
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}

// codexMCPDrift is the one exact comparator used by installation and doctor.
// Its labels deliberately omit values from environment/header fields because a
// foreign registration may contain credentials.
func codexMCPDrift(got, want codexMCPState) []string {
	var drift []string
	if got.Name != want.Name {
		drift = append(drift, "server name differs")
	}
	if got.Enabled != want.Enabled {
		if !got.Enabled && want.Enabled {
			drift = append(drift, "registration is disabled")
		} else {
			drift = append(drift, "enabled state differs")
		}
	}
	if !equalOptionalString(got.DisabledReason, want.DisabledReason) {
		drift = append(drift, "disabled reason differs")
	}
	if got.Transport.Type != want.Transport.Type {
		drift = append(drift, "transport type differs")
	} else {
		switch got.Transport.Type {
		case "stdio":
			if got.Transport.Command != want.Transport.Command {
				drift = append(drift, "bridge command differs")
			}
			if !slices.Equal(got.Transport.Args, want.Transport.Args) {
				drift = append(drift, "bridge arguments differ")
			}
			if !maps.Equal(got.Transport.Env, want.Transport.Env) ||
				!rawMessagesEqual(got.Transport.EnvVars, want.Transport.EnvVars) {
				drift = append(drift, "bridge environment differs")
			}
			if !equalOptionalString(got.Transport.CWD, want.Transport.CWD) {
				drift = append(drift, "bridge working directory differs")
			}
		case "streamable_http":
			if got.Transport.URL != want.Transport.URL {
				drift = append(drift, "HTTP URL differs")
			}
			if !equalOptionalString(got.Transport.BearerTokenEnvVar, want.Transport.BearerTokenEnvVar) ||
				!maps.Equal(got.Transport.HTTPHeaders, want.Transport.HTTPHeaders) ||
				!maps.Equal(got.Transport.EnvHTTPHeaders, want.Transport.EnvHTTPHeaders) {
				drift = append(drift, "HTTP authentication fields differ")
			}
		}
	}
	if !slices.Equal(got.EnabledTools, want.EnabledTools) || !slices.Equal(got.DisabledTools, want.DisabledTools) {
		drift = append(drift, "tool filters differ")
	}
	if !equalOptionalFloat(got.StartupTimeoutSec, want.StartupTimeoutSec) ||
		!equalOptionalFloat(got.ToolTimeoutSec, want.ToolTimeoutSec) {
		drift = append(drift, "timeouts differ")
	}
	return drift
}

func equalOptionalString(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func equalOptionalFloat(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func rawMessagesEqual(a, b []json.RawMessage) bool {
	return slices.EqualFunc(a, b, func(x, y json.RawMessage) bool {
		return bytes.Equal(bytes.TrimSpace(x), bytes.TrimSpace(y))
	})
}

// mcpRegClass is the shared classification vocabulary for a found registration
// under the reserved Seamless name, whatever surface holds it (the Codex CLI
// store or the Claude desktop config file): exact matches are left alone,
// owned drift is repaired in place, and an incompatible entry is never touched
// -- the owner must remove it explicitly.
type mcpRegClass string

const (
	mcpRegExact        mcpRegClass = "exact"
	mcpRegOwnedDrifted mcpRegClass = "owned-drifted"
	mcpRegIncompatible mcpRegClass = "incompatible"
)

func classifyCodexMCPState(got, want codexMCPState) (mcpRegClass, []string) {
	drift := codexMCPDrift(got, want)
	if len(drift) == 0 {
		return mcpRegExact, nil
	}
	if codexMCPIsOwned(got, want) {
		return mcpRegOwnedDrifted, drift
	}
	return mcpRegIncompatible, drift
}

// codexMCPIsOwned is conservative because "seamless" is reserved but users may
// deliberately configure the documented direct-HTTP alternative. A stdio entry
// is repairable when its command is the desired command or is recognizably the
// seam/seam.exe CLI; other entries require explicit removal by the owner.
func codexMCPIsOwned(got, want codexMCPState) bool {
	if got.Name != seamlessMCPName || got.Transport.Type != "stdio" {
		return false
	}
	return got.Transport.Command == want.Transport.Command || isSeamCommand(got.Transport.Command)
}

func isSeamCommand(command string) bool {
	base := pathpkg.Base(strings.ReplaceAll(command, `\`, "/"))
	return strings.EqualFold(base, "seam") || strings.EqualFold(base, "seam.exe")
}

// mcpCommandRunner makes timeout behavior explicit and keeps reconciliation
// testable with an isolated fake client executable.
type mcpCommandRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}

type execMCPCommandRunner struct {
	client  string
	path    string
	timeout time.Duration
}

func (r execMCPCommandRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = mcpCommandTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, r.path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// Machine-readable MCP commands own stdout. A successful CLI may still emit
	// benign diagnostics on stderr (Codex prints a PATH-alias warning in a
	// restricted environment); mixing that text into JSON makes an exact,
	// healthy registration look malformed. On failure retain stderr beside any
	// partial stdout so installer/doctor diagnostics stay actionable.
	if err != nil && stderr.Len() > 0 {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, stderr.Bytes()...)
	}
	op := strings.TrimSpace(r.client + " " + strings.Join(args, " "))
	if commandCtx.Err() != nil {
		return out, fmt.Errorf("%s: %w", op, commandCtx.Err())
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

func inspectCodexMCP(ctx context.Context, runner mcpCommandRunner) (codexMCPState, bool, error) {
	out, getErr := runner.Run(ctx, "mcp", "get", seamlessMCPName, "--json")
	if getErr == nil {
		state, err := parseCodexMCPState(out)
		if err != nil {
			return codexMCPState{}, false, fmt.Errorf("inspect Codex MCP registration: %w", err)
		}
		return state, true, nil
	}
	if errors.Is(getErr, context.DeadlineExceeded) || errors.Is(getErr, context.Canceled) {
		return codexMCPState{}, false, fmt.Errorf("inspect Codex MCP registration: %w", getErr)
	}

	// A failed get has no structured absent error. Consult list --json rather
	// than matching the CLI's human prose; only a successful list that omits the
	// reserved name proves absence.
	listOut, listErr := runner.Run(ctx, "mcp", "list", "--json")
	if listErr != nil {
		return codexMCPState{}, false, fmt.Errorf("inspect Codex MCP registration: get failed and absence check failed: %w", errors.Join(getErr, listErr))
	}
	names, err := parseCodexMCPListNames(listOut)
	if err != nil {
		return codexMCPState{}, false, fmt.Errorf("inspect Codex MCP registration: %w", err)
	}
	if !slices.Contains(names, seamlessMCPName) {
		return codexMCPState{}, false, nil
	}
	return codexMCPState{}, false, fmt.Errorf("inspect Codex MCP registration: reserved name is listed but get failed: %w", getErr)
}

func parseCodexMCPListNames(data []byte) ([]string, error) {
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse `codex mcp list --json`: %w", err)
	}
	if entries == nil {
		return nil, errors.New("parse `codex mcp list --json`: expected array")
	}
	names := make([]string, 0, len(entries))
	for i, fields := range entries {
		var name string
		if err := requiredJSONField(fields, "name", &name, false); err != nil {
			return nil, fmt.Errorf("parse `codex mcp list --json` entry %d: %w", i, err)
		}
		if name == "" {
			return nil, fmt.Errorf("parse `codex mcp list --json` entry %d: name is empty", i)
		}
		names = append(names, name)
	}
	return names, nil
}

type mcpRegAction string

const (
	mcpRegAdded     mcpRegAction = "added"
	mcpRegUnchanged mcpRegAction = "unchanged"
	mcpRegRepaired  mcpRegAction = "repaired"
)

type mcpRegResult struct {
	Action mcpRegAction
	Drift  []string
}

func reconcileCodexMCP(ctx context.Context, runner mcpCommandRunner, seamBin, configPath string) (mcpRegResult, error) {
	want, err := desiredCodexMCPState(seamBin, configPath)
	if err != nil {
		return mcpRegResult{}, err
	}
	got, present, err := inspectCodexMCP(ctx, runner)
	if err != nil {
		return mcpRegResult{}, err
	}

	action := mcpRegAdded
	var priorDrift []string
	if present {
		class, drift := classifyCodexMCPState(got, want)
		switch class {
		case mcpRegExact:
			return mcpRegResult{Action: mcpRegUnchanged}, nil
		case mcpRegIncompatible:
			return mcpRegResult{}, fmt.Errorf(
				"reserved MCP name %q has an incompatible configuration (%s); remove it explicitly with `codex mcp remove %s` before rerunning install-hooks",
				seamlessMCPName, strings.Join(drift, ", "), seamlessMCPName)
		case mcpRegOwnedDrifted:
			action = mcpRegRepaired
			priorDrift = drift
		}
	}

	// Codex 0.144.6 replaces an existing named entry directly with `mcp add`, so
	// owned drift needs no remove-first gap. If a future CLI rejects replacement,
	// the old entry remains intact and this returns a visible error.
	addOut, err := runner.Run(ctx, codexMCPAddArgs(seamBin, configPath)...)
	if err != nil {
		return mcpRegResult{}, fmt.Errorf("write desired Codex MCP registration: %w%s", err, commandOutputSuffix(addOut))
	}

	// Never report success from the add command alone. A fresh get --json must
	// decode and exactly match the same desired value used above.
	verified, verifiedPresent, err := inspectCodexMCP(ctx, runner)
	if err != nil {
		return mcpRegResult{}, fmt.Errorf("verify Codex MCP registration after add: %w", err)
	}
	if !verifiedPresent {
		return mcpRegResult{}, errors.New("verify Codex MCP registration after add: reserved name is still absent")
	}
	if drift := codexMCPDrift(verified, want); len(drift) > 0 {
		return mcpRegResult{}, fmt.Errorf("verify Codex MCP registration after add: desired state still differs (%s)", strings.Join(drift, ", "))
	}
	return mcpRegResult{Action: action, Drift: priorDrift}, nil
}

func commandOutputSuffix(out []byte) string {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// mcpManualCommand is the client CLI command line a person pastes to do what
// the installer could not, each argument quoted for this computer's shell
// (shellQuoter): the Claude Code registration's JSON holds quotes and spaces,
// and a path may. A word every shell reads literally stays bare.
func mcpManualCommand(client string, args []string) string {
	quote := shellQuoter(runtime.GOOS)
	words := make([]string, 0, len(args)+1)
	words = append(words, client)
	for _, arg := range args {
		words = append(words, quote(arg))
	}
	return strings.Join(words, " ")
}

func registerCodexMCP(seamBin, configPath string) error {
	args := codexMCPAddArgs(seamBin, configPath)
	manual := mcpManualCommand("codex", args)
	codex, err := exec.LookPath("codex")
	if err != nil {
		fieldRow("mcp", yellow("incomplete (management CLI not found)"))
		fmt.Printf("%s%s\n", fieldCont, dim(codexAppMCPSetupHint(seamBin, configPath)))
		fmt.Printf("%s%s\n", fieldCont, dim("CLI alternative: "+manual))
		return nil
	}

	result, err := reconcileCodexMCP(context.Background(), execMCPCommandRunner{
		client: "codex", path: codex, timeout: mcpCommandTimeout,
	}, seamBin, configPath)
	if err != nil {
		fieldRow("mcp", yellow("registration reconciliation failed"))
		fmt.Printf("%s%s\n", fieldCont, dim(err.Error()))
		fmt.Printf("%s%s\n", fieldCont, dim(manual))
		return fmt.Errorf("reconcile Codex MCP registration: %w", err)
	}

	switch result.Action {
	case mcpRegUnchanged:
		fieldRow("mcp", dim("already registered (exact stdio bridge)"))
	case mcpRegRepaired:
		fieldRow("mcp", green("repaired")+dim(" ("+strings.Join(result.Drift, ", ")+")"))
	default:
		fieldRow("mcp", green("registered")+dim(" (stdio bridge: seam mcp-proxy)"))
	}
	return nil
}

// codexAppMCPSetupHint is the app-only fallback when no `codex mcp` management
// command is available. Hooks and skills can still be installed by writing the
// shared Codex home, but MCP is not complete until the user adds this exact
// stdio bridge in the desktop settings and restarts the app.
func codexAppMCPSetupHint(seamBin, configPath string) string {
	args := []string{"mcp-proxy"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	quotedArgs := make([]string, len(args))
	for i, arg := range args {
		quotedArgs[i] = fmt.Sprintf("%q", arg)
	}
	return fmt.Sprintf(
		"in Codex app: Settings > MCP servers > Add server > STDIO; name %q; command %q; arguments %s; Save, then Restart",
		seamlessMCPName, seamBin, strings.Join(quotedArgs, " "))
}

// mcpBridgePathProblems checks the local paths that an exact registration will
// execute -- the same stdio bridge whichever surface holds it (Codex CLI store
// or Claude desktop config). It is kept separate from the state comparators:
// installation may intentionally be staged before the binary exists, while
// doctor must call that state non-operational.
func mcpBridgePathProblems(command string, args []string) []string {
	var problems []string
	if !commandPathExists(command) {
		problems = append(problems, "bridge executable is missing")
	}
	if configPath := mcpBridgeConfigPath(args); configPath != "" {
		if info, err := os.Stat(configPath); err != nil || info.IsDir() {
			problems = append(problems, "config path is missing")
		}
	}
	return problems
}

func commandPathExists(command string) bool {
	if strings.ContainsAny(command, `/\`) {
		info, err := os.Stat(command)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(command)
	return err == nil
}

func mcpBridgeConfigPath(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--config" {
			return args[i+1]
		}
	}
	return ""
}
