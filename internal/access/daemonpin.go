package access

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The daemon's account, pinned by root (the owner's decision of 2026-11-10,
// after SystemAccount). A system account alone does not single the daemon out:
// a deployment may run other services, or a restricted local account, as role
// accounts too, and any of them would pass. So root writes the daemon's account
// into a file only root can change, and seam approve, seam touchid enroll, the
// Touch ID approver (macos/SeamApprove, Socket.swift, which mirrors this file)
// and serve accept that account and no other. With no pin they fall back to
// SystemAccount.
//
// The file holds one line, the account's name and its uid separated by one
// space, with at most one newline after it:
//
//	_seamless 480
//
// The name is for messages; every comparison is by uid, so no user database is
// consulted at check time. The name is a letter or "_" then letters, digits,
// ".", "_" or "-", at most 64 bytes. The uid is decimal with no leading zero,
// at least 1 (root is never the daemon's account) and within a uid's range.
// Anything else -- a second line, a tab, a carriage return, a third field -- is
// refused.
//
// The pin is believed only when nobody but root can have written it: a regular
// file owned by root and writable by no group or other account, every
// directory above it (on its real path after symlinks, and on the path its
// directory resolves to) owned by root and writable by no group or other
// account, sticky bit or not. A pin that is there but fails any of this, or
// does not parse, is ErrDaemonPin: every caller refuses on it and none falls
// back, since falling back is what a forged or broken pin would want. A pin
// that is not there at all (nothing at the path) is the fallback.

// DaemonAccountPath is where root pins the daemon's account.
const DaemonAccountPath = "/opt/seamless/etc/daemon-account"

// maxPinBytes bounds what is read of a pin: one short line.
const maxPinBytes = 256

// ErrDaemonPin marks a pin that is there but is not believed. Its message names
// the path and the problem.
var ErrDaemonPin = errors.New("the daemon account pin")

// DaemonPin is a trusted, parsed pin.
type DaemonPin struct {
	Name string
	UID  uint32
	// Path is the file it was read from.
	Path string
}

// String is the account as messages name it: "_seamless (uid 480)".
func (p DaemonPin) String() string { return fmt.Sprintf("%s (uid %d)", p.Name, p.UID) }

// pinFacts is what the trust check needs of one path.
type pinFacts struct {
	uid  uint32
	mode fs.FileMode
}

// pinFS is how a pin is read: hostPinFS on this platform, a stand-in in tests.
type pinFS struct {
	// lstat reports a path without following a final symlink; an error that
	// is fs.ErrNotExist means nothing is there.
	lstat func(string) (pinFacts, error)
	// evalSymlinks resolves every symlink in a path.
	evalSymlinks func(string) (string, error)
	// open reads the file at a resolved path: the facts of the descriptor it
	// read (fstat, so they are the file read), and at most n bytes.
	open func(path string, n int) (pinFacts, []byte, error)
}

// DaemonAccount reads the pin at DaemonAccountPath. present is false when
// nothing is there; a pin that is there but not believed is an ErrDaemonPin
// error (with present true), on which the caller refuses.
func DaemonAccount() (pin DaemonPin, present bool, err error) {
	return readDaemonPin(DaemonAccountPath, hostPinFS)
}

// DaemonUID is the rule every client and serve hold a uid to as the daemon's
// account: with a pin, the pinned uid alone; with none, a system account, as
// system judges it (SystemAccount, or a test's stand-in). The rule applied
// comes back for the end of a refusal. Callers refuse uid 0 and their own uid
// before asking, and refuse on a pin error rather than call this.
func DaemonUID(uid uint32, pin DaemonPin, pinned bool, system func(uint32) (bool, string)) (bool, string) {
	if pinned {
		return uid == pin.UID, fmt.Sprintf("%s pins the daemon's account as %s", pin.Path, pin)
	}
	return system(uid)
}

func pinError(path, problem string) error {
	return fmt.Errorf("%w %s %s", ErrDaemonPin, path, problem)
}

// readDaemonPin is DaemonAccount over fsys, for the pin at path.
func readDaemonPin(path string, fsys pinFS) (DaemonPin, bool, error) {
	if _, err := fsys.lstat(path); errors.Is(err, fs.ErrNotExist) {
		return DaemonPin{}, false, nil
	} else if err != nil {
		return DaemonPin{}, true, pinError(path, fmt.Sprintf("cannot be read: %v", err))
	}
	real, err := fsys.evalSymlinks(path)
	if err != nil {
		return DaemonPin{}, true, pinError(path, fmt.Sprintf("cannot be resolved: %v", err))
	}
	// The directory the path names decides what a symlink at it points to,
	// and the directory the file is really in decides what the file is.
	parent, err := fsys.evalSymlinks(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return DaemonPin{}, true, pinError(path, fmt.Sprintf("cannot be resolved: %v", err))
	}
	dirs := []string{parent}
	if d := filepath.Dir(real); d != parent {
		dirs = append(dirs, d)
	}
	for _, dir := range dirs {
		if p := pinDirProblem(dir, fsys); p != "" {
			return DaemonPin{}, true, pinError(path, p)
		}
	}
	// The file as it stands, before it is opened, and as it was read.
	facts, err := fsys.lstat(real)
	if err != nil {
		return DaemonPin{}, true, pinError(path, fmt.Sprintf("cannot be read: %v", err))
	}
	if p := pinFileProblem(facts); p != "" {
		return DaemonPin{}, true, pinError(path, p)
	}
	facts, data, err := fsys.open(real, maxPinBytes+1)
	if err != nil {
		return DaemonPin{}, true, pinError(path, fmt.Sprintf("cannot be read: %v", err))
	}
	if p := pinFileProblem(facts); p != "" {
		return DaemonPin{}, true, pinError(path, p)
	}
	pin, err := parseDaemonPin(data)
	if err != nil {
		return DaemonPin{}, true, pinError(path, err.Error())
	}
	pin.Path = path
	return pin, true, nil
}

// pinDirProblem walks dir and every directory above it, and says why one could
// be rewritten by an account other than root, or "" when none can.
func pinDirProblem(dir string, fsys pinFS) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		facts, err := fsys.lstat(d)
		switch {
		case err != nil:
			return fmt.Sprintf("is under %s, which cannot be read: %v", d, err)
		case !facts.mode.IsDir():
			return fmt.Sprintf("is under %s, which is not a directory", d)
		case facts.uid != 0:
			return fmt.Sprintf("is under %s, which is owned by uid %d: every directory above the pin must belong to root", d, facts.uid)
		case facts.mode.Perm()&0o022 != 0:
			return fmt.Sprintf("is under %s, which is mode %04o: every directory above the pin must be writable by root alone", d, facts.mode.Perm())
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// pinFileProblem says why the pin file itself could be another account's, or
// "" when only root can have written it.
func pinFileProblem(f pinFacts) string {
	switch {
	case !f.mode.IsRegular():
		return "is not a regular file"
	case f.uid != 0:
		return fmt.Sprintf("is owned by uid %d: it must belong to root", f.uid)
	case f.mode.Perm()&0o022 != 0:
		return fmt.Sprintf("is mode %04o: it must be writable by root alone (0644)", f.mode.Perm())
	}
	return ""
}

var (
	pinName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	pinUID  = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
)

// ValidDaemonAccountName reports whether name may stand in a pin: the shape
// the package comment gives, which is also safe to print inside a shell
// command line.
func ValidDaemonAccountName(name string) bool { return pinName.MatchString(name) }

// parseDaemonPin reads the one line the package comment describes.
func parseDaemonPin(data []byte) (DaemonPin, error) {
	const want = `: it must be one line, the daemon's account name and uid ("_seamless 480")`
	if len(data) > maxPinBytes {
		return DaemonPin{}, fmt.Errorf("is longer than %d bytes%s", maxPinBytes, want)
	}
	line := strings.TrimSuffix(string(data), "\n")
	if strings.ContainsAny(line, "\n\r") {
		return DaemonPin{}, errors.New("has more than one line" + want)
	}
	if line == "" {
		return DaemonPin{}, errors.New("is empty" + want)
	}
	fields := strings.Split(line, " ")
	if len(fields) != 2 {
		return DaemonPin{}, fmt.Errorf("has %d space-separated fields, not 2%s", len(fields), want)
	}
	name, digits := fields[0], fields[1]
	if !ValidDaemonAccountName(name) {
		return DaemonPin{}, fmt.Errorf("names %q, which is not an account name%s", name, want)
	}
	uid, err := strconv.ParseUint(digits, 10, 32)
	if !pinUID.MatchString(digits) || err != nil {
		return DaemonPin{}, fmt.Errorf("gives %q as the uid, which is not a uid above 0%s", digits, want)
	}
	return DaemonPin{Name: name, UID: uint32(uid)}, nil
}
