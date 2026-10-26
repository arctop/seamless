package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// throwawayInstance builds a data dir with a migrated seam.db and two markdown
// files, points config.Load at it via a temp seamless.yaml, and asserts that
// what config.Load resolves really is the temp dir. That assertion is the
// safety rail: every test below runs a real CLI verb, and one of them writes,
// so a leaked SEAMLESS_* from the developer's shell must fail the test rather
// than send it at ~/.seamless.
func throwawayInstance(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	ctx := context.Background()
	_, err = store.EnsureProject(ctx, db, "seam", "Seam")
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01SESSCLIEXPORT", Name: "cc/cli-export", ProjectSlug: "seam",
		Status: core.SessionCompleted, Findings: "the archive is the whole instance",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: "01TASKCLIEXPORT", ProjectSlug: "seam", Title: "export then import",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, db.Close())

	for rel, body := range map[string]string{
		"memory/_global/user-prefers-uv.md": "---\nid: 01MEMGLOB\nkind: convention\n---\nalways uv\n",
		"notes/seam/export-design.md":       "---\nid: 01NOTEA\n---\nexport design\n",
	} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}

	cfgPath := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("data_dir: "+dir+"\naddr: 127.0.0.1:0\n"), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	t.Setenv("SEAMLESS_DATA_DIR", dir)

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, dir, cfg.DataDir, "the test must never resolve a data dir outside its temp tree")
	return dir
}

func TestRunExport_WritesAnArchiveAndRefusesToClobberIt(t *testing.T) {
	throwawayInstance(t)
	dest := filepath.Join(t.TempDir(), "instance.tar.gz")

	require.NoError(t, runExport([]string{"-o", dest}))

	info, err := os.Stat(dest)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "an archive carries the corpus; it stays owner-only")
	}
	require.Greater(t, info.Size(), int64(0))

	_, err = os.Stat(dest + exportTmpSuffix)
	require.True(t, os.IsNotExist(err), "the temp file must be renamed away, not left behind")

	names, manifest := readArchive(t, dest)
	require.Equal(t, archive.ManifestName, names[0], "the manifest is the first entry so a reader can refuse early")
	require.Contains(t, names, archive.DBName)
	require.Contains(t, names, "memory/_global/user-prefers-uv.md")
	require.Contains(t, names, "notes/seam/export-design.md")
	require.Equal(t, archive.FormatVersion, manifest.FormatVersion)
	require.True(t, manifest.IncludesDB)
	require.Equal(t, 1, manifest.Counts.MemoryFiles)
	require.Equal(t, 1, manifest.Counts.NoteFiles)

	// Second run at the same path: refused, and the first archive survives.
	before, err := os.ReadFile(dest)
	require.NoError(t, err)
	err = runExport([]string{"-o", dest})
	require.ErrorContains(t, err, "already exists")
	after, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused export must not touch the archive already there")
}

func TestRunExport_NoDBLeavesTheDatabaseOut(t *testing.T) {
	throwawayInstance(t)
	dest := filepath.Join(t.TempDir(), "knowledge.tar.gz")

	require.NoError(t, runExport([]string{"-o", dest, "-no-db"}))

	names, manifest := readArchive(t, dest)
	require.NotContains(t, names, archive.DBName)
	require.False(t, manifest.IncludesDB)
	require.Zero(t, manifest.SchemaVersion, "a NoDB manifest describes no schema")
	require.Contains(t, exportSummary(manifest, dest, 1), "--no-db")
}

func TestRunExport_DefaultNameLandsInTheWorkingDirectory(t *testing.T) {
	throwawayInstance(t)
	out := t.TempDir()
	t.Chdir(out)

	require.NoError(t, runExport(nil))

	entries, err := filepath.Glob(filepath.Join(out, "seamless-*.tar.gz"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "an -o-less export writes exactly one default-named archive here")
	names, _ := readArchive(t, entries[0])
	require.Equal(t, archive.ManifestName, names[0])
}

// A PRESENT but empty -o is uninterpretable, so it must not silently become the
// derived default name (AGENTS.md > Required patterns).
func TestRunExport_EmptyOutputFlagIsAnError(t *testing.T) {
	throwawayInstance(t)
	err := runExport([]string{"-o", ""})
	require.ErrorContains(t, err, "-o is empty")
}

func TestDefaultExportName(t *testing.T) {
	at := time.Date(2026, 9, 23, 22, 5, 1, 0, time.UTC)
	require.Equal(t, "seamless-nuc.local-20260923T220501Z.tar.gz", defaultExportName("NUC.local", at))
	// Local time in, UTC in the name: two machines exporting at the same
	// instant must produce the same stamp.
	require.Equal(t, "seamless-a-20260923T220501Z.tar.gz",
		defaultExportName("a", at.In(time.FixedZone("UTC+3", 3*3600))))
}

func TestSanitizeHostForFilename(t *testing.T) {
	for in, want := range map[string]string{
		"argon":            "argon",
		"NUC.local":        "nuc.local",
		"host/with/slash":  "host-with-slash",
		`win\box`:          "win-box",
		"host:1":           "host-1",
		"":                 "unknown",
		"   ":              "unknown",
		"///":              "unknown",
		"eitans-macbook13": "eitans-macbook13",
	} {
		require.Equal(t, want, sanitizeHostForFilename(in), "sanitizeHostForFilename(%q)", in)
	}
}

func TestHumanSize(t *testing.T) {
	require.Equal(t, "0 B", humanSize(0))
	require.Equal(t, "512 B", humanSize(512))
	require.Equal(t, "1.0 KiB", humanSize(1024))
	require.Equal(t, "1.5 MiB", humanSize(1024*1024*3/2))
	require.Equal(t, "2.0 GiB", humanSize(2*1024*1024*1024))
}

// readArchive returns the entry names in tar order plus the decoded manifest.
func readArchive(t *testing.T, path string) ([]string, archive.Manifest) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer func() { _ = gz.Close() }()

	var names []string
	var manifest archive.Manifest
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
		if hdr.Name == archive.ManifestName {
			require.NoError(t, json.NewDecoder(tr).Decode(&manifest))
		}
	}
	require.NotEmpty(t, names)
	return names, manifest
}
