package console

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// The Held screen's part-2 sections: the lockdown switch, the review's flagged
// items with Mark safe, and the files in drift quarantine with Accept -- each
// previewed the way the held queue is (secrets redacted, hidden characters
// spelled out), and each action the owner's.
func TestHeldPage_ReviewDriftAndLockdown(t *testing.T) {
	ctx := context.Background()
	f := newHeldConsole(t, config.Features{Screening: true}, "basic")
	// The daemon's wiring: the service knows the feature is on.
	f.scr = screening.New(screening.Config{DB: f.db, Files: f.mgr, Events: events.NewRecorder(f.db),
		Enabled: func(context.Context) bool { return true }})
	svcFix := rewire(t, f)

	now := time.Now().UTC()
	key := "ghp_" + strings.Repeat("Q1w2", 9)
	flagged, err := f.mgr.WriteMemory(ctx, core.Memory{ID: "01FLAGGED0000000000000000A", Kind: core.KindReference,
		Name: "leaked-token", Project: "app", Description: "a token", Body: "token " + key + "\n",
		Created: now, Updated: now, ValidFrom: now})
	require.NoError(t, err)
	clean, err := f.mgr.WriteMemory(ctx, core.Memory{ID: "01CLEAN00000000000000000AA", Kind: core.KindGotcha,
		Name: "boot-race", Project: "app", Description: "boot race", Body: "It races.\n",
		Created: now, Updated: now, ValidFrom: now})
	require.NoError(t, err)
	require.NoError(t, f.scr.Sync(ctx))
	abs := filepath.Join(f.mgr.Store().DataDir(), clean.FilePath)
	raw, err := os.ReadFile(abs)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(abs, append(raw, []byte("hidden"+string(rune(0x202E))+"text\n")...), 0o600))
	_, err = f.scr.CheckDrift(ctx)
	require.NoError(t, err)

	page := getPeek(t, svcFix, "/console/held").Body.String()
	require.Contains(t, page, "Lockdown")
	require.Contains(t, page, `data-flagged="`+flagged.FilePath+`"`)
	require.Contains(t, page, `data-drift="`+clean.FilePath+`"`)
	require.NotContains(t, page, key, "the flagged item's secret is redacted on the page")
	require.Contains(t, page, `title="unicode.bidi: `+screening.Describe(screening.UnicodeBidi)+`">[U+202E]</mark>`,
		"the drifted text's hidden character is spelled out, and marked in its finding's color")

	rr := postForm(svcFix, "/console/held/corpus/safe", "path="+url.QueryEscape(flagged.FilePath))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	rec, _, err := store.CorpusRecordByPath(ctx, f.db, flagged.FilePath)
	require.NoError(t, err)
	require.Equal(t, store.ReviewSafe, rec.ReviewStatus)

	rr = postForm(svcFix, "/console/held/corpus/accept", "path="+url.QueryEscape(clean.FilePath))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	rec, _, err = store.CorpusRecordByPath(ctx, f.db, clean.FilePath)
	require.NoError(t, err)
	require.False(t, rec.Drifted())
	rr = postForm(svcFix, "/console/held/corpus/accept", "path=memory/app/none.md")
	require.Equal(t, http.StatusNotFound, rr.Code)

	rr = postForm(svcFix, "/console/held/lockdown", "on=1")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, f.scr.LockdownOn(ctx))
	require.Contains(t, getPeek(t, svcFix, "/console/held").Body.String(), `data-lockdown="on"`)
	rr = postForm(svcFix, "/console/held/lockdown", "on=maybe")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	require.True(t, f.scr.LockdownOn(ctx), "a malformed value changes nothing")
	rr = postForm(svcFix, "/console/held/lockdown", "on=0")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.False(t, f.scr.LockdownOn(ctx))

	rr = postForm(svcFix, "/console/held/review", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "Reviewed+2+items")
}

// rewire builds a console over the fixture's database and files with the
// fixture's (re-built) screening service.
func rewire(t *testing.T, f heldFixture) *http.ServeMux {
	t.Helper()
	svc, err := New(Config{
		DB: f.db, Events: events.NewRecorder(f.db), APIKey: testKey, Files: f.mgr, Screening: f.scr,
		Features: config.Features{Screening: true}, Level: "basic",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return mux
}
