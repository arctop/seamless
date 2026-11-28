package console

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// UX 35: Settings > Features is one row per feature (icon, name, hint,
// data-kept chip, line, switch), and each switch saves at once.

// fsRow cuts one feature's row out of a rendered Features section.
func fsRow(t *testing.T, page, key string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<div class="feature-row[^"]*" data-feature="` + regexp.QuoteMeta(key) + `">.*?</label>\s*</div>`).FindString(page)
	require.NotEmpty(t, m, "the %s row", key)
	return m
}

// fsSection cuts the Features section out of a rendered Settings page.
func fsSection(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<section class="settings-zone settings-section" id="features">.*?</section>`).FindString(page)
	require.NotEmpty(t, m, "the Features section")
	return m
}

// The row's view comes from the registry: its line, its glyph, the data-kept
// chip only while there is data, and a hint that says what the feature is
// (Blurb), what turning it off hides (WhenOff, or WhenOffPlain at Basic) and,
// with data, what stays.
func TestFsFeatureRows_ViewFromTheRegistry(t *testing.T) {
	cards := featureCards(config.Features{}, navCounts{Labs: 2, Trials: 8, Held: 1})
	require.Len(t, cards, len(features.Registry()))
	for i, f := range features.Registry() {
		c := cards[i]
		require.Equal(t, f.Line, c.Line, f.Key)
		require.Equal(t, featureIcons[f.Key], c.Icon, f.Key)
		require.True(t, strings.HasPrefix(c.Hint, f.Blurb+" "+c.WhenOff), "%s: the hint is the blurb, then what turning it off hides", f.Key)
		require.True(t, strings.HasPrefix(c.HintPlain, f.Blurb+" "+c.WhenOffPlain), "%s: at Basic, without the tool names", f.Key)
		for _, tool := range f.Tools {
			require.Contains(t, c.Hint, tool, f.Key)
			require.NotContains(t, c.HintPlain, tool, "%s: Basic names no agent tool", f.Key)
		}
	}

	research, screening := cards[0], cards[1]
	require.Equal(t, string(features.Research), research.Key)
	require.Equal(t, "8 trials kept", research.Kept)
	require.True(t, strings.HasSuffix(research.Hint, " Data kept: 8 trials across 2 labs."), "the hint says what stays")
	require.Equal(t, string(features.Screening), screening.Key)
	require.Equal(t, "1 write held", screening.Kept)
	require.True(t, strings.HasSuffix(screening.Hint, "turning this off releases none of them."))
	for _, c := range cards {
		if c.Key != research.Key && c.Key != screening.Key {
			require.Empty(t, c.Kept, "%s keeps no data of its own", c.Key)
		}
	}

	// No data: no chip, and the hint stops at what turning it off hides.
	// The JSON line keeps its no-data sentence, as before.
	empty := featureCards(config.Features{}, navCounts{})
	require.Empty(t, empty[0].Kept)
	require.Equal(t, empty[0].Blurb+" "+empty[0].WhenOff, empty[0].Hint)
	require.Equal(t, "No trial data yet.", empty[0].DataKept)
	require.Empty(t, empty[1].Kept)
	require.Equal(t, "Nothing is held.", empty[1].DataKept)

	require.Equal(t, "2 writes held", featureKept(features.Screening, navCounts{Held: 2}))
	require.Equal(t, "1 trial kept", featureKept(features.Research, navCounts{Trials: 1, Labs: 1}))
	require.Empty(t, featureKept(features.Diary, navCounts{Trials: 3, Held: 3}))
	require.Equal(t, "a b", featureHint("a", "b", "", "c"), "no data-kept sentence without data")
	require.Equal(t, "a b c", featureHint("a", "b", "k", "c"))
}

// Every registered feature has a glyph of its own, and the glyph exists.
func TestFsFeatureRows_EveryFeatureHasAnIcon(t *testing.T) {
	seen := map[string]features.Key{}
	for _, f := range features.Registry() {
		name, ok := featureIcons[f.Key]
		require.True(t, ok, "%s needs a row glyph in featureIcons", f.Key)
		_, ok = lucidePaths[name]
		require.True(t, ok, "%s: glyph %q is not in lucidePaths", f.Key, name)
		owner, dup := seen[name]
		require.False(t, dup, "%s and %s share the glyph %q", owner, f.Key, name)
		seen[name] = f.Key
	}
	require.Len(t, featureIcons, len(features.Registry()), "featureIcons names only registered features")
	_, ok := lucidePaths[featureIconDefault]
	require.True(t, ok)
	require.Equal(t, featureIconDefault, featureIcon("no-such-feature"))
}

// The section draws one row per feature: no Enabled/Disabled badge, no
// "Available in the console and to agents", no Save bar. Its one form saves
// at once (data-autosave) and holds every switch, so a change posts them all
// to POST /console/settings/features, which rebuilds the set from checkbox
// presence.
func TestFsFeaturesSection_RowsThatSaveAtOnce(t *testing.T) {
	_, mux, _ := newGatedConsole(t)
	page := getHTMLBody(t, mux, "/console/settings?s=features")
	sec := fsSection(t, page)

	for _, gone := range []string{"Available in the console and to agents", "feature-state-pill", ">Enabled<", ">Disabled<",
		"settings-savebar", "data-form-discard", `id="features-state"`, "feature-card", "<small>"} {
		require.NotContains(t, sec, gone)
	}

	forms := regexp.MustCompile(`<form [^>]*>`).FindAllString(sec, -1)
	require.Equal(t, []string{`<form class="feature-rows" method="post" action="/console/settings/features" data-features-form data-autosave data-autosave-note="Agents see it from their next session.">`},
		forms, "one form, saving at once; the reset form is drawn only with an override")
	form := sec[strings.Index(sec, "<form "):strings.Index(sec, "</form>")]
	require.Len(t, regexp.MustCompile(`<div class="feature-row(?: is-on)?" data-feature="`).FindAllString(sec, -1), len(features.Registry()),
		"one row per feature")
	require.Equal(t, len(features.Registry()), strings.Count(form, `type="checkbox" role="switch"`), "every switch is in the one form")

	for _, f := range features.Registry() {
		row := fsRow(t, sec, string(f.Key))
		require.Contains(t, row, string(icon(featureIcons[f.Key])), "%s: its glyph", f.Key)
		require.Contains(t, row, `<strong class="feature-row-name">`+template.HTMLEscapeString(f.Label)+`</strong>`)
		require.Contains(t, row, `<p class="feature-row-line" id="feature-line-`+string(f.Key)+`">`+template.HTMLEscapeString(f.Line)+`</p>`)
		require.Contains(t, row, `aria-label="About `+template.HTMLEscapeString(f.Label)+`"`, "%s: the hint names the feature", f.Key)
		require.Contains(t, row, template.HTMLEscapeString(f.Blurb), "%s: the blurb lives in the hint", f.Key)
		require.Contains(t, row, `<input type="checkbox" role="switch" name="`+featureField(f.Key)+`" value="1" aria-label="`+
			template.HTMLEscapeString(f.Label)+`" aria-describedby="feature-line-`+string(f.Key)+`">`,
			"%s: off, and named for the toast (Diary is off.)", f.Key)
	}
	require.Contains(t, fsRow(t, sec, "research"), `<span class="badge compact quiet feature-row-kept">1 trial kept</span>`)
	require.NotContains(t, fsRow(t, sec, "diary"), "feature-row-kept", "no chip without data")
	require.Contains(t, form, `<div class="feature-rows-save"><button class="btn small primary" type="submit">`,
		"without script the form still posts (console.css hides it under .js)")

	// A change posts the whole form: here, the Diary switched on with the
	// rest as they stand (all off). The route reads every switch.
	rr := postForm(mux, "/console/settings/features", url.Values{"feature_diary": {"1"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "/console/settings?s=features&notice=")
	require.True(t, strings.HasSuffix(rr.Header().Get("Location"), "#features"), "back to the section, in place")
	sec = fsSection(t, getHTMLBody(t, mux, "/console/settings?s=features"))
	require.Contains(t, fsRow(t, sec, "diary"), `aria-describedby="feature-line-diary" checked>`)
	require.Contains(t, fsRow(t, sec, "diary"), `class="feature-row is-on"`)
	require.NotContains(t, fsRow(t, sec, "research"), " checked>")
}

// The hint holds what turning a feature off hides: with the agent tool names
// at Standard and above, without them at Basic (the surface registered as
// "the agent tool names"). The data-kept chip shows at every level.
func TestFsFeaturesSection_HintFollowsTheLevel(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	seedTrial(t, db, "cold-lab", "boot loop trial", "fail", "demo", "", nil, 1)
	research, ok := features.Get(features.Research)
	require.True(t, ok)

	row := fsRow(t, getHTMLBody(t, mux, "/console/settings?s=features"), "research")
	require.Contains(t, row, template.HTMLEscapeString(featureHides(research, false, false)))
	require.NotContains(t, row, "lab_open")
	require.Contains(t, row, "1 trial kept")

	setLevel(t, db, "standard")
	row = fsRow(t, getHTMLBody(t, mux, "/console/settings?s=features"), "research")
	require.Contains(t, row, template.HTMLEscapeString(featureHides(research, true, false)))
	require.Contains(t, row, "(lab_open, trial_record, trial_query)")
	require.Contains(t, row, "1 trial kept")
}

// A row is on when its feature is, whoever turned it on: the file/env base
// or a stored override.
func TestFsFeaturesSection_SwitchFollowsTheResolvedState(t *testing.T) {
	db, mux := newConsoleFeatures(t, config.Features{Diary: true})
	sec := fsSection(t, getHTMLBody(t, mux, "/console/settings?s=features"))
	require.Contains(t, fsRow(t, sec, "diary"), " checked>", "on in the config file")
	require.NotContains(t, fsRow(t, sec, "research"), " checked>")

	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Research: true}))
	sec = fsSection(t, getHTMLBody(t, mux, "/console/settings?s=features"))
	require.Contains(t, fsRow(t, sec, "research"), " checked>", "the stored override wins")
	require.NotContains(t, fsRow(t, sec, "diary"), " checked>")
}

// The toast says when agents see the change: the form's data-autosave-note,
// which shell.js adds after the words it names the change with.
func TestFsFeaturesSection_ToastSaysWhenAgentsSeeIt(t *testing.T) {
	js := string(shellJS)
	require.Contains(t, js, "var noted = el.closest('[data-autosave-note]');",
		"shell.js reads the note the features form carries (an edits.json entry of UX 35)")
	require.Contains(t, js, "if (control.type === 'checkbox') return name + ' is ' + (values.indexOf(control.value) !== -1 ? 'on' : 'off') + '.' + note;")
}
