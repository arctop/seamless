package config

import (
	"fmt"
	"path/filepath"
	"time"
)

// Scheduled backups: the defaults, and the bounds Backup.Validate enforces.
const (
	// DefaultBackupEvery is the time between scheduled backups: daily.
	DefaultBackupEvery = 24 * time.Hour
	// MinBackupEvery is the shortest allowed interval. A backup archives the
	// whole instance, so anything tighter than hourly spends more disk and
	// I/O than the work it protects is worth.
	MinBackupEvery = time.Hour
	// MaxBackupEvery is the longest allowed interval: 30 days.
	MaxBackupEvery = 720 * time.Hour

	// DefaultBackupKeep is how many automatic backups are kept.
	DefaultBackupKeep = 7
	// MinBackupKeep is the fewest automatic backups kept: the newest one.
	MinBackupKeep = 1
	// MaxBackupKeep is the most automatic backups kept.
	MaxBackupKeep = 100
)

// Backup configures scheduled backups: on a schedule, the daemon archives the
// whole instance (the database snapshot, the memory and note corpus, and a
// manifest of what it holds) into one folder, and keeps the newest Keep
// automatic backups there. When to run, what to skip and what to prune are
// internal/backup's calls; config only carries the settings.
//
// The console's runtime override (BackupOverride, stored by
// store.SetBackupOverride) simply wins over these values, field by field,
// until the owner resets it, like the briefing and features rows. Settings >
// Backups in the console is the recommended place to set them: a backup: key
// in the file pins it to a release that knows the block (AGENTS.md "Release
// discipline").
type Backup struct {
	// Auto turns scheduled backups on or off. nil (the key unset) means the
	// build decides: on for release builds, off for builds from source, so a
	// throwaway daemon (make run, a fixture, a benchmark arm) never writes
	// backups. That call is internal/backup's, not config's.
	Auto *bool `yaml:"auto"`
	// Every is the time between scheduled backups, within [MinBackupEvery,
	// MaxBackupEvery].
	Every Duration `yaml:"every"`
	// Keep is how many automatic backups to keep, within [MinBackupKeep,
	// MaxBackupKeep]. Backups the owner makes, safety backups taken before a
	// restore and the updater's pre-update backups are never pruned.
	Keep int `yaml:"keep"`
	// Dir is the folder backups are written to: an absolute path (a leading ~
	// expands at load). Empty means <data_dir>/backups. Safety backups taken
	// before a restore always stay in <data_dir>/backups whatever Dir says.
	// Whether the folder is usable (it exists or can be made, is writable, is
	// not inside the data dir) is judged when a backup runs, not here, so a
	// disconnected disk never stops the daemon from starting.
	Dir string `yaml:"dir"`
}

// Validate rejects a backup block the daemon cannot honor: every outside
// [MinBackupEvery, MaxBackupEvery], keep outside [MinBackupKeep,
// MaxBackupKeep], or a dir that is set but not absolute. A zero every or keep
// is out of range, never "use the default": a default applies only when its
// key is absent. Every state of auto is valid.
func (b Backup) Validate() error {
	if err := validateBackupEvery(b.Every); err != nil {
		return err
	}
	if err := validateBackupKeep(b.Keep); err != nil {
		return err
	}
	return validateBackupDir(b.Dir, " (or start with ~/)")
}

// BackupOverride is the console's runtime override of the backup settings,
// stored as JSON in the settings table (store.BackupOverride). A nil field is
// no override: that setting follows the file/env value. A set field wins over
// file and env until the owner resets the override. Every travels as a
// duration string ("24h"), never as nanoseconds. A set but empty Dir chooses
// <data_dir>/backups even when the file names another folder.
type BackupOverride struct {
	Auto  *bool     `json:"auto,omitempty"`
	Every *Duration `json:"every,omitempty"`
	Keep  *int      `json:"keep,omitempty"`
	Dir   *string   `json:"dir,omitempty"`
}

// Validate rejects an override holding a value Backup.Validate would refuse:
// each set field must lie within the same bounds as its file/env key. A stored
// Dir is never expanded, so it must already be absolute (or empty). Unset
// fields are not judged.
func (o BackupOverride) Validate() error {
	if o.Every != nil {
		if err := validateBackupEvery(*o.Every); err != nil {
			return err
		}
	}
	if o.Keep != nil {
		if err := validateBackupKeep(*o.Keep); err != nil {
			return err
		}
	}
	if o.Dir != nil {
		return validateBackupDir(*o.Dir, "")
	}
	return nil
}

func validateBackupEvery(d Duration) error {
	if v := d.Std(); v < MinBackupEvery || v > MaxBackupEvery {
		return fmt.Errorf("config: backup.every: %s is outside [%s, %s]",
			d, Duration(MinBackupEvery), Duration(MaxBackupEvery))
	}
	return nil
}

func validateBackupKeep(n int) error {
	if n < MinBackupKeep || n > MaxBackupKeep {
		return fmt.Errorf("config: backup.keep: %d is outside [%d, %d]", n, MinBackupKeep, MaxBackupKeep)
	}
	return nil
}

// validateBackupDir refuses a set but relative dir. hint follows "absolute
// path" in the message: the file's key may also start with ~/, which load
// expands, and the stored override's may not.
func validateBackupDir(dir, hint string) error {
	if dir != "" && !filepath.IsAbs(dir) {
		return fmt.Errorf("config: backup.dir must be an absolute path%s, got %q", hint, dir)
	}
	return nil
}
