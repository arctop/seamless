package console

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

func TestServeNavigationJS(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/navigation.js", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "text/javascript")
	require.Contains(t, rr.Body.String(), "window.SeamConsole")
}

func TestLayout_LoadsSharedNavigationClient(t *testing.T) {
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), `<script src="/console/static/navigation.js"></script>`)
}

// The sidebar is the one part of the morphed document whose SHAPE changes:
// toggling an optional feature adds or removes its nav entries. The client
// patches counts and the active marker in place (which is what preserves the
// count bump), and that patch is index-by-index -- so it silently does nothing
// when the two link lists have different lengths. These two tests pin the two
// halves of the fix together: the server really does render a different link
// set per feature state, and the client really does have the structural branch
// that copes with it. Without the branch the owner switches a feature off and
// the sidebar keeps offering its screens until the next full page load.
func TestNav_LinkSetChangesWithFeatureState(t *testing.T) {
	navLinks := regexp.MustCompile(`<nav class="nav".*?</nav>`)
	hrefs := regexp.MustCompile(`href="(/console/[^"]*)"`)
	linkSet := func(feats config.Features) []string {
		_, mux := newConsoleFeatures(t, feats)
		page := getPeek(t, mux, "/console/settings")
		require.Equal(t, http.StatusOK, page.Code)
		nav := navLinks.FindString(strings.ReplaceAll(page.Body.String(), "\n", " "))
		require.NotEmpty(t, nav, "the settings page must render the sidebar")
		var out []string
		for _, m := range hrefs.FindAllStringSubmatch(nav, -1) {
			out = append(out, m[1])
		}
		return out
	}

	off := linkSet(config.Features{})
	on := linkSet(config.Features{Research: true})

	require.NotEqual(t, len(off), len(on),
		"the two nav link lists must differ in LENGTH -- that is exactly the case the in-place count patch cannot express")
	for _, href := range []string{"/console/labs", "/console/trials"} {
		require.NotContains(t, off, href, "a disabled feature must not be offered in the sidebar")
		require.Contains(t, on, href, "an enabled feature must be offered in the sidebar")
	}
}

func TestNavigationJS_MorphsTheNavWhenItsLinkSetChanges(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/navigation.js", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	js := rr.Body.String()

	require.Contains(t, js, "function navShape(",
		"the client must be able to tell that the sidebar's link set changed")
	require.Contains(t, js, "morphNode(currentNav, freshNav)",
		"a changed link set must morph the whole nav; the index-by-index count patch cannot add or remove entries")
	require.Regexp(t, `navShape\(currentNav\) !== navShape\(freshNav\)`, js,
		"the structural branch must be selected by comparing the two link sets")
}

func TestQueryForms_UseInPlaceNavigation(t *testing.T) {
	getForm := regexp.MustCompile(`<form[^>]*method="get"[^>]*>`)
	for _, name := range pageNames {
		source, err := templateFS.ReadFile("templates/" + name + ".html")
		require.NoError(t, err)
		for _, form := range getForm.FindAllString(string(source), -1) {
			require.Contains(t, form, "data-seam-query", "%s has a GET data form outside the shared no-reload path: %s", name, form)
		}
	}
}

func TestQueryControls_AreWiredAcrossConsole(t *testing.T) {
	wantMarkers := map[string]int{
		"overview":  1,
		"now":       1,
		"retrieval": 1,
		"projects":  4,
		"context":   2,
		"sessions":  5,
		"search":    5,
		"memories":  2,
		"notes":     2,
		"plans":     1,
		"trials":    2,
		"gardener":  2,
		"settings":  1,
	}
	for name, want := range wantMarkers {
		source, err := templateFS.ReadFile("templates/" + name + ".html")
		require.NoError(t, err)
		require.GreaterOrEqual(t, strings.Count(string(source), "data-seam-query"), want,
			"%s must keep every filter, sort, search, and time-window control on the shared in-place path", name)
	}
}

func TestMutationForms_UseInPlaceNavigation(t *testing.T) {
	client := string(navigationJS)
	require.Contains(t, client, `method === 'post' && !!form.closest('.main')`,
		"owner POST forms inside the console view must use the shared no-reload path")
	require.Contains(t, client, `load(target.href, { method: 'POST'`)

	for _, name := range []string{"settings", "gardener"} {
		source, err := templateFS.ReadFile("templates/" + name + ".html")
		require.NoError(t, err)
		require.Contains(t, string(source), `document.addEventListener('seam:content-updated'`,
			"%s has page-owned controls that must be re-enhanced after a mutation patch", name)
	}
}

// The isolation control and its confirm step are pure server-rendered markup:
// the POST forms ride the shared mutation path (they live inside .main), the
// confirm step is a page state rather than a JS dialog, and the way out of it is
// a data-seam-query link that patches the view instead of navigating.
func TestIsolationControl_StaysOnTheSharedNoReloadPath(t *testing.T) {
	source, err := templateFS.ReadFile("templates/projectdetail.html")
	require.NoError(t, err)
	page := string(source)

	require.Contains(t, page, `<form class="iso-form" method="post" action="/console/projects/{{.Slug}}/isolation">`)
	require.Contains(t, page, `{{define "isolation-confirm"}}`)
	require.Contains(t, page, `<a class="btn small" href="{{.Cancel}}" data-seam-query>Cancel</a>`)
	require.Contains(t, page, `<a class="btn small" href="{{.Cancel}}" data-seam-query>Back</a>`)
	for _, banned := range []string{"confirm(", "alert(", "<dialog"} {
		require.NotContains(t, page, banned,
			"the tighten confirmation is server-rendered, not a JS dialog")
	}
}

func TestDataRefreshClients_NeverReloadDocument(t *testing.T) {
	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)

	for name, source := range map[string]string{
		"layout":     string(layout),
		"navigation": string(navigationJS),
		"library":    string(libraryJS),
		"sky":        string(skyJS),
		// The shell hides the level and update banners in place; a "Got it"
		// never reloads to make one go away.
		"shell": string(shellJS),
	} {
		require.NotContains(t, source, "location.reload(", "%s must keep data refreshes inside the current document", name)
	}
	require.NotContains(t, string(libraryJS), "location.href = href",
		"a reader fetch failure must surface the error, not degrade into a document navigation")
	require.NotContains(t, string(libraryJS), "window.scrollTo(",
		"a reader swap must preserve the document position instead of looking like a reload")
	require.Contains(t, string(libraryJS), "reader=1",
		"a reader selection must request only the server-rendered reader fragment")
	require.Contains(t, string(libraryJS), "el.innerHTML = html",
		"a reader selection must render the fetched fragment into the existing reader")
	require.Contains(t, string(navigationJS), "morphNode(currentMain, freshMain)")
	require.Contains(t, string(navigationJS), "The current data is unchanged")
}

// Every registered screen is a real GET route this console mounts -- not the
// styled-404 catch-all. A registry entry pointing at nothing would put a dead
// link in the sidebar, the palette, the shortcut map, and the docs at once.
func TestScreenRegistry_HrefsAreRegisteredRoutes(t *testing.T) {
	_, mux := newConsoleFeatures(t, featuresAll(true))
	for _, sc := range screenRegistry() {
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, sc.Href, nil))
		require.NotEmpty(t, pattern, "screen %q links to %q, which no route claims", sc.ID, sc.Href)
		require.NotEqual(t, "GET /console/", pattern,
			"screen %q links to %q, which only the styled-404 catch-all answers: register the route, or "+
				"fix the registry entry in screens.go", sc.ID, sc.Href)
	}
}

// The layout renders EXACTLY the registry's visible entries, at each level and
// feature state -- the three-way sibling of TestNav_LinkSetChangesWithFeatureState.
// Because the g-chords, the ? sheet, and the palette's Jump to all read these
// links (shell.js and search.js derive from the DOM), this also proves they
// follow the level with no list of their own.
//
// The expected sets are the plan's screen matrix, transcribed on purpose: the
// matrix is a product decision, so changing it should take a deliberate edit
// here as well as in screens.go.
func TestNav_RendersExactlyTheVisibleScreensPerLevel(t *testing.T) {
	basic := []string{"/console/", "/console/memories", "/console/notes", "/console/gardener",
		"/console/sessions", "/console/settings"}
	standard := []string{"/console/", "/console/now", "/console/memories", "/console/notes",
		"/console/gardener", "/console/projects", "/console/plans", "/console/tasks", "/console/sessions",
		"/console/system", "/console/privacy", "/console/settings"}
	standardResearch := []string{"/console/", "/console/now", "/console/memories", "/console/notes",
		"/console/gardener", "/console/projects", "/console/plans", "/console/tasks", "/console/sessions",
		"/console/labs", "/console/trials", "/console/system", "/console/privacy", "/console/settings"}
	advanced := []string{"/console/", "/console/now", "/console/interactions", "/console/memories",
		"/console/notes", "/console/retrieval", "/console/gardener", "/console/projects", "/console/plans",
		"/console/tasks", "/console/sessions", "/console/system", "/console/privacy", "/console/settings"}
	advancedResearch := []string{"/console/", "/console/now", "/console/interactions", "/console/memories",
		"/console/notes", "/console/retrieval", "/console/gardener", "/console/projects", "/console/plans",
		"/console/tasks", "/console/sessions", "/console/labs", "/console/trials", "/console/system", "/console/privacy",
		"/console/settings"}

	navRe := regexp.MustCompile(`<nav class="nav".*?</nav>`)
	linkRe := regexp.MustCompile(`<a href="(/console/[^"]*)" data-key="([^"]*)" data-tip="([^"]*)"`)
	for _, tc := range []struct {
		level    string
		research bool
		want     []string
	}{
		{"basic", false, basic},
		{"basic", true, basic},
		{"standard", false, standard},
		{"standard", true, standardResearch},
		{"advanced", false, advanced},
		{"advanced", true, advancedResearch},
	} {
		t.Run(tc.level+map[bool]string{true: "+research", false: ""}[tc.research], func(t *testing.T) {
			feats := config.Features{Research: tc.research}
			_, mux := newConsoleLevel(t, feats, tc.level)
			page := getPeek(t, mux, "/console/settings")
			require.Equal(t, http.StatusOK, page.Code)
			nav := navRe.FindString(strings.ReplaceAll(page.Body.String(), "\n", " "))
			require.NotEmpty(t, nav)

			var got []string
			for _, m := range linkRe.FindAllStringSubmatch(nav, -1) {
				got = append(got, m[1])
				require.NotEmpty(t, m[2], "every nav link carries its g-chord: %s", m[1])
				require.NotEmpty(t, m[3], "every nav link carries its collapsed-rail tip: %s", m[1])
			}
			require.Equal(t, tc.want, got, "the sidebar at %s must be exactly the matrix's entries", tc.level)
			require.Equal(t, strings.Count(nav, "<a ")-strings.Count(nav, "data-jump="), len(got),
				"every visible nav link must be a registry entry")

			lvl, err := parseLevel(tc.level)
			require.NoError(t, err)
			var derived []string
			for _, sc := range visibleScreens(feats, lvl) {
				if sc.NavRow {
					derived = append(derived, sc.Href)
				}
			}
			require.Equal(t, derived, got, "the layout renders visibleScreens and nothing else")
		})
	}
}
