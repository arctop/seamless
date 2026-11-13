package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// Moves and read-and-write grants (isolation.requests, plan fenced-sessions):
// the kind each ask files, the carry its facts state, the access the owner
// chooses, and what approving one records -- per tier.

const (
	confDataroom = "example-dataroom" // confidential
	confPayroll  = "example-payroll"  // confidential, never locked
	sealedS2     = "example-sealed"   // sealed
	openDocs     = "example-docs"     // open
)

// newKindFixture is newGrantFixture with a few more projects: a data room and
// a payroll project (confidential), a second sealed one and a second open one.
func newKindFixture(t *testing.T) *reqFixture {
	t.Helper()
	f := newGrantFixture(t)
	ctx := context.Background()
	for slug, state := range map[string]core.Isolation{confDataroom: core.IsolationConfidential,
		confPayroll: core.IsolationConfidential, sealedS2: core.IsolationSealed, openDocs: core.IsolationOpen} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, f.db, slug, state))
	}
	return f
}

// conversation opens a conversation's ambient row in project: the client's
// session id ext, stamped with proc as the SessionStart hook does.
func (f *reqFixture) conversation(t *testing.T, project, ext string, proc access.ProcessToken) string {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, f.db, core.Session{
		ID: id, Name: "cc/" + ext, ProjectSlug: project, Status: core.SessionActive, Ambient: true,
		ExternalSessionID: ext, ExternalClient: "claude-code", CWD: "/work/" + project, Model: "example-model",
		CreatedAt: f.now, UpdatedAt: f.now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, f.db, id, proc.AgentID()))
	return id
}

// link opens an explicit session in project linked to conversation ext, as
// session_start does.
func (f *reqFixture) link(t *testing.T, project, ext string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(context.Background(), f.db, core.Session{
		ID: id, Name: "linked-" + id, ProjectSlug: project, Status: core.SessionActive,
		ExternalSessionID: ext, ExternalClient: "claude-code", CreatedAt: f.now, UpdatedAt: f.now,
	}))
	return id
}

func convHolder(t *testing.T, ext string) store.Holder {
	t.Helper()
	h, err := store.HolderOfConversation("claude-code", ext)
	require.NoError(t, err)
	return h
}

func TestKindFor_FollowsHomeAndTarget(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	for _, tc := range []struct {
		home, target, want string
		err                error
	}{
		{openOps, confHR, store.AccessKindMove, nil},
		{"", confHR, store.AccessKindMove, nil},
		{confDataroom, confHR, store.AccessKindGrant, nil},
		{openOps, sealedV, store.AccessKindMove, nil},
		{confHR, sealedV, store.AccessKindMove, nil},
		{sealedS2, sealedV, store.AccessKindMove, nil},
		{"", sealedV, store.AccessKindMove, nil},
		{sealedV, confHR, "", ErrSealed},
		{confHR, openDocs, "", ErrNotConfidential},
		{openOps, "example-none", "", ErrNotConfidential},
		{confHR, confHR, "", ErrIsHome},
	} {
		got, err := g.KindFor(ctx, tc.home, tc.target)
		if tc.err != nil {
			require.ErrorIs(t, err, tc.err, "%s -> %s", tc.home, tc.target)
			continue
		}
		require.NoError(t, err, "%s -> %s", tc.home, tc.target)
		require.Equal(t, tc.want, got, "%s -> %s", tc.home, tc.target)
	}
}

// Without peer_auth: an open home's conversation asks to read a confidential
// project, and its ask is a move -- held by the conversation, stating its
// carry and the move line; asked again it is found; denied, a refusal files
// nothing and names the denial; approved, the conversation works in the
// target from now on.
func TestAsk_MoveFromAnOpenHome(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, openOps, "ext-1", agent)
	linked := f.link(t, openDocs, "ext-1")

	// Asked from a session_start link elsewhere: the home is still the
	// conversation's ambient row.
	a, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: linked, Via: ViaRefusal}})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, a.Kind)
	require.Equal(t, openOps, a.FromProject)
	require.Equal(t, home, a.SessionID, "the session a move leaves is the home's")
	require.Equal(t, convHolder(t, "ext-1"), a.Holder())
	require.Equal(t, store.Carry{Home: openOps, Holds: []string{confHR}, WriteTo: []string{confHR}}, a.Carry)

	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, facts.Kind)
	require.Equal(t, "read-write", facts.Access)
	require.Equal(t, openOps, facts.From)
	require.Equal(t, &CarryFacts{Home: openOps, Holds: []string{confHR}, WriteTo: []string{confHR}}, facts.Carry)
	require.Empty(t, facts.AccessChoices)
	require.False(t, facts.Locked)
	require.Equal(t, Warning{Code: WarnMove, Text: "This agent has worked in example-ops. If you approve, it works in example-hr " +
		"from now on, and what it saw may be written into example-hr. It can no longer write to example-ops or the global " +
		"scope until the conversation ends."}, facts.Warnings[0])

	// One waiting request per asker and project; an explicit ask fills in
	// the reason a refusal's request lacks.
	var asked Asked
	again, err := g.Ask(WithAsked(ctx, &asked), AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home,
		Reason: "read the payroll calendar", Via: ViaRequest}})
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID)
	require.Equal(t, "read the payroll calendar", again.Reason)
	filed, reason := asked.Outcome(again)
	require.False(t, filed)
	require.Equal(t, ReasonAdded, reason)

	// Denied: a refusal files nothing and names the denial; the agent's
	// explicit ask files anew.
	require.NoError(t, g.Deny(ctx, a.ID, "console"))
	_, err = g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home, Via: ViaRefusal}})
	var denied *DeniedError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "move request", denied.What())
	b, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home, Via: ViaRequest}})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID)

	// Approved with the bearer key (the advisory tier): the move, the child.
	bf, err := g.Facts(ctx, b.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: b.ID, Duration: ForSession, Digest: bf.Digest, Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrNotAGrant, "a move is not a grant's to decide")
	_, err = g.ApproveMove(ctx, Decision{ID: b.ID, Duration: ForSession, Digest: bf.Digest, Proof: Proof{Kind: ProofKey}, Access: "read-only"})
	require.ErrorIs(t, err, ErrAccess, "a move offers no access choice")
	done, err := g.ApproveMove(ctx, Decision{ID: b.ID, Duration: ForSession, Digest: bf.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)
	require.Nil(t, done.Lease)
	require.Equal(t, confHR, done.Child.ProjectSlug)
	require.Equal(t, home, done.Child.ParentSessionID)
	require.Equal(t, home, done.Left.ID)
	row, err := store.AccessApprovalByID(ctx, f.db, b.ID)
	require.NoError(t, err)
	require.Equal(t, "bearer-key", row.DecidedBy)
	evs := f.eventsOf(t, core.EventAccessApproved)
	require.Len(t, evs, 1)
	require.Equal(t, store.AccessKindMove, evs[0].Payload["kind"])
	require.Equal(t, done.Child.ID, evs[0].SessionID)

	// Its home is the target now: there is nothing to ask for there.
	_, err = g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: done.Child.ID}})
	require.ErrorIs(t, err, ErrIsHome)
	// From its confidential home, another confidential project is a grant.
	c, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confPayroll, SessionID: done.Child.ID}})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindGrant, c.Kind)
	require.Equal(t, confHR, c.FromProject)
}

// Any home moves into a sealed project, and the line names what the move
// carries in, which the seal otherwise keeps out.
func TestAsk_MoveIntoASealedProjectNamesTheCarryIn(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, confDataroom, "ext-2", agent)
	a, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: sealedV, SessionID: home}})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, a.Kind)
	require.Equal(t, store.Carry{Home: confDataroom, Holds: []string{confDataroom, sealedV}, WriteTo: []string{sealedV}}, a.Carry)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "This agent has worked in example-dataroom. If you approve, it works in example-vault from now on, and "+
		"what it saw may be written into example-vault. example-vault is sealed, so nothing enters it otherwise: approving "+
		"lets in what this agent carries from example-dataroom. It can no longer write to example-dataroom or the global "+
		"scope until the conversation ends.", facts.Warnings[0].Text)
}

// A move belongs to a conversation or a process, never a session alone: a
// client that sends no identity cannot move, though it still reads.
func TestAsk_ASessionAloneCannotMove(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	plain := f.sessionIn(t, openOps, "cc/plain")
	_, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: plain}})
	require.ErrorIs(t, err, ErrNoConversation)
	a, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: plain})
	require.NoError(t, err, "a read grant still files, as before")
	require.Equal(t, store.HolderSession, a.Holder().Kind())
	require.True(t, a.Carry.IsZero(), "an open project's read grant states no carry, as before")
}

// Between confidential projects the ask is a grant, which may ask for read
// and write. The owner chooses: read-only by default, or read and write; the
// facts say what each lets happen, and approving records the flows the
// chosen access lists.
func TestAsk_ReadAndWriteGrantBetweenConfidentialProjects(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, confHR, "ext-3", agent)

	a, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confPayroll, SessionID: home}, Write: true})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindGrant, a.Kind)
	require.Equal(t, store.GrantAccessWrite, a.RequestedAccess)
	require.Equal(t, store.Carry{Home: confHR, Holds: []string{confHR, confPayroll}, WriteTo: []string{confHR, confPayroll}}, a.Carry)

	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "read-only", facts.Access, "read-only is the default whatever was asked")
	require.Equal(t, []string{"read-only", "read-write"}, facts.AccessChoices)
	require.Equal(t, "read-write", facts.AskedAccess)
	require.Equal(t, &CarryFacts{Home: confHR, Holds: []string{confHR, confPayroll}, WriteTo: []string{confHR}}, facts.Carry,
		"the facts state the default approval")
	require.Contains(t, facts.Warnings, Warning{Code: WarnCopyInto, Text: "content from example-payroll may be copied into example-hr"})
	require.Equal(t, "content from example-payroll may be copied into example-hr, and it may change example-payroll's "+
		"tasks, memories and notes; content from example-hr may be copied into example-payroll", facts.ReadWrite)

	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}, Access: "write"})
	require.ErrorIs(t, err, ErrAccess)
	grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}, Access: "read-write"})
	require.NoError(t, err)
	require.Equal(t, store.GrantAccessWrite, grant.Access)
	require.Equal(t, convHolder(t, "ext-3"), grant.Holder())
	flows, err := store.FlowsForHolder(ctx, f.db, []store.Holder{convHolder(t, "ext-3")})
	require.NoError(t, err)
	require.ElementsMatch(t, []store.FlowPair{{From: confHR, To: confPayroll}, {From: confPayroll, To: confHR}}, flowPairs(flows))
	granted := f.eventsOf(t, core.EventAccessGranted)
	require.Equal(t, store.GrantAccessWrite, granted[0].Payload["access"])

	// Asked again for write while the write grant is live: answered with it.
	_, err = g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confPayroll, SessionID: home}, Write: true})
	var live *AccessLiveError
	require.ErrorAs(t, err, &live)

	// Read-only, the default, records only what read-only lists.
	second := f.conversation(t, confHR, "ext-4", other)
	b, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confPayroll, SessionID: second}, Write: true})
	require.NoError(t, err)
	bf, err := g.Facts(ctx, b.ID)
	require.NoError(t, err)
	read, err := g.Approve(ctx, Decision{ID: b.ID, Duration: For1h, Digest: bf.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)
	require.Equal(t, store.GrantAccessRead, read.Access)
	flows, err = store.FlowsForHolder(ctx, f.db, []store.Holder{convHolder(t, "ext-4")})
	require.NoError(t, err)
	require.Equal(t, []store.FlowPair{{From: confPayroll, To: confHR}}, flowPairs(flows))

	// A denied read-and-write request names itself so.
	c, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confDataroom, SessionID: second}, Write: true})
	require.NoError(t, err)
	require.NoError(t, g.Deny(ctx, c.ID, "console"))
	_, err = g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confDataroom, SessionID: second, Via: ViaRefusal}, Write: true})
	var denied *DeniedError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "read-and-write request", denied.What())
}

func flowPairs(flows []store.Flow) []store.FlowPair {
	out := make([]store.FlowPair, len(flows))
	for i, f := range flows {
		out[i] = store.FlowPair{From: f.From, To: f.To}
	}
	return out
}

// The owner's example: a conversation homed in a data room, holding a
// read-and-write grant into finance, asks to read legal. The approval says
// legal's content may end up in the data room and in finance, and approving
// it clears exactly those flows.
func TestAsk_TheWholeCarry(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, confDataroom, "ext-5", agent)
	w, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: lockFin, SessionID: home}, Write: true})
	require.NoError(t, err)
	wf, err := g.Facts(ctx, w.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: w.ID, Duration: For4h, Digest: wf.Digest, Proof: Proof{Kind: ProofKey}, Access: "read-write"})
	require.NoError(t, err)

	r, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: lockLeg, SessionID: home}})
	require.NoError(t, err)
	require.Equal(t, store.Carry{Home: confDataroom, Holds: []string{confDataroom, lockFin, lockLeg},
		WriteTo: []string{confDataroom, lockFin}}, r.Carry)
	rf, err := g.Facts(ctx, r.ID)
	require.NoError(t, err)
	require.Contains(t, rf.Warnings, Warning{Code: WarnCopyInto,
		Text: "content from example-legal may be copied into example-dataroom and example-finance"})
	require.Equal(t, "content from example-legal may be copied into example-dataroom and example-finance, and it may change "+
		"example-legal's tasks, memories and notes; content from example-dataroom and example-finance may be copied into "+
		"example-legal", rf.ReadWrite)
	_, err = g.Approve(ctx, Decision{ID: r.ID, Duration: For1h, Digest: rf.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)
	flows, err := store.FlowsForHolder(ctx, f.db, []store.Holder{convHolder(t, "ext-5")})
	require.NoError(t, err)
	var fromLegal []store.FlowPair
	for _, p := range flowPairs(flows) {
		if p.From == lockLeg {
			fromLegal = append(fromLegal, p)
		}
		require.NotEqual(t, lockLeg, p.To, "a read-only approval clears nothing into legal")
	}
	require.ElementsMatch(t, []store.FlowPair{{From: lockLeg, To: confDataroom}, {From: lockLeg, To: lockFin}}, fromLegal)
}

// RequestGrant keeps its meaning: a read grant from the asking session's own
// project. From a confidential project it now states its carry, so the owner
// sees the whole of it and may choose read and write; the conversation holds
// it.
func TestRequestGrant_FromAConfidentialHomeStatesItsCarry(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, confHR, "ext-6", agent)
	a, err := g.RequestGrant(ctx, GrantInput{Project: confPayroll, SessionID: home})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindGrant, a.Kind)
	require.Equal(t, store.GrantAccessRead, a.RequestedAccess)
	require.Equal(t, convHolder(t, "ext-6"), a.Holder())
	require.Equal(t, store.Carry{Home: confHR, Holds: []string{confHR, confPayroll}, WriteTo: []string{confHR}}, a.Carry)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"read-only", "read-write"}, facts.AccessChoices)
	require.Equal(t, "read-only", facts.AskedAccess)

	// From an open project: the legacy read grant, its line word for word.
	open := f.conversation(t, openOps, "ext-7", other)
	b, err := g.RequestGrant(ctx, GrantInput{Project: confPayroll, SessionID: open})
	require.NoError(t, err)
	require.True(t, b.Carry.IsZero())
	bf, err := g.Facts(ctx, b.ID)
	require.NoError(t, err)
	require.Nil(t, bf.Carry)
	require.Empty(t, bf.AccessChoices)
	require.Contains(t, bf.Warnings, Warning{Code: WarnCopyInto, Text: "content from example-payroll may be copied into example-ops's memories"})
}

// The end of a conversation's own (ambient) row is the conversation's: every
// grant it holds ends, whichever of its sessions asked. Its flows and its
// label stay. A move ends the row it leaves without that.
func TestGrantsRevokeSession_EndsTheConversationsGrants(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, confHR, "ext-8", agent)
	linked := f.link(t, confHR, "ext-8")
	a, err := g.RequestGrant(ctx, GrantInput{Project: confPayroll, SessionID: linked})
	require.NoError(t, err)
	require.Equal(t, linked, a.SessionID)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}})
	require.NoError(t, err)

	n, err := g.RevokeSession(ctx, home)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the ambient row's end ends the conversation's grant")
	live, err := g.Live(ctx)
	require.NoError(t, err)
	require.Empty(t, live)
	flows, err := store.HolderFlows(ctx, f.db, []store.Holder{convHolder(t, "ext-8")})
	require.NoError(t, err)
	require.NotEmpty(t, flows, "flows stay")
	label, err := store.HolderLabel(ctx, f.db, []store.Holder{convHolder(t, "ext-8")}, confHR)
	require.NoError(t, err)
	require.Len(t, label, 2, "the label stays")
}

// Under peer_auth: a move into a locked project carries its lease, in one
// approval, bound to the child; Touch ID approves no move; the lease core
// approves none either.
func TestApproveMove_PeerAuthLockedTargetCarriesItsLease(t *testing.T) {
	f := newReqFixture(t)
	f.svc.cfg.ForceLocked = []string{lockFin, lockLeg}
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	ctx := context.Background()
	sess := f.session(t, "cc/ops", "/work/ops", agent)
	vctx := access.WithView(ctx, f.view(t, bridge, agent, shell))

	_, err := g.RequestGrant(vctx, GrantInput{Project: lockFin, SessionID: sess})
	require.ErrorIs(t, err, access.ErrNotReachable, "a read grant never opens a lock")
	a, err := g.Ask(vctx, AskInput{GrantInput: GrantInput{Project: lockFin, SessionID: sess}})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, a.Kind)
	require.Equal(t, agent.Key(), a.ProcessKey)
	require.Equal(t, store.HolderProcess, a.Holder().Kind())
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, facts.Locked)
	require.Contains(t, facts.Warnings[0].Text, "until the agent process exits")

	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrIsMove, "a lease is only a move's lock half")
	_, err = g.ApproveMove(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofKey}})
	require.ErrorIs(t, err, ErrPeerProof)
	_, err = g.ApproveMove(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest,
		Proof: Proof{Kind: ProofTouchID, CredentialID: "x", Signature: []byte{1}}})
	require.ErrorIs(t, err, ErrTouchIDMove)
	require.False(t, f.view(t, bridge, agent, shell).Visible(lockFin))

	done, err := g.ApproveMove(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.NoError(t, err)
	require.NotNil(t, done.Lease)
	require.Equal(t, done.Child.ID, done.Lease.SessionID)
	require.Equal(t, f.now.Add(time.Hour), done.Lease.ExpiresAt)
	require.Equal(t, lockFin, done.Child.ProjectSlug)
	require.True(t, f.view(t, bridge, agent, shell).Visible(lockFin), "the move's lease opens the lock to the tree")

	// A move into a project no lock covers carries none.
	sess2 := f.session(t, "cc/ops2", "/work/ops", other)
	_, err = store.EnsureProject(ctx, f.db, confHR, confHR)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, confHR, core.IsolationConfidential))
	b, err := g.Ask(access.WithView(ctx, f.view(t, otherBr, other, shell)), AskInput{GrantInput: GrantInput{Project: confHR, SessionID: sess2}})
	require.NoError(t, err)
	bf, err := g.Facts(ctx, b.ID)
	require.NoError(t, err)
	require.False(t, bf.Locked)
	moved, err := g.ApproveMove(ctx, Decision{ID: b.ID, Duration: ForSession, Digest: bf.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.NoError(t, err)
	require.Nil(t, moved.Lease)
}

// The facts digest covers the carry and the access offered, and a passkey or
// Touch ID proof signs the access chosen: a proof over read-only never
// approves read and write, and a carry that changed is new facts.
func TestFacts_DigestBindsTheCarryAndTheAccess(t *testing.T) {
	setup := func(t *testing.T) (*reqFixture, *Grants, store.AccessApproval, Facts) {
		f := newReqFixture(t)
		ctx := context.Background()
		for _, slug := range []string{confHR, confPayroll} {
			_, err := store.EnsureProject(ctx, f.db, slug, slug)
			require.NoError(t, err)
			require.NoError(t, store.SetProjectIsolation(ctx, f.db, slug, core.IsolationConfidential))
		}
		g := NewGrants(GrantsConfig{Approvals: f.svc})
		sess := f.session(t, "cc/hr", "/work/hr", agent)
		_, err := f.db.ExecContext(ctx, `UPDATE sessions SET project_slug = ? WHERE id = ?`, confHR, sess)
		require.NoError(t, err)
		a, err := g.Ask(access.WithView(ctx, f.view(t, bridge, agent, shell)),
			AskInput{GrantInput: GrantInput{Project: confPayroll, SessionID: sess}, Write: true})
		require.NoError(t, err)
		facts, err := g.Facts(ctx, a.ID)
		require.NoError(t, err)
		return f, g, a, facts
	}

	t.Run("a changed carry is new facts", func(t *testing.T) {
		f, g, a, facts := setup(t)
		ctx := context.Background()
		_, err := f.db.ExecContext(ctx, `UPDATE access_approvals SET carry = ? WHERE id = ?`,
			`{"home":"example-hr","holds":["example-hr","example-payroll","example-legal"],"write_to":["example-hr","example-payroll"]}`, a.ID)
		require.NoError(t, err)
		now, err := g.Facts(ctx, a.ID)
		require.NoError(t, err)
		require.NotEqual(t, facts.Digest, now.Digest)
		_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
		require.ErrorIs(t, err, ErrFactsChanged)
	})

	t.Run("a passkey signs the access", func(t *testing.T) {
		f, g, a, facts := setup(t)
		ctx := context.Background()
		key, err := passkeytest.New(true)
		require.NoError(t, err)
		f.enrollWithCode(t, key, "security key")
		_, err = f.svc.AssertionOptionsForAccess(ctx, a.ID, For1h, "write", facts.Digest)
		require.ErrorIs(t, err, ErrAccess)
		opts, err := f.svc.AssertionOptionsForAccess(ctx, a.ID, For1h, "", facts.Digest)
		require.NoError(t, err)
		assertion, err := key.Get(opts, origin)
		require.NoError(t, err)
		_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Access: "read-write",
			Proof: Proof{Kind: ProofPasskey, Assertion: assertion}})
		require.ErrorIs(t, err, ErrFactsChanged, "signed read-only, sent as read and write")
		require.ErrorIs(t, err, ErrBindingChanged)
		grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest,
			Proof: Proof{Kind: ProofPasskey, Assertion: assertion}})
		require.NoError(t, err)
		require.Equal(t, store.GrantAccessRead, grant.Access)
	})

	t.Run("Touch ID signs the access", func(t *testing.T) {
		f, g, a, facts := setup(t)
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
		code := f.code(t, store.ApproverTouchID, pub)
		cred, err := f.svc.EnrollTouchID(ctx, "this Mac", code, pub, sign(TouchIDEnrollMessage(code, pub)))
		require.NoError(t, err)
		// What the helper signs today: the facts' own access, read-only.
		readOnly := sign(TouchIDApproveMessage(a.ID, a.Challenge, facts.Digest, facts.Access, For1h))
		_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Access: "read-write",
			Proof: Proof{Kind: ProofTouchID, CredentialID: cred.ID, Signature: readOnly}})
		require.ErrorIs(t, err, ErrTouchIDKey, "a touch over read-only never approves read and write")
		readWrite := sign(TouchIDApproveMessage(a.ID, a.Challenge, facts.Digest, "read-write", For1h))
		grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Access: "read-write",
			Proof: Proof{Kind: ProofTouchID, CredentialID: cred.ID, Signature: readWrite}})
		require.NoError(t, err)
		require.Equal(t, store.GrantAccessWrite, grant.Access)
	})
}

// A lock's request offers its own access alone, and its facts read as they
// always did: no carry, no choices, no lock mark.
func TestFacts_LockRequestUnchanged(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	a := f.request(t, lockFin, f.session(t, "cc/fin", "/work/ops", agent), bridge, agent, shell)
	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, facts.Carry)
	require.Empty(t, facts.AccessChoices)
	require.Empty(t, facts.ReadWrite)
	require.False(t, facts.Locked)
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}, Access: "read-only"})
	require.ErrorIs(t, err, ErrAccess)
	lease, err := f.svc.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}, Access: "read-write"})
	require.NoError(t, err, "naming the facts' own access is the default")
	require.Equal(t, f.now.Add(time.Hour), lease.ExpiresAt)
}
