package console

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// skThemeBlock returns the body of the theme block that opens with head in
// console.css: everything up to the block's closing brace on a line of its own
// (indented by indent).
func skThemeBlock(t *testing.T, css, head, indent string) string {
	t.Helper()
	at := strings.Index(css, head)
	require.NotEqual(t, -1, at, "console.css has no %q block", head)
	body := css[at+len(head):]
	end := strings.Index(body, "\n"+indent+"}\n")
	require.NotEqual(t, -1, end, "the %q block never closes", head)
	return body[:end]
}

// Every group kind maps to an accent console.css defines, through one token
// that each of the three theme blocks sets; no two kinds share a token; and
// the retired memories wear the memory token muted, never a hue of their own.
// The chips used to carry the plural group kind, which no accent rule named,
// so every chip fell back to one muted color.
func TestSkSearch_EveryGroupKindWearsADefinedAccent(t *testing.T) {
	css := string(consoleCSS)
	blocks := map[string]string{
		"light":          skThemeBlock(t, css, ":root {", ""),
		"dark":           skThemeBlock(t, css, `[data-theme="dark"] {`, ""),
		"dark by the OS": skThemeBlock(t, css, `:root:not([data-theme="light"]) {`, "  "),
	}
	tokenOf := map[string]string{} // accent -> token
	for _, kind := range searchGroupKinds(searchScopes) {
		accent := searchAccents[kind]
		require.NotEmpty(t, accent, "group kind %s has no accent", kind)
		if _, seen := tokenOf[accent]; seen {
			continue
		}
		rule := regexp.MustCompile(`(?m)^\[data-search-kind="` + regexp.QuoteMeta(accent) + `"\]\s*\{\s*--search-accent:\s*var\((--[a-z0-9-]+)\);\s*\}`)
		m := rule.FindAllStringSubmatch(css, -1)
		require.Len(t, m, 1, "console.css defines the %s accent (group %s) once", accent, kind)
		token := m[0][1]
		for theme, block := range blocks {
			require.Regexp(t, `(^|[\s;])`+regexp.QuoteMeta(token)+`:`, block, "%s: %s sets %s", accent, theme, token)
		}
		tokenOf[accent] = token
	}
	var tokens []string
	for _, token := range tokenOf {
		require.NotContains(t, tokens, token, "two kinds share one accent token")
		tokens = append(tokens, token)
	}
	require.Len(t, tokens, len(searchScopes)-1, "one accent per scope's kind")

	memory := tokenOf[searchAccents[searchRetiredKind]]
	require.Equal(t, tokenOf["memory"], memory, "the retired memories wear the memory accent")
	require.Regexp(t, `\[data-search-kind="memory"\]\[data-retired\]\s*\{\s*--search-accent:\s*color-mix\(in oklab, var\(`+
		regexp.QuoteMeta(memory)+`\) \d+%, var\(--muted\)\);\s*\}`, css, "muted, never a new hue")
}

// The accent is decided at the source: each group carries the singular kind
// its rows carry, so on the page a breakdown chip is the legend for its rows,
// and the type pills and an empty page's scope chip wear the same accents.
func TestSkSearch_ChipsAreTheLegendForTheirRows(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSearchMemory(t, db, "01SKMEM", "sk-zircon-memory", "zircon memory", "seam", "zircon body")
	seedSearchMemory(t, db, "01SKOLD", "sk-zircon-old", "zircon old", "seam", "zircon old body")
	sxRetire(t, db, "01SKOLD", "")
	seedSearchNote(t, db, "01SKNOTE", "sk-zircon-note", "Zircon note", "d", "seam", "zircon", now)
	seedSearchTask(t, db, "01SKTASK", "seam", "zircon task", "")
	seedSearchTask(t, db, "01SKSTEP", "seam", "a step", "zircon-plan")
	seedTrial(t, db, "zircon-lab", "zircon trial", core.OutcomePass, "seam", "", nil, 1)
	require.NoError(t, store.CreateProject(ctx, db, core.Project{ID: "01SKPRJ", Slug: "zircon-tools", Name: "Zircon Tools", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: "01SKSES", Name: "cc/zircon", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))

	var data searchData
	getJSON(t, mux, "/console/search?format=json&archived=1&q=zircon", &data)
	var kinds []string
	for _, g := range data.Groups {
		kinds = append(kinds, g.Kind)
		require.Equal(t, searchAccents[g.Kind], g.Accent, g.Kind)
		require.Equal(t, g.Kind == searchRetiredKind, g.Retired, g.Kind)
		for _, r := range g.Rows {
			require.Equal(t, g.Accent, r.Kind, "group %s: its rows wear its accent", g.Kind)
			require.Equal(t, g.Retired, r.Retired != "", "group %s", g.Kind)
		}
	}
	require.ElementsMatch(t, searchGroupKinds(searchScopes), kinds, "the fixture reaches every group kind")

	page := getHTMLBody(t, mux, "/console/search?archived=1&q=zircon")
	breakdown := page[strings.Index(page, `<div class="search-breakdown"`):]
	breakdown = breakdown[:strings.Index(breakdown, "</div>")]
	chip := regexp.MustCompile(`<span data-search-kind="([a-z]+)"( data-retired)?>`)
	row := regexp.MustCompile(`<article class="search-result row-link" data-search-kind="([a-z]+)"[^>]*?( data-retired="[a-z]+")?>`)
	legend := func(re *regexp.Regexp, html string) []string {
		var out []string
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			key := m[1]
			if m[2] != "" {
				key += " retired"
			}
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
		slices.Sort(out)
		return out
	}
	chips := legend(chip, breakdown)
	require.Len(t, chips, len(data.Groups), "one chip per group")
	require.Equal(t, chips, legend(row, page), "every chip names its rows' accent, and every row's accent has its chip")
	require.Contains(t, chips, "memory retired")

	for _, scope := range searchScopes {
		label := searchScopeLabel(scope)
		if scope == "all" {
			require.Contains(t, page, ` aria-current="page">All</a>`, "All names no kind")
			continue
		}
		require.Contains(t, page, ` data-search-kind="`+searchAccents[scope]+`">`+label+`</a>`, "the %s pill", scope)
	}

	empty := getHTMLBody(t, mux, "/console/search?scope=notes&q=nothing-matches-this")
	require.Contains(t, empty, `title="Remove this filter" data-search-kind="note">Notes`)
}

// The palette's group heads wear the same accent, from the group's own key.
func TestSkSearchJS_PaletteGroupsWearTheAccent(t *testing.T) {
	js := string(searchJS)
	for _, want := range []string{
		"group(g.label, g.count + (g.more ? '+' : ''), items, 'remote', g);",
		"if (g && g.accent) head.setAttribute('data-search-kind', g.accent);",
		"if (g && g.retired) head.setAttribute('data-retired', '');",
	} {
		require.Contains(t, js, want)
	}
}
