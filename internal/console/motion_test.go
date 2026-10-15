package console

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cssRule is one innermost rule of the stylesheet: its selector (whitespace
// collapsed) and its declarations. Rules inside @media come out as themselves;
// a @keyframes block comes out as its percentage steps.
type cssRule struct{ selector, body string }

func cssRules(t *testing.T) []cssRule {
	t.Helper()
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(consoleCSS), "")
	var rules []cssRule
	for _, m := range regexp.MustCompile(`([^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		rules = append(rules, cssRule{strings.Join(strings.Fields(m[1]), " "), m[2]})
	}
	require.NotEmpty(t, rules)
	return rules
}

// keyframesBody is the whole body of @keyframes name.
func keyframesBody(t *testing.T, name string) string {
	t.Helper()
	css := string(consoleCSS)
	opener := "@keyframes " + name + " {"
	at := strings.Index(css, opener)
	require.NotEqual(t, -1, at, "missing %q", opener)
	depth := 1
	for i := at + len(opener); i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[at+len(opener) : i]
			}
		}
	}
	t.Fatalf("unclosed %q", opener)
	return ""
}

// An idle console draws nothing. One animation that never ends keeps a page
// producing sixty frames a second for as long as it stays open: the Overview's
// aurora drift, star twinkle, orb halo, and live-dot pings once held a 6K
// window at half a CPU core and ~12% of an M2 Ultra's GPU with nothing
// happening, and the drifting aurora alone stalled headless Chrome outright.
// Motion plays once on arrival or answers a real event, so a loop is an
// exception that names why it may loop -- and this list only shrinks.
func TestMotion_NothingLoopsUnlessItSaysWhy(t *testing.T) {
	allowed := map[string]string{
		// Loading indicators: they run only while something is in flight.
		`body:has(.main[aria-busy="true"]) .nav-progress`: "a page load is in flight",
		`.cmdk.loading .cmdk-input-row svg`:               "the palette is waiting on a search",
		// Other screens' ambient loops, older than this rule. Each should become
		// event-driven motion the way the Overview's did; none is on every page.
		`.step.doing .dot`:    "project detail: the plan step in progress",
		`.mom-cal-ember .ico`: "sessions: the momentum streak ember",
		`.mom-cal-cell.today`: "sessions: today's momentum cell",
		`.now-hot.on .ico`:    "now: the hot agent's icon",
		`.area-head-halo`:     "project detail and retrieval: the area chart's newest point",
		`.sess-state.live`:    "sessions: a live session's heartbeat",
		`.ix-empty::after`:    "interactions: the empty stream listening",
		`.ls-star`:            "login: the observatory's stars",
		`.login-sky-svg`:      "login: the observatory's drift",
		`.now-agent[data-fresh="hot"]::before, .now-agent[data-fresh="warm"]::before`: "now: light orbiting an agent mid-flight",
		`.ix-empty .empty-ico::before, .ix-empty .empty-ico::after`:                   "interactions: the empty stream listening",
	}
	loops := regexp.MustCompile(`(?:^|;)\s*animation(?:-iteration-count)?\s*:[^;]*\binfinite\b`)
	var found []string
	for _, r := range cssRules(t) {
		if loops.MatchString(r.body) {
			found = append(found, r.selector)
			_, ok := allowed[r.selector]
			require.True(t, ok, "%s loops forever: an idle console must draw nothing -- play it once, or on an event", r.selector)
		}
	}
	for selector := range allowed {
		require.Contains(t, found, selector, "%s no longer loops: drop it from the allowlist", selector)
	}
}

// The motion that answers events must not bring back the per-frame
// main-thread work it replaced: the live-dot ping used to grow a box-shadow (a
// repaint of every frame on every page), and the nav landing animated the
// icon's colour and drop-shadow, which under a busy fleet re-laid the whole
// page out on every frame. They animate transform and opacity only, which the
// compositor plays alone. shell.js clears .ping when a ring ends, by its
// keyframes' name, so the two files must agree on the names or a dot rings
// once and never again.
func TestMotion_EventRingsStayOnTheCompositor(t *testing.T) {
	prop := regexp.MustCompile(`([\w-]+)\s*:`)
	for _, name := range []string{"orb-halo", "live-ping", "sky-twinkle", "nav-land", "nav-land-glow"} {
		for _, m := range prop.FindAllStringSubmatch(keyframesBody(t, name), -1) {
			require.Contains(t, []string{"transform", "opacity"}, m[1], "@keyframes %s animates %s: use transform or opacity", name, m[1])
		}
	}

	rings := map[string]string{}
	for _, r := range cssRules(t) {
		for _, name := range []string{"orb-halo", "live-ping"} {
			if strings.Contains(r.body, "animation: "+name+" ") {
				rings[name] = r.selector
			}
		}
	}
	require.Equal(t, map[string]string{"orb-halo": "a.brand .dot.on.ping::after", "live-ping": ".live-dot.ping::after"}, rings,
		"each ring plays only while shell.js has set .ping")

	shell := string(shellJS)
	require.Contains(t, shell, `el.classList.add('ping')`)
	require.Contains(t, shell, `e.animationName === 'orb-halo' || e.animationName === 'live-ping'`)
	require.Contains(t, shell, `document.addEventListener('animationcancel', unping)`, "a ring cut short must not leave .ping behind")
}

// The layers behind every page stay viewport-sized and still. The aurora once
// sat on a pseudo-element bled 25vmax past every edge and pinned with
// will-change -- a 10080x6966 px layer (270 MB) in a 6K window. The sidebar,
// the one frame always on screen, carries no backdrop-filter: a blur re-runs on
// the GPU whenever anything behind it changes. And the flashlight's pointer
// coordinates do not inherit, or each pointer frame restyles the hovered
// panel's whole subtree -- for the sky's panel, every star.
func TestMotion_TheFrameStaysCheapToDraw(t *testing.T) {
	rules := cssRules(t)
	// Every rule for the selector, @media variants included (the sidebar has a
	// phone drawer rule), joined.
	body := func(selector string) string {
		var bodies []string
		for _, r := range rules {
			if r.selector == selector {
				bodies = append(bodies, r.body)
			}
		}
		require.NotEmpty(t, bodies, "missing rule %s", selector)
		return strings.Join(bodies, "\n")
	}

	aurora := body("body::before")
	require.Contains(t, aurora, "inset: 0;", "the aurora fills the viewport and no more")
	require.NotContains(t, aurora, "will-change")
	require.NotContains(t, aurora, "animation")

	require.NotContains(t, body(".sidebar"), "backdrop-filter")
	require.NotContains(t, body("html::after"), "animation", "the star dust holds still")

	css := string(consoleCSS)
	require.Contains(t, css, `@property --mx { syntax: "*"; inherits: false; }`)
	require.Contains(t, css, `@property --my { syntax: "*"; inherits: false; }`)
}
