package console

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// The knowledge sky's side of the live protocol: what an Overview refresh
// sends a page that already holds a chart.
//
// The chart is most of the Overview's bytes -- every star carries its facts
// for the tooltip and the search -- while an event changes a handful of stars,
// or none. So the daemon keeps the fingerprints of the keyed pieces of the
// last few charts it drew (skyLedger), a page names the chart it holds (the
// panel's data-live-v, sent back as X-Seam-Sky), and the answer carries only
// what changed since: a keep stub for the whole panel when nothing did, else
// the frame layers that changed, the stars that changed or arrived, and a gone
// stub for each star that left. static/navigation.js applies it, and fetches
// the whole page instead when the page turns out not to hold that chart.

// skyHeader carries the version of the chart a page holds on a live or
// in-place fetch of the Overview. Only those fetches send it (heldSky).
const skyHeader = "X-Seam-Sky"

// skyLedgerSize is how many charts the ledger remembers. A page whose chart
// has aged out -- a tab left hidden through many refreshes -- gets the whole
// panel once and is current again.
const skyLedgerSize = 8

// skyClose ends the stars group and the chart. The effects layer between them
// (hover and selection reticles, beams, flares) belongs to the page script;
// the morph leaves it alone.
const skyClose = `</g><g class="sky-fx" data-live-skip></g></svg>`

// skyPiece is one keyed element of the chart -- a frame layer or a star --
// with the fingerprint of its markup. The id is what the morph matches it by.
type skyPiece struct {
	tag, id, html string
	sum           uint64
}

func newSkyPiece(tag, id, html string) skyPiece {
	return skyPiece{tag: tag, id: id, html: html, sum: skySum(html)}
}

// skySum fingerprints a piece of markup.
func skySum(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s)) //nolint:errcheck // hash.Hash.Write never returns an error (documented contract)
	return h.Sum64()
}

// skyChart is the chart as the live protocol sees it: the svg start tag, the
// frame layers in document order, and the stars in id order.
type skyChart struct {
	open  string
	frame []skyPiece
	stars []skyPiece
}

// full draws the whole chart.
func (c *skyChart) full() template.HTML {
	var b strings.Builder
	n := len(c.open) + len(skyClose) + 64
	for _, p := range c.frame {
		n += len(p.html)
	}
	for _, p := range c.stars {
		n += len(p.html)
	}
	b.Grow(n)
	b.WriteString(c.open)
	for _, p := range c.frame {
		b.WriteString(p.html)
	}
	b.WriteString(`<g class="sky-stars" id="sky-stars">`)
	for _, p := range c.stars {
		b.WriteString(p.html)
	}
	b.WriteString(skyClose)
	return template.HTML(b.String())
}

// delta draws the chart for a page that holds base: a frame layer that has
// not changed is a keep stub, and the stars group, marked data-live-patch,
// carries only the stars that changed or arrived, then a gone stub for each
// star that left. The svg start tag is always whole: its label carries the
// counts, and the morph updates its attributes in place.
func (c *skyChart) delta(base *skyMark) template.HTML {
	var b strings.Builder
	b.WriteString(c.open)
	for _, p := range c.frame {
		if sum, ok := base.frame[p.id]; ok && sum == p.sum {
			fmt.Fprintf(&b, `<%s id="%s" data-live-keep></%s>`, p.tag, template.HTMLEscapeString(p.id), p.tag)
			continue
		}
		b.WriteString(p.html)
	}
	b.WriteString(`<g class="sky-stars" id="sky-stars" data-live-patch>`)
	here := make(map[string]bool, len(c.stars))
	for _, p := range c.stars {
		here[p.id] = true
		if sum, ok := base.stars[p.id]; !ok || sum != p.sum {
			b.WriteString(p.html)
		}
	}
	var gone []string
	for id := range base.stars {
		if !here[id] {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	for _, id := range gone {
		fmt.Fprintf(&b, `<g id="%s" data-live-gone></g>`, template.HTMLEscapeString(id))
	}
	b.WriteString(skyClose)
	return template.HTML(b.String())
}

// mark fingerprints the chart for the ledger. readout and kinds stand in for
// the panel parts the template draws beside the chart (skyData's rows).
func (c *skyChart) mark(readout, kinds uint64) *skyMark {
	m := &skyMark{
		frame:   make(map[string]uint64, len(c.frame)),
		stars:   make(map[string]uint64, len(c.stars)),
		readout: readout,
		kinds:   kinds,
	}
	h := fnv.New64a()
	var word [8]byte
	add := func(id string, sum uint64) {
		h.Write([]byte(id)) //nolint:errcheck // hash.Hash.Write never returns an error (documented contract)
		binary.LittleEndian.PutUint64(word[:], sum)
		h.Write([]byte{0}) //nolint:errcheck // as above
		h.Write(word[:])   //nolint:errcheck // as above
	}
	add("svg", skySum(c.open))
	for _, p := range c.frame {
		m.frame[p.id] = p.sum
		add(p.id, p.sum)
	}
	for _, p := range c.stars {
		m.stars[p.id] = p.sum
		add(p.id, p.sum)
	}
	add("readout", readout)
	add("kinds", kinds)
	m.version = fmt.Sprintf("%016x", h.Sum64())
	return m
}

// skyMark is what the ledger keeps of one chart: its version and the
// fingerprint of every keyed piece, enough to tell what changed since. It is
// never modified once made, so readers share it without the ledger's lock.
type skyMark struct {
	version        string
	frame, stars   map[string]uint64
	readout, kinds uint64
}

// skyLedger remembers the last few charts the Overview drew. One ledger serves
// every open Overview: an owner's tabs mostly hold the same few versions.
type skyLedger struct {
	mu    sync.Mutex
	marks []*skyMark // oldest first
}

// answer settles what d's panel is for a page holding the chart named held
// ("" for none): the whole panel, a keep stub, or a delta against the held
// chart. It remembers d's chart either way, since the page receiving this
// answer holds it next.
func (l *skyLedger) answer(d *skyData, held string) {
	base := l.remember(d.mark, held)
	if base == nil {
		return
	}
	d.Base = base.version
	if base.version == d.Version {
		d.Keep = true
		return
	}
	d.SVG = d.chart.delta(base)
	d.KeepReadout = base.readout == d.mark.readout
	d.KeepKinds = base.kinds == d.mark.kinds
}

// remember records m as the newest chart and returns the remembered chart
// named held, or nil.
func (l *skyLedger) remember(m *skyMark, held string) *skyMark {
	l.mu.Lock()
	defer l.mu.Unlock()
	var base *skyMark
	if held != "" {
		for _, old := range l.marks {
			if old.version == held {
				base = old
			}
		}
	}
	l.marks = slices.DeleteFunc(l.marks, func(old *skyMark) bool { return old.version == m.version })
	l.marks = append(l.marks, m)
	if over := len(l.marks) - skyLedgerSize; over > 0 {
		l.marks = slices.Delete(l.marks, 0, over)
	}
	return base
}

// heldSky is the chart version a request says its page holds. Only the
// console's own live and in-place fetches carry one: a delta answer is
// meaningless to anything that has not got the page it patches.
func heldSky(r *http.Request) string {
	if !patchRequest(r) {
		return ""
	}
	return r.Header.Get(skyHeader)
}
