package approval

// Console unlocks (step 7b): the owner opens a locked project in one browser's
// console for a limited time, with a passkey. The proof is an assertion over a
// fresh challenge that names the project and the browser
// (store.ConsoleUnlockChallenge); the result is a store.ConsoleUnlock row
// behind the browser's seamless_unlock cookie. It is never a lease: no agent
// process, the owner's included, reads a locked project through it.
//
// The other way in -- `seamlessd console-open --unlock`, run as the daemon's
// own account -- writes the row directly (store.CreateConsoleUnlockLink) and
// needs nothing here.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// ConsoleLease is how long a console unlock lasts (the owner's answer of
// 2026-11-06): three hours, capped by peer_auth.max_lease.
const ConsoleLease = 3 * time.Hour

// ConsoleUnlockTTL is how long a started console unlock waits for its browser.
const ConsoleUnlockTTL = 5 * time.Minute

// EventConsoleUnlocked records a console unlock going live (item_id: the
// unlock; payload: proof, seconds).
const EventConsoleUnlocked core.EventKind = "access.console_unlocked"

// ErrNoPasskey is a passkey console unlock on a daemon with no passkey
// enrolled: the terminal path (console-open --unlock) still works.
var ErrNoPasskey = errors.New("no passkey is enrolled")

// ConsoleLeaseLength is how long a console unlock stays live: ConsoleLease,
// or peer_auth.max_lease when that is shorter.
func (s *Service) ConsoleLeaseLength() time.Duration {
	return min(ConsoleLease, s.leaseCap())
}

// BeginConsoleUnlock starts a passkey ceremony that unlocks project in the
// browser whose seamless_unlock cookie hashes to tokenHash. It returns the
// ceremony's id and the navigator.credentials.get options over its challenge,
// with every enrolled passkey allowed. The caller has checked that project is
// locked.
func (s *Service) BeginConsoleUnlock(ctx context.Context, project, tokenHash string) (Begun, error) {
	const op = "approval.BeginConsoleUnlock"
	if err := refuseRestricted(ctx); err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	creds, err := s.passkeys(ctx)
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(creds) == 0 {
		return Begun{}, fmt.Errorf("%s: %w", op, ErrNoPasskey)
	}
	u, err := store.BeginConsoleUnlock(ctx, s.cfg.DB, project, tokenHash, s.ConsoleLeaseLength(), ConsoleUnlockTTL, s.cfg.Now())
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	opts, err := s.cfg.Passkeys.BeginAssertion(u.Challenge, creds)
	if err != nil {
		return Begun{}, fmt.Errorf("%s: %w", op, err)
	}
	return Begun{ID: u.ID, Options: opts}, nil
}

// FinishConsoleUnlock verifies the browser's assertion for ceremony id -- begun
// by the same browser, for the same project, and still waiting -- records the
// passkey's use (a compare-and-set on its counter), and makes the unlock live.
func (s *Service) FinishConsoleUnlock(ctx context.Context, id, project, tokenHash string, response []byte) (store.ConsoleUnlock, error) {
	const op = "approval.FinishConsoleUnlock"
	if err := refuseRestricted(ctx); err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if !s.PasskeysOn() {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrPasskeysOff)
	}
	now := s.cfg.Now()
	pending, err := store.PendingConsoleUnlock(ctx, s.cfg.DB, id, project, tokenHash, now)
	if err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	allowed := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		allowed[i] = ToPasskey(r)
	}
	used, err := s.cfg.Passkeys.FinishAssertion(pending.Challenge, allowed, response)
	if err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	row, ok := rowFor(rows, used.ID)
	if !ok {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, passkey.ErrUnknownCredential)
	}
	if err := store.RecordApproverUse(ctx, s.cfg.DB, row.ID, row.SignCount, used.SignCount, used.BackupState, now); err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	granted, err := store.GrantConsoleUnlock(ctx, s.cfg.DB, pending.ID, tokenHash, row.ID, now)
	if err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	s.record(ctx, core.Event{Kind: EventConsoleUnlocked, ItemID: granted.ID, ProjectSlug: granted.Project,
		Payload: map[string]any{"proof": ProofPasskey, "seconds": int64(granted.Lease / time.Second)}})
	return granted, nil
}
