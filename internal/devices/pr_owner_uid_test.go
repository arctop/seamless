package devices

// Owner decision R-D1 (2026-11-15): a relay's pairing records the owner's uid
// on its computer, and a server without peer_auth serves that uid alone
// through the relay.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// prPairRelayFor pairs a relay named name through the service, naming owner
// as the owner's uid on its computer (0: none, as a relay of an earlier
// release pairs).
func (f *pairFixture) prPairRelayFor(t *testing.T, name string, id *RelayIdentity, owner uint32) store.Device {
	t.Helper()
	answer, err := f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindRelay, Name: name,
		Secret: pairSecret(t), RelayKey: id.PublicKey(), OwnerUID: owner})
	require.NoError(t, err)
	return answer.Device
}

// prV2Headers is a version 2 relay's headers for uid, one named process.
func prV2Headers(uid string) http.Header {
	return http.Header{RelayVersionHeader: {RelayWireVersion2}, RelayUIDHeader: {uid}, RelayChainHeader: {judgeTok(2200).Key()},
		RelayProgramsHeader: {EncodeRelayProgram("/usr/local/bin/claude")}}
}

// Without peer_auth, a relay whose pairing recorded the owner's uid is
// served for that uid alone: another uid is refused with the remedy, a
// request that forwards no caller is refused, and the health check is not
// judged. A relay whose pairing recorded none is served as before, for every
// uid.
func TestPROwnerUID_EdgeServesTheRecordedOwnerAlone(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 16)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	bound := f.prPairRelayFor(t, "example-laptop", id, 501)
	require.Equal(t, uint32(501), bound.OwnerUID)
	require.False(t, bound.OwnerUnbound())
	client := deviceClient(pool, id)

	for name, h := range map[string]http.Header{"v1": judgeHeaders("1", "501", judgeChainHeader), "v2": prV2Headers("501")} {
		status, body, p, ok := deviceGet(t, client, ts.URL+"/api/mcp", h, seen)
		require.Equal(t, http.StatusOK, status, "%s: %s", name, body)
		require.True(t, ok, name)
		require.Equal(t, bound.ID, p.cred.DeviceID, "%s: the owner's uid is the owner, as every caller was", name)
	}
	for name, h := range map[string]http.Header{"v1": judgeHeaders("1", "502", judgeChainHeader), "v2": prV2Headers("502"),
		"root": judgeHeaders("1", "0", judgeChainHeader)} {
		status, body, _, ok := deviceGet(t, client, ts.URL+"/api/mcp", h, seen)
		require.Equal(t, http.StatusForbidden, status, "%s: %s", name, body)
		require.False(t, ok, "%s: nothing reached a route", name)
		require.Contains(t, body, "is not the owner's uid there (501, recorded when the relay paired)", name)
		require.Contains(t, body, "turn peer_auth on and map it as restricted", "%s: the refusal names the remedy", name)
		require.Contains(t, body, "peer_auth.relays: {example-laptop: {owner_uid: 501, restricted: {<its name>: ", name)
	}
	status, body, _, ok := deviceGet(t, client, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.False(t, ok, "a request that forwards no caller is never served as the owner")
	require.Contains(t, body, "this server serves uid 501 there, the owner's, and no one else through it")
	status, body, p, ok := deviceGet(t, client, ts.URL+HealthPath, nil, seen)
	require.Equal(t, http.StatusOK, status, "the relay's own probe of the server forwards no caller, and is not judged: %s", body)
	require.True(t, ok)
	require.Equal(t, bound.ID, p.cred.DeviceID)

	// Paired as a relay of an earlier release pairs, naming no uid: served as
	// before, whoever it forwards.
	old := pairIdentity(t, "example-desk")
	unbound := f.prPairRelayFor(t, "example-desk", old, 0)
	require.True(t, unbound.OwnerUnbound())
	for name, h := range map[string]http.Header{"the owner": judgeHeaders("1", "501", judgeChainHeader),
		"another uid": judgeHeaders("1", "502", judgeChainHeader), "no caller": nil} {
		status, body, p, ok := deviceGet(t, deviceClient(pool, old), ts.URL+"/api/mcp", h, seen)
		require.Equal(t, http.StatusOK, status, "%s: %s", name, body)
		require.True(t, ok, name)
		require.Equal(t, unbound.ID, p.cred.DeviceID, name)
	}

	// A key device names no uid, and is the bearer request it always was.
	_, key := f.pairKeyDevice(t, "example-ci")
	status, _, _, ok = deviceGet(t, deviceClient(pool, nil), ts.URL+"/api/mcp", http.Header{"Authorization": {"Bearer " + key}}, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, ok)
}

// Under peer_auth the edge judges no uid: the daemon's relay judge does,
// from peer_auth.relays, whatever uid the pairing recorded.
func TestPROwnerUID_UnderPeerAuthTheEdgeJudgesNoUID(t *testing.T) {
	f := pairNewFixture(t, true)
	seen := make(chan deviceProbe, 4)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	pending := f.prPairRelayFor(t, "example-laptop", id, 501)
	require.Equal(t, store.DevicePending, pending.Status)
	require.Equal(t, uint32(501), pending.OwnerUID, "recorded while it waits")
	_, err := store.ApproveDevice(context.Background(), f.db, pending.ID, pending.Fingerprint, "01PASSKEY", f.pairNow())
	require.NoError(t, err)
	for name, h := range map[string]http.Header{"another uid": judgeHeaders("1", "502", judgeChainHeader), "no caller": nil} {
		status, body, p, ok := deviceGet(t, deviceClient(pool, id), ts.URL+"/api/mcp", h, seen)
		require.Equal(t, http.StatusOK, status, "%s: %s", name, body)
		require.True(t, ok, "%s: the request reached the routes, for the relay judge there", name)
		require.Equal(t, uint32(501), p.cred.OwnerUID, name)
	}
}

// The pairing endpoint records a relay's owner_uid, refuses one from a key
// device, and a status request carries the secret alone.
func TestPROwnerUID_PairEndpoint(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, _ := f.deviceServer(t, nil, nil)
	post := func(client *http.Client, path string, body map[string]any) (int, pairError, PairAnswer) {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		resp, err := client.Post(ts.URL+path, "application/json", bytes.NewReader(raw))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var refused pairError
		var answer PairAnswer
		if resp.StatusCode == http.StatusOK {
			require.NoError(t, json.Unmarshal(data, &answer))
		} else {
			require.NoError(t, json.Unmarshal(data, &refused))
		}
		return resp.StatusCode, refused, answer
	}
	id := pairIdentity(t, "example-laptop")
	secret := pairSecret(t)
	status, _, answer := post(deviceClient(pool, id), PairPath, map[string]any{"code": f.pairCode(t), "kind": KindRelay,
		"name": "example-laptop", "secret": secret, "owner_uid": 501})
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, uint32(501), answer.Device.OwnerUID, "the answer shows what the server recorded")
	d, err := store.DeviceByID(context.Background(), f.db, answer.Device.ID)
	require.NoError(t, err)
	require.Equal(t, uint32(501), d.OwnerUID)

	_, unused := f.pairCount(t)
	status, refused, _ := post(deviceClient(pool, nil), PairPath, map[string]any{"code": f.pairCode(t), "kind": KindKey,
		"name": "example-ci", "secret": pairSecret(t), "owner_uid": 501})
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "input", refused.Reason)
	require.Equal(t, ErrKeyOwnerUID.Error(), refused.Error)
	_, after := f.pairCount(t)
	require.Equal(t, unused+1, after, "the refused pairing used no code")

	status, refused, _ = post(deviceClient(pool, nil), PairStatusPath, map[string]any{"secret": secret, "owner_uid": 501})
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "a status request carries the secret alone", refused.Error)
	for name, uid := range map[string]any{"negative": -1, "past 2^32-1": uint64(1) << 32, "text": "501"} {
		status, refused, _ = post(deviceClient(pool, id), PairPath, map[string]any{"code": f.pairCode(t), "kind": KindRelay,
			"name": "example-laptop-2", "secret": pairSecret(t), "owner_uid": uid})
		require.Equal(t, http.StatusBadRequest, status, name)
		require.Equal(t, "input", refused.Reason, name)
	}
}

// prOldPairRequest is the pairing request of the release before migration
// 050, frozen: what a server of that release decodes, strictly.
type prOldPairRequest struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// prOldServerAnswer is what that release answers a pairing that names the
// owner's uid, as measured against it (wave nr1117b, at 9c267113).
const prOldServerAnswer = `{"error":"the request must be one JSON object of code, kind, name and secret: json: unknown field \"owner_uid\"","reason":"input"}`

// A relay of this release that pairs with a server of an earlier one is told
// to upgrade the server first: that server refuses the owner's uid before it
// reads the code, and the relay never retries without it.
func TestPROwnerUID_AnOlderServerIsNamedAndNothingPairsUnbound(t *testing.T) {
	var asked atomic.Int32
	old := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		var in prOldPairRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writePairJSON(w, http.StatusBadRequest, pairError{Error: "the request must be one JSON object of code, kind, name and secret: " +
				err.Error(), Reason: "input"})
			return
		}
		t.Errorf("the older server read a pairing: %+v", in)
	}))
	t.Cleanup(old.Close)
	resp, err := old.Client().Post(old.URL+PairPath, "application/json", strings.NewReader(`{"code":"x","owner_uid":501}`))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, prOldServerAnswer, strings.TrimSpace(string(body)), "the stand-in answers as that release does")
	asked.Store(0)

	id := pairIdentity(t, "example-laptop")
	_, err = Pair(context.Background(), old.Client(), old.URL, PairRequest{Code: "ABCD-EFGH-JKMN-PQRS", Name: "example-laptop",
		Relay: id, OwnerUID: 501})
	require.ErrorIs(t, err, ErrServerTooOld)
	require.Contains(t, err.Error(), "Upgrade seamlessd on the server first")
	require.EqualValues(t, 1, asked.Load(), "one request, and no retry that would pair unbound")

	// The same answer to a pairing that named no uid is that server's own
	// input refusal, passed on, not this one.
	same := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, prOldServerAnswer+"\n")
	}))
	t.Cleanup(same.Close)
	_, _, err = postPair(context.Background(), same.Client(), same.URL+PairPath, pairRequest{Code: "ABCD-EFGH-JKMN-PQRS", Kind: KindKey,
		Name: "example-ci", Secret: pairSecret(t)})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrServerTooOld), "%v", err)
	require.ErrorContains(t, err, "the server refused the pairing (input)")
}

// A server that answers a pairing without recording the uid it was given
// (none that this repository ships: a stand-in) is never kept as a pairing.
func TestPROwnerUID_APairingTheServerDidNotBindIsRefused(t *testing.T) {
	id := pairIdentity(t, "example-laptop")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePairJSON(w, http.StatusOK, PairAnswer{Status: StatusPaired, Device: store.Device{ID: "01EXAMPLEDEVICE", Kind: KindRelay,
			Name: "example-laptop", Status: store.DeviceActive, Fingerprint: id.Fingerprint()}})
	}))
	t.Cleanup(srv.Close)
	_, err := Pair(context.Background(), srv.Client(), srv.URL, PairRequest{Code: "ABCD-EFGH-JKMN-PQRS", Name: "example-laptop",
		Relay: id, OwnerUID: 501})
	require.ErrorContains(t, err, "the server recorded owner uid 0 for this pairing (device 01EXAMPLEDEVICE), and this relay named 501")
	require.ErrorContains(t, err, "revoke that device")
}

// A relay pairs naming the owner's uid, and a key device names none: both are
// refused before anything is sent.
func TestPROwnerUID_ClientRefusesBeforeSending(t *testing.T) {
	noDial := &http.Client{Transport: clientNoDial{t}}
	id := pairIdentity(t, "example-laptop")
	_, err := Pair(context.Background(), noDial, "https://example-server:8081", PairRequest{Code: "ABCD-EFGH-JKMN-PQRS",
		Name: "example-laptop", Relay: id})
	require.ErrorIs(t, err, ErrNoOwnerUID)
	_, err = Pair(context.Background(), noDial, "https://example-server:8081", PairRequest{Code: "ABCD-EFGH-JKMN-PQRS",
		Name: "example-ci", OwnerUID: 501})
	require.ErrorIs(t, err, ErrKeyOwnerUID)
}

// The edge's refusal code is write screening's, which this package cannot
// import outside a test.
func TestPROwnerUID_RefusalCodeIsScreenings(t *testing.T) {
	require.Equal(t, screening.RefusedPeerUnknown, refusedPeerUnknown)
}
