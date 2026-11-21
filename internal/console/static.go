package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The console's static files, and how a browser keeps them.
//
// A page links each one through the static template func, which names the
// file's version: the hex SHA-256 of the bytes this build embeds
// (/console/static/console.css?v=<digest>). A page itself is given no
// lifetime, so each load is drawn by the running daemon and names its
// versions. A build that changes a file changes its link, so the first page
// load after an upgrade fetches the new file; and since a link names one
// set of bytes for good, a request that names the file's version is cached
// for a year without revalidating.
//
// Any other request -- no version (the service worker's importScripts, a
// hand-typed URL) or another build's (a page drawn by the daemon before an
// upgrade) -- gets the current bytes no-cache: the browser keeps the copy but
// asks again before each use, and an unchanged file answers 304 to its ETag,
// the same digest. No answer is ever kept as fresh under a version its bytes
// do not have.

// staticPrefix is the path the static files are served under.
const staticPrefix = "/console/static/"

const (
	cssType = "text/css; charset=utf-8"
	jsType  = "text/javascript; charset=utf-8"
	svgType = "image/svg+xml"

	// staticImmutable is a versioned file's lifetime: a year, and immutable,
	// so a reload does not revalidate it either.
	staticImmutable = "public, max-age=31536000, immutable"
	// staticRevalidate keeps a copy that is checked with the daemon before
	// each use.
	staticRevalidate = "no-cache"
)

// staticFile is one embedded file and its version.
type staticFile struct {
	body        []byte
	contentType string
	version     string // hex SHA-256 of body
	etag        string // version, quoted: a strong validator
}

func newStaticFile(contentType string, body []byte) *staticFile {
	sum := sha256.Sum256(body)
	v := hex.EncodeToString(sum[:])
	return &staticFile{body: body, contentType: contentType, version: v, etag: `"` + v + `"`}
}

// write answers r with f under cacheControl. The ETag rides on every answer,
// and http.ServeContent answers a matching If-None-Match with 304 (keeping
// Cache-Control, so a revalidation also refreshes the stored policy) and HEAD
// without a body.
func (f *staticFile) write(w http.ResponseWriter, r *http.Request, cacheControl string) {
	h := w.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("Cache-Control", cacheControl)
	h.Set("ETag", f.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.body))
}

// staticSet is the files under /console/static/, by name.
type staticSet map[string]*staticFile

// staticFiles is this build's set. Register routes each file by its own path,
// and every page links them through its url.
var staticFiles = staticSet{
	"console.css": newStaticFile(cssType, consoleCSS),
	"favicon.svg": newStaticFile(svgType, faviconSVG),
	// The scripts every page loads: the Interactions client (value rendering
	// and the volume histogram), the command palette, the library screens'
	// rail and in-place reader, the chart hover readout, the in-place data
	// navigation (navigation.js, which the live refresh shares) and the
	// console chrome (sidebar, drawer, shortcuts, toasts).
	"interactions.js": newStaticFile(jsType, interactionsJS),
	"search.js":       newStaticFile(jsType, searchJS),
	"library.js":      newStaticFile(jsType, libraryJS),
	"charts.js":       newStaticFile(jsType, chartsJS),
	"navigation.js":   newStaticFile(jsType, navigationJS),
	"shell.js":        newStaticFile(jsType, shellJS),
	// One page's script each: the Overview's knowledge sky, the System
	// page's orrery, the Privacy page, Settings > Backups and the Diary.
	"sky.js":     newStaticFile(jsType, skyJS),
	"orrery.js":  newStaticFile(jsType, orreryJS),
	"privacy.js": newStaticFile(jsType, privacyJS),
	"backups.js": newStaticFile(jsType, backupsJS),
	"diary.js":   newStaticFile(jsType, diaryJS),
	// The WebAuthn helpers the passkey ceremonies share, and the browser
	// key's script (browserproof), which a page loads while an unlock or a
	// sign-in needs it and the service worker imports unversioned.
	"passkey.js":    newStaticFile(jsType, passkeyJS),
	"browserkey.js": newStaticFile(jsType, browserKeyJS),
}

// url is the static template func: the link a page uses for the file called
// name, its version included. A name that is no static file fails the page's
// render: the link would be a 404.
func (set staticSet) url(name string) (string, error) {
	f, ok := set[name]
	if !ok {
		return "", fmt.Errorf("console: no static file %q", name)
	}
	return staticPrefix + name + "?v=" + f.version, nil
}

// serve answers a GET of /console/static/<name>: immutable when the request
// names the file's version, revalidated on every use otherwise.
func (set staticSet) serve(w http.ResponseWriter, r *http.Request) {
	f, ok := set[strings.TrimPrefix(r.URL.Path, staticPrefix)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	cacheControl := staticRevalidate
	if r.URL.Query().Get("v") == f.version {
		cacheControl = staticImmutable
	}
	f.write(w, r, cacheControl)
}

// serviceWorker is the console's service worker, served at /console/sw.js so
// its scope is /console/.
var serviceWorker = newStaticFile(jsType, swJS)

// serveServiceWorker serves the worker, revalidated on every update check,
// so a fixed worker reaches browsers that already run one. The scripts it
// imports are unversioned URLs, so they are revalidated too: a worker's
// imports go through the browser's HTTP cache (the registration's default
// updateViaCache, "imports"), and a copy kept fresh would let the new worker
// an upgrade installs run the old daemon's script.
func (s *Service) serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	serviceWorker.write(w, r, staticRevalidate)
}

// The build. A tab left open across an upgrade keeps the old stylesheet and
// scripts: the live refresh and the in-place loads (navigation.js) morph the
// new daemon's markup into the document they already run. So the daemon names
// its build in every page's head (the build template func) and on every page
// and patch it draws (buildHeader), and navigation.js, seeing an answer from
// another build, offers the owner a reload. It never reloads by itself
// (console-data-updates-never-reload-document).

// buildHeader names the build that drew a console page or patch.
const buildHeader = "X-Seam-Build"

// consoleBuild is this build's id: one digest over the version of every
// static file and the service worker. Two builds share it only when every
// file a page can load is the same.
var consoleBuild = buildID(staticFiles, serviceWorker)

func buildID(set staticSet, worker *staticFile) string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(set)) {
		h.Write([]byte(name + " " + set[name].version + "\n"))
	}
	h.Write([]byte("sw.js " + worker.version + "\n"))
	return hex.EncodeToString(h.Sum(nil))
}

// buildName is the build template func: the page head's seamless-build meta.
func buildName() string { return consoleBuild }
