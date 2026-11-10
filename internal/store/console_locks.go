package store

// Console locks (migration 037): locks the owner turned on from the browser.
// The approve step that proves each change lives in internal/approval; this file
// keeps the rows and the two rules the rows must never break, both judged inside
// the transaction that writes: a console lock needs an isolation fence, and a
// fence cannot come down under a lock.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
)

var (
	// ErrConsoleLockNotFound is a removal of a lock the console did not set.
	ErrConsoleLockNotFound = errors.New("this project has no lock set from the console")
	// ErrConsoleLockExists is a lock on a project the console already locks.
	ErrConsoleLockExists = errors.New("this project is already locked from the console")
	// ErrConsoleLockUnfenced is a lock on a project with no isolation fence. The
	// fence keeps a locked project out of the family, parent and cross-project
	// gardener surfaces the lock does not reach on its own.
	ErrConsoleLockUnfenced = errors.New("a locked project must be confidential or sealed")
	// ErrIsolationLocked is a locked project's fence asked to come down to open.
	ErrIsolationLocked = errors.New("a locked project must stay confidential or sealed")
)

// ConsoleLock is one lock the console set.
type ConsoleLock struct {
	Project    string    `json:"project"`
	Credential string    `json:"credential"`
	LockedAt   time.Time `json:"locked_at"`
}

// ConsoleLocks returns every lock the console set, by project.
func ConsoleLocks(ctx context.Context, db *sql.DB) ([]ConsoleLock, error) {
	const op = "store.ConsoleLocks"
	rows, err := db.QueryContext(ctx, `SELECT project_slug, credential, locked_at FROM console_locks ORDER BY project_slug`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConsoleLock
	for rows.Next() {
		var l ConsoleLock
		var at string
		if err := rows.Scan(&l.Project, &l.Credential, &at); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if l.LockedAt, err = core.ParseTime(at); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// ConsoleLockedSlugs returns the projects the console locks, sorted.
func ConsoleLockedSlugs(ctx context.Context, db *sql.DB) ([]string, error) {
	locks, err := ConsoleLocks(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(locks))
	for i, l := range locks {
		out[i] = l.Project
	}
	return out, nil
}

// HasConsoleLock reports whether the console locks project.
func HasConsoleLock(ctx context.Context, db *sql.DB, project string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_locks WHERE project_slug = ?`, project).Scan(&n); err != nil {
		return false, fmt.Errorf("store.HasConsoleLock: %w", err)
	}
	return n > 0, nil
}

// AddConsoleLock records a lock on project, proven by credential, and ends the
// access the project still had (AddConsoleLockEndingAccess).
func AddConsoleLock(ctx context.Context, db *sql.DB, project, credential string, now time.Time) error {
	_, err := AddConsoleLockEndingAccess(ctx, db, project, credential, now)
	return err
}

// EndedAccess counts the permissions on one project that a new lock ended.
type EndedAccess struct {
	Leases, Unlocks, Grants int64
}

// AddTo sets each nonzero count in an event payload as leases, unlocks and
// grants, and returns the payload.
func (e EndedAccess) AddTo(payload map[string]any) map[string]any {
	for k, n := range map[string]int64{"leases": e.Leases, "unlocks": e.Unlocks, "grants": e.Grants} {
		if n > 0 {
			payload[k] = n
		}
	}
	return payload
}

// AddConsoleLockEndingAccess records a lock on project, proven by credential.
// The project must exist and carry an isolation fence, and the console must not
// lock it already.
//
// In the same transaction it ends every permission on the project that is
// still live: agents' leases, read grants, and browsers' console unlocks, live
// or pending. Each one dates from before the lock came off, since an unlocked
// project grants none of these, and a lock turned back on means every agent and
// browser asks again. The lock and the endings commit together or not at all,
// so a lock is never recorded with an old approval still open behind it.
func AddConsoleLockEndingAccess(ctx context.Context, db *sql.DB, project, credential string, now time.Time) (EndedAccess, error) {
	const op = "store.AddConsoleLock"
	var ended EndedAccess
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ended, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	var isolation string
	err = tx.QueryRowContext(ctx, `SELECT isolation FROM projects WHERE slug = ?`, project).Scan(&isolation)
	if errors.Is(err, sql.ErrNoRows) {
		return ended, fmt.Errorf("%s: %w: %q", op, ErrProjectNotFound, project)
	}
	if err != nil {
		return ended, fmt.Errorf("%s: %w", op, err)
	}
	if !core.Isolation(isolation).FencesOutbound() {
		return ended, fmt.Errorf("%s: %w", op, ErrConsoleLockUnfenced)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_locks WHERE project_slug = ?`, project).Scan(&n); err != nil {
		return ended, fmt.Errorf("%s: %w", op, err)
	}
	if n > 0 {
		return ended, fmt.Errorf("%s: %w", op, ErrConsoleLockExists)
	}
	at := core.FormatTime(now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO console_locks (project_slug, credential, locked_at) VALUES (?, ?, ?)`,
		project, credential, at); err != nil {
		return ended, fmt.Errorf("%s: %w", op, err)
	}
	// A pending unlock's code and challenge are cleared as EndAllConsoleUnlocks
	// clears them, so it can no longer be approved or finished.
	endings := []struct {
		n    *int64
		q    string
		args []any
	}{
		{&ended.Leases, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
			WHERE project_slug = ? AND revoked_at IS NULL AND expires_at > ?`, []any{at, LeaseRevokedLocked, project, at}},
		{&ended.Grants, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
			WHERE project_slug = ? AND revoked_at IS NULL AND expires_at > ?`, []any{at, LeaseRevokedLocked, project, at}},
		{&ended.Unlocks, `UPDATE console_unlocks SET status = ?, ended_at = ?, ended_reason = ?, claim_hash = '', challenge = NULL
			WHERE project_slug = ? AND status IN (?, ?)`,
			[]any{ConsoleUnlockEnded, at, ConsoleUnlockEndLocked, project, ConsoleUnlockLive, ConsoleUnlockPending}},
	}
	for _, e := range endings {
		res, err := tx.ExecContext(ctx, e.q, e.args...)
		if err != nil {
			return EndedAccess{}, fmt.Errorf("%s: end earlier access: %w", op, err)
		}
		if *e.n, err = res.RowsAffected(); err != nil {
			return EndedAccess{}, fmt.Errorf("%s: end earlier access: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return EndedAccess{}, fmt.Errorf("%s: %w", op, err)
	}
	return ended, nil
}

// RemoveConsoleLock deletes the console's lock on project.
func RemoveConsoleLock(ctx context.Context, db *sql.DB, project string) error {
	const op = "store.RemoveConsoleLock"
	res, err := db.ExecContext(ctx, `DELETE FROM console_locks WHERE project_slug = ?`, project)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w: %q", op, ErrConsoleLockNotFound, project)
	}
	return nil
}

// LoosenProjectIsolation is SetProjectIsolation for a fence the owner brings
// down: it refuses to open a project the file force-locks or the console locks,
// since a lock without its fence reaches the family, parent and cross-project
// surfaces the fence kept it out of. The check and the write share a transaction,
// so a lock taken between them cannot slip past.
func LoosenProjectIsolation(ctx context.Context, db *sql.DB, slug string, state core.Isolation, forceLocked []string) error {
	const op = "store.LoosenProjectIsolation"
	if !state.Valid() {
		return fmt.Errorf("%s: invalid isolation %q", op, state)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if !state.FencesOutbound() {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_locks WHERE project_slug = ?`, slug).Scan(&n); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		if n > 0 || slices.Contains(forceLocked, slug) {
			return fmt.Errorf("%s: %w", op, ErrIsolationLocked)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE projects SET isolation = ? WHERE slug = ?`, string(state), slug)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w: %q", op, ErrProjectNotFound, slug)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// ImportedLockCredential is console_locks.credential for a lock an archive
// import set. No passkey signed that lock here: it was already on the project
// in the backup, and the import keeps it.
const ImportedLockCredential = "import"

// PasskeyFact describes one enrolled passkey for a backup's manifest: its label
// and whether it can be used away from the machine that enrolled it. It holds
// no key material.
type PasskeyFact struct {
	Name     string `json:"name"`
	Portable bool   `json:"portable"`
}

// LockFacts is what a database knows about its locks and who can open them.
type LockFacts struct {
	// ConsoleLocked are the projects the console locks, sorted.
	ConsoleLocked []string
	// Passkeys are the enrolled passkeys. A Touch ID key is not one: its key
	// never leaves the Mac that made it.
	Passkeys []PasskeyFact
}

// ReadLockFacts reads a database's LockFacts. A database that predates either
// table has none of what that table holds, which is not an error: an export
// never migrates the database it reads.
func ReadLockFacts(ctx context.Context, db *sql.DB) (LockFacts, error) {
	const op = "store.ReadLockFacts"
	var facts LockFacts
	ok, err := tableExists(ctx, db, "console_locks")
	if err != nil {
		return facts, fmt.Errorf("%s: %w", op, err)
	}
	if ok {
		if facts.ConsoleLocked, err = ConsoleLockedSlugs(ctx, db); err != nil {
			return facts, fmt.Errorf("%s: %w", op, err)
		}
	}
	if ok, err = tableExists(ctx, db, "approver_credentials"); err != nil {
		return facts, fmt.Errorf("%s: %w", op, err)
	}
	if ok {
		creds, err := ApproverCredentials(ctx, db, ApproverPasskey)
		if err != nil {
			return facts, fmt.Errorf("%s: %w", op, err)
		}
		for _, c := range creds {
			facts.Passkeys = append(facts.Passkeys, PasskeyFact{Name: c.Name, Portable: passkeyTravels(c)})
		}
	}
	return facts, nil
}

// passkeyTravels reports whether a passkey can sign away from the machine that
// enrolled it: a synced one, a security key, or a phone. It is what the
// authenticator said at registration, so it can only say "may work": whether
// the owner still holds the authenticator is not something a row can tell.
func passkeyTravels(c ApproverCredential) bool {
	if c.BackupEligible || c.Attachment == "cross-platform" {
		return true
	}
	return slices.ContainsFunc(c.Transports, func(t string) bool {
		return t == "usb" || t == "nfc" || t == "ble" || t == "hybrid"
	})
}
