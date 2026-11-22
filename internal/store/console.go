package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// NavCounts are the cheap roll-up counts the console shows in its sidebar. It is
// a handful of COUNT queries, safe to run on every page load.
type NavCounts struct {
	Sessions         int // all sessions
	Memories         int // active memories
	Notes            int // all notes
	OpenTasks        int // open or in_progress
	PendingProposals int // pending gardener proposals
	Projects         int // registered projects
	Plans            int // distinct plan:<slug> compositions (captures + composed)
	Labs             int // distinct trial labs
	Trials           int // all recorded trials
}

// GetNavCounts computes the sidebar counts.
func GetNavCounts(ctx context.Context, db *sql.DB) (NavCounts, error) {
	var n NavCounts
	scalar := func(dest *int, query string) error {
		if err := db.QueryRowContext(ctx, query).Scan(dest); err != nil {
			return fmt.Errorf("store.GetNavCounts: %w", err)
		}
		return nil
	}
	if err := scalar(&n.Sessions, `SELECT COUNT(*) FROM sessions`); err != nil {
		return n, err
	}
	if err := scalar(&n.Memories, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL`); err != nil {
		return n, err
	}
	if err := scalar(&n.Notes, `SELECT COUNT(*) FROM notes_index`); err != nil {
		return n, err
	}
	if err := scalar(&n.OpenTasks, `SELECT COUNT(*) FROM tasks WHERE status IN ('open','in_progress')`); err != nil {
		return n, err
	}
	if err := scalar(&n.PendingProposals, `SELECT COUNT(*) FROM gardener_proposals WHERE status = 'pending'`); err != nil {
		return n, err
	}
	if err := scalar(&n.Projects, `SELECT COUNT(*) FROM projects`); err != nil {
		return n, err
	}
	// A "plan" is a composition keyed by a plan:<slug> tag (both cc-plan captures
	// and composed plans carry it). Count distinct (project, plan:<slug>) PAIRS,
	// not distinct tag values: a plan is identified by project+slug (see the
	// console's planKey), so the same plan name running in two projects is two
	// plans and the Plans screen lists two rows. Counting the value alone would
	// badge that as one and disagree with the page it links to. Prefix matches
	// internal/plans slugTagPrefix.
	if err := scalar(&n.Plans, `SELECT COUNT(*) FROM (SELECT DISTINCT n.project, je.value FROM notes_index n, json_each(n.tags) je WHERE je.value LIKE 'plan:%')`); err != nil {
		return n, err
	}
	if err := scalar(&n.Labs, `SELECT COUNT(DISTINCT lab) FROM trials`); err != nil {
		return n, err
	}
	if err := scalar(&n.Trials, `SELECT COUNT(*) FROM trials`); err != nil {
		return n, err
	}
	return n, nil
}

// ProjectCounts are the per-project totals the console project peek shows. The
// channels do not overlap in the way coverage does; each is a plain count.
type ProjectCounts struct {
	Memories  int `json:"memories"`  // active memories in the project
	Sessions  int `json:"sessions"`  // sessions scoped to the project
	OpenTasks int `json:"openTasks"` // open or in_progress tasks
	Notes     int `json:"notes"`     // notes in the project
}

// GetProjectCounts computes the per-project roll-up for one slug in a single
// round trip (scalar subqueries). It backs the console project peek.
func GetProjectCounts(ctx context.Context, db *sql.DB, slug string) (ProjectCounts, error) {
	var c ProjectCounts
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM memories_index WHERE project = ? AND invalid_at IS NULL),
			(SELECT COUNT(*) FROM sessions WHERE project_slug = ?),
			(SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND status IN ('open','in_progress')),
			(SELECT COUNT(*) FROM notes_index WHERE project = ?)`,
		slug, slug, slug, slug).
		Scan(&c.Memories, &c.Sessions, &c.OpenTasks, &c.Notes)
	if err != nil {
		return c, fmt.Errorf("store.GetProjectCounts: %w", err)
	}
	return c, nil
}

// MemoryStat is an active memory annotated with its retrieval counts, for the
// Retrieval page's top-injected and stale lists.
type MemoryStat struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Project      string     `json:"project"`
	Injects      int        `json:"injects"`
	Reads        int        `json:"reads"`
	Sessions     int        `json:"sessions"` // distinct sessions reached (retrieval-reach view)
	Updated      time.Time  `json:"updated"`
	LastInjected *time.Time `json:"lastInjected,omitempty"`
}

// SessionCoverage measures how much Claude Code session knowledge Seamless is
// retaining: a session is "covered" when it left a durable artifact behind --
// non-empty findings, or at least one written memory, note, or recorded trial.
// It is a rough proxy for "how much of what happened in a session did we keep".
// The per-channel counts (Findings/Memories/Notes/Trials) overlap -- a session
// can be covered several ways -- so only Total and Covered partition the set.
type SessionCoverage struct {
	Total    int `json:"total"`    // all sessions (the denominator)
	Covered  int `json:"covered"`  // sessions with >=1 durable artifact
	Findings int `json:"findings"` // sessions whose findings are non-empty
	Memories int `json:"memories"` // sessions that wrote >=1 memory
	Notes    int `json:"notes"`    // sessions that created >=1 note
	Trials   int `json:"trials"`   // sessions that recorded >=1 trial
}

// GetSessionCoverage computes the coverage roll-up in a single pass over the
// sessions created within the window (a zero `since` means all time), testing
// each against the event log for durable artifacts. It reads the event log
// directly rather than retrieval_stats, so it needs no rebuild.
func GetSessionCoverage(ctx context.Context, db *sql.DB, since time.Time) (SessionCoverage, error) {
	c, err := sessionCoverage(ctx, db, "", false, since, time.Time{})
	if err != nil {
		return c, fmt.Errorf("store.GetSessionCoverage: %w", err)
	}
	return c, nil
}

// sessionCoverage is the one coverage query behind GetSessionCoverage, its
// per-project sibling, and the bounded prior-window comparison the console's
// judged metrics draw on. The window is [since, until): a zero bound is open on
// that side. byProject scopes to project_slug = project; the flag (rather than a
// non-empty project) is what keeps "" unambiguous, since an empty project_slug
// marks real global sessions.
func sessionCoverage(ctx context.Context, db *sql.DB, project string, byProject bool, since, until time.Time) (SessionCoverage, error) {
	var c SessionCoverage
	args := []any{string(core.EventMemoryWritten), string(core.EventNoteWritten), string(core.EventTrialRecorded)}
	var clauses []string
	if byProject {
		clauses = append(clauses, "s.project_slug = ?")
		args = append(args, project)
	}
	if !since.IsZero() {
		clauses = append(clauses, "s.created_at >= ?")
		args = append(args, core.FormatTime(since))
	}
	if !until.IsZero() {
		clauses = append(clauses, "s.created_at < ?")
		args = append(args, core.FormatTime(until))
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	err := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN has_findings OR has_mem OR has_note OR has_trial THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(has_findings), 0),
			COALESCE(SUM(has_mem), 0),
			COALESCE(SUM(has_note), 0),
			COALESCE(SUM(has_trial), 0)
		FROM (
			SELECT
				(s.findings <> '') AS has_findings,
				EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?) AS has_mem,
				EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?) AS has_note,
				EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?) AS has_trial
			FROM sessions s`+where+`
		)`,
		args...,
	).Scan(&c.Total, &c.Covered, &c.Findings, &c.Memories, &c.Notes, &c.Trials)
	if err != nil {
		return c, err
	}
	return c, nil
}

// CoverageBucket is one time bucket of the session-coverage trend: a pre-formatted
// tick label, how many sessions started in it, and how many of those retained
// knowledge. Total == 0 means no sessions in the bucket, so its coverage is
// undefined (the trend renders it as a dip to the floor, not a ceiling).
type CoverageBucket struct {
	Label   string `json:"label"`
	Total   int    `json:"total"`   // sessions created in the bucket
	Covered int    `json:"covered"` // of those, how many retained knowledge
}

// SessionCoverageBuckets computes the windowed session-coverage trend in the
// viewer's local time -- hourly for the 24h window, daily otherwise (for "all",
// daily from the earliest session). It applies the same covered-ness test as
// GetSessionCoverage over the same window, so the coverage hero and this trend
// describe the same set of sessions. Buckets are contiguous (a quiet stretch
// reads as a dip), and it shares localBucketAxis/bucketKey with the injection
// trend so the two charts' x-axes line up. Returns nil when the window holds no
// sessions.
func SessionCoverageBuckets(ctx context.Context, db *sql.DB, w RetrievalWindow, now time.Time) ([]CoverageBucket, error) {
	hourly := w.Key == "24h"
	// Widen the SQL bound by one bucket so a session near the window boundary is
	// not dropped before local re-bucketing.
	lower := w.Since
	if !lower.IsZero() {
		if hourly {
			lower = lower.Add(-time.Hour)
		} else {
			lower = lower.AddDate(0, 0, -1)
		}
	}
	args := []any{string(core.EventMemoryWritten), string(core.EventNoteWritten), string(core.EventTrialRecorded)}
	q := `
		SELECT
			s.created_at,
			CASE WHEN
				s.findings <> ''
				OR EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?)
				OR EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?)
				OR EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?)
			THEN 1 ELSE 0 END AS covered
		FROM sessions s`
	if !lower.IsZero() {
		q += ` WHERE s.created_at >= ?`
		args = append(args, core.FormatTime(lower))
	}
	q += ` ORDER BY s.created_at ASC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.SessionCoverageBuckets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type acc struct{ total, covered int }
	counts := map[string]*acc{}
	var earliest time.Time
	any := false
	for rows.Next() {
		var createdAt string
		var covered int
		if err := rows.Scan(&createdAt, &covered); err != nil {
			return nil, fmt.Errorf("store.SessionCoverageBuckets: scan: %w", err)
		}
		ts, err := core.ParseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("store.SessionCoverageBuckets: parse ts: %w", err)
		}
		any = true
		if earliest.IsZero() || ts.Before(earliest) {
			earliest = ts
		}
		k := bucketKey(ts, hourly)
		a := counts[k]
		if a == nil {
			a = &acc{}
			counts[k] = a
		}
		a.total++
		a.covered += covered
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SessionCoverageBuckets: rows: %w", err)
	}
	if !any {
		return nil, nil
	}

	start := w.Since
	if start.IsZero() {
		start = earliest
	}
	axis := localBucketAxis(start, now, hourly)
	out := make([]CoverageBucket, 0, len(axis))
	for _, t := range axis {
		b := CoverageBucket{Label: t.label}
		if a := counts[t.key]; a != nil {
			b.Total, b.Covered = a.total, a.covered
		}
		out = append(out, b)
	}
	return out, nil
}

// ActivitySince is the owner's check-in digest: what the fleet recorded
// between since (inclusive) and until (exclusive). Every field is a plain count
// of recorded rows -- the console's "since you were last here" line says exactly
// these numbers and links each to the screen that lists them.
type ActivitySince struct {
	Sessions        int `json:"sessions"`        // sessions started
	MemoriesWritten int `json:"memoriesWritten"` // memory.written events
	NotesWritten    int `json:"notesWritten"`    // note.written events
	TasksClosed     int `json:"tasksClosed"`     // tasks closed as done in the span
	Proposals       int `json:"proposals"`       // gardener proposals raised, any status
	Mishaps         int `json:"mishaps"`         // agent.mishap events
}

// GetActivitySince counts the check-in digest in one round trip.
func GetActivitySince(ctx context.Context, db *sql.DB, since, until time.Time) (ActivitySince, error) {
	var a ActivitySince
	lo, hi := core.FormatTime(since), core.FormatTime(until)
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sessions WHERE created_at >= ? AND created_at < ?),
			(SELECT COUNT(*) FROM events WHERE kind = ? AND ts >= ? AND ts < ?),
			(SELECT COUNT(*) FROM events WHERE kind = ? AND ts >= ? AND ts < ?),
			(SELECT COUNT(*) FROM tasks WHERE status = 'done' AND closed_at >= ? AND closed_at < ?),
			(SELECT COUNT(*) FROM gardener_proposals WHERE created_at >= ? AND created_at < ?),
			(SELECT COUNT(*) FROM events WHERE kind = ? AND ts >= ? AND ts < ?)`,
		lo, hi,
		string(core.EventMemoryWritten), lo, hi,
		string(core.EventNoteWritten), lo, hi,
		lo, hi,
		lo, hi,
		string(core.EventAgentMishap), lo, hi,
	).Scan(&a.Sessions, &a.MemoriesWritten, &a.NotesWritten, &a.TasksClosed, &a.Proposals, &a.Mishaps)
	if err != nil {
		return ActivitySince{}, fmt.Errorf("store.GetActivitySince: %w", err)
	}
	return a, nil
}
