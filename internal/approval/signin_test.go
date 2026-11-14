package approval

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const signinOrigin = "http://localhost:8081"

// signinClock is a settable clock for the service under test.
type signinClock struct{ now time.Time }

func (c *signinClock) Now() time.Time { return c.now }

func signinService(t *testing.T) (*sql.DB, *SignIn, *signinClock) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	clock := &signinClock{now: time.Now()}
	return db, signinServiceOn(t, db, clock), clock
}

func signinServiceOn(t *testing.T, db *sql.DB, clock *signinClock) *SignIn {
	t.Helper()
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{signinOrigin}})
	require.NoError(t, err)
	svc, err := NewSignIn(SignInConfig{DB: db, Passkeys: v, Origins: []string{signinOrigin}, Now: clock.Now})
	require.NoError(t, err)
	return svc
}

// signinEnroll adds a's passkey through the service and approves its code as
// the owner does on the server.
func signinEnroll(t *testing.T, db *sql.DB, svc *SignIn, a *passkeytest.Authenticator, name string) store.ApproverCredential {
	t.Helper()
	ctx := context.Background()
	begun, err := svc.BeginEnrollment(ctx, name)
	require.NoError(t, err)
	created, err := a.Create(begun.Options, signinOrigin)
	require.NoError(t, err)
	added, err := svc.FinishEnrollment(ctx, begun.Ticket, name, created)
	require.NoError(t, err)
	cred, _, err := store.ApprovePasskeyEnrollment(ctx, db, added.Code, time.Now())
	require.NoError(t, err)
	return cred
}

func signinAuthenticatorOf(t *testing.T, counting bool) *passkeytest.Authenticator {
	t.Helper()
	a, err := passkeytest.New(counting)
	require.NoError(t, err)
	return a
}

// A sign-in needs an enrolled passkey; an enrollment's finish enrolls
// nothing, only the owner's approval of its code does.
func TestSignIn_EnrollmentWaitsForTheOwner(t *testing.T) {
	db, svc, _ := signinService(t)
	ctx := context.Background()
	_, err := svc.BeginSignIn(ctx, "browser-key")
	require.ErrorIs(t, err, ErrNoPasskey)
	has, err := svc.HasPasskey(ctx)
	require.NoError(t, err)
	require.False(t, has)

	a := signinAuthenticatorOf(t, false)
	begun, err := svc.BeginEnrollment(ctx, "phone")
	require.NoError(t, err)
	created, err := a.Create(begun.Options, signinOrigin)
	require.NoError(t, err)
	added, err := svc.FinishEnrollment(ctx, begun.Ticket, "phone", created)
	require.NoError(t, err)
	require.Equal(t, "phone", added.Name)
	n, err := store.CountApprovers(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n, "the finish enrolls nothing")
	e, err := svc.Enrollment(ctx, added.ID)
	require.NoError(t, err)
	require.Equal(t, store.EnrollmentPending, e.Status)
	require.Equal(t, added.Fingerprint, e.Fingerprint())
	require.Equal(t, "localhost", e.Credential.RPID, "the passkey's relying party is recorded")

	// Sent again, the same passkey does not wait twice.
	_, err = svc.FinishEnrollment(ctx, begun.Ticket, "phone", created)
	require.ErrorIs(t, err, store.ErrEnrollmentWaiting)

	cred, _, err := store.ApprovePasskeyEnrollment(ctx, db, added.Code, time.Now())
	require.NoError(t, err)
	require.Equal(t, a.CredentialID(), cred.CredentialID)
	has, err = svc.HasPasskey(ctx)
	require.NoError(t, err)
	require.True(t, has)
}

// The enrollment's challenge is bound to the name it was begun for, and its
// ticket to this process.
func TestSignIn_EnrollmentTicketBindsNameAndProcess(t *testing.T) {
	db, svc, clock := signinService(t)
	ctx := context.Background()
	begun, err := svc.BeginEnrollment(ctx, "phone")
	require.NoError(t, err)
	created, err := signinAuthenticatorOf(t, false).Create(begun.Options, signinOrigin)
	require.NoError(t, err)
	_, err = svc.FinishEnrollment(ctx, begun.Ticket, "laptop", created)
	require.ErrorIs(t, err, passkey.ErrVerification, "another name fails the challenge")

	restarted := signinServiceOn(t, db, clock)
	_, err = restarted.FinishEnrollment(ctx, begun.Ticket, "phone", created)
	require.ErrorIs(t, err, passkey.ErrVerification, "a ticket from before a restart fails its challenge")

	clock.now = clock.now.Add(enrollTicketTTL)
	_, err = svc.FinishEnrollment(ctx, begun.Ticket, "phone", created)
	require.ErrorIs(t, err, ErrSignInTicket, "past its deadline")

	for _, bad := range []string{"", "v1", "v2.1.AAAAAAAAAAAAAAAAAAAAAA", "v1.x.AAAAAAAAAAAAAAAAAAAAAA", "v1.99999999999.short"} {
		_, err := svc.FinishEnrollment(ctx, bad, "phone", created)
		require.ErrorIs(t, err, ErrSignInTicket, "%q", bad)
	}
	_, err = svc.BeginEnrollment(ctx, "bad\x1bname")
	require.ErrorIs(t, err, store.ErrInvalidApprover)
}

// A sign-in's assertion is bound to the browser key that began it, works once
// and only within its ticket's time, and records the passkey's use.
func TestSignIn_AssertionIsBoundToTheBrowserKey(t *testing.T) {
	db, svc, clock := signinService(t)
	ctx := context.Background()
	a := signinAuthenticatorOf(t, false)
	cred := signinEnroll(t, db, svc, a, "phone")

	_, err := svc.BeginSignIn(ctx, "")
	require.ErrorIs(t, err, ErrNoBrowserKey)
	begun, err := svc.BeginSignIn(ctx, "key-A")
	require.NoError(t, err)
	asserted, err := a.Get(begun.Options, signinOrigin)
	require.NoError(t, err)

	_, err = svc.FinishSignIn(ctx, "key-B", begun.Ticket, asserted)
	require.ErrorIs(t, err, passkey.ErrVerification, "another browser's key fails the challenge")
	signed, err := svc.FinishSignIn(ctx, "key-A", begun.Ticket, asserted)
	require.NoError(t, err)
	require.Equal(t, cred.ID, signed.Credential.ID)
	require.NotEmpty(t, signed.Cookie)
	require.WithinDuration(t, clock.now.Add(SignInLifetime), signed.SignIn.ExpiresAt, time.Second)
	live, err := store.LiveConsoleSignIn(ctx, db, store.HashConsoleSignIn(signed.Cookie, "key-A"), clock.now)
	require.NoError(t, err)
	require.Equal(t, signed.SignIn.ID, live.ID)
	used, err := store.ApproverByID(ctx, db, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, used.LastUsedAt, "the passkey's use is recorded")

	// A synced passkey's counter stays 0, so only the ticket stops a replay.
	_, err = svc.FinishSignIn(ctx, "key-A", begun.Ticket, asserted)
	require.ErrorIs(t, err, ErrSignInTicket)

	later, err := svc.BeginSignIn(ctx, "key-A")
	require.NoError(t, err)
	late, err := a.Get(later.Options, signinOrigin)
	require.NoError(t, err)
	clock.now = clock.now.Add(signInTicketTTL)
	_, err = svc.FinishSignIn(ctx, "key-A", later.Ticket, late)
	require.ErrorIs(t, err, ErrSignInTicket, "past its deadline")
}

// A counting authenticator's replayed assertion fails its counter as well.
func TestSignIn_CountingAuthenticatorReplay(t *testing.T) {
	db, svc, _ := signinService(t)
	ctx := context.Background()
	a := signinAuthenticatorOf(t, true)
	signinEnroll(t, db, svc, a, "security key")
	first, err := svc.BeginSignIn(ctx, "key-A")
	require.NoError(t, err)
	asserted, err := a.Get(first.Options, signinOrigin)
	require.NoError(t, err)
	_, err = svc.FinishSignIn(ctx, "key-A", first.Ticket, asserted)
	require.NoError(t, err)
	second, err := svc.BeginSignIn(ctx, "key-A")
	require.NoError(t, err)
	_, err = svc.FinishSignIn(ctx, "key-A", second.Ticket, asserted)
	require.Error(t, err, "an old assertion over a new ticket")
}

// A restricted account reaches none of it.
func TestSignIn_RefusesARestrictedAccount(t *testing.T) {
	_, svc, _ := signinService(t)
	accounts := map[string]access.Account{"owner": {UID: 501, Name: "owner"}, "example-helper": {UID: 502, Name: "example-helper"}}
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"example-helper"}},
		func(n string) (access.Account, error) { return accounts[n], nil })
	require.NoError(t, err)
	v, ok := p.PeerView(502)
	require.True(t, ok)
	ctx := access.WithView(context.Background(), v)
	_, err = svc.BeginSignIn(ctx, "key")
	require.ErrorIs(t, err, ErrRestricted)
	_, err = svc.BeginEnrollment(ctx, "phone")
	require.ErrorIs(t, err, ErrRestricted)
	_, err = svc.FinishSignIn(ctx, "key", "v1.1.x", nil)
	require.ErrorIs(t, err, ErrRestricted)
	_, err = svc.FinishEnrollment(ctx, "v1.1.x", "phone", nil)
	require.ErrorIs(t, err, ErrRestricted)
}
