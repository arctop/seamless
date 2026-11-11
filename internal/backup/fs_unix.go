//go:build !windows

package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// canWrite asks the kernel whether this account may create entries in dir,
// without creating one.
func canWrite(dir string) error {
	if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
		return &fs.PathError{Op: "access", Path: dir, Err: err}
	}
	return nil
}

// lstatFacts reads a path's owner and mode, not following a final symlink.
func lstatFacts(path string) (FileFacts, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return FileFacts{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileFacts{}, fmt.Errorf("%s: no owner to read", path)
	}
	return FileFacts{
		UID: st.Uid, Mode: fi.Mode(), IsDir: fi.IsDir(), Symlink: fi.Mode()&fs.ModeSymlink != 0,
	}, nil
}

// sameDevice reports whether a and b are on the same file system: a mount
// point and the directory above it are not, unless nothing is mounted there.
func sameDevice(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

func isReadOnlyFS(err error) bool { return errors.Is(err, unix.EROFS) }

func isNotDir(err error) bool { return errors.Is(err, unix.ENOTDIR) }

// syncDir flushes dir's entries, so a rename that published a backup survives
// a power loss. Best effort: some file systems refuse to sync a directory.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync() //nolint:errcheck // best effort: some file systems refuse to sync a directory, and the rename stands either way
	_ = d.Close()
}
