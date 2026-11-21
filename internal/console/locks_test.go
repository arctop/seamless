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

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// lkSeedLock declares an owner's lock on a checkout of app and returns it.
func lkSeedLock(t *testing.T, db *sql.DB, name, root string, state core.RepoLockState, projects ...string) core.RepoLock {
	t.Helper()
	aliases, err := store.RepoLockAliasesFor(store.CWDIdentity{Host: "example-laptop", RepoRoot: root,
		Origin: "git@github.com:example/app.git"}, "")
	require.NoError(t, err)
	out, err := store.DeclareRepoLock(context.Background(), db, store.RepoLockDeclaration{
		Name: name, Aliases: aliases, Projects: projects, State: state, RecordAliases: true,
	}, time.Now().UTC())
	require.NoError(t, err)
	return out.Lock
}

// lkSeedSession creates an active session in project and returns its id.
func lkSeedSession(t *testing.T, db *sql.DB, name, project string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

func lkPost(mux *http.ServeMux, path string, asJSON bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	return do(mux, req)
}

// lkConsole is a console with repo locks on (and research, as newConsole).
func lkConsole(t *testing.T) (*sql.DB, *http.ServeMux) {
	t.Helper()
	return newConsoleFeatures(t, config.Features{Research: true, RepoLocks: true})
}

func lkGet(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	rr := do(mux, authedReq(httptest.NewRequest(http.MethodGet, path, nil)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

func TestLocksPage_ListsLocksAndWaitingDeclarations(t *testing.T) {
	db, mux := lkConsole(t)
	ctx := context.Background()
	held := lkSeedLock(t, db, "main", "/work/app", core.RepoLockActive, "example-ops")
	sess := lkSeedSession(t, db, "cc/lk-holder", "example-ops")
	_, err := store.AcquireRepoLock(ctx, db, held.ID, sess, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	waiting := lkSeedLock(t, db, "release", "/work/app", core.RepoLockRequested)

	page := lkGet(t, mux, "/console/locks")
	require.Contains(t, page, "LOCK app main")
	require.Contains(t, page, `href="/console/sessions/`+sess+`">cc/lk-holder</a>`)
	require.Contains(t, page, `action="/console/locks/`+held.ID+`/release"`)
	require.Contains(t, page, "Waiting for your approval")
	require.Contains(t, page, `action="/console/locks/`+waiting.ID+`/approve"`)
	require.Contains(t, page, `action="/console/locks/`+waiting.ID+`/discard"`)
	require.Contains(t, page, "<code>github.com/example/app</code>")
	require.NotContains(t, page, `method="get"`, "the page's forms are POSTs the shared no-reload path takes")

	var d lkPageData
	getJSON(t, mux, "/console/locks", &d)
	require.Len(t, d.Locks, 1)
	require.Len(t, d.Requested, 1)
	require.Equal(t, "cc/lk-holder", d.Locks[0].HolderName)
	require.True(t, d.Locks[0].Held)
	require.Equal(t, []string{"example-ops"}, d.Locks[0].Projects)

	one := lkGet(t, mux, "/console/locks/"+held.ID)
	require.Contains(t, one, `id="lock-`+held.ID+`" data-lock="`+held.ID+`" aria-current="true"`)
	rr := do(mux, authedReq(httptest.NewRequest(http.MethodGet, "/console/locks/01NOSUCHLOCK", nil)))
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// Force-release, approve and discard: the owner's decisions, each refused
// where it does not apply, each recorded.
func TestLocksPage_OwnerDecisions(t *testing.T) {
	db, mux := lkConsole(t)
	ctx := context.Background()
	held := lkSeedLock(t, db, "main", "/work/app", core.RepoLockActive)
	sess := lkSeedSession(t, db, "cc/lk-holder", "example-ops")
	_, err := store.AcquireRepoLock(ctx, db, held.ID, sess, time.Minute, time.Now().UTC())
	require.NoError(t, err)

	rr := lkPost(mux, "/console/locks/"+held.ID+"/release", true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out struct {
		Lock         lkRow  `json:"lock"`
		PriorHolder  string `json:"prior_holder"`
		PriorSession string `json:"prior_holder_session"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, sess, out.PriorHolder)
	require.Equal(t, "cc/lk-holder", out.PriorSession)
	require.False(t, out.Lock.Held)
	l, ok, err := store.RepoLockByID(ctx, db, held.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "", l.Holder)
	rr = lkPost(mux, "/console/locks/"+held.ID+"/release", true)
	require.Equal(t, http.StatusConflict, rr.Code, "nothing to release")

	waiting := lkSeedLock(t, db, "release", "/work/app", core.RepoLockRequested)
	rr = lkPost(mux, "/console/locks/"+waiting.ID+"/approve", false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/locks?notice="+url.QueryEscape("Approved LOCK app release: agents may hold it now."), rr.Header().Get("Location"))
	l, _, err = store.RepoLockByID(ctx, db, waiting.ID)
	require.NoError(t, err)
	require.Equal(t, core.RepoLockActive, l.State)
	require.Equal(t, http.StatusConflict, lkPost(mux, "/console/locks/"+waiting.ID+"/approve", true).Code)
	require.Equal(t, http.StatusConflict, lkPost(mux, "/console/locks/"+waiting.ID+"/discard", true).Code, "an active lock is not discarded")

	other := lkSeedLock(t, db, "deploy", "/work/app", core.RepoLockRequested)
	rr = lkPost(mux, "/console/locks/"+other.ID+"/discard", true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	_, ok, err = store.RepoLockByID(ctx, db, other.ID)
	require.NoError(t, err)
	require.False(t, ok, "a discarded declaration is gone")

	require.Equal(t, http.StatusNotFound, lkPost(mux, "/console/locks/01NOSUCHLOCK/release", true).Code)

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind = ? AND json_extract(payload, '$.by') = 'console'`,
		string(core.EventRepoLockChanged)).Scan(&n))
	require.Equal(t, 3, n, "force_released, approved, discarded")
}

// The page reads through the request's handle: a holder in a project the
// handle hides is a session it does not show, and a hidden listed project is
// not listed. The lock itself, no project's, is shown and can be released.
func TestLocksPage_ReadsThroughTheFilteredHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	l := lkSeedLock(t, db, "main", "/work/fin", core.RepoLockActive, "example-finance", "example-ops")
	hiddenSess := lkSeedSession(t, db, "cc/lk-fin-agent", "example-finance")
	_, err = store.AcquireRepoLock(ctx, db, l.ID, hiddenSess, time.Minute, time.Now().UTC())
	require.NoError(t, err)
	// A waiter in the hidden project counts in the queue, and is not named.
	hiddenWaiter := lkSeedSession(t, db, "cc/lk-fin-waiter", "example-finance")
	_, err = store.AcquireRepoLockWaiting(ctx, db, l.ID, hiddenWaiter, time.Minute, time.Now().UTC(), true)
	require.NoError(t, err)

	view, err := store.OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	svc, err := New(Config{DB: view, WriteDB: db, Events: events.NewRecorder(db), APIKey: testKey,
		Retrieve: retrieve.New(view, nil, config.Defaults().Budgets, nil), Level: "advanced",
		Features: config.Features{RepoLocks: true}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	page := lkGet(t, mux, "/console/locks")
	require.Contains(t, page, "LOCK app main")
	require.Contains(t, page, "a session this console does not show")
	require.NotContains(t, page, "cc/lk-fin-agent")
	require.NotContains(t, page, hiddenSess)
	require.NotContains(t, page, "example-finance")
	require.Contains(t, page, `href="/console/projects/example-ops"`)
	require.Contains(t, page, "1 waiting, next a session this console does not show")
	require.NotContains(t, page, "cc/lk-fin-waiter")
	var d lkPageData
	getJSON(t, mux, "/console/locks", &d)
	require.Equal(t, 1, d.Locks[0].Waiting)
	require.True(t, d.Locks[0].NextWaiterHidden)
	require.Empty(t, d.Locks[0].NextWaiter, "a hidden waiter's id is not handed out")

	rr := lkPost(mux, "/console/locks/"+l.ID+"/release", true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NotContains(t, rr.Body.String(), hiddenSess, "the freed holder is not named either")
	got, _, err := store.RepoLockByID(ctx, db, l.ID)
	require.NoError(t, err)
	require.Equal(t, "", got.Holder, "the release landed on the write handle")
}

// The Locks screen is a Standard screen of the Work group, after Sessions,
// whose badge counts the declarations waiting for the owner. (The Diary leads
// the group, behind its own feature.)
func TestLocksScreen_InTheSidebarWithItsBadge(t *testing.T) {
	sc, ok := screenByID("locks")
	require.True(t, ok)
	require.Equal(t, groupWork, sc.Group)
	require.Equal(t, levelStandard, sc.Min)
	ids := []string{}
	for _, s := range screenRegistry() {
		if s.Group == groupWork {
			ids = append(ids, s.ID)
		}
	}
	require.Equal(t, []string{"diary", "projects", "plans", "tasks", "sessions", "locks"}, ids)
	n, found := navCounts{Locks: 2}.count("locks")
	require.True(t, found)
	require.Equal(t, 2, n)

	db, mux := lkConsole(t)
	lkSeedLock(t, db, "release", "/work/app", core.RepoLockRequested)
	page := lkGet(t, mux, "/console/settings")
	nav := regexpNav(page)
	require.Contains(t, nav, `href="/console/locks"`)
	require.Regexp(t, `href="/console/locks"[^>]*>.*?<span class="count"[^>]*>1</span>`, nav)

	_, basic := newConsoleLevel(t, config.Features{Research: true, RepoLocks: true}, "basic")
	require.NotContains(t, regexpNav(lkGet(t, basic, "/console/settings")), `href="/console/locks"`,
		"a Basic sidebar is as it was")
}

// With the feature off -- the default -- the screen is gone from the sidebar,
// every route answers the switched-off page (403 for a JSON caller), and no
// decision runs; turning it on restores them, the locks untouched.
func TestLocksPage_FollowsTheFeature(t *testing.T) {
	db, off := newConsoleFeatures(t, config.Features{Research: true})
	l := lkSeedLock(t, db, "main", "/work/app", core.RepoLockRequested)
	require.NotContains(t, regexpNav(lkGet(t, off, "/console/settings")), `href="/console/locks"`)
	for _, path := range []string{"/console/locks", "/console/locks/" + l.ID} {
		body := getPeek(t, off, path).Body.String()
		require.Contains(t, body, `data-feature-off="`+string(features.RepoLocks)+`"`, path)
		req := authedReq(httptest.NewRequest(http.MethodGet, path, nil))
		req.Header.Set("Accept", "application/json")
		require.Equal(t, http.StatusForbidden, do(off, req).Code, path)
	}
	for _, action := range []string{"approve", "discard", "release"} {
		require.Equal(t, http.StatusForbidden, lkPost(off, "/console/locks/"+l.ID+"/"+action, true).Code, action)
	}
	got, ok, err := store.RepoLockByID(context.Background(), db, l.ID)
	require.NoError(t, err)
	require.True(t, ok, "nothing ran while the feature was off")
	require.Equal(t, core.RepoLockRequested, got.State)

	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Research: true, RepoLocks: true}))
	require.Contains(t, lkGet(t, off, "/console/locks"), "LOCK app main", "turning it on restores the page live")
}

// regexpNav cuts the sidebar out of a page.
func regexpNav(page string) string {
	flat := strings.ReplaceAll(page, "\n", " ")
	start := strings.Index(flat, `<nav class="nav"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(flat[start:], "</nav>")
	if end < 0 {
		return flat[start:]
	}
	return flat[start : start+end]
}

// The Locks page shows a lock's queue -- how many sessions wait, and the next
// one -- and the restricted account a holding or waiting session belongs to,
// so the owner sees which account holds what.
func TestLocksPage_ShowsTheQueueAndTheAccount(t *testing.T) {
	db, mux := lkConsole(t)
	ctx := context.Background()
	l := lkSeedLock(t, db, "main", "/work/app", core.RepoLockActive, "example-ops")
	holder := lkSeedSession(t, db, "_example_agent:cc/lk-agent", "example-ops")
	_, err := db.ExecContext(ctx, `UPDATE sessions SET account = '_example_agent' WHERE id = ?`, holder)
	require.NoError(t, err)
	next := lkSeedSession(t, db, "cc/lk-next", "example-ops")
	third := lkSeedSession(t, db, "cc/lk-third", "example-ops")
	now := time.Now().UTC()
	_, err = store.AcquireRepoLock(ctx, db, l.ID, holder, time.Minute, now)
	require.NoError(t, err)
	for _, s := range []string{next, third} {
		_, err := store.AcquireRepoLockWaiting(ctx, db, l.ID, s, time.Minute, now, true)
		require.NoError(t, err)
	}

	page := lkGet(t, mux, "/console/locks")
	require.Contains(t, page, `>_example_agent:cc/lk-agent</a> (account _example_agent)`)
	require.Contains(t, page, `2 waiting, next <a href="/console/sessions/`+next+`">cc/lk-next</a>`)
	var d lkPageData
	getJSON(t, mux, "/console/locks", &d)
	require.Equal(t, "_example_agent", d.Locks[0].HolderAccount)
	require.Equal(t, 2, d.Locks[0].Waiting)
	require.Equal(t, next, d.Locks[0].NextWaiter)
	require.Equal(t, "cc/lk-next", d.Locks[0].NextWaiterName)
	require.Empty(t, d.Locks[0].NextWaiterAccount, "the owner's own session carries no account")

	// Nobody waiting, no queue words.
	free := lkSeedLock(t, db, "release", "/work/app", core.RepoLockActive)
	one := lkGet(t, mux, "/console/locks/"+free.ID)
	require.Equal(t, 1, strings.Count(one, "data-lock-queue"), "only the lock with a queue shows one")
}
