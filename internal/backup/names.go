package backup

import (
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/archive"
)

// The parts of a backup's file name:
//
//	seamless-<host>-<YYYYMMDDTHHMMSSZ>.tar.gz                  made by you (Back up now, seamlessd export)
//	seamless-<host>-<YYYYMMDDTHHMMSSZ>-auto.tar.gz             automatic
//	seamless-<host>-<YYYYMMDDTHHMMSSZ>-before-restore.tar.gz   the safety backup a restore takes
//	pre-update-v<version>-<YYYYMMDDTHHMMSSZ>.tar.gz            the updater's (cmd/seamlessd/update_backup.go)
//
// The host is sanitized (HostForFilename) and may itself hold dashes, so a
// name is parsed from the right: the extension, then the kind suffix, then the
// fixed-width stamp.
const (
	namePrefix          = "seamless-"
	nameExt             = ".tar.gz"
	nameStamp           = "20060102T150405Z"
	suffixAuto          = "-auto"
	suffixBeforeRestore = "-before-restore"
	preUpdatePrefix     = "pre-update-v"
)

// FileName is the name of a backup of kind (an archive.Kind* constant; empty
// or KindManual is a backup the owner made) taken on host at at.
// KindPreUpdate is the updater's to name, not this package's, and is written
// as a manual name here.
func FileName(host string, at time.Time, kind string) string {
	name := namePrefix + HostForFilename(host) + "-" + at.UTC().Format(nameStamp)
	switch kind {
	case archive.KindScheduled:
		name += suffixAuto
	case archive.KindBeforeRestore:
		name += suffixBeforeRestore
	}
	return name + nameExt
}

// HostForFilename reduces a hostname to lowercase letters, digits, dot, dash
// and underscore. An empty or fully-unusable hostname becomes "unknown" rather
// than an empty segment: config.Hostname returns "" for a machine whose name
// could not be read, and "seamless--20260101T000000Z.tar.gz" hides that.
func HostForFilename(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(host)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "unknown"
	}
	return name
}

// Name is what a backup's file name says about it.
type Name struct {
	// Kind is the archive kind the name implies: KindScheduled for -auto,
	// KindBeforeRestore, KindPreUpdate, and KindManual for a plain
	// seamless-<host>-<stamp> name, which `seamlessd export` writes too (the
	// manifest's Kind tells those apart).
	Kind string
	// Host is the sanitized host in a seamless- name; empty for a pre-update
	// one.
	Host string
	// At is the stamp, UTC.
	At time.Time
	// FromVersion is the release a pre-update backup was taken before
	// replacing, as its name spells it (no leading v).
	FromVersion string
}

// ParseName reads a name FileName or the updater wrote, ok false for any
// other: a dot-prefixed temp file, another extension, a stamp that does not
// parse.
func ParseName(name string) (Name, bool) {
	if strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return Name{}, false
	}
	rest, ok := strings.CutSuffix(name, nameExt)
	if !ok {
		return Name{}, false
	}
	if v, ok := strings.CutPrefix(rest, preUpdatePrefix); ok {
		i := strings.LastIndexByte(v, '-')
		if i <= 0 || !plausibleVersion(v[:i]) {
			return Name{}, false
		}
		at, err := time.Parse(nameStamp, v[i+1:])
		if err != nil {
			return Name{}, false
		}
		return Name{Kind: archive.KindPreUpdate, At: at.UTC(), FromVersion: v[:i]}, true
	}

	rest, ok = strings.CutPrefix(rest, namePrefix)
	if !ok {
		return Name{}, false
	}
	n := Name{Kind: archive.KindManual}
	if r, ok := strings.CutSuffix(rest, suffixAuto); ok {
		rest, n.Kind = r, archive.KindScheduled
	} else if r, ok := strings.CutSuffix(rest, suffixBeforeRestore); ok {
		rest, n.Kind = r, archive.KindBeforeRestore
	}
	// <host>-<stamp>: the stamp is fixed-width, and the host is not empty.
	if len(rest) < len(nameStamp)+2 || rest[len(rest)-len(nameStamp)-1] != '-' {
		return Name{}, false
	}
	at, err := time.Parse(nameStamp, rest[len(rest)-len(nameStamp):])
	if err != nil {
		return Name{}, false
	}
	n.Host = rest[:len(rest)-len(nameStamp)-1]
	n.At = at.UTC()
	return n, true
}

// plausibleVersion accepts what a release version looks like in a file name:
// it starts with a digit and holds only letters, digits, dots, dashes and
// plus signs. The updater writes only parsed versions; this keeps any other
// pre-update-v* file out of the list without importing the updater's parser.
func plausibleVersion(v string) bool {
	if v == "" || v[0] < '0' || v[0] > '9' {
		return false
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-', r == '+':
		default:
			return false
		}
	}
	return true
}

// partialPrefix opens the name of a backup still being written: a dot file in
// the destination, carrying the install's id so the startup sweep removes only
// this install's leftovers from a folder several installs share.
const partialPrefix = ".seamless-partial-"

// partialName is the temp file one job writes before the rename.
func partialName(installID, jobID string) string {
	return fmt.Sprintf("%s%s-%s%s", partialPrefix, installID, jobID, ".tmp")
}

// isOwnPartial reports whether name is a temp file a job of installID left.
func isOwnPartial(name, installID string) bool {
	if installID == "" {
		return false
	}
	rest, ok := strings.CutPrefix(name, partialPrefix+installID+"-")
	return ok && strings.HasSuffix(rest, ".tmp")
}

// stagingPrefix opens the name of a job's snapshot staging directory under
// LocalDir. One daemon serves a data dir (seamlessd.lock), so every one there
// is this install's. The updater's own staging (.export-*) is never touched.
const stagingPrefix = ".staging-"
