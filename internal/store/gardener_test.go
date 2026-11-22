package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestProposalLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	p, err := CreateProposal(ctx, db, ProposalArchive, map[string]any{"key": "archive:m1", "name": "m1"})
	require.NoError(t, err)
	require.Equal(t, ProposalPending, p.Status)
	require.NotEmpty(t, p.ID)

	pending, err := PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "m1", pending[0].Payload["name"])

	// Kind filter.
	require.Empty(t, mustPending(t, db, ProposalMerge))
	require.Len(t, mustPending(t, db, ProposalArchive), 1)

	// Keys span every status (used by the gardener to avoid re-proposing). A
	// pending proposal is a hard block: nothing re-raises what is already asked.
	keys, err := ProposalKeyBlocks(ctx, db)
	require.NoError(t, err)
	require.True(t, keys["archive:m1"].Hard)

	// Resolve moves it out of pending and stamps resolved_at.
	require.NoError(t, ResolveProposal(ctx, db, p.ID, ProposalApplied, "owner", time.Now().UTC()))
	require.Empty(t, mustPending(t, db, ""))

	got, ok, err := ProposalByID(ctx, db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ProposalApplied, got.Status)
	require.NotNil(t, got.ResolvedAt)
	require.Equal(t, "owner", got.ResolvedBy, "who decided is recorded with the decision")

	// The key is still known after resolution (never re-proposed).
	keys, err = ProposalKeyBlocks(ctx, db)
	require.NoError(t, err)
	require.True(t, keys["archive:m1"].Hard)

	// Resolving an already-resolved proposal is an error.
	require.Error(t, ResolveProposal(ctx, db, p.ID, ProposalDismissed, "owner", time.Now().UTC()))
	// Invalid status is rejected.
	require.Error(t, ResolveProposal(ctx, db, p.ID, "bogus", "owner", time.Now().UTC()))
}

// The apply result round-trips through the result column, and ReopenProposal
// returns a resolved proposal to the queue -- clearing both the resolution
// stamp and the now-meaningless result, while leaving the dedup key intact.
func TestProposalResultAndReopen(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	p, err := CreateProposal(ctx, db, ProposalDigest, map[string]any{"key": "digest:seamless:2026-07"})
	require.NoError(t, err)
	require.Nil(t, p.Result, "a fresh proposal has no result")

	// A pending proposal cannot be reopened -- only a resolved one.
	require.Error(t, ReopenProposal(ctx, db, p.ID))

	require.NoError(t, ResolveProposal(ctx, db, p.ID, ProposalApplied, "owner", time.Now().UTC()))
	require.NoError(t, RecordProposalResult(ctx, db, p.ID, map[string]any{
		"note_id": "01NOTE", "title": "seamless digest 2026-07",
	}))

	got, ok, err := ProposalByID(ctx, db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "01NOTE", got.Result["note_id"])

	require.NoError(t, ReopenProposal(ctx, db, p.ID))
	got, _, err = ProposalByID(ctx, db, p.ID)
	require.NoError(t, err)
	require.Equal(t, ProposalPending, got.Status)
	require.Nil(t, got.ResolvedAt, "reopening clears the resolution stamp")
	require.Empty(t, got.ResolvedBy, "and who decided: a pending proposal has no decider")
	require.Nil(t, got.Result, "reopening clears the stale apply result")
	require.Len(t, mustPending(t, db, ""), 1, "the proposal is back in the queue")

	// Dedup is unaffected: the key survives the whole round trip.
	keys, err := ProposalKeyBlocks(ctx, db)
	require.NoError(t, err)
	require.True(t, keys["digest:seamless:2026-07"].Hard)

	// A second undo of the same proposal is refused (it is pending again).
	require.Error(t, ReopenProposal(ctx, db, p.ID))
	// So is undoing something that never existed.
	require.Error(t, ReopenProposal(ctx, db, "01NOSUCHPROPOSAL"))
}

// ResolvedProposals pages the decided proposals, newest resolution first --
// the order the console's decision history relies on -- and
// CountResolvedProposals counts the same rows. Each row says who decided it.
func TestResolvedProposals_PagesByResolution(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	// Create oldest-first, resolve in the same order, so "newest resolved first"
	// is genuinely the reverse of creation rather than an accident of id order.
	var ids []string
	for i := range 4 {
		p, err := CreateProposal(ctx, db, ProposalArchive, map[string]any{"key": "archive:m" + string(rune('a'+i))})
		require.NoError(t, err)
		ids = append(ids, p.ID)
	}
	require.NoError(t, ResolveProposal(ctx, db, ids[0], ProposalApplied, "owner", base))
	require.NoError(t, ResolveProposal(ctx, db, ids[1], ProposalDismissed, "cc/agent-1", base.Add(time.Minute)))
	require.NoError(t, ResolveProposal(ctx, db, ids[2], ProposalApplied, "", base.Add(2*time.Minute)))
	// ids[3] stays pending.

	recent, err := ResolvedProposals(ctx, db, 0, 10)
	require.NoError(t, err)
	require.Len(t, recent, 3, "the pending proposal is not decided")
	require.Equal(t, []string{ids[2], ids[1], ids[0]}, []string{recent[0].ID, recent[1].ID, recent[2].ID})
	require.Equal(t, ProposalDismissed, recent[1].Status)
	require.Equal(t, []string{"", "cc/agent-1", "owner"},
		[]string{recent[0].ResolvedBy, recent[1].ResolvedBy, recent[2].ResolvedBy})

	n, err := CountResolvedProposals(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 3, n)

	first, err := ResolvedProposals(ctx, db, 0, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, ids[2], first[0].ID)
	second, err := ResolvedProposals(ctx, db, 2, 2)
	require.NoError(t, err)
	require.Len(t, second, 1, "the second page holds what the first left")
	require.Equal(t, ids[0], second[0].ID)
	past, err := ResolvedProposals(ctx, db, 4, 2)
	require.NoError(t, err)
	require.Empty(t, past)

	// A page that cannot exist is the caller's bug, not "everything".
	_, err = ResolvedProposals(ctx, db, 0, 0)
	require.Error(t, err)
	_, err = ResolvedProposals(ctx, db, -1, 10)
	require.Error(t, err)
}

// The column is migration 055's: a row an older release wrote (no column in
// its INSERT) reads as unknown, and so does one it resolved (its UPDATE does
// not name the column).
func TestProposalResolvedBy_OlderReleaseRowsReadUnknown(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := core.FormatTime(time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))
	// The previous release's own statements, verbatim in shape.
	_, err := db.ExecContext(ctx, `INSERT INTO gardener_proposals (id, kind, payload, status, created_at)
		VALUES (?, ?, ?, ?, ?)`, "01OLDRELEASEPROPOSAL000000", ProposalArchive, `{"key":"archive:old"}`, ProposalPending, at)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE gardener_proposals SET status = ?, resolved_at = ?
		WHERE id = ? AND status = ?`, ProposalDismissed, at, "01OLDRELEASEPROPOSAL000000", ProposalPending)
	require.NoError(t, err)
	got, ok, err := ProposalByID(ctx, db, "01OLDRELEASEPROPOSAL000000")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ProposalDismissed, got.Status)
	require.Empty(t, got.ResolvedBy)
}

// A rejection has two strengths, and the store is where they differ: a
// dismissal records WHEN the owner decided, so a pass can ask whether its
// evidence is newer, while a hide records that nothing newer will matter.
func TestProposalKeyBlocks_RejectionTiers(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	dismissed, err := CreateProposal(ctx, db, ProposalToolError, map[string]any{"key": "err:a"})
	require.NoError(t, err)
	hidden, err := CreateProposal(ctx, db, ProposalToolError, map[string]any{"key": "err:b"})
	require.NoError(t, err)
	require.NoError(t, ResolveProposal(ctx, db, dismissed.ID, ProposalDismissed, "owner", at))
	require.NoError(t, ResolveProposal(ctx, db, hidden.ID, ProposalHidden, "owner", at))

	blocks, err := ProposalKeyBlocks(ctx, db)
	require.NoError(t, err)
	require.False(t, blocks["err:a"].Hard, "a dismissal is a soft block")
	require.True(t, at.Equal(blocks["err:a"].DismissedAt), "and carries the moment to measure recurrence from")
	require.True(t, blocks["err:b"].Hard, "a hide is not")

	// A key re-raised after its dismissal has two rows. The pending one wins:
	// nothing re-proposes what is already in the queue.
	reraised, err := CreateProposal(ctx, db, ProposalToolError, map[string]any{"key": "err:a"})
	require.NoError(t, err)
	blocks, err = ProposalKeyBlocks(ctx, db)
	require.NoError(t, err)
	require.True(t, blocks["err:a"].Hard, "the strongest rule across a key's rows wins")

	// Dismissing that one too leaves the key soft again, at the LATER stamp.
	later := at.Add(48 * time.Hour)
	require.NoError(t, ResolveProposal(ctx, db, reraised.ID, ProposalDismissed, "owner", later))
	blocks, err = ProposalKeyBlocks(ctx, db)
	require.NoError(t, err)
	require.False(t, blocks["err:a"].Hard)
	require.True(t, later.Equal(blocks["err:a"].DismissedAt), "the newest dismissal is the one to beat")
}

// Hiding is reversible two ways, and they are not the same operation: unhide
// lifts the block while leaving the proposal resolved, undo puts the proposal
// itself back in the queue.
func TestHiddenProposals_UnhideAndReopen(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	p, err := CreateProposal(ctx, db, ProposalToolError, map[string]any{"key": "err:a"})
	require.NoError(t, err)
	require.NoError(t, ResolveProposal(ctx, db, p.ID, ProposalHidden, "owner", at))

	hidden, err := HiddenProposals(ctx, db)
	require.NoError(t, err)
	require.Len(t, hidden, 1)
	require.Equal(t, p.ID, hidden[0].ID)
	require.NotNil(t, hidden[0].ResolvedAt)

	// Unhide demotes to a regular dismissal, keeping the moment of the decision
	// -- that stamp is what a recurrence is measured against.
	require.NoError(t, UnhideProposal(ctx, db, p.ID, "owner"))
	got, ok, err := ProposalByID(ctx, db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ProposalDismissed, got.Status)
	require.True(t, at.Equal(*got.ResolvedAt), "unhide does not restamp the decision")
	require.Equal(t, "owner", got.ResolvedBy, "whoever lifted the block decided the dismissal")
	require.Empty(t, mustPending(t, db, ""), "and does not return it to the queue")

	empty, err := HiddenProposals(ctx, db)
	require.NoError(t, err)
	require.Empty(t, empty)

	// Unhiding something that is not hidden is an error, not a silent no-op.
	require.Error(t, UnhideProposal(ctx, db, p.ID, "owner"))

	// A hide is undoable like any other rejection: reopening returns it. (The
	// proposal is dismissed by now, so it goes back through the queue first --
	// only a pending proposal can be resolved into a tier.)
	require.NoError(t, ReopenProposal(ctx, db, p.ID))
	require.NoError(t, ResolveProposal(ctx, db, p.ID, ProposalHidden, "owner", at))
	require.Error(t, ResolveProposal(ctx, db, p.ID, ProposalHidden, "owner", at), "already resolved")
	require.NoError(t, ReopenProposal(ctx, db, p.ID))
	require.Len(t, mustPending(t, db, ""), 1)
}

func mustPending(t *testing.T, db *sql.DB, kind string) []Proposal {
	t.Helper()
	ps, err := PendingProposals(context.Background(), db, kind)
	require.NoError(t, err)
	return ps
}

// ProposalsAppliedBy lists what one decider applied and still stands applied,
// newest first and uncapped; ResolvedProposalsOmitting and its count page
// through every other decision, so the two together list each decision once.
// A dismissal by that decider, and an apply it made that was undone, are not
// its applies.
func TestGtProposalsAppliedBy_AndTheRestOmitThem(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mk := func(i int) string {
		t.Helper()
		p, err := CreateProposal(ctx, db, ProposalToolError, map[string]any{"key": "tool_error:x:" + string(rune('a'+i))})
		require.NoError(t, err)
		return p.ID
	}
	auto1, auto2, autoDismissed, owner, undone, pending := mk(0), mk(1), mk(2), mk(3), mk(4), mk(5)
	require.NoError(t, ResolveProposal(ctx, db, auto1, ProposalApplied, "auto", base))
	require.NoError(t, ResolveProposal(ctx, db, auto2, ProposalApplied, "auto", base.Add(2*time.Minute)))
	require.NoError(t, ResolveProposal(ctx, db, autoDismissed, ProposalDismissed, "auto", base.Add(3*time.Minute)))
	require.NoError(t, ResolveProposal(ctx, db, owner, ProposalApplied, "owner", base.Add(time.Minute)))
	require.NoError(t, ResolveProposal(ctx, db, undone, ProposalApplied, "auto", base.Add(4*time.Minute)))
	require.NoError(t, ReopenProposal(ctx, db, undone))
	require.NoError(t, ResolveProposal(ctx, db, undone, ProposalHidden, "owner", base.Add(5*time.Minute)))
	_ = pending

	applied, err := ProposalsAppliedBy(ctx, db, "auto")
	require.NoError(t, err)
	require.Equal(t, []string{auto2, auto1}, gtIDs(applied), "newest first; a dismissal is no apply, and an undone apply left")

	rest, err := ResolvedProposalsOmitting(ctx, db, "auto", 0, 10)
	require.NoError(t, err)
	require.Equal(t, []string{undone, autoDismissed, owner}, gtIDs(rest))
	n, err := CountResolvedProposalsOmitting(ctx, db, "auto")
	require.NoError(t, err)
	require.Equal(t, 3, n)

	// Omitting nobody is the whole history.
	all, err := ResolvedProposalsOmitting(ctx, db, "", 0, 10)
	require.NoError(t, err)
	require.Len(t, all, 5)
	n, err = CountResolvedProposals(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 5, n)

	// A decider of "" names nobody: it lists nothing rather than every apply
	// whose decider was never recorded.
	require.NoError(t, ResolveProposal(ctx, db, pending, ProposalApplied, "", base.Add(6*time.Minute)))
	none, err := ProposalsAppliedBy(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, none)
	_, err = ResolvedProposalsOmitting(ctx, db, "auto", 0, 0)
	require.Error(t, err, "a page that cannot exist is still refused")
}

func gtIDs(ps []Proposal) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}
