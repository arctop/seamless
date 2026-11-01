package features

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

func TestRegistry_ResearchEntry(t *testing.T) {
	f, ok := Get(Research)
	require.True(t, ok)
	require.Equal(t, Key("research"), f.Key)
	require.Equal(t, "Research labs & trials", f.Label)
	require.NotEmpty(t, f.Blurb)
	require.Equal(t, []string{"lab_open", "trial_record", "trial_query"}, f.Tools)
	require.Equal(t, []string{"labs", "trials"}, f.NavIDs)
	require.Equal(t, []string{"/console/labs", "/console/trials"}, f.RoutePrefixes)
	require.Equal(t, "seam-research", f.Skill)
	require.False(t, f.Default, "optional features ship off")
}

func TestRegistry_MomentumEntry(t *testing.T) {
	f, ok := Get(Momentum)
	require.True(t, ok)
	require.Equal(t, Key("momentum"), f.Key)
	require.Equal(t, "Momentum", f.Label)
	require.NotEmpty(t, f.Blurb)
	require.Empty(t, f.Tools, "momentum owns no MCP tools -- its surfaces live inside existing pages")
	require.Empty(t, f.NavIDs, "momentum owns no screens of its own")
	require.Empty(t, f.RoutePrefixes)
	require.Empty(t, f.Skill)
	require.False(t, f.Default, "optional features ship off")
}

func TestRegistry_GamificationEntry(t *testing.T) {
	f, ok := Get(Gamification)
	require.True(t, ok)
	require.Equal(t, Key("gamification"), f.Key)
	require.Equal(t, "Gamification", f.Label)
	require.NotEmpty(t, f.Blurb)
	require.Empty(t, f.Tools, "gamification owns no MCP tools -- its surfaces live inside the Now screen")
	require.Empty(t, f.NavIDs, "gamification owns no screens of its own -- the Now screen itself is core")
	require.Empty(t, f.RoutePrefixes)
	require.Empty(t, f.Skill)
	require.NotEmpty(t, f.Surfaces, "each in-page surface registers its owner-facing phrase")
	require.False(t, f.Default, "optional features ship off")
}

func TestRegistry_ScreeningEntry(t *testing.T) {
	f, ok := Get(Screening)
	require.True(t, ok)
	require.Equal(t, Key("screening"), f.Key)
	require.Equal(t, "Write screening", f.Label)
	require.NotEmpty(t, f.Blurb)
	require.Empty(t, f.Tools, "screening owns no MCP tools -- it screens the existing write tools")
	require.Equal(t, []string{"held"}, f.NavIDs)
	require.Equal(t, []string{"/console/held"}, f.RoutePrefixes)
	require.Empty(t, f.Skill)
	require.NotEmpty(t, f.Surfaces, "the provenance marks are an in-page surface")
	require.False(t, f.Default, "optional features ship off")
}

func TestRegistry_OrderIsResearchThenMomentumThenGamificationThenScreening(t *testing.T) {
	reg := Registry()
	require.Len(t, reg, 4)
	require.Equal(t, Research, reg[0].Key, "registry order is the Settings card order")
	require.Equal(t, Momentum, reg[1].Key)
	require.Equal(t, Gamification, reg[2].Key)
	require.Equal(t, Screening, reg[3].Key)
}

func TestRegistry_WellFormed(t *testing.T) {
	seenKey := map[Key]bool{}
	seenTool := map[string]Key{}
	for _, f := range Registry() {
		require.NotEmpty(t, f.Key)
		require.NotEmpty(t, f.Label, "%s needs a Settings label", f.Key)
		require.NotEmpty(t, f.Blurb, "%s needs a Settings blurb", f.Key)
		require.False(t, seenKey[f.Key], "duplicate feature key %s", f.Key)
		seenKey[f.Key] = true
		for _, tool := range f.Tools {
			owner, dup := seenTool[tool]
			require.False(t, dup, "tool %s claimed by both %s and %s", tool, owner, f.Key)
			seenTool[tool] = f.Key
		}
		// Every feature must be wired to a config field in both directions, or
		// its Settings toggle would silently do nothing.
		var c config.Features
		f.Set(&c, true)
		require.True(t, f.Enabled(c), "%s: Set(true) must be visible to Enabled", f.Key)
		f.Set(&c, false)
		require.False(t, f.Enabled(c), "%s: Set(false) must be visible to Enabled", f.Key)
	}
}

func TestRegistry_IsACopy(t *testing.T) {
	got := Registry()
	require.NotEmpty(t, got)
	got[0].Label = "mutated"
	require.NotEqual(t, "mutated", Registry()[0].Label, "Registry must hand out a copy")
}

func TestEnabled_UnknownKeyIsOff(t *testing.T) {
	require.False(t, Enabled(config.Features{Research: true}, Key("no-such-feature")))
}

func TestDefaults_MatchConfigDefaults(t *testing.T) {
	require.Equal(t, config.Defaults().Features, Defaults(),
		"registry defaults and config.Defaults() must agree")
	require.Equal(t, config.Features{}, Defaults(),
		"every optional feature ships off, so the zero value is the default")
}

func TestHiddenTools(t *testing.T) {
	hidden := HiddenTools(config.Features{Research: false})
	require.Equal(t, map[string]bool{"lab_open": true, "trial_record": true, "trial_query": true}, hidden)

	require.Empty(t, HiddenTools(config.Features{Research: true}),
		"with every feature on, nothing is hidden")
}

func TestToolOwners(t *testing.T) {
	owners := ToolOwners()
	require.Equal(t, Research, owners["trial_record"])
	require.Len(t, owners, 3)
}
