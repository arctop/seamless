package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// devices2PairKeyDevice pairs a key device named name through svc, and
// returns the device as the pairing left it.
func devices2PairKeyDevice(t *testing.T, svc *devices.Service, name string) store.Device {
	t.Helper()
	ctx := context.Background()
	code, err := svc.MintCode(ctx, store.DeviceByTerminal)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	answer, err := svc.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindKey, Name: name, Secret: secret})
	require.NoError(t, err)
	return answer.Device
}

// Under peer_auth, doctor names each key device a one-time code alone paired,
// while peer_auth was off (review I2): it is still the owner over TCP, though
// no passkey approved it. A key device a passkey approved, a revoked one, and
// a relay (refused under peer_auth until a passkey approves it) are not named.
// Nothing while peer_auth is off, pairing is off, or there is no database.
func TestDevices2_DoctorNamesCodeOnlyKeyDevices(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Paired while peer_auth was off: the code approved them.
	off := devices.New(devices.Config{DB: db, LocalHost: "example-server"})
	codeOnly := devices2PairKeyDevice(t, off, "example-ci")
	revoked := devices2PairKeyDevice(t, off, "example-old")
	_, err = off.Revoke(ctx, revoked.ID, store.DeviceByTerminal)
	require.NoError(t, err)
	relayID, err := devices.NewRelayIdentity("example-laptop")
	require.NoError(t, err)
	code, err := off.MintCode(ctx, store.DeviceByTerminal)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	_, err = off.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindRelay, Name: "example-laptop", Secret: secret,
		RelayKey: relayID.PublicKey()})
	require.NoError(t, err)

	// Paired under peer_auth, and approved with a passkey.
	on := devices.New(devices.Config{DB: db, LocalHost: "example-server", PeerAuth: true})
	approved := devices2PairKeyDevice(t, on, "example-desk")
	require.Equal(t, store.DevicePending, approved.Status)
	_, err = store.ApproveDevice(ctx, db, approved.ID, approved.Fingerprint, "01PASSKEYEXAMPLE", time.Now())
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	require.Empty(t, codeOnlyDeviceChecks(ctx, cfg, db), "peer_auth off: the code is the approval, and nothing is amiss")

	cfg.PeerAuth.Enabled = true
	rows := codeOnlyDeviceChecks(ctx, cfg, db)
	require.Len(t, rows, 1, "%+v", rows)
	require.Equal(t, statusWarn, rows[0].status)
	require.Equal(t, "peer_auth devices", rows[0].name)
	for _, want := range []string{"key device example-ci", codeOnly.ID, "a one-time code alone",
		"revoke and pair again, so a passkey approves it", "seamlessd devices revoke " + codeOnly.ID} {
		require.Contains(t, rows[0].detail, want)
	}

	require.Empty(t, codeOnlyDeviceChecks(ctx, cfg, nil), "no database to read")
	loopback := config.Defaults()
	loopback.PeerAuth.Enabled = true
	require.Empty(t, codeOnlyDeviceChecks(ctx, loopback, db), "an install that pairs nothing has no such row")
}
