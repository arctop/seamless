package backup

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/store"
)

// Where a listed backup lives.
const (
	// WhereFolder is the chosen folder (Settings.Dir).
	WhereFolder = "folder"
	// WhereLocal is LocalDir, listed beside a chosen folder elsewhere: the
	// safety backups a restore takes and the updater's pre-update backups.
	WhereLocal = "local"
)

// Entry is one backup in a listing.
type Entry struct {
	// Ref names the backup to the service: "<where>:<name>". It is resolved
	// against a fresh listing, never used as a path.
	Ref   string `json:"ref"`
	Where string `json:"where"`
	Name  string `json:"name"`
	// Path is the file, absolute.
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`

	// Named is true when the name is one this package or the updater writes,
	// and Parsed is what it says.
	Named  bool `json:"named"`
	Parsed Name `json:"parsed"`

	// Kind is why the backup was written: the manifest's kind when it was
	// read (empty for a plain export), else the one the name implies.
	Kind string `json:"kind"`
	// CreatedAt is when the snapshot was taken: the manifest's time, else the
	// name's, else the file's modification time.
	CreatedAt time.Time `json:"created_at"`

	// Manifest is the archive's manifest, nil when it was not read (a
	// dataless placeholder) or could not be (Unreadable says why).
	Manifest   *archive.Manifest `json:"manifest,omitempty"`
	Unreadable string            `json:"unreadable,omitempty"`
	// Dataless is an iCloud Drive or File Provider placeholder whose contents
	// are not on this machine: listed from its name, never opened, and not
	// restorable until it is downloaded.
	Dataless bool `json:"dataless,omitempty"`

	// Ours is a backup this install wrote: the manifest names its install id
	// (or, for a placeholder, the service recorded writing it).
	Ours bool `json:"ours,omitempty"`
	// FromAnother is a backup another install wrote (another machine, or a
	// reinstall): its manifest names an install id that is not ours.
	FromAnother bool `json:"from_another,omitempty"`
	// TooNew is a backup a newer Seamless made, which this build cannot
	// restore: an archive format or a database schema it does not know.
	TooNew bool `json:"too_new,omitempty"`
}

// Fingerprint is the backup's content fingerprint, "" when unknown.
func (e Entry) Fingerprint() string {
	if e.Manifest == nil {
		return ""
	}
	return e.Manifest.Fingerprint
}

// Listing is what List found.
type Listing struct {
	// Dir is the chosen folder, and DirCheck its state when it could not be
	// listed (Usable otherwise).
	Dir      string      `json:"dir"`
	DirCheck FolderCheck `json:"dir_check"`
	// Local is LocalDir when it is listed too (the chosen folder is
	// elsewhere), else "".
	Local string `json:"local,omitempty"`
	// Entries are newest first.
	Entries []Entry `json:"entries"`
}

// Find returns the entry ref names.
func (l Listing) Find(ref string) (Entry, bool) {
	for _, e := range l.Entries {
		if e.Ref == ref {
			return e, true
		}
	}
	return Entry{}, false
}

// manifestCache keeps each listed file's manifest by path, size and
// modification time, so a list of large backups in a cloud folder reads each
// one once.
type manifestCache struct {
	mu sync.Mutex
	m  map[string]cachedManifest
}

type cachedManifest struct {
	size     int64
	mod      time.Time
	manifest *archive.Manifest
	err      string
}

func (c *manifestCache) read(path string, fi fs.FileInfo) (*archive.Manifest, string) {
	c.mu.Lock()
	if got, ok := c.m[path]; ok && got.size == fi.Size() && got.mod.Equal(fi.ModTime()) {
		c.mu.Unlock()
		return got.manifest, got.err
	}
	c.mu.Unlock()

	m, err := readManifestFile(path)
	entry := cachedManifest{size: fi.Size(), mod: fi.ModTime()}
	if err != nil {
		entry.err = manifestProblem(err)
	} else {
		entry.manifest = &m
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]cachedManifest{}
	}
	c.m[path] = entry
	c.mu.Unlock()
	return entry.manifest, entry.err
}

// retain drops every cached path a listing of dirs no longer holds, keeping
// the paths of other folders.
func (c *manifestCache) retain(dirs []string, seen map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.m {
		if slices.Contains(dirs, filepath.Dir(p)) && !seen[p] {
			delete(c.m, p)
		}
	}
}

func (c *manifestCache) forget(path string) {
	c.mu.Lock()
	delete(c.m, path)
	c.mu.Unlock()
}

func readManifestFile(path string) (archive.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return archive.Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	return archive.ReadManifest(f)
}

// manifestProblem is the short reason a backup's manifest could not be read.
func manifestProblem(err error) string {
	switch {
	case errors.Is(err, archive.ErrNotArchive), errors.Is(err, archive.ErrUnsafeEntry):
		return "damaged: not a readable Seamless backup"
	case errors.Is(err, fs.ErrPermission):
		return "not readable by Seamless"
	}
	return err.Error()
}

// lister lists backup folders for the service.
type lister struct {
	folders folders
	cache   *manifestCache
}

// list reads the chosen folder and, when it is elsewhere, LocalDir. installID
// is this install's; wrote reports a backup the service recorded writing
// (for a placeholder whose manifest stays unread).
func (l lister) list(dir, installID string, wrote func(dir, name string) bool) Listing {
	out := Listing{Dir: dir}
	local := filepath.Clean(LocalDir(l.folders.dataDir))
	dirs := []string{dir}
	if !sameDir(dir, local) {
		out.Local = local
		dirs = append(dirs, local)
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		where := WhereFolder
		if d == out.Local {
			where = WhereLocal
		}
		entries, check := l.listDir(d, where, installID, wrote, seen)
		if where == WhereFolder {
			out.DirCheck = check
		}
		out.Entries = append(out.Entries, entries...)
	}
	l.cache.retain(dirs, seen)
	slices.SortFunc(out.Entries, func(a, b Entry) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.Name, a.Name)
	})
	return out
}

// listDir lists one folder. A folder inside the data dir (other than
// LocalDir) is never read, whatever the config says. A folder that cannot be
// read lists nothing, and the check says why.
func (l lister) listDir(dir, where, installID string, wrote func(dir, name string) bool, seen map[string]bool) ([]Entry, FolderCheck) {
	check := FolderCheck{Dir: dir, State: FolderReady}
	if l.folders.insideDataDir(dir) {
		check.State = FolderInsideDataDir
		return nil, check
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if c := l.folders.check(dir); c.State != FolderWillCreate {
				return nil, c
			}
			// Not made yet: an empty list, not a problem.
			return nil, FolderCheck{Dir: dir, State: FolderWillCreate}
		}
		return nil, l.folders.fail(check, err)
	}
	var out []Entry
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") || !de.Type().IsRegular() || !strings.HasSuffix(name, nameExt) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue // gone since the read
		}
		path := filepath.Join(dir, name)
		e := Entry{
			Ref: where + ":" + name, Where: where, Name: name, Path: path,
			Size: fi.Size(), ModTime: fi.ModTime(), Dataless: isDataless(fi),
		}
		e.Parsed, e.Named = ParseName(name)
		if e.Dataless {
			if !e.Named {
				// Reading it would download it, and its name does not say it
				// is ours to list.
				continue
			}
		} else {
			e.Manifest, e.Unreadable = l.cache.read(path, fi)
			seen[path] = true
			if !e.Named && e.Manifest == nil {
				continue // some other .tar.gz
			}
		}
		describe(&e, installID, wrote)
		out = append(out, e)
	}
	return out, check
}

// describe fills an entry's kind, time and provenance from what was read.
func describe(e *Entry, installID string, wrote func(dir, name string) bool) {
	switch {
	case e.Manifest != nil:
		m := e.Manifest
		e.Kind = m.Kind
		e.CreatedAt = m.CreatedAt
		e.Ours = installID != "" && m.InstallID == installID
		e.FromAnother = installID != "" && m.InstallID != "" && m.InstallID != installID
		e.TooNew = m.FormatVersion > archive.FormatVersion || m.SchemaVersion > store.LatestSchemaVersion()
	case e.Named:
		e.Kind = e.Parsed.Kind
		e.CreatedAt = e.Parsed.At
		e.Ours = e.Dataless && wrote != nil && wrote(filepath.Dir(e.Path), e.Name)
	}
	if e.CreatedAt.IsZero() {
		if e.Named {
			e.CreatedAt = e.Parsed.At
		} else {
			e.CreatedAt = e.ModTime.UTC()
		}
	}
}

// sameDir reports whether a and b name one directory: the same path, or two
// that exist and are the same file.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// parseRef splits a Ref into where and name, refusing anything that is not
// one of ours.
func parseRef(ref string) (where, name string, err error) {
	where, name, ok := strings.Cut(ref, ":")
	if !ok || (where != WhereFolder && where != WhereLocal) || name == "" ||
		strings.ContainsAny(name, `/\`) || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return "", "", fmt.Errorf("%w: %q", ErrNoSuchBackup, ref)
	}
	return where, name, nil
}
