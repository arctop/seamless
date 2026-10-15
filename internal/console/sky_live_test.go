package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// skyFixture is a sky of n memories across three scopes, a third of them
// surfaced within the last day.
func skyFixture(t *testing.T, now time.Time, n int) ([]core.Memory, map[string]store.RetrievalStat) {
	t.Helper()
	var mems []core.Memory
	stats := map[string]store.RetrievalStat{}
	for i := range n {
		m := core.Memory{
			ID: mustID(t), Kind: core.MemoryKinds[i%len(core.MemoryKinds)], Name: fmt.Sprintf("m-%02d", i),
			Project: []string{"alpha", "beta", ""}[i%3], Created: now.Add(-90 * 24 * time.Hour),
		}
		mems = append(mems, m)
		if i%3 == 0 {
			at := now.Add(-time.Duration(i) * time.Minute)
			stats[m.ID] = store.RetrievalStat{InjectCount: 1, LastInjectedAt: &at}
		}
	}
	return mems, stats
}

// A page that holds nothing gets the whole panel, one that holds the current
// chart gets a keep stub, and one that holds an earlier chart gets only what
// changed: here one star surfacing again inside the day ring, so every frame
// layer and the readout are kept as they are.
func TestSkyLedger_AnswersWholeKeepOrDelta(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	mems, stats := skyFixture(t, now, 60)
	var ledger skyLedger

	first := buildSky(mems, stats, now)
	ledger.answer(first, "")
	require.Empty(t, first.Base, "a page holding no chart gets the whole panel")
	require.False(t, first.Keep)
	require.Regexp(t, `^[0-9a-f]{16}$`, first.Version)
	require.Equal(t, 60, strings.Count(string(first.SVG), `<g id="s-`))
	require.Contains(t, string(first.SVG), `<g class="sky-stars" id="sky-stars">`)

	same := buildSky(mems, stats, now)
	ledger.answer(same, first.Version)
	require.True(t, same.Keep, "nothing changed: the page keeps its panel")
	require.Equal(t, first.Version, same.Base)

	again := mems[3].ID // surfaced 3 minutes ago, so still in the day ring
	at := now.Add(-time.Second)
	moved := map[string]store.RetrievalStat{}
	for id, st := range stats {
		moved[id] = st
	}
	moved[again] = store.RetrievalStat{InjectCount: 2, LastInjectedAt: &at}
	next := buildSky(mems, moved, now)
	ledger.answer(next, first.Version)
	require.False(t, next.Keep)
	require.Equal(t, first.Version, next.Base)
	require.NotEqual(t, first.Version, next.Version)
	svg := string(next.SVG)
	require.Equal(t, 1, strings.Count(svg, `<g id="s-`), "only the star that changed travels")
	require.Contains(t, svg, `<g id="s-`+again+`" class="st warm"`)
	require.Contains(t, svg, `<g class="sky-stars" id="sky-stars" data-live-patch>`)
	require.Contains(t, svg, `<defs id="sky-defs" data-live-keep></defs>`)
	for _, layer := range []string{"sky-grid", "sky-wedges", "sky-ticks", "sky-labels", "sky-core"} {
		require.Contains(t, svg, `<g id="`+layer+`" data-live-keep></g>`, "the %s layer did not change", layer)
	}
	require.True(t, next.KeepReadout, "no ring changed its count")
	require.True(t, next.KeepKinds)

	var forgetful skyLedger
	whole := buildSky(mems, moved, now)
	forgetful.answer(whole, first.Version)
	require.Empty(t, whole.Base, "a chart this daemon does not remember gets the whole panel")
	require.Equal(t, 60, strings.Count(string(whole.SVG), `<g id="s-`))
}

// A star that arrived travels whole and goes in the patch; one that left is a
// gone stub; and the ledger forgets a chart once it has aged out.
func TestSkyDelta_ArrivalsDeparturesAndAgedOutCharts(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	mems, stats := skyFixture(t, now, 40)
	var ledger skyLedger
	base := buildSky(mems, stats, now)
	ledger.answer(base, "")

	left := mems[7]
	gone := now.Add(-time.Minute)
	later := append([]core.Memory(nil), mems...)
	later[7].InvalidAt = &gone
	arrived := core.Memory{ID: mustID(t), Kind: left.Kind, Name: "arrived", Project: left.Project, Created: now.Add(-90 * 24 * time.Hour)}
	later = append(later, arrived)
	next := buildSky(later, stats, now)
	ledger.answer(next, base.Version)
	svg := string(next.SVG)
	require.Contains(t, svg, `<g id="s-`+left.ID+`" data-live-gone></g>`)
	require.Regexp(t, `<g id="s-`+arrived.ID+`" class="st"[^>]*data-n="arrived"`, svg)
	require.NotContains(t, svg, `<g id="s-`+mems[8].ID+`"`, "an unchanged star does not travel")

	for i := range skyLedgerSize {
		churn := map[string]store.RetrievalStat{}
		at := now.Add(-time.Duration(i+1) * time.Second)
		churn[mems[0].ID] = store.RetrievalStat{InjectCount: i + 10, LastInjectedAt: &at}
		ledger.answer(buildSky(mems, churn, now), "")
	}
	aged := buildSky(later, stats, now)
	ledger.answer(aged, base.Version)
	require.Empty(t, aged.Base, "after skyLedgerSize newer charts the base has aged out")
}

// Ring boundaries hold while one memory changes ring, so the live morph moves
// that star alone; and snapping never closes a ring, whatever the counts.
func TestRingRadii_HoldWhileOneMemoryChangesRing(t *testing.T) {
	bandN := [skyBands]int{190, 200, 360, 110, 74}
	moved := bandN
	moved[bandMonth]--
	moved[bandDay]++
	require.Equal(t, ringRadii(bandN, 934), ringRadii(moved, 934))

	for _, counts := range [][skyBands]int{{0, 0, 0, 0, 1}, {1, 0, 0, 0, 0}, {5000, 0, 0, 0, 1}, {3, 1, 4, 1, 5}, {0, 900, 0, 900, 0}} {
		total := 0
		for _, n := range counts {
			total += n
		}
		radii := ringRadii(counts, total)
		require.Equal(t, skyCore, radii[0])
		require.Equal(t, skyRim, radii[skyBands])
		for b := range skyBands {
			require.Greater(t, radii[b+1], radii[b], "ring %d keeps a width for counts %v", b, counts)
		}
	}
}

// Wedges hold while a scope gains a memory, so a write moves no other star;
// a scope's wedge still follows its share of the memories.
func TestLayoutWedges_HoldWhileAScopeGainsAMemory(t *testing.T) {
	scopes := func(sizes []int) ([]*skyScope, int) {
		var out []*skyScope
		total := 0
		for i, n := range sizes {
			out = append(out, &skyScope{slug: fmt.Sprintf("scope-%d", i), n: n})
			total += n
		}
		return out, total
	}
	sizes := []int{300, 200, 150, 100, 60, 40, 20, 10, 5, 3, 2, 1}
	before, total := scopes(sizes)
	layoutWedges(before, total)
	grown := append([]int(nil), sizes...)
	grown[0]++
	after, total := scopes(grown)
	layoutWedges(after, total)
	for i := range before {
		require.Equal(t, before[i].a0, after[i].a0, "wedge %d", i)
		require.Equal(t, before[i].a1, after[i].a1, "wedge %d", i)
	}
	for i := 1; i < len(before); i++ {
		require.GreaterOrEqual(t, before[i-1].a1-before[i-1].a0, before[i].a1-before[i].a0, "a larger scope never gets a narrower wedge")
	}
}

// skyVersion is the chart version a page's sky panel names.
func skyVersion(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`id="sky" aria-label="Knowledge sky" data-live-v="([0-9a-f]{16})"`).FindStringSubmatch(body)
	require.NotNil(t, m, "the page has a sky panel naming its chart")
	return m[1]
}

// liveGet is the live client's fetch: X-Seam-Live, plus the chart it holds.
func liveGet(t *testing.T, mux *http.ServeMux, path, held string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set(liveHeader, "1")
	if held != "" {
		req.Header.Set(skyHeader, held)
	}
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	return rr
}

// The Overview's live refresh carries the sky only as far as it changed since
// the chart the page holds: a keep stub when nothing did, the one changed star
// when a memory surfaced again. A version the daemon never drew, or one sent
// outside the console's own fetches, gets the whole panel.
func TestOverview_LiveRefreshSendsOnlyWhatChanged(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)
	var ids []string
	for i := range 12 {
		ids = append(ids, writeMemory(t, mgr, core.KindGotcha, "seamless", fmt.Sprintf("live-%02d", i), "a memory").ID)
	}
	surface := func(id string) {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventInjected, ItemID: id, Payload: map[string]any{"hook": "session-start"}})
		require.NoError(t, err)
	}
	surface(ids[3])

	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	held := skyVersion(t, page.Body.String())
	require.Equal(t, 12, strings.Count(page.Body.String(), `<g id="s-`))

	quiet := liveGet(t, mux, "/console/", held)
	require.Equal(t, "no-store", quiet.Header().Get("Cache-Control"))
	body := quiet.Body.String()
	require.Contains(t, body, `<section class="ov2-panel sky-panel" id="sky" data-live-keep data-live-base="`+held+`"></section>`)
	require.NotContains(t, body, `<g id="s-`)

	surface(ids[3]) // the same memory again: still in the day ring
	changed := liveGet(t, mux, "/console/", held).Body.String()
	require.Contains(t, changed, `data-live-base="`+held+`"`)
	require.Contains(t, changed, `<g class="sky-stars" id="sky-stars" data-live-patch>`)
	require.Equal(t, 1, strings.Count(changed, `<g id="s-`))
	require.Regexp(t, `<g id="s-`+ids[3]+`" class="st warm new"[^>]*data-i="2"`, changed)
	require.Contains(t, changed, `<div class="sky-summary" id="sky-summary" data-live-keep></div>`)
	require.NotEqual(t, held, skyVersion(t, changed))

	unknown := liveGet(t, mux, "/console/", "0000000000000000").Body.String()
	require.NotContains(t, unknown, "data-live-base")
	require.Equal(t, 12, strings.Count(unknown, `<g id="s-`))

	req := httptest.NewRequest(http.MethodGet, "/console/", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set(skyHeader, held)
	plain := do(mux, req).Body.String()
	require.NotContains(t, plain, "data-live-base", "only the console's own fetches get an answer drawn against a held chart")
	require.Equal(t, 12, strings.Count(plain, `<g id="s-`))
}

// The console's own fetches of a page it shows get just what the morph reads
// -- the title, the nav, the account copy, the main view -- and never a copy
// a later navigation could take for the page.
func TestRender_PatchRequestsGetOnlyWhatTheMorphReads(t *testing.T) {
	_, mux := newConsole(t)
	for _, path := range []string{"/console/", "/console/memories", "/console/sessions", "/console/settings"} {
		full := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, full.Code)
		require.Contains(t, full.Body.String(), `<script src="/console/static/navigation.js"></script>`)
		require.Empty(t, full.Header().Get("Cache-Control"), "%s: a page load is a page", path)

		for _, header := range []string{liveHeader, partialHeader} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+testKey)
			req.Header.Set(header, "1")
			rr := do(mux, req)
			require.Equal(t, http.StatusOK, rr.Code)
			require.Equal(t, "no-store", rr.Header().Get("Cache-Control"), "%s %s", path, header)
			body := rr.Body.String()
			for _, part := range []string{`<title>`, `<aside class="sidebar">`, `<nav class="nav"`, `<span class="account-copy">`, `<main class="main" id="main"`} {
				require.Contains(t, body, part, "%s %s: patchPage reads %s", path, header, part)
			}
			for _, chrome := range []string{`rel="stylesheet"`, `<script src="/console/static/`, `id="cmdk"`, `id="live-pill"`, `class="mobile-bar"`} {
				require.NotContains(t, body, chrome, "%s %s: a patch carries no %s", path, header, chrome)
			}
		}
	}
}

// The analytics screens rebuild retrieval stats only when the events a
// rebuild reads have changed, or the decay window has passed.
func TestFreshRetrievalStats_RebuildsOnlyWhenItsInputsMove(t *testing.T) {
	db, svc := newTestService(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)
	record := func(e core.Event) {
		_, err := rec.Record(ctx, e)
		require.NoError(t, err)
	}
	// A row no rebuild would write: it survives exactly as long as no rebuild runs.
	plant := func() {
		_, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO retrieval_stats (item_id, inject_count, read_count) VALUES ('planted', 1, 0)`)
		require.NoError(t, err)
	}
	planted := func() bool {
		_, ok, err := store.GetRetrievalStat(ctx, db, "planted")
		require.NoError(t, err)
		return ok
	}

	record(core.Event{Kind: core.EventInjected, ItemID: "A"})
	svc.freshRetrievalStats(ctx)
	plant()
	svc.freshRetrievalStats(ctx)
	require.True(t, planted(), "nothing a rebuild reads changed")
	record(core.Event{Kind: core.EventToolCall})
	svc.freshRetrievalStats(ctx)
	require.True(t, planted(), "a tool call is not a rebuild input")

	record(core.Event{Kind: core.EventMemoryRead, ItemID: "A"})
	svc.freshRetrievalStats(ctx)
	require.False(t, planted(), "a read is: the stats were rebuilt")
	a, ok, err := store.GetRetrievalStat(ctx, db, "A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, a.ReadCount)

	plant()
	svc.stats.at = time.Now().Add(-statsDecayWindow)
	svc.freshRetrievalStats(ctx)
	require.False(t, planted(), "past the decay window the stats are rebuilt anyway")
}

// The live client and the in-place loads send the chart a page holds, the
// morph applies an answer drawn against it, and a page that turns out not to
// hold it forgets what it holds and is answered whole.
func TestLiveClients_SendHeldViewsAndRecoverWhole(t *testing.T) {
	nav := string(navigationJS)
	for _, marker := range []string{"data-live-base", "data-live-keep", "data-live-patch", "data-live-gone"} {
		require.Contains(t, nav, "'"+marker+"'", "the morph knows the %s marker", marker)
	}
	require.Contains(t, nav, "headers['"+skyHeader+"']", "the held chart travels in the header the server reads")
	require.Contains(t, nav, "'"+partialHeader+"': '1'")
	require.Contains(t, nav, "if (marker(attr.name)) continue;", "a marker is never copied onto the page")
	require.Contains(t, nav, "forgetHeldViews();")
	require.Contains(t, nav, "if (!mutation) heldViews(headers);", "a POST is answered whole, so it is never sent twice")
	require.Contains(t, nav, "stale(error) && !mutation && !options.whole")

	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	require.Contains(t, string(layout), "'"+liveHeader+"': '1'")
	require.Contains(t, string(layout), "window.SeamConsole.heldViews(headers);")
	require.Contains(t, string(layout), "if (window.SeamConsole && window.SeamConsole.stale(error)) { again = true; return; }")
}
