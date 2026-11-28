// Plan rollups: a plan's status is derived from its step tasks, never stored.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// PlanRollup is the per-plan aggregate the briefing surfaces: Total step tasks,
// how many are Done (closed), InFlight (in_progress), and Claimable (ready).
// GatesOpen counts the gates still waiting on people: the plan's gate-kind
// tasks open or in progress, and its unfinished steps whose own gate is open.
// LastActivity is the newest updated_at across the plan's steps.
type PlanRollup struct {
	Slug         string    `json:"slug"`
	Total        int       `json:"total"`
	Done         int       `json:"done"`
	InFlight     int       `json:"inFlight"`
	Claimable    int       `json:"claimable"`
	GatesOpen    int       `json:"gatesOpen,omitempty"`
	LastActivity time.Time `json:"lastActivity"`
}

// PlanFinishLinePct is the closed-step share at which an active plan is "at
// the finish line" -- the qualification the momentum feature's Overview card
// and briefing emphasis both derive from, so the two surfaces agree by
// construction.
const PlanFinishLinePct = 80

// AtFinishLine reports whether this rollup is an active plan within reach of
// done: at least PlanFinishLinePct of its steps closed, with work remaining.
// Integer math, never rounded up: 4/5 qualifies, 3/4 (75%) does not.
func (p PlanRollup) AtFinishLine() bool {
	return p.Total > 0 && p.Done < p.Total && p.Done*100 >= p.Total*PlanFinishLinePct
}

// StepsLeft is how many steps remain open or in flight.
func (p PlanRollup) StepsLeft() int { return p.Total - p.Done }

// FinishLinePhrase renders the distance-to-done phrase both momentum surfaces
// share, so the card and the briefing cannot drift: "one step from shipped".
func (p PlanRollup) FinishLinePhrase() string {
	if left := p.StepsLeft(); left != 1 {
		return fmt.Sprintf("%d steps from shipped", left)
	}
	return "one step from shipped"
}

// ActivePlans returns a rollup for each not-yet-complete plan in a project
// (plans whose every step is closed are omitted, like a done stage). Claimable
// counts ready open steps (same readiness rule as ReadyTasksForPlan, sqlReady)
// and GatesOpen the gates waiting on people (sqlGatesOpen); the
// plan's status is derived, never stored. One grouped query covers every plan.
// Ordered most-recently-active first (newest step updated_at), ties broken by
// slug, so the briefing and the console lead with what just moved.
//
// This is a briefing contract: dropping the complete plans is the whole point,
// so the filter lives here rather than in the shared query.
func ActivePlans(ctx context.Context, db *sql.DB, project string) ([]PlanRollup, error) {
	all, err := planRollups(ctx, db, project)
	if err != nil {
		return nil, err
	}
	var plans []PlanRollup
	for _, p := range all {
		if p.Done >= p.Total {
			continue // every step closed -> plan complete, not active
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// PlanRollupsForProject returns a rollup for EVERY plan in a project, complete
// or not, in the same order. The console's plan workspace partitions active
// from completed itself (a finished plan still gets a rollup line); ActivePlans
// is this set with the complete plans dropped, so the two cannot disagree.
func PlanRollupsForProject(ctx context.Context, db *sql.DB, project string) ([]PlanRollup, error) {
	return planRollups(ctx, db, project)
}

// planRollups is the shared grouped query behind both plan rollup views.
func planRollups(ctx context.Context, db *sql.DB, project string) ([]PlanRollup, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT plan_slug,
		       COUNT(*),
		       SUM(CASE WHEN status IN ('done','dropped') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+sqlReady("tasks")+` THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+sqlGatesOpen("tasks")+` THEN 1 ELSE 0 END),
		       MAX(updated_at)
		  FROM tasks
		 WHERE project_slug = ? AND plan_slug <> ''
		 GROUP BY plan_slug
		 ORDER BY MAX(updated_at) DESC, plan_slug ASC`, project)
	if err != nil {
		return nil, fmt.Errorf("store.planRollups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var plans []PlanRollup
	for rows.Next() {
		var p PlanRollup
		var last string
		if err := rows.Scan(&p.Slug, &p.Total, &p.Done, &p.InFlight, &p.Claimable, &p.GatesOpen, &last); err != nil {
			return nil, fmt.Errorf("store.planRollups: scan: %w", err)
		}
		if p.LastActivity, err = core.ParseTime(last); err != nil {
			return nil, fmt.Errorf("store.planRollups: parse last activity: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.planRollups: %w", err)
	}
	return plans, nil
}

// ProjectPlan is a plan rollup paired with the project that owns it, for the
// cross-project surfaces (a plan is keyed by project+slug, never slug alone).
type ProjectPlan struct {
	PlanRollup
	Project string `json:"project"`
}

// AllPlanRollups returns a rollup for every plan across every project, complete
// or not, most recently active first. It backs the Now screen's moving-plans
// rail; per-project views use PlanRollupsForProject.
func AllPlanRollups(ctx context.Context, db *sql.DB) ([]ProjectPlan, error) {
	return allPlanRollups(ctx, db)
}

// allPlanRollups is the shared all-projects grouped query: the same aggregate
// as planRollups with project_slug in the grouping key, so the per-project and
// cross-project views cannot drift.
func allPlanRollups(ctx context.Context, db *sql.DB) ([]ProjectPlan, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT project_slug, plan_slug,
		       COUNT(*),
		       SUM(CASE WHEN status IN ('done','dropped') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status = 'in_progress' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+sqlReady("tasks")+` THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+sqlGatesOpen("tasks")+` THEN 1 ELSE 0 END),
		       MAX(updated_at)
		  FROM tasks
		 WHERE plan_slug <> ''
		 GROUP BY project_slug, plan_slug
		 ORDER BY MAX(updated_at) DESC, project_slug ASC, plan_slug ASC`)
	if err != nil {
		return nil, fmt.Errorf("store.allPlanRollups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var plans []ProjectPlan
	for rows.Next() {
		var p ProjectPlan
		var last string
		if err := rows.Scan(&p.Project, &p.Slug, &p.Total, &p.Done, &p.InFlight, &p.Claimable, &p.GatesOpen, &last); err != nil {
			return nil, fmt.Errorf("store.allPlanRollups: scan: %w", err)
		}
		if p.LastActivity, err = core.ParseTime(last); err != nil {
			return nil, fmt.Errorf("store.allPlanRollups: parse last activity: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.allPlanRollups: %w", err)
	}
	return plans, nil
}

// FinishLinePlan is one near-done plan for the momentum finish-line card: its
// rollup, the project it belongs to, and the exact remaining step titles.
type FinishLinePlan struct {
	PlanRollup
	Project   string   `json:"project"`
	Remaining []string `json:"remaining"`
}

// FinishLinePlans returns every active plan at the finish line (AtFinishLine)
// across all projects, most recently active first, each carrying the titles of
// its not-yet-closed steps oldest-first: momentum's finish-line cards.
func FinishLinePlans(ctx context.Context, db *sql.DB) ([]FinishLinePlan, error) {
	all, err := allPlanRollups(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("store.FinishLinePlans: %w", err)
	}
	var plans []FinishLinePlan
	for _, p := range all {
		if p.AtFinishLine() {
			plans = append(plans, FinishLinePlan{PlanRollup: p.PlanRollup, Project: p.Project})
		}
	}
	for i := range plans {
		if plans[i].Remaining, err = planRemainingTitles(ctx, db, plans[i].Project, plans[i].Slug); err != nil {
			return nil, err
		}
	}
	return plans, nil
}

// planRemainingTitles lists a plan's not-yet-closed step titles, oldest first --
// the "exact remaining steps" the finish-line card names.
func planRemainingTitles(ctx context.Context, db *sql.DB, project, plan string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT title FROM tasks
		 WHERE project_slug = ? AND plan_slug = ? AND status IN ('open','in_progress')
		 ORDER BY created_at ASC`, project, plan)
	if err != nil {
		return nil, fmt.Errorf("store.planRemainingTitles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var titles []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("store.planRemainingTitles: scan: %w", err)
		}
		titles = append(titles, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.planRemainingTitles: %w", err)
	}
	return titles, nil
}

// PlanStepProjects names the projects holding a step of plan, each once, by
// slug ("" is the global scope): a plan is keyed by project and slug, so a
// slug with steps in two projects is two plans (plan:console-ux-pass UX 07).
// Read through a request's handle, a locked project it does not read is not
// named.
func PlanStepProjects(ctx context.Context, db *sql.DB, plan string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT project_slug FROM tasks
		 WHERE plan_slug = ? AND plan_slug <> ''
		 ORDER BY project_slug ASC`, plan)
	if err != nil {
		return nil, fmt.Errorf("store.PlanStepProjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("store.PlanStepProjects: scan: %w", err)
		}
		out = append(out, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PlanStepProjects: %w", err)
	}
	return out, nil
}

// PlanDroppedCounts counts each plan's dropped steps, keyed by project and
// slug; a plan with none is absent. A rollup's Done counts done and dropped
// alike (a plan whose every step closed is complete), and this says how many
// of those closed steps were dropped rather than done.
func PlanDroppedCounts(ctx context.Context, db *sql.DB) (map[PlanRef]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT project_slug, plan_slug, COUNT(*) FROM tasks
		 WHERE plan_slug <> '' AND status = 'dropped'
		 GROUP BY project_slug, plan_slug`)
	if err != nil {
		return nil, fmt.Errorf("store.PlanDroppedCounts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[PlanRef]int{}
	for rows.Next() {
		var (
			ref PlanRef
			n   int
		)
		if err := rows.Scan(&ref.Project, &ref.Slug, &n); err != nil {
			return nil, fmt.Errorf("store.PlanDroppedCounts: scan: %w", err)
		}
		out[ref] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PlanDroppedCounts: %w", err)
	}
	return out, nil
}
