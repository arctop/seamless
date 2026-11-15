// The task read surface: the ready queue, the blocked list with each task's
// blockers, and the plain per-project/per-plan listings. "Ready" means open with
// no blocker still open or in_progress and no open gate -- one rule, sqlReady in
// tasks_gates.go, which ClaimTask's branch (a) and the plan rollups build on too.
package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/arctop/seamless/internal/core"
)

// ReadyTasks returns the actionable tasks for a project: open tasks with no
// blocking dependency still open or in_progress (done/dropped deps do not block)
// and no open gate (a gate-kind task is never ready, and a step whose own gate
// is open waits for it to clear). in_progress tasks are excluded (already
// claimed) but still block their dependents. Ordered oldest-created first, ties
// broken by id (ULID-monotonic), so the queue is stable and agent-predictable.
func ReadyTasks(ctx context.Context, db *sql.DB, project string) ([]core.Task, error) {
	rows, err := queueTasks(ctx, db, sqlReady("t"), oldestFirst, queueScope{project: project})
	if err != nil {
		return nil, fmt.Errorf("store.ReadyTasks: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// ReadyTasksForPlan returns the ready (claimable) step tasks of one plan in a
// project: open, unclaimed, with no unfinished blocker and no open gate. Same
// readiness rule as ReadyTasks, scoped to plan_slug instead of excluding plan
// tasks.
func ReadyTasksForPlan(ctx context.Context, db *sql.DB, project, plan string) ([]core.Task, error) {
	rows, err := queueTasks(ctx, db, sqlReady("t"), oldestFirst, queueScope{project: project, plan: plan})
	if err != nil {
		return nil, fmt.Errorf("store.ReadyTasksForPlan: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// AllReadyTasks returns the ready tasks across every project (see ReadyTasks for
// the readiness rule), oldest-created first. It backs the console Tasks page.
func AllReadyTasks(ctx context.Context, db *sql.DB) ([]core.Task, error) {
	rows, err := queueTasks(ctx, db, sqlReady("t"), oldestFirst, queueScope{all: true})
	if err != nil {
		return nil, fmt.Errorf("store.AllReadyTasks: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// AllBlockedTasks returns open-but-not-ready tasks across every project, each
// with its still-open blockers. A task that waits on people is not among them
// (AllGatedTasks, AllGateTasks).
func AllBlockedTasks(ctx context.Context, db *sql.DB) ([]BlockedTask, error) {
	return blockedTasks(ctx, db, queueScope{all: true})
}

// AllTasksByStatus returns every task with the given status across all projects,
// newest-updated first.
func AllTasksByStatus(ctx context.Context, db *sql.DB, status core.TaskStatus) ([]core.Task, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE status = ? ORDER BY updated_at DESC, id DESC`, string(status))
	if err != nil {
		return nil, fmt.Errorf("store.AllTasksByStatus: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// ListTasks returns a project's tasks, optionally filtered by status, newest
// first. An empty status returns every status.
func ListTasks(ctx context.Context, db *sql.DB, project string, status core.TaskStatus) ([]core.Task, error) {
	query := `SELECT ` + taskCols + ` FROM tasks WHERE project_slug = ? AND plan_slug = ''`
	args := []any{project}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, string(status))
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ListTasks: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// ListTasksForPlan returns a plan's step tasks in a project, optionally filtered
// by status, newest first. Unlike ListTasks it includes (only) plan-scoped tasks.
func ListTasksForPlan(ctx context.Context, db *sql.DB, project string, status core.TaskStatus, plan string) ([]core.Task, error) {
	query := `SELECT ` + taskCols + ` FROM tasks WHERE project_slug = ? AND plan_slug = ?`
	args := []any{project, plan}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, string(status))
	}
	query += ` ORDER BY created_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ListTasksForPlan: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// BlockedTask is an open task that is not ready, paired with the blockers still
// keeping it off the ready queue (open/in_progress dependencies).
type BlockedTask struct {
	Task     core.Task   `json:"task"`
	Blockers []core.Task `json:"blockers"`
}

// BlockedTasks returns a project's open-but-not-ready tasks, each with its
// still-open blockers, so a caller can render the dependency chain legibly. A
// task that waits on people -- a gate, a step whose own gate is open, or one
// an open gate blocks -- is listed by GatedTasks or GateTasks instead.
func BlockedTasks(ctx context.Context, db *sql.DB, project string) ([]BlockedTask, error) {
	return blockedTasks(ctx, db, queueScope{project: project})
}

// BlockedTasksForPlan returns a plan's open-but-not-ready step tasks, each with
// its still-open blockers (BlockedTasks' rule).
func BlockedTasksForPlan(ctx context.Context, db *sql.DB, project, plan string) ([]BlockedTask, error) {
	return blockedTasks(ctx, db, queueScope{project: project, plan: plan})
}

// blockedTasks backs BlockedTasks, BlockedTasksForPlan and AllBlockedTasks.
func blockedTasks(ctx context.Context, db *sql.DB, scope queueScope) ([]BlockedTask, error) {
	rows, err := queueTasks(ctx, db, sqlBlocked("t"), oldestFirst, scope)
	if err != nil {
		return nil, fmt.Errorf("store.BlockedTasks: %w", err)
	}
	return attachBlockers(ctx, db, rows)
}

// attachBlockers drains blocked-task rows and pairs each with its still-open
// blockers, fetched in one batched query (not one per task).
func attachBlockers(ctx context.Context, db *sql.DB, rows *sql.Rows) ([]BlockedTask, error) {
	tasks, err := scanTasksWithDeps(ctx, db, rows)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	blockers, err := openBlockersForTasks(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	out := make([]BlockedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, BlockedTask{Task: t, Blockers: blockers[t.ID]})
	}
	return out, nil
}

// openBlockersForTasks returns the open/in_progress dependencies (blockers) of
// many tasks in a handful of batched IN queries instead of one query per task,
// keyed by task id and ordered by created_at then id within each task. Tasks with
// no open blockers are absent from the map.
func openBlockersForTasks(ctx context.Context, db *sql.DB, taskIDs []string) (map[string][]core.Task, error) {
	out := make(map[string][]core.Task, len(taskIDs))
	for start := 0; start < len(taskIDs); start += depsBatchSize {
		batch := taskIDs[start:min(start+depsBatchSize, len(taskIDs))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := db.QueryContext(ctx, `SELECT d.task_id, `+prefixCols("b", taskCols)+`
			FROM task_deps d JOIN tasks b ON b.id = d.depends_on
			WHERE d.task_id IN (`+placeholders(len(batch))+`) AND b.status IN ('open','in_progress')
			ORDER BY d.task_id ASC, b.created_at ASC, b.id ASC`, args...)
		if err != nil {
			return nil, fmt.Errorf("store.openBlockersForTasks: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var taskID string
				t, err := scanTask(rows, &taskID)
				if err != nil {
					return fmt.Errorf("store.openBlockersForTasks: %w", err)
				}
				out[taskID] = append(out[taskID], t)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TasksBlockedBy returns the tasks that depend on id -- the inverse of a task's
// DependsOn edges, i.e. the tasks this one blocks. Unlike openBlockers it applies
// no status filter (closed dependents are included too), oldest-created first. It
// backs the console task peek's reverse "blocks" section.
func TasksBlockedBy(ctx context.Context, db *sql.DB, id string) ([]core.Task, error) {
	if id == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+prefixCols("t", taskCols)+`
		FROM task_deps d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on = ?
		ORDER BY t.created_at ASC, t.id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("store.TasksBlockedBy: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}
