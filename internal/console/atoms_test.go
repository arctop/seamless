package console

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPointDelta_DirectionIsNotGoodness(t *testing.T) {
	tests := []struct {
		name             string
		now, prior       int
		has              bool
		good             goodness
		wantShow         bool
		wantText, wantOK string
		wantArrow        string
	}{
		{name: "reach rose", now: 61, prior: 57, has: true, good: riseGood, wantShow: true, wantText: "4pt", wantOK: "yes", wantArrow: "up"},
		{name: "reach fell", now: 54, prior: 57, has: true, good: riseGood, wantShow: true, wantText: "3pt", wantOK: "no", wantArrow: "down"},
		{name: "misses rose", now: 40, prior: 30, has: true, good: riseBad, wantShow: true, wantText: "10pt", wantOK: "no", wantArrow: "up"},
		{name: "misses fell", now: 20, prior: 30, has: true, good: riseBad, wantShow: true, wantText: "10pt", wantOK: "yes", wantArrow: "down"},
		{name: "flat", now: 30, prior: 30, has: true, good: riseGood, wantShow: true, wantText: "flat", wantOK: "neutral"},
		{name: "no prior window", now: 61, prior: 0, has: false, good: riseGood},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := pointDelta(tc.now, tc.prior, tc.has, tc.good)
			require.Equal(t, tc.wantShow, d.Show)
			if !tc.wantShow {
				require.Empty(t, string(deltaChip(d)), "an absent comparison must render nothing")
				return
			}
			require.Equal(t, tc.wantText, d.Text)
			require.Equal(t, tc.wantOK, d.Good)
			require.Equal(t, tc.wantArrow, d.Arrow)
			require.Contains(t, string(deltaChip(d)), `data-good="`+tc.wantOK+`"`)
		})
	}
}

func TestVolumeDelta_ZeroPriorIsNewNotInfinite(t *testing.T) {
	require.Equal(t, "new", volumeDelta(12, 0, true, noJudgment).Text)
	require.Equal(t, "flat", volumeDelta(0, 0, true, noJudgment).Text)
	d := volumeDelta(112, 100, true, noJudgment)
	require.Equal(t, "12%", d.Text)
	require.Equal(t, "up", d.Arrow)
	require.Equal(t, "neutral", d.Good, "a volume with no target carries no verdict")
	require.False(t, volumeDelta(12, 4, false, noJudgment).Show)
}

func TestBands_StateTheThresholdInWords(t *testing.T) {
	ok := floorBand(61, reachTargetPct, true)
	require.Equal(t, "ok", ok.Tone)
	require.Contains(t, ok.Text, "55%")
	warn := floorBand(41, reachTargetPct, true)
	require.Equal(t, "warn", warn.Tone)
	require.Contains(t, warn.Text, "below")
	require.Contains(t, string(bandLine(warn)), `class="band warn"`)

	require.Equal(t, "ok", ceilingBand(10, deadWeightCeilingPct, true).Tone)
	require.Equal(t, "warn", ceilingBand(90, deadWeightCeilingPct, true).Tone)

	require.False(t, floorBand(61, reachTargetPct, false).Show, "no denominator means no verdict")
	require.Empty(t, string(bandLine(floorBand(61, reachTargetPct, false))))
	require.Empty(t, string(bandLine(noteBand(""))), "an empty note renders nothing")
	require.Equal(t, `<span class="band">vs prior 7d</span>`, string(bandLine(noteBand("vs prior 7d"))))
}

func TestOfDelta_IsANeutralDenominator(t *testing.T) {
	d := ofDelta(212)
	require.True(t, d.Show)
	require.Equal(t, "of 212", d.Text)
	require.Equal(t, "neutral", d.Good)
	require.NotContains(t, string(deltaChip(d)), "uarr", "a denominator has no direction")
}

func TestEvtSev_OneMappingForEveryStream(t *testing.T) {
	tests := map[string]string{
		"agent.mishap":       sevDanger,
		"hook.error":         sevDanger,
		"update.failed":      sevDanger,
		"update.started":     sevSystem,
		"retrieval.injected": sevInject,
		"memory.written":     sevWrite,
		"note.written":       sevWrite,
		"trial.recorded":     sevWrite,
		"gardener.action":    sevSystem,
		"memory.archived":    sevSystem,
		"memory.read":        sevSystem,
		"session.started":    sevSystem,
		"tool.call":          sevSystem,
		"plan.approved":      sevSystem,
		"":                   sevSystem,
	}
	for kind, want := range tests {
		require.Equal(t, want, evtSev(kind), "kind %q", kind)
	}
	// A failed call is transport by kind but a failure to the eye.
	require.Equal(t, sevDanger, rowSev("tool.call", true))
	require.Equal(t, sevSystem, rowSev("tool.call", false))
	require.Equal(t, sevDanger, rowSev("agent.mishap", false))
}

// The client mirrors the Go mapping for live rows; if one moves without the
// other, the same event is coded differently on Interactions than everywhere
// else. Assert the JS carries every kind the Go switch names.
func TestInteractionsJSMirrorsSeverityMapping(t *testing.T) {
	js := string(interactionsJS)
	require.Contains(t, js, "function rowSev(kind, isError)")
	for _, kind := range []string{"agent.mishap", "hook.error", "update.failed", "retrieval.injected", "memory.written", "note.written", "trial.recorded"} {
		require.Contains(t, js, "'"+kind+"'", "interactions.js must classify %q", kind)
	}
	for _, sev := range []string{sevDanger, sevInject, sevWrite, sevSystem} {
		require.True(t, strings.Contains(js, "'"+sev+"'"), "interactions.js must emit %q", sev)
	}
}

// The reader chip warns past the staleness horizon it is given, whatever that
// is: the same line the Overview's card and the sky's stale ring draw.
func TestStReaderChip_WarnsPastTheHorizon(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -400)
	at := func(days int) *time.Time { v := now.AddDate(0, 0, -days); return &v }
	for _, tc := range []struct {
		name    string
		last    *time.Time
		created time.Time
		horizon int
		tone    string
	}{
		{"fresh at any horizon", at(3), old, 30, "ok"},
		{"inside a long horizon", at(60), old, 90, ""},
		{"past a short one", at(60), old, 30, "warn"},
		{"just inside", at(29), old, 30, ""},
		{"just past", at(31), old, 30, "warn"},
		{"never surfaced, older than the horizon", nil, now.AddDate(0, 0, -45), 30, "warn"},
		{"never surfaced, younger than the horizon", nil, now.AddDate(0, 0, -45), 90, ""},
	} {
		_, tone := surfacedStaleness(tc.last, tc.created, now, tc.horizon)
		require.Equal(t, tc.tone, tone, tc.name)
	}
}

// staleCutoff reaches back whole days from now, in UTC.
func TestStStaleCutoff(t *testing.T) {
	now := time.Date(2026, 11, 21, 3, 4, 5, 0, time.FixedZone("example", -8*3600))
	require.Equal(t, time.Date(2026, 8, 23, 11, 4, 5, 0, time.UTC), staleCutoff(now, 90))
}
