package backup

import (
	"path/filepath"
	"time"

	"github.com/arctop/seamless/internal/config"
)

// Where a setting's value came from, as the console and doctor name it.
const (
	SourceDefault = "default" // nothing set it: the build decides Auto, the rest are the defaults
	SourceConfig  = "config"  // seamless.yaml or a SEAMLESS_BACKUP_* variable
	SourceConsole = "console" // the console's stored override
)

// LocalDirName is <data_dir>/backups: the default folder, where safety backups
// taken before a restore and the updater's pre-update backups always go, and
// where the service keeps its own records (.schedule.json, .staging-*).
// archive.Export walks only memory/ and notes/, so a backup never ends up
// inside the next one.
const LocalDirName = "backups"

// LocalDir is <dataDir>/backups.
func LocalDir(dataDir string) string { return filepath.Join(dataDir, LocalDirName) }

// Settings is the backup behavior in force: the file/env config with the
// console's stored override layered on (Effective).
type Settings struct {
	// Auto is whether scheduled backups run.
	Auto       bool
	AutoSource string
	// Every is the time between scheduled backups.
	Every       time.Duration
	EverySource string
	// Keep is how many of this install's automatic backups stay in Dir.
	Keep       int
	KeepSource string
	// Dir is the folder backups go to, absolute and cleaned: the configured
	// folder, or LocalDir when none is set.
	Dir       string
	DirSource string
	// DirIsLocal is true when Dir is LocalDir: the data dir's own folder.
	DirIsLocal bool
}

// Effective merges the file/env backup config with the console's override.
// Like the briefing and features overrides, and unlike the update override, a
// field the console set simply wins over file and env until it is reset.
//
//   - Auto: the override, else the file/env key, else the build decides: on for
//     a release build, off for a build from source, so a throwaway daemon
//     (make run, a fixture, a benchmark arm, CI) never writes backups.
//   - Every and Keep: the override, else the file/env value. A loaded config is
//     validated, so its values are in range; a caller that built base by hand
//     with a zero value gets the default, and one out of range is clamped,
//     never trusted. An override value out of range is ignored (store
//     validates the row, so only a hand-built override has one).
//   - Dir: the override (a set but empty one means LocalDir even when the file
//     names another folder), else the file/env value, else LocalDir. A
//     relative dir cannot be honored (relative to what?) and reads as unset.
//
// The source of Every and Keep is SourceConfig only when the value differs
// from the default: a loaded config cannot say whether a key equal to its
// default was written, and "default" is the truer word for both.
func Effective(base config.Backup, override config.BackupOverride, release bool, dataDir string) Settings {
	local := filepath.Clean(LocalDir(dataDir))
	s := Settings{
		Auto: release, AutoSource: SourceDefault,
		Every: config.DefaultBackupEvery, EverySource: SourceDefault,
		Keep: config.DefaultBackupKeep, KeepSource: SourceDefault,
		Dir: local, DirSource: SourceDefault,
	}

	switch {
	case override.Auto != nil:
		s.Auto, s.AutoSource = *override.Auto, SourceConsole
	case base.Auto != nil:
		s.Auto, s.AutoSource = *base.Auto, SourceConfig
	}

	if v := base.Every.Std(); v > 0 {
		s.Every = clampDuration(v, config.MinBackupEvery, config.MaxBackupEvery)
		if s.Every != config.DefaultBackupEvery {
			s.EverySource = SourceConfig
		}
	}
	if o := override.Every; o != nil && inRange(o.Std(), config.MinBackupEvery, config.MaxBackupEvery) {
		s.Every, s.EverySource = o.Std(), SourceConsole
	}

	if base.Keep > 0 {
		s.Keep = min(max(base.Keep, config.MinBackupKeep), config.MaxBackupKeep)
		if s.Keep != config.DefaultBackupKeep {
			s.KeepSource = SourceConfig
		}
	}
	if o := override.Keep; o != nil && *o >= config.MinBackupKeep && *o <= config.MaxBackupKeep {
		s.Keep, s.KeepSource = *o, SourceConsole
	}

	if filepath.IsAbs(base.Dir) {
		s.Dir, s.DirSource = filepath.Clean(base.Dir), SourceConfig
	}
	if o := override.Dir; o != nil {
		switch {
		case *o == "":
			s.Dir, s.DirSource = local, SourceConsole
		case filepath.IsAbs(*o):
			s.Dir, s.DirSource = filepath.Clean(*o), SourceConsole
		}
	}
	s.DirIsLocal = s.Dir == local
	return s
}

func inRange(v, lo, hi time.Duration) bool { return v >= lo && v <= hi }

func clampDuration(v, lo, hi time.Duration) time.Duration { return min(max(v, lo), hi) }
