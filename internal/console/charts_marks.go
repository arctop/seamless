package console

import (
	"cmp"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file holds the per-mark chart primitives: the series ring, the day
// rhythm, ranked bars, the thin segmented bar, the month heat grid, the
// presence strip, the stat tile, and the table each chart can also be read as.
// Like the rest of charts.go they are pure functions over plain inputs that
// return template.HTML: nothing here reads a store or checks a feature, and
// every free-form string (a name, a note, an href) is escaped here, never
// trusted from the caller.
//
// Color does one job per chart. A series (an agent) wears an explicit slot,
// --series-1..5, so it keeps its color on every day the caller draws it; a
// series with no slot folds into Other's neutral. The single-hue charts never
// wear a series color, so nothing beside the ring can be read as an agent:
// the month grid and the presence strip are brand light (magnitude, as on the
// Sessions calendar), and the ranked bars and the segmented bar are ink.
//
// Every mark answers through charts.js's data-hover layer: the chart's root
// carries one readout per mark as JSON (markAttr), every element of a mark
// names its readout with data-i (and a legend row that only lights its mark,
// with data-lit), and each mark's hit area is larger than the mark.

// ---------------------------------------------------------------------------
// Units, shares and series slots
// ---------------------------------------------------------------------------

// chartUnit says how a chart prints its values.
type chartUnit int

const (
	// unitCount prints a count: in full under 10,000 ("1,284"), compact above ("12.4k").
	unitCount chartUnit = iota
	// unitTokens prints model tokens as the rest of the console does (compactNum): "148M", "12.4k", "980".
	unitTokens
	// unitMinutes prints agent time from whole minutes: "45m", "2h 10m", "6h".
	unitMinutes
)

// figure formats a value in the unit.
func (u chartUnit) figure(v int64) string {
	switch u {
	case unitTokens:
		return compactNum(int(v))
	case unitMinutes:
		return chartMinutes(v)
	default:
		if v < 10000 {
			return groupDigits(int(v))
		}
		return compactNum(int(v))
	}
}

// chartMinutes prints whole minutes as agent time: "45m", "2h 10m", "6h".
func chartMinutes(m int64) string {
	if m < 60 {
		return strconv.FormatInt(m, 10) + "m"
	}
	h, r := m/60, m%60
	if r == 0 {
		return strconv.FormatInt(h, 10) + "h"
	}
	return strconv.FormatInt(h, 10) + "h " + strconv.FormatInt(r, 10) + "m"
}

// chartShare prints part/total as a whole percentage: "61%", "<1%" for a part
// too small to round up to one, "" when there is no total to share.
func chartShare(part, total int64) string {
	if total <= 0 {
		return ""
	}
	if part <= 0 {
		return "0%"
	}
	p := float64(part) / float64(total) * 100
	if p < 0.5 {
		return "<1%"
	}
	return strconv.Itoa(int(math.Round(p))) + "%"
}

// seriesSlots is how many categorical colors there are: --series-1..5.
const seriesSlots = 5

// seriesClass is the CSS class a slot wears: series-1..5, or series-other.
// The class sets --chart-c, which every series mark, swatch and key reads.
func seriesClass(slot int) string {
	if slot >= 1 && slot <= seriesSlots {
		return "series-" + strconv.Itoa(slot)
	}
	return "series-other"
}

// assignSlots picks the color slots once for a whole span -- the Diary picks
// them per month, so an agent wears one color on every day of it. The five
// largest positive totals take slots 1..5, largest first, ties by key; every
// other key gets 0, which every chart folds into Other.
func assignSlots(totals map[string]int64) map[string]int {
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(totals[b], totals[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	out := make(map[string]int, len(keys))
	slot := 0
	for _, k := range keys {
		if totals[k] > 0 && slot < seriesSlots {
			slot++
			out[k] = slot
			continue
		}
		out[k] = 0
	}
	return out
}

// foldItem is one named series on its way into a chart.
type foldItem struct {
	name   string
	slot   int
	weight int64
	index  int // its position in the caller's slice
}

// foldSlots keeps at most one series per slot 1..5 -- the heaviest, ties by
// name -- and returns them in slot order, with everything else (slot 0, a slot
// out of range, the lighter of two sharing one) heaviest first for the chart
// to draw as one Other. A series with no weight is absent and is dropped.
func foldSlots(items []foldItem) (kept, other []foldItem) {
	var bySlot [seriesSlots + 1]*foldItem
	for i := range items {
		it := items[i]
		if it.weight <= 0 {
			continue
		}
		if it.slot < 1 || it.slot > seriesSlots {
			other = append(other, it)
			continue
		}
		cur := bySlot[it.slot]
		switch {
		case cur == nil:
			bySlot[it.slot] = &items[i]
		case it.weight > cur.weight || (it.weight == cur.weight && it.name < cur.name):
			other = append(other, *cur)
			bySlot[it.slot] = &items[i]
		default:
			other = append(other, it)
		}
	}
	for _, it := range bySlot[1:] {
		if it != nil {
			kept = append(kept, *it)
		}
	}
	slices.SortFunc(other, func(a, b foldItem) int {
		if c := cmp.Compare(b.weight, a.weight); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	return kept, other
}

// otherName names the folded tail: a lone series keeps its own name (it is
// still drawn in Other's neutral), several read "Other (3)".
func otherName(other []foldItem) string {
	if len(other) == 1 {
		return other[0].name
	}
	return "Other (" + strconv.Itoa(len(other)) + ")"
}

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

// safeHref returns href escaped for an attribute when it is a same-origin path
// ("/console/...", never "//host" or "/\host") or an in-page "#anchor", made
// of printable ASCII with no space or backslash anywhere. Anything else -- a
// "javascript:" or "https://" URL, a relative path, a control character a
// browser would strip before reading the rest -- is refused (ok false), and
// the caller renders text instead of a link.
func safeHref(href string) (string, bool) {
	if len(href) < 2 {
		return "", false
	}
	for i := 0; i < len(href); i++ {
		if c := href[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			return "", false
		}
	}
	switch {
	case href[0] == '#':
	case href[0] == '/' && href[1] != '/':
	default:
		return "", false
	}
	return template.HTMLEscapeString(href), true
}

// safeAnchor accepts only an in-page "#id" made of id characters, for the
// stat tile's jump to the section that lists its items.
func safeAnchor(a string) (string, bool) {
	if len(a) < 2 || a[0] != '#' {
		return "", false
	}
	for i := 1; i < len(a); i++ {
		c := a[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':' || c == '.') {
			return "", false
		}
	}
	return a, true
}

// ---------------------------------------------------------------------------
// The per-mark readout payload static/charts.js reads
// ---------------------------------------------------------------------------

// markRow is one series line inside a readout: its color key (a series or
// ramp class), its value, its name.
type markRow struct {
	Tone  string `json:"tone"`
	Value string `json:"value"`
	Name  string `json:"name"`
}

// markReadout is what one mark says on hover or keyboard focus. Every string
// is formatted here; the client only places it, with textContent. At is the
// point the tooltip sits above, in the chart svg's viewBox units; the HTML
// charts leave it nil and the client places the tooltip over the mark's box.
type markReadout struct {
	Label string      `json:"label"`
	Value string      `json:"value"`
	Sub   string      `json:"sub,omitempty"`
	Rows  []markRow   `json:"rows,omitempty"`
	At    *[2]float64 `json:"at,omitempty"`
}

// markData is a chart's whole readout payload: the viewBox the At points are
// in (zero on the HTML charts), the mark a keyboard walk lands on first, and
// one readout per mark, indexed by the marks' data-i.
type markData struct {
	W     float64       `json:"w,omitempty"`
	H     float64       `json:"h,omitempty"`
	Start int           `json:"start"`
	Marks []markReadout `json:"marks"`
}

// markAttr renders the data-hover attribute that arms a chart's per-mark
// readouts. Like hoverAttr it is progressive enhancement: the chart reads
// whole without it (aria-label, legend, figures, table view), so a payload
// that cannot be marshaled drops the readout rather than the chart.
func markAttr(d markData) string {
	if len(d.Marks) == 0 {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	// json.Marshal escapes <, > and &; HTMLEscapeString closes the quote.
	return ` data-hover="` + template.HTMLEscapeString(string(b)) + `"`
}

func markAt(x, y float64) *[2]float64 {
	p := [2]float64{math.Round(x*100) / 100, math.Round(y*100) / 100}
	return &p
}

// legendRow is one row of a ring or segmented-bar legend: the swatch carrying
// the identity, then the name, share and figure in ink. lit (>= 0) is the mark
// the row lights on hover; the row is the readout itself, so it shows none.
func legendRow(lit int, tone, name, share, fig string) string {
	var b strings.Builder
	b.WriteString(`<li class="chart-legend-row ` + tone + `"`)
	if lit >= 0 {
		b.WriteString(` data-lit="` + strconv.Itoa(lit) + `"`)
	}
	b.WriteString(`><i class="chart-swatch" aria-hidden="true"></i><span class="chart-legend-name">` + template.HTMLEscapeString(name) + `</span>`)
	if share != "" {
		b.WriteString(`<span class="chart-legend-share">` + template.HTMLEscapeString(share) + `</span>`)
	}
	b.WriteString(`<span class="chart-legend-fig">` + template.HTMLEscapeString(fig) + `</span></li>`)
	return b.String()
}

// ---------------------------------------------------------------------------
// Ring -- one whole split by series, the total in its center
// ---------------------------------------------------------------------------

// ringSlice is one series' part of the ring's whole.
type ringSlice struct {
	Name  string // shown as text
	Slot  int    // 1..5 wears --series-N; anything else folds into Other
	Value int64  // zero or below: absent, draws nothing
}

// ringSpec is a whole split by series: the day's model tokens by agent.
type ringSpec struct {
	Label  string    // what the whole is ("Model tokens by agent"): leads the summary, captions the table
	Unit   chartUnit // how the values print
	Core   string    // optional word under the center total ("tokens")
	Slices []ringSlice
}

// ringPart is one drawn segment: a kept series, or the folded Other.
type ringPart struct {
	name    string
	tone    string
	value   int64
	members []foldItem // Other's series, heaviest first
}

// parts folds the slices into at most five slotted segments plus Other, in
// slot order, so neighbors are the validated neighbors of the palette.
func (r ringSpec) parts() (parts []ringPart, total int64) {
	items := make([]foldItem, 0, len(r.Slices))
	for i, s := range r.Slices {
		items = append(items, foldItem{name: s.Name, slot: s.Slot, weight: s.Value, index: i})
	}
	kept, other := foldSlots(items)
	for _, k := range kept {
		parts = append(parts, ringPart{name: k.name, tone: seriesClass(k.slot), value: k.weight})
		total += k.weight
	}
	if len(other) > 0 {
		var sum int64
		for _, o := range other {
			sum += o.weight
		}
		parts = append(parts, ringPart{name: otherName(other), tone: seriesClass(0), value: sum, members: other})
		total += sum
	}
	return parts, total
}

const (
	ringBox    = 136.0 // the svg's side, px
	ringR      = 54.0  // the ring's center line
	ringGap    = 2.0   // the surface gap between segments, px along the center line
	ringSweepT = 720.0 // the draw-on, ms
)

// sweepTiming is when the arc [a, b] of the ring (fractions of the whole)
// draws on, so the segments sweep in as one ease-out: each arc runs linearly
// between the times ease-out-cubic (1 - (1-t)^3) reaches its two ends.
func sweepTiming(a, b, total float64) (delay, dur int) {
	inv := func(f float64) float64 { return 1 - math.Cbrt(1-f) }
	d0, d1 := inv(a)*total, inv(b)*total
	return int(math.Round(d0)), max(1, int(math.Round(d1-d0)))
}

// ringChart renders the ring: up to five slotted segments plus Other, starting
// at twelve o'clock, a 2px surface gap between them, the total in the center,
// and a legend row per segment (swatch, name, share, figure). One segment is no
// split, so it draws no ring, only its legend line; nothing to share renders
// nothing. On arrival the ring turns into place about its center while its
// segments light in clockwise, once (static/console.css): transform and
// opacity only.
func ringChart(r ringSpec) template.HTML {
	parts, total := r.parts()
	if total <= 0 {
		return ""
	}
	summary := ringSummary(r, parts, total)
	if len(parts) == 1 {
		p := parts[0]
		return template.HTML(`<div class="chart chart-ring single"><ul class="chart-legend" aria-label="` +
			template.HTMLEscapeString(summary) + `">` + legendRow(-1, p.tone, p.name, "", r.Unit.figure(p.value)) + `</ul></div>`)
	}
	c := 2 * math.Pi * ringR
	mid := ringBox / 2
	var segs, hits, legend strings.Builder
	marks := make([]markReadout, 0, len(parts))
	var acc int64
	for i, p := range parts {
		a := float64(acc) / float64(total)
		b := float64(acc+p.value) / float64(total)
		acc += p.value
		arc := (b - a) * c
		g := min(ringGap, arc/2) // a sliver keeps half its length
		delay, dur := sweepTiming(a, b, ringSweepT)
		fmt.Fprintf(&segs, `<circle class="chart-ring-seg %s" data-i="%d" cx="%g" cy="%g" r="%g" fill="none" stroke-dasharray="%.2f %.2f" transform="rotate(%.2f %g %g)" style="--d:%dms;--t:%dms"></circle>`,
			p.tone, i, mid, mid, ringR, arc-g, c, a*360-90+g/2/c*360, mid, mid, delay, dur)
		// The hit arc spans the whole share and is twice the ring's width, so
		// the pointer finds a segment without aiming at its painted pixels.
		fmt.Fprintf(&hits, `<circle class="chart-hit" data-i="%d" cx="%g" cy="%g" r="%g" fill="none" stroke-dasharray="%.2f %.2f" transform="rotate(%.2f %g %g)"></circle>`,
			i, mid, mid, ringR, arc, c, a*360-90, mid, mid)
		phi := (a+b)*math.Pi - math.Pi/2
		m := markReadout{
			Label: p.name, Value: r.Unit.figure(p.value),
			Sub: chartShare(p.value, total) + " of " + r.Unit.figure(total),
			At:  markAt(mid+ringR*math.Cos(phi), mid+ringR*math.Sin(phi)),
		}
		if len(p.members) > 1 {
			for _, o := range p.members {
				m.Rows = append(m.Rows, markRow{Tone: p.tone, Value: r.Unit.figure(o.weight), Name: o.name})
			}
		}
		marks = append(marks, m)
		legend.WriteString(legendRow(i, p.tone, p.name, chartShare(p.value, total), r.Unit.figure(p.value)))
	}
	core := `<strong>` + template.HTMLEscapeString(r.Unit.figure(total)) + `</strong>`
	if r.Core != "" {
		core += `<span>` + template.HTMLEscapeString(r.Core) + `</span>`
	}
	return template.HTML(fmt.Sprintf(`<div class="chart chart-ring"%s><div class="chart-ring-dial">`+
		`<svg class="chart-svg chart-ring-svg chart-walk" viewBox="0 0 %g %g" width="%g" height="%g" role="img" aria-label="%s" tabindex="0">`+
		`<g class="chart-ring-segs" style="transform-origin:%gpx %gpx">%s</g><g class="chart-ring-hits">%s</g></svg>`+
		`<div class="chart-ring-core" aria-hidden="true">%s</div></div><ul class="chart-legend">%s</ul></div>`,
		markAttr(markData{W: ringBox, H: ringBox, Marks: marks}), ringBox, ringBox, ringBox, ringBox,
		template.HTMLEscapeString(summary), mid, mid, segs.String(), hits.String(), core, legend.String()))
}

// ringSummary is the ring's aria-label: the whole, then every part's share.
func ringSummary(r ringSpec, parts []ringPart, total int64) string {
	var b strings.Builder
	b.WriteString(cmp.Or(r.Label, "Share by series") + ": " + r.Unit.figure(total) + " in total")
	for i, p := range parts {
		if i == 0 {
			b.WriteString(". ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(p.name + " " + chartShare(p.value, total) + " (" + r.Unit.figure(p.value) + ")")
	}
	b.WriteString(".")
	return b.String()
}

// ---------------------------------------------------------------------------
// Day rhythm -- hourly columns stacked by series
// ---------------------------------------------------------------------------

// rhythmSeries is one series' value in each hourly column.
type rhythmSeries struct {
	Name   string
	Slot   int     // as ringSlice.Slot
	Values []int64 // one per column of Hours: a missing value is zero, an extra one is ignored
}

// rhythmSpec is a day by the hour, stacked by series: agent-minutes by agent.
type rhythmSpec struct {
	Label    string         // what is measured ("Agent time by hour")
	Unit     chartUnit      // how the values print
	Hours    []time.Time    // the local start of each column: 24, or 23 or 25 on a DST day (the caller's)
	Series   []rhythmSeries // stacked bottom-up in slot order; folded like the ring
	MinScale int64          // the plot's full height stands for at least this much (0: the busiest hour)
}

// rhythmPart is one stacked layer: a kept series, or the folded Other.
type rhythmPart struct {
	name   string
	tone   string
	values []int64 // one per hour
}

func (r rhythmSpec) parts() []rhythmPart {
	n := len(r.Hours)
	items := make([]foldItem, 0, len(r.Series))
	vals := make([][]int64, len(r.Series))
	for i, s := range r.Series {
		v := make([]int64, n)
		var sum int64
		for h := 0; h < n && h < len(s.Values); h++ {
			if s.Values[h] > 0 {
				v[h] = s.Values[h]
				sum += s.Values[h]
			}
		}
		vals[i] = v
		items = append(items, foldItem{name: s.Name, slot: s.Slot, weight: sum, index: i})
	}
	kept, other := foldSlots(items)
	parts := make([]rhythmPart, 0, len(kept)+1)
	for _, k := range kept {
		parts = append(parts, rhythmPart{name: k.name, tone: seriesClass(k.slot), values: vals[k.index]})
	}
	if len(other) > 0 {
		sum := make([]int64, n)
		for _, o := range other {
			for h := range sum {
				sum[h] += vals[o.index][h]
			}
		}
		parts = append(parts, rhythmPart{name: otherName(other), tone: seriesClass(0), values: sum})
	}
	return parts
}

const (
	rhythmW      = 560.0 // viewBox width; the svg scales to its container
	rhythmH      = 112.0 // viewBox height
	rhythmTop    = 16.0  // headroom over the busiest column for its figure
	rhythmBase   = 111.5 // the baseline hairline
	rhythmPadX   = 4.0
	rhythmBarMax = 16.0 // a column never fills its slot: the rest is air
	rhythmGap    = 2.0  // the surface gap between stacked parts
	rhythmRadius = 4.0  // the rounded data end; square at the baseline
	rhythmRise   = 14   // per-column entry stagger, ms
)

// clockLabels is each column's local start ("14:00"), plus the end of the last
// one. A wall-clock time the day holds twice (the hour a DST change repeats)
// carries its zone on both, so "01:00 PDT" and "01:00 PST" stay two hours.
func clockLabels(hours []time.Time) []string {
	seen := map[string]int{}
	for _, h := range hours {
		seen[h.Format("15:04")]++
	}
	out := make([]string, len(hours)+1)
	for i, h := range hours {
		l := h.Format("15:04")
		if seen[l] > 1 {
			l = h.Format("15:04 MST")
		}
		out[i] = l
	}
	if n := len(hours); n > 0 {
		out[n] = hours[n-1].Add(time.Hour).Format("15:04")
	}
	return out
}

// rhythmChart renders the day's rhythm: one column per local hour of the
// caller's day (so a DST day has 23 or 25), stacked bottom-up by slot with a
// 2px surface gap between parts and a rounded data end, on a quiet baseline,
// with the busiest hour's figure on its cap and hour ticks at 00, 06, 12 and
// 18. Two or more series get a key. A day with nothing in it renders nothing.
// The columns rise once on arrival (static/console.css).
func rhythmChart(r rhythmSpec) template.HTML {
	n := len(r.Hours)
	if n == 0 {
		return ""
	}
	parts := r.parts()
	totals := make([]int64, n)
	var sum int64
	peak := 0
	for h := range n {
		for _, p := range parts {
			totals[h] += p.values[h]
		}
		sum += totals[h]
		if totals[h] > totals[peak] {
			peak = h
		}
	}
	if sum <= 0 {
		return ""
	}
	scale := float64(max(totals[peak], r.MinScale))
	slot := (rhythmW - 2*rhythmPadX) / float64(n)
	bar := min(slot*0.64, rhythmBarMax)
	ph := rhythmBase - rhythmTop
	labels := clockLabels(r.Hours)

	var cols, hits strings.Builder
	marks := make([]markReadout, n)
	tops := make([]float64, n)
	for h := range n {
		x := rhythmPadX + float64(h)*slot
		bx, cx := x+(slot-bar)/2, x+slot/2
		tops[h] = rhythmBase
		type seg struct {
			tone   string
			y0, y1 float64 // bottom, top
		}
		var stack []seg
		var cum int64
		for _, p := range parts {
			v := p.values[h]
			if v <= 0 {
				continue
			}
			y0 := rhythmBase - float64(cum)/scale*ph
			cum += v
			stack = append(stack, seg{p.tone, y0, rhythmBase - float64(cum)/scale*ph})
		}
		if len(stack) > 0 {
			// The column rises from its own foot on the baseline: the origin is
			// in the svg's user units, so it holds in every engine without
			// leaning on transform-box for a <g>.
			fmt.Fprintf(&cols, `<g class="chart-col" data-i="%d" style="--d:%dms;transform-origin:%.2fpx %gpx">`, h, h*rhythmRise, cx, rhythmBase)
			for k, s := range stack {
				last := k == len(stack)-1
				top := s.y1
				if !last {
					top += rhythmGap // the part above starts after the gap; the column keeps its true height
				}
				if s.y0-top < 1 {
					top = s.y0 - 1 // a sliver stays visible
				}
				if last {
					rr := min(rhythmRadius, bar/2, s.y0-top)
					fmt.Fprintf(&cols, `<path class="chart-seg %s" d="M%.2f %.2fV%.2fA%.2f %.2f 0 0 1 %.2f %.2fH%.2fA%.2f %.2f 0 0 1 %.2f %.2fV%.2fZ"></path>`,
						s.tone, bx, s.y0, top+rr, rr, rr, bx+rr, top, bx+bar-rr, rr, rr, bx+bar, top+rr, s.y0)
					tops[h] = top
				} else {
					fmt.Fprintf(&cols, `<rect class="chart-seg %s" x="%.2f" y="%.2f" width="%.2f" height="%.2f"></rect>`,
						s.tone, bx, top, bar, s.y0-top)
				}
			}
			cols.WriteString(`</g>`)
		}
		// The whole hour's slot, full height, is the hit target: the pointer
		// aims at an hour, never at a 10px column.
		fmt.Fprintf(&hits, `<rect class="chart-hit" data-i="%d" x="%.2f" y="0" width="%.2f" height="%g"></rect>`, h, x, slot, rhythmH)
		m := markReadout{Label: labels[h] + "-" + labels[h+1], Value: r.Unit.figure(totals[h]), At: markAt(cx, tops[h])}
		for _, p := range parts {
			if p.values[h] > 0 {
				m.Rows = append(m.Rows, markRow{Tone: p.tone, Value: r.Unit.figure(p.values[h]), Name: p.name})
			}
		}
		marks[h] = m
	}

	// The busiest hour's figure rides its cap, in HTML so it keeps its size
	// when the chart scales; near an edge it aligns inward instead of centering.
	pcx := rhythmPadX + (float64(peak)+0.5)*slot
	edge := ""
	switch {
	case pcx < 40:
		edge = " start"
	case pcx > rhythmW-40:
		edge = " end"
	}
	capHTML := fmt.Sprintf(`<span class="chart-cap%s" style="left:%.2f%%;top:%.2f%%">%s</span>`,
		edge, pcx/rhythmW*100, tops[peak]/rhythmH*100, template.HTMLEscapeString(r.Unit.figure(totals[peak])))

	var ticks strings.Builder
	seenHour := map[int]bool{}
	for h, t := range r.Hours {
		if t.Minute() != 0 || t.Hour()%6 != 0 || seenHour[t.Hour()] {
			continue
		}
		seenHour[t.Hour()] = true
		cls := ""
		if h == 0 {
			cls = ` class="first"`
		}
		fmt.Fprintf(&ticks, `<span%s style="left:%.2f%%">%s</span>`, cls, (rhythmPadX+float64(h)*slot)/rhythmW*100, t.Format("15:04"))
	}

	var key strings.Builder
	if len(parts) > 1 {
		key.WriteString(`<ul class="chart-key">`)
		for _, p := range parts {
			key.WriteString(`<li class="` + p.tone + `"><i class="chart-swatch" aria-hidden="true"></i>` + template.HTMLEscapeString(p.name) + `</li>`)
		}
		key.WriteString(`</ul>`)
	}

	summary := fmt.Sprintf("%s, %d hours: %s in total; busiest %s with %s.",
		cmp.Or(r.Label, "By hour"), n, r.Unit.figure(sum), marks[peak].Label, r.Unit.figure(totals[peak]))
	return template.HTML(fmt.Sprintf(`<div class="chart chart-rhythm"%s><div class="chart-plot">`+
		`<svg class="chart-svg chart-rhythm-svg chart-walk" viewBox="0 0 %g %g" role="img" aria-label="%s" tabindex="0">`+
		`<line class="chart-base" x1="0" x2="%g" y1="%g" y2="%g" vector-effect="non-scaling-stroke"></line>`+
		`<g class="chart-cols">%s</g><g class="chart-hits">%s</g></svg>%s</div>`+
		`<div class="chart-ticks" aria-hidden="true">%s</div>%s</div>`,
		markAttr(markData{W: rhythmW, H: rhythmH, Start: peak, Marks: marks}), rhythmW, rhythmH, template.HTMLEscapeString(summary),
		rhythmW, rhythmBase, rhythmBase, cols.String(), hits.String(), capHTML, ticks.String(), key.String()))
}

// ---------------------------------------------------------------------------
// Ranked bars -- one hue, largest first
// ---------------------------------------------------------------------------

// rankRow is one ranked part: a name, its value, and where the name links.
type rankRow struct {
	Name  string // the row's name: text, or the link's text when Href is accepted
	Value int64
	Href  string // optional same-origin path or "#anchor"; a refused one renders the name as text
	Peek  bool   // with an Href: the link opens in the detail pane (data-peek)
	Title string // optional hover title on the name ("no longer exists")
}

// rankSpec is a whole's parts compared on one baseline: agent time by project.
type rankSpec struct {
	Label string    // what is ranked ("Agent time by project")
	Unit  chartUnit // how the values print
	Rows  []rankRow // drawn largest first, ties by name
}

// sorted returns the rows largest first (a negative value counts as zero),
// their total, and the largest value, which spans the full bar.
func (r rankSpec) sorted() (rows []rankRow, total, top int64) {
	rows = slices.Clone(r.Rows)
	for i := range rows {
		rows[i].Value = max(rows[i].Value, 0)
		total += rows[i].Value
	}
	slices.SortStableFunc(rows, func(a, b rankRow) int {
		if c := cmp.Compare(b.Value, a.Value); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	if len(rows) > 0 {
		top = rows[0].Value
	}
	return rows, total, top
}

// rankBars renders ranked horizontal bars in one ink tone, largest first: a
// name, a bar from a shared baseline with a rounded data end, and the figure,
// per row. With an accepted Href the name is a link (with data-peek when Peek
// is set); without one it is text. Nothing to rank renders nothing.
func rankBars(r rankSpec) template.HTML {
	rows, total, top := r.sorted()
	if total <= 0 {
		return ""
	}
	var b strings.Builder
	marks := make([]markReadout, 0, len(rows))
	for i, row := range rows {
		name := template.HTMLEscapeString(row.Name)
		title := ""
		if row.Title != "" {
			title = ` title="` + template.HTMLEscapeString(row.Title) + `"`
		}
		nameHTML := `<span class="chart-rank-name"` + title + `>` + name + `</span>`
		if href, ok := safeHref(row.Href); ok {
			peek := ""
			if row.Peek {
				peek = " data-peek"
			}
			nameHTML = `<a class="chart-rank-name" href="` + href + `"` + peek + title + `>` + name + `</a>`
		}
		fig := r.Unit.figure(row.Value)
		bar := "" // a row at zero keeps its place and figure, and draws no bar
		if row.Value > 0 {
			bar = fmt.Sprintf(`<span class="chart-rank-bar" style="width:%.2f%%"></span>`, float64(row.Value)/float64(top)*100)
		}
		fmt.Fprintf(&b, `<li class="chart-rank-row" data-i="%d">%s<span class="chart-rank-track" aria-hidden="true">%s</span><span class="chart-rank-fig">%s</span></li>`,
			i, nameHTML, bar, template.HTMLEscapeString(fig))
		marks = append(marks, markReadout{Label: row.Name, Value: fig, Sub: chartShare(row.Value, total) + " of " + r.Unit.figure(total)})
	}
	return template.HTML(`<div class="chart chart-rank"` + markAttr(markData{Marks: marks}) + `><ol class="chart-rank-list" aria-label="` +
		template.HTMLEscapeString(rankSummary(r, rows)) + `">` + b.String() + `</ol></div>`)
}

// rankSummary names the first five rows and counts the rest.
func rankSummary(r rankSpec, rows []rankRow) string {
	var b strings.Builder
	b.WriteString(cmp.Or(r.Label, "Ranked") + ": ")
	for i, row := range rows {
		if i == 5 {
			b.WriteString(", and " + plural(len(rows)-5, "more", "more"))
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(row.Name + " " + r.Unit.figure(row.Value))
	}
	b.WriteString(".")
	return b.String()
}

// ---------------------------------------------------------------------------
// Thin segmented bar -- the token mix
// ---------------------------------------------------------------------------

// mixPart is one part of the segmented bar's whole.
type mixPart struct {
	Name  string
	Value int64 // zero or below draws nothing on the bar; its figure stays in the list
}

// mixSpec is one whole split into ordered parts: the day's tokens as output,
// fresh input, cache write and cache read.
type mixSpec struct {
	Label string    // what the whole is ("Token mix")
	Unit  chartUnit // how the values print
	Parts []mixPart // in ramp order: the first wears the strongest step, the fourth the lightest; later ones the neutral
}

// mixSteps is how many steps the ink ramp has (mix-1..4 in static/console.css).
const mixSteps = 4

// mixClass is a part's ramp step by its position, or Other's neutral past it.
func mixClass(i int) string {
	if i < mixSteps {
		return "mix-" + strconv.Itoa(i+1)
	}
	return seriesClass(0)
}

// mixBar renders the thin segmented bar: one 8px whole, its parts in their
// given order, widths by value, a 2px surface gap between them and no text on
// the bar; the figures sit beside it in a list (swatch, name, share, figure).
// Nothing in the whole renders nothing.
func mixBar(m mixSpec) template.HTML {
	var total int64
	for _, p := range m.Parts {
		total += max(p.Value, 0)
	}
	if total <= 0 {
		return ""
	}
	var bar, list strings.Builder
	marks := make([]markReadout, 0, len(m.Parts))
	start := 0
	for i, p := range m.Parts {
		v := max(p.Value, 0)
		tone := mixClass(i)
		if v > 0 {
			fmt.Fprintf(&bar, `<i class="chart-mix-seg %s" data-i="%d" style="flex-grow:%.3f"></i>`, tone, i, float64(v)/float64(total)*100)
		}
		if v > max(m.Parts[start].Value, 0) {
			start = i
		}
		list.WriteString(legendRow(i, tone, p.Name, chartShare(v, total), m.Unit.figure(v)))
		marks = append(marks, markReadout{Label: p.Name, Value: m.Unit.figure(v), Sub: chartShare(v, total) + " of " + m.Unit.figure(total)})
	}
	var sum strings.Builder
	sum.WriteString(cmp.Or(m.Label, "Mix") + ": " + m.Unit.figure(total) + " in total")
	for i, p := range m.Parts {
		if i == 0 {
			sum.WriteString(". ")
		} else {
			sum.WriteString(", ")
		}
		sum.WriteString(p.Name + " " + chartShare(max(p.Value, 0), total))
	}
	sum.WriteString(".")
	return template.HTML(`<div class="chart chart-mix"` + markAttr(markData{Start: start, Marks: marks}) +
		`><div class="chart-mix-bar chart-walk" role="img" aria-label="` + template.HTMLEscapeString(sum.String()) + `" tabindex="0">` +
		bar.String() + `</div><ul class="chart-legend chart-mix-legend">` + list.String() + `</ul></div>`)
}

// ---------------------------------------------------------------------------
// Month heat grid -- one hue in five steps, a cell per day
// ---------------------------------------------------------------------------

// monthDay is one day of the month grid.
type monthDay struct {
	Value int64  // its magnitude: agent time
	Href  string // optional same-origin path or "#anchor": the cell becomes a link
	Note  string // optional second line of the day's readout ("14 sessions in 6 projects")
}

// monthSpec is a month as a calendar, a cell per day shaded by magnitude.
type monthSpec struct {
	Label    string     // what shades the cells ("Agent time per day")
	Unit     chartUnit  // how the values print
	Start    time.Time  // the month's first day, at local midnight
	Days     []monthDay // one per day from Start, in order
	Today    string     // "2006-01-02": outlined; the days after it are still to come, quiet and never links
	Selected string     // "2006-01-02" of the day the page shows, outlined; "" for none
}

// monthGrid renders the month heat grid: a Sunday-first calendar, one cell per
// day carrying its date as data-day, shaded in five steps of the brand hue
// (l0 a day with nothing in it -- a pause, never a warning -- to l4 the
// busiest), today and the selected day outlined. A day with an accepted Href
// is a real link around its cell, so the grid works without JS and a modified
// click opens a new tab; one without is not interactive, and a day still to
// come is never a link. It is the Sessions calendar's sibling and shares none
// of its classes, so the calendar's click handler never sees these cells.
func monthGrid(m monthSpec) template.HTML {
	n := len(m.Days)
	if n == 0 {
		return ""
	}
	keys := make([]string, n)
	future := make([]bool, n)
	var maxV, total int64
	active, busiest := 0, -1
	for i, d := range m.Days {
		keys[i] = m.Start.AddDate(0, 0, i).Format("2006-01-02")
		future[i] = m.Today != "" && keys[i] > m.Today
		v := max(d.Value, 0)
		if future[i] || v == 0 {
			continue
		}
		active++
		total += v
		if v > maxV {
			maxV, busiest = v, i
		}
	}

	var cells strings.Builder
	marks := make([]markReadout, n)
	for i, d := range m.Days {
		day := m.Start.AddDate(0, 0, i)
		v := max(d.Value, 0)
		lvl := 0
		if !future[i] && v > 0 && maxV > 0 {
			lvl = int(min(4, 1+(v*4-1)/maxV))
		}
		cls := "chart-month-cell l" + strconv.Itoa(lvl)
		if future[i] {
			cls += " future"
		}
		if keys[i] == m.Today {
			cls += " today"
		}
		if keys[i] == m.Selected {
			cls += " sel"
		}
		label := day.Format("Mon, Jan 02")
		marks[i] = markReadout{Label: label}
		aria := label
		mark := ""
		if !future[i] {
			fig := m.Unit.figure(v)
			marks[i].Value, marks[i].Sub = fig, d.Note
			aria += ": " + fig
			if d.Note != "" {
				aria += ", " + d.Note
			}
			mark = ` data-i="` + strconv.Itoa(i) + `"`
		}
		style := ""
		if i == 0 {
			style = ` style="grid-column-start:` + strconv.Itoa(int(m.Start.Weekday())+1) + `"`
		}
		num := strconv.Itoa(day.Day())
		if href, ok := safeHref(d.Href); ok && !future[i] {
			fmt.Fprintf(&cells, `<a class="%s" href="%s" data-day="%s"%s aria-label="%s"%s>%s</a>`,
				cls, href, keys[i], mark, template.HTMLEscapeString(aria), style, num)
		} else {
			fmt.Fprintf(&cells, `<span class="%s" data-day="%s"%s role="img" aria-label="%s"%s>%s</span>`,
				cls, keys[i], mark, template.HTMLEscapeString(aria), style, num)
		}
	}

	summary := cmp.Or(m.Label, "By day") + ", " + m.Start.Format("January 2006") + ": "
	if active == 0 {
		summary += "nothing recorded."
	} else {
		summary += fmt.Sprintf("%s, %s in total; busiest %s with %s.", plural(active, "active day", "active days"),
			m.Unit.figure(total), m.Start.AddDate(0, 0, busiest).Format("Mon, Jan 02"), m.Unit.figure(maxV))
	}
	return template.HTML(`<div class="chart chart-month"` + markAttr(markData{Marks: marks}) + ` role="group" aria-label="` +
		template.HTMLEscapeString(summary) + `"><div class="chart-month-wd" aria-hidden="true"><span>Su</span><span>Mo</span><span>Tu</span><span>We</span><span>Th</span><span>Fr</span><span>Sa</span></div>` +
		`<div class="chart-month-grid">` + cells.String() + `</div></div>`)
}

// ---------------------------------------------------------------------------
// Presence strip -- when in the day something was worked
// ---------------------------------------------------------------------------

// presenceSpan is one lit stretch of the day.
type presenceSpan struct{ From, To time.Time }

// presenceSpec is a thin 24-hour strip of lit stretches on the rhythm's axis.
type presenceSpec struct {
	Label string         // what the strip shows ("When it was worked")
	Hours []time.Time    // the day's hourly columns, the slice the rhythm chart takes: the strip's axis
	Spans []presenceSpan // lit stretches; clipped to the axis, overlapping ones merged
}

// litSpans clips the spans to [start, end), drops the empty ones, and merges
// the ones that overlap or touch, earliest first.
func litSpans(spans []presenceSpan, start, end time.Time) []presenceSpan {
	var out []presenceSpan
	for _, s := range spans {
		if s.From.Before(start) {
			s.From = start
		}
		if s.To.After(end) {
			s.To = end
		}
		if s.To.After(s.From) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b presenceSpan) int { return a.From.Compare(b.From) })
	merged := out[:0]
	for _, s := range out {
		if k := len(merged) - 1; k >= 0 && !s.From.After(merged[k].To) {
			if s.To.After(merged[k].To) {
				merged[k].To = s.To
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// presenceStrip renders the thin strip: the day from the first column's start
// to the last one's end (so a DST day keeps the rhythm's 23 or 25 hours), a
// quiet track with notches at 06, 12 and 18, and each lit stretch as a
// segment. Nothing lit renders nothing.
func presenceStrip(p presenceSpec) template.HTML {
	n := len(p.Hours)
	if n == 0 {
		return ""
	}
	start, end := p.Hours[0], p.Hours[n-1].Add(time.Hour)
	span := float64(end.Sub(start))
	lit := litSpans(p.Spans, start, end)
	if span <= 0 || len(lit) == 0 {
		return ""
	}
	var notches, segs strings.Builder
	seen := map[int]bool{}
	for _, t := range p.Hours[1:] {
		if t.Minute() != 0 || t.Hour()%6 != 0 || seen[t.Hour()] {
			continue
		}
		seen[t.Hour()] = true
		fmt.Fprintf(&notches, `<i class="chart-presence-notch" style="left:%.3f%%"></i>`, float64(t.Sub(start))/span*100)
	}
	marks := make([]markReadout, 0, len(lit))
	var sum int64
	parts := make([]string, 0, len(lit))
	for i, s := range lit {
		fmt.Fprintf(&segs, `<i class="chart-presence-seg" data-i="%d" style="left:%.3f%%;width:%.3f%%"></i>`,
			i, float64(s.From.Sub(start))/span*100, float64(s.To.Sub(s.From))/span*100)
		mins := int64(math.Round(s.To.Sub(s.From).Minutes()))
		sum += mins
		when := s.From.Format("15:04") + "-" + s.To.Format("15:04")
		parts = append(parts, when)
		marks = append(marks, markReadout{Label: when, Value: chartMinutes(mins)})
	}
	summary := cmp.Or(p.Label, "Presence") + ": " + strings.Join(parts, ", ") + "; " + chartMinutes(sum) + " in all."
	return template.HTML(`<div class="chart chart-presence"` + markAttr(markData{Marks: marks}) +
		`><div class="chart-presence-track chart-walk" role="img" aria-label="` + template.HTMLEscapeString(summary) + `" tabindex="0">` +
		notches.String() + segs.String() + `</div></div>`)
}

// ---------------------------------------------------------------------------
// Stat tile -- the number is the chart
// ---------------------------------------------------------------------------

// tileSpec is one headline number: the Overview's vital tile, for a volume.
type tileSpec struct {
	Label  string // sentence case, no trailing colon; a tile without one renders nothing
	Value  string // the formatted figure (chartUnit.figure); empty shows an em dash
	Icon   string // optional icon name (see icon)
	Sub    string // optional line under the value
	Delta  delta  // optional comparison (atoms.go): a volume is noJudgment, and an absent one renders nothing
	Spark  spark  // optional trend; under two points renders nothing
	Anchor string // optional in-page "#id" the tile jumps to: the section that lists its items
}

// statTile renders a stat tile in the Overview vitals' markup and classes, so
// it reads as one of them: label, value, the delta chip and the sparkline.
// With an accepted Anchor the tile is a link down the page; without one it is
// a plain tile that does not lift on hover.
func statTile(t tileSpec) template.HTML {
	if t.Label == "" {
		return ""
	}
	value := template.HTMLEscapeString(t.Value)
	if t.Value == "" {
		value = "n/a"
	}
	label := string(icon(t.Icon)) + template.HTMLEscapeString(t.Label)
	open, end := `<div class="ov2-vital chart-tile">`, `</div>`
	if a, ok := safeAnchor(t.Anchor); ok {
		open, end = `<a class="ov2-vital chart-tile" href="`+a+`">`, `</a>`
		label += string(icon("arrow-down"))
	}
	var b strings.Builder
	b.WriteString(open)
	b.WriteString(`<span class="ov2-vital-label">` + label + `</span>`)
	b.WriteString(`<div class="ov2-vital-num"><strong>` + value + `</strong>` + string(deltaChip(t.Delta)) + `</div>`)
	if t.Sub != "" {
		b.WriteString(`<span class="ov2-vital-sub">` + template.HTMLEscapeString(t.Sub) + `</span>`)
	}
	if sp := sparkLine(t.Spark); sp != "" {
		b.WriteString(`<div class="ov2-vital-foot">` + string(sp) + `</div>`)
	}
	b.WriteString(end)
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Table view -- every chart's accessible twin
// ---------------------------------------------------------------------------

// dataTable is a chart's data as rows, for the "show as table" disclosure:
// the first column names the row, every other column is a figure.
type dataTable struct {
	Caption string
	Head    []string
	Rows    [][]string
	Foot    []string // the total row, or nil
}

// html renders the table, everything escaped; no rows renders nothing.
func (t dataTable) html() template.HTML {
	if len(t.Rows) == 0 {
		return ""
	}
	esc := template.HTMLEscapeString
	row := func(b *strings.Builder, cells []string, head bool) {
		b.WriteString(`<tr>`)
		for i, c := range cells {
			switch {
			case head:
				cls := ""
				if i > 0 {
					cls = ` class="num"`
				}
				b.WriteString(`<th scope="col"` + cls + `>` + esc(c) + `</th>`)
			case i == 0:
				b.WriteString(`<th scope="row">` + esc(c) + `</th>`)
			default:
				b.WriteString(`<td class="num">` + esc(c) + `</td>`)
			}
		}
		b.WriteString(`</tr>`)
	}
	var b strings.Builder
	b.WriteString(`<table class="chart-table">`)
	if t.Caption != "" {
		b.WriteString(`<caption>` + esc(t.Caption) + `</caption>`)
	}
	b.WriteString(`<thead>`)
	row(&b, t.Head, true)
	b.WriteString(`</thead><tbody>`)
	for _, r := range t.Rows {
		row(&b, r, false)
	}
	b.WriteString(`</tbody>`)
	if len(t.Foot) > 0 {
		b.WriteString(`<tfoot>`)
		row(&b, t.Foot, false)
		b.WriteString(`</tfoot>`)
	}
	b.WriteString(`</table>`)
	return template.HTML(b.String())
}

// ringTable is the ring as a table: a row per segment, Other naming its series.
func ringTable(r ringSpec) template.HTML {
	parts, total := r.parts()
	if total <= 0 {
		return ""
	}
	t := dataTable{Caption: cmp.Or(r.Label, "Share by series"), Head: []string{"Name", "Share", "Value"}}
	for _, p := range parts {
		name := p.name
		if len(p.members) > 1 {
			names := make([]string, len(p.members))
			for i, o := range p.members {
				names[i] = o.name
			}
			name += ": " + strings.Join(names, ", ")
		}
		t.Rows = append(t.Rows, []string{name, chartShare(p.value, total), r.Unit.figure(p.value)})
	}
	t.Foot = []string{"Total", "100%", r.Unit.figure(total)}
	return t.html()
}

// rhythmTable is the rhythm as a table: a row per hour, a column per series.
func rhythmTable(r rhythmSpec) template.HTML {
	n := len(r.Hours)
	parts := r.parts()
	if n == 0 || len(parts) == 0 {
		return ""
	}
	labels := clockLabels(r.Hours)
	t := dataTable{Caption: cmp.Or(r.Label, "By hour"), Head: []string{"Hour"}}
	for _, p := range parts {
		t.Head = append(t.Head, p.name)
	}
	t.Head = append(t.Head, "Total")
	sums := make([]int64, len(parts))
	var all int64
	for h := range n {
		cells := []string{labels[h] + "-" + labels[h+1]}
		var tot int64
		for k, p := range parts {
			cells = append(cells, r.Unit.figure(p.values[h]))
			tot += p.values[h]
			sums[k] += p.values[h]
		}
		all += tot
		t.Rows = append(t.Rows, append(cells, r.Unit.figure(tot)))
	}
	if all <= 0 {
		return ""
	}
	t.Foot = []string{"Total"}
	for _, s := range sums {
		t.Foot = append(t.Foot, r.Unit.figure(s))
	}
	t.Foot = append(t.Foot, r.Unit.figure(all))
	return t.html()
}

// rankTable is the ranked bars as a table, largest first.
func rankTable(r rankSpec) template.HTML {
	rows, total, _ := r.sorted()
	if total <= 0 {
		return ""
	}
	t := dataTable{Caption: cmp.Or(r.Label, "Ranked"), Head: []string{"Name", "Value", "Share"}}
	for _, row := range rows {
		t.Rows = append(t.Rows, []string{row.Name, r.Unit.figure(row.Value), chartShare(row.Value, total)})
	}
	t.Foot = []string{"Total", r.Unit.figure(total), "100%"}
	return t.html()
}

// mixTable is the segmented bar as a table, in the bar's order.
func mixTable(m mixSpec) template.HTML {
	var total int64
	for _, p := range m.Parts {
		total += max(p.Value, 0)
	}
	if total <= 0 {
		return ""
	}
	t := dataTable{Caption: cmp.Or(m.Label, "Mix"), Head: []string{"Part", "Value", "Share"}}
	for _, p := range m.Parts {
		v := max(p.Value, 0)
		t.Rows = append(t.Rows, []string{p.Name, m.Unit.figure(v), chartShare(v, total)})
	}
	t.Foot = []string{"Total", m.Unit.figure(total), "100%"}
	return t.html()
}

// monthTable is the month grid as a table: a row per day up to today (a day
// still to come has nothing to say yet).
func monthTable(m monthSpec) template.HTML {
	t := dataTable{Caption: cmp.Or(m.Label, "By day") + ", " + m.Start.Format("January 2006"), Head: []string{"Day", "Value"}}
	var total int64
	for i, d := range m.Days {
		day := m.Start.AddDate(0, 0, i)
		if m.Today != "" && day.Format("2006-01-02") > m.Today {
			break
		}
		v := max(d.Value, 0)
		total += v
		t.Rows = append(t.Rows, []string{day.Format("Mon, Jan 02"), m.Unit.figure(v)})
	}
	t.Foot = []string{"Total", m.Unit.figure(total)}
	return t.html()
}

// presenceTable is the presence strip as a table, a row per lit stretch.
func presenceTable(p presenceSpec) template.HTML {
	n := len(p.Hours)
	if n == 0 {
		return ""
	}
	lit := litSpans(p.Spans, p.Hours[0], p.Hours[n-1].Add(time.Hour))
	t := dataTable{Caption: cmp.Or(p.Label, "Presence"), Head: []string{"From", "To", "Duration"}}
	var sum int64
	for _, s := range lit {
		mins := int64(math.Round(s.To.Sub(s.From).Minutes()))
		sum += mins
		t.Rows = append(t.Rows, []string{s.From.Format("15:04"), s.To.Format("15:04"), chartMinutes(mins)})
	}
	t.Foot = []string{"Total", "", chartMinutes(sum)}
	return t.html()
}
