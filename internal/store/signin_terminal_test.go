package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A terminal sign-in waits, not live, until the daemon's account approves its
// code; approval signs in the browser that began it, bound to its cookie and
// key, and that browser only.
func TestTerminalSignIn_ApprovalSignsInOnlyTheBrowserThatBegan(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	row, code, err := BeginTerminalSignIn(ctx, db, "cookie-a", "key-a", apNow)
	require.NoError(t, err)
	require.NotEmpty(t, FormatUnlockCode(code))

	tokenA := HashConsoleSignIn("cookie-a", "key-a")
	_, err = LiveConsoleSignIn(ctx, db, tokenA, apNow)
	require.ErrorIs(t, err, ErrSignInNotFound, "a waiting sign-in is never live")
	n, err := CountLiveConsoleSignIns(ctx, db, "", apNow)
	require.NoError(t, err)
	require.Zero(t, n)
	_, state, err := TerminalSignInFor(ctx, db, row.ID, tokenA, apNow)
	require.NoError(t, err)
	require.Equal(t, TerminalSignInWaiting, state)
	_, _, err = TerminalSignInFor(ctx, db, row.ID, HashConsoleSignIn("cookie-a", "key-b"), apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "the cookie with another browser's key names nothing")

	s, err := ApproveTerminalSignIn(ctx, db, code, 7*24*time.Hour, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, SignInByDaemon, s.Credential)
	require.Equal(t, apNow.Add(time.Minute+7*24*time.Hour), s.ExpiresAt)

	live, err := LiveConsoleSignIn(ctx, db, tokenA, apNow.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, row.ID, live.ID)
	for _, other := range []string{HashConsoleSignIn("cookie-a", "key-b"), HashConsoleSignIn("cookie-b", "key-a")} {
		_, err = LiveConsoleSignIn(ctx, db, other, apNow.Add(2*time.Minute))
		require.ErrorIs(t, err, ErrSignInNotFound, "the code signed in no other cookie or key")
	}
	_, state, err = TerminalSignInFor(ctx, db, row.ID, tokenA, apNow.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, TerminalSignInLive, state)

	_, err = ApproveTerminalSignIn(ctx, db, code, time.Hour, apNow.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "a code works once")
	since, err := PasskeySignInsSince(ctx, db, apNow.Add(-time.Hour))
	require.NoError(t, err)
	require.Zero(t, since, "the terminal's sign-in is no passkey's proof")
}

// A code is refused malformed, mistyped, past its deadline, and once its
// browser signed out or a restore ended it.
func TestTerminalSignIn_Refusals(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	_, err := ApproveTerminalSignIn(ctx, db, "bad!", time.Hour, apNow)
	require.ErrorIs(t, err, ErrInvalidSignIn)
	_, _, err = BeginTerminalSignIn(ctx, db, "", "key", apNow)
	require.ErrorIs(t, err, ErrInvalidSignIn, "no cookie, nothing to bind")
	_, _, err = BeginTerminalSignIn(ctx, db, "cookie", "", apNow)
	require.ErrorIs(t, err, ErrInvalidSignIn, "no key, nothing to bind")

	_, late, err := BeginTerminalSignIn(ctx, db, "cookie-late", "key", apNow)
	require.NoError(t, err)
	_, err = ApproveTerminalSignIn(ctx, db, late, time.Hour, apNow.Add(TerminalSignInTTL))
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "past its deadline")

	_, out, err := BeginTerminalSignIn(ctx, db, "cookie-out", "key", apNow)
	require.NoError(t, err)
	n, err := EndConsoleSignInByCookie(ctx, db, "cookie-out", apNow)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "signing out ends a waiting sign-in of that cookie")
	_, err = ApproveTerminalSignIn(ctx, db, out, time.Hour, apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound)

	_, reset, err := BeginTerminalSignIn(ctx, db, "cookie-reset", "key", apNow)
	require.NoError(t, err)
	_, err = EndAllConsoleSignIns(ctx, db, SignInEndReset, apNow)
	require.NoError(t, err)
	_, err = ApproveTerminalSignIn(ctx, db, reset, time.Hour, apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "a restore ends the codes it found waiting")
}

// The waiting sign-ins are capped, so a caller looping on the begin route
// cannot grow the table; ones past their deadline make room.
func TestTerminalSignIn_Capped(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for i := range maxWaitingTerminalSignIns {
		_, _, err := BeginTerminalSignIn(ctx, db, "cookie-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "key", apNow)
		require.NoError(t, err)
	}
	_, _, err := BeginTerminalSignIn(ctx, db, "one-more", "key", apNow)
	require.ErrorIs(t, err, ErrTerminalSignInBusy)
	_, _, err = BeginTerminalSignIn(ctx, db, "one-more", "key", apNow.Add(TerminalSignInTTL))
	require.NoError(t, err, "the expired ones are purged first")
}

// The proof setup waits for counts a passkey's sign-ins made since a time,
// and nothing else.
func TestPasskeySignInsSince(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	_, err := CreateConsoleSignIn(ctx, db, "c1", "k1", "01CRED", time.Hour, apNow)
	require.NoError(t, err)
	_, _, err = BeginTerminalSignIn(ctx, db, "c2", "k2", apNow.Add(time.Minute))
	require.NoError(t, err)
	n, err := PasskeySignInsSince(ctx, db, apNow)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = PasskeySignInsSince(ctx, db, apNow.Add(time.Second))
	require.NoError(t, err)
	require.Zero(t, n, "a sign-in before the time is not counted")
}
