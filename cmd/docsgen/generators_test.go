package main

import (
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/features"
	seamlessmcp "github.com/arctop/seamless/internal/mcp"
)

// repoRoot points the test at the repository root, the cwd docsgen requires:
// generators read repo files (seamless.yaml.example) by relative path, and `go
// test` runs from the package directory.
func repoRoot(t *testing.T) {
	t.Helper()
	t.Chdir("../..")
	_, err := os.Stat("go.mod")
	require.NoError(t, err, "test must run from the repo root")
}

// TestGenerateMCPToolsCoversCatalog renders every tool in the catalog through
// the generator. It is the docs half of the parity contract that
// internal/mcp.catalog_test holds up from the server side: whatever the server
// registers must be renderable, with a stable anchor, and a parameter table that
// is not silently empty.
func TestGenerateMCPToolsCoversCatalog(t *testing.T) {
	repoRoot(t)

	names := make([]string, 0, len(seamlessmcp.Catalog()))
	for _, tool := range seamlessmcp.Catalog() {
		names = append(names, tool.Name)
	}
	require.Len(t, names, seamlessmcp.ToolCount)

	md, err := generateMCPTools(&Page{Src: "reference/mcp/all.md", Tools: names}, "docs-src")
	require.NoError(t, err)

	for _, name := range names {
		require.Contains(t, md, "## "+name+" {#"+name+"}", "%s: heading with a pinned anchor", name)
	}
	require.NotContains(t, md, "<slug>", "generated text is HTML-escaped, or the browser eats it as a tag")
	require.Contains(t, md, "plan:&lt;slug&gt;")
}

func TestGenerateMCPToolsParamTable(t *testing.T) {
	repoRoot(t)

	md, err := generateMCPTools(&Page{Src: "reference/mcp/tasks.md", Tools: []string{"tasks_add"}}, "docs-src")
	require.NoError(t, err)

	require.Contains(t, md, "| Parameter | Type | Required | Description |")
	require.Contains(t, md, "| `title` | string | **yes** |", "required params are marked and come first")
	require.Contains(t, md, "| `plan` | string | no |")
	require.Less(t, strings.Index(md, "| `title` |"), strings.Index(md, "| `plan` |"),
		"required parameters sort before optional ones")
}

// TestGenerateMCPToolsRendersEnums: an enum's values are the most useful thing
// on the page, and their pipes are the likeliest way to shred a table row.
func TestGenerateMCPToolsRendersEnums(t *testing.T) {
	repoRoot(t)

	md, err := generateMCPTools(&Page{Src: "reference/mcp/tasks.md", Tools: []string{"tasks_update"}}, "docs-src")
	require.NoError(t, err)
	require.Contains(t, md, "One of: `open`, `in_progress`, `done`, `dropped`")
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| `status`") {
			require.Equal(t, 5, strings.Count(line, "|")-strings.Count(line, "\\|"),
				"a table row must have exactly 4 columns; unescaped pipes add more")
		}
	}
}

// TestGenerateMCPToolsAnnotatesOptionalTools: a tool an optional feature owns
// must say so on its reference page, and a tool no feature owns must not. The
// set is read from the registry here exactly as the generator reads it, so a
// newly optional tool is annotated without anyone editing a docs page.
func TestGenerateMCPToolsAnnotatesOptionalTools(t *testing.T) {
	repoRoot(t)

	owners := features.ToolOwners()
	require.NotEmpty(t, owners, "the registry owns at least one tool")
	names := slices.Sorted(maps.Keys(owners))
	// A tool no feature owns, rendered last so the tail check below is exact.
	names = append(names, "memory_write")

	md, err := generateMCPTools(&Page{Src: "reference/mcp/lab-gardener-usage.md", Tools: names}, "docs-src")
	require.NoError(t, err)

	for name, key := range owners {
		require.Contains(t, md, "**Optional** - part of *", "%s: optional tools carry the note", name)
		require.Contains(t, md, "hidden when the `"+string(key)+"` feature is disabled", "%s names its feature", name)
	}
	tail := md[strings.Index(md, "## memory_write"):]
	require.NotContains(t, tail, "**Optional**", "a tool no feature owns carries no note")
}

func TestGenerateMCPToolsErrors(t *testing.T) {
	repoRoot(t)

	_, err := generateMCPTools(&Page{Src: "a.md"}, "docs-src")
	require.ErrorContains(t, err, "needs a `tools:` list")

	_, err = generateMCPTools(&Page{Src: "a.md", Tools: []string{"tasks_teleport"}}, "docs-src")
	require.ErrorContains(t, err, "no such MCP tool")
}

// TestGenerateConfigCoversExample is the config half of the same idea: every key
// the shipped example file sets must appear in the generated table, or the
// reference is lying about the surface.
func TestGenerateConfigCoversExample(t *testing.T) {
	repoRoot(t)

	md, err := generateConfig(&Page{Src: "reference/configuration.md"}, "docs-src")
	require.NoError(t, err)

	for _, key := range []string{
		"addr", "data_dir", "mcp.api_key",
		"budgets.max_briefing_tokens", "budgets.recall_budget_tokens",
		"briefing.findings_count", "briefing.hard_cap_multiplier",
		"llm.provider", "llm.openai.chat_model", "llm.ollama.base_url", "llm.anthropic.chat_model",
		"gardener.enabled", "gardener.session_idle_minutes",
		"capture.allowed_ports", "plan_capture.enabled",
		"update.check", "update.check_interval", "update.auto", "update.max_defer", "update.min_age",
		"peer_auth.enabled", "peer_auth.owner", "peer_auth.socket.path", "peer_auth.socket.mode",
		"peer_auth.socket.group", "peer_auth.users",
	} {
		require.Contains(t, md, "| `"+key+"` |", "key %s is missing from the table", key)
	}

	require.Contains(t, md, "| `addr` | string | `127.0.0.1:8081` |")
	require.Contains(t, md, "| `mcp.api_key` | string | - |", "a key with no default says so")
	require.Contains(t, md, "| `capture.allowed_ports` | []int | `[80, 443]` |")
	require.Contains(t, md, "| `update.check` | bool, optional | unset |",
		"an optional key's unset default is a state of its own, not a missing default")
	require.Contains(t, md, "| `update.check_interval` | duration | `6h` |",
		"a duration reads as it is written, not as a config.Duration of nanoseconds")
	require.Contains(t, md, "| `update.auto` | bool, optional | unset |", "unset auto is on, which the docs prose says")
	require.Contains(t, md, "| `update.max_defer` | duration | `24h` |")
	require.Contains(t, md, "| `update.min_age` | duration | `24h` |")
	require.Contains(t, md, "```yaml", "the example file ships verbatim")
	require.NotContains(t, md, "| `sourcePath` |", "unexported bookkeeping is not a config key")
}

// TestConfigTypeAndDefault pins the Type and Default cells for the kinds the
// real config does not exercise yet: a SET optional key renders its value the
// way a plain key would (a zero one included, as a dash), and a zero duration
// is a dash like any other zero.
func TestConfigTypeAndDefault(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		wantType string
		wantDflt string
	}{
		{"unset optional bool", (*bool)(nil), "bool, optional", "unset"},
		{"set optional bool", new(true), "bool, optional", "`true`"},
		{"set optional false", new(false), "bool, optional", "-"},
		{"duration", config.Duration(90 * time.Minute), "duration", "`1h30m`"},
		{"zero duration", config.Duration(0), "duration", "-"},
		{"plain int", 60, "int", "`60`"},
		{"map of structs", map[string]config.PeerUser(nil), "map of string to {sealed []string, projects []string}", "-"},
		{"map of scalars", map[string]int(nil), "map of string to int", "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := reflect.ValueOf(tt.value)
			require.Equal(t, tt.wantType, configType(v.Type()))
			require.Equal(t, tt.wantDflt, formatDefault(v))
		})
	}
}

func TestGenerateUnknown(t *testing.T) {
	_, err := generate("nope", &Page{}, "docs-src")
	require.ErrorContains(t, err, "unknown generator (known: config, console-levels, mcp-tools)")
}

func TestEscapeCell(t *testing.T) {
	require.Equal(t, "startup\\|resume", escapeCell("startup|resume"))
	require.Equal(t, "plan:&lt;slug&gt;", escapeCell("plan:<slug>"))
	require.Equal(t, "a &amp; b", escapeCell("a & b"))
	require.Equal(t, "one two", escapeCell("one\ntwo"), "newlines would end the table early")
	// Code spans are escaped by the renderer, so pre-escaping would double-encode.
	require.Equal(t, "<slug>", escapeCodeCell("<slug>"))
	require.Equal(t, "a\\|b", escapeCodeCell("a|b"))
}

// The console's level matrix is generated from the console's own registries,
// so every screen, Settings section, and in-page surface appears, once, with
// the first level that shows it -- the docs cannot promise what the gates do
// not do.
func TestGenerateConsoleLevels_MatchesTheRegistries(t *testing.T) {
	out, err := generateConsoleLevels(nil, "")
	require.NoError(t, err)
	for _, group := range []string{console.MatrixScreens, console.MatrixSections, console.MatrixSurfaces} {
		require.Contains(t, out, "### "+group+"\n")
	}
	require.Contains(t, out, "| | Basic | Standard | Advanced |")
	for _, row := range console.LevelMatrix() {
		first := slices.Index(config.ConsoleLevels, row.Min)
		require.GreaterOrEqual(t, first, 0, row.Name)
		var cells []string
		for i := range config.ConsoleLevels {
			if i >= first {
				cells = append(cells, "shown")
			} else {
				cells = append(cells, "-")
			}
		}
		require.Contains(t, out, "**"+escapeCell(row.Name)+"**", row.Name)
		require.Contains(t, out, " | "+strings.Join(cells, " | ")+" |", row.Name)
	}
	require.Equal(t, 1, strings.Count(out, "**Retrieval**"), "each row once")
}

func TestPlaceGenerated(t *testing.T) {
	got, err := placeGenerated("a\n\n"+generateHere+"\n\nb\n", "GEN\n")
	require.NoError(t, err)
	require.Equal(t, "a\n\nGEN\n\nb\n", got)

	got, err = placeGenerated("a\n\n", "GEN\n")
	require.NoError(t, err)
	require.Equal(t, "a\n\nGEN\n", got, "no marker appends, as before")

	_, err = placeGenerated(generateHere+generateHere, "GEN")
	require.Error(t, err)
}
