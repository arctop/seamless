package store

// Migration 050 (owner decision R-D1): a relay's pairing records the owner's
// uid on its computer.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A database from before 050 migrates with every relay unbound, the previous
// release's inserts keep working after it (taking the default), and a relay
// pairing records the uid it names; a key device names none.
func TestPRMigration050_OwnerUID(t *testing.T) {
	ctx := context.Background()
	db := openPartialDB(t, 50)
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 49, v, "the fixture is the previous release's schema")
	at := core.FormatTime(deviceT0)
	insert := func(id, name string) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO devices (id, kind, name, status, fingerprint, public_key, key_hash, key_prefix,
			secret_hash, code_id, created_at, pending_until, enrolled_at) VALUES (?, 'relay', ?, 'active', ?, x'00', ?, '', ?, '01CODE', ?, ?, ?)`,
			id, name, ApproverKeyFingerprint([]byte(name)), deviceHash("pub:"+name), deviceHash("secret:"+name), at, at, at)
		require.NoError(t, err)
	}
	insert("01OLDRELAY", "example-old")

	require.NoError(t, migrate(db, migrationList()))
	v, err = SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), v)
	old, err := DeviceByID(ctx, db, "01OLDRELAY")
	require.NoError(t, err)
	require.Zero(t, old.OwnerUID, "a pairing from before 050 recorded no uid")
	require.True(t, old.OwnerUnbound())

	// The previous release, rolled back to, still inserts by name.
	insert("01ROLLEDBACK", "example-rolled")
	back, err := DeviceByID(ctx, db, "01ROLLEDBACK")
	require.NoError(t, err)
	require.True(t, back.OwnerUnbound())

	// The column holds a uid, and nothing else.
	for _, bad := range []int64{-1, 1 << 32} {
		_, err := db.ExecContext(ctx, `UPDATE devices SET owner_uid = ? WHERE id = '01OLDRELAY'`, bad)
		require.Error(t, err, "owner_uid %d", bad)
	}
	_, err = db.ExecContext(ctx, `UPDATE devices SET owner_uid = 4294967295 WHERE id = '01OLDRELAY'`)
	require.NoError(t, err, "the largest uid")
}

// PairDevice records a relay's owner uid, and refuses one on a key device.
func TestPRPairDevice_RecordsTheOwnerUID(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	nd := deviceRelay("example-laptop", "relay-key-uid", false)
	nd.OwnerUID = 501
	d, err := PairDevice(ctx, db, code, nd, deviceT0)
	require.NoError(t, err)
	require.Equal(t, uint32(501), d.OwnerUID)
	require.False(t, d.OwnerUnbound())
	byKey, err := DeviceByKey(ctx, db, DeviceRelayKind, nd.KeyHash)
	require.NoError(t, err)
	require.Equal(t, uint32(501), byKey.OwnerUID, "every reader scans it")
	list, err := ListDevices(ctx, db)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, uint32(501), list[0].OwnerUID)

	code, _, err = MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	key := deviceKeyed("example-ci", "abcdkey", false)
	key.OwnerUID = 501
	_, err = PairDevice(ctx, db, code, key, deviceT0)
	require.ErrorIs(t, err, ErrInvalidDevice)
	require.ErrorContains(t, err, "a key device names no owner's uid")
	key.OwnerUID = 0
	k, err := PairDevice(ctx, db, code, key, deviceT0)
	require.NoError(t, err, "the code was not used by the refused pairing")
	require.False(t, k.OwnerUnbound(), "a key device is never an unbound relay")
}

// A restore keeps today's devices with the uid each recorded.
func TestPRRestoreKeepsTheOwnerUID(t *testing.T) {
	ctx := context.Background()
	today := deviceDB(t)
	code, _, err := MintPairingCode(ctx, today, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	nd := deviceRelay("example-laptop", "relay-key-kept", false)
	nd.OwnerUID = 501
	d, err := PairDevice(ctx, today, code, nd, deviceT0)
	require.NoError(t, err)
	kept, err := ReadKeptState(ctx, today)
	require.NoError(t, err)
	restored := deviceDB(t)
	_, err = ApplyKeptState(ctx, restored, kept, ApplyKeptOptions{Now: deviceT0})
	require.NoError(t, err)
	got, err := DeviceByID(ctx, restored, d.ID)
	require.NoError(t, err)
	require.Equal(t, uint32(501), got.OwnerUID)
}
