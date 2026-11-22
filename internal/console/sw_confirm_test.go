package console

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// UX 16 item 14: a confirmation asks in the page (data-confirm, which shell.js
// arms on the first press), never through a native confirm() dialog.

// No console template asks through confirm(), or carries an inline submit or
// click handler that could.
func TestSwNoNativeConfirm(t *testing.T) {
	entries, err := fs.ReadDir(templateFS, "templates")
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		n++
		b, err := fs.ReadFile(templateFS, "templates/"+e.Name())
		require.NoError(t, err)
		src := string(b)
		require.NotContains(t, src, "confirm(", "%s asks through a native confirm()", e.Name())
		require.NotContains(t, src, "onsubmit=", e.Name())
		require.NotContains(t, src, "onclick=", e.Name())
	}
	require.Greater(t, n, 40, "the walk reached the templates")
}

// swButtonTags is every <button ...> start tag in a template's source.
var swButtonTags = regexp.MustCompile(`<button[^>]*>`)

// Each confirmation the native dialogs used to ask now sits, word for word, in
// the data-confirm of the submit button it guards.
func TestSwConfirmsMovedOntoTheirButtons(t *testing.T) {
	for name, sentences := range map[string][]string{
		"held.html": {
			"Turn lockdown on? Agents receive only confirmed or reviewed memories, and their changes to binding rules wait for you.",
			"{{if .Proposal}}Discard this proposal? Nothing lands, and its text is erased.{{else}}Discard this held write? Nothing lands, and its text is erased.{{end}}",
			"Re-run the review? Every memory and note is screened again, and clean ones are marked safe as they read now.",
			"Clear the outside-text mark on this session? Its changes to binding rules will land without a hold.",
		},
		"settings.html": {
			"Clear the stored override and go back to the file/env configuration? No data is deleted.",
			"Install {{.Target}} now? The daemon restarts to finish, so agent sessions lose Seamless for a moment. This skips the soak and does not wait for agents to go idle.",
			"Clear the console override and go back to the file/env configuration?",
			"Delete this project family? Projects and their knowledge will not be deleted.",
			"Switch embeddings off? Recall and search degrade to lexical-only after the next restart.",
			"Re-embed every memory and note with the active model? This calls the embedding provider once per item.",
		},
		"settings_backups.html": {
			"{{if .Undo}}Delete the safety backup from before your last restore? Deleting it removes that restore&rsquo;s undo.{{else}}Delete this backup? It is removed from its folder and cannot be brought back.{{end}}",
		},
		"access_one.html": {"Deny this request? "},
	} {
		b, err := fs.ReadFile(templateFS, "templates/"+name)
		require.NoError(t, err)
		tags := swButtonTags.FindAllString(string(b), -1)
		for _, sentence := range sentences {
			found := false
			for _, tag := range tags {
				if strings.Contains(tag, `data-confirm="`+sentence) {
					require.Contains(t, tag, `type="submit"`, "%s: %q guards a submit", name, sentence)
					require.Contains(t, tag, `data-confirm-label="`, "%s: %q names its armed word", name, sentence)
					found = true
				}
			}
			require.True(t, found, "%s: no button asks %q", name, sentence)
		}
	}
}

// The locked tier's Deny asks in the page too. It is a submit button for a
// form of its own (the two forms it sits in cannot hold one), which the page's
// script sends as JSON on the second press; without script that form posts and
// the deny follows the redirect back to the request, as the advisory tier's
// plain form always did.
func TestSwAccessDenyAsksInThePage(t *testing.T) {
	f := newAccessFixture(t)
	id, _ := f.file(t)

	page := getHTMLBody(t, f.mux, "/console/access/"+id)
	require.Contains(t, page, `<button class="btn" id="access-deny" type="submit" form="access-deny-form" data-confirm="Deny this request? example-finance stays locked for that session." data-confirm-label="Deny it">Deny</button>`)
	require.Contains(t, page, `<form id="access-deny-form" method="post" action="/console/access/`+id+`/deny">`)
	require.NotContains(t, page, "data-deny-confirm")
	require.Contains(t, page, "document.getElementById('access-deny-form')")

	// The script's request: JSON, as before.
	status, out := f.post(t, "/console/access/"+id+"/deny", url.Values{}, false)
	require.Equal(t, http.StatusOK, status, string(out["error"]))
	require.Contains(t, string(out["denied"]), "R-")

	// A browser without script: the form's own post lands back on the request.
	id2, _ := f.file(t)
	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/access/"+id2+"/deny",
		strings.NewReader(url.Values{"next": {"/console/access/" + id2}}.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := do(f.mux, req)
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/access/"+id2+"?notice=Denied+"), rr.Header().Get("Location"))
}

// The held screen's confirmations render on their buttons.
func TestSwHeldConfirmsRender(t *testing.T) {
	f := newHeldConsole(t, config.Features{Screening: true}, "advanced")
	f.holdMemory(t, "sw-held-rule", "a rule\n", "unicode.zero_width")
	page := getHTMLBody(t, f.mux, "/console/held")
	require.Contains(t, page, `data-confirm="Discard this held write? Nothing lands, and its text is erased." data-confirm-label="Discard it">`)
	require.Contains(t, page, `data-confirm="Re-run the review? Every memory and note is screened again, and clean ones are marked safe as they read now." data-confirm-label="Re-run it">Re-run review</button>`)
	require.NotContains(t, page, "confirm(")
}
