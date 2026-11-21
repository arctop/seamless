package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// requireTotalsAreTheLedger holds a session's token totals to the sum of its
// ledger rows: the ledger is the single source.
func requireTotalsAreTheLedger(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()
	ctx := context.Background()
	sess, ok, err := SessionByID(ctx, db, sessionID)
	require.NoError(t, err)
	require.True(t, ok, sessionID)
	rows, err := SessionUsage(ctx, db, sessionID)
	require.NoError(t, err)
	buckets := make([]core.UsageBucket, len(rows))
	for i, r := range rows {
		buckets[i] = r.UsageBucket
	}
	require.Equal(t, core.SumUsage(buckets), sess.Tokens, "session %s: its totals are its ledger's sum", sessionID)
}

// ledgerAmbient creates an active ambient session of conversation ext.
func ledgerAmbient(t *testing.T, db *sql.DB, id, client, ext, project string, created time.Time) core.Session {
	t.Helper()
	s := core.Session{ID: id, Name: client + "/" + ext, ProjectSlug: project, Status: core.SessionActive, Ambient: true,
		ExternalClient: client, ExternalSessionID: ext, Model: "claude-x", CreatedAt: created, UpdatedAt: created}
	require.NoError(t, CreateSession(context.Background(), db, s))
	return s
}

// ledgerRow is a usage row at the quarter-hour starting at at.
func ledgerRow(at time.Time, model string, thread core.UsageThread, in, out, msgs, mask int) core.UsageBucket {
	return core.UsageBucket{Bucket: at.UTC(), Model: model, Thread: thread, Input: in, Output: out, Messages: msgs, MinuteMask: mask}
}

func ledgerBuckets(rows []UsageRow) []core.UsageBucket {
	out := make([]core.UsageBucket, len(rows))
	for i, r := range rows {
		out[i] = r.UsageBucket
	}
	return out
}

// A Claude Code harvest replaces the conversation's rows with its
// transcript's, in order, and the totals become their sum. A grown transcript
// harvested again replaces them -- never adds to them -- and the same one
// harvested twice leaves the same rows.
func TestReplaceConversationUsage_ReplacesAndSumsTheTotals(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := ledgerAmbient(t, db, "01LEDGER00000000000000000A", "claude-code", "led-1", "example-ops", day)

	first := []core.UsageBucket{
		ledgerRow(day, "claude-x", core.UsageThreadMain, 100, 10, 2, 0b11),
		ledgerRow(day, "claude-x", core.UsageThreadSubagent, 40, 4, 1, 0b100),
		ledgerRow(day.Add(15*time.Minute), "claude-y", core.UsageThreadMain, 7, 3, 1, 1<<14),
	}
	ok, err := ReplaceConversationUsage(ctx, db, "claude-code", "led-1", first, day.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	got, err := SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, first, ledgerBuckets(got))
	requireTotalsAreTheLedger(t, db, s.ID)

	ok, err = ReplaceConversationUsage(ctx, db, "claude-code", "led-1", first, day.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	got, err = SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, first, ledgerBuckets(got), "the same transcript again: the same rows")

	grown := append([]core.UsageBucket{}, first...)
	grown[2].Output += 5
	grown = append(grown, ledgerRow(day.Add(30*time.Minute), "claude-y", core.UsageThreadMain, 1, 1, 1, 1))
	ok, err = ReplaceConversationUsage(ctx, db, "claude-code", "led-1", grown, day.Add(2*time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	got, err = SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, grown, ledgerBuckets(got), "the grown transcript's rows, in place of the old")
	requireTotalsAreTheLedger(t, db, s.ID)
	sess, _, err := SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, core.SumUsage(grown), sess.Tokens, "never the sum of two harvests")
}

// Replace against add: a Claude Code harvest is absolute (the last one is the
// rows), a Codex credit adds what the counter grew by -- into one row while
// the turns end in one quarter-hour, its messages counted and minutes or-ed,
// and into a new row in the next. A counter that shrank in any count, or did
// not move, writes nothing.
func TestUsage_ReplaceAgainstAdd(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	at := time.Date(2026, 11, 20, 9, 2, 0, 0, time.UTC)
	s := ledgerAmbient(t, db, "01LEDGER00000000000000000B", "codex", "led-cx", "example-ops", at)

	credit := func(in, cached, out int, when time.Time) bool {
		t.Helper()
		u := core.TokenUsage{Input: in, Cached: cached, Output: out}
		u.Normalize()
		ok, err := CreditConversationUsage(ctx, db, "codex", "led-cx", UsageCredit{Cumulative: u, Model: "gpt-x", At: when, Turn: true})
		require.NoError(t, err)
		return ok
	}
	require.True(t, credit(100, 1000, 10, at))
	require.True(t, credit(150, 1800, 25, at.Add(5*time.Minute)))
	require.False(t, credit(150, 1800, 25, at.Add(6*time.Minute)), "the same total: nothing grew")
	require.False(t, credit(140, 1900, 30, at.Add(7*time.Minute)), "a count shrank: nothing written")
	require.True(t, credit(160, 1900, 30, at.Add(20*time.Minute)))

	got, err := SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	q := core.UsageBucketStart(at)
	require.Equal(t, []core.UsageBucket{
		{Bucket: q, Model: "gpt-x", Thread: core.UsageThreadMain, Input: 150, Cached: 1800, Output: 25, Messages: 2,
			MinuteMask: 1<<2 | 1<<7},
		{Bucket: q.Add(15 * time.Minute), Model: "gpt-x", Thread: core.UsageThreadMain, Input: 10, Cached: 100, Output: 5,
			Messages: 1, MinuteMask: 1 << 7},
	}, ledgerBuckets(got))
	requireTotalsAreTheLedger(t, db, s.ID)

	// A credit that is no turn (a pushed total) counts no message and no minute,
	// and a credit naming no model takes the session's.
	u := core.TokenUsage{Input: 170, Cached: 1900, Output: 30}
	u.Normalize()
	ok, err := CreditConversationUsage(ctx, db, "codex", "led-cx", UsageCredit{Cumulative: u, At: at.Add(50 * time.Minute)})
	require.NoError(t, err)
	require.True(t, ok)
	got, err = SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, core.UsageBucket{Bucket: q.Add(45 * time.Minute), Model: "claude-x", Thread: core.UsageThreadMain, Input: 10},
		got[2].UsageBucket)

	// A Claude Code harvest after them is absolute: its rows are the session's.
	rows := []core.UsageBucket{ledgerRow(q, "claude-x", core.UsageThreadMain, 1, 1, 1, 1)}
	ok, err = ReplaceConversationUsage(ctx, db, "codex", "led-cx", rows, at.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	got, err = SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, rows, ledgerBuckets(got))
	requireTotalsAreTheLedger(t, db, s.ID)
}

// The harvest lands on the conversation's ambient row whatever its status:
// completed by the agent's session_end or expired by the reaper, the row takes
// the counts and keeps its status and the time it ended at.
func TestReplaceConversationUsage_AnEndedRowTakesTheCountsAsMetadata(t *testing.T) {
	ctx := context.Background()
	for _, status := range []core.SessionStatus{core.SessionCompleted, core.SessionExpired} {
		t.Run(string(status), func(t *testing.T) {
			db := openTestDB(t)
			ended := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
			s := ledgerAmbient(t, db, "01LEDGER00000000000000000C", "claude-code", "led-end", "example-ops", ended.Add(-time.Hour))
			s.Status, s.UpdatedAt = status, ended
			require.NoError(t, UpdateSession(ctx, db, s))

			rows := []core.UsageBucket{ledgerRow(ended.Add(-time.Hour), "claude-x", core.UsageThreadMain, 30, 3, 1, 1)}
			ok, err := ReplaceConversationUsage(ctx, db, "claude-code", "led-end", rows, ended.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, ok)
			got, _, err := SessionByID(ctx, db, s.ID)
			require.NoError(t, err)
			require.Equal(t, 33, got.Tokens.Total)
			require.Equal(t, status, got.Status, "never revived")
			require.True(t, ended.Equal(got.UpdatedAt), "the time it ended at does not move")
		})
	}
}

// A moved conversation (isolation.requests) is one transcript across its
// sessions: the session the move ended keeps the buckets that ended by the
// move, the child takes every later one -- the quarter-hour the move fell in
// included -- and the two together are the whole transcript, counted once.
// A parent that harvested exactly the buckets before the move keeps them, and
// the child takes the totals less the parent's, as the rule before the ledger
// gave it (the child's totals were the transcript's less its parents').
func TestReplaceConversationUsage_AMoveSplitsTheBuckets(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	start := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	move := time.Date(2026, 11, 20, 10, 7, 0, 0, time.UTC)
	parent := core.Session{ID: "01LEDGERPARENT000000000000", Name: "cc/led-move", ProjectSlug: "example-ops",
		Status: core.SessionCompleted, ExternalClient: "claude-code", ExternalSessionID: "led-move",
		CreatedAt: start, UpdatedAt: move}
	require.NoError(t, CreateSession(ctx, db, parent))
	child := core.Session{ID: "01LEDGERCHILD0000000000000", Name: "cc/led-move@example-finance", ProjectSlug: "example-finance",
		Status: core.SessionActive, Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "led-move",
		ParentSessionID: parent.ID, CreatedAt: move, UpdatedAt: move}
	require.NoError(t, CreateSession(ctx, db, child))

	before := []core.UsageBucket{
		ledgerRow(start, "claude-x", core.UsageThreadMain, 100, 10, 3, 0b111),
		ledgerRow(start.Add(15*time.Minute), "claude-x", core.UsageThreadMain, 50, 5, 1, 1), // 09:45, ends 10:00
	}
	after := []core.UsageBucket{
		ledgerRow(move.Truncate(15*time.Minute), "claude-x", core.UsageThreadMain, 20, 2, 2, 0b10000001), // 10:00: the move's
		ledgerRow(move.Truncate(15*time.Minute).Add(15*time.Minute), "claude-x", core.UsageThreadSubagent, 9, 1, 1, 1),
	}
	all := append(append([]core.UsageBucket{}, before...), after...)
	ok, err := ReplaceConversationUsage(ctx, db, "claude-code", "led-move", all, move.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	parentRows, err := SessionUsage(ctx, db, parent.ID)
	require.NoError(t, err)
	require.Equal(t, before, ledgerBuckets(parentRows), "the parent keeps the buckets that ended by the move")
	childRows, err := SessionUsage(ctx, db, child.ID)
	require.NoError(t, err)
	require.Equal(t, after, ledgerBuckets(childRows), "the child takes the rest, the move's quarter-hour included")
	requireTotalsAreTheLedger(t, db, parent.ID)
	requireTotalsAreTheLedger(t, db, child.ID)

	p, _, err := SessionByID(ctx, db, parent.ID)
	require.NoError(t, err)
	c, _, err := SessionByID(ctx, db, child.ID)
	require.NoError(t, err)
	whole := core.SumUsage(all)
	require.Equal(t, whole, addUsage(p.Tokens, c.Tokens), "the conversation is counted once")
	less := core.TokenUsage{Input: whole.Input - p.Tokens.Input, Cached: whole.Cached - p.Tokens.Cached,
		CacheCreation: whole.CacheCreation - p.Tokens.CacheCreation, Output: whole.Output - p.Tokens.Output}
	less.Normalize()
	require.Equal(t, less, c.Tokens, "the child holds the totals less what the parent keeps")
	require.Equal(t, core.SessionCompleted, p.Status)
	require.True(t, move.Equal(p.UpdatedAt), "the parent keeps the time the move ended it at")
}

// UsageBetween returns the rows whose quarter-hour starts in [from, to), by
// bucket, then session, model and thread; a bound that is not a quarter-hour
// counts a bucket where it starts, and an empty range returns nothing.
func TestUsageBetween_RangeAndOrder(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	a := ledgerAmbient(t, db, "01LEDGERA00000000000000000", "claude-code", "led-a", "example-ops", day)
	b := ledgerAmbient(t, db, "01LEDGERB00000000000000000", "claude-code", "led-b", "example-ops", day)
	_, err := ReplaceConversationUsage(ctx, db, "claude-code", "led-a", []core.UsageBucket{
		ledgerRow(day.Add(-15*time.Minute), "claude-x", core.UsageThreadMain, 1, 0, 1, 1),
		ledgerRow(day, "claude-y", core.UsageThreadMain, 2, 0, 1, 1),
		ledgerRow(day, "claude-x", core.UsageThreadSubagent, 3, 0, 1, 1),
		ledgerRow(day.Add(24*time.Hour-15*time.Minute), "claude-x", core.UsageThreadMain, 4, 0, 1, 1),
		ledgerRow(day.Add(24*time.Hour), "claude-x", core.UsageThreadMain, 5, 0, 1, 1),
	}, day)
	require.NoError(t, err)
	_, err = ReplaceConversationUsage(ctx, db, "claude-code", "led-b", []core.UsageBucket{
		ledgerRow(day, "claude-x", core.UsageThreadMain, 6, 0, 1, 1),
	}, day)
	require.NoError(t, err)

	got, err := UsageBetween(ctx, db, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	type key struct {
		session string
		bucket  time.Time
		model   string
		thread  core.UsageThread
		input   int
	}
	keys := make([]key, len(got))
	for i, r := range got {
		keys[i] = key{r.SessionID, r.Bucket, r.Model, r.Thread, r.Input}
	}
	require.Equal(t, []key{
		{a.ID, day, "claude-x", core.UsageThreadSubagent, 3},
		{a.ID, day, "claude-y", core.UsageThreadMain, 2},
		{b.ID, day, "claude-x", core.UsageThreadMain, 6},
		{a.ID, day.Add(24*time.Hour - 15*time.Minute), "claude-x", core.UsageThreadMain, 4},
	}, keys, "the day's buckets: the one before it and the one after it are not")

	mid, err := UsageBetween(ctx, db, day.Add(time.Minute), day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, mid, 1, "a bucket counts where it starts")
	none, err := UsageBetween(ctx, db, day, day)
	require.NoError(t, err)
	require.Empty(t, none)
}

// The console's filtered handle hides a locked project's sessions, and their
// ledger rows with them.
func TestUsageBetween_TheViewHidesALockedProjectsRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	ledgerAmbient(t, db, "01LEDGERFIN000000000000000", "claude-code", "led-fin", "example-finance", day)
	ops := ledgerAmbient(t, db, "01LEDGEROPS000000000000000", "claude-code", "led-ops", "example-ops", day)
	for _, ext := range []string{"led-fin", "led-ops"} {
		_, err := ReplaceConversationUsage(ctx, db, "claude-code", ext,
			[]core.UsageBucket{ledgerRow(day, "claude-x", core.UsageThreadMain, 10, 1, 1, 1)}, day)
		require.NoError(t, err)
	}
	all, err := UsageBetween(ctx, db, day, day.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, all, 2)

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	seen, err := UsageBetween(ctx, view, day, day.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, seen, 1)
	require.Equal(t, ops.ID, seen[0].SessionID, "only the open project's session's row")
}

// Rows that do not hold are refused whole, and nothing is written.
func TestReplaceConversationUsage_RefusesRowsThatDoNotHold(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := ledgerAmbient(t, db, "01LEDGERBAD000000000000000", "claude-code", "led-bad", "example-ops", day)
	good := ledgerRow(day, "claude-x", core.UsageThreadMain, 1, 1, 1, 1)
	for name, bad := range map[string]core.UsageBucket{
		"unaligned":      ledgerRow(day.Add(time.Minute), "claude-x", core.UsageThreadMain, 1, 1, 1, 1),
		"unknown thread": ledgerRow(day, "claude-x", "sidechain", 1, 1, 1, 1),
		"negative":       ledgerRow(day, "claude-x", core.UsageThreadMain, -1, 1, 1, 1),
		"mask past 15":   ledgerRow(day, "claude-x", core.UsageThreadMain, 1, 1, 1, 1<<15),
		"duplicate":      good,
	} {
		_, err := ReplaceConversationUsage(ctx, db, "claude-code", "led-bad", []core.UsageBucket{good, bad}, day)
		require.ErrorIs(t, err, ErrInvalidUsage, name)
	}
	rows, err := SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Empty(t, rows)
	_, err = CreditConversationUsage(ctx, db, "claude-code", "led-bad",
		UsageCredit{Cumulative: core.TokenUsage{Input: -1, Output: 2, Total: 1}, At: day})
	require.ErrorIs(t, err, ErrInvalidUsage)
	_, err = RefreshConversationUsage(ctx, db, "claude-code", "led-bad",
		[]core.UsageBucket{good, ledgerRow(day, "claude-x", core.UsageThreadMain, -1, 1, 1, 1)}, day)
	require.ErrorIs(t, err, ErrInvalidUsage)
	rows, err = SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// ledgerUpdatedAt is a session's updated_at.
func ledgerUpdatedAt(t *testing.T, db *sql.DB, id string) time.Time {
	t.Helper()
	sess, ok, err := SessionByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok, id)
	return sess.UpdatedAt
}

// A mid-session refresh writes as Replace does, and its heartbeat with it;
// rows the ledger already holds write nothing, not even the heartbeat; and a
// read a later harvest overtook -- fewer messages than the ledger holds --
// writes nothing, whether the later one was a refresh or the end's Replace.
// So the ledger never moves back, whichever harvest lands last.
func TestRefreshConversationUsage_NeverMovesTheLedgerBack(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := ledgerAmbient(t, db, "01LEDGERLIVE00000000000000", "claude-code", "led-live", "example-ops", day)
	refresh := func(rows []core.UsageBucket, now time.Time) UsageRefresh {
		t.Helper()
		got, err := RefreshConversationUsage(ctx, db, "claude-code", "led-live", rows, now)
		require.NoError(t, err)
		return got
	}
	ledger := func() []core.UsageBucket {
		t.Helper()
		rows, err := SessionUsage(ctx, db, s.ID)
		require.NoError(t, err)
		return ledgerBuckets(rows)
	}

	// Two turns by the first prompt's read; the end reads a third.
	mid := []core.UsageBucket{
		ledgerRow(day, "claude-x", core.UsageThreadMain, 100, 10, 2, 0b11),
		ledgerRow(day, "claude-x", core.UsageThreadSubagent, 40, 4, 1, 0b100),
	}
	full := append(append([]core.UsageBucket{}, mid...), ledgerRow(day.Add(15*time.Minute), "claude-x", core.UsageThreadMain, 7, 3, 1, 1))

	require.Equal(t, RefreshWritten, refresh(mid, day.Add(time.Minute)))
	require.Equal(t, mid, ledger())
	requireTotalsAreTheLedger(t, db, s.ID)
	require.True(t, day.Add(time.Minute).Equal(ledgerUpdatedAt(t, db, s.ID)), "an active row takes the write as a heartbeat")

	require.Equal(t, RefreshUnchanged, refresh(mid, day.Add(2*time.Minute)))
	require.True(t, day.Add(time.Minute).Equal(ledgerUpdatedAt(t, db, s.ID)), "rows it holds write nothing, the heartbeat included")

	// The end harvests the whole transcript.
	ok, err := ReplaceConversationUsage(ctx, db, "claude-code", "led-live", full, day.Add(20*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, RefreshBehind, refresh(mid, day.Add(21*time.Minute)), "a read the end overtook")
	require.Equal(t, full, ledger(), "the end's rows stand")
	require.True(t, day.Add(20*time.Minute).Equal(ledgerUpdatedAt(t, db, s.ID)))
	require.Equal(t, RefreshUnchanged, refresh(full, day.Add(22*time.Minute)), "the same transcript, read after the end")

	// A later read of a grown transcript writes; one before it then does not.
	grown := append(append([]core.UsageBucket{}, full...), ledgerRow(day.Add(30*time.Minute), "claude-y", core.UsageThreadMain, 1, 1, 1, 1))
	require.Equal(t, RefreshWritten, refresh(grown, day.Add(31*time.Minute)))
	require.Equal(t, RefreshBehind, refresh(full, day.Add(32*time.Minute)), "a refresh another refresh overtook")
	require.Equal(t, grown, ledger())
	requireTotalsAreTheLedger(t, db, s.ID)
}

// A refresh lands where Replace does: on a row the agent or the reaper ended,
// without reviving it or moving its time; split at a move like Replace; and
// nowhere for a conversation with no ambient row.
func TestRefreshConversationUsage_LandsAsReplaceDoes(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	ended := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := ledgerAmbient(t, db, "01LEDGERLIVEEND00000000000", "claude-code", "led-live-end", "example-ops", ended.Add(-time.Hour))
	s.Status, s.UpdatedAt = core.SessionExpired, ended
	require.NoError(t, UpdateSession(ctx, db, s))
	rows := []core.UsageBucket{ledgerRow(ended.Add(-time.Hour), "claude-x", core.UsageThreadMain, 30, 3, 1, 1)}
	got, err := RefreshConversationUsage(ctx, db, "claude-code", "led-live-end", rows, ended.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, RefreshWritten, got)
	sess, _, err := SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, 33, sess.Tokens.Total)
	require.Equal(t, core.SessionExpired, sess.Status, "never revived")
	require.True(t, ended.Equal(sess.UpdatedAt), "the time it ended at does not move")

	start := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	move := time.Date(2026, 11, 20, 10, 7, 0, 0, time.UTC)
	parent := core.Session{ID: "01LEDGERLIVEPARENT00000000", Name: "cc/led-live-move", ProjectSlug: "example-ops",
		Status: core.SessionCompleted, ExternalClient: "claude-code", ExternalSessionID: "led-live-move",
		CreatedAt: start, UpdatedAt: move}
	require.NoError(t, CreateSession(ctx, db, parent))
	child := core.Session{ID: "01LEDGERLIVECHILD000000000", Name: "cc/led-live-move@example-finance", ProjectSlug: "example-finance",
		Status: core.SessionActive, Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "led-live-move",
		ParentSessionID: parent.ID, CreatedAt: move, UpdatedAt: move}
	require.NoError(t, CreateSession(ctx, db, child))
	before := []core.UsageBucket{ledgerRow(start, "claude-x", core.UsageThreadMain, 100, 10, 3, 0b111)}
	after := []core.UsageBucket{ledgerRow(move.Truncate(15*time.Minute), "claude-x", core.UsageThreadMain, 20, 2, 2, 0b10000001)}
	got, err = RefreshConversationUsage(ctx, db, "claude-code", "led-live-move", append(append([]core.UsageBucket{}, before...), after...), move.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, RefreshWritten, got)
	parentRows, err := SessionUsage(ctx, db, parent.ID)
	require.NoError(t, err)
	require.Equal(t, before, ledgerBuckets(parentRows), "the parent keeps the buckets that ended by the move")
	childRows, err := SessionUsage(ctx, db, child.ID)
	require.NoError(t, err)
	require.Equal(t, after, ledgerBuckets(childRows))
	requireTotalsAreTheLedger(t, db, parent.ID)
	requireTotalsAreTheLedger(t, db, child.ID)
	require.True(t, move.Equal(ledgerUpdatedAt(t, db, parent.ID)), "the parent keeps the time the move ended it at")
	require.True(t, move.Add(time.Minute).Equal(ledgerUpdatedAt(t, db, child.ID)), "the active child takes the heartbeat")

	got, err = RefreshConversationUsage(ctx, db, "claude-code", "led-live-none", rows, ended)
	require.NoError(t, err)
	require.Equal(t, RefreshNoConversation, got)
	got, err = RefreshConversationUsage(ctx, db, "claude-code", "led-live-end", nil, ended)
	require.NoError(t, err)
	require.Equal(t, RefreshNoConversation, got, "no rows write nothing")
}
