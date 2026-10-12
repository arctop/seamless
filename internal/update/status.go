package update

import (
	"fmt"
	"strings"
	"time"
)

// Notice windows: how long the briefing mentions an update.
const (
	// updatedNoticeWindow is how long after a version change the briefing
	// says the daemon updated.
	updatedNoticeWindow = 24 * time.Hour
	// availableNoticeWindow is how long the briefing mentions one available
	// release, counted from when this install first saw it. After that the
	// console and doctor still show it; sessions stop hearing about it.
	availableNoticeWindow = 7 * 24 * time.Hour
)

// Mode is what the update machinery does on this install.
type Mode string

const (
	// ModeOff: no update checks at all.
	ModeOff Mode = "off"
	// ModeNotify: check and tell the owner; installing stays theirs.
	ModeNotify Mode = "notify"
	// ModeAuto: check, and install a newer release by itself (plan 2.4):
	// after its soak, when the daemon is idle or its deadline has come.
	ModeAuto Mode = "auto"
)

// Status is the one snapshot of the update check that every surface renders:
// the briefing notice, the console Updates section and health strip, doctor,
// /healthz. The checker owns it; readers get a copy.
type Status struct {
	// The running build.
	Version      string    `json:"version"` // as built, e.g. "0.7.2"
	Distribution string    `json:"distribution"`
	Release      bool      `json:"release"` // IsReleaseBuild
	Instance     string    `json:"instance"`
	StartedAt    time.Time `json:"started_at"`
	Install      Install   `json:"install"`
	Settings     Settings  `json:"settings"`

	// What the last successful check saw. Newest is nil until one succeeds.
	Newest      *Release    `json:"newest,omitempty"`
	Available   bool        `json:"available"` // Newest is newer than Version
	SeenAt      time.Time   `json:"seen_at,omitzero"`
	CheckedAt   time.Time   `json:"checked_at,omitzero"`
	NextCheckAt time.Time   `json:"next_check_at,omitzero"`
	CheckError  *CheckError `json:"check_error,omitempty"`

	// Updated is the last version change observed at startup (release
	// builds only).
	Updated *Updated `json:"updated,omitempty"`

	// Stopped is true when the checker's loop died (a recovered panic); the
	// daemon keeps serving and the surfaces say the check is not running.
	Stopped bool `json:"stopped,omitempty"`

	// Automatic updates (plan 2.4). Applying, LastAttempt, Hold, Paused,
	// Blocked and Backoff show whatever the mode (an Update now runs in any
	// mode, a manual `seamlessd update` too); Target, PendingSince, Waiting
	// and WaitCode are only set while the daemon updates itself (ModeAuto).

	// CanApply reports whether this daemon has an updater wired (a
	// Spawner). Without one it installs nothing, whatever Settings.Auto says.
	CanApply bool `json:"can_apply,omitempty"`
	// Target is the release an automatic update would install now (Target):
	// soaked, not held or blocked, carrying the checksums bundle. nil when
	// there is none.
	Target *Release `json:"target,omitempty"`
	// PendingSince is when the pending update began waiting for this running
	// version, on GitHub's clock; the deadline is PendingSince plus
	// Settings.MaxDefer. Zero when nothing is pending.
	PendingSince time.Time `json:"pending_since,omitzero"`
	// Waiting says for the owner why nothing is being installed right now
	// (an update in progress, live sessions, a soak, a hold...), and WaitCode
	// is the same as a Wait* word. Both are empty when nothing is pending,
	// and when the pending update starts at the next tick.
	Waiting  string `json:"waiting,omitempty"`
	WaitCode string `json:"wait_code,omitempty"`
	// Applying is the update under way, if one is.
	Applying *Applying `json:"applying,omitempty"`
	// LastAttempt is the newest attempt the daemon folded, automatic or
	// manual. Its Error is for the owner's eyes; no notice uses it.
	LastAttempt *AttemptResult `json:"last_attempt,omitempty"`
	// Hold is the skip-through a deliberate downgrade set; Resume or Update
	// now lifts it.
	Hold *Hold `json:"hold,omitempty"`
	// Paused is set while automatic updates have turned themselves off
	// after repeated rollbacks or a failed one; Resume lifts it.
	Paused *Pause `json:"paused,omitempty"`
	// Blocked lists the releases above the running version that automatic
	// updates skip.
	Blocked []Block `json:"blocked,omitempty"`
	// Backoff is set while a failed attempt delays the next one.
	Backoff *Backoff `json:"backoff,omitempty"`
}

// Applying is the update under way: an attempt the updater reports live, or
// one this daemon spawned that has not reported yet (or whose updater still
// holds the lock).
type Applying struct {
	ID   string  `json:"id"`
	From Version `json:"from"`
	To   Version `json:"to"`
	Why  string  `json:"why"`
	// Stage is the updater's latest stage; "" until it writes a record.
	Stage Stage `json:"stage,omitempty"`
	// SpawnedAt is when this daemon spawned it; zero for one it did not (a
	// manual `seamlessd update`). HeartbeatAt is the record's latest write.
	SpawnedAt   time.Time `json:"spawned_at,omitzero"`
	HeartbeatAt time.Time `json:"heartbeat_at,omitzero"`
}

// Mode reports what the update machinery does here and why, in words for the
// owner. It is ModeAuto only when every one of these holds: checks are on,
// the owner allows automatic updates (Settings.Auto), this is a release build
// the installer made (Release, and Install.NotifyOnly is false), an updater is
// wired (CanApply), and automatic updates have not paused themselves
// (Paused). Otherwise a mode that checks is ModeNotify, and the reason names
// what is missing.
func (s Status) Mode() (Mode, string) {
	if !s.Settings.Check {
		switch {
		case s.Settings.CheckLocked:
			return ModeOff, "update checks are off (update.check: false in the config file or environment)"
		case s.Settings.CheckSource == SourceConsole:
			return ModeOff, "update checks are off (turned off in the console)"
		case !s.Release && s.Distribution == DistributionRelease:
			return ModeOff, "update checks are off: this build is not a published release (a snapshot) and does not check unless update.check is true"
		case !s.Release:
			return ModeOff, "update checks are off: builds from source do not check unless update.check is true"
		default:
			return ModeOff, "update checks are off"
		}
	}
	if s.Install.Kind != KindInstaller && s.Install.Reason != "" {
		return ModeNotify, "told about new releases, never updated unattended: " + s.Install.Reason
	}
	told := "this install is told about new releases; installing one is your call (" + s.Install.Hint + ")"
	switch {
	case s.Install.NotifyOnly(), !s.CanApply, !s.Release:
		return ModeNotify, told
	case !s.Settings.Auto && s.Settings.AutoLocked:
		return ModeNotify, "automatic updates are off (update.auto: false in the config file or environment), so " + told
	case !s.Settings.Auto && s.Settings.AutoSource == SourceConsole:
		return ModeNotify, "automatic updates are off (turned off in the console), so " + told
	case !s.Settings.Auto:
		return ModeNotify, "automatic updates are off, so " + told
	case s.Paused != nil:
		return ModeNotify, "automatic updates paused themselves (" + PauseWords(s.Paused.Reason) +
			"), so " + told + " until they are resumed in the console"
	}
	reason := "installs new releases by itself"
	if s.Settings.MinAge > 0 {
		reason += " once they are " + SpanWords(s.Settings.MinAge) + " old"
	}
	reason += ", when no agent session is live, or at a lull in requests after waiting " + SpanWords(s.Settings.MaxDefer)
	if h := s.Hold; h != nil {
		reason += fmt.Sprintf("; releases up to v%s are skipped because this install went back from v%s, until automatic updates are resumed in the console",
			h.Through, h.From)
	}
	return ModeAuto, reason
}

// Current returns the running version parsed, and false for a build whose
// version is not a clean release (a dev or snapshot build).
func (s Status) Current() (Version, bool) { return Parse(s.Version) }

// Confirming reports whether the update under way has already put its
// version in place: this daemon runs Applying.To, and the updater is only
// confirming that it answers (not rolling it back). The surfaces then say
// the update is being confirmed, not that the version it replaced is
// updating.
func (s Status) Confirming() bool {
	a := s.Applying
	if a == nil || a.Stage == StageRollback {
		return false
	}
	cur, ok := s.Current()
	return ok && cur == a.To
}

// Notice renders the one briefing line about updates for a session on host
// ("" or localHost is the daemon's own machine), or "" when there is nothing
// to say. At most one line, first match wins:
//
//  1. automatic updates paused themselves (repeated rollbacks, or a failed
//     one), within a week of pausing;
//  2. an automatic update failed in a way that blocks its release, and the
//     daemon still runs an older one, within a week of the failure;
//  3. the daemon updated within the last day -- saying so, locally, when
//     Codex's hooks changed with it and need re-approving;
//  4. a newer release is available and this install will not install it by
//     itself -- it does not update itself (Mode is not ModeAuto), or the
//     release is blocked or carries no checksums bundle -- within a week of
//     first seeing it. A release a hold skips is the owner's own pin, and no
//     news.
//
// The text is assembled from parsed versions, the constant release URL, fixed
// words and the fixed hint for the install's kind -- never from the release
// API's text or an updater's error (constraint
// update-surfaces-render-parsed-versions-only) -- and it is worded as the
// owner's action, so an agent that reads it does not decide to restart the
// daemon under every other session. Every line passes the briefing's
// sanitizeField unchanged; the status tests pin that.
func (s Status) Notice(host, localHost string, now time.Time) string {
	remote := host != "" && !strings.EqualFold(host, localHost)

	if line := s.troubleNotice(remote, now); line != "" {
		return line
	}

	if u := s.Updated; u != nil && u.Direction == DirectionUpgrade {
		if age := now.Sub(u.At); age >= 0 && age < updatedNoticeWindow {
			if remote {
				return fmt.Sprintf("The Seamless server updated to v%s (from v%s) %s ago. "+
					"The Seamless client on this machine updates separately; owner action, not a task for this session.",
					u.To, u.From, compactAge(age))
			}
			if u.CodexHooks {
				return fmt.Sprintf("Seamless updated to v%s (from v%s) %s ago, and its Codex hooks changed: "+
					"Codex runs them again once the owner re-approves them in Codex's /hooks. Release notes: %s",
					u.To, u.From, compactAge(age), ReleaseURL(u.To))
			}
			return fmt.Sprintf("Seamless updated to v%s (from v%s) %s ago. Release notes: %s",
				u.To, u.From, compactAge(age), ReleaseURL(u.To))
		}
	}

	if !s.Settings.Check || !s.Available || s.Newest == nil {
		return ""
	}
	if mode, _ := s.Mode(); mode == ModeAuto {
		v := s.Newest.Version
		if held(s.Hold, v) || (s.Newest.ChecksumsBundle && blocked(s.Blocked, v) == nil) {
			// It installs this one by itself, or the owner pinned below it.
			return ""
		}
	}
	if age := now.Sub(s.SeenAt); age < 0 || age >= availableNoticeWindow {
		return ""
	}
	cur, ok := s.Current()
	if !ok {
		return "" // Available is never set for a dev build; belt and braces
	}
	if remote {
		return fmt.Sprintf("The Seamless server can update to v%s (it runs v%s). Owner action on the server, not a task for this session: %s",
			s.Newest.Version, cur, s.Install.Hint)
	}
	return fmt.Sprintf("Seamless v%s is available (running v%s). Owner action, not a task for this session: %s",
		s.Newest.Version, cur, s.Install.Hint)
}

// troubleNotice is the line for automatic updates in trouble -- paused, or
// a release blocked by a failed attempt -- or "". It says nothing once the
// owner has turned automatic updates (or checks) off: that is their answer.
func (s Status) troubleNotice(remote bool, now time.Time) string {
	if !s.Settings.Check || !s.Settings.Auto {
		return ""
	}
	recent := func(t time.Time) bool {
		age := now.Sub(t)
		return age >= 0 && age < availableNoticeWindow
	}
	if p := s.Paused; p != nil && recent(p.At) {
		if remote {
			return fmt.Sprintf("The Seamless server paused its automatic updates (%s). "+
				"Owner action on the server, not a task for this session.", PauseWords(p.Reason))
		}
		return fmt.Sprintf("Seamless paused its automatic updates (%s). Owner action, not a task for this session: "+
			"run seamlessd doctor, then resume them in the console under Settings > Updates.", PauseWords(p.Reason))
	}
	a := s.LastAttempt
	if a == nil || !spawnedByDaemon(a.Why) || !recent(a.FoldedAt) {
		return ""
	}
	b := blocked(s.Blocked, a.To)
	cur, ok := s.Current()
	if b == nil || !ok || cur.Compare(a.To) >= 0 {
		return ""
	}
	if remote {
		return fmt.Sprintf("The Seamless server could not update itself to v%s (%s) and stays on v%s. "+
			"Owner action on the server, not a task for this session.", a.To, BlockWords(b.Reason), cur)
	}
	return fmt.Sprintf("Seamless could not update itself to v%s (%s) and stays on v%s; automatic updates skip that release. "+
		"Owner action, not a task for this session: see Settings > Updates in the console, or run seamlessd doctor.",
		a.To, BlockWords(b.Reason), cur)
}

// compactAge renders a duration the way the briefing does elsewhere: "45m",
// "5h", "3d". Anything under a minute is "1m". It floors, which suits an age;
// a span still to go is UntilWords'.
func compactAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(1, int(d/time.Minute)))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
