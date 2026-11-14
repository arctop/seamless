package relay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/access"
)

// The relay's preflight: the rules serve's peer_auth preflight
// (cmd/seamlessd filesystemProblems) holds the daemon to, applied to the
// relay's own files. The uid and process chain the relay reports are believed
// only because no process of the owner's can make it say something else, so
// it refuses to serve unless:
//
//   - it runs as the account root pinned as the daemon's
//     (access.DaemonAccount), or, with no pin, as a system account
//     (access.SystemAccount), and never as root: seam, its hooks, the bridge
//     and the Touch ID helper refuse a socket any other account serves;
//   - its data directory is its own and 0700, so its key is readable by no
//     other account;
//   - the socket's directory is writable by no one but it or root, so no one
//     can unlink the socket and listen in its place;
//   - the binary it runs, its pairing and its CA copy belong to it or root and
//     are writable by no one else;
//   - every directory above all of them belongs to root or it and is writable
//     by no other account (unless sticky): an account that owns a directory
//     anywhere above can rename what is below it away and replace it.

// FileFacts is what the checks need of one path, read without following a
// final symlink.
type FileFacts struct {
	UID     uint32
	GID     uint32
	Mode    fs.FileMode
	IsDir   bool
	Symlink bool
	Socket  bool
}

// Env is what the preflight reads from this machine. HostEnv is the real one;
// a test plays the relay's account and points the root-owned layout's checks
// at temp dirs by replacing the fields.
type Env struct {
	// EUID is the account the relay runs as.
	EUID int
	// Exe is the binary's path; nil skips the binary's check (doctor, when it
	// runs as another account than the relay's, cannot tell which binary the
	// relay runs).
	Exe func() (string, error)
	// Inspect reports a path's owner and mode without following a final
	// symlink.
	Inspect func(string) (FileFacts, error)
	// DaemonAccount reads root's pin of the daemon's account; nil is
	// access.DaemonAccount.
	DaemonAccount func() (access.DaemonPin, bool, error)
	// SystemAccount judges an account no pin names; nil is
	// access.SystemAccount.
	SystemAccount func(uint32) (bool, string)
}

// HostEnv is this process on this machine.
func HostEnv() Env {
	return Env{
		EUID:          os.Geteuid(),
		Exe:           os.Executable,
		Inspect:       inspectPath,
		DaemonAccount: access.DaemonAccount,
		SystemAccount: access.SystemAccount,
	}
}

func (e Env) pin() (access.DaemonPin, bool, error) {
	if e.DaemonAccount == nil {
		return access.DaemonAccount()
	}
	return e.DaemonAccount()
}

func (e Env) system(uid uint32) (bool, string) {
	if e.SystemAccount == nil {
		return access.SystemAccount(uid)
	}
	return e.SystemAccount(uid)
}

// Layout is where one relay keeps its files.
type Layout struct {
	// DataDir holds the pairing and the key (DefaultDataDir).
	DataDir string
	// Socket is the socket it serves (DefaultSocketPath).
	Socket string
}

// DefaultLayout is the root-owned layout.
func DefaultLayout() Layout { return Layout{DataDir: DefaultDataDir, Socket: DefaultSocketPath} }

// guideSection is where the peer_auth guide creates and pins the account the
// relay runs as, the same way as the daemon's.
const guideSection = `"Deploy it: the daemon runs as its own account"`

// AccountProblems lists every reason the account the relay runs as is not one
// the agents' clients would trust to answer the socket.
func (e Env) AccountProblems() []string {
	var out []string
	pin, pinned, pinErr := e.pin()
	switch {
	case e.EUID == 0:
		out = append(out, "the relay runs as root: run it as its own unprivileged account (a LaunchDaemon with UserName, "+
			"or a systemd unit with User=)")
	case e.EUID < 0:
		out = append(out, "this platform names no account the relay runs as")
	case pinErr != nil:
		// Below, whoever runs this.
	default:
		ok, rule := access.DaemonUID(uint32(e.EUID), pin, pinned, e.system)
		switch {
		case ok:
		case pinned:
			out = append(out, fmt.Sprintf("the relay runs as uid %d, but %s, and seam, its hooks, its bridge and the Touch ID helper "+
				"trust no other account answering the socket: run the relay as %s", e.EUID, rule, pin.Name))
		default:
			out = append(out, fmt.Sprintf("the relay runs as uid %d, which is not a system account (%s), and with no pin at %s, seam, "+
				"its hooks, its bridge and the Touch ID helper refuse a socket another account serves: run it as a system account, "+
				"created and pinned as the peer_auth guide does under %s", e.EUID, rule, access.DaemonAccountPath, guideSection))
		}
	}
	if pinErr != nil {
		out = append(out, fmt.Sprintf("%v -- no socket is trusted while it stands: have root rewrite it as the peer_auth guide "+
			"does under %s", pinErr, guideSection))
	}
	return out
}

// Problems lists every reason the relay would not serve with this layout:
// the account (AccountProblems), the data directory (created 0700 when missing
// and create is set, once the account and its parents have passed), the
// socket's directory, the binary (unless Exe is nil), and the pairing files in
// the data directory.
func (e Env) Problems(l Layout, create bool) []string {
	out := e.AccountProblems()
	if !filepath.IsAbs(l.DataDir) {
		return append(out, fmt.Sprintf("the data directory %q is not an absolute path", l.DataDir))
	}
	if !filepath.IsAbs(l.Socket) {
		return append(out, fmt.Sprintf("the socket %q is not an absolute path", l.Socket))
	}
	if within(l.Socket, l.DataDir) {
		out = append(out, fmt.Sprintf("the socket %s is inside the data directory %s, which is 0700: no caller could reach it -- "+
			"keep it in a directory of its own (such as /opt/seamless/run)", l.Socket, l.DataDir))
	}
	dirOK := true
	if p := e.privateDirProblem(l.DataDir, create && len(out) == 0); p != "" {
		out = append(out, p)
		dirOK = false
	}
	if p := e.socketDirProblem(filepath.Dir(l.Socket)); p != "" {
		out = append(out, p)
	}
	if e.Exe != nil {
		if exe, err := e.Exe(); err != nil {
			out = append(out, "cannot locate this binary to check who may replace it: "+err.Error())
		} else if p := e.trustedFileProblem("the seamlessd binary", exe); p != "" {
			out = append(out, p)
		}
	}
	if dirOK {
		out = append(out, e.pairingFileProblems(l.DataDir)...)
	}
	return out
}

// PairProblems lists every reason a pairing must not be made here: the
// account (AccountProblems), and the data directory the key goes into
// (created 0700 when missing, once its parents have passed), with the pairing
// files already in it.
func (e Env) PairProblems(dataDir string) []string {
	out := e.AccountProblems()
	if !filepath.IsAbs(dataDir) {
		return append(out, fmt.Sprintf("the data directory %q is not an absolute path", dataDir))
	}
	// Made only for an account that may run the relay: one made by another
	// (root, a person's) would belong to it, and the relay refuse it.
	if p := e.privateDirProblem(dataDir, len(out) == 0); p != "" {
		return append(out, p)
	}
	return append(out, e.pairingFileProblems(dataDir)...)
}

// pairingFileProblems checks the files the relay keeps in its data directory:
// regular files of its own, readable and writable by it alone. A directory
// another account cannot list (doctor, run as one) is left unchecked.
func (e Env) pairingFileProblems(dataDir string) []string {
	names := []string{StateName}
	if cas, err := caCopies(dataDir); err == nil {
		names = append(names, cas...)
	}
	var out []string
	for _, n := range names {
		path := filepath.Join(dataDir, n)
		f, err := e.Inspect(path)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			// Absent, or another account's look: the relay checks it itself.
		case err != nil:
			out = append(out, fmt.Sprintf("%s: %v", path, err))
		case f.Symlink || !f.Mode.IsRegular():
			out = append(out, fmt.Sprintf("%s is not a regular file: the relay writes its pairing itself", path))
		case int(f.UID) != e.EUID:
			out = append(out, fmt.Sprintf("%s is owned by uid %d, not the relay's account (uid %d): pair again as the relay's account", path, f.UID, e.EUID))
		case f.Mode.Perm()&0o077 != 0:
			out = append(out, fmt.Sprintf("%s is mode %04o: it holds the relay's private key and must be readable by the relay's account "+
				"alone -- chmod 600 %s", path, f.Mode.Perm(), path))
		}
	}
	return out
}

// privateDirProblem checks the data directory: the relay's own, no group or
// world bits at all, under directories no other account can rewrite. create
// makes a missing one 0700 once its parent has passed.
func (e Env) privateDirProblem(dir string, create bool) string {
	const what = "the relay's data directory"
	if p := e.ancestorProblem(what, dir); p != "" {
		return p
	}
	facts, err := e.Inspect(dir)
	if errors.Is(err, fs.ErrNotExist) && create {
		if mkErr := os.Mkdir(dir, DataDirMode); mkErr != nil {
			return fmt.Sprintf("%s %s does not exist and could not be created: %v", what, dir, mkErr)
		}
		facts, err = e.Inspect(dir)
	}
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, dir, err)
	}
	switch {
	case facts.Symlink:
		return fmt.Sprintf("%s %s is a symlink: name the real directory", what, dir)
	case !facts.IsDir:
		return fmt.Sprintf("%s %s is not a directory", what, dir)
	case int(facts.UID) != e.EUID:
		return fmt.Sprintf("%s %s is owned by uid %d, not the relay's account (uid %d): chown it to the relay's account", what, dir, facts.UID, e.EUID)
	case facts.Mode.Perm()&0o077 != 0:
		return fmt.Sprintf("%s %s is mode %04o: it must be 0700 (no group or world access) -- chmod 700 %s", what, dir, facts.Mode.Perm(), dir)
	}
	return ""
}

// socketDirProblem checks the socket's directory: searchable by the accounts
// that connect, writable by no one but the relay's account or root, since
// whoever can write it can unlink the socket and listen in its place.
func (e Env) socketDirProblem(dir string) string {
	const what = "the socket's directory"
	if p := e.ancestorProblem(what, dir); p != "" {
		return p
	}
	facts, err := e.Inspect(dir)
	if err != nil {
		return fmt.Sprintf("%s %s: %v -- create it (owned by the relay's account, mode 0755)", what, dir, err)
	}
	switch {
	case facts.Symlink:
		return fmt.Sprintf("%s %s is a symlink: name the real directory", what, dir)
	case !facts.IsDir:
		return fmt.Sprintf("%s %s is not a directory", what, dir)
	case facts.UID != 0 && int(facts.UID) != e.EUID:
		return fmt.Sprintf("%s %s is owned by uid %d: it must belong to the relay's account or root", what, dir, facts.UID)
	case facts.Mode.Perm()&0o022 != 0:
		return fmt.Sprintf("%s %s is mode %04o: it must not be group- or world-writable (0755 lets callers reach the socket "+
			"without letting them replace it)", what, dir, facts.Mode.Perm())
	}
	return ""
}

// trustedFileProblem checks a file the relay trusts: it must belong to root
// or the relay's account and be writable by no one else, through directories
// no one else can rewrite.
func (e Env) trustedFileProblem(what, path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, path, err)
	}
	if p := e.ancestorProblem(what, real); p != "" {
		return p
	}
	// A symlink's own directory decides what the link points at next start.
	if real != filepath.Clean(path) {
		if p := e.ancestorProblem(what, path); p != "" {
			return p
		}
	}
	facts, err := e.Inspect(real)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, real, err)
	}
	switch {
	case facts.UID != 0 && int(facts.UID) != e.EUID:
		return fmt.Sprintf("%s %s is owned by uid %d: it must belong to root or the relay's account, or that account can make "+
			"the relay say what it likes", what, real, facts.UID)
	case facts.Mode.Perm()&0o022 != 0:
		return fmt.Sprintf("%s %s is mode %04o: it must not be group- or world-writable", what, real, facts.Mode.Perm())
	}
	return ""
}

// ancestorProblem walks every directory above path (symlinks resolved first)
// and refuses one another account could rewrite.
func (e Env) ancestorProblem(what, path string) string {
	parent := filepath.Dir(filepath.Clean(path))
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, parent, err)
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		facts, err := e.Inspect(dir)
		if err != nil {
			return fmt.Sprintf("%s: directory %s: %v", what, dir, err)
		}
		sticky := facts.Mode&fs.ModeSticky != 0
		switch {
		case facts.UID != 0 && int(facts.UID) != e.EUID:
			return fmt.Sprintf("%s: directory %s above it is owned by uid %d, who could rename it away and replace it -- keep the "+
				"relay's files under directories owned by root or the relay's account (outside any person's home)", what, dir, facts.UID)
		case facts.Mode.Perm()&0o022 != 0 && !sticky:
			return fmt.Sprintf("%s: directory %s above it is mode %04o, writable by other accounts", what, dir, facts.Mode.Perm())
		}
		if next := filepath.Dir(dir); next == dir {
			return ""
		}
	}
}

// within reports whether path is dir or lies inside it, lexically.
func within(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
