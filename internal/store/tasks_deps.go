// The task dependency DAG: edge insertion with dangling-reference and cycle
// rejection, plus the depends-on reads (single and batched) the queue builds on.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// cycleWalkCap bounds the dependency walk in createsCycle, a backstop against a
// pathological graph (the edges form a DAG by construction, so this is never hit
// in practice).
const cycleWalkCap = 10000

// ErrTaskCycle is returned when adding a dependency edge would create a cycle.
var ErrTaskCycle = errors.New("dependency would create a cycle")

// addDeps inserts depends-on edges for taskID, rejecting dangling references and
// cycles. Duplicate edges are ignored. It runs inside the caller's transaction.
func addDeps(ctx context.Context, tx *sql.Tx, taskID string, deps []string) error {
	for _, dep := range dedupeStrings(deps) {
		if dep == taskID {
			return fmt.Errorf("store: task %q cannot depend on itself", taskID)
		}
		exists, err := taskExists(ctx, tx, dep)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("store: %w: depends_on %q", ErrTaskNotFound, dep)
		}
		cyclic, err := createsCycle(ctx, tx, taskID, dep)
		if err != nil {
			return err
		}
		if cyclic {
			return fmt.Errorf("store: %w: %q -> %q", ErrTaskCycle, taskID, dep)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_deps (task_id, depends_on) VALUES (?, ?)`,
			taskID, dep); err != nil {
			return fmt.Errorf("store: insert dep: %w", err)
		}
	}
	return nil
}

// createsCycle reports whether adding the edge taskID -> dep (taskID is blocked
// by dep) would close a cycle: it walks dep's existing depends-on chain and
// returns true if it can already reach taskID.
func createsCycle(ctx context.Context, q rowQuerier, taskID, dep string) (bool, error) {
	stack := []string{dep}
	visited := make(map[string]bool)
	for steps := 0; len(stack) > 0 && steps < cycleWalkCap; steps++ {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == taskID {
			return true, nil
		}
		if visited[cur] {
			continue
		}
		visited[cur] = true
		children, err := dependsOnOf(ctx, q, cur)
		if err != nil {
			return false, err
		}
		stack = append(stack, children...)
	}
	return false, nil
}

// dependsOnOf returns the ids a task directly depends on.
func dependsOnOf(ctx context.Context, q rowQuerier, taskID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT depends_on FROM task_deps WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: dependsOnOf: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: dependsOnOf scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// depsBatchSize caps the ids per IN clause in dependsOnForTasks, comfortably
// under SQLite's bound-parameter limit.
const depsBatchSize = 500

// dependsOnForTasks returns the depends-on ids for many tasks in a handful of
// batched IN queries (instead of one query per task), keyed by task id and
// ordered by depends_on within each task (the task_deps primary-key order, the
// same order dependsOnOf yields). Tasks with no deps are absent from the map.
func dependsOnForTasks(ctx context.Context, q rowQuerier, taskIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(taskIDs))
	for start := 0; start < len(taskIDs); start += depsBatchSize {
		batch := taskIDs[start:min(start+depsBatchSize, len(taskIDs))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		// The rows ARE closed, by the deferred Close in the closure below.
		// sqlclosecheck only recognizes a defer in the same function scope as the
		// query, and this is a batch loop: a plain defer here would hold every
		// batch's rows open until the whole loop returned.
		//nolint:sqlclosecheck // closed by the closure's defer, see above
		rows, err := q.QueryContext(ctx, `SELECT task_id, depends_on FROM task_deps
			WHERE task_id IN (`+placeholders(len(batch))+`)
			ORDER BY task_id ASC, depends_on ASC`, args...)
		if err != nil {
			return nil, fmt.Errorf("store: dependsOnForTasks: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var taskID, dep string
				if err := rows.Scan(&taskID, &dep); err != nil {
					return fmt.Errorf("store: dependsOnForTasks scan: %w", err)
				}
				out[taskID] = append(out[taskID], dep)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TaskProjects returns the project of each task among ids, keyed by id, in a
// handful of batched IN queries; unknown ids are absent. It is the read fence's
// input for a dependency list: a depends_on edge names only the task id, and a
// surface judges each against its caller by the project the task sits in.
func TaskProjects(ctx context.Context, db *sql.DB, ids []string) (map[string]string, error) {
	ids = dedupeStrings(ids)
	out := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += depsBatchSize {
		batch := ids[start:min(start+depsBatchSize, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		//nolint:sqlclosecheck // closed by the closure's defer, as in dependsOnForTasks
		rows, err := db.QueryContext(ctx, `SELECT id, project_slug FROM tasks
			WHERE id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("store.TaskProjects: %w", err)
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id, project string
				if err := rows.Scan(&id, &project); err != nil {
					return fmt.Errorf("store.TaskProjects: scan: %w", err)
				}
				out[id] = project
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func taskExists(ctx context.Context, q rowQuerier, id string) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT 1 FROM tasks WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return false, fmt.Errorf("store: taskExists: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return rows.Next(), rows.Err()
}

// dedupeStrings returns s with blanks dropped and duplicates removed, order
// preserved.
func dedupeStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	var out []string
	for _, v := range s {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
