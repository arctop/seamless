package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Read grants on the access pages (isolation.requests). Off -- Grants nil --
// nothing on them changes; on without peer_auth (the advisory tier) the page
// lists a confidential project's requests, approves one with the bearer key,
// and revokes a live grant.

type grantConsole struct {
	db     *sql.DB
	mux    *http.ServeMux
	grants *approval.Grants
}

func newGrantConsole(t *testing.T) *grantConsole {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen, "example-finance": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	rec := events.NewRecorder(db)
	grants := approval.NewGrants(approval.GrantsConfig{DB: db, Events: rec, LeaseCap: 8 * time.Hour})
	cs, err := New(Config{DB: db, Events: rec, APIKey: testKey, Level: "advanced", Grants: grants})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	// The agent's session, stamped with its process as the SessionStart hook does.
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: "cc/ops", ProjectSlug: "example-ops",
		Status: core.SessionActive, Ambient: true, Host: "laptop", CWD: "/work/example-ops", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, db, id, accAgent.AgentID()))
	return &grantConsole{db: db, mux: mux, grants: grants}
}

func (g *grantConsole) post(t *testing.T, path string, form url.Values, asJSON bool) *httptest.ResponseRecorder {
	t.Helper()
	req := authedReq(httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Seamless-Agent-Process", accAgent.AgentID())
	req.Header.Set("X-Seamless-Host", "laptop")
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	return do(g.mux, req)
}

func (g *grantConsole) getJSON(t *testing.T, path string, v any) {
	t.Helper()
	req := authedReq(httptest.NewRequest(http.MethodGet, path, nil))
	req.Header.Set("Accept", "application/json")
	rr := do(g.mux, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), v), rr.Body.String())
}

// Off: no lock and no read grants -- the access routes, the grant revoke
// included, are the 404 they always were, and the sidebar has no access
// screen.
func TestGrantsConsole_OffIsNotFound(t *testing.T) {
	_, mux := newConsole(t)
	for _, path := range []string{"/console/access", "/console/access/R-AAAA"} {
		rr := do(mux, authedReq(httptest.NewRequest(http.MethodGet, path, nil)))
		require.Equal(t, http.StatusNotFound, rr.Code, path)
		require.Contains(t, rr.Body.String(), "no project is locked, so there are no approvers to manage")
	}
	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/access/grants/01KANY/revoke", nil))
	rr := do(mux, req)
	require.Equal(t, http.StatusNotFound, rr.Code)
	rr = do(mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/", nil)))
	require.NotContains(t, rr.Body.String(), `href="/console/access"`)
	require.Equal(t, []screen{}, withAccessScreens([]screen{}, false, false))
}

// A locked daemon with read grants off lists no grants: the JSON has no grants
// key and the page no Read grants section.
func TestGrantsConsole_LockWithoutGrantsShowsNone(t *testing.T) {
	f := newAccessFixture(t)
	f.file(t)
	status, out := f.jsonDo(t, authedReq(httptest.NewRequest(http.MethodGet, "/console/access", nil)))
	require.Equal(t, http.StatusOK, status)
	_, has := out["grants"]
	require.False(t, has)
	_, has = out["advisory"]
	require.False(t, has)
	rr := do(f.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access", nil)))
	require.NotContains(t, rr.Body.String(), "Read grants")
	require.Contains(t, rr.Body.String(), "Agent leases")
}

func TestGrantsConsole_AdvisoryRequestApproveRevoke(t *testing.T) {
	g := newGrantConsole(t)
	ctx := context.Background()

	// The sidebar offers the access screen, never the approvers one.
	rr := do(g.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/", nil)))
	require.Contains(t, rr.Body.String(), `href="/console/access"`)
	require.NotContains(t, rr.Body.String(), `href="/console/approvers"`)

	// seam access request, from the agent's shell.
	rr = g.post(t, "/console/access/request", url.Values{"project": {"example-finance"}, "reason": {"check the invoice totals"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var filed struct {
		ID    string `json:"id"`
		Short string `json:"short_id"`
		Kind  string `json:"kind"`
		From  string `json:"from"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	require.Equal(t, store.AccessKindGrant, filed.Kind)
	require.Equal(t, "example-ops", filed.From)
	rr = g.post(t, "/console/access/request", url.Values{"project": {"example-ops"}}, true)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "not confidential: nothing to request")

	var list struct {
		Requests []approval.Facts `json:"requests"`
		Advisory bool             `json:"advisory"`
		Grants   []grantRow       `json:"grants"`
	}
	g.getJSON(t, "/console/access", &list)
	require.True(t, list.Advisory)
	require.Len(t, list.Requests, 1)
	require.Equal(t, "read-only", list.Requests[0].Access)
	require.Empty(t, list.Grants)

	var one struct {
		Request  approval.Facts `json:"request"`
		Advisory bool           `json:"advisory"`
	}
	g.getJSON(t, "/console/access/"+filed.Short, &one)
	require.True(t, one.Advisory)
	require.Contains(t, one.Request.Warnings, approval.Warning{Code: approval.WarnCopyInto,
		Text: "content from example-finance may be copied into example-ops's memories"})
	page := do(g.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+filed.ID, nil))).Body.String()
	require.Contains(t, page, "content from example-finance may be copied into example-ops&#39;s memories")
	require.Contains(t, page, "this approval is advisory")
	require.Contains(t, page, `action="/console/access/`+filed.ID+`/approve"`)

	// Approve with the bearer key: the advisory tier's decision.
	rr = g.post(t, "/console/access/"+filed.ID+"/approve", url.Values{"duration": {"1h"}, "digest": {"stale"}}, true)
	require.Equal(t, http.StatusConflict, rr.Code)
	rr = g.post(t, "/console/access/"+filed.ID+"/approve", url.Values{"duration": {"1h"}, "digest": {one.Request.Digest}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	g.getJSON(t, "/console/access", &list)
	require.Empty(t, list.Requests)
	require.Len(t, list.Grants, 1)
	require.Equal(t, "example-finance", list.Grants[0].Project)
	require.Equal(t, "example-ops", list.Grants[0].From)
	require.Equal(t, "cc/ops", list.Grants[0].Session)
	html := do(g.mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/access", nil))).Body.String()
	require.Contains(t, html, "Read grants")
	require.Contains(t, html, `action="/console/access/grants/`+list.Grants[0].ID+`/revoke"`)
	require.NotContains(t, html, "Agent leases", "no lease exists without peer_auth")

	// Revoke: a plain form answers with the page and a notice.
	rr = g.post(t, "/console/access/grants/"+list.Grants[0].ID+"/revoke", url.Values{}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	live, err := store.LiveAccessGrants(ctx, g.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, live)
	rr = g.post(t, "/console/access/grants/"+list.Grants[0].ID+"/revoke", url.Values{}, true)
	require.Equal(t, http.StatusNotFound, rr.Code)

	// Deny, through the plain form.
	rr = g.post(t, "/console/access/request", url.Values{"project": {"example-finance"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	rr = g.post(t, "/console/access/"+filed.ID+"/deny", url.Values{"next": {"/console/access/" + filed.ID}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	a, err := store.AccessApprovalByID(ctx, g.db, filed.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalDenied, a.Status)
}

// Under peer_auth the bearer key proves nothing for a read grant: the
// approve route without a passkey response is refused.
func TestGrantsConsole_PeerTierNeedsAProof(t *testing.T) {
	f := newAccessFixture(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, f.db, "example-hr", "example-hr")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, "example-hr", core.IsolationConfidential))
	_, err = store.EnsureProject(ctx, f.db, "example-ops", "example-ops")
	require.NoError(t, err)
	grants := approval.NewGrants(approval.GrantsConfig{Approvals: f.svc})
	cs, err := New(Config{DB: f.db, Events: events.NewRecorder(f.db), APIKey: testKey, Level: "advanced", Approvals: f.svc, Grants: grants})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, f.db, core.Session{ID: id, Name: "cc/ops", ProjectSlug: "example-ops",
		Status: core.SessionActive, Ambient: true, CWD: "/work/example-ops", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, f.db, id, accAgent.AgentID()))

	req := httptest.NewRequest(http.MethodPost, "/console/access/request", strings.NewReader("project=example-hr"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr := do(mux, f.asAgent(t, req))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var filed map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	filedID := filed["id"].(string)
	facts, err := grants.Facts(ctx, filedID)
	require.NoError(t, err)

	req = authedReq(httptest.NewRequest(http.MethodPost, "/console/access/"+filedID+"/approve",
		strings.NewReader(url.Values{"duration": {"1h"}, "digest": {facts.Digest}}.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr = do(mux, req)
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "approving needs a passkey here, or seam approve")
	live, err := store.LiveAccessGrants(ctx, f.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, live)

	// A locked project still goes the lock's way.
	req = httptest.NewRequest(http.MethodPost, "/console/access/request", strings.NewReader("project=example-finance"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr = do(mux, f.asAgent(t, req))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	require.Equal(t, store.AccessKindRequest, filed["kind"])
}
