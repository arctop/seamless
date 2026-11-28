package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// closeDoneTask inserts a task closed done at the given instant.
func closeDoneTask(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "demo", Title: "shipped", Status: core.TaskDone, ClosedAt: &at,
		CreatedAt: at.Add(-time.Hour), UpdatedAt: at,
	}))
}

// The records ledger begins from the history, not from nothing: its first
// evaluation seeds the day records with the best day before today -- its real
// count and day, the earliest on a tie -- and the agents live now as a Seen
// mark, so judging today against it crosses nothing that history already
// beat, and nothing at all for the live agents.
func TestEvaluateGamificationRecords_SeedsFromHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	today := localDay(now)
	key := func(d time.Time) string { return bucketKey(d, false) }

	tie, best := today.AddDate(0, 0, -9), today.AddDate(0, 0, -5)
	for range 3 {
		closeDoneTask(t, db, best.Add(10*time.Hour))
	}
	for range 2 {
		closeDoneTask(t, db, tie.Add(10*time.Hour))
		insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", tie.Add(11*time.Hour))
		insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", best.Add(11*time.Hour))
	}
	// Today's own output is judged against the seed, never part of it.
	closeDoneTask(t, db, now)

	recs, crossings, err := EvaluateGamificationRecords(ctx, db, DayCounts{TasksClosed: 1, MemoriesWritten: 2}, 4, now)
	require.NoError(t, err)
	require.Empty(t, crossings, "the first look breaks no record history already holds")
	require.Equal(t, RecordMark{N: 3, Day: key(best)}, recs.TasksDay)
	require.Equal(t, RecordMark{N: 2, Day: key(tie)}, recs.MemoriesDay, "a tie keeps the earliest day")
	require.Equal(t, RecordMark{N: 4, Day: key(today), Seen: true}, recs.LiveAgents,
		"the live agents have no history: counted when the ledger began, not a record")

	// The seed is stored: the next look reads it rather than seeding again.
	_, found, err := gamificationRecordsStored(ctx, db)
	require.NoError(t, err)
	require.True(t, found)

	// Beating the seed is a record, with the seed as the value it beat.
	recs, crossings, err = EvaluateGamificationRecords(ctx, db, DayCounts{TasksClosed: 4, MemoriesWritten: 2}, 5, now)
	require.NoError(t, err)
	require.Equal(t, []RecordCrossing{
		{Key: "tasks_day", Label: "most tasks closed in a day", N: 4, Prev: 3},
		{Key: "live_agents", Label: "most agents live at once", N: 5, Prev: 4},
	}, crossings)
	require.Equal(t, RecordMark{N: 5, Day: key(today)}, recs.LiveAgents, "a beaten seed is a real record")
}

// An install with no history and no live agent seeds unset records, so its
// first activity sets them; a corrupt row is no ledger, seeded again rather
// than failing the page.
func TestEvaluateGamificationRecords_EmptyHistoryAndCorruptRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	recs, crossings, err := EvaluateGamificationRecords(ctx, db, DayCounts{}, 0, now)
	require.NoError(t, err)
	require.Empty(t, crossings)
	require.Equal(t, GamificationRecords{}, recs, "nothing to judge: every record unset")

	_, crossings, err = EvaluateGamificationRecords(ctx, db, DayCounts{MemoriesWritten: 1}, 1, now)
	require.NoError(t, err)
	require.Len(t, crossings, 2, "the first memory and the first live agent set their records")

	require.NoError(t, SetSetting(ctx, db, SettingGamificationRecords, "not json"))
	insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", localDay(now).AddDate(0, 0, -1).Add(time.Hour))
	recs, crossings, err = EvaluateGamificationRecords(ctx, db, DayCounts{MemoriesWritten: 1}, 1, now)
	require.NoError(t, err)
	require.Empty(t, crossings, "reseeded from history: yesterday's one memory already holds the record")
	require.Equal(t, 1, recs.MemoriesDay.N)
}
