package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// Provenance stamps (plan fenced-sessions P6): a write carries, for each
// fenced project its conversation holds other than the destination, the
// record that cleared that project's content into it -- read from the same
// rows the clearance rule judges, never from the caller.

func stampMoveOf(t *testing.T, db *sql.DB, c *crConv) Move {
	t.Helper()
	h, err := HolderOfConversation(crClient, c.ext)
	require.NoError(t, err)
	moves, err := MovesForHolder(context.Background(), db, []Holder{h})
	require.NoError(t, err)
	require.Len(t, moves, 1)
	return moves[0]
}

func stampOf(project, via, ref string) core.Stamp {
	return core.Stamp{Project: project, Via: via, Ref: ref}
}

// The advisory tier: the conversation is the client's own session, found
// from its identity. Each way content gets into a label stamps a write with
// what cleared it: a grant, a move, the conversation's own session.
func TestStamps_AdvisoryTierNamesWhatClearedTheWrite(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)

	// A read grant between confidential projects: the home's writes may
	// contain the target's content.
	read := crStart(t, db, crData)
	rg := read.grant(crFin, false)
	rctx := read.judged(ctx)
	require.Equal(t, []core.Stamp{stampOf(crFin, core.StampViaGrant, rg.ApprovalID)}, StampsOf(rctx, crData))
	require.Nil(t, StampsOf(rctx, crFin), "a read grant clears nothing into its target (the write is refused)")

	// A read-and-write grant: each side may now hold the other's content.
	write := crStart(t, db, crData)
	wg := write.grant(crFin, true)
	wctx := write.judged(ctx)
	require.Equal(t, []core.Stamp{stampOf(crData, core.StampViaGrant, wg.ApprovalID)}, StampsOf(wctx, crFin))
	require.Equal(t, []core.Stamp{stampOf(crFin, core.StampViaGrant, wg.ApprovalID)}, StampsOf(wctx, crData))

	// A move from a confidential home into a sealed project: the move clears
	// the home's content into it.
	mover := crStart(t, db, crData)
	mover.move(crSafe, crData)
	mctx := mover.judged(ctx)
	require.Equal(t, crSafe, mustGrants(t, mctx).Home())
	require.Equal(t, []core.Stamp{stampOf(crData, core.StampViaMove, stampMoveOf(t, db, mover).ApprovalID)}, StampsOf(mctx, crSafe))

	// A fenced home's own session in another fenced project (a binding from
	// before the switch was on): no approval cleared it, so the stamp names
	// that session.
	native := crStart(t, db, crData)
	link := native.link(crFin)
	require.Equal(t, []core.Stamp{stampOf(crFin, core.StampViaSession, link.ID)}, StampsOf(native.judged(ctx), crData))

	// The label's own project is never a stamp: a write into it, from a
	// conversation holding nothing else, carries none.
	home := crStart(t, db, crFin)
	require.Nil(t, StampsOf(home.judged(ctx), crFin))
	// Nothing held, nothing stamped.
	require.Nil(t, StampsOf(crStart(t, db, crOps).judged(ctx), crOps))
	// Off: no judged conversation in ctx.
	require.Nil(t, StampsOf(ctx, crData))

	// The hooks' entry point reads the same state.
	got, err := ConversationStamps(ctx, db, read.ref(), crData, crNow)
	require.NoError(t, err)
	require.Equal(t, StampsOf(rctx, crData), got)
}

// The peer_auth tier: the conversation is the agent's process tree, holding
// the grants keyed to a process of its chain.
func TestStamps_PeerTierIsTheProcessTree(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	policy, err := access.New(config.PeerAuth{Owner: "owner"}, func(n string) (access.Account, error) {
		return access.Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: crBoot}
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: crBoot}
	view := func(chain ...access.ProcessToken) context.Context {
		v, ok := policy.PeerChainView(501, chain)
		require.True(t, ok)
		return access.WithView(ctx, v)
	}
	c := crStart(t, db, crData)
	c.process, c.pid = agent.Key(), agent.PID
	require.NoError(t, SetSessionAgentProcess(ctx, db, c.amb.ID, agent.AgentID()))
	g := c.grant(crFin, true)

	stamps, err := ConversationStamps(view(agent), db, c.ref(), crFin, crNow)
	require.NoError(t, err)
	require.Equal(t, []core.Stamp{stampOf(crData, core.StampViaGrant, g.ApprovalID)}, stamps)
	other, err := ConversationStamps(view(stranger), db, c.ref(), crFin, crNow)
	require.NoError(t, err)
	require.Nil(t, other, "another tree holds none of this tree's grants")
	tcp, err := ConversationStamps(access.WithView(ctx, policy.OwnerView()), db, c.ref(), crFin, crNow)
	require.NoError(t, err)
	require.Nil(t, tcp, "the TCP key holds nothing")
}

// A task's, a trial's and a session's stamps live in the index alone; each
// keeps the approval it was first stamped with, and a read names what the
// reference is.
func TestStampWorkItem_IndexOnlyKinds(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crData)
	g := c.grant(crFin, true)
	c.move(crSafe, crData)
	move := stampMoveOf(t, db, c)
	link := c.link(crLegal)

	stamps := []core.Stamp{
		stampOf(crFin, core.StampViaGrant, g.ApprovalID),
		stampOf(crLegal, core.StampViaSession, link.ID),
		stampOf(crOps, core.StampViaMove, move.ApprovalID),
		stampOf(crHR, core.StampViaGrant, "01NOAPPROVALHERE"),
		stampOf(crSafe, core.StampViaGrant, "01OWNPROJECT"),    // the item's own project
		stampOf("example two", core.StampViaGrant, "01SPACED"), // not one token
	}
	for _, kind := range []string{core.ItemKindTask, core.ItemKindTrial, core.ItemKindSession} {
		require.NoError(t, StampWorkItem(ctx, db, kind, "01"+kind, crSafe, stamps, crNow))
	}
	got, err := ProvenanceForItems(ctx, db, core.ItemKindTask, []string{"01" + core.ItemKindTask, "01none"})
	require.NoError(t, err)
	require.Equal(t, map[string][]core.Stamp{"01" + core.ItemKindTask: {
		stampOf(crFin, core.StampViaGrant, g.ApprovalID),
		stampOf(crHR, "", "01NOAPPROVALHERE"), // a reference this database holds no row for
		stampOf(crLegal, core.StampViaSession, link.ID),
		stampOf(crOps, core.StampViaMove, move.ApprovalID),
	}}, got)
	require.Equal(t, 12, migration039Count(t, db, "item_provenance"), "four stamps on each of three kinds")

	// Stamping again keeps the first approval.
	require.NoError(t, StampWorkItem(ctx, db, core.ItemKindTask, "01"+core.ItemKindTask, crSafe,
		[]core.Stamp{stampOf(crFin, core.StampViaMove, move.ApprovalID)}, crNow.Add(time.Hour)))
	again, err := ProvenanceForItem(ctx, db, core.ItemKindTask, "01"+core.ItemKindTask)
	require.NoError(t, err)
	require.Equal(t, g.ApprovalID, again[0].ApprovalID)
	require.Equal(t, crNow, again[0].CreatedAt)

	require.ErrorIs(t, StampWorkItem(ctx, db, core.ItemKindMemory, "01M", crSafe, stamps, crNow), ErrInvalidProvenance,
		"a memory's stamps are its file's")
	require.NoError(t, StampWorkItem(ctx, db, core.ItemKindTask, "01empty", crSafe, nil, crNow))
}

// The index mirrors a memory's or a note's file exactly: what the file names,
// with the file's approval, and nothing it does not name.
func TestProvenanceMirror_FollowsTheFile(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	rows := func() []ProvenanceStamp {
		t.Helper()
		got, err := ProvenanceForItem(ctx, db, core.ItemKindNote, "01NOTE")
		require.NoError(t, err)
		return got
	}
	file := []core.Stamp{stampOf(crFin, core.StampViaGrant, "01A"), stampOf(crLegal, core.StampViaMove, "01B"),
		stampOf(crData, core.StampViaGrant, "01OWN"), {Project: crHR}, {Ref: "01NOPROJECT"}}
	require.NoError(t, ProvenanceMirror(ctx, db, core.ItemKindNote, "01NOTE", crData, file, crNow))
	got := rows()
	require.Len(t, got, 2, "the item's own project and entries with no project or reference stay in the file")
	require.Equal(t, crFin, got[0].Source)
	require.Equal(t, crLegal, got[1].Source)

	// The owner's hand edit: one stamp removed, another's approval changed. The
	// file wins; the remaining row keeps its first time.
	require.NoError(t, ProvenanceMirror(ctx, db, core.ItemKindNote, "01NOTE", crData,
		[]core.Stamp{stampOf(crFin, core.StampViaGrant, "01C")}, crNow.Add(time.Hour)))
	got = rows()
	require.Len(t, got, 1)
	require.Equal(t, "01C", got[0].ApprovalID)
	require.Equal(t, crNow, got[0].CreatedAt)

	// A move into the project it was stamped for: the stamp leaves the index,
	// never the file.
	require.NoError(t, ProvenanceMirror(ctx, db, core.ItemKindNote, "01NOTE", crFin,
		[]core.Stamp{stampOf(crFin, core.StampViaGrant, "01C")}, crNow))
	require.Empty(t, rows())

	require.NoError(t, ProvenanceMirror(ctx, db, core.ItemKindNote, "01NOTE", crData,
		[]core.Stamp{stampOf(crFin, core.StampViaGrant, "01C")}, crNow))
	require.NoError(t, ProvenanceForget(ctx, db, core.ItemKindNote, "01NOTE"))
	require.Empty(t, rows(), "the item left the corpus")
	require.ErrorIs(t, ProvenanceMirror(ctx, db, core.ItemKindTask, "01T", crData, nil, crNow), ErrInvalidProvenance)
}

// A task moved to another project takes its stamps along; the one naming its
// new project as the source says nothing there and leaves the index.
func TestProvenanceRelocate_KeepsTheProjectTrue(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	require.NoError(t, StampWorkItem(ctx, db, core.ItemKindTask, "01T", crData,
		[]core.Stamp{stampOf(crFin, core.StampViaGrant, "01A"), stampOf(crLegal, core.StampViaGrant, "01B")}, crNow))
	require.NoError(t, ProvenanceRelocate(ctx, db, core.ItemKindTask, "01T", crLegal))
	in, err := ProvenanceForProject(ctx, db, crLegal, "", 0)
	require.NoError(t, err)
	require.Len(t, in, 1)
	require.Equal(t, crFin, in[0].Source)
	left, err := ProvenanceForProject(ctx, db, crData, "", 0)
	require.NoError(t, err)
	require.Empty(t, left)
}

func mustGrants(t *testing.T, ctx context.Context) access.Grants {
	t.Helper()
	g, ok := access.GrantsFrom(ctx)
	require.True(t, ok)
	return g
}

// The hooks' one call: the conversation's stamps for the destination, on the
// work-record item about to be written; none for a conversation holding
// nothing else, and none without a judged state to read (no conversation).
func TestStampConversationWrite_TheHooksCall(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crData)
	g := c.grant(crFin, false)
	require.NoError(t, StampConversationWrite(ctx, db, c.ref(), core.ItemKindSession, c.amb.ID, crData, crNow))
	got, err := ProvenanceForItems(ctx, db, core.ItemKindSession, []string{c.amb.ID})
	require.NoError(t, err)
	require.Equal(t, []core.Stamp{stampOf(crFin, core.StampViaGrant, g.ApprovalID)}, got[c.amb.ID])

	plain := crStart(t, db, crLegal)
	require.NoError(t, StampConversationWrite(ctx, db, plain.ref(), core.ItemKindSession, plain.amb.ID, crLegal, crNow))
	require.NoError(t, StampConversationWrite(ctx, db, ConversationRef{}, core.ItemKindSession, "01NONE", crLegal, crNow))
	require.Equal(t, 1, migration039Count(t, db, "item_provenance"))
}
