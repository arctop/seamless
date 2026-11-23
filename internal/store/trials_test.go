package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestQueryTrials_LimitServiceBoundary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, limit := range []int{-1, MaxTrialQueryLimit + 1} {
		_, err := QueryTrials(ctx, db, TrialFilter{Limit: limit})
		require.ErrorIs(t, err, ErrInvalidTrialQueryLimit)
	}
	for _, limit := range []int{0, 1, MaxTrialQueryLimit} {
		_, err := QueryTrials(ctx, db, TrialFilter{Limit: limit})
		require.NoError(t, err)
	}
}

func addTrial(t *testing.T, db *sql.DB, lab, title string, outcome core.TrialOutcome, metrics map[string]any, seq int) string {
	t.Helper()
	return addTrialIn(t, db, lab, title, outcome, metrics, "demo", "", seq)
}

func addTrialIn(t *testing.T, db *sql.DB, lab, title string, outcome core.TrialOutcome, metrics map[string]any, project, session string, seq int) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	ts := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
	require.NoError(t, CreateTrial(context.Background(), db, core.Trial{
		ID: id, Lab: lab, Title: title, Outcome: outcome, Metrics: metrics,
		ProjectSlug: project, SessionID: session, CreatedAt: ts,
	}))
	return id
}

func TestQueryTrialsFilters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	addTrial(t, db, "demo-dfu", "baseline", core.OutcomeFail, map[string]any{"fw": "2.0.3", "hz": 497}, 1)
	addTrial(t, db, "demo-dfu", "retry", core.OutcomePass, map[string]any{"fw": "2.0.4", "hz": 500}, 2)
	addTrial(t, db, "other-lab", "unrelated", core.OutcomePass, map[string]any{"hz": 497}, 3)

	// Lab filter, newest first.
	byLab, err := QueryTrials(ctx, db, TrialFilter{Lab: "demo-dfu"})
	require.NoError(t, err)
	require.Len(t, byLab, 2)
	require.Equal(t, "retry", byLab[0].Title)

	// Outcome filter.
	fails, err := QueryTrials(ctx, db, TrialFilter{Lab: "demo-dfu", Outcome: string(core.OutcomeFail)})
	require.NoError(t, err)
	require.Len(t, fails, 1)
	require.Equal(t, "baseline", fails[0].Title)

	// Metrics equality filter: 497 (int literal) matches the stored 497.
	byHz, err := QueryTrials(ctx, db, TrialFilter{Lab: "demo-dfu", MetricsEquals: map[string]any{"hz": 497}})
	require.NoError(t, err)
	require.Len(t, byHz, 1)
	require.Equal(t, "baseline", byHz[0].Title)
	require.EqualValues(t, 497, byHz[0].Metrics["hz"])

	// Multi-key metrics filter that nothing satisfies.
	none, err := QueryTrials(ctx, db, TrialFilter{MetricsEquals: map[string]any{"hz": 497, "fw": "9.9.9"}})
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestQueryTrials_SessionFilter(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sessA, err := core.NewID()
	require.NoError(t, err)
	addTrialIn(t, db, "lab-a", "by session a", core.OutcomePass, nil, "demo", sessA, 1)
	addTrialIn(t, db, "lab-a", "no session", core.OutcomeFail, nil, "demo", "", 2)

	bySess, err := QueryTrials(ctx, db, TrialFilter{SessionID: sessA})
	require.NoError(t, err)
	require.Len(t, bySess, 1)
	require.Equal(t, "by session a", bySess[0].Title)
}

func TestTrialByID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id := addTrial(t, db, "demo-dfu", "baseline", core.OutcomeFail, map[string]any{"hz": 497}, 1)

	tr, found, err := TrialByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "baseline", tr.Title)
	require.Equal(t, "demo-dfu", tr.Lab)
	require.EqualValues(t, 497, tr.Metrics["hz"])

	_, found, err = TrialByID(ctx, db, "nope")
	require.NoError(t, err)
	require.False(t, found)
}

// ListLabs aggregates each lab's outcome tallies (free-form and empty outcomes
// land in Other), distinct sessions and projects, and the first/last stamps,
// most recently active lab first.
func TestListLabsAggregates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	sessA, err := core.NewID()
	require.NoError(t, err)
	addTrialIn(t, db, "old-lab", "ancient", core.OutcomePass, nil, "demo", "", 1)
	addTrialIn(t, db, "hot-lab", "t1", core.OutcomePass, nil, "", sessA, 2)
	addTrialIn(t, db, "hot-lab", "t2", core.OutcomeFail, nil, "demo", sessA, 3)
	addTrialIn(t, db, "hot-lab", "t3", core.TrialOutcome("exploded"), nil, "demo", "", 4)
	addTrialIn(t, db, "hot-lab", "t4", "", nil, "demo", "", 5)

	labs, err := ListLabs(ctx, db)
	require.NoError(t, err)
	require.Len(t, labs, 2)

	hot := labs[0]
	require.Equal(t, "hot-lab", hot.Lab, "most recently active lab first")
	require.Equal(t, 4, hot.Trials)
	require.Equal(t, 1, hot.Pass)
	require.Equal(t, 1, hot.Fail)
	require.Zero(t, hot.Partial)
	require.Zero(t, hot.Inconclusive)
	require.Equal(t, 2, hot.Other, "free-form and empty outcomes both count as other")
	require.Equal(t, 1, hot.Sessions, "distinct sessions, empty excluded")
	require.Equal(t, []string{"", "demo"}, hot.Projects, "distinct projects sorted, global kept")
	require.True(t, hot.LastAt.After(hot.FirstAt))

	require.Equal(t, "old-lab", labs[1].Lab)
	require.Equal(t, []string{"demo"}, labs[1].Projects)
}

func TestSearchTrials(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id := addTrial(t, db, "demo-dfu", "baseline sweep", core.OutcomeFail, nil, 1)
	addTrial(t, db, "demo-dfu", "retry sweep", core.OutcomePass, nil, 2)
	addTrial(t, db, "other-lab", "unrelated", core.OutcomePass, nil, 3)

	// Title match, newest first.
	byTitle, err := SearchTrials(ctx, db, "sweep", 10)
	require.NoError(t, err)
	require.Len(t, byTitle, 2)
	require.Equal(t, "retry sweep", byTitle[0].Title)

	// Lab match.
	byLab, err := SearchTrials(ctx, db, "other-lab", 10)
	require.NoError(t, err)
	require.Len(t, byLab, 1)
	require.Equal(t, "unrelated", byLab[0].Title)

	// Exact id match.
	byID, err := SearchTrials(ctx, db, id, 10)
	require.NoError(t, err)
	require.Len(t, byID, 1)
	require.Equal(t, "baseline sweep", byID[0].Title)

	// LIKE metacharacters match literally, not as wildcards.
	none, err := SearchTrials(ctx, db, "%", 10)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestGetNavCounts_CountsLabsAndTrials(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	addTrial(t, db, "lab-a", "t1", core.OutcomePass, nil, 1)
	addTrial(t, db, "lab-a", "t2", core.OutcomeFail, nil, 2)
	addTrial(t, db, "lab-b", "t3", core.OutcomePass, nil, 3)

	n, err := GetNavCounts(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 2, n.Labs)
	require.Equal(t, 3, n.Trials)
}

func TestCountTrials_MatchesTheFilterWithoutALimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for i := range 5 {
		outcome := core.OutcomePass
		if i%2 == 0 {
			outcome = core.OutcomeFail
		}
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTrial(ctx, db, core.Trial{ID: id, Lab: "count-lab", Title: "t", Outcome: outcome, CreatedAt: time.Now().UTC()}))
	}
	n, err := CountTrials(ctx, db, TrialFilter{Lab: "count-lab"})
	require.NoError(t, err)
	require.Equal(t, 5, n)
	n, err = CountTrials(ctx, db, TrialFilter{Lab: "count-lab", Outcome: string(core.OutcomeFail)})
	require.NoError(t, err)
	require.Equal(t, 3, n)
	_, err = CountTrials(ctx, db, TrialFilter{MetricsEquals: map[string]any{"hz": 1}})
	require.Error(t, err, "a metrics filter is QueryTrials' alone")
}
