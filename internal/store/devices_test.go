package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// deviceDB is a fresh database for the device tests.
func deviceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// deviceHash is a stand-in SHA-256 hex for a key or a secret.
func deviceHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// deviceRelay is a relay pairing of name with a stand-in key.
func deviceRelay(name, key string, pending bool) NewDevice {
	return NewDevice{Kind: DeviceRelayKind, Name: name, Fingerprint: ApproverKeyFingerprint([]byte(key)),
		PublicKey: []byte(key), KeyHash: deviceHash("pub:" + key), SecretHash: deviceHash("secret:" + name + key), Pending: pending}
}

// deviceKeyed is a key device pairing of name, paired at once unless pending.
func deviceKeyed(name, key string, pending bool) NewDevice {
	d := NewDevice{Kind: DeviceKeyKind, Name: name, Fingerprint: ApproverKeyFingerprint([]byte("fp:" + name + key)),
		SecretHash: deviceHash("secret:" + name + key), Pending: pending}
	if !pending {
		d.KeyHash, d.KeyPrefix = deviceHash("key:"+key), "seamless_dk_"+key[:4]
	}
	return d
}

var deviceT0 = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

func TestPairingCode_MintFormatAndCap(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, expires, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	require.Equal(t, code, FormatPairingCode(code), "printed in its canonical form")
	require.Len(t, NormalizeEnrollCode(code), 16, "80 bits: without peer_auth a code is the approval")
	require.Equal(t, deviceT0.Add(PairingCodeTTL), expires)
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT code_hash FROM device_pairing_codes`).Scan(&stored))
	require.NotContains(t, stored, NormalizeEnrollCode(code), "only the hash is stored")

	_, _, err = MintPairingCode(ctx, db, "browser", deviceT0)
	require.ErrorIs(t, err, ErrInvalidDevice, "minted_by is console or terminal")
	_, _, err = MintPairingCode(ctx, db, DeviceByRestore, deviceT0)
	require.ErrorIs(t, err, ErrInvalidDevice)

	for range maxOpenPairingCodes - 1 {
		_, _, err := MintPairingCode(ctx, db, DeviceByTerminal, deviceT0)
		require.NoError(t, err)
	}
	_, _, err = MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.ErrorIs(t, err, ErrTooManyPairingCodes, "a caller looping on the mint cannot grow the table")
	// Expired codes free the cap.
	_, _, err = MintPairingCode(ctx, db, DeviceByConsole, deviceT0.Add(PairingCodeTTL))
	require.NoError(t, err)
}

// A code pairs one device, once, within its lifetime; a malformed, expired or
// used code pairs nothing.
func TestPairDevice_CodeIsSingleUseAndShortLived(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)

	d, err := PairDevice(ctx, db, code, deviceRelay("example-laptop", "relay-key-1", false), deviceT0.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, DeviceActive, d.Status, "without peer_auth the code is the approval")
	require.NotNil(t, d.EnrolledAt)
	require.Empty(t, d.ApprovedBy, "a code-only pairing names no approving credential")
	require.True(t, d.HasKey)

	_, err = PairDevice(ctx, db, code, deviceRelay("example-ci", "relay-key-2", false), deviceT0.Add(2*time.Minute))
	require.ErrorIs(t, err, ErrPairingCode, "a used code fails")

	late, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	_, err = PairDevice(ctx, db, late, deviceRelay("example-ci", "relay-key-2", false), deviceT0.Add(PairingCodeTTL))
	require.ErrorIs(t, err, ErrPairingCode, "an expired code fails")

	for _, bad := range []string{"", "AAAA-BBBB", "not a code at all!", "AAAA-BBBB-CCCC-DDDD"} {
		_, err := PairDevice(ctx, db, bad, deviceRelay("example-ci", "relay-key-2", false), deviceT0)
		require.ErrorIs(t, err, ErrPairingCode, bad)
	}
	list, err := ListDevices(ctx, db)
	require.NoError(t, err)
	require.Len(t, list, 1, "no refused code recorded a device")
}

// Of two pairings racing for one code, exactly one succeeds.
func TestPairDevice_RacingForOneCode(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = PairDevice(ctx, db, code, deviceKeyed("example-ci-"+string(rune('a'+i)), "key-"+string(rune('a'+i))+"xxxx", false), deviceT0)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
			continue
		}
		require.ErrorIs(t, err, ErrPairingCode)
	}
	require.Equal(t, 1, won)
}

// A name or a key a live device holds is refused, and the code stays usable.
func TestPairDevice_NameAndKeyAreTakenWhileLive(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	mint := func() string {
		code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
		require.NoError(t, err)
		return code
	}
	first, err := PairDevice(ctx, db, mint(), deviceRelay("example-laptop", "relay-key-1", false), deviceT0)
	require.NoError(t, err)

	code := mint()
	_, err = PairDevice(ctx, db, code, deviceKeyed("example-laptop", "key-zzzz", false), deviceT0)
	require.ErrorIs(t, err, ErrDeviceNameTaken, "one device's name is not another's to take")
	again := deviceRelay("example-ci", "relay-key-1", false)
	_, err = PairDevice(ctx, db, code, again, deviceT0)
	require.ErrorIs(t, err, ErrDeviceKeyTaken)
	// The code was not used by either refusal.
	ok, err := PairDevice(ctx, db, code, deviceRelay("example-ci", "relay-key-9", false), deviceT0)
	require.NoError(t, err)
	require.Equal(t, DeviceActive, ok.Status)

	// The server's own name is refused, and only to a caller with a code.
	reserved := deviceKeyed("example-server", "key-rrrr", false)
	reserved.Reserved = "example-server"
	_, err = PairDevice(ctx, db, "AAAA-BBBB-CCCC-DDDD", reserved, deviceT0)
	require.ErrorIs(t, err, ErrPairingCode, "without a code the name is never judged")
	_, err = PairDevice(ctx, db, mint(), reserved, deviceT0)
	require.ErrorIs(t, err, ErrDeviceNameReserved)

	// Revoked, the name and the key are free again; a pairing's secret is
	// never reused.
	_, err = RevokeDevice(ctx, db, first.ID, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	repair := deviceRelay("example-laptop", "relay-key-1", false)
	code = mint()
	_, err = PairDevice(ctx, db, code, repair, deviceT0)
	require.ErrorIs(t, err, ErrDeviceSecretUsed, "the first pairing's secret answers for it alone")
	repair.SecretHash = deviceHash("a fresh secret")
	_, err = PairDevice(ctx, db, code, repair, deviceT0)
	require.NoError(t, err)
}

// A pending pairing waits for the owner, then expires; an approval is a
// compare-and-set on the row still pending and still showing the fingerprint
// it was bound to.
func TestApproveDevice_PendingOnlyAndBoundToItsFingerprint(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	d, err := PairDevice(ctx, db, code, deviceRelay("example-laptop", "relay-key-1", true), deviceT0)
	require.NoError(t, err)
	require.Equal(t, DevicePending, d.Status)
	require.Nil(t, d.EnrolledAt)
	require.Equal(t, deviceT0.Add(DevicePendingTTL), d.PendingUntil)

	_, err = ApproveDevice(ctx, db, d.ID, ApproverKeyFingerprint([]byte("another key")), "01PASSKEY", deviceT0)
	require.ErrorIs(t, err, ErrDeviceNotPending, "an approval bound to another key's fingerprint takes nothing")
	_, err = ApproveDevice(ctx, db, d.ID, d.Fingerprint, "", deviceT0)
	require.ErrorIs(t, err, ErrInvalidDevice, "an approval names its credential")
	_, err = ApproveDevice(ctx, db, "01NOSUCHDEVICE", d.Fingerprint, "01PASSKEY", deviceT0)
	require.ErrorIs(t, err, ErrDeviceNotFound)

	got, err := ApproveDevice(ctx, db, d.ID, d.Fingerprint, "01PASSKEY", deviceT0.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, DeviceActive, got.Status)
	require.Equal(t, "01PASSKEY", got.ApprovedBy)
	_, err = ApproveDevice(ctx, db, d.ID, d.Fingerprint, "01PASSKEY", deviceT0.Add(time.Minute))
	require.ErrorIs(t, err, ErrDeviceNotPending, "an approval works once")

	code, _, err = MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	late, err := PairDevice(ctx, db, code, deviceRelay("example-ci", "relay-key-2", true), deviceT0)
	require.NoError(t, err)
	_, err = ApproveDevice(ctx, db, late.ID, late.Fingerprint, "01PASSKEY", deviceT0.Add(DevicePendingTTL))
	require.ErrorIs(t, err, ErrDeviceNotPending, "a pairing past its deadline is not approved")
	require.Equal(t, DeviceExpired, late.StatusAt(deviceT0.Add(DevicePendingTTL)))
}

// A key device that waited collects its key once, by its secret, and only
// once it is active.
func TestCollectDeviceKey_OnceAndOnlyWhenActive(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	nd := deviceKeyed("example-ci", "key-aaaa", true)
	d, err := PairDevice(ctx, db, code, nd, deviceT0)
	require.NoError(t, err)
	require.False(t, d.HasKey)

	_, err = CollectDeviceKey(ctx, db, nd.SecretHash, deviceHash("k1"), "seamless_dk_k1")
	require.ErrorIs(t, err, ErrDeviceKeyCollected, "a pending device has no key to collect")
	_, err = ApproveDevice(ctx, db, d.ID, d.Fingerprint, "01PASSKEY", deviceT0)
	require.NoError(t, err)
	_, err = CollectDeviceKey(ctx, db, deviceHash("another secret"), deviceHash("k1"), "seamless_dk_k1")
	require.ErrorIs(t, err, ErrDeviceKeyCollected, "another device's secret collects nothing")
	got, err := CollectDeviceKey(ctx, db, nd.SecretHash, deviceHash("k1"), "seamless_dk_k1")
	require.NoError(t, err)
	require.True(t, got.HasKey)
	_, err = CollectDeviceKey(ctx, db, nd.SecretHash, deviceHash("k2"), "seamless_dk_k2")
	require.ErrorIs(t, err, ErrDeviceKeyCollected, "a key is collected once")

	found, err := DeviceByKey(ctx, db, DeviceKeyKind, deviceHash("k1"))
	require.NoError(t, err)
	require.Equal(t, d.ID, found.ID)
	_, err = DeviceByKey(ctx, db, DeviceKeyKind, deviceHash("k2"))
	require.ErrorIs(t, err, ErrDeviceNotFound)
	_, err = DeviceByKey(ctx, db, DeviceRelayKind, deviceHash("k1"))
	require.ErrorIs(t, err, ErrDeviceNotFound, "a key device's hash is not a relay's")
}

// A revoked device stays on record as revoked, and a pending one is denied;
// DeviceByKey still names it, so a caller can tell revoked from unknown.
func TestRevokeDevice_EndsOnceAndStaysOnRecord(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	nd := deviceRelay("example-laptop", "relay-key-1", false)
	d, err := PairDevice(ctx, db, code, nd, deviceT0)
	require.NoError(t, err)
	_, err = RevokeDevice(ctx, db, d.ID, "someone", deviceT0)
	require.ErrorIs(t, err, ErrInvalidDevice)
	got, err := RevokeDevice(ctx, db, d.ID, DeviceByConsole, deviceT0.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, DeviceRevoked, got.Status)
	require.Equal(t, DeviceByConsole, got.RevokedBy)
	require.NotNil(t, got.RevokedAt)
	_, err = RevokeDevice(ctx, db, d.ID, DeviceByConsole, deviceT0.Add(time.Hour))
	require.ErrorIs(t, err, ErrDeviceEnded)
	_, err = RevokeDevice(ctx, db, "01NOSUCH", DeviceByConsole, deviceT0)
	require.ErrorIs(t, err, ErrDeviceNotFound)
	byKey, err := DeviceByKey(ctx, db, DeviceRelayKind, nd.KeyHash)
	require.NoError(t, err)
	require.Equal(t, DeviceRevoked, byKey.Status, "the revoked row still answers for its key")

	code, _, err = MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	pending, err := PairDevice(ctx, db, code, deviceRelay("example-ci", "relay-key-2", true), deviceT0)
	require.NoError(t, err)
	denied, err := RevokeDevice(ctx, db, pending.ID, DeviceByTerminal, deviceT0)
	require.NoError(t, err)
	require.Equal(t, DeviceDenied, denied.Status)
	_, err = ApproveDevice(ctx, db, pending.ID, pending.Fingerprint, "01PASSKEY", deviceT0)
	require.ErrorIs(t, err, ErrDeviceNotPending, "a denied pairing is not approved")
}

// RevokeAllDevices ends every live device and voids the unused codes.
func TestRevokeAllDevices_EndsEveryLiveDeviceAndCode(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	for i, nd := range []NewDevice{deviceRelay("example-laptop", "relay-key-1", false), deviceRelay("example-ci", "relay-key-2", true)} {
		code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
		require.NoError(t, err, i)
		_, err = PairDevice(ctx, db, code, nd, deviceT0)
		require.NoError(t, err)
	}
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	n, err := RevokeAllDevices(ctx, db, DeviceByRestore, deviceT0.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	list, err := ListDevices(ctx, db)
	require.NoError(t, err)
	for _, d := range list {
		require.False(t, d.Live(deviceT0.Add(time.Minute)), d.Name)
		require.Equal(t, DeviceByRestore, d.RevokedBy)
	}
	_, err = PairDevice(ctx, db, code, deviceRelay("example-new", "relay-key-3", false), deviceT0.Add(time.Minute))
	require.ErrorIs(t, err, ErrPairingCode, "a code minted before the restore pairs nothing after it")
}

func TestTouchDevice_AtMostOnceAMinute(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	d, err := PairDevice(ctx, db, code, deviceRelay("example-laptop", "relay-key-1", false), deviceT0)
	require.NoError(t, err)
	require.NoError(t, TouchDevice(ctx, db, d.ID, deviceT0.Add(time.Second)))
	require.NoError(t, TouchDevice(ctx, db, d.ID, deviceT0.Add(30*time.Second)))
	got, err := DeviceByID(ctx, db, d.ID)
	require.NoError(t, err)
	require.Equal(t, deviceT0.Add(time.Second), *got.LastUsedAt, "a use within the minute writes nothing")
	require.NoError(t, TouchDevice(ctx, db, d.ID, deviceT0.Add(2*time.Minute)))
	got, err = DeviceByID(ctx, db, d.ID)
	require.NoError(t, err)
	require.Equal(t, deviceT0.Add(2*time.Minute), *got.LastUsedAt)

	c, err := CountDevices(ctx, db, deviceT0)
	require.NoError(t, err)
	require.Equal(t, DeviceCounts{Relays: 1}, c)
}

func TestValidDeviceName(t *testing.T) {
	for _, ok := range []string{"example-laptop", "example-laptop.local", "ci_runner-7", "a", "x1.example.ts.net"} {
		require.NoError(t, ValidDeviceName(ok), ok)
	}
	for _, bad := range []string{"", "Example-Laptop", "-lead", "trail-", "a..b", ".lead", "trail.", "has space", "semi;colon",
		"new\nline", string(make([]byte, MaxDeviceNameLen+1))} {
		require.ErrorIs(t, ValidDeviceName(bad), ErrInvalidDevice, "%q", bad)
	}
}

// The store refuses a pairing record of the wrong shape for its kind.
func TestPairDevice_RefusesMalformedRecords(t *testing.T) {
	ctx := context.Background()
	db := deviceDB(t)
	code, _, err := MintPairingCode(ctx, db, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	for name, nd := range map[string]NewDevice{
		"no kind":           {Name: "example-ci", Fingerprint: ApproverKeyFingerprint([]byte("x")), SecretHash: deviceHash("s")},
		"relay without key": {Kind: DeviceRelayKind, Name: "example-ci", Fingerprint: ApproverKeyFingerprint([]byte("x")), SecretHash: deviceHash("s")},
		"key with a public key": {Kind: DeviceKeyKind, Name: "example-ci", Fingerprint: ApproverKeyFingerprint([]byte("x")),
			SecretHash: deviceHash("s"), PublicKey: []byte("pub"), KeyHash: deviceHash("k"), KeyPrefix: "p"},
		"pending key with its hash": {Kind: DeviceKeyKind, Name: "example-ci", Fingerprint: ApproverKeyFingerprint([]byte("x")),
			SecretHash: deviceHash("s"), KeyHash: deviceHash("k"), Pending: true},
		"bad fingerprint": {Kind: DeviceKeyKind, Name: "example-ci", Fingerprint: "nope", SecretHash: deviceHash("s"),
			KeyHash: deviceHash("k"), KeyPrefix: "p"},
		"no secret": {Kind: DeviceKeyKind, Name: "example-ci", Fingerprint: ApproverKeyFingerprint([]byte("x")),
			KeyHash: deviceHash("k"), KeyPrefix: "p"},
	} {
		_, err := PairDevice(ctx, db, code, nd, deviceT0)
		require.ErrorIs(t, err, ErrInvalidDevice, name)
	}
	_, err = PairDevice(ctx, db, code, deviceRelay("example-ci", "relay-key-1", false), deviceT0)
	require.NoError(t, err, "no refusal used the code")
}

// A restore keeps today's devices and codes, not the backup's (keptTables):
// a device revoked since the backup stays revoked, one paired since stays
// paired, and a code used since stays used.
func TestApplyKeptState_KeepsTodaysDevices(t *testing.T) {
	ctx := context.Background()
	backup := deviceDB(t)
	code, _, err := MintPairingCode(ctx, backup, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	lost, err := PairDevice(ctx, backup, code, deviceRelay("example-lost", "relay-key-lost", false), deviceT0)
	require.NoError(t, err)
	staleCode, _, err := MintPairingCode(ctx, backup, DeviceByConsole, deviceT0)
	require.NoError(t, err)

	today := deviceDB(t)
	code, _, err = MintPairingCode(ctx, today, DeviceByConsole, deviceT0)
	require.NoError(t, err)
	kept, err := PairDevice(ctx, today, code, deviceRelay("example-laptop", "relay-key-1", false), deviceT0)
	require.NoError(t, err)
	state, err := ReadKeptState(ctx, today)
	require.NoError(t, err)
	_, err = ApplyKeptState(ctx, backup, state, ApplyKeptOptions{Now: deviceT0})
	require.NoError(t, err)

	list, err := ListDevices(ctx, backup)
	require.NoError(t, err)
	require.Len(t, list, 1, "the restored database holds today's devices")
	require.Equal(t, kept.ID, list[0].ID)
	require.Equal(t, DeviceActive, list[0].Status, "a device paired since the backup stays paired")
	_, err = DeviceByID(ctx, backup, lost.ID)
	require.ErrorIs(t, err, ErrDeviceNotFound, "a device today does not hold does not come back")
	_, err = PairDevice(ctx, backup, staleCode, deviceRelay("example-ci", "relay-key-2", false), deviceT0)
	require.ErrorIs(t, err, ErrPairingCode, "a code today does not hold pairs nothing")
}
