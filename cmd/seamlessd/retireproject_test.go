package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func openRetireTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(context.Background(), db, "alpha"))
	return db
}

func requireRetired(t *testing.T, db *sql.DB, slug string, want bool) core.Project {
	t.Helper()
	p, ok, err := store.ProjectBySlug(context.Background(), db, slug)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, p.Retired())
	return p
}

func TestRetireProject_UnknownSlug(t *testing.T) {
	db := openRetireTestDB(t)
	var out bytes.Buffer
	err := retireProject(context.Background(), db, &out, "nope", false, false, time.Now())
	require.ErrorIs(t, err, errProjectNotFound)
	err = retireProject(context.Background(), db, &out, "nope", true, false, time.Now())
	require.ErrorIs(t, err, errProjectNotFound)
}

func TestRetireProject_RefusesWhileInUse(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ts := core.FormatTime(now)

	// Another host's repo registers "app"; this machine maps a path to it too.
	_, _, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
		Host: "beta", CWD: "/elsewhere/app", RepoRoot: "/elsewhere/app",
	}, "alpha")
	require.NoError(t, err)
	local := t.TempDir()
	require.NoError(t, store.AddRepoMapping(ctx, db, local, "app"))

	// One active memory and an archived one; one note; one open task and a done one.
	for _, m := range []struct {
		id  string
		inv any
	}{{"01MEMLIVE", nil}, {"01MEMGONE", ts}} {
		_, err = db.ExecContext(ctx, `
			INSERT INTO memories_index
			    (id, kind, name, description, project, file_path, tags, valid_from,
			     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
			VALUES (?, 'gotcha', ?, 'd', 'app', ?, '[]', ?, ?, NULL, '', 'h', ?, ?)`,
			m.id, m.id, "memory/app/"+m.id+".md", ts, m.inv, ts, ts)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO notes_index
		    (id, title, slug, description, project, file_path, tags, source_url,
		     content_hash, created_at, updated_at)
		VALUES ('01NOTE', 't', 'stray', '', 'app', 'notes/app/stray.md', '[]', '', 'h', ?, ?)`, ts, ts)
	require.NoError(t, err)
	for id, status := range map[string]core.TaskStatus{"01TASKOPEN": core.TaskOpen, "01TASKDONE": core.TaskDone} {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "app", Title: id, Status: status, CreatedAt: now, UpdatedAt: now,
		}))
	}

	// Dry-run or not, every blocker is printed and nothing changes.
	for _, dry := range []bool{true, false} {
		var out bytes.Buffer
		err = retireProject(ctx, db, &out, "app", false, dry, now)
		require.ErrorIs(t, err, errProjectInUse)
		got := out.String()
		require.Contains(t, got, "seamlessd unmap-repo --path "+local)
		require.Contains(t, got, "seamlessd unmap-repo --host beta --path /elsewhere/app")
		require.Contains(t, got, "1 active memory, 1 note, 1 open task")
		requireRetired(t, db, "app", false)
	}
}

// The remedy printed for another host's mapping names a --host/--path pair that
// unmap-repo accepts and finds, whichever OS this daemon runs and whichever OS
// the client does: each path is shown as the client sent it, and resolves as
// shown.
func TestRetireProject_RemoteRemediesResolve(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	sent := map[string]string{
		"beta":   "/elsewhere/app",
		"winbox": `C:\repos\app`,
		"uncbox": `\\srv\share\app`,
	}
	for _, host := range []string{"beta", "winbox", "uncbox"} {
		slug, _, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
			Host: host, CWD: sent[host], RepoRoot: sent[host],
		}, "alpha")
		require.NoError(t, err)
		require.Equal(t, "app", slug, "the same repo on another device joins the project")
	}

	var out bytes.Buffer
	err := retireProject(ctx, db, &out, "app", false, true, time.Now())
	require.ErrorIs(t, err, errProjectInUse)
	rows, err := store.RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, len(sent))
	for _, r := range rows {
		require.Equal(t, sent[r.Host], r.Path, "stored as the client sent it")
		require.Contains(t, out.String(), "seamlessd unmap-repo --host "+r.Host+" --path "+r.Path+"`")
		targets, err := unmapTargets(ctx, db, r.Path, false, r.Host)
		require.NoError(t, err, r.Path)
		require.Len(t, targets, 1)
		require.Equal(t, r.Host, targets[0].Host)
	}
}

func TestRetireProject_RetireAndUndo(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	_, err := store.EnsureProject(ctx, db, "app-2", "app-2")
	require.NoError(t, err)
	// Sessions are history, not content: they never block a retire.
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01SESS", Name: "cc/s", ProjectSlug: "app-2", Status: core.SessionCompleted,
		CreatedAt: now, UpdatedAt: now,
	}))

	run := func(undo, dry bool) string {
		t.Helper()
		var out bytes.Buffer
		require.NoError(t, retireProject(ctx, db, &out, "app-2", undo, dry, now))
		return out.String()
	}

	require.Contains(t, run(false, true), "would retire")
	requireRetired(t, db, "app-2", false)

	require.Contains(t, run(false, false), `retired project "app-2"`)
	p := requireRetired(t, db, "app-2", true)
	require.True(t, p.RetiredAt.Equal(now))

	// Idempotent: a second retire keeps the original stamp.
	require.Contains(t, run(false, false), "already retired")
	p = requireRetired(t, db, "app-2", true)
	require.True(t, p.RetiredAt.Equal(now))

	require.Contains(t, run(true, true), "would un-retire")
	requireRetired(t, db, "app-2", true)

	require.Contains(t, run(true, false), `un-retired project "app-2"`)
	requireRetired(t, db, "app-2", false)
	require.Contains(t, run(true, false), "is not retired")
}
