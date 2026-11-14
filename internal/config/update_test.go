package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// unsetEnv removes keys for the duration of the test and restores them after:
// t.Setenv registers the restore, the Unsetenv makes the key truly absent
// (an empty value would be a present, unparseable one).
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		require.NoError(t, os.Unsetenv(k))
	}
}

// isolateUpdateEnv gives a loading test a clean slate: no SEAMLESS_* variable
// from the developer's shell (a stray SEAMLESS_UPDATE_MIN_AGE would fail every
// load), and a throwaway HOME for data_dir's ~ to expand into.
func isolateUpdateEnv(t *testing.T) {
	t.Helper()
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
}

func TestUpdateDefaults(t *testing.T) {
	u := Defaults().Update
	require.Nil(t, u.Check, "unset: the build decides, not config")
	require.Equal(t, DefaultUpdateCheckInterval, u.CheckInterval.Std())
	require.Equal(t, 6*time.Hour, u.CheckInterval.Std())
	require.Nil(t, u.Auto, "unset: on, as internal/update reads it")
	require.Equal(t, DefaultUpdateMaxDefer, u.MaxDefer.Std())
	require.Equal(t, 24*time.Hour, u.MaxDefer.Std())
	require.Equal(t, DefaultUpdateMinAge, u.MinAge.Std())
	require.Equal(t, 24*time.Hour, u.MinAge.Std())
	require.NoError(t, u.Validate())
}

func TestUpdateValidate(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		wantErr  bool
	}{
		{"zero is out of range, not the default", 0, true},
		{"just under the floor", 59 * time.Minute, true},
		{"the floor", time.Hour, false},
		{"the default", 6 * time.Hour, false},
		{"the ceiling", 720 * time.Hour, false},
		{"just over the ceiling", 721 * time.Hour, true},
		{"negative", -time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := Defaults().Update
			u.CheckInterval = Duration(tt.interval)
			err := u.Validate()
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "config: update.check_interval: ")
			require.ErrorContains(t, err, "outside [1h, 720h]")
		})
	}

	// Validate is wired into the whole-config check.
	c := Defaults()
	c.Update.CheckInterval = Duration(59 * time.Minute)
	require.ErrorContains(t, c.Validate(), "update.check_interval")
}

func TestUpdateValidate_DeferAndSoak(t *testing.T) {
	tests := []struct {
		name     string
		maxDefer time.Duration
		minAge   time.Duration
		wantErr  string // "" = valid
	}{
		{"the defaults", 24 * time.Hour, 24 * time.Hour, ""},
		// max_defer: [1m, 720h].
		{"max_defer zero is out of range, not the default", 0, 24 * time.Hour, "config: update.max_defer: 0s is outside [1m, 720h]"},
		{"max_defer just under the floor", 59 * time.Second, 24 * time.Hour, "config: update.max_defer: 59s is outside [1m, 720h]"},
		{"max_defer the floor", time.Minute, 24 * time.Hour, ""},
		{"max_defer the ceiling", 720 * time.Hour, 24 * time.Hour, ""},
		{"max_defer just over the ceiling", 721 * time.Hour, 24 * time.Hour, "config: update.max_defer: 721h is outside [1m, 720h]"},
		{"max_defer negative", -time.Minute, 24 * time.Hour, "config: update.max_defer: -1m is outside [1m, 720h]"},
		// min_age: [0, 720h], and zero is no soak.
		{"min_age zero is no soak", 24 * time.Hour, 0, ""},
		{"min_age a second", 24 * time.Hour, time.Second, ""},
		{"min_age the ceiling", 24 * time.Hour, 720 * time.Hour, ""},
		{"min_age just over the ceiling", 24 * time.Hour, 721 * time.Hour, "config: update.min_age: 721h is outside [0s, 720h]"},
		{"min_age negative", 24 * time.Hour, -time.Second, "config: update.min_age: -1s is outside [0s, 720h]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := Defaults().Update
			u.MaxDefer, u.MinAge = Duration(tt.maxDefer), Duration(tt.minAge)
			err := u.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}

	// Both are wired into the whole-config check.
	c := Defaults()
	c.Update.MaxDefer = 0
	require.ErrorContains(t, c.Validate(), "update.max_defer")
	c = Defaults()
	c.Update.MinAge = Duration(721 * time.Hour)
	require.ErrorContains(t, c.Validate(), "update.min_age")
}

func TestLoadFrom_UpdateCheckFile(t *testing.T) {
	isolateUpdateEnv(t)

	tests := []struct {
		name string
		body string
		want *bool // nil = unset
	}{
		{"block absent", "addr: 127.0.0.1:8081\n", nil},
		{"key absent", "update:\n  check_interval: 12h\n", nil},
		{"explicit false", "update:\n  check: false\n", new(false)},
		{"explicit true", "update:\n  check: true\n", new(true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadFrom(writeConfig(t, tt.body))
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Update.Check)
		})
	}
}

func TestLoadFrom_UpdateCheckEnv(t *testing.T) {
	isolateUpdateEnv(t)
	falseFile := writeConfig(t, "update:\n  check: false\n")
	tests := []struct {
		name    string
		file    string // "" = no file
		env     string
		want    *bool
		wantErr bool
	}{
		{name: "true", env: "true", want: new(true)},
		{name: "false", env: "false", want: new(false)},
		{name: "ParseBool spelling, trimmed", env: " 1 ", want: new(true)},
		{name: "env wins over file", file: falseFile, env: "true", want: new(true)},
		{name: "garbage is an error, never false", env: "nope", wantErr: true},
		{name: "set but empty is an error, never unset", env: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SEAMLESS_UPDATE_CHECK", tt.env)
			cfg, err := LoadFrom(tt.file)
			if tt.wantErr {
				require.ErrorContains(t, err, "config: env SEAMLESS_UPDATE_CHECK: ")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Update.Check)
		})
	}

	t.Run("unset env leaves the file value", func(t *testing.T) {
		unsetEnv(t, "SEAMLESS_UPDATE_CHECK")
		cfg, err := LoadFrom(falseFile)
		require.NoError(t, err)
		require.Equal(t, new(false), cfg.Update.Check)

		cfg, err = LoadFrom("")
		require.NoError(t, err)
		require.Nil(t, cfg.Update.Check)
	})
}

func TestLoadFrom_UpdateCheckIntervalEnv(t *testing.T) {
	isolateUpdateEnv(t)
	tests := []struct {
		name    string
		env     string
		want    time.Duration
		wantErr string
	}{
		{name: "hours", env: "12h", want: 12 * time.Hour},
		{name: "compound, trimmed", env: " 1h30m ", want: 90 * time.Minute},
		{name: "the ceiling", env: "720h", want: 720 * time.Hour},
		{name: "bare number needs a unit", env: "30", wantErr: `config: env SEAMLESS_UPDATE_CHECK_INTERVAL: invalid duration "30": needs a unit`},
		{name: "garbage", env: "soon", wantErr: "config: env SEAMLESS_UPDATE_CHECK_INTERVAL: "},
		{name: "set but empty", env: "", wantErr: "config: env SEAMLESS_UPDATE_CHECK_INTERVAL: "},
		{name: "negative", env: "-6h", wantErr: "must not be negative"},
		// Parses, then fails the range check.
		{name: "below the floor", env: "30m", wantErr: "config: update.check_interval: 30m is outside [1h, 720h]"},
		{name: "bare zero parses, then fails the range", env: "0", wantErr: "config: update.check_interval: 0s is outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SEAMLESS_UPDATE_CHECK_INTERVAL", tt.env)
			cfg, err := LoadFrom("")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Update.CheckInterval.Std())
		})
	}

	t.Run("env wins over file", func(t *testing.T) {
		t.Setenv("SEAMLESS_UPDATE_CHECK_INTERVAL", "24h")
		cfg, err := LoadFrom(writeConfig(t, "update:\n  check_interval: 12h\n"))
		require.NoError(t, err)
		require.Equal(t, 24*time.Hour, cfg.Update.CheckInterval.Std())
	})
}

func TestLoadFrom_UpdateAutoFile(t *testing.T) {
	isolateUpdateEnv(t)

	tests := []struct {
		name      string
		body      string
		wantAuto  *bool // nil = unset
		wantCheck *bool
	}{
		{"block absent", "addr: 127.0.0.1:8081\n", nil, nil},
		{"key absent", "update:\n  check: true\n", nil, new(true)},
		{"explicit false", "update:\n  auto: false\n", new(false), nil},
		{"explicit true", "update:\n  auto: true\n", new(true), nil},
		// Loading keeps both as written: how auto yields to check is
		// internal/update's merge, not config's.
		{"both, as written", "update:\n  check: false\n  auto: true\n", new(true), new(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadFrom(writeConfig(t, tt.body))
			require.NoError(t, err)
			require.Equal(t, tt.wantAuto, cfg.Update.Auto)
			require.Equal(t, tt.wantCheck, cfg.Update.Check)
		})
	}
}

func TestLoadFrom_UpdateAutoEnv(t *testing.T) {
	isolateUpdateEnv(t)
	falseFile := writeConfig(t, "update:\n  auto: false\n")
	tests := []struct {
		name    string
		file    string // "" = no file
		env     string
		want    *bool
		wantErr bool
	}{
		{name: "true", env: "true", want: new(true)},
		{name: "false", env: "false", want: new(false)},
		{name: "ParseBool spelling, trimmed", env: " 0 ", want: new(false)},
		{name: "env wins over file", file: falseFile, env: "true", want: new(true)},
		{name: "garbage is an error, never false", env: "nope", wantErr: true},
		{name: "set but empty is an error, never unset", env: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SEAMLESS_UPDATE_AUTO", tt.env)
			cfg, err := LoadFrom(tt.file)
			if tt.wantErr {
				require.ErrorContains(t, err, "config: env SEAMLESS_UPDATE_AUTO: ")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Update.Auto)
		})
	}

	t.Run("unset env leaves the file value", func(t *testing.T) {
		unsetEnv(t, "SEAMLESS_UPDATE_AUTO")
		cfg, err := LoadFrom(falseFile)
		require.NoError(t, err)
		require.Equal(t, new(false), cfg.Update.Auto)

		cfg, err = LoadFrom("")
		require.NoError(t, err)
		require.Nil(t, cfg.Update.Auto)
	})
}

func TestLoadFrom_UpdateDeferAndSoakEnv(t *testing.T) {
	isolateUpdateEnv(t)
	const maxDefer, minAge = "SEAMLESS_UPDATE_MAX_DEFER", "SEAMLESS_UPDATE_MIN_AGE"
	tests := []struct {
		name    string
		key     string
		env     string
		want    time.Duration
		wantErr string
	}{
		{name: "max_defer hours", key: maxDefer, env: "12h", want: 12 * time.Hour},
		{name: "max_defer the floor, trimmed", key: maxDefer, env: " 1m ", want: time.Minute},
		{name: "max_defer the ceiling", key: maxDefer, env: "720h", want: 720 * time.Hour},
		{name: "max_defer bare number needs a unit", key: maxDefer, env: "30", wantErr: `config: env SEAMLESS_UPDATE_MAX_DEFER: invalid duration "30": needs a unit`},
		{name: "max_defer garbage", key: maxDefer, env: "later", wantErr: "config: env SEAMLESS_UPDATE_MAX_DEFER: "},
		{name: "max_defer set but empty", key: maxDefer, env: "", wantErr: "config: env SEAMLESS_UPDATE_MAX_DEFER: "},
		{name: "max_defer negative", key: maxDefer, env: "-1h", wantErr: "must not be negative"},
		// Parse, then fail the range check.
		{name: "max_defer bare zero parses, then fails the range", key: maxDefer, env: "0", wantErr: "config: update.max_defer: 0s is outside [1m, 720h]"},
		{name: "max_defer above the ceiling", key: maxDefer, env: "721h", wantErr: "config: update.max_defer: 721h is outside [1m, 720h]"},

		{name: "min_age compound, trimmed", key: minAge, env: " 1h30m ", want: 90 * time.Minute},
		{name: "min_age bare zero is no soak", key: minAge, env: "0", want: 0},
		{name: "min_age 0s is no soak", key: minAge, env: "0s", want: 0},
		{name: "min_age the ceiling", key: minAge, env: "720h", want: 720 * time.Hour},
		{name: "min_age bare number needs a unit", key: minAge, env: "24", wantErr: `config: env SEAMLESS_UPDATE_MIN_AGE: invalid duration "24": needs a unit`},
		{name: "min_age set but empty", key: minAge, env: "", wantErr: "config: env SEAMLESS_UPDATE_MIN_AGE: "},
		{name: "min_age negative", key: minAge, env: "-24h", wantErr: "must not be negative"},
		{name: "min_age above the ceiling", key: minAge, env: "721h", wantErr: "config: update.min_age: 721h is outside [0s, 720h]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.env)
			cfg, err := LoadFrom("")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			got := cfg.Update.MaxDefer
			if tt.key == minAge {
				got = cfg.Update.MinAge
			}
			require.Equal(t, tt.want, got.Std())
		})
	}

	t.Run("env wins over file", func(t *testing.T) {
		t.Setenv(maxDefer, "2h")
		t.Setenv(minAge, "0")
		cfg, err := LoadFrom(writeConfig(t, "update:\n  max_defer: 12h\n  min_age: 48h\n"))
		require.NoError(t, err)
		require.Equal(t, 2*time.Hour, cfg.Update.MaxDefer.Std())
		require.Zero(t, cfg.Update.MinAge.Std())
	})
}

func TestLoadFrom_UpdateFileRejects(t *testing.T) {
	isolateUpdateEnv(t)

	tests := []struct {
		name string
		body string
		want []string // every fragment must appear
	}{
		// Leaving the key out is how check and auto say "unset"; null is not a
		// second spelling.
		{"explicit null check", "update:\n  check: null\n", []string{"config.update.check must not be null"}},
		{"implicit null check", "update:\n  check:\n", []string{"config.update.check must not be null"}},
		{"explicit null auto", "update:\n  auto: null\n", []string{"config.update.auto must not be null"}},
		{"implicit null auto", "update:\n  auto:\n", []string{"config.update.auto must not be null"}},
		{"null block", "update: null\n", []string{"config.update must not be null"}},
		{"misspelled key", "update:\n  chek: true\n", []string{"field chek not found"}},
		{"misspelled interval", "update:\n  interval: 6h\n", []string{"field interval not found"}},
		{"misspelled auto", "update:\n  auto_update: false\n", []string{"field auto_update not found"}},
		{"misspelled max_defer", "update:\n  max_delay: 24h\n", []string{"field max_delay not found"}},
		{"non-bool check", "update:\n  check: sometimes\n", []string{"cannot unmarshal", "sometimes"}},
		{"non-bool auto", "update:\n  auto: nightly\n", []string{"cannot unmarshal", "nightly"}},
		{"bare int interval", "update:\n  check_interval: 30\n", []string{"line 2", `invalid duration "30": needs a unit`}},
		{"float interval", "update:\n  check_interval: 1.5\n", []string{"line 2", "is not a duration"}},
		{"zero interval", "update:\n  check_interval: 0\n", []string{"config: update.check_interval: 0s is outside [1h, 720h]"}},
		{"interval below the floor", "update:\n  check_interval: 59m\n", []string{"config: update.check_interval: 59m is outside"}},
		{"interval above the ceiling", "update:\n  check_interval: 721h\n", []string{"config: update.check_interval: 721h is outside"}},
		{"bare int max_defer", "update:\n  max_defer: 24\n", []string{"line 2", `invalid duration "24": needs a unit`}},
		{"zero max_defer", "update:\n  max_defer: 0\n", []string{"config: update.max_defer: 0s is outside [1m, 720h]"}},
		{"max_defer below the floor", "update:\n  max_defer: 30s\n", []string{"config: update.max_defer: 30s is outside"}},
		{"max_defer above the ceiling", "update:\n  max_defer: 721h\n", []string{"config: update.max_defer: 721h is outside"}},
		{"bare int min_age", "update:\n  min_age: 24\n", []string{"line 2", `invalid duration "24": needs a unit`}},
		{"float min_age", "update:\n  min_age: 0.5\n", []string{"line 2", "is not a duration"}},
		{"negative min_age", "update:\n  min_age: -1h\n", []string{"line 2", "must not be negative"}},
		{"min_age above the ceiling", "update:\n  min_age: 721h\n", []string{"config: update.min_age: 721h is outside [0s, 720h]"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFrom(writeConfig(t, tt.body))
			for _, frag := range tt.want {
				require.ErrorContains(t, err, frag)
			}
		})
	}
}

func TestLoadFrom_UpdateIntervalFile(t *testing.T) {
	isolateUpdateEnv(t)

	cfg, err := LoadFrom(writeConfig(t, "update:\n  check_interval: 1h30m\n"))
	require.NoError(t, err)
	require.Equal(t, 90*time.Minute, cfg.Update.CheckInterval.Std())
	require.Nil(t, cfg.Update.Check, "setting the interval leaves check unset")

	// An absent interval keeps the default, even when the block is present.
	cfg, err = LoadFrom(writeConfig(t, "update:\n  check: true\n"))
	require.NoError(t, err)
	require.Equal(t, DefaultUpdateCheckInterval, cfg.Update.CheckInterval.Std())
}

func TestLoadFrom_UpdateDeferAndSoakFile(t *testing.T) {
	isolateUpdateEnv(t)

	cfg, err := LoadFrom(writeConfig(t, "update:\n  max_defer: 90m\n  min_age: 0\n"))
	require.NoError(t, err)
	require.Equal(t, 90*time.Minute, cfg.Update.MaxDefer.Std())
	require.Zero(t, cfg.Update.MinAge.Std(), "a bare zero min_age is no soak, never the default")
	require.Nil(t, cfg.Update.Auto, "setting the durations leaves auto unset")
	require.Equal(t, DefaultUpdateCheckInterval, cfg.Update.CheckInterval.Std())

	// Absent keys keep their defaults, even when the block is present.
	cfg, err = LoadFrom(writeConfig(t, "update:\n  auto: false\n"))
	require.NoError(t, err)
	require.Equal(t, DefaultUpdateMaxDefer, cfg.Update.MaxDefer.Std())
	require.Equal(t, DefaultUpdateMinAge, cfg.Update.MinAge.Std())
}

func TestLoadFrom_UpdateReleaseSigners(t *testing.T) {
	isolateUpdateEnv(t)
	abs := filepath.Join(t.TempDir(), "release_signers")
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	t.Run("unset by default: nothing is pinned", func(t *testing.T) {
		require.Empty(t, Defaults().Update.ReleaseSigners)
		cfg, err := LoadFrom(writeConfig(t, "update:\n  min_age: 1h\n"))
		require.NoError(t, err)
		require.Empty(t, cfg.Update.ReleaseSigners)
	})
	t.Run("an absolute path", func(t *testing.T) {
		cfg, err := LoadFrom(writeConfig(t, "update:\n  release_signers: "+strconv.Quote(abs)+"\n"))
		require.NoError(t, err)
		require.Equal(t, abs, cfg.Update.ReleaseSigners)
	})
	t.Run("a leading ~ expands", func(t *testing.T) {
		cfg, err := LoadFrom(writeConfig(t, "update:\n  release_signers: ~/signers\n"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "signers"), cfg.Update.ReleaseSigners)
	})
	t.Run("a relative path is refused, never resolved against the working directory", func(t *testing.T) {
		_, err := LoadFrom(writeConfig(t, "update:\n  release_signers: release_signers\n"))
		require.ErrorContains(t, err, "update.release_signers must be an absolute path")
	})
	t.Run("the environment wins over the file", func(t *testing.T) {
		t.Setenv("SEAMLESS_UPDATE_RELEASE_SIGNERS", abs)
		cfg, err := LoadFrom(writeConfig(t, "update:\n  release_signers: /elsewhere/signers\n"))
		require.NoError(t, err)
		require.Equal(t, abs, cfg.Update.ReleaseSigners)
	})
	t.Run("a relative path in the environment is refused too", func(t *testing.T) {
		t.Setenv("SEAMLESS_UPDATE_RELEASE_SIGNERS", "signers")
		_, err := LoadFrom("")
		require.ErrorContains(t, err, "update.release_signers must be an absolute path")
	})
}
