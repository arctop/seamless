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
	// Every template file, the partials a page parses with it included
	// (settings_backups.html, context_shared.html, the peek fragments).
	entries, err := templateFS.ReadDir("templates")
	require.NoError(t, err)
	for _, e := range entries {
		source, err := templateFS.ReadFile("templates/" + e.Name())
		require.NoError(t, err)
		for _, form := range getForm.FindAllString(string(source), -1) {
			require.Contains(t, form, "data-seam-query", "%s has a GET data form outside the shared no-reload path: %s", e.Name(), form)
		}
	}
}

// Settings > Backups stays on the shared no-reload path: its links into the
// confirm card and back are data-seam-query, its forms are plain POSTs the
// shared path takes, the restart wait and the live refresh re-draw the view in
// place, and its script re-applies what it holds after every morph.
func TestBackupsSection_StaysOnTheSharedNoReloadPath(t *testing.T) {
	source, err := templateFS.ReadFile("templates/settings_backups.html")
	require.NoError(t, err)
	page := string(source)
	require.Contains(t, page, `<a class="btn small" href="{{.Restore}}" data-seam-query>`)
	require.Contains(t, page, `href="/console/settings?s=backups#backups" data-seam-query>Cancel</a>`)
	require.Contains(t, page, `<a class="btn small" href="{{.}}" data-seam-query>{{icon "undo-2"}} Undo this restore</a>`)
	for _, action := range []string{"/console/settings/backups", "/console/settings/backups/now", "/console/settings/backups/delete",
		"/console/settings/backups/restore", "/console/settings/backups/restore/cancel", "/console/settings/backups/restore/dismiss"} {
		require.Contains(t, page, `method="post" action="`+action+`"`, action)
	}

	js := string(backupsJS)
	require.NotContains(t, js, "location.reload(", "the section never reloads the document")
	require.NotContains(t, js, "location.href =", "nor navigates to refresh")
	require.Contains(t, js, "window.SeamConsole.patchPage(doc, 'backups')", "a re-draw morphs the view in place")
	require.Contains(t, js, "document.addEventListener('seam:content-updated', enhance)", "held state is re-applied after a morph")
	require.Contains(t, js, "document.addEventListener('seam:event'", "backup.* events refresh the section")
	require.Contains(t, js, "f.addEventListener('submit'", "the ceremony runs before the shared POST path, on the form itself")
	require.NotContains(t, string(passkeyJS), "location.reload(")
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

// A POST's redirect to a #anchor must land there in place too. fetch() follows a
// 303 out of sight and a Response's url never carries a fragment (measured in
// Chrome: a header set on the 303 reads null after the follow, and
// redirect:'manual' is an opaque status-0 answer), so the in-place client asks
// for the redirect to be named instead. Each redirecting handler answers that
// request with exactly the location its 303 would send, fragment included, and
// answers every other caller with the 303 as before.
func TestMutationRedirects_NameTheirAnchorToTheInPlaceClient(t *testing.T) {
	db, mux := newConsole(t)
	seedIsolationProject(t, db, "acme")

	for _, tc := range []struct {
		name, path, body, prefix, anchor string
	}{
		{"settingsBack", "/console/settings/features", "", "/console/settings?s=features&notice=", "features"},
		{"backups flash", "/console/settings/backups/restore/dismiss", "", "/console/settings?s=backups&error=", "backups"},
		{"isolation confirm", "/console/projects/acme/isolation", "state=confidential",
			"/console/projects/acme?tab=overview&isolate=confidential", "isolation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := postForm(mux, tc.path, tc.body)
			require.Equal(t, http.StatusSeeOther, plain.Code, plain.Body.String())
			loc := plain.Header().Get("Location")
			require.True(t, strings.HasPrefix(loc, tc.prefix), "got %q", loc)
			require.True(t, strings.HasSuffix(loc, "#"+tc.anchor), "the redirect lands on its block: %q", loc)
			require.Empty(t, plain.Header().Get(locationHeader), "only a caller that asks gets the named form")

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testKey)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set(partialHeader, "1")
			req.Header.Set(followHeader, "1")
			named := do(mux, req)
			require.Equal(t, http.StatusNoContent, named.Code,
				"a 3xx would be followed by fetch() out of sight, fragment and all")
			require.Empty(t, named.Header().Get("Location"))
			require.Equal(t, loc, named.Header().Get(locationHeader),
				"the named location is exactly the redirect's, #anchor included")
			require.Equal(t, "no-store", named.Header().Get("Cache-Control"))
			require.Empty(t, named.Body.String())
		})
	}
}

// followWriter names only a redirect the browser would follow with a GET. A 307
// or 308 repeats the method and body, which only the browser can do, and an
// answer that is not a redirect is the answer.
func TestFollowable_NamesOnlyGetRedirects(t *testing.T) {
	answer := func(code int, follow bool) *httptest.ResponseRecorder {
		h := followable(func(w http.ResponseWriter, r *http.Request) {
			if code >= 300 && code < 400 {
				http.Redirect(w, r, "/console/x?y=1#z", code)
				return
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte("page"))
		})
		req := httptest.NewRequest(http.MethodPost, "/console/p", nil)
		if follow {
			req.Header.Set(followHeader, "1")
		}
		rr := httptest.NewRecorder()
		h(rr, req)
		return rr
	}

	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		rr := answer(code, true)
		require.Equal(t, http.StatusNoContent, rr.Code, "%d", code)
		require.Equal(t, "/console/x?y=1#z", rr.Header().Get(locationHeader), "%d", code)
		require.Empty(t, rr.Header().Get("Location"), "%d", code)

		rr = answer(code, false)
		require.Equal(t, code, rr.Code)
		require.Equal(t, "/console/x?y=1#z", rr.Header().Get("Location"))
	}
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		rr := answer(code, true)
		require.Equal(t, code, rr.Code, "a %d keeps its method: the browser follows it", code)
		require.Equal(t, "/console/x?y=1#z", rr.Header().Get("Location"))
		require.Empty(t, rr.Header().Get(locationHeader))
	}
	rr := answer(http.StatusOK, true)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "page", rr.Body.String())
	require.Empty(t, rr.Header().Get(locationHeader))
}

// The client half: an in-place load keeps the fragment of the URL it asked for
// (or of the location a mutation's redirect names), puts it in history, and
// lands on the anchor after the morph. A Response's url has no fragment, so a
// load that took its next URL from response.url alone lost the anchor, its
// history entry and its scroll.
func TestNavigationJS_KeepsTheAnchor(t *testing.T) {
	js := string(navigationJS)

	require.Contains(t, js, "if (mutation) headers['"+followHeader+"'] = '1';",
		"a mutation asks for its redirect to be named")
	require.Contains(t, js, "response.headers.get('"+locationHeader+"')")
	require.Contains(t, js, "landing = new URL(named, target.href);")
	require.Contains(t, js, "if (landing.origin !== location.origin) throw",
		"a named location off the console is never fetched")
	require.Equal(t, 2, strings.Count(js, "heldViews("),
		"only a GET's own fetch sends held views: a mutation's follow is answered whole, so it never retries the POST")

	require.Contains(t, js, "var landing = target;", "a GET lands where it asked to, fragment included")
	require.Contains(t, js, "finalURL.hash = landing.hash;", "response.url drops the fragment: put it back")
	require.Contains(t, js, "if (!canonical.hash) canonical.hash = finalURL.hash;",
		"a canonical URL from the page keeps the fragment too")
	require.NotContains(t, js, "new URL(response.url || target.href",
		"the next URL is not response.url alone: it has no fragment")

	// Order: morph, then history, then the scroll.
	patch := strings.Index(js, "patchPage(doc, options.source")
	push := strings.Index(js, "history.pushState(state, '', nextURL)")
	keep := strings.Index(js, "nextURL = canonical.href;")
	scroll := strings.Index(js, "anchor.scrollIntoView({ block: 'start' })")
	require.True(t, patch >= 0 && keep > patch && push > keep && scroll > push,
		"the fragment goes into history and the view lands on it after the morph")
	require.Contains(t, js, "if (currentViewURL.hash && (mutation || currentViewURL.href !== shown))",
		"a mutation always lands on its anchor; drawing the shown view again keeps the scroll")
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
