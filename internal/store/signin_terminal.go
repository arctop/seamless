package store

// Terminal sign-in (console.sign_in: passkey): the way into the console with
// no passkey at all, for an owner who lost every one. It is a terminal
// unlock's pattern (console_unlocks.go) applied to the sign-in itself: the
// browser begins it and shows a pairing code, and the owner approves that
// code in a terminal on the server, as the daemon's account under peer_auth
// (`seamlessd console-open --sign-in --code`).
//
// The row lives in console_signins, bound from the start, as every sign-in
// is, to HashConsoleSignIn of the cookie the begin set and the browser's key.
// While it waits, its credential is terminalSignInWaiting and the hash of its
// code, and its expires_at is its created_at, so no reader of a live sign-in
// (LiveConsoleSignIn, CountLiveConsoleSignIns) ever counts it. Approval is
// the row's one transition: a compare-and-set on the code that makes it live
// for the sign-in's lifetime under SignInByDaemon. The code names the row and
// carries nothing else, so approving it signs in the browser that began it and
// no other: a cookie read off another port, without that browser's key,
// hashes to no row.
//
// Anyone who reaches the console can begin one, so a code someone else sent
// the owner names their browser. The begin therefore records what it can see
// of the browser (TerminalSignInBrowser: the address its request came from
// and its User-Agent; migration 044), and the approval shows that, with when
// it began (TerminalSignInByCode), and asks before it approves exactly the row
// it showed.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// SignInByDaemon is the credential of a terminal sign-in: the daemon's own
// account approved its code. No approver credential has this id, so removing
// a passkey never ends one; a restore or a sign-out does.
const SignInByDaemon = "daemon-account"

// TerminalSignInTTL is how long a terminal sign-in waits for its code to be
// approved.
const TerminalSignInTTL = 10 * time.Minute

// terminalSignInWaiting prefixes the credential of a terminal sign-in still
// waiting for its code, followed by the code's hash.
const terminalSignInWaiting = "terminal-code:"

// maxWaitingTerminalSignIns caps the terminal sign-ins waiting at once: a
// caller looping on the begin route, which needs no credential, cannot grow
// the table without bound.
const maxWaitingTerminalSignIns = 32

var (
	// ErrTerminalSignInBusy is a terminal sign-in begun while
	// maxWaitingTerminalSignIns wait already.
	ErrTerminalSignInBusy = errors.New("too many terminal sign-ins are waiting")
	// ErrTerminalSignInNotFound is a code that names no terminal sign-in still
	// waiting: mistyped, already approved, past its deadline, or never shown.
	ErrTerminalSignInNotFound = errors.New("no terminal sign-in is waiting for that code")
)

// TerminalSignInState is a terminal sign-in as the browser that began it
// sees it.
type TerminalSignInState string

const (
	// TerminalSignInWaiting: its code waits for the daemon's account.
	TerminalSignInWaiting TerminalSignInState = "pending"
	// TerminalSignInLive: approved; the browser is signed in.
	TerminalSignInLive TerminalSignInState = "signed-in"
	// TerminalSignInExpired: its code was never approved in time, or the
	// sign-in it became has expired.
	TerminalSignInExpired TerminalSignInState = "expired"
	// TerminalSignInEnded: signed out, or ended by a restore.
	TerminalSignInEnded TerminalSignInState = "ended"
)

// TerminalSignInBrowser is what a terminal sign-in's begin records of the
// browser that began it, for the owner to check before approving its code:
// console-open --sign-in shows it, and the sign-in page shows it beside the
// code. Neither field proves anything. The address is the connection's peer
// (a proxy's, when one sits in front of the console), and the User-Agent is
// whatever the request sent. Stored and read back through Clean, both are
// printable ASCII and bounded, so neither can write an escape into the
// owner's terminal or grow the row.
type TerminalSignInBrowser struct {
	// Addr is the address the begin request came from.
	Addr string
	// UserAgent is the User-Agent header the begin request sent.
	UserAgent string
}

// The most of a TerminalSignInBrowser a row keeps, in characters.
const (
	MaxTerminalSignInAddr      = 64
	MaxTerminalSignInUserAgent = 256
)

// Clean returns b as a terminal sign-in keeps and shows it: each field
// trimmed, every character outside printable ASCII (space to tilde) replaced
// by '?' -- a control character, an escape's ESC, a newline, any non-ASCII
// letter or invalid byte -- and cut to its maximum, ending in "...".
func (b TerminalSignInBrowser) Clean() TerminalSignInBrowser {
	return TerminalSignInBrowser{
		Addr:      printableASCII(b.Addr, MaxTerminalSignInAddr),
		UserAgent: printableASCII(b.UserAgent, MaxTerminalSignInUserAgent),
	}
}

// printableASCII is s trimmed, with every rune outside ' '..'~' replaced by
// '?', and cut to at most limit bytes, the last three "..." when it was cut.
// It reads no further than it keeps, however long s is.
func printableASCII(s string, limit int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if b.Len() == limit {
			out := b.String()
			return out[:limit-3] + "..."
		}
		if r < ' ' || r > '~' {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// WaitingTerminalSignIn is a terminal sign-in's row with what its begin
// recorded of the browser (Browser, already Clean).
type WaitingTerminalSignIn struct {
	ConsoleSignIn
	Browser TerminalSignInBrowser
}

// terminalSignInCols reads a terminal sign-in's row and its browser
// (scanTerminalSignIn).
const terminalSignInCols = consoleSignInCols + `, remote_addr, user_agent`

func scanTerminalSignIn(row interface{ Scan(...any) error }) (WaitingTerminalSignIn, error) {
	var b TerminalSignInBrowser
	s, err := scanConsoleSignIn(row, &b.Addr, &b.UserAgent)
	if err != nil {
		return WaitingTerminalSignIn{}, err
	}
	return WaitingTerminalSignIn{ConsoleSignIn: s, Browser: b.Clean()}, nil
}

// terminalSignInCodeHash is what a waiting row keeps of its code: hex SHA-256
// of the code's canonical form under a prefix no other code uses.
func terminalSignInCodeHash(code string) string {
	sum := sha256.Sum256([]byte("seamless console terminal sign-in code v1\x00" + NormalizeEnrollCode(code)))
	return hex.EncodeToString(sum[:])
}

// isTerminalWaiting is the SQL test of a row whose credential is a waiting
// terminal sign-in's, and waitingTerminalSignIn the predicate of one still
// waiting at the bound time (its ? is the deadline floor: now minus the TTL).
var (
	isTerminalWaiting     = `substr(credential, 1, ` + strconv.Itoa(len(terminalSignInWaiting)) + `) = '` + terminalSignInWaiting + `'`
	waitingTerminalSignIn = `ended_at IS NULL AND ` + isTerminalWaiting + ` AND created_at > ?`
)

// newTerminalSignInCode draws a code, printed XXXX-XXXX. A variable so a test
// can force the re-roll.
var newTerminalSignInCode = func() (string, error) { return newUnlockCode() }

// BeginTerminalSignIn records a terminal sign-in for the browser that holds
// cookie and browserKey, waiting TerminalSignInTTL for the daemon's account to
// approve its code, with what the begin request showed of that browser (from,
// kept Clean), and returns the row and the code. Waiting rows past their
// deadline are purged first, and at most maxWaitingTerminalSignIns wait.
func BeginTerminalSignIn(ctx context.Context, db *sql.DB, cookie, browserKey string, from TerminalSignInBrowser,
	now time.Time) (WaitingTerminalSignIn, string, error) {
	const op = "store.BeginTerminalSignIn"
	token := HashConsoleSignIn(cookie, browserKey)
	if token == "" {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w: a cookie and a browser key are required", op, ErrInvalidSignIn)
	}
	id, err := core.NewID()
	if err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	from = from.Clean()
	now = now.UTC()
	at, floor := core.FormatTime(now), core.FormatTime(now.Add(-TerminalSignInTTL))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM console_signins WHERE ended_at IS NULL AND `+isTerminalWaiting+` AND created_at <= ?`,
		floor); err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: purge: %w", op, err)
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_signins WHERE `+waitingTerminalSignIn, floor).Scan(&waiting); err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if waiting >= maxWaitingTerminalSignIns {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, ErrTerminalSignInBusy)
	}
	code := ""
	for range unlockCodeTries {
		c, err := newTerminalSignInCode()
		if err != nil {
			return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: code: %w", op, err)
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_signins WHERE credential = ? AND `+waitingTerminalSignIn,
			terminalSignInWaiting+terminalSignInCodeHash(c), floor).Scan(&taken); err != nil {
			return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
		}
		if taken == 0 {
			code = c
			break
		}
	}
	if code == "" {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: no free code after %d tries", op, unlockCodeTries)
	}
	// expires_at is created_at: a waiting row is never live.
	if _, err := tx.ExecContext(ctx, `INSERT INTO console_signins (id, token_hash, cookie_hash, credential, created_at, expires_at,
		remote_addr, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, token, hashSignInCookie(cookie),
		terminalSignInWaiting+terminalSignInCodeHash(code), at, at, from.Addr, from.UserAgent); err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	s, err := scanTerminalSignIn(tx.QueryRowContext(ctx, `SELECT `+terminalSignInCols+` FROM console_signins WHERE id = ?`, id))
	if err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return WaitingTerminalSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	return s, code, nil
}

// TerminalSignInByCode returns the terminal sign-in code names while it waits
// at now, with what its begin recorded of the browser: what the owner checks
// before approving it (console-open --sign-in). A malformed code is
// ErrInvalidSignIn; one that names no waiting sign-in -- mistyped, already
// approved, past its deadline, or ended -- is ErrTerminalSignInNotFound.
func TerminalSignInByCode(ctx context.Context, db *sql.DB, code string, now time.Time) (WaitingTerminalSignIn, error) {
	const op = "store.TerminalSignInByCode"
	if FormatUnlockCode(code) == "" {
		return WaitingTerminalSignIn{}, fmt.Errorf("%s: %w: a code is %d letters and digits, XXXX-XXXX", op, ErrInvalidSignIn, unlockCodeLen)
	}
	w, err := scanTerminalSignIn(db.QueryRowContext(ctx, `SELECT `+terminalSignInCols+` FROM console_signins
		WHERE credential = ? AND `+waitingTerminalSignIn, terminalSignInWaiting+terminalSignInCodeHash(code),
		core.FormatTime(now.UTC().Add(-TerminalSignInTTL))))
	if errors.Is(err, sql.ErrNoRows) {
		return WaitingTerminalSignIn{}, fmt.Errorf("%s: %w", op, ErrTerminalSignInNotFound)
	}
	if err != nil {
		return WaitingTerminalSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	return w, nil
}

// ApproveTerminalSignIn signs in the browser whose terminal sign-in id shows
// code, still waiting at now: the row becomes live for lifetime from now,
// under SignInByDaemon. id is the row TerminalSignInByCode showed the owner,
// so the approval signs in that browser or none. A malformed code is
// ErrInvalidSignIn; a code and id that name no waiting row are
// ErrTerminalSignInNotFound. It is a compare-and-set on the row's id and code,
// which it clears, so of two racing approvals exactly one succeeds and a code
// works once. The browser is the one that began it: approval names no cookie
// and no key.
func ApproveTerminalSignIn(ctx context.Context, db *sql.DB, id, code string, lifetime time.Duration, now time.Time) (ConsoleSignIn, error) {
	const op = "store.ApproveTerminalSignIn"
	if FormatUnlockCode(code) == "" {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w: a code is %d letters and digits, XXXX-XXXX", op, ErrInvalidSignIn, unlockCodeLen)
	}
	if lifetime < time.Second {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w: a sign-in lasts at least a second", op, ErrInvalidSignIn)
	}
	now = now.UTC()
	waiting := terminalSignInWaiting + terminalSignInCodeHash(code)
	floor := core.FormatTime(now.Add(-TerminalSignInTTL))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE console_signins SET credential = ?, expires_at = ?
		WHERE id = ? AND credential = ? AND `+waitingTerminalSignIn,
		SignInByDaemon, core.FormatTime(now.Add(lifetime)), id, waiting, floor)
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return ConsoleSignIn{}, fmt.Errorf("%s: %w", op, ErrTerminalSignInNotFound)
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

// TerminalSignInFor returns terminal sign-in id as the browser whose cookie
// and key hash to tokenHash sees it, and its state at now. Another browser's
// row, or an id that names none, is ErrTerminalSignInNotFound: the same
// answer either way.
func TerminalSignInFor(ctx context.Context, db *sql.DB, id, tokenHash string, now time.Time) (ConsoleSignIn, TerminalSignInState, error) {
	const op = "store.TerminalSignInFor"
	if !validTokenHash(tokenHash) {
		return ConsoleSignIn{}, "", fmt.Errorf("%s: %w", op, ErrTerminalSignInNotFound)
	}
	s, err := scanConsoleSignIn(db.QueryRowContext(ctx, `SELECT `+consoleSignInCols+` FROM console_signins
		WHERE id = ? AND token_hash = ? AND (credential = ? OR `+isTerminalWaiting+`)`,
		id, tokenHash, SignInByDaemon))
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleSignIn{}, "", fmt.Errorf("%s: %w", op, ErrTerminalSignInNotFound)
	}
	if err != nil {
		return ConsoleSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	switch {
	case s.EndedAt != nil:
		return s, TerminalSignInEnded, nil
	case strings.HasPrefix(s.Credential, terminalSignInWaiting):
		if s.CreatedAt.Add(TerminalSignInTTL).After(now) {
			s.Credential = "" // the code's hash is the daemon's, not the page's
			return s, TerminalSignInWaiting, nil
		}
		s.Credential = ""
		return s, TerminalSignInExpired, nil
	case s.ExpiresAt.After(now):
		return s, TerminalSignInLive, nil
	}
	return s, TerminalSignInExpired, nil
}

// PasskeySignInsSince counts the console sign-ins a passkey made at or after
// since (the terminal's own, SignInByDaemon, are not counted, nor are the
// waiting ones): the proof `seamlessd setup` waits for before it retires the
// key.
func PasskeySignInsSince(ctx context.Context, db *sql.DB, since time.Time) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_signins
		WHERE created_at >= ? AND credential != ? AND NOT `+isTerminalWaiting,
		core.FormatTime(since.UTC()), SignInByDaemon).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.PasskeySignInsSince: %w", err)
	}
	return n, nil
}
