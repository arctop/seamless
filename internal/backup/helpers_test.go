package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// t0 is the fake clock's start: a fixed wall time, so names and the schedule
// are deterministic.
var t0 = time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type recorder struct {
	mu     sync.Mutex
	events []core.Event
}

func (r *recorder) Record(_ context.Context, e core.Event) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return "", nil
}

func (r *recorder) of(kind core.EventKind) []core.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.Event
	for _, e := range r.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// seedDataDir makes a data dir with a database holding one project and one
// task, and a memory file, and returns the dir and the live handle.
func seedDataDir(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, archive.DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	_, err = store.EnsureProject(ctx, db, "alpha", "Alpha")
	require.NoError(t, err)
	addTask(t, db, "01TASKSEED")
	mem := filepath.Join(dir, archive.MemoryTree, "alpha", "boot-race.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(mem), 0o700))
	require.NoError(t, os.WriteFile(mem, []byte("---\nid: 01MEMSEED\nkind: gotcha\n---\nchroma races on boot\n"), 0o600))
	return dir, db
}

// emptyDataDir makes a data dir whose database holds nothing.
func emptyDataDir(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, archive.DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return dir, db
}

func addTask(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "alpha", Title: "task " + id, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now,
	}))
}

func ptr[T any](v T) *T { return &v }

// harness is one running Service over a seeded data dir, with a fake clock
// and hand-fed ticks and polls.
type harness struct {
	t        *testing.T
	dataDir  string
	db       *sql.DB
	folder   string
	clock    *fakeClock
	ticks    chan time.Time
	polls    chan time.Time
	rec      *recorder
	svc      *Service
	ctx      context.Context
	cancel   context.CancelFunc
	restarts atomic.Int32
	deps     Deps
}

type harnessOpt func(*Deps)

// newHarness starts a Service with automatic backups on into a temp folder.
func newHarness(t *testing.T, dataDir string, db *sql.DB, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, dataDir: dataDir, db: db, clock: &fakeClock{t: t0}, rec: &recorder{}}
	h.folder = filepath.Join(t.TempDir(), "Seamless Backups")
	d := Deps{
		DataDir: dataDir, DB: db, Release: true, Version: "0.8.1", Host: "test-host",
		Base:      config.Backup{Auto: ptr(true), Every: config.Duration(24 * time.Hour), Keep: 7, Dir: h.folder},
		FreeBytes: func(string) (uint64, error) { return 1 << 40, nil },
		Events:    h.rec,
		RequestRestart: func() {
			h.restarts.Add(1)
		},
		Now: h.clock.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(&d)
	}
	h.deps = d
	h.start()
	return h
}

// start (re)builds and starts the service from h.deps.
func (h *harness) start() {
	h.ticks = make(chan time.Time)
	h.polls = make(chan time.Time)
	d := h.deps
	ticks, polls := h.ticks, h.polls
	d.Ticks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	d.Polls = func() (<-chan time.Time, func()) { return polls, func() {} }
	h.svc = New(d)
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.svc.Start(h.ctx)
	h.t.Cleanup(h.stop)
}

// stop ends the service and waits for it.
func (h *harness) stop() {
	h.cancel()
	h.svc.Wait()
}

// restart stops the service and starts a new one on the same data dir: a
// daemon restart.
func (h *harness) restart() {
	h.stop()
	h.start()
}

// sync returns once the loop has handled everything sent before it.
func (h *harness) sync() Status {
	st, err := h.svc.Refresh(context.Background())
	require.NoError(h.t, err)
	return st
}

// tick feeds the loop one tick at the fake clock's time.
func (h *harness) tick() Status {
	h.ticks <- h.clock.Now()
	return h.sync()
}

// poll feeds the loop one pending-restore poll.
func (h *harness) poll() {
	h.polls <- h.clock.Now()
	h.sync()
}

// idle waits for any run to finish and returns the status.
func (h *harness) idle() Status {
	h.t.Helper()
	var st Status
	require.Eventually(h.t, func() bool {
		st = h.svc.Status()
		return st.Running == nil
	}, 30*time.Second, 2*time.Millisecond)
	return st
}

// runScheduled moves the clock by d, ticks, and waits for the run.
func (h *harness) runScheduled(d time.Duration) Status {
	h.t.Helper()
	h.clock.Add(d)
	h.tick()
	return h.idle()
}

// files lists the visible files in dir.
func files(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

// craftArchive writes a gzipped tar whose first entry is the manifest m.
func craftArchive(t *testing.T, m archive.Manifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	require.NoError(t, err)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: archive.ManifestName, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}))
	_, err = tw.Write(data)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}
