package console

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"html/template"
	"regexp"
	"strings"
	"time"
)

// The shared parts of Settings and the lock screens (plan:console-ux-pass,
// UX 33): the (i) hint, the time a lease or an unlock has left, and the
// source chip's model. Their markup partials are in templates/kit_shared.html,
// their behavior in static/shell.js, their look in console.css's "UX 33:
// Shared parts" section.

// hint renders an (i) button and its tooltip: a short explanation kept one
// hover, focus or tap away instead of a paragraph on the page. The tooltip is
// in the page, so a page without script carries it, and the button names it
// with aria-describedby, so a screen reader reads it with the button. CSS opens
// it on hover and on keyboard focus; shell.js pins it open on a tap and closes
// it on Esc, a tap outside, or another hint opening. The text is escaped.
func hint(text string) template.HTML { return hintMarkup("", text) }

// hintFor is hint for a named thing: the button reads "About <label>".
func hintFor(label, text string) template.HTML { return hintMarkup(label, text) }

func hintMarkup(label, text string) template.HTML {
	id := hintID(label, text)
	name := "About this"
	if label = strings.TrimSpace(label); label != "" {
		name = "About " + label
	}
	return template.HTML(`<span class="hint"><button type="button" class="hint-btn" aria-label="` +
		template.HTMLEscapeString(name) + `" aria-describedby="` + id + `">` + string(icon("info")) +
		`</button><span class="hint-pop" id="` + id + `" role="tooltip">` + template.HTMLEscapeString(text) +
		`</span></span>`)
}

// hintID names a hint's tooltip after its words, so a page draws the same id
// on every render (the live morph keeps a pinned hint pinned, and the defaults
// golden stays put). Two hints with the same words on one page would share it:
// uniqueHintIDs, run over every page render draws, numbers the later ones.
func hintID(label, text string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(label))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	return fmt.Sprintf("hint-%08x", h.Sum32())
}

// hintToken is a hint id as hintMarkup quotes it. Each hint quotes its id
// exactly twice, the button's aria-describedby first and the tooltip's id
// second; the escaped text can never carry a quote.
var hintToken = regexp.MustCompile(`"hint-[0-9a-f]{8}"`)

// uniqueHintIDs gives every hint on a rendered page its own id: the second
// hint with the same id becomes <id>-2 (its aria-describedby and its id
// together), the third <id>-3, and so on. A page without hints is returned
// as it is.
func uniqueHintIDs(page []byte) []byte {
	if !bytes.Contains(page, []byte(`"hint-`)) {
		return page
	}
	seen := make(map[string]int)
	return hintToken.ReplaceAllFunc(page, func(tok []byte) []byte {
		n := seen[string(tok)]
		seen[string(tok)] = n + 1
		if n < 2 {
			return tok
		}
		return fmt.Appendf(nil, `"%s-%d"`, tok[1:len(tok)-1], n/2+1)
	})
}

// leftWarn is when a time left turns warn: renewal warnings start 30 minutes
// before a lease ends (memory locked-projects-approval-design).
const leftWarn = 30 * time.Minute

// leftWords is a time left in words, and its badge tone: "2 h 41 m left" (ok),
// "28 min left" (warn from 30 minutes), "ended" (no tone). static/shell.js
// counts down with the same words (leftWords there); keep the two in step.
func leftWords(d time.Duration) (words, tone string) {
	if d <= 0 {
		return "ended", ""
	}
	tone = "ok"
	if d <= leftWarn {
		tone = "warn"
	}
	mins := int(d / time.Minute)
	switch {
	case mins < 1:
		return "under 1 min left", tone
	case mins < 60:
		return fmt.Sprintf("%d min left", mins), tone
	case mins < 24*60:
		if m := mins % 60; m > 0 {
			return fmt.Sprintf("%d h %d m left", mins/60, m), tone
		}
		return fmt.Sprintf("%d h left", mins/60), tone
	default:
		days, hours := mins/(24*60), mins%(24*60)/60
		if hours > 0 {
			return fmt.Sprintf("%d d %d h left", days, hours), tone
		}
		return fmt.Sprintf("%d d left", days), tone
	}
}

// timeLeft renders how long a lease, a grant or an unlock has left as a
// badge: a <time data-left> whose words the server writes ("2 h 41 m left",
// "ended"), so a page without script says them, and whose title is the end in
// UTC, which shell.js turns into this browser's clock (data-clock="title") and
// whose words it counts down every 30 seconds while the page is in sight. A
// nil or zero time is a lease with no end of its own: "until the session
// ends", which never counts.
func timeLeft(v any) template.HTML { return timeLeftAt(v, time.Now()) }

func timeLeftAt(v any, now time.Time) template.HTML {
	clockIcon := string(icon("clock"))
	t, ok := timeOf(v)
	if !ok {
		return template.HTML(`<span class="badge time-left">` + clockIcon + `<span>until the session ends</span></span>`)
	}
	words, tone := leftWords(t.Sub(now))
	class := "badge time-left"
	if tone != "" {
		class += " " + tone
	}
	return template.HTML(`<time class="` + class + `" datetime="` + t.UTC().Format(time.RFC3339) +
		`" data-left data-clock="title" title="` + ts(t) + `">` + clockIcon + `<span>` + words + `</span></time>`)
}

// srcChip is the source chip's model (templates/kit_shared.html, "source-chip"):
// where a section's values come from, said once in its head.
type srcChip struct {
	// Source is "file" (the config file, or the build's default), "override"
	// (set here: a console override is in force) or "locked" (the config file
	// decides, and the console cannot override it).
	Source string
	// Label is the chip's word for Source.
	Label string
	// Note is the precedence sentence, shown as the chip's hint.
	Note string
	// Reset is the id of the form that hands the values back to the config
	// file, drawn as the chip's Reset button; only an override has one.
	Reset string
	// Confirm, when set, is the data-confirm sentence Reset asks with.
	Confirm string
}

// sourceChipLabels are the chip's words, by source.
var sourceChipLabels = map[string]string{
	"file":     "Config file",
	"override": "Set here",
	"locked":   "Config file decides",
}

// sourceChip builds the source chip's model for {{template "source-chip"
// (sourceChip "override" .Note "features-reset")}}: the source, the precedence
// sentence, the reset form's id (empty when there is none), and optionally the
// sentence Reset confirms with. An unknown source is a template mistake and
// fails the render.
func sourceChip(source, note, reset string, confirm ...string) (srcChip, error) {
	label, ok := sourceChipLabels[source]
	if !ok {
		return srcChip{}, fmt.Errorf("sourceChip: unknown source %q (want file, override or locked)", source)
	}
	c := srcChip{Source: source, Label: label, Note: note}
	if source == "override" {
		c.Reset = reset
		if len(confirm) > 0 {
			c.Confirm = confirm[0]
		}
	}
	return c, nil
}
