package main

// The updater's backup (plan step 5) and the one restore it ever makes on its
// own (step 8): a pre-update archive of the instance, taken with archive.Export
// over a store.OpenExisting handle (no migration), and -- only when the new
// release migrated the database past what the rollback target knows -- that
// archive's database put back in place. Markdown files are never restored
// automatically: they are the source of truth, the rollback target's startup
// reconcile re-indexes them, and recovering them from the backup is the
// owner's call: `seamlessd import --from <backup>` restores only into an empty
// data dir (into a populated one it merges), so the old one goes aside first.
//
// That restore never reduces the set of locked projects (owner decision,
// 2026-11-10). The console's locks live only in the database, so one the owner
// turned on while the new release ran is in the migrated database and not in
// the backup's: the rollback reads the migrated database's console locks
// before anything moves, and restoreDatabase puts the backup's database in
// place only once its snapshot is shown to hold every one of them.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

const (
	// backupDirName is <data_dir>/backups, 0700, beside memory/ and notes/;
	// archive.Export walks only those two trees, so a backup never ends up
	// inside the next one.
	backupDirName = "backups"
	// backupKeep is how many pre-update backups stay: the newest two.
	backupKeep = 2
	// backupPrefix and backupSuffix frame a pre-update backup's name,
	// pre-update-v<from>-<utc>.tar.gz; backupStamp is the <utc> part.
	backupPrefix = "pre-update-v"
	backupSuffix = ".tar.gz"
	backupStamp  = "20060102T150405Z"
	// backupSpare is free space the backup leaves on top of its own need.
	backupSpare = 64 << 20
	// sqliteMagic opens every SQLite database file.
	sqliteMagic = "SQLite format 3\x00"
)

// backupDir is where pre-update backups go.
func backupDir(dataDir string) string { return filepath.Join(dataDir, backupDirName) }

// backupName is the file a backup taken at at, before an update from from,
// is written to.
func backupName(from update.Version, at time.Time) string {
	return backupPrefix + from.String() + "-" + at.UTC().Format(backupStamp) + backupSuffix
}

// backupTime parses a name backupName wrote, ok false for any other file.
func backupTime(name string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(name, backupPrefix)
	if !ok {
		return time.Time{}, false
	}
	rest, ok = strings.CutSuffix(rest, backupSuffix)
	if !ok {
		return time.Time{}, false
	}
	i := strings.LastIndexByte(rest, '-')
	if i < 0 {
		return time.Time{}, false
	}
	if _, ok := update.Parse(rest[:i]); !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(backupStamp, rest[i+1:])
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// pruneBackups deletes every pre-update backup in dir but the newest keep,
// by the time in their names. Only names backupName writes are ever touched.
func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type found struct {
		name string
		at   time.Time
	}
	var backups []found
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if at, ok := backupTime(e.Name()); ok {
			backups = append(backups, found{e.Name(), at})
		}
	}
	slices.SortFunc(backups, func(a, b found) int { return b.at.Compare(a.at) })
	var errs []error
	for _, b := range backups[min(keep, len(backups)):] {
		if err := os.Remove(filepath.Join(dir, b.name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fileSHA256 is the lowercase hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// dbSchemaVersion reads the schema version of the database at dbPath through
// a store.OpenExisting handle -- never store.Open, which would migrate it --
// and closes the handle before it returns.
func dbSchemaVersion(dbPath string) (int, error) {
	db, err := store.OpenExisting(dbPath)
	if err != nil {
		return 0, err
	}
	v, err := store.SchemaVersion(db)
	if cerr := db.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return v, err
}

// backupNeed estimates the free space a backup takes: the database and its
// WAL twice (the VACUUM INTO snapshot Export stages, then the archive holding
// it), the markdown trees once, and backupSpare.
func backupNeed(dataDir string) (uint64, error) {
	var need uint64 = backupSpare
	for _, name := range []string{archive.DBName, archive.DBName + "-wal"} {
		fi, err := os.Lstat(filepath.Join(dataDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		need += 2 * uint64(fi.Size())
	}
	for _, tree := range []string{archive.MemoryTree, archive.NotesTree} {
		err := filepath.WalkDir(filepath.Join(dataDir, tree), func(_ string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			need += uint64(fi.Size())
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return need, nil
}

// backupRequest is one pre-update backup to take.
type backupRequest struct {
	dataDir   string
	from      update.Version // the release being replaced, named in the file
	attemptID string         // names the staging directory
	at        time.Time
	exe       string // the installed seamlessd, hashed for H0
	version   string // this build, recorded in the archive's manifest
	freeBytes func(dir string) (uint64, error)
}

// backupResult is what a backup recorded.
type backupResult struct {
	// path is the archive, absolute.
	path string
	// schema is S0: the schema version of the database in the archive, read
	// from its manifest, so it describes exactly what a restore would put back.
	schema int
	// binSHA is H0: the SHA-256 of the installed seamlessd before the
	// installer ran. A different hash afterwards means the binary was swapped.
	binSHA string
}

// errNoSpace is a backup the disk has no room for.
var errNoSpace = errors.New("not enough free space for the backup")

// takeBackup records H0, checks for room, and writes the instance with
// archive.Export to <data_dir>/backups/pre-update-v<from>-<utc>.tar.gz (0600
// in a 0700 directory), through a temp file renamed into place, keeping the
// newest backupKeep. Export stages its database snapshot in a directory the
// backup creates beside the archives and removes, so the snapshot lands on
// the data dir's disk rather than in a small RAM-backed temp. Every database
// handle is closed by the time it returns.
func takeBackup(ctx context.Context, r backupRequest) (backupResult, error) {
	binSHA, err := fileSHA256(r.exe)
	if err != nil {
		return backupResult{}, fmt.Errorf("hash the installed seamlessd: %w", err)
	}
	dir := backupDir(r.dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return backupResult{}, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return backupResult{}, err
	}
	need, err := backupNeed(r.dataDir)
	if err != nil {
		return backupResult{}, fmt.Errorf("size the data dir: %w", err)
	}
	free, err := r.freeBytes(dir)
	if err != nil {
		return backupResult{}, fmt.Errorf("read free space on %s: %w", dir, err)
	}
	if free < need {
		return backupResult{}, fmt.Errorf("%w: %d MiB free on %s, %d MiB needed", errNoSpace, free>>20, dir, need>>20)
	}

	staging := filepath.Join(dir, ".export-"+r.attemptID)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return backupResult{}, err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	tmp, err := os.CreateTemp(dir, ".pre-update-*.partial")
	if err != nil {
		return backupResult{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close()
		return backupResult{}, err
	}
	manifest, err := archive.Export(ctx, archive.ExportOptions{
		DataDir: r.dataDir, Out: tmp, SeamlessdVersion: r.version, TmpDir: staging, Kind: archive.KindPreUpdate,
	})
	if err != nil {
		_ = tmp.Close()
		return backupResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return backupResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return backupResult{}, err
	}
	path := filepath.Join(dir, backupName(r.from, r.at))
	if err := os.Rename(tmp.Name(), path); err != nil {
		return backupResult{}, err
	}
	keep = true
	res := backupResult{path: path, schema: manifest.SchemaVersion, binSHA: binSHA}
	if err := pruneBackups(dir, backupKeep); err != nil {
		// The new backup is in place; an old one that would not go is only
		// space, and the next backup prunes again.
		return res, fmt.Errorf("%w: %w", errBackupPrune, err)
	}
	return res, nil
}

// errBackupPrune is a backup that succeeded but could not remove an older
// one: takeBackup returns the result with it, and the caller only logs it.
var errBackupPrune = errors.New("prune old backups")

// restoreDatabase puts the database inside the backup archive at backupPath
// back as dataDir's seam.db: the rollback's step when the release it undoes
// migrated the database past schema wantSchema, which the rollback target was
// built for. The archive's manifest must describe that schema and carry a
// database; the snapshot is streamed to a temp file first, and only then are
// the live seam.db, seam.db-wal and seam.db-shm renamed aside -- never
// deleted -- to <name>.pre-rollback-<attemptID>, all three, because a WAL left
// beside a different database would be replayed into it. A failure after the
// renames puts them back. The caller has stopped the daemon: nothing may hold
// the database while its files move. The suffix is returned for the record.
//
// vet, when not nil, reads the extracted snapshot at the path it is given
// before anything is renamed, and an error from it is returned with nothing
// moved: what the snapshot lacks is judged while the live database is still
// in place, so no moment leaves a database that fails the vet as seam.db --
// not even for an updater killed mid-restore.
func restoreDatabase(dataDir, backupPath, attemptID string, wantSchema int, vet func(snapshot string) error) (string, error) {
	tmp := filepath.Join(dataDir, archive.DBName+".restore-"+attemptID)
	if err := extractSnapshotDB(backupPath, tmp, wantSchema); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if vet != nil {
		err := vet(tmp)
		// SQLite removes the WAL and shared-memory files its last connection
		// to the snapshot leaves; should one stay, it holds nothing (the vet
		// only reads) and pairs with no database once the snapshot is renamed.
		for _, side := range []string{tmp + "-wal", tmp + "-shm"} {
			_ = os.Remove(side) //nolint:errcheck // best effort: usually absent, and inert if it stays
		}
		if err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
	}
	suffix := ".pre-rollback-" + attemptID
	var moved []string
	undo := func() {
		for _, p := range moved {
			_ = os.Rename(p+suffix, p) //nolint:errcheck // best effort: the failure being returned names the files
		}
		_ = os.Remove(tmp)
	}
	for _, name := range []string{archive.DBName, archive.DBName + "-wal", archive.DBName + "-shm"} {
		p := filepath.Join(dataDir, name)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Rename(p, p+suffix); err != nil {
			undo()
			return "", fmt.Errorf("move %s aside: %w", p, err)
		}
		moved = append(moved, p)
	}
	if err := os.Rename(tmp, filepath.Join(dataDir, archive.DBName)); err != nil {
		undo()
		return "", fmt.Errorf("put the restored database in place: %w", err)
	}
	return suffix, nil
}

// resetRestoredAccess ends every live access lease, console unlock and read
// grant in the database restoreDatabase put back at dataDir
// (store.ResetRestoredAccess). It opens the database without migrating it: the
// rollback target is the release the backup was taken by, and it migrates its
// own database when it starts.
func resetRestoredAccess(ctx context.Context, dataDir string, now time.Time) (store.RestoredAccess, error) {
	db, err := store.OpenExisting(filepath.Join(dataDir, archive.DBName))
	if err != nil {
		return store.RestoredAccess{}, err
	}
	got, err := store.ResetRestoredAccess(ctx, db, now)
	if cerr := db.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return got, err
}

// unrestoreDatabase undoes a restoreDatabase that returned suffix: the
// restored seam.db and the WAL and shared-memory files opening it left are
// renamed aside to <name>.unrestored-<attemptID> -- never deleted -- and the
// migrated files restoreDatabase moved aside are put back.
func unrestoreDatabase(dataDir, suffix, attemptID string) error {
	names := []string{archive.DBName, archive.DBName + "-wal", archive.DBName + "-shm"}
	for _, name := range names {
		p := filepath.Join(dataDir, name)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Rename(p, p+".unrestored-"+attemptID); err != nil {
			return fmt.Errorf("move the restored %s aside: %w", name, err)
		}
	}
	for _, name := range names {
		p := filepath.Join(dataDir, name)
		if _, err := os.Lstat(p + suffix); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Rename(p+suffix, p); err != nil {
			return fmt.Errorf("put the migrated %s back: %w", name, err)
		}
	}
	return nil
}

// restoredAccessWords names what ResetRestoredAccess ended, for the rollback's
// record: "1 access lease and 2 read grants".
func restoredAccessWords(r store.RestoredAccess) string {
	var parts []string
	for _, c := range []struct {
		n    int64
		noun string
	}{
		{r.Leases, "access lease"},
		{r.Unlocks, "console unlock"},
		{r.Grants, "read grant"},
		{r.SignIns, "console sign-in"},
		{r.Enrollments, "passkey waiting for approval"},
	} {
		if c.n > 0 {
			parts = append(parts, update.Plural(c.n, c.noun))
		}
	}
	if len(parts) == 0 {
		return "no access"
	}
	return andList(parts)
}

// andList joins words for a record: "a", "a and b", "a, b and c".
func andList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// dbConsoleLocks returns the projects the console locks in the database at
// dbPath, sorted, through a store.OpenExisting handle -- never store.Open,
// which would migrate it -- closed before it returns. A database without the
// console_locks table (one that predates migration 037) has no console locks.
// The database's WAL is part of it, so the rollback reads a database before
// its files move: a -wal renamed aside no longer pairs with its main file.
func dbConsoleLocks(ctx context.Context, dbPath string) ([]string, error) {
	db, err := store.OpenExisting(dbPath)
	if err != nil {
		return nil, err
	}
	var slugs []string
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'console_locks'`).Scan(&n)
	if err != nil {
		err = fmt.Errorf("look for the console_locks table: %w", err)
	} else if n > 0 {
		slugs, err = store.ConsoleLockedSlugs(ctx, db)
	}
	if cerr := db.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return slugs, err
}

// missingLocksError is a backup whose database does not hold every console
// lock the migrated one does, naming the projects it lacks.
type missingLocksError struct{ projects []string }

func (e *missingLocksError) Error() string {
	if len(e.projects) == 1 {
		return "the backup's database does not hold the console lock on " + e.projects[0]
	}
	return "the backup's database does not hold the console locks on " + andList(e.projects)
}

// keepConsoleLocks is restoreDatabase's vet for a rollback whose migrated
// database holds the console locks in locked: the snapshot must hold every
// one, or the restore would open a project the owner locked after the backup
// (a *missingLocksError). A snapshot whose locks cannot be read fails too: it
// cannot be shown to keep them. The snapshot's locks locked lacks -- ones the
// owner removed after the backup, which the restore brings back, failing
// closed -- go to *returned for the record.
func keepConsoleLocks(ctx context.Context, locked []string, returned *[]string) func(snapshot string) error {
	return func(snapshot string) error {
		got, err := dbConsoleLocks(ctx, snapshot)
		if err != nil {
			return fmt.Errorf("read the backup's console locks: %w", err)
		}
		var missing []string
		for _, slug := range locked {
			if !slices.Contains(got, slug) {
				missing = append(missing, slug)
			}
		}
		if len(missing) > 0 {
			return &missingLocksError{projects: missing}
		}
		*returned = nil
		for _, slug := range got {
			if !slices.Contains(locked, slug) {
				*returned = append(*returned, slug)
			}
		}
		return nil
	}
}

// extractSnapshotDB writes the seam.db entry of the archive at backupPath to
// dst (which must not exist), after checking the manifest describes a
// database at schema wantSchema. It reads the archive layout archive.Export
// writes -- manifest.json first, then seam.db -- and nothing else from it.
func extractSnapshotDB(backupPath, dst string, wantSchema int) error {
	f, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a backup archive: %w", backupPath, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil || hdr.Name != archive.ManifestName || hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("%s does not start with %s", backupPath, archive.ManifestName)
	}
	raw, err := io.ReadAll(io.LimitReader(tr, 1<<20+1))
	if err != nil {
		return err
	}
	var m archive.Manifest
	if len(raw) > 1<<20 || json.Unmarshal(raw, &m) != nil {
		return fmt.Errorf("%s has an unreadable manifest", backupPath)
	}
	switch {
	case m.FormatVersion != archive.FormatVersion:
		return fmt.Errorf("%s has archive format %d, not %d", backupPath, m.FormatVersion, archive.FormatVersion)
	case !m.IncludesDB:
		return fmt.Errorf("%s holds no database", backupPath)
	case m.SchemaVersion != wantSchema:
		return fmt.Errorf("%s holds schema %d, not %d", backupPath, m.SchemaVersion, wantSchema)
	}

	for {
		hdr, err = tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s holds no %s entry", backupPath, archive.DBName)
		}
		if err != nil {
			return err
		}
		if hdr.Name == archive.DBName {
			break
		}
	}
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("%s: %s is not a regular file entry", backupPath, archive.DBName)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, tr)
	if err == nil && n != hdr.Size {
		err = fmt.Errorf("short %s entry: %d of %d bytes", archive.DBName, n, hdr.Size)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	head := make([]byte, len(sqliteMagic))
	if df, err := os.Open(dst); err == nil {
		_, rerr := io.ReadFull(df, head)
		_ = df.Close()
		if rerr != nil || !bytes.Equal(head, []byte(sqliteMagic)) {
			return fmt.Errorf("%s: %s is not a SQLite database", backupPath, archive.DBName)
		}
	} else {
		return err
	}
	return nil
}
