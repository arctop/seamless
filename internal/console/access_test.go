package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const accessBoot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"

var (
	accBridge = access.ProcessToken{PID: 300, Version: 3000, Boot: accessBoot}
	accAgent  = access.ProcessToken{PID: 200, Version: 2000, Boot: accessBoot}
)

type accessFixture struct {
	db     *sql.DB
	mux    *http.ServeMux
	svc    *approval.Service
	policy *access.Policy
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	return newAccessFixtureWith(t, false)
}

// newAccessFixtureWith is newAccessFixture, with read grants on
// (isolation.requests) when grants is set.
func newAccessFixtureWith(t *testing.T, grants bool) *accessFixture {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.EnsureProject(ctx, db, "example-finance", "example-finance")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	p, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{"example-finance"}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}, Events: events.NewRecorder(db)})
	cfg := Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "advanced", Approvals: svc,
		Terminal: &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"},
	}
	if grants {
		cfg.Grants = approval.NewGrants(approval.GrantsConfig{Approvals: svc})
	}
	cs, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	return &accessFixture{db: db, mux: mux, svc: svc, policy: p}
}

// asAgent marks a request as the owner's agent on the socket, with its kernel
// chain, as peerHandler would.
func (f *accessFixture) asAgent(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	v, ok := f.policy.PeerChainView(501, []access.ProcessToken{accBridge, accAgent})
	require.True(t, ok)
	req.Header.Set("X-Seamless-Agent-Process", accAgent.AgentID())
	return req.WithContext(access.WithView(req.Context(), v))
}

func (f *accessFixture) jsonDo(t *testing.T, req *http.Request) (int, map[string]json.RawMessage) {
	t.Helper()
	req.Header.Set("Accept", "application/json")
	rr := do(f.mux, req)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return rr.Code, out
}

func (f *accessFixture) file(t *testing.T) (id, short string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/console/access/request",
		strings.NewReader(url.Values{"project": {"example-finance"}, "reason": {"reconcile the ledger notes"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, out := f.jsonDo(t, f.asAgent(t, req))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	require.NoError(t, json.Unmarshal(out["id"], &id))
	require.NoError(t, json.Unmarshal(out["short_id"], &short))
	// The passkey page only once a passkey is enrolled: a page no passkey
	// can sign is no way in.
	var page string
	require.NoError(t, json.Unmarshal(out["page"], &page))
	keys, err := store.ApproverCredentials(context.Background(), f.db, store.ApproverPasskey)
	require.NoError(t, err)
	if len(keys) == 0 {
		require.Empty(t, page)
	} else {
		require.Equal(t, approverOrigin+"/console/access/"+id, page)
	}
	return id, short
}

// The page the request answer names is the passkey page only while a passkey
// is enrolled.
func TestAccess_RequestNamesThePasskeyPageOnlyWithAPasskey(t *testing.T) {
	f := newAccessFixture(t)
	id, _ := f.file(t)
	ctx := context.Background()
	require.Empty(t, f.svc.Ways(ctx, id).Page, "passkeys set up, none enrolled")
	code, _, err := store.MintEnrollCode(ctx, f.db, store.ApproverPasskey, time.Now())
	require.NoError(t, err)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	begun, err := f.svc.BeginPasskey(ctx, "key", code)
	require.NoError(t, err)
	created, err := key.Create(begun.Options, approverOrigin)
	require.NoError(t, err)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)
	require.Equal(t, approverOrigin+"/console/access/"+id, f.svc.Ways(ctx, id).Page)
}

func TestAccess_OffIsNotFound(t *testing.T) {
	_, mux := newConsole(t)
	for _, path := range []string{"/console/access", "/console/access/R-AAAA"} {
		rr := do(mux, authedReq(httptest.NewRequest(http.MethodGet, path, nil)))
		require.Equal(t, http.StatusNotFound, rr.Code, path)
	}
}

func TestAccess_RequestListAndFacts(t *testing.T) {
	f := newAccessFixture(t)
	id, short := f.file(t)
	again, _ := f.file(t)
	require.Equal(t, id, again, "a repeated request finds the pending one")

	// The TCP key has no process: it cannot ask.
	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/access/request", strings.NewReader("project=example-finance")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, out := f.jsonDo(t, req)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(out["error"]), "no verified process")

	status, out = f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, "/console/access", nil)))
	require.Equal(t, http.StatusOK, status)
	var list struct {
		Requests []approval.Facts  `json:"requests"`
		Terminal *TerminalApprover `json:"terminal"`
		Dur      []string          `json:"durations"`
	}
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Requests, 1)
	require.Equal(t, short, list.Requests[0].Short)
	require.Equal(t, "reconcile the ledger notes", list.Requests[0].Reason)
	require.Equal(t, "_seamless", list.Terminal.Account)
	require.Equal(t, approval.Durations, list.Dur)

	status, out = f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+short, nil)))
	require.Equal(t, http.StatusOK, status, "a short id resolves")
	require.Empty(t, out["passkey_options"], "no passkey is enrolled: nothing to assert with")
	require.Contains(t, string(out["status"]), "pending")

	rr := do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id, nil)))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "reconcile the ledger notes")
	require.Contains(t, rr.Body.String(), "The agent says", "the reason is labeled as the agent's")
	require.Contains(t, rr.Body.String(), "seam approve "+short)
	require.Contains(t, rr.Body.String(), "No passkey is enrolled here")
	rr = do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access", nil)))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), short)

	rr = do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access/R-ZZZZ", nil)))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// enrollPasskey enrolls a virtual authenticator with a one-time code.
func (f *accessFixture) enrollPasskey(t *testing.T) *passkeytest.Authenticator {
	t.Helper()
	ctx := context.Background()
	code, _, err := store.MintEnrollCode(ctx, f.db, store.ApproverPasskey, time.Now())
	require.NoError(t, err)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	begun, err := f.svc.BeginPasskey(ctx, "key", code)
	require.NoError(t, err)
	created, err := key.Create(begun.Options, approverOrigin)
	require.NoError(t, err)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)
	return key
}

// post sends an approve or deny form, as the owner (the TCP key) or as the
// agent on the socket.
func (f *accessFixture) post(t *testing.T, path string, form url.Values, agent bool) (int, map[string]json.RawMessage) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if agent {
		req = f.asAgent(t, req)
	} else {
		req = authedReq(req)
	}
	return f.jsonDo(t, req)
}

// page reads a request's page as JSON (path may carry ?for=), with its facts.
func (f *accessFixture) page(t *testing.T, path string) (map[string]json.RawMessage, approval.Facts) {
	t.Helper()
	status, one := f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, path, nil)))
	require.Equal(t, http.StatusOK, status)
	var facts approval.Facts
	require.NoError(t, json.Unmarshal(one["request"], &facts))
	return one, facts
}

func TestAccess_PasskeyApproveAndDeny(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	key := f.enrollPasskey(t)

	id, _ := f.file(t)
	one, _ := f.page(t, "/console/access/"+id)
	require.Empty(t, one["passkey_options"], "no duration chosen: no options to sign with")
	require.Empty(t, one["for"])
	status, one := f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?for=1h", nil)))
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `"1h"`, string(one["for"]))
	var facts approval.Facts
	require.NoError(t, json.Unmarshal(one["request"], &facts))
	assertion, err := key.Get(one["passkey_options"], approverOrigin)
	require.NoError(t, err)

	post := func(path string, form url.Values, agent bool) (int, map[string]json.RawMessage) {
		return f.post(t, path, form, agent)
	}
	status, out := post("/console/access/"+id+"/approve", url.Values{"duration": {"1h"}, "digest": {"stale"}, "response": {string(assertion)}}, false)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, string(out["error"]), "facts changed")

	status, out = post("/console/access/"+id+"/approve", url.Values{"duration": {"1h"}, "digest": {facts.Digest}, "response": {string(assertion)}}, false)
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	leases, err := store.ActiveAccessLeasesFor(ctx, f.db, []string{accAgent.Key()}, time.Now())
	require.NoError(t, err)
	require.Len(t, leases, 1, "the agent's tree now holds the lease")

	status, _ = post("/console/access/"+id+"/approve", url.Values{"duration": {"1h"}, "digest": {facts.Digest}, "response": {string(assertion)}}, false)
	require.Equal(t, http.StatusConflict, status, "single use")

	// Deny: an agent on the socket may not; the owner may.
	_, err = store.RevokeAccessLeasesForSession(ctx, f.db, "", store.LeaseRevokedOwner, time.Now())
	require.Error(t, err)
	_, err = f.db.Exec(`UPDATE access_leases SET revoked_at = ?, revoked_reason = 'revoked'`, core.FormatTime(time.Now()))
	require.NoError(t, err)
	id2, _ := f.file(t)
	require.NotEqual(t, id, id2)
	status, _ = post("/console/access/"+id2+"/deny", url.Values{}, true)
	require.Equal(t, http.StatusForbidden, status, "an agent cannot deny")
	status, out = post("/console/access/"+id2+"/deny", url.Values{}, false)
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	a, err := store.AccessApprovalByID(ctx, f.db, id2)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalDenied, a.Status)
}

// asLeasedAgent is asAgent with the live leases the store holds for the
// agent's chain, as peerHandler's leasedView attaches them.
func (f *accessFixture) asLeasedAgent(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	chain := []access.ProcessToken{accBridge, accAgent}
	v, ok := f.policy.PeerChainView(501, chain)
	require.True(t, ok)
	now := time.Now()
	rows, err := store.ActiveAccessLeasesFor(context.Background(), f.db, []string{accBridge.Key(), accAgent.Key()}, now)
	require.NoError(t, err)
	req.Header.Set("X-Seamless-Agent-Process", accAgent.AgentID())
	return req.WithContext(access.WithView(req.Context(), v.WithLeases(now, store.AccessLeases(rows))))
}

// `seam access request` from a tree that holds a live lease, more than the
// expiry warning from its end: the lease answers and nothing is filed. With
// read grants on, a leased locked project is still the lock's: no grant is
// asked for it.
func TestAccess_RequestFromALeasedTree(t *testing.T) {
	for _, grants := range []bool{false, true} {
		t.Run(fmt.Sprintf("read grants %v", grants), func(t *testing.T) {
			f := newAccessFixtureWith(t, grants)
			ctx := context.Background()
			id, short := f.file(t)
			facts, err := f.svc.Facts(ctx, id)
			require.NoError(t, err)
			lease, err := f.svc.Approve(ctx, approval.Decision{ID: id, Duration: approval.For4h, Digest: facts.Digest,
				Proof: approval.Proof{Kind: approval.ProofDaemon}})
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/console/access/request",
				strings.NewReader(url.Values{"project": {"example-finance"}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			status, out := f.jsonDo(t, f.asLeasedAgent(t, req))
			require.Equal(t, http.StatusBadRequest, status)
			var msg string
			require.NoError(t, json.Unmarshal(out["error"], &msg))
			require.Equal(t, "project example-finance is already open to this agent until "+access.Clock(lease.ExpiresAt)+
				" (approved as "+short+"), so nothing was filed. Nothing extends it silently: from "+
				access.Clock(lease.ExpiresAt.Add(-30*time.Minute))+", 30 minutes before it ends, asking again files a new approval", msg)
			var n int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM access_approvals`).Scan(&n))
			require.Equal(t, 1, n, "nothing filed: no refresh, no read grant request")
		})
	}
}

// The passkey page binds the duration (decision D1): ?for= fixes it, the page
// shows it read-only with a way back to the choice, and the options it issues
// sign it with the facts shown. A virtual authenticator approves for every
// duration; the same assertion sent with another duration, or after the facts
// changed, approves nothing.
func TestAccess_PasskeyPageBindsTheDuration(t *testing.T) {
	lengths := map[string]time.Duration{approval.ForSession: 8 * time.Hour, approval.For1h: time.Hour, approval.For4h: 4 * time.Hour}
	for _, duration := range approval.Durations {
		t.Run(duration, func(t *testing.T) {
			f := newAccessFixture(t)
			ctx := context.Background()
			key := f.enrollPasskey(t)
			id, _ := f.file(t)
			one, facts := f.page(t, "/console/access/"+id+"?for="+duration)
			assertion, err := key.Get(one["passkey_options"], approverOrigin)
			require.NoError(t, err)

			for _, tampered := range approval.Durations {
				if tampered == duration {
					continue
				}
				status, out := f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {tampered},
					"digest": {facts.Digest}, "response": {string(assertion)}}, false)
				require.Equal(t, http.StatusConflict, status, "signed for %s, sent as %s", duration, tampered)
				var msg string
				require.NoError(t, json.Unmarshal(out["error"], &msg))
				require.Equal(t, "the passkey approved this request for another duration, or other facts, than this "+
					"approval names: nothing was approved; reload the page, check the duration and approve again", msg)
			}
			a, err := store.AccessApprovalByID(ctx, f.db, id)
			require.NoError(t, err)
			require.Equal(t, store.ApprovalPending, a.Status)

			status, out := f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {duration},
				"digest": {facts.Digest}, "response": {string(assertion)}}, false)
			require.Equal(t, http.StatusOK, status, string(out["error"]))
			leases, err := store.ActiveAccessLeasesFor(ctx, f.db, []string{accAgent.Key()}, time.Now())
			require.NoError(t, err)
			require.Len(t, leases, 1)
			require.WithinDuration(t, time.Now().Add(lengths[duration]), leases[0].ExpiresAt, time.Minute)
		})
	}

	t.Run("facts that changed", func(t *testing.T) {
		f := newAccessFixture(t)
		ctx := context.Background()
		key := f.enrollPasskey(t)
		id, _ := f.file(t)
		one, shown := f.page(t, "/console/access/"+id+"?for=4h")
		assertion, err := key.Get(one["passkey_options"], approverOrigin)
		require.NoError(t, err)
		// Another request of the same agent, for another project: a warning
		// the owner did not see.
		_, err = store.EnsureProject(ctx, f.db, "example-legal", "example-legal")
		require.NoError(t, err)
		_, err = store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: "example-legal", Kind: store.AccessKindRequest,
			Lease: 8 * time.Hour, TTL: 15 * time.Minute, PeerUID: 501, PeerPID: accAgent.PID, ProcessKey: accAgent.Key()}, time.Now())
		require.NoError(t, err)
		_, fresh := f.page(t, "/console/access/"+id)
		require.NotEqual(t, shown.Digest, fresh.Digest)

		status, out := f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {"4h"},
			"digest": {shown.Digest}, "response": {string(assertion)}}, false)
		require.Equal(t, http.StatusConflict, status)
		require.Contains(t, string(out["error"]), "the request's facts changed since this page loaded")
		status, _ = f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {"4h"},
			"digest": {fresh.Digest}, "response": {string(assertion)}}, false)
		require.Equal(t, http.StatusBadRequest, status, "the fresh digest is not what the passkey signed")
		a, err := store.AccessApprovalByID(ctx, f.db, id)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, a.Status)
	})

	t.Run("the page", func(t *testing.T) {
		f := newAccessFixture(t)
		f.enrollPasskey(t)
		id, _ := f.file(t)
		html := func(path string) string {
			req := authedReq(httptest.NewRequest(http.MethodGet, path, nil))
			req.Host = "localhost:8081"
			rr := do(f.mux, req)
			require.Equal(t, http.StatusOK, rr.Code)
			return rr.Body.String()
		}

		// ?for= fixes the duration: shown read-only, with the way back.
		fixed := html("/console/access/" + id + "?for=1h")
		require.Contains(t, fixed, "For how long: <strong>1 hour</strong>")
		require.Contains(t, fixed, `<a href="/console/access/`+id+`" data-seam-query>choose another duration</a>`)
		require.Contains(t, fixed, `data-duration="1h"`)
		require.Contains(t, fixed, `data-options="{`, "the options for that duration")
		require.Contains(t, fixed, "Approve with a passkey")
		require.NotContains(t, fixed, `name="for"`, "no duration to choose")
		require.NotContains(t, fixed, `name="duration"`)
		require.Contains(t, html("/console/access/"+id+"?for=session"), "For how long: <strong>until this session ends (at most 8h)</strong>", "a lease reads 8h, never Go's 8h0m0s")

		// No ?for=, or one the daemon does not offer: the owner chooses, and
		// the choice loads the page for it in place.
		for _, path := range []string{"/console/access/" + id, "/console/access/" + id + "?for=8d"} {
			choose := html(path)
			require.Contains(t, choose, `<form id="access-choose" class="access-form" method="get" action="/console/access/`+id+`" data-seam-query>`, path)
			for _, d := range approval.Durations {
				require.Contains(t, choose, `name="for" value="`+d+`"`, path)
			}
			require.Contains(t, choose, "Continue to approve with a passkey", path)
			require.NotContains(t, choose, `data-options="`, "%s: no options before the duration is chosen", path)
			require.NotContains(t, choose, "For how long: <strong>", path)
		}
	})
}

// A request an older daemon filed for a broad ancestor (the tmux server) is
// shown with its warning first, and the console's passkey approval refuses
// it: the approve core never approves one, whatever the proof.
func TestAccess_ApproveRefusesABroadAnchor(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	tmux := access.ProcessToken{PID: 3698, Version: 36980, Boot: accessBoot}
	row, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: "example-finance", Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: 15 * time.Minute,
		PeerUID: 501, PeerPID: tmux.PID, ProcessKey: tmux.Key(), Program: "/opt/homebrew/Cellar/tmux/3.8/bin/tmux",
	}, time.Now())
	require.NoError(t, err)
	facts, err := f.svc.Facts(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, approval.WarnAnchorIsShell, facts.Warnings[0].Code)

	req := httptest.NewRequest(http.MethodPost, "/console/access/"+row.ID+"/approve",
		strings.NewReader(url.Values{"duration": {"1h"}, "digest": {facts.Digest}, "response": {"{}"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, out := f.jsonDo(t, authedReq(req))
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, string(out["error"]), "never approved; deny it")
	leases, err := store.ActiveAccessLeasesFor(ctx, f.db, []string{tmux.Key()}, time.Now())
	require.NoError(t, err)
	require.Empty(t, leases)
}

// A request past its wait reads expired, by its id or by its short id, even
// before a sweep writes the status: the page offers nothing to decide.
func TestAccess_PageOfARequestPastItsWait(t *testing.T) {
	f := newAccessFixture(t)
	f.enrollPasskey(t)
	id, short := f.file(t)
	_, err := f.db.ExecContext(context.Background(), `UPDATE access_approvals SET expires_at = ? WHERE id = ?`,
		core.FormatTime(time.Now().Add(-time.Minute)), id)
	require.NoError(t, err)
	for _, ref := range []string{id, short} {
		req := authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+ref, nil))
		req.Host = "localhost:8081"
		rr := do(f.mux, req)
		require.Equal(t, http.StatusOK, rr.Code, ref)
		page := rr.Body.String()
		require.Contains(t, page, "This request is <strong>expired</strong>.", ref)
		require.Contains(t, page, "Waited until", ref)
		require.NotContains(t, page, `id="access-deny"`, ref)
		require.NotContains(t, page, "Continue to approve with a passkey", ref)
	}
}
