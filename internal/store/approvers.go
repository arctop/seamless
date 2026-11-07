package store

// Approver credentials (migration 032): what can sign an access approval's
// challenge with the owner present -- a WebAuthn passkey or the macOS Touch ID
// key. This file keeps the rows and the enrollment rules:
//
//   - The first credential needs a one-time enrollment code. Only
//     `seamlessd approvers enroll-code`, run as the daemon's own account,
//     mints one (MintEnrollCode); no route does. The code is the secret: it is
//     stored only as a hash, lives EnrollCodeTTL, and is consumed in the same
//     transaction as the credential it enrolls.
//   - A later credential needs a code too, or the approval of an existing
//     credential over a challenge that binds the new one. The caller verifies
//     that assertion (internal/passkey); EnrollApprover checks the approving
//     credential exists.
//
// The ceremonies themselves live above the store; a row here is input to them
// and never a grant on its own.

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

	"github.com/arctop/seamless/internal/core"
)

// Approver credential kinds.
const (
	// ApproverPasskey is a WebAuthn credential.
	ApproverPasskey = "passkey"
	// ApproverTouchID is the macOS Touch ID helper's Secure Enclave key.
	ApproverTouchID = "touchid"
)

// ApproverKinds is the canonical kind set.
var ApproverKinds = []string{ApproverPasskey, ApproverTouchID}

const (
	// EnrollCodeTTL is how long an enrollment code stays valid.
	EnrollCodeTTL = 10 * time.Minute
	// AddedByCode is approver_credentials.added_by for a credential a code
	// enrolled; otherwise it holds the approving credential's id.
	AddedByCode = "code"
	// enrollCodeSymbols is Crockford's base32: no I, L, O or U.
	enrollCodeSymbols = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// enrollCodeLen is 16 symbols, 80 bits.
	enrollCodeLen = 16
	// approverUserHandleKey is the settings key of the owner's WebAuthn user
	// handle: one random value every passkey is registered to.
	approverUserHandleKey = "approver_user_handle"
	// MaxApproverNameRunes caps an approver's label.
	MaxApproverNameRunes = 64
)

var (
	// ErrEnrollCode is an enrollment code that is absent, expired, already
	// used, or minted for another kind. One error for all four, so a caller
	// learns nothing about which.
	ErrEnrollCode = errors.New("enrollment code is not valid")
	// ErrEnrollProof is an enrollment with no proof (or two): it needs exactly
	// one of a code or an existing credential's approval, and the first
	// credential needs a code.
	ErrEnrollProof = errors.New("enrollment needs a code or an existing credential's approval")
	// ErrApproverNotFound is an approver id or credential id with no row.
	ErrApproverNotFound = errors.New("approver credential not found")
	// ErrApproverExists is a credential already enrolled.
	ErrApproverExists = errors.New("approver credential already enrolled")
	// ErrInvalidApprover is a credential record this store refuses.
	ErrInvalidApprover = errors.New("invalid approver credential")
	// ErrApproverCounter is a use recorded against a sign counter that moved
	// since it was read: two assertions raced, and only one may pass.
	ErrApproverCounter = errors.New("approver credential counter changed")
)

// ApproverCredential is one enrolled credential.
type ApproverCredential struct {
	ID                string
	Kind              string
	CredentialID      []byte
	PublicKey         []byte
	Alg               int64
	SignCount         uint32
	BackupEligible    bool
	BackupState       bool
	AAGUID            []byte
	Transports        []string
	Attachment        string
	AttestationFormat string
	UserHandle        []byte
	Name              string
	AddedAt           time.Time
	AddedBy           string
	LastUsedAt        *time.Time
}

func validApproverKind(kind string) error {
	if !slices.Contains(ApproverKinds, kind) {
		return fmt.Errorf("%w: kind %q: valid kinds are %s", ErrInvalidApprover, kind, strings.Join(ApproverKinds, ", "))
	}
	return nil
}

// NormalizeEnrollCode turns a code as typed into its canonical form:
// uppercase, without the dashes and spaces it is printed with, and with
// Crockford's look-alikes folded (O to 0, I and L to 1).
func NormalizeEnrollCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func enrollCodeHash(code string) string {
	sum := sha256.Sum256([]byte(NormalizeEnrollCode(code)))
	return hex.EncodeToString(sum[:])
}

// MintEnrollCode creates a one-time enrollment code for kind, valid for
// EnrollCodeTTL from now, and returns it formatted for a person
// (XXXX-XXXX-XXXX-XXXX). Only the hash is stored. Nothing but
// `seamlessd approvers enroll-code` calls it: no route may.
func MintEnrollCode(ctx context.Context, db *sql.DB, kind string, now time.Time) (string, time.Time, error) {
	if err := validApproverKind(kind); err != nil {
		return "", time.Time{}, fmt.Errorf("store.MintEnrollCode: %w", err)
	}
	raw := make([]byte, enrollCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("store.MintEnrollCode: %w", err)
	}
	var b strings.Builder
	for i, v := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(enrollCodeSymbols[int(v)%len(enrollCodeSymbols)])
	}
	code := b.String()
	expires := now.Add(EnrollCodeTTL)
	if _, err := db.ExecContext(ctx, `INSERT INTO approver_enroll_codes (hash, kind, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		enrollCodeHash(code), kind, core.FormatTime(now), core.FormatTime(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("store.MintEnrollCode: %w", err)
	}
	return code, expires, nil
}

// CheckEnrollCode reports whether code is a live, unused code for kind at now,
// without consuming it: the begin step of an enrollment, so a mistyped code is
// refused before the owner touches an authenticator.
func CheckEnrollCode(ctx context.Context, db *sql.DB, kind, code string, now time.Time) error {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_enroll_codes
		WHERE hash = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`,
		enrollCodeHash(code), kind, core.FormatTime(now)).Scan(&n)
	if err != nil {
		return fmt.Errorf("store.CheckEnrollCode: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.CheckEnrollCode: %w", ErrEnrollCode)
	}
	return nil
}

func validApproverCredential(c ApproverCredential) error {
	if err := validApproverKind(c.Kind); err != nil {
		return err
	}
	if n := len(c.CredentialID); n == 0 || n > 1023 {
		return fmt.Errorf("%w: credential id of %d bytes", ErrInvalidApprover, n)
	}
	if len(c.PublicKey) == 0 {
		return fmt.Errorf("%w: no public key", ErrInvalidApprover)
	}
	if c.Kind == ApproverTouchID && (len(c.PublicKey) != 65 || c.PublicKey[0] != 0x04) {
		return fmt.Errorf("%w: a Touch ID key is an uncompressed P-256 point (65 bytes)", ErrInvalidApprover)
	}
	name := strings.TrimSpace(c.Name)
	if name == "" || len([]rune(name)) > MaxApproverNameRunes || strings.ContainsAny(name, "\r\n\t") {
		return fmt.Errorf("%w: a name is 1 to %d characters on one line", ErrInvalidApprover, MaxApproverNameRunes)
	}
	return nil
}

// EnrollApprover stores cred, proven by exactly one of: code, a live unused
// enrollment code of cred's kind (consumed here, in the same transaction), or
// approvedBy, the id of an existing credential whose approval of this
// enrollment the caller has verified. The first credential needs a code. It
// returns the stored row.
func EnrollApprover(ctx context.Context, db *sql.DB, cred ApproverCredential, code, approvedBy string, now time.Time) (ApproverCredential, error) {
	const op = "store.EnrollApprover"
	cred.Name = strings.TrimSpace(cred.Name)
	if err := validApproverCredential(cred); err != nil {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	if (code == "") == (approvedBy == "") {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrEnrollProof)
	}
	id, err := core.NewID()
	if err != nil {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()

	addedBy := AddedByCode
	if code != "" {
		res, err := tx.ExecContext(ctx, `UPDATE approver_enroll_codes SET used_at = ?, used_for = ?
			WHERE hash = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`,
			core.FormatTime(now), id, enrollCodeHash(code), cred.Kind, core.FormatTime(now))
		if err != nil {
			return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
		}
		if n != 1 {
			return ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrEnrollCode)
		}
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_credentials WHERE id = ?`, approvedBy).Scan(&n); err != nil {
			return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
		}
		if n == 0 {
			return ApproverCredential{}, fmt.Errorf("%s: %w: the approving credential %q is not enrolled", op, ErrEnrollProof, approvedBy)
		}
		addedBy = approvedBy
	}

	var dup int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_credentials WHERE kind = ? AND credential_id = ?`,
		cred.Kind, cred.CredentialID).Scan(&dup); err != nil {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	if dup > 0 {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, ErrApproverExists)
	}
	cred.ID, cred.AddedAt, cred.AddedBy, cred.LastUsedAt = id, now, addedBy, nil
	if _, err := tx.ExecContext(ctx, `INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, sign_count,
		backup_eligible, backup_state, aaguid, transports, attachment, attestation_format, user_handle, name, added_at, added_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cred.ID, cred.Kind, cred.CredentialID, cred.PublicKey, cred.Alg, int64(cred.SignCount),
		boolInt(cred.BackupEligible), boolInt(cred.BackupState), cred.AAGUID, strings.Join(cred.Transports, ","),
		cred.Attachment, cred.AttestationFormat, cred.UserHandle, cred.Name, core.FormatTime(now), addedBy); err != nil {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return ApproverCredential{}, fmt.Errorf("%s: %w", op, err)
	}
	return cred, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const approverCols = `id, kind, credential_id, public_key, alg, sign_count, backup_eligible, backup_state, aaguid,
	transports, attachment, attestation_format, user_handle, name, added_at, added_by, last_used_at`

func scanApprover(row interface{ Scan(...any) error }) (ApproverCredential, error) {
	var (
		c                ApproverCredential
		count            int64
		be, bs           int
		transports       string
		addedAt          string
		lastUsed         sql.NullString
		aaguid, userHand []byte
	)
	if err := row.Scan(&c.ID, &c.Kind, &c.CredentialID, &c.PublicKey, &c.Alg, &count, &be, &bs, &aaguid,
		&transports, &c.Attachment, &c.AttestationFormat, &userHand, &c.Name, &addedAt, &c.AddedBy, &lastUsed); err != nil {
		return ApproverCredential{}, err
	}
	if count < 0 || count > int64(^uint32(0)) {
		return ApproverCredential{}, fmt.Errorf("%w: sign count %d out of range", ErrInvalidApprover, count)
	}
	c.SignCount = uint32(count)
	c.BackupEligible, c.BackupState = be != 0, bs != 0
	c.AAGUID, c.UserHandle = aaguid, userHand
	if transports != "" {
		c.Transports = strings.Split(transports, ",")
	}
	t, err := core.ParseTime(addedAt)
	if err != nil {
		return ApproverCredential{}, fmt.Errorf("added_at: %w", err)
	}
	c.AddedAt = t
	if lastUsed.Valid {
		lu, err := core.ParseTime(lastUsed.String)
		if err != nil {
			return ApproverCredential{}, fmt.Errorf("last_used_at: %w", err)
		}
		c.LastUsedAt = &lu
	}
	return c, nil
}

// ApproverCredentials returns the enrolled credentials of kind ("" for every
// kind), oldest first.
func ApproverCredentials(ctx context.Context, db *sql.DB, kind string) ([]ApproverCredential, error) {
	if kind != "" {
		if err := validApproverKind(kind); err != nil {
			return nil, fmt.Errorf("store.ApproverCredentials: %w", err)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT `+approverCols+` FROM approver_credentials
		WHERE ? = '' OR kind = ? ORDER BY added_at, id`, kind, kind)
	if err != nil {
		return nil, fmt.Errorf("store.ApproverCredentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ApproverCredential
	for rows.Next() {
		c, err := scanApprover(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ApproverCredentials: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ApproverCredentials: %w", err)
	}
	return out, nil
}

// ApproverByID returns one enrolled credential.
func ApproverByID(ctx context.Context, db *sql.DB, id string) (ApproverCredential, error) {
	c, err := scanApprover(db.QueryRowContext(ctx, `SELECT `+approverCols+` FROM approver_credentials WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ApproverCredential{}, fmt.Errorf("store.ApproverByID: %w: %q", ErrApproverNotFound, id)
	}
	if err != nil {
		return ApproverCredential{}, fmt.Errorf("store.ApproverByID: %w", err)
	}
	return c, nil
}

// CountApprovers returns how many credentials are enrolled.
func CountApprovers(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_credentials`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountApprovers: %w", err)
	}
	return n, nil
}

// RecordApproverUse stores what one verified assertion learned: the new sign
// counter and backup state, and the time. It is a compare-and-set on the
// counter the assertion was verified against, so of two assertions racing
// over one stored counter only the first is recorded; the second is
// ErrApproverCounter and must not be honored.
func RecordApproverUse(ctx context.Context, db *sql.DB, id string, verifiedAgainst, newCount uint32, backupState bool, now time.Time) error {
	res, err := db.ExecContext(ctx, `UPDATE approver_credentials SET sign_count = ?, backup_state = ?, last_used_at = ?
		WHERE id = ? AND sign_count = ?`, int64(newCount), boolInt(backupState), core.FormatTime(now), id, int64(verifiedAgainst))
	if err != nil {
		return fmt.Errorf("store.RecordApproverUse: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.RecordApproverUse: %w", err)
	}
	if n == 1 {
		return nil
	}
	if _, err := ApproverByID(ctx, db, id); err != nil {
		return fmt.Errorf("store.RecordApproverUse: %w", err)
	}
	return fmt.Errorf("store.RecordApproverUse: %w", ErrApproverCounter)
}

// RemoveApprover deletes one credential. Removing the last one leaves only
// the terminal approval path; leases already granted are untouched (reset
// revokes them).
func RemoveApprover(ctx context.Context, db *sql.DB, id string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM approver_credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store.RemoveApprover: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.RemoveApprover: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.RemoveApprover: %w: %q", ErrApproverNotFound, id)
	}
	return nil
}

// ResetApprovers deletes every credential and every unused enrollment code,
// revokes every live lease and read grant (reason reset) and ends every
// console unlock, in one transaction: what the
// owner does when a credential may be in someone else's hands. It returns
// how many credentials and leases it ended.
func ResetApprovers(ctx context.Context, db *sql.DB, now time.Time) (credentials, leases int64, err error) {
	const op = "store.ResetApprovers"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM approver_credentials`)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	if credentials, err = res.RowsAffected(); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM approver_enroll_codes WHERE used_at IS NULL`); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	res, err = tx.ExecContext(ctx, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
		WHERE revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), LeaseRevokedReset, core.FormatTime(now))
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	if leases, err = res.RowsAffected(); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	// So was a read grant under peer_auth (isolation.requests): it ends with
	// them too. Its holder stays confined, as after any revoke.
	if _, err := tx.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), LeaseRevokedReset, core.FormatTime(now)); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	// A console unlock was granted by a credential too: it ends with them.
	if _, err := tx.ExecContext(ctx, `UPDATE console_unlocks SET status = 'ended', ended_at = ?, ended_reason = ?,
		claim_hash = '', challenge = NULL WHERE status IN ('live', 'pending')`, core.FormatTime(now), ConsoleUnlockEndReset); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("%s: %w", op, err)
	}
	return credentials, leases, nil
}

// ApproverUserHandle returns the owner's WebAuthn user handle: 32 random
// bytes, made on first use and kept in settings, so every passkey is
// registered to the same user (and an authenticator never holds two
// credentials for one owner).
func ApproverUserHandle(ctx context.Context, db *sql.DB) ([]byte, error) {
	v, ok, err := GetSetting(ctx, db, approverUserHandleKey)
	if err != nil {
		return nil, fmt.Errorf("store.ApproverUserHandle: %w", err)
	}
	if ok {
		h, err := hex.DecodeString(v)
		if err != nil || len(h) != 32 {
			return nil, fmt.Errorf("store.ApproverUserHandle: %w: the stored handle is not 32 hex-encoded bytes", ErrInvalidApprover)
		}
		return h, nil
	}
	h := make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		return nil, fmt.Errorf("store.ApproverUserHandle: %w", err)
	}
	// INSERT OR IGNORE, then read back: two first uses racing agree on one.
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`,
		approverUserHandleKey, hex.EncodeToString(h)); err != nil {
		return nil, fmt.Errorf("store.ApproverUserHandle: %w", err)
	}
	return ApproverUserHandle(ctx, db)
}
