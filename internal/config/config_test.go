package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestDefaults(t *testing.T) {
	d := Defaults()
	require.Equal(t, "127.0.0.1:8081", d.Addr)
	require.Equal(t, "~/.seamless", d.DataDir)
	require.Equal(t, ProviderOpenAI, d.LLM.Provider)
	require.Equal(t, "text-embedding-3-large", d.LLM.OpenAI.EmbeddingModel)
	require.Equal(t, 3072, d.LLM.OpenAI.EmbeddingDims)
	require.Equal(t, "https://api.anthropic.com", d.LLM.Anthropic.BaseURL)
	require.Equal(t, 1500, d.Budgets.MaxBriefingTokens)
	require.Equal(t, 1000, d.Budgets.RecallBudgetTokens)
	require.Equal(t, 0, d.Budgets.ToolEventMaxChars) // 0 = unlimited
	require.True(t, d.Gardener.Enabled)
	require.Equal(t, 60, d.Gardener.IntervalMinutes)
	require.Equal(t, 0.88, d.Gardener.DedupThreshold)
	require.Equal(t, 90, d.Gardener.StalenessDays)
	require.Equal(t, 30, d.Gardener.DigestDays)
	require.Equal(t, 30, d.Gardener.ToolEventRetentionDays)
	require.Equal(t, 14, d.Gardener.StalePlanDays)
	require.Equal(t, 14, d.Gardener.StaleStageDays)
	require.Equal(t, 45, d.Gardener.SessionIdleMinutes)
	// Briefing defaults reproduce the historical hardcoded auto-inject behavior;
	// the constraint/convention tier caps are the deliberate departures
	// (tiering on by default, constraint-split shape).
	require.Equal(t, 4, d.Briefing.ConstraintMaxFull)
	require.Equal(t, 4, d.Briefing.ConventionMaxFull)
	require.Equal(t, 0, d.Briefing.MemoryMaxAgeDays)
	require.Equal(t, 0, d.Briefing.MemoryMaxItems)
	require.Equal(t, 3, d.Briefing.FindingsCount)
	require.Equal(t, 0, d.Briefing.FindingsMaxAgeDays)
	require.Equal(t, 3, d.Briefing.ReadyTasksShown)
	require.Equal(t, 7, d.Briefing.PendingPlanMaxDays)
	require.Equal(t, 7, d.Briefing.StageUnknownMaxAgeDays)
	require.Equal(t, 2, d.Briefing.HardCapMultiplier)
	require.True(t, d.Briefing.IncludeParentMemories)
	require.Equal(t, 2, d.Briefing.SiblingFindingsCount)
	require.False(t, d.Briefing.IncludeSiblingMemories)
	require.Equal(t, 0.3, d.Search.SemanticFloor)
	require.True(t, d.PlanCapture.Enabled)
	require.True(t, d.PlanCapture.AutoTask)
	require.True(t, d.PlanCapture.InjectRelated)
	require.Equal(t, []int{80, 443}, d.Capture.AllowedPorts)
}

func TestLoadFrom_FileOverridesDefaults(t *testing.T) {
	path := writeConfig(t, `
addr: "127.0.0.1:9099"
data_dir: "/tmp/seamless-test-abs"
llm:
  provider: ollama
  openai:
    api_key: "sk-from-file"
gardener:
  enabled: false
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)

	require.Equal(t, "127.0.0.1:9099", cfg.Addr)
	require.Equal(t, "/tmp/seamless-test-abs", cfg.DataDir)
	require.Equal(t, ProviderOllama, cfg.LLM.Provider)
	require.Equal(t, "sk-from-file", cfg.LLM.OpenAI.APIKey)
	require.False(t, cfg.Gardener.Enabled)
	// Absent keys keep their defaults.
	require.Equal(t, "text-embedding-3-large", cfg.LLM.OpenAI.EmbeddingModel)
	require.Equal(t, 1500, cfg.Budgets.MaxBriefingTokens)
	require.Equal(t, path, cfg.SourcePath())
}

func TestLoadFrom_EnvWinsOverFile(t *testing.T) {
	path := writeConfig(t, `
addr: "127.0.0.1:9099"
llm:
  openai:
    api_key: "sk-from-file"
`)
	t.Setenv("SEAMLESS_ADDR", "127.0.0.1:7000")
	t.Setenv("SEAMLESS_OPENAI_API_KEY", "sk-from-env")
	t.Setenv("SEAMLESS_MCP_API_KEY", "static-key-123")

	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:7000", cfg.Addr)
	require.Equal(t, "sk-from-env", cfg.LLM.OpenAI.APIKey)
	require.Equal(t, "static-key-123", cfg.MCP.APIKey)
}

func TestLoadFrom_EnvOnlyNoFile(t *testing.T) {
	t.Setenv("SEAMLESS_GARDENER_ENABLED", "false")
	t.Setenv("SEAMLESS_MAX_BRIEFING_TOKENS", "800")
	t.Setenv("SEAMLESS_GARDENER_STALENESS_DAYS", "45")
	t.Setenv("SEAMLESS_GARDENER_DEDUP_THRESHOLD", "0.91")
	t.Setenv("SEAMLESS_TOOL_EVENT_MAX_CHARS", "4096")
	t.Setenv("SEAMLESS_TOOL_EVENT_RETENTION_DAYS", "7")
	t.Setenv("SEAMLESS_PLAN_CAPTURE_ENABLED", "false")
	t.Setenv("SEAMLESS_PLAN_CAPTURE_AUTO_TASK", "false")
	t.Setenv("SEAMLESS_PLAN_CAPTURE_INJECT_RELATED", "false")
	t.Setenv("SEAMLESS_GARDENER_STALE_PLAN_DAYS", "21")
	t.Setenv("SEAMLESS_SEARCH_SEMANTIC_FLOOR", "0.45")
	t.Setenv("SEAMLESS_BRIEFING_UTILITY_WEIGHT", "0.8")
	t.Setenv("SEAMLESS_BRIEFING_UTILITY_MODE", "off")
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.False(t, cfg.Gardener.Enabled)
	require.Equal(t, 800, cfg.Budgets.MaxBriefingTokens)
	require.Equal(t, 45, cfg.Gardener.StalenessDays)
	require.Equal(t, 0.91, cfg.Gardener.DedupThreshold)
	require.Equal(t, 4096, cfg.Budgets.ToolEventMaxChars)
	require.Equal(t, 7, cfg.Gardener.ToolEventRetentionDays)
	require.Equal(t, 21, cfg.Gardener.StalePlanDays)
	require.False(t, cfg.PlanCapture.Enabled)
	require.False(t, cfg.PlanCapture.AutoTask)
	require.False(t, cfg.PlanCapture.InjectRelated)
	require.Equal(t, 0.45, cfg.Search.SemanticFloor)
	require.Equal(t, 0.8, cfg.Briefing.UtilityWeight)
	require.Equal(t, "off", cfg.Briefing.UtilityMode)
	require.Equal(t, "", cfg.SourcePath())
}

func TestLoadFrom_AnthropicBaseURL(t *testing.T) {
	path := writeConfig(t, `
llm:
  anthropic:
    base_url: "http://127.0.0.1:9911"
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:9911", cfg.LLM.Anthropic.BaseURL)
	// Absent sibling keys keep their defaults.
	require.Equal(t, "claude-sonnet-5", cfg.LLM.Anthropic.ChatModel)

	t.Setenv("SEAMLESS_ANTHROPIC_BASE_URL", "http://127.0.0.1:9922")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:9922", cfg.LLM.Anthropic.BaseURL, "env wins over file")
}

func TestLoadFrom_ExpandsHome(t *testing.T) {
	path := writeConfig(t, `data_dir: "~/seamless-home-test"`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "seamless-home-test"), cfg.DataDir)
	require.False(t, strings.Contains(cfg.DataDir, "~"))
	require.Equal(t, filepath.Join(cfg.DataDir, "seam.db"), cfg.DBPath())
	require.Equal(t, filepath.Join(cfg.DataDir, "memory"), cfg.MemoryDir())
	require.Equal(t, filepath.Join(cfg.DataDir, "notes"), cfg.NotesDir())
}

func TestLoadFrom_BadEnvInt(t *testing.T) {
	t.Setenv("SEAMLESS_MAX_BRIEFING_TOKENS", "not-a-number")
	_, err := LoadFrom("")
	require.Error(t, err)
}

func TestLoadFrom_StrictYAML(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown top-level key",
			body: "adress: 127.0.0.1:8081\n",
			want: "field adress not found",
		},
		{
			name: "unknown nested key",
			body: "llm:\n  provder: openai\n",
			want: "field provder not found",
		},
		{
			name: "trailing document",
			body: "addr: 127.0.0.1:8081\n---\naddr: 127.0.0.1:9090\n",
			want: "multiple YAML documents",
		},
		{
			name: "duplicate key",
			body: "addr: 127.0.0.1:8081\naddr: 127.0.0.1:9090\n",
			want: "mapping key \"addr\" already defined",
		},
		{
			name: "wrong scalar type",
			body: "gardener:\n  interval_minutes: soon\n",
			want: "cannot unmarshal",
		},
		{
			name: "explicit null",
			body: "llm:\n  provider: null\n",
			want: "config.llm.provider must not be null",
		},
		{
			name: "implicit null",
			body: "gardener:\n  interval_minutes:\n",
			want: "config.gardener.interval_minutes must not be null",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFrom(writeConfig(t, tt.body))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadFrom_ExplicitInvalidNumericDoesNotDefault(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"zero gardener interval", "gardener:\n  interval_minutes: 0\n", "gardener.interval_minutes"},
		{"zero gardener dedup threshold", "gardener:\n  dedup_threshold: 0\n", "gardener.dedup_threshold"},
		{"zero gardener staleness", "gardener:\n  staleness_days: 0\n", "gardener.staleness_days"},
		{"zero gardener digest window", "gardener:\n  digest_days: 0\n", "gardener.digest_days"},
		{"zero session idle", "gardener:\n  session_idle_minutes: 0\n", "gardener.session_idle_minutes"},
		{"NaN gardener dedup threshold", "gardener:\n  dedup_threshold: .nan\n", "gardener.dedup_threshold"},
		{"infinite semantic floor", "search:\n  semantic_floor: .inf\n", "search.semantic_floor"},
		{"NaN briefing utility", "briefing:\n  utility_weight: .nan\n", "briefing.utility_weight"},
		{"negative OpenAI dimensions", "llm:\n  openai:\n    embedding_dims: -1\n", "llm.openai.embedding_dims"},
		{"oversized Ollama dimensions", "llm:\n  ollama:\n    embedding_dims: 65537\n", "llm.ollama.embedding_dims"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFrom(writeConfig(t, tt.body))
			require.ErrorContains(t, err, tt.want)
		})
	}

	t.Run("absent values retain defaults", func(t *testing.T) {
		cfg, err := LoadFrom(writeConfig(t, "gardener:\n  enabled: false\n"))
		require.NoError(t, err)
		require.Equal(t, 60, cfg.Gardener.IntervalMinutes)
		require.Equal(t, 0.88, cfg.Gardener.DedupThreshold)
		require.Equal(t, 90, cfg.Gardener.StalenessDays)
		require.Equal(t, 30, cfg.Gardener.DigestDays)
		require.Equal(t, 45, cfg.Gardener.SessionIdleMinutes)
	})

	t.Run("zero dimensions retain auto-detect meaning", func(t *testing.T) {
		cfg, err := LoadFrom(writeConfig(t, "llm:\n  openai:\n    embedding_dims: 0\n  ollama:\n    embedding_dims: 0\n"))
		require.NoError(t, err)
		require.Zero(t, cfg.LLM.OpenAI.EmbeddingDims)
		require.Zero(t, cfg.LLM.Ollama.EmbeddingDims)
	})

	t.Run("zero disabling windows remain valid", func(t *testing.T) {
		cfg, err := LoadFrom(writeConfig(t, "gardener:\n  tool_event_retention_days: 0\n  stale_plan_days: 0\n  stale_stage_days: 0\n"))
		require.NoError(t, err)
		require.Zero(t, cfg.Gardener.ToolEventRetentionDays)
		require.Zero(t, cfg.Gardener.StalePlanDays)
		require.Zero(t, cfg.Gardener.StaleStageDays)
	})
}

func TestLoadFrom_NonFiniteEnvRejected(t *testing.T) {
	for _, key := range []string{
		"SEAMLESS_GARDENER_DEDUP_THRESHOLD",
		"SEAMLESS_SEARCH_SEMANTIC_FLOOR",
		"SEAMLESS_BRIEFING_UTILITY_WEIGHT",
	} {
		t.Run(key, func(t *testing.T) {
			for _, value := range []string{"NaN", "+Inf", "-Inf"} {
				t.Run(value, func(t *testing.T) {
					t.Setenv(key, value)
					_, err := LoadFrom("")
					require.Error(t, err)
				})
			}
		})
	}
}

func TestLoadFrom_BriefingFileAndEnv(t *testing.T) {
	path := writeConfig(t, `
briefing:
  constraint_max_full: 5
  convention_max_full: 6
  memory_max_age_days: 60
  findings_count: 5
  include_sibling_memories: true
gardener:
  session_idle_minutes: 30
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, 5, cfg.Briefing.ConstraintMaxFull)
	require.Equal(t, 6, cfg.Briefing.ConventionMaxFull)
	require.Equal(t, 60, cfg.Briefing.MemoryMaxAgeDays)
	require.Equal(t, 5, cfg.Briefing.FindingsCount)
	require.True(t, cfg.Briefing.IncludeSiblingMemories)
	require.Equal(t, 30, cfg.Gardener.SessionIdleMinutes)
	// Absent briefing keys keep their defaults.
	require.Equal(t, 3, cfg.Briefing.ReadyTasksShown)
	require.Equal(t, 7, cfg.Briefing.PendingPlanMaxDays)
	require.Equal(t, 7, cfg.Briefing.StageUnknownMaxAgeDays)
	require.True(t, cfg.Briefing.IncludeParentMemories)

	t.Setenv("SEAMLESS_BRIEFING_CONSTRAINT_MAX_FULL", "6")
	t.Setenv("SEAMLESS_BRIEFING_CONVENTION_MAX_FULL", "2")
	t.Setenv("SEAMLESS_BRIEFING_MEMORY_MAX_AGE_DAYS", "90")
	t.Setenv("SEAMLESS_BRIEFING_STAGE_UNKNOWN_MAX_AGE_DAYS", "10")
	t.Setenv("SEAMLESS_GARDENER_STALE_STAGE_DAYS", "21")
	t.Setenv("SEAMLESS_BRIEFING_MEMORY_MAX_ITEMS", "25")
	t.Setenv("SEAMLESS_BRIEFING_FINDINGS_MAX_AGE_DAYS", "14")
	t.Setenv("SEAMLESS_BRIEFING_READY_TASKS_SHOWN", "1")
	t.Setenv("SEAMLESS_BRIEFING_PENDING_PLAN_MAX_DAYS", "3")
	t.Setenv("SEAMLESS_BRIEFING_HARD_CAP_MULTIPLIER", "3")
	t.Setenv("SEAMLESS_BRIEFING_SIBLING_FINDINGS_COUNT", "0")
	t.Setenv("SEAMLESS_BRIEFING_INCLUDE_PARENT_MEMORIES", "false")
	t.Setenv("SEAMLESS_BRIEFING_INCLUDE_SIBLING_MEMORIES", "false")
	t.Setenv("SEAMLESS_GARDENER_SESSION_IDLE_MINUTES", "20")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, 90, cfg.Briefing.MemoryMaxAgeDays, "env wins over file")
	require.Equal(t, 6, cfg.Briefing.ConstraintMaxFull, "env wins over file")
	require.Equal(t, 2, cfg.Briefing.ConventionMaxFull, "env wins over file")
	require.Equal(t, 10, cfg.Briefing.StageUnknownMaxAgeDays)
	require.Equal(t, 21, cfg.Gardener.StaleStageDays)
	require.Equal(t, 25, cfg.Briefing.MemoryMaxItems)
	require.Equal(t, 14, cfg.Briefing.FindingsMaxAgeDays)
	require.Equal(t, 1, cfg.Briefing.ReadyTasksShown)
	require.Equal(t, 3, cfg.Briefing.PendingPlanMaxDays)
	require.Equal(t, 3, cfg.Briefing.HardCapMultiplier)
	require.Equal(t, 0, cfg.Briefing.SiblingFindingsCount)
	require.False(t, cfg.Briefing.IncludeParentMemories)
	require.False(t, cfg.Briefing.IncludeSiblingMemories)
	require.Equal(t, 20, cfg.Gardener.SessionIdleMinutes)
}

func TestLoadFrom_FeaturesFileAndEnv(t *testing.T) {
	// Optional features ship off: no file, no env, nothing enabled.
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.False(t, cfg.Features.Research, "optional features default to off")
	require.False(t, cfg.Features.Momentum, "optional features default to off")

	path := writeConfig(t, `
features:
  research: true
  momentum: true
`)
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.Features.Research)
	require.True(t, cfg.Features.Momentum)

	t.Setenv("SEAMLESS_FEATURES_MOMENTUM", "false")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.False(t, cfg.Features.Momentum, "env wins over file")
	require.True(t, cfg.Features.Research, "the momentum env override leaves research alone")
	// t.Setenv above registered the restore-to-unset cleanup; drop the override
	// so the research assertions below run without it.
	require.NoError(t, os.Unsetenv("SEAMLESS_FEATURES_MOMENTUM"))

	t.Setenv("SEAMLESS_FEATURES_RESEARCH", "false")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.False(t, cfg.Features.Research, "env wins over file")

	t.Setenv("SEAMLESS_FEATURES_RESEARCH", "1")
	cfg, err = LoadFrom("")
	require.NoError(t, err)
	require.True(t, cfg.Features.Research, "env alone enables the feature")

	// Present but uninterpretable is an error, never a silent default.
	t.Setenv("SEAMLESS_FEATURES_RESEARCH", "yes-please")
	_, err = LoadFrom("")
	require.Error(t, err)
}

func TestLoadFrom_ConsoleLevelFileAndEnv(t *testing.T) {
	// A fresh installation starts at the fewest knobs.
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.Equal(t, "basic", cfg.Console.Level, "the console defaults to basic")
	require.Equal(t, ConsoleLevels[0], cfg.Console.Level, "the default is the first (fewest-surfaces) level")

	path := writeConfig(t, `
console:
  level: standard
`)
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, "standard", cfg.Console.Level)

	t.Setenv("SEAMLESS_CONSOLE_LEVEL", "advanced")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, "advanced", cfg.Console.Level, "env wins over file")

	// An empty override is absent: it resolves to the default rather than
	// leaving consumers an empty level to guess about.
	t.Setenv("SEAMLESS_CONSOLE_LEVEL", "")
	cfg, err = LoadFrom("")
	require.NoError(t, err)
	require.Equal(t, "basic", cfg.Console.Level)

	// Present but uninterpretable is an error naming the valid values, never a
	// silent default -- from the environment and from the file alike.
	t.Setenv("SEAMLESS_CONSOLE_LEVEL", "expert")
	_, err = LoadFrom("")
	require.ErrorContains(t, err, `console.level invalid "expert"`)
	require.ErrorContains(t, err, strings.Join(ConsoleLevels, ", "))
	require.NoError(t, os.Unsetenv("SEAMLESS_CONSOLE_LEVEL"))

	bad := writeConfig(t, `
console:
  level: Advanced
`)
	_, err = LoadFrom(bad)
	require.ErrorContains(t, err, "valid values are basic, standard, advanced")

	// A typo'd key fails at load like every other block.
	typo := writeConfig(t, `
console:
  levle: advanced
`)
	_, err = LoadFrom(typo)
	require.Error(t, err)
}

// console.sign_in is off -- the key -- unless the file says passkey. It is
// file only: an environment variable of its name changes nothing, since serve
// and console-open must read the same answer from the same file.
func TestLoadFrom_ConsoleSignIn(t *testing.T) {
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.Equal(t, ConsoleSignInKey, cfg.Console.SignIn, "the key signs a browser in by default")
	require.False(t, cfg.Console.PasskeySignIn())
	require.Equal(t, ConsoleSignInKey, ConsoleSignIns[0], "the default is listed first")

	path := writeConfig(t, `
console:
  sign_in: passkey
`)
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, ConsoleSignInPasskey, cfg.Console.SignIn)
	require.True(t, cfg.Console.PasskeySignIn())

	t.Setenv("SEAMLESS_CONSOLE_SIGN_IN", "key")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.Console.PasskeySignIn(), "file only: the environment does not turn it off")
	t.Setenv("SEAMLESS_CONSOLE_SIGN_IN", "passkey")
	cfg, err = LoadFrom("")
	require.NoError(t, err)
	require.False(t, cfg.Console.PasskeySignIn(), "file only: the environment does not turn it on")
	require.NoError(t, os.Unsetenv("SEAMLESS_CONSOLE_SIGN_IN"))

	empty := writeConfig(t, `
console:
  sign_in: ""
`)
	cfg, err = LoadFrom(empty)
	require.NoError(t, err)
	require.Equal(t, ConsoleSignInKey, cfg.Console.SignIn, "an empty value is absent")

	bad := writeConfig(t, `
console:
  sign_in: password
`)
	_, err = LoadFrom(bad)
	require.ErrorContains(t, err, `console.sign_in invalid "password"`)
	require.ErrorContains(t, err, "valid values are key, passkey")
}

func TestLoadFrom_FeaturesStrictYAML(t *testing.T) {
	path := writeConfig(t, `
features:
  reserach: true
`)
	_, err := LoadFrom(path)
	require.Error(t, err, "a typo'd feature key must fail loudly, not ship silently off")
}

func TestLoadFrom_CaptureAllowedPorts(t *testing.T) {
	path := writeConfig(t, `
capture:
  allowed_ports: [80, 443, 8080]
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, []int{80, 443, 8080}, cfg.Capture.AllowedPorts)

	// Env replaces the file's list wholesale rather than merging into it.
	t.Setenv("SEAMLESS_CAPTURE_ALLOWED_PORTS", "443, 8443")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, []int{443, 8443}, cfg.Capture.AllowedPorts, "env wins over file")
}

func TestLoadFrom_CaptureAllowedPortsEmptyMeansDefault(t *testing.T) {
	// An emptied allowlist must fall back to 80/443, never to "any port": an
	// empty set here would silently disable capture's SSRF port guard.
	t.Run("explicit empty list in file", func(t *testing.T) {
		path := writeConfig(t, "capture:\n  allowed_ports: []\n")
		cfg, err := LoadFrom(path)
		require.NoError(t, err)
		require.Equal(t, []int{80, 443}, cfg.Capture.AllowedPorts)
	})
	t.Run("empty env override", func(t *testing.T) {
		path := writeConfig(t, "capture:\n  allowed_ports: [8080]\n")
		t.Setenv("SEAMLESS_CAPTURE_ALLOWED_PORTS", "")
		cfg, err := LoadFrom(path)
		require.NoError(t, err)
		require.Equal(t, []int{80, 443}, cfg.Capture.AllowedPorts)
	})
	t.Run("key absent entirely", func(t *testing.T) {
		cfg, err := LoadFrom("")
		require.NoError(t, err)
		require.Equal(t, []int{80, 443}, cfg.Capture.AllowedPorts)
	})
}

func TestLoadFrom_CaptureAllowedPortsInvalid(t *testing.T) {
	t.Run("out of range in file", func(t *testing.T) {
		path := writeConfig(t, "capture:\n  allowed_ports: [80, 70000]\n")
		_, err := LoadFrom(path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "capture.allowed_ports")
	})
	t.Run("non-numeric env", func(t *testing.T) {
		t.Setenv("SEAMLESS_CAPTURE_ALLOWED_PORTS", "80,https")
		_, err := LoadFrom("")
		require.Error(t, err)
	})
	t.Run("out of range env", func(t *testing.T) {
		t.Setenv("SEAMLESS_CAPTURE_ALLOWED_PORTS", "0")
		_, err := LoadFrom("")
		require.Error(t, err)
	})
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults-ok", func(*Config) {}, false},
		{"empty-addr", func(c *Config) { c.Addr = "" }, true},
		{"empty-datadir", func(c *Config) { c.DataDir = "" }, true},
		{"unknown-provider", func(c *Config) { c.LLM.Provider = "gemini" }, true},
		{"zero-briefing-budget", func(c *Config) { c.Budgets.MaxBriefingTokens = 0 }, true},
		{"negative-recall-budget", func(c *Config) { c.Budgets.RecallBudgetTokens = -1 }, true},
		{"zero-tool-event-cap-ok", func(c *Config) { c.Budgets.ToolEventMaxChars = 0 }, false},
		{"negative-tool-event-cap", func(c *Config) { c.Budgets.ToolEventMaxChars = -1 }, true},
		{"zero-tool-event-retention-ok", func(c *Config) { c.Gardener.ToolEventRetentionDays = 0 }, false},
		{"negative-tool-event-retention", func(c *Config) { c.Gardener.ToolEventRetentionDays = -1 }, true},
		{"zero-stale-plan-days-ok", func(c *Config) { c.Gardener.StalePlanDays = 0 }, false},
		{"negative-stale-plan-days", func(c *Config) { c.Gardener.StalePlanDays = -1 }, true},
		{"zero-stale-stage-days-ok", func(c *Config) { c.Gardener.StaleStageDays = 0 }, false},
		{"negative-stale-stage-days", func(c *Config) { c.Gardener.StaleStageDays = -1 }, true},
		{"zero-gardener-interval", func(c *Config) { c.Gardener.IntervalMinutes = 0 }, true},
		{"negative-gardener-interval", func(c *Config) { c.Gardener.IntervalMinutes = -1 }, true},
		{"zero-gardener-dedup", func(c *Config) { c.Gardener.DedupThreshold = 0 }, true},
		{"NaN-gardener-dedup", func(c *Config) { c.Gardener.DedupThreshold = math.NaN() }, true},
		{"zero-gardener-staleness", func(c *Config) { c.Gardener.StalenessDays = 0 }, true},
		{"zero-gardener-digest", func(c *Config) { c.Gardener.DigestDays = 0 }, true},
		{"zero-session-idle", func(c *Config) { c.Gardener.SessionIdleMinutes = 0 }, true},
		{"negative-session-idle", func(c *Config) { c.Gardener.SessionIdleMinutes = -1 }, true},
		{"zero-openai-dims-auto-ok", func(c *Config) { c.LLM.OpenAI.EmbeddingDims = 0 }, false},
		{"negative-openai-dims", func(c *Config) { c.LLM.OpenAI.EmbeddingDims = -1 }, true},
		{"max-ollama-dims-ok", func(c *Config) { c.LLM.Ollama.EmbeddingDims = MaxEmbeddingDimensions }, false},
		{"oversized-ollama-dims", func(c *Config) { c.LLM.Ollama.EmbeddingDims = MaxEmbeddingDimensions + 1 }, true},
		{"zero-briefing-knobs-ok", func(c *Config) { c.Briefing = Briefing{} }, false},
		{"negative-briefing-findings", func(c *Config) { c.Briefing.FindingsCount = -1 }, true},
		{"zero-constraint-max-full-ok", func(c *Config) { c.Briefing.ConstraintMaxFull = 0 }, false},
		{"negative-constraint-max-full", func(c *Config) { c.Briefing.ConstraintMaxFull = -1 }, true},
		{"zero-convention-max-full-ok", func(c *Config) { c.Briefing.ConventionMaxFull = 0 }, false},
		{"negative-convention-max-full", func(c *Config) { c.Briefing.ConventionMaxFull = -1 }, true},
		{"negative-briefing-memory-age", func(c *Config) { c.Briefing.MemoryMaxAgeDays = -1 }, true},
		{"negative-briefing-stage-window", func(c *Config) { c.Briefing.StageUnknownMaxAgeDays = -1 }, true},
		{"negative-briefing-hard-cap", func(c *Config) { c.Briefing.HardCapMultiplier = -1 }, true},
		{"utility-weight-one-ok", func(c *Config) { c.Briefing.UtilityWeight = 1 }, false},
		{"negative-utility-weight", func(c *Config) { c.Briefing.UtilityWeight = -0.1 }, true},
		{"utility-weight-above-one", func(c *Config) { c.Briefing.UtilityWeight = 1.1 }, true},
		{"NaN-utility-weight", func(c *Config) { c.Briefing.UtilityWeight = math.NaN() }, true},
		{"infinite-utility-weight", func(c *Config) { c.Briefing.UtilityWeight = math.Inf(1) }, true},
		{"empty-utility-mode-ok", func(c *Config) { c.Briefing.UtilityMode = "" }, false},
		{"unknown-utility-mode", func(c *Config) { c.Briefing.UtilityMode = "sideways" }, true},
		{"empty-console-level-ok", func(c *Config) { c.Console.Level = "" }, false},
		{"advanced-console-level-ok", func(c *Config) { c.Console.Level = "advanced" }, false},
		{"unknown-console-level", func(c *Config) { c.Console.Level = "expert" }, true},
		{"console-level-case-is-not-normalized", func(c *Config) { c.Console.Level = "Basic" }, true},
		{"empty-console-sign-in-ok", func(c *Config) { c.Console.SignIn = "" }, false},
		{"passkey-console-sign-in-ok", func(c *Config) { c.Console.SignIn = ConsoleSignInPasskey }, false},
		{"unknown-console-sign-in", func(c *Config) { c.Console.SignIn = "password" }, true},
		{"console-sign-in-case-is-not-normalized", func(c *Config) { c.Console.SignIn = "Passkey" }, true},
		{"zero-semantic-floor-ok", func(c *Config) { c.Search.SemanticFloor = 0 }, false},
		{"one-semantic-floor-ok", func(c *Config) { c.Search.SemanticFloor = 1 }, false},
		{"negative-semantic-floor", func(c *Config) { c.Search.SemanticFloor = -0.1 }, true},
		{"semantic-floor-above-one", func(c *Config) { c.Search.SemanticFloor = 1.1 }, true},
		{"NaN-semantic-floor", func(c *Config) { c.Search.SemanticFloor = math.NaN() }, true},
		{"infinite-semantic-floor", func(c *Config) { c.Search.SemanticFloor = math.Inf(-1) }, true},
		{"custom-capture-ports-ok", func(c *Config) { c.Capture.AllowedPorts = []int{8080, 65535} }, false},
		{"zero-capture-port", func(c *Config) { c.Capture.AllowedPorts = []int{0} }, true},
		{"negative-capture-port", func(c *Config) { c.Capture.AllowedPorts = []int{-1} }, true},
		{"capture-port-above-range", func(c *Config) { c.Capture.AllowedPorts = []int{65536} }, true},

		// Transport: role, server_url, tls.
		{"role-server-ok", func(c *Config) { c.Role = RoleServer }, false},
		{"empty-role-ok", func(c *Config) { c.Role = "" }, false},
		{"unknown-role", func(c *Config) { c.Role = "peer" }, true},
		{"role-case-is-not-normalized", func(c *Config) { c.Role = "Server" }, true},
		{"server-url-ok", func(c *Config) { c.AdvertisedURL = "http://seam.lan:8081" }, false},
		{"server-url-https-ok", func(c *Config) { c.AdvertisedURL = "https://seam.lan" }, false},
		{"server-url-trailing-slash-ok", func(c *Config) { c.AdvertisedURL = "https://seam.lan/" }, false},
		{"server-url-bare-host", func(c *Config) { c.AdvertisedURL = "seam.lan:8081" }, true},
		{"server-url-scheme-relative", func(c *Config) { c.AdvertisedURL = "//seam.lan:8081" }, true},
		{"server-url-bad-scheme", func(c *Config) { c.AdvertisedURL = "ws://seam.lan:8081" }, true},
		{"server-url-no-host", func(c *Config) { c.AdvertisedURL = "http://" }, true},
		{"server-url-with-path", func(c *Config) { c.AdvertisedURL = "http://seam.lan:8081/api" }, true},
		{"server-url-with-query", func(c *Config) { c.AdvertisedURL = "http://seam.lan:8081?x=1" }, true},
		{"server-url-with-fragment", func(c *Config) { c.AdvertisedURL = "http://seam.lan:8081#top" }, true},
		{"tls-both-ok", func(c *Config) { c.TLS = TLS{CertFile: "c.pem", KeyFile: "k.pem"} }, false},
		{"tls-ca-only-ok", func(c *Config) { c.TLS = TLS{CAFile: "ca.pem"} }, false},
		{"tls-cert-without-key", func(c *Config) { c.TLS = TLS{CertFile: "c.pem"} }, true},
		{"tls-key-without-cert", func(c *Config) { c.TLS = TLS{KeyFile: "k.pem"} }, true},
		{"client-with-url-ok", func(c *Config) { c.Role, c.AdvertisedURL = RoleClient, "https://seam.lan" }, false},
		{"client-without-url", func(c *Config) { c.Role = RoleClient }, true},
		{"client-with-ca-ok", func(c *Config) {
			c.Role, c.AdvertisedURL, c.TLS = RoleClient, "https://seam.lan", TLS{CAFile: "ca.pem"}
		}, false},
		{"client-with-server-cert", func(c *Config) {
			c.Role, c.AdvertisedURL, c.TLS = RoleClient, "https://seam.lan", TLS{CertFile: "c.pem", KeyFile: "k.pem"}
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Defaults()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLoadFrom_TransportFileAndEnv(t *testing.T) {
	path := writeConfig(t, `
addr: 0.0.0.0:8081
server_url: http://seam.lan:8081
allowed_hosts: [seam, mac.local]
tls:
  cert_file: /etc/seamless/cert.pem
  key_file: /etc/seamless/key.pem
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, RoleServer, cfg.Role, "absent role keeps the default")
	require.Equal(t, "http://seam.lan:8081", cfg.AdvertisedURL)
	require.Equal(t, []string{"seam", "mac.local"}, cfg.AllowedHosts)
	require.True(t, cfg.TLSEnabled())
	// server_url wins over the derived scheme AND the derived host.
	require.Equal(t, "http://seam.lan:8081", cfg.ServerURL())
	require.Equal(t, []string{"seam", "mac.local", "seam.lan"}, cfg.AllowedHostsEffective())

	t.Setenv("SEAMLESS_SERVER_URL", "https://seam.example:9443")
	t.Setenv("SEAMLESS_ALLOWED_HOSTS", "one, two ,")
	t.Setenv("SEAMLESS_TLS_CA_FILE", "/etc/seamless/ca.pem")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, "https://seam.example:9443", cfg.ServerURL(), "env wins over file")
	require.Equal(t, []string{"one", "two"}, cfg.AllowedHosts, "env replaces the list, blanks dropped")
	require.Equal(t, "/etc/seamless/ca.pem", cfg.TLS.CAFile)
}

func TestLoadFrom_ClientRole(t *testing.T) {
	path := writeConfig(t, "role: client\nserver_url: https://seam.lan\n")
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.IsClient())
	require.False(t, cfg.TLSEnabled(), "a client holds no server certificate")
	require.Equal(t, "https://seam.lan", cfg.ServerURL())

	t.Setenv("SEAMLESS_ROLE", "server")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.False(t, cfg.IsClient(), "env wins over file")
}

func TestLoadFrom_TransportInvalid(t *testing.T) {
	t.Run("bad role in file", func(t *testing.T) {
		_, err := LoadFrom(writeConfig(t, "role: peer\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "valid values are server, client")
	})
	t.Run("bad role in env", func(t *testing.T) {
		t.Setenv("SEAMLESS_ROLE", "worker")
		_, err := LoadFrom("")
		require.Error(t, err)
	})
	t.Run("bare host server_url", func(t *testing.T) {
		_, err := LoadFrom(writeConfig(t, "server_url: seam.lan:8081\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "absolute http:// or https:// URL")
	})
	t.Run("client without server_url", func(t *testing.T) {
		_, err := LoadFrom(writeConfig(t, "role: client\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "requires server_url")
	})
	t.Run("half-set tls", func(t *testing.T) {
		_, err := LoadFrom(writeConfig(t, "tls:\n  cert_file: /c.pem\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be set together")
	})
	t.Run("unknown tls key", func(t *testing.T) {
		_, err := LoadFrom(writeConfig(t, "tls:\n  cert: /c.pem\n"))
		require.Error(t, err, "KnownFields rejects a typo rather than ignoring it")
	})
}

func TestLoadFrom_ExpandsHomeInTLSPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	path := writeConfig(t, "tls:\n  cert_file: ~/certs/c.pem\n  key_file: ~/certs/k.pem\n  ca_file: ~/certs/ca.pem\n")
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "certs", "c.pem"), cfg.TLS.CertFile)
	require.Equal(t, filepath.Join(home, "certs", "k.pem"), cfg.TLS.KeyFile)
	require.Equal(t, filepath.Join(home, "certs", "ca.pem"), cfg.TLS.CAFile)
}

func TestBriefingPresets(t *testing.T) {
	presets := BriefingPresets()
	require.Len(t, presets, 3)
	require.Equal(t, []string{"lean", "balanced", "rich"},
		[]string{presets[0].Key, presets[1].Key, presets[2].Key}, "leanest first")
	require.Equal(t, Defaults().Briefing, presets[1].Briefing, "Balanced is the defaults exactly")
	for _, p := range presets {
		require.NoError(t, p.Briefing.Validate(), p.Key)
		require.NotEmpty(t, p.Label, p.Key)
		require.NotEmpty(t, p.Intent, p.Key)
		// The token budget lives in Budgets, not Briefing: a preset cannot move it.
		got, ok := MatchBriefingPreset(p.Briefing)
		require.True(t, ok, p.Key)
		require.Equal(t, p.Key, got.Key)
	}
	// Presets are ordered by how much they put in.
	require.Less(t, presets[0].Briefing.ConstraintMaxFull, presets[1].Briefing.ConstraintMaxFull)
	require.Less(t, presets[1].Briefing.ConstraintMaxFull, presets[2].Briefing.ConstraintMaxFull)

	// Fresh copies: mutating one never reaches the table.
	presets[1].Briefing.FindingsCount = 99
	require.Equal(t, Defaults().Briefing, BriefingPresets()[1].Briefing)
}

func TestMatchBriefingPreset(t *testing.T) {
	b := Defaults().Briefing
	p, ok := MatchBriefingPreset(b)
	require.True(t, ok)
	require.Equal(t, "balanced", p.Key)

	// An empty utility mode means auto and still matches.
	b.UtilityMode = ""
	p, ok = MatchBriefingPreset(b)
	require.True(t, ok)
	require.Equal(t, "balanced", p.Key)

	// One knob off is a custom recipe.
	b.FindingsCount++
	_, ok = MatchBriefingPreset(b)
	require.False(t, ok)
	_, ok = MatchBriefingPreset(Briefing{})
	require.False(t, ok)
}
