package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProjectActivity is what the console's project switcher orders a project by
// and marks it with (plan:console-ux-pass UX 27): when work last moved there,
// and whether the owner starred it. No count: the switcher's counts are each
// screen's own, what choosing the project would show there.
type ProjectActivity struct {
	// LastActive is the project's latest session update or task change,
	// whichever is later: the Projects board's last activity
	// (ProjectsWithCounts' LastActive and LastTask, which the board's rows
	// combine the same way). Zero when the project has neither.
	LastActive time.Time
	// Favorite is the project's star (projects.favorite, favorite_set's
	// project kind). False for the global scope and for a slug with no
	// projects row.
	Favorite bool
}

// ProjectActivityBySlug returns, keyed by slug ("" is the global scope), every
// scope on db that has a session, a task or a projects row: its last activity
// and its star. Read it through the request's handle: a project the handle
// hides has no row here.
func ProjectActivityBySlug(ctx context.Context, db *sql.DB) (map[string]ProjectActivity, error) {
	const op = "store.ProjectActivityBySlug"
	out := map[string]ProjectActivity{}
	// Timestamps are fixed-width canonical UTC text (core.TimeFormat), so the
	// lexical MAX is the latest instant, across both tables at once.
	rows, err := db.QueryContext(ctx, `
		SELECT slug, MAX(at) FROM (
			SELECT project_slug AS slug, MAX(updated_at) AS at FROM sessions GROUP BY project_slug
			UNION ALL
			SELECT project_slug, MAX(updated_at) FROM tasks GROUP BY project_slug
		) GROUP BY slug`)
	if err != nil {
		return nil, fmt.Errorf("%s: activity: %w", op, err)
	}
	if err := func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				slug string
				at   sql.NullString
			)
			if err := rows.Scan(&slug, &at); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			last, err := nullTime(at)
			if err != nil {
				return fmt.Errorf("last active: %w", err)
			}
			out[slug] = ProjectActivity{LastActive: last}
		}
		return rows.Err()
	}(); err != nil {
		return nil, fmt.Errorf("%s: activity: %w", op, err)
	}

	stars, err := db.QueryContext(ctx, `SELECT slug, favorite FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("%s: projects: %w", op, err)
	}
	defer func() { _ = stars.Close() }()
	for stars.Next() {
		var (
			slug string
			fav  bool
		)
		if err := stars.Scan(&slug, &fav); err != nil {
			return nil, fmt.Errorf("%s: projects: scan: %w", op, err)
		}
		a := out[slug]
		a.Favorite = fav
		out[slug] = a
	}
	if err := stars.Err(); err != nil {
		return nil, fmt.Errorf("%s: projects: %w", op, err)
	}
	return out, nil
}
