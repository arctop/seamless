package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/arctop/seamless/internal/core"
)

// ListSessionsForProject returns a project's sessions newest-updated first,
// optionally filtered by status and to those updated since a cutoff (a zero
// `since` means all time), capped at limit (default 100).
//
// It is STRICT per-slug (project_slug = ?): unlike RecentFindings it does NOT
// union the global scope, so a project sees only its own sessions. It rejects an
// empty project: an empty project_slug marks real global sessions, so "" is
// ambiguous between "all sessions" and "global-only" -- use ListSessions for all
// sessions (there is no global-only variant; pass an explicit slug for one
// project).
func ListSessionsForProject(ctx context.Context, db *sql.DB, project string, status core.SessionStatus, since time.Time, limit int) ([]core.Session, error) {
	if project == "" {
		return nil, errors.New("store.ListSessionsForProject: empty project is ambiguous -- project_slug='' rows are real global sessions, so \"\" is ambiguous between \"all\" and \"global-only\"; use ListSessions for all sessions, or pass an explicit slug")
	}
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT ` + sessionCols + ` FROM sessions WHERE project_slug = ?`
	args := []any{project}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, string(status))
	}
	if !since.IsZero() {
		query += ` AND updated_at >= ?`
		args = append(args, core.FormatTime(since))
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ListSessionsForProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListSessionsForProject: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ProjectSessionTally is one project's sessions by state, the project page's
// Sessions tab header: Live and Idle split the active sessions by the
// liveness rule (core.Session.LiveAsOf, here sessionLive), Completed and
// Expired are the two closed statuses, and Total is every session of the
// project, whatever its status.
type ProjectSessionTally struct {
	Total     int `json:"total"`
	Live      int `json:"live"`
	Idle      int `json:"idle"`
	Completed int `json:"completed"`
	Expired   int `json:"expired"`
}

// ProjectSessionTallyAt counts project's sessions by state at now, strict
// per slug as ListSessionsForProject reads them, in one query. An idleTTL
// <= 0 falls back to core.SessionIdleTTL. It rejects an empty project, as
// ListSessionsForProject does: an empty project_slug marks the global scope's
// own sessions.
func ProjectSessionTallyAt(ctx context.Context, db *sql.DB, project string, now time.Time, idleTTL time.Duration) (ProjectSessionTally, error) {
	if project == "" {
		return ProjectSessionTally{}, errors.New("store.ProjectSessionTallyAt: empty project is ambiguous -- project_slug='' rows are the global scope's own sessions; pass an explicit slug")
	}
	if idleTTL <= 0 {
		idleTTL = core.SessionIdleTTL
	}
	var t ProjectSessionTally
	var active int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN `+sessionLive+` THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status = 'expired' THEN 1 ELSE 0 END), 0)
		FROM sessions WHERE project_slug = ?`,
		core.FormatTime(now.UTC().Add(-idleTTL)), core.FormatTime(now.UTC()), project).
		Scan(&t.Total, &t.Live, &active, &t.Completed, &t.Expired)
	if err != nil {
		return ProjectSessionTally{}, fmt.Errorf("store.ProjectSessionTallyAt: %w", err)
	}
	t.Idle = active - t.Live
	return t, nil
}

// MemoriesForSession returns the memories a session produced -- the index rows
// whose source_session matches the session -- newest-updated first.
// Memory.SourceSession holds the session NAME for ambient stamps
// (cc/ab12cd34) but the session ULID for bound stamps (see the
// source-session-stamps memory), so the query matches either spelling and
// callers pass the whole session.
func MemoriesForSession(ctx context.Context, db *sql.DB, sess core.Session) ([]core.Memory, error) {
	stamps := make([]any, 0, 2)
	for _, s := range []string{sess.Name, sess.ID} {
		if s != "" {
			stamps = append(stamps, s)
		}
	}
	if len(stamps) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?, ", len(stamps)-1) + "?"
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index WHERE source_session IN (`+placeholders+`)
		ORDER BY updated_at DESC, id DESC`, stamps...)
	if err != nil {
		return nil, fmt.Errorf("store.MemoriesForSession: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.MemoriesForSession: %w", err)
	}
	return mems, nil
}

// TasksClaimedBy returns the tasks a session holds or held -- the rows whose
// claimed_by matches the session ULID -- oldest-created first (ties by id, the
// ULID-monotonic order the ready queue uses). Task.ClaimedBy stores the session
// ULID, never its name.
//
// GUARD (the inverse of MemoriesForSession): if the argument is not ULID-shaped
// -- it contains '/' or is not 26 chars or fails to parse as a ULID -- it is
// almost certainly a session name that would silently match nothing; the call is
// rejected with a message pointing at SessionByName to resolve the id.
func TasksClaimedBy(ctx context.Context, db *sql.DB, sessionID string) ([]core.Task, error) {
	if !LooksLikeSessionULID(sessionID) {
		return nil, errors.New("store.TasksClaimedBy: expected a session ULID; resolve the name via SessionByName(...).ID first")
	}
	rows, err := db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE claimed_by = ?
		ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.TasksClaimedBy: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// DistinctPlanSlugsForProject returns the full set of plan slugs a project has
// ever had, including completed plans, sorted. Unlike ActivePlans (which drops a
// plan once every step is closed) this keeps completed plans, so it backs a
// history/lineage view. It rejects an empty project for the same reason as
// ListSessionsForProject.
//
// Plan slugs are unique only PER PROJECT: two projects may each have a plan
// "refactor". A cross-project consumer must key by (project, slug), never slug
// alone.
func DistinctPlanSlugsForProject(ctx context.Context, db *sql.DB, project string) ([]string, error) {
	if project == "" {
		return nil, errors.New("store.DistinctPlanSlugsForProject: empty project is ambiguous -- project_slug='' rows are real global tasks; pass an explicit slug (plan slugs are unique only per project)")
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT plan_slug FROM tasks
		WHERE project_slug = ? AND plan_slug <> '' ORDER BY plan_slug`, project)
	if err != nil {
		return nil, fmt.Errorf("store.DistinctPlanSlugsForProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("store.DistinctPlanSlugsForProject: scan: %w", err)
		}
		out = append(out, slug)
	}
	return out, rows.Err()
}

// ProjectMemoriesIncludingInvalid returns a project's memory index rows -- both
// active and invalid (superseded/archived) -- newest-updated first, for a
// lineage view that must render supersession chains.
//
// It is STRICT per-slug (project = ?): it does NOT union the global (empty) scope.
// This is the deliberate opposite of ActiveMemories, which DOES union global
// rows into a project's view. Mixing the two conventions is exactly how a
// project count comes out larger than expected, so callers must pick the strict
// variant on purpose. It rejects an empty project for the same reason as
// ListSessionsForProject.
func ProjectMemoriesIncludingInvalid(ctx context.Context, db *sql.DB, project string) ([]core.Memory, error) {
	if project == "" {
		return nil, errors.New("store.ProjectMemoriesIncludingInvalid: empty project is ambiguous -- project='' rows are real global memories; this strict variant does not union global (unlike ActiveMemories), so pass an explicit slug")
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index WHERE project = ?
		ORDER BY updated_at DESC, id DESC`, project)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectMemoriesIncludingInvalid: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectMemoriesIncludingInvalid: %w", err)
	}
	return mems, nil
}

// ProjectsByParent returns the projects whose parent_slug equals parent, ordered
// by slug (using idx_projects_parent). It backs walking a project's children in
// the parent/child topology a split builds.
func ProjectsByParent(ctx context.Context, db *sql.DB, parent string) ([]core.Project, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+projectCols+` FROM projects WHERE parent_slug = ? ORDER BY slug`, parent)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectsByParent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ProjectsByParent: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LooksLikeSessionULID reports whether s has the shape of a bare session ULID: no
// '/' (session names always contain one), exactly 26 chars, and a valid ULID
// encoding. It disambiguates the session-name vs session-id relation guards, and
// lets provenance consumers pick the right lookup for a source_session stamp
// (name for ambient stamps, ULID for bound stamps) without a doomed first query.
func LooksLikeSessionULID(s string) bool {
	if strings.Contains(s, "/") || len(s) != 26 {
		return false
	}
	_, err := ulid.Parse(s)
	return err == nil
}
