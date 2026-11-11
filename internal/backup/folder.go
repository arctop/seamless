package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FolderState is a folder check's verdict.
type FolderState string

const (
	// FolderReady: the folder exists, is a directory, and as far as a look
	// can tell this account may write there.
	FolderReady FolderState = "ready"
	// FolderWillCreate: the folder does not exist yet, and the nearest
	// directory above it that does is one this account may write in.
	FolderWillCreate FolderState = "will-create"
	// FolderNotWritable: the file system's permissions refuse this account
	// (EACCES, a read-only disk).
	FolderNotWritable FolderState = "not-writable"
	// FolderPrivacy: macOS's privacy protection keeps Seamless out of the
	// folder (EPERM from TCC), which System Settings > Privacy & Security
	// answers, not chmod.
	FolderPrivacy FolderState = "privacy"
	// FolderInsideDataDir: the folder is inside Seamless's data folder (only
	// its own backups folder is allowed). Nothing else about such a path is
	// answered, so the check is no oracle into the corpus.
	FolderInsideDataDir FolderState = "inside-data-dir"
	// FolderNotDirectory: the path names a file, or passes through one.
	FolderNotDirectory FolderState = "not-directory"
	// FolderDisconnected: the folder is on a disk that is not mounted
	// (/Volumes/<disk> on macOS, /media/<user>/<disk> on Linux, a missing
	// drive letter on Windows). Nothing is created there: writing under an
	// empty mount point fills the boot disk instead.
	FolderDisconnected FolderState = "disconnected"
	// FolderMissing: a backup ran and the folder it was saved with is gone.
	// The check before saving answers FolderWillCreate instead; only the
	// default folder, LocalDir, is ever re-created by a backup.
	FolderMissing FolderState = "missing"
	// FolderUnsafe: under peer_auth, another account could rewrite the folder
	// or a directory above it, or the disk does not keep a file private to
	// the daemon's account (exFAT, a network share). Problem says which.
	FolderUnsafe FolderState = "unsafe"
	// FolderNotAbsolute: the path is not absolute.
	FolderNotAbsolute FolderState = "not-absolute"
	// FolderError: anything else; Problem carries the error.
	FolderError FolderState = "error"
)

// FolderCheck is what a folder check found.
type FolderCheck struct {
	// Dir is the folder asked about, cleaned.
	Dir   string      `json:"dir"`
	State FolderState `json:"state"`
	// Free is the space available to this account on the folder's disk (or,
	// for a folder still to be created, on the disk of the directory above
	// it), when FreeKnown.
	Free      uint64 `json:"free,omitempty"`
	FreeKnown bool   `json:"free_known,omitempty"`
	// Volume is the mount point a FolderDisconnected folder is waiting for.
	Volume string `json:"volume,omitempty"`
	// Problem is the detail behind FolderUnsafe and FolderError.
	Problem string `json:"problem,omitempty"`
}

// Usable reports whether a backup may be written there.
func (c FolderCheck) Usable() bool { return c.State == FolderReady || c.State == FolderWillCreate }

// Message is the owner-facing sentence for an unusable folder, or "" for a
// usable one. It names the folder the owner chose and nothing below it.
func (c FolderCheck) Message() string {
	switch c.State {
	case FolderNotWritable:
		return fmt.Sprintf("%s is not writable", c.Dir)
	case FolderPrivacy:
		return fmt.Sprintf("macOS is keeping Seamless out of %s: allow it in System Settings > Privacy & Security > Files and Folders (or Full Disk Access)", c.Dir)
	case FolderInsideDataDir:
		return "the folder is inside Seamless's data folder: choose a folder outside it"
	case FolderNotDirectory:
		return fmt.Sprintf("%s is not a folder", c.Dir)
	case FolderDisconnected:
		return fmt.Sprintf("%s is not connected", c.Volume)
	case FolderMissing:
		return fmt.Sprintf("%s no longer exists: reconnect its disk, or choose the folder again", c.Dir)
	case FolderUnsafe:
		return c.Problem
	case FolderNotAbsolute:
		return "the folder must be an absolute path"
	case FolderError:
		return fmt.Sprintf("%s cannot be used: %s", c.Dir, c.Problem)
	}
	return ""
}

// Reason is the fixed word an event records for an unusable folder.
func (c FolderCheck) Reason() string { return "folder-" + string(c.State) }

// FileFacts is what the peer_auth folder rule reads about a path, without
// following a final symlink.
type FileFacts struct {
	UID     uint32
	Mode    fs.FileMode
	IsDir   bool
	Symlink bool
}

// Guard holds backup folders to peer_auth's rule (cmd/seamlessd's
// filesystemProblems): the daemon's files live where no other account can
// rename them away or replace them, and every backup file stays private to
// the daemon's account. nil (peer_auth off) checks none of it.
type Guard struct {
	// EUID is the daemon's account.
	EUID int
	// Inspect reads a path's facts without following a final symlink. nil:
	// os.Lstat (macOS and Linux; peer_auth runs nowhere else).
	Inspect func(path string) (FileFacts, error)
}

func (g *Guard) inspect(path string) (FileFacts, error) {
	if g.Inspect != nil {
		return g.Inspect(path)
	}
	return lstatFacts(path)
}

// folders judges candidate backup folders for one data dir.
type folders struct {
	dataDir   string
	goos      string
	guard     *Guard
	freeBytes func(dir string) (uint64, error)
}

// check is the look the console takes while the owner types: stats, an
// access check and a directory read, and never a write. The data-dir guard
// answers first, before any probe of the path, so what lies inside the data
// dir changes no answer.
func (f folders) check(dir string) FolderCheck {
	c := FolderCheck{Dir: dir}
	if !filepath.IsAbs(dir) {
		c.State = FolderNotAbsolute
		return c
	}
	c.Dir = filepath.Clean(dir)
	if f.insideDataDir(c.Dir) {
		c.State = FolderInsideDataDir
		return c
	}
	if vol, ok := f.disconnected(c.Dir); ok {
		c.State, c.Volume = FolderDisconnected, vol
		return c
	}

	target := c.Dir
	fi, err := os.Stat(c.Dir)
	switch {
	case err == nil:
		if !fi.IsDir() {
			c.State = FolderNotDirectory
			return c
		}
		c.State = FolderReady
	case errors.Is(err, fs.ErrNotExist):
		parent, perr := nearestExisting(c.Dir)
		if perr != nil {
			return f.fail(c, perr)
		}
		pfi, perr := os.Stat(parent)
		if perr != nil {
			return f.fail(c, perr)
		}
		if !pfi.IsDir() {
			c.State = FolderNotDirectory
			return c
		}
		target, c.State = parent, FolderWillCreate
	default:
		return f.fail(c, err)
	}

	if err := canWrite(target); err != nil {
		return f.fail(c, err)
	}
	if err := readProbe(target); err != nil {
		return f.fail(c, err)
	}
	if f.guard != nil {
		if p := f.guardProblem(c.Dir); p != "" {
			c.State, c.Problem = FolderUnsafe, p
			return c
		}
	}
	f.fillFree(&c, target)
	return c
}

// prepare is what saving a folder (and every backup into it) does: the check,
// then the folder made (0700) and a file created and removed in it, so a
// folder that only looks writable is caught now. create false refuses a
// missing folder rather than making it (FolderMissing): a backup re-creates
// only LocalDir, since an empty mount point or a folder the owner removed is
// no place to write.
func (f folders) prepare(dir string, create bool) FolderCheck {
	c := f.check(dir)
	if !c.Usable() {
		return c
	}
	local := c.Dir == filepath.Clean(LocalDir(f.dataDir))
	if c.State == FolderWillCreate {
		if !create && !local {
			c.State = FolderMissing
			return c
		}
		if err := os.MkdirAll(c.Dir, 0o700); err != nil {
			return f.fail(c, err)
		}
	}
	if local {
		if err := os.Chmod(c.Dir, 0o700); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			return f.fail(c, err)
		}
	}
	if f.guard != nil {
		if p := f.guardProblem(c.Dir); p != "" {
			c.State, c.Problem = FolderUnsafe, p
			return c
		}
	}
	if err := f.writeProbe(c.Dir); err != nil {
		var unsafe *unsafeFileError
		if errors.As(err, &unsafe) {
			c.State, c.Problem = FolderUnsafe, unsafe.Error()
			return c
		}
		return f.fail(c, err)
	}
	c.State = FolderReady
	f.fillFree(&c, c.Dir)
	return c
}

// writeProbe creates, checks and removes a dot file in dir. Under the guard
// the file must come out owned by the daemon's account with no group or world
// bits: a disk that cannot keep that (exFAT, many network shares) cannot keep
// a backup private either.
func (f folders) writeProbe(dir string) error {
	p, err := os.CreateTemp(dir, ".seamless-probe-*")
	if err != nil {
		return err
	}
	name := p.Name()
	defer func() { _ = os.Remove(name) }()
	if err := p.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = p.Close()
		return err
	}
	if _, err := p.WriteString("seamless\n"); err != nil {
		_ = p.Close()
		return err
	}
	if err := p.Close(); err != nil {
		return err
	}
	if f.guard != nil {
		return f.privateFileProblem(name)
	}
	return nil
}

// unsafeFileError is a written file the disk did not keep private.
type unsafeFileError struct{ msg string }

func (e *unsafeFileError) Error() string { return e.msg }

// privateFileProblem checks one file the daemon wrote under the guard.
func (f folders) privateFileProblem(path string) error {
	facts, err := f.guard.inspect(path)
	if err != nil {
		return err
	}
	switch {
	case int(facts.UID) != f.guard.EUID:
		return &unsafeFileError{fmt.Sprintf("files written to %s come out owned by uid %d, not the daemon's account (uid %d): the disk does not keep ownership (exFAT, a network share) -- choose a folder on a disk that does", filepath.Dir(path), facts.UID, f.guard.EUID)}
	case facts.Mode.Perm()&0o077 != 0:
		return &unsafeFileError{fmt.Sprintf("files written to %s come out mode %04o, readable by other accounts: the disk does not keep permissions (exFAT, a network share) -- choose a folder on a disk that does", filepath.Dir(path), facts.Mode.Perm())}
	}
	return nil
}

// guardProblem applies peer_auth's rule to a folder: the folder itself, when
// it exists, must be a real directory belonging to the daemon's account (or
// root) that no other account may write, and every directory above it, along
// the path as written and along the path the links resolve to, must belong
// to root or the daemon's account and be writable by no other (a sticky
// directory such as /tmp excepted, as filesystemProblems excepts it).
func (f folders) guardProblem(dir string) string {
	if facts, err := f.guard.inspect(dir); err == nil {
		switch {
		case facts.Symlink:
			return fmt.Sprintf("%s is a symlink: choose the real folder", dir)
		case facts.UID != 0 && int(facts.UID) != f.guard.EUID:
			return fmt.Sprintf("%s is owned by uid %d, not the daemon's account (uid %d): another account could replace what is in it", dir, facts.UID, f.guard.EUID)
		case facts.Mode.Perm()&0o022 != 0:
			return fmt.Sprintf("%s is mode %04o, writable by other accounts", dir, facts.Mode.Perm())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("%s: %v", dir, err)
	}

	paths := []string{filepath.Dir(dir)}
	if near, err := nearestExisting(dir); err == nil {
		if real, err := filepath.EvalSymlinks(near); err == nil && real != near {
			paths = append(paths, real)
		}
	}
	for _, start := range paths {
		for p := start; ; p = filepath.Dir(p) {
			facts, err := f.guard.inspect(p)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				// Not there yet: made 0700 by the daemon when the folder is.
			case err != nil:
				return fmt.Sprintf("directory %s above %s: %v", p, dir, err)
			case facts.UID != 0 && int(facts.UID) != f.guard.EUID:
				return fmt.Sprintf("directory %s above %s is owned by uid %d, who could rename it away and replace it: keep backups under directories owned by root or the daemon's account", p, dir, facts.UID)
			case facts.Mode.Perm()&0o022 != 0 && facts.Mode&fs.ModeSticky == 0:
				return fmt.Sprintf("directory %s above %s is mode %04o, writable by other accounts", p, dir, facts.Mode.Perm())
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return ""
}

// insideDataDir reports whether dir is the data dir or anything under it other
// than LocalDir itself. It compares every ancestor, along the path as written
// and along the path its existing part resolves to, with the data dir by
// os.SameFile, which also catches another spelling of the same directory (a
// case variant on a case-insensitive disk, a symlink into the data dir). A
// path that only passes through the data dir lexically counts too, so a
// symlink inside the data dir that points out of it changes nothing.
func (f folders) insideDataDir(dir string) bool {
	data := filepath.Clean(f.dataDir)
	local := filepath.Clean(LocalDir(f.dataDir))
	if dir == local {
		return false
	}
	dataFI, statErr := os.Stat(data)
	same := func(p string) bool {
		if p == data {
			return true
		}
		if statErr != nil {
			return false
		}
		fi, err := os.Stat(p)
		return err == nil && os.SameFile(fi, dataFI)
	}
	// Another spelling of LocalDir (a case variant, the data dir reached
	// through its real path) is allowed only directly in the data dir: a link
	// planted deeper that leads to it would otherwise answer differently from
	// a missing path beside it.
	if same(filepath.Dir(dir)) {
		if lfi, err := os.Stat(local); err == nil {
			if dfi, err := os.Stat(dir); err == nil && os.SameFile(lfi, dfi) {
				return false
			}
		}
	}
	walk := func(start string) bool {
		for p := start; ; p = filepath.Dir(p) {
			if same(p) {
				return true
			}
			if filepath.Dir(p) == p {
				return false
			}
		}
	}
	if walk(dir) {
		return true
	}
	if near, err := nearestExisting(dir); err == nil {
		if real, err := filepath.EvalSymlinks(near); err == nil && walk(real) {
			return true
		}
	}
	return false
}

// disconnected reports the mount point dir is waiting for, when dir lies on a
// removable or network disk's mount point that is absent or is not a mount
// point at all (an empty directory left behind on the boot disk).
func (f folders) disconnected(dir string) (string, bool) {
	root := mountRoot(f.goos, dir)
	if root == "" {
		return "", false
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return root, errors.Is(err, fs.ErrNotExist)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		// /Volumes/Macintosh HD links to /: a link is not a mount point to wait
		// for.
		return "", false
	}
	if same, err := sameDevice(root, filepath.Dir(root)); err == nil && same {
		return root, true
	}
	return "", false
}

// mountRoot is the mount point a removable or network disk would be at for
// dir: /Volumes/<disk> on macOS; /media/<user>/<disk>, /run/media/<user>/<disk>
// and /mnt/<disk> on Linux; the drive root on Windows (other than C:). ""
// when dir lies on none.
func mountRoot(goos, dir string) string {
	parts := func(prefix string, n int) string {
		rest, ok := strings.CutPrefix(dir, prefix)
		if !ok {
			return ""
		}
		segs := strings.SplitN(rest, "/", n+1)
		if len(segs) < n || slicesHasEmpty(segs[:n]) {
			return ""
		}
		return prefix + strings.Join(segs[:n], "/")
	}
	switch goos {
	case "darwin":
		return parts("/Volumes/", 1)
	case "linux":
		for _, p := range []struct {
			prefix string
			n      int
		}{{"/run/media/", 2}, {"/media/", 2}, {"/mnt/", 1}} {
			if r := parts(p.prefix, p.n); r != "" {
				return r
			}
		}
	case "windows":
		// Parsed here, not with filepath.VolumeName, so the rule is the same
		// whatever OS reads it.
		if len(dir) >= 2 && dir[1] == ':' && isLetter(dir[0]) && !strings.EqualFold(dir[:2], "C:") {
			return dir[:2] + `\`
		}
	}
	return ""
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

func slicesHasEmpty(s []string) bool {
	for _, v := range s {
		if v == "" {
			return true
		}
	}
	return false
}

// nearestExisting is dir, or the nearest directory above it that exists.
func nearestExisting(dir string) (string, error) {
	for p := dir; ; p = filepath.Dir(p) {
		_, err := os.Lstat(p)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(p) == p {
			return "", err
		}
	}
}

// readProbe opens dir and reads one name: on macOS, the privacy protection
// refuses that (EPERM) where a stat alone may pass.
func readProbe(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if _, err := d.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// fail classifies err into the check's state.
func (f folders) fail(c FolderCheck, err error) FolderCheck {
	c.State = classify(f.goos, err)
	if c.State == FolderError {
		c.Problem = err.Error()
	}
	return c
}

// classify maps a file-system error to a folder state: EPERM on macOS is the
// privacy protection (TCC), any other permission error or a read-only disk is
// "not writable", a path through a file is "not a folder".
func classify(goos string, err error) FolderState {
	switch {
	case goos == "darwin" && errors.Is(err, syscall.EPERM):
		return FolderPrivacy
	case errors.Is(err, fs.ErrPermission), isReadOnlyFS(err):
		return FolderNotWritable
	case isNotDir(err):
		return FolderNotDirectory
	case errors.Is(err, fs.ErrNotExist):
		return FolderMissing
	}
	return FolderError
}

func (f folders) fillFree(c *FolderCheck, dir string) {
	if f.freeBytes == nil {
		return
	}
	if n, err := f.freeBytes(dir); err == nil {
		c.Free, c.FreeKnown = n, true
	}
}
