package console

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Read in place (plan:console-ux-pass UX 31): static/panel.js opens an item
// link's ?peek=1 fragment in one wide panel over the page. The repo has no
// JavaScript runner, so these tests pin the script's structure: which clicks
// and keys it takes, which it leaves alone, the history entries it adds, and
// where its markup sits in the layout. pn_walk_test.go walks the rendered
// pages of the review fixture.

// pnJS is panel.js's source.
func pnJS() string { return string(panelJS) }

// pnListener returns the body of the one listener head opens in src and
// whether it is registered in the capture phase (its call ends ", true);").
func pnListener(t *testing.T, src, head string) (string, bool) {
	t.Helper()
	body := lvBody(t, src, head)
	after := src[strings.Index(src, head)+len(head)+len(body):]
	switch {
	case strings.HasPrefix(after, "}, true);"):
		return body, true
	case strings.HasPrefix(after, "});"):
		return body, false
	}
	t.Fatalf("listener %q ends with neither %q nor %q: %.20q", head, "});", "}, true);", after)
	return "", false
}

// The panel is drawn by the layout, outside the view a morph replaces and
// before the script that drives it, and never in a patch answer.
func TestPnPanelSitsOutsideTheView(t *testing.T) {
	src := lvTemplate(t, "layout.html")
	head := `<section class="read-panel" id="read-panel" role="dialog" aria-label="Read in place" tabindex="-1" hidden>`
	require.Equal(t, 1, strings.Count(src, head), "one labelled dialog region, hidden until a link opens it")
	at := strings.Index(src, head)
	require.Greater(t, at, strings.Index(src, "</main>{{end}}"), "outside the view a live morph replaces")
	require.Less(t, at, strings.Index(src, `<script src="{{static "panel.js"}}"></script>`), "drawn before panel.js runs")
	require.Less(t, at, strings.Index(src, `{{define "patch"}}`), "in the page, never in a patch")
	require.Contains(t, src, `<div class="read-scrim" id="read-scrim" hidden></div>`)
	panel := src[at : at+strings.Index(src[at:], "</section>")]
	for _, part := range []string{
		`<p class="read-panel-where" data-panel-where></p>`,
		`data-panel-step="-1" aria-label="Previous item" aria-keyshortcuts="k"`,
		`data-panel-step="1" aria-label="Next item" aria-keyshortcuts="j"`,
		`<a class="read-panel-btn read-panel-full" data-panel-full data-no-panel>Open full page`,
		`data-panel-close aria-label="Close and go back" aria-keyshortcuts="Escape"><kbd>Esc</kbd>Close</button>`,
		`<div class="read-panel-body" id="read-panel-body"></div>`,
	} {
		require.Contains(t, panel, part)
	}
	require.NotContains(t, panel, `href=`, "Open full page gets its href when an item opens")

	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/").Body.String()
	require.Equal(t, 1, strings.Count(page, `id="read-panel"`))
	require.Greater(t, strings.Index(page, `id="read-panel"`), strings.Index(page, "</main>"))
	for _, header := range []string{liveHeader, partialHeader} {
		req := httptest.NewRequest(http.MethodGet, "/console/", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set(header, "1")
		rr := do(mux, req)
		require.Equal(t, http.StatusOK, rr.Code)
		require.NotContains(t, rr.Body.String(), "read-panel", "%s: a patch carries no panel, so no morph can touch it", header)
	}
}

// RULES contract 4: the layout's script tags stay as the pre-wave commit
// registered them, panel.js right after shell.js. The order is load-bearing:
// every script that cancels a click or takes Esc first registers before
// panel.js does.
func TestPnLayoutScriptOrderKeepsItsBytes(t *testing.T) {
	src := lvTemplate(t, "layout.html")
	const scripts = `<script src="{{static "interactions.js"}}"></script>
<script src="{{static "search.js"}}"></script>
<script src="{{static "library.js"}}"></script>
<script src="{{static "scope.js"}}"></script>
<script src="{{static "charts.js"}}"></script>
<script src="{{static "navigation.js"}}"></script>
<script src="{{static "shell.js"}}"></script>
<script src="{{static "panel.js"}}"></script>
<script>
`
	require.Equal(t, 1, strings.Count(src, scripts))
	require.Equal(t, 1, strings.Count(src, `<div id="live-toast" role="status" aria-live="polite" hidden></div>`))
}

// RULES contract 1: window.SeamPanel's four members, and leave records the
// way back before it navigates.
func TestPnSeamPanelContract(t *testing.T) {
	js := pnJS()
	require.Equal(t, 1, strings.Count(js, "window.SeamPanel = { isOpen: isOpen, open: open, close: close, leave: leave };"))
	require.Contains(t, js, "function isOpen() { var p = panel(); return !!(p && !p.hidden); }")
	require.Contains(t, js, "  function leave(href) {\n    rememberWayBack(href);\n    location.href = href;\n  }")
	open := lvBody(t, js, "function open(href, opener) {")
	lvInOrder(t, open,
		"var url = itemURL(href);",
		"if (!url || !panel()) return false;",
		"if (opener && inPanel(opener)) {",
		"return show(h, 'push');",
		"list = listFrom(opener, h);",
		"return show(h, wasOpen ? 'replace' : 'push');")
	for _, banned := range []string{"location.reload(", "location.replace(", "build-reload"} {
		require.NotContains(t, js, banned)
	}
}

// pnItem is panel.js's item pattern, compiled the same way in Go.
func pnItem(t *testing.T) *regexp.Regexp {
	t.Helper()
	m := regexp.MustCompile(`var ITEM = /(.+)/;`).FindAllStringSubmatch(pnJS(), -1)
	require.Len(t, m, 1)
	return regexp.MustCompile(m[0][1])
}

// The links the panel reads: an item's own path, of a kind with a ?peek=1
// fragment. A project link goes to the project; a sub-route, a list or the
// event stream is never an item.
func TestPnReadsTheItemKindsOnly(t *testing.T) {
	item := pnItem(t)
	for path, reads := range map[string]bool{
		"/console/memories/01M7W4E7NMFYPK6RT63ZCBRRZC":            true,
		"/console/notes/01M7CSFS9M52S4SYJ76QFK7B40":               true,
		"/console/tasks/01M7CWXMXMQ234P3FYZBXR5ZXB":               true,
		"/console/plans/ops-dns-cutover":                          true,
		"/console/sessions/01M81BGXDMBN42FMSSS3E2YNBH":            true,
		"/console/events/01M81BGXDMBN42FMSSS3E2YNBH":              true,
		"/console/trials/01M81BGXDMBN42FMSSS3E2YNBH":              true,
		"/console/projects/example-ops":                           false,
		"/console/sessions/01M81BGXDMBN42FMSSS3E2YNBH/transcript": false,

		"/console/events":                              false,
		"/console/memories":                            false,
		"/console/memories/":                           false,
		"/console/plans/ops-dns-cutover/approve":       false,
		"/console/gardener/01M81BGXDMBN42FMSSS3E2YNBH": false,
		"/console/labs/storage":                        false,
		"/console/settings":                            false,
	} {
		require.Equal(t, reads, item.MatchString(path), path)
	}
	// Every kind with a peek fragment is decided: read in the panel, or (a
	// project) left to its page. A new fragment kind fails here until it is.
	kinds := regexp.MustCompile(`\(([a-z|]+)\)`).FindStringSubmatch(item.String())
	require.Len(t, kinds, 2)
	read := strings.Split(kinds[1], "|")
	for _, name := range peekNames {
		plural := map[string]string{"memory": "memories", "note": "notes", "task": "tasks", "project": "projects",
			"session": "sessions", "event": "events", "plan": "plans", "trial": "trials"}[name]
		require.NotEmpty(t, plural, "peek kind %s has a decision", name)
		require.Equal(t, name != "project", slices.Contains(read, plural), "%s: read in the panel unless it is a project", name)
	}
	require.Contains(t, pnJS(), "function itemURL(href) {\n    if (!href || href.charAt(0) === '#') return null;",
		"an in-page #anchor is never an item, even on an item's own page")
}

// RULES contract 2: one bubble-phase click listener, leaving alone what
// another handler took, a modified or non-main click, a click in a form or
// on a button or copy button, the in-place parts, and the links that ask for
// the full page (which record the way back first).
func TestPnClicksLeaveTheirOwnersAlone(t *testing.T) {
	js := pnJS()
	require.Contains(t, js, "var FULL = '[data-no-panel], .dossier-open';")
	require.Contains(t, js, "var INPLACE = '[data-jump], [data-open-step], .list-detail-split';")
	body, capture := pnListener(t, js, "document.addEventListener('click', function (e) {")
	require.False(t, capture, "the bubble phase: every in-place handler loaded before panel.js cancels its click first")
	lvInOrder(t, body,
		"if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;",
		"if (isOpen()) {",
		"t.closest('#read-scrim, #read-panel [data-panel-close]')) { e.preventDefault(); close(); return; }",
		"var step = t.closest('#read-panel [data-panel-step]');",
		"var a = t.closest('a[href]');",
		"if (!a || a.target || a.hasAttribute('download')) return;",
		"var url = itemURL(a.getAttribute('href'));",
		"if (!url || t.closest('form, button, .copy-btn')) return;",
		"if (a.closest(FULL)) { rememberWayBack(url.href); return; }",
		"if (a.closest(INPLACE)) return;",
		"if (!isOpen() && pathOf(url) === location.pathname + location.search) return;",
		"e.preventDefault();",
		"open(url.href, a);")
	require.Equal(t, 1, strings.Count(js, "e.preventDefault();\n    open(url.href, a);"), "the one place a link click becomes the panel")
	// A whole row reads in the panel too, through the API, unless the row is
	// the detail split's; anything the panel does not read navigates.
	layout := lvTemplate(t, "layout.html")
	rows := lvBetween(t, layout, "/* Whole-row clicks:", "location.href = href;\n  });")
	lvInOrder(t, rows,
		"if (e.defaultPrevented) return;",
		"if (!row || row.closest('.list-detail-split')) return;",
		"if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;",
		"if (e.target.closest('a, button, form')) return;",
		"if (window.SeamPanel && window.SeamPanel.open(href, row)) return;",
		"location.href = href;")
	require.NotContains(t, layout, "if (document.querySelector('.list-detail-split')) return;\n  document.addEventListener('click'",
		"rows on every screen without the split read in place")
}

// RULES contract 3: a capture-phase key listener that, while the panel is
// open, takes Esc, j and k only, and leaves the key to whatever holds it.
func TestPnKeysTakeEscJAndKOnly(t *testing.T) {
	js := pnJS()
	body, capture := pnListener(t, js, "document.addEventListener('keydown', function (e) {")
	require.True(t, capture, "the capture phase, after search.js's and the shell's own capture listeners")
	lvInOrder(t, body,
		"if (!isOpen() || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return;",
		"if (e.key !== 'Escape' && e.key !== 'j' && e.key !== 'k') return;",
		"if (typing(e.target) || othersHoldKeys()) return;",
		"e.preventDefault();",
		"e.stopPropagation();",
		"if (e.key === 'Escape') close();",
		"else if (!e.repeat) stepBy(e.key === 'j' ? 1 : -1);")
	holds := lvBody(t, js, "function othersHoldKeys() {")
	for _, part := range []string{"var ids = ['cmdk', 'keys'];", "hasAttribute('data-drawer')", "querySelector('.chord-hint')", "querySelector('button[data-armed]')"} {
		require.Contains(t, holds, part, "the sheet, the drawer, a g-chord and an armed confirm let go of Esc in the bubble phase")
	}
	// The palette closes on Esc first, in its own capture listener, and
	// cancels the key: the panel then leaves it alone.
	search := string(searchJS)
	require.Contains(t, search, "if (isOpen() && e.key === 'Escape') {\n      e.preventDefault();\n      e.stopPropagation();\n      close();")
	// The Diary's day keys stand down while the panel is open.
	require.Contains(t, string(diaryJS), "if (window.SeamPanel && window.SeamPanel.isOpen()) return;")
}

// History: opening adds one #peek= entry, a link inside the panel one more,
// k and j replace theirs; close goes back through all of them, Back shows
// what the entry names, and a reload reopens it without adding one.
func TestPnHistoryEntries(t *testing.T) {
	js := pnJS()
	require.Contains(t, js, "var HASH = '#peek=';")
	require.Contains(t, js, "function hashFor(href) { return location.pathname + location.search + HASH + encodeURIComponent(href); }")
	show := lvBody(t, js, "function show(href, how) {")
	require.Contains(t, show, "if (how === 'push') history.pushState(stateFor(href, depth() + 1), '', hashFor(href));")
	require.Contains(t, show, "else if (how === 'replace') history.replaceState(stateFor(href, depth()), '', hashFor(href));")
	require.Equal(t, 1, strings.Count(js, "history.pushState("), "show alone adds an entry")
	require.Contains(t, lvBody(t, js, "function stepBy(d) {"), "show(list[i], 'replace');", "k / j replace the entry")
	require.Contains(t, lvBody(t, js, "function depth() {"), "return st && st.seamPanel ? (st.seamPanelDepth || 0) : 0;")
	closeFn := lvBody(t, js, "function close() {")
	lvInOrder(t, closeFn, "var d = depth();", "if (d > 0) {", "closing = true;", "history.go(-d);", "strip();", "teardown();")
	pop := lvBody(t, js, "window.addEventListener('popstate', function () {")
	lvInOrder(t, pop, "var h = st && st.seamPanel ? st.seamPanel : '';", "if (closing) {", "teardown();",
		"if (!h && !splitPage()) h = hashHref();", "show(h, 'none');", "if (isOpen()) teardown();")
	adopt := lvBody(t, js, "(function adopt() {")
	lvInOrder(t, adopt, "history.replaceState(stateFor(h, 0), '', location.href);", "show(h, 'none');")
	// The Diary's and Interactions' pane leaves the panel's entries alone.
	layout := lvTemplate(t, "layout.html")
	require.Contains(t, layout, "function panelEntry() { return !!(history.state && history.state.seamPanel); }")
	require.Contains(t, layout, "window.addEventListener('popstate', function () {\n    if (panelEntry()) return;")
	require.Contains(t, layout, "var m = panelEntry() ? null : location.hash.match(/^#peek=(.+)$/);")
	require.Contains(t, layout, "if (peek && !peek.closest('.list-detail-split')) peek = null;")
}

// The way back: the page left for an item's own page is recorded in this
// tab's sessionStorage, and the item page's back link names it.
func TestPnTheItemPageNamesTheWayBack(t *testing.T) {
	js := pnJS()
	require.Contains(t, js, "var BACK_KEY = 'seamless-way-back';")
	remember := lvBody(t, js, "function rememberWayBack(to) {")
	lvInOrder(t, remember, "var url = itemURL(to);", "var title = pageTitle();",
		"if (here.hash.indexOf(HASH) === 0) here.hash = '';",
		"sessionStorage.setItem(BACK_KEY, JSON.stringify({ href: here.pathname + here.search + here.hash, title: title.slice(0, 80), to: url.pathname }));")
	require.Contains(t, js, "function pageTitle() { return (document.title || '').replace(/\\s*·\\s*Seamless$/, '').trim(); }")
	apply := lvBody(t, js, "function applyWayBack() {")
	lvInOrder(t, apply, "document.querySelector('.main a[data-item-back]')", "if (rec.to !== location.pathname) {",
		"a.setAttribute('href', rec.href);", "label.textContent = 'Back to ' + rec.title;")
	require.Contains(t, lvBody(t, js, "function wayBack() {"), "if (rec.href.indexOf('/console/') !== 0 || rec.href.indexOf('/console//') === 0 || !rec.title) return null;",
		"only a console path is ever a way back")
	require.Contains(t, js, "  applyWayBack();\n", "on load")
	updated := lvBody(t, js, "document.addEventListener('seam:content-updated', function () {")
	require.Contains(t, updated, "applyWayBack();", "and after every morph, which resets the link")
	// RULES contract 8: the frame keeps the attribute the way back is found by.
	frame := lvTemplate(t, "item_shared.html")
	require.Contains(t, frame, "data-item-back>")
}

// Actions inside the panel post their own form (navigation.js posts only the
// ones inside .main) under the follow contract, and draw the item again.
func TestPnActionsInsideThePanel(t *testing.T) {
	js := pnJS()
	body, capture := pnListener(t, js, "document.addEventListener('submit', function (e) {")
	require.False(t, capture, "after the shell's confirm, which cancels an unconfirmed post in the capture phase")
	lvInOrder(t, body,
		"if (e.defaultPrevented || !form || !inPanel(form)) return;",
		"if (method !== 'post') return;",
		"e.preventDefault();",
		"if (target.origin !== location.origin) return;",
		"headers: { Accept: 'text/html', 'X-Seam-Follow': '1' }",
		"var named = r.headers.get('X-Seam-Location');",
		"window.SeamConsole.flash(problem || notice, problem ? 'error' : 'ok');",
		"return load(acted, true).then(function (answer) {",
		"if (answer && answer.status === 404 && current === acted) close();")
	require.NotContains(t, body, "patchPage", "the page behind is never morphed by the panel")
	require.NotContains(t, body, "SeamConsole.load", "nor loaded")
	require.Contains(t, string(navigationJS), "method === 'post' && !!form.closest('.main')", "navigation.js leaves the panel's forms to it")
}

// The page behind keeps its live refresh while the panel is open (the panel
// is outside the morph), and the panel catches up when the stream comes back.
func TestPnTheLiveRefreshGoesOnBehindThePanel(t *testing.T) {
	live := lvLiveScript(t)
	require.Contains(t, live, "function safe() { return !document.hidden && !fieldFocused() && !selecting() && !paneOpen() && !cmdkOpen(); }")
	require.Contains(t, live, "function paneOpen() { return !!document.querySelector('.list-detail-split.detail-open'); }")
	require.NotContains(t, live, "SeamPanel")
	require.NotContains(t, live, "read-panel")
	js := pnJS()
	resumed := lvBody(t, js, "document.addEventListener('seam:stream-resumed', function () {")
	require.Contains(t, resumed, "if (isOpen() && current) load(current, true);")
	require.Contains(t, lvBody(t, js, "document.addEventListener('seam:content-updated', function () {"), "markRow();")
	require.NotContains(t, js, "new EventSource(", "no stream of its own")
}
