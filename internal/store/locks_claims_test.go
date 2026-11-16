package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A live repo lock keeps its session live as a live task claim does: the
// reaper leaves a quiet holder alone, every reader counts it live, and the
// first pass after the lease lapses reaps it. A lease ending exactly at now
// keeps nothing live (core.RepoLock.HoldLive's boundary).
func TestRepoLockHold_KeepsItsSessionLiveUntilItLapses(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	const idle = 45 * time.Minute
	holder := clSession(t, db, "cc/landing", "example-ops", core.SessionActive, now.Add(-2*time.Hour))
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	_, err := AcquireRepoLock(ctx, db, l.ID, holder, 30*time.Minute, now)
	require.NoError(t, err)

	sess, ok, err := SessionByID(ctx, db, holder)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, sess.HeldUntil, "the lock's lease is a hold")
	require.Equal(t, now.Add(30*time.Minute), *sess.HeldUntil)
	require.True(t, sess.LiveAsOf(now, idle))

	expired, err := ExpireIdleSessions(ctx, db, now, idle)
	require.NoError(t, err)
	require.Empty(t, expired, "a live lock keeps its quiet session")
	n, err := LiveSessionCount(ctx, db, now.Add(-idle))
	require.NoError(t, err)
	require.Equal(t, 1, n, "and the live count counts it")
	listed, err := ListSessions(ctx, db, core.SessionActive, now.Add(-idle), 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, holder, listed[0].ID)

	// Exactly at the lease's end the hold is no longer live.
	lapse := now.Add(30 * time.Minute)
	expired, err = ExpireIdleSessions(ctx, db, lapse, idle)
	require.NoError(t, err)
	require.Len(t, expired, 1, "the first pass after the lease lapses reaps it")
	require.Equal(t, holder, expired[0].ID)
}

// A lapsed hold another session takes over is the prior holder's lost hold:
// it hears of it once, with the lock's id and name and who took it. A
// heartbeat, a take of a free lock and a holder's own release tell nobody.
func TestAcquireRepoLock_TakingALapsedHoldTellsItsHolderOnce(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock

	_, err := AcquireRepoLock(ctx, db, l.ID, "sessA", time.Minute, lkNow)
	require.NoError(t, err)
	none, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Empty(t, none, "a free lock was taken from nobody")

	took, err := AcquireRepoLock(ctx, db, l.ID, "sessB", time.Minute, lkNow.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, took.Reclaimed)
	pending, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	n := pending[0]
	require.Equal(t, ClaimResourceLock, n.Kind)
	require.Equal(t, l.ID, n.ResourceID)
	require.Equal(t, "main", n.Label)
	require.Empty(t, n.Project, "a lock is no project's")
	require.Equal(t, "sessB", n.NewHolder)
	require.Equal(t, ClaimLostReclaimed, n.Reason)
	require.NotNil(t, n.LeaseExpiresAt)
	require.Equal(t, lkNow.Add(time.Minute), *n.LeaseExpiresAt)

	text := n.Text("cc/taker")
	require.Contains(t, text, "Lock lost: your hold on repo lock "+l.ID+" (main) lapsed at ")
	require.Contains(t, text, "session cc/taker acquired it at")
	require.Contains(t, text, "acquire it again with locks_acquire once it is free")
	require.NotContains(t, text, "global scope")
	require.NotContains(t, text, "project")

	_, err = AcquireRepoLock(ctx, db, l.ID, "sessB", time.Minute, lkNow.Add(3*time.Minute))
	require.NoError(t, err, "a heartbeat")
	_, err = ReleaseRepoLock(ctx, db, l.ID, "sessB", lkNow.Add(4*time.Minute))
	require.NoError(t, err)
	_, err = AcquireRepoLock(ctx, db, l.ID, "sessC", time.Minute, lkNow.Add(5*time.Minute))
	require.NoError(t, err)
	for _, s := range []string{"sessB", "sessC"} {
		got, err := PendingClaimNotices(ctx, db, s)
		require.NoError(t, err)
		require.Empty(t, got, "%s lost nothing it does not know about", s)
	}
	again, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Len(t, again, 1, "still the one notice")
}

// The owner's force-release tells the holder once, whether its lease was live
// or lapsed; a free lock has nobody to tell.
func TestForceReleaseRepoLock_TellsTheHolderOnce(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	_, err := AcquireRepoLock(ctx, db, l.ID, "sessA", time.Hour, lkNow)
	require.NoError(t, err)

	freed, err := ForceReleaseRepoLock(ctx, db, l.ID, lkNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "sessA", freed.PriorHolder)
	require.Empty(t, freed.Lock.Holder)
	pending, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, ClaimResourceLock, pending[0].Kind)
	require.Equal(t, ClaimLostReleased, pending[0].Reason)
	require.Empty(t, pending[0].NewHolder)
	require.Contains(t, pending[0].Text(""), "Lock lost: the owner released your hold on repo lock "+l.ID+" (main) at ")

	_, err = ForceReleaseRepoLock(ctx, db, l.ID, lkNow.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrRepoLockFree)
	again, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Len(t, again, 1)
}

// The reaper's release frees only the holds that lapsed, each with a notice;
// a hold live at the pass is the session's own again and stays.
func TestReleaseReapedRepoLocks_FreesOnlyLapsedHolds(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	lapsed := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	live := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "release").Lock
	_, err := AcquireRepoLock(ctx, db, lapsed.ID, "sessA", time.Hour, lkNow.Add(-2*time.Hour))
	require.NoError(t, err)
	_, err = AcquireRepoLock(ctx, db, live.ID, "sessA", time.Hour, lkNow)
	require.NoError(t, err)

	n, err := ReleaseReapedRepoLocks(ctx, db, "sessA", lkNow)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, _, err := RepoLockByID(ctx, db, lapsed.ID)
	require.NoError(t, err)
	require.Empty(t, got.Holder, "the lapsed hold is freed")
	got, _, err = RepoLockByID(ctx, db, live.ID)
	require.NoError(t, err)
	require.Equal(t, "sessA", got.Holder, "the live hold stays")

	pending, err := PendingClaimNotices(ctx, db, "sessA")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, lapsed.ID, pending[0].ResourceID)
	require.Equal(t, ClaimLostReaped, pending[0].Reason)
	require.Contains(t, pending[0].Text(""), "when this session had gone idle. Acquire it again with locks_acquire")

	n, err = ReleaseReapedRepoLocks(ctx, db, "", lkNow)
	require.NoError(t, err)
	require.Zero(t, n, "no session, nothing to free")
}

// A task notice's words are a claim's, as before locks shared the notices.
func TestClaimNoticeText_ATaskKeepsAClaimsWords(t *testing.T) {
	at := time.Date(2026, 11, 15, 14, 5, 0, 0, time.UTC)
	n := ClaimNotice{Kind: ClaimResourceTask, ResourceID: "01T", Project: "demo", Reason: ClaimLostReclaimed,
		NewHolder: "sessB", LeaseExpiresAt: &at, LostAt: at.Add(15 * time.Minute)}
	text := n.Text("cc/taker")
	require.Contains(t, text, "Claim lost: your claim on task 01T in project demo lapsed at ")
	require.Contains(t, text, "session cc/taker claimed it at")
	require.Contains(t, text, "claim it again with tasks_claim once it is free.")
	require.Contains(t, text, "To keep a claim through long work, re-claim it before its lease lapses.")
}
