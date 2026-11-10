// Package browserproof is how a browser proves, on each console request, that
// it is the browser a console unlock was given to.
//
// A browser sends its cookies to every port of the host that set them, so a
// program running as the owner can listen on another local port, open the
// owner's browser there, and receive the console's cookies (task
// 01M73SVP8HWD6KPF3ZC29RG6CT, confirmed in Safari, Chrome and Firefox). A
// cookie alone therefore cannot name a browser. Storage is different: IndexedDB
// belongs to one origin, scheme, host and port together. So the console's page
// keeps a non-extractable ECDSA P-256 key there (static/browserkey.js), and the
// console's service worker (static/sw.js) signs every request the console's
// own pages make with it. The unlock's stored hash covers the cookie and this
// key together (store.HashBrowserUnlock), so a cookie read on another port
// names no unlock without a proof only this origin can make.
//
// A proof is one header:
//
//	Seamless-Browser-Proof: v1.<unix seconds>.<key>.<signature>
//
// key is the public key, unpadded base64url of its SPKI DER; signature is the
// unpadded base64url of the 64-byte r||s (IEEE P1363, what WebCrypto returns)
// over SHA-256 of Message. It names the time, not the request: when the service
// worker follows a redirect for a page's fetch, the header travels to the new
// path, and a proof bound to the first path would fail there. It never leaves
// the origin. The worker signs only same-origin requests, a page's fetch is
// sent same-origin so a redirect elsewhere fails, and a navigation's redirect
// is a new navigation the worker signs again. A proof stands for Window either
// side of the daemon's clock.
//
// Nothing here is a secret the daemon keeps: it stores only hashes that cover
// the public key, and the private key never leaves the browser.
package browserproof

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Header carries a request's proof.
const Header = "Seamless-Browser-Proof"

// Window is how far a proof's time may stand from the daemon's clock, either
// way.
const Window = 5 * time.Minute

// maxKeyChars bounds the key part: a P-256 SPKI is 91 bytes, 122 characters.
const maxKeyChars = 160

var (
	// ErrNone is a request with no proof.
	ErrNone = errors.New("no browser proof")
	// ErrClock is a proof that verifies but whose time is more than Window
	// from the daemon's: the browser's clock is off.
	ErrClock = errors.New("the browser's clock is too far from the daemon's")
	// ErrInvalid is a proof that does not parse or does not verify.
	ErrInvalid = errors.New("invalid browser proof")
)

// Message is what a browser signs for a proof made at unix.
func Message(unix int64) []byte {
	return []byte("seamless browser proof v1\n" + strconv.FormatInt(unix, 10))
}

// EncodeKey is a public key as a proof names it: unpadded base64url SPKI DER.
func EncodeKey(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("browserproof: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(der), nil
}

// parseKey reads a key part: canonical unpadded base64url of the canonical
// SPKI DER of a P-256 key, so one key has exactly one spelling.
func parseKey(s string) (*ecdsa.PublicKey, error) {
	if s == "" || len(s) > maxKeyChars {
		return nil, ErrInvalid
	}
	der, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, ErrInvalid
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, ErrInvalid
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, ErrInvalid
	}
	again, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil || !bytes.Equal(again, der) {
		return nil, ErrInvalid
	}
	return pub, nil
}

// Verify checks a proof header against now and returns the key it verifies
// under. An empty header is ErrNone; a valid signature at a time outside
// Window is ErrClock, so the caller can say the browser's clock is off;
// anything else that fails is ErrInvalid. Every error returns no key.
func Verify(header string, now time.Time) (string, error) {
	if header == "" {
		return "", ErrNone
	}
	parts := strings.Split(header, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return "", ErrInvalid
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || parts[1] != strconv.FormatInt(unix, 10) {
		return "", ErrInvalid
	}
	pub, err := parseKey(parts[2])
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[3])
	if err != nil || len(sig) != 64 {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(Message(unix))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, sum[:], r, s) {
		return "", ErrInvalid
	}
	at := time.Unix(unix, 0)
	if at.Before(now.Add(-Window)) || at.After(now.Add(Window)) {
		return "", ErrClock
	}
	return parts[2], nil
}

// Sign makes the proof a browser holding priv sends at now: what the
// console's service worker computes, for tests and for any Go client that
// stands in for a browser.
func Sign(priv *ecdsa.PrivateKey, now time.Time) (string, error) {
	key, err := EncodeKey(&priv.PublicKey)
	if err != nil {
		return "", err
	}
	unix := now.Unix()
	sum := sha256.Sum256(Message(unix))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return "", fmt.Errorf("browserproof: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "v1." + strconv.FormatInt(unix, 10) + "." + key + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// NewKey is a fresh P-256 key, as a browser's WebCrypto makes one.
func NewKey() (*ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("browserproof: %w", err)
	}
	return priv, nil
}
