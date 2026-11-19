// Package shellcmd renders the commands Seamless prints for the owner to
// paste, or runs itself, when root or another account runs them through sudo:
// a setup's root step, the daemon account's `seamlessd approve`, a restart, a
// restore. The owner's shell finds a bare name on the owner's PATH, and so
// does sudo wherever sudoers sets no secure_path (macOS sets none); that PATH
// can lead through a directory the owner's processes write (~/.local/bin among
// them), so a planted `sudo` or `env` would run with the owner's password, or
// as the daemon's account. Such commands name sudo, and the env that sudo -u
// runs, by absolute path (Sudo, Env), and quote every value (PosixArg).
//
// Diagnostic hints the owner runs as themselves (lsof, launchctl print) stay
// bare: where those tools live differs across systems, and a wrong path breaks
// a hint that a bare name would have run (owner decision 2026-11-18, task L4).
//
// It imports nothing of Seamless, so cmd/seamlessd, cmd/seam and the console
// share one table.
package shellcmd

import "strings"

// Sudo and Env are sudo and env by absolute path: the same on macOS and on
// Linux (env's /usr/bin path is the one every POSIX system keeps).
const (
	Sudo = "/usr/bin/sudo"
	Env  = "/usr/bin/env"
)

// Tools is where each tool a printed root command runs lives, on the computer
// it is printed for.
type Tools struct {
	Sudo, Env, ID, Install, Tee, Chmod, Chown, Mv string
	// Sum is the SHA-256 check, which reads its list from stdin with -c.
	Sum string
	// macOS's account and service tools.
	Dscl, Launchctl string
	// Linux's. /bin/systemctl holds on a Debian or Ubuntu whose /bin is not
	// merged into /usr/bin, and wherever it is.
	Useradd, Groupadd, Usermod, Systemctl string
	// Tailscale is the tailnet's command where its own packages put it,
	// root's: the macOS app's, Linux's /usr/bin. Never a package manager's
	// prefix the owner can write (Homebrew's), which root would run.
	Tailscale string
}

// ToolsFor is goos's tools: macOS ("darwin"), else Linux.
func ToolsFor(goos string) Tools {
	t := Tools{Sudo: Sudo, Env: Env, ID: "/usr/bin/id", Install: "/usr/bin/install",
		Tee: "/usr/bin/tee", Chmod: "/bin/chmod", Mv: "/bin/mv"}
	if goos == "darwin" {
		t.Chown, t.Sum = "/usr/sbin/chown", "/usr/bin/shasum -a 256"
		t.Dscl, t.Launchctl = "/usr/bin/dscl", "/bin/launchctl"
		t.Tailscale = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		return t
	}
	t.Chown, t.Sum = "/bin/chown", "/usr/bin/sha256sum"
	t.Useradd, t.Groupadd, t.Usermod, t.Systemctl = "/usr/sbin/useradd", "/usr/sbin/groupadd", "/usr/sbin/usermod", "/bin/systemctl"
	t.Tailscale = "/usr/bin/tailscale"
	return t
}

// AsRoot is a command root runs: sudo, the tool, then its words, joined by
// spaces as given (quote values with PosixArg first).
func (t Tools) AsRoot(tool string, words ...string) string {
	return strings.Join(append([]string{t.Sudo, tool}, words...), " ")
}

// PosixArg quotes s for a POSIX shell: bare when it is all letters, digits and
// punctuation no shell reads specially anywhere in a word, else in single
// quotes, each embedded quote closing them, escaped, and reopening them. "="
// and "~" are not bare: zsh expands a word starting with "=", and a leading
// "~" is a home directory. The empty string is a pair of single quotes.
func PosixArg(s string) string {
	if bare(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// bare reports whether s is non-empty and all ASCII letters, digits and
// "_@%+:,./-".
func bare(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("_@%+:,./-", c) >= 0:
		default:
			return false
		}
	}
	return true
}
