package archive

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
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// copyRel is a session's transcript copy, relative to a data dir.
func copyRel(id string) string { return TranscriptsTree + "/" + id + TranscriptSuffix }

// gzipOf compresses s as the transcripts service stores a copy.
func gzipOf(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(s))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// newSessionID mints a session ULID, the only id a copy is named by.
func newSessionID(t *testing.T) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	return id
}

// flagSession creates session id in db and flags it.
func flagSession(t *testing.T, db *sql.DB, id string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/" + strings.ToLower(id[len(id)-8:]), ProjectSlug: "alpha",
		Status: core.SessionCompleted, CreatedAt: at, UpdatedAt: at,
	}))
	_, err := store.SetSessionFlag(ctx, db, id, core.VerdictNotable, "worth re-reading", at)
	require.NoError(t, err)
}

// keepCopy writes data as session id's copy under dataDir and records it as
// kept, copied at at.
func keepCopy(t *testing.T, db *sql.DB, dataDir, id string, data []byte, at time.Time) {
	t.Helper()
	writeCopyFile(t, dataDir, id, data)
	recorded, err := store.SetTranscriptCopy(context.Background(), db, id, core.TranscriptCopied, int64(len(data))*3, at)
	require.NoError(t, err)
	require.True(t, recorded)
}

// writeCopyFile puts data on disk as session id's copy, as the service does:
// 0600 in a 0700 directory.
func writeCopyFile(t *testing.T, dataDir, id string, data []byte) {
	t.Helper()
	dir := filepath.Join(dataDir, TranscriptsTree)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+TranscriptSuffix), data, 0o600))
}

// copiesOn maps each file under dataDir's transcripts/ to its bytes.
func copiesOn(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(dataDir, TranscriptsTree))
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	require.NoError(t, err)
	out := map[string]string{}
	for _, de := range des {
		data, err := os.ReadFile(filepath.Join(dataDir, TranscriptsTree, de.Name()))
		require.NoError(t, err)
		out[de.Name()] = string(data)
	}
	return out
}

// copyRecord is the copy columns of session id's row in dataDir's database.
func copyRecord(t *testing.T, dataDir, id string) (verdict, state sql.NullString, bytes sql.NullInt64) {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dataDir, DBName))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, db.QueryRow(`SELECT verdict, transcript_copy, transcript_bytes FROM sessions WHERE id = ?`, id).
		Scan(&verdict, &state, &bytes))
	return verdict, state, bytes
}

// exportOpts writes an archive of dataDir with opts applied.
func exportOpts(t *testing.T, dataDir string, noDB bool) ([]byte, Manifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := Export(context.Background(), ExportOptions{
		DataDir: dataDir, Out: &buf, NoDB: noDB, SeamlessdVersion: "0.0.0-test", Host: "h", TmpDir: t.TempDir(),
	})
	require.NoError(t, err)
	return buf.Bytes(), m
}

// copyScene is a data dir holding a kept copy, and everything around it an
// export must leave out.
type copyScene struct {
	dir  string
	kept string // the session whose copy the snapshot records as kept
	data []byte // its copy's bytes
}

func newCopyScene(t *testing.T) copyScene {
	t.Helper()
	dir := seedInstance(t)
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	now := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

	sc := copyScene{dir: dir, kept: newSessionID(t), data: gzipOf(t, `{"type":"user","text":"the kept one"}`+"\n")}
	flagSession(t, db, sc.kept, now)
	keepCopy(t, db, dir, sc.kept, sc.data, now)

	// Flagged, but its copy was never made: a file under its name is not one
	// the record keeps.
	tooLarge := newSessionID(t)
	flagSession(t, db, tooLarge, now)
	_, err = store.SetTranscriptCopy(context.Background(), db, tooLarge, core.TranscriptTooLarge, 0, now)
	require.NoError(t, err)
	writeCopyFile(t, dir, tooLarge, []byte("not recorded as kept"))

	// A copy no flag names (an unflag whose removal failed).
	writeCopyFile(t, dir, newSessionID(t), []byte("an orphan"))

	// Flagged and recorded as kept, but its name on disk is a link.
	linked := newSessionID(t)
	flagSession(t, db, linked, now)
	_, err = store.SetTranscriptCopy(context.Background(), db, linked, core.TranscriptCopied, 10, now)
	require.NoError(t, err)
	outside := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(outside, []byte("outside the data dir"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, TranscriptsTree, linked+TranscriptSuffix)))

	// Flagged and recorded as kept, cleared from disk since.
	gone := newSessionID(t)
	flagSession(t, db, gone, now)
	_, err = store.SetTranscriptCopy(context.Background(), db, gone, core.TranscriptCopied, 10, now)
	require.NoError(t, err)

	// A partial copy the service was still writing.
	writeCopyFile(t, dir, ".tmp-123", []byte("partial"))
	return sc
}

// ---------------------------------------------------------------------------
// export
// ---------------------------------------------------------------------------

// An archive with the database carries exactly the copies its snapshot
// records as kept, 0600 like every entry, after the markdown, and its manifest
// counts them; one without the database carries none.
func TestExport_CarriesTheCopiesTheSnapshotKeeps(t *testing.T) {
	sc := newCopyScene(t)
	for _, tc := range []struct {
		name  string
		noDB  bool
		want  []string
		bytes int64
	}{
		{"with the database", false, []string{copyRel(sc.kept)}, int64(len(sc.data))},
		{"without the database", true, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, m := exportOpts(t, sc.dir, tc.noDB)
			entries := readArchive(t, bytes.NewReader(data))
			var got []string
			for _, e := range entries {
				if isTranscriptEntry(e.hdr.Name) {
					got = append(got, e.hdr.Name)
					require.Equal(t, int64(0o600), e.hdr.Mode)
					require.Equal(t, sc.data, e.data, "the copy's bytes, as stored")
				}
			}
			require.Equal(t, tc.want, got)
			require.Equal(t, len(tc.want), m.Counts.TranscriptFiles)
			require.Equal(t, tc.bytes, m.Counts.TranscriptBytes)
			if len(tc.want) > 0 {
				names := entryNames(entries)
				require.Equal(t, copyRel(sc.kept), names[len(names)-1], "the copies follow the markdown")
			} else {
				raw, err := json.Marshal(m)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "transcript", "an archive holding none says nothing of copies")
			}

			v, err := Verify(context.Background(), bytes.NewReader(data), nil)
			require.NoError(t, err)
			require.Equal(t, len(tc.want), v.TranscriptFiles)
		})
	}
}

// A snapshot from before the session flag records no copy, so its archive
// carries none whatever the disk holds.
func TestExport_ASnapshotWithoutTheCopyRecordCarriesNone(t *testing.T) {
	sc := newCopyScene(t)
	db, err := sql.Open("sqlite", filepath.Join(sc.dir, DBName))
	require.NoError(t, err)
	_, err = db.Exec(`DROP INDEX idx_sessions_flagged`)
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE sessions DROP COLUMN transcript_copy`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	data, m := exportOpts(t, sc.dir, false)
	require.Zero(t, m.Counts.TranscriptFiles)
	for _, e := range readArchive(t, bytes.NewReader(data)) {
		require.False(t, isTranscriptEntry(e.hdr.Name), e.hdr.Name)
	}
}

// A copies directory that is a link is refused, as a markdown tree's is.
func TestExport_ASymlinkedCopiesDirIsRefused(t *testing.T) {
	sc := newCopyScene(t)
	real := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.Rename(filepath.Join(sc.dir, TranscriptsTree), real))
	require.NoError(t, os.Symlink(real, filepath.Join(sc.dir, TranscriptsTree)))
	_, err := Export(context.Background(), ExportOptions{DataDir: sc.dir, Out: io.Discard, Host: "h", TmpDir: t.TempDir()})
	require.ErrorIs(t, err, ErrUnsafeEntry)
}

// allocated is the heap the process allocated while fn ran: what a
// whole-file buffer would show, however short-lived.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A copy is streamed both ways: one larger than any markdown entry may be
// (and than a whole-file read would cost) is written and restored with an
// allocation far below its size. The file is sparse, so the disk pays for
// it only once it is restored.
func TestTranscripts_AreStreamedNotBuffered(t *testing.T) {
	if testing.Short() {
		t.Skip("writes and restores an 80 MB copy")
	}
	dir := seedInstance(t)
	db, err := store.OpenExisting(filepath.Join(dir, DBName))
	require.NoError(t, err)
	id := newSessionID(t)
	now := time.Now().UTC()
	flagSession(t, db, id, now)
	const size = maxEntryBytes + 16<<20
	require.NoError(t, os.MkdirAll(filepath.Join(dir, TranscriptsTree), 0o700))
	f, err := os.OpenFile(filepath.Join(dir, copyRel(id)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	_, err = store.SetTranscriptCopy(context.Background(), db, id, core.TranscriptCopied, size, now)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	archivePath := filepath.Join(t.TempDir(), "a.tar.gz")
	out, err := os.Create(archivePath)
	require.NoError(t, err)
	var m Manifest
	wrote := allocated(func() {
		m, err = Export(context.Background(), ExportOptions{DataDir: dir, Out: out, Host: "h", TmpDir: t.TempDir()})
	})
	require.NoError(t, err)
	require.NoError(t, out.Close())
	require.Equal(t, 1, m.Counts.TranscriptFiles)
	require.Equal(t, int64(size), m.Counts.TranscriptBytes)
	require.Less(t, wrote, uint64(size/4), "the export held the copy in memory")

	dst := filepath.Join(t.TempDir(), "restored")
	in, err := os.Open(archivePath)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	var rep *ImportReport
	read := allocated(func() {
		rep, err = Import(context.Background(), ImportOptions{DataDir: dst, Src: in, Logger: quietLogger()})
	})
	require.NoError(t, err)
	require.Equal(t, 1, rep.Transcripts)
	require.Less(t, read, uint64(size/4), "the restore held the copy in memory")
	fi, err := os.Stat(filepath.Join(dst, copyRel(id)))
	require.NoError(t, err)
	require.Equal(t, int64(size), fi.Size())
}

// ---------------------------------------------------------------------------
// extract
// ---------------------------------------------------------------------------

// A fresh restore puts the copies back beside the rows that name them: 0600
// in a 0700 directory, byte for byte, and the dry run predicts them.
func TestImport_FreshRestoresTheCopiesWithTheirRows(t *testing.T) {
	sc := newCopyScene(t)
	data, _ := exportOpts(t, sc.dir, false)

	dry, err := Import(context.Background(), ImportOptions{DataDir: filepath.Join(t.TempDir(), "dry"), Src: bytes.NewReader(data), DryRun: true, Logger: quietLogger()})
	require.NoError(t, err)
	require.Equal(t, 1, dry.Transcripts)

	dst := filepath.Join(t.TempDir(), "restored")
	rep, err := Import(context.Background(), ImportOptions{DataDir: dst, Src: bytes.NewReader(data), Logger: quietLogger()})
	require.NoError(t, err)
	require.Equal(t, ModeFresh, rep.Mode)
	require.Equal(t, 1, rep.Transcripts)
	require.Contains(t, rep.String(), "transcript copies: 1")
	require.Equal(t, map[string]string{sc.kept + TranscriptSuffix: string(sc.data)}, copiesOn(t, dst))
	verdict, state, _ := copyRecord(t, dst, sc.kept)
	require.Equal(t, string(core.VerdictNotable), verdict.String)
	require.Equal(t, string(core.TranscriptCopied), state.String)

	if runtime.GOOS != "windows" {
		di, err := os.Stat(filepath.Join(dst, TranscriptsTree))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), di.Mode().Perm())
		fi, err := os.Stat(filepath.Join(dst, copyRel(sc.kept)))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	svc, err := transcripts.New(transcripts.Config{DataDir: dst})
	require.NoError(t, err)
	require.True(t, svc.Kept(sc.kept), "the transcripts service finds the restored copy")
}

// The extractor takes a transcripts/ entry only as a regular file named
// <session ULID>.jsonl.gz, bounded in size, and only beside a database;
// anything else there is refused before a byte is written, and a refusal
// leaves nothing behind.
func TestImport_TranscriptEntriesAreGuarded(t *testing.T) {
	id := newSessionID(t)
	withDB := goodManifest()
	withDB.IncludesDB = true
	reg := func(name string) tar.Header { return tar.Header{Typeflag: tar.TypeReg, Name: name} }
	tests := []struct {
		name     string
		manifest Manifest
		entries  []tar.Header
		bodies   []string
	}{
		{"not a ULID", withDB, []tar.Header{reg(TranscriptsTree + "/not-a-session" + TranscriptSuffix)}, []string{"x"}},
		{"a ULID not in its canonical form", withDB, []tar.Header{reg(TranscriptsTree + "/" + strings.ToLower(id) + TranscriptSuffix)}, []string{"x"}},
		{"another suffix", withDB, []tar.Header{reg(TranscriptsTree + "/" + id + ".jsonl")}, []string{"x"}},
		{"a suffix past the copy's", withDB, []tar.Header{reg(TranscriptsTree + "/" + id + TranscriptSuffix + ".md")}, []string{"x"}},
		{"a nested name", withDB, []tar.Header{reg(TranscriptsTree + "/alpha/" + id + TranscriptSuffix)}, []string{"x"}},
		{"a partial copy's name", withDB, []tar.Header{reg(TranscriptsTree + "/.tmp-123")}, []string{"x"}},
		{"the directory itself", withDB, []tar.Header{{Typeflag: tar.TypeDir, Name: TranscriptsTree}}, []string{""}},
		{"a symlink", withDB, []tar.Header{{Typeflag: tar.TypeSymlink, Name: copyRel(id), Linkname: "/etc/passwd"}}, []string{""}},
		{"a hardlink", withDB, []tar.Header{{Typeflag: tar.TypeLink, Name: copyRel(id), Linkname: DBName}}, []string{""}},
		{"a copy without a database", goodManifest(), []tar.Header{reg(copyRel(id))}, []string{"x"}},
		{"a copy twice", withDB, []tar.Header{reg(copyRel(id)), reg(copyRel(id))}, []string{"x", "y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := craftArchive(t, tt.manifest, tt.entries, tt.bodies)
			dst := t.TempDir()
			_, err := Import(context.Background(), ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
			require.ErrorIs(t, err, ErrUnsafeEntry)
			require.Empty(t, dirEntryNames(t, dst), "a refused archive leaves nothing behind")
			_, err = Verify(context.Background(), bytes.NewReader(data), nil)
			require.ErrorIs(t, err, ErrUnsafeEntry)
		})
	}

	t.Run("over the size a copy may be", func(t *testing.T) {
		// The header claims more than the bound and no body follows: the
		// refusal is made on the header, before anything is read.
		data := oversizeCopyArchive(t, withDB, copyRel(id), maxTranscriptEntryBytes+1)
		dst := t.TempDir()
		_, err := Import(context.Background(), ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
		require.ErrorIs(t, err, ErrUnsafeEntry)
		require.ErrorContains(t, err, "a transcript copy may be")
		require.Empty(t, dirEntryNames(t, dst))
		_, err = Verify(context.Background(), bytes.NewReader(data), nil)
		require.ErrorIs(t, err, ErrUnsafeEntry)
	})

	t.Run("a good copy ahead of a bad entry is rolled back", func(t *testing.T) {
		data := craftArchive(t, withDB,
			[]tar.Header{reg(copyRel(id)), reg("../x.md")}, []string{"a copy", "evil"})
		dst := t.TempDir()
		_, err := Import(context.Background(), ImportOptions{DataDir: dst, Src: bytes.NewReader(data)})
		require.ErrorIs(t, err, ErrUnsafeEntry)
		require.Empty(t, dirEntryNames(t, dst))
	})
}

// oversizeCopyArchive is an archive whose one entry after the manifest is
// name's header claiming size bytes, cut off right after it.
func oversizeCopyArchive(t *testing.T, m Manifest, name string, size int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mj, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeReg, Name: ManifestName, Size: int64(len(mj)), Mode: 0o600}))
	_, err = tw.Write(mj)
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Format: tar.FormatPAX, Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600}))
	require.NoError(t, gz.Close()) // the tar is never closed: its body is not there
	return buf.Bytes()
}

// A manifest that names more or fewer copies than the archive holds is an
// incomplete archive.
func TestVerify_CountsTheCopies(t *testing.T) {
	id := newSessionID(t)
	m := goodManifest()
	m.IncludesDB = true
	m.Counts.TranscriptFiles = 2
	data := craftArchive(t, m,
		[]tar.Header{{Typeflag: tar.TypeReg, Name: DBName}, {Typeflag: tar.TypeReg, Name: copyRel(id)}},
		[]string{"SQLite format 3\x00", "a copy"})
	_, err := Verify(context.Background(), bytes.NewReader(data), nil)
	require.ErrorIs(t, err, ErrIncomplete)
	require.ErrorContains(t, err, "1 transcript copies, the manifest lists 2")
}

// A copy an interrupted extraction left under its temporary name is one the
// transcripts service sweeps when the daemon starts.
func TestTranscripts_AnInterruptedExtractionIsSwept(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, TranscriptsTree, transcriptTempPrefix+"0123456789abcdef")
	require.NoError(t, os.MkdirAll(filepath.Dir(partial), 0o700))
	require.NoError(t, os.WriteFile(partial, []byte("half a copy"), 0o600))
	_, err := transcripts.New(transcripts.Config{DataDir: dir})
	require.NoError(t, err)
	require.NoFileExists(t, partial)
}

// ---------------------------------------------------------------------------
// merge
// ---------------------------------------------------------------------------

// A merge brings a copy only for a session whose row here holds the archive's
// own copy record after the row pass: a session the merge adds, or one this
// instance has with the same copy, missing from its disk. A session here with
// a copy of its own, a different copy record or no flag keeps what it has,
// and no file is ever replaced. The dry run predicts the real run, and a
// merge run again brings nothing more.
func TestImport_MergeBringsTheCopiesOfTheSessionsItBrings(t *testing.T) {
	ctx := context.Background()
	src := seedInstance(t)
	srcDB, err := store.OpenExisting(filepath.Join(src, DBName))
	require.NoError(t, err)
	at := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

	// Five sessions both instances will hold, each with a copy.
	type kept struct{ id, data string }
	var same, missing, recopied, unflagged, ownFile kept
	for _, k := range []*kept{&same, &missing, &recopied, &unflagged, &ownFile} {
		k.id, k.data = newSessionID(t), "archive copy of a session both hold"
		flagSession(t, srcDB, k.id, at)
		keepCopy(t, srcDB, src, k.id, []byte(k.data+" "+k.id), at)
	}
	require.NoError(t, srcDB.Close())

	// The destination: a fresh restore of the same data, then changed.
	dst := filepath.Join(t.TempDir(), "dst")
	first, _ := exportOpts(t, src, false)
	_, err = Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(first), Logger: quietLogger()})
	require.NoError(t, err)
	dstDB, err := store.OpenExisting(filepath.Join(dst, DBName))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dst, copyRel(missing.id))))
	require.NoError(t, os.Remove(filepath.Join(dst, copyRel(recopied.id))))
	_, err = store.SetTranscriptCopy(ctx, dstDB, recopied.id, core.TranscriptCopied, 99, at.Add(time.Hour))
	require.NoError(t, err)
	_, _, err = store.ClearSessionFlag(ctx, dstDB, unflagged.id)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dst, copyRel(unflagged.id))))
	writeCopyFile(t, dst, ownFile.id, []byte("this instance's own copy"))
	require.NoError(t, dstDB.Close())

	// The archive gains a session the destination does not have.
	srcDB, err = store.OpenExisting(filepath.Join(src, DBName))
	require.NoError(t, err)
	fresh := newSessionID(t)
	flagSession(t, srcDB, fresh, at)
	keepCopy(t, srcDB, src, fresh, []byte("a session only the archive holds"), at)
	require.NoError(t, srcDB.Close())
	second, m := exportOpts(t, src, false)
	require.Equal(t, 6, m.Counts.TranscriptFiles)

	dry, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(second), DryRun: true, Logger: quietLogger()})
	require.NoError(t, err)
	require.Equal(t, ModeMerge, dry.Mode)
	require.Equal(t, 2, dry.Transcripts, "the dry run predicts the real run")
	require.NotContains(t, copiesOn(t, dst), fresh+TranscriptSuffix, "a dry run writes nothing")

	rep, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(second), Logger: quietLogger()})
	require.NoError(t, err)
	require.Equal(t, 2, rep.Transcripts)
	got := copiesOn(t, dst)
	require.Equal(t, "a session only the archive holds", got[fresh+TranscriptSuffix], "a session the merge adds brings its copy")
	require.Equal(t, missing.data+" "+missing.id, got[missing.id+TranscriptSuffix], "the same copy, missing here, comes back")
	require.NotContains(t, got, recopied.id+TranscriptSuffix, "a copy record of this instance's own is not the archive's copy")
	require.NotContains(t, got, unflagged.id+TranscriptSuffix, "a flag cleared here stays cleared")
	require.Equal(t, "this instance's own copy", got[ownFile.id+TranscriptSuffix], "a file here is never replaced")
	require.Equal(t, same.data+" "+same.id, got[same.id+TranscriptSuffix])
	verdict, _, _ := copyRecord(t, dst, unflagged.id)
	require.False(t, verdict.Valid, "the merge does not bring the flag back either")

	again, err := Import(ctx, ImportOptions{DataDir: dst, Src: bytes.NewReader(second), Logger: quietLogger()})
	require.NoError(t, err)
	require.Zero(t, again.Transcripts, "a merge run again brings nothing more")
	require.Equal(t, got, copiesOn(t, dst))
}

// quietLogger discards the import's log lines.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
