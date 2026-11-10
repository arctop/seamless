//go:build !darwin && !linux

package access

// SystemAccount answers false here: this package knows no system-account range
// on this platform, and peer_auth, whose daemon it judges, runs on macOS and
// Linux only (serve refuses it here before any account is checked).
func SystemAccount(uint32) (bool, string) {
	return false, "this platform has no system-account range Seamless knows (peer_auth runs on macOS and Linux only)"
}
