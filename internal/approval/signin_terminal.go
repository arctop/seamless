package approval

// The terminal sign-in (store/signin_terminal.go): the console's way in for an
// owner with no passkey left. The browser begins it under a fresh cookie bound
// to its key, exactly as a passkey sign-in binds one, and shows a code; the
// owner approves the code on the server as the daemon's account (`seamlessd
// console-open --sign-in --code`), which opens the database directly, so no
// route of this service approves one. The begin records what its request
// showed of the browser (WithTerminalSignInBrowser), which the owner's
// terminal shows before it asks to approve the code.

import (
	"context"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// TerminalSignIn is a terminal sign-in a browser just began: the cookie to
// give it (its only copy; the row keeps a hash), the row's id the page asks
// after, the code it shows, how long the code waits, and the browser as the
// row recorded it -- what the owner's terminal will show beside the code.
type TerminalSignIn struct {
	ID           string
	Cookie       string
	Code         string
	PendingUntil time.Time
	Browser      store.TerminalSignInBrowser
}

// terminalSignInBrowserKey carries a begin request's browser in its context.
type terminalSignInBrowserKey struct{}

// WithTerminalSignInBrowser returns ctx carrying what the request beginning a
// terminal sign-in showed of its browser -- the address it came from and its
// User-Agent -- for BeginTerminalSignIn to record. The console sets it from
// the request; a begin without it records nothing, and the owner's terminal
// then says so.
func WithTerminalSignInBrowser(ctx context.Context, b store.TerminalSignInBrowser) context.Context {
	return context.WithValue(ctx, terminalSignInBrowserKey{}, b)
}

// BeginTerminalSignIn begins a terminal sign-in for the browser whose key
// (browserproof) is browserKey, under a fresh cookie, recording the browser
// ctx carries (WithTerminalSignInBrowser). It signs nothing in: the daemon's
// account approving the code does.
func (s *SignIn) BeginTerminalSignIn(ctx context.Context, browserKey string) (TerminalSignIn, error) {
	const op = "approval.SignIn.BeginTerminalSignIn"
	if err := refuseRestricted(ctx); err != nil {
		return TerminalSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	if browserKey == "" {
		return TerminalSignIn{}, fmt.Errorf("%s: %w", op, ErrNoBrowserKey)
	}
	cookie, err := store.NewConsoleToken()
	if err != nil {
		return TerminalSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	from, _ := ctx.Value(terminalSignInBrowserKey{}).(store.TerminalSignInBrowser)
	row, code, err := store.BeginTerminalSignIn(ctx, s.cfg.DB, cookie, browserKey, from, s.cfg.Now())
	if err != nil {
		return TerminalSignIn{}, fmt.Errorf("%s: %w", op, err)
	}
	return TerminalSignIn{ID: row.ID, Cookie: cookie, Code: code, PendingUntil: row.CreatedAt.Add(store.TerminalSignInTTL),
		Browser: row.Browser}, nil
}

// TerminalSignInState reports terminal sign-in id to the browser holding
// cookie and browserKey, the pair it was begun for: another browser, or a
// cookie without its key, finds nothing (store.ErrTerminalSignInNotFound).
func (s *SignIn) TerminalSignInState(ctx context.Context, id, cookie, browserKey string) (store.ConsoleSignIn, store.TerminalSignInState, error) {
	const op = "approval.SignIn.TerminalSignInState"
	if err := refuseRestricted(ctx); err != nil {
		return store.ConsoleSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	if browserKey == "" {
		return store.ConsoleSignIn{}, "", fmt.Errorf("%s: %w", op, ErrNoBrowserKey)
	}
	row, state, err := store.TerminalSignInFor(ctx, s.cfg.DB, id, store.HashConsoleSignIn(cookie, browserKey), s.cfg.Now())
	if err != nil {
		return store.ConsoleSignIn{}, "", fmt.Errorf("%s: %w", op, err)
	}
	return row, state, nil
}
