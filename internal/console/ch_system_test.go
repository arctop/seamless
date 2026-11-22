package console

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// chCSSBlocks returns the body of every `@media <query> {` block in the
// stylesheet, each up to its matching brace.
func chCSSBlocks(t *testing.T, query string) []string {
	t.Helper()
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(consoleCSS), "")
	opener := "@media " + query + " {"
	var out []string
	for at := strings.Index(css, opener); at >= 0; {
		depth, start := 1, at+len(opener)
		i := start
		for ; i < len(css) && depth > 0; i++ {
			switch css[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		require.Zero(t, depth, "unclosed %q", opener)
		out = append(out, css[start:i-1])
		next := strings.Index(css[i:], opener)
		if next < 0 {
			break
		}
		at = i + next
	}
	require.NotEmpty(t, out, "no %q", opener)
	return out
}

// Without WebGL the System page draws no deck: the stage collapses to its
// one-line note (and to a part's readout once one is picked from the list),
// and nothing is left that steers a view the browser cannot draw: no Reset
// view, no Still, no hint, no labels, no canvas. The parts list keeps every
// part's button and the readout, so the page still answers everything.
func TestChSystemWithoutWebGLIsItsList(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("static", "orrery.js"))
	require.NoError(t, err)
	js := string(src)
	fallback := lvBetween(t, js, "    scene = new S.Scene(canvas, labels,", "  /* ---- readout")
	lvInOrder(t, fallback,
		"} catch (e) {",
		"scene = null;",
		"stage.classList.add('no-gl');",
		"if (nogl) nogl.hidden = false;",
		"[canvas, labels, caps, stage.querySelector('.sys-tools'), stage.querySelector('.sys-hint')].forEach(function (el) { if (el) el.hidden = true; });")
	require.Contains(t, js, "if (scene) scene.select(id, fly); else showReadout(id);", "a part picked from the list opens its readout without a scene")

	css := string(consoleCSS)
	for _, rule := range []string{
		".sys-stage.no-gl { height: auto; min-height: 0; overflow: visible;",
		".sys-stage.no-gl canvas, .sys-stage.no-gl .sys-tools, .sys-stage.no-gl .sys-hint,\n.sys-stage.no-gl .sys-labels, .sys-stage.no-gl .sys-captions { display: none; }",
		".sys-stage.no-gl .sys-nogl { position: static;",
		".sys-stage.no-gl .sys-readout { position: static; display: none;",
		".sys-stage.no-gl .sys-readout.open { display: flex; }",
	} {
		require.Contains(t, css, rule)
	}
	// The phone rule that gives the stage 70% of the viewport is less
	// specific than the collapse, which wins at every width.
	require.Contains(t, strings.Join(chCSSBlocks(t, "(max-width: 760px)"), "\n"), ".sys-stage { height: 70vh; min-height: 420px; }")

	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/system").Body.String()
	require.Contains(t, page, `<div class="sys-nogl" id="sys-nogl" hidden><p>This browser can&rsquo;t draw the 3D view. Every part is listed below with the same readouts.</p></div>`)
	require.Contains(t, page, `id="sys-home"`)
	require.Contains(t, page, `id="sys-calm"`)
	require.Contains(t, page, `data-pick="database"`)
}

// On a phone the Overview's four vitals sit two by two, not stacked one per
// row (about 600px of the first screen); the Diary's tiles keep sizing by
// their own container.
func TestChPhoneVitalsTwoByTwo(t *testing.T) {
	css := string(consoleCSS)
	require.NotContains(t, css, ".ov2-vitals { grid-template-columns: 1fr; }")
	var phone string
	for _, b := range chCSSBlocks(t, "(max-width: 760px)") {
		if strings.Contains(b, ".ov2-window-tabs { order: 9; }") {
			phone = b
		}
	}
	require.NotEmpty(t, phone, "the Overview's phone block")
	require.Contains(t, phone, ".ov2-vitals { grid-template-columns: repeat(2, minmax(0, 1fr)); }")

	var tight string
	for _, b := range chCSSBlocks(t, "(max-width: 760px)") {
		if strings.Contains(b, ".ov2-vitals:not(.dy-tiles)") {
			tight = b
		}
	}
	for _, rule := range []string{
		".ov2-vitals:not(.dy-tiles) .ov2-vital { padding: 12px 12px 10px; }",
		".ov2-vitals:not(.dy-tiles) .ov2-vital-num strong { font-size: var(--text-2xl); }",
		".ov2-vitals:not(.dy-tiles) .ov2-vital-foot { flex-wrap: wrap; }",
	} {
		require.Contains(t, tight, rule)
	}
	require.Contains(t, css, ".dy-tiles { margin: 0; grid-template-columns: repeat(4, minmax(0, 1fr)); }", "the Diary's tiles are their own")
}
