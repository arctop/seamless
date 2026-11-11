package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackupDefaults(t *testing.T) {
	b := Defaults().Backup
	require.Nil(t, b.Auto, "unset: the build decides, not config")
	require.Equal(t, DefaultBackupEvery, b.Every.Std())
	require.Equal(t, 24*time.Hour, b.Every.Std())
	require.Equal(t, DefaultBackupKeep, b.Keep)
	require.Equal(t, 7, b.Keep)
	require.Empty(t, b.Dir, "empty: <data_dir>/backups")
	require.NoError(t, b.Validate())
}

func TestBackupValidate(t *testing.T) {
	abs := t.TempDir()
	tests := []struct {
		name    string
		mutate  func(*Backup)
		wantErr string // "" = valid
	}{
		{"the defaults", func(*Backup) {}, ""},
		// every: [1h, 720h].
		{"every zero is out of range, not the default", func(b *Backup) { b.Every = 0 }, "config: backup.every: 0s is outside [1h, 720h]"},
		{"every just under the floor", func(b *Backup) { b.Every = Duration(59 * time.Minute) }, "config: backup.every: 59m is outside [1h, 720h]"},
		{"every the floor", func(b *Backup) { b.Every = Duration(time.Hour) }, ""},
		{"every a week", func(b *Backup) { b.Every = Duration(168 * time.Hour) }, ""},
		{"every the ceiling", func(b *Backup) { b.Every = Duration(720 * time.Hour) }, ""},
		{"every just over the ceiling", func(b *Backup) { b.Every = Duration(721 * time.Hour) }, "config: backup.every: 721h is outside [1h, 720h]"},
		{"every negative", func(b *Backup) { b.Every = Duration(-time.Hour) }, "config: backup.every: -1h is outside [1h, 720h]"},
		// keep: [1, 100].
		{"keep zero is out of range, not the default", func(b *Backup) { b.Keep = 0 }, "config: backup.keep: 0 is outside [1, 100]"},
		{"keep the floor", func(b *Backup) { b.Keep = 1 }, ""},
		{"keep the ceiling", func(b *Backup) { b.Keep = 100 }, ""},
		{"keep just over the ceiling", func(b *Backup) { b.Keep = 101 }, "config: backup.keep: 101 is outside [1, 100]"},
		{"keep negative", func(b *Backup) { b.Keep = -1 }, "config: backup.keep: -1 is outside [1, 100]"},
		// dir: empty or absolute.
		{"dir absolute", func(b *Backup) { b.Dir = abs }, ""},
		{"dir relative", func(b *Backup) { b.Dir = "backups" }, `config: backup.dir must be an absolute path (or start with ~/), got "backups"`},
		{"dir dot-relative", func(b *Backup) { b.Dir = "./backups" }, "config: backup.dir must be an absolute path"},
		// auto: every state is valid.
		{"auto on", func(b *Backup) { b.Auto = new(true) }, ""},
		{"auto off", func(b *Backup) { b.Auto = new(false) }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Defaults().Backup
			tt.mutate(&b)
			err := b.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}

	// Validate is wired into the whole-config check.
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Backup.Every = Duration(30 * time.Minute) },
		func(c *Config) { c.Backup.Keep = 0 },
		func(c *Config) { c.Backup.Dir = "relative" },
	} {
		c := Defaults()
		mutate(&c)
		require.ErrorContains(t, c.Validate(), "config: backup.")
	}
}

func TestLoadFrom_BackupFile(t *testing.T) {
	isolateUpdateEnv(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	abs := filepath.ToSlash(t.TempDir())

	tests := []struct {
		name string
		body string
		want Backup
	}{
		{"block absent", "addr: 127.0.0.1:8081\n", Defaults().Backup},
		{"auto off", "backup:\n  auto: false\n", Backup{Auto: new(false), Every: Duration(DefaultBackupEvery), Keep: DefaultBackupKeep}},
		{"auto on", "backup:\n  auto: true\n", Backup{Auto: new(true), Every: Duration(DefaultBackupEvery), Keep: DefaultBackupKeep}},
		// Absent keys keep their defaults, even when the block is present, and
		// setting the others leaves auto unset.
		{"every and keep", "backup:\n  every: 6h\n  keep: 30\n", Backup{Every: Duration(6 * time.Hour), Keep: 30}},
		{"an absolute dir", "backup:\n  dir: '" + abs + "'\n", Backup{Every: Duration(DefaultBackupEvery), Keep: DefaultBackupKeep, Dir: abs}},
		{"a ~ dir expands", "backup:\n  dir: ~/Backups/Seamless\n", Backup{Every: Duration(DefaultBackupEvery), Keep: DefaultBackupKeep, Dir: filepath.Join(home, "Backups", "Seamless")}},
		{"an empty dir is the default folder", "backup:\n  dir: \"\"\n", Defaults().Backup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadFrom(writeConfig(t, tt.body))
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.Backup)
		})
	}
}

func TestLoadFrom_BackupFileRejects(t *testing.T) {
	isolateUpdateEnv(t)

	tests := []struct {
		name string
		body string
		want []string // every fragment must appear
	}{
		// Leaving the key out is how auto says "unset"; null is not a second
		// spelling.
		{"explicit null auto", "backup:\n  auto: null\n", []string{"config.backup.auto must not be null"}},
		{"implicit null auto", "backup:\n  auto:\n", []string{"config.backup.auto must not be null"}},
		{"null block", "backup: null\n", []string{"config.backup must not be null"}},
		{"misspelled key", "backup:\n  enabled: true\n", []string{"field enabled not found"}},
		{"misspelled every", "backup:\n  interval: 24h\n", []string{"field interval not found"}},
		{"misspelled dir", "backup:\n  folder: /tmp\n", []string{"field folder not found"}},
		{"plural block", "backups:\n  auto: true\n", []string{"field backups not found"}},
		{"non-bool auto", "backup:\n  auto: daily\n", []string{"cannot unmarshal", "daily"}},
		{"bare int every", "backup:\n  every: 24\n", []string{"line 2", `invalid duration "24": needs a unit`}},
		{"float every", "backup:\n  every: 1.5\n", []string{"line 2", "is not a duration"}},
		{"zero every", "backup:\n  every: 0\n", []string{"config: backup.every: 0s is outside [1h, 720h]"}},
		{"every below the floor", "backup:\n  every: 30m\n", []string{"config: backup.every: 30m is outside"}},
		{"every above the ceiling", "backup:\n  every: 721h\n", []string{"config: backup.every: 721h is outside"}},
		{"zero keep", "backup:\n  keep: 0\n", []string{"config: backup.keep: 0 is outside [1, 100]"}},
		{"keep above the ceiling", "backup:\n  keep: 101\n", []string{"config: backup.keep: 101 is outside [1, 100]"}},
		{"non-int keep", "backup:\n  keep: many\n", []string{"cannot unmarshal", "many"}},
		{"relative dir", "backup:\n  dir: backups\n", []string{`config: backup.dir must be an absolute path (or start with ~/), got "backups"`}},
		{"~user dir is not expanded", "backup:\n  dir: ~someone/backups\n", []string{"config: backup.dir must be an absolute path"}},
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

func TestLoadFrom_BackupEnv(t *testing.T) {
	isolateUpdateEnv(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	abs := t.TempDir()
	file := writeConfig(t, "backup:\n  auto: false\n  every: 6h\n  keep: 3\n  dir: '"+filepath.ToSlash(abs)+"'\n")

	tests := []struct {
		name    string
		key     string
		env     string
		file    string // "" = no file
		check   func(t *testing.T, b Backup)
		wantErr string
	}{
		{name: "auto true", key: "SEAMLESS_BACKUP_AUTO", env: "true", check: func(t *testing.T, b Backup) { require.Equal(t, new(true), b.Auto) }},
		{name: "auto ParseBool spelling, trimmed", key: "SEAMLESS_BACKUP_AUTO", env: " 0 ", check: func(t *testing.T, b Backup) { require.Equal(t, new(false), b.Auto) }},
		{name: "auto env wins over file", key: "SEAMLESS_BACKUP_AUTO", env: "true", file: file, check: func(t *testing.T, b Backup) { require.Equal(t, new(true), b.Auto) }},
		{name: "auto garbage is an error, never false", key: "SEAMLESS_BACKUP_AUTO", env: "nope", wantErr: "config: env SEAMLESS_BACKUP_AUTO: "},
		{name: "auto set but empty is an error, never unset", key: "SEAMLESS_BACKUP_AUTO", env: "", wantErr: "config: env SEAMLESS_BACKUP_AUTO: "},

		{name: "every hours", key: "SEAMLESS_BACKUP_EVERY", env: "168h", check: func(t *testing.T, b Backup) { require.Equal(t, 168*time.Hour, b.Every.Std()) }},
		{name: "every env wins over file", key: "SEAMLESS_BACKUP_EVERY", env: " 12h ", file: file, check: func(t *testing.T, b Backup) { require.Equal(t, 12*time.Hour, b.Every.Std()) }},
		{name: "every bare number needs a unit", key: "SEAMLESS_BACKUP_EVERY", env: "24", wantErr: `config: env SEAMLESS_BACKUP_EVERY: invalid duration "24": needs a unit`},
		{name: "every set but empty", key: "SEAMLESS_BACKUP_EVERY", env: "", wantErr: "config: env SEAMLESS_BACKUP_EVERY: "},
		{name: "every bare zero parses, then fails the range", key: "SEAMLESS_BACKUP_EVERY", env: "0", wantErr: "config: backup.every: 0s is outside [1h, 720h]"},

		{name: "keep", key: "SEAMLESS_BACKUP_KEEP", env: " 30 ", check: func(t *testing.T, b Backup) { require.Equal(t, 30, b.Keep) }},
		{name: "keep env wins over file", key: "SEAMLESS_BACKUP_KEEP", env: "100", file: file, check: func(t *testing.T, b Backup) { require.Equal(t, 100, b.Keep) }},
		{name: "keep garbage", key: "SEAMLESS_BACKUP_KEEP", env: "lots", wantErr: "config: env SEAMLESS_BACKUP_KEEP: "},
		{name: "keep out of range", key: "SEAMLESS_BACKUP_KEEP", env: "0", wantErr: "config: backup.keep: 0 is outside [1, 100]"},

		{name: "dir absolute", key: "SEAMLESS_BACKUP_DIR", env: abs, check: func(t *testing.T, b Backup) { require.Equal(t, abs, b.Dir) }},
		{name: "dir ~ expands", key: "SEAMLESS_BACKUP_DIR", env: "~/Dropbox/Seamless Backups", check: func(t *testing.T, b Backup) {
			require.Equal(t, filepath.Join(home, "Dropbox", "Seamless Backups"), b.Dir)
		}},
		{name: "dir set but empty is the default folder, over the file", key: "SEAMLESS_BACKUP_DIR", env: "", file: file, check: func(t *testing.T, b Backup) { require.Empty(t, b.Dir) }},
		{name: "dir relative", key: "SEAMLESS_BACKUP_DIR", env: "backups", wantErr: "config: backup.dir must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.env)
			cfg, err := LoadFrom(tt.file)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, cfg.Backup)
		})
	}

	t.Run("unset env leaves the file values", func(t *testing.T) {
		cfg, err := LoadFrom(file)
		require.NoError(t, err)
		require.Equal(t, Backup{Auto: new(false), Every: Duration(6 * time.Hour), Keep: 3, Dir: filepath.ToSlash(abs)}, cfg.Backup)

		cfg, err = LoadFrom("")
		require.NoError(t, err)
		require.Equal(t, Defaults().Backup, cfg.Backup)
	})
}

func TestBackupOverrideValidate(t *testing.T) {
	abs := t.TempDir()
	every := func(d time.Duration) *Duration { v := Duration(d); return &v }
	tests := []struct {
		name    string
		o       BackupOverride
		wantErr string // "" = valid
	}{
		{"empty overrides nothing", BackupOverride{}, ""},
		{"every field set, in range", BackupOverride{Auto: new(false), Every: every(time.Hour), Keep: new(100), Dir: new(abs)}, ""},
		{"an empty dir is the default folder", BackupOverride{Dir: new("")}, ""},
		{"every out of range", BackupOverride{Every: every(30 * time.Minute)}, "config: backup.every: 30m is outside [1h, 720h]"},
		{"every zero", BackupOverride{Every: every(0)}, "config: backup.every: 0s is outside [1h, 720h]"},
		{"keep zero", BackupOverride{Keep: new(0)}, "config: backup.keep: 0 is outside [1, 100]"},
		{"keep over the ceiling", BackupOverride{Keep: new(101)}, "config: backup.keep: 101 is outside [1, 100]"},
		// A stored dir is never expanded, so it does not offer ~/ as a way out.
		{"a ~ dir is not expanded", BackupOverride{Dir: new("~/Backups")}, `config: backup.dir must be an absolute path, got "~/Backups"`},
		{"a relative dir", BackupOverride{Dir: new("backups")}, `config: backup.dir must be an absolute path, got "backups"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.o.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

// TestBackupOverrideJSON pins the stored row's shape: unset fields are left
// out, every is a duration string, and a set but empty dir survives the round
// trip (it chooses the default folder, which is not the same as no override).
func TestBackupOverrideJSON(t *testing.T) {
	out, err := json.Marshal(BackupOverride{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(out))

	every := Duration(6 * time.Hour)
	o := BackupOverride{Auto: new(false), Every: &every, Keep: new(14), Dir: new("")}
	out, err = json.Marshal(o)
	require.NoError(t, err)
	require.JSONEq(t, `{"auto":false,"every":"6h","keep":14,"dir":""}`, string(out))

	var back BackupOverride
	require.NoError(t, json.Unmarshal(out, &back))
	require.Equal(t, o, back)

	// A number is never read as nanoseconds.
	require.ErrorContains(t, json.Unmarshal([]byte(`{"every":21600000000000}`), &back), "cannot unmarshal number")
}
