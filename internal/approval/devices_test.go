package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// deviceFixture is the approval service with a passkey enrolled, the device
// approvals over it, and pairing under peer_auth on the same database.
type deviceFixture struct {
	*fixture
	key       *passkeytest.Authenticator
	cred      store.ApproverCredential
	approvals *DeviceApprovals
	pairing   *devices.Service
	rec       *events.Recorder
}

func deviceNewFixture(t *testing.T) *deviceFixture {
	t.Helper()
	f := newFixture(t)
	rec := events.NewRecorder(f.db)
	f.svc.cfg.Events = rec
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	approvals, err := NewDeviceApprovals(f.svc)
	require.NoError(t, err)
	d := &deviceFixture{fixture: f, key: key, approvals: approvals, rec: rec}
	d.pairing = devices.New(devices.Config{DB: f.db, Events: rec, PeerAuth: true, Approver: approvals,
		LocalHost: "example-server", Now: func() time.Time { return f.now }})
	return d
}

// devicePending pairs a relay named name under peer_auth: it waits.
func (d *deviceFixture) devicePending(t *testing.T, name string) store.Device {
	t.Helper()
	ctx := context.Background()
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	answer, err := d.pairing.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindRelay, Name: name, Secret: secret,
		RelayKey: id.PublicKey()})
	require.NoError(t, err)
	require.Equal(t, devices.StatusPending, answer.Status, "under peer_auth a code without approval enrolls nothing")
	return answer.Device
}

// deviceAssert begins an approval of id and returns its ticket and the
// passkey's answer.
func (d *deviceFixture) deviceAssert(t *testing.T, id string) (string, []byte) {
	t.Helper()
	begun, err := d.approvals.BeginDeviceApproval(context.Background(), id)
	require.NoError(t, err)
	resp, err := d.key.Get(begun.Options, origin)
	require.NoError(t, err)
	return begun.Ticket, resp
}

func TestDeviceApproval_PasskeyPairsTheDevice(t *testing.T) {
	f := deviceNewFixture(t)
	ctx := context.Background()
	pending := f.devicePending(t, "example-laptop")

	_, err := f.approvals.BeginDeviceApproval(ctx, pending.ID)
	require.ErrorIs(t, err, ErrNoPasskey, "nothing can prove the approval yet")
	f.cred = f.enrollWithCode(t, f.key, "key")

	ticket, resp := f.deviceAssert(t, pending.ID)
	paired, err := f.pairing.FinishApproval(ctx, pending.ID, ticket, resp)
	require.NoError(t, err)
	require.Equal(t, store.DeviceActive, paired.Status)
	require.Equal(t, f.cred.ID, paired.ApprovedBy, "the device names the passkey that approved it")
	used, err := store.ApproverByID(ctx, f.db, f.cred.ID)
	require.NoError(t, err)
	require.NotNil(t, used.LastUsedAt, "the passkey's use is recorded")

	evs, err := f.rec.ByKinds(ctx, []core.EventKind{devices.EventPaired}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, pending.ID, evs[0].ItemID)
	require.Equal(t, devices.ProofPasskey, evs[0].Payload["proof"])
	require.Equal(t, f.cred.ID, evs[0].Payload["credential"])

	_, err = f.pairing.FinishApproval(ctx, pending.ID, ticket, resp)
	require.ErrorIs(t, err, ErrDeviceNotWaiting, "an approval works once")
	_, err = f.approvals.BeginDeviceApproval(ctx, pending.ID)
	require.ErrorIs(t, err, ErrDeviceNotWaiting)
}

// Another key cannot take an approval: an assertion over one device's
// challenge approves no other, and leaves both waiting.
func TestDeviceApproval_AnotherDeviceCannotTakeTheApproval(t *testing.T) {
	f := deviceNewFixture(t)
	ctx := context.Background()
	f.cred = f.enrollWithCode(t, f.key, "key")
	mine := f.devicePending(t, "example-laptop")
	theirs := f.devicePending(t, "example-ci")

	ticket, resp := f.deviceAssert(t, mine.ID)
	_, err := f.pairing.FinishApproval(ctx, theirs.ID, ticket, resp)
	require.Error(t, err, "the assertion signed the challenge of the device the owner looked at")
	for _, id := range []string{mine.ID, theirs.ID} {
		d, err := store.DeviceByID(ctx, f.db, id)
		require.NoError(t, err)
		require.Equal(t, store.DevicePending, d.Status, id)
	}
	// The owner's own approval still works for the device it was made for: a
	// failed finish uses up nothing but that attempt.
	ticket, resp = f.deviceAssert(t, mine.ID)
	_, err = f.pairing.FinishApproval(ctx, mine.ID, ticket, resp)
	require.NoError(t, err)
}

// The ticket ends with its deadline, and a pairing ends with its own.
func TestDeviceApproval_Deadlines(t *testing.T) {
	f := deviceNewFixture(t)
	ctx := context.Background()
	f.cred = f.enrollWithCode(t, f.key, "key")
	pending := f.devicePending(t, "example-laptop")
	ticket, resp := f.deviceAssert(t, pending.ID)
	f.now = f.now.Add(deviceTicketTTL)
	_, err := f.pairing.FinishApproval(ctx, pending.ID, ticket, resp)
	require.ErrorIs(t, err, ErrSignInTicket)
	_, err = f.pairing.FinishApproval(ctx, pending.ID, "v1.garbage", resp)
	require.ErrorIs(t, err, ErrSignInTicket)

	f.now = f.now.Add(store.DevicePendingTTL)
	_, err = f.approvals.BeginDeviceApproval(ctx, pending.ID)
	require.ErrorIs(t, err, ErrDeviceNotWaiting, "a pairing past its deadline is not approved")
	_, err = f.approvals.BeginDeviceApproval(ctx, "01NOSUCHDEVICE")
	require.ErrorIs(t, err, store.ErrDeviceNotFound)
}

// A denied pairing is not approved, whatever the passkey says.
func TestDeviceApproval_DeniedIsNotApproved(t *testing.T) {
	f := deviceNewFixture(t)
	ctx := context.Background()
	f.cred = f.enrollWithCode(t, f.key, "key")
	pending := f.devicePending(t, "example-laptop")
	ticket, resp := f.deviceAssert(t, pending.ID)
	_, err := f.pairing.Revoke(ctx, pending.ID, store.DeviceByConsole)
	require.NoError(t, err)
	_, err = f.pairing.FinishApproval(ctx, pending.ID, ticket, resp)
	require.ErrorIs(t, err, ErrDeviceNotWaiting)
}

// A restricted account approves nothing.
func TestDeviceApproval_RefusesRestricted(t *testing.T) {
	f := newGrantFixture(t)
	approvals, err := NewDeviceApprovals(f.svc)
	require.NoError(t, err)
	rctx := access.WithView(context.Background(), restrictedView(t, f, bridge, agent, shell))
	_, err = approvals.BeginDeviceApproval(rctx, "01ANYDEVICE")
	require.ErrorIs(t, err, ErrRestricted)
	_, err = approvals.FinishDeviceApproval(rctx, "01ANYDEVICE", "v1.1.x", []byte("{}"))
	require.ErrorIs(t, err, ErrRestricted)
}

// Without peer_auth a pairing needs no passkey, and the service offers none.
func TestDeviceApproval_NoneWithoutPeerAuth(t *testing.T) {
	f := deviceNewFixture(t)
	off := devices.New(devices.Config{DB: f.db, Approver: f.approvals})
	require.Nil(t, off.Approvals())
	_, err := off.BeginApproval(context.Background(), "01ANYDEVICE")
	require.ErrorIs(t, err, devices.ErrNoApprover)
}
