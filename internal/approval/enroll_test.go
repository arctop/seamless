package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

const origin = "http://localhost:8081"

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type fixture struct {
	db  *sql.DB
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{origin}})
	require.NoError(t, err)
	f := &fixture{db: db, now: t0}
	f.svc = New(Config{DB: db, Passkeys: v, UserName: "owner", UserDisplayName: "Seamless owner", Now: func() time.Time { return f.now }})
	return f
}

// codeFor mints a code of kind for the key named by fingerprint, as
// `seamlessd approvers enroll-code <kind> --key <fingerprint>` does.
func (f *fixture) codeFor(t *testing.T, kind, fingerprint string) string {
	t.Helper()
	code, _, err := store.MintEnrollCode(context.Background(), f.db, kind, fingerprint, f.now)
	require.NoError(t, err)
	return code
}

// code mints a code of kind for publicKey.
func (f *fixture) code(t *testing.T, kind string, publicKey []byte) string {
	t.Helper()
	return f.codeFor(t, kind, store.ApproverKeyFingerprint(publicKey))
}

// startByCode begins a by-code enrollment and creates a's credential, leaving
// it waiting for the code minted for the fingerprint Finish returned.
func (f *fixture) startByCode(t *testing.T, a *passkeytest.Authenticator, name string) (string, Finished) {
	t.Helper()
	ctx := context.Background()
	begun, err := f.svc.BeginPasskey(ctx, name)
	require.NoError(t, err)
	resp, err := a.Create(begun.Options, origin)
	require.NoError(t, err)
	fin, err := f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.NoError(t, err)
	require.NotEmpty(t, fin.Fingerprint)
	return begun.ID, fin
}

func (f *fixture) enrollWithCode(t *testing.T, a *passkeytest.Authenticator, name string) store.ApproverCredential {
	t.Helper()
	id, fin := f.startByCode(t, a, name)
	cred, err := f.svc.ConfirmPasskeyCode(context.Background(), id, f.codeFor(t, store.ApproverPasskey, fin.Fingerprint))
	require.NoError(t, err)
	return cred
}

func (f *fixture) approverCount(t *testing.T) int {
	t.Helper()
	n, err := store.CountApprovers(context.Background(), f.db)
	require.NoError(t, err)
	return n
}

func TestPasskeyEnrollment_WithACode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	cred := f.enrollWithCode(t, a, "security key")
	require.Equal(t, store.ApproverPasskey, cred.Kind)
	require.Equal(t, store.AddedByCode, cred.AddedBy)
	require.Equal(t, a.CredentialID(), cred.CredentialID)
	require.Equal(t, passkey.AlgES256, cred.Alg)
	require.Equal(t, "localhost", cred.RPID, "the passkey records the RP ID it was registered under")
	stored, err := store.ApproverByID(ctx, f.db, cred.ID)
	require.NoError(t, err)
	require.Equal(t, "localhost", stored.RPID)

	// The options asked for what the design says: UV required, resident key
	// preferred, attestation none, the three algorithms, no attachment.
	begun, err := f.svc.BeginPasskey(ctx, "another")
	require.NoError(t, err)
	var o struct {
		PublicKey struct {
			RP                     struct{ ID string } `json:"rp"`
			Attestation            string              `json:"attestation"`
			AuthenticatorSelection map[string]any      `json:"authenticatorSelection"`
			PubKeyCredParams       []struct {
				Alg int64 `json:"alg"`
			} `json:"pubKeyCredParams"`
			ExcludeCredentials []json.RawMessage `json:"excludeCredentials"`
		} `json:"publicKey"`
	}
	require.NoError(t, json.Unmarshal(begun.Options, &o))
	require.Equal(t, "localhost", o.PublicKey.RP.ID)
	require.Contains(t, []string{"", "none"}, o.PublicKey.Attestation)
	require.Equal(t, "required", o.PublicKey.AuthenticatorSelection["userVerification"])
	require.Equal(t, "preferred", o.PublicKey.AuthenticatorSelection["residentKey"])
	require.NotContains(t, o.PublicKey.AuthenticatorSelection, "authenticatorAttachment")
	var algs []int64
	for _, p := range o.PublicKey.PubKeyCredParams {
		algs = append(algs, p.Alg)
	}
	require.ElementsMatch(t, []int64{-7, -8, -257}, algs)
	require.Len(t, o.PublicKey.ExcludeCredentials, 1, "an enrolled passkey is excluded")
	_, err = a.Create(begun.Options, origin)
	require.Error(t, err, "the same authenticator is refused by its own exclusion")
}

func TestPasskeyEnrollment_CodeRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, err := f.svc.BeginPasskey(ctx, "")
	require.ErrorIs(t, err, store.ErrInvalidApprover, "a name is required")

	// A code that is not valid is refused at confirm, once the key it would
	// enroll exists.
	a, err := passkeytest.New(false)
	require.NoError(t, err)
	id, fin := f.startByCode(t, a, "key")
	_, err = f.svc.ConfirmPasskeyCode(ctx, id, "AAAA-BBBB-CCCC-DDDD")
	require.ErrorIs(t, err, store.ErrEnrollCode, "an absent code is refused")
	_, err = f.svc.ConfirmPasskeyCode(ctx, id, f.codeFor(t, store.ApproverTouchID, fin.Fingerprint))
	require.ErrorIs(t, err, store.ErrEnrollCode, "a Touch ID code does not enroll a passkey")
	_, err = f.svc.ConfirmPasskeyCode(ctx, id, "")
	require.ErrorIs(t, err, store.ErrEnrollProof, "no code at all is no proof")
	require.Zero(t, f.approverCount(t))

	// A code that expires before it is typed is refused at confirm. It was
	// minted for this authenticator's key at the start; the enrollment that
	// waits for it began later, so the code ages out first.
	late := f.codeFor(t, store.ApproverPasskey, fin.Fingerprint)
	f.now = t0.Add(store.EnrollCodeTTL - time.Minute)
	lateID, lateFin := f.startByCode(t, a, "key")
	require.Equal(t, fin.Fingerprint, lateFin.Fingerprint, "the same authenticator, the same key")
	f.now = t0.Add(store.EnrollCodeTTL)
	_, err = f.svc.ConfirmPasskeyCode(ctx, lateID, late)
	require.ErrorIs(t, err, store.ErrEnrollCode)

	// An enrollment left waiting longer than EnrollTTL is gone, before finish
	// or after it.
	begun, err := f.svc.BeginPasskey(ctx, "key")
	require.NoError(t, err)
	resp, err := a.Create(begun.Options, origin)
	require.NoError(t, err)
	f.now = f.now.Add(EnrollTTL)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.ErrorIs(t, err, ErrNoEnrollment)
	waiting, waitingFin := f.startByCode(t, a, "key")
	stale := f.codeFor(t, store.ApproverPasskey, waitingFin.Fingerprint)
	f.now = f.now.Add(EnrollTTL)
	_, err = f.svc.ConfirmPasskeyCode(ctx, waiting, stale)
	require.ErrorIs(t, err, ErrNoEnrollment)

	// A reused code: two enrollments of one key wait for one code, and the
	// second is refused it.
	f.now = t0.Add(time.Hour)
	first, firstFin := f.startByCode(t, a, "key")
	second, _ := f.startByCode(t, a, "again")
	code := f.codeFor(t, store.ApproverPasskey, firstFin.Fingerprint)
	_, err = f.svc.ConfirmPasskeyCode(ctx, first, code)
	require.NoError(t, err)
	_, err = f.svc.ConfirmPasskeyCode(ctx, second, code)
	require.ErrorIs(t, err, store.ErrEnrollCode)

	// An enrollment is finished once.
	_, err = f.svc.FinishPasskey(ctx, first, resp)
	require.ErrorIs(t, err, ErrNoEnrollment)
	_, err = f.svc.ConfirmPasskeyCode(ctx, first, code)
	require.ErrorIs(t, err, ErrNoEnrollment, "a confirmed enrollment is gone")
	_, err = f.svc.FinishPasskey(ctx, "never-begun", resp)
	require.ErrorIs(t, err, ErrNoEnrollment)
	_, err = f.svc.ConfirmPasskeyCode(ctx, "never-begun", code)
	require.ErrorIs(t, err, ErrNoEnrollment)
}

func TestPasskeyEnrollment_WrongOriginIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := passkeytest.New(false)
	require.NoError(t, err)
	begun, err := f.svc.BeginPasskey(ctx, "key")
	require.NoError(t, err)
	resp, err := a.Create(begun.Options, "http://127.0.0.1:8081")
	require.NoError(t, err)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.ErrorIs(t, err, passkey.ErrVerification)
	n, err := store.CountApprovers(ctx, f.db)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestEnrollTouchID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	signFor := func(k *ecdsa.PrivateKey, msg []byte) []byte {
		d := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, k, d[:])
		require.NoError(t, err)
		return sig
	}
	code := f.code(t, store.ApproverTouchID, pub)

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, pub, signFor(other, TouchIDEnrollMessage(code, pub)))
	require.ErrorIs(t, err, ErrTouchIDKey, "proof by another key")
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, pub, signFor(key, TouchIDEnrollMessage("WRONG-CODE", pub)))
	require.ErrorIs(t, err, ErrTouchIDKey, "a proof over another code")
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, []byte{4, 1, 2}, []byte{1})
	require.ErrorIs(t, err, ErrTouchIDKey, "not a P-256 point")
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", f.code(t, store.ApproverPasskey, pub), pub, signFor(key, TouchIDEnrollMessage(code, pub)))
	require.Error(t, err)

	cred, err := f.svc.EnrollTouchID(ctx, "this Mac", code, pub, signFor(key, TouchIDEnrollMessage(code, pub)))
	require.NoError(t, err)
	require.Equal(t, store.ApproverTouchID, cred.Kind)
	require.Equal(t, pub, cred.PublicKey)
	sum := sha256.Sum256(pub)
	require.Equal(t, sum[:], cred.CredentialID)

	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, pub, signFor(key, TouchIDEnrollMessage(code, pub)))
	require.ErrorIs(t, err, store.ErrEnrollCode, "single use")
	require.NoError(t, VerifyTouchID(pub, []byte("hello"), signFor(key, []byte("hello"))))
}

// The attack a code that bound to nothing allowed: an agent that read the
// owner's terminal holds a live code minted for the owner's key. It enrolls its
// own software P-256 key with that code, signing the enrollment message
// correctly with its own key. The signature verifies; the code does not, because
// it was minted for another key. Nothing is enrolled and the code is not spent,
// so the owner's key still enrolls with it.
func TestEnrollTouchID_ACodeEnrollsOnlyTheKeyItWasMintedFor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	newKey := func() (*ecdsa.PrivateKey, []byte) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		pub, err := k.PublicKey.Bytes()
		require.NoError(t, err)
		return k, pub
	}
	signFor := func(k *ecdsa.PrivateKey, msg []byte) []byte {
		d := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, k, d[:])
		require.NoError(t, err)
		return sig
	}
	owner, ownerPub := newKey()
	attacker, attackerPub := newKey()
	code := f.code(t, store.ApproverTouchID, ownerPub)

	sig := signFor(attacker, TouchIDEnrollMessage(code, attackerPub))
	require.NoError(t, VerifyTouchID(attackerPub, TouchIDEnrollMessage(code, attackerPub), sig), "the attacker's proof is a good one")
	_, err := f.svc.EnrollTouchID(ctx, "Touch ID on this Mac", code, attackerPub, sig)
	require.ErrorIs(t, err, store.ErrEnrollCode)
	_, _, err = f.svc.EnrollTouchIDReplacing(ctx, "Touch ID on this Mac", code, attackerPub, sig, nil)
	require.ErrorIs(t, err, store.ErrEnrollCode)
	require.Zero(t, f.approverCount(t), "nothing is enrolled")

	cred, err := f.svc.EnrollTouchID(ctx, "this Mac", code, ownerPub, signFor(owner, TouchIDEnrollMessage(code, ownerPub)))
	require.NoError(t, err, "the code was not spent: the owner's key enrolls with it")
	require.Equal(t, ownerPub, cred.PublicKey)
	require.Equal(t, 1, f.approverCount(t))
}

// The same attack with a passkey: an agent's own enrollment, with its own
// authenticator, cannot use the code minted for the owner's passkey.
func TestPasskeyEnrollment_ACodeEnrollsOnlyTheKeyItWasMintedFor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owner, err := passkeytest.New(true)
	require.NoError(t, err)
	attacker, err := passkeytest.New(true)
	require.NoError(t, err)

	ownerID, ownerFin := f.startByCode(t, owner, "security key")
	code := f.codeFor(t, store.ApproverPasskey, ownerFin.Fingerprint)

	attackerID, attackerFin := f.startByCode(t, attacker, "x")
	require.NotEqual(t, ownerFin.Fingerprint, attackerFin.Fingerprint, "two authenticators, two keys")
	_, err = f.svc.ConfirmPasskeyCode(ctx, attackerID, code)
	require.ErrorIs(t, err, store.ErrEnrollCode)
	require.Zero(t, f.approverCount(t), "nothing is enrolled")

	cred, err := f.svc.ConfirmPasskeyCode(ctx, ownerID, code)
	require.NoError(t, err, "the code was not spent: the owner's passkey enrolls with it")
	require.Equal(t, owner.CredentialID(), cred.CredentialID)

	_, err = f.svc.ConfirmPasskeyCode(ctx, attackerID, code)
	require.ErrorIs(t, err, store.ErrEnrollCode, "and the attacker's enrollment still cannot use it")
	require.Equal(t, 1, f.approverCount(t))
}

// A mistyped code leaves the enrollment waiting: the passkey the owner just
// created is not lost to a typo, and the right code still enrolls it.
func TestConfirmPasskeyCode_AWrongCodeCanBeRetried(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := passkeytest.New(true)
	require.NoError(t, err)
	id, fin := f.startByCode(t, a, "security key")
	code := f.codeFor(t, store.ApproverPasskey, fin.Fingerprint)

	wrong := code[:len(code)-1] + "Z"
	if strings.HasSuffix(code, "Z") {
		wrong = code[:len(code)-1] + "Y"
	}
	_, err = f.svc.ConfirmPasskeyCode(ctx, id, wrong)
	require.ErrorIs(t, err, store.ErrEnrollCode)
	require.Zero(t, f.approverCount(t))

	cred, err := f.svc.ConfirmPasskeyCode(ctx, id, "  "+code+"\n")
	require.NoError(t, err, "the enrollment waited; a typed code is trimmed")
	require.Equal(t, a.CredentialID(), cred.CredentialID)
	require.Equal(t, store.AddedByCode, cred.AddedBy)
}

// F2 of the console-lock review: an enrolled passkey never vouches for a new
// one. With a passkey enrolled, a second enrollment still finishes waiting for
// the code minted for its own key, and nothing but that code enrolls it.
func TestPasskeyEnrollment_EverySecondPasskeyNeedsACode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := passkeytest.New(true)
	require.NoError(t, err)
	firstRow := f.enrollWithCode(t, first, "security key")

	second, err := passkeytest.New(false)
	require.NoError(t, err)
	id, fin := f.startByCode(t, second, "phone")
	require.NotEmpty(t, fin.Fingerprint, "a second passkey waits for a code like the first")
	_, err = f.svc.ConfirmPasskeyCode(ctx, id, "")
	require.ErrorIs(t, err, store.ErrEnrollProof, "no code is no proof, whatever is enrolled")
	require.Equal(t, 1, f.approverCount(t))

	// The code minted for the enrolled passkey's key does not enroll the new one.
	_, err = f.svc.ConfirmPasskeyCode(ctx, id, f.code(t, store.ApproverPasskey, firstRow.PublicKey))
	require.ErrorIs(t, err, store.ErrEnrollCode)
	require.Equal(t, 1, f.approverCount(t))

	added, err := f.svc.ConfirmPasskeyCode(ctx, id, f.codeFor(t, store.ApproverPasskey, fin.Fingerprint))
	require.NoError(t, err)
	require.Equal(t, store.AddedByCode, added.AddedBy, "enrolled by its code, never by another passkey")
	require.Equal(t, second.CredentialID(), added.CredentialID)
	require.Equal(t, 2, f.approverCount(t))
}

// ConfirmPasskeyCode needs a credential to enroll: an enrollment that has
// begun but not finished has none.
func TestConfirmPasskeyCode_NeedsACredential(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	begun, err := f.svc.BeginPasskey(ctx, "key")
	require.NoError(t, err)
	_, err = f.svc.ConfirmPasskeyCode(ctx, begun.ID, f.codeFor(t, store.ApproverPasskey, "Y2WK-2PG4-B7DB-FATZ"))
	require.ErrorIs(t, err, ErrNoEnrollment)
	require.Zero(t, f.approverCount(t))
}

func TestPasskeysOff(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := New(Config{DB: db})
	require.False(t, svc.PasskeysOn())
	_, err = svc.BeginPasskey(context.Background(), "k")
	require.ErrorIs(t, err, ErrPasskeysOff)
	_, err = svc.ConfirmPasskeyCode(context.Background(), "id", "code")
	require.ErrorIs(t, err, ErrPasskeysOff)
	var nilSvc *Service
	require.False(t, nilSvc.PasskeysOn())
}

// A Touch ID key that replaces this Mac's earlier one retires it in the same
// enrollment, but only a credential whose own key signed the request to be
// retired: never a passkey, never another Mac's key, never on a signature over
// another enrollment, and nothing when the enrollment itself is refused.
func TestEnrollTouchIDReplacing(t *testing.T) {
	ctx := context.Background()
	newKey := func(t *testing.T) (*ecdsa.PrivateKey, []byte) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		pub, err := k.PublicKey.Bytes()
		require.NoError(t, err)
		return k, pub
	}
	signFor := func(t *testing.T, k *ecdsa.PrivateKey, msg []byte) []byte {
		d := sha256.Sum256(msg)
		sig, err := ecdsa.SignASN1(rand.Reader, k, d[:])
		require.NoError(t, err)
		return sig
	}
	enroll := func(t *testing.T, f *fixture, name string) (*ecdsa.PrivateKey, store.ApproverCredential) {
		k, pub := newKey(t)
		code := f.code(t, store.ApproverTouchID, pub)
		c, err := f.svc.EnrollTouchID(ctx, name, code, pub, signFor(t, k, TouchIDEnrollMessage(code, pub)))
		require.NoError(t, err)
		return k, c
	}
	ids := func(t *testing.T, f *fixture) []string {
		rows, err := store.ApproverCredentials(ctx, f.db, "")
		require.NoError(t, err)
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}

	f := newFixture(t)
	oldKey, old := enroll(t, f, "Touch ID on example-mac")
	otherKey, other := enroll(t, f, "Touch ID on example-mini")
	pk, err := store.EnrollApprover(ctx, f.db, store.ApproverCredential{Kind: store.ApproverPasskey, CredentialID: []byte("pk"),
		PublicKey: []byte{0xa5}, Alg: -7, Name: "security key"}, f.code(t, store.ApproverPasskey, []byte{0xa5}), f.now)
	require.NoError(t, err)

	newK, pub := newKey(t)
	code := f.code(t, store.ApproverTouchID, pub)
	enrollSig := signFor(t, newK, TouchIDEnrollMessage(code, pub))
	replace := func(k *ecdsa.PrivateKey, id string) TouchIDReplacement {
		return TouchIDReplacement{ID: id, Signature: signFor(t, k, TouchIDReplaceMessage(code, pub, id))}
	}
	refused := []TouchIDReplacement{
		replace(otherKey, old.ID), // another Mac's key asking to retire this Mac's
		replace(oldKey, other.ID), // this Mac's key asking to retire another Mac's
		replace(oldKey, pk.ID),    // a passkey
		replace(oldKey, "01NOSUCHAPPROVER0000000000"),
		{ID: old.ID, Signature: signFor(t, oldKey, TouchIDReplaceMessage("ZZZZ-ZZZZ-ZZZZ-ZZZZ", pub, old.ID))}, // another enrollment
		{ID: old.ID, Signature: signFor(t, oldKey, TouchIDEnrollMessage(code, pub))},                           // another message
		{ID: old.ID, Signature: []byte{1, 2, 3}},
	}

	// A refused enrollment retires nothing, even with a good request to.
	_, _, err = f.svc.EnrollTouchIDReplacing(ctx, "new", code, pub, signFor(t, otherKey, TouchIDEnrollMessage(code, pub)),
		[]TouchIDReplacement{replace(oldKey, old.ID)})
	require.ErrorIs(t, err, ErrTouchIDKey)
	require.ElementsMatch(t, []string{old.ID, other.ID, pk.ID}, ids(t, f))

	// Every other request is left alone, and the enrollment goes on.
	got, retired, err := f.svc.EnrollTouchIDReplacing(ctx, "Touch ID on example-mac", code, pub, enrollSig,
		append(refused, replace(oldKey, old.ID)))
	require.NoError(t, err)
	require.Equal(t, []string{old.ID}, retired)
	require.Equal(t, pub, got.PublicKey)
	require.ElementsMatch(t, []string{other.ID, pk.ID, got.ID}, ids(t, f))

	// Without a request to retire, nothing is.
	f2 := newFixture(t)
	_, old2 := enroll(t, f2, "Touch ID on example-mac")
	k3, pub3 := newKey(t)
	code3 := f2.code(t, store.ApproverTouchID, pub3)
	got3, retired, err := f2.svc.EnrollTouchIDReplacing(ctx, "again", code3, pub3, signFor(t, k3, TouchIDEnrollMessage(code3, pub3)), nil)
	require.NoError(t, err)
	require.Empty(t, retired)
	require.ElementsMatch(t, []string{old2.ID, got3.ID}, ids(t, f2))
}

func TestTouchIDReplaceMessage(t *testing.T) {
	pub := append([]byte{4}, make([]byte, 64)...)
	sum := sha256.Sum256([]byte("ABCDEFGHJKMNPQRS"))
	want := append(append(append([]byte("seamless touchid replacement v1\x00"), sum[:]...), pub...), "01OLDAPPROVER0000000000000"...)
	require.Equal(t, want, TouchIDReplaceMessage("abcd-efgh-jkmn-pqrs", pub, "01OLDAPPROVER0000000000000"))
	require.NotEqual(t, TouchIDEnrollMessage("ABCDEFGHJKMNPQRS", pub), TouchIDReplaceMessage("ABCDEFGHJKMNPQRS", pub, ""),
		"an enrollment's signature is never a replacement's")
}
