package console

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Projects, Sessions, and Interactions are operational explorers rather than
// document libraries, but they share the same orientation contract: one hero,
// one explicit control bar, and a labeled map/stream stage.
func TestOperationalExplorerScreens_ShareOrientationChrome(t *testing.T) {
	_, mux := newConsole(t)
	for _, tc := range []struct {
		path string
		kind string
	}{
		{path: "/console/projects", kind: "projects"},
		{path: "/console/sessions", kind: "sessions"},
		{path: "/console/interactions", kind: "interactions"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			page := getPeek(t, mux, tc.path)
			require.Equal(t, http.StatusOK, page.Code)
			body := page.Body.String()
			require.Contains(t, body, `class="ops-page" data-ops="`+tc.kind+`"`)
			// The compact title row replaced the hero + control bar; the stage
			// below it is unchanged.
			require.Contains(t, body, `class="mv2-bar"`)
			require.Contains(t, body, `class="mv2-count`)
			require.NotContains(t, body, `class="lib-hero ops-hero"`)
			require.NotContains(t, body, `class="lib-controlbar ops-controlbar`)
			require.Contains(t, body, `class="ops-stage`)
		})
	}
}

func TestProjectsExplorer_ExposesReachWindow(t *testing.T) {
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/projects?w=7d")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, "Reach window")
	require.Contains(t, body, `href="/console/projects?group=activity&amp;sort=recent&amp;q=&amp;w=24h"`)
	require.Contains(t, body, `href="/console/projects?group=activity&amp;sort=recent&amp;q=&amp;w=all"`)
	require.Contains(t, body, "Reach = active memories surfaced")
	require.Contains(t, body, `class="project-summary-grid"`)
	require.Contains(t, body, "Knowledge reach", "portfolio reach moved into the summary grid")
}

func TestProjectsExplorer_AtlasIsResponsive(t *testing.T) {
	css := string(consoleCSS)
	require.Contains(t, css, `grid-template-areas: "main reach knowledge work activity"`)
	require.Contains(t, css, `grid-template-areas: "main reach activity" "main knowledge work"`)
	require.Contains(t, css, `grid-template-areas: "main main" "reach activity" "knowledge work"`)
	require.Contains(t, css, `.project-card { display: flex; flex-direction: column; }`)
}

func TestInteractionsExplorer_ExposesTraceFiltersAndVisibleCount(t *testing.T) {
	_, mux := newConsole(t)
	page := getPeek(t, mux, "/console/interactions")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `id="ix-count"`)
	require.Contains(t, body, `data-cat="prompt"`)
	require.Contains(t, body, `id="ix-empty-copy"`)
	require.Contains(t, body, `class="list-detail-split ops-split interactions-split"`)
}

func TestOperationalExplorerStyles_SideBySideDetailIsResponsive(t *testing.T) {
	css := string(consoleCSS)
	require.Contains(t, css, ".ops-split.detail-open {")
	require.Contains(t, css, "grid-template-columns: minmax(360px, .85fr) minmax(480px, 1.15fr)")

	stackAt := strings.Index(css, "@media (max-width: 960px)")
	require.NotEqual(t, -1, stackAt)
	require.Contains(t, css[stackAt:], ".ops-split.detail-open")
	require.Contains(t, css[stackAt:], "display: flex; flex-direction: column")
}
