package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The release workflow's shape for the owner's signature (step D): goreleaser
// leaves a draft, a publish job that waits for the owner's approval checks
// checksums.txt.sshsig and only then publishes, and everything public after it
// (the MCP registry, the site) waits for the publish. actionlint and
// goreleaser check, in CI's config-lint, are what parse these for real; this
// holds the decisions they cannot see.
func TestReleaseWorkflow_PublishesOnlyAfterTheOwnersSignature(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "release.yml"))
	require.NoError(t, err)
	var wf struct {
		Jobs map[string]struct {
			Needs       any               `yaml:"needs"`
			If          string            `yaml:"if"`
			Environment any               `yaml:"environment"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Name string `yaml:"name"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
				With map[string]any
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &wf))

	gr, ok := wf.Jobs["goreleaser"]
	require.True(t, ok)
	require.Equal(t, "release", gr.Environment, "step F: the tag's release jobs wait for the owner")
	for _, s := range gr.Steps {
		require.NotContains(t, s.Run, "--draft=false", "the goreleaser job never publishes")
	}

	pub, ok := wf.Jobs["publish"]
	require.True(t, ok)
	require.Equal(t, "goreleaser", pub.Needs)
	require.Equal(t, "release", pub.Environment, "the publish waits for the owner's approval, after the owner signs")
	require.Equal(t, "write", pub.Permissions["contents"])
	var check, publish = -1, -1
	for i, s := range pub.Steps {
		if strings.Contains(s.Run, "releasekit verify-owner -require") && check < 0 {
			check = i
			require.Contains(t, s.Run, `if [ "$CANONICAL" != true ]`, "a fork's release publishes as before")
			require.Contains(t, s.Run, "checksums.txt.sshsig")
		}
		if strings.Contains(s.Run, "gh release edit") && strings.Contains(s.Run, "--draft=false") {
			publish = i
		}
	}
	require.GreaterOrEqual(t, check, 0, "the publish job checks the owner's signature")
	require.Greater(t, publish, check, "and publishes only after the check")

	for _, job := range []string{"mcp-registry", "pages"} {
		j, ok := wf.Jobs[job]
		require.True(t, ok, job)
		require.Equal(t, "publish", j.Needs, "%s waits for the publish", job)
		require.Contains(t, j.If, "needs.publish.result == 'success'", job)
	}

	gor, err := os.ReadFile(filepath.Join(repoRoot, ".goreleaser.yaml"))
	require.NoError(t, err)
	var cfg struct {
		Checksum struct {
			ExtraFiles []struct {
				Glob         string `yaml:"glob"`
				NameTemplate string `yaml:"name_template"`
			} `yaml:"extra_files"`
		} `yaml:"checksum"`
		Release struct {
			Draft            bool `yaml:"draft"`
			UseExistingDraft bool `yaml:"use_existing_draft"`
		} `yaml:"release"`
	}
	require.NoError(t, yaml.Unmarshal(gor, &cfg))
	require.True(t, cfg.Release.Draft, "goreleaser leaves the release a draft for the owner to sign")
	require.True(t, cfg.Release.UseExistingDraft)
	listed := map[string]string{}
	for _, f := range cfg.Checksum.ExtraFiles {
		listed[f.Glob] = f.NameTemplate
	}
	require.Equal(t, "install", listed["docs/install"], "checksums.txt vouches for the installers under their release names")
	require.Equal(t, "install.ps1", listed["docs/install.ps1"])
}
