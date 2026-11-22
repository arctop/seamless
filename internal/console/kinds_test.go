package console

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// One glossary line for each kind core accepts, and none for a kind it does
// not: a kind added to core without its line fails here.
func TestLibKindGlossaryCoversEveryCoreKind(t *testing.T) {
	for _, k := range core.MemoryKinds {
		line := kindGloss(string(k))
		require.NotEmpty(t, line, "kind %s has no glossary line in kinds.go", k)
		require.NotContains(t, line, "\n", "%s: one line", k)
		require.LessOrEqual(t, len(line), 100, "%s: one short line", k)
		require.True(t, strings.HasSuffix(line, "."), "%s: a sentence", k)
	}
	for k := range kindGlossary {
		require.True(t, k.Valid(), "kinds.go explains %q, which core does not accept", k)
	}
	require.Len(t, kindGlossary, len(core.MemoryKinds))
	require.Empty(t, kindGloss("bogus"))
}

// A kind explains itself wherever it shows: under its rail group's header,
// as the chip's tooltip in the one list, the reader and the peek, and in the
// Kind menu.
func TestLibKindChipsExplainThemselves(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	m := writeMemory(t, mgr, core.KindStage, "pay", "launch-stage", "where the launch stands")
	gloss := kindGloss(string(core.KindStage))

	grouped := getHTMLBody(t, mux, "/console/memories?group=project")
	require.Contains(t, grouped, `<p class="rail-subgroup-gloss" title="`+gloss+`">`+gloss+`</p>`)
	flat := getHTMLBody(t, mux, "/console/memories")
	require.Contains(t, flat, `<span class="ri-chip ri-kind" data-kind="stage" title="`+gloss+`">stage</span>`)
	require.Contains(t, flat, `<span class="kind" title="`+gloss+`">stage</span>`, "the reader's chip")
	require.Contains(t, flat, `title="`+gloss+`">stage<span class="n">1</span></a>`, "the Kind menu")
	peek := getPeek(t, mux, "/console/memories/"+m.ID+"?peek=1").Body.String()
	require.Contains(t, peek, `<span class="kind" title="`+gloss+`">stage</span>`)
}

// The one list's kind chip reads its kind in the kind's color on the
// row's own ground. The [data-kind="..."] rules paint the kind dots that
// same color as a background, and they match the chip too (it carries
// data-kind), so without its own background the chip was a solid blob
// with its word in its own color: unreadable.
func TestConsoleCSS_KindChipKeepsItsWordReadable(t *testing.T) {
	css := string(consoleCSS)
	require.Contains(t, css, `[data-kind="stage"]      { background: var(--k-stage); }`, "the dot rule this guards against is still there")
	i := strings.Index(css, ".ri-chip.ri-kind {")
	require.GreaterOrEqual(t, i, 0)
	rule := css[i : i+strings.Index(css[i:], "}")]
	require.Contains(t, rule, "color: var(--lib-item-accent);")
	require.Contains(t, rule, "background: none;", "the chip clears the kind background its data-kind attribute picks up")
}
