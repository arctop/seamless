package store

// Console unlocks (peer_auth.locked; migration 033): the owner opens a locked
// project in one browser's console for a limited time.
//
// A browser holds a random seamless_unlock cookie; the database holds only its
// hash, one row per project it opens. A row goes live after a proof: a passkey
// assertion over a challenge bound to the project and to that browser
// (BeginConsoleUnlock, GrantConsoleUnlock), or a one-time link that only the
// daemon's own account can write (CreateConsoleUnlockLink, run by `seamlessd
// console-open --unlock`; ClaimConsoleUnlockLink). The console then reads that
// browser's requests through the handle that leaves the project in
// (store.Views). An unlock is never a lease: nothing here is read by the
// access layer, so no agent process -- the owner's included -- ever reads a
// locked project through one.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// Console unlock statuses.
const (
	ConsoleUnlockPending = "pending"
	ConsoleUnlockLive    = "live"
	ConsoleUnlockEnded   = "ended"
)

// Why a console unlock ended before its expiry.
const (
	// ConsoleUnlockEndLocked is the owner locking it again.
	ConsoleUnlockEndLocked = "locked"
	// ConsoleUnlockEndReset is every unlock ending at once: a restored
	// database, or every approver credential removed.
	ConsoleUnlockEndReset = "reset"
)

// ConsoleUnlockEndReasons is the canonical set of end reasons.
var ConsoleUnlockEndReasons = []string{ConsoleUnlockEndLocked, ConsoleUnlockEndReset}

// ConsoleUnlockByDaemon is the credential of an unlock link: the daemon's own
// account wrote it.
const ConsoleUnlockByDaemon = "daemon-account"

// ConsoleTokenBytes is the entropy of a seamless_unlock cookie and of an
// unlock link's secret.
const ConsoleTokenBytes = 32

// maxPendingConsoleUnlocks caps the passkey ceremonies waiting at once, so a
// caller looping on begin cannot grow the table without bound.
const maxPendingConsoleUnlocks = 32

var (
	// ErrConsoleUnlockNotFound is a ceremony or link that is not there, not
	// pending, not this browser's or this project's, or past its deadline.
	ErrConsoleUnlockNotFound = errors.New("console unlock not found")
	// ErrConsoleUnlockNotLive is an unlock that already ended or expired.
	ErrConsoleUnlockNotLive = errors.New("console unlock is not live")
	// ErrInvalidConsoleUnlock is a malformed argument.
	ErrInvalidConsoleUnlock = errors.New("invalid console unlock")
	// ErrConsoleUnlockBusy is a ceremony begun while too many wait already.
	ErrConsoleUnlockBusy = errors.New("too many console unlocks in progress")
)

// ConsoleUnlock is one row of console_unlocks.
type ConsoleUnlock struct {
	ID           string
	Project      string
	Status       string
	TokenHash    string // the browser's cookie hash ("" for an unclaimed link)
	Challenge    []byte
	Credential   string
	Lease        time.Duration
	CreatedAt    time.Time
	PendingUntil time.Time
	GrantedAt    *time.Time
	ExpiresAt    *time.Time
	EndedAt      *time.Time
	EndedReason  string
}

// NewConsoleToken returns a fresh random secret: a seamless_unlock cookie's
// value or an unlock link's. Only its hash is ever stored.
func NewConsoleToken() (string, error) {
	b := make([]byte, ConsoleTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store.NewConsoleToken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashConsoleToken is what console_unlocks stores of a cookie or a link
// secret: hex SHA-256 under a domain prefix. An empty token has no hash.
func HashConsoleToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("seamless console unlock v1\x00" + token))
	return hex.EncodeToString(sum[:])
}

// validTokenHash reports whether h is a HashConsoleToken result.
func validTokenHash(h string) bool {
	if len(h) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// ConsoleUnlockChallenge is the 32 bytes an owner's passkey signs to unlock
// project in one browser: it names the ceremony, the project and the browser's
// cookie hash, over a fresh nonce. An assertion over it cannot be lifted onto
// another project, another browser, or an access request.
func ConsoleUnlockChallenge(id, project, tokenHash string, nonce []byte) []byte {
	h := sha256.New()
	for _, part := range [][]byte{[]byte("seamless console unlock challenge v1"), []byte(id), []byte(project), []byte(tokenHash), nonce} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

func validConsoleUnlock(project string, lease, ttl time.Duration) error {
	if strings.TrimSpace(project) == "" || project != strings.TrimSpace(project) {
		return fmt.Errorf("%w: a project slug is required", ErrInvalidConsoleUnlock)
	}
	if lease < time.Second || ttl < time.Second {
		return fmt.Errorf("%w: a lease and a deadline last at least a second", ErrInvalidConsoleUnlock)
	}
	return nil
}

const consoleUnlockCols = `id, project_slug, status, token_hash, challenge, credential, lease_seconds, created_at, pending_until,
	granted_at, expires_at, ended_at, ended_reason`

func scanConsoleUnlock(row interface{ Scan(...any) error }) (ConsoleUnlock, error) {
	var (
		u                          ConsoleUnlock
		seconds                    int64
		created, pending           string
		granted, expires, endedTxt sql.NullString
	)
	if err := row.Scan(&u.ID, &u.Project, &u.Status, &u.TokenHash, &u.Challenge, &u.Credential, &seconds, &created, &pending,
		&granted, &expires, &endedTxt, &u.EndedReason); err != nil {
		return ConsoleUnlock{}, err
	}
	u.Lease = time.Duration(seconds) * time.Second
	var err error
	if u.CreatedAt, err = core.ParseTime(created); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("console unlock %s created_at: %w", u.ID, err)
	}
	if u.PendingUntil, err = core.ParseTime(pending); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("console unlock %s pending_until: %w", u.ID, err)
	}
	for _, f := range []struct {
		name string
		src  sql.NullString
		dst  **time.Time
	}{{"granted_at", granted, &u.GrantedAt}, {"expires_at", expires, &u.ExpiresAt}, {"ended_at", endedTxt, &u.EndedAt}} {
		if !f.src.Valid {
			continue
		}
		t, err := core.ParseTime(f.src.String)
		if err != nil {
			return ConsoleUnlock{}, fmt.Errorf("console unlock %s %s: %w", u.ID, f.name, err)
		}
		*f.dst = &t
	}
	return u, nil
}

func consoleUnlockByID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (ConsoleUnlock, error) {
	u, err := scanConsoleUnlock(q.QueryRowContext(ctx, `SELECT `+consoleUnlockCols+` FROM console_unlocks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleUnlock{}, ErrConsoleUnlockNotFound
	}
	return u, err
}

// BeginConsoleUnlock records a passkey ceremony that will unlock project for
// the browser whose cookie hashes to tokenHash: pending for ttl, carrying a
// fresh challenge (ConsoleUnlockChallenge), live for lease once granted.
// Ceremonies past their deadline are purged first, and at most
// maxPendingConsoleUnlocks wait at once.
func BeginConsoleUnlock(ctx context.Context, db *sql.DB, project, tokenHash string, lease, ttl time.Duration, now time.Time) (ConsoleUnlock, error) {
	const op = "store.BeginConsoleUnlock"
	if err := validConsoleUnlock(project, lease, ttl); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if !validTokenHash(tokenHash) {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w: a browser token hash is required", op, ErrInvalidConsoleUnlock)
	}
	id, err := core.NewID()
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	nonce := make([]byte, ConsoleTokenBytes)
	if _, err := rand.Read(nonce); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: nonce: %w", op, err)
	}
	now = now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM console_unlocks WHERE status = ? AND pending_until <= ?`,
		ConsoleUnlockPending, core.FormatTime(now)); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: purge: %w", op, err)
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_unlocks WHERE status = ? AND challenge IS NOT NULL`,
		ConsoleUnlockPending).Scan(&waiting); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if waiting >= maxPendingConsoleUnlocks {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrConsoleUnlockBusy)
	}
	challenge := ConsoleUnlockChallenge(id, project, tokenHash, nonce)
	if _, err := tx.ExecContext(ctx, `INSERT INTO console_unlocks (id, project_slug, status, token_hash, challenge,
		lease_seconds, created_at, pending_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, project, ConsoleUnlockPending, tokenHash, challenge, int64(lease/time.Second),
		core.FormatTime(now), core.FormatTime(now.Add(ttl))); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	u, err := consoleUnlockByID(ctx, tx, id)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	return u, nil
}

// PendingConsoleUnlock returns passkey ceremony id while it waits for the
// browser whose cookie hashes to tokenHash, for project. Another browser's
// ceremony, another project's, a link, a granted or late ceremony are all
// ErrConsoleUnlockNotFound.
func PendingConsoleUnlock(ctx context.Context, db *sql.DB, id, project, tokenHash string, now time.Time) (ConsoleUnlock, error) {
	const op = "store.PendingConsoleUnlock"
	if !validTokenHash(tokenHash) {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrConsoleUnlockNotFound)
	}
	u, err := scanConsoleUnlock(db.QueryRowContext(ctx, `SELECT `+consoleUnlockCols+` FROM console_unlocks
		WHERE id = ? AND project_slug = ? AND token_hash = ? AND status = ? AND challenge IS NOT NULL AND pending_until > ?`,
		id, project, tokenHash, ConsoleUnlockPending, core.FormatTime(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrConsoleUnlockNotFound)
	}
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	return u, nil
}

// GrantConsoleUnlock makes passkey ceremony id live for the credential that
// proved it, until now plus its lease. It is a compare-and-set on the row
// still being that browser's pending ceremony before its deadline, so of two
// racing grants exactly one succeeds; the challenge is cleared with it.
func GrantConsoleUnlock(ctx context.Context, db *sql.DB, id, tokenHash, credential string, now time.Time) (ConsoleUnlock, error) {
	const op = "store.GrantConsoleUnlock"
	if strings.TrimSpace(credential) == "" {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w: the proving credential is required", op, ErrInvalidConsoleUnlock)
	}
	if !validTokenHash(tokenHash) {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrConsoleUnlockNotFound)
	}
	u, err := consoleUnlockByID(ctx, db, id)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	at := core.FormatTime(now)
	res, err := db.ExecContext(ctx, `UPDATE console_unlocks
		SET status = ?, credential = ?, challenge = NULL, granted_at = ?, expires_at = ?
		WHERE id = ? AND token_hash = ? AND status = ? AND challenge IS NOT NULL AND pending_until > ?`,
		ConsoleUnlockLive, credential, at, core.FormatTime(now.Add(u.Lease)), id, tokenHash, ConsoleUnlockPending, at)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := oneRow(res); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	return consoleUnlockByIDWrapped(ctx, db, id, op)
}

func consoleUnlockByIDWrapped(ctx context.Context, db *sql.DB, id, op string) (ConsoleUnlock, error) {
	u, err := consoleUnlockByID(ctx, db, id)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	return u, nil
}

// oneRow turns an UPDATE that touched no row into ErrConsoleUnlockNotFound.
func oneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConsoleUnlockNotFound
	}
	return nil
}

// CreateConsoleUnlockLink records a one-time link that unlocks project, for
// lease, in whichever browser claims it before now plus ttl. It returns the
// link's secret -- the only place it ever exists outside the browser -- and the
// row. Only `seamlessd console-open --unlock`, run as the daemon's own account,
// calls it: no route does.
func CreateConsoleUnlockLink(ctx context.Context, db *sql.DB, project string, lease, ttl time.Duration, now time.Time) (string, ConsoleUnlock, error) {
	const op = "store.CreateConsoleUnlockLink"
	if err := validConsoleUnlock(project, lease, ttl); err != nil {
		return "", ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	id, err := core.NewID()
	if err != nil {
		return "", ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	secret, err := NewConsoleToken()
	if err != nil {
		return "", ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	now = now.UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO console_unlocks (id, project_slug, status, claim_hash, credential,
		lease_seconds, created_at, pending_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, project, ConsoleUnlockPending, HashConsoleToken(secret), ConsoleUnlockByDaemon, int64(lease/time.Second),
		core.FormatTime(now), core.FormatTime(now.Add(ttl))); err != nil {
		return "", ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	u, err := consoleUnlockByIDWrapped(ctx, db, id, op)
	if err != nil {
		return "", ConsoleUnlock{}, err
	}
	return secret, u, nil
}

// ClaimConsoleUnlockLink makes the link whose secret is given live for the
// browser whose cookie hashes to tokenHash, until now plus its lease. It is a
// compare-and-set on the link still pending before its deadline, and it clears
// the link's hash, so a link works once.
func ClaimConsoleUnlockLink(ctx context.Context, db *sql.DB, secret, tokenHash string, now time.Time) (ConsoleUnlock, error) {
	const op = "store.ClaimConsoleUnlockLink"
	claim := HashConsoleToken(secret)
	if claim == "" || !validTokenHash(tokenHash) {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrConsoleUnlockNotFound)
	}
	at := core.FormatTime(now)
	u, err := scanConsoleUnlock(db.QueryRowContext(ctx, `SELECT `+consoleUnlockCols+` FROM console_unlocks
		WHERE claim_hash = ? AND status = ? AND pending_until > ?`, claim, ConsoleUnlockPending, at))
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, ErrConsoleUnlockNotFound)
	}
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	res, err := db.ExecContext(ctx, `UPDATE console_unlocks
		SET status = ?, token_hash = ?, claim_hash = '', granted_at = ?, expires_at = ?
		WHERE id = ? AND claim_hash = ? AND status = ? AND pending_until > ?`,
		ConsoleUnlockLive, tokenHash, at, core.FormatTime(now.Add(u.Lease)), u.ID, claim, ConsoleUnlockPending, at)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := oneRow(res); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	return consoleUnlockByIDWrapped(ctx, db, u.ID, op)
}

func queryConsoleUnlocks(ctx context.Context, db *sql.DB, op, where string, args ...any) ([]ConsoleUnlock, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+consoleUnlockCols+` FROM console_unlocks WHERE `+where+`
		ORDER BY project_slug, expires_at DESC, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConsoleUnlock
	for rows.Next() {
		u, err := scanConsoleUnlock(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// LiveConsoleUnlocks returns the unlocks live at now for the browser whose
// cookie hashes to tokenHash. A malformed hash -- no cookie, or one this
// daemon never issued the shape of -- holds none.
func LiveConsoleUnlocks(ctx context.Context, db *sql.DB, tokenHash string, now time.Time) ([]ConsoleUnlock, error) {
	if !validTokenHash(tokenHash) {
		return nil, nil
	}
	return queryConsoleUnlocks(ctx, db, "store.LiveConsoleUnlocks", `token_hash = ? AND status = ? AND expires_at > ?`,
		tokenHash, ConsoleUnlockLive, core.FormatTime(now))
}

// AllLiveConsoleUnlocks returns every browser's unlocks live at now.
func AllLiveConsoleUnlocks(ctx context.Context, db *sql.DB, now time.Time) ([]ConsoleUnlock, error) {
	return queryConsoleUnlocks(ctx, db, "store.AllLiveConsoleUnlocks", `status = ? AND expires_at > ?`,
		ConsoleUnlockLive, core.FormatTime(now))
}

func validConsoleUnlockEnd(reason string) error {
	if !slices.Contains(ConsoleUnlockEndReasons, reason) {
		return fmt.Errorf("%w: end reason %q: valid values are %s", ErrInvalidConsoleUnlock, reason, strings.Join(ConsoleUnlockEndReasons, ", "))
	}
	return nil
}

// EndConsoleUnlock ends one live unlock at now. One that already ended or
// expired is ErrConsoleUnlockNotLive. It returns the row as it ended.
func EndConsoleUnlock(ctx context.Context, db *sql.DB, id, reason string, now time.Time) (ConsoleUnlock, error) {
	const op = "store.EndConsoleUnlock"
	if err := validConsoleUnlockEnd(reason); err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	at := core.FormatTime(now)
	res, err := db.ExecContext(ctx, `UPDATE console_unlocks SET status = ?, ended_at = ?, ended_reason = ?
		WHERE id = ? AND status = ? AND expires_at > ?`, ConsoleUnlockEnded, at, reason, id, ConsoleUnlockLive, at)
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return ConsoleUnlock{}, fmt.Errorf("%s: %w: %q", op, ErrConsoleUnlockNotLive, id)
	}
	return consoleUnlockByIDWrapped(ctx, db, id, op)
}

// EndAllConsoleUnlocks ends every live unlock and every pending ceremony and
// link at now (a restore brings back the unlocks a snapshot held), returning
// how many.
func EndAllConsoleUnlocks(ctx context.Context, db *sql.DB, reason string, now time.Time) (int64, error) {
	const op = "store.EndAllConsoleUnlocks"
	if err := validConsoleUnlockEnd(reason); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	res, err := db.ExecContext(ctx, `UPDATE console_unlocks SET status = ?, ended_at = ?, ended_reason = ?, claim_hash = '', challenge = NULL
		WHERE status IN (?, ?)`, ConsoleUnlockEnded, core.FormatTime(now), reason, ConsoleUnlockLive, ConsoleUnlockPending)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return n, nil
}

// LiveAccessLeases returns every agent lease live at now, for the owner's
// access page: not revoked, expiring after now.
func LiveAccessLeases(ctx context.Context, db *sql.DB, now time.Time) ([]AccessLease, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+leaseCols+` FROM access_leases
		WHERE revoked_at IS NULL AND expires_at > ? ORDER BY project_slug, expires_at DESC, id`, core.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("store.LiveAccessLeases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanLeases(rows)
	if err != nil {
		return nil, fmt.Errorf("store.LiveAccessLeases: %w", err)
	}
	return out, nil
}

// AccessLeaseByID returns one lease row, live or not.
func AccessLeaseByID(ctx context.Context, db *sql.DB, id string) (AccessLease, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+leaseCols+` FROM access_leases WHERE id = ?`, id)
	if err != nil {
		return AccessLease{}, fmt.Errorf("store.AccessLeaseByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanLeases(rows)
	if err != nil {
		return AccessLease{}, fmt.Errorf("store.AccessLeaseByID: %w", err)
	}
	if len(out) == 0 {
		return AccessLease{}, fmt.Errorf("store.AccessLeaseByID: %w: %q", ErrLeaseNotLive, id)
	}
	return out[0], nil
}

// ProjectTally is what a locked project shows where it cannot be read: its
// name and how many items it holds -- aggregates, never a row of content.
type ProjectTally struct {
	Slug       string
	Name       string
	Registered bool
	Memories   int // active memories
	Notes      int
	Sessions   int
	OpenTasks  int // open or in progress
}

// ProjectTallies counts each project in slugs on db, which must be the main
// handle (a filtered one hides exactly these projects). Every slug gets a
// tally, in the order given; one with no projects row is unregistered, named
// by its slug, and counts what rows name it.
func ProjectTallies(ctx context.Context, db *sql.DB, slugs []string) ([]ProjectTally, error) {
	const op = "store.ProjectTallies"
	out := make([]ProjectTally, 0, len(slugs))
	for _, slug := range slugs {
		t := ProjectTally{Slug: slug, Name: slug}
		var name sql.NullString
		err := db.QueryRowContext(ctx, `SELECT name FROM projects WHERE slug = ?`, slug).Scan(&name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("%s: %w", op, err)
		default:
			t.Registered = true
			if strings.TrimSpace(name.String) != "" {
				t.Name = name.String
			}
		}
		for _, c := range []struct {
			dst *int
			q   string
		}{
			{&t.Memories, `SELECT COUNT(*) FROM memories_index WHERE project = ? AND invalid_at IS NULL`},
			{&t.Notes, `SELECT COUNT(*) FROM notes_index WHERE project = ?`},
			{&t.Sessions, `SELECT COUNT(*) FROM sessions WHERE project_slug = ?`},
			{&t.OpenTasks, `SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND status IN ('open','in_progress')`},
		} {
			if err := db.QueryRowContext(ctx, c.q, slug).Scan(c.dst); err != nil {
				return nil, fmt.Errorf("%s: %w", op, err)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// CountRegisteredProjects counts the slugs that have a projects row on db
// (the main handle: a filtered one hides exactly these).
func CountRegisteredProjects(ctx context.Context, db *sql.DB, slugs []string) (int, error) {
	if len(slugs) == 0 {
		return 0, nil
	}
	args := make([]any, len(slugs))
	for i, s := range slugs {
		args[i] = s
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE slug IN (`+placeholders(len(args))+`)`, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountRegisteredProjects: %w", err)
	}
	return n, nil
}
