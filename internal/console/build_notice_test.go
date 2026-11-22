package console

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The build notice (static.go, navigation.js, layout.html): a tab left open
// across an upgrade keeps its old stylesheet and scripts while the live
// refresh and the in-place loads draw the new daemon's markup into it. The
// daemon names its build on every page and patch, and the page offers the
// owner a reload when an answer came from another build. There is no JS
// harness here, so the browser half is pinned by its shape.

var buildMetaRe = regexp.MustCompile(`<meta name="seamless-build" content="([^"]*)">`)

// pageBuilds returns the build every seamless-build meta in body names.
func pageBuilds(body string) []string {
	var out []string
	for _, m := range buildMetaRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// The build id is one digest over every static file's version and the
// service worker's, so a build that changes any file a page can load
// changes it, and only such a build does.
func TestBuild_IDCoversEveryFileAPageLoads(t *testing.T) {
	h := sha256.New()
	names := make([]string, 0, len(staticFiles))
	for name := range staticFiles {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		src, err := os.ReadFile("static/" + name)
		require.NoError(t, err)
		sum := sha256.Sum256(src)
		h.Write([]byte(name + " " + hex.EncodeToString(sum[:]) + "\n"))
	}
	sw, err := os.ReadFile("static/sw.js")
	require.NoError(t, err)
	sum := sha256.Sum256(sw)
	h.Write([]byte("sw.js " + hex.EncodeToString(sum[:]) + "\n"))
	require.Equal(t, hex.EncodeToString(h.Sum(nil)), consoleBuild)

	set := func(css string) staticSet {
		return staticSet{"console.css": newStaticFile(cssType, []byte(css)), "shell.js": newStaticFile(jsType, []byte("x"))}
	}
	worker := newStaticFile(jsType, []byte("w"))
	require.Equal(t, buildID(set(".a{}"), worker), buildID(set(".a{}"), worker), "the same files are the same build")
	require.NotEqual(t, buildID(set(".a{}"), worker), buildID(set(".b{}"), worker), "a changed stylesheet is another build")
	require.NotEqual(t, buildID(set(".a{}"), worker), buildID(set(".a{}"), newStaticFile(jsType, []byte("v"))), "so is a changed worker")
}

// Every page the console draws names its build twice -- in its head and in
// its X-Seam-Build header -- and so does every patch (the live refresh's and
// the in-place loads'), the one the morph reads. The page carries the build
// notice, its card hidden; a patch carries no notice, so a morph can never
// replace or hide it.
func TestBuild_PagesAndPatchesNameTheBuild(t *testing.T) {
	_, mux := newConsole(t)
	for _, path := range []string{"/console/", "/console/memories", "/console/sessions", "/console/settings", "/console/diary", "/console/system"} {
		full := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, full.Code, path)
		require.Equal(t, consoleBuild, full.Header().Get(buildHeader), "%s: the page's header", path)
		require.Equal(t, []string{consoleBuild}, pageBuilds(full.Body.String()), "%s: the page's head", path)
		body := full.Body.String()
		require.Contains(t, body, `<div class="build-notice" id="build-notice" role="status" aria-live="polite">`, path)
		require.Contains(t, body, `<div class="build-notice-card" hidden>`, "%s: the notice starts hidden", path)

		for _, header := range []string{liveHeader, partialHeader} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+testKey)
			req.Header.Set(header, "1")
			rr := do(mux, req)
			require.Equal(t, http.StatusOK, rr.Code, "%s %s", path, header)
			require.Equal(t, consoleBuild, rr.Header().Get(buildHeader), "%s %s: the patch's header", path, header)
			require.Equal(t, []string{consoleBuild}, pageBuilds(rr.Body.String()), "%s %s: the patch's head, which the live refresh is compared by", path, header)
			require.NotContains(t, rr.Body.String(), `id="build-notice"`, "%s %s: a patch carries no notice", path, header)
		}
	}
}

// The notice's words, and its one action: a plain link to the page, which
// only the owner's click follows. It is outside the view a morph replaces.
func TestBuild_NoticeIsAPoliteStatusWithAReloadLink(t *testing.T) {
	src, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	layout := string(src)
	const notice = `<div class="build-notice" id="build-notice" role="status" aria-live="polite">
  <div class="build-notice-card" hidden>
    <p class="build-notice-copy">Seamless was updated. Reload this page to use the new version.</p>
    <a class="btn small primary" id="build-reload" href="" data-no-prefetch>Reload</a>
  </div>
</div>`
	require.Equal(t, 1, strings.Count(layout, notice))
	at := strings.Index(layout, notice)
	require.Greater(t, at, strings.Index(layout, `</main>{{end}}`), "the notice is outside the view a morph replaces")
	require.Less(t, at, strings.Index(layout, `{{define "patch"}}`), "and in the page, not the patch")

	for name, js := range map[string][]byte{
		"interactions.js": interactionsJS, "search.js": searchJS, "library.js": libraryJS, "scope.js": scopeJS, "charts.js": chartsJS,
		"navigation.js": navigationJS, "shell.js": shellJS, "sky.js": skyJS, "diary.js": diaryJS, "backups.js": backupsJS,
	} {
		require.NotContains(t, string(js), "build-reload", "%s: no script acts on the Reload link; only the owner's click does", name)
	}

	css := string(consoleCSS)
	for _, rule := range []string{
		".build-notice {", ".build-notice-card {", ".build-notice-card[hidden] { display: none; }",
		"body:has(.build-notice-card:not([hidden])) #live-toast", "body:has(.build-notice-card:not([hidden])) .main { padding-bottom:",
	} {
		require.Contains(t, css, rule, "the notice's styles: %s", rule)
	}
}

// navigation.js compares every answer it applies with the page's build: the
// header for its own loads (query controls, mutations, popstate), and the
// patch's head for a caller that hands over only the document (the live
// refresh, a page script's redraw). On a difference it reveals the notice
// and does nothing else.
func TestBuild_NavigationComparesAndOnlyReveals(t *testing.T) {
	js := string(navigationJS)
	require.Contains(t, js, "var BUILD_HEADER = '"+buildHeader+"';")
	require.Contains(t, js, `doc.querySelector('meta[name="seamless-build"]')`)
	require.Contains(t, js, "var pageBuild = buildOf(document);", "the page's build is the one its head named when it loaded")
	require.Contains(t, js, "var build = response.headers.get(BUILD_HEADER);", "load reads the answer's header")
	require.Contains(t, js, "patchPage(doc, options.source || 'query', build)", "and hands it to the morph")
	require.Contains(t, js, "function patchPage(doc, source, build) {")
	require.Contains(t, js, "noticeBuild(build || buildOf(doc));", "every morph compares, by header or by the patch's head")
	require.Equal(t, 2, strings.Count(js, "noticeBuild("), "one definition, one call: in patchPage, which every morph goes through")

	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	require.Contains(t, string(layout), "window.SeamConsole.patchPage(doc, 'live')", "the live refresh morphs through patchPage, so its patch's head is compared")

	start := strings.Index(js, "  function noticeBuild(answerBuild) {")
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(js[start:], "\n  }\n")
	require.Greater(t, end, 0)
	body := js[start : start+end]
	require.Equal(t, `  function noticeBuild(answerBuild) {
    if (!pageBuild || !answerBuild || answerBuild === pageBuild) return;
    var card = document.querySelector('#build-notice .build-notice-card');
    if (card && card.hidden) card.hidden = false;`, body, "an answer from another build only reveals the notice, once")

	for _, banned := range []string{"location.reload(", "location.replace(", "location.href =", "createElement('script')"} {
		require.False(t, strings.Contains(js, banned), "navigation.js never reloads, navigates or swaps a script on its own: %s", banned)
	}
	require.Equal(t, 1, strings.Count(js, "location.assign("), "its one navigation is to the login page, which is a sign-in, not a data update")
}

// The login and Add a passkey pages are standalone: they load no
// navigation.js and are never morphed (a fetch that lands on the login page
// is a document navigation, navigation.js and the live refresh), so there is
// no build there to compare.
func TestBuild_StandalonePagesHaveNothingToCompare(t *testing.T) {
	_, mux := newConsole(t)
	login := do(mux, httptest.NewRequest(http.MethodGet, "/console/login", nil))
	require.Equal(t, http.StatusOK, login.Code)
	require.NotContains(t, login.Body.String(), "navigation.js")

	sc := signinNewConsole(t, nil)
	browser := signinNewBrowser(t)
	for _, target := range []string{"/console/login", "/console/passkeys/add"} {
		rr := sc.do(browser, browser.request(t, http.MethodGet, target, nil))
		require.Equal(t, http.StatusOK, rr.Code, target)
		require.NotContains(t, rr.Body.String(), "navigation.js", target)
	}
	js := string(navigationJS)
	require.Contains(t, js, "if (finalURL.pathname === '/console/login') {\n        location.assign(finalURL.href);", "a load that lands on the login page navigates to it")
}
