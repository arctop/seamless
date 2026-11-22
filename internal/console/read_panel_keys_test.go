package console

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every row of the ? sheet sits in one of its sections' lists. Two rows
// added beside the sheet's card instead (p, then Enter) rendered as stray
// text in the overlay, outside every <dl>.
func TestKeysSheet_EveryRowSitsInItsList(t *testing.T) {
	b, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	layout := string(b)
	start := strings.Index(layout, `<div id="keys" class="keys-overlay"`)
	require.GreaterOrEqual(t, start, 0, "the layout draws the ? sheet")
	end := strings.Index(layout[start:], `<script src=`)
	require.Greater(t, end, 0, "the scripts follow the sheet")
	sheet := layout[start : start+end]
	last := strings.LastIndex(sheet, "</dl>")
	require.Greater(t, last, 0)
	require.NotContains(t, sheet[last:], "<dt>", "a row after the sheet's last list sits outside every list")
	for _, row := range []string{
		`<div><dt><kbd>p</kbd></dt><dd>Choose a project (Memories, Notes, Plans)</dd></div>`,
		`<div><dt><kbd>enter</kbd></dt><dd>Open the focused row's page (Memories)</dd></div>`,
	} {
		require.Equal(t, 1, strings.Count(sheet, row), "the sheet lists %s once", row)
	}
	lists := regexp.MustCompile(`(?s)<dl[^>]*>.*?</dl>`).FindAllString(sheet, -1)
	rows := 0
	for _, l := range lists {
		rows += strings.Count(l, "<dt>")
	}
	require.Equal(t, strings.Count(sheet, "<dt>"), rows, "every row is inside a <dl>")
}

// While the read-in-place panel is open, the keys that act on the page
// behind it leave that page alone: the Gardener's a and x (library.js asks
// shellHoldsKey) and the / that focuses the page's filter (search.js).
func TestReadPanel_KeysBehindItStayPut(t *testing.T) {
	guard := `if (window.SeamPanel && window.SeamPanel.isOpen()) return`
	lib := string(libraryJS)
	at := strings.Index(lib, "function shellHoldsKey()")
	require.GreaterOrEqual(t, at, 0)
	body := lib[at : at+strings.Index(lib[at:], "\n  }\n")]
	require.Contains(t, body, guard+" true;", "an open panel holds the Gardener's a and x")

	search := string(searchJS)
	slash := strings.Index(search, `if (e.key === '/'`)
	require.GreaterOrEqual(t, slash, 0)
	branch := search[slash : slash+strings.Index(search[slash:], "e.preventDefault();")]
	require.Contains(t, branch, guard+";", "an open panel keeps / from focusing the filter behind it")
}

// A peek fragment, what the panel draws, gets the hint-id pass a page
// gets: two equal hints in it are two tooltips, each its own button's.
func TestPeekFragment_RepeatedHintsGetTheirOwnIDs(t *testing.T) {
	_, svc := newTestService(t)
	frag := template.Must(template.New("peek_zz").Funcs(funcs).Parse(
		`{{define "peek-body"}}<p>{{hint "Same words."}}</p><p>{{hint "Same words."}}</p>{{end}}`))
	svc.fragments["zz-hints"] = frag
	rec := httptest.NewRecorder()
	svc.renderFragment(rec, httptest.NewRequest(http.MethodGet, "/console/memories/x?peek=1", nil), "zz-hints", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	id := hintID("", "Same words.")
	require.Equal(t, 1, strings.Count(body, `id="`+id+`"`), "the first hint keeps its id")
	require.Equal(t, 1, strings.Count(body, `id="`+id+`-2"`), "the second hint gets its own")
	require.Equal(t, 1, strings.Count(body, `aria-describedby="`+id+`-2"`), "and its button points at it")
}
