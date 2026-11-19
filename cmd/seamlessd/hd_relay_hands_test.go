//go:build darwin || linux

package main

// The relay matrix's owner-hands rows (owner decision R-D3): serve's TCP
// stack under peer_auth, relays paired through the real flow, and real
// requests through a fake relay. A request is the owner's own hands on the
// relay's computer only when the relay is mapped and passkey-approved, its
// pairing recorded the mapping's owner uid, it forwards that uid, and it
// named (wire version 2) every process of a whole chain with no agent in it.
// Then it makes the owner's decisions -- a pairing code, a deny, a revoke, a
// device's revocation -- and each records an event naming the relay and the
// chain. Every other caller is refused, and changes nothing: the refusals
// come first.

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// The owner's terminal on the laptop: seam under zsh under login under the
// terminal (relayvTerm, iTerm2), named as the relay's kernel names them.
var (
	hdrLogin = relayvTok(2500)
	hdrShell = relayvTok(2510)
	hdrSeam  = relayvTok(2520)

	hdrNames = map[int32]string{
		relayvTerm.PID: relay2Named[relayvTerm.PID],
		hdrLogin.PID:   "/usr/bin/login",
		hdrShell.PID:   "/bin/zsh",
		hdrSeam.PID:    relayvSeam,
	}
	hdrTerminal = relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{hdrSeam, hdrShell, hdrLogin, relayvTerm}}
)

// hdrState is what the owner's decisions would change: the open pairing
// codes, a request's status, a lease's end, a device's status, and the
// relay decisions recorded.
type hdrState struct {
	codes     int
	request   string
	lease     string
	device    string
	decisions int
}

// hdrFixture is one round's targets: a pending request, a live lease and a
// device to revoke.
type hdrFixture struct {
	request, lease, device string
}

func (d *relayvDaemon) hdrState(t *testing.T, f hdrFixture) hdrState {
	t.Helper()
	ctx := context.Background()
	codes, err := store.CountOpenPairingCodes(ctx, d.db, time.Now())
	require.NoError(t, err)
	a, err := store.AccessApprovalByID(ctx, d.db, f.request)
	require.NoError(t, err)
	l, err := store.AccessLeaseByID(ctx, d.db, f.lease)
	require.NoError(t, err)
	dev, err := store.DeviceByID(ctx, d.db, f.device)
	require.NoError(t, err)
	evs, err := events.NewRecorder(d.db).ByKinds(ctx, []core.EventKind{devices.EventRelayDecision}, "", "", 100)
	require.NoError(t, err)
	return hdrState{codes: codes, request: a.Status, lease: l.RevokedReason, device: dev.Status, decisions: len(evs)}
}

// hdrPending files a pending request of this machine's agent agent, as its
// refusal at the lock files one.
func (d *relayvDaemon) hdrPending(t *testing.T, agent access.ProcessToken) string {
	t.Helper()
	a, err := store.CreateAccessApproval(context.Background(), d.db, store.AccessRequest{Project: relayvFin,
		Kind: store.AccessKindRequest, Lease: time.Hour, TTL: 10 * time.Minute, PeerUID: relayvOwnerUID, PeerPID: agent.PID,
		ProcessKey: agent.Key(), Program: relayvClaude}, time.Now())
	require.NoError(t, err)
	return a.ID
}

// hdrDecisions are the owner's decisions one caller tries against f, each a
// POST: its name and target.
func hdrDecisions(f hdrFixture) [][2]string {
	return [][2]string{
		{"a pairing code", "/console/devices/code"},
		{"a deny", "/console/access/" + f.request + "/deny"},
		{"a lease's revoke", "/console/access/leases/" + f.lease + "/revoke"},
		{"a device's revocation", "/console/devices/" + f.device + "/revoke"},
	}
}

func TestHDRelayHands_Matrix(t *testing.T) {
	d := newRelayvDaemon(t)
	ctx := context.Background()
	laptop, laptopID := d.pair(t, relayvLaptop)
	desk, deskID := d.pair(t, relayvDesk)
	_, studioID := d.pair(t, relayvStudio) // paired and approved, not mapped
	_, oldID := d.pairCodeOnly(t, relayvOld)
	deviceKey := d.pairKey(t, "example-ci")
	via := d.httpClient(laptopID)

	// The targets: a request of this machine's agent, its lease, and a
	// device to revoke (one more paired relay, mapped nowhere).
	victim, _ := d.pair(t, "example-spare")
	f := hdrFixture{request: d.hdrPending(t, relayvLocalTok(5200)), lease: d.leaseFor(t, "", relayvLocalTok(5300), relayvFin).ID,
		device: victim.ID}
	before := d.hdrState(t, f)
	require.Equal(t, store.ApprovalPending, before.request)
	require.Empty(t, before.lease)
	require.Equal(t, store.DeviceActive, before.device)

	refused := func(t *testing.T, client *http.Client, headers map[string]string, want string) {
		t.Helper()
		for _, dec := range hdrDecisions(f) {
			status, body := d.do(t, client, http.MethodPost, dec[1], url.Values{}, headers)
			require.Equal(t, http.StatusForbidden, status, "%s: %s", dec[0], body)
			require.Contains(t, body, want, dec[0])
		}
		require.Equal(t, before, d.hdrState(t, f), "a refused caller changed nothing and recorded no decision")
	}
	names := func(drop int32) map[int32]string {
		out := map[int32]string{}
		for pid, n := range hdrNames {
			if pid != drop {
				out[pid] = n
			}
		}
		return out
	}

	// The refusals, first.
	t.Run("a relay of wire version 1 is refused: it names no program", func(t *testing.T) {
		refused(t, via, hdrTerminal.relayvHeaders(), "a relay of wire version 1")
	})
	t.Run("an agent's chain is refused, naming the agent", func(t *testing.T) {
		refused(t, via, relayvCallerA.relay2Headers(relay2Named),
			"Claude Code 2.1.319 (pid 2200 on example-laptop) is in its process chain: an agent started it")
		refused(t, via, relayvCallerScript.relay2Headers(relay2Named), "is in its process chain: an agent started it")
	})
	t.Run("a chain with a process the relay did not name is refused", func(t *testing.T) {
		refused(t, via, hdrTerminal.relay2Headers(names(hdrLogin.PID)), "no program for pid 2500")
	})
	t.Run("a chain as long as the bound may be cut short, and is refused", func(t *testing.T) {
		long := relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{hdrSeam}}
		named := map[int32]string{hdrSeam.PID: relayvSeam, relayvTerm.PID: hdrNames[relayvTerm.PID]}
		for i := range access.MaxChainHops - 2 {
			shell := relayvTok(int32(2600 + i))
			long.chain = append(long.chain, shell)
			named[shell.PID] = "/bin/zsh"
		}
		long.chain = append(long.chain, relayvTerm)
		require.Len(t, long.chain, access.MaxChainHops)
		refused(t, via, long.relay2Headers(named), "may have been cut short")
	})
	t.Run("a chain that ends short of the session's root is refused", func(t *testing.T) {
		short := relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{hdrSeam, hdrShell}}
		refused(t, via, short.relay2Headers(hdrNames), "ends at zsh (pid 2510 on example-laptop)")
		alone := relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{hdrSeam}}
		refused(t, via, alone.relay2Headers(hdrNames), "ends at seam")
	})
	t.Run("a restricted account there is refused", func(t *testing.T) {
		agent := relayvCaller{uid: relay2RestrictedUID, chain: hdrTerminal.chain}
		refused(t, via, agent.relay2Headers(hdrNames), "the console is the owner's surface")
	})
	t.Run("a key device is refused", func(t *testing.T) {
		refused(t, d.httpClient(nil), map[string]string{"Authorization": "Bearer " + deviceKey}, "never the owner's hands")
	})
	t.Run("a relay paired with a code alone, and one not mapped, are refused before any route", func(t *testing.T) {
		refused(t, d.httpClient(oldID), hdrTerminal.relay2Headers(hdrNames), "paired with a one-time code alone")
		refused(t, d.httpClient(studioID), hdrTerminal.relay2Headers(hdrNames), "peer_auth.relays does not map it")
	})
	t.Run("a pairing whose recorded uid is not the mapping's, or none, is refused", func(t *testing.T) {
		desking := d.httpClient(deskID)
		setOwner := func(uid uint32) {
			_, err := d.db.ExecContext(ctx, `UPDATE devices SET owner_uid = ? WHERE id = ?`, uid, desk.ID)
			require.NoError(t, err)
		}
		setOwner(777)
		refused(t, desking, hdrTerminal.relay2Headers(hdrNames),
			"relay example-desk's pairing recorded uid 777 there as the owner's, and peer_auth.relays maps owner_uid 501")
		setOwner(0)
		refused(t, desking, hdrTerminal.relay2Headers(hdrNames), "recorded no owner's uid there")
		setOwner(relayvOwnerUID)
		status, body := d.do(t, desking, http.MethodGet, "/console/?format=json", nil, hdrTerminal.relay2Headers(hdrNames))
		require.Equal(t, http.StatusOK, status, "its reads stay open: %s", body)
	})
	t.Run("this machine's socket keeps its refusal of a deny and a revoke", func(t *testing.T) {
		local := d.socket(t, relayvOwnerUID, relayvLocalTok(5410), relayvLocalTok(5400))
		for target, want := range map[string]string{
			"/console/access/" + f.request + "/deny":        "deny from the console",
			"/console/access/leases/" + f.lease + "/revoke": "revoke from the console",
		} {
			req, err := http.NewRequest(http.MethodPost, local.URL+target, strings.NewReader(""))
			require.NoError(t, err)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			b := new(strings.Builder)
			_, err = io.Copy(b, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusForbidden, resp.StatusCode, b.String())
			require.Contains(t, b.String(), want)
		}
		require.Equal(t, before, d.hdrState(t, f))
	})

	// The owner's own hands.
	t.Run("the owner's terminal through a version 2 relay makes the owner's decisions", func(t *testing.T) {
		headers := hdrTerminal.relay2Headers(hdrNames)
		for _, dec := range hdrDecisions(f) {
			status, body := d.do(t, via, http.MethodPost, dec[1], url.Values{}, headers)
			require.Equal(t, http.StatusOK, status, "%s: %s", dec[0], body)
		}
		after := d.hdrState(t, f)
		require.Equal(t, before.codes+1, after.codes, "a code was minted")
		require.Equal(t, store.ApprovalDenied, after.request, "the request was denied")
		require.Equal(t, store.LeaseRevokedOwner, after.lease, "the lease was revoked")
		require.Equal(t, store.DeviceRevoked, after.device, "the device was revoked")

		evs, err := events.NewRecorder(d.db).ByKinds(ctx, []core.EventKind{devices.EventRelayDecision}, "", "", 100)
		require.NoError(t, err)
		require.Len(t, evs, len(hdrDecisions(f)), "one event per decision")
		routes := map[string]bool{}
		for _, e := range evs {
			require.Equal(t, laptop.ID, e.ItemID, "the relay's device")
			require.Equal(t, relayvLaptop, e.Payload["name"])
			require.EqualValues(t, relayvOwnerUID, e.Payload["uid"])
			require.Equal(t, []any{"seam", "zsh", "login", "iTerm2"}, e.Payload["programs"], "the chain, by base name")
			routes[e.Payload["route"].(string)] = true
		}
		require.Equal(t, map[string]bool{"POST /console/devices/code": true, "POST /console/access/{id}/deny": true,
			"POST /console/access/leases/{id}/revoke": true, "POST /console/devices/{id}/revoke": true}, routes)
	})

	t.Run("its hands are no proof: an approval still needs the owner's own", func(t *testing.T) {
		agent := relayvLocalTok(5500)
		id := d.hdrPending(t, agent)
		headers := hdrTerminal.relay2Headers(hdrNames)
		status, body := d.do(t, via, http.MethodGet, "/console/access/"+id+"?format=json", nil, headers)
		require.Equal(t, http.StatusOK, status, body)
		var one pmAccessOne
		relayvJSON(t, body, &one)
		status, body = d.do(t, via, http.MethodPost, "/console/access/"+id+"/approve",
			url.Values{"duration": {approval.For1h}, "digest": {one.Request.Digest}}, headers)
		require.NotEqual(t, http.StatusOK, status, body)
		require.NotContains(t, body, "never the owner's hands")
		require.NotContains(t, body, "does not take for the owner's own hands")
		a, err := store.AccessApprovalByID(ctx, d.db, id)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, a.Status, "nothing approved without a proof")

		// With the owner's Touch ID key, enrolled through the relay, the
		// owner's terminal approves, and the decision is recorded as one
		// made through the relay.
		key := pmNewKey(t)
		pub, err := key.PublicKey.Bytes()
		require.NoError(t, err)
		code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverTouchID, store.ApproverKeyFingerprint(pub), time.Now())
		require.NoError(t, err)
		sig, err := pmSign(key, approval.TouchIDEnrollMessage(code, pub))
		require.NoError(t, err)
		status, body = d.do(t, via, http.MethodPost, "/console/approvers/touchid/enroll", url.Values{
			"name": {"the laptop"}, "code": {code},
			"public_key": {base64.StdEncoding.EncodeToString(pub)}, "signature": {base64.StdEncoding.EncodeToString(sig)},
		}, headers)
		require.Equal(t, http.StatusOK, status, body)
		var enrolled struct {
			ID string `json:"id"`
		}
		relayvJSON(t, body, &enrolled)
		status, body = d.do(t, via, http.MethodGet, "/console/access/"+id+"?format=json", nil, headers)
		require.Equal(t, http.StatusOK, status, body)
		relayvJSON(t, body, &one)
		good, err := pmTouchIDSignature(key, one, approval.For1h)
		require.NoError(t, err)
		decisions := d.hdrState(t, f).decisions
		status, body = d.do(t, via, http.MethodPost, "/console/access/"+id+"/approve",
			pmTouchIDForm(enrolled.ID, good, approval.For1h, one.Request.Digest), headers)
		require.Equal(t, http.StatusOK, status, body)
		a, err = store.AccessApprovalByID(ctx, d.db, id)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalApproved, a.Status)
		require.Equal(t, enrolled.ID, a.DecidedBy, "the proof decided")
		require.Equal(t, decisions+1, d.hdrState(t, f).decisions, "and the relay's chain is on record")
	})
}
