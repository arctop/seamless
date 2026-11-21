package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// hrAt is the fixed clock the history tests write at.
var hrAt = time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)

// hrTask creates the owner's open task in project with body and returns its id.
func hrTask(t *testing.T, db *sql.DB, project, body string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateTask(context.Background(), db, core.Task{ID: id, ProjectSlug: project, Title: "task " + id,
		Body: body, Status: core.TaskOpen, CreatedAt: hrAt, UpdatedAt: hrAt}))
	return id
}

// hrRevisions is TaskBodyRevisions, required to succeed.
func hrRevisions(t *testing.T, db *sql.DB, id string) []core.TaskBodyRevision {
	t.Helper()
	revs, err := TaskBodyRevisions(context.Background(), db, id)
	require.NoError(t, err)
	return revs
}

// hrBody is the task's stored body.
func hrBody(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	task, err := TaskByID(context.Background(), db, id)
	require.NoError(t, err)
	return task.Body
}

func hrStr(s string) *string { return &s }

// A replaced body is kept, newest first, with when, how and by which session
// the change was made; the task carries the new body.
func TestTaskBodyRevisions_AReplacedBodyIsKept(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := hrTask(t, db, "demo", "the acceptance criteria")
	const session = "01M7Y7ETR8063NJMCQBN796D0K"

	_, err := UpdateTask(ctx, db, id, TaskPatch{Body: hrStr("findings only")}, session, hrAt.Add(time.Minute))
	require.NoError(t, err)
	_, err = UpdateTask(ctx, db, id, TaskPatch{Body: hrStr("the third body")}, "", hrAt.Add(2*time.Minute))
	require.NoError(t, err)

	require.Equal(t, []core.TaskBodyRevision{
		{TaskID: id, Revision: 2, Body: "findings only", Change: core.TaskBodyReplaced, ReplacedAt: hrAt.Add(2 * time.Minute)},
		{TaskID: id, Revision: 1, Body: "the acceptance criteria", Change: core.TaskBodyReplaced,
			ReplacedAt: hrAt.Add(time.Minute), ReplacedBy: session},
	}, hrRevisions(t, db, id))
	require.Equal(t, "the third body", hrBody(t, db, id))
	require.Empty(t, hrRevisions(t, db, "01NOSUCHTASK0000000000000"), "an id that names no task has none")
}

// Only a change of the stored body keeps a revision: a status, title, gate,
// dependency, commit or project change keeps none, nor does the same body sent
// again, nor replacing an empty body (there was no text to lose).
func TestTaskBodyRevisions_OnlyAChangedBodyKeepsOne(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, slug := range []string{"demo", "other"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	id := hrTask(t, db, "demo", "the body")
	blocker := hrTask(t, db, "demo", "")
	who, what := "owner", "sign off the rollout"
	for _, tc := range []struct {
		name  string
		patch TaskPatch
	}{
		{"a gate set", TaskPatch{Gate: &TaskGatePatch{Who: &who, What: &what}}},
		{"the gate cleared", TaskPatch{ClearGate: &TaskGateClear{By: "cc/agent", Note: "signed off"}}},
		{"a status move", TaskPatch{Status: statusPtr(core.TaskInProgress)}},
		{"a title change", TaskPatch{Title: hrStr("a new title")}},
		{"the same body again", TaskPatch{Body: hrStr("the body")}},
		{"a blocker added", TaskPatch{AddDependsOn: []string{blocker}}},
		{"the blocker removed", TaskPatch{RemoveDependsOn: []string{blocker}}},
		{"a commit recorded", TaskPatch{AddCommits: []string{strings.Repeat("ab", 20)}}},
		{"a project move", TaskPatch{ProjectSlug: hrStr("other")}},
		{"closed", TaskPatch{Status: statusPtr(core.TaskDone)}},
	} {
		_, err := UpdateTask(ctx, db, id, tc.patch, "", hrAt.Add(time.Minute))
		require.NoError(t, err, tc.name)
		require.Empty(t, hrRevisions(t, db, id), tc.name)
	}
	require.Equal(t, "the body", hrBody(t, db, id))

	empty := hrTask(t, db, "demo", "")
	_, err := UpdateTask(ctx, db, empty, TaskPatch{Body: hrStr("now some text")}, "", hrAt)
	require.NoError(t, err)
	require.Empty(t, hrRevisions(t, db, empty), "replacing an empty body keeps nothing")
	_, err = UpdateTask(ctx, db, empty, TaskPatch{Body: hrStr("")}, "", hrAt.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, []string{"now some text"}, hrBodies(hrRevisions(t, db, empty)), "blanking a body keeps it")
}

// An append adds notes_append's line to the body read inside the update's
// transaction and keeps the body before it as a revision. An empty body takes
// the line alone. A blank append, and an append beside a replacement, are
// refused and write nothing.
func TestTaskBodyAppend_KeepsTheBodyBeforeIt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := hrTask(t, db, "demo", "the acceptance criteria\n\n")

	res, err := UpdateTaskDetailed(ctx, db, id, TaskPatch{AppendBody: hrStr("found the race")}, "", hrAt)
	require.NoError(t, err)
	want := "the acceptance criteria\n\n2026-11-21 09:30 -- found the race\n"
	require.Equal(t, want, res.Task.Body)
	require.Equal(t, want, hrBody(t, db, id))
	require.Equal(t, []core.TaskBodyRevision{{TaskID: id, Revision: 1, Body: "the acceptance criteria\n\n",
		Change: core.TaskBodyAppended, ReplacedAt: hrAt}}, hrRevisions(t, db, id))

	_, err = UpdateTask(ctx, db, id, TaskPatch{AppendBody: hrStr("fixed it")}, "", hrAt.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, want+"\n2026-11-21 10:30 -- fixed it\n", hrBody(t, db, id))
	require.Len(t, hrRevisions(t, db, id), 2)

	empty := hrTask(t, db, "demo", "")
	_, err = UpdateTask(ctx, db, empty, TaskPatch{AppendBody: hrStr("first line")}, "", hrAt)
	require.NoError(t, err)
	require.Equal(t, "2026-11-21 09:30 -- first line\n", hrBody(t, db, empty), "an empty body takes the line alone")
	require.Empty(t, hrRevisions(t, db, empty))

	before := hrBody(t, db, id)
	for _, tc := range []struct {
		name  string
		patch TaskPatch
		want  error
	}{
		{"blank", TaskPatch{AppendBody: hrStr(" \n\t")}, ErrTaskBodyAppendBlank},
		{"empty", TaskPatch{AppendBody: hrStr(""), Status: statusPtr(core.TaskDone)}, ErrTaskBodyAppendBlank},
		{"beside a replacement", TaskPatch{Body: hrStr("new"), AppendBody: hrStr("more")}, ErrTaskBodyConflict},
	} {
		_, err := UpdateTask(ctx, db, id, tc.patch, "", hrAt.Add(2*time.Hour))
		require.ErrorIs(t, err, tc.want, tc.name)
		require.Equal(t, before, hrBody(t, db, id), tc.name)
		require.Len(t, hrRevisions(t, db, id), 2, tc.name)
	}
	task, err := TaskByID(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, task.Status, "a refused patch writes none of its other fields")
}

// Appends from many callers at once all land: each reads the body inside its
// own transaction, so none writes over another's line, and each keeps the body
// it extended as its own revision.
func TestTaskBodyAppend_ConcurrentAppendsLoseNothing(t *testing.T) {
	db := openTestDB(t)
	id := hrTask(t, db, "demo", "base line")
	const appenders = 8
	start := make(chan struct{})
	errs := make([]error, appenders)
	var wg sync.WaitGroup
	for i := range appenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = UpdateTask(context.Background(), db, id,
				TaskPatch{AppendBody: hrStr(fmt.Sprintf("finding-%02d", i))}, "", hrAt)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "appender %d", i)
	}
	body := hrBody(t, db, id)
	require.True(t, strings.HasPrefix(body, "base line\n\n"), body)
	for i := range appenders {
		require.Equal(t, 1, strings.Count(body, fmt.Sprintf("finding-%02d", i)), "append %d: %s", i, body)
	}
	revs := hrRevisions(t, db, id)
	require.Len(t, revs, appenders)
	for i, r := range revs {
		require.Equal(t, appenders-i, r.Revision, "numbered in the order the appends landed")
		require.Equal(t, core.TaskBodyAppended, r.Change)
		require.True(t, strings.HasPrefix(body, strings.TrimRight(r.Body, "\n")), "revision %d is a prefix of the body", r.Revision)
	}
}

// A task keeps TaskBodyRevisionsKept revisions. Past the cap the oldest one an
// append made goes first, since the body after it still holds its text, then
// the oldest replaced; the newest is never removed, and a number is never used
// twice.
func TestTaskBodyRevisions_RetentionKeepsTheNewestAndTheReplacedFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	update := func(id string, p TaskPatch) {
		t.Helper()
		_, err := UpdateTask(ctx, db, id, p, "", hrAt)
		require.NoError(t, err)
	}

	t.Run("replacements: the newest are kept", func(t *testing.T) {
		id := hrTask(t, db, "demo", "body 0")
		for i := 1; i <= TaskBodyRevisionsKept+5; i++ {
			update(id, TaskPatch{Body: hrStr(fmt.Sprintf("body %d", i))})
		}
		revs := hrRevisions(t, db, id)
		require.Len(t, revs, TaskBodyRevisionsKept)
		require.Equal(t, TaskBodyRevisionsKept+5, revs[0].Revision)
		require.Equal(t, fmt.Sprintf("body %d", TaskBodyRevisionsKept+4), revs[0].Body, "the newest is the body just replaced")
		require.Equal(t, 6, revs[len(revs)-1].Revision, "the five oldest went")
	})

	t.Run("a body wiped, then appended to: the wiped body outlives the appends", func(t *testing.T) {
		id := hrTask(t, db, "demo", "the acceptance criteria")
		update(id, TaskPatch{Body: hrStr("a summary that wiped them")})
		for i := range TaskBodyRevisionsKept + 10 {
			update(id, TaskPatch{AppendBody: hrStr(fmt.Sprintf("progress %02d", i))})
		}
		revs := hrRevisions(t, db, id)
		require.Len(t, revs, TaskBodyRevisionsKept)
		oldest := revs[len(revs)-1]
		require.Equal(t, 1, oldest.Revision)
		require.Equal(t, core.TaskBodyReplaced, oldest.Change)
		require.Equal(t, "the acceptance criteria", oldest.Body, "the wiped body is kept")
		require.Equal(t, TaskBodyRevisionsKept+11, revs[0].Revision, "the newest append's revision")
		for _, r := range revs[:len(revs)-1] {
			require.Equal(t, core.TaskBodyAppended, r.Change)
		}
		body := hrBody(t, db, id)
		for i := range TaskBodyRevisionsKept + 10 {
			require.Contains(t, body, fmt.Sprintf("progress %02d", i), "no appended line is lost with its revision")
		}
	})

	t.Run("replacements, then an append: the newest stays and the oldest replaced goes", func(t *testing.T) {
		id := hrTask(t, db, "demo", "body 0")
		for i := 1; i <= TaskBodyRevisionsKept; i++ {
			update(id, TaskPatch{Body: hrStr(fmt.Sprintf("body %d", i))})
		}
		update(id, TaskPatch{AppendBody: hrStr("one more line")})
		revs := hrRevisions(t, db, id)
		require.Len(t, revs, TaskBodyRevisionsKept)
		require.Equal(t, TaskBodyRevisionsKept+1, revs[0].Revision)
		require.Equal(t, core.TaskBodyAppended, revs[0].Change, "the newest revision is never the one removed")
		require.Equal(t, 2, revs[len(revs)-1].Revision, "the oldest replaced went")
		update(id, TaskPatch{Body: hrStr("after the append")})
		revs = hrRevisions(t, db, id)
		require.Len(t, revs, TaskBodyRevisionsKept)
		require.Equal(t, TaskBodyRevisionsKept+2, revs[0].Revision, "numbers are never used twice")
		require.Equal(t, core.TaskBodyReplaced, revs[0].Change)
		require.Equal(t, fmt.Sprintf("body %d\n\n2026-11-21 09:30 -- one more line\n", TaskBodyRevisionsKept), revs[0].Body)
		require.Equal(t, TaskBodyRevisionsKept, revs[1].Revision,
			"the append's revision went first: the newest still holds its text")
		require.Equal(t, 2, revs[len(revs)-1].Revision, "and every replaced one stayed")
	})
}

// A revision carries the label the task's text had when it was replaced: the
// restricted account whose words it may hold. An account's append stamps the
// task's label; the owner's append, even beside a new title, never clears it.
func TestTaskBodyRevisions_CarryTheReplacedTextsLabel(t *testing.T) {
	db := openTestDB(t)
	owner := context.Background()
	acct := restrictedCtx(t, "example-open")
	const agent = "_example_agent"
	id := hrTask(t, db, "example-open", "the owner's body")
	label := func() string {
		t.Helper()
		task, err := TaskByID(owner, db, id)
		require.NoError(t, err)
		return task.UpdatedAccount
	}

	_, err := UpdateTask(acct, db, id, TaskPatch{Body: hrStr("the account's body")}, "", hrAt)
	require.NoError(t, err)
	require.Equal(t, agent, label())
	_, err = UpdateTask(owner, db, id, TaskPatch{Title: hrStr("the owner's title"), AppendBody: hrStr("the owner adds")}, "", hrAt)
	require.NoError(t, err)
	require.Equal(t, agent, label(), "the owner's append keeps every word the account wrote, so the label stays")
	_, err = UpdateTask(owner, db, id, TaskPatch{Title: hrStr("all the owner's"), Body: hrStr("all the owner's body")}, "", hrAt)
	require.NoError(t, err)
	require.Empty(t, label(), "the owner rewrote every field")
	_, err = UpdateTask(acct, db, id, TaskPatch{AppendBody: hrStr("the account adds")}, "", hrAt)
	require.NoError(t, err)
	require.Equal(t, agent, label(), "an account's append puts its words in the body")

	revs := hrRevisions(t, db, id)
	require.Len(t, revs, 4)
	for _, c := range []struct {
		body, account string
	}{
		{"all the owner's body", ""},
		{"the account's body\n\n2026-11-21 09:30 -- the owner adds\n", agent},
		{"the account's body", agent},
		{"the owner's body", ""},
	} {
		r := revs[0]
		revs = revs[1:]
		require.Equal(t, c.body, r.Body)
		require.Equal(t, c.account, r.UpdatedAccount, "revision %d: the label its text had", r.Revision)
	}
}

// The revisions follow their task through a move, and leave with its row.
func TestTaskBodyRevisions_FollowTheTask(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := hrTask(t, db, "demo", "first")
	_, err := UpdateTask(ctx, db, id, TaskPatch{Body: hrStr("second"), ProjectSlug: hrStr("other")}, "", hrAt)
	require.NoError(t, err)
	require.Equal(t, []string{"first"}, hrBodies(hrRevisions(t, db, id)), "a move and a body change in one patch")
	_, err = UpdateTask(ctx, db, id, TaskPatch{ProjectSlug: hrStr("demo")}, "", hrAt)
	require.NoError(t, err)
	require.Equal(t, []string{"first"}, hrBodies(hrRevisions(t, db, id)))

	_, err = db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_body_revisions WHERE task_id = ?`, id).Scan(&n))
	require.Zero(t, n, "a task's row removed by hand takes its revisions with it")

	_, err = db.ExecContext(ctx, `INSERT INTO task_body_revisions (task_id, revision, body, change, replaced_at)
		VALUES ('01NOSUCHTASK0000000000000', 1, 'x', 'replace', 'x')`)
	require.ErrorContains(t, err, "FOREIGN KEY", "a revision names a task that exists")
}

// The table takes exactly the canonical set of body changes: the CHECK in
// migration 054 is a transcription of core.TaskBodyChanges, held to it here.
func TestTaskBodyRevisions_ChangeIsTheCanonicalSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := hrTask(t, db, "demo", "")
	insert := func(revision int, change string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO task_body_revisions (task_id, revision, body, change, replaced_at)
			VALUES (?, ?, 'a body', ?, ?)`, id, revision, change, core.FormatTime(hrAt))
		return err
	}
	for i, c := range core.TaskBodyChanges {
		require.NoError(t, insert(i+1, string(c)), c)
	}
	require.ErrorContains(t, insert(len(core.TaskBodyChanges)+1, "rewrite"), "CHECK")
	require.ErrorContains(t, insert(0, string(core.TaskBodyReplaced)), "CHECK", "revisions count from 1")
	require.Len(t, hrRevisions(t, db, id), len(core.TaskBodyChanges))
}

func hrBodies(revs []core.TaskBodyRevision) []string {
	out := make([]string, len(revs))
	for i, r := range revs {
		out[i] = r.Body
	}
	return out
}
