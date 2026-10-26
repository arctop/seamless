package gardener

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// fakeEmbedder returns a one-hot unit vector along an axis encoded in the text as
// "@axis:N@". Two memories with the same axis embed identically (cosine 1.0);
// different axes are orthogonal (cosine 0). This lets a test force a duplicate.
type fakeEmbedder struct{}

func (fakeEmbedder) Model() string { return "fake-model" }

func (fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	axis := 0
	if _, rest, ok := strings.Cut(text, "@axis:"); ok {
		if val, _, ok := strings.Cut(rest, "@"); ok {
			axis, _ = strconv.Atoi(val)
		}
	}
	vec := make([]float32, 16)
	if axis >= 0 && axis < len(vec) {
		vec[axis] = 1
	}
	return vec, nil
}

// fakeChat returns a fixed completion, standing in for the digest summarizer.
type fakeChat struct{ out string }

func (f fakeChat) Model() string { return "fake-chat" }
func (f fakeChat) Complete(_ context.Context, _, _ string) (string, error) {
	return f.out, nil
}

func writeMem(t *testing.T, mgr *files.Manager, name, project, axis string, kind core.MemoryKind, updated time.Time, body string) {
	t.Helper()
	_, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: mustID(t), Kind: kind, Name: name, Description: name + " description",
		Project: project, Body: "@axis:" + axis + "@\n\n" + body,
		Created: updated, Updated: updated, ValidFrom: updated,
	})
	require.NoError(t, err)
}

func mustID(t *testing.T) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	return id
}

func TestPruneToolEvents_OnlyOldTransport(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)

	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -40)  // beyond a 30-day window
	fresh := now.AddDate(0, 0, -5) // inside the window

	mustRec := func(id string, ts time.Time, kind core.EventKind) {
		_, err := rec.Record(ctx, core.Event{ID: id, TS: ts, Kind: kind})
		require.NoError(t, err)
	}
	mustRec("01A", old, core.EventToolCall)      // old transport -> pruned
	mustRec("01B", old, core.EventHookPrompt)    // old transport -> pruned
	mustRec("01C", old, core.EventMemoryWritten) // old domain -> kept
	mustRec("01D", fresh, core.EventToolCall)    // fresh transport -> kept
	mustRec("01E", old, core.EventRecallMiss)    // old transport -> pruned

	g := New(db, nil, nil, nil, rec, Config{ToolEventRetentionDays: 30}, slog.Default())
	g.now = func() time.Time { return now }
	g.pruneToolEvents(ctx)

	got, err := rec.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	kinds := map[core.EventKind]bool{}
	for _, e := range got {
		kinds[e.Kind] = true
	}
	require.True(t, kinds[core.EventMemoryWritten], "domain event survives")
	require.True(t, kinds[core.EventToolCall], "fresh transport event survives")
	require.False(t, kinds[core.EventHookPrompt], "old transport event pruned")
	require.False(t, kinds[core.EventRecallMiss], "old recall miss pruned")

	// Retention 0 disables pruning entirely (no-op).
	g0 := New(db, nil, nil, nil, rec, Config{ToolEventRetentionDays: 0}, slog.Default())
	g0.now = func() time.Time { return now }
	g0.pruneToolEvents(ctx)
	got2, err := rec.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, got2, 2)
}

func TestReapStaleSessions(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)

	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	mk := func(name string, updatedAt time.Time, status core.SessionStatus) string {
		id := mustID(t)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: "seamless", Status: status,
			CreatedAt: updatedAt, UpdatedAt: updatedAt,
		}))
		return id
	}

	live := mk("cc/live", now.Add(-2*time.Minute), core.SessionActive)
	stale := mk("sess/stale", now.Add(-2*time.Hour), core.SessionActive)

	// The stale session holds a task claim, which reaping must return to the queue.
	taskID := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: taskID, ProjectSlug: "seamless", Title: "held work", Status: core.TaskOpen,
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
	}))
	_, err = store.ClaimTask(ctx, db, taskID, stale, 30*time.Minute, now.Add(-2*time.Hour))
	require.NoError(t, err)

	g := New(db, nil, nil, nil, rec, Config{}, slog.Default())
	g.now = func() time.Time { return now }
	g.reapStaleSessions(ctx)

	// Stale -> expired; live untouched.
	gotStale, _, err := store.SessionByID(ctx, db, stale)
	require.NoError(t, err)
	require.Equal(t, core.SessionExpired, gotStale.Status)
	gotLive, _, err := store.SessionByID(ctx, db, live)
	require.NoError(t, err)
	require.Equal(t, core.SessionActive, gotLive.Status)

	// The held task is released back to open.
	task, err := store.TaskByID(ctx, db, taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, task.Status)
	require.Empty(t, task.ClaimedBy)

	// A session.ended event stamped reason=expired was recorded for the reap.
	evs, err := rec.RecentExcluding(ctx, 20)
	require.NoError(t, err)
	var reaped bool
	for _, e := range evs {
		if e.Kind == core.EventSessionEnded && e.SessionID == stale {
			require.Equal(t, "expired", e.Payload["reason"])
			reaped = true
		}
	}
	require.True(t, reaped, "the reap recorded a session.ended event")
}

func TestReapStaleSessions_ConfiguredIdleTTL(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	id := mustID(t)
	// Quiet for 20 minutes: inside the 45m default, outside a 10m configured TTL.
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/quiet", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: now.Add(-20 * time.Minute), UpdatedAt: now.Add(-20 * time.Minute),
	}))

	gDefault := New(db, nil, nil, nil, nil, Config{}, slog.Default())
	gDefault.now = func() time.Time { return now }
	gDefault.reapStaleSessions(ctx)
	got, _, err := store.SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, core.SessionActive, got.Status, "inside the default TTL: not reaped")

	gShort := New(db, nil, nil, nil, nil, Config{SessionIdle: 10 * time.Minute}, slog.Default())
	gShort.now = func() time.Time { return now }
	gShort.reapStaleSessions(ctx)
	got, _, err = store.SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, core.SessionExpired, got.Status, "outside the configured TTL: reaped")
}

func TestRunOnce_ProducesAllThreeProposalKinds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	mgr.SetEmbedder(fakeEmbedder{})
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()

	// Dedup: two near-identical memories (same embedding axis) -> one merge.
	writeMem(t, mgr, "dup-a", "", "1", core.KindGotcha, now.Add(-2*time.Hour), "first copy")
	writeMem(t, mgr, "dup-b", "", "1", core.KindGotcha, now.Add(-1*time.Hour), "second copy")
	// A distinct, fresh memory: neither duplicate nor stale.
	writeMem(t, mgr, "unique-fresh", "", "3", core.KindReference, now, "unrelated")
	// Staleness: old, distinct, unreferenced, archivable kind -> one archive.
	writeMem(t, mgr, "ancient", "", "4", core.KindGotcha, now.Add(-120*24*time.Hour), "long unread")
	// A constraint just as old must NOT be archived (protected kind).
	writeMem(t, mgr, "old-rule", "", "5", core.KindConstraint, now.Add(-200*24*time.Hour), "still binding")
	// A referenced old memory must NOT be archived (something links to it).
	writeMem(t, mgr, "referenced-old", "", "6", core.KindGotcha, now.Add(-200*24*time.Hour), "keep me")
	writeMem(t, mgr, "linker", "", "7", core.KindReference, now, "see [[referenced-old]] for context")

	// Digest: two completed sessions with findings in one project -> one digest.
	for i := range 2 {
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: mustID(t), Name: "cc/sess" + strconv.Itoa(i), ProjectSlug: "proj-x",
			Status: core.SessionCompleted, Findings: "finding number " + strconv.Itoa(i),
			CreatedAt: now, UpdatedAt: now,
		}))
	}

	rec := events.NewRecorder(db)
	g := New(db, mgr, fakeEmbedder{}, fakeChat{out: "## Digest\n- did things"}, rec,
		Config{DedupThreshold: 0.88, StalenessDays: 90, DigestDays: 30}, slog.Default())

	res, err := g.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.Merges, "one duplicate pair")
	require.Equal(t, 1, res.Archives, "one stale unreferenced memory")
	require.Equal(t, 1, res.Digests, "one project digest")

	// Verify the proposals landed with the right kinds.
	merges, err := store.PendingProposals(ctx, db, store.ProposalMerge)
	require.NoError(t, err)
	require.Len(t, merges, 1)
	archives, err := store.PendingProposals(ctx, db, store.ProposalArchive)
	require.NoError(t, err)
	require.Len(t, archives, 1)
	require.Equal(t, "ancient", archives[0].Payload["name"], "only the unreferenced, non-constraint stale memory")
	digests, err := store.PendingProposals(ctx, db, store.ProposalDigest)
	require.NoError(t, err)
	require.Len(t, digests, 1)
	require.Equal(t, "proj-x", digests[0].Payload["project"])

	// Idempotent: a second pass re-proposes nothing (all keys already exist).
	res2, err := g.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, res2.Total(), "already-proposed items are not raised again")
}

// A memory's [[links]] are what protect OTHER memories from staleness
// archiving, so an unreadable body does not just lose its own links -- it makes
// every absence from the protection set unprovable. Archiving on that basis
// would propose retiring a live, referenced memory. The pass must refuse and
// report itself failed instead. Regression for F17: before, the unreadable body
// was warned and skipped, and "referenced-old" was proposed for archive.
func TestRunOnce_UnreadableBodyBlocksArchivesRatherThanFalselyProposing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	// referenced-old is stale but protected: linker's body points at it.
	writeMem(t, mgr, "referenced-old", "", "6", core.KindGotcha, now.Add(-200*24*time.Hour), "keep me")
	writeMem(t, mgr, "linker", "", "7", core.KindReference, now, "see [[referenced-old]] for context")

	// The index still lists linker as active, but its body is gone -- so the
	// protection it provides is now invisible to the scan.
	require.NoError(t, os.Remove(filepath.Join(dir, "memory", "_global", "linker.md")))

	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{StalenessDays: 90, DigestDays: 30}, slog.Default())

	res, err := g.RunOnce(ctx)
	require.NoError(t, err, "one failing pass must not abort the run")

	archives, err := store.PendingProposals(ctx, db, store.ProposalArchive)
	require.NoError(t, err)
	require.Empty(t, archives, "must not propose archiving a memory whose protection could not be read")

	// The zero must not read as "nothing was stale".
	require.Equal(t, 0, res.Archives)
	require.False(t, res.OK(), "a refused pass is not a clean pass")
	require.Contains(t, res.Failed, "staleness")
}

// The counts are only trustworthy when every pass ran; a healthy run says so.
func TestRunOnce_OKWhenEveryPassRuns(t *testing.T) {
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	writeMem(t, mgr, "readable", "", "1", core.KindGotcha, time.Now().UTC(), "nothing stale here")

	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{StalenessDays: 90, DigestDays: 30}, slog.Default())

	res, err := g.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, res.OK())
	require.Empty(t, res.Failed)
	require.Equal(t, 0, res.Total(), "a genuine nothing-to-propose zero")
}

func TestRunOnce_DegradesWithoutEmbedderOrChat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	// No embedder, no chat: dedup and digest are skipped; staleness still runs.
	now := time.Now().UTC()
	mgr.SetEmbedder(fakeEmbedder{})
	writeMem(t, mgr, "ancient", "", "2", core.KindGotcha, now.Add(-120*24*time.Hour), "old")

	g := New(db, mgr, nil, nil, nil, Config{}, slog.Default())
	res, err := g.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, res.Merges)
	require.Equal(t, 0, res.Digests)
	require.Equal(t, 1, res.Archives)
}
