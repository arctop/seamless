package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const (
	memFavPath   = "memory/seam/chroma-boot-race.md"
	memGlobPath  = "memory/_global/user-prefers-uv.md"
	notePath     = "notes/seam/export-design.md"
	seedEmbModel = "text-embedding-3-small"
)

// seedInstance builds a throwaway data dir with a migrated seam.db carrying one
// row in every table the archive reports, plus the two markdown trees. It
// returns the data dir.
func seedInstance(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	db, err := storetest.OpenFresh(t, filepath.Join(dir, DBName))
	require.NoError(t, err)

	_, err = store.EnsureProject(ctx, db, "seam", "Seam")
	require.NoError(t, err)

	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01SESSEXPORT", Name: "cc/export", ProjectSlug: "seam",
		Status: core.SessionCompleted, Findings: "the snapshot precedes the walk",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: "01TASKEXPORTA", ProjectSlug: "seam", Title: "snapshot first",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: "01TASKEXPORTB", ProjectSlug: "seam", Title: "walk second",
		Status: core.TaskOpen, DependsOn: []string{"01TASKEXPORTA"},
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTrial(ctx, db, core.Trial{
		ID: "01TRIALEXPORT", Lab: "vacuum-into", Title: "bound parameter",
		Expected: "a snapshot appears", Actual: "it did", Outcome: core.OutcomePass,
		ProjectSlug: "seam", CreatedAt: now,
	}))
	_, err = db.ExecContext(ctx, `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"01EVENTEXPORT", core.FormatTime(now), "memory.write", "01SESSEXPORT", "seam", "01MEMFAV", "{}")
	require.NoError(t, err)
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01MEMFAV", "memory", seedEmbModel,
		[]float32{0.1, 0.2, 0.3}))
	require.NoError(t, store.SetSetting(ctx, db, "repo_project_map",
		`{"/Users/someone/repos/seamless":"seamless"}`))
	require.NoError(t, db.Close())

	// A favorite memory, a superseded one, a global one, and a note: the
	// frontmatter rides through the archive verbatim, so the export never has
	// to understand it.
	writeFile(t, dir, memFavPath, "---\nid: 01MEMFAV\nkind: gotcha\nfavorite: true\n---\nchroma races on boot\n")
	writeFile(t, dir, "memory/seam/old-boot-race.md",
		"---\nid: 01MEMOLD\nkind: gotcha\ninvalid_at: 2026-07-10T18:00:00Z\nsuperseded_by: 01MEMFAV\n---\nsuperseded\n")
	writeFile(t, dir, memGlobPath, "---\nid: 01MEMGLOB\nkind: convention\n---\nalways uv\n")
	writeFile(t, dir, notePath, "---\nid: 01NOTEA\n---\nexport design\n")
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// ---------------------------------------------------------------------------
// reading an archive back (the export tests own a minimal reader; the guarded
// extractor is a separate concern)
// ---------------------------------------------------------------------------

type tarEntry struct {
	hdr  tar.Header
	data []byte
}

func readArchive(t *testing.T, r io.Reader) []tarEntry {
	t.Helper()
	gz, err := gzip.NewReader(r)
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var out []tarEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		out = append(out, tarEntry{hdr: *hdr, data: data})
	}
	return out
}

func entryNames(entries []tarEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.hdr.Name)
	}
	return names
}

func entryByName(t *testing.T, entries []tarEntry, name string) tarEntry {
	t.Helper()
	for _, e := range entries {
		if e.hdr.Name == name {
			return e
		}
	}
	t.Fatalf("entry %q not found in %v", name, entryNames(entries))
	return tarEntry{}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestVacuumIntoAcceptsABoundParameter pins the form snapshotDB relies on. The
// plan allowed for a fallback (a quoted string literal, doubling each embedded
// single quote) in case the modernc driver rejected a parameter here; it does
// not, so there is no quoting path in the exporter at all, and this test is
// what keeps it that way.
func TestVacuumIntoAcceptsABoundParameter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, DBName)
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES ('01PIN', 'memory', 'seam', '', 'n', 'd', 'hello world')`)
	require.NoError(t, err)

	// A path with a single quote in it is exactly what a literal-quoting
	// implementation would get wrong, so the fixture includes one.
	target := filepath.Join(dir, "o'brien snapshot.db")
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, target)
	require.NoError(t, err, "modernc accepts a bound parameter as the VACUUM INTO target")

	snap, err := sql.Open("sqlite", "file:"+target+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })
	var n int
	require.NoError(t, snap.QueryRow(`SELECT COUNT(*) FROM fts`).Scan(&n))
	require.Equal(t, 1, n)

	// The target must not exist: SQLite refuses rather than overwriting, which
	// is why the exporter vacuums into a fresh temp dir.
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, target)
	require.Error(t, err, "VACUUM INTO must not clobber an existing file")
}

func TestExport_LayoutManifestFirstPermissionsAndCounts(t *testing.T) {
	dir := seedInstance(t)

	var buf bytes.Buffer
	manifest, err := Export(context.Background(), ExportOptions{
		DataDir:          dir,
		Out:              &buf,
		SeamlessdVersion: "0.4.11-test",
		Host:             "exporthost",
	})
	require.NoError(t, err)

	entries := readArchive(t, bytes.NewReader(buf.Bytes()))
	require.Equal(t, []string{
		ManifestName,
		DBName,
		memGlobPath,
		memFavPath,
		"memory/seam/old-boot-race.md",
		notePath,
	}, entryNames(entries), "manifest first, then the snapshot, then memory/ before notes/ in walk order")

	for _, e := range entries {
		require.Equalf(t, byte(tar.TypeReg), e.hdr.Typeflag, "%s must be a regular file entry", e.hdr.Name)
		require.Equalf(t, int64(0o600), e.hdr.Mode, "%s must be mode 0600", e.hdr.Name)
		require.Zerof(t, e.hdr.Uid, "%s must carry no uid", e.hdr.Name)
		require.Zerof(t, e.hdr.Gid, "%s must carry no gid", e.hdr.Name)
		require.Emptyf(t, e.hdr.Uname, "%s must carry no user name", e.hdr.Name)
		require.Emptyf(t, e.hdr.Gname, "%s must carry no group name", e.hdr.Name)
		// FormatPAX on the writer means "PAX semantics when an extension is
		// needed, never GNU". A short ASCII name needs none, so the header on
		// the wire is the USTAR subset every PAX reader accepts.
		require.Zerof(t, e.hdr.Format&tar.FormatGNU, "%s must not be a GNU header", e.hdr.Name)
		require.NotZerof(t, e.hdr.Format&(tar.FormatUSTAR|tar.FormatPAX),
			"%s must be USTAR or PAX", e.hdr.Name)
	}

	// The manifest describes the archive it is the first entry of.
	var got Manifest
	require.NoError(t, json.Unmarshal(entryByName(t, entries, ManifestName).data, &got))
	require.Equal(t, FormatVersion, got.FormatVersion)
	require.Equal(t, store.LatestSchemaVersion(), got.SchemaVersion)
	require.Equal(t, "0.4.11-test", got.SeamlessdVersion)
	require.Equal(t, "exporthost", got.SourceHost)
	require.True(t, got.IncludesDB)
	require.Equal(t, []string{seedEmbModel}, got.EmbeddingModels)
	require.Equal(t, 3, got.Counts.MemoryFiles)
	require.Equal(t, 1, got.Counts.NoteFiles)
	require.Equal(t, 2, got.Counts.Tables["tasks"])
	require.Equal(t, 1, got.Counts.Tables["task_deps"])
	require.Equal(t, 1, got.Counts.Tables["sessions"])
	require.Equal(t, 1, got.Counts.Tables["trials"])
	require.Equal(t, 1, got.Counts.Tables["events"])
	require.Equal(t, 1, got.Counts.Tables["embeddings"])
	require.Equal(t, 1, got.Counts.Tables["projects"])
	require.Equal(t, 1, got.Counts.Tables["settings"])
	require.Contains(t, got.Counts.Tables, "schema_migrations",
		"the table list is enumerated from the snapshot, not transcribed here")

	// The returned manifest is the one that was written.
	require.Equal(t, got.Counts, manifest.Counts)
	require.Equal(t, got.SchemaVersion, manifest.SchemaVersion)
	require.True(t, got.CreatedAt.Equal(manifest.CreatedAt))
	require.False(t, manifest.CreatedAt.IsZero())

	// Markdown rides through byte-for-byte.
	for _, rel := range []string{memFavPath, memGlobPath, notePath} {
		want, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		require.NoError(t, err)
		require.Equal(t, want, entryByName(t, entries, rel).data, rel)
	}

	// The snapshot is a real database that passes quick_check and agrees with
	// the manifest.
	snapPath := filepath.Join(t.TempDir(), "restored.db")
	require.NoError(t, os.WriteFile(snapPath, entryByName(t, entries, DBName).data, 0o600))
	snap, err := sql.Open("sqlite", "file:"+snapPath+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	var check string
	require.NoError(t, snap.QueryRow(`PRAGMA quick_check`).Scan(&check))
	require.Equal(t, "ok", check)

	v, err := store.SchemaVersion(snap)
	require.NoError(t, err)
	require.Equal(t, got.SchemaVersion, v)
	for table, want := range got.Counts.Tables {
		var n int
		require.NoError(t, snap.QueryRow(`SELECT COUNT(*) FROM "`+table+`"`).Scan(&n))
		require.Equalf(t, want, n, "table %s", table)
	}
}

// TestExport_SkipsWhatIsNotCorpus covers the live data dir's real contents: a
// stray backups/ directory and seamlessd.log sit next to the two trees, and the
// trees themselves collect dot files and (if the owner arranges it) symlinks.
// None of it may enter the archive.
func TestExport_SkipsWhatIsNotCorpus(t *testing.T) {
	dir := seedInstance(t)

	// Outside the two trees entirely.
	writeFile(t, dir, "backups/seam-2026-07-01.md", "a hand-rolled backup")
	writeFile(t, dir, "seamlessd.log", "INFO daemon started")
	writeFile(t, dir, "config.yaml", "mcp:\n  api_key: secret\n")

	// Inside the trees, but not corpus.
	writeFile(t, dir, "memory/seam/.DS_Store", "finder junk")
	writeFile(t, dir, "memory/seam/scratch.txt", "not markdown")
	writeFile(t, dir, "memory/seam/.seamless-tmp-01ABC", "a half-written atomic write")
	writeFile(t, dir, "memory/.hidden/secret.md", "inside a dot directory")

	// A symlinked file and a symlinked directory, both pointing at real .md
	// content outside the data dir.
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "leaked.md"), []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "leaked.md"),
		filepath.Join(dir, "memory", "seam", "linked.md")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "memory", "linkdir")))

	var buf bytes.Buffer
	manifest, err := Export(context.Background(), ExportOptions{DataDir: dir, Out: &buf, Host: "h"})
	require.NoError(t, err)

	names := entryNames(readArchive(t, bytes.NewReader(buf.Bytes())))
	require.Equal(t, []string{
		ManifestName,
		DBName,
		memGlobPath,
		memFavPath,
		"memory/seam/old-boot-race.md",
		notePath,
	}, names)
	require.Equal(t, 3, manifest.Counts.MemoryFiles)
	require.Equal(t, 1, manifest.Counts.NoteFiles)
}

// TestExport_SymlinkedTreeRootIsRefused: skipping it would produce a
// successful-looking export of an empty corpus, which is the worst possible
// backup.
func TestExport_SymlinkedTreeRootIsRefused(t *testing.T) {
	dir := seedInstance(t)
	elsewhere := t.TempDir()
	require.NoError(t, os.RemoveAll(filepath.Join(dir, NotesTree)))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(dir, NotesTree)))

	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{DataDir: dir, Out: &buf, Host: "h"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsafeEntry)
}

// TestExport_ConsistentUnderAPendingWrite is the reason the snapshot is a
// VACUUM INTO rather than a file copy: a second handle holds an uncommitted
// write (exactly what a running daemon looks like mid-request), and the archive
// must contain the committed state without it.
func TestExport_ConsistentUnderAPendingWrite(t *testing.T) {
	dir := seedInstance(t)
	ctx := context.Background()

	writer, err := store.Open(filepath.Join(dir, DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })

	tx, err := writer.BeginTx(ctx, nil)
	require.NoError(t, err)
	now := core.FormatTime(time.Now().UTC())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tasks (id, project_slug, title, body, status, created_by,
		                   plan_slug, claimed_by, lease_expires_at, created_at, updated_at, closed_at)
		VALUES (?, 'seam', 'uncommitted', '', 'open', '', '', '', NULL, ?, ?, NULL)`,
		"01TASKPENDING", now, now)
	require.NoError(t, err, "the pending write must reach the WAL, not just the tx object")

	var buf bytes.Buffer
	manifest, err := Export(ctx, ExportOptions{DataDir: dir, Out: &buf, Host: "h"})
	require.NoError(t, err, "an in-flight writer must not block the snapshot")
	require.Equal(t, 2, manifest.Counts.Tables["tasks"], "the uncommitted task must not be counted")

	require.NoError(t, tx.Rollback())

	snapPath := filepath.Join(t.TempDir(), "restored.db")
	entries := readArchive(t, bytes.NewReader(buf.Bytes()))
	require.NoError(t, os.WriteFile(snapPath, entryByName(t, entries, DBName).data, 0o600))
	snap, err := sql.Open("sqlite", "file:"+snapPath+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	var n int
	require.NoError(t, snap.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&n))
	require.Equal(t, 2, n)
	require.NoError(t, snap.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, "01TASKPENDING").Scan(&n))
	require.Equal(t, 0, n, "an uncommitted row must never appear in a snapshot")
}

func TestExport_NoDB(t *testing.T) {
	dir := seedInstance(t)

	var buf bytes.Buffer
	manifest, err := Export(context.Background(), ExportOptions{
		DataDir: dir, Out: &buf, NoDB: true, Host: "h",
	})
	require.NoError(t, err)

	names := entryNames(readArchive(t, bytes.NewReader(buf.Bytes())))
	require.NotContains(t, names, DBName)
	require.Equal(t, ManifestName, names[0])
	require.Len(t, names, 5, "manifest plus the four markdown files")

	// The manifest describes no database rather than describing one that is not
	// in the archive.
	require.False(t, manifest.IncludesDB)
	require.Zero(t, manifest.SchemaVersion)
	require.Empty(t, manifest.Counts.Tables)
	require.Empty(t, manifest.EmbeddingModels)
	require.Equal(t, 3, manifest.Counts.MemoryFiles)
	require.Equal(t, 1, manifest.Counts.NoteFiles)
}

// A knowledge-only export must work on a data dir that has no database at all,
// which is what makes --no-db usable against a git-synced corpus checkout.
func TestExport_NoDB_WithoutADatabase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, memGlobPath, "---\nid: 01MEMGLOB\n---\nalways uv\n")

	var buf bytes.Buffer
	manifest, err := Export(context.Background(), ExportOptions{
		DataDir: dir, Out: &buf, NoDB: true, Host: "h",
	})
	require.NoError(t, err)
	require.Equal(t, []string{ManifestName, memGlobPath},
		entryNames(readArchive(t, bytes.NewReader(buf.Bytes()))))
	require.Equal(t, 1, manifest.Counts.MemoryFiles)
	require.Zero(t, manifest.Counts.NoteFiles, "an absent notes tree is empty, not an error")
}

func TestExport_MissingDatabaseIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, memGlobPath, "---\nid: 01MEMGLOB\n---\nalways uv\n")

	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{DataDir: dir, Out: &buf, Host: "h"})
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Zero(t, buf.Len(), "a failed export writes nothing")
}

func TestExport_RejectsEmptyArguments(t *testing.T) {
	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{Out: &buf})
	require.Error(t, err)

	_, err = Export(context.Background(), ExportOptions{DataDir: t.TempDir()})
	require.Error(t, err)
}

// TestExport_LongNameUsesPAXNotGNU is the half of the format choice that a
// short-name archive cannot show. validate.Name allows a 255-character memory
// name, which overflows the 100-byte USTAR name field and cannot be split at a
// "/" into the USTAR prefix, so the entry needs an extension record -- and it
// must be the PAX one, because a GNU header is what several readers (and older
// bsdtar invocations) handle worst.
func TestExport_LongNameUsesPAXNotGNU(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a-very-long-memory-name-", 8) + "end" // 195 chars
	rel := "memory/_global/" + long + ".md"
	writeFile(t, dir, rel, "---\nid: 01MEMLONG\n---\nlong\n")

	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{DataDir: dir, Out: &buf, NoDB: true, Host: "h"})
	require.NoError(t, err)

	entries := readArchive(t, bytes.NewReader(buf.Bytes()))
	require.Equal(t, []string{ManifestName, rel}, entryNames(entries))

	e := entryByName(t, entries, rel)
	require.Equal(t, tar.FormatPAX, e.hdr.Format, "a long name must extend via PAX, not GNU")
	require.Equal(t, int64(0o600), e.hdr.Mode)
}

// stagingProbe is an Export destination that, on the first write it receives,
// records what is staged under dir: the archive is streamed while the database
// snapshot still sits in the staging directory.
type stagingProbe struct {
	dir      string
	buf      bytes.Buffer
	probed   bool
	staged   []string // seamless-export-* directories under dir at the first write
	snapshot bool     // whether the one staging directory held the snapshot
}

func (p *stagingProbe) Write(b []byte) (int, error) {
	if !p.probed {
		p.probed = true
		p.staged, _ = filepath.Glob(filepath.Join(p.dir, "seamless-export-*"))
		if len(p.staged) == 1 {
			_, err := os.Stat(filepath.Join(p.staged[0], DBName))
			p.snapshot = err == nil
		}
	}
	return p.buf.Write(b)
}

// The staging directory holds a full copy of the database, so where it goes is
// the caller's choice: a large snapshot must not have to fit on a small
// RAM-backed /tmp. It lands under TmpDir (the OS default when empty) while the
// archive is written, and is gone when Export returns.
func TestExport_StagesTheSnapshotUnderTmpDir(t *testing.T) {
	tests := []struct {
		name     string
		explicit bool // pass the parent as TmpDir, rather than as the OS default
	}{
		{name: "TmpDir names the parent", explicit: true},
		{name: "empty TmpDir keeps the OS default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := seedInstance(t)
			parent := t.TempDir()
			opts := ExportOptions{DataDir: dir, Host: "h"}
			if tt.explicit {
				opts.TmpDir = parent
			} else {
				// os.TempDir reads TMPDIR on Unix and TMP/TEMP on Windows.
				for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
					t.Setenv(key, parent)
				}
			}
			probe := &stagingProbe{dir: parent}
			opts.Out = probe

			_, err := Export(context.Background(), opts)
			require.NoError(t, err)
			require.True(t, probe.probed)
			require.Len(t, probe.staged, 1, "exactly one staging dir, under %s", parent)
			require.True(t, probe.snapshot, "the database snapshot is staged there")

			left, err := os.ReadDir(parent)
			require.NoError(t, err)
			require.Empty(t, left, "the staging dir is removed before Export returns")
			require.Contains(t, entryNames(readArchive(t, bytes.NewReader(probe.buf.Bytes()))), DBName)
		})
	}
}

// A TmpDir that does not exist is an error, never a silent fallback to the OS
// default the caller chose to avoid; nor does Export create it.
func TestExport_MissingTmpDirIsAnError(t *testing.T) {
	dir := seedInstance(t)
	missing := filepath.Join(t.TempDir(), "absent")

	var buf bytes.Buffer
	_, err := Export(context.Background(), ExportOptions{DataDir: dir, Out: &buf, Host: "h", TmpDir: missing})
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, err, missing)
	require.Zero(t, buf.Len(), "a failed export writes nothing")
	require.NoDirExists(t, missing)
}
