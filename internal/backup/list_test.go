package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/store"
)

func TestList_ShowsWhatEachBackupIs(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	ctx := context.Background()

	_, err := h.svc.BackUpNow(ctx)
	require.NoError(t, err)
	st := h.idle()
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome, st.LastRun.Error)
	mine := st.LastRun.Name

	put := func(dir, name string, data []byte) {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	at := func(day int) time.Time { return time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC) }

	newer := archive.Manifest{FormatVersion: 1, SchemaVersion: store.LatestSchemaVersion() + 1, IncludesDB: true, CreatedAt: at(1), SeamlessdVersion: "0.9.0"}
	put(h.folder, "seamless-test-host-20261101T000000Z.tar.gz", craftArchive(t, newer))
	other := archive.Manifest{FormatVersion: 1, IncludesDB: true, CreatedAt: at(2), Kind: archive.KindScheduled, InstallID: "01OTHERINSTALLAAAAAAAAAAAA", SourceHost: "test-host"}
	put(h.folder, "seamless-test-host-20261102T000000Z-auto.tar.gz", craftArchive(t, other))
	put(h.folder, "seamless-test-host-20261103T000000Z.tar.gz", []byte("not a gzip stream"))
	exported := archive.Manifest{FormatVersion: 1, IncludesDB: true, CreatedAt: at(4)}
	put(h.folder, "my-export.tar.gz", craftArchive(t, exported))
	put(h.folder, "random.tar.gz", []byte("junk"))
	put(h.folder, "notes.txt", []byte("junk"))
	put(h.folder, ".seamless-partial-01X-01Y.tmp", []byte("junk"))
	require.NoError(t, os.Mkdir(filepath.Join(h.folder, "dir.tar.gz"), 0o700))
	pre := archive.Manifest{FormatVersion: 1, IncludesDB: true, CreatedAt: at(5), Kind: archive.KindPreUpdate}
	put(LocalDir(dataDir), "pre-update-v0.8.0-20261105T000000Z.tar.gz", craftArchive(t, pre))

	l, err := h.svc.List(ctx)
	require.NoError(t, err)
	require.Equal(t, h.folder, l.Dir)
	require.Equal(t, FolderReady, l.DirCheck.State)
	require.Equal(t, LocalDir(dataDir), l.Local)

	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Ref)
	}
	require.Equal(t, []string{
		"folder:" + mine, // made today by the real clock, newest
		"local:pre-update-v0.8.0-20261105T000000Z.tar.gz",
		"folder:my-export.tar.gz",
		"folder:seamless-test-host-20261103T000000Z.tar.gz",
		"folder:seamless-test-host-20261102T000000Z-auto.tar.gz",
		"folder:seamless-test-host-20261101T000000Z.tar.gz",
	}, names, "newest first; junk, dot files and directories are not listed")

	byRef := func(ref string) Entry {
		e, ok := l.Find(ref)
		require.True(t, ok, ref)
		return e
	}
	e := byRef("folder:" + mine)
	require.True(t, e.Ours)
	require.False(t, e.FromAnother)
	require.Equal(t, archive.KindManual, e.Kind)
	require.NotNil(t, e.Manifest)
	require.Equal(t, []archive.ProjectCounts{{Slug: "alpha", Name: "Alpha", Memories: 0, Tasks: 1}}, e.Manifest.Projects)
	require.NotEmpty(t, e.Fingerprint())

	e = byRef("folder:seamless-test-host-20261101T000000Z.tar.gz")
	require.True(t, e.TooNew, "a newer schema is shown, and cannot be restored")
	require.Equal(t, "0.9.0", e.Manifest.SeamlessdVersion)

	e = byRef("folder:seamless-test-host-20261102T000000Z-auto.tar.gz")
	require.True(t, e.FromAnother)
	require.False(t, e.Ours)

	e = byRef("folder:seamless-test-host-20261103T000000Z.tar.gz")
	require.Nil(t, e.Manifest)
	require.Contains(t, e.Unreadable, "damaged")
	require.Equal(t, archive.KindManual, e.Kind, "the name still says what it was meant to be")
	require.Equal(t, at(3), e.CreatedAt)

	e = byRef("folder:my-export.tar.gz")
	require.False(t, e.Named)
	require.Empty(t, e.Kind, "a plain export")
	require.False(t, e.Ours)
	require.False(t, e.FromAnother, "an archive naming no install is no other install's")

	e = byRef("local:pre-update-v0.8.0-20261105T000000Z.tar.gz")
	require.Equal(t, WhereLocal, e.Where)
	require.Equal(t, archive.KindPreUpdate, e.Kind)
	require.Equal(t, "0.8.0", e.Parsed.FromVersion)
}

func TestList_ReadsEachManifestOnce(t *testing.T) {
	dir := t.TempDir()
	m := archive.Manifest{FormatVersion: 1, IncludesDB: true, CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Fingerprint: "aaaa"}
	path := filepath.Join(dir, "seamless-h-20261101T000000Z.tar.gz")
	require.NoError(t, os.WriteFile(path, craftArchive(t, m), 0o600))
	fi, err := os.Stat(path)
	require.NoError(t, err)

	l := lister{folders: folders{dataDir: t.TempDir()}, cache: &manifestCache{}}
	got, _ := l.listDir(dir, WhereFolder, "", nil, map[string]bool{})
	require.Len(t, got, 1)
	require.Equal(t, "aaaa", got[0].Fingerprint())

	// The same size and time: the cached manifest answers, unread.
	require.NoError(t, os.WriteFile(path, make([]byte, fi.Size()), 0o600))
	require.NoError(t, os.Chtimes(path, fi.ModTime(), fi.ModTime()))
	got, _ = l.listDir(dir, WhereFolder, "", nil, map[string]bool{})
	require.Equal(t, "aaaa", got[0].Fingerprint(), "unchanged by size and time: not read again")

	// A new time: read again.
	later := fi.ModTime().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, later, later))
	got, _ = l.listDir(dir, WhereFolder, "", nil, map[string]bool{})
	require.Nil(t, got[0].Manifest)
	require.NotEmpty(t, got[0].Unreadable)

	// A file gone from a listing leaves the cache.
	require.NoError(t, os.Remove(path))
	l.list(dir, "", nil)
	require.Empty(t, l.cache.m)
}

func TestList_FolderProblems(t *testing.T) {
	data := t.TempDir()
	l := lister{folders: folders{dataDir: data, goos: "linux"}, cache: &manifestCache{}}

	missing := filepath.Join(t.TempDir(), "not-yet")
	got := l.list(missing, "", nil)
	require.Equal(t, FolderWillCreate, got.DirCheck.State, "a folder not made yet lists empty")
	require.Empty(t, got.Entries)

	got = l.list(filepath.Join(data, "memory"), "", nil)
	require.Equal(t, FolderInsideDataDir, got.DirCheck.State, "never read, whatever the config says")

	got = l.list("/mnt/seamless-test-absent-disk-7f3a/Backups", "", nil)
	require.Equal(t, FolderDisconnected, got.DirCheck.State)

	got = l.list(LocalDir(data), "", nil)
	require.Empty(t, got.Local, "the local folder chosen is listed once")
}

func TestParseRef(t *testing.T) {
	where, name, err := parseRef("folder:seamless-h-20261101T000000Z.tar.gz")
	require.NoError(t, err)
	require.Equal(t, WhereFolder, where)
	require.Equal(t, "seamless-h-20261101T000000Z.tar.gz", name)
	for _, bad := range []string{"", "folder:", "elsewhere:x.tar.gz", "folder:../x.tar.gz", "folder:a/b", `local:a\b`, "local:..", "folder:.schedule.json", "/etc/passwd"} {
		_, _, err := parseRef(bad)
		require.ErrorIs(t, err, ErrNoSuchBackup, bad)
	}
}
