package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// The check-in digest counts exactly what was recorded after t, and only that:
// the same memory write an hour before t does not appear in it.
func TestSinceDigest_CountsWhatHappenedAfterT(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)
	now := time.Now().UTC()
	mark := now.Add(-time.Hour)

	sess, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sess, Name: "cc/checkin", Status: core.SessionActive,
		CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now,
	}))
	for _, e := range []core.Event{
		{TS: now.Add(-5 * time.Minute), Kind: core.EventMemoryWritten, SessionID: sess},
		{TS: now.Add(-4 * time.Minute), Kind: core.EventMemoryWritten, SessionID: sess},
		{TS: now.Add(-3 * time.Minute), Kind: core.EventNoteWritten, SessionID: sess},
		{TS: now.Add(-2 * time.Minute), Kind: core.EventAgentMishap, SessionID: sess},
		{TS: now.Add(-2 * time.Hour), Kind: core.EventMemoryWritten}, // before the mark
	} {
		id, err := core.NewID()
		require.NoError(t, err)
		e.ID = id
		_, err = rec.Record(ctx, e)
		require.NoError(t, err)
	}

	var got struct {
		Since time.Time `json:"since"`
		store.ActivitySince
	}
	getJSON(t, mux, "/console/since?t="+strconv.FormatInt(mark.UnixMilli(), 10), &got)
	require.Equal(t, 1, got.Sessions)
	require.Equal(t, 2, got.MemoriesWritten, "the write before t is not news")
	require.Equal(t, 1, got.NotesWritten)
	require.Equal(t, 1, got.Mishaps)
	require.WithinDuration(t, mark, got.Since, time.Second)
}

// A present-but-uninterpretable t is refused by name, never replaced by a
// default window.
func TestSinceDigest_RejectsBadT(t *testing.T) {
	_, mux := newConsole(t)
	future := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	ancient := strconv.FormatInt(time.Now().Add(-200*24*time.Hour).UnixMilli(), 10)
	for _, q := range []string{"", "?t=", "?t=yesterday", "?t=-5", "?t=" + future, "?t=" + ancient} {
		req := httptest.NewRequest(http.MethodGet, "/console/since"+q, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code, "GET /console/since%s", q)
		require.Contains(t, rr.Body.String(), `"error"`)
	}
}

// A live event still carries the []string its producer built; one read back
// from the log carries JSON's []any. Both must yield the surfaced ids.
func TestInjectedEventItemIDs_BothPayloadShapes(t *testing.T) {
	live := core.Event{Kind: core.EventInjected, Payload: map[string]any{"item_ids": []string{"a", "", "b"}}}
	stored := core.Event{Kind: core.EventInjected, Payload: map[string]any{"item_ids": []any{"a", 7, "b"}}}
	require.Equal(t, []string{"a", "b"}, injectedEventItemIDs(live))
	require.Equal(t, []string{"a", "b"}, injectedEventItemIDs(stored))
	require.Equal(t, []string{"a", "b"}, toEventRow(live).ItemIDs, "the live row names what reached the agent")
	require.Nil(t, toEventRow(core.Event{Kind: core.EventMemoryWritten, ItemID: "x"}).ItemIDs, "only injections carry the list")
}

// Every proposal kind the store can hold reads as words in the ledger, so a
// kind added to store.ProposalKinds without a phrase fails here, not in prose.
func TestGardenerSummary_CoversEveryProposalKind(t *testing.T) {
	for _, k := range store.ProposalKinds {
		_, ok := proposalWork[k]
		require.True(t, ok, "proposal kind %q has no ledger phrase", k)
	}
	require.Equal(t, "proposed archiving a memory", gardenerSummary(map[string]any{"action": "propose", "kind": "archive"}))
	require.Equal(t, "armed utility ranking for orbital", gardenerSummary(map[string]any{"action": "utility_armed", "project": "orbital"}))
	require.Equal(t, "dismissed a proposal", gardenerSummary(map[string]any{"action": "dismiss"}))
	require.Equal(t, "settled a moot proposal: memory seamless/x is already inactive",
		gardenerSummary(map[string]any{"action": "settle", "reason": "memory seamless/x is already inactive"}))
	require.Equal(t, "applied a brand new proposal", gardenerSummary(map[string]any{"action": "apply", "kind": "brand_new"}))
}
