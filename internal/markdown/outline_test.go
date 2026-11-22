package markdown

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// RenderOutline gives each h2 and h3 an id of the prefix and a slug of its
// text, numbered when two would share one, and lists them in order; h1 and
// h4 keep no id and stay out of the list.
func TestRenderOutline_GivesSectionHeadingsIDs(t *testing.T) {
	body := "# The note\n\nintro\n\n## Summary\n\ntext\n\n### Root cause, **found**\n\nx\n\n" +
		"## Summary\n\ny\n\n## Summary 2\n\nz\n\n#### Deep\n\nw\n\n## [[known]] links\n"
	html, outline := RenderOutline(body, testResolver)
	got := string(html)
	require.Equal(t, []Heading{
		{Level: 2, Text: "Summary", ID: "doc-summary"},
		{Level: 3, Text: "Root cause, found", ID: "doc-root-cause-found"},
		{Level: 2, Text: "Summary", ID: "doc-summary-2"},
		{Level: 2, Text: "Summary 2", ID: "doc-summary-2-2"},
		{Level: 2, Text: "known links", ID: "doc-known-links"},
	}, outline)
	for _, h := range outline {
		require.Contains(t, got, `<h`+string(rune('0'+h.Level))+` id="`+h.ID+`">`, h.ID)
	}
	require.Contains(t, got, "<h1>The note</h1>", "an h1 keeps no id")
	require.Contains(t, got, "<h4>Deep</h4>", "nor does an h4")
	require.Contains(t, got, `href="/console/memories/ID123"`, "wiki links still resolve")
	ids := regexp.MustCompile(`id="([^"]*)"`).FindAllStringSubmatch(got, -1)
	require.Len(t, ids, len(outline), "no id but the outline's")
	for _, m := range ids {
		require.Regexp(t, `^doc-[a-z0-9]+(-[a-z0-9]+)*$`, m[1])
	}
}

// A heading's text cannot set an attribute: attribute syntax stays text,
// raw HTML stays out, and the one id is built from letters and digits.
func TestRenderOutline_HostileHeadingCannotInjectAttributes(t *testing.T) {
	for body, listed := range map[string]bool{
		"## Heading {#evil onclick=alert(1)}\n":            true,
		"## x\" onmouseover=\"steal()\n":                   true,
		"## <script>alert(1)</script> & \"quotes\"\n":      true,
		"## javascript:alert(1)\n":                         true,
		"## ????\n":                                        true,
		"## <img src=x onerror=alert(1)>\n":                false, // raw HTML alone: no text, no entry
		"## <h2 id=\"doc-x\" onclick=\"steal()\">x</h2>\n": true,
	} {
		html, outline := RenderOutline(body, testResolver)
		got := strings.ToLower(string(html))
		require.NotRegexp(t, `<h2[^>]*\son[a-z]+=`, got, body)
		require.NotContains(t, got, "<script", body)
		require.NotContains(t, got, "<img", body)
		require.Equal(t, 1, strings.Count(got, "<h2"), body)
		if !listed {
			require.Empty(t, outline, body)
			require.NotContains(t, got, "id=", body)
			continue
		}
		require.Regexp(t, `<h2 id="doc-[a-z0-9-]+">`, got, body)
		require.Len(t, outline, 1, body)
		require.Regexp(t, `^doc-[a-z0-9]+(-[a-z0-9]+)*$`, outline[0].ID, body)
	}
	_, outline := RenderOutline("## ????\n", nil)
	require.Equal(t, "doc-section", outline[0].ID, "a heading with no letters still gets a usable anchor")
}

// Render, which the peek and reader fragments use, keeps no heading id: only
// a page that lists the sections asks for them.
func TestRender_HeadingsKeepNoID(t *testing.T) {
	got := string(Render("## Summary\n\n### Detail\n", testResolver))
	require.Contains(t, got, "<h2>Summary</h2>")
	require.Contains(t, got, "<h3>Detail</h3>")
	require.NotContains(t, got, "id=")
	html, outline := RenderOutline("   \n", nil)
	require.Empty(t, html)
	require.Empty(t, outline)
}
