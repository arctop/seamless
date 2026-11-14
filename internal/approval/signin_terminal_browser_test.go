package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// The begin records the browser its request's context carries, cleaned as the
// row keeps it, and returns it for the page; the lookup the owner's terminal
// makes reads the same. A begin with none records nothing.
func TestTerminalSignIn_RecordsTheRequestsBrowser(t *testing.T) {
	db, svc, clock := signinService(t)
	from := store.TerminalSignInBrowser{Addr: "192.0.2.44", UserAgent: "Mozilla/5.0 Example\x1b[31m"}
	begun, err := svc.BeginTerminalSignIn(WithTerminalSignInBrowser(context.Background(), from), "key-A")
	require.NoError(t, err)
	want := store.TerminalSignInBrowser{Addr: "192.0.2.44", UserAgent: "Mozilla/5.0 Example?[31m"}
	require.Equal(t, want, begun.Browser)
	w, err := store.TerminalSignInByCode(context.Background(), db, begun.Code, clock.now)
	require.NoError(t, err)
	require.Equal(t, begun.ID, w.ID)
	require.Equal(t, want, w.Browser)

	bare, err := svc.BeginTerminalSignIn(context.Background(), "key-B")
	require.NoError(t, err)
	require.Equal(t, store.TerminalSignInBrowser{}, bare.Browser)
}
