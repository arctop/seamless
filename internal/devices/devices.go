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
// What a paired credential is NOT, yet: a relay's forwarded attributes (the
// local caller's uid, process chain) never count. Every header under
// RelayAttributePrefix is stripped at the edge from every request, a relay's
// included, until the server judges a relay's requests as it judges its own
// socket's (task 01M73RFZ9TMRQXWM6GBZPBZN6R). Under peer_auth a paired device
// is therefore no more than a bearer request: the owner over TCP, holding no
// lease, so it never opens a locked project.
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
	"strings"

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

// RelayAttributePrefix names the headers a relay will forward about its local
// caller (uid, process chain). The edge strips every one of them from every
// request until the server judges them (package doc).
const RelayAttributePrefix = "X-Seamless-Relay-"

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
		if len(name) >= len(RelayAttributePrefix) && strings.EqualFold(name[:len(RelayAttributePrefix)], RelayAttributePrefix) {
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
