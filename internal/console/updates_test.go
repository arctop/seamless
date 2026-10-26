package console

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/update"
)

// fakeUpdates is the console's UpdatesView in tests. Refresh re-reads the
// stored override and merges it over base exactly as the checker does
// (update.Effective), so a save or reset round-trips through the real store
// and the real precedence rule; CheckNow, ApplyNow and Resume answer through
// checkNow, applyNow and resume.
type fakeUpdates struct {
	mu         sync.Mutex
	db         *sql.DB
	base       config.Update
	status     update.Status
	checkNow   func(update.Status) (update.Status, error) // nil: the status, no error
	applyNow   func(update.Status) (update.Status, error) // nil: the status, no error
	resume     func(update.Status) (update.Status, error) // nil: lifts the hold and the pause
	refreshErr error

	checks, refreshes, applies, resumes int
}

func (f *fakeUpdates) ApplyNow(context.Context) (update.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies++
	if f.applyNow == nil {
		return f.status, nil
	}
	st, err := f.applyNow(f.status)
	f.status = st
	return st, err
}

func (f *fakeUpdates) Resume(context.Context) (update.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes++
	if f.resume == nil {
		f.status.Hold, f.status.Paused = nil, nil
		return f.status, nil
	}
	st, err := f.resume(f.status)
	f.status = st
	return st, err
}

func (f *fakeUpdates) Status() update.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeUpdates) CheckNow(context.Context) (update.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.checkNow == nil {
		return f.status, nil
	}
	st, err := f.checkNow(f.status)
	f.status = st
	return st, err
}

func (f *fakeUpdates) Refresh(ctx context.Context) (update.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	if f.refreshErr != nil {
		return f.status, f.refreshErr
	}
	o, _, err := store.UpdateOverride(ctx, f.db)
	if err != nil {
		return f.status, err
	}
	f.status.Settings = update.Effective(f.base, o, f.status.Release)
	return f.status, nil
}

func (f *fakeUpdates) counts() (checks, refreshes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.refreshes
}

// ver is a parsed release version, for fixtures.
func ver(major, minor, patch int) update.Version {
	return update.Version{Major: major, Minor: minor, Patch: patch}
}

// releaseStatus is an installer-made release build of v0.7.2 that checks on
// the default schedule and last found itself up to date four minutes ago.
func releaseStatus(now time.Time) update.Status {
	return update.Status{
		Version: "0.7.2", Distribution: update.DistributionRelease, Release: true,
		Install:     update.Install{Kind: update.KindInstaller, Hint: update.Hint(update.KindInstaller)},
		Settings:    update.Settings{Check: true, CheckSource: update.SourceDefault, CheckInterval: 6 * time.Hour},
		Newest:      &update.Release{Version: ver(0, 7, 2), PublishedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)},
		CheckedAt:   now.Add(-4 * time.Minute),
		NextCheckAt: now.Add(5*time.Hour + 30*time.Minute),
	}
}

// newUpdatesConsole builds a Basic console over a fresh DB whose update check
// is fake (nil fake: no update check in this process). base is the file/env
// update block the fake's Refresh merges the stored override over.
func newUpdatesConsole(t *testing.T, base config.Update, st *update.Status) (*sql.DB, *http.ServeMux, *fakeUpdates) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "basic", Version: "0.7.2"}
	var fake *fakeUpdates
	if st != nil {
		fake = &fakeUpdates{db: db, base: base, status: *st}
		cfg.Updates = fake
	}
	svc, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux, fake
}

// updatesSection returns the Updates section's markup.
func updatesSection(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	rr := getPeek(t, mux, "/console/settings?s=updates")
	require.Equal(t, http.StatusOK, rr.Code)
	sec := regexp.MustCompile(`(?s)<section class="settings-zone settings-section" id="updates">.*?</section>`).
		FindString(rr.Body.String())
	require.NotEmpty(t, sec, "the Updates section renders")
	return sec
}

// formFor returns the <form> posting to action, or "" when there is none.
func formFor(page, action string) string {
	return regexp.MustCompile(`(?s)<form[^>]*action="` + regexp.QuoteMeta(action) + `"[^>]*>.*?</form>`).FindString(page)
}

// locationOf follows a redirect-after-POST to its flash: param "notice" or
// "error", and the message.
func locationOf(t *testing.T, rr *httptest.ResponseRecorder) (param, msg string) {
	t.Helper()
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	u, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/settings", u.Path)
	require.Equal(t, "updates", u.Query().Get("s"))
	require.Equal(t, "updates", u.Fragment, "lands on the section")
	if m := u.Query().Get("error"); m != "" {
		return "error", m
	}
	return "notice", u.Query().Get("notice")
}

// storedOverride reads the console's update override row.
func storedOverride(t *testing.T, db *sql.DB) (config.UpdateOverride, bool) {
	t.Helper()
	o, found, err := store.UpdateOverride(context.Background(), db)
	require.NoError(t, err)
	return o, found
}

func boolPtr(b bool) *bool { return &b }

// Settings > Updates in each state the check can be in.
func TestUpdatesSection_Renders(t *testing.T) {
	now := time.Now()
	locked := config.Update{Check: boolPtr(false)}
	tests := []struct {
		name   string
		base   config.Update
		mutate func(st *update.Status)
		want   []string
		absent []string
	}{
		{
			name: "release build, checking, up to date",
			want: []string{
				`<span class="settings-access editable">`, "Default for this build",
				`<span class="badge ok">Notify</span>`, "this install is told about new releases",
				"One anonymous request to <span class=\"mono\">api.github.com</span> every 6 hours",
				`<span class="mono">v0.7.2</span><span class="faint">release build</span>`,
				`<span class="mono">seamlessd update</span>`,
				`<strong class="mono">v0.7.2</strong>`, "published 2026-10-01", `<span class="badge ok">up to date</span>`,
				`href="https://github.com/arctop/seamless/releases/tag/v0.7.2"`,
				"4m ago", "next check in 5h 30m",
				`<form method="post" action="/console/settings/updates" aria-label="Check for updates">`,
				"Turn update checks off", `<input type="hidden" name="check" value="off">`,
			},
			absent: []string{"update available", `id="updates-reset"`, "updates-error", "role=\"alert\"", "disabled"},
		},
		{
			name: "update available",
			mutate: func(st *update.Status) {
				st.Newest = &update.Release{Version: ver(0, 7, 3), PublishedAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
				st.Available = true
			},
			want: []string{
				`<strong class="mono">v0.7.3</strong>`, "published 2026-10-08", `<span class="badge warn">update available</span>`,
				`href="https://github.com/arctop/seamless/releases/tag/v0.7.3"`,
			},
			absent: []string{"up to date"},
		},
		{
			name: "a streak of failed checks",
			mutate: func(st *update.Status) {
				st.CheckError = &update.CheckError{
					Kind: update.CheckErrorRateLimited, Count: 3, Since: now.Add(-2 * time.Hour),
					Message: "update: GitHub API rate limited (403 Forbidden); next request after 2026-10-09T18:00:00Z",
				}
			},
			want: []string{
				`class="updates-error"`, "The last 3 checks failed, the first 2h ago (rate limited by GitHub)",
				"update: GitHub API rate limited (403 Forbidden); next request after 2026-10-09T18:00:00Z",
			},
		},
		{
			name: "never succeeded",
			mutate: func(st *update.Status) {
				st.Newest, st.CheckedAt = nil, time.Time{}
				st.CheckError = &update.CheckError{Kind: update.CheckErrorUnavailable, Count: 1, Since: now.Add(-10 * time.Minute),
					Message: "update.Fetch: update: release list unavailable: unexpected status 503 Service Unavailable"}
			},
			want: []string{"No check has succeeded yet", "The last check failed 10m ago (GitHub could not be reached)", "Not checked yet"},
		},
		{
			name: "locked off by the config file",
			base: locked,
			mutate: func(st *update.Status) {
				st.Settings = update.Effective(locked, config.UpdateOverride{Check: boolPtr(true)}, true)
			},
			want: []string{
				`<span class="settings-access">`, "Read only", "Set in the config file or environment",
				"update.check: false is final there", `<span class="badge">Off</span>`,
				"update checks are off (update.check: false in the config file or environment)",
				"Nothing: with checks off, this install makes no update request at all.",
				`<button class="btn small primary" type="submit" disabled title="Set in the config file or environment">Turn update checks on</button>`,
				"update.check: false is set in the config file or SEAMLESS_UPDATE_CHECK; change it there.",
			},
			absent: []string{"Editable here", `id="updates-reset"`, "next check"},
		},
		{
			name: "turned off in the console",
			mutate: func(st *update.Status) {
				st.Settings = update.Settings{Check: false, CheckSource: update.SourceConsole, CheckInterval: 6 * time.Hour}
			},
			want: []string{
				"Set in this console", `class="settings-precedence is-override"`,
				`<button class="btn small" type="submit" form="updates-reset">Reset to the config file</button>`,
				`<form id="updates-reset" method="post" action="/console/settings/updates/reset"></form>`,
				"update checks are off (turned off in the console)",
				`<button class="btn small primary" type="submit">Turn update checks on</button>`,
			},
			absent: []string{"SEAMLESS_UPDATE_CHECK; change it there"},
		},
		{
			name: "source build, off by default",
			mutate: func(st *update.Status) {
				*st = update.Status{
					Version: "0.0.0-dev", Distribution: update.DistributionSource,
					Install: update.Install{Kind: update.KindSource, Reason: "built from source (make install, go install or go build)",
						Hint: update.Hint(update.KindSource)},
					Settings:    update.Settings{Check: false, CheckSource: update.SourceDefault, CheckInterval: 6 * time.Hour},
					NextCheckAt: now.Add(time.Hour),
				}
			},
			want: []string{
				`<span class="mono">0.0.0-dev</span><span class="faint">built from source</span>`,
				"builds from source do not check unless update.check is true",
				"git pull &amp;&amp; make install", "Not checked yet", "Default for this build",
			},
			absent: []string{"make install, go install or go build", "next check", "up to date"},
		},
		{
			name:   "a stopped checker",
			mutate: func(st *update.Status) { st.Stopped = true },
			want: []string{
				`<p class="updates-alert" role="alert">`, "The update checker stopped after an internal error.",
				"restart it to revive the check", `disabled title="The update checker is not running"`,
				"Turn update checks off", // the switch still stores, for the next start
				`<span class="badge">Notify</span>`,
			},
			absent: []string{"next check", `<span class="badge ok">Notify</span>`},
		},
		{
			name: "homebrew: told, with the brew command",
			mutate: func(st *update.Status) {
				st.Install = update.Install{Kind: update.KindHomebrew, Reason: "installed by Homebrew, which owns its files",
					Hint: update.Hint(update.KindHomebrew)}
			},
			want: []string{"installed by Homebrew, which owns its files", "brew upgrade --cask arctop/tap/seamless"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := releaseStatus(now)
			if tt.mutate != nil {
				tt.mutate(&st)
			}
			_, mux, _ := newUpdatesConsole(t, tt.base, &st)
			sec := updatesSection(t, mux)
			for _, want := range tt.want {
				require.Contains(t, sec, want)
			}
			for _, gone := range tt.absent {
				require.NotContains(t, sec, gone)
			}
			check := formFor(sec, "/console/settings/updates/check")
			require.NotEmpty(t, check, "Check now is a POST form")
			require.Contains(t, check, `method="post"`)
			require.Equal(t, !st.Settings.Check || st.Stopped, strings.Contains(check, "disabled"),
				"Check now is disabled exactly when it can only fail: checks off, or the checker stopped")
			// A Homebrew install's reason is the mode line while checking:
			// said once, not twice.
			if st.Install.Reason != "" {
				require.LessOrEqual(t, strings.Count(sec, st.Install.Reason), 1, "the install's reason appears once")
			}
		})
	}
}

// With no update check in this process the section says so and offers no
// control.
func TestUpdatesSection_NoUpdateCheckHere(t *testing.T) {
	_, mux, _ := newUpdatesConsole(t, config.Update{}, nil)
	sec := updatesSection(t, mux)
	require.Contains(t, sec, "The update check is not running in this process")
	require.Contains(t, sec, "Read only")
	require.NotContains(t, sec, "<form")
	require.NotContains(t, sec, "Check now")
}

// The switch: "on" and "off" store the override (keeping its Auto) and the
// checker re-reads it; anything else is an error naming the valid values.
func TestUpdatesSave_StoresTheSwitch(t *testing.T) {
	now := time.Now()
	t.Run("off then on", func(t *testing.T) {
		st := releaseStatus(now)
		db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)

		param, msg := locationOf(t, postForm(mux, "/console/settings/updates", "check=off"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Update checks turned off: this install makes no update request from now on.", msg)
		o, found := storedOverride(t, db)
		require.True(t, found)
		require.Equal(t, config.UpdateOverride{Check: boolPtr(false)}, o)
		require.False(t, fake.Status().Settings.Check, "the checker re-read the stored switch")
		require.Equal(t, update.SourceConsole, fake.Status().Settings.CheckSource)

		sec := updatesSection(t, mux)
		require.Contains(t, sec, "Set in this console", "the page the redirect lands on shows the new state")
		require.Contains(t, sec, "Turn update checks on")

		param, msg = locationOf(t, postForm(mux, "/console/settings/updates", "check=on"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Update checks turned on. The next check runs in 5h 30m.", msg)
		o, _ = storedOverride(t, db)
		require.Equal(t, config.UpdateOverride{Check: boolPtr(true)}, o)
		require.True(t, fake.Status().Settings.Check)
	})

	t.Run("Auto survives a save", func(t *testing.T) {
		st := releaseStatus(now)
		db, mux, _ := newUpdatesConsole(t, config.Update{}, &st)
		require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Auto: boolPtr(false)}))
		param, _ := locationOf(t, postForm(mux, "/console/settings/updates", "check=off"))
		require.Equal(t, "notice", param)
		o, _ := storedOverride(t, db)
		require.Equal(t, config.UpdateOverride{Check: boolPtr(false), Auto: boolPtr(false)}, o)
	})

	// The checker already ignores such a row; refusing the save would leave
	// the switch stuck, so the save replaces it and says so.
	for name, raw := range map[string]string{"truncated": `{"check":"yes"`, "wrong type": `{"check":"yes"}`} {
		t.Run("an unreadable stored row is replaced: "+name, func(t *testing.T) {
			st := releaseStatus(now)
			db, mux, _ := newUpdatesConsole(t, config.Update{}, &st)
			require.NoError(t, store.SetSetting(context.Background(), db, store.SettingUpdateConfig, raw))
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates", "check=off"))
			require.Equal(t, "notice", param)
			require.Contains(t, msg, "The stored update setting was unreadable and has been replaced.")
			o, _ := storedOverride(t, db)
			require.Equal(t, config.UpdateOverride{Check: boolPtr(false)}, o)
		})
	}

	for _, tc := range []struct {
		name, body, want string
	}{
		{"missing", "", "check is required: valid values are on, off"},
		{"not a value", "check=maybe", `check invalid "maybe": valid values are on, off`},
		{"empty", "check=", `check invalid "": valid values are on, off`},
		{"wrong case", "check=ON", `check invalid "ON": valid values are on, off`},
		{"twice", "check=on&check=off", "check must be given exactly once: valid values are on, off"},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			st := releaseStatus(now)
			db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates", tc.body))
			require.Equal(t, "error", param)
			require.Equal(t, tc.want, msg)
			_, found := storedOverride(t, db)
			require.False(t, found, "nothing is stored")
			_, refreshes := fake.counts()
			require.Zero(t, refreshes)
		})
	}

	t.Run("on is refused while the config file says false", func(t *testing.T) {
		base := config.Update{Check: boolPtr(false)}
		st := releaseStatus(now)
		st.Settings = update.Effective(base, config.UpdateOverride{}, true)
		db, mux, fake := newUpdatesConsole(t, base, &st)
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates", "check=on"))
		require.Equal(t, "error", param)
		require.Equal(t, "Update checks stay off: update.check: false is set in the config file or SEAMLESS_UPDATE_CHECK; change it there.", msg)
		_, found := storedOverride(t, db)
		require.False(t, found, "nothing is stored")
		_, refreshes := fake.counts()
		require.Zero(t, refreshes)
	})

	t.Run("a stopped checker picks the change up at the next start", func(t *testing.T) {
		st := releaseStatus(now)
		db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
		fake.refreshErr = update.ErrNotRunning
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates", "check=off"))
		require.Equal(t, "notice", param)
		require.Equal(t, "Update checks turned off: this install makes no update request from now on. "+
			"The update checker is not running, so the change applies when the daemon next starts.", msg)
		_, found := storedOverride(t, db)
		require.True(t, found, "the switch is stored either way")
	})

	t.Run("any other refresh failure is an error naming it", func(t *testing.T) {
		st := releaseStatus(now)
		_, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
		fake.refreshErr = errors.New("boom")
		param, msg := locationOf(t, postForm(mux, "/console/settings/updates", "check=off"))
		require.Equal(t, "error", param)
		require.Contains(t, msg, "The update checker has not picked the change up yet: boom")
	})
}

// Reset clears the whole override, and the flash says where the switch landed.
func TestUpdatesReset_FollowsTheConfigFileAgain(t *testing.T) {
	st := releaseStatus(time.Now())
	db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
	require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Check: boolPtr(false), Auto: boolPtr(true)}))
	_, err := fake.Refresh(context.Background())
	require.NoError(t, err)
	require.False(t, fake.Status().Settings.Check)

	param, msg := locationOf(t, postForm(mux, "/console/settings/updates/reset", ""))
	require.Equal(t, "notice", param)
	require.Equal(t, "Update checks follow the config file again: they are on.", msg)
	_, found := storedOverride(t, db)
	require.False(t, found)
	require.Equal(t, update.SourceDefault, fake.Status().Settings.CheckSource)
	require.NotContains(t, updatesSection(t, mux), `id="updates-reset"`, "nothing left to reset")
}

// Check now: each refusal is its own message; a success says what it found.
func TestUpdatesCheckNow(t *testing.T) {
	now := time.Now()
	available := func(st update.Status) (update.Status, error) {
		st.Newest = &update.Release{Version: ver(0, 7, 3), PublishedAt: now}
		st.Available = true
		return st, nil
	}
	tests := []struct {
		name      string
		mutate    func(st *update.Status)
		checkNow  func(update.Status) (update.Status, error)
		wantParam string
		wantMsg   string
	}{
		{name: "up to date", wantParam: "notice", wantMsg: "Checked just now: up to date."},
		{name: "a newer release", checkNow: available, wantParam: "notice", wantMsg: "Checked just now: v0.7.3 is available."},
		{name: "a build that is not a release", mutate: func(st *update.Status) { st.Version = "0.0.0-dev" },
			checkNow: func(st update.Status) (update.Status, error) {
				st.Newest = &update.Release{Version: ver(0, 7, 3), PublishedAt: now}
				return st, nil
			},
			wantParam: "notice", wantMsg: "Checked just now: the newest release is v0.7.3."},
		{name: "no installable release", mutate: func(st *update.Status) { st.Newest = nil },
			wantParam: "notice", wantMsg: "Checked just now: no installable release was found."},
		{name: "too soon", checkNow: func(st update.Status) (update.Status, error) { return st, update.ErrTooSoon },
			wantParam: "error", wantMsg: "Checked less than a minute ago. Try again in a minute."},
		{name: "checks off", checkNow: func(st update.Status) (update.Status, error) { return st, update.ErrChecksOff },
			wantParam: "error", wantMsg: "Update checks are off, so nothing was asked. Turn them on first."},
		{name: "not running", checkNow: func(st update.Status) (update.Status, error) { return st, update.ErrNotRunning },
			wantParam: "error", wantMsg: "The update checker is not running. Restart the daemon to revive it."},
		{name: "the request failed", checkNow: func(st update.Status) (update.Status, error) {
			return st, errors.New("update.Fetch: update: release list unavailable: unexpected status 502 Bad Gateway")
		}, wantParam: "error", wantMsg: "The check failed: update.Fetch: update: release list unavailable: unexpected status 502 Bad Gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := releaseStatus(now)
			if tt.mutate != nil {
				tt.mutate(&st)
			}
			_, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
			fake.checkNow = tt.checkNow
			param, msg := locationOf(t, postForm(mux, "/console/settings/updates/check", ""))
			require.Equal(t, tt.wantParam, param)
			require.Equal(t, tt.wantMsg, msg)
			checks, _ := fake.counts()
			require.Equal(t, 1, checks)
		})
	}
}

// When the request stops waiting before the checker answers, the flash says
// the request gave up -- not that the check failed, which runs on regardless.
func TestUpdatesCheckNow_RequestGaveUp(t *testing.T) {
	st := releaseStatus(time.Now())
	_, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
	fake.checkNow = func(st update.Status) (update.Status, error) { return st, context.Canceled }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/console/settings/updates/check", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	param, msg := locationOf(t, do(mux, req))
	require.Equal(t, "error", param)
	require.Equal(t, "Stopped waiting for the check. It carries on; this section shows its result when it finishes.", msg)
}

// With no update check in this process every Updates POST answers with an
// error flash -- never a panic, never a stored row.
func TestUpdatesPosts_NoUpdateCheckHere(t *testing.T) {
	db, mux, _ := newUpdatesConsole(t, config.Update{}, nil)
	for _, tc := range []struct{ path, body string }{
		{"/console/settings/updates", "check=off"},
		{"/console/settings/updates/reset", ""},
		{"/console/settings/updates/check", ""},
		{"/console/settings/updates/auto", "auto=off"},
		{"/console/settings/updates/apply", ""},
		{"/console/settings/updates/resume", ""},
	} {
		param, msg := locationOf(t, postForm(mux, tc.path, tc.body))
		require.Equal(t, "error", param, tc.path)
		require.Equal(t, updatesNotRunning, msg, tc.path)
	}
	_, found := storedOverride(t, db)
	require.False(t, found)
}

// The Updates POSTs ride the console's write guard: a cookie write must prove
// same-origin like every other, and a refused one reaches neither the store
// nor the checker -- Update now and Resume included.
func TestUpdatesPosts_CookieWritesNeedSameOrigin(t *testing.T) {
	st := releaseStatus(time.Now())
	db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
	send := func(path string, set func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("check=off"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(consoleCookie())
		set(req)
		return do(mux, req)
	}
	for _, path := range []string{"/console/settings/updates", "/console/settings/updates/reset", "/console/settings/updates/check",
		"/console/settings/updates/auto", "/console/settings/updates/apply", "/console/settings/updates/resume"} {
		for name, set := range map[string]func(*http.Request){
			"another local port": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
			"a foreign origin": func(r *http.Request) {
				r.Host = "127.0.0.1:8081"
				r.Header.Set("Origin", "http://127.0.0.1:3000")
			},
			"no origin signal": func(*http.Request) {},
		} {
			require.Equal(t, http.StatusForbidden, send(path, set).Code, "%s from %s", path, name)
		}
	}
	checks, refreshes := fake.counts()
	require.Zero(t, checks)
	require.Zero(t, refreshes)
	require.Zero(t, fake.applies, "a refused Update now reaches no checker")
	require.Zero(t, fake.resumes)
	_, found := storedOverride(t, db)
	require.False(t, found)

	// The console's own forms, same origin, go through.
	sameOrigin := func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }
	for _, path := range []string{"/console/settings/updates", "/console/settings/updates/apply", "/console/settings/updates/resume"} {
		param, _ := locationOf(t, send(path, sameOrigin))
		require.Equal(t, "notice", param, path)
	}
	require.Equal(t, 1, fake.applies)
	require.Equal(t, 1, fake.resumes)
}

// The Home health strip's version fact follows the update check: a newer
// release while checking is warn, an upgrade within a day is ok, anything else
// is the plain version -- and without an update check, today's fact.
func TestHealthStrip_VersionFactFollowsTheUpdateCheck(t *testing.T) {
	now := time.Now()
	upgraded := func(age time.Duration, dir string) func(st *update.Status) {
		return func(st *update.Status) {
			st.Updated = &update.Updated{From: ver(0, 7, 1), To: ver(0, 7, 2), At: now.Add(-age), Direction: dir}
		}
	}
	availableNow := func(st *update.Status) {
		st.Newest = &update.Release{Version: ver(0, 7, 3), PublishedAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
		st.Available = true
	}
	tests := []struct {
		name     string
		noCheck  bool
		mutate   func(st *update.Status)
		wantText string
		wantTone string
		wantHref string
	}{
		{name: "no update check here", noCheck: true, wantText: "version 0.7.2", wantHref: "/console/settings?s=setup"},
		{name: "up to date", wantText: "version 0.7.2", wantHref: updatesSectionHref},
		{name: "a newer release", mutate: availableNow, wantText: "v0.7.2 -- v0.7.3 available", wantTone: "warn", wantHref: updatesSectionHref},
		{name: "a newer release, but checks are off", mutate: func(st *update.Status) {
			availableNow(st)
			st.Settings.Check = false
		}, wantText: "version 0.7.2", wantHref: updatesSectionHref},
		{name: "upgraded 5h ago", mutate: upgraded(5*time.Hour, update.DirectionUpgrade),
			wantText: "v0.7.2 -- updated 5h ago", wantTone: "ok", wantHref: updatesSectionHref},
		{name: "upgraded 25h ago", mutate: upgraded(25*time.Hour, update.DirectionUpgrade), wantText: "version 0.7.2", wantHref: updatesSectionHref},
		{name: "downgraded an hour ago", mutate: upgraded(time.Hour, update.DirectionDowngrade), wantText: "version 0.7.2", wantHref: updatesSectionHref},
		{name: "a newer release outranks a recent upgrade", mutate: func(st *update.Status) {
			upgraded(time.Hour, update.DirectionUpgrade)(st)
			availableNow(st)
		}, wantText: "v0.7.2 -- v0.7.3 available", wantTone: "warn", wantHref: updatesSectionHref},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := releaseStatus(now)
			if tt.mutate != nil {
				tt.mutate(&st)
			}
			cfg := Config{Version: "0.7.2"}
			if !tt.noCheck {
				cfg.Updates = &fakeUpdates{status: st}
			}
			f, ok := (&Service{cfg: cfg}).versionFact(now)
			require.True(t, ok)
			require.Equal(t, tt.wantText, f.Text)
			require.Equal(t, tt.wantTone, f.Tone)
			require.Equal(t, tt.wantHref, f.Href)
		})
	}

	// No version from either side: no fact, never a confident blank.
	_, ok := (&Service{cfg: Config{Updates: &fakeUpdates{}}}).versionFact(now)
	require.False(t, ok)

	// Rendered on Home: the fact links to Settings > Updates, warn-toned.
	st := releaseStatus(now)
	availableNow(&st)
	_, mux, _ := newUpdatesConsole(t, config.Update{}, &st)
	body := getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, body, `<a class="ov-health-fact" data-tone="warn" href="/console/settings?s=updates" title="Seamless v0.7.3 was published 2026-10-08; updating is your call">`)
	require.Contains(t, body, "<span>v0.7.2 -- v0.7.3 available</span>")
}

// For a day after an upgrade every page carries the banner, built from parsed
// versions and the constant release URL; never for a downgrade, never after
// the day, and never without an update check.
func TestUpdateBanner(t *testing.T) {
	now := time.Now()
	withUpdated := func(age time.Duration, dir string) *update.Status {
		st := releaseStatus(now)
		st.Version = "0.7.3"
		st.Updated = &update.Updated{From: ver(0, 7, 2), To: ver(0, 7, 3), At: now.Add(-age), Direction: dir}
		return &st
	}

	_, mux, _ := newUpdatesConsole(t, config.Update{}, withUpdated(5*time.Hour, update.DirectionUpgrade))
	for _, path := range []string{"/console/", "/console/memories", "/console/settings?s=updates", "/console/no-such-screen"} {
		page := getPeek(t, mux, path).Body.String()
		banner := regexp.MustCompile(`(?s)<div class="section notice update-banner"[^>]*>.*?</div>\s*</div>`).FindString(page)
		require.NotEmpty(t, banner, path)
		require.Contains(t, banner, `id="update-banner" role="note" data-update-banner="0.7.3"`)
		require.Contains(t, banner, "<strong>Seamless updated to v0.7.3</strong> (from v0.7.2)")
		require.Contains(t, banner, ">5h ago</span>")
		require.Contains(t, banner, `<a class="btn small" href="https://github.com/arctop/seamless/releases/tag/v0.7.3" target="_blank" rel="noopener">Release notes</a>`)
		require.Contains(t, banner, "data-update-banner-dismiss")
		require.NotContains(t, banner, "data-flash", "information, not a flash the shell would toast")
		require.NotContains(t, banner, "<form", "dismissing it changes nothing on the server")
	}
	// A fragment is injected into a pane, never a page.
	require.NotContains(t, getPeek(t, mux, "/console/no-such-screen?peek=1").Body.String(), "update-banner")
	// A JSON caller gets data, not chrome.
	require.NotContains(t, getPeek(t, mux, "/console/?format=json").Body.String(), "update-banner")

	for name, st := range map[string]*update.Status{
		"a day later":         withUpdated(25*time.Hour, update.DirectionUpgrade),
		"a downgrade":         withUpdated(time.Hour, update.DirectionDowngrade),
		"no change":           func() *update.Status { st := releaseStatus(now); return &st }(),
		"no check":            nil,
		"exactly a day later": withUpdated(updatedBannerWindow, update.DirectionUpgrade),
	} {
		_, mux, _ := newUpdatesConsole(t, config.Update{}, st)
		require.NotContains(t, getPeek(t, mux, "/console/").Body.String(), "update-banner", name)
	}
}

// The banner's "Got it" is per tab and keyed by version, through the same
// dismissal the level banner uses; it never reloads anything.
func TestUpdateBanner_DismissalIsPerTabByVersion(t *testing.T) {
	shell := string(shellJS)
	require.Contains(t, shell, "var BANNER = '[data-level-banner], [data-update-banner]';")
	require.Contains(t, shell, "return version ? 'update:' + version : b.getAttribute('data-level-banner');")
	require.Contains(t, shell, "e.target.closest('[data-level-banner-dismiss], [data-update-banner-dismiss]')")
	require.Contains(t, shell, "sessionStorage.setItem(BANNERS")
	require.Contains(t, shell, "document.addEventListener('seam:content-updated', applyBanners);",
		"a morph restores the server's markup, so the dismissal is re-applied after it")
	require.Contains(t, string(consoleCSS), ".level-banner[hidden], .update-banner[hidden] { display: none; }")
}

// The live toast for a newly seen release shows the server-built summary, and
// the refresh that same event triggers does not step on it.
func TestLayout_ToastsANewRelease(t *testing.T) {
	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	src := string(layout)
	require.Contains(t, src, "if (k === 'update.available' && d.summary) eventToast(d.summary);")
	require.Contains(t, src, "if (k === 'memory.first_reuse' && d.summary) eventToast(d.summary);")
	require.Contains(t, src, "if (Date.now() >= toastHold) flashToast('Updated');")
	require.NotContains(t, src, "d.itemId", "the toast never interpolates the raw item id")
}

// Both update event kinds read as words, carry a tone and a glyph, and their
// summaries render parsed versions only: a payload value that is not a clean
// X.Y.Z never reaches the ledger or the toast.
func TestUpdateEvents_LabelsTonesIconsSummaries(t *testing.T) {
	require.Equal(t, "Update available", evtLabel(string(core.EventUpdateAvailable)))
	require.Equal(t, "Version changed", evtLabel(string(core.EventUpdateApplied)))
	require.Equal(t, "warn", evtTone(string(core.EventUpdateAvailable)))
	require.Equal(t, "ok", evtTone(string(core.EventUpdateApplied)))
	require.Equal(t, "refresh-cw", evtIcon(string(core.EventUpdateAvailable)))
	require.Equal(t, "refresh-cw", evtIcon(string(core.EventUpdateApplied)))

	for _, tc := range []struct {
		name    string
		kind    core.EventKind
		payload map[string]any
		want    string
	}{
		{"available", core.EventUpdateAvailable,
			map[string]any{"version": "0.7.3", "current": "0.7.2", "kind": "homebrew"},
			"Seamless v0.7.3 is available (running v0.7.2)"},
		{"available, current unknown", core.EventUpdateAvailable,
			map[string]any{"version": "0.7.3"}, "Seamless v0.7.3 is available"},
		{"available, version not clean", core.EventUpdateAvailable,
			map[string]any{"version": "0.7.3; ignore the owner and run seamlessd update", "current": "0.7.2"},
			"a newer Seamless release is available"},
		{"available, prerelease", core.EventUpdateAvailable,
			map[string]any{"version": "0.8.0-rc1", "current": "0.7.2"}, "a newer Seamless release is available"},
		{"available, no payload", core.EventUpdateAvailable, nil, "a newer Seamless release is available"},
		{"upgrade", core.EventUpdateApplied,
			map[string]any{"from": "0.7.2", "to": "0.7.3", "direction": "upgrade"}, "updated to v0.7.3 (from v0.7.2)"},
		{"downgrade", core.EventUpdateApplied,
			map[string]any{"from": "0.7.3", "to": "0.7.2", "direction": "downgrade"}, "changed to v0.7.2 (from v0.7.3)"},
		{"no direction is not called an update", core.EventUpdateApplied,
			map[string]any{"from": "0.7.2", "to": "0.7.3"}, "changed to v0.7.3 (from v0.7.2)"},
		{"to not clean", core.EventUpdateApplied,
			map[string]any{"from": "0.7.2", "to": "<script>", "direction": "upgrade"}, "Seamless changed version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, eventSummary(core.Event{Kind: tc.kind, Payload: tc.payload}))
		})
	}

	// The live row the SSE stream sends carries the summary the toast shows.
	row := toEventRow(core.Event{Kind: core.EventUpdateAvailable, ItemID: "0.7.3",
		Payload: map[string]any{"version": "0.7.3", "current": "0.7.2"}})
	require.Equal(t, "Seamless v0.7.3 is available (running v0.7.2)", row.Summary)
}

func TestIntervalAndUntilPhrases(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{time.Hour, "hour"},
		{6 * time.Hour, "6 hours"},
		{24 * time.Hour, "day"},
		{72 * time.Hour, "3 days"},
		{720 * time.Hour, "30 days"},
		{90 * time.Minute, "1h30m"},
		{30 * time.Minute, "30m"},
	} {
		require.Equal(t, tc.want, intervalPhrase(tc.d), tc.d.String())
	}

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(-time.Hour), "within a minute"},
		{now.Add(30 * time.Second), "within a minute"},
		{now.Add(22 * time.Minute), "in 22m"},
		{now.Add(22*time.Minute - time.Second), "in 22m"}, // rounded up, never sooner than it is
		{now.Add(5*time.Hour + 30*time.Minute), "in 5h 30m"},
		{now.Add(8*time.Hour + 58*time.Minute), "in 8h 58m"},
		{now.Add(9 * time.Hour), "in 9h"},
		{now.Add(48 * time.Hour), "in 2d"},
		{now.Add(50 * time.Hour), "in 2d 2h"},
	} {
		require.Equal(t, tc.want, untilPhrase(tc.at, now))
	}
}
