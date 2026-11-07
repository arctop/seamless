package access

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Read grants as the access layer judges them (isolation.requests): a row is
// kept only for the holder the request speaks for, and what it keeps opens one
// target from one project, while every kept row confines writes.

const (
	grantOps  = "example-ops"
	grantFin  = "example-finance"
	grantHR   = "example-hr"
	grantBoot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
)

func TestJudgeGrants_NoPeerAuthKeysOnTheSession(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	rows := []Grant{
		{ID: "g1", Project: grantFin, From: grantOps, Session: "S1", Expires: now.Add(time.Hour)},
		{ID: "g2", Project: grantHR, From: grantOps, Session: "S2", Expires: now.Add(time.Hour)},           // another session's
		{ID: "g3", Project: grantHR, From: grantOps, Process: "boot:1:1", Expires: now.Add(time.Hour)},     // a process grant: never without peer_auth
		{ID: "g4", Project: grantHR, From: "example-legal", Session: "S1", Expires: now.Add(-time.Minute)}, // expired: confines only
	}
	ctx := WithGrants(context.Background(), JudgeGrants(context.Background(), "S1", now, rows))
	require.True(t, ReadGranted(ctx, grantOps, grantFin))
	require.False(t, ReadGranted(ctx, grantOps, grantHR), "another session's grant and a process grant are not this session's")
	require.False(t, ReadGranted(ctx, "example-legal", grantHR), "an expired grant reads nothing")
	require.False(t, ReadGranted(ctx, "", grantFin), "the global scope is never granted")
	require.False(t, ReadGranted(ctx, grantHR, grantFin), "a grant opens its target to its own From project only")
	require.Equal(t, []string{grantFin}, GrantedTargets(ctx, grantOps))
	require.Empty(t, GrantedTargets(ctx, grantHR))

	confined, ok := JudgeGrants(context.Background(), "S1", now, rows).Confined()
	require.True(t, ok)
	require.Equal(t, []string{"example-legal", grantOps}, confined, "every grant ever approved confines, live or not")
	require.False(t, WriteConfined(ctx, grantOps))
	require.True(t, WriteConfined(ctx, grantFin), "a write into the target is refused")
	require.True(t, WriteConfined(ctx, ""), "a write into the global scope is refused")

	// No session, no grants.
	require.Equal(t, Grants{}, JudgeGrants(context.Background(), "", now, rows))
}

func TestJudgeGrants_PeerAuthKeysOnTheChain(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	policy, err := New(config.PeerAuth{Owner: "owner"}, func(n string) (Account, error) {
		return Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	agent := ProcessToken{PID: 200, Version: 2000, Boot: grantBoot}
	bridge := ProcessToken{PID: 300, Version: 3000, Boot: grantBoot}
	stranger := ProcessToken{PID: 400, Version: 4000, Boot: grantBoot}
	rows := []Grant{
		{ID: "g1", Project: grantFin, From: grantOps, Process: agent.Key(), Expires: now.Add(time.Hour)},
		{ID: "g2", Project: grantHR, From: grantOps, Process: stranger.Key(), Expires: now.Add(time.Hour)},
		{ID: "g3", Project: grantHR, From: grantOps, Session: "S1", Expires: now.Add(time.Hour)}, // a session grant: never under peer_auth
		{ID: "g4", Project: "example-legal", From: grantOps, Process: agent.Key(), Expires: now.Add(time.Hour), Ended: true},
	}
	v, ok := policy.PeerChainView(501, []ProcessToken{bridge, agent})
	require.True(t, ok)
	ctx := WithView(context.Background(), v)
	g := JudgeGrants(ctx, "S1", now, rows)
	ctx = WithGrants(ctx, g)
	require.True(t, ReadGranted(ctx, grantOps, grantFin), "the agent's grant covers the bridge it started")
	require.False(t, ReadGranted(ctx, grantOps, grantHR), "a stranger's grant and a session grant are not this tree's")
	require.False(t, ReadGranted(ctx, grantOps, "example-legal"), "a revoked grant reads nothing")
	confined, ok := g.Confined()
	require.True(t, ok)
	require.Equal(t, []string{grantOps}, confined)

	// The TCP key holds none, whatever it is handed.
	tcp := WithView(context.Background(), policy.OwnerView())
	require.Equal(t, Grants{}, JudgeGrants(tcp, "S1", now, rows))
}

// No grants in ctx -- isolation.requests off -- answers no and confines
// nothing, which is how the fence stays exactly as it was.
func TestGrants_OffChangesNothing(t *testing.T) {
	ctx := context.Background()
	require.False(t, ReadGranted(ctx, grantOps, grantFin))
	require.Empty(t, GrantedTargets(ctx, grantOps))
	require.False(t, WriteConfined(ctx, grantFin))
	require.False(t, WriteConfined(ctx, ""))
	_, ok := GrantsFrom(ctx)
	require.False(t, ok)
}
