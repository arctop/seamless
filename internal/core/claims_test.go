package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The one liveness rule: active, and updated within the threshold or holding a
// live claim. A session idle past the threshold is live exactly while a claim
// it holds is.
func TestSessionLiveAsOf_IdleOrHoldingALiveClaim(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ttl := 45 * time.Minute
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	for _, tc := range []struct {
		name    string
		status  SessionStatus
		updated time.Duration // before now
		held    *time.Time
		live    bool
	}{
		{"fresh, no claim", SessionActive, time.Minute, nil, true},
		{"updated exactly the threshold ago", SessionActive, ttl, nil, true},
		{"a nanosecond past the threshold", SessionActive, ttl + time.Nanosecond, nil, false},
		{"idle, no claim", SessionActive, 2 * time.Hour, nil, false},
		{"idle, holding a live claim", SessionActive, 2 * time.Hour, at(time.Hour), true},
		{"idle, its claim lapsed", SessionActive, 2 * time.Hour, at(-time.Minute), false},
		{"idle, its claim lapsing exactly now", SessionActive, 2 * time.Hour, at(0), false},
		{"idle, its claim lapsing a nanosecond from now", SessionActive, 2 * time.Hour, at(time.Nanosecond), true},
		{"completed, holding a live claim", SessionCompleted, time.Minute, at(time.Hour), false},
		{"expired, holding a live claim", SessionExpired, 2 * time.Hour, at(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Session{Status: tc.status, UpdatedAt: now.Add(-tc.updated), HeldUntil: tc.held}
			require.Equal(t, tc.live, s.LiveAsOf(now, ttl))
		})
	}

	// A non-positive threshold falls back to SessionIdleTTL.
	quiet := Session{Status: SessionActive, UpdatedAt: now.Add(-SessionIdleTTL + time.Minute)}
	require.True(t, quiet.LiveAsOf(now, 0))
	quiet.UpdatedAt = now.Add(-SessionIdleTTL - time.Minute)
	require.False(t, quiet.LiveAsOf(now, -1))
}

// The boundary is ClaimLive's: a lease lapsing exactly at now keeps neither the
// task held nor its session live, and one a nanosecond later keeps both.
func TestSessionHoldsAt_MatchesClaimLiveAtTheBoundary(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, lease := range []time.Time{now.Add(-time.Second), now, now.Add(time.Nanosecond), now.Add(time.Hour)} {
		task := Task{Status: TaskInProgress, ClaimedBy: "01SESSION", LeaseExpiresAt: &lease}
		sess := Session{Status: SessionActive, UpdatedAt: now.Add(-2 * time.Hour), HeldUntil: &lease}
		require.Equal(t, task.ClaimLive(now), sess.HoldsAt(now), "lease %s", lease)
		require.Equal(t, task.ClaimLive(now), sess.LiveAsOf(now, time.Minute), "lease %s", lease)
	}
	require.False(t, Session{Status: SessionActive}.HoldsAt(now), "no claim holds nothing")
}

// The ceiling is four hours, whole seconds (lease_seconds counts it).
func TestMaxClaimLease(t *testing.T) {
	require.Equal(t, 4*time.Hour, MaxClaimLease)
	require.Zero(t, MaxClaimLease%time.Second)
	require.Greater(t, MaxClaimLease, SessionIdleTTL, "a claim must be able to outlast the idle threshold")
}
