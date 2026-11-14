package devices

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// liveSweepEvery is how often the service checks the devices of the requests
// still running (a live feed, an MCP stream) and ends those of a device the
// owner revoked from somewhere else than this daemon (the terminal).
const liveSweepEvery = 30 * time.Second

var (
	// ErrServerName is a pairing under this server's own host name: a device
	// speaking for the server's host could have the server read its own disk
	// on a remote caller's word.
	ErrServerName = errors.New("that is this server's own name: pair the device under its own")
	// ErrRelayNeedsCertificate is a relay pairing that presented no client
	// certificate: a relay pairs over TLS, offering RelayALPN.
	ErrRelayNeedsCertificate = errors.New("a relay pairs over TLS with its client certificate")
	// ErrKeyWithCertificate is a key device pairing that presented a client
	// certificate: only a relay does.
	ErrKeyWithCertificate = errors.New("a key device pairs without a client certificate")
	// ErrNoApprover is an approval asked of a server whose pairings need none
	// (peer_auth off) or that has no passkey ceremony.
	ErrNoApprover = errors.New("pairings on this server need no passkey: the one-time code is the approval")
	// ErrUnknownPairing is a status asked with a secret no pairing chose.
	ErrUnknownPairing = errors.New("no pairing was made with that secret")
	// ErrRelayNotPaired is a relay certificate no active device holds.
	ErrRelayNotPaired = errors.New("this relay is not paired with this server")
	// ErrRelayPending is a relay certificate whose pairing still waits for the
	// owner's approval.
	ErrRelayPending = errors.New("this relay's pairing is waiting for the owner's approval on the server's Devices page")
	// ErrRelayRevoked is a relay certificate whose pairing was revoked.
	ErrRelayRevoked = errors.New("this relay's pairing was revoked: pair it again")
	// ErrKind is a pairing of a kind the server does not know.
	ErrKind = errors.New("invalid device kind: valid kinds are relay, key")
)

// ServerFacts are what a client needs from this server to pair with it.
type ServerFacts struct {
	// URL is the address clients dial (server_url).
	URL string `json:"url"`
	// CertFingerprint is the TLS certificate's SHA-256 fingerprint
	// (CertFingerprint), "" without TLS.
	CertFingerprint string `json:"cert_fingerprint,omitempty"`
	// TLS says the listener is https: a relay can pair only then.
	TLS bool `json:"tls"`
}

// Ceremony is a begun passkey approval: the ticket the browser sends back
// with its answer, and the options for navigator.credentials.get.
type Ceremony struct {
	Ticket  string          `json:"ticket"`
	Options json.RawMessage `json:"options"`
}

// Approver is the owner's passkey approval of a pending pairing
// (*approval.DeviceApprovals under peer_auth). It verifies the assertion and
// makes the device active (store.ApproveDevice), and records the pairing.
type Approver interface {
	BeginDeviceApproval(ctx context.Context, id string) (Ceremony, error)
	FinishDeviceApproval(ctx context.Context, id, ticket string, response []byte) (store.Device, error)
	// PasskeysOn reports whether the relying party is configured, and
	// Origins where a passkey works.
	PasskeysOn() bool
	Origins() []string
}

// Config wires a Service.
type Config struct {
	DB     *sql.DB
	Events *events.Recorder
	// PeerAuth makes every pairing wait for the owner's passkey (Approver);
	// off, the one-time code is the approval.
	PeerAuth bool
	// Approver is the passkey approval; nil while PeerAuth is off.
	Approver Approver
	// LocalHost is this server's own host name (config.Hostname): no device
	// may pair under it.
	LocalHost string
	// Server is what a client needs to pair (the Devices page, the closing
	// output of the server's setup).
	Server ServerFacts
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service is device pairing on a networked server: the codes, the pairings,
// the edge that authenticates a paired device's requests, and revocation. It
// is safe for concurrent use.
type Service struct {
	cfg Config
	mu  sync.Mutex
	// live are the cancel functions of the requests a paired device is
	// making right now, by device id, so a revocation ends them at once.
	live map[string]map[uint64]context.CancelFunc
	seq  uint64
}

// New builds a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.LocalHost = NormalizeName(cfg.LocalHost)
	return &Service{cfg: cfg, live: map[string]map[uint64]context.CancelFunc{}}
}

// PeerAuth reports whether a pairing waits for the owner's passkey.
func (s *Service) PeerAuth() bool { return s.cfg.PeerAuth }

// Server returns what a client needs to pair.
func (s *Service) Server() ServerFacts { return s.cfg.Server }

// Approvals returns the passkey approval, nil when pairings need none.
func (s *Service) Approvals() Approver {
	if !s.cfg.PeerAuth {
		return nil
	}
	return s.cfg.Approver
}

func (s *Service) record(ctx context.Context, e core.Event) {
	if s.cfg.Events == nil {
		return
	}
	if _, err := s.cfg.Events.Record(context.WithoutCancel(ctx), e); err != nil {
		s.cfg.Logger.Warn("devices: record event", "kind", e.Kind, "error", err)
	}
}

// Code is a minted pairing code.
type Code struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	// Approval says what else a pairing with it needs: "" (the code is the
	// approval) or "passkey" (the owner approves it on the Devices page).
	Approval string `json:"approval,omitempty"`
}

// MintCode mints a one-time pairing code, from by (console or terminal). A
// server no other computer can pair with (ErrPairingNeedsTLS), or whose
// address names a user (ErrPairingNamesUser), mints none
// (ServerFacts.PairingRefusal).
func (s *Service) MintCode(ctx context.Context, by string) (Code, error) {
	const op = "devices.MintCode"
	if err := s.cfg.Server.PairingRefusal(); err != nil {
		return Code{}, fmt.Errorf("%s: %w", op, err)
	}
	code, expires, err := store.MintPairingCode(ctx, s.cfg.DB, by, s.cfg.Now())
	if err != nil {
		return Code{}, fmt.Errorf("%s: %w", op, err)
	}
	s.record(ctx, core.Event{Kind: EventCodeMinted, Payload: map[string]any{
		"by": by, "minutes": int(store.PairingCodeTTL / time.Minute)}})
	c := Code{Code: code, ExpiresAt: expires}
	if s.cfg.PeerAuth {
		c.Approval = ProofPasskey
	}
	return c, nil
}

// PairInput is one pairing request, as the pairing endpoint read it.
type PairInput struct {
	Code   string
	Kind   string
	Name   string
	Secret string
	// RelayKey is the DER SubjectPublicKeyInfo of the client certificate the
	// request's TLS connection presented, nil for none: a relay's key comes
	// from the handshake that proved the relay holds it, never from the body.
	RelayKey []byte
}

// PairAnswer is a pairing's state, as the device is told it.
type PairAnswer struct {
	// Status is paired, pending, denied, expired or revoked.
	Status string       `json:"status"`
	Device store.Device `json:"device"`
	// Key is a key device's own key, given once: in the answer that pairs
	// it, or, under peer_auth, the first status after the owner approved it.
	Key string `json:"key,omitempty"`
}

// Pairing statuses a device is told.
const (
	StatusPaired  = "paired"
	StatusPending = "pending"
)

// Pair takes a pairing request. Without peer_auth the code is the approval
// and the device is active at once; a key device's key comes back in the
// answer, once, and the server keeps only its hash. Under peer_auth the
// pairing waits for the owner's passkey and nothing is active yet.
func (s *Service) Pair(ctx context.Context, in PairInput) (PairAnswer, error) {
	const op = "devices.Pair"
	name := NormalizeName(in.Name)
	if err := store.ValidDeviceName(name); err != nil {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
	}
	secretHash, err := SecretHash(in.Secret)
	if err != nil {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
	}
	// The server's own name is refused after the code is, inside the
	// pairing's transaction: a caller without a code learns nothing of it.
	nd := store.NewDevice{Kind: in.Kind, Name: name, SecretHash: secretHash, Pending: s.cfg.PeerAuth, Reserved: s.cfg.LocalHost}
	var key string
	switch in.Kind {
	case KindRelay:
		if len(in.RelayKey) == 0 {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, ErrRelayNeedsCertificate)
		}
		pub, err := x509.ParsePKIXPublicKey(in.RelayKey)
		if err != nil {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, ErrRelayCertificate)
		}
		if err := acceptedRelayKey(pub); err != nil {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
		}
		nd.PublicKey, nd.KeyHash, nd.Fingerprint = in.RelayKey, PublicKeyHash(in.RelayKey), PublicKeyFingerprint(in.RelayKey)
	case KindKey:
		if len(in.RelayKey) != 0 {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, ErrKeyWithCertificate)
		}
		if nd.Fingerprint, err = SecretFingerprint(in.Secret); err != nil {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
		}
		if !s.cfg.PeerAuth {
			var shown string
			if key, shown, err = NewKey(); err != nil {
				return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
			}
			nd.KeyHash, nd.KeyPrefix = KeyHash(key), shown
		}
	default:
		return PairAnswer{}, fmt.Errorf("%s: %w", op, ErrKind)
	}
	d, err := store.PairDevice(ctx, s.cfg.DB, in.Code, nd, s.cfg.Now())
	if errors.Is(err, store.ErrDeviceNameReserved) {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, ErrServerName)
	}
	if err != nil {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
	}
	if d.Status == store.DevicePending {
		s.record(ctx, core.Event{Kind: EventPending, ItemID: d.ID, Payload: map[string]any{"kind": d.Kind, "name": d.Name}})
		return PairAnswer{Status: StatusPending, Device: d}, nil
	}
	s.record(ctx, core.Event{Kind: EventPaired, ItemID: d.ID, Payload: map[string]any{
		"kind": d.Kind, "name": d.Name, "proof": ProofCode}})
	return PairAnswer{Status: StatusPaired, Device: d, Key: key}, nil
}

// Status answers a device asking after its pairing with its secret. A key
// device the owner approved collects its key here, once.
func (s *Service) Status(ctx context.Context, secret string) (PairAnswer, error) {
	const op = "devices.Status"
	secretHash, err := SecretHash(secret)
	if err != nil {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
	}
	d, err := store.DeviceBySecret(ctx, s.cfg.DB, secretHash)
	if errors.Is(err, store.ErrDeviceNotFound) {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, ErrUnknownPairing)
	}
	if err != nil {
		return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
	}
	switch status := d.StatusAt(s.cfg.Now()); status {
	case store.DevicePending:
		return PairAnswer{Status: StatusPending, Device: d}, nil
	case store.DeviceActive:
		if d.Kind != KindKey || d.HasKey {
			return PairAnswer{Status: StatusPaired, Device: d}, nil
		}
		key, shown, err := NewKey()
		if err != nil {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
		}
		collected, err := store.CollectDeviceKey(ctx, s.cfg.DB, secretHash, KeyHash(key), shown)
		if errors.Is(err, store.ErrDeviceKeyCollected) {
			// Another status call collected it first, or the device ended in
			// between: the answer names the state as it now is, without a key.
			now, rerr := store.DeviceBySecret(ctx, s.cfg.DB, secretHash)
			if rerr != nil {
				return PairAnswer{}, fmt.Errorf("%s: %w", op, rerr)
			}
			st := now.StatusAt(s.cfg.Now())
			if st == store.DeviceActive {
				st = StatusPaired
			}
			return PairAnswer{Status: st, Device: now}, nil
		}
		if err != nil {
			return PairAnswer{}, fmt.Errorf("%s: %w", op, err)
		}
		return PairAnswer{Status: StatusPaired, Device: collected, Key: key}, nil
	default:
		return PairAnswer{Status: status, Device: d}, nil
	}
}

// List returns every device (store.ListDevices).
func (s *Service) List(ctx context.Context) ([]store.Device, error) {
	out, err := store.ListDevices(ctx, s.cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("devices.List: %w", err)
	}
	return out, nil
}

// Revoke ends the device id, from by: an active device is revoked and its
// credential refused from its next request on, and every request it is
// making now ends; a pending pairing is denied. A relay's leases and grants,
// and the access requests its processes still wait on, end in the
// revocation's own transaction (store.RevokeDeviceEndingAccess).
func (s *Service) Revoke(ctx context.Context, id, by string) (store.Device, error) {
	const op = "devices.Revoke"
	d, endedAccess, err := store.RevokeDeviceEndingAccess(ctx, s.cfg.DB, id, by, s.cfg.Now())
	if err != nil {
		return store.Device{}, fmt.Errorf("%s: %w", op, err)
	}
	ended := s.endLive(id)
	action := "revoke"
	if d.Status == store.DeviceDenied {
		action = "deny"
	}
	payload := map[string]any{"kind": d.Kind, "name": d.Name, "action": action, "by": by, "requests": ended}
	for k, n := range map[string]int64{"leases": endedAccess.Leases, "grants": endedAccess.Grants, "access_requests": endedAccess.Requests} {
		if n > 0 {
			payload[k] = n
		}
	}
	s.record(ctx, core.Event{Kind: EventRevoked, ItemID: d.ID, Payload: payload})
	if endedAccess.Leases+endedAccess.Grants > 0 {
		// Said the way every other early end of agents' access is said, so the
		// access page and the feed see it without knowing about devices.
		s.record(ctx, core.Event{Kind: core.EventAccessRevoked, Payload: map[string]any{
			"reason": store.LeaseRevokedDevice, "leases": endedAccess.Leases, "grants": endedAccess.Grants}})
	}
	return d, nil
}

// BeginApproval starts the owner's passkey approval of the pending device id.
func (s *Service) BeginApproval(ctx context.Context, id string) (Ceremony, error) {
	a := s.Approvals()
	if a == nil {
		return Ceremony{}, fmt.Errorf("devices.BeginApproval: %w", ErrNoApprover)
	}
	return a.BeginDeviceApproval(ctx, id)
}

// FinishApproval verifies the owner's assertion and pairs the device.
func (s *Service) FinishApproval(ctx context.Context, id, ticket string, response []byte) (store.Device, error) {
	a := s.Approvals()
	if a == nil {
		return store.Device{}, fmt.Errorf("devices.FinishApproval: %w", ErrNoApprover)
	}
	return a.FinishDeviceApproval(ctx, id, ticket, response)
}

// track registers a request the device id is making, and returns its context
// (cancelled when the device is revoked) and the function that ends the
// registration.
func (s *Service) track(ctx context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.seq++
	n := s.seq
	if s.live[id] == nil {
		s.live[id] = map[uint64]context.CancelFunc{}
	}
	s.live[id][n] = cancel
	s.mu.Unlock()
	return ctx, func() {
		cancel()
		s.mu.Lock()
		delete(s.live[id], n)
		if len(s.live[id]) == 0 {
			delete(s.live, id)
		}
		s.mu.Unlock()
	}
}

// endLive cancels every request the device id is making, and returns how
// many.
func (s *Service) endLive(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.live[id])
	for _, cancel := range s.live[id] {
		cancel()
	}
	delete(s.live, id)
	return n
}

// liveDevices lists the devices with a request running.
func (s *Service) liveDevices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.live))
	for id := range s.live {
		out = append(out, id)
	}
	return out
}

// Run ends, every liveSweepEvery until ctx ends, the running requests of a
// device that is no longer active: one revoked from the terminal, which this
// daemon did not see happen. It returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(liveSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

// sweep is one pass of Run.
func (s *Service) sweep(ctx context.Context) {
	for _, id := range s.liveDevices() {
		d, err := store.DeviceByID(ctx, s.cfg.DB, id)
		switch {
		case errors.Is(err, store.ErrDeviceNotFound):
			s.endLive(id)
		case err != nil:
			s.cfg.Logger.Warn("devices: check a live device", "device", id, "error", err)
		case d.Status != store.DeviceActive:
			s.endLive(id)
		}
	}
}
