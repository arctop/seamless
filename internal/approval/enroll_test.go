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

func (f *fixture) code(t *testing.T, kind string) string {
	t.Helper()
	code, _, err := store.MintEnrollCode(context.Background(), f.db, kind, f.now)
	require.NoError(t, err)
	return code
}

func (f *fixture) enrollWithCode(t *testing.T, a *passkeytest.Authenticator, name string) store.ApproverCredential {
	t.Helper()
	ctx := context.Background()
	begun, err := f.svc.BeginPasskey(ctx, name, f.code(t, store.ApproverPasskey))
	require.NoError(t, err)
	resp, err := a.Create(begun.Options, origin)
	require.NoError(t, err)
	fin, err := f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.NoError(t, err)
	require.NotNil(t, fin.Credential)
	require.Nil(t, fin.Approve)
	return *fin.Credential
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

	// The options asked for what the design says: UV required, resident key
	// preferred, attestation none, the three algorithms, no attachment.
	begun, err := f.svc.BeginPasskey(ctx, "another", f.code(t, store.ApproverPasskey))
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

	_, err := f.svc.BeginPasskey(ctx, "key", "")
	require.ErrorIs(t, err, store.ErrEnrollProof, "the first passkey needs a code")
	_, err = f.svc.BeginPasskey(ctx, "key", "AAAA-BBBB-CCCC-DDDD")
	require.ErrorIs(t, err, store.ErrEnrollCode, "an absent code is refused before the authenticator is touched")
	_, err = f.svc.BeginPasskey(ctx, "key", f.code(t, store.ApproverTouchID))
	require.ErrorIs(t, err, store.ErrEnrollCode, "a Touch ID code does not enroll a passkey")
	_, err = f.svc.BeginPasskey(ctx, "", f.code(t, store.ApproverPasskey))
	require.ErrorIs(t, err, store.ErrInvalidApprover, "a name is required")

	// A code that expires between begin and finish is refused at finish.
	a, err := passkeytest.New(false)
	require.NoError(t, err)
	late := f.code(t, store.ApproverPasskey)
	f.now = t0.Add(store.EnrollCodeTTL - time.Minute)
	begun, err := f.svc.BeginPasskey(ctx, "key", late)
	require.NoError(t, err)
	resp, err := a.Create(begun.Options, origin)
	require.NoError(t, err)
	f.now = t0.Add(store.EnrollCodeTTL)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.ErrorIs(t, err, store.ErrEnrollCode)

	// An enrollment left waiting longer than EnrollTTL is gone.
	begun, err = f.svc.BeginPasskey(ctx, "key", f.code(t, store.ApproverPasskey))
	require.NoError(t, err)
	resp, err = a.Create(begun.Options, origin)
	require.NoError(t, err)
	f.now = f.now.Add(EnrollTTL)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.ErrorIs(t, err, ErrNoEnrollment)

	// A reused code: the second enrollment with it fails at begin.
	f.now = t0.Add(time.Hour)
	code := f.code(t, store.ApproverPasskey)
	begun, err = f.svc.BeginPasskey(ctx, "key", code)
	require.NoError(t, err)
	resp, err = a.Create(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.NoError(t, err)
	_, err = f.svc.BeginPasskey(ctx, "again", code)
	require.ErrorIs(t, err, store.ErrEnrollCode)

	// An enrollment is finished once.
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.ErrorIs(t, err, ErrNoEnrollment)
	_, err = f.svc.FinishPasskey(ctx, "never-begun", resp)
	require.ErrorIs(t, err, ErrNoEnrollment)
}

func TestPasskeyEnrollment_WrongOriginIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := passkeytest.New(false)
	require.NoError(t, err)
	begun, err := f.svc.BeginPasskey(ctx, "key", f.code(t, store.ApproverPasskey))
	require.NoError(t, err)
	resp, err := a.Create(begun.Options, "http://127.0.0.1:8081")
	require.NoError(t, err)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, resp)
	require.ErrorIs(t, err, passkey.ErrVerification)
	n, err := store.CountApprovers(ctx, f.db)
	require.NoError(t, err)
	require.Zero(t, n)
}

// A second passkey without a code: it is created, then an existing passkey
// approves it over a challenge that binds the new credential.
func TestPasskeyEnrollment_ApprovedByAnExistingPasskey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := passkeytest.New(true)
	require.NoError(t, err)
	firstRow := f.enrollWithCode(t, first, "security key")

	second, err := passkeytest.New(false)
	require.NoError(t, err)
	begun, err := f.svc.BeginPasskey(ctx, "phone", "")
	require.NoError(t, err)
	created, err := second.Create(begun.Options, origin)
	require.NoError(t, err)
	fin, err := f.svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)
	require.Nil(t, fin.Credential, "not stored until an existing passkey approves it")
	require.NotNil(t, fin.Approve)
	n, err := store.CountApprovers(ctx, f.db)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, err = second.Get(fin.Approve, origin)
	require.Error(t, err, "only an enrolled passkey may approve: the new one is not allowed")

	assertion, err := first.Get(fin.Approve, origin)
	require.NoError(t, err)
	added, err := f.svc.ApprovePasskey(ctx, begun.ID, assertion)
	require.NoError(t, err)
	require.Equal(t, firstRow.ID, added.AddedBy)
	require.Equal(t, second.CredentialID(), added.CredentialID)

	approver, err := store.ApproverByID(ctx, f.db, firstRow.ID)
	require.NoError(t, err)
	require.Equal(t, uint32(1), approver.SignCount, "the approving passkey's counter moved")
	require.NotNil(t, approver.LastUsedAt)

	_, err = f.svc.ApprovePasskey(ctx, begun.ID, assertion)
	require.ErrorIs(t, err, ErrNoEnrollment, "an approval is used once")
}

// An approval made for one enrollment does not approve another: the
// challenge binds the enrollment and the new credential.
func TestPasskeyEnrollment_ApprovalIsBoundToItsEnrollment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := passkeytest.New(false)
	require.NoError(t, err)
	f.enrollWithCode(t, first, "key")

	start := func(a *passkeytest.Authenticator) (string, json.RawMessage) {
		begun, err := f.svc.BeginPasskey(ctx, "new", "")
		require.NoError(t, err)
		created, err := a.Create(begun.Options, origin)
		require.NoError(t, err)
		fin, err := f.svc.FinishPasskey(ctx, begun.ID, created)
		require.NoError(t, err)
		return begun.ID, fin.Approve
	}
	b, err := passkeytest.New(false)
	require.NoError(t, err)
	c, err := passkeytest.New(false)
	require.NoError(t, err)
	idB, approveB := start(b)
	idC, _ := start(c)

	assertionForB, err := first.Get(approveB, origin)
	require.NoError(t, err)
	_, err = f.svc.ApprovePasskey(ctx, idC, assertionForB)
	require.ErrorIs(t, err, passkey.ErrVerification, "B's approval does not enroll C")
	_, err = f.svc.ApprovePasskey(ctx, idC, assertionForB)
	require.ErrorIs(t, err, ErrNoEnrollment, "a failed approval ends that enrollment: one attempt each")
	added, err := f.svc.ApprovePasskey(ctx, idB, assertionForB)
	require.NoError(t, err, "B's approval still enrolls B")
	require.Equal(t, b.CredentialID(), added.CredentialID)
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
	code := f.code(t, store.ApproverTouchID)

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, pub, signFor(other, TouchIDEnrollMessage(code, pub)))
	require.ErrorIs(t, err, ErrTouchIDKey, "proof by another key")
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, pub, signFor(key, TouchIDEnrollMessage("WRONG-CODE", pub)))
	require.ErrorIs(t, err, ErrTouchIDKey, "a proof over another code")
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", code, []byte{4, 1, 2}, []byte{1})
	require.ErrorIs(t, err, ErrTouchIDKey, "not a P-256 point")
	_, err = f.svc.EnrollTouchID(ctx, "this Mac", f.code(t, store.ApproverPasskey), pub, signFor(key, TouchIDEnrollMessage(code, pub)))
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

func TestPasskeysOff(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := New(Config{DB: db})
	require.False(t, svc.PasskeysOn())
	_, err = svc.BeginPasskey(context.Background(), "k", "c")
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
		code := f.code(t, store.ApproverTouchID)
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
		PublicKey: []byte{0xa5}, Alg: -7, Name: "security key"}, f.code(t, store.ApproverPasskey), "", f.now)
	require.NoError(t, err)

	newK, pub := newKey(t)
	code := f.code(t, store.ApproverTouchID)
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
	code3 := f2.code(t, store.ApproverTouchID)
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
