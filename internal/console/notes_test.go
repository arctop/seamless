package console

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

// libNote writes a note with explicit times and tags.
func libNote(t *testing.T, mgr *files.Manager, project, slug, title string, at time.Time, tags []string) core.Note {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	n, err := mgr.WriteNote(context.Background(), core.Note{
		ID: id, Title: title, Slug: slug, Project: project, Body: "body of " + title, Tags: tags,
		Created: at, Updated: at,
	})
	require.NoError(t, err)
	return n
}

// A plan:<slug> tag is a link to the plan with the plan's status: a captured
// plan's own, else its phase from its steps. The tags software stamps fold
// behind "+N"; the peek links the plan and folds them too.
func TestLibNotePlanChipLinksToPlanWithStatus(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	capture := libNote(t, mgr, "example-api", "cc-plan-api-rate-limiting", "API rate limiting", now,
		[]string{"cc-plan", "plan:api-rate-limiting", "plan-status:presented", "created-by:agent", "billing"})

	reader := getPeek(t, mux, "/console/notes/"+capture.ID+"?reader=1").Body.String()
	require.Contains(t, reader, `<a class="lib-plan-chip" href="/console/plans/api-rate-limiting"`)
	require.Contains(t, reader, `<span class="mono">plan:api-rate-limiting</span> <span class="badge accent">presented</span>`)
	require.Contains(t, reader, `<span class="badge">billing</span>`, "a tag written for people stays in view")
	fold := strings.Index(reader, `<details class="lib-tag-fold">`)
	require.NotEqual(t, -1, fold)
	require.Contains(t, reader[fold:], `>+3</summary>`, "the three tags software stamps fold behind +3")
	for _, machine := range []string{">cc-plan<", ">plan-status:presented<", ">created-by:agent<"} {
		require.NotContains(t, reader[:fold], machine)
		require.Contains(t, reader[fold:], machine)
	}
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/plans/api-rate-limiting").Code, "the chip names a real plan page")

	narrative := libNote(t, mgr, "example-api", "plan-search-v2", "Plan: search v2", now, []string{"plan:search-v2", "created-by:agent"})
	for _, task := range []core.Task{
		{ID: "T1", Title: "index", Status: core.TaskInProgress},
		{ID: "T2", Title: "schema", Status: core.TaskDone},
	} {
		task.ProjectSlug, task.PlanSlug, task.CreatedAt, task.UpdatedAt = "example-api", "search-v2", now, now
		require.NoError(t, store.CreateTask(ctx, db, task))
	}
	composed := getPeek(t, mux, "/console/notes/"+narrative.ID+"?reader=1").Body.String()
	require.Contains(t, composed, `href="/console/plans/search-v2" title="Plan: search v2: 1 of 2 steps done">`)
	require.Contains(t, composed, `<span class="badge accent">in progress</span>`, "a composed plan reads its phase")
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/plans/search-v2").Code)

	page := libMain(t, getHTMLBody(t, mux, "/console/notes/"+capture.ID))
	require.Contains(t, page, `<span class="badge accent">presented</span>`, "the page's reader is the same reader")

	peek := getPeek(t, mux, "/console/notes/"+capture.ID+"?peek=1").Body.String()
	require.Contains(t, peek, `<dt>Plan</dt>`)
	require.Contains(t, peek, `<a class="lib-plan-chip" href="/console/plans/api-rate-limiting">`)
	require.Contains(t, peek, `>+3</summary>`)
	require.NotContains(t, peek, `>plan:api-rate-limiting</span> `, "a peek does not look the status up")
}

// The machine tags are exactly the ones software stamps.
func TestLibNoteTagView(t *testing.T) {
	got := noteDetail{Tags: []string{"plan:a", "created-by:gardener", "plan-status:shipped", "cc-plan", "agent-cache",
		"plan:b", "digest", "planning", "created"}}.TagView()
	require.Equal(t, []string{"a", "b"}, []string{got.Plans[0].Slug, got.Plans[1].Slug})
	require.Equal(t, "/console/plans/a", got.Plans[0].Href)
	require.Equal(t, []string{"created-by:gardener", "plan-status:shipped", "cc-plan", "agent-cache"}, got.Machine)
	require.Equal(t, []string{"digest", "planning", "created"}, got.Tags)
	require.Empty(t, noteDetail{}.TagView().Plans)
}

// Notes filter by project and star, in the URL; the header says how many of
// all; the star returns with the filters; bad values are a 400.
func TestLibNotesFilters(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Now().UTC().Add(-time.Hour)
	libNote(t, mgr, "pay", "pay-a", "Pay A", base, nil)
	b := libNote(t, mgr, "pay", "pay-b", "Pay B", base.Add(time.Minute), nil)
	c := libNote(t, mgr, "", "inbox-c", "Inbox C", base.Add(2*time.Minute), nil)
	for _, n := range []core.Note{b, c} {
		require.Equal(t, http.StatusSeeOther, postFavorite(mux, "note", n.ID, url.Values{"favorite": {"1"}}).Code)
	}

	for path, want := range map[string]int{
		"/console/notes?format=json&project=pay":       2,
		"/console/notes?format=json&project=pay&fav=1": 1,
		"/console/notes?format=json&project=global":    1,
		"/console/notes?format=json&fav=1":             2,
		"/console/notes?format=json&kind=bogus":        3, // Notes have no kind: the filter is not theirs
	} {
		var d notesData
		getJSON(t, mux, path, &d)
		require.Equal(t, want, d.Count, path)
		require.Equal(t, 3, d.Total, path)
	}

	const qs = "?fav=1&project=pay"
	esc := strings.ReplaceAll(qs, "&", "&amp;")
	main := libMain(t, getHTMLBody(t, mux, "/console/notes"+qs))
	require.Contains(t, main, "<strong>1</strong>of 3 notes")
	require.Contains(t, main, `aria-label="Project: pay"`)
	require.Contains(t, main, `data-auto-url="/console/notes/`+b.ID+esc+`"`)
	require.Contains(t, main, `value="/console/notes/`+b.ID+esc+`" data-lib-next>`)
	require.Contains(t, main, `href="/console/notes?fav=1&amp;project=global">Global inbox<span class="n">1</span>`)

	rr := postFavorite(mux, "note", b.ID, url.Values{"favorite": {"0"}, "next": {"/console/notes/" + b.ID + qs}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/notes/"+b.ID+qs, rr.Header().Get("Location"))

	for query, want := range map[string]string{
		"project=all": `invalid project "all"`,
		"fav=2":       `invalid fav "2"`,
		"group=tree":  `invalid group "tree"`,
		"sort=reach":  `invalid sort "reach": valid values are recent, name, favorites`,
	} {
		for _, path := range []string{"/console/notes?", "/console/notes/" + b.ID + "?"} {
			rr := getPeek(t, mux, path+query)
			require.Equal(t, http.StatusBadRequest, rr.Code, path+query)
			require.Contains(t, html.UnescapeString(rr.Body.String()), want, path+query)
		}
	}
}
