package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// UX 39 (plan:console-ux-pass): Settings > Workspaces finds a project and
// shows only what is set. The directory carries a filter and a With repo
// routes switch, both in a bar the page script reveals; the switch starts on
// when more than 12 rows have no route; the Family column is drawn only when
// some project has a family or a parent; and the heads speak plain words.

// wrSeed registers routed projects (each with one repo route) and unrouted
// ones, named so a test can find them again.
func wrSeed(t *testing.T, db *sql.DB, routed, unrouted int) {
	t.Helper()
	ctx := context.Background()
	for i := range routed {
		slug := fmt.Sprintf("example-routed-%02d", i)
		_, err := store.EnsureProject(ctx, db, slug, fmt.Sprintf("Routed %02d", i))
		require.NoError(t, err)
		require.NoError(t, store.AddRepoMapping(ctx, db, "/work/"+slug, slug))
	}
	for i := range unrouted {
		slug := fmt.Sprintf("example-plain-%02d", i)
		_, err := store.EnsureProject(ctx, db, slug, "")
		require.NoError(t, err)
	}
}

// wrDirectory cuts the directory panel out of a rendered Workspaces page:
// from the panel's opening tag to the first family dialog.
func wrDirectory(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, `<section class="registry-panel workspace-panel`)
	require.NotEqual(t, -1, start, "the directory panel renders")
	dir := page[start:]
	end := strings.Index(dir, `<dialog`)
	require.NotEqual(t, -1, end)
	return dir[:end]
}

// wrToggle is the With repo routes switch's input as the server draws it.
var wrToggle = regexp.MustCompile(`<input type="checkbox" role="switch" data-workspace-routed-only( checked)?>`)

// The switch's default follows the counts: offered only where it changes
// what shows (some rows routed, some not), and on from the 13th row without a
// route. Counted on the rows themselves, so the boundary is exact.
func TestWrDirectory_ToggleDefaultFollowsTheCounts(t *testing.T) {
	scope := func(slug string, repos int) workspaceScope {
		w := workspaceScope{Slug: slug, Registered: true}
		for i := range repos {
			w.Repos = append(w.Repos, repoRoute{Path: fmt.Sprintf("/work/%s-%d", slug, i)})
		}
		return w
	}
	rows := func(routed, unrouted int) []workspaceScope {
		var out []workspaceScope
		for i := range routed {
			out = append(out, scope(fmt.Sprintf("r%d", i), 1+i%2))
		}
		for i := range unrouted {
			out = append(out, scope(fmt.Sprintf("u%d", i), 0))
		}
		return out
	}
	for _, tc := range []struct {
		name               string
		routed, unrouted   int
		toggle, routedOnly bool
	}{
		{"no rows", 0, 0, false, false},
		{"no route anywhere: the switch would hide every row", 0, 46, false, false},
		{"every row routed: the switch would hide nothing", 5, 0, false, false},
		{"twelve without a route: offered, off", 1, workspaceRoutedOnlyAfter, true, false},
		{"thirteen without a route: offered, on", 1, workspaceRoutedOnlyAfter + 1, true, true},
		{"the owner's scale", 9, 46, true, true},
		{"a few without a route", 40, 3, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newWorkspaceDirectory(rows(tc.routed, tc.unrouted))
			require.Equal(t, tc.routed, dir.Routed)
			require.Equal(t, tc.unrouted, dir.Unrouted)
			require.Equal(t, tc.toggle, dir.Toggle, "offered")
			require.Equal(t, tc.routedOnly, dir.RoutedOnly, "starts on")
			require.False(t, dir.Family, "no row has a family or a parent")
		})
	}
	require.Equal(t, 12, workspaceRoutedOnlyAfter, "the task's threshold: on when more than 12 have no route")
}

// The same rule, through the page: the server draws the switch checked, or
// not, or not at all, from the request's own read of the directory.
func TestWrDirectory_PageDrawsTheToggleFromTheCounts(t *testing.T) {
	for _, tc := range []struct {
		name             string
		routed, unrouted int
		want             string // "" means no switch at all
	}{
		{"no route anywhere", 0, 14, ""},
		{"twelve without a route", 2, 12, "off"},
		{"thirteen without a route", 2, 13, "on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mux := newConsole(t)
			wrSeed(t, db, tc.routed, tc.unrouted)
			dir := wrDirectory(t, getHTMLBody(t, mux, "/console/settings?s=workspaces"))
			m := wrToggle.FindAllStringSubmatch(dir, -1)
			switch tc.want {
			case "":
				require.Empty(t, m, "no switch where it would change nothing")
				require.NotContains(t, dir, "With repo routes")
			case "off":
				require.Len(t, m, 1)
				require.Empty(t, m[0][1], "the switch starts off")
			case "on":
				require.Len(t, m, 1)
				require.Equal(t, " checked", m[0][1], "the switch starts on")
			}
			require.Contains(t, dir, `<span class="registry-count" data-workspace-shown aria-live="polite">`+fmt.Sprint(tc.routed+tc.unrouted)+`</span>`)
		})
	}
}

// Without script nothing is hidden: the server draws every row with no hidden
// attribute and keeps the bar (the filter and the switch) hidden, for the page
// script to reveal. The bar is kept off the redraw that follows a family save.
// Each row carries what the filter matches (slug, name, every route's path)
// and whether it has a route.
func TestWrDirectory_EveryRowShowsAndCarriesWhatTheFilterMatches(t *testing.T) {
	db, mux := newConsole(t)
	wrSeed(t, db, 2, 13)
	ctx := context.Background()
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/second-checkout", "example-routed-00"))
	dir := wrDirectory(t, getHTMLBody(t, mux, "/console/settings?s=workspaces"))

	require.Contains(t, dir, `<div class="workspace-tools" id="workspace-tools" data-workspace-tools data-live-skip hidden>`)
	require.Contains(t, dir, `<input type="search" placeholder="Find a project" aria-label="Find a project by slug, name or repo route" autocomplete="off" spellcheck="false" data-workspace-filter>`)
	require.Contains(t, dir, `<p class="registry-empty workspace-none" data-workspace-none hidden>No project matches.</p>`)

	rows := regexp.MustCompile(`<article class="workspace-row[^"]*"[^>]*>`).FindAllString(dir, -1)
	require.Len(t, rows, 15, "every row is drawn")
	for _, row := range rows {
		require.NotContains(t, row, "hidden", "the server hides no row: %s", row)
	}
	require.Contains(t, dir, `<article class="workspace-row" data-workspace-scope="example-routed-00" data-workspace-find="example-routed-00 Routed 00 /work/example-routed-00 /work/second-checkout" data-workspace-routed>`)
	require.Contains(t, dir, `<article class="workspace-row" data-workspace-scope="example-plain-00" data-workspace-find="example-plain-00">`)
	require.Equal(t, 2, strings.Count(dir, " data-workspace-routed>"), "only the routed rows say so")

	// The page script's selectors are the markup's: a rename on one side
	// would leave the filter matching nothing.
	script := settingsPageSource(t)
	for _, hook := range []string{"data-workspace-directory", "data-workspace-tools", "data-workspace-filter", "data-workspace-routed-only", "data-workspace-find", "data-workspace-shown", "data-workspace-none"} {
		require.Contains(t, script, "'["+hook+"]'", "the script reads %s", hook)
		require.Contains(t, dir, " "+hook, "the markup carries %s", hook)
	}
	require.Contains(t, script, "row.hasAttribute('data-workspace-routed')")
	require.Contains(t, script, "row.getAttribute('data-workspace-find')")
}

// The script narrows rows in place: delegated on document (so a section the
// sub-nav swaps in needs no wiring), run again after every in-place swap, and
// it never reloads or navigates.
func TestWrDirectory_FilterScriptNarrowsInPlace(t *testing.T) {
	page := settingsPageSource(t)
	start := strings.Index(page, "function filterWorkspaces()")
	require.NotEqual(t, -1, start)
	script := page[start:]
	script = script[:strings.Index(script, "})();")]
	require.Contains(t, script, "document.addEventListener('input', function (event) {\n    if (event.target.matches && event.target.matches('[data-workspace-filter]')) filterWorkspaces();")
	require.Contains(t, script, "document.addEventListener('change', function (event) {\n    if (event.target.matches && event.target.matches('[data-workspace-routed-only]')) filterWorkspaces();")
	require.Contains(t, script, "document.addEventListener('seam:content-updated', filterWorkspaces);")
	require.Contains(t, script, "if (tools) tools.hidden = false;", "the script reveals the bar")
	require.Contains(t, script, "row.hidden = !keep;")
	for _, never := range []string{"location.reload", "location.assign", "location.href =", "fetch("} {
		require.NotContains(t, script, never)
	}

	// A row is display: grid, so the hidden attribute needs its own rule; so do
	// the bar and the family dialogs' options, whose filter sets hidden too.
	css := string(consoleCSS)
	require.Contains(t, css, ".workspace-row[hidden], .workspace-tools[hidden], .workspace-none[hidden], .family-project-option[hidden] { display: none; }")
	require.Contains(t, css, ".workspace-panel.is-no-family { --workspace-columns:")
}

// The Family column is drawn only when some project has a family or a parent.
func TestWrDirectory_FamilyColumnOnlyWhenSet(t *testing.T) {
	t.Run("none set", func(t *testing.T) {
		db, mux := newConsole(t)
		wrSeed(t, db, 1, 2)
		dir := wrDirectory(t, getHTMLBody(t, mux, "/console/settings?s=workspaces"))
		require.Contains(t, dir, `<section class="registry-panel workspace-panel is-no-family" data-workspace-directory>`)
		require.Contains(t, dir, "<span>Project</span><span>Repo route</span>\n")
		require.NotContains(t, dir, "<span>Family</span>")
		require.NotContains(t, dir, "workspace-lineage")
		require.NotContains(t, dir, "No family membership")
		require.Contains(t, dir, `aria-label="Projects, repo routes">`)
	})
	t.Run("a family", func(t *testing.T) {
		db, mux := newConsole(t)
		wrSeed(t, db, 1, 2)
		require.NoError(t, store.SetProjectFamilies(context.Background(), db, map[string][]string{
			"example-suite": {"example-plain-00", "example-plain-01"},
		}))
		dir := wrDirectory(t, getHTMLBody(t, mux, "/console/settings?s=workspaces"))
		require.Contains(t, dir, `<section class="registry-panel workspace-panel" data-workspace-directory>`)
		require.Contains(t, dir, "<span>Project</span><span>Repo route</span><span>Family</span>")
		require.Equal(t, 3, strings.Count(dir, `<div class="workspace-cell workspace-lineage">`), "one lineage cell per row")
		require.Equal(t, 2, strings.Count(dir, `class="workspace-tag family"`))
	})
	t.Run("a parent", func(t *testing.T) {
		db, mux := newConsole(t)
		ctx := context.Background()
		for _, slug := range []string{"example-platform", "example-web"} {
			_, err := store.EnsureProject(ctx, db, slug, "")
			require.NoError(t, err)
		}
		require.NoError(t, store.SetProjectParent(ctx, db, "example-web", "example-platform", time.Now().UTC()))
		dir := wrDirectory(t, getHTMLBody(t, mux, "/console/settings?s=workspaces"))
		require.NotContains(t, dir, "is-no-family")
		require.Contains(t, dir, `class="workspace-tag parent"`)
	})
}

// The heads speak plain words, and project over workspace and scope (UX 16
// item 9): no "Routing directory" kicker, no "intentionally compact", no
// "arrival routes"; the section's label and ?s= id stay.
func TestWrHeads_PlainWords(t *testing.T) {
	db, mux := newConsole(t)
	wrSeed(t, db, 1, 1)
	// A route to no project survives only in the legacy map (the repo_map
	// table refuses one), which RepoMapRows folds in.
	require.NoError(t, store.SetSetting(context.Background(), db, store.SettingRepoProjectMap, `{"/work/loose": ""}`))
	page := getHTMLBody(t, mux, "/console/settings?s=workspaces")
	section := page[strings.Index(page, `id="workspace-registry"`):]
	section = section[:strings.Index(section, `<dialog`)]
	source := settingsPageSource(t)
	for _, gone := range []string{"Routing directory", "intentionally compact", "arrival route"} {
		require.NotContains(t, section, gone)
		require.NotContains(t, source, gone)
	}
	require.NotContains(t, section, "Unbound")
	require.Contains(t, section, `<h2>Workspaces<span class="hint">`, "the head: the title and its hint")
	require.Contains(t, section, "A repo route ties a repository folder to a project")
	require.Contains(t, section, "<div><span>Repo routes</span><h3>Projects and their repositories</h3><p>An agent that starts in a project&rsquo;s repository works in that project.</p></div>")
	require.Contains(t, section, "<strong>Repo routes with no project</strong>", "the routes that resolve to no project, at Advanced")
	require.Contains(t, page, `data-section-link="workspaces"`)
	require.Contains(t, page, `<span class="settings-subnav-label">Workspaces</span>`)

	// Below Advanced the routes with no project stay hidden, as before, and
	// the Experience cards name them in the same words.
	_, std := newConsoleLevel(t, config.Features{}, "standard")
	require.NotContains(t, getHTMLBody(t, std, "/console/settings?s=workspaces"), "Repo routes with no project")
	require.Contains(t, getHTMLBody(t, std, "/console/settings?s=experience"),
		"<li>Settings \u203a Workspaces: each repo route&#39;s host and the repo routes with no project</li>")
}

// ?format=json keeps exactly the fields it had: the directory's counts are a
// method's answer, never a field.
func TestWrJSON_KeepsItsFields(t *testing.T) {
	db, mux := newConsole(t)
	wrSeed(t, db, 1, 13)
	var raw map[string]json.RawMessage
	getJSON(t, mux, "/console/settings?format=json", &raw)
	for _, key := range []string{"projects", "repoMap", "families", "workspaces", "unboundRepos", "familyEditors", "familyOptions", "sectionStatus"} {
		require.Contains(t, raw, key)
	}
	for key := range raw {
		lower := strings.ToLower(key)
		require.NotContains(t, lower, "routedonly", key)
		require.NotContains(t, lower, "directory", key)
	}
	var rows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["workspaces"], &rows))
	require.Len(t, rows, 14)
	for _, row := range rows {
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			require.Contains(t, []string{"slug", "name", "description", "parentSlug", "registered", "retired", "parentRegistered", "repos", "families"}, k)
		}
	}
}
