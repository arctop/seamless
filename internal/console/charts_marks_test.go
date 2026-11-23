package console

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// chartsMarkPayload pulls a per-mark chart's data-hover payload back out of its
// markup and decodes it, so a test can assert on the readouts the client shows.
// The attribute is HTML-escaped, so the first raw quote after it ends it.
func chartsMarkPayload(t *testing.T, markup string) markData {
	t.Helper()
	const key = ` data-hover="`
	i := strings.Index(markup, key)
	require.GreaterOrEqual(t, i, 0, "chart is missing its readout payload")
	rest := markup[i+len(key):]
	end := strings.Index(rest, `"`)
	require.GreaterOrEqual(t, end, 0, "unterminated data-hover attribute")
	var out markData
	require.NoError(t, json.Unmarshal([]byte(html.UnescapeString(rest[:end])), &out))
	return out
}

// chartsHours is n hourly columns from start.
func chartsHours(start time.Time, n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = start.Add(time.Duration(i) * time.Hour)
	}
	return out
}

// chartsFloats pulls every float out of s, in order.
func chartsFloats(t *testing.T, s string) []float64 {
	t.Helper()
	var out []float64
	for _, m := range regexp.MustCompile(`-?\d+(?:\.\d+)?`).FindAllString(s, -1) {
		f, err := strconv.ParseFloat(m, 64)
		require.NoError(t, err)
		out = append(out, f)
	}
	return out
}

// chartsAttr is one attribute's value from a single tag.
func chartsAttr(t *testing.T, tag, name string) string {
	t.Helper()
	m := regexp.MustCompile(` ` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(tag)
	require.NotNil(t, m, "tag has no %s: %s", name, tag)
	return m[1]
}

// chartsTags is every opening tag that starts with prefix.
func chartsTags(markup, prefix string) []string {
	var out []string
	for rest := markup; ; {
		i := strings.Index(rest, prefix)
		if i < 0 {
			return out
		}
		end := strings.Index(rest[i:], ">")
		if end < 0 {
			return out
		}
		out = append(out, rest[i:i+end+1])
		rest = rest[i+end+1:]
	}
}

const chartsNasty = `"><script>alert(1)</script>`

func TestChartUnit_Figures(t *testing.T) {
	for _, tc := range []struct {
		unit chartUnit
		v    int64
		want string
	}{
		{unitCount, 0, "0"},
		{unitCount, 9, "9"},
		{unitCount, 1284, "1,284"},
		{unitCount, 9999, "9,999"},
		{unitCount, 12400, "12.4k"},
		{unitTokens, 980, "980"},
		{unitTokens, 12400, "12.4k"},
		{unitTokens, 148_000_000, "148M"},
		{unitMinutes, 0, "0m"},
		{unitMinutes, 45, "45m"},
		{unitMinutes, 60, "1h"},
		{unitMinutes, 130, "2h 10m"},
		{unitMinutes, 400, "6h 40m"},
	} {
		require.Equal(t, tc.want, tc.unit.figure(tc.v), "unit %d value %d", tc.unit, tc.v)
	}

	require.Equal(t, "61%", chartShare(61, 100))
	require.Equal(t, "<1%", chartShare(1, 1000), "a sliver is never rounded down to nothing")
	require.Equal(t, "0%", chartShare(0, 10))
	require.Empty(t, chartShare(5, 0), "no whole, no share")
}

// Slots are picked once for the whole span, by its totals, so a series keeps
// its color on every day; the tail past five gets 0 and folds into Other.
func TestAssignSlots_TopFiveByTotalTiesByKey(t *testing.T) {
	got := assignSlots(map[string]int64{"a": 10, "b": 50, "c": 50, "d": 5, "e": 7, "f": 3, "g": 0})
	require.Equal(t, map[string]int{"b": 1, "c": 2, "a": 3, "e": 4, "d": 5, "f": 0, "g": 0}, got)
	require.Empty(t, assignSlots(nil))
}

// Every href a primitive renders is a same-origin path or an in-page anchor,
// escaped; anything else is text, never a link.
func TestSafeHref(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"/console/diary?day=2026-11-20", "/console/diary?day=2026-11-20", true},
		{"/console/diary?day=2026-11-20&m=2026-11", "/console/diary?day=2026-11-20&amp;m=2026-11", true},
		{"#tasks", "#tasks", true},
		{`/console/x"onmouseover="alert(1)`, "/console/x&#34;onmouseover=&#34;alert(1)", true},
		{"", "", false},
		{"/", "", false},
		{"#", "", false},
		{"javascript:alert(1)", "", false},
		{"JavaScript:alert(1)", "", false},
		{"data:text/html,x", "", false},
		{"https://example.com/", "", false},
		{"//example.com/", "", false},
		{`/\example.com/`, "", false},
		{"relative/path", "", false},
		{"?day=2026-11-20", "", false},
		{"/path with space", "", false},
		{"/a\tb", "", false},
		{"/a\nb", "", false},
		{" /lead", "", false},
		{"/nul\x00", "", false},
		{"/caf\u00e9", "", false},
	} {
		got, ok := safeHref(tc.in)
		require.Equal(t, tc.ok, ok, "%q", tc.in)
		require.Equal(t, tc.want, got, "%q", tc.in)
	}

	for in, ok := range map[string]bool{"#tasks": true, "#plan:diary": true, "#": false, "#bad id": false, "/console/tasks": false, "javascript:x": false} {
		_, got := safeAnchor(in)
		require.Equal(t, ok, got, "%q", in)
	}
}

func TestMarkAttr_EscapesIntoTheAttribute(t *testing.T) {
	got := markAttr(markData{Marks: []markReadout{{Label: chartsNasty, Value: "1"}}})
	body := strings.TrimSuffix(strings.TrimPrefix(got, ` data-hover="`), `"`)
	for _, c := range []string{`"`, "<", ">"} {
		require.NotContains(t, body, c, "a label must not be able to break out of the attribute")
	}
	require.Equal(t, chartsNasty, chartsMarkPayload(t, `<div`+got+`>`).Marks[0].Label, "escaped, not mangled")
	require.Empty(t, markAttr(markData{}), "no marks, no readout to arm")
}

func TestRingChart(t *testing.T) {
	t.Run("nothing to share renders nothing", func(t *testing.T) {
		require.Empty(t, ringChart(ringSpec{}))
		require.Empty(t, ringChart(ringSpec{Slices: []ringSlice{{Name: "a", Slot: 1}, {Name: "b", Slot: 2, Value: -4}}}))
		require.Empty(t, ringTable(ringSpec{}))
	})

	t.Run("one series draws no ring, only its legend line", func(t *testing.T) {
		got := string(ringChart(ringSpec{Label: "Model tokens by agent", Unit: unitTokens,
			Slices: []ringSlice{{Name: "Claude Code / example-model", Slot: 1, Value: 148_000_000}}}))
		require.Contains(t, got, `class="chart chart-ring single"`)
		require.NotContains(t, got, "<svg")
		require.NotContains(t, got, "data-hover", "one line has nothing to point at")
		require.Equal(t, 1, strings.Count(got, `class="chart-legend-row series-1"`))
		require.Contains(t, got, `<span class="chart-legend-fig">148M</span>`)
		require.NotContains(t, got, "chart-legend-share", "a whole is not a share")
		require.Contains(t, got, `aria-label="Model tokens by agent: 148M in total. Claude Code / example-model 100% (148M)."`)

		// Two unslotted series are still one segment: Other, alone, on a line.
		got = string(ringChart(ringSpec{Slices: []ringSlice{{Name: "a", Value: 3}, {Name: "b", Value: 4}}}))
		require.Contains(t, got, "chart-ring single")
		require.Contains(t, got, `<span class="chart-legend-name">Other (2)</span>`)
	})

	two := ringSpec{Label: "Model tokens by agent", Unit: unitTokens, Core: "tokens", Slices: []ringSlice{
		{Name: "Codex / example-model", Slot: 2, Value: 100},
		{Name: "Claude Code / example-model", Slot: 1, Value: 300},
	}}

	t.Run("segments in slot order, the total in the center, a legend row each", func(t *testing.T) {
		got := string(ringChart(two))
		segs := chartsTags(got, `<circle class="chart-ring-seg`)
		require.Len(t, segs, 2)
		require.Contains(t, segs[0], "series-1", "slot order, not input order: neighbors are the palette's validated neighbors")
		require.Contains(t, segs[1], "series-2")
		require.Len(t, chartsTags(got, `<circle class="chart-hit"`), 2, "each segment has its own, larger hit arc")
		require.Contains(t, got, `<div class="chart-ring-core" aria-hidden="true"><strong>400</strong><span>tokens</span></div>`)
		require.Contains(t, got, `<li class="chart-legend-row series-1" data-lit="0">`)
		require.Contains(t, got, `<span class="chart-legend-share">75%</span><span class="chart-legend-fig">300</span>`)
		require.Contains(t, got, `<li class="chart-legend-row series-2" data-lit="1">`)
		require.Contains(t, got, `role="img" aria-label="Model tokens by agent: 400 in total. Claude Code / example-model 75% (300), Codex / example-model 25% (100)." tabindex="0"`)
		require.Contains(t, got, `class="chart-svg chart-ring-svg chart-walk"`, "the ring is one keyboard tab stop")
	})

	t.Run("geometry: the gaps and segments close the circle from twelve o'clock", func(t *testing.T) {
		got := string(ringChart(two))
		c := 2 * math.Pi * ringR
		var drawn float64
		for _, seg := range chartsTags(got, `<circle class="chart-ring-seg`) {
			dash := chartsFloats(t, chartsAttr(t, seg, "stroke-dasharray"))
			require.InDelta(t, c, dash[1], 0.01, "one dash per circumference")
			drawn += dash[0] + ringGap
		}
		require.InDelta(t, c, drawn, 0.02, "segments plus their 2px gaps are the whole ring")
		first := chartsFloats(t, chartsAttr(t, chartsTags(got, `<circle class="chart-ring-seg`)[0], "transform"))
		require.InDelta(t, -90+ringGap/2/c*360, first[0], 0.01, "the first segment starts at twelve, half a gap in")

		// The draw-on sweeps once around: the first starts at once, the last
		// ends at the sweep's end.
		var ends []float64
		for _, seg := range chartsTags(got, `<circle class="chart-ring-seg`) {
			f := chartsFloats(t, chartsAttr(t, seg, "style")[strings.Index(chartsAttr(t, seg, "style"), "--d:"):])
			ends = append(ends, f[0], f[0]+f[1])
		}
		require.Equal(t, 0.0, ends[0])
		require.InDelta(t, ringSweepT, ends[len(ends)-1], 1)
		require.InDelta(t, ends[1], ends[2], 1, "the second picks up where the first ended")
	})

	t.Run("hover: each readout sits on its own segment", func(t *testing.T) {
		hov := chartsMarkPayload(t, string(ringChart(two)))
		require.Len(t, hov.Marks, 2)
		require.Equal(t, ringBox, hov.W)
		for _, m := range hov.Marks {
			require.NotNil(t, m.At)
			require.InDelta(t, ringR, math.Hypot(m.At[0]-ringBox/2, m.At[1]-ringBox/2), 0.02, "the anchor is on the ring")
		}
		require.Equal(t, "Claude Code / example-model", hov.Marks[0].Label)
		require.Equal(t, "300", hov.Marks[0].Value)
		require.Equal(t, "75% of 400", hov.Marks[0].Sub)
		require.Greater(t, hov.Marks[0].At[1], ringBox/2, "75% from twelve o'clock centers low on the ring")
	})

	t.Run("the Other fold: five slots, the rest one neutral segment", func(t *testing.T) {
		r := ringSpec{Unit: unitTokens, Slices: []ringSlice{
			{Name: "s1", Slot: 1, Value: 70}, {Name: "s2", Slot: 2, Value: 60}, {Name: "s3", Slot: 3, Value: 50},
			{Name: "s4", Slot: 4, Value: 40}, {Name: "s5", Slot: 5, Value: 30},
			{Name: "tail-a", Slot: 0, Value: 5}, {Name: "tail-b", Slot: 0, Value: 9},
		}}
		got := string(ringChart(r))
		segs := chartsTags(got, `<circle class="chart-ring-seg`)
		require.Len(t, segs, 6, "at most five segments plus Other")
		require.Contains(t, segs[5], "series-other")
		require.Contains(t, got, `<span class="chart-legend-name">Other (2)</span>`)
		other := chartsMarkPayload(t, got).Marks[5]
		require.Equal(t, "14", other.Value)
		require.Equal(t, []markRow{{Tone: "series-other", Value: "9", Name: "tail-b"}, {Tone: "series-other", Value: "5", Name: "tail-a"}}, other.Rows,
			"Other's readout names who is in it, heaviest first")
		require.Contains(t, string(ringTable(r)), `<th scope="row">Other (2): tail-b, tail-a</th>`)

		// A slot two series claim keeps the heavier; the lighter folds, and a slot
		// out of range folds too. A lone folded series keeps its own name.
		got = string(ringChart(ringSpec{Slices: []ringSlice{
			{Name: "light", Slot: 1, Value: 2}, {Name: "heavy", Slot: 1, Value: 8}, {Name: "far", Slot: 9, Value: 0},
		}}))
		require.Contains(t, got, `<li class="chart-legend-row series-1" data-lit="0"><i class="chart-swatch" aria-hidden="true"></i><span class="chart-legend-name">heavy</span>`)
		require.Contains(t, got, `<li class="chart-legend-row series-other" data-lit="1"><i class="chart-swatch" aria-hidden="true"></i><span class="chart-legend-name">light</span>`)
		got = string(ringChart(ringSpec{Slices: []ringSlice{{Name: "kept", Slot: 1, Value: 2}, {Name: "far", Slot: 9, Value: 1}}}))
		require.Contains(t, got, `series-other" data-lit="1"><i class="chart-swatch" aria-hidden="true"></i><span class="chart-legend-name">far</span>`)
	})

	t.Run("names are escaped everywhere they land", func(t *testing.T) {
		r := ringSpec{Label: chartsNasty, Core: chartsNasty, Slices: []ringSlice{{Name: chartsNasty, Slot: 1, Value: 2}, {Name: "b", Slot: 2, Value: 1}}}
		for _, got := range []string{string(ringChart(r)), string(ringTable(r))} {
			require.NotContains(t, got, "<script")
			require.Contains(t, got, "&lt;script&gt;")
		}
		require.Equal(t, chartsNasty, chartsMarkPayload(t, string(ringChart(r))).Marks[0].Label)
	})
}

func TestRhythmChart(t *testing.T) {
	zone := time.FixedZone("PST", -8*3600)
	day := chartsHours(time.Date(2026, 11, 20, 0, 0, 0, 0, zone), 24)
	vals := func(set map[int]int64) []int64 {
		v := make([]int64, 24)
		for h, n := range set {
			v[h] = n
		}
		return v
	}
	two := rhythmSpec{Label: "Agent time by hour", Unit: unitMinutes, Hours: day, Series: []rhythmSeries{
		{Name: "Codex / example-model", Slot: 2, Values: vals(map[int]int64{14: 40, 15: 10})},
		{Name: "Claude Code / example-model", Slot: 1, Values: vals(map[int]int64{9: 30, 14: 20})},
	}}

	t.Run("an empty day renders nothing", func(t *testing.T) {
		require.Empty(t, rhythmChart(rhythmSpec{}))
		require.Empty(t, rhythmChart(rhythmSpec{Hours: day, Series: []rhythmSeries{{Name: "a", Slot: 1, Values: make([]int64, 24)}}}))
		require.Empty(t, rhythmTable(rhythmSpec{Hours: day}))
	})

	t.Run("one series: a column per hour, no key, four hour ticks", func(t *testing.T) {
		got := string(rhythmChart(rhythmSpec{Unit: unitMinutes, Hours: day, Series: []rhythmSeries{{Name: "a", Slot: 1, Values: vals(map[int]int64{3: 12})}}}))
		require.Len(t, chartsTags(got, `<rect class="chart-hit"`), 24, "every hour answers, quiet ones too")
		require.Len(t, chartsTags(got, `<g class="chart-col"`), 1, "only an hour with something in it draws a column")
		require.NotContains(t, got, "chart-key", "one series needs no legend")
		ticks := regexp.MustCompile(`<span(?: class="first")? style="left:[\d.]+%">(\d\d:\d\d)</span>`).FindAllStringSubmatch(got, -1)
		require.Len(t, ticks, 4)
		for i, want := range []string{"00:00", "06:00", "12:00", "18:00"} {
			require.Equal(t, want, ticks[i][1])
		}
		require.Contains(t, got, `<span class="first" style="left:0.71%">00:00</span>`)
		require.Contains(t, got, `<span style="left:50.00%">12:00</span>`)
	})

	t.Run("stacked by slot, a 2px gap, the data end rounded", func(t *testing.T) {
		got := string(rhythmChart(two))
		col := got[strings.Index(got, `<g class="chart-col" data-i="14"`):]
		col = col[:strings.Index(col, "</g>")]
		// The columns rise once, staggered by the hour, each from its own foot
		// on the baseline: its slot's center, in the svg's user units.
		n := float64(len(two.Hours))
		foot := fmt.Sprintf(`style="--d:196ms;transform-origin:%.2fpx %gpx"`, rhythmPadX+14.5*(rhythmW-2*rhythmPadX)/n, rhythmBase)
		require.Contains(t, col, foot)
		rect := chartsTags(col, `<rect class="chart-seg`)
		path := chartsTags(col, `<path class="chart-seg`)
		require.Len(t, rect, 1)
		require.Len(t, path, 1)
		require.Contains(t, rect[0], "series-1", "slot 1 sits at the baseline whatever the input order")
		require.Contains(t, path[0], "series-2", "the top part carries the rounded data end")
		require.Contains(t, chartsAttr(t, path[0], "d"), "A4.00 4.00 0 0 1", "a 4px rounded end")

		d := chartsFloats(t, chartsAttr(t, path[0], "d"))
		pathBottom := d[1]
		rectTop := chartsFloats(t, chartsAttr(t, rect[0], "y"))[0]
		rectHeight := chartsFloats(t, chartsAttr(t, rect[0], "height"))[0]
		require.InDelta(t, rhythmGap, rectTop-pathBottom, 0.011, "the surface shows between the parts")
		require.InDelta(t, rhythmBase, rectTop+rectHeight, 0.011, "square at the baseline")
		// The path reads M left,bottom V A ... right-of-corner,top H ... A ... right,top V bottom Z.
		require.InDelta(t, rhythmTop, d[9], 0.011, "the busiest hour fills the plot")

		require.Contains(t, got, `<ul class="chart-key"><li class="series-1"><i class="chart-swatch" aria-hidden="true"></i>Claude Code / example-model</li><li class="series-2">`)
		require.Contains(t, got, `<span class="chart-cap" style="left:60.27%;top:14.29%">1h</span>`, "the busiest hour's figure rides its cap")
		require.Contains(t, got, `aria-label="Agent time by hour, 24 hours: 1h 40m in total; busiest 14:00-15:00 with 1h."`)
	})

	t.Run("hover: a readout per hour, on its column, every series listed", func(t *testing.T) {
		got := string(rhythmChart(two))
		hov := chartsMarkPayload(t, got)
		require.Len(t, hov.Marks, 24)
		require.Equal(t, 14, hov.Start, "the keyboard walk lands on the busiest hour")
		m := hov.Marks[14]
		require.Equal(t, "14:00-15:00", m.Label)
		require.Equal(t, "1h", m.Value)
		require.Equal(t, []markRow{
			{Tone: "series-1", Value: "20m", Name: "Claude Code / example-model"},
			{Tone: "series-2", Value: "40m", Name: "Codex / example-model"},
		}, m.Rows)
		require.Equal(t, "0m", hov.Marks[2].Value)
		require.Empty(t, hov.Marks[2].Rows)

		// The anchor is the drawn column's top center, from the same geometry.
		for h, mk := range hov.Marks {
			at := `<g class="chart-col" data-i="` + strconv.Itoa(h) + `"`
			i := strings.Index(got, at)
			if i < 0 {
				require.InDelta(t, rhythmBase, mk.At[1], 0.01, "a quiet hour points at the baseline")
				continue
			}
			col := got[i:]
			col = col[:strings.Index(col, "</g>")]
			d := chartsFloats(t, chartsAttr(t, chartsTags(col, `<path class="chart-seg`)[0], "d"))
			left, right, top := d[0], d[16], d[9]
			require.InDelta(t, (left+right)/2, mk.At[0], 0.011, "hour %d", h)
			require.InDelta(t, top, mk.At[1], 0.011, "hour %d", h)
		}
	})

	t.Run("a floor on the scale keeps a quiet day looking quiet", func(t *testing.T) {
		quiet := rhythmSpec{Unit: unitMinutes, Hours: day, MinScale: 60, Series: []rhythmSeries{{Name: "a", Slot: 1, Values: vals(map[int]int64{10: 15})}}}
		d := chartsFloats(t, chartsAttr(t, chartsTags(string(rhythmChart(quiet)), `<path class="chart-seg`)[0], "d"))
		require.InDelta(t, rhythmBase-(rhythmBase-rhythmTop)/4, d[9], 0.011, "15 of a 60-minute scale is a quarter of the plot")
	})

	t.Run("a 25-hour day draws 25 columns and names the repeated hour's zones", func(t *testing.T) {
		pdt, pst := time.FixedZone("PDT", -7*3600), time.FixedZone("PST", -8*3600)
		mid := time.Date(2026, 11, 1, 0, 0, 0, 0, pdt)
		hours := make([]time.Time, 25)
		for i := range hours {
			zone := pdt
			if i >= 2 {
				zone = pst
			}
			hours[i] = mid.Add(time.Duration(i) * time.Hour).In(zone)
		}
		v := make([]int64, 25)
		v[1], v[2], v[24] = 5, 7, 9
		r := rhythmSpec{Unit: unitMinutes, Hours: hours, Series: []rhythmSeries{{Name: "a", Slot: 1, Values: v}}}
		got := string(rhythmChart(r))
		require.Len(t, chartsTags(got, `<rect class="chart-hit"`), 25)
		hov := chartsMarkPayload(t, got)
		require.Equal(t, "00:00-01:00 PDT", hov.Marks[0].Label)
		require.Equal(t, "01:00 PDT-01:00 PST", hov.Marks[1].Label)
		require.Equal(t, "01:00 PST-02:00", hov.Marks[2].Label)
		require.Equal(t, "23:00-00:00", hov.Marks[24].Label)
		require.Equal(t, 4, strings.Count(got, `%">`)-strings.Count(got, `chart-cap`), "still four ticks: 00, 06, 12, 18")
		require.Contains(t, got, ", 25 hours:")
		require.Contains(t, string(rhythmTable(r)), `<th scope="row">01:00 PDT-01:00 PST</th>`)
		require.Equal(t, 25, strings.Count(string(rhythmTable(r)), `<tr><th scope="row">`)-1, "a row per hour, plus the total")

		spring, sdt := time.FixedZone("PST", -8*3600), time.FixedZone("PDT", -7*3600)
		smid := time.Date(2026, 3, 8, 0, 0, 0, 0, spring)
		short := make([]time.Time, 23)
		for i := range short {
			zone := spring
			if i >= 2 {
				zone = sdt
			}
			short[i] = smid.Add(time.Duration(i) * time.Hour).In(zone)
		}
		got = string(rhythmChart(rhythmSpec{Unit: unitMinutes, Hours: short, Series: []rhythmSeries{{Name: "a", Slot: 1, Values: []int64{0, 0, 4}}}}))
		require.Len(t, chartsTags(got, `<rect class="chart-hit"`), 23)
		require.Equal(t, "03:00-04:00", chartsMarkPayload(t, got).Marks[2].Label, "the skipped hour is simply not there")
	})

	t.Run("names are escaped in the key, the readout and the table", func(t *testing.T) {
		r := rhythmSpec{Label: chartsNasty, Hours: day, Series: []rhythmSeries{
			{Name: chartsNasty, Slot: 1, Values: vals(map[int]int64{1: 3})}, {Name: "b", Slot: 2, Values: vals(map[int]int64{1: 1})},
		}}
		for _, got := range []string{string(rhythmChart(r)), string(rhythmTable(r))} {
			require.NotContains(t, got, "<script")
		}
		require.Equal(t, chartsNasty, chartsMarkPayload(t, string(rhythmChart(r))).Marks[1].Rows[0].Name)
	})
}

func TestRankBars(t *testing.T) {
	t.Run("nothing to rank renders nothing", func(t *testing.T) {
		require.Empty(t, rankBars(rankSpec{}))
		require.Empty(t, rankBars(rankSpec{Rows: []rankRow{{Name: "a"}, {Name: "b", Value: -3}}}))
		require.Empty(t, rankTable(rankSpec{}))
	})

	r := rankSpec{Label: "Agent time by project", Unit: unitMinutes, Rows: []rankRow{
		{Name: "example-finance", Value: 124},
		{Name: "example-ops", Value: 190, Href: "/console/projects/example-ops", Peek: true},
		{Name: "example-legal", Value: 124, Href: "/console/projects/example-legal"},
		{Name: "global", Value: 0},
	}}

	t.Run("largest first, one bar each, the figure beside it", func(t *testing.T) {
		got := string(rankBars(r))
		names := regexp.MustCompile(`class="chart-rank-name"[^>]*>([^<]+)<`).FindAllStringSubmatch(got, -1)
		require.Len(t, names, 4)
		for i, want := range []string{"example-ops", "example-finance", "example-legal", "global"} {
			require.Equal(t, want, names[i][1], "largest first, ties by name")
		}
		require.Contains(t, got, `<span class="chart-rank-bar" style="width:100.00%"></span>`, "the largest spans the bar")
		require.Contains(t, got, `<span class="chart-rank-bar" style="width:65.26%"></span>`)
		require.Contains(t, got, `<span class="chart-rank-track" aria-hidden="true"></span><span class="chart-rank-fig">0m</span>`, "a row at zero draws no bar")
		require.Contains(t, got, `<span class="chart-rank-fig">3h 10m</span>`)
		require.Contains(t, got, `<ol class="chart-rank-list" aria-label="Agent time by project: example-ops 3h 10m, example-finance 2h 4m, example-legal 2h 4m, global 0m.">`)
		hov := chartsMarkPayload(t, got)
		require.Equal(t, markReadout{Label: "example-ops", Value: "3h 10m", Sub: "43% of 7h 18m"}, hov.Marks[0])
	})

	t.Run("a row with an href is a link, with data-peek when asked; without one it is text", func(t *testing.T) {
		got := string(rankBars(r))
		require.Contains(t, got, `<a class="chart-rank-name" href="/console/projects/example-ops" data-peek>example-ops</a>`)
		require.Contains(t, got, `<a class="chart-rank-name" href="/console/projects/example-legal">example-legal</a>`)
		require.Contains(t, got, `<span class="chart-rank-name">example-finance</span>`)
		require.Equal(t, 2, strings.Count(got, "<a "))
	})

	t.Run("a refused href renders text, and a quote cannot break out", func(t *testing.T) {
		for _, href := range []string{"javascript:alert(1)", "//example.com/x", `/\example.com`, "https://example.com/", "projects/x"} {
			got := string(rankBars(rankSpec{Rows: []rankRow{{Name: "example-ops", Value: 1, Href: href, Peek: true}}}))
			require.NotContains(t, got, "<a ", href)
			require.NotContains(t, got, "href=", href)
			require.NotContains(t, got, "data-peek", href)
			require.Contains(t, got, `<span class="chart-rank-name">example-ops</span>`, href)
		}
		got := string(rankBars(rankSpec{Rows: []rankRow{{Name: chartsNasty, Value: 1, Href: `/console/x"onmouseover="alert(1)`, Title: chartsNasty}}}))
		require.NotContains(t, got, "<script")
		require.NotContains(t, got, `onmouseover="`)
		require.Contains(t, got, `href="/console/x&#34;onmouseover=&#34;alert(1)"`)
		require.Contains(t, got, `title="&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`)
		require.NotContains(t, string(rankTable(rankSpec{Rows: []rankRow{{Name: chartsNasty, Value: 1}}})), "<script")
	})
}

func TestMixBar(t *testing.T) {
	require.Empty(t, mixBar(mixSpec{}))
	require.Empty(t, mixBar(mixSpec{Parts: []mixPart{{Name: "a"}, {Name: "b", Value: -1}}}))
	require.Empty(t, mixTable(mixSpec{}))

	m := mixSpec{Label: "Token mix", Unit: unitTokens, Parts: []mixPart{
		{Name: "Output", Value: 1_200_000}, {Name: "Fresh input", Value: 3_400_000},
		{Name: "Cache write", Value: 0}, {Name: "Cache read", Value: 135_000_000},
	}}
	got := string(mixBar(m))
	bar := got[strings.Index(got, `<div class="chart-mix-bar`):]
	bar = bar[strings.Index(bar, ">")+1 : strings.Index(bar, "</div>")]
	require.Equal(t, 3, strings.Count(bar, `<i class="chart-mix-seg`), "a part at zero draws nothing")
	require.Regexp(t, `^(<i class="chart-mix-seg mix-\d" data-i="\d" style="flex-grow:[\d.]+"></i>)+$`, bar, "no text on the bar")
	for i, want := range []string{"mix-1", "mix-2", "mix-4"} {
		require.Contains(t, chartsTags(bar, `<i class="chart-mix-seg`)[i], want, "the ramp follows the parts' order")
	}
	require.Equal(t, 4, strings.Count(got, `<li class="chart-legend-row`), "every part keeps its figure beside the bar")
	require.Contains(t, got, `<li class="chart-legend-row mix-3" data-lit="2"><i class="chart-swatch" aria-hidden="true"></i><span class="chart-legend-name">Cache write</span><span class="chart-legend-share">0%</span><span class="chart-legend-fig">0</span></li>`)
	require.Contains(t, got, `<span class="chart-legend-share">97%</span><span class="chart-legend-fig">135M</span>`)
	require.Contains(t, got, `aria-label="Token mix: 139.6M in total. Output 1%, Fresh input 2%, Cache write 0%, Cache read 97%."`)
	require.Equal(t, 3, chartsMarkPayload(t, got).Start, "the keyboard walk lands on the largest part")
	require.Contains(t, string(mixBar(mixSpec{Parts: []mixPart{{"a", 1}, {"b", 1}, {"c", 1}, {"d", 1}, {"e", 1}}})), `chart-mix-seg series-other" data-i="4"`,
		"past the ramp's four steps a part wears the neutral, never a series color")
	require.Contains(t, string(mixTable(m)), `<tfoot><tr><th scope="row">Total</th><td class="num">139.6M</td><td class="num">100%</td></tr></tfoot>`)
}

func TestMonthGrid(t *testing.T) {
	zone := time.FixedZone("PST", -8*3600)
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, zone) // a Sunday
	days := make([]monthDay, 30)
	for i := range days {
		days[i] = monthDay{Href: "/console/diary?day=2026-11-" + strconv.Itoa(100 + i + 1)[1:]}
	}
	days[0].Value, days[2].Value, days[3].Value, days[5].Value = 120, 30, 61, 91
	days[1].Href = ""
	days[25].Value = 999 // after today: still to come, so it shades nothing
	m := monthSpec{Label: "Agent time per day", Unit: unitMinutes, Start: nov, Days: days, Today: "2026-11-20", Selected: "2026-11-04"}
	got := string(monthGrid(m))

	t.Run("nothing renders nothing", func(t *testing.T) {
		require.Empty(t, monthGrid(monthSpec{Start: nov}))
		require.Empty(t, monthTable(monthSpec{Start: nov}))
	})

	t.Run("a cell per day carrying its date, Sunday first", func(t *testing.T) {
		require.Equal(t, 30, strings.Count(got, `class="chart-month-cell`))
		require.Equal(t, 30, strings.Count(got, `data-day="2026-11-`))
		require.Contains(t, got, `data-day="2026-11-01" data-i="0" aria-label="Sun, Nov 01: 2h" style="grid-column-start:1">1</a>`)
		dec := string(monthGrid(monthSpec{Start: time.Date(2026, 12, 1, 0, 0, 0, 0, zone), Days: make([]monthDay, 31)}))
		require.Contains(t, dec, `style="grid-column-start:3"`, "December 2026 opens on a Tuesday")
		require.Equal(t, 1, strings.Count(dec, "grid-column-start"), "only the first cell is placed")
		require.Contains(t, got, `<span>Su</span><span>Mo</span>`)
	})

	t.Run("one hue in five steps", func(t *testing.T) {
		for day, lvl := range map[string]string{"01": "l4", "02": "l0", "03": "l1", "04": "l3", "06": "l4", "26": "l0"} {
			cell := regexp.MustCompile(`class="([^"]*)" (?:href="[^"]*" )?data-day="2026-11-` + day + `"`).FindStringSubmatch(got)
			require.NotNil(t, cell, day)
			require.Contains(t, strings.Fields(cell[1]), lvl, "Nov %s", day)
		}
	})

	t.Run("a day with an href is a real link; without one it is not interactive", func(t *testing.T) {
		require.Contains(t, got, `<a class="chart-month-cell l4" href="/console/diary?day=2026-11-01" data-day="2026-11-01"`)
		require.Contains(t, got, `<span class="chart-month-cell l0" data-day="2026-11-02" data-i="1" role="img" aria-label="Mon, Nov 02: 0m">2</span>`)
		require.Equal(t, 19, strings.Count(got, "<a "), "the days up to today that link, not the 2nd")
	})

	t.Run("today and the selected day are outlined, once each", func(t *testing.T) {
		require.Equal(t, 1, strings.Count(got, " today"))
		require.Contains(t, got, `<a class="chart-month-cell l0 today" href="/console/diary?day=2026-11-20"`)
		require.Equal(t, 1, strings.Count(got, " sel"))
		require.Contains(t, got, `<a class="chart-month-cell l3 sel" href="/console/diary?day=2026-11-04"`)
	})

	t.Run("a day still to come is quiet and never a link", func(t *testing.T) {
		require.Equal(t, 10, strings.Count(got, " future"))
		require.Contains(t, got, `<span class="chart-month-cell l0 future" data-day="2026-11-26" role="img" aria-label="Thu, Nov 26">26</span>`)
		require.NotContains(t, got, "day=2026-11-21", "no link past today, though one was passed")
		hov := chartsMarkPayload(t, got)
		require.Len(t, hov.Marks, 30)
		require.Empty(t, hov.Marks[25].Value)
	})

	t.Run("the summary and the table", func(t *testing.T) {
		require.Contains(t, got, `role="group" aria-label="Agent time per day, November 2026: 4 active days, 5h 2m in total; busiest Sun, Nov 01 with 2h."`)
		tbl := string(monthTable(m))
		require.Equal(t, 20, strings.Count(tbl, `<tr><th scope="row">`)-1, "a row per day up to today, plus the total")
		require.Contains(t, tbl, `<caption>Agent time per day, November 2026</caption>`)
	})

	t.Run("hrefs and notes are escaped, and the Sessions calendar's classes stay its own", func(t *testing.T) {
		odd := []monthDay{{Value: 1, Href: "javascript:alert(1)", Note: chartsNasty}, {Value: 1, Href: "//example.com"}}
		g := string(monthGrid(monthSpec{Unit: unitMinutes, Start: nov, Days: odd}))
		require.NotContains(t, g, "<a ")
		require.NotContains(t, g, "javascript:")
		require.NotContains(t, g, "<script")
		require.Contains(t, g, `aria-label="Sun, Nov 01: 1m, &#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`)
		require.NotContains(t, got, "mom-cal", "the calendar's click handler must never see these cells")
	})
}

func TestPresenceStrip(t *testing.T) {
	zone := time.FixedZone("PST", -8*3600)
	mid := time.Date(2026, 11, 20, 0, 0, 0, 0, zone)
	day := chartsHours(mid, 24)
	at := func(h, m int) time.Time { return mid.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

	require.Empty(t, presenceStrip(presenceSpec{}))
	require.Empty(t, presenceStrip(presenceSpec{Hours: day}))
	require.Empty(t, presenceStrip(presenceSpec{Hours: day, Spans: []presenceSpan{{at(30, 0), at(31, 0)}, {at(5, 0), at(5, 0)}}}), "off the axis or empty: nothing lit")

	got := string(presenceStrip(presenceSpec{Label: "When it was worked", Hours: day, Spans: []presenceSpan{
		{at(9, 30), at(11, 0)}, {at(6, 0), at(12, 0)}, {at(12, 0), at(12, 15)}, {at(23, 30), at(25, 0)},
	}}))
	require.Equal(t, 2, strings.Count(got, `<i class="chart-presence-seg"`), "overlapping and touching stretches merge")
	require.Contains(t, got, `<i class="chart-presence-seg" data-i="0" style="left:25.000%;width:26.042%"></i>`)
	require.Contains(t, got, `style="left:97.917%;width:2.083%"`, "clipped to the day's end")
	for _, left := range []string{"25.000", "50.000", "75.000"} {
		require.Contains(t, got, `<i class="chart-presence-notch" style="left:`+left+`%"></i>`)
	}
	require.Equal(t, 3, strings.Count(got, "chart-presence-notch"))
	require.Contains(t, got, `aria-label="When it was worked: 06:00-12:15, 23:30-00:00; 6h 45m in all." tabindex="0"`)
	hov := chartsMarkPayload(t, got)
	require.Equal(t, markReadout{Label: "06:00-12:15", Value: "6h 15m"}, hov.Marks[0])

	// A 25-hour day keeps the rhythm's axis: its last hour ends at 100%.
	long := chartsHours(mid, 25)
	got = string(presenceStrip(presenceSpec{Hours: long, Spans: []presenceSpan{{long[24], long[24].Add(time.Hour)}}}))
	require.Contains(t, got, `style="left:96.000%;width:4.000%"`)

	tbl := string(presenceTable(presenceSpec{Hours: day, Spans: []presenceSpan{{at(6, 0), at(7, 5)}}}))
	require.Contains(t, tbl, `<tr><th scope="row">06:00</th><td class="num">07:05</td><td class="num">1h 5m</td></tr>`)
}

func TestStatTile(t *testing.T) {
	require.Empty(t, statTile(tileSpec{Value: "9"}), "a tile without a label renders nothing")

	got := string(statTile(tileSpec{Label: "Agent time", Value: "6h 40m", Icon: "clock"}))
	require.True(t, strings.HasPrefix(got, `<div class="ov2-vital chart-tile">`), "the Overview vital's markup, as a plain tile")
	require.Contains(t, got, `<span class="ov2-vital-label"><svg class="ico ico-clock"`)
	require.Contains(t, got, `<div class="ov2-vital-num"><strong>6h 40m</strong></div>`, "an absent comparison renders nothing")
	require.NotContains(t, got, "ico-arrow-down")
	require.NotContains(t, got, "ov2-vital-foot", "no trend, no foot")

	got = string(statTile(tileSpec{Label: "Tasks closed", Value: "9", Anchor: "#tasks",
		Delta: volumeDelta(12, 10, true, noJudgment),
		Spark: spark{Points: []store.TrendBucket{{Count: 1}, {Count: 3}, {Count: 2}}, Label: "Tasks closed over 14 days"}}))
	require.True(t, strings.HasPrefix(got, `<a class="ov2-vital chart-tile" href="#tasks">`))
	require.True(t, strings.HasSuffix(got, `</a>`))
	require.Contains(t, got, `<svg class="ico ico-arrow-down"`, "the link's cue points down the page")
	require.Contains(t, got, `<span class="delta" data-good="neutral">`, "a volume carries no verdict")
	require.Contains(t, got, `<div class="ov2-vital-foot"><svg class="ov2-spark"`)

	for _, a := range []string{"/console/tasks", "javascript:alert(1)", "#bad id", "#"} {
		require.True(t, strings.HasPrefix(string(statTile(tileSpec{Label: "x", Anchor: a})), `<div `), "%q is no in-page anchor", a)
	}
	require.Contains(t, string(statTile(tileSpec{Label: "Kept"})), "<strong>n/a</strong>")
	got = string(statTile(tileSpec{Label: chartsNasty, Value: chartsNasty, Sub: chartsNasty}))
	require.NotContains(t, got, "<script")
	require.Equal(t, 3, strings.Count(got, "&lt;script&gt;"))
}

func TestDataTable(t *testing.T) {
	require.Empty(t, dataTable{Caption: "x"}.html(), "no rows, no table")
	got := string(dataTable{Caption: "Mix", Head: []string{"Part", "Value"}, Rows: [][]string{{"a<b", "1"}}, Foot: []string{"Total", "1"}}.html())
	require.Equal(t, `<table class="chart-table"><caption>Mix</caption><thead><tr><th scope="col">Part</th><th scope="col" class="num">Value</th></tr></thead>`+
		`<tbody><tr><th scope="row">a&lt;b</th><td class="num">1</td></tr></tbody><tfoot><tr><th scope="row">Total</th><td class="num">1</td></tr></tfoot></table>`, got)
}

// The series colors are tokens, declared in every theme block: the light
// root, the explicit dark theme, and the OS-dark block that must equal it.
// Dark is its own validated step set, never the light one inherited, and no
// series wears a status color.
func TestSeriesTokens_EveryThemeCarriesThem(t *testing.T) {
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(consoleCSS), "")
	block := func(opener string) map[string]string {
		at := strings.Index(css, opener)
		require.NotEqual(t, -1, at, "missing %q", opener)
		body := css[at+len(opener):]
		body = body[:strings.Index(body, "}")]
		out := map[string]string{}
		for _, m := range regexp.MustCompile(`(--[\w-]+)\s*:\s*([^;]+);`).FindAllStringSubmatch(body, -1) {
			out[m[1]] = strings.TrimSpace(m[2])
		}
		return out
	}
	light, dark, osDark := block(":root {"), block(`[data-theme="dark"] {`), block(`:root:not([data-theme="light"]) {`)
	for _, slot := range []string{"1", "2", "3", "4", "5", "other"} {
		name := "--series-" + slot
		require.Regexp(t, `^#[0-9a-f]{6}$`, light[name], name)
		require.Regexp(t, `^#[0-9a-f]{6}$`, dark[name], name)
		require.Equal(t, dark[name], osDark[name], "%s: System-dark is the Dark theme", name)
		require.NotEqual(t, light[name], dark[name], "%s: the dark step is chosen for the dark surface", name)
		for _, status := range []string{"--ok", "--warn", "--danger", "--live"} {
			require.NotEqual(t, light[status], light[name], "%s must not be %s", name, status)
			require.NotEqual(t, dark[status], dark[name], "%s must not be %s", name, status)
		}
		require.Contains(t, css, ".series-"+slot+" { --chart-c: var("+name+"); }")
	}
}

// The ring and the columns draw on once and hold still under reduced motion;
// hover motion is opacity alone, so it stays on the compositor. The entry
// motion is transform and opacity only too: the ring once swept its
// stroke-dasharray, which repaints every frame of the sweep.
func TestChartMarks_MotionIsEntryOnly(t *testing.T) {
	css := string(consoleCSS)
	require.Contains(t, css, "animation: chart-ring-in var(--t, 1ms) var(--ease-out) var(--d, 0ms) backwards;")
	require.Contains(t, css, ".chart-ring-segs { animation: chart-ring-turn .72s var(--ease-out) backwards; }")
	require.Contains(t, css, "animation: chart-rise .55s var(--ease-out) var(--d, 0ms) backwards;")
	require.Contains(t, css, ".chart-ring-seg, .chart-ring-segs, .chart-col, .chart-cap { animation: none; }")
	require.Contains(t, string(ringChart(ringSpec{Unit: unitTokens, Slices: []ringSlice{{Name: "a", Slot: 1, Value: 3}, {Name: "b", Slot: 2, Value: 1}}})),
		fmt.Sprintf(`<g class="chart-ring-segs" style="transform-origin:%gpx %gpx">`, ringBox/2, ringBox/2), "the ring turns about its center")
	names := regexp.MustCompile(`@keyframes (chart-[\w-]+)`).FindAllStringSubmatch(css, -1)
	require.NotEmpty(t, names)
	for _, m := range names {
		for _, p := range regexp.MustCompile(`([\w-]+)\s*:`).FindAllStringSubmatch(keyframesBody(t, m[1]), -1) {
			require.Contains(t, []string{"transform", "opacity"}, p[1], "@keyframes %s animates %s: use transform or opacity", m[1], p[1])
		}
	}
	for _, r := range cssRules(t) {
		if !strings.Contains(r.selector, "chart-") {
			continue
		}
		if m := regexp.MustCompile(`transition:\s*([\w-]+)`).FindStringSubmatch(r.body); m != nil {
			require.Contains(t, []string{"opacity", "transform", "none"}, m[1], "%s transitions %s", r.selector, m[1])
		}
	}
	for _, name := range []string{"chart-rise"} {
		for _, m := range regexp.MustCompile(`([\w-]+)\s*:`).FindAllStringSubmatch(keyframesBody(t, name), -1) {
			require.Equal(t, "transform", m[1], "@keyframes %s", name)
		}
	}
}

// The client inserts every label with textContent: names come from agents and
// config, so none may reach innerHTML.
func TestChartsJS_MarkLayer(t *testing.T) {
	js := string(chartsJS)
	require.Contains(t, js, "t.closest('.chart[data-hover]')")
	require.Contains(t, js, "'[data-i], [data-lit]'")
	require.Contains(t, js, "classList.contains('chart-walk')")
	require.Contains(t, js, "document.addEventListener('seam:content-updated', restAll)", "the roving tabindex survives the live morph")
	require.Contains(t, js, "e.textContent = text")
	require.NotContains(t, js, "innerHTML")
}
