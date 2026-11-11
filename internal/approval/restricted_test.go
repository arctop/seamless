package approval

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Restricted local accounts (peer_auth.restricted) file no access request and
// no read grant, and decide nothing: every entry point of the approval service
// refuses them plainly, and no access_approvals, access_leases or
// access_grants row is ever written for them.

const restrictedUID = 502

// restrictedView is a restricted account's socket view for chain, its reach
// filled the way the transport fills it (every open project).
func restrictedView(t *testing.T, f *reqFixture, chain ...access.ProcessToken) access.View {
	t.Helper()
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{lockFin, lockLeg},
		Restricted: []string{"_example_agent"}}, func(n string) (access.Account, error) {
		switch n {
		case "owner":
			return access.Account{UID: 501, Name: "owner"}, nil
		case "_example_agent":
			return access.Account{UID: restrictedUID, Name: "_example_agent"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	f.policy = p
	v, ok := p.PeerChainView(restrictedUID, chain)
	require.True(t, ok)
	open, err := store.OpenProjectSlugs(context.Background(), f.db)
	require.NoError(t, err)
	v = v.WithOpenProjects(open)
	require.Equal(t, "_example_agent", v.Account())
	return v
}

// countRows counts a table's rows.
func countRows(t *testing.T, f *reqFixture, table string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n))
	return n
}

func TestRestricted_FilesNoRequestAndNoGrant(t *testing.T) {
	f := newGrantFixture(t)
	v := restrictedView(t, f, bridge, agent, shell)
	rctx := access.WithView(context.Background(), v)
	ops := f.session(t, "cc/ops", "/work/ops", agent)

	// A lock's request, by the view it states and by the context it runs in,
	// for a locked project and for one it reaches: refused before anything
	// is looked up, with the same plain error.
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		project string
	}{
		{"the view names it, a locked project", context.Background(), lockFin},
		{"the view names it, an open project", context.Background(), openOps},
		{"the context names it", rctx, lockLeg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Request(tc.ctx, RequestInput{Project: tc.project, View: v, SessionID: ops,
				AgentProcess: agent.AgentID(), Reason: "reconcile the ledger notes", Via: "refusal"})
			require.ErrorIs(t, err, ErrRestricted)
			require.ErrorContains(t, err, "a restricted local account files no access requests")
		})
	}
	// The owner's view stated over a restricted context is still refused: the
	// context is who is calling.
	owner := f.view(t, bridge, agent, shell)
	_, err := f.svc.Request(rctx, RequestInput{Project: lockFin, View: owner, SessionID: ops})
	require.ErrorIs(t, err, ErrRestricted)

	// A read grant, in either tier.
	hr := f.sessionIn(t, openOps, "cc/ops-2")
	for name, g := range map[string]*Grants{
		"advisory": f.advisory(),
		"peer":     NewGrants(GrantsConfig{DB: f.db, Approvals: f.svc}),
	} {
		_, err := g.RequestGrant(rctx, GrantInput{Project: confHR, SessionID: hr, AgentProcess: agent.AgentID(), Via: "refusal"})
		require.ErrorIs(t, err, ErrRestricted, name)
	}

	require.Zero(t, countRows(t, f, "access_approvals"), "no request row")
	require.Zero(t, countRows(t, f, "access_leases"))
	require.Zero(t, countRows(t, f, "access_grants"))
	require.Empty(t, f.eventsOf(t, core.EventAccessRequested), "nothing was filed, so nothing is recorded")
}

// A restricted account decides nothing: an approval, a grant's approval and a
// denial through its context are refused and leave the request pending; the
// owner's decision of the same request still lands.
func TestRestricted_DecidesNothing(t *testing.T) {
	f := newGrantFixture(t)
	ops := f.session(t, "cc/ops", "/work/ops", agent)
	a := f.request(t, lockFin, ops, bridge, agent, shell)
	rctx := access.WithView(context.Background(), restrictedView(t, f, bridge, agent, shell))

	facts, err := f.svc.Facts(context.Background(), a.ID)
	require.NoError(t, err)
	d := Decision{ID: a.ID, Duration: ForSession, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}}
	_, err = f.svc.Approve(rctx, d)
	require.ErrorIs(t, err, ErrRestricted)
	require.ErrorIs(t, f.svc.Deny(rctx, a.ID, "console"), ErrRestricted)
	_, err = NewGrants(GrantsConfig{DB: f.db, Approvals: f.svc}).Approve(rctx, d)
	require.ErrorIs(t, err, ErrRestricted)
	_, err = f.svc.BeginConsoleUnlock(rctx, lockFin, "token-hash")
	require.ErrorIs(t, err, ErrRestricted)
	_, err = f.svc.BeginPasskey(rctx, "a key")
	require.ErrorIs(t, err, ErrRestricted)
	_, err = f.svc.ConfirmPasskeyCode(rctx, "an-enrollment", f.code(t, store.ApproverPasskey, []byte("a key")))
	require.ErrorIs(t, err, ErrRestricted)
	_, err = f.svc.EnrollTouchID(rctx, "a key", f.code(t, store.ApproverTouchID, []byte("a key")), nil, nil)
	require.ErrorIs(t, err, ErrRestricted)

	got, err := store.AccessApprovalByID(context.Background(), f.db, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalPending, got.Status, "a refused decision leaves the request as it was")
	require.Zero(t, countRows(t, f, "access_leases"))
	require.Zero(t, countRows(t, f, "approver_credentials"))

	lease := f.approveDaemon(t, a.ID, ForSession)
	require.Equal(t, lockFin, lease.Project, "the owner's own decision is unchanged")
	require.WithinDuration(t, f.now.Add(8*time.Hour), lease.ExpiresAt, time.Second)
}
