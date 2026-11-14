package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// A terminal sign-in is begun under a fresh cookie bound to the browser's key,
// signs nothing in until its code is approved, and is seen only by the
// browser holding both: a cookie with another key finds nothing.
func TestTerminalSignIn_BoundToTheBrowserThatBegan(t *testing.T) {
	db, svc, clock := signinService(t)
	ctx := context.Background()
	_, err := svc.BeginTerminalSignIn(ctx, "")
	require.ErrorIs(t, err, ErrNoBrowserKey)

	begun, err := svc.BeginTerminalSignIn(ctx, "key-A")
	require.NoError(t, err)
	require.NotEmpty(t, begun.Cookie)
	require.NotEmpty(t, store.FormatUnlockCode(begun.Code))
	require.Equal(t, clock.now.Add(store.TerminalSignInTTL).Unix(), begun.PendingUntil.Unix())
	again, err := svc.BeginTerminalSignIn(ctx, "key-A")
	require.NoError(t, err)
	require.NotEqual(t, begun.Cookie, again.Cookie, "every begin is a fresh cookie")

	_, state, err := svc.TerminalSignInState(ctx, begun.ID, begun.Cookie, "key-A")
	require.NoError(t, err)
	require.Equal(t, store.TerminalSignInWaiting, state)
	_, _, err = svc.TerminalSignInState(ctx, begun.ID, begun.Cookie, "key-B")
	require.ErrorIs(t, err, store.ErrTerminalSignInNotFound)
	_, _, err = svc.TerminalSignInState(ctx, begun.ID, again.Cookie, "key-A")
	require.ErrorIs(t, err, store.ErrTerminalSignInNotFound, "another of this browser's cookies is another sign-in")

	_, err = store.ApproveTerminalSignIn(ctx, db, begun.ID, begun.Code, SignInLifetime, clock.now)
	require.NoError(t, err)
	row, state, err := svc.TerminalSignInState(ctx, begun.ID, begun.Cookie, "key-A")
	require.NoError(t, err)
	require.Equal(t, store.TerminalSignInLive, state)
	require.Equal(t, store.SignInByDaemon, row.Credential)

	clock.now = clock.now.Add(SignInLifetime)
	_, state, err = svc.TerminalSignInState(ctx, begun.ID, begun.Cookie, "key-A")
	require.NoError(t, err)
	require.Equal(t, store.TerminalSignInExpired, state, "it lasts as long as a passkey's sign-in")
}

// A restricted account begins none and reads none.
func TestTerminalSignIn_RefusesARestrictedAccount(t *testing.T) {
	_, svc, _ := signinService(t)
	accounts := map[string]access.Account{"owner": {UID: 501, Name: "owner"}, "example-helper": {UID: 502, Name: "example-helper"}}
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"example-helper"}},
		func(n string) (access.Account, error) { return accounts[n], nil })
	require.NoError(t, err)
	v, ok := p.PeerView(502)
	require.True(t, ok)
	ctx := access.WithView(context.Background(), v)
	_, err = svc.BeginTerminalSignIn(ctx, "key")
	require.ErrorIs(t, err, ErrRestricted)
	_, _, err = svc.TerminalSignInState(ctx, "x", "c", "key")
	require.ErrorIs(t, err, ErrRestricted)
}
