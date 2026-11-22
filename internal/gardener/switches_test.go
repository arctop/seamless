package gardener

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The owner's rule (Gate A, 2026-11-21): automatic apply may take exactly the
// four kinds that are reversible, need no LLM and only add or retag. This
// pins config's one list to the store's kind names, to store.ProposalKinds
// order, and to the undo policy.
func TestAaAutoSafe_IsTheOwnersFourKinds(t *testing.T) {
	owners := []string{store.ProposalMemoryWanted, store.ProposalToolError, store.ProposalShipPlan, store.ProposalAbandonPlan}
	var inOrder []string
	for _, k := range store.ProposalKinds {
		require.Equal(t, slices.Contains(owners, k), AutoSafe(k), k)
		if AutoSafe(k) {
			inOrder = append(inOrder, k)
			require.True(t, CanUndoApply(k), "%s applies by itself only because it can be undone", k)
		}
	}
	require.Equal(t, inOrder, AutoSafeKinds(), "in store.ProposalKinds order")
	require.Equal(t, inOrder, config.GardenerAutoApplyKinds, "config's list is the same list")
	require.False(t, AutoSafe("nonsense"))
	require.False(t, AutoSafe(""))
}

// Config refuses at load exactly the kinds AutoSafe refuses: the file's
// validation and the gardener's judgement cannot drift apart.
func TestAaAutoSafe_ConfigValidationAgrees(t *testing.T) {
	for _, k := range append(slices.Clone(store.ProposalKinds), "nonsense", "MEMORY_WANTED") {
		cfg := config.Defaults()
		cfg.Gardener.AutoApply = []string{k}
		err := cfg.Validate()
		if AutoSafe(k) {
			require.NoError(t, err, k)
		} else {
			require.ErrorContains(t, err, "gardener.auto_apply", k)
		}
		ok := config.GardenerOverride{AutoApply: &[]string{k}}.Validate() == nil
		require.Equal(t, AutoSafe(k), ok, "the stored override refuses what the file does: %s", k)
	}
}

func TestAaEffective(t *testing.T) {
	none := []string{}
	tests := []struct {
		name     string
		base     config.Gardener
		o        config.GardenerOverride
		want     Switches
		overrode bool
	}{
		{name: "nothing set: agents may apply, nothing by itself",
			want: Switches{AgentsMayApply: true, AgentsSource: SourceDefault, AutoApply: []string{}, AutoSource: SourceDefault}},
		{name: "file true can be turned off here",
			base: config.Gardener{AgentsMayApply: new(true)}, o: config.GardenerOverride{AgentsMayApply: new(false)},
			want:     Switches{AgentsMayApply: false, AgentsSource: SourceConsole, AutoApply: []string{}, AutoSource: SourceDefault},
			overrode: true},
		{name: "file true alone",
			base: config.Gardener{AgentsMayApply: new(true)},
			want: Switches{AgentsMayApply: true, AgentsSource: SourceConfig, AutoApply: []string{}, AutoSource: SourceDefault}},
		{name: "file false is final over the console's on",
			base: config.Gardener{AgentsMayApply: new(false)}, o: config.GardenerOverride{AgentsMayApply: new(true)},
			want: Switches{AgentsMayApply: false, AgentsSource: SourceConfig, AgentsLocked: true, AutoApply: []string{}, AutoSource: SourceDefault}},
		{name: "file kinds, ordered and once each",
			base: config.Gardener{AutoApply: []string{"ship_plan", "memory_wanted", "ship_plan"}},
			want: Switches{AgentsMayApply: true, AgentsSource: SourceDefault, AutoApply: []string{"memory_wanted", "ship_plan"}, AutoSource: SourceConfig}},
		{name: "the console's kinds win over the file's",
			base: config.Gardener{AutoApply: []string{"memory_wanted"}}, o: config.GardenerOverride{AutoApply: &[]string{"tool_error"}},
			want:     Switches{AgentsMayApply: true, AgentsSource: SourceDefault, AutoApply: []string{"tool_error"}, AutoSource: SourceConsole},
			overrode: true},
		{name: "the console's choice of none wins too",
			base: config.Gardener{AutoApply: []string{"memory_wanted"}}, o: config.GardenerOverride{AutoApply: &none},
			want:     Switches{AgentsMayApply: true, AgentsSource: SourceDefault, AutoApply: []string{}, AutoSource: SourceConsole},
			overrode: true},
		{name: "a kind that is not AutoSafe never applies by itself",
			base: config.Gardener{AutoApply: []string{"merge", "tool_error"}},
			want: Switches{AgentsMayApply: true, AgentsSource: SourceDefault, AutoApply: []string{"tool_error"}, AutoSource: SourceConfig}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Effective(tt.base, tt.o)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.overrode, got.Overridden())
			for _, k := range store.ProposalKinds {
				require.Equal(t, slices.Contains(tt.want.AutoApply, k), got.AutoApplies(k), k)
			}
		})
	}
}

// The stored row is read when a check decides; one that cannot be read is no
// choice (the file decides, Unreadable), and a failed read is an error the
// caller decides nothing on.
func TestAaReadSwitches(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	base := config.Gardener{AutoApply: []string{"memory_wanted"}}

	sw, err := ReadSwitches(ctx, db, base, slog.Default())
	require.NoError(t, err)
	require.Equal(t, []string{"memory_wanted"}, sw.AutoApply)
	require.False(t, sw.Unreadable)

	require.NoError(t, store.SetGardenerOverride(ctx, db, config.GardenerOverride{AgentsMayApply: new(false)}))
	sw, err = ReadSwitches(ctx, db, base, slog.Default())
	require.NoError(t, err)
	require.False(t, sw.AgentsMayApply, "the console's choice applies at the next read")
	require.Equal(t, SourceConsole, sw.AgentsSource)

	require.NoError(t, store.SetSetting(ctx, db, store.SettingGardenerConfig, `{"auto_apply":["merge"]}`))
	sw, err = ReadSwitches(ctx, db, base, slog.Default())
	require.NoError(t, err)
	require.True(t, sw.Unreadable)
	require.True(t, sw.AgentsMayApply, "an unreadable row is no choice: the file decides")
	require.Equal(t, []string{"memory_wanted"}, sw.AutoApply)

	require.NoError(t, db.Close())
	_, err = ReadSwitches(ctx, db, base, slog.Default())
	require.Error(t, err, "a failed read is not a choice")
}
