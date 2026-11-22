package console

// A note's own page (itempage.go has the frame every item page shares).

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/markdown"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

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

// itemReadingPace is the words a minute a reading time assumes.
const itemReadingPace = 230

// noteItemPage answers a note's own URL in HTML: the item page, its place in
// the list the URL names (lq and v; listed when it names one,
// namesNoteList), else in its project.
func (s *Service) noteItemPage(w http.ResponseWriter, r *http.Request, d noteDetail, lq libQuery, v noteView) {
	ctx := r.Context()
	listed := namesNoteList(r.URL.Query())
	// qs is the list's state the place's neighbours and the star carry; the
	// page size is the list's alone.
	qs := ""
	if listed {
		qs = noteListQS(lq, v)
	}
	// The body again, with an id on each section heading for On this page;
	// the reader's and the peek's copies keep the body Render gives them.
	var outline []markdown.Heading
	if d.BodyLoaded {
		d.Body, outline = markdown.RenderOutline(d.BodyText, s.wikiResolver(ctx, d.Project))
	}
	sel := s.noteReaderOf(ctx, d, lq)
	page := &itemPage{
		Kind: "note", ID: d.ID, Title: d.Title, Lead: d.Description,
		Star: itemStar{Action: "/console/favorites/note/" + d.ID, Next: notesListPath + "/" + d.ID + qs,
			On: d.Favorite, Noun: "note"},
	}
	if listed {
		l := notePlaceList(lq, v)
		place, found, err := store.NotePlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return noteListWords(l, n) }, notesListPath, qs)
		} else {
			listed = false
		}
	}
	if page.Place == nil {
		// Its project's list, newest first; an agent's working copy among
		// the project's notes with the working copies shown, which its
		// neighbours' links name, so k / j stay in that list.
		own, ov := libQuery{Sort: libDefaultSort, Project: scopeParam(d.Project)}, noteView{Caches: isAgentCache(d.Tags)}
		oqs := ""
		if ov.Caches {
			oqs = noteListQS(own, ov)
		}
		l := notePlaceList(own, ov)
		place, found, err := store.NotePlace(ctx, s.db(ctx), l, d.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if found {
			page.Place = placeOf(place, func(n int) string { return noteListWords(l, n) }, notesListPath, oqs)
		}
	}
	scoped := listHref(notesListPath, d.Project, "", lq)
	page.Back = frameLink{Label: noteBackLabel(d.Project), Href: listHref(notesListPath, d.Project, "", libQuery{})}
	if listed {
		page.Back = frameLink{Label: noteListBack(lq), Href: noteListHref(lq, noteView{Tag: v.Tag, Plans: v.Plans, Caches: v.Caches})}
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
	// The other notes the project's list draws: its working copies stay
	// folded here as there.
	more, total, err := store.NotesOfProject(ctx, s.db(ctx), d.Project, d.ID, plans.TagAgent, itemMoreLimit)
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

// notePlaceList is the Notes list lq and v name, as a place query reads it:
// the list the Notes page draws (notesPage, buildNoteList), in the sort's
// order, the agent working copies left out unless v shows them.
// TestItPlaceNotesAgreeWithTheList holds the two equal.
func notePlaceList(lq libQuery, v noteView) store.NoteList {
	l := store.NoteList{ByProject: lq.Project != "", Project: lq.Project, Fav: lq.Fav,
		Query: strings.ToLower(lq.Q), Tag: v.Tag, Plans: v.Plans, Sort: lq.Sort}
	if l.Project == validate.GlobalProject {
		l.Project = ""
	}
	if !v.Caches {
		l.Without = plans.TagAgent
	}
	return l
}

// noteListWords names a Notes list of n: "notes in example-ops", "starred plan
// notes tagged research matching "x"".
func noteListWords(l store.NoteList, n int) string {
	var b strings.Builder
	if l.Fav {
		b.WriteString("starred ")
	}
	if l.Plans {
		b.WriteString("plan ")
	}
	b.WriteString(nounOf(n, "note", "notes"))
	if l.Tag != "" {
		b.WriteString(" tagged " + l.Tag)
	}
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
