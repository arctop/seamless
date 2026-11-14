package store

// Approver credentials (migration 032): what can sign an access approval's
// challenge with the owner present -- a WebAuthn passkey or the macOS Touch ID
// key. This file keeps the rows and the enrollment rules:
//
//   - Every credential needs a one-time enrollment code. Only
//     `seamlessd approvers enroll-code`, run as the daemon's own account,
//     mints one (MintEnrollCode); no route does. A code is minted for one key,
//     named by its fingerprint (ApproverKeyFingerprint), so a code read off the
//     owner's terminal cannot enroll any other key. It is stored only as a hash
//     that binds the fingerprint, lives EnrollCodeTTL, and is consumed in the
//     same transaction as the credential it enrolls.
//   - With console sign-in by passkey (console.sign_in: passkey) a passkey can
//     also come the other way round: the device creates it first and shows a
//     short code naming it, and `seamlessd approvers approve --code`, run as
//     the daemon's own account, enrolls exactly that passkey
//     (ApprovePasskeyEnrollment, signin.go). That code names a waiting row and
//     carries no key, so it cannot enroll another one; no route approves.
//   - An enrolled credential never vouches for a new one. An enrolled passkey
//     can take a console lock off, so a single approval tricked out of the
//     owner must not be enough to add a key someone else holds. (Rows an
//     earlier build enrolled that way keep the approving credential's id in
//     added_by; doctor names them.)
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
	// enrolled: every one this build enrolls. A row an earlier build enrolled by
	// another credential's approval holds that credential's id instead.
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
	// used, or minted for another kind or another key. One error for all five,
	// so a caller learns nothing about which.
	ErrEnrollCode = errors.New("enrollment code is not valid")
	// ErrEnrollProof is an enrollment with no code: every credential needs one.
	ErrEnrollProof = errors.New("enrollment needs a one-time enrollment code")
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
	// RPID is the WebAuthn relying party a passkey was enrolled under
	// (migration 038): it works under that RP ID and no other. "" for a Touch
	// ID key, and for a passkey enrolled before the column existed (unknown).
	RPID string
}

// StrandedPasskeys returns the enrolled passkeys whose recorded RP ID is known
// and is not rpID: the ones a daemon serving rpID can no longer verify. A
// passkey with no recorded RP ID is not among them, since nothing says where
// it belongs.
func StrandedPasskeys(creds []ApproverCredential, rpID string) []ApproverCredential {
	var out []ApproverCredential
	for _, c := range creds {
		if c.Kind == ApproverPasskey && c.RPID != "" && c.RPID != rpID {
			out = append(out, c)
		}
	}
	return out
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

// enrollCodeHash is what the table stores for a code minted for the key named
// by fingerprint. The fingerprint is inside the hash, so the same code offered
// with any other key finds no row.
func enrollCodeHash(code, fingerprint string) string {
	sum := sha256.Sum256([]byte(NormalizeEnrollCode(code) + "\x00" + NormalizeEnrollCode(fingerprint)))
	return hex.EncodeToString(sum[:])
}

// ApproverKeyFingerprint is how a person names a public key when minting its
// enrollment code: SHA-256 of the key's bytes, its first 80 bits as 16 Crockford
// base32 symbols, printed XXXX-XXXX-XXXX-XXXX. For a Touch ID key the bytes are
// the raw uncompressed P-256 point, for a passkey its COSE key. The Swift helper
// computes the same thing (Messages.keyFingerprint), and a test on each side
// pins the same vector. Finding another key with this fingerprint is a 2^80
// search, and a code lives ten minutes.
func ApproverKeyFingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	var b strings.Builder
	var acc, bits uint32
	for _, v := range sum {
		acc, bits = acc<<8|uint32(v), bits+8
		for bits >= 5 && b.Len() < enrollCodeLen {
			bits -= 5
			b.WriteByte(enrollCodeSymbols[(acc>>bits)&31])
		}
		if b.Len() == enrollCodeLen {
			break
		}
	}
	return FormatKeyFingerprint(b.String())
}

// FormatKeyFingerprint prints a fingerprint as typed in its canonical form,
// grouped as XXXX-XXXX-XXXX-XXXX, or returns "" when it is not one.
func FormatKeyFingerprint(fingerprint string) string {
	norm := NormalizeEnrollCode(fingerprint)
	if len(norm) != enrollCodeLen || strings.Trim(norm, enrollCodeSymbols) != "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(norm); i++ {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(norm[i])
	}
	return b.String()
}

// MintEnrollCode creates a one-time enrollment code for kind that enrolls only
// the key named by fingerprint (ApproverKeyFingerprint), valid for
// EnrollCodeTTL from now, and returns it formatted for a person
// (XXXX-XXXX-XXXX-XXXX). Only the hash is stored. Nothing but
// `seamlessd approvers enroll-code` calls it: no route may.
func MintEnrollCode(ctx context.Context, db *sql.DB, kind, fingerprint string, now time.Time) (string, time.Time, error) {
	if err := validApproverKind(kind); err != nil {
		return "", time.Time{}, fmt.Errorf("store.MintEnrollCode: %w", err)
	}
	if FormatKeyFingerprint(fingerprint) == "" {
		return "", time.Time{}, fmt.Errorf("store.MintEnrollCode: %w: %q is not a key fingerprint (XXXX-XXXX-XXXX-XXXX)", ErrInvalidApprover, fingerprint)
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
		enrollCodeHash(code, fingerprint), kind, core.FormatTime(now), core.FormatTime(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("store.MintEnrollCode: %w", err)
	}
	return code, expires, nil
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
	if c.Kind == ApproverTouchID && c.RPID != "" {
		return fmt.Errorf("%w: a Touch ID key has no relying party", ErrInvalidApprover)
	}
	if len(c.RPID) > 253 || strings.ContainsAny(c.RPID, " \r\n\t") {
		return fmt.Errorf("%w: rp_id %q is not a host name", ErrInvalidApprover, c.RPID)
	}
	name := strings.TrimSpace(c.Name)
	if name == "" || len([]rune(name)) > MaxApproverNameRunes || strings.ContainsAny(name, "\r\n\t") {
		return fmt.Errorf("%w: a name is 1 to %d characters on one line", ErrInvalidApprover, MaxApproverNameRunes)
	}
	return nil
}

// EnrollApprover stores cred, proven by code: a live unused enrollment code of
// cred's kind minted for cred's key, consumed here in the same transaction. It
// returns the stored row.
func EnrollApprover(ctx context.Context, db *sql.DB, cred ApproverCredential, code string, now time.Time) (ApproverCredential, error) {
	stored, _, err := enrollApprover(ctx, db, "store.EnrollApprover", cred, code, now, nil)
	return stored, err
}

// RetiredApprover names an enrolled Touch ID credential that an enrollment
// retires: its id, and the public key its holder's request to retire it was
// verified against.
type RetiredApprover struct {
	ID        string
	PublicKey []byte
}

// EnrollTouchIDRetiring stores the Touch ID key cred as EnrollApprover does,
// proven by a live touchid code, and in the same transaction, only once the
// new row is in, removes each credential in retire that is still a Touch ID
// key with exactly that id and public key. The caller has verified that each
// one's own key asked for it (approval.TouchIDReplaceMessage): a Touch ID key
// retires only itself, so this never removes a passkey or a key whose holder
// did not sign. A retiree that no longer matches is left alone. It returns the
// stored row and the ids it removed.
func EnrollTouchIDRetiring(ctx context.Context, db *sql.DB, cred ApproverCredential, code string, now time.Time,
	retire []RetiredApprover) (ApproverCredential, []string, error) {
	const op = "store.EnrollTouchIDRetiring"
	if cred.Kind != ApproverTouchID {
		return ApproverCredential{}, nil, fmt.Errorf("%s: %w: only a Touch ID key retires a credential", op, ErrInvalidApprover)
	}
	return enrollApprover(ctx, db, op, cred, code, now, retire)
}

// enrollApprover is EnrollApprover and EnrollTouchIDRetiring: it stores cred
// and removes the retirees in one transaction, every error wrapped in op.
func enrollApprover(ctx context.Context, db *sql.DB, op string, cred ApproverCredential, code string, now time.Time,
	retire []RetiredApprover) (ApproverCredential, []string, error) {
	stored, retired, err := enrollApproverTx(ctx, db, cred, code, now, retire)
	if err != nil {
		return ApproverCredential{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	return stored, retired, nil
}

func enrollApproverTx(ctx context.Context, db *sql.DB, cred ApproverCredential, code string, now time.Time,
	retire []RetiredApprover) (ApproverCredential, []string, error) {
	cred.Name = strings.TrimSpace(cred.Name)
	if err := validApproverCredential(cred); err != nil {
		return ApproverCredential{}, nil, err
	}
	if strings.TrimSpace(code) == "" {
		return ApproverCredential{}, nil, ErrEnrollProof
	}
	id, err := core.NewID()
	if err != nil {
		return ApproverCredential{}, nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ApproverCredential{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE approver_enroll_codes SET used_at = ?, used_for = ?
		WHERE hash = ? AND kind = ? AND used_at IS NULL AND expires_at > ?`,
		core.FormatTime(now), id, enrollCodeHash(code, ApproverKeyFingerprint(cred.PublicKey)), cred.Kind, core.FormatTime(now))
	if err != nil {
		return ApproverCredential{}, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ApproverCredential{}, nil, err
	}
	if n != 1 {
		return ApproverCredential{}, nil, ErrEnrollCode
	}

	cred.ID = id
	if cred, err = insertApproverTx(ctx, tx, cred, AddedByCode, now); err != nil {
		return ApproverCredential{}, nil, err
	}
	// Retire only after the new row is in, in the same transaction: an
	// enrollment that fails retires nothing.
	var retired []string
	for _, r := range retire {
		res, err := tx.ExecContext(ctx, `DELETE FROM approver_credentials WHERE id = ? AND id <> ? AND kind = ? AND public_key = ?`,
			r.ID, cred.ID, ApproverTouchID, r.PublicKey)
		if err != nil {
			return ApproverCredential{}, nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return ApproverCredential{}, nil, err
		}
		if n == 1 && !slices.Contains(retired, r.ID) {
			retired = append(retired, r.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		return ApproverCredential{}, nil, err
	}
	return cred, retired, nil
}

// insertApproverTx stores cred inside tx as an enrolled credential added by
// addedBy, under cred.ID or a fresh id when it has none. The caller has judged
// the proof (a consumed enrollment code, an approved passkey enrollment); a
// credential of that kind and id already enrolled is ErrApproverExists.
func insertApproverTx(ctx context.Context, tx *sql.Tx, cred ApproverCredential, addedBy string, now time.Time) (ApproverCredential, error) {
	cred.Name = strings.TrimSpace(cred.Name)
	if err := validApproverCredential(cred); err != nil {
		return ApproverCredential{}, err
	}
	if cred.ID == "" {
		id, err := core.NewID()
		if err != nil {
			return ApproverCredential{}, err
		}
		cred.ID = id
	}
	var dup int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approver_credentials WHERE kind = ? AND credential_id = ?`,
		cred.Kind, cred.CredentialID).Scan(&dup); err != nil {
		return ApproverCredential{}, err
	}
	if dup > 0 {
		return ApproverCredential{}, ErrApproverExists
	}
	cred.AddedAt, cred.AddedBy, cred.LastUsedAt = now, addedBy, nil
	if _, err := tx.ExecContext(ctx, `INSERT INTO approver_credentials (id, kind, credential_id, public_key, alg, sign_count,
		backup_eligible, backup_state, aaguid, transports, attachment, attestation_format, user_handle, name, added_at, added_by, rp_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cred.ID, cred.Kind, cred.CredentialID, cred.PublicKey, cred.Alg, int64(cred.SignCount),
		boolInt(cred.BackupEligible), boolInt(cred.BackupState), cred.AAGUID, strings.Join(cred.Transports, ","),
		cred.Attachment, cred.AttestationFormat, cred.UserHandle, cred.Name, core.FormatTime(now), addedBy, cred.RPID); err != nil {
		return ApproverCredential{}, err
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
	transports, attachment, attestation_format, user_handle, name, added_at, added_by, last_used_at, rp_id`

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
		&transports, &c.Attachment, &c.AttestationFormat, &userHand, &c.Name, &addedAt, &c.AddedBy, &lastUsed, &c.RPID); err != nil {
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
// revokes them). The browsers it signed in to the console are signed out
// (RemoveApproverSigningOut).
func RemoveApprover(ctx context.Context, db *sql.DB, id string) error {
	_, err := RemoveApproverSigningOut(ctx, db, id, time.Now())
	return err
}

// RemoveApproverSigningOut is RemoveApprover at now, returning how many
// console sign-ins the credential had made that it ended (reason removed), in
// the same transaction as the delete: a passkey the owner removes, because it
// may be in someone else's hands, keeps no browser signed in.
func RemoveApproverSigningOut(ctx context.Context, db *sql.DB, id string, now time.Time) (int64, error) {
	const op = "store.RemoveApprover"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM approver_credentials WHERE id = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%s: %w: %q", op, ErrApproverNotFound, id)
	}
	signedOut, err := endSignIns(ctx, tx, SignInEndRemoved, now, `credential = ?`, id)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return signedOut, nil
}

// ApproversReset counts what ResetApproversAll ended.
type ApproversReset struct {
	Credentials, Leases int64
	// SignIns are the browsers signed out of the console; Enrollments the
	// passkeys that were waiting for the owner's approval.
	SignIns, Enrollments int64
}

// ResetApprovers deletes every credential and every unused enrollment code,
// revokes every live lease and read grant (reason reset) and ends every
// console unlock, every console sign-in and every passkey waiting for
// approval, in one transaction: what the owner does when a credential may be
// in someone else's hands. It returns how many credentials and leases it
// ended (ResetApproversAll counts the rest).
func ResetApprovers(ctx context.Context, db *sql.DB, now time.Time) (credentials, leases int64, err error) {
	r, err := ResetApproversAll(ctx, db, now)
	return r.Credentials, r.Leases, err
}

// ResetApproversAll is ResetApprovers, counting everything it ended.
func ResetApproversAll(ctx context.Context, db *sql.DB, now time.Time) (ApproversReset, error) {
	const op = "store.ResetApprovers"
	var r ApproversReset
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM approver_credentials`)
	if err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	if r.Credentials, err = res.RowsAffected(); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM approver_enroll_codes WHERE used_at IS NULL`); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	res, err = tx.ExecContext(ctx, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
		WHERE revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), LeaseRevokedReset, core.FormatTime(now))
	if err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	if r.Leases, err = res.RowsAffected(); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	// So was a read grant under peer_auth (isolation.requests): it ends with
	// them too. Its holder stays confined, as after any revoke.
	if _, err := tx.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), LeaseRevokedReset, core.FormatTime(now)); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	// A console unlock was granted by a credential too: it ends with them.
	if _, err := tx.ExecContext(ctx, `UPDATE console_unlocks SET status = 'ended', ended_at = ?, ended_reason = ?,
		claim_hash = '', challenge = NULL WHERE status IN ('live', 'pending')`, core.FormatTime(now), ConsoleUnlockEndReset); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	// So was every console sign-in, and a passkey still waiting would come
	// in after the reset: both end with them.
	if r.SignIns, err = endSignIns(ctx, tx, SignInEndReset, now, `1 = 1`); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	if r.Enrollments, err = endEnrollments(ctx, tx, SignInEndReset, now); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return ApproversReset{}, fmt.Errorf("%s: %w", op, err)
	}
	return r, nil
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
