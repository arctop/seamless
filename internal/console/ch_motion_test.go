package console

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// chHeadScript is the layout's first inline script: the one that runs before
// first paint.
func chHeadScript(t *testing.T) string {
	t.Helper()
	return lvBetween(t, lvTemplate(t, "layout.html"), "(function(){var d=document.documentElement;", "})();")
}

// The entrance (the page settling in, its numbers counting up) plays once per
// browser tab, on the first page the tab shows (decision D6): html.reveal is
// set only when this tab's sessionStorage has no mark yet, and the mark is
// set first, so a tab whose storage refuses never plays it rather than
// playing it on every page. Every animation of it hangs off html.reveal, the
// count-up included, and reduced motion still stills it.
func TestChEntrancePlaysOncePerTab(t *testing.T) {
	head := chHeadScript(t)
	require.Contains(t, head, "d.classList.add('js');")
	require.Contains(t, head, "try{if(!sessionStorage.getItem('seamless-arrived')){sessionStorage.setItem('seamless-arrived','1');d.classList.add('reveal');}}catch(e){}")
	require.Equal(t, 1, strings.Count(lvTemplate(t, "layout.html"), "'reveal'"), "the head script alone sets it")
	// The theme, sidebar and sky still apply before paint, after it.
	require.Contains(t, head, "if(localStorage.getItem('seamless-sky')==='folded')d.setAttribute('data-sky','folded');")

	shell := string(shellJS)
	require.Contains(t, shell, "if (root.classList.contains('reveal')) countUp();", "the count-up plays with the entrance only")
	require.Contains(t, lvBody(t, shell, "function countUp() {"), "if (calm) return;", "reduced motion: numbers render at their value")
	require.Contains(t, shell, "setTimeout(function () { root.classList.remove('reveal'); }, 1100);")
	require.NotContains(t, shell, "classList.add('reveal')", "shell.js only ever removes it")
	require.NotContains(t, string(navigationJS), "'reveal'", "an in-place load never replays it")

	// Every rule that plays the entrance on a page's view (the rise, the sky's
	// stars) hangs off html.reveal, and the global reduced-motion rule still
	// cuts every animation short. (The rise also plays where an owner opens
	// something, the phone filters and the settings save bar, and on the
	// login page, which is not drawn in this layout.)
	var rises int
	for _, r := range cssRules(t) {
		if !strings.Contains(r.body, "rise-in") && !strings.Contains(r.body, "sky-rise") {
			continue
		}
		for _, sel := range strings.Split(r.selector, ",") {
			sel = strings.TrimSpace(sel)
			if strings.Contains(sel, ".main") || strings.Contains(sel, ".sky-stars") {
				rises++
				require.True(t, strings.HasPrefix(sel, ".reveal "), "%s plays the entrance outside html.reveal", sel)
			}
		}
	}
	require.GreaterOrEqual(t, rises, 2)
	require.Regexp(t, regexp.MustCompile(`@media \(prefers-reduced-motion: reduce\) \{\s*\*, \*::before, \*::after \{ animation-duration: 0\.001ms !important;`), string(consoleCSS))

	// The page as served: the mark is the browser's, never the server's.
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, "sessionStorage.setItem('seamless-arrived','1');d.classList.add('reveal');")
	require.NotContains(t, page, `class="reveal`)
}
