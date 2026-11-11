package approval

// Console restores: under peer_auth with a locked project, the owner restores
// a backup from the console only with a passkey ceremony, because the bearer
// key is not proof of the owner. The assertion signs a challenge naming the
// backup's SHA-256 and the mode, so an approval to bring back what is missing
// can never be replayed to replace everything, or be lifted onto another
// backup file. The digest is computed by the server from the file
// (backup.Service.Digest), never taken from a client; the console computes it
// again at finish and passes what it found, which must equal the ceremony's.
//
// The ceremony only proves the owner asked. It stages nothing: the caller
// stages the restore with the credential FinishRestore returns, and the
// backup package records it with the backup.restored event.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// RestoreMode says what a restore does, in the backup package's words
// (backup.ModeReplace, backup.ModeMerge).
type RestoreMode string

const (
	// RestoreReplace puts the backup in place of everything.
	RestoreReplace RestoreMode = "replace"
	// RestoreMerge adds what the backup has and this install does not.
	RestoreMerge RestoreMode = "merge"
)

// restoreTTL is how long a begun restore ceremony waits for its assertion.
const restoreTTL = ConsoleUnlockTTL

var (
	// ErrNoRestore is a finish that names no ceremony in progress for this
	// backup and mode: expired, already used, or never begun.
	ErrNoRestore = errors.New("no restore approval is in progress for this backup")
	// ErrInvalidRestoreMode is a mode other than replace or merge.
	ErrInvalidRestoreMode = errors.New("invalid restore mode: valid values are replace, merge")
	// ErrInvalidRestoreDigest is a digest that is not a lowercase hex SHA-256.
	ErrInvalidRestoreDigest = errors.New("invalid backup digest: want a lowercase hex SHA-256")
	// ErrTooManyRestores is a begin refused because maxPending wait already.
	ErrTooManyRestores = errors.New("too many restore approvals are in progress")
)

// ParseRestoreMode reads a restore mode from a request.
func ParseRestoreMode(s string) (RestoreMode, error) {
	switch RestoreMode(s) {
	case RestoreReplace, RestoreMerge:
		return RestoreMode(s), nil
	}
	return "", ErrInvalidRestoreMode
}

// validDigest reports whether d is a lowercase hex SHA-256, as
// archive.Verified.SHA256 and backup.Service.Digest write one.
func validDigest(d string) bool {
	if len(d) != 2*sha256.Size {
		return false
	}
	for i := range len(d) {
		c := d[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// restoreAllowed refuses a ceremony over a digest or mode no restore could use.
func restoreAllowed(digest string, mode RestoreMode) error {
	if _, err := ParseRestoreMode(string(mode)); err != nil {
		return err
	}
	if !validDigest(digest) {
		return ErrInvalidRestoreDigest
	}
	return nil
}

// pendingRestore is one restore ceremony between its two requests.
type pendingRestore struct {
	digest    string
	mode      RestoreMode
	challenge []byte
	expires   time.Time
}

// restoreChallenge is the 32-byte challenge a passkey signs to approve a
// restore: it names this ceremony, the backup's digest and the mode.
func restoreChallenge(id, digest string, mode RestoreMode, nonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("seamless console restore v1\x00"))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write([]byte(digest))
	h.Write([]byte{0})
	h.Write([]byte(mode))
	h.Write([]byte{0})
	h.Write(nonce)
	return h.Sum(nil)
}

// BeginRestore starts the passkey ceremony that approves restoring the backup
// whose SHA-256 is digest, in mode, returning its id and the
// navigator.credentials.get options. The caller computed digest from the file
// and has checked the backup can be restored in mode.
func (s *Service) BeginRestore(ctx context.Context, digest string, mode RestoreMode) (Begun, error) {
	const op = "approval.BeginRestore"
	if err := refuseRestricted(ctx); err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	if err := restoreAllowed(digest, mode); err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	creds, err := s.passkeys(ctx)
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(creds) == 0 {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrNoPasskey)
	}
	id, err := core.NewID()
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	challenge := restoreChallenge(id, digest, mode, nonce)
	opts, err := s.cfg.Passkeys.BeginAssertion(challenge, creds)
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(now)
	if len(s.restores) >= maxPending {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrTooManyRestores)
	}
	s.restores[id] = &pendingRestore{digest: digest, mode: mode, challenge: challenge, expires: now.Add(restoreTTL)}
	return Begun{ID: id, Options: opts}, nil
}

// FinishRestore verifies the assertion for ceremony id, which must have been
// begun for this digest and mode, and records the passkey's use, returning the
// passkey that approved it: the caller stages the restore naming it. digest
// is the one the caller computed again from the file, so a backup that
// changed since the ceremony began is refused. A ceremony is single use: a
// failed assertion ends it too.
func (s *Service) FinishRestore(ctx context.Context, id, digest string, mode RestoreMode, response []byte) (store.ApproverCredential, error) {
	const op = "approval.FinishRestore"
	if err := refuseRestricted(ctx); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	now := s.cfg.Now()
	s.mu.Lock()
	s.gc(now)
	p, ok := s.restores[id]
	if ok {
		delete(s.restores, id)
	}
	s.mu.Unlock()
	if !ok || p.digest != digest || p.mode != mode {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrNoRestore)
	}
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	allowed := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		allowed[i] = ToPasskey(r)
	}
	used, err := s.cfg.Passkeys.FinishAssertion(p.challenge, allowed, response)
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	row, ok := rowFor(rows, used.ID)
	if !ok {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, passkey.ErrUnknownCredential)
	}
	if err := store.RecordApproverUse(ctx, s.cfg.DB, row.ID, row.SignCount, used.SignCount, used.BackupState, now); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	return row, nil
}
