package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestComputeStage(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		in   StageInputs
		want ProjectStage
	}{
		{"unregistered stays seedling", StageInputs{Memories: 100, Events: 5000}, StageSeedling},
		{"young stays seedling", StageInputs{CreatedAt: now.Add(-2 * day), Memories: 10, Events: 100}, StageSeedling},
		{"old but empty stays seedling", StageInputs{CreatedAt: now.Add(-400 * day)}, StageSeedling},
		{"sprouting at the exact bars", StageInputs{CreatedAt: now.Add(-7 * day), Memories: 5, Events: 25}, StageSprouting},
		{"established", StageInputs{CreatedAt: now.Add(-31 * day), Memories: 20, Events: 300}, StageEstablished},
		{"deep-rooted needs reach", StageInputs{CreatedAt: now.Add(-100 * day), Memories: 50, Events: 2000}, StageEstablished},
		{"deep-rooted", StageInputs{CreatedAt: now.Add(-100 * day), Memories: 50, Events: 2000, HasReach: true, ReachRate: 45}, StageDeepRooted},
		{"reach below the bar holds established", StageInputs{CreatedAt: now.Add(-100 * day), Memories: 50, Events: 2000, HasReach: true, ReachRate: 30}, StageEstablished},
	} {
		require.Equal(t, tc.want, ComputeStage(tc.in, now), tc.name)
	}
	require.Greater(t, StageDeepRooted.Rank(), StageEstablished.Rank())
	require.Greater(t, StageEstablished.Rank(), StageSprouting.Rank())
	require.Greater(t, StageSprouting.Rank(), StageSeedling.Rank())
	require.Equal(t, 0, ProjectStage("garbage").Rank(), "unknown reads as seedling")
}

// seedRegisteredProject registers a project and backdates its creation.
func seedRegisteredProject(t *testing.T, db *sql.DB, slug string, created time.Time) {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateProject(ctx, db, core.Project{ID: id, Slug: slug, Name: slug}))
	_, err = db.ExecContext(ctx, `UPDATE projects SET created_at = ? WHERE slug = ?`,
		core.FormatTime(created), slug)
	require.NoError(t, err)
}

// seedProjectEvents writes n minimal event rows for a project.
func seedProjectEvents(t *testing.T, db *sql.DB, slug string, n int, ts time.Time) {
	t.Helper()
	for range n {
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = db.ExecContext(context.Background(), `
			INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
			VALUES (?, ?, 'tool.call', '', ?, '', '{}')`,
			id, core.FormatTime(ts), slug)
		require.NoError(t, err)
	}
}

func TestEvaluateProjectStages_LatchesAndNeverRegresses(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	seedRegisteredProject(t, db, "demo", now.AddDate(0, 0, -10))
	seedRegisteredProject(t, db, "fresh", now)
	seedProjectEvents(t, db, "demo", 30, now.Add(-time.Hour))

	rows := []ProjectBoardRow{
		{Project: ""}, // the global scope earns no stage
		{Project: "demo", Memories: 6},
		{Project: "fresh", Memories: 0},
	}

	// The first evaluation seeds the latch: demo already holds sprouting, but
	// nobody watched it cross, so no crossing is minted and no since is claimed.
	stages, crossings, err := EvaluateProjectStages(ctx, db, rows, now)
	require.NoError(t, err)
	require.NotContains(t, stages, "", "the global scope is not a project")
	require.Equal(t, StageSprouting, stages["demo"].Stage)
	require.True(t, stages["demo"].Since.IsZero(), "a seeded stage claims no crossing day")
	require.Equal(t, StageSeedling, stages["fresh"].Stage)
	require.Empty(t, crossings, "seeding is not crossing")
	require.Equal(t, 6, stages["demo"].Inputs.Memories, "the inputs ride along for the progress tooltip")
	require.Equal(t, 30, stages["demo"].Inputs.Events)

	// fresh was seen as a seedling, so growing past the bar is a crossing.
	later := now.AddDate(0, 0, 8)
	seedProjectEvents(t, db, "fresh", 30, later.Add(-time.Hour))
	rows[2].Memories = 6
	stages, crossings, err = EvaluateProjectStages(ctx, db, rows, later)
	require.NoError(t, err)
	require.Equal(t, []StageCrossing{{Project: "fresh", From: StageSeedling, To: StageSprouting}}, crossings,
		"one crossing minted, for the project the latch watched grow")
	require.Equal(t, later, stages["fresh"].Since)

	// A second evaluation mints nothing new.
	_, crossings, err = EvaluateProjectStages(ctx, db, rows, later)
	require.NoError(t, err)
	require.Empty(t, crossings, "the latch was already set")

	// The project sheds its memories: the stage stands (never regress).
	rows[1].Memories = 0
	stages, crossings, err = EvaluateProjectStages(ctx, db, rows, later)
	require.NoError(t, err)
	require.Empty(t, crossings)
	require.Equal(t, StageSprouting, stages["demo"].Stage, "stages never regress")

	// A project the latch has never seen (one a console lock kept off the
	// board) is seeded at the stage it holds, not crossed into it.
	seedRegisteredProject(t, db, "hidden", now.AddDate(0, -2, 0))
	seedProjectEvents(t, db, "hidden", 300, now)
	stages, crossings, err = EvaluateProjectStages(ctx, db,
		append(rows, ProjectBoardRow{Project: "hidden", Memories: 20}), later)
	require.NoError(t, err)
	require.Empty(t, crossings)
	require.Equal(t, StageEstablished, stages["hidden"].Stage)

	// A corrupt latch row degrades to a fresh seed, never a failed page or a
	// burst of crossings.
	require.NoError(t, SetSetting(ctx, db, SettingProjectStages, "not json"))
	rows[1].Memories = 6
	stages, crossings, err = EvaluateProjectStages(ctx, db, rows, later)
	require.NoError(t, err)
	require.Empty(t, crossings)
	require.Equal(t, StageSprouting, stages["demo"].Stage)
}
