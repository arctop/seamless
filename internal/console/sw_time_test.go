package console

import (
	"context"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// UX 16 item 13: a time's hover reads in the browser's own zone. A title that
// was ts's UTC stamp is now agoTime (an age) or timeAs (any other text), a
// <time data-clock="title"> whose UTC title shell.js swaps for local time.

// swTimeTemplates are the templates this sweep converted (memories.html and
// notes.html are the item page's, tasks' the task screens').
var swTimeTemplates = []string{
	"gardener", "held", "labs", "mishaps", "overview", "peek_memory", "peek_note", "peek_project",
	"peek_session", "peek_trial", "retrieval", "session", "sessions", "settings", "settings_backups", "trials",
}

func TestSwTimeTitlesAreLocal(t *testing.T) {
	bare := regexp.MustCompile(`title="\{\{ts [^}]*\}\}"`)
	for _, name := range swTimeTemplates {
		b, err := fs.ReadFile(templateFS, "templates/"+name+".html")
		require.NoError(t, err)
		src := string(b)
		require.Empty(t, bare.FindAllString(src, -1), "%s.html titles a time with UTC alone", name)
		// No time is printed in UTC as text either.
		require.NotRegexp(t, `>[^<{]*\{\{ts `, src, "%s.html prints a UTC stamp", name)
	}
}

func TestSwTimeTitlesRenderLocal(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-3 * time.Hour)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: "01SWTIME", Name: "cc/sw-time", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: at, UpdatedAt: at}))
	local := `<time datetime="` + at.Format(time.RFC3339) + `" data-clock="title" title="` + ts(at) + `">3h</time>`

	list := getHTMLBody(t, mux, "/console/sessions")
	require.Contains(t, list, `<span class="when">`+local+`</span>`)
	page := getHTMLBody(t, mux, "/console/sessions/01SWTIME")
	require.Contains(t, page, "<div><dt>Started</dt><dd>"+local+"</dd></div>")
	require.Contains(t, page, "<span>updated "+local+"</span>")
	peek := getHTMLBody(t, mux, "/console/sessions/01SWTIME?peek=1")
	require.Contains(t, peek, `<span class="session-peek-updated">updated `+local+`</span>`)
	require.False(t, strings.Contains(list+page+peek, `title="`+ts(at)+`">3h</span>`), "no UTC-only title left")
}
