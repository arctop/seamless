package diary

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A daylight-saving day has 23 or 25 hours, each bucket lands in the local
// hour it ran in, and the next midnight comes from AddDate: 24 hours after a
// fall-back midnight is still the same day.
func TestDay_DaylightSavingDays(t *testing.T) {
	loc := dmZone(t, "America/Los_Angeles")
	f := dmOpen(t, loc)
	// Spring forward: 2026-03-08, 02:00 PST becomes 03:00 PDT.
	spring := f.session(dmSession{Project: "example-ops", Created: f.at(2026, 3, 8, 1, 0), Updated: f.at(2026, 3, 8, 4, 0)})
	f.work(spring, f.at(2026, 3, 8, 1, 30), 15, 15_000, "claude-x", core.UsageThreadMain)
	f.work(spring, f.at(2026, 3, 8, 3, 0), 30, 30_000, "claude-x", core.UsageThreadMain)
	// Fall back: 2026-11-01, 02:00 PDT becomes 01:00 PST; 01:30 happens twice.
	fall := f.session(dmSession{Project: "example-ops", Created: f.at(2026, 11, 1, 0, 30), Updated: f.at(2026, 11, 1, 23, 59)})
	firstHalf := time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)  // 01:30 PDT
	secondHalf := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC) // 01:30 PST
	f.work(fall, firstHalf, 15, 15_000, "claude-x", core.UsageThreadMain)
	f.work(fall, secondHalf, 15, 15_000, "claude-x", core.UsageThreadMain)
	late := f.at(2026, 11, 1, 23, 30) // 07:30Z on Nov 2: past lo+24h, still Nov 1
	f.work(fall, late, 15, 15_000, "claude-x", core.UsageThreadMain)

	s := f.day(f.at(2026, 3, 8, 12, 0))
	require.Equal(t, 23, s.Hours)
	require.Len(t, s.Usage.Hours, 23)
	require.Equal(t, "01:00", s.Usage.Hours[1].Label)
	require.Equal(t, "03:00", s.Usage.Hours[2].Label, "02:00 never happened")
	require.Equal(t, 15, s.Usage.Hours[1].Minutes)
	require.Equal(t, 30, s.Usage.Hours[2].Minutes)
	require.Equal(t, "2026-03-08", s.Day)

	fd := f.day(f.at(2026, 11, 1, 12, 0))
	require.Equal(t, 25, fd.Hours)
	require.Len(t, fd.Usage.Hours, 25)
	require.Equal(t, "01:00", fd.Usage.Hours[1].Label)
	require.Equal(t, "01:00", fd.Usage.Hours[2].Label, "01:00 happens twice")
	require.Equal(t, 15, fd.Usage.Hours[1].Minutes)
	require.Equal(t, 15, fd.Usage.Hours[2].Minutes)
	require.Equal(t, "23:00", fd.Usage.Hours[24].Label)
	require.Equal(t, 15, fd.Usage.Hours[24].Minutes)
	require.Equal(t, 45, fd.Usage.Minutes, "the late bucket is the fall-back day's")
	next := f.day(f.at(2026, 11, 2, 12, 0))
	require.Zero(t, next.Usage.Minutes)
	require.Equal(t, 24, next.Hours)

	m, err := monthAt(f.ctx, f.db, f.at(2026, 11, 15, 0, 0), f.at(2027, 1, 1, 0, 0), loc)
	require.NoError(t, err)
	require.Len(t, m.Days, 30)
	require.Equal(t, "2026-11-01", m.Days[0].Day)
	require.Equal(t, 45, m.Days[0].Facts.Minutes)
	require.Zero(t, m.Days[1].Facts.Minutes)
}

// Before the ledger: a session's stored total counts on the day it ended,
// marked approximate, with no hour, thread or minutes; a session with no
// total at all is unreported and named.
func TestDay_ADayOlderThanTheLedger(t *testing.T) {
	f := dmOpen(t, time.UTC)
	first := f.at(2026, 6, 10, 22, 0)
	old := f.session(dmSession{Project: "example-ops", Created: first, Updated: first.Add(3 * time.Hour), Model: "claude-old",
		Totals: core.TokenUsage{Input: 1_000, Cached: 50_000, CacheCreation: 2_000, Output: 7_000}, Findings: "Old work"})
	none := f.session(dmSession{Project: "example-ops", Created: first.Add(time.Hour), Updated: first.Add(150 * time.Minute)})

	d1 := f.day(first)
	d2 := f.day(first.Add(3 * time.Hour))
	sd := func(e Entry, id string) SessionDay {
		for _, th := range append(e.Spotlight, e.Also...) {
			for _, s := range th.Sessions {
				if s.Session.ID == id {
					return s
				}
			}
		}
		for _, s := range e.Short.Sessions {
			if s.Session.ID == id {
				return s
			}
		}
		require.Failf(t, "no session-day", "%s", id)
		return SessionDay{}
	}
	a1 := sd(d1, old)
	require.True(t, a1.Approximate)
	require.Equal(t, "2026-06-11", a1.EndedOn)
	require.Zero(t, a1.Tokens.Total, "counted on the day it ended, not the day it began")
	require.False(t, d1.Usage.Approximate)
	require.False(t, d1.Facts.Approximate)

	a2 := sd(d2, old)
	require.True(t, a2.Approximate)
	require.Equal(t, 60_000, a2.Tokens.Total)
	require.Zero(t, a2.Minutes)
	require.True(t, d2.Usage.Approximate)
	require.True(t, d2.Facts.Approximate)
	require.Equal(t, 60_000, d2.Usage.Tokens.Total)
	require.Len(t, d2.Usage.Agents, 1)
	require.Equal(t, AgentUsage{Key: "claude-code/claude-old", Harness: "claude-code", Model: "claude-old",
		Tokens:   core.TokenUsage{Input: 1_000, Cached: 50_000, CacheCreation: 2_000, Output: 7_000, Total: 60_000},
		Sessions: 1, Approximate: Part{Tokens: 60_000}}, d2.Usage.Agents[0])
	for _, h := range d2.Usage.Hours {
		require.Zero(t, h.Tokens, "an approximate total has no hour")
	}
	require.True(t, sd(d1, none).Unreported)
	require.Equal(t, []Ref{d1.Usage.Unreported[0]}, d1.Usage.Unreported)
	require.Equal(t, none, d1.Usage.Unreported[0].ID)
	require.Equal(t, 1, d1.Facts.Unreported)
	require.Equal(t, 1, d1.Usage.Reported)

	m, err := monthAt(f.ctx, f.db, first, first.AddDate(1, 0, 0), time.UTC)
	require.NoError(t, err)
	require.True(t, m.Days[10].Facts.Approximate)
	require.Equal(t, 60_000, m.Days[10].Facts.Tokens)
	require.Equal(t, d2.Facts, m.Days[10].Facts)
}

// A plan ships when its last open step closes as done: judged at the moment
// of the close, for the plan's own project, and never when the last step was
// dropped or the plan was already complete.
func TestDay_PlanShipping(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(4 * time.Hour)})
	f.work(s, day, 60, 60_000, "claude-x", core.UsageThreadMain)
	earlier := day.AddDate(0, 0, -2)
	at := func(m int) time.Time { return day.Add(time.Duration(m) * time.Minute) }

	// ships: two steps, the second closes as done.
	a1 := f.task("example-ops", "ships", "a1", earlier, "-")
	a2 := f.task("example-ops", "ships", "a2", earlier, "-")
	f.move(a1, earlier.Add(time.Hour), s, "open", "done")
	f.move(a2, at(10), s, "open", "done")
	// dropped: the last open step is dropped.
	b1 := f.task("example-ops", "dropped", "b1", earlier, "-")
	b2 := f.task("example-ops", "dropped", "b2", earlier, "-")
	f.move(b1, at(11), s, "open", "done")
	f.move(b2, at(12), s, "open", "dropped")
	// twin: the same slug in two projects; only example-ops' steps all close.
	c1 := f.task("example-ops", "twin", "c1", earlier, "-")
	f.task("example-finance", "twin", "c2", earlier, "-")
	f.move(c1, at(13), s, "open", "done")
	// later: ships at the close, then gains a step the same day.
	d1 := f.task("example-ops", "later", "d1", earlier, "-")
	f.move(d1, at(14), s, "open", "done")
	f.task("example-ops", "later", "d2", at(30), s)
	// done: complete before the day; an edit restates done.
	e1 := f.task("example-ops", "done", "e1", earlier, "-")
	f.move(e1, earlier.Add(time.Hour), s, "open", "done")
	f.move(e1, at(15), s, "done", "done")
	// reopened: closed, reopened and closed again on the day ships once.
	g1 := f.task("example-ops", "reopened", "g1", earlier, "-")
	f.move(g1, at(16), s, "open", "done")
	f.move(g1, at(17), s, "done", "open")
	f.move(g1, at(18), s, "open", "done")

	e := f.day(day)
	shipped := map[string]bool{}
	for _, r := range e.Moments.Shipped {
		shipped[r.Project+":"+r.ID] = true
	}
	require.Equal(t, map[string]bool{"example-ops:ships": true, "example-ops:twin": true, "example-ops:later": true,
		"example-ops:reopened": true}, shipped)
	require.Equal(t, 4, e.Facts.PlansShipped)
	require.True(t, dmThread(t, e, "plan:example-ops:ships").Shipped)
	require.False(t, dmThread(t, e, "plan:example-ops:dropped").Shipped)
	require.Equal(t, &Progress{Start: Count{0, 2}, End: Count{2, 2}}, dmThread(t, e, "plan:example-ops:dropped").Progress)
	require.Equal(t, &Progress{Start: Count{0, 1}, End: Count{1, 2}}, dmThread(t, e, "plan:example-ops:later").Progress)
	require.Equal(t, &Progress{Start: Count{1, 1}, End: Count{1, 1}}, dmThread(t, e, "plan:example-ops:done").Progress)
	require.Equal(t, WordShipped, e.Character.Word)
	require.Contains(t, dmThread(t, e, "plan:example-ops:ships").Reasons, Reason{Kind: ReasonShipped, Count: 1, Points: PointsShipped})
	// a2, b1, c1, d1 and g1 (closed, reopened and closed again: once); not a1
	// (closed before the day), b2 (dropped) or e1 (an edit of a done task).
	require.Equal(t, 5, e.Facts.TasksClosed)
}
