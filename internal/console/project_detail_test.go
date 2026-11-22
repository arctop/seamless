package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// insertRawMemory seeds a memory index row directly (the console test harness
// has no Files layer to index through), mirroring files.IndexMemory's columns.
func insertRawMemory(t *testing.T, db *sql.DB, id, project, sourceSession string, ts time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags,
		     valid_from, invalid_at, superseded_by, source_session,
		     content_hash, created_at, updated_at)
		VALUES (?, 'gotcha', ?, 'a seeded memory', ?, ?, '[]', ?, NULL, NULL, ?, '', ?, ?)`,
		id, id, project, id+".md", core.FormatTime(ts), sourceSession,
		core.FormatTime(ts), core.FormatTime(ts))
	require.NoError(t, err)
}

// getHTMLBody issues an authenticated browser GET (no JSON) and returns the body
// after asserting a 200 -- so a template execution error (which renders 500)
// fails the test.
func getHTMLBody(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s -> %d: %s", path, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// Every tab renders, each on a page of its own: the page draws the tab it
// shows and no other panel.
func TestProjectWorkspace_RendersAllTabs(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "seamless", Name: "Seamless", Description: "memory substrate",
		CreatedAt: now, UpdatedAt: now,
	}))
	// Session IDs are ULIDs in production (the relation guards require it).
	sessID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/s1", ProjectSlug: "seamless", Status: core.SessionActive,
		Findings: "building the tabbed workspace", CreatedAt: now, UpdatedAt: now,
	}))
	// A plan step the live session claims -> exercises the plan timeline and the
	// Sessions "holds" indicator. Context deliberately ignores execution state.
	taskID := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: taskID, ProjectSlug: "seamless", Title: "Build project detail",
		Status: core.TaskOpen, PlanSlug: "demo", CreatedAt: now, UpdatedAt: now,
	}))
	_, err := store.ClaimTask(ctx, db, taskID, sessID, 15*time.Minute, now)
	require.NoError(t, err)
	// A memory the session produced -> exercises the tree's memory leaf, the
	// Memories tab lineage cell, and memories-by-kind.
	insertRawMemory(t, db, mustID(t), "seamless", "cc/s1", now)

	for _, tab := range projectTabKeys {
		body := getHTMLBody(t, mux, "/console/projects/seamless?tab="+tab)
		require.Contains(t, body, `<div class="tabpanel on" data-panel="`+tab+`"`, "panel %q must render", tab)
		require.Equal(t, 1, strings.Count(body, `<div class="tabpanel`), "tab %q: one panel per page", tab)
		require.Contains(t, body, "root project", "the shared-briefing banner (no parent/children), on every tab")
	}
	body := getHTMLBody(t, mux, "/console/projects/seamless?tab=tasks")
	require.Contains(t, body, "plan:demo", "plan slug on the tasks panel")
	require.Contains(t, body, "Build project detail", "the plan step title")
	sessions := tabPanel(t, getHTMLBody(t, mux, "/console/projects/seamless?tab=sessions"), "sessions")
	require.Contains(t, sessions, "cc/s1", "the claiming session appears on the sessions panel")
	// The row's headline names the task first (sessionHeadlines), so find the
	// claim chip itself: it names the held task by title, linked.
	at := strings.Index(sessions, `<span class="claim"`)
	require.GreaterOrEqual(t, at, 0, "the held task's claim chip")
	claim := sessions[at:]
	claim = claim[:strings.Index(claim, "</a></span>")+len("</a></span>")]
	require.Contains(t, claim, `<a href="/console/tasks/`+taskID+`">Build project detail</a>`,
		"the session names the task it holds by title, linked to the task")
	require.Contains(t, claim, "ico-terminal", "with the step rows' holder icon")
	require.NotContains(t, sessions, "ico-lock", "a claim is never drawn as a lock")
}

func TestProjectWorkspace_DeepLinkTabAndFamilyChild(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, slug := range []string{"acme", "acme-ios"} {
		require.NoError(t, store.CreateProject(ctx, db, core.Project{
			ID: mustID(t), Slug: slug, Name: slug, CreatedAt: now, UpdatedAt: now,
		}))
	}
	require.NoError(t, store.SetProjectParent(ctx, db, "acme-ios", "acme", now))

	// A ?tab= deep-link marks that panel active server-side (no-JS + shareable).
	body := getHTMLBody(t, mux, "/console/projects/acme-ios?tab=context")
	require.Contains(t, body, `class="tabpanel on" data-panel="context"`)
	require.Contains(t, body, "What acme-ios receives and shares")
	require.Contains(t, body, "Parent memory")
}

func TestProjectWorkspace_LegacyRelationsTabRedirectsToContext(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "demo", Name: "demo", CreatedAt: now, UpdatedAt: now,
	}))
	req := httptest.NewRequest(http.MethodGet, "/console/projects/demo?tab=relations&w=7d", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusPermanentRedirect, rr.Code)
	require.Equal(t, "/console/projects/demo?tab=context&w=7d", rr.Header().Get("Location"))
}

func TestProjectWorkspace_BadTabIs400(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "seamless", Name: "seamless", CreatedAt: now, UpdatedAt: now,
	}))

	for _, path := range []string{
		"/console/projects/seamless?tab=bogus",
		"/console/projects/seamless?tab=",
		"/console/projects/seamless?tab=context&tab=overview",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code, "GET %s", path)
	}
}

func TestProjectWorkspace_UnknownSlug404(t *testing.T) {
	_, mux := newConsole(t)
	req := httptest.NewRequest(http.MethodGet, "/console/projects/nope", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestProjectDetail_PeekIsThinSummary(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "seamless", Name: "Seamless", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "s1", Name: "cc/s1", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))

	// The peek/CLI path returns the thin summary struct, not the workspace page.
	var d projectDetail
	getJSON(t, mux, "/console/projects/seamless?format=json", &d)
	require.Equal(t, "seamless", d.Slug)
	require.Equal(t, 1, d.Sessions)
}

// A bound stamp is the session's ULID: the Memories tab's lineage cell and the
// memory page name the session instead (P13, 2026-11-14: "from 01M7DHMQ..."
// where cc/71e9c68f-... was meant), and a stamp whose session is gone reads as
// stored.
func TestProjectWorkspace_LineageNamesTheSourceSession(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "seamless", Name: "Seamless", CreatedAt: now, UpdatedAt: now,
	}))
	sessID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/71e9c68f-bound", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	bound, gone, goneSess := mustID(t), mustID(t), mustID(t)
	insertRawMemory(t, db, bound, "seamless", sessID, now)
	insertRawMemory(t, db, gone, "seamless", goneSess, now)

	body := getHTMLBody(t, mux, "/console/projects/seamless?tab=memories")
	require.Contains(t, body, "from cc/71e9c68f-bound</span>")
	require.NotContains(t, body, "from "+sessID)
	require.Contains(t, body, "from "+goneSess+"</span>", "a session that is gone: the stamp as stored")

	page := getHTMLBody(t, mux, "/console/memories/"+bound)
	require.Contains(t, page, `<a href="/console/sessions/`+sessID+`">cc/71e9c68f-bound</a>`)
	require.NotContains(t, page, ">"+sessID+"<", "the ULID is the link, never the label")
}
