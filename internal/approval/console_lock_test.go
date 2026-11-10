package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// lockFixture is a Service with one confidential project to lock, and a passkey.
func lockFixture(t *testing.T) (*fixture, *passkeytest.Authenticator) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	f.svc.cfg.Events = events.NewRecorder(f.db)
	f.svc.cfg.ForceLocked = []string{"example-file"}
	for _, slug := range []string{"example-conf", "example-open", "example-file"} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, "example-conf", core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, "example-file", core.IsolationConfidential))
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	return f, key
}

func TestLockChange_RoundTrip(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()

	_, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.ErrorIs(t, err, ErrNoPasskey, "no identity yet: nothing can prove the change")
	cred := f.enrollWithCode(t, key, "key")

	begun, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.NoError(t, err)
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)
	row, err := f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionAdd, resp)
	require.NoError(t, err)
	require.Equal(t, cred.ID, row.ID)

	locks, err := store.ConsoleLocks(ctx, f.db)
	require.NoError(t, err)
	require.Len(t, locks, 1)
	require.Equal(t, "example-conf", locks[0].Project)
	require.Equal(t, cred.ID, locks[0].Credential, "the lock names the passkey that proved it")
	used, err := store.ApproverByID(ctx, f.db, cred.ID)
	require.NoError(t, err)
	require.NotNil(t, used.LastUsedAt, "the passkey's use is recorded")

	_, err = f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.ErrorIs(t, err, store.ErrConsoleLockExists)

	begun, err = f.svc.BeginLockChange(ctx, "example-conf", LockActionRemove)
	require.NoError(t, err)
	resp, err = key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionRemove, resp)
	require.NoError(t, err)
	has, err := store.HasConsoleLock(ctx, f.db, "example-conf")
	require.NoError(t, err)
	require.False(t, has)
}

// The console changes only what it set, and only with a fence in place.
func TestLockChange_Refusals(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()
	f.enrollWithCode(t, key, "key")

	for _, action := range []LockAction{LockActionAdd, LockActionRemove} {
		_, err := f.svc.BeginLockChange(ctx, "example-file", action)
		require.ErrorIs(t, err, ErrForceLocked, string(action))
	}
	_, err := f.svc.BeginLockChange(ctx, "example-open", LockActionAdd)
	require.ErrorIs(t, err, store.ErrConsoleLockUnfenced)
	_, err = f.svc.BeginLockChange(ctx, "example-gone", LockActionAdd)
	require.ErrorIs(t, err, store.ErrConsoleLockUnfenced, "an unregistered slug has no fence")
	_, err = f.svc.BeginLockChange(ctx, "example-conf", LockActionRemove)
	require.ErrorIs(t, err, store.ErrConsoleLockNotFound)
	_, err = f.svc.BeginLockChange(ctx, "example-conf", LockAction("open"))
	require.ErrorIs(t, err, ErrInvalidLockAction)
	_, err = ParseLockAction("")
	require.ErrorIs(t, err, ErrInvalidLockAction)
}

// An assertion proves one ceremony: one project, one direction, once.
func TestLockChange_AssertionIsBound(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()
	f.enrollWithCode(t, key, "key")
	locked := func() bool {
		t.Helper()
		has, err := store.HasConsoleLock(ctx, f.db, "example-conf")
		require.NoError(t, err)
		return has
	}

	cases := map[string]func(id string, resp []byte) error{
		"the other direction": func(id string, resp []byte) error {
			_, err := f.svc.FinishLockChange(ctx, id, "example-conf", LockActionRemove, resp)
			return err
		},
		"another project": func(id string, resp []byte) error {
			_, err := f.svc.FinishLockChange(ctx, id, "example-open", LockActionAdd, resp)
			return err
		},
		"an unknown ceremony": func(_ string, resp []byte) error {
			_, err := f.svc.FinishLockChange(ctx, "no-such-id", "example-conf", LockActionAdd, resp)
			return err
		},
	}
	for name, finish := range cases {
		begun, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
		require.NoError(t, err, name)
		resp, err := key.Get(begun.Options, origin)
		require.NoError(t, err, name)
		require.ErrorIs(t, finish(begun.ID, resp), ErrNoLockChange, name)
		require.False(t, locked(), name)
	}

	begun, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.NoError(t, err)
	_, err = f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionAdd, []byte(`{"id":"x"}`))
	require.Error(t, err, "garbage is not an assertion")
	require.False(t, locked())
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionAdd, resp)
	require.ErrorIs(t, err, ErrNoLockChange, "a failed finish ends the ceremony")

	begun, err = f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.NoError(t, err)
	resp, err = key.Get(begun.Options, origin)
	require.NoError(t, err)
	f.now = f.now.Add(lockChangeTTL + time.Second)
	_, err = f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionAdd, resp)
	require.ErrorIs(t, err, ErrNoLockChange, "a ceremony expires")
	require.False(t, locked())
}

// A lock change is made once per ceremony, and a restored assertion for a
// finished one does nothing.
func TestLockChange_SingleUse(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()
	f.enrollWithCode(t, key, "key")
	begun, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.NoError(t, err)
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionAdd, resp)
	require.NoError(t, err)
	_, err = f.svc.FinishLockChange(ctx, begun.ID, "example-conf", LockActionAdd, resp)
	require.ErrorIs(t, err, ErrNoLockChange)
}
