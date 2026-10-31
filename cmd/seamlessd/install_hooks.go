package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/hooks"
	agentskills "github.com/arctop/seamless/internal/skills"
	"github.com/arctop/seamless/internal/store"
)

// runInstallHooks wires an agent client to Seamless in one command: it installs
// the hooks into that client's config file and, unless --mcp=false, registers
// the MCP server through its management surface. --client selects which: claude
// (~/.claude/settings.json + `claude mcp add`), claude-desktop (the Claude app
// chat surface -- desktop-config MCP bridge only, no hooks or skills), codex
// ($CODEX_HOME/hooks.json + `codex mcp add ... seam mcp-proxy`), a comma list
// of those, all (every target this platform can host), or detect (the default:
// the targets present on this machine, the same selection the curl installer
// makes; an error when none are, so nothing is wired without an explicit
// choice). For the P2 dogfood, point --settings at THIS repo's project-scoped
// .claude/settings.json so v2 hooks fire only here.
func runInstallHooks(args []string) error {
	fs := flag.NewFlagSet("install-hooks", flag.ContinueOnError)
	clientFlag := fs.String("client", "detect", "which agent client(s) to install for: claude|claude-desktop|codex, a comma list of those, or all|detect (claude-desktop wires only the Claude app chat surface's MCP bridge)")
	settings := fs.String("settings", "~/.claude/settings.json", "Claude Code settings.json to install into")
	codexHooksFlag := fs.String("codex-hooks", "", "Codex hooks.json to install into (default $CODEX_HOME/hooks.json, else ~/.codex/hooks.json)")
	desktopConfigFlag := fs.String("desktop-config", "", "Claude desktop app claude_desktop_config.json to register the MCP bridge in (default: the app's per-OS location)")
	urlFlag := fs.String("url", "", "base URL of seamlessd for this run only (default derived from config); does not change the config file")
	serverURLFlag := fs.String("server-url", "", "install as a CLIENT of the seamlessd at this base `URL`: writes role: client, server_url and mcp.api_key to ~/.config/seamless/seamless.yaml on first run")
	apiKeyFlag := fs.String("api-key", "", "that server's bearer `KEY`, for --server-url (default $SEAMLESS_MCP_API_KEY)")
	seamFlag := fs.String("seam", "", "path to the seam CLI for command hooks (default: sibling of this binary, else PATH)")
	mcpFlag := fs.Bool("mcp", true, "register MCP through claude/codex mcp add (prints the Codex app fallback when the management CLI is absent)")
	skillsFlag := fs.Bool("skills", true, "install the client's seam-onboard and seam-research skills")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clientSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "client" {
			clientSet = true
		}
	})
	targets, err := resolveInstallTargets(*clientFlag, clientSet)
	if err != nil {
		return fmt.Errorf("seamlessd.install-hooks: %w", err)
	}
	// The chat surface is MCP-only, so selecting only it with --mcp=false
	// leaves nothing this run could install; present-but-contradictory flags
	// are an error, never a silent no-op. In a wider selection the hook
	// clients still have work, so the desktop block just notes the skip.
	if !*mcpFlag && len(targets) == 1 && targets[0] == targetClaudeDesktop {
		return errors.New("seamlessd.install-hooks: --client claude-desktop with --mcp=false leaves nothing to install (the Claude app chat surface has no hooks or skills)")
	}
	// --api-key only ever names the key of the server --server-url points at. On
	// its own it would be silently ignored, which is the one thing a flag
	// carrying a credential must never be.
	if strings.TrimSpace(*apiKeyFlag) != "" && strings.TrimSpace(*serverURLFlag) == "" {
		return errors.New("seamlessd.install-hooks: --api-key applies to --server-url (a client install); a server reads its own key from its config file")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.install-hooks: %w", err)
	}
	// --server-url makes this machine a CLIENT of a daemon running elsewhere, so
	// the bootstrap writes role/server_url/key instead of generating a key for a
	// local daemon that will never run here. Either way the config file is the
	// owner's: neither call edits an existing one (memory
	// first-run-key-bootstrap-rules).
	var wrote string
	if strings.TrimSpace(*serverURLFlag) != "" {
		cfg, wrote, err = config.EnsureClientConfig(*serverURLFlag, *apiKeyFlag)
		if err != nil {
			return fmt.Errorf("seamlessd.install-hooks: %w", err)
		}
		if wrote != "" {
			fmt.Printf("%s wrote the client config %s\n", green("first run:"), dim("-> "+tildePath(wrote)))
		}
	} else {
		cfg, wrote, err = config.EnsureAPIKey(cfg)
		if err != nil {
			return fmt.Errorf("seamlessd.install-hooks: %w", err)
		}
		if wrote != "" {
			fmt.Printf("%s generated mcp.api_key %s\n", green("first run:"), dim("-> "+tildePath(wrote)))
		}
	}
	// A peer_auth socket client needs no key: the daemon names it by its uid.
	if strings.TrimSpace(cfg.MCP.APIKey) == "" && cfg.SocketPath() == "" {
		src := cfg.SourcePath()
		if src == "" {
			src = "the environment (SEAMLESS_MCP_API_KEY)"
		}
		return fmt.Errorf("seamlessd.install-hooks: mcp.api_key is empty in %s; set it (openssl rand -hex 32)", src)
	}
	baseURL := *urlFlag
	if baseURL == "" {
		baseURL = cfg.HookBaseURL()
	}
	// What the installers describe the daemon by (baseURL) and where an HTTP
	// request to it goes (dialURL) differ only for a socket: unix:///path is not
	// a URL a request can be built on.
	dialURL := baseURL
	if hooks.IsSocketBaseURL(baseURL) {
		dialURL = cfg.ServerURL()
	}
	seamBin := resolveSeamBin(*seamFlag)
	if _, lookErr := exec.LookPath(seamBin); lookErr != nil {
		fmt.Printf("%s seam CLI not found (%q); command hooks fail until it is installed\n%s%s\n",
			yellow("warning:"), seamBin, fieldCont, dim("go install github.com/arctop/seamless/cmd/seam@latest"))
	}
	configPath := absConfigPath(cfg.SourcePath())

	// The chat surface installs no skills, so its options load only when a hook
	// client is in the selection -- a desktop-only run must not depend on them.
	var skillOpts agentskills.Options
	if *skillsFlag && len(hookClientsFor(targets)) > 0 {
		skillOpts, err = agentskills.OptionsFromEnvironment()
		if err != nil {
			return fmt.Errorf("seamlessd.install-hooks: %w", err)
		}
		// A client has no database here, so the stored override lives on the
		// server and is read over HTTP; a server reads its own row.
		if cfg.IsClient() {
			skillOpts.DisabledSkills = disabledFeatureSkills(clientFeatures(cfg, dialURL))
		} else {
			skillOpts.DisabledSkills = disabledFeatureSkills(effectiveFeatures(cfg))
		}
	}

	for _, target := range targets {
		var client hooks.Client
		switch target {
		case targetClaudeDesktop:
			// No hooks.Client behind this target (see the
			// claude-chat-surface-client-model decision): only the
			// desktop-config MCP registration, and no skills below.
			fmt.Printf("\n%s\n", bold("Claude app (chat)"))
			if !*mcpFlag {
				fieldRow("mcp", dim("skipped (--mcp=false); the chat surface has nothing else to install"))
				continue
			}
			if err := registerClaudeDesktopMCP(*desktopConfigFlag, seamBin, configPath); err != nil {
				return fmt.Errorf("seamlessd.install-hooks: %w", err)
			}
			continue
		case targetCodex:
			client = hooks.ClientCodex
			if err := installCodexHooks(cfg, *codexHooksFlag, baseURL, seamBin, configPath, *mcpFlag); err != nil {
				return fmt.Errorf("seamlessd.install-hooks: %w", err)
			}
		default:
			client = hooks.ClientClaudeCode
			if err := installClaudeHooks(cfg, *settings, baseURL, seamBin, configPath, *mcpFlag); err != nil {
				return fmt.Errorf("seamlessd.install-hooks: %w", err)
			}
		}
		if *skillsFlag {
			// Skills are an optional convenience layer, so they degrade like the
			// missing-seam-binary and MCP-registration steps rather than aborting.
			// install-hooks runs from `curl | sh` under `set -eu` before the service
			// is registered: a non-writable ~/.claude/skills must not cost the user
			// their daemon, nor stop a later client in the --client all loop.
			if err := installClientSkills(client, skillOpts); err != nil {
				fmt.Printf("%s skills for %s: %v\n%s%s\n", yellow("warning:"), client, err,
					fieldCont, dim("set SEAMLESS_NO_ONBOARD_SKILL=1 / SEAMLESS_NO_RESEARCH_SKILL=1 to skip, or rerun install-hooks --skills"))
			}
		}
	}
	// Close a client install by naming what was wired, since the usual next
	// steps (a service, a local database, a console on this machine) are all
	// somewhere else.
	if cfg.IsClient() {
		fmt.Printf("\n%s %s %s\n", dim("client of"), baseURL, dim("(no daemon or service on this machine)"))
	}
	return nil
}

// effectiveFeatures resolves the optional-features config the way every live
// surface does: the file/env base carried on cfg, with the stored override row
// (the console toggle, or the grandfather migration) decoded over it. Skills are
// written into the client's config directory, so install-hooks is the one moment
// Seamless can bring them in line with what the owner has actually switched on.
//
// install-hooks runs outside the daemon, so it reads the row straight from the
// database -- but only when one already exists: a machine that has never run
// seamlessd has no database and therefore no override, and creating one here
// would be a side effect of wiring up a client. It opens an existing one with
// store.OpenExisting and never migrates it: the curl installer runs install-hooks
// BEFORE it restarts the service (the config bootstrap has to come first), so on
// an upgrade the database is still being served by the OLD daemon, and migrating
// it here would change the schema under a running binary that does not know the
// new one. The new daemon migrates it on its first start.
//
// Reading without migrating is exact as long as no pending migration writes the
// row, and only migration 022 ever has: a database from before it (releases up
// to v0.4.9) gets 022's one-time seeding mirrored read-only
// (featuresGrandfatherPending), or upgrading from such a release would remove
// seam-research from an installation the new daemon is about to keep research
// on for.
//
// It is failure-soft by contract, like every other reader of this row: an
// unreadable database or a corrupt row warns and falls back to the file/env base
// rather than costing the user their install.
func effectiveFeatures(cfg config.Config) config.Features {
	dbPath := cfg.DBPath()
	if _, err := os.Lstat(dbPath); err != nil {
		return cfg.Features // no database yet -> no stored override
	}
	db, err := store.OpenExisting(dbPath)
	if err != nil {
		fmt.Printf("%s cannot open %s to read the feature toggles (%v)\n%s%s\n",
			yellow("warning:"), tildePath(dbPath), err, fieldCont, dim("using the file/env features config"))
		return cfg.Features
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	effective, overridden, err := store.FeaturesConfig(ctx, db, cfg.Features)
	if err == nil && !overridden {
		var seeds bool
		seeds, err = featuresGrandfatherPending(ctx, db)
		if seeds {
			effective.Research = true
		}
	}
	if err != nil {
		fmt.Printf("%s cannot read the stored feature toggles (%v)\n%s%s\n",
			yellow("warning:"), err, fieldCont, dim("using the file/env features config"))
		return cfg.Features
	}
	return effective
}

// featuresGrandfatherVersion is migration 022 (022_features_grandfather.sql),
// the only migration that writes the stored features override.
const featuresGrandfatherVersion = 22

// featuresGrandfatherPending reports whether migration 022 has yet to run on db
// and will seed research on when it does. The query is the migration's whole
// predicate -- the database holds trials and no override row exists -- read
// instead of applied; an applied migration is never edited, so this copy cannot
// drift from it. Both tables date from migration 001, so the probe is safe on
// every schema an installed seamlessd can have left.
func featuresGrandfatherPending(ctx context.Context, db *sql.DB) (bool, error) {
	applied, err := store.SchemaVersion(db)
	if err != nil {
		return false, err
	}
	if applied >= featuresGrandfatherVersion {
		return false, nil
	}
	const q = `SELECT EXISTS (SELECT 1 FROM trials)
	           AND NOT EXISTS (SELECT 1 FROM settings WHERE key = ?)`
	var seeds bool
	if err := db.QueryRowContext(ctx, q, store.SettingFeaturesConfig).Scan(&seeds); err != nil {
		return false, fmt.Errorf("probe migration %d's grandfather clause: %w", featuresGrandfatherVersion, err)
	}
	return seeds, nil
}

// disabledFeatureSkills names the client-side skills whose optional feature is
// off. The names come from the registry (Feature.Skill), never from a literal
// here, so a future optional feature that ships a skill is covered the moment it
// is registered.
func disabledFeatureSkills(effective config.Features) []string {
	var names []string
	for _, feature := range features.Registry() {
		if feature.Skill == "" || feature.Enabled(effective) {
			continue
		}
		names = append(names, feature.Skill)
	}
	return names
}

// installClientSkills follows the same client selection as hooks and MCP. The
// embedded assets make this work from an installed release binary; no checkout
// or archive path is needed at runtime.
func installClientSkills(client hooks.Client, opts agentskills.Options) error {
	skillClient, err := agentSkillClient(client)
	if err != nil {
		return err
	}
	result, err := agentskills.Install(skillClient, opts)
	if err != nil {
		return err
	}
	printSkillAction("onboard", agentskills.OnboardName, result.Root, result.Onboard,
		"SEAMLESS_NO_ONBOARD_SKILL")
	printSkillAction("research", agentskills.ResearchName, result.Root, result.Research,
		"SEAMLESS_NO_RESEARCH_SKILL")
	return nil
}

func agentSkillClient(client hooks.Client) (agentskills.Client, error) {
	switch client {
	case hooks.ClientClaudeCode:
		return agentskills.ClientClaude, nil
	case hooks.ClientCodex:
		return agentskills.ClientCodex, nil
	default:
		return "", fmt.Errorf("invalid skill client %q: valid values are claude, codex", client)
	}
}

// featureOffHint explains the two skill outcomes an optional feature produces:
// the package is absent because the feature is off, and here is where it goes
// back on. The skill is not deleted data -- it is regenerated by the next
// install-hooks run once the feature is enabled.
const featureOffHint = "the feature is off (console Settings -> Features, or SEAMLESS_FEATURES_*)"

func printSkillAction(label, name, root string, action agentskills.Action, skipEnv string) {
	dst := tildePath(filepath.Join(root, name))
	switch action {
	case agentskills.ActionInstalled:
		fieldRow(label, green("installed")+dim("  · "+dst))
	case agentskills.ActionUpdated:
		fieldRow(label, green("updated")+dim("  · "+dst))
	case agentskills.ActionUnchanged:
		fieldRow(label, dim("unchanged  · "+dst))
	case agentskills.ActionAlreadyDelivered:
		fieldRow(label, dim("already used; one-shot skill not reinstalled"))
	case agentskills.ActionSkipped:
		fieldRow(label, dim("skipped ("+skipEnv+")"))
	case agentskills.ActionRemoved:
		fieldRow(label, yellow("removed")+dim("  · "+dst+"; "+featureOffHint))
	case agentskills.ActionDisabled:
		fieldRow(label, dim("not installed; "+featureOffHint))
	default:
		fieldRow(label, yellow("unknown install result "+string(action)))
	}
}

// runInstallSummary prints the final "Seamless" block that closes `make install`:
// the build version and where the binaries and config landed. It is invoked by
// the Makefile (which owns those paths), not typed by a user, so it stays out of
// usage(). Missing flags simply drop their row, so it degrades cleanly.
func runInstallSummary(args []string) error {
	fs := flag.NewFlagSet("install-summary", flag.ContinueOnError)
	binDir := fs.String("bin-dir", "", "directory the binaries were installed into")
	configPath := fs.String("config", "", "path to the active seamless.yaml")
	bins := fs.String("bins", "seamlessd,seam", "comma-separated installed binary filenames")
	if err := fs.Parse(args); err != nil {
		return err
	}
	printInstallSummary(*binDir, *configPath, splitBins(*bins))
	return nil
}

// printInstallSummary renders the install-complete block in the same styled
// shape as the per-client blocks above it: a bold header and aligned label rows.
func printInstallSummary(binDir, configPath string, bins []string) {
	fmt.Printf("\n%s\n", bold("Seamless"))
	fieldRow("version", buildVersion())
	if binDir != "" {
		names := strings.Join(bins, ", ")
		if len(bins) > 1 {
			names = "{" + names + "}"
		}
		fieldRow("bin", tildePath(binDir)+"/"+names)
	}
	if configPath != "" {
		fieldRow("config", tildePath(configPath))
	}
	// A quiet call-to-action footer: the two doors to "what now?", the actionable
	// anchors lifted out of the dim guidance so the eye lands on them.
	fmt.Printf("\n  %s %s %s %s\n",
		dim("Next:"), green("seam --help"), dim("or"), green("https://thereisnospoon.org/docs"))
}

// splitBins parses the comma-separated --bins list into filenames, dropping
// blanks so a trailing comma or empty value yields no phantom entry.
func splitBins(raw string) []string {
	var out []string
	for b := range strings.SplitSeq(raw, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// installTarget is the installer's selection currency: what --client, the
// interactive menu, and the curl installers choose between. It is deliberately
// NOT hooks.Client (see the claude-chat-surface-client-model decision): the
// Claude app chat surface is an install target with no hook client behind it,
// and widening hooks.Client to carry the menu would widen the hook-request
// parse boundary to a client that can never legitimately send a hook.
type installTarget string

const (
	targetClaudeCode    installTarget = "claude"
	targetClaudeDesktop installTarget = "claude-desktop"
	targetCodex         installTarget = "codex"
)

// detectedTargets is one machine probe handed to the selection functions, so
// parsing and prompting stay pure and table-testable.
type detectedTargets struct {
	claude, codex, desktop bool
	// desktopSupported: this platform has a Claude desktop config location at
	// all (macOS/Windows). It gates what "all" expands to -- a Linux --client
	// all must not fail on a chat surface that cannot exist there.
	desktopSupported bool
}

func detectInstallTargets() detectedTargets {
	return detectedTargets{
		claude:           claudeDetected(),
		codex:            codexDetected(),
		desktop:          claudeDesktopSurfaceDetected(),
		desktopSupported: claudeDesktopSupported(),
	}
}

// detected returns the detected targets in canonical order.
func (d detectedTargets) detected() []installTarget {
	return orderedTargets(d.claude, d.codex, d.desktop)
}

// allTargets returns what an explicit "all" wires: both hook clients always,
// plus the chat surface where the platform can host it.
func (d detectedTargets) allTargets() []installTarget {
	return orderedTargets(true, true, d.desktopSupported)
}

// orderedTargets composes a selection in the stable install order: the Claude
// surfaces first (Claude Code, then the Claude app chat surface), then Codex.
// The parameters keep their detection order (claude, codex, desktop); only the
// emitted order is the install order, which the menu numbering follows.
func orderedTargets(claude, codex, desktop bool) []installTarget {
	var out []installTarget
	if claude {
		out = append(out, targetClaudeCode)
	}
	if desktop {
		out = append(out, targetClaudeDesktop)
	}
	if codex {
		out = append(out, targetCodex)
	}
	return out
}

// hookClientsFor filters an install-target selection down to the hook clients
// it contains -- the chat surface installs no hooks and no skills.
func hookClientsFor(targets []installTarget) []hooks.Client {
	var out []hooks.Client
	for _, t := range targets {
		switch t {
		case targetClaudeCode:
			out = append(out, hooks.ClientClaudeCode)
		case targetCodex:
			out = append(out, hooks.ClientCodex)
		}
	}
	return out
}

// targetNames maps install targets to their display labels, in order.
func targetNames(targets []installTarget) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		switch t {
		case targetClaudeDesktop:
			out[i] = "Claude app (chat)"
		case targetCodex:
			out[i] = "Codex"
		default:
			out[i] = "Claude Code"
		}
	}
	return out
}

// parseInstallTargets maps the --client flag to install targets in a stable
// order. The value is one target or a comma list of targets -- claude,
// claude-desktop (the Claude app chat surface: MCP bridge only, no hooks or
// skills), codex -- or a selector: "all" is every target this platform can host
// (naming claude-desktop explicitly still works anywhere, for --desktop-config
// overrides), "both" remains the two hook clients, and "detect" (also the
// meaning of an empty value) resolves to the detected set -- the same
// selection the curl installer's select_agent_client makes. With nothing
// detected, detect is an error, never a silent Claude Code default: wiring a
// client the user does not run creates its config directory and makes doctor
// report a phantom install.
func parseInstallTargets(raw string, det detectedTargets) ([]installTarget, error) {
	if trimmed := strings.ToLower(strings.TrimSpace(raw)); trimmed == "" || trimmed == "detect" || trimmed == "auto" {
		targets := det.detected()
		if len(targets) == 0 {
			return nil, errors.New("no agent client was detected on this machine (Claude Code, the Claude app, or Codex); pass --client claude|claude-desktop|codex|all to select one anyway")
		}
		return targets, nil
	}
	var claude, codex, desktop bool
	for tok := range strings.SplitSeq(strings.ToLower(raw), ",") {
		switch strings.TrimSpace(tok) {
		case "":
			// a stray comma selects nothing
		case "claude", "claude-code", "cc":
			claude = true
		case "claude-desktop", "desktop":
			desktop = true
		case "codex", "cx":
			codex = true
		case "all":
			claude, codex = true, true
			desktop = desktop || det.desktopSupported
		case "both":
			claude, codex = true, true
		case "detect", "auto":
			return nil, fmt.Errorf("--client %q: detect cannot be combined with named targets", raw)
		default:
			return nil, fmt.Errorf("unknown --client %q: valid values are claude, claude-desktop, codex, all, detect, or a comma list of targets", raw)
		}
	}
	targets := orderedTargets(claude, codex, desktop)
	if len(targets) == 0 {
		return nil, fmt.Errorf("--client %q selects no target: valid values are claude, claude-desktop, codex, all, detect, or a comma list of targets", raw)
	}
	return targets, nil
}

// resolveInstallTargets decides which targets to install for. An explicit
// --client always wins and drives every non-interactive path, so scripts, CI,
// and any piped invocation keep their exact prior behavior. When --client was
// omitted AND stdin is a terminal, it prompts the user to pick from the three
// targets (annotating each with whether it was detected on this machine). When
// --client was omitted and stdin is not a terminal, it falls back to the flag
// default (detect: the detected set, an error when nothing is present) with no
// prompt, so a redirected or automated run never blocks and a codex-only
// machine still gets the right profile.
func resolveInstallTargets(clientFlag string, clientSet bool) ([]installTarget, error) {
	det := detectInstallTargets()
	if clientSet || !stdinIsTerminal() {
		return parseInstallTargets(clientFlag, det)
	}
	return promptInstallTargets(os.Stdin, os.Stdout, det)
}

// promptInstallTargets asks an interactive user which target(s) to wire up.
// The menu is multi-select -- with three targets the detected set can be any
// subset, so an answer is one or more numbers/names separated by commas. It
// defaults to the detected set, re-prompts on an unrecognized answer, and
// takes the default on EOF so a closed stdin cannot loop forever. When nothing
// was detected it first warns and asks whether to install at all (default no),
// then offers the menu with no default -- the user must name the target they
// are opting into, because there is no detected set to fall back on.
func promptInstallTargets(in io.Reader, out io.Writer, det detectedTargets) ([]installTarget, error) {
	reader := bufio.NewReader(in)
	if len(det.detected()) == 0 {
		if err := confirmInstallWithoutClients(reader, out); err != nil {
			return nil, err
		}
	}
	def := defaultTargetChoice(det)
	fmt.Fprintln(out, bold("Install Seamless for which agent client?"))
	fmt.Fprintf(out, "  %s Claude Code %s\n", dim("[1]"), detectedColor(det.claude))
	fmt.Fprintf(out, "  %s Claude app (chat) %s\n", dim("[2]"), desktopMenuTag(det))
	fmt.Fprintf(out, "  %s Codex (app/CLI/IDE) %s\n", dim("[3]"), detectedColor(det.codex))
	fmt.Fprintf(out, "  %s All\n", dim("[4]"))
	for {
		if def == "" {
			fmt.Fprint(out, "Enter choices like 1 or 1,3: ")
		} else {
			fmt.Fprintf(out, "Enter choices like 1 or 1,3 [%s]: ", def)
		}
		line, err := reader.ReadString('\n')
		choice := strings.TrimSpace(line)
		if choice == "" {
			choice = def
		}
		if targets, ok := targetsForChoice(choice, det); ok {
			return targets, nil
		}
		if err != nil {
			// EOF/read error with an unusable answer: take the default rather
			// than loop on a stdin that will never yield more input. With no
			// default there is nothing safe to take, so abort.
			if def == "" {
				return nil, errors.New("no agent client selected")
			}
			targets, _ := targetsForChoice(def, det)
			return targets, nil
		}
		fmt.Fprintln(out, "  please enter numbers 1-4, comma-separated for several")
	}
}

// desktopMenuTag annotates the chat-surface menu entry: the usual detection
// tag where the platform can host it, an explicit unsupported note where it
// cannot -- choosing it there still parses, and registration then names the
// real constraint instead of the menu hiding the option.
func desktopMenuTag(det detectedTargets) string {
	if !det.desktopSupported {
		return dim("(not supported on this OS)")
	}
	return detectedColor(det.desktop)
}

// confirmInstallWithoutClients gates the interactive install when no agent
// client was detected: it warns and asks for an explicit yes, defaulting to no
// on an empty answer or EOF, so pressing Enter (or a closed stdin) aborts
// rather than wiring a client that is not there.
func confirmInstallWithoutClients(reader *bufio.Reader, out io.Writer) error {
	fmt.Fprintf(out, "%s no agent client was detected on this machine (Claude Code, the Claude app, or Codex)\n", yellow("warning:"))
	for {
		fmt.Fprint(out, "Install anyway? [y/N]: ")
		line, err := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return nil
		case "", "n", "no":
			return errors.New("aborted: no agent client detected (answer y, or rerun with --client claude|claude-desktop|codex|all)")
		}
		if err != nil {
			return errors.New("aborted: no agent client detected (answer y, or rerun with --client claude|claude-desktop|codex|all)")
		}
		fmt.Fprintln(out, "  please answer y or n")
	}
}

// targetsForChoice maps a menu answer -- numbers or target words, singly or as
// a comma list -- to install targets. The second return is false for any
// unrecognized token, so the caller can re-prompt.
func targetsForChoice(s string, det detectedTargets) ([]installTarget, bool) {
	var claude, codex, desktop, any bool
	for tok := range strings.SplitSeq(strings.ToLower(s), ",") {
		switch strings.TrimSpace(tok) {
		case "":
			continue
		case "1", "claude", "claude-code", "cc":
			claude = true
		case "2", "claude-desktop", "desktop":
			desktop = true
		case "3", "codex", "cx":
			codex = true
		case "4", "all":
			claude, codex = true, true
			desktop = desktop || det.desktopSupported
		case "both":
			claude, codex = true, true
		default:
			return nil, false
		}
		any = true
	}
	if !any {
		return nil, false
	}
	return orderedTargets(claude, codex, desktop), true
}

// defaultTargetChoice is the menu default: the detected set as menu numbers
// ("1,3"), collapsed to "4" when everything this platform can host was
// detected. Empty when nothing is detected -- there is deliberately no Claude
// Code fallback, so callers must either ask the user or fail, never silently
// wire a client that is not installed.
func defaultTargetChoice(det detectedTargets) string {
	detected := det.detected()
	if len(detected) == 0 {
		return ""
	}
	if len(detected) > 1 && slices.Equal(detected, det.allTargets()) {
		return "4"
	}
	nums := make([]string, 0, len(detected))
	for _, t := range detected {
		switch t {
		case targetClaudeDesktop:
			nums = append(nums, "2")
		case targetCodex:
			nums = append(nums, "3")
		default:
			nums = append(nums, "1")
		}
	}
	return strings.Join(nums, ",")
}

func detectedTag(ok bool) string {
	if ok {
		return "(detected)"
	}
	return "(not detected)"
}

// detectedColor is detectedTag tinted for the interactive menu: green when the
// client is present, dim when it is not.
func detectedColor(ok bool) string {
	if ok {
		return green(detectedTag(ok))
	}
	return dim(detectedTag(ok))
}

// stdinIsTerminal reports whether stdin is an interactive terminal, so the
// installer prompts a human but never blocks (or silently picks a
// detection-based default for) a piped, /dev/null, or otherwise redirected run.
// term.IsTerminal does the real ioctl -- unlike an os.ModeCharDevice heuristic,
// which misreads /dev/null (also a character device) as a terminal.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// claudeDetected reports whether Claude Code appears installed on this machine:
// the `claude` CLI on PATH, or a ~/.claude directory (Claude Code creates it on
// first run). It keeps install/doctor from nagging a user who does not run CC.
// A directory that a prior `install-hooks --client claude` created also counts,
// which is correct: that user opted into the Claude Code profile.
func claudeDetected() bool {
	if _, err := exec.LookPath("claude"); err == nil {
		return true
	}
	if home, err := expandHome("~/.claude"); err == nil {
		if info, statErr := os.Stat(home); statErr == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// codexDetected reports whether Codex appears installed: the `codex` CLI on
// PATH, or a $CODEX_HOME/~/.codex directory. Symmetric with claudeDetected.
func codexDetected() bool {
	if _, err := exec.LookPath("codex"); err == nil {
		return true
	}
	dir := "~/.codex"
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		dir = home
	}
	if path, err := expandHome(dir); err == nil {
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// claudeDesktopSupported reports whether this platform has a Claude desktop
// config location at all -- the Claude app ships for macOS and Windows only.
func claudeDesktopSupported() bool {
	_, err := defaultClaudeDesktopConfigPath()
	return err == nil
}

// claudeDesktopSurfaceDetected reports whether the Claude app chat surface
// appears present: the app bundle (macOS), or an existing desktop config file
// -- the only portable signal on Windows, and the same gating doctor's chat
// check uses. Symmetric with claudeDetected/codexDetected.
func claudeDesktopSurfaceDetected() bool {
	if claudeDesktopAppDetected() {
		return true
	}
	path, err := defaultClaudeDesktopConfigPath()
	if err != nil {
		return false
	}
	info, statErr := os.Stat(path)
	return statErr == nil && !info.IsDir()
}

// installClaudeHooks installs the Claude Code hook profile into settings.json and
// (unless doMCP is false) registers the MCP server with the claude CLI.
func installClaudeHooks(cfg config.Config, settings, baseURL, seamBin, configPath string, doMCP bool) error {
	path, err := expandHome(settings)
	if err != nil {
		return err
	}
	res, err := hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientClaudeCode, SettingsPath: path, BaseURL: baseURL,
		APIKey: cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: configPath,
	})
	if err != nil {
		return err
	}
	printClientBlock(res, "Claude Code", path)
	if doMCP {
		registerClaudeMCP(baseURL, seamBin, configPath)
	}
	return nil
}

// installCodexHooks installs the shared local Codex host profile used by the
// desktop app, CLI, and IDE extension into $CODEX_HOME/hooks.json. Unless doMCP
// is false it registers the seam mcp-proxy stdio bridge through the Codex
// management CLI, or prints the exact app-only settings fallback. It closes by
// pointing at Codex's hook trust gate when it changed the hook definitions.
// No config we write can satisfy that gate on the user's behalf (see the
// codex-hook-contract memory).
func installCodexHooks(cfg config.Config, codexHooks, baseURL, seamBin, configPath string, doMCP bool) error {
	target := codexHooks
	if strings.TrimSpace(target) == "" {
		target = defaultCodexHooksPath()
	}
	path, err := expandHome(target)
	if err != nil {
		return err
	}
	res, err := hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientCodex, SettingsPath: path, BaseURL: baseURL,
		APIKey: cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: configPath,
	})
	if err != nil {
		return err
	}
	printClientBlock(res, "Codex (app/CLI/IDE)", path)
	if doMCP {
		if err := registerCodexMCP(seamBin, configPath); err != nil {
			return err
		}
	}
	// Codex ignores new or changed hooks until the user trusts them; no config we
	// write can do that on their behalf (see the codex-hook-contract memory).
	// An unchanged install has no new action for the user, so keep routine upgrade
	// output quiet and leave the always-unverified diagnostic to doctor.
	if res.Changed {
		fieldRow("trust", yellow("unverified"))
		fmt.Printf("%s%s\n", fieldCont, dim("CLI: inspect and approve the current definitions with /hooks"))
		fmt.Printf("%s%s\n", fieldCont, dim("desktop app: /hooks is not available; confirm a repo chat receives <seam-briefing>"))
		fmt.Printf("%s%s\n", fieldCont, dim("headless: pass --dangerously-bypass-hook-trust"))
	}
	return nil
}

// printClientBlock renders one client's install as a compact, colored block: a
// bold client header, a one-line hook summary (action counts + the settings
// path), the specific hooks that changed (omitted when nothing did), and any
// backup. The caller adds the mcp (and, for Codex, trust) rows beneath it, so
// both client paths report identically.
func printClientBlock(res hooks.InstallResult, client, path string) {
	fmt.Printf("\n%s\n", bold(client))
	summary, changed := summarizeActions(res.Actions)
	fieldRow("hooks", summary+"  "+dim("· "+tildePath(path)))
	for _, line := range changed {
		fmt.Printf("%s%s\n", fieldCont, line)
	}
	if res.BackupPath != "" {
		fieldRow("backup", dim(tildePath(res.BackupPath)))
	}
}

// summarizeActions turns per-hook action strings ("SessionStart: added") into a
// colored count summary ("2 added, 4 unchanged") plus one detail line per action
// that changed something ("added: SessionStart, PostToolUse"). Unchanged hooks
// are counted but never enumerated -- they are the boring, scannable majority.
func summarizeActions(actions []string) (summary string, changed []string) {
	// Stable display order; unchanged trails so the eye lands on changes first.
	order := []string{"added", "updated", "adopted", "deduped", "unchanged"}
	counts := map[string]int{}
	events := map[string][]string{}
	for _, a := range actions {
		event, act, ok := strings.Cut(a, ": ")
		if !ok {
			continue
		}
		counts[act]++
		events[act] = append(events[act], event)
	}
	var segs []string
	for _, act := range order {
		if counts[act] == 0 {
			continue
		}
		segs = append(segs, colorAction(act, fmt.Sprintf("%d %s", counts[act], act)))
		if act != "unchanged" {
			changed = append(changed, colorAction(act, act+": ")+strings.Join(events[act], ", "))
		}
	}
	return strings.Join(segs, dim(", ")), changed
}

// colorAction tints an action word by outcome: green for a real change (added /
// updated / adopted), yellow for a dedupe, dim for unchanged.
func colorAction(act, text string) string {
	switch act {
	case "unchanged":
		return dim(text)
	case "deduped":
		return yellow(text)
	default:
		return green(text)
	}
}

// defaultCodexHooksPath is where the Codex installer writes when --codex-hooks is
// omitted: $CODEX_HOME/hooks.json if CODEX_HOME is set (Codex's own override),
// else ~/.codex/hooks.json. The "~" is expanded by the caller (expandHome).
func defaultCodexHooksPath() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "hooks.json")
	}
	return "~/.codex/hooks.json"
}

// claudeHeadersHelper builds the command line Claude Code runs at connect time
// to obtain the Authorization header (see `seam mcp-headers`). --config is baked
// in for the same reason the command hooks and the codex bridge bake it in: the
// client records this line with no environment, so it must resolve config from
// any cwd on its own.
func claudeHeadersHelper(seamBin, configPath string) string {
	helper := seamBin + " mcp-headers"
	if configPath != "" {
		helper += " --config " + configPath
	}
	return helper
}

// claudeMCPAddArgs builds the claude CLI argv that registers the Seamless MCP
// server.
//
// It registers via `mcp add-json` with a headersHelper rather than `mcp add
// --header "Authorization: Bearer <key>"`, because that form put the daemon's
// sole credential into this subprocess's argv, readable by any local account
// via `ps auxww` for as long as the call ran (audit L4). Nothing in this argv
// is secret now: it names a command, and the key is read from the 0600 config
// by that command at connection time. This is the same trade the Codex
// registration already makes with the mcp-proxy bridge.
//
// Under an https base URL it registers the `seam mcp-proxy` stdio bridge
// instead, for the same reason the hooks go exec-form there (see
// hooks.profileForBaseURL): Claude Code performs an http MCP connection with its
// own client, which has nowhere to be told about tls.ca_file, so a private-CA
// server fails in the handshake. The bridge speaks stdio to Claude Code and
// https to the daemon through the seam CLI's own trust store. This is the same
// registration the Codex bridge already uses (codexMCPAddArgs).
//
// --scope user is deliberate: the default local scope ties the registration to
// the directory it ran from, and the tools then vanish in every other repo.
func claudeMCPAddArgs(baseURL, seamBin, configPath string) []string {
	spec := map[string]any{
		"type":          "http",
		"url":           baseURL + "/api/mcp",
		"headersHelper": claudeHeadersHelper(seamBin, configPath),
	}
	if hooks.CommandOnlyBaseURL(baseURL) {
		args := []any{"mcp-proxy"}
		if configPath != "" {
			args = append(args, "--config", configPath)
		}
		spec = map[string]any{"type": "stdio", "command": seamBin, "args": args}
	}
	// A map of fixed keys over strings cannot fail to marshal.
	blob, _ := json.Marshal(spec) //nolint:errcheck // static map of strings; marshal cannot fail
	return []string{"mcp", "add-json", "--scope", "user", "seamless", string(blob)}
}

// registerClaudeMCP registers /api/mcp with the Claude Code CLI so hooks and
// MCP tools land in one command. Best-effort by design: the hooks are already
// installed at this point, so a missing or failing claude CLI degrades to
// printing the manual command rather than failing the install.
func registerClaudeMCP(baseURL, seamBin, configPath string) {
	args := claudeMCPAddArgs(baseURL, seamBin, configPath)
	manual := "claude " + strings.Join(args, " ")
	claude, err := exec.LookPath("claude")
	if err != nil {
		fieldRow("mcp", yellow("claude CLI not found"))
		fmt.Printf("%s%s\n", fieldCont, dim(manual))
		return
	}
	runner := execMCPCommandRunner{client: "claude", path: claude, timeout: mcpCommandTimeout}
	ctx := context.Background()

	// Claude Code 2.1.215 exposes no JSON flag for mcp get/list. Do not turn its
	// human health report into a second brittle state parser: retain the narrow
	// stored-Authorization migration below, then verify existence after add.
	// Codex's machine-readable surface is reconciled exactly in
	// reconcileCodexMCP.
	// An existing registration is normally left alone -- except the one shape
	// this change exists to retire. Installs predating the headersHelper switch
	// stored the bearer key as a literal header in ~/.claude.json, and a plain
	// "already registered" would leave it there forever, so the fix would only
	// ever reach new users. Re-register those in place.
	upgrade := false
	if out, gerr := runner.Run(ctx, "mcp", "get", seamlessMCPName); gerr == nil {
		if !strings.Contains(string(out), "Authorization") {
			fieldRow("mcp", dim("already registered"))
			return
		}
		upgrade = true
		// add-json refuses an existing name, so the old entry goes first. If the
		// add below then fails, the manual command is printed -- which is why
		// this is not attempted unless the claude CLI is known to work.
		if out, rerr := runner.Run(ctx, "mcp", "remove", "--scope", "user", seamlessMCPName); rerr != nil {
			fieldRow("mcp", yellow("could not replace the stored-key registration"))
			fmt.Printf("%s%s\n", fieldCont, dim(strings.TrimSpace(string(out))))
			fmt.Printf("%s%s\n", fieldCont, dim(manual))
			return
		}
	} else if errors.Is(gerr, context.DeadlineExceeded) || errors.Is(gerr, context.Canceled) {
		fieldRow("mcp", yellow("registration check timed out"))
		fmt.Printf("%s%s\n", fieldCont, dim(gerr.Error()))
		fmt.Printf("%s%s\n", fieldCont, dim(manual))
		return
	}

	if out, aerr := runner.Run(ctx, args...); aerr != nil {
		fieldRow("mcp", yellow("registration failed"))
		fmt.Printf("%s%s\n", fieldCont, dim(strings.TrimSpace(string(out))))
		fmt.Printf("%s%s\n", fieldCont, dim(manual))
		return
	}
	if _, verr := runner.Run(ctx, "mcp", "get", seamlessMCPName); verr != nil {
		fieldRow("mcp", yellow("registration could not be verified"))
		fmt.Printf("%s%s\n", fieldCont, dim(verr.Error()))
		fmt.Printf("%s%s\n", fieldCont, dim(manual))
		return
	}
	if upgrade {
		fieldRow("mcp", green("re-registered")+dim(" (--scope user; bearer key moved out of ~/.claude.json)"))
		return
	}
	fieldRow("mcp", green("registered")+dim(" (--scope user, key via headersHelper)"))
}

// codexMCPAddArgs builds the codex CLI argv that registers the Seamless MCP
// bridge as a stdio server (design decision D6): `codex mcp add seamless --
// <seam> mcp-proxy --config <yaml>`. The bridge reads the bearer key from
// --config at runtime, so -- unlike a streamable-HTTP registration -- no secret
// is duplicated into ~/.codex/config.toml. --config is baked in because codex
// records the argv with no environment (same reason the command hooks carry it).
func codexMCPAddArgs(seamBin, configPath string) []string {
	args := []string{"mcp", "add", "seamless", "--", seamBin, "mcp-proxy"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	return args
}

// absConfigPath makes the loaded config file absolute so it can be baked into
// the SessionStart command hook as SEAMLESS_CONFIG (the hook fires from any cwd,
// where a relative "seamless.yaml" would not resolve). "" (defaults+env, no
// file) stays "" so no SEAMLESS_CONFIG is emitted.
func absConfigPath(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	if abs, err := filepath.Abs(src); err == nil {
		return abs
	}
	return src
}

// resolveSeamBin picks the seam CLI path baked into the SessionStart command
// hook. An explicit --seam wins and is made absolute; otherwise it prefers the
// seam binary sitting next to this seamlessd (the normal `make build` layout),
// then resolves PATH. The final bare-name fallback is only for the preflight
// warning/manual-repair path when seam is not installed. The name carries .exe
// on Windows: exec-form command hooks require the real filename.
func resolveSeamBin(override string) string {
	if candidate := strings.TrimSpace(override); candidate != "" {
		if expanded, err := expandHome(candidate); err == nil {
			candidate = expanded
		}
		if !strings.ContainsAny(candidate, `/\`) {
			if found, err := exec.LookPath(candidate); err == nil {
				candidate = found
			}
		}
		if abs, err := filepath.Abs(candidate); err == nil {
			return abs
		}
		return candidate
	}
	name := seamBinName()
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		if abs, absErr := filepath.Abs(found); absErr == nil {
			return abs
		}
		return found
	}
	return name
}

// seamBinName is the seam CLI's filename for the OS install-hooks runs on --
// which is also the OS the hooks will fire on, so runtime.GOOS is correct.
func seamBinName() string {
	if runtime.GOOS == "windows" {
		return "seam.exe"
	}
	return "seam"
}
