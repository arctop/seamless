package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// Mode is what an import into a given destination turns out to be. It is a
// property of the DESTINATION, not a flag: there is no --mode override, because
// the only thing an override could do is turn a merge into a restore that
// overwrites a populated instance.
type Mode string

const (
	// ModeFresh restores the archive bit-for-bit into an empty destination.
	ModeFresh Mode = "fresh"
	// ModeMerge folds the archive into an instance that already has data,
	// first-writer-wins by ULID.
	ModeMerge Mode = "merge"
)

// importTmpDB is the name a fresh restore extracts the snapshot under before
// renaming it into place. The rename is the last thing a fresh restore does, so
// an interrupted run leaves markdown and no database -- which DetectMode reads
// as fresh again, making the retry a clean repeat rather than a merge of a
// half-restored instance.
const importTmpDB = DBName + ".import-tmp"

// DetectMode reports whether an import into dataDir would be a fresh restore or
// a merge.
//
// Fresh iff the data dir is absent, or it holds neither seam.db nor seam.db-wal
// AND neither markdown tree holds a regular non-dot file. The -wal file is part
// of the test because a database can legitimately be sitting in WAL with its
// main file freshly checkpointed; treating that as "no database" would restore
// on top of a live instance. Dot-prefixed names do not count, so a .DS_Store or
// a stray .seamless-tmp-* from an interrupted write does not make a pristine
// destination look populated.
func DetectMode(dataDir string) (Mode, error) {
	if dataDir == "" {
		return "", fmt.Errorf("archive.DetectMode: empty data dir")
	}
	info, err := os.Lstat(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return ModeFresh, nil
	}
	if err != nil {
		return "", fmt.Errorf("archive.DetectMode: %s: %w", dataDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("archive.DetectMode: %s is not a directory", dataDir)
	}

	for _, name := range []string{DBName, DBName + "-wal"} {
		_, err := os.Lstat(filepath.Join(dataDir, name))
		if err == nil {
			return ModeMerge, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("archive.DetectMode: %s: %w", name, err)
		}
	}

	for _, tree := range []string{MemoryTree, NotesTree} {
		populated, err := treeHasFile(filepath.Join(dataDir, tree))
		if err != nil {
			return "", err
		}
		if populated {
			return ModeMerge, nil
		}
	}
	return ModeFresh, nil
}

// treeHasFile reports whether root holds any regular, non-dot-prefixed file.
// Symlinks do not count (they are not corpus, and following one would let a
// link to somebody else's file decide this instance's import mode) and neither
// do directories.
func treeHasFile(root string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if p != root && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		found = true
		return fs.SkipAll
	})
	if err != nil {
		return false, fmt.Errorf("archive.DetectMode: walk %s: %w", root, err)
	}
	return found, nil
}

// ImportOptions configures one import run.
type ImportOptions struct {
	// DataDir is the destination instance root. It may not exist yet.
	DataDir string

	// Src is the archive stream. It is an io.Reader, not a path, so `import
	// --from -` can read the archive from stdin; the reader is consumed once,
	// front to back, and never seeked.
	Src io.Reader

	// DryRun reports what the import would do and imports nothing. A fresh
	// destination is left untouched entirely (nothing is even extracted); a
	// merge still stages the archive in a temp dir and opens the destination
	// database, because the counts it promises are the real ones and the only
	// way to compute them is against both schemas.
	DryRun bool

	// Embedder vectorizes memories and notes as they are written during a
	// merge. Nil is supported and warned about: the merge imports no embeddings
	// row (vectors belong to whichever model this machine runs), so without an
	// embedder the imported items are lexically searchable and not semantically
	// until a re-embed pass runs.
	Embedder llm.Embedder

	// Logger receives the counts. Nil uses slog.Default.
	Logger *slog.Logger

	// OnLocks is asked what to do when the archive holds locked projects, once
	// the manifest has been read and before anything is written. An error stops
	// the import with nothing changed. Nil keeps the locks, the safe answer. It
	// is called on a dry run too, and must not prompt there.
	OnLocks func(ctx context.Context, mode Mode, m Manifest) (LockChoice, error)

	// ForceLocked are the destination config's force locks. The import writes no
	// console lock for these, since the file already holds them, and cannot
	// remove them.
	ForceLocked []string

	// KeepLocked are projects a FRESH restore keeps locked on top of the
	// archive's own locks: the ones the destination held before its data was
	// set aside for this restore, so a restore never reduces the set of locked
	// projects (owner decision, 2026-11-10). Each is registered, fenced and
	// locked as an archive lock is under LocksKeep, whatever the owner chose for
	// the archive's own. A merge ignores it: a merge never removes a lock the
	// destination holds.
	KeepLocked []LockedProject

	// TmpDir is where a merge stages the archive (its snapshot and markdown)
	// before folding it in. Empty means the OS default, which is often a small
	// RAM-backed tmpfs; a caller merging a large archive names a directory on a
	// real disk. It must already exist.
	TmpDir string
}

// PathCollision is a corpus file the merge refused to write because the path it
// belongs at is already held by a different item. Both ids are reported because
// the resolution is a human decision -- keep B's, rename A's, or delete the
// tombstone holding the name -- and it cannot be made without knowing which two
// items are involved. The importer never renames around a collision.
type PathCollision struct {
	// Path is the data-dir-relative path both items want.
	Path string
	// IncomingID is the archive's item; ExistingID is the one already there,
	// empty when the file exists on disk with no readable id.
	IncomingID string
	ExistingID string
}

// NameCollision is a row the merge could not insert because a UNIQUE column
// (sessions.name, projects.slug) is already taken by a different row. Reported,
// never resolved: minting "cc/ab12cd34-2" would invent a session name that
// nothing else in either instance refers to.
type NameCollision struct {
	Table      string
	Column     string
	Value      string
	IncomingID string
	ExistingID string
}

// ImportReport is what one import did, or (DryRun) would do.
type ImportReport struct {
	Mode    Mode
	DryRun  bool
	DataDir string

	// Manifest is the archive's own description of itself, carried through so
	// the caller can print what it was handed without re-reading the file.
	Manifest Manifest

	// Memories and Notes count corpus files written: restored verbatim for a
	// fresh import, written through the files layer for a merge. Skipped counts
	// items whose id was already present (the idempotent re-merge).
	Memories int
	Notes    int
	Skipped  int

	// Projects counts projects-table rows registered for a slug the archive's
	// corpus referenced but its projects table did not carry.
	Projects int

	// Rows is the per-table inserted count of the merge row pass, keyed by
	// table name. Empty for a fresh restore, where the whole database arrives
	// as a snapshot and the manifest's counts describe it.
	Rows map[string]int

	// Locks is what became of the archive's locked projects, nil when it held
	// none.
	Locks *LockOutcome

	PathCollisions []PathCollision
	NameCollisions []NameCollision
	Warnings       []string
}

// String renders the report for a CLI, in the shape importer.Report uses.
func (r ImportReport) String() string {
	var b strings.Builder
	mode := string(r.Mode)
	if r.DryRun {
		mode += ", dry run"
	}
	fmt.Fprintf(&b, "import (%s) into %s: %d memories, %d notes, %d projects (%d skipped, already present)",
		mode, r.DataDir, r.Memories, r.Notes, r.Projects, r.Skipped)

	if len(r.Rows) > 0 {
		parts := make([]string, 0, len(r.Rows))
		for _, table := range mergeTables {
			if n, ok := r.Rows[table]; ok {
				parts = append(parts, fmt.Sprintf("%s %d", table, n))
			}
		}
		fmt.Fprintf(&b, "\nrows inserted: %s", strings.Join(parts, ", "))
	}
	if l := r.Locks; l != nil {
		if len(l.Locked) > 0 {
			fmt.Fprintf(&b, "\nlocked: %s", strings.Join(l.Locked, ", "))
		}
		if len(l.Removed) > 0 {
			fmt.Fprintf(&b, "\nlocks removed: %s", strings.Join(l.Removed, ", "))
		}
		if len(l.StillLocked) > 0 {
			fmt.Fprintf(&b, "\nstill locked by this instance's own lock: %s", strings.Join(l.StillLocked, ", "))
		}
	}
	for _, c := range r.PathCollisions {
		fmt.Fprintf(&b, "\npath collision: %s wanted by %s, held by %s (not written)",
			c.Path, c.IncomingID, existingOrUnknown(c.ExistingID))
	}
	for _, c := range r.NameCollisions {
		fmt.Fprintf(&b, "\n%s.%s collision: %q wanted by %s, held by %s (not inserted)",
			c.Table, c.Column, c.Value, c.IncomingID, existingOrUnknown(c.ExistingID))
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, "\nwarnings (%d):", len(r.Warnings))
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "\n  - %s", w)
		}
	}
	return b.String()
}

func existingOrUnknown(id string) string {
	if id == "" {
		return "an unreadable file"
	}
	return id
}

// Import restores or merges the archive in opts.Src into opts.DataDir.
//
// Which of the two it is comes from DetectMode, is recorded in the report, and
// is never overridden: restoring over a populated instance would replace its
// database, and merging into an empty one would rewrite every markdown file
// through the YAML marshaller instead of restoring it byte-for-byte.
//
// The report is returned even on failure, so a caller can print what happened
// before the error that stopped it.
func Import(ctx context.Context, opts ImportOptions) (*ImportReport, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.DataDir == "" {
		return nil, fmt.Errorf("archive.Import: empty data dir")
	}
	if opts.Src == nil {
		return nil, fmt.Errorf("archive.Import: nil source reader")
	}

	mode, err := DetectMode(opts.DataDir)
	if err != nil {
		return nil, err
	}
	rep := &ImportReport{Mode: mode, DryRun: opts.DryRun, DataDir: opts.DataDir}

	a, err := openArchive(opts.Src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = a.Close() }()
	rep.Manifest = a.manifest

	canonicalLocks(a.manifest, rep)
	if a.manifest.LocksError != "" {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"the export could not read its locks (%q): a project it held locked from the console arrives unlocked", a.manifest.LocksError))
	}

	// Asked here: the manifest has been read and nothing has been written. A
	// refusal returns no report, because there is nothing to report.
	choice := LocksKeep
	if opts.OnLocks != nil && a.manifest.Locks != nil && len(a.manifest.Locks.Projects) > 0 {
		if choice, err = opts.OnLocks(ctx, mode, a.manifest); err != nil {
			return nil, err
		}
	}

	switch mode {
	case ModeFresh:
		err = importFresh(ctx, a, opts, choice, logger, rep)
	case ModeMerge:
		err = importMerge(ctx, a, opts, choice, logger, rep)
	default:
		// Unreachable: DetectMode returns one of the two or an error. Named
		// rather than defaulted, so a third mode added later fails loudly.
		return rep, fmt.Errorf("archive.Import: unknown mode %q", mode)
	}

	if err != nil {
		// Either log or return, never both: the caller prints the report and
		// the error it is returned with.
		return rep, err
	}
	// Counts and ids only. A corpus body never reaches a log line, here or
	// anywhere else in this package.
	logger.Info("archive: import finished",
		"mode", string(mode), "dry_run", opts.DryRun,
		"memories", rep.Memories, "notes", rep.Notes, "skipped", rep.Skipped,
		"projects", rep.Projects, "rows", rep.Rows,
		"path_collisions", len(rep.PathCollisions),
		"name_collisions", len(rep.NameCollisions),
		"warnings", len(rep.Warnings))
	return rep, nil
}

// importFresh restores the archive verbatim into an empty destination.
//
// Order: markdown first, then the database under a temporary name (which is
// where the archive puts it, so this is really "rename it last"), then the
// rename. Reconcile afterwards heals the one window the exporter leaves open --
// it snapshots the database before walking the trees, so a file written during
// the export is in the archive with no index row behind it.
func importFresh(ctx context.Context, a *archiveReader, opts ImportOptions, choice LockChoice, logger *slog.Logger, rep *ImportReport) error {
	if opts.DryRun {
		// Nothing is extracted at all: the manifest already carries every count
		// a fresh restore would produce, and a dry run that wrote the trees and
		// then deleted them would be indistinguishable from a failed restore if
		// it were interrupted.
		rep.Memories = a.manifest.Counts.MemoryFiles
		rep.Notes = a.manifest.Counts.NoteFiles
		rep.Warnings = append(rep.Warnings, embeddingWarnings(a.manifest, opts.Embedder, ModeFresh)...)
		return applyFreshLocks(ctx, nil, opts, manifestLocks(a.manifest), choice, rep)
	}

	ex, err := a.extractTo(ctx, opts.DataDir, importTmpDB)
	if err != nil {
		return err
	}
	rep.Memories = ex.MemoryFiles
	rep.Notes = ex.NoteFiles

	dbPath := filepath.Join(opts.DataDir, DBName)
	if ex.dbPath != "" {
		if err := os.Rename(ex.dbPath, dbPath); err != nil {
			ex.rollback()
			return fmt.Errorf("archive.Import: install database: %w", err)
		}
	}

	// store.Open, not OpenExisting: a snapshot from an older seamlessd has to
	// migrate forward before anything reads it, and a NoDB archive has no
	// database to open at all, so this is also where one gets created.
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("archive.Import: open restored database: %w", err)
	}
	defer func() { _ = db.Close() }()
	// A restored database brings back the leases, console unlocks, read
	// grants and console sign-ins it held when the snapshot was taken,
	// including ones revoked or signed out since: every live one ends here,
	// and a locked project's agents ask again, and the owner signs in again.
	if _, err := store.ResetRestoredAccess(ctx, db, time.Now()); err != nil {
		return fmt.Errorf("archive.Import: %w", err)
	}
	// Credentials are not a live permission ResetRestoredAccess can end: they
	// are who may approve, and this restore is often how a machine gets them
	// back. But one removed since the backup (a lost key, say) is back too.
	if err := warnRestoredApprovers(ctx, db, rep); err != nil {
		return err
	}
	// A paired device is a live credential the snapshot holds as it stood,
	// one the owner revoked since included: every one ends here, and each
	// computer pairs again. A backup restored through the backup service
	// gets today's devices back on top (store.ApplyKeptState).
	if err := revokeRestoredDevices(ctx, db, rep); err != nil {
		return err
	}
	// The whole database is the archive's, so its console locks are the
	// archive's. The manifest adds what the snapshot could not hold: the
	// exporting config's force locks, or every lock of a --no-db archive.
	locks, err := withSnapshotLocks(ctx, db, manifestLocks(a.manifest))
	if err != nil {
		return err
	}
	if err := applyFreshLocks(ctx, db, opts, locks, choice, rep); err != nil {
		return err
	}

	mgr, err := files.NewManager(opts.DataDir, db, logger)
	if err != nil {
		return fmt.Errorf("archive.Import: %w", err)
	}
	defer func() { _ = mgr.Close() }()
	if opts.Embedder != nil {
		mgr.SetEmbedder(opts.Embedder)
	}
	// Reconcile, not Start: a restore must not leave a watcher running against
	// a data dir this process is about to stop caring about.
	if err := mgr.Reconcile(ctx); err != nil {
		return fmt.Errorf("archive.Import: reconcile: %w", err)
	}

	rep.Warnings = append(rep.Warnings, embeddingWarnings(a.manifest, opts.Embedder, ModeFresh)...)
	return nil
}

// revokeRestoredDevices revokes every device the restored database brought
// back and voids its unused pairing codes, and says so in the report.
func revokeRestoredDevices(ctx context.Context, db *sql.DB, rep *ImportReport) error {
	n, err := store.RevokeAllDevices(ctx, db, store.DeviceByRestore, time.Now())
	if err != nil {
		return fmt.Errorf("archive.Import: %w", err)
	}
	if n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"revoked the %d paired device(s) this backup held: pair each computer again from the console's Devices page", n))
	}
	return nil
}

// embeddingWarnings reports what the caller has to do about vectors.
//
// Vectors from two different embedding models are not comparable -- cosine
// between them is noise, not similarity -- so a mismatch is not a degradation
// that heals itself. It is reported rather than acted on because re-embedding
// is a metered provider round trip per item, which is the owner's decision and
// the console's job.
func embeddingWarnings(m Manifest, embedder llm.Embedder, mode Mode) []string {
	var out []string
	local := ""
	if embedder != nil {
		local = embedder.Model()
	}

	switch mode {
	case ModeFresh:
		if len(m.EmbeddingModels) == 0 || local == "" {
			return nil
		}
		if !slices.Contains(m.EmbeddingModels, local) {
			out = append(out, fmt.Sprintf(
				"restored embeddings were made by %s but this instance embeds with %s; re-embed from the console (Settings -> Embeddings) or semantic recall will miss the restored corpus",
				strings.Join(m.EmbeddingModels, ", "), local))
		}
	case ModeMerge:
		// A merge never imports the embeddings table: those vectors belong to
		// the source machine's model. Imported items get vectors only if this
		// instance has an embedder to make them with.
		if local == "" {
			out = append(out, "no embedder configured: imported memories and notes are lexically searchable but have no vectors; configure an embedder and re-embed from the console")
		}
	}
	return out
}
