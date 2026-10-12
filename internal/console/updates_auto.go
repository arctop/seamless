package console

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/update"
)

// The console's view of automatic updates (plan 2.4): the Settings > Updates
// rows for the switch, the pending update, the one under way, the last attempt
// and what holds automatic updates back, plus the failure banner every page
// carries.
//
// Every word here is fixed or built from a parsed update.Version. The one free
// text is an attempt's Error, the updater's own summary for the owner: only the
// Last attempt row in Settings > Updates shows it -- never the banner, a flash,
// a toast, an event summary or the health strip.

// troubleBannerWindow is how long after a pause or a failed attempt every page
// says so: the same week the briefing's trouble line covers (internal/update).
// Settings > Updates keeps showing both after that.
const troubleBannerWindow = 7 * 24 * time.Hour

// selfUpdating reports whether this install can install a release by itself
// at all: a release build the installer made, with an updater wired. Only then
// do the Auto switch, Update now, Resume and the rows about what holds
// automatic updates back mean anything; everywhere else the mode line says
// what is missing.
func selfUpdating(st update.Status) bool {
	return st.Release && !st.Install.NotifyOnly() && st.CanApply
}

// updatesAutoRow is the Automatic updates row and its switch: whether the owner
// lets this install update itself, and who decided.
type updatesAutoRow struct {
	// On is the effective permission (Settings.Auto); Words says who decided
	// and what that means.
	On    bool
	Words string
	// Switch offers the console's switch. It needs checks on: with them off
	// nothing is fetched to install, and the switch could not show the choice
	// that comes back with them. Locked disables turning it on, and LockedNote
	// says where it was set.
	Switch     bool
	Locked     bool
	LockedNote string
}

// autoLockedText names where a locked automatic-updates switch was set: its
// own update.auto: false, or a locked update.check: false (nothing installs
// without checks).
func autoLockedText(set update.Settings) string {
	if set.AutoSource == update.SourceConfig {
		return "update.auto: false is set in the config file or SEAMLESS_UPDATE_AUTO; change it there."
	}
	return updatesLockedText
}

// autoRow builds the Automatic updates row from the effective settings; paused
// is true while automatic updates have paused themselves, which an "on" row
// says.
func autoRow(set update.Settings, paused bool) updatesAutoRow {
	r := updatesAutoRow{On: set.Auto, Switch: set.Check, Locked: set.AutoLocked}
	switch {
	case set.AutoSource == update.SourceCheck:
		r.Words = "Off while update checks are off: with no update traffic there is nothing to install. " +
			"Turning checks back on brings back the setting in force before."
	case set.AutoSource == update.SourceConfig && !set.Auto:
		r.Words = "Off: update.auto: false is set in the config file or environment, so this console cannot turn them on."
	case set.AutoSource == update.SourceConfig:
		r.Words = "On: update.auto: true is set in the config file or environment; you can still turn them off here."
	case set.AutoSource == update.SourceConsole && set.Auto:
		r.Words = "On: turned on in this console, which wins over the config file until you reset it."
	case set.AutoSource == update.SourceConsole:
		r.Words = "Off: turned off in this console. New releases are announced, and installing one is your call."
	default:
		r.Words = "On by default: a new release installs by itself at a quiet moment."
	}
	if paused && r.On {
		r.Words += " For now they paused themselves: see Paused below."
	}
	if r.Switch && r.Locked {
		r.LockedNote = autoLockedText(set)
	}
	return r
}

// updatesNext is the Next update row: the release automatic updates install
// next and what they wait for. It is set only while this install updates
// itself (ModeAuto: update sets Target, PendingSince and Waiting then and only
// then) and no update is under way.
type updatesNext struct {
	// Version is the target ("v0.7.3"), or the newest release while it soaks
	// or is not a target for another reason (no checksums bundle).
	Version string
	// Line says what happens and when: the target "installs by itself" with
	// update's own wait reason, or why the newest release is not a target yet.
	Line string
	// Pending is when the update began waiting for this running version, and
	// Deadline when it stops waiting for an idle daemon and takes the next
	// lull in requests (Pending plus update.max_defer); DeadlineIn says when
	// in words ("in 19h"), or "" once it has passed. Both zero when nothing is
	// pending.
	Pending, Deadline time.Time
	DeadlineIn        string
	// SoakEnds is when the newest release has soaked, while it soaks.
	SoakEnds time.Time
}

// nextRow builds the Next update row, or nil when nothing is pending or the
// newest release is skipped (blocked or held: those rows say so).
func nextRow(st update.Status, now time.Time) *updatesNext {
	var n *updatesNext
	switch {
	case st.Target != nil:
		v := st.Target.Version
		n = &updatesNext{Version: "v" + v.String(), Line: "installs by itself within a minute"}
		if st.Waiting != "" {
			n.Line = "installs by itself: " + st.Waiting
		}
	case st.WaitCode == update.WaitBlocked, st.WaitCode == update.WaitHeld:
		// The Skipped releases and Held back rows say it.
		return nil
	case st.Waiting != "" && st.Available && st.Newest != nil:
		v := st.Newest.Version
		n = &updatesNext{Version: "v" + v.String(), Line: st.Waiting}
		if st.WaitCode == update.WaitSoak && st.Settings.MinAge > 0 {
			n.Line = "installs by itself once it is " + update.SpanWords(st.Settings.MinAge) + " old"
			n.SoakEnds = st.Newest.PublishedAt.Add(st.Settings.MinAge)
		}
	default:
		return nil
	}
	if !st.PendingSince.IsZero() {
		n.Pending = st.PendingSince
		n.Deadline = st.PendingSince.Add(st.Settings.MaxDefer)
		if n.Deadline.After(now) {
			n.DeadlineIn = untilPhrase(n.Deadline, now)
		}
	}
	return n
}

// updatesApplying is the Updating now row: the update under way.
type updatesApplying struct {
	To, From string // "v0.7.3"
	// Stage says what the updater is doing, in words.
	Stage string
	// Confirming is set once this daemon runs To and the updater is only
	// confirming it (update.Status.Confirming): the badge reads confirming,
	// not in progress.
	Confirming bool
	// Why says who started it ("automatic", "Update now").
	Why string
	// Since is when this daemon started it; Heard, the updater's latest word,
	// stands in for an update the daemon did not start (seamlessd update run
	// by hand).
	Since, Heard time.Time
}

// applyingRow builds the Updating now row from st, whose Applying is set.
// While it is confirming, the stage is said from this daemon's side: the
// record may still name the install the new version came up in.
func applyingRow(st update.Status) *updatesApplying {
	a := st.Applying
	r := &updatesApplying{
		To: "v" + a.To.String(), From: "v" + a.From.String(),
		Stage: stageWords(a.Stage), Why: whyWords(a.Why),
		Since: a.SpawnedAt, Heard: a.HeartbeatAt,
	}
	if st.Confirming() {
		r.Confirming = true
		r.Stage = "the new version is serving; the updater is confirming it"
	}
	return r
}

// updatesAttempt is the Last attempt row. Error and LogPath appear here and
// nowhere else in the console.
type updatesAttempt struct {
	// Outcome is the result in words and Tone its badge ("ok", "warn",
	// "danger", or "" for neutral).
	Outcome, Tone string
	To, From      string // "v0.7.3"
	Why           string
	// At is when it finished, or when the daemon settled it.
	At time.Time
	// Detail says what the outcome means for the install, in fixed words.
	Detail string
	// Error is the updater's own summary: owner-only text, escaped on render.
	// A failed rollback's carries the recovery steps and the backup path.
	Error string
	// LogPath is the updater's full log of the attempt.
	LogPath string
	// Rewire is set for an update that applied with warnings: the new version
	// serves, and seamlessd install-hooks re-wires the agents.
	Rewire bool
}

// attemptRow builds the Last attempt row.
func attemptRow(a update.AttemptResult) *updatesAttempt {
	to, from := "v"+a.To.String(), "v"+a.From.String()
	r := &updatesAttempt{
		To: to, From: from, Why: whyWords(a.Why), At: a.FinishedAt,
		Error: strings.TrimSpace(a.Error), LogPath: strings.TrimSpace(a.LogPath),
	}
	if r.At.IsZero() {
		r.At = a.FoldedAt
	}
	switch a.Outcome {
	case update.OutcomeApplied:
		r.Outcome, r.Tone = "applied", "ok"
		r.Detail = to + " is installed, and it answered as the running version."
		if a.Warnings {
			r.Outcome, r.Tone, r.Rewire = "applied with warnings", "warn", true
			r.Detail = to + " serves, but the installer reported a problem, so some agent wiring may be stale."
		}
	case update.OutcomeUnverified:
		r.Outcome, r.Tone = "applied, unconfirmed", "ok"
		r.Detail = to + " runs, but the updater stopped before it confirmed so."
	case update.OutcomeRolledBack:
		r.Outcome, r.Tone = "rolled back", "warn"
		r.Detail = "It failed after installing, and " + from + " was restored."
	case update.OutcomeFailed:
		r.Outcome, r.Tone = "failed", "warn"
		r.Detail = failedDetail(a.Stage, from)
		if words := update.RefusalWords(a.Refused()); words != "" {
			r.Detail = "It stopped while " + stageWords(update.StageGates) + ": " + words + ", so nothing changed."
		}
	case update.OutcomeBroken:
		r.Outcome, r.Tone = "not rolled back", "danger"
		r.Detail = "It failed after installing, and " + from + " could not be restored cleanly, so automatic updates paused themselves."
		if r.Error != "" {
			r.Detail += " The error below says how to recover."
		}
	case update.OutcomeInterrupted:
		r.Outcome, r.Tone = "interrupted", "warn"
		r.Detail = "The updater stopped before it finished (killed, crashed, or asleep with the machine); " + from + " still runs."
	case update.OutcomeSuperseded:
		r.Outcome = "superseded"
		r.Detail = "The install changed some other way before it finished."
	default:
		r.Outcome = "unknown outcome"
	}
	return r
}

// refusalAction is the owner's action for the gate refusal behind a
// (update.RefusalAction), while it still concerns this install -- the daemon
// runs a's From -- else "". Keyed on the refusal's word, never on a's Error.
func refusalAction(st update.Status, a update.AttemptResult) string {
	if cur, ok := st.Current(); !ok || cur != a.From {
		return ""
	}
	return update.RefusalAction(a.Refused())
}

// failedDetail says where a failed attempt stopped: every failed attempt left
// the install as it was.
func failedDetail(stage update.Stage, from string) string {
	switch {
	case stage == update.StageSpawn:
		return "The updater never started, so nothing changed."
	case stage == update.StageVerify:
		return "The release did not pass verification, so nothing was installed."
	case stage == update.StageInstall:
		return "The installer failed and left " + from + " in place."
	case stage.PreSwap():
		return "It stopped while " + stageWords(stage) + ", before anything changed."
	default:
		return "It stopped before anything changed."
	}
}

// updatesPaused is the Paused row: automatic updates turned themselves off.
type updatesPaused struct {
	// Reason is why, in words; Versions the releases that caused it ("v0.7.3,
	// v0.7.4").
	Reason, Versions string
	At               time.Time
	// Danger marks a pause after an update that could not be rolled back.
	Danger bool
}

// updatesHold is the Held back row: a deliberate downgrade's skip-through.
type updatesHold struct {
	Through, From string // "v0.7.4"
	At            time.Time
}

// updatesBlock is one release automatic updates skip, and why.
type updatesBlock struct {
	Version, Reason string
	At              time.Time
}

// updatesBackoff is the Next try row: a failed attempt delays the next one.
type updatesBackoff struct {
	// Line names the failure ("The attempt on v0.7.3 was interrupted"); In is
	// when the next try may run ("in 1h") and Until exactly when.
	Line, In string
	Until    time.Time
}

// updatesAction is one of the automatic-update buttons. Offered renders it,
// Blocked disables it and says why (empty when it can run), and Target names
// the release Update now installs ("v0.7.3").
type updatesAction struct {
	Offered bool
	Blocked string
	Target  string
}

// autoRows fills the panel's automatic-update rows and buttons from st.
func autoRows(p *updatesPanel, st update.Status, now time.Time) {
	p.SelfUpdating = selfUpdating(st)
	if st.Applying != nil {
		p.Applying = applyingRow(st)
	}
	if a := st.LastAttempt; a != nil {
		p.LastAttempt = attemptRow(*a)
		if act := refusalAction(st, *a); act != "" {
			p.LastAttempt.Detail += " " + capitalize(act) + "."
		}
	}
	if !p.SelfUpdating {
		return
	}
	p.Auto = autoRow(st.Settings, st.Paused != nil)
	if mode, _ := st.Mode(); mode == update.ModeAuto && st.Applying == nil {
		p.Next = nextRow(st, now)
	}
	if pz := st.Paused; pz != nil {
		p.Paused = &updatesPaused{Reason: update.PauseWords(pz.Reason), Versions: versionList(pz.Versions), At: pz.At,
			Danger: pz.Reason == update.PauseBroken}
	}
	if h := st.Hold; h != nil {
		p.Hold = &updatesHold{Through: "v" + h.Through.String(), From: "v" + h.From.String(), At: h.At}
	}
	for _, b := range st.Blocked {
		p.Blocked = append(p.Blocked, updatesBlock{Version: "v" + b.Version.String(), Reason: update.BlockWords(b.Reason), At: b.At})
	}
	if b := st.Backoff; b != nil {
		p.Backoff = &updatesBackoff{Line: backoffLine(*b), In: untilPhrase(b.Until, now), Until: b.Until}
	}

	p.UpdateNow = updatesAction{Offered: true}
	if st.Newest != nil && st.Available {
		p.UpdateNow.Target = "v" + st.Newest.Version.String()
	}
	switch {
	case st.Stopped:
		p.UpdateNow.Blocked = "The update checker is not running"
	case !st.Settings.Check:
		p.UpdateNow.Blocked = "Update checks are off"
	case st.Applying != nil:
		p.UpdateNow.Blocked = "An update is in progress"
	case p.UpdateNow.Target == "":
		p.UpdateNow.Blocked = "No newer release is known"
	case !st.Newest.ChecksumsBundle:
		p.UpdateNow.Blocked = p.UpdateNow.Target + " carries no signed checksums bundle, which Update now verifies"
	}
	if st.Hold != nil || st.Paused != nil {
		p.Resume = updatesAction{Offered: true}
		if st.Stopped {
			p.Resume.Blocked = "The update checker is not running"
		}
	}
}

// updateAlert is the banner every page carries while automatic updates are in
// trouble: they paused themselves, or the last update the daemon started did
// not apply. It points to Settings > Updates, where the details are, and is
// built from fixed words and parsed versions only -- never the attempt's
// Error.
type updateAlert struct {
	// Key names what it is about, for the per-tab "Got it" (shell.js reads it
	// as data-update-banner): a newer failure or pause shows again.
	Key string
	// Head is the bold lead ("The update to v0.7.3 rolled back."), Line the
	// rest.
	Head, Line string
	// Danger marks an update that may have left the install broken.
	Danger bool
}

// updateAlertFor returns the trouble banner for st at now, or nil. It speaks,
// for a week, while the owner still lets the daemon update itself -- checks on,
// and automatic updates on for anything but the owner's own Update now -- and
// says nothing while an update is under way (it may fix the trouble) or once
// the install reached the failed release some other way. A pause outranks a
// failed attempt; an update the owner ran by hand is theirs to watch. A
// failure the updater's gates refused says why, and while the daemon still
// runs the release the attempt started from, the owner's action.
func updateAlertFor(st update.Status, now time.Time) *updateAlert {
	if st.Applying != nil || !st.Settings.Check {
		return nil
	}
	recent := func(t time.Time) bool {
		age := now.Sub(t)
		return age >= 0 && age < troubleBannerWindow
	}
	if pz := st.Paused; pz != nil && st.Settings.Auto && recent(pz.At) {
		line := capitalize(update.PauseWords(pz.Reason))
		if vs := versionList(pz.Versions); vs != "" {
			line += " (" + vs + ")"
		}
		return &updateAlert{
			Key:    "paused-" + strconv.FormatInt(pz.At.Unix(), 10),
			Head:   "Automatic updates paused themselves.",
			Line:   line + ", so nothing installs by itself until you resume them.",
			Danger: pz.Reason == update.PauseBroken,
		}
	}

	a := st.LastAttempt
	if a == nil || !recent(a.FoldedAt) {
		return nil
	}
	switch a.Why {
	case update.WhyNow:
	case update.WhyAuto:
		if !st.Settings.Auto {
			return nil
		}
	default:
		return nil
	}
	stays := a.From
	if cur, ok := st.Current(); ok {
		if cur.Compare(a.To) >= 0 {
			return nil
		}
		stays = cur
	}
	to := "v" + a.To.String()
	alert := &updateAlert{Key: "failed-" + a.To.String() + "-" + strconv.FormatInt(a.FoldedAt.Unix(), 10)}
	next := "."
	if mode, _ := st.Mode(); mode == update.ModeAuto {
		next = retryWords(st, a.To, now)
	}
	switch a.Outcome {
	case update.OutcomeRolledBack:
		alert.Head = "The update to " + to + " rolled back."
		alert.Line = "Seamless is back on v" + stays.String() + next
	case update.OutcomeFailed:
		alert.Head = "The update to " + to + " failed."
		alert.Line = "Seamless stays on v" + stays.String() + next
		// A gate refusal says why, and what the owner does about it, in
		// update's fixed words for its refusal word.
		if words := update.RefusalWords(a.Refused()); words != "" {
			alert.Line += " " + capitalize(words)
			if act := refusalAction(st, *a); act != "" {
				alert.Line += "; " + act
			}
			alert.Line += "."
		}
	case update.OutcomeInterrupted:
		alert.Head = "The update to " + to + " did not finish."
		alert.Line = "The updater stopped before it was done; Seamless stays on v" + stays.String() + next
	case update.OutcomeBroken:
		alert.Head = "The update to " + to + " could not be rolled back cleanly."
		alert.Line = "Settings > Updates has what went wrong and how to recover."
		alert.Danger = true
	default:
		return nil
	}
	return alert
}

// retryWords ends a failure banner's line with what automatic updates do next
// about release v, on an install that updates itself: skip it, or try again
// after their backoff.
func retryWords(st update.Status, v update.Version, now time.Time) string {
	for _, b := range st.Blocked {
		if b.Version == v {
			return ", and automatic updates skip v" + v.String() + "."
		}
	}
	if b := st.Backoff; b != nil && b.Version == v {
		return ", and tries again " + untilPhrase(b.Until, now) + "."
	}
	return "."
}

// whyWords says who started an attempt.
func whyWords(why string) string {
	switch why {
	case update.WhyAuto:
		return "automatic"
	case update.WhyNow:
		return "Update now"
	case update.WhyManual:
		return "seamlessd update, run by hand"
	default:
		return ""
	}
}

// stageWords says what an update does in stage: "verifying the release". A
// stage this release does not know (a newer updater's) reads as a plain step.
func stageWords(stage update.Stage) string {
	switch stage {
	case "", update.StageSpawn:
		return "starting the updater"
	case update.StageLock:
		return "taking the update lock"
	case update.StageGates:
		return "checking it may install now"
	case update.StageFetch:
		return "downloading the installer"
	case update.StageVerify:
		return "verifying the release"
	case update.StagePreflight:
		return "checking that a rollback would work"
	case update.StageBackup:
		return "backing up this install"
	case update.StageInstall:
		return "running the installer"
	case update.StageConfirm:
		return "waiting for the new version to answer"
	case update.StageRollback:
		return "rolling back"
	case update.StageDone:
		return "finishing"
	default:
		return "working"
	}
}

// backoffLine names the failure a backoff follows.
func backoffLine(b update.Backoff) string {
	what := "failed"
	if b.Reason == update.OutcomeInterrupted {
		what = "was interrupted"
	}
	line := "The attempt on v" + b.Version.String() + " " + what
	if b.Count > 1 {
		line += fmt.Sprintf(" (%d in a row)", b.Count)
	}
	return line
}

// versionList renders versions as "v0.7.3, v0.7.4".
func versionList(vs []update.Version) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = "v" + v.String()
	}
	return strings.Join(out, ", ")
}

// capitalize upper-cases the first letter of a fixed English phrase.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
