package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/bench"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestGitSnapshotRestoreDiff(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "myapp")
	head := initRepo(t, repo)

	sha, err := gitSnapshot(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, head, sha)

	// An agent edits a tracked file, adds a new one, and leaves an ignored one.
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n// edited\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "ratelimit.go"), []byte("package main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "build", "out"), []byte("junk"), 0o644))

	diff, err := gitDiff(ctx, repo, sha)
	require.NoError(t, err)
	require.Contains(t, diff, "main.go")
	require.Contains(t, diff, "+// edited")
	require.Contains(t, diff, "ratelimit.go", "a file the agent CREATED must appear in the diff")
	require.NotContains(t, diff, "build/out", "ignored files stay out of the diff")

	// The working tree keeps no benchmark scaffolding: the intent-to-add only
	// touched .git/index.
	entries, err := os.ReadDir(repo)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.ElementsMatch(t, []string{".git", ".gitignore", "build", "main.go", "ratelimit.go"}, names)

	require.NoError(t, gitRestore(ctx, repo, sha))
	after, err := gitDiff(ctx, repo, sha)
	require.NoError(t, err)
	require.Empty(t, after, "a restored repo diffs clean against its snapshot")
	require.NoDirExists(t, filepath.Join(repo, "build"), "clean -fdx removes ignored leftovers too")
}

func TestDumpEvents(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	rec := events.NewRecorder(db)
	base := time.Now().UTC().Truncate(time.Second)
	for i, kind := range []core.EventKind{core.EventInjected, core.EventToolCall, core.EventMemoryRead} {
		_, err := rec.Record(ctx, core.Event{
			Kind:    kind,
			TS:      base.Add(time.Duration(i) * time.Second),
			Payload: map[string]any{"i": i},
		})
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	out := filepath.Join(t.TempDir(), bench.EventsFile)
	require.NoError(t, dumpEvents(ctx, &arm{seamless: true, dataDir: dataDir}, out))

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	var got []core.Event
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got, 3)
	require.Equal(t, core.EventInjected, got[0].Kind, "the dump reads oldest first")
	require.Equal(t, core.EventMemoryRead, got[2].Kind)

	// A vanilla arm gets an empty array, never a missing file: "no Seamless, so
	// no events" is a fact the grader should read, not infer.
	vanillaOut := filepath.Join(t.TempDir(), bench.EventsFile)
	require.NoError(t, dumpEvents(ctx, &arm{seamless: false}, vanillaOut))
	b, err = os.ReadFile(vanillaOut)
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(b))
}

func TestFindTranscript(t *testing.T) {
	configDir := t.TempDir()
	projects := filepath.Join(configDir, "projects", "-tmp-myapp")
	require.NoError(t, os.MkdirAll(projects, 0o755))

	stale := filepath.Join(projects, "previous-run.jsonl")
	fresh := filepath.Join(projects, "this-run.jsonl")
	require.NoError(t, os.WriteFile(stale, []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(fresh, []byte("{}\n"), 0o644))

	since := time.Now()
	old := since.Add(-time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	// The session id wins outright.
	got, err := findTranscript(configDir, "previous-run", since)
	require.NoError(t, err)
	require.Equal(t, stale, got)

	// Without one, only files written since the run started are candidates.
	got, err = findTranscript(configDir, "", since)
	require.NoError(t, err)
	require.Equal(t, fresh, got)

	// Nothing recent at all is not an error.
	require.NoError(t, os.Chtimes(fresh, old, old))
	got, err = findTranscript(configDir, "", since)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "x.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git", "secret"), []byte("marker"), 0o644))

	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("must not be copied"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(src, "link.txt")))

	require.NoError(t, copyTree(src, dst, func(rel string) bool { return rel == ".git" }))

	b, err := os.ReadFile(filepath.Join(dst, "sub", "x.txt"))
	require.NoError(t, err)
	require.Equal(t, "x", string(b))
	require.FileExists(t, filepath.Join(dst, "main.go"))
	require.NoDirExists(t, filepath.Join(dst, ".git"), "the skip predicate prunes the whole subtree")
	require.NoFileExists(t, filepath.Join(dst, "link.txt"), "symlinks are never followed out of the tree")
}

func TestCapture_WritesTheFrozenLayout(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	base := t.TempDir()
	cond := bench.Condition{Name: "mechanism", Profile: bench.ProfileMechanism, Client: bench.ClientClaude}
	f := newArmFixture(t, base, cond, 8099)
	a, err := loadArm(f.envFile, cond)
	require.NoError(t, err)
	a.snapshot = f.head

	// A seeded data dir and a transcript, as a finished run would have left.
	db, err := storetest.OpenFresh(t, filepath.Join(a.dataDir, "seam.db"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	projects := filepath.Join(a.configDir, "projects", "-fixture")
	require.NoError(t, os.MkdirAll(projects, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projects, "sess.jsonl"), []byte("{\"type\":\"user\"}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(a.repo, "ratelimit.go"), []byte("package main\n"), 0o644))

	dir := t.TempDir()
	require.NoError(t, capture(ctx, a, dir, runOutcome{sessionID: "sess", startedAt: time.Now().Add(-time.Minute)}))

	for _, name := range []string{bench.DiffFile, bench.EventsFile, bench.TranscriptFile} {
		require.FileExists(t, filepath.Join(dir, name))
	}
	require.DirExists(t, filepath.Join(dir, bench.RepoDirName))
	require.DirExists(t, filepath.Join(dir, bench.DataDirName))
	require.NoDirExists(t, filepath.Join(dir, bench.RepoDirName, ".git"))
	require.FileExists(t, filepath.Join(dir, bench.DataDirName, "seam.db"))

	diff, err := os.ReadFile(filepath.Join(dir, bench.DiffFile))
	require.NoError(t, err)
	require.Contains(t, string(diff), "ratelimit.go")
}

// The daemon writes in WAL mode, so a run's most recent events can still live
// in seam.db-wal when the arm is preserved. The dump must see them and the
// copy must carry the sidecars: a data/ holding seam.db alone would look like
// the mechanism went quiet right at the end of every run.
func TestCapture_PreservesTheWriteAheadLogTail(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	base := t.TempDir()
	cond := bench.Condition{Name: "mechanism", Profile: bench.ProfileMechanism, Client: bench.ClientClaude}
	f := newArmFixture(t, base, cond, 8099)
	a, err := loadArm(f.envFile, cond)
	require.NoError(t, err)
	a.snapshot = f.head

	db, err := storetest.OpenFresh(t, filepath.Join(a.dataDir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	for _, kind := range []core.EventKind{core.EventToolCall, core.EventSessionEnded} {
		_, err := rec.Record(ctx, core.Event{Kind: kind, TS: time.Now().UTC()})
		require.NoError(t, err)
	}
	// A live connection keeps the WAL from being checkpointed away, which is
	// exactly the state a just-stopped arm can be in.
	require.FileExists(t, filepath.Join(a.dataDir, "seam.db-wal"))

	before, err := os.ReadDir(a.dataDir)
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, capture(ctx, a, dir, runOutcome{startedAt: time.Now().Add(-time.Minute)}))

	var evs []core.Event
	b, err := os.ReadFile(filepath.Join(dir, bench.EventsFile))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &evs))
	require.Len(t, evs, 2, "the dump must see events still held in the WAL")

	for _, e := range before {
		require.FileExists(t, filepath.Join(dir, bench.DataDirName, e.Name()),
			"every file in the arm's data dir is preserved, sidecars included")
	}
}

func TestCapture_MissingTranscriptIsNotAnError(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	cond := bench.Condition{Name: "vanilla", Profile: bench.ProfileVanilla, Client: bench.ClientClaude}
	f := newArmFixture(t, base, cond, 0)
	a, err := loadArm(f.envFile, cond)
	require.NoError(t, err)
	a.snapshot = f.head

	dir := t.TempDir()
	require.NoError(t, capture(context.Background(), a, dir, runOutcome{startedAt: time.Now()}))
	require.NoFileExists(t, filepath.Join(dir, bench.TranscriptFile))
	require.NoDirExists(t, filepath.Join(dir, bench.DataDirName), "a vanilla arm has no data dir to preserve")

	rec := bench.RunRecord{Scenario: "s", Condition: cond, Run: 1}
	require.NoError(t, bench.WriteRunRecord(dir, rec))
	_, arts, err := bench.LoadRun(dir)
	require.NoError(t, err)
	require.Empty(t, arts.Transcript)
	require.Empty(t, arts.DataDir)
	require.NotEmpty(t, arts.RepoDir)
}

func TestGitError_NamesTheCommand(t *testing.T) {
	requireGit(t)
	_, err := git(context.Background(), t.TempDir(), "rev-parse", "HEAD")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "git rev-parse HEAD"), err.Error())
}
