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
// AddCommits records commit ids on the task (commit records: full lowercase
// ids, each a no-op when already recorded), attributed to the actor. They land
// in the same transaction as the rest of the patch and behind the same holder
// lock, so whoever may update the task may record its commits, and a task
// another session holds refuses them like any other change.
type TaskPatch struct {
	Status       *core.TaskStatus
	Title        *string
	Body         *string
	ProjectSlug  *string
	AddDependsOn []string
	AddCommits   []string
}

// CreateTask inserts a task and its dependency edges in one transaction. Every
// depends_on must reference an existing task (dangling references are rejected)
// and must not create a cycle. The caller mints the ULID id and timestamps.
//
// The task's account is the restricted account the request in ctx speaks for
// (access.AccountOf) unless the caller set one; every later change stamps
// updated_account the same way, so a plan step a restricted account created,
// claimed or closed says so whichever tool did it.
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
// are validated for existence and cycles like CreateTask.
//
// actor is the caller's session id (empty for callers with no session). A task
// with a live claim (see core.Task.ClaimLive) is locked to its holder: any
// mutation by a different actor is rejected with ErrTaskClaimConflict, so an
// agent's claimed task cannot be edited or closed out from under it. The holder
// itself, an expired lease, and the owner override (ForceReleaseTask) are the
// only ways past the lock.
func UpdateTask(ctx context.Context, db *sql.DB, id string, patch TaskPatch, actor string, now time.Time) (core.Task, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return core.Task{}, fmt.Errorf("store.UpdateTask: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	t, ok, err := taskByIDTx(ctx, tx, id)
	if err != nil {
		return core.Task{}, err
	}
	if !ok {
		return core.Task{}, fmt.Errorf("store.UpdateTask: %w: %q", ErrTaskNotFound, id)
	}
	if t.ClaimLive(now) && t.ClaimedBy != actor {
		return core.Task{}, fmt.Errorf("store.UpdateTask: %w: task %q held by %q",
			ErrTaskClaimConflict, id, t.ClaimedBy)
	}

	if patch.Title != nil {
		t.Title = *patch.Title
	}
	if patch.Body != nil {
		t.Body = *patch.Body
	}
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
		t.ClaimedBy, leaseAt, access.AccountOf(ctx), t.ID); err != nil {
		return core.Task{}, fmt.Errorf("store.UpdateTask: update: %w", err)
	}
	if err := addDeps(ctx, tx, t.ID, patch.AddDependsOn); err != nil {
		return core.Task{}, err
	}
	if _, err := recordCommitsTx(ctx, tx, CommitOwnerTask, t.ID, actor, patch.AddCommits, now); err != nil {
		return core.Task{}, fmt.Errorf("store.UpdateTask: %w", err)
	}
	// Title, body, and project are all indexed, and a patch may change any of
	// them; refreshing unconditionally keeps the mirror correct without the
	// writer having to know which fields FTS reads.
	if err := IndexTaskFTS(ctx, tx, t); err != nil {
		return core.Task{}, fmt.Errorf("store.UpdateTask: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return core.Task{}, fmt.Errorf("store.UpdateTask: commit: %w", err)
	}
	return TaskByID(ctx, db, id)
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
