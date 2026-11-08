package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Read grants (isolation.requests): who may ask, the copy line every surface
// shows, the advisory tier's bearer-key decision, and the peer tier's anchor
// and proofs -- the same as a lock's lease, never a lease.

const (
	confHR  = "example-hr"    // confidential, not locked
	sealedV = "example-vault" // sealed
)

func newGrantFixture(t *testing.T) *reqFixture {
	t.Helper()
	f := newReqFixture(t)
	ctx := context.Background()
	for slug, state := range map[string]core.Isolation{confHR: core.IsolationConfidential, sealedV: core.IsolationSealed} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, f.db, slug, state))
	}
	return f
}

func (f *reqFixture) advisory() *Grants {
	return NewGrants(GrantsConfig{DB: f.db, Events: f.rec, LeaseCap: 8 * time.Hour, Now: func() time.Time { return f.now }})
}

func (f *reqFixture) sessionIn(t *testing.T, project, name string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(context.Background(), f.db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, CWD: "/work/" + project,
		Model: "example-model", CreatedAt: f.now, UpdatedAt: f.now,
	}))
	return id
}

func (f *reqFixture) eventsOf(t *testing.T, kind core.EventKind) []core.Event {
	t.Helper()
	evs, err := f.rec.ByKinds(context.Background(), []core.EventKind{kind}, "", "", 200)
	require.NoError(t, err)
	return evs
}

func TestRequestGrant_WhoMayAsk(t *testing.T) {
	f := newGrantFixture(t)
	g := f.advisory()
	ctx := context.Background()
	ops := f.sessionIn(t, openOps, "cc/ops")
	hr := f.sessionIn(t, confHR, "cc/hr")
	vault := f.sessionIn(t, sealedV, "cc/vault")
	global := f.sessionIn(t, "", "cc/global")

	a, err := g.RequestGrant(ctx, GrantInput{Project: lockFin, SessionID: ops, Reason: "check the invoice totals", Via: "refusal"})
	require.NoError(t, err, "an open project's session may ask for a confidential one")
	require.Equal(t, store.AccessKindGrant, a.Kind)
	require.Equal(t, openOps, a.FromProject)
	require.Equal(t, ops, a.SessionID)
	require.Empty(t, a.ProcessKey, "without peer_auth the session is the holder")
	again, err := g.RequestGrant(ctx, GrantInput{Project: lockFin, SessionID: ops})
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID, "a repeated ask answers with the pending request")

	_, err = g.RequestGrant(ctx, GrantInput{Project: lockFin, SessionID: hr})
	require.NoError(t, err, "a confidential project's session may ask too")

	for name, tc := range map[string]struct {
		in   GrantInput
		want error
	}{
		"a sealed target":         {GrantInput{Project: sealedV, SessionID: ops}, ErrSealed},
		"a sealed requester":      {GrantInput{Project: lockFin, SessionID: vault}, ErrSealed},
		"an open target":          {GrantInput{Project: openOps, SessionID: hr}, ErrNotConfidential},
		"an unknown target":       {GrantInput{Project: "example-none", SessionID: ops}, ErrNotConfidential},
		"its own project":         {GrantInput{Project: confHR, SessionID: hr}, ErrNotConfidential},
		"the global scope":        {GrantInput{Project: lockFin, SessionID: global}, ErrNoProject},
		"no session":              {GrantInput{Project: lockFin}, ErrNoSession},
		"a session elsewhere":     {GrantInput{Project: lockFin, SessionID: ops, From: confHR}, store.ErrInvalidAccessRequest},
		"a peer view, advisory":   {GrantInput{Project: lockFin, SessionID: ops}, ErrNoProcess},
		"a reason over the limit": {GrantInput{Project: confHR, SessionID: ops, Reason: string(make([]rune, 201))}, store.ErrInvalidAccessRequest},
	} {
		c := ctx
		if name == "a peer view, advisory" {
			c = access.WithView(ctx, f.view(t, bridge, agent))
		}
		_, err := g.RequestGrant(c, tc.in)
		require.ErrorIs(t, err, tc.want, name)
	}
	// Nothing a refusal above filed: two requests, both grants.
	pending, err := g.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.Len(t, f.eventsOf(t, core.EventAccessRequested), 2)
}

func TestGrantFacts_CopyLineAndReadOnly(t *testing.T) {
	f := newGrantFixture(t)
	g := f.advisory()
	ctx := context.Background()
	ops := f.sessionIn(t, openOps, "cc/ops")
	a, err := g.RequestGrant(ctx, GrantInput{Project: lockFin, SessionID: ops})
	require.NoError(t, err)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "read-only", facts.Access)
	require.Equal(t, store.AccessKindGrant, facts.Kind)
	require.Equal(t, openOps, facts.From)
	require.Equal(t, "confidential", facts.Isolation)
	require.Contains(t, facts.Warnings, Warning{Code: WarnCopyInto,
		Text: "content from example-finance may be copied into example-ops's memories"})
	require.NotEmpty(t, facts.Digest)

	// A lock's request carries no From, and its JSON no from key.
	lockFacts := Facts{ID: "x"}
	require.Empty(t, lockFacts.From)
}

func TestGrantApprove_AdvisoryKey(t *testing.T) {
	f := newGrantFixture(t)
	g := f.advisory()
	ctx := context.Background()
	ops := f.sessionIn(t, openOps, "cc/ops")
	hr := f.sessionIn(t, confHR, "cc/hr")
	a, err := g.RequestGrant(ctx, GrantInput{Project: lockFin, SessionID: ops})
	require.NoError(t, err)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)

	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: "stale", Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrFactsChanged)
	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: "2h", Digest: facts.Digest, Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrDuration)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrIsGrant, "the lease core never approves a grant request")

	grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)
	require.Equal(t, lockFin, grant.Project)
	require.Equal(t, openOps, grant.FromProject)
	require.Equal(t, ops, grant.SessionID)
	require.Equal(t, "bearer-key", grant.DecidedBy)
	require.Equal(t, f.now.Add(time.Hour), grant.ExpiresAt)

	granted := f.eventsOf(t, core.EventAccessGranted)
	require.Len(t, granted, 1)
	require.Equal(t, a.ID, granted[0].ItemID)
	require.Equal(t, lockFin, granted[0].ProjectSlug)
	require.Equal(t, grant.ID, granted[0].Payload["grant"])
	confined := f.eventsOf(t, core.EventAccessConfined)
	require.Len(t, confined, 1, "an open project's session is write-confined by its grant")
	require.Equal(t, openOps, confined[0].ProjectSlug)
	require.Equal(t, ops, confined[0].SessionID)

	// A confidential requester was confined already: no confinement event.
	b, err := g.RequestGrant(ctx, GrantInput{Project: lockFin, SessionID: hr})
	require.NoError(t, err)
	bf, err := g.Facts(ctx, b.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: b.ID, Duration: ForSession, Digest: bf.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)
	require.Len(t, f.eventsOf(t, core.EventAccessConfined), 1)

	// A target tightened to sealed while the request waited is not opened.
	c, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: ops})
	require.NoError(t, err)
	cf, err := g.Facts(ctx, c.ID)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, confHR, core.IsolationSealed))
	_, err = g.Approve(ctx, Decision{ID: c.ID, Duration: For1h, Digest: cf.Digest, Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrFactsChanged, "the isolation is one of the facts the owner saw")
	cf, err = g.Facts(ctx, c.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: c.ID, Duration: For1h, Digest: cf.Digest, Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrSealed, "looked at again, a sealed project is still never granted")

	// Revoke and the session end, each an access.revoked event.
	_, err = g.RevokeGrant(ctx, grant.ID)
	require.NoError(t, err)
	_, err = g.RevokeGrant(ctx, grant.ID)
	require.ErrorIs(t, err, store.ErrGrantNotLive)
	n, err := g.RevokeSession(ctx, hr)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	live, err := g.Live(ctx)
	require.NoError(t, err)
	require.Empty(t, live)
	require.Len(t, f.eventsOf(t, core.EventAccessRevoked), 2)
}

func TestGrant_PeerTierAnchorsAndProves(t *testing.T) {
	f := newGrantFixture(t)
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	require.False(t, g.Advisory())
	ctx := context.Background()
	sess := f.session(t, "cc/ops", "/work/ops", agent) // in example-ops, stamped with the agent

	// The TCP key holds no process: nothing to anchor.
	_, err := g.RequestGrant(access.WithView(ctx, f.policy.OwnerView()), GrantInput{Project: confHR, SessionID: sess})
	require.ErrorIs(t, err, ErrNoProcess)
	// No View at all is not peer_auth's caller.
	_, err = g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: sess})
	require.ErrorIs(t, err, ErrNoProcess)
	// A locked target is the lock's, never a grant's.
	_, err = g.RequestGrant(access.WithView(ctx, f.view(t, bridge, agent, shell)), GrantInput{Project: lockFin, SessionID: sess})
	require.ErrorIs(t, err, access.ErrNotReachable)

	vctx := access.WithView(ctx, f.view(t, bridge, agent, shell))
	a, err := g.RequestGrant(vctx, GrantInput{Project: confHR, SessionID: sess})
	require.NoError(t, err)
	require.Equal(t, agent.Key(), a.ProcessKey, "the grant will name the agent, as a lease does")
	require.Equal(t, agent.PID, a.PeerPID)
	require.Equal(t, openOps, a.FromProject)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)

	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrPeerProof, "the bearer key proves nothing under peer_auth")
	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest})
	require.ErrorIs(t, err, ErrNoProof)

	grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.NoError(t, err)
	require.Equal(t, agent.Key(), grant.ProcessKey)
	require.Equal(t, "daemon-account", grant.DecidedBy)
	require.Equal(t, f.now.Add(4*time.Hour), grant.ExpiresAt)

	// It opens reads for the agent's tree, and for nothing else.
	rows, err := store.AccessGrantsForHolder(ctx, f.db, []string{bridge.Key(), agent.Key(), shell.Key()}, "")
	require.NoError(t, err)
	judged := access.JudgeGrants(vctx, sess, f.now, store.AccessGrants(rows))
	require.True(t, access.ReadGranted(access.WithGrants(vctx, judged), openOps, confHR))
	stranger := access.WithView(ctx, f.view(t, otherBr, other))
	require.False(t, access.ReadGranted(access.WithGrants(stranger, access.JudgeGrants(stranger, sess, f.now, store.AccessGrants(rows))), openOps, confHR))

	// No lease was written.
	leases, err := store.LiveAccessLeases(ctx, f.db, f.now)
	require.NoError(t, err)
	require.Empty(t, leases)
}

// An asker that holds a live read grant asks again: while more than the
// expiry warning remains, the grant answers and nothing is filed; from the
// warning on, a new request is filed (a new approval). A grant from another
// project, or a revoked one, answers nothing.
func TestRequestGrant_LiveGrantAnswersUntilTheExpiryWarning(t *testing.T) {
	f := newGrantFixture(t)
	g := f.advisory()
	ctx := context.Background()
	ops := f.sessionIn(t, openOps, "cc/ops")
	a, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: ops})
	require.NoError(t, err)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)
	count := func() int {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM access_approvals`).Scan(&n))
		return n
	}
	before := count()

	for _, at := range []time.Time{f.now, grant.ExpiresAt.Add(-access.ExpiryWarning - time.Second)} {
		f.now = at
		_, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: ops, Via: "request"})
		require.ErrorIs(t, err, ErrAccessLive)
		var live *AccessLiveError
		require.ErrorAs(t, err, &live)
		require.Equal(t, AccessLiveError{Project: confHR, From: openOps, AccessID: grant.ID, ApprovalID: a.ID, Expires: grant.ExpiresAt}, *live)
		require.Equal(t, "project "+confHR+" is already readable from project "+openOps+" until "+access.Clock(grant.ExpiresAt)+
			" (approved as "+ShortID(a.ID)+"), so nothing was filed. Nothing extends it silently: from "+
			access.Clock(grant.ExpiresAt.Add(-30*time.Minute))+", 30 minutes before it ends, asking again files a new approval", live.Answer())
		require.Equal(t, before, count())
	}

	// Another project's session holds no grant: it asks as before.
	legal := f.sessionIn(t, lockLeg, "cc/legal")
	other, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: legal})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindGrant, other.Kind)

	// From the warning on: a new request.
	f.now = grant.ExpiresAt.Add(-access.ExpiryWarning)
	again, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: ops})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, again.ID)
	require.Equal(t, before+2, count())

	// A revoked grant answers nothing, whatever its expiry.
	require.NoError(t, g.Deny(ctx, again.ID, "test"))
	f.now = grant.GrantedAt
	_, err = g.RevokeGrant(ctx, grant.ID)
	require.NoError(t, err)
	_, err = g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: ops})
	require.NoError(t, err)
}
