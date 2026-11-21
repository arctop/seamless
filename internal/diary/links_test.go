package diary

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// dmDeleteTask removes a task row as no code path does (tasks are never
// deleted): the one way to reach an unresolved task on the main handle.
func (f *dmFixture) dmDeleteTask(id string) {
	f.tb.Helper()
	f.exec(`DELETE FROM tasks WHERE id = ?`, id)
}

// An item the handle no longer returns is unresolved and keeps what its event
// recorded: a memory's name, a note's title or slug, and for a task only a
// title an event carried, which no task.transition does.
func TestDay_UnresolvedItemsKeepWhatTheEventRecorded(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour)})
	f.work(s, day, 30, 30_000, "claude-x", core.UsageThreadMain)
	gone := f.task("example-ops", "", "Gone task", day.AddDate(0, 0, -3), "-")
	f.move(gone, day.Add(5*time.Minute), s, "open", "done")
	titled := f.task("example-ops", "", "Titled task", day.AddDate(0, 0, -3), "-")
	f.event(core.EventTaskTransition, day.Add(6*time.Minute), s, "example-ops", titled,
		map[string]any{"to": "done", "from": "open", "title": "What the event said"})
	f.dmDeleteTask(gone)
	f.dmDeleteTask(titled)
	memory := f.id("MEMO")
	f.event(core.EventMemoryWritten, day.Add(7*time.Minute), s, "example-ops", memory, map[string]any{"name": "deleted-gotcha"})
	note := f.id("NOTE")
	f.event(core.EventNoteWritten, day.Add(8*time.Minute), s, "example-ops", note, map[string]any{"slug": "deleted-note", "appended": true})

	e := f.day(day)
	th := dmThread(t, e, "project:example-ops")
	require.Len(t, th.Moved, 2)
	require.Equal(t, Ref{Kind: RefTask, ID: gone, Project: "example-ops"}, th.Moved[0].Task,
		"no title recorded, so none kept; the id is still the link's")
	require.Equal(t, "open", th.Moved[0].From, "the update's own from")
	require.Equal(t, "done", th.Moved[0].To)
	require.Equal(t, Ref{Kind: RefTask, ID: titled, Title: "What the event said", Project: "example-ops"}, th.Moved[1].Task)
	require.Equal(t, []KeptItem{{Item: Ref{Kind: RefMemory, ID: memory, Title: "deleted-gotcha", Project: "example-ops"},
		New: true, Writes: 1, At: day.Add(7 * time.Minute), Sessions: []string{s}}}, e.Kept.Memories)
	require.Equal(t, Ref{Kind: RefNote, ID: note, Title: "deleted-note", Project: "example-ops"}, e.Kept.Notes[0].Item)
	require.False(t, e.Kept.Notes[0].New, "an append is no creation")
}

// A memory moved to another project names the project it is in now, and
// every memory carries its lifecycle state.
func TestDay_MemoriesCarryTheirProjectAndStateNow(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour)})
	moved := f.memory("moved-gotcha", "example-ops", day)
	f.event(core.EventMemoryWritten, day, s, "example-ops", moved, map[string]any{"name": "moved-gotcha"})
	f.exec(`UPDATE memories_index SET project = 'example-finance' WHERE id = ?`, moved)
	replaced := f.memory("replaced-rule", "example-ops", day)
	f.event(core.EventMemoryWritten, day.Add(time.Minute), s, "example-ops", replaced, map[string]any{"name": "replaced-rule"})
	newer := f.memory("newer-rule", "example-ops", day)
	f.exec(`UPDATE memories_index SET invalid_at = ?, superseded_by = ? WHERE id = ?`,
		core.FormatTime(day.Add(time.Hour)), newer, replaced)
	archived := f.memory("archived-note", "example-ops", day)
	f.event(core.EventMemoryWritten, day.Add(2*time.Minute), s, "example-ops", archived, map[string]any{"name": "archived-note"})
	f.exec(`UPDATE memories_index SET invalid_at = ? WHERE id = ?`, core.FormatTime(day.Add(time.Hour)), archived)

	e := f.day(day)
	got := map[string]Ref{}
	for _, k := range e.Kept.Memories {
		got[k.Item.ID] = k.Item
	}
	require.Equal(t, Ref{Kind: RefMemory, ID: moved, Title: "moved-gotcha", Project: "example-finance", Resolved: true,
		Status: MemoryActive}, got[moved], "the project it is in now, not the event's")
	require.Equal(t, MemorySuperseded, got[replaced].Status)
	require.Equal(t, MemoryArchived, got[archived].Status)
	require.Contains(t, dmProjectIDs(e), "example-finance")
}

// A plan has a page only where its composition's primary note lives: a
// task-only plan has none, and of two projects running one slug only the
// note's has one.
func TestDay_PlanPagesFollowTheComposition(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour)})
	f.work(s, day, 30, 30_000, "claude-x", core.UsageThreadMain)
	f.note("Plan: shared", "example-ops", []string{"plan:shared", "design"}, day.AddDate(0, 0, -5))
	f.note("Agent run", "example-finance", []string{"plan:shared", plans.TagAgent}, day.AddDate(0, 0, -6))
	f.note("Captured plan", "example-ops", []string{"plan:captured", plans.TagPlan}, day.AddDate(0, 0, -5))
	f.note("Agent only", "example-ops", []string{"plan:agents", plans.TagAgent}, day.AddDate(0, 0, -5))
	for _, p := range []struct{ project, slug string }{
		{"example-ops", "shared"}, {"example-finance", "shared"}, {"example-ops", "taskonly"},
		{"example-ops", "captured"}, {"example-ops", "agents"},
	} {
		task := f.task(p.project, p.slug, "Step of "+p.slug, day.AddDate(0, 0, -1), "-")
		f.move(task, day.Add(10*time.Minute), s, "", "in_progress")
	}

	e := f.day(day)
	page := func(key string) bool {
		th := dmThread(t, e, key)
		require.NotNil(t, th.Plan, key)
		require.Equal(t, RefPlan, th.Plan.Kind)
		require.Equal(t, th.Project, th.Plan.Project)
		return th.Plan.HasPage
	}
	require.True(t, page("plan:example-ops:shared"), "the narrative note is in example-ops")
	require.False(t, page("plan:example-finance:shared"), "the slug resolves, to the other project's note")
	require.False(t, page("plan:example-ops:taskonly"), "a task-only plan has no page")
	require.True(t, page("plan:example-ops:captured"), "a cc-plan capture is the primary")
	require.False(t, page("plan:example-ops:agents"), "agent caches alone are no composition")
}

// planPrimary chooses the primary note plans.Composition chooses, for every
// shape of composition.
func TestPlanPrimary_AgreesWithComposition(t *testing.T) {
	f := dmOpen(t, time.UTC)
	base := f.at(2026, 11, 1, 9, 0)
	f.note("Narrative", "example-ops", []string{"plan:composed"}, base)
	f.note("Later support", "example-ops", []string{"plan:composed"}, base.Add(time.Hour))
	f.note("Agent run", "example-ops", []string{"plan:composed", plans.TagAgent}, base.Add(-time.Hour))
	f.note("Capture", "example-ops", []string{"plan:captured", plans.TagPlan}, base.Add(2*time.Hour))
	f.note("Earlier narrative", "example-ops", []string{"plan:captured"}, base)
	f.note("Agents", "example-ops", []string{"plan:agents", plans.TagAgent}, base)
	f.note("Tie a", "example-ops", []string{"plan:tie"}, base)
	f.note("Tie b", "example-ops", []string{"plan:tie"}, base)
	slugs := []string{"composed", "captured", "agents", "tie", "absent"}
	tagged, err := store.DiaryPlanNotes(f.ctx, f.db, slugs)
	require.NoError(t, err)
	for _, slug := range slugs {
		want, _, wantOK, err := plans.Composition(f.ctx, f.db, slug)
		require.NoError(t, err)
		got, ok := planPrimary(tagged[slug])
		require.Equal(t, wantOK, ok, slug)
		require.Equal(t, want.ID, got.ID, slug)
	}
}

// A commit names the task or session that recorded it, and counts toward the
// thread of its owner.
func TestDay_CommitsNameTheirOwner(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	f.project("example-ops", false)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour), Findings: "Shipped the fix"})
	f.work(s, day, 20, 20_000, "claude-x", core.UsageThreadMain)
	task := f.task("example-ops", "fixes", "Fix the parser", day.AddDate(0, 0, -1), "-")
	f.move(task, day.Add(5*time.Minute), s, "open", "done")
	byTask := f.commit("task", task, s, day.Add(5*time.Minute))
	bySession := f.commit("session", s, s, day.Add(50*time.Minute))
	f.commit("session", s, s, day.AddDate(0, 0, 1))

	e := f.day(day)
	require.Len(t, e.Moments.Commits, 2, "the next day's commit is the next day's")
	c := e.Moments.Commits[0]
	require.Equal(t, RefCommit, c.Kind)
	require.Equal(t, byTask, c.ID)
	require.Equal(t, byTask[:12], c.Title)
	require.Equal(t, &Ref{Kind: RefTask, ID: task, Title: "Fix the parser", Project: "example-ops", Resolved: true, Status: "done"}, c.Owner)
	require.Equal(t, bySession, e.Moments.Commits[1].ID)
	require.Equal(t, RefSession, e.Moments.Commits[1].Owner.Kind)
	require.Equal(t, s, e.Moments.Commits[1].Owner.ID)
	th := dmThread(t, e, "plan:example-ops:fixes")
	require.Len(t, th.Commits, 2, "the task's commit by its thread, the session's by its session's")
	require.Contains(t, th.Reasons, Reason{Kind: ReasonCommits, Count: 2, Points: 2})
}

// Every project the entry names is a Ref: resolved when the handle holds it,
// marked when retired.
func TestDay_ProjectRefs(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	f.project("example-ops", false)
	f.project("example-old", true)
	for _, p := range []string{"example-ops", "example-old", "example-unregistered"} {
		s := f.session(dmSession{Project: p, Created: day, Updated: day.Add(time.Hour)})
		f.work(s, day, 15, 15_000, "claude-x", core.UsageThreadMain)
	}
	e := f.day(day)
	require.Equal(t, []Ref{
		{Kind: RefProject, ID: "example-old", Title: "example-old", Project: "example-old", Resolved: true, Retired: true},
		{Kind: RefProject, ID: "example-ops", Title: "example-ops", Project: "example-ops", Resolved: true},
		{Kind: RefProject, ID: "example-unregistered", Title: "example-unregistered", Project: "example-unregistered"},
	}, e.Projects)
}

// Sessions in no project are labeled by their working directory, and the
// global scope is "global".
func TestDay_SessionsInNoProject(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	a := f.session(dmSession{CWD: "/home/owner/scratch/", Created: day, Updated: day.Add(time.Hour)})
	f.work(a, day, 20, 20_000, "claude-x", core.UsageThreadMain)
	b := f.session(dmSession{CWD: `C:\work\tools`, Created: day, Updated: day.Add(time.Hour)})
	f.work(b, day, 20, 20_000, "claude-x", core.UsageThreadMain)
	c := f.session(dmSession{Created: day, Updated: day.Add(time.Hour)})
	f.work(c, day, 20, 20_000, "claude-x", core.UsageThreadMain)
	e := f.day(day)
	require.ElementsMatch(t, []string{"cwd:scratch", "cwd:tools", "project:"}, dmKeys(e))
	require.Equal(t, "global", dmThread(t, e, "project:").Label)
	require.Equal(t, "scratch", dmThread(t, e, "cwd:scratch").Label)
	require.Empty(t, dmThread(t, e, "cwd:scratch").Project)
	require.Equal(t, "Session in tools", dmThread(t, e, "cwd:tools").Sessions[0].About)
	require.Equal(t, 0, e.Facts.Projects, "a directory is no project")
	require.Empty(t, e.Projects)
}

// The view hides a locked project's sessions, tasks, events and ledger rows:
// its work is simply absent from the day and its totals.
func TestDay_ALockedProjectIsAbsentThroughTheView(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 9, 0)
	open := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour)})
	f.work(open, day, 30, 30_000, "claude-x", core.UsageThreadMain)
	locked := f.session(dmSession{Project: "example-finance", Created: day, Updated: day.Add(time.Hour), Findings: "secret work"})
	f.work(locked, day, 60, 60_000, "claude-x", core.UsageThreadMain)
	task := f.task("example-finance", "ledger", "Secret step", day.AddDate(0, 0, -1), "-")
	f.move(task, day.Add(10*time.Minute), locked, "open", "done")
	m := f.memory("secret-gotcha", "example-finance", day)
	f.event(core.EventMemoryWritten, day, locked, "example-finance", m, map[string]any{"name": "secret-gotcha"})
	f.commit("task", task, locked, day.Add(10*time.Minute))
	f.exec(`INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, 'retrieval.injected', ?, 'example-finance', '', '{"prompt":"a secret prompt"}')`,
		f.id("EVNT"), core.FormatTime(day), locked)

	all := f.day(day)
	require.Equal(t, 90, all.Usage.Minutes)
	require.Equal(t, 1, all.Facts.TasksClosed)

	view, err := store.OpenView(f.path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	e, err := dayAt(f.ctx, view, day, day.AddDate(1, 0, 0), time.UTC)
	require.NoError(t, err)
	require.Equal(t, []string{"project:example-ops"}, dmKeys(e))
	require.Equal(t, 30, e.Usage.Minutes)
	require.Equal(t, 1, e.Facts.Sessions)
	require.Zero(t, e.Facts.TasksClosed)
	require.Zero(t, e.Facts.Kept)
	require.Zero(t, e.Facts.PlansShipped)
	require.Empty(t, e.Moments.Commits)
	require.Len(t, e.Usage.Agents, 1)
	require.Equal(t, 30, e.Usage.Agents[0].Minutes)
	for _, p := range e.Projects {
		require.NotEqual(t, "example-finance", p.ID)
	}
	m2, err := monthAt(f.ctx, view, day, day.AddDate(1, 0, 0), time.UTC)
	require.NoError(t, err)
	require.Equal(t, e.Facts, m2.Days[17].Facts, "the month through the view agrees with the day")
}

// dmProjectIDs lists an entry's project Refs' ids.
func dmProjectIDs(e Entry) []string {
	var out []string
	for _, p := range e.Projects {
		out = append(out, p.ID)
	}
	return out
}
