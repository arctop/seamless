package console

// An item's own page (plan:console-ux-pass UX 26): the one reading frame a
// memory, a note and a task open in from a link, without the library beside
// it. The page draws the item and nothing else: the way back, its place in a
// list (k / j), where it lives, its title, actions, lead and facts, its body
// in one reading column, and a context column for what is around it. Every
// read goes through the request's handle (s.db), and the item's place is one
// scoped query (store/itempage.go), never the list it is in.
//
// The frame is templates/item_shared.html's item-frame; each page's own
// template (memories.html, notes.html, tasks.html) fills its kind-specific
// blocks (item-banner, item-actions, item-facts, item-main, item-side). JSON,
// ?peek=1 and ?reader=1 answer as they always did (peek.go).

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/markdown"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// itemPage is an item's own page: what the frame draws for every kind, and
// the kind's own view (Memory, Note or Task: one of them is set).
type itemPage struct {
	// Kind is memory, note or task: the page's data-item, and the noun its
	// labels use.
	Kind string
	ID   string
	// Title is the name (a memory, in the mono face: Mono) or the title (a
	// note, a task, in the serif face); Lead is the description under it.
	Title string
	Mono  bool
	Lead  string
	// Back is the way back: the collection, scoped to the item's project, or
	// the list the URL names. Browser Back is left alone.
	Back frameLink
	// Place is the item's place in its list, with the previous and next
	// items (k / j); nil when it has none.
	Place *itemPlace
	// Crumbs say where the item lives, each a link to the collection scoped
	// to it.
	Crumbs []frameLink
	// Star is the title's star.
	Star   itemStar
	Memory *memoryItem
	Note   *noteItem
	Task   *taskItem
}

// frameLink is one link the frame draws: the way back, a crumb, a neighbour.
// Mono sets it in the mono face (a plan's slug); Accent in the kind's color
// (a memory's kind crumb).
type frameLink struct {
	Label  string
	Href   string
	Mono   bool
	Accent bool
}

// itemPlace is the item's place in its list: Text says it ("3 of 12
// runbooks in example-ops"), Prev and Next are its neighbours (nil at an end).
type itemPlace struct {
	Text       string
	Prev, Next *frameLink
}

// itemStar is the title's star: the favorites route, the page it returns to
// (this one), whether the item is starred, and its noun.
type itemStar struct {
	Action string
	Next   string
	On     bool
	Noun   string
}

// itemMoreLimit is how many other items a context column lists.
const itemMoreLimit = 5

// placeOf turns a store place into the frame's: the words for the list
// (noun, given the list's length) and the neighbours' links, each the item
// path with the list's query (qs: "" for the default list).
func placeOf(p store.ItemPlace, words func(total int) string, path, qs string) *itemPlace {
	out := &itemPlace{Text: fmt.Sprintf("%d of %d %s", p.Pos, p.Total, words(p.Total))}
	if p.Prev.ID != "" {
		out.Prev = &frameLink{Label: p.Prev.Label, Href: path + "/" + p.Prev.ID + qs}
	}
	if p.Next.ID != "" {
		out.Next = &frameLink{Label: p.Next.Label, Href: path + "/" + p.Next.ID + qs}
	}
	return out
}

// scopeParam is a project as a list's ?project= names it: its slug, or
// global for the global scope.
func scopeParam(project string) string {
	if project == "" {
		return validate.GlobalProject
	}
	return project
}

// listHref is a collection's list URL scoped to project, with kind when one
// is given, and the presentation (sort, grouping) of keep.
func listHref(path, project, kind string, keep libQuery) string {
	q := libQuery{Sort: keep.Sort, Group: keep.Group, Project: scopeParam(project), Kind: kind}
	if q.Sort == "" {
		q.Sort = libDefaultSort
	}
	return q.href(path)
}

// projectCrumb is the crumb for an item's project: the project's own page
// when this request reads one, else its collection scoped to it. The global
// scope has no page: its crumb is the collection's global scope, named as
// the collection names it (global).
func (s *Service) projectCrumb(ctx context.Context, project, scoped, global string) frameLink {
	if project == "" {
		return frameLink{Label: global, Href: scoped}
	}
	if _, ok, err := store.ProjectBySlug(ctx, s.db(ctx), project); err != nil {
		s.logger.Warn("console: item page project", "project", project, "error", err)
	} else if ok {
		return frameLink{Label: project, Href: "/console/projects/" + url.PathEscape(project)}
	}
	return frameLink{Label: project, Href: scoped}
}

// scopeWords names a list's project scope: " in example-ops", " shared
// globally", or nothing for every project.
func scopeWords(project string, byProject bool, global string) string {
	switch {
	case !byProject:
		return ""
	case project == "":
		return " " + global
	default:
		return " in " + project
	}
}

// queryWords names a list's text filter: ` matching "x"`, or nothing.
func queryWords(q string) string {
	if q == "" {
		return ""
	}
	return " matching “" + q + "”"
}

// --- memories ---------------------------------------------------------------

// memoryItem is a memory's own page: the reader's view (the detail, the
// sessions it surfaced to, the list query its links carry) and the context
// column's other memories of its kind in its project.
type memoryItem struct {
	*memoryReader
	// More is up to itemMoreLimit other active memories of its kind in its
	// project, newest first; MoreTitle heads them and MoreAll links to all
	// MoreTotal of them.
	More      []memoryRef
	MoreTitle string
	MoreAll   frameLink
	MoreTotal int
}

// memoryNoun is what a list of n memories of kind calls them.
func memoryNoun(kind string, n int) string {
	switch kind {
	case "":
		return nounOf(n, "memory", "memories")
	case string(core.KindRefuted):
		return nounOf(n, "refuted memory", "refuted memories")
	default:
		return nounOf(n, kind, kind+"s")
	}
}

// nounOf is the noun for n of a thing, one or many, without the number.
func nounOf(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// memoryItemPage answers a memory's own URL in HTML: the item page, its
// place in the list the URL names (lq; listed when it names one), else in
// its project and kind.
func (s *Service) memoryItemPage(w http.ResponseWriter, r *http.Request, d memoryDetail, lq libQuery) {
	ctx := r.Context()
	listed := lq.QS() != ""
	sel := s.memoryReaderOf(ctx, d, lq)
	active := d.Status == "active"
	kinds := make([]string, 0, len(core.MemoryKinds))
	for _, k := range core.MemoryKinds {
		kinds = append(kinds, string(k))
	}
	page := &itemPage{
		Kind: "memory", ID: d.ID, Title: d.Name, Mono: true, Lead: d.Description,
		Star: itemStar{Action: "/console/favorites/memory/" + d.ID, Next: memoriesListPath + "/" + d.ID + lq.QS(),
			On: d.Favorite, Noun: "memory"},
	}

	// The place: in the list the URL names, when it holds the memory; else in
	// the memory's project and kind, newest first.
	if listed {
		l := store.MemoryList{
			ByProject: lq.Project != "", Project: lq.Project, Kind: lq.Kind, Fav: lq.Fav,
			Never: lq.Surfaced == "never", Query: strings.ToLower(lq.Q), Inactive: !active,
			Sort: lq.Sort, Tree: active && lq.tree(), Kinds: kinds,
		}
		if l.Project == validate.GlobalProject {
			l.Project = ""
		}
		place, found, err := store.MemoryPlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return memoryListWords(l, n) }, memoriesListPath, lq.QS())
		} else {
			listed = false
		}
	}
	if page.Place == nil {
		l := store.MemoryList{ByProject: true, Project: d.Project, Kind: d.Kind, Inactive: !active, Sort: libDefaultSort}
		place, found, err := store.MemoryPlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return memoryListWords(l, n) }, memoriesListPath, "")
		}
	}

	// The way back and the crumbs.
	scoped := listHref(memoriesListPath, d.Project, "", lq)
	page.Back = frameLink{Label: memoryBackLabel(d.Project), Href: listHref(memoriesListPath, d.Project, "", libQuery{})}
	if listed {
		page.Back = frameLink{Label: memoryListBack(lq), Href: lq.href(memoriesListPath)}
	}
	page.Crumbs = []frameLink{
		s.projectCrumb(ctx, d.Project, scoped, "Shared globally"),
		{Label: "Memories", Href: scoped},
		{Label: memoryNoun(d.Kind, 2), Href: listHref(memoriesListPath, d.Project, d.Kind, lq), Accent: true},
	}

	item := &memoryItem{memoryReader: sel, MoreTitle: "More " + memoryNoun(d.Kind, 2) + scopeWords(d.Project, true, "shared globally")}
	more, total, err := store.MemoriesOfKind(ctx, s.db(ctx), d.Project, d.Kind, d.ID, itemMoreLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, m := range more {
		item.More = append(item.More, memoryRef{ID: m.ID, Name: m.Name, Kind: string(m.Kind), Project: m.Project})
	}
	item.MoreTotal = total
	item.MoreAll = frameLink{
		Label: fmt.Sprintf("All %d %s%s", total, memoryNoun(d.Kind, total), scopeWords(d.Project, true, "shared globally")),
		Href:  listHref(memoriesListPath, d.Project, d.Kind, libQuery{}),
	}
	page.Memory = item
	s.render(w, r, "memories", pageData{Title: "Memory " + d.Name, Active: "memories", Data: memoriesData{Item: page}})
}

// memoryListWords names a Memories list of n: "runbooks in example-ops",
// "starred memories", "archived or superseded gotchas matching "x"".
func memoryListWords(l store.MemoryList, n int) string {
	var b strings.Builder
	if l.Inactive {
		b.WriteString("archived or superseded ")
	}
	if l.Fav {
		b.WriteString("starred ")
	}
	b.WriteString(memoryNoun(l.Kind, n))
	if l.Never {
		b.WriteString(" never surfaced")
	}
	b.WriteString(scopeWords(l.Project, l.ByProject, "shared globally"))
	b.WriteString(queryWords(l.Query))
	return b.String()
}

// memoryBackLabel is the way back to a memory's project's memories.
func memoryBackLabel(project string) string {
	if project == "" {
		return "Memories shared globally"
	}
	return project + "’s memories"
}

// memoryListBack is the way back to the Memories list lq names.
func memoryListBack(lq libQuery) string {
	noun := memoryNoun(lq.Kind, 2)
	switch lq.Project {
	case "":
		return strings.ToUpper(noun[:1]) + noun[1:]
	case validate.GlobalProject:
		return strings.ToUpper(noun[:1]) + noun[1:] + " shared globally"
	default:
		return lq.Project + "’s " + noun
	}
}

// --- notes ------------------------------------------------------------------

// noteItem is a note's own page: the reader's view (the detail, its tags as
// a person reads them, the list query its star carries), its body with an id
// on each section heading and those headings as Outline, the plans it is
// part of with their progress, and other notes in its project.
type noteItem struct {
	*noteReader
	Outline []markdown.Heading
	PartOf  []itemPlanPart
	// Minutes is the reading time, at itemReadingPace words a minute.
	Minutes int
	// More is up to itemMoreLimit other notes in its project, newest first;
	// MoreTitle heads them and MoreAll links to all MoreTotal of them.
	More      []noteRef
	MoreTitle string
	MoreAll   frameLink
	MoreTotal int
}

// noteRef is a compact pointer to another note.
type noteRef struct {
	ID    string
	Title string
}

// itemPlanPart is a plan a note is part of: its title and page, its status in
// words with a tone, its steps done of all and when it last moved.
type itemPlanPart struct {
	Slug, Title, Href string
	Status, Tone      string
	Done, Total, Pct  int
	Moved             time.Time
}

// itemReadingPace is the words a minute a reading time assumes.
const itemReadingPace = 230

// noteItemPage answers a note's own URL in HTML: the item page, its place in
// the list the URL names (lq; listed when it names one), else in its
// project.
func (s *Service) noteItemPage(w http.ResponseWriter, r *http.Request, d noteDetail, lq libQuery) {
	ctx := r.Context()
	listed := lq.QS() != ""
	// The body again, with an id on each section heading for On this page;
	// the reader's and the peek's copies keep the body Render gives them.
	var outline []markdown.Heading
	if d.BodyLoaded {
		d.Body, outline = markdown.RenderOutline(d.BodyText, s.wikiResolver(ctx, d.Project))
	}
	sel := s.noteReaderOf(ctx, d, lq)
	page := &itemPage{
		Kind: "note", ID: d.ID, Title: d.Title, Lead: d.Description,
		Star: itemStar{Action: "/console/favorites/note/" + d.ID, Next: notesListPath + "/" + d.ID + lq.QS(),
			On: d.Favorite, Noun: "note"},
	}
	if listed {
		l := store.NoteList{ByProject: lq.Project != "", Project: lq.Project, Fav: lq.Fav,
			Query: strings.ToLower(lq.Q), Sort: lq.Sort, Tree: lq.tree()}
		if l.Project == validate.GlobalProject {
			l.Project = ""
		}
		place, found, err := store.NotePlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return noteListWords(l, n) }, notesListPath, lq.QS())
		} else {
			listed = false
		}
	}
	if page.Place == nil {
		l := store.NoteList{ByProject: true, Project: d.Project, Sort: libDefaultSort}
		place, found, err := store.NotePlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return noteListWords(l, n) }, notesListPath, "")
		}
	}
	scoped := listHref(notesListPath, d.Project, "", lq)
	page.Back = frameLink{Label: noteBackLabel(d.Project), Href: listHref(notesListPath, d.Project, "", libQuery{})}
	if listed {
		page.Back = frameLink{Label: noteListBack(lq), Href: lq.href(notesListPath)}
	}
	page.Crumbs = []frameLink{s.projectCrumb(ctx, d.Project, scoped, "Global inbox"), {Label: "Notes", Href: scoped}}
	// Delete returns to the way back (note-delete posts the Notes list with
	// the reader's QS), since the note it deleted has no page left.
	sel.QS = strings.TrimPrefix(page.Back.Href, notesListPath)

	item := &noteItem{noteReader: sel, Outline: outline,
		MoreTitle: "More" + scopeWords(d.Project, true, "in the global inbox")}
	if d.Words > 0 {
		item.Minutes = max(1, int(math.Round(float64(d.Words)/itemReadingPace)))
	}
	for _, p := range sel.Tagged.Plans {
		item.PartOf = append(item.PartOf, s.planPartOf(ctx, d.Project, p.Slug))
	}
	more, total, err := store.NotesOfProject(ctx, s.db(ctx), d.Project, d.ID, itemMoreLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, n := range more {
		item.More = append(item.More, noteRef{ID: n.ID, Title: n.Title})
	}
	item.MoreTotal = total
	item.MoreAll = frameLink{
		Label: fmt.Sprintf("All %d %s%s", total, nounOf(total, "note", "notes"), scopeWords(d.Project, true, "in the global inbox")),
		Href:  listHref(notesListPath, d.Project, "", libQuery{}),
	}
	page.Note = item
	s.render(w, r, "notes", pageData{Title: "Note " + d.Title, Active: "notes", Data: notesData{Item: page}})
}

// noteListWords names a Notes list of n: "notes in example-ops", "starred notes
// matching "x"".
func noteListWords(l store.NoteList, n int) string {
	var b strings.Builder
	if l.Fav {
		b.WriteString("starred ")
	}
	b.WriteString(nounOf(n, "note", "notes"))
	b.WriteString(scopeWords(l.Project, l.ByProject, "in the global inbox"))
	b.WriteString(queryWords(l.Query))
	return b.String()
}

// noteBackLabel is the way back to a note's project's notes.
func noteBackLabel(project string) string {
	if project == "" {
		return "The global inbox"
	}
	return project + "’s notes"
}

// noteListBack is the way back to the Notes list lq names.
func noteListBack(lq libQuery) string {
	switch lq.Project {
	case "":
		return "Notes"
	case validate.GlobalProject:
		return "The global inbox"
	default:
		return lq.Project + "’s notes"
	}
}

// planPartOf is the plan slug in project as a note's Part of card shows it,
// read the way the reader's plan chip reads it (planStatusOf). A plan the
// request cannot resolve keeps its slug and link alone.
func (s *Service) planPartOf(ctx context.Context, project, slug string) itemPlanPart {
	part := itemPlanPart{Slug: slug, Title: slug, Href: planHref(project, slug)}
	primary, _, ok, err := plans.CompositionIn(ctx, s.db(ctx), project, slug)
	if err != nil {
		s.logger.Warn("console: note part of plan", "slug", slug, "error", err)
		return part
	}
	if !ok {
		return part
	}
	row := s.planRow(ctx, primary, nil, nil, time.Time{})
	if strings.TrimSpace(row.Title) != "" {
		part.Title = row.Title
	}
	part.Done, part.Total, part.Pct, part.Moved = row.TasksDone, row.TasksTotal, percent(row.TasksDone, row.TasksTotal), row.Updated
	part.Status, part.Tone = planStatusWords(row)
	return part
}

// --- tasks ------------------------------------------------------------------

// taskItem is a task's own page: the detail the reader shows (its actions
// returning to this page), and its plan as a mini card.
type taskItem struct {
	*taskDetail
	Plan *itemPlanCard
}

// itemPlanCard is a task's plan as its page's mini card: the narrative's
// title (the slug without one), the steps done of all and the rest counted
// by their words, a bar, and every step, this one marked: the closed ones in
// the order they closed, then the rest in the plan card's order (groupSteps).
type itemPlanCard struct {
	Slug, Title, Href string
	Done, Total       int
	DonePct, DoingPct int
	Counts            []planCountVM
	Steps             []itemPlanStep
	current           int // this step's index in Steps, -1 when absent
}

// itemPlanStep is one step on the mini card.
type itemPlanStep struct {
	ID      string
	Title   string
	Word    taskWord
	Current bool
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

// place is the step's place on its plan's card: "Step 2 of 5 in this plan".
func (c *itemPlanCard) place() *itemPlace {
	out := &itemPlace{Text: fmt.Sprintf("Step %d of %d in this plan", c.current+1, len(c.Steps))}
	if c.current > 0 {
		p := c.Steps[c.current-1]
		out.Prev = &frameLink{Label: p.Title, Href: "/console/tasks/" + p.ID}
	}
	if c.current+1 < len(c.Steps) {
		n := c.Steps[c.current+1]
		out.Next = &frameLink{Label: n.Title, Href: "/console/tasks/" + n.ID}
	}
	return out
}
