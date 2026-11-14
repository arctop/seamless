package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// deviceConsole builds a console that pairs devices (peer_auth off: the code
// is the approval), and returns its database, its pairing and its mux.
func deviceConsole(t *testing.T) (*sql.DB, *devices.Service, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	pairing := devices.New(devices.Config{DB: db, Events: rec, LocalHost: "example-server",
		Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: "AB:CD"}})
	svc, err := New(Config{
		DB: db, Events: rec, APIKey: testKey,
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Devices:  pairing,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, pairing, mux
}

// devicePost posts form to path with the bearer key, as JSON when asJSON.
func devicePost(t *testing.T, mux *http.ServeMux, path string, form url.Values, asJSON bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	return do(mux, authedReq(req))
}

// A console that pairs nothing has no Devices page, no route that answers,
// and no link to one.
func TestDevices_OffIsNotFound(t *testing.T) {
	_, mux := newConsole(t)
	rr := do(mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/devices", nil)))
	require.Equal(t, http.StatusNotFound, rr.Code)
	for _, path := range []string{"/console/devices/code", "/console/devices/01X/approve/begin",
		"/console/devices/01X/approve/finish", "/console/devices/01X/revoke"} {
		require.Equal(t, http.StatusNotFound, devicePost(t, mux, path, url.Values{}, true).Code, path)
	}
	home := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, home.Code)
	require.NotContains(t, home.Body.String(), "/console/devices", "no page names it")
}

// The page lists the devices together, mints a code shown once in its
// answer, and revokes.
func TestDevices_PageCodeAndRevoke(t *testing.T) {
	db, pairing, mux := deviceConsole(t)
	ctx := context.Background()

	rr := devicePost(t, mux, "/console/devices/code", url.Values{}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
	var minted struct {
		Code, Approval, Unix, Windows string
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &minted))
	require.Equal(t, minted.Code, store.FormatPairingCode(minted.Code))
	require.Empty(t, minted.Approval, "without peer_auth the code is the approval")
	require.Contains(t, minted.Unix, "SEAMLESS_PAIR_CODE="+minted.Code)
	require.Contains(t, minted.Windows, "$env:SEAMLESS_PAIR_CODE='"+minted.Code+"'")

	secret, err := devices.NewSecret()
	require.NoError(t, err)
	answer, err := pairing.Pair(ctx, devices.PairInput{Code: minted.Code, Kind: devices.KindKey, Name: "example-ci", Secret: secret})
	require.NoError(t, err)

	page := getPeek(t, mux, "/console/devices")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, "example-ci")
	require.Contains(t, body, answer.Device.KeyPrefix+"…", "a key device shows its key prefix")
	require.NotContains(t, body, answer.Key, "never the key")
	require.Contains(t, body, "The code is the approval")
	require.Contains(t, body, "https://example-server.example.ts.net:8081")
	require.Contains(t, body, "AB:CD")
	require.Contains(t, body, `action="/console/devices/`+answer.Device.ID+`/revoke"`)

	// A browser without the page's script gets the page, with the code.
	html := devicePost(t, mux, "/console/devices/code", url.Values{}, false)
	require.Equal(t, http.StatusOK, html.Code)
	open, err := store.CountOpenPairingCodes(ctx, db, answer.Device.CreatedAt)
	require.NoError(t, err)
	require.Equal(t, 1, open)
	shown := regexp.MustCompile(`id="devices-code-value">([0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4})<`).FindStringSubmatch(html.Body.String())
	require.Len(t, shown, 2, "the code is drawn into the answer")
	require.NotContains(t, html.Header().Get("Location"), shown[1], "and never into a URL")

	rr = devicePost(t, mux, "/console/devices/"+answer.Device.ID+"/revoke", url.Values{}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "/console/devices?notice=")
	d, err := store.DeviceByID(ctx, db, answer.Device.ID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceRevoked, d.Status)
	require.Equal(t, store.DeviceByConsole, d.RevokedBy)
	rr = devicePost(t, mux, "/console/devices/"+answer.Device.ID+"/revoke", url.Values{}, true)
	require.Equal(t, http.StatusConflict, rr.Code)
	rr = devicePost(t, mux, "/console/devices/01NOSUCH/revoke", url.Values{}, true)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// Without peer_auth there is nothing to approve; under it an approval needs
// a browser.
func TestDevices_ApprovalNeedsPeerAuthAndABrowser(t *testing.T) {
	_, _, mux := deviceConsole(t)
	rr := devicePost(t, mux, "/console/devices/01X/approve/begin", url.Values{}, true)
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "the one-time code is the approval")
}

// A paired device's credential is the bearer path in the console -- the
// edge marked the request -- and it signs no browser in.
func TestDevices_DeviceCredentialIsBearerAndSignsNoBrowserIn(t *testing.T) {
	_, pairing, mux := deviceConsole(t)
	ctx := context.Background()
	code, err := pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	answer, err := pairing.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindKey, Name: "example-ci", Secret: secret})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/console/?format=json", nil)
	req = req.WithContext(devices.WithCredential(req.Context(), devices.Credential{DeviceID: answer.Device.ID, Kind: devices.KindKey, Name: "example-ci"}))
	require.Equal(t, http.StatusOK, do(mux, req).Code, "the edge's mark is the owner's key")

	// The key itself, unmarked (no edge), is a wrong key here.
	req = httptest.NewRequest(http.MethodGet, "/console/?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+answer.Key)
	require.Equal(t, http.StatusUnauthorized, do(mux, req).Code)

	// The login form compares the shared key alone: a device key sets no
	// cookie, marked or not.
	form := url.Values{"key": {answer.Key}, "next": {"/console/"}}
	login := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login = login.WithContext(devices.WithCredential(login.Context(), devices.Credential{DeviceID: answer.Device.ID, Kind: devices.KindKey, Name: "example-ci"}))
	rr := do(mux, login)
	require.Empty(t, rr.Result().Cookies(), "a device key signs no browser in to the console")
	require.Contains(t, rr.Body.String(), "invalid key")
}

// The Devices screen is placed only when the console pairs devices.
func TestDevices_ScreenOnlyWhenOn(t *testing.T) {
	base := []screen{screens[0], {ID: "settings", NavRow: true}}
	require.Equal(t, base, withDevicesScreen(base, false))
	on := withDevicesScreen(base, true)
	require.Len(t, on, 3)
	require.Equal(t, "devices", on[1].ID, "ahead of the pinned Settings entry")
	for _, sc := range screens {
		require.NotEqual(t, "devices", sc.ID, "not in the registry: a console that pairs nothing never names it")
		require.NotEqual(t, devicesScreen.Key, sc.Key, "its g-chord is its own")
	}
	for _, sc := range lockScreens {
		require.NotEqual(t, devicesScreen.Key, sc.Key)
	}
}

// With pairing on, the sidebar links the Devices page (the screen render.go
// places through withDevicesScreen).
func TestDevices_NavLinkWhenOn(t *testing.T) {
	_, _, mux := deviceConsole(t)
	home := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, home.Code)
	require.Contains(t, home.Body.String(), `href="/console/devices"`)
}

// Under peer_auth a pairing waits on the page until the owner approves it
// with a passkey, from a browser: the bearer key cannot, and a finished
// approval pairs exactly the device the ceremony was begun for.
func TestDevices_PasskeyApprovalThroughThePage(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	rec := events.NewRecorder(db)
	appr := approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}, Events: rec})
	approvals, err := approval.NewDeviceApprovals(appr)
	require.NoError(t, err)
	pairing := devices.New(devices.Config{DB: db, Events: rec, PeerAuth: true, Approver: approvals, LocalHost: "example-server",
		Server: devices.ServerFacts{URL: "https://example-server:8081", TLS: true}})
	svc, err := New(Config{DB: db, Events: rec, APIKey: testKey, Approvals: appr, Devices: pairing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, db, appr, key, "key")

	code, err := pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	require.Equal(t, devices.ProofPasskey, code.Approval)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	waiting, err := pairing.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindKey, Name: "example-ci", Secret: secret})
	require.NoError(t, err)
	require.Equal(t, devices.StatusPending, waiting.Status)

	page := getPeek(t, mux, "/console/devices")
	require.Contains(t, page.Body.String(), "Waiting for your approval")
	require.Contains(t, page.Body.String(), waiting.Device.Fingerprint, "the page shows the fingerprint the device shows")
	require.Contains(t, page.Body.String(), "A code alone pairs nothing here")

	base := "/console/devices/" + waiting.Device.ID + "/approve"
	rr := devicePost(t, mux, base+"/begin", url.Values{}, true)
	require.Equal(t, http.StatusForbidden, rr.Code, "the bearer key cannot approve a device: a passkey in a browser does")

	browser := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: consoleToken(testKey)})
		return do(mux, req)
	}
	rr = browser(base+"/begin", url.Values{})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var begun devices.Ceremony
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
	resp, err := key.Get(begun.Options, approverOrigin)
	require.NoError(t, err)

	// The same assertion cannot pair another waiting device.
	code2, err := pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	secret2, err := devices.NewSecret()
	require.NoError(t, err)
	other, err := pairing.Pair(ctx, devices.PairInput{Code: code2.Code, Kind: devices.KindKey, Name: "example-other", Secret: secret2})
	require.NoError(t, err)
	rr = browser("/console/devices/"+other.Device.ID+"/approve/finish", url.Values{"ticket": {begun.Ticket}, "response": {string(resp)}})
	require.NotEqual(t, http.StatusOK, rr.Code, "another device cannot take this approval")

	rr = browser(base+"/finish", url.Values{"ticket": {begun.Ticket}, "response": {string(resp)}})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	d, err := store.DeviceByID(ctx, db, waiting.Device.ID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceActive, d.Status)
	require.NotEmpty(t, d.ApprovedBy)
	still, err := store.DeviceByID(ctx, db, other.Device.ID)
	require.NoError(t, err)
	require.Equal(t, store.DevicePending, still.Status)

	collected, err := pairing.Status(ctx, secret)
	require.NoError(t, err)
	require.True(t, devices.IsKey(collected.Key), "the approved device collects its own key")
}

// devices2Console is a console that pairs devices (peer_auth off) with the
// server facts given, and returns its database and mux.
func devices2Console(t *testing.T, facts devices.ServerFacts) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	pairing := devices.New(devices.Config{DB: db, Events: rec, LocalHost: "example-server", Server: facts})
	svc, err := New(Config{DB: db, Events: rec, APIKey: testKey, Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Devices: pairing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// Where no other computer can pair -- this server's address is plain http to
// another host, which the client and the edge both refuse -- the page says
// pairing needs TLS and offers no code, and the code route mints none (409).
func TestDevices2_PairingNeedsTLSMintsNothing(t *testing.T) {
	db, mux := devices2Console(t, devices.ServerFacts{URL: "http://192.0.2.10:8081"})
	page := getPeek(t, mux, "/console/devices")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, "Pairing needs TLS here")
	require.Contains(t, body, "<code>seamlessd setup</code>'s address step")
	require.NotContains(t, body, `id="devices-code-form"`, "no code is offered")
	require.NotContains(t, body, "SEAMLESS_PAIR_CODE", "nor a one-liner that could only fail")
	require.Contains(t, body, "a pairing over plain http from another computer is refused")

	for _, asJSON := range []bool{true, false} {
		rr := devicePost(t, mux, "/console/devices/code", url.Values{}, asJSON)
		require.Equal(t, http.StatusConflict, rr.Code)
		require.Contains(t, rr.Body.String(), "pairing needs TLS here")
	}
	open, err := store.CountOpenPairingCodes(context.Background(), db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no code was minted")

	// Loopback over plain http still pairs, on this computer.
	_, mux = devices2Console(t, devices.ServerFacts{URL: "http://127.0.0.1:8081"})
	require.Equal(t, http.StatusOK, devicePost(t, mux, "/console/devices/code", url.Values{}, true).Code)
}

// The one-liners carry this server's certificate fingerprint, in the form
// setup on the other computer reads, and the page says the code works once.
func TestDevices2_OneLinersCarryTheFingerprint(t *testing.T) {
	fp := devices.CertFingerprint([]byte("example certificate"))
	_, mux := devices2Console(t, devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: fp})
	rr := devicePost(t, mux, "/console/devices/code", url.Values{}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var minted struct {
		Code, Unix, Windows string
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &minted))
	require.Contains(t, minted.Unix, "SEAMLESS_SERVER_URL=https://example-server.example.ts.net:8081 SEAMLESS_SERVER_FINGERPRINT="+fp+
		" SEAMLESS_PAIR_CODE="+minted.Code+" sh")
	require.Contains(t, minted.Windows, "$env:SEAMLESS_SERVER_FINGERPRINT='"+fp+"'; $env:SEAMLESS_PAIR_CODE='"+minted.Code+"'")

	html := devicePost(t, mux, "/console/devices/code", url.Values{}, false)
	require.Equal(t, http.StatusOK, html.Code)
	body := html.Body.String()
	require.Contains(t, body, "SEAMLESS_SERVER_FINGERPRINT="+fp+" SEAMLESS_PAIR_CODE=")
	require.Contains(t, body, "Each carries this server's certificate fingerprint, which setup there compares before it sends the code.")
	require.Contains(t, body, "The code works once, within 10 minutes: the line it leaves in that computer's shell history is dead after use.")
}
