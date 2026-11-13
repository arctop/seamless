package commitfeed

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// goldenOwnerSnapshot is the snapshot of goldenOwnerRecord, byte for byte, as
// it was written before entries could name a restricted account. The owner's
// sessions and tasks must never change shape.
const goldenOwnerSnapshot = `{"kind":"seamless_feed","version":1,"host":"build-host","taken_at":"2026-10-31T12:00:00.000Z","since":"2026-10-28T12:00:00.000Z","sessions":[{"id":"cx/bbbb-2222","repo":"plain","started_at":"2026-10-28T12:00:00.000Z","ended_at":"2026-10-28T13:00:00.000Z"},{"id":"cc/aaaa-1111","repo":"App-Repo","started_at":"2026-10-29T12:00:00.000Z","ended_at":null,"commits":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]},{"id":"01KX0000000000000000000S03","repo":null,"started_at":"2026-10-30T12:00:00.000Z","ended_at":"2026-10-30T12:00:00.000Z"},{"id":"01KX0000000000000000000S04","repo":"App-Repo","started_at":"2026-10-30T12:00:00.000Z","ended_at":"2026-10-30T13:00:00.000Z"}],"tasks":[{"id":"01KX0000000000000000000T01","repo":"App-Repo","commits":["cccccccccccccccccccccccccccccccccccccccc"]},{"id":"01KX0000000000000000000T02"}]}`

// goldenOwnerRecord is a record of the owner's work alone: sessions named
// every way the snapshot lists one -- by name, and by ULID for a name off the
// pattern, a name that only looks like a restricted account's included -- and
// tasks with and without recorded commits.
func goldenOwnerRecord(t *testing.T, ctx context.Context) {
	t.Helper()
	db := feedDB(t)
	mapRepo(t, db, "h1", "/work/app", "app", "git@github.com:owner/App-Repo.git")
	mapRepo(t, db, "h1", "/work/plain", "plain", "")
	day := 24 * time.Hour
	add := func(id, name, project, cwd string, status core.SessionStatus, created, updated time.Time) {
		require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: project, Status: status,
			CWD: cwd, Host: "h1", CreatedAt: created, UpdatedAt: updated}))
	}
	add("01KX0000000000000000000S01", "cc/aaaa-1111", "app", "/work/app", core.SessionActive, taken.Add(-2*day), taken.Add(-time.Hour))
	add("01KX0000000000000000000S02", "cx/bbbb-2222", "plain", "/work/plain", core.SessionCompleted, taken.Add(-3*day), taken.Add(-3*day+time.Hour))
	add("01KX0000000000000000000S03", "a name with spaces", "app", "/elsewhere", core.SessionCompleted, taken.Add(-day), taken.Add(-day))
	add("01KX0000000000000000000S04", "_example_owner:cc/1234", "app", "/work/app", core.SessionCompleted, taken.Add(-day), taken.Add(-day+time.Hour))
	for _, task := range []core.Task{
		{ID: "01KX0000000000000000000T01", ProjectSlug: "app", Title: "one", Status: core.TaskOpen, CreatedAt: taken.Add(-day), UpdatedAt: taken.Add(-day)},
		{ID: "01KX0000000000000000000T02", ProjectSlug: "elsewhere", Title: "two", Status: core.TaskInProgress, CreatedAt: taken.Add(-day), UpdatedAt: taken.Add(-day)},
	} {
		require.NoError(t, store.CreateTask(ctx, db, task))
	}
	_, err := store.RecordSessionCommits(ctx, db, "01KX0000000000000000000S01", "01KX0000000000000000000S01",
		[]string{commitB, commitA}, taken.Add(-time.Hour))
	require.NoError(t, err)
	_, err = store.UpdateTask(ctx, db, "01KX0000000000000000000T01", store.TaskPatch{AddCommits: []string{commitC}},
		"01KX0000000000000000000S01", taken.Add(-30*time.Minute))
	require.NoError(t, err)

	snap, trim, err := Build(ctx, db, Options{Host: "build-host", Window: 30 * day, Now: taken})
	require.NoError(t, err)
	require.Equal(t, Trimmed{}, trim)
	raw := encode(t, snap)
	require.NoError(t, CheckJSON(raw, taken))
	require.Equal(t, goldenOwnerSnapshot, string(raw))
}

func TestBuild_OwnerGolden(t *testing.T) {
	goldenOwnerRecord(t, context.Background())
}
