package diary

import (
	"slices"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
)

// planPrimary is the primary note of a plan's composition, chosen as
// plans.Composition chooses it, from the notes tagged into it in NotesByTag's
// order (store.DiaryPlanNotes reads them in one query for every plan of a
// day, where Composition reads one plan): the first cc-plan capture, else the
// earliest-created note that is no agent cache. ok is false when no note
// qualifies: a task-only plan has no composition, and no page.
// TestPlanPrimary_AgreesWithComposition holds the two to one answer.
func planPrimary(tagged []core.Note) (core.Note, bool) {
	for _, n := range tagged {
		if slices.Contains(n.Tags, plans.TagPlan) {
			return n, true
		}
	}
	idx := -1
	for i, n := range tagged {
		if slices.Contains(n.Tags, plans.TagAgent) {
			continue
		}
		if idx == -1 || plans.EarlierPrimary(n, tagged[idx]) {
			idx = i
		}
	}
	if idx == -1 {
		return core.Note{}, false
	}
	return tagged[idx], true
}
