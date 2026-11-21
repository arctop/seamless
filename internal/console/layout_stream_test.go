package console

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A page in the browser's back/forward cache lets its event stream go: six
// held streams fill the browser's connections to the console and the next
// page load waits forever (measured in Chrome over CDP: a tab stalled on its
// seventh console page). The stream closes on pagehide and opens again on a
// pageshow from the cache, in the layout and in the Interactions feed.
func TestLayout_TheEventStreamLetsTheBackForwardCacheGo(t *testing.T) {
	for name, open := range map[string]string{
		"templates/layout.html":       "new EventSource('/console/events')",
		"templates/interactions.html": "new EventSource('/console/events?feed=interactions')",
	} {
		src, err := templateFS.ReadFile(name)
		require.NoError(t, err)
		s := string(src)
		require.Equal(t, 1, strings.Count(s, "new EventSource("), "%s: one stream", name)
		require.Contains(t, s, "function connect() {\n    es = "+open, name)
		require.Regexp(t, `window\.addEventListener\('pagehide', function \(\) \{ if \(es\) \{ es\.close\(\); es = null;`, s, name)
		require.Contains(t, s, "window.addEventListener('pageshow', function (e) { if (e.persisted && !es) connect(); });", name)
	}
}

// Opening the detail pane caps the list into a scroll box that starts at its
// top; the item just clicked is put back where it was on screen, measured
// before the pane opens and restored after.
func TestLayout_APeekKeepsTheClickedItemInPlace(t *testing.T) {
	src, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	s := string(src)
	keep := strings.Index(s, "var keep = p.hidden ? anchorOf(lastTrigger) : null;")
	open := strings.Index(s, "s.classList.add('detail-open');\n      restore(keep);")
	require.Positive(t, keep, "the clicked item's place is noted while the pane is still closed")
	require.Greater(t, open, keep, "and put back once the pane has opened")
	require.Contains(t, s, "var lp = el.closest('.list-pane');")
	require.Contains(t, s, "k.lp.scrollTop += r.top - Math.max(lo, Math.min(k.top, hi));")
}
