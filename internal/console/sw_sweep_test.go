package console

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// UX 16's one-hour fixes, items 5, 6, 9, 12 and 16 (plan:console-ux-pass).

// swOutcomeLinks is the Trials outcome seg's links, by label.
func swOutcomeLinks(t *testing.T, page string) map[string]string {
	t.Helper()
	start := strings.Index(page, `<span class="mv2-sort-label">Outcome</span>`)
	require.NotEqual(t, -1, start)
	seg := page[start:]
	seg = seg[:strings.Index(seg, "</div>")]
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`<a class="[^"]*" href="([^"]*)">([a-z]+)</a>`).FindAllStringSubmatch(seg, -1) {
		out[m[2]] = m[1]
	}
	return out
}

// Item 5: choosing an outcome opens the list anew, so the reader shows that
// outcome's first match, never the trial that was on screen.
func TestSwTrialsOutcomeLinksOpenTheList(t *testing.T) {
	db, mux := newConsole(t)
	failID := seedTrial(t, db, "hot-lab", "boot fails", core.OutcomeFail, "demo", "", nil, 1)
	passID := seedTrial(t, db, "hot-lab", "boot passes", core.OutcomePass, "demo", "", nil, 2)

	links := swOutcomeLinks(t, getHTMLBody(t, mux, "/console/trials/"+passID))
	require.Equal(t, "/console/trials", links["all"])
	require.Equal(t, "/console/trials?outcome=fail", links["fail"])
	require.Equal(t, "/console/trials?outcome=pass", links["pass"])
	for label, href := range links {
		require.NotContains(t, href, passID, "%s keeps no selection", label)
	}

	// The lab filter rides along, with no stray separator.
	links = swOutcomeLinks(t, getHTMLBody(t, mux, "/console/trials/"+passID+"?lab=hot-lab"))
	require.Equal(t, "/console/trials?lab=hot-lab", links["all"])
	require.Equal(t, "/console/trials?lab=hot-lab&amp;outcome=fail", links["fail"])

	// Where that link lands: the failed trial in the reader.
	failed := getHTMLBody(t, mux, "/console/trials?outcome=fail")
	require.Contains(t, failed, `data-auto-url="/console/trials/`+failID+`?outcome=fail"`)
	require.Contains(t, failed, "boot fails")
	require.NotContains(t, failed, "boot passes")
}

// Item 6: the line under "Available in the console and to agents" says what
// turning the switch off does, on a card that is on and on one that is off.
func TestSwFeatureSwitchSaysWhatOffDoes(t *testing.T) {
	_, mux := newConsoleFeatures(t, config.Features{Research: true})
	page := getHTMLBody(t, mux, "/console/settings?s=features")
	require.Contains(t, page, `<span><strong>Available in the console and to agents</strong><small>Turning this off hides the Labs &amp; Trials screens`,
		"an enabled switch says what switching it off takes away")
	require.NotContains(t, page, "<small>Hides ")
	require.Equal(t, "Turning this off hides nothing on its own.", featureWhenOff(features.Feature{}))
}

// Item 9: Settings says project and Overview where it said scope, workspace
// and Home, and the backups list points up at the form that sits above it.
func TestSwSettingsSaysProjectAndOverview(t *testing.T) {
	_, mux := newConsole(t)
	updates := getHTMLBody(t, mux, "/console/settings?s=updates")
	require.Contains(t, updates, "tells you: here, on the Overview, and in your agents’ briefings.")
	require.NotContains(t, updates, "on Home")

	ws := getHTMLBody(t, mux, "/console/settings?s=workspaces")
	ws = ws[strings.Index(ws, `id="workspace-registry"`):]
	ws = ws[:strings.Index(ws, `<dialog`)]
	require.Contains(t, ws, "See which project each repository resolves to")
	for _, gone := range []string{"Scope topology", "project scope", "workspace scopes", "per scope", "workspace metadata"} {
		require.NotContains(t, ws, gone)
	}

	briefing := getHTMLBody(t, mux, "/console/settings?s=briefing")
	require.Contains(t, briefing, "Let related projects contribute lower-priority context to the current project.")
}

// Item 12: a lock's badge says its state in its tone, a live hold asks before
// Force release, and the end of a hold carries its date.
func TestSwLocksToneConfirmAndDate(t *testing.T) {
	db, mux := lkConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	live := lkSeedLock(t, db, "main", "/work/app", core.RepoLockActive)
	lapsed := lkSeedLock(t, db, "release", "/work/app", core.RepoLockActive)
	free := lkSeedLock(t, db, "docs", "/work/app", core.RepoLockActive)
	sess := lkSeedSession(t, db, "cc/sw-holder", "example-ops")
	_, err := store.AcquireRepoLock(ctx, db, live.ID, sess, time.Hour, now)
	require.NoError(t, err)
	_, err = store.AcquireRepoLock(ctx, db, lapsed.ID, sess, time.Minute, now.Add(-2*time.Hour))
	require.NoError(t, err)

	page := lkGet(t, mux, "/console/locks")
	row := func(id string) string {
		start := strings.Index(page, `id="lock-`+id+`"`)
		require.NotEqual(t, -1, start, id)
		r := page[start:]
		return r[:strings.Index(r, "</li>")]
	}
	held, gone, idle := row(live.ID), row(lapsed.ID), row(free.ID)

	require.Contains(t, held, `<span class="badge accent">held</span>`)
	require.Contains(t, gone, `<span class="badge warn">lapsed</span>`)
	require.Contains(t, idle, `<span class="badge">free</span>`)

	require.Contains(t, held, `data-confirm="End this live hold? Its holder is not told, and may be landing a change right now." data-confirm-label="Release it">Force release</button>`)
	require.Contains(t, gone, `<button class="btn" type="submit">Force release</button>`, "a lapsed hold releases at once")
	require.NotContains(t, idle, "Force release", "a free lock has no hold to end")

	require.Regexp(t, `, until <time datetime="[^"]+" data-clock="date">\d{4}-\d\d-\d\d \d\d:\d\d</time>`, held)
	require.Regexp(t, `, lapsed <time datetime="[^"]+" data-clock="date">\d{4}-\d\d-\d\d \d\d:\d\d</time>`, gone)
	require.NotContains(t, gone, "until", "a lapsed hold says when it lapsed")
}

// Item 16: Search's breakdown reads "1 Session", not "1 Sessions"; the
// palette's group heads (the JSON label) stay the kind's name.
func TestSwSearchBreakdownCountsInTheSingular(t *testing.T) {
	for _, tc := range []struct {
		g    searchGroup
		want string
	}{
		{searchGroup{Kind: "sessions", Label: "Sessions", Accent: "session", Count: 1}, "Session"},
		{searchGroup{Kind: "memories", Label: "Memories", Accent: "memory", Count: 1}, "Memory"},
		{searchGroup{Kind: "sessions", Label: "Sessions", Accent: "session", Count: 2}, "Sessions"},
		{searchGroup{Kind: "sessions", Label: "Sessions", Accent: "session", Count: 1, More: true}, "Sessions"},
		{searchGroup{Kind: searchRetiredKind, Label: "Archived & superseded", Accent: "memory", Count: 1, Retired: true}, "Archived & superseded"},
	} {
		require.Equal(t, tc.want, tc.g.Noun(), "%+v", tc.g)
	}

	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedSearchNote(t, db, "01SWNOTE", "sw-garnet-note", "Garnet note", "d", "seam", "garnet", now)
	for _, s := range []core.Session{
		{ID: "01SWSES1", Name: "cc/garnet-one"}, {ID: "01SWSES2", Name: "cc/garnet-two"},
	} {
		s.ProjectSlug, s.Status, s.CreatedAt, s.UpdatedAt = "seam", core.SessionActive, now, now
		require.NoError(t, store.CreateSession(ctx, db, s))
	}
	page := getHTMLBody(t, mux, "/console/search?q=garnet")
	breakdown := page[strings.Index(page, `<div class="search-breakdown"`):]
	breakdown = breakdown[:strings.Index(breakdown, "</div>")]
	require.Contains(t, breakdown, `<b>1</b> Note</span>`)
	require.Contains(t, breakdown, `<b>2</b> Sessions</span>`)

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=garnet", &data)
	for _, g := range data.Groups {
		if g.Kind == "notes" {
			require.Equal(t, "Notes", g.Label, "the palette heads the group with the kind's name")
		}
	}
}
