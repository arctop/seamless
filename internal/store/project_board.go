package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ProjectBoardRow is one project's health roll-up for the project board: the
// same strict per-slug counts GetProjectCounts computes for a single project,
// batched across every project in one pass, plus liveness (live sessions),
// backpressure (blocked tasks), recency (last session activity), and retrieval
// reach joined from BuildRetrievalReport.
//
// Counts are STRICT per-slug (project = slug / project_slug = slug), never a
// global union: the global scope ("") is its own row whose counts are the
// global-scope counts, never folded into each named project. A slug that appears
// in the data tables but has no projects-table row (files are the source of
// truth and can drift ahead of the registry) still gets a row, flagged
// Unregistered so the caller can surface the drift rather than hide it.
type ProjectBoardRow struct {
	Project      string    `json:"project"`      // slug; "" is the global scope
	Memories     int       `json:"memories"`     // active memories, project = slug (strict, matches GetProjectCounts.Memories)
	Sessions     int       `json:"sessions"`     // all sessions project_slug = slug (matches GetProjectCounts.Sessions)
	LiveSessions int       `json:"liveSessions"` // live sessions: active AND updated within core.SessionIdleTTL (matches the Sessions screen)
	OpenTasks    int       `json:"openTasks"`    // status IN ('open','in_progress') (matches GetProjectCounts.OpenTasks)
	Blocked      int       `json:"blocked"`      // open tasks that are not ready: an open/in_progress blocker, or waiting on people (a gate, an open gate of its own)
	Notes        int       `json:"notes"`        // notes project = slug (matches GetProjectCounts.Notes)
	LastActive   time.Time `json:"lastActive"`   // MAX(sessions.updated_at) for the project; zero when none
	TokensTotal  int       `json:"tokensTotal"`  // SUM(sessions.total_tokens): real model token burn harvested from transcripts
	Unregistered bool      `json:"unregistered"` // slug seen in data tables but absent from the projects table
	ReachRate    int       `json:"reachRate"`    // Surfaced/Active rounded %, from BuildRetrievalReport; 0 when absent
	Surfaced     int       `json:"surfaced"`     // distinct active memories surfaced >=1x in the window
	Active       int       `json:"active"`       // active memories (reach denominator) per BuildRetrievalReport
}

// ProjectsWithCounts returns a ProjectBoardRow for every project -- every slug
// that appears in memories_index / sessions / tasks / notes_index unioned with
// the projects table -- with strict per-slug counts, liveness, blocked-task
// backpressure, and last-active recency computed in SQL, then retrieval reach
// (ReachRate/Surfaced/Active) joined by slug from BuildRetrievalReport over the
// given window. LiveSessions counts sessions that are active AND updated within
// idleTTL of now (<= 0 falls back to core.SessionIdleTTL), matching the
// Sessions screen's live bucket, so an active-but-idle session awaiting the
// reaper never inflates the board.
//
// The counts mirror GetProjectCounts exactly (same strict per-slug predicates),
// so a single row equals the single-project peek. The global scope ("") is its
// own row (never folded into named projects); it is never flagged Unregistered
// because the global scope is legitimately never registered. Rows are ordered by
// slug (the global "" scope first).
func ProjectsWithCounts(ctx context.Context, db *sql.DB, window RetrievalWindow, now time.Time, idleTTL time.Duration) ([]ProjectBoardRow, error) {
	rows := map[string]*ProjectBoardRow{}
	row := func(slug string) *ProjectBoardRow {
		r := rows[slug]
		if r == nil {
			r = &ProjectBoardRow{Project: slug}
			rows[slug] = r
		}
		return r
	}

	// Active memories per project (strict): project = slug, invalid_at IS NULL.
	if err := scanGroupCount(ctx, db,
		`SELECT project, COUNT(*) FROM memories_index WHERE invalid_at IS NULL GROUP BY project`,
		func(slug string, n int) { row(slug).Memories = n }); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: memories: %w", err)
	}

	// Sessions per project: all statuses (matches GetProjectCounts.Sessions),
	// plus live count (active within the idle TTL) and MAX(updated_at) for
	// recency, in one pass. LastActive deliberately ignores the cutoff: it
	// reflects last activity regardless of live/idle.
	if idleTTL <= 0 {
		idleTTL = core.SessionIdleTTL
	}
	liveCutoff := core.FormatTime(now.UTC().Add(-idleTTL))
	sessRows, err := db.QueryContext(ctx, `
		SELECT project_slug, COUNT(*),
		       SUM(CASE WHEN status = 'active' AND updated_at >= ? THEN 1 ELSE 0 END),
		       MAX(updated_at),
		       COALESCE(SUM(total_tokens), 0)
		FROM sessions GROUP BY project_slug`, liveCutoff)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: sessions: %w", err)
	}
	if err := func() error {
		defer func() { _ = sessRows.Close() }()
		for sessRows.Next() {
			var (
				slug        string
				total, live int
				lastUpdated sql.NullString
				tokensTotal int
			)
			if err := sessRows.Scan(&slug, &total, &live, &lastUpdated, &tokensTotal); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			last, err := nullTime(lastUpdated)
			if err != nil {
				return fmt.Errorf("last_active: %w", err)
			}
			r := row(slug)
			r.Sessions = total
			r.LiveSessions = live
			r.LastActive = last
			r.TokensTotal = tokensTotal
		}
		return sessRows.Err()
	}(); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: sessions: %w", err)
	}

	// Open tasks per project (matches GetProjectCounts.OpenTasks): no plan filter,
	// so plan-step tasks count too.
	if err := scanGroupCount(ctx, db,
		`SELECT project_slug, COUNT(*) FROM tasks WHERE status IN ('open','in_progress') GROUP BY project_slug`,
		func(slug string, n int) { row(slug).OpenTasks = n }); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: open tasks: %w", err)
	}

	// Blocked tasks per project: open tasks that are not ready -- an
	// open/in_progress blocker, or waiting on people -- the open tasks the
	// readiness rule (sqlReady) leaves out: BlockedTasks, GatedTasks and the
	// open GateTasks together.
	if err := scanGroupCount(ctx, db, `
		SELECT t.project_slug, COUNT(*) FROM tasks t
		WHERE t.status = 'open' AND NOT (`+sqlReady("t")+`)
		GROUP BY t.project_slug`,
		func(slug string, n int) { row(slug).Blocked = n }); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: blocked tasks: %w", err)
	}

	// Notes per project (matches GetProjectCounts.Notes).
	if err := scanGroupCount(ctx, db,
		`SELECT project, COUNT(*) FROM notes_index GROUP BY project`,
		func(slug string, n int) { row(slug).Notes = n }); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: notes: %w", err)
	}

	// Registered slugs: union the projects table in (so an empty registered
	// project still gets a row) and mark every non-empty slug absent from it as
	// Unregistered. The global scope "" is never registered and never flagged.
	registered := map[string]bool{}
	if err := scanGroupCount(ctx, db, `SELECT slug, 1 FROM projects`,
		func(slug string, _ int) { registered[slug] = true; row(slug) }); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: projects: %w", err)
	}
	for slug, r := range rows {
		r.Unregistered = slug != "" && !registered[slug]
	}

	// Join retrieval reach by slug (ReachRate/Surfaced/Active only; the board's
	// last-active comes from sessions, which ByProject has no notion of).
	report, err := BuildRetrievalReport(ctx, db, window, 0)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectsWithCounts: reach: %w", err)
	}
	for _, pr := range report.ByProject {
		r := row(pr.Project)
		r.ReachRate = pr.ReachRate
		r.Surfaced = pr.Surfaced
		r.Active = pr.Active
	}

	out := make([]ProjectBoardRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// GetSessionCoverageForProject computes the session-coverage roll-up (see
// GetSessionCoverage) restricted to one project's sessions (project_slug = ?).
//
// It rejects an empty project: an empty project_slug marks real global sessions,
// so "" is ambiguous between "all sessions" and "global-only" -- the caller who
// wants the all-sessions number must use GetSessionCoverage instead.
func GetSessionCoverageForProject(ctx context.Context, db *sql.DB, project string, since time.Time) (SessionCoverage, error) {
	if project == "" {
		return SessionCoverage{}, errors.New("store.GetSessionCoverageForProject: empty project is ambiguous -- project_slug='' rows are real global sessions; use GetSessionCoverage for the all-sessions number, or pass an explicit slug")
	}
	c, err := sessionCoverage(ctx, db, project, true, since, time.Time{})
	if err != nil {
		return c, fmt.Errorf("store.GetSessionCoverageForProject: %w", err)
	}
	return c, nil
}

// scanGroupCount runs a two-column (key, count) GROUP BY query and calls fn for
// each row. It centralizes the per-table roll-up scans ProjectsWithCounts merges.
func scanGroupCount(ctx context.Context, db *sql.DB, query string, fn func(key string, n int)) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			key string
			n   int
		)
		if err := rows.Scan(&key, &n); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		fn(key, n)
	}
	return rows.Err()
}
