package devices

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// judgeBoot is the relay computer's boot in these tests.
const judgeBoot = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

// judgeTok is one process of the relay's computer, as the relay renders it.
func judgeTok(pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: judgeBoot}
}

// judgeAttrs and judgeChainHeader are one well-formed forwarded caller: uid
// 501, a bridge under an agent under a terminal.
var (
	judgeAttrs       = RelayAttributes{UID: 501, Chain: []access.ProcessToken{judgeTok(2210), judgeTok(2200), judgeTok(2100)}}
	judgeChainHeader = judgeTok(2210).Key() + "," + judgeTok(2200).Key() + "," + judgeTok(2100).Key()
)

// judgeHeaders is the relay's three headers with the given values.
func judgeHeaders(version, uid, chain string) http.Header {
	return http.Header{RelayVersionHeader: {version}, RelayUIDHeader: {uid}, RelayChainHeader: {chain}}
}

// The wire, version 1: three headers, each once, strictly; anything else is
// refused whole, and a request without any carries none.
func TestParseRelayAttributes_TheWire(t *testing.T) {
	got, present, err := ParseRelayAttributes(judgeHeaders("1", "501", judgeChainHeader))
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, judgeAttrs, got)
	for _, tok := range got.Chain {
		require.Empty(t, tok.Relay, "which relay a process belongs to is the server's to say, never the wire's")
	}

	_, present, err = ParseRelayAttributes(http.Header{"Authorization": {"Bearer x"}, "X-Seamless-Host": {"example-laptop"}})
	require.NoError(t, err)
	require.False(t, present, "no relay header at all: nothing forwarded")

	max := make([]string, access.MaxChainHops)
	for i := range max {
		max[i] = judgeTok(int32(3000 + i)).Key()
	}
	got, _, err = ParseRelayAttributes(judgeHeaders("1", "4294967295", strings.Join(max, ",")))
	require.NoError(t, err, "the largest uid and the longest chain")
	require.Equal(t, uint32(4294967295), got.UID)
	require.Len(t, got.Chain, access.MaxChainHops)

	tooLong := strings.Join(append(max, judgeTok(9).Key()), ",")
	dup := judgeHeaders("1", "501", judgeChainHeader)
	dup.Add(RelayUIDHeader, "0")
	extra := judgeHeaders("1", "501", judgeChainHeader)
	extra.Set("X-Seamless-Relay-Name", "example-laptop")
	for name, h := range map[string]http.Header{
		"no version":                    {RelayUIDHeader: {"501"}, RelayChainHeader: {judgeChainHeader}},
		"version 2":                     judgeHeaders("2", "501", judgeChainHeader),
		"version with space":            judgeHeaders("1 ", "501", judgeChainHeader),
		"no uid":                        {RelayVersionHeader: {"1"}, RelayChainHeader: {judgeChainHeader}},
		"uid not decimal":               judgeHeaders("1", "50a", judgeChainHeader),
		"negative uid":                  judgeHeaders("1", "-1", judgeChainHeader),
		"uid past 2^32-1":               judgeHeaders("1", "4294967296", judgeChainHeader),
		"empty uid":                     judgeHeaders("1", "", judgeChainHeader),
		"no chain":                      {RelayVersionHeader: {"1"}, RelayUIDHeader: {"501"}},
		"empty chain":                   judgeHeaders("1", "501", ""),
		"too many hops":                 judgeHeaders("1", "501", tooLong),
		"an empty hop":                  judgeHeaders("1", "501", judgeChainHeader+","),
		"a space after a comma":         judgeHeaders("1", "501", judgeTok(2210).Key()+", "+judgeTok(2200).Key()),
		"pid zero":                      judgeHeaders("1", "501", judgeBoot+":0:5"),
		"a leading zero":                judgeHeaders("1", "501", judgeBoot+":0221:5"),
		"a plus sign":                   judgeHeaders("1", "501", judgeBoot+":+221:5"),
		"two parts":                     judgeHeaders("1", "501", judgeBoot+":221"),
		"four parts":                    judgeHeaders("1", "501", judgeBoot+":221:5:6"),
		"a boot with a slash":           judgeHeaders("1", "501", "a/b:221:5"),
		"a relay-qualified key":         judgeHeaders("1", "501", access.ProcessToken{PID: 221, Version: 5, Boot: judgeBoot, Relay: "01RELAY"}.Key()),
		"a header set twice":            dup,
		"a relay header the wire lacks": extra,
	} {
		t.Run(name, func(t *testing.T) {
			_, present, err := ParseRelayAttributes(h)
			require.True(t, present)
			require.ErrorIs(t, err, ErrRelayAttributes)
		})
	}
}

// The edge reads a relay's forwarded attributes only under the relay's own
// credential: well formed, they ride the context; malformed, the request is
// refused before any route; absent, the relay is judged as before. A bearer
// request or a key device carrying the same headers has them stripped
// unread. No route ever sees the raw headers.
func TestEdge_RelayAttributesOnlyUnderARelaysCredential(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 16)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	d := f.pairRelayDevice(t, "example-laptop", id)
	relay := deviceClient(pool, id)

	status, _, p, reached := deviceGet(t, relay, ts.URL+"/api/mcp", judgeHeaders("1", "501", judgeChainHeader), seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, reached)
	require.Equal(t, d.ID, p.cred.DeviceID)
	require.True(t, p.forwarded)
	require.Equal(t, judgeAttrs, p.relay)
	require.Empty(t, p.relayHdrs)

	status, body, _, reached := deviceGet(t, relay, ts.URL+"/api/mcp", judgeHeaders("1", "50a", judgeChainHeader), seen)
	require.Equal(t, http.StatusBadRequest, status, "a malformed report is refused")
	require.Contains(t, body, ErrRelayAttributes.Error())
	require.False(t, reached, "nothing reaches a route")

	status, _, p, reached = deviceGet(t, relay, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusOK, status, "a relay that forwards nothing is judged as before")
	require.True(t, reached)
	require.False(t, p.forwarded)

	_, key := f.pairKeyDevice(t, "example-ci")
	for name, auth := range map[string]string{"the shared key": "Bearer shared-key", "a key device": "Bearer " + key} {
		h := judgeHeaders("1", "501", judgeChainHeader)
		h.Set("Authorization", auth)
		status, _, p, reached := deviceGet(t, deviceClient(pool, nil), ts.URL+"/api/mcp", h, seen)
		require.Equal(t, http.StatusOK, status, name)
		require.True(t, reached, name)
		require.False(t, p.forwarded, "%s: relay attributes are never read from a bearer request", name)
		require.Empty(t, p.relayHdrs, "%s: and never reach a route", name)

		h = judgeHeaders("1", "50a", "garbage")
		h.Set("Authorization", auth)
		status, _, p, _ = deviceGet(t, deviceClient(pool, nil), ts.URL+"/api/mcp", h, seen)
		require.Equal(t, http.StatusOK, status, "%s: a bearer request's relay headers are not read at all, so not judged", name)
		require.False(t, p.forwarded, name)
	}
}

// Credential.Approved is whether a passkey approved the pairing: never for a
// pairing the code alone approved (peer_auth off), always under peer_auth.
func TestEdge_CredentialSaysWhetherAPasskeyApprovedThePairing(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 4)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	f.pairRelayDevice(t, "example-laptop", id)
	_, _, p, reached := deviceGet(t, deviceClient(pool, id), ts.URL+"/api/mcp", nil, seen)
	require.True(t, reached)
	require.False(t, p.cred.Approved, "a pairing the code alone approved")

	g := pairNewFixture(t, true)
	gseen := make(chan deviceProbe, 4)
	gts, gpool, _ := g.deviceServer(t, gseen, nil)
	gid := pairIdentity(t, "example-desk")
	answer, err := g.svc.Pair(context.Background(), PairInput{Code: g.pairCode(t), Kind: KindRelay, Name: "example-desk",
		Secret: pairSecret(t), RelayKey: gid.PublicKey()})
	require.NoError(t, err)
	_, err = store.ApproveDevice(context.Background(), g.db, answer.Device.ID, answer.Device.Fingerprint, "01PASSKEY", g.pairNow())
	require.NoError(t, err)
	_, _, p, reached = deviceGet(t, deviceClient(gpool, gid), gts.URL+"/api/mcp", nil, gseen)
	require.True(t, reached)
	require.True(t, p.cred.Approved, "approved with a passkey")
}

// judgeRelayLease plants a live lease on project for process of relay, as an
// approval of the relay's request leaves one.
func judgeRelayLease(t *testing.T, f *pairFixture, relay string, process access.ProcessToken, project string) store.AccessLease {
	t.Helper()
	ctx := context.Background()
	process.Relay = relay
	a, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: project, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: process.PID, ProcessKey: process.Key()}, f.pairNow())
	require.NoError(t, err)
	lease, err := store.ApproveAccess(ctx, f.db, a.ID, a.Challenge, "daemon-account", f.pairNow())
	require.NoError(t, err)
	return lease
}

// Revoking a relay ends its leases with it, and says so the way every early
// end of agents' access is said; another relay's lease stays.
func TestRevoke_EndsTheRelaysLeases(t *testing.T) {
	f := pairNewFixture(t, false)
	ctx := context.Background()
	laptop := f.pairRelayDevice(t, "example-laptop", pairIdentity(t, "example-laptop"))
	desk := f.pairRelayDevice(t, "example-desk", pairIdentity(t, "example-desk"))
	mine := judgeRelayLease(t, f, laptop.ID, judgeTok(2200), "example-finance")
	other := judgeRelayLease(t, f, desk.ID, judgeTok(2200), "example-finance")
	require.Equal(t, laptop.ID, mine.RelayID)

	_, err := f.svc.Revoke(ctx, laptop.ID, store.DeviceByConsole)
	require.NoError(t, err)
	live, err := store.ActiveAccessLeasesFor(ctx, f.db, []string{mine.ProcessKey, other.ProcessKey}, f.pairNow())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, other.ID, live[0].ID, "only the revoked relay's lease ends")

	evs, err := events.NewRecorder(f.db).ByKinds(ctx, []core.EventKind{EventRevoked, core.EventAccessRevoked}, "", "", 10)
	require.NoError(t, err)
	var revoked, accessEnded map[string]any
	for _, e := range evs {
		switch e.Kind {
		case EventRevoked:
			revoked = e.Payload
		case core.EventAccessRevoked:
			accessEnded = e.Payload
		}
	}
	require.NotNil(t, revoked)
	require.EqualValues(t, 1, revoked["leases"], "the device's revocation counts the leases it ended")
	require.NotNil(t, accessEnded, "access.revoked is recorded, as for any other early end")
	require.Equal(t, store.LeaseRevokedDevice, accessEnded["reason"])
	require.EqualValues(t, 1, accessEnded["leases"])
}
