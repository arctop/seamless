package access

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// The daemon's account is a system account (the owner's decision of
// 2026-11-10). Under peer_auth the daemon runs as an account of its own, which
// the guide creates as a system account, and a restricted local account is an
// ordinary login account. A client that judged the daemon only by refusing its
// own uid and root would believe a restricted account serving a socket of its
// own, once the owner's config named that socket. Only root can create a
// system account, so seam approve, seam touchid enroll and the Touch ID
// approver refuse a daemon whose uid is not one, and serve refuses to start as
// one, so an install the clients would refuse fails at start instead.
//
// SystemAccount (one file per platform) answers for this machine. The rules
// themselves are here, untagged, so each platform's is tested on every one.

// darwinSystemUIDLimit is where macOS stops: its system (role) accounts are
// below 500, and the accounts it creates for people start at 501.
const darwinSystemUIDLimit = 500

// defaultUIDMin is Linux's first ordinary uid when login.defs names none: the
// value shadow-utils itself defaults to.
const defaultUIDMin = 1000

// darwinSystemAccount is SystemAccount's rule on macOS.
func darwinSystemAccount(uid uint32) (bool, string) {
	return uid < darwinSystemUIDLimit, fmt.Sprintf("macOS system accounts are below uid %d", darwinSystemUIDLimit)
}

// linuxSystemAccount is SystemAccount's rule on Linux, with UID_MIN read from
// the login.defs at path: useradd --system allocates below it, and every
// ordinary account starts at or above it.
func linuxSystemAccount(uid uint32, path string) (bool, string) {
	limit, from := uidMinAt(path)
	return uid < limit, fmt.Sprintf("system accounts here are below uid %d, %s", limit, from)
}

// uidMinAt is UID_MIN from the login.defs at path, and where the value came
// from. A file that cannot be read gives the default.
func uidMinAt(path string) (uint32, string) {
	f, err := os.Open(path)
	if err != nil {
		return defaultUIDMin, fmt.Sprintf("the default, since %s cannot be read", path)
	}
	defer func() { _ = f.Close() }()
	return parseUIDMin(f, path)
}

// parseUIDMin reads UID_MIN from a login.defs, named path in the answer. Like
// shadow-utils, a later line wins over an earlier one and "#" starts a comment
// line. A file that cannot be read through, names no UID_MIN or names one that
// is not a decimal uid gives the default.
func parseUIDMin(r io.Reader, path string) (uint32, string) {
	var (
		value string
		found bool
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "UID_MIN" {
			value, found = fields[1], true
		}
	}
	switch {
	case sc.Err() != nil:
		return defaultUIDMin, fmt.Sprintf("the default, since %s cannot be read", path)
	case !found:
		return defaultUIDMin, fmt.Sprintf("the default, since %s sets no UID_MIN", path)
	}
	n, err := strconv.ParseUint(strings.Trim(value, `"`), 10, 32)
	if err != nil {
		return defaultUIDMin, fmt.Sprintf("the default, since %s sets UID_MIN %q, which is not a uid", path, value)
	}
	return uint32(n), "the UID_MIN in " + path
}
