package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// fileDigest is a backup file's digest as backup.Service.Digest writes it.
func fileDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

var (
	digestA = fileDigest("backup A")
	digestB = fileDigest("backup B")
)

// restoreFixture is a Service with a passkey enrolled.
func restoreFixture(t *testing.T) (*fixture, *passkeytest.Authenticator, store.ApproverCredential) {
	t.Helper()
	f := newFixture(t)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	return f, key, f.enrollWithCode(t, key, "key")
}

// The modes are the backup package's words: the console passes one straight
// through to backup.RestoreRequest.Mode.
func TestRestoreMode_IsTheBackupPackagesWord(t *testing.T) {
	require.Equal(t, backup.ModeReplace, string(RestoreReplace))
	require.Equal(t, backup.ModeMerge, string(RestoreMerge))
	for _, s := range []string{"replace", "merge"} {
		m, err := ParseRestoreMode(s)
		require.NoError(t, err)
		require.Equal(t, s, string(m))
	}
	for _, s := range []string{"", "Replace", "restore", "merge "} {
		_, err := ParseRestoreMode(s)
		require.ErrorIs(t, err, ErrInvalidRestoreMode, "%q", s)
	}
}

func TestRestore_RoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.BeginRestore(ctx, digestA, RestoreReplace)
	require.ErrorIs(t, err, ErrNoPasskey, "no identity yet: nothing can prove the restore")

	key, err := passkeytest.New(true)
	require.NoError(t, err)
	cred := f.enrollWithCode(t, key, "key")
	for _, mode := range []RestoreMode{RestoreReplace, RestoreMerge} {
		f.now = f.now.Add(time.Minute)
		begun, err := f.svc.BeginRestore(ctx, digestA, mode)
		require.NoError(t, err, mode)
		require.NotEmpty(t, begun.ID)
		resp, err := key.Get(begun.Options, origin)
		require.NoError(t, err, mode)
		row, err := f.svc.FinishRestore(ctx, begun.ID, digestA, mode, resp)
		require.NoError(t, err, mode)
		require.Equal(t, cred.ID, row.ID, "the restore names the passkey that approved it")

		used, err := store.ApproverByID(ctx, f.db, cred.ID)
		require.NoError(t, err)
		require.NotNil(t, used.LastUsedAt, "the passkey's use is recorded")
		require.True(t, used.LastUsedAt.Equal(f.now), mode)
	}
}

// Nothing is asked of the owner for a restore no backup could be.
func TestRestore_Refusals(t *testing.T) {
	f, _, _ := restoreFixture(t)
	ctx := context.Background()

	_, err := f.svc.BeginRestore(ctx, digestA, RestoreMode("everything"))
	require.ErrorIs(t, err, ErrInvalidRestoreMode)
	_, err = f.svc.BeginRestore(ctx, digestA, "")
	require.ErrorIs(t, err, ErrInvalidRestoreMode)
	for name, d := range map[string]string{
		"empty":     "",
		"short":     digestA[:63],
		"long":      digestA + "0",
		"uppercase": strings.ToUpper(digestA),
		"not hex":   "g" + digestA[1:],
		"a name":    "seamless-host-20261111T030000Z-auto.tar.gz",
	} {
		_, err := f.svc.BeginRestore(ctx, d, RestoreReplace)
		require.ErrorIs(t, err, ErrInvalidRestoreDigest, name)
	}

	off := New(Config{DB: f.db, Now: func() time.Time { return f.now }})
	_, err = off.BeginRestore(ctx, digestA, RestoreReplace)
	require.ErrorIs(t, err, ErrPasskeysOff)
	_, err = off.FinishRestore(ctx, "any", digestA, RestoreReplace, []byte(`{}`))
	require.ErrorIs(t, err, ErrPasskeysOff)
}

// An assertion approves one ceremony: one backup, one mode, once. A finish
// that names anything else ends the ceremony without approving it.
func TestRestore_AssertionIsBound(t *testing.T) {
	f, key, _ := restoreFixture(t)
	ctx := context.Background()

	cases := map[string]func(id string, resp []byte) error{
		"another backup": func(id string, resp []byte) error {
			_, err := f.svc.FinishRestore(ctx, id, digestB, RestoreReplace, resp)
			return err
		},
		"the other mode": func(id string, resp []byte) error {
			_, err := f.svc.FinishRestore(ctx, id, digestA, RestoreMerge, resp)
			return err
		},
		"an unknown ceremony": func(_ string, resp []byte) error {
			_, err := f.svc.FinishRestore(ctx, "no-such-id", digestA, RestoreReplace, resp)
			return err
		},
	}
	for name, finish := range cases {
		begun, err := f.svc.BeginRestore(ctx, digestA, RestoreReplace)
		require.NoError(t, err, name)
		resp, err := key.Get(begun.Options, origin)
		require.NoError(t, err, name)
		require.ErrorIs(t, finish(begun.ID, resp), ErrNoRestore, name)
		if name != "an unknown ceremony" {
			_, err = f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreReplace, resp)
			require.ErrorIs(t, err, ErrNoRestore, "%s: a mismatched finish ends the ceremony", name)
		}
	}

	begun, err := f.svc.BeginRestore(ctx, digestA, RestoreReplace)
	require.NoError(t, err)
	_, err = f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreReplace, []byte(`{"id":"x"}`))
	require.Error(t, err, "garbage is not an assertion")
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreReplace, resp)
	require.ErrorIs(t, err, ErrNoRestore, "a failed finish ends the ceremony")

	begun, err = f.svc.BeginRestore(ctx, digestA, RestoreReplace)
	require.NoError(t, err)
	resp, err = key.Get(begun.Options, origin)
	require.NoError(t, err)
	f.now = f.now.Add(restoreTTL)
	_, err = f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreReplace, resp)
	require.ErrorIs(t, err, ErrNoRestore, "a ceremony expires")
}

// The challenge itself names the ceremony, the backup and the mode, so an
// assertion over one can never verify as another's.
func TestRestore_ChallengeNamesWhatItApproves(t *testing.T) {
	nonce := bytes.Repeat([]byte{7}, 32)
	base := restoreChallenge("id-1", digestA, RestoreReplace, nonce)
	require.Len(t, base, sha256.Size)
	for name, other := range map[string][]byte{
		"another ceremony": restoreChallenge("id-2", digestA, RestoreReplace, nonce),
		"another backup":   restoreChallenge("id-1", digestB, RestoreReplace, nonce),
		"the other mode":   restoreChallenge("id-1", digestA, RestoreMerge, nonce),
		"another nonce":    restoreChallenge("id-1", digestA, RestoreReplace, bytes.Repeat([]byte{8}, 32)),
		"a lock change":    lockChangeChallenge("id-1", digestA, LockAction(RestoreReplace), nonce),
	} {
		require.NotEqual(t, base, other, name)
	}
}

// A restore is approved once per ceremony: the same assertion sent again
// approves nothing.
func TestRestore_SingleUse(t *testing.T) {
	f, key, _ := restoreFixture(t)
	ctx := context.Background()
	begun, err := f.svc.BeginRestore(ctx, digestA, RestoreMerge)
	require.NoError(t, err)
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreMerge, resp)
	require.NoError(t, err)
	_, err = f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreMerge, resp)
	require.ErrorIs(t, err, ErrNoRestore)
}

// Waiting restore ceremonies are capped per backup and mode, and in all, and
// a begin never waits for room: past a backup and mode's cap it displaces
// that one's oldest, past maxPending the oldest of the fullest. A loop over
// other backups, or the other mode of the owner's, never displaces the
// owner's ceremony, which still finishes; the control is a loop on the
// owner's own backup and mode, which does.
func TestRestore_PendingCaps(t *testing.T) {
	f, key, _ := restoreFixture(t)
	ctx := context.Background()
	var loop []restoreScope
	for i := range maxPending / maxPendingPerBackup {
		loop = append(loop, restoreScope{fileDigest(fmt.Sprintf("loop backup %d", i)), RestoreReplace})
	}
	begin := func(sc restoreScope) Begun {
		t.Helper()
		f.now = f.now.Add(time.Second)
		b, err := f.svc.BeginRestore(ctx, sc.digest, sc.mode)
		require.NoError(t, err, "a begin never waits for room")
		return b
	}
	waiting := func(sc *restoreScope) int {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		n := 0
		for _, p := range f.svc.restores {
			if sc == nil || p.scope() == *sc {
				n++
			}
		}
		return n
	}
	held := func(id string) bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		_, ok := f.svc.restores[id]
		return ok
	}

	// One backup and mode: the fifth begin displaces the first.
	first := begin(loop[0])
	for range maxPendingPerBackup {
		begin(loop[0])
	}
	require.False(t, held(first.ID))
	require.Equal(t, maxPendingPerBackup, waiting(&loop[0]))

	// Every slot taken across backups: the owner's begin fits, and the
	// fullest backup and mode with the oldest ceremony gives one up.
	for _, sc := range loop[1:] {
		for range maxPendingPerBackup {
			begin(sc)
		}
	}
	require.Equal(t, maxPending, waiting(nil))
	mine := restoreScope{digestA, RestoreMerge}
	owners := begin(mine)
	require.Equal(t, maxPending, waiting(nil), "the overall cap holds")
	require.Equal(t, maxPendingPerBackup-1, waiting(&loop[0]))

	// The loop goes on over its own backups and the owner's backup in the
	// other mode; the owner's ceremony stays, and finishes.
	for range 3 {
		for _, sc := range loop {
			begin(sc)
		}
		begin(restoreScope{digestA, RestoreReplace})
	}
	require.True(t, held(owners.ID), "a loop over other backups and modes never displaces the owner's ceremony")
	require.Equal(t, maxPending, waiting(nil))
	resp, err := key.Get(owners.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishRestore(ctx, owners.ID, digestA, RestoreMerge, resp)
	require.NoError(t, err)

	// The control: a loop on the owner's own backup and mode displaces it.
	owners = begin(mine)
	for range maxPendingPerBackup {
		begin(mine)
	}
	require.False(t, held(owners.ID), "a loop on the same backup and mode crowds the owner's ceremony out")
	require.Equal(t, maxPendingPerBackup, waiting(&mine))
}

// A restricted local account approves no restore: it can neither begin a
// ceremony nor finish the owner's, and its attempt leaves the owner's
// ceremony usable and the passkey unused.
func TestRestore_RestrictedIsRefused(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	cred := f.enrollWithCode(t, key, "key")
	rctx := access.WithView(ctx, restrictedView(t, f, bridge, agent, shell))

	_, err = f.svc.BeginRestore(rctx, digestA, RestoreReplace)
	require.ErrorIs(t, err, ErrRestricted)

	begun, err := f.svc.BeginRestore(ctx, digestA, RestoreReplace)
	require.NoError(t, err)
	resp, err := key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishRestore(rctx, begun.ID, digestA, RestoreReplace, resp)
	require.ErrorIs(t, err, ErrRestricted)
	untouched, err := store.ApproverByID(ctx, f.db, cred.ID)
	require.NoError(t, err)
	require.Nil(t, untouched.LastUsedAt, "a refused finish records no use")

	row, err := f.svc.FinishRestore(ctx, begun.ID, digestA, RestoreReplace, resp)
	require.NoError(t, err, "the owner's ceremony survives a restricted attempt")
	require.Equal(t, cred.ID, row.ID)
}
