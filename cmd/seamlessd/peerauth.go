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
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
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
	egid    int                             // the daemon's effective gid: the one group a TLS key may be readable by
	resolve access.Resolver                 // OS account lookup
	exe     func() (string, error)          // this binary's path
	inspect func(string) (fileFacts, error) // owner and mode of a path (no symlink follow)
	// systemAccount judges the daemon's uid when no pin names its account
	// (access.SystemAccount; nil is that too, so an env that forgot it is held
	// to the real rule).
	systemAccount func(uid uint32) (bool, string)
	// daemonAccount reads root's pin of the daemon's account
	// (access.DaemonAccount; nil is that too).
	daemonAccount func() (access.DaemonPin, bool, error)
}

// fileFacts is what the permission checks need of one path.
type fileFacts struct {
	uid     uint32
	gid     uint32
	mode    os.FileMode
	isDir   bool
	symlink bool
	socket  bool
}

func defaultPeerAuthEnv() peerAuthEnv {
	return peerAuthEnv{
		euid:          os.Geteuid(),
		egid:          os.Getegid(),
		resolve:       access.LookupAccount,
		exe:           os.Executable,
		inspect:       inspectPath,
		systemAccount: access.SystemAccount,
		daemonAccount: access.DaemonAccount,
	}
}

// isSystemAccount is env.systemAccount, access.SystemAccount when unset.
func (env peerAuthEnv) isSystemAccount(uid uint32) (bool, string) {
	if env.systemAccount == nil {
		return access.SystemAccount(uid)
	}
	return env.systemAccount(uid)
}

// daemonPin is env.daemonAccount, access.DaemonAccount when unset.
func (env peerAuthEnv) daemonPin() (access.DaemonPin, bool, error) {
	if env.daemonAccount == nil {
		return access.DaemonAccount()
	}
	return env.daemonAccount()
}

// guideAccountSection is the guide's section that creates and pins the
// daemon's account.
const guideAccountSection = `"Deploy it: the daemon runs as its own account"`

// pinCommand is the one command line that pins account as the daemon's, as the
// guide writes it. account must be ValidDaemonAccountName: it is printed into
// a shell command.
func pinCommand(account string) string {
	return fmt.Sprintf(`echo "%s $(id -u %s)" | sudo tee %s >/dev/null && sudo chmod 0644 %s`,
		account, account, access.DaemonAccountPath, access.DaemonAccountPath)
}

// noPinNote is serve's warning and doctor's row when no pin names the
// daemon's account.
func (rt *peerAuthRuntime) noPinNote() string {
	return fmt.Sprintf("no pin at %s names the daemon's account, so seam approve, seam touchid enroll and the Touch ID approver "+
		"fall back to accepting any system account as the daemon's, another service's or a restricted role account included: "+
		"pin this one, as an administrator: %s", access.DaemonAccountPath, pinCommand(rt.daemonAccountName()))
}

// daemonAccountName is the name of the account this process runs as, for the
// pin command: "_seamless", the guide's, when that is the owner or root (not
// the daemon's) or the name does not resolve to one a pin may hold.
func (rt *peerAuthRuntime) daemonAccountName() string {
	if rt.env.euid > 0 && rt.env.euid != int(rt.policy.Owner().UID) && rt.env.resolve != nil {
		if a, err := rt.env.resolve(strconv.Itoa(rt.env.euid)); err == nil && a.UID == uint32(rt.env.euid) && access.ValidDaemonAccountName(a.Name) {
			return a.Name
		}
	}
	return "_seamless"
}

// peerAuthRuntime is peer_auth as serve runs it.
type peerAuthRuntime struct {
	policy *access.Policy
	cfg    config.PeerAuth
	env    peerAuthEnv
}

// preparePeerAuth builds the policy and runs the filesystem preflight, before
// serve opens or creates anything: the daemon must not run as the owner (whose
// processes would then read every file it writes) nor as root, and must run as
// the account root pinned (access.DaemonAccount) or, with no pin, as a system
// account (access.SystemAccount): seam approve and the Touch ID approver refuse
// any other daemon. A pin that is there but not believed is refused, and so is
// a restricted account that is the pinned one. The data directory, the config
// file and this binary must be beyond the owner's reach; and the socket
// directory must be one only the daemon (or root) can write.
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
	pin, pinned, pinErr := rt.env.daemonPin()
	switch {
	case rt.env.euid == int(owner.UID):
		problems = append(problems, fmt.Sprintf("the daemon runs as %s, the owner: every process of that account could read the data directory directly -- run it as its own account (a LaunchDaemon or systemd unit with a dedicated user)", owner.Name))
	case rt.env.euid == 0:
		problems = append(problems, "the daemon runs as root: run it as its own unprivileged account (a LaunchDaemon with UserName, or a systemd unit with User=)")
	case pinErr != nil:
		// Below, whoever runs this.
	default:
		ok, rule := access.DaemonUID(uint32(rt.env.euid), pin, pinned, rt.env.isSystemAccount)
		switch {
		case ok:
		case pinned:
			problems = append(problems, fmt.Sprintf("the daemon runs as uid %d, but %s, and seam approve and the Touch ID approver accept no other: "+
				"run the daemon as %s, or have root rewrite the pin as the peer_auth guide does under %s", rt.env.euid, rule, pin.Name, guideAccountSection))
		default:
			problems = append(problems, fmt.Sprintf("the daemon runs as uid %d, which is not a system account (%s), and with no pin at %s, seam approve "+
				"and the Touch ID approver refuse a daemon that is not one: run it as a system account, created and pinned as the peer_auth "+
				"guide does under %s (dscl with a uid below 500 on macOS, useradd --system on Linux)", rt.env.euid, rule, access.DaemonAccountPath, guideAccountSection))
		}
	}
	if pinErr != nil {
		problems = append(problems, fmt.Sprintf("%v -- serve and its clients trust no daemon while it stands: have root rewrite it as the peer_auth guide does under %s", pinErr, guideAccountSection))
	}
	if pinned {
		for _, a := range rt.policy.RestrictedAccounts() {
			if a.UID == pin.UID {
				problems = append(problems, fmt.Sprintf("peer_auth.restricted lists %s (uid %d), the account %s pins as the daemon's: the clients would trust "+
					"a socket it serves -- list another account, or have root correct the pin", a.Name, a.UID, pin.Path))
			}
		}
	}
	if p := rt.privateDirProblem("data_dir", cfg.DataDir, create); p != "" {
		problems = append(problems, p)
	}
	if p := rt.socketDirProblem(filepath.Dir(rt.cfg.Socket.Path)); p != "" {
		problems = append(problems, p)
	}
	if src := cfg.SourcePath(); src != "" {
		if p := rt.trustedFileProblem(configFileWhat, src); p != "" {
			problems = append(problems, p)
		}
	}
	if exe, err := rt.env.exe(); err != nil {
		problems = append(problems, "cannot locate this binary to check who may replace it: "+err.Error())
	} else if p := rt.trustedFileProblem("seamlessd binary", exe); p != "" {
		problems = append(problems, p)
	}
	// update.release_signers decides which releases the daemon's own updater
	// installs, so not even the daemon's account may change it: root's file,
	// through root's directories, and keys serve can read.
	if path := cfg.Update.ReleaseSigners; path != "" {
		fsys := signersFS{euid: rt.env.euid, inspect: rt.env.inspect, open: func(p string) (io.ReadCloser, error) { return os.Open(p) }}
		if _, err := loadReleaseSigners(path, true, fsys); err != nil {
			problems = append(problems, err.Error()+" -- have root install it: sudo install -o root -m 0644 <release_signers> "+path)
		}
	}
	// The TLS pair: whoever can read the key serves the console's own name with
	// the real certificate, on another port or address, and whoever can rewrite
	// either file decides what the daemon presents at its next start.
	if cfg.TLSEnabled() {
		for _, f := range []struct{ what, path string }{
			{tlsCertWhat, cfg.TLS.CertFile},
			{tlsKeyWhat, cfg.TLS.KeyFile},
		} {
			if p := rt.trustedFileProblem(f.what, f.path); p != "" {
				problems = append(problems, p+rt.tlsLayoutHint())
			}
		}
	}
	return problems
}

// tlsCertWhat and tlsKeyWhat are how the preflight names the TLS pair. The key
// is a secret, so like the config file it must also be unreadable by other
// accounts.
const (
	tlsCertWhat = "tls.cert_file"
	tlsKeyWhat  = "tls.key_file"
)

// tlsLayoutHint ends a TLS file's problem with where the guide keeps the pair.
func (rt *peerAuthRuntime) tlsLayoutHint() string {
	return fmt.Sprintf(" -- keep the certificate and key under /opt/seamless/etc/tls (root:%s, the key 0640) and point tls.cert_file "+
		"and tls.key_file there, as the peer_auth guide does under %s", rt.daemonAccountName(), guideMoveSection)
}

// guideMoveSection is the guide's section that moves an existing install over,
// TLS files included.
const guideMoveSection = `"Move an existing install over"`

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

// configFileWhat is how the preflight names the config file. It holds secrets,
// so like the TLS key (tlsKeyWhat) it must also be unreadable by other
// accounts.
const configFileWhat = "config file"

// trustedFileProblem checks a file the daemon trusts (its config, its own
// binary, its TLS pair): whoever can rewrite it can switch the boundary off at
// the next start, so it must belong to root or the daemon's account and be
// writable by no one else, through directories no one else can rewrite. The
// TLS key must also be readable by no account but the daemon's: world-readable
// never, group-readable only by the daemon's own group.
func (rt *peerAuthRuntime) trustedFileProblem(what, path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Sprintf("%s %s: %v", what, path, err)
	}
	if p := rt.ancestorProblem(what, real); p != "" {
		return p
	}
	// A symlink's own directory decides what the link points at next start.
	if real != filepath.Clean(path) {
		if p := rt.ancestorProblem(what, path); p != "" {
			return p
		}
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
	case what == configFileWhat && facts.mode.Perm()&0o004 != 0:
		return fmt.Sprintf("%s %s is mode %04o: every account can read it, and it holds the bearer key; make it 0640, readable by the daemon's group only", what, real, facts.mode.Perm())
	case what == tlsKeyWhat && facts.mode.Perm()&0o004 != 0:
		return fmt.Sprintf("%s %s is mode %04o: every account can read the TLS private key, and with it serve this daemon's name; make it 0640, readable by the daemon's group only", what, real, facts.mode.Perm())
	case what == tlsKeyWhat && facts.mode.Perm()&0o040 != 0 && int(facts.gid) != rt.env.egid:
		return fmt.Sprintf("%s %s is mode %04o with group %d, not the daemon's group (%d): that group's members can read the TLS private key; chgrp it to the daemon's group, or make it 0600", what, real, facts.mode.Perm(), facts.gid, rt.env.egid)
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

// projectProblems checks the config file's locks against the projects table:
// every project it force-locks must exist -- a misspelled entry leaves the
// project it meant open, which is exactly the failure peer_auth exists to
// prevent -- and must carry an isolation fence (confidential or sealed), which
// keeps it out of the family, parent and cross-project gardener surfaces the
// policy does not reach on its own. A console lock is not checked here: the
// store holds those rules at the write, and a refusal to start could only be
// cleared by hand.
func projectProblems(ctx context.Context, db *sql.DB, policy *access.Policy) ([]string, error) {
	var problems []string
	for _, slug := range policy.ForceLocked() {
		p, found, err := store.ProjectBySlug(ctx, db, slug)
		if err != nil {
			return nil, err
		}
		if !found {
			problems = append(problems, fmt.Sprintf("peer_auth names project %q, which does not exist: if that is a typo, the project it meant is NOT protected", slug))
			continue
		}
		if !p.Isolation.FencesOutbound() {
			problems = append(problems, fmt.Sprintf("project %q is locked (peer_auth) but its isolation is %s: make it confidential first (seam project isolation %s confidential --yes)", slug, isolationWord(p.Isolation), slug))
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
// an authenticated one carries. A request a relay peer_auth.relays maps
// forwarded is judged again inside the device edge (relayHandler).
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
	// chain walks the connecting process's parent chain (access.Peer.Chain),
	// per request: the tree as it is when the request runs. Nil when the
	// kernel named no process for the connection (chainErr says why).
	chain    func() ([]access.ChainHop, error)
	chainErr error
}

// peerConnContext reads the peer's uid, and the process that connected, once
// per connection, at accept, from the kernel. It never fails the connection:
// peerHandler refuses a request whose connection has no credential, and a
// connection whose process could not be read simply holds no lease.
func peerConnContext(ctx context.Context, c net.Conn) context.Context {
	uid, err := access.PeerUID(c)
	cred := peerCred{uid: uid, err: err}
	if err == nil {
		if peer, perr := access.ReadPeer(c); perr != nil {
			cred.chainErr = perr
		} else {
			cred.chain = peer.Chain
		}
	}
	return context.WithValue(ctx, peerCredKey{}, cred)
}

// peerLeases is how peerHandler opens locked projects: it loads the live
// leases of a process chain (store.ActiveAccessLeasesFor) at now. The zero
// value loads none, so every locked project stays locked.
type peerLeases struct {
	load func(ctx context.Context, processKeys []string, now time.Time) ([]access.Lease, error)
	now  func() time.Time
	// chains walks the owner's process chain even while nothing is locked:
	// read grants (isolation.requests) under peer_auth are keyed on it.
	chains bool
	// open loads every project whose isolation is open (store.OpenProjectSlugs):
	// a restricted account's reach for one request, from which the View drops
	// the locked ones. Nil loads none, so such an account reaches only the
	// global scope, read-only.
	open func(ctx context.Context) ([]string, error)
}

// storeLeases is the daemon's peerLeases: rows from the database, judged
// again by the View.
func storeLeases(db *sql.DB) peerLeases {
	return peerLeases{
		load: func(ctx context.Context, keys []string, now time.Time) ([]access.Lease, error) {
			rows, err := store.ActiveAccessLeasesFor(ctx, db, keys, now)
			if err != nil {
				return nil, err
			}
			return store.AccessLeases(rows), nil
		},
		now: time.Now,
		open: func(ctx context.Context) ([]string, error) {
			return store.OpenProjectSlugs(ctx, db)
		},
	}
}

// peerHandler admits a socket request only when the kernel named a peer the
// policy knows, and serves it with that peer's View. Everyone else gets 403
// before any route runs. For the owner, while the policy locks a project, the
// View also carries the connection's process chain and the leases live on it
// (leasedView).
func peerHandler(policy *access.Policy, leases peerLeases, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := r.Context().Value(peerCredKey{}).(peerCred)
		if !ok || cred.err != nil {
			logger.Warn("peer_auth: socket request without a peer credential", "error", cred.err)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, "forbidden: no peer credential", http.StatusForbidden)
			return
		}
		view, known := policy.PeerView(cred.uid)
		if !known {
			logger.Warn("peer_auth: refused socket peer", "uid", cred.uid)
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, fmt.Sprintf("forbidden: uid %d is not configured under peer_auth", cred.uid), http.StatusForbidden)
			return
		}
		if view.Principal().Kind == access.KindOwner && (policy.HasLocked() || leases.chains) {
			view = leasedView(r.Context(), policy, cred, leases, logger, view)
		}
		if view.Principal().Kind == access.KindRestricted {
			// A restricted account's reach is the projects open right now, judged
			// per request like a lease. A lookup that fails refuses the request
			// rather than serving it a reach it would misread as "no projects".
			var open []string
			if leases.open != nil {
				var err error
				if open, err = leases.open(r.Context()); err != nil {
					logger.Warn("peer_auth: open projects unavailable; refusing a restricted account's request", "uid", cred.uid, "error", err)
					http.Error(w, "service unavailable: the project list could not be read", http.StatusServiceUnavailable)
					return
				}
			}
			view = view.WithOpenProjects(open)
		}
		ctx := access.WithView(r.Context(), view)
		if view.Principal().Kind == access.KindOwner {
			ctx = access.WithAgentChain(ctx, sync.OnceValue(func() bool { return chainHasAgent(cred) }))
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// chainHasAgent reports whether an agent program is in the connection's process
// chain as the kernel names it. A chain that cannot be walked counts as an
// agent's: the guards that ask would rather send the owner to the console.
func chainHasAgent(cred peerCred) bool {
	if cred.chain == nil {
		return true
	}
	hops, err := cred.chain()
	if err != nil {
		return true
	}
	for _, h := range hops {
		if approval.IsAgentProgram(h.Name) {
			return true
		}
	}
	return false
}

// leasedView is the owner's socket view with the connection's process chain
// and its live leases attached. It fails closed: a connection whose process
// the kernel did not name, a chain that cannot be walked (the process that
// connected is gone), or a lease lookup that fails leaves the plain view, on
// which every locked project stays locked.
func leasedView(ctx context.Context, policy *access.Policy, cred peerCred, leases peerLeases, logger *slog.Logger, plain access.View) access.View {
	if cred.chain == nil {
		logger.Debug("peer_auth: no process for this connection; locked projects stay locked", "error", cred.chainErr)
		return plain
	}
	hops, err := cred.chain()
	if err != nil {
		logger.Warn("peer_auth: process chain unavailable; locked projects stay locked", "error", err)
		return plain
	}
	view, ok := policy.PeerChainView(cred.uid, access.Tokens(hops))
	if !ok || !view.Principal().HasProcess || leases.load == nil {
		return view
	}
	now := time.Now
	if leases.now != nil {
		now = leases.now
	}
	at := now()
	keys := make([]string, 0, len(hops))
	for _, h := range hops {
		keys = append(keys, h.Token.Key())
	}
	rows, err := leases.load(ctx, keys, at)
	if err != nil {
		logger.Warn("peer_auth: lease lookup failed; locked projects stay locked", "error", err)
		return view
	}
	return view.WithLeases(at, rows)
}

// logPolicy writes the policy into the startup log: which account is the owner,
// what is locked, and each restricted account. Slugs and account names only.
func (rt *peerAuthRuntime) logPolicy(logger *slog.Logger) {
	owner := rt.policy.Owner()
	logger.Info("peer_auth enabled", "socket", rt.cfg.Socket.Path, "owner", owner.Name, "owner_uid", owner.UID,
		"locked", strings.Join(rt.policy.OwnerLocked(), ","))
	for _, a := range rt.policy.RestrictedAccounts() {
		logger.Info("peer_auth restricted account", "user", a.Name, "uid", a.UID)
	}
	for _, name := range slices.Sorted(maps.Keys(rt.cfg.Relays)) {
		logger.Info("peer_auth relay mapped", "relay", name, "owner_uid", rt.cfg.Relays[name].OwnerUID)
		for _, a := range rt.policy.RelayRestrictedAccounts(name) {
			logger.Info("peer_auth relay restricted account", "relay", name, "user", a.Name, "uid", a.UID)
		}
	}
	switch pin, pinned, err := rt.env.daemonPin(); {
	case err != nil:
		// The preflight refused it; nothing reaches here.
	case pinned:
		logger.Info("peer_auth daemon account pinned", "pin", pin.Path, "user", pin.Name, "uid", pin.UID)
	default:
		logger.Warn("peer_auth: " + rt.noPinNote())
	}
}
