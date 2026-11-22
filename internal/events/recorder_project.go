package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/arctop/seamless/internal/core"
)

// ErrNoProject is ProjectEvents' answer to an empty project: an empty
// project_slug marks the global scope's own events, so "" is ambiguous between
// "every project" and "global only", and the project page always names one
// project.
var ErrNoProject = errors.New("events.ProjectEvents: empty project is ambiguous: name a project slug")

// ProjectEvents returns one project's events, newest first: the events
// recorded with project_slug = project, of the given kinds when kinds is
// non-empty, and never of the excluded kinds. A non-positive limit defaults
// to 50.
//
// It is the project page's read (Recent activity, the plan ledger, the
// Interactions tab). Before it, those panels read the fleet's newest events
// and kept the project's, so a quiet project on a busy fleet showed nothing.
// The (project_slug, ts) index (migration 056) serves it; through the
// console's filtered handle (ReadingFrom) a locked project's events are not
// there to read.
func (r *Recorder) ProjectEvents(ctx context.Context, project string, kinds, exclude []core.EventKind, limit int) ([]core.Event, error) {
	if project == "" {
		return nil, ErrNoProject
	}
	q, args := projectEventsQuery(project, kinds, exclude, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.ProjectEvents: %w", err)
	}
	return scanEvents(rows)
}

// projectEventsQuery is ProjectEvents' statement and its arguments.
func projectEventsQuery(project string, kinds, exclude []core.EventKind, limit int) (string, []any) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, ts, kind, session_id, project_slug, item_id, payload
	      FROM events WHERE project_slug = ?`
	args := []any{project}
	if ph, inArgs := kindArgs(kinds); ph != "" {
		q += ` AND kind IN (` + ph + `)`
		args = append(args, inArgs...)
	}
	if ph, exArgs := kindArgs(exclude); ph != "" {
		q += ` AND kind NOT IN (` + ph + `)`
		args = append(args, exArgs...)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	return q, append(args, limit)
}
