package main

// The relay rows of the peer_auth matrix (plan networked-relay): serve's TCP
// stack under peer_auth with pairing on, a project locked, relays paired
// through the real flow -- two mapped in peer_auth.relays, one paired but not
// mapped, one mapped but paired with a code alone -- and a fake relay (a TLS
// client with the relay's certificate setting the wire's headers for one local
// caller) as the transport. A lease is keyed on (the relay, the process): it
// opens the locked project to its agent's tree through that relay and to
// nothing else -- not a sibling agent on the same computer, not the same chain
// on this machine's socket or through another relay, not a bearer request
// forging the chain. Revoking the relay ends the lease and refuses the relay.

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// relayvTCPWords is the locked refusal a TCP caller gets: the owner over TCP,
// holding no lease.
const relayvTCPWords = "the TCP connection and its bearer key never reach a locked project"

// relayvRelayWords is the locked refusal a mapped relay's caller gets from a
// relay of wire version 1, which names no program.
const relayvRelayWords = "Seamless files no access request for this agent on another computer"

func TestPeerAuthRelayMatrix(t *testing.T) {
	d := newRelayvDaemon(t)
	ctx := context.Background()
	laptop, laptopID := d.pair(t, relayvLaptop)
	_, deskID := d.pair(t, relayvDesk)
	_, studioID := d.pair(t, relayvStudio) // paired and approved, not mapped
	_, oldID := d.pairCodeOnly(t, relayvOld)
	lease := d.leaseFor(t, laptop.ID, relayvAgentA, relayvFin)
	require.True(t, strings.HasPrefix(lease.ProcessKey, access.RelayKeyPrefix(laptop.ID)), "the lease names the relay")

	approvalsNow := func() int {
		var n int
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_approvals`).Scan(&n))
		return n
	}

	t.Run("two agents on one client computer: the leased tree reads and writes, the sibling is refused", func(t *testing.T) {
		a := d.mcpVia(t, laptopID, "", relayvCallerA.relayvHeaders())
		// A confidential project reads from its own sessions: the agent binds
		// one there, which the lease allows.
		text, refused := paCall(t, a, "session_start", map[string]any{"project": relayvFin, "name": "relay-fin-a"})
		require.False(t, refused, text)
		require.Contains(t, text, "Access to "+relayvFin+" was approved", "the lease's briefing rides the first result, once")
		text, refused = relayvRead(t, a, relayvFin, d.finMem)
		require.False(t, refused, text)
		require.Contains(t, text, relayvSecret)
		out := paCallOK(t, a, "memory_write", map[string]any{"project": relayvFin, "name": "relay-wrote", "kind": "gotcha",
			"description": "written through the relay", "body": "body"})
		require.NotEmpty(t, out["id"])
		m, found, err := store.MemoryByName(ctx, d.db, relayvFin, "relay-wrote")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, relayvFin, m.Project, "the write landed in the locked project")

		script := d.mcpVia(t, laptopID, "", relayvCallerScript.relayvHeaders())
		text, refused = paCall(t, script, "session_start", map[string]any{"project": relayvFin, "name": "relay-fin-script"})
		require.False(t, refused, text)
		text, refused = relayvRead(t, script, relayvFin, d.finMem)
		require.False(t, refused, "a grandchild of the leased agent shares its lease: %s", text)
		require.Contains(t, text, relayvSecret)

		before := approvalsNow()
		b := d.mcpVia(t, laptopID, "", relayvCallerB.relayvHeaders())
		text, refused = paCall(t, b, "session_start", map[string]any{"project": relayvFin, "name": "relay-fin-b"})
		require.True(t, refused, "the sibling binds nothing in the locked project: %s", text)
		require.Contains(t, text, "is locked")
		text, refused = relayvRead(t, b, relayvFin, d.finMem)
		require.True(t, refused, "the sibling agent holds no lease")
		require.Contains(t, text, "project "+relayvFin+" is locked")
		require.Contains(t, text, relayvRelayWords)
		require.NotContains(t, text, relayvSecret)
		text, _ = paCall(t, b, "recall", map[string]any{"query": "ledger"})
		require.NotContains(t, text, relayvSecret, "recall without project= leaves the locked project out")
		require.Equal(t, before, approvalsNow(), "nothing is filed for a relay's process")

		text, refused = relayvRead(t, b, relayvOpen, d.openMem)
		require.False(t, refused, text)
		require.Contains(t, text, relayvOpenToken, "an open project reads as it does for the owner")

		// The relay's enrolled name is the request's host, whatever host it
		// claims (memory network-install-identity-is-host-scoped).
		var host string
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT host FROM sessions WHERE name = ?`, "relay-fin-a").Scan(&host))
		require.Equal(t, relayvLaptop, host)
	})

	t.Run("the same chain anywhere else holds nothing", func(t *testing.T) {
		// This machine's socket, with the very tokens of agent A's chain.
		local := d.socket(t, relayvOwnerUID, relayvBridgeA, relayvAgentA, relayvTerm)
		cli := paClientHeaders(t, local.URL, "", map[string]string{mcp.AgentProcessHeader: relayvAgentA.AgentID()})
		text, refused := relayvRead(t, cli, relayvFin, d.finMem)
		require.True(t, refused, "a process of this machine is not the relay's")
		require.NotContains(t, text, relayvSecret)

		// Another mapped relay forwarding agent A's chain.
		desk := d.mcpVia(t, deskID, "", relayvCallerA.relayvHeaders())
		text, refused = relayvRead(t, desk, relayvFin, d.finMem)
		require.True(t, refused, "another relay's process of the same pid, start and boot")
		require.Contains(t, text, relayvRelayWords)
		require.NotContains(t, text, relayvSecret)
	})

	t.Run("a forged chain over bearer holds nothing", func(t *testing.T) {
		key := d.pairKey(t, "example-ci")
		for name, auth := range map[string]string{"the shared key": relayvKey, "a key device's key": key} {
			cli := d.mcpVia(t, nil, auth, relayvCallerA.relayvHeaders())
			text, refused := relayvRead(t, cli, relayvFin, d.finMem)
			require.True(t, refused, name)
			require.Contains(t, text, relayvTCPWords, "%s: the owner over TCP, its forged relay headers stripped unread", name)
			require.NotContains(t, text, relayvSecret, name)
			text, refused = relayvRead(t, cli, relayvOpen, d.openMem)
			require.False(t, refused, "%s: %s", name, text)
		}
		// Malformed relay headers over bearer are not read at all.
		status := d.initStatus(t, d.httpClient(nil), map[string]string{"Authorization": "Bearer " + relayvKey,
			devices.RelayVersionHeader: "9", devices.RelayUIDHeader: "nope", devices.RelayChainHeader: ""})
		require.Equal(t, http.StatusOK, status)
	})

	t.Run("a relay with no mapping is refused for every uid", func(t *testing.T) {
		// Review H1 (wave nt1113e): its callers are every account on its
		// computer that reaches its socket, so none of them is the owner until
		// peer_auth.relays says which uid is.
		codes := func() int {
			var n int
			require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_pairing_codes`).Scan(&n))
			return n
		}
		before := codes()
		for _, c := range []relayvCaller{relayvCallerA, {uid: 502, chain: relayvCallerA.chain}, {uid: 0, chain: relayvCallerA.chain},
			{uid: 65534, chain: relayvCallerA.chain}} {
			for _, h := range []map[string]string{c.relayvHeaders(), c.relay2Headers(relay2Named)} {
				require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(studioID), h), "uid %d", c.uid)
			}
			status, body := d.do(t, d.httpClient(studioID), http.MethodGet, "/console/?format=json", nil, c.relay2Headers(relay2Named))
			require.Equal(t, http.StatusForbidden, status, "uid %d: %s", c.uid, body)
			require.Contains(t, body, "peer_auth.relays does not map it")
			status, _ = d.do(t, d.httpClient(studioID), http.MethodPost, "/console/devices/code", url.Values{}, c.relay2Headers(relay2Named))
			require.Equal(t, http.StatusForbidden, status, "uid %d mints no pairing code through it", c.uid)
		}
		require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(studioID), nil), "nor with no caller at all")
		require.Equal(t, http.StatusBadRequest, d.initStatus(t, d.httpClient(studioID), map[string]string{
			devices.RelayVersionHeader: "1", devices.RelayUIDHeader: "501", devices.RelayChainHeader: "nope"}),
			"headers it does send must be well formed")
		status, _ := d.do(t, d.httpClient(studioID), http.MethodGet, "/healthz", nil, nil)
		require.Equal(t, http.StatusOK, status, "the health check reads no caller")
		require.Equal(t, before, codes(), "no code was minted")
	})

	t.Run("malformed relay headers are refused 400", func(t *testing.T) {
		long := make([]string, access.MaxChainHops+1)
		for i := range long {
			long[i] = relayvTok(int32(3000 + i)).Key()
		}
		good := relayvCallerA.relayvHeaders()
		with := func(k, v string) map[string]string {
			h := map[string]string{}
			for hk, hv := range good {
				h[hk] = hv
			}
			if v == "" && k != devices.RelayChainHeader {
				delete(h, k)
			} else {
				h[k] = v
			}
			return h
		}
		for name, h := range map[string]map[string]string{
			"none at all":                 nil,
			"no version":                  with(devices.RelayVersionHeader, ""),
			"version 2":                   with(devices.RelayVersionHeader, "2"),
			"no uid":                      with(devices.RelayUIDHeader, ""),
			"a uid not decimal":           with(devices.RelayUIDHeader, "5o1"),
			"a uid past 2^32-1":           with(devices.RelayUIDHeader, "4294967296"),
			"an empty chain":              with(devices.RelayChainHeader, ""),
			"too many hops":               with(devices.RelayChainHeader, strings.Join(long, ",")),
			"an entry that is no process": with(devices.RelayChainHeader, relayvAgentA.Key()+",not-a-process"),
			"a relay-qualified entry": with(devices.RelayChainHeader, access.ProcessToken{PID: 2200, Version: 1, Boot: relayvBoot,
				Relay: laptop.ID}.Key()),
		} {
			require.Equal(t, http.StatusBadRequest, d.initStatus(t, d.httpClient(laptopID), h), name)
		}
		// One header sent twice.
		req, err := http.NewRequest(http.MethodPost, d.tcp.URL+"/api/mcp", strings.NewReader(relayvInitialize()))
		require.NoError(t, err)
		for k, v := range good {
			req.Header.Set(k, v)
		}
		req.Header.Add(devices.RelayUIDHeader, "0")
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.httpClient(laptopID).Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a header the relay sets once")
		require.Equal(t, http.StatusOK, d.initStatus(t, d.httpClient(laptopID), good), "the same relay, well formed")
		status, body := d.do(t, d.httpClient(laptopID), http.MethodGet, "/healthz", nil, nil)
		require.Equal(t, http.StatusOK, status, "the relay's own probe of the server forwards no caller, and reads none: %s", body)
	})

	t.Run("a uid the mapping does not name is refused", func(t *testing.T) {
		for _, uid := range []uint32{503, 0, 65534} {
			c := relayvCaller{uid: uid, chain: relayvCallerA.chain}
			require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(laptopID), c.relayvHeaders()), "uid %d", uid)
			require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(laptopID), c.relay2Headers(relay2Named)), "uid %d", uid)
		}
		// The laptop's restricted account is another computer's account only:
		// the desk maps none.
		c := relayvCaller{uid: relay2RestrictedUID, chain: relayvCallerA.chain}
		require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(deskID), c.relay2Headers(relay2Named)))
	})

	t.Run("a relay paired with a code alone is refused until it pairs again", func(t *testing.T) {
		status, body := d.do(t, d.httpClient(oldID), http.MethodGet, "/console/?format=json", nil, relayvCallerA.relayvHeaders())
		require.Equal(t, http.StatusForbidden, status, body)
		require.Contains(t, body, "pair it again")
		status, _ = d.do(t, d.httpClient(oldID), http.MethodGet, "/console/?format=json", nil, nil)
		require.Equal(t, http.StatusForbidden, status, "with or without what it forwards")
	})

	t.Run("an explicit access request through a relay files nothing", func(t *testing.T) {
		before := approvalsNow()
		b := d.mcpVia(t, laptopID, "", relayvCallerB.relayvHeaders())
		text, refused := paCall(t, b, "access_request", map[string]any{"project": relayvFin, "reason": "reconcile the ledger"})
		require.True(t, refused, text)
		require.Contains(t, text, "no access request is filed for this agent on another computer")
		require.Equal(t, before, approvalsNow())
	})

	t.Run("the Touch ID helper's signature is verified by the server, through a relay", func(t *testing.T) {
		// A request of this machine's agent, filed at its lock refusal.
		agent, bridge := relayvLocalTok(5200), relayvLocalTok(5210)
		local := d.socket(t, relayvOwnerUID, bridge, agent)
		cli := paClientHeaders(t, local.URL, "", map[string]string{mcp.AgentProcessHeader: agent.AgentID()})
		text, refused := relayvRead(t, cli, relayvFin, d.finMem)
		require.True(t, refused)
		short := lmShortRe.FindString(text)
		require.NotEmpty(t, short, text)
		a, err := d.svc.Resolve(ctx, short)
		require.NoError(t, err)

		// The helper on the client computer, run by launchd: no agent above it.
		helper := relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{relayvTok(2400)}}
		via := d.httpClient(laptopID)
		key := pmNewKey(t)
		pub, err := key.PublicKey.Bytes()
		require.NoError(t, err)
		code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverTouchID, store.ApproverKeyFingerprint(pub), time.Now())
		require.NoError(t, err)
		sig, err := pmSign(key, approval.TouchIDEnrollMessage(code, pub))
		require.NoError(t, err)
		status, body := d.do(t, via, http.MethodPost, "/console/approvers/touchid/enroll", url.Values{
			"name": {"the laptop"}, "code": {code},
			"public_key": {base64.StdEncoding.EncodeToString(pub)}, "signature": {base64.StdEncoding.EncodeToString(sig)},
		}, helper.relayvHeaders())
		require.Equal(t, http.StatusOK, status, body)
		var enrolled struct {
			ID string `json:"id"`
		}
		relayvJSON(t, body, &enrolled)
		require.NotEmpty(t, enrolled.ID)

		read := func(client *http.Client, headers map[string]string) pmAccessOne {
			status, body := d.do(t, client, http.MethodGet, "/console/access/"+a.ID+"?format=json", nil, headers)
			require.Equal(t, http.StatusOK, status, body)
			var one pmAccessOne
			relayvJSON(t, body, &one)
			return one
		}
		bearer := map[string]string{"Authorization": "Bearer " + relayvKey}
		require.Empty(t, read(d.httpClient(nil), bearer).Challenge, "the challenge is read on a socket only")
		one := read(via, helper.relayvHeaders())
		require.NotEmpty(t, one.Challenge, "a relay's socket is one")
		good, err := pmTouchIDSignature(key, one, approval.For1h)
		require.NoError(t, err)

		status, body = d.do(t, d.httpClient(nil), http.MethodPost, "/console/access/"+a.ID+"/approve",
			pmTouchIDForm(enrolled.ID, good, approval.For1h, one.Request.Digest), bearer)
		require.Equal(t, http.StatusForbidden, status, "a Touch ID proof never over the bearer key: %s", body)
		forged, err := pmSign(pmNewKey(t), []byte("not the decision"))
		require.NoError(t, err)
		status, body = d.do(t, via, http.MethodPost, "/console/access/"+a.ID+"/approve",
			pmTouchIDForm(enrolled.ID, forged, approval.For1h, one.Request.Digest), helper.relayvHeaders())
		require.NotEqual(t, http.StatusOK, status, "a signature that does not verify approves nothing: %s", body)
		pending, err := store.AccessApprovalByID(ctx, d.db, a.ID)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, pending.Status)

		status, body = d.do(t, via, http.MethodPost, "/console/access/"+a.ID+"/approve",
			pmTouchIDForm(enrolled.ID, good, approval.For1h, one.Request.Digest), helper.relayvHeaders())
		require.Equal(t, http.StatusOK, status, body)
		approved, err := store.AccessApprovalByID(ctx, d.db, a.ID)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalApproved, approved.Status)
		require.Equal(t, enrolled.ID, approved.DecidedBy, "the server verified the helper's signature")
		got, err := store.AccessLeaseByID(ctx, d.db, approved.LeaseID)
		require.NoError(t, err)
		require.Equal(t, agent.Key(), got.ProcessKey, "the lease is this machine's agent's")
		require.Empty(t, got.RelayID)
	})

	// Last: it revokes the laptop's relay.
	t.Run("a revoked relay is refused, and its lease ends with it", func(t *testing.T) {
		_, err := d.pairing.Revoke(ctx, laptop.ID, store.DeviceByConsole)
		require.NoError(t, err)
		ended, err := store.AccessLeaseByID(ctx, d.db, lease.ID)
		require.NoError(t, err)
		require.Equal(t, store.LeaseRevokedDevice, ended.RevokedReason, "ended in the revocation's transaction")
		require.Equal(t, http.StatusUnauthorized, d.initStatus(t, d.httpClient(laptopID), relayvCallerA.relayvHeaders()))

		// Paired again under the same name, it is another relay: the old
		// lease, and the old relay's processes, are nothing to it.
		_, again := d.pair(t, relayvLaptop)
		a := d.mcpVia(t, again, "", relayvCallerA.relayvHeaders())
		text, refused := relayvRead(t, a, relayvFin, d.finMem)
		require.True(t, refused)
		require.NotContains(t, text, relayvSecret)
	})
}
