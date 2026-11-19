package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// The owner's own hands through a relay (owner decision R-D3). The
// transport (cmd/seamlessd's relayHandler) judges a mapped relay's chain
// and carries the judgment on the request; the console lets the owner's
// decisions through only for a judgment that says owner, for that relay, on
// the owner's socket view through it. Every other relay request, every key
// device, and every relay request without peer_auth is refused, saying why.

// crhPrograms is the owner's terminal on the relay's computer, as a relay of
// wire version 2 names it, the peer first.
var crhPrograms = []string{"/Users/owner/.local/bin/seam", "/bin/zsh", "/usr/bin/login",
	"/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"}

// crhRelayCtx is what relayHandler gives the owner's request through the
// relay cred names: the owner's socket view through it, with hands as its
// judgment of the caller (FromAgent answers it, as there).
func crhRelayCtx(t *testing.T, cred devices.Credential, why string) context.Context {
	t.Helper()
	policy, err := access.New(config.PeerAuth{Owner: "owner", Relays: map[string]config.PeerRelay{cred.Name: {OwnerUID: 501}}},
		func(n string) (access.Account, error) { return access.Account{UID: 501, Name: n}, nil })
	require.NoError(t, err)
	chain := []access.ProcessToken{{PID: 4100, Version: 7, Boot: "boot-a"}, {PID: 4000, Version: 3, Boot: "boot-a"},
		{PID: 3900, Version: 2, Boot: "boot-a"}, {PID: 3800, Version: 1, Boot: "boot-a"}}
	view, ok := policy.RelayChainViewNamed(cred.DeviceID, cred.Name, 501, chain, crhPrograms)
	require.True(t, ok)
	hands := access.RelayHands{Relay: cred.DeviceID, Name: cred.Name, Programs: view.Principal().Programs, Why: why}
	ctx := access.WithView(devices.WithCredential(context.Background(), cred), view)
	ctx = access.WithAgentChain(ctx, func() bool { return !hands.Owner() })
	return access.WithRelayHands(ctx, hands)
}

// crhRelayDecisions reads the relay decisions the event log holds.
func crhRelayDecisions(t *testing.T, rec *events.Recorder) []core.Event {
	t.Helper()
	evs, err := rec.ByKinds(context.Background(), []core.EventKind{devices.EventRelayDecision}, "", "", 100)
	require.NoError(t, err)
	return evs
}

// Under peer_auth, a chain the transport judged the owner's own hands passes
// every owner-decision guard: the ownerHands routes, a deny, the revokes,
// Lock again, and an approval with no proof, which goes on to the approve
// core (and is refused there for want of a proof: the hands are no proof). A
// judgment that says no is refused on every one of them with why, as is a
// key device whatever its context carries.
func TestConsole2_RelayOwnerHands_UnderPeerAuth(t *testing.T) {
	var pairing *devices.Service
	lc := signinNewLockedConsole(t, func(c *Config) {
		pairing = devices.New(devices.Config{DB: c.WriteDB, LocalHost: "example-server",
			Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true}})
		c.Devices = pairing
	})
	rec := events.NewRecorder(lc.db)
	routes := append(con2OwnerHandsRoutes(t),
		con2Route{http.MethodPost, "/console/access/leases/01NOSUCHITEM/revoke"},
		con2Route{http.MethodPost, "/console/access/unlocks/01NOSUCHITEM/end"},
		con2Route{http.MethodPost, "/console/access/01NOSUCHITEM/deny"},
		con2Route{http.MethodPost, "/console/access/01NOSUCHITEM/approve"},
	)
	creds := con2Creds(t)
	relay, key := creds[0], creds[1]

	const why = "Claude Code 2.1.319 (pid 4000 on example-laptop) is in its process chain: an agent started it"
	for _, rt := range routes {
		rr := lc.do(con2Req(crhRelayCtx(t, relay, why), rt, url.Values{}))
		con2RequireDeviceRefused(t, rr, relay, "an agent's chain "+rt.method+" "+rt.path)
		require.Contains(t, rr.Body.String(), why, rt.path)
		require.Contains(t, rr.Body.String(), "a terminal of your own on that computer, with no agent above it", rt.path)

		// A key device is never judged: a judgment its context carries
		// changes nothing.
		kctx := access.WithRelayHands(devices.WithCredential(context.Background(), key),
			access.RelayHands{Relay: key.DeviceID, Name: key.Name, Programs: crhPrograms})
		con2RequireDeviceRefused(t, lc.do(con2Req(kctx, rt, url.Values{})), key, "a key device "+rt.method+" "+rt.path)
	}
	open, err := store.CountOpenPairingCodes(context.Background(), lc.db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no refused request minted a code")
	require.Empty(t, crhRelayDecisions(t, rec), "a refused request records no decision")

	for _, rt := range routes {
		rr := lc.do(con2Req(crhRelayCtx(t, relay, ""), rt, url.Values{}))
		con2RequireNotRefused(t, rr, "the owner's hands "+rt.method+" "+rt.path)
		require.NotContains(t, rr.Body.String(), "revoke from the console, or with the TCP key", rt.path)
		require.NotContains(t, rr.Body.String(), "deny from the console", rt.path)
	}
	// The approval went on to the core, which takes none without a proof.
	rr := lc.do(con2Req(crhRelayCtx(t, relay, ""), con2Route{http.MethodPost, "/console/access/01NOSUCHITEM/approve"}, url.Values{}))
	require.NotEqual(t, http.StatusOK, rr.Code, rr.Body.String())
}

// Each decision the owner's hands make through a relay records one event
// naming the relay (its name and device) and the chain's programs by base
// name; a route that refused or failed records none.
func TestConsole2_RelayOwnerHands_RecordsEachDecision(t *testing.T) {
	var pairing *devices.Service
	lc := signinNewLockedConsole(t, func(c *Config) {
		pairing = devices.New(devices.Config{DB: c.WriteDB, LocalHost: "example-server",
			Server: devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true}})
		c.Devices = pairing
	})
	rec := events.NewRecorder(lc.db)
	relay := con2Creds(t)[0]

	rr := lc.do(con2Req(crhRelayCtx(t, relay, ""), con2Route{http.MethodPost, "/console/devices/code"}, url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), `"code"`)
	evs := crhRelayDecisions(t, rec)
	require.Len(t, evs, 1)
	require.Equal(t, relay.DeviceID, evs[0].ItemID)
	require.Equal(t, relay.Name, evs[0].Payload["name"])
	require.Equal(t, "POST /console/devices/code", evs[0].Payload["route"])
	require.EqualValues(t, 501, evs[0].Payload["uid"])
	require.Equal(t, []any{"seam", "zsh", "login", "Terminal"}, evs[0].Payload["programs"], "base names, never a path")
	require.NotContains(t, evs[0].Payload, "path")

	// A decision that fails records nothing: the lease is not there.
	rr = lc.do(con2Req(crhRelayCtx(t, relay, ""), con2Route{http.MethodPost, "/console/access/leases/01NOSUCHITEM/revoke"}, url.Values{}))
	require.GreaterOrEqual(t, rr.Code, http.StatusBadRequest, rr.Body.String())
	// Nor does a write that is no owner's decision, which any caller makes.
	rr = lc.do(con2Req(crhRelayCtx(t, relay, ""), con2Route{http.MethodPost, "/console/settings/level"}, url.Values{"level": {"basic"}}))
	require.Less(t, rr.Code, http.StatusBadRequest, rr.Body.String())
	require.Len(t, crhRelayDecisions(t, rec), 1)
}

// Without peer_auth no chain is judged: a relay's request carries no View,
// and a judgment that says owner -- forged into its context -- passes
// nothing. The refusal says why.
func TestConsole2_RelayOwnerHands_NeverWithoutPeerAuth(t *testing.T) {
	db, mux := con2PlainConsole(t)
	relay := con2Creds(t)[0]
	forged := access.WithRelayHands(devices.WithCredential(context.Background(), relay),
		access.RelayHands{Relay: relay.DeviceID, Name: relay.Name, Programs: crhPrograms})
	for _, rt := range con2OwnerHandsRoutes(t) {
		for name, ctx := range map[string]context.Context{"no judgment": devices.WithCredential(context.Background(), relay), "forged": forged} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, con2Req(ctx, rt, url.Values{}))
			con2RequireDeviceRefused(t, rr, relay, name+" "+rt.method+" "+rt.path)
		}
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, con2Req(devices.WithCredential(context.Background(), relay), con2Route{http.MethodPost, "/console/devices/code"}, url.Values{}))
	require.True(t, strings.Contains(rr.Body.String(), "judges one only under peer_auth"), rr.Body.String())
	open, err := store.CountOpenPairingCodes(context.Background(), db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open)
}
