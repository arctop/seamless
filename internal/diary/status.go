package diary

import (
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A task's status at an instant is replayed from its task.transition history
// (store.DiaryTransitions): the status the last transition before the instant
// moved it to. Before its first recorded transition a task had the status
// that transition moved it from: the from every tasks_update records, open
// for a claim, in_progress for a release, nothing for a creation (it did not
// exist yet). A task with no history at all (seeded or imported) reads from
// its row: closed from closed_at on, open before, and its status now when it
// was never closed.

// closed reports whether a status is terminal: done or dropped, the plan
// rollups' closed (store.planRollups).
func closed(status string) bool { return core.TaskStatus(status).Closed() }

// statusBeforeFirst is a task's status before its first recorded transition
// first: "" when that transition created it.
func statusBeforeFirst(first store.DiaryTransition) string {
	switch {
	case first.From != "":
		return first.From
	case first.Created:
		return ""
	case first.Claimed:
		return string(core.TaskOpen)
	case first.Released:
		return string(core.TaskInProgress)
	}
	return string(core.TaskOpen)
}

// statusBefore is task t's status just before at, replayed from history (its
// transitions, oldest first): "" when it did not exist then.
func statusBefore(t store.DiaryTask, history []store.DiaryTransition, at time.Time) string {
	if !t.Created.Before(at) {
		return ""
	}
	last := ""
	for _, tr := range history {
		if !tr.At.Before(at) {
			break
		}
		if tr.To != "" {
			last = tr.To
		}
	}
	if last != "" {
		return last
	}
	if len(history) > 0 {
		// It existed before at, and its first transition is at or after it.
		if s := statusBeforeFirst(history[0]); s != "" {
			return s
		}
		return string(core.TaskOpen)
	}
	if t.Closed != nil && !t.Closed.Before(at) {
		return string(core.TaskOpen)
	}
	return string(t.Status)
}

// progressAt counts a plan's steps closed of all just before at.
func (s *span) progressAt(steps []string, at time.Time) Count {
	var c Count
	for _, id := range steps {
		t, ok := s.tasks[id]
		if !ok {
			continue
		}
		st := statusBefore(t, s.history[id], at)
		if st == "" {
			continue
		}
		c.Total++
		if closed(st) {
			c.Closed++
		}
	}
	return c
}

// shippedOn reports whether plan p shipped in [lo, hi): a transition in the
// range closed one of its steps as done while every other step that existed
// then was already closed -- milestones.go's checkPlanShipped test, judged at
// the moment of each close, in event order, for the plan keyed by its project
// and slug. A plan whose last open step was dropped completed without
// shipping. events is the range's events, oldest first.
func (s *span) shippedOn(p store.PlanRef, events []store.DiaryEvent, lo time.Time) bool {
	steps := s.steps[p]
	if len(steps) == 0 {
		return false
	}
	in := make(map[string]bool, len(steps))
	state := make(map[string]string, len(steps))
	for _, id := range steps {
		in[id] = true
		if t, ok := s.tasks[id]; ok {
			if st := statusBefore(t, s.history[id], lo); st != "" {
				state[id] = st
			}
		}
	}
	for _, e := range events {
		if e.Kind != core.EventTaskTransition || !in[e.ItemID] || e.To == "" {
			continue
		}
		prev, existed := state[e.ItemID]
		state[e.ItemID] = e.To
		if e.To != string(core.TaskDone) || (existed && closed(prev)) {
			continue
		}
		// A step created after lo with no transition yet still counts once it
		// exists: a seeded or imported row records none.
		all := true
		for _, id := range steps {
			st, ok := state[id]
			if !ok {
				t, known := s.tasks[id]
				if !known || t.Created.After(e.At) {
					continue
				}
				st = statusBefore(t, s.history[id], e.At)
				if st == "" {
					continue
				}
			}
			if !closed(st) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
