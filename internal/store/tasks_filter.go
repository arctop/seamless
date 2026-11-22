// The console Tasks page's filtered reads (plan:console-ux-pass UX 08): the
// queue's groups narrowed by project, plan and text, the closed tasks a page at
// a time, the open tasks per project for its menu, when each task's state
// began, and a task's history. They build on the queue's predicates
// (tasks_gates.go) and add none of their own, so the page's groups stay the
// partition the board counts (TaskCountsByProject).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// TaskFilter narrows the console's task lists. The zero filter is the
// default list: every project's tasks outside a plan, which is what
// tasks_ready means for agents.
type TaskFilter struct {
	// ByProject narrows to Project's tasks; Project "" is then the global
	// scope.
	ByProject bool
	Project   string
	// Plan narrows to that plan's steps. Without it, Steps includes the plan
	// steps beside the tasks outside a plan, and neither keeps those alone.
	Plan  string
	Steps bool
	// Query keeps the tasks whose title or body contains it (any case), whose
	// id it is, or -- 8 characters or more of a ULID's alphabet -- whose id
	// begins or ends with it: the console prints a task's last 8 characters,
	// so a pasted short id finds its task.
	Query string
}

// where renders the filter's conditions over alias, each led by AND.
func (f TaskFilter) where(alias string) (string, []any) {
	var (
		b    strings.Builder
		args []any
	)
	if f.ByProject {
		b.WriteString(" AND " + alias + ".project_slug = ?")
		args = append(args, f.Project)
	}
	switch {
	case f.Plan != "":
		b.WriteString(" AND " + alias + ".plan_slug = ?")
		args = append(args, f.Plan)
	case !f.Steps:
		b.WriteString(" AND " + alias + ".plan_slug = ''")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		pred, qargs := taskTextPredicate(alias, q)
		b.WriteString(" AND (" + pred + ")")
		args = append(args, qargs...)
	}
	return b.String(), args
}

// taskTextPredicate matches q in a task's title or body, as its whole id, or
// as the start or end of its id (taskIDPart).
func taskTextPredicate(alias, q string) (string, []any) {
	needle := likeContains(q)
	pred := alias + `.title LIKE ? ESCAPE '\' OR ` + alias + `.body LIKE ? ESCAPE '\' OR ` +
		alias + `.id = ? COLLATE NOCASE`
	args := []any{needle, needle, q}
	if part, ok := taskIDPart(q); ok {
		pred += ` OR ` + alias + `.id LIKE ? ESCAPE '\' OR ` + alias + `.id LIKE ? ESCAPE '\'`
		args = append(args, escapeLikePrefix(part)+"%", "%"+escapeLikePrefix(part))
	}
	return pred, args
}

// taskIDPart reads q as part of a task id: 8 to 25 characters of the ULID
// alphabet (Crockford base32), upper-cased. A shorter word would match ids by
// chance; 26 is a whole id, which the exact arm matches.
func taskIDPart(q string) (string, bool) {
	part := strings.ToUpper(strings.TrimSpace(q))
	if len(part) < minSearchIDPrefix || len(part) >= 26 {
		return "", false
	}
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, r := range part {
		if !strings.ContainsRune(crockford, r) {
			return "", false
		}
	}
	return part, true
}

// filteredQueue runs one queue group's query (group, a predicate over t) under
// the filter, ordered by order.
func filteredQueue(ctx context.Context, db *sql.DB, group, order string, f TaskFilter) (*sql.Rows, error) {
	where, args := f.where("t")
	return db.QueryContext(ctx, `SELECT `+prefixCols("t", taskCols)+` FROM tasks t
		WHERE `+group+where+`
		ORDER BY `+order, args...)
}

// FilteredReadyTasks is the ready group (sqlReady) under f, oldest-created
// first: the order agents claim in.
func FilteredReadyTasks(ctx context.Context, db *sql.DB, f TaskFilter) ([]core.Task, error) {
	rows, err := filteredQueue(ctx, db, sqlReady("t"), oldestFirst, f)
	if err != nil {
		return nil, fmt.Errorf("store.FilteredReadyTasks: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// FilteredBlockedTasks is the blocked group (sqlBlocked) under f, each task
// with its unfinished blockers, oldest first.
func FilteredBlockedTasks(ctx context.Context, db *sql.DB, f TaskFilter) ([]BlockedTask, error) {
	rows, err := filteredQueue(ctx, db, sqlBlocked("t"), oldestFirst, f)
	if err != nil {
		return nil, fmt.Errorf("store.FilteredBlockedTasks: %w", err)
	}
	return attachBlockers(ctx, db, rows)
}

// FilteredGatedTasks is the steps an open gate holds (sqlGated) under f, each
// with its unfinished blockers, oldest first.
func FilteredGatedTasks(ctx context.Context, db *sql.DB, f TaskFilter) ([]BlockedTask, error) {
	rows, err := filteredQueue(ctx, db, sqlGated("t"), oldestFirst, f)
	if err != nil {
		return nil, fmt.Errorf("store.FilteredGatedTasks: %w", err)
	}
	return attachBlockers(ctx, db, rows)
}

// FilteredGateTasks is the gates open or in progress (sqlGates) under f, each
// with its unfinished blockers, by who must act and then oldest first.
func FilteredGateTasks(ctx context.Context, db *sql.DB, f TaskFilter) ([]BlockedTask, error) {
	rows, err := filteredQueue(ctx, db, sqlGates("t"), byWho, f)
	if err != nil {
		return nil, fmt.Errorf("store.FilteredGateTasks: %w", err)
	}
	return attachBlockers(ctx, db, rows)
}

// FilteredStartedTasks is the steps in progress under f -- held by a live
// claim, lapsed, or started with no claim; a gate someone started stays with
// the gates (FilteredGateTasks) -- oldest-created first.
func FilteredStartedTasks(ctx context.Context, db *sql.DB, f TaskFilter) ([]core.Task, error) {
	rows, err := filteredQueue(ctx, db, "t.status = 'in_progress' AND t.kind <> 'gate'", oldestFirst, f)
	if err != nil {
		return nil, fmt.Errorf("store.FilteredStartedTasks: %w", err)
	}
	return scanTasksWithDeps(ctx, db, rows)
}

// FilteredClosedTasks is one page of the done and dropped tasks under f,
// newest-closed first (a row with no closed_at by its last update), limit to
// a page from offset, and how many there are in all.
func FilteredClosedTasks(ctx context.Context, db *sql.DB, f TaskFilter, limit, offset int) ([]core.Task, int, error) {
	where, args := f.where("t")
	const closed = "t.status IN ('done','dropped')"
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks t WHERE `+closed+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store.FilteredClosedTasks: count: %w", err)
	}
	if total == 0 || limit <= 0 {
		return nil, total, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+prefixCols("t", taskCols)+` FROM tasks t
		WHERE `+closed+where+`
		ORDER BY COALESCE(t.closed_at, t.updated_at) DESC, t.id DESC
		LIMIT ? OFFSET ?`, append(args, limit, max(offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("store.FilteredClosedTasks: %w", err)
	}
	tasks, err := scanTasksWithDeps(ctx, db, rows)
	if err != nil {
		return nil, 0, fmt.Errorf("store.FilteredClosedTasks: %w", err)
	}
	return tasks, total, nil
}

// OpenTaskCountsByProject counts the unfinished tasks (open or in progress)
// under f in each project that has one, keyed by slug ("" is the global
// scope), for a menu of projects: f's own project, if any, is not applied.
func OpenTaskCountsByProject(ctx context.Context, db *sql.DB, f TaskFilter) (map[string]int, error) {
	f.ByProject, f.Project = false, ""
	where, args := f.where("t")
	rows, err := db.QueryContext(ctx, `SELECT t.project_slug, COUNT(*) FROM tasks t
		WHERE t.status IN ('open','in_progress')`+where+`
		GROUP BY t.project_slug`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.OpenTaskCountsByProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var (
			slug string
			n    int
		)
		if err := rows.Scan(&slug, &n); err != nil {
			return nil, fmt.Errorf("store.OpenTaskCountsByProject: scan: %w", err)
		}
		out[slug] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.OpenTaskCountsByProject: %w", err)
	}
	return out, nil
}

// TaskProjectExists reports whether any task, in any status, sits in project
// ("" is the global scope).
func TaskProjectExists(ctx context.Context, db *sql.DB, project string) (bool, error) {
	var found bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE project_slug = ?)`, project).Scan(&found); err != nil {
		return false, fmt.Errorf("store.TaskProjectExists: %w", err)
	}
	return found, nil
}

// TaskPlanExists reports whether any task, in any status, is a step of plan.
func TaskPlanExists(ctx context.Context, db *sql.DB, plan string) (bool, error) {
	if plan == "" {
		return false, nil
	}
	var found bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE plan_slug = ?)`, plan).Scan(&found); err != nil {
		return false, fmt.Errorf("store.TaskPlanExists: %w", err)
	}
	return found, nil
}

// AnyPlanStep reports whether any task, in any status, is a plan's step.
func AnyPlanStep(ctx context.Context, db *sql.DB) (bool, error) {
	var found bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE plan_slug <> '')`).Scan(&found); err != nil {
		return false, fmt.Errorf("store.AnyPlanStep: %w", err)
	}
	return found, nil
}

// TaskSince is when a task's state began, as far as the record tells. Each
// is zero when nothing records it.
type TaskSince struct {
	// Status is when the task's latest run of task.transition events whose
	// to is its current status began: when it was created, reopened or
	// released (open), first claimed or started (in progress), or closed. A
	// heartbeat's re-claim, an edit, or a gate set or cleared continues the
	// run. Zero when the task's latest recorded status is another one.
	Status time.Time
	// GateSet is the latest task.transition that set the task's gate.
	GateSet time.Time
	// BlockersClosed is the latest closed_at among the task's finished
	// dependencies: when the last of them closed.
	BlockersClosed time.Time
}

// TaskStateSince reads, for each of tasks (each with its status as read), when
// its state began (TaskSince): two batched queries, whatever the count.
func TaskStateSince(ctx context.Context, db *sql.DB, tasks []core.Task) (map[string]TaskSince, error) {
	out := make(map[string]TaskSince, len(tasks))
	status := make(map[string]string, len(tasks))
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if _, seen := status[t.ID]; !seen {
			ids = append(ids, t.ID)
		}
		status[t.ID] = string(t.Status)
	}
	for start := 0; start < len(ids); start += depsBatchSize {
		batch := ids[start:min(start+depsBatchSize, len(ids))]
		if err := taskRunStarts(ctx, db, batch, status, out); err != nil {
			return nil, err
		}
		if err := taskBlockersClosed(ctx, db, batch, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// taskRunStarts fills each batch task's Status and GateSet from its
// task.transition events, read oldest first.
func taskRunStarts(ctx context.Context, db *sql.DB, batch []string, status map[string]string, out map[string]TaskSince) error {
	args := make([]any, 0, len(batch)+1)
	args = append(args, string(core.EventTaskTransition))
	for _, id := range batch {
		args = append(args, id)
	}
	// +kind keeps the planner on the item index: one task's events, in order.
	rows, err := db.QueryContext(ctx, `SELECT item_id, ts,
		       COALESCE(CASE WHEN json_valid(payload) THEN json_extract(payload, '$.to') END, ''),
		       COALESCE(CASE WHEN json_valid(payload) THEN json_extract(payload, '$.gate') END, '')
		  FROM events
		 WHERE +kind = ? AND item_id IN (`+placeholders(len(batch))+`)
		 ORDER BY item_id, ts, id`, args...)
	if err != nil {
		return fmt.Errorf("store.TaskStateSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		cur, runTo string
		runStart   time.Time
	)
	flush := func() {
		if cur != "" && runTo != "" && runTo == status[cur] {
			s := out[cur]
			s.Status = runStart
			out[cur] = s
		}
	}
	for rows.Next() {
		var item, ts, to, gate string
		if err := rows.Scan(&item, &ts, &to, &gate); err != nil {
			return fmt.Errorf("store.TaskStateSince: scan: %w", err)
		}
		at, err := core.ParseTime(ts)
		if err != nil {
			return fmt.Errorf("store.TaskStateSince: ts: %w", err)
		}
		if item != cur {
			flush()
			cur, runTo, runStart = item, "", time.Time{}
		}
		if to != "" && to != runTo {
			runTo, runStart = to, at
		}
		if gate == "set" {
			s := out[item]
			s.GateSet = at
			out[item] = s
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store.TaskStateSince: %w", err)
	}
	flush()
	return nil
}

// taskBlockersClosed fills each batch task's BlockersClosed.
func taskBlockersClosed(ctx context.Context, db *sql.DB, batch []string, out map[string]TaskSince) error {
	args := make([]any, len(batch))
	for i, id := range batch {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT d.task_id, MAX(b.closed_at)
		  FROM task_deps d JOIN tasks b ON b.id = d.depends_on
		 WHERE d.task_id IN (`+placeholders(len(batch))+`) AND b.closed_at IS NOT NULL AND b.closed_at <> ''
		 GROUP BY d.task_id`, args...)
	if err != nil {
		return fmt.Errorf("store.TaskStateSince: blockers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, closed string
		if err := rows.Scan(&id, &closed); err != nil {
			return fmt.Errorf("store.TaskStateSince: blockers: scan: %w", err)
		}
		at, err := core.ParseTime(closed)
		if err != nil {
			return fmt.Errorf("store.TaskStateSince: blockers: closed_at: %w", err)
		}
		s := out[id]
		s.BlockersClosed = at
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store.TaskStateSince: blockers: %w", err)
	}
	return nil
}

// TaskTransitions returns a task's newest limit task.transition events, oldest
// first: its history as the task's writers recorded it (tasks_add, tasks_claim,
// tasks_release, tasks_update, the console's task writes). A payload that is
// not JSON reads as empty.
func TaskTransitions(ctx context.Context, db *sql.DB, id string, limit int) ([]core.Event, error) {
	if id == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, ts, session_id, project_slug, item_id, payload
		  FROM events
		 WHERE item_id = ? AND +kind = ?
		 ORDER BY ts DESC, id DESC
		 LIMIT ?`, id, string(core.EventTaskTransition), limit)
	if err != nil {
		return nil, fmt.Errorf("store.TaskTransitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Event
	for rows.Next() {
		var (
			e           core.Event
			ts, payload string
		)
		if err := rows.Scan(&e.ID, &ts, &e.SessionID, &e.ProjectSlug, &e.ItemID, &payload); err != nil {
			return nil, fmt.Errorf("store.TaskTransitions: scan: %w", err)
		}
		if e.TS, err = core.ParseTime(ts); err != nil {
			return nil, fmt.Errorf("store.TaskTransitions: ts: %w", err)
		}
		e.Kind = core.EventTaskTransition
		if json.Unmarshal([]byte(payload), &e.Payload) != nil || e.Payload == nil {
			e.Payload = map[string]any{}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.TaskTransitions: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}
