package diary

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A session moving steps in two plans appears in both plan threads, marked
// shared, with its whole usage in each; the day's totals count it once.
func TestDay_ASessionInTwoPlansIsSharedAndCountedOnce(t *testing.T) {
	f := dmOpen(t, time.UTC)
	f.project("example-ops", false)
	start := f.at(2026, 11, 18, 9, 0)
	s := f.session(dmSession{Project: "example-ops", Created: start, Updated: start.Add(3 * time.Hour),
		Findings: "Moved two plans forward"})
	f.work(s, start, 90, 90_000, "claude-x", core.UsageThreadMain)
	a := f.task("example-ops", "alpha", "Alpha step", start.AddDate(0, 0, -2), "-")
	b := f.task("example-ops", "beta", "Beta step", start.AddDate(0, 0, -2), "-")
	f.move(a, start.Add(10*time.Minute), s, "", "in_progress")
	f.move(a, start.Add(40*time.Minute), s, "in_progress", "done")
	f.move(b, start.Add(50*time.Minute), s, "", "in_progress")

	e := f.day(start)
	alpha := dmThread(t, e, "plan:example-ops:alpha")
	beta := dmThread(t, e, "plan:example-ops:beta")
	for _, th := range []Thread{alpha, beta} {
		require.Len(t, th.Sessions, 1, th.Key)
		require.True(t, th.Sessions[0].Shared, th.Key)
		require.Equal(t, 1, th.Shared, th.Key)
		require.Equal(t, 90, th.Minutes, "a shared session counts in full toward %s", th.Key)
	}
	require.Equal(t, 90, e.Usage.Minutes, "the day counts the session once")
	require.Equal(t, e.Usage.Tokens, alpha.Tokens, "and its tokens once")
	require.Equal(t, 1, e.Facts.Sessions)
	require.Equal(t, 1, e.Facts.TasksClosed)
	require.Equal(t, []Move{{
		Task: Ref{Kind: RefTask, ID: a, Title: "Alpha step", Project: "example-ops", Resolved: true, Status: "done"},
		From: "open", To: "done", At: start.Add(40 * time.Minute), Claimed: true, Closed: true, Sessions: []string{s},
	}}, alpha.Moved)
	require.True(t, alpha.Shipped, "alpha's only step closed as done")
	require.Equal(t, &Progress{Start: Count{0, 1}, End: Count{1, 1}}, alpha.Progress)
	require.Equal(t, &Progress{Start: Count{0, 1}, End: Count{0, 1}}, beta.Progress)
	require.NotContains(t, e.Headline, "Mostly", "two threads share the day's whole agent time: neither is mostly the day")
}

// Short sessions fold: no outcome, under ten minutes and under 4k output.
// Each limit alone keeps a session out of the fold, and an outcome does too.
func TestDay_TheMinorFold(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 8, 0)
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	quick := f.session(dmSession{Project: "example-ops", Created: at(1), Updated: at(1).Add(5 * time.Minute)})
	f.work(quick, at(1), 4, 4*dmRate, "claude-x", core.UsageThreadMain)
	zero := f.session(dmSession{Project: "example-ops", Created: at(2), Updated: at(2), Findings: core.FindingNoSummary})
	harvested := f.session(dmSession{Project: "example-ops", Created: at(3), Updated: at(3).Add(9 * time.Minute),
		Findings: "(auto-harvested) Answered a question about the build"})
	f.work(harvested, at(3), 9, MinorOutput-3_000, "claude-x", core.UsageThreadMain)
	long := f.session(dmSession{Project: "example-ops", Created: at(4), Updated: at(4).Add(time.Hour)})
	f.work(long, at(4), 10, 1_000, "claude-x", core.UsageThreadMain)
	wordy := f.session(dmSession{Project: "example-ops", Created: at(5), Updated: at(5).Add(time.Hour)})
	f.work(wordy, at(5), 6, MinorOutput, "claude-x", core.UsageThreadMain)
	writer := f.session(dmSession{Project: "example-ops", Created: at(6), Updated: at(6), Explicit: true})
	m := f.memory("a-gotcha", "example-ops", at(6))
	f.event(core.EventMemoryWritten, at(6), writer, "example-ops", m, map[string]any{"name": "a-gotcha"})
	ended := f.session(dmSession{Project: "example-ops", Created: at(7), Updated: at(7).Add(2 * time.Minute),
		Findings: "Wrote up the release checklist"})

	e := f.day(day)
	short := map[string]bool{}
	for _, sd := range e.Short.Sessions {
		short[sd.Session.ID] = true
		require.True(t, sd.Minor)
	}
	require.Equal(t, map[string]bool{quick: true, zero: true, harvested: true}, short,
		"an auto-harvested finding is not an outcome")
	require.Equal(t, 3, e.Short.Count)
	require.Equal(t, 13, e.Short.Minutes)
	th := dmThread(t, e, "project:example-ops")
	var in []string
	for _, sd := range th.Sessions {
		in = append(in, sd.Session.ID)
		require.False(t, sd.Minor)
	}
	require.ElementsMatch(t, []string{long, wordy, writer, ended}, in,
		"ten minutes, MinorOutput tokens, a memory written and session_end's own findings each keep a session out of the fold")
	require.Equal(t, 7, e.Facts.Sessions, "a minor session is still a session of the day")
	require.Equal(t, 3, e.Facts.Short)
	require.Equal(t, 4+9+10+6, e.Usage.Minutes, "and its agent time still counts")
}

// A flagged session pins its thread into the spotlight below the floor, and
// is never minor.
func TestDay_AFlagPinsItsThread(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 10, 0)
	flagged := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(3 * time.Minute), Verdict: "notable"})
	f.work(flagged, day, 2, 500, "claude-x", core.UsageThreadMain)
	f.event(core.EventSessionFlagged, day.Add(time.Hour), flagged, "example-ops", "", map[string]any{"verdict": "notable", "reason": "odd"})
	plain := f.session(dmSession{Project: "example-finance", Created: day, Updated: day.Add(3 * time.Minute)})
	f.work(plain, day, 2, 500, "claude-x", core.UsageThreadMain)
	// A session from last week, flagged today: the flag is today's moment, and
	// no activity of the session's.
	old := day.AddDate(0, 0, -7)
	earlier := f.session(dmSession{Project: "example-legal", Created: old, Updated: old.Add(time.Hour), Verdict: "good"})
	f.work(earlier, old, 30, 30_000, "claude-x", core.UsageThreadMain)
	f.event(core.EventSessionFlagged, day.Add(2*time.Hour), earlier, "example-legal", "", map[string]any{"verdict": "good"})

	e := f.day(day)
	require.Equal(t, []string{"project:example-ops"}, dmSpotKeys(e))
	th := e.Spotlight[0]
	require.True(t, th.Pinned)
	require.Less(t, th.Score, float64(SpotlightFloor), "pinned below the floor")
	require.Equal(t, ReasonFlagged, th.Reasons[len(th.Reasons)-1].Kind)
	require.Equal(t, "notable", th.Sessions[0].Flag)
	require.Equal(t, 1, e.Short.Count, "the unflagged twin folds")
	require.Len(t, e.Moments.Flags, 2)
	require.Equal(t, Flag{Session: th.Sessions[0].Session, Verdict: "notable", Reason: "odd", At: day.Add(time.Hour)},
		e.Moments.Flags[0])
	require.Equal(t, earlier, e.Moments.Flags[1].Session.ID)
	require.Equal(t, 2, e.Facts.Sessions, "flagging a session later credits it with no activity")
}

// Threads rank by significance: outcomes, then effort; the floor keeps a
// small thread in Also; the cap keeps the spotlight to six. Output is at the
// measured rate of a thinking model (dmRate tokens a minute).
func TestDay_SignificanceOrdersTheSpotlight(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 8, 0)
	// A plan with three steps closed by one session in 40 minutes.
	planner := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour)})
	f.work(planner, day, 40, 40*dmRate, "claude-x", core.UsageThreadMain)
	for i := range 3 {
		task := f.task("example-ops", "rollout", "Step", day.AddDate(0, 0, -1), "-")
		f.move(task, day.Add(time.Duration(10+i)*time.Minute), planner, "open", "done")
	}
	f.task("example-ops", "rollout", "Last step", day.AddDate(0, 0, -1), "-")
	// Two hours of exploring with no outcome.
	explorer := f.session(dmSession{Project: "example-finance", Created: day, Updated: day.Add(3 * time.Hour)})
	f.work(explorer, day.Add(time.Hour), 120, 120*dmRate, "claude-x", core.UsageThreadMain)
	// Fifteen minutes of a small fix.
	fixer := f.session(dmSession{Project: "example-legal", Created: day, Updated: day.Add(time.Hour)})
	f.work(fixer, day.Add(4*time.Hour), 15, 15*dmRate, "claude-x", core.UsageThreadMain)
	// Six more projects, each with half an hour of work.
	for _, p := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		s := f.session(dmSession{Project: "example-" + p, Created: day, Updated: day.Add(time.Hour)})
		f.work(s, day.Add(5*time.Hour), 30, 30*dmRate, "claude-x", core.UsageThreadMain)
	}

	e := f.day(day)
	rollout := dmThread(t, e, "plan:example-ops:rollout")
	require.Equal(t, []Reason{
		{Kind: ReasonClosed, Count: 3, Points: 9},
		{Kind: ReasonMinutes, Count: 40, Points: 4},
		{Kind: ReasonOutput, Count: 40 * dmRate, Points: 40 * dmRate / float64(OutputPerPoint)},
	}, rollout.Reasons)
	require.InDelta(t, 15.0, rollout.Score, 1e-9)
	explore := dmThread(t, e, "project:example-finance")
	require.InDelta(t, 17.9, explore.Score, 1e-9, "12 for two hours (the cap), 5.9 for their output")
	require.Equal(t, []string{"project:example-finance", "plan:example-ops:rollout",
		"project:example-p1", "project:example-p2", "project:example-p3", "project:example-p4"}, dmSpotKeys(e),
		"most significant first, ties by key, six at most")
	require.InDelta(t, 4.5, dmThread(t, e, "project:example-p5").Score, 1e-9, "at the floor, and past the cap")
	fixed := dmThread(t, e, "project:example-legal")
	require.InDelta(t, 2.2, fixed.Score, 1e-9)
	require.Equal(t, []string{"project:example-p5", "project:example-p6", "project:example-legal"},
		dmKeys(e)[len(e.Spotlight):], "the rest, in the same order, under the floor last")
}

// A note appended five times is one note; a memory written again is one
// updated memory; deleting a note is no write.
func TestDay_KeptCountsDistinctItems(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(2 * time.Hour)})
	f.work(s, day, 30, 30_000, "claude-x", core.UsageThreadMain)
	n := f.note("Release notes", "example-ops", nil, day)
	f.event(core.EventNoteWritten, day, s, "example-ops", n, map[string]any{"title": "Release notes"})
	for i := range 4 {
		f.event(core.EventNoteWritten, day.Add(time.Duration(i+1)*time.Minute), s, "example-ops", n,
			map[string]any{"slug": "release-notes", "appended": true})
	}
	old := f.memory("old-gotcha", "example-ops", day.AddDate(0, -1, 0))
	f.event(core.EventMemoryWritten, day.Add(10*time.Minute), s, "example-ops", old, map[string]any{"name": "old-gotcha", "updated": true})
	f.event(core.EventMemoryWritten, day.Add(11*time.Minute), s, "example-ops", old, map[string]any{"name": "old-gotcha", "edited": true})
	gone := f.id("NOTE")
	f.event(core.EventNoteWritten, day.Add(20*time.Minute), s, "example-ops", gone, map[string]any{"slug": "scratch", "deleted": true})

	e := f.day(day)
	require.Equal(t, 2, e.Facts.Kept)
	require.Len(t, e.Kept.Notes, 1)
	note := e.Kept.Notes[0]
	require.Equal(t, 5, note.Writes)
	require.True(t, note.New)
	require.Equal(t, "Release notes", note.Item.Title)
	require.Equal(t, []string{s}, note.Sessions)
	require.Len(t, e.Kept.Memories, 1)
	require.False(t, e.Kept.Memories[0].New)
	require.Equal(t, 2, e.Kept.Memories[0].Writes)
	require.Equal(t, 1, e.Kept.New)
	require.Equal(t, 1, e.Kept.Updated)
	th := dmThread(t, e, "project:example-ops")
	require.Len(t, th.Kept, 2)
	require.Equal(t, Reason{Kind: ReasonKept, Count: 2, Points: 2}, th.Reasons[0])
}

// A session across midnight is a session of both days, each counting only its
// own agent time; it started on the first and ended on the second.
func TestDay_ASessionAcrossMidnight(t *testing.T) {
	f := dmOpen(t, time.UTC)
	start := f.at(2026, 11, 18, 23, 15)
	s := f.session(dmSession{Project: "example-ops", Created: start, Updated: start.Add(2 * time.Hour),
		Findings: "Finished the migration"})
	f.work(s, start, 45, 45_000, "claude-x", core.UsageThreadMain)
	f.work(s, f.at(2026, 11, 19, 0, 0), 30, 30_000, "claude-x", core.UsageThreadMain)

	first, second := f.day(start), f.day(start.Add(2*time.Hour))
	require.Equal(t, 45, first.Usage.Minutes)
	require.Equal(t, 30, second.Usage.Minutes)
	a := dmThread(t, first, "project:example-ops").Sessions[0]
	b := dmThread(t, second, "project:example-ops").Sessions[0]
	require.True(t, a.Started)
	require.False(t, a.Ended)
	require.False(t, b.Started)
	require.True(t, b.Ended)
	require.Equal(t, 45, a.Hours[23])
	require.Equal(t, 30, b.Hours[0])
	require.Equal(t, 1, first.SessionsStarted)
	require.Equal(t, 0, second.SessionsStarted, "sessions?day= lists the sessions created that day")
	require.Equal(t, 1, second.Facts.Sessions)
	require.Contains(t, dmThread(t, first, "project:example-ops").Reasons, Reason{Kind: ReasonMinutes, Count: 45, Points: 4.5},
		"agent minutes score on the day they ran")
	require.NotContains(t, dmReasonKinds(dmThread(t, first, "project:example-ops")), ReasonFindings,
		"findings count on the day the session ended")
	require.Contains(t, dmReasonKinds(dmThread(t, second, "project:example-ops")), ReasonFindings)
	require.Equal(t, "findings", b.AboutFrom)
}

// Today reads "so far", and its live session with no tokens yet is
// unreported and named.
func TestDay_TodaySoFar(t *testing.T) {
	f := dmOpen(t, time.UTC)
	start := f.at(2026, 11, 20, 9, 0)
	live := f.session(dmSession{Project: "example-ops", Created: start, Updated: start.Add(time.Hour), Status: core.SessionActive})
	task := f.task("example-ops", "", "Wire it", start, live)
	f.move(task, start.Add(time.Minute), live, "", "in_progress")

	e, err := dayAt(f.ctx, f.db, start, start.Add(2*time.Hour), time.UTC)
	require.NoError(t, err)
	require.True(t, e.Today)
	require.Contains(t, e.Headline, "so far.")
	require.Equal(t, 1, e.Facts.Unreported)
	require.Len(t, e.Usage.Unreported, 1)
	require.Equal(t, live, e.Usage.Unreported[0].ID)
	require.Equal(t, 0, e.Usage.Reported)
	th := dmThread(t, e, "project:example-ops")
	require.Equal(t, 1, th.Unreported)
	require.True(t, th.Sessions[0].Unreported)
	_, err = dayAt(f.ctx, f.db, start.AddDate(0, 0, 1), start.Add(2*time.Hour), time.UTC)
	require.ErrorIs(t, err, ErrFutureDay)
}

// Agent time adds up across agents running in parallel, past the wall clock;
// the split by agent, thread and hour holds it exactly.
func TestDay_AParallelAfternoonSplitsByAgent(t *testing.T) {
	f := dmOpen(t, time.UTC)
	start := f.at(2026, 11, 18, 14, 0)
	lead := f.session(dmSession{Project: "example-ops", Created: start, Updated: start.Add(2 * time.Hour), Model: "claude-x"})
	f.work(lead, start, 60, 60_000, "claude-x", core.UsageThreadMain)
	f.work(lead, start, 50, 50_000, "claude-x", core.UsageThreadSubagent)
	other := f.session(dmSession{Project: "example-ops", Created: start, Updated: start.Add(2 * time.Hour), Client: "codex"})
	f.work(other, start.Add(30*time.Minute), 60, 30_000, "gpt-y", core.UsageThreadMain)

	e := f.day(start)
	require.Equal(t, 170, e.Usage.Minutes, "170 agent minutes in two wall-clock hours")
	require.Len(t, e.Usage.Agents, 2)
	cc := e.Usage.Agents[0]
	require.Equal(t, "claude-code/claude-x", cc.Key)
	require.Equal(t, Part{Minutes: 60, Tokens: 690_000}, cc.Main)
	require.Equal(t, Part{Minutes: 50, Tokens: 575_000}, cc.Subagent)
	require.Equal(t, 1, cc.Sessions)
	cx := e.Usage.Agents[1]
	require.Equal(t, "codex/gpt-y", cx.Key)
	require.Equal(t, 60, cx.Minutes)
	sum := 0
	for _, h := range e.Usage.Hours {
		agents := 0
		for _, a := range h.Agents {
			agents += a.Minutes
		}
		require.Equal(t, h.Minutes, agents, h.Label)
		sum += h.Minutes
	}
	require.Equal(t, 170, sum)
	require.Equal(t, 60+50+30, e.Usage.Hours[14].Minutes)
	require.Equal(t, []AgentHour{
		{Key: "claude-code/claude-x", Minutes: 110, Tokens: 690_000 + 575_000},
		{Key: "codex/gpt-y", Minutes: 30, Tokens: 172_500},
	}, e.Usage.Hours[14].Agents, "in Usage.Agents' order")
	require.Equal(t, 30, e.Usage.Hours[15].Minutes)
}

// dmReasonKinds lists a thread's reason kinds.
func dmReasonKinds(t Thread) []string {
	var out []string
	for _, r := range t.Reasons {
		out = append(out, r.Kind)
	}
	return out
}
