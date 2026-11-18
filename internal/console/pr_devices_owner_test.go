package console

// The Devices page and a relay's owner uid (owner decision R-D1): without
// peer_auth, a relay whose pairing recorded none is warned about, with the
// remedy, and one that recorded one says which uid it serves.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// prDevicesConsole is deviceConsole with peer_auth as asked, and two relays
// paired: example-laptop naming uid 501, example-desk naming none (as a relay
// of an earlier release pairs). It returns the mux and the two devices.
func prDevicesConsole(t *testing.T, peerAuth bool) (*http.ServeMux, store.Device, store.Device) {
	t.Helper()
	return prDevicesConsoleAt(t, peerAuth, time.Time{})
}

// prDevicesConsoleAt is prDevicesConsole with the served certificate valid
// until notAfter (zero: unknown).
func prDevicesConsoleAt(t *testing.T, peerAuth bool, notAfter time.Time) (*http.ServeMux, store.Device, store.Device) {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	pairing := devices.New(devices.Config{DB: db, Events: rec, LocalHost: "example-server", PeerAuth: peerAuth,
		Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: "AB:CD", CertNotAfter: notAfter}})
	svc, err := New(Config{DB: db, Events: rec, APIKey: testKey, Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Devices: pairing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	pair := func(name string, owner uint32) store.Device {
		id, err := devices.NewRelayIdentity(name)
		require.NoError(t, err)
		code, err := pairing.MintCode(ctx, store.DeviceByConsole)
		require.NoError(t, err)
		secret, err := devices.NewSecret()
		require.NoError(t, err)
		answer, err := pairing.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindRelay, Name: name, Secret: secret,
			RelayKey: id.PublicKey(), OwnerUID: owner})
		require.NoError(t, err)
		d := answer.Device
		if peerAuth {
			d, err = store.ApproveDevice(ctx, db, d.ID, d.Fingerprint, "01PASSKEYEXAMPLE", time.Now())
			require.NoError(t, err)
		}
		return d
	}
	return mux, pair("example-laptop", 501), pair("example-desk", 0)
}

func TestPRDevicesPage_OwnerUIDWithoutPeerAuth(t *testing.T) {
	mux, bound, unbound := prDevicesConsole(t, false)
	page := getPeek(t, mux, "/console/devices")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `data-device-unbound="`+unbound.ID+`"`, "the relay that recorded no uid is warned about")
	require.NotContains(t, body, `data-device-unbound="`+bound.ID+`"`, "the one that did is not")
	require.Contains(t, body, "Re-pair it to bind the owner's uid")
	require.Contains(t, body, "seamlessd relay pair --replace --owner-uid &lt;your uid there&gt;")
	require.Contains(t, body, "serves uid 501 there alone, yours")
	require.Contains(t, body, `id="devices-owner-only"`)

	// The page's JSON carries the uid a pairing recorded, and none for one
	// that recorded none.
	js := getPeek(t, mux, "/console/devices?format=json")
	require.Equal(t, http.StatusOK, js.Code)
	require.Contains(t, js.Body.String(), `"owner_uid": 501`)
	require.Equal(t, 1, strings.Count(js.Body.String(), `"owner_uid"`))
}

func TestPRDevicesPage_OwnerUIDUnderPeerAuth(t *testing.T) {
	mux, _, unbound := prDevicesConsole(t, true)
	page := getPeek(t, mux, "/console/devices")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.NotContains(t, body, "data-device-unbound", "under peer_auth, peer_auth.relays judges: nothing to bind")
	require.NotContains(t, body, `data-device-unbound="`+unbound.ID+`"`)
	require.Contains(t, body, "it named uid 501 there as yours")
	require.NotContains(t, body, `id="devices-owner-only"`)
}

// From 14 days before the served certificate's notAfter, the page says that
// every relay paired here must pair again after the renewal, and names them;
// before that, and with no certificate known, it says nothing of it.
func TestPRDevicesPage_RenewalWarning(t *testing.T) {
	for name, tc := range map[string]struct {
		left time.Duration
		want string
	}{
		"ten days out":   {left: 10*24*time.Hour + time.Hour, want: "in 10 days"},
		"the last day":   {left: time.Hour, want: "in 0 days"},
		"one day out":    {left: 24*time.Hour + time.Hour, want: "in 1 day."},
		"expired":        {left: -time.Hour, want: "expired on"},
		"a month out":    {left: 30 * 24 * time.Hour},
		"no certificate": {},
	} {
		notAfter := time.Time{}
		if tc.left != 0 {
			notAfter = time.Now().Add(tc.left)
		}
		mux, _, _ := prDevicesConsoleAt(t, false, notAfter)
		page := getPeek(t, mux, "/console/devices")
		require.Equal(t, http.StatusOK, page.Code, name)
		body := page.Body.String()
		js := getPeek(t, mux, "/console/devices?format=json").Body.String()
		if tc.want == "" {
			require.NotContains(t, body, `id="devices-renewal"`, name)
			require.NotContains(t, js, `"renewal"`, name)
			continue
		}
		require.Contains(t, body, `id="devices-renewal"`, name)
		require.Contains(t, body, tc.want, name)
		require.Contains(t, body, "every relay paired here must pair again: <strong>example-desk</strong>, <strong>example-laptop</strong>", name)
		require.Contains(t, js, `"renewal": {`, name)
	}
}
