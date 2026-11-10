//go:build linux

package access

// loginDefsPath is where shadow-utils keeps UID_MIN.
const loginDefsPath = "/etc/login.defs"

// SystemAccount reports whether uid is a system account on this machine, with
// the rule it applied, for the end of a refusal: on Linux, below the UID_MIN of
// /etc/login.defs (1000 when the file or the key is missing or unreadable).
// Only root can create one, so no ordinary account (a restricted account's
// included) can serve a socket as one. Root answers true: every caller that
// must not trust it refuses uid 0 on its own first.
func SystemAccount(uid uint32) (bool, string) { return linuxSystemAccount(uid, loginDefsPath) }
