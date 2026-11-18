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
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	pairing := devices.New(devices.Config{DB: db, Events: rec, LocalHost: "example-server", PeerAuth: peerAuth,
		Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: "AB:CD"}})
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
