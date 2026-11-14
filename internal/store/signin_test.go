package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// signinCred is a passkey a device made, as the Add a passkey page records it,
// with a public key of its own so two devices' keys differ.
func signinCred(id string) ApproverCredential {
	c := passkeyCred(id)
	c.PublicKey = []byte("cose-key-" + id)
	c.RPID = "localhost"
	return c
}

// A sign-in's hash covers the cookie and the browser's key together, under a
// prefix of its own: neither alone gives it, and no unlock's hash is one.
func TestHashConsoleSignIn_BindsTheCookieToTheKey(t *testing.T) {
	h := HashConsoleSignIn("cookie", "key")
	require.Len(t, h, 64)
	require.NotEqual(t, h, HashConsoleSignIn("cookie", "other-key"))
	require.NotEqual(t, h, HashConsoleSignIn("other-cookie", "key"))
	require.NotEqual(t, h, HashBrowserUnlock("cookie", "key"), "a sign-in is never an unlock")
	require.NotEqual(t, h, HashConsoleToken("cookie"))
	require.NotEqual(t, h, hashSignInCookie("cookie"))
	require.Empty(t, HashConsoleSignIn("", "key"))
	require.Empty(t, HashConsoleSignIn("cookie", ""))
}

// A sign-in is live for its lifetime and no longer, named only by its cookie
// and key together; a sign-out ends it by the cookie alone.
func TestConsoleSignIn_LifetimeAndSignOut(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	s, err := CreateConsoleSignIn(ctx, db, "cookie-a", "key-a", "01CRED", time.Hour, apNow)
	require.NoError(t, err)
	require.Equal(t, apNow.Add(time.Hour), s.ExpiresAt)

	got, err := LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-a", "key-a"), apNow.Add(59*time.Minute))
	require.NoError(t, err)
	require.Equal(t, s.ID, got.ID)
	require.Equal(t, "01CRED", got.Credential)
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-a", "key-a"), apNow.Add(time.Hour))
	require.ErrorIs(t, err, ErrSignInNotFound, "expired at its expiry")
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-a", "key-b"), apNow)
	require.ErrorIs(t, err, ErrSignInNotFound, "another key")
	_, err = LiveConsoleSignIn(ctx, db, "not-a-hash", apNow)
	require.ErrorIs(t, err, ErrSignInNotFound)

	n, err := EndConsoleSignInByCookie(ctx, db, "cookie-other", apNow)
	require.NoError(t, err)
	require.Zero(t, n)
	n, err = EndConsoleSignInByCookie(ctx, db, "cookie-a", apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-a", "key-a"), apNow.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrSignInNotFound, "signed out")
	var reason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT ended_reason FROM console_signins WHERE id = ?`, s.ID).Scan(&reason))
	require.Equal(t, SignInEndSignedOut, reason)

	for _, bad := range []struct{ cookie, key, cred string }{{"", "k", "c"}, {"c", "", "c"}, {"c", "k", " "}} {
		_, err := CreateConsoleSignIn(ctx, db, bad.cookie, bad.key, bad.cred, time.Hour, apNow)
		require.ErrorIs(t, err, ErrInvalidSignIn)
	}
	_, err = EndAllConsoleSignIns(ctx, db, "forgotten", apNow)
	require.ErrorIs(t, err, ErrInvalidSignIn, "an end reason outside the set")
}

// A new sign-in prunes the records that ended or expired long ago, and keeps
// the recent ones.
func TestConsoleSignIn_PrunesOldRecords(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	_, err := CreateConsoleSignIn(ctx, db, "old", "k", "01CRED", time.Hour, apNow.Add(-40*24*time.Hour))
	require.NoError(t, err)
	_, err = CreateConsoleSignIn(ctx, db, "recent", "k", "01CRED", time.Hour, apNow.Add(-2*24*time.Hour))
	require.NoError(t, err)
	_, err = CreateConsoleSignIn(ctx, db, "new", "k", "01CRED", time.Hour, apNow)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_signins`).Scan(&n))
	require.Equal(t, 2, n)
}

// The code a device shows names its own waiting passkey and carries no key:
// approving it enrolls exactly that passkey, unchanged, and no other; another
// device's passkey waits for its own code; a code works once.
func TestPasskeyEnrollment_ACodeEnrollsThePasskeyItNamesAlone(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	mine, theirs := signinCred("phone"), signinCred("attacker")
	e, code, err := BeginPasskeyEnrollment(ctx, db, mine, PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)
	require.Equal(t, EnrollmentPending, e.Status)
	require.Equal(t, apNow.Add(PasskeyEnrollmentTTL), e.PendingUntil)
	require.Regexp(t, `^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`, code)
	_, theirCode, err := BeginPasskeyEnrollment(ctx, db, theirs, PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)

	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT code_hash FROM passkey_enrollments WHERE id = ?`, e.ID).Scan(&stored))
	require.NotContains(t, stored, NormalizeEnrollCode(code), "only the code's hash is kept")

	shown, err := PasskeyEnrollmentByCode(ctx, db, " "+code+" ", apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, e.ID, shown.ID)
	require.Equal(t, ApproverKeyFingerprint(mine.PublicKey), shown.Fingerprint())

	cred, approved, err := ApprovePasskeyEnrollment(ctx, db, code, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, e.ID, approved.ID)
	require.Equal(t, mine.CredentialID, cred.CredentialID)
	require.Equal(t, mine.PublicKey, cred.PublicKey)
	require.Equal(t, mine.Alg, cred.Alg)
	require.Equal(t, mine.Transports, cred.Transports)
	require.Equal(t, mine.BackupState, cred.BackupState)
	require.Equal(t, mine.UserHandle, cred.UserHandle)
	require.Equal(t, mine.Name, cred.Name)
	require.Equal(t, "localhost", cred.RPID)
	require.Equal(t, AddedByCode, cred.AddedBy)
	back, err := ApproverByID(ctx, db, cred.ID)
	require.NoError(t, err)
	require.Equal(t, mine.PublicKey, back.PublicKey)

	creds, err := ApproverCredentials(ctx, db, ApproverPasskey)
	require.NoError(t, err)
	require.Len(t, creds, 1, "the other device's passkey is not enrolled by this code")
	_, _, err = ApprovePasskeyEnrollment(ctx, db, code, apNow.Add(time.Minute))
	require.ErrorIs(t, err, ErrEnrollmentNotFound, "a code works once")
	row, err := PasskeyEnrollmentByID(ctx, db, e.ID)
	require.NoError(t, err)
	require.Equal(t, EnrollmentEnrolled, row.Status)
	require.Equal(t, cred.ID, row.EnrolledAs)

	waiting, err := PendingPasskeyEnrollments(ctx, db, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, waiting, 1)
	require.Equal(t, theirs.CredentialID, waiting[0].Credential.CredentialID, "the other device's passkey still waits")
	theirCred, _, err := ApprovePasskeyEnrollment(ctx, db, theirCode, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, theirs.PublicKey, theirCred.PublicKey, "its own code enrolls its own key")

	_, _, err = ApprovePasskeyEnrollment(ctx, db, "ABC-!", apNow)
	require.ErrorIs(t, err, ErrInvalidEnrollment)
	_, _, err = ApprovePasskeyEnrollment(ctx, db, "ABCD-EFGH", apNow)
	require.ErrorIs(t, err, ErrEnrollmentNotFound)
}

// A waiting passkey goes with its deadline; the same passkey waits once; an
// enrolled one is not offered again; the queue is capped.
func TestPasskeyEnrollment_DeadlineDuplicatesAndCap(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	c := signinCred("phone")
	_, code, err := BeginPasskeyEnrollment(ctx, db, c, PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)
	_, _, err = BeginPasskeyEnrollment(ctx, db, c, PasskeyEnrollmentTTL, apNow)
	require.ErrorIs(t, err, ErrEnrollmentWaiting)

	late := apNow.Add(PasskeyEnrollmentTTL)
	_, _, err = ApprovePasskeyEnrollment(ctx, db, code, late)
	require.ErrorIs(t, err, ErrEnrollmentNotFound, "past its deadline")
	_, err = PasskeyEnrollmentByCode(ctx, db, code, late)
	require.ErrorIs(t, err, ErrEnrollmentNotFound)

	_, code, err = BeginPasskeyEnrollment(ctx, db, c, PasskeyEnrollmentTTL, late)
	require.NoError(t, err, "an expired wait is purged, so the passkey can wait again")
	_, _, err = ApprovePasskeyEnrollment(ctx, db, code, late)
	require.NoError(t, err)
	_, _, err = BeginPasskeyEnrollment(ctx, db, c, PasskeyEnrollmentTTL, late)
	require.ErrorIs(t, err, ErrApproverExists, "an enrolled passkey")

	for i := range maxPendingEnrollments {
		_, _, err := BeginPasskeyEnrollment(ctx, db, signinCred(fmt.Sprintf("d%02d", i)), PasskeyEnrollmentTTL, late)
		require.NoError(t, err)
	}
	_, _, err = BeginPasskeyEnrollment(ctx, db, signinCred("one-too-many"), PasskeyEnrollmentTTL, late)
	require.ErrorIs(t, err, ErrEnrollmentBusy)
	_, _, err = BeginPasskeyEnrollment(ctx, db, signinCred("after-the-wait"), PasskeyEnrollmentTTL, late.Add(PasskeyEnrollmentTTL))
	require.NoError(t, err, "a full queue empties with its deadline")
}

// The code is re-rolled while another waiting passkey shows it.
func TestPasskeyEnrollment_CodeReRolled(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	codes := []string{"AAAA-AAAA", "AAAA-AAAA", "BBBB-BBBB"}
	old := newEnrollmentCode
	t.Cleanup(func() { newEnrollmentCode = old })
	newEnrollmentCode = func() (string, error) {
		c := codes[0]
		codes = codes[1:]
		return c, nil
	}
	_, first, err := BeginPasskeyEnrollment(ctx, db, signinCred("one"), PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)
	_, second, err := BeginPasskeyEnrollment(ctx, db, signinCred("two"), PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)
	require.Equal(t, "AAAA-AAAA", first)
	require.Equal(t, "BBBB-BBBB", second)
}

// A name prints in the owner's terminal when the code is approved: no control
// or format character, which could rewrite what the owner reads, gets in.
func TestPasskeyEnrollment_NamesAreText(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for _, name := range []string{"", "   ", "phone\x1b[2J", "phone\u202eenohp", "line\nbreak", "tab\tbed", string(make([]rune, 65))} {
		c := signinCred("x")
		c.Name = name
		_, _, err := BeginPasskeyEnrollment(ctx, db, c, PasskeyEnrollmentTTL, apNow)
		require.ErrorIs(t, err, ErrInvalidApprover, "%q", name)
	}
	c := signinCred("y")
	c.Name = "  Telefon von Zoë  "
	e, _, err := BeginPasskeyEnrollment(ctx, db, c, PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)
	require.Equal(t, "Telefon von Zoë", e.Credential.Name)

	touch := signinCred("z")
	touch.Kind = ApproverTouchID
	_, _, err = BeginPasskeyEnrollment(ctx, db, touch, PasskeyEnrollmentTTL, apNow)
	require.ErrorIs(t, err, ErrInvalidEnrollment, "only a passkey is enrolled this way")
}

// Removing a passkey signs out the browsers it signed in; reset ends every
// sign-in and every waiting passkey with the credentials.
func TestApprovers_RemoveAndResetEndSignIns(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a := enrollOne(t, db, passkeyCred("a"))
	b := enrollOne(t, db, passkeyCred("b"))
	_, err := CreateConsoleSignIn(ctx, db, "cookie-1", "key-1", a.ID, time.Hour, apNow)
	require.NoError(t, err)
	_, err = CreateConsoleSignIn(ctx, db, "cookie-2", "key-2", a.ID, time.Hour, apNow)
	require.NoError(t, err)
	_, err = CreateConsoleSignIn(ctx, db, "cookie-3", "key-3", b.ID, time.Hour, apNow)
	require.NoError(t, err)

	n, err := RemoveApproverSigningOut(ctx, db, a.ID, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	live := func(cookie, key string) bool {
		_, err := LiveConsoleSignIn(ctx, db, HashConsoleSignIn(cookie, key), apNow.Add(2*time.Minute))
		if err == nil {
			return true
		}
		require.ErrorIs(t, err, ErrSignInNotFound)
		return false
	}
	require.False(t, live("cookie-1", "key-1"))
	require.False(t, live("cookie-2", "key-2"))
	require.True(t, live("cookie-3", "key-3"), "another passkey's sign-in stays")
	var reason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT ended_reason FROM console_signins WHERE credential = ? LIMIT 1`, a.ID).Scan(&reason))
	require.Equal(t, SignInEndRemoved, reason)
	_, err = RemoveApproverSigningOut(ctx, db, a.ID, apNow)
	require.ErrorIs(t, err, ErrApproverNotFound)

	_, code, err := BeginPasskeyEnrollment(ctx, db, signinCred("waiting"), PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)
	r, err := ResetApproversAll(ctx, db, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, ApproversReset{Credentials: 1, SignIns: 1, Enrollments: 1}, r)
	require.False(t, live("cookie-3", "key-3"))
	_, _, err = ApprovePasskeyEnrollment(ctx, db, code, apNow.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrEnrollmentNotFound, "a passkey that waited through a reset is not enrolled after it")

	// The old signature counts what it always did.
	creds, leases, err := ResetApprovers(ctx, db, apNow.Add(3*time.Minute))
	require.NoError(t, err)
	require.Zero(t, creds)
	require.Zero(t, leases)
}

// A restored database ends the sign-ins and the waiting passkeys it brings
// back, as it ends console unlocks (needs the access_reset.go change this
// work hands over: ResetRestoredAccess's console_signins and
// passkey_enrollments steps).
func TestResetRestoredAccess_EndsSignInsAndWaitingPasskeys(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	_, err := CreateConsoleSignIn(ctx, db, "cookie", "key", "01CRED", time.Hour, apNow)
	require.NoError(t, err)
	_, code, err := BeginPasskeyEnrollment(ctx, db, signinCred("phone"), PasskeyEnrollmentTTL, apNow)
	require.NoError(t, err)

	got, err := ResetRestoredAccess(ctx, db, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Total(), "the sign-in and the waiting passkey, counted")
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie", "key"), apNow.Add(time.Minute))
	require.ErrorIs(t, err, ErrSignInNotFound)
	_, _, err = ApprovePasskeyEnrollment(ctx, db, code, apNow.Add(time.Minute))
	require.ErrorIs(t, err, ErrEnrollmentNotFound)
}

// The two tables migration 041 adds exist on a fresh database, and start empty.
func TestMigration041_TablesStartEmpty(t *testing.T) {
	db := leaseDB(t)
	for _, table := range []string{"console_signins", "passkey_enrollments"} {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n), table)
		require.Zero(t, n, table)
	}
	var v int
	require.NoError(t, db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v))
	require.GreaterOrEqual(t, v, 41)
}
