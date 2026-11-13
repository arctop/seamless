package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// ftsRow reads the single fts row for an item id. found is false when the item
// is not indexed, which is the assertion for "this should not be searchable".
func ftsRow(t *testing.T, db *sql.DB, itemID string) (kind, project, title, name, description, body string, found bool) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT kind, project, title, name, description, body FROM fts WHERE item_id = ?`, itemID).
		Scan(&kind, &project, &title, &name, &description, &body)
	if err == sql.ErrNoRows {
		return "", "", "", "", "", "", false
	}
	require.NoError(t, err)
	return kind, project, title, name, description, body, true
}

func TestIndexTaskFTS_CreateUpdateAndClose(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateTask(ctx, db, core.Task{
		ID: "01TASK", ProjectSlug: "seam", Title: "wire the reconciler",
		Body:   "the watcher debounce is 300ms, so the hash check has to run after it",
		Status: core.TaskOpen, PlanSlug: "search-visibility", CreatedAt: now, UpdatedAt: now,
	}))

	kind, project, title, name, _, body, found := ftsRow(t, db, "01TASK")
	require.True(t, found, "a created task must be searchable")
	require.Equal(t, core.ItemKindTask, kind)
	require.Equal(t, "seam", project)
	require.Equal(t, "wire the reconciler", title)
	require.Equal(t, "search-visibility", name, "the plan slug rides in name")
	require.Contains(t, body, "debounce")

	// An edit refreshes the mirror rather than leaving the old text behind.
	newBody := "superseded by the content_hash retry"
	_, err := UpdateTask(ctx, db, "01TASK", TaskPatch{Body: &newBody}, "", now)
	require.NoError(t, err)
	_, _, _, _, _, body, found = ftsRow(t, db, "01TASK")
	require.True(t, found)
	require.Equal(t, newBody, body)
	require.NotContains(t, body, "debounce", "the stale body must not survive the update")

	// A closed task STAYS indexed: "why was this dropped" is exactly the query
	// the work record exists to answer.
	dropped := core.TaskDropped
	_, err = UpdateTask(ctx, db, "01TASK", TaskPatch{Status: &dropped}, "", now)
	require.NoError(t, err)
	_, _, _, _, _, _, found = ftsRow(t, db, "01TASK")
	require.True(t, found, "a dropped task must remain searchable")

	hits, err := FTSSearch(ctx, db, "content_hash retry", []string{core.ItemKindTask}, nil, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "01TASK", hits[0].ItemID)
}

// A task moved to another project moves with it in the mirror, or the isolation
// fence (which filters on fts.project inside the candidate query) would keep
// serving it to its old scope.
func TestIndexTaskFTS_ProjectReassignmentMovesTheRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateTask(ctx, db, core.Task{
		ID: "01MOVE", ProjectSlug: "parent", Title: "split me", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	child := "child"
	_, err := UpdateTask(ctx, db, "01MOVE", TaskPatch{ProjectSlug: &child}, "", now)
	require.NoError(t, err)

	_, project, _, _, _, _, found := ftsRow(t, db, "01MOVE")
	require.True(t, found)
	require.Equal(t, "child", project)
}

func TestIndexTrialFTS_JoinsProseAndDropsBlanks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	require.NoError(t, CreateTrial(ctx, db, core.Trial{
		ID: "01TRIAL", Lab: "boot-race", Title: "readiness gate",
		Expected: "container healthy before first query",
		Actual:   "still raced on a cold cache",
		Outcome:  core.OutcomeFail, ProjectSlug: "seam", CreatedAt: time.Now().UTC(),
	}))

	kind, _, title, name, description, body, found := ftsRow(t, db, "01TRIAL")
	require.True(t, found)
	require.Equal(t, core.ItemKindTrial, kind)
	require.Equal(t, "readiness gate", title)
	require.Equal(t, "boot-race", name, "the lab rides in name")
	require.Equal(t, "fail", description, "the outcome rides in description")
	require.Equal(t, "container healthy before first query\nstill raced on a cold cache", body,
		"the empty changes field must not contribute a blank line")

	hits, err := FTSSearch(ctx, db, "cold cache", []string{core.ItemKindTrial}, nil, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "01TRIAL", hits[0].ItemID)
}

// A session enters the corpus for its findings alone: no findings, no row. The
// alternative is one row per process start, most of them saying nothing.
func TestIndexSessionFTS_FindingsOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01SESS", Name: "cc/abc", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	_, _, _, _, _, _, found := ftsRow(t, db, "01SESS")
	require.False(t, found, "an active session with no findings is not knowledge")

	ended := core.Session{
		ID: "01SESS", Name: "cc/abc", ProjectSlug: "seam", Status: core.SessionCompleted,
		Findings: "the reconciler skips unchanged files via content_hash", UpdatedAt: now,
	}
	require.NoError(t, UpdateSession(ctx, db, ended))

	kind, project, title, _, _, body, found := ftsRow(t, db, "01SESS")
	require.True(t, found)
	require.Equal(t, core.ItemKindSession, kind)
	require.Equal(t, "seam", project)
	require.Equal(t, "cc/abc", title)
	require.Equal(t, ended.Findings, body)

	// The auto-harvest sentinel says nothing, and clearing findings removes the
	// row rather than leaving a stale one behind.
	ended.Findings = core.FindingNoSummary
	require.NoError(t, UpdateSession(ctx, db, ended))
	_, _, _, _, _, _, found = ftsRow(t, db, "01SESS")
	require.False(t, found, "the no-summary sentinel must not be indexed")
}

// Codex has no SessionEnd, so the Stop hook's targeted findings write is the
// only path those sessions' findings ever take into the corpus.
func TestIndexSessionFTS_AmbientFindingsPath(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01AMB", Name: "cx/zz", ProjectSlug: "seam", Status: core.SessionActive,
		ExternalClient: "codex", ExternalSessionID: "ext-1", Ambient: true,
		CreatedAt: now, UpdatedAt: now,
	}))

	updated, err := UpdateAmbientFindings(ctx, db, "codex", "ext-1", "the lease heartbeat is a re-claim", now)
	require.NoError(t, err)
	require.True(t, updated)

	kind, _, _, _, _, body, found := ftsRow(t, db, "01AMB")
	require.True(t, found)
	require.Equal(t, core.ItemKindSession, kind)
	require.Equal(t, "the lease heartbeat is a re-claim", body)
}

// An explicit session that session_start linked to the conversation shares the
// ambient row's external identity. UpdateAmbientFindings writes only the
// ambient row, so its search mirror must reindex that row, even when the link
// was touched later than the Stop's clock read.
func TestIndexSessionFTS_AmbientFindingsPathIgnoresLinkedSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01AMBLNK", Name: "cx/zy", ProjectSlug: "seam", Status: core.SessionActive,
		ExternalClient: "codex", ExternalSessionID: "ext-2", Ambient: true,
		CreatedAt: now, UpdatedAt: now,
	}))
	later := now.Add(time.Minute)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01LINKED", Name: "sess/linked", ProjectSlug: "example-finance", Status: core.SessionActive,
		Findings:       "the link's own findings",
		ExternalClient: "codex", ExternalSessionID: "ext-2",
		CreatedAt: later, UpdatedAt: later,
	}))

	updated, err := UpdateAmbientFindings(ctx, db, "codex", "ext-2", "the ambient row's turn", now)
	require.NoError(t, err)
	require.True(t, updated)

	_, project, _, _, _, body, found := ftsRow(t, db, "01AMBLNK")
	require.True(t, found, "the ambient row's findings must reach the search mirror")
	require.Equal(t, "seam", project)
	require.Equal(t, "the ambient row's turn", body)
}

// TestMigration024_BackfillsTheWorkRecord upgrades a database whose tasks,
// trials, and sessions predate the work-record mirror and verifies they become
// searchable -- the half of the change that no writer hook can cover.
func TestMigration024_BackfillsTheWorkRecord(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	dsn := "file:" + url.PathEscape(dbPath) + "?_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Cut the list at the migration under test rather than at its end, so
	// appending later migrations does not silently re-point this test.
	all := migrationList()
	before := 0
	for i, m := range all {
		if m.Version == 24 {
			before = i
			break
		}
	}
	require.NotZero(t, before, "migration 24 must exist and must not be the first")
	require.NoError(t, migrate(db, all[:before]))

	const ts = "2026-07-01T00:00:00Z"
	_, err = db.Exec(`
		INSERT INTO tasks (id, project_slug, title, body, status, created_by, plan_slug,
		                   created_at, updated_at)
		VALUES ('01OLDTASK', 'seam', 'old queue item', 'the vulncheck step needs the module cache',
		        'done', '', 'search-visibility', ?, ?)`, ts, ts)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO trials (id, lab, title, changes, expected, actual, outcome,
		                    metrics, session_id, project_slug, created_at)
		VALUES ('01OLDTRIAL', 'boot-race', 'gate probe', '', 'healthy first',
		        'raced anyway', 'fail', '{}', '', 'seam', ?)`, ts)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO sessions (id, name, project_slug, status, findings, created_at, updated_at)
		VALUES ('01OLDSESS', 'cc/old', 'seam', 'completed', 'the readiness gate fixed it', ?, ?)`, ts, ts)
	require.NoError(t, err)
	// Two sessions that say nothing must stay out of the corpus.
	_, err = db.Exec(`
		INSERT INTO sessions (id, name, project_slug, status, findings, created_at, updated_at)
		VALUES ('01BLANK', 'cc/blank', 'seam', 'completed', '', ?, ?),
		       ('01AUTO', 'cc/auto', 'seam', 'completed', ?, ?, ?)`,
		ts, ts, core.FindingNoSummary, ts, ts)
	require.NoError(t, err)

	require.NoError(t, migrate(db, all))

	ctx := context.Background()
	for _, tc := range []struct{ query, kind, wantID string }{
		{"vulncheck module cache", core.ItemKindTask, "01OLDTASK"},
		{"raced anyway", core.ItemKindTrial, "01OLDTRIAL"},
		{"readiness gate", core.ItemKindSession, "01OLDSESS"},
	} {
		hits, err := FTSSearch(ctx, db, tc.query, []string{tc.kind}, nil, 5)
		require.NoError(t, err, tc.query)
		require.Len(t, hits, 1, "backfilled %s must be searchable", tc.kind)
		require.Equal(t, tc.wantID, hits[0].ItemID)
	}

	_, _, _, name, _, _, found := ftsRow(t, db, "01OLDTASK")
	require.True(t, found)
	require.Equal(t, "search-visibility", name, "the backfill must map plan_slug like IndexTaskFTS does")

	_, _, _, _, _, _, found = ftsRow(t, db, "01BLANK")
	require.False(t, found)
	_, _, _, _, _, _, found = ftsRow(t, db, "01AUTO")
	require.False(t, found)

	// Migration 024 deletes before it inserts, so a DB where a newer binary
	// already indexed some rows does not end up with duplicates.
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM fts WHERE item_id = '01OLDTASK'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestSessionFindingsIndexable(t *testing.T) {
	require.False(t, SessionFindingsIndexable(""))
	require.False(t, SessionFindingsIndexable("   \n "))
	require.False(t, SessionFindingsIndexable(core.FindingNoSummary))
	require.True(t, SessionFindingsIndexable("we ruled out the driver"))
}

// TestReindexWorkFTS_RebuildsAndSkipsMissingIDs covers the merge-import shape:
// rows land in the tables by bulk INSERT (nothing calls the per-writer index
// hooks), so the mirror has to be rebuilt by id afterwards. Ids with no row are
// skipped rather than failing the whole reindex, because INSERT OR IGNORE means
// the caller's id list is a superset of what actually landed.
func TestReindexWorkFTS_RebuildsAndSkipsMissingIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateTask(ctx, db, core.Task{
		ID: "01RTASK", ProjectSlug: "seam", Title: "reindex the work record",
		Body: "the merge import inserts rows behind the writers", Status: core.TaskOpen,
		PlanSlug: "archives", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, CreateTrial(ctx, db, core.Trial{
		ID: "01RTRIAL", Lab: "vacuum-into", Title: "bound parameter form",
		Expected: "a snapshot file appears", Actual: "it did",
		Outcome: core.OutcomePass, ProjectSlug: "seam", CreatedAt: now,
	}))
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01RSESS", Name: "cc/reindex", ProjectSlug: "seam",
		Status: core.SessionCompleted, Findings: "export vacuums before it walks the trees",
		CreatedAt: now, UpdatedAt: now,
	}))
	// A session with nothing worth indexing must stay out of the corpus even
	// though its id is handed to the reindex.
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01RQUIET", Name: "cc/quiet", ProjectSlug: "seam",
		Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now,
	}))

	// Wipe the mirror: this is the state a bulk row INSERT leaves behind.
	_, err := db.ExecContext(ctx, `DELETE FROM fts`)
	require.NoError(t, err)
	for _, id := range []string{"01RTASK", "01RTRIAL", "01RSESS"} {
		_, _, _, _, _, _, found := ftsRow(t, db, id)
		require.Falsef(t, found, "%s should be unindexed before the reindex", id)
	}

	require.NoError(t, ReindexWorkFTS(ctx, db,
		[]string{"01RTASK", "01MISSINGTASK"},
		[]string{"01RTRIAL", "01MISSINGTRIAL"},
		[]string{"01RSESS", "01RQUIET", "01MISSINGSESS"},
	))

	kind, project, title, name, _, body, found := ftsRow(t, db, "01RTASK")
	require.True(t, found)
	require.Equal(t, core.ItemKindTask, kind)
	require.Equal(t, "seam", project)
	require.Equal(t, "reindex the work record", title)
	require.Equal(t, "archives", name, "the plan slug still rides in name")
	require.Contains(t, body, "behind the writers")

	kind, _, title, name, description, body, found := ftsRow(t, db, "01RTRIAL")
	require.True(t, found)
	require.Equal(t, core.ItemKindTrial, kind)
	require.Equal(t, "bound parameter form", title)
	require.Equal(t, "vacuum-into", name)
	require.Equal(t, "pass", description)
	require.Contains(t, body, "a snapshot file appears")

	kind, _, title, _, _, body, found = ftsRow(t, db, "01RSESS")
	require.True(t, found)
	require.Equal(t, core.ItemKindSession, kind)
	require.Equal(t, "cc/reindex", title)
	require.Equal(t, "export vacuums before it walks the trees", body)

	_, _, _, _, _, _, found = ftsRow(t, db, "01RQUIET")
	require.False(t, found, "a session with no findings must not be indexed")

	for _, id := range []string{"01MISSINGTASK", "01MISSINGTRIAL", "01MISSINGSESS"} {
		_, _, _, _, _, _, found := ftsRow(t, db, id)
		require.Falsef(t, found, "%s has no row and must not be indexed", id)
	}

	// Empty id lists are a no-op, not an error: a merge that imported only
	// memories still calls this.
	require.NoError(t, ReindexWorkFTS(ctx, db, nil, nil, nil))

	// Re-running is idempotent -- the mirror is rebuildable, so a second pass
	// must leave exactly one row per item.
	require.NoError(t, ReindexWorkFTS(ctx, db, []string{"01RTASK"}, []string{"01RTRIAL"}, []string{"01RSESS"}))
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fts WHERE item_id = ?`, "01RTASK").Scan(&n))
	require.Equal(t, 1, n)
}
