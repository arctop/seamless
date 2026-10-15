package console

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"html/template"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The knowledge sky: the Overview's star chart of everything the fleet knows,
// built to stay readable at hundreds or thousands of memories.
//
// Every active memory is a star, and none is dropped. Angle is scope: each
// project, and the global scope, owns a wedge sized by its share of the
// memories, largest first, clockwise from the top. Radius is when the memory
// last surfaced into an agent's context: the core holds the last 24 hours, the
// rings sit at the console's own fresh and stale thresholds, and the rim belt
// holds what has never surfaced. Colour is kind; size is demand -- the
// query-gated utility score -- so a big star is one agents pull, not one the
// briefing merely shows.
//
// Each ring's area follows how many stars it holds (with a floor, so an empty
// ring still reads as a ring), which keeps the density even: a busy core does
// not pile up and a sparse belt does not sprawl. Inside a ring, a scope's stars
// take slots on a golden-ratio lattice keyed by memory id, so a star keeps its
// place while its scope and ring are unchanged. Ring boundaries and wedge
// shares move in steps (skyAreaStep, skyShares), not with every count: an
// exact share would shift every ring or wedge a little whenever one memory
// surfaced or was written, and with it hundreds of stars. A live morph
// therefore moves only the stars whose standing changed -- a memory that just
// surfaced glides into the core -- instead of reshuffling the sky.
//
// The SVG is the picture; the panel beside it (overview.html) states the same
// numbers as text, and static/sky.js adds search, filters, and the live beams.
// The chart is drawn as keyed, fingerprinted pieces (skyChart), so a live
// refresh sends a page only the pieces that changed since the chart it holds
// (skyLedger).

const (
	skyBox      = 600.0              // square viewBox edge
	skyC        = skyBox / 2         // centre
	skyRim      = 262.0              // outer radius of the star field
	skyCore     = 38.0               // the "now" core
	skyGutter   = 16 * math.Pi / 180 // the ring-label gutter at 12 o'clock
	skyEvenArea = 0.28               // share of the field split evenly between rings; the rest follows their counts
	skyAreaStep = 0.02               // ring boundaries snap to this share of the field's area
	skyShares   = 150                // wedges share the circle in about this many units of memories
	skyLoad     = 0.78               // target occupancy of a cell's slot lattice
	skyHaloAt   = 0.75               // utility from which a star wears a halo
	skyGlyph    = 7.0                // advance of one rim-label glyph (10-unit mono, tracked), in viewBox units
	skyDigit    = 5.6                // advance of one count digit
	skyLabelIn  = skyRim + 10        // rim-label baseline on the top half
	skyLabelOut = skyRim + 17        // on the bottom half, where glyphs hang toward the centre
	invPhi      = 0.6180339887498949 // 1/golden ratio: the lattice's angular step
	degree      = math.Pi / 180
)

// skyBand is a ring: how long ago a memory last surfaced into an agent's
// context. The thresholds are the console's judged ones (staleSurfacedOKDays,
// staleSurfacedDays), so the sky, the memory reader's chip, and the Overview's
// "going stale" card all draw the same lines.
type skyBand int

const (
	bandDay   skyBand = iota // within the last 24 hours
	bandWeek                 // within staleSurfacedOKDays
	bandMonth                // within staleSurfacedDays
	bandStale                // longer ago than that
	bandNever                // never surfaced
	skyBands
)

// label is the readout row for the ring.
func (b skyBand) label() string {
	switch b {
	case bandDay:
		return "Last 24 hours"
	case bandWeek:
		return fmt.Sprintf("1 to %d days ago", staleSurfacedOKDays)
	case bandMonth:
		return fmt.Sprintf("%d to %d days ago", staleSurfacedOKDays, staleSurfacedDays)
	case bandStale:
		return fmt.Sprintf("Over %d days ago", staleSurfacedDays)
	default:
		return "Never surfaced"
	}
}

// tick is the mark on the ring's outer edge in the gutter ("" for none: the
// stale ring's outer edge is where the never belt begins, not a time).
func (b skyBand) tick() string {
	switch b {
	case bandDay:
		return "24h"
	case bandWeek:
		return strconv.Itoa(staleSurfacedOKDays) + "d"
	case bandMonth:
		return strconv.Itoa(staleSurfacedDays) + "d"
	default:
		return ""
	}
}

// skyCuts are the ring boundaries for one render.
type skyCuts struct{ day, week, staleAt time.Time }

func skyCutoffs(now time.Time) skyCuts {
	u := now.UTC()
	return skyCuts{
		day:     u.Add(-24 * time.Hour),
		week:    u.AddDate(0, 0, -staleSurfacedOKDays),
		staleAt: u.AddDate(0, 0, -staleSurfacedDays),
	}
}

func (c skyCuts) band(surfaced *time.Time) skyBand {
	switch {
	case surfaced == nil || surfaced.IsZero():
		return bandNever
	case !surfaced.Before(c.day):
		return bandDay
	case !surfaced.Before(c.week):
		return bandWeek
	case !surfaced.Before(c.staleAt):
		return bandMonth
	default:
		return bandStale
	}
}

// stale is the Overview's "going stale" predicate verbatim
// (store.CountMemoriesUnsurfacedSince): old enough to have had its turn, and not
// surfaced within the window. The readout's count and the attention card's
// therefore always agree.
func (c skyCuts) stale(surfaced *time.Time, created time.Time) bool {
	return created.Before(c.staleAt) && (surfaced == nil || surfaced.IsZero() || surfaced.Before(c.staleAt))
}

// skyStar is one memory as the sky draws it.
type skyStar struct {
	ID, Name, Desc, Kind, Project string
	Tags                          []string
	Band                          skyBand
	Stale, Fav, New               bool
	Surfaced                      *time.Time
	Created                       time.Time
	Injects, Reads                int
	Utility                       float64
	x, y                          float64
}

// skyScope is one wedge.
type skyScope struct {
	slug   string
	n      int
	bands  [skyBands]int
	stale  int
	a0, a1 float64 // wedge span, radians clockwise from 12 o'clock
	stars  []*skyStar
}

func (sc *skyScope) name() string {
	if sc.slug == "" {
		return "global"
	}
	return sc.slug
}

// skyData is the Overview panel payload (HTML only: overviewData.Sky is
// json:"-"). SVG is the chart; the rest is the readout beside it.
type skyData struct {
	SVG       template.HTML
	Total     int
	Stale     int
	StaleDays int
	Bands     []skyBandRow
	Scopes    []skyScopeRow
	Kinds     []kindCount

	// Version names this panel's content for the live protocol (skyLedger).
	// Base is set when the answer goes to a page that holds an earlier panel
	// this daemon remembers: Keep means that page already holds Version, and
	// otherwise SVG is a delta against Base, with KeepReadout and KeepKinds
	// saying which template-drawn parts it can keep as they are.
	Version     string
	Base        string
	Keep        bool
	KeepReadout bool
	KeepKinds   bool

	chart *skyChart
	mark  *skyMark
}

// skyBandRow is one ring in the readout. W is the bar length relative to the
// fullest ring, so the rows compare at a glance.
type skyBandRow struct {
	Band  int
	Label string
	N     int
	Pct   int
	W     string
}

// skyScopeRow is one scope in the readout: its size, how much of it is going
// stale, and its ring mix as a stacked bar (Segs are percent widths by band).
type skyScopeRow struct {
	Slug  string
	Name  string
	N     int
	Stale int
	Segs  []skySeg
	Href  string
}

type skySeg struct {
	Band int
	W    string
}

// buildSky projects the active memories into the sky. It returns nil when
// there is nothing to draw, so the template can drop the panel.
func buildSky(mems []core.Memory, stats map[string]store.RetrievalStat, now time.Time) *skyData {
	cuts := skyCutoffs(now)
	byScope := map[string]*skyScope{}
	var bandN [skyBands]int
	kinds := map[string]int{}
	total, stale := 0, 0
	for _, m := range mems {
		if !m.Active() {
			continue
		}
		st := stats[m.ID]
		s := &skyStar{
			ID: m.ID, Name: m.Name, Desc: m.Description, Kind: string(m.Kind), Project: m.Project,
			Tags: m.Tags, Created: m.Created, Fav: m.Favorite,
			Injects: st.InjectCount, Reads: st.ReadCount, Utility: st.Utility,
			Surfaced: st.LastInjectedAt,
		}
		s.Band = cuts.band(s.Surfaced)
		s.Stale = cuts.stale(s.Surfaced, m.Created)
		s.New = !m.Created.IsZero() && now.Sub(m.Created) < 24*time.Hour

		sc := byScope[m.Project]
		if sc == nil {
			sc = &skyScope{slug: m.Project}
			byScope[m.Project] = sc
		}
		sc.stars = append(sc.stars, s)
		sc.n++
		sc.bands[s.Band]++
		bandN[s.Band]++
		kinds[s.Kind]++
		total++
		if s.Stale {
			sc.stale++
			stale++
		}
	}
	if total == 0 {
		return nil
	}

	// Largest scope first, clockwise from the gutter; the readout lists them in
	// the same order, so the list reads around the sky.
	scopes := make([]*skyScope, 0, len(byScope))
	for _, sc := range byScope {
		scopes = append(scopes, sc)
	}
	slices.SortFunc(scopes, func(a, b *skyScope) int {
		if c := cmp.Compare(b.n, a.n); c != 0 {
			return c
		}
		return cmp.Compare(a.slug, b.slug)
	})
	layoutWedges(scopes, total)
	radii := ringRadii(bandN, total)
	for _, sc := range scopes {
		var cells [skyBands][]*skyStar
		for _, s := range sc.stars {
			cells[s.Band] = append(cells[s.Band], s)
		}
		for b, cell := range cells {
			if len(cell) > 0 {
				placeCell(cell, sc.a0, sc.a1, radii[b], radii[b+1])
			}
		}
	}

	chart := renderSky(scopes, radii, bandN, total)
	d := &skyData{
		SVG:       chart.full(),
		Total:     total,
		Stale:     stale,
		StaleDays: staleSurfacedDays,
		Kinds:     skyKinds(kinds),
		chart:     chart,
	}
	fullest := slices.Max(bandN[:])
	for b := range skyBands {
		d.Bands = append(d.Bands, skyBandRow{
			Band: int(b), Label: b.label(), N: bandN[b],
			Pct: int(math.Round(100 * float64(bandN[b]) / float64(total))),
			W:   pct(float64(bandN[b]) / float64(max(fullest, 1))),
		})
	}
	for _, sc := range scopes {
		row := skyScopeRow{Slug: sc.slug, Name: sc.name(), N: sc.n, Stale: sc.stale}
		if sc.slug != "" {
			row.Href = "/console/projects/" + sc.slug + "?tab=memories"
		}
		for b, n := range sc.bands {
			if n > 0 {
				row.Segs = append(row.Segs, skySeg{Band: b, W: pct(float64(n) / float64(sc.n))})
			}
		}
		d.Scopes = append(d.Scopes, row)
	}
	// The readout and the kind legend are drawn by the template from these
	// rows, so the rows stand in for their markup in the fingerprint.
	d.mark = chart.mark(
		skySum(fmt.Sprint(d.Bands, d.Stale, d.StaleDays, d.Scopes)),
		skySum(fmt.Sprint(d.Kinds)))
	d.Version = d.mark.version
	return d
}

// layoutWedges gives each scope its span. Every scope gets a floor so a scope
// of one memory is still a visible, hoverable wedge; the rest of the circle is
// shared by count, which keeps star density even across scopes.
//
// The count is taken in whole units of about 1/skyShares of all memories (at
// least one unit per scope), not star by star. Every wedge after a scope that
// grows moves round by its growth, so an exact share would turn each newly
// written memory into a small move of nearly every star; in units, the wedges
// hold still until a scope has grown or shrunk by a unit.
func layoutWedges(scopes []*skyScope, total int) {
	n := float64(len(scopes))
	gap := math.Min(0.9*degree, 36*degree/n)
	avail := 2*math.Pi - skyGutter - (n-1)*gap
	floor := math.Min(2.2*degree, 0.5*avail/n)
	flex := avail - n*floor
	unit := max(1, math.Round(float64(total)/skyShares))
	units := make([]float64, len(scopes))
	sum := 0.0
	for i, sc := range scopes {
		units[i] = max(1, math.Round(float64(sc.n)/unit))
		sum += units[i]
	}
	a := skyGutter / 2
	for i, sc := range scopes {
		w := floor + flex*units[i]/sum
		sc.a0, sc.a1 = a, a+w
		a += w + gap
	}
}

// ringRadii returns the ring boundaries, core outwards: radii[b] is ring b's
// inner edge and radii[b+1] its outer one. Area, not radius, is what a star
// needs, so the boundaries are placed on cumulative area shares, snapped to
// steps of skyAreaStep: an exact boundary would move whenever one memory
// changed ring, and with it every star in the rings on either side of it.
// Every ring's floor (skyEvenArea) is far wider than a step, so snapping never
// closes a ring.
func ringRadii(bandN [skyBands]int, total int) [skyBands + 1]float64 {
	var radii [skyBands + 1]float64
	radii[0] = skyCore
	inner, outer := skyCore*skyCore, skyRim*skyRim
	cum := 0.0
	for b := range skyBands {
		cum += skyEvenArea/float64(skyBands) + (1-skyEvenArea)*float64(bandN[b])/float64(total)
		snapped := math.Round(math.Min(cum, 1)/skyAreaStep) * skyAreaStep
		radii[b+1] = math.Sqrt(inner + snapped*(outer-inner))
	}
	radii[skyBands] = skyRim
	return radii
}

// placeCell seats one scope's stars in one ring. The cell carries a lattice of
// slots -- a golden-ratio (Fibonacci) lattice mapped area-uniformly onto the
// ring sector, the most even spread a fixed point set gets -- with some room to
// spare, and each star takes the slot its id hashes to, probing forward on a
// collision. Stars are seated in id order, and ULIDs are creation-ordered, so a
// newly written memory takes a free slot rather than displacing anyone; the
// lattice is only redrawn when a cell outgrows its size class.
func placeCell(cell []*skyStar, a0, a1, ri, ro float64) {
	slices.SortFunc(cell, func(a, b *skyStar) int { return cmp.Compare(a.ID, b.ID) })
	ids := make([]string, len(cell))
	for i, s := range cell {
		ids[i] = s.ID
	}
	slots := skySlots(len(cell))
	padR := math.Min(4, 0.2*(ro-ri))
	ri, ro = ri+padR, ro-padR
	padA := math.Min(0.18*(a1-a0), 4/((ri+ro)/2))
	a0, a1 = a0+padA, a1-padA
	for i, p := range seatCell(ids, slots) {
		u := (float64(p) + 0.5) / float64(slots)
		v := math.Mod(0.5+float64(p)*invPhi, 1)
		cell[i].x, cell[i].y = polar(math.Sqrt(ri*ri+u*(ro*ro-ri*ri)), a0+v*(a1-a0))
	}
}

// seatCell assigns each id (in the given order) the lattice slot its hash
// names, probing forward past taken ones.
func seatCell(ids []string, slots int) []int {
	taken := make([]bool, slots)
	out := make([]int, len(ids))
	for i, id := range ids {
		p := int(skyHash(id) % uint32(slots))
		for taken[p] {
			p = (p + 1) % slots
		}
		taken[p] = true
		out[i] = p
	}
	return out
}

// skySlots is the lattice size for n stars: the smallest size class that keeps
// occupancy at or under skyLoad. Classes grow by about a third, so a busy cell
// is redrawn rarely.
func skySlots(n int) int {
	need := int(math.Ceil(float64(n) / skyLoad))
	s := 1
	for s < need {
		s = max(s+1, int(math.Ceil(float64(s)*1.34)))
	}
	return s
}

// renderSky draws the chart as keyed pieces: the svg start tag, the frame
// layers in document order, then the stars. Stars come last and in id order:
// the morph keys them by id, and a stable order means it only ever inserts or
// removes one, never re-seats the rest (a moved node would drop its glide
// transition).
func renderSky(scopes []*skyScope, radii [skyBands + 1]float64, bandN [skyBands]int, total int) *skyChart {
	alt := fmt.Sprintf("Knowledge sky: %d active memories in %d scopes, placed by when each last surfaced to an agent: "+
		"%d in the last 24 hours, %d within %d days, %d within %d days, %d longer ago, %d never.",
		total, len(scopes), bandN[bandDay], bandN[bandWeek], staleSurfacedOKDays,
		bandN[bandMonth], staleSurfacedDays, bandN[bandStale], bandN[bandNever])
	c := &skyChart{
		open: fmt.Sprintf(`<svg class="sky-svg" viewBox="0 0 %g %g" preserveAspectRatio="xMidYMid meet" role="img" aria-label="%s" data-c="%g" data-core="%g" data-rim="%g">`,
			skyBox, skyBox, template.HTMLEscapeString(alt), skyC, skyCore, skyRim),
	}

	// Gradients: the core's glow, and one halo per kind (a gradient cannot
	// inherit a colour from the star that uses it). The rim labels' arcs live
	// here too, beside the text that is set on them.
	var defs, labels strings.Builder
	defs.WriteString(`<defs id="sky-defs"><radialGradient id="sky-core-glow"><stop offset="0" class="sky-core-stop0"/><stop offset=".6" class="sky-core-stop1"/><stop offset="1" class="sky-core-stop2"/></radialGradient>`)
	for _, kd := range core.MemoryKinds {
		fmt.Fprintf(&defs, `<radialGradient id="sky-h-%s"><stop offset="0" style="stop-color:var(--k-%s);stop-opacity:.5"/><stop offset=".45" style="stop-color:var(--k-%s);stop-opacity:.14"/><stop offset="1" style="stop-color:var(--k-%s);stop-opacity:0"/></radialGradient>`,
			kd, kd, kd, kd)
	}
	labels.WriteString(`<g class="sky-labels" id="sky-labels">`)
	for i, lb := range placeRimLabels(scopes) {
		cls := "sky-label"
		if lb.tight {
			cls += " tight"
		}
		fmt.Fprintf(&defs, `<path id="sky-lp-%d" d="%s"/>`, i, lb.path)
		fmt.Fprintf(&labels, `<text class="%s" data-w="%d"><textPath href="#sky-lp-%d" startOffset="50%%" text-anchor="middle">%s<tspan class="sky-count" dx="5">%d</tspan></textPath></text>`,
			cls, i, i, template.HTMLEscapeString(scopes[i].name()), scopes[i].n)
	}
	defs.WriteString(`</defs>`)
	labels.WriteString(`</g>`)

	// The grid: the field, the stale ring's warm tint and the never belt, the
	// ring lines, the rim, and the spokes between scopes.
	var grid strings.Builder
	grid.WriteString(`<g class="sky-grid" id="sky-grid">`)
	fmt.Fprintf(&grid, `<circle class="sky-field" cx="%g" cy="%g" r="%g"/>`, skyC, skyC, skyRim)
	for _, band := range []skyBand{bandStale, bandNever} {
		ri, ro := radii[band], radii[band+1]
		fmt.Fprintf(&grid, `<circle class="sky-tint" data-b="%d" cx="%g" cy="%g" r="%s" stroke-width="%s"/>`,
			band, skyC, skyC, f1((ri+ro)/2), f1(ro-ri))
	}
	for band := bandDay; band < bandNever; band++ {
		fmt.Fprintf(&grid, `<circle class="sky-ring" data-b="%d" cx="%g" cy="%g" r="%s"/>`, band, skyC, skyC, f1(radii[band+1]))
	}
	fmt.Fprintf(&grid, `<circle class="sky-rim" cx="%g" cy="%g" r="%g"/>`, skyC, skyC, skyRim)
	// A spoke runs core to rim where it bounds a wedge wide enough to read as
	// a territory; between two slivers it shrinks to a tick on the rim, so a
	// run of small scopes reads as a graduated edge rather than a dense fan.
	var spokes strings.Builder
	edge := func(a float64, wide bool) {
		from := skyCore + 3
		if !wide {
			from = skyRim - 9
		}
		x0, y0 := polar(from, a)
		x1, y1 := polar(skyRim, a)
		fmt.Fprintf(&spokes, "M%s %sL%s %s", f1(x0), f1(y0), f1(x1), f1(y1))
	}
	const wideWedge = 6 * degree
	for i, sc := range scopes {
		prevWide := i > 0 && scopes[i-1].a1-scopes[i-1].a0 >= wideWedge
		edge(sc.a0, prevWide || sc.a1-sc.a0 >= wideWedge)
	}
	last := scopes[len(scopes)-1] // never empty: buildSky returns nil first
	edge(last.a1, last.a1-last.a0 >= wideWedge)
	fmt.Fprintf(&grid, `<path class="sky-spokes" d="%s"/>`, spokes.String())
	grid.WriteString(`</g>`)

	// Wedges: invisible until hovered or focused; the page script lights them.
	var wedges strings.Builder
	wedges.WriteString(`<g class="sky-wedges" id="sky-wedges">`)
	for i, sc := range scopes {
		fmt.Fprintf(&wedges, `<path id="sky-w-%d" class="sky-wedge" data-w="%d" data-p="%s" data-a0="%.4f" data-a1="%.4f" d="%s"/>`,
			i, i, template.HTMLEscapeString(sc.slug), sc.a0, sc.a1, sectorPath(skyCore, skyRim, sc.a0, sc.a1))
	}
	wedges.WriteString(`</g>`)

	// Ring ticks in the gutter; the rim labels follow them.
	var ticks strings.Builder
	ticks.WriteString(`<g class="sky-ticks" id="sky-ticks">`)
	for band := bandDay; band < bandStale; band++ {
		fmt.Fprintf(&ticks, `<text class="sky-tick" data-b="%d" x="%g" y="%s">%s</text>`, band, skyC, f1(skyC-radii[band+1]), band.tick())
	}
	fmt.Fprintf(&ticks, `<text class="sky-tick" data-b="%d" x="%g" y="%s">never</text>`, bandNever, skyC, f1(skyC-(radii[bandNever]+skyRim)/2))
	ticks.WriteString(`</g>`)

	// The core: what reached an agent in the last day.
	coreG := fmt.Sprintf(`<g class="sky-core" id="sky-core" data-sky-core><circle class="sky-core-glow" cx="%g" cy="%g" r="%g"/><circle class="sky-core-ring" cx="%g" cy="%g" r="%g"/>`+
		`<text class="sky-core-n" x="%g" y="%g">%d</text><text class="sky-core-l" x="%g" y="%g">last 24h</text></g>`,
		skyC, skyC, skyCore*1.9, skyC, skyC, skyCore-6, skyC, skyC+3, bandN[bandDay], skyC, skyC+16)

	c.frame = []skyPiece{
		newSkyPiece("defs", "sky-defs", defs.String()),
		newSkyPiece("g", "sky-grid", grid.String()),
		newSkyPiece("g", "sky-wedges", wedges.String()),
		newSkyPiece("g", "sky-ticks", ticks.String()),
		newSkyPiece("g", "sky-labels", labels.String()),
		newSkyPiece("g", "sky-core", coreG),
	}

	// The stars.
	var stars []*skyStar
	for _, sc := range scopes {
		stars = append(stars, sc.stars...)
	}
	slices.SortFunc(stars, func(a, c *skyStar) int { return cmp.Compare(a.ID, c.ID) })
	c.stars = make([]skyPiece, 0, len(stars))
	var star strings.Builder
	for _, s := range stars {
		star.Reset()
		writeStar(&star, s)
		c.stars = append(c.stars, newSkyPiece("g", "s-"+s.ID, star.String()))
	}
	return c
}

// writeStar emits one star: a group carrying the position and the facts the
// page script reads (tooltip, search, filters), around the dot and its marks.
// Position rides a CSS transform so a moved star glides.
func writeStar(b *strings.Builder, s *skyStar) {
	u := math.Max(0, math.Min(s.Utility, 1))
	r := 1.5 + 2.9*math.Sqrt(u)
	cls := "st"
	if u >= skyHaloAt {
		cls += " hi"
	}
	if s.Band == bandDay {
		cls += " warm"
	}
	if s.Fav {
		cls += " fav"
	}
	if s.New {
		cls += " new"
	}
	x, y := f1(s.x), f1(s.y)
	fmt.Fprintf(b, `<g id="s-%s" class="%s" style="transform:translate(%spx,%spx)`, template.HTMLEscapeString(s.ID), cls, x, y)
	if s.Band == bandDay {
		fmt.Fprintf(b, `;--d:-%.1fs`, float64(skyHash(s.ID+"t")%40)/10)
	}
	fmt.Fprintf(b, `" data-x="%s" data-y="%s" data-k="%s" data-b="%d" data-p="%s" data-n="%s"`,
		x, y, template.HTMLEscapeString(s.Kind), s.Band, template.HTMLEscapeString(s.Project), template.HTMLEscapeString(s.Name))
	if s.Desc != "" {
		fmt.Fprintf(b, ` data-d="%s"`, template.HTMLEscapeString(s.Desc))
	}
	if len(s.Tags) > 0 {
		fmt.Fprintf(b, ` data-g="%s"`, template.HTMLEscapeString(strings.Join(s.Tags, " ")))
	}
	if s.Surfaced != nil && !s.Surfaced.IsZero() {
		fmt.Fprintf(b, ` data-t="%d"`, s.Surfaced.Unix())
	}
	if !s.Created.IsZero() {
		fmt.Fprintf(b, ` data-c="%d"`, s.Created.Unix())
	}
	if s.Injects > 0 {
		fmt.Fprintf(b, ` data-i="%d"`, s.Injects)
	}
	if s.Reads > 0 {
		fmt.Fprintf(b, ` data-r="%d"`, s.Reads)
	}
	if s.Stale {
		b.WriteString(` data-s="1"`)
	}
	b.WriteString(`>`)
	if u >= skyHaloAt {
		fmt.Fprintf(b, `<circle class="halo" r="%s" style="fill:url(#sky-h-%s)"/>`, f1(r*3.6), template.HTMLEscapeString(skyKindKey(s.Kind)))
	}
	fmt.Fprintf(b, `<circle class="dot" r="%.2f"/>`, r)
	if s.Fav {
		fmt.Fprintf(b, `<circle class="ring" r="%.2f"/>`, r+2.6)
	}
	if s.New {
		a := r + 4
		fmt.Fprintf(b, `<path class="spk" d="M-%.1f 0H%.1fM0 -%.1fV%.1f"/>`, a, a, a, a)
	}
	b.WriteString(`</g>`)
}

// rimLabel is a scope's name on the rim: the arc it is set on, and whether it
// is tight -- hidden until its wedge is lit, because it would collide.
type rimLabel struct {
	path   string
	tight  bool
	lo, hi float64 // the stretch of rim the name claims, radians
}

// placeRimLabels sets the scope names just outside the rim. Largest scope
// first, each name is centred on its wedge and claims the stretch of rim it
// needs, which may run past a narrow wedge into its neighbours' rim; a name
// that would overlap one already placed, or run into the ring-label gutter, is
// tight. On the bottom half the arc runs counter-clockwise so the name reads
// left to right, a line further out because its glyphs now hang inwards. Every
// arc is long enough for its name, so a tight label can still be shown alone.
func placeRimLabels(scopes []*skyScope) []rimLabel {
	out := make([]rimLabel, len(scopes))
	type claim struct{ lo, hi float64 }
	var claims []claim
	for i, sc := range scopes { // largest first
		need := float64(len([]rune(sc.name())))*skyGlyph + float64(len(strconv.Itoa(sc.n)))*skyDigit + 5
		mid := (sc.a0 + sc.a1) / 2
		bottom := math.Cos(mid) < 0
		r := skyLabelIn
		if bottom {
			r = skyLabelOut
		}
		half := need / r / 2
		lo, hi := mid-half-degree, mid+half+degree
		tight := lo < skyGutter/2 || hi > 2*math.Pi-skyGutter/2
		for _, c := range claims {
			if lo < c.hi && hi > c.lo {
				tight = true
				break
			}
		}
		if !tight {
			claims = append(claims, claim{lo, hi})
		}

		span := math.Max(sc.a1-sc.a0-2*degree, (need+14)/r) / 2
		from, to, sweep := mid-span, mid+span, 1
		if bottom {
			from, to, sweep = to, from, 0
		}
		large := 0
		if 2*span > math.Pi {
			large = 1
		}
		x0, y0 := polar(r, from)
		x1, y1 := polar(r, to)
		out[i] = rimLabel{
			path:  fmt.Sprintf("M%s %sA%s %s 0 %d %d %s %s", f1(x0), f1(y0), f1(r), f1(r), large, sweep, f1(x1), f1(y1)),
			tight: tight,
			lo:    lo,
			hi:    hi,
		}
	}
	return out
}

// sectorPath is the annular sector between radii r0 < r1 and angles a0 < a1.
func sectorPath(r0, r1, a0, a1 float64) string {
	large := 0
	if a1-a0 > math.Pi {
		large = 1
	}
	x0, y0 := polar(r1, a0)
	x1, y1 := polar(r1, a1)
	x2, y2 := polar(r0, a1)
	x3, y3 := polar(r0, a0)
	return fmt.Sprintf("M%s %sA%s %s 0 %d 1 %s %sL%s %sA%s %s 0 %d 0 %s %sZ",
		f1(x0), f1(y0), f1(r1), f1(r1), large, f1(x1), f1(y1),
		f1(x2), f1(y2), f1(r0), f1(r0), large, f1(x3), f1(y3))
}

// polar maps (radius, angle clockwise from 12 o'clock) to viewBox coordinates.
func polar(r, a float64) (x, y float64) {
	return skyC + r*math.Sin(a), skyC - r*math.Cos(a)
}

// skyKinds is the legend: kinds by count, largest first.
func skyKinds(counts map[string]int) []kindCount {
	out := make([]kindCount, 0, len(counts))
	for k, n := range counts {
		out = append(out, kindCount{Kind: k, N: n})
	}
	slices.SortFunc(out, func(a, b kindCount) int {
		if c := cmp.Compare(b.N, a.N); c != 0 {
			return c
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	return out
}

// skyKindKey names a kind's halo gradient; an unknown kind borrows the
// refuted grey rather than referencing a gradient that does not exist.
func skyKindKey(kind string) string {
	if slices.Contains(core.MemoryKinds, core.MemoryKind(kind)) {
		return kind
	}
	return string(core.KindRefuted)
}

// skyHash is a stable 32-bit hash for slot and twinkle placement.
func skyHash(key string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(key)) //nolint:errcheck // hash.Hash.Write never returns an error (documented contract)
	return h.Sum32()
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func pct(share float64) string { return strconv.FormatFloat(100*share, 'f', 1, 64) }
