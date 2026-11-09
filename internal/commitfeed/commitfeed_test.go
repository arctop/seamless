package commitfeed

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Synthetic commit ids.
var (
	commitA = strings.Repeat("a", 40)
	commitB = strings.Repeat("b", 64)
	commitC = strings.Repeat("c", 40)
)

// The instant every test takes its snapshot at.
var taken = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func feedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mapRepo(t *testing.T, db *sql.DB, host, path, slug, origin string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO repo_map (host, path, slug, origin, created_at)
		VALUES (?, ?, ?, ?, ?)`, host, path, slug, origin, core.FormatTime(taken))
	require.NoError(t, err)
}

func addSession(t *testing.T, db *sql.DB, name, project, cwd string, status core.SessionStatus, created, updated time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: status, CWD: cwd, Host: "h1",
		CreatedAt: created, UpdatedAt: updated,
	}))
	return id
}

func addTask(t *testing.T, db *sql.DB, project string, status core.TaskStatus, at time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, Title: "work " + id, Body: "a body the snapshot never carries",
		Status: status, CreatedAt: at, UpdatedAt: at,
	}))
	return id
}

// decode reads an encoded snapshot back as plain JSON, for asserting what a
// consumer sees (null versus absent included).
func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func encode(t *testing.T, snap Snapshot) []byte {
	t.Helper()
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	return raw
}

// Build lists every session live in the window under its name, with the
// repository its working directory is in -- unless its project, or the
// repository's, is fenced -- and every task a commit in the window may name,
// each with what it recorded; and nothing else of them.
func TestBuild_ListsTheWindowAndNamesOnlyWhatMayLeave(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	mapRepo(t, db, "h1", "/work/app", "app", "git@github.com:owner/App-Repo.git")
	mapRepo(t, db, "h1", "/work/plain", "plain", "")
	mapRepo(t, db, "h1", "/work/secret", "secret", "https://example.com/owner/secret-repo")
	mapRepo(t, db, "h1", "/work/two-a", "two", "https://example.com/owner/first.git")
	mapRepo(t, db, "h1", "/work/two-b", "two", "https://example.com/owner/second.git")
	_, err := store.EnsureProject(ctx, db, "secret", "secret")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "secret", core.IsolationConfidential))

	day := 24 * time.Hour
	active := addSession(t, db, "cc/aaaa-1111", "app", "/work/app/sub/dir", core.SessionActive, taken.Add(-2*day), taken.Add(-time.Hour))
	ended := addSession(t, db, "cx/bbbb-2222", "plain", "/work/plain", core.SessionCompleted, taken.Add(-3*day), taken.Add(-3*day+time.Hour))
	addSession(t, db, "sess/cccc3333", "secret", "/work/secret", core.SessionExpired, taken.Add(-day), taken.Add(-day+time.Minute))
	odd := addSession(t, db, "a name with spaces", "app", "/elsewhere", core.SessionCompleted, taken.Add(-day), taken.Add(-day))
	addSession(t, db, "cc/dddd-4444", "locked-proj", "/work/app", core.SessionActive, taken.Add(-time.Hour), taken.Add(-time.Minute))
	addSession(t, db, "cc/eeee-5555", "app", "/work/app", core.SessionCompleted, taken.Add(-90*day), taken.Add(-60*day))

	open := addTask(t, db, "two", core.TaskOpen, taken.Add(-200*day))
	done := addTask(t, db, "app", core.TaskOpen, taken.Add(-day))
	hidden := addTask(t, db, "secret", core.TaskInProgress, taken.Add(-day))
	addTask(t, db, "app", core.TaskDone, taken.Add(-100*day))

	_, err = store.RecordSessionCommits(ctx, db, active, active, []string{commitB, commitA}, taken.Add(-time.Hour))
	require.NoError(t, err)
	doneStatus := core.TaskDone
	_, err = store.UpdateTask(ctx, db, done, store.TaskPatch{Status: &doneStatus, AddCommits: []string{commitC}}, active, taken.Add(-30*time.Minute))
	require.NoError(t, err)

	snap, trim, err := Build(ctx, db, Options{
		Host: "host-1.example", Window: 30 * day, Now: taken,
		Locked: func(p string) bool { return p == "locked-proj" },
	})
	require.NoError(t, err)
	require.Equal(t, Trimmed{}, trim)
	raw := encode(t, snap)
	require.NoError(t, CheckJSON(raw, taken), "the snapshot passes the contract: %s", raw)
	for _, never := range []string{"body", "work ", "/work/", "secret-repo", "first", "second", "locked-proj", "a name with spaces"} {
		require.NotContains(t, string(raw), never, "identifiers and times only")
	}

	got := decode(t, raw)
	require.Equal(t, "seamless_feed", got["kind"])
	require.EqualValues(t, 1, got["version"])
	require.Equal(t, "host-1.example", got["host"])
	require.Equal(t, "2026-10-31T12:00:00.000Z", got["taken_at"])
	require.Equal(t, "2026-10-01T12:00:00.000Z", got["since"], "the window, since the record began earlier")

	sessions := got["sessions"].([]any)
	byID := map[string]map[string]any{}
	for _, x := range sessions {
		e := x.(map[string]any)
		byID[e["id"].(string)] = e
	}
	require.Len(t, byID, 5, "the session that ended before the window is left out")
	require.Equal(t, map[string]any{
		"id": "cc/aaaa-1111", "repo": "App-Repo", "started_at": "2026-10-29T12:00:00.000Z", "ended_at": nil,
		"commits": []any{commitA, commitB},
	}, byID["cc/aaaa-1111"], "named by its origin; open; its commits oldest first, one call's by id")
	require.Equal(t, map[string]any{
		"id": "cx/bbbb-2222", "repo": "plain", "started_at": "2026-10-28T12:00:00.000Z", "ended_at": "2026-10-28T13:00:00.000Z",
	}, byID["cx/bbbb-2222"], "no origin: named by its path; ended; no commits, no key")
	require.Nil(t, byID["sess/cccc3333"]["repo"], "a confidential project's repository is never named")
	require.Contains(t, byID["sess/cccc3333"], "repo", "repo is always present, null when withheld")
	require.Equal(t, "2026-10-30T12:01:00.000Z", byID["sess/cccc3333"]["ended_at"], "an expired session ended at its last activity")
	require.Contains(t, byID, odd, "a name outside the pattern: its ULID")
	require.Nil(t, byID[odd]["repo"], "a working directory no repository covers")
	require.Nil(t, byID["cc/dddd-4444"]["repo"], "a locked project: never named, even in a named repository")
	require.NotContains(t, byID, ended, "a named session is listed by its name, not its ULID")

	tasks := got["tasks"].([]any)
	require.Len(t, tasks, 3, "open, in-progress and recently changed; the long-closed one is left out")
	byTask := map[string]map[string]any{}
	for _, x := range tasks {
		e := x.(map[string]any)
		byTask[e["id"].(string)] = e
	}
	require.Equal(t, map[string]any{"id": done, "repo": "App-Repo", "commits": []any{commitC}}, byTask[done])
	require.Equal(t, map[string]any{"id": open}, byTask[open], "a project with two repositories names none")
	require.Equal(t, map[string]any{"id": hidden}, byTask[hidden], "a confidential project's task: no repository")
	t.Logf("snapshot: %s", raw)
}

// since never claims more than the record holds: with no session it is the
// moment the snapshot was taken, and a record younger than the window starts
// it at the first session.
func TestBuild_SinceStartsWithTheRecord(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	opts := Options{Host: "h", Window: 30 * 24 * time.Hour, Now: taken}

	snap, _, err := Build(ctx, db, opts)
	require.NoError(t, err)
	require.Equal(t, snap.TakenAt, snap.Since, "nothing recorded, nothing covered")
	raw := encode(t, snap)
	require.NoError(t, CheckJSON(raw, taken))
	require.Contains(t, string(raw), `"sessions":[],"tasks":[]`, "empty lists, never null")

	first := taken.Add(-50 * time.Hour).Add(123456789 * time.Nanosecond)
	addSession(t, db, "cc/ffff-6666", "", "", core.SessionCompleted, first, first.Add(time.Hour))
	snap, _, err = Build(ctx, db, opts)
	require.NoError(t, err)
	require.Equal(t, "2026-10-29T10:00:00.123Z", snap.Since, "the first session's start, to the millisecond")
	require.Len(t, snap.Sessions, 1)

	_, _, err = Build(ctx, db, Options{Host: "Not A Host", Window: time.Hour, Now: taken})
	require.ErrorContains(t, err, "lowercase host name")
	_, _, err = Build(ctx, db, Options{Host: "h", Now: taken})
	require.ErrorContains(t, err, "window")
}

// A recorded id that is not a full lowercase commit id (a row edited by hand:
// the record refuses one) is left out and counted, so the rest still reaches
// the consumer rather than the whole snapshot being refused.
func TestBuild_LeavesOutAnInvalidRecord(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	sess := addSession(t, db, "cc/abab-1212", "", "", core.SessionActive, taken.Add(-time.Hour), taken)
	_, err := db.ExecContext(ctx, `INSERT INTO commit_records (owner_kind, owner_id, commit_id, recorded_by, recorded_at)
		VALUES ('session', ?, ?, '', ?), ('session', ?, ?, '', ?)`,
		sess, strings.ToUpper(commitA), core.FormatTime(taken), sess, commitC, core.FormatTime(taken))
	require.NoError(t, err)
	snap, trim, err := Build(ctx, db, Options{Host: "h", Window: time.Hour, Now: taken})
	require.NoError(t, err)
	require.Equal(t, 1, trim.Commits)
	require.Equal(t, []string{commitC}, snap.Sessions[0].Commits)
	require.NoError(t, CheckJSON(encode(t, snap), taken))
}

// Past MaxSessions the earliest-ended sessions go, and since moves to the
// latest end among them: every session live after since is still listed.
func TestTrimSessions_MovesSincePastWhatItLeavesOut(t *testing.T) {
	since := taken.Add(-30 * 24 * time.Hour)
	sessions := make([]store.FeedSession, 0, MaxSessions+2)
	for i := range MaxSessions + 2 {
		at := since.Add(time.Duration(i) * time.Second)
		sessions = append(sessions, store.FeedSession{ID: fmt.Sprint("s", i), CreatedAt: at, UpdatedAt: at.Add(time.Minute)})
	}
	sessions[1].Active = true // its times say it ended early, but it is still open
	kept, moved, n := trimSessions(sessions, since, taken)
	require.Equal(t, 2, n)
	require.Len(t, kept, MaxSessions)
	require.Equal(t, []string{"s1", "s3"}, []string{kept[0].ID, kept[1].ID},
		"s0 and s2 ended earliest and go; the open s1 stays; order kept")
	require.Equal(t, sessions[2].UpdatedAt, moved, "since moves to the later end of the two left out")

	same, unmoved, none := trimSessions(sessions[:10], since, taken)
	require.Zero(t, none)
	require.Equal(t, since, unmoved)
	require.Len(t, same, 10)
}

// CheckJSON refuses what the consumer refuses, naming where.
func TestCheckJSON_RefusesWhatTheConsumerRefuses(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{
			"kind": "seamless_feed", "version": 1, "host": "h1", "taken_at": "2026-10-31T12:00:00.000Z",
			"since": "2026-10-01T12:00:00Z",
			"sessions": []any{map[string]any{
				"id": "cc/aaaa-1111", "repo": "app", "started_at": "2026-10-30T12:00:00.123456789Z", "ended_at": nil,
				"commits": []any{commitA, commitB},
			}},
			"tasks": []any{map[string]any{"id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "repo": nil}},
		}
	}
	raw, err := json.Marshal(good())
	require.NoError(t, err)
	require.NoError(t, CheckJSON(raw, taken))

	session := func(m map[string]any) map[string]any { return m["sessions"].([]any)[0].(map[string]any) }
	task := func(m map[string]any) map[string]any { return m["tasks"].([]any)[0].(map[string]any) }
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		where  string
	}{
		{"an unknown name", func(m map[string]any) { m["extra"] = 1 }, "snapshot: a name"},
		{"a missing list", func(m map[string]any) { delete(m, "tasks") }, "snapshot.tasks: missing"},
		{"another kind", func(m map[string]any) { m["kind"] = "commits" }, "snapshot.kind"},
		{"another version", func(m map[string]any) { m["version"] = 2 }, "snapshot.version"},
		{"an uppercase host", func(m map[string]any) { m["host"] = "Host" }, "snapshot.host"},
		{"taken in the future", func(m map[string]any) { m["taken_at"] = "2026-10-31T12:10:00Z" }, "snapshot.taken_at"},
		{"taken before 2020", func(m map[string]any) { m["taken_at"] = "2019-12-31T23:59:59Z" }, "snapshot.taken_at"},
		{"an offset time", func(m map[string]any) { m["since"] = "2026-10-01T12:00:00+02:00" }, "snapshot.since"},
		{"a date that does not exist", func(m map[string]any) { m["since"] = "2026-02-30T00:00:00Z" }, "snapshot.since"},
		{"null sessions", func(m map[string]any) { m["sessions"] = nil }, "snapshot.sessions"},
		{"a session without ended_at", func(m map[string]any) { delete(session(m), "ended_at") }, "ended_at: missing"},
		{"a session id with a space", func(m map[string]any) { session(m)["id"] = "cc/a b" }, "sessions[0].id"},
		{"a repository path", func(m map[string]any) { session(m)["repo"] = "owner/app" }, "sessions[0].repo"},
		{"an empty repository", func(m map[string]any) { session(m)["repo"] = "" }, "sessions[0].repo"},
		{"an uppercase commit", func(m map[string]any) { session(m)["commits"] = []any{strings.ToUpper(commitA)} }, "commits[0]"},
		{"an abbreviated commit", func(m map[string]any) { session(m)["commits"] = []any{"abc1234"} }, "commits[0]"},
		{"a commit twice", func(m map[string]any) { session(m)["commits"] = []any{commitA, commitA} }, "commits[1]: given twice"},
		{"commits as a string", func(m map[string]any) { session(m)["commits"] = commitA }, "commits: not an array"},
		{"a session twice", func(m map[string]any) {
			m["sessions"] = append(m["sessions"].([]any), session(m))
		}, "snapshot.sessions[1]: given twice"},
		{"a task id that is no ULID", func(m map[string]any) { task(m)["id"] = "task-1" }, "tasks[0].id"},
		{"a task with an unknown name", func(m map[string]any) { task(m)["title"] = "x" }, "tasks[0]: a name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := good()
			tc.mutate(m)
			raw, err := json.Marshal(m)
			require.NoError(t, err)
			err = CheckJSON(raw, taken)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.where)
		})
	}
	require.Error(t, CheckJSON([]byte(`{} {}`), taken), "one document")
}

// The writer replaces the file whole, at its mode, leaving nothing beside it.
func TestWriter_ReplacesTheFileWhole(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "snapshot-seamless_feed.json")
	now := taken
	w := NewWriter(WriterConfig{DB: db, File: file, Every: time.Minute, Window: time.Hour, Host: "h1",
		Now: func() time.Time { return now }})

	require.NoError(t, w.WriteOnce(ctx))
	first, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, CheckJSON(first, now))
	require.True(t, strings.HasSuffix(string(first), "}\n"), "one JSON document, one line")
	if runtime.GOOS != "windows" {
		st, err := os.Stat(file)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(FileMode), st.Mode().Perm())
	}

	addSession(t, db, "cc/1234-abcd", "", "", core.SessionActive, now.Add(-time.Minute), now)
	now = now.Add(time.Minute)
	require.NoError(t, w.WriteOnce(ctx))
	second, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NotEqual(t, string(first), string(second))
	require.Contains(t, string(second), `"id":"cc/1234-abcd"`)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is left beside it")

	missing := NewWriter(WriterConfig{DB: db, File: filepath.Join(dir, "absent", "s.json"), Every: time.Minute,
		Window: time.Hour, Host: "h1"})
	require.Error(t, missing.WriteOnce(ctx), "the writer does not create the directory")
}

// Run writes as it starts and stops with its context; it never starts without
// a file, and refuses a configuration it could not run.
func TestWriter_RunWritesAtStartAndStops(t *testing.T) {
	db := feedDB(t)
	idle := NewWriter(WriterConfig{DB: db})
	require.NoError(t, idle.Run(context.Background()), "no file: off")
	idle.Wait()

	for _, bad := range []WriterConfig{
		{DB: db, File: "/x.json", Window: time.Hour, Host: "h1"},
		{DB: db, File: "/x.json", Every: time.Minute, Host: "h1"},
		{DB: db, File: "/x.json", Every: time.Minute, Window: time.Hour, Host: "UPPER"},
	} {
		require.Error(t, NewWriter(bad).Run(context.Background()))
	}

	file := filepath.Join(t.TempDir(), "s.json")
	ctx, cancel := context.WithCancel(context.Background())
	w := NewWriter(WriterConfig{DB: db, File: file, Every: time.Hour, Window: time.Hour, Host: "h1"})
	require.NoError(t, w.Run(ctx))
	require.Eventually(t, func() bool {
		_, err := os.Stat(file)
		return err == nil
	}, 10*time.Second, 5*time.Millisecond, "the first snapshot is written at start")
	cancel()
	w.Wait()
}

// A session that started within the millisecond the snapshot is taken in is
// listed: only what is written is cut to the millisecond, never the query.
func TestBuild_ListsASessionStartedInTheSameMillisecond(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	start := taken.Add(500 * time.Microsecond)
	addSession(t, db, "cc/1a2b-3c4d", "", "", core.SessionActive, start, start)
	snap, _, err := Build(ctx, db, Options{Host: "h", Window: time.Hour, Now: taken.Add(700 * time.Microsecond)})
	require.NoError(t, err)
	require.Len(t, snap.Sessions, 1)
	require.Equal(t, "2026-10-31T12:00:00.000Z", snap.Sessions[0].StartedAt)
	require.Equal(t, snap.Sessions[0].StartedAt, snap.TakenAt)
	require.NoError(t, CheckJSON(encode(t, snap), taken))
}

// A session still marked active but idle past the liveness threshold has
// ended at its last activity, as the reaper would expire it: whether or not
// the reaper runs, a dead agent's session never reads as open forever.
func TestBuild_AnIdleSessionHasEnded(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	day := 24 * time.Hour
	addSession(t, db, "cc/0001-live", "", "", core.SessionActive, taken.Add(-day), taken.Add(-10*time.Minute))
	addSession(t, db, "cc/0002-dead", "", "", core.SessionActive, taken.Add(-day), taken.Add(-2*time.Hour))
	addSession(t, db, "cc/0003-gone", "", "", core.SessionActive, taken.Add(-10*day), taken.Add(-9*day))

	snap, _, err := Build(ctx, db, Options{Host: "h", Window: 2 * day, Now: taken, Idle: 45 * time.Minute})
	require.NoError(t, err)
	byID := map[string]Session{}
	for _, s := range snap.Sessions {
		byID[s.ID] = s
	}
	require.Len(t, byID, 2, "idle since before the window: it ended there")
	require.Nil(t, byID["cc/0001-live"].EndedAt, "within the threshold: still open")
	require.NotNil(t, byID["cc/0002-dead"].EndedAt)
	require.Equal(t, "2026-10-31T10:00:00.000Z", *byID["cc/0002-dead"].EndedAt, "ended at its last activity")
	require.NoError(t, CheckJSON(encode(t, snap), taken))

	trusting, _, err := Build(ctx, db, Options{Host: "h", Window: 2 * day, Now: taken})
	require.NoError(t, err)
	require.Len(t, trusting.Sessions, 3, "no threshold: the status alone decides")
}
