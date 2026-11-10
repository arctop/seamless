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

// AddConsoleLock records a lock on project, proven by credential. The project
// must exist and carry an isolation fence, and the console must not lock it
// already.
func AddConsoleLock(ctx context.Context, db *sql.DB, project, credential string, now time.Time) error {
	const op = "store.AddConsoleLock"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	var isolation string
	err = tx.QueryRowContext(ctx, `SELECT isolation FROM projects WHERE slug = ?`, project).Scan(&isolation)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w: %q", op, ErrProjectNotFound, project)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !core.Isolation(isolation).FencesOutbound() {
		return fmt.Errorf("%s: %w", op, ErrConsoleLockUnfenced)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_locks WHERE project_slug = ?`, project).Scan(&n); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n > 0 {
		return fmt.Errorf("%s: %w", op, ErrConsoleLockExists)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO console_locks (project_slug, credential, locked_at) VALUES (?, ?, ?)`,
		project, credential, core.FormatTime(now)); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
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
