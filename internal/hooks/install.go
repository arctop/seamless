package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/files"
)

// managedMarker tags a hook entry as owned by Seamless, so re-installs replace it
// in place. It is deliberately distinct from v1's "seam_managed" so v1 and v2
// entries never match (and clobber) each other if they ever share a file.
const managedMarker = "seamless_managed"

// hookSpec describes one hook Seamless installs for a client.
type hookSpec struct {
	Event    string // hooks key (SessionStart, UserPromptSubmit, Stop, ...)
	Matcher  string // "" omits the matcher key (UserPromptSubmit/Stop have none)
	Endpoint string // path appended to the base URL (the http url, and the dedup key)
	Timeout  int    // seconds (both clients' unit)
	CLIArg   string // non-empty => install a `command` hook (`<seam> hook <CLIArg>`) not http; profileForBaseURL fills the empty ones under https
}

// seamlessHooks is the set installed/removed together.
//
// SessionStart and SessionEnd are command hooks. Claude Code only runs
// command/mcp_tool hooks for SessionStart, so an http one is silently skipped
// and the briefing/ambient session never fire. SessionEnd does support http,
// but at process exit the fire-and-forget request races the teardown, so the
// ambient-session harvest often never lands and sessions pile up as active;
// running it as a command hook Claude Code waits on makes the harvest reliable.
// Each runs `seam hook <event>` (exec form, no shell), which forwards the
// payload to Endpoint. UserPromptSubmit fires mid-turn where http is reliable,
// so it keeps an http hook (and carries the bearer key into settings.json) --
// on an http base URL. Against an https one profileForBaseURL turns it into a
// command hook like the rest, because Claude Code's own HTTP client cannot be
// pointed at tls.ca_file.
//
// The plan-capture hooks (PostToolUse, SubagentStop, PermissionRequest) are
// command hooks too: the seam CLI pre-filters PostToolUse locally so the
// machine-wide Write/Edit hot path never touches the network for non-plan
// files. PostToolUse must stay a SINGLE entry (matcher-joined) -- the
// dedupe/adopt logic assumes one managed entry per event.
//
// SubagentStart mirrors the Codex profile: every Task-tool child gets the
// constraints-only briefing from /api/hooks/subagent-start. Claude Code emits
// no SessionStart for its subagents (its sources are startup|resume|clear|
// compact), so without this hook a child runs with zero constraint awareness.
// No matcher: every agent type is briefed.
var seamlessHooks = []hookSpec{
	{Event: "SessionStart", Matcher: "startup|resume|clear|compact", Endpoint: "/api/hooks/session-start", Timeout: 10, CLIArg: "session-start"},
	{Event: "UserPromptSubmit", Matcher: "", Endpoint: "/api/hooks/user-prompt-submit", Timeout: 5},
	{Event: "SessionEnd", Matcher: "", Endpoint: "/api/hooks/session-end", Timeout: 10, CLIArg: "session-end"},
	{Event: "PostToolUse", Matcher: "Write|Edit|MultiEdit|ExitPlanMode", Endpoint: "/api/hooks/post-tool-use", Timeout: 10, CLIArg: "post-tool-use"},
	{Event: "SubagentStart", Matcher: "", Endpoint: "/api/hooks/subagent-start", Timeout: 10, CLIArg: "subagent-start"},
	{Event: "SubagentStop", Matcher: "", Endpoint: "/api/hooks/subagent-stop", Timeout: 10, CLIArg: "subagent-stop"},
	{Event: "PermissionRequest", Matcher: "ExitPlanMode", Endpoint: "/api/hooks/permission-request", Timeout: 10, CLIArg: "permission-request"},
}

// codexHooks is the shared local Codex app/CLI/IDE install profile (design
// decisions D4-D6). Codex has no http hook type and (through 0.144.6) no
// SessionEnd event; its session
// end is reaper-driven off the per-turn Stop hook (D5). SubagentStart injects a
// constraints-only briefing into a child, and SubagentStop performs only a safe
// parent heartbeat. Neither enters Claude Code's durable plan-note capture: no
// Claude-style plan-file/ExitPlanMode surface has been verified for Codex.
// UserPromptSubmit is a command hook here (CC keeps it http). Every Codex command
// hook is a SHELL STRING, not the exec-form argv CC uses (buildEntry emits the
// difference).
var codexHooks = []hookSpec{
	{Event: "SessionStart", Matcher: "startup|resume|clear|compact", Endpoint: "/api/hooks/session-start", Timeout: 10, CLIArg: "session-start"},
	{Event: "UserPromptSubmit", Matcher: "", Endpoint: "/api/hooks/user-prompt-submit", Timeout: 5, CLIArg: "user-prompt-submit"},
	{Event: "Stop", Matcher: "", Endpoint: "/api/hooks/stop", Timeout: 10, CLIArg: "stop"},
	{Event: "SubagentStart", Matcher: "", Endpoint: "/api/hooks/subagent-start", Timeout: 10, CLIArg: "subagent-start"},
	{Event: "SubagentStop", Matcher: "", Endpoint: "/api/hooks/subagent-stop", Timeout: 10, CLIArg: "subagent-stop"},
}

// resolveHookProfile validates a programmatic client selection and returns its
// canonical Client plus profile. The zero value is the documented absent/default
// Claude selection; every non-empty value must belong to HookClients.
func resolveHookProfile(raw Client) (Client, []hookSpec, error) {
	client, err := parseClient(string(raw), raw != "")
	if err != nil {
		return "", nil, err
	}
	if client == ClientCodex {
		return client, codexHooks, nil
	}
	return client, seamlessHooks, nil
}

// profileForBaseURL adapts a client profile to the transport its base URL
// implies: under https every hook is a command hook, under http the table is
// returned untouched.
//
// The decision is DERIVED from opts.BaseURL rather than carried as an install
// option on purpose. Install, InstalledStatus, Uninstall, and doctor's
// desired-vs-installed comparison all already share that one field, so they
// cannot disagree about which shape is correct; an extra option would have to be
// set identically in four places, and the one that forgot would wire one shape
// and judge another -- a desync whose only symptom is a hook reported stale
// forever, or worse, silently rewritten on every run.
//
// Why https forces it: Claude Code performs an http hook's request with its own
// HTTP client, which knows nothing of tls.ca_file. Against a private-CA or
// self-signed server that request dies in the TLS handshake with nowhere to
// configure trust, and a hook that quietly stops firing is the worst failure
// this profile has. `seam hook <event>` makes the same request through the CLI's
// single HTTP client, which does read tls.ca_file (cmd/seam/env.go httpClient),
// and as a bonus it keeps the bearer key out of settings.json. The seam CLI has
// always accepted every event in this table as a command hook (cmd/seam/hook.go
// hookEvents), which is what makes the flip a transport change rather than a new
// protocol.
//
// A peer_auth socket base URL (unix:///path) forces it for a stronger reason:
// Claude Code's http hook cannot dial a Unix socket at all, and the socket is
// the only way such an install reaches its daemon.
func profileForBaseURL(profile []hookSpec, baseURL string) []hookSpec {
	if !CommandOnlyBaseURL(baseURL) {
		return profile
	}
	out := slices.Clone(profile)
	for i := range out {
		if out[i].CLIArg == "" {
			// The CLI event name is the last segment of the endpoint for every
			// spec that already has one (TestHookSpecCLIArgsDeriveFromEndpoints
			// pins it), so this derives rather than transcribing a second table.
			out[i].CLIArg = pathpkg.Base(out[i].Endpoint)
		}
	}
	return out
}

// IsHTTPSBaseURL reports whether baseURL names an https endpoint. A value that
// does not parse is not https: the shapes this gates are the strictly safer
// ones, so an unreadable URL falls back to today's profile rather than to a
// speculative change.
//
// It is exported for one caller: the installer's MCP registration, which must
// reach the SAME conclusion as the hook profile above (https -> the seam stdio
// bridge, http -> the direct URL). Two copies of the scheme test is how a
// machine ends up with exec-form hooks and an http MCP registration that cannot
// verify the server.
func IsHTTPSBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https")
}

// IsSocketBaseURL reports whether baseURL names a peer_auth Unix socket
// (unix:///path, config.Config.HookBaseURL).
func IsSocketBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "unix")
}

// CommandOnlyBaseURL reports whether every hook and the MCP registration must go
// through the seam CLI for baseURL: an https one (Claude Code cannot be told
// about tls.ca_file) or a peer_auth socket (Claude Code cannot dial one). The
// installer's MCP registration asks the same question, so the two cannot
// disagree.
func CommandOnlyBaseURL(baseURL string) bool {
	return IsHTTPSBaseURL(baseURL) || IsSocketBaseURL(baseURL)
}

// InstallOptions configures an install.
type InstallOptions struct {
	Client       Client // agent client profile; "" (zero value) => Claude Code
	SettingsPath string // target file: CC settings.json or Codex hooks.json (created if absent)
	BaseURL      string // e.g. http://127.0.0.1:8081
	APIKey       string // static bearer key (written into the CC http hook header; Codex command hooks carry none)
	SeamBin      string // path to the seam CLI for command hooks; "" => "seam" (PATH)
	ConfigPath   string // abs seamless.yaml passed to command hooks as `--config` so they resolve config from any cwd; "" omits it
}

// InstallResult reports what an install did.
type InstallResult struct {
	Changed    bool
	BackupPath string   // "" when no backup was written
	Actions    []string // per-hook: "SessionStart: added|updated|unchanged"
}

// InstallStatus separates exact current definitions from stale definitions
// that Seamless owns or can confidently adopt. Owned is their union in profile
// order and is the set uninstall may remove; foreign definitions are omitted.
type InstallStatus struct {
	Current []string
	Stale   []string
	Owned   []string
}

// Install merges the client's Seamless hook entries into the settings/hooks file
// at opts.SettingsPath, preserving unknown keys, replacing any existing
// Seamless-managed entries in place, and backing the file up once before the
// first change. It is idempotent: an already-current file is left untouched.
func Install(opts InstallOptions) (InstallResult, error) {
	client, profile, err := resolveHookProfile(opts.Client)
	if err != nil {
		return InstallResult{}, fmt.Errorf("hooks.Install: %w", err)
	}
	// A socket install carries no key at all: the daemon identifies its caller
	// by the uid the kernel reports, and its hooks are all command hooks, which
	// never write a key into the settings file anyway.
	if strings.TrimSpace(opts.APIKey) == "" && !IsSocketBaseURL(opts.BaseURL) {
		return InstallResult{}, fmt.Errorf("hooks.Install: api key is required")
	}
	if strings.TrimSpace(opts.SettingsPath) == "" {
		return InstallResult{}, fmt.Errorf("hooks.Install: settings path is required")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return InstallResult{}, fmt.Errorf("hooks.Install: base URL is required")
	}
	if err := validateDefinitionPaths("hooks.Install", opts.SeamBin, opts.ConfigPath); err != nil {
		return InstallResult{}, err
	}
	profile = profileForBaseURL(profile, opts.BaseURL)

	settings, mode, err := loadSettings(opts.SettingsPath)
	if err != nil {
		return InstallResult{}, err
	}
	hooksObj := nestedObject(settings, "hooks")

	// Both clients nest event arrays under a top-level "hooks" key, so the merge
	// engine below is shared; only the validated profile and each entry's handler
	// shape (buildEntry) vary by client. An empty client is Claude Code.
	var res InstallResult
	for _, hs := range profile {
		desired := buildEntry(client, hs, opts.BaseURL, opts.APIKey, opts.SeamBin, opts.ConfigPath)
		desiredURL := strings.TrimRight(opts.BaseURL, "/") + hs.Endpoint
		arr := entryArray(hooksObj, hs.Event)
		// Classify exact current definitions, marked stale definitions, and only
		// the known marker-stripped Seamless URL/command shapes. Arbitrary tools
		// whose arguments happen to contain hook + event remain foreign. A v1
		// "seam_managed" entry at another URL remains foreign too.
		matches, classes := classifiedHookIndices(client, arr, desired, hs, desiredURL)
		switch {
		case len(matches) == 0:
			arr = append(arr, desired)
			res.Changed = true
			res.Actions = append(res.Actions, hs.Event+": added")
		case len(matches) == 1 && classes[0] == hookDefinitionCurrent:
			res.Actions = append(res.Actions, hs.Event+": unchanged")
		default:
			// Keep the first owned entry (replacing it with the canonical
			// desired form) and drop any other owned duplicates.
			arr[matches[0]] = desired
			arr = removeIndices(arr, matches[1:])
			res.Changed = true
			res.Actions = append(res.Actions, hs.Event+": "+matchAction(len(matches), classes[0]))
		}
		hooksObj[hs.Event] = arr
	}
	if !res.Changed {
		return res, nil
	}
	settings["hooks"] = hooksObj

	backup, err := backupOnce(opts.SettingsPath, mode)
	if err != nil {
		return InstallResult{}, err
	}
	res.BackupPath = backup

	if err := writeSettings(opts.SettingsPath, settings, mode); err != nil {
		return InstallResult{}, err
	}
	return res, nil
}

// InstalledEvents is the set of hook events Seamless installs for a client, in
// install order. A caller (doctor) compares InstalledStatus against
// len(InstalledEvents) for the same client.
func InstalledEvents(client Client) ([]string, error) {
	_, profile, err := resolveHookProfile(client)
	if err != nil {
		return nil, fmt.Errorf("hooks.InstalledEvents: %w", err)
	}
	out := make([]string, len(profile))
	for i, hs := range profile {
		out[i] = hs.Event
	}
	return out, nil
}

// CommandHookEndpoints returns the `seam hook <arg>` events the installer wires
// as command hooks across every client profile, each mapped to the endpoint that
// hook must forward to. It is a union: the Codex profile adds user-prompt-submit
// (a command hook there, http for CC) and stop, so the CLI pin covers both
// clients. An event that both profiles wire as a command hook shares one endpoint.
//
// It exists for the seam CLI's test. The CLI keeps its own copy of this mapping
// -- it cannot import this package without dragging the store, the retriever, and
// SQLite into a binary whose job is one HTTP POST -- and a hook fails open by
// contract, so drift between the two copies is a silent no-op rather than an
// error: install-hooks would write a command line the CLI rejects, or forward to
// a route that is not there, and the only symptom would be a briefing that
// stopped arriving.
func CommandHookEndpoints() map[string]string {
	out := make(map[string]string)
	for _, profile := range [][]hookSpec{seamlessHooks, codexHooks} {
		for _, hs := range profile {
			if hs.CLIArg != "" {
				out[hs.CLIArg] = hs.Endpoint
			}
		}
	}
	return out
}

// InstalledStatus classifies every event in a client's profile against the
// exact desired definition described by opts. An event is Current only when it
// has exactly one current definition and no stale owned duplicate. A missing or
// empty file yields an empty status and no error.
func InstalledStatus(opts InstallOptions) (InstallStatus, error) {
	client, profile, err := resolveHookProfile(opts.Client)
	if err != nil {
		return InstallStatus{}, fmt.Errorf("hooks.InstalledStatus: %w", err)
	}
	if strings.TrimSpace(opts.SettingsPath) == "" {
		return InstallStatus{}, fmt.Errorf("hooks.InstalledStatus: settings path is required")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return InstallStatus{}, fmt.Errorf("hooks.InstalledStatus: base URL is required")
	}
	if err := validateDefinitionPaths("hooks.InstalledStatus", opts.SeamBin, opts.ConfigPath); err != nil {
		return InstallStatus{}, err
	}
	profile = profileForBaseURL(profile, opts.BaseURL)

	settings, _, err := loadSettings(opts.SettingsPath)
	if err != nil {
		return InstallStatus{}, err
	}
	hooksObj := nestedObject(settings, "hooks")
	var status InstallStatus
	for _, hs := range profile {
		desired := buildEntry(client, hs, opts.BaseURL, opts.APIKey, opts.SeamBin, opts.ConfigPath)
		desiredURL := strings.TrimRight(opts.BaseURL, "/") + hs.Endpoint
		indices, classes := classifiedHookIndices(client, entryArray(hooksObj, hs.Event), desired, hs, desiredURL)
		if len(indices) == 0 {
			continue
		}
		status.Owned = append(status.Owned, hs.Event)
		if len(indices) == 1 && classes[0] == hookDefinitionCurrent {
			status.Current = append(status.Current, hs.Event)
		} else {
			status.Stale = append(status.Stale, hs.Event)
		}
	}
	return status, nil
}

// RecordedCommandPaths returns the seam binary and config path recorded in the
// first seam-shaped command hook of the client's profile in settingsPath.
// Doctor uses it to judge an install against the paths it was installed with
// rather than against the running binary's siblings: `make doctor` from a repo
// checkout must not report a ~/.local/bin install stale. ok is false when the
// file cannot be read or no command hook runs a seam executable.
func RecordedCommandPaths(clientName Client, settingsPath string) (seamBin, configPath string, ok bool) {
	_, profile, err := resolveHookProfile(clientName)
	if err != nil {
		return "", "", false
	}
	settings, _, err := loadSettings(settingsPath)
	if err != nil {
		return "", "", false
	}
	hooksObj := nestedObject(settings, "hooks")
	for _, hs := range profile {
		if hs.CLIArg == "" {
			continue
		}
		for _, entry := range entryArray(hooksObj, hs.Event) {
			entryMap, isMap := entry.(map[string]any)
			if !isMap {
				continue
			}
			handlers, isArr := entryMap["hooks"].([]any)
			if !isArr {
				continue
			}
			for _, h := range handlers {
				handler, isHandler := h.(map[string]any)
				if !isHandler || handler["type"] != "command" {
					continue
				}
				if bin, config, found := commandHookPaths(handler); found {
					return bin, config, true
				}
			}
		}
	}
	return "", "", false
}

// commandHookPaths extracts the executable and --config value from one command
// handler, in both shapes the installer has ever written: exec form (bare
// `command` plus an `args` array) and shell-string form (the whole invocation
// in `command`, optionally behind a legacy SEAMLESS_CONFIG= prefix). Only a
// seam-named executable is accepted, so foreign hooks are never read.
func commandHookPaths(handler map[string]any) (seamBin, configPath string, ok bool) {
	command, isStr := handler["command"].(string)
	if !isStr {
		return "", "", false
	}
	if rawArgs, hasArgs := handler["args"]; hasArgs {
		args, argsOK := hookStringArgs(rawArgs)
		if !argsOK || !isSeamExecutable(command) {
			return "", "", false
		}
		return command, flagValue(args, "--config"), true
	}
	words, splitOK := splitHookCommand(command)
	if !splitOK || len(words) == 0 {
		return "", "", false
	}
	var envConfig string
	if v, found := strings.CutPrefix(words[0], "SEAMLESS_CONFIG="); found {
		envConfig = v
		words = words[1:]
	}
	if len(words) == 0 || !isSeamExecutable(words[0]) {
		return "", "", false
	}
	config := flagValue(words[1:], "--config")
	if config == "" {
		config = envConfig
	}
	return words[0], config, true
}

// flagValue returns the token following the given flag, or "".
func flagValue(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// loadSettings decodes settings.json into a generic map (preserving unknown
// keys) and returns the file mode to preserve. A missing file yields an empty
// map and 0o600 (the file holds a bearer key).
func loadSettings(path string) (map[string]any, os.FileMode, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, 0o600, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("hooks: read %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	// PowerShell 5.1 file tooling (Set-Content and friends) prepends a UTF-8
	// BOM, which encoding/json rejects as garbage before the first token. A
	// user or agent hand-editing hooks.json on Windows is exactly who produces
	// this (seen live on the first Windows Codex install); tolerate it on read
	// -- the post-merge write is BOM-free, which quietly repairs the file.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	settings := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, 0, fmt.Errorf("hooks: parse %s: %w", path, err)
		}
	}
	return settings, mode, nil
}

// nestedObject returns settings[key] as a map, or a fresh one.
func nestedObject(settings map[string]any, key string) map[string]any {
	if v, ok := settings[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// entryArray returns hooksObj[event] as a slice, or nil.
func entryArray(hooksObj map[string]any, event string) []any {
	if v, ok := hooksObj[event].([]any); ok {
		return v
	}
	return nil
}

func buildEntry(client Client, hs hookSpec, baseURL, apiKey, seamBin, configPath string) map[string]any {
	var hook map[string]any
	switch {
	case client == ClientCodex:
		// Codex command hooks are SHELL STRINGS, not exec-form argv (its config
		// schema has no `args` field -- see the versioned Codex contract fixtures).
		// The whole invocation is one `command` string, plus a `command_windows`
		// variant carrying Windows quoting for a Windows install. Both run the same
		// `<seam> hook <event> --config <yaml> --client codex`: no bearer key is
		// written into hooks.json (the CLI loads it from --config at hook time), and
		// --client codex is what selects the Codex payload adapter server-side.
		bin := seamBin
		if bin == "" {
			bin = "seam"
		}
		hook = map[string]any{
			"type":            "command",
			"command":         codexCommand(bin, hs.CLIArg, configPath, posixQuote, posixQuote),
			"command_windows": codexCommand(bin, hs.CLIArg, configPath, winCommandPath, winQuote),
			"timeout":         hs.Timeout,
		}
	case hs.CLIArg != "":
		// Command hook, EXEC form: `command` is the bare seam binary and `args`
		// carries the rest, so Claude Code spawns it directly with no shell. That
		// is the one shape that behaves identically on every OS -- on Windows CC
		// runs a shell-form command hook through PowerShell, where the old POSIX
		// string (a `VAR=value` env prefix plus single-quoting) is not valid
		// syntax. Exec form sidesteps quoting entirely: each arg is passed verbatim.
		//
		// Claude Code pipes the event JSON to stdin and injects stdout; `seam hook
		// <arg>` forwards that to Endpoint and echoes the response, so no bearer
		// key is written into settings.json. The hook fires from any cwd, so the
		// config path is passed as `--config` (exec form carries no environment, so
		// this replaces the old SEAMLESS_CONFIG env prefix) -- otherwise the CLI's
		// cwd-relative config search misses seamless.yaml and it can't authenticate.
		bin := seamBin
		if bin == "" {
			bin = "seam"
		}
		args := []any{"hook", hs.CLIArg}
		if configPath != "" {
			args = append(args, "--config", configPath)
		}
		hook = map[string]any{
			"type":    "command",
			"command": bin,
			"args":    args,
			"timeout": hs.Timeout,
		}
	default:
		hook = map[string]any{
			"type":    "http",
			"url":     strings.TrimRight(baseURL, "/") + hs.Endpoint,
			"timeout": hs.Timeout,
			"headers": map[string]any{"Authorization": "Bearer " + apiKey},
		}
	}
	entry := map[string]any{
		managedMarker: true,
		"hooks":       []any{hook},
	}
	if hs.Matcher != "" {
		entry["matcher"] = hs.Matcher
	}
	return entry
}

// codexCommand builds a Codex command-hook shell string:
//
//	<seam> hook <event> [--config <yaml>] --client codex
//
// binQuote renders the executable in COMMAND position and argQuote the config
// path in ARGUMENT position; the split exists because PowerShell treats the two
// positions differently (see winCommandPath). posixQuote serves both roles in
// `command`; command_windows pairs winCommandPath with winQuote. The `hook
// <event>` token is left unquoted for readability; classification parses the
// full known command shape and verifies the seam executable rather than
// matching tokens.
func codexCommand(seamBin, cliArg, configPath string, binQuote, argQuote func(string) string) string {
	parts := []string{binQuote(seamBin), "hook", cliArg}
	if configPath != "" {
		parts = append(parts, "--config", argQuote(configPath))
	}
	parts = append(parts, "--client", string(ClientCodex))
	return strings.Join(parts, " ")
}

// posixQuote single-quotes s for a POSIX shell, escaping any embedded single
// quote (path -> '...'\”...'). Codex runs command hooks as shell strings, so an
// unquoted path with a space would split into separate argv.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// winQuote double-quotes s for a Windows ARGUMENT position, where both
// PowerShell and cmd.exe accept it. Windows paths cannot contain a double
// quote, so no escaping is needed. Never use it for the executable itself --
// that is winCommandPath's job.
func winQuote(s string) string {
	return `"` + s + `"`
}

// winCommandPath renders the executable of a `command_windows` string. Pinned
// live on Codex CLI 0.146.0 (Windows): command hooks run through PowerShell,
// where a leading double-quoted path is a string EXPRESSION, not an invocation
// -- every hook died with "Unexpected token 'hook'" (exit 1) before seam ever
// ran. A bare path is a command in both PowerShell and cmd.exe (verified live
// in both), so plain paths go unquoted; a path PowerShell cannot take bare
// (space, paren, apostrophe, ...) gets the call operator form `& "<path>"`,
// which PowerShell requires and cmd.exe cannot parse -- acceptable because
// PowerShell is the only shell observed running command_windows.
func winCommandPath(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '\\' || r == '/' || r == ':' || r == '.' || r == '-' || r == '_' || r == '~':
		default:
			return `& "` + s + `"`
		}
	}
	return s
}

// isManaged reports whether e is a hook entry carrying the Seamless managed marker.
func isManaged(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	v, ok := m[managedMarker].(bool)
	return ok && v
}

// sameURL compares two hook URLs ignoring a trailing slash.
func sameURL(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// removeIndices returns arr without the elements at the given (ascending) indices.
func removeIndices(arr []any, idxs []int) []any {
	if len(idxs) == 0 {
		return arr
	}
	drop := make(map[int]bool, len(idxs))
	for _, i := range idxs {
		drop[i] = true
	}
	out := make([]any, 0, len(arr)-len(idxs))
	for i, e := range arr {
		if !drop[i] {
			out = append(out, e)
		}
	}
	return out
}

// matchAction labels a rewrite when at least one owned entry already existed:
// collapsing duplicates, updating our own marked entry, or adopting an unmarked one.
func matchAction(n int, firstClass hookDefinitionClass) string {
	switch {
	case n > 1:
		return "deduped"
	case firstClass == hookDefinitionManagedStale:
		return "updated"
	default:
		return "adopted"
	}
}

func validateDefinitionPaths(op, seamBin, configPath string) error {
	if seamBin != "" && !isSeamExecutable(seamBin) {
		return fmt.Errorf("%s: seam binary must be named seam or seam.exe", op)
	}
	if configPath != "" && !isAbsoluteHookPath(configPath) {
		return fmt.Errorf("%s: config path must be absolute", op)
	}
	return nil
}

// canonicalEqual compares two JSON values ignoring key order and numeric
// formatting, so a re-install that changes nothing does not rewrite the file.
func canonicalEqual(a, b any) bool {
	ca, err1 := json.Marshal(a) // encoding/json sorts map keys
	cb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(ca, cb)
}

// backupOnce copies path to a timestamped sibling the first time Seamless
// changes it, so the true original is preserved. It skips when any Seamless
// backup already exists (never overwriting the original with a modified copy) or
// when the file does not exist yet.
func backupOnce(path string, mode os.FileMode) (string, error) {
	// Glob only fails on a malformed pattern; an unreadable dir yields no
	// matches, which correctly reads as "no backup yet" and makes one.
	if matches, _ := filepath.Glob(path + ".seamless-bak-*"); len(matches) > 0 { //nolint:errcheck
		return "", nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("hooks: backup read: %w", err)
	}
	backup := path + ".seamless-bak-" + time.Now().UTC().Format("20060102-150405")
	// The backup is a verbatim copy, so if the file being replaced held a
	// bearer key this copy does too -- and it is the one nothing will ever
	// rewrite. It gets the same clamp as the live file.
	if err := os.WriteFile(backup, data, secretSafeMode(mode, data)); err != nil {
		return "", fmt.Errorf("hooks: write backup: %w", err)
	}
	return backup, nil
}

// secretSafeMode narrows mode to owner-only when data carries a bearer
// credential, and otherwise leaves it exactly as found.
//
// This exists because loadSettings preserves an existing file's mode, and
// Claude Code commonly creates ~/.claude/settings.json as 0644 -- so writing
// our `Authorization: Bearer <key>` header into it would persist the daemon's
// sole credential world-readable. On a shared machine that hands any other
// local account the ability to read and write the operator's entire memory
// corpus. (Audit L3.)
//
// The test is the file's content rather than "did *we* just add a key", so it
// also covers the backup copy above and the uninstall path, neither of which
// knows the key. A masking AND is deliberate: this may only ever narrow
// permissions, never widen a deliberately stricter file.
func secretSafeMode(mode os.FileMode, data []byte) os.FileMode {
	if !bytes.Contains(data, []byte("Bearer ")) {
		return mode
	}
	return mode & 0o600
}

// writeSettings atomically writes the settings map, sorted-key indented, with a
// trailing newline, preserving the file mode -- except that a file carrying a
// bearer credential is clamped to owner-only (see secretSafeMode).
func writeSettings(path string, settings map[string]any, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("hooks: mkdir %s: %w", dir, err)
		}
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("hooks: marshal settings: %w", err)
	}
	out = append(out, '\n')
	if err := files.AtomicWrite(path, out, secretSafeMode(mode, out)); err != nil {
		return fmt.Errorf("hooks: write settings: %w", err)
	}
	return nil
}
