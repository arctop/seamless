package console

import (
	"encoding/json"
	"fmt"
	"html"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// The .area-line draw-on animation rides on pathLength="1": the CSS sets
// stroke-dasharray:1 so one dash spans the whole path, then animates the offset
// to 0. vector-effect="non-scaling-stroke" moves the dash pattern into screen
// space, which defeats that normalization -- the dash then covers only 1/scale of
// the path, so the line's tail is silently clipped at every width past the
// 560-unit viewBox and the fill runs on without it. At the design width scale is
// 1.0 and the bug is invisible, which is exactly why it needs pinning here.
//
// The gridlines and the peak marker keep non-scaling-stroke: they carry no
// pathLength, so a screen-space dash is what they actually want.
func TestAreaChart_LineHasNoNonScalingStroke(t *testing.T) {
	got := string(areaChart([]store.TrendBucket{
		{Label: "Jul 10", Count: 1},
		{Label: "Jul 13", Count: 9},
		{Label: "Jul 15", Count: 4},
	}))

	line := svgTag(t, got, `<path class="area-line"`)
	require.Contains(t, line, `pathLength="1"`, "the draw-on animation needs the normalization")
	require.NotContains(t, line, "non-scaling-stroke",
		"non-scaling-stroke + pathLength clips the line's tail as the card widens")

	// The peak marker's leader line is the control: it wants the screen-space stroke.
	require.Contains(t, svgTag(t, got, `<g class="area-peak"><line`), "non-scaling-stroke")
}

// The hover readout is only honest if the crosshair lands on the line the eye is
// following: every point charts.js snaps to must be a coordinate the path was
// actually drawn through. Both come from the same x()/y() closures, so this pins
// them staying that way -- a plot tweak that forgets the payload (or vice versa)
// silently offsets the tooltip from the curve.
func TestAreaChart_HoverPointsTrackTheDrawnLine(t *testing.T) {
	points := []store.TrendBucket{
		{Label: "Jul 10", Count: 1},
		{Label: "Jul 13", Count: 9},
		{Label: "Jul 15", Count: 4},
	}
	got := string(areaChart(points))

	hov := hoverPayload(t, got)
	require.Len(t, hov.Pts, len(points))
	line := svgTag(t, got, `<path class="area-line"`)
	for i, p := range hov.Pts {
		require.Equal(t, points[i].Label, p.Label)
		require.Contains(t, line, fmt.Sprintf("%.1f %.1f", p.X, p.Y),
			"hover point %d is not on the drawn line", i)
	}
	// The value text is the tooltip's headline -- the client only positions it.
	require.Equal(t, "1 injection", hov.Pts[0].Value)
	require.Equal(t, "9 injections", hov.Pts[1].Value)
	// The crosshair spans the plot, top gridline to baseline.
	require.Less(t, hov.Top, hov.Bot)
	require.Contains(t, svgTag(t, got, `<path class="area-fill"`), fmt.Sprintf("%.1f", hov.Bot))
}

// A bucket with no sessions is plotted at the floor (a quiet stretch reads as a
// dip, not a ceiling) -- but 0% coverage and "nothing ran" are different facts,
// and the readout is where the difference gets said out loud.
func TestCoverageTrend_HoverDistinguishesQuietFromUncovered(t *testing.T) {
	hov := hoverPayload(t, string(coverageTrend([]store.CoverageBucket{
		{Label: "Jul 13", Total: 5, Covered: 4},
		{Label: "Jul 14", Total: 0, Covered: 0},
		{Label: "Jul 15", Total: 1, Covered: 0},
	})))
	require.Len(t, hov.Pts, 3)

	require.Equal(t, "80% covered", hov.Pts[0].Value)
	require.Equal(t, "4 of 5 sessions retained knowledge", hov.Pts[0].Sub)

	require.Equal(t, "No sessions", hov.Pts[1].Value)
	require.Empty(t, hov.Pts[1].Sub, "a quiet bucket has no coverage to report")

	require.Equal(t, "0% covered", hov.Pts[2].Value)
	require.Equal(t, "0 of 1 session retained knowledge", hov.Pts[2].Sub)
}

// The payload is hand-written into an attribute, so its escaping is the chart's
// job: a label carrying a quote must not be able to close data-hover early.
func TestHoverAttr_EscapesIntoTheAttribute(t *testing.T) {
	const nasty = `"><script>`
	got := hoverAttr(560, 150, 14, 142, []hoverPoint{{X: 1, Y: 2, Label: nasty, Value: "1 injection"}})

	body := strings.TrimSuffix(strings.TrimPrefix(got, ` data-hover="`), `"`)
	for _, c := range []string{`"`, "<", ">"} {
		require.NotContains(t, body, c, "a label must not be able to break out of the attribute")
	}
	// Escaped, not mangled: it still round-trips to what the tooltip should say.
	require.Equal(t, nasty, hoverPayload(t, `<div class="area"`+got+`></div>`).Pts[0].Label)

	require.Empty(t, hoverAttr(560, 150, 14, 142, nil), "no points -- no readout to arm")
}

// hoverPayload pulls a chart's data-hover attribute back out of its markup and
// decodes it, so a test can assert on the readout the client will show. The
// attribute is HTML-escaped, so the first raw quote after it is its terminator.
func hoverPayload(t *testing.T, markup string) hoverData {
	t.Helper()
	const key = ` data-hover="`
	i := strings.Index(markup, key)
	require.GreaterOrEqual(t, i, 0, "chart is missing the hover payload")
	rest := markup[i+len(key):]
	end := strings.Index(rest, `"`)
	require.GreaterOrEqual(t, end, 0, "unterminated data-hover attribute")

	var out hoverData
	require.NoError(t, json.Unmarshal([]byte(html.UnescapeString(rest[:end])), &out))
	return out
}

// svgTag returns prefix plus the rest of the tag it ends inside, so a test can
// assert on one element's attributes without matching the whole chart. The scan
// for the tag end starts after the prefix, so a prefix may span into a nested
// element (`<g class="x"><line`) to reach the tag that carries the attributes.
func svgTag(t *testing.T, markup, prefix string) string {
	t.Helper()
	i := strings.Index(markup, prefix)
	require.GreaterOrEqual(t, i, 0, "chart is missing %s", prefix)
	rest := markup[i:]
	end := strings.Index(rest[len(prefix):], ">")
	require.GreaterOrEqual(t, end, 0, "unterminated tag at %s", prefix)
	return rest[:len(prefix)+end+1]
}

// The capture calendar encodes two things per day cell: session volume as
// intensity and covered-ness as a distinct dot. A day with no sessions is an
// empty resting cell, never a warning tone (that is CSS, but the class
// contract starts here).
func TestCalendarGrid(t *testing.T) {
	// A Sunday start, one full week: quiet, busy, covered, quiet...
	start := time.Date(2026, 8, 2, 0, 0, 0, 0, time.Local) // a Sunday
	days := []store.CoverageBucket{
		{Label: "Aug 02"},                       // no sessions
		{Label: "Aug 03", Total: 1},             // active, nothing captured
		{Label: "Aug 04", Total: 4, Covered: 2}, // busy, captured
		{Label: "Aug 05"},
		{Label: "Aug 06", Total: 2, Covered: 1},
		{Label: "Aug 07"},
		{Label: "Aug 08"},
	}
	svg := string(calendarGrid(days, start, ""))

	require.Contains(t, svg, `class="mom-cal-grid"`)
	require.Contains(t, svg, `role="img"`)
	require.Contains(t, svg, `tabindex="0"`, "the grid is a keyboard tab stop for the charts.js readout")
	require.Equal(t, 7, strings.Count(svg, "mom-cal-cell"), "one cell per day")
	require.Equal(t, 2, strings.Count(svg, "mom-cal-dot"), "one dot per covered day, none elsewhere")
	require.Contains(t, svg, `class="mom-cal-cell l0"`, "a quiet day rests at level zero")
	require.Contains(t, svg, `class="mom-cal-cell l4"`, "the busiest day carries full intensity")
	require.Contains(t, svg, "Tue, Aug 04: 4 sessions, 2 captured")
	require.Contains(t, svg, "Sun, Aug 02: no sessions")
	require.Equal(t, 7, strings.Count(svg, "data-day="), "every cell carries its date for the day drill-down")
	require.Contains(t, svg, `data-day="2026-08-04"`)
	require.NotContains(t, svg, " sel", "no cell is outlined while no day is focused")
	require.Contains(t, svg, `>Aug</text>`, "the month labels its first column")
	require.Contains(t, svg, ">Mon</text>")
	require.Equal(t, 1, strings.Count(svg, "mom-cal-col"), "one wave group per week column")
	require.Contains(t, svg, `class="mom-cal-col" style="--d:0ms"`, "the entry-wave delay staggers by column")
	require.Equal(t, 1, strings.Count(svg, " today"), "exactly one breathing cell")
	require.Contains(t, svg, `class="mom-cal-cell l0 today"`, "the final bucket is today's cell")

	// The ?day drill-down's focused cell wears the sel outline -- exactly one.
	svg = string(calendarGrid(days, start, "2026-08-04"))
	require.Equal(t, 1, strings.Count(svg, " sel"), "exactly one focused cell")
	require.Contains(t, svg, `class="mom-cal-cell l4 sel"`, "the focused day keeps its intensity tone")

	// Two weeks: the second column opens its own wave group with a later delay.
	two := append(slices.Clone(days), days...)
	svg = string(calendarGrid(two, start, ""))
	require.Equal(t, 2, strings.Count(svg, "mom-cal-col"))
	require.Contains(t, svg, `style="--d:12ms"`)

	require.Empty(t, calendarGrid(nil, start, ""), "no days renders nothing")
}
