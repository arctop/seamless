// Package sshsigtest makes SSHSIG signatures with software keys, for tests
// of code that verifies them (internal/sshsig and its callers). A hardware
// security key cannot sign in a test, so its signatures are built the way
// one builds them -- the application's hash, the flags byte, the counter
// and the data's hash, signed with the key -- from a software key of the
// same algorithm, with whatever flags the test asks for. Never import it
// from code that ships.
package sshsigtest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"math/big"
	"strings"

	"github.com/arctop/seamless/internal/sshsig"
)

// Key is a throwaway signing key of one sshsig key type.
type Key struct {
	Type        string
	Application string // security keys only; "ssh:" by default
	ed          ed25519.PrivateKey
	ec          *ecdsa.PrivateKey
}

// NewKey makes a key of keyType (one of sshsig.KeyTypes). A security key's
// application is "ssh:", the default ssh-keygen gives one.
func NewKey(keyType string) (*Key, error) {
	k := &Key{Type: keyType}
	switch keyType {
	case sshsig.KeyEd25519, sshsig.KeySKEd25519:
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		k.ed = priv
	case sshsig.KeyECDSAP256, sshsig.KeySKECDSAP256, sshsig.KeyECDSAP384, sshsig.KeyECDSAP521:
		priv, err := ecdsa.GenerateKey(curveOf(keyType), rand.Reader)
		if err != nil {
			return nil, err
		}
		k.ec = priv
	default:
		return nil, fmt.Errorf("sshsigtest: unsupported key type %q", keyType)
	}
	if strings.HasPrefix(keyType, "sk-") {
		k.Application = "ssh:"
	}
	return k, nil
}

// FromEd25519Seed makes an ssh-ed25519 or sk-ssh-ed25519 key from a 32-byte
// seed, for a fixed test vector.
func FromEd25519Seed(keyType string, seed []byte) (*Key, error) {
	if keyType != sshsig.KeyEd25519 && keyType != sshsig.KeySKEd25519 {
		return nil, fmt.Errorf("sshsigtest: %q is not an ed25519 key type", keyType)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("sshsigtest: seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	k := &Key{Type: keyType, ed: ed25519.NewKeyFromSeed(seed)}
	if keyType == sshsig.KeySKEd25519 {
		k.Application = "ssh:"
	}
	return k, nil
}

func curveOf(keyType string) elliptic.Curve {
	switch keyType {
	case sshsig.KeyECDSAP384:
		return elliptic.P384()
	case sshsig.KeyECDSAP521:
		return elliptic.P521()
	}
	return elliptic.P256()
}

func curveName(keyType string) string {
	switch keyType {
	case sshsig.KeyECDSAP384:
		return "nistp384"
	case sshsig.KeyECDSAP521:
		return "nistp521"
	}
	return "nistp256"
}

func hashOf(keyType string) func() hash.Hash {
	switch keyType {
	case sshsig.KeyECDSAP384:
		return sha512.New384
	case sshsig.KeyECDSAP521:
		return sha512.New
	}
	return sha256.New
}

// security reports whether k is a security key type.
func (k *Key) security() bool { return strings.HasPrefix(k.Type, "sk-") }

// PublicBlob is the public key's wire encoding.
func (k *Key) PublicBlob() []byte {
	var b bytes.Buffer
	str(&b, []byte(k.Type))
	if k.ed != nil {
		str(&b, k.ed.Public().(ed25519.PublicKey))
	} else {
		str(&b, []byte(curveName(k.Type)))
		pub, err := k.ec.PublicKey.Bytes()
		if err != nil {
			panic(err) // a generated key always encodes
		}
		str(&b, pub)
	}
	if k.security() {
		str(&b, []byte(k.Application))
	}
	return b.Bytes()
}

// AuthorizedKey is the key as an authorized_keys or allowed_signers line
// writes it: "type base64".
func (k *Key) AuthorizedKey() string {
	return k.Type + " " + base64.StdEncoding.EncodeToString(k.PublicBlob())
}

// SignersLine is an allowed_signers line for k: principal, then
// namespaces="..." when namespaces is not empty, then the key.
func (k *Key) SignersLine(principal, namespaces string) string {
	if namespaces == "" {
		return principal + " " + k.AuthorizedKey()
	}
	return fmt.Sprintf("%s namespaces=%q %s", principal, namespaces, k.AuthorizedKey())
}

// Fingerprint is the key's SHA256 fingerprint.
func (k *Key) Fingerprint() string {
	sum := sha256.Sum256(k.PublicBlob())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Options shape one signature. The zero value is a sha512 signature, as
// ssh-keygen makes, with a security key's flags 0x05 (touched and verified)
// and counter 1.
type Options struct {
	HashAlgorithm string // "sha512" when empty
	Flags         *byte  // a security key's flags; 0x05 when nil
	Counter       uint32 // a security key's counter; 1 when zero
}

// Sign signs message for namespace and returns the armored SSHSIG.
func (k *Key) Sign(namespace string, message []byte, o Options) ([]byte, error) {
	alg := o.HashAlgorithm
	if alg == "" {
		alg = "sha512"
	}
	data, err := sshsig.SignedData(namespace, alg, message)
	if err != nil {
		return nil, err
	}
	flags := sshsig.FlagUserPresence | sshsig.FlagUserVerification
	if o.Flags != nil {
		flags = *o.Flags
	}
	counter := o.Counter
	if counter == 0 {
		counter = 1
	}
	signed := data
	if k.security() {
		signed = sshsig.SecurityKeyData(k.Application, flags, counter, data)
	}
	var sigBytes []byte
	if k.ed != nil {
		sigBytes = ed25519.Sign(k.ed, signed)
	} else {
		h := hashOf(k.Type)()
		h.Write(signed)
		r, s, err := ecdsa.Sign(rand.Reader, k.ec, h.Sum(nil))
		if err != nil {
			return nil, err
		}
		var rs bytes.Buffer
		mpint(&rs, r)
		mpint(&rs, s)
		sigBytes = rs.Bytes()
	}
	var sig bytes.Buffer
	str(&sig, []byte(k.Type))
	str(&sig, sigBytes)
	if k.security() {
		sig.WriteByte(flags)
		var c [4]byte
		binary.BigEndian.PutUint32(c[:], counter)
		sig.Write(c[:])
	}
	return Armor(Blob(k.PublicBlob(), namespace, alg, sig.Bytes())), nil
}

// Blob assembles an SSHSIG blob from its fields, for tests that need a
// malformed one too.
func Blob(publicKey []byte, namespace, hashAlgorithm string, signature []byte) []byte {
	var b bytes.Buffer
	b.WriteString("SSHSIG")
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], 1)
	b.Write(v[:])
	str(&b, publicKey)
	str(&b, []byte(namespace))
	str(&b, nil)
	str(&b, []byte(hashAlgorithm))
	str(&b, signature)
	return b.Bytes()
}

// Armor wraps a blob the way ssh-keygen does: 70 base64 characters a line.
func Armor(blob []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return []byte(b.String())
}

// Flags is a pointer to f, for Options.Flags.
func Flags(f byte) *byte { return &f }

func str(b *bytes.Buffer, s []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.Write(s)
}

func mpint(b *bytes.Buffer, x *big.Int) {
	raw := x.Bytes()
	if len(raw) > 0 && raw[0]&0x80 != 0 {
		raw = append([]byte{0}, raw...)
	}
	str(b, raw)
}
