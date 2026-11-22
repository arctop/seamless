package console

// Shared display atoms: the judged-metric pair (delta chip + judgment band) every
// headline number carries, and the severity class every event stream row is
// coded by. Both live here rather than in the pages that use them because their
// whole point is that three screens agree -- a metric judged one way on Overview
// and another way on Retrieval is worse than an unjudged number, and an error row
// that looks different on the session trace than in the live feed is not findable
// at a glance.

import (
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
)

// Judgment thresholds. These are the console's stated targets, not tunables:
// they appear verbatim in the band line under each metric ("at or above 55%
// target"), so a reader can see what the number is being judged against. The
// staleness horizon is the one threshold that is a setting: it is the
// gardener's (Service.staleDays), and every label prints the configured value.
const (
	reachTargetPct       = 55 // memory reach: share of active memories that surfaced
	continuityTargetPct  = 80 // knowledge continuity: share of sessions that retained knowledge
	demandTargetPct      = 50 // demand rate: share of briefing-surfaced memories agents also pulled
	staleSurfacedOKDays  = 7  // "fresh": surfaced within this many days
	deadWeightCeilingPct = 35 // waste share: injected tokens spent on undemanded memories
	missCeilingPct       = 40 // recall miss rate
)

// goodness says which direction of movement is good for a metric -- the thing a
// bare arrow cannot say. A rising error count and a rising reach rate both point
// up; only one of them is good news, and the chip has to color them differently.
type goodness int

const (
	riseGood   goodness = iota // higher is better: reach, continuity, demand
	riseBad                    // higher is worse: misses, waste, dead weight
	noJudgment                 // a volume with no target: movement is information, not a verdict
)

// delta is the rendered comparison chip for a headline metric. A zero delta
// renders nothing: a page with no prior window shows the number alone rather
// than a fabricated zero.
type delta struct {
	Show  bool
	Arrow string // "up" | "down" | "" (flat, or a plain denominator chip)
	Text  string // "4pt", "12%", "of 212"
	Good  string // "yes" | "no" | "neutral" -- drives .delta[data-good]
}

// pointDelta compares two percentages in percentage points. has is the caller's
// "there is a prior window at all" answer -- false yields an absent chip.
func pointDelta(now, prior int, has bool, g goodness) delta {
	if !has {
		return delta{}
	}
	diff := now - prior
	return delta{Show: true, Arrow: arrowFor(diff), Text: magnitude(diff, "pt"), Good: judge(diff, g)}
}

// volumeDelta compares two counts as a relative change. A zero prior cannot
// produce a percentage, so a window that went from nothing to something reads
// "new" rather than an infinite rise.
func volumeDelta(now, prior int, has bool, g goodness) delta {
	if !has {
		return delta{}
	}
	if prior == 0 {
		if now == 0 {
			return delta{Show: true, Text: "flat", Good: "neutral"}
		}
		return delta{Show: true, Text: "new", Good: judge(1, g)}
	}
	pct := int(float64(now-prior)/float64(prior)*100 + copySign(0.5, float64(now-prior)))
	return delta{Show: true, Arrow: arrowFor(pct), Text: magnitude(pct, "%"), Good: judge(pct, g)}
}

// ofDelta is the neutral denominator chip ("of 212") a count carries when it has
// no target to be judged against but does have a whole it is part of.
func ofDelta(total int) delta {
	return delta{Show: true, Text: "of " + strconv.Itoa(total), Good: "neutral"}
}

func arrowFor(diff int) string {
	switch {
	case diff > 0:
		return "up"
	case diff < 0:
		return "down"
	default:
		return ""
	}
}

func magnitude(diff int, unit string) string {
	if diff == 0 {
		return "flat"
	}
	if diff < 0 {
		diff = -diff
	}
	return strconv.Itoa(diff) + unit
}

func judge(diff int, g goodness) string {
	if diff == 0 || g == noJudgment {
		return "neutral"
	}
	rose := diff > 0
	if g == riseBad {
		rose = !rose
	}
	if rose {
		return "yes"
	}
	return "no"
}

func copySign(mag, sign float64) float64 {
	if sign < 0 {
		return -mag
	}
	return mag
}

// band is the one-line judgment under a metric: what the number is measured
// against, in words. An absent band means the metric has no defined target --
// which is a legitimate answer, and better than inventing one.
type band struct {
	Show bool
	Tone string // "ok" | "warn" | "" (neutral context, no verdict)
	Text string
}

// floorBand judges a value that should stay AT OR ABOVE target.
func floorBand(value, target int, has bool) band {
	if !has {
		return band{}
	}
	t := strconv.Itoa(target)
	if value >= target {
		return band{Show: true, Tone: "ok", Text: "at or above " + t + "% target"}
	}
	return band{Show: true, Tone: "warn", Text: "below " + t + "% target"}
}

// ceilingBand judges a value that should stay AT OR BELOW a ceiling -- the
// inverted metrics (waste, misses) where less is the good news.
func ceilingBand(value, ceiling int, has bool) band {
	if !has {
		return band{}
	}
	c := strconv.Itoa(ceiling)
	if value <= ceiling {
		return band{Show: true, Tone: "ok", Text: "within " + c + "% ceiling"}
	}
	return band{Show: true, Tone: "warn", Text: "above " + c + "% ceiling"}
}

// noteBand states context without a verdict ("vs prior 30d"), for the volume
// metrics that have no target.
func noteBand(text string) band {
	if text == "" {
		return band{}
	}
	return band{Show: true, Text: text}
}

// deltaChip renders the .delta atom. Direction and goodness are separate
// attributes on purpose: the arrow says which way the number moved, data-good
// says whether that is good, and only the second one is colored.
func deltaChip(d delta) template.HTML {
	if !d.Show {
		return ""
	}
	good := d.Good
	if good == "" {
		good = "neutral"
	}
	var b strings.Builder
	b.WriteString(`<span class="delta" data-good="` + template.HTMLEscapeString(good) + `">`)
	switch d.Arrow {
	case "up":
		b.WriteString(`<span aria-hidden="true">&uarr;</span>`)
	case "down":
		b.WriteString(`<span aria-hidden="true">&darr;</span>`)
	}
	b.WriteString(template.HTMLEscapeString(d.Text))
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// bandLine renders the .band atom -- the threshold a metric is judged against,
// stated in words. An unset band renders nothing.
func bandLine(bd band) template.HTML {
	if !bd.Show || bd.Text == "" {
		return ""
	}
	cls := "band"
	if bd.Tone != "" {
		cls += " " + bd.Tone
	}
	return template.HTML(`<span class="` + template.HTMLEscapeString(cls) + `">` +
		template.HTMLEscapeString(bd.Text) + `</span>`)
}

// ---------------------------------------------------------------------------
// Event severity -- the one mapping every stream row is coded by
// ---------------------------------------------------------------------------

// Severity classes. Four, deliberately: a stream row's left edge is scanned, not
// read, and more than four colors stops being a scan.
const (
	sevDanger = "danger" // something went wrong: agent-reported mishaps, hook failures, failed updates
	sevInject = "inject" // context reached an agent
	sevWrite  = "write"  // durable knowledge was created
	sevSystem = "system" // everything else: lifecycle, curation, transport
)

// evtSev maps an event kind to its stream severity class. It is the single
// source for the Interactions feed, the session trace, and the Overview ledger,
// so an error looks the same on all three; static/interactions.js mirrors it for
// the client-rendered live rows.
//
// Unlike evtTone (which colors a kind chip by domain), this answers only "how
// urgently should the eye stop here", which is why gardener and lifecycle events
// -- visually distinct under evtTone -- collapse into one neutral class.
func evtSev(kind string) string {
	switch kind {
	case "agent.mishap", "hook.error", "update.failed":
		return sevDanger
	case "retrieval.injected":
		return sevInject
	case "memory.written", "note.written", "trial.recorded":
		return sevWrite
	default:
		return sevSystem
	}
}

// rowSev is evtSev with a stream row's error flag folded in: a failed tool call
// is transport by kind but a failure to the eye, and the left edge answers the
// second question. Rows that carry an is_error signal use this; the Overview
// ledger, which has none, uses evtSev directly.
func rowSev(kind string, isError bool) string {
	if isError {
		return sevDanger
	}
	return evtSev(kind)
}

// ---------------------------------------------------------------------------
// Memory reader signals
// ---------------------------------------------------------------------------

// staleDays is the console's one staleness horizon, in days: the configured
// gardener.staleness_days. An active memory with no update, injection or read
// for that long is stale, and the gardener's staleness pass proposes archiving
// it. Retrieval's quiet-knowledge list and the Overview's "going stale" count
// (one set: store.StaleMemories and store.CountStaleMemories), the knowledge
// sky's stale ring and the memory reader's chip all judge by this one number
// and print it, so no screen keeps a second opinion. A non-positive value falls
// back to the default, as the gardener's own setting does; config validation
// refuses one in a running daemon, so only a test's bare Config reaches it.
func (s *Service) staleDays() int {
	if d := s.cfg.GardenerCfg.StalenessDays; d > 0 {
		return d
	}
	return config.Defaults().Gardener.StalenessDays
}

// staleCutoff is the instant a horizon of days reaches back to from now:
// activity before it does not keep a memory fresh.
func staleCutoff(now time.Time, days int) time.Time {
	return now.UTC().AddDate(0, 0, -days)
}

// surfacedStaleness renders how long ago a memory last entered an agent context,
// with the tone its chip carries. The warn line is the staleness horizon
// (staleDays, passed in), so the chip, the sky's stale ring and the Overview's
// card draw the same line.
//
// A memory that has never surfaced is only warned about once it has had the
// full window to: something written this morning has not gone quiet, it has not
// had its turn yet, and coloring it warn would make every burst of writing look
// like decay.
func surfacedStaleness(lastInjected *time.Time, created, now time.Time, staleDays int) (age, tone string) {
	horizon := time.Duration(staleDays) * 24 * time.Hour
	if lastInjected == nil || lastInjected.IsZero() {
		if !created.IsZero() && now.Sub(created) > horizon {
			return "never", "warn"
		}
		return "never", ""
	}
	since := now.Sub(*lastInjected)
	switch {
	case since <= staleSurfacedOKDays*24*time.Hour:
		tone = "ok"
	case since > horizon:
		tone = "warn"
	}
	return ago(*lastInjected), tone
}

// bodyEchoesDescription reports that a memory's body says nothing its
// description does not: it is absent, or it is the same sentence again.
//
// Echoing happens for real -- an agent capturing mid-session often writes the
// one line it has twice -- and rendering it as a document makes the reader
// hunt for a difference that is not there. Comparison is normalized (case,
// whitespace, markdown emphasis, trailing punctuation) so the near-misses that
// echoing produces in practice are caught too.
func bodyEchoesDescription(body, description string) bool {
	nb := normalizeProse(body)
	if nb == "" {
		return true
	}
	nd := normalizeProse(description)
	return nd != "" && nb == nd
}

// normalizeProse reduces a fragment to comparable words: lowercase, no markdown
// emphasis or heading marks, single-spaced, no trailing sentence punctuation.
func normalizeProse(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '*', '_', '`', '#', '>':
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, " .!:;-")
}
