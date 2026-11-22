package console

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// How a browser keeps the console's static files (static.go): a page links
// each one by its version, the SHA-256 of the bytes this build serves, and
// that link is cached for good; every other request for it is revalidated
// against an ETag of the same digest.

const (
	immutableCC  = "public, max-age=31536000, immutable"
	revalidateCC = "no-cache"
)

// staticLink is the link a page names a static file by.
func staticLink(t *testing.T, name string) string {
	t.Helper()
	link, err := staticFiles.url(name)
	require.NoError(t, err)
	return link
}

// staticGET serves a GET of target, with If-None-Match when etag is set.
func staticGET(mux http.Handler, target, etag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// Every route under /console/static/ serves one file of the set, the set has
// no file without a route, and each file's version is the digest of its
// source file in static/: the bytes the daemon serves.
func TestStatic_EveryRouteServesItsVersionedFile(t *testing.T) {
	_, mux := newConsole(t)
	var routed []string
	for _, p := range consoleGETRoutes(t) {
		if name, ok := strings.CutPrefix(p, "/console/static/"); ok {
			routed = append(routed, name)
		}
	}
	names := make([]string, 0, len(staticFiles))
	for name := range staticFiles {
		names = append(names, name)
	}
	slices.Sort(routed)
	slices.Sort(names)
	require.Equal(t, names, routed, "every static route serves a file of the set, and every file has a route")

	types := map[string]string{".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".svg": "image/svg+xml"}
	for _, name := range names {
		src, err := os.ReadFile("static/" + name)
		require.NoError(t, err)
		sum := sha256.Sum256(src)
		version := hex.EncodeToString(sum[:])
		link := staticLink(t, name)
		require.Equal(t, "/console/static/"+name+"?v="+version, link, name)

		rr := staticGET(mux, link, "")
		require.Equal(t, http.StatusOK, rr.Code, link)
		require.Equal(t, src, rr.Body.Bytes(), "%s serves its source file", link)
		require.Equal(t, types[path.Ext(name)], rr.Header().Get("Content-Type"), link)
		require.Equal(t, immutableCC, rr.Header().Get("Cache-Control"), link)
		require.Equal(t, `"`+version+`"`, rr.Header().Get("ETag"), link)
		require.Equal(t, "nosniff", rr.Header().Get("X-Content-Type-Options"), "%s keeps the security headers", link)

		head := httptest.NewRecorder()
		mux.ServeHTTP(head, httptest.NewRequest(http.MethodHead, link, nil))
		require.Equal(t, http.StatusOK, head.Code, link)
		require.Empty(t, head.Body.Bytes(), "HEAD %s", link)
		require.Equal(t, immutableCC, head.Header().Get("Cache-Control"), "HEAD %s", link)
	}
}

// A request that does not name the file's version -- none (the service
// worker's importScripts, a hand-typed URL), another build's (a page drawn
// before an upgrade), or an empty one -- gets the current bytes, revalidated
// on every use. A revalidation of an unchanged file is a 304 that keeps the
// policy; one of a changed file is the new bytes.
func TestStatic_AnyOtherRequestIsRevalidated(t *testing.T) {
	_, mux := newConsole(t)
	other := strings.Repeat("0", 64)
	for name, f := range staticFiles {
		base := "/console/static/" + name
		for _, target := range []string{base, base + "?v=" + other, base + "?v=", base + "?v=" + f.version[:16]} {
			rr := staticGET(mux, target, "")
			require.Equal(t, http.StatusOK, rr.Code, target)
			require.Equal(t, f.body, rr.Body.Bytes(), target)
			require.Equal(t, revalidateCC, rr.Header().Get("Cache-Control"), "%s is never kept fresh: it does not name these bytes", target)
			require.Equal(t, f.etag, rr.Header().Get("ETag"), target)
		}

		for _, etag := range []string{f.etag, "W/" + f.etag, `"x", ` + f.etag} {
			rr := staticGET(mux, base, etag)
			require.Equal(t, http.StatusNotModified, rr.Code, "%s with If-None-Match %s", base, etag)
			require.Empty(t, rr.Body.Bytes(), base)
			require.Equal(t, revalidateCC, rr.Header().Get("Cache-Control"), "a 304 keeps the policy: %s", base)
			require.Equal(t, f.etag, rr.Header().Get("ETag"), base)
		}
		rr := staticGET(mux, base, `"`+other+`"`)
		require.Equal(t, http.StatusOK, rr.Code, "%s: another build's copy is replaced", base)
		require.Equal(t, f.body, rr.Body.Bytes(), base)

		// A browser revalidating the versioned link (a forced reload in a
		// browser that ignores immutable) is answered 304 and kept for good.
		rr = staticGET(mux, staticLink(t, name), f.etag)
		require.Equal(t, http.StatusNotModified, rr.Code, name)
		require.Equal(t, immutableCC, rr.Header().Get("Cache-Control"), name)
	}
}

// The service worker and the scripts it imports have no version in their
// URLs, so they are revalidated on every use and answer 304 when unchanged.
func TestStatic_ServiceWorkerRevalidates(t *testing.T) {
	_, mux := newConsole(t)
	rr := staticGET(mux, "/console/sw.js", "")
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, swJS, rr.Body.Bytes())
	require.Equal(t, revalidateCC, rr.Header().Get("Cache-Control"))
	etag := rr.Header().Get("ETag")
	sum := sha256.Sum256(swJS)
	require.Equal(t, `"`+hex.EncodeToString(sum[:])+`"`, etag)

	rr = staticGET(mux, "/console/sw.js", etag)
	require.Equal(t, http.StatusNotModified, rr.Code)
	require.Equal(t, revalidateCC, rr.Header().Get("Cache-Control"))
	rr = staticGET(mux, "/console/sw.js?v="+hex.EncodeToString(sum[:]), "")
	require.Equal(t, revalidateCC, rr.Header().Get("Cache-Control"), "the worker is never kept fresh, whatever its URL says")
}

// After an upgrade, the next page load uses the new files: the new build's
// page links a changed file by a URL the browser has never been answered
// at, so it cannot use the copy it keeps under the old link and fetches the
// new bytes. The old link, which a page drawn before the upgrade still
// names, is answered with the new bytes revalidated, and the old copy's
// ETag no longer matches.
func TestStatic_ABuildChangeChangesTheLink(t *testing.T) {
	old := staticSet{"console.css": newStaticFile(cssType, []byte(".a { color: red; }\n"))}
	next := staticSet{"console.css": newStaticFile(cssType, []byte(".a { color: blue; }\n"))}
	oldLink, err := old.url("console.css")
	require.NoError(t, err)
	nextLink, err := next.url("console.css")
	require.NoError(t, err)
	require.NotEqual(t, oldLink, nextLink, "a changed file changes its link")

	serve := http.HandlerFunc(next.serve)
	rr := staticGET(serve, nextLink, "")
	require.Equal(t, immutableCC, rr.Header().Get("Cache-Control"))
	require.Equal(t, ".a { color: blue; }\n", rr.Body.String())

	rr = staticGET(serve, oldLink, old["console.css"].etag)
	require.Equal(t, http.StatusOK, rr.Code, "the old copy does not revalidate")
	require.Equal(t, ".a { color: blue; }\n", rr.Body.String())
	require.Equal(t, revalidateCC, rr.Header().Get("Cache-Control"), "the new bytes are never kept fresh under the old link")

	same := staticSet{"console.css": newStaticFile(cssType, []byte(".a { color: blue; }\n"))}
	sameLink, err := same.url("console.css")
	require.NoError(t, err)
	require.Equal(t, nextLink, sameLink, "an unchanged file keeps its link across builds, and its cached copy")

	_, err = next.url("console.js")
	require.Error(t, err, "a link to no static file fails the render")
	require.Equal(t, http.StatusNotFound, staticGET(serve, "/console/static/console.js", "").Code)
}

// staticRefRe finds a reference to a static file in a template's source.
var (
	staticRefRe  = regexp.MustCompile(`(?:src|href)="(/console/static/[^"]*)"`)
	staticFuncRe = regexp.MustCompile(`\{\{static "([^"]*)"\}\}`)
)

// Every template links a static file through the static func, never by a
// bare path, and names only files the set serves; every file but the
// worker's import is linked from somewhere.
func TestStatic_TemplatesLinkByVersion(t *testing.T) {
	linked := map[string]bool{}
	err := fs.WalkDir(templateFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, err := fs.ReadFile(templateFS, p)
		if err != nil {
			return err
		}
		for _, m := range staticRefRe.FindAllStringSubmatch(string(src), -1) {
			t.Errorf("%s links %s without its version: use {{static %q}}", p, m[1], strings.TrimPrefix(m[1], "/console/static/"))
		}
		for _, m := range staticFuncRe.FindAllStringSubmatch(string(src), -1) {
			_, ok := staticFiles[m[1]]
			require.True(t, ok, "%s links %q, which is no static file", p, m[1])
			linked[m[1]] = true
		}
		return nil
	})
	require.NoError(t, err)
	for name := range staticFiles {
		require.True(t, linked[name], "%s is linked from no template", name)
	}
}

// pageStaticLinks returns every static URL a rendered page links in a src or
// href attribute.
func pageStaticLinks(body string) []string {
	var out []string
	for _, m := range staticRefRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// requireVersionedPage checks a rendered page: it links the wanted files,
// every link names this build's version and is answered immutable, and the
// page itself is never kept fresh, so its next load names the next build's
// versions.
func requireVersionedPage(t *testing.T, mux http.Handler, page string, rr *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, page)
	require.NotContains(t, rr.Header().Get("Cache-Control"), "max-age", "%s is never kept fresh", page)
	require.Empty(t, rr.Header().Get("Expires"), page)
	require.Empty(t, rr.Header().Get("Last-Modified"), "%s gives no heuristic lifetime", page)
	links := pageStaticLinks(rr.Body.String())
	for _, name := range want {
		require.Contains(t, links, staticLink(t, name), "%s links %s by its version", page, name)
	}
	for _, link := range links {
		name, _, _ := strings.Cut(strings.TrimPrefix(link, "/console/static/"), "?")
		require.Equal(t, staticLink(t, name), link, "%s links this build's version", page)
		require.Equal(t, immutableCC, staticGET(mux, link, "").Header().Get("Cache-Control"), "%s: %s", page, link)
	}
}

// The console's pages, the login and sign-in pages included (served before
// a session exists), link this build's versions.
func TestStatic_PagesLinkThisBuildsVersions(t *testing.T) {
	_, mux := newConsole(t)
	layout := []string{"favicon.svg", "console.css", "interactions.js", "search.js", "library.js", "scope.js", "charts.js", "navigation.js", "shell.js", "panel.js"}

	req := httptest.NewRequest(http.MethodGet, "/console/", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	requireVersionedPage(t, mux, "/console/", do(mux, req), append(layout, "sky.js")...)

	requireVersionedPage(t, mux, "key login", do(mux, httptest.NewRequest(http.MethodGet, "/console/login", nil)),
		"favicon.svg", "console.css")

	sc := signinNewConsole(t, nil)
	browser := signinNewBrowser(t)
	requireVersionedPage(t, sc.mux, "passkey login", sc.do(browser, browser.request(t, http.MethodGet, "/console/login", nil)),
		"favicon.svg", "console.css", "browserkey.js", "passkey.js")
	requireVersionedPage(t, sc.mux, "add a passkey", sc.do(browser, browser.request(t, http.MethodGet, "/console/passkeys/add", nil)),
		"favicon.svg", "console.css", "passkey.js")
}
