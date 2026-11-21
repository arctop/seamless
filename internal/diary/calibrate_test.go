package diary

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The calibration days: synthetic days built to the plan's four patterns, at
// the shapes of this repository's own transcripts (a working session is half
// an hour to two hours of agent time at about a thousand output tokens a
// minute; a short question is minutes). Each asserts the spotlight a person
// would name as the day's work, and that no zero-minute session reaches it.

// The output rates the calibration days write: this repository's median
// output tokens per minute of agent time, on the main thread and in
// subagents (counted from its Claude Code transcripts, 2026-11-20).
const (
	dmRate    = 3950
	dmSubRate = 5050
)

// dmCalibrationLog renders a day's spotlight for the test log.
func dmCalibrationLog(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s\n", e.Day, e.Character.Word, e.Headline)
	for _, t := range e.Spotlight {
		fmt.Fprintf(&b, "  spot %-34s %5.1f  %s\n", t.Key, t.Score, dmReasonLine(t))
	}
	for _, t := range e.Also {
		fmt.Fprintf(&b, "  also %-34s %5.1f  %s\n", t.Key, t.Score, dmReasonLine(t))
	}
	fmt.Fprintf(&b, "  short %d sessions, %dm\n", e.Short.Count, e.Short.Minutes)
	return b.String()
}

func dmReasonLine(t Thread) string {
	var parts []string
	for _, r := range t.Reasons {
		parts = append(parts, fmt.Sprintf("%s=%d(%.1f)", r.Kind, r.Count, r.Points))
	}
	return strings.Join(parts, " ")
}

// dmNoZeroMinuteInSpotlight fails when a spotlight thread holds a session
// with no agent time and no outcome.
func dmNoZeroMinuteInSpotlight(t *testing.T, e Entry) {
	t.Helper()
	for _, th := range e.Spotlight {
		for _, sd := range th.Sessions {
			if sd.Minutes == 0 {
				require.True(t, sd.Flag != "" || sd.Ended || len(th.Moved) > 0 || len(th.Kept) > 0,
					"a zero-minute session in the spotlight must carry an outcome: %s in %s", sd.Session.ID, th.Key)
			}
		}
	}
}

func TestCalibration_ABusyPlanDay(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 0, 0)
	h := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	// The plan: fourteen steps, ten closed before the day; two sessions close
	// three, claim a fourth, and write a memory and a note.
	var steps []string
	for i := range 14 {
		s := f.task("example-ops", "rollout", fmt.Sprintf("Step %d", i+1), day.AddDate(0, 0, -10), "-")
		if i < 10 {
			f.move(s, day.AddDate(0, 0, -9+i/2), "", "open", "done")
		}
		steps = append(steps, s)
	}
	a := f.session(dmSession{Project: "example-ops", Created: h(9, 0), Updated: h(10, 30), Findings: "Closed steps 11 and 12"})
	f.work(a, h(9, 5), 70, 70*dmRate, "claude-x", core.UsageThreadMain)
	f.move(steps[10], h(9, 10), a, "", "in_progress")
	f.move(steps[10], h(9, 50), a, "in_progress", "done")
	f.move(steps[11], h(9, 55), a, "", "in_progress")
	f.move(steps[11], h(10, 20), a, "in_progress", "done")
	m := f.memory("rollout-gotcha", "example-ops", h(10, 0))
	f.event(core.EventMemoryWritten, h(10, 0), a, "example-ops", m, map[string]any{"name": "rollout-gotcha"})
	b := f.session(dmSession{Project: "example-ops", Created: h(14, 0), Updated: h(15, 0), Findings: "Closed step 13"})
	f.work(b, h(14, 0), 45, 45*dmRate, "claude-x", core.UsageThreadMain)
	f.move(steps[12], h(14, 5), b, "", "in_progress")
	f.move(steps[12], h(14, 40), b, "in_progress", "done")
	f.move(steps[13], h(14, 45), b, "", "in_progress")
	n := f.note("Rollout notes", "example-ops", nil, h(14, 50))
	f.event(core.EventNoteWritten, h(14, 50), b, "example-ops", n, map[string]any{"title": "Rollout notes"})
	// Beside it: two hours of exploring with no outcome, a small fix closed,
	// and five short questions.
	x := f.session(dmSession{Project: "example-finance", Created: h(11, 0), Updated: h(13, 30)})
	f.work(x, h(11, 0), 120, 120*dmRate, "claude-x", core.UsageThreadMain)
	w := f.session(dmSession{Project: "example-web", Created: h(16, 0), Updated: h(16, 20)})
	f.work(w, h(16, 0), 12, 12*dmRate, "claude-x", core.UsageThreadMain)
	fix := f.task("example-web", "", "Fix the header", h(16, 2), w)
	f.move(fix, h(16, 15), w, "open", "done")
	for i, p := range []string{"example-ops", "example-legal", "example-web", "example-finance", ""} {
		q := f.session(dmSession{Project: p, CWD: "/home/owner/scratch", Created: h(17, 10*i), Updated: h(17, 10*i+5),
			Findings: "(auto-harvested) Answered a question"})
		f.work(q, h(17, 10*i), 2+i, (2+i)*dmRate, "claude-x", core.UsageThreadMain)
	}

	e := f.day(day)
	t.Log(dmCalibrationLog(e))
	require.Equal(t, "plan:example-ops:rollout", e.Spotlight[0].Key, "the plan that moved leads the day")
	require.Equal(t, []string{"plan:example-ops:rollout", "project:example-finance", "project:example-web"}, dmSpotKeys(e))
	require.Equal(t, 5, e.Short.Count)
	require.Equal(t, &Progress{Start: Count{10, 14}, End: Count{13, 14}}, e.Spotlight[0].Progress)
	require.True(t, strings.HasPrefix(e.Headline, "Led by rollout in example-ops"), e.Headline)
	require.Contains(t, e.Headline, "3 steps closed, the plan is now at 13 of 14")
	dmNoZeroMinuteInSpotlight(t, e)
}

func TestCalibration_ADayOfZeroMinuteSessions(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 0, 0)
	// Twenty-five sessions that started and ended with nothing harvested,
	// three quick questions, and one hour of real work.
	for i := range 25 {
		at := day.Add(time.Duration(8*60+i*20) * time.Minute)
		f.session(dmSession{Project: []string{"example-ops", "example-web", ""}[i%3], CWD: "/home/owner/tools",
			Created: at, Updated: at, Findings: core.FindingNoSummary})
	}
	for i := range 3 {
		at := day.Add(time.Duration(10+i) * time.Hour)
		q := f.session(dmSession{Project: "example-web", Created: at, Updated: at.Add(4 * time.Minute)})
		f.work(q, at, 3, 3*dmRate, "claude-x", core.UsageThreadMain)
	}
	real := f.session(dmSession{Project: "example-ops", Created: day.Add(14 * time.Hour), Updated: day.Add(15 * time.Hour),
		Findings: "Reworked the importer"})
	f.work(real, day.Add(14*time.Hour), 55, 55*dmRate, "claude-x", core.UsageThreadMain)

	e := f.day(day)
	t.Log(dmCalibrationLog(e))
	require.Equal(t, []string{"project:example-ops"}, dmSpotKeys(e))
	require.Empty(t, e.Also)
	require.Equal(t, 28, e.Short.Count)
	require.Equal(t, 29, e.Facts.Sessions)
	require.Equal(t, 25, e.Facts.Unreported, "a session with no token record is unreported, never a zero")
	dmNoZeroMinuteInSpotlight(t, e)
}

func TestCalibration_AParallelAgentAfternoon(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 0, 0)
	h := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	// An orchestrating session claims and closes four steps while its
	// subagents work beside it; three agents in their own sessions write the
	// code without touching a task.
	var steps []string
	for i := range 6 {
		steps = append(steps, f.task("example-ops", "wave", fmt.Sprintf("Step %d", i+1), day.AddDate(0, 0, -1), "-"))
	}
	lead := f.session(dmSession{Project: "example-ops", Created: h(13, 0), Updated: h(18, 0), Findings: "Landed four steps"})
	f.work(lead, h(13, 0), 40, 40*dmRate, "claude-x", core.UsageThreadMain)
	f.work(lead, h(13, 10), 150, 150*dmSubRate, "claude-x", core.UsageThreadSubagent)
	for i := range 4 {
		f.move(steps[i], h(13, 5+i), lead, "", "in_progress")
		f.move(steps[i], h(16, 10*i), lead, "in_progress", "done")
	}
	for i, minutes := range []int{90, 75, 60} {
		w := f.session(dmSession{Project: "example-ops", Created: h(13, 15), Updated: h(16, 30),
			Findings: fmt.Sprintf("Implemented part %d", i+1)})
		f.work(w, h(13, 20), minutes, minutes*dmRate, "claude-y", core.UsageThreadMain)
	}

	e := f.day(day)
	t.Log(dmCalibrationLog(e))
	require.Equal(t, []string{"plan:example-ops:wave", "project:example-ops"}, dmSpotKeys(e))
	require.Equal(t, 40+150+90+75+60, e.Usage.Minutes, "agent time across parallel agents, past the wall clock")
	require.Greater(t, e.Usage.Minutes, 5*60)
	require.Equal(t, WordDeep, e.Character.Word, "seven hours of agent time in one project")
	dmNoZeroMinuteInSpotlight(t, e)
}

func TestCalibration_ASessionAcrossMidnight(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 0, 0)
	step := f.task("example-ops", "migrate", "Move the data", day.AddDate(0, 0, -1), "-")
	other := f.task("example-ops", "migrate", "Verify", day.AddDate(0, 0, -1), "-")
	s := f.session(dmSession{Project: "example-ops", Created: day.Add(23*time.Hour + 10*time.Minute),
		Updated: day.Add(25*time.Hour + 30*time.Minute), Findings: "Migrated and verified"})
	f.work(s, day.Add(23*time.Hour+10*time.Minute), 50, 50*dmRate, "claude-x", core.UsageThreadMain)
	f.move(step, day.Add(23*time.Hour+15*time.Minute), s, "", "in_progress")
	f.work(s, day.Add(24*time.Hour), 85, 85*dmRate, "claude-x", core.UsageThreadMain)
	f.move(step, day.Add(24*time.Hour+40*time.Minute), s, "in_progress", "done")
	f.move(other, day.Add(25*time.Hour), s, "open", "done")

	first, second := f.day(day), f.day(day.AddDate(0, 0, 1))
	t.Log(dmCalibrationLog(first))
	t.Log(dmCalibrationLog(second))
	require.Equal(t, []string{"plan:example-ops:migrate"}, dmSpotKeys(first), "the claim and fifty minutes before midnight")
	require.Equal(t, []string{"plan:example-ops:migrate"}, dmSpotKeys(second))
	require.True(t, second.Spotlight[0].Shipped)
	require.Equal(t, WordShipped, second.Character.Word)
	require.Equal(t, 50, first.Usage.Minutes)
	require.Equal(t, 85, second.Usage.Minutes)
	dmNoZeroMinuteInSpotlight(t, first)
	dmNoZeroMinuteInSpotlight(t, second)
}
