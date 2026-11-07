package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

const (
	boot    = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	lockFin = "example-finance"
	lockLeg = "example-legal"
	openOps = "example-ops"
)

var (
	bridge  = access.ProcessToken{PID: 300, Version: 3000, Boot: boot}
	agent   = access.ProcessToken{PID: 200, Version: 2000, Boot: boot}
	shell   = access.ProcessToken{PID: 100, Version: 1000, Boot: boot}
	other   = access.ProcessToken{PID: 210, Version: 2100, Boot: boot} // another agent, a sibling
	otherBr = access.ProcessToken{PID: 310, Version: 3100, Boot: boot}
)

type reqFixture struct {
	*fixture
	policy *access.Policy
	rec    *events.Recorder
}

func newReqFixture(t *testing.T) *reqFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	for _, slug := range []string{lockFin, lockLeg, openOps} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, lockFin, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, lockLeg, core.IsolationConfidential))
	p, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{lockFin, lockLeg}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	rec := events.NewRecorder(f.db)
	f.svc.cfg.Events = rec
	f.svc.cfg.LeaseCap = 8 * time.Hour
	return &reqFixture{fixture: f, policy: p, rec: rec}
}

// view is the owner's socket view for a chain, with the leases the store
// holds for it (what peerHandler builds).
func (f *reqFixture) view(t *testing.T, chain ...access.ProcessToken) access.View {
	t.Helper()
	v, ok := f.policy.PeerChainView(501, chain)
	require.True(t, ok)
	keys := make([]string, len(chain))
	for i, c := range chain {
		keys[i] = c.Key()
	}
	rows, err := store.ActiveAccessLeasesFor(context.Background(), f.db, keys, f.now)
	require.NoError(t, err)
	return v.WithLeases(f.now, store.AccessLeases(rows))
}

func (f *reqFixture) session(t *testing.T, name, cwd string, proc access.ProcessToken) string {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, f.db, core.Session{
		ID: id, Name: name, ProjectSlug: openOps, Status: core.SessionActive, Ambient: true,
		ExternalSessionID: name + "-x", ExternalClient: "claude-code", CWD: cwd, Model: "example-model",
		CreatedAt: f.now, UpdatedAt: f.now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, f.db, id, proc.AgentID()))
	return id
}

func (f *reqFixture) request(t *testing.T, project, sessionID string, chain ...access.ProcessToken) store.AccessApproval {
	t.Helper()
	a, err := f.svc.Request(context.Background(), RequestInput{Project: project, View: f.view(t, chain...), SessionID: sessionID, Reason: "reconcile the ledger notes"})
	require.NoError(t, err)
	return a
}

func (f *reqFixture) approveDaemon(t *testing.T, id, duration string) store.AccessLease {
	t.Helper()
	facts, err := f.svc.Facts(context.Background(), id)
	require.NoError(t, err)
	lease, err := f.svc.Approve(context.Background(), Decision{ID: id, Duration: duration, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.NoError(t, err)
	return lease
}

func TestRequest_AnchorIsTheSessionsAgentInTheChain(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)

	a := f.request(t, lockFin, sess, bridge, agent, shell)
	require.Equal(t, agent.Key(), a.ProcessKey, "the lease will name the agent, not the bridge that asked")
	require.Equal(t, agent.PID, a.PeerPID)
	require.Equal(t, store.AccessKindRequest, a.Kind)
	require.Equal(t, "cc/fin", a.SessionName)
	require.Equal(t, "reconcile the ledger notes", a.Reason)

	again := f.request(t, lockFin, sess, bridge, agent, shell)
	require.Equal(t, a.ID, again.ID, "a repeated refusal finds the pending request")

	// No session: the claim names the anchor, if it is in the chain.
	got, err := f.svc.Request(ctx, RequestInput{Project: lockLeg, View: f.view(t, bridge, agent, shell), AgentProcess: agent.AgentID()})
	require.NoError(t, err)
	require.Equal(t, agent.Key(), got.ProcessKey)
	_, err = f.svc.Request(ctx, RequestInput{Project: lockLeg, View: f.view(t, otherBr, other, shell), AgentProcess: agent.AgentID()})
	require.ErrorIs(t, err, ErrAnchorNotInChain, "a claim outside the chain is refused")
	_, err = f.svc.Request(ctx, RequestInput{Project: lockLeg, View: f.view(t, bridge, agent, shell)})
	require.ErrorIs(t, err, ErrNoAnchor)
	_, err = f.svc.Request(ctx, RequestInput{Project: openOps, View: f.view(t, bridge, agent, shell), AgentProcess: agent.AgentID()})
	require.ErrorIs(t, err, ErrNotLocked)
	tcp, _ := f.policy.PeerView(501)
	_, err = f.svc.Request(ctx, RequestInput{Project: lockFin, View: f.policy.OwnerView(), AgentProcess: agent.AgentID()})
	require.ErrorIs(t, err, ErrNoProcess, "the TCP key has no process to approve")
	_, err = f.svc.Request(ctx, RequestInput{Project: lockFin, View: tcp, AgentProcess: agent.AgentID()})
	require.ErrorIs(t, err, ErrNoProcess, "nor a socket view with no chain")

	long := make([]rune, store.MaxAccessReasonRunes+1)
	for i := range long {
		long[i] = 'x'
	}
	_, err = f.svc.Request(ctx, RequestInput{Project: lockFin, View: f.view(t, otherBr, other, shell), AgentProcess: other.AgentID(),
		Reason: string(long)})
	require.ErrorIs(t, err, store.ErrInvalidAccessRequest, "the reason is capped at 200 characters")
}

func TestApprove_DaemonPath_TheTreeReadsAndWrites(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)

	require.True(t, f.view(t, bridge, agent, shell).Locked(lockFin))
	lease := f.approveDaemon(t, a.ID, ForSession)
	require.Equal(t, f.now.Add(8*time.Hour), lease.ExpiresAt, "until the session ends: the cap, revoked at session end")
	require.Equal(t, agent.Key(), lease.ProcessKey)

	for name, chain := range map[string][]access.ProcessToken{
		"the bridge":                 {bridge, agent, shell},
		"a script's CLI call":        {{PID: 320, Version: 3200, Boot: boot}, {PID: 310, Version: 3100, Boot: boot}, agent, shell},
		"the agent itself, directly": {agent, shell},
	} {
		v := f.view(t, chain...)
		require.True(t, v.Visible(lockFin), name)
		require.True(t, v.Writable(lockFin), name)
		require.True(t, v.Locked(lockLeg), "%s: the lease opens only its project", name)
	}
	require.True(t, f.view(t, otherBr, other, shell).Locked(lockFin), "another tree stays locked")
	require.True(t, f.policy.OwnerView().WithLeases(f.now, []access.Lease{lease.Lease()}).Locked(lockFin), "TCP is refused")

	// A request from the leased tree is a refresh, carrying the old reason.
	refresh, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess, Reason: "new text"})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindRefresh, refresh.Kind)
	require.Equal(t, "reconcile the ledger notes", refresh.Reason, "a refresh carries no new free text")

	// session_end revokes.
	n, err := f.svc.RevokeSession(ctx, sess)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.True(t, f.view(t, bridge, agent, shell).Locked(lockFin))
}

func TestApprove_DurationsAreCapped(t *testing.T) {
	f := newReqFixture(t)
	f.svc.cfg.LeaseCap = 2 * time.Hour
	for choice, want := range map[string]time.Duration{ForSession: 2 * time.Hour, For1h: time.Hour, For4h: 2 * time.Hour} {
		got, err := f.svc.LeaseLength(choice)
		require.NoError(t, err)
		require.Equal(t, want, got, choice)
	}
	_, err := f.svc.LeaseLength("forever")
	require.ErrorIs(t, err, ErrDuration)
}

func TestApprove_Refusals(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)

	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest})
	require.ErrorIs(t, err, ErrNoProof, "an acceptance without a proof grants nothing")
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: "0000", Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrFactsChanged)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: "8d", Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrDuration)

	// The facts change when the same tree asks for another project too.
	f.request(t, lockLeg, sess, bridge, agent, shell)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrFactsChanged, "a new warning since the owner looked")
	fresh, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Contains(t, warningCodes(fresh), WarnOtherPending)

	f.approveDaemon(t, a.ID, For1h)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: fresh.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, store.ErrApprovalNotPending, "single use")

	// Expired.
	b := f.request(t, lockFin, f.session(t, "cc/b", "/work/ops", other), otherBr, other, shell)
	bf, err := f.svc.Facts(ctx, b.ID)
	require.NoError(t, err)
	f.now = f.now.Add(RequestTTL)
	_, err = f.svc.Approve(ctx, Decision{ID: b.ID, Duration: For1h, Digest: bf.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.Error(t, err)

	// Deny leaves the project locked.
	f.now = t0.Add(time.Hour)
	c := f.request(t, lockLeg, f.session(t, "cc/c", "/work/ops", other), otherBr, other, shell)
	require.NoError(t, f.svc.Deny(ctx, c.ID, "console"))
	require.True(t, f.view(t, otherBr, other, shell).Locked(lockLeg))
	cf, err := f.svc.Facts(ctx, c.ID)
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, Decision{ID: c.ID, Duration: For1h, Digest: cf.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, store.ErrApprovalNotPending)
}

func warningCodes(f Facts) []string {
	var out []string
	for _, w := range f.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func TestFacts_Warnings(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	f.svc.cfg.Host = "laptop"
	require.NoError(t, store.AddRepoMapping(ctx, f.db, "/work/finance", lockFin))
	sess := f.session(t, "cc/fin", "/work/elsewhere", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "confidential", facts.Isolation)
	require.Equal(t, "read-write", facts.Access)
	require.Equal(t, "R-"+a.ID[len(a.ID)-4:], facts.Short)
	require.Equal(t, "reconcile the ledger notes", facts.Reason)
	require.NotEmpty(t, facts.Digest)

	_, err = store.SetSessionTaint(ctx, f.db, sess, "web", "tool", f.now)
	require.NoError(t, err)
	tainted, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Contains(t, warningCodes(tainted), WarnTainted)
	require.NotEqual(t, facts.Digest, tainted.Digest, "a warning is part of the digest")

	got, err := f.svc.Resolve(ctx, facts.Short)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)
	got, err = f.svc.Resolve(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)
	_, err = f.svc.Resolve(ctx, "R-ZZZZ")
	require.ErrorIs(t, err, store.ErrApprovalNotFound)
}

func TestApprove_PasskeyPath(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	cred := f.enrollWithCode(t, key, "security key")
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	b := f.request(t, lockLeg, f.session(t, "cc/b", "/work/ops", other), otherBr, other, shell)
	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)

	opts, err := f.svc.AssertionOptions(ctx, a.ID)
	require.NoError(t, err)
	assertion, err := key.Get(opts, origin)
	require.NoError(t, err)

	// Another request's proof does not approve this one.
	bf, err := f.svc.Facts(ctx, b.ID)
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, Decision{ID: b.ID, Duration: For1h, Digest: bf.Digest, Proof: Proof{Kind: ProofPasskey, Assertion: assertion}})
	require.Error(t, err)

	lease, err := f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest, Proof: Proof{Kind: ProofPasskey, Assertion: assertion}})
	require.NoError(t, err)
	require.Equal(t, f.now.Add(4*time.Hour), lease.ExpiresAt)
	approval, err := store.AccessApprovalByID(ctx, f.db, a.ID)
	require.NoError(t, err)
	require.Equal(t, cred.ID, approval.DecidedBy)

	// A replayed proof: the request is no longer pending.
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest, Proof: Proof{Kind: ProofPasskey, Assertion: assertion}})
	require.ErrorIs(t, err, store.ErrApprovalNotPending)
	require.True(t, f.view(t, bridge, agent, shell).Visible(lockFin))
}

func TestApprove_TouchIDPath(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	sign := func(msg []byte) []byte {
		d := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
		require.NoError(t, err)
		return sig
	}
	code := f.code(t, store.ApproverTouchID)
	cred, err := f.svc.EnrollTouchID(ctx, "this Mac", code, pub, sign(TouchIDEnrollMessage(code, pub)))
	require.NoError(t, err)

	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	msg := TouchIDApproveMessage(a.ID, a.Challenge, facts.Digest, facts.Access, For1h)

	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For4h, Digest: facts.Digest,
		Proof: Proof{Kind: ProofTouchID, CredentialID: cred.ID, Signature: sign(msg)}})
	require.ErrorIs(t, err, ErrTouchIDKey, "a changed duration breaks the signature")
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	d := sha256.Sum256(msg)
	wrong, err := ecdsa.SignASN1(rand.Reader, other, d[:])
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest,
		Proof: Proof{Kind: ProofTouchID, CredentialID: cred.ID, Signature: wrong}})
	require.ErrorIs(t, err, ErrTouchIDKey, "a wrong key")
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest,
		Proof: Proof{Kind: ProofTouchID, CredentialID: "nope", Signature: sign(msg)}})
	require.ErrorIs(t, err, ErrTouchIDKey)

	lease, err := f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest,
		Proof: Proof{Kind: ProofTouchID, CredentialID: cred.ID, Signature: sign(msg)}})
	require.NoError(t, err)
	require.Equal(t, f.now.Add(time.Hour), lease.ExpiresAt)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest,
		Proof: Proof{Kind: ProofTouchID, CredentialID: cred.ID, Signature: sign(msg)}})
	require.ErrorIs(t, err, store.ErrApprovalNotPending, "a replay")
}

func TestEvents_CodesAndIdsOnly(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	a := f.request(t, lockFin, sess, bridge, agent, shell)
	f.approveDaemon(t, a.ID, ForSession)
	_, err := f.svc.RevokeSession(ctx, sess)
	require.NoError(t, err)
	rows, err := f.db.QueryContext(ctx, `SELECT kind, payload FROM events WHERE kind LIKE 'access.%' ORDER BY rowid`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var kinds []string
	for rows.Next() {
		var kind, payload string
		require.NoError(t, rows.Scan(&kind, &payload))
		kinds = append(kinds, kind)
		require.NotContains(t, payload, "ledger", "the agent's reason never enters an event")
		require.NotContains(t, payload, "/work", "nor a path")
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"access.requested", "access.approved", "access.revoked"}, kinds)
}
