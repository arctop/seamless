package update

import (
	"fmt"
	"slices"
	"time"
)

// The automatic update's policy (plan 2.4), as pure functions of explicit
// inputs: Target picks the release, Decide picks the moment. The checker
// gathers the inputs and acts on the answers.

// Automatic-update timing.
const (
	// quietFor is the lull the deadline path waits for: this long since the
	// last counted request finished.
	quietFor = 90 * time.Second

	// postUpdateCheckDelay is how long a daemon that starts on a different
	// version waits before it asks GitHub anything, the first check and any
	// pre-update re-check alike: the update that just happened settles, and
	// installs that updated together do not ask together.
	postUpdateCheckDelay = 30 * time.Minute

	// recheckFresh is how recent a successful fetch must be for a spawn to
	// rely on it. Older than that, the checker re-checks the release list
	// first (plan 2.4: a conditional re-fetch, so a release pulled in the
	// meantime -- the yank gap -- is never installed, and the soak is
	// measured against a Date header GitHub sent within the last minute).
	recheckFresh = time.Minute

	// recheckSpacing is the least time between two pre-update re-checks, so
	// a decision that keeps falling through cannot spend GitHub's anonymous
	// rate limit (60 requests an hour per address).
	recheckSpacing = 15 * time.Minute

	// backoffBase is the wait after the first failure that blocks nothing;
	// each failure in a row doubles it, up to backoffMax (plan 2.2: 1h-24h).
	backoffBase = time.Hour
	backoffMax  = 24 * time.Hour

	// installFailuresToBlock is how many installer failures in a row on one
	// release, each leaving the install as it was, block that release: one
	// is likely the network (the installer downloads the archive itself),
	// three is the release or this machine.
	installFailuresToBlock = 3

	// rollbacksToPause is how many releases in a row automatic updates may
	// roll back before they turn themselves off (plan 2.2).
	rollbacksToPause = 2

	// maxBlocks bounds the blocked-release list; the oldest go first.
	maxBlocks = 20
)

// Paths an update takes to its spawn, as Spawn.Path, the update.started
// event and the logs name them. Each is a fixed word.
const (
	// PathIdle: no live agent session and nothing in flight.
	PathIdle = "idle"
	// PathDeadline: pending for max_defer, nothing in flight, and 90 seconds
	// without a request.
	PathDeadline = "deadline"
	// PathForced: pending for 1.5x max_defer and nothing in flight.
	PathForced = "forced"
	// PathOverdue: pending for 2x max_defer, whatever is in flight. A request
	// that never returns (a hung tool call) keeps the in-flight count above
	// zero until a restart, which would otherwise hold off every update
	// forever; the restart ends that request, and its client retries.
	PathOverdue = "overdue"
	// PathNow: the owner's Update now (Checker.ApplyNow).
	PathNow = "now"
)

// Wait codes: why an automatic update is not being applied right now, as a
// fixed word for code (Status.WaitCode). Status.Waiting carries the same in
// words for the owner.
const (
	// WaitInProgress: an update is under way.
	WaitInProgress = "in_progress"
	// WaitBackoff: an earlier attempt failed; the next waits for its backoff.
	WaitBackoff = "backoff"
	// WaitSessions: agent sessions are live (and the deadline has not come).
	WaitSessions = "sessions"
	// WaitSessionCount: the live sessions could not be counted, which is
	// never idle.
	WaitSessionCount = "session_count"
	// WaitInFlight: requests are being served.
	WaitInFlight = "in_flight"
	// WaitQuiet: past the deadline, waiting for 90 seconds without requests.
	WaitQuiet = "quiet"
	// WaitActivity: no activity tracker is wired, so only the overdue path
	// can apply.
	WaitActivity = "activity_unknown"
	// WaitRecheck: the release list is re-checked before installing.
	WaitRecheck = "recheck"
	// WaitSave: the state file could not be saved, and a spawn is never
	// started without its record on disk.
	WaitSave = "save"
	// WaitSoak, WaitHeld, WaitBlocked, WaitUnsigned: the newest release is
	// not a target yet (soaking), or is skipped (a hold, a block, no
	// checksums bundle).
	WaitSoak     = "soak"
	WaitHeld     = "held"
	WaitBlocked  = "blocked"
	WaitUnsigned = "unsigned"
)

// Block reasons (Block.Reason): why automatic updates skip a release.
const (
	// BlockRolledBack: an update to it was rolled back.
	BlockRolledBack = "rolled_back"
	// BlockVerify: it failed verification -- its signature, the signing
	// identity, the checksums or a pin -- or it turned out to be pulled.
	BlockVerify = "verify"
	// BlockInstall: its installer failed installFailuresToBlock times in a
	// row, leaving the install as it was each time.
	BlockInstall = "install"
	// BlockBroken: an update to it could not be rolled back cleanly.
	BlockBroken = "broken"
)

// Pause reasons (Pause.Reason): why automatic updates turned themselves off.
const (
	// PauseRollbacks: two releases in a row rolled back.
	PauseRollbacks = "rollbacks"
	// PauseBroken: an update could not be rolled back cleanly.
	PauseBroken = "broken"
)

// serverClock estimates GitHub's clock at now: the Date header of the last
// successful check plus the time since that check, the time since never
// negative and capped at maxElapsed. ok is false when no check has carried a
// Date. The cap keeps a local clock that jumps ahead from moving the
// estimate more than one check interval (scheduleSlack); the spawn path
// re-checks the list within a minute of installing (recheckFresh), so what
// the soak is finally measured against is GitHub's own Date.
func serverClock(serverDate, checkedAt, now time.Time, maxElapsed time.Duration) (time.Time, bool) {
	if serverDate.IsZero() || checkedAt.IsZero() {
		return time.Time{}, false
	}
	return serverDate.Add(min(max(now.Sub(checkedAt), 0), maxElapsed)), true
}

// GitHubClock is serverClock over s, for the CLI: the clock the daemon
// measures soaks and the pending deadline against, at now, with interval the
// effective check_interval. doctor and `update --check` read the same state,
// so on a machine whose clock is off they still say what the daemon decides.
func (s State) GitHubClock(now time.Time, interval time.Duration) (time.Time, bool) {
	return serverClock(s.ServerDate, s.CheckedAt, now, scheduleSlack(interval))
}

// soaked reports whether r is at least minAge old at clock (GitHub's). A zero
// minAge is no soak; with a soak, an unknown (zero) clock soaks nothing.
func soaked(r Release, clock time.Time, minAge time.Duration) bool {
	if minAge <= 0 {
		return true
	}
	return !clock.IsZero() && !r.PublishedAt.Add(minAge).After(clock)
}

// blocked returns the block on v, or nil.
func blocked(blocks []Block, v Version) *Block {
	i := slices.IndexFunc(blocks, func(b Block) bool { return b.Version == v })
	if i < 0 {
		return nil
	}
	return &blocks[i]
}

// held reports whether hold keeps v from automatic updates.
func held(hold *Hold, v Version) bool {
	return hold != nil && v.Compare(hold.Through) <= 0
}

// Target returns the release an automatic update installs: the newest
// release above current that
//
//   - carries the checksums bundle the unattended updater verifies
//     (ChecksumsBundle);
//   - is above a hold's skip_through, when there is a hold;
//   - is not blocked; and
//   - has soaked: published at least minAge before clock, which is GitHub's
//     clock (serverClock), never the local one. A zero clock soaks nothing
//     unless minAge is zero.
//
// It is the maximum by version among those, never the first in list order (a
// backport is listed after the newer release it follows). ok is false when no
// release qualifies. An automatic update never moves to an older version.
func Target(rels []Release, current Version, clock time.Time, minAge time.Duration, blocks []Block, hold *Hold) (Release, bool) {
	var best Release
	found := false
	for _, r := range rels {
		switch {
		case r.Version.Compare(current) <= 0,
			!r.ChecksumsBundle,
			held(hold, r.Version),
			blocked(blocks, r.Version) != nil,
			!soaked(r, clock, minAge):
			continue
		}
		if !found || r.Version.Compare(best.Version) > 0 {
			best, found = r, true
		}
	}
	return best, found
}

// newestWait says why the newest release, above the running version, is not
// an automatic update's target -- as a wait code and in words for the owner
// (Status.Waiting) -- or returns "" when nothing holds it back: one of
// HeldBack's skips, or a soak not over on clock (GitHub's).
func newestWait(r Release, clock time.Time, minAge time.Duration, blocks []Block, hold *Hold) (string, string) {
	switch code, why := HeldBack(r, blocks, hold); code {
	case WaitHeld:
		return code, fmt.Sprintf("v%s is held back: %s", r.Version, why)
	case WaitBlocked:
		return code, fmt.Sprintf("automatic updates skip v%s: %s", r.Version, why)
	case WaitUnsigned:
		return code, fmt.Sprintf("v%s %s", r.Version, unsignedWords)
	}
	switch {
	case soaked(r, clock, minAge):
		return "", ""
	case clock.IsZero():
		return WaitSoak, fmt.Sprintf("v%s waits out its %s soak; GitHub's clock is not known yet", r.Version, SpanWords(minAge))
	default:
		return WaitSoak, fmt.Sprintf("v%s waits out its %s soak, about %s to go", r.Version, SpanWords(minAge),
			UntilWords(r.PublishedAt.Add(minAge).Sub(clock)))
	}
}

// backoffWait is the wait after the n-th failure in a row (n >= 1):
// backoffBase doubled per earlier failure, capped at backoffMax.
func backoffWait(n int) time.Duration {
	d := backoffBase
	for i := 1; i < n && d < backoffMax; i++ {
		d *= 2
	}
	return min(d, backoffMax)
}

// DecideInput is everything Decide reads. The checker fills it each tick
// for a target that exists; a test fills it by hand.
type DecideInput struct {
	// Now is the local clock, which the activity stamps and the backoff use.
	Now time.Time
	// Pending is how long an update has been pending for the running
	// version, on GitHub's clock (Pending.Since); MaxDefer is the deadline.
	Pending  time.Duration
	MaxDefer time.Duration
	// InProgress: an attempt is live, or one the daemon spawned has not
	// shown up yet.
	InProgress bool
	// BackoffUntil is when the backoff after a failed attempt ends.
	BackoffUntil time.Time
	// Sessions is the live agent session count (store.LiveSessionCount since
	// now minus session_idle_minutes); SessionsErr is why it could not be
	// counted, which is never idle.
	Sessions    int
	SessionsErr error
	// Activity reports whether a request tracker is wired; InFlight and
	// LastActivity are its snapshot: requests being served, and when the
	// last counted one finished (the tracker's start before any has).
	Activity     bool
	InFlight     int64
	LastActivity time.Time
}

// Decision is Decide's answer: apply now by Path, or wait, with Code and
// Wait saying why.
type Decision struct {
	Apply bool
	Path  string // a Path* word, when Apply
	Code  string // a Wait* word, when not
	Wait  string // the same, in words for the owner
}

// waitFor is a Decision to wait.
func waitFor(code, words string) Decision { return Decision{Code: code, Wait: words} }

// Decide is the plan 2.4 apply table, for a target that exists. First match
// wins:
//
//  1. an attempt is in progress: wait;
//  2. a backoff is running: wait;
//  3. idle -- no live agent session, the count succeeded, nothing in
//     flight: apply (PathIdle);
//  4. pending >= max_defer, nothing in flight, 90s without a request:
//     apply (PathDeadline);
//  5. pending >= 1.5x max_defer and nothing in flight: apply (PathForced);
//  6. pending >= 2x max_defer: apply whatever is in flight (PathOverdue);
//  7. otherwise wait, saying for what.
//
// Row 6 answers a request that never returns: it keeps the in-flight count
// above zero until a restart, so rows 3-5 never hold again, and without row
// 6 one hung tool call would hold off every update. A client looping on a
// bad key (401s) keeps the 90 seconds of quiet from ever holding, so row 4
// never applies; its requests are short, so the in-flight count is zero at
// nearly every sample, and rows 3 (no session live) and 5 still do. With no
// tracker wired (Activity false) nothing is known to be out of flight, and
// only row 6 applies.
func Decide(in DecideInput) Decision {
	switch {
	case in.InProgress:
		return waitFor(WaitInProgress, "an update is in progress")
	case in.Now.Before(in.BackoffUntil):
		return waitFor(WaitBackoff, fmt.Sprintf("an earlier attempt failed; trying again in %s",
			UntilWords(in.BackoffUntil.Sub(in.Now))))
	}
	noneInFlight := in.Activity && in.InFlight == 0
	quiet := noneInFlight && in.Now.Sub(in.LastActivity) >= quietFor
	idle := in.SessionsErr == nil && in.Sessions == 0
	forced := in.MaxDefer + in.MaxDefer/2
	overdue := 2 * in.MaxDefer
	switch {
	case idle && noneInFlight:
		return Decision{Apply: true, Path: PathIdle}
	case in.Pending >= in.MaxDefer && quiet:
		return Decision{Apply: true, Path: PathDeadline}
	case in.Pending >= forced && noneInFlight:
		return Decision{Apply: true, Path: PathForced}
	case in.Pending >= overdue:
		return Decision{Apply: true, Path: PathOverdue}
	}

	// Waiting: name the next thing that has to change.
	latest := ""
	if in.Pending < in.MaxDefer {
		latest = fmt.Sprintf(", or at a lull in requests in %s", UntilWords(in.MaxDefer-in.Pending))
	}
	switch {
	case !in.Activity:
		return waitFor(WaitActivity, fmt.Sprintf("request activity is not tracked, so it installs once it has waited %s (in %s)",
			SpanWords(overdue), UntilWords(overdue-in.Pending)))
	case in.InFlight > 0 && (idle || in.Pending >= in.MaxDefer):
		return waitFor(WaitInFlight, fmt.Sprintf("waiting for %s in flight to finish", Plural(in.InFlight, "request")))
	case in.Pending >= in.MaxDefer:
		return waitFor(WaitQuiet, fmt.Sprintf("past its %s deadline; waiting for %s without requests",
			SpanWords(in.MaxDefer), SpanWords(quietFor)))
	case in.SessionsErr != nil:
		return waitFor(WaitSessionCount, "live agent sessions could not be counted"+latest)
	default:
		return waitFor(WaitSessions, fmt.Sprintf("waiting for %s to go idle%s",
			Plural(int64(in.Sessions), "live agent session"), latest))
	}
}
