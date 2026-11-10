//go:build darwin

package access

// SystemAccount reports whether uid is a system account on this machine, with
// the rule it applied, for the end of a refusal: on macOS, below uid 500. Only
// root can create one, so no ordinary account (a restricted account's
// included) can serve a socket as one. Root answers true: every caller that
// must not trust it refuses uid 0 on its own first.
func SystemAccount(uid uint32) (bool, string) { return darwinSystemAccount(uid) }
