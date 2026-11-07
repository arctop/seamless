package console

// Optional-feature gating for the console.
//
// The registry (internal/features) is the single source of truth for what an
// optional feature owns; this file is the console's half of it: resolving the
// effective state per request, gating the feature's routes, hiding its surfaces,
// and rendering the friendly page a gated route answers with.
//
// Two deliberate choices live here:
//
//   - Resolution is LIVE, not frozen at construction. The Settings save writes a
//     settings row, and the very next render must reflect it -- that is what
//     makes the nav entries appear and disappear on the existing form-POST morph
//     with no reload.
//   - Resolution is FAILURE-SOFT. A corrupt override row must never take the
//     console down: it logs and falls back to the file/env base, which is the
//     conservative answer (an optional feature stays as configured rather than
//     flipping on because a row failed to parse).
//
// A disabled route is NOT a 404. The console is owner-only, so the honest answer
// to "you asked for a screen you switched off" is to say so and offer the switch
// back -- see gate/featureOff below.

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// featuresConfig resolves the optional-feature config for this request -- the
// file/env base with the stored override row layered over it -- and reports
// whether such a row is in force (what the Settings reset button and precedence
// breadcrumb key off).
//
// It returns no error on purpose: failure-soft is the contract, so the decision
// is made once, here, rather than at every call site. A corrupt row is logged
// and degrades to the base config.
func (s *Service) featuresConfig(ctx context.Context) (config.Features, bool) {
	cfg, overridden, err := store.FeaturesConfig(ctx, s.db(ctx), s.cfg.Features)
	if err != nil {
		s.logger.Warn("console: features config", "error", err)
		return s.cfg.Features, false
	}
	return cfg, overridden
}

// effectiveFeatures is featuresConfig for the gates and the surface guards,
// which only need the resolved state.
func (s *Service) effectiveFeatures(ctx context.Context) config.Features {
	cfg, _ := s.featuresConfig(ctx)
	return cfg
}

// gate wraps a handler so it runs only while its feature is on. Composed into
// the route helpers in Register, so a gated route keeps the same security
// headers, auth, and form handling as every other route.
func (s *Service) gate(key features.Key, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if features.Enabled(s.effectiveFeatures(r.Context()), key) {
			next(w, r)
			return
		}
		s.featureOff(w, r, key)
	}
}

// featureOffData is the payload for the "this feature is off" page.
type featureOffData struct {
	Key   string `json:"feature"`
	Label string `json:"label"`
	Blurb string `json:"blurb,omitempty"`
	// Error is the same message the JSON branch returns, so a caller reading
	// either shape sees one sentence.
	Error string `json:"error"`
	// SettingsHref is where the owner turns the feature back on.
	SettingsHref string `json:"settingsHref"`
}

// featureOffMessage is the one sentence every disabled surface says, in the
// JSON body and on the page.
func featureOffMessage(key features.Key) string {
	return fmt.Sprintf("the %s feature is disabled", key)
}

// featureOff answers a request for a screen whose feature is switched off.
//
// A JSON caller (the seam CLI) gets 403 with the house {"error": ...} shape. A
// browser gets 200 and a short explanation: this is not an error, it is the
// console telling its only user what they already decided, plus the data
// reassurance and a link back to the switch. A ?peek=1 fetch gets a
// fragment-shaped notice instead, because the pane injects the response body
// verbatim and a layout-wrapped page there would nest the whole console.
func (s *Service) featureOff(w http.ResponseWriter, r *http.Request, key features.Key) {
	msg := featureOffMessage(key)
	if wantsJSON(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
		return
	}
	f, ok := features.Get(key)
	label := string(key)
	blurb := ""
	if ok {
		label, blurb = f.Label, f.Blurb
	}
	if r.URL.Query().Get("peek") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w,
			`<article class="peek-entity"><div class="peek-head"><span class="badge">off</span></div>`+
				`<h2 class="peek-title">%s is switched off</h2>`+
				`<p class="peek-desc">Nothing was deleted &mdash; the data is preserved and returns as soon as the feature is on.</p>`+
				`<p class="peek-note"><a href="/console/settings?s=features">Turn it back on in Settings</a></p></article>`,
			template.HTMLEscapeString(label))
		return
	}
	s.render(w, r, "feature_off", pageData{
		Title:  label,
		Active: "settings",
		Data: featureOffData{
			Key: string(key), Label: label, Blurb: blurb, Error: msg,
			SettingsHref: "/console/settings?s=features",
		},
	})
}

// ---------------------------------------------------------------------------
// Settings cards
// ---------------------------------------------------------------------------

// featureCard is one optional feature as the Settings zone renders it -- and, by
// the same struct, as `GET /console/settings?format=json` exposes it. Key and
// Enabled are the machine-readable contract (`seam doctor` computes the expected
// tool count from Tools and Enabled); the rest is owner-facing copy.
type featureCard struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Blurb   string   `json:"blurb"`
	Enabled bool     `json:"enabled"`
	Default bool     `json:"default"`
	Tools   []string `json:"tools"`
	// Field is the form field name the toggle submits under.
	Field string `json:"field"`
	// WhenOff spells out what switching this off hides, generated from the
	// registry rather than transcribed, so it cannot drift from what the gates
	// actually do.
	WhenOff string `json:"whenOff"`
	// WhenOffPlain is WhenOff without the agent tool names, for the Basic
	// Settings page: the same promise in words a non-technical owner can use.
	WhenOffPlain string `json:"-"`
	// DataKept is the live reassurance line ("Data kept: 12 trials across 3
	// labs"), empty when the console has no count to offer for this feature.
	DataKept string `json:"dataKept,omitempty"`
}

// featureFieldPrefix namespaces the toggle form fields, so the features form's
// inputs can never collide with another Settings form's field names.
const featureFieldPrefix = "feature_"

// featureField is the form field name for a feature's toggle.
func featureField(key features.Key) string { return featureFieldPrefix + string(key) }

// featureWhenOff generates the "what disappears" line from registry metadata:
// the in-page surfaces (Surfaces, verbatim), the screens (NavIDs), the search
// scope (a NavID that is also a search scope), and the agent tools by name.
// Everything it names is something a gate in this package or in internal/mcp
// actually enforces.
func featureWhenOff(f features.Feature) string {
	return featureHides(f, true)
}

// featureWhenOffPlain is featureWhenOff without the agent tools' names: it says
// agents lose the matching tools without spelling out their identifiers.
func featureWhenOffPlain(f features.Feature) string {
	return featureHides(f, false)
}

// featureHides builds the "what disappears" sentence, naming the agent tools
// when toolNames is set and counting them otherwise.
func featureHides(f features.Feature, toolNames bool) string {
	parts := slices.Clone(f.Surfaces)
	if len(f.NavIDs) > 0 {
		screens := make([]string, 0, len(f.NavIDs))
		for _, id := range f.NavIDs {
			screens = append(screens, titleWord(id))
		}
		noun := "screens"
		if len(screens) == 1 {
			noun = "screen"
		}
		parts = append(parts, "the "+strings.Join(screens, " & ")+" "+noun)
	}
	var scopes []string
	for _, id := range f.NavIDs {
		if slices.Contains(searchScopes, id) {
			scopes = append(scopes, id)
		}
	}
	if len(scopes) > 0 {
		noun := "search scopes"
		if len(scopes) == 1 {
			noun = "search scope"
		}
		parts = append(parts, "the "+strings.Join(scopes, " and ")+" "+noun)
	}
	if len(f.Tools) > 0 {
		tools := plural(len(f.Tools), "agent tool", "agent tools")
		if toolNames {
			tools += " (" + strings.Join(f.Tools, ", ") + ")"
		}
		parts = append(parts, tools)
	}
	if len(parts) == 0 {
		return "Hides nothing on its own."
	}
	return "Hides " + joinWithAnd(parts) + "."
}

// featureDataKept states what the feature's data amounts to right now -- the
// concrete half of "nothing is ever deleted". It is keyed by feature because
// only the console knows where a feature's rows live; an unknown key yields no
// line rather than an invented one.
func featureDataKept(key features.Key, n navCounts) string {
	switch key {
	case features.Research:
		if n.Trials == 0 {
			return "No trial data yet."
		}
		return fmt.Sprintf("Data kept: %s across %s.",
			plural(n.Trials, "trial", "trials"), plural(n.Labs, "lab", "labs"))
	case features.Screening:
		// Turning screening off never releases what it holds: the writes stay
		// out of the corpus until it is back on and the owner decides.
		if n.Held == 0 {
			return "Nothing is held."
		}
		return fmt.Sprintf("Data kept: %s waiting for review; turning this off releases none of them.",
			plural(n.Held, "held write", "held writes"))
	default:
		return ""
	}
}

// featureCards builds the Settings zone's cards from the registry, in registry
// order, so a newly registered feature appears with no template change. counts
// must be the RAW nav counts (not the feature-filtered ones the sidebar uses):
// the reassurance line has to report the data of a feature that is off.
func featureCards(cfg config.Features, counts navCounts) []featureCard {
	reg := features.Registry()
	cards := make([]featureCard, 0, len(reg))
	for _, f := range reg {
		cards = append(cards, featureCard{
			Key:          string(f.Key),
			Label:        f.Label,
			Blurb:        f.Blurb,
			Enabled:      f.Enabled(cfg),
			Default:      f.Default,
			Tools:        f.Tools,
			Field:        featureField(f.Key),
			WhenOff:      featureWhenOff(f),
			WhenOffPlain: featureWhenOffPlain(f),
			// The counts are the feature's own data, so they are reported
			// whether it is on or off -- that is the whole point of the line.
			DataKept: featureDataKept(f.Key, counts),
		})
	}
	return cards
}

// titleWord upper-cases the first rune of a nav id for display ("labs" ->
// "Labs"). Nav ids are lowercase ASCII slugs by construction.
func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// joinWithAnd renders a list as prose: "a", "a and b", "a, b, and c".
func joinWithAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}
