package diary

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// dayKinds are the events the day model reads.
var dayKinds = []core.EventKind{
	core.EventTaskTransition,
	core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved, core.EventPlanShipped,
	core.EventMemoryWritten, core.EventNoteWritten, core.EventTrialRecorded,
	core.EventAgentMishap, core.EventSessionFlagged,
}

// credits reports whether an event credits its session with activity on the
// day it was recorded: the agent's own work does, a flag does not (the owner
// may flag a session long after it ended). The transport events (tool.call,
// hook.prompt, retrieval.injected) are left out on purpose: a month of them
// costs more to read than everything else together, and the ledger already
// says when a session was at work.
func credits(e store.DiaryEvent) bool {
	return e.SessionID != "" && e.Kind != core.EventSessionFlagged
}

// span is one range of whole local days as the store holds it: the rows every
// day of it is composed from. Day reads a span of one day, Month one of a
// month, Usual one of its lookback; every Facts comes from the same span
// rules, so the three agree.
type span struct {
	lo, hi time.Time
	loc    *time.Location
	starts []time.Time // the days' midnights

	usage   []store.UsageRow
	events  []store.DiaryEvent
	commits []store.DiaryCommit
	// around is the sessions created or last updated in the span; sessions is
	// every session the span names (around, the ledger's and the events'),
	// keyed by id. An id the handle does not hold is absent.
	around   []store.DiarySession
	sessions map[string]store.DiarySession
	// tasks is every task the span's transitions name, and with plans every
	// step of the plans they and the plan events name; steps lists each such
	// plan's steps; history is each of those tasks' transitions before hi,
	// oldest first. All three are nil without plans.
	tasks   map[string]store.DiaryTask
	steps   map[store.PlanRef][]string
	history map[string][]store.DiaryTransition
}

// load reads the span [lo, hi) of whole local days. withPlans also reads the
// plans' steps and the tasks' histories, which plan progress, shipping and a
// move's status at the day's start need.
func load(ctx context.Context, db *sql.DB, lo, hi time.Time, loc *time.Location, withPlans bool) (*span, error) {
	s := &span{lo: lo, hi: hi, loc: loc, starts: dayStarts(lo, hi)}
	var err error
	if s.usage, err = store.UsageBetween(ctx, db, lo, hi); err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	if s.events, err = store.DiaryEvents(ctx, db, lo, hi, dayKinds); err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	if s.around, err = store.DiarySessionsBetween(ctx, db, lo, hi); err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	if s.commits, err = store.DiaryCommits(ctx, db, lo, hi); err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	s.sessions = make(map[string]store.DiarySession, len(s.around))
	for _, ss := range s.around {
		s.sessions[ss.ID] = ss
	}
	var missing []string
	need := func(id string) {
		if _, ok := s.sessions[id]; !ok && id != "" {
			missing = append(missing, id)
		}
	}
	for _, r := range s.usage {
		need(r.SessionID)
	}
	var taskIDs []string
	for _, e := range s.events {
		if credits(e) {
			need(e.SessionID)
		}
		if e.Kind == core.EventTaskTransition && e.ItemID != "" {
			taskIDs = append(taskIDs, e.ItemID)
		}
	}
	if len(missing) > 0 {
		more, err := store.DiarySessionsByIDs(ctx, db, missing)
		if err != nil {
			return nil, fmt.Errorf("diary: %w", err)
		}
		maps.Copy(s.sessions, more)
	}
	if s.tasks, err = store.DiaryTasksByIDs(ctx, db, taskIDs); err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	if !withPlans {
		return s, nil
	}
	plans := map[store.PlanRef]bool{}
	for _, e := range s.events {
		switch e.Kind {
		case core.EventTaskTransition:
			if p, ok := s.planOf(e); ok {
				plans[p] = true
			}
		case core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved:
			if e.PlanSlug != "" {
				plans[store.PlanRef{Project: e.Project, Slug: e.PlanSlug}] = true
			}
		}
	}
	refs := slices.SortedFunc(maps.Keys(plans), comparePlans)
	steps, err := store.DiaryPlanSteps(ctx, db, refs)
	if err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	s.steps = make(map[store.PlanRef][]string, len(refs))
	for _, t := range steps {
		s.tasks[t.ID] = t
		p := store.PlanRef{Project: t.Project, Slug: t.Plan}
		s.steps[p] = append(s.steps[p], t.ID)
	}
	for _, ids := range s.steps {
		slices.Sort(ids)
	}
	history, err := store.DiaryTransitions(ctx, db, slices.Collect(maps.Keys(s.tasks)), hi)
	if err != nil {
		return nil, fmt.Errorf("diary: %w", err)
	}
	s.history = make(map[string][]store.DiaryTransition, len(s.tasks))
	for _, t := range history {
		s.history[t.TaskID] = append(s.history[t.TaskID], t)
	}
	return s, nil
}

// planOf is the plan a task.transition's task is a step of: the task as it is
// now, or for a task the handle no longer holds, the plan the event named (a
// plan's tracking task records it). ok is false outside any plan.
func (s *span) planOf(e store.DiaryEvent) (store.PlanRef, bool) {
	if t, ok := s.tasks[e.ItemID]; ok {
		return store.PlanRef{Project: t.Project, Slug: t.Plan}, t.Plan != ""
	}
	return store.PlanRef{Project: e.Project, Slug: e.PlanSlug}, e.PlanSlug != ""
}

// comparePlans orders plans by project, then slug.
func comparePlans(a, b store.PlanRef) int {
	if a.Project != b.Project {
		if a.Project < b.Project {
			return -1
		}
		return 1
	}
	switch {
	case a.Slug < b.Slug:
		return -1
	case a.Slug > b.Slug:
		return 1
	}
	return 0
}

// dayRows is one day of a span: its bounds and its rows.
type dayRows struct {
	lo, hi  time.Time
	usage   []store.UsageRow
	events  []store.DiaryEvent
	commits []store.DiaryCommit
	// started and updated are the sessions created, and last updated, on the
	// day.
	started, updated []string
}

// split cuts the span into its days.
func (s *span) split() []dayRows {
	out := make([]dayRows, len(s.starts))
	for i, lo := range s.starts {
		out[i].lo = lo
		if i+1 < len(s.starts) {
			out[i].hi = s.starts[i+1]
		} else {
			out[i].hi = s.hi
		}
	}
	for _, r := range s.usage {
		if i := dayIndex(s.starts, s.hi, r.Bucket); i >= 0 {
			out[i].usage = append(out[i].usage, r)
		}
	}
	for _, e := range s.events {
		if i := dayIndex(s.starts, s.hi, e.At); i >= 0 {
			out[i].events = append(out[i].events, e)
		}
	}
	for _, c := range s.commits {
		if i := dayIndex(s.starts, s.hi, c.At); i >= 0 {
			out[i].commits = append(out[i].commits, c)
		}
	}
	for _, ss := range s.around {
		if i := dayIndex(s.starts, s.hi, ss.Created); i >= 0 {
			out[i].started = append(out[i].started, ss.ID)
		}
		if i := dayIndex(s.starts, s.hi, ss.Updated); i >= 0 {
			out[i].updated = append(out[i].updated, ss.ID)
		}
	}
	return out
}
