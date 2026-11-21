package main

import (
	"io"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// maxAccountFileBytes bounds one local account file read by groupHoldsOnly. A
// longer one answers false, as an unreadable one does.
const maxAccountFileBytes = 8 << 20

// groupHoldsOnly reports whether no account but uid is in group gid, by the
// local account files under etc (passwd and group, the two Go's os/user reads
// without cgo): uid has an entry in passwd, no other account's primary group is
// gid, group has an entry for gid, and that entry lists no member but uid's
// own name. It is the user private group Debian's and Ubuntu's adduser make,
// and the group their default umask 002 leaves every new directory writable
// by, ~/.config included.
//
// It fails closed: a file it cannot read, a line it cannot parse, or a NIS
// compat line ("+" or "-", which pulls in accounts from elsewhere) answers
// false. An account the files do not hold (one a directory service such as LDAP
// or sssd supplies) answers false too, since its groups are not in these files.
func groupHoldsOnly(etc fs.FS, uid int, gid uint32) bool {
	passwd, ok := accountEntries(etc, "passwd", 7)
	if !ok {
		return false
	}
	group, ok := accountEntries(etc, "group", 4)
	if !ok {
		return false
	}
	var mine []string
	for _, e := range passwd {
		u, err := strconv.ParseUint(e[2], 10, 32)
		if err != nil {
			return false
		}
		g, err := strconv.ParseUint(e[3], 10, 32)
		if err != nil {
			return false
		}
		switch {
		case int(u) == uid:
			mine = append(mine, e[0])
		case uint32(g) == gid:
			return false // another account's primary group
		}
	}
	if len(mine) == 0 {
		return false
	}
	found := false
	for _, e := range group {
		g, err := strconv.ParseUint(e[2], 10, 32)
		if err != nil {
			return false
		}
		if uint32(g) != gid {
			continue
		}
		found = true
		for m := range strings.SplitSeq(e[3], ",") {
			if m = strings.TrimSpace(m); m != "" && !slices.Contains(mine, m) {
				return false
			}
		}
	}
	return found
}

// accountEntries reads one colon-separated account file under etc into its
// entries of exactly n fields, skipping blank lines and comments. ok is false
// when the file cannot be read, is over maxAccountFileBytes, or holds a line
// of another shape or a NIS compat line.
func accountEntries(etc fs.FS, name string, n int) (_ [][]string, ok bool) {
	f, err := etc.Open(name)
	if err != nil {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAccountFileBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || len(data) > maxAccountFileBytes {
		return nil, false
	}
	var entries [][]string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			return nil, false
		}
		fields := strings.Split(line, ":")
		if len(fields) != n {
			return nil, false
		}
		entries = append(entries, fields)
	}
	return entries, true
}
