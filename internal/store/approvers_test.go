package store

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var apNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func touchKey(b byte) []byte {
	k := bytes.Repeat([]byte{b}, 65)
	k[0] = 0x04
	return k
}

func passkeyCred(id string) ApproverCredential {
	return ApproverCredential{
		Kind: ApproverPasskey, CredentialID: []byte(id), PublicKey: []byte{0xa5, 0x01, 0x02}, Alg: -7,
		BackupEligible: true, BackupState: true, AAGUID: bytes.Repeat([]byte{0}, 16), Transports: []string{"internal", "hybrid"},
		Attachment: "platform", AttestationFormat: "none", UserHandle: []byte("handle"), Name: "laptop " + id,
	}
}

func TestMigration032_ApproverTables(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	var v int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v))
	require.GreaterOrEqual(t, v, 32)
	for _, table := range []string{"approver_credentials", "approver_enrol_codes"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n))
		require.Zero(t, n, "%s starts empty", table)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, name, added_at, added_by)
		VALUES ('x', 'password', X'01', X'01', -7, 'n', 't', 'code')`)
	require.Error(t, err, "the kind is checked by the schema too")
}

func TestEnrolCode_Shape(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	code, exp, err := MintEnrolCode(ctx, db, ApproverPasskey, apNow)
	require.NoError(t, err)
	require.Equal(t, apNow.Add(EnrolCodeTTL), exp)
	require.Regexp(t, `^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$`, code)

	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT hash FROM approver_enrol_codes`).Scan(&stored))
	require.NotContains(t, stored, strings.ReplaceAll(code, "-", ""), "only the hash is stored")
	require.Len(t, stored, 64)

	// As typed: lowercase, no dashes, spaces, and Crockford's look-alikes.
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	require.NoError(t, CheckEnrolCode(ctx, db, ApproverPasskey, typed, apNow))
	require.Equal(t, "0110", NormalizeEnrolCode("oIl0"))

	_, _, err = MintEnrolCode(ctx, db, "password", apNow)
	require.ErrorIs(t, err, ErrInvalidApprover)
}

func TestEnrolApprover_CodeRules(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	code, _, err := MintEnrolCode(ctx, db, ApproverPasskey, apNow)
	require.NoError(t, err)
	touchCode, _, err := MintEnrolCode(ctx, db, ApproverTouchID, apNow)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		code string
		at   time.Time
	}{
		"absent":                        {"AAAA-BBBB-CCCC-DDDD", apNow},
		"expired":                       {code, apNow.Add(EnrolCodeTTL)},
		"another kind's code":           {touchCode, apNow},
		"one symbol off":                {code[:len(code)-1] + "Z", apNow},
		"a long time later, still dead": {code, apNow.Add(24 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.code == code[:len(code)-1]+"Z" && strings.HasSuffix(code, "Z") {
				t.Skip("the random code already ends in Z")
			}
			require.ErrorIs(t, CheckEnrolCode(ctx, db, ApproverPasskey, tc.code, tc.at), ErrEnrolCode)
			_, err := EnrolApprover(ctx, db, passkeyCred("c-"+name), tc.code, "", tc.at)
			require.ErrorIs(t, err, ErrEnrolCode)
		})
	}

	first, err := EnrolApprover(ctx, db, passkeyCred("c1"), code, "", apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, AddedByCode, first.AddedBy)
	require.NotEmpty(t, first.ID)

	_, err = EnrolApprover(ctx, db, passkeyCred("c2"), code, "", apNow.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrEnrolCode, "a code is single use")
	require.ErrorIs(t, CheckEnrolCode(ctx, db, ApproverPasskey, code, apNow.Add(2*time.Minute)), ErrEnrolCode)

	var usedFor string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT used_for FROM approver_enrol_codes WHERE kind = 'passkey'`).Scan(&usedFor))
	require.Equal(t, first.ID, usedFor)

	got, err := ApproverCredentials(ctx, db, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, first.ID, got[0].ID)
	require.Equal(t, []string{"internal", "hybrid"}, got[0].Transports)
	require.True(t, got[0].BackupEligible)
	require.Equal(t, []byte("handle"), got[0].UserHandle)
}

func TestEnrolApprover_ProofRules(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()

	_, err := EnrolApprover(ctx, db, passkeyCred("c1"), "", "", apNow)
	require.ErrorIs(t, err, ErrEnrolProof, "no proof at all")
	_, err = EnrolApprover(ctx, db, passkeyCred("c1"), "", "no-such-credential", apNow)
	require.ErrorIs(t, err, ErrEnrolProof, "the first credential needs a code: there is nothing to approve it")

	code, _, err := MintEnrolCode(ctx, db, ApproverPasskey, apNow)
	require.NoError(t, err)
	first, err := EnrolApprover(ctx, db, passkeyCred("c1"), code, "", apNow)
	require.NoError(t, err)

	code2, _, err := MintEnrolCode(ctx, db, ApproverPasskey, apNow)
	require.NoError(t, err)
	_, err = EnrolApprover(ctx, db, passkeyCred("c2"), code2, first.ID, apNow)
	require.ErrorIs(t, err, ErrEnrolProof, "two proofs is not one")
	require.NoError(t, CheckEnrolCode(ctx, db, ApproverPasskey, code2, apNow), "a refused enrolment consumes nothing")

	second, err := EnrolApprover(ctx, db, passkeyCred("c2"), "", first.ID, apNow)
	require.NoError(t, err, "a later credential, approved by an existing one")
	require.Equal(t, first.ID, second.AddedBy)

	_, err = EnrolApprover(ctx, db, passkeyCred("c1"), "", first.ID, apNow)
	require.ErrorIs(t, err, ErrApproverExists)

	touch := ApproverCredential{Kind: ApproverTouchID, CredentialID: []byte("tk1"), PublicKey: touchKey(7), Alg: -7, Name: "this Mac"}
	tcode, _, err := MintEnrolCode(ctx, db, ApproverTouchID, apNow)
	require.NoError(t, err)
	tk, err := EnrolApprover(ctx, db, touch, tcode, "", apNow)
	require.NoError(t, err)
	require.Equal(t, ApproverTouchID, tk.Kind)

	passkeys, err := ApproverCredentials(ctx, db, ApproverPasskey)
	require.NoError(t, err)
	require.Len(t, passkeys, 2)
	n, err := CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestEnrolApprover_RefusesBadRecords(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for name, c := range map[string]ApproverCredential{
		"no kind":                   {CredentialID: []byte("x"), PublicKey: []byte{1}, Name: "n"},
		"no credential id":          {Kind: ApproverPasskey, PublicKey: []byte{1}, Name: "n"},
		"an over-long credential":   {Kind: ApproverPasskey, CredentialID: make([]byte, 1024), PublicKey: []byte{1}, Name: "n"},
		"no public key":             {Kind: ApproverPasskey, CredentialID: []byte("x"), Name: "n"},
		"a short Touch ID key":      {Kind: ApproverTouchID, CredentialID: []byte("x"), PublicKey: []byte{4, 1, 2}, Name: "n"},
		"a compressed Touch ID key": {Kind: ApproverTouchID, CredentialID: []byte("x"), PublicKey: append([]byte{2}, make([]byte, 64)...), Name: "n"},
		"no name":                   {Kind: ApproverPasskey, CredentialID: []byte("x"), PublicKey: []byte{1}, Name: "  "},
		"a two-line name":           {Kind: ApproverPasskey, CredentialID: []byte("x"), PublicKey: []byte{1}, Name: "a\nb"},
		"an over-long name":         {Kind: ApproverPasskey, CredentialID: []byte("x"), PublicKey: []byte{1}, Name: strings.Repeat("n", 65)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := EnrolApprover(ctx, db, c, "AAAA", "", apNow)
			require.ErrorIs(t, err, ErrInvalidApprover)
		})
	}
}

func enrolOne(t *testing.T, db *sql.DB, c ApproverCredential) ApproverCredential {
	t.Helper()
	ctx := context.Background()
	code, _, err := MintEnrolCode(ctx, db, c.Kind, apNow)
	require.NoError(t, err)
	got, err := EnrolApprover(ctx, db, c, code, "", apNow)
	require.NoError(t, err)
	return got
}

func TestRecordApproverUse_CompareAndSet(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	c := enrolOne(t, db, passkeyCred("c1"))

	require.NoError(t, RecordApproverUse(ctx, db, c.ID, 0, 5, false, apNow.Add(time.Minute)))
	got, err := ApproverByID(ctx, db, c.ID)
	require.NoError(t, err)
	require.Equal(t, uint32(5), got.SignCount)
	require.False(t, got.BackupState)
	require.NotNil(t, got.LastUsedAt)
	require.Equal(t, apNow.Add(time.Minute), *got.LastUsedAt)

	require.ErrorIs(t, RecordApproverUse(ctx, db, c.ID, 0, 6, false, apNow), ErrApproverCounter, "verified against a counter that moved")
	require.ErrorIs(t, RecordApproverUse(ctx, db, "nope", 0, 6, false, apNow), ErrApproverNotFound)

	// Two assertions verified against the same stored counter: one wins.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = RecordApproverUse(ctx, db, c.ID, 5, 6, false, apNow)
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrApproverCounter)
		}
	}
	require.Equal(t, 1, wins)
}

func TestRemoveAndResetApprovers(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a := enrolOne(t, db, passkeyCred("c1"))
	enrolOne(t, db, passkeyCred("c2"))
	require.NoError(t, RemoveApprover(ctx, db, a.ID))
	require.ErrorIs(t, RemoveApprover(ctx, db, a.ID), ErrApproverNotFound)
	_, err := ApproverByID(ctx, db, a.ID)
	require.ErrorIs(t, err, ErrApproverNotFound)

	// A live lease and a pending code: reset ends both.
	approval, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), apNow)
	require.NoError(t, err)
	_, err = ApproveAccess(ctx, db, approval.ID, approval.Challenge, "cred-1", apNow)
	require.NoError(t, err)
	pending, _, err := MintEnrolCode(ctx, db, ApproverPasskey, apNow)
	require.NoError(t, err)

	creds, leases, err := ResetApprovers(ctx, db, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(1), creds)
	require.Equal(t, int64(1), leases)
	n, err := CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n)
	live, err := ActiveAccessLeases(ctx, db, fenceProc.Key(), apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, live, "reset revokes every lease")
	var reason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases`).Scan(&reason))
	require.Equal(t, LeaseRevokedReset, reason)
	require.ErrorIs(t, CheckEnrolCode(ctx, db, ApproverPasskey, pending, apNow.Add(time.Minute)), ErrEnrolCode,
		"an unused code minted before the reset is gone")
}

func TestApproverUserHandle_StableAndRandom(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	h1, err := ApproverUserHandle(ctx, db)
	require.NoError(t, err)
	require.Len(t, h1, 32)
	h2, err := ApproverUserHandle(ctx, db)
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	other := leaseDB(t)
	h3, err := ApproverUserHandle(ctx, other)
	require.NoError(t, err)
	require.NotEqual(t, h1, h3)

	require.NoError(t, SetSetting(ctx, other, approverUserHandleKey, "not hex"))
	_, err = ApproverUserHandle(ctx, other)
	require.ErrorIs(t, err, ErrInvalidApprover)
}
