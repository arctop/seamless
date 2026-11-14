package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The one-time briefing after a move (migration 040): the move that opened a
// child is found from the child, the claim is single and survives a re-read,
// and a forgotten claim can be won again.
func TestMoveBriefing_ClaimOnceForTheChild(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen,
		"example-finance": core.IsolationConfidential} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, SetProjectIsolation(ctx, db, slug, state))
	}
	left := moveHome(t, db, "example-ops", "")
	a, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops"), migration039Now)
	require.NoError(t, err)
	done, err := MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, migration039Now)
	require.NoError(t, err)

	_, found, err := MoveIntoSession(ctx, db, left.ID)
	require.NoError(t, err)
	require.False(t, found, "the session a move left is no move's child")
	mi, found, err := MoveIntoSession(ctx, db, done.Child.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, done.Move.ID, mi.Move.ID)
	require.Equal(t, "example-finance", mi.Move.ToProject)
	require.Empty(t, mi.LeaseID, "no lock: no lease")
	require.False(t, mi.Briefed)

	at := migration039Now.Add(time.Minute)
	won, err := ClaimMoveBriefing(ctx, db, mi.Move.ID, at)
	require.NoError(t, err)
	require.True(t, won)
	won, err = ClaimMoveBriefing(ctx, db, mi.Move.ID, at)
	require.NoError(t, err)
	require.False(t, won, "claimed once")
	mi, _, err = MoveIntoSession(ctx, db, done.Child.ID)
	require.NoError(t, err)
	require.True(t, mi.Briefed)

	require.NoError(t, ForgetMoveBriefing(ctx, db, mi.Move.ID))
	won, err = ClaimMoveBriefing(ctx, db, mi.Move.ID, at)
	require.NoError(t, err)
	require.True(t, won, "a forgotten claim is won again")

	_, found, err = MoveIntoSession(ctx, db, "")
	require.NoError(t, err)
	require.False(t, found)
}
