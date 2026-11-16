// Lease-based task claiming: the compare-and-set claim and the ways a claim ends
// (holder release, owner force-release, session teardown, the reaper's release
// of a lapsed claim, lease expiry). Expiry is enforced lazily inside ClaimTask
// -- there is no background sweeper. A claim taken from its holder without its
// say -- a reclaim of its lapsed lease, the owner's force release, the reaper's
// release -- records a notice for the holder in the same transaction
// (claim_notices.go). None of them writes updated_account: a claim or release
// changes no text, so it neither labels an owner's task with the claimant's
// account nor clears the label of an account whose words the task still
// carries (taskTextAccount).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrTaskClaimConflict is returned when a claim loses the compare-and-set race
// to another live lease (or, from Release/Update paths, when the caller is not
// the holder). A refused claim whose cause is not a holder reports ErrTaskBlocked
// or ErrTaskClosed instead -- those need a different fix (finish the blocker;
// nothing, respectively), and reporting them as "already claimed" used to send
// agents chasing a holder that did not exist ("held by \"\"").
var ErrTaskClaimConflict = errors.New("task already claimed")

// ErrTaskBlocked is returned when a claim targets an open task with an
// unfinished dependency; the message names the blockers. No one holds the task
// -- it becomes claimable the moment its blockers close.
var ErrTaskBlocked = errors.New("task blocked")

// TaskBlockedError is ClaimTask's ErrTaskBlocked refusal, carrying the blockers
// it names so a surface can judge each against its caller's read fence before
// the message reaches anyone: a blocker in a project the caller may not read
// is that project's content, its id included. A surface that withholds some
// keeps the rest in Blockers and describes the ones it left out in Withheld,
// which the message renders after them; the store itself sets only TaskID and
// Blockers.
type TaskBlockedError struct {
	TaskID   string
	Blockers []core.Task
	// Withheld is rendered verbatim after the named blockers ("" renders
	// nothing): the surface's words for the blockers it may not name.
	Withheld string
	// Remedy, when set, is rendered on a second line, "Remedy: <Remedy>",
	// the way a lock refusal ends: how the caller reaches a withheld blocker
	// it was told the locked project of. "" renders nothing, so a refusal
	// without one reads exactly as before.
	Remedy string
}

func (e *TaskBlockedError) Error() string {
	parts := make([]string, 0, len(e.Blockers)+1)
	for _, b := range e.Blockers {
		parts = append(parts, fmt.Sprintf("%s (%s)", b.ID, b.Status))
	}
	if e.Withheld != "" {
		parts = append(parts, e.Withheld)
	}
	msg := fmt.Sprintf("store.ClaimTask: %s: task %q waits on %s", ErrTaskBlocked, e.TaskID, strings.Join(parts, ", "))
	if e.Remedy != "" {
		msg += "\nRemedy: " + e.Remedy
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrTaskBlocked) hold.
func (e *TaskBlockedError) Unwrap() error { return ErrTaskBlocked }

// ErrTaskClosed is returned when a claim targets a done/dropped task. No one
// holds it either; there is simply nothing left to claim.
var ErrTaskClosed = errors.New("task closed")

// ClaimResult reports the outcome of a successful ClaimTask. Reclaimed is true
// when a different session's lapsed lease was stolen; PriorHolder then names that
// session so the caller can record a reclaim event. The prior holder's notice
// (ClaimLostReclaimed) is already recorded.
type ClaimResult struct {
	Task        core.Task
	Reclaimed   bool
	PriorHolder string
}

// ClaimTask atomically claims a task for sessionID, moving it to in_progress and
// stamping a lease that expires at now+lease. It is a compare-and-set (mirrors
// ResolveProposal): the write lands only when the task is claimable --
//
//	(a) open and ready (no open/in_progress blocker), or
//	(b) in_progress and held by sessionID (a re-claim / heartbeat that refreshes
//	    the lease), or
//	(c) in_progress but carrying no live claim -- nobody holds it, so it is up for
//	    grabs (a steal from a dead holder, or a row that was never claimed).
//
// Otherwise it returns an error naming the actual refusal: ErrTaskClaimConflict
// when another live lease holds the task, ErrTaskBlocked when the task is open
// with an unfinished dependency (a *TaskBlockedError naming the blockers),
// ErrTaskClosed when it is done/dropped, ErrTaskGated (a *TaskGatedError) when
// it waits on people: a gate is never claimed, in any branch, and an open step
// whose own gate is open is not ready (branch (a) is sqlReady, the queue's own
// rule). Lease expiry is enforced lazily here -- there is no background
// sweeper. sessionID must be non-empty.
//
// core.Task.ClaimLive is the single authority on whether a task is held, and
// branches (b)+(c) are exactly its negation for an in_progress row: an empty
// claimed_by (no holder), a NULL lease_expires_at (no lease stamped), or a
// lapsed lease (lease_expires_at <= now -- ClaimLive uses After, so a lease
// expiring exactly at now is already dead). Branch (c) must tolerate a claimless
// in_progress row because UpdateTask can produce one: `seam task start <id>` and
// MCP tasks_update status=in_progress patch the status without stamping a claim,
// which is a legitimate owner action. Such a row is already freely editable by
// anyone (UpdateTask's holder-lock also defers to ClaimLive), so refusing to
// claim it would strand it as permanently unclaimable until someone reopened it.
//
// Branch (a) deliberately ignores claimed_by: a live claim exists only on an
// in_progress task with an unexpired lease, and every path that writes
// status='open' clears the claim fields, so a claim value on an open row can
// only be stale residue (rows written before reopen released claims). Claiming
// such a row overwrites the residue, self-healing it.
//
// The read of the prior holder, the compare-and-set and a reclaim's notice to
// that holder run in one transaction (the pool has one connection), so the
// notice says exactly what the claim did: a holder that released, re-claimed or
// closed its task between a separate read and the write is never told it lost
// a claim it gave up or still holds.
func ClaimTask(ctx context.Context, db *sql.DB, id, sessionID string, lease time.Duration, now time.Time) (ClaimResult, error) {
	if sessionID == "" {
		return ClaimResult{}, errors.New("store.ClaimTask: empty session id")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("store.ClaimTask: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	prior, ok, err := taskByIDTx(ctx, tx, id)
	if err != nil {
		return ClaimResult{}, err
	}
	if !ok {
		return ClaimResult{}, fmt.Errorf("store.ClaimTask: %w: %q", ErrTaskNotFound, id)
	}

	nowStr := core.FormatTime(now.UTC())
	expStr := core.FormatTime(now.Add(lease).UTC())
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks
		   SET status = 'in_progress', claimed_by = ?, lease_expires_at = ?, updated_at = ?
		 WHERE id = ? AND kind <> 'gate'
		   AND (
		         (`+sqlReady("tasks")+`)
		      OR (status = 'in_progress' AND (
		             claimed_by = ?
		          OR claimed_by = ''
		          OR lease_expires_at IS NULL
		          OR lease_expires_at <= ?))
		   )`,
		sessionID, expStr, nowStr, id, sessionID, nowStr)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("store.ClaimTask: %w", err)
	}
	// A zero here is the CAS losing, which is a conflict -- so a driver failure
	// must not reach that branch and be reported as another agent holding the
	// task, an answer the caller would believe and act on.
	n, err := res.RowsAffected()
	if err != nil {
		return ClaimResult{}, fmt.Errorf("store.ClaimTask: rows affected: %w", err)
	}
	if n == 0 {
		// Give the one connection back before the diagnosis reads on the pool.
		_ = tx.Rollback()
		cur, err := claimRefusal(ctx, db, id, now)
		return ClaimResult{Task: cur}, err
	}
	// Only a steal from a real prior holder counts as a reclaim. Overwriting a
	// stale claim left on an open row (branch (a)), or picking up a claimless
	// in_progress row left by `seam task start` (branch (c)), takes the task from
	// nobody -- an ordinary claim, not a lease steal -- and the empty prior
	// ClaimedBy excludes both, leaving PriorHolder blank.
	reclaimed := prior.Status == core.TaskInProgress &&
		prior.ClaimedBy != "" && prior.ClaimedBy != sessionID
	holder := ""
	if reclaimed {
		holder = prior.ClaimedBy
		if err := RecordClaimNotice(ctx, tx, ClaimNotice{
			SessionID: prior.ClaimedBy, Kind: ClaimResourceTask, ResourceID: id, Project: prior.ProjectSlug,
			NewHolder: sessionID, Reason: ClaimLostReclaimed, LeaseExpiresAt: prior.LeaseExpiresAt, LostAt: now,
		}); err != nil {
			return ClaimResult{}, fmt.Errorf("store.ClaimTask: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ClaimResult{}, fmt.Errorf("store.ClaimTask: commit: %w", err)
	}
	updated, err := TaskByID(ctx, db, id)
	if err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{Task: updated, Reclaimed: reclaimed, PriorHolder: holder}, nil
}

// claimRefusal diagnoses a claim whose compare-and-set missed. The WHERE can
// miss for four unrelated reasons -- held by a live lease, open but blocked by
// an unfinished dependency, waiting on people (a gate, or an open gate of its
// own), or already closed -- and the caller's next move is different for each
// (wait for or steal the lease; finish the blockers; have the person clear the
// gate; walk away). It re-reads the row rather than trusting the pre-CAS
// snapshot: on the contended path the snapshot is exactly what a racing winner
// just invalidated.
// The diagnosis is advisory, not part of the CAS -- the claim itself already
// failed atomically, and core.Task.ClaimLive stays the single held/not-held
// authority here too.
func claimRefusal(ctx context.Context, db *sql.DB, id string, now time.Time) (core.Task, error) {
	cur, ok, err := taskByIDTx(ctx, db, id)
	if err != nil {
		return core.Task{}, err
	}
	if !ok {
		return core.Task{}, fmt.Errorf("store.ClaimTask: %w: %q", ErrTaskNotFound, id)
	}
	switch {
	case cur.ClaimLive(now):
		return cur, fmt.Errorf("store.ClaimTask: %w: task %q held by %q",
			ErrTaskClaimConflict, id, cur.ClaimedBy)
	case cur.Status.Closed():
		return cur, fmt.Errorf("store.ClaimTask: %w: task %q is %s",
			ErrTaskClosed, id, cur.Status)
	case cur.IsGate():
		return cur, &TaskGatedError{TaskID: id, IsGate: true, Gate: cur.Gate}
	case cur.Status == core.TaskOpen && cur.Gate.Open():
		return cur, &TaskGatedError{TaskID: id, Gate: cur.Gate}
	case cur.Status == core.TaskOpen:
		blockers, err := openBlockersForTasks(ctx, db, []string{id})
		if err != nil {
			return core.Task{}, err
		}
		if bs := blockers[id]; len(bs) > 0 {
			return cur, &TaskBlockedError{TaskID: id, Blockers: bs}
		}
	}
	// The re-read no longer explains the miss: the row changed between the CAS
	// and this look (a racer claimed and released, a blocker closed, ...). Report
	// the transient loss rather than inventing a cause; a retry will resolve it.
	return cur, fmt.Errorf("store.ClaimTask: %w: task %q changed during the claim; retry",
		ErrTaskClaimConflict, id)
}

// ReleaseTask releases a task held by sessionID, reopening it (status back to
// open, claim and lease cleared) so another agent can pick it up. Only the
// current holder may release; otherwise it returns ErrTaskClaimConflict (or
// ErrTaskNotFound when the id is unknown).
func ReleaseTask(ctx context.Context, db *sql.DB, id, sessionID string, now time.Time) (core.Task, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE tasks SET status = 'open', claimed_by = '', lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND claimed_by = ? AND status = 'in_progress'`,
		core.FormatTime(now.UTC()), id, sessionID)
	if err != nil {
		return core.Task{}, fmt.Errorf("store.ReleaseTask: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.Task{}, fmt.Errorf("store.ReleaseTask: rows affected: %w", err)
	}
	if n == 0 {
		if _, ok, err := taskByIDTx(ctx, db, id); err != nil {
			return core.Task{}, err
		} else if !ok {
			return core.Task{}, fmt.Errorf("store.ReleaseTask: %w: %q", ErrTaskNotFound, id)
		}
		return core.Task{}, fmt.Errorf("store.ReleaseTask: %w: task %q not held by %q",
			ErrTaskClaimConflict, id, sessionID)
	}
	return TaskByID(ctx, db, id)
}

// ForceReleaseTask unconditionally releases a claimed (in_progress) task,
// reopening it (status back to open, claim and lease cleared) regardless of who
// holds it or whether the lease is still live. It backs the owner override (the
// console "release lock" button and `seam task release --force`); it is not
// reachable from the agent MCP surface. Unlike ReleaseTask it does not check the
// holder. Returns ErrTaskClaimConflict when the task is not in_progress (nothing
// to release) and ErrTaskNotFound when the id is unknown.
//
// A task that had a holder records a notice for it (ClaimLostReleased) in the
// same transaction, whether its lease was live or had lapsed: either way its
// name was on the claim, and its next re-claim would otherwise take the task
// back without a word. A claimless in_progress row (`seam task start`) had no
// holder to tell.
func ForceReleaseTask(ctx context.Context, db *sql.DB, id string, now time.Time) (core.Task, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return core.Task{}, fmt.Errorf("store.ForceReleaseTask: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	prior, ok, err := taskByIDTx(ctx, tx, id)
	if err != nil {
		return core.Task{}, err
	}
	if !ok {
		return core.Task{}, fmt.Errorf("store.ForceReleaseTask: %w: %q", ErrTaskNotFound, id)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = 'open', claimed_by = '', lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND status = 'in_progress'`,
		core.FormatTime(now.UTC()), id)
	if err != nil {
		return core.Task{}, fmt.Errorf("store.ForceReleaseTask: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.Task{}, fmt.Errorf("store.ForceReleaseTask: rows affected: %w", err)
	}
	if n == 0 {
		return core.Task{}, fmt.Errorf("store.ForceReleaseTask: %w: task %q is not claimed",
			ErrTaskClaimConflict, id)
	}
	if prior.ClaimedBy != "" {
		if err := RecordClaimNotice(ctx, tx, ClaimNotice{
			SessionID: prior.ClaimedBy, Kind: ClaimResourceTask, ResourceID: id, Project: prior.ProjectSlug,
			Reason: ClaimLostReleased, LeaseExpiresAt: prior.LeaseExpiresAt, LostAt: now,
		}); err != nil {
			return core.Task{}, fmt.Errorf("store.ForceReleaseTask: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return core.Task{}, fmt.Errorf("store.ForceReleaseTask: commit: %w", err)
	}
	return TaskByID(ctx, db, id)
}

// ReleaseReapedClaims is the reaper's release: it reopens every task sessionID
// still holds a lapsed claim on -- the session the reaper just expired, which
// it expires only once no claim of its is live -- and records a notice for each
// (ClaimLostReaped), in one transaction. Without the notice the holder would
// never hear of it: a later claimant takes an open task, which is no reclaim. A
// claim live at now is left alone: one the holder took after the reap, through
// a call that revived its session, is its own again. It returns how many claims
// it released.
func ReleaseReapedClaims(ctx context.Context, db *sql.DB, sessionID string, now time.Time) (int, error) {
	const op = "store.ReleaseReapedClaims"
	if sessionID == "" {
		return 0, nil
	}
	nowStr := core.FormatTime(now.UTC())
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	held, err := lapsedClaimsTx(ctx, tx, sessionID, nowStr)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	released := 0
	for _, t := range held {
		res, err := tx.ExecContext(ctx, `
			UPDATE tasks SET status = 'open', claimed_by = '', lease_expires_at = NULL, updated_at = ?
			 WHERE id = ? AND `+lapsedClaimOf, nowStr, t.ID, sessionID, nowStr)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("%s: rows affected: %w", op, err)
		}
		if n == 0 {
			continue
		}
		if err := RecordClaimNotice(ctx, tx, ClaimNotice{
			SessionID: sessionID, Kind: ClaimResourceTask, ResourceID: t.ID, Project: t.ProjectSlug,
			Reason: ClaimLostReaped, LeaseExpiresAt: t.LeaseExpiresAt, LostAt: now,
		}); err != nil {
			return 0, fmt.Errorf("%s: %w", op, err)
		}
		released++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: commit: %w", op, err)
	}
	return released, nil
}

// lapsedClaimOf matches a task the bound session holds a claim on that is not
// live at the bound instant: core.Task.ClaimLive's negation for a row whose
// holder is that session. Two binds: the session id, then now.
const lapsedClaimOf = `claimed_by = ? AND status = 'in_progress' AND (lease_expires_at IS NULL OR lease_expires_at <= ?)`

// lapsedClaimsTx reads the tasks sessionID holds a lapsed claim on, inside tx;
// the rows are closed before it returns, so the caller's writes never wait on
// this read.
func lapsedClaimsTx(ctx context.Context, tx *sql.Tx, sessionID, now string) ([]core.Task, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE `+lapsedClaimOf, sessionID, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var held []core.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		held = append(held, t)
	}
	return held, rows.Err()
}

// ReleaseClaimsForSession reopens every in_progress task currently claimed by
// sessionID (called on session_end so a departing agent's work returns to the
// queue). It returns the number of tasks released. The holder is ending its own
// session, so no notice is recorded.
func ReleaseClaimsForSession(ctx context.Context, db *sql.DB, sessionID string, now time.Time) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE tasks SET status = 'open', claimed_by = '', lease_expires_at = NULL, updated_at = ?
		 WHERE claimed_by = ? AND status = 'in_progress'`,
		core.FormatTime(now.UTC()), sessionID)
	if err != nil {
		return 0, fmt.Errorf("store.ReleaseClaimsForSession: %w", err)
	}
	// The count is the whole answer here, and 0 is a perfectly ordinary one, so a
	// driver failure would report "released nothing" for an UPDATE that may well
	// have freed several of a departing agent's tasks.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.ReleaseClaimsForSession: rows affected: %w", err)
	}
	return int(n), nil
}
