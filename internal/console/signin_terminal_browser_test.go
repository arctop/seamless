package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// signin3Begin begins a terminal sign-in from b, its request coming from
// remoteAddr with userAgent, and returns the row's id and the browser the
// begin's answer says it recorded.
func signin3Begin(t *testing.T, sc *signinConsole, b *signinBrowser, remoteAddr, userAgent string) (string, map[string]string) {
	t.Helper()
	req := b.request(t, http.MethodPost, "/console/signin/terminal/begin", url.Values{})
	req.RemoteAddr = remoteAddr
	req.Header.Set("User-Agent", userAgent)
	rr := sc.do(b, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got := signinJSON(t, rr)
	var id string
	require.NoError(t, json.Unmarshal(got["id"], &id))
	var browser map[string]string
	require.NoError(t, json.Unmarshal(got["browser"], &browser), "the begin's answer names the browser it recorded")
	return id, browser
}

// signin3Recorded is what the row of terminal sign-in id keeps of its browser.
func signin3Recorded(t *testing.T, sc *signinConsole, id string) (string, string) {
	t.Helper()
	var addr, agent string
	require.NoError(t, sc.db.QueryRow(`SELECT remote_addr, user_agent FROM console_signins WHERE id = ?`, id).Scan(&addr, &agent))
	return addr, agent
}

// The begin records the address its request came from (the connection's
// peer, without its port) and the User-Agent it sent, as printable ASCII, and
// its answer shows the page the same, for the owner to compare with what the
// terminal shows beside the code.
func TestTerminalSignIn_RecordsTheBrowserThatBegan(t *testing.T) {
	sc := signinNewConsole(t, nil)
	for _, tc := range []struct {
		remote, agent, wantAddr, wantAgent string
	}{
		{"192.0.2.44:51234", "Mozilla/5.0 (Macintosh) Example/17.0", "192.0.2.44", "Mozilla/5.0 (Macintosh) Example/17.0"},
		{"[::ffff:192.0.2.45]:443", "Example/1.0", "192.0.2.45", "Example/1.0"},
		{"[2001:db8::7]:8443", "", "2001:db8::7", ""},
		{"127.0.0.1:50000", "\x1b[2J\x1b]0;owned\x07Example\r\nSign in? y", "127.0.0.1", "?[2J?]0;owned?Example??Sign in? y"},
	} {
		id, browser := signin3Begin(t, sc, signinNewBrowser(t), tc.remote, tc.agent)
		addr, agent := signin3Recorded(t, sc, id)
		require.Equal(t, tc.wantAddr, addr, tc.remote)
		require.Equal(t, tc.wantAgent, agent, tc.remote)
		require.Equal(t, map[string]string{"address": tc.wantAddr, "user_agent": tc.wantAgent}, browser, tc.remote)
	}
}
