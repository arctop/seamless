package update

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/stretchr/testify/require"
)

// ---- fakes -----------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type fakeResp struct {
	page  Page
	err   error
	panic bool
}

type fakeFetcher struct {
	mu      sync.Mutex
	etags   []string
	resps   []fakeResp
	onFetch func()
}

func (f *fakeFetcher) push(r ...fakeResp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resps = append(f.resps, r...)
}

func (f *fakeFetcher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.etags)
}

func (f *fakeFetcher) Fetch(_ context.Context, etag string) (Page, error) {
	f.mu.Lock()
	f.etags = append(f.etags, etag)
	var r fakeResp
	if len(f.resps) > 0 {
		r, f.resps = f.resps[0], f.resps[1:]
	} else {
		r = fakeResp{err: ErrUnavailable}
	}
	on := f.onFetch
	f.mu.Unlock()
	if on != nil {
		on()
	}
	if r.panic {
		panic("fetcher exploded")
	}
	return r.page, r.err
}

type fakeRecorder struct {
	mu     sync.Mutex
	events []core.Event
}

func (r *fakeRecorder) Record(_ context.Context, e core.Event) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return "id", nil
}

// RecordOnce keeps the real recorder's latch: once per kind + item, ever.
func (r *fakeRecorder) RecordOnce(_ context.Context, e core.Event) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.events {
		if x.Kind == e.Kind && x.ItemID == e.ItemID {
			return "", false, nil
		}
	}
	r.events = append(r.events, e)
	return "id", true, nil
}

func (r *fakeRecorder) of(k core.EventKind) []core.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.Event
	for _, e := range r.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// ---- harness ---------------------------------------------------------------

var t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

type harness struct {
	t      *testing.T
	clock  *fakeClock
	ticks  chan time.Time
	fetch  *fakeFetcher
	rec    *fakeRecorder
	dir    string
	deps   Deps
	c      *Checker
	ctx    context.Context
	cancel context.CancelFunc

	// polls is the loop's poll channel, and polling counts the poll tickers
	// it runs: 1 while an update is under way, 0 otherwise.
	polls   chan time.Time
	polling atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		clock: &fakeClock{t: t0},
		ticks: make(chan time.Time),
		polls: make(chan time.Time),
		fetch: &fakeFetcher{},
		rec:   &fakeRecorder{},
		dir:   t.TempDir(),
	}
	h.deps = Deps{
		DataDir:      h.dir,
		Version:      "0.7.2",
		Distribution: DistributionRelease,
		GOOS:         "linux",
		Install:      Install{Kind: KindInstaller, Hint: Hint(KindInstaller)},
		Instance:     "01INSTANCE",
		PID:          4242,
		LocalHost:    "studio",
		Base:         config.Update{CheckInterval: config.Duration(6 * time.Hour)},
		Fetcher:      h.fetch,
		Events:       h.rec,
		Now:          h.clock.Now,
		Ticks:        func() (<-chan time.Time, func()) { return h.ticks, func() {} },
		Polls: func() (<-chan time.Time, func()) {
			h.polling.Add(1)
			return h.polls, func() { h.polling.Add(-1) }
		},
		Rand: func(int64) int64 { return 0 },
	}
	return h
}

func (h *harness) start() {
	h.t.Helper()
	h.c = New(h.deps)
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.c.Start(h.ctx)
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		h.c.Wait()
	}
}

// tick delivers one tick and waits until the loop has fully handled it: the
// tick channel is unbuffered, and the Refresh after it can only be served once
// the loop is back at its select.
func (h *harness) tick() Status {
	h.t.Helper()
	h.ticks <- h.clock.Now()
	st, err := h.c.Refresh(context.Background())
	require.NoError(h.t, err)
	return st
}

func (h *harness) state() State {
	h.t.Helper()
	s, err := ReadState(h.dir)
	require.NoError(h.t, err)
	return s
}

func releasePage(etag string, tags ...string) Page {
	var rels []APIRelease
	for _, tag := range tags {
		rels = append(rels, APIRelease{TagName: tag, PublishedAt: at("2026-10-08T00:00:00Z"), Assets: allAssets()})
	}
	return Page{Releases: rels, ETag: etag, ServerDate: t0}
}

// ---- tests -----------------------------------------------------------------

func TestChecker_FirstCheckIsSoonNotImmediate(t *testing.T) {
	h := newHarness(t)
	h.deps.Rand = func(n int64) int64 { return n / 2 } // 4m of the 8m spread
	h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")})
	h.start()

	st := h.c.Status()
	require.Equal(t, t0.Add(firstCheckDelay+firstCheckJitter/2), st.NextCheckAt)

	h.clock.Add(5 * time.Minute)
	h.tick()
	require.Zero(t, h.fetch.calls(), "not due yet")

	h.clock.Add(time.Minute)
	st = h.tick()
	require.Equal(t, 1, h.fetch.calls())
	require.NotNil(t, st.Newest)
	require.Equal(t, "0.7.3", st.Newest.Version.String())
	require.True(t, st.Available)
}

func TestChecker_PersistsNextCheckBeforeFetching(t *testing.T) {
	h := newHarness(t)
	var during State
	h.fetch.onFetch = func() {
		s, err := ReadState(h.dir)
		require.NoError(t, err)
		during = s
	}
	h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")})
	h.start()
	h.clock.Add(10 * time.Minute)
	h.tick()

	// While the request was in flight, the file already said "not before
	// another 15 minutes": a crash here cannot make the next start ask again.
	require.Equal(t, h.clock.Now().Add(errorBackoffBase), during.NextCheckAt)
	// After success the schedule is one (jittered) interval out.
	require.Equal(t, h.clock.Now().Add(6*time.Hour-6*time.Hour/10), h.state().NextCheckAt)
}

func TestChecker_KeepsASavedScheduleAcrossRestart(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, SaveState(h.dir, State{NextCheckAt: t0.Add(time.Hour)}))
	h.start()
	require.Equal(t, t0.Add(time.Hour), h.c.Status().NextCheckAt)

	h.clock.Add(30 * time.Minute)
	h.tick()
	require.Zero(t, h.fetch.calls(), "a restart does not jump the saved schedule")
}

func TestChecker_FarFutureScheduleIsRescheduled(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, SaveState(h.dir, State{NextCheckAt: t0.Add(30 * 24 * time.Hour)}))
	h.start()
	require.Equal(t, t0.Add(firstCheckDelay), h.c.Status().NextCheckAt)
}

func TestChecker_SourceBuildNeverWritesState(t *testing.T) {
	h := newHarness(t)
	h.deps.Distribution = DistributionSource
	h.start()

	st := h.c.Status()
	require.False(t, st.Settings.Check, "a source build does not check unless told to")
	require.False(t, st.Release)
	h.clock.Add(24 * time.Hour)
	h.tick()
	require.Zero(t, h.fetch.calls())
	_, err := os.Stat(StateDir(h.dir))
	require.True(t, os.IsNotExist(err), "a source build never creates the update dir")
	require.Empty(t, h.rec.events)
}

func TestChecker_SourceBuildWithCheckOnKeepsItInMemory(t *testing.T) {
	h := newHarness(t)
	h.deps.Distribution = DistributionSource
	yes := true
	h.deps.Base.Check = &yes
	h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")})
	h.start()
	h.clock.Add(time.Hour)
	st := h.tick()
	require.Equal(t, 1, h.fetch.calls())
	require.NotNil(t, st.Newest)
	_, err := os.Stat(StateDir(h.dir))
	require.True(t, os.IsNotExist(err))
}

func TestChecker_AvailableIsRecordedOncePerVersion(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(
		fakeResp{page: releasePage(`W/"1"`, "v0.7.3")},
		fakeResp{page: Page{NotModified: true, ETag: `W/"1"`}},
		fakeResp{page: releasePage(`W/"2"`, "v0.7.4", "v0.7.3")},
	)
	h.start()
	for range 3 {
		h.clock.Add(7 * time.Hour)
		h.tick()
	}
	require.Equal(t, 3, h.fetch.calls())
	got := h.rec.of(core.EventUpdateAvailable)
	require.Len(t, got, 2)
	require.Equal(t, "0.7.3", got[0].ItemID)
	require.Equal(t, "0.7.4", got[1].ItemID)
	require.Equal(t, map[string]any{"version": "0.7.4", "current": "0.7.2", "kind": "installer"}, got[1].Payload)
}

func TestChecker_NotModifiedReusesTheCachedList(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(
		fakeResp{page: releasePage(`W/"1"`, "v0.7.3")},
		fakeResp{page: Page{NotModified: true}},
	)
	h.start()
	h.clock.Add(time.Hour)
	h.tick()
	h.clock.Add(7 * time.Hour)
	st := h.tick()

	h.fetch.mu.Lock()
	etags := append([]string(nil), h.fetch.etags...)
	h.fetch.mu.Unlock()
	require.Equal(t, []string{"", `W/"1"`}, etags)
	require.NotNil(t, st.Newest)
	require.Equal(t, "0.7.3", st.Newest.Version.String())
	require.Equal(t, `W/"1"`, h.state().ETag)
}

func TestChecker_ErrorsBackOff(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(
		fakeResp{err: ErrUnavailable},
		fakeResp{err: ErrUnavailable},
		fakeResp{err: ErrUnavailable},
		fakeResp{page: releasePage(`W/"1"`, "v0.7.3")},
	)
	h.start()
	h.clock.Add(time.Hour)

	for _, want := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour} {
		st := h.tick()
		require.NotNil(t, st.CheckError)
		require.Equal(t, CheckErrorUnavailable, st.CheckError.Kind)
		require.Equal(t, h.clock.Now().Add(want), st.NextCheckAt)
		h.clock.Add(want)
	}
	st := h.tick()
	require.Nil(t, st.CheckError, "a success clears the streak")
	require.Equal(t, 4, h.fetch.calls())
}

func TestChecker_BackoffIsCappedAtTheInterval(t *testing.T) {
	h := newHarness(t)
	h.deps.Base.CheckInterval = config.Duration(time.Hour)
	for range 6 {
		h.fetch.push(fakeResp{err: ErrUnavailable})
	}
	h.start()
	h.clock.Add(time.Hour)
	var (
		st    Status
		waits []time.Duration
	)
	for range 6 {
		st = h.tick()
		wait := st.NextCheckAt.Sub(h.clock.Now())
		waits = append(waits, wait)
		h.clock.Add(wait)
	}
	require.Equal(t, 6, st.CheckError.Count)
	require.Equal(t, []time.Duration{
		15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour, time.Hour, time.Hour,
	}, waits, "doubling from 15m, capped at the 1h interval")
}

func TestChecker_RateLimitWaitsForRetryAt(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.clock.Add(time.Hour)
	retry := h.clock.Now().Add(3 * time.Hour)
	h.fetch.push(fakeResp{err: &RateLimitError{RetryAt: retry, Status: "403 Forbidden"}})
	st := h.tick()
	require.Equal(t, retry, st.NextCheckAt)
	require.Equal(t, CheckErrorRateLimited, st.CheckError.Kind)
}

func TestChecker_IntervalJitter(t *testing.T) {
	for name, tc := range map[string]struct {
		rand func(int64) int64
		want time.Duration
	}{
		"low end":  {func(int64) int64 { return 0 }, 6*time.Hour - 36*time.Minute},
		"high end": {func(n int64) int64 { return n - 1 }, 6*time.Hour + 36*time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.Rand = tc.rand
			h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")})
			h.start()
			h.clock.Add(time.Hour)
			st := h.tick()
			require.Equal(t, h.clock.Now().Add(tc.want), st.NextCheckAt)
		})
	}
}

func TestChecker_RecordsAVersionChangeOnce(t *testing.T) {
	h := newHarness(t)
	prev := Version{0, 7, 1}
	require.NoError(t, SaveState(h.dir, State{LastRunVersion: &prev}))
	h.start()

	st := h.c.Status()
	require.NotNil(t, st.Updated)
	require.Equal(t, DirectionUpgrade, st.Updated.Direction)
	require.Equal(t, "0.7.1", st.Updated.From.String())
	require.Equal(t, "0.7.2", st.Updated.To.String())
	applied := h.rec.of(core.EventUpdateApplied)
	require.Len(t, applied, 1)
	require.Equal(t, map[string]any{"from": "0.7.1", "to": "0.7.2", "direction": "upgrade"}, applied[0].Payload)

	saved := h.state()
	require.Equal(t, "0.7.2", saved.LastRunVersion.String())
	require.Equal(t, "01INSTANCE", saved.Running.Instance)
	require.Equal(t, 4242, saved.Running.PID)
	require.Equal(t, KindInstaller, saved.Running.Kind)

	// A restart on the same version records nothing new.
	h.stop()
	h.rec = &fakeRecorder{}
	h.deps.Events = h.rec
	h.deps.Instance = "01SECOND"
	h.start()
	require.Empty(t, h.rec.of(core.EventUpdateApplied))
	require.Equal(t, "01SECOND", h.state().Running.Instance)
	require.NotNil(t, h.c.Status().Updated, "the last change is still known for the notice window")
}

func TestChecker_RecordsADowngrade(t *testing.T) {
	h := newHarness(t)
	prev := Version{0, 7, 4}
	require.NoError(t, SaveState(h.dir, State{LastRunVersion: &prev}))
	h.start()
	st := h.c.Status()
	require.Equal(t, DirectionDowngrade, st.Updated.Direction)
	require.Empty(t, st.Notice("", "studio", t0), "a downgrade is deliberate; nothing to announce")
}

func TestChecker_FirstRunRecordsNoChange(t *testing.T) {
	h := newHarness(t)
	h.start()
	require.Nil(t, h.c.Status().Updated)
	require.Empty(t, h.rec.events)
	require.Equal(t, "0.7.2", h.state().LastRunVersion.String())
}

func TestChecker_CorruptStateIsQuarantined(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, os.MkdirAll(StateDir(h.dir), 0o700))
	require.NoError(t, os.WriteFile(StatePath(h.dir), []byte("{garbage"), 0o600))
	h.start()
	require.Empty(t, h.rec.events, "no version change can be told from a corrupt file")
	entries, err := os.ReadDir(StateDir(h.dir))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.Contains(t, names, "state.json.corrupt-20261009T120000Z")
	require.Contains(t, names, "state.json")
}

func TestChecker_CheckNow(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(
		fakeResp{page: releasePage(`W/"1"`, "v0.7.3")},
		fakeResp{err: ErrUnavailable},
	)
	h.start()

	st, err := h.c.CheckNow(context.Background())
	require.NoError(t, err)
	require.True(t, st.Available)
	require.Equal(t, 1, h.fetch.calls())

	h.clock.Add(30 * time.Second)
	_, err = h.c.CheckNow(context.Background())
	require.ErrorIs(t, err, ErrTooSoon)
	require.Equal(t, 1, h.fetch.calls())

	h.clock.Add(time.Minute)
	st, err = h.c.CheckNow(context.Background())
	require.ErrorIs(t, err, ErrUnavailable, "a failed check is reported to the asker")
	require.NotNil(t, st.CheckError)
}

func TestChecker_CheckNowRefusedWhenOff(t *testing.T) {
	h := newHarness(t)
	no := false
	h.deps.Base.Check = &no
	h.start()
	st, err := h.c.CheckNow(context.Background())
	require.ErrorIs(t, err, ErrChecksOff)
	require.True(t, st.Settings.CheckLocked)
	require.Zero(t, h.fetch.calls())
}

func TestChecker_ConsoleOverrideIsReadLive(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var override config.UpdateOverride
	h.deps.Override = func(context.Context) (config.UpdateOverride, error) {
		mu.Lock()
		defer mu.Unlock()
		return override, nil
	}
	h.start()
	require.True(t, h.c.Status().Settings.Check)

	no := false
	mu.Lock()
	override.Check = &no
	mu.Unlock()
	st, err := h.c.Refresh(context.Background())
	require.NoError(t, err)
	require.False(t, st.Settings.Check)
	require.Equal(t, SourceConsole, st.Settings.CheckSource)

	h.clock.Add(24 * time.Hour)
	h.tick()
	require.Zero(t, h.fetch.calls(), "turned off from the console means no request")
}

func TestChecker_UnreadableOverrideFallsBackToConfig(t *testing.T) {
	h := newHarness(t)
	h.deps.Override = func(context.Context) (config.UpdateOverride, error) {
		return config.UpdateOverride{}, errors.New("database is locked")
	}
	h.start()
	st := h.c.Status()
	require.True(t, st.Settings.Check)
	require.Equal(t, SourceDefault, st.Settings.CheckSource)
}

func TestChecker_PanicStopsTheLoopNotTheDaemon(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(fakeResp{panic: true})
	h.start()
	h.clock.Add(time.Hour)
	h.ticks <- h.clock.Now()
	h.c.Wait() // returns: the loop is gone

	st := h.c.Status()
	require.True(t, st.Stopped)
	_, err := h.c.CheckNow(context.Background())
	require.ErrorIs(t, err, ErrNotRunning)
}

func TestChecker_WaitWithoutStart(t *testing.T) {
	c := New(Deps{Version: "0.7.2", Distribution: DistributionRelease})
	c.Wait() // must not block
	_, err := c.CheckNow(context.Background())
	require.ErrorIs(t, err, ErrNotRunning)
}

func TestChecker_NoticeAdapter(t *testing.T) {
	h := newHarness(t)
	h.deps.Install = Install{Kind: KindHomebrew, Hint: Hint(KindHomebrew)}
	h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")})
	h.start()
	h.clock.Add(time.Hour)
	h.tick()
	require.Contains(t, h.c.Notice(context.Background(), "studio"), "Seamless v0.7.3 is available (running v0.7.2)")
	require.Contains(t, h.c.Notice(context.Background(), "laptop"), "The Seamless server can update to v0.7.3")
}

func TestChecker_StatusIsACopy(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")})
	h.start()
	h.clock.Add(time.Hour)
	st := h.tick()
	st.Newest.Version = Version{9, 9, 9}
	require.Equal(t, "0.7.3", h.c.Status().Newest.Version.String())
}

// TestChecker_KeepsWallTimeOnly: the checker's clock drops the monotonic
// reading, which stops while the machine sleeps; every deadline is wall time.
func TestChecker_KeepsWallTimeOnly(t *testing.T) {
	c := New(Deps{Version: "0.7.2", Distribution: DistributionRelease})
	now := c.d.Now()
	require.Equal(t, now.String(), now.Round(0).String(), "no monotonic reading")
	require.WithinDuration(t, time.Now(), now, time.Minute)
}

// TestChecker_AClockThatWentBackDoesNotStallTheSchedule: a clock that jumps
// back three days while the daemon runs leaves the next check three days out;
// the tick pulls it in.
func TestChecker_AClockThatWentBackDoesNotStallTheSchedule(t *testing.T) {
	h := newHarness(t)
	h.fetch.push(fakeResp{page: releasePage(`W/"1"`, "v0.7.3")}, fakeResp{page: Page{NotModified: true, ETag: `W/"1"`}})
	h.start()
	h.clock.Add(time.Hour)
	h.tick()
	require.Equal(t, 1, h.fetch.calls())

	h.clock.Add(-72 * time.Hour)
	h.tick()
	require.Equal(t, 2, h.fetch.calls(), "checked at once rather than in three days")

	// A rate limit's own longest wait is never mistaken for a wrong clock.
	h.clock.Add(7 * time.Hour)
	retry := h.clock.Now().Add(maxRetryWait)
	h.fetch.push(fakeResp{err: &RateLimitError{RetryAt: retry, Status: "403 Forbidden"}})
	h.tick()
	require.Equal(t, 3, h.fetch.calls())
	h.clock.Add(time.Minute)
	st := h.tick()
	require.Equal(t, retry, st.NextCheckAt)
	require.Equal(t, 3, h.fetch.calls())
}
