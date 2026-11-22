package console

// Tests of the sidebar's wayfinding (plan:console-ux-pass UX 14, the owner's
// decision of 2026-11-21): the groups in the order Live, Work, Knowledge,
// Research, Server; the Access group first whenever it exists, at every
// level; a Mishaps row under Live whose queue follows the reviewed stamp; and
// Projects, Plans and Tasks at Basic, with no level banner on their pages.
// Every navigation surface derives from the one registry.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// wfGroupOrder is the owner's decision, transcribed on purpose: changing it
// takes a deliberate edit here as well as in screens.go.
var wfGroupOrder = []string{groupLive, groupWork, groupKnowledge, groupResearch, groupServer}

var wfSecRe = regexp.MustCompile(`<div class="nav-sec">([^<]+)</div>`)

// wfSections reads the sidebar's section headings, in order, from a page.
func wfSections(t *testing.T, page string) []string {
	t.Helper()
	var out []string
	for _, m := range wfSecRe.FindAllStringSubmatch(sbNav(t, page), -1) {
		out = append(out, m[1])
	}
	return out
}

// wfGroupLabels is the headed groups navGroups lays out, in order.
func wfGroupLabels(ns navSections) []string {
	var out []string
	for _, g := range ns {
		if !g.Pinned {
			out = append(out, g.Label)
		}
	}
	return out
}

// The registry's groups come in the owner's order at every level and feature
// state, each one contiguous, with the Diary first in Work; the rendered
// sidebar draws exactly those headings.
func TestWfNavGroupOrder(t *testing.T) {
	for _, feats := range []config.Features{featuresAll(false), featuresAll(true)} {
		for _, lvl := range allLevels() {
			got := wfGroupLabels(navGroups(visibleScreens(feats, lvl), "", navCounts{}))
			want := slices.DeleteFunc(slices.Clone(wfGroupOrder), func(g string) bool { return !slices.Contains(got, g) })
			require.Equal(t, want, got, "%s: the groups keep the one order", lvl)
		}
	}
	all := navGroups(visibleScreens(featuresAll(true), levelAdvanced), "", navCounts{})
	require.Equal(t, wfGroupOrder, wfGroupLabels(all), "advanced with every feature shows every group")
	for _, g := range all {
		if g.Label == groupWork {
			require.Equal(t, "diary", g.Entries[0].ID, "the Diary leads the Work group")
		}
		if g.Label == groupLive {
			require.Equal(t, "mishaps", g.Entries[len(g.Entries)-1].ID, "Mishaps is the Live group's last row")
		}
	}

	for _, tc := range []struct {
		level string
		feats config.Features
		want  []string
	}{
		{"basic", config.Features{}, []string{"Live", "Work", "Knowledge"}},
		{"standard", config.Features{Research: true}, []string{"Live", "Work", "Knowledge", "Research", "Server"}},
		{"advanced", featuresAll(true), wfGroupOrder},
	} {
		_, mux := newConsoleLevel(t, tc.feats, tc.level)
		require.Equal(t, tc.want, wfSections(t, sbPage(t, mux, "/console/settings")), tc.level)
	}
}

// With a project locked, the Access group (Access requests, Approvers) is the
// sidebar's first group at every level, ahead of Live; with read grants only,
// it holds Access requests alone and still comes first. Devices, inserted
// ahead of the pinned tail, stays in Server.
func TestWfAccessGroupFirst(t *testing.T) {
	for _, lvl := range allLevels() {
		visible := visibleScreens(featuresAll(true), lvl)
		locked := withAccessScreens(visible, true, false)
		require.Len(t, locked, len(visible)+2)
		require.Equal(t, []string{"access", "approvers"}, []string{locked[0].ID, locked[1].ID}, "%s: Access first", lvl)
		require.Equal(t, visible, locked[2:], "%s: the rest in registry order", lvl)
		groups := navGroups(locked, "", navCounts{})
		require.Equal(t, groupAccess, groups[0].Label)
		seen := map[string]bool{}
		for _, g := range groups {
			require.False(t, seen[g.Label], "%s: group %q is contiguous", lvl, g.Label)
			seen[g.Label] = true
		}

		grants := withAccessScreens(visible, false, true)
		require.Equal(t, "access", grants[0].ID, "%s: read grants put Access requests first", lvl)
		require.Equal(t, visible, grants[1:])
		require.Equal(t, visible, withAccessScreens(visible, false, false), "neither: the registry's screens")

		withDevices := withAccessScreens(withDevicesScreen(visible, true), true, false)
		ids := make([]string, len(withDevices))
		for i, sc := range withDevices {
			ids[i] = sc.ID
		}
		require.Equal(t, "access", ids[0])
		at := slices.Index(ids, "devices")
		require.Equal(t, "settings", ids[at+1], "Devices closes the Server group, above Settings")
	}

	// Rendered, with a locked project (Config.Approvals), at every level.
	for _, lvl := range config.ConsoleLevels {
		lc := signinNewLockedConsole(t, func(cfg *Config) { cfg.Level = lvl })
		status, page := lc.get(t, "/console/memories", "")
		require.Equal(t, http.StatusOK, status)
		secs := wfSections(t, page)
		require.Equal(t, groupAccess, secs[0], "%s: the Access group comes first", lvl)
		require.Equal(t, groupLive, secs[1], lvl)
		nav := sbNav(t, page)
		access, approvers, overview := strings.Index(nav, `href="/console/access"`),
			strings.Index(nav, `href="/console/approvers"`), strings.Index(nav, `href="/console/"`)
		require.True(t, access >= 0 && approvers > access && overview > approvers,
			"%s: Access requests, then Approvers, then the Overview", lvl)
	}

	// Rendered, with read grants only (Config.Grants), at Basic.
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	cs, err := New(Config{DB: db, Events: rec, APIKey: testKey, Level: "basic",
		Grants: approval.NewGrants(approval.GrantsConfig{DB: db, Events: rec, LeaseCap: 8 * time.Hour})})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	page := sbPage(t, mux, "/console/memories")
	require.Equal(t, []string{"Access", "Live", "Work", "Knowledge"}, wfSections(t, page))
	nav := sbNav(t, page)
	require.Contains(t, nav, `href="/console/access"`)
	require.NotContains(t, nav, `href="/console/approvers"`, "no approver credentials without peer_auth")
	require.Less(t, strings.Index(nav, `href="/console/access"`), strings.Index(nav, `href="/console/"`))
}

// The Mishaps badge follows the reviewed stamp: nothing at zero, a report
// from before the stamp is not counted, one after it is, Mark reviewed clears
// it, and it always equals the Overview's mishap item.
func TestWfMishapsBadge_FollowsTheReviewedStamp(t *testing.T) {
	c := ndNew(t, "basic", config.Features{})
	ctx := context.Background()
	now := time.Now().UTC()
	badge := func() sbBadge { return sbBadges(t, sbPage(t, c.mux, "/console/memories"))["/console/mishaps"] }
	item := func() int {
		for _, card := range ndRead(t, c.mux).Attention {
			if card.Kind == needMishaps {
				return card.Count
			}
		}
		return 0
	}

	page := sbPage(t, c.mux, "/console/memories")
	require.Contains(t, sbNav(t, page), `href="/console/mishaps"`, "the row is there with nothing to review")
	require.False(t, badge().Drawn, "zero draws nothing")
	require.NotContains(t, sbNav(t, page), "data-waiting")

	ndMishap(t, c.db, "example-ops", "shell-trap: a bare == died", now.Add(-3*time.Hour))
	require.Equal(t, sbBadge{Drawn: true, Kind: "queue", Count: 1}, badge(), "before the first review every report counts")
	_, err := store.MarkMishapsReviewed(ctx, c.db, now.Add(-2*time.Hour))
	require.NoError(t, err)
	require.False(t, badge().Drawn, "a report before the stamp is not counted")
	require.Zero(t, item())

	ndMishap(t, c.db, "example-ops", "wrong-target: the live config", now.Add(-time.Hour))
	ndMishap(t, c.db, "", "other: a global one", now.Add(-30*time.Minute))
	require.Equal(t, sbBadge{Drawn: true, Kind: "queue", Count: 2}, badge(), "the reports after the stamp")
	require.Equal(t, item(), badge().Count, "the badge is the Overview's mishap item")
	page = sbPage(t, c.mux, "/console/memories")
	require.Contains(t, sbNav(t, page), "data-waiting", "a queue with something in it marks the nav")
	require.Regexp(t, `href="/console/mishaps"[^>]*>.*?<span class="count" data-badge="queue" title="Mishaps reported since you last marked them reviewed">2</span>`,
		sbNav(t, page))

	rr := ndPost(c.mux, "/console/mishaps/reviewed", url.Values{"through": {core.FormatTime(now)}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	require.False(t, badge().Drawn, "Mark reviewed clears it")
	require.Zero(t, item())

	// A stamp that does not read counts every report: more, never fewer.
	require.NoError(t, store.SetSetting(ctx, c.db, store.SettingMishapsReviewed, "not a time"))
	require.Equal(t, 3, badge().Count)
	require.Equal(t, 3, item())
}

// At Basic the sidebar shows Projects, Plans and Tasks with their queues, and
// none of their pages, the list or an item's own, carries the level banner.
func TestWfBasicShowsTheWork(t *testing.T) {
	c := ndNew(t, "basic", config.Features{})
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, c.db, "example-ops", "example-ops")
	require.NoError(t, err)
	gate := ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Sign the renewal", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign the renewal"}})
	step := ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Order the disks", PlanSlug: "ops-rollout"})
	ndCapture(t, c.mgr, "example-ops", "ops-rollout", plans.StatusPresented)

	page := sbPage(t, c.mux, "/console/memories")
	badges := sbBadges(t, page)
	for _, href := range []string{"/console/projects", "/console/plans", "/console/tasks"} {
		_, ok := badges[href]
		require.True(t, ok, "%s is in the Basic sidebar", href)
	}
	require.Equal(t, sbBadge{Drawn: true, Kind: "queue", Count: 1}, badges["/console/tasks"], "the gate on the owner")
	require.Equal(t, sbBadge{Drawn: true, Kind: "queue", Count: 1}, badges["/console/plans"], "the presented capture")
	require.False(t, badges["/console/projects"].Drawn, "an inventory carries no number")

	for _, path := range []string{"/console/projects", "/console/projects/example-ops", "/console/plans",
		"/console/plans/ops-rollout?project=example-ops", "/console/tasks", "/console/tasks/" + gate,
		"/console/tasks/" + step} {
		rr := getPeek(t, c.mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.NotContains(t, rr.Body.String(), "data-level-banner", "%s carries no level banner at Basic", path)
	}
	for _, id := range []string{"projects", "plans", "tasks"} {
		require.Nil(t, levelBannerFor(id, levelBasic), id)
		sc, ok := screenByID(id)
		require.True(t, ok)
		require.Equal(t, levelBasic, sc.Min, id)
	}
}

// The sidebar, the ? sheet, the g-chords, the palette's Jump to and the docs
// matrix all derive from the one registry: the scripts read the rendered
// links (no list of their own), the rendered links are the registry's in its
// order with their keys, and the docs matrix is the registry's rows in order.
func TestWfNavSurfacesDeriveFromTheRegistry(t *testing.T) {
	shell, search := string(shellJS), string(searchJS)
	require.Contains(t, shell, "document.querySelectorAll('nav.nav a[data-key]')",
		"the ? sheet and the g-chords read the sidebar's links")
	require.Contains(t, search, "document.querySelectorAll('nav.nav a[href]')",
		"the palette's Jump to reads the sidebar's links and its hidden jump rows")

	_, mux := newConsoleLevel(t, featuresAll(true), "advanced")
	nav := sbNav(t, sbPage(t, mux, "/console/settings"))
	linkRe := regexp.MustCompile(`<a href="(/console/[^"]*)" data-key="([^"]*)" data-tip="([^"]*)" class="[^"]*">.*?<span class="nav-label">([^<]*)</span>`)
	var got, want []string
	for _, m := range linkRe.FindAllStringSubmatch(nav, -1) {
		require.Equal(t, m[3], m[4], "the tip and the label are one name")
		got = append(got, m[1]+" "+m[2]+" "+m[4])
	}
	for _, sc := range visibleScreens(featuresAll(true), levelAdvanced) {
		if sc.NavRow {
			want = append(want, sc.Href+" "+sc.Key+" "+sc.Label)
		}
	}
	require.Equal(t, want, got)
	require.Contains(t, got, "/console/mishaps f Mishaps")

	var rows, labels []string
	for _, r := range LevelMatrix() {
		if r.Group == MatrixScreens {
			rows = append(rows, r.Name)
			switch r.Name {
			case "Mishaps", "Projects", "Plans", "Tasks":
				require.Equal(t, "basic", r.Min, r.Name)
				require.Empty(t, r.Note, "%s has a sidebar row", r.Name)
			}
		}
	}
	for _, sc := range screenRegistry() {
		labels = append(labels, sc.Label)
	}
	require.Equal(t, labels, rows, "the docs matrix lists the screens in registry order")
}

// A JSON caller is level-blind: the nav order and the Basic work screens
// change what the console draws, never what a JSON answer carries.
func TestWfJSONIsLevelBlind(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	seedLevelFleet(t, db)
	for _, path := range []string{"/console/tasks?format=json", "/console/plans?format=json",
		"/console/projects?format=json", "/console/mishaps?format=json"} {
		var answers []map[string]any
		for _, lvl := range config.ConsoleLevels {
			setLevel(t, db, lvl)
			answers = append(answers, rawJSON(t, mux, path))
		}
		require.Equal(t, answers[0], answers[1], path)
		require.Equal(t, answers[0], answers[2], path)
	}
	req := httptest.NewRequest(http.MethodGet, "/console/tasks?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	require.NotContains(t, do(mux, req).Body.String(), "data-level-banner")
}
