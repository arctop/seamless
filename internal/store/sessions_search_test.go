package store

// The Sessions list's filters as one query (SessionFilter.where), the count
// beside its cap (CountSessionsWhere), and the claims a list of sessions holds
// (ClaimsHeldBy).

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// snSession inserts a session whose created and updated times are given.
func snSession(t *testing.T, db *sql.DB, name, project, findings string, status core.SessionStatus, created, updated time.Time) core.Session {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	s := core.Session{ID: id, Name: name, ProjectSlug: project, Status: status, Findings: findings,
		CreatedAt: created, UpdatedAt: updated}
	require.NoError(t, CreateSession(context.Background(), db, s))
	return s
}

func snIDs(sessions []core.Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}

// The search is the query's, not the page's: a session older than the newest
// 200 is found by a word of its findings, by its name, and by its id, and the
// count says how many the cap left out. Every term must appear.
func TestSnListSessionsWhere_SearchReachesPastTheCap(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-48 * time.Hour)
	oldest := snSession(t, db, "cc/oldest", "example-ops", "Found the **Kestrel** race in the watcher",
		core.SessionCompleted, base, base)
	for i := 1; i <= 204; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		snSession(t, db, fmt.Sprintf("cc/run-%03d", i), "example-ops", "routine work", core.SessionCompleted, at, at)
	}

	newest, err := ListSessionsWhere(ctx, db, SessionFilter{Limit: 200})
	require.NoError(t, err)
	require.Len(t, newest, 200)
	require.NotContains(t, snIDs(newest), oldest.ID, "the oldest is past the cap")
	n, err := CountSessionsWhere(ctx, db, SessionFilter{Limit: 200})
	require.NoError(t, err)
	require.Equal(t, 205, n, "the count is the cap's N")

	for _, q := range []string{"kestrel", "KESTREL watcher", "cc/oldest", oldest.ID[len(oldest.ID)-8:], "  kestrel  "} {
		f := SessionFilter{Query: q, Limit: 200}
		got, err := ListSessionsWhere(ctx, db, f)
		require.NoError(t, err, q)
		require.Equal(t, []string{oldest.ID}, snIDs(got), "%q finds the session past the cap", q)
		n, err := CountSessionsWhere(ctx, db, f)
		require.NoError(t, err)
		require.Equal(t, 1, n, q)
	}
	got, err := ListSessionsWhere(ctx, db, SessionFilter{Query: "kestrel nowhere", Limit: 200})
	require.NoError(t, err)
	require.Empty(t, got, "every term must appear")
	got, err = ListSessionsWhere(ctx, db, SessionFilter{Query: "example-ops", Limit: 500})
	require.NoError(t, err)
	require.Len(t, got, 205, "the project is searched too")
	require.Equal(t, []string{"kestrel", "watcher"}, SessionTerms("  Kestrel\tWATCHER "))
}

// The project, the day and the retained test are the query's too, and the
// count agrees with the list for each.
func TestSnListSessionsWhere_FiltersAreTheQuerys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	ops := snSession(t, db, "cc/ops", "example-ops", "wrote it down", core.SessionCompleted, day.Add(2*time.Hour), day.Add(3*time.Hour))
	opsBare := snSession(t, db, "cc/ops-bare", "example-ops", "", core.SessionCompleted, day.Add(-2*time.Hour), day.Add(time.Hour))
	opsNote := snSession(t, db, "cc/ops-note", "example-ops", "", core.SessionExpired, day.Add(5*time.Hour), day.Add(6*time.Hour))
	global := snSession(t, db, "cc/global", "", "", core.SessionCompleted, day.Add(26*time.Hour), day.Add(27*time.Hour))
	insertEvent(t, db, core.EventNoteWritten, "note-1", "{}", day.Add(5*time.Hour))
	_, err := db.ExecContext(ctx, `UPDATE events SET session_id = ? WHERE item_id = 'note-1'`, opsNote.ID)
	require.NoError(t, err)

	cases := []struct {
		name string
		f    SessionFilter
		want []string
	}{
		{"one project", SessionFilter{ByProject: true, Project: "example-ops"}, []string{opsNote.ID, ops.ID, opsBare.ID}},
		{"the global scope", SessionFilter{ByProject: true, Project: ""}, []string{global.ID}},
		{"no project filter", SessionFilter{Project: "example-ops"}, []string{global.ID, opsNote.ID, ops.ID, opsBare.ID}},
		{"created that day", SessionFilter{CreatedFrom: day, CreatedTo: day.AddDate(0, 0, 1)}, []string{opsNote.ID, ops.ID}},
		{"retained: findings or an artifact", SessionFilter{Retained: SessionRetainedYes}, []string{opsNote.ID, ops.ID}},
		{"retained nothing", SessionFilter{Retained: SessionRetainedNo}, []string{global.ID, opsBare.ID}},
		{"together", SessionFilter{ByProject: true, Project: "example-ops", Retained: SessionRetainedNo}, []string{opsBare.ID}},
	}
	for _, c := range cases {
		got, err := ListSessionsWhere(ctx, db, c.f)
		require.NoError(t, err, c.name)
		require.Equal(t, c.want, snIDs(got), c.name)
		n, err := CountSessionsWhere(ctx, db, c.f)
		require.NoError(t, err, c.name)
		require.Equal(t, len(c.want), n, c.name)
	}
}

// Through the console's filtered handle a locked project's sessions are absent
// from the search, the count and the project filter alike.
func TestSnListSessionsWhere_LockedProjectIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	snSession(t, db, "cc/ledger", "example-finance", "closed the LEDGER-7Q books", core.SessionCompleted, now, now)
	open := snSession(t, db, "cc/ops", "example-ops", "ran the ops runbook", core.SessionCompleted, now, now)
	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })

	for _, f := range []SessionFilter{{Query: "ledger-7q"}, {ByProject: true, Project: "example-finance"}} {
		got, err := ListSessionsWhere(ctx, db, f)
		require.NoError(t, err)
		require.Len(t, got, 1, "the main handle has it")
		got, err = ListSessionsWhere(ctx, view, f)
		require.NoError(t, err)
		require.Empty(t, got, "the view does not")
		n, err := CountSessionsWhere(ctx, view, f)
		require.NoError(t, err)
		require.Zero(t, n)
	}
	got, err := ListSessionsWhere(ctx, view, SessionFilter{})
	require.NoError(t, err)
	require.Equal(t, []string{open.ID}, snIDs(got))
}

// ClaimsHeldBy reads a whole list's in-progress claims in one statement: each
// session's, oldest-created first, a lapsed lease included for the caller to
// judge, a closed task and an id that is no ULID left out, and a locked
// project's tasks absent through the view.
func TestSnClaimsHeldBy_ReadsAListsClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	a := snSession(t, db, "cc/a", "example-ops", "", core.SessionActive, now, now)
	b := snSession(t, db, "cc/b", "example-ops", "", core.SessionActive, now, now)
	idle := snSession(t, db, "cc/idle", "example-ops", "", core.SessionActive, now, now)
	task := func(project, title string, created time.Time) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: title, Status: core.TaskOpen,
			CreatedBy: "test", CreatedAt: created, UpdatedAt: created}))
		return id
	}
	first := task("example-ops", "first", now.Add(-3*time.Hour))
	second := task("example-ops", "second", now.Add(-2*time.Hour))
	ledger := task("example-finance", "the ledger", now.Add(-time.Hour))
	done := task("example-ops", "done already", now.Add(-4*time.Hour))
	lapsed := task("example-ops", "lapsed", now.Add(-5*time.Hour))
	for _, c := range []struct {
		task, session string
		at            time.Time
	}{{second, a.ID, now}, {first, a.ID, now}, {ledger, b.ID, now}, {done, b.ID, now}, {lapsed, idle.ID, now.Add(-2 * time.Hour)}} {
		_, err := ClaimTask(ctx, db, c.task, c.session, time.Hour, c.at)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = ?`, done)
	require.NoError(t, err)

	got, err := ClaimsHeldBy(ctx, db, []string{a.ID, b.ID, idle.ID, a.ID, "cc/a", ""})
	require.NoError(t, err)
	titles := map[string][]string{}
	for id, tasks := range got {
		for _, tk := range tasks {
			titles[id] = append(titles[id], tk.Title)
		}
	}
	require.Equal(t, map[string][]string{a.ID: {"first", "second"}, b.ID: {"the ledger"}, idle.ID: {"lapsed"}}, titles)
	require.False(t, got[idle.ID][0].ClaimLive(now), "a lapsed lease is the caller's to judge")

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	got, err = ClaimsHeldBy(ctx, view, []string{a.ID, b.ID})
	require.NoError(t, err)
	require.Len(t, got[a.ID], 2)
	require.NotContains(t, got, b.ID, "the locked project's task is absent")
}

// SessionFirstPrompts reads each session's first prompt with words in it, of
// its whole life: a blank prompt and a later one are passed over, an injection
// carries one as a recall miss does, a payload that is not JSON holds none,
// and a locked project's session is absent through the view. Over one day it
// agrees with DiaryFirstPrompts.
func TestSnSessionFirstPrompts_FirstWithWordsPerSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	a := snSession(t, db, "cc/a", "example-ops", "", core.SessionActive, day, day)
	b := snSession(t, db, "cc/b", "example-ops", "", core.SessionCompleted, day, day)
	quiet := snSession(t, db, "cc/quiet", "example-ops", "", core.SessionCompleted, day, day)
	ledger := snSession(t, db, "cc/ledger", "example-finance", "", core.SessionActive, day, day)
	put := func(sess core.Session, kind core.EventKind, payload string, at time.Time) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
			VALUES (?, ?, ?, ?, ?, '', ?)`, id, core.FormatTime(at), string(kind), sess.ID, sess.ProjectSlug, payload)
		require.NoError(t, err)
	}
	put(a, core.EventToolCall, `{"tool":"recall","prompt":"not a prompt event"}`, day.Add(time.Minute))
	put(a, core.EventHookPrompt, `{"prompt":"   "}`, day.Add(2*time.Minute))
	put(a, core.EventHookPrompt, `{"prompt":"fix the watcher"}`, day.Add(3*time.Minute))
	put(a, core.EventHookPrompt, `{"prompt":"and then this"}`, day.Add(4*time.Minute))
	put(b, core.EventHookPrompt, `not json "prompt": at all`, day.Add(time.Minute))
	put(b, core.EventInjected, `{"prompt":"why is it slow","item_ids":["m1"]}`, day.Add(2*time.Minute))
	put(quiet, core.EventInjected, `{"hook":"session-start"}`, day.Add(time.Minute))
	put(ledger, core.EventHookPrompt, `{"prompt":"close the LEDGER-7Q books"}`, day.Add(time.Minute))

	ids := []string{a.ID, b.ID, quiet.ID, ledger.ID, a.ID, ""}
	got, err := SessionFirstPrompts(ctx, db, ids)
	require.NoError(t, err)
	require.Equal(t, map[string]string{a.ID: "fix the watcher", b.ID: "why is it slow", ledger.ID: "close the LEDGER-7Q books"}, got)

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	got, err = SessionFirstPrompts(ctx, view, ids)
	require.NoError(t, err)
	require.NotContains(t, got, ledger.ID, "the locked project's prompt is not read")
	require.Len(t, got, 2)

	// The Diary's read over the day finds the same for sessions whose first
	// prompt has words; it is not asked about b, whose payload that is not
	// JSON fails DiaryFirstPrompts' json_extract, nor about a, whose blank
	// first prompt it stops at.
	c := snSession(t, db, "cc/c", "example-ops", "", core.SessionCompleted, day, day)
	put(c, core.EventInjected, `{"prompt":"rotate the keys\nfirst","item_ids":[]}`, day.Add(time.Minute))
	put(c, core.EventHookPrompt, `{"prompt":"later"}`, day.Add(2*time.Minute))
	diary, err := DiaryFirstPrompts(ctx, db, day, day.Add(24*time.Hour), []string{c.ID, quiet.ID})
	require.NoError(t, err)
	mine, err := SessionFirstPrompts(ctx, db, []string{c.ID, quiet.ID})
	require.NoError(t, err)
	require.Equal(t, map[string]string{c.ID: "rotate the keys\nfirst"}, mine)
	require.Equal(t, diary, mine)
}
