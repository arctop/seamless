package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestHeldWrites_InsertListResolve(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	h := HeldWrite{
		ID: "01HOLD0000000000000000000A", CreatedAt: at, Op: HeldOpWrite, Tool: "memory_write",
		ItemKind: core.ItemKindMemory, ItemID: "01MEM", Project: "app", Name: "no-force-push",
		MemoryKind: core.KindConstraint, SessionID: "01SESS", Reasons: []string{"session.tainted"},
		Content: "---\nid: 01MEM\n---\nbody\n", Detail: map[string]string{"path": "memory/app/no-force-push.md"},
	}
	require.NoError(t, InsertHeldWrite(ctx, db, h))
	older := h
	older.ID, older.CreatedAt, older.ItemID, older.Name = "01HOLD0000000000000000000B", at.Add(-time.Hour), "01MEM2", "other"
	require.NoError(t, InsertHeldWrite(ctx, db, older))

	got, ok, err := HeldWriteByID(ctx, db, h.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, HeldStatusHeld, got.Status)
	require.Equal(t, []string{"session.tainted"}, got.Reasons)
	require.Equal(t, "memory/app/no-force-push.md", got.DetailValue("path"))
	require.Equal(t, core.KindConstraint, got.MemoryKind)
	require.Nil(t, got.ResolvedAt)
	require.True(t, got.CreatedAt.Equal(at))

	list, err := HeldWrites(ctx, db, HeldStatusHeld, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, h.ID, list[0].ID, "newest first")
	n, err := CountPendingHolds(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	forItem, err := PendingHoldsFor(ctx, db, "01MEM")
	require.NoError(t, err)
	require.Len(t, forItem, 1)
	named, err := PendingHoldsNamed(ctx, db, core.ItemKindMemory, "app", "no-force-push")
	require.NoError(t, err)
	require.Len(t, named, 1)

	// Resolving erases the held text and moves the row out of the queue; a
	// second resolution of the same row is refused, whichever status it asks for.
	require.NoError(t, ResolveHeldWrite(ctx, db, h.ID, HeldStatusDiscarded, at.Add(time.Minute)))
	require.ErrorIs(t, ResolveHeldWrite(ctx, db, h.ID, HeldStatusReleased, at), ErrHoldNotPending)
	require.ErrorIs(t, ResolveHeldWrite(ctx, db, "01NOSUCH", HeldStatusReleased, at), ErrHoldNotPending)
	require.ErrorContains(t, ResolveHeldWrite(ctx, db, older.ID, "held", at), "invalid status")
	got, _, err = HeldWriteByID(ctx, db, h.ID)
	require.NoError(t, err)
	require.Equal(t, HeldStatusDiscarded, got.Status)
	require.Empty(t, got.Content, "a discarded write's text does not survive")
	require.NotNil(t, got.ResolvedAt)
	n, err = CountPendingHolds(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestOwnerConfirmations_UpsertAndRead(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, UpsertOwnerConfirmation(ctx, db, "01A", "d1", ConfirmedViaRelease, now))
	require.NoError(t, UpsertOwnerConfirmation(ctx, db, "01A", "d2", ConfirmedViaRelease, now.Add(time.Minute)))
	got, err := OwnerConfirmations(ctx, db, []string{"01A", "01B"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"01A": "d2"}, got, "the newest confirmation wins; unconfirmed ids are absent")
	empty, err := OwnerConfirmations(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestContentWriters_NewestWriteWins(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for i, e := range []struct{ kind, session, item string }{
		{string(core.EventMemoryWritten), "01S1", "01A"},
		{string(core.EventMemoryRead), "01S9", "01A"}, // a read is not a write
		{string(core.EventMemoryWritten), "01S2", "01A"},
		{string(core.EventNoteWritten), "", "01N"},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
			VALUES (?, ?, ?, ?, '', ?, '{}')`, core.FormatTime(time.Now()), core.FormatTime(time.Date(2026, 10, 1, 0, i, 0, 0, time.UTC)),
			e.kind, e.session, e.item)
		require.NoError(t, err)
	}
	got, err := ContentWriters(ctx, db, []string{"01A", "01N", "01Z"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"01A": "01S2", "01N": ""}, got)
}

func TestSessionTaints_FirstMarkWinsAndOnlyClearRemovesIt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01S", Name: "cc/abc", ProjectSlug: "app",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))

	first, err := SetSessionTaint(ctx, db, "01S", "web_fetch", "hook", now)
	require.NoError(t, err)
	require.True(t, first)
	first, err = SetSessionTaint(ctx, db, "01S", "mail", "hook", now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, first, "a session is tainted from its FIRST outside read")

	tm, ok, err := SessionTaintOf(ctx, db, "01S")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "web_fetch", tm.Reason)
	require.True(t, tm.TaintedAt.Equal(now))

	list, err := TaintedSessions(ctx, db)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "cc/abc", list[0].Name)
	require.Equal(t, "app", list[0].Project)

	cleared, err := ClearSessionTaint(ctx, db, "01S")
	require.NoError(t, err)
	require.True(t, cleared)
	cleared, err = ClearSessionTaint(ctx, db, "01S")
	require.NoError(t, err)
	require.False(t, cleared)
	_, ok, err = SessionTaintOf(ctx, db, "01S")
	require.NoError(t, err)
	require.False(t, ok)
}
