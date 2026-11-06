package passkey_test

// A virtual WebAuthn authenticator and client for the tests: it reads the
// options BeginRegistration and BeginAssertion produce, and answers with the
// JSON a browser's PublicKeyCredential.toJSON() would, building
// clientDataJSON, authenticatorData and a "none" attestationObject itself and
// signing with crypto/ecdsa, crypto/ed25519 or crypto/rsa. It is not Apple's
// or anyone's: no attestation, no platform API, just the wire format.
// Ceremony options (the with* functions) tamper with one field at a time for
// the negative tests.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"slices"
	"sync"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/stretchr/testify/require"
)

// algES384 is a COSE algorithm the package does not accept, for the
// "algorithm not allowed" rows.
const algES384 int64 = -35

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

var b64 = base64.RawURLEncoding.EncodeToString

// rsa2048 is generated once: a 2048-bit key takes long enough under the race
// detector that every RS256 row generating its own would dominate the run.
var rsa2048 = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

type authenticator struct {
	alg        int64
	signer     crypto.Signer
	credID     []byte
	aaguid     []byte
	userHandle []byte // learned at registration, returned with assertions
	counter    uint32
	counting   bool // false: a synced passkey, which reports 0 every time
	backup     bool // BE and BS: a synced passkey
	attachment string
	transports []string
}

// newAuthenticator makes an authenticator holding a fresh key for alg
// (AlgES256, AlgEdDSA, AlgRS256 or algES384). A counting authenticator (a
// security key) increments its counter with every assertion; one that does
// not (a synced passkey) reports 0 and sets the backup flags.
func newAuthenticator(t *testing.T, alg int64, counting bool) *authenticator {
	t.Helper()
	var signer crypto.Signer
	var err error
	switch alg {
	case -7:
		signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case algES384:
		signer, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case -8:
		_, signer, err = ed25519.GenerateKey(rand.Reader)
	case -257:
		signer, err = rsa2048()
	default:
		t.Fatalf("virtual authenticator: unsupported algorithm %d", alg)
	}
	require.NoError(t, err)
	return newAuthenticatorWithKey(t, alg, signer, counting)
}

func newAuthenticatorWithKey(t *testing.T, alg int64, signer crypto.Signer, counting bool) *authenticator {
	t.Helper()
	a := &authenticator{
		alg:        alg,
		signer:     signer,
		credID:     randomBytes(t, 32),
		aaguid:     make([]byte, 16),
		counting:   counting,
		backup:     !counting,
		attachment: "cross-platform",
		transports: []string{"usb", "nfc"},
	}
	if !counting {
		a.attachment = "platform"
		a.transports = []string{"internal", "hybrid"}
	}
	return a
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// coseKey encodes the public key as a COSE_Key.
func (a *authenticator) coseKey(t *testing.T) []byte {
	t.Helper()
	return coseKeyFor(t, a.alg, a.signer.Public())
}

func coseKeyFor(t *testing.T, alg int64, pub crypto.PublicKey) []byte {
	t.Helper()
	var key map[int]any
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		point, err := k.Bytes()
		require.NoError(t, err)
		size := (len(point) - 1) / 2
		crv := 1 // P-256
		if k.Curve == elliptic.P384() {
			crv = 2
		}
		key = map[int]any{1: 2, 3: alg, -1: crv, -2: point[1 : 1+size], -3: point[1+size:]}
	case ed25519.PublicKey:
		key = map[int]any{1: 1, 3: alg, -1: 6, -2: []byte(k)}
	case *rsa.PublicKey:
		key = map[int]any{1: 3, 3: alg, -1: k.N.Bytes(), -2: big.NewInt(int64(k.E)).Bytes()}
	default:
		t.Fatalf("virtual authenticator: unsupported key %T", pub)
	}
	out, err := webauthncbor.Marshal(key)
	require.NoError(t, err)
	return out
}

// sign signs msg as alg does: ES256/ES384 over a SHA-2 digest (DER), EdDSA
// over the message itself, RS256 PKCS#1 v1.5 over SHA-256.
func sign(t *testing.T, alg int64, signer crypto.Signer, msg []byte) []byte {
	t.Helper()
	var sig []byte
	var err error
	switch k := signer.(type) {
	case *ecdsa.PrivateKey:
		h := crypto.SHA256
		if alg == algES384 {
			h = crypto.SHA384
		}
		d := h.New()
		d.Write(msg)
		sig, err = ecdsa.SignASN1(rand.Reader, k, d.Sum(nil))
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, msg)
	case *rsa.PrivateKey:
		digest := sha256.Sum256(msg)
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	default:
		t.Fatalf("virtual authenticator: unsupported signer %T", signer)
	}
	require.NoError(t, err)
	return sig
}

// ceremony is everything one response is built from; a ceremonyOption
// changes one part of it before it is encoded.
type ceremony struct {
	typ        string
	challenge  []byte
	origin     string
	rpID       string
	flags      byte
	counter    uint32
	userHandle []byte
	coseKey    []byte // registration only
	signAlg    int64  // assertion only
	signer     crypto.Signer
	corruptSig bool
}

type ceremonyOption func(*ceremony)

func withOrigin(origin string) ceremonyOption { return func(c *ceremony) { c.origin = origin } }
func withType(typ string) ceremonyOption      { return func(c *ceremony) { c.typ = typ } }
func withChallenge(ch []byte) ceremonyOption  { return func(c *ceremony) { c.challenge = ch } }
func withRPID(rpID string) ceremonyOption     { return func(c *ceremony) { c.rpID = rpID } }
func withoutFlag(f byte) ceremonyOption       { return func(c *ceremony) { c.flags &^= f } }
func withFlag(f byte) ceremonyOption          { return func(c *ceremony) { c.flags |= f } }
func withCounter(n uint32) ceremonyOption     { return func(c *ceremony) { c.counter = n } }
func withUserHandle(h []byte) ceremonyOption  { return func(c *ceremony) { c.userHandle = h } }
func withCorruptSignature() ceremonyOption    { return func(c *ceremony) { c.corruptSig = true } }

// withSigner signs the assertion with another authenticator's key and
// algorithm (the credential's own key did not make the signature).
func withSigner(other *authenticator) ceremonyOption {
	return func(c *ceremony) { c.signer, c.signAlg = other.signer, other.alg }
}

func (c ceremony) clientDataJSON(t *testing.T) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"type":        c.typ,
		"challenge":   b64(c.challenge),
		"origin":      c.origin,
		"crossOrigin": false,
	})
	require.NoError(t, err)
	return out
}

// authData is rpIdHash | flags | signCount, then for a registration the
// attested credential data: aaguid | credIdLen | credId | COSE key.
func (c ceremony) authData(a *authenticator, attested bool) []byte {
	rpIDHash := sha256.Sum256([]byte(c.rpID))
	out := slices.Concat(rpIDHash[:], []byte{c.flags}, binary.BigEndian.AppendUint32(nil, c.counter))
	if attested {
		out = slices.Concat(out, a.aaguid, binary.BigEndian.AppendUint16(nil, uint16(len(a.credID))), a.credID, c.coseKey)
	}
	return out
}

type creationOptions struct {
	PublicKey struct {
		RP struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"rp"`
		User struct {
			ID          protocol.URLEncodedBase64 `json:"id"`
			Name        string                    `json:"name"`
			DisplayName string                    `json:"displayName"`
		} `json:"user"`
		Challenge        protocol.URLEncodedBase64 `json:"challenge"`
		PubKeyCredParams []struct {
			Type string `json:"type"`
			Alg  int64  `json:"alg"`
		} `json:"pubKeyCredParams"`
		ExcludeCredentials []struct {
			Type string                    `json:"type"`
			ID   protocol.URLEncodedBase64 `json:"id"`
		} `json:"excludeCredentials"`
		AuthenticatorSelection map[string]any `json:"authenticatorSelection"`
		Attestation            string         `json:"attestation"`
	} `json:"publicKey"`
}

type requestOptions struct {
	PublicKey struct {
		Challenge        protocol.URLEncodedBase64 `json:"challenge"`
		RPID             string                    `json:"rpId"`
		UserVerification string                    `json:"userVerification"`
		AllowCredentials []struct {
			Type       string                    `json:"type"`
			ID         protocol.URLEncodedBase64 `json:"id"`
			Transports []string                  `json:"transports"`
		} `json:"allowCredentials"`
	} `json:"publicKey"`
}

// create answers navigator.credentials.create(options) from origin with a
// "none" attestation, as a browser does when no attestation is requested.
func (a *authenticator) create(t *testing.T, optionsJSON []byte, origin string, opts ...ceremonyOption) []byte {
	t.Helper()
	var o creationOptions
	require.NoError(t, json.Unmarshal(optionsJSON, &o))
	a.userHandle = slices.Clone(o.PublicKey.User.ID)
	flags := byte(flagUP | flagUV | flagAT)
	if a.backup {
		flags |= flagBE | flagBS
	}
	c := ceremony{
		typ:       "webauthn.create",
		challenge: o.PublicKey.Challenge,
		origin:    origin,
		rpID:      o.PublicKey.RP.ID,
		flags:     flags,
		counter:   a.counter,
		coseKey:   a.coseKey(t),
	}
	for _, opt := range opts {
		opt(&c)
	}
	clientData := c.clientDataJSON(t)
	authData := c.authData(a, true)
	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	require.NoError(t, err)
	out, err := json.Marshal(map[string]any{
		"id":                      b64(a.credID),
		"rawId":                   b64(a.credID),
		"type":                    "public-key",
		"authenticatorAttachment": a.attachment,
		"response": map[string]any{
			"clientDataJSON":     b64(clientData),
			"attestationObject":  b64(attObj),
			"authenticatorData":  b64(authData),
			"publicKeyAlgorithm": a.alg,
			"transports":         a.transports,
		},
		"clientExtensionResults": map[string]any{},
	})
	require.NoError(t, err)
	return out
}

// get answers navigator.credentials.get(options) from origin. It requires its
// credential to be among the allowed ones, as a real authenticator would.
func (a *authenticator) get(t *testing.T, optionsJSON []byte, origin string, opts ...ceremonyOption) []byte {
	t.Helper()
	var o requestOptions
	require.NoError(t, json.Unmarshal(optionsJSON, &o))
	allowed := false
	for _, c := range o.PublicKey.AllowCredentials {
		allowed = allowed || slices.Equal([]byte(c.ID), a.credID)
	}
	require.True(t, allowed, "virtual authenticator: its credential is not in allowCredentials")
	if a.counting {
		a.counter++
	}
	flags := byte(flagUP | flagUV)
	if a.backup {
		flags |= flagBE | flagBS
	}
	c := ceremony{
		typ:        "webauthn.get",
		challenge:  o.PublicKey.Challenge,
		origin:     origin,
		rpID:       o.PublicKey.RPID,
		flags:      flags,
		counter:    a.counter,
		userHandle: a.userHandle,
		signAlg:    a.alg,
		signer:     a.signer,
	}
	for _, opt := range opts {
		opt(&c)
	}
	clientData := c.clientDataJSON(t)
	authData := c.authData(a, false)
	clientDataHash := sha256.Sum256(clientData)
	sig := sign(t, c.signAlg, c.signer, slices.Concat(authData, clientDataHash[:]))
	if c.corruptSig {
		sig[len(sig)/2] ^= 0xff
	}
	response := map[string]any{
		"clientDataJSON":    b64(clientData),
		"authenticatorData": b64(authData),
		"signature":         b64(sig),
	}
	if c.userHandle != nil {
		response["userHandle"] = b64(c.userHandle)
	}
	out, err := json.Marshal(map[string]any{
		"id":                      b64(a.credID),
		"rawId":                   b64(a.credID),
		"type":                    "public-key",
		"authenticatorAttachment": a.attachment,
		"response":                response,
		"clientExtensionResults":  map[string]any{},
	})
	require.NoError(t, err)
	return out
}
