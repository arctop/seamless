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

// A kind explains itself wherever it shows: under its group's heading in a
// project's list, as a row's kind tooltip and a kind chip's in the list, and
// in the reader and the peek.
func TestLibKindChipsExplainThemselves(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	m := writeMemory(t, mgr, core.KindStage, "pay", "launch-stage", "where the launch stands")
	gloss := kindGloss(string(core.KindStage))

	project := getHTMLBody(t, mux, "/console/memories?project=pay")
	require.Contains(t, project, `<h2 id="ml-gh-stage">stages</h2><p class="ml-gloss">`+gloss+`</p>`, "a kind group's heading")
	flat := getHTMLBody(t, mux, "/console/memories")
	require.Contains(t, flat, `<span class="ml-kind" title="`+gloss+`">stage</span>`, "a row's kind")
	require.Contains(t, flat, `title="`+gloss+`"><i class="kdot" data-kind="stage"></i>stage<span class="n">1</span></a>`, "the kind chip")
	reader := getPeek(t, mux, "/console/memories/"+m.ID+"?reader=1").Body.String()
	require.Contains(t, reader, `<span class="kind" title="`+gloss+`">stage</span>`, "the reader's chip")
	peek := getPeek(t, mux, "/console/memories/"+m.ID+"?peek=1").Body.String()
	require.Contains(t, peek, `<span class="kind" title="`+gloss+`">stage</span>`)
}

// The kind colors paint the kind dots only. A bare [data-kind="..."] rule
// also matched the one list's kind chip (it carries data-kind for its
// accent), filled it with the kind's color under a word in that same
// color, and left a solid blob nobody could read; any later chip or row
// that names its kind would go the same way. Every rule that colors by
// kind is scoped to .kdot, a Memories row's kind is a colored word on the
// row's own ground, and a kind chip's color is its dot's alone.
func TestConsoleCSS_KindColorsPaintOnlyTheDots(t *testing.T) {
	css := string(consoleCSS)
	n := 0
	for _, line := range strings.Split(css, "\n") {
		j := strings.Index(line, "[data-kind=")
		if j < 0 || !strings.Contains(line, "background: var(--k-") {
			continue
		}
		n++
		require.True(t, strings.HasPrefix(strings.TrimSpace(line), ".kdot[data-kind="), "a kind color rule matches more than the dots: %s", line)
	}
	require.Equal(t, len(core.MemoryKinds), n, "one dot rule per memory kind")
	rule := func(selector string) string {
		t.Helper()
		i := strings.Index(css, "\n"+selector+" {")
		require.GreaterOrEqual(t, i, 0, "no %s rule", selector)
		return css[i : i+strings.Index(css[i:], "}")]
	}
	kind := rule(".ml-kind")
	require.Contains(t, kind, "color: var(--lib-item-accent);", "the row's kind word carries the kind's color")
	require.NotContains(t, kind, "background", "the word sits on the row's own ground")
	chip := rule(".ml-chip")
	require.NotContains(t, chip, "--k-", "a kind chip's color is its dot's")
	require.NotContains(t, chip, "--lib-item-accent")
	require.NotContains(t, css, ".ri-kind", "the rail's kind chip went with the rail")
}
