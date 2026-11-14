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

// Moves and read-and-write grants on the access pages (isolation.requests):
// `seam access request --write` files the kind the asker's home and the
// target call for, and the request's page shows the kind, the carry and the
// read or read-and-write choice, and decides it.

// kindConsole is the advisory tier's console (read grants on, no peer_auth)
// over an open project, two confidential ones, and one conversation's
// ambient row in home, stamped with accAgent.
func kindConsole(t *testing.T, home string) (*grantConsole, string) {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen,
		"example-hr": core.IsolationConfidential, "example-payroll": core.IsolationConfidential} {
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
	id := conversationIn(t, db, home, "ext-kind")
	return &grantConsole{db: db, mux: mux, grants: grants}, id
}

// conversationIn opens a conversation's ambient row in project, stamped with
// accAgent on host laptop, as the SessionStart hook does.
func conversationIn(t *testing.T, db *sql.DB, project, ext string) string {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: "cc/" + ext, ProjectSlug: project,
		Status: core.SessionActive, Ambient: true, Host: "laptop", CWD: "/work/" + project, ExternalClient: "claude-code",
		ExternalSessionID: ext, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, db, id, accAgent.AgentID()))
	return id
}

func (g *grantConsole) html(t *testing.T, path string) string {
	t.Helper()
	rr := do(g.mux, authedReq(httptest.NewRequest(http.MethodGet, path, nil)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

func TestKindsConsole_MoveFromAnOpenHome(t *testing.T) {
	g, home := kindConsole(t, "example-ops")
	ctx := context.Background()

	rr := g.post(t, "/console/access/request", url.Values{"project": {"example-hr"}, "write": {"maybe"}}, true)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), `invalid write \"maybe\": valid values are true and false`)

	rr = g.post(t, "/console/access/request", url.Values{"project": {"example-hr"}, "write": {"true"},
		"reason": {"update the onboarding checklist"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var filed struct {
		ID, Kind, Project, From string
		Filed                   bool
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	require.Equal(t, store.AccessKindMove, filed.Kind)
	require.Equal(t, "example-ops", filed.From)
	require.True(t, filed.Filed)

	page := g.html(t, "/console/access/"+filed.ID)
	require.Contains(t, page, "move into a fenced project for good")
	require.Contains(t, page, "<dt>Leaves</dt><dd>example-ops</dd>")
	require.Contains(t, page, "<dt>Holds</dt><dd>example-hr <span class=\"faint\">(once approved)</span></dd>")
	require.Contains(t, page, "This agent has worked in example-ops. If you approve, it works in example-hr from now on")
	require.Contains(t, page, "a move into a fenced project, for good")
	require.Contains(t, page, `<input type="hidden" name="duration" value="session">`)
	require.Contains(t, page, "How long: <strong>for good: a move does not end</strong>")
	require.NotContains(t, page, `name="duration" value="1h"`, "a move no lock covers asks no duration")
	require.NotContains(t, page, `name="access"`, "a move offers no access choice")
	list := g.html(t, "/console/access")
	require.Contains(t, list, `<span class="badge">move from example-ops</span>`)

	var one accessOneData
	g.getJSON(t, "/console/access/"+filed.ID, &one)
	rr = g.post(t, "/console/access/"+filed.ID+"/approve", url.Values{"duration": {"session"}, "digest": {one.Request.Digest}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var done struct{ Approved, Kind, Project, Session string }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &done))
	require.Equal(t, store.AccessKindMove, done.Kind)
	require.Equal(t, "cc/ext-kind@example-hr", done.Session)
	amb, found, err := store.AmbientSessionByExternalIdentity(ctx, g.db, "claude-code", "ext-kind")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "example-hr", amb.ProjectSlug)
	require.Equal(t, home, amb.ParentSessionID)

	// From the home it moved into, another confidential project is a grant.
	rr = g.post(t, "/console/access/request", url.Values{"project": {"example-payroll"}, "write": {"1"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var next struct{ Kind, From string }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &next))
	require.Equal(t, store.AccessKindGrant, next.Kind, "from its confidential home now, a grant")
	require.Equal(t, "example-hr", next.From)
}

func TestKindsConsole_ReadAndWriteGrant(t *testing.T) {
	g, _ := kindConsole(t, "example-hr")
	rr := g.post(t, "/console/access/request", url.Values{"project": {"example-payroll"}, "write": {"true"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var filed struct{ ID, Kind, From, Access string }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &filed))
	require.Equal(t, store.AccessKindGrant, filed.Kind)
	require.Equal(t, "write", filed.Access)

	page := g.html(t, "/console/access/"+filed.ID)
	require.Contains(t, page, `<input type="radio" name="access" value="read-only" checked> read-only (the default): content from example-payroll may be copied into example-hr`)
	require.Contains(t, page, `<input type="radio" name="access" value="read-write"> read and write: content from example-payroll may be copied into example-hr, and it may change example-payroll&#39;s tasks, memories and notes; content from example-hr may be copied into example-payroll`)
	require.Contains(t, page, "<dt>It asks for</dt><dd>read and write</dd>")
	require.Contains(t, page, "(once approved, read-only)")
	require.Contains(t, page, `name="duration" value="1h"`, "a grant has its duration")
	list := g.html(t, "/console/access")
	require.Contains(t, list, `<span class="badge">read-only or read and write, for example-hr</span><span class="badge">asks to write</span>`)

	var one accessOneData
	g.getJSON(t, "/console/access/"+filed.ID, &one)
	rr = g.post(t, "/console/access/"+filed.ID+"/approve", url.Values{"duration": {"1h"}, "digest": {one.Request.Digest},
		"access": {"read-write"}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Contains(t, loc.Query().Get("notice"), "example-payroll is readable and writable from example-hr, until ")
	require.Contains(t, g.html(t, "/console/access"), "read and write, for example-hr")
	live, err := g.grants.Live(context.Background())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, store.GrantAccessWrite, live[0].Access)
}

// Under peer_auth the passkey page offers the access choice with the
// duration, and the assertion signs both: the page's options for read and
// write approve read and write, and the same assertion sent as read-only
// approves nothing.
func TestKindsConsole_PasskeyPageSignsTheAccess(t *testing.T) {
	f := newAccessFixtureWith(t, true)
	ctx := context.Background()
	for _, slug := range []string{"example-hr", "example-payroll"} {
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, f.db, slug, core.IsolationConfidential))
	}
	key := f.enrollPasskey(t)
	conversationIn(t, f.db, "example-hr", "ext-peer")
	req := httptest.NewRequest(http.MethodPost, "/console/access/request",
		strings.NewReader(url.Values{"project": {"example-payroll"}, "write": {"true"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Seamless-Host", "laptop")
	status, out := f.jsonDo(t, f.asAgent(t, req))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	var id string
	require.NoError(t, json.Unmarshal(out["id"], &id))

	pageReq := authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?for=1h&access=read-write", nil))
	pageReq.Host = "localhost:8081"
	rr := do(f.mux, pageReq)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `data-access="read-write"`)
	require.Contains(t, rr.Body.String(), "Access: <strong>read and write</strong>")
	require.Contains(t, rr.Body.String(), `data-options="{`)
	choose := do(f.mux, func() *http.Request {
		r := authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id, nil))
		r.Host = "localhost:8081"
		return r
	}())
	require.Contains(t, choose.Body.String(), `name="access" value="read-write"`, "the choose step offers the access")

	// The in-session dialog sends the owner here with only ?for= (it chose how
	// long, never the access): the page keeps the access step, with that
	// duration preselected, instead of offering a passkey that signs
	// read-only (P13, 2026-11-14).
	fromDialog := do(f.mux, func() *http.Request {
		r := authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?for=1h", nil))
		r.Host = "localhost:8081"
		return r
	}())
	require.Equal(t, http.StatusOK, fromDialog.Code)
	body := fromDialog.Body.String()
	require.Contains(t, body, `id="access-choose"`)
	require.Contains(t, body, `name="access" value="read-write"`)
	require.Contains(t, body, `name="for" value="1h" checked`, "the dialog's duration stays chosen")
	require.NotContains(t, body, `name="for" value="session" checked`)
	require.NotContains(t, body, `data-options="{`, "no passkey before the access is chosen")
	require.NotContains(t, body, "Access: <strong>read-only</strong>")
	readOnly := do(f.mux, func() *http.Request {
		r := authedReq(httptest.NewRequest(http.MethodGet, "/console/access/"+id+"?for=1h&access=read-only", nil))
		r.Host = "localhost:8081"
		return r
	}())
	require.Contains(t, readOnly.Body.String(), "Access: <strong>read-only</strong>", "read-only chosen on purpose signs it")
	require.Contains(t, readOnly.Body.String(), `data-options="{`)

	one, facts := f.page(t, "/console/access/"+id+"?for=1h&access=read-write")
	require.JSONEq(t, `"read-write"`, string(one["access"]))
	opts := one["passkey_options"]
	require.NotEmpty(t, opts)
	assertion, err := key.Get(opts, approverOrigin)
	require.NoError(t, err)

	status, out = f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {"1h"}, "digest": {facts.Digest},
		"response": {string(assertion)}}, false)
	require.Equal(t, http.StatusConflict, status, "signed read and write, sent as read-only")
	require.Contains(t, string(out["error"]), "the passkey approved this request for another duration, or other facts")
	status, out = f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {"1h"}, "digest": {facts.Digest},
		"response": {string(assertion)}, "access": {"read-write"}}, false)
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	require.JSONEq(t, `"write"`, string(out["access"]))
}

// A move is never approved with Touch ID: the approver of this release does
// not word one. The console says how to approve it instead.
func TestKindsConsole_TouchIDApprovesNoMove(t *testing.T) {
	f := newAccessFixtureWith(t, true)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, f.db, "example-hr", "example-hr")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, "example-hr", core.IsolationConfidential))
	conversationIn(t, f.db, "example-ops", "ext-touch")
	req := httptest.NewRequest(http.MethodPost, "/console/access/request",
		strings.NewReader(url.Values{"project": {"example-hr"}, "write": {"true"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Seamless-Host", "laptop")
	status, out := f.jsonDo(t, f.asAgent(t, req))
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	require.JSONEq(t, `"move"`, string(out["kind"]))
	var id string
	require.NoError(t, json.Unmarshal(out["id"], &id))
	_, facts := f.page(t, "/console/access/"+id)
	status, out = f.post(t, "/console/access/"+id+"/approve", url.Values{"duration": {"session"}, "digest": {facts.Digest},
		"proof": {"touchid"}, "credential": {"01KCRED"}, "signature": {"AQID"}}, true)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, string(out["error"]), "a move is approved with a passkey or seam approve in the owner's own terminal")
	a, err := store.AccessApprovalByID(ctx, f.db, id)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalPending, a.Status)
}
