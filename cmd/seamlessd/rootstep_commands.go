package main

// The commands setup prints for root, or for the relay's or the daemon's
// account, for the owner to read and paste (relay_install.go, setup_retire.go,
// setup_address.go). Setup runs none of them, so what it prints must hold even
// when a process of the owner's has rewritten every file the owner can write:
//
//   - Root never opens a path the owner can write. The owner's shell opens it
//     and root only writes what it reads (sudo tee <staged> < <path>): a
//     symlink to a file only root may read then fails in the owner's shell,
//     before root runs at all.
//   - A copy is checked before anything else touches it: staged under root's
//     own directory, hashed there, and only then made root's and moved into
//     place, all in one AND-list, so a failed check stops the block.
//   - A staged copy is root's alone until it passes (rootstepCopyCommands):
//     what the owner's shell hands root may be any file the owner can read,
//     and a TLS key is secret, so root makes the file empty and 0600 before
//     tee fills it, and tee keeps the mode of the file it opens.
//   - A file whose content is public (the Server's certificate) is printed in
//     the step itself, as a here-document root writes from: no file of the
//     owner's is read for it.
//   - Every tool is named by its absolute path. The owner's shell finds a bare
//     name on the owner's PATH, and so does sudo wherever sudoers sets no
//     secure_path (macOS sets none), and that PATH can lead through a
//     directory the owner's processes write, ~/.local/bin among them.
//   - Every value is quoted for the shell (posixShellArg), whatever checked it
//     before.

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/devices"
)

// rootstepTools is where each tool a printed command runs lives, on the
// computer it is printed for.
type rootstepTools struct {
	sudo, env, id, install, tee, chmod, chown, mv string
	// sum is the SHA-256 check, which reads its list from stdin with -c.
	sum string
	// macOS's account and service tools.
	dscl, launchctl string
	// Linux's. /bin/systemctl holds on a Debian or Ubuntu whose /bin is not
	// merged into /usr/bin, and wherever it is.
	useradd, groupadd, usermod, systemctl string
	// tailscale is the tailnet's command where its own packages put it,
	// root's: the macOS app's, Linux's /usr/bin. Never a package manager's
	// prefix the owner can write (Homebrew's), which root would run.
	tailscale string
}

// rootstepToolsFor is goos's tools: macOS, else Linux.
func rootstepToolsFor(goos string) rootstepTools {
	t := rootstepTools{sudo: "/usr/bin/sudo", env: "/usr/bin/env", id: "/usr/bin/id", install: "/usr/bin/install",
		tee: "/usr/bin/tee", chmod: "/bin/chmod", mv: "/bin/mv"}
	if goos == "darwin" {
		t.chown, t.sum = "/usr/sbin/chown", "/usr/bin/shasum -a 256"
		t.dscl, t.launchctl = "/usr/bin/dscl", "/bin/launchctl"
		t.tailscale = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		return t
	}
	t.chown, t.sum = "/bin/chown", "/usr/bin/sha256sum"
	t.useradd, t.groupadd, t.usermod, t.systemctl = "/usr/sbin/useradd", "/usr/sbin/groupadd", "/usr/sbin/usermod", "/bin/systemctl"
	t.tailscale = "/usr/bin/tailscale"
	return t
}

// asRoot is a command root runs: sudo, the tool, then its words.
func (t rootstepTools) asRoot(tool string, words ...string) string {
	return strings.Join(append([]string{t.sudo, tool}, words...), " ")
}

// rootstepChain is a block's commands as one AND-list to paste whole: each but
// the last ends its line with &&, so the first that fails stops the rest. A
// command with a here-document (rootstepHeredoc) carries its && on its first
// line, the one that opens the document.
func rootstepChain(cmds []string) string {
	var b strings.Builder
	for i, c := range cmds {
		head, body, doc := strings.Cut(c, "\n")
		b.WriteString(head)
		if i < len(cmds)-1 {
			b.WriteString(" &&")
		}
		b.WriteString("\n")
		if doc {
			b.WriteString(body + "\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// rootstepHeredoc is cmd reading body from a quoted here-document, whose
// delimiter is no line of body.
func rootstepHeredoc(cmd, body string) string {
	body = strings.TrimSuffix(body, "\n")
	lines := strings.Split(body, "\n")
	delim := "EOF"
	for n := 1; slices.Contains(lines, delim); n++ {
		delim = "EOF" + strconv.Itoa(n)
	}
	return cmd + " <<'" + delim + "'\n" + body + "\n" + delim
}

// rootstepCopy is a file root copies from one of the owner's: source, which
// the owner's shell opens, whose bytes setup read and hashed (sha256); staged,
// where root's copy is made and checked, beside dest in a directory only root
// can make a file in; and dest, where it goes as owner:group with mode.
type rootstepCopy struct {
	source, sha256     string
	staged, dest       string
	owner, group, mode string
}

// rootstepCopyCommands copies c as root, opening no file of the owner's:
// root makes the staged copy empty and its own alone (0600), so nothing root
// writes there is readable by another account before it is checked, whatever
// sudo's umask; the owner's shell opens the source and root only writes what
// it reads (tee opens the file that is there, keeping its mode); the staged
// copy is checked against the hash setup took, before anything else touches
// it; and only then is it made owner:group, given its mode, and moved into
// place in one rename.
func rootstepCopyCommands(t rootstepTools, c rootstepCopy) []string {
	staged := posixShellArg(c.staged)
	return []string{
		t.asRoot(t.install, "-m", "0600", "/dev/null", staged),
		t.asRoot(t.tee, staged, ">/dev/null", "<", posixShellArg(c.source)),
		// An empty hash fails the check rather than skipping it.
		"echo " + posixShellArg(c.sha256+"  "+c.staged) + " | " + t.asRoot(t.sum, "-c"),
		t.asRoot(t.chown, posixShellArg(c.owner+":"+c.group), staged),
		t.asRoot(t.chmod, posixShellArg(c.mode), staged),
		t.asRoot(t.mv, "-f", staged, posixShellArg(c.dest)),
	}
}

// rootstepPinCommands pins acct as the daemon's account on this computer
// (access.DaemonAccountPath). Its uid is read when the block runs (an account
// useradd makes has none before), and an account that is not there stops the
// block instead of pinning no uid.
func rootstepPinCommands(t rootstepTools, acct string) []string {
	q := posixShellArg(acct)
	return []string{
		"seamless_uid=$(" + t.id + " -u " + q + ")",
		`printf '%s %s\n' ` + q + ` "$seamless_uid" | ` + t.asRoot(t.tee, access.DaemonAccountPath, ">/dev/null"),
		t.asRoot(t.chmod, "0644", access.DaemonAccountPath),
	}
}

// rootstepMaxPEM bounds what is read of a certificate setup kept: one
// certificate is a few kilobytes.
const rootstepMaxPEM = 64 << 10

// rootstepReadFile reads a file setup kept, as the owner: a regular file of at
// most rootstepMaxPEM bytes.
func rootstepReadFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, rootstepMaxPEM+1))
	if err != nil {
		return nil, err
	}
	if len(data) > rootstepMaxPEM {
		return nil, fmt.Errorf("%s is larger than a certificate", path)
	}
	return data, nil
}

// rootstepCAPEM is the certificate kept at path (setupCompareCert's copy of the
// one the owner compared), read as the owner, for the root step to print: one
// certificate whose fingerprint is fingerprint, re-encoded, so nothing else the
// file holds -- a line that would end the here-document early among it --
// reaches the step.
func rootstepCAPEM(read func(string) ([]byte, error), path, fingerprint string) (string, error) {
	data, err := read(path)
	if err != nil {
		return "", err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("it is not one PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", fmt.Errorf("it does not parse: %w", err)
	}
	if got := devices.CertFingerprint(block.Bytes); !clientSameFingerprint(fingerprint, got) {
		return "", fmt.Errorf("its fingerprint is %s, not the %s compared", got, fingerprint)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})), nil
}

// rootstepSHA256 reports whether s is a SHA-256 as clientExeSHA256 writes it:
// 64 lower-case hex digits.
func rootstepSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
