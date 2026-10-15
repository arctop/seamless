package console

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// The Overview draws every active memory as a star in its scope's wedge, with
// the readout -- rings, going stale, scopes, kinds -- as text beside it; the
// JSON contract carries none of it.
func TestOverview_RendersKnowledgeSky(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	a := writeMemory(t, mgr, core.KindGotcha, "seamless", "sky-gotcha", "a gotcha")
	b := writeMemory(t, mgr, core.KindDecision, "", "sky-global-decision", "a global decision")

	page := getPeek(t, mux, "/console/")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Regexp(t, `<section class="ov2-panel sky-panel" id="sky" aria-label="Knowledge sky" data-live-v="[0-9a-f]{16}">`, body,
		"a whole panel names its chart, for the page's live refreshes to send back")
	require.Contains(t, body, `class="sky-svg"`)
	require.Contains(t, body, `id="s-`+a.ID+`"`)
	require.Contains(t, body, `id="s-`+b.ID+`"`)
	require.Contains(t, body, `data-n="sky-gotcha"`, "a star carries its name for the tooltip and the search")
	require.Contains(t, body, `data-d="a gotcha"`, "and its description")
	require.Contains(t, body, `data-sky-scope="seamless"`)
	require.Contains(t, body, `data-sky-scope="" data-name="global"`, "the global scope is its own wedge")
	for band := range skyBands {
		require.Contains(t, body, fmt.Sprintf(`data-sky-band="%d"`, band), "every ring has a readout row")
	}
	require.Contains(t, body, `data-sky-kind="gotcha"`, "the kind legend filters the sky")
	require.Contains(t, body, `<script src="/console/static/sky.js"></script>`)
	require.NotContains(t, body, `aria-label="Memory fabric"`, "the sky replaces the separate kind panel")

	// Everything the page script creates lives in nodes the live morph skips.
	for _, owned := range []string{
		`<g class="sky-fx" data-live-skip>`, `<div class="sky-tip" data-live-skip hidden>`,
		`<div class="sky-find" data-live-skip>`, `<div class="sky-view" data-live-skip`,
	} {
		require.Contains(t, body, owned)
	}

	var data map[string]any
	getJSON(t, mux, "/console/?format=json", &data)
	_, hasSky := data["Sky"]
	require.False(t, hasSky, "the sky is HTML-only")
}

// A ring is when a memory last surfaced, on the console's own thresholds; the
// marks say what else is true of it.
func TestBuildSky_RingsFollowTheConsoleThresholds(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	day := 24 * time.Hour
	cases := []struct {
		name     string
		created  time.Time
		surfaced *time.Time
		band     skyBand
		stale    bool
	}{
		{"hour-ago", now.Add(-90 * day), ago(time.Hour), bandDay, false},
		{"three-days", now.Add(-90 * day), ago(3 * day), bandWeek, false},
		{"twenty-days", now.Add(-90 * day), ago(20 * day), bandMonth, false},
		{"sixty-days", now.Add(-90 * day), ago(60 * day), bandStale, true},
		{"never-old", now.Add(-60 * day), nil, bandNever, true},
		{"never-young", now.Add(-3 * day), nil, bandNever, false},
	}
	var mems []core.Memory
	stats := map[string]store.RetrievalStat{}
	for _, c := range cases {
		id := mustID(t)
		mems = append(mems, core.Memory{ID: id, Kind: core.KindGotcha, Name: c.name, Project: "p", Created: c.created})
		stats[id] = store.RetrievalStat{ItemID: id, LastInjectedAt: c.surfaced, InjectCount: 3}
	}
	fresh := core.Memory{ID: mustID(t), Kind: core.KindRunbook, Name: "written-this-morning", Project: "p", Created: now.Add(-2 * time.Hour), Favorite: true}
	mems = append(mems, fresh)

	sky := buildSky(mems, stats, now)
	require.NotNil(t, sky)
	svg := string(sky.SVG)
	for i, c := range cases {
		st := starTag(t, svg, mems[i].ID)
		require.Contains(t, st, fmt.Sprintf(`data-b="%d"`, c.band), c.name)
		if c.stale {
			require.Contains(t, st, `data-s="1"`, c.name)
		} else {
			require.NotContains(t, st, `data-s="1"`, c.name)
		}
	}
	require.Contains(t, starTag(t, svg, mems[0].ID), `class="st warm"`, "today's stars twinkle")
	freshTag := starTag(t, svg, fresh.ID)
	require.Contains(t, freshTag, `class="st fav new"`, "a star, and new today")
	require.NotContains(t, freshTag, "data-t=", "never surfaced carries no time")

	require.Equal(t, 7, sky.Total)
	require.Equal(t, 2, sky.Stale)
	var counts []int
	for _, row := range sky.Bands {
		counts = append(counts, row.N)
	}
	require.Equal(t, []int{1, 1, 1, 1, 3}, counts)
	require.Equal(t, fmt.Sprintf("Over %d days ago", staleSurfacedDays), sky.Bands[bandStale].Label)
}

// The readout's "going stale" count is the Overview attention card's, by
// construction: the same predicate over the same stats, never a second opinion.
func TestOverview_SkyStaleCountMatchesTheAttentionCard(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	rec := events.NewRecorder(db)
	write := func(name string, created time.Time) core.Memory {
		m, err := mgr.WriteMemory(ctx, core.Memory{
			ID: mustID(t), Kind: core.KindGotcha, Name: name, Description: "d", Project: "p",
			Body: "b", Created: created, Updated: created, ValidFrom: created,
		})
		require.NoError(t, err)
		return m
	}
	inject := func(m core.Memory, at time.Time) {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventInjected, TS: at, Payload: map[string]any{"item_ids": []any{m.ID}, "hook": "session-start"}})
		require.NoError(t, err)
	}
	inject(write("surfaced-today", now.AddDate(0, 0, -90)), now.Add(-2*time.Hour))
	inject(write("quiet-for-two-months", now.AddDate(0, 0, -90)), now.AddDate(0, 0, -60))
	write("never-and-old", now.AddDate(0, 0, -90))
	write("never-but-new", now.AddDate(0, 0, -2))

	body := getPeek(t, mux, "/console/").Body.String() // rebuilds the stats from the event log
	want, err := store.CountMemoriesUnsurfacedSince(ctx, db, now.AddDate(0, 0, -staleSurfacedDays))
	require.NoError(t, err)
	require.Equal(t, 2, want)
	require.Contains(t, body, "<strong>2</strong> going stale", "the sky's readout")
	require.Contains(t, body, "2 memories going stale", "the attention card")
	require.Equal(t, 2, strings.Count(body, `data-s="1"`), "and exactly those two stars are marked")
}

// Every active memory is a star -- no cap, no sampling -- and a large, skewed
// fleet still renders finite geometry inside the rim.
func TestBuildSky_DrawsEveryMemory(t *testing.T) {
	now := time.Now()
	var mems []core.Memory
	stats := map[string]store.RetrievalStat{}
	for i := range 1500 {
		id := mustID(t)
		// A long tail of scopes: scope k holds roughly 1500/(k+1) memories.
		scope := ""
		for k := 1; k < 30; k++ {
			if i%(k+1) == 0 {
				scope = "scope-" + strconv.Itoa(k)
			}
		}
		mems = append(mems, core.Memory{ID: id, Kind: core.MemoryKinds[i%len(core.MemoryKinds)], Name: "m" + strconv.Itoa(i), Project: scope, Created: now.Add(-100 * 24 * time.Hour)})
		if i%3 > 0 {
			at := now.Add(-time.Duration(i%70) * 24 * time.Hour)
			stats[id] = store.RetrievalStat{LastInjectedAt: &at, Utility: float64(i%10) / 10}
		}
	}
	superseded := time.Now()
	mems = append(mems, core.Memory{ID: mustID(t), Kind: core.KindGotcha, Name: "gone", InvalidAt: &superseded})

	sky := buildSky(mems, stats, now)
	require.NotNil(t, sky)
	svg := string(sky.SVG)
	require.Equal(t, 1500, sky.Total, "an inactive memory is not a star")
	require.Equal(t, 1500, strings.Count(svg, `<g id="s-`))
	require.NotContains(t, svg, "NaN")
	require.NotContains(t, svg, "Inf")

	pos := regexp.MustCompile(`data-x="([-\d.]+)" data-y="([-\d.]+)"`)
	for _, m := range pos.FindAllStringSubmatch(svg, -1) {
		x, _ := strconv.ParseFloat(m[1], 64)
		y, _ := strconv.ParseFloat(m[2], 64)
		r := math.Hypot(x-skyC, y-skyC)
		require.GreaterOrEqual(t, r, skyCore, "a star never sits inside the core")
		require.LessOrEqual(t, r, skyRim, "or beyond the rim")
	}
	require.Nil(t, buildSky(nil, nil, now), "no active memories, no sky")
}

// The layout is a pure function of the memories and their stats: the same
// input renders byte-identical SVG, so a live morph changes nothing that did
// not change.
func TestBuildSky_Deterministic(t *testing.T) {
	now := time.Now()
	var mems []core.Memory
	for i, k := range []core.MemoryKind{core.KindGotcha, core.KindDecision, core.KindRunbook, core.KindConstraint} {
		for j := range 5 {
			mems = append(mems, core.Memory{ID: mustID(t), Kind: k, Name: strings.Repeat("m", i+j+1), Project: []string{"a", "b", ""}[j%3]})
		}
	}
	stats := map[string]store.RetrievalStat{mems[0].ID: {InjectCount: 9, Utility: 0.8, LastInjectedAt: &now}}
	one := buildSky(mems, stats, now)
	two := buildSky(mems, stats, now)
	require.NotNil(t, one)
	require.Equal(t, one.SVG, two.SVG)
	require.Equal(t, 20, one.Total)
	require.Len(t, one.Scopes, 3)
	require.Contains(t, starTag(t, string(one.SVG), mems[0].ID), `class="st hi warm"`,
		"the most-pulled, freshly surfaced memory wears a halo and twinkles")
}

// A star keeps its slot while its scope and ring hold: a newly written memory
// (the highest ULID) takes a free slot and moves no one, and no two stars ever
// share a slot.
func TestSeatCell_NewcomersMoveNoOne(t *testing.T) {
	var ids []string
	for range 40 {
		ids = append(ids, mustID(t))
	}
	slots := skySlots(len(ids) + 1)
	require.Equal(t, slots, skySlots(len(ids)), "the test stays inside one size class")
	before := seatCell(ids, slots)
	after := seatCell(append(append([]string(nil), ids...), mustID(t)), slots)
	require.Equal(t, before, after[:len(ids)])

	seen := map[int]bool{}
	for _, p := range after {
		require.False(t, seen[p], "slot %d is double-booked", p)
		require.Less(t, p, slots)
		seen[p] = true
	}
	for n := 1; n < 2000; n++ {
		require.LessOrEqual(t, float64(n)/float64(skySlots(n)), skyLoad+1e-9, "a cell never runs past its load factor (n=%d)", n)
	}
}

// Rim labels never collide: every name shown claims its own stretch of rim,
// clear of the ring-label gutter. The largest scope's name is always shown.
func TestPlaceRimLabels_ShownLabelsNeverOverlap(t *testing.T) {
	var mems []core.Memory
	sizes := []int{190, 181, 125, 41, 30, 28, 27, 20, 14, 12, 8, 6, 6, 5, 4, 4, 3, 3, 2, 2, 2, 1, 1, 1}
	for k, n := range sizes {
		for range n {
			mems = append(mems, core.Memory{ID: mustID(t), Kind: core.KindGotcha, Name: "m", Project: "scope-with-a-long-name-" + strconv.Itoa(k)})
		}
	}
	sky := buildSky(mems, nil, time.Now())
	require.NotNil(t, sky)

	scopes := make([]*skyScope, 0, len(sizes))
	for i, row := range sky.Scopes {
		sc := &skyScope{slug: row.Slug, n: row.N}
		scopes = append(scopes, sc)
		require.Equal(t, sizes[i], row.N, "scopes run largest first")
	}
	layoutWedges(scopes, len(mems))
	labels := placeRimLabels(scopes)
	require.False(t, labels[0].tight, "the largest scope is always named")
	var shown []rimLabel
	for _, lb := range labels {
		if lb.tight {
			continue
		}
		require.GreaterOrEqual(t, lb.lo, skyGutter/2)
		require.LessOrEqual(t, lb.hi, 2*math.Pi-skyGutter/2)
		for _, o := range shown {
			require.False(t, lb.lo < o.hi && lb.hi > o.lo, "two shown labels overlap")
		}
		shown = append(shown, lb)
	}
	require.Greater(t, len(shown), 3, "more than the three largest scopes get a name on the rim")
	require.Less(t, len(shown), len(labels), "and the slivers wait for a hover")
}

// Agent-written text reaches the page only as escaped attribute values.
func TestBuildSky_EscapesAgentText(t *testing.T) {
	m := core.Memory{
		ID: mustID(t), Kind: core.KindGotcha, Name: "plain-name", Project: "p",
		Description: `"><img src=x onerror=alert(1)>`, Tags: []string{`t"<b>`},
	}
	svg := string(buildSky([]core.Memory{m}, nil, time.Now()).SVG)
	require.NotContains(t, svg, "<img")
	require.NotContains(t, svg, "<b>")
	require.Contains(t, svg, `data-d="&#34;&gt;&lt;img src=x onerror=alert(1)&gt;"`)
}

// The page script is served, stays inside the document, and re-derives its
// state after every live morph.
func TestServeSkyJS(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/sky.js", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "text/javascript")
	js := rr.Body.String()
	require.Contains(t, js, `document.addEventListener('seam:content-updated'`)
	require.Contains(t, js, `document.addEventListener('seam:event'`, "live injections light the sky over the shared stream")
	require.NotContains(t, js, "new EventSource", "never a second stream")
	require.NotContains(t, js, "innerHTML", "agent text is set as text, never parsed as markup")
}

// starTag returns the opening tag of one star in the rendered SVG.
func starTag(t *testing.T, svg, id string) string {
	t.Helper()
	at := strings.Index(svg, `<g id="s-`+id+`"`)
	require.NotEqual(t, -1, at, "no star for %s", id)
	end := strings.Index(svg[at:], ">")
	return svg[at : at+end+1]
}
