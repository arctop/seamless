// Task CRUD and the declarations the rest of the tasks_*.go files share.
// The dependency-aware queue splits across: tasks_claim.go (leases), tasks_deps.go
// (the edge DAG), tasks_query.go (the ready/blocked/list reads), tasks_plans.go
// (plan rollups) and tasks_scan.go (row scanning).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// taskCols is the SELECT list for the tasks table, matching scanTask.
const taskCols = `id, project_slug, title, body, status, created_by,
	plan_slug, claimed_by, lease_expires_at, favorite, created_at, updated_at, closed_at,
	account, updated_account`

// ErrTaskNotFound is returned when a task id does not exist (e.g. a dangling
// depends_on reference).
var ErrTaskNotFound = errors.New("task not found")

// rowQuerier is the read subset shared by *sql.DB and *sql.Tx, so the cycle and
// dangling-reference checks run identically inside or outside a transaction.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// TaskPatch is the set of mutable fields UpdateTask may change; nil fields are
// left untouched. AddDependsOn edges are added (existing edges are kept).
// ProjectSlug reassigns the task to another project (used when a split moves a
// project's open work to a child); the holder-lock still applies.
//
// RemoveDependsOn edges are removed: each id that is one of the task's
// blockers stops being one, and an id that is not is left alone and reported
// (TaskUpdateResult.AbsentDependsOn) -- a no-op, never an error. A blocker in a
// project the request's View does not reach (access.Visible, peer_auth) counts
// as not a blocker: it is neither removed nor told apart from an id that
// names nothing, the rule the MCP surface applies to an added one. An id may
// not be both added and removed in one patch (ErrTaskDepConflict).
//
// AddCommits records commit ids on the task (commit records: full lowercase
// ids, each a no-op when already recorded), attributed to the actor. They land
// in the same transaction as the rest of the patch and behind the same holder
// lock, so whoever may update the task may record its commits, and a task
// another session holds refuses them like any other change.
//
// IfClaimedBy changes nothing: it is a precondition. Set, the patch applies
// only while the task's claim, live or lapsed, is held by that session, read
// inside the update's transaction; otherwise the update is refused with
// ErrTaskClaimConflict and writes nothing. A receipt's write (SendReceipt)
// sets it to the pre-move session whose claim JudgeReceipt judged.
type TaskPatch struct {
	Status          *core.TaskStatus
	Title           *string
	Body            *string
	ProjectSlug     *string
	AddDependsOn    []string
	RemoveDependsOn []string
	AddCommits      []string
	IfClaimedBy     *string
}

// onlyRemovals reports whether the patch asks for nothing but blocker
// removals, so a patch whose every removal names a non-blocker changes nothing.
func (p TaskPatch) onlyRemovals() bool {
	return len(p.RemoveDependsOn) > 0 && p.Status == nil && p.Title == nil && p.Body == nil &&
		p.ProjectSlug == nil && len(p.AddDependsOn) == 0 && len(p.AddCommits) == 0
}

// ErrTaskDepConflict is returned when one patch both adds and removes the same
// dependency: the two orders mean opposite things, so neither is guessed.
var ErrTaskDepConflict = errors.New("dependency both added and removed")

// TaskUpdateResult is UpdateTaskDetailed's answer: the task after the patch,
// and what the patch's removals did.
type TaskUpdateResult struct {
	Task core.Task
	// From is the task's status before the patch, read inside its transaction:
	// a history record of the update tells a status move (From !=
	// Task.Status) from an update that moved none.
	From core.TaskStatus
	// RemovedDependsOn are the RemoveDependsOn ids that were blockers of the
	// task and no longer are; AbsentDependsOn the rest (not a blocker, no such
	// task, or a blocker the request may not reach). Both keep the patch's
	// order, deduplicated.
	RemovedDependsOn []string
	AbsentDependsOn  []string
	// Unchanged is true when the patch carried only removals and none named a
	// blocker: nothing was written, updated_at included.
	Unchanged bool
}

// CreateTask inserts a task and its dependency edges in one transaction. Every
// depends_on must reference an existing task (dangling references are rejected)
// and must not create a cycle. The caller mints the ULID id and timestamps.
//
// The task's account is the restricted account the request in ctx speaks for
// (access.AccountOf) unless the caller set one, so a plan step a restricted
// account created says so whichever tool created it. updated_account is the
// text's label after that, and only UpdateTask moves it (taskTextAccount).
func CreateTask(ctx context.Context, db *sql.DB, t core.Task) error {
	if t.Account == "" {
		t.Account = access.AccountOf(ctx)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.CreateTask: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var closedAt any
	if t.ClosedAt != nil {
		closedAt = core.FormatTime(*t.ClosedAt)
	}
	var leaseAt any
	if t.LeaseExpiresAt != nil {
		leaseAt = core.FormatTime(*t.LeaseExpiresAt)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (id, project_slug, title, body, status, created_by,
		                   plan_slug, claimed_by, lease_expires_at,
		                   created_at, updated_at, closed_at, account)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectSlug, t.Title, t.Body, string(t.Status), t.CreatedBy,
		t.PlanSlug, t.ClaimedBy, leaseAt,
		core.FormatTime(t.CreatedAt), core.FormatTime(t.UpdatedAt), closedAt, t.Account); err != nil {
		return fmt.Errorf("store.CreateTask: insert: %w", err)
	}
	if err := addDeps(ctx, tx, t.ID, t.DependsOn); err != nil {
		return err
	}
	if err := IndexTaskFTS(ctx, tx, t); err != nil {
		return fmt.Errorf("store.CreateTask: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.CreateTask: commit: %w", err)
	}
	return nil
}

// UpdateTask applies patch to the task and returns the updated task (with deps).
// Moving to a terminal status stamps closed_at; reopening clears it. Added deps
// are validated for existence and cycles like CreateTask; removed ones need no
// check (dropping an edge cannot close a cycle).
//
// actor is the caller's session id (empty for callers with no session). A task
// with a live claim (see core.Task.ClaimLive) is locked to its holder: any
// mutation by a different actor is rejected with ErrTaskClaimConflict, so an
// agent's claimed task cannot be edited or closed out from under it. The holder
// itself, an expired lease, and the owner override (ForceReleaseTask) are the
// only ways past the lock. The lock is judged before anything else, a removal
// that would be a no-op included.
//
// updated_account moves only when the title or body actually changes
// (taskTextAccount); a status, project, dependency or commit change keeps it.
//
// A task moved to another project takes its provenance stamps along
// (item_provenance, ProvenanceRelocate) once the move commits, whichever
// surface moved it; a stamp naming its new project as the source leaves the
// index. Should that step fail, the error says the move landed and the
// stamps did not follow.
//
// UpdateTaskDetailed is the same update, answering what the removals did too.
func UpdateTask(ctx context.Context, db *sql.DB, id string, patch TaskPatch, actor string, now time.Time) (core.Task, error) {
	res, err := UpdateTaskDetailed(ctx, db, id, patch, actor, now)
	if err != nil {
		return core.Task{}, err
	}
	return res.Task, nil
}

// UpdateTaskDetailed is UpdateTask answering a TaskUpdateResult: which of the
// patch's RemoveDependsOn ids were blockers (now removed) and which were not,
// and the status the task had before. A patch of removals alone that names no
// blocker writes nothing (Unchanged). Its errors carry UpdateTask's prefix:
// they reach agents verbatim through tasks_update.
func UpdateTaskDetailed(ctx context.Context, db *sql.DB, id string, patch TaskPatch, actor string, now time.Time) (TaskUpdateResult, error) {
	remove := dedupeStrings(patch.RemoveDependsOn)
	for _, dep := range patch.AddDependsOn {
		if slices.Contains(remove, dep) {
			return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w: %q", ErrTaskDepConflict, dep)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	t, ok, err := taskByIDTx(ctx, tx, id)
	if err != nil {
		return TaskUpdateResult{}, err
	}
	if !ok {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w: %q", ErrTaskNotFound, id)
	}
	if t.ClaimLive(now) && t.ClaimedBy != actor {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w: task %q held by %q",
			ErrTaskClaimConflict, id, t.ClaimedBy)
	}
	if patch.IfClaimedBy != nil && t.ClaimedBy != *patch.IfClaimedBy {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w: task %q is no longer held by %q",
			ErrTaskClaimConflict, id, *patch.IfClaimedBy)
	}

	res := TaskUpdateResult{From: t.Status}
	fromProject := t.ProjectSlug
	res.RemovedDependsOn, res.AbsentDependsOn, err = removeDeps(ctx, tx, t.ID, remove)
	if err != nil {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w", err)
	}
	if patch.onlyRemovals() && len(res.RemovedDependsOn) == 0 {
		// Nothing to change: answer the row as it stands, read inside this
		// transaction (the pool has one connection), and roll back.
		deps, err := dependsOnOf(ctx, tx, t.ID)
		if err != nil {
			return TaskUpdateResult{}, err
		}
		t.DependsOn = deps
		res.Task, res.Unchanged = t, true
		return res, nil
	}

	prevTitle, prevBody := t.Title, t.Body
	if patch.Title != nil {
		t.Title = *patch.Title
	}
	if patch.Body != nil {
		t.Body = *patch.Body
	}
	t.UpdatedAccount = taskTextAccount(t.UpdatedAccount, access.AccountOf(ctx),
		t.Title != prevTitle, t.Body != prevBody, t.Body)
	if patch.ProjectSlug != nil {
		t.ProjectSlug = *patch.ProjectSlug
	}
	if patch.Status != nil && *patch.Status != t.Status {
		t.Status = *patch.Status
		if t.Status.Closed() {
			at := now.UTC()
			t.ClosedAt = &at
			// A finished task releases its claim so it never lingers as a
			// stale holder or blocks a lease reclaim.
			t.ClaimedBy = ""
			t.LeaseExpiresAt = nil
		} else {
			t.ClosedAt = nil
			if t.Status == core.TaskOpen {
				// Reopening returns the task to the queue, so it releases the
				// claim like ReleaseTask does, keeping the invariant that an
				// open task carries no claim. ClaimTask tolerates stale residue
				// on open rows (self-healing), but every write of status='open'
				// still clears the claim so the residue never propagates.
				t.ClaimedBy = ""
				t.LeaseExpiresAt = nil
			}
		}
	}
	t.UpdatedAt = now.UTC()

	var closedAt any
	if t.ClosedAt != nil {
		closedAt = core.FormatTime(*t.ClosedAt)
	}
	var leaseAt any
	if t.LeaseExpiresAt != nil {
		leaseAt = core.FormatTime(*t.LeaseExpiresAt)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks SET title = ?, body = ?, project_slug = ?, status = ?, updated_at = ?, closed_at = ?,
		                 claimed_by = ?, lease_expires_at = ?, updated_account = ?
		 WHERE id = ?`,
		t.Title, t.Body, t.ProjectSlug, string(t.Status), core.FormatTime(t.UpdatedAt), closedAt,
		t.ClaimedBy, leaseAt, t.UpdatedAccount, t.ID); err != nil {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: update: %w", err)
	}
	if err := addDeps(ctx, tx, t.ID, patch.AddDependsOn); err != nil {
		return TaskUpdateResult{}, err
	}
	if _, err := recordCommitsTx(ctx, tx, CommitOwnerTask, t.ID, actor, patch.AddCommits, now); err != nil {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w", err)
	}
	// Title, body, and project are all indexed, and a patch may change any of
	// them; refreshing unconditionally keeps the mirror correct without the
	// writer having to know which fields FTS reads.
	if err := IndexTaskFTS(ctx, tx, t); err != nil {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: commit: %w", err)
	}
	if t.ProjectSlug != fromProject {
		// The stamps follow the task, so the came-from lists by project stay
		// true. It runs on the pool once the move committed: the pool's one
		// connection is the transaction's until then.
		if err := ProvenanceRelocate(ctx, db, core.ItemKindTask, t.ID, t.ProjectSlug); err != nil {
			return TaskUpdateResult{}, fmt.Errorf("store.UpdateTask: task %q moved to project %q, but its provenance "+
				"stamps did not follow: %w", id, t.ProjectSlug, err)
		}
	}
	res.Task, err = TaskByID(ctx, db, id)
	if err != nil {
		return TaskUpdateResult{}, err
	}
	return res, nil
}

// taskTextAccount is a task's updated_account after a change: the restricted
// account (peer_auth.restricted) whose words the title or body may still
// carry, or "" when none can. It follows the text, the rule a memory's or
// note's account label follows (mcp's accountAfter), and nothing else: a
// status flip, a claim or release, a lease lapsing, a project move, a
// dependency added or removed, a recorded commit or a star keeps the value the
// row has, so the owner closing a task an account rewrote leaves the label on,
// and an account that only claimed the owner's task never puts one there.
//
// writer is the account the change speaks for ("" for the owner). A
// restricted account's change of the title or the body stamps its name. The
// owner's change clears the label only once it leaves no text it did not
// write: the title rewritten, and the body rewritten too unless it is empty.
// The column names one account for the whole task, not which field it wrote,
// so an owner's edit of one field cannot know the other is its own; keeping a
// label a little too long names an account whose words are gone, while
// clearing it early would pass an account's title off as the owner's on every
// task line. (A memory's account can follow its body alone because its
// description only summarizes the body; a task's title is its text.)
// Unchanged values count as no change, so resending the same text moves
// nothing.
func taskTextAccount(current, writer string, titleChanged, bodyChanged bool, body string) string {
	switch {
	case writer != "" && (titleChanged || bodyChanged):
		return writer
	case writer == "" && titleChanged && (bodyChanged || body == ""):
		return ""
	}
	return current
}

// TaskByID returns a task with its dependency ids populated. found is false when
// absent.
func TaskByID(ctx context.Context, db *sql.DB, id string) (core.Task, error) {
	t, ok, err := taskByIDTx(ctx, db, id)
	if err != nil {
		return core.Task{}, err
	}
	if !ok {
		return core.Task{}, fmt.Errorf("store.TaskByID: %w: %q", ErrTaskNotFound, id)
	}
	deps, err := dependsOnOf(ctx, db, id)
	if err != nil {
		return core.Task{}, err
	}
	t.DependsOn = deps
	return t, nil
}

// taskByIDTx loads a task row (without deps) via any querier.
func taskByIDTx(ctx context.Context, q rowQuerier, id string) (core.Task, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return core.Task{}, false, fmt.Errorf("store: taskByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return core.Task{}, false, rows.Err()
	}
	t, err := scanTask(rows)
	if err != nil {
		return core.Task{}, false, fmt.Errorf("store: scan task: %w", err)
	}
	return t, true, nil
}
