package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The fields the backups feature added to the manifest: why an archive was
// written, which install wrote it, what each project held, and a fingerprint
// a later backup compares itself with.

func TestExport_RecordsKindInstallProjectsAndFingerprint(t *testing.T) {
	src := seedIndexed(t, specA)
	data, m := exportWith(t, ExportOptions{DataDir: src, Kind: KindScheduled, InstallID: "01INSTALLAAAAAAAAAAAAAAAAA"})

	got, err := ReadManifest(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, m, got, "the manifest Export returns is the one in the archive")
	require.Equal(t, KindScheduled, got.Kind)
	require.Equal(t, "01INSTALLAAAAAAAAAAAAAAAAA", got.InstallID)
	require.Zero(t, got.ProjectsOmitted)
	require.Equal(t, []ProjectCounts{
		{Slug: "alpha", Memories: 1, Retired: 1, Notes: 1, Tasks: 2, Sessions: 1, Trials: 1},
		{Slug: "", Memories: 1},
	}, got.Projects, "largest first, counted from the snapshot's index")

	// Nothing changed since the export, so the live database fingerprints the
	// same: the backup scheduler's skip rests on exactly this.
	db, err := store.OpenExisting(filepath.Join(src, DBName))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	live, err := store.ContentFingerprint(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, live, got.Fingerprint)
}

func TestExport_NoDBCountsProjectsFromTheTrees(t *testing.T) {
	src := seedIndexed(t, specA)
	_, m := exportWith(t, ExportOptions{DataDir: src, NoDB: true})
	require.Equal(t, []ProjectCounts{
		{Slug: "alpha", Memories: 2, Notes: 1},
		{Slug: "", Memories: 1},
	}, m.Projects, "without an index every memory file counts")
	require.Empty(t, m.Fingerprint, "a fingerprint describes a database")
}

func TestCapProjects(t *testing.T) {
	var in []ProjectCounts
	for i := range maxManifestProjects + 5 {
		in = append(in, ProjectCounts{Slug: fmt.Sprintf("p%04d", i), Memories: i % 3})
	}
	got, omitted := capProjects(in)
	require.Len(t, got, maxManifestProjects)
	require.Equal(t, 5, omitted)
	require.Equal(t, 2, got[0].Memories, "largest first")
	require.Equal(t, "p0002", got[0].Slug, "equal sizes by slug")
	require.Zero(t, got[len(got)-1].Memories)

	none, omitted := capProjects(nil)
	require.Nil(t, none)
	require.Zero(t, omitted)
}

func TestReadManifest_ReadsWhatImportRefuses(t *testing.T) {
	newer := goodManifest()
	newer.SchemaVersion = store.LatestSchemaVersion() + 1
	got, err := ReadManifest(bytes.NewReader(craftArchive(t, newer, nil, nil)))
	require.NoError(t, err, "a list of backups shows one this build cannot restore")
	require.Equal(t, newer.SchemaVersion, got.SchemaVersion)
	_, err = Import(context.Background(), ImportOptions{DataDir: t.TempDir(), Src: bytes.NewReader(craftArchive(t, newer, nil, nil))})
	require.ErrorIs(t, err, ErrSchemaTooNew)

	other := goodManifest()
	other.FormatVersion = FormatVersion + 1
	got, err = ReadManifest(bytes.NewReader(craftArchive(t, other, nil, nil)))
	require.NoError(t, err)
	require.Equal(t, FormatVersion+1, got.FormatVersion)

	_, err = ReadManifest(bytes.NewReader([]byte("not a gzip stream")))
	require.ErrorIs(t, err, ErrNotArchive)
	_, err = ReadManifest(bytes.NewReader(craftArchive(t, nil,
		[]tar.Header{{Name: "memory/x/y.md", Typeflag: tar.TypeReg}}, []string{"body"})))
	require.ErrorIs(t, err, ErrNotArchive, "an archive whose first entry is not the manifest is not ours")
}

func TestVerify_AGoodArchive(t *testing.T) {
	data := exportOf(t, seedIndexed(t, specA))
	var copied bytes.Buffer
	v, err := Verify(context.Background(), bytes.NewReader(data), &copied)
	require.NoError(t, err)

	sum := sha256.Sum256(data)
	require.Equal(t, hex.EncodeToString(sum[:]), v.SHA256)
	require.Equal(t, int64(len(data)), v.Bytes)
	require.Equal(t, data, copied.Bytes(), "the copy is the archive, byte for byte")
	require.True(t, v.HasDB)
	require.Equal(t, 3, v.MemoryFiles)
	require.Equal(t, 1, v.NoteFiles)
	require.Positive(t, v.Unpacked)
}

func TestVerify_Refusals(t *testing.T) {
	good := exportOf(t, seedIndexed(t, specA))
	// The damaged byte is the stored CRC's first (the gzip trailer's), which
	// the stream's end always checks. A byte flipped at an arbitrary place in
	// the deflate data can land on bits that change nothing it decodes to (an
	// unused code length in a block's Huffman header): the archive's layout
	// moves with every schema change, and such an archive is not damaged.
	flipped := bytes.Clone(good)
	flipped[len(flipped)-8] ^= 0xff

	short := goodManifest()
	short.Counts.MemoryFiles = 2
	newer := goodManifest()
	newer.SchemaVersion = store.LatestSchemaVersion() + 1
	saysDB := goodManifest()
	saysDB.IncludesDB = true

	tests := []struct {
		name string
		data []byte
		is   error
	}{
		{"truncated", good[:len(good)/2], nil},
		{"a damaged byte", flipped, nil},
		{"trailing garbage", append(bytes.Clone(good), []byte("garbage")...), ErrNotArchive},
		{"fewer files than the manifest lists", craftArchive(t, short,
			[]tar.Header{{Name: "memory/alpha/a.md", Typeflag: tar.TypeReg}}, []string{"---\nid: 01A\n---\n"}), ErrIncomplete},
		{"a database the manifest denies", craftArchive(t, goodManifest(),
			[]tar.Header{{Name: DBName, Typeflag: tar.TypeReg}}, []string{"SQLite format 3\x00"}), ErrIncomplete},
		{"no database the manifest promises", craftArchive(t, saysDB, nil, nil), ErrIncomplete},
		{"an unsafe entry", craftArchive(t, goodManifest(),
			[]tar.Header{{Name: "memory/../../x.md", Typeflag: tar.TypeReg}}, []string{"x"}), ErrUnsafeEntry},
		{"a newer schema", craftArchive(t, newer, nil, nil), ErrSchemaTooNew},
		{"not an archive", []byte("plain text"), ErrNotArchive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Verify(context.Background(), bytes.NewReader(tt.data), nil)
			require.Error(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
		})
	}
}

func TestVerify_StopsWhenTheContextEnds(t *testing.T) {
	data := exportOf(t, seedIndexed(t, specA))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Verify(ctx, bytes.NewReader(data), nil)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestImport_FreshKeepsTheDestinationsLocks(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, "archived-lock")
	data := exportOf(t, src)

	dst := t.TempDir()
	rep, err := importInto(t, dst, data, func(o *ImportOptions) {
		o.KeepLocked = []LockedProject{
			{Slug: "alpha", Isolation: string(core.IsolationConfidential)},
			{Slug: "gamma", Isolation: string(core.IsolationSealed)},
		}
	})
	require.NoError(t, err)
	require.Equal(t, ModeFresh, rep.Mode)
	require.Equal(t, []string{"alpha", "archived-lock", "gamma"}, consoleLocksIn(t, dst))
	require.Equal(t, core.IsolationConfidential, isolationIn(t, dst, "alpha"))
	require.Equal(t, core.IsolationSealed, isolationIn(t, dst, "gamma"), "a kept lock the backup lacks is registered with its fence")
	require.NotNil(t, rep.Locks)
	require.Equal(t, []string{"alpha", "archived-lock", "gamma"}, rep.Locks.Locked)
}

func TestImport_FreshKeptLocksSurviveRemovingTheArchivesOwn(t *testing.T) {
	src := seedIndexed(t, specA)
	lockInConsole(t, src, "archived-lock")
	lockInConsole(t, src, "both")
	data := exportOf(t, src)

	dst := t.TempDir()
	rep, err := importInto(t, dst, data, func(o *ImportOptions) {
		o.OnLocks = func(context.Context, Mode, Manifest) (LockChoice, error) { return LocksRemove, nil }
		o.KeepLocked = []LockedProject{{Slug: "both", Isolation: string(core.IsolationSealed)}}
	})
	require.NoError(t, err)
	require.Equal(t, []string{"both"}, consoleLocksIn(t, dst), "the destination's own lock is not the archive's to remove")
	require.Equal(t, []string{"archived-lock"}, rep.Locks.Removed)
	require.Equal(t, []string{"both"}, rep.Locks.Locked)
}

func TestImport_MergeStagesUnderTmpDir(t *testing.T) {
	data := exportOf(t, seedIndexed(t, specA))
	dst := seedIndexed(t, specB)
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := importInto(t, dst, data, func(o *ImportOptions) { o.TmpDir = missing })
	require.Error(t, err)
	require.Contains(t, err.Error(), missing, "the merge stages where it is told, never quietly in the OS temp dir")

	_, err = importInto(t, dst, data, func(o *ImportOptions) { o.TmpDir = t.TempDir() })
	require.NoError(t, err)
}
