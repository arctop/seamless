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

// The kind colors paint the kind dots only. A bare [data-kind="..."] rule
// also matched the one list's kind chip (it carries data-kind for its
// accent), filled it with the kind's color under a word in that same
// color, and left a solid blob nobody could read; any later chip or row
// that names its kind would go the same way. Every rule that colors by
// kind is scoped to .kdot, and the chip keeps no background of its own.
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
	i := strings.Index(css, ".ri-chip.ri-kind {")
	require.GreaterOrEqual(t, i, 0)
	rule := css[i : i+strings.Index(css[i:], "}")]
	require.Contains(t, rule, "color: var(--lib-item-accent);", "the chip's word carries the kind's color")
	require.NotContains(t, rule, "background", "the chip sits on the row's own ground")
}
