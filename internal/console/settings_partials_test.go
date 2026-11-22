package console

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// settingsPageSource is the Settings page's own template text: settings.html
// and its section partials (render.go's settingsPartials), Backups' partial
// aside, which its own tests read. A test that asserts what the page's markup
// or script holds reads this, so a section can move between files freely.
func settingsPageSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, name := range append([]string{"templates/settings.html"}, settingsPartials...) {
		if name == "templates/settings_backups.html" {
			continue
		}
		src, err := templateFS.ReadFile(name)
		require.NoError(t, err, name)
		b.Write(src)
		b.WriteByte('\n')
	}
	return b.String()
}

// Every partial render.go parses with the page exists and defines the
// template the page's dispatch calls.
func TestSettingsPartials_EachDefinesItsSection(t *testing.T) {
	page, err := templateFS.ReadFile("templates/settings.html")
	require.NoError(t, err)
	for _, name := range settingsPartials {
		src, err := templateFS.ReadFile(name)
		require.NoError(t, err, name)
		define := "settings-" + strings.TrimSuffix(strings.TrimPrefix(name, "templates/settings_"), ".html")
		if define == "settings-gardener" {
			define = "settings-gardener-switches"
		}
		require.Contains(t, string(src), `{{define "`+define+`"}}`, name)
	}
	for _, sec := range []string{"experience", "features", "setup", "updates", "briefing", "workspaces", "engine"} {
		require.Contains(t, string(page), `{{if eq .Section "`+sec+`"}}{{template "settings-`+sec+`" $}}{{end}}`, sec)
	}
}
