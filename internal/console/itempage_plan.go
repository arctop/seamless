package console

// A plan's own page (plan:console-ux-pass UX 32): /console/plans/{slug}?project=
// in the reading frame a memory, a note and a task open in (itempage.go,
// templates/item_shared.html), so the four read alike. The model is
// planDetailIn's, unchanged: its JSON is what `seam plan show` reads, and the
// ?peek=1, ?reader=1 and ?steps=1 fragments answer from it as before. This
// file only places it in the frame:
//
//   - the way back: the Plans board scoped to the plan's project;
//   - where it lives: project, then Plans;
//   - the title row: the narrative's title (the slug without a note) with
//     copy and the star, then plan:<slug> with copy, the status in words, and
//     Approve while a capture awaits it;
//   - the facts: where it came from, steps done of all, model tokens as the
//     plan card shows them, updated;
//   - the body column: the card's steps first, in dependency order, then the
//     narrative;
//   - the context column: the narrative note, the attached notes, and the
//     project.
//
// A plan has no place in a list (the board groups by state, which only the
// whole board knows), so the frame draws no previous or next. Every read is
// planDetailIn's or projectCrumb's, through the request's handle (s.db).
// templates/plans.html fills the frame's blocks.

import (
	"net/http"
	"net/url"
	"strings"
)

// planItemPage is a plan's own page: the frame every kind shares (itemPage,
// whose fields the frame reads promoted) and the plan's own view.
type planItemPage struct {
	itemPage
	Plan *planDetailData
	// StateText says, for a plan with no status of its own (a composed
	// plan), the state the board groups it under, with StateTone its tone;
	// a capture says its own status, as its card does. A stalled plan says
	// so with its stalled mark instead.
	StateText, StateTone string
	// Plans is the board scoped to the plan's project, the context column's
	// way to the project's other plans.
	Plans frameLink
}

// planStateWords names a board state for a plan's title row: its group,
// in the words a sentence uses, and a tone. Stalled has none: the stalled
// mark ("stalled 9d") says it with how long.
func planStateWords(state string) (text, tone string) {
	switch state {
	case planStateNeeds:
		return "needs you", "warn"
	case planStateInProgress:
		return "in progress", "accent"
	case planStateReady:
		return "ready", ""
	case planStateShipped:
		return "shipped", "ok"
	}
	return "", ""
}

// planTitleText is a plan title as the frame's title shows it: each " -- "
// a dash that stays with the word before it, as planTitleHTML draws it on
// the board, in plain text (the frame escapes its title). The stored title
// is untouched.
func planTitleText(title string) string {
	return strings.ReplaceAll(title, " -- ", ": ")
}

// planBackLabel is the way back to a plan's project's plans.
func planBackLabel(project string) string {
	if project == "" {
		return "Plans in the global scope"
	}
	return project + "’s plans"
}

// planPage answers a plan's own URL in HTML: d (planDetailIn's) in the
// item frame.
func (s *Service) planPage(w http.ResponseWriter, r *http.Request, d planDetailData) {
	ctx := r.Context()
	project := d.Row.Project
	board := "/console/plans?project=" + url.QueryEscape(planProjectParam(project))
	page := &planItemPage{
		itemPage: itemPage{
			Kind: "plan", ID: d.Row.Slug, Title: planTitleText(d.Row.Title), Mono: !d.Card.HasNote, Lead: d.Lead,
			Back:   frameLink{Label: planBackLabel(project), Href: board},
			Crumbs: []frameLink{s.projectCrumb(ctx, project, board, "global"), {Label: "Plans", Href: board}},
		},
		Plan:  &d,
		Plans: frameLink{Label: "Every plan in " + planProjectLabel(project), Href: board},
	}
	if project == "" {
		page.Plans.Label = "Every plan in the global scope"
	}
	// A plan stars through its narrative note: without one there is nothing
	// to star, and the frame draws no star (an itemStar with no Action).
	if d.Card.HasNote {
		page.Star = itemStar{Action: d.Card.StarAction(), Next: d.Return, On: d.Card.Favorite, Noun: "plan"}
	}
	if d.Card.StatusText == "" {
		page.StateText, page.StateTone = planStateWords(d.Card.State)
	}
	s.render(w, r, "plans", pageData{Title: "Plan " + d.Row.Title, Active: "plans", Data: plansData{Item: page, Return: d.Return}})
}
