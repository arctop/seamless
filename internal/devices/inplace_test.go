package devices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// ipApprover records which ceremony the service asked of it.
type ipApprover struct{ calls []string }

func (a *ipApprover) BeginDeviceApproval(context.Context, string) (Ceremony, error) {
	a.calls = append(a.calls, "begin pairing")
	return Ceremony{}, nil
}

func (a *ipApprover) FinishDeviceApproval(context.Context, string, string, []byte) (store.Device, error) {
	a.calls = append(a.calls, "finish pairing")
	return store.Device{}, nil
}

func (a *ipApprover) BeginInPlaceApproval(context.Context, string) (Ceremony, error) {
	a.calls = append(a.calls, "begin in place")
	return Ceremony{}, nil
}

func (a *ipApprover) FinishInPlaceApproval(context.Context, string, string, []byte) (store.Device, error) {
	a.calls = append(a.calls, "finish in place")
	return store.Device{}, nil
}

func (a *ipApprover) PasskeysOn() bool  { return true }
func (a *ipApprover) Origins() []string { return nil }

// ipRelay records an active relay named name its one-time code alone paired.
func ipRelay(t *testing.T, ctx context.Context, svc *Service, name string) store.Device {
	t.Helper()
	id := pairIdentity(t, name)
	code, _, err := store.MintPairingCode(ctx, svc.cfg.DB, store.DeviceByConsole, time.Now())
	require.NoError(t, err)
	secret := sha256.Sum256([]byte("secret:" + name))
	spki := id.PublicKey()
	d, err := store.PairDevice(ctx, svc.cfg.DB, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: PublicKeyFingerprint(spki), PublicKey: spki, KeyHash: PublicKeyHash(spki),
		SecretHash: hex.EncodeToString(secret[:]), OwnerUID: 501}, time.Now())
	require.NoError(t, err)
	return d
}

// The approval in place (owner decision R-D6) is offered only under
// peer_auth, with a passkey approval, for an active relay its code alone
// paired that peer_auth.relays maps; the service asks the in-place ceremony
// for exactly those, and the pairing approval for every other device.
func TestService_InPlaceOffersAMappedCodeOnlyRelay(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	a := &ipApprover{}
	mapped := func(name string) bool { return name == "example-laptop" || name == "example-old" }
	svc := New(Config{DB: db, PeerAuth: true, Approver: a, RelayMapped: mapped, LocalHost: "example-server"})

	laptop := ipRelay(t, ctx, svc, "example-laptop")
	studio := ipRelay(t, ctx, svc, "example-studio")
	old := ipRelay(t, ctx, svc, "example-old")
	_, err = svc.Revoke(ctx, old.ID, store.DeviceByConsole)
	require.NoError(t, err)
	old, err = store.DeviceByID(ctx, db, old.ID)
	require.NoError(t, err)
	approved := laptop
	approved.ApprovedBy = "01PASSKEY"

	require.True(t, svc.InPlace(laptop), "mapped, active, its code alone")
	require.False(t, svc.InPlace(studio), "not mapped")
	require.False(t, svc.InPlace(old), "revoked: it pairs again")
	require.False(t, svc.InPlace(approved), "a passkey approved it")
	key := laptop
	key.Kind = store.DeviceKeyKind
	require.False(t, svc.InPlace(key), "a key device keeps today's way")
	for name, off := range map[string]*Service{
		"peer_auth off":  New(Config{DB: db, Approver: a, RelayMapped: mapped}),
		"no approver":    New(Config{DB: db, PeerAuth: true, RelayMapped: mapped}),
		"nothing mapped": New(Config{DB: db, PeerAuth: true, Approver: a}),
	} {
		require.False(t, off.InPlace(laptop), name)
	}

	_, err = svc.BeginApproval(ctx, laptop.ID)
	require.NoError(t, err)
	_, err = svc.FinishApproval(ctx, laptop.ID, "ticket", nil)
	require.NoError(t, err)
	for _, id := range []string{studio.ID, old.ID, "01NOSUCHDEVICE"} {
		_, err = svc.BeginApproval(ctx, id)
		require.NoError(t, err)
		_, err = svc.FinishApproval(ctx, id, "ticket", nil)
		require.NoError(t, err)
	}
	require.Equal(t, []string{"begin in place", "finish in place",
		"begin pairing", "finish pairing", "begin pairing", "finish pairing", "begin pairing", "finish pairing"}, a.calls)
}
