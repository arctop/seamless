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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
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
// place in the list the URL names (lq and v; listed when it names one,
// namesMemoryList), else in its project and kind.
func (s *Service) memoryItemPage(w http.ResponseWriter, r *http.Request, d memoryDetail, lq libQuery, v memoryView) {
	ctx := r.Context()
	listed := namesMemoryList(r.URL.Query())
	// qs is the list's state every link of the page keeps: the place's
	// neighbours, the star's and the archive's way back, the successor
	// links. The page size is the list's alone.
	qs := ""
	if listed {
		qs = memoryListQS(lq, v)
	}
	sel := s.memoryReaderOf(ctx, d, lq)
	sel.QS = qs
	active := d.Status == "active"
	page := &itemPage{
		Kind: "memory", ID: d.ID, Title: d.Name, Mono: true, Lead: d.Description,
		Star: itemStar{Action: "/console/favorites/memory/" + d.ID, Next: memoriesListPath + "/" + d.ID + qs,
			On: d.Favorite, Noun: "memory"},
	}

	// The place: in the list the URL names, when it holds the memory; else in
	// the memory's project and kind, newest first. A memory that is no
	// longer active is placed among the archived and superseded ones.
	if listed {
		l := memoryPlaceList(lq, !active)
		place, found, err := store.MemoryPlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return memoryListWords(l, n) }, memoriesListPath, qs)
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
		page.Back = frameLink{Label: memoryListBack(lq, v), Href: memoryListHref(lq, memoryView{Inactive: v.Inactive})}
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

// memoryPlaceList is the Memories list lq names, as a place query reads it:
// the list the Memories page draws (memoriesPage, buildMemoryList), a
// project's by kind in core's order and then the sort, every project's in the
// sort's order; inactive for the archived and superseded one.
// TestItPlaceAgreesWithTheList holds the two equal.
func memoryPlaceList(lq libQuery, inactive bool) store.MemoryList {
	kinds := make([]string, 0, len(core.MemoryKinds))
	for _, k := range core.MemoryKinds {
		kinds = append(kinds, string(k))
	}
	l := store.MemoryList{
		ByProject: lq.Project != "", Project: lq.Project, Kind: lq.Kind, Fav: lq.Fav,
		Never: lq.Surfaced == "never", Query: strings.ToLower(lq.Q), Inactive: inactive,
		Sort: lq.Sort, Tree: lq.Project != "", Kinds: kinds,
	}
	if l.Project == validate.GlobalProject {
		l.Project = ""
	}
	return l
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

// memoryListBack is the way back to the Memories list lq and v name.
func memoryListBack(lq libQuery, v memoryView) string {
	noun := memoryNoun(lq.Kind, 2)
	if v.Inactive {
		noun = "archived and superseded " + noun
	}
	switch lq.Project {
	case "":
		return strings.ToUpper(noun[:1]) + noun[1:]
	case validate.GlobalProject:
		return strings.ToUpper(noun[:1]) + noun[1:] + " shared globally"
	default:
		return lq.Project + "’s " + noun
	}
}

// --- the plan parts a note's and a task's page share ------------------------

// itemPlanPart is a plan a note is part of: its title and page, its status in
// words with a tone, its steps done of all and when it last moved.
type itemPlanPart struct {
	Slug, Title, Href string
	Status, Tone      string
	Done, Total, Pct  int
	Moved             time.Time
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
