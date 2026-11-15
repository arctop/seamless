package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An entry of peer_auth.restricted is a plain name or uid, or a mapping with
// account and at most one of exclude and only (plan agent-accounts-and-releases,
// step B). The plain form keeps its meaning byte for byte; {account} alone is
// the plain form too.
func TestPeerAuth_RestrictedShapes(t *testing.T) {
	cfg, err := loadYAML(t, peerBase+`  restricted:
    - "502"
    - _example_plain
    - account: _example_bare
    - account: _example_agent
      exclude: [example-pipeline, example-vault]
    - account: _example_desk
      only: [example-desk]
`)
	require.NoError(t, err)
	require.Equal(t, []RestrictedAccount{
		{Account: "502"},
		{Account: "_example_plain"},
		{Account: "_example_bare"},
		{Account: "_example_agent", Exclude: []string{"example-pipeline", "example-vault"}},
		{Account: "_example_desk", Only: []string{"example-desk"}},
	}, cfg.PeerAuth.Restricted)
	require.False(t, cfg.PeerAuth.Restricted[2].Shaped(), "{account} alone is the plain form")
	require.True(t, cfg.PeerAuth.Restricted[3].Shaped())
	require.True(t, cfg.PeerAuth.Restricted[4].Shaped())
	require.Equal(t, []string{"502", "_example_plain", "_example_bare", "_example_agent", "_example_desk"},
		RestrictedNames(cfg.PeerAuth.Restricted))
	require.Equal(t, []string{"example-desk", "example-pipeline", "example-vault"}, cfg.PeerAuth.ReferencedProjects(),
		"the slugs a shaped entry names are the block's too")

	// An unquoted uid reads as the same account it did before shapes existed.
	cfg, err = loadYAML(t, peerBase+"  restricted: [502]\n")
	require.NoError(t, err)
	require.Equal(t, BareRestricted("502"), cfg.PeerAuth.Restricted)

	// A YAML alias resolves to what it names.
	cfg, err = loadYAML(t, peerBase+"  restricted:\n    - &desk {account: _example_desk, only: [example-desk]}\n")
	require.NoError(t, err)
	require.Equal(t, []RestrictedAccount{{Account: "_example_desk", Only: []string{"example-desk"}}}, cfg.PeerAuth.Restricted)

	entry := func(body string) string { return peerBase + "  restricted:\n" + body }
	for _, tc := range []struct{ name, yaml, want string }{
		// The yaml.v3 regression: Node.Decode does not inherit KnownFields, so
		// without the explicit key check this typo loaded as a plain entry --
		// the full reach. It must be refused, with its name and line.
		{"a mistyped exclude", entry("    - account: _example_agent\n      exlude: [example-vault]\n"), `(line 10) has unknown key "exlude"`},
		{"a mistyped only", entry("    - account: _example_agent\n      onyl: [example-desk]\n"), `unknown key "onyl"`},
		{"exclude with only", entry("    - account: _example_agent\n      exclude: [example-vault]\n      only: [example-ops]\n"),
			"sets both exclude and only"},
		{"an empty only", entry("    - account: _example_agent\n      only: []\n"), "only is empty"},
		{"an empty exclude", entry("    - account: _example_agent\n      exclude: []\n"), "exclude is empty"},
		{"only as a scalar", entry("    - account: _example_agent\n      only: example-desk\n"), "only is a list of project slugs"},
		{"a slug that is a mapping", entry("    - account: _example_agent\n      only: [{a: b}]\n"), "only is a list of project slugs"},
		{"no account", entry("    - only: [example-desk]\n"), "has no account"},
		{"account as a list", entry("    - account: [_example_agent]\n"), "account is an OS user name or uid"},
		{"a list as an entry", entry("    - [_example_agent]\n"), "is neither an account"},
		{"the global scope", entry("    - account: _example_agent\n      only: [global]\n"), "names the global scope"},
		{"the global scope by its folder name", entry("    - account: _example_agent\n      exclude: [_global]\n"), "names the global scope"},
		{"an invalid slug", entry("    - account: _example_agent\n      only: [\"a/b\"]\n"), "invalid project"},
		{"a padded slug", entry("    - account: _example_agent\n      only: [\" example-desk\"]\n"), "without surrounding space"},
		{"a slug twice", entry("    - account: _example_agent\n      exclude: [example-vault, example-vault]\n"), "twice"},
		{"one account in both forms", entry("    - _example_agent\n    - account: _example_agent\n      only: [example-desk]\n"),
			`lists "_example_agent" twice`},
		{"the owner, shaped", entry("    - account: _example_owner\n      only: [example-desk]\n"), "the owner"},
		{"while disabled", "peer_auth:\n  restricted:\n    - account: _example_agent\n      only: [example-desk]\n",
			"while peer_auth is disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A block built in code rather than loaded is held to the same shape rules by
// validatePeerRestricted.
func TestPeerAuth_RestrictedShapeValidation(t *testing.T) {
	base := PeerAuth{Enabled: true, Owner: "_example_owner"}
	for _, tc := range []struct {
		name  string
		entry RestrictedAccount
		want  string
	}{
		{"both", RestrictedAccount{Account: "_a", Exclude: []string{"x"}, Only: []string{"y"}}, "both exclude and only"},
		{"global", RestrictedAccount{Account: "_a", Only: []string{""}}, "global scope"},
	} {
		p := base
		p.Restricted = []RestrictedAccount{tc.entry}
		err := validatePeerRestricted(p)
		require.Error(t, err, tc.name)
		require.Contains(t, err.Error(), tc.want, tc.name)
	}
	p := base
	p.Restricted = []RestrictedAccount{{Account: "_a", Exclude: []string{"example-vault"}}, {Account: "_b", Only: []string{"example-desk"}}}
	require.NoError(t, validatePeerRestricted(p))
}
