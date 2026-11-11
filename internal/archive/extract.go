package archive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

const (
	// maxManifestBytes bounds the first entry. The manifest is a few hundred
	// bytes of JSON; a gigabyte of it is a decompression bomb wearing the name,
	// and the reader has to decide that before it has anything else to go on.
	maxManifestBytes = 1 << 20

	// maxEntryBytes bounds one markdown entry, which is read whole so it can be
	// written atomically. The largest legitimate memory or note is orders of
	// magnitude under this; the cap exists so a crafted archive cannot make the
	// importer allocate without limit. The database entry is exempt because it
	// is streamed to disk rather than buffered.
	maxEntryBytes = 64 << 20
)

// archiveReader is the guarded reader for an archive stream.
//
// It is a streaming reader, not a random-access one, because an archive is read
// from a file OR from stdin and only one of those can seek. That is also why
// manifest.json is the first entry: every refusal that can be made from the
// manifest -- wrong format, a schema this build cannot understand -- is made
// before a single other entry has been read, let alone written.
type archiveReader struct {
	gz *gzip.Reader
	tr *tar.Reader

	// manifest is the parsed first entry, already checked.
	manifest Manifest

	// seen holds the entry names consumed so far. A repeated name is refused:
	// in a stream the second copy would silently overwrite the first, and an
	// archive that names the same file twice is not one this package wrote.
	seen map[string]bool
}

// openArchive reads and validates the manifest, leaving the reader positioned
// on the second entry.
//
// The three refusals here are the whole preflight, and each names a different
// thing the caller can do about it: ErrNotArchive means "this file is not one
// of ours", an unsupported format_version means "this build cannot read this
// container", and ErrSchemaTooNew means "this is ours and I am too old".
func openArchive(r io.Reader) (_ *archiveReader, retErr error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("archive.open: %w: not a gzip stream: %w", ErrNotArchive, err)
	}
	defer func() {
		if retErr != nil {
			_ = gz.Close()
		}
	}()

	tr := tar.NewReader(gz)
	m, err := readManifestEntry(tr, "archive.open")
	if err != nil {
		return nil, err
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("archive.open: unsupported format_version %d: this build reads %d",
			m.FormatVersion, FormatVersion)
	}
	if latest := store.LatestSchemaVersion(); m.SchemaVersion > latest {
		return nil, fmt.Errorf("archive.open: %w: schema_version %d, this build understands up to %d; run `seamlessd update`",
			ErrSchemaTooNew, m.SchemaVersion, latest)
	}

	return &archiveReader{
		gz:       gz,
		tr:       tr,
		manifest: m,
		seen:     map[string]bool{ManifestName: true},
	}, nil
}

// readManifestEntry reads the first tar entry as the manifest, with the checks
// that make an archive one of ours: the name, a regular file, the size cap and
// valid JSON. The format and schema refusals are openArchive's, because a list
// of backups has to show an archive this build cannot restore and say why.
func readManifestEntry(tr *tar.Reader, op string) (Manifest, error) {
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w: no entries: %w", op, ErrNotArchive, err)
	}
	if hdr.Name != ManifestName {
		return Manifest{}, fmt.Errorf("%s: %w: first entry is %q, want %q",
			op, ErrNotArchive, hdr.Name, ManifestName)
	}
	if hdr.Typeflag != tar.TypeReg {
		return Manifest{}, fmt.Errorf("%s: %w: %s is not a regular file entry",
			op, ErrUnsafeEntry, ManifestName)
	}

	data, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w: read manifest: %w", op, ErrNotArchive, err)
	}
	if len(data) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%s: %w: manifest exceeds %d bytes", op, ErrNotArchive, maxManifestBytes)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w: manifest is not valid JSON: %w", op, ErrNotArchive, err)
	}
	return m, nil
}

// ReadManifest returns an archive's manifest, reading its first entry and
// nothing after it: cheap enough to run over every backup in a folder. Unlike
// Import it refuses neither an unsupported format_version nor a schema newer
// than this build, which a caller compares with FormatVersion and
// store.LatestSchemaVersion to say why such a backup cannot be restored here.
// An error wraps ErrNotArchive, or ErrUnsafeEntry for a manifest entry that is
// not a regular file.
func ReadManifest(r io.Reader) (Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("archive.ReadManifest: %w: not a gzip stream: %w", ErrNotArchive, err)
	}
	defer func() { _ = gz.Close() }()
	return readManifestEntry(tar.NewReader(gz), "archive.ReadManifest")
}

// Close releases the gzip reader. It does not close the underlying stream,
// which the caller owns.
func (a *archiveReader) Close() error { return a.gz.Close() }

// nextEntry advances to the next entry and applies the layout guards, returning
// io.EOF at the end of the archive. Every guard runs on the HEADER, before the
// body has been read or a byte written, so a refusal costs nothing and leaves
// nothing behind.
func (a *archiveReader) nextEntry() (*tar.Header, error) {
	hdr, err := a.tr.Next()
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("archive.extract: read entry: %w", err)
	}
	// TypeReg only. A symlink or hardlink entry is the classic tar escape (it
	// redirects a later entry's write outside the destination), and a directory
	// entry exists only to carry a permission, which this format does not do.
	if hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("archive.extract: %w: %s has tar type %q, want a regular file",
			ErrUnsafeEntry, hdr.Name, string(hdr.Typeflag))
	}
	if err := checkEntryName(hdr.Name); err != nil {
		return nil, err
	}
	if a.seen[hdr.Name] {
		return nil, fmt.Errorf("archive.extract: %w: %s appears twice", ErrUnsafeEntry, hdr.Name)
	}
	a.seen[hdr.Name] = true
	return hdr, nil
}

// checkEntryName enforces the archive layout on an entry name.
//
// The name is checked as a slash path first (backslash rejected outright, and
// path.Clean must be a fixed point, which kills "memory/./x.md", "memory//x.md"
// and a trailing slash) and only then handed to validate.Path, which is the
// repo's shared traversal guard. The layout check is last because it is the
// narrowest: even a perfectly safe relative path is refused unless it is one of
// the two fixed names or a .md file under one of the two trees.
func checkEntryName(name string) error {
	if name == "" {
		return fmt.Errorf("archive.extract: %w: empty entry name", ErrUnsafeEntry)
	}
	if strings.ContainsRune(name, '\\') {
		return fmt.Errorf("archive.extract: %w: %q contains a backslash", ErrUnsafeEntry, name)
	}
	if path.Clean(name) != name {
		return fmt.Errorf("archive.extract: %w: %q is not a clean path", ErrUnsafeEntry, name)
	}
	if err := validate.Path(name); err != nil {
		return fmt.Errorf("archive.extract: %w: %q: %w", ErrUnsafeEntry, name, err)
	}
	if name == ManifestName || name == DBName {
		return nil
	}
	top, rest, ok := strings.Cut(name, "/")
	if !ok || rest == "" || (top != MemoryTree && top != NotesTree) {
		return fmt.Errorf("archive.extract: %w: %q is outside the archive layout", ErrUnsafeEntry, name)
	}
	if path.Ext(name) != ".md" {
		return fmt.Errorf("archive.extract: %w: %q is not a markdown file", ErrUnsafeEntry, name)
	}
	return nil
}

// extraction records what one extractTo put on disk, so a failure part-way
// through can put the destination back the way it found it. A half-extracted
// archive is worse than none: the next run's DetectMode would see corpus files
// and choose merge, folding a partial restore into an instance that has no
// database yet.
type extraction struct {
	dest string

	// files and dirs are absolute paths, in creation order.
	files []string
	dirs  []string

	// dbPath is where the seam.db entry landed, "" when the archive carried
	// none (a NoDB export).
	dbPath string

	MemoryFiles int
	NoteFiles   int
}

// rollback removes everything this extraction created, deepest first.
// Directories that turn out to be non-empty (because something else was already
// there) are left alone: os.Remove refuses them, which is the desired outcome.
func (ex *extraction) rollback() {
	for i := len(ex.files) - 1; i >= 0; i-- {
		_ = os.Remove(ex.files[i])
	}
	if ex.dbPath != "" {
		_ = os.Remove(ex.dbPath)
	}
	for i := len(ex.dirs) - 1; i >= 0; i-- {
		_ = os.Remove(ex.dirs[i])
	}
}

// extractTo writes every remaining entry into dest, which is created with 0700
// if it does not exist.
//
// dbName is the on-disk name the seam.db entry is written under. A fresh
// restore passes a temporary name and renames it into place after the whole
// archive has been read, so an interrupted restore leaves the markdown trees
// and no database -- which the next run detects as a fresh destination again.
//
// Extraction is rooted with os.OpenRoot(dest): every directory this creates and
// every existing entry it inspects is resolved through the root, so a symlink
// planted in the destination cannot redirect a component outside it. The
// markdown writes themselves go through files.AtomicWrite (temp + fsync +
// rename, 0600), the same writer the corpus uses everywhere else, on a path
// whose parent the root has just verified is a real directory inside dest; the
// database is streamed through the root with O_EXCL instead, because buffering
// a multi-hundred-megabyte snapshot to hand it to AtomicWrite would be the only
// unbounded allocation in the importer.
func (a *archiveReader) extractTo(ctx context.Context, dest, dbName string) (_ *extraction, retErr error) {
	ex := &extraction{dest: dest}
	defer func() {
		if retErr != nil {
			ex.rollback()
		}
	}()

	if err := ex.ensureDest(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return nil, fmt.Errorf("archive.extract: open destination %s: %w", dest, err)
	}
	defer func() { _ = root.Close() }()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hdr, err := a.nextEntry()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		// The layout guards above are about the archive; this one is about the
		// destination, and both have to hold.
		if err := validate.PathWithinDir(filepath.FromSlash(hdr.Name), dest); err != nil {
			return nil, fmt.Errorf("archive.extract: %w: %s: %w", ErrUnsafeEntry, hdr.Name, err)
		}

		target := hdr.Name
		if hdr.Name == DBName {
			target = dbName
		}
		if err := ex.mkdirAllRoot(root, path.Dir(target)); err != nil {
			return nil, err
		}
		if err := ensureWritableTarget(root, target); err != nil {
			return nil, err
		}

		if hdr.Name == DBName {
			if err := ex.writeStream(root, target, a.tr); err != nil {
				return nil, err
			}
			continue
		}
		if err := ex.writeAtomic(target, a.tr); err != nil {
			return nil, err
		}
		switch {
		case strings.HasPrefix(hdr.Name, MemoryTree+"/"):
			ex.MemoryFiles++
		case strings.HasPrefix(hdr.Name, NotesTree+"/"):
			ex.NoteFiles++
		}
	}
	return ex, nil
}

// ensureDest creates the destination directory when it is absent, recording it
// so a failed extraction removes the directory it invented.
func (ex *extraction) ensureDest() error {
	if info, err := os.Lstat(ex.dest); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive.extract: %w: destination %s is a symlink", ErrUnsafeEntry, ex.dest)
		}
		if !info.IsDir() {
			return fmt.Errorf("archive.extract: %w: destination %s is not a directory", ErrUnsafeEntry, ex.dest)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("archive.extract: destination %s: %w", ex.dest, err)
	}
	if err := os.MkdirAll(ex.dest, 0o700); err != nil {
		return fmt.Errorf("archive.extract: create destination %s: %w", ex.dest, err)
	}
	ex.dirs = append(ex.dirs, ex.dest)
	return nil
}

// mkdirAllRoot creates dir (a slash path relative to root) one component at a
// time. It is not root.MkdirAll because the rollback needs to know which
// directories it actually created, and because a component that already exists
// as a symlink or a plain file is a refusal rather than something to walk
// through.
func (ex *extraction) mkdirAllRoot(root *os.Root, dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(dir, "/") {
		if cur == "" {
			cur = seg
		} else {
			cur += "/" + seg
		}
		info, err := root.Lstat(cur)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive.extract: %w: %s is a symlink", ErrUnsafeEntry, cur)
			}
			if !info.IsDir() {
				return fmt.Errorf("archive.extract: %w: %s is not a directory", ErrUnsafeEntry, cur)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("archive.extract: inspect %s: %w", cur, err)
		}
		if err := root.Mkdir(cur, 0o700); err != nil {
			return fmt.Errorf("archive.extract: create %s: %w", cur, err)
		}
		ex.dirs = append(ex.dirs, filepath.Join(ex.dest, filepath.FromSlash(cur)))
	}
	return nil
}

// ensureWritableTarget refuses to write onto anything that is not an ordinary
// file. A destination the importer considers empty can still hold a symlink --
// DetectMode counts REGULAR files, so a tree of dangling links reads as fresh --
// and writing through one is how an archive gets to name a file outside the
// data dir.
func ensureWritableTarget(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("archive.extract: inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive.extract: %w: %s exists and is not a regular file", ErrUnsafeEntry, name)
	}
	return nil
}

// writeAtomic reads one entry whole and writes it with the corpus writer.
func (ex *extraction) writeAtomic(name string, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, maxEntryBytes+1))
	if err != nil {
		return fmt.Errorf("archive.extract: read %s: %w", name, err)
	}
	if len(data) > maxEntryBytes {
		return fmt.Errorf("archive.extract: %w: %s exceeds %d bytes", ErrUnsafeEntry, name, maxEntryBytes)
	}
	abs := filepath.Join(ex.dest, filepath.FromSlash(name))
	if err := files.AtomicWrite(abs, data, 0o600); err != nil {
		return fmt.Errorf("archive.extract: write %s: %w", name, err)
	}
	ex.files = append(ex.files, abs)
	return nil
}

// writeStream copies one entry to a new file through the root. O_EXCL rather
// than a rename-over: the database is only ever extracted to a name nothing
// else owns (a temporary one for a fresh restore, a private temp dir for a
// merge), so clobbering is a bug, not a case to handle.
func (ex *extraction) writeStream(root *os.Root, name string, r io.Reader) (retErr error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("archive.extract: create %s: %w", name, err)
	}
	// Recorded before the copy so a failure mid-stream is rolled back.
	ex.dbPath = filepath.Join(ex.dest, filepath.FromSlash(name))
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("archive.extract: write %s: %w", name, err)
	}
	// The umask can narrow but never widen the mode O_CREATE asked for, so the
	// chmod is what makes 0600 exact rather than a ceiling.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("archive.extract: chmod %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("archive.extract: sync %s: %w", name, err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return fmt.Errorf("archive.extract: close %s: %w", name, err)
	}
	return nil
}
