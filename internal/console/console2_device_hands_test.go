package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A paired device carries agents, never the owner's hands (review H1, wave
// nt1113e): its credential -- a relay's client certificate, a key device's
// key, which the daemon's edge matched -- makes none of the owner's
// decisions, with peer_auth on or off. The shared bearer key and the owner's
// browser cookie make them as before.

// con2DeviceRefusal is what every refusal of a device's request says.
const con2DeviceRefusal = "never the owner's hands"

// con2Route is one console route: its method and a path to request.
type con2Route struct{ method, path string }

// con2OwnerHandsRoutes is every route console.go registers through ownerHands,
// read from the source, so a route added there later is covered here too.
func con2OwnerHandsRoutes(t *testing.T) []con2Route {
	t.Helper()
	src, err := os.ReadFile("console.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^\s*(?:post|handle)\("(GET|POST) (/console/[^"]+)",.*\bownerHands\(`)
	param := regexp.MustCompile(`\{[^}]+\}`)
	var out []con2Route
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out = append(out, con2Route{method: m[1], path: param.ReplaceAllString(m[2], "01NOSUCHITEM")})
	}
	require.GreaterOrEqual(t, len(out), 17, "the parse found the ownerHands routes: %v", out)
	require.Contains(t, out, con2Route{http.MethodPost, "/console/devices/code"})
	require.Contains(t, out, con2Route{http.MethodGet, "/console/settings/backups/folder"})
	return out
}

// con2Creds are the two kinds of paired device, as the edge marks a request.
func con2Creds(t *testing.T) []devices.Credential {
	t.Helper()
	relayID, err := core.NewID()
	require.NoError(t, err)
	keyID, err := core.NewID()
	require.NoError(t, err)
	return []devices.Credential{
		{DeviceID: relayID, Kind: devices.KindRelay, Name: "example-laptop", Approved: true},
		{DeviceID: keyID, Kind: devices.KindKey, Name: "example-ci"},
	}
}

// con2Req is a request of rt, as JSON, with form when it posts.
func con2Req(ctx context.Context, rt con2Route, form url.Values) *http.Request {
	var body *strings.Reader
	if form == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(rt.method, "http://localhost:8081"+rt.path, body).WithContext(ctx)
	if rt.method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	return req
}

// con2RequireDeviceRefused checks a device's refusal: 403, the JSON shape
// refusedFromAgent answers with ({"error": ...}), naming the device and where
// the owner decides instead.
func con2RequireDeviceRefused(t *testing.T, rr *httptest.ResponseRecorder, cred devices.Credential, what string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", what, rr.Body.String())
	var answer map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &answer), "%s: %s", what, rr.Body.String())
	require.Len(t, answer, 1, what)
	msg := answer["error"]
	require.Contains(t, msg, "the owner's decision", what)
	require.Contains(t, msg, con2DeviceRefusal, what)
	require.Contains(t, msg, cred.Name, what)
	require.Contains(t, msg, "the console of the server you sign in to", what)
	require.Contains(t, msg, "a terminal on the server itself", what)
	if cred.Kind == devices.KindRelay {
		require.Contains(t, msg, "through the relay", what)
	} else {
		require.Contains(t, msg, "with the device key", what)
	}
}

// con2RequireNotRefused checks that neither a device's refusal nor an
// agent's answered: what the route does next (a 404 for a feature that is
// off, an error for a missing id) is its own.
func con2RequireNotRefused(t *testing.T, rr *httptest.ResponseRecorder, what string) {
	t.Helper()
	body := rr.Body.String()
	require.NotContains(t, body, con2DeviceRefusal, "%s: %d %s", what, rr.Code, body)
	require.NotContains(t, body, "process an agent started", "%s: %d %s", what, rr.Code, body)
}

// con2PlainConsole is a console without peer_auth that pairs devices (the
// code is the approval), with the bearer key.
func con2PlainConsole(t *testing.T) (*sql.DB, http.Handler) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	pairing := devices.New(devices.Config{DB: db, Events: rec, LocalHost: "example-server",
		Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true}})
	cs, err := New(Config{DB: db, Events: rec, APIKey: testKey, Level: "advanced",
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil), Devices: pairing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	return db, mux
}

// Without peer_auth, a relay and a key device are each refused on every
// ownerHands route, the pairing code the review minted through a relay
// included, and nothing is minted; the bearer key and the owner's browser
// cookie still pass.
func TestConsole2_DeviceRefusedOnEveryOwnerHandsRoute_WithoutPeerAuth(t *testing.T) {
	db, mux := con2PlainConsole(t)
	routes := con2OwnerHandsRoutes(t)
	for _, cred := range con2Creds(t) {
		for _, rt := range routes {
			ctx := devices.WithCredential(context.Background(), cred)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, con2Req(ctx, rt, url.Values{}))
			con2RequireDeviceRefused(t, rr, cred, cred.Kind+" "+rt.method+" "+rt.path)
		}
	}
	open, err := store.CountOpenPairingCodes(context.Background(), db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no device minted a pairing code")

	for _, rt := range routes {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, authedReq(con2Req(context.Background(), rt, url.Values{})))
		con2RequireNotRefused(t, rr, "bearer "+rt.method+" "+rt.path)

		req := con2Req(context.Background(), rt, url.Values{})
		req.AddCookie(consoleCookie())
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rr = httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		con2RequireNotRefused(t, rr, "cookie "+rt.method+" "+rt.path)
	}
	// The key still mints a code: the owner's own key is the owner.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authedReq(con2Req(context.Background(), con2Route{http.MethodPost, "/console/devices/code"}, url.Values{})))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), `"code"`)
}

// con2RelayView is what the daemon's relayHandler gives a mapped relay's
// request from the owner's uid there: the owner's socket view through the
// relay, whose chain counts as an agent's.
func con2RelayView(t *testing.T, ctx context.Context, cred devices.Credential) context.Context {
	t.Helper()
	policy, err := access.New(config.PeerAuth{Owner: "owner", Relays: map[string]config.PeerRelay{cred.Name: {OwnerUID: 501}}},
		func(n string) (access.Account, error) { return access.Account{UID: 501, Name: n}, nil })
	require.NoError(t, err)
	view, ok := policy.RelayChainViewNamed(cred.DeviceID, cred.Name, 501,
		[]access.ProcessToken{{PID: 4100, Version: 7, Boot: "boot-a"}, {PID: 4000, Version: 3, Boot: "boot-a"}}, []string{"seam", "zsh"})
	require.True(t, ok)
	require.True(t, view.Principal().Socket)
	return access.WithAgentChain(access.WithView(ctx, view), func() bool { return true })
}

// Under peer_auth (a locked project, approvers on): a mapped relay's request
// (the owner's uid there, judged as the socket is) and a key device's (a TCP
// caller, the owner's view) are each refused on every ownerHands route, and
// on the access routes that are the owner's decisions; the bearer key and the
// owner's browser pass.
func TestConsole2_DeviceRefusedOnEveryOwnerHandsRoute_UnderPeerAuth(t *testing.T) {
	var pairing *devices.Service
	lc := signinNewLockedConsole(t, func(c *Config) {
		pairing = devices.New(devices.Config{DB: c.WriteDB, LocalHost: "example-server",
			Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true}})
		c.Devices = pairing
	})
	routes := append(con2OwnerHandsRoutes(t),
		con2Route{http.MethodPost, "/console/access/leases/01NOSUCHITEM/revoke"},
		con2Route{http.MethodPost, "/console/access/unlocks/01NOSUCHITEM/end"},
		con2Route{http.MethodPost, "/console/access/01NOSUCHITEM/deny"},
		con2Route{http.MethodPost, "/console/access/01NOSUCHITEM/approve"},
		con2Route{http.MethodPost, "/console/approvers/passkey/begin"},
		con2Route{http.MethodPost, "/console/approvers/passkey/finish"},
		con2Route{http.MethodPost, "/console/approvers/passkey/confirm"},
		con2Route{http.MethodPost, "/console/projects/" + openSlug + "/isolation?state=open"},
	)
	require.NoError(t, store.SetProjectIsolation(context.Background(), lc.db, openSlug, core.IsolationConfidential))
	for _, cred := range con2Creds(t) {
		for _, rt := range routes {
			ctx := devices.WithCredential(context.Background(), cred)
			if cred.Kind == devices.KindRelay {
				ctx = con2RelayView(t, ctx, cred)
			}
			rr := lc.do(con2Req(ctx, rt, url.Values{"name": {"laptop key"}}))
			con2RequireDeviceRefused(t, rr, cred, cred.Kind+" "+rt.method+" "+rt.path)
		}
	}
	got, err := store.IsolationOf(context.Background(), lc.db, openSlug)
	require.NoError(t, err)
	require.Equal(t, core.IsolationConfidential, got, "no device brought the fence down")

	for _, rt := range routes {
		rr := lc.do(authedReq(con2Req(context.Background(), rt, url.Values{})))
		con2RequireNotRefused(t, rr, "bearer "+rt.method+" "+rt.path)
		form := url.Values{}
		if rt.method == http.MethodGet {
			form = nil
		}
		browser := lc.browser(rt.method, rt.path, "", form)
		browser.Header.Set("Accept", "application/json")
		rr = lc.do(browser)
		con2RequireNotRefused(t, rr, "browser "+rt.method+" "+rt.path)
	}
}

// The advisory tier (read grants without peer_auth): a paired device neither
// approves a grant without a proof, nor denies one, nor revokes one -- each
// the bearer key's decision there, which a device's credential is not. A
// device's approval that carries a proof is judged by the approve core, as the
// Touch ID helper's through its relay is.
func TestConsole2_AdvisoryTier_DeviceDecidesNothing(t *testing.T) {
	g := newGrantConsole(t)
	ctx := context.Background()
	rr := g.post(t, "/console/access/request", url.Values{"project": {"example-finance"}, "reason": {"check the totals"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var filed struct {
		ID    string `json:"id"`
		Short string `json:"short_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	var one struct {
		Request approval.Facts `json:"request"`
	}
	g.getJSON(t, "/console/access/"+filed.Short, &one)
	decision := url.Values{"duration": {"1h"}, "digest": {one.Request.Digest}}

	pending := func() {
		t.Helper()
		a, err := store.AccessApprovalByID(ctx, g.db, filed.ID)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, a.Status)
	}
	for _, cred := range con2Creds(t) {
		dctx := devices.WithCredential(ctx, cred)
		rr := do(g.mux, con2Req(dctx, con2Route{http.MethodPost, "/console/access/" + filed.ID + "/approve"}, decision))
		con2RequireDeviceRefused(t, rr, cred, cred.Kind+" approve")
		pending()
		rr = do(g.mux, con2Req(dctx, con2Route{http.MethodPost, "/console/access/" + filed.ID + "/deny"}, url.Values{}))
		con2RequireDeviceRefused(t, rr, cred, cred.Kind+" deny")
		pending()
		// A proof the core cannot verify approves nothing, and is not the
		// device's refusal: the core judged it.
		withProof := url.Values{"duration": {"1h"}, "digest": {one.Request.Digest}, "response": {"{}"}}
		rr = do(g.mux, con2Req(dctx, con2Route{http.MethodPost, "/console/access/" + filed.ID + "/approve"}, withProof))
		con2RequireNotRefused(t, rr, cred.Kind+" approve with a proof")
		require.NotEqual(t, http.StatusOK, rr.Code, rr.Body.String())
		pending()
	}

	// The bearer key's decision lands; a device cannot revoke the grant.
	rr = g.post(t, "/console/access/"+filed.ID+"/approve", decision, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	live, err := store.LiveAccessGrants(ctx, g.db, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	for _, cred := range con2Creds(t) {
		rr := do(g.mux, con2Req(devices.WithCredential(ctx, cred),
			con2Route{http.MethodPost, "/console/access/grants/" + live[0].ID + "/revoke"}, url.Values{}))
		con2RequireDeviceRefused(t, rr, cred, cred.Kind+" revoke")
	}
	still, err := store.LiveAccessGrants(ctx, g.db, time.Now())
	require.NoError(t, err)
	require.Len(t, still, 1, "the grant is still live")
	rr = g.post(t, "/console/access/grants/"+live[0].ID+"/revoke", url.Values{}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

// Reads stay open to a paired device: seam on a client computer reads the
// console's JSON through its relay (seam access, seam plan, seam doctor, the
// Touch ID helper's challenge), as an agent on the server's own socket does.
func TestConsole2_DeviceReadsStayOpen(t *testing.T) {
	_, mux := con2PlainConsole(t)
	for _, cred := range con2Creds(t) {
		for _, path := range []string{"/console/?format=json", "/console/settings?format=json", "/console/plans?format=json",
			"/console/sessions?format=json", "/console/devices?format=json"} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, con2Req(devices.WithCredential(context.Background(), cred), con2Route{http.MethodGet, path}, nil))
			require.Equal(t, http.StatusOK, rr.Code, "%s %s: %s", cred.Kind, path, rr.Body.String())
		}
	}
}
