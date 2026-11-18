package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A value of peer_auth.relays.<relay>.restricted is the account's uid there,
// as before, or a mapping with uid and at most one of exclude and only (plan
// networked-relay; the shapes peer_auth.restricted's entries take). The uid
// form keeps its meaning; {uid} alone is the plain form too.
func TestPeerAuth_RelayRestrictedShapes(t *testing.T) {
	relay := func(body string) string {
		return peerBase + "  relays:\n    example-laptop:\n      owner_uid: 501\n      restricted:\n" + body
	}
	cfg, err := loadYAML(t, relay(`        _example_helper: 502
        _example_plain:
          uid: 503
        _example_agent:
          uid: 504
          exclude: [example-pipeline, example-vault]
        _example_desk:
          uid: 505
          only: [example-desk]
`))
	require.NoError(t, err)
	got := cfg.PeerAuth.Relays["example-laptop"].Restricted
	require.Equal(t, map[string]RelayRestricted{
		"_example_helper": {UID: 502},
		"_example_plain":  {UID: 503},
		"_example_agent":  {UID: 504, Exclude: []string{"example-pipeline", "example-vault"}},
		"_example_desk":   {UID: 505, Only: []string{"example-desk"}},
	}, got)
	require.False(t, got["_example_helper"].Shaped())
	require.False(t, got["_example_plain"].Shaped(), "{uid} alone is the plain form")
	require.True(t, got["_example_agent"].Shaped())
	require.True(t, got["_example_desk"].Shaped())
	require.Equal(t, []string{"example-desk", "example-pipeline", "example-vault"}, cfg.PeerAuth.ReferencedProjects(),
		"the slugs a relay's shaped entry names are the block's too")

	// The slugs of both lists, this machine's and a relay's, once each.
	cfg, err = loadYAML(t, peerBase+`  force_locked: [example-finance]
  restricted:
    - account: _example_local
      only: [example-desk]
  relays:
    example-laptop:
      owner_uid: 501
      restricted:
        _example_desk: {uid: 505, only: [example-desk]}
        _example_agent: {uid: 504, exclude: [example-vault]}
`)
	require.NoError(t, err)
	require.Equal(t, []string{"example-desk", "example-finance", "example-vault"}, cfg.PeerAuth.ReferencedProjects())

	// A YAML alias resolves to what it names.
	cfg, err = loadYAML(t, relay("        _example_desk: &desk {uid: 505, only: [example-desk]}\n"))
	require.NoError(t, err)
	require.Equal(t, map[string]RelayRestricted{"_example_desk": {UID: 505, Only: []string{"example-desk"}}},
		cfg.PeerAuth.Relays["example-laptop"].Restricted)

	// Two accounts, on one relay or on two, may name one project (B-D10), and
	// two relays may each list one uid.
	cfg, err = loadYAML(t, peerBase+`  relays:
    example-laptop:
      owner_uid: 501
      restricted:
        _example_desk: {uid: 505, only: [example-desk]}
        _example_twin: {uid: 506, only: [example-desk]}
    example-studio:
      owner_uid: 501
      restricted:
        _example_desk: {uid: 505, only: [example-desk]}
`)
	require.NoError(t, err)
	require.Len(t, cfg.PeerAuth.Relays, 2)

	entry := func(body string) string { return relay("        _example_agent:\n" + body) }
	for _, tc := range []struct{ name, yaml, want string }{
		// The yaml.v3 regression, as for peer_auth.restricted: Node.Decode does
		// not inherit KnownFields, so without the explicit key check this typo
		// would load as a plain entry -- the full reach.
		{"a mistyped exclude", entry("          uid: 504\n          exlude: [example-vault]\n"), `(line 14) has unknown key "exlude"`},
		{"a mistyped only", entry("          uid: 504\n          onyl: [example-desk]\n"), `unknown key "onyl"`},
		{"the other form's key", entry("          account: _example_agent\n          only: [example-desk]\n"), `unknown key "account"`},
		// A key twice: the decoder's own refusal never sees a node a custom
		// UnmarshalYAML reads, and the last list would replace the first.
		{"exclude twice", entry("          uid: 504\n          exclude: [example-vault]\n          exclude: [example-pipeline]\n"),
			"(line 15) sets exclude twice (first on line 14)"},
		{"only twice", entry("          uid: 504\n          only: [example-vault]\n          only: [example-vault, example-ops]\n"),
			"sets only twice"},
		{"uid twice", entry("          uid: 504\n          uid: 505\n"), "sets uid twice"},
		{"exclude with only", entry("          uid: 504\n          exclude: [example-vault]\n          only: [example-ops]\n"),
			"sets both exclude and only"},
		{"an empty only", entry("          uid: 504\n          only: []\n"), "only is empty"},
		{"an empty exclude", entry("          uid: 504\n          exclude: []\n"), "exclude is empty"},
		{"only as a scalar", entry("          uid: 504\n          only: example-desk\n"), "only is a list of project slugs"},
		{"a slug that is a mapping", entry("          uid: 504\n          only: [{a: b}]\n"), "only is a list of project slugs"},
		{"no uid", entry("          only: [example-desk]\n"), "has no uid"},
		{"an empty mapping", relay("        _example_agent: {}\n"), "has no uid"},
		{"uid as a list", entry("          uid: [504]\n"), "uid is the account's numeric uid"},
		{"a negative uid", entry("          uid: -1\n"), "cannot unmarshal"},
		{"a uid past 2^32-1", entry("          uid: 4294967296\n"), "cannot unmarshal"},
		{"a name as the uid", entry("          uid: _example_agent\n"), "cannot unmarshal"},
		{"a list as an entry", relay("        _example_agent: [504]\n"), "is neither a uid nor a mapping"},
		// As before shapes existed: a null value is refused by name.
		{"a null entry", relay("        _example_agent:\n"), "config.peer_auth.relays.example-laptop.restricted._example_agent must not be null"},
		{"a null only", entry("          uid: 504\n          only:\n"), "restricted._example_agent.only must not be null"},
		{"root, shaped", entry("          uid: 0\n          only: [example-desk]\n"), "uid 0 is root"},
		{"the owner, shaped", entry("          uid: 501\n          only: [example-desk]\n"), "uid 501 is owner_uid"},
		{"one uid in both forms", relay("        _example_agent: 504\n        _example_other: {uid: 504, only: [example-desk]}\n"),
			"lists uid 504 twice"},
		{"the global scope", entry("          uid: 504\n          only: [global]\n"), "names the global scope"},
		{"the global scope by its folder name", entry("          uid: 504\n          exclude: [_global]\n"), "names the global scope"},
		{"an invalid slug", entry("          uid: 504\n          only: [\"a/b\"]\n"), "invalid project"},
		{"a padded slug", entry("          uid: 504\n          only: [\" example-desk\"]\n"), "without surrounding space"},
		{"a slug twice", entry("          uid: 504\n          exclude: [example-vault, example-vault]\n"), "twice"},
		{"while disabled", "peer_auth:\n  relays:\n    example-laptop:\n      owner_uid: 501\n      restricted:\n" +
			"        _example_agent: {uid: 504, only: [example-desk]}\n", "while peer_auth is disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A refusal names the relay and the account whose list it is.
func TestPeerAuth_RelayRestrictedShapeNamesTheEntry(t *testing.T) {
	_, err := loadYAML(t, peerBase+"  relays:\n    example-laptop:\n      owner_uid: 501\n      restricted:\n"+
		"        _example_agent: {uid: 504, only: [global]}\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), `config: peer_auth.relays "example-laptop" restricted "_example_agent" only names the global scope`)
}

// A block built in code rather than loaded is held to the same shape rules by
// validatePeerRelays.
func TestPeerAuth_RelayRestrictedShapeValidation(t *testing.T) {
	base := PeerAuth{Enabled: true, Owner: "_example_owner"}
	for _, tc := range []struct {
		name  string
		entry RelayRestricted
		want  string
	}{
		{"both", RelayRestricted{UID: 504, Exclude: []string{"x"}, Only: []string{"y"}}, "both exclude and only"},
		{"global", RelayRestricted{UID: 504, Only: []string{""}}, "global scope"},
		{"a slug twice", RelayRestricted{UID: 504, Exclude: []string{"x", "x"}}, "twice"},
	} {
		p := base
		p.Relays = map[string]PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]RelayRestricted{"_a": tc.entry}}}
		err := validatePeerRelays(p)
		require.Error(t, err, tc.name)
		require.Contains(t, err.Error(), tc.want, tc.name)
	}
	p := base
	p.Relays = map[string]PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]RelayRestricted{
		"_a": {UID: 504, Exclude: []string{"example-vault"}}, "_b": {UID: 505, Only: []string{"example-desk"}}, "_c": {UID: 506}}}}
	require.NoError(t, validatePeerRelays(p))
	require.Equal(t, map[string]RelayRestricted{"_a": {UID: 504}, "_b": {UID: 505}},
		BareRelayRestricted(map[string]uint32{"_a": 504, "_b": 505}))
}
