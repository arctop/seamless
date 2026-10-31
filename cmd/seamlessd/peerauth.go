package main

// peer_auth at the transport edge: the policy, the preflight that refuses a
// deployment in which the boundary would not hold, the Unix-socket listener,
// and the handlers that put each request's principal into its context. The
// policy itself is internal/access; everything past these handlers reads the
// View they install and nothing else.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// errPeerAuthPreflight marks a peer_auth deployment serve refuses to run: a
// boundary that is configured but would not hold is worse than none, because
// the owner believes it is up.
var errPeerAuthPreflight = errors.New("peer_auth preflight")

// peerAuthEnv is what the preflight reads from the running process, injectable
// so tests can play a daemon account without being one.
type peerAuthEnv struct {
	euid    int                             // the daemon's effective uid
	resolve access.Resolver                 // OS account lookup
	exe     func() (string, error)          // this binary's path
	inspect func(string) (fileFacts, error) // owner and mode of a path (no symlink follow)
}

// fileFacts is what the permission checks need of one path.
type fileFacts struct {
	uid     uint32
	mode    os.FileMode
	isDir   bool
	symlink bool
	socket  bool
}

func defaultPeerAuthEnv() peerAuthEnv {
	return peerAuthEnv{
		euid:    os.Geteuid(),
		resolve: access.LookupAccount,
		exe:     os.Executable,
		inspect: inspectPath,
	}
}

// peerAuthRuntime is peer_auth as serve runs it.
type peerAuthRuntime struct {
	policy *access.Policy
	cfg    config.PeerAuth
	env    peerAuthEnv
}

// preparePeerAuth builds the policy and runs the filesystem preflight, before
// serve opens or creates anything: the daemon must not run as the owner (whose
// processes would then read every file it writes) nor as root; the data
// directory, the config file and this binary must be beyond the owner's reach;
// and the socket directory must be one only the daemon (or root) can write.
func preparePeerAuth(cfg config.Config, env peerAuthEnv) (*peerAuthRuntime, error) {
	if !access.PeerCredSupported {
		return nil, fmt.Errorf("%w: this platform has no Unix-socket peer credentials Seamless can read (macOS and Linux only)", errPeerAuthPreflight)
	}
	policy, err := access.New(cfg.PeerAuth, env.resolve)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errPeerAuthPreflight, err)
	}
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth, env: env}
	if problems := rt.filesystemProblems(cfg, true); len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", errPeerAuthPreflight, strings.Join(problems, "; "))
	}
	return rt, nil
}

// filesystemProblems lists every reason the files would not hold the boundary,
// for serve (which refuses on any) and doctor (which reports each). create lets
// serve make a missing data dir; doctor only looks.
func (rt *peerAuthRuntime) filesystemProblems(cfg config.Config, create bool) []string {
	var problems []string
	owner := rt.policy.Owner()
	switch {
	case rt.env.euid == int(owner.UID):
		problems = append(problems, fmt.Sprintf("the daemon runs as %s, the owner: every process of that account could read the data directory directly -- run it as its own account (a LaunchDaemon or systemd unit with a dedicated user)", owner.Name))
	case rt.env.euid == 0:
		problems = append(problems, "the daemon runs as root: run it as its own unprivileged account (a LaunchDaemon with UserName, or a systemd unit with User=)")
	}
	if p := rt.privateDirProblem("data_dir", cfg.DataDir, create); p != "" {
		problems = append(problems, p)
	}
	if p := rt.socketDirProblem(filepath.Dir(rt.cfg.Socket.Path)); p != "" {
		problems = append(problems, p)
	}
	if src := cfg.SourcePath(); src != "" {
		if p := rt.trustedFileProblem("config file", src); p != "" {
			problems = append(problems, p)
		}
	}
	if exe, err := rt.env.exe(); err != nil {
		problems = append(problems, "cannot locate this binary to check who may replace it: "+err.Error())
	} else if p := rt.trustedFileProblem("seamlessd binary", exe); p != "" {
		problems = append(problems, p)
	}
	return problems
}

// privateDirProblem checks a directory only the daemon may enter: owned by the
// daemon's account, no group or world bits at all, and reached through
// directories no other account can rewrite. create makes a missing directory
// (0700) once its parent has passed.
func (rt *peerAuthRuntime) privateDirProblem(key, dir string, create bool) string {
	if p := rt.ancestorProblem(key, dir); p != "" {
		return p
	}
	facts, err := rt.env.inspect(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		if mkErr := os.Mkdir(dir, 0o700); mkErr != nil {
			return fmt.Sprintf("%s %s does not exist and could not be created: %v", key, dir, mkErr)
		}
		facts, err = rt.env.inspect(dir)
	}
	if err != nil {
		return fmt.Sprintf("%s %s: %v", key, dir, err)
	}
	switch {
	case facts.symlink:
		return fmt.Sprintf("%s %s is a symlink: point the config at the real directory", key, dir)
	case !facts.isDir:
		return fmt.Sprintf("%s %s is not a directory", key, dir)
	case int(facts.uid) != rt.env.euid:
		return fmt.Sprintf("%s %s is owned by uid %d, not the daemon's account (uid %d): chown it to the daemon's account", key, dir, facts.uid, rt.env.euid)
	case facts.mode.Perm()&0o077 != 0:
		return fmt.Sprintf("%s %s is mode %04o: it must be 0700 (no group or world access) -- chmod 700 %s", key, dir, facts.mode.Perm(), dir)
	}
	return ""
}

// socketDirProblem checks the socket's directory: it may be searchable by the
// accounts that connect, but writable by nobody except the daemon (or root),
// since whoever can write it can unlink the socket and listen in its place --
// and every caller would then hand its traffic to an impostor.
func (rt *peerAuthRuntime) socketDirProblem(dir string) string {
	if p := rt.ancestorProblem("peer_auth.socket.path", dir); p != "" {
		return p
	}
	facts, err := rt.env.inspect(dir)
	if err != nil {
		return fmt.Sprintf("socket directory %s: %v -- create it (owned by the daemon's account, mode 0755)", dir, err)
	}
	switch {
	case facts.symlink:
		return fmt.Sprintf("socket directory %s is a symlink: point peer_auth.socket.path at the real directory", dir)
	case !facts.isDir:
		return fmt.Sprintf("socket directory %s is not a directory", dir)
	case facts.uid != 0 && int(facts.uid) != rt.env.euid:
		return fmt.Sprintf("socket directory %s is owned by uid %d: it must belong to the daemon's account or root", dir, facts.uid)
	case facts.mode.Perm()&0o022 != 0:
		return fmt.Sprintf("socket directory %s is mode %04o: it must not be group- or world-writable (0755 lets callers reach the socket without letting them replace it)", dir, facts.mode.Perm())
	}
	return ""
}

// trustedFileProblem checks a file the daemon trusts (its config, its own
// binary): whoever can rewrite it can switch the boundary off at the next
// start, so it must belong to root or the daemon's account and be writable by
// no one else, through directories no one else can rewrite.
func (rt *peerAuthRuntime) trustedFileProblem(what, path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, path, err)
	}
	if p := rt.ancestorProblem(what, real); p != "" {
		return p
	}
	facts, err := rt.env.inspect(real)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, real, err)
	}
	switch {
	case facts.uid != 0 && int(facts.uid) != rt.env.euid:
		return fmt.Sprintf("%s %s is owned by uid %d: it must belong to root or the daemon's account, or that account can rewrite the daemon's rules", what, real, facts.uid)
	case facts.mode.Perm()&0o022 != 0:
		return fmt.Sprintf("%s %s is mode %04o: it must not be group- or world-writable", what, real, facts.mode.Perm())
	}
	return ""
}

// ancestorProblem walks every directory above path (symlinks resolved first)
// and refuses one another account could rewrite: owned by someone other than
// root or the daemon's account, or group/world-writable without the sticky bit.
// A directory the owner account controls anywhere above the data is a
// directory it can rename the data out of and replace.
func (rt *peerAuthRuntime) ancestorProblem(key, path string) string {
	parent := filepath.Dir(filepath.Clean(path))
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", key, parent, err)
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		facts, err := rt.env.inspect(dir)
		if err != nil {
			return fmt.Sprintf("%s: directory %s: %v", key, dir, err)
		}
		sticky := facts.mode&os.ModeSticky != 0
		switch {
		case facts.uid != 0 && int(facts.uid) != rt.env.euid:
			return fmt.Sprintf("%s: directory %s above it is owned by uid %d, who could rename it away and replace it -- keep the daemon's files under directories owned by root or the daemon's account (outside any person's home)", key, dir, facts.uid)
		case facts.mode.Perm()&0o022 != 0 && !sticky:
			return fmt.Sprintf("%s: directory %s above it is mode %04o, writable by other accounts", key, dir, facts.mode.Perm())
		}
		if next := filepath.Dir(dir); next == dir {
			return ""
		}
	}
}

// projectProblems checks the policy against the projects table: every project
// it names must exist -- a misspelled seal leaves the project it meant open,
// which is exactly the failure peer_auth exists to prevent -- and every sealed
// project must carry an isolation fence (confidential or sealed), which keeps
// it out of the family, parent and cross-project gardener surfaces the policy
// does not reach on its own.
func projectProblems(ctx context.Context, db *sql.DB, policy *access.Policy) ([]string, error) {
	var problems []string
	sealed := policy.SealedProjects()
	for _, slug := range policy.Projects() {
		p, found, err := store.ProjectBySlug(ctx, db, slug)
		if err != nil {
			return nil, err
		}
		if !found {
			problems = append(problems, fmt.Sprintf("peer_auth names project %q, which does not exist: if that is a typo, the project it meant is NOT protected", slug))
			continue
		}
		if slices.Contains(sealed, slug) && !p.Isolation.FencesOutbound() {
			problems = append(problems, fmt.Sprintf("project %q is sealed to an OS account but its isolation is %q: make it confidential first (seam project isolation %s confidential --yes)", slug, isolationWord(p.Isolation), slug))
		}
	}
	return problems, nil
}

func isolationWord(i core.Isolation) string {
	if i == "" {
		return string(core.IsolationOpen)
	}
	return string(i)
}

// listenSocket binds the peer_auth socket: a stale socket of the daemon's own
// is removed first (the data-dir lock already proved no other daemon of this
// install is running), anything else at the path is refused, and the file gets
// the configured mode and group.
func (rt *peerAuthRuntime) listenSocket() (net.Listener, error) {
	path := rt.cfg.Socket.Path
	if facts, err := rt.env.inspect(path); err == nil {
		if !facts.socket || int(facts.uid) != rt.env.euid {
			return nil, fmt.Errorf("%w: %s exists and is not this daemon's socket: remove it by hand if it is stale", errPeerAuthPreflight, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("seamlessd.serve: remove stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("seamlessd.serve: socket %s: %w", path, err)
	}
	mode, err := rt.cfg.Socket.SocketFileMode()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("seamlessd.serve: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, os.FileMode(mode)); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("seamlessd.serve: chmod socket %s: %w", path, err)
	}
	if g := strings.TrimSpace(rt.cfg.Socket.Group); g != "" {
		gid, err := lookupGID(g)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("seamlessd.serve: peer_auth.socket.group: %w", err)
		}
		if err := os.Chown(path, -1, gid); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("seamlessd.serve: chgrp socket %s to %s (the daemon's account must be a member): %w", path, g, err)
		}
	}
	return ln, nil
}

func lookupGID(name string) (int, error) {
	var (
		g   *user.Group
		err error
	)
	if _, nerr := strconv.Atoi(name); nerr == nil {
		g, err = user.LookupGroupId(name)
	} else {
		g, err = user.LookupGroup(name)
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// ownerHandler marks every TCP request as the owner's. The bearer key still
// decides whether a request is authenticated at all; this only says whose reach
// an authenticated one carries.
func ownerHandler(policy *access.Policy, next http.Handler) http.Handler {
	view := policy.OwnerView()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), view)))
	})
}

// peerCredKey carries the kernel's answer for one socket connection from
// ConnContext to peerHandler.
type peerCredKey struct{}

type peerCred struct {
	uid uint32
	err error
}

// peerConnContext reads the peer's uid once per connection, at accept, from the
// kernel. It never fails the connection: peerHandler refuses a request whose
// connection has no credential.
func peerConnContext(ctx context.Context, c net.Conn) context.Context {
	uid, err := access.PeerUID(c)
	return context.WithValue(ctx, peerCredKey{}, peerCred{uid: uid, err: err})
}

// peerHandler admits a socket request only when the kernel named a peer the
// policy knows, and serves it with that peer's View. Everyone else gets 403
// before any route runs.
func peerHandler(policy *access.Policy, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := r.Context().Value(peerCredKey{}).(peerCred)
		if !ok || cred.err != nil {
			logger.Warn("peer_auth: socket request without a peer credential", "error", cred.err)
			http.Error(w, "forbidden: no peer credential", http.StatusForbidden)
			return
		}
		view, known := policy.PeerView(cred.uid)
		if !known {
			logger.Warn("peer_auth: refused socket peer", "uid", cred.uid)
			http.Error(w, fmt.Sprintf("forbidden: uid %d is not configured under peer_auth", cred.uid), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), view)))
	})
}

// logPolicy writes the policy into the startup log: which account is the owner,
// and each configured account's reach. Slugs and account names only.
func (rt *peerAuthRuntime) logPolicy(logger *slog.Logger) {
	owner := rt.policy.Owner()
	logger.Info("peer_auth enabled", "socket", rt.cfg.Socket.Path, "owner", owner.Name, "owner_uid", owner.UID,
		"sealed", strings.Join(rt.policy.SealedProjects(), ","))
	for _, a := range rt.policy.Accounts() {
		logger.Info("peer_auth account", "user", a.Account.Name, "uid", a.Account.UID,
			"sealed", strings.Join(a.Sealed, ","), "projects", strings.Join(a.Projects, ","), "home", a.Home)
	}
}
