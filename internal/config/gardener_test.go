package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The two switches ship unset: agents may apply (nil means on), and nothing
// applies by itself.
func TestAaGardenerSwitches_Defaults(t *testing.T) {
	g := Defaults().Gardener
	require.Nil(t, g.AgentsMayApply)
	require.Nil(t, g.AutoApply)
	require.NoError(t, Defaults().Validate())
}

// The file sets both; the environment wins over the file, and a set but
// unparseable agents switch is an error, never a silent default.
func TestAaGardenerSwitches_FileAndEnv(t *testing.T) {
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := writeConfig(t, `
gardener:
  agents_may_apply: false
  auto_apply: [tool_error, memory_wanted]
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, new(false), cfg.Gardener.AgentsMayApply)
	require.Equal(t, []string{"tool_error", "memory_wanted"}, cfg.Gardener.AutoApply)

	t.Setenv("SEAMLESS_GARDENER_AGENTS_MAY_APPLY", "true")
	t.Setenv("SEAMLESS_GARDENER_AUTO_APPLY", "ship_plan, abandon_plan,")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, new(true), cfg.Gardener.AgentsMayApply, "env wins over file")
	require.Equal(t, []string{"ship_plan", "abandon_plan"}, cfg.Gardener.AutoApply, "a trailing comma is harmless")

	t.Setenv("SEAMLESS_GARDENER_AUTO_APPLY", "")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Gardener.AutoApply, "an empty variable empties the list")

	t.Setenv("SEAMLESS_GARDENER_AGENTS_MAY_APPLY", "sometimes")
	_, err = LoadFrom(path)
	require.ErrorContains(t, err, "SEAMLESS_GARDENER_AGENTS_MAY_APPLY")
}

// A kind automatic apply may not take is refused at load, from the file and
// from the environment alike, and the error says why and names the four.
func TestAaGardenerAutoApply_RefusedAtLoad(t *testing.T) {
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	_, err := LoadFrom(writeConfig(t, "gardener:\n  auto_apply: [memory_wanted, merge]\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), `gardener.auto_apply: "merge" cannot be applied automatically`)
	require.Contains(t, err.Error(), "reversible, need no LLM and only add or retag")
	require.Contains(t, err.Error(), "valid values are abandon_plan, memory_wanted, tool_error, ship_plan")

	t.Setenv("SEAMLESS_GARDENER_AUTO_APPLY", "consolidate")
	_, err = LoadFrom("")
	require.ErrorContains(t, err, `gardener.auto_apply: "consolidate" cannot be applied automatically`)
}

// The override row's shape: a nil field is no override, an empty kinds list is
// a choice of none, and a kind outside the four is refused.
func TestAaGardenerOverride_JSONAndValidate(t *testing.T) {
	raw, err := json.Marshal(GardenerOverride{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(raw))

	none := []string{}
	raw, err = json.Marshal(GardenerOverride{AgentsMayApply: new(false), AutoApply: &none})
	require.NoError(t, err)
	require.JSONEq(t, `{"agents_may_apply":false,"auto_apply":[]}`, string(raw))
	var back GardenerOverride
	require.NoError(t, json.Unmarshal(raw, &back))
	require.NotNil(t, back.AutoApply, "an empty list survives the round trip as a choice")
	require.Empty(t, *back.AutoApply)

	require.NoError(t, GardenerOverride{AutoApply: &[]string{"ship_plan"}}.Validate())
	require.ErrorContains(t, GardenerOverride{AutoApply: &[]string{"split"}}.Validate(), `"split" cannot be applied automatically`)
}
