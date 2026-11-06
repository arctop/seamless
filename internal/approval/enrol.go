// Package approval is the owner's approval of access to locked projects
// (peer_auth.locked): the approver credentials that can sign an approval, and
// how they are enrolled.
//
// An approver credential is a WebAuthn passkey (internal/passkey) or the
// macOS Touch ID helper's Secure Enclave key. The first one is enrolled with a
// one-time code that only `seamlessd approvers enroll-code`, run as the
// daemon's own account, can mint; a later one with a code too, or with the
// approval of an existing passkey over a challenge that binds the new
// credential. The store keeps the rows and the code rules (store/approvers.go);
// this package runs the ceremonies around them.
package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// EnrolTTL is how long a started passkey enrolment waits for its browser.
const EnrolTTL = 5 * time.Minute

// maxPending caps the passkey enrolments in flight, so a caller looping on
// begin cannot grow the table without bound.
const maxPending = 32

var (
	// ErrNoEnrolment is a finish or approve step naming an enrolment that was
	// never begun, already finished, or expired.
	ErrNoEnrolment = errors.New("no such enrolment in progress")
	// ErrPasskeysOff is a passkey ceremony on a daemon whose relying party
	// is not configured (no Service, or no Verifier).
	ErrPasskeysOff = errors.New("passkeys are not configured")
	// ErrTouchIDKey is a Touch ID key or proof that does not verify.
	ErrTouchIDKey = errors.New("invalid Touch ID key")
)

// Config wires a Service.
type Config struct {
	DB *sql.DB
	// Passkeys is the relying party (internal/passkey). Nil disables every
	// passkey ceremony; Touch ID enrolment still works.
	Passkeys *passkey.Verifier
	// Origins are the relying party's origins, for the pages that must send
	// the owner there (a ceremony on any other origin fails).
	Origins []string
	// UserName and UserDisplayName are what an authenticator shows for the
	// owner's account ("owner", "Seamless owner on <host>").
	UserName, UserDisplayName string
	// Events records access.requested, approved, denied and revoked; nil
	// records nothing.
	Events *events.Recorder
	// Host is this machine's name, for the repo-map warning (a session's cwd
	// outside the project's mapped repositories).
	Host string
	// LeaseCap is peer_auth.max_lease; 0 means 8 hours.
	LeaseCap time.Duration
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service runs enrolments. It is safe for concurrent use.
type Service struct {
	cfg     Config
	mu      sync.Mutex
	pending map[string]*pendingEnrol
}

// New builds a Service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.UserName == "" {
		cfg.UserName = "owner"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{cfg: cfg, pending: map[string]*pendingEnrol{}}
}

// PasskeysOn reports whether the passkey ceremonies are available.
func (s *Service) PasskeysOn() bool { return s != nil && s.cfg.Passkeys != nil }

// Origins returns the relying party's origins.
func (s *Service) Origins() []string { return slices.Clone(s.cfg.Origins) }

// pendingEnrol is one passkey enrolment between its requests.
type pendingEnrol struct {
	name    string
	code    string // "" when an existing passkey approves it
	state   passkey.RegistrationState
	expires time.Time
	// cred and binding are set once the new credential is created and an
	// existing passkey's approval is awaited.
	cred    *passkey.Credential
	binding []byte
}

// Begun is a started passkey enrolment: its id and the creation options for
// navigator.credentials.create.
type Begun struct {
	ID      string          `json:"id"`
	Options json.RawMessage `json:"options"`
}

// Finished is a passkey enrolment's result. Either the credential was stored
// (Credential set), or an existing passkey must approve it first: Approve then
// holds the request options for navigator.credentials.get.
type Finished struct {
	Credential *store.ApproverCredential `json:"credential,omitempty"`
	Approve    json.RawMessage           `json:"approve,omitempty"`
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// gc drops expired enrolments; the caller holds mu.
func (s *Service) gc(now time.Time) {
	for id, p := range s.pending {
		if !now.Before(p.expires) {
			delete(s.pending, id)
		}
	}
}

// take removes and returns a live enrolment.
func (s *Service) take(id string, now time.Time) (*pendingEnrol, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(now)
	p, ok := s.pending[id]
	if !ok {
		return nil, ErrNoEnrolment
	}
	delete(s.pending, id)
	return p, nil
}

func (s *Service) user(ctx context.Context) (passkey.User, error) {
	handle, err := store.ApproverUserHandle(ctx, s.cfg.DB)
	if err != nil {
		return passkey.User{}, err
	}
	return passkey.User{Handle: handle, Name: s.cfg.UserName, DisplayName: s.cfg.UserDisplayName}, nil
}

func (s *Service) passkeys(ctx context.Context) ([]passkey.Credential, error) {
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return nil, err
	}
	out := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		out[i] = ToPasskey(r)
	}
	return out, nil
}

// BeginPasskey starts enrolling a passkey named name. With a code, the code
// must be live (it is checked now and consumed at the end); without one, an
// existing passkey will have to approve the new credential, so one must be
// enrolled. Every enrolled passkey is excluded, so an authenticator that holds
// one already is not enrolled twice.
func (s *Service) BeginPasskey(ctx context.Context, name, code string) (Begun, error) {
	if !s.PasskeysOn() {
		return Begun{}, ErrPasskeysOff
	}
	now := s.cfg.Now()
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > store.MaxApproverNameRunes || strings.ContainsAny(name, "\r\n\t") {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w: a name is 1 to %d characters on one line", store.ErrInvalidApprover, store.MaxApproverNameRunes)
	}
	existing, err := s.passkeys(ctx)
	if err != nil {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", err)
	}
	if code = strings.TrimSpace(code); code != "" {
		if err := store.CheckEnrolCode(ctx, s.cfg.DB, store.ApproverPasskey, code, now); err != nil {
			return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", err)
		}
	} else if len(existing) == 0 {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", store.ErrEnrolProof)
	}
	user, err := s.user(ctx)
	if err != nil {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", err)
	}
	opts, state, err := s.cfg.Passkeys.BeginRegistration(user, existing)
	if err != nil {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", err)
	}
	id, err := newID()
	if err != nil {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(now)
	if len(s.pending) >= maxPending {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: too many enrolments in progress; try again in a few minutes")
	}
	s.pending[id] = &pendingEnrol{name: name, code: code, state: state, expires: now.Add(EnrolTTL)}
	return Begun{ID: id, Options: opts}, nil
}

// FinishPasskey verifies the browser's creation response for enrolment id.
// With a code, the credential is stored and the code consumed, in one
// transaction. Without one, the new credential waits for an existing
// passkey's approval (ApprovePasskey) over a challenge that binds it.
func (s *Service) FinishPasskey(ctx context.Context, id string, response []byte) (Finished, error) {
	if !s.PasskeysOn() {
		return Finished{}, ErrPasskeysOff
	}
	now := s.cfg.Now()
	p, err := s.take(id, now)
	if err != nil || p.cred != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", ErrNoEnrolment)
	}
	user, err := s.user(ctx)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	cred, err := s.cfg.Passkeys.FinishRegistration(user, p.state, response)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	if p.code != "" {
		stored, err := store.EnrolApprover(ctx, s.cfg.DB, FromPasskey(cred, p.name), p.code, "", now)
		if err != nil {
			return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
		}
		return Finished{Credential: &stored}, nil
	}
	existing, err := s.passkeys(ctx)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	binding := enrolBinding(id, cred)
	opts, err := s.cfg.Passkeys.BeginAssertion(binding, existing)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	p.cred, p.binding = &cred, binding
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[id] = p
	return Finished{Approve: opts}, nil
}

// ApprovePasskey verifies an existing passkey's approval of enrolment id's new
// credential -- an assertion over the challenge that binds it -- records the
// approving credential's use, and stores the new one as added by it.
func (s *Service) ApprovePasskey(ctx context.Context, id string, response []byte) (store.ApproverCredential, error) {
	if !s.PasskeysOn() {
		return store.ApproverCredential{}, ErrPasskeysOff
	}
	now := s.cfg.Now()
	p, err := s.take(id, now)
	if err != nil || p.cred == nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", ErrNoEnrolment)
	}
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", err)
	}
	allowed := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		allowed[i] = ToPasskey(r)
	}
	used, err := s.cfg.Passkeys.FinishAssertion(p.binding, allowed, response)
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", err)
	}
	approver, ok := rowFor(rows, used.ID)
	if !ok {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", passkey.ErrUnknownCredential)
	}
	if err := store.RecordApproverUse(ctx, s.cfg.DB, approver.ID, approver.SignCount, used.SignCount, used.BackupState, now); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", err)
	}
	stored, err := store.EnrolApprover(ctx, s.cfg.DB, FromPasskey(*p.cred, p.name), "", approver.ID, now)
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", err)
	}
	return stored, nil
}

func rowFor(rows []store.ApproverCredential, credID []byte) (store.ApproverCredential, bool) {
	for _, r := range rows {
		if string(r.CredentialID) == string(credID) {
			return r, true
		}
	}
	return store.ApproverCredential{}, false
}

// enrolBinding is the 32-byte challenge an existing passkey signs to approve a
// new credential: it names this enrolment and the new credential's id and
// public key, so the approval cannot be lifted onto another enrolment or
// another key.
func enrolBinding(id string, c passkey.Credential) []byte {
	h := sha256.New()
	h.Write([]byte("seamless approver enrolment v1\x00"))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(c.ID)
	h.Write([]byte{0})
	h.Write(c.PublicKey)
	return h.Sum(nil)
}

// TouchIDEnrolMessage is what the Touch ID helper signs with its new key to
// enrol it: proof that the code's holder holds the key. The code is folded in
// as its hash, never in the clear.
func TouchIDEnrolMessage(code string, publicKey []byte) []byte {
	sum := sha256.Sum256([]byte(store.NormalizeEnrolCode(code)))
	msg := []byte("seamless touchid enrolment v1\x00")
	msg = append(msg, sum[:]...)
	return append(msg, publicKey...)
}

// EnrolTouchID stores a Touch ID key: publicKey is the raw uncompressed P-256
// point, and signature an ASN.1 ECDSA signature by that key over
// TouchIDEnrolMessage(code, publicKey). The code must be a live touchid code;
// it is consumed with the insert.
func (s *Service) EnrolTouchID(ctx context.Context, name, code string, publicKey, signature []byte) (store.ApproverCredential, error) {
	if err := VerifyTouchID(publicKey, TouchIDEnrolMessage(code, publicKey), signature); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.EnrolTouchID: %w", err)
	}
	keyID := sha256.Sum256(publicKey)
	cred := store.ApproverCredential{
		Kind: store.ApproverTouchID, CredentialID: keyID[:], PublicKey: publicKey, Alg: passkey.AlgES256, Name: name,
	}
	stored, err := store.EnrolApprover(ctx, s.cfg.DB, cred, code, "", s.cfg.Now())
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.EnrolTouchID: %w", err)
	}
	return stored, nil
}

// VerifyTouchID checks an ASN.1 ECDSA P-256 signature by publicKey (a raw
// uncompressed point) over SHA-256 of message: what the Secure Enclave's
// ecdsaSignatureMessageX962SHA256 produces.
func VerifyTouchID(publicKey, message, signature []byte) error {
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), publicKey)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTouchIDKey, err)
	}
	digest := sha256.Sum256(message)
	if !ecdsa.VerifyASN1(pub, digest[:], signature) {
		return fmt.Errorf("%w: the signature does not verify", ErrTouchIDKey)
	}
	return nil
}

// ToPasskey is a stored passkey row as internal/passkey verifies it.
func ToPasskey(r store.ApproverCredential) passkey.Credential {
	return passkey.Credential{
		ID: r.CredentialID, PublicKey: r.PublicKey, Algorithm: r.Alg, SignCount: r.SignCount,
		AAGUID: r.AAGUID, Transports: r.Transports, Attachment: r.Attachment,
		BackupEligible: r.BackupEligible, BackupState: r.BackupState, UserHandle: r.UserHandle,
		AttestationFormat: r.AttestationFormat,
	}
}

// FromPasskey is a registered passkey as a row to store, labeled name.
func FromPasskey(c passkey.Credential, name string) store.ApproverCredential {
	return store.ApproverCredential{
		Kind: store.ApproverPasskey, CredentialID: c.ID, PublicKey: c.PublicKey, Alg: c.Algorithm, SignCount: c.SignCount,
		BackupEligible: c.BackupEligible, BackupState: c.BackupState, AAGUID: c.AAGUID, Transports: c.Transports,
		Attachment: c.Attachment, AttestationFormat: c.AttestationFormat, UserHandle: c.UserHandle, Name: name,
	}
}
