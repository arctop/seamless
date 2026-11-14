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

// The in-session dialog's decision (plan fenced-sessions P9, owner decision
// D7): in the advisory tier Approve or Deny there decides a grant or move
// request, recorded as the dialog's; under peer_auth the dialog decides
// nothing, and the lock's approve core never took such a proof.

// advMoveDecided reads the move an approval recorded for request id.
func advMoveDecided(t *testing.T, f *reqFixture, id string) store.Move {
	t.Helper()
	var m store.Move
	require.NoError(t, f.db.QueryRowContext(context.Background(),
		`SELECT id, decided_by, to_project FROM access_moves WHERE approval_id = ?`, id).Scan(&m.ID, &m.DecidedBy, &m.ToProject))
	return m
}

func TestDialogDecision_AdvisoryApprovesAGrantAsTheDialogs(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, confHR, "ext-adv-1", agent)
	a, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confPayroll, SessionID: home}, Write: true})
	require.NoError(t, err)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)

	_, err = g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: "stale", Proof: Proof{Kind: ProofDialog}})
	require.ErrorIs(t, err, ErrFactsChanged, "the dialog's Approve is over the facts it showed")
	grant, err := g.Approve(ctx, Decision{ID: a.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDialog},
		Access: "read-write"})
	require.NoError(t, err)
	require.Equal(t, DecidedByDialog, grant.DecidedBy)
	require.Equal(t, "in-session-dialog", grant.DecidedBy, "the value the record keeps")
	require.Equal(t, store.GrantAccessWrite, grant.Access, "the access chosen in the dialog")
	require.Equal(t, f.now.Add(time.Hour), grant.ExpiresAt)
	row, err := store.AccessApprovalByID(ctx, f.db, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalApproved, row.Status)
	require.Equal(t, DecidedByDialog, row.DecidedBy)
	granted := f.eventsOf(t, core.EventAccessGranted)
	require.Len(t, granted, 1)
	require.Equal(t, ProofDialog, granted[0].Payload["proof"])
}

func TestDialogDecision_AdvisoryApprovesAMoveAsTheDialogs(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, openOps, "ext-adv-2", agent)
	a, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home, Via: ViaRefusal}})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, a.Kind)
	facts, err := g.Facts(ctx, a.ID)
	require.NoError(t, err)

	_, err = g.ApproveMove(ctx, Decision{ID: a.ID, Duration: ForSession, Digest: facts.Digest, Proof: Proof{Kind: ProofDialog},
		Access: "read-only"})
	require.ErrorIs(t, err, ErrAccess, "a move offers no access choice, in the dialog as anywhere")
	done, err := g.ApproveMove(ctx, Decision{ID: a.ID, Duration: ForSession, Digest: facts.Digest, Proof: Proof{Kind: ProofDialog}})
	require.NoError(t, err)
	require.Equal(t, confHR, done.Child.ProjectSlug)
	require.Equal(t, home, done.Left.ID)
	require.Equal(t, DecidedByDialog, done.Move.DecidedBy)
	m := advMoveDecided(t, f, a.ID)
	require.Equal(t, DecidedByDialog, m.DecidedBy)
	require.Equal(t, confHR, m.ToProject)
	row, err := store.AccessApprovalByID(ctx, f.db, a.ID)
	require.NoError(t, err)
	require.Equal(t, DecidedByDialog, row.DecidedBy)
	approved := f.eventsOf(t, core.EventAccessApproved)
	require.Len(t, approved, 1)
	require.Equal(t, ProofDialog, approved[0].Payload["proof"])
}

// Deny in the dialog closes the request as the dialog's; the agent then hears
// the denial at its next refusal, and its explicit ask files anew (Addendum 7).
func TestDialogDecision_AdvisoryDenyIsTheDialogs(t *testing.T) {
	f := newKindFixture(t)
	g := f.advisory()
	ctx := context.Background()
	home := f.conversation(t, openOps, "ext-adv-3", agent)
	a, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home, Via: ViaRefusal}})
	require.NoError(t, err)

	require.NoError(t, g.DenyInDialog(ctx, a.ID))
	row, err := store.AccessApprovalByID(ctx, f.db, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalDenied, row.Status)
	require.Equal(t, DecidedByDialog, row.DecidedBy)
	denied := f.eventsOf(t, core.EventAccessDenied)
	require.Len(t, denied, 1)
	require.Equal(t, DecidedByDialog, denied[0].Payload["proof"])
	require.ErrorIs(t, g.DenyInDialog(ctx, a.ID), store.ErrApprovalNotPending, "a decided request stays decided")

	_, err = g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home, Via: ViaRefusal}})
	var de *DeniedError
	require.ErrorAs(t, err, &de, "a refusal after the dialog's Deny files nothing and names it")
	again, err := g.Ask(ctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: home, Via: ViaRequest}})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, again.ID)

	// A lock's request is never the dialog's to deny: only peer_auth files one.
	now := f.now
	lock, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: lockFin, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: RequestTTL, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key()}, now)
	require.NoError(t, err)
	require.ErrorIs(t, g.DenyInDialog(ctx, lock.ID), ErrNotAGrant)
	row, err = store.AccessApprovalByID(ctx, f.db, lock.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalPending, row.Status)
}

// Under peer_auth the 2026-11-06 rule stands: the dialog's decision is
// refused for a grant and a move, its Deny too, and the lock's approve core
// never took it.
func TestDialogDecision_PeerAuthRefusesTheDialog(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	for _, slug := range []string{confHR, confPayroll} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, f.db, slug, core.IsolationConfidential))
	}
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	require.False(t, g.Advisory())
	sess := f.session(t, "cc/ops", "/work/ops", agent)
	vctx := access.WithView(ctx, f.view(t, bridge, agent, shell))

	move, err := g.Ask(vctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: sess}})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, move.Kind)
	mf, err := g.Facts(ctx, move.ID)
	require.NoError(t, err)
	_, err = g.ApproveMove(ctx, Decision{ID: move.ID, Duration: ForSession, Digest: mf.Digest, Proof: Proof{Kind: ProofDialog}})
	require.ErrorIs(t, err, ErrDialogPeerAuth)

	_, err = f.db.ExecContext(ctx, `UPDATE sessions SET project_slug = ? WHERE id = ?`, confPayroll, sess)
	require.NoError(t, err)
	grant, err := g.Ask(vctx, AskInput{GrantInput: GrantInput{Project: confHR, SessionID: sess}, Write: true})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindGrant, grant.Kind)
	gf, err := g.Facts(ctx, grant.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: grant.ID, Duration: For1h, Digest: gf.Digest, Proof: Proof{Kind: ProofDialog}})
	require.ErrorIs(t, err, ErrDialogPeerAuth)
	_, err = g.Approve(ctx, Decision{ID: grant.ID, Duration: For1h, Digest: gf.Digest, Proof: Proof{Kind: ProofDialog},
		Access: "read-write"})
	require.ErrorIs(t, err, ErrDialogPeerAuth)
	require.ErrorIs(t, g.DenyInDialog(ctx, grant.ID), ErrDialogPeerAuth)

	lock := f.request(t, lockFin, sess, bridge, agent, shell)
	lf, err := f.svc.Facts(ctx, lock.ID)
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, Decision{ID: lock.ID, Duration: For1h, Digest: lf.Digest, Proof: Proof{Kind: ProofDialog}})
	require.ErrorIs(t, err, ErrNoProof, "the lock's core knows no dialog proof")

	for _, id := range []string{move.ID, grant.ID, lock.ID} {
		row, err := store.AccessApprovalByID(ctx, f.db, id)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, row.Status, "nothing was decided")
		require.Empty(t, row.DecidedBy)
	}
	live, err := g.Live(ctx)
	require.NoError(t, err)
	require.Empty(t, live)
}
