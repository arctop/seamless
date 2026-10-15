package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// seedMemoryRow inserts a minimal active memory index row for stats/staleness
// tests (no file needed; these tests exercise the index + events only).
func seedMemoryRow(t *testing.T, db *sql.DB, id, name string, updated time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO memories_index
			(id, kind, name, description, project, file_path, tags, content_hash, created_at, updated_at)
		VALUES (?, 'gotcha', ?, '', '', ?, '[]', 'h', ?, ?)`,
		id, name, "memory/_global/"+name+".md",
		core.FormatTime(updated), core.FormatTime(updated))
	require.NoError(t, err)
}

// insertEvent is a tiny helper writing a raw event row.
func insertEvent(t *testing.T, db *sql.DB, kind core.EventKind, itemID, payload string, ts time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, '', '', ?, ?)`,
		id, core.FormatTime(ts), string(kind), itemID, payload)
	require.NoError(t, err)
}

func TestRebuildRetrievalStats_FromEvents(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	// mem A: injected twice (once via item_ids array, once again later), read once.
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["A","B"]}`, base)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["A"]}`, base.Add(time.Hour))
	insertEvent(t, db, core.EventMemoryRead, "A", "{}", base.Add(2*time.Hour))
	// A coarse hook injection with no item ids contributes nothing per-item.
	insertEvent(t, db, core.EventInjected, "", `{"hook":"session-start"}`, base.Add(3*time.Hour))

	require.NoError(t, RebuildRetrievalStats(ctx, db))

	a, ok, err := GetRetrievalStat(ctx, db, "A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, a.InjectCount)
	require.Equal(t, 1, a.ReadCount)
	require.NotNil(t, a.LastInjectedAt)
	require.Equal(t, base.Add(time.Hour).Unix(), a.LastInjectedAt.Unix())
	require.NotNil(t, a.LastReadAt)
	require.Equal(t, base.Add(2*time.Hour).Unix(), a.LastReadAt.Unix())

	b, ok, err := GetRetrievalStat(ctx, db, "B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, b.InjectCount)
	require.Equal(t, 0, b.ReadCount)
	require.Nil(t, b.LastReadAt)

	// Rebuild is idempotent (clears then re-derives).
	require.NoError(t, RebuildRetrievalStats(ctx, db))
	a2, ok, err := GetRetrievalStat(ctx, db, "A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, a2.InjectCount)

	_, ok, err = GetRetrievalStat(ctx, db, "missing")
	require.NoError(t, err)
	require.False(t, ok)
}

// getStat fetches a stats row that must exist.
func getStat(t *testing.T, db *sql.DB, id string) RetrievalStat {
	t.Helper()
	s, ok, err := GetRetrievalStat(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok, "stats row for %s", id)
	return s
}

// Utility weighs query-gated demand only: an explicit read outranks a recall
// hit outranks a prompt match, and a briefing injection -- pure exposure --
// counts for the inject counters but never for utility.
func TestRebuildRetrievalStats_UtilityWeightsBySource(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Hour)

	insertEvent(t, db, core.EventMemoryRead, "READ", "{}", ts)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["RECALL"],"source":"recall"}`, ts)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["PROMPT"],"hook":"user-prompt-submit"}`, ts)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["BRIEF"],"hook":"session-start"}`, ts)

	require.NoError(t, rebuildRetrievalStats(ctx, db, now))

	read, recall, prompt, brief := getStat(t, db, "READ"), getStat(t, db, "RECALL"), getStat(t, db, "PROMPT"), getStat(t, db, "BRIEF")
	require.Greater(t, read.Utility, recall.Utility)
	require.Greater(t, recall.Utility, prompt.Utility)
	require.Greater(t, prompt.Utility, 0.0)
	require.Zero(t, brief.Utility, "briefing exposure is not demand")
	require.Equal(t, 1, brief.InjectCount, "the injection still counts as reach")

	require.Greater(t, read.Components.Read, 0.0)
	require.Zero(t, read.Components.Recall)
	require.Greater(t, recall.Components.Recall, 0.0)
	require.Greater(t, prompt.Components.Prompt, 0.0)

	scores, err := UtilityScores(ctx, db)
	require.NoError(t, err)
	require.Contains(t, scores, "READ")
	require.NotContains(t, scores, "BRIEF", "zero-utility rows stay out of the ranking map")
}

// The same signal is worth less the older it is: two half-lives quarter it.
func TestRebuildRetrievalStats_UtilityDecay(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	insertEvent(t, db, core.EventMemoryRead, "FRESH", "{}", now.Add(-time.Hour))
	insertEvent(t, db, core.EventMemoryRead, "OLD", "{}", now.Add(-28*24*time.Hour))

	require.NoError(t, rebuildRetrievalStats(ctx, db, now))

	fresh, old := getStat(t, db, "FRESH"), getStat(t, db, "OLD")
	require.Greater(t, fresh.Utility, old.Utility)
	require.InDelta(t, utilityWeightRead/4, old.Components.Read, 0.01,
		"28 days = two half-lives = a quarter of the weight")
}

// Utility saturates: no volume of demand reaches 1.0, so one hot memory cannot
// dominate the briefing blend.
func TestRebuildRetrievalStats_UtilityBounded(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// Sessionless events count individually (no dedup key), so this is 200
	// full-weight reads.
	for i := 0; i < 200; i++ {
		insertEvent(t, db, core.EventMemoryRead, "HOT", "{}", now.Add(-time.Minute))
	}
	require.NoError(t, rebuildRetrievalStats(ctx, db, now))

	hot := getStat(t, db, "HOT")
	require.Less(t, hot.Utility, 1.0)
	require.Greater(t, hot.Utility, 0.9)
}

// A signal class credits an item once per session: hammering one topic eight
// times in a session is one unit of demand, not eight.
func TestRebuildRetrievalStats_UtilityPerSessionDedup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Hour)

	for i := 0; i < 5; i++ {
		insertEvent(t, db, core.EventInjected, "",
			`{"item_ids":["BURST"],"hook":"user-prompt-submit","claude_session_id":"s1"}`, ts)
	}
	insertEvent(t, db, core.EventInjected, "",
		`{"item_ids":["SPREAD"],"hook":"user-prompt-submit","claude_session_id":"s1"}`, ts)
	insertEvent(t, db, core.EventInjected, "",
		`{"item_ids":["SPREAD"],"hook":"user-prompt-submit","claude_session_id":"s2"}`, ts)

	require.NoError(t, rebuildRetrievalStats(ctx, db, now))

	burst, spread := getStat(t, db, "BURST"), getStat(t, db, "SPREAD")
	require.Equal(t, 5, burst.InjectCount, "inject counters keep every event")
	require.InDelta(t, utilityWeightPrompt*utilityDecay(time.Hour), burst.Components.Prompt, 0.001,
		"five same-session prompt matches credit once")
	require.Greater(t, spread.Utility, burst.Utility,
		"two sessions of demand beat five repeats in one")
}

func TestStaleMemories(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-90 * 24 * time.Hour)

	// fresh: updated recently -> never stale regardless of stats.
	seedMemoryRow(t, db, "fresh", "fresh", now.Add(-1*24*time.Hour))
	// old-untouched: updated long ago, never injected/read -> stale.
	seedMemoryRow(t, db, "old", "old", now.Add(-120*24*time.Hour))
	// old-but-injected: updated long ago but injected recently -> NOT stale.
	seedMemoryRow(t, db, "kept", "kept", now.Add(-120*24*time.Hour))
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["kept"]}`, now.Add(-2*24*time.Hour))

	require.NoError(t, RebuildRetrievalStats(ctx, db))

	stale, err := StaleMemories(ctx, db, cutoff)
	require.NoError(t, err)
	names := make([]string, len(stale))
	for i, m := range stale {
		names[i] = m.Name
	}
	require.Equal(t, []string{"old"}, names)
}

// An injection whose payload does not parse still counts through its item_id
// column; it just cannot be classified for utility or tied to a session.
func TestRebuildRetrievalStats_UnparseablePayloadCountsByItemID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	insertEvent(t, db, core.EventInjected, "A", `{"source":"recall","item_ids":["B"]`, base)
	insertEvent(t, db, core.EventInjected, "", `not json`, base)
	require.NoError(t, RebuildRetrievalStats(ctx, db))

	a := getStat(t, db, "A")
	require.Equal(t, 1, a.InjectCount)
	require.Zero(t, a.Utility, "an unparseable payload carries no demand class")
	_, ok, err := GetRetrievalStat(ctx, db, "B")
	require.NoError(t, err)
	require.False(t, ok, "ids inside an unparseable payload are not trusted")
}

// The inputs fingerprint moves with exactly the events a rebuild reads, so a
// caller can skip a rebuild that would replay the same log.
func TestRetrievalStatsInputs_FollowTheEventsARebuildReads(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute)

	empty, err := RetrievalStatsInputs(ctx, db)
	require.NoError(t, err)

	insertEvent(t, db, core.EventToolCall, "", "{}", at)
	insertEvent(t, db, core.EventMemoryWritten, "A", "{}", at)
	same, err := RetrievalStatsInputs(ctx, db)
	require.NoError(t, err)
	require.Equal(t, empty, same, "events a rebuild never reads leave the inputs alone")

	for _, kind := range []core.EventKind{core.EventInjected, core.EventMemoryRead, core.EventNoteRead} {
		before, err := RetrievalStatsInputs(ctx, db)
		require.NoError(t, err)
		// An older timestamp than the newest: the count still moves.
		insertEvent(t, db, kind, "A", `{"item_ids":["A"]}`, at.Add(-time.Hour))
		after, err := RetrievalStatsInputs(ctx, db)
		require.NoError(t, err)
		require.NotEqual(t, before, after, "a %s event is an input", kind)
	}
}
