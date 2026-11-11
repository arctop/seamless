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

	"github.com/arctop/seamless/internal/core"
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

// fingerprintOf names c's key the way the owner does when minting its code.
func fingerprintOf(c ApproverCredential) string { return ApproverKeyFingerprint(c.PublicKey) }

// enrollCodeLive reports whether code is live and unused for kind and the key
// named by fingerprint at now, without consuming it.
func enrollCodeLive(t *testing.T, db *sql.DB, kind, code, fingerprint string, now time.Time) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM approver_enroll_codes
		WHERE hash = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`,
		enrollCodeHash(code, fingerprint), kind, core.FormatTime(now)).Scan(&n))
	return n == 1
}

func TestMigration032_ApproverTables(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	var v int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v))
	require.GreaterOrEqual(t, v, 32)
	for _, table := range []string{"approver_credentials", "approver_enroll_codes"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n))
		require.Zero(t, n, "%s starts empty", table)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, name, added_at, added_by)
		VALUES ('x', 'password', X'01', X'01', -7, 'n', 't', 'code')`)
	require.Error(t, err, "the kind is checked by the schema too")
}

func TestEnrollCode_Shape(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	cred := passkeyCred("c1")
	code, exp, err := MintEnrollCode(ctx, db, ApproverPasskey, fingerprintOf(cred), apNow)
	require.NoError(t, err)
	require.Equal(t, apNow.Add(EnrollCodeTTL), exp)
	require.Regexp(t, `^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$`, code)

	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT hash FROM approver_enroll_codes`).Scan(&stored))
	require.NotContains(t, stored, strings.ReplaceAll(code, "-", ""), "only the hash is stored")
	require.Len(t, stored, 64)

	// As typed: lowercase, no dashes, spaces, and Crockford's look-alikes.
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	_, err = EnrollApprover(ctx, db, cred, typed, apNow)
	require.NoError(t, err)
	require.Equal(t, "0110", NormalizeEnrollCode("oIl0"))

	_, _, err = MintEnrollCode(ctx, db, "password", fingerprintOf(cred), apNow)
	require.ErrorIs(t, err, ErrInvalidApprover)
}

func TestEnrollApprover_CodeRules(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	fp := fingerprintOf(passkeyCred("c1"))
	code, _, err := MintEnrollCode(ctx, db, ApproverPasskey, fp, apNow)
	require.NoError(t, err)
	// For the same key: only the kind differs.
	touchCode, _, err := MintEnrollCode(ctx, db, ApproverTouchID, fp, apNow)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		code string
		at   time.Time
	}{
		"absent":                        {"AAAA-BBBB-CCCC-DDDD", apNow},
		"expired":                       {code, apNow.Add(EnrollCodeTTL)},
		"another kind's code":           {touchCode, apNow},
		"one symbol off":                {code[:len(code)-1] + "Z", apNow},
		"a long time later, still dead": {code, apNow.Add(24 * time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.code == code[:len(code)-1]+"Z" && strings.HasSuffix(code, "Z") {
				t.Skip("the random code already ends in Z")
			}
			require.False(t, enrollCodeLive(t, db, ApproverPasskey, tc.code, fp, tc.at))
			_, err := EnrollApprover(ctx, db, passkeyCred("c-"+name), tc.code, tc.at)
			require.ErrorIs(t, err, ErrEnrollCode)
		})
	}

	first, err := EnrollApprover(ctx, db, passkeyCred("c1"), code, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, AddedByCode, first.AddedBy)
	require.NotEmpty(t, first.ID)

	_, err = EnrollApprover(ctx, db, passkeyCred("c2"), code, apNow.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrEnrollCode, "a code is single use")
	require.False(t, enrollCodeLive(t, db, ApproverPasskey, code, fp, apNow.Add(2*time.Minute)))

	var usedFor string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT used_for FROM approver_enroll_codes WHERE kind = 'passkey'`).Scan(&usedFor))
	require.Equal(t, first.ID, usedFor)

	got, err := ApproverCredentials(ctx, db, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, first.ID, got[0].ID)
	require.Equal(t, []string{"internal", "hybrid"}, got[0].Transports)
	require.True(t, got[0].BackupEligible)
	require.Equal(t, []byte("handle"), got[0].UserHandle)
}

func TestEnrollApprover_ProofRules(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()

	_, err := EnrollApprover(ctx, db, passkeyCred("c1"), "", apNow)
	require.ErrorIs(t, err, ErrEnrollProof, "no code at all")
	_, err = EnrollApprover(ctx, db, passkeyCred("c1"), "   ", apNow)
	require.ErrorIs(t, err, ErrEnrollProof, "a blank code is no code")

	fp := fingerprintOf(passkeyCred("c1"))
	code, _, err := MintEnrollCode(ctx, db, ApproverPasskey, fp, apNow)
	require.NoError(t, err)
	first, err := EnrollApprover(ctx, db, passkeyCred("c1"), code, apNow)
	require.NoError(t, err)
	require.Equal(t, AddedByCode, first.AddedBy)

	// An enrolled credential never stands in for a later one's code: there is
	// no approval path to take.
	_, err = EnrollApprover(ctx, db, passkeyCred("c2"), "", apNow)
	require.ErrorIs(t, err, ErrEnrollProof, "a second credential needs a code too")

	code2, _, err := MintEnrollCode(ctx, db, ApproverPasskey, fp, apNow)
	require.NoError(t, err)
	second, err := EnrollApprover(ctx, db, passkeyCred("c2"), code2, apNow)
	require.NoError(t, err, "a later credential, with a code of its own")
	require.Equal(t, AddedByCode, second.AddedBy)

	code3, _, err := MintEnrollCode(ctx, db, ApproverPasskey, fp, apNow)
	require.NoError(t, err)
	_, err = EnrollApprover(ctx, db, passkeyCred("c1"), code3, apNow)
	require.ErrorIs(t, err, ErrApproverExists)
	require.True(t, enrollCodeLive(t, db, ApproverPasskey, code3, fp, apNow), "a refused enrollment consumes nothing")

	touch := ApproverCredential{Kind: ApproverTouchID, CredentialID: []byte("tk1"), PublicKey: touchKey(7), Alg: -7, Name: "this Mac"}
	tcode, _, err := MintEnrollCode(ctx, db, ApproverTouchID, fingerprintOf(touch), apNow)
	require.NoError(t, err)
	tk, err := EnrollApprover(ctx, db, touch, tcode, apNow)
	require.NoError(t, err)
	require.Equal(t, ApproverTouchID, tk.Kind)

	passkeys, err := ApproverCredentials(ctx, db, ApproverPasskey)
	require.NoError(t, err)
	require.Len(t, passkeys, 2)
	n, err := CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

func TestEnrollApprover_RefusesBadRecords(t *testing.T) {
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
		"a Touch ID key with an RP": {Kind: ApproverTouchID, CredentialID: []byte("x"), PublicKey: touchKey(1), Name: "n", RPID: "localhost"},
		"an RP ID with a space":     {Kind: ApproverPasskey, CredentialID: []byte("x"), PublicKey: []byte{1}, Name: "n", RPID: "a b"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := EnrollApprover(ctx, db, c, "AAAA", apNow)
			require.ErrorIs(t, err, ErrInvalidApprover)
		})
	}
}

// Migration 038: a passkey keeps the RP ID it was enrolled under, a row from
// before the column reads as unknown, and only a known, different RP ID
// strands a passkey.
func TestApproverRPID_RecordedAndStranded(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	named := passkeyCred("named")
	named.RPID = "studio.example.ts.net"
	enrollOne(t, db, named)
	local := passkeyCred("local")
	local.RPID = "localhost"
	enrollOne(t, db, local)
	enrollOne(t, db, touchCred(7, "this mac"))
	// A row the previous release inserted names no rp_id column at all.
	_, err := db.ExecContext(ctx, `INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, name, added_at, added_by)
		VALUES ('old', 'passkey', X'0102', X'a501', -7, 'older key', ?, 'code')`, core.FormatTime(apNow))
	require.NoError(t, err)

	creds, err := ApproverCredentials(ctx, db, "")
	require.NoError(t, err)
	got := map[string]string{}
	for _, c := range creds {
		got[c.Name] = c.RPID
	}
	require.Equal(t, map[string]string{"laptop named": "studio.example.ts.net", "laptop local": "localhost",
		"this mac": "", "older key": ""}, got)

	names := func(cs []ApproverCredential) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	require.Equal(t, []string{"laptop local"}, names(StrandedPasskeys(creds, "studio.example.ts.net")),
		"the localhost passkey cannot sign for the name; the unknown one and the Touch ID key are not judged")
	require.Equal(t, []string{"laptop named"}, names(StrandedPasskeys(creds, "localhost")))
	require.Empty(t, StrandedPasskeys(nil, "localhost"))
}

func enrollOne(t *testing.T, db *sql.DB, c ApproverCredential) ApproverCredential {
	t.Helper()
	ctx := context.Background()
	code, _, err := MintEnrollCode(ctx, db, c.Kind, fingerprintOf(c), apNow)
	require.NoError(t, err)
	got, err := EnrollApprover(ctx, db, c, code, apNow)
	require.NoError(t, err)
	return got
}

func TestRecordApproverUse_CompareAndSet(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	c := enrollOne(t, db, passkeyCred("c1"))

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
	a := enrollOne(t, db, passkeyCred("c1"))
	enrollOne(t, db, passkeyCred("c2"))
	require.NoError(t, RemoveApprover(ctx, db, a.ID))
	require.ErrorIs(t, RemoveApprover(ctx, db, a.ID), ErrApproverNotFound)
	_, err := ApproverByID(ctx, db, a.ID)
	require.ErrorIs(t, err, ErrApproverNotFound)

	// A live lease and a pending code: reset ends both.
	approval, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), apNow)
	require.NoError(t, err)
	_, err = ApproveAccess(ctx, db, approval.ID, approval.Challenge, "cred-1", apNow)
	require.NoError(t, err)
	pendingFP := fingerprintOf(passkeyCred("c3"))
	pending, _, err := MintEnrollCode(ctx, db, ApproverPasskey, pendingFP, apNow)
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
	require.False(t, enrollCodeLive(t, db, ApproverPasskey, pending, pendingFP, apNow.Add(time.Minute)),
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

// A reset ends every console unlock too: a credential granted it.
func TestResetApprovers_EndsConsoleUnlocks(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	token, err := NewConsoleToken()
	require.NoError(t, err)
	terminalUnlock(t, db, "example-finance", HashConsoleToken(token), 3*time.Hour, apNow)
	live, err := AllLiveConsoleUnlocks(ctx, db, apNow)
	require.NoError(t, err)
	require.Len(t, live, 1)

	_, _, err = ResetApprovers(ctx, db, apNow.Add(time.Minute))
	require.NoError(t, err)
	live, err = AllLiveConsoleUnlocks(ctx, db, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, live)
}

func touchCred(b byte, name string) ApproverCredential {
	k := touchKey(b)
	return ApproverCredential{Kind: ApproverTouchID, CredentialID: []byte{b, b}, PublicKey: k, Alg: -7, Name: name}
}

// A Touch ID enrollment retires the credentials it names, in its own
// transaction and only once the new key is stored: never a passkey, never a
// row whose public key is not the one the caller verified, and nothing at all
// when the enrollment itself fails.
func TestEnrollTouchIDRetiring(t *testing.T) {
	ctx := context.Background()
	fresh := func(t *testing.T) (*sql.DB, ApproverCredential, ApproverCredential, ApproverCredential) {
		db := leaseDB(t)
		old := enrollOne(t, db, touchCred(1, "Touch ID on example-mac"))
		other := enrollOne(t, db, touchCred(2, "Touch ID on example-mini"))
		pk := enrollOne(t, db, passkeyCred("c1"))
		return db, old, other, pk
	}
	ids := func(t *testing.T, db *sql.DB) []string {
		rows, err := ApproverCredentials(ctx, db, "")
		require.NoError(t, err)
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	code := func(t *testing.T, db *sql.DB, cred ApproverCredential) string {
		c, _, err := MintEnrollCode(ctx, db, ApproverTouchID, fingerprintOf(cred), apNow)
		require.NoError(t, err)
		return c
	}

	t.Run("retires the named key with its own public key", func(t *testing.T) {
		db, old, other, pk := fresh(t)
		got, retired, err := EnrollTouchIDRetiring(ctx, db, touchCred(3, "Touch ID on example-mac"), code(t, db, touchCred(3, "")), apNow,
			[]RetiredApprover{{ID: old.ID, PublicKey: old.PublicKey}, {ID: old.ID, PublicKey: old.PublicKey}})
		require.NoError(t, err)
		require.Equal(t, []string{old.ID}, retired, "named twice, retired once")
		require.ElementsMatch(t, []string{other.ID, pk.ID, got.ID}, ids(t, db))
	})
	t.Run("never a passkey, another key, a stale key or the new one", func(t *testing.T) {
		db, old, other, pk := fresh(t)
		got, retired, err := EnrollTouchIDRetiring(ctx, db, touchCred(3, "new"), code(t, db, touchCred(3, "")), apNow, []RetiredApprover{
			{ID: pk.ID, PublicKey: pk.PublicKey},     // a passkey
			{ID: other.ID, PublicKey: old.PublicKey}, // another key's id with this key's public key
			{ID: "01NOSUCHAPPROVER0000000000", PublicKey: old.PublicKey},
			{ID: old.ID, PublicKey: touchKey(9)}, // a public key the row does not hold
		})
		require.NoError(t, err)
		require.Empty(t, retired)
		require.ElementsMatch(t, []string{old.ID, other.ID, pk.ID, got.ID}, ids(t, db))
	})
	t.Run("a failed enrollment retires nothing", func(t *testing.T) {
		db, old, other, pk := fresh(t)
		retire := []RetiredApprover{{ID: old.ID, PublicKey: old.PublicKey}}
		_, _, err := EnrollTouchIDRetiring(ctx, db, touchCred(3, "new"), "AAAA-BBBB-CCCC-DDDD", apNow, retire)
		require.ErrorIs(t, err, ErrEnrollCode)
		_, _, err = EnrollTouchIDRetiring(ctx, db, touchCred(1, "the same key again"), code(t, db, touchCred(1, "")), apNow, retire)
		require.ErrorIs(t, err, ErrApproverExists)
		_, _, err = EnrollTouchIDRetiring(ctx, db, touchCred(3, "a\nb"), code(t, db, touchCred(3, "")), apNow, retire)
		require.ErrorIs(t, err, ErrInvalidApprover)
		_, _, err = EnrollTouchIDRetiring(ctx, db, touchCred(3, "no code"), "", apNow, retire)
		require.ErrorIs(t, err, ErrEnrollProof)
		_, _, err = EnrollTouchIDRetiring(ctx, db, passkeyCred("c9"), code(t, db, passkeyCred("c9")), apNow, retire)
		require.ErrorIs(t, err, ErrInvalidApprover, "only a Touch ID key retires one")
		require.ElementsMatch(t, []string{old.ID, other.ID, pk.ID}, ids(t, db))
	})
	t.Run("EnrollApprover retires nothing", func(t *testing.T) {
		db, old, other, pk := fresh(t)
		got, err := EnrollApprover(ctx, db, touchCred(3, "Touch ID on example-mac"), code(t, db, touchCred(3, "")), apNow)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{old.ID, other.ID, pk.ID, got.ID}, ids(t, db))
	})
}

// A code enrolls the key it was minted for and no other: offered with another
// key it finds no row, and is neither consumed nor spent. This is what makes a
// code read off the owner's terminal useless to whoever read it.
func TestEnrollCode_BoundToItsKey(t *testing.T) {
	ctx := context.Background()
	passkeyB := passkeyCred("b")
	passkeyB.PublicKey = []byte{0xa5, 0x09, 0x09}
	for name, tc := range map[string]struct {
		kind       string
		mine, them ApproverCredential
	}{
		"Touch ID": {ApproverTouchID, touchCred(1, "owner"), touchCred(2, "someone else")},
		"passkey":  {ApproverPasskey, passkeyCred("a"), passkeyB},
	} {
		t.Run(name, func(t *testing.T) {
			db := leaseDB(t)
			require.NotEqual(t, fingerprintOf(tc.mine), fingerprintOf(tc.them), "two keys, two fingerprints")
			code, _, err := MintEnrollCode(ctx, db, tc.kind, fingerprintOf(tc.mine), apNow)
			require.NoError(t, err)

			_, err = EnrollApprover(ctx, db, tc.them, code, apNow)
			require.ErrorIs(t, err, ErrEnrollCode, "the same code with another key")
			n, err := CountApprovers(ctx, db)
			require.NoError(t, err)
			require.Zero(t, n, "nothing is enrolled")
			require.True(t, enrollCodeLive(t, db, tc.kind, code, fingerprintOf(tc.mine), apNow), "and the code is not spent")

			got, err := EnrollApprover(ctx, db, tc.mine, code, apNow)
			require.NoError(t, err, "its own key still enrolls with it")
			require.Equal(t, tc.mine.PublicKey, got.PublicKey)

			_, err = EnrollApprover(ctx, db, tc.them, code, apNow)
			require.ErrorIs(t, err, ErrEnrollCode, "spent now, for every key")
		})
	}
}

// A fingerprint that is not one is refused before anything is minted.
func TestMintEnrollCode_RefusesABadFingerprint(t *testing.T) {
	ctx := context.Background()
	db := leaseDB(t)
	for name, bad := range map[string]string{
		"empty":              "",
		"blanks":             "   ",
		"too short":          "Y2WK-2PG4-B7DB-FAT",
		"too long":           "Y2WK-2PG4-B7DB-FATZZ",
		"not base32":         "Y2WK-2PG4-B7DB-FAT!",
		"a symbol it omits":  "UUUU-UUUU-UUUU-UUUU",
		"an account's words": "the owner's own key",
	} {
		t.Run(name, func(t *testing.T) {
			require.Empty(t, FormatKeyFingerprint(bad))
			_, _, err := MintEnrollCode(ctx, db, ApproverPasskey, bad, apNow)
			require.ErrorIs(t, err, ErrInvalidApprover)
			_, _, err = MintEnrollCode(ctx, db, ApproverTouchID, bad, apNow)
			require.ErrorIs(t, err, ErrInvalidApprover)
		})
	}
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_enroll_codes`).Scan(&n))
	require.Zero(t, n, "a refused mint stores nothing")
}

// The fingerprint a person compares between two screens: this vector is
// pinned on the Swift side as well (Messages.keyFingerprint), so the helper's
// window and the daemon name a key the same way.
func TestApproverKeyFingerprint_PinnedVector(t *testing.T) {
	key := append([]byte{0x04}, bytes.Repeat([]byte{0x11}, 64)...)
	require.Len(t, key, 65)
	const want = "Y2WK-2PG4-B7DB-FATZ"
	require.Equal(t, want, ApproverKeyFingerprint(key))
	require.NotEqual(t, want, ApproverKeyFingerprint(append([]byte{0x04}, bytes.Repeat([]byte{0x12}, 64)...)))

	// As typed, it is the same fingerprint: the code binds it, so a sloppy
	// --key still names the key the owner read.
	for _, typed := range []string{want, "y2wk-2pg4-b7db-fatz", "Y2WK2PG4B7DBFATZ", "y2wk 2pg4 b7db fatz"} {
		require.Equal(t, want, FormatKeyFingerprint(typed), typed)
	}
	db := leaseDB(t)
	ctx := context.Background()
	code, _, err := MintEnrollCode(ctx, db, ApproverTouchID, "y2wk-2pg4-b7db-fatz", apNow)
	require.NoError(t, err)
	got, err := EnrollApprover(ctx, db, ApproverCredential{Kind: ApproverTouchID, CredentialID: []byte("k"), PublicKey: key, Alg: -7, Name: "this Mac"}, code, apNow)
	require.NoError(t, err, "a code minted for the key as typed enrolls that key")
	require.Equal(t, key, got.PublicKey)
}
