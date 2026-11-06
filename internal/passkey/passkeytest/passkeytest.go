// Package passkeytest is a virtual WebAuthn authenticator for tests outside
// internal/passkey: an ES256 key that answers the options BeginRegistration
// and BeginAssertion produce with the JSON a browser's
// PublicKeyCredential.toJSON() would send. It builds clientDataJSON,
// authenticatorData and a "none" attestationObject itself. It is test support
// only -- nothing outside a _test.go file imports it -- and internal/passkey's
// own tests keep a richer one that can tamper with every field.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

var b64 = base64.RawURLEncoding.EncodeToString

// Authenticator is one virtual authenticator holding one ES256 credential.
type Authenticator struct {
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	counter    uint32
	// Counting makes it count signatures like a hardware key; false is a
	// synced passkey, which reports 0 every time.
	Counting bool
	// Synced sets the backup-eligible and backup-state flags.
	Synced bool
}

// New returns an authenticator with a fresh key and credential id.
func New(counting bool) (*Authenticator, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return &Authenticator{key: key, credID: id, Counting: counting, Synced: !counting}, nil
}

// CredentialID is the authenticator's credential id.
func (a *Authenticator) CredentialID() []byte { return slices.Clone(a.credID) }

func (a *Authenticator) flags() byte {
	f := byte(flagUP | flagUV)
	if a.Synced {
		f |= flagBE | flagBS
	}
	return f
}

func (a *Authenticator) coseKey() ([]byte, error) {
	point, err := a.key.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: point[1:33], -3: point[33:]})
}

func clientData(typ string, challenge []byte, origin string) ([]byte, error) {
	return json.Marshal(map[string]any{"type": typ, "challenge": b64(challenge), "origin": origin, "crossOrigin": false})
}

func authData(rpID string, flags byte, counter uint32, attested []byte) []byte {
	h := sha256.Sum256([]byte(rpID))
	return slices.Concat(h[:], []byte{flags}, binary.BigEndian.AppendUint32(nil, counter), attested)
}

// Create answers navigator.credentials.create(options) from origin.
func (a *Authenticator) Create(optionsJSON []byte, origin string) ([]byte, error) {
	var o struct {
		PublicKey struct {
			RP struct {
				ID string `json:"id"`
			} `json:"rp"`
			User struct {
				ID protocol.URLEncodedBase64 `json:"id"`
			} `json:"user"`
			Challenge          protocol.URLEncodedBase64 `json:"challenge"`
			ExcludeCredentials []struct {
				ID protocol.URLEncodedBase64 `json:"id"`
			} `json:"excludeCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(optionsJSON, &o); err != nil {
		return nil, err
	}
	for _, ex := range o.PublicKey.ExcludeCredentials {
		if slices.Equal([]byte(ex.ID), a.credID) {
			return nil, errors.New("passkeytest: this authenticator is excluded (InvalidStateError)")
		}
	}
	a.userHandle = slices.Clone(o.PublicKey.User.ID)
	cose, err := a.coseKey()
	if err != nil {
		return nil, err
	}
	attested := slices.Concat(make([]byte, 16), binary.BigEndian.AppendUint16(nil, uint16(len(a.credID))), a.credID, cose)
	ad := authData(o.PublicKey.RP.ID, a.flags()|flagAT, a.counter, attested)
	cd, err := clientData("webauthn.create", o.PublicKey.Challenge, origin)
	if err != nil {
		return nil, err
	}
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key", "authenticatorAttachment": "cross-platform",
		"response": map[string]any{
			"clientDataJSON": b64(cd), "attestationObject": b64(att), "authenticatorData": b64(ad),
			"publicKeyAlgorithm": -7, "transports": []string{"usb"},
		},
		"clientExtensionResults": map[string]any{},
	})
}

// Get answers navigator.credentials.get(options) from origin. Like a real
// authenticator it refuses when its credential is not allowed.
func (a *Authenticator) Get(optionsJSON []byte, origin string) ([]byte, error) {
	var o struct {
		PublicKey struct {
			Challenge        protocol.URLEncodedBase64 `json:"challenge"`
			RPID             string                    `json:"rpId"`
			AllowCredentials []struct {
				ID protocol.URLEncodedBase64 `json:"id"`
			} `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(optionsJSON, &o); err != nil {
		return nil, err
	}
	allowed := len(o.PublicKey.AllowCredentials) == 0
	for _, c := range o.PublicKey.AllowCredentials {
		allowed = allowed || slices.Equal([]byte(c.ID), a.credID)
	}
	if !allowed {
		return nil, fmt.Errorf("passkeytest: credential %s is not allowed", b64(a.credID))
	}
	if a.Counting {
		a.counter++
	}
	ad := authData(o.PublicKey.RPID, a.flags(), a.counter, nil)
	cd, err := clientData("webauthn.get", o.PublicKey.Challenge, origin)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(slices.Concat(ad, h[:]))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key", "authenticatorAttachment": "cross-platform",
		"response": map[string]any{
			"clientDataJSON": b64(cd), "authenticatorData": b64(ad), "signature": b64(sig), "userHandle": b64(a.userHandle),
		},
		"clientExtensionResults": map[string]any{},
	})
}
