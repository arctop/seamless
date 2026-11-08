// Row scanning for the tasks table. scanTasksWithDeps is the batched path every
// multi-row task query drains through, so listing N tasks costs 1 query for the
// rows plus a handful for their edges -- not N+1.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// scanTasksWithDeps drains task rows and populates each task's DependsOn ids
// with one batched query (not one per task).
func scanTasksWithDeps(ctx context.Context, db *sql.DB, rows *sql.Rows) ([]core.Task, error) {
	defer func() { _ = rows.Close() }()
	var tasks []core.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.scanTasksWithDeps: %w", err)
	}
	if len(tasks) == 0 {
		return tasks, nil
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	deps, err := dependsOnForTasks(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].DependsOn = deps[tasks[i].ID]
	}
	return tasks, nil
}

// scanTask scans one taskCols row. lead holds destinations for any columns
// selected before taskCols (e.g. a grouping key in a batched join); callers that
// select only taskCols pass none.
func scanTask(rows *sql.Rows, lead ...any) (core.Task, error) {
	var (
		t                core.Task
		status           string
		created, updated string
		lease, closed    sql.NullString
	)
	dest := make([]any, 0, len(lead)+15)
	dest = append(dest, lead...)
	dest = append(dest, &t.ID, &t.ProjectSlug, &t.Title, &t.Body, &status,
		&t.CreatedBy, &t.PlanSlug, &t.ClaimedBy, &lease, &t.Favorite, &created, &updated, &closed,
		&t.Account, &t.UpdatedAccount)
	if err := rows.Scan(dest...); err != nil {
		return core.Task{}, err
	}
	t.Status = core.TaskStatus(status)
	var err error
	if t.CreatedAt, err = core.ParseTime(created); err != nil {
		return core.Task{}, fmt.Errorf("created_at: %w", err)
	}
	if t.UpdatedAt, err = core.ParseTime(updated); err != nil {
		return core.Task{}, fmt.Errorf("updated_at: %w", err)
	}
	if t.LeaseExpiresAt, err = nullTimePtr(lease); err != nil {
		return core.Task{}, fmt.Errorf("lease_expires_at: %w", err)
	}
	if t.ClosedAt, err = nullTimePtr(closed); err != nil {
		return core.Task{}, fmt.Errorf("closed_at: %w", err)
	}
	return t, nil
}

// prefixCols rewrites a comma-separated column list to alias.col form, so a
// joined query can select one table's columns unambiguously.
func prefixCols(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
