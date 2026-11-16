package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestExpireStaleSessions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	cutoff := now.Add(-core.SessionIdleTTL)

	mk := func(name string, updatedAt time.Time, status core.SessionStatus) string {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: "seamless", Status: status,
			CreatedAt: updatedAt, UpdatedAt: updatedAt,
		}))
		return id
	}

	live := mk("cc/live", now.Add(-5*time.Minute), core.SessionActive)       // fresh -> kept
	stale := mk("sess/stale", now.Add(-2*time.Hour), core.SessionActive)     // idle -> reaped
	done := mk("cc/done", now.Add(-3*time.Hour), core.SessionCompleted)      // terminal -> untouched
	borderline := mk("sess/edge", now.Add(-time.Minute), core.SessionActive) // just active -> kept

	reaped, err := ExpireStaleSessions(ctx, db, cutoff)
	require.NoError(t, err)
	require.Len(t, reaped, 1, "only the idle active session is reaped")
	require.Equal(t, stale, reaped[0].ID)

	// The reaped session flips to expired but keeps its last-active timestamp.
	got, ok, err := SessionByID(ctx, db, stale)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionExpired, got.Status)
	require.WithinDuration(t, now.Add(-2*time.Hour), got.UpdatedAt, time.Second,
		"updated_at is preserved as the last-alive time")

	// The live, borderline, and already-completed sessions are untouched.
	for id, want := range map[string]core.SessionStatus{
		live: core.SessionActive, borderline: core.SessionActive, done: core.SessionCompleted,
	} {
		s, ok, err := SessionByID(ctx, db, id)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, want, s.Status)
	}

	// Idempotent: a second pass finds nothing new (the reaped row is no longer active).
	again, err := ExpireStaleSessions(ctx, db, cutoff)
	require.NoError(t, err)
	require.Empty(t, again)
}

// Regression: a heartbeat landing between the reaper's candidate SELECT and its
// per-row flip must win -- the session is provably alive and must not be expired
// (nor have its task claims released). Reconstructs the interleaving with the
// same pieces ExpireStaleSessions runs: candidate read, then TouchSession, then
// the guarded flip.
func TestExpireStaleSessions_HeartbeatBetweenSelectAndFlip(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	cutoff := now.Add(-core.SessionIdleTTL)

	id, err := core.NewID()
	require.NoError(t, err)
	idle := now.Add(-2 * core.SessionIdleTTL)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/racing", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: idle, UpdatedAt: idle,
	}))

	// Reaper step 1: the candidate read sees the session as stale.
	candidates, err := staleActiveSessions(ctx, db, cutoff, now)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	// Interleaved heartbeat: the agent proves it is alive.
	require.NoError(t, TouchSession(ctx, db, id, now))

	// Reaper step 2: the flip must observe the heartbeat and decline.
	flipped, err := expireSessionIfStillStale(ctx, db, id, cutoff, now)
	require.NoError(t, err)
	require.False(t, flipped, "a heartbeated session must not be reaped")

	got, ok, err := SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, got.Status, "session stays active after the heartbeat")
}

// Regression: a graceful completion (session_end / SessionEnd hook) landing
// between the candidate SELECT and the flip must win -- the reaper must neither
// flip the row nor report it as reaped, or the caller would record a duplicate
// session.ended event and double-release its claims.
func TestExpireStaleSessions_GracefulEndBetweenSelectAndFlip(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	cutoff := now.Add(-core.SessionIdleTTL)

	id, err := core.NewID()
	require.NoError(t, err)
	idle := now.Add(-2 * core.SessionIdleTTL)
	sess := core.Session{
		ID: id, Name: "sess/ending", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: idle, UpdatedAt: idle,
	}
	require.NoError(t, CreateSession(ctx, db, sess))

	candidates, err := staleActiveSessions(ctx, db, cutoff, now)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	// Interleaved graceful end.
	sess.Status = core.SessionCompleted
	sess.Findings = "done"
	sess.UpdatedAt = now
	require.NoError(t, UpdateSession(ctx, db, sess))

	flipped, err := expireSessionIfStillStale(ctx, db, id, cutoff, now)
	require.NoError(t, err)
	require.False(t, flipped, "a completed session must not be reported as reaped")

	got, ok, err := SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionCompleted, got.Status, "graceful completion is preserved")
	require.Equal(t, "done", got.Findings)
}

func TestTouchSession(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	old := time.Now().UTC().Add(-2 * time.Hour)

	activeID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: activeID, Name: "cc/beat", ProjectSlug: "seamless", Status: core.SessionActive,
		ExternalSessionID: "beat-full", ExternalClient: "claude-code", Ambient: true,
		CreatedAt: old, UpdatedAt: old,
	}))
	doneID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: doneID, Name: "cc/gone", ProjectSlug: "seamless", Status: core.SessionCompleted,
		ExternalSessionID: "gone-full", ExternalClient: "claude-code", Ambient: true,
		CreatedAt: old, UpdatedAt: old,
	}))

	beat := time.Now().UTC()
	require.NoError(t, TouchSession(ctx, db, activeID, beat))
	require.NoError(t, TouchAmbientSession(ctx, db, "claude-code", "beat-full", beat))

	got, _, err := SessionByID(ctx, db, activeID)
	require.NoError(t, err)
	require.WithinDuration(t, beat, got.UpdatedAt, time.Second, "active session heartbeated forward")

	// Touching a completed session is a no-op: the active-only guard protects it
	// from being resurrected as live.
	require.NoError(t, TouchSession(ctx, db, doneID, beat))
	done, _, err := SessionByID(ctx, db, doneID)
	require.NoError(t, err)
	require.WithinDuration(t, old, done.UpdatedAt, time.Second, "completed session not touched")
	require.Equal(t, core.SessionCompleted, done.Status)

	// Empty id/name are safe no-ops.
	require.NoError(t, TouchSession(ctx, db, "", beat))
	require.NoError(t, TouchAmbientSession(ctx, db, "", "", beat))
}

// A session idle past the threshold while it holds a live claim is not reaped;
// the first pass at or after its last lease lapses reaps it. The boundary is
// ClaimLive's: a lease lapsing exactly at the pass's now keeps nothing live.
func TestExpireIdleSessions_ALiveClaimKeepsItsSession(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	idle := now.Add(-2 * time.Hour)
	holder := clSession(t, db, "cc/holder", "example-ops", core.SessionActive, idle)
	later := clSession(t, db, "cc/later", "example-ops", core.SessionActive, idle)
	clClaim(t, db, "example-ops", holder, 30*time.Minute, now)                // lapses at now+30m
	clClaim(t, db, "example-ops", later, 30*time.Minute+time.Nanosecond, now) // a nanosecond later

	reaped, err := ExpireIdleSessions(ctx, db, now, core.SessionIdleTTL)
	require.NoError(t, err)
	require.Empty(t, reaped, "idle, but holding live claims: both stay")

	reaped, err = ExpireIdleSessions(ctx, db, now.Add(30*time.Minute), core.SessionIdleTTL)
	require.NoError(t, err)
	require.Len(t, reaped, 1, "the lease lapsing exactly now keeps nothing live")
	require.Equal(t, holder, reaped[0].ID)

	reaped, err = ExpireIdleSessions(ctx, db, now.Add(30*time.Minute+time.Nanosecond), core.SessionIdleTTL)
	require.NoError(t, err)
	require.Len(t, reaped, 1)
	require.Equal(t, later, reaped[0].ID)
}

// A claim the session takes between the reaper's candidate read and its flip
// wins, as a heartbeat does: the session holds a live claim at flip time, so it
// is live and is not reaped.
func TestExpireIdleSessions_ClaimBetweenSelectAndFlip(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-core.SessionIdleTTL)
	id := clSession(t, db, "cc/claiming", "example-ops", core.SessionActive, now.Add(-2*time.Hour))

	candidates, err := staleActiveSessions(ctx, db, cutoff, now)
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	clClaim(t, db, "example-ops", id, 15*time.Minute, now)

	flipped, err := expireSessionIfStillStale(ctx, db, id, cutoff, now)
	require.NoError(t, err)
	require.False(t, flipped, "a session that just took a claim is live")
	got, ok, err := SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, got.Status)
}
