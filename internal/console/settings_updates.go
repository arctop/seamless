package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// Settings > Updates: what the background update check and automatic updates
// do on this install, what they last saw and did, and the owner's controls --
// the check switch, the automatic-updates switch, a reset to the config file,
// "Check now", "Update now" and "Resume automatic updates". The section and its
// routes read and steer Config.Updates; with none (a process that runs no
// update check) the section says so and every POST answers with an error
// flash.

// updatesNotRunning is the flash every Updates POST answers with when this
// process runs no update check.
const updatesNotRunning = "The update check is not running in this process, so there is nothing to change here."

// updatesLockedText names where a locked switch was set and where to change it.
const updatesLockedText = "update.check: false is set in the config file or SEAMLESS_UPDATE_CHECK; change it there."

// updatesCheckerDown is the flash for a POST the stopped checker cannot serve.
const updatesCheckerDown = "The update checker is not running. Restart the daemon to revive it."

// updatesPanel is the Updates section's view of the update check's snapshot.
// Every version and link in it comes from parsed update.Version values and
// update.ReleaseURL. The free text it shows is this daemon's own: the check's
// last error, and the last update attempt's error in the Last attempt row.
type updatesPanel struct {
	// Running is false when this process runs no update check: the section
	// says so and offers nothing else.
	Running bool
	// Stopped reports that the checker's loop died after an internal error.
	Stopped bool

	// Checking is the effective update.check. Mode and ModeReason are
	// Status.Mode() in words ("Automatic", "Notify" or "Off", and why).
	Checking   bool
	Mode       string
	ModeReason string
	// Interval is the time between scheduled checks, for "every ..." ("6
	// hours").
	Interval string
	// Source names who decided Checking (update.SourceDefault, SourceConfig
	// or SourceConsole), with the owner-facing label and note for it. Locked
	// marks an explicit file/env false the console may not turn on.
	Source      string
	SourceLabel string
	SourceNote  string
	Locked      bool

	// Version is the running build: "v0.7.2", or the build's own version
	// string when it is not a published release ("0.0.0-dev"). Build says
	// which kind of build it is ("release build", "built from source",
	// "snapshot build"), InstallReason why this install is told about
	// updates rather than updated (empty when the mode line already says
	// it), and Hint the command the owner runs to update.
	Version       string
	Build         string
	InstallReason string
	Hint          string

	// Newest is the newest installable release the last successful check saw
	// ("v0.7.3"; empty before one has), with its publication time and release
	// page. Available marks it newer than the running build; Comparable is
	// false for a build that is not a published release, which is never
	// compared.
	Newest          string
	NewestPublished time.Time
	NewestURL       string
	Available       bool
	Comparable      bool

	// CheckedAt is the last successful check; NextCheck when the next
	// scheduled one runs ("in 5h"), set only while checks are on and the
	// checker runs.
	CheckedAt   time.Time
	NextCheck   string
	NextCheckAt time.Time
	// Error is the current streak of failed checks, nil when the last check
	// worked.
	Error *updatesError
	// CheckNowBlocked says why "Check now" cannot run (checks are off, or the
	// checker stopped); empty when it can.
	CheckNowBlocked string
	// LockedNote says where a locked switch was set and where to change it
	// (the same sentence the refused save flashes); empty when not locked.
	LockedNote string
	// Overridden reports a console choice in force for the check switch or
	// the automatic-updates switch, which Reset clears.
	Overridden bool

	// Automatic updates (plan 2.4), filled by autoRows (updates_auto.go).
	// SelfUpdating reports that this install can install a release by itself
	// at all; the Auto row and switch, Update now, Resume and the rows about
	// the pending update and what holds it back appear only then. Applying
	// and LastAttempt show whatever the mode: an update run by hand is shown
	// too.
	SelfUpdating bool
	Auto         updatesAutoRow
	Next         *updatesNext
	Applying     *updatesApplying
	LastAttempt  *updatesAttempt
	Paused       *updatesPaused
	Hold         *updatesHold
	Blocked      []updatesBlock
	Backoff      *updatesBackoff
	UpdateNow    updatesAction
	Resume       updatesAction
}

// updatesError is a streak of failed checks, in words.
type updatesError struct {
	// Line says how many checks failed, since when, and why ("The last 3
	// checks failed, the first 2h ago (rate limited by GitHub)").
	Line    string
	Since   time.Time
	Message string
}

// updatesPanelAt builds the Updates section from the checker's snapshot.
func (s *Service) updatesPanelAt(now time.Time) updatesPanel {
	if s.cfg.Updates == nil {
		return updatesPanel{}
	}
	st := s.cfg.Updates.Status()
	mode, modeReason := st.Mode()
	p := updatesPanel{
		Running:    true,
		Stopped:    st.Stopped,
		Checking:   st.Settings.Check,
		Mode:       "Off",
		ModeReason: modeReason,
		Interval:   intervalPhrase(st.Settings.CheckInterval),
		Source:     st.Settings.CheckSource,
		Locked:     st.Settings.CheckLocked,
		Hint:       st.Install.Hint,
		CheckedAt:  st.CheckedAt,
	}
	switch mode {
	case update.ModeNotify:
		p.Mode = "Notify"
	case update.ModeAuto:
		p.Mode = "Automatic"
	}
	p.SourceLabel, p.SourceNote = updatesSource(st.Settings)
	p.Overridden = st.Settings.CheckSource == update.SourceConsole || st.Settings.AutoSource == update.SourceConsole

	cur, comparable := st.Current()
	p.Comparable = comparable
	switch {
	case comparable:
		p.Version = "v" + cur.String()
	case strings.TrimSpace(st.Version) != "":
		p.Version = strings.TrimSpace(st.Version)
	default:
		p.Version = "unknown"
	}
	switch {
	case st.Release:
		p.Build = "release build"
	case st.Distribution == update.DistributionRelease:
		// Stamped by goreleaser without a clean X.Y.Z: a snapshot, which
		// counts as a source build everywhere the stamp matters.
		p.Build = "snapshot build"
	default:
		p.Build = "built from source"
	}
	// The install's reason once: the mode line already carries it while
	// checking, and the build line says "built from source" for that kind.
	if reason := strings.TrimSpace(st.Install.Reason); !strings.Contains(modeReason, reason) && st.Install.Kind != update.KindSource {
		p.InstallReason = reason
	}

	if n := st.Newest; n != nil {
		p.Newest = "v" + n.Version.String()
		p.NewestPublished = n.PublishedAt
		p.NewestURL = update.ReleaseURL(n.Version)
		p.Available = st.Available
	}
	if p.Checking && !p.Stopped && !st.NextCheckAt.IsZero() {
		p.NextCheck, p.NextCheckAt = untilPhrase(st.NextCheckAt, now), st.NextCheckAt
	}
	if ce := st.CheckError; ce != nil {
		p.Error = &updatesError{Line: checkErrorLine(*ce), Since: ce.Since, Message: ce.Message}
	}
	switch {
	case !p.Checking:
		p.CheckNowBlocked = "Update checks are off"
	case p.Stopped:
		p.CheckNowBlocked = "The update checker is not running"
	}
	if p.Locked {
		p.LockedNote = updatesLockedText
	}
	autoRows(&p, st, now)
	return p
}

// updatesSource names who decided the check switch, and what that means for
// the console's own switch -- or, when only the automatic-updates switch is
// this console's choice, says that one instead, since Reset clears it.
func updatesSource(set update.Settings) (label, note string) {
	switch {
	case set.CheckLocked:
		return "Set in the config file or environment",
			"update.check: false is final there, so this console cannot turn checks on."
	case set.CheckSource == update.SourceConsole:
		return "Set in this console", "It wins over the config file and the build's default until you reset it."
	case set.AutoSource == update.SourceConsole && set.CheckSource == update.SourceConfig:
		return "Automatic updates set in this console",
			"That choice wins over the config file until you reset it; update checks follow the config file."
	case set.AutoSource == update.SourceConsole:
		return "Automatic updates set in this console",
			"That choice wins over the config file until you reset it; update checks follow the build's default."
	case set.CheckSource == update.SourceConfig:
		return "Set in the config file or environment",
			"update.check: true is set there; you can still turn checks off here."
	default:
		return "Default for this build",
			"Release builds check for updates and builds from source do not. Switch it here, or set update.check in the config file."
	}
}

// checkErrorLine says how long a streak of failed checks has run, and why.
func checkErrorLine(ce update.CheckError) string {
	why := "GitHub could not be reached"
	if ce.Kind == update.CheckErrorRateLimited {
		why = "rate limited by GitHub"
	}
	if ce.Count <= 1 {
		return fmt.Sprintf("The last check failed %s (%s)", agoPhrase(ce.Since), why)
	}
	return fmt.Sprintf("The last %d checks failed, the first %s (%s)", ce.Count, agoPhrase(ce.Since), why)
}

// settingsUpdatesSave stores the console's update-check switch: form field
// check, "on" or "off". Anything else is an error naming the two, and "on"
// while the config file or environment says update.check: false is refused
// with nothing stored -- the file's false is final (update.Effective). The
// stored override keeps whatever else it carries (Auto). The checker then
// re-reads it, so the section the redirect lands on shows the new state.
func (s *Service) settingsUpdatesSave(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		settingsUpdatesFlash(w, r, updatesNotRunning)
		return
	}
	on, refusal := onOffField(r.PostForm, "check")
	if refusal != "" {
		settingsUpdatesFlash(w, r, refusal)
		return
	}
	if on && s.cfg.Updates.Status().Settings.CheckLocked {
		settingsUpdatesFlash(w, r, "Update checks stay off: "+updatesLockedText)
		return
	}

	ctx := r.Context()
	o, replaced, err := s.updateOverrideForSave(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	o.Check = &on
	if err := store.SetUpdateOverride(ctx, s.writeDB(), o); err != nil {
		s.serverError(w, r, err)
		return
	}

	msg := "Update checks turned off: this install makes no update request from now on."
	if on {
		msg = "Update checks turned on."
	}
	if replaced {
		msg += " The stored update setting was unreadable and has been replaced."
	}
	st, err := s.cfg.Updates.Refresh(ctx)
	if err != nil {
		s.updatesRefreshFailed(w, r, msg, err)
		return
	}
	if on && st.Settings.Check && !st.NextCheckAt.IsZero() {
		msg += " The next check runs " + untilPhrase(st.NextCheckAt, time.Now()) + "."
	}
	settingsUpdatesNotice(w, r, msg)
}

// settingsUpdatesAuto stores the console's automatic-updates switch: form
// field auto, "on" or "off", validated like the check switch. "on" is refused
// with nothing stored while the config file or environment keeps automatic
// updates off -- update.auto: false, or a locked update.check: false, since
// nothing installs without checks (Settings.AutoLocked). With checks merely
// off the choice is stored and applies once they are back on. The stored
// override keeps its Check, and the checker then re-reads it.
func (s *Service) settingsUpdatesAuto(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		settingsUpdatesFlash(w, r, updatesNotRunning)
		return
	}
	on, refusal := onOffField(r.PostForm, "auto")
	if refusal != "" {
		settingsUpdatesFlash(w, r, refusal)
		return
	}
	if set := s.cfg.Updates.Status().Settings; on && set.AutoLocked {
		settingsUpdatesFlash(w, r, "Automatic updates stay off: "+autoLockedText(set))
		return
	}

	ctx := r.Context()
	o, replaced, err := s.updateOverrideForSave(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	o.Auto = &on
	if err := store.SetUpdateOverride(ctx, s.writeDB(), o); err != nil {
		s.serverError(w, r, err)
		return
	}

	msg := "Automatic updates turned off: new releases are still announced, and installing one is your call."
	if on {
		msg = "Automatic updates turned on."
	}
	if replaced {
		msg += " The stored update setting was unreadable and has been replaced."
	}
	st, err := s.cfg.Updates.Refresh(ctx)
	if err != nil {
		s.updatesRefreshFailed(w, r, msg, err)
		return
	}
	if on {
		msg += autoOnNote(st)
	}
	settingsUpdatesNotice(w, r, msg)
}

// autoOnNote says what turning automatic updates on does here, from the
// status the checker republished.
func autoOnNote(st update.Status) string {
	mode, reason := st.Mode()
	switch {
	case !st.Settings.Check:
		return " They apply once update checks are back on."
	case mode == update.ModeAuto:
		return " Seamless " + reason + "."
	case st.Paused != nil && selfUpdating(st):
		return " They paused themselves earlier (" + update.PauseWords(st.Paused.Reason) + "): resume them to let them run again."
	case !selfUpdating(st):
		return " This install does not install releases by itself, so nothing changes here."
	default:
		return ""
	}
}

// onOffField reads a required on|off form field: the value, or the flash that
// refuses it, naming the valid values.
func onOffField(form url.Values, name string) (on bool, refusal string) {
	values, present := form[name]
	switch {
	case !present:
		return false, name + " is required: valid values are on, off"
	case len(values) != 1:
		return false, name + " must be given exactly once: valid values are on, off"
	case values[0] != "on" && values[0] != "off":
		return false, fmt.Sprintf("%s invalid %q: valid values are on, off", name, values[0])
	}
	return values[0] == "on", ""
}

// updateOverrideForSave reads the stored update override a switch is about to
// change. A row that does not decode cannot be kept, and refusing would leave
// the switches stuck: the checker already ignores it, so the save replaces it
// (replaced), and the flash says so.
func (s *Service) updateOverrideForSave(ctx context.Context) (o config.UpdateOverride, replaced bool, err error) {
	o, _, err = store.UpdateOverride(ctx, s.cfg.DB)
	if err == nil {
		return o, false, nil
	}
	if !undecodableOverride(err) {
		return config.UpdateOverride{}, false, err
	}
	s.logger.Warn("console: replacing an unreadable update override", "error", err)
	return config.UpdateOverride{}, true, nil
}

// undecodableOverride reports whether err is a stored update override that is
// not valid JSON for config.UpdateOverride, as opposed to a failed read.
func undecodableOverride(err error) bool {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.As(err, &syntax) || errors.As(err, &typ)
}

// settingsUpdatesReset clears the console's update override -- both switches
// -- so the config file and environment (or, without either, the build)
// decide again.
func (s *Service) settingsUpdatesReset(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		settingsUpdatesFlash(w, r, updatesNotRunning)
		return
	}
	ctx := r.Context()
	if err := store.ClearUpdateOverride(ctx, s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	self := selfUpdating(s.cfg.Updates.Status())
	msg := "Update checks follow the config file again."
	if self {
		msg = "Update checks and automatic updates follow the config file again."
	}
	st, err := s.cfg.Updates.Refresh(ctx)
	if err != nil {
		s.updatesRefreshFailed(w, r, msg, err)
		return
	}
	if !self {
		settingsUpdatesNotice(w, r, "Update checks follow the config file again: they are "+onOff(st.Settings.Check)+".")
		return
	}
	settingsUpdatesNotice(w, r, "Update checks and automatic updates follow the config file again: checks are "+
		onOff(st.Settings.Check)+", automatic updates are "+onOff(st.Settings.Auto)+".")
}

// onOff is "on" or "off".
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// updatesRefreshFailed answers a stored change the checker could not pick up
// at once; saved is the sentence that is true regardless. A stopped checker
// reads the stored row when the daemon next starts, so that is a notice;
// anything else is an error naming what went wrong.
func (s *Service) updatesRefreshFailed(w http.ResponseWriter, r *http.Request, saved string, err error) {
	if errors.Is(err, update.ErrNotRunning) {
		settingsUpdatesNotice(w, r, saved+" The update checker is not running, so the change applies when the daemon next starts.")
		return
	}
	settingsUpdatesFlash(w, r, saved+" The update checker has not picked the change up yet: "+err.Error())
}

// settingsUpdatesCheck is "Check now": one request to GitHub, right away. The
// checker refuses while checks are off and within a minute of the previous
// check; each refusal is its own clear message.
func (s *Service) settingsUpdatesCheck(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		settingsUpdatesFlash(w, r, updatesNotRunning)
		return
	}
	st, err := s.cfg.Updates.CheckNow(r.Context())
	switch {
	case errors.Is(err, update.ErrChecksOff):
		settingsUpdatesFlash(w, r, "Update checks are off, so nothing was asked. Turn them on first.")
	case errors.Is(err, update.ErrTooSoon):
		settingsUpdatesFlash(w, r, "Checked less than a minute ago. Try again in a minute.")
	case errors.Is(err, update.ErrNotRunning):
		settingsUpdatesFlash(w, r, updatesCheckerDown)
	case err != nil && r.Context().Err() != nil:
		// This request stopped waiting, not the check: it runs on in the
		// checker, which records its own result.
		settingsUpdatesFlash(w, r, "Stopped waiting for the check. It carries on; this section shows its result when it finishes.")
	case err != nil:
		settingsUpdatesFlash(w, r, "The check failed: "+err.Error())
	default:
		settingsUpdatesNotice(w, r, checkedNowNotice(st))
	}
}

// settingsUpdatesApply is "Update now": install the newest release at once,
// skipping the soak, the wait for an idle daemon, blocks and the backoff, and
// lifting a hold (Checker.ApplyNow). The form asks for a confirm first. Each
// refusal is its own message, built from the status the checker returned;
// none carries an update attempt's error text.
func (s *Service) settingsUpdatesApply(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		settingsUpdatesFlash(w, r, updatesNotRunning)
		return
	}
	st, err := s.cfg.Updates.ApplyNow(r.Context())
	switch {
	case err == nil:
		settingsUpdatesNotice(w, r, appliedNowNotice(st))
	case errors.Is(err, update.ErrChecksOff):
		settingsUpdatesFlash(w, r, "Update checks are off, so nothing was installed. Turn them on first.")
	case errors.Is(err, update.ErrCannotApply):
		settingsUpdatesFlash(w, r, "Nothing was installed: this daemon does not install updates by itself ("+
			cannotApplyWhy(st)+"). To update it, run "+st.Install.Hint+".")
	case errors.Is(err, update.ErrInProgress):
		msg := "An update is already in progress."
		if a := st.Applying; a != nil {
			msg = "An update to v" + a.To.String() + " is already in progress."
		}
		settingsUpdatesFlash(w, r, msg+" This section shows how it goes.")
	case errors.Is(err, update.ErrTooSoon):
		settingsUpdatesFlash(w, r, "The last release check failed less than a minute ago, so nothing was installed. Try again in a minute.")
	case errors.Is(err, update.ErrUpToDate):
		msg := "Nothing to install: no newer release is known."
		if cur, ok := st.Current(); ok {
			msg = "Nothing to install: no release newer than v" + cur.String() + " is known."
		}
		settingsUpdatesFlash(w, r, msg)
	case errors.Is(err, update.ErrUnsigned):
		what := "the newest release"
		if st.Newest != nil {
			what = "v" + st.Newest.Version.String()
		}
		settingsUpdatesFlash(w, r, "Nothing was installed: "+what+" carries no signed checksums bundle, which Update now verifies.")
	case errors.Is(err, update.ErrNotRunning):
		settingsUpdatesFlash(w, r, updatesCheckerDown)
	case r.Context().Err() != nil:
		// This request stopped waiting, not the checker: the command runs on,
		// and the section shows an update once it starts.
		settingsUpdatesFlash(w, r, "Stopped waiting for Update now. It carries on; this section shows the update once it starts.")
	default:
		settingsUpdatesFlash(w, r, "Update now failed: "+err.Error())
	}
}

// appliedNowNotice says that Update now handed the update to the updater,
// from the status the checker returned.
func appliedNowNotice(st update.Status) string {
	a := st.Applying
	if a == nil {
		return "The update started. This section shows how it goes."
	}
	return "Updating to v" + a.To.String() + " (from v" + a.From.String() + "). " +
		"The daemon restarts to finish; this section shows how it went."
}

// cannotApplyWhy says why this daemon installs nothing by itself, the way the
// checker decides it: an install the installer did not make, or no updater
// wired.
func cannotApplyWhy(st update.Status) string {
	switch reason := strings.TrimSpace(st.Install.Reason); {
	case st.Install.NotifyOnly() && reason != "":
		return reason
	case st.Install.NotifyOnly():
		return "the installer did not make this install"
	default:
		return "no updater is wired into this daemon"
	}
}

// settingsUpdatesResume is "Resume automatic updates": lift the pause they
// put on themselves after rollbacks, and the hold a deliberate downgrade set
// (Checker.Resume). It changes no setting. The flash says what it lifted.
func (s *Service) settingsUpdatesResume(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Updates == nil {
		settingsUpdatesFlash(w, r, updatesNotRunning)
		return
	}
	before := s.cfg.Updates.Status()
	st, err := s.cfg.Updates.Resume(r.Context())
	switch {
	case err == nil:
		settingsUpdatesNotice(w, r, resumedNotice(before, st))
	case errors.Is(err, update.ErrNotRunning):
		settingsUpdatesFlash(w, r, updatesCheckerDown)
	case r.Context().Err() != nil:
		settingsUpdatesFlash(w, r, "Stopped waiting for the update checker. The resume carries on; this section shows the result.")
	default:
		settingsUpdatesFlash(w, r, "Could not resume automatic updates: "+err.Error())
	}
}

// resumedNotice says what Resume lifted, comparing the status before it with
// the one it returned.
func resumedNotice(before, after update.Status) string {
	var lifted []string
	if pz := before.Paused; pz != nil && after.Paused == nil {
		lifted = append(lifted, "the pause ("+update.PauseWords(pz.Reason)+")")
	}
	if h := before.Hold; h != nil && after.Hold == nil {
		lifted = append(lifted, "the hold on releases up to v"+h.Through.String())
	}
	if len(lifted) == 0 {
		return "Nothing to resume: automatic updates were neither paused nor holding releases back."
	}
	msg := "Lifted " + strings.Join(lifted, " and ") + "."
	if mode, _ := after.Mode(); mode == update.ModeAuto {
		return msg + " Automatic updates can install new releases again."
	}
	if !after.Settings.Auto {
		return msg + " Automatic updates are switched off, so nothing installs by itself until you turn them on."
	}
	return msg
}

// checkedNowNotice says what a successful "Check now" found, from the parsed
// versions in the status it returned.
func checkedNowNotice(st update.Status) string {
	switch {
	case st.Newest == nil:
		return "Checked just now: no installable release was found."
	case st.Available:
		return "Checked just now: v" + st.Newest.Version.String() + " is available."
	}
	if _, ok := st.Current(); !ok {
		return "Checked just now: the newest release is v" + st.Newest.Version.String() + "."
	}
	return "Checked just now: up to date."
}

func settingsUpdatesFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "updates", "error", msg, "updates")
}

func settingsUpdatesNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "updates", "notice", msg, "updates")
}
