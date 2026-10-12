package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// The console's view of the background update check: Settings > Updates
// (settings_updates.go, with automatic updates in updates_auto.go), the Home
// health strip's version fact, the one-day "updated" banner and the trouble
// banner every page carries, and the update event kinds.
//
// Every version and link these surfaces render is a parsed update.Version or
// update.ReleaseURL -- never text from the release API (constraint
// update-surfaces-render-parsed-versions-only).

// UpdatesView is what the console needs from the background update check
// (*update.Checker in the daemon). CheckNow, ApplyNow and Resume are
// Settings > Updates' "Check now", "Update now" and "Resume automatic
// updates"; Refresh republishes the status after a switch is stored.
type UpdatesView interface {
	Status() update.Status
	CheckNow(ctx context.Context) (update.Status, error)
	Refresh(ctx context.Context) (update.Status, error)
	ApplyNow(ctx context.Context) (update.Status, error)
	Resume(ctx context.Context) (update.Status, error)
}

// The daemon's checker is the one production UpdatesView.
var _ UpdatesView = (*update.Checker)(nil)

// updatedBannerWindow is how long after an upgrade every page says so: the
// same day the briefing's "Seamless updated" line covers (internal/update).
const updatedBannerWindow = 24 * time.Hour

// updatesSectionHref is Settings > Updates, where every update surface links.
const updatesSectionHref = "/console/settings?s=updates"

// recentUpgrade returns the version change the daemon observed at startup when
// it is an upgrade made within updatedBannerWindow of now, else nil. A
// downgrade is the owner's own act (or a rollback), never news to announce.
func recentUpgrade(st update.Status, now time.Time) *update.Updated {
	u := st.Updated
	if u == nil || u.Direction != update.DirectionUpgrade {
		return nil
	}
	if age := now.Sub(u.At); age < 0 || age >= updatedBannerWindow {
		return nil
	}
	return u
}

// updateBanner is the note every page carries for a day after the daemon
// upgraded. It is a banner rather than a toast because the tab's live stream
// is reconnecting right after the restart, and a toast would be missed.
type updateBanner struct {
	// To and From are the parsed versions ("0.7.3"; the template adds the v).
	// To also keys the per-tab dismissal, so a newer update shows again.
	To, From string
	// Ago is how long ago the new version started ("5h ago", "just now"),
	// and At the exact time, for the hover.
	Ago, At string
	// NotesURL is the release page of To (update.ReleaseURL).
	NotesURL string
	// CodexHooks reports that Codex's hooks.json changed with the update:
	// Codex skips changed hooks until the owner re-approves them in its
	// /hooks, so the banner says so.
	CodexHooks bool
	// Rewire reports that the attempt which brought To applied with warnings:
	// the banner names seamlessd install-hooks, which re-wires the agents. It
	// is the attempt's flag, never its error text.
	Rewire bool
}

// updateBannerFor builds the banner from st, or nil when the daemon did not
// upgrade within the last day.
func updateBannerFor(st update.Status, now time.Time) *updateBanner {
	u := recentUpgrade(st, now)
	if u == nil {
		return nil
	}
	b := &updateBanner{
		To: u.To.String(), From: u.From.String(),
		Ago: agoPhrase(u.At), At: ts(u.At),
		NotesURL:   update.ReleaseURL(u.To),
		CodexHooks: u.CodexHooks,
	}
	if a := st.LastAttempt; a != nil && a.Outcome == update.OutcomeApplied && a.Warnings && a.To == u.To {
		b.Rewire = true
	}
	return b
}

// updateChrome builds the update banners every page carries: the one-day
// "updated" banner and the trouble banner. Both are nil when this process runs
// no update check.
func (s *Service) updateChrome(now time.Time) (*updateBanner, *updateAlert) {
	if s.cfg.Updates == nil {
		return nil, nil
	}
	st := s.cfg.Updates.Status()
	return updateBannerFor(st, now), updateAlertFor(st, now)
}

// versionFact is the health strip's version statement. With no update check in
// this process it is today's plain build version, linked to Your setup. With
// one it links to Settings > Updates and says the one thing the owner may want
// to know: an update under way -- confirming (ok) once this daemon already
// runs its version -- else a newer release while checks are on -- warn when
// installing it is the owner's call, neutral when this install installs it by
// itself or the owner's own hold skips it -- else an upgrade within the last
// day (ok), else the plain version. ok is false when there is no version to
// state at all.
func (s *Service) versionFact(now time.Time) (healthFact, bool) {
	version := strings.TrimSpace(s.cfg.Version)
	if s.cfg.Updates == nil {
		if version == "" {
			return healthFact{}, false
		}
		return healthFact{Icon: "info", Text: "version " + version, Href: "/console/settings?s=setup"}, true
	}
	st := s.cfg.Updates.Status()
	if a := st.Applying; a != nil && st.Confirming() {
		return healthFact{
			Icon: "refresh-cw", Tone: "ok", Href: updatesSectionHref,
			Text:  "v" + a.To.String() + " -- confirming the update",
			Title: "Updated from v" + a.From.String() + "; the updater is confirming that v" + a.To.String() + " answers",
		}, true
	}
	if a := st.Applying; a != nil {
		return healthFact{
			Icon: "refresh-cw", Href: updatesSectionHref,
			Text:  "v" + a.From.String() + " -- updating to v" + a.To.String(),
			Title: "Updating to v" + a.To.String() + ": " + stageWords(a.Stage),
		}, true
	}
	if cur, ok := st.Current(); ok && st.Settings.Check && st.Available && st.Newest != nil {
		newest := st.Newest.Version.String()
		tone, then := newestFate(st, now)
		return healthFact{
			Icon: "refresh-cw", Tone: tone, Href: updatesSectionHref,
			Text:  "v" + cur.String() + " -- v" + newest + " available",
			Title: "Seamless v" + newest + " was published " + day(st.Newest.PublishedAt) + "; " + then,
		}, true
	}
	if u := recentUpgrade(st, now); u != nil {
		return healthFact{
			Icon: "refresh-cw", Tone: "ok", Href: updatesSectionHref,
			Text:  "v" + u.To.String() + " -- updated " + agoPhrase(u.At),
			Title: "Updated from v" + u.From.String() + " at " + ts(u.At),
		}, true
	}
	if version == "" {
		version = strings.TrimSpace(st.Version)
	}
	if version == "" {
		return healthFact{}, false
	}
	return healthFact{Icon: "info", Text: "version " + version, Href: updatesSectionHref}, true
}

// newestFate says what becomes of the newest release, for the version fact's
// hover, with the tone that goes with it. Where this install updates itself
// (ModeAuto) it installs by itself -- and when -- unless automatic updates
// skip it: the owner's own hold is no news (neutral), while a block or a
// missing checksums bundle leaves it to the owner (warn). Anywhere else
// installing it is the owner's call (warn).
func newestFate(st update.Status, now time.Time) (tone, then string) {
	newest := st.Newest.Version
	if mode, _ := st.Mode(); mode != update.ModeAuto {
		if st.Paused != nil && selfUpdating(st) && st.Settings.Auto {
			return "warn", "automatic updates paused themselves, so updating is your call"
		}
		return "warn", "updating is your call"
	}
	switch code, why := update.HeldBack(*st.Newest, st.Blocked, st.Hold); code {
	case update.WaitHeld:
		return "", "this install went back from v" + st.Hold.From.String() + ", so automatic updates skip it until they are resumed"
	case update.WaitBlocked:
		return "warn", "automatic updates skip it (" + why + "), so updating to it is your call"
	case update.WaitUnsigned:
		return "warn", "it carries no signed checksums bundle, so updating to it is your call"
	}
	switch target := st.Target; {
	case target != nil && target.Version == newest && st.Waiting != "":
		return "", "it installs by itself: " + st.Waiting
	case target != nil && target.Version == newest:
		return "", "it installs by itself within a minute"
	case st.Settings.MinAge > 0 && st.Newest.PublishedAt.Add(st.Settings.MinAge).After(now):
		return "", "it installs by itself once it is " + update.SpanWords(st.Settings.MinAge) + " old"
	default:
		return "", "it installs by itself"
	}
}

// updateAvailableSummary is the ledger line of an update.available event,
// built from its payload's versions only once they parse. The live toast
// shows the same line.
func updateAvailableSummary(p map[string]any) string {
	v, ok := update.Parse(payloadStr(p, "version"))
	if !ok {
		return "a newer Seamless release is available"
	}
	line := "Seamless v" + v.String() + " is available"
	if cur, ok := update.Parse(payloadStr(p, "current")); ok {
		line += " (running v" + cur.String() + ")"
	}
	return line
}

// updateAppliedSummary is the ledger line of an update.applied event: an
// upgrade reads "updated to", any other change "changed to", so a rollback or
// a pinned older version is never called an update.
func updateAppliedSummary(p map[string]any) string {
	to, ok := update.Parse(payloadStr(p, "to"))
	if !ok {
		return "Seamless changed version"
	}
	verb := "changed to"
	if payloadStr(p, "direction") == update.DirectionUpgrade {
		verb = "updated to"
	}
	line := verb + " v" + to.String()
	if from, ok := update.Parse(payloadStr(p, "from")); ok {
		line += " (from v" + from.String() + ")"
	}
	return line
}

// updateStartedSummary is the ledger line of an update.started event: the
// daemon handed an update to its updater. It names who asked from the fixed
// why word and the versions once they parse.
func updateStartedSummary(p map[string]any) string {
	to, ok := update.Parse(payloadStr(p, "to"))
	if !ok {
		return "an update started"
	}
	line := "update to v" + to.String() + " started"
	switch payloadStr(p, "why") {
	case update.WhyAuto:
		line = "automatic " + line
	case update.WhyNow:
		line = "Update now: " + line
	}
	if from, ok := update.Parse(payloadStr(p, "from")); ok {
		line += " (from v" + from.String() + ")"
	}
	return line
}

// updateFailedSummary is the ledger line of an update.failed event: an update
// the daemon started did not apply. It is built from the parsed target version,
// the fixed outcome word and, for a failure the updater's gates refused, the
// fixed words for that refusal -- the event never carries the updater's error
// text, and the line never names a stage or a path.
func updateFailedSummary(p map[string]any) string {
	to, ok := update.Parse(payloadStr(p, "to"))
	if !ok {
		return "an update did not apply"
	}
	line := "the update to v" + to.String()
	switch payloadStr(p, "outcome") {
	case update.OutcomeRolledBack:
		return line + " rolled back"
	case update.OutcomeFailed:
		if r := payloadStr(p, "refusal"); update.KnownRefusal(r) {
			return line + " failed: " + update.RefusalWords(r)
		}
		return line + " failed"
	case update.OutcomeBroken:
		return line + " could not be rolled back cleanly"
	case update.OutcomeInterrupted:
		return line + " was interrupted"
	default:
		return line + " did not apply"
	}
}

// intervalPhrase renders a check interval for "every ...": "6 hours", "hour",
// "day", "3 days", or the compact duration when it is not whole hours
// ("1h30m").
func intervalPhrase(d time.Duration) string {
	const dayLen = 24 * time.Hour
	switch {
	case d < time.Hour || d%time.Hour != 0:
		return config.Duration(d).String()
	case d%dayLen == 0:
		return countPhrase(int(d/dayLen), "day")
	default:
		return countPhrase(int(d/time.Hour), "hour")
	}
}

// countPhrase is "hour" for one and "6 hours" for more.
func countPhrase(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// untilPhrase is ago() looking forward, rounded up rather than floored (see
// update.UntilWords): "in 22m", "in 8h 58m", "in 3d 4h". A time already
// reached, or under a minute away, is "within a minute": the checker's
// one-minute tick picks a due check up by then.
func untilPhrase(t, now time.Time) string {
	d := t.Sub(now)
	if d <= time.Minute {
		return "within a minute"
	}
	return "in " + update.UntilWords(d)
}
