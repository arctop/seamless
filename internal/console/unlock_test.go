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

	restarts int // RequestRestart calls
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

	lc.policy, err = access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{lockedSlug}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	views, err := store.OpenViews(path, lc.policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	rec := events.NewRecorder(db)
	lc.svc = approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}, Events: rec, ForceLocked: []string{lockedSlug}})
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	cs, err := New(Config{
		DB: base, WriteDB: db, Files: mgr, Events: rec.ReadingFrom(base), Retrieve: ret, DataDir: filepath.Join(dir, "data"),
		APIKey: testKey, Level: "advanced", Approvals: lc.svc, Views: views, ForceLocked: []string{lockedSlug},
		RequestRestart: func() { lc.restarts++ },
		Terminal:       &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"},
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

// terminalBegun is POST /console/unlock/<project>/terminal's answer.
type terminalBegun struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	Command      string `json:"command"`
	PendingUntil string `json:"pending_until"`
	Minutes      int    `json:"minutes"`
}

// beginTerminal begins a terminal unlock of project in the browser holding
// unlock ("" for a fresh one), and returns the answer and the browser's
// cookie: the one it sent, or the one the begin set.
func (lc *lockedConsole) beginTerminal(t *testing.T, project, unlock string) (terminalBegun, string) {
	t.Helper()
	rr := lc.do(lc.browser(http.MethodPost, "/console/unlock/"+project+"/terminal", unlock, url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var begun terminalBegun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
	cookie := unlock
	for _, c := range rr.Result().Cookies() {
		if c.Name == unlockCookieName {
			require.True(t, c.HttpOnly)
			require.Equal(t, http.SameSiteStrictMode, c.SameSite)
			require.Equal(t, "/console", c.Path)
			require.Positive(t, c.MaxAge)
			cookie = c.Value
		}
	}
	require.NotEmpty(t, cookie, "the begin issues a fresh browser its cookie")
	return begun, cookie
}

// terminalState asks the status endpoint after unlock id as the browser
// holding unlock, and returns the HTTP status and the state it names.
func (lc *lockedConsole) terminalState(t *testing.T, project, id, unlock string) (int, string) {
	t.Helper()
	req := lc.browser(http.MethodGet, "/console/unlock/"+project+"/terminal?id="+url.QueryEscape(id), unlock, nil)
	req.Header.Set("Accept", "application/json")
	rr := lc.do(req)
	var got struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got), rr.Body.String())
	return rr.Code, got.State
}

// approveCode is the terminal's half, as console-open --unlock --code runs it.
func (lc *lockedConsole) approveCode(t *testing.T, project, code string) store.ConsoleUnlock {
	t.Helper()
	u, err := lc.svc.ApproveTerminalUnlock(context.Background(), project, code, 3*time.Hour)
	require.NoError(t, err)
	return u
}

// unlockByTerminal unlocks the locked project for a fresh browser the way the
// terminal path does -- the browser begins it, the daemon's account approves
// its code -- and returns that browser's cookie value.
func (lc *lockedConsole) unlockByTerminal(t *testing.T) string {
	t.Helper()
	begun, cookie := lc.beginTerminal(t, lockedSlug, "")
	lc.approveCode(t, lockedSlug, begun.Code)
	status, state := lc.terminalState(t, lockedSlug, begun.ID, cookie)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, terminalLive, state)
	return cookie
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
		if p == "/console/events" || strings.HasPrefix(p, "/console/static/") || p == "/console/login" {
			continue // the live feed streams; statics and login are public
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
	require.Contains(t, page, `id="unlock-terminal-form"`, "the terminal path is offered with no passkey enrolled")
	require.NotContains(t, page, "console-open --unlock", "no command until this browser has begun an unlock and has its code")
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
	cookie := lc.unlockByTerminal(t)

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
	cookie := lc.unlockByTerminal(t)
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
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")

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

// The terminal path: the browser begins it and shows a code and the command;
// the daemon's account approves the code; the page, asking the status
// endpoint, sees it live. Only that browser reads the project.
func TestLockedConsole_TerminalUnlock(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()

	_, page := lc.get(t, "/console/unlock/"+lockedSlug, "")
	require.Contains(t, page, `id="unlock-terminal-form"`)
	require.Contains(t, page, "Approve from a terminal")
	require.Contains(t, page, "a code works for 10 minutes")
	bearerPage := lc.do(authedReq(httptest.NewRequest(http.MethodGet, "/console/unlock/"+lockedSlug, nil)))
	require.Equal(t, http.StatusOK, bearerPage.Code)
	require.NotContains(t, bearerPage.Body.String(), `id="unlock-terminal-form"`, "only a browser is offered an unlock")

	begun, cookie := lc.beginTerminal(t, lockedSlug, "")
	require.Equal(t, begun.Code, store.FormatUnlockCode(begun.Code), "XXXX-XXXX")
	require.Equal(t, "sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd "+
		"console-open --unlock "+lockedSlug+" --code "+begun.Code, begun.Command)
	require.Equal(t, 10, begun.Minutes)
	_, err := time.Parse(time.RFC3339, begun.PendingUntil)
	require.NoError(t, err)

	status, state := lc.terminalState(t, lockedSlug, begun.ID, cookie)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, terminalPending, state)
	_, body := lc.get(t, "/console/memories?project="+lockedSlug, cookie)
	require.NotContains(t, body, lockedSecret, "waiting opens nothing")

	// Nobody but the beginning browser learns the row's state: another
	// browser, no cookie, an unknown id and another project all get the
	// answer an id that never existed gets; the TCP key is refused.
	other, err := store.NewConsoleToken()
	require.NoError(t, err)
	for name, c := range map[string]struct{ project, id, cookie string }{
		"another browser": {lockedSlug, begun.ID, other},
		"no cookie":       {lockedSlug, begun.ID, ""},
		"unknown id":      {lockedSlug, "01NOSUCHTHING", cookie},
	} {
		status, state := lc.terminalState(t, c.project, c.id, c.cookie)
		require.Equal(t, http.StatusNotFound, status, name)
		require.Equal(t, terminalExpired, state, name)
	}
	bearer := authedReq(httptest.NewRequest(http.MethodGet, "/console/unlock/"+lockedSlug+"/terminal?id="+begun.ID, nil))
	bearer.AddCookie(&http.Cookie{Name: unlockCookieName, Value: cookie})
	require.Equal(t, http.StatusForbidden, lc.do(bearer).Code, "the TCP key reads no browser's unlock, even holding its cookie")

	u := lc.approveCode(t, lockedSlug, strings.ToLower(begun.Code))
	require.Equal(t, begun.ID, u.ID)
	req := lc.browser(http.MethodGet, "/console/unlock/"+lockedSlug+"/terminal?id="+begun.ID, cookie, nil)
	req.Header.Set("Accept", "application/json")
	rr := lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"state": "live"`)
	require.Contains(t, rr.Body.String(), `"open": "/console/projects/`+lockedSlug+`"`)
	reissued := false
	for _, c := range rr.Result().Cookies() {
		reissued = reissued || (c.Name == unlockCookieName && c.Value == cookie && c.MaxAge > 0)
	}
	require.True(t, reissued, "live: the cookie lasts as long as the unlock")
	_, body = lc.get(t, "/console/memories?project="+lockedSlug, cookie)
	require.Contains(t, body, lockedSecret, "the beginning browser reads the project")
	_, body = lc.get(t, "/console/memories?project="+lockedSlug, other)
	require.NotContains(t, body, lockedSecret, "and only that browser")
	_, err = lc.svc.ApproveTerminalUnlock(ctx, lockedSlug, begun.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "a code works once")

	// A second unlock in the same browser keeps its cookie.
	rr = lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/terminal", cookie, url.Values{}))
	require.Equal(t, http.StatusOK, rr.Code)
	for _, c := range rr.Result().Cookies() {
		require.NotEqual(t, unlockCookieName, c.Name, "a browser with a live unlock keeps its cookie")
	}
	evs, err := events.NewRecorder(lc.db).ByKinds(ctx, []core.EventKind{approval.EventConsoleUnlocked}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1, "the approval is recorded once")
	require.Equal(t, approval.ProofDaemon, evs[0].Payload["proof"])

	// Expired before approval: the page is told so.
	late, lateCookie := lc.beginTerminal(t, lockedSlug, "")
	_, err = lc.db.Exec(`UPDATE console_unlocks SET pending_until = ? WHERE id = ?`, core.FormatTime(time.Now().Add(-time.Second)), late.ID)
	require.NoError(t, err)
	status, state = lc.terminalState(t, lockedSlug, late.ID, lateCookie)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, terminalExpired, state)
	_, err = lc.svc.ApproveTerminalUnlock(ctx, lockedSlug, late.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "an expired code fails")
	// Locked again: ended.
	live, err := store.LiveConsoleUnlocks(ctx, lc.db, store.HashConsoleToken(cookie), time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	_, err = store.EndConsoleUnlock(ctx, lc.db, live[0].ID, store.ConsoleUnlockEndLocked, time.Now())
	require.NoError(t, err)
	status, state = lc.terminalState(t, lockedSlug, begun.ID, cookie)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, terminalEnded, state)
}

// The terminal begin refuses what the passkey begin refuses: the TCP key, an
// agent's socket, a restricted account, a cross-origin or unlabeled write, a
// stranger, and a project that is not locked. None of them writes a row.
func TestLockedConsole_TerminalBeginGates(t *testing.T) {
	lc := newLockedConsole(t)
	target := "/console/unlock/" + lockedSlug + "/terminal"
	post := func(r *http.Request) *http.Request {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", "application/json")
		return r
	}
	sockView, ok := lc.policy.PeerChainView(501, []access.ProcessToken{{PID: 300, Version: 3000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}})
	require.True(t, ok)
	sock := post(httptest.NewRequest(http.MethodPost, target, strings.NewReader("")))
	sock = sock.WithContext(access.WithView(sock.Context(), sockView))
	restrictedPolicy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{lockedSlug}, Restricted: []string{"_helper"}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_helper":
				return access.Account{UID: 503, Name: "_helper"}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	rview, ok := restrictedPolicy.PeerChainView(503, nil)
	require.True(t, ok)
	restricted := post(httptest.NewRequest(http.MethodPost, target, strings.NewReader("")))
	restricted = restricted.WithContext(access.WithView(restricted.Context(), rview))
	crossSite := lc.browser(http.MethodPost, target, "", url.Values{})
	crossSite.Header.Set("Sec-Fetch-Site", "same-site") // another local port
	unlabeled := lc.browser(http.MethodPost, target, "", url.Values{})
	unlabeled.Header.Del("Sec-Fetch-Site")
	stranger := post(httptest.NewRequest(http.MethodPost, target, strings.NewReader("")))

	for name, c := range map[string]struct {
		req  *http.Request
		want int
	}{
		"the TCP key":            {post(authedReq(httptest.NewRequest(http.MethodPost, target, strings.NewReader("")))), http.StatusForbidden},
		"an agent's socket":      {sock, http.StatusForbidden},
		"a restricted account":   {restricted, http.StatusForbidden},
		"another local port":     {crossSite, http.StatusForbidden},
		"no origin header":       {unlabeled, http.StatusForbidden},
		"no credential":          {stranger, http.StatusUnauthorized},
		"a project not locked":   {lc.browser(http.MethodPost, "/console/unlock/"+openSlug+"/terminal", "", url.Values{}), http.StatusNotFound},
		"an unknown project":     {lc.browser(http.MethodPost, "/console/unlock/no-such/terminal", "", url.Values{}), http.StatusNotFound},
		"the status, no session": {httptest.NewRequest(http.MethodGet, target+"?id=x", nil), http.StatusSeeOther},
	} {
		rr := lc.do(c.req)
		require.Equal(t, c.want, rr.Code, "%s: %s", name, rr.Body.String())
		for _, ck := range rr.Result().Cookies() {
			require.NotEqual(t, unlockCookieName, ck.Name, "%s: no unlock cookie", name)
		}
	}
	var n int
	require.NoError(t, lc.db.QueryRow(`SELECT COUNT(*) FROM console_unlocks`).Scan(&n))
	require.Zero(t, n, "no refused begin wrote a row")
}

// A key holder looping on one begin route fills that kind's pending cap and
// is then refused, while the other way in stays open: a full terminal cap
// still lets the owner's passkey ceremony through, and a full passkey cap the
// owner's terminal unlock.
func TestLockedConsole_TerminalBeginCapped(t *testing.T) {
	const (
		terminal = "/terminal"
		passkey  = "/begin"
	)
	begin := func(lc *lockedConsole, route string) *httptest.ResponseRecorder {
		return lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+route, "", url.Values{}))
	}
	for name, c := range map[string]struct{ fill, other string }{
		"terminal begins full": {terminal, passkey},
		"passkey begins full":  {passkey, terminal},
	} {
		t.Run(name, func(t *testing.T) {
			lc := newLockedConsole(t)
			key, err := passkeytest.New(true)
			require.NoError(t, err)
			enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")
			for range 32 {
				rr := begin(lc, c.fill)
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			}
			rr := begin(lc, c.fill)
			require.Equal(t, http.StatusConflict, rr.Code)
			require.Contains(t, rr.Body.String(), "too many unlocks are in progress")

			rr = begin(lc, c.other)
			require.Equal(t, http.StatusOK, rr.Code, "the other way in has its own cap: %s", rr.Body.String())
			cookie := ""
			for _, ck := range rr.Result().Cookies() {
				if ck.Name == unlockCookieName {
					cookie = ck.Value
				}
			}
			require.NotEmpty(t, cookie)
			if c.other == terminal {
				// And it carries through to a live unlock.
				var begun terminalBegun
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
				lc.approveCode(t, lockedSlug, begun.Code)
			} else {
				var started approval.Begun
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &started))
				assertion, err := key.Get(started.Options, approverOrigin)
				require.NoError(t, err)
				rr = lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+"/finish", cookie,
					url.Values{"id": {started.ID}, "response": {string(assertion)}}))
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			}
			_, body := lc.get(t, "/console/memories?project="+lockedSlug, cookie)
			require.Contains(t, body, lockedSecret, "the owner unlocks past the full cap")
		})
	}
}

// The security review's two attacks on the unlock link, against the terminal
// path: a party holding the console's TCP key and everything the terminal
// shows -- the code, the command, the result -- but not the beginning
// browser's cookie gets no live unlock, cannot read the row's state, and
// cannot move the approval onto a cookie of its own.
func TestLockedConsole_TerminalCodeOpensOnlyTheBeginningBrowser(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	owner, ownerCookie := lc.beginTerminal(t, lockedSlug, "")

	// The agent's browser: the console cookie is derived from the key, so an
	// agent holding the key is "a browser" to the console. It plants an
	// unlock cookie of its own choosing first.
	planted, err := store.NewConsoleToken()
	require.NoError(t, err)
	theirs, agentCookie := lc.beginTerminal(t, lockedSlug, planted)
	require.NotEqual(t, planted, agentCookie, "a cookie naming no live unlock is never adopted")
	require.NotEqual(t, owner.Code, theirs.Code, "the code is the server's, never the caller's")

	// It cannot read the owner's row, by id or with its own or planted cookie.
	for _, c := range []string{agentCookie, planted, ""} {
		status, _ := lc.terminalState(t, lockedSlug, owner.ID, c)
		require.Equal(t, http.StatusNotFound, status)
	}
	// It sends the owner's code to every unlock route a browser has.
	for _, p := range []string{"/terminal", "/begin", "/finish"} {
		rr := lc.do(lc.browser(http.MethodPost, "/console/unlock/"+lockedSlug+p, agentCookie,
			url.Values{"code": {owner.Code}, "id": {owner.ID}, "response": {"{}"}}))
		require.Less(t, rr.Code, 500, p)
	}

	// The owner approves the code the owner's page shows.
	u := lc.approveCode(t, lockedSlug, owner.Code)
	require.Equal(t, store.HashConsoleToken(ownerCookie), u.TokenHash, "the approval opens the browser that began it")

	for _, c := range []string{agentCookie, planted} {
		live, err := store.LiveConsoleUnlocks(ctx, lc.db, store.HashConsoleToken(c), time.Now())
		require.NoError(t, err)
		require.Empty(t, live, "the agent's cookies hold no unlock")
		_, body := lc.get(t, "/console/memories?project="+lockedSlug, c)
		require.NotContains(t, body, lockedSecret)
		rr := lc.do(func() *http.Request {
			r := lc.browser(http.MethodGet, "/console/projects/"+lockedSlug, c, nil)
			r.Header.Set("Accept", "application/json")
			return r
		}())
		require.Equal(t, http.StatusForbidden, rr.Code)
		require.Contains(t, rr.Body.String(), `"locked": true`)
	}
	// Replaying the code, from the terminal again, opens nothing more.
	_, err = lc.svc.ApproveTerminalUnlock(ctx, lockedSlug, owner.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound)
	// Its own row still waits for its own code, which only the owner's
	// terminal can approve.
	w, err := store.ConsoleUnlockFor(ctx, lc.db, theirs.ID, lockedSlug, store.HashConsoleToken(agentCookie))
	require.NoError(t, err)
	require.Equal(t, store.ConsoleUnlockPending, w.Status)
	all, err := store.AllLiveConsoleUnlocks(ctx, lc.db, time.Now())
	require.NoError(t, err)
	require.Len(t, all, 1, "one approval, one live unlock")
	_, body := lc.get(t, "/console/memories?project="+lockedSlug, ownerCookie)
	require.Contains(t, body, lockedSecret, "the owner's browser reads it")
}

// A code approves only its own project: one begun for the locked project
// cannot unlock another, even one this browser could begin.
func TestLockedConsole_TerminalCodeIsPerProject(t *testing.T) {
	lc := newLockedConsole(t)
	begun, cookie := lc.beginTerminal(t, lockedSlug, "")
	_, err := lc.svc.ApproveTerminalUnlock(context.Background(), openSlug, begun.Code, time.Hour)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound)
	status, state := lc.terminalState(t, lockedSlug, begun.ID, cookie)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, terminalPending, state, "a refused approval changes nothing")
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
	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	begun, code, err := store.BeginTerminalUnlock(ctx, db, "example-ops", store.HashConsoleToken(cookie), time.Hour, time.Minute, time.Now())
	require.NoError(t, err)
	_, err = store.ApproveTerminalUnlock(ctx, db, "example-ops", code, time.Hour, time.Now())
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
	for _, target := range []string{"/console/unlock/example-ops", "/console/unlock/example-ops/terminal?id=" + begun.ID} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(consoleCookie())
		req.AddCookie(&http.Cookie{Name: unlockCookieName, Value: cookie})
		require.Equal(t, http.StatusNotFound, do(mux, req).Code, target)
	}
	for _, target := range []string{"/console/unlock/example-ops/begin", "/console/unlock/example-ops/terminal",
		"/console/access/unlocks/x/end", "/console/access/leases/x/revoke"} {
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
	cookie := lc.unlockByTerminal(t)
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
	unlock := lc.unlockByTerminal(t)
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
