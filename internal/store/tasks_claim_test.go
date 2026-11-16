package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// addPlanTask inserts an open task composed into plan, returning its id.
func addPlanTask(t *testing.T, db *sql.DB, project, plan, title string, seq int, deps ...string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
	require.NoError(t, CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, Title: title, Status: core.TaskOpen,
		PlanSlug: plan, DependsOn: deps, CreatedAt: base, UpdatedAt: base,
	}))
	return id
}

// TestClaimTask_ConcurrentSingleWinner is the headline acceptance: many sessions
// race to claim the same ready task and exactly one wins; the rest see a conflict.
func TestClaimTask_ConcurrentSingleWinner(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		winner  string
		conflct int
	)
	start := make(chan struct{})
	for i := range racers {
		sess := "sess-" + strconv.Itoa(i)
		wg.Go(func() {
			<-start
			res, err := ClaimTask(context.Background(), db, id, sess, time.Minute, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				winner = res.Task.ClaimedBy
			case errors.Is(err, ErrTaskClaimConflict):
				conflct++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	require.Equal(t, 1, wins, "exactly one claim must win the race")
	require.Equal(t, racers-1, conflct, "every loser must see a claim conflict")

	got, err := TaskByID(context.Background(), db, id)
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, got.Status)
	require.Equal(t, winner, got.ClaimedBy)
}

func TestClaimTask_ConflictWhenHeld(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict)

	got, err := TaskByID(context.Background(), db, id)
	require.NoError(t, err)
	require.Equal(t, "sessA", got.ClaimedBy, "the original holder keeps the claim")
}

func TestClaimTask_HolderHeartbeatRefreshesLease(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	first, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)
	require.NotNil(t, first.Task.LeaseExpiresAt)

	later := now.Add(30 * time.Second)
	second, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, later)
	require.NoError(t, err)
	require.False(t, second.Reclaimed, "re-claim by the holder is a heartbeat, not a reclaim")
	require.True(t, second.Task.LeaseExpiresAt.After(*first.Task.LeaseExpiresAt), "lease must extend")
}

func TestClaimTask_ExpiredLeaseReclaimable(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	// sessB cannot steal while the lease is live.
	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(30*time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict)

	// After the lease lapses, sessB reclaims and the prior holder is recorded.
	res, err := ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, res.Reclaimed)
	require.Equal(t, "sessA", res.PriorHolder)
	require.Equal(t, "sessB", res.Task.ClaimedBy)
}

// TestClaimTask_ExactLeaseBoundaryIsNotLive pins the expiry boundary to
// core.Task.ClaimLive, which uses LeaseExpiresAt.After(now): a lease expiring at
// exactly now is already dead, so the task is claimable at that instant (and
// UpdateTask's holder-lock lets a non-holder in at the same instant, so ClaimTask
// must not disagree).
func TestClaimTask_ExactLeaseBoundaryIsNotLive(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	first, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)
	expiry := *first.Task.LeaseExpiresAt

	// One nanosecond before expiry the claim is still live and unstealable.
	require.True(t, first.Task.ClaimLive(expiry.Add(-time.Nanosecond)))
	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, expiry.Add(-time.Nanosecond))
	require.ErrorIs(t, err, ErrTaskClaimConflict, "a lease is live right up to its expiry instant")

	// At exactly the expiry instant the claim is dead, so sessB takes over.
	require.False(t, first.Task.ClaimLive(expiry), "ClaimLive uses After: an equal lease is not live")
	res, err := ClaimTask(context.Background(), db, id, "sessB", time.Minute, expiry)
	require.NoError(t, err, "a lease expiring exactly at now must be claimable, matching ClaimLive")
	require.True(t, res.Reclaimed)
	require.Equal(t, "sessA", res.PriorHolder)
	require.Equal(t, "sessB", res.Task.ClaimedBy)
}

// TestClaimTask_ClaimlessInProgressIsClaimable is the regression for a task
// pushed straight to in_progress with no claim (`seam task start <id>`, MCP
// tasks_update status=in_progress): UpdateTask patches the status without
// stamping claimed_by/lease_expires_at. Before the fix no branch of ClaimTask
// matched such a row -- not open, not held by the claimer, no expired lease -- so
// it was permanently unclaimable by anyone until someone reopened it, even though
// core.Task.ClaimLive (and therefore UpdateTask's holder-lock) already treated it
// as unheld.
func TestClaimTask_ClaimlessInProgressIsClaimable(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	// The owner starts the task with no session identity, exactly as `seam task
	// start` does: status moves to in_progress, claim fields stay empty.
	started, err := UpdateTask(context.Background(), db, id,
		TaskPatch{Status: statusPtr(core.TaskInProgress)}, "", now)
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, started.Status)
	require.Empty(t, started.ClaimedBy, "the owner start path stamps no claim")
	require.Nil(t, started.LeaseExpiresAt)
	require.False(t, started.ClaimLive(now), "nobody holds a claimless in_progress task")

	res, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now.Add(time.Second))
	require.NoError(t, err, "a claimless in_progress task must be claimable")
	require.Equal(t, "sessA", res.Task.ClaimedBy)
	require.Equal(t, core.TaskInProgress, res.Task.Status)
	require.NotNil(t, res.Task.LeaseExpiresAt)
	require.True(t, res.Task.ClaimLive(now.Add(time.Second)), "the claimer ends up the live holder")
	require.False(t, res.Reclaimed, "taking a task nobody held is not a lease steal")
	require.Empty(t, res.PriorHolder)

	// The fresh claim is an ordinary live claim: nobody else can take it.
	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(2*time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict)
}

// TestClaimTask_LiveClaimStaysUnstealable is the invariant the claimless-row fix
// must not weaken: relaxing branch (c) to "no live claim" must still refuse a row
// another session genuinely holds, whatever the claimer does.
func TestClaimTask_LiveClaimStaysUnstealable(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	held, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)
	require.True(t, held.Task.ClaimLive(now))

	// A non-holder cannot claim it, and a repeated attempt does not wear it down.
	for _, at := range []time.Time{now, now.Add(time.Second), now.Add(59 * time.Second)} {
		_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, at)
		require.ErrorIs(t, err, ErrTaskClaimConflict, "a live claim is never stealable")
	}

	// The holder's claim is untouched: same session, same lease.
	got, err := TaskByID(context.Background(), db, id)
	require.NoError(t, err)
	require.Equal(t, "sessA", got.ClaimedBy)
	require.Equal(t, *held.Task.LeaseExpiresAt, *got.LeaseExpiresAt, "a failed claim must not extend the lease")
}

// TestClaimTask_BlockedTaskNotClaimable also pins the refusal's identity: a
// blocked task is nobody's claim conflict. ClaimTask used to funnel every CAS
// miss into ErrTaskClaimConflict quoting prior.ClaimedBy -- for a blocked task
// that read `held by ""`, sending agents to force-release a task that had no
// holder when the actual fix was finishing the blocker.
func TestClaimTask_BlockedTaskNotClaimable(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2, a) // B blocked by open A
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, b, "sessA", time.Minute, now)
	require.ErrorIs(t, err, ErrTaskBlocked, "a blocked task is blocked, not claimed")
	require.NotErrorIs(t, err, ErrTaskClaimConflict, "no holder exists, so no claim conflict")
	require.ErrorContains(t, err, a, "the refusal must name the blocker")
	require.ErrorContains(t, err, "open", "and the blocker's status")

	// The live repro (C3/B7): the blocker in_progress instead of open. Same
	// refusal, same blocker named -- and still not "already claimed".
	setStatus(t, db, a, core.TaskInProgress)
	_, err = ClaimTask(context.Background(), db, b, "sessA", time.Minute, now)
	require.ErrorIs(t, err, ErrTaskBlocked, "an in_progress blocker still blocks")
	require.NotErrorIs(t, err, ErrTaskClaimConflict)
	require.ErrorContains(t, err, a)

	// Close the blocker; now B is claimable.
	setStatus(t, db, a, core.TaskDone)
	_, err = ClaimTask(context.Background(), db, b, "sessA", time.Minute, now.Add(time.Minute))
	require.NoError(t, err)
}

// TestClaimTask_ClosedTaskNotClaimable: claiming a done/dropped task is the
// third refusal ClaimTask used to misreport as "already claimed" -- nothing
// holds a closed task, there is just nothing left to claim.
func TestClaimTask_ClosedTaskNotClaimable(t *testing.T) {
	db := newTaskDB(t)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	for _, status := range []core.TaskStatus{core.TaskDone, core.TaskDropped} {
		id := addTask(t, db, "demo", "task "+string(status), 1)
		setStatus(t, db, id, status)

		_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
		require.ErrorIs(t, err, ErrTaskClosed, "a %s task is closed, not claimed", status)
		require.NotErrorIs(t, err, ErrTaskClaimConflict)
		require.ErrorContains(t, err, string(status))
	}
}

// TestClaimTask_StaleClaimOnOpenTaskSelfHeals is the regression for legacy rows
// stuck from before UpdateTask cleared claim fields on reopen: an open task with
// a leftover claimed_by (and even a still-future lease) must be claimable, since
// a live claim only exists on an in_progress task. Claiming it overwrites the
// residue and is an ordinary claim, not a reclaim.
func TestClaimTask_StaleClaimOnOpenTaskSelfHeals(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	// Manufacture the legacy stuck state directly: no current code path writes
	// status='open' with a claim. The lease is deliberately in the future to
	// prove that status, not lease expiry, gates the open branch.
	_, err := db.Exec(`UPDATE tasks SET claimed_by = 'deadsess', lease_expires_at = ? WHERE id = ?`,
		core.FormatTime(now.Add(time.Hour)), id)
	require.NoError(t, err)
	require.Equal(t, []string{"A"}, readyTitles(t, db, "demo"), "the stuck row still shows as ready")

	res, err := ClaimTask(context.Background(), db, id, "sessB", time.Minute, now)
	require.NoError(t, err, "an open task must be claimable regardless of a stale claimed_by")
	require.Equal(t, "sessB", res.Task.ClaimedBy)
	require.Equal(t, core.TaskInProgress, res.Task.Status)
	require.False(t, res.Reclaimed, "overwriting stale residue on an open row is not a lease steal")

	// The healed claim is a normal live claim: a third session cannot steal it.
	_, err = ClaimTask(context.Background(), db, id, "sessC", time.Minute, now.Add(time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict, "a genuinely held in_progress task stays unstealable")

	got, err := TaskByID(context.Background(), db, id)
	require.NoError(t, err)
	require.Equal(t, "sessB", got.ClaimedBy)
}

// TestClaimTask_StaleClaimDoesNotBypassReadiness: the relaxed open branch still
// enforces the readiness rule -- a blocked open task is unclaimable even when it
// carries a stale claim.
func TestClaimTask_StaleClaimDoesNotBypassReadiness(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2, a) // B blocked by open A
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := db.Exec(`UPDATE tasks SET claimed_by = 'deadsess' WHERE id = ?`, b)
	require.NoError(t, err)

	_, err = ClaimTask(context.Background(), db, b, "sessA", time.Minute, now)
	require.ErrorIs(t, err, ErrTaskBlocked, "a blocked task is not claimable, stale claim or not")
	require.NotErrorIs(t, err, ErrTaskClaimConflict, "stale residue on a blocked row is still not a holder")
}

// TestMigrationOpenClaimsRepair: migration 007 clears claim fields on legacy
// status='open' rows (stuck from before reopen released claims) while leaving a
// genuinely held in_progress row untouched, and its UPDATE is idempotent.
func TestMigrationOpenClaimsRepair(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	// Build the schema as it was before the repair migration, so the legacy rows
	// pre-exist it the way they will on a live database at the next restart.
	var pre []Migration
	for _, m := range migrationList() {
		if m.Version < 7 {
			pre = append(pre, m)
		}
	}
	require.NoError(t, migrate(db, pre))

	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	insert := func(id, status, claimedBy string, lease any) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO tasks (id, project_slug, title, status,
			claimed_by, lease_expires_at, created_at, updated_at)
			VALUES (?, 'demo', ?, ?, ?, ?, ?, ?)`,
			id, id, status, claimedBy, lease, core.FormatTime(now), core.FormatTime(now))
		require.NoError(t, err)
	}
	insert("stuck-claim", "open", "deadsess", core.FormatTime(now.Add(time.Hour)))
	insert("stuck-lease", "open", "", core.FormatTime(now.Add(time.Hour)))
	insert("held", "in_progress", "sessA", core.FormatTime(now.Add(time.Hour)))

	require.NoError(t, migrate(db, migrationList()))

	assertRepaired := func() {
		t.Helper()
		for _, id := range []string{"stuck-claim", "stuck-lease"} {
			got, err := TaskByID(context.Background(), db, id)
			require.NoError(t, err)
			require.Equal(t, core.TaskOpen, got.Status)
			require.Empty(t, got.ClaimedBy, "%s: repair must clear the stale claim", id)
			require.Nil(t, got.LeaseExpiresAt, "%s: repair must clear the stale lease", id)
		}
		held, err := TaskByID(context.Background(), db, "held")
		require.NoError(t, err)
		require.Equal(t, core.TaskInProgress, held.Status)
		require.Equal(t, "sessA", held.ClaimedBy, "a live in_progress claim survives the repair")
		require.NotNil(t, held.LeaseExpiresAt)
	}
	assertRepaired()

	// The repair UPDATE is idempotent: re-running it is a no-op.
	_, err = db.Exec(migration007)
	require.NoError(t, err)
	assertRepaired()

	// End to end: the previously stuck row is claimable after the repair.
	res, err := ClaimTask(context.Background(), db, "stuck-claim", "sessB", time.Minute, now)
	require.NoError(t, err)
	require.Equal(t, "sessB", res.Task.ClaimedBy)
	require.False(t, res.Reclaimed)
}

func TestReleaseTask_HolderReopensNonHolderRejected(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	_, err = ReleaseTask(context.Background(), db, id, "sessB", now.Add(time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict, "only the holder may release")

	released, err := ReleaseTask(context.Background(), db, id, "sessA", now.Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, released.Status)
	require.Empty(t, released.ClaimedBy)
	require.Nil(t, released.LeaseExpiresAt)

	// Reopened -> claimable again.
	require.Equal(t, []string{"A"}, readyTitles(t, db, "demo"))
}

func TestUpdateTask_CloseClearsClaim(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	done := core.TaskDone
	updated, err := UpdateTask(context.Background(), db, id, TaskPatch{Status: &done}, "sessA", now.Add(time.Second))
	require.NoError(t, err)
	require.Empty(t, updated.ClaimedBy, "closing a task releases its claim")
	require.Nil(t, updated.LeaseExpiresAt)
}

// TestUpdateTask_HolderCheck is the acceptance for the write-lock: a live claim
// locks the task to its holder. A non-holder update is rejected; the holder
// updates freely; and once the lease lapses a non-holder may take over.
func TestUpdateTask_HolderCheck(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	// A different session cannot mutate the live-claimed task.
	done := core.TaskDone
	_, err = UpdateTask(context.Background(), db, id, TaskPatch{Status: &done}, "sessB", now.Add(time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict, "a non-holder cannot update a live-claimed task")

	// A caller with no session identity is likewise blocked.
	_, err = UpdateTask(context.Background(), db, id, TaskPatch{Status: &done}, "", now.Add(time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict, "an empty actor is not the holder")

	// The holder still updates its own task.
	title := "A renamed"
	updated, err := UpdateTask(context.Background(), db, id, TaskPatch{Title: &title}, "sessA", now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, "A renamed", updated.Title)

	// After the lease expires the claim is no longer live, so a non-holder update
	// is allowed (matching ClaimTask's lazy-expiry reclaim).
	afterExpiry := now.Add(2 * time.Minute)
	reopened, err := UpdateTask(context.Background(), db, id, TaskPatch{Status: statusPtr(core.TaskOpen)}, "sessB", afterExpiry)
	require.NoError(t, err, "an expired lease no longer locks the task")
	require.Equal(t, core.TaskOpen, reopened.Status)
}

// TestUpdateTask_ReopenClearsClaim is the regression for a reopened task
// keeping a stale claim: before the fix, moving an in_progress task back to
// open via UpdateTask left claimed_by/lease_expires_at behind, so the task
// showed in the ready queue but was permanently unclaimable (ClaimTask requires
// an empty claimed_by on open tasks, and both release paths require in_progress).
func TestUpdateTask_ReopenClearsClaim(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	// After the lease lapses, a non-holder reopens the task via UpdateTask.
	reopened, err := UpdateTask(context.Background(), db, id,
		TaskPatch{Status: statusPtr(core.TaskOpen)}, "sessB", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, reopened.Status)
	require.Empty(t, reopened.ClaimedBy, "reopening must release the stale claim")
	require.Nil(t, reopened.LeaseExpiresAt)

	// The reopened task is both ready and actually claimable again.
	require.Equal(t, []string{"A"}, readyTitles(t, db, "demo"))
	res, err := ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(3*time.Minute))
	require.NoError(t, err)
	require.Equal(t, "sessB", res.Task.ClaimedBy)
}

// TestUpdateTask_HolderReopenReleasesClaim: the holder reopening its own
// live-claimed task releases the claim, matching ReleaseTask semantics.
func TestUpdateTask_HolderReopenReleasesClaim(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	reopened, err := UpdateTask(context.Background(), db, id,
		TaskPatch{Status: statusPtr(core.TaskOpen)}, "sessA", now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, reopened.Status)
	require.Empty(t, reopened.ClaimedBy)
	require.Nil(t, reopened.LeaseExpiresAt)

	// Another session can claim it immediately.
	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(2*time.Second))
	require.NoError(t, err)
}

// TestForceReleaseTask covers the owner override: it reopens a claimed task
// regardless of holder or lease liveness, and errors when there is nothing to
// release.
func TestForceReleaseTask(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "A", 1)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)

	// Force-release while the lease is still live (the override's whole point).
	released, err := ForceReleaseTask(context.Background(), db, id, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, released.Status)
	require.Empty(t, released.ClaimedBy)
	require.Nil(t, released.LeaseExpiresAt)

	// Reopened -> a different session can now claim it.
	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, now.Add(2*time.Second))
	require.NoError(t, err)

	// Force-releasing a task that is not in progress is a conflict, not a no-op.
	other := addTask(t, db, "demo", "B", 2)
	_, err = ForceReleaseTask(context.Background(), db, other, now)
	require.ErrorIs(t, err, ErrTaskClaimConflict, "an unclaimed task has no lock to release")

	// An unknown id is a not-found.
	_, err = ForceReleaseTask(context.Background(), db, "nope", now)
	require.ErrorIs(t, err, ErrTaskNotFound)
}

func TestReleaseClaimsForSession(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2)
	c := addTask(t, db, "demo", "C", 3)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, a, "sessA", time.Minute, now)
	require.NoError(t, err)
	_, err = ClaimTask(context.Background(), db, b, "sessA", time.Minute, now)
	require.NoError(t, err)
	_, err = ClaimTask(context.Background(), db, c, "sessB", time.Minute, now)
	require.NoError(t, err)

	n, err := ReleaseClaimsForSession(context.Background(), db, "sessA", now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 2, n, "both of sessA's claims are released")

	// sessB's claim is untouched.
	got, err := TaskByID(context.Background(), db, c)
	require.NoError(t, err)
	require.Equal(t, "sessB", got.ClaimedBy)
	require.Equal(t, core.TaskInProgress, got.Status)
}

func TestPlanFilteredQueuesSeparatePlanFromDefault(t *testing.T) {
	db := newTaskDB(t)
	plain := addTask(t, db, "demo", "plain", 1)
	step := addPlanTask(t, db, "demo", "myplan", "step", 2)

	// Default ready-queue excludes plan steps.
	require.Equal(t, []string{"plain"}, readyTitles(t, db, "demo"))

	// Plan-scoped ready returns only that plan's steps.
	planReady, err := ReadyTasksForPlan(context.Background(), db, "demo", "myplan")
	require.NoError(t, err)
	require.Len(t, planReady, 1)
	require.Equal(t, step, planReady[0].ID)

	// Default list excludes plan steps; plan list includes only them.
	def, err := ListTasks(context.Background(), db, "demo", "")
	require.NoError(t, err)
	require.Len(t, def, 1)
	require.Equal(t, plain, def[0].ID)

	planList, err := ListTasksForPlan(context.Background(), db, "demo", "", "myplan")
	require.NoError(t, err)
	require.Len(t, planList, 1)
	require.Equal(t, step, planList[0].ID)
}

func TestActivePlansRollup(t *testing.T) {
	db := newTaskDB(t)
	// Plan "p1": 4 steps -- one done, one in flight (claimed), one claimable,
	// and one open but blocked by the claimable step (so not claimable).
	s1 := addPlanTask(t, db, "demo", "p1", "s1", 1)
	s2 := addPlanTask(t, db, "demo", "p1", "s2", 2)
	s3 := addPlanTask(t, db, "demo", "p1", "s3", 3)
	addPlanTask(t, db, "demo", "p1", "s4", 4, s3)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	setStatus(t, db, s1, core.TaskDone)
	_, err := ClaimTask(context.Background(), db, s2, "sessA", time.Minute, now)
	require.NoError(t, err)

	// Plan "p2": all steps closed -> omitted from active plans.
	d := addPlanTask(t, db, "demo", "p2", "done", 5)
	setStatus(t, db, d, core.TaskDone)

	plans, err := ActivePlans(context.Background(), db, "demo")
	require.NoError(t, err)
	require.Len(t, plans, 1, "only the incomplete plan is active")
	p := plans[0]
	require.Equal(t, "p1", p.Slug)
	require.Equal(t, 4, p.Total)
	require.Equal(t, 1, p.Done)
	require.Equal(t, 1, p.InFlight)
	require.Equal(t, 1, p.Claimable, "a blocked open step is not claimable")
}

func TestActivePlansOrderByLastActivity(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	slugs := func(plans []PlanRollup) []string {
		out := make([]string, len(plans))
		for i, p := range plans {
			out[i] = p.Slug
		}
		return out
	}

	// Newest step activity inverts the slug order; alpha gets a second step so
	// closing one later keeps the plan active.
	a1 := addPlanTask(t, db, "demo", "alpha", "a1", 1)
	addPlanTask(t, db, "demo", "alpha", "a2", 2)
	addPlanTask(t, db, "demo", "mid", "m1", 5)
	addPlanTask(t, db, "demo", "zeta", "z1", 10)

	plans, err := ActivePlans(ctx, db, "demo")
	require.NoError(t, err)
	require.Equal(t, []string{"zeta", "mid", "alpha"}, slugs(plans),
		"most recently active first, not alphabetical")
	require.Equal(t, time.Date(2026, 7, 10, 12, 10, 0, 0, time.UTC), plans[0].LastActivity,
		"LastActivity is the newest step updated_at")

	// Touching an alpha step bumps the whole plan to the top.
	done := core.TaskDone
	_, err = UpdateTask(ctx, db, a1, TaskPatch{Status: &done}, "",
		time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	plans, err = ActivePlans(ctx, db, "demo")
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zeta", "mid"}, slugs(plans))
	require.Equal(t, time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC), plans[0].LastActivity)
}

// clNoticeRows counts every claim notice, delivered or not.
func clNoticeRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM claim_notices`).Scan(&n))
	return n
}

// clPending is the notices waiting for session.
func clPending(t *testing.T, db *sql.DB, session string) []ClaimNotice {
	t.Helper()
	pending, err := PendingClaimNotices(context.Background(), db, session)
	require.NoError(t, err)
	return pending
}

// A claim taken after the holder's lease lapsed records exactly one notice for
// the holder, in the claim's own transaction: what it held, who took it, when,
// and the lease that lapsed. The new holder is told nothing, and its heartbeat
// records nothing more.
func TestClaimTask_ReclaimRecordsOneNoticeForThePriorHolder(t *testing.T) {
	db := newTaskDB(t)
	id := addTask(t, db, "example-ops", "A", 1)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, id, "sessA", time.Minute, now)
	require.NoError(t, err)
	_, err = ClaimTask(context.Background(), db, id, "sessA", time.Minute, now.Add(30*time.Second))
	require.NoError(t, err, "the holder's heartbeat")
	require.Zero(t, clNoticeRows(t, db), "a heartbeat loses nothing")

	stolenAt := now.Add(3 * time.Minute)
	res, err := ClaimTask(context.Background(), db, id, "sessB", time.Minute, stolenAt)
	require.NoError(t, err)
	require.True(t, res.Reclaimed)

	pending := clPending(t, db, "sessA")
	require.Len(t, pending, 1)
	n := pending[0]
	require.NotEmpty(t, n.ID)
	require.Equal(t, ClaimResourceTask, n.Kind)
	require.Equal(t, id, n.ResourceID)
	require.Empty(t, n.Label, "a task is named by its id")
	require.Equal(t, "example-ops", n.Project)
	require.Equal(t, "sessB", n.NewHolder)
	require.Equal(t, ClaimLostReclaimed, n.Reason)
	require.NotNil(t, n.LeaseExpiresAt)
	require.True(t, now.Add(30*time.Second+time.Minute).Equal(*n.LeaseExpiresAt), "the lease that lapsed")
	require.True(t, stolenAt.Equal(n.LostAt))
	require.Nil(t, n.DeliveredAt)

	_, err = ClaimTask(context.Background(), db, id, "sessB", time.Minute, stolenAt.Add(time.Second))
	require.NoError(t, err)
	require.Empty(t, clPending(t, db, "sessB"))
	require.Equal(t, 1, clNoticeRows(t, db), "exactly one notice")
}

// A claim that takes the task from nobody records no notice: the first claim,
// a claimless in_progress row (`seam task start`), stale residue on an open
// row, and a refused claim.
func TestClaimTask_NoNoticeWithoutAPriorHolder(t *testing.T) {
	db := newTaskDB(t)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

	first := addTask(t, db, "demo", "first", 1)
	_, err := ClaimTask(context.Background(), db, first, "sessA", time.Minute, now)
	require.NoError(t, err)
	_, err = ClaimTask(context.Background(), db, first, "sessB", time.Minute, now.Add(time.Second))
	require.ErrorIs(t, err, ErrTaskClaimConflict, "a refused claim takes nothing")

	claimless := addTask(t, db, "demo", "claimless", 2)
	_, err = UpdateTask(context.Background(), db, claimless, TaskPatch{Status: statusPtr(core.TaskInProgress)}, "", now)
	require.NoError(t, err)
	_, err = ClaimTask(context.Background(), db, claimless, "sessB", time.Minute, now)
	require.NoError(t, err)

	residue := addTask(t, db, "demo", "residue", 3)
	_, err = db.Exec(`UPDATE tasks SET claimed_by = 'deadsess', lease_expires_at = ? WHERE id = ?`,
		core.FormatTime(now.Add(-time.Hour)), residue)
	require.NoError(t, err)
	_, err = ClaimTask(context.Background(), db, residue, "sessB", time.Minute, now)
	require.NoError(t, err)

	require.Zero(t, clNoticeRows(t, db))
}

// The owner's force release records one notice for the holder, whether its
// lease was live or had lapsed; a claimless in_progress row has no holder to
// tell, and a refused release records nothing.
func TestForceReleaseTask_RecordsANoticeForTheHolder(t *testing.T) {
	db := newTaskDB(t)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

	live := addTask(t, db, "example-ops", "live", 1)
	_, err := ClaimTask(context.Background(), db, live, "sessA", time.Hour, now)
	require.NoError(t, err)
	releasedAt := now.Add(time.Minute)
	_, err = ForceReleaseTask(context.Background(), db, live, releasedAt)
	require.NoError(t, err)
	pending := clPending(t, db, "sessA")
	require.Len(t, pending, 1)
	require.Equal(t, ClaimLostReleased, pending[0].Reason)
	require.Equal(t, live, pending[0].ResourceID)
	require.Empty(t, pending[0].NewHolder, "nobody holds it after the release")
	require.True(t, releasedAt.Equal(pending[0].LostAt))

	lapsed := addTask(t, db, "example-ops", "lapsed", 2)
	_, err = ClaimTask(context.Background(), db, lapsed, "sessB", time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = ForceReleaseTask(context.Background(), db, lapsed, now)
	require.NoError(t, err)
	require.Len(t, clPending(t, db, "sessB"), 1, "a lapsed claim still named its holder")

	claimless := addTask(t, db, "example-ops", "claimless", 3)
	_, err = UpdateTask(context.Background(), db, claimless, TaskPatch{Status: statusPtr(core.TaskInProgress)}, "", now)
	require.NoError(t, err)
	_, err = ForceReleaseTask(context.Background(), db, claimless, now)
	require.NoError(t, err)

	open := addTask(t, db, "example-ops", "open", 4)
	_, err = ForceReleaseTask(context.Background(), db, open, now)
	require.ErrorIs(t, err, ErrTaskClaimConflict)
	_, err = ForceReleaseTask(context.Background(), db, "nope", now)
	require.ErrorIs(t, err, ErrTaskNotFound)

	require.Equal(t, 2, clNoticeRows(t, db), "one notice per holder told, none for the rest")
}

// The reaper's release reopens the session's lapsed claims with a notice each,
// and leaves a live claim alone: one taken after the reap is its holder's own.
func TestReleaseReapedClaims_LapsedOnlyWithANoticeEach(t *testing.T) {
	db := newTaskDB(t)
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	lapsed := addTask(t, db, "example-ops", "lapsed", 1)
	_, err := ClaimTask(context.Background(), db, lapsed, "sessA", time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)
	live := addTask(t, db, "example-ops", "live", 2)
	_, err = ClaimTask(context.Background(), db, live, "sessA", time.Hour, now)
	require.NoError(t, err)
	others := addTask(t, db, "example-ops", "others", 3)
	_, err = ClaimTask(context.Background(), db, others, "sessB", time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)

	n, err := ReleaseReapedClaims(context.Background(), db, "sessA", now)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := TaskByID(context.Background(), db, lapsed)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, got.Status)
	require.Empty(t, got.ClaimedBy)
	got, err = TaskByID(context.Background(), db, live)
	require.NoError(t, err)
	require.Equal(t, "sessA", got.ClaimedBy, "a live claim is not the reaper's to release")
	got, err = TaskByID(context.Background(), db, others)
	require.NoError(t, err)
	require.Equal(t, "sessB", got.ClaimedBy, "another session's claim is untouched")

	pending := clPending(t, db, "sessA")
	require.Len(t, pending, 1)
	require.Equal(t, ClaimLostReaped, pending[0].Reason)
	require.Equal(t, lapsed, pending[0].ResourceID)
	require.True(t, now.Add(-time.Hour+time.Minute).Equal(*pending[0].LeaseExpiresAt))

	n, err = ReleaseReapedClaims(context.Background(), db, "", now)
	require.NoError(t, err)
	require.Zero(t, n)
}
