// Package sshsig verifies OpenSSH signatures over files: the SSHSIG format
// `ssh-keygen -Y sign` writes (OpenSSH's PROTOCOL.sshsig), checked against
// an allowed_signers file (ssh-keygen(1), ALLOWED SIGNERS), in Go and with
// no ssh-keygen to shell out to.
//
// It covers the key types a release is signed with: hardware security keys
// (sk-ssh-ed25519@openssh.com, sk-ecdsa-sha2-nistp256@openssh.com), whose
// signatures also carry the authenticator's flags and counter, and the plain
// ssh-ed25519 and ecdsa-sha2-nistp256/384/521 keys. Any other key type, a
// certificate, or an RSA key is refused rather than skipped, so a file that
// names one fails loudly instead of trusting fewer keys than it lists.
//
// The package is pure: no network, no files, no clock. The standard library
// does the cryptography.
package sshsig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"slices"
	"strings"
)

// The key types this package verifies, as OpenSSH names them on the wire.
const (
	KeyEd25519     = "ssh-ed25519"
	KeySKEd25519   = "sk-ssh-ed25519@openssh.com"
	KeyECDSAP256   = "ecdsa-sha2-nistp256"
	KeyECDSAP384   = "ecdsa-sha2-nistp384"
	KeyECDSAP521   = "ecdsa-sha2-nistp521"
	KeySKECDSAP256 = "sk-ecdsa-sha2-nistp256@openssh.com"
)

// KeyTypes lists every key type this package verifies.
var KeyTypes = []string{KeyEd25519, KeySKEd25519, KeyECDSAP256, KeyECDSAP384, KeyECDSAP521, KeySKECDSAP256}

// The authenticator flags a security key's signature carries (the FIDO
// flags byte, as OpenSSH's sk-api.h names them).
const (
	// FlagUserPresence: the key's owner touched it for this signature.
	FlagUserPresence byte = 0x01
	// FlagUserVerification: the key verified its owner (a PIN or a
	// biometric) for this signature.
	FlagUserVerification byte = 0x04
)

const (
	// magicPreamble opens both the signature blob and the data a key signs.
	magicPreamble = "SSHSIG"
	// sigVersion is the only SSHSIG version there is.
	sigVersion = 1
	armorBegin = "-----BEGIN SSH SIGNATURE-----"
	armorEnd   = "-----END SSH SIGNATURE-----"
	// MaxArmorBytes bounds an armored signature. A real one is well under a
	// kilobyte; anything this large is not one.
	MaxArmorBytes = 16 << 10
)

// Errors. Every failure wraps exactly one of them, so a caller can say why
// in a fixed word (errors.Is) while the message names the detail.
var (
	// ErrMalformed: the signature is not a well-formed SSHSIG blob in its
	// armor, or uses a hash or key type this package does not verify.
	ErrMalformed = errors.New("sshsig: malformed signature")
	// ErrNamespace: the signature was made for another namespace than the
	// one asked for, or its key is listed for other namespaces only.
	ErrNamespace = errors.New("sshsig: wrong namespace")
	// ErrUnlisted: the key that made the signature is not an allowed signer.
	ErrUnlisted = errors.New("sshsig: the signing key is not an allowed signer")
	// ErrBadSignature: the signature does not verify over the message with
	// its key -- another file, or altered bytes.
	ErrBadSignature = errors.New("sshsig: the signature does not verify")
	// ErrFlags: a security key's signature lacks a flag the policy requires
	// (user presence, user verification).
	ErrFlags = errors.New("sshsig: the security key's flags do not meet the policy")
	// ErrSigners: an allowed_signers file this package will not use.
	ErrSigners = errors.New("sshsig: invalid allowed_signers")
)

// PublicKey is a parsed public key of one of KeyTypes.
type PublicKey struct {
	keyType     string
	blob        []byte // its wire encoding, which signatures compare against
	ed          ed25519.PublicKey
	ec          *ecdsa.PublicKey
	application string // a security key's FIDO application ("ssh:..."), else ""
}

// Type is the key's OpenSSH type name.
func (k *PublicKey) Type() string { return k.keyType }

// Marshal is the key's wire encoding (the blob an authorized_keys or
// allowed_signers line carries in base64).
func (k *PublicKey) Marshal() []byte { return slices.Clone(k.blob) }

// Fingerprint is the key's SHA256 fingerprint as ssh-keygen -l prints it:
// "SHA256:" and the unpadded base64 of the SHA-256 of its wire encoding.
func (k *PublicKey) Fingerprint() string {
	sum := sha256.Sum256(k.blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// SecurityKey reports whether the key lives on a hardware security key (an
// sk- type), whose signatures carry flags and a counter.
func (k *PublicKey) SecurityKey() bool { return k.application != "" }

// Application is a security key's FIDO application, "" for any other key.
func (k *PublicKey) Application() string { return k.application }

// ecCurve is the curve and digest of an ECDSA key type.
func ecCurve(keyType string) (curve elliptic.Curve, name string, h func() hash.Hash, ok bool) {
	switch keyType {
	case KeyECDSAP256, KeySKECDSAP256:
		return elliptic.P256(), "nistp256", sha256.New, true
	case KeyECDSAP384:
		return elliptic.P384(), "nistp384", sha512.New384, true
	case KeyECDSAP521:
		return elliptic.P521(), "nistp521", sha512.New, true
	}
	return nil, "", nil, false
}

// ParsePublicKey parses a public key's wire encoding. A key type outside
// KeyTypes, a certificate among them, is refused.
func ParsePublicKey(blob []byte) (*PublicKey, error) {
	r := reader{buf: blob}
	keyType, err := r.str()
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	k := &PublicKey{keyType: string(keyType), blob: slices.Clone(blob)}
	switch k.keyType {
	case KeyEd25519, KeySKEd25519:
		pub, err := r.str()
		if err != nil {
			return nil, fmt.Errorf("%s public key: %w", k.keyType, err)
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s public key: %d bytes, want %d", k.keyType, len(pub), ed25519.PublicKeySize)
		}
		k.ed = ed25519.PublicKey(slices.Clone(pub))
	case KeyECDSAP256, KeyECDSAP384, KeyECDSAP521, KeySKECDSAP256:
		curve, name, _, _ := ecCurve(k.keyType)
		gotName, err := r.str()
		if err != nil {
			return nil, fmt.Errorf("%s public key: %w", k.keyType, err)
		}
		if string(gotName) != name {
			return nil, fmt.Errorf("%s public key names curve %q, want %q", k.keyType, gotName, name)
		}
		point, err := r.str()
		if err != nil {
			return nil, fmt.Errorf("%s public key: %w", k.keyType, err)
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return nil, fmt.Errorf("%s public key: %w", k.keyType, err)
		}
		k.ec = pub
	default:
		return nil, fmt.Errorf("unsupported key type %q (supported: %s)", k.keyType, strings.Join(KeyTypes, ", "))
	}
	if k.keyType == KeySKEd25519 || k.keyType == KeySKECDSAP256 {
		app, err := r.str()
		if err != nil {
			return nil, fmt.Errorf("%s public key: application: %w", k.keyType, err)
		}
		if len(app) == 0 {
			return nil, fmt.Errorf("%s public key names no application", k.keyType)
		}
		k.application = string(app)
	}
	if !r.done() {
		return nil, fmt.Errorf("%s public key: %d trailing bytes", k.keyType, len(r.buf))
	}
	return k, nil
}

// Signature is a parsed SSHSIG signature, not yet verified.
type Signature struct {
	// Key is the public key the signature names as its signer. Nothing
	// vouches for it until Verify finds it among the allowed signers.
	Key *PublicKey
	// Namespace is the namespace it was made for (ssh-keygen -Y sign -n).
	Namespace string
	// HashAlgorithm is the hash of the message it signs: "sha256" or "sha512".
	HashAlgorithm string
	// Flags and Counter are a security key's authenticator flags and
	// signature counter; both zero for any other key.
	Flags   byte
	Counter uint32

	sigType string
	sig     []byte // ed25519: 64 bytes; ecdsa: the (mpint r, mpint s) blob
}

// Unarmor decodes the ASCII armor ssh-keygen writes around a signature: one
// BEGIN line, base64, one END line, and only whitespace around them.
func Unarmor(armored []byte) ([]byte, error) {
	if len(armored) > MaxArmorBytes {
		return nil, fmt.Errorf("%w: %d bytes is too large for a signature", ErrMalformed, len(armored))
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(armored), "\r\n", "\n"))
	body, ok := strings.CutPrefix(text, armorBegin)
	if !ok {
		return nil, fmt.Errorf("%w: it does not begin with %q", ErrMalformed, armorBegin)
	}
	body, ok = strings.CutSuffix(body, armorEnd)
	if !ok {
		return nil, fmt.Errorf("%w: it does not end with %q", ErrMalformed, armorEnd)
	}
	if strings.Contains(body, "-----") {
		return nil, fmt.Errorf("%w: it holds more than one armored block", ErrMalformed)
	}
	b64 := strings.Join(strings.Fields(body), "")
	raw, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %w", ErrMalformed, err)
	}
	return raw, nil
}

// ParseSignature parses an armored SSHSIG signature, as `ssh-keygen -Y sign`
// writes it: version 1, a supported key, a sha256 or sha512 message hash, an
// empty reserved field, a signature of the key's own type, and not one byte
// more.
func ParseSignature(armored []byte) (*Signature, error) {
	raw, err := Unarmor(armored)
	if err != nil {
		return nil, err
	}
	r := reader{buf: raw}
	magic, err := r.take(len(magicPreamble))
	if err != nil || string(magic) != magicPreamble {
		return nil, fmt.Errorf("%w: it does not start with %q", ErrMalformed, magicPreamble)
	}
	version, err := r.u32()
	if err != nil {
		return nil, fmt.Errorf("%w: version: %w", ErrMalformed, err)
	}
	if version != sigVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrMalformed, version, sigVersion)
	}
	var fields [5][]byte
	for i, name := range []string{"public key", "namespace", "reserved", "hash algorithm", "signature"} {
		if fields[i], err = r.str(); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrMalformed, name, err)
		}
	}
	if !r.done() {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrMalformed, len(r.buf))
	}
	key, err := ParsePublicKey(fields[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	s := &Signature{Key: key, Namespace: string(fields[1]), HashAlgorithm: string(fields[3])}
	switch {
	case s.Namespace == "":
		return nil, fmt.Errorf("%w: empty namespace", ErrMalformed)
	case len(fields[2]) != 0:
		return nil, fmt.Errorf("%w: the reserved field is not empty", ErrMalformed)
	case s.HashAlgorithm != "sha256" && s.HashAlgorithm != "sha512":
		return nil, fmt.Errorf("%w: hash algorithm %q (want sha256 or sha512)", ErrMalformed, s.HashAlgorithm)
	}
	if err := s.parseSig(fields[4]); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return s, nil
}

// sigTypeOf is the signature type a key of keyType signs with.
func sigTypeOf(keyType string) string { return keyType }

// parseSig reads the signature field: string type, string signature, and for
// a security key byte flags and uint32 counter.
func (s *Signature) parseSig(field []byte) error {
	r := reader{buf: field}
	sigType, err := r.str()
	if err != nil {
		return fmt.Errorf("signature type: %w", err)
	}
	s.sigType = string(sigType)
	if want := sigTypeOf(s.Key.keyType); s.sigType != want {
		return fmt.Errorf("a %s key's signature has type %q, want %q", s.Key.keyType, s.sigType, want)
	}
	if s.sig, err = r.str(); err != nil {
		return fmt.Errorf("signature bytes: %w", err)
	}
	if s.Key.SecurityKey() {
		flags, err := r.take(1)
		if err != nil {
			return fmt.Errorf("security key flags: %w", err)
		}
		s.Flags = flags[0]
		if s.Counter, err = r.u32(); err != nil {
			return fmt.Errorf("security key counter: %w", err)
		}
	}
	if !r.done() {
		return fmt.Errorf("signature: %d trailing bytes", len(r.buf))
	}
	switch s.Key.keyType {
	case KeyEd25519, KeySKEd25519:
		if len(s.sig) != ed25519.SignatureSize {
			return fmt.Errorf("ed25519 signature is %d bytes, want %d", len(s.sig), ed25519.SignatureSize)
		}
	default:
		if _, _, err := ecdsaRS(s.sig); err != nil {
			return err
		}
	}
	return nil
}

// ecdsaRS reads an ECDSA signature blob: mpint r, mpint s, nothing more.
func ecdsaRS(blob []byte) (*big.Int, *big.Int, error) {
	r := reader{buf: blob}
	rb, err := r.mpint()
	if err != nil {
		return nil, nil, fmt.Errorf("ecdsa signature r: %w", err)
	}
	sb, err := r.mpint()
	if err != nil {
		return nil, nil, fmt.Errorf("ecdsa signature s: %w", err)
	}
	if !r.done() {
		return nil, nil, fmt.Errorf("ecdsa signature: %d trailing bytes", len(r.buf))
	}
	return rb, sb, nil
}

// SignedData is the data an SSHSIG signer's key signs for message: the magic
// preamble, the namespace, an empty reserved field, the hash algorithm, and
// the message's hash under it. A security key signs it in turn as its
// message (with its application, flags and counter).
func SignedData(namespace, hashAlgorithm string, message []byte) ([]byte, error) {
	var digest []byte
	switch hashAlgorithm {
	case "sha256":
		sum := sha256.Sum256(message)
		digest = sum[:]
	case "sha512":
		sum := sha512.Sum512(message)
		digest = sum[:]
	default:
		return nil, fmt.Errorf("%w: hash algorithm %q", ErrMalformed, hashAlgorithm)
	}
	var b bytes.Buffer
	b.WriteString(magicPreamble)
	for _, f := range [][]byte{[]byte(namespace), nil, []byte(hashAlgorithm), digest} {
		writeString(&b, f)
	}
	return b.Bytes(), nil
}

// SecurityKeyData is what a security key signs for data: SHA-256 of its
// application, the flags byte, the big-endian counter, and SHA-256 of data
// (OpenSSH's ssh-ed25519-sk.c and ssh-ecdsa-sk.c).
func SecurityKeyData(application string, flags byte, counter uint32, data []byte) []byte {
	app := sha256.Sum256([]byte(application))
	msg := sha256.Sum256(data)
	out := make([]byte, 0, 2*sha256.Size+5)
	out = append(out, app[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, counter)
	return append(out, msg[:]...)
}

// verifyCrypto checks the signature over message with its own key, nothing
// about who that key is.
func (s *Signature) verifyCrypto(message []byte) error {
	data, err := SignedData(s.Namespace, s.HashAlgorithm, message)
	if err != nil {
		return err
	}
	if s.Key.SecurityKey() {
		data = SecurityKeyData(s.Key.application, s.Flags, s.Counter, data)
	}
	switch s.Key.keyType {
	case KeyEd25519, KeySKEd25519:
		if !ed25519.Verify(s.Key.ed, data, s.sig) {
			return ErrBadSignature
		}
		return nil
	default:
		_, _, newHash, _ := ecCurve(s.Key.keyType)
		h := newHash()
		h.Write(data)
		rInt, sInt, err := ecdsaRS(s.sig)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrMalformed, err)
		}
		if !ecdsa.Verify(s.Key.ec, h.Sum(nil), rInt, sInt) {
			return ErrBadSignature
		}
		return nil
	}
}

// Policy is what Verify requires beyond a good signature by a listed key.
type Policy struct {
	// Namespace is the namespace the signature must have been made for, and
	// that the signer's allowed_signers line must allow. Required.
	Namespace string
	// SecurityKeyFlags are the flags a security key's signature must carry
	// (FlagUserPresence, FlagUserVerification); every one of them must be
	// set. Other keys carry no flags, and this does not apply to them.
	SecurityKeyFlags byte
}

// Verified is a signature Verify accepted.
type Verified struct {
	// Signer is the allowed_signers line whose key made the signature.
	Signer Signer
	// Fingerprint is the signing key's SHA256 fingerprint.
	Fingerprint   string
	KeyType       string
	HashAlgorithm string
	// Flags and Counter are a security key's; zero for any other key.
	Flags   byte
	Counter uint32
}

// Verify checks that armored is an SSHSIG signature over message, made for
// policy's namespace by a key that signers allows for that namespace, and
// that a security key's signature carries the flags policy requires. The
// checks run in that order -- the signature parses, its namespace, its key
// listed (and allowed the namespace), the signature itself, the flags -- and
// the first that fails is the error (ErrMalformed, ErrNamespace, ErrUnlisted,
// ErrBadSignature, ErrFlags). Flags are read only from a signature that
// verified, since only then does the key vouch for them.
func Verify(signers []Signer, policy Policy, message, armored []byte) (Verified, error) {
	if policy.Namespace == "" {
		return Verified{}, errors.New("sshsig: Verify needs a namespace")
	}
	s, err := ParseSignature(armored)
	if err != nil {
		return Verified{}, err
	}
	if s.Namespace != policy.Namespace {
		return Verified{}, fmt.Errorf("%w: the signature is for namespace %q, not %q", ErrNamespace, s.Namespace, policy.Namespace)
	}
	var listed, allowed []Signer
	for _, sg := range signers {
		if !bytes.Equal(sg.Key.blob, s.Key.blob) {
			continue
		}
		listed = append(listed, sg)
		if sg.AllowsNamespace(policy.Namespace) {
			allowed = append(allowed, sg)
		}
	}
	switch {
	case len(listed) == 0:
		return Verified{}, fmt.Errorf("%w: %s key %s", ErrUnlisted, s.Key.keyType, s.Key.Fingerprint())
	case len(allowed) == 0:
		return Verified{}, fmt.Errorf("%w: the key %s (%s) is listed for namespaces %q only, not %q",
			ErrNamespace, s.Key.Fingerprint(), strings.Join(listed[0].Principals, ","), strings.Join(listed[0].Namespaces, ","), policy.Namespace)
	}
	if err := s.verifyCrypto(message); err != nil {
		if errors.Is(err, ErrBadSignature) {
			return Verified{}, fmt.Errorf("%w: by %s (%s)", ErrBadSignature, s.Key.Fingerprint(), strings.Join(allowed[0].Principals, ","))
		}
		return Verified{}, err
	}
	if s.Key.SecurityKey() {
		if missing := policy.SecurityKeyFlags &^ s.Flags; missing != 0 {
			return Verified{}, fmt.Errorf("%w: the signature by %s (%s) carries flags 0x%02x, without %s",
				ErrFlags, s.Key.Fingerprint(), strings.Join(allowed[0].Principals, ","), s.Flags, flagWords(missing))
		}
	}
	return Verified{
		Signer: allowed[0], Fingerprint: s.Key.Fingerprint(), KeyType: s.Key.keyType,
		HashAlgorithm: s.HashAlgorithm, Flags: s.Flags, Counter: s.Counter,
	}, nil
}

// flagWords names the flags in f.
func flagWords(f byte) string {
	var words []string
	if f&FlagUserPresence != 0 {
		words = append(words, "user presence (0x01: the key was not touched)")
	}
	if f&FlagUserVerification != 0 {
		words = append(words, "user verification (0x04: no PIN or biometric)")
	}
	if rest := f &^ (FlagUserPresence | FlagUserVerification); rest != 0 {
		words = append(words, fmt.Sprintf("flags 0x%02x", rest))
	}
	return strings.Join(words, " and ")
}

// reader reads OpenSSH's wire encoding (RFC 4251): uint32, string, mpint.
type reader struct{ buf []byte }

func (r *reader) done() bool { return len(r.buf) == 0 }

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || len(r.buf) < n {
		return nil, errors.New("truncated")
	}
	out := r.buf[:n]
	r.buf = r.buf[n:]
	return out, nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (r *reader) str() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if uint64(n) > uint64(len(r.buf)) {
		return nil, fmt.Errorf("a %d-byte string with %d bytes left", n, len(r.buf))
	}
	return r.take(int(n))
}

// mpint reads a non-negative multiple-precision integer in its minimal
// encoding: no needless leading zero byte, and a leading zero where the high
// bit would otherwise read as negative.
func (r *reader) mpint() (*big.Int, error) {
	b, err := r.str()
	if err != nil {
		return nil, err
	}
	switch {
	case len(b) == 0:
		return new(big.Int), nil
	case b[0]&0x80 != 0:
		return nil, errors.New("negative mpint")
	case b[0] == 0 && (len(b) == 1 || b[1]&0x80 == 0):
		return nil, errors.New("mpint with a needless leading zero")
	}
	return new(big.Int).SetBytes(b), nil
}

func writeString(b *bytes.Buffer, s []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.Write(s)
}
