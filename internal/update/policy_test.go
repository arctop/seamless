package update

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rel is a filtered release published at pub, with or without the checksums
// bundle.
func rel(v string, pub time.Time, bundle bool) Release {
	ver, ok := Parse(v)
	if !ok {
		panic("bad version " + v)
	}
	return Release{Version: ver, PublishedAt: pub, ChecksumsBundle: bundle}
}

func ver(v string) Version {
	out, ok := Parse(v)
	if !ok {
		panic("bad version " + v)
	}
	return out
}

func TestTarget(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-3 * time.Hour)
	tests := []struct {
		name   string
		rels   []Release
		cur    string
		clock  time.Time
		minAge time.Duration
		blocks []Block
		hold   *Hold
		want   string // "" for no target
	}{
		{"the newest soaked release", []Release{rel("0.7.3", old, true), rel("0.7.4", old, true)}, "0.7.2", now, day, nil, nil, "0.7.4"},
		{"nothing above current", []Release{rel("0.7.2", old, true), rel("0.7.1", old, true)}, "0.7.2", now, day, nil, nil, ""},
		{"a newer release still soaking leaves the older one", []Release{rel("0.7.4", fresh, true), rel("0.7.3", old, true)}, "0.7.2", now, day, nil, nil, "0.7.3"},
		{"only soaking releases", []Release{rel("0.7.3", fresh, true)}, "0.7.2", now, day, nil, nil, ""},
		{"soaked exactly at min_age", []Release{rel("0.7.3", now.Add(-day), true)}, "0.7.2", now, day, nil, nil, "0.7.3"},
		{"no soak takes the newest at once", []Release{rel("0.7.4", now, true), rel("0.7.3", old, true)}, "0.7.2", now, 0, nil, nil, "0.7.4"},
		{"an unknown clock soaks nothing", []Release{rel("0.7.3", old, true)}, "0.7.2", time.Time{}, day, nil, nil, ""},
		{"an unknown clock with no soak", []Release{rel("0.7.3", old, true)}, "0.7.2", time.Time{}, 0, nil, nil, "0.7.3"},
		{"no checksums bundle is skipped", []Release{rel("0.7.4", old, false), rel("0.7.3", old, true)}, "0.7.2", now, day, nil, nil, "0.7.3"},
		{"a blocked newest falls back to the next", []Release{rel("0.7.4", old, true), rel("0.7.3", old, true)}, "0.7.2", now, day,
			[]Block{{Version: ver("0.7.4"), Reason: BlockRolledBack}}, nil, "0.7.3"},
		{"a blocked older one does not hide a newer one", []Release{rel("0.7.4", old, true), rel("0.7.3", old, true)}, "0.7.2", now, day,
			[]Block{{Version: ver("0.7.3"), Reason: BlockRolledBack}}, nil, "0.7.4"},
		{"a hold skips everything up to it", []Release{rel("0.7.5", old, true), rel("0.7.4", old, true)}, "0.7.2", now, day, nil,
			&Hold{Through: ver("0.7.5"), From: ver("0.7.5")}, ""},
		{"a release past the hold is a candidate", []Release{rel("0.7.6", old, true), rel("0.7.5", old, true)}, "0.7.2", now, day, nil,
			&Hold{Through: ver("0.7.5"), From: ver("0.7.5")}, "0.7.6"},
		{"never a downgrade", []Release{rel("0.7.1", old, true)}, "0.7.2", now, 0, nil, nil, ""},
		{"no releases", nil, "0.7.2", now, day, nil, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Target(tt.rels, ver(tt.cur), tt.clock, tt.minAge, tt.blocks, tt.hold)
			if tt.want == "" {
				require.False(t, ok, "got %s", got.Version)
				return
			}
			require.True(t, ok)
			require.Equal(t, tt.want, got.Version.String())
		})
	}
}

// TestTarget_FromTheReleaseList runs GitHub's list through Filter and Target
// together: drafts, rc tags, uploads still open and a per-OS asset missing
// never become a target, a release without the checksums bundle is told about
// but not installed, and a backport listed first does not outrank a newer
// version.
func TestTarget_FromTheReleaseList(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour)
	withBundle := append(allAssets(), APIAsset{Name: checksumsBundleAsset, State: "uploaded"})
	posixOnly := append(append([]APIAsset{}, posixAssets...), APIAsset{Name: checksumsBundleAsset, State: "uploaded"})
	list := []APIRelease{
		{TagName: "v0.6.9", PublishedAt: &old, Assets: withBundle}, // a backport, created last
		{TagName: "v0.7.5", Draft: true, PublishedAt: &old, Assets: withBundle},
		{TagName: "v0.7.4-rc1", PublishedAt: &old, Assets: withBundle},
		{TagName: "v0.7.4", PublishedAt: &old, Assets: append(append([]APIAsset{}, posixAssets...),
			APIAsset{Name: checksumsBundleAsset, State: "open"})}, // bundle still uploading
		{TagName: "v0.7.3", PublishedAt: &old, Assets: posixOnly},
		{TagName: "v0.7.2", PublishedAt: &old, Assets: allAssets()}, // no bundle: up to v0.6.0's kind
	}

	linux := Filter(list, "linux")
	require.Equal(t, []string{"0.7.4", "0.7.3", "0.7.2", "0.6.9"}, versions(linux))
	newest, _ := Newest(linux)
	require.Equal(t, "0.7.4", newest.Version.String(), "told about, though not installable unattended")
	require.False(t, newest.ChecksumsBundle, "an open upload is not an uploaded bundle")

	got, ok := Target(linux, ver("0.6.9"), now, 24*time.Hour, nil, nil)
	require.True(t, ok)
	require.Equal(t, "0.7.3", got.Version.String())
	_, ok = Target(linux, ver("0.7.3"), now, 24*time.Hour, nil, nil)
	require.False(t, ok, "v0.7.4 has no uploaded bundle; v0.7.2 is older")

	// Windows needs the ps1 pair, which v0.7.3 lacks here.
	windows := Filter(list, "windows")
	require.Equal(t, []string{"0.7.2", "0.6.9"}, versions(windows))
	_, ok = Target(windows, ver("0.6.9"), now, 24*time.Hour, nil, nil)
	require.False(t, ok, "v0.7.2 carries no bundle")
	got, ok = Target(windows, ver("0.6.8"), now, 24*time.Hour, nil, nil)
	require.True(t, ok)
	require.Equal(t, "0.6.9", got.Version.String(), "the backport is a fine target for an install below it")
}

// TestTarget_ReleaseCadence is October 2026's cadence -- releases three to
// four hours apart -- at no soak and the default 24h soak, measured on
// GitHub's clock: the local clock running two days ahead changes nothing.
// At each moment the list holds what GitHub had published by then.
func TestTarget_ReleaseCadence(t *testing.T) {
	base := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	offsets := []time.Duration{0, 3 * time.Hour, 7 * time.Hour, 10*time.Hour + 30*time.Minute, 14 * time.Hour, 17*time.Hour + 30*time.Minute}
	var all []Release
	for i, off := range offsets {
		all = append(all, rel(fmt.Sprintf("0.7.%d", i), base.Add(off), true))
	}
	listedAt := func(server time.Time) []Release {
		var out []Release
		for _, r := range all {
			if !r.PublishedAt.After(server) {
				out = append(out, r)
			}
		}
		return out
	}
	cur := ver("0.6.9")

	// The local clock is two days ahead of GitHub's; the check saw GitHub's
	// Date, and serverClock extrapolates from it, never from the local time.
	localAhead := 48 * time.Hour
	at := func(sinceBase time.Duration) time.Time {
		checkedAt := base.Add(sinceBase).Add(localAhead) // local time of the check
		clock, ok := serverClock(base.Add(sinceBase), checkedAt, checkedAt.Add(time.Minute), scheduleSlack(6*time.Hour))
		require.True(t, ok)
		return clock
	}

	for _, tc := range []struct {
		sinceBase time.Duration
		soak      string // the target with the 24h soak, "" for none
		noSoak    string
	}{
		{time.Hour, "", "0.7.0"},
		{18 * time.Hour, "", "0.7.5"},
		{24 * time.Hour, "0.7.0", "0.7.5"},
		{27*time.Hour + time.Minute, "0.7.1", "0.7.5"},
		{31 * time.Hour, "0.7.2", "0.7.5"},
		{38 * time.Hour, "0.7.4", "0.7.5"},
		{41*time.Hour + 30*time.Minute, "0.7.5", "0.7.5"},
	} {
		t.Run(fmt.Sprint(tc.sinceBase), func(t *testing.T) {
			clock := at(tc.sinceBase)
			rels := listedAt(base.Add(tc.sinceBase))
			got, ok := Target(rels, cur, clock, 24*time.Hour, nil, nil)
			if tc.soak == "" {
				require.False(t, ok)
			} else {
				require.True(t, ok)
				require.Equal(t, tc.soak, got.Version.String())
			}
			got, ok = Target(rels, cur, clock, 0, nil, nil)
			require.True(t, ok)
			require.Equal(t, tc.noSoak, got.Version.String())
		})
	}
}

func TestServerClock(t *testing.T) {
	server := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	local := server.Add(-5 * time.Minute) // this machine runs five minutes slow
	limit := scheduleSlack(6 * time.Hour)

	got, ok := serverClock(server, local, local.Add(2*time.Hour), limit)
	require.True(t, ok)
	require.Equal(t, server.Add(2*time.Hour), got, "GitHub's Date plus the time since the check")

	got, _ = serverClock(server, local, local.Add(-time.Hour), limit)
	require.Equal(t, server, got, "a clock that went back adds nothing")

	got, _ = serverClock(server, local, local.Add(48*time.Hour), limit)
	require.Equal(t, server.Add(limit), got, "a clock two days ahead moves GitHub's by one check interval at most")
	require.Equal(t, 6*time.Hour+36*time.Minute+firstCheckSpread, limit)

	_, ok = serverClock(time.Time{}, local, local, limit)
	require.False(t, ok, "no Date header, no clock")
	_, ok = serverClock(server, time.Time{}, local, limit)
	require.False(t, ok, "never checked")
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	const maxDefer = 24 * time.Hour
	countErr := errors.New("database is locked")
	type row struct {
		name string
		in   DecideInput
		path string // "" when it waits
		code string // the wait code when it waits
	}
	base := func(mutate func(*DecideInput)) DecideInput {
		in := DecideInput{Now: now, MaxDefer: maxDefer, Activity: true, LastActivity: now.Add(-time.Hour)}
		mutate(&in)
		return in
	}
	tests := []row{
		// An attempt under way, or a backoff, holds everything.
		{"in progress, even idle", base(func(in *DecideInput) { in.InProgress = true }), "", WaitInProgress},
		{"in progress, even overdue", base(func(in *DecideInput) { in.InProgress = true; in.Pending = 3 * maxDefer }), "", WaitInProgress},
		{"backing off", base(func(in *DecideInput) { in.BackoffUntil = now.Add(time.Minute) }), "", WaitBackoff},
		{"a backoff ending now has ended", base(func(in *DecideInput) { in.BackoffUntil = now }), PathIdle, ""},

		// Idle.
		{"idle", base(func(*DecideInput) {}), PathIdle, ""},
		{"idle, though a request ended a second ago", base(func(in *DecideInput) { in.LastActivity = now.Add(-time.Second) }), PathIdle, ""},
		{"no session, a request in flight", base(func(in *DecideInput) { in.InFlight = 1 }), "", WaitInFlight},
		{"a session is live", base(func(in *DecideInput) { in.Sessions = 1 }), "", WaitSessions},
		{"the count failed: never idle", base(func(in *DecideInput) { in.SessionsErr = countErr }), "", WaitSessionCount},

		// The deadline: max_defer, nothing in flight, 90s quiet.
		{"deadline with quiet", base(func(in *DecideInput) {
			in.Sessions = 2
			in.Pending = maxDefer
			in.LastActivity = now.Add(-91 * time.Second)
		}), PathDeadline, ""},
		{"deadline without quiet", base(func(in *DecideInput) {
			in.Sessions = 2
			in.Pending = maxDefer
			in.LastActivity = now.Add(-30 * time.Second)
		}), "", WaitQuiet},
		{"deadline, a request in flight", base(func(in *DecideInput) { in.Sessions = 2; in.Pending = maxDefer; in.InFlight = 1 }), "", WaitInFlight},
		{"just short of the deadline", base(func(in *DecideInput) { in.Sessions = 2; in.Pending = maxDefer - time.Minute }), "", WaitSessions},
		{"deadline with a failed count", base(func(in *DecideInput) { in.SessionsErr = countErr; in.Pending = maxDefer }), PathDeadline, ""},
		{"a stamp from the future is not quiet", base(func(in *DecideInput) { in.Sessions = 1; in.Pending = maxDefer; in.LastActivity = now.Add(time.Hour) }), "", WaitQuiet},

		// Forced: 1.5x, nothing in flight, no quiet needed.
		{"forced", base(func(in *DecideInput) { in.Sessions = 2; in.Pending = 36 * time.Hour; in.LastActivity = now }), PathForced, ""},
		{"forced waits for in flight", base(func(in *DecideInput) { in.Sessions = 2; in.Pending = 36 * time.Hour; in.InFlight = 2 }), "", WaitInFlight},

		// Overdue: 2x, whatever is in flight (a hung request).
		{"overdue with a request in flight", base(func(in *DecideInput) { in.Sessions = 2; in.Pending = 48 * time.Hour; in.InFlight = 1 }), PathOverdue, ""},
		{"a hung request alone, short of 2x", base(func(in *DecideInput) { in.Pending = 47 * time.Hour; in.InFlight = 1 }), "", WaitInFlight},
		{"a hung request alone, at 2x", base(func(in *DecideInput) { in.Pending = 48 * time.Hour; in.InFlight = 1 }), PathOverdue, ""},

		// A client looping on a bad key: never quiet, rarely in flight.
		{"401 loop, sessions live, past the deadline", base(func(in *DecideInput) { in.Sessions = 1; in.Pending = 30 * time.Hour; in.LastActivity = now }), "", WaitQuiet},
		{"401 loop, sessions live, forced", base(func(in *DecideInput) { in.Sessions = 1; in.Pending = 36 * time.Hour; in.LastActivity = now }), PathForced, ""},
		{"401 loop, no session: idle", base(func(in *DecideInput) { in.LastActivity = now }), PathIdle, ""},

		// No activity tracker wired: nothing is known out of flight.
		{"no tracker, idle", base(func(in *DecideInput) { in.Activity = false }), "", WaitActivity},
		{"no tracker, forced", base(func(in *DecideInput) { in.Activity = false; in.Pending = 36 * time.Hour }), "", WaitActivity},
		{"no tracker, overdue", base(func(in *DecideInput) { in.Activity = false; in.Pending = 48 * time.Hour }), PathOverdue, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.in)
			if tt.path != "" {
				require.True(t, d.Apply, "waits: %s (%s)", d.Code, d.Wait)
				require.Equal(t, tt.path, d.Path)
				require.Empty(t, d.Code)
				return
			}
			require.False(t, d.Apply, "applies by %s", d.Path)
			require.Equal(t, tt.code, d.Code)
			require.NotEmpty(t, d.Wait)
			require.False(t, injectionScrub.MatchString(d.Wait), "the console shows %q", d.Wait)
		})
	}
}

func TestDecide_WaitWords(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	in := DecideInput{Now: now, MaxDefer: 24 * time.Hour, Activity: true, Sessions: 2, Pending: 3 * time.Hour, LastActivity: now}
	require.Equal(t, "waiting for 2 live agent sessions to go idle, or at a lull in requests in 21h", Decide(in).Wait)
	in.Sessions = 1
	require.Equal(t, "waiting for 1 live agent session to go idle, or at a lull in requests in 21h", Decide(in).Wait)
	in.Pending = 25 * time.Hour
	require.Equal(t, "past its 24h deadline; waiting for 90s without requests", Decide(in).Wait)
	in.InFlight = 3
	require.Equal(t, "waiting for 3 requests in flight to finish", Decide(in).Wait)
	in = DecideInput{Now: now, MaxDefer: 24 * time.Hour, BackoffUntil: now.Add(90 * time.Minute)}
	require.Equal(t, "an earlier attempt failed; trying again in 1h 30m", Decide(in).Wait)
}

func TestNewestWait(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := rel("0.7.4", now.Add(-5*time.Hour), true)
	day := 24 * time.Hour

	code, words := newestWait(r, now, day, nil, &Hold{Through: ver("0.7.5"), From: ver("0.7.5")})
	require.Equal(t, WaitHeld, code)
	require.Equal(t, "v0.7.4 is held back: this install went back from v0.7.5, so automatic updates skip releases up to v0.7.5 until they are resumed", words)

	code, words = newestWait(r, now, day, []Block{{Version: ver("0.7.4"), Reason: BlockRolledBack}}, nil)
	require.Equal(t, WaitBlocked, code)
	require.Equal(t, "automatic updates skip v0.7.4: the update to it rolled back", words)

	unsigned := r
	unsigned.ChecksumsBundle = false
	code, words = newestWait(unsigned, now, day, nil, nil)
	require.Equal(t, WaitUnsigned, code)
	require.Equal(t, "v0.7.4 carries no signed checksums bundle, which an automatic update verifies", words)

	code, words = newestWait(r, now, day, nil, nil)
	require.Equal(t, WaitSoak, code)
	require.Equal(t, "v0.7.4 waits out its 24h soak, about 19h to go", words)
	code, words = newestWait(r, time.Time{}, day, nil, nil)
	require.Equal(t, WaitSoak, code)
	require.Equal(t, "v0.7.4 waits out its 24h soak; GitHub's clock is not known yet", words)

	code, words = newestWait(r, now, 0, nil, nil)
	require.Empty(t, code)
	require.Empty(t, words)
}

func TestBackoffWait(t *testing.T) {
	var got []time.Duration
	for n := 1; n <= 7; n++ {
		got = append(got, backoffWait(n))
	}
	require.Equal(t, []time.Duration{
		time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour,
	}, got)
	require.Equal(t, 24*time.Hour, backoffWait(1000), "no overflow")
}
