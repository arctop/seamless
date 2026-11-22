package console

// A task's own page (itempage.go has the frame every item page shares).

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// --- tasks ------------------------------------------------------------------

// taskItem is a task's own page: the detail the reader shows (its actions
// returning to this page), and its plan as a mini card.
type taskItem struct {
	*taskDetail
	Plan *itemPlanCard
}

// taskItemPage answers a task's own URL in HTML: the item page, its place in
// the Tasks list the URL names (q; listed when it names one), else in its
// plan's steps, else in its project's tasks.
func (s *Service) taskItemPage(w http.ResponseWriter, r *http.Request, t core.Task, d taskDetail, q taskQuery) {
	ctx := r.Context()
	now := time.Now().UTC()
	listed := q.QS() != ""
	d.Next = q.listNext(d.ID)
	self := "/console/tasks/" + d.ID + q.QS()
	page := &itemPage{
		Kind: "task", ID: d.ID, Title: taskPageTitle(t),
		Star: itemStar{Action: "/console/favorites/task/" + d.ID, Next: self, On: d.Favorite, Noun: "task"},
	}
	item := &taskItem{taskDetail: &d}
	if t.PlanSlug != "" {
		card, err := s.taskPlanCard(ctx, t, now)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		item.Plan = card
	}
	if listed {
		place, found, err := store.TaskPlace(ctx, s.db(ctx), q.filter(), q.Sort == taskSortNewest, now, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return taskListWords(q, n) }, "/console/tasks", q.QS())
		} else {
			listed = false
		}
	}
	switch {
	case page.Place != nil:
	case item.Plan != nil && item.Plan.current >= 0:
		page.Place = item.Plan.place()
	case t.PlanSlug == "":
		f := store.TaskFilter{ByProject: true, Project: t.ProjectSlug}
		place, found, err := store.TaskPlace(ctx, s.db(ctx), f, false, now, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			words := func(n int) string {
				return nounOf(n, "task", "tasks") + scopeWords(t.ProjectSlug, true, "in the global scope")
			}
			page.Place = placeOf(place, words, "/console/tasks", "")
		}
	}

	project := scopeParam(t.ProjectSlug)
	tasksHere := "/console/tasks?project=" + url.QueryEscape(project)
	switch {
	case listed:
		page.Back = frameLink{Label: taskListBack(q), Href: q.href("/console/tasks")}
	case t.PlanSlug != "":
		page.Back = frameLink{Label: "plan:" + t.PlanSlug, Href: planHref(t.ProjectSlug, t.PlanSlug), Mono: true}
	case t.ProjectSlug == "":
		page.Back = frameLink{Label: "Tasks in the global scope", Href: tasksHere}
	default:
		page.Back = frameLink{Label: t.ProjectSlug + "’s tasks", Href: tasksHere}
	}
	if t.PlanSlug != "" {
		page.Crumbs = []frameLink{
			s.projectCrumb(ctx, t.ProjectSlug, tasksHere, "global"),
			{Label: "Plans", Href: "/console/plans?project=" + url.QueryEscape(project)},
			{Label: "plan:" + t.PlanSlug, Href: planHref(t.ProjectSlug, t.PlanSlug), Mono: true},
		}
	} else {
		page.Crumbs = []frameLink{s.projectCrumb(ctx, t.ProjectSlug, tasksHere, "global"), {Label: "Tasks", Href: tasksHere}}
	}
	page.Task = item
	s.render(w, r, "tasks", pageData{Title: taskPageTitle(t), Active: "tasks", Data: tasksData{Item: page}})
}

// taskListWords names a Tasks list of n: "tasks in example-ops", "steps of
// plan:x matching "y"".
func taskListWords(q taskQuery, n int) string {
	var b strings.Builder
	switch {
	case q.Plan != "":
		b.WriteString(nounOf(n, "step", "steps") + " of plan:" + q.Plan)
	case q.Steps:
		b.WriteString(nounOf(n, "task and step", "tasks and steps"))
	default:
		b.WriteString(nounOf(n, "task", "tasks"))
	}
	switch q.Project {
	case "":
	case validate.GlobalProject:
		b.WriteString(" in the global scope")
	default:
		b.WriteString(" in " + q.Project)
	}
	b.WriteString(queryWords(q.Q))
	return b.String()
}

// taskListBack is the way back to the Tasks list q names.
func taskListBack(q taskQuery) string {
	switch {
	case q.Plan != "":
		return "Steps of plan:" + q.Plan
	case q.Project == validate.GlobalProject:
		return "Tasks in the global scope"
	case q.Project != "":
		return q.Project + "’s tasks"
	default:
		return "Tasks"
	}
}

// taskPlanCard reads t's plan for its mini card, through the request's
// handle: the plan's steps in its project worded as the plan card words them
// (planStep), the head's counts, and the narrative note's title.
func (s *Service) taskPlanCard(ctx context.Context, t core.Task, now time.Time) (*itemPlanCard, error) {
	db := s.db(ctx)
	steps, err := store.ListTasksForPlan(ctx, db, t.ProjectSlug, "", t.PlanSlug)
	if err != nil {
		return nil, err
	}
	card := &itemPlanCard{Slug: t.PlanSlug, Title: t.PlanSlug, Href: planHref(t.ProjectSlug, t.PlanSlug), current: -1}
	primary, _, ok, err := plans.CompositionIn(ctx, db, t.ProjectSlug, t.PlanSlug)
	if err != nil {
		return nil, err
	}
	if ok && strings.TrimSpace(primary.Title) != "" {
		card.Title = primary.Title
	}
	cache := make(map[string]core.Task, len(steps))
	for _, st := range steps {
		cache[st.ID] = st
	}
	lockedBy, err := s.lockedBlockersOf(ctx, cache)
	if err != nil {
		return nil, err
	}
	pc := planPageCtx(steps, cache, lockedBy, &planInputs{namer: s.sessionNamer(ctx), now: now}, card.Href)
	rows := make([]planStepVM, 0, len(steps))
	inFlight := 0
	for _, st := range steps {
		row := s.planStep(ctx, st, pc)
		rows = append(rows, row)
		switch {
		case st.Status.Closed():
			card.Done++
		case st.Status == core.TaskInProgress:
			inFlight++
		}
	}
	card.Total = len(rows)
	card.DonePct, card.DoingPct = percent(card.Done, card.Total), percent(inFlight, card.Total)
	card.Counts = planCounts(rows)
	// The plan as it went: its closed steps in the order they closed, then
	// the unfinished ones in the plan card's dependency order (groupSteps).
	var done, live []planStepVM
	for _, g := range groupSteps(rows) {
		for _, st := range g.Steps {
			if st.State == "done" {
				done = append(done, st)
			} else {
				live = append(live, st)
			}
		}
	}
	slices.Reverse(done)
	for _, st := range slices.Concat(done, live) {
		if st.ID == t.ID {
			card.current = len(card.Steps)
		}
		card.Steps = append(card.Steps, itemPlanStep{ID: st.ID, Title: st.Title, Word: st.Word, Current: st.ID == t.ID})
	}
	return card, nil
}
