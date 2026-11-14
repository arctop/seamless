package store

// Paired devices (migration 042; plan networked-relay). Every computer that
// connects to a networked server pairs with it once, through a one-time code,
// and gets a credential of its own:
//
//   - a relay (macOS and Linux clients) proves itself with a TLS client
//     certificate over its own key: the row keeps the public key and its hash,
//     and a request is the relay's only when the key it presented hashes to an
//     active row's key_hash;
//   - a key device (Windows, or a machine where the owner has no root) holds a
//     bearer key of its own: the row keeps only the key's hash.
//
// The code is minted on the server (the console's Devices page, or
// `seamlessd devices code`), lives PairingCodeTTL, and pairs one computer once.
// It is never the long-lived credential. Without peer_auth it is the approval:
// the pairing it takes is active at once (approved_by ''). Under peer_auth the
// pairing waits (pending) until the owner approves it with a passkey, after
// comparing the fingerprint both ends show (ApproveDevice); the code alone
// opens nothing.
//
// The rows are input to the daemon's edge (internal/devices) and never a grant
// on their own: the edge matches a request's credential against an active row
// on every request, so a revoked device is refused at its next request.

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

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// Device kinds.
const (
	// DeviceRelayKind is a client machine's relay: a TLS client certificate over
	// its own key.
	DeviceRelayKind = "relay"
	// DeviceKeyKind is a computer with a bearer key of its own.
	DeviceKeyKind = "key"
)

// DeviceKinds is the canonical kind set.
var DeviceKinds = []string{DeviceRelayKind, DeviceKeyKind}

// Device statuses.
const (
	// DevicePending waits for the owner's passkey (peer_auth only).
	DevicePending = "pending"
	// DeviceActive is paired: its credential is accepted.
	DeviceActive = "active"
	// DeviceDenied is a pending pairing the owner turned down.
	DeviceDenied = "denied"
	// DeviceExpired is a pending pairing nobody approved in time.
	DeviceExpired = "expired"
	// DeviceRevoked is a paired device the owner revoked.
	DeviceRevoked = "revoked"
)

// DeviceStatuses is the canonical status set.
var DeviceStatuses = []string{DevicePending, DeviceActive, DeviceDenied, DeviceExpired, DeviceRevoked}

// Where a code was minted or a device was ended from.
const (
	DeviceByConsole  = "console"
	DeviceByTerminal = "terminal"
	// DeviceByRestore is a device a restored database brought back, revoked
	// on the way in (archive.Import).
	DeviceByRestore = "restore"
)

// DeviceBys is the canonical set of minted_by and revoked_by values.
var DeviceBys = []string{DeviceByConsole, DeviceByTerminal, DeviceByRestore}

const (
	// PairingCodeTTL is how long a pairing code stays valid: time to carry it
	// to the other computer and enter it there.
	PairingCodeTTL = 10 * time.Minute
	// DevicePendingTTL is how long a pairing waits for the owner's approval.
	DevicePendingTTL = 10 * time.Minute
	// pairingCodeLen is 16 Crockford base32 symbols, 80 bits: without
	// peer_auth a code is the approval, so it must not be guessable within
	// its lifetime.
	pairingCodeLen = 16
	// maxOpenPairingCodes caps the codes waiting to be used at once, so a
	// caller looping on the mint cannot grow the table without bound.
	maxOpenPairingCodes = 16
	// pairingCodeKeep is how long a used or expired code stays on record.
	pairingCodeKeep = 30 * 24 * time.Hour
	// deviceTouchEvery is how often a device's last_used_at moves: at most
	// one write a minute per device, whatever its request rate.
	deviceTouchEvery = time.Minute
	// MaxDeviceNameLen is a host name's length limit.
	MaxDeviceNameLen = 253
	// maxKeyPrefixLen bounds key_prefix.
	maxKeyPrefixLen = 32
)

var (
	// ErrPairingCode is a pairing code that is malformed, absent, expired or
	// already used. One error for all, so a caller learns nothing about which.
	ErrPairingCode = errors.New("pairing code is not valid")
	// ErrTooManyPairingCodes is a mint refused while maxOpenPairingCodes wait.
	ErrTooManyPairingCodes = errors.New("too many pairing codes are waiting to be used")
	// ErrDeviceNotFound is a device id, secret or key that names no row.
	ErrDeviceNotFound = errors.New("device not found")
	// ErrDeviceNameTaken is a pairing under a name a pending or active device
	// holds: revoke that one first.
	ErrDeviceNameTaken = errors.New("a device of that name is already paired or waiting")
	// ErrDeviceNameReserved is a pairing under the server's own host name.
	ErrDeviceNameReserved = errors.New("that is the server's own name")
	// ErrDeviceKeyTaken is a pairing of a key a pending or active device holds.
	ErrDeviceKeyTaken = errors.New("that key is already paired or waiting")
	// ErrDeviceNotPending is an approval of a device that is not waiting:
	// already approved, denied, revoked, or past its deadline.
	ErrDeviceNotPending = errors.New("that device is not waiting for approval")
	// ErrDeviceEnded is a revocation of a device already ended.
	ErrDeviceEnded = errors.New("that device is already revoked or was never paired")
	// ErrDeviceSecretUsed is a pairing with a secret an earlier pairing
	// chose: a device draws a fresh one for every pairing.
	ErrDeviceSecretUsed = errors.New("that pairing secret was used before: draw a new one")
	// ErrDeviceKeyCollected is a key collection for a device that has no key
	// to give: not a key device, not active, or its key already collected.
	ErrDeviceKeyCollected = errors.New("this device has no key waiting to be collected")
	// ErrInvalidDevice is a malformed device argument.
	ErrInvalidDevice = errors.New("invalid device")
)

// Device is one row of devices.
type Device struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Fingerprint is what both ends show the owner, XXXX-XXXX-XXXX-XXXX.
	Fingerprint string `json:"fingerprint"`
	// PublicKey is a relay's DER SubjectPublicKeyInfo; nil for a key device.
	PublicKey []byte `json:"-"`
	// KeyPrefix is the start of a key device's key; HasKey says the device
	// collected one (a relay always has its key).
	KeyPrefix    string     `json:"key_prefix,omitempty"`
	HasKey       bool       `json:"has_key"`
	CodeID       string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	PendingUntil time.Time  `json:"pending_until"`
	EnrolledAt   *time.Time `json:"enrolled_at,omitempty"`
	// ApprovedBy is the passkey that approved it; "" for a pairing the code
	// alone approved.
	ApprovedBy string     `json:"approved_by,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	RevokedBy  string     `json:"revoked_by,omitempty"`
}

// Live reports whether the device is pending or active at now: a pending one
// past its deadline is not.
func (d Device) Live(now time.Time) bool {
	switch d.Status {
	case DeviceActive:
		return true
	case DevicePending:
		return now.Before(d.PendingUntil)
	}
	return false
}

// StatusAt is the device's status as of now: a pending row past its deadline
// reads as expired, though the row still says pending until the next pairing
// sweeps it.
func (d Device) StatusAt(now time.Time) string {
	if d.Status == DevicePending && !now.Before(d.PendingUntil) {
		return DeviceExpired
	}
	return d.Status
}

// NewDevice is what a pairing records.
type NewDevice struct {
	Kind string
	Name string
	// Fingerprint is what both ends show (FormatKeyFingerprint form).
	Fingerprint string
	// PublicKey is a relay's DER SubjectPublicKeyInfo; nil for a key device.
	PublicKey []byte
	// KeyHash is a relay's public key hash, or the hash of the key a key
	// device paired with at once; "" for a key device that waits.
	KeyHash   string
	KeyPrefix string
	// SecretHash is the hash of the pairing secret the device chose.
	SecretHash string
	// Pending makes the pairing wait for the owner's approval (peer_auth);
	// otherwise the code approves it and it is active at once.
	Pending bool
	// Reserved is the server's own host name, which no device may take
	// (ErrDeviceNameReserved); "" reserves none. It is judged after the code,
	// so a caller without one learns nothing of it.
	Reserved string
}

// ValidDeviceName checks a device's name: a host name as config.Hostname
// reports one, lower-cased, of 1 to MaxDeviceNameLen characters drawn from
// letters, digits, '-', '_' and '.', with no empty label and no label that
// starts or ends with '-'. It becomes the host every request of the device
// speaks for, so it is held to the shape of one.
func ValidDeviceName(name string) error {
	if name == "" || len(name) > MaxDeviceNameLen {
		return fmt.Errorf("%w: a device name is 1 to %d characters", ErrInvalidDevice, MaxDeviceNameLen)
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" {
			return fmt.Errorf("%w: device name %q has an empty label", ErrInvalidDevice, name)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w: device name %q has a label that starts or ends with '-'", ErrInvalidDevice, name)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return fmt.Errorf("%w: device name %q: a name holds lower-case letters, digits, '-', '_' and '.'", ErrInvalidDevice, name)
			}
		}
	}
	return nil
}

func validDeviceKind(kind string) error {
	if !slices.Contains(DeviceKinds, kind) {
		return fmt.Errorf("%w: kind %q: valid kinds are %s", ErrInvalidDevice, kind, strings.Join(DeviceKinds, ", "))
	}
	return nil
}

func validDeviceBy(by string) error {
	if !slices.Contains(DeviceBys, by) {
		return fmt.Errorf("%w: %q: valid values are %s", ErrInvalidDevice, by, strings.Join(DeviceBys, ", "))
	}
	return nil
}

// validHexHash reports a 64-character lower-case hex string: a SHA-256.
func validHexHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (d NewDevice) validate() error {
	if err := validDeviceKind(d.Kind); err != nil {
		return err
	}
	if err := ValidDeviceName(d.Name); err != nil {
		return err
	}
	if FormatKeyFingerprint(d.Fingerprint) != d.Fingerprint || d.Fingerprint == "" {
		return fmt.Errorf("%w: fingerprint %q is not XXXX-XXXX-XXXX-XXXX", ErrInvalidDevice, d.Fingerprint)
	}
	if !validHexHash(d.SecretHash) {
		return fmt.Errorf("%w: a pairing needs its secret's hash", ErrInvalidDevice)
	}
	if len(d.KeyPrefix) > maxKeyPrefixLen {
		return fmt.Errorf("%w: key prefix too long", ErrInvalidDevice)
	}
	switch d.Kind {
	case DeviceRelayKind:
		if len(d.PublicKey) == 0 || !validHexHash(d.KeyHash) || d.KeyPrefix != "" {
			return fmt.Errorf("%w: a relay pairs with its public key and that key's hash", ErrInvalidDevice)
		}
	case DeviceKeyKind:
		if len(d.PublicKey) != 0 {
			return fmt.Errorf("%w: a key device has no public key", ErrInvalidDevice)
		}
		if d.Pending && (d.KeyHash != "" || d.KeyPrefix != "") {
			return fmt.Errorf("%w: a key device that waits for approval collects its key later", ErrInvalidDevice)
		}
		if !d.Pending && (!validHexHash(d.KeyHash) || d.KeyPrefix == "") {
			return fmt.Errorf("%w: a key device paired at once needs its key's hash and prefix", ErrInvalidDevice)
		}
	}
	return nil
}

// pairingCodeHash is what code_hash stores of a code: hex SHA-256 of its
// canonical form under a prefix no other code uses.
func pairingCodeHash(code string) string {
	sum := sha256.Sum256([]byte("seamless device pairing code v1\x00" + NormalizeEnrollCode(code)))
	return hex.EncodeToString(sum[:])
}

// FormatPairingCode prints a pairing code as typed in its canonical form,
// XXXX-XXXX-XXXX-XXXX, or returns "" when it is not one.
func FormatPairingCode(code string) string {
	norm := NormalizeEnrollCode(code)
	if len(norm) != pairingCodeLen || strings.Trim(norm, enrollCodeSymbols) != "" {
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

// newPairingCode draws a fresh code. A variable so a test can force a
// collision.
var newPairingCode = func() (string, error) {
	raw := make([]byte, pairingCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	b := make([]byte, pairingCodeLen)
	for i, v := range raw {
		// 256 is a multiple of the 32 symbols, so every symbol is as likely.
		b[i] = enrollCodeSymbols[int(v)%len(enrollCodeSymbols)]
	}
	return FormatPairingCode(string(b)), nil
}

// MintPairingCode creates a one-time pairing code, valid for PairingCodeTTL
// from now, minted from by (console or terminal), and returns it formatted
// for a person (XXXX-XXXX-XXXX-XXXX) with its expiry. Only its hash is stored.
// Codes that ended more than pairingCodeKeep ago are pruned first; at most
// maxOpenPairingCodes wait to be used at once.
func MintPairingCode(ctx context.Context, db *sql.DB, by string, now time.Time) (string, time.Time, error) {
	const op = "store.MintPairingCode"
	if by != DeviceByConsole && by != DeviceByTerminal {
		return "", time.Time{}, fmt.Errorf("%s: %w: minted_by %q: valid values are console, terminal", op, ErrInvalidDevice, by)
	}
	id, err := core.NewID()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", op, err)
	}
	now = now.UTC()
	at := core.FormatTime(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_pairing_codes WHERE expires_at <= ?`,
		core.FormatTime(now.Add(-pairingCodeKeep))); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: prune: %w", op, err)
	}
	var open int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_pairing_codes WHERE used_at IS NULL AND expires_at > ?`,
		at).Scan(&open); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", op, err)
	}
	if open >= maxOpenPairingCodes {
		return "", time.Time{}, fmt.Errorf("%s: %w", op, ErrTooManyPairingCodes)
	}
	code := ""
	for range unlockCodeTries {
		c, err := newPairingCode()
		if err != nil {
			return "", time.Time{}, fmt.Errorf("%s: code: %w", op, err)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_pairing_codes WHERE code_hash = ?`,
			pairingCodeHash(c)).Scan(&taken); err != nil {
			return "", time.Time{}, fmt.Errorf("%s: %w", op, err)
		}
		if taken == 0 {
			code = c
			break
		}
	}
	if code == "" {
		return "", time.Time{}, fmt.Errorf("%s: no free code after %d tries", op, unlockCodeTries)
	}
	expires := now.Add(PairingCodeTTL)
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_pairing_codes (id, code_hash, minted_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, id, pairingCodeHash(code), by, at, core.FormatTime(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", op, err)
	}
	return code, expires, nil
}

// CountOpenPairingCodes counts the codes still waiting to be used at now.
func CountOpenPairingCodes(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_pairing_codes WHERE used_at IS NULL AND expires_at > ?`,
		core.FormatTime(now)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountOpenPairingCodes: %w", err)
	}
	return n, nil
}

const deviceCols = `id, kind, name, status, fingerprint, public_key, key_hash, key_prefix, code_id, created_at, pending_until,
	enrolled_at, approved_by, last_used_at, revoked_at, revoked_by`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var (
		d                         Device
		keyHash                   sql.NullString
		created, pending          string
		enrolled, used, revokedAt sql.NullString
		publicKey                 []byte
	)
	if err := row.Scan(&d.ID, &d.Kind, &d.Name, &d.Status, &d.Fingerprint, &publicKey, &keyHash, &d.KeyPrefix, &d.CodeID,
		&created, &pending, &enrolled, &d.ApprovedBy, &used, &revokedAt, &d.RevokedBy); err != nil {
		return Device{}, err
	}
	d.PublicKey = publicKey
	d.HasKey = keyHash.Valid && keyHash.String != ""
	var err error
	if d.CreatedAt, err = core.ParseTime(created); err != nil {
		return Device{}, fmt.Errorf("device %s created_at: %w", d.ID, err)
	}
	if d.PendingUntil, err = core.ParseTime(pending); err != nil {
		return Device{}, fmt.Errorf("device %s pending_until: %w", d.ID, err)
	}
	for _, f := range []struct {
		name string
		src  sql.NullString
		dst  **time.Time
	}{{"enrolled_at", enrolled, &d.EnrolledAt}, {"last_used_at", used, &d.LastUsedAt}, {"revoked_at", revokedAt, &d.RevokedAt}} {
		if !f.src.Valid {
			continue
		}
		t, err := core.ParseTime(f.src.String)
		if err != nil {
			return Device{}, fmt.Errorf("device %s %s: %w", d.ID, f.name, err)
		}
		*f.dst = &t
	}
	return d, nil
}

// deviceQueryer is the read half a *sql.DB and a *sql.Tx share.
type deviceQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func deviceWhere(ctx context.Context, q deviceQueryer, where string, args ...any) (Device, error) {
	d, err := scanDevice(q.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrDeviceNotFound
	}
	return d, err
}

// expireDevices marks the pending devices past their deadline at at as
// expired, which frees their names and keys.
func expireDevices(ctx context.Context, tx *sql.Tx, at string) error {
	_, err := tx.ExecContext(ctx, `UPDATE devices SET status = ? WHERE status = ? AND pending_until <= ?`,
		DeviceExpired, DevicePending, at)
	return err
}

// PairDevice takes code and records d, in one transaction: the code is used
// (it pairs this device and no other), and the device waits for the owner
// (d.Pending) or is active at once. A code that is malformed, absent, expired
// or used is ErrPairingCode; a name or a key a live device holds is
// ErrDeviceNameTaken or ErrDeviceKeyTaken, and then the code stays unused. Of
// two pairings racing for one code exactly one succeeds.
func PairDevice(ctx context.Context, db *sql.DB, code string, d NewDevice, now time.Time) (Device, error) {
	const op = "store.PairDevice"
	if FormatPairingCode(code) == "" {
		return Device{}, fmt.Errorf("%s: %w", op, ErrPairingCode)
	}
	if err := d.validate(); err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	id, err := core.NewID()
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	now = now.UTC()
	at := core.FormatTime(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	// The code first, before anything is written: a caller with a code that
	// pairs nothing (the pairing endpoint needs no other credential) only
	// reads.
	var codeID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM device_pairing_codes WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`,
		pairingCodeHash(code), at).Scan(&codeID)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, fmt.Errorf("%s: %w", op, ErrPairingCode)
	}
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := expireDevices(ctx, tx, at); err != nil {
		return Device{}, fmt.Errorf("%s: expire: %w", op, err)
	}
	var names, keys, secrets int
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status IN (?, ?) AND name = ?), 0),
		COALESCE(SUM(status IN (?, ?) AND key_hash IS NOT NULL AND key_hash = ?), 0),
		COALESCE(SUM(secret_hash = ?), 0) FROM devices`,
		DevicePending, DeviceActive, d.Name, DevicePending, DeviceActive, d.KeyHash, d.SecretHash).Scan(&names, &keys, &secrets); err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	switch {
	case d.Reserved != "" && d.Name == d.Reserved:
		return Device{}, fmt.Errorf("%s: %w", op, ErrDeviceNameReserved)
	case names > 0:
		return Device{}, fmt.Errorf("%s: %w", op, ErrDeviceNameTaken)
	case keys > 0 && d.KeyHash != "":
		return Device{}, fmt.Errorf("%s: %w", op, ErrDeviceKeyTaken)
	case secrets > 0:
		return Device{}, fmt.Errorf("%s: %w", op, ErrDeviceSecretUsed)
	}
	res, err := tx.ExecContext(ctx, `UPDATE device_pairing_codes SET used_at = ?, used_by = ? WHERE id = ? AND used_at IS NULL`,
		at, id, codeID)
	if err != nil {
		return Device{}, fmt.Errorf("%s: use code: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Device{}, fmt.Errorf("%s: use code: %w", op, err)
	}
	if n != 1 {
		return Device{}, fmt.Errorf("%s: %w", op, ErrPairingCode)
	}
	status, pendingUntil := DeviceActive, now
	var enrolled any = at
	if d.Pending {
		status, pendingUntil, enrolled = DevicePending, now.Add(DevicePendingTTL), nil
	}
	var keyHash any
	if d.KeyHash != "" {
		keyHash = d.KeyHash
	}
	var publicKey any
	if len(d.PublicKey) > 0 {
		publicKey = d.PublicKey
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices (id, kind, name, status, fingerprint, public_key, key_hash, key_prefix,
		secret_hash, code_id, created_at, pending_until, enrolled_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, d.Kind, d.Name, status, d.Fingerprint, publicKey, keyHash, d.KeyPrefix, d.SecretHash, codeID, at,
		core.FormatTime(pendingUntil), enrolled); err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	out, err := deviceWhere(ctx, tx, `id = ?`, id)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// DeviceByID returns the device id names, ErrDeviceNotFound for none.
func DeviceByID(ctx context.Context, db *sql.DB, id string) (Device, error) {
	d, err := deviceWhere(ctx, db, `id = ?`, id)
	if err != nil {
		return Device{}, fmt.Errorf("store.DeviceByID: %w", err)
	}
	return d, nil
}

// DeviceBySecret returns the device whose pairing secret hashes to
// secretHash: the device asking after its own pairing.
func DeviceBySecret(ctx context.Context, db *sql.DB, secretHash string) (Device, error) {
	if !validHexHash(secretHash) {
		return Device{}, fmt.Errorf("store.DeviceBySecret: %w", ErrDeviceNotFound)
	}
	d, err := deviceWhere(ctx, db, `secret_hash = ?`, secretHash)
	if err != nil {
		return Device{}, fmt.Errorf("store.DeviceBySecret: %w", err)
	}
	return d, nil
}

// DeviceByKey returns the device of kind whose credential hashes to keyHash:
// the live one (pending or active) when there is one, else the one ended most
// recently, so a caller can tell a revoked credential from one never paired.
// ErrDeviceNotFound when none ever held it.
func DeviceByKey(ctx context.Context, db *sql.DB, kind, keyHash string) (Device, error) {
	const op = "store.DeviceByKey"
	if validDeviceKind(kind) != nil || !validHexHash(keyHash) {
		return Device{}, fmt.Errorf("%s: %w", op, ErrDeviceNotFound)
	}
	d, err := deviceWhere(ctx, db, `kind = ? AND key_hash = ?
		ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END, created_at DESC, id DESC LIMIT 1`,
		kind, keyHash)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	return d, nil
}

// ListDevices returns every device: the live ones first (waiting, then
// paired, newest first), then the ended ones, newest ended first. It only
// reads: a pending row past its deadline still says pending (StatusAt).
func ListDevices(ctx context.Context, db *sql.DB) ([]Device, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices
		ORDER BY CASE status WHEN 'pending' THEN 0 WHEN 'active' THEN 1 ELSE 2 END,
		COALESCE(revoked_at, enrolled_at, created_at) DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.ListDevices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("store.ListDevices: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListDevices: %w", err)
	}
	return out, nil
}

// explainDevice turns a compare-and-set that matched nothing into the reason:
// the row is absent, or not in the state the change needs.
func explainDevice(ctx context.Context, db *sql.DB, id string, notThere error) error {
	_, err := deviceWhere(ctx, db, `id = ?`, id)
	if errors.Is(err, ErrDeviceNotFound) {
		return ErrDeviceNotFound
	}
	if err != nil {
		return err
	}
	return notThere
}

// ApproveDevice makes the pending device id active, approved by credential
// (the passkey whose assertion the caller verified): a compare-and-set on the
// row still pending before its deadline at now and still showing fingerprint,
// the fingerprint the owner's assertion was bound to. Of two racing approvals
// exactly one succeeds. A device that is not waiting is ErrDeviceNotPending.
func ApproveDevice(ctx context.Context, db *sql.DB, id, fingerprint, credential string, now time.Time) (Device, error) {
	const op = "store.ApproveDevice"
	if strings.TrimSpace(credential) == "" {
		return Device{}, fmt.Errorf("%s: %w: an approval names the credential that proved it", op, ErrInvalidDevice)
	}
	at := core.FormatTime(now)
	res, err := db.ExecContext(ctx, `UPDATE devices SET status = ?, enrolled_at = ?, approved_by = ?
		WHERE id = ? AND status = ? AND pending_until > ? AND fingerprint = ?`,
		DeviceActive, at, credential, id, DevicePending, at, fingerprint)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return Device{}, fmt.Errorf("%s: %w", op, explainDevice(ctx, db, id, ErrDeviceNotPending))
	}
	d, err := DeviceByID(ctx, db, id)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	return d, nil
}

// CollectDeviceKey gives the active key device whose pairing secret hashes to
// secretHash the key that hashes to keyHash: a compare-and-set on the device
// having none yet, so a key is collected once and the server never holds it.
// A device with no key waiting is ErrDeviceKeyCollected.
func CollectDeviceKey(ctx context.Context, db *sql.DB, secretHash, keyHash, keyPrefix string) (Device, error) {
	const op = "store.CollectDeviceKey"
	if !validHexHash(secretHash) || !validHexHash(keyHash) || keyPrefix == "" || len(keyPrefix) > maxKeyPrefixLen {
		return Device{}, fmt.Errorf("%s: %w", op, ErrInvalidDevice)
	}
	res, err := db.ExecContext(ctx, `UPDATE devices SET key_hash = ?, key_prefix = ?
		WHERE secret_hash = ? AND kind = ? AND status = ? AND key_hash IS NULL`,
		keyHash, keyPrefix, secretHash, DeviceKeyKind, DeviceActive)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return Device{}, fmt.Errorf("%s: %w", op, ErrDeviceKeyCollected)
	}
	d, err := deviceWhere(ctx, db, `secret_hash = ?`, secretHash)
	if err != nil {
		return Device{}, fmt.Errorf("%s: %w", op, err)
	}
	return d, nil
}

// RevokeDevice ends the device id at now, from by: an active device is
// revoked, a pending one denied. Its credential is refused from the next
// request on, and a relay's access ends with it (RevokeDeviceEndingAccess). A
// device already ended is ErrDeviceEnded. It returns the row as it now stands.
func RevokeDevice(ctx context.Context, db *sql.DB, id, by string, now time.Time) (Device, error) {
	d, _, err := RevokeDeviceEndingAccess(ctx, db, id, by, now)
	return d, err
}

// DeviceAccessEnded counts what revoking a relay ended with it: its processes'
// live leases and grants, and the access requests they were still waiting on.
type DeviceAccessEnded struct {
	Leases, Grants, Requests int64
}

// Total is how many permissions and requests were ended in all.
func (e DeviceAccessEnded) Total() int64 { return e.Leases + e.Grants + e.Requests }

// RevokeDeviceEndingAccess is RevokeDevice, and in the same transaction it
// ends everything the device's processes still hold: a relay's live leases
// and grants (relay_id, migration 043; revoked as LeaseRevokedDevice) and its
// requests still waiting for a decision (every process key of the relay
// starts with access.RelayKeyPrefix), marked expired so no proof can turn one
// into a lease later. The revocation and the endings commit together or not
// at all, so a relay is never recorded revoked with its access still open
// behind it. A key device holds no lease, grant or request of its own: there
// is nothing more to end.
func RevokeDeviceEndingAccess(ctx context.Context, db *sql.DB, id, by string, now time.Time) (Device, DeviceAccessEnded, error) {
	const op = "store.RevokeDevice"
	var ended DeviceAccessEnded
	if err := validDeviceBy(by); err != nil {
		return Device{}, ended, fmt.Errorf("%s: %w", op, err)
	}
	at := core.FormatTime(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, ended, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE devices SET status = CASE status WHEN ? THEN ? ELSE ? END, revoked_at = ?, revoked_by = ?
		WHERE id = ? AND status IN (?, ?)`,
		DevicePending, DeviceDenied, DeviceRevoked, at, by, id, DevicePending, DeviceActive)
	if err != nil {
		return Device{}, ended, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Device{}, ended, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		if err := tx.Rollback(); err != nil {
			return Device{}, ended, fmt.Errorf("%s: %w", op, err)
		}
		return Device{}, ended, fmt.Errorf("%s: %w", op, explainDevice(ctx, db, id, ErrDeviceEnded))
	}
	d, err := deviceWhere(ctx, tx, `id = ?`, id)
	if err != nil {
		return Device{}, ended, fmt.Errorf("%s: %w", op, err)
	}
	if d.Kind == DeviceRelayKind {
		if ended, err = endRelayAccessTx(ctx, tx, []string{d.ID}, at); err != nil {
			return Device{}, DeviceAccessEnded{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Device{}, DeviceAccessEnded{}, fmt.Errorf("%s: %w", op, err)
	}
	return d, ended, nil
}

// liveRelayIDsTx lists the relays still pending or active, inside tx.
func liveRelayIDsTx(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM devices WHERE kind = ? AND status IN (?, ?) ORDER BY id`,
		DeviceRelayKind, DevicePending, DeviceActive)
	if err != nil {
		return nil, fmt.Errorf("live relays: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("live relays: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("live relays: %w", err)
	}
	return out, nil
}

// endRelayAccessTx ends, inside tx at at, the live leases and grants of the
// relays' processes and their requests still waiting for a decision.
func endRelayAccessTx(ctx context.Context, tx *sql.Tx, relays []string, at string) (DeviceAccessEnded, error) {
	var ended DeviceAccessEnded
	for _, relay := range relays {
		if relay == "" {
			continue
		}
		prefix := access.RelayKeyPrefix(relay)
		for _, e := range []struct {
			n    *int64
			q    string
			args []any
		}{
			{&ended.Leases, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
				WHERE relay_id = ? AND revoked_at IS NULL AND expires_at > ?`, []any{at, LeaseRevokedDevice, relay, at}},
			{&ended.Grants, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
				WHERE relay_id = ? AND revoked_at IS NULL AND expires_at > ?`, []any{at, LeaseRevokedDevice, relay, at}},
			// substr, not LIKE: the prefix is compared as it is, with no pattern
			// characters to escape.
			{&ended.Requests, `UPDATE access_approvals SET status = ?, decided_at = ?
				WHERE status = ? AND substr(process_key, 1, ?) = ?`,
				[]any{ApprovalExpired, at, ApprovalPending, len(prefix), prefix}},
		} {
			res, err := tx.ExecContext(ctx, e.q, e.args...)
			if err != nil {
				return DeviceAccessEnded{}, fmt.Errorf("end the relay's access: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return DeviceAccessEnded{}, fmt.Errorf("end the relay's access: %w", err)
			}
			*e.n += n
		}
	}
	return ended, nil
}

// RevokeAllDevices ends every pending and active device at now, from by, and
// voids every pairing code still waiting to be used, returning how many
// devices ended. A restored database brings its devices back as they stood
// when the snapshot was taken, revoked ones included; archive.Import calls
// this so none of them is accepted until it pairs again. Each relay's access
// ends with it, in the same transaction, as RevokeDeviceEndingAccess ends it.
func RevokeAllDevices(ctx context.Context, db *sql.DB, by string, now time.Time) (int64, error) {
	const op = "store.RevokeAllDevices"
	if err := validDeviceBy(by); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	at := core.FormatTime(now)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	relays, err := liveRelayIDsTx(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := endRelayAccessTx(ctx, tx, relays, at); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE devices SET status = CASE status WHEN ? THEN ? ELSE ? END, revoked_at = ?, revoked_by = ?
		WHERE status IN (?, ?)`, DevicePending, DeviceDenied, DeviceRevoked, at, by, DevicePending, DeviceActive)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE device_pairing_codes SET expires_at = ? WHERE used_at IS NULL AND expires_at > ?`,
		at, at); err != nil {
		return 0, fmt.Errorf("%s: void codes: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return n, nil
}

// TouchDevice records the active device id's use at now, at most once every
// deviceTouchEvery: a request that finds the last use recent writes nothing.
func TouchDevice(ctx context.Context, db *sql.DB, id string, now time.Time) error {
	if _, err := db.ExecContext(ctx, `UPDATE devices SET last_used_at = ?
		WHERE id = ? AND status = ? AND (last_used_at IS NULL OR last_used_at <= ?)`,
		core.FormatTime(now), id, DeviceActive, core.FormatTime(now.Add(-deviceTouchEvery))); err != nil {
		return fmt.Errorf("store.TouchDevice: %w", err)
	}
	return nil
}

// DeviceCounts are the devices by kind and state, for doctor.
type DeviceCounts struct {
	Relays, Keys, Pending int
}

// CountDevices counts the active devices by kind and the pending ones still
// before their deadline at now.
func CountDevices(ctx context.Context, db *sql.DB, now time.Time) (DeviceCounts, error) {
	var c DeviceCounts
	if err := db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status = ? AND kind = ?), 0), COALESCE(SUM(status = ? AND kind = ?), 0),
		COALESCE(SUM(status = ? AND pending_until > ?), 0) FROM devices`,
		DeviceActive, DeviceRelayKind, DeviceActive, DeviceKeyKind, DevicePending, core.FormatTime(now)).Scan(&c.Relays, &c.Keys, &c.Pending); err != nil {
		return DeviceCounts{}, fmt.Errorf("store.CountDevices: %w", err)
	}
	return c, nil
}
