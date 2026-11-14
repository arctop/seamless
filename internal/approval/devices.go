package approval

// Device pairing under peer_auth (plan networked-relay): a computer pairs
// with a one-time code, and the pairing waits until the owner approves it
// with a passkey on the console's Devices page. The code alone opens
// nothing, because a code read off a screen or a terminal is no proof of the
// owner. The owner compares the fingerprint the device's terminal shows with
// the one the page shows, and the assertion signs a challenge bound to that
// device: its id, kind, name, fingerprint and key. An approval of one device
// can never be lifted onto another, and a pairing whose key differs from the
// one the page showed finds no row to approve (store.ApproveDevice compares
// the fingerprint inside its write).
//
// The ceremony is stateless until it succeeds, as the console's sign-in is
// (signin.go): begin hands the browser a ticket (a deadline and a nonce) and
// the options over a challenge that is a keyed hash of the ticket and the
// device under a secret only this process holds; finish recomputes it from
// the ticket and the device as it stands. Nothing is stored per begin, and
// the device's compare-and-set makes an approval single use.
//
// No route and no terminal command approves a pairing any other way: the
// passkey on the Devices page is the one proof this build takes.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// deviceTicketTTL is how long a begun approval waits for its assertion.
const deviceTicketTTL = ConsoleUnlockTTL

// ErrDeviceNotWaiting is an approval of a device that is not waiting for one:
// already paired, denied, revoked, or past its deadline.
var ErrDeviceNotWaiting = errors.New("that device is not waiting for approval")

// DeviceApprovals approves pending device pairings with the owner's passkey.
// It satisfies devices.Approver. It is safe for concurrent use.
type DeviceApprovals struct {
	svc    *Service
	secret []byte
}

// NewDeviceApprovals builds the approvals over svc, the daemon's approval
// service, whose relying party and enrolled passkeys it uses.
func NewDeviceApprovals(svc *Service) (*DeviceApprovals, error) {
	if svc == nil {
		return nil, fmt.Errorf("approval.NewDeviceApprovals: %w", ErrPasskeysOff)
	}
	secret := make([]byte, sha256.Size)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("approval.NewDeviceApprovals: %w", err)
	}
	return &DeviceApprovals{svc: svc, secret: secret}, nil
}

// PasskeysOn reports whether the relying party is configured.
func (a *DeviceApprovals) PasskeysOn() bool { return a.svc.PasskeysOn() }

// Origins returns where a passkey works.
func (a *DeviceApprovals) Origins() []string { return a.svc.Origins() }

// deviceChallenge is the 32 bytes the owner's passkey signs to approve d: a
// keyed hash, under this process's secret, of the ticket's deadline and
// nonce and of everything the page showed about the device.
func (a *DeviceApprovals) deviceChallenge(deadline int64, nonce []byte, d store.Device) []byte {
	m := hmac.New(sha256.New, a.secret)
	for _, part := range [][]byte{
		[]byte("seamless device approval v1"), []byte(strconv.FormatInt(deadline, 10)), nonce,
		[]byte(d.ID), []byte(d.Kind), []byte(d.Name), []byte(d.Fingerprint), d.PublicKey,
	} {
		m.Write(part)
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// waiting reads the device id, which must be waiting for approval now.
func (a *DeviceApprovals) waiting(ctx context.Context, id string) (store.Device, error) {
	d, err := store.DeviceByID(ctx, a.svc.cfg.DB, id)
	if err != nil {
		return store.Device{}, err
	}
	if d.StatusAt(a.svc.cfg.Now()) != store.DevicePending {
		return store.Device{}, ErrDeviceNotWaiting
	}
	return d, nil
}

// BeginDeviceApproval starts the owner's approval of the pending device id: a
// ticket, and the navigator.credentials.get options over a challenge bound to
// that device, allowing every enrolled passkey.
func (a *DeviceApprovals) BeginDeviceApproval(ctx context.Context, id string) (devices.Ceremony, error) {
	const op = "approval.BeginDeviceApproval"
	if err := refuseRestricted(ctx); err != nil {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, err)
	}
	if !a.svc.PasskeysOn() {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	d, err := a.waiting(ctx, id)
	if err != nil {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, err)
	}
	creds, err := a.svc.passkeys(ctx)
	if err != nil {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(creds) == 0 {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, ErrNoPasskey)
	}
	ticket, deadline, nonce, err := newTicket(a.svc.cfg.Now(), deviceTicketTTL)
	if err != nil {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, err)
	}
	opts, err := a.svc.cfg.Passkeys.BeginAssertion(a.deviceChallenge(deadline, nonce, d), creds)
	if err != nil {
		return devices.Ceremony{}, fmt.Errorf("%s: %w", op, err)
	}
	return devices.Ceremony{Ticket: ticket, Options: opts}, nil
}

// FinishDeviceApproval verifies the owner's assertion for ticket over the
// device id as it stands, records the passkey's use (a compare-and-set on its
// counter) and pairs the device: active, approved by that passkey
// (store.ApproveDevice, which compares the fingerprint the challenge was bound
// to inside its write). A key device collects its key on its next status.
func (a *DeviceApprovals) FinishDeviceApproval(ctx context.Context, id, ticket string, response []byte) (store.Device, error) {
	const op = "approval.FinishDeviceApproval"
	if err := refuseRestricted(ctx); err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	if !a.svc.PasskeysOn() {
		return store.Device{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	now := a.svc.cfg.Now()
	deadline, nonce, err := parseTicket(ticket, now)
	if err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	d, err := a.waiting(ctx, id)
	if err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	rows, err := store.ApproverCredentials(ctx, a.svc.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(rows) == 0 {
		return store.Device{}, fmt.Errorf("%s: %w", op, ErrNoPasskey)
	}
	allowed := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		allowed[i] = ToPasskey(r)
	}
	used, err := a.svc.cfg.Passkeys.FinishAssertion(a.deviceChallenge(deadline, nonce, d), allowed, response)
	if err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	row, ok := rowFor(rows, used.ID)
	if !ok {
		return store.Device{}, fmt.Errorf("%s: %w", op, passkey.ErrUnknownCredential)
	}
	if err := store.RecordApproverUse(ctx, a.svc.cfg.DB, row.ID, row.SignCount, used.SignCount, used.BackupState, now); err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	paired, err := store.ApproveDevice(ctx, a.svc.cfg.DB, d.ID, d.Fingerprint, row.ID, now)
	if errors.Is(err, store.ErrDeviceNotPending) {
		return store.Device{}, fmt.Errorf("%s: %w", op, ErrDeviceNotWaiting)
	}
	if err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	a.svc.record(ctx, core.Event{Kind: devices.EventPaired, ItemID: paired.ID, Payload: map[string]any{
		"kind": paired.Kind, "name": paired.Name, "proof": devices.ProofPasskey, "credential": row.ID}})
	return paired, nil
}
