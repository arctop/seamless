package main

// Device pairing on a networked server (plan networked-relay; internal/devices):
// when it is on, how serve wires it, doctor's rows, the closing output a
// server's setup prints, and `seamlessd devices`.

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// pairingOn reports whether this install pairs devices: it runs the daemon
// (not role: client) and serves other computers -- a TLS listener, a bind
// beyond loopback, or a server_url naming a host other than this one (a
// reverse proxy in front of a loopback bind). Nothing else turns it on, and no
// key does: a server that serves no other computer has no device to pair, and
// a default install (loopback, no TLS, no server_url) then has no Devices
// page, no pairing endpoint and no client-certificate request, exactly as
// before.
func pairingOn(cfg config.Config) bool {
	if cfg.IsClient() || cfg.SocketPath() != "" {
		return false
	}
	if cfg.TLSEnabled() || !isLoopbackBind(cfg.Addr) {
		return true
	}
	return strings.TrimSpace(cfg.AdvertisedURL) != "" && !isLoopbackServerHost(cfg.ServerHost())
}

// certFingerprintFile is the SHA-256 fingerprint (devices.CertFingerprint) of
// the first certificate in the PEM file at path: the server's own, which a
// client compares before it pairs. It is a hash of the certificate's bytes;
// whether the TLS stack can serve the certificate is the key pair load's
// question, and its error says so.
func certFingerprintFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read tls.cert_file: %w", err)
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return "", fmt.Errorf("tls.cert_file %s holds no certificate", path)
		}
		if block.Type == "CERTIFICATE" {
			return devices.CertFingerprint(block.Bytes), nil
		}
	}
}

// pairingFacts are what a client needs from this server to pair.
func pairingFacts(cfg config.Config) (devices.ServerFacts, error) {
	facts := devices.ServerFacts{URL: cfg.ServerURL(), TLS: cfg.TLSEnabled()}
	if facts.TLS {
		fp, err := certFingerprintFile(cfg.TLS.CertFile)
		if err != nil {
			return devices.ServerFacts{}, err
		}
		facts.CertFingerprint = fp
	}
	return facts, nil
}

// newPairing builds serve's device pairing, or nil when pairingOn says it is
// off. Under peer_auth a pairing waits for the owner's passkey, approved
// through svc's relying party.
func newPairing(cfg config.Config, db *sql.DB, rec *events.Recorder, svc *approval.Service, logger *slog.Logger) (*devices.Service, error) {
	if !pairingOn(cfg) {
		return nil, nil
	}
	facts, err := pairingFacts(cfg)
	if err != nil {
		return nil, fmt.Errorf("device pairing: %w", err)
	}
	dc := devices.Config{DB: db, Events: rec, PeerAuth: cfg.PeerAuth.Enabled, LocalHost: config.Hostname(),
		Server: facts, Logger: logger}
	if cfg.PeerAuth.Enabled {
		approvals, err := approval.NewDeviceApprovals(svc)
		if err != nil {
			return nil, fmt.Errorf("device pairing: %w", err)
		}
		dc.Approver = approvals
	}
	return devices.New(dc), nil
}

// devicePairingChecks is doctor's device pairing section: nothing while this
// install serves no other computers.
func devicePairingChecks(ctx context.Context, cfg config.Config, db *sql.DB) []check {
	if !pairingOn(cfg) {
		return nil
	}
	const name = "device pairing"
	how := "on: a computer pairs with a one-time code from the console's Devices page or `seamlessd devices code`, " +
		"and the code is the approval"
	if cfg.PeerAuth.Enabled {
		how = "on: a computer pairs with a one-time code, then waits until you approve it with a passkey on the " +
			"console's Devices page"
	}
	out := []check{{statusOK, name, how}}
	names := (devices.ServerFacts{URL: cfg.ServerURL()}).PairingNamesUser()
	if names {
		out = append(out, check{statusWarn, name, "server_url names a user, so no code is minted and no one-liner printed: " +
			"remove it, Seamless uses none (leave scheme://host:port), then restart the daemon"})
	}
	if !cfg.TLSEnabled() {
		line := "no TLS on this listener: a relay cannot pair (it needs a client certificate), and a pairing over plain " +
			"http from another computer is refused, since its code and a key device's key would cross the network in the clear"
		if (devices.ServerFacts{URL: cfg.ServerURL()}).PairingNeedsTLS() {
			line += "; server_url is plain http, so no other computer can pair here, and no code is minted"
		}
		out = append(out, check{statusWarn, name, line + "; give it TLS with seamlessd setup's address step, or set " +
			"tls.cert_file and tls.key_file"})
	} else if _, err := certFingerprintFile(cfg.TLS.CertFile); err != nil {
		out = append(out, check{statusFail, name, err.Error()})
	}
	if dial := cfg.DaemonAddress(); !names && (devices.ServerFacts{URL: dial}).PairingLoopbackOnly() {
		out = append(out, check{statusWarn, name, "the address clients dial, " + dial + ", is this computer's loopback, and the " +
			"pairing one-liners carry it, so another computer cannot use them: " + pairingLoopbackFix})
	}
	if db != nil {
		c, err := store.CountDevices(ctx, db, time.Now())
		if err != nil {
			return append(out, check{statusFail, name, "paired devices: " + err.Error()})
		}
		line := fmt.Sprintf("%d relay(s) and %d key device(s) paired", c.Relays, c.Keys)
		if c.Pending > 0 {
			line += fmt.Sprintf("; %d pairing(s) waiting for your approval", c.Pending)
		}
		out = append(out, check{statusInfo, name, line})
	}
	return out
}

// pairingLoopbackFix is how a Server whose address is this computer's
// loopback (devices.ServerFacts.PairingLoopbackOnly) becomes one the other
// computers can pair with. It names no setup step: setup's address step
// writes server_url when it makes a computer the Server, and a computer that
// is the Server already changes its address by hand (setupChanges offers it
// no address step).
const pairingLoopbackFix = "set server_url to the address the other computers reach this one at, then restart the daemon (" +
	setupDocs + "guides/network-install/#step-1-widen-the-server)"

// serverPairingClosing writes what a Client needs from this Server: its
// address, its certificate's fingerprint, how to get a pairing code, and the
// client one-liners, which carry the fingerprint. code is a code just minted,
// or "" for the placeholder. The server's setup prints it at its close;
// `seamlessd devices code` prints it with the code. A Server no other
// computer can pair with (devices.ServerFacts.PairingNeedsTLS), or whose
// address names a user (devices.ServerFacts.PairingNamesUser), gets the
// reasons instead of a code and the one-liners, and the address without its
// user; one whose address is this computer's loopback
// (devices.ServerFacts.PairingLoopbackOnly) gets both, for a client on this
// computer, and says another computer cannot use them.
func serverPairingClosing(w io.Writer, facts devices.ServerFacts, code string, peerAuth bool) {
	minutes := int(store.PairingCodeTTL / time.Minute)
	names := facts.PairingNamesUser()
	fmt.Fprintf(w, "To connect another computer to this server:\n\n")
	fmt.Fprintf(w, "  Address:                 %s\n", facts.Shown().URL)
	if facts.CertFingerprint != "" {
		fmt.Fprintf(w, "  Certificate fingerprint: %s\n", facts.CertFingerprint)
		fmt.Fprintf(w, "    (the other computer shows it before it pairs: they must match)\n")
	} else {
		fmt.Fprintf(w, "  No TLS: a relay cannot pair, and a pairing over plain http from another computer is refused.\n")
	}
	if facts.PairingLoopbackOnly() && !names {
		fmt.Fprintf(w, "  This address is this computer's loopback, and the one-liners below carry it, so another computer cannot use them:\n"+
			"    there it names that computer itself. They pair a client on this computer only. To pair another computer,\n"+
			"    %s.\n", pairingLoopbackFix)
	}
	if names {
		fmt.Fprintf(w, "  server_url names a user: remove it, Seamless uses none. The one-liners would carry it, password and all,\n"+
			"    into the other computer's shell history, and setup there refuses an address that names one.\n"+
			"    Leave scheme://host:port in server_url, restart the daemon, then run `seamlessd devices code`.\n")
	}
	if facts.PairingNeedsTLS() {
		fmt.Fprintf(w, "  Pairing needs TLS here: this address is plain http, so no other computer can pair with it.\n"+
			"    Give the server TLS with seamlessd setup's address step, or tls.cert_file and tls.key_file,\n"+
			"    then run `seamlessd devices code`.\n")
		return
	}
	if names {
		return
	}
	if code == "" {
		fmt.Fprintf(w, "  Pairing code:            get one on the console's Devices page, or run `seamlessd devices code` here\n")
	} else {
		fmt.Fprintf(w, "  Pairing code:            %s (one computer, %d minutes)\n", code, minutes)
	}
	if peerAuth {
		fmt.Fprintf(w, "    The code alone pairs nothing: approve the computer with your passkey on the Devices page,\n"+
			"    after checking that its fingerprint matches the one it shows.\n")
	} else {
		fmt.Fprintf(w, "    The code is the approval: whoever enters it first pairs as you.\n")
	}
	shown := code
	if shown == "" {
		shown = "<code>"
	}
	unix, windows := devices.ClientCommands(facts, shown)
	fmt.Fprintf(w, "\n  macOS and Linux (pairs a relay):\n    %s\n", unix)
	fmt.Fprintf(w, "  Windows (pairs a key device of its own):\n    %s\n", windows)
	fmt.Fprintf(w, "  The code works once, within %d minutes: the line it leaves in that computer's shell history is dead after use.\n",
		minutes)
}

const devicesUsage = `usage: seamlessd devices <verb>

The computers paired with this server (relays and key devices). Run these on
the server: as the daemon's own account under peer_auth.

  code [--json]          mint a one-time pairing code (10 minutes, one computer) and print
                         what the other computer needs
  list [--json]          list the paired devices and the pairings waiting for approval
  revoke <id|name>       revoke a device (its credential is refused from its next request),
                         or deny a pairing that waits
`

// devicesCmd is one verb with its arguments checked.
type devicesCmd struct {
	verb   string
	asJSON bool
	ref    string
}

// parseDevices checks a verb and its arguments and touches nothing.
func parseDevices(args []string) (devicesCmd, error) {
	cmd := devicesCmd{verb: args[0]}
	fs := flag.NewFlagSet("devices "+cmd.verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print JSON")
	switch cmd.verb {
	case "code", "list":
		if err := fs.Parse(args[1:]); err != nil {
			return cmd, fmt.Errorf("%s: %w", cmd.verb, err)
		}
		if fs.NArg() != 0 {
			return cmd, fmt.Errorf("%s takes no argument but --json", cmd.verb)
		}
		cmd.asJSON = *asJSON
	case "revoke":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return cmd, errors.New("revoke takes one device: its id or its name")
		}
		cmd.ref = args[1]
	default:
		return cmd, fmt.Errorf("unknown verb %q: valid verbs are code, list, revoke", cmd.verb)
	}
	return cmd, nil
}

func runDevices(args []string) error {
	if len(args) == 0 || args[0] == "help" || slices.ContainsFunc(args, func(a string) bool {
		return a == "-h" || a == "-help" || a == "--help"
	}) {
		fmt.Fprint(os.Stderr, devicesUsage)
		if len(args) == 0 {
			return errors.New("seamlessd.devices: a verb is required")
		}
		return nil
	}
	cmd, err := parseDevices(args)
	if err != nil {
		return fmt.Errorf("seamlessd.devices: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.devices: %w", err)
	}
	if !pairingOn(cfg) {
		return errors.New("seamlessd.devices: this install serves no other computers, so it pairs none: " +
			"pairing is on for a server with TLS, a bind beyond loopback, or a server_url naming another host")
	}
	db, err := openServedDB(cfg, os.Stderr)
	if err != nil {
		return fmt.Errorf("seamlessd.devices: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := cmd.run(context.Background(), db, cfg, os.Stdout); err != nil {
		return fmt.Errorf("seamlessd.devices: %w", err)
	}
	return nil
}

// run runs the verb against db, the served database.
func (c devicesCmd) run(ctx context.Context, db *sql.DB, cfg config.Config, out io.Writer) error {
	facts, err := pairingFacts(cfg)
	if err != nil {
		return err
	}
	svc := devices.New(devices.Config{DB: db, Events: events.NewRecorder(db), PeerAuth: cfg.PeerAuth.Enabled,
		LocalHost: config.Hostname(), Server: facts})
	switch c.verb {
	case "code":
		if err := facts.PairingRefusal(); err != nil {
			// No other computer could use the code, or its one-liners would
			// carry a user and password: refused, not minted.
			return err
		}
		code, err := svc.MintCode(ctx, store.DeviceByTerminal)
		if err != nil {
			return err
		}
		if c.asJSON {
			unix, windows := devices.ClientCommands(facts, code.Code)
			minted := map[string]any{"code": code.Code, "expires_at": code.ExpiresAt.UTC().Format(time.RFC3339),
				"approval": code.Approval, "server": facts, "unix": unix, "windows": windows}
			if facts.PairingLoopbackOnly() {
				// The one-liners name this computer's loopback: no other
				// computer can use them (the text output says so).
				minted["loopback"] = true
			}
			return writeDevicesJSON(out, minted)
		}
		serverPairingClosing(out, facts, code.Code, cfg.PeerAuth.Enabled)
		return nil
	case "list":
		list, err := svc.List(ctx)
		if err != nil {
			return err
		}
		if c.asJSON {
			if list == nil {
				list = []store.Device{}
			}
			return writeDevicesJSON(out, list)
		}
		return printDevices(out, list, time.Now())
	case "revoke":
		id, err := deviceRef(ctx, svc, c.ref)
		if err != nil {
			return err
		}
		d, err := svc.Revoke(ctx, id, store.DeviceByTerminal)
		if err != nil {
			return err
		}
		if d.Status == store.DeviceDenied {
			fmt.Fprintf(out, "Denied the pairing of %s (%s).\n", d.Name, d.ID)
			return nil
		}
		fmt.Fprintf(out, "Revoked %s (%s): it is refused from its next request.\n", d.Name, d.ID)
		return nil
	}
	return fmt.Errorf("unknown verb %q", c.verb)
}

// deviceRef resolves a device id, or the name of a live device, to its id.
func deviceRef(ctx context.Context, svc *devices.Service, ref string) (string, error) {
	list, err := svc.List(ctx)
	if err != nil {
		return "", err
	}
	now := time.Now()
	name := devices.NormalizeName(ref)
	for _, d := range list {
		if d.ID == ref || (d.Name == name && d.Live(now)) {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("no device %q: run seamlessd devices list", ref)
}

func writeDevicesJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printDevices lists the devices for a person.
func printDevices(out io.Writer, list []store.Device, now time.Time) error {
	if len(list) == 0 {
		_, err := fmt.Fprintln(out, "No device is paired. Mint a code with `seamlessd devices code`.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tKIND\tSTATUS\tFINGERPRINT OR KEY\tLAST SEEN")
	for _, d := range list {
		ident := d.Fingerprint
		if d.Kind == store.DeviceKeyKind && d.KeyPrefix != "" {
			ident = d.KeyPrefix + "..."
		}
		seen := "-"
		if d.LastUsedAt != nil {
			seen = d.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, d.Kind, d.StatusAt(now), ident, seen)
	}
	return tw.Flush()
}
