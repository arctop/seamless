// Package approval is the owner's approval of access to locked projects:
// the approver credentials that can sign an approval, and
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

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// EnrollTTL is how long a started passkey enrollment waits for its browser and,
// by a code, for the owner to mint the code for the new key.
const EnrollTTL = 10 * time.Minute

// maxPending caps the passkey enrollments in flight, so a caller looping on
// begin cannot grow the table without bound.
const maxPending = 32

var (
	// ErrNoEnrollment is a finish or approve step naming an enrollment that was
	// never begun, already finished, or expired.
	ErrNoEnrollment = errors.New("no such enrollment in progress")
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
	// passkey ceremony; Touch ID enrollment still works.
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
	// ForceLocked is peer_auth.force_locked: the locks the console may not
	// change.
	ForceLocked []string
	// Programs names the process a token names, for the anchor rule
	// (anchor.go) and the facts' program: its executable path, else the
	// kernel's name for it, "" when no live process of this boot is that
	// token's. Nil is access.ProgramOf.
	Programs func(access.ProcessToken) string
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Service runs enrollments. It is safe for concurrent use.
type Service struct {
	cfg         Config
	mu          sync.Mutex
	pending     map[string]*pendingEnroll
	lockChanges map[string]*pendingLockChange
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
	if cfg.Programs == nil {
		cfg.Programs = access.ProgramOf
	}
	return &Service{cfg: cfg, pending: map[string]*pendingEnroll{}, lockChanges: map[string]*pendingLockChange{}}
}

// PasskeysOn reports whether the passkey ceremonies are available.
func (s *Service) PasskeysOn() bool { return s != nil && s.cfg.Passkeys != nil }

// Origins returns the relying party's origins.
func (s *Service) Origins() []string { return slices.Clone(s.cfg.Origins) }

// ErrTooManyEnrollments is a passkey enrollment refused because maxPending are
// already waiting.
var ErrTooManyEnrollments = errors.New("too many passkey enrollments in progress")

// pendingEnroll is one passkey enrollment between its requests.
type pendingEnroll struct {
	name    string
	byCode  bool // false when an existing passkey approves it
	state   passkey.RegistrationState
	expires time.Time
	// cred is set once the new credential is created; it then waits for the
	// code minted for it, or for an existing passkey's approval over binding.
	cred    *passkey.Credential
	binding []byte
}

// Begun is a started passkey enrollment: its id and the creation options for
// navigator.credentials.create.
type Begun struct {
	ID      string          `json:"id"`
	Options json.RawMessage `json:"options"`
}

// Finished is a passkey enrollment's result once the new credential exists. It
// waits for one more proof: Fingerprint names the key to mint a code for
// (ConfirmPasskeyCode takes the code), or Approve holds the request options for
// an existing passkey's navigator.credentials.get (ApprovePasskey).
type Finished struct {
	Fingerprint string          `json:"fingerprint,omitempty"`
	Approve     json.RawMessage `json:"approve,omitempty"`
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// gc drops expired enrollments; the caller holds mu.
func (s *Service) gc(now time.Time) {
	for id, p := range s.pending {
		if !now.Before(p.expires) {
			delete(s.pending, id)
		}
	}
	for id, p := range s.lockChanges {
		if !now.Before(p.expires) {
			delete(s.lockChanges, id)
		}
	}
}

// take removes and returns a live enrollment.
func (s *Service) take(id string, now time.Time) (*pendingEnroll, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(now)
	p, ok := s.pending[id]
	if !ok {
		return nil, ErrNoEnrollment
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

// BeginPasskey starts enrolling a passkey named name. By code, the code comes
// after the credential exists, because it is minted for that credential's key
// (ConfirmPasskeyCode). Otherwise an existing passkey will have to approve the
// new credential, so one must be enrolled. Every enrolled passkey is excluded,
// so an authenticator that holds one already is not enrolled twice.
func (s *Service) BeginPasskey(ctx context.Context, name string, byCode bool) (Begun, error) {
	if err := refuseRestricted(ctx); err != nil {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", err)
	}
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
	if !byCode && len(existing) == 0 {
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", store.ErrEnrollProof)
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
		return Begun{}, fmt.Errorf("approval.BeginPasskey: %w", ErrTooManyEnrollments)
	}
	s.pending[id] = &pendingEnroll{name: name, byCode: byCode, state: state, expires: now.Add(EnrollTTL)}
	return Begun{ID: id, Options: opts}, nil
}

// FinishPasskey verifies the browser's creation response for enrollment id. The
// new credential then waits: by code, for the code minted for its key
// (Finished.Fingerprint, ConfirmPasskeyCode); otherwise for an existing
// passkey's approval (ApprovePasskey) over a challenge that binds it.
func (s *Service) FinishPasskey(ctx context.Context, id string, response []byte) (Finished, error) {
	if err := refuseRestricted(ctx); err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	if !s.PasskeysOn() {
		return Finished{}, ErrPasskeysOff
	}
	now := s.cfg.Now()
	p, err := s.take(id, now)
	if err != nil || p.cred != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", ErrNoEnrollment)
	}
	user, err := s.user(ctx)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	cred, err := s.cfg.Passkeys.FinishRegistration(user, p.state, response)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	if p.byCode {
		p.cred = &cred
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pending[id] = p
		return Finished{Fingerprint: store.ApproverKeyFingerprint(cred.PublicKey)}, nil
	}
	existing, err := s.passkeys(ctx)
	if err != nil {
		return Finished{}, fmt.Errorf("approval.FinishPasskey: %w", err)
	}
	binding := enrollBinding(id, cred)
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

// ConfirmPasskeyCode enrolls enrollment id's new credential with the code minted
// for its key: the credential is stored and the code consumed in one
// transaction. A code that is not valid for this key leaves the enrollment
// waiting, so a mistyped code can be typed again.
func (s *Service) ConfirmPasskeyCode(ctx context.Context, id, code string) (store.ApproverCredential, error) {
	if err := refuseRestricted(ctx); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ConfirmPasskeyCode: %w", err)
	}
	if !s.PasskeysOn() {
		return store.ApproverCredential{}, ErrPasskeysOff
	}
	now := s.cfg.Now()
	p, err := s.take(id, now)
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ConfirmPasskeyCode: %w", ErrNoEnrollment)
	}
	if !p.byCode || p.cred == nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ConfirmPasskeyCode: %w", ErrNoEnrollment)
	}
	stored, err := store.EnrollApprover(ctx, s.cfg.DB, FromPasskey(*p.cred, p.name), strings.TrimSpace(code), "", now)
	if err != nil {
		if errors.Is(err, store.ErrEnrollCode) || errors.Is(err, store.ErrEnrollProof) {
			s.mu.Lock()
			s.pending[id] = p
			s.mu.Unlock()
		}
		return store.ApproverCredential{}, fmt.Errorf("approval.ConfirmPasskeyCode: %w", err)
	}
	return stored, nil
}

// ApprovePasskey verifies an existing passkey's approval of enrollment id's new
// credential -- an assertion over the challenge that binds it -- records the
// approving credential's use, and stores the new one as added by it.
func (s *Service) ApprovePasskey(ctx context.Context, id string, response []byte) (store.ApproverCredential, error) {
	if err := refuseRestricted(ctx); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", err)
	}
	if !s.PasskeysOn() {
		return store.ApproverCredential{}, ErrPasskeysOff
	}
	now := s.cfg.Now()
	p, err := s.take(id, now)
	if err != nil || p.cred == nil || p.byCode {
		return store.ApproverCredential{}, fmt.Errorf("approval.ApprovePasskey: %w", ErrNoEnrollment)
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
	stored, err := store.EnrollApprover(ctx, s.cfg.DB, FromPasskey(*p.cred, p.name), "", approver.ID, now)
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

// enrollBinding is the 32-byte challenge an existing passkey signs to approve a
// new credential: it names this enrollment and the new credential's id and
// public key, so the approval cannot be lifted onto another enrollment or
// another key.
func enrollBinding(id string, c passkey.Credential) []byte {
	h := sha256.New()
	h.Write([]byte("seamless approver enrollment v1\x00"))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(c.ID)
	h.Write([]byte{0})
	h.Write(c.PublicKey)
	return h.Sum(nil)
}

// TouchIDEnrollMessage is what the Touch ID helper signs with its new key to
// enroll it: proof that the code's holder holds the key. The code is folded in
// as its hash, never in the clear.
func TouchIDEnrollMessage(code string, publicKey []byte) []byte {
	sum := sha256.Sum256([]byte(store.NormalizeEnrollCode(code)))
	msg := []byte("seamless touchid enrollment v1\x00")
	msg = append(msg, sum[:]...)
	return append(msg, publicKey...)
}

// EnrollTouchID stores a Touch ID key: publicKey is the raw uncompressed P-256
// point, and signature an ASN.1 ECDSA signature by that key over
// TouchIDEnrollMessage(code, publicKey). The code must be a live touchid code;
// it is consumed with the insert.
func (s *Service) EnrollTouchID(ctx context.Context, name, code string, publicKey, signature []byte) (store.ApproverCredential, error) {
	stored, _, err := s.enrollTouchID(ctx, "approval.EnrollTouchID", name, code, publicKey, signature, nil)
	return stored, err
}

// TouchIDReplaceMessage is what an older Touch ID key signs so that the
// enrollment of the new key retires it: the helper's proof that it holds the
// older key, and so that both keys belong to this Mac (a Secure Enclave key
// never leaves the Mac that made it, and only the signed helper reaches it).
// It binds the enrollment's code (as its hash), the new key, and the id of the
// credential retired.
func TouchIDReplaceMessage(code string, publicKey []byte, retiredID string) []byte {
	sum := sha256.Sum256([]byte(store.NormalizeEnrollCode(code)))
	msg := []byte("seamless touchid replacement v1\x00")
	msg = append(msg, sum[:]...)
	msg = append(msg, publicKey...)
	return append(msg, retiredID...)
}

// TouchIDReplacement is an enrolled Touch ID credential that a new key
// replaces: its id, and its own key's ASN.1 ECDSA signature over
// TouchIDReplaceMessage.
type TouchIDReplacement struct {
	ID        string
	Signature []byte
}

// EnrollTouchIDReplacing is EnrollTouchID for a key that replaces this Mac's
// earlier ones. Each replacement whose own key signed its
// TouchIDReplaceMessage is retired in the enrollment's transaction, once the
// new key is stored; one that is not an enrolled Touch ID key, or whose
// signature does not verify, is left alone and the enrollment goes on, so a
// key retires only itself: never a passkey, never another Mac's key. It returns
// the stored key and the ids it retired.
func (s *Service) EnrollTouchIDReplacing(ctx context.Context, name, code string, publicKey, signature []byte,
	replaces []TouchIDReplacement) (store.ApproverCredential, []string, error) {
	return s.enrollTouchID(ctx, "approval.EnrollTouchIDReplacing", name, code, publicKey, signature, replaces)
}

func (s *Service) enrollTouchID(ctx context.Context, op, name, code string, publicKey, signature []byte,
	replaces []TouchIDReplacement) (store.ApproverCredential, []string, error) {
	if err := refuseRestricted(ctx); err != nil {
		return store.ApproverCredential{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := VerifyTouchID(publicKey, TouchIDEnrollMessage(code, publicKey), signature); err != nil {
		return store.ApproverCredential{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	var retire []store.RetiredApprover
	for _, r := range replaces {
		old, err := store.ApproverByID(ctx, s.cfg.DB, r.ID)
		switch {
		case errors.Is(err, store.ErrApproverNotFound):
			continue
		case err != nil:
			return store.ApproverCredential{}, nil, fmt.Errorf("%s: %w", op, err)
		case old.Kind != store.ApproverTouchID:
			continue
		}
		if VerifyTouchID(old.PublicKey, TouchIDReplaceMessage(code, publicKey, old.ID), r.Signature) != nil {
			s.cfg.Logger.Debug("approval: a Touch ID enrollment's request to retire a key does not verify; the key stays", "credential", old.ID)
			continue
		}
		retire = append(retire, store.RetiredApprover{ID: old.ID, PublicKey: old.PublicKey})
	}
	keyID := sha256.Sum256(publicKey)
	cred := store.ApproverCredential{
		Kind: store.ApproverTouchID, CredentialID: keyID[:], PublicKey: publicKey, Alg: passkey.AlgES256, Name: name,
	}
	stored, retired, err := store.EnrollTouchIDRetiring(ctx, s.cfg.DB, cred, code, s.cfg.Now(), retire)
	if err != nil {
		return store.ApproverCredential{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, id := range retired {
		s.cfg.Logger.Info("approval: a Touch ID enrollment retired the key it replaces", "credential", id, "replaced_by", stored.ID)
	}
	return stored, retired, nil
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
