package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Synthetic commit ids: a SHA-1-length and a SHA-256-length one per digit.
func sha1Of(c byte) string   { return strings.Repeat(string(c), 40) }
func sha256Of(c byte) string { return strings.Repeat(string(c), 64) }

func commitsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func commitSession(t *testing.T, db *sql.DB, name string, status core.SessionStatus, created, updated time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: "demo", Status: status, CWD: "/work/demo",
		CreatedAt: created, UpdatedAt: updated,
	}))
	return id
}

func commitTask(t *testing.T, db *sql.DB, title string, status core.TaskStatus, at time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "demo", Title: title, Status: status, CreatedAt: at, UpdatedAt: at,
	}))
	return id
}

// A session's record grows by the ids it does not hold yet, in the order they
// were recorded; an invalid id or an unknown session records nothing.
func TestRecordSessionCommits_DedupesInOrderAndRefusesWhole(t *testing.T) {
	ctx := context.Background()
	db := commitsDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sess := commitSession(t, db, "cc/aaaa1111", core.SessionActive, t0, t0)

	added, err := store.RecordSessionCommits(ctx, db, sess, sess, []string{sha1Of('b'), sha256Of('a')}, t0)
	require.NoError(t, err)
	require.Equal(t, 2, added)
	added, err = store.RecordSessionCommits(ctx, db, sess, sess, []string{sha256Of('a'), sha1Of('c')}, t0.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, added, "a commit the session already holds is a no-op")

	got, err := store.RecordedCommits(ctx, db, store.CommitOwnerSession, []string{sess, "01UNKNOWNUNKNOWNUNKNOWNUNK"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{sess: {sha256Of('a'), sha1Of('b'), sha1Of('c')}}, got,
		"oldest record first; one instant orders by id; an owner with none is absent")

	for _, bad := range []string{strings.ToUpper(sha1Of('d')), "abc1234", sha1Of('g'), sha1Of('d') + "0"} {
		_, err = store.RecordSessionCommits(ctx, db, sess, sess, []string{sha1Of('e'), bad}, t0)
		require.ErrorIs(t, err, store.ErrInvalidCommitID, "%q", bad)
	}
	_, err = store.RecordSessionCommits(ctx, db, "01UNKNOWNUNKNOWNUNKNOWNUNK", "", []string{sha1Of('e')}, t0)
	require.ErrorIs(t, err, store.ErrSessionNotFound)

	got, err = store.RecordedCommits(ctx, db, store.CommitOwnerSession, []string{sess})
	require.NoError(t, err)
	require.Len(t, got[sess], 3, "a refused call recorded nothing, its valid ids included")
	tasks, err := store.RecordedCommits(ctx, db, store.CommitOwnerTask, []string{sess})
	require.NoError(t, err)
	require.Empty(t, tasks, "the owner kind is part of the key")
}

// An owner never holds more than MaxRecordedCommits: the call that would pass
// the bound records nothing, while ids it already holds still read as no-ops.
func TestRecordSessionCommits_BoundIsTheOwnersTotal(t *testing.T) {
	ctx := context.Background()
	db := commitsDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sess := commitSession(t, db, "cc/bbbb2222", core.SessionActive, t0, t0)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for i := range store.MaxRecordedCommits {
		_, err := tx.ExecContext(ctx, `INSERT INTO commit_records (owner_kind, owner_id, commit_id, recorded_by, recorded_at)
			VALUES ('session', ?, ?, '', ?)`, sess, fmt.Sprintf("%040x", i), core.FormatTime(t0))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	_, err = store.RecordSessionCommits(ctx, db, sess, sess, []string{fmt.Sprintf("%040x", 1), sha1Of('f')}, t0)
	require.ErrorIs(t, err, store.ErrTooManyCommits)
	added, err := store.RecordSessionCommits(ctx, db, sess, sess, []string{fmt.Sprintf("%040x", 7)}, t0)
	require.NoError(t, err, "an id already held adds nothing, so it fits")
	require.Zero(t, added)

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commit_records WHERE owner_id = ?`, sess).Scan(&n))
	require.Equal(t, store.MaxRecordedCommits, n)
}

// A task's commits go through UpdateTask: the same transaction and the same
// holder lock as every other change, so a task another session holds refuses
// them, and a refused patch applies none of its parts.
func TestUpdateTask_RecordsCommitsBehindTheHolderLock(t *testing.T) {
	ctx := context.Background()
	db := commitsDB(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	holder := commitSession(t, db, "cc/cccc3333", core.SessionActive, t0, t0)
	other := commitSession(t, db, "cc/dddd4444", core.SessionActive, t0, t0)
	task := commitTask(t, db, "ship it", core.TaskOpen, t0)
	_, err := store.ClaimTask(ctx, db, task, holder, time.Hour, t0)
	require.NoError(t, err)

	_, err = store.UpdateTask(ctx, db, task, store.TaskPatch{AddCommits: []string{sha1Of('a')}}, other, t0.Add(time.Minute))
	require.ErrorIs(t, err, store.ErrTaskClaimConflict, "a task another session holds refuses its commits")

	title := "renamed"
	_, err = store.UpdateTask(ctx, db, task, store.TaskPatch{Title: &title, AddCommits: []string{"ABC"}}, holder, t0.Add(time.Minute))
	require.ErrorIs(t, err, store.ErrInvalidCommitID)
	same, err := store.TaskByID(ctx, db, task)
	require.NoError(t, err)
	require.Equal(t, "ship it", same.Title, "a refused patch applies none of its parts")

	at := t0.Add(2 * time.Minute)
	done := core.TaskDone
	updated, err := store.UpdateTask(ctx, db, task, store.TaskPatch{Status: &done, AddCommits: []string{sha1Of('a'), sha256Of('b')}}, holder, at)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, updated.Status)
	require.True(t, updated.UpdatedAt.Equal(at), "recording commits is a change of the task")

	got, err := store.RecordedCommits(ctx, db, store.CommitOwnerTask, []string{task})
	require.NoError(t, err)
	require.Equal(t, []string{sha1Of('a'), sha256Of('b')}, got[task])
	var by string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT recorded_by FROM commit_records WHERE owner_id = ? LIMIT 1`, task).Scan(&by))
	require.Equal(t, holder, by, "a task's record names the acting session")

	// Closed and unclaimed now: anyone who may update it may add a commit.
	_, err = store.UpdateTask(ctx, db, task, store.TaskPatch{AddCommits: []string{sha1Of('c')}}, "", at.Add(time.Minute))
	require.NoError(t, err)
	got, err = store.RecordedCommits(ctx, db, store.CommitOwnerTask, []string{task})
	require.NoError(t, err)
	require.Len(t, got[task], 3)
}

// The snapshot's reads: a session belongs to the window when it was live in
// it, and a task when it is open or changed in it.
func TestFeedSessionsAndTasks_Window(t *testing.T) {
	ctx := context.Background()
	db := commitsDB(t)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(30 * 24 * time.Hour)

	_, ok, err := store.EarliestSessionStart(ctx, db)
	require.NoError(t, err)
	require.False(t, ok, "no session: the record has not begun")

	endedBefore := commitSession(t, db, "cc/e1", core.SessionCompleted, since.Add(-72*time.Hour), since.Add(-time.Hour))
	endedInside := commitSession(t, db, "cc/e2", core.SessionCompleted, since.Add(-time.Hour), since.Add(time.Hour))
	openLong := commitSession(t, db, "cc/e3", core.SessionActive, since.Add(-48*time.Hour), since.Add(-24*time.Hour))
	expiredInside := commitSession(t, db, "cc/e4", core.SessionExpired, since.Add(2*time.Hour), since.Add(3*time.Hour))
	startedAfter := commitSession(t, db, "cc/e5", core.SessionActive, until.Add(time.Hour), until.Add(time.Hour))

	sessions, err := store.FeedSessions(ctx, db, since, until)
	require.NoError(t, err)
	var ids []string
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	require.Equal(t, []string{openLong, endedInside, expiredInside}, ids, "oldest first; live in the window only")
	require.True(t, sessions[0].Active)
	require.False(t, sessions[2].Active, "an expired session has ended")
	require.Equal(t, "/work/demo", sessions[1].CWD)
	require.NotContains(t, ids, endedBefore)
	require.NotContains(t, ids, startedAfter)

	first, ok, err := store.EarliestSessionStart(ctx, db)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, first.Equal(since.Add(-72*time.Hour)))

	oldOpen := commitTask(t, db, "backlog", core.TaskOpen, since.Add(-90*24*time.Hour))
	oldDone := commitTask(t, db, "long done", core.TaskDone, since.Add(-90*24*time.Hour))
	newDone := commitTask(t, db, "just done", core.TaskDone, since.Add(time.Hour))
	tasks, err := store.FeedTasks(ctx, db, since, 0)
	require.NoError(t, err)
	ids = nil
	for _, x := range tasks {
		ids = append(ids, x.ID)
	}
	require.Equal(t, []string{newDone, oldOpen}, ids, "most recently changed first")
	require.NotContains(t, ids, oldDone)
	tasks, err = store.FeedTasks(ctx, db, since, 1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, newDone, tasks[0].ID)
}

// The feed's and the snapshot's reads carry the restricted account behind a
// session or a task, and "" for the owner's: the account that started a
// session, the one that created a task or wrote its text, and the one whose
// session recorded a commit on it.
func TestFeedReads_CarryTheAccount(t *testing.T) {
	ctx := context.Background()
	db := commitsDB(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	session := func(name, account string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: "demo",
			Status: core.SessionActive, Account: account, CreatedAt: at, UpdatedAt: at}))
		return id
	}
	agent := session("cc/a1", "_example_agent")
	peer := session("cc/p1", "_example_peer")
	owner := session("cc/o1", "")

	procs, err := store.SessionAgentProcesses(ctx, db, []string{agent, owner})
	require.NoError(t, err)
	require.Equal(t, store.SessionProcess{Name: "_example_agent:cc/a1", Account: "_example_agent"}, procs[agent])
	require.Equal(t, store.SessionProcess{Name: "cc/o1"}, procs[owner])

	sessions, err := store.FeedSessions(ctx, db, at.Add(-time.Hour), at.Add(time.Hour))
	require.NoError(t, err)
	accounts := map[string]string{}
	for _, s := range sessions {
		accounts[s.ID] = s.Account
	}
	require.Equal(t, map[string]string{agent: "_example_agent", peer: "_example_peer", owner: ""}, accounts)

	filed := commitTask(t, db, "filed", core.TaskOpen, at)
	_, err = db.ExecContext(ctx, `UPDATE tasks SET account = ?, updated_account = ? WHERE id = ?`, "_example_agent", "_example_peer", filed)
	require.NoError(t, err)
	mixed := commitTask(t, db, "mixed recorders", core.TaskOpen, at)
	ownerOnly := commitTask(t, db, "the owner's", core.TaskOpen, at)
	noSession := commitTask(t, db, "recorded with no session", core.TaskOpen, at)
	for _, rec := range []struct {
		task, by string
		commit   string
		at       time.Time
	}{
		{mixed, owner, sha1Of('1'), at},
		{mixed, peer, sha1Of('2'), at.Add(time.Minute)},
		{mixed, agent, sha1Of('3'), at.Add(2 * time.Minute)},
		{ownerOnly, owner, sha1Of('4'), at},
		{noSession, "", sha1Of('5'), at},
	} {
		_, err := store.UpdateTask(ctx, db, rec.task, store.TaskPatch{AddCommits: []string{rec.commit}}, rec.by, rec.at)
		require.NoError(t, err)
	}

	tasks, err := store.FeedTasks(ctx, db, at, 0)
	require.NoError(t, err)
	byID := map[string]store.FeedTask{}
	for _, x := range tasks {
		byID[x.ID] = x
	}
	require.Equal(t, "_example_agent", byID[filed].Account)
	require.Equal(t, "_example_peer", byID[filed].UpdatedAccount)
	require.Empty(t, byID[mixed].Account)
	require.Empty(t, byID[mixed].UpdatedAccount, "a recorded commit moves no text label")

	recorders, err := store.TaskCommitAccounts(ctx, db, []string{filed, mixed, ownerOnly, noSession})
	require.NoError(t, err)
	require.Equal(t, map[string]string{mixed: "_example_peer"}, recorders,
		"the earliest record a restricted account's session made; the owner's and a sessionless call's name none")
}
