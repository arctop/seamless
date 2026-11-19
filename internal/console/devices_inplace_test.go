package console

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The Devices page's approval in place (owner decision R-D6): under
// peer_auth, a relay its one-time code alone paired that peer_auth.relays
// maps waits for the owner's passkey, and the approval keeps its pairing.

// ipConsole is a console under peer_auth that pairs devices and maps the
// relays named example-laptop and example-old, with a passkey enrolled.
func ipConsole(t *testing.T) (*sql.DB, *devices.Service, *http.ServeMux, *passkeytest.Authenticator) {
	t.Helper()
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
		Server:      devices.ServerFacts{URL: "https://example-server:8081", TLS: true},
		RelayMapped: func(name string) bool { return name == "example-laptop" || name == "example-old" }})
	svc, err := New(Config{DB: db, Events: rec, APIKey: testKey, Approvals: appr, Devices: pairing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, db, appr, key, "key")
	return db, pairing, mux, key
}

// ipCodeOnly records a relay named name paired the way a server without
// peer_auth pairs one: active at once, its code the only approval.
func ipCodeOnly(t *testing.T, db *sql.DB, name string) store.Device {
	t.Helper()
	ctx := context.Background()
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	code, _, err := store.MintPairingCode(ctx, db, store.DeviceByConsole, time.Now())
	require.NoError(t, err)
	secret := sha256.Sum256([]byte("secret:" + name))
	spki := id.PublicKey()
	d, err := store.PairDevice(ctx, db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: devices.PublicKeyFingerprint(spki), PublicKey: spki, KeyHash: devices.PublicKeyHash(spki),
		SecretHash: hex.EncodeToString(secret[:]), OwnerUID: 501}, time.Now())
	require.NoError(t, err)
	return d
}

// ipBrowser is a request of the owner's signed-in browser.
func ipBrowser(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: consoleToken(testKey)})
	return req
}

// ipPage is the Devices page's JSON.
func ipPage(t *testing.T, mux *http.ServeMux) devicesData {
	t.Helper()
	rr := getPeek(t, mux, "/console/devices?format=json")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var d devicesData
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &d), rr.Body.String())
	return d
}

func ipIDs(rows []deviceRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func TestDevices_InPlaceApprovalThroughThePage(t *testing.T) {
	ctx := context.Background()
	db, _, mux, key := ipConsole(t)
	laptop := ipCodeOnly(t, db, "example-laptop")
	studio := ipCodeOnly(t, db, "example-studio") // not mapped
	old := ipCodeOnly(t, db, "example-old")
	_, err := store.RevokeDevice(ctx, db, old.ID, store.DeviceByConsole, time.Now())
	require.NoError(t, err)

	page := ipPage(t, mux)
	require.Equal(t, []string{laptop.ID}, ipIDs(page.InPlace), "the mapped code-only relay waits for approval")
	require.True(t, page.InPlace[0].InPlace)
	require.Equal(t, []string{studio.ID}, ipIDs(page.Paired), "an unmapped one is not offered")
	require.Contains(t, ipIDs(page.Ended), old.ID, "a revoked one is not offered: it pairs again")
	html := getPeek(t, mux, "/console/devices").Body.String()
	require.Contains(t, html, `data-device-in-place="`+laptop.ID+`"`)
	require.Contains(t, html, "Paired while this server ran without peer_auth")
	require.NotContains(t, html, `data-device-in-place="`+studio.ID+`"`)

	base := "/console/devices/" + laptop.ID + "/approve"
	// No caller but the owner's signed-in browser approves: not the bearer
	// key, not an agent on the socket, not the owner's own hands through a
	// relay (a relay's request is never a browser's).
	rr := devicePost(t, mux, base+"/begin", url.Values{}, true)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "approving a device needs a passkey")
	policy, err := access.New(config.PeerAuth{Owner: "owner", Relays: map[string]config.PeerRelay{"example-desk": {OwnerUID: 501}}},
		func(n string) (access.Account, error) { return access.Account{UID: 501, Name: n}, nil })
	require.NoError(t, err)
	agent, ok := policy.PeerView(501)
	require.True(t, ok)
	socket := access.WithAgentChain(access.WithView(context.Background(), agent), func() bool { return true })
	hands := crhRelayCtx(t, devices.Credential{DeviceID: studio.ID, Kind: devices.KindRelay, Name: "example-desk", Approved: true}, "")
	for name, ctx := range map[string]context.Context{"an agent on the socket": socket, "the owner's hands through a relay": hands} {
		req := ipBrowser(base+"/begin", url.Values{}).WithContext(ctx)
		rr := do(mux, req)
		require.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", name, rr.Body.String())
		require.Contains(t, rr.Body.String(), "approving a device needs a passkey", name)
	}
	still, err := store.DeviceByID(ctx, db, laptop.ID)
	require.NoError(t, err)
	require.Empty(t, still.ApprovedBy)

	rr = do(mux, ipBrowser(base+"/begin", url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var begun devices.Ceremony
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
	resp, err := key.Get(begun.Options, approverOrigin)
	require.NoError(t, err)
	rr = do(mux, ipBrowser(base+"/finish", url.Values{"ticket": {begun.Ticket}, "response": {string(resp)}}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	approved, err := store.DeviceByID(ctx, db, laptop.ID)
	require.NoError(t, err)
	require.NotEmpty(t, approved.ApprovedBy)
	require.Equal(t, laptop.Fingerprint, approved.Fingerprint, "the same certificate")
	require.Equal(t, store.DeviceActive, approved.Status)
	evs, err := events.NewRecorder(db).ByKinds(ctx, []core.EventKind{devices.EventPaired}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, true, evs[0].Payload["in_place"])

	page = ipPage(t, mux)
	require.Empty(t, page.InPlace)
	require.ElementsMatch(t, []string{laptop.ID, studio.ID}, ipIDs(page.Paired), "paired, as a passkey-approved relay")

	// A revoked relay is not approved, whatever the page once offered.
	rr = do(mux, ipBrowser("/console/devices/"+old.ID+"/approve/begin", url.Values{}))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "not waiting for approval")
}
