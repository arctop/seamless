package store

// Judged-metric support: the console renders no headline number without a
// comparison, so every window it can show has to be computable over the equally
// long stretch that preceded it. WindowVitals is that comparison -- the smallest
// rollup a delta chip needs, bounded on BOTH sides, unlike the open-ended
// RetrievalReport the current window is drawn from.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// WindowVitals is a bounded window's comparable rollup: reach, injection volume,
// the sessions that received context, and knowledge continuity.
//
// ActiveMemories is the knowledge base as it stands NOW, not as it stood inside
// the window -- the same convention RetrievalReport documents. That is what makes
// a reach delta readable: both sides divide by one denominator, so the movement
// is the numerator's (how much of today's knowledge base was reaching agents
// then vs now) rather than a mix of reach and churn.
type WindowVitals struct {
	Injections       int `json:"injections"`       // item-level injections in the window
	InjectedTokens   int `json:"injectedTokens"`   // estimated tokens of injected context (reach's cost side)
	MemoriesSurfaced int `json:"memoriesSurfaced"` // distinct active memories surfaced >=1x
	SessionsReached  int `json:"sessionsReached"`  // distinct sessions that received >=1 injection
	ActiveMemories   int `json:"activeMemories"`   // active memories as of now (reach denominator)
	ReachRate        int `json:"reachRate"`        // MemoriesSurfaced / ActiveMemories, rounded %
	CovTotal         int `json:"covTotal"`         // sessions started in the window (continuity denominator)
	Covered          int `json:"covered"`          // of those, how many retained knowledge
	Coverage         int `json:"coverage"`         // Covered / CovTotal, rounded %
}

// PriorWindow returns the equally long stretch immediately before w: the window
// a delta chip compares against. ok is false when there is nothing to compare --
// the "all time" selection has no prior window, and neither does a window whose
// lower bound is not in the past.
func PriorWindow(w RetrievalWindow, now time.Time) (since, until time.Time, ok bool) {
	if w.Since.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	span := now.Sub(w.Since)
	if span <= 0 {
		return time.Time{}, time.Time{}, false
	}
	return w.Since.Add(-span), w.Since, true
}

// GetWindowVitals computes the rollup across every scope over [since, until).
// A zero bound is open on that side.
func GetWindowVitals(ctx context.Context, db *sql.DB, since, until time.Time) (WindowVitals, error) {
	v, err := windowVitals(ctx, db, "", false, since, until)
	if err != nil {
		return v, fmt.Errorf("store.GetWindowVitals: %w", err)
	}
	return v, nil
}

// GetWindowVitalsForProject computes the rollup restricted to one project: its
// own active memories are the reach denominator and only its sessions count
// toward continuity. It rejects an empty project for the same reason
// GetSessionCoverageForProject does -- an empty project_slug marks real global
// sessions, so "" cannot mean "every scope" here.
func GetWindowVitalsForProject(ctx context.Context, db *sql.DB, project string, since, until time.Time) (WindowVitals, error) {
	if project == "" {
		return WindowVitals{}, errors.New("store.GetWindowVitalsForProject: empty project is ambiguous -- project_slug='' rows are real global sessions; use GetWindowVitals for the all-scopes number, or pass an explicit slug")
	}
	v, err := windowVitals(ctx, db, project, true, since, until)
	if err != nil {
		return v, fmt.Errorf("store.GetWindowVitalsForProject: %w", err)
	}
	return v, nil
}

func windowVitals(ctx context.Context, db *sql.DB, project string, byProject bool, since, until time.Time) (WindowVitals, error) {
	var v WindowVitals
	meta, err := activeMemoryMeta(ctx, db)
	if err != nil {
		return v, err
	}
	for _, m := range meta {
		if !byProject || m.project == project {
			v.ActiveMemories++
		}
	}

	args := []any{string(core.EventInjected)}
	q := `SELECT session_id, item_id, payload FROM events WHERE kind = ?`
	if !since.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, core.FormatTime(since))
	}
	if !until.IsZero() {
		q += ` AND ts < ?`
		args = append(args, core.FormatTime(until))
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return v, fmt.Errorf("query injections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	surfaced := map[string]struct{}{}
	sessions := map[string]struct{}{}
	for rows.Next() {
		var sessionID, itemID, payload string
		if err := rows.Scan(&sessionID, &itemID, &payload); err != nil {
			return v, fmt.Errorf("scan injection: %w", err)
		}
		p, _ := decodeInjectedPayload(payload)
		ids := injectedItemIDs(itemID, p)
		cost := injectedTokenCost(payload)
		if !byProject {
			// All-scope cost counts each event once, matching
			// RetrievalReport.InjectedTokens so the delta compares like with like.
			v.InjectedTokens += cost
		}
		counted := false
		countedIDs := 0
		for _, id := range ids {
			m, active := meta[id]
			if byProject && (!active || m.project != project) {
				// A project's volume is the volume attributable to ITS memories,
				// so a global memory injected into one of its sessions is not
				// this project's reach.
				continue
			}
			v.Injections++
			counted = true
			countedIDs++
			if active {
				surfaced[id] = struct{}{}
			}
		}
		if byProject && countedIDs > 0 {
			// Same equal-share attribution as the report's per-memory token
			// split: one event records one cost for its whole content block.
			v.InjectedTokens += int(float64(cost)*float64(countedIDs)/float64(len(ids)) + 0.5)
		}
		if counted {
			if s := injectedSessionKey(sessionID, p); s != "" {
				sessions[s] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("injections: %w", err)
	}
	v.MemoriesSurfaced = len(surfaced)
	v.SessionsReached = len(sessions)
	if v.ActiveMemories > 0 {
		v.ReachRate = int(float64(v.MemoriesSurfaced)/float64(v.ActiveMemories)*100 + 0.5)
	}

	cov, err := sessionCoverage(ctx, db, project, byProject, since, until)
	if err != nil {
		return v, fmt.Errorf("coverage: %w", err)
	}
	v.CovTotal, v.Covered = cov.Total, cov.Covered
	if cov.Total > 0 {
		v.Coverage = int(float64(cov.Covered)/float64(cov.Total)*100 + 0.5)
	}
	return v, nil
}
