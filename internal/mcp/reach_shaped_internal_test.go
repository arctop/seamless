package mcp

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// rsViews returns the request contexts of a plain restricted account and of
// one whose entry shapes its reach (only [example-desk]).
func rsViews(t *testing.T) (bare, shaped context.Context) {
	t.Helper()
	uids := map[string]uint32{"owner": 501, "_example_agent": 502, "_example_desk": 503}
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []config.RestrictedAccount{
		{Account: "_example_agent"}, {Account: "_example_desk", Only: []string{"example-desk"}},
	}}, func(name string) (access.Account, error) {
		if uid, ok := uids[name]; ok {
			return access.Account{UID: uid, Name: name}, nil
		}
		return access.Account{}, fmt.Errorf("unknown account %q", name)
	})
	require.NoError(t, err)
	b, ok := policy.PeerView(502)
	require.True(t, ok)
	s, ok := policy.PeerView(503)
	require.True(t, ok)
	ctx := context.Background()
	return access.WithView(ctx, b.WithOpenProjects([]string{"example-open"})),
		access.WithView(ctx, s.WithReach(nil, []string{"example-desk"}))
}

var rsStateWord = regexp.MustCompile(`\b(open|confidential|sealed|locked)\b`)

// A plain entry's texts are unchanged; a shaped entry's name the projects
// project_list shows and no isolation state (decision B-D9).
func TestRestricted_ShapedAgentTexts(t *testing.T) {
	bare, shaped := rsViews(t)

	require.Equal(t, fmt.Sprintf(restrictedWriteScopeHelp, "_example_agent"), writeScopeHelpFor(bare))
	help := writeScopeHelpFor(shaped)
	require.Equal(t, fmt.Sprintf(shapedWriteScopeHelp, "_example_desk"), help)
	require.False(t, rsStateWord.MatchString(help), help)

	tools := []mcp.Tool{{Name: "memory_write", InputSchema: mcp.ToolInputSchema{Properties: map[string]any{
		"project": map[string]any{"type": "string", "description": writeProjectArgDesc}}}}}
	descOf := func(ts []mcp.Tool) string {
		return ts[0].InputSchema.Properties["project"].(map[string]any)["description"].(string)
	}
	require.Equal(t, restrictedProjectArgDesc, descOf(restrictedToolText(tools, false)))
	require.Equal(t, shapedProjectArgDesc, descOf(restrictedToolText(tools, true)))
	require.False(t, rsStateWord.MatchString(shapedProjectArgDesc))
	require.Equal(t, writeProjectArgDesc, descOf(tools), "the shared list is copied, never written through")

	require.EqualError(t, restrictedPlacementRefusal("_example_agent", "", false), "no cwd or project was given, and the global "+
		"scope is read-only for account _example_agent: a restricted account works only in open projects that already exist -- "+
		"name one with project=<slug> (project_list lists the ones it reaches)")
	for _, cwd := range []string{"", "/work/x"} {
		err := restrictedPlacementRefusal("_example_desk", cwd, true)
		require.Contains(t, err.Error(), "account _example_desk does not write the global scope: it works only in the projects project_list shows")
		require.False(t, rsStateWord.MatchString(err.Error()), err.Error())
	}

	require.Equal(t, access.RestrictedNoRequests("_example_agent"), restrictedAccessRequestRefusal(bare, "_example_agent").Error())
	require.Contains(t, restrictedAccessRequestRefusal(shaped, "_example_desk").Error(), "the owner gives this account only the projects project_list shows")
}
