package console

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// The shared parts of Settings and the lock screens (UX 33, kit.go,
// templates/kit_shared.html, shell.js and navigation.js).

// A hint escapes its words, names its tooltip with aria-describedby, and
// carries a name: "About this", or "About <label>" for hintFor.
func TestKtHint_EscapesAndNamesItsTooltip(t *testing.T) {
	out := string(hint(`Saves <b>now</b> & "always"`))
	id := hintID("", `Saves <b>now</b> & "always"`)
	require.Regexp(t, `^hint-[0-9a-f]{8}$`, id)
	require.Equal(t, `<span class="hint"><button type="button" class="hint-btn" aria-label="About this" aria-describedby="`+id+`">`+
		string(icon("info"))+`</button><span class="hint-pop" id="`+id+`" role="tooltip">Saves &lt;b&gt;now&lt;/b&gt; &amp; &#34;always&#34;</span></span>`, out)
	require.NotContains(t, out, "<b>", "the words are text, never markup")

	named := string(hintFor(`Momentum <beta>`, "Counts streaks."))
	require.Contains(t, named, `aria-label="About Momentum &lt;beta&gt;"`)
	require.Contains(t, named, `aria-describedby="`+hintID(`Momentum <beta>`, "Counts streaks.")+`"`)
	require.NotEqual(t, hintID("A", "words"), hintID("B", "words"), "a label is part of the id")
	require.Equal(t, hintID("", "x"), hintID("", "x"), "the id is the words', the same on every render")
}

// ktRenderPage renders body as a page through the console's own render
// path (the layout and the patch both), with kit_shared.html beside it.
func ktRenderPage(t *testing.T, svc *Service, body string, partial bool) string {
	t.Helper()
	page := template.Must(template.Must(template.New("layout").Funcs(funcs).ParseFS(templateFS, "templates/kit_shared.html")).
		Parse(`{{define "layout"}}<!doctype html><html><head><title>t</title></head><body><main class="main">` + body +
			`</main></body></html>{{end}}{{define "patch"}}<main class="main">` + body + `</main>{{end}}`))
	svc.pages["kt-page"] = page
	req := authedReq(httptest.NewRequest(http.MethodGet, "/console/kt", nil))
	if partial {
		req.Header.Set(partialHeader, "1")
	}
	rr := httptest.NewRecorder()
	svc.render(rr, req, "kt-page", pageData{Title: "t"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// ktIDs parses a page and returns every element id (with its count) and every
// aria-describedby pointer.
func ktIDs(t *testing.T, page string) (ids map[string]*html.Node, dupes []string, described []string) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	require.NoError(t, err)
	ids = map[string]*html.Node{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				switch a.Key {
				case "id":
					if ids[a.Val] != nil {
						dupes = append(dupes, a.Val)
					}
					ids[a.Val] = n
				case "aria-describedby":
					described = append(described, a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return ids, dupes, described
}

func ktAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// Two hints with the same words on one page still get two ids, and each
// button describes itself by its own tooltip, on the full page and on the
// patch the in-place client fetches, the same on every render. Two source
// chips with the same note do too.
func TestKtHint_TwoEqualHintsOnOnePageAreStillValid(t *testing.T) {
	_, svc := newTestService(t)
	body := `<p>One {{hint "Same words."}}</p><p>Two {{hint "Same words."}}</p><p>Three {{hint "Same words."}}</p>` +
		`<p>{{hintFor "Momentum" "Other words."}}</p>` +
		`{{template "source-chip" (sourceChip "override" "A note." "reset-a")}}{{template "source-chip" (sourceChip "file" "A note." "")}}`
	for _, partial := range []bool{false, true} {
		page := ktRenderPage(t, svc, body, partial)
		ids, dupes, described := ktIDs(t, page)
		require.Empty(t, dupes, "every id on the page is its own")
		require.Len(t, described, 6, "six hints, six buttons")
		seen := map[string]bool{}
		for _, ref := range described {
			require.False(t, seen[ref], "two buttons point at one tooltip: %s", ref)
			seen[ref] = true
			tip := ids[ref]
			require.NotNil(t, tip, "aria-describedby=%q names no element", ref)
			require.Equal(t, "tooltip", ktAttr(tip, "role"))
			btn := tip.PrevSibling
			require.NotNil(t, btn)
			require.Equal(t, ref, ktAttr(btn, "aria-describedby"), "the tooltip is its own button's")
		}
		same := hintID("", "Same words.")
		for _, want := range []string{same, same + "-2", same + "-3"} {
			require.Contains(t, ids, want)
		}
		note := hintID("where this comes from", "A note.")
		require.Contains(t, ids, note)
		require.Contains(t, ids, note+"-2")
		require.Equal(t, page, ktRenderPage(t, svc, body, partial), "the ids are the same on every render")
	}
	// A page without hints is passed through as it is.
	plain := []byte(`<p id="x">no hints</p>`)
	require.Equal(t, plain, uniqueHintIDs(plain))
}

// The time left's words and tones, at the board's cases and their edges: ok
// above 30 minutes, warn from 30 minutes (renewal warnings start there), and
// ended with no tone.
func TestKtTimeLeft_WordsAndTones(t *testing.T) {
	for _, tc := range []struct {
		left        time.Duration
		words, tone string
	}{
		{2 * time.Hour, "2 h left", "ok"},
		{2*time.Hour + 41*time.Minute + 30*time.Second, "2 h 41 m left", "ok"},
		{31 * time.Minute, "31 min left", "ok"},
		{30*time.Minute + time.Second, "30 min left", "ok"},
		{30 * time.Minute, "30 min left", "warn"},
		{29 * time.Minute, "29 min left", "warn"},
		{28*time.Minute + 59*time.Second, "28 min left", "warn"},
		{59 * time.Second, "under 1 min left", "warn"},
		{time.Millisecond, "under 1 min left", "warn"},
		{0, "ended", ""},
		{-5 * time.Minute, "ended", ""},
		{27*time.Hour + 10*time.Minute, "1 d 3 h left", "ok"},
		{48 * time.Hour, "2 d left", "ok"},
	} {
		words, tone := leftWords(tc.left)
		require.Equal(t, tc.words, words, "%s", tc.left)
		require.Equal(t, tc.tone, tone, "%s", tc.left)
	}
}

// The time left renders its words for a page without script, the end in
// datetime and as a UTC title shell.js turns into this browser's clock, and
// its tone as the badge's.
func TestKtTimeLeft_Markup(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	end := now.Add(29 * time.Minute)
	out := string(timeLeftAt(end, now))
	require.Equal(t, `<time class="badge time-left warn" datetime="2026-11-22T12:29:00Z" data-left data-clock="title" title="2026-11-22 12:29 UTC">`+
		string(icon("clock"))+`<span>29 min left</span></time>`, out)
	require.Contains(t, string(timeLeftAt(&end, now.Add(-2*time.Hour))), `class="badge time-left ok"`)
	require.Contains(t, string(timeLeftAt(end, end)), `class="badge time-left"`)
	require.Contains(t, string(timeLeftAt(end, end)), `<span>ended</span>`)

	for _, open := range []any{time.Time{}, (*time.Time)(nil), nil} {
		got := string(timeLeftAt(open, now))
		require.Contains(t, got, `<span>until the session ends</span>`)
		require.NotContains(t, got, "datetime=", "a lease with no end of its own never counts")
	}
}

// shell.js counts the time left down as kit.go's leftWords words it, every 30
// seconds, only while the page is in sight, and stops at "ended".
func TestKtTimeLeft_ScriptCountsAsTheServerWords(t *testing.T) {
	js := ktSection(t, string(shellJS), "  // Time left (time[data-left]", "  tickLeft();\n})();")
	for _, want := range []string{
		"var LEFT_TICK_MS = 30000;",
		"var LEFT_WARN_MS = 30 * 60 * 1000;",
		"if (ms <= 0) return ['ended', ''];",
		"var tone = ms <= LEFT_WARN_MS ? 'warn' : 'ok';",
		"var mins = Math.floor(ms / 60000);",
		"if (mins < 1) return ['under 1 min left', tone];",
		"if (mins < 60) return [mins + ' min left', tone];",
		"if (mins < 1440) return [Math.floor(mins / 60) + ' h ' + (mins % 60 ? (mins % 60) + ' m ' : '') + 'left', tone];",
		"return [Math.floor(mins / 1440) + ' d ' + (hours ? hours + ' h ' : '') + 'left', tone];",
		"if (document.visibilityState === 'hidden') return;",
		"document.addEventListener('visibilitychange', tickLeft);",
		"document.addEventListener('seam:content-updated', tickLeft);",
		"if (counting) leftTimer = setTimeout(tickLeft, LEFT_TICK_MS);",
	} {
		require.Contains(t, js, want)
	}
	require.NotContains(t, js, "setInterval(", "a hidden page keeps no timer: each tick schedules the next while one counts")
	require.Equal(t, 30*time.Minute, leftWarn, "the script's warn is the server's")
	// A fragment swapped in later counts too, through the clocks' one observer.
	require.Contains(t, string(shellJS), "if (left) tickLeft();")
}

// ktSection is the text of src from the line that starts with from to the
// first to after it.
func ktSection(t *testing.T, src, from, to string) string {
	t.Helper()
	i := strings.Index(src, from)
	require.GreaterOrEqual(t, i, 0, "no %q", from)
	j := strings.Index(src[i:], to)
	require.GreaterOrEqual(t, j, 0, "no %q after %q", to, from)
	return src[i : i+j+len(to)]
}

// ktKit parses kit_shared.html with the console's funcs, for its partials.
func ktKit(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("kt").Funcs(funcs).ParseFS(templateFS, "templates/kit_shared.html"))
}

func ktExec(t *testing.T, tmpl *template.Template, name string, data any) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := tmpl.ExecuteTemplate(&b, name, data)
	return b.String(), err
}

// The source chip's three looks: Config file, Set here with its Reset (and
// the confirm the old button asked with), and Config file decides with a
// lock. The note is the chip's hint. An unknown source fails the render.
func TestKtSourceChip_ThreeLooks(t *testing.T) {
	kit := template.Must(ktKit(t).Parse(`{{define "kt"}}{{template "source-chip" (sourceChip .S .N .R)}}{{end}}` +
		`{{define "kt-confirm"}}{{template "source-chip" (sourceChip "override" "From here." "brief-reset" "Clear it?")}}{{end}}`))
	type in struct{ S, N, R string }

	file, err := ktExec(t, kit, "kt", in{"file", "Following the config file.", "ignored"})
	require.NoError(t, err)
	require.Contains(t, file, `<span class="src-chip is-file"><span class="src-chip-dot" aria-hidden="true"></span><span class="src-chip-label">Config file</span>`)
	require.Contains(t, file, `role="tooltip">Following the config file.</span>`)
	require.Contains(t, file, `aria-label="About where this comes from"`)
	require.NotContains(t, file, "Reset", "only an override resets")

	set, err := ktExec(t, kit, "kt", in{"override", "Set in the console.", "features-reset"})
	require.NoError(t, err)
	require.Contains(t, set, `<span class="src-chip is-override">`)
	require.Contains(t, set, `<span class="src-chip-label">Set here</span>`)
	require.Contains(t, set, `<button class="btn small src-chip-reset" type="submit" form="features-reset">Reset</button>`)
	require.NotContains(t, set, "data-confirm", "Reset confirms only where the old button did")

	confirm, err := ktExec(t, kit, "kt-confirm", nil)
	require.NoError(t, err)
	require.Contains(t, confirm, `form="brief-reset" data-confirm="Clear it?" data-confirm-label="Reset it">Reset</button>`)

	locked, err := ktExec(t, kit, "kt", in{"locked", "The config file sets this.", ""})
	require.NoError(t, err)
	require.Contains(t, locked, `<span class="src-chip is-locked">`+string(icon("lock"))+`<span class="src-chip-label">Config file decides</span>`)
	require.NotContains(t, locked, "<button class=\"btn", "the file decides: nothing to reset here")

	none, err := ktExec(t, kit, "kt", in{"override", "", ""})
	require.NoError(t, err)
	require.NotContains(t, none, "hint", "no note, no hint")
	require.NotContains(t, none, "Reset", "no reset form, no button")

	_, err = ktExec(t, kit, "kt", in{"env", "x", ""})
	require.ErrorContains(t, err, `unknown source "env"`)
}

// The shared steps list: done (a check), now (numbered, current) and todo
// (numbered), with Backups' "doing" read as now.
func TestKtSteps_DoneNowTodo(t *testing.T) {
	type step struct{ Label, State string }
	out, err := ktExec(t, ktKit(t), "steps", []step{{"Asked", "done"}, {"Run <it>", "doing"}, {"Opens", "todo"}})
	require.NoError(t, err)
	require.Equal(t, `<ol class="steps">`+
		`<li class="steps-item is-done"><span class="steps-mark" aria-hidden="true">`+string(icon("check"))+`</span><span class="steps-label">Asked<span class="steps-sr"> (done)</span></span></li>`+
		`<li class="steps-item is-now" aria-current="step"><span class="steps-mark" aria-hidden="true">2</span><span class="steps-label">Run &lt;it&gt;</span></li>`+
		`<li class="steps-item is-todo"><span class="steps-mark" aria-hidden="true">3</span><span class="steps-label">Opens</span></li>`+
		`</ol>`, out)
	now, err := ktExec(t, ktKit(t), "steps", []step{{"Go", "now"}})
	require.NoError(t, err)
	require.Contains(t, now, `<li class="steps-item is-now" aria-current="step">`)

	// Backups' restore progress is drawn with it, and its own list is gone.
	src, err := templateFS.ReadFile("templates/settings_backups.html")
	require.NoError(t, err)
	require.Contains(t, string(src), `{{template "steps" .Steps}}`)
	require.NotContains(t, string(src), "bk-step")
	require.NotContains(t, string(consoleCSS), ".bk-step", "the shared .steps replace Backups' own rules")
	require.Contains(t, string(consoleCSS), ".steps-item.is-now .steps-mark")
}

// ktPostRoutes reads the patterns Register mounts through its post() helper
// (console.go): the routes the follow contract and the origin check cover.
func ktPostRoutes(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "console.go", nil, 0)
	require.NoError(t, err)
	var routes []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Register" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "post" {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				pattern, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				routes = append(routes, pattern)
			}
			return true
		})
	}
	return routes
}

// ktRouteMatches reports whether a form's action reaches the POST pattern: a
// {name} segment takes any one segment, a {name...} the rest, and a template
// action in the form's path stands for one segment.
func ktRouteMatches(pattern, action string) bool {
	path, ok := strings.CutPrefix(pattern, "POST ")
	if !ok {
		return false
	}
	action, _, _ = strings.Cut(action, "?")
	action, _, _ = strings.Cut(action, "#")
	want, got := strings.Split(path, "/"), strings.Split(action, "/")
	for i, seg := range want {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}") {
			return i < len(got)
		}
		if i >= len(got) {
			return false
		}
		switch {
		case seg == "{$}":
			return got[i] == "" && i == len(got)-1
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			if got[i] == "" {
				return false
			}
		case strings.Contains(got[i], "{{"):
			return false // a template action stands only where the pattern takes any segment
		case seg != got[i]:
			return false
		}
	}
	return len(want) == len(got)
}

var (
	ktFormTag   = regexp.MustCompile(`(?s)<form\b[^>]*>`)
	ktAutosave  = regexp.MustCompile(`\sdata-autosave(\s|=|>|$)`)
	ktActionRe  = regexp.MustCompile(`\saction="([^"]*)"`)
	ktMethodRe  = regexp.MustCompile(`\smethod="([^"]*)"`)
	ktTemplated = regexp.MustCompile(`\{\{.*?\}\}`)
)

// ktAutosaveFaults lists what is wrong with each data-autosave form in src:
// it must post, and post to a route Register mounts through post().
func ktAutosaveFaults(src string, routes []string) (forms int, faults []string) {
	for _, tag := range ktFormTag.FindAllString(src, -1) {
		if !ktAutosave.MatchString(tag) {
			continue
		}
		forms++
		if m := ktMethodRe.FindStringSubmatch(tag); m == nil || !strings.EqualFold(m[1], "post") {
			faults = append(faults, "not a POST form: "+tag)
			continue
		}
		m := ktActionRe.FindStringSubmatch(tag)
		if m == nil || !strings.HasPrefix(m[1], "/console/") {
			faults = append(faults, "no literal /console/ action: "+tag)
			continue
		}
		// A template action inside the path stands for one segment.
		action := ktTemplated.ReplaceAllString(m[1], "{{}}")
		matched := false
		for _, r := range routes {
			if ktRouteMatches(r, action) {
				matched = true
				break
			}
		}
		if !matched {
			faults = append(faults, "action is no route registered through post(): "+tag)
		}
	}
	return forms, faults
}

// Every form that saves at once posts to a route Register mounts through
// post(), so the follow contract and the origin check hold for it. No page
// carries one yet (UX 34 to 39 add them): the planted forms below prove the
// check would catch a wrong one.
func TestKtAutosave_FormsPostToRegisteredRoutes(t *testing.T) {
	routes := ktPostRoutes(t)
	require.Contains(t, routes, "POST /console/settings/features", "the parse sees Register's post() calls")
	require.NotContains(t, routes, "POST /console/login", "a route mounted through handle() is not post()'s")
	require.Greater(t, len(routes), 50)

	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, rerr := templateFS.ReadFile(path)
		require.NoError(t, rerr)
		_, faults := ktAutosaveFaults(string(src), routes)
		require.Empty(t, faults, "%s", path)
		return nil
	})
	require.NoError(t, err)

	// The controls: a good form passes, each wrong one is caught.
	for _, tc := range []struct {
		form string
		ok   bool
	}{
		{`<form method="post" action="/console/settings/features" data-autosave>`, true},
		{`<form data-autosave method="post" action="/console/settings/updates">`, true},
		{`<form method="post" action="/console/projects/{{.Slug}}/isolation" data-autosave>`, true},
		{`<form method="post" action="/console/tasks/{{.ID}}/status" class="x" data-autosave="">`, true},
		{`<form method="post" action="/console/settings/nowhere" data-autosave>`, false},
		{`<form method="post" action="/console/login" data-autosave>`, false},
		{`<form method="post" action="/console/settings/{{.Which}}" data-autosave>`, false},
		{`<form action="/console/settings/features" data-autosave>`, false},
		{`<form method="get" action="/console/settings/features" data-autosave>`, false},
		{`<form method="post" action="{{.Action}}" data-autosave>`, false},
		{`<form method="post" action="/console/settings/features/extra" data-autosave>`, false},
	} {
		forms, faults := ktAutosaveFaults(tc.form, routes)
		require.Equal(t, 1, forms, tc.form)
		if tc.ok {
			require.Empty(t, faults, tc.form)
		} else {
			require.Len(t, faults, 1, "the check misses a wrong form: %s", tc.form)
		}
	}
	forms, _ := ktAutosaveFaults(`<form method="post" action="/console/settings/nowhere" data-autosaved>`, routes)
	require.Zero(t, forms, "only data-autosave itself marks a form")
}

// The autosave posts only through navigation.js: it submits the form, which
// the shared submit listener takes (X-Seam-Follow, the follow GET), and never
// fetches on its own. Only a switch, a radio or a select saves at once.
func TestKtAutosave_PostsThroughTheSharedPath(t *testing.T) {
	js := ktSection(t, string(shellJS), "  // Switches that save at once (D9)", "  document.addEventListener('seam:content-updated', keepAutoValues);")
	require.Contains(t, js, "form.requestSubmit();")
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "sendBeacon", ".submit()", "location.reload(", "location.href ="} {
		require.NotContains(t, js, banned, "the autosave posts only through the shared path")
	}
	require.Contains(t, js, "return !!el && (el.type === 'checkbox' || el.type === 'radio' || el.tagName === 'SELECT');",
		"typing in a text or number field never posts")
	require.Contains(t, js, "if (autoSaving && e.target === autoSaving.form) autoSaving.sent = e.defaultPrevented;",
		"a form navigation.js did not take is left to the browser")
	require.Contains(t, js, "el.setAttribute('aria-busy', 'true');")
	require.Contains(t, js, "el.disabled = true;")
	require.Contains(t, js, "label: 'Undo', run: function () { replay(save, save.to, save.from, true); }",
		"Undo posts the values from before the change")
	require.Contains(t, js, "if (form) setFormValues(form, save.from, save.name);", "a failed save puts the control back")
	require.Contains(t, js, "label: 'Try again', run: function () { retry(save); }")
	require.Contains(t, js, "document.addEventListener('seam:submitted'")

	nav := string(navigationJS)
	require.Contains(t, nav, "var autosave = form.hasAttribute('data-autosave');")
	require.Contains(t, nav, "history: autosave ? 'replace' : 'push', source: 'mutation', outcome: outcome",
		"the post stays on load(): X-Seam-Follow and the follow GET")
	require.Contains(t, nav, "document.dispatchEvent(new CustomEvent('seam:submitted', {")
	require.Contains(t, nav, "detail: { form: form, ok: landed === true && !outcome.error, error: outcome.error || '' }")
	require.Contains(t, nav, "outcome.error = finalURL.searchParams.get('error') || '';",
		"a save the server refused (?error=) is a failed one")

	// shell.js loads after navigation.js, so its submit listener sees the
	// shared one's preventDefault.
	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	nv, sh := strings.Index(string(layout), `{{static "navigation.js"}}`), strings.Index(string(layout), `{{static "shell.js"}}`)
	require.True(t, nv > 0 && sh > nv)
}

// flash takes an optional action: a real button after the words that takes
// the toast down and runs it. Every two-argument call keeps working; a toast
// stays while the pointer or focus is on it and goes soon after focus leaves.
func TestKtFlash_ActionButton(t *testing.T) {
	nav := string(navigationJS)
	require.Contains(t, nav, "function flash(message, tone, action) {")
	require.Contains(t, nav, "if (action && action.label && typeof action.run === 'function') {", "the action is optional")
	require.Contains(t, nav, "button.type = 'button';")
	require.Contains(t, nav, "button.className = 'toast-action';")
	require.Contains(t, nav, "button.textContent = action.label;", "the label is text, never markup")
	require.Contains(t, nav, "if (toast.matches(':hover, :focus-within')) { holdToast(toast, TOAST_LINGER_MS); return; }")
	require.Contains(t, nav, "toast.addEventListener('focusout', function (event) {")
	require.Contains(t, nav, "holdToast(toast, button ? ACTION_MS : TOAST_MS);")
	require.Contains(t, nav, "flash: flash,")
	// The toast element is layout.html's, unchanged: role=status, polite.
	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	require.Contains(t, string(layout), `<div id="live-toast" role="status" aria-live="polite" hidden></div>`)
	require.Contains(t, string(consoleCSS), "#live-toast:has(button) { pointer-events: auto; }", "a toast with a button takes the pointer")
}

// A flash is one-shot: the address an in-place load keeps in history never
// carries ?notice= or ?error=, or the next live refresh would say it again.
func TestKtNavigation_FlashParamsNeverEnterHistory(t *testing.T) {
	nav := string(navigationJS)
	strip := strings.Index(nav, "if (canonical.searchParams.has(key)) canonical.searchParams.delete(key);")
	keep := strings.Index(nav, "nextURL = canonical.href;")
	push := strings.Index(nav, "history.pushState(state, '', nextURL)")
	require.True(t, strip > 0 && keep > strip && push > keep, "the params go before the URL is kept and pushed")
	require.Contains(t, nav, "['notice', 'error'].forEach(function (key) {")
}

// Hints: a tap pins one, Esc closes it in the capture phase, before
// panel.js's listener, and only when it closes something; the pinned one is
// pinned again after a morph.
func TestKtHints_ScriptPinsAndCloses(t *testing.T) {
	js := ktSection(t, string(shellJS), "  // Hints ({{hint}} and {{hintFor}}, kit.go)", "  // Switches that save at once (D9)")
	require.Contains(t, js, "if (e.key !== 'Escape' || e.defaultPrevented) return;")
	require.Contains(t, js, "if (!open.length) return;\n    e.preventDefault();", "Esc is taken only when it closes a hint")
	require.Contains(t, js, "  }, true);", "Esc is caught in the capture phase")
	require.Contains(t, js, "h.classList.add('is-pinned');")
	require.Contains(t, js, "shutOtherHints(h);", "one hint is open at a time")
	require.Contains(t, js, "document.querySelectorAll('.hint.is-pinned').forEach(unpinHint);", "a tap outside closes it")
	require.Contains(t, js, "document.addEventListener('seam:content-updated', function () {\n    if (!pinnedHint) return;",
		"the morph resets classes: the pinned hint is pinned again")
	require.Contains(t, js, "e.preventDefault();\n      if (h.classList.contains('is-pinned'))",
		"a hint's click is its own: panel.js leaves a prevented click alone")

	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	sh, pn := strings.Index(string(layout), `{{static "shell.js"}}`), strings.Index(string(layout), `{{static "panel.js"}}`)
	require.True(t, sh > 0 && pn > sh, "shell.js registers its Esc before panel.js")

	css := string(consoleCSS)
	require.Contains(t, css, ".hint:hover .hint-pop, .hint:focus-within .hint-pop, .hint.is-pinned .hint-pop { display: block; }",
		"hover and keyboard focus open a hint by CSS")
	require.Contains(t, css, ".hint.is-shut .hint-pop { display: none; }")
}
