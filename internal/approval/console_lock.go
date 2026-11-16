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
const EventConsoleLockChanged = core.EventConsoleLockChanged

// EventConsoleLockFailed records a ceremony that began and then failed: the
// finish that ends it names the wrong project or direction, its passkey does
// not verify, or the change cannot be applied. A finish naming no ceremony
// records nothing, since its id is all it has and that is the caller's text.
const EventConsoleLockFailed = core.EventConsoleLockFailed

// The reasons a failed lock change records.
const (
	LockFailMismatch   = "mismatch"   // the finish named another project or direction than its ceremony's
	LockFailAssertion  = "assertion"  // the passkey's answer did not verify
	LockFailCredential = "credential" // the passkey that answered is not enrolled
	LockFailCounter    = "counter"    // the passkey's signature counter did not increase, as a cloned key's does not (passkey.ErrSignCount)
	LockFailRace       = "race"       // another use of the same passkey was recorded first (store.ErrApproverCounter)
	LockFailState      = "state"      // the project changed between begin and finish
	LockFailError      = "error"      // the daemon could not read or write what it needed
)

// lockChangeTTL is how long a begun ceremony waits for its assertion.
const lockChangeTTL = ConsoleUnlockTTL

// maxPendingPerProject caps the ceremonies one project and direction keep
// waiting at once. A begin never waits for room: past this cap the oldest of
// its project and direction goes, and past maxPending in all the oldest of the
// fullest project and direction goes. So a caller looping on begin crowds out
// only the project and direction it loops on, and only while it keeps
// looping: no burst can hold every lock change off until its ceremonies
// expire.
const maxPendingPerProject = 4

var (
	// ErrForceLocked is a console change to a lock the config file holds.
	ErrForceLocked = errors.New("this project is force locked in the daemon's config file: only the file can change it")
	// ErrNoLockChange is a finish that names no ceremony in progress for this
	// project and direction: expired, already used, or never begun.
	ErrNoLockChange = errors.New("no lock change is in progress for this project")
	// ErrInvalidLockAction is an action other than lock or remove.
	ErrInvalidLockAction = errors.New("invalid lock action: valid values are lock, remove")
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
	s.makeRoomForLockChange(project, action)
	s.lockChanges[id] = &pendingLockChange{project: project, action: action, challenge: challenge, expires: now.Add(lockChangeTTL)}
	return Begun{ID: id, Options: opts}, nil
}

// makeRoomForLockChange drops, under s.mu, the ceremony a new one for project
// and action displaces (maxPendingPerProject): the oldest of its own project
// and direction when they hold the cap, else, when maxPending wait in all, the
// oldest of the fullest project and direction, the one with the oldest
// ceremony on a tie.
func (s *Service) makeRoomForLockChange(project string, action LockAction) {
	type bucket struct {
		project string
		action  LockAction
	}
	older := func(a, b string) bool {
		pa, pb := s.lockChanges[a], s.lockChanges[b]
		return pa.expires.Before(pb.expires) || (pa.expires.Equal(pb.expires) && a < b)
	}
	sizes := map[bucket]int{}
	oldest := map[bucket]string{}
	for id, p := range s.lockChanges {
		b := bucket{p.project, p.action}
		sizes[b]++
		if o, ok := oldest[b]; !ok || older(id, o) {
			oldest[b] = id
		}
	}
	own := bucket{project, action}
	if sizes[own] >= maxPendingPerProject {
		delete(s.lockChanges, oldest[own])
		return
	}
	if len(s.lockChanges) < maxPending {
		return
	}
	var victim bucket
	found := false
	for b, n := range sizes {
		if !found || n > sizes[victim] || (n == sizes[victim] && older(oldest[b], oldest[victim])) {
			victim, found = b, true
		}
	}
	delete(s.lockChanges, oldest[victim])
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
	if !ok {
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrNoLockChange)
	}
	failed := func(credential, reason string, err error) (store.ApproverCredential, error) {
		s.record(ctx, core.Event{Kind: EventConsoleLockFailed, ItemID: credential, ProjectSlug: p.project,
			Payload: map[string]any{"action": string(p.action), "reason": reason}})
		return store.ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	if p.project != project || p.action != action {
		// A ceremony's approval lifted onto another project or direction: the
		// challenge names its own, so the passkey could not approve this one.
		return failed("", LockFailMismatch, ErrNoLockChange)
	}
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return failed("", LockFailError, err)
	}
	allowed := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		allowed[i] = ToPasskey(r)
	}
	used, err := s.cfg.Passkeys.FinishAssertion(p.challenge, allowed, response)
	if err != nil {
		switch {
		case errors.Is(err, passkey.ErrUnknownCredential):
			return failed("", LockFailCredential, err)
		case errors.Is(err, passkey.ErrSignCount):
			return failed("", LockFailCounter, err)
		}
		return failed("", LockFailAssertion, err)
	}
	row, ok := rowFor(rows, used.ID)
	if !ok {
		return failed("", LockFailCredential, passkey.ErrUnknownCredential)
	}
	if err := store.RecordApproverUse(ctx, s.cfg.DB, row.ID, row.SignCount, used.SignCount, used.BackupState, now); err != nil {
		if errors.Is(err, store.ErrApproverCounter) {
			return failed(row.ID, LockFailRace, err)
		}
		return failed(row.ID, LockFailError, err)
	}
	var ended store.EndedAccess
	if action == LockActionAdd {
		ended, err = store.AddConsoleLockEndingAccess(ctx, s.cfg.DB, project, row.ID, now)
	} else {
		err = store.RemoveConsoleLock(ctx, s.cfg.DB, project)
	}
	if err != nil {
		reason := LockFailError
		if errors.Is(err, store.ErrConsoleLockExists) || errors.Is(err, store.ErrConsoleLockNotFound) ||
			errors.Is(err, store.ErrConsoleLockUnfenced) || errors.Is(err, store.ErrProjectNotFound) {
			reason = LockFailState
		}
		return failed(row.ID, reason, err)
	}
	s.record(ctx, core.Event{Kind: EventConsoleLockChanged, ItemID: row.ID, ProjectSlug: project,
		Payload: ended.AddTo(map[string]any{"proof": ProofPasskey, "action": string(action), "by": "console"})})
	if ended.Leases+ended.Grants > 0 {
		// Said the way every other early end of agents' access is said, so the
		// access page and the feed see it without knowing about locks.
		s.record(ctx, core.Event{Kind: core.EventAccessRevoked, ProjectSlug: project,
			Payload: map[string]any{"reason": store.LeaseRevokedLocked, "leases": ended.Leases, "grants": ended.Grants}})
	}
	return row, nil
}
