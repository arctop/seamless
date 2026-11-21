package console

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A service worker's imports go through the browser's HTTP cache (the
// registration's default updateViaCache, "imports"), so a script the worker
// imports and the console serves with a lifetime would reach the new worker
// an upgrade installs as the old daemon's copy for that long: the worker
// would sign with the old browserkey.js. Every script sw.js imports is served
// no-cache, as the worker itself is. That holds because the worker imports
// them by a URL with no version: a page links every static file by its
// version (static.go), and that link is kept for good, while an unversioned
// request for any static file is revalidated on every use.
func TestSKStatic_WorkerImportsAreRevalidated(t *testing.T) {
	_, mux := newConsole(t)
	header := func(path string) string {
		t.Helper()
		rr := do(mux, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rr.Code, path)
		return rr.Header().Get("Cache-Control")
	}

	imports := map[string]bool{}
	for _, m := range regexp.MustCompile(`importScripts\(([^)]*)\)`).FindAllStringSubmatch(string(swJS), -1) {
		for _, arg := range strings.Split(m[1], ",") {
			url := strings.Trim(strings.TrimSpace(arg), `'"`)
			require.True(t, strings.HasPrefix(url, "/console/static/"), "the worker imports only the console's own static files: %q", url)
			imports[url] = true
		}
	}
	require.Equal(t, map[string]bool{"/console/static/browserkey.js": true}, imports, "the imports this test knows")

	require.Equal(t, "no-cache", header("/console/sw.js"))
	for url := range imports {
		require.Equal(t, "no-cache", header(url), "%s: the worker imports it", url)
	}
	statics := 0
	for _, path := range consoleGETRoutes(t) {
		if !strings.HasPrefix(path, "/console/static/") {
			continue
		}
		statics++
		require.Equal(t, "no-cache", header(path), "%s: an unversioned request is revalidated", path)
		require.Equal(t, "public, max-age=31536000, immutable", header(staticLink(t, strings.TrimPrefix(path, "/console/static/"))),
			"%s: a page's versioned link is kept for good", path)
	}
	require.Greater(t, statics, 10, "the static files were found")
}

// After an upgrade the worker still running is the old one. The page asks the
// console for the worker again before it unlocks or signs in, and before it
// fetches a page drawn without a proof again, and waits while a newer worker
// takes the page; it never reloads the document to get there
// (console-data-updates-never-reload-document). There is no JS harness here,
// so this pins the shape: the browser behavior is the manual smoke's.
func TestSKBrowserKey_WaitsForTheNewestWorker(t *testing.T) {
	key := string(browserKeyJS)
	require.Contains(t, key, "reg.update()", "the page asks for the worker again")
	require.Contains(t, key, "sw.addEventListener('controllerchange', changed)", "and waits for a newer one to take the page")
	require.Contains(t, key, "reg.installing || reg.waiting", "while one installs or waits")
	require.Contains(t, key, "reg.active.state === 'activating'", "or activates")
	require.Equal(t, 2, strings.Count(key, "current(reg, WAIT)"), "ready() and recover() both wait for the newest worker")
	require.Contains(t, key, "setTimeout(function () { settle(false); }, ms)", "the wait is bounded")
	require.Contains(t, key, `if (!ok) throw new Error('The console\'s service worker did not start in this browser: reload the page and try again.');`,
		"a wait that runs out says so in the same words")
	require.Contains(t, key, "window.SeamConsole.load(location.href, { history: 'replace', source: 'query' })", "recover fetches the view in place")
	for name, src := range map[string]string{"browserkey.js": key, "sw.js": string(swJS)} {
		for _, banned := range []string{"location.reload(", "location.href ="} {
			require.NotContains(t, src, banned, "%s never reloads or navigates the document", name)
		}
	}
	sw := string(swJS)
	require.Contains(t, sw, "self.skipWaiting()", "a new worker does not wait for the old one's pages to close")
	require.Contains(t, sw, "self.clients.claim()", "and takes the open pages, which is the controllerchange the page waits for")
}
