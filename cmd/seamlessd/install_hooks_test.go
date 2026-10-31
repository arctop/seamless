package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/hooks"
	agentskills "github.com/arctop/seamless/internal/skills"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The skills step is an optional convenience layer: an unwritable skill root
// must cost a warning, not the daemon bootstrap (install-hooks runs from
// `curl | sh` under `set -eu`), and must not stop a later client in the
// --client all loop.
func TestRunInstallHooks_SkillFailureDegradesAndContinues(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("mcp:\n  api_key: \"test-key\"\n"), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	// This test is about the failure degrading, not about feature gating: with
	// research on, both skills have work to do for the second client.
	t.Setenv("SEAMLESS_FEATURES_RESEARCH", "1")

	// An unwritable Claude skill root: ~/.claude/skills is a regular file, so
	// creating skill directories under it fails for the first client wired.
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", "skills"), nil, 0o644))

	settings := filepath.Join(tmp, "settings.json")
	codexHooks := filepath.Join(tmp, "hooks.json")
	err := runInstallHooks([]string{
		"--client", "all", "--settings", settings, "--codex-hooks", codexHooks,
		"--mcp=false", "--seam", "/opt/seam", "--url", "http://127.0.0.1:8081",
	})
	require.NoError(t, err, "a failed skills step must degrade to a warning, not abort")

	// Both clients' hooks landed: the claude skills failure did not stop the loop.
	require.FileExists(t, settings)
	require.FileExists(t, codexHooks)
	// The later client's skills still installed; the failed root stayed a file.
	require.FileExists(t, filepath.Join(codexHome, "skills", agentskills.OnboardName, "SKILL.md"))
	require.FileExists(t, filepath.Join(codexHome, "skills", agentskills.ResearchName, "SKILL.md"))
	info, statErr := os.Stat(filepath.Join(home, ".claude", "skills"))
	require.NoError(t, statErr)
	require.False(t, info.IsDir())
}

// The Claude Code MCP registration names a headersHelper command instead of
// carrying the bearer key: the key would otherwise sit in this argv, readable
// via `ps auxww` during install (audit L4). Same trade as the Codex bridge
// below -- the key is read from the 0600 config at connection time.
func TestClaudeMCPAddArgs(t *testing.T) {
	args := claudeMCPAddArgs("http://127.0.0.1:8081", "/opt/seam", "/etc/seamless.yaml")
	require.Equal(t, []string{
		"mcp", "add-json", "--scope", "user", "seamless",
		`{"headersHelper":"/opt/seam mcp-headers --config /etc/seamless.yaml",` +
			`"type":"http","url":"http://127.0.0.1:8081/api/mcp"}`,
	}, args)

	// No config path -> no trailing --config, matching codexMCPAddArgs.
	require.Contains(t, claudeMCPAddArgs("http://127.0.0.1:8081", "/opt/seam", "")[5],
		`"headersHelper":"/opt/seam mcp-headers"`)
}

// The whole point of L4: no argv this installer builds may contain the key.
func TestClaudeMCPAddArgs_CarriesNoSecret(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	joined := strings.Join(claudeMCPAddArgs("http://127.0.0.1:8081", "/opt/seam", "/etc/seamless.yaml"), " ")
	require.NotContains(t, joined, key)
	require.NotContains(t, joined, "Bearer")
}

// The Codex MCP registration is a stdio bridge (codex mcp add ... -- <cmd>), not
// a streamable-HTTP URL: no bearer key is duplicated into codex config, and the
// bridge loads it from --config at runtime (design decision D6).
func TestCodexMCPAddArgs(t *testing.T) {
	args := codexMCPAddArgs("/opt/seam", "/etc/seamless.yaml")
	require.Equal(t, []string{
		"mcp", "add", "seamless", "--", "/opt/seam", "mcp-proxy", "--config", "/etc/seamless.yaml",
	}, args)
	// No config path -> no trailing --config (matches the command-hook builder).
	require.Equal(t, []string{
		"mcp", "add", "seamless", "--", "/opt/seam", "mcp-proxy",
	}, codexMCPAddArgs("/opt/seam", ""))
}

func TestCodexAppMCPSetupHint(t *testing.T) {
	hint := codexAppMCPSetupHint("/opt/seam", "/etc/seamless.yaml")
	require.Contains(t, hint, "Settings > MCP servers > Add server > STDIO")
	require.Contains(t, hint, `name "seamless"`)
	require.Contains(t, hint, `command "/opt/seam"`)
	require.Contains(t, hint, `arguments "mcp-proxy" "--config" "/etc/seamless.yaml"`)
	require.Contains(t, hint, "Save, then Restart")
	require.NotContains(t, hint, "Bearer")
	require.NotContains(t, hint, "api_key")
}

// summarizeActions runs with color disabled (stdout is not a tty under go
// test), so counts and detail lines compare as plain text.
func TestSummarizeActions(t *testing.T) {
	summary, changed := summarizeActions([]string{
		"SessionStart: added", "UserPromptSubmit: unchanged", "SessionEnd: added",
		"PostToolUse: deduped", "SubagentStop: unchanged",
	})
	// added precedes deduped precedes unchanged; unchanged is counted, not listed.
	require.Equal(t, "2 added, 1 deduped, 2 unchanged", summary)
	require.Equal(t, []string{
		"added: SessionStart, SessionEnd",
		"deduped: PostToolUse",
	}, changed)

	// All unchanged -> a single dim count, no detail lines.
	summary, changed = summarizeActions([]string{"A: unchanged", "B: unchanged"})
	require.Equal(t, "2 unchanged", summary)
	require.Empty(t, changed)

	// A malformed entry (no ": ") is skipped, not counted.
	summary, _ = summarizeActions([]string{"garbage", "A: added"})
	require.Equal(t, "1 added", summary)
}

func TestInstallCodexHooks_TrustGuidanceOnlyWhenDefinitionsChange(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.MCP.APIKey = "test-key"
	hooksPath := filepath.Join(dir, "hooks.json")

	first := captureStdout(t, func() error {
		return installCodexHooks(cfg, hooksPath, "http://127.0.0.1:8081",
			filepath.Join(dir, "seam"), filepath.Join(dir, "seamless.yaml"), false)
	})
	require.Contains(t, first, "5 added")
	require.Contains(t, first, "trust")
	require.Contains(t, first, "unverified")
	require.Contains(t, first, "inspect and approve the current definitions with /hooks")

	second := captureStdout(t, func() error {
		return installCodexHooks(cfg, hooksPath, "http://127.0.0.1:8081",
			filepath.Join(dir, "seam"), filepath.Join(dir, "seamless.yaml"), false)
	})
	require.Contains(t, second, "5 unchanged")
	require.NotContains(t, second, "trust")
	require.NotContains(t, second, "inspect and approve the current definitions with /hooks")
	require.NotContains(t, second, "<seam-briefing>")
}

func TestInstallClientSkills_ReportsUnchangedOnIdenticalReinstall(t *testing.T) {
	opts := agentskills.Options{HomeDir: t.TempDir()}

	first := captureStdout(t, func() error {
		return installClientSkills(hooks.ClientClaudeCode, opts)
	})
	require.Contains(t, first, "onboard  installed")
	require.Contains(t, first, "research  installed")

	second := captureStdout(t, func() error {
		return installClientSkills(hooks.ClientClaudeCode, opts)
	})
	require.Contains(t, second, "onboard  unchanged")
	require.Contains(t, second, "research  unchanged")
	require.NotContains(t, second, "updated")
}

// install-hooks is the one moment Seamless can bring a client's skill directory
// in line with the optional features the owner actually has switched on: skills
// live in ~/.claude/skills and $CODEX_HOME/skills, which the daemon cannot touch
// when a toggle flips in the console. The effective feature is the file/env base
// layered with the stored override, so the console toggle wins here too.
func TestRunInstallHooks_ResearchSkillFollowsTheEffectiveFeature(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	cfgPath := filepath.Join(tmp, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("data_dir: "+dataDir+"\nmcp:\n  api_key: \"test-key\"\n"), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	onboard := filepath.Join(codexHome, "skills", agentskills.OnboardName, "SKILL.md")
	research := filepath.Join(codexHome, "skills", agentskills.ResearchName, "SKILL.md")
	install := func(t *testing.T) string {
		t.Helper()
		return captureStdout(t, func() error {
			return runInstallHooks([]string{
				"--client", "codex", "--codex-hooks", filepath.Join(tmp, "hooks.json"),
				"--mcp=false", "--seam", "/opt/seam", "--url", "http://127.0.0.1:8081",
			})
		})
	}

	// Optional features ship off, so a fresh install wires the client without
	// the skill that documents the research tools.
	out := install(t)
	require.Contains(t, out, "research  not installed")
	require.FileExists(t, onboard)
	require.NoFileExists(t, research)
	require.NoDirExists(t, dataDir, "reading the toggles must not create a database")

	// The stored override -- what the console Settings form writes -- wins over
	// the file/env base, and install-hooks reads it live.
	setStoredResearch(t, dataDir, true)
	out = install(t)
	require.Contains(t, out, "research  installed")
	require.FileExists(t, research)

	// The explicit opt-out still forces a skip while the feature is on, and
	// leaves the installed package alone.
	t.Setenv("SEAMLESS_NO_RESEARCH_SKILL", "1")
	out = install(t)
	require.Contains(t, out, "research  skipped (SEAMLESS_NO_RESEARCH_SKILL)")
	require.FileExists(t, research)
	t.Setenv("SEAMLESS_NO_RESEARCH_SKILL", "")

	// Switching the feature back off removes what the enabled run installed, so
	// the doctor INFO line clears on the rerun it recommends.
	setStoredResearch(t, dataDir, false)
	out = install(t)
	require.Contains(t, out, "research  removed")
	require.NoFileExists(t, research)
	require.NoDirExists(t, filepath.Join(codexHome, "skills", agentskills.ResearchName))
	require.FileExists(t, onboard, "only the disabled feature's package is removed")
}

// The skill names come from the feature registry, never from a second literal in
// the installer, so a future optional feature gates its skill by being
// registered -- and every skill a feature claims must be one install-hooks
// actually maintains, or the toggle would silently gate nothing.
func TestDisabledFeatureSkills_DerivesFromTheRegistry(t *testing.T) {
	require.Contains(t, disabledFeatureSkills(config.Features{}), agentskills.ResearchName)
	require.NotContains(t, disabledFeatureSkills(config.Features{Research: true}), agentskills.ResearchName)

	published := agentskills.Published()
	for _, feature := range features.Registry() {
		if feature.Skill == "" {
			continue
		}
		require.Contains(t, published, feature.Skill,
			"feature %s names a skill install-hooks does not maintain", feature.Key)
		require.Contains(t, disabledFeatureSkills(features.Defaults()), feature.Skill,
			"optional features ship off, so %s must be gated by default", feature.Skill)
	}
}

func setStoredResearch(t *testing.T, dataDir string, on bool) {
	t.Helper()
	db, err := store.Open(filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Research: on}))
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)
	original := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = original })

	runErr := fn()
	os.Stdout = original
	require.NoError(t, w.Close())
	require.NoError(t, runErr)
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return string(raw)
}

func TestSplitBins(t *testing.T) {
	require.Equal(t, []string{"seamlessd", "seam"}, splitBins("seamlessd,seam"))
	require.Equal(t, []string{"seamlessd", "seam"}, splitBins(" seamlessd , seam "))
	require.Equal(t, []string{"seamlessd"}, splitBins("seamlessd,,"))
	require.Empty(t, splitBins(""))
}

// Selection fixtures: a macOS/Windows-shaped machine (desktop supported) and a
// Linux-shaped one (no Claude app build exists there).
func detTargets(claude, codex, desktop bool) detectedTargets {
	return detectedTargets{claude: claude, codex: codex, desktop: desktop, desktopSupported: true}
}

func detTargetsNoDesktop(claude, codex bool) detectedTargets {
	return detectedTargets{claude: claude, codex: codex}
}

func TestParseInstallTargets(t *testing.T) {
	cc := []installTarget{targetClaudeCode}
	cx := []installTarget{targetCodex}
	dt := []installTarget{targetClaudeDesktop}
	hookPair := []installTarget{targetClaudeCode, targetCodex}
	everything := []installTarget{targetClaudeCode, targetClaudeDesktop, targetCodex}

	for _, tt := range []struct {
		raw  string
		det  detectedTargets
		want []installTarget
	}{
		// Explicit values ignore detection entirely.
		{"claude", detTargets(false, true, false), cc},
		{"CC", detTargets(false, true, false), cc},
		{"codex", detTargets(true, false, false), cx},
		{"claude-desktop", detTargets(false, false, false), dt},
		{"desktop", detTargets(false, false, false), dt},
		// Comma lists compose targets; order and duplicates do not matter.
		{"claude,claude-desktop", detTargets(false, false, false), []installTarget{targetClaudeCode, targetClaudeDesktop}},
		{"desktop, codex", detTargets(false, false, false), []installTarget{targetClaudeDesktop, targetCodex}},
		{"codex,claude,codex,", detTargets(false, false, false), hookPair},
		// all is every target the platform can host; both stays the hook pair.
		{"all", detTargets(false, false, false), everything},
		{"all", detTargetsNoDesktop(false, false), hookPair},
		{"both", detTargets(true, true, true), hookPair},
		// Naming the chat surface beside all keeps it even where unsupported
		// (the --desktop-config override case).
		{"all,claude-desktop", detTargetsNoDesktop(false, false), everything},
		// detect (and its empty/auto spellings) follows the machine, matching
		// the curl installer's select_agent_client.
		{"detect", detTargets(true, true, true), everything},
		{"detect", detTargets(false, true, false), cx},
		{"detect", detTargets(false, false, true), dt},
		{"", detTargets(true, false, true), []installTarget{targetClaudeCode, targetClaudeDesktop}},
		{"auto", detTargets(false, true, false), cx},
	} {
		got, err := parseInstallTargets(tt.raw, tt.det)
		require.NoError(t, err, "raw %q", tt.raw)
		require.Equal(t, tt.want, got, "raw %q det=%+v", tt.raw, tt.det)
	}

	// detect with nothing present errors instead of silently wiring Claude
	// Code -- explicit values remain the only way to force an install.
	for _, raw := range []string{"detect", "", "auto"} {
		_, err := parseInstallTargets(raw, detTargets(false, false, false))
		require.ErrorContains(t, err, "no agent client was detected", "raw %q", raw)
		require.ErrorContains(t, err, "--client", "raw %q", raw)
	}

	_, err := parseInstallTargets("gemini", detTargets(true, true, true))
	require.ErrorContains(t, err, "unknown --client")
	require.ErrorContains(t, err, "detect")

	// detect combined with a named target is contradictory, not a union.
	_, err = parseInstallTargets("detect,claude", detTargets(true, true, true))
	require.ErrorContains(t, err, "cannot be combined")

	// Present-but-empty (a bare comma) is uninterpretable, never a default.
	_, err = parseInstallTargets(",", detTargets(true, true, true))
	require.ErrorContains(t, err, "selects no target")
}

func TestHookClientsForAndTargetNames(t *testing.T) {
	everything := []installTarget{targetClaudeCode, targetClaudeDesktop, targetCodex}
	require.Equal(t, []hooks.Client{hooks.ClientClaudeCode, hooks.ClientCodex}, hookClientsFor(everything))
	require.Empty(t, hookClientsFor([]installTarget{targetClaudeDesktop}))
	require.Equal(t, []string{"Claude Code", "Claude app (chat)", "Codex"}, targetNames(everything))
}

func TestDefaultTargetChoice(t *testing.T) {
	require.Equal(t, "", defaultTargetChoice(detTargets(false, false, false))) // nothing detected -> no default
	require.Equal(t, "1", defaultTargetChoice(detTargets(true, false, false)))
	require.Equal(t, "3", defaultTargetChoice(detTargets(false, true, false))) // codex-only machine defaults to codex
	require.Equal(t, "2", defaultTargetChoice(detTargets(false, false, true))) // chat-only machine defaults to the app
	require.Equal(t, "1,3", defaultTargetChoice(detTargets(true, true, false)))
	require.Equal(t, "1,2", defaultTargetChoice(detTargets(true, false, true)))
	require.Equal(t, "4", defaultTargetChoice(detTargets(true, true, true))) // everything detected -> All
	// On a platform without the chat surface, both hook clients ARE everything.
	require.Equal(t, "4", defaultTargetChoice(detTargetsNoDesktop(true, true)))
	require.Equal(t, "1", defaultTargetChoice(detTargetsNoDesktop(true, false)))
}

func TestTargetsForChoice(t *testing.T) {
	det := detTargets(true, true, true)
	for _, tt := range []struct {
		in   string
		want []installTarget
	}{
		{"1", []installTarget{targetClaudeCode}},
		{"2", []installTarget{targetClaudeDesktop}},
		{"1,2", []installTarget{targetClaudeCode, targetClaudeDesktop}},
		{"3, 2", []installTarget{targetClaudeDesktop, targetCodex}},
		{"4", []installTarget{targetClaudeCode, targetClaudeDesktop, targetCodex}},
		{"all", []installTarget{targetClaudeCode, targetClaudeDesktop, targetCodex}},
		{"both", []installTarget{targetClaudeCode, targetCodex}},
		{"desktop", []installTarget{targetClaudeDesktop}},
		{"claude,codex", []installTarget{targetClaudeCode, targetCodex}},
	} {
		got, ok := targetsForChoice(tt.in, det)
		require.True(t, ok, "in %q", tt.in)
		require.Equal(t, tt.want, got, "in %q", tt.in)
	}

	// All on a Linux-shaped machine excludes the surface that cannot exist.
	got, ok := targetsForChoice("4", detTargetsNoDesktop(true, true))
	require.True(t, ok)
	require.Equal(t, []installTarget{targetClaudeCode, targetCodex}, got)

	for _, bad := range []string{"", ",", "9", "1,9", "gemini"} {
		_, ok := targetsForChoice(bad, det)
		require.False(t, ok, "in %q", bad)
	}
}

func TestPromptInstallTargets(t *testing.T) {
	cc := []installTarget{targetClaudeCode}
	cx := []installTarget{targetCodex}
	hookPair := []installTarget{targetClaudeCode, targetCodex}
	everything := []installTarget{targetClaudeCode, targetClaudeDesktop, targetCodex}

	for _, tt := range []struct {
		name  string
		input string
		det   detectedTargets
		want  []installTarget
	}{
		{"pick claude", "1\n", detTargets(true, false, false), cc},
		{"pick codex", "3\n", detTargets(true, true, false), cx},
		{"pick all", "4\n", detTargets(true, true, true), everything},
		{"pick a pair", "1,2\n", detTargets(true, true, true), []installTarget{targetClaudeCode, targetClaudeDesktop}},
		{"word alias", "codex\n", detTargets(false, true, false), cx},
		{"both keeps the hook pair", "both\n", detTargets(true, true, true), hookPair},
		{"empty takes default codex", "\n", detTargets(false, true, false), cx},
		{"empty takes default everything", "\n", detTargets(true, true, true), everything},
		{"empty takes default detected pair", "\n", detTargets(true, false, true), []installTarget{targetClaudeCode, targetClaudeDesktop}},
		{"reprompt then valid", "9\nboth\n", detTargets(true, true, false), hookPair},
		{"eof falls back to default", "", detTargets(false, true, false), cx},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			got, err := promptInstallTargets(strings.NewReader(tt.input), &out, tt.det)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			// The menu always renders every target with a detection tag, Claude
			// surfaces first.
			require.Contains(t, out.String(), "[1] Claude Code")
			require.Contains(t, out.String(), "[2] Claude app (chat)")
			require.Contains(t, out.String(), "[3] Codex (app/CLI/IDE)")
			require.Contains(t, out.String(), "[4] All")
		})
	}

	// Where the platform cannot host the chat surface the entry stays visible
	// but says so, instead of the menu silently renumbering itself.
	var out strings.Builder
	_, err := promptInstallTargets(strings.NewReader("1\n"), &out, detTargetsNoDesktop(true, true))
	require.NoError(t, err)
	require.Contains(t, out.String(), "[2] Claude app (chat) (not supported on this OS)")
}

func TestInstallerMenuLabelsStayInSync(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "docs", "install"),
		filepath.Join("..", "..", "docs", "install.ps1"),
	} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(raw), "[2] Claude app (chat)", path)
		require.Contains(t, string(raw), "[3] Codex (app/CLI/IDE)", path)
		require.Contains(t, string(raw), "[4] All", path)
	}
}

func TestPromptInstallTargets_NothingDetected(t *testing.T) {
	cx := []installTarget{targetCodex}
	hookPair := []installTarget{targetClaudeCode, targetCodex}

	// An explicit yes reaches the target menu, which then has no default: the
	// user must name the target they are opting into.
	for _, tt := range []struct {
		name  string
		input string
		want  []installTarget
	}{
		{"yes then codex", "y\n3\n", cx},
		{"yes then desktop", "yes\n2\n", []installTarget{targetClaudeDesktop}},
		{"yes reprompt then valid", "y\n\nboth\n", hookPair},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			got, err := promptInstallTargets(strings.NewReader(tt.input), &out, detTargets(false, false, false))
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Contains(t, out.String(), "no agent client was detected")
		})
	}

	// The confirm gate defaults to no: empty answer, an explicit no, and EOF
	// all abort before any menu is shown.
	for _, tt := range []struct {
		name  string
		input string
	}{
		{"empty answer aborts", "\n"},
		{"explicit no aborts", "n\n"},
		{"eof aborts", ""},
		{"garbage then eof aborts", "maybe\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			_, err := promptInstallTargets(strings.NewReader(tt.input), &out, detTargets(false, false, false))
			require.ErrorContains(t, err, "aborted: no agent client detected")
		})
	}

	// Yes at the gate but EOF at the menu still aborts: with nothing detected
	// there is no default selection to fall back on.
	var out strings.Builder
	_, err := promptInstallTargets(strings.NewReader("y\n"), &out, detTargets(false, false, false))
	require.ErrorContains(t, err, "no agent client selected")
}

func TestDetectedTag(t *testing.T) {
	require.Equal(t, "(detected)", detectedTag(true))
	require.Equal(t, "(not detected)", detectedTag(false))
}

func TestAgentSkillClient_FollowsHookSelection(t *testing.T) {
	claude, err := agentSkillClient(hooks.ClientClaudeCode)
	require.NoError(t, err)
	require.Equal(t, agentskills.ClientClaude, claude)
	codex, err := agentSkillClient(hooks.ClientCodex)
	require.NoError(t, err)
	require.Equal(t, agentskills.ClientCodex, codex)
	_, err = agentSkillClient(hooks.Client("gemini"))
	require.ErrorContains(t, err, "valid values are claude, codex")
}

// clientInstallEnv isolates a client-install test from the developer's own
// install: a throwaway HOME and none of the SEAMLESS_* overrides that would
// otherwise decide the config search or the key.
func clientInstallEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, key := range []string{"SEAMLESS_CONFIG", "SEAMLESS_MCP_API_KEY", "SEAMLESS_SERVER_URL", "SEAMLESS_ROLE"} {
		if v, ok := os.LookupEnv(key); ok {
			t.Setenv(key, v)
		}
		require.NoError(t, os.Unsetenv(key))
	}
}

// A client install: the config file it writes, the server it wires against, the
// database it must not open, and where the feature state comes from when there
// is no database to read it from.
func TestRunInstallHooks_ServerURLWritesTheClientConfigAndOpensNoDatabase(t *testing.T) {
	home := t.TempDir()
	clientInstallEnv(t, home)
	srv, seen := settingsServer(t, http.StatusOK,
		`{"featuresConfig":{"research":true,"momentum":false,"gamification":false}}`)

	settings := filepath.Join(t.TempDir(), "settings.json")
	out := captureStdout(t, func() error {
		return runInstallHooks([]string{
			"--client", "claude", "--settings", settings, "--mcp=false",
			"--seam", "/opt/seam", "--server-url", srv.URL, "--api-key", "server-key",
		})
	})

	// The config file is the client config, with no data dir.
	cfgPath := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	body := string(raw)
	require.Contains(t, body, "role: client\n")
	require.Contains(t, body, "server_url: \""+srv.URL+"\"\n")
	require.Contains(t, body, "api_key: \"server-key\"\n")
	require.NotContains(t, body, "\ndata_dir:")
	info, err := os.Stat(cfgPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	// The hooks point at the server, not at a loopback daemon.
	hookFile, err := os.ReadFile(settings)
	require.NoError(t, err)
	require.Contains(t, string(hookFile), srv.URL+"/api/hooks/user-prompt-submit")
	require.Contains(t, string(hookFile), "Bearer server-key")
	require.NotContains(t, string(hookFile), "127.0.0.1:8081")

	// Nothing local was opened or created: no data dir, no seam.db.
	require.NoDirExists(t, filepath.Join(home, ".seamless"))

	// The feature state came over the wire. research is off in this machine's
	// (default) config and on at the server, and the skill follows the server.
	require.Len(t, *seen, 1)
	require.FileExists(t, filepath.Join(home, ".claude", "skills", agentskills.ResearchName, "SKILL.md"))

	require.Contains(t, out, "first run:")
	require.Contains(t, out, "client of "+srv.URL)

	// Re-running the same install is idempotent, not a refusal of its own file.
	out = captureStdout(t, func() error {
		return runInstallHooks([]string{
			"--client", "claude", "--settings", settings, "--mcp=false",
			"--seam", "/opt/seam", "--server-url", srv.URL, "--api-key", "server-key",
		})
	})
	require.NotContains(t, out, "first run:")
	require.Contains(t, out, "client of "+srv.URL)
	after, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Equal(t, body, string(after))
}

// Under https every Claude Code hook is exec-form -- including UserPromptSubmit,
// the one http hook of an http install -- and the MCP registration is the stdio
// bridge. Claude Code's own HTTP client cannot be told about tls.ca_file; seam
// can.
func TestRunInstallHooks_HTTPSClientWiresExecFormHooks(t *testing.T) {
	home := t.TempDir()
	clientInstallEnv(t, home)

	settings := filepath.Join(t.TempDir(), "settings.json")
	out := captureStdout(t, func() error {
		return runInstallHooks([]string{
			"--client", "claude", "--settings", settings, "--mcp=false", "--skills=false",
			"--seam", absFixture("/opt/seam"), "--server-url", "https://seam.example:8443", "--api-key", "server-key",
		})
	})

	raw, err := os.ReadFile(settings)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	hooksObj := parsed["hooks"].(map[string]any)
	for event := range hooksObj {
		entry := hooksObj[event].([]any)[0].(map[string]any)
		handler := entry["hooks"].([]any)[0].(map[string]any)
		require.Equal(t, "command", handler["type"], "%s must be exec-form under https", event)
	}
	ups := hooksObj["UserPromptSubmit"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	require.Equal(t, absFixture("/opt/seam"), ups["command"])
	require.Contains(t, ups["args"], "user-prompt-submit")
	// No http hook means no bearer key in settings.json.
	require.NotContains(t, string(raw), "Bearer")
	require.NotContains(t, string(raw), "server-key")

	require.Contains(t, out, "client of https://seam.example:8443")
}

// The https MCP registration is the seam stdio bridge: Claude Code speaks stdio
// to seam, and seam speaks https to the daemon with the CLI's own trust store.
func TestClaudeMCPAddArgs_HTTPSRegistersTheStdioBridge(t *testing.T) {
	args := claudeMCPAddArgs("https://seam.example:8443", "/opt/seam", "/etc/seamless.yaml")
	require.Equal(t, []string{
		"mcp", "add-json", "--scope", "user", "seamless",
		`{"args":["mcp-proxy","--config","/etc/seamless.yaml"],"command":"/opt/seam","type":"stdio"}`,
	}, args)
	joined := strings.Join(args, " ")
	require.NotContains(t, joined, "/api/mcp", "an https registration never hands Claude Code the URL directly")
	require.NotContains(t, joined, "mcp-headers")

	// Without a config path the bridge still registers, resolving config itself.
	require.Equal(t, `{"args":["mcp-proxy"],"command":"/opt/seam","type":"stdio"}`,
		claudeMCPAddArgs("https://seam.example:8443", "/opt/seam", "")[5])
}

// --api-key alone would be a credential silently ignored.
func TestRunInstallHooks_APIKeyWithoutServerURLIsAnError(t *testing.T) {
	clientInstallEnv(t, t.TempDir())
	err := runInstallHooks([]string{
		"--client", "claude", "--settings", filepath.Join(t.TempDir(), "settings.json"),
		"--mcp=false", "--skills=false", "--api-key", "server-key",
	})
	require.ErrorContains(t, err, "--api-key applies to --server-url")
}

// The config file is the owner's: a server install already on this machine is
// never rewritten into a client one.
func TestRunInstallHooks_ServerURLRefusesAnExistingConfig(t *testing.T) {
	home := t.TempDir()
	clientInstallEnv(t, home)
	cfgPath := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o700))
	body := "mcp:\n  api_key: \"local-key\"\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))

	err := runInstallHooks([]string{
		"--client", "claude", "--settings", filepath.Join(t.TempDir(), "settings.json"),
		"--mcp=false", "--skills=false", "--server-url", "https://seam.example:8443", "--api-key", "server-key",
	})
	require.ErrorContains(t, err, "never edited on your behalf")
	require.ErrorContains(t, err, "role: client")
	after, err2 := os.ReadFile(cfgPath)
	require.NoError(t, err2)
	require.Equal(t, body, string(after))
}

// rewindSchema makes db look like a database an older release last served:
// schema_migrations stops at applied. Only the version bookkeeping is rewound,
// which is all effectiveFeatures can see -- it asks the version and reads two
// tables every schema has had since migration 001.
func rewindSchema(t *testing.T, db *sql.DB, applied int) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE version > ?`, applied)
	require.NoError(t, err)
}

// schemaVersionAt reads the applied schema version without migrating.
func schemaVersionAt(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	v, err := store.SchemaVersion(db)
	require.NoError(t, err)
	return v
}

// addTrial records one trial: the data migration 022's grandfather clause looks
// for before it keeps research on.
func addTrial(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "lab", "Lab")
	require.NoError(t, err)
	require.NoError(t, store.CreateTrial(ctx, db, core.Trial{
		ID: "01TRIALGRANDFATHER", Lab: "boot-race", Title: "a trial the owner already recorded",
		Outcome: core.OutcomePass, ProjectSlug: "lab", CreatedAt: time.Now().UTC(),
	}))
}

// install-hooks runs while the OLD daemon still serves the database -- the
// installers restart the service only afterwards -- so it reads the stored
// toggles without migrating anything, and for a database from before migration
// 022 it still answers with what 022 is about to seed.
func TestEffectiveFeatures_ReadsWithoutMigrating(t *testing.T) {
	ctx := context.Background()
	beforeGrandfather := featuresGrandfatherVersion - 4 // 018: v0.4.9, the last release without it
	tests := []struct {
		name    string
		applied int
		setup   func(t *testing.T, db *sql.DB)
		want    bool
	}{
		{name: "current, no override", applied: store.LatestSchemaVersion()},
		{
			name: "current, research switched on in the console", applied: store.LatestSchemaVersion(),
			setup: func(t *testing.T, db *sql.DB) {
				require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
			},
			want: true,
		},
		{
			// 022 has run, so whatever it was going to seed is already a row.
			name: "older but past 022, with trials", applied: featuresGrandfatherVersion + 1, setup: addTrial,
		},
		{
			name: "before 022, with trials: research is about to be kept on", applied: beforeGrandfather,
			setup: addTrial, want: true,
		},
		{name: "before 022, without trials", applied: beforeGrandfather},
		{
			// 022 seeds only where no override row exists at all, even a blank one.
			name: "before 022, with trials and an override row already present", applied: beforeGrandfather,
			setup: func(t *testing.T, db *sql.DB) {
				addTrial(t, db)
				require.NoError(t, store.SetSetting(ctx, db, store.SettingFeaturesConfig, " "))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			dbPath := filepath.Join(dataDir, "seam.db")
			db, err := storetest.OpenFresh(t, dbPath)
			require.NoError(t, err)
			if tt.setup != nil {
				tt.setup(t, db)
			}
			rewindSchema(t, db, tt.applied)
			require.NoError(t, db.Close())

			var got config.Features
			out := captureStdout(t, func() error {
				got = effectiveFeatures(config.Config{DataDir: dataDir})
				return nil
			})
			require.NotContains(t, out, "warning", "a readable database of any age is no reason to warn")
			require.Equal(t, tt.want, got.Research)
			require.Equal(t, tt.applied, schemaVersionAt(t, dbPath), "install-hooks must never migrate the database")
		})
	}
}

// Failure-soft: a database that cannot be read costs a warning, never the
// install, and is left exactly as it was found.
func TestEffectiveFeatures_UnreadableDatabaseFallsBackUntouched(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "seam.db")
	garbage := []byte("not a SQLite database, and nothing may migrate one over it\n")
	require.NoError(t, os.WriteFile(dbPath, garbage, 0o600))

	var got config.Features
	out := captureStdout(t, func() error {
		got = effectiveFeatures(config.Config{DataDir: dataDir, Features: config.Features{Research: true}})
		return nil
	})
	require.Contains(t, out, "warning:")
	require.Contains(t, out, "using the file/env features config")
	require.True(t, got.Research, "the file/env base is the fallback")
	after, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.Equal(t, garbage, after)
}

// End to end, the upgrade the installer performs over a release from before
// migration 022: the old daemon's database is read as it stands, a trial user's
// research skill is kept, and the schema is left for the new daemon to migrate.
func TestRunInstallHooks_UpgradeFromBefore022KeepsResearchWithoutMigrating(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	cfgPath := filepath.Join(tmp, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("data_dir: "+dataDir+"\nmcp:\n  api_key: \"test-key\"\n"), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)

	dbPath := filepath.Join(dataDir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	addTrial(t, db)
	rewindSchema(t, db, featuresGrandfatherVersion-4)
	require.NoError(t, db.Close())

	out := captureStdout(t, func() error {
		return runInstallHooks([]string{
			"--client", "codex", "--codex-hooks", filepath.Join(tmp, "hooks.json"),
			"--mcp=false", "--seam", "/opt/seam", "--url", "http://127.0.0.1:8081",
		})
	})
	require.Contains(t, out, "research  installed")
	require.NotContains(t, out, "feature toggles")
	require.FileExists(t, filepath.Join(codexHome, "skills", agentskills.ResearchName, "SKILL.md"))
	require.Equal(t, featuresGrandfatherVersion-4, schemaVersionAt(t, dbPath),
		"the old daemon's schema is the new daemon's to migrate")
}

// A peer_auth socket install registers the seam stdio bridge: Claude Code's own
// http MCP client cannot dial a Unix socket.
func TestClaudeMCPAddArgs_SocketUsesTheBridge(t *testing.T) {
	args := claudeMCPAddArgs("unix:///opt/seamless/run/seamlessd.sock", "/opt/seam", "/etc/seamless.yaml")
	require.Equal(t, []string{
		"mcp", "add-json", "--scope", "user", "seamless",
		`{"args":["mcp-proxy","--config","/etc/seamless.yaml"],"command":"/opt/seam","type":"stdio"}`,
	}, args)
}
