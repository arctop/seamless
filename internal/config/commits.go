package config

import (
	"fmt"
	"path/filepath"
	"time"
)

// Commits is commit records, off by default: agents may name the commits they
// made, and the daemon may write a snapshot of its sessions, tasks and those
// commits for an integrity monitor that matches a branch's commits to the
// tracked work that made them. Off is exactly the previous behavior:
// session_end and tasks_update do not offer the commits argument (a call that
// sends one is refused as an unknown parameter), nothing is recorded, and no
// snapshot is written. The two halves switch on separately: a snapshot without
// recorded commits still tells a monitor which sessions ran when, and where.
type Commits struct {
	// Record offers the commits argument on session_end and tasks_update: the
	// full ids (40 or 64 hex digits) of the commits a session made or a task's
	// work produced, recorded on that session or task for the snapshot. Off --
	// the default -- the argument is not offered.
	Record bool `yaml:"record"`
	// SnapshotFile is where the daemon writes the commit-matching snapshot:
	// one JSON document, replaced whole on every write (written beside it,
	// then renamed into place, mode 0640). It lists the sessions live in the
	// snapshot window and the tasks a commit in it may name, each with its
	// repository and the commits it recorded -- identifiers and times only.
	// Its directory must exist. Empty -- the default -- writes none. A leading
	// ~ expands.
	SnapshotFile string `yaml:"snapshot_file"`
	// SnapshotInterval is how often the snapshot is rewritten (it is also
	// written once at start). The default is 5m.
	SnapshotInterval Duration `yaml:"snapshot_interval"`
	// SnapshotWindow is how far back the snapshot reaches: its since is this
	// long before it was taken, or the start of the first session when that is
	// later. A commit seen before since is outside what the snapshot covers.
	// The default is 720h (30 days).
	SnapshotWindow Duration `yaml:"snapshot_window"`
}

// The commits block's defaults.
const (
	DefaultCommitsSnapshotInterval = Duration(5 * time.Minute)
	DefaultCommitsSnapshotWindow   = Duration(30 * 24 * time.Hour)
)

// Bounds. Below the interval floor the writer would rebuild faster than a
// monitor reads; above the ceiling the snapshot would lag a whole day. The
// window spans an hour to a year: a branch merged after its sessions left the
// window loses its time match, and past a year the snapshot outgrows what a
// consumer reads.
const (
	minCommitsSnapshotInterval = Duration(30 * time.Second)
	maxCommitsSnapshotInterval = Duration(24 * time.Hour)
	minCommitsSnapshotWindow   = Duration(time.Hour)
	maxCommitsSnapshotWindow   = Duration(365 * 24 * time.Hour)
)

// Validate rejects a present-but-unusable commits block: a relative snapshot
// path, or an interval or window outside its bounds (0 included: the empty
// snapshot_file is the off switch, not a zero interval).
func (c Commits) Validate() error {
	if c.SnapshotFile != "" && !filepath.IsAbs(c.SnapshotFile) {
		return fmt.Errorf("config: commits.snapshot_file must be an absolute path (or start with ~/), got %q", c.SnapshotFile)
	}
	if c.SnapshotInterval < minCommitsSnapshotInterval || c.SnapshotInterval > maxCommitsSnapshotInterval {
		return fmt.Errorf("config: commits.snapshot_interval must be in [%s, %s]",
			minCommitsSnapshotInterval, maxCommitsSnapshotInterval)
	}
	if c.SnapshotWindow < minCommitsSnapshotWindow || c.SnapshotWindow > maxCommitsSnapshotWindow {
		return fmt.Errorf("config: commits.snapshot_window must be in [%s, %s]",
			minCommitsSnapshotWindow, maxCommitsSnapshotWindow)
	}
	return nil
}
