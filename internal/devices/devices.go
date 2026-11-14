// Package devices is device pairing for a networked server (plan
// networked-relay): every computer that connects to the server pairs with it
// once, through a one-time code the owner mints on the server, and gets a
// credential of its own.
//
//   - A relay (macOS and Linux clients) proves itself with a TLS client
//     certificate over its own key, on the server's TLS listener. Only a
//     client that offers RelayALPN is asked for a certificate
//     (WithRelayClientAuth), so a browser is never prompted for one, and the
//     certificate is matched by its key against an enrolled relay, never
//     verified against a CA: a chain is no proof of anything here.
//   - A key device (Windows, or a machine where the owner has no root, such as
//     a container or CI) holds a bearer key of its own, which replaces the
//     server's shared key for that device. The server stores only its hash.
//
// Without peer_auth the code is the approval: there is no daemon account and
// perhaps no passkey, and every process of the owner's is the owner anyway.
// Under peer_auth a pairing waits until the owner approves it with a passkey
// on the console's Devices page (internal/approval), after comparing the
// fingerprint both ends show: the code alone opens nothing.
//
// The daemon's edge (Service.Edge) authenticates a paired credential before
// any route sees the request, and marks the request with it (WithCredential).
// Every surface that checks the bearer key asks Bearer, the one verifier, so a
// paired device is the owner wherever the shared key is, and nowhere else. A
// device key never signs a browser in to the console: the login form compares
// the shared key alone.
//
// A relay forwards, with every request, what its own kernel reported of the
// local caller on the relay's Unix socket: the uid, the process chain and,
// from the wire's version 2, the program each process of the chain runs
// (RelayAttributes). The edge reads those headers only
// under an active relay's Credential, and never passes them on raw: a route
// sees what the edge read (RelayAttributesOf), and every other request has
// them stripped, so a bearer request or a key device carrying them is judged
// as the bearer request it is. They count only under peer_auth, for a relay
// peer_auth.relays maps (the daemon's relay judge, cmd/seamlessd): such a
// request is judged as the daemon's own socket judges one, its leases keyed
// on the relay and the process. Under peer_auth the judge refuses a relay the
// mapping does not name; a key device is no more than a bearer request: the
// owner over TCP, holding no lease, so it never opens a locked project.
package devices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Device kinds, as the store names them.
const (
	KindRelay = store.DeviceRelayKind
	KindKey   = store.DeviceKeyKind
)

// KeyPrefix starts every device key: the edge tells one from the server's
// shared key by it, and a secret scanner can find one.
const KeyPrefix = "seamless_dk_"

const (
	// keyBytes is a device key's randomness: 256 bits.
	keyBytes = 32
	// keyShown is how much of a key the Devices page shows: the prefix and
	// four characters of the random part.
	keyShown = len(KeyPrefix) + 4
	// secretBytes is a pairing secret's randomness: 256 bits.
	secretBytes = 32
)

// RelayALPN is the application protocol a relay offers in its TLS hello. The
// server asks for a client certificate only from a client offering it, and
// never selects it: the connection still speaks h2 or http/1.1.
const RelayALPN = "seamless-relay/1"

// RelayAttributePrefix names the headers a relay forwards about its local
// caller (uid, process chain). The edge reads them only under an active
// relay's Credential and strips every one of them from every request before
// any route runs (package doc).
const RelayAttributePrefix = "X-Seamless-Relay-"

// The headers a relay sets on every request it forwards, once each: the
// version, the local peer's uid as the relay's kernel reported it (decimal),
// the peer's kernel-verified process chain, the peer first, each process its
// access.ProcessToken.Key ("<boot>:<pid>:<version>") joined by "," with no
// spaces, and -- from version 2 -- the program each process of the chain runs
// (RelayProgramsHeader). The relay removes every header under
// RelayAttributePrefix a local client sent before it sets these, so a local
// process never supplies one.
const (
	RelayVersionHeader = "X-Seamless-Relay-Version"
	RelayUIDHeader     = "X-Seamless-Relay-Uid"
	RelayChainHeader   = "X-Seamless-Relay-Chain"
	// RelayProgramsHeader (version 2) names the program of every process of
	// the chain, in the chain's order, as the relay's kernel names it
	// (access.ProgramOf on the relay's computer): one entry per process, each
	// EncodeRelayProgram's form, joined by "," with no spaces. An empty entry
	// is a process whose program the kernel did not name.
	RelayProgramsHeader = "X-Seamless-Relay-Programs"
	// RelayWireVersion is version 1: the uid and the chain, no programs. A
	// server still reads it, and files no access request for such a relay's
	// processes (the anchor rule cannot judge a process nobody named).
	RelayWireVersion = "1"
	// RelayWireVersion2 is the version a relay of this release sends: version
	// 1's three headers and RelayProgramsHeader.
	RelayWireVersion2 = "2"
)

// MaxRelayProgramBytes bounds one program a relay names: no kernel names a
// longer one (macOS's proc_pidpath and Linux's /proc/<pid>/exe both stop at
// 4096 bytes), and a relay sends an empty entry rather than a longer one.
const MaxRelayProgramBytes = 4096

// ErrRelayAttributes is a relay request whose forwarded attributes are
// missing or malformed: the request is refused (400), never served as the
// owner.
var ErrRelayAttributes = errors.New("malformed relay attributes")

// RelayAttributes are what a relay forwarded about the local caller of one
// request: the uid and the process chain its own kernel reported, the peer
// first. Each process is one of the relay's computer, as the relay rendered
// it (ProcessToken.Relay unset): which relay it belongs to is the server's to
// say (access.Policy.RelayChainView), from the certificate the relay
// presented.
//
// Programs (wire version 2) names the program each process of Chain runs, as
// the relay's kernel named it, entry i for Chain[i]; "" where it named none.
// It is nil for a version 1 relay, which names no program.
type RelayAttributes struct {
	UID      uint32
	Chain    []access.ProcessToken
	Programs []string
}

// Named reports whether the relay named the chain's programs (wire version
// 2): only then can the anchor rule judge one of its processes.
func (a RelayAttributes) Named() bool {
	return len(a.Chain) > 0 && len(a.Programs) == len(a.Chain)
}

// ParseRelayAttributes reads the relay attribute headers of h, strictly.
// present is false when h carries no header under RelayAttributePrefix at
// all. Present, they must be exactly the headers the version they name sets,
// each once:
//
//   - version 1: the version, a decimal uid of at most 2^32-1, and a chain of
//     one to access.MaxChainHops processes, each a valid process key of the
//     relay's computer (access.ParseProcessKey);
//   - version 2: those three, and the programs (RelayProgramsHeader): exactly
//     one entry per process of the chain, each in EncodeRelayProgram's one
//     form, decoding to at most MaxRelayProgramBytes and no NUL.
//
// Anything else -- another version, a header the version lacks, a count of
// programs other than the chain's, an entry not in the one form -- is
// ErrRelayAttributes.
func ParseRelayAttributes(h http.Header) (attrs RelayAttributes, present bool, err error) {
	for name := range h {
		if !hasRelayPrefix(name) {
			continue
		}
		present = true
		switch http.CanonicalHeaderKey(name) {
		case RelayVersionHeader, RelayUIDHeader, RelayChainHeader, RelayProgramsHeader:
		default:
			return RelayAttributes{}, true, fmt.Errorf("%w: %s is not a relay header", ErrRelayAttributes, name)
		}
	}
	if !present {
		return RelayAttributes{}, false, nil
	}
	one := func(name string) (string, error) {
		vals := h.Values(name)
		switch len(vals) {
		case 0:
			return "", fmt.Errorf("%w: no %s", ErrRelayAttributes, name)
		case 1:
			return vals[0], nil
		}
		return "", fmt.Errorf("%w: %s is set %d times", ErrRelayAttributes, name, len(vals))
	}
	version, err := one(RelayVersionHeader)
	if err != nil {
		return RelayAttributes{}, true, err
	}
	switch version {
	case RelayWireVersion:
		if len(h.Values(RelayProgramsHeader)) > 0 {
			return RelayAttributes{}, true, fmt.Errorf("%w: %s is not a version %s header", ErrRelayAttributes,
				RelayProgramsHeader, RelayWireVersion)
		}
	case RelayWireVersion2:
	default:
		return RelayAttributes{}, true, fmt.Errorf("%w: version %q: this server reads versions %s and %s", ErrRelayAttributes,
			version, RelayWireVersion, RelayWireVersion2)
	}
	uidText, err := one(RelayUIDHeader)
	if err != nil {
		return RelayAttributes{}, true, err
	}
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil {
		return RelayAttributes{}, true, fmt.Errorf("%w: uid %q is not a decimal uid of at most 4294967295", ErrRelayAttributes, uidText)
	}
	chainText, err := one(RelayChainHeader)
	if err != nil {
		return RelayAttributes{}, true, err
	}
	if chainText == "" {
		return RelayAttributes{}, true, fmt.Errorf("%w: the process chain is empty", ErrRelayAttributes)
	}
	keys := strings.Split(chainText, ",")
	if len(keys) > access.MaxChainHops {
		return RelayAttributes{}, true, fmt.Errorf("%w: a process chain of %d hops, at most %d", ErrRelayAttributes, len(keys), access.MaxChainHops)
	}
	chain := make([]access.ProcessToken, 0, len(keys))
	for _, key := range keys {
		t, err := access.ParseProcessKey(key)
		if err != nil {
			return RelayAttributes{}, true, fmt.Errorf("%w: %w", ErrRelayAttributes, err)
		}
		chain = append(chain, t)
	}
	attrs = RelayAttributes{UID: uint32(uid), Chain: chain}
	if version == RelayWireVersion {
		return attrs, true, nil
	}
	programsText, err := one(RelayProgramsHeader)
	if err != nil {
		return RelayAttributes{}, true, err
	}
	entries := strings.Split(programsText, ",")
	if len(entries) != len(chain) {
		return RelayAttributes{}, true, fmt.Errorf("%w: %d programs for a process chain of %d", ErrRelayAttributes, len(entries), len(chain))
	}
	attrs.Programs = make([]string, len(entries))
	for i, e := range entries {
		p, err := DecodeRelayProgram(e)
		if err != nil {
			return RelayAttributes{}, true, fmt.Errorf("%w: program %d: %w", ErrRelayAttributes, i+1, err)
		}
		attrs.Programs[i] = p
	}
	return attrs, true, nil
}

// errRelayProgram is a program entry that is not EncodeRelayProgram's form.
var errRelayProgram = errors.New("not a relay program entry")

// relayProgramPlain reports whether EncodeRelayProgram writes c as it is:
// RFC 3986's unreserved bytes and "/".
func relayProgramPlain(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~' || c == '/'
}

// EncodeRelayProgram is one program as the wire carries it (version 2): every
// byte in A-Z a-z 0-9 - . _ ~ / as it is, every other byte as %XX with two
// upper-case hex digits. So an entry holds no "," and nothing outside
// printable ASCII, and one program has exactly one encoding. A program the
// wire cannot carry -- longer than MaxRelayProgramBytes, or holding a NUL,
// which no kernel names -- is sent as no program at all (""), as a relay
// sends one its kernel did not name.
func EncodeRelayProgram(program string) string {
	if len(program) > MaxRelayProgramBytes || strings.IndexByte(program, 0) >= 0 {
		return ""
	}
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(program); i++ {
		c := program[i]
		if relayProgramPlain(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// EncodeRelayPrograms is the programs header's value for programs, entry i
// for the chain's process i.
func EncodeRelayPrograms(programs []string) string {
	out := make([]string, len(programs))
	for i, p := range programs {
		out[i] = EncodeRelayProgram(p)
	}
	return strings.Join(out, ",")
}

// DecodeRelayProgram reads one program entry back, strictly: it must be
// exactly EncodeRelayProgram's rendering of what it decodes to -- every "%"
// followed by two upper-case hex digits, no byte left plain that must be
// encoded, none encoded that may stand plain -- and decode to at most
// MaxRelayProgramBytes with no NUL. "" is a program nobody named.
func DecodeRelayProgram(entry string) (string, error) {
	if len(entry) > 3*MaxRelayProgramBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", errRelayProgram, MaxRelayProgramBytes)
	}
	hexVal := func(c byte) (byte, bool) {
		switch {
		case '0' <= c && c <= '9':
			return c - '0', true
		case 'A' <= c && c <= 'F':
			return c - 'A' + 10, true
		}
		return 0, false
	}
	out := make([]byte, 0, len(entry))
	for i := 0; i < len(entry); i++ {
		c := entry[i]
		switch {
		case relayProgramPlain(c):
			out = append(out, c)
		case c == '%':
			if i+2 >= len(entry) {
				return "", fmt.Errorf("%w: a %% without two hex digits", errRelayProgram)
			}
			hi, ok1 := hexVal(entry[i+1])
			lo, ok2 := hexVal(entry[i+2])
			if !ok1 || !ok2 {
				return "", fmt.Errorf("%w: a %% without two upper-case hex digits", errRelayProgram)
			}
			v := hi<<4 | lo
			if relayProgramPlain(v) {
				return "", fmt.Errorf("%w: %q is encoded and stands plain", errRelayProgram, string(rune(v)))
			}
			if v == 0 {
				return "", fmt.Errorf("%w: a NUL", errRelayProgram)
			}
			out = append(out, v)
			i += 2
		default:
			return "", fmt.Errorf("%w: byte 0x%02x is not encoded", errRelayProgram, c)
		}
	}
	if len(out) > MaxRelayProgramBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", errRelayProgram, MaxRelayProgramBytes)
	}
	return string(out), nil
}

// hasRelayPrefix reports whether a header name is under RelayAttributePrefix,
// in any case.
func hasRelayPrefix(name string) bool {
	return len(name) >= len(RelayAttributePrefix) && strings.EqualFold(name[:len(RelayAttributePrefix)], RelayAttributePrefix)
}

type relayAttributesKey struct{}

// withRelayAttributes returns ctx carrying a, what the edge read of the
// request's relay headers under the relay's Credential.
func withRelayAttributes(ctx context.Context, a RelayAttributes) context.Context {
	return context.WithValue(ctx, relayAttributesKey{}, a)
}

// RelayAttributesOf returns what the request's relay forwarded about its local
// caller, when the edge read any: only a request under an active relay's
// Credential that carried the headers has them. Nothing else ever does --
// whatever headers it sent.
func RelayAttributesOf(ctx context.Context) (RelayAttributes, bool) {
	a, ok := ctx.Value(relayAttributesKey{}).(RelayAttributes)
	return a, ok
}

// hostHeader and hookHostParam are where a request names the machine it
// speaks for: the MCP surface's X-Seamless-Host header (mcp.HostHeader) and
// the hooks' host query parameter (hooks.IdentityQueryParams). Both are
// test-pinned against their owners; this package cannot import either.
const (
	hostHeader    = "X-Seamless-Host"
	hookHostParam = "host"
	hookPathStart = "/api/hooks/"
)

// Events, in the access.* family. Payloads carry ids, kinds and fixed words,
// never a code, a key or a secret.
const (
	// EventCodeMinted records a pairing code minted (payload: by, minutes).
	EventCodeMinted core.EventKind = "access.device_code"
	// EventPending records a pairing waiting for the owner's passkey
	// (item_id: the device; payload: kind, name).
	EventPending core.EventKind = "access.device_pending"
	// EventPaired records a device paired (item_id: the device; payload:
	// kind, name, proof -- code or passkey -- and credential, the passkey).
	EventPaired core.EventKind = "access.device_paired"
	// EventRevoked records a device revoked, or a pending pairing denied
	// (item_id: the device; payload: kind, name, action -- revoke or deny --
	// and by).
	EventRevoked core.EventKind = "access.device_revoked"
)

// The proofs a pairing records.
const (
	// ProofCode is a pairing the one-time code approved (peer_auth off).
	ProofCode = "code"
	// ProofPasskey is a pairing the owner approved with a passkey.
	ProofPasskey = "passkey"
)

// Credential is the paired device a request presented the credential of:
// the edge sets it once it matched an active device (WithCredential).
type Credential struct {
	DeviceID string
	Kind     string
	// Name is the device's enrolled host name: the host the request speaks
	// for.
	Name string
	// Approved reports a pairing the owner approved with a passkey
	// (store.Device.ApprovedBy): every pairing under peer_auth. A device paired
	// while peer_auth was off has the one-time code alone behind it.
	Approved bool
}

type credentialKey struct{}

// WithCredential returns ctx carrying c. Only the daemon's edge calls it, once
// it matched the request's own credential against an active device.
func WithCredential(ctx context.Context, c Credential) context.Context {
	return context.WithValue(ctx, credentialKey{}, c)
}

// CredentialOf returns the paired device the request's credential named, if
// the edge matched one.
func CredentialOf(ctx context.Context) (Credential, bool) {
	c, ok := ctx.Value(credentialKey{}).(Credential)
	return c, ok && c.DeviceID != ""
}

// Bearer reports whether r presents the owner's key: a paired device's own
// credential, which the edge matched for this request, or the server's shared
// key in its Authorization header (a constant-time compare; an empty shared
// key matches nothing). It is the one verifier every surface that checks the
// bearer key calls -- MCP, the hooks, A2A and the console -- so a device key
// passes exactly where the shared key does.
func Bearer(r *http.Request, sharedKey string) bool {
	if _, ok := CredentialOf(r.Context()); ok {
		return true
	}
	if sharedKey == "" {
		return false
	}
	token, ok := bearerToken(r)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(sharedKey)) == 1
}

// bearerToken is the token of r's Authorization: Bearer header.
func bearerToken(r *http.Request) (string, bool) {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	return parts[1], true
}

// StripRelayAttributes removes every header under RelayAttributePrefix from
// h, and reports how many it removed.
func StripRelayAttributes(h http.Header) int {
	n := 0
	for name := range h {
		if hasRelayPrefix(name) {
			delete(h, name)
			n++
		}
	}
	return n
}

// NewKey draws a device key, and returns it with the part of it the Devices
// page shows. The server keeps only KeyHash of it.
func NewKey() (key, shown string, err error) {
	raw := make([]byte, keyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("devices.NewKey: %w", err)
	}
	key = KeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return key, key[:keyShown], nil
}

// IsKey reports whether s has a device key's shape.
func IsKey(s string) bool {
	if !strings.HasPrefix(s, KeyPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s[len(KeyPrefix):])
	return err == nil && len(raw) == keyBytes
}

// KeyHash is what the server stores of a device key: hex SHA-256 under a
// prefix of its own.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte("seamless device key v1\x00" + key))
	return hex.EncodeToString(sum[:])
}

// ErrSecret is a pairing secret of the wrong shape.
var ErrSecret = errors.New("a pairing secret is 32 random bytes, base64url without padding")

// NewSecret draws a pairing secret: the device chooses it, sends it with its
// pairing, and asks after the pairing with it. It is the only thing that can
// collect a key device's key.
func NewSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("devices.NewSecret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func secretBytesOf(secret string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(secret)
	if err != nil || len(raw) != secretBytes {
		return nil, ErrSecret
	}
	return raw, nil
}

// SecretHash is what the server stores of a pairing secret.
func SecretHash(secret string) (string, error) {
	raw, err := secretBytesOf(secret)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("seamless device pairing secret v1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

// SecretFingerprint is a key device's fingerprint while it waits: what its
// terminal and the Devices page both show, XXXX-XXXX-XXXX-XXXX. It is a hash
// under another prefix than SecretHash's, so showing it reveals nothing that
// asks after the pairing.
func SecretFingerprint(secret string) (string, error) {
	raw, err := secretBytesOf(secret)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("seamless device pairing fingerprint v1\x00"), raw...))
	return store.ApproverKeyFingerprint(sum[:]), nil
}

// PublicKeyFingerprint is a relay's fingerprint, XXXX-XXXX-XXXX-XXXX, over its
// DER SubjectPublicKeyInfo: the same form as an approver key's
// (store.ApproverKeyFingerprint).
func PublicKeyFingerprint(spki []byte) string {
	return store.ApproverKeyFingerprint(spki)
}

// PublicKeyHash is what a relay's requests are matched by: hex SHA-256 of its
// DER SubjectPublicKeyInfo, all 256 bits (the fingerprint shows 80 of them,
// for a person to compare).
func PublicKeyHash(spki []byte) string {
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:])
}

// CertFingerprint is a certificate's SHA-256 fingerprint as openssl prints it
// (`openssl x509 -noout -fingerprint -sha256`): upper-case hex pairs joined by
// colons. The server shows its own for a client to compare before it pairs.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	var b strings.Builder
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", v)
	}
	return b.String()
}

// sameCertFingerprint compares two CertFingerprint values as a person may
// type them: case and separators aside.
func sameCertFingerprint(a, b string) bool {
	norm := func(s string) string {
		return strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(s)))
	}
	na, nb := norm(a), norm(b)
	return na != "" && subtle.ConstantTimeCompare([]byte(na), []byte(nb)) == 1
}

// NormalizeName turns a host name as a machine reports it into a device name:
// trimmed and lower-cased, as config.Hostname does.
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// The installer one-liners that pair another computer with this server. The
// environment names are the ones docs/install and docs/install.ps1 read
// (`seamlessd setup`'s knobs), written once here so the Devices page, the
// server's setup and `seamlessd devices code` print the same commands.
const (
	unixPairCommand    = "curl -fsSL https://thereisnospoon.org/install | SEAMLESS_SETUP=client SEAMLESS_SERVER_URL=%s SEAMLESS_PAIR_CODE=%s sh"
	windowsPairCommand = "$env:SEAMLESS_SETUP='client'; $env:SEAMLESS_SERVER_URL='%s'; $env:SEAMLESS_PAIR_CODE='%s'; irm https://thereisnospoon.org/install.ps1 | iex"
)

// ClientCommands are the one-liners that pair another computer with the
// server at serverURL using code: macOS and Linux (whose client pairs a
// relay), and Windows (a key device). code may be a placeholder.
func ClientCommands(serverURL, code string) (unix, windows string) {
	return fmt.Sprintf(unixPairCommand, serverURL, code), fmt.Sprintf(windowsPairCommand, serverURL, code)
}
