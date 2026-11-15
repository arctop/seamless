package access

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Per-account reach (peer_auth.restricted's exclude and only; plan
// agent-accounts-and-releases, step B; memory
// restricted-account-reach-decisions). Generic names only: example-ops,
// example-pipeline and example-vault open, example-desk confidential,
// example-sealed sealed, example-other another confidential, lockFin locked.

const (
	rsOps      = "example-ops"
	rsPipeline = "example-pipeline"
	rsVault    = "example-vault"
	rsDesk     = "example-desk"
	rsSealed   = "example-sealed"
	rsOther    = "example-other"
	rsLockedOp = "example-locked-open" // open, and locked
)

var (
	rsOpen   = []string{rsOps, rsPipeline, rsVault, rsLockedOp}
	rsFenced = []string{rsDesk, rsSealed, rsOther, lockFin}
)

var rsAccounts = map[string]Account{
	"owner":          {UID: 501, Name: "owner"},
	"_example_bare":  {UID: 510, Name: "_example_bare"},
	"_example_excl":  {UID: 511, Name: "_example_excl"},
	"_example_multi": {UID: 512, Name: "_example_multi"},
	"_example_desk":  {UID: 513, Name: "_example_desk"},
	"_example_mixed": {UID: 514, Name: "_example_mixed"},
	"_example_lockd": {UID: 515, Name: "_example_lockd"},
	"_example_seal":  {UID: 516, Name: "_example_seal"},
	"_example_ghost": {UID: 517, Name: "_example_ghost"},
	"_example_twin":  {UID: 518, Name: "_example_twin"},
}

func rsPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled: true, Owner: "owner",
		ForceLocked: []string{lockFin, rsLockedOp},
		Restricted: []config.RestrictedAccount{
			{Account: "_example_bare"},
			{Account: "_example_excl", Exclude: []string{rsPipeline, rsVault}},
			{Account: "_example_multi", Only: []string{rsOps, rsPipeline}},
			{Account: "_example_desk", Only: []string{rsDesk}},
			{Account: "_example_mixed", Only: []string{rsDesk, rsOps}},
			{Account: "_example_lockd", Only: []string{lockFin}},
			{Account: "_example_seal", Only: []string{rsSealed}},
			{Account: "_example_ghost", Only: []string{"example-nowhere"}},
			{Account: "_example_twin", Only: []string{rsDesk}}, // B-D10: two accounts, one project
		},
	}, func(n string) (Account, error) {
		if a, ok := rsAccounts[n]; ok {
			return a, nil
		}
		return Account{}, fmt.Errorf("unknown account %q", n)
	})
	require.NoError(t, err)
	return p
}

func rsView(t *testing.T, p *Policy, account string) View {
	t.Helper()
	v, ok := p.PeerView(rsAccounts[account].UID)
	require.True(t, ok, account)
	return v.WithReach(rsOpen, rsFenced)
}

// The reach rule per shape: bare and exclude draw from the open, unlocked
// projects; only with two or more names keeps the open, unlocked ones among
// them; only with one name reaches it in any known isolation state unless it
// is locked. Global is readable by every view and writable by none.
func TestRestricted_ReachShapes(t *testing.T) {
	p := rsPolicy(t)
	for _, tc := range []struct {
		account string
		want    []string
		shaped  bool
	}{
		{"_example_bare", []string{rsOps, rsPipeline, rsVault}, false},
		{"_example_excl", []string{rsOps}, true},
		{"_example_multi", []string{rsOps, rsPipeline}, true},
		{"_example_desk", []string{rsDesk}, true},
		{"_example_mixed", []string{rsOps}, true}, // a fenced project beside others is not reached
		{"_example_lockd", []string{}, true},      // a lock closes even the one project
		{"_example_seal", []string{rsSealed}, true},
		{"_example_ghost", []string{}, true}, // an unregistered project is never reached
		{"_example_twin", []string{rsDesk}, true},
	} {
		v := rsView(t, p, tc.account)
		got, ok := v.Allowed()
		require.True(t, ok, tc.account)
		require.Equal(t, tc.want, got, tc.account)
		require.Equal(t, tc.shaped, v.ReachShaped(), tc.account)
		for _, slug := range append(append([]string{}, rsOpen...), rsFenced...) {
			in := false
			for _, w := range tc.want {
				in = in || w == slug
			}
			require.Equal(t, in, v.Visible(slug), "%s reads %s", tc.account, slug)
			require.Equal(t, in, v.Writable(slug), "%s writes %s", tc.account, slug)
		}
		require.True(t, v.Visible(""), "%s reads global", tc.account)
		require.False(t, v.Writable(""), "%s never writes global", tc.account)
		require.False(t, v.Visible("example-nowhere"))
	}
}

// The invariant that lets clearance stay out of restricted accounts: a reach
// that holds a confidential or sealed project holds nothing else, for every
// shape and every mix of states.
func TestRestricted_ReachInvariant(t *testing.T) {
	p := rsPolicy(t)
	fenced := map[string]bool{}
	for _, s := range rsFenced {
		fenced[s] = true
	}
	for account := range rsAccounts {
		if account == "owner" {
			continue
		}
		got, _ := rsView(t, p, account).Allowed()
		for _, slug := range got {
			if fenced[slug] {
				require.Len(t, got, 1, "%s's reach holds fenced %s and more: %v", account, slug, got)
			}
		}
	}
}

// Reach follows the projects' states at the request (B-D6): an excluded
// project stays excluded once confidential and once open again; a single
// only project is reached while open, confidential or sealed, and not once
// it is gone; a multi-project only drops a project that turned confidential.
func TestRestricted_ReachFollowsStates(t *testing.T) {
	p := rsPolicy(t)
	excl, _ := p.PeerView(rsAccounts["_example_excl"].UID)
	got, _ := excl.WithReach([]string{rsOps, rsVault}, []string{rsPipeline}).Allowed()
	require.Equal(t, []string{rsOps}, got)

	desk, _ := p.PeerView(rsAccounts["_example_desk"].UID)
	for _, states := range []struct{ open, fenced []string }{
		{[]string{rsDesk}, nil},
		{nil, []string{rsDesk}},
	} {
		got, _ := desk.WithReach(states.open, states.fenced).Allowed()
		require.Equal(t, []string{rsDesk}, got)
	}
	got, _ = desk.WithReach(rsOpen, nil).Allowed()
	require.Empty(t, got, "a project no longer registered is not reached")

	multi, _ := p.PeerView(rsAccounts["_example_multi"].UID)
	got, _ = multi.WithReach([]string{rsOps}, []string{rsPipeline}).Allowed()
	require.Equal(t, []string{rsOps}, got)

	// WithOpenProjects is WithReach with nothing fenced.
	got, _ = desk.WithOpenProjects([]string{rsDesk}).Allowed()
	require.Equal(t, []string{rsDesk}, got)
	got, _ = desk.WithOpenProjects(nil).Allowed()
	require.Empty(t, got)
}

// Texts (B-D9): a plain entry keeps the texts it had before shapes, word for
// word; a shaped one gets the neutral set, which names no isolation state and
// reads the same for an excluded, closed or unregistered project.
func TestRestricted_ShapedTexts(t *testing.T) {
	p := rsPolicy(t)
	bare := rsView(t, p, "_example_bare")
	require.EqualError(t, bare.Refusal(rsDesk), "project example-desk is not reachable from account _example_bare: a restricted "+
		"account reaches open projects that are not locked, and reads the global scope; it cannot create a project either -- the "+
		"owner creates and maps them")
	require.EqualError(t, bare.Refusal(""), "the global scope is read-only for account _example_bare: a restricted account reads "+
		"it and writes only into open projects -- name one with project=<slug>")
	require.Equal(t, "account _example_bare files no access requests: a restricted account reaches the open projects that "+
		"are not locked, and reads the global scope; the owner's approval never opens anything else to it, so "+
		"there is nothing to request -- keep to the projects project_list shows",
		RestrictedNoRequestsFor(WithView(context.Background(), bare), "_example_bare"))

	excl := rsView(t, p, "_example_excl")
	want := func(project string) string {
		return "project " + project + " is not reachable from account _example_excl: the owner gives this account only the " +
			"projects project_list shows; it cannot create a project either -- the owner creates and maps them"
	}
	for _, slug := range []string{rsVault, rsDesk, lockFin, "example-nowhere"} { // excluded, confidential, locked, unregistered
		err := excl.Refusal(slug)
		require.EqualError(t, err, want(slug))
		require.True(t, errors.Is(err, ErrNotReachable))
	}
	require.EqualError(t, excl.Refusal(""), "account _example_excl does not write the global scope: it writes only into the "+
		"projects project_list shows -- name one with project=<slug>")
	ctx := WithView(context.Background(), excl)
	require.True(t, ReachShaped(ctx))
	msg := RestrictedNoRequestsFor(ctx, "_example_excl")
	require.Contains(t, msg, "the owner gives this account only the projects project_list shows")
	stateWord := regexp.MustCompile(`\b(open|confidential|sealed|locked)\b`)
	for _, text := range []string{msg, excl.Refusal(rsVault).Error(), excl.Refusal("").Error()} {
		require.False(t, stateWord.MatchString(text), "a shaped text names no isolation state: %s", text)
	}
	require.False(t, ReachShaped(context.Background()))
	require.False(t, ReachShaped(WithView(context.Background(), p.OwnerView())))
}

// A relay's restricted account has no shape (B-D11) even when its uid there
// is one this machine's config shapes: the rule is this socket's alone.
func TestRestricted_RelayAccountHasNoShape(t *testing.T) {
	p, err := New(config.PeerAuth{
		Enabled: true, Owner: "owner",
		Restricted: []config.RestrictedAccount{{Account: "_example_desk", Only: []string{rsDesk}}},
		Relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 700,
			Restricted: map[string]uint32{"helper": rsAccounts["_example_desk"].UID}}},
	}, func(n string) (Account, error) {
		if a, ok := rsAccounts[n]; ok {
			return a, nil
		}
		return Account{}, fmt.Errorf("unknown account %q", n)
	})
	require.NoError(t, err)
	v, ok := p.RelayChainView(judgeLaptop, "example-laptop", rsAccounts["_example_desk"].UID, nil)
	require.True(t, ok)
	v = v.WithReach(rsOpen, rsFenced)
	got, _ := v.Allowed()
	require.Equal(t, []string{rsLockedOp, rsOps, rsPipeline, rsVault}, got, "the relay's account has the plain reach")
	require.False(t, v.ReachShaped())
}

// A disabled entry (B-D7) is refused at the door; every other account is
// served, and the policy it came from is unchanged.
func TestRestricted_WithDisabled(t *testing.T) {
	p := rsPolicy(t)
	d := p.WithDisabled([]uint32{rsAccounts["_example_excl"].UID, 9999})
	_, ok := d.PeerView(rsAccounts["_example_excl"].UID)
	require.False(t, ok)
	require.True(t, d.RestrictedDisabled(rsAccounts["_example_excl"].UID))
	require.False(t, d.RestrictedDisabled(9999), "an unlisted uid is not a disabled entry")
	_, ok = d.PeerView(rsAccounts["_example_bare"].UID)
	require.True(t, ok)
	_, ok = p.PeerView(rsAccounts["_example_excl"].UID)
	require.True(t, ok, "WithDisabled copies")

	var disabled []string
	for _, e := range d.RestrictedEntries() {
		if e.Disabled {
			disabled = append(disabled, e.Account.Name)
		}
	}
	require.Equal(t, []string{"_example_excl"}, disabled)
}

// RestrictedEntries and ReachOf, which doctor and the startup log read.
func TestRestricted_Entries(t *testing.T) {
	p := rsPolicy(t)
	byName := map[string]RestrictedEntry{}
	for _, e := range p.RestrictedEntries() {
		byName[e.Account.Name] = e
	}
	require.False(t, byName["_example_bare"].Shaped())
	require.Equal(t, []string{rsPipeline, rsVault}, byName["_example_excl"].Exclude)
	require.Equal(t, []string{rsDesk, rsOps}, byName["_example_mixed"].Only)
	require.Equal(t, []string{rsDesk}, p.ReachOf(rsAccounts["_example_desk"].UID, rsOpen, rsFenced))
	require.Nil(t, p.ReachOf(4242, rsOpen, rsFenced))
}

// A block built in code is held to the two rules whose breach would widen an
// account: never exclude with only, never an empty only.
func TestRestricted_NewRefusesWideningShapes(t *testing.T) {
	resolve := func(n string) (Account, error) { return rsAccounts[n], nil }
	for name, e := range map[string]config.RestrictedAccount{
		"both":       {Account: "_example_excl", Exclude: []string{rsVault}, Only: []string{rsOps}},
		"empty only": {Account: "_example_excl", Only: []string{}},
	} {
		_, err := New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []config.RestrictedAccount{e}}, resolve)
		require.ErrorIs(t, err, ErrInvalidPolicy, name)
	}
}
