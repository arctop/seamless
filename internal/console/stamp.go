package console

import (
	"context"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Provenance stamps on the console's item pages and search results (plan
// fenced-sessions P6): the line "may contain content from <project> (<grant|
// move> <short id>)" for an item a conversation wrote while it held content
// from a fenced project. It names projects and approvals, never content, and
// renders nothing for an item with no stamp -- every item on an install that
// never turned isolation.requests on. A memory's and a note's stamps are its
// file's; a task's and a session's are the index's, read through the
// request's handle (s.db), which hides a stamp with its item.

// stampsByID returns the stamps of the items of kind with ids, as their pages
// show them. A failed read is logged and shows none: a warning that cannot be
// read must not take the page down with it.
func (s *Service) stampsByID(ctx context.Context, kind string, ids []string) map[string][]core.Stamp {
	if len(ids) == 0 {
		return nil
	}
	got, err := store.ProvenanceForItems(ctx, s.db(ctx), kind, ids)
	if err != nil {
		s.logger.Warn("console: provenance stamps", "kind", kind, "error", err)
		return nil
	}
	return got
}

// itemStamps is stampsByID for one item in project.
func (s *Service) itemStamps(ctx context.Context, kind, id, project string) []core.Stamp {
	return core.StampsShown(s.stampsByID(ctx, kind, []string{id})[id], project)
}
