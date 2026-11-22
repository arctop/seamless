package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// findGroup returns the board group whose label starts with prefix.
func findGroup(t *testing.T, groups []projectGroupVM, prefix string) projectGroupVM {
	t.Helper()
	for _, g := range groups {
		if strings.HasPrefix(g.Label, prefix) {
			return g
		}
	}
	t.Fatalf("no group with label prefix %q in %v", prefix, groups)
	return projectGroupVM{}
}

func TestProjectsBoard_FamilyGrouping(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mkProj := func(slug string) {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateProject(ctx, db, core.Project{
			ID: id, Slug: slug, Name: slug, CreatedAt: now, UpdatedAt: now,
		}))
	}
	mkSess := func(id, name, project string, status core.SessionStatus, updated time.Time) {
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: status,
			CreatedAt: now, UpdatedAt: updated,
		}))
	}

	// A shared-briefing family (acme -> ios, web), a standalone, and a global
	// session so the "" scope has a row -- drawn as the shared-context line,
	// never as a band of its own.
	for _, slug := range []string{"acme", "acme-ios", "acme-web", "solo", "old-proj"} {
		mkProj(slug)
	}
	require.NoError(t, store.SetProjectParent(ctx, db, "acme-ios", "acme", now))
	require.NoError(t, store.SetProjectParent(ctx, db, "acme-web", "acme", now))
	require.NoError(t, store.RetireProject(ctx, db, "old-proj", now))

	// One live session in a child (drives Working / LiveSessions), one global.
	mkSess("s1", "cc/s1", "acme-ios", core.SessionActive, now)
	mkSess("s2", "cc/s2", "", core.SessionCompleted, now)

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&group=family", &data)

	require.Equal(t, "family", data.Group)
	require.Equal(t, "recent", data.Sort)
	require.Equal(t, 5, data.Projects, "acme, acme-ios, acme-web, solo, old-proj (global excluded)")
	require.Equal(t, 1, data.Working)
	require.Equal(t, 1, data.Families)

	require.Equal(t, "acme · shared briefing", data.Groups[0].Label, "no Root band leads the Family view")
	pbNoGlobalRow(t, data)
	require.NotNil(t, data.Global, "global is the shared-context line")

	fam := findGroup(t, data.Groups, "acme · shared briefing")
	require.Equal(t, 3, fam.Count, "parent + 2 children")
	require.Equal(t, "acme", fam.Rows[0].Slug)
	require.False(t, fam.Rows[0].Child)
	require.True(t, fam.Rows[1].Child)
	require.True(t, fam.Rows[2].Child)
	require.Contains(t, fam.Note, "2 children")

	solo := findGroup(t, data.Groups, "Standalone")
	require.Len(t, solo.Rows, 1)
	require.Equal(t, "solo", solo.Rows[0].Slug)

	ret := findGroup(t, data.Groups, "Retired")
	require.Len(t, ret.Rows, 1)
	require.Equal(t, "old-proj", ret.Rows[0].Slug)
	require.True(t, ret.Rows[0].Retired)
	require.True(t, ret.Folded, "the retired band folds")
	require.False(t, ret.Open)

	body := getHTMLBody(t, mux, "/console/projects?group=family")
	require.Contains(t, body, `id="p-acme-ios" class="project-card row-link child`, "a child is drawn under its parent")
	require.Contains(t, body, `<details class="project-group project-fold" id="pg-retired">`)
}

func TestProjectsBoard_FlatMode(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"a", "b"} {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateProject(ctx, db, core.Project{
			ID: id, Slug: slug, Name: slug, CreatedAt: now, UpdatedAt: now,
		}))
	}
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "s1", Name: "cc/s1", ProjectSlug: "a", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&group=flat&sort=name", &data)
	require.Equal(t, "flat", data.Group)
	require.Nil(t, data.Groups)
	require.Len(t, data.Flat, 2)
	require.Equal(t, "a", data.Flat[0].Slug) // sort=name
	require.Equal(t, "b", data.Flat[1].Slug)
}

func TestProjectsBoard_PortfolioSignalsAndRichCards(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "seamless", Name: "Seamless", Description: "shared agent context",
		CreatedAt: now, UpdatedAt: now,
	}))
	sessionID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessionID, Name: "cc/projects", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	_, err := db.ExecContext(ctx, `UPDATE sessions SET total_tokens = 12400 WHERE id = ?`, sessionID)
	require.NoError(t, err)
	memoryA, memoryB := mustID(t), mustID(t)
	insertMemoryRow(t, db, memoryA, "project-atlas", "seamless")
	insertMemoryRow(t, db, memoryB, "project-health", "seamless")

	recorder := events.NewRecorder(db)
	_, err = recorder.Record(ctx, core.Event{
		Kind: core.EventInjected, SessionID: sessionID, ProjectSlug: "seamless",
		Payload: map[string]any{"item_ids": []any{memoryA}},
	})
	require.NoError(t, err)

	blockerID := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: blockerID, ProjectSlug: "seamless", Title: "decide the layout", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: mustID(t), ProjectSlug: "seamless", Title: "ship the layout", Status: core.TaskOpen,
		DependsOn: []string{blockerID}, CreatedAt: now, UpdatedAt: now,
	}))
	stamp := core.FormatTime(now)
	_, err = db.ExecContext(ctx, `
		INSERT INTO notes_index
		    (id, title, slug, description, project, file_path, tags, content_hash, created_at, updated_at)
		VALUES (?, 'Project brief', 'project-brief', '', 'seamless', 'notes/seamless/project-brief.md', '[]', 'hash', ?, ?)`,
		mustID(t), stamp, stamp)
	require.NoError(t, err)

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&group=flat&w=all", &data)
	require.Equal(t, 1, data.Projects)
	require.Equal(t, 1, data.Working)
	require.Equal(t, 1, data.LiveScopes)
	require.Equal(t, 2, data.Memories)
	require.Equal(t, 1, data.Notes)
	require.Equal(t, 12400, data.TokensTotal)
	require.Equal(t, 2, data.OpenTasks)
	require.Equal(t, 1, data.Blocked)
	require.Equal(t, 1, data.Attention)
	require.Equal(t, 1, data.Surfaced)
	require.Equal(t, 2, data.Active)
	require.Equal(t, 50, data.ReachRate)
	require.Len(t, data.Flat, 1)
	require.Equal(t, 1, data.Flat[0].Notes)
	require.Equal(t, 1, data.Flat[0].Surfaced)
	require.Equal(t, 2, data.Flat[0].Active)
	require.Equal(t, 12400, data.Flat[0].TokensTotal)

	body := getHTMLBody(t, mux, "/console/projects?w=all")
	require.Contains(t, body, `class="project-summary-grid"`)
	require.Contains(t, body, "1 of 2 surfaced", "the reach card states its denominator")
	require.Contains(t, body, `class="project-card row-link working blocked"`)
	require.Contains(t, body, "1 / 2 surfaced")
	require.Contains(t, body, "1 blocked")
	require.Contains(t, body, "12.4k tokens")
	require.Contains(t, body, "Model tokens")
	require.Contains(t, body, "Open project")
	page := pbBoardHTML(t, body)
	require.NotContains(t, page, "workspace", "the board says project")
	require.NotContains(t, page, "scope", "the board says project")
}

// pbBoardHTML is the board's own markup in a rendered page, without the shell.
func pbBoardHTML(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<div class="ops-page" data-ops="projects"`)
	end := strings.Index(body, "</main>")
	require.True(t, start >= 0 && end > start, "the page draws the board")
	return body[start:end]
}

func TestProjectsBoard_BadParamsAre400(t *testing.T) {
	_, mux := newConsole(t)
	cases := []struct{ path, wants string }{
		{"/console/projects?group=bogus", "activity, family, flat"},
		{"/console/projects?sort=bogus", "recent, coverage, name"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Accept", "application/json")
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code, "GET %s", c.path)
		require.Contains(t, rr.Body.String(), c.wants, "GET %s must name the valid values", c.path)
	}
}

// pbNoGlobalRow fails when the global scope is a row anywhere on the board.
func pbNoGlobalRow(t *testing.T, data projectsData) {
	t.Helper()
	for _, g := range data.Groups {
		for _, row := range g.Rows {
			require.False(t, row.Global || row.Slug == "", "global is a row in %q", g.Label)
		}
	}
	for _, row := range data.Flat {
		require.False(t, row.Global || row.Slug == "", "global is a row in the flat view")
	}
}

// pbSlugs lists a band's rows by slug, in order.
func pbSlugs(rows []projectRowVM) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Slug)
	}
	return out
}

// pbProject registers slug and gives it one session updated at (zero: none)
// with the given status.
func pbProject(t *testing.T, db *sql.DB, slug string, status core.SessionStatus, at time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, slug, slug)
	require.NoError(t, err)
	if at.IsZero() {
		return
	}
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: mustID(t), Name: "cc/" + slug, ProjectSlug: slug, Status: status, CreatedAt: at, UpdatedAt: at,
	}))
}

// The default view groups by activity: Active (a live agent, or a session or
// task in 30 days), Quiet, Retired. Inside a band, Recent puts starred
// projects first, then live ones, then the most recent; another sort orders
// the band by its own key.
func TestPbActivityGroupingAndOrder(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := 24 * time.Hour

	pbProject(t, db, "p-star", core.SessionCompleted, now.Add(-20*day))
	require.NoError(t, store.SetProjectFavorite(ctx, db, "p-star", true))
	pbProject(t, db, "p-live", core.SessionActive, now)
	pbProject(t, db, "p-recent", core.SessionCompleted, now.Add(-day))
	pbProject(t, db, "p-old", core.SessionCompleted, now.Add(-10*day))
	pbProject(t, db, "p-quiet", core.SessionCompleted, now.Add(-40*day))
	pbProject(t, db, "p-empty", "", time.Time{})
	pbProject(t, db, "p-gone", core.SessionCompleted, now.Add(-day))
	require.NoError(t, store.RetireProject(ctx, db, "p-gone", now))
	// A task moved two days ago, with no session in the project: Active.
	pbProject(t, db, "p-task", "", time.Time{})
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: mustID(t), ProjectSlug: "p-task", Title: "moved elsewhere", Status: core.TaskOpen,
		CreatedAt: now.Add(-50 * day), UpdatedAt: now.Add(-2 * day),
	}))
	// A starred project that went quiet stays in Quiet: the star orders a band,
	// it does not move a project between bands.
	pbProject(t, db, "p-starquiet", core.SessionCompleted, now.Add(-60*day))
	require.NoError(t, store.SetProjectFavorite(ctx, db, "p-starquiet", true))

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json", &data)
	require.Equal(t, "activity", data.Group, "Activity is the default view")
	require.Equal(t, "recent", data.Sort)
	require.Len(t, data.Groups, 3)

	act := data.Groups[0]
	require.Equal(t, "active", act.Key)
	require.False(t, act.Folded)
	require.Equal(t, []string{"p-star", "p-live", "p-recent", "p-task", "p-old"}, pbSlugs(act.Rows),
		"starred first, then live, then the most recent")
	quiet := data.Groups[1]
	require.Equal(t, "quiet", quiet.Key)
	require.Equal(t, []string{"p-starquiet", "p-quiet", "p-empty"}, pbSlugs(quiet.Rows))
	require.True(t, quiet.Folded)
	require.False(t, quiet.Open, "Quiet stays folded with no filter")
	ret := data.Groups[2]
	require.Equal(t, "retired", ret.Key)
	require.Equal(t, []string{"p-gone"}, pbSlugs(ret.Rows))
	require.True(t, ret.Folded)
	require.False(t, ret.Open)

	getJSON(t, mux, "/console/projects?format=json&sort=name", &data)
	require.Equal(t, []string{"p-live", "p-old", "p-recent", "p-star", "p-task"}, pbSlugs(data.Groups[0].Rows),
		"the Sort menu still orders inside a band")
	getJSON(t, mux, "/console/projects?format=json&sort=favorites", &data)
	require.Equal(t, []string{"p-star", "p-live", "p-recent", "p-task", "p-old"}, pbSlugs(data.Groups[0].Rows))

	// The Flat view keeps Recent as pure recency.
	getJSON(t, mux, "/console/projects?format=json&group=flat", &data)
	require.Equal(t, "p-live", data.Flat[0].Slug)

	body := getHTMLBody(t, mux, "/console/projects")
	require.Contains(t, body, `<section class="project-group" id="pg-active">`)
	require.Contains(t, body, `<details class="project-group project-fold" id="pg-quiet">`)
	require.Contains(t, body, "<strong>Quiet</strong>")
	require.Contains(t, body, "3 projects")
	require.Less(t, strings.Index(body, `id="p-p-star"`), strings.Index(body, `id="p-p-live"`))
	require.Contains(t, body, `class="on" href="/console/projects?group=activity&amp;sort=recent&amp;q=&amp;w=24h">Activity</a>`)
	require.Contains(t, body, "Projects by activity")
	require.NotContains(t, body, "Project topology")
	require.NotContains(t, body, "Scopes grouped")
}

// With nothing Active, Quiet stands open: there is nothing above it to keep in
// view.
func TestPbQuietOpensWhenNothingIsActive(t *testing.T) {
	db, mux := newConsole(t)
	pbProject(t, db, "p-a", "", time.Time{})
	pbProject(t, db, "p-b", core.SessionCompleted, time.Now().UTC().Add(-90*24*time.Hour))

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json", &data)
	require.Len(t, data.Groups, 1)
	require.Equal(t, "quiet", data.Groups[0].Key)
	require.True(t, data.Groups[0].Open)
	require.Contains(t, getHTMLBody(t, mux, "/console/projects"), `id="pg-quiet" open>`)
}

// A filter that matches a project inside the Quiet fold opens it, on a page
// load and on the in-place load the filter form makes (the patch the morph
// applies: it adds the open attribute and never takes one away).
func TestPbQuietFoldOpensForAMatchingFilter(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	pbProject(t, db, "p-busy", core.SessionCompleted, now.Add(-time.Hour))
	pbProject(t, db, "p-sleepy", core.SessionCompleted, now.Add(-45*24*time.Hour))
	pbProject(t, db, "p-gone", core.SessionCompleted, now.Add(-45*24*time.Hour))
	require.NoError(t, store.RetireProject(context.Background(), db, "p-gone", now))

	var data projectsData
	getJSON(t, mux, "/console/projects?format=json", &data)
	require.Equal(t, "quiet", data.Groups[1].Key)
	require.False(t, data.Groups[1].Open)
	require.False(t, data.Groups[2].Open)

	getJSON(t, mux, "/console/projects?format=json&q=sleepy", &data)
	require.Len(t, data.Groups, 1, "the filter keeps only the matching project")
	require.Equal(t, "quiet", data.Groups[0].Key)
	require.True(t, data.Groups[0].Open, "a match inside Quiet opens it")
	getJSON(t, mux, "/console/projects?format=json&q=gone", &data)
	require.Equal(t, "retired", data.Groups[0].Key)
	require.True(t, data.Groups[0].Open, "a match inside Retired opens it")

	const open = `<details class="project-group project-fold" id="pg-quiet" open>`
	require.Contains(t, getHTMLBody(t, mux, "/console/projects?q=sleepy"), open)
	req := httptest.NewRequest(http.MethodGet, "/console/projects?q=sleepy&group=activity&sort=recent&w=24h", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("X-Seam-Partial", "1")
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), open, "the in-place load carries the open fold")
	require.NotContains(t, getHTMLBody(t, mux, "/console/projects?q=busy"), "pg-quiet", "no Quiet band when nothing in it matches")
}

// Global is not a project: no view draws it as a row, and every view draws it
// as the shared-context line under the tiles, linking to the Context page.
func TestPbGlobalIsTheSharedContextLine(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pbProject(t, db, "p-a", core.SessionCompleted, now)
	pbProject(t, db, "p-b", core.SessionCompleted, now.Add(-40*24*time.Hour))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: mustID(t), Name: "cc/global", ProjectSlug: "", Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now,
	}))
	insertMemoryRow(t, db, mustID(t), "global-conv", "")
	insertMemoryRow(t, db, mustID(t), "global-conv-2", "")
	stamp := core.FormatTime(now)
	_, err := db.ExecContext(ctx, `
		INSERT INTO notes_index (id, title, slug, description, project, file_path, tags, content_hash, created_at, updated_at)
		VALUES (?, 'Shared', 'shared', '', '', 'notes/_global/shared.md', '[]', 'h', ?, ?)`, mustID(t), stamp, stamp)
	require.NoError(t, err)

	for _, view := range []string{"activity", "family", "flat"} {
		var data projectsData
		getJSON(t, mux, "/console/projects?format=json&w=all&sort=name&group="+view, &data)
		pbNoGlobalRow(t, data)
		require.Equal(t, 2, data.Projects, view)
		require.NotNil(t, data.Global, view)
		require.Equal(t, 2, data.Global.Memories)
		require.Equal(t, 1, data.Global.Notes)
		require.True(t, strings.HasPrefix(data.Global.Line, "2 memories, 1 note"), data.Global.Line)

		body := getHTMLBody(t, mux, "/console/projects?group="+view)
		require.Contains(t, body, `<a class="project-global-line" href="/console/context"`, view)
		require.Contains(t, body, "<strong>Shared context:</strong> 2 memories, 1 note", view)
		require.NotContains(t, body, "Global context", "%s draws no global card", view)
		require.NotContains(t, body, "shared root", view)
		require.Less(t, strings.Index(body, `class="project-summary-grid"`), strings.Index(body, "project-global-line"),
			"the line sits under the summary tiles")
	}

	// A filter narrows the rows, never the shared-context line.
	var data projectsData
	getJSON(t, mux, "/console/projects?format=json&q=p-a", &data)
	require.NotNil(t, data.Global)
}

// A locked project is a row in the same bands as every other, sorted by its
// last session start or end -- a live session by its start, never by a
// heartbeat -- and it still shows its name, counts and that time only.
func TestPbLockedRowSortsAmongTheOthers(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	day := 24 * time.Hour
	set := func(q string, args ...any) {
		_, err := lc.db.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	// The locked project's live session started five days ago and
	// heartbeated a minute ago; another of its sessions ended three days ago.
	set(`UPDATE sessions SET created_at = ?, updated_at = ? WHERE id = ?`,
		core.FormatTime(now.Add(-5*day)), core.FormatTime(now.Add(-time.Minute)), lc.sessID)
	require.NoError(t, store.CreateSession(ctx, lc.db, core.Session{
		ID: mustID(t), Name: "cc/ledger-close", ProjectSlug: lockedSlug, Status: core.SessionCompleted,
		CreatedAt: now.Add(-4 * day), UpdatedAt: now.Add(-3 * day),
	}))
	pbProject(t, lc.db, "example-web", core.SessionCompleted, now.Add(-6*day))
	pbProject(t, lc.db, "example-quiet", core.SessionCompleted, now.Add(-40*day))
	require.NoError(t, store.CreateSession(ctx, lc.db, core.Session{
		ID: mustID(t), Name: "cc/ops", ProjectSlug: openSlug, Status: core.SessionCompleted,
		CreatedAt: now.Add(-day), UpdatedAt: now.Add(-day),
	}))

	get := func(target string) string {
		rr := lc.do(lc.browser(http.MethodGet, target, "", nil))
		require.Equal(t, http.StatusOK, rr.Code, target)
		return rr.Body.String()
	}
	var data projectsData
	require.NoError(t, json.Unmarshal([]byte(get("/console/projects?format=json")), &data))
	require.Equal(t, "active", data.Groups[0].Key)
	require.Equal(t, []string{openSlug, lockedSlug, "example-web"}, pbSlugs(data.Groups[0].Rows),
		"the locked row sorts by its last session end, between the open ones")
	row := data.Groups[0].Rows[1]
	require.True(t, row.Locked)
	require.WithinDuration(t, now.Add(-3*day), row.LastActive, time.Second, "its latest start or end, not the heartbeat")
	require.Equal(t, projectRowVM{Slug: lockedSlug, Name: lockedSlug, Locked: true, TileIcon: "lock",
		Sessions: 2, OpenTasks: 1, Memories: 1, Notes: 1, LastActive: row.LastActive}, row,
		"name, counts and last activity: nothing else is read for a locked row")
	require.Equal(t, []string{"example-quiet"}, pbSlugs(data.Groups[1].Rows))
	require.Len(t, data.Locked, 1, "the tallies stay listed for a script")
	require.Equal(t, 4, data.Projects, "a locked row counts as a project")

	require.NoError(t, json.Unmarshal([]byte(get("/console/projects?format=json&group=flat")), &data))
	require.Equal(t, []string{openSlug, lockedSlug, "example-web", "example-quiet"}, pbSlugs(data.Flat))
	require.NoError(t, json.Unmarshal([]byte(get("/console/projects?format=json&group=family")), &data))
	require.Equal(t, []string{openSlug, lockedSlug, "example-web", "example-quiet"}, pbSlugs(findGroup(t, data.Groups, "Standalone").Rows))

	for _, target := range []string{"/console/projects", "/console/projects?group=flat", "/console/projects?group=family",
		"/console/projects?format=json", "/console/projects?q=ledger"} {
		body := get(target)
		for _, secret := range []string{lockedSecret, "ledger-close", "cc/ledger", "reconcile", lc.memID, lc.noteID, lc.taskID, lc.sessID} {
			require.NotContains(t, body, secret, "%s shows a locked item", target)
		}
	}
	html := get("/console/projects")
	require.Contains(t, html, `<article id="p-`+lockedSlug+`" class="project-card row-link project-locked" data-href="/console/unlock/`+lockedSlug+`">`)
	require.Contains(t, html, `href="/console/unlock/`+lockedSlug+`"`)
	require.Contains(t, html, "Unlock")
	require.Less(t, strings.Index(html, `id="p-`+openSlug+`"`), strings.Index(html, `id="p-`+lockedSlug+`"`))
	require.Less(t, strings.Index(html, `id="p-`+lockedSlug+`"`), strings.Index(html, `id="p-example-web"`))
	require.NotContains(t, html, "Name and counts only; unlock one", "no band of its own after every group")
}

// The remembered view: a few lines of the page's own script, outside the view
// (a morph never replaces them), on the shared no-reload path. Storage access
// is wrapped, and a patch, which carries no scripts, never repeats them.
func TestPbRemembersTheViewInPlace(t *testing.T) {
	_, mux := newConsole(t)
	body := getHTMLBody(t, mux, "/console/projects")
	require.Contains(t, body, `data-ops="projects" data-board-group="activity" data-board-sort="recent"`)
	start := strings.Index(body, "seamless-projects-view")
	require.Positive(t, start, "the full page carries the script")
	script := body[strings.LastIndex(body[:start], "<script>"):]
	script = script[:strings.Index(script, "</script>")]
	require.Contains(t, script, "try { return localStorage.getItem(key); } catch (e) { return null; }")
	require.Contains(t, script, "try { localStorage.setItem(key, value); } catch (e) {}")
	require.Contains(t, script, "window.SeamConsole.load(url.href, { history: 'replace', source: 'query' })")
	require.Contains(t, script, "if (url.searchParams.has(p)) return;", "a URL that names a view or sort wins")
	require.Contains(t, script, "document.addEventListener('seam:content-updated'")
	require.NotContains(t, script, "location.reload(")
	require.NotContains(t, script, "location.href =")
	require.NotContains(t, script, "location.assign(")

	req := httptest.NewRequest(http.MethodGet, "/console/projects", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("X-Seam-Live", "1")
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `data-board-group="activity"`, "the live refresh keeps the view's name current")
	require.NotContains(t, rr.Body.String(), "seamless-projects-view", "a patch carries no script")
}
