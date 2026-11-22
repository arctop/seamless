package console

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
)

// The console's event streams are held only while their page is in sight.
// A browser opens at most six connections to a console served over plain
// http and every open stream keeps one: with six console tabs open, the
// first tab's live refresh waited for a connection forever and the page
// silently stopped updating (measured in Chrome, 2026-11-22). The repo has
// no JavaScript runner, so these tests pin the scripts' structure: which
// function does what, in what order, and that nothing else opens a stream.

// lvTemplate reads a console template's source.
func lvTemplate(t *testing.T, name string) string {
	t.Helper()
	src, err := templateFS.ReadFile("templates/" + name)
	require.NoError(t, err)
	return string(src)
}

// lvBetween returns s from the one occurrence of start through the first
// occurrence of end after it.
func lvBetween(t *testing.T, s, start, end string) string {
	t.Helper()
	require.Equal(t, 1, strings.Count(s, start), "exactly one %q", start)
	i := strings.Index(s, start)
	j := strings.Index(s[i:], end)
	require.GreaterOrEqual(t, j, 0, "%q after %q", end, start)
	return s[i : i+j+len(end)]
}

// lvLiveScript is the layout's live-refresh script, from its counters
// through the pill's click.
func lvLiveScript(t *testing.T) string {
	t.Helper()
	return lvBetween(t, lvTemplate(t, "layout.html"), "  var n = 0;", "if (pill) pill.onclick = function () { apply(true); };")
}

// lvFeedScript is the Interactions page's gap-fill and stream section.
func lvFeedScript(t *testing.T) string {
	t.Helper()
	return lvBetween(t, lvTemplate(t, "interactions.html"), "  var filling = 0;", "  } else if (liveEl) {")
}

// lvBody returns the block that head opens (head ends with its "{"), up to
// the matching "}", skipping braces inside quotes and comments. head must
// occur exactly once in src.
func lvBody(t *testing.T, src, head string) string {
	t.Helper()
	require.True(t, strings.HasSuffix(head, "{"), "head %q opens a block", head)
	require.Equal(t, 1, strings.Count(src, head), "exactly one %q", head)
	start := strings.Index(src, head) + len(head)
	depth := 1
	for i := start; i < len(src); i++ {
		switch c := src[i]; c {
		case '\'', '"':
			for i++; i < len(src) && src[i] != c; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
			} else if i+1 < len(src) && src[i+1] == '*' {
				end := strings.Index(src[i+2:], "*/")
				require.GreaterOrEqual(t, end, 0, "an unclosed comment in %q", head)
				i += end + 3
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start:i]
			}
		}
	}
	t.Fatalf("no closing brace for %q", head)
	return ""
}

// lvInOrder requires each part in s, each after the one before.
func lvInOrder(t *testing.T, s string, parts ...string) {
	t.Helper()
	at := 0
	for _, p := range parts {
		i := strings.Index(s[at:], p)
		require.GreaterOrEqual(t, i, 0, "%q, after %q, in:\n%s", p, s[:at], s)
		at += i + len(p)
	}
}

// lvConst reads a `var NAME = <int>;` from a script.
func lvConst(t *testing.T, s, name string) int {
	t.Helper()
	m := regexp.MustCompile(`var `+name+` = (\d+);`).FindAllStringSubmatch(s, -1)
	require.Len(t, m, 1, "one %s", name)
	var v int
	_, err := fmt.Sscan(m[0][1], &v)
	require.NoError(t, err)
	return v
}

// lvCallers lists the word before each call of call in s, in order.
func lvCallers(s, call string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(\S+) `+regexp.QuoteMeta(call)).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// A hidden page lets its stream go once STREAM_GRACE has passed and it is
// still hidden; a page shown again before then keeps it. Letting go leaves
// the orb and the live dots alone, so the owner never sees "disconnected"
// for a close the page made on purpose.
func TestLvLayout_AHiddenPageLetsItsStreamGoAfterTheGrace(t *testing.T) {
	js := lvLiveScript(t)

	grace := lvConst(t, js, "STREAM_GRACE")
	require.Equal(t, 10000, grace, "long enough for a quick look at another tab, short enough that six background tabs free their connections in seconds")

	require.Contains(t, js, "document.addEventListener('visibilitychange', function () { if (document.hidden) rest(); else wake(); });")

	rest := lvBody(t, js, "function rest() {")
	lvInOrder(t, rest,
		"if (hideTimer || !es) return;", // one grace at a time, and none without a stream
		"hideTimer = setTimeout(function () {",
		"hideTimer = null;",
		"if (document.hidden) letGo();", // still hidden when the grace ends
		"}, STREAM_GRACE);")

	letGo := lvBody(t, js, "function letGo() {")
	lvInOrder(t, letGo,
		"if (hideTimer) { clearTimeout(hideTimer); hideTimer = null; }",
		"if (!es) return;",
		"es.close(); es = null; missed = true;")
	for _, indicator := range []string{"setOrb", "setLive", "connected"} {
		require.NotContains(t, letGo, indicator, "a stream let go on purpose leaves the indicators as they were")
	}

	wake := lvBody(t, js, "function wake() {")
	lvInOrder(t, wake,
		"if (hideTimer) { clearTimeout(hideTimer); hideTimer = null; }", // a quick look away keeps the stream
		"if (!document.hidden) connect();")
	require.NotContains(t, wake, "catchUp", "a page shown with its stream still open missed nothing")
}

// Shown again, the page opens a stream, and only once that stream is open
// does it catch up, once: the flag is cleared before the call. The catch-up
// goes through trigger, so safe() still decides when the page may change,
// and a page that keeps its own view live (SEAM_NO_LIVE_REFRESH) is only
// told, never refreshed under its feet. Like every refresh the stream
// starts, it says nothing: only the owner's tap on the pill says "Updated".
func TestLvLayout_ShownAgainItReconnectsAndCatchesUpOnce(t *testing.T) {
	js := lvLiveScript(t)

	onopen := lvBody(t, js, "es.onopen = function () {")
	lvInOrder(t, onopen,
		"connected = true; setOrb(true); setLive(true);",
		"if (missed) { missed = false; catchUp(); }")
	require.Equal(t, 1, strings.Count(js, "catchUp();"), "the catch-up runs from onopen alone")

	// What marks a gap: a stream let go, a stream that failed, a page opened
	// out of sight. Nothing else.
	require.Equal(t, 3, strings.Count(js, "missed = true;"))
	require.Contains(t, lvBody(t, js, "es.onerror = function () {"), "connected = false; setOrb(false); setLive(false); missed = true;")
	require.Contains(t, js, "  if (document.hidden) missed = true; else connect();\n")

	catchUp := lvBody(t, js, "function catchUp() {")
	lvInOrder(t, catchUp,
		"document.dispatchEvent(new CustomEvent('seam:stream-resumed'));",
		"if (window.SEAM_NO_LIVE_REFRESH) return;",
		"trigger();")
	require.NotContains(t, catchUp, "apply(", "the catch-up waits for a safe moment like any live refresh")

	require.Contains(t, js, "function trigger() { if (safe()) apply(); else { pending = true; showPill(); } }")
	require.Contains(t, js, "debounce = setTimeout(trigger, 700);")
	// Only the pill's own refresh says "Updated": an event's, the catch-up's
	// and a deferred one's (the interval) say nothing.
	lvInOrder(t, lvBody(t, js, "function apply(manual) {"),
		"n = 0; pending = false; hidePill();",
		"if (manual === true && Date.now() >= toastHold && !actionToastUp()) flashToast('Updated');")
	require.Equal(t, 1, strings.Count(js, "flashToast('Updated')"))
	require.Equal(t, 1, strings.Count(js, "apply(true)"), "the pill alone asks for a manual refresh")
	require.Equal(t, []string{"(safe())", "safe())"}, lvCallers(js, "apply();"), "trigger and the interval refresh without a word")

	// A catch-up that has to wait says so without inventing a count.
	require.Contains(t, js, "pill.textContent = (n ? n + ' update' + (n > 1 ? 's' : '') : 'may be out of date') + ' · tap to refresh';")
}

// One stream per page, ever: the only EventSource is made in connect, past a
// guard that refuses while one is open; the visibility and back/forward
// cache handlers all go through wake and letGo; the stream is set in exactly
// two places, made and dropped.
func TestLvLayout_OneStreamAtATime(t *testing.T) {
	page := lvTemplate(t, "layout.html")
	js := lvLiveScript(t)

	require.Equal(t, 1, strings.Count(page, "new EventSource("), "one stream in the layout")
	require.Contains(t, js, "function streaming() { return !!es && es.readyState !== EventSource.CLOSED; }")
	connect := lvBody(t, js, "function connect() {")
	lvInOrder(t, connect,
		"if (streaming()) return;",
		"if (es) es.close();", // a stream the browser gave up on
		"es = new EventSource('/console/events');")

	assigns := regexp.MustCompile(`\bes = [^;]+;`).FindAllString(js, -1)
	require.Equal(t, []string{"es = null;", "es = new EventSource('/console/events');", "es = null;"}, assigns,
		"declared, made in connect, dropped in letGo")

	require.Equal(t, []string{"(!document.hidden)", "else"}, lvCallers(js, "connect();"), "connect runs from wake and at a visible start, nowhere else")

	require.Contains(t, js, "window.addEventListener('pagehide', letGo);")
	require.Contains(t, js, "window.addEventListener('pageshow', function (e) { if (e.persisted) wake(); });")
	require.NotContains(t, js, "e.persisted && !es", "wake's guard is connect's own")
}

// A live refresh can never hang: its fetch is aborted after LIVE_TIMEOUT,
// and an aborted fetch lands on the same path as any failure: busy cleared,
// the refresh pending, the pill shown. Nothing reloads or navigates the
// document but the sign-in redirect.
func TestLvLayout_ALiveRefreshCanNeverHang(t *testing.T) {
	js := lvLiveScript(t)

	require.Equal(t, 15000, lvConst(t, js, "LIVE_TIMEOUT"))
	apply := lvBody(t, js, "function apply(manual) {")
	lvInOrder(t, apply,
		"if (busy) { again = true; return; }",
		"busy = true;",
		"var stop = window.AbortController ? new AbortController() : null;",
		"var limit = stop ? setTimeout(function () { stop.abort(); }, LIVE_TIMEOUT) : null;",
		"fetch(location.href, { credentials: 'same-origin', headers: headers, signal: stop ? stop.signal : undefined })",
		".catch(function (error) {",
		"if (window.SeamConsole && window.SeamConsole.stale(error)) { again = true; return; }",
		"pending = true; showPill();",
		".finally(function () { if (limit) clearTimeout(limit); busy = false; if (again) { again = false; schedule(); } });")
	require.Equal(t, 1, strings.Count(apply, "fetch("), "one fetch, and it is the bounded one")

	require.Contains(t, js, "setInterval(function () { if (pending && !busy && safe()) apply(); }, 1500);", "a refresh given up is tried again at the next safe moment")
	for _, reload := range []string{"location.reload", "location.href =", "location.replace"} {
		require.NotContains(t, js, reload)
	}
	require.Equal(t, 1, strings.Count(js, "location.assign("), "the sign-in redirect is the one navigation")
}

// The Interactions feed keeps the same rule with the same grace: held only
// while its page is in sight, one stream at a time, let go without dimming
// its live dot, and gap-filled each time it opens.
func TestLvInteractions_TheFeedFollowsTheSameRule(t *testing.T) {
	page := lvTemplate(t, "interactions.html")
	js := lvFeedScript(t)

	require.Equal(t, lvConst(t, lvLiveScript(t), "STREAM_GRACE"), lvConst(t, js, "STREAM_GRACE"), "the layout's grace")
	require.Equal(t, 1, strings.Count(page, "new EventSource("), "one feed stream")

	connect := lvBody(t, js, "function connect() {")
	lvInOrder(t, connect,
		"if (streaming()) return;",
		"if (es) es.close();",
		"es = new EventSource('/console/events?feed=interactions');")
	lvInOrder(t, lvBody(t, js, "es.onopen = function () {"), "liveEl.classList.remove('off');", "gapFill();")

	lvInOrder(t, lvBody(t, js, "function rest() {"),
		"if (hideTimer || !es) return;",
		"hideTimer = setTimeout(function () {", "hideTimer = null;", "if (document.hidden) letGo();", "}, STREAM_GRACE);")
	letGo := lvBody(t, js, "function letGo() {")
	lvInOrder(t, letGo, "clearTimeout(hideTimer);", "if (!es) return;", "es.close(); es = null;")
	require.NotContains(t, letGo, "liveEl", "a stream let go on purpose leaves the live dot as it was")
	lvInOrder(t, lvBody(t, js, "function wake() {"), "clearTimeout(hideTimer);", "if (!document.hidden) connect();")

	start := lvBody(t, js, "if (window.EventSource) {")
	lvInOrder(t, start,
		"if (!document.hidden) connect();",
		"document.addEventListener('visibilitychange', function () { if (document.hidden) rest(); else wake(); });",
		"window.addEventListener('pagehide', letGo);",
		"window.addEventListener('pageshow', function (e) { if (e.persisted) wake(); });")
	require.Equal(t, []string{"(!document.hidden)", "(!document.hidden)"}, lvCallers(js, "connect();"), "wake and a visible start, nowhere else")
}

// The gap-fill asks for the newest rows first, back to the last row seen,
// and pages on while the server hands back a cursor and the feed has room:
// one ascending page would bring back the oldest rows missed and leave a
// hole before the newest.
func TestLvInteractions_GapFillPagesNewestFirstToTheLastRowSeen(t *testing.T) {
	js := lvFeedScript(t)
	fill := lvBody(t, js, "function gapFill() {")
	lvInOrder(t, fill,
		"if (!last) return;",
		"var mine = ++filling, since = last.ts, got = 0;",
		"var url = '/console/interactions?format=json&afterTs=' + encodeURIComponent(since);",
		"if (before) url += '&before=' + encodeURIComponent(before.id) + '&beforeTs=' + encodeURIComponent(before.ts);",
		"if (mine !== filling) return;", // a newer gap-fill ends this one
		"list.forEach(handleLive);",     // handleLive skips a row already shown
		"if (data.nextTs && data.nextId && got < maxRows) page({ ts: data.nextTs, id: data.nextId });",
		"page(null);")
	require.NotContains(t, fill, "since=", "no ascending page")
	require.Contains(t, lvTemplate(t, "interactions.html"), "if (rows[d.id] || buffered[d.id]) {", "handleLive skips a row the feed holds")
}

// The server side of that gap-fill: afterTs pages run newest first, carry a
// cursor while a page is full, and together hold every interaction at or
// after the bound once, none before it.
func TestLvInteractions_AfterTsPagesCoverTheGapOnce(t *testing.T) {
	db, mux := newConsole(t)
	rec := events.NewRecorder(db)
	ctx := context.Background()
	base := time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)
	const total = 450
	for i := range total {
		_, err := rec.Record(ctx, core.Event{
			TS: base.Add(time.Duration(i) * time.Second), Kind: core.EventToolCall,
			ProjectSlug: "demo", Payload: map[string]any{"tool": "recall", "n": i},
		})
		require.NoError(t, err)
	}
	since := core.FormatTime(base.Add(100 * time.Second)) // the last row seen: the 101st

	seen := map[string]bool{}
	var order []time.Time
	path := "/console/interactions?format=json&afterTs=" + url.QueryEscape(since)
	for pages := 0; ; pages++ {
		require.Less(t, pages, 5, "the cursor ends")
		var data interactionsData
		getJSON(t, mux, path, &data)
		for _, r := range data.Rows {
			require.False(t, seen[r.ID], "row %s once", r.ID)
			seen[r.ID] = true
			order = append(order, r.TS)
		}
		if data.NextTS == "" {
			break
		}
		path = "/console/interactions?format=json&afterTs=" + url.QueryEscape(since) +
			"&before=" + url.QueryEscape(data.NextID) + "&beforeTs=" + url.QueryEscape(data.NextTS)
	}
	require.Len(t, seen, total-100, "every row at or after the bound")
	for i := 1; i < len(order); i++ {
		require.True(t, order[i].Before(order[i-1]), "newest first")
	}
	require.Equal(t, base.Add((total-1)*time.Second), order[0], "the first page starts at the newest")
	require.Equal(t, base.Add(100*time.Second), order[len(order)-1], "and the last reaches the bound")
}

// The scripts reach the owner as written: html/template passes them through.
func TestLvServedPagesCarryTheStreamRules(t *testing.T) {
	_, mux, _ := newConsoleWithSession(t, "cc/testsess")
	for path, lines := range map[string][]string{
		"/console/": {
			"var STREAM_GRACE = 10000;",
			"function streaming() { return !!es && es.readyState !== EventSource.CLOSED; }",
			"hideTimer = setTimeout(function () { hideTimer = null; if (document.hidden) letGo(); }, STREAM_GRACE);",
			"var limit = stop ? setTimeout(function () { stop.abort(); }, LIVE_TIMEOUT) : null;",
			"document.dispatchEvent(new CustomEvent('seam:stream-resumed'));",
		},
		"/console/interactions": {
			"es = new EventSource('/console/events?feed=interactions');",
			"var url = '/console/interactions?format=json&afterTs=' + encodeURIComponent(since);",
		},
	} {
		rr := getPeek(t, mux, path)
		require.Equal(t, 200, rr.Code, path)
		body := rr.Body.String()
		for _, line := range lines {
			require.Contains(t, body, line, path)
		}
	}
}

// The pages that keep their own view live (SEAM_NO_LIVE_REFRESH) and redraw
// only on seam:event get no refresh from the layout's catch-up: each listens
// for seam:stream-resumed, or a Diary left in a background tab, or a backup
// that started while Settings was hidden, stays stale after the owner returns
// until the next event happens to arrive.
func TestLvOwnLivenessPagesRedrawOnStreamResumed(t *testing.T) {
	for name, src := range map[string][]byte{"diary.js": diaryJS, "backups.js": backupsJS} {
		s := string(src)
		require.Contains(t, s, "addEventListener('seam:event'", name+" redraws on events")
		require.Contains(t, s, "addEventListener('seam:stream-resumed'", name+" redraws when the layout's stream opens again")
	}
	require.Contains(t, lvLiveScript(t), "new CustomEvent('seam:stream-resumed')", "the layout tells those pages")
}
