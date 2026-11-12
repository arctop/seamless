package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// restoreInstance is a served data dir (no peer_auth) whose database holds a
// project, a task and an indexed memory, with the config naming it.
type restoreInstance struct {
	dataDir string
	folder  string
}

func newRestoreInstance(t *testing.T) *restoreInstance {
	t.Helper()
	root := t.TempDir()
	in := &restoreInstance{dataDir: filepath.Join(root, "data"), folder: filepath.Join(root, "Seamless Backups")}
	cfgPath := filepath.Join(root, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(strings.Join([]string{
		"role: server",
		"data_dir: " + in.dataDir,
		"mcp:",
		"  api_key: restore-test-key",
		"",
	}, "\n")), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	t.Setenv("SEAMLESS_DATA_DIR", in.dataDir)
	t.Setenv("SEAMLESS_BACKUP_DIR", in.folder)
	t.Setenv("HOME", root)

	db := in.db(t)
	_, err := store.EnsureProject(context.Background(), db, "alpha", "Alpha")
	require.NoError(t, err)
	in.addTask(t, db, "01TASKSEED")
	in.writeMemory(t, "boot-race", "01MEMSEED")
	in.reconcile(t, db)
	require.NoError(t, db.Close())
	return in
}

func (in *restoreInstance) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(in.dataDir, archive.DBName))
	require.NoError(t, err)
	return db
}

func (in *restoreInstance) addTask(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "alpha", Title: "task " + id, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now,
	}))
}

func (in *restoreInstance) writeMemory(t *testing.T, name, id string) {
	t.Helper()
	p := filepath.Join(in.dataDir, archive.MemoryTree, "alpha", name+".md")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte("---\nid: "+id+"\nkind: gotcha\nname: "+name+"\ndescription: "+name+
		"\nproject: alpha\ncreated: 2026-11-10T03:00:00Z\nupdated: 2026-11-10T03:00:00Z\nvalid_from: 2026-11-10T03:00:00Z\n---\n"+name+"\n"), 0o600))
}

func (in *restoreInstance) reconcile(t *testing.T, db *sql.DB) {
	t.Helper()
	mgr, err := files.NewManager(in.dataDir, db, nil)
	require.NoError(t, err)
	require.NoError(t, mgr.Reconcile(context.Background()))
	require.NoError(t, mgr.Close())
}

// backUp exports the instance into the backup folder under name.
func (in *restoreInstance) backUp(t *testing.T, name string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(in.folder, 0o700))
	path := filepath.Join(in.folder, name)
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = archive.Export(context.Background(), archive.ExportOptions{DataDir: in.dataDir, Out: f, Host: "test-host",
		Kind: archive.KindManual, TmpDir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}

// changeToday adds what the backup does not have.
func (in *restoreInstance) changeToday(t *testing.T) {
	t.Helper()
	db := in.db(t)
	in.addTask(t, db, "01TASKTODAY")
	in.writeMemory(t, "today-only", "01MEMTODAY")
	in.reconcile(t, db)
	require.NoError(t, db.Close())
}

func (in *restoreInstance) tasks(t *testing.T) []string {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(in.dataDir, archive.DBName))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id FROM tasks ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

func testRestoreDeps(out *strings.Builder) restoreDeps {
	return restoreDeps{
		in: strings.NewReader(""), out: out, euid: os.Geteuid(), inspect: inspectPath,
		restartService: func() error { return nil },
		waitFor:        30 * time.Second, poll: 5 * time.Millisecond, now: time.Now,
	}
}

func TestRestoreCmd_WithNoDaemonAppliesAndSavesTodaysData(t *testing.T) {
	in := newRestoreInstance(t)
	name := "seamless-test-host-20261110T030000Z.tar.gz"
	in.backUp(t, name)
	in.changeToday(t)
	require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, in.tasks(t))

	var out strings.Builder
	err := restoreCmd(context.Background(), []string{name, "--yes"}, testRestoreDeps(&out))
	require.NoError(t, err, out.String())
	require.Contains(t, out.String(), "restored the backup from")
	require.Contains(t, out.String(), "restore it to undo this")
	require.Equal(t, []string{"01TASKSEED"}, in.tasks(t))
	require.NoFileExists(t, filepath.Join(in.dataDir, archive.MemoryTree, "alpha", "today-only.md"))
	require.False(t, backup.RestorePending(in.dataDir))

	res, err := backup.ReadResult(in.dataDir)
	require.NoError(t, err)
	require.Equal(t, backup.RestoreDone, res.Outcome)
	require.Equal(t, "cli", res.RequestedBy)
	require.NotEmpty(t, res.Previous, res.PreviousError)
	require.FileExists(t, filepath.Join(backup.LocalDir(in.dataDir), res.Previous))
	des, err := os.ReadDir(backup.RestoreRoot(in.dataDir))
	require.NoError(t, err)
	require.Len(t, des, 1, "only last.json: the work dir is gone")

	// Undo, by name, merging this time: today's task comes back beside the
	// backup's.
	out.Reset()
	err = restoreCmd(context.Background(), []string{"--merge", "--yes", res.Previous}, testRestoreDeps(&out))
	require.NoError(t, err, out.String())
	require.Contains(t, out.String(), "items added")
	require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, in.tasks(t))
	require.FileExists(t, filepath.Join(in.dataDir, archive.MemoryTree, "alpha", "today-only.md"))
}

func TestRestoreCmd_WithADaemonStagesAndWaitsForItsResult(t *testing.T) {
	in := newRestoreInstance(t)
	path := in.backUp(t, "seamless-test-host-20261110T030000Z.tar.gz")
	in.changeToday(t)

	// The daemon: it holds the data dir lock, notices the staged restore and
	// applies it at its restart.
	lock, err := tryLockFile(filepath.Join(in.dataDir, dataDirLockName))
	require.NoError(t, err)
	daemon := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for !backup.RestorePending(in.dataDir) {
			if time.Now().After(deadline) {
				daemon <- os.ErrDeadlineExceeded
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		_, err := backup.Applier{DataDir: in.dataDir}.Apply(context.Background())
		daemon <- err
	}()

	var out strings.Builder
	err = restoreCmd(context.Background(), []string{"--yes", path}, testRestoreDeps(&out))
	require.NoError(t, err, out.String())
	require.NoError(t, <-daemon)
	require.NoError(t, lock.Close())
	// Windows has no in-place restart: the command restarts the service
	// itself (a stub here) instead of waiting for the daemon to.
	waits := "waiting for Seamless to restart and finish"
	if runtime.GOOS == "windows" {
		waits = "restarting Seamless"
	}
	require.Contains(t, out.String(), waits)
	require.Contains(t, out.String(), "restored the backup from")
	require.Equal(t, []string{"01TASKSEED"}, in.tasks(t))
}

func TestRestoreCmd_ADaemonThatNeverFinishesIsReportedNotHidden(t *testing.T) {
	in := newRestoreInstance(t)
	path := in.backUp(t, "seamless-test-host-20261110T030000Z.tar.gz")
	lock, err := tryLockFile(filepath.Join(in.dataDir, dataDirLockName))
	require.NoError(t, err)
	defer func() { _ = lock.Close() }()

	var out strings.Builder
	d := testRestoreDeps(&out)
	d.waitFor = 50 * time.Millisecond
	err = restoreCmd(context.Background(), []string{"--yes", path}, d)
	require.ErrorContains(t, err, "restart it within an hour")
	require.True(t, backup.RestorePending(in.dataDir), "the staged restore stays for the next start")
}

func TestRestoreCmd_Refusals(t *testing.T) {
	in := newRestoreInstance(t)
	path := in.backUp(t, "seamless-test-host-20261110T030000Z.tar.gz")
	var out strings.Builder

	err := restoreCmd(context.Background(), []string{path}, testRestoreDeps(&out))
	require.ErrorContains(t, err, "confirm on a terminal, or pass --yes")

	d := testRestoreDeps(&out)
	d.interactive, d.in = true, strings.NewReader("n\n")
	err = restoreCmd(context.Background(), []string{path}, d)
	require.ErrorContains(t, err, "not restored")

	err = restoreCmd(context.Background(), []string{"--yes", "no-such-backup.tar.gz"}, testRestoreDeps(&out))
	require.ErrorContains(t, err, "seamlessd backups lists them")

	err = restoreCmd(context.Background(), []string{"--yes", path, "another"}, testRestoreDeps(&out))
	require.ErrorContains(t, err, "one backup at a time")

	require.NoError(t, os.MkdirAll(backup.RestoreRoot(in.dataDir), 0o700))
	require.NoError(t, os.WriteFile(backup.PendingPath(in.dataDir), []byte(`{"stage":"staged"}`), 0o600))
	err = restoreCmd(context.Background(), []string{"--yes", path}, testRestoreDeps(&out))
	require.ErrorContains(t, err, "another restore is waiting to finish")
	require.NoError(t, os.Remove(backup.PendingPath(in.dataDir)))

	require.Equal(t, []string{"01TASKSEED"}, in.tasks(t), "nothing changed")
}

func TestRestoreAccountCheck_PeerAuthNeedsTheDaemonsAccountNeverRoot(t *testing.T) {
	cfg := config.Config{DataDir: "/opt/seamless/data"}
	facts := func(uid uint32) func(string) (fileFacts, error) {
		return func(string) (fileFacts, error) { return fileFacts{uid: uid, mode: 0o700, isDir: true}, nil }
	}
	require.ErrorContains(t, restoreAccountCheck(cfg, restoreDeps{euid: 0, inspect: facts(0)}), "never as root")
	require.ErrorContains(t, restoreAccountCheck(cfg, restoreDeps{euid: 501, inspect: facts(260)}), "not the daemon's account")
	require.NoError(t, restoreAccountCheck(cfg, restoreDeps{euid: 260, inspect: facts(260)}))
}

func TestRunBackups_ListsTheFolderAndTheSafetyBackups(t *testing.T) {
	in := newRestoreInstance(t)
	in.backUp(t, "seamless-test-host-20261110T030000Z.tar.gz")
	require.NoError(t, os.MkdirAll(backup.LocalDir(in.dataDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(backup.LocalDir(in.dataDir), "unrelated.txt"), []byte("x"), 0o600))

	out := captureStdout(t, func() error { return runBackups(nil) })
	require.Contains(t, out, "folder: "+in.folder)
	require.Contains(t, out, "made by you")
	require.Contains(t, out, "seamless-test-host-20261110T030000Z.tar.gz")
	require.Contains(t, out, "1 memories")
	require.NotContains(t, out, "unrelated.txt")

	js := captureStdout(t, func() error { return runBackups([]string{"--json"}) })
	require.Contains(t, js, `"ref": "folder:seamless-test-host-20261110T030000Z.tar.gz"`)
}

func TestBackupsCheck_DoctorSaysWhatABackupOrRestoreNeeds(t *testing.T) {
	in := newRestoreInstance(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	ctx := context.Background()
	now := time.Now()

	c := backupsCheck(ctx, cfg, nil, now)
	require.Equal(t, statusInfo, c.status)
	require.Contains(t, c.detail, "automatic backups are off", "a source build backs up only when configured")

	on := true
	cfg.Backup.Auto = &on
	c = backupsCheck(ctx, cfg, nil, now)
	require.Contains(t, c.detail, "none made yet")

	require.NoError(t, os.MkdirAll(backup.RestoreRoot(in.dataDir), 0o700))
	require.NoError(t, os.WriteFile(backup.PendingPath(in.dataDir), []byte(`{"stage":"swapping","id":"01X"}`), 0o600))
	c = backupsCheck(ctx, cfg, nil, now)
	require.Equal(t, statusWarn, c.status)
	require.Contains(t, c.detail, "part way through its swap")

	require.NoError(t, os.WriteFile(backup.PendingPath(in.dataDir), []byte(`{"stage":"staged","backup":{"name":"b.tar.gz"},"requested_at":"2026-11-11T10:00:00Z"}`), 0o600))
	c = backupsCheck(ctx, cfg, nil, now)
	require.Equal(t, statusInfo, c.status)
	require.Contains(t, c.detail, "applies when Seamless restarts")
	require.NoError(t, os.Remove(backup.PendingPath(in.dataDir)))

	prev := filepath.Join(backup.RestoreRoot(in.dataDir), "01OLD", "previous")
	require.NoError(t, os.MkdirAll(filepath.Join(prev, "memory"), 0o700))
	c = backupsCheck(ctx, cfg, nil, now)
	require.Equal(t, statusWarn, c.status)
	require.Contains(t, c.detail, prev)
}

func TestAttendedUpdate_RefusesWhileARestoreWaits(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w)
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	require.NoError(t, os.MkdirAll(backup.RestoreRoot(w.dataDir), 0o700))
	require.NoError(t, os.WriteFile(backup.PendingPath(w.dataDir), []byte(`{"stage":"staged"}`), 0o600))

	out, err := runAttended(t, w, w.deps(nil))
	require.ErrorContains(t, err, "a restore is waiting to finish", out)
	require.Empty(t, w.installs)
	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	require.Empty(t, a.ID, "nothing recorded")
}

func TestApplyPendingRestore_ServeFinishesOrDropsAStagedRestore(t *testing.T) {
	stage := func(t *testing.T, in *restoreInstance, path string) {
		t.Helper()
		db := in.db(t)
		defer func() { require.NoError(t, db.Close()) }()
		_, err := backup.Stager{DataDir: in.dataDir, DB: db}.Stage(context.Background(),
			backup.StageRequest{Path: path, Mode: backup.ModeReplace, RequestedBy: "console", Proof: "owner"})
		require.NoError(t, err)
	}
	cfgOf := func(t *testing.T) config.Config {
		t.Helper()
		cfg, err := config.Load()
		require.NoError(t, err)
		return cfg
	}
	quietLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("applied under the lock", func(t *testing.T) {
		in := newRestoreInstance(t)
		path := in.backUp(t, "seamless-test-host-20261110T030000Z.tar.gz")
		in.changeToday(t)
		stage(t, in, path)
		require.NoError(t, applyPendingRestore(context.Background(), cfgOf(t), nil, true, quietLog))
		require.Equal(t, []string{"01TASKSEED"}, in.tasks(t))
		res, err := backup.ReadResult(in.dataDir)
		require.NoError(t, err)
		require.Equal(t, backup.RestoreDone, res.Outcome)
	})
	t.Run("dropped without a lock", func(t *testing.T) {
		in := newRestoreInstance(t)
		path := in.backUp(t, "seamless-test-host-20261110T030000Z.tar.gz")
		in.changeToday(t)
		stage(t, in, path)
		require.NoError(t, applyPendingRestore(context.Background(), cfgOf(t), nil, false, quietLog))
		require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, in.tasks(t), "nothing changed")
		res, err := backup.ReadResult(in.dataDir)
		require.NoError(t, err)
		require.Equal(t, backup.RestoreReasonUnsupported, res.Reason)
		require.False(t, backup.RestorePending(in.dataDir))
	})
	t.Run("nothing staged", func(t *testing.T) {
		in := newRestoreInstance(t)
		require.NoError(t, applyPendingRestore(context.Background(), cfgOf(t), nil, true, quietLog))
		require.NoDirExists(t, backup.RestoreRoot(in.dataDir))
	})
}
