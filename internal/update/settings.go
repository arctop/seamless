package update

import (
	"time"

	"github.com/arctop/seamless/internal/config"
)

// Where a setting's value came from, as the console and doctor name it.
const (
	SourceDefault = "default" // nothing set it: the build decides Check, and Auto is on
	SourceConfig  = "config"  // seamless.yaml or a SEAMLESS_UPDATE_* variable
	SourceConsole = "console" // the console's stored override
	SourceCheck   = "check"   // Auto only: off because update checks are off
)

// Settings is the update behavior in force: the file/env config with the
// console's stored override layered on (Effective).
type Settings struct {
	// Check is whether the daemon asks GitHub at all. false means no update
	// traffic of any kind.
	Check bool
	// CheckSource names who decided Check (SourceDefault, SourceConfig or
	// SourceConsole), and CheckLocked is true when the console may not turn
	// it on: the file or environment set it false.
	CheckSource string
	CheckLocked bool
	// CheckInterval is the time between scheduled checks.
	CheckInterval time.Duration

	// Auto is whether the owner lets the daemon install a newer release by
	// itself. It is a permission, not a promise: only an install that
	// updates itself ever acts on it (Install.NotifyOnly), and it is false
	// whenever Check is, since with no update traffic there is nothing to
	// install.
	Auto bool
	// AutoSource names who decided Auto (SourceDefault, SourceConfig,
	// SourceConsole, or SourceCheck when checks being off did), and
	// AutoLocked is true when the console may not turn it on: the file or
	// environment set update.auto false, or locked update.check off. While
	// checks are off but not locked, a console choice for Auto is kept and
	// applies once they are back on.
	AutoSource string
	AutoLocked bool
	// MaxDefer is how long a pending automatic update waits for an idle
	// daemon before it takes the next lull in requests instead.
	MaxDefer time.Duration
	// MinAge is the soak: how long a release must have been published before
	// an automatic update takes it. Zero is no soak.
	MinAge time.Duration
	// ReleaseSigners is update.release_signers, the file that pins which
	// keys must have signed a release for the updater to install it; "" when
	// nothing is pinned. It comes from the file or the environment only: the
	// console can neither set nor clear it.
	ReleaseSigners string `json:"ReleaseSigners,omitempty"`
}

// Effective merges the file/env update config with the console's override.
// Unlike the briefing and features overrides, where the stored row simply
// wins, here the MORE RESTRICTIVE setting wins, for check and auto alike:
//
//   - an explicit file/env false is final: the override cannot turn it on,
//     and the console shows the toggle locked with where it was set -- so
//     "check: false means no network" is never one click from untrue;
//   - an explicit file/env true can still be turned off from the console;
//   - with the key unset the console is free either way, and with no override
//     either, a release build checks and a source build does not, and auto is
//     on.
//
// Auto also yields to Check: with checks off nothing can be fetched to
// install, so Auto is false (SourceCheck), locked exactly when Check is. Its
// own file/env false is named first, since that one still holds once checks
// come back on. Unset auto is on for every build: Settings say what the owner
// allows, and whether this install can update itself is Detect's verdict
// (Install.NotifyOnly), which already rules out source builds.
//
// The durations come from base. A zero CheckInterval or MaxDefer -- a caller
// that never loaded a config, since a loaded one is validated nonzero -- falls
// back to its default. MinAge passes through: zero is a valid soak (none), so
// a caller building base by hand sets it, as config.Defaults does.
//
// release is IsReleaseBuild for the running binary.
func Effective(base config.Update, override config.UpdateOverride, release bool) Settings {
	s := Settings{
		CheckInterval:  base.CheckInterval.Std(),
		MaxDefer:       base.MaxDefer.Std(),
		MinAge:         base.MinAge.Std(),
		ReleaseSigners: base.ReleaseSigners,
	}
	if s.CheckInterval <= 0 {
		s.CheckInterval = config.DefaultUpdateCheckInterval
	}
	if s.MaxDefer <= 0 {
		s.MaxDefer = config.DefaultUpdateMaxDefer
	}
	switch {
	case base.Check != nil && !*base.Check:
		s.Check, s.CheckSource, s.CheckLocked = false, SourceConfig, true
	case override.Check != nil:
		s.Check, s.CheckSource = *override.Check, SourceConsole
	case base.Check != nil:
		s.Check, s.CheckSource = true, SourceConfig
	default:
		s.Check, s.CheckSource = release, SourceDefault
	}
	switch {
	case base.Auto != nil && !*base.Auto:
		s.Auto, s.AutoSource, s.AutoLocked = false, SourceConfig, true
	case !s.Check:
		s.Auto, s.AutoSource, s.AutoLocked = false, SourceCheck, s.CheckLocked
	case override.Auto != nil:
		s.Auto, s.AutoSource = *override.Auto, SourceConsole
	case base.Auto != nil:
		s.Auto, s.AutoSource = true, SourceConfig
	default:
		s.Auto, s.AutoSource = true, SourceDefault
	}
	return s
}
