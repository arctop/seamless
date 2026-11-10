package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Registers the "sqlite" driver for the read-only verify handle below.
	// store imports it too, but relying on a transitive import to register a
	// driver is how a later refactor turns this into a runtime panic.
	_ "modernc.org/sqlite"

	"github.com/arctop/seamless/internal/store"
)

// ExportOptions configures one export. Only DataDir and Out are required.
type ExportOptions struct {
	// DataDir is the instance root: the directory holding seam.db, memory/ and
	// notes/. It is named explicitly rather than read from config so an export
	// can target a data dir this process does not serve.
	DataDir string

	// Out receives the gzipped tar. It is an io.Writer, not a path, because the
	// CLI writes to a temp file it renames into place and to stdout for `-o -`;
	// choosing the destination is the caller's job.
	Out io.Writer

	// NoDB exports the markdown trees only. The manifest then carries no
	// schema version, no table counts, and no embedding models: those describe
	// a database this archive does not contain, and stamping them from the live
	// file would describe bytes that are not here. seam.db is opened read-only
	// and only to record the locks, so a knowledge-only export still works on a
	// data dir with no database at all.
	NoDB bool

	// SeamlessdVersion is recorded verbatim in the manifest. It is passed in
	// because the build version is linked into package main and this package
	// sits below cmd/.
	SeamlessdVersion string

	// Host overrides the recorded source host. Empty means os.Hostname.
	Host string

	// ForceLocked are the exporting config's force locks, and PasskeyRPID the
	// relying party its passkeys belong to. Both are passed in because this
	// package never reads config. The update's own pre-update backup leaves them
	// empty: it is put back on the machine whose config still holds them.
	ForceLocked []string
	PasskeyRPID string

	// TmpDir is where the export stages: the database snapshot is written into
	// a fresh seamless-export-* directory under it, streamed into the archive,
	// and removed before Export returns, so it needs room for one full copy of
	// seam.db. Empty means the OS default (os.TempDir), which is often a small
	// RAM-backed tmpfs; a caller snapshotting a large database names a directory
	// on a real disk instead, such as one beside the data dir. It must already
	// exist: a missing TmpDir is an error, never a quiet fallback to the very
	// default the caller chose to avoid.
	TmpDir string
}

// Export writes a complete archive of the instance at opts.DataDir to opts.Out
// and returns the manifest it recorded.
//
// Ordering is deliberate. The database snapshot is taken FIRST and the markdown
// trees are walked second, so the file set is a superset of what the snapshot's
// indexes describe: a memory written during the export is present as a file
// with no index row, which the import's Reconcile heals. The other order loses
// the file.
//
// The snapshot is `VACUUM INTO` on a store.OpenExisting handle. OpenExisting
// (not Open) because a newer binary must never migrate a live database as a
// side effect of backing it up, and VACUUM INTO (not a file copy) because WAL
// means the bytes on disk are not a consistent database on their own: the
// snapshot is taken inside a read transaction, so a write in flight from the
// running daemon is simply not in it.
func Export(ctx context.Context, opts ExportOptions) (Manifest, error) {
	if opts.DataDir == "" {
		return Manifest{}, fmt.Errorf("archive.Export: empty data dir")
	}
	if opts.Out == nil {
		return Manifest{}, fmt.Errorf("archive.Export: nil output writer")
	}

	host := opts.Host
	if host == "" {
		h, err := os.Hostname()
		if err != nil {
			return Manifest{}, fmt.Errorf("archive.Export: hostname: %w", err)
		}
		host = h
	}

	tmpDir, err := os.MkdirTemp(opts.TmpDir, "seamless-export-")
	if err != nil {
		return Manifest{}, fmt.Errorf("archive.Export: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	manifest := Manifest{
		FormatVersion:    FormatVersion,
		SeamlessdVersion: opts.SeamlessdVersion,
		CreatedAt:        time.Now().UTC(),
		SourceHost:       host,
		IncludesDB:       !opts.NoDB,
	}

	snapPath := ""
	if !opts.NoDB {
		snapPath = filepath.Join(tmpDir, DBName)
		if err := snapshotDB(ctx, filepath.Join(opts.DataDir, DBName), snapPath); err != nil {
			return Manifest{}, err
		}
		schemaVersion, tables, models, err := inspectSnapshot(ctx, snapPath)
		if err != nil {
			return Manifest{}, err
		}
		manifest.SchemaVersion = schemaVersion
		manifest.Counts.Tables = tables
		manifest.EmbeddingModels = models
	}

	// Read from the snapshot when there is one, so the record describes the
	// bytes in this archive. A --no-db export has none, and still has to say
	// what was locked: its files are the locked projects' content.
	locksDB := snapPath
	if opts.NoDB {
		locksDB = filepath.Join(opts.DataDir, DBName)
	}
	locks, err := exportLocks(ctx, locksDB, opts)
	if err != nil {
		if !opts.NoDB {
			return Manifest{}, err
		}
		manifest.LocksError = err.Error()
		if locks, err = exportLocks(ctx, "", opts); err != nil {
			return Manifest{}, err
		}
	}
	manifest.Locks = locks

	memFiles, err := collectTree(ctx, opts.DataDir, MemoryTree)
	if err != nil {
		return Manifest{}, err
	}
	noteFiles, err := collectTree(ctx, opts.DataDir, NotesTree)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Counts.MemoryFiles = len(memFiles)
	manifest.Counts.NoteFiles = len(noteFiles)

	if err := writeArchive(opts.Out, manifest, snapPath, append(memFiles, noteFiles...)); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// snapshotDB writes a consistent copy of dbPath to snapPath, which must not
// exist.
//
// The bound-parameter form is the one that works on modernc/sqlite (pinned by
// TestVacuumIntoAcceptsABoundParameter): SQLite parses the INTO target as an
// expression, so `?` is a legal target and the path never goes through string
// quoting. The database itself is opened read-only in intent -- OpenExisting
// runs no migration -- so the only write this makes is the snapshot file.
func snapshotDB(ctx context.Context, dbPath, snapPath string) error {
	src, err := store.OpenExisting(dbPath)
	if err != nil {
		return fmt.Errorf("archive.Export: open source db: %w", err)
	}
	defer func() { _ = src.Close() }()

	if _, err := src.ExecContext(ctx, `VACUUM INTO ?`, snapPath); err != nil {
		return fmt.Errorf("archive.Export: snapshot %s: %w", dbPath, err)
	}
	return nil
}

// inspectSnapshot verifies the snapshot and reads what the manifest reports
// about it. Reading from the snapshot rather than the live database is the
// point: the manifest has to describe the bytes in the archive, and the live
// database has moved on by the time the tar is written.
func inspectSnapshot(ctx context.Context, snapPath string) (schemaVersion int, tables map[string]int, models []string, err error) {
	// mode=ro, and no journal_mode pragma: a read-only handle must not create
	// -wal/-shm files next to the snapshot we are about to tar.
	dsn := "file:" + url.PathEscape(snapPath) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, openErr := sql.Open("sqlite", dsn)
	if openErr != nil {
		return 0, nil, nil, fmt.Errorf("archive.Export: verify snapshot: %w", openErr)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	if err := quickCheck(ctx, db); err != nil {
		return 0, nil, nil, err
	}

	schemaVersion, err = store.SchemaVersion(db)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("archive.Export: verify snapshot: %w", err)
	}

	tables, err = tableCounts(ctx, db)
	if err != nil {
		return 0, nil, nil, err
	}

	models, err = embeddingModels(ctx, db)
	if err != nil {
		return 0, nil, nil, err
	}
	return schemaVersion, tables, models, nil
}

// quickCheck runs PRAGMA quick_check on the snapshot. It reports every row: on
// success SQLite returns the single row "ok", and on corruption it returns one
// row per problem, so reading only the first would hide the rest of the report
// from the error message.
func quickCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("archive.Export: quick_check: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("archive.Export: quick_check: %w", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("archive.Export: quick_check: %w", err)
	}
	if len(out) != 1 || out[0] != "ok" {
		return fmt.Errorf("archive.Export: snapshot failed quick_check: %s", strings.Join(out, "; "))
	}
	return nil
}

// tableCounts returns row counts keyed by table name, enumerated from
// sqlite_master rather than from a list in this file: a table added by a later
// migration has to appear in the manifest without anyone remembering to update
// the exporter. The name filter is the one TableCount uses -- SQLite internals
// and the fts5 shadow tables are not user data.
func tableCounts(ctx context.Context, db *sql.DB) (map[string]int, error) {
	// The names are drained in their own scope before any COUNT runs: the
	// handle is capped at one connection, so counting while the name cursor is
	// still open would deadlock rather than merely be untidy.
	names, err := tableNames(ctx, db)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(names))
	for _, name := range names {
		var n int
		// The identifier comes from sqlite_master, not from a caller, and is
		// quoted anyway so a table named like a keyword still counts.
		q := `SELECT COUNT(*) FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return nil, fmt.Errorf("archive.Export: count %s: %w", name, err)
		}
		counts[name] = n
	}
	return counts, nil
}

// tableNames lists the user tables in the snapshot, newest migration included,
// in a scope that closes its cursor before the caller queries again.
func tableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'fts_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("archive.Export: list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("archive.Export: list tables: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Export: list tables: %w", err)
	}
	return names, nil
}

// embeddingModels returns the distinct embedding models present in the
// snapshot, sorted. An empty embeddings table yields nil, not an empty slice,
// so the manifest simply omits the field.
func embeddingModels(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT model FROM embeddings ORDER BY model`)
	if err != nil {
		return nil, fmt.Errorf("archive.Export: embedding models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("archive.Export: embedding models: %w", err)
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Export: embedding models: %w", err)
	}
	return models, nil
}

// treeFile is one markdown file destined for the archive, read at walk time.
type treeFile struct {
	name    string // slash-separated, relative to the data dir
	data    []byte
	modTime time.Time
}

// collectTree walks one markdown tree and returns its files in lexical order.
//
// The walk mirrors files.Manager.Reconcile: Lstat semantics (WalkDir never
// follows a symlink, and DirEntry.Info describes the link itself), skip
// symlinks, skip anything that is not a regular file, take only .md. It adds
// one rule Reconcile does not need: dot-prefixed names are skipped outright,
// which drops .DS_Store, a stray .git, and the files layer's own
// .seamless-tmp-* write staging.
//
// Only these two roots are walked, which is why the stray backups/ directory
// and seamlessd.log that live beside them in a real data dir can never be
// picked up: they are not reachable from here, rather than being filtered by
// name somewhere.
//
// Contents are read during the walk instead of at tar-writing time because the
// manifest -- which carries the counts -- is the FIRST tar entry. Reading later
// would let a file deleted in between turn the manifest into a description of
// an archive that was never written. Corpus files are markdown, so holding them
// is cheap relative to the snapshot already on disk.
func collectTree(ctx context.Context, dataDir, tree string) ([]treeFile, error) {
	root := filepath.Join(dataDir, tree)

	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("archive.Export: lstat %s: %w", root, err)
	}
	// A symlinked or non-directory tree root is refused rather than skipped: an
	// export that silently contained no memories would look like a successful
	// backup of an empty instance.
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("archive.Export: %w: %s is a symlink", ErrUnsafeEntry, root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("archive.Export: %w: %s is not a directory", ErrUnsafeEntry, root)
	}

	var out []treeFile
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path != root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil // WalkDir does not follow it; a link is not a corpus file.
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from walking the caller's own data dir.
		if err != nil {
			return err
		}
		out = append(out, treeFile{
			name:    filepath.ToSlash(rel),
			data:    data,
			modTime: fi.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("archive.Export: walk %s: %w", root, err)
	}
	return out, nil
}

// writeArchive streams the gzipped tar: manifest first, then the snapshot, then
// the markdown files in walk order.
func writeArchive(out io.Writer, manifest Manifest, snapPath string, files []treeFile) (retErr error) {
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("archive.Export: marshal manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	// Close in order and report the first failure: a swallowed flush error
	// yields a truncated archive that looks like a successful export.
	defer func() {
		if err := tw.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("archive.Export: close tar: %w", err)
		}
		if err := gz.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("archive.Export: close gzip: %w", err)
		}
	}()

	if err := writeEntry(tw, ManifestName, manifest.CreatedAt, int64(len(manifestJSON)), func(w io.Writer) error {
		_, err := w.Write(manifestJSON)
		return err
	}); err != nil {
		return err
	}

	if snapPath != "" {
		info, err := os.Stat(snapPath)
		if err != nil {
			return fmt.Errorf("archive.Export: stat snapshot: %w", err)
		}
		if err := writeEntry(tw, DBName, manifest.CreatedAt, info.Size(), func(w io.Writer) error {
			f, err := os.Open(snapPath) //nolint:gosec // our own temp snapshot.
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(w, f)
			return err
		}); err != nil {
			return err
		}
	}

	for _, f := range files {
		if err := writeEntry(tw, f.name, f.modTime, int64(len(f.data)), func(w io.Writer) error {
			_, err := w.Write(f.data)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeEntry writes one regular-file tar entry.
//
// Mode 0600 with uid/gid and uname/gname zeroed on every entry: the archive
// carries data, not the exporting machine's ownership, and an archive restored
// under a different account must not try to reinstate a uid that means
// something else there. There are no directory entries at all -- the extractor
// creates parents itself with 0700 -- so the archive has no entry whose only
// job is to carry a permission.
func writeEntry(tw *tar.Writer, name string, modTime time.Time, size int64, body func(io.Writer) error) error {
	hdr := &tar.Header{
		Format:   tar.FormatPAX,
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     size,
		Mode:     0o600,
		Uid:      0,
		Gid:      0,
		Uname:    "",
		Gname:    "",
		ModTime:  modTime.UTC().Truncate(time.Second),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("archive.Export: header %s: %w", name, err)
	}
	if err := body(tw); err != nil {
		return fmt.Errorf("archive.Export: write %s: %w", name, err)
	}
	return nil
}
