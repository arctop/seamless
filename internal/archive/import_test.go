package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// instanceSpec parameterizes the two throwaway instances the merge tests need:
// A (exported) and B (imported into). Every id, slug and name is disjoint
// between them unless a test deliberately overlaps one, so a count that moves
// names exactly one cause.
type instanceSpec struct {
	project    string
	memName    string
	memID      string
	noteSlug   string
	noteID     string
	globalMem  string
	globalID   string
	sessionID  string
	sessName   string
	taskA      string
	taskB      string
	trialID    string
	eventID    string
	settingVal string
}

var specA = instanceSpec{
	project: "alpha", memName: "alpha-boot-race", memID: "01MEMALPHA",
	noteSlug: "alpha-design", noteID: "01NOTEALPHA",
	globalMem: "alpha-prefers-uv", globalID: "01MEMALPHAG",
	sessionID: "01SESSALPHA", sessName: "cc/alpha",
	taskA: "01TASKALPHA1", taskB: "01TASKALPHA2",
	trialID: "01TRIALALPHA", eventID: "01EVENTALPHA",
	settingVal: `{"/Users/a/repos/alpha":"alpha"}`,
}

var specB = instanceSpec{
	project: "beta", memName: "beta-boot-race", memID: "01MEMBETA",
	noteSlug: "beta-design", noteID: "01NOTEBETA",
	globalMem: "beta-prefers-uv", globalID: "01MEMBETAG",
	sessionID: "01SESSBETA", sessName: "cc/beta",
	taskA: "01TASKBETA1", taskB: "01TASKBETA2",
	trialID: "01TRIALBETA", eventID: "01EVENTBETA",
	settingVal: `{"/Users/b/repos/beta":"beta"}`,
}

const testEmbedModel = "text-embedding-3-small"

// seedIndexed builds a data dir whose index agrees with its files: memories and
// notes are written THROUGH the files layer, not dropped on disk, so the
// restored-instance assertions can compare index rows and content hashes rather
// than only file bytes.
func seedIndexed(t *testing.T, s instanceSpec) string {
	t.Helper()
	dir := t.TempDir()
	seedIndexedInto(t, dir, s)
	return dir
}

func seedIndexedInto(t *testing.T, dir string, s instanceSpec) {
	t.Helper()
	ctx := context.Background()

	db, err := storetest.OpenFresh(t, filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, mgr.Close()) }()

	now := time.Date(2026, 7, 10, 18, 0, 0, 0, time.UTC)
	invalid := now.Add(-time.Hour)

	_, err = mgr.WriteMemory(ctx, core.Memory{
		ID: s.memID, Kind: core.MemoryKind("gotcha"), Name: s.memName,
		Description: "boot races", Project: s.project, Body: "it races on boot\n",
		Tags: []string{"boot"}, Favorite: true,
		Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)

	// A superseded memory: invalid_at + superseded_by have to ride through the
	// archive untouched, and in particular must not be re-stamped by lifecycle.
	_, err = mgr.WriteMemory(ctx, core.Memory{
		ID: s.memID + "OLD", Kind: core.MemoryKind("gotcha"), Name: s.memName + "-old",
		Description: "the earlier claim", Project: s.project, Body: "superseded\n",
		Created: now, Updated: now, ValidFrom: now,
		InvalidAt: &invalid, SupersededBy: s.memID,
	})
	require.NoError(t, err)

	_, err = mgr.WriteMemory(ctx, core.Memory{
		ID: s.globalID, Kind: core.MemoryKind("convention"), Name: s.globalMem,
		Description: "always uv", Body: "always uv\n",
		Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)

	_, err = mgr.WriteNote(ctx, core.Note{
		ID: s.noteID, Title: "design", Slug: s.noteSlug, Project: s.project,
		Body: "the design\n", Created: now, Updated: now,
	})
	require.NoError(t, err)

	_, err = store.EnsureProject(ctx, db, s.project, s.project)
	require.NoError(t, err)

	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: s.sessionID, Name: s.sessName, ProjectSlug: s.project,
		Status: core.SessionCompleted, Findings: "the row pass runs on one pinned conn",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: s.taskA, ProjectSlug: s.project, Title: "extract first",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: s.taskB, ProjectSlug: s.project, Title: "merge second",
		Status: core.TaskOpen, DependsOn: []string{s.taskA},
		CreatedAt: now, UpdatedAt: now,
	}))
	_, err = store.RecordSessionCommits(ctx, db, s.sessionID, s.sessionID, []string{strings.Repeat("c", 40)}, now)
	require.NoError(t, err)
	require.NoError(t, store.CreateTrial(ctx, db, core.Trial{
		ID: s.trialID, Lab: "import", Title: "attach + insert or ignore",
		Expected: "rows appear once", Actual: "they did", Outcome: core.OutcomePass,
		ProjectSlug: s.project, CreatedAt: now,
	}))
	_, err = db.ExecContext(ctx, `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.eventID, core.FormatTime(now), "memory.write", s.sessionID, s.project, s.memID, "{}")
	require.NoError(t, err)

	require.NoError(t, store.UpsertEmbedding(ctx, db, s.memID, "memory", testEmbedModel,
		[]float32{0.1, 0.2, 0.3}))
	require.NoError(t, store.SetSetting(ctx, db, "repo_project_map", s.settingVal))
}

// exportOf returns the archive bytes for a data dir.
func exportOf(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{DataDir: dir, Out: &buf, Host: "exporthost"})
	require.NoError(t, err)
	return buf.Bytes()
}

// countsOf reads the per-table row counts of a data dir's database, reusing the
// exporter's enumeration so a table added by a later migration is compared
// without anyone updating this test.
func countsOf(t *testing.T, dir string) map[string]int {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	c, err := tableCounts(context.Background(), db)
	require.NoError(t, err)
	return c
}

// hashesOf maps file_path -> content_hash for both index tables.
func hashesOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	out := map[string]string{}
	scan := func(q string) {
		rows, err := db.QueryContext(context.Background(), q)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		for rows.Next() {
			var p, h string
			require.NoError(t, rows.Scan(&p, &h))
			out[p] = h
		}
		require.NoError(t, rows.Err())
	}
	scan(`SELECT file_path, content_hash FROM memories_index`)
	scan(`SELECT file_path, content_hash FROM notes_index`)
	return out
}

// corpusBytes maps every .md path under a data dir to its content.
func corpusBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tree := range []string{MemoryTree, NotesTree} {
		root := filepath.Join(dir, tree)
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if info.IsDir() || filepath.Ext(p) != ".md" {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			require.NoError(t, err)
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			out[filepath.ToSlash(rel)] = string(data)
			return nil
		})
		require.NoError(t, err)
	}
	return out
}

func settingOf(t *testing.T, dir, key string) string {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	v, ok, err := store.GetSetting(context.Background(), db, key)
	require.NoError(t, err)
	require.True(t, ok)
	return v
}

func ftsCount(t *testing.T, dir, itemID string) int {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM fts WHERE item_id = ?`, itemID).Scan(&n))
	return n
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// ---------------------------------------------------------------------------
// DetectMode
// ---------------------------------------------------------------------------

func TestDetectMode(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string
		want  Mode
	}{
		{
			name:  "absent data dir",
			setup: func(t *testing.T, dir string) string { return filepath.Join(dir, "nope") },
			want:  ModeFresh,
		},
		{
			name:  "empty data dir",
			setup: func(t *testing.T, dir string) string { return dir },
			want:  ModeFresh,
		},
		{
			name: "empty trees",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, MemoryTree, "_global"), 0o700))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, NotesTree, "_global"), 0o700))
				return dir
			},
			want: ModeFresh,
		},
		{
			name: "only dot files",
			setup: func(t *testing.T, dir string) string {
				writeFile(t, dir, MemoryTree+"/_global/.DS_Store", "finder junk")
				writeFile(t, dir, MemoryTree+"/.seamless-tmp-01ABC", "half a write")
				return dir
			},
			want: ModeFresh,
		},
		{
			name: "one memory",
			setup: func(t *testing.T, dir string) string {
				writeFile(t, dir, MemoryTree+"/_global/x.md", "---\nid: 01X\n---\nx\n")
				return dir
			},
			want: ModeMerge,
		},
		{
			name: "one note",
			setup: func(t *testing.T, dir string) string {
				writeFile(t, dir, NotesTree+"/_global/x.md", "---\nid: 01X\n---\nx\n")
				return dir
			},
			want: ModeMerge,
		},
		{
			name: "only seam.db",
			setup: func(t *testing.T, dir string) string {
				writeFile(t, dir, DBName, "not really a database")
				return dir
			},
			want: ModeMerge,
		},
		{
			name: "only the write-ahead log",
			setup: func(t *testing.T, dir string) string {
				writeFile(t, dir, DBName+"-wal", "a checkpointed database is still a database")
				return dir
			},
			want: ModeMerge,
		},
		{
			name: "a non-corpus file outside the trees does not count",
			setup: func(t *testing.T, dir string) string {
				writeFile(t, dir, "seamlessd.log", "INFO started")
				writeFile(t, dir, "backups/old.md", "a hand-rolled backup")
				return dir
			},
			want: ModeFresh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setup(t, t.TempDir())
			got, err := DetectMode(dir)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestDetectMode_RejectsAFileAsDataDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a-file")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	_, err := DetectMode(path)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// fresh restore
// ---------------------------------------------------------------------------

func TestImport_FreshRoundTrip(t *testing.T) {
	src := seedIndexed(t, specA)
	data := exportOf(t, src)

	dst := filepath.Join(t.TempDir(), "restored")
	rep, err := Import(context.Background(), ImportOptions{
		DataDir: dst, Src: bytes.NewReader(data),
	})
	require.NoError(t, err)
	require.Equal(t, ModeFresh, rep.Mode)
	require.Equal(t, 3, rep.Memories)
	require.Equal(t, 1, rep.Notes)
	require.Empty(t, rep.Rows, "a fresh restore carries the whole database, not per-table inserts")
	require.Empty(t, rep.PathCollisions)
	require.Empty(t, rep.NameCollisions)

	// Every markdown file is byte-identical: a restore re-renders nothing.
	require.Equal(t, corpusBytes(t, src), corpusBytes(t, dst))

	// Every table agrees, embeddings and settings included -- the two tables a
	// MERGE deliberately leaves alone, which is exactly why a restore has to
	// carry them.
	srcCounts, dstCounts := countsOf(t, src), countsOf(t, dst)
	require.Equal(t, srcCounts, dstCounts)
	require.Equal(t, 1, dstCounts["embeddings"])
	require.Equal(t, 1, dstCounts["settings"])
	require.Equal(t, specA.settingVal, settingOf(t, dst, "repo_project_map"))

	// Reconcile ran during the import and found nothing to do: the snapshot's
	// hashes already describe the restored files.
	require.Equal(t, hashesOf(t, src), hashesOf(t, dst))

	// The temp name the snapshot was extracted under is gone.
	require.NotContains(t, dirEntryNames(t, dst), importTmpDB)
	require.Contains(t, dirEntryNames(t, dst), DBName)
}

func TestImport_FreshDryRunWritesNothing(t *testing.T) {
	src := seedIndexed(t, specA)
	data := exportOf(t, src)

	dst := t.TempDir()
	rep, err := Import(context.Background(), ImportOptions{
		DataDir: dst, Src: bytes.NewReader(data), DryRun: true,
	})
	require.NoError(t, err)
	require.Equal(t, ModeFresh, rep.Mode)
	require.True(t, rep.DryRun)
	require.Equal(t, rep.Manifest.Counts.MemoryFiles, rep.Memories)
	require.Equal(t, rep.Manifest.Counts.NoteFiles, rep.Notes)
	require.Equal(t, 3, rep.Memories)
	require.Equal(t, 1, rep.Notes)
	require.Empty(t, dirEntryNames(t, dst), "a fresh dry run extracts nothing at all")
	require.Contains(t, rep.String(), "dry run")
}

// A knowledge-only archive restores into an empty destination and gets a
// database built from the files it carried.
func TestImport_FreshFromNoDBArchive(t *testing.T) {
	src := seedIndexed(t, specA)
	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{DataDir: src, Out: &buf, NoDB: true, Host: "h"})
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), "restored")
	rep, err := Import(context.Background(), ImportOptions{DataDir: dst, Src: &buf})
	require.NoError(t, err)
	require.Equal(t, ModeFresh, rep.Mode)
	require.Equal(t, corpusBytes(t, src), corpusBytes(t, dst))

	counts := countsOf(t, dst)
	require.Equal(t, 3, counts["memories_index"], "Reconcile indexed the restored files")
	require.Equal(t, 1, counts["notes_index"])
	require.Zero(t, counts["sessions"], "there was no database in the archive")
}

// ---------------------------------------------------------------------------
// merge
// ---------------------------------------------------------------------------

func TestImport_Merge(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	data := exportOf(t, src)
	dst := seedIndexed(t, specB)

	before := countsOf(t, dst)
	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, rep.Mode)
	require.Equal(t, 3, rep.Memories)
	require.Equal(t, 1, rep.Notes)
	require.Zero(t, rep.Skipped)
	require.Empty(t, rep.PathCollisions)
	require.Empty(t, rep.NameCollisions)
	require.Equal(t, map[string]int{
		"projects": 1, "sessions": 1, "tasks": 2, "task_deps": 1,
		"trials": 1, "events": 1, "gardener_proposals": 0, "commit_records": 1,
		"item_provenance": 0,
	}, rep.Rows)
	require.Zero(t, rep.Projects, "the archive's own projects row carried the slug")

	after := countsOf(t, dst)
	require.Equal(t, before["settings"], after["settings"])
	require.Equal(t, specB.settingVal, settingOf(t, dst, "repo_project_map"),
		"settings describe the destination machine and are never imported")
	require.Equal(t, before["embeddings"], after["embeddings"],
		"vectors belong to whichever model this instance runs")
	require.Equal(t, before["memories_index"]+3, after["memories_index"])
	require.Equal(t, before["notes_index"]+1, after["notes_index"])

	// The row pass INSERTs directly, so the FTS mirror is only correct if the
	// reindex ran.
	for _, id := range []string{specA.taskA, specA.taskB, specA.trialID, specA.sessionID} {
		require.Equalf(t, 1, ftsCount(t, dst, id), "no fts row for %s", id)
	}
	// Imported memories are indexed through the files layer.
	require.Equal(t, 1, ftsCount(t, dst, specA.memID))

	// No embedder was configured, so the report says so rather than pretending
	// the imported corpus is semantically searchable.
	require.Len(t, rep.Warnings, 1)
	require.Contains(t, rep.Warnings[0], "no embedder configured")

	// Second run: everything is already here.
	second, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Zero(t, second.Memories)
	require.Zero(t, second.Notes)
	require.Equal(t, 4, second.Skipped)
	require.Zero(t, second.Projects)
	for table, n := range second.Rows {
		require.Zerof(t, n, "second merge inserted into %s", table)
	}
	require.Equal(t, after, countsOf(t, dst), "an idempotent merge changes no count")
}

func TestImport_MergePathCollisionLeavesTheDestinationFile(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	data := exportOf(t, src)

	// B holds a DIFFERENT memory at the path A's memory belongs at.
	dst := seedIndexed(t, specB)
	occupied := files.MemoryRelPath(specA.project, specA.memName)
	seedForeignMemory(t, dst, "01MEMSQUATTER", specA.project, specA.memName)
	beforeBytes, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(occupied)))
	require.NoError(t, err)

	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Len(t, rep.PathCollisions, 1)
	require.Equal(t, PathCollision{
		Path: occupied, IncomingID: specA.memID, ExistingID: "01MEMSQUATTER",
	}, rep.PathCollisions[0])
	require.Equal(t, 2, rep.Memories, "the other two memories still land")

	afterBytes, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(occupied)))
	require.NoError(t, err)
	require.Equal(t, beforeBytes, afterBytes, "a collision never overwrites")
	require.Contains(t, rep.String(), "path collision")
}

func TestImport_MergeSessionNameCollisionIsReported(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	data := exportOf(t, src)

	dst := seedIndexed(t, specB)
	seedForeignSession(t, dst, "01SESSSQUATTER", specA.sessName)

	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Equal(t, []NameCollision{{
		Table: "sessions", Column: "name", Value: specA.sessName,
		IncomingID: specA.sessionID, ExistingID: "01SESSSQUATTER",
	}}, rep.NameCollisions)
	require.Zero(t, rep.Rows["sessions"], "INSERT OR IGNORE drops the colliding row")
	require.Contains(t, rep.String(), "sessions.name collision")
}

func TestImport_MergeDryRunPredictsTheRealRun(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	data := exportOf(t, src)

	// Both destinations are seeded identically, including the collisions the
	// prediction has to account for: a taken session name (a UNIQUE column the
	// primary key knows nothing about) and an occupied corpus path.
	seedDst := func(t *testing.T) string {
		t.Helper()
		dir := seedIndexed(t, specB)
		seedForeignSession(t, dir, "01SESSSQUATTER", specA.sessName)
		seedForeignMemory(t, dir, "01MEMSQUATTER", specA.project, specA.memName)
		return dir
	}

	dryDir := seedDst(t)
	dryBefore := countsOf(t, dryDir)
	dry, err := Import(ctx, ImportOptions{DataDir: dryDir, Src: bytes.NewReader(data), DryRun: true})
	require.NoError(t, err)

	liveDir := seedDst(t)
	live, err := Import(ctx, ImportOptions{DataDir: liveDir, Src: bytes.NewReader(data)})
	require.NoError(t, err)

	require.Equal(t, live.Rows, dry.Rows)
	require.Equal(t, live.Memories, dry.Memories)
	require.Equal(t, live.Notes, dry.Notes)
	require.Equal(t, live.Skipped, dry.Skipped)
	require.Equal(t, live.Projects, dry.Projects)
	require.Equal(t, live.PathCollisions, dry.PathCollisions)
	require.Equal(t, live.NameCollisions, dry.NameCollisions)

	// And the dry run imported nothing.
	require.Equal(t, dryBefore, countsOf(t, dryDir))
	require.Equal(t, corpusBytes(t, dryDir), corpusBytes(t, seedDst(t)))
}

// Two archive entries whose frontmatter sends them to the SAME corpus path
// (their file names differ, their memory names do not) must be predicted the
// way they are performed: the first lands, the second is a collision. The dry
// run only gets this right if it accounts for the writes it has itself promised.
func TestImport_MergeDryRunAccountsForItsOwnPendingWrites(t *testing.T) {
	ctx := context.Background()
	const dup = "---\nid: %s\nkind: gotcha\nname: same-name\n---\nbody\n"
	data := craftArchive(t, goodManifest(), []tar.Header{
		{Typeflag: tar.TypeReg, Name: "memory/_global/first.md"},
		{Typeflag: tar.TypeReg, Name: "memory/_global/second.md"},
	}, []string{
		fmt.Sprintf(dup, "01MEMFIRST"),
		fmt.Sprintf(dup, "01MEMSECOND"),
	})

	dry, err := Import(ctx, ImportOptions{
		DataDir: seedIndexed(t, specB), Src: bytes.NewReader(data), DryRun: true,
	})
	require.NoError(t, err)
	live, err := Import(ctx, ImportOptions{
		DataDir: seedIndexed(t, specB), Src: bytes.NewReader(data),
	})
	require.NoError(t, err)

	require.Equal(t, 1, live.Memories)
	require.Equal(t, live.Memories, dry.Memories)
	require.Equal(t, live.PathCollisions, dry.PathCollisions)
	require.Len(t, live.PathCollisions, 1)
	require.Equal(t, "memory/_global/same-name.md", live.PathCollisions[0].Path)
	require.Equal(t, "01MEMSECOND", live.PathCollisions[0].IncomingID)
	require.Equal(t, "01MEMFIRST", live.PathCollisions[0].ExistingID)
}

// An archive a release before migration 039 wrote merges into this one: the
// staged snapshot migrates forward, so its sessions arrive continuing no other
// and it carries no stamps. (The other way round, an archive this release
// writes is refused by an older one as a newer schema: ErrSchemaTooNew.)
func TestImport_MergeAnArchiveFromBefore039(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	db, err := store.OpenExisting(filepath.Join(src, DBName))
	require.NoError(t, err)
	// Put the snapshot back in schema 38's shape: what 039 added, gone.
	for _, q := range []string{
		`DROP INDEX idx_sessions_parent`, `DROP INDEX idx_sessions_conversation`,
		`ALTER TABLE sessions DROP COLUMN parent_session_id`,
		`DROP INDEX idx_access_approvals_conversation`,
		`ALTER TABLE access_approvals DROP COLUMN conversation_id`, `ALTER TABLE access_approvals DROP COLUMN conversation_client`,
		`ALTER TABLE access_approvals DROP COLUMN carry`, `ALTER TABLE access_approvals DROP COLUMN requested_access`,
		`DROP INDEX idx_access_grants_conversation`,
		`ALTER TABLE access_grants DROP COLUMN conversation_id`, `ALTER TABLE access_grants DROP COLUMN conversation_client`,
		`ALTER TABLE access_grants DROP COLUMN access`,
		`DROP TABLE item_provenance`, `DROP TABLE access_flows`, `DROP TABLE access_moves`,
		// What 041 added (console sign-in with a passkey).
		`DROP TABLE console_signins`, `DROP TABLE passkey_enrollments`,
		`DELETE FROM schema_migrations WHERE version >= 39`,
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	v, err := store.SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 38, v)
	require.NoError(t, db.Close())

	data := exportOf(t, src)
	dst := seedIndexed(t, specB)
	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, rep.Mode)
	require.Equal(t, 1, rep.Rows["sessions"])
	require.Zero(t, rep.Rows["item_provenance"])

	out, err := store.OpenExisting(filepath.Join(dst, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, out.Close()) }()
	s, found, err := store.SessionByID(ctx, out, specA.sessionID)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, s.ParentSessionID)
	var null int
	require.NoError(t, out.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ? AND parent_session_id IS NULL`,
		specA.sessionID).Scan(&null))
	require.Equal(t, 1, null)
}

// A provenance stamp rides with the items it stamps (mergeTables): a merge
// carries it, a dry run predicts it, and the same archive imported again adds
// no second copy, the stamp's key being the item, its kind and the source. The
// approval it names stays named although approvals are never merged.
func TestImport_MergeCarriesProvenanceStamps(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	srcDB, err := store.Open(filepath.Join(src, DBName))
	require.NoError(t, err)
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	for _, s := range []store.ProvenanceStamp{
		{ItemKind: core.ItemKindTask, ItemID: specA.taskA, Project: specA.project, Source: "example-finance", ApprovalID: "01APPROVALFIN"},
		{ItemKind: core.ItemKindSession, ItemID: specA.sessionID, Project: specA.project, Source: "example-legal", ApprovalID: "01APPROVALLEG"},
	} {
		require.NoError(t, store.ProvenanceUpsert(ctx, srcDB, s, now))
	}
	// A memory's stamp is its file's (frontmatter may_contain), and the files
	// layer mirrors it into item_provenance, as a stamped write leaves it.
	srcMgr, err := files.NewManager(src, srcDB, nil)
	require.NoError(t, err)
	mem, err := srcMgr.Store().ReadMemory(files.MemoryRelPath(specA.project, specA.memName))
	require.NoError(t, err)
	mem.MayContain = []core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: "01APPROVALFIN"}}
	_, err = srcMgr.WriteMemory(ctx, mem)
	require.NoError(t, err)
	require.NoError(t, srcMgr.Close())
	require.NoError(t, srcDB.Close())
	data := exportOf(t, src)

	dry, err := Import(ctx, ImportOptions{DataDir: seedIndexed(t, specB), Src: bytes.NewReader(data), DryRun: true})
	require.NoError(t, err)
	dst := seedIndexed(t, specB)
	live, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Equal(t, 3, live.Rows["item_provenance"])
	require.Equal(t, live.Rows["item_provenance"], dry.Rows["item_provenance"], "the dry run predicts the stamps")

	db, err := store.OpenExisting(filepath.Join(dst, DBName))
	require.NoError(t, err)
	got, err := store.ProvenanceForItem(ctx, db, core.ItemKindTask, specA.taskA)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, specA.project, got[0].Project)
	require.Equal(t, "example-finance", got[0].Source)
	require.Equal(t, "01APPROVALFIN", got[0].ApprovalID)
	merged, err := store.ProvenanceForItem(ctx, db, core.ItemKindMemory, specA.memID)
	require.NoError(t, err)
	require.Len(t, merged, 1, "the merged memory's file carries its stamp, and the index mirrors it")
	require.NoError(t, db.Close())
	mergedMem, err := files.NewStore(dst).ReadMemory(files.MemoryRelPath(specA.project, specA.memName))
	require.NoError(t, err)
	require.Equal(t, []core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: "01APPROVALFIN"}}, mergedMem.MayContain)

	again, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Zero(t, again.Rows["item_provenance"], "the same archive again adds no second stamp")
	require.Equal(t, 3, countsOf(t, dst)["item_provenance"])
}

// TestImport_MergeLeavesTablesOutsideTheContractAlone generalizes the settings
// and embeddings assertions. Every table outside mergeTables (and the mirrors
// the files pass and the post-passes rebuild) must leave a merge with the row
// count it went in with -- including tables a later migration adds, because the
// default for a new table has to be "not merged" until someone decides
// otherwise rather than the other way round.
func TestImport_MergeLeavesTablesOutsideTheContractAlone(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)

	// A row in a table the merge deliberately does not carry: jobs is a work
	// queue, and importing another machine's pending work would make this
	// daemon run it.
	srcDB, err := store.Open(filepath.Join(src, DBName))
	require.NoError(t, err)
	now := core.FormatTime(time.Date(2026, 7, 10, 18, 0, 0, 0, time.UTC))
	_, err = srcDB.ExecContext(ctx, `
		INSERT INTO jobs (id, type, payload, status, attempts, created_at, updated_at)
		VALUES ('01JOBALPHA', 'embed', '{}', 'pending', 0, ?, ?)`, now, now)
	require.NoError(t, err)
	// A browser signed in to the exporting machine's console, and a passkey
	// waiting for its owner: per-install live state, behind a cookie and a
	// code only that machine's browser and owner hold. A merge carries
	// neither: a sign-in merged here would be one nobody made here, and a
	// waiting passkey one this owner never saw.
	signedAt := time.Now()
	_, err = store.CreateConsoleSignIn(ctx, srcDB, "sig-cookie", "sig-browser-key", "01PASSKEYALPHA", time.Hour, signedAt)
	require.NoError(t, err)
	_, _, err = store.BeginPasskeyEnrollment(ctx, srcDB, store.ApproverCredential{Kind: store.ApproverPasskey,
		CredentialID: []byte{7, 7}, PublicKey: []byte{9, 9}, Alg: -7, Name: "phone"}, store.PasskeyEnrollmentTTL, signedAt)
	require.NoError(t, err)
	require.NoError(t, srcDB.Close())

	data := exportOf(t, src)
	dst := seedIndexed(t, specB)
	before := countsOf(t, dst)
	_, err = Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	after := countsOf(t, dst)

	mayChange := map[string]bool{
		"memories_index": true, "notes_index": true, "fts": true, "retrieval_stats": true,
	}
	for _, table := range mergeTables {
		mayChange[table] = true
	}
	for table, want := range before {
		if mayChange[table] {
			continue
		}
		require.Equalf(t, want, after[table],
			"merge changed %s, which is outside the row-pass contract", table)
	}
	require.Zero(t, after["jobs"], "another machine's work queue is not this machine's")
	require.Equal(t, before["settings"], after["settings"])
	require.Equal(t, before["embeddings"], after["embeddings"])
	require.Zero(t, after["console_signins"], "another machine's console sign-ins are not this machine's")
	require.Zero(t, after["passkey_enrollments"], "nor are the passkeys waiting for its owner")
}

// A merge whose corpus names a project the archive's database does not carry
// still gets a projects row, the way importer.backfillProjects does it.
func TestImport_MergeBackfillsProjectsTheArchiveDidNotCarry(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	// Drop the project row from A but keep its memory, which names the slug.
	srcDB, err := store.OpenExisting(filepath.Join(src, DBName))
	require.NoError(t, err)
	_, err = srcDB.ExecContext(ctx, `DELETE FROM projects`)
	require.NoError(t, err)
	require.NoError(t, srcDB.Close())

	data := exportOf(t, src)
	dst := seedIndexed(t, specB)

	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.NoError(t, err)
	require.Zero(t, rep.Rows["projects"])
	require.Equal(t, 1, rep.Projects)

	db, err := store.OpenExisting(filepath.Join(dst, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, ok, err := store.ProjectBySlug(ctx, db, specA.project)
	require.NoError(t, err)
	require.True(t, ok)
}

// seedForeignMemory drops a memory owned by another id at the path the archive
// wants, bypassing the files layer so the test controls the bytes exactly.
func seedForeignMemory(t *testing.T, dir, id, project, name string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, mgr.Close()) }()

	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	_, err = mgr.WriteMemory(ctx, core.Memory{
		ID: id, Kind: core.MemoryKind("gotcha"), Name: name, Project: project,
		Description: "the destination got here first", Body: "mine\n",
		Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)
}

func seedForeignSession(t *testing.T, dir, id, name string) {
	t.Helper()
	db, err := store.Open(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, Status: core.SessionCompleted,
		CreatedAt: now, UpdatedAt: now,
	}))
}

// ---------------------------------------------------------------------------
// refusals
// ---------------------------------------------------------------------------

// craftArchive builds an archive with an arbitrary manifest and entry list, so
// the guards can be tested against tars no exporter would ever write.
func craftArchive(t *testing.T, manifest any, entries []tar.Header, bodies []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if manifest != nil {
		mj, err := json.Marshal(manifest)
		require.NoError(t, err)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Format: tar.FormatPAX, Typeflag: tar.TypeReg,
			Name: ManifestName, Size: int64(len(mj)), Mode: 0o600,
		}))
		_, err = tw.Write(mj)
		require.NoError(t, err)
	}
	for i, hdr := range entries {
		h := hdr
		h.Format = tar.FormatPAX
		if h.Mode == 0 {
			h.Mode = 0o600
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[i]))
		}
		require.NoError(t, tw.WriteHeader(&h))
		if h.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(bodies[i]))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func goodManifest() Manifest {
	return Manifest{
		FormatVersion: FormatVersion, SchemaVersion: store.LatestSchemaVersion(),
		SeamlessdVersion: "0.0.0-test", CreatedAt: time.Now().UTC(),
		SourceHost: "h", IncludesDB: false,
	}
}

func TestImport_UnsafeEntriesAreRefusedAndLeaveNothing(t *testing.T) {
	const body = "---\nid: 01EVIL\n---\nevil\n"
	tests := []struct {
		name    string
		entries []tar.Header
		bodies  []string
	}{
		{
			name:    "parent traversal",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: "../x.md"}},
			bodies:  []string{body},
		},
		{
			name:    "absolute path",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: "/abs.md"}},
			bodies:  []string{body},
		},
		{
			name:    "traversal through a legal prefix",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: "memory/../../x.md"}},
			bodies:  []string{body},
		},
		{
			name:    "backslash separator",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: `memory\_global\x.md`}},
			bodies:  []string{body},
		},
		{
			name:    "unclean path",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: "memory/./_global/x.md"}},
			bodies:  []string{body},
		},
		{
			name:    "symlink",
			entries: []tar.Header{{Typeflag: tar.TypeSymlink, Name: "memory/_global/x.md", Linkname: "/etc/passwd"}},
			bodies:  []string{""},
		},
		{
			name:    "hardlink",
			entries: []tar.Header{{Typeflag: tar.TypeLink, Name: "memory/_global/x.md", Linkname: "seam.db"}},
			bodies:  []string{""},
		},
		{
			name:    "directory entry",
			entries: []tar.Header{{Typeflag: tar.TypeDir, Name: "memory/_global"}},
			bodies:  []string{""},
		},
		{
			name:    "outside the layout",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: "other/x.md"}},
			bodies:  []string{body},
		},
		{
			name:    "not markdown",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: "memory/_global/x.txt"}},
			bodies:  []string{body},
		},
		{
			name:    "repeated entry",
			entries: []tar.Header{{Typeflag: tar.TypeReg, Name: ManifestName}},
			bodies:  []string{"{}"},
		},
		{
			name: "a good entry ahead of a bad one is rolled back",
			entries: []tar.Header{
				{Typeflag: tar.TypeReg, Name: "memory/_global/good.md"},
				{Typeflag: tar.TypeReg, Name: "../x.md"},
			},
			bodies: []string{body, body},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := craftArchive(t, goodManifest(), tt.entries, tt.bodies)
			dst := t.TempDir()
			_, err := Import(context.Background(), ImportOptions{
				DataDir: dst, Src: bytes.NewReader(data),
			})
			require.Error(t, err)
			require.ErrorIs(t, err, ErrUnsafeEntry)
			require.Empty(t, dirEntryNames(t, dst), "a refused archive leaves nothing behind")
		})
	}
}

func TestImport_NewerSchemaIsRefusedBeforeExtraction(t *testing.T) {
	m := goodManifest()
	m.SchemaVersion = store.LatestSchemaVersion() + 1
	data := craftArchive(t, m,
		[]tar.Header{{Typeflag: tar.TypeReg, Name: "memory/_global/x.md"}},
		[]string{"---\nid: 01X\n---\nx\n"})

	dst := t.TempDir()
	_, err := Import(context.Background(), ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSchemaTooNew)
	require.Empty(t, dirEntryNames(t, dst))
}

func TestImport_NotAnArchive(t *testing.T) {
	t.Run("not gzip", func(t *testing.T) {
		_, err := Import(context.Background(), ImportOptions{
			DataDir: t.TempDir(), Src: bytes.NewReader([]byte("plain text, not a tarball")),
		})
		require.ErrorIs(t, err, ErrNotArchive)
	})

	t.Run("manifest is not first", func(t *testing.T) {
		data := craftArchive(t, nil,
			[]tar.Header{
				{Typeflag: tar.TypeReg, Name: "memory/_global/x.md"},
				{Typeflag: tar.TypeReg, Name: ManifestName},
			},
			[]string{"---\nid: 01X\n---\nx\n", "{}"})
		_, err := Import(context.Background(), ImportOptions{
			DataDir: t.TempDir(), Src: bytes.NewReader(data),
		})
		require.ErrorIs(t, err, ErrNotArchive)
	})

	t.Run("empty tar", func(t *testing.T) {
		data := craftArchive(t, nil, nil, nil)
		_, err := Import(context.Background(), ImportOptions{
			DataDir: t.TempDir(), Src: bytes.NewReader(data),
		})
		require.ErrorIs(t, err, ErrNotArchive)
	})

	t.Run("manifest is not json", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Format: tar.FormatPAX, Typeflag: tar.TypeReg,
			Name: ManifestName, Size: 3, Mode: 0o600,
		}))
		_, err := tw.Write([]byte("no!"))
		require.NoError(t, err)
		require.NoError(t, tw.Close())
		require.NoError(t, gz.Close())

		_, err = Import(context.Background(), ImportOptions{
			DataDir: t.TempDir(), Src: bytes.NewReader(buf.Bytes()),
		})
		require.ErrorIs(t, err, ErrNotArchive)
	})
}

func TestImport_UnsupportedFormatVersion(t *testing.T) {
	m := goodManifest()
	m.FormatVersion = FormatVersion + 1
	data := craftArchive(t, m, nil, nil)
	_, err := Import(context.Background(), ImportOptions{
		DataDir: t.TempDir(), Src: bytes.NewReader(data),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "format_version")
}

func TestImport_RejectsEmptyArguments(t *testing.T) {
	_, err := Import(context.Background(), ImportOptions{Src: bytes.NewReader(nil)})
	require.Error(t, err)
	_, err = Import(context.Background(), ImportOptions{DataDir: t.TempDir()})
	require.Error(t, err)
}

// A symlink planted in a destination that DetectMode still calls fresh (it
// counts regular files) must not receive the write.
func TestImport_FreshRefusesToWriteThroughASymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("not yours"), 0o600))

	dst := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dst, MemoryTree, "_global"), 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(dst, MemoryTree, "_global", "x.md")))

	mode, err := DetectMode(dst)
	require.NoError(t, err)
	require.Equal(t, ModeFresh, mode, "a symlink is not a regular file")

	data := craftArchive(t, goodManifest(),
		[]tar.Header{{Typeflag: tar.TypeReg, Name: "memory/_global/x.md"}},
		[]string{"---\nid: 01X\n---\nmine now\n"})
	_, err = Import(context.Background(), ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
	require.ErrorIs(t, err, ErrUnsafeEntry)

	got, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "not yours", string(got))
}

// A restored database brings back the leases on locked projects it held when
// the snapshot was taken; a fresh restore ends every live one, so a locked
// project's agents ask again.
func TestImport_FreshRevokesRestoredLeases(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	srcDB, err := store.Open(filepath.Join(src, DBName))
	require.NoError(t, err)
	proc := access.ProcessToken{PID: 4242, Version: 7, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	now := time.Now().UTC()
	a, err := store.CreateAccessApproval(ctx, srcDB, store.AccessRequest{
		Project: "example-finance", Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: 10 * time.Minute,
		PeerUID: 501, PeerPID: proc.PID, ProcessKey: proc.Key(),
	}, now)
	require.NoError(t, err)
	_, err = store.ApproveAccess(ctx, srcDB, a.ID, a.Challenge, "daemon-account", now)
	require.NoError(t, err)
	require.NoError(t, srcDB.Close())

	dst := filepath.Join(t.TempDir(), "restored")
	_, err = Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(exportOf(t, src))})
	require.NoError(t, err)
	dstDB, err := store.Open(filepath.Join(dst, DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = dstDB.Close() })
	live, err := store.ActiveAccessLeases(ctx, dstDB, proc.Key(), time.Now())
	require.NoError(t, err)
	require.Empty(t, live, "no lease survives a restore")
	var reason string
	require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases`).Scan(&reason))
	require.Equal(t, store.LeaseRevokedReset, reason)
}

// A console sign-in is per-install live state, like a console unlock: a
// restored database brings back the sign-ins it held when the snapshot was
// taken, ones signed out since included, so a fresh restore ends every live
// one and every passkey still waiting for approval (store.ResetRestoredAccess),
// and the owner signs in again.
func TestImport_FreshEndsRestoredSignIns(t *testing.T) {
	ctx := context.Background()
	src := seedIndexed(t, specA)
	srcDB, err := store.Open(filepath.Join(src, DBName))
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = store.CreateConsoleSignIn(ctx, srcDB, "sig-cookie", "sig-browser-key", "01PASSKEYALPHA", 24*time.Hour, now)
	require.NoError(t, err)
	_, code, err := store.BeginPasskeyEnrollment(ctx, srcDB, store.ApproverCredential{Kind: store.ApproverPasskey,
		CredentialID: []byte{7, 7}, PublicKey: []byte{9, 9}, Alg: -7, Name: "phone"}, store.PasskeyEnrollmentTTL, now)
	require.NoError(t, err)
	require.NoError(t, srcDB.Close())

	dst := filepath.Join(t.TempDir(), "restored")
	_, err = Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(exportOf(t, src))})
	require.NoError(t, err)
	dstDB, err := store.Open(filepath.Join(dst, DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = dstDB.Close() })
	_, err = store.LiveConsoleSignIn(ctx, dstDB, store.HashConsoleSignIn("sig-cookie", "sig-browser-key"), time.Now())
	require.ErrorIs(t, err, store.ErrSignInNotFound, "no sign-in survives a restore")
	var reason string
	require.NoError(t, dstDB.QueryRowContext(ctx, `SELECT ended_reason FROM console_signins`).Scan(&reason))
	require.Equal(t, store.SignInEndReset, reason)
	_, err = store.PasskeyEnrollmentByCode(ctx, dstDB, code, time.Now())
	require.ErrorIs(t, err, store.ErrEnrollmentNotFound, "a passkey that waited in the snapshot cannot be approved after it")
	_, _, err = store.ApprovePasskeyEnrollment(ctx, dstDB, code, time.Now())
	require.ErrorIs(t, err, store.ErrEnrollmentNotFound)
}
