package approval

// Console locks: the owner turns a project's lock on, or takes it off, from the
// browser. Each change is one passkey ceremony, because the bearer key is not
// proof of the owner (anything that can read it passes): the assertion signs a
// challenge naming the project and the direction, so an approval to lock can
// never be replayed to remove a lock, or be lifted onto another project.
//
// The console removes only the locks it set. A project peer_auth.force_locked
// lists stays locked until the file changes.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// LockAction says which way a console lock change goes.
type LockAction string

const (
	// LockActionAdd turns a lock on.
	LockActionAdd LockAction = "lock"
	// LockActionRemove takes a console lock off.
	LockActionRemove LockAction = "remove"
)

// EventConsoleLockChanged records a lock turned on or off from the console.
const EventConsoleLockChanged core.EventKind = "access.console_lock_changed"

// lockChangeTTL is how long a begun ceremony waits for its assertion.
const lockChangeTTL = ConsoleUnlockTTL

var (
	// ErrForceLocked is a console change to a lock the config file holds.
	ErrForceLocked = errors.New("this project is force locked in the daemon's config file: only the file can change it")
	// ErrNoLockChange is a finish that names no ceremony in progress for this
	// project and direction: expired, already used, or never begun.
	ErrNoLockChange = errors.New("no lock change is in progress for this project")
	// ErrInvalidLockAction is an action other than lock or remove.
	ErrInvalidLockAction = errors.New("invalid lock action: valid values are lock, remove")
	// ErrTooManyLockChanges is a begin refused because maxPending wait already.
	ErrTooManyLockChanges = errors.New("too many lock changes are in progress")
)

// ParseLockAction reads a lock action from a request.
func ParseLockAction(s string) (LockAction, error) {
	switch LockAction(s) {
	case LockActionAdd, LockActionRemove:
		return LockAction(s), nil
	}
	return "", ErrInvalidLockAction
}

// pendingLockChange is one ceremony between its two requests.
type pendingLockChange struct {
	project   string
	action    LockAction
	challenge []byte
	expires   time.Time
}

// lockChangeChallenge is the 32-byte challenge a passkey signs to approve a
// change: it names this ceremony, the project and the direction.
func lockChangeChallenge(id, project string, action LockAction, nonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("seamless console lock change v1\x00"))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write([]byte(project))
	h.Write([]byte{0})
	h.Write([]byte(action))
	h.Write([]byte{0})
	h.Write(nonce)
	return h.Sum(nil)
}

// BeginLockChange starts the passkey ceremony for one lock change, returning its
// id and the navigator.credentials.get options. It refuses a change that could
// not be applied, before the owner is asked to touch anything.
func (s *Service) BeginLockChange(ctx context.Context, project string, action LockAction) (Begun, error) {
	const op = "approval.BeginLockChange"
	if err := refuseRestricted(ctx); err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	if err := s.lockChangeAllowed(ctx, project, action); err != nil {
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
	challenge := lockChangeChallenge(id, project, action, nonce)
	opts, err := s.cfg.Passkeys.BeginAssertion(challenge, creds)
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(now)
	if len(s.lockChanges) >= maxPending {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrTooManyLockChanges)
	}
	s.lockChanges[id] = &pendingLockChange{project: project, action: action, challenge: challenge, expires: now.Add(lockChangeTTL)}
	return Begun{ID: id, Options: opts}, nil
}

// lockChangeAllowed refuses a change the current state rules out. The store
// judges a lock's fence again inside its write.
func (s *Service) lockChangeAllowed(ctx context.Context, project string, action LockAction) error {
	if slices.Contains(s.cfg.ForceLocked, project) {
		return ErrForceLocked
	}
	if _, err := ParseLockAction(string(action)); err != nil {
		return err
	}
	locked, err := store.HasConsoleLock(ctx, s.cfg.DB, project)
	if err != nil {
		return err
	}
	switch {
	case action == LockActionAdd && locked:
		return store.ErrConsoleLockExists
	case action == LockActionRemove && !locked:
		return store.ErrConsoleLockNotFound
	case action == LockActionRemove:
		return nil
	}
	isolation, err := store.IsolationOf(ctx, s.cfg.DB, project)
	if err != nil {
		return err
	}
	if !isolation.FencesOutbound() {
		return store.ErrConsoleLockUnfenced
	}
	return nil
}

// FinishLockChange verifies the assertion for ceremony id, which must have been
// begun for this project and direction, records the passkey's use and applies
// the change. A ceremony is single use: a failed assertion ends it too.
func (s *Service) FinishLockChange(ctx context.Context, id, project string, action LockAction, response []byte) (store.ApproverCredential, error) {
	const op = "approval.FinishLockChange"
	if err := refuseRestricted(ctx); err != nil {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	now := s.cfg.Now()
	s.mu.Lock()
	s.gc(now)
	p, ok := s.lockChanges[id]
	if ok {
		delete(s.lockChanges, id)
	}
	s.mu.Unlock()
	if !ok || p.project != project || p.action != action {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrNoLockChange)
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
	if action == LockActionAdd {
		err = store.AddConsoleLock(ctx, s.cfg.DB, project, row.ID, now)
	} else {
		err = store.RemoveConsoleLock(ctx, s.cfg.DB, project)
	}
	if err != nil {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	s.record(ctx, core.Event{Kind: EventConsoleLockChanged, ItemID: row.ID, ProjectSlug: project,
		Payload: map[string]any{"proof": ProofPasskey, "action": string(action)}})
	return row, nil
}
