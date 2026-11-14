package console

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// One address holds a few waiting codes (store.ErrTerminalSignInBusyFrom):
// the next begin from it is a 429 that says why and what to do, and a begin
// from another address still gets a code.
func TestTerminalSignIn_BusyFromThisAddress(t *testing.T) {
	sc := signinNewConsole(t, nil)
	for range 4 {
		signin3Begin(t, sc, signinNewBrowser(t), "192.0.2.50:40000", "Example/1.0")
	}
	b := signinNewBrowser(t)
	req := b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{})
	req.RemoteAddr = "192.0.2.50:40001"
	rr := sc.do(b, req)
	require.Equal(t, http.StatusTooManyRequests, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "too many sign-in codes are waiting from this address: use a code a page here "+
		"already shows, or wait until one expires (a code waits 10 minutes)")
	signin3Begin(t, sc, signinNewBrowser(t), "192.0.2.51:40000", "Example/1.0")
}
