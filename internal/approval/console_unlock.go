package approval

// Console unlocks (step 7b): the owner opens a locked project in one browser's
// console for a limited time. The browser always begins it, and the row is
// bound to that browser's seamless_unlock cookie from the start. Two proofs
// make it live:
//
//   - a passkey: an assertion over a fresh challenge that names the project and
//     the browser (store.ConsoleUnlockChallenge; BeginConsoleUnlock,
//     FinishConsoleUnlock);
//   - the terminal: the page shows a pairing code (BeginTerminalUnlock), and
//     `seamlessd console-open --unlock <project> --code <code>`, run as the
//     daemon's own account, approves it (ApproveTerminalUnlock). The code only
//     names the row, so whoever reads it off the owner's terminal or screen
//     learns nothing that opens another browser.
//
// The result is a store.ConsoleUnlock row behind that cookie. It is never a
// lease: no agent process, the owner's included, reads a locked project
// through it.

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

// ConsoleTerminalUnlockTTL is how long an unlock a browser began for the
// terminal waits for its code to be approved: time to open a terminal, run
// the command and answer sudo.
const ConsoleTerminalUnlockTTL = 10 * time.Minute

// TerminalUnlock is an unlock a browser began for the terminal to approve:
// its id, which the page asks after, and the pairing code it shows.
type TerminalUnlock struct {
	ID           string    `json:"id"`
	Code         string    `json:"code"` // XXXX-XXXX
	PendingUntil time.Time `json:"pending_until"`
}

// EventConsoleUnlocked records a console unlock going live (item_id: the
// unlock; payload: proof, seconds).
const EventConsoleUnlocked core.EventKind = "access.console_unlocked"

// ErrNoPasskey is a passkey console unlock on a daemon with no passkey
// enrolled: the terminal path (BeginTerminalUnlock) still works.
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

// BeginTerminalUnlock starts an unlock of project in the browser whose
// seamless_unlock cookie hashes to tokenHash, for the daemon's account to
// approve in a terminal within ConsoleTerminalUnlockTTL. It needs no passkey,
// so a holder of the console's key can begin one too: terminal unlocks are
// capped on their own, apart from passkey ceremonies, and filling their cap
// never blocks a passkey unlock. The caller has checked that project is
// locked.
func (s *Service) BeginTerminalUnlock(ctx context.Context, project, tokenHash string) (TerminalUnlock, error) {
	const op = "approval.BeginTerminalUnlock"
	if err := refuseRestricted(ctx); err != nil {
		return TerminalUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	u, code, err := store.BeginTerminalUnlock(ctx, s.cfg.DB, project, tokenHash, s.ConsoleLeaseLength(), ConsoleTerminalUnlockTTL, s.cfg.Now())
	if err != nil {
		return TerminalUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	return TerminalUnlock{ID: u.ID, Code: code, PendingUntil: u.PendingUntil}, nil
}

// ApproveTerminalUnlock is the terminal's half, `seamlessd console-open
// --unlock --code`: it makes live, for lease, the unlock of project whose page
// shows code, in the browser that began it. The caller runs as the daemon's
// own account (its proof) and has checked that project is locked. lease is at
// most ConsoleLeaseLength.
func (s *Service) ApproveTerminalUnlock(ctx context.Context, project, code string, lease time.Duration) (store.ConsoleUnlock, error) {
	const op = "approval.ApproveTerminalUnlock"
	if err := refuseRestricted(ctx); err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if lease > s.ConsoleLeaseLength() {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w: a console unlock lasts at most %s", op, store.ErrInvalidConsoleUnlock, s.ConsoleLeaseLength())
	}
	u, err := store.ApproveTerminalUnlock(ctx, s.cfg.DB, project, code, lease, s.cfg.Now())
	if err != nil {
		return store.ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	s.record(ctx, core.Event{Kind: EventConsoleUnlocked, ItemID: u.ID, ProjectSlug: u.Project,
		Payload: map[string]any{"proof": ProofDaemon, "seconds": int64(u.Lease / time.Second)}})
	return u, nil
}
