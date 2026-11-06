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
	cs, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "advanced", Approvals: svc,
		Terminal: &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"},
	})
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
	var page string
	require.NoError(t, json.Unmarshal(out["page"], &page))
	require.Equal(t, approverOrigin+"/console/access/"+id, page)
	return id, short
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

func TestAccess_PasskeyApproveAndDeny(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	code, _, err := store.MintEnrolCode(ctx, f.db, store.ApproverPasskey, time.Now())
	require.NoError(t, err)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	begun, err := f.svc.BeginPasskey(ctx, "key", code)
	require.NoError(t, err)
	created, err := key.Create(begun.Options, approverOrigin)
	require.NoError(t, err)
	_, err = f.svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)

	id, _ := f.file(t)
	status, one := f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id, nil)))
	require.Equal(t, http.StatusOK, status)
	var facts approval.Facts
	require.NoError(t, json.Unmarshal(one["request"], &facts))
	assertion, err := key.Get(one["passkey_options"], approverOrigin)
	require.NoError(t, err)

	post := func(path string, form url.Values, agent bool) (int, map[string]json.RawMessage) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if agent {
			req = f.asAgent(t, req)
		} else {
			req = authedReq(req)
		}
		return f.jsonDo(t, req)
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
