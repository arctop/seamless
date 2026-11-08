package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The locked console's fixture: example-finance is locked, example-ops is
// the owner's open project. Every secret string lives only in the locked one.
const (
	lockedSlug   = "example-finance"
	openSlug     = "example-ops"
	lockedSecret = "LEDGER-SECRET-7Q"
	openMarker   = "OPS-OWN-CONTENT"
)

type lockedConsole struct {
	db     *sql.DB
	cs     *Service
	rec    *events.Recorder
	mux    http.Handler
	svc    *approval.Service
	policy *access.Policy
	mgr    *files.Manager

	memID, noteID, taskID, sessID, plan string
}

func newLockedConsole(t *testing.T) *lockedConsole {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for _, slug := range []string{lockedSlug, openSlug} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, lockedSlug, core.IsolationConfidential))

	lc := &lockedConsole{db: db, mgr: mgr, plan: "ledger-plan"}
	now := time.Now().UTC()
	mem := writeMemory(t, mgr, core.KindGotcha, lockedSlug, "ledger-close", "the close runs "+lockedSecret)
	lc.memID = mem.ID
	writeMemory(t, mgr, core.KindGotcha, openSlug, "ops-runbook", "the ops runbook "+openMarker)
	noteID, err := core.NewID()
	require.NoError(t, err)
	_, err = mgr.WriteNote(ctx, core.Note{ID: noteID, Slug: "ledger-narrative", Title: "Ledger plan " + lockedSecret, Project: lockedSlug,
		Description: "the plan " + lockedSecret, Body: "# Ledger\n\n" + lockedSecret,
		Tags: []string{plans.SlugTag(lc.plan), "created-by:agent"}, Created: now, Updated: now})
	require.NoError(t, err)
	lc.noteID = noteID
	lc.taskID, err = core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: lc.taskID, ProjectSlug: lockedSlug, Title: "reconcile " + lockedSecret,
		Status: core.TaskOpen, CreatedBy: "test", CreatedAt: now, UpdatedAt: now}))
	lc.sessID, err = core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: lc.sessID, Name: "cc/ledger", ProjectSlug: lockedSlug,
		Status: core.SessionActive, Findings: "found " + lockedSecret, CreatedAt: now, UpdatedAt: now}))

	lc.policy, err = access.New(config.PeerAuth{Owner: "owner", Locked: []string{lockedSlug}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	views, err := store.OpenViews(path, nil, lc.policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	rec := events.NewRecorder(db)
	lc.svc = approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}, Events: rec})
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	cs, err := New(Config{
		DB: base, WriteDB: db, Files: mgr, Events: rec.ReadingFrom(base), Retrieve: ret, DataDir: filepath.Join(dir, "data"),
		APIKey: testKey, Level: "advanced", Approvals: lc.svc, Views: views,
		Terminal: &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"},
	})
	require.NoError(t, err)
	lc.cs, lc.rec = cs, rec
	mux := http.NewServeMux()
	cs.Register(mux)
	owner := lc.policy.OwnerView()
	// What the daemon's ownerHandler does on TCP; a socket request in a test
	// arrives with its view already set (socket below).
	lc.mux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := access.FromContext(r.Context()); !ok {
			r = r.WithContext(access.WithView(r.Context(), owner))
		}
		mux.ServeHTTP(w, r)
	})
	return lc
}

// browser is a request from a signed-in browser, with its unlock cookie when
// unlock is set. A write carries the same-origin header a browser sends.
func (lc *lockedConsole) browser(method, target, unlock string, form url.Values) *http.Request {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "http://localhost:8081"+target, body)
	req.AddCookie(consoleCookie())
	if unlock != "" {
		req.AddCookie(&http.Cookie{Name: unlockCookieName, Value: unlock})
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return req
}

func (lc *lockedConsole) do(req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	lc.mux.ServeHTTP(rr, req)
	return rr
}

func (lc *lockedConsole) get(t *testing.T, target, unlock string) (int, string) {
	t.Helper()
	rr := lc.do(lc.browser(http.MethodGet, target, unlock, nil))
	return rr.Code, rr.Body.String()
}

// unlockLink unlocks the locked project for a fresh browser the way
// console-open --unlock does, and returns that browser's cookie value.
func (lc *lockedConsole) unlockLink(t *testing.T) string {
	t.Helper()
	secret, _, err := store.CreateConsoleUnlockLink(context.Background(), lc.db, lockedSlug, 3*time.Hour, 10*time.Minute, time.Now())
	require.NoError(t, err)
	rr := lc.do(lc.browser(http.MethodGet, "/console/unlock?t="+url.QueryEscape(secret), "", nil))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "Unlocked in this browser", "the answer is the unlock page itself")
	require.Contains(t, rr.Body.String(), `history.replaceState(null, '', '/console/unlock/' + encodeURIComponent("`+lockedSlug+`"))`,
		"the used link leaves the address bar")
	require.NotContains(t, rr.Body.String(), secret)
	for _, c := range rr.Result().Cookies() {
		if c.Name == unlockCookieName {
			require.True(t, c.HttpOnly)
			require.Equal(t, http.SameSiteStrictMode, c.SameSite)
			require.Equal(t, "/console", c.Path)
			require.Positive(t, c.MaxAge)
			return c.Value
		}
	}
	t.Fatal("the claim set no unlock cookie")
	return ""
}

// consoleGETRoutes reads the console's GET routes from its source, as the
// peer_auth matrix does, so a route added later is crawled too.
func consoleGETRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("console.go")
	require.NoError(t, err)
	var out []string
	for _, m := range regexp.MustCompile(`handle\("GET (/console/[^"]*)"`).FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	require.NotEmpty(t, out)
	return out
}

func (lc *lockedConsole) crawlTargets(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range consoleGETRoutes(t) {
		if p == "/console/events" || strings.HasPrefix(p, "/console/static/") || p == "/console/login" || p == "/console/unlock" {
			continue // the live feed streams; statics and login are public; a claim needs a link
		}
		p = strings.TrimSuffix(p, "{$}")
		for _, r := range []struct{ prefix, from, to string }{
			{"/console/sessions/", "{id}", lc.sessID}, {"/console/memories/", "{id}", lc.memID},
			{"/console/notes/", "{id}", lc.noteID}, {"/console/tasks/", "{id}", lc.taskID},
			{"/console/plans/", "{slug}", lc.plan}, {"/console/projects/", "{slug}", lockedSlug},
			{"/console/unlock/", "{project}", lockedSlug}, {"/console/labs/", "{name...}", "lab"},
		} {
			if strings.HasPrefix(p, r.prefix) {
				p = strings.Replace(p, r.from, r.to, 1)
			}
		}
		p = strings.NewReplacer("{id}", "01NOSUCHTHING", "{slug}", "no-such", "{name...}", "no-such").Replace(p)
		out = append(out, p)
	}
	return append(out,
		"/console/search?q="+url.QueryEscape(lockedSecret), "/console/search?q=ledger",
		"/console/memories?project="+lockedSlug, "/console/notes?project="+lockedSlug, "/console/tasks?project="+lockedSlug,
		"/console/sessions?project="+lockedSlug, "/console/projects/"+lockedSlug+"?tab=memories",
		"/console/plans?project="+lockedSlug+"&w=all", "/console/plans?w=all")
}

// The console crawl, locked: no page, HTML or JSON, holds the locked
// project's content or its items' ids, while its name and counts show where
// projects are listed.
func TestLockedConsole_CrawlLocked(t *testing.T) {
	lc := newLockedConsole(t)
	sawOwn := false
	for _, target := range lc.crawlTargets(t) {
		for _, asJSON := range []bool{false, true} {
			req := lc.browser(http.MethodGet, target, "", nil)
			if asJSON {
				req.Header.Set("Accept", "application/json")
			}
			rr := lc.do(req)
			body := rr.Body.String()
			require.Less(t, rr.Code, 500, "GET %s (json=%v): %s", target, asJSON, body)
			if !strings.Contains(target, lockedSecret) { // a search echoes its own query
				require.NotContains(t, body, lockedSecret, "GET %s (json=%v) leaked locked content", target, asJSON)
			}
			// An error that echoes the id the URL asked for discloses
			// nothing the caller did not already hold.
			for _, id := range []string{lc.memID, lc.noteID, lc.taskID, lc.sessID, "ledger-close", "cc/ledger"} {
				if !strings.Contains(target, id) {
					require.NotContains(t, body, id, "GET %s (json=%v) named a locked item", target, asJSON)
				}
			}
			sawOwn = sawOwn || strings.Contains(body, openMarker)
		}
	}
	require.True(t, sawOwn, "the crawl reached the owner's own content: it is not an empty console")

	// The board lists the locked project by name, with its counts.
	var board projectsData
	rr := lc.do(func() *http.Request {
		r := lc.browser(http.MethodGet, "/console/projects?format=json", "", nil)
		return r
	}())
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &board))
	require.Equal(t, []store.ProjectTally{{Slug: lockedSlug, Name: lockedSlug, Registered: true, Memories: 1, Notes: 1, Sessions: 1, OpenTasks: 1}}, board.Locked)
	_, html := lc.get(t, "/console/projects", "")
	require.Contains(t, html, `href="/console/unlock/`+lockedSlug+`"`)
	require.Contains(t, html, "project-locked")

	// Its workspace goes to its unlock page; a script is told it is locked.
	rr = lc.do(lc.browser(http.MethodGet, "/console/projects/"+lockedSlug, "", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/unlock/"+lockedSlug, rr.Header().Get("Location"))
	req := lc.browser(http.MethodGet, "/console/projects/"+lockedSlug, "", nil)
	req.Header.Set("Accept", "application/json")
	rr = lc.do(req)
	require.Equal(t, http.StatusForbidden, rr.Code)
	require.Contains(t, rr.Body.String(), "is locked")

	status, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, "Open tasks")
	require.Contains(t, page, "console-open --unlock "+lockedSlug+" --for 3h")
	require.Contains(t, page, "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd")
	require.Contains(t, page, "No passkey is enrolled yet")

	// The open project is not locked: nothing to unlock.
	status, _ = lc.get(t, "/console/unlock/"+openSlug, "")
	require.Equal(t, http.StatusNotFound, status)

	// The nav offers the access screens while a project is locked.
	require.Contains(t, page, `href="/console/access" data-key="a"`)
	require.Contains(t, page, `href="/console/approvers" data-key="k"`)
}

func TestLockedConsole_UnlockedBrowserReads(t *testing.T) {
	lc := newLockedConsole(t)
	cookie := lc.unlockLink(t)

	readsLocked := func(unlock string) bool {
		t.Helper()
		status, body := lc.get(t, "/console/memories?project="+lockedSlug, unlock)
		require.Equal(t, http.StatusOK, status)
		return strings.Contains(body, lockedSecret)
	}
	require.True(t, readsLocked(cookie), "the unlocked browser reads the project")
	status, body := lc.get(t, "/console/projects/"+lockedSlug+"?tab=memories", cookie)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "ledger-close")
	_, body = lc.get(t, "/console/plans/"+lc.plan, cookie)
	require.Contains(t, body, lockedSecret)
	_, body = lc.get(t, "/console/search?q="+url.QueryEscape("close"), cookie)
	require.Contains(t, body, "ledger-close", "search reaches the unlocked project")
	_, body = lc.get(t, "/console/unlock/"+lockedSlug, cookie)
	require.Contains(t, body, "Unlocked in this browser")
	require.Contains(t, body, "Lock it again")
	var board projectsData
	rr := lc.do(func() *http.Request {
		r := lc.browser(http.MethodGet, "/console/projects?format=json", cookie, nil)
		return r
	}())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &board))
	require.Empty(t, board.Locked, "an unlocked project is a board row, not a locked one")

	// Another browser, a forged cookie, and the TCP key with the cookie: locked.
	require.False(t, readsLocked(""), "another browser stays locked")
	forged, err := store.NewConsoleToken()
	require.NoError(t, err)
	require.False(t, readsLocked(forged), "a forged cookie opens nothing")
	bearer := authedReq(httptest.NewRequest(http.MethodGet, "/console/memories?project="+lockedSlug, nil))
	bearer.AddCookie(&http.Cookie{Name: unlockCookieName, Value: cookie})
	rr = lc.do(bearer)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), lockedSecret, "an unlock is a browser's, never the key's")

	// The unlock never reaches an agent: no lease exists.
	leases, err := store.LiveAccessLeases(context.Background(), lc.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, leases)

	// Expired: locked again.
	_, err = lc.db.Exec(`UPDATE console_unlocks SET expires_at = ? WHERE status = 'live'`, core.FormatTime(time.Now().Add(-time.Second)))
	require.NoError(t, err)
	require.False(t, readsLocked(cookie), "an expired unlock opens nothing")
}

func TestLockedConsole_LockAgainAndRevoke(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	cookie := lc.unlockLink(t)
	live, err := store.LiveConsoleUnlocks(ctx, lc.db, store.HashConsoleToken(cookie), time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	id := live[0].ID

	status, page := lc.get(t, "/console/access", cookie)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, "this browser")
	require.Contains(t, page, "the daemon&#39;s account (console-open)")

	// A cross-origin write is refused (console-csrf-origin-check-contract).
	cross := lc.browser(http.MethodPost, "/console/access/unlocks/"+id+"/end", cookie, url.Values{})
	cross.Header.Set("Sec-Fetch-Site", "same-site")
	require.Equal(t, http.StatusForbidden, lc.do(cross).Code)
	// An agent on the socket may not lock or revoke.
	view, ok := lc.policy.PeerChainView(501, []access.ProcessToken{{PID: 300, Version: 3000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}})
	require.True(t, ok)
	sock := httptest.NewRequest(http.MethodPost, "/console/access/unlocks/"+id+"/end", strings.NewReader(""))
	sock.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sock.Header.Set("Accept", "application/json")
	require.Equal(t, http.StatusForbidden, lc.do(sock.WithContext(access.WithView(sock.Context(), view))).Code)

	rr := lc.do(lc.browser(http.MethodPost, "/console/access/unlocks/"+id+"/end", cookie, url.Values{"next": {"/console/unlock/" + lockedSlug}}))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/unlock/"+lockedSlug+"?notice="))
	_, body := lc.get(t, "/console/memories?project="+lockedSlug, cookie)
	require.NotContains(t, body, lockedSecret, "locked again")
	rr = lc.do(lc.browser(http.MethodPost, "/console/access/unlocks/"+id+"/end", cookie, url.Values{}))
	require.Equal(t, http.StatusNotFound, rr.Code, "it already ended")

	// An agent lease, revoked from the access page.
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	a, err := store.CreateAccessApproval(ctx, lc.db, store.AccessRequest{Project: lockedSlug, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key(), SessionName: "cc/ledger"}, time.Now())
	require.NoError(t, err)
	lease, err := store.ApproveAccessFor(ctx, lc.db, a.ID, a.Challenge, "daemon-account", time.Hour, time.Now())
	require.NoError(t, err)
	_, page = lc.get(t, "/console/access", "")
	require.Contains(t, page, `/console/access/leases/`+lease.ID+`/revoke`)
	require.Contains(t, page, "cc/ledger")
	sock = httptest.NewRequest(http.MethodPost, "/console/access/leases/"+lease.ID+"/revoke", strings.NewReader(""))
	sock.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sock.Header.Set("Accept", "application/json")
	require.Equal(t, http.StatusForbidden, lc.do(sock.WithContext(access.WithView(sock.Context(), view))).Code, "an agent cannot revoke")
	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/access/leases/"+lease.ID+"/revoke", strings.NewReader("")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr = lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	leases, err := store.LiveAccessLeases(ctx, lc.db, time.Now())
	require.NoError(t, err)
	require.Empty(t, leases)
	revoked, err := store.AccessLeaseByID(ctx, lc.db, lease.ID)
	require.NoError(t, err)
	require.Equal(t, store.LeaseRevokedOwner, revoked.RevokedReason)
}

func TestLockedConsole_PasskeyUnlock(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	code, _, err := store.MintEnrollCode(ctx, lc.db, store.ApproverPasskey, time.Now())
	require.NoError(t, err)
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	begun, err := lc.svc.BeginPasskey(ctx, "key", code)
	require.NoError(t, err)
	created, err := key.Create(begun.Options, approverOrigin)
	require.NoError(t, err)
	_, err = lc.svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)

	_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
	require.Contains(t, page, `id="unlock-form"`, "on the passkey origin the button is offered")

	// The TCP key cannot begin one: an unlock is per browser.
	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/unlock/"+lockedSlug+"/begin", strings.NewReader("")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.Equal(t, http.StatusForbidden, lc.do(req).Code)

	rr := lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/begin", "", url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var started approval.Begun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &started))
	var cookie string
	for _, c := range rr.Result().Cookies() {
		if c.Name == unlockCookieName {
			cookie = c.Value
			require.True(t, c.HttpOnly)
			require.Equal(t, http.SameSiteStrictMode, c.SameSite)
		}
	}
	require.NotEmpty(t, cookie, "begin issues the browser its cookie")
	assertion, err := key.Get(started.Options, approverOrigin)
	require.NoError(t, err)

	// Another browser presenting this ceremony's assertion gets nothing.
	other, err := store.NewConsoleToken()
	require.NoError(t, err)
	rr = lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/finish", other, url.Values{"id": {started.ID}, "response": {string(assertion)}}))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	// Without the browser's cookie at all, neither.
	rr = lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/finish", "", url.Values{"id": {started.ID}, "response": {string(assertion)}}))
	require.Equal(t, http.StatusConflict, rr.Code)

	rr = lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/finish", cookie, url.Values{"id": {started.ID}, "response": {string(assertion)}}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), `"unlocked": "`+lockedSlug+`"`)
	_, body := lc.get(t, "/console/memories?project="+lockedSlug, cookie)
	require.Contains(t, body, lockedSecret, "the passkey unlock opens the project in this browser")
	_, body = lc.get(t, "/console/memories?project="+lockedSlug, "")
	require.NotContains(t, body, lockedSecret, "and only in this browser")

	// A second project unlock in the same browser keeps the same cookie.
	rr = lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/begin", cookie, url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code)
	for _, c := range rr.Result().Cookies() {
		require.NotEqual(t, unlockCookieName, c.Name, "a browser with a live unlock keeps its cookie")
	}
}

func TestLockedConsole_ClaimLink(t *testing.T) {
	lc := newLockedConsole(t)
	secret, _, err := store.CreateConsoleUnlockLink(context.Background(), lc.db, lockedSlug, time.Hour, 10*time.Minute, time.Now())
	require.NoError(t, err)
	// The TCP key cannot claim: a link is for a browser.
	rr := lc.do(authedReq(httptest.NewRequest(http.MethodGet, "/console/unlock?t="+url.QueryEscape(secret), nil)))
	require.Equal(t, http.StatusBadRequest, rr.Code)
	rr = lc.do(lc.browser(http.MethodGet, "/console/unlock", "", nil))
	require.Equal(t, http.StatusBadRequest, rr.Code, "an incomplete link")
	rr = lc.do(lc.browser(http.MethodGet, "/console/unlock?t="+url.QueryEscape(secret), "", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	rr = lc.do(lc.browser(http.MethodGet, "/console/unlock?t="+url.QueryEscape(secret), "", nil))
	require.Equal(t, http.StatusNotFound, rr.Code, "a link works once")
	evs, err := events.NewRecorder(lc.db).ByKinds(context.Background(), []core.EventKind{approval.EventConsoleUnlocked}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
}

// A `seam` command on the socket reads through its agent tree's lease: a
// leased tree's `seam plan show` reads the locked project while another tree
// is refused.
func TestLockedConsole_LeasedSocketReads(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	const boot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: boot}
	cli := access.ProcessToken{PID: 320, Version: 3200, Boot: boot}
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: boot}
	a, err := store.CreateAccessApproval(ctx, lc.db, store.AccessRequest{Project: lockedSlug, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key()}, time.Now())
	require.NoError(t, err)
	_, err = store.ApproveAccessFor(ctx, lc.db, a.ID, a.Challenge, "daemon-account", time.Hour, time.Now())
	require.NoError(t, err)

	socket := func(target string, chain ...access.ProcessToken) (int, string) {
		t.Helper()
		v, ok := lc.policy.PeerChainView(501, chain)
		require.True(t, ok)
		keys := make([]string, len(chain))
		for i, c := range chain {
			keys[i] = c.Key()
		}
		rows, err := store.ActiveAccessLeasesFor(ctx, lc.db, keys, time.Now())
		require.NoError(t, err)
		v = v.WithLeases(time.Now(), store.AccessLeases(rows))
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Accept", "application/json")
		rr := lc.do(req.WithContext(access.WithView(req.Context(), v)))
		return rr.Code, rr.Body.String()
	}
	status, body := socket("/console/plans/"+lc.plan+"?format=json", cli, agent)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, lockedSecret, "the leased tree's seam plan show reads the project")
	status, body = socket("/console/plans/"+lc.plan+"?format=json", stranger)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "is locked", "another tree's seam plan show is told the lock, not not-found")
	require.NotContains(t, body, lockedSecret)
	status, body = socket("/console/tasks/"+lc.taskID+"?format=json", cli, agent)
	require.Equal(t, http.StatusOK, status, body)
	status, _ = socket("/console/tasks/"+lc.taskID+"?format=json", stranger)
	require.Equal(t, http.StatusNotFound, status)

	// A browser stays locked while the tree holds its lease.
	_, html := lc.get(t, "/console/plans/"+lc.plan, "")
	require.NotContains(t, html, lockedSecret)
}

// With nothing locked, nothing an existing user sees changes: no access
// screens in the sidebar, the palette or the docs matrix, no locked rows, no
// unlock pages, and an unlock cookie is not even looked at.
func TestConsoleOff_LockedSurfacesAbsent(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "example-ops", "example-ops")
	require.NoError(t, err)
	// A live unlock row and its cookie exist; a console with nothing locked
	// must not care.
	secret, _, err := store.CreateConsoleUnlockLink(ctx, db, "example-ops", time.Hour, time.Minute, time.Now())
	require.NoError(t, err)
	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	_, err = store.ClaimConsoleUnlockLink(ctx, db, secret, store.HashConsoleToken(cookie), time.Now())
	require.NoError(t, err)

	page := func(target, unlock string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(consoleCookie())
		if unlock != "" {
			req.AddCookie(&http.Cookie{Name: unlockCookieName, Value: unlock})
		}
		rr := do(mux, req)
		require.Equal(t, http.StatusOK, rr.Code, target)
		return rr.Body.String()
	}
	for _, target := range []string{"/console/projects", "/console/settings", "/console/projects?format=json", "/console/memories?format=json"} {
		without := page(target, "")
		require.Equal(t, without, page(target, cookie), "%s: an unlock cookie changes nothing with nothing locked", target)
		for _, s := range []string{"Access requests", `href="/console/approvers"`, "project-locked", `"locked"`, "/console/unlock"} {
			require.NotContains(t, without, s, target)
		}
	}
	for _, target := range []string{"/console/unlock/example-ops", "/console/unlock?t=" + secret} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(consoleCookie())
		require.Equal(t, http.StatusNotFound, do(mux, req).Code, target)
	}
	for _, target := range []string{"/console/unlock/example-ops/begin", "/console/access/unlocks/x/end", "/console/access/leases/x/revoke"} {
		req := authedReq(httptest.NewRequest(http.MethodPost, target, strings.NewReader("")))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.Equal(t, http.StatusNotFound, do(mux, req).Code, target)
	}
}

// The access screens are not in the registry: a default install's sidebar,
// palette and docs matrix are exactly the registry's.
func TestLockScreens_OnlyWhileLocked(t *testing.T) {
	for _, sc := range screenRegistry() {
		for _, lock := range lockScreens {
			require.NotEqual(t, lock.ID, sc.ID)
			require.NotEqual(t, lock.Key, sc.Key, "g-chord %s is taken by %s", lock.Key, sc.ID)
			require.NotEqual(t, lock.Href, sc.Href)
		}
	}
	for _, row := range LevelMatrix() {
		for _, lock := range lockScreens {
			require.NotEqual(t, lock.Label, row.Name, "the docs matrix names no lock-only screen")
		}
	}
	for _, lock := range lockScreens {
		if lock.Badge != "" {
			_, ok := navCounts{}.count(lock.Badge)
			require.True(t, ok, lock.Badge)
		}
	}
	for _, lvl := range []level{levelBasic, levelStandard, levelAdvanced} {
		visible := visibleScreens(featuresAll(true), lvl)
		require.Equal(t, visible, withLockScreens(visible, false), "off: the same screens")
		with := withLockScreens(visible, true)
		require.Len(t, with, len(visible)+len(lockScreens))
		ids := make([]string, len(with))
		for i, sc := range with {
			ids[i] = sc.ID
		}
		at := slices.Index(ids, "access")
		require.Equal(t, "approvers", ids[at+1])
		require.Equal(t, "settings", ids[at+2], "the access screens sit just above Settings")
		groups := navGroups(with, "", navCounts{})
		seen := map[string]bool{}
		for _, g := range groups {
			require.False(t, seen[g.Label], "group %q is contiguous", g.Label)
			seen[g.Label] = true
		}
	}
}

// The live feed follows the unlock: an event of the locked project reaches an
// unlocked browser's stream, never another browser's, and stops the moment the
// unlock is locked again -- on the stream already open.
func TestLockedConsole_LiveFeedFollowsTheUnlock(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	cookie := lc.unlockLink(t)
	id, err := lc.rec.Record(ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: lockedSlug, Payload: map[string]any{"tool": "recall"}})
	require.NoError(t, err)
	ev, ok, err := lc.rec.ByID(ctx, id)
	require.NoError(t, err)
	require.True(t, ok)

	streamCtx := func(unlock string) context.Context {
		req := lc.browser(http.MethodGet, "/console/events", unlock, nil)
		req = req.WithContext(access.WithView(req.Context(), lc.policy.OwnerView()))
		return lc.cs.withReach(req, credCookie).Context()
	}
	unlocked, locked := streamCtx(cookie), streamCtx("")
	require.True(t, lc.cs.liveEventVisible(unlocked, ev))
	require.False(t, lc.cs.liveEventVisible(locked, ev))
	require.False(t, reachOf(unlocked).until.IsZero(), "the stream ends with the unlock")

	live, err := store.LiveConsoleUnlocks(ctx, lc.db, store.HashConsoleToken(cookie), time.Now())
	require.NoError(t, err)
	_, err = store.EndConsoleUnlock(ctx, lc.db, live[0].ID, store.ConsoleUnlockEndLocked, time.Now())
	require.NoError(t, err)
	require.False(t, lc.cs.liveEventVisible(unlocked, ev), "locked again: the open stream stops passing it")
}

func TestUnlockHelpers(t *testing.T) {
	for d, want := range map[time.Duration]string{3 * time.Hour: "3h", 90 * time.Minute: "90m", 90 * time.Second: "1m30s"} {
		require.Equal(t, want, flagDuration(d))
	}
	require.Equal(t, "3 hours", leaseWord(3*time.Hour))
	require.Equal(t, "90 minutes", leaseWord(90*time.Minute))
	require.Equal(t, "_seamless", shellWord("_seamless"))
	require.Equal(t, `'/opt/my seamless/bin'`, shellWord("/opt/my seamless/bin"))
	require.Equal(t, `'it'\''s'`, shellWord("it's"))
	require.Equal(t, `''`, shellWord(""))
}

// The locked project's briefing preview says why it does not preview, in a
// locked browser and in one unlocked for it alike -- never "unknown project",
// which the board's locked row would contradict -- and shows nothing of it.
func TestBriefingPreview_LockedProjectSaysWhy(t *testing.T) {
	lc := newLockedConsole(t)
	unlock := lc.unlockLink(t)
	for _, cookie := range []string{"", unlock} {
		code, body := lc.get(t, "/console/settings/briefing/preview?project="+lockedSlug+"&format=json", cookie)
		require.Equal(t, http.StatusForbidden, code, "unlocked=%v", cookie != "")
		require.Contains(t, body, "project "+lockedSlug+" is locked: its briefing reaches an agent session only after")
		require.NotContains(t, body, "unknown project")
		require.NotContains(t, body, lockedSecret)
	}
	code, _ := lc.get(t, "/console/settings/briefing/preview?project="+openSlug+"&format=json", "")
	require.Equal(t, http.StatusOK, code, "an open project still previews")
}
