package console

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// A begin never waits for room: past the few codes one address may hold, the
// next begin from it still gets a code, and the page that showed the
// address's oldest finds nothing waiting and says why. A page showing a code
// from another address keeps it however long the loop runs, and a page whose
// code its own address's loop crowded out hears the same answer -- the
// control.
func TestTerminalSignIn_BeginDisplacesTheAddressOldest(t *testing.T) {
	sc := signinNewConsole(t, nil)
	owner := signinNewBrowser(t)
	ownerID, _ := signin3Begin(t, sc, owner, "192.0.2.51:40000", "Example/1.0")
	type page struct {
		b  *signinBrowser
		id string
	}
	var loop []page
	for range 4 {
		b := signinNewBrowser(t)
		id, _ := signin3Begin(t, sc, b, "192.0.2.50:40000", "Example/1.0")
		loop = append(loop, page{b, id})
	}
	for range 8 {
		signin3Begin(t, sc, signinNewBrowser(t), "192.0.2.50:40001", "Example/1.0")
	}

	gone := func(p page) {
		t.Helper()
		rr := sc.do(p.b, p.b.request(t, http.MethodGet, "/console/signin/terminal/status?id="+url.QueryEscape(p.id), nil))
		require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
		require.Contains(t, rr.Body.String(), "this browser has no sign-in code still waiting: it expired, or newer codes "+
			"shown from this address took its place. Show a new code.")
	}
	for _, p := range loop {
		gone(p)
	}
	code, state := retireStatus(t, sc, owner, ownerID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "pending", state, "a loop from another address never reaches the owner's code")

	// The control: a loop from the owner's own address crowds it out.
	for range 4 {
		signin3Begin(t, sc, signinNewBrowser(t), "192.0.2.51:40002", "Example/1.0")
	}
	gone(page{owner, ownerID})
}
