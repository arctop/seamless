package console

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// Three events mean something now waits on the owner. Each raises a toast in
// fixed words that links to the screen that acts on it: never the event's
// own text, so an agent's words are not what the toast says, and an access
// request that arrives as its kind alone (sse.go) toasts the same. The
// screens are real routes.
func TestChToastsLinkWhatWaitsOnTheOwner(t *testing.T) {
	js := lvLiveScript(t)
	asks := lvBetween(t, js, "  var ASKS = {", "  };")
	want := map[core.EventKind]string{
		core.EventAccessRequested: "'access.requested': { text: 'An agent asks for access.', href: '/console/access', label: 'Review requests' }",
		core.EventMemoryHeld:      "'memory.held': { text: 'A write was held.', href: '/console/held', label: 'Review held writes' }",
		core.EventAgentMishap:     "'agent.mishap': { text: 'An agent reported a mishap.', href: '/console/mishaps', label: 'Review mishaps' }",
	}
	for _, line := range want {
		require.Contains(t, asks, line)
	}
	require.Len(t, regexp.MustCompile(`(?m)^    '[a-z_.]+': \{`).FindAllString(asks, -1), len(want), "these three, no more")

	routes := consoleGETRoutes(t)
	for _, href := range regexp.MustCompile(`href: '([^']+)'`).FindAllStringSubmatch(asks, -1) {
		require.Contains(t, routes, href[1], "the toast links to a screen that exists")
	}

	onmessage := lvBody(t, js, "es.onmessage = function (ev) {")
	lvInOrder(t, onmessage,
		"if (Object.prototype.hasOwnProperty.call(ASKS, k)) askToast(ASKS[k]);",
		"if (window.SEAM_NO_LIVE_REFRESH) return;") // a page that keeps its own view live still toasts
	require.NotContains(t, lvBody(t, js, "function flashToast(msg, ask) {"), "innerHTML", "the link is built, never parsed")
	lvInOrder(t, lvBody(t, js, "function flashToast(msg, ask) {"),
		"toast.textContent = msg;",
		"if (ask && location.pathname !== ask.href) {",
		"a.href = ask.href; a.textContent = ask.label;",
		"holdToast(ask ? ASK_MS : 2600);")
	require.Equal(t, 8000, lvConst(t, js, "ASK_MS"))
	require.Contains(t, lvBody(t, js, "function holdToast(ms) {"), "if (toast.matches(':hover, :focus-within')) { holdToast(1500); return; }",
		"a toast stays while the owner is reaching for its link")
	require.Contains(t, js, "function askToast(ask) { flashToast(ask.text, ask); toastHold = Date.now() + ASK_MS; }",
		"a refresh the owner taps for meanwhile does not replace it")

	css := string(consoleCSS)
	require.Contains(t, css, "#live-toast:has(a) { pointer-events: auto; }", "a linked toast takes the pointer; a plain one lets clicks through")

	// Served as written.
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/").Body.String()
	for _, line := range want {
		require.Contains(t, page, line)
	}
}
