package approval

// Asking again (owner decisions 2026-11-08, task 01M6X9M1TYWXVKTDJ9BH1CK16P):
// an explicit request's reason fills in the reason a refusal's request lacks,
// and once the owner denies a request a refusal names the denial and files
// nothing, while the agent's explicit request -- what the owner asks it to
// send after correcting it -- always files a new one.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func (f *reqFixture) approvals(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM access_approvals`).Scan(&n))
	return n
}

func TestRequest_ExplicitReasonFillsARefusalsRequest(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	in := RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess, Via: ViaRefusal}
	a, err := f.svc.Request(ctx, in)
	require.NoError(t, err)
	require.Empty(t, a.Reason, "a refusal has no reason to give")
	shown, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)

	// A refusal never writes words into it.
	in.Reason = "from a refusal"
	again, err := f.svc.Request(ctx, in)
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID)
	require.Empty(t, again.Reason)

	// The agent asks properly: the same request, now with its reason.
	in.Via, in.Reason = ViaRequest, "  reconcile the ledger notes  "
	got, err := f.svc.Request(ctx, in)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID, "the pending request, not a second one")
	require.Equal(t, "reconcile the ledger notes", got.Reason)
	now, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "reconcile the ledger notes", now.Reason)
	require.NotEqual(t, shown.Digest, now.Digest, "the reason is one of the facts")
	require.Equal(t, 1, f.approvals(t))
	require.Len(t, f.eventsOf(t, core.EventAccessRequested), 1, "filled in, not filed again")

	// An owner deciding over the facts as they stood is asked to look again.
	_, err = f.svc.Approve(ctx, Decision{ID: a.ID, Duration: ForSession, Digest: shown.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrFactsChanged)

	// A reason, once there, is never replaced.
	in.Reason = "something else entirely"
	got, err = f.svc.Request(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "reconcile the ledger notes", got.Reason)
	f.approveDaemon(t, a.ID, For1h)
}

func TestRequest_ExplicitReasonTooLongIsRefused(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	in := RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess, Via: ViaRefusal}
	a, err := f.svc.Request(ctx, in)
	require.NoError(t, err)
	in.Via, in.Reason = ViaRequest, strings.Repeat("x", store.MaxAccessReasonRunes+1)
	_, err = f.svc.Request(ctx, in)
	require.ErrorIs(t, err, store.ErrInvalidAccessRequest)
	row, err := store.AccessApprovalByID(ctx, f.db, a.ID)
	require.NoError(t, err)
	require.Empty(t, row.Reason)
}

func TestRequest_ARefusalAfterADenialFilesNothing(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	refusal := RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess, Via: ViaRefusal}
	a, err := f.svc.Request(ctx, refusal)
	require.NoError(t, err)
	f.now = f.now.Add(2 * time.Minute)
	deniedAt := f.now
	require.NoError(t, f.svc.Deny(ctx, a.ID, "owner"))
	before := f.approvals(t)

	f.now = f.now.Add(time.Hour)
	_, err = f.svc.Request(ctx, refusal)
	require.ErrorIs(t, err, ErrDenied)
	var denied *DeniedError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, a.ID, denied.Request.ID)
	require.True(t, deniedAt.Equal(denied.At()), "%s", denied.At())
	require.Equal(t, "access request", denied.What())
	lr := denied.LockRequest()
	require.Equal(t, access.LockRequest{ID: a.ID, Short: ShortID(a.ID), Denied: true, DeniedAt: denied.At(),
		DeniedWhat: "access request"}, lr)
	require.Equal(t, before, f.approvals(t), "nothing new for the owner: no loop")

	// The denial is this agent's, for this project: another project, and
	// another agent, ask as before.
	_, err = f.svc.Request(ctx, RequestInput{Project: lockLeg, View: f.view(t, bridge, agent, shell), SessionID: sess, Via: ViaRefusal})
	require.NoError(t, err)
	_, err = f.svc.Request(ctx, RequestInput{Project: lockFin, View: f.view(t, otherBr, other, shell),
		SessionID: f.session(t, "cc/b", "/work/ops", other), Via: ViaRefusal})
	require.NoError(t, err)

	// The owner tells the agent to ask again: its explicit request files anew.
	asked, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess,
		Via: ViaRequest, Reason: "the ledger and its notes"})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, asked.ID)
	require.Equal(t, store.ApprovalPending, asked.Status)
	require.Equal(t, "the ledger and its notes", asked.Reason)
	// A refusal now names that one, pending.
	again, err := f.svc.Request(ctx, refusal)
	require.NoError(t, err)
	require.Equal(t, asked.ID, again.ID)

	// Only a denial holds a refusal back: one that lapsed undecided does not.
	f.now = asked.ExpiresAt.Add(time.Second)
	fresh, err := f.svc.Request(ctx, refusal)
	require.NoError(t, err)
	require.NotEqual(t, asked.ID, fresh.ID)
}

func TestRequestGrant_AskingAgain(t *testing.T) {
	f := newGrantFixture(t)
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	ctx := access.WithView(context.Background(), f.view(t, bridge, agent, shell))
	sess := f.session(t, "cc/ops", "/work/ops", agent)
	refusal := GrantInput{Project: confHR, SessionID: sess, Via: ViaRefusal}

	a, err := g.RequestGrant(ctx, refusal)
	require.NoError(t, err)
	require.Empty(t, a.Reason)
	filled, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: sess, Via: ViaRequest, Reason: "the hiring plan"})
	require.NoError(t, err)
	require.Equal(t, a.ID, filled.ID)
	require.Equal(t, "the hiring plan", filled.Reason)

	require.NoError(t, g.Deny(ctx, a.ID, "owner"))
	before := f.approvals(t)
	_, err = g.RequestGrant(ctx, refusal)
	var denied *DeniedError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, a.ID, denied.Request.ID)
	require.Equal(t, "read request", denied.What())
	require.Equal(t, before, f.approvals(t))

	asked, err := g.RequestGrant(ctx, GrantInput{Project: confHR, SessionID: sess, Via: ViaRequest, Reason: "the hiring plan, again"})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, asked.ID)
	require.Equal(t, store.AccessKindGrant, asked.Kind)
}

// What an ask did, for the answer to say so (access_request, seam access
// request): filed or found waiting, and what became of the reason. A
// refresh's reason is its own, like any request's (owner decision 2026-11-09).
func TestRequest_AskedSaysWhatTheAskDid(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	ask := func(project, reason string, view access.View, via string) (store.AccessApproval, Asked) {
		t.Helper()
		var asked Asked
		a, err := f.svc.Request(WithAsked(ctx, &asked), RequestInput{Project: project, View: view, SessionID: sess, Reason: reason, Via: via})
		require.NoError(t, err)
		return a, asked
	}
	v := f.view(t, bridge, agent, shell)

	// A new request, with a reason, then the same project asked again.
	a, asked := ask(lockFin, "  the ledger  ", v, ViaRequest)
	filed, reason := asked.Outcome(a)
	require.True(t, filed)
	require.Equal(t, ReasonAdded, reason)
	require.Equal(t, "the ledger", a.Reason)

	found, asked := ask(lockFin, "", v, ViaRequest)
	require.Equal(t, a.ID, found.ID)
	filed, reason = asked.Outcome(found)
	require.False(t, filed)
	require.Equal(t, ReasonNone, reason)

	found, asked = ask(lockFin, "another reason", v, ViaRequest)
	filed, reason = asked.Outcome(found)
	require.False(t, filed)
	require.Equal(t, ReasonKept, reason, "the waiting request keeps its own")
	require.Equal(t, "the ledger", found.Reason)

	// A new request with no reason.
	b, asked := ask(lockLeg, "", v, ViaRequest)
	filed, reason = asked.Outcome(b)
	require.True(t, filed)
	require.Equal(t, ReasonNone, reason)

	// A refusal's blank request takes the explicit reason when it is asked
	// for properly.
	sess2 := f.session(t, "cc/other", "/work/ops", other)
	vo := f.view(t, otherBr, other, shell)
	blank, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: vo, SessionID: sess2, Via: ViaRefusal})
	require.NoError(t, err)
	require.Empty(t, blank.Reason)
	var filled Asked
	got, err := f.svc.Request(WithAsked(ctx, &filled), RequestInput{Project: lockFin, View: vo, SessionID: sess2, Via: ViaRequest, Reason: "reconcile the quarter"})
	require.NoError(t, err)
	require.Equal(t, blank.ID, got.ID)
	require.Equal(t, "reconcile the quarter", got.Reason)
	filed, reason = filled.Outcome(got)
	require.False(t, filed)
	require.Equal(t, ReasonAdded, reason, "filled in: the words are this call's")

	// A context without an Asked changes nothing.
	again, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: v, SessionID: sess, Via: ViaRequest})
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID)
}

// A refresh is filed from the expiry warning on. It carries the reason it was
// given, labeled like any request's, or no reason at all: the earlier request's
// words are not reused (owner decision 2026-11-09). A later explicit reason
// finds it waiting; as of this change FillAccessReason leaves a refresh alone,
// so the answer truthfully says the reason was not stored (kept).
func TestRequest_RefreshCarriesItsOwnReason(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	sess := f.session(t, "cc/fin", "/work/ops", agent)
	v := f.view(t, bridge, agent, shell)
	a, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: v, SessionID: sess, Reason: "reconcile the ledger notes"})
	require.NoError(t, err)
	lease := f.approveDaemon(t, a.ID, For1h)

	f.now = lease.ExpiresAt.Add(-access.ExpiryWarning)
	var asked Asked
	refresh, err := f.svc.Request(WithAsked(ctx, &asked), RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess, Via: ViaRequest})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindRefresh, refresh.Kind)
	require.Empty(t, refresh.Reason, "given no reason, a refresh shows none: the old one is not reused")
	filed, reason := asked.Outcome(refresh)
	require.True(t, filed)
	require.Equal(t, ReasonNone, reason)
	shown, err := f.svc.Facts(ctx, refresh.ID)
	require.NoError(t, err)
	require.Empty(t, shown.Reason)

	// A later reason, given to the waiting refresh: it fills the blank, as on
	// any waiting request, and the answer says so.
	var later Asked
	got, err := f.svc.Request(WithAsked(ctx, &later), RequestInput{Project: lockFin, View: f.view(t, bridge, agent, shell), SessionID: sess, Via: ViaRequest, Reason: "still reconciling"})
	require.NoError(t, err)
	require.Equal(t, refresh.ID, got.ID)
	filed, reason = later.Outcome(got)
	require.False(t, filed)
	require.Equal(t, ReasonAdded, reason)
	require.Equal(t, "still reconciling", got.Reason)
}

func TestRequestGrant_AskedSaysWhatTheAskDid(t *testing.T) {
	f := newGrantFixture(t)
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	ctx := access.WithView(context.Background(), f.view(t, bridge, agent, shell))
	sess := f.session(t, "cc/ops", "/work/ops", agent)
	ask := func(reason string) (store.AccessApproval, Asked) {
		t.Helper()
		var asked Asked
		a, err := g.RequestGrant(WithAsked(ctx, &asked), GrantInput{Project: confHR, SessionID: sess, Via: ViaRequest, Reason: reason})
		require.NoError(t, err)
		return a, asked
	}

	a, asked := ask("the hiring plan")
	filed, reason := asked.Outcome(a)
	require.True(t, filed)
	require.Equal(t, ReasonAdded, reason)

	again, asked := ask("")
	require.Equal(t, a.ID, again.ID)
	filed, reason = asked.Outcome(again)
	require.False(t, filed)
	require.Equal(t, ReasonNone, reason)

	again, asked = ask("another reason")
	filed, reason = asked.Outcome(again)
	require.False(t, filed)
	require.Equal(t, ReasonKept, reason)
}
