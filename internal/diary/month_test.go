package diary

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Every day of a month agrees with its Day: the same Facts, top projects and
// character, a month of generated history long, across a daylight-saving
// change and the ledger's first day.
func TestMonth_AgreesWithEveryDay(t *testing.T) {
	loc := dmZone(t, "America/Los_Angeles")
	f := dmOpen(t, loc)
	from := f.at(2026, 10, 20, 0, 0)
	f.inTx(func() {
		dmGenerate(f, from, 26, 7, dmScale{sessions: 8, prompts: true, content: 64, ledger: f.at(2026, 10, 25, 0, 0)})
	})
	now := f.at(2026, 11, 14, 15, 0) // mid-month: the month stops at today
	m, err := monthAt(f.ctx, f.db, f.at(2026, 11, 3, 0, 0), now, loc)
	require.NoError(t, err)
	require.Equal(t, "2026-11", m.Month)
	require.Len(t, m.Days, 14, "the days that have begun, today the last")
	busy := 0
	for _, md := range m.Days {
		day, err := ParseDay(md.Day)
		require.NoError(t, err)
		d, err := dayAt(f.ctx, f.db, time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc), now, loc)
		require.NoError(t, err)
		require.Equal(t, d.Facts, md.Facts, md.Day)
		require.Equal(t, d.Character, md.Character, md.Day)
		require.Equal(t, d.Usage.Projects[:min(len(d.Usage.Projects), MonthProjects)], md.Projects, md.Day)
		if d.Facts.Sessions > 0 {
			busy++
		}
	}
	require.Greater(t, busy, 8, "the history is not empty")
	oct, err := monthAt(f.ctx, f.db, f.at(2026, 10, 1, 0, 0), now, loc)
	require.NoError(t, err)
	require.Len(t, oct.Days, 31)
	approx := 0
	for _, md := range oct.Days {
		if md.Facts.Approximate {
			approx++
		}
	}
	require.Positive(t, approx, "the days before the ledger read the stored totals")
	_, err = monthAt(f.ctx, f.db, f.at(2026, 12, 1, 0, 0), now, loc)
	require.ErrorIs(t, err, ErrFutureDay)
}

// The usual day is the median of the trailing active days, and no
// comparison at all with fewer than three.
func TestUsual(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 20, 0, 0)
	u, err := usualAt(f.ctx, f.db, day, time.UTC)
	require.NoError(t, err)
	require.False(t, u.Has)
	require.Empty(t, u.Series)

	minutes := []int{10, 40, 20, 90, 30}
	for i, m := range minutes {
		start := day.AddDate(0, 0, -2*(i+1)).Add(9 * time.Hour) // every other day back
		s := f.session(dmSession{Project: "example-ops", Created: start, Updated: start.Add(3 * time.Hour)})
		f.work(s, start, m, m*1000, "claude-x", "main")
		if i < 2 {
			u, err = usualAt(f.ctx, f.db, day, time.UTC)
			require.NoError(t, err)
			require.False(t, u.Has, "%d active days are no comparison", i+1)
			require.Len(t, u.Series, i+1)
		}
	}
	u, err = usualAt(f.ctx, f.db, day, time.UTC)
	require.NoError(t, err)
	require.True(t, u.Has)
	require.Equal(t, 5, u.Days)
	require.Equal(t, 30, u.Minutes, "the median of 10, 40, 20, 90, 30")
	require.Equal(t, "2026-11-10", u.Series[0].Day, "oldest first")
	require.Equal(t, "2026-11-18", u.Series[4].Day)
	// A day long before the rest still counts, within the lookback.
	far := day.AddDate(0, 0, -60).Add(9 * time.Hour)
	s := f.session(dmSession{Project: "example-ops", Created: far, Updated: far.Add(time.Hour)})
	f.work(s, far, 50, 50_000, "claude-x", "main")
	u, err = usualAt(f.ctx, f.db, day, time.UTC)
	require.NoError(t, err)
	require.Equal(t, 6, u.Days)
	require.Equal(t, 35, u.Minutes, "the median of six: (30 + 40) / 2")
	today := f.session(dmSession{Project: "example-ops", Created: day.Add(time.Hour), Updated: day.Add(2 * time.Hour)})
	f.work(today, day.Add(time.Hour), 500, 500_000, "claude-x", "main")
	same, err := usualAt(f.ctx, f.db, day.Add(23*time.Hour), time.UTC)
	require.NoError(t, err)
	require.Equal(t, u, same, "the day itself is never its own usual")
}

// The JSON a day marshals to is camelCase, carries every list as a list
// (never null), and keeps its keys: the screen's ?format=json adds hrefs to
// exactly this.
func TestEntry_JSONShape(t *testing.T) {
	f := dmOpen(t, time.UTC)
	empty := f.day(f.at(2026, 11, 18, 0, 0))
	raw, err := json.Marshal(empty)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "null", "an empty day lists nothing as [], never null")

	f.inTx(func() {
		dmGenerate(f, f.at(2026, 11, 10, 0, 0), 10, 3, dmScale{sessions: 10, prompts: true, content: 16})
	})
	busy := f.day(f.at(2026, 11, 17, 0, 0))
	raw, err = json.Marshal(busy)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "null")
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &top))
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	require.Equal(t, []string{"also", "character", "day", "end", "facts", "headline", "hours", "kept", "moments",
		"projects", "sessionsStarted", "short", "spotlight", "start", "today", "usage"}, keys)
	for _, k := range keys {
		require.False(t, strings.Contains(k, "_"), "camelCase: %s", k)
	}
	require.NotEmpty(t, busy.Spotlight)
	th, err := json.Marshal(busy.Spotlight[0])
	require.NoError(t, err)
	for _, k := range []string{`"key"`, `"kind"`, `"label"`, `"project"`, `"moved"`, `"sessions"`, `"minutes"`, `"tokens"`,
		`"shared"`, `"unreported"`, `"hours"`, `"kept"`, `"commits"`, `"score"`, `"reasons"`} {
		require.Contains(t, string(th), k)
	}
	m, err := monthAt(f.ctx, f.db, f.at(2026, 11, 1, 0, 0), f.at(2027, 1, 1, 0, 0), time.UTC)
	require.NoError(t, err)
	raw, err = json.Marshal(m)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "null")
	u, err := usualAt(f.ctx, f.db, f.at(2026, 11, 20, 0, 0), time.UTC)
	require.NoError(t, err)
	raw, err = json.Marshal(u)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "null")
}
