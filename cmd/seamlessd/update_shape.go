package main

// The shape of an install, as an unattended update hands it to the installer:
// where the binaries live and which agent clients and skills the owner already
// has. All of it is observed, never chosen -- an update re-runs the installer
// over exactly what is there, and never adds a client or a skill the owner
// does not have (or removed). Every read here is over an injected shapeEnv, so
// a test stands any layout up under a temp HOME and never touches the live
// ~/.claude, ~/.codex or ~/.seamless.

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/hooks"
	agentskills "github.com/arctop/seamless/internal/skills"
	"github.com/arctop/seamless/internal/update"
)

// shapeEnv is the slice of the machine the shape is read from.
type shapeEnv struct {
	goos string
	home string
	// environ is the environment the installer child will inherit: CODEX_HOME
	// and APPDATA are read from it, so the shape is observed where the
	// installer's own `seamlessd install-hooks` will look.
	environ  []string
	sameFile func(a, b string) bool
	// claudeManagedDir is Claude Code's managed settings directory
	// (claudeManagedSettingsDir; "" for none). Seamless hooks there wire Claude
	// Code as surely as the user file's do.
	claudeManagedDir string
}

// lookupEnvIn returns key's value in environ, the last one winning as in a
// real environment; Windows names are case-insensitive. A value that is empty
// after trimming counts as unset, which is how both installers read their
// knobs (${NAME:-} in sh, `if ($env:NAME)` in PowerShell).
func lookupEnvIn(goos string, environ []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !sameEnvName(goos, k, key) {
			continue
		}
		val, found = v, strings.TrimSpace(v) != ""
	}
	return val, found
}

// sameEnvName compares two environment variable names, case-insensitively on
// Windows.
func sameEnvName(goos, a, b string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// expandIn expands a leading ~ against the env's home, as expandHome does
// against the process's.
func (e shapeEnv) expandIn(p string) string {
	if p == "~" {
		return e.home
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(e.home, rest)
	}
	return p
}

// codexHome is $CODEX_HOME as the installer child will see it, "" when unset.
func (e shapeEnv) codexHome() string {
	v, ok := lookupEnvIn(e.goos, e.environ, "CODEX_HOME")
	if !ok {
		return ""
	}
	return e.expandIn(strings.TrimSpace(v))
}

// claudeSettingsPath is the Claude Code settings file install-hooks writes by
// default (its --settings flag).
func (e shapeEnv) claudeSettingsPath() string {
	return filepath.Join(e.home, ".claude", "settings.json")
}

// codexHooksPath is the Codex hooks file install-hooks writes by default:
// $CODEX_HOME/hooks.json when CODEX_HOME is set, else ~/.codex/hooks.json
// (defaultCodexHooksPath, over the child's environment). The updater keeps
// CODEX_HOME in the installer's environment, so observing here is observing
// the file the installer will rewrite.
func (e shapeEnv) codexHooksPath() string {
	if home := e.codexHome(); home != "" {
		return filepath.Join(home, "hooks.json")
	}
	return filepath.Join(e.home, ".codex", "hooks.json")
}

// seamIn is the seam CLI the installer puts in dir.
func (e shapeEnv) seamIn(dir string) string {
	if e.goos == "windows" {
		return filepath.Join(dir, "seam.exe")
	}
	return filepath.Join(dir, "seam")
}

// errShapeMismatch is an install whose parts disagree about where it lives.
// An update refuses it: the installer would put the new binaries in one place
// while something keeps running the old ones from another.
var errShapeMismatch = errors.New("the install's parts disagree about where it lives")

// installDirFor is SEAMLESS_INSTALL_DIR for an update: the directory of the
// program the installer-written service runs (the service probe's Program),
// which must also hold this executable (exe, symlinks resolved), and be where
// the seam command hooks recorded for Claude Code and Codex run seam from
// (hooks.RecordedCommandPaths). Any disagreement is errShapeMismatch.
// Directories are compared as files (sameFile), never as strings.
func installDirFor(env shapeEnv, svc update.ServiceProbe, exe string) (string, error) {
	if svc.Program == "" || !filepath.IsAbs(svc.Program) {
		return "", fmt.Errorf("%w: the service %s names no absolute program", errShapeMismatch, svc.Path)
	}
	dir := filepath.Dir(svc.Program)
	if exe == "" || !env.sameFile(dir, filepath.Dir(exe)) {
		return "", fmt.Errorf("%w: the service runs %s, but this seamlessd is %s", errShapeMismatch, svc.Program, exe)
	}
	for _, c := range []struct {
		client hooks.Client
		name   string
		path   string
	}{
		{hooks.ClientClaudeCode, "Claude Code", env.claudeSettingsPath()},
		{hooks.ClientCodex, "Codex", env.codexHooksPath()},
	} {
		seamBin, _, ok := hooks.RecordedCommandPaths(c.client, c.path)
		if !ok || !strings.ContainsAny(seamBin, `/\`) {
			continue // nothing recorded, or a bare name resolved through PATH
		}
		seamBin = env.expandIn(seamBin)
		if !env.sameFile(dir, filepath.Dir(seamBin)) {
			return "", fmt.Errorf("%w: the %s hooks in %s run %s, not the seam in %s",
				errShapeMismatch, c.name, c.path, seamBin, dir)
		}
	}
	return dir, nil
}

// The installer's client targets (SEAMLESS_CLIENT), in its install order:
// docs/install compose_clients and install.ps1 ConvertTo-ClientList write the
// Claude surfaces first, then Codex.
const (
	shapeClaude        = "claude"
	shapeClaudeDesktop = "claude-desktop"
	shapeCodex         = "codex"
)

// observeClients lists the agent clients this install already wires, as
// SEAMLESS_CLIENT targets: Claude Code when ~/.claude/settings.json holds
// Seamless-owned hooks (hooks.InstalledStatus: current or stale), or when
// Claude Code's managed settings do -- re-running install-hooks there writes no
// hook the managed file carries, but it keeps the skills and the MCP
// registration current, which an update must not drop -- Codex the
// same for its hooks file, and the Claude app when its desktop config holds a
// seamless MCP entry that install-hooks would keep or repair (exact or owned
// drift; a foreign entry under the name is not ours to touch). A client whose
// file cannot be read is left out -- an update never adds a client -- and
// named in notes, for the attempt log.
func observeClients(env shapeEnv, cfg config.Config, dir, configPath string) (clients, notes []string) {
	seamBin := env.seamIn(dir)
	owns := func(client hooks.Client, name, path string) bool {
		status, err := hooks.InstalledStatus(hooks.InstallOptions{
			Client: client, SettingsPath: path, BaseURL: cfg.ServerURL(),
			APIKey: cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: configPath,
		})
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s left out: %v", name, err))
			return false
		}
		return len(status.Owned) > 0
	}
	claudeWired := owns(hooks.ClientClaudeCode, "Claude Code", env.claudeSettingsPath())
	if !claudeWired && env.claudeManagedDir != "" {
		managed := inspectClaudeManagedHooks(env.claudeManagedDir, hooks.InstallOptions{
			Client: hooks.ClientClaudeCode, BaseURL: cfg.ServerURL(),
			APIKey: cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: configPath,
		})
		claudeWired = len(managed.sources) > 0
		notes = append(notes, managed.errs...)
	}
	if claudeWired {
		clients = append(clients, shapeClaude)
	}
	if ok, note := desktopRegistered(env, seamBin, configPath); ok {
		clients = append(clients, shapeClaudeDesktop)
	} else if note != "" {
		notes = append(notes, note)
	}
	if owns(hooks.ClientCodex, "Codex", env.codexHooksPath()) {
		clients = append(clients, shapeCodex)
	}
	return clients, notes
}

// desktopRegistered reports whether the Claude app's desktop config holds a
// seamless MCP entry install-hooks owns, with a note when the file exists but
// cannot be judged.
func desktopRegistered(env shapeEnv, seamBin, configPath string) (bool, string) {
	appData, _ := lookupEnvIn(env.goos, env.environ, "APPDATA")
	path, err := claudeDesktopConfigPathFor(env.goos, env.home, appData)
	if err != nil {
		return false, "" // no Claude app on this OS, or no %APPDATA%
	}
	_, servers, _, err := loadClaudeDesktopConfig(path)
	if err != nil {
		return false, fmt.Sprintf("Claude app left out: %v", err)
	}
	raw, ok := servers[seamlessMCPName]
	if !ok {
		return false, ""
	}
	entry, err := parseClaudeDesktopMCPServer(raw)
	if err != nil {
		return false, fmt.Sprintf("Claude app left out: %s: %v", path, err)
	}
	want, err := desiredClaudeDesktopMCPServer(seamBin, configPath)
	if err != nil {
		return false, fmt.Sprintf("Claude app left out: %v", err)
	}
	if class, _ := classifyClaudeDesktopMCP(entry, want); class == mcpRegIncompatible {
		return false, fmt.Sprintf("Claude app left out: the %q entry in %s is not Seamless's", seamlessMCPName, path)
	}
	return true, ""
}

// skillClients maps the hook clients among SEAMLESS_CLIENT targets to the
// skill homes install-hooks delivers into. The Claude app gets no skills.
func skillClients(clients []string) []agentskills.Client {
	var out []agentskills.Client
	for _, c := range clients {
		switch c {
		case shapeClaude:
			out = append(out, agentskills.ClientClaude)
		case shapeCodex:
			out = append(out, agentskills.ClientCodex)
		}
	}
	return out
}

// skillOptOuts names the maintained skills an update must tell install-hooks
// to leave alone: every skill that is not delivered (agentskills.Delivered)
// to EVERY wired hook client.
//
// The rule follows from two facts. An update never adds a skill the owner
// removed, and the only lever is an opt-out knob (SEAMLESS_NO_ONBOARD_SKILL,
// SEAMLESS_NO_RESEARCH_SKILL) that install-hooks reads process-wide, for every
// client at once. So when one wired client has a skill and another does not,
// refreshing the first would add it to the second; the opt-out instead leaves
// both exactly as they are (the first keeps its copy, unrefreshed until a
// manual update or install-hooks). A skill no wired client has is opted out
// too, and a skill whose delivery cannot be read is opted out and noted: the
// safe reading of "unknown" is "do not add".
func skillOptOuts(env shapeEnv, clients []string) (skip, notes []string) {
	homes := skillClients(clients)
	if len(homes) == 0 {
		return nil, nil // no hook client: install-hooks installs no skills
	}
	opts := agentskills.Options{HomeDir: env.home, CodexHome: env.codexHome()}
	for _, name := range agentskills.Published() {
		for _, c := range homes {
			delivered, err := agentskills.Delivered(c, opts, name)
			if err != nil {
				notes = append(notes, fmt.Sprintf("skill %s left alone: %v", name, err))
			}
			if err != nil || !delivered {
				skip = append(skip, name)
				break
			}
		}
	}
	return skip, notes
}

// skillOptOutKnob is the installer knob that opts out of skill name:
// seam-onboard -> SEAMLESS_NO_ONBOARD_SKILL. The name is derived, not listed
// a second time, and must be one installerKnobs carries.
func skillOptOutKnob(name string) (string, error) {
	short, ok := strings.CutPrefix(name, "seam-")
	knob := "SEAMLESS_NO_" + strings.ToUpper(strings.ReplaceAll(short, "-", "_")) + "_SKILL"
	if !ok || !slices.Contains(installerKnobs, knob) {
		return "", fmt.Errorf("skill %q has no installer opt-out knob (looked for %s)", name, knob)
	}
	return knob, nil
}

// installShape is an install as an update observed it.
type installShape struct {
	// dir is where the binaries live (SEAMLESS_INSTALL_DIR).
	dir string
	// clients are the SEAMLESS_CLIENT targets already wired, in install
	// order; none means SEAMLESS_NO_HOOKS.
	clients []string
	// skipSkills are the skills to opt out of (skillOptOuts).
	skipSkills []string
	// notes say what was left out and why, for the attempt log.
	notes []string
}

// observeInstallShape reads the whole shape of an install for an unattended
// update. Only a disagreement about the install dir is an error: the rest
// degrades toward changing less.
func observeInstallShape(env shapeEnv, cfg config.Config, configPath string, svc update.ServiceProbe, exe string) (installShape, error) {
	dir, err := installDirFor(env, svc, exe)
	if err != nil {
		return installShape{}, err
	}
	s := installShape{dir: dir}
	var notes []string
	s.clients, notes = observeClients(env, cfg, dir, configPath)
	s.notes = append(s.notes, notes...)
	s.skipSkills, notes = skillOptOuts(env, s.clients)
	s.notes = append(s.notes, notes...)
	return s, nil
}

// knobs renders the shape as installer knobs: the install dir, the observed
// clients or SEAMLESS_NO_HOOKS when there are none, and one opt-out per
// skipped skill.
func (s installShape) knobs() (map[string]string, error) {
	k := map[string]string{"SEAMLESS_INSTALL_DIR": s.dir}
	if len(s.clients) == 0 {
		k["SEAMLESS_NO_HOOKS"] = "1"
	} else {
		k["SEAMLESS_CLIENT"] = strings.Join(s.clients, ",")
	}
	for _, name := range s.skipSkills {
		knob, err := skillOptOutKnob(name)
		if err != nil {
			return nil, err
		}
		k[knob] = "1"
	}
	return k, nil
}

// releaseKnobs pins the installer to r: SEAMLESS_VERSION always, and
// SEAMLESS_CHECKSUMS_SHA256 when the updater verified r's checksums.txt.
func releaseKnobs(r releaseAssets) map[string]string {
	k := map[string]string{"SEAMLESS_VERSION": r.version.String()}
	if r.pin != "" {
		k["SEAMLESS_CHECKSUMS_SHA256"] = r.pin
	}
	return k
}

// mergeKnobs combines knob sets; a later set wins a name both carry.
func mergeKnobs(sets ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, s := range sets {
		for k, v := range s {
			out[k] = v
		}
	}
	return out
}

// knobEnv renders knobs as KEY=VALUE entries in installerKnobs order. A name
// installerKnobs does not list is refused: that list is the contract both
// installers are held to (installer_knobs_test.go), and a knob outside it is
// one no installer is known to read.
func knobEnv(knobs map[string]string) ([]string, error) {
	for name := range knobs {
		if !slices.Contains(installerKnobs, name) {
			return nil, fmt.Errorf("installer knob %s is not in installerKnobs", name)
		}
	}
	out := make([]string, 0, len(knobs))
	for _, name := range installerKnobs {
		if v, ok := knobs[name]; ok {
			out = append(out, name+"="+v)
		}
	}
	return out, nil
}

// unattendedInstallerEnv is the environment an unattended update runs the
// installer in: the updater's own inherited environment with every SEAMLESS_*
// variable dropped (case-insensitively on Windows), plus exactly knobs.
//
// Inherited minus SEAMLESS_*, not a whitelist. The updater's environment is
// already a service manager's minimal one, by way of how the daemon starts it
// (update_spawn.go): on darwin it inherits the daemon's, which is launchd's;
// on Linux its transient unit starts from the systemd user manager's, not the
// daemon's, plus the proxy and CA variables the spawner forwards; on Windows
// the detached child inherits the daemon's, and the fallback update task's
// comes from the owner's logon session, like the daemon's. The installers
// need more of it than a list would keep working: PATH for curl, tar, shasum
// and launchctl; HOME; TMPDIR for mktemp; XDG_RUNTIME_DIR or
// DBUS_SESSION_BUS_ADDRESS for systemctl --user; the proxy and CA-bundle
// variables curl and Invoke-WebRequest honor; CODEX_HOME, which install-hooks
// reads; and on Windows SystemRoot, USERPROFILE, APPDATA, LOCALAPPDATA, TEMP,
// PSModulePath, PROCESSOR_ARCHITECTURE and the rest PowerShell and its cmdlets
// expect.
// Dropping SEAMLESS_* is what matters: it removes every knob the installer
// would otherwise act on (SEAMLESS_CONFIG, SEAMLESS_SERVER_URL,
// SEAMLESS_MCP_API_KEY, SEAMLESS_NO_SERVICE, ...), so the only ones it sees
// are the ones the updater chose.
func unattendedInstallerEnv(goos string, inherited []string, knobs map[string]string) ([]string, error) {
	kv, err := knobEnv(knobs)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(inherited)+len(kv))
	for _, e := range inherited {
		name, _, _ := strings.Cut(e, "=")
		if isSeamlessVar(goos, name) {
			continue
		}
		out = append(out, e)
	}
	return append(out, kv...), nil
}

// isSeamlessVar reports whether an environment variable name is a SEAMLESS_*
// one, case-insensitively on Windows.
func isSeamlessVar(goos, name string) bool {
	if goos == "windows" {
		name = strings.ToUpper(name)
	}
	return strings.HasPrefix(name, "SEAMLESS_")
}

// attendedInstallerEnv is the environment an attended `seamlessd update` runs
// the installer in: the owner's environment as is -- the knobs they set win --
// plus each of knobs they did not set (lookupEnvIn: empty is unset). A name in
// force replaces the owner's value instead: a rollback must run with the
// rollback target's SEAMLESS_VERSION and checksums pin whatever the owner
// pinned for the update.
func attendedInstallerEnv(goos string, inherited []string, knobs map[string]string, force ...string) ([]string, error) {
	add := map[string]string{}
	for name, v := range knobs {
		if _, set := lookupEnvIn(goos, inherited, name); set && !slices.Contains(force, name) {
			continue
		}
		add[name] = v
	}
	kv, err := knobEnv(add)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(inherited)+len(kv))
	for _, e := range inherited {
		name, _, _ := strings.Cut(e, "=")
		if slices.ContainsFunc(kv, func(k string) bool {
			knob, _, _ := strings.Cut(k, "=")
			return sameEnvName(goos, knob, name)
		}) {
			continue // replaced, or an empty value the knob fills
		}
		out = append(out, e)
	}
	return append(out, kv...), nil
}
