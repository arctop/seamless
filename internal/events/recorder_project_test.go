package events

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// pvRecord records an event at ts and returns its id.
func pvRecord(t *testing.T, r *Recorder, e core.Event, ts time.Time) string {
	t.Helper()
	e.TS = ts
	id, err := r.Record(context.Background(), e)
	require.NoError(t, err)
	return id
}

func pvIDs(evs []core.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

// ProjectEvents reads one project's events, newest first, whatever the rest
// of the fleet recorded since: a quiet project's activity is not cut off by a
// busy one's.
func TestPvProjectEvents_OneProjectNewestFirst(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	quietOld := pvRecord(t, r, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "example-quiet"}, base)
	quietCall := pvRecord(t, r, core.Event{Kind: core.EventToolCall, ProjectSlug: "example-quiet"}, base.Add(time.Minute))
	quietNew := pvRecord(t, r, core.Event{Kind: core.EventTaskTransition, ProjectSlug: "example-quiet"}, base.Add(2*time.Minute))
	pvRecord(t, r, core.Event{Kind: core.EventMemoryWritten}, base.Add(3*time.Minute)) // the global scope's own
	for i := 0; i < 300; i++ {
		pvRecord(t, r, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "example-busy"}, base.Add(time.Hour+time.Duration(i)*time.Second))
	}

	got, err := r.ProjectEvents(ctx, "example-quiet", nil, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{quietNew, quietCall, quietOld}, pvIDs(got), "the project's own events, newest first")
	for _, e := range got {
		require.Equal(t, "example-quiet", e.ProjectSlug)
	}

	got, err = r.ProjectEvents(ctx, "example-quiet", nil, []core.EventKind{core.EventToolCall}, 0)
	require.NoError(t, err)
	require.Equal(t, []string{quietNew, quietOld}, pvIDs(got), "an excluded kind is left out")

	got, err = r.ProjectEvents(ctx, "example-quiet", []core.EventKind{core.EventTaskTransition, core.EventToolCall}, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{quietNew, quietCall}, pvIDs(got), "only the kinds asked for")

	got, err = r.ProjectEvents(ctx, "example-quiet", nil, nil, 1)
	require.NoError(t, err)
	require.Equal(t, []string{quietNew}, pvIDs(got), "the limit keeps the newest")

	got, err = r.ProjectEvents(ctx, "example-busy", nil, nil, 0)
	require.NoError(t, err)
	require.Len(t, got, 50, "a non-positive limit is 50")

	// A timestamp tie orders by id, as every other read of the log does.
	tieA := pvRecord(t, r, core.Event{ID: "01M80TIE00000000000000000A", Kind: core.EventMemoryWritten, ProjectSlug: "example-tie"}, base)
	tieB := pvRecord(t, r, core.Event{ID: "01M80TIE00000000000000000B", Kind: core.EventMemoryWritten, ProjectSlug: "example-tie"}, base)
	got, err = r.ProjectEvents(ctx, "example-tie", nil, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{tieB, tieA}, pvIDs(got))

	_, err = r.ProjectEvents(ctx, "", nil, nil, 0)
	require.ErrorIs(t, err, ErrNoProject, "the global scope is not a project: empty is refused, never read as every project")
}

// Through the console's filtered handle, a locked project's events are not
// there to read, and neither are the events filed under an open project that
// ran in a locked project's session or are about its items.
func TestPvProjectEvents_LockedProjectIsAbsent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	lockedSess, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: lockedSess, Name: "cc/ledger", ProjectSlug: "example-finance",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	lockedTask, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: lockedTask, ProjectSlug: "example-finance", Title: "reconcile",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))

	main := NewRecorder(db)
	shown := pvRecord(t, main, core.Event{Kind: core.EventTaskTransition, ProjectSlug: "example-ops"}, now)
	bySession := pvRecord(t, main, core.Event{Kind: core.EventTaskTransition, ProjectSlug: "example-ops", SessionID: lockedSess}, now)
	aboutItem := pvRecord(t, main, core.Event{Kind: core.EventTaskTransition, ProjectSlug: "example-ops", ItemID: lockedTask}, now)
	ownEvent := pvRecord(t, main, core.Event{Kind: core.EventTaskTransition, ProjectSlug: "example-finance"}, now)

	view, err := store.OpenView(dbPath, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	reading := main.ReadingFrom(view)

	got, err := reading.ProjectEvents(ctx, "example-ops", nil, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{shown}, pvIDs(got), "an open project's events, without the ones about the locked project")
	got, err = reading.ProjectEvents(ctx, "example-finance", nil, nil, 0)
	require.NoError(t, err)
	require.Empty(t, got, "the locked project's own events are not there")

	// The main handle still reads every one.
	got, err = main.ProjectEvents(ctx, "example-ops", nil, nil, 0)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{shown, bySession, aboutItem}, pvIDs(got))
	got, err = main.ProjectEvents(ctx, "example-finance", nil, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{ownEvent}, pvIDs(got))
}

// The read is served by the (project_slug, ts) index (migration 056), on the
// main handle and through the filtered one: it never scans the log.
func TestPvProjectEvents_SearchesTheProjectIndex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	view, err := store.OpenView(dbPath, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	ctx := context.Background()

	for _, tc := range []struct {
		name           string
		kinds, exclude []core.EventKind
	}{
		{"every kind but the excluded", nil, []core.EventKind{core.EventToolCall, core.EventHookPrompt}},
		{"one kind", []core.EventKind{core.EventTaskTransition}, nil},
		{"several kinds", []core.EventKind{core.EventToolCall, core.EventHookPrompt, core.EventInjected}, nil},
	} {
		q, args := projectEventsQuery("example-ops", tc.kinds, tc.exclude, 8)
		for name, handle := range map[string]*sql.DB{"main": db, "view": view} {
			plan := pvPlan(t, ctx, handle, q, args)
			require.Contains(t, plan, "USING INDEX idx_events_project (project_slug=?)", "%s, %s: %s", tc.name, name, plan)
			require.NotRegexp(t, `SCAN (main\.)?events\b`, plan, "%s, %s: the log is never scanned", tc.name, name)
		}
	}
}

// pvPlan is EXPLAIN QUERY PLAN's detail lines for q on db, one per line.
func pvPlan(t *testing.T, ctx context.Context, db *sql.DB, q string, args []any) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		lines = append(lines, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(lines, "\n")
}
