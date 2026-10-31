package store

import (
	"context"

	"github.com/arctop/seamless/internal/access"
)

// accessFilter renders the peer_auth reach of the request in ctx as a SQL
// predicate over column (a project slug column), with its arguments: " AND
// column NOT IN (...)" for the owner while projects are sealed to other
// accounts, " AND column IN (..., ”)" for a configured account (its own
// projects plus the global scope it may read), and nothing at all with no View
// -- peer_auth off, or background work serving no caller.
//
// It is for the few reads that cannot lean on a filtered handle (OpenView): the
// fts virtual table has no view to hide behind, because MATCH, bm25() and
// snippet() only work on the table itself.
func accessFilter(ctx context.Context, column string) (string, []any) {
	v, ok := access.FromContext(ctx)
	if !ok {
		return "", nil
	}
	if allowed, bounded := v.Allowed(); bounded {
		args := make([]any, 0, len(allowed)+1)
		for _, p := range allowed {
			args = append(args, p)
		}
		args = append(args, "")
		return ` AND ` + column + ` IN (` + placeholders(len(args)) + `)`, args
	}
	hidden := v.Hidden()
	if len(hidden) == 0 {
		return "", nil
	}
	args := make([]any, len(hidden))
	for i, p := range hidden {
		args[i] = p
	}
	return ` AND ` + column + ` NOT IN (` + placeholders(len(args)) + `)`, args
}
