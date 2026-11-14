package devices

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"
)

// relayCertLifetime is how long a relay's self-signed certificate is dated
// for. Nothing checks its dates: the server matches the key it carries, and
// revocation, not expiry, ends a relay.
const relayCertLifetime = 20 * 365 * 24 * time.Hour

var (
	// ErrRelayCertificate is a relay certificate the server will not match:
	// more than one certificate, or a key of a type it does not accept.
	ErrRelayCertificate = errors.New("a relay presents one certificate over an ECDSA P-256 or Ed25519 key")
	// ErrRelayIdentity is a relay identity that does not load.
	ErrRelayIdentity = errors.New("invalid relay identity")
)

// WithRelayClientAuth returns a copy of base, the server's TLS config, that
// asks a relay for its client certificate: a client whose hello offers
// RelayALPN gets a config that requests one (tls.RequestClientCert) and
// checks only its shape (one certificate, an accepted key type). Every other
// client -- a browser, curl, seam with a key -- is never asked for a
// certificate, so the handshake it sees is exactly base's and no browser shows
// a certificate picker.
//
// No chain is verified, here or anywhere: a CA's signature is no proof that a
// relay was paired with this server. The handshake proves the client holds the
// private key of the certificate it sent (CertificateVerify), and the edge
// then matches that key against the enrolled relays on every request
// (Service.Edge), so a revoked relay is refused at its next request.
//
// A relay's connection never resumes a session: a ticket would carry the
// certificate past a full handshake. base's own GetConfigForClient, if any, is
// replaced.
func WithRelayClientAuth(base *tls.Config) *tls.Config {
	out := base.Clone()
	relay := base.Clone()
	relay.GetConfigForClient = nil
	relay.ClientAuth = tls.RequestClientCert
	relay.VerifyConnection = verifyRelayConnection
	relay.SessionTicketsDisabled = true
	// ALPN still selects h2 or http/1.1, never RelayALPN: the marker only says
	// "ask me for my certificate".
	relay.NextProtos = []string{"h2", "http/1.1"}
	out.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if slices.Contains(hello.SupportedProtos, RelayALPN) {
			return relay, nil
		}
		return nil, nil
	}
	return out
}

// verifyRelayConnection checks a relay certificate's shape: none (the request
// is then judged by the bearer key, as before), or exactly one over an
// accepted key.
func verifyRelayConnection(cs tls.ConnectionState) error {
	switch len(cs.PeerCertificates) {
	case 0:
		return nil
	case 1:
		return acceptedRelayKey(cs.PeerCertificates[0].PublicKey)
	}
	return ErrRelayCertificate
}

// acceptedRelayKey reports whether a relay's public key is of a type the
// server accepts: ECDSA over P-256, or Ed25519.
func acceptedRelayKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() {
			return nil
		}
	case ed25519.PublicKey:
		if len(k) == ed25519.PublicKeySize {
			return nil
		}
	}
	return ErrRelayCertificate
}

// RelayKeyOf returns the DER SubjectPublicKeyInfo of the certificate a relay
// presented on cs, when it presented one; ok is false when it presented none.
func RelayKeyOf(cs *tls.ConnectionState) (spki []byte, ok bool, err error) {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return nil, false, nil
	}
	if len(cs.PeerCertificates) != 1 {
		return nil, true, ErrRelayCertificate
	}
	cert := cs.PeerCertificates[0]
	if err := acceptedRelayKey(cert.PublicKey); err != nil {
		return nil, true, err
	}
	return cert.RawSubjectPublicKeyInfo, true, nil
}

// RelayIdentity is a relay's key and the self-signed certificate it presents
// over it. The relay generates it at pairing and keeps it in its own data
// directory, readable by its account alone.
type RelayIdentity struct {
	cert tls.Certificate
	spki []byte
}

// NewRelayIdentity generates a relay identity: an ECDSA P-256 key, and a
// self-signed certificate over it naming name.
func NewRelayIdentity(name string) (*RelayIdentity, error) {
	const op = "devices.NewRelayIdentity"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: NormalizeName(name), Organization: []string{"Seamless relay"}},
		NotBefore:    now,
		NotAfter:     now.Add(relayCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return relayIdentityOf(der, key)
}

func relayIdentityOf(der []byte, key crypto.Signer) (*RelayIdentity, error) {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelayIdentity, err)
	}
	if err := acceptedRelayKey(leaf.PublicKey); err != nil {
		return nil, err
	}
	if !samePublicKey(leaf.PublicKey, key.Public()) {
		return nil, fmt.Errorf("%w: the key does not match the certificate", ErrRelayIdentity)
	}
	return &RelayIdentity{
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		spki: leaf.RawSubjectPublicKeyInfo,
	}, nil
}

func samePublicKey(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	e, ok := a.(equaler)
	return ok && e.Equal(b)
}

// LoadRelayIdentity reads a relay identity MarshalPEM wrote.
func LoadRelayIdentity(certPEM, keyPEM []byte) (*RelayIdentity, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: no certificate", ErrRelayIdentity)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%w: no private key", ErrRelayIdentity)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelayIdentity, err)
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%w: the key cannot sign", ErrRelayIdentity)
	}
	return relayIdentityOf(cb.Bytes, key)
}

// MarshalPEM encodes the identity: the certificate, and the private key as
// PKCS #8. The key's PEM is a secret: write it 0600, in a directory only the
// relay's account reads.
func (id *RelayIdentity) MarshalPEM() (certPEM, keyPEM []byte, err error) {
	der, err := x509.MarshalPKCS8PrivateKey(id.cert.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("devices.RelayIdentity.MarshalPEM: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: id.cert.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// PublicKey is the identity's DER SubjectPublicKeyInfo.
func (id *RelayIdentity) PublicKey() []byte { return slices.Clone(id.spki) }

// Fingerprint is what the relay's terminal shows for the owner to compare
// with the Devices page (PublicKeyFingerprint).
func (id *RelayIdentity) Fingerprint() string { return PublicKeyFingerprint(id.spki) }

// ClientTLS returns a copy of base (nil: a TLS 1.2 floor and nothing else)
// that presents the relay's certificate whenever the server asks, and offers
// RelayALPN beside http/1.1 so the server asks. Verification of the server is
// base's, untouched: InsecureSkipVerify is never set here.
func (id *RelayIdentity) ClientTLS(base *tls.Config) *tls.Config {
	var out *tls.Config
	if base == nil {
		out = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		out = base.Clone()
	}
	cert := id.cert
	out.Certificates = nil
	out.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
	protos := slices.DeleteFunc(slices.Clone(out.NextProtos), func(p string) bool { return p == RelayALPN })
	if len(protos) == 0 {
		protos = []string{"http/1.1"}
	}
	out.NextProtos = append(protos, RelayALPN)
	return out
}
