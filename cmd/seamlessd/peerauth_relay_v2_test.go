package main

// The relay matrix's wire version 2 and restricted-account rows (wave
// nt1113e): a relay that names each process's program lets the server file
// and judge a request for an agent behind it exactly as for one on its own
// socket, and a restricted account on a client computer is judged as one on
// this socket: open projects only, every write labeled with the account and
// the host, every binding-kind write a proposal.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

func TestPeerAuthRelayMatrix_V2(t *testing.T) {
	d := newRelayvDaemon(t)
	ctx := context.Background()
	laptop, laptopID := d.pair(t, relayvLaptop)
	approvalsNow := func() int {
		var n int
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_approvals`).Scan(&n))
		return n
	}

	var asked store.AccessApproval
	t.Run("an agent behind a version 2 relay files its own request, named as the relay's kernel names it", func(t *testing.T) {
		b := d.mcpVia(t, laptopID, "", relayvCallerB.relay2Headers(relay2Named))
		text, refused := relayvRead(t, b, relayvFin, d.finMem)
		require.True(t, refused)
		require.NotContains(t, text, relayvSecret)
		short := lmShortRe.FindString(text)
		require.NotEmpty(t, short, "the refusal names the request it filed: %s", text)
		require.Contains(t, text, "access request "+short+" is waiting")

		a, err := d.svc.Resolve(ctx, short)
		require.NoError(t, err)
		agentB := relayvAgentB
		agentB.Relay = laptop.ID
		require.Equal(t, agentB.Key(), a.ProcessKey, "the anchor is the relay's agent, keyed on the relay and the process")
		require.Equal(t, relayvClaude, a.Program, "its program, as the relay's kernel named it")
		require.EqualValues(t, relayvOwnerUID, a.PeerUID)

		facts, err := d.svc.Facts(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, "Claude Code 2.1.319", facts.Agent)
		require.NotEmpty(t, facts.Warnings)
		require.Equal(t, approval.WarnRelay, facts.Warnings[0].Code, "which computer asks comes first")
		require.Contains(t, facts.Warnings[0].Text, "the agent runs on "+relayvLaptop+", another computer")
		asked = a

		// An explicit ask from the same agent finds that request.
		text, refused = paCall(t, b, "access_request", map[string]any{"project": relayvFin, "reason": "reconcile the ledger"})
		require.False(t, refused, text)
		require.Contains(t, text, approval.ShortID(a.ID))
	})

	t.Run("the owner approves it, and the lease opens the project to that tree through that relay alone", func(t *testing.T) {
		require.NotEmpty(t, asked.ID)
		facts, err := d.svc.Facts(ctx, asked.ID)
		require.NoError(t, err)
		lease, err := d.svc.Approve(ctx, approval.Decision{ID: asked.ID, Duration: approval.For1h, Digest: facts.Digest,
			Proof: approval.Proof{Kind: approval.ProofDaemon}})
		require.NoError(t, err)
		require.Equal(t, laptop.ID, lease.RelayID)

		b := d.mcpVia(t, laptopID, "", relayvCallerB.relay2Headers(relay2Named))
		// A confidential project reads from its own sessions: the agent binds
		// one there, which the lease allows.
		text, refused := paCall(t, b, "session_start", map[string]any{"project": relayvFin, "name": "relay2-fin-b"})
		require.False(t, refused, text)
		require.Contains(t, text, "Access to "+relayvFin+" was approved", "the lease's briefing rides the first result, once")
		text, refused = relayvRead(t, b, relayvFin, d.finMem)
		require.False(t, refused, text)
		require.Contains(t, text, relayvSecret)

		// The same tree's process on this machine, and a script of agent A's
		// tree, hold nothing of it.
		local := d.socket(t, relayvOwnerUID, relayvBridgeB, relayvAgentB)
		cli := paClientHeaders(t, local.URL, "", map[string]string{mcp.AgentProcessHeader: relayvAgentB.AgentID()})
		text, refused = relayvRead(t, cli, relayvFin, d.finMem)
		require.True(t, refused)
		require.NotContains(t, text, relayvSecret)
	})

	t.Run("a broad anchor behind a relay is never filed, and says why", func(t *testing.T) {
		before := approvalsNow()
		shell := relayvTok(2600)
		caller := relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{relayvTok(2610), shell, relayvTerm}}
		h := caller.relay2Headers(map[int32]string{2610: relayvSeam, 2600: "/bin/zsh", relayvTerm.PID: relay2Named[relayvTerm.PID]})
		h[mcp.AgentProcessHeader] = shell.AgentID()
		cli := d.mcpVia(t, laptopID, "", h)
		text, refused := paCall(t, cli, "access_request", map[string]any{"project": relayvFin, "reason": "from a shell"})
		require.True(t, refused, text)
		require.Contains(t, text, "zsh, a shell")
		text, refused = relayvRead(t, cli, relayvFin, d.finMem)
		require.True(t, refused)
		require.Contains(t, text, "zsh, a shell", "the lock's refusal says why nothing was filed")
		require.Equal(t, before, approvalsNow())
	})

	t.Run("an anchor the relay's kernel did not name is not filed", func(t *testing.T) {
		before := approvalsNow()
		unnamed := map[int32]string{relayvBridgeA.PID: relayvSeam, relayvTerm.PID: relay2Named[relayvTerm.PID]}
		cli := d.mcpVia(t, laptopID, "", relayvCallerA.relay2Headers(unnamed))
		text, refused := paCall(t, cli, "access_request", map[string]any{"project": relayvFin, "reason": "unnamed"})
		require.True(t, refused, text)
		require.Contains(t, text, "its relay did not say which program is asking")
		require.Equal(t, before, approvalsNow())
	})

	t.Run("a version 1 relay's agent still files nothing", func(t *testing.T) {
		before := approvalsNow()
		cli := d.mcpVia(t, laptopID, "", relayvCallerA.relayvHeaders())
		text, refused := paCall(t, cli, "access_request", map[string]any{"project": relayvFin, "reason": "old relay"})
		require.True(t, refused, text)
		require.Contains(t, text, "a relay of wire version 1 never does")
		require.Equal(t, before, approvalsNow())
	})

	t.Run("a malformed version 2 report is refused 400", func(t *testing.T) {
		good := relayvCallerA.relay2Headers(relay2Named)
		with := func(k, v string) map[string]string {
			h := map[string]string{}
			for hk, hv := range good {
				h[hk] = hv
			}
			h[k] = v
			return h
		}
		programs := good[devices.RelayProgramsHeader]
		for name, h := range map[string]map[string]string{
			"one program too few":   with(devices.RelayProgramsHeader, programs[:strings.LastIndex(programs, ",")]),
			"one program too many":  with(devices.RelayProgramsHeader, programs+",%2Fbin%2Fsh"),
			"lower-case hex":        with(devices.RelayProgramsHeader, "%2fbin,"+programs[strings.Index(programs, ",")+1:]),
			"a byte left plain":     with(devices.RelayProgramsHeader, "/bin/z sh"+programs[strings.Index(programs, ","):]),
			"a plain byte encoded":  with(devices.RelayProgramsHeader, "%41"+programs[strings.Index(programs, ","):]),
			"a NUL":                 with(devices.RelayProgramsHeader, "a%00b"+programs[strings.Index(programs, ","):]),
			"an over-long program":  with(devices.RelayProgramsHeader, strings.Repeat("a", devices.MaxRelayProgramBytes+1)+programs[strings.Index(programs, ","):]),
			"no programs":           with(devices.RelayProgramsHeader, ""),
			"programs in version 1": with(devices.RelayVersionHeader, devices.RelayWireVersion),
			"version 3":             with(devices.RelayVersionHeader, "3"),
		} {
			if name == "no programs" {
				delete(h, devices.RelayProgramsHeader)
			}
			require.Equal(t, http.StatusBadRequest, d.initStatus(t, d.httpClient(laptopID), h), name)
		}
		require.Equal(t, http.StatusOK, d.initStatus(t, d.httpClient(laptopID), good), "the same relay, well formed")
	})

	t.Run("a restricted account on the client computer reaches the open projects only", func(t *testing.T) {
		agent := relayvCaller{uid: relay2RestrictedUID, chain: []access.ProcessToken{relayvTok(2710), relayvTok(2700)}}
		for _, h := range []map[string]string{agent.relay2Headers(map[int32]string{2700: relayvClaude}), agent.relayvHeaders()} {
			cli := d.mcpVia(t, laptopID, "", h)
			text, refused := relayvRead(t, cli, relayvOpen, d.openMem)
			require.False(t, refused, text)
			require.Contains(t, text, relayvOpenToken)
			text, refused = relayvRead(t, cli, relayvFin, d.finMem)
			require.True(t, refused)
			require.NotContains(t, text, relayvSecret)
			require.Contains(t, text, "not reachable from account "+relay2RestrictedLabel)
			text, _ = paCall(t, cli, "project_list", map[string]any{})
			require.NotContains(t, text, relayvFin, "a project it cannot reach is not listed for it")
			text, refused = paCall(t, cli, "access_request", map[string]any{"project": relayvFin, "reason": "let me in"})
			require.True(t, refused, text)
			require.Contains(t, text, "files no access requests")
		}
	})

	t.Run("its writes are labeled with the account and the host, and its rules are proposals", func(t *testing.T) {
		agent := relayvCaller{uid: relay2RestrictedUID, chain: []access.ProcessToken{relayvTok(2710), relayvTok(2700)}}
		cli := d.mcpVia(t, laptopID, "", agent.relay2Headers(map[int32]string{2700: relayvClaude}))
		text, refused := paCall(t, cli, "session_start", map[string]any{"project": relayvOpen, "name": "relay2-agent"})
		require.False(t, refused, text)
		var sessAccount, sessHost string
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT account, host FROM sessions WHERE name = ?`,
			relay2RestrictedLabel+":relay2-agent").Scan(&sessAccount, &sessHost))
		require.Equal(t, relay2RestrictedLabel, sessAccount, "its session is in its own namespace, labeled")
		require.Equal(t, relayvLaptop, sessHost)

		out := paCallOK(t, cli, "memory_write", map[string]any{"project": relayvOpen, "name": "relay2-note", "kind": "gotcha",
			"description": "written by a restricted account on a client computer", "body": "body"})
		require.NotEmpty(t, out["id"])
		m, found, err := store.MemoryByName(ctx, d.db, relayvOpen, "relay2-note")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, relay2RestrictedLabel, m.Account, "the label: the account and the host")

		text, refused = paCall(t, cli, "memory_write", map[string]any{"project": relayvOpen, "name": "relay2-rule", "kind": "constraint",
			"description": "a rule from a client computer's restricted account", "body": "always do x"})
		require.False(t, refused, text)
		_, found, err = store.MemoryByName(ctx, d.db, relayvOpen, "relay2-rule")
		require.NoError(t, err)
		require.False(t, found, "a binding rule never lands")
		var reasons, detail string
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT reasons, detail FROM held_writes WHERE name = ? AND status = 'held'`,
			"relay2-rule").Scan(&reasons, &detail))
		require.Contains(t, reasons, "account.restricted", "it waits for the owner's promotion")
		var d2 map[string]any
		require.NoError(t, json.Unmarshal([]byte(detail), &d2))
		require.Equal(t, relay2RestrictedLabel, d2["account"])

		_, refused = paCall(t, cli, "memory_write", map[string]any{"project": relayvFin, "name": "relay2-sneak", "kind": "gotcha",
			"description": "x", "body": "x"})
		require.True(t, refused, "never into a locked project")
	})
}

// A restricted account's reach is loaded per request through a relay as on
// this socket; a lookup that fails refuses the request (503) rather than
// serving it a reach it would misread as none.
func TestRelayHandler_V2RestrictedReachFailsClosed(t *testing.T) {
	d := newRelayvDaemon(t)
	_, laptopID := d.pair(t, relayvLaptop)
	var seen []access.View
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := access.FromContext(r.Context())
		seen = append(seen, v)
		w.WriteHeader(http.StatusOK)
	})
	serve := func(open func(context.Context) ([]string, error)) *httptest.Server {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		h := ownerHandler(d.policy, d.pairing.Edge(relayHandler(d.policy, peerLeases{open: open}, logger, probe)))
		cert, pool := deviceServerCert(t)
		ts := httptest.NewUnstartedServer(h)
		ts.TLS = devices.WithRelayClientAuth(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
		ts.StartTLS()
		t.Cleanup(ts.Close)
		d.pool = pool // d.httpClient trusts this server from here on
		return ts
	}
	agent := relayvCaller{uid: relay2RestrictedUID, chain: []access.ProcessToken{relayvTok(2710)}}
	get := func(ts *httptest.Server) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/console/?format=json", nil)
		require.NoError(t, err)
		for k, v := range agent.relay2Headers(nil) {
			req.Header.Set(k, v)
		}
		resp, err := d.httpClient(laptopID).Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode
	}
	failing := serve(func(context.Context) ([]string, error) { return nil, errors.New("example: the database is locked") })
	require.Equal(t, http.StatusServiceUnavailable, get(failing))
	require.Empty(t, seen, "nothing reached a route")

	working := serve(func(context.Context) ([]string, error) { return []string{relayvOpen, relayvFin}, nil })
	require.Equal(t, http.StatusOK, get(working))
	require.Len(t, seen, 1)
	reach, ok := seen[0].Allowed()
	require.True(t, ok)
	require.Equal(t, []string{relayvOpen}, reach, "the locked project is never in its reach")
	require.Equal(t, relay2RestrictedLabel, seen[0].Account())
	require.True(t, access.FromAgent(access.WithView(context.Background(), seen[0])), "an account's request is never the owner's hands")
}
