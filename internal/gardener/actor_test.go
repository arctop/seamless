package gardener

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func TestAgentActor_NamesTheSessionAndNeverTheOwner(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"cc/abc123", "cc/abc123"},
		{"sess/01ABC", "sess/01ABC"},
		{"", ByAgent},
		// A session's name is the agent's choice: a reserved one is kept
		// apart from the value it spells.
		{ByOwner, "session owner"},
		{ByAuto, "session auto"},
		{ByGardener, "session gardener"},
		{ByAgent, "session agent"},
	} {
		require.Equal(t, tc.want, AgentActor(tc.in), tc.in)
		require.True(t, IsAgentActor(AgentActor(tc.in)), tc.in)
		name, ok := SessionNameOf(AgentActor(tc.in))
		require.Equal(t, tc.in != "", ok, tc.in)
		require.Equal(t, tc.in, name, "the session is found again from what was recorded")
	}
	name, ok := SessionNameOf("session cc/x")
	require.True(t, ok)
	require.Equal(t, "session cc/x", name, "only a reserved name was prefixed")
	for _, by := range []string{ByOwner, ByAuto, ByGardener, ""} {
		require.False(t, IsAgentActor(by), by)
	}
	require.Empty(t, ActorOf(context.Background()))
	require.Equal(t, "cc/x", ActorOf(WithActor(context.Background(), "cc/x")))
}

// gdActionEvents returns the gardener.action payloads about one proposal,
// oldest first.
func gdActionEvents(t *testing.T, g *Service, id string) []map[string]any {
	t.Helper()
	evs, err := g.events.ByKinds(context.Background(), []core.EventKind{core.EventGardenerAction}, "", "", 200)
	require.NoError(t, err)
	var out []map[string]any
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].ItemID == id {
			out = append(out, evs[i].Payload)
		}
	}
	return out
}

// Every decision records who made it: on the proposal (resolved_by) and on its
// event (by, with the owner's spelled as a console write). Undo clears the
// decider with the decision and records who undid it; a call that names
// nobody records nobody.
func TestDecisions_RecordWhoDecided(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()
	writeMem(t, mgr, "old-guidance", "example-ops", "1", core.KindGotcha, now, "body")
	id := memoryID(t, g, "example-ops", "old-guidance")

	resolvedBy := func(pid string) string {
		t.Helper()
		p, ok, err := store.ProposalByID(ctx, g.db, pid)
		require.NoError(t, err)
		require.True(t, ok)
		return p.ResolvedBy
	}
	propose := func(key string) string {
		t.Helper()
		p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
			"key": key, "id": id, "name": "old-guidance", "project": "example-ops",
		})
		require.NoError(t, err)
		return p.ID
	}

	owner := WithActor(ctx, ByOwner)
	agent := WithActor(ctx, AgentActor("cc/worker-1"))

	// An agent dismisses; the owner undoes it, then applies.
	p1 := propose("archive:" + id)
	require.NoError(t, g.Dismiss(agent, p1))
	require.Equal(t, "cc/worker-1", resolvedBy(p1))
	require.NoError(t, g.Undo(owner, p1))
	require.Empty(t, resolvedBy(p1), "a pending proposal has no decider")
	_, err := g.Apply(owner, p1)
	require.NoError(t, err)
	require.Equal(t, ByOwner, resolvedBy(p1))
	evs := gdActionEvents(t, g, p1)
	require.Len(t, evs, 3)
	require.Equal(t, []any{"dismiss", "undo_reject", "apply"}, []any{evs[0]["action"], evs[1]["action"], evs[2]["action"]})
	require.Equal(t, []any{"cc/worker-1", "console", "console"}, []any{evs[0]["by"], evs[1]["by"], evs[2]["by"]})

	// An agent hides; the owner lifts the block, which stands as the owner's
	// dismissal.
	p2 := propose("archive:other")
	require.NoError(t, g.Hide(agent, p2))
	require.Equal(t, "cc/worker-1", resolvedBy(p2))
	require.NoError(t, g.Unhide(owner, p2))
	require.Equal(t, ByOwner, resolvedBy(p2))

	// Nobody named: nothing recorded, and no by on the event.
	p3 := propose("archive:third")
	require.NoError(t, g.Dismiss(ctx, p3))
	require.Empty(t, resolvedBy(p3))
	evs = gdActionEvents(t, g, p3)
	require.Len(t, evs, 1)
	require.NotContains(t, evs[0], "by")
}

// A proposal an apply finds moot is settled in the applier's name.
func TestApply_MootSettlesInTheAppliersName(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	writeMem(t, mgr, "gone", "example-ops", "1", core.KindGotcha, time.Now().UTC(), "body")
	id := memoryID(t, g, "example-ops", "gone")
	p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{"key": "archive:" + id, "id": id, "name": "gone"})
	require.NoError(t, err)
	archiveByHand(t, g, mgr, id)
	res, err := g.Apply(WithActor(ctx, AgentActor("cc/worker-2")), p.ID)
	require.NoError(t, err)
	require.NotEmpty(t, res["settled"])
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalDismissed, got.Status)
	require.Equal(t, "cc/worker-2", got.ResolvedBy)
}
