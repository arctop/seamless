package approval

// Console sign-in with a passkey (console.sign_in: passkey). A browser signs
// in to the console with a passkey the owner enrolled, so no browser needs the
// bearer key, and a new device adds its passkey through an enrollment the
// owner approves on the server.
//
// Both ceremonies are stateless until they succeed. Begin hands the browser a
// ticket (a deadline and a nonce) and the options over a challenge that is a
// keyed hash of the ticket under a secret only this process holds (challenge);
// finish recomputes it from the ticket the browser sends back. Nothing is
// stored per begin, so a caller looping on a begin route, which needs no
// credential, fills no table and blocks no one. A sign-in's ticket works once:
// the first finish whose assertion verifies claims its nonce. An enrollment's
// needs no claim: the store takes one credential once.
//
//   - Sign-in binds its challenge to the browser's key (browserproof), so an
//     assertion signs in the browser that began it and no other, and the
//     sign-in row binds the new cookie to that key (store.HashConsoleSignIn).
//   - Enrollment binds its challenge to the passkey's name, then records the
//     new credential as waiting for the owner (store.BeginPasskeyEnrollment)
//     with a short code the device shows. Only `seamlessd approvers approve`,
//     run on the server as the daemon's account under peer_auth, enrolls it
//     (store.ApprovePasskeyEnrollment): this service has no approve, and no
//     enrolled passkey vouches for a new one (F2).

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// SignInLifetime is how long a console sign-in lasts before the browser signs
// in again, unless SignInConfig.Lifetime says otherwise.
const SignInLifetime = 7 * 24 * time.Hour

// signInTicketTTL is how long a begun sign-in waits for its assertion, and
// enrollTicketTTL how long a begun enrollment waits for its passkey.
const (
	signInTicketTTL = ConsoleUnlockTTL
	enrollTicketTTL = EnrollTTL
)

// signInNonceBytes is the entropy of a ticket's nonce.
const signInNonceBytes = 16

var (
	// ErrSignInTicket is a finish whose ticket is malformed, past its
	// deadline, already used, or from before this daemon started.
	ErrSignInTicket = errors.New("this sign-in or enrollment expired or was already used")
	// ErrNoBrowserKey is a sign-in ceremony without the browser's key: the
	// sign-in is bound to it, so there is nothing to begin or finish.
	ErrNoBrowserKey = errors.New("a console sign-in needs the browser's key")
)

// SignInConfig wires a SignIn.
type SignInConfig struct {
	DB *sql.DB
	// Passkeys is the relying party (config.RelyingParty); required.
	Passkeys *passkey.Verifier
	// Origins are the relying party's origins, for the pages that must send
	// the owner there.
	Origins []string
	// UserName and UserDisplayName are what an authenticator shows for the
	// owner's account, as the approval service names it.
	UserName, UserDisplayName string
	// Lifetime is how long a sign-in lasts; 0 means SignInLifetime.
	Lifetime time.Duration
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// SignIn runs the console's passkey sign-in and the Add a passkey page's
// enrollment. It is safe for concurrent use.
type SignIn struct {
	cfg    SignInConfig
	secret []byte
	mu     sync.Mutex
	// used are the nonces of tickets a finish already used, with their
	// deadlines, so a ticket works once; an entry goes when its deadline
	// passes, as the ticket itself does.
	used map[string]int64
}

// NewSignIn builds a SignIn with a fresh secret for its tickets.
func NewSignIn(cfg SignInConfig) (*SignIn, error) {
	if cfg.DB == nil || cfg.Passkeys == nil {
		return nil, fmt.Errorf("approval.NewSignIn: %w: a database and a relying party are required", ErrPasskeysOff)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.UserName == "" {
		cfg.UserName = "owner"
	}
	if cfg.Lifetime <= 0 {
		cfg.Lifetime = SignInLifetime
	}
	secret := make([]byte, sha256.Size)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("approval.NewSignIn: %w", err)
	}
	cfg.Origins = slices.Clone(cfg.Origins)
	return &SignIn{cfg: cfg, secret: secret, used: map[string]int64{}}, nil
}

// Origins returns the relying party's origins: where the sign-in and the Add
// a passkey pages work.
func (s *SignIn) Origins() []string { return slices.Clone(s.cfg.Origins) }

// RPID returns the relying party a passkey must belong to.
func (s *SignIn) RPID() string { return s.cfg.Passkeys.RPID() }

// Lifetime returns how long a sign-in lasts.
func (s *SignIn) Lifetime() time.Duration { return s.cfg.Lifetime }

// Ticketed is a begun ceremony: the ticket the browser sends back with its
// answer, and the options for navigator.credentials.get or create.
type Ticketed struct {
	Ticket  string          `json:"ticket"`
	Options json.RawMessage `json:"options"`
}

// SignedIn is a browser that just signed in: the cookie to give it (its only
// copy; the row keeps a hash), the row, and the passkey that signed.
type SignedIn struct {
	Cookie     string
	SignIn     store.ConsoleSignIn
	Credential store.ApproverCredential
}

// PasskeyAdded is a passkey a device created, waiting for the owner: the
// enrollment's id (the page asks after it), the code the owner approves, and
// what the owner compares on both screens.
type PasskeyAdded struct {
	ID           string    `json:"id"`
	Code         string    `json:"code"`
	Name         string    `json:"name"`
	Fingerprint  string    `json:"fingerprint"`
	PendingUntil time.Time `json:"pending_until"`
}

// newTicket returns a ticket good for ttl from now, its deadline and nonce.
func newTicket(now time.Time, ttl time.Duration) (string, int64, []byte, error) {
	nonce := make([]byte, signInNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, nil, err
	}
	deadline := now.Add(ttl).Unix()
	return "v1." + strconv.FormatInt(deadline, 10) + "." + base64.RawURLEncoding.EncodeToString(nonce), deadline, nonce, nil
}

// parseTicket reads a ticket still good at now: its deadline and nonce.
func parseTicket(ticket string, now time.Time) (int64, []byte, error) {
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, nil, ErrSignInTicket
	}
	deadline, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || parts[1] != strconv.FormatInt(deadline, 10) {
		return 0, nil, ErrSignInTicket
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(nonce) != signInNonceBytes {
		return 0, nil, ErrSignInTicket
	}
	if now.Unix() >= deadline {
		return 0, nil, ErrSignInTicket
	}
	return deadline, nonce, nil
}

// challenge is the 32 bytes a ceremony's authenticator signs: a keyed hash of
// the purpose, the ticket's deadline and nonce, and what the ceremony is
// bound to (the browser's key; the passkey's name), under this process's
// secret. Only this process can make it, and it changes with any part.
func (s *SignIn) challenge(purpose string, deadline int64, nonce []byte, bound string) []byte {
	m := hmac.New(sha256.New, s.secret)
	for _, part := range [][]byte{[]byte("seamless console " + purpose + " v1"), []byte(strconv.FormatInt(deadline, 10)), nonce, []byte(bound)} {
		m.Write(part)
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// claim marks a ticket's nonce used, refusing one used already. Entries past
// their deadline go first: their tickets are refused by parseTicket anyway.
func (s *SignIn) claim(nonce []byte, deadline int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, d := range s.used {
		if now.Unix() >= d {
			delete(s.used, k)
		}
	}
	key := string(nonce)
	if _, ok := s.used[key]; ok {
		return ErrSignInTicket
	}
	s.used[key] = deadline
	return nil
}

func (s *SignIn) passkeyRows(ctx context.Context) ([]store.ApproverCredential, []passkey.Credential, error) {
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return nil, nil, err
	}
	creds := make([]passkey.Credential, len(rows))
	for i, r := range rows {
		creds[i] = ToPasskey(r)
	}
	return rows, creds, nil
}

func (s *SignIn) user(ctx context.Context) (passkey.User, error) {
	handle, err := store.ApproverUserHandle(ctx, s.cfg.DB)
	if err != nil {
		return passkey.User{}, err
	}
	return passkey.User{Handle: handle, Name: s.cfg.UserName, DisplayName: s.cfg.UserDisplayName}, nil
}

// HasPasskey reports whether a passkey is enrolled, so a sign-in can work.
func (s *SignIn) HasPasskey(ctx context.Context) (bool, error) {
	rows, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey)
	if err != nil {
		return false, fmt.Errorf("approval.SignIn.HasPasskey: %w", err)
	}
	return len(rows) > 0, nil
}

// BeginSignIn starts signing in the browser whose key (browserproof) is
// browserKey: a ticket, and the navigator.credentials.get options over a
// challenge bound to that key, allowing every enrolled passkey. No passkey
// enrolled is ErrNoPasskey.
func (s *SignIn) BeginSignIn(ctx context.Context, browserKey string) (Ticketed, error) {
	const op = "approval.SignIn.BeginSignIn"
	if err := refuseRestricted(ctx); err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	if browserKey == "" {
		return Ticketed{}, fmt.Errorf("%s: %w", op, ErrNoBrowserKey)
	}
	_, creds, err := s.passkeyRows(ctx)
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(creds) == 0 {
		return Ticketed{}, fmt.Errorf("%s: %w", op, ErrNoPasskey)
	}
	ticket, deadline, nonce, err := newTicket(s.cfg.Now(), signInTicketTTL)
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	opts, err := s.cfg.Passkeys.BeginAssertion(s.challenge("sign-in", deadline, nonce, browserKey), creds)
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	return Ticketed{Ticket: ticket, Options: opts}, nil
}

// FinishSignIn verifies the browser's assertion for ticket, which must have
// been begun by the browser whose key is browserKey and still be good, claims
// the ticket, records the passkey's use (a compare-and-set on its counter),
// and signs the browser in: a fresh cookie, whose sign-in row binds it to
// browserKey for the configured lifetime.
func (s *SignIn) FinishSignIn(ctx context.Context, browserKey, ticket string, response []byte) (SignedIn, error) {
	const op = "approval.SignIn.FinishSignIn"
	if err := refuseRestricted(ctx); err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	if browserKey == "" {
		return SignedIn{}, fmt.Errorf("%s: %w", op, ErrNoBrowserKey)
	}
	now := s.cfg.Now()
	deadline, nonce, err := parseTicket(ticket, now)
	if err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	rows, creds, err := s.passkeyRows(ctx)
	if err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	if len(creds) == 0 {
		return SignedIn{}, fmt.Errorf("%s: %w", op, ErrNoPasskey)
	}
	used, err := s.cfg.Passkeys.FinishAssertion(s.challenge("sign-in", deadline, nonce, browserKey), creds, response)
	if err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	row, ok := rowFor(rows, used.ID)
	if !ok {
		return SignedIn{}, fmt.Errorf("%s: %w", op, passkey.ErrUnknownCredential)
	}
	if err := s.claim(nonce, deadline, now); err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := store.RecordApproverUse(ctx, s.cfg.DB, row.ID, row.SignCount, used.SignCount, used.BackupState, now); err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	cookie, err := store.NewConsoleToken()
	if err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	signedIn, err := store.CreateConsoleSignIn(ctx, s.cfg.DB, cookie, browserKey, row.ID, s.cfg.Lifetime, now)
	if err != nil {
		return SignedIn{}, fmt.Errorf("%s: %w", op, err)
	}
	return SignedIn{Cookie: cookie, SignIn: signedIn, Credential: row}, nil
}

// BeginEnrollment starts adding a passkey named name, from a device that need
// not be signed in: a ticket, and the navigator.credentials.create options
// over a challenge bound to the name, excluding every enrolled passkey (an
// authenticator that holds one, synced passkeys included, is refused by the
// browser). The passkey is enrolled only once the owner approves its code on
// the server.
func (s *SignIn) BeginEnrollment(ctx context.Context, name string) (Ticketed, error) {
	const op = "approval.SignIn.BeginEnrollment"
	if err := refuseRestricted(ctx); err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	name = strings.TrimSpace(name)
	if err := store.ValidPasskeyName(name); err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	_, existing, err := s.passkeyRows(ctx)
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	user, err := s.user(ctx)
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	ticket, deadline, nonce, err := newTicket(s.cfg.Now(), enrollTicketTTL)
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	opts, _, err := s.cfg.Passkeys.BeginRegistrationWith(user, existing, s.challenge("enrollment", deadline, nonce, name))
	if err != nil {
		return Ticketed{}, fmt.Errorf("%s: %w", op, err)
	}
	return Ticketed{Ticket: ticket, Options: opts}, nil
}

// FinishEnrollment verifies the device's new passkey for ticket and name (the
// name the ticket was begun for: another one fails the challenge) and records
// the passkey as waiting for the owner with a fresh code.
// It enrolls nothing: store.ApprovePasskeyEnrollment does, run as the
// daemon's account.
func (s *SignIn) FinishEnrollment(ctx context.Context, ticket, name string, response []byte) (PasskeyAdded, error) {
	const op = "approval.SignIn.FinishEnrollment"
	if err := refuseRestricted(ctx); err != nil {
		return PasskeyAdded{}, fmt.Errorf("%s: %w", op, err)
	}
	now := s.cfg.Now()
	deadline, nonce, err := parseTicket(ticket, now)
	if err != nil {
		return PasskeyAdded{}, fmt.Errorf("%s: %w", op, err)
	}
	name = strings.TrimSpace(name)
	if err := store.ValidPasskeyName(name); err != nil {
		return PasskeyAdded{}, fmt.Errorf("%s: %w", op, err)
	}
	user, err := s.user(ctx)
	if err != nil {
		return PasskeyAdded{}, fmt.Errorf("%s: %w", op, err)
	}
	state := passkey.RegistrationState{Challenge: s.challenge("enrollment", deadline, nonce, name), UserHandle: user.Handle}
	cred, err := s.cfg.Passkeys.FinishRegistration(user, state, response)
	if err != nil {
		return PasskeyAdded{}, fmt.Errorf("%s: %w", op, err)
	}
	// The ticket is not claimed: anyone who reaches the page can finish one,
	// so a claim would let them grow the claims without bound, and the store
	// already makes a replay harmless -- one credential waits once
	// (ErrEnrollmentWaiting) and is enrolled once (ErrApproverExists).
	// The passkey is bound for life to the RP ID it was just made under:
	// recorded, so a later change of rp_id names the passkeys it strands.
	row := FromPasskey(cred, name)
	row.RPID = s.cfg.Passkeys.RPID()
	e, code, err := store.BeginPasskeyEnrollment(ctx, s.cfg.DB, row, store.PasskeyEnrollmentTTL, now)
	if err != nil {
		return PasskeyAdded{}, fmt.Errorf("%s: %w", op, err)
	}
	return PasskeyAdded{ID: e.ID, Code: code, Name: e.Credential.Name, Fingerprint: e.Fingerprint(), PendingUntil: e.PendingUntil}, nil
}

// Enrollment returns enrollment id as the page waiting on its approval reads
// it.
func (s *SignIn) Enrollment(ctx context.Context, id string) (store.PasskeyEnrollment, error) {
	e, err := store.PasskeyEnrollmentByID(ctx, s.cfg.DB, id)
	if err != nil {
		return store.PasskeyEnrollment{}, fmt.Errorf("approval.SignIn.Enrollment: %w", err)
	}
	return e, nil
}
