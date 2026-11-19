package approval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
)

// The approval in place (owner decision R-D6): a relay its one-time code
// alone paired, once peer_auth.relays maps it, waits for the owner's passkey
// on the Devices page, and the approval keeps its pairing.

// inPlaceFixture is deviceFixture's daemon with relay pairing that maps the
// relays named in mapped.
func inPlaceFixture(t *testing.T, mapped ...string) *deviceFixture {
	t.Helper()
	f := deviceNewFixture(t)
	f.pairing = devices.New(devices.Config{DB: f.db, Events: f.rec, PeerAuth: true, Approver: f.approvals,
		LocalHost: "example-server", Now: f.svc.cfg.Now, RelayMapped: func(name string) bool {
			for _, m := range mapped {
				if m == name {
					return true
				}
			}
			return false
		}})
	return f
}

// codeOnlyRelay records a relay named name paired the way a server without
// peer_auth pairs one: active at once, its code the only approval.
func (d *deviceFixture) codeOnlyRelay(t *testing.T, name string) store.Device {
	t.Helper()
	ctx := context.Background()
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	code, _, err := store.MintPairingCode(ctx, d.db, store.DeviceByConsole, d.now)
	require.NoError(t, err)
	secret := sha256.Sum256([]byte("secret:" + name))
	spki := id.PublicKey()
	dev, err := store.PairDevice(ctx, d.db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: devices.PublicKeyFingerprint(spki), PublicKey: spki, KeyHash: devices.PublicKeyHash(spki),
		SecretHash: hex.EncodeToString(secret[:]), OwnerUID: 501}, d.now)
	require.NoError(t, err)
	require.True(t, dev.CodeOnlyRelay())
	return dev
}

func TestDeviceApproval_InPlaceKeepsThePairing(t *testing.T) {
	f := inPlaceFixture(t, "example-laptop")
	ctx := context.Background()
	relay := f.codeOnlyRelay(t, "example-laptop")
	require.True(t, f.pairing.InPlace(relay))

	_, err := f.pairing.BeginApproval(ctx, relay.ID)
	require.ErrorIs(t, err, ErrNoPasskey, "nothing can prove the approval yet")
	f.cred = f.enrollWithCode(t, f.key, "key")
	begun, err := f.pairing.BeginApproval(ctx, relay.ID)
	require.NoError(t, err)
	resp, err := f.key.Get(begun.Options, origin)
	require.NoError(t, err)

	approved, err := f.pairing.FinishApproval(ctx, relay.ID, begun.Ticket, resp)
	require.NoError(t, err)
	require.Equal(t, relay.ID, approved.ID, "the same device")
	require.Equal(t, relay.Fingerprint, approved.Fingerprint, "the same certificate")
	require.Equal(t, relay.PublicKey, approved.PublicKey)
	require.Equal(t, store.DeviceActive, approved.Status)
	require.Equal(t, f.cred.ID, approved.ApprovedBy, "now a passkey-approved pairing")
	require.False(t, f.pairing.InPlace(approved), "no longer offered")

	evs, err := f.rec.ByKinds(ctx, []core.EventKind{devices.EventPaired}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, relay.ID, evs[0].ItemID)
	require.Equal(t, devices.ProofPasskey, evs[0].Payload["proof"])
	require.Equal(t, f.cred.ID, evs[0].Payload["credential"])
	require.Equal(t, true, evs[0].Payload["in_place"])

	_, err = f.pairing.FinishApproval(ctx, relay.ID, begun.Ticket, resp)
	require.ErrorIs(t, err, ErrDeviceNotWaiting, "an approval works once")
	_, err = f.pairing.BeginApproval(ctx, relay.ID)
	require.ErrorIs(t, err, ErrDeviceNotWaiting)
}

// Only a code-only relay peer_auth.relays maps is offered, under peer_auth:
// not one it does not map, not a pending pairing (which keeps its own
// ceremony), not a revoked relay, and nothing without peer_auth.
func TestDeviceApproval_InPlaceOnlyForAMappedCodeOnlyRelay(t *testing.T) {
	f := inPlaceFixture(t, "example-laptop", "example-old")
	ctx := context.Background()
	f.cred = f.enrollWithCode(t, f.key, "key")
	unmapped := f.codeOnlyRelay(t, "example-studio")
	require.False(t, f.pairing.InPlace(unmapped))
	_, err := f.pairing.BeginApproval(ctx, unmapped.ID)
	require.ErrorIs(t, err, ErrDeviceNotWaiting, "an unmapped relay is not offered")

	pending := f.devicePending(t, "example-desk")
	require.False(t, f.pairing.InPlace(pending))
	_, err = f.approvals.BeginInPlaceApproval(ctx, pending.ID)
	require.ErrorIs(t, err, ErrDeviceNotWaiting, "a pending pairing is no relay to approve in place")

	revoked := f.codeOnlyRelay(t, "example-old")
	_, err = f.pairing.Revoke(ctx, revoked.ID, store.DeviceByConsole)
	require.NoError(t, err)
	gone, err := store.DeviceByID(ctx, f.db, revoked.ID)
	require.NoError(t, err)
	require.False(t, f.pairing.InPlace(gone), "a revoked relay pairs again")
	_, err = f.pairing.BeginApproval(ctx, revoked.ID)
	require.ErrorIs(t, err, ErrDeviceNotWaiting)

	off := devices.New(devices.Config{DB: f.db, Approver: f.approvals, RelayMapped: func(string) bool { return true }})
	relay := f.codeOnlyRelay(t, "example-laptop")
	require.False(t, off.InPlace(relay), "without peer_auth nothing waits")
	_, err = off.BeginApproval(ctx, relay.ID)
	require.ErrorIs(t, err, devices.ErrNoApprover)
}

// A relay revoked while the owner's passkey answers is not approved, and the
// two ceremonies' challenges never answer each other.
func TestDeviceApproval_InPlaceRevokedMeanwhileAndItsOwnChallenge(t *testing.T) {
	f := inPlaceFixture(t, "example-laptop")
	ctx := context.Background()
	f.cred = f.enrollWithCode(t, f.key, "key")
	relay := f.codeOnlyRelay(t, "example-laptop")
	begun, err := f.pairing.BeginApproval(ctx, relay.ID)
	require.NoError(t, err)
	resp, err := f.key.Get(begun.Options, origin)
	require.NoError(t, err)
	_, err = f.pairing.Revoke(ctx, relay.ID, store.DeviceByConsole)
	require.NoError(t, err)
	_, err = f.pairing.FinishApproval(ctx, relay.ID, begun.Ticket, resp)
	require.ErrorIs(t, err, ErrDeviceNotWaiting)
	_, err = f.approvals.FinishInPlaceApproval(ctx, relay.ID, begun.Ticket, resp)
	require.ErrorIs(t, err, ErrDeviceNotWaiting, "the approval core refuses it too")
	d, err := store.DeviceByID(ctx, f.db, relay.ID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceRevoked, d.Status)
	require.Empty(t, d.ApprovedBy)

	nonce := []byte("nonce")
	require.False(t, bytes.Equal(f.approvals.deviceChallenge(1, nonce, relay), f.approvals.inPlaceChallenge(1, nonce, relay)),
		"a pairing's approval and an approval in place sign different challenges")
}

// A restricted account approves nothing in place either.
func TestDeviceApproval_InPlaceRefusesRestricted(t *testing.T) {
	f := newGrantFixture(t)
	approvals, err := NewDeviceApprovals(f.svc)
	require.NoError(t, err)
	rctx := access.WithView(context.Background(), restrictedView(t, f, bridge, agent, shell))
	_, err = approvals.BeginInPlaceApproval(rctx, "01ANYDEVICE")
	require.ErrorIs(t, err, ErrRestricted)
	_, err = approvals.FinishInPlaceApproval(rctx, "01ANYDEVICE", "v1.1.x", []byte("{}"))
	require.ErrorIs(t, err, ErrRestricted)
}
