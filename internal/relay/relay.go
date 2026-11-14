// Package relay is the relay daemon on a client computer (plan
// networked-relay): `seamlessd relay serve`, run as a system account of its
// own, serves the Unix socket path agents use beside a peer_auth daemon, so
// seam, its hooks, the seam mcp-proxy bridge and the Touch ID helper reach the
// server through it exactly as they reach a daemon on their own machine, with
// no bearer key on the computer.
//
// What it does, per local connection: it reads the caller's uid and process
// (access.PeerUID, access.ReadPeer) from its own kernel at accept, walks the
// process chain per request (access.Peer.Chain), and forwards the request to
// the server it paired with, over TLS with its own client certificate
// (devices.RelayIdentity), carrying those two facts in the headers the wire
// contract names (HeaderVersion, HeaderUID, HeaderChain). Every
// X-Seamless-Relay-* header and the Authorization header a local client sent
// are removed first; everything else passes through unchanged, the agent
// process identity (X-Seamless-Agent-Process, which the local seam computes
// with internal/agentproc) and Mcp-Session-Id included. Responses stream back
// as they arrive: MCP's streamable HTTP and SSE, an elicitation's round trip,
// the console's live feed.
//
// What it never does: decide anything. The server judges every request (the
// relay says who is asking, never that the owner said yes); the relay
// interprets no content, caches nothing and answers nothing from memory. A
// server it cannot reach, or one presenting any other certificate than the one
// pinned at pairing, gets every request a clear refusal (502) and nothing
// more (D6).
//
// Why it needs root to set up: the uid and chain it reports are believed only
// because no process of the owner's can change what the relay says. So it runs
// as an account of its own, pinned by root like the daemon's
// (access.DaemonAccount), with its files where only that account and root can
// write: the binary, the socket's directory and its data directory, which
// holds its private key. Its preflight (Env.Problems) refuses any other
// layout, as serve's peer_auth preflight does. It runs on macOS and Linux
// only, the platforms whose kernels name a socket's peer; a Windows computer
// pairs as a key device.
package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/files"
)

// The root-owned layout, as on a peer_auth server (the peer_auth guide's
// /opt/seamless): the socket where agents dial a daemon, and a data directory
// of the relay's own, 0700, beside the daemon's.
const (
	// DefaultSocketPath is the socket agents dial beside a peer_auth daemon
	// (server_url: unix:///opt/seamless/run/seamlessd.sock).
	DefaultSocketPath = "/opt/seamless/run/seamlessd.sock"
	// DefaultDataDir is the relay's own data directory: its pairing and key.
	DefaultDataDir = "/opt/seamless/relay"
	// DefaultBinary is where the layout keeps seamlessd.
	DefaultBinary = "/opt/seamless/bin/seamlessd"
)

// The files in the data directory.
const (
	// StateName is the pairing: the server's address, the fingerprint of its
	// certificate (pinned), the device the server enrolled, and the relay's
	// certificate and private key. 0600.
	StateName = "relay.json"
	// LockName is the one-relay-per-data-dir lock serve holds. 0600.
	LockName = "relay.lock"
	// caPrefix and caSuffix name the copy of the root a server's certificate
	// chains to, when the owner gave one at pairing (server-ca-<hash>.pem,
	// 0600): named by its content, so a new pairing never overwrites the copy
	// the current one reads before the state switches over.
	caPrefix = "server-ca-"
	caSuffix = ".pem"
)

// Modes of what the relay writes.
const (
	// DataDirMode is the data directory's: the relay's account alone.
	DataDirMode fs.FileMode = 0o700
	// FileMode is every file in it: the state holds the private key.
	FileMode fs.FileMode = 0o600
	// SocketMode is the socket's: every local account may connect, and the
	// server judges the uid the relay reports for it.
	SocketMode fs.FileMode = 0o666
)

// stateVersion is the format of StateName this release writes and reads.
const stateVersion = 1

// maxStateBytes bounds what is read of the state: a few kilobytes of PEM.
const maxStateBytes = 64 << 10

var (
	// ErrUnsupported is the relay on a platform it does not run on (Supported
	// is false): there, no kernel call names a socket's peer.
	ErrUnsupported = errors.New("the relay runs on macOS and Linux only: a computer on another platform pairs with the server as a key device (seamlessd setup)")
	// ErrNotPaired is a data directory with no pairing in it.
	ErrNotPaired = errors.New("this relay is not paired with a server: pair it with `seamlessd relay pair --server URL --code CODE`, as the relay's account")
	// ErrState is a pairing that does not load.
	ErrState = errors.New("the relay's pairing does not load")
)

// State is the relay's pairing, as relay.json keeps it.
type State struct {
	Version int `json:"version"`
	// ServerURL is the server's address, https://host[:port].
	ServerURL string `json:"server_url"`
	// ServerFingerprint is the SHA-256 fingerprint of the certificate the
	// server presented at pairing (devices.CertFingerprint, openssl's form):
	// the only certificate the relay forwards to.
	ServerFingerprint string `json:"server_fingerprint"`
	// CAFile names the copy of the root the server's certificate chains to,
	// beside the state, when the owner gave one at pairing; "" trusts the
	// system's roots.
	CAFile string `json:"ca_file,omitempty"`
	// DeviceID, Name and Fingerprint are the device the server enrolled: its
	// id, the host name it speaks for, and the relay key's fingerprint the
	// Devices page shows.
	DeviceID    string    `json:"device_id"`
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	PairedAt    time.Time `json:"paired_at"`
	// Certificate and Key are the relay's identity (devices.RelayIdentity
	// MarshalPEM). Key is the secret.
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
}

// Identity loads the relay's identity from the state.
func (s State) Identity() (*devices.RelayIdentity, error) {
	id, err := devices.LoadRelayIdentity([]byte(s.Certificate), []byte(s.Key))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrState, err)
	}
	return id, nil
}

// CAPath is the path of the state's CA copy in dataDir, "" when it names none.
func (s State) CAPath(dataDir string) string {
	if s.CAFile == "" {
		return ""
	}
	return filepath.Join(dataDir, s.CAFile)
}

// validate checks every field a relay relies on.
func (s State) validate() error {
	if s.Version != stateVersion {
		return fmt.Errorf("%w: format version %d, and this release reads %d", ErrState, s.Version, stateVersion)
	}
	if _, err := ServerURLOf(s.ServerURL); err != nil {
		return fmt.Errorf("%w: %w", ErrState, err)
	}
	if !devices.ValidCertFingerprint(s.ServerFingerprint) {
		return fmt.Errorf("%w: no pinned server certificate fingerprint", ErrState)
	}
	if s.CAFile != "" && !validCAName(s.CAFile) {
		return fmt.Errorf("%w: ca_file %q is not a CA copy this relay writes", ErrState, s.CAFile)
	}
	if s.DeviceID == "" || s.Name == "" {
		return fmt.Errorf("%w: no device", ErrState)
	}
	id, err := s.Identity()
	if err != nil {
		return err
	}
	if s.Fingerprint != id.Fingerprint() {
		return fmt.Errorf("%w: the recorded fingerprint %s is not the key's (%s)", ErrState, s.Fingerprint, id.Fingerprint())
	}
	return nil
}

// ServerURLOf parses a server address a relay forwards to: https, a host, an
// optional port, and nothing else (a trailing slash aside).
func ServerURLOf(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(raw), "/"))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%q is not a server address: %w", raw, err)
	case u.Scheme != "https":
		return nil, fmt.Errorf("%q is not an https address: a relay reaches the server over TLS, with its client certificate", raw)
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("%q names no host", raw)
	case u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, fmt.Errorf("%q must be the server's address alone: https://host:port", raw)
	}
	return u, nil
}

// LoadState reads the pairing in dataDir. A directory with none is
// ErrNotPaired; a pairing that is there but does not load, or that is not a
// regular file of its own, is ErrState: the relay refuses to serve on either.
func LoadState(dataDir string) (State, error) {
	path := filepath.Join(dataDir, StateName)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, ErrNotPaired
	}
	if err != nil {
		return State{}, fmt.Errorf("relay.LoadState: %w", err)
	}
	if !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("%w: %s is not a regular file", ErrState, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return State{}, fmt.Errorf("relay.LoadState: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return State{}, fmt.Errorf("relay.LoadState: %w", err)
	}
	if len(data) > maxStateBytes {
		return State{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrState, path, maxStateBytes)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("%w: %s: %w", ErrState, path, err)
	}
	if err := s.validate(); err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// SaveState writes s as the pairing in dataDir, atomically (a temp file
// renamed over the old one) and 0600, then removes every CA copy s does not
// name. The rename is the switch: until it lands the previous pairing stands.
func SaveState(dataDir string, s State) error {
	s.Version = stateVersion
	if err := s.validate(); err != nil {
		return fmt.Errorf("relay.SaveState: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("relay.SaveState: %w", err)
	}
	if err := files.AtomicWrite(filepath.Join(dataDir, StateName), append(data, '\n'), FileMode); err != nil {
		return fmt.Errorf("relay.SaveState: %w", err)
	}
	return removeStaleCAs(dataDir, s.CAFile)
}

// writeCA copies a root certificate into dataDir under a name of its content,
// 0600, and returns the name.
func writeCA(dataDir string, pemBytes []byte) (string, error) {
	sum := sha256.Sum256(pemBytes)
	name := caPrefix + hex.EncodeToString(sum[:8]) + caSuffix
	if err := files.AtomicWrite(filepath.Join(dataDir, name), pemBytes, FileMode); err != nil {
		return "", fmt.Errorf("relay: copy the server's CA: %w", err)
	}
	return name, nil
}

// validCAName reports whether name is one writeCA gives.
func validCAName(name string) bool {
	h, ok := strings.CutPrefix(name, caPrefix)
	if !ok {
		return false
	}
	h, ok = strings.CutSuffix(h, caSuffix)
	if !ok || len(h) != 16 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// caCopies lists the CA copies in dataDir.
func caCopies(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if validCAName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// removeStaleCAs removes every CA copy in dataDir but keep.
func removeStaleCAs(dataDir, keep string) error {
	names, err := caCopies(dataDir)
	if err != nil {
		return fmt.Errorf("relay: list CA copies: %w", err)
	}
	for _, n := range names {
		if n == keep {
			continue
		}
		if err := os.Remove(filepath.Join(dataDir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("relay: remove an old CA copy: %w", err)
		}
	}
	return nil
}
