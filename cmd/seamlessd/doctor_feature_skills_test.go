package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	agentskills "github.com/arctop/seamless/internal/skills"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The skill install is client-side, so it can outlive the feature it documents:
// the owner switches research off in the console and ~/.claude/skills still
// holds a package naming three tools the server no longer exposes. doctor is
// where that drift surfaces -- as INFO, because nothing is broken and no data is
// at risk, and only while the skill is on disk AND the feature is off.
func TestFeatureSkillsCheck_InfoOnlyWhenAnInstalledSkillOutlivesItsFeature(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("SEAMLESS_NO_RESEARCH_SKILL", "")

	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	cfg := config.Defaults()
	cfg.Features.Research = false

	// Feature off and no skill installed: the state install-hooks leaves behind.
	c := featureSkillsCheck(db, cfg)
	require.Equal(t, statusOK, c.status)
	require.Equal(t, "feature skills", c.name)
	require.Contains(t, c.detail, "no installed skill documents a disabled optional feature")

	result, err := agentskills.Install(agentskills.ClientClaude, agentskills.Options{HomeDir: home})
	require.NoError(t, err)
	require.Equal(t, agentskills.ActionInstalled, result.Research)

	c = featureSkillsCheck(db, cfg)
	require.Equal(t, statusInfo, c.status)
	require.Contains(t, c.detail, filepath.Join(home, ".claude", "skills", agentskills.ResearchName))
	require.Contains(t, c.detail, "feature research is off")
	require.Contains(t, c.detail, "agents may see a skill referencing disabled tools")
	require.Contains(t, c.detail, "seamlessd install-hooks")
	require.Contains(t, c.detail, "re-enable the feature")
	// The other client's root is empty, so it is not named.
	require.NotContains(t, c.detail, codexHome)

	// Enabled in the file/env base: same files on disk, no drift to report.
	enabled := cfg
	enabled.Features.Research = true
	require.Equal(t, statusOK, featureSkillsCheck(db, enabled).status)

	// And enabled by the stored override the console writes, which layers over
	// the file/env base exactly as it does for every other reader.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	require.Equal(t, statusOK, featureSkillsCheck(db, cfg).status)

	// Turning it off through the same row brings the line back.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: false}))
	require.Equal(t, statusInfo, featureSkillsCheck(db, enabled).status)

	// Removing the package is the other way out, and clears it.
	require.NoError(t, store.ClearFeaturesConfig(ctx, db))
	_, err = agentskills.Install(agentskills.ClientClaude, agentskills.Options{
		HomeDir:        home,
		DisabledSkills: []string{agentskills.ResearchName},
	})
	require.NoError(t, err)
	require.Equal(t, statusOK, featureSkillsCheck(db, cfg).status)
}

// A corrupt override row must not hide the check behind a false OK: the reader
// is failure-soft everywhere else, but here "cannot tell" is its own answer.
func TestFeatureSkillsCheck_UnreadableOverrideWarns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, store.SetSetting(context.Background(), db, store.SettingFeaturesConfig, "{not json"))
	c := featureSkillsCheck(db, config.Defaults())
	require.Equal(t, statusWarn, c.status)
	require.Contains(t, c.detail, "cannot read the stored feature toggles")
}
