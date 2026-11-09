package main

// seam checks who answers before it believes what it is told.
//
// Under peer_auth, seam approve and seam touchid enroll build a sudo command
// line from what the daemon says: the account, the program and the config it
// names, and the facts and digest the owner approves. Which daemon that is
// comes from server_url in the owner's own config file, and any agent running
// as the owner can edit that file. So a process of the agent's could serve a
// socket of its own, show the owner forged facts and choose what sudo runs.
//
// What the agent cannot do is serve a socket as another uid: the kernel records
// the peer's uid when the connection is made, and nothing the peer sends
// changes it. So before anything a daemon says is shown as fact or becomes an
// argument, the kernel is asked who answered (dialSocketDaemon), and the answer
// must be neither the caller's own uid nor root, and must be the account the
// daemon names for itself (verifiedDaemon.accountIsPeer).
//
// The first connection is only for the check and is closed. A later connection
// is a new dial of a path the agent may control, and a symlink flipped between
// the two would pass the check against the real daemon and then read the
// planted one. So the client handed back checks every connection it opens, and
// what is read through it came from a process the kernel named.
//
// A TCP server_url carries no peer identity, so it is refused for anything that
// builds a sudo command line. The seamlessd path, the config path, sudo and env
// are deliberately not checked further: once the peer is verified they are the
// real daemon's words, and an agent able to poison PATH can equally replace
// seam.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

var (
	// errServerNotSocket is a server_url that is not a Unix socket: nothing says
	// who answered it.
	errServerNotSocket = errors.New("server_url is not the peer_auth socket, so nothing says who answered: " +
		"point server_url at the socket (unix:///path), or approve on the console's passkey page")
	// errNoDaemonDialer is a test env that set no e.dialDaemon.
	errNoDaemonDialer = errors.New("no daemon dialer in this environment")
	// errNoAccountLookup is a test env that set no e.lookupAccount.
	errNoAccountLookup = errors.New("no account lookup in this environment")
	// errPeerChanged is a connection answered by another uid than the check saw.
	errPeerChanged = errors.New("the daemon's socket is answered by another account than the one just checked")
)

// verifiedDaemon is a daemon the kernel has named. The zero value is "not
// verified": its nil client is cfg's own (accessCallVia).
type verifiedDaemon struct {
	cfg    config.Config
	peer   uint32
	client *http.Client
}

// verifyDaemon asks the kernel who answers at cfg's server_url and refuses a
// peer that is the caller's own uid, or root: the two a planted socket can be
// (an agent runs as the owner) and the one no daemon should be. A TCP server_url
// is errServerNotSocket, which only the advisory path survives.
func verifyDaemon(ctx context.Context, e *env, cfg config.Config) (verifiedDaemon, error) {
	if e.dialDaemon == nil {
		return verifiedDaemon{}, errNoDaemonDialer
	}
	peer, client, err := e.dialDaemon(ctx, cfg)
	if err != nil {
		return verifiedDaemon{}, err
	}
	if peer == 0 || peer == uint32(os.Geteuid()) {
		who := fmt.Sprintf("your own uid (%d)", peer)
		if peer == 0 {
			who = "root (uid 0)"
		}
		return verifiedDaemon{}, fmt.Errorf("refusing to trust the daemon at %s: it is answered by %s, not by the daemon's own account. %s",
			printable(cfg.DaemonAddress()), who, serverURLNote(cfg))
	}
	return verifiedDaemon{cfg: cfg, peer: peer, client: client}, nil
}

// terminal is terminalOf(one) for a verified daemon: the account it names must
// be the uid the kernel saw answer.
func (d verifiedDaemon) terminal(e *env, one accessOne) (terminalApprover, error) {
	t, err := terminalOf(one)
	if err != nil {
		return terminalApprover{}, err
	}
	if err := d.accountIsPeer(e, t.Account); err != nil {
		return terminalApprover{}, err
	}
	return t, nil
}

// accountIsPeer refuses an account name that is not the uid answering.
func (d verifiedDaemon) accountIsPeer(e *env, account string) error {
	uid, err := e.accountUID(account)
	if err != nil {
		return fmt.Errorf("refusing to trust the daemon at %s: it names %q as its account, which this machine cannot resolve (%w). %s",
			printable(d.cfg.DaemonAddress()), account, err, serverURLNote(d.cfg))
	}
	if uid != d.peer {
		return fmt.Errorf("refusing to trust the daemon at %s: it names %q as its account (uid %d), but uid %d answered. %s",
			printable(d.cfg.DaemonAddress()), account, uid, d.peer, serverURLNote(d.cfg))
	}
	return nil
}

// serverURLNote says where server_url came from, ending a refusal: the owner's
// config is exactly what a planted daemon needs to have changed.
func serverURLNote(cfg config.Config) string {
	where := "server_url (no config file was read: SEAMLESS_SERVER_URL?)"
	if p := cfg.SourcePath(); p != "" {
		where = "server_url in " + printable(p)
	}
	return where + " may have been changed: check it, or approve on the console's passkey page"
}

// accountUID resolves the account sudo -u would run as: a #uid as that uid (the
// form the accountName shape allows), anything else by its name.
func (e *env) accountUID(account string) (uint32, error) {
	if digits, ok := strings.CutPrefix(account, "#"); ok {
		uid, err := strconv.ParseUint(digits, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("%q is not a uid", account)
		}
		return uint32(uid), nil
	}
	if e.lookupAccount == nil {
		return 0, errNoAccountLookup
	}
	return e.lookupAccount(account)
}

// lookupAccountUID is the real e.lookupAccount.
func lookupAccountUID(name string) (uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("account %q has uid %q, which is not a number", name, u.Uid)
	}
	return uint32(uid), nil
}

// isRootAccount reports an account name sudo -u would run as root, by name or
// as any spelling of #0.
func isRootAccount(account string) bool {
	if strings.EqualFold(account, "root") {
		return true
	}
	digits, ok := strings.CutPrefix(account, "#")
	if !ok {
		return false
	}
	uid, err := strconv.ParseUint(digits, 10, 32)
	return err == nil && uid == 0
}

// errRootAccount refuses a daemon that names root as its account.
func errRootAccount(account string) error {
	return fmt.Errorf("the daemon named %q as its account, which is root: a Seamless daemon runs as its own account, "+
		"so sudo is not run for it. server_url may have been changed: check it, or approve on the console's passkey page", account)
}

// dialSocketDaemon is the real e.dialDaemon.
func dialSocketDaemon(ctx context.Context, cfg config.Config) (uint32, *http.Client, error) {
	sock := cfg.SocketPath()
	if sock == "" {
		return 0, nil, errServerNotSocket
	}
	return pinSocketPeer(ctx, sock, access.PeerUID)
}

// pinSocketPeer dials sock, asks the kernel who answered, closes that
// connection, and returns the peer's uid with a client that holds every
// connection it opens to the same uid (the package comment says why).
func pinSocketPeer(ctx context.Context, sock string, peerUID func(net.Conn) (uint32, error)) (uint32, *http.Client, error) {
	dialer := &net.Dialer{Timeout: config.DialTimeout}
	first, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return 0, nil, fmt.Errorf("console unreachable at unix:%s: %w", sock, err)
	}
	defer func() { _ = first.Close() }()
	peer, err := peerUID(first)
	if err != nil {
		return 0, nil, fmt.Errorf("find out who answers on the socket %s: %w", sock, err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "unix", sock)
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(conn)
		if err == nil && uid != peer {
			err = fmt.Errorf("%w: uid %d, where uid %d answered the check", errPeerChanged, uid, peer)
		}
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	return peer, &http.Client{Timeout: consoleTimeout, Transport: transport}, nil
}
