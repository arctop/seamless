package console

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/update"
)

// The console's view of automatic updates (plan 2.4): the Settings > Updates
// switch row and its next update, the update under way, the last attempt, and
// the alert card that says what holds automatic updates back, plus the
// failure banner every page carries.
//
// Every word here is fixed or built from a parsed update.Version. The one free
// text is an attempt's Error, the updater's own summary for the owner: only the
// hint of the attempt in Settings > Updates shows it -- never the banner, a
// flash, a toast, an event summary or the health strip.

// troubleBannerWindow is how long after a pause or a failed attempt every page
// says so: the same week the briefing's trouble line covers (internal/update).
// Settings > Updates keeps showing both after that.
const troubleBannerWindow = 7 * 24 * time.Hour

// selfUpdating reports whether this install can install a release by itself
// at all: a release build the installer made, with an updater wired. Only then
// do the Auto switch, Update now, Resume and the alerts about what holds
// automatic updates back mean anything; everywhere else the first card's
// "To update" row says what is missing.
func selfUpdating(st update.Status) bool {
	return st.Release && !st.Install.NotifyOnly() && st.CanApply
}

// updatesAutoRow is the "Install updates by themselves" row and its switch:
// whether the owner lets this install update itself.
type updatesAutoRow struct {
	// On is the effective permission (Settings.Auto), the switch's state.
	// Sub is the row's line when no update is pending (Next says it then),
	// and Hint what happens when one is.
	On   bool
	Sub  string
	Hint string
	// Switch offers the console's switch. It needs checks on: with them off
	// nothing is fetched to install, and the switch could not show the choice
	// that comes back with them. Locked disables turning it on, and LockedNote
	// is the hint of the locked chip beside it, which says where it was set.
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

// autoRow builds the automatic-updates row from the effective settings;
// paused is true while automatic updates have paused themselves, which the
// alert card says and an "on" row points to. Who decided is the source
// chip's to say (the head's, or the locked one beside the switch), so the
// line says only what the switch means now.
func autoRow(set update.Settings, paused bool, byHand string) updatesAutoRow {
	r := updatesAutoRow{On: set.Auto, Switch: set.Check, Locked: set.AutoLocked, Hint: autoHint(set, byHand)}
	switch {
	case set.AutoSource == update.SourceCheck:
		r.Sub = "Off while update checks are off: with no update traffic there is nothing to install."
	case !set.Auto:
		r.Sub = "Off: new releases are announced, and installing one is your call."
	case paused:
		r.Sub = "On, but they paused themselves: see above."
	default:
		r.Sub = "On: a new release installs by itself at a quiet moment."
	}
	if r.Switch && r.Locked {
		r.LockedNote = autoLockedText(set)
	}
	return r
}

// autoHint says what automatic updates do with a new release, in the
// settings' own words (update.SpanWords, as Status.Mode says them), and the
// command that updates this install by hand.
func autoHint(set update.Settings, byHand string) string {
	h := "A new release installs by itself"
	if set.MinAge > 0 {
		h += " once it is " + update.SpanWords(set.MinAge) + " old"
	}
	return h + ", when no agent session is live, or at a lull in requests after waiting " + update.SpanWords(set.MaxDefer) +
		". Seamless restarts to finish; a release that fails its health check is rolled back and skipped. To update by hand, run " + byHand + "."
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

// updatesAttempt is the last update attempt: the first card's quiet line
// when it needs nothing from the owner, else the alert card's attempt. Error
// and LogPath appear in Settings > Updates and nowhere else in the console.
type updatesAttempt struct {
	// Outcome is the result in words, Code update's word for it
	// (update.OutcomeRolledBack), and Tone its weight ("ok", "warn",
	// "danger", or "" for neutral).
	Outcome, Code, Tone string
	To, From            string // "v0.7.3"
	Why                 string
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
	// Hint is the quiet line's hint: what it meant, and the updater's own
	// message.
	Hint string
}

// attemptRow builds the Last attempt row.
func attemptRow(a update.AttemptResult) *updatesAttempt {
	to, from := "v"+a.To.String(), "v"+a.From.String()
	r := &updatesAttempt{
		To: to, From: from, Why: whyWords(a.Why), At: a.FinishedAt, Code: a.Outcome,
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
		if words := update.SignatureWords(a.SignatureRefused()); words != "" {
			r.Detail = "update.release_signers refused " + to + ": " + words + ", so nothing changed."
		}
	case update.OutcomeBroken:
		r.Outcome, r.Tone = "not rolled back", "danger"
		r.Detail = "It failed after installing, and " + from + " could not be restored cleanly, so automatic updates paused themselves."
		if r.Error != "" {
			r.Detail += " Its details say how to recover."
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

// updatesAction is one of the automatic-update buttons. Offered renders it,
// Blocked disables it and says why (empty when it can run), and Target names
// the release Update now installs ("v0.7.3").
type updatesAction struct {
	Offered bool
	Blocked string
	Target  string
}

// upAlertCard is the one alert card Settings > Updates and Settings > Backups
// open with while something needs the owner: every exceptional state, worst
// first, each with its own action. Tone is the card's, its worst item's.
type upAlertCard struct {
	Tone  string
	Items []upAlertItem
}

// upAlertItem is one exceptional state in an alert card: a head, one line,
// the details one hint away, and the action that answers it.
type upAlertItem struct {
	// Kind names the state, for tests and styling: "stopped", "paused",
	// "held", "skipped", "backoff", "attempt" (Updates), "failing",
	// "manual" (Backups). Tone is "danger" or "warn".
	Kind, Tone string
	Head, Line string
	// At is when it began, said after the line ("2h ago"); Until and
	// UntilWords end the line with when it changes ("in 40m").
	At         time.Time
	Until      time.Time
	UntilWords string
	// Detail is the service's own words for the cause, on a line of its own
	// (Backups' run error). Hint keeps the rest one hover away.
	Detail string
	Hint   string
	// The actions: Resume automatic updates (Blocked says why it waits),
	// the command that re-wires the agents, the updater's log, and Backups'
	// Try again.
	Resume        bool
	ResumeBlocked string
	Rewire        bool
	Log           pathView
	Retry         bool
	RetryBlocked  string
}

// add appends item and keeps the card's tone its worst item's.
func (c *upAlertCard) add(item upAlertItem) {
	c.Items = append(c.Items, item)
	if c.Tone != "danger" {
		c.Tone = item.Tone
	}
}

// orNil is the card, or nil without an item.
func (c *upAlertCard) orNil() *upAlertCard {
	if len(c.Items) == 0 {
		return nil
	}
	return c
}

// autoRows fills the panel's automatic-update parts from st: the switch row
// and its next update, the update under way, Update now, the last attempt,
// and the alert card.
func autoRows(p *updatesPanel, st update.Status, now time.Time) {
	p.SelfUpdating = selfUpdating(st)
	if st.Applying != nil {
		p.Applying = applyingRow(st)
	}
	card := &upAlertCard{}
	if st.Stopped {
		card.add(upAlertItem{Kind: "stopped", Tone: "danger", Head: "The update checker stopped after an internal error.",
			Line: "The daemon keeps serving; restart it to revive the check."})
	}
	defer func() { p.Alert = card.orNil() }()

	var attempt *updatesAttempt
	alerting := false
	if a := st.LastAttempt; a != nil {
		attempt = attemptRow(*a)
		if act := refusalAction(st, *a); act != "" {
			attempt.Detail += " " + capitalize(act) + "."
		}
		alerting = attemptNeedsOwner(st, *a)
		if !alerting {
			attempt.Hint = attempt.Detail
			if attempt.Error != "" {
				attempt.Hint += "\n\nThe updater said: " + attempt.Error
			}
			p.LastUpdate = attempt
		}
	}
	if !p.SelfUpdating {
		if alerting {
			card.add(attemptItem(st, attempt, nil, nil, now))
		}
		return
	}
	p.Auto = autoRow(st.Settings, st.Paused != nil, st.Install.Hint)
	if mode, _ := st.Mode(); mode == update.ModeAuto && st.Applying == nil {
		p.Next = nextRow(st, now)
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

	resumeBlocked := ""
	if st.Stopped {
		resumeBlocked = "The update checker is not running"
	}
	resumeOffered := false
	if pz := st.Paused; pz != nil {
		tone := "warn"
		if pz.Reason == update.PauseBroken {
			tone = "danger"
		}
		line := capitalize(update.PauseWords(pz.Reason))
		if vs := versionList(pz.Versions); vs != "" {
			line += " (" + vs + ")"
		}
		card.add(upAlertItem{Kind: "paused", Tone: tone, Head: "Automatic updates paused themselves.",
			Line: line + ". Update now still works.", At: pz.At,
			Hint:   "Nothing installs by itself until you resume them. Resume lifts the pause; it changes no setting.",
			Resume: true, ResumeBlocked: resumeBlocked})
		resumeOffered = true
	}
	if h := st.Hold; h != nil {
		card.add(upAlertItem{Kind: "held", Tone: "warn", Head: "Releases up to v" + h.Through.String() + " are held back.",
			Line: "This install went back from v" + h.From.String() + ".", At: h.At,
			Hint:   "Resume lets automatic updates take them again; Update now lifts the hold too.",
			Resume: !resumeOffered, ResumeBlocked: resumeBlocked})
	}

	// A skip or a backoff that is about the failed attempt's release says so
	// in the attempt's own line; any other gets an item of its own.
	var forAttempt *update.Block
	var others []update.Block
	for i, b := range st.Blocked {
		if alerting && b.Version == st.LastAttempt.To && forAttempt == nil {
			forAttempt = &st.Blocked[i]
			continue
		}
		others = append(others, b)
	}
	if len(others) > 0 {
		card.add(skippedItem(others))
	}
	var backoff *update.Backoff
	if b := st.Backoff; b != nil {
		if alerting && b.Version == st.LastAttempt.To {
			backoff = b
		} else {
			card.add(upAlertItem{Kind: "backoff", Tone: "warn", Head: backoffLine(*b) + ".",
				Line: "Automatic updates try again", Until: b.Until, UntilWords: untilPhrase(b.Until, now)})
		}
	}
	if alerting {
		card.add(attemptItem(st, attempt, forAttempt, backoff, now))
	}
}

// skippedHint is what a skipped release means for the owner's two buttons.
const skippedHint = "Automatic updates skip these releases until a newer one is out; Resume does not clear them, and Update now does not skip them."

// skippedItem is the alert for releases automatic updates skip.
func skippedItem(bs []update.Block) upAlertItem {
	item := upAlertItem{Kind: "skipped", Tone: "warn", Hint: skippedHint}
	if len(bs) == 1 {
		item.Head = "Automatic updates skip v" + bs[0].Version.String() + "."
		item.Line = capitalize(update.BlockWords(bs[0].Reason)) + "."
		item.At = bs[0].At
		return item
	}
	item.Head = fmt.Sprintf("Automatic updates skip %d releases.", len(bs))
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = "v" + b.Version.String() + ": " + update.BlockWords(b.Reason)
	}
	item.Line = capitalize(strings.Join(parts, "; ")) + "."
	return item
}

// attemptNeedsOwner reports whether the last attempt belongs in the alert
// card: it failed, rolled back or was interrupted while the install is still
// below the release it tried, it could not be rolled back cleanly, or it
// applied with warnings to the version running now. Anything else -- applied,
// superseded, or a failure the install has since moved past -- is history,
// the first card's quiet line.
func attemptNeedsOwner(st update.Status, a update.AttemptResult) bool {
	cur, ok := st.Current()
	switch a.Outcome {
	case update.OutcomeBroken:
		return true
	case update.OutcomeApplied:
		return a.Warnings && (!ok || cur == a.To)
	case update.OutcomeFailed, update.OutcomeRolledBack, update.OutcomeInterrupted:
		return !ok || cur.Compare(a.To) < 0
	}
	return false
}

// attemptItem is the alert for an attempt that needs the owner: its head,
// what it meant (with what automatic updates do next about its release:
// skip it, or try again), and its details -- who started it and the
// updater's own message -- in the hint, beside its log and, for one that
// applied with warnings, the command that re-wires the agents.
func attemptItem(st update.Status, a *updatesAttempt, skip *update.Block, backoff *update.Backoff, now time.Time) upAlertItem {
	item := upAlertItem{Kind: "attempt", Tone: "warn", Line: a.Detail, At: a.At, Rewire: a.Rewire, Log: viewPath(a.LogPath)}
	switch {
	case a.Tone == "danger":
		item.Tone = "danger"
		item.Head = "The update to " + a.To + " could not be rolled back cleanly."
	case a.Rewire:
		item.Head = a.To + " applied with warnings."
		item.Line = a.Detail + " Run seamlessd install-hooks to re-wire your agents."
	case a.Code == update.OutcomeRolledBack:
		item.Head = "The update to " + a.To + " rolled back."
	case a.Code == update.OutcomeInterrupted:
		item.Head = "The update to " + a.To + " did not finish."
	default:
		item.Head = "The update to " + a.To + " failed."
	}
	switch {
	case skip != nil:
		item.Line += " Automatic updates skip " + a.To + ": " + update.BlockWords(skip.Reason) + "."
		item.Hint = skippedHint + " "
	case backoff != nil:
		item.Line += " " + backoffLine(*backoff) + "; automatic updates try again"
		item.Until, item.UntilWords = backoff.Until, untilPhrase(backoff.Until, now)
	}
	item.Hint += a.From + " to " + a.To
	if a.Why != "" {
		item.Hint += ", " + a.Why
	}
	item.Hint += ": " + a.Outcome + "."
	if a.Error != "" {
		item.Hint += "\n\nThe updater said: " + a.Error
	}
	return item
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
		if words := update.SignatureWords(a.SignatureRefused()); words != "" {
			alert.Line += " The release is not the owner's: " + words + "."
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
