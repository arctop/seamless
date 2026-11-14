package store

// Console sign-in with a passkey (console.sign_in: passkey; migration 041).
// Two records, both empty while the key signs browsers in (the default):
//
//   - console_signins: a browser signed in. The browser holds a random
//     seamless_console cookie and a key only the console's origin can use
//     (internal/console/browserproof); the row keeps HashConsoleSignIn of the
//     two together, so a cookie read on another port of the host, without the
//     key, names no sign-in. Only a passkey assertion makes a row
//     (approval.SignIn). It ends at its expiry, at a sign-out (found by the
//     cookie alone: signing out needs no proof), when the passkey that signed
//     it in is removed, or at a reset or a restore.
//   - passkey_enrollments: a passkey a device created on the console's Add a
//     passkey page, waiting for the owner. Its short code names the row and
//     nothing else. ApprovePasskeyEnrollment, run by `seamlessd approvers
//     approve` (as the daemon's account under peer_auth), copies the row's
//     credential into approver_credentials. The approval carries no key, so a
//     code read off a screen or a terminal cannot enroll another one, and an
//     enrolled passkey vouches for no new one: no route can approve.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/arctop/seamless/internal/core"
)

// Why a console sign-in ended before its expiry.
const (
	// SignInEndSignedOut is the browser signing out.
	SignInEndSignedOut = "signed-out"
	// SignInEndRemoved is the passkey that signed it in being removed.
	SignInEndRemoved = "removed"
	// SignInEndReset is every sign-in ending at once: every approver
	// credential reset, or a restored database.
	SignInEndReset = "reset"
)

// SignInEndReasons is the canonical set of sign-in end reasons.
var SignInEndReasons = []string{SignInEndSignedOut, SignInEndRemoved, SignInEndReset}

// signInKeep is how long an ended or expired sign-in stays on record; a new
// sign-in prunes older ones.
const signInKeep = 30 * 24 * time.Hour

var (
	// ErrSignInNotFound is a cookie and key that name no live sign-in.
	ErrSignInNotFound = errors.New("console sign-in not found")
	// ErrInvalidSignIn is a malformed sign-in argument.
	ErrInvalidSignIn = errors.New("invalid console sign-in")
)

// ConsoleSignIn is one row of console_signins.
type ConsoleSignIn struct {
	ID          string
	Credential  string // the approver credential that signed the browser in
	CreatedAt   time.Time
	ExpiresAt   time.Time
	EndedAt     *time.Time
	EndedReason string
}

// HashConsoleSignIn is the token_hash a sign-in stores: the browser's
// seamless_console cookie and the public key its proofs verify under
// (browserproof), hashed together under a prefix of their own, apart from an
// unlock's (HashBrowserUnlock). Either one empty has no hash.
func HashConsoleSignIn(cookie, browserKey string) string {
	if cookie == "" || browserKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("seamless console sign-in v1\x00" + cookie + "\x00" + browserKey))
	return hex.EncodeToString(sum[:])
}

// hashSignInCookie is the cookie_hash a sign-in stores: the cookie alone, for
// a sign-out to find its row by.
func hashSignInCookie(cookie string) string {
	if cookie == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("seamless console sign-in cookie v1\x00" + cookie))
	return hex.EncodeToString(sum[:])
}

const consoleSignInCols = `id, credential, created_at, expires_at, ended_at, ended_reason`

func scanConsoleSignIn(row interface{ Scan(...any) error }) (ConsoleSignIn, error) {
	var (
		s                ConsoleSignIn
		created, expires string
		ended            sql.NullString
	)
	if err := row.Scan(&s.ID, &s.Credential, &created, &expires, &ended, &s.EndedReason); err != nil {
		return ConsoleSignIn{}, err
	}
	var err error
	if s.CreatedAt, err = core.ParseTime(created); err != nil {
		return ConsoleSignIn{}, fmt.Errorf("console sign-in %s created_at: %w", s.ID, err)
	}
	if s.ExpiresAt, err = core.ParseTime(expires); err != nil {
		return ConsoleSignIn{}, fmt.Errorf("console sign-in %s expires_at: %w", s.ID, err)
	}
	if ended.Valid {
		t, err := core.ParseTime(ended.String)
		if err != nil {
			return ConsoleSignIn{}, fmt.Errorf("console sign-in %s ended_at: %w", s.ID, err)
		}
		s.EndedAt = &t
	}
	return s, nil
}

// CreateConsoleSignIn records that the browser holding cookie and browserKey
// is signed in, by credential, for lifetime from now. Sign-ins that ended or
// expired more than signInKeep ago are pruned in the same transaction.
func CreateConsoleSignIn(ctx context.Context, db *sql.DB, cookie, browserKey, credential string, lifetime time.Duration, now time.Time) (ConsoleSignIn, error) {
	const op = "store.CreateConsoleSignIn"
	token := HashConsoleSignIn(cookie, browserKey)
	if token == "" {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w: a cookie and a browser key are required", op, ErrInvalidSignIn)
	}
	if strings.TrimSpace(credential) == "" {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w: the credential that signed in is required", op, ErrInvalidSignIn)
	}
	if lifetime < time.Second {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w: a sign-in lasts at least a second", op, ErrInvalidSignIn)
	}
	id, err := core.NewID()
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	now = now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	old := core.FormatTime(now.Add(-signInKeep))
	if _, err := tx.ExecContext(ctx, `DELETE FROM console_signins WHERE expires_at <= ? OR ended_at <= ?`, old, old); err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: prune: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO console_signins (id, token_hash, cookie_hash, credential, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, token, hashSignInCookie(cookie), credential,
		core.FormatTime(now), core.FormatTime(now.Add(lifetime))); err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	s, err := scanConsoleSignIn(tx.QueryRowContext(ctx, `SELECT `+consoleSignInCols+` FROM console_signins WHERE id = ?`, id))
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	return s, nil
}

// LiveConsoleSignIn returns the sign-in live at now for the browser whose
// cookie and key hash to tokenHash: ErrSignInNotFound for a malformed hash, or
// one that names no row, or only rows that ended or expired.
func LiveConsoleSignIn(ctx context.Context, db *sql.DB, tokenHash string, now time.Time) (ConsoleSignIn, error) {
	const op = "store.LiveConsoleSignIn"
	if !validTokenHash(tokenHash) {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, ErrSignInNotFound)
	}
	s, err := scanConsoleSignIn(db.QueryRowContext(ctx, `SELECT `+consoleSignInCols+` FROM console_signins
		WHERE token_hash = ? AND ended_at IS NULL AND expires_at > ? ORDER BY expires_at DESC, id LIMIT 1`,
		tokenHash, core.FormatTime(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, ErrSignInNotFound)
	}
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	return s, nil
}

func validSignInEnd(reason string) error {
	if !slices.Contains(SignInEndReasons, reason) {
		return fmt.Errorf("%w: end reason %q: valid values are %s", ErrInvalidSignIn, reason, strings.Join(SignInEndReasons, ", "))
	}
	return nil
}

// endSignIns ends the live sign-ins where says (with args), for reason, at now,
// and the terminal sign-ins still waiting for their code there
// (signin_terminal.go), so a code shown before a sign-out or a restore can no
// longer sign anyone in.
func endSignIns(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, reason string, now time.Time, where string, args ...any) (int64, error) {
	if err := validSignInEnd(reason); err != nil {
		return 0, err
	}
	at := core.FormatTime(now)
	res, err := q.ExecContext(ctx, `UPDATE console_signins SET ended_at = ?, ended_reason = ?
		WHERE ended_at IS NULL AND (expires_at > ? OR `+isTerminalWaiting+`) AND `+where, append([]any{at, reason, at}, args...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EndConsoleSignInByCookie signs out the browser holding cookie: every live
// sign-in under it ends, signed-out, at now. It needs the cookie alone, not
// the browser's proof, so a sign-out works from a page the service worker did
// not sign; a cookie names nothing more than that. It returns how many ended
// (0 for a cookie that names none).
func EndConsoleSignInByCookie(ctx context.Context, db *sql.DB, cookie string, now time.Time) (int64, error) {
	h := hashSignInCookie(cookie)
	if h == "" {
		return 0, nil
	}
	n, err := endSignIns(ctx, db, SignInEndSignedOut, now, `cookie_hash = ?`, h)
	if err != nil {
		return 0, fmt.Errorf("store.EndConsoleSignInByCookie: %w", err)
	}
	return n, nil
}

// EndAllConsoleSignIns ends every live sign-in at now, for reason, and returns
// how many.
func EndAllConsoleSignIns(ctx context.Context, db *sql.DB, reason string, now time.Time) (int64, error) {
	n, err := endSignIns(ctx, db, reason, now, `1 = 1`)
	if err != nil {
		return 0, fmt.Errorf("store.EndAllConsoleSignIns: %w", err)
	}
	return n, nil
}

// CountLiveConsoleSignIns counts the sign-ins live at now: credential's, or
// every one when credential is "".
func CountLiveConsoleSignIns(ctx context.Context, db *sql.DB, credential string, now time.Time) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_signins
		WHERE ended_at IS NULL AND expires_at > ? AND (? = '' OR credential = ?)`,
		core.FormatTime(now), credential, credential).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountLiveConsoleSignIns: %w", err)
	}
	return n, nil
}

// Passkey enrollments: a device's passkey, waiting for the owner.

// Passkey enrollment statuses.
const (
	EnrollmentPending  = "pending"
	EnrollmentEnrolled = "enrolled"
	EnrollmentEnded    = "ended"
)

// PasskeyEnrollmentTTL is how long a passkey a device created waits for the
// owner's approval: time to reach a terminal on the server and run the
// command.
const PasskeyEnrollmentTTL = 10 * time.Minute

// maxPendingEnrollments caps the passkeys waiting at once, so a caller looping
// on the Add a passkey page cannot grow the table without bound. A full queue
// refuses the next until one is approved or its time runs out.
const maxPendingEnrollments = 32

var (
	// ErrEnrollmentNotFound is a code that names no passkey still waiting:
	// mistyped, already approved, past its deadline, or never shown.
	ErrEnrollmentNotFound = errors.New("no passkey is waiting for that code")
	// ErrEnrollmentWaiting is a passkey already waiting for approval.
	ErrEnrollmentWaiting = errors.New("that passkey is already waiting for approval")
	// ErrEnrollmentBusy is an enrollment refused while maxPendingEnrollments
	// wait.
	ErrEnrollmentBusy = errors.New("too many passkeys are waiting for approval")
	// ErrInvalidEnrollment is a malformed enrollment argument.
	ErrInvalidEnrollment = errors.New("invalid passkey enrollment")
)

// PasskeyEnrollment is one row of passkey_enrollments.
type PasskeyEnrollment struct {
	ID     string
	Status string
	// Credential is the passkey as approver_credentials will hold it, with
	// no ID yet.
	Credential   ApproverCredential
	CreatedAt    time.Time
	PendingUntil time.Time
	EnrolledAs   string
	EndedAt      *time.Time
	EndedReason  string
}

// Fingerprint names the enrollment's key the way the device's page shows it
// (ApproverKeyFingerprint).
func (e PasskeyEnrollment) Fingerprint() string {
	return ApproverKeyFingerprint(e.Credential.PublicKey)
}

// ValidPasskeyName checks the label a device gives its passkey: 1 to
// MaxApproverNameRunes characters once trimmed, with no control or format
// characters. The approval prints it in the owner's terminal, where a control
// character could rewrite what the owner reads.
func ValidPasskeyName(name string) error {
	name = strings.TrimSpace(name)
	n := len([]rune(name))
	if n == 0 || n > MaxApproverNameRunes {
		return fmt.Errorf("%w: a name is 1 to %d characters", ErrInvalidApprover, MaxApproverNameRunes)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return fmt.Errorf("%w: a name holds letters, digits, spaces and punctuation, on one line", ErrInvalidApprover)
		}
	}
	return nil
}

// enrollmentCodeHash is what code_hash stores of a code: hex SHA-256 of its
// canonical form under a prefix no other code uses.
func enrollmentCodeHash(code string) string {
	sum := sha256.Sum256([]byte("seamless passkey enrollment code v1\x00" + NormalizeEnrollCode(code)))
	return hex.EncodeToString(sum[:])
}

// newEnrollmentCode draws a fresh code, printed XXXX-XXXX like a terminal
// unlock's. A variable so a test can force the re-roll.
var newEnrollmentCode = func() (string, error) {
	raw := make([]byte, unlockCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	b := make([]byte, unlockCodeLen)
	for i, v := range raw {
		// 256 is a multiple of the 32 symbols, so every symbol is as likely.
		b[i] = enrollCodeSymbols[int(v)%len(enrollCodeSymbols)]
	}
	return FormatUnlockCode(string(b)), nil
}

const passkeyEnrollmentCols = `id, status, name, credential_id, public_key, alg, sign_count, backup_eligible, backup_state, aaguid,
	transports, attachment, attestation_format, user_handle, rp_id, created_at, pending_until, enrolled_as, ended_at, ended_reason`

func scanPasskeyEnrollment(row interface{ Scan(...any) error }) (PasskeyEnrollment, error) {
	var (
		e                PasskeyEnrollment
		count            int64
		be, bs           int
		transports       string
		created, pending string
		ended            sql.NullString
		aaguid, userHand []byte
	)
	c := &e.Credential
	if err := row.Scan(&e.ID, &e.Status, &c.Name, &c.CredentialID, &c.PublicKey, &c.Alg, &count, &be, &bs, &aaguid,
		&transports, &c.Attachment, &c.AttestationFormat, &userHand, &c.RPID, &created, &pending, &e.EnrolledAs,
		&ended, &e.EndedReason); err != nil {
		return PasskeyEnrollment{}, err
	}
	if count < 0 || count > int64(^uint32(0)) {
		return PasskeyEnrollment{}, fmt.Errorf("%w: sign count %d out of range", ErrInvalidEnrollment, count)
	}
	c.Kind, c.SignCount = ApproverPasskey, uint32(count)
	c.BackupEligible, c.BackupState = be != 0, bs != 0
	c.AAGUID, c.UserHandle = aaguid, userHand
	if transports != "" {
		c.Transports = strings.Split(transports, ",")
	}
	var err error
	if e.CreatedAt, err = core.ParseTime(created); err != nil {
		return PasskeyEnrollment{}, fmt.Errorf("passkey enrollment %s created_at: %w", e.ID, err)
	}
	if e.PendingUntil, err = core.ParseTime(pending); err != nil {
		return PasskeyEnrollment{}, fmt.Errorf("passkey enrollment %s pending_until: %w", e.ID, err)
	}
	if ended.Valid {
		t, err := core.ParseTime(ended.String)
		if err != nil {
			return PasskeyEnrollment{}, fmt.Errorf("passkey enrollment %s ended_at: %w", e.ID, err)
		}
		e.EndedAt = &t
	}
	return e, nil
}

// BeginPasskeyEnrollment records cred, a passkey a device just created, as
// waiting for the owner's approval for ttl, and returns the row and the code
// the device shows, printed XXXX-XXXX. Enrollments past their deadline are
// purged first; at most maxPendingEnrollments wait at once. A passkey already
// enrolled is ErrApproverExists, one already waiting ErrEnrollmentWaiting.
func BeginPasskeyEnrollment(ctx context.Context, db *sql.DB, cred ApproverCredential, ttl time.Duration, now time.Time) (PasskeyEnrollment, string, error) {
	const op = "store.BeginPasskeyEnrollment"
	cred.Name = strings.TrimSpace(cred.Name)
	if cred.Kind != ApproverPasskey {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w: only a passkey is enrolled this way", op, ErrInvalidEnrollment)
	}
	if err := ValidPasskeyName(cred.Name); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if err := validApproverCredential(cred); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if ttl < time.Second {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w: an enrollment waits at least a second", op, ErrInvalidEnrollment)
	}
	id, err := core.NewID()
	if err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	now = now.UTC()
	at := core.FormatTime(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM passkey_enrollments WHERE status = ? AND pending_until <= ?`, EnrollmentPending, at); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: purge: %w", op, err)
	}
	var waiting, sameKey, enrolled int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(credential_id = ?), 0) FROM passkey_enrollments WHERE status = ?`,
		cred.CredentialID, EnrollmentPending).Scan(&waiting, &sameKey); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_credentials WHERE kind = ? AND credential_id = ?`,
		ApproverPasskey, cred.CredentialID).Scan(&enrolled); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	switch {
	case enrolled > 0:
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, ErrApproverExists)
	case sameKey > 0:
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, ErrEnrollmentWaiting)
	case waiting >= maxPendingEnrollments:
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, ErrEnrollmentBusy)
	}
	code := ""
	for range unlockCodeTries {
		c, err := newEnrollmentCode()
		if err != nil {
			return PasskeyEnrollment{}, "", fmt.Errorf("%s: code: %w", op, err)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM passkey_enrollments WHERE code_hash = ? AND status = ?`,
			enrollmentCodeHash(c), EnrollmentPending).Scan(&taken); err != nil {
			return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
		}
		if taken == 0 {
			code = c
			break
		}
	}
	if code == "" {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: no free code after %d tries", op, unlockCodeTries)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO passkey_enrollments (id, status, code_hash, name, credential_id, public_key,
		alg, sign_count, backup_eligible, backup_state, aaguid, transports, attachment, attestation_format, user_handle, rp_id,
		created_at, pending_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, EnrollmentPending, enrollmentCodeHash(code), cred.Name, cred.CredentialID, cred.PublicKey, cred.Alg,
		int64(cred.SignCount), boolInt(cred.BackupEligible), boolInt(cred.BackupState), cred.AAGUID,
		strings.Join(cred.Transports, ","), cred.Attachment, cred.AttestationFormat, cred.UserHandle, cred.RPID,
		at, core.FormatTime(now.Add(ttl))); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	e, err := scanPasskeyEnrollment(tx.QueryRowContext(ctx, `SELECT `+passkeyEnrollmentCols+` FROM passkey_enrollments WHERE id = ?`, id))
	if err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return PasskeyEnrollment{}, "", fmt.Errorf("%s: %w", op, err)
	}
	return e, code, nil
}

// pendingByCode reads the enrollment code names, still waiting at at.
func pendingByCode(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, code, at string) (PasskeyEnrollment, error) {
	e, err := scanPasskeyEnrollment(q.QueryRowContext(ctx, `SELECT `+passkeyEnrollmentCols+` FROM passkey_enrollments
		WHERE code_hash = ? AND status = ? AND pending_until > ?`, enrollmentCodeHash(code), EnrollmentPending, at))
	if errors.Is(err, sql.ErrNoRows) {
		return PasskeyEnrollment{}, ErrEnrollmentNotFound
	}
	return e, err
}

// PasskeyEnrollmentByCode returns the passkey code names while it waits: what
// the owner is about to approve. A malformed code is ErrInvalidEnrollment, one
// that names no waiting passkey ErrEnrollmentNotFound.
func PasskeyEnrollmentByCode(ctx context.Context, db *sql.DB, code string, now time.Time) (PasskeyEnrollment, error) {
	const op = "store.PasskeyEnrollmentByCode"
	if FormatUnlockCode(code) == "" {
		return PasskeyEnrollment{}, fmt.Errorf("%s: %w: a code is %d letters and digits, XXXX-XXXX", op, ErrInvalidEnrollment, unlockCodeLen)
	}
	e, err := pendingByCode(ctx, db, code, core.FormatTime(now))
	if err != nil {
		return PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	return e, nil
}

// ApprovePasskeyEnrollment enrolls the passkey code names, still waiting at
// now: its credential, exactly as the device made it, becomes an approver
// credential (added_by code), and the row is marked enrolled with the code
// cleared, in one transaction. The code names the row and carries no key, so
// it enrolls that passkey and no other. It is a compare-and-set on the row
// still waiting, so of two racing approvals exactly one succeeds and a code
// works once. It returns the stored credential and the row as it was.
func ApprovePasskeyEnrollment(ctx context.Context, db *sql.DB, code string, now time.Time) (ApproverCredential, PasskeyEnrollment, error) {
	const op = "store.ApprovePasskeyEnrollment"
	if FormatUnlockCode(code) == "" {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w: a code is %d letters and digits, XXXX-XXXX",
			op, ErrInvalidEnrollment, unlockCodeLen)
	}
	now = now.UTC()
	at := core.FormatTime(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	e, err := pendingByCode(ctx, tx, code, at)
	if err != nil {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	stored, err := insertApproverTx(ctx, tx, e.Credential, AddedByCode, now)
	if err != nil {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE passkey_enrollments SET status = ?, code_hash = '', enrolled_as = ?
		WHERE id = ? AND status = ? AND code_hash = ? AND pending_until > ?`,
		EnrollmentEnrolled, stored.ID, e.ID, EnrollmentPending, enrollmentCodeHash(code), at)
	if err != nil {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, ErrEnrollmentNotFound)
	}
	if err := tx.Commit(); err != nil {
		return ApproverCredential{}, PasskeyEnrollment{}, fmt.Errorf("%s: %w", op, err)
	}
	return stored, e, nil
}

// PasskeyEnrollmentByID returns enrollment id in whatever state, for the page
// waiting on its approval. A row purged past its deadline, or an id that
// never existed, is ErrEnrollmentNotFound.
func PasskeyEnrollmentByID(ctx context.Context, db *sql.DB, id string) (PasskeyEnrollment, error) {
	e, err := scanPasskeyEnrollment(db.QueryRowContext(ctx, `SELECT `+passkeyEnrollmentCols+` FROM passkey_enrollments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return PasskeyEnrollment{}, fmt.Errorf("store.PasskeyEnrollmentByID: %w", ErrEnrollmentNotFound)
	}
	if err != nil {
		return PasskeyEnrollment{}, fmt.Errorf("store.PasskeyEnrollmentByID: %w", err)
	}
	return e, nil
}

// PendingPasskeyEnrollments returns the passkeys waiting at now, oldest first.
func PendingPasskeyEnrollments(ctx context.Context, db *sql.DB, now time.Time) ([]PasskeyEnrollment, error) {
	const op = "store.PendingPasskeyEnrollments"
	rows, err := db.QueryContext(ctx, `SELECT `+passkeyEnrollmentCols+` FROM passkey_enrollments
		WHERE status = ? AND pending_until > ? ORDER BY created_at, id`, EnrollmentPending, core.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []PasskeyEnrollment
	for rows.Next() {
		e, err := scanPasskeyEnrollment(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// EndAllPasskeyEnrollments ends every waiting enrollment at now, for reason (a
// SignInEndReasons value), clearing its code so it can no longer be approved,
// and returns how many.
func EndAllPasskeyEnrollments(ctx context.Context, db *sql.DB, reason string, now time.Time) (int64, error) {
	n, err := endEnrollments(ctx, db, reason, now)
	if err != nil {
		return 0, fmt.Errorf("store.EndAllPasskeyEnrollments: %w", err)
	}
	return n, nil
}

func endEnrollments(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, reason string, now time.Time) (int64, error) {
	if err := validSignInEnd(reason); err != nil {
		return 0, err
	}
	res, err := q.ExecContext(ctx, `UPDATE passkey_enrollments SET status = ?, code_hash = '', ended_at = ?, ended_reason = ?
		WHERE status = ?`, EnrollmentEnded, core.FormatTime(now), reason, EnrollmentPending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
