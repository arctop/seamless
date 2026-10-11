package main

// The CLI's read-only view of the background update check and of automatic
// updates: doctor's "updates" row and its two update checks, and the rows
// `seamlessd update --check` adds. None of it asks the daemon or writes
// anything. The daemon records its own view of itself in
// <data_dir>/update/state.json, the updater records each attempt in
// attempt.json (internal/update), and the effective settings are the config
// file plus the console's stored override, merged the way the daemon merges
// them.
//
// What the daemon decides from live inputs stays the daemon's: Decide reads
// the live agent sessions and the request activity, which only the daemon
// has. So these surfaces show what it recorded -- the pending deadline, an
// update under way, the last attempt, a hold, a pause, blocked releases, a
// backoff -- and what update.Target makes of the recorded release list, never
// a fresh decision.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// staleCheckWarnAfter is how long the release check may keep failing before
// doctor warns instead of informing: a day of errors is a laptop offline, a
// week is a proxy or a firewall the owner should hear about.
const staleCheckWarnAfter = 7 * 24 * time.Hour

// timeLayout is how these surfaces print a point in time, in local time.
const timeLayout = "2006-01-02 15:04"

// updateView is what the CLI can know about the update check on this machine.
type updateView struct {
	// release is whether the daemon's build is a published release: from its
	// own record when there is one, else this binary's stamp; distribution
	// is the stamp it was judged from.
	release      bool
	distribution string
	settings     update.Settings
	// state is the daemon's record; stateErr why it could not be read.
	state    update.State
	stateErr error
	// attempt is the updater's newest record (attempt.json), attemptErr why
	// it could not be read; zero when no attempt ran here. It carries what
	// the daemon's folded copy (State.LastAttempt) does not: the backup path,
	// and an attempt the daemon has not folded yet.
	attempt    update.Attempt
	attemptErr error
	// dataDir is where the records were read, "" on a client machine.
	dataDir string
	// install is the install kind the daemon detected (from its record), or
	// the build-only verdict when it never recorded one.
	install update.Install
	// recorded reports whether a daemon has written its record.
	recorded bool
}

// loadUpdateView assembles the view. db may be nil (no database to read the
// console's override from); the override is then ignored, as the daemon does
// when its row is unreadable.
func loadUpdateView(ctx context.Context, cfg config.Config, db *sql.DB) updateView {
	v := updateView{release: update.IsReleaseBuild(distribution, version), distribution: distribution}
	if !cfg.IsClient() {
		v.dataDir = cfg.DataDir
		v.state, v.stateErr = update.ReadState(cfg.DataDir)
		v.attempt, v.attemptErr = update.ReadAttempt(cfg.DataDir)
	}
	if r := v.state.Running; r.Instance != "" {
		v.recorded = true
		v.release = update.IsReleaseBuild(r.Distribution, r.Version.String())
		v.distribution = r.Distribution
		v.install = update.Install{Kind: r.Kind, Reason: r.Reason, Hint: update.Hint(r.Kind)}
	} else {
		// No record: only what the build says can be known without the
		// daemon's own process facts (Detect's remaining gates need them).
		probe := update.Probe{Distribution: distribution, Version: version, Client: cfg.IsClient()}
		v.install = update.Detect(probe)
		if v.install.Kind == update.KindUnknown && v.release {
			v.install.Reason = "the daemon has not recorded how it was installed yet"
		}
	}
	var override config.UpdateOverride
	if db != nil {
		if o, _, err := store.UpdateOverride(ctx, db); err == nil {
			override = o
		}
	}
	v.settings = update.Effective(cfg.Update, override, v.release)
	return v
}

// status is the view as an update.Status, for its Mode wording. It carries
// the daemon's own facts -- whether an updater is wired, the pause and the
// hold -- so a daemon that updates itself reads as ModeAuto here too.
func (v updateView) status() update.Status {
	return update.Status{
		Distribution: v.distribution, Release: v.release, Install: v.install, Settings: v.settings,
		CanApply: v.state.Running.CanApply, Paused: v.state.Paused, Hold: v.state.Hold,
	}
}

// newest returns the newest installable release the daemon last saw.
func (v updateView) newest() (update.Release, bool) {
	return update.Newest(v.state.Releases)
}

// current is the version the daemon runs: its record's, else this binary's
// when no daemon recorded one. ok is false for a build that is not a clean
// release.
func (v updateView) current() (update.Version, bool) {
	if v.recorded {
		return v.state.Running.Version, true
	}
	return update.Parse(version)
}

// pauseHolds reports whether the pause is what keeps the daemon from updating
// itself: without it, Mode would be ModeAuto. A pause the owner has since
// answered by turning automatic updates (or checks) off is their call, and
// no news -- the briefing's notice keeps the same silence.
func (v updateView) pauseHolds() bool {
	if v.state.Paused == nil {
		return false
	}
	s := v.status()
	s.Paused = nil
	mode, _ := s.Mode()
	return mode == update.ModeAuto
}

// backoff is the recorded backoff while it still delays the next automatic
// attempt, else nil.
func (v updateView) backoff(now time.Time) *update.Backoff {
	if b := v.state.Backoff; b != nil && now.Before(b.Until) {
		return b
	}
	return nil
}

// autoTarget is what update.Target makes of the recorded release list: the
// release an automatic update installs now, and the newest one it takes once
// that one's soak is over.
//
// The daemon measures the soak and the pending deadline on GitHub's clock
// (the last Date header plus the time since), and so does this view, from the
// same record (State.GitHubClock): a machine clock that is days off cannot
// make a soak read as over here while the daemon still waits it out.
type autoTarget struct {
	now   update.Release
	nowOK bool
	next  update.Release
	// nextOK reports a release an automatic update takes, soak aside, and
	// soakEnd when its soak is over.
	nextOK  bool
	soakEnd time.Time
	// clock is GitHub's clock at now as the daemon estimates it, or now when
	// no check has carried a Date: what soakEnd and the pending deadline are
	// compared with.
	clock time.Time
}

// target applies update.Target to the recorded release list, blocks and hold.
func (v updateView) target(cur update.Version, now time.Time) autoTarget {
	st, minAge := v.state, v.settings.MinAge
	var t autoTarget
	t.clock = now
	if c, ok := st.GitHubClock(now, v.settings.CheckInterval); ok {
		t.clock = c
	}
	t.now, t.nowOK = update.Target(st.Releases, cur, t.clock, minAge, st.Blocks, st.Hold)
	// No soak: a zero minimum age soaks every release whatever the clock.
	t.next, t.nextOK = update.Target(st.Releases, cur, time.Time{}, 0, st.Blocks, st.Hold)
	if t.nextOK {
		t.soakEnd = t.next.PublishedAt.Add(minAge)
	}
	return t
}

// skipped is why automatic updates skip a release above the running version,
// soak aside: update.HeldBack's verdict.
type skipped struct {
	// code is update.WaitHeld (a hold, the owner's own pin after a deliberate
	// downgrade), WaitBlocked (a block a failed attempt left) or WaitUnsigned
	// (no checksums bundle to verify); reason says it after "skip it: ".
	code   string
	reason string
}

// news reports a skip the owner hears about, since the release is then
// installed only by hand. A hold is their own pin, and no news.
func (s skipped) news() bool { return s.code != update.WaitHeld }

// phrase is the skip after "vX is".
func (s skipped) phrase() string {
	if s.code == update.WaitHeld {
		return "held back: " + s.reason
	}
	return "skipped: " + s.reason
}

// skipOf says why automatic updates skip r, a release above the running
// version, soak aside; ok is false when nothing does. A hold's words say where
// it is resumed.
func (v updateView) skipOf(r update.Release) (skipped, bool) {
	code, why := update.HeldBack(r, v.state.Blocks, v.state.Hold)
	switch code {
	case "":
		return skipped{}, false
	case update.WaitHeld:
		why += " in the console"
	}
	return skipped{code: code, reason: why}, true
}

// underWay is an update the records show under way: one the daemon spawned
// and has not folded, or one the updater reports running (a manual
// `seamlessd update` too). The daemon also probes the updater's lock, which a
// read-only CLI does not take, so a record that has gone quiet is reported as
// quiet rather than judged.
type underWay struct {
	from, to update.Version
	why      string
	// stage is the updater's latest stage, "" until it writes a record.
	stage update.Stage
	// since is when it started (the record) or was spawned; beat is the
	// record's latest write, zero with no record.
	since, beat time.Time
	// quiet reports a record whose heartbeat went stale, or a spawn no
	// record has named within update.AttemptStartWindow.
	quiet bool
}

// underWay finds the update under way, if the records show one.
func (v updateView) underWay(now time.Time) (underWay, bool) {
	rec, sp := v.attempt, v.state.Spawn
	switch {
	case rec.ID != "" && rec.Live(now):
		return underWay{from: rec.From, to: rec.To, why: rec.Why, stage: rec.Stage, since: rec.StartedAt, beat: rec.HeartbeatAt}, true
	case sp == nil:
		return underWay{}, false
	case rec.ID == sp.ID && rec.Finished():
		// Finished: lastAttempt shows it until the daemon folds it.
		return underWay{}, false
	case rec.ID == sp.ID:
		return underWay{from: sp.From, to: sp.To, why: sp.Why, stage: rec.Stage, since: sp.At, beat: rec.HeartbeatAt, quiet: true}, true
	default:
		return underWay{from: sp.From, to: sp.To, why: sp.Why, since: sp.At, quiet: now.Sub(sp.At) >= update.AttemptStartWindow}, true
	}
}

// progress says how far the update under way has got.
func (u underWay) progress(now time.Time) string {
	switch {
	case u.quiet && u.beat.IsZero():
		return fmt.Sprintf("spawned %s ago, and the updater never recorded a start; the daemon settles it", humanDuration(now.Sub(u.since)))
	case u.quiet:
		return fmt.Sprintf("no word from the updater for %s, last at stage %s; the daemon settles it once the updater is gone",
			humanDuration(now.Sub(u.beat)), u.stage)
	case u.stage == "":
		return fmt.Sprintf("the updater is starting, spawned %s ago", humanDuration(now.Sub(u.since)))
	default:
		return fmt.Sprintf("stage %s, started %s ago", u.stage, humanDuration(now.Sub(u.since)))
	}
}

// shownAttempt is the newest update attempt as the CLI shows it: the one the
// daemon folded (State.LastAttempt), or a finished record in attempt.json the
// daemon has not folded yet. A running daemon folds a record within a minute
// of its last write, so an unsettled one usually means the daemon is down.
type shownAttempt struct {
	update.AttemptResult
	// settled reports whether the daemon folded it.
	settled bool
	// backup is the record's BackupPath, when the record carries one.
	backup string
}

// when is when the attempt ended, as far as the records say.
func (a shownAttempt) when() time.Time {
	switch {
	case !a.FinishedAt.IsZero():
		return a.FinishedAt
	case !a.FoldedAt.IsZero():
		return a.FoldedAt
	default:
		return a.StartedAt
	}
}

// lastAttempt resolves the newest attempt, if any ran here.
func (v updateView) lastAttempt() (shownAttempt, bool) {
	rec, last := v.attempt, v.state.LastAttempt
	if rec.ID != "" && rec.Finished() && (last == nil || saysMore(rec, last)) {
		return shownAttempt{AttemptResult: recordResult(rec), backup: rec.BackupPath}, true
	}
	if last == nil {
		return shownAttempt{}, false
	}
	a := shownAttempt{AttemptResult: *last, settled: true}
	if rec.ID == last.ID {
		a.backup = rec.BackupPath
	} else if h, ok := v.fromHistory(last.ID); ok {
		a.backup = h.BackupPath
	}
	return a, true
}

// saysMore reports whether a finished record says more than the folded last
// attempt, by the daemon's own fold rules: it is a later attempt, or the real
// outcome of the one folded as interrupted.
func saysMore(rec update.Attempt, last *update.AttemptResult) bool {
	if rec.ID == last.ID {
		return last.Outcome == update.OutcomeInterrupted
	}
	seen := last.StartedAt
	if seen.IsZero() {
		seen = last.SpawnedAt
	}
	return rec.StartedAt.After(seen)
}

// recordResult is a finished record as the daemon folds it. Classify reads a
// finished record's own fields only, so the running version it is given
// does not matter here.
func recordResult(a update.Attempt) update.AttemptResult {
	outcome := update.Classify(a, a.From)
	return update.AttemptResult{
		ID: a.ID, From: a.From, To: a.To, Why: a.Why, Outcome: outcome, Stage: a.Stage,
		RolledBack: a.RolledBack, Warnings: outcome == update.OutcomeApplied && a.Error != "",
		Error: a.Error, Refusal: a.Refusal, LogPath: a.LogPath, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
	}
}

// refusalAction is the owner's action for the gate refusal behind a
// (update.RefusalAction), while it still concerns this install -- the daemon
// runs a's From, cur -- else "". It is keyed on the refusal's word, never on
// the record's error text.
func refusalAction(a update.AttemptResult, cur update.Version) string {
	if a.From != cur {
		return ""
	}
	return update.RefusalAction(a.Refused())
}

// fromHistory finds attempt id in attempts.jsonl, newest line first. An
// unreadable history only costs the backup row it would have filled.
func (v updateView) fromHistory(id string) (update.Attempt, bool) {
	if v.dataDir == "" {
		return update.Attempt{}, false
	}
	hist, _, err := update.ReadAttemptHistory(v.dataDir)
	if err != nil {
		return update.Attempt{}, false
	}
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].ID == id {
			return hist[i], true
		}
	}
	return update.Attempt{}, false
}

// updatesRow is the updates row being built: its status, what it says, and
// the owner's action, which always ends the line.
type updatesRow struct {
	status checkStatus
	text   string
	action string
}

func (r updatesRow) check() check {
	detail := r.text
	if r.action != "" {
		detail += " -- " + r.action
	}
	return check{r.status, "updates", detail}
}

// updatesCheck is doctor's "updates" row. It stays one line, the most urgent
// thing first: an update that could not be rolled back cleanly and that the
// daemon has not settled (it is likely down), an update under way, automatic
// updates that paused themselves; then the release check and what it found,
// worded for the mode, with the last attempt while it still concerns this
// install. The rest is `seamlessd update --check`'s.
func updatesCheck(ctx context.Context, cfg config.Config, db *sql.DB, now time.Time) check {
	const name = "updates"
	v := loadUpdateView(ctx, cfg, db)
	if v.stateErr != nil {
		return check{statusWarn, name, fmt.Sprintf("cannot read %s: %v", update.StatePath(cfg.DataDir), v.stateErr)}
	}
	if a, ok := v.lastAttempt(); ok && !a.settled && a.Outcome == update.OutcomeBroken {
		return check{statusWarn, name, brokenText(a, now) + " (the daemon has not settled this attempt; is it running? seamlessd status)"}
	}
	mode, reason := v.status().Mode()
	if mode == update.ModeOff {
		return check{statusInfo, name, reason + "; update with: " + v.install.Hint}
	}
	if u, ok := v.underWay(now); ok {
		if u.quiet {
			return check{statusInfo, name, fmt.Sprintf("the update to v%s (%s) has gone quiet: %s", u.to, whyText(u.why), u.progress(now))}
		}
		return check{statusInfo, name, fmt.Sprintf("installing v%s now (%s; %s)", u.to, whyText(u.why), u.progress(now))}
	}
	cur, curOK := v.current()
	if v.pauseHolds() {
		return v.pausedRow(cur, curOK, now).check()
	}

	st := v.state
	if ce := st.CheckError; ce != nil {
		detail := fmt.Sprintf("the release check has failed %d time(s) since %s (%s): %s",
			ce.Count, ce.Since.Local().Format(timeLayout), ce.Kind, ce.Message)
		if now.Sub(ce.Since) >= staleCheckWarnAfter {
			return check{statusWarn, name, detail + " -- a proxy for the daemon goes in its service environment (HTTPS_PROXY)"}
		}
		return check{statusInfo, name, detail + "; it retries " + whenText(st.NextCheckAt, now)}
	}
	if st.CheckedAt.IsZero() {
		return check{statusInfo, name, "no release check recorded yet (the daemon checks a few minutes after it starts)"}
	}

	checked := fmt.Sprintf("checked %s ago", humanDuration(now.Sub(st.CheckedAt)))
	newest, ok := v.newest()
	var r updatesRow
	switch {
	case !ok:
		return check{statusInfo, name, "no installable release in the newest 20 (" + checked + ")"}
	case !curOK:
		return check{statusInfo, name, fmt.Sprintf("development build; the newest release is v%s (%s)", newest.Version, checked)}
	case mode == update.ModeAuto:
		r = v.autoRow(cur, newest, checked, now)
	case newest.Version.Compare(cur) > 0:
		r = updatesRow{statusWarn, fmt.Sprintf("v%s is available (running v%s; %s)", newest.Version, cur, checked), "update with: " + v.install.Hint}
	default:
		r = updatesRow{statusOK, fmt.Sprintf("up to date at v%s (%s; %s install)", cur, checked, v.install.Kind), ""}
	}
	if clause, action := v.lastClause(cur, now); clause != "" {
		r.text += clause
		if r.status == statusOK {
			r.status = statusInfo // a clause is something to read, or to do
		}
		if action != "" {
			// Until the owner acts, every retry is refused the same way.
			if r.action != "" {
				action += "; " + r.action
			}
			r.status, r.action = max(r.status, statusWarn), action
		}
	}
	return r.check()
}

// autoRow is the updates row of a daemon that updates itself, once its
// release check has run: what it installs next and when, or why it installs
// nothing, from the recorded release list and the recorded deadline.
func (v updateView) autoRow(cur update.Version, newest update.Release, checked string, now time.Time) updatesRow {
	if newest.Version.Compare(cur) <= 0 {
		return updatesRow{statusOK, fmt.Sprintf("automatic; up to date at v%s (%s; %s install)", cur, checked, v.install.Kind), ""}
	}
	t := v.target(cur, now)
	var skip skipped
	skips := false
	if !t.nextOK || t.next.Version != newest.Version {
		skip, skips = v.skipOf(newest)
	}
	text := fmt.Sprintf("automatic, running v%s (%s); ", cur, checked)
	if !t.nextOK {
		if skips && skip.news() {
			return updatesRow{statusWarn, fmt.Sprintf("v%s is available (running v%s; %s), but automatic updates skip it: %s",
				newest.Version, cur, checked, skip.reason), "update with: " + v.install.Hint}
		}
		return updatesRow{statusInfo, text + fmt.Sprintf("v%s is %s", newest.Version, skip.phrase()), ""}
	}

	switch b := v.backoff(now); {
	case b != nil:
		text += fmt.Sprintf("the next attempt waits out a backoff until %s (%s), after %s in a row on v%s",
			b.Until.Local().Format(timeLayout), whenText(b.Until, now), update.Plural(int64(b.Count), "failure"), b.Version)
	case t.nowOK:
		text += fmt.Sprintf("installs v%s once no agent session is live, or at a lull in requests %s",
			t.now.Version, v.deadlineText(cur, t.clock))
		if t.next.Version != t.now.Version {
			text += fmt.Sprintf("; then v%s, once it is %s old (at %s)", t.next.Version, update.SpanWords(v.settings.MinAge), t.soakEnd.Local().Format(timeLayout))
		}
	default:
		text += fmt.Sprintf("installs v%s once it is %s old (at %s, %s)",
			t.next.Version, update.SpanWords(v.settings.MinAge), t.soakEnd.Local().Format(timeLayout), whenText(t.soakEnd, t.clock))
	}
	if !skips {
		return updatesRow{statusInfo, text, ""}
	}
	text += fmt.Sprintf("; v%s is %s", newest.Version, skip.phrase())
	if !skip.news() {
		return updatesRow{statusInfo, text, ""}
	}
	return updatesRow{statusWarn, text, "install it by hand with: " + v.install.Hint}
}

// deadlineText says when the lull path opens for the running version: the
// recorded pending deadline (pending since, plus max_defer), or the wait the
// daemon starts at its next look when it has not recorded one yet. clock is
// autoTarget.clock, the clock the deadline was recorded on.
func (v updateView) deadlineText(cur update.Version, clock time.Time) string {
	p := v.state.Pending
	if p == nil || p.Running != cur {
		return "after waiting " + update.SpanWords(v.settings.MaxDefer)
	}
	deadline := p.Since.Add(v.settings.MaxDefer)
	if deadline.After(clock) {
		return fmt.Sprintf("from %s (%s)", deadline.Local().Format(timeLayout), whenText(deadline, clock))
	}
	return fmt.Sprintf("now that its deadline (%s) has passed", deadline.Local().Format(timeLayout))
}

// pausedRow is the updates row while automatic updates have paused
// themselves: since when and why -- with the updater's own words when the
// attempt that paused them is the last one, its recovery steps for an update
// that could not be rolled back -- a newer release that is not blocked, and
// the way out.
func (v updateView) pausedRow(cur update.Version, curOK bool, now time.Time) updatesRow {
	p := v.state.Paused
	ago := humanDuration(now.Sub(p.At))
	text := fmt.Sprintf("automatic updates paused themselves %s ago: %s", ago, pauseText(p))
	action := "resume them in the console under Settings > Updates"
	if a, ok := v.lastAttempt(); ok && causedPause(a, p) {
		if a.Outcome == update.OutcomeBroken {
			text = fmt.Sprintf("automatic updates paused themselves %s ago, because %s", ago, brokenText(a, now))
			action += " once the install is sound"
		} else {
			text += "; the last attempt " + attemptSummary(a, now)
		}
	}
	if newest, ok := v.newest(); ok && curOK && newest.Version.Compare(cur) > 0 {
		if s, skips := v.skipOf(newest); !skips || s.code != update.WaitBlocked {
			text += fmt.Sprintf("; v%s is available (running v%s)", newest.Version, cur)
			action += ", or update by hand with: " + v.install.Hint
		}
	}
	return updatesRow{statusWarn, text, action}
}

// causedPause reports whether a, an attempt a daemon started, is one whose
// outcome paused automatic updates.
func causedPause(a shownAttempt, p *update.Pause) bool {
	if a.Why != update.WhyAuto && a.Why != update.WhyNow {
		return false
	}
	if a.Outcome != update.OutcomeBroken && a.Outcome != update.OutcomeRolledBack {
		return false
	}
	for _, ver := range p.Versions {
		if ver == a.To {
			return true
		}
	}
	return false
}

// lastClause is the updates row's word on the last attempt when it still
// concerns this install and the row has not said it already: a failure on
// the way from the running version, with the updater's own error -- the
// owner's to read, so the CLI shows it -- or an update to it that applied
// with warnings, with the command that finishes it. action is the owner's
// action for a gate refusal (refusalAction), "" for anything else.
func (v updateView) lastClause(cur update.Version, now time.Time) (clause, action string) {
	a, ok := v.lastAttempt()
	if !ok {
		return "", ""
	}
	switch a.Outcome {
	case update.OutcomeApplied:
		if a.Warnings && a.To == cur {
			return fmt.Sprintf("; the update to v%s applied with warnings: finish wiring the clients with seamlessd install-hooks", a.To), ""
		}
	case update.OutcomeRolledBack, update.OutcomeFailed, update.OutcomeInterrupted:
		if a.From == cur {
			return "; the last attempt " + attemptSummary(a, now), refusalAction(a.AttemptResult, cur)
		}
	case update.OutcomeBroken:
		if a.From == cur || a.To == cur {
			return "; " + brokenText(a, now), ""
		}
	}
	return "", ""
}

// attemptSummary is one attempt for a doctor row, after "the last attempt":
// its versions, who started it, when, how it ended, and its error.
func attemptSummary(a shownAttempt, now time.Time) string {
	out := fmt.Sprintf("(v%s -> v%s, %s, %s ago) %s", a.From, a.To, whyText(a.Why), humanDuration(now.Sub(a.when())), outcomeText(a.AttemptResult))
	if e := attemptError(a.AttemptResult); e != "" {
		out += ": " + e
	}
	if !a.settled {
		out += " (not yet settled by the daemon)"
	}
	return out
}

// brokenText is an attempt that could not be rolled back cleanly. Its error
// is the updater's recovery steps, the backup to import from among them, so
// it is shown whole, with the attempt's log.
func brokenText(a shownAttempt, now time.Time) string {
	out := fmt.Sprintf("the update from v%s to v%s (%s, %s ago) could not be rolled back cleanly",
		a.From, a.To, whyText(a.Why), humanDuration(now.Sub(a.when())))
	if a.Error != "" {
		out += ": " + a.Error
	}
	if a.LogPath != "" {
		out += "; log: " + tildePath(a.LogPath)
	}
	return out
}

// attemptError is an attempt's error for display. A failure's error begins
// with the stage the updater stopped at, which outcomeText already names, so
// that prefix goes.
func attemptError(a update.AttemptResult) string {
	if a.Outcome == update.OutcomeFailed {
		return strings.TrimPrefix(a.Error, string(a.Stage)+": ")
	}
	return a.Error
}

// outcomeText says how an attempt ended, after its versions.
func outcomeText(a update.AttemptResult) string {
	switch a.Outcome {
	case update.OutcomeApplied:
		if a.Warnings {
			return "applied with warnings"
		}
		return "applied"
	case update.OutcomeUnverified:
		return "installed, unconfirmed: the updater stopped before it confirmed the new release"
	case update.OutcomeRolledBack:
		return "rolled back"
	case update.OutcomeFailed:
		return fmt.Sprintf("failed at %s, leaving the install as it was", a.Stage)
	case update.OutcomeBroken:
		return "could not be rolled back cleanly"
	case update.OutcomeInterrupted:
		return "was interrupted before it finished"
	case update.OutcomeSuperseded:
		return "was overtaken by another change to the install"
	default:
		// A word from a newer release than this one.
		return fmt.Sprintf("ended %q", a.Outcome)
	}
}

// backoffText says how the failure that set a backoff ended (Backoff.Reason,
// an Outcome word).
func backoffText(reason string) string {
	switch reason {
	case update.OutcomeFailed:
		return "failed, leaving the install as it was"
	case update.OutcomeInterrupted:
		return "was interrupted"
	case update.OutcomeApplied, update.OutcomeUnverified:
		return "reported applied while the daemon still ran the release it replaced"
	default:
		return fmt.Sprintf("ended %q", reason)
	}
}

// whyText names who started an attempt.
func whyText(why string) string {
	switch why {
	case update.WhyAuto:
		return "automatic"
	case update.WhyNow:
		return "Update now"
	case update.WhyManual:
		return "seamlessd update"
	default:
		// A word from a newer release than this one.
		return why
	}
}

// pauseText says why automatic updates paused themselves, naming the
// releases that did it: update's words with the versions after them, "two
// updates in a row rolled back (v0.7.3, v0.7.4)", as the console's banner has
// it.
func pauseText(p *update.Pause) string {
	if len(p.Versions) == 0 {
		return update.PauseWords(p.Reason)
	}
	names := make([]string, len(p.Versions))
	for i, ver := range p.Versions {
		names[i] = "v" + ver.String()
	}
	return update.PauseWords(p.Reason) + " (" + strings.Join(names, ", ") + ")"
}

// updateCheckRows prints what the daemon's update check is doing, under
// `update --check`'s verdict: the mode and the last check, then what
// automatic updates have recorded -- the target and its soak, the pending
// deadline, an update under way, the last attempt with its error, log and
// backup, a hold, a pause, blocked releases, a backoff -- and the rollback
// drill's switch. A client machine has no daemon record, so it gets the mode
// and "never" only.
func updateCheckRows(w io.Writer, v updateView, now time.Time) {
	mode, reason := v.status().Mode()
	fieldRowTo(w, "mode", string(mode)+dim(" -- "+reason))
	st := v.state
	switch {
	case v.stateErr != nil:
		fieldRowTo(w, "checked", yellow("unknown")+dim(" -- "+v.stateErr.Error()))
		return
	case st.CheckError != nil:
		fieldRowTo(w, "checked", yellow(fmt.Sprintf("failing since %s", st.CheckError.Since.Local().Format(timeLayout)))+
			dim(fmt.Sprintf(" (%d tries; %s)", st.CheckError.Count, st.CheckError.Message)))
	case st.CheckedAt.IsZero():
		fieldRowTo(w, "checked", dim("never by the daemon"))
	default:
		next := ""
		if v.settings.Check && !st.NextCheckAt.IsZero() {
			next = "; next " + whenText(st.NextCheckAt, now)
		}
		fieldRowTo(w, "checked", fmt.Sprintf("%s ago by the daemon%s", humanDuration(now.Sub(st.CheckedAt)), next))
	}

	cur, curOK := v.current()
	if mode == update.ModeAuto && curOK {
		v.targetRows(w, cur, now)
	}
	if u, ok := v.underWay(now); ok {
		text := fmt.Sprintf("v%s -> v%s (%s): %s", u.from, u.to, whyText(u.why), u.progress(now))
		if u.quiet {
			text = yellow(text)
		}
		fieldRowTo(w, "update", text)
	}
	v.lastRows(w, now)
	if h := st.Hold; h != nil {
		fieldRowTo(w, "hold", fmt.Sprintf("releases up to v%s are skipped", h.Through)+
			dim(fmt.Sprintf(" -- this install went back from v%s on %s; resuming automatic updates in the console (Settings > Updates) lifts it",
				h.From, h.At.Local().Format(timeLayout))))
	}
	if p := st.Paused; p != nil {
		fieldRowTo(w, "paused", yellow("since "+p.At.Local().Format(timeLayout))+
			dim(" -- "+pauseText(p)+"; resume automatic updates in the console (Settings > Updates)"))
	}
	if curOK {
		var blocked []string
		for _, b := range st.Blocks {
			if b.Version.Compare(cur) > 0 {
				blocked = append(blocked, fmt.Sprintf("v%s (%s, %s)", b.Version, update.BlockWords(b.Reason), b.At.Local().Format(timeLayout)))
			}
		}
		if len(blocked) > 0 {
			fieldRowTo(w, "blocked", yellow(strings.Join(blocked, "; "))+dim(" -- automatic updates skip these; seamlessd update and Update now do not"))
		}
	}
	if b := v.backoff(now); b != nil {
		fieldRowTo(w, "backoff", fmt.Sprintf("next attempt %s (%s)", whenText(b.Until, now), b.Until.Local().Format(timeLayout))+
			dim(fmt.Sprintf(" -- %s in a row on v%s; the last %s", update.Plural(int64(b.Count), "failure"), b.Version, backoffText(b.Reason))))
	}
	if v.dataDir != "" {
		drill := filepath.Join(update.StateDir(v.dataDir), confirmDrillName)
		if _, err := os.Lstat(drill); err == nil {
			fieldRowTo(w, "drill", yellow("on")+dim(" -- "+tildePath(drill)+" exists, so every update fails its confirmation and rolls back; delete it to end the drill"))
		}
	}
}

// targetRows prints what an automatic update installs next, and the pending
// deadline.
func (v updateView) targetRows(w io.Writer, cur update.Version, now time.Time) {
	newest, ok := v.newest()
	if !ok || newest.Version.Compare(cur) <= 0 {
		fieldRowTo(w, "target", dim(fmt.Sprintf("none -- nothing newer than v%s", cur)))
		return
	}
	t := v.target(cur, now)
	soak := func(end time.Time) string {
		switch {
		case v.settings.MinAge <= 0:
			return "no soak"
		case end.After(t.clock):
			return fmt.Sprintf("soaks until %s, %s", end.Local().Format(timeLayout), whenText(end, t.clock))
		default:
			return "soaked since " + end.Local().Format(timeLayout)
		}
	}
	var text string
	switch {
	case !t.nextOK:
		text = yellow("none")
	case t.nowOK && t.now.Version != t.next.Version:
		text = "v" + t.now.Version.String() + dim(" ("+soak(t.now.PublishedAt.Add(v.settings.MinAge))+")") +
			"; then v" + t.next.Version.String() + dim(" ("+soak(t.soakEnd)+")")
	default:
		text = "v" + t.next.Version.String() + dim(" ("+soak(t.soakEnd)+")")
	}
	if !t.nextOK || t.next.Version != newest.Version {
		if s, ok := v.skipOf(newest); ok {
			// A hold and a block have rows of their own below.
			note := s.phrase()
			switch s.code {
			case update.WaitHeld:
				note = "held back (hold, below)"
			case update.WaitBlocked:
				note = "blocked (below)"
			}
			text += dim(fmt.Sprintf(" -- v%s is %s", newest.Version, note))
		}
	}
	fieldRowTo(w, "target", text)

	if p := v.state.Pending; p != nil && p.Running == cur {
		deadline := p.Since.Add(v.settings.MaxDefer)
		due := "passed"
		if deadline.After(t.clock) {
			due = whenText(deadline, t.clock)
		}
		fieldRowTo(w, "pending", fmt.Sprintf("since %s; deadline %s (%s)", p.Since.Local().Format(timeLayout), deadline.Local().Format(timeLayout), due)+
			dim(" -- installs once no agent session is live, or at a lull in requests after the deadline"))
	}
}

// lastRows prints the last attempt: how it ended, the updater's error, the
// command an update that applied with warnings -- or a gate refusal that
// still concerns this install -- still needs, its log and the backup it took.
func (v updateView) lastRows(w io.Writer, now time.Time) {
	if v.attemptErr != nil {
		fieldRowTo(w, "record", yellow("unreadable")+dim(" -- "+v.attemptErr.Error()))
	}
	a, ok := v.lastAttempt()
	if !ok {
		return
	}
	text := fmt.Sprintf("v%s -> v%s %s", a.From, a.To, outcomeText(a.AttemptResult))
	if clean := a.Outcome == update.OutcomeApplied && !a.Warnings; !clean && a.Outcome != update.OutcomeSuperseded {
		text = yellow(text)
	}
	extra := fmt.Sprintf(" (%s, %s ago)", whyText(a.Why), humanDuration(now.Sub(a.when())))
	if !a.settled {
		extra += "; not yet settled by the daemon"
	}
	fieldRowTo(w, "last", text+dim(extra))
	if e := attemptError(a.AttemptResult); e != "" {
		fieldRowTo(w, "error", e)
	}
	if a.Warnings {
		fieldRowTo(w, "fix", "seamlessd install-hooks"+dim(" -- the update applied, but the installer did not finish wiring the clients"))
	}
	if cur, ok := v.current(); ok {
		if act := refusalAction(a.AttemptResult, cur); act != "" {
			fieldRowTo(w, "fix", act)
		}
	}
	if a.LogPath != "" {
		fieldRowTo(w, "log", tildePath(a.LogPath)+gone(a.LogPath, fmt.Sprintf("removed; the newest %d logs are kept", updateLogKeep)))
	}
	if a.backup != "" {
		fieldRowTo(w, "backup", tildePath(a.backup)+gone(a.backup, fmt.Sprintf("removed; the newest %d backups are kept", backupKeep)))
	}
}

// gone is a dim note when path no longer exists, "" while it does.
func gone(path, note string) string {
	if _, err := os.Lstat(path); err == nil {
		return ""
	}
	return dim(" (" + note + ")")
}

// updateConfirmChecks are doctor's checks for what fails every update's
// confirmation: the rollback drill's switch left on, and a daemon this
// machine's own client cannot verify. Each reports only when it has
// something to say.
func updateConfirmChecks(cfg config.Config) []check {
	var out []check
	if c, ok := updateDrillCheck(cfg); ok {
		out = append(out, c)
	}
	if c, ok := tlsTrustCheck(cfg); ok {
		out = append(out, c)
	}
	return out
}

// updateDrillCheck warns while the rollback drill's switch,
// <data_dir>/update/test-fail-confirm, exists: the updater then fails every
// confirmation on purpose, so a drill left behind turns every update --
// automatic, Update now or `seamlessd update` -- into a rollback. It tests the
// file the way the updater does (update_engine.go), and says nothing while it
// is absent.
func updateDrillCheck(cfg config.Config) (check, bool) {
	path := filepath.Join(update.StateDir(cfg.DataDir), confirmDrillName)
	if _, err := os.Lstat(path); err != nil {
		return check{}, false
	}
	return check{statusWarn, "update drill", path +
		" exists: the rollback drill is on, so every update fails its confirmation and rolls back -- delete it to end the drill"}, true
}

// tlsTrustCheck reports whether this machine's own client trusts the daemon:
// config.HTTPClient, the one TLS trust decision, which the hooks and the seam
// CLI dial with and the updater confirms an update through (/healthz at
// server_url). A certificate that client cannot verify may well work for
// every other machine and still fail here, and then every update fails its
// confirmation and rolls back, and this machine's hooks fail the same way.
//
// It is decided from the config alone, without a dial. The verdict is
// x509.Verify of the configured chain, under that client's own roots (the
// system's, plus tls.ca_file), for the host server_url names: the
// verification crypto/tls runs in the client's handshake. So it agrees with
// a live request whenever server_url reaches this daemon
// (TestTLSTrustCheck_AgreesWithALiveRequest), and it still answers while the
// daemon is down, which is how doctor often finds it. A server_url that
// reaches the daemon through a proxy presenting another certificate is
// outside it, as it is outside the tls row.
//
// A tls.ca_file the client cannot load fails every dial, TLS or not, so it
// warns on any install. With TLS off and a usable client there is nothing to
// say.
func tlsTrustCheck(cfg config.Config) (check, bool) {
	const name = "tls trust"
	const consequence = "this machine's hooks cannot reach the daemon, and every update would fail its confirmation and then its rollback's, ending not rolled back with automatic updates paused"
	hc, err := cfg.HTTPClient(0)
	if err != nil {
		return check{statusWarn, name, err.Error() + " -- " + consequence + "; fix or unset tls.ca_file"}, true
	}
	if !cfg.TLSEnabled() {
		return check{}, false
	}
	base := cfg.ServerURL()
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" {
		return check{}, false
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil || len(pair.Certificate) == 0 {
		return check{}, false // the tls row fails on it
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return check{}, false // the tls row fails on it
	}
	cannot := func(why error) (check, bool) {
		return check{statusWarn, name, fmt.Sprintf("this machine's client cannot verify %s (%v) -- %s; %s",
			base, why, consequence, trustRemedy(why, cfg))}, true
	}
	opts := x509.VerifyOptions{DNSName: cfg.ServerHost(), Intermediates: x509.NewCertPool()}
	for _, der := range pair.Certificate[1:] {
		// The client's handshake parses every certificate the daemon sends,
		// and one that does not parse fails it.
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return cannot(err)
		}
		opts.Intermediates.AddCert(c)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		return check{statusInfo, name, "not checked: the client's transport cannot be inspected"}, true
	}
	if tr.TLSClientConfig != nil {
		opts.Roots = tr.TLSClientConfig.RootCAs
	}
	if _, err := leaf.Verify(opts); err != nil {
		return cannot(err)
	}
	return check{statusOK, name, "this machine's client verifies " + base + ", which its hooks dial and an update confirms through"}, true
}

// trustRemedy is the repair for a certificate this machine's client cannot
// verify, by why.
func trustRemedy(err error, cfg config.Config) string {
	var host x509.HostnameError
	var authority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &host):
		return fmt.Sprintf("reissue %s for %s, or correct server_url", cfg.TLS.CertFile, cfg.ServerHost())
	case errors.As(err, &authority):
		return fmt.Sprintf("set tls.ca_file to the CA that signed %s, or trust that CA system-wide", cfg.TLS.CertFile)
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return fmt.Sprintf("renew %s", cfg.TLS.CertFile)
	default:
		return fmt.Sprintf("check the chain in %s against tls.ca_file", cfg.TLS.CertFile)
	}
}

// whenText renders a scheduled time relative to now: "in 3h", or "now" once due.
func whenText(t, now time.Time) string {
	if t.IsZero() || !t.After(now) {
		return "now"
	}
	return "in " + humanDuration(t.Sub(now))
}

// humanDuration renders a duration compactly: "45s", "12m", "5h", "3d".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d/time.Second)))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
