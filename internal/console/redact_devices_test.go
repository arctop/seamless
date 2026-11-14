package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
)

// redactSecret is the password every address below carries; no page, JSON
// or refusal may show it, nor the user.
const redactSecret = "s3cret"

// redactRequireNoUser holds a body to carrying neither half of the user.
func redactRequireNoUser(t *testing.T, body string) {
	t.Helper()
	require.NotContains(t, body, redactSecret)
	require.NotContains(t, body, "alice")
}

// Where this server's address names a user, the Devices page shows the
// address without it, says to take it out of server_url, and offers no code
// and no one-liner; its JSON carries neither half; the code route mints
// none (409) and says why. The devices paired before are still listed.
func TestRedact_DevicesPageRefusesAUserInTheAddress(t *testing.T) {
	fp := devices.CertFingerprint([]byte("example certificate"))
	db, mux := devices2Console(t, devices.ServerFacts{URL: "https://alice:" + redactSecret + "@example-server.example:8081", TLS: true,
		CertFingerprint: fp})
	page := getPeek(t, mux, "/console/devices")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	redactRequireNoUser(t, body)
	require.Contains(t, body, "<code>https://example-server.example:8081</code>", "the address, without its user")
	require.Contains(t, body, `id="devices-names-user"`)
	require.Contains(t, body, "<code>server_url</code> names a user: remove it, Seamless uses none.")
	require.Contains(t, body, "restart the daemon, then get a code here")
	require.NotContains(t, body, `id="devices-code-form"`, "no code is offered")
	require.NotContains(t, body, "SEAMLESS_PAIR_CODE", "nor a one-liner")
	require.NotContains(t, body, `id="devices-needs-tls"`, "the address is https")
	require.Contains(t, body, "No computer is paired yet.", "the list stays")

	req := httptest.NewRequest(http.MethodGet, "/console/devices", nil)
	req.Header.Set("Accept", "application/json")
	rr := do(mux, authedReq(req))
	require.Equal(t, http.StatusOK, rr.Code)
	redactRequireNoUser(t, rr.Body.String())
	var data struct {
		Server    devices.ServerFacts `json:"server"`
		NamesUser bool                `json:"names_user"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &data), rr.Body.String())
	require.True(t, data.NamesUser)
	require.Equal(t, "https://example-server.example:8081", data.Server.URL)
	require.Equal(t, fp, data.Server.CertFingerprint)

	for _, asJSON := range []bool{true, false} {
		rr := devicePost(t, mux, "/console/devices/code", url.Values{}, asJSON)
		require.Equal(t, http.StatusConflict, rr.Code)
		require.Contains(t, rr.Body.String(), "server_url names a user: remove it, Seamless uses none")
		redactRequireNoUser(t, rr.Body.String())
	}
	open, err := store.CountOpenPairingCodes(context.Background(), db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no code was minted")
}

// Both refusals at once, where both hold: a user in a plain http address to
// another host. With a loopback address the user's refusal stands alone: no
// one-liner is shown for the loopback note to be about.
func TestRedact_DevicesPageNamesEveryRefusal(t *testing.T) {
	_, mux := devices2Console(t, devices.ServerFacts{URL: "http://alice:" + redactSecret + "@192.0.2.10:8081"})
	body := getPeek(t, mux, "/console/devices").Body.String()
	redactRequireNoUser(t, body)
	require.Contains(t, body, `id="devices-names-user"`)
	require.Contains(t, body, `id="devices-needs-tls"`)
	require.NotContains(t, body, `id="devices-code-form"`)
	rr := devicePost(t, mux, "/console/devices/code", url.Values{}, true)
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "server_url names a user")
	require.Contains(t, rr.Body.String(), "pairing needs TLS here")

	_, mux = devices2Console(t, devices.ServerFacts{URL: "http://alice:" + redactSecret + "@127.0.0.1:8081"})
	body = getPeek(t, mux, "/console/devices").Body.String()
	redactRequireNoUser(t, body)
	require.Contains(t, body, `id="devices-names-user"`)
	require.NotContains(t, body, `id="devices-loopback"`)
	require.NotContains(t, body, `id="devices-code-form"`)
	require.Equal(t, http.StatusConflict, devicePost(t, mux, "/console/devices/code", url.Values{}, true).Code)
}
