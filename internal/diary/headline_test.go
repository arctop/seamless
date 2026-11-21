package diary

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// What a session was about: the first of its own findings, the day's first
// prompt, its harvested findings without the prefix, and the default.
func TestAbout_TheFallbackChain(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, c := range []struct {
		name, findings, prompt string
		want, from             string
	}{
		{"own findings first", "## Fixed the parser\n\nDetails follow.", "fix the parser", "Fixed the parser", AboutFindings},
		{"own findings over a prompt, first line with words", "\n\n- Shipped **v2**\n- more", "ship", "Shipped v2", AboutFindings},
		{"the prompt when there are no findings", "", "Why does the build fail?\nmore", "Why does the build fail?", AboutPrompt},
		{"the prompt over harvested findings", "(auto-harvested) Answered it", "what is this", "what is this", AboutPrompt},
		{"harvested findings without the prefix", "(auto-harvested) Answered the question.", "", "Answered the question.", AboutHarvested},
		{"the no-summary sentinel is nothing", core.FindingNoSummary, "", "Session in example-ops", AboutDefault},
		{"the default", "", "", "Session in example-ops", AboutDefault},
		{"blank lines and markup only fall through", "---\n\n", "  \n", "Session in example-ops", AboutDefault},
		{"capped on a word", long, "", strings.TrimSpace(core.TruncateWords(strings.TrimSpace(long), AboutMaxRunes)), AboutFindings},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, from := about(c.findings, c.prompt, "example-ops")
			require.Equal(t, c.want, got)
			require.Equal(t, c.from, from)
		})
	}
}

// Each character word needs its threshold, checked in order, at the threshold
// and one below it.
func TestCharacter_Thresholds(t *testing.T) {
	for _, c := range []struct {
		name string
		f    Facts
		want string
	}{
		{"nothing at all", Facts{}, WordQuiet},
		{"a plan shipped", Facts{Sessions: 1, PlansShipped: 1, TasksClosed: 9}, WordShipped},
		{"closing at the threshold", Facts{Sessions: 1, TasksClosed: ClosingDayTasks}, WordClosing},
		{"one under closing", Facts{Sessions: 1, TasksClosed: ClosingDayTasks - 1}, WordLight},
		{"a plan drafted", Facts{Sessions: 1, PlansDrafted: 1}, WordPlanning},
		{"tasks created at the threshold", Facts{Sessions: 1, TasksCreated: PlanningDayTasks}, WordPlanning},
		{"one under the tasks created", Facts{Sessions: 1, TasksCreated: PlanningDayTasks - 1}, WordLight},
		{"planning closed too much", Facts{Sessions: 1, PlansDrafted: 1, TasksClosed: PlanningDayMaxClosed}, WordLight},
		{"deep work at both thresholds", Facts{Sessions: 2, Minutes: DeepWorkMinutes, TopShare: DeepWorkShare}, WordDeep},
		{"deep work spread too thin", Facts{Sessions: 2, Minutes: DeepWorkMinutes, TopShare: DeepWorkShare - 1}, WordLight},
		{"deep work one minute short", Facts{Sessions: 2, Minutes: DeepWorkMinutes - 1, TopShare: 100}, WordLight},
		{"busy by minutes", Facts{Sessions: 2, Minutes: BusyDayMinutes, TopShare: 30}, WordBusy},
		{"busy by sessions", Facts{Sessions: BusyDaySessions}, WordBusy},
		{"short sessions are not busy", Facts{Sessions: BusyDaySessions + 20, Short: 21}, WordLight},
		{"busy by projects worked in", Facts{Sessions: 4, Projects: BusyDayProjects, WorkProjects: BusyDayProjects}, WordBusy},
		{"projects only short sessions touched are not busy", Facts{Sessions: 6, Projects: 6, WorkProjects: BusyDayProjects - 1}, WordLight},
		{"one under busy", Facts{Sessions: BusyDaySessions - 1, WorkProjects: BusyDayProjects - 1, Minutes: BusyDayMinutes - 1}, WordLight},
		{"deep beats busy", Facts{Sessions: 20, Minutes: 400, TopShare: 80}, WordDeep},
		{"an item kept is not quiet", Facts{Kept: 1}, WordLight},
	} {
		t.Run(c.name, func(t *testing.T) {
			tag := Character(c.f)
			require.Equal(t, c.want, tag.Word)
			require.NotEmpty(t, tag.Rule)
		})
	}
	require.Equal(t, "2h or more of agent time, 60% or more of it in one project",
		Character(Facts{Sessions: 1, Minutes: 200, TopShare: 70}).Rule, "the rule prints the thresholds verbatim")
	// A working day reads as one: two hours in one project is deep work, and
	// three hours spread over projects is busy, not light.
	require.Equal(t, WordDeep, Character(Facts{Sessions: 2, Minutes: 145, TopShare: 91, WorkProjects: 3}).Word)
	require.Equal(t, WordBusy, Character(Facts{Sessions: 4, Minutes: 211, TopShare: 51, WorkProjects: 2}).Word)
	require.Equal(t, WordLight, Character(Facts{Sessions: 1, Minutes: 25, TopShare: 100, WorkProjects: 1}).Word)
}

// The headline is composed from the facts, deterministically.
func TestHeadline(t *testing.T) {
	plan := Thread{Key: "plan:example-ops:rollout", Kind: ThreadPlan, Label: "rollout", Project: "example-ops", Minutes: 300,
		Moved:    []Move{{Closed: true, From: "open", To: "done"}, {Closed: true, From: "in_progress", To: "done"}, {From: "open", To: "in_progress"}},
		Progress: &Progress{Start: Count{10, 14}, End: Count{12, 14}}}
	side := Thread{Key: "project:example-finance", Kind: ThreadProject, Label: "example-finance", Project: "example-finance", Minutes: 100}
	for _, c := range []struct {
		name string
		e    Entry
		want string
	}{
		{"quiet", Entry{}, "Nothing recorded."},
		{"quiet today", Entry{Today: true}, "Nothing recorded so far."},
		{"mostly one plan", Entry{Facts: Facts{Sessions: 14, Projects: 6, Minutes: 400}, Spotlight: []Thread{plan, side}},
			"Mostly rollout in example-ops: 2 steps closed, the plan is now at 12 of 14. 14 sessions across 6 projects."},
		{"led, not mostly: it counts the things worked on", Entry{Facts: Facts{Sessions: 3, Projects: 2, Minutes: 900},
			Spotlight: []Thread{plan, side}},
			"Led by rollout in example-ops: 2 steps closed, the plan is now at 12 of 14. 3 sessions across 2 projects worked on 2 things."},
		{"led today", Entry{Today: true, Facts: Facts{Sessions: 4, Projects: 2, Minutes: 900},
			Spotlight: []Thread{plan}, Also: []Thread{side, {Kind: ThreadProject, Label: "example-api", Project: "example-api"}}},
			"Led by rollout in example-ops: 2 steps closed, the plan is now at 12 of 14. 4 sessions across 2 projects worked on 3 things so far."},
		{"shipped today, its project named once", Entry{Today: true, Facts: Facts{Sessions: 1, Projects: 1, Minutes: 300},
			Usage: Usage{Projects: []ProjectUsage{{Project: "example-ops"}}},
			Spotlight: []Thread{{Kind: ThreadPlan, Label: "rollout", Project: "example-ops", Minutes: 300, Shipped: true,
				Moved: []Move{{Closed: true}}, Progress: &Progress{End: Count{14, 14}}}}},
			"Mostly rollout in example-ops: 1 step closed and the plan shipped (14 of 14). 1 session so far."},
		{"a plan at rest", Entry{Facts: Facts{Sessions: 2, Projects: 2, Minutes: 120},
			Spotlight: []Thread{{Kind: ThreadPlan, Label: "rollout", Project: "example-ops", Minutes: 120,
				Moved: []Move{{From: "in_progress", To: "in_progress"}}, Progress: &Progress{Start: Count{4, 7}, End: Count{4, 7}}}}},
			"Mostly rollout in example-ops: the plan is at 4 of 7. 2 sessions across 2 projects."},
		{"a project thread with items, named once", Entry{Facts: Facts{Sessions: 2, Projects: 1, Minutes: 40},
			Usage: Usage{Projects: []ProjectUsage{{Project: "example-ops"}}},
			Also:  []Thread{{Kind: ThreadProject, Label: "example-ops", Project: "example-ops", Minutes: 40, Kept: []Ref{{}, {}}}}},
			"Mostly example-ops: 2 items kept. 2 sessions."},
		{"one project the lead does not name", Entry{Facts: Facts{Sessions: 1, Projects: 1, Minutes: 40},
			Usage:     Usage{Projects: []ProjectUsage{{Project: "example-web"}}},
			Spotlight: []Thread{{Kind: ThreadProject, Label: "scratch", Minutes: 40}}},
			"Mostly scratch: 40m of agent time. 1 session in example-web."},
		{"only effort", Entry{Facts: Facts{Sessions: 1, Minutes: 95}, Spotlight: []Thread{{Kind: ThreadProject, Label: "scratch", Minutes: 95}}},
			"Mostly scratch: 1h 35m of agent time. 1 session."},
		{"only short sessions", Entry{Facts: Facts{Sessions: 4, Projects: 2, Minutes: 12}},
			"Only short sessions: 12m of agent time. 4 sessions across 2 projects."},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, Headline(c.e))
		})
	}
}

func TestDuration(t *testing.T) {
	for minutes, want := range map[int]string{0: "0m", 45: "45m", 60: "1h", 61: "1h 1m", 400: "6h 40m"} {
		require.Equal(t, want, Duration(minutes))
	}
}
