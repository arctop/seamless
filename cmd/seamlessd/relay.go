//go:build darwin || linux

package main

// seamlessd relay: the relay daemon on a client computer (plan
// networked-relay; internal/relay holds what it does). `relay serve` serves
// the socket agents dial beside a peer_auth daemon and forwards every request
// to the server it paired with; `relay pair` pairs it. Both run as the relay's
// own account, never as root or a person's: the preflight refuses any other
// (relay.Env.Problems). Installing the account, the layout and the service is
// the client setup's root step, not this command's.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/relay"
)

// errRelayPreflight marks a layout or account the relay refuses to run with,
// as serve's errPeerAuthPreflight does for the daemon.
var errRelayPreflight = errors.New("relay preflight")

// relayUsage is `seamlessd relay`'s help.
const relayUsage = `usage:
  seamlessd relay serve [--data-dir DIR] [--socket PATH] [--socket-mode MODE] [--socket-group GROUP]
      serve the socket agents dial (default %[2]s) and forward every request to the server this relay
      is paired with; run it as the relay's own account (a LaunchDaemon or systemd unit), never as root.
      Every local account may connect (mode 0666) and the server judges each uid under peer_auth; a
      server without it takes every caller for the owner, so restrict the socket (0660 and a group
      of the owner's) on a computer with other accounts
  seamlessd relay pair --server URL --code CODE [--fingerprint SHA256] [--ca-file PEM] [--name NAME]
                       [--replace] [--json] [--data-dir DIR]
      pair this relay with a server, as the relay's account: CODE comes from the server's Devices page or
      seamlessd devices code (--code - reads it from stdin); --fingerprint is the server certificate's
      SHA-256 as the server shows it, compared before the code is sent
  The data directory defaults to %[1]s (0700, the relay's account's).
`

// relayUsageText is relayUsage with the defaults filled in.
func relayUsageText() string {
	return fmt.Sprintf(relayUsage, relay.DefaultDataDir, relay.DefaultSocketPath)
}

// runRelay is `seamlessd relay <command>`.
func runRelay(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, relayUsageText())
		return errors.New("seamlessd relay: name a command: serve or pair")
	}
	switch args[0] {
	case "serve":
		return runRelayServe(args[1:])
	case "pair":
		return runRelayPair(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, relayUsageText())
		return nil
	}
	return fmt.Errorf("seamlessd relay: unknown command %q: valid commands are serve, pair", args[0])
}

// relayServeOptions is one `seamlessd relay serve`.
type relayServeOptions struct {
	layout relay.Layout
	env    relay.Env
	// access is who may connect to the socket (--socket-mode, --socket-group).
	access relay.SocketAccess
	logger *slog.Logger
	// ready, when set, is told the socket's address once it is bound.
	ready func(net.Addr)
}

// runRelayServe parses `seamlessd relay serve` and serves until SIGINT or
// SIGTERM.
func runRelayServe(args []string) error {
	fs := flag.NewFlagSet("relay serve", flag.ContinueOnError)
	dataDir := fs.String("data-dir", relay.DefaultDataDir, "the relay's data directory: its pairing and key (0700)")
	socket := fs.String("socket", relay.DefaultSocketPath, "the socket agents dial")
	mode := fs.String("socket-mode", config.DefaultPeerSocketMode, "the socket's mode, in octal: who may try to connect")
	group := fs.String("socket-group", "", "a group to hand the socket to (by name or gid), with a mode such as 0660")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd relay serve: unexpected argument %q", fs.Arg(0))
	}
	access, err := relaySocketAccess(*mode, *group)
	if err != nil {
		return fmt.Errorf("seamlessd relay serve: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return relayServe(ctx, relayServeOptions{
		layout: relay.Layout{DataDir: filepath.Clean(*dataDir), Socket: filepath.Clean(*socket)},
		env:    relay.HostEnv(),
		access: access,
		logger: slog.Default(),
	})
}

// relaySocketAccess reads --socket-mode and --socket-group as serve reads
// peer_auth.socket's mode and group.
func relaySocketAccess(mode, group string) (relay.SocketAccess, error) {
	bits, err := config.PeerSocket{Mode: mode}.SocketFileMode()
	if err != nil {
		return relay.SocketAccess{}, fmt.Errorf("--socket-mode: %w", err)
	}
	a := relay.SocketAccess{Mode: os.FileMode(bits)}
	if g := strings.TrimSpace(group); g != "" {
		gid, err := lookupGID(g)
		if err != nil {
			return relay.SocketAccess{}, fmt.Errorf("--socket-group %s: %w", g, err)
		}
		if gid == 0 {
			return relay.SocketAccess{}, fmt.Errorf("--socket-group %s is root's group: name a group of the accounts that may connect", g)
		}
		a.GID = gid
	}
	return a, nil
}

// relayServe runs the relay until ctx ends: the preflight, the data dir's
// lock, the pairing, the socket, then the forwarder. It refuses to start on
// any preflight problem and while unpaired; a server it cannot reach is no
// reason not to start, since every request fails closed until it answers.
func relayServe(ctx context.Context, o relayServeOptions) error {
	const op = "seamlessd.relay"
	logger := o.logger
	if logger == nil {
		logger = slog.Default()
	}
	if problems := o.env.Problems(o.layout, true); len(problems) > 0 {
		return fmt.Errorf("%s: %w: %s", op, errRelayPreflight, strings.Join(problems, "; "))
	}
	lock, err := waitLockFile(ctx, filepath.Join(o.layout.DataDir, relay.LockName), dataDirLockWait, dataDirLockPoll, func(path, holder string) {
		logger.Info("relay data dir in use; waiting for the relay that holds it to exit", "lock", path, "holder", holder)
	})
	if errors.Is(err, errLockHeld) {
		return fmt.Errorf("%s: another relay serves %s: %w", op, o.layout.DataDir, err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = lock.Close() }()
	st, err := relay.LoadState(o.layout.DataDir)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	rt, err := st.Transport(o.layout.DataDir)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	server, err := relay.ServerURLOf(st.ServerURL)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	ln, err := o.env.ListenWith(o.layout.Socket, o.access)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	logger.Info("seamless relay listening", "socket", o.layout.Socket, "server", st.ServerURL,
		"server_fingerprint", st.ServerFingerprint, "device", st.DeviceID, "name", st.Name, "version", buildVersion())
	if o.ready != nil {
		o.ready(ln.Addr())
	}
	var probe sync.WaitGroup
	probe.Go(func() { relayStartupProbe(ctx, server.String(), rt, logger) })
	defer probe.Wait()
	return relay.Serve(ctx, ln, relay.NewForwarder(server, st.ServerFingerprint, rt, logger), logger)
}

// relayStartupProbe tells the log whether the server answers the relay at
// start: its /healthz over the pinned, certificate-presenting transport. It
// decides nothing; every request is judged on its own.
func relayStartupProbe(ctx context.Context, server string, rt http.RoundTripper, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, detail, err := relayHealthz(ctx, &http.Client{Transport: rt, CheckRedirect: config.RefuseRedirect}, server+"/healthz")
	switch accepted, why := relayProbeVerdict(status, err); {
	case err != nil:
		logger.Warn("relay: the server does not answer yet; every request fails closed until it does", "server", server, "error", err)
	case accepted:
		logger.Info("relay: the server answers, with the pinned certificate, and accepts this relay", "server", server, "status", status)
	default:
		logger.Warn("relay: "+why, "server", server, "status", status, "answer", detail)
	}
}

// relayPairOptions is one `seamlessd relay pair`.
type relayPairOptions struct {
	dataDir string
	env     relay.Env
	pair    relay.PairOptions
	json    bool
}

// runRelayPair parses `seamlessd relay pair` and pairs, waiting for the
// owner's approval where the server asks for it, until SIGINT or SIGTERM.
func runRelayPair(args []string) error {
	fs := flag.NewFlagSet("relay pair", flag.ContinueOnError)
	server := fs.String("server", "", "the server's address, https://host:port (required)")
	code := fs.String("code", "", "the one-time pairing code (required; - reads it from stdin)")
	fingerprint := fs.String("fingerprint", "", "the server certificate's SHA-256 fingerprint, as the server shows it")
	caFile := fs.String("ca-file", "", "the root certificate the server's certificate chains to, when the system does not trust it")
	name := fs.String("name", "", "the host name this relay speaks for (default: this computer's)")
	replace := fs.Bool("replace", false, "replace the pairing this relay holds")
	asJSON := fs.Bool("json", false, "print the pairing as JSON")
	dataDir := fs.String("data-dir", relay.DefaultDataDir, "the relay's data directory (0700)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd relay pair: unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*server) == "" || strings.TrimSpace(*code) == "" {
		return errors.New("seamlessd relay pair: --server and --code are required")
	}
	o := relayPairOptions{dataDir: filepath.Clean(*dataDir), env: relay.HostEnv(), json: *asJSON,
		pair: relay.PairOptions{Server: *server, Code: *code, Name: *name, ServerFingerprint: *fingerprint, Replace: *replace}}
	if *code == "-" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("seamlessd relay pair: read the code from stdin: %w", err)
		}
		o.pair.Code = strings.TrimSpace(line)
	}
	if *caFile != "" {
		ca, err := os.ReadFile(*caFile)
		if err != nil {
			return fmt.Errorf("seamlessd relay pair: --ca-file: %w", err)
		}
		o.pair.CA = ca
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return relayPair(ctx, o, os.Stdout, os.Stderr)
}

// relayPairJSON is what --json prints: the pairing's public facts, never its
// key.
type relayPairJSON struct {
	ServerURL         string    `json:"server_url"`
	ServerFingerprint string    `json:"server_fingerprint"`
	DeviceID          string    `json:"device_id"`
	Name              string    `json:"name"`
	Fingerprint       string    `json:"fingerprint"`
	PairedAt          time.Time `json:"paired_at"`
}

// relayPair pairs the relay as o says, after the account and data directory
// checks (a key must never land where another account could read it), and
// tells out what was pinned.
func relayPair(ctx context.Context, o relayPairOptions, out, notes io.Writer) error {
	const op = "seamlessd relay pair"
	if problems := o.env.PairProblems(o.dataDir); len(problems) > 0 {
		return fmt.Errorf("%s: %w: %s", op, errRelayPreflight, strings.Join(problems, "; "))
	}
	waitOut := out
	if o.json {
		waitOut = notes
	}
	o.pair.Waiting = func(fp string) {
		fmt.Fprintf(waitOut, "The server waits for the owner's approval: approve this computer on the server's Devices page, with a passkey.\n"+
			"The page shows this relay's key as %s: approve it only if it shows that.\n", fp)
	}
	st, err := relay.Pair(ctx, o.dataDir, o.pair)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if o.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(relayPairJSON{ServerURL: st.ServerURL, ServerFingerprint: st.ServerFingerprint, DeviceID: st.DeviceID,
			Name: st.Name, Fingerprint: st.Fingerprint, PairedAt: st.PairedAt})
	}
	fmt.Fprintf(out, "Paired: this relay speaks for %s (device %s) on %s.\n", st.Name, st.DeviceID, st.ServerURL)
	fmt.Fprintf(out, "  The server's certificate: SHA-256 %s.\n", st.ServerFingerprint)
	fmt.Fprintln(out, "  The relay forwards to that certificate alone: compare it with the one the server shows (seamlessd devices code prints it).")
	fmt.Fprintf(out, "  This relay's key: %s, as the server's Devices page shows it.\n", st.Fingerprint)
	fmt.Fprintln(out, "Start the relay, or restart it, to serve this pairing: seamlessd relay serve, as this account.")
	return nil
}
