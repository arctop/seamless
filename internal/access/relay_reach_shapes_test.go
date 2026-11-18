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

// Per-account reach for a relay's restricted accounts (peer_auth.relays'
// restricted, with exclude and only; plan networked-relay): the shapes of
// reach_shapes_test.go, forwarded by a relay. The projects are that file's:
// example-ops, example-pipeline and example-vault open, example-desk
// confidential, example-sealed sealed, lockFin and example-locked-open
// locked.
//
// The uids are chosen to collide: a rule is found by the relay and the uid
// together, so one uid means a different account on this machine, on
// example-laptop and on example-studio.

const (
	rrLaptop = "example-laptop"
	rrStudio = "example-studio"
)

// rrLaptopUIDs are example-laptop's restricted accounts, by name there. bare
// has the uid of this machine's shaped _example_desk (513), excl that of this
// machine's _example_excl (511) under another exclude list.
var rrLaptopUIDs = map[string]uint32{
	"bare": 513, "excl": 511, "multi": 612, "desk": 613, "mixed": 614, "lockd": 615, "seal": 616, "ghost": 617, "twin": 618,
}

func rrPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled: true, Owner: "owner",
		ForceLocked: []string{lockFin, rsLockedOp},
		Restricted: []config.RestrictedAccount{
			{Account: "_example_desk", Only: []string{rsDesk}},
			{Account: "_example_excl", Exclude: []string{rsPipeline, rsVault}},
		},
		Relays: map[string]config.PeerRelay{
			rrLaptop: {OwnerUID: judgeRelayUID, Restricted: map[string]config.RelayRestricted{
				"bare":  {UID: rrLaptopUIDs["bare"]},
				"excl":  {UID: rrLaptopUIDs["excl"], Exclude: []string{rsOps}},
				"multi": {UID: rrLaptopUIDs["multi"], Only: []string{rsOps, rsPipeline}},
				"desk":  {UID: rrLaptopUIDs["desk"], Only: []string{rsDesk}},
				"mixed": {UID: rrLaptopUIDs["mixed"], Only: []string{rsDesk, rsOps}},
				"lockd": {UID: rrLaptopUIDs["lockd"], Only: []string{lockFin}},
				"seal":  {UID: rrLaptopUIDs["seal"], Only: []string{rsSealed}},
				"ghost": {UID: rrLaptopUIDs["ghost"], Only: []string{"example-nowhere"}},
				"twin":  {UID: rrLaptopUIDs["twin"], Only: []string{rsDesk}}, // B-D10: two accounts, one project
			}},
			// The same uids on another computer: its own entries, its own rules.
			rrStudio: {OwnerUID: judgeRelayUID, Restricted: map[string]config.RelayRestricted{
				"desk":  {UID: rrLaptopUIDs["desk"]},                          // 613: plain here
				"other": {UID: rrLaptopUIDs["bare"], Only: []string{rsVault}}, // 513: shaped here
			}},
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

// rrView is the view of the account named account on relay's computer, with
// the projects' states of reach_shapes_test.go.
func rrView(t *testing.T, p *Policy, device, relay string, uid uint32) View {
	t.Helper()
	v, ok := p.RelayChainViewNamed(device, relay, uid, []ProcessToken{judgeTok(2200)}, nil)
	require.True(t, ok, "%s uid %d", relay, uid)
	return v.WithReach(rsOpen, rsFenced)
}

// The reach rule per shape, for an account behind a relay: the rule of
// TestRestricted_ReachShapes, row for row.
func TestRelayRestricted_ReachShapes(t *testing.T) {
	p := rrPolicy(t)
	for _, tc := range []struct {
		account string
		want    []string
		shaped  bool
	}{
		{"bare", []string{rsOps, rsPipeline, rsVault}, false},
		{"excl", []string{rsPipeline, rsVault}, true},
		{"multi", []string{rsOps, rsPipeline}, true},
		{"desk", []string{rsDesk}, true},
		{"mixed", []string{rsOps}, true}, // a fenced project beside others is not reached
		{"lockd", []string{}, true},      // a lock closes even the one project
		{"seal", []string{rsSealed}, true},
		{"ghost", []string{}, true}, // an unregistered project is never reached
		{"twin", []string{rsDesk}, true},
	} {
		v := rrView(t, p, judgeLaptop, rrLaptop, rrLaptopUIDs[tc.account])
		got, ok := v.Allowed()
		require.True(t, ok, tc.account)
		require.Equal(t, tc.want, got, tc.account)
		require.Equal(t, tc.shaped, v.ReachShaped(), tc.account)
		require.Equal(t, RelayAccountLabel(tc.account, rrLaptop), v.Account(), tc.account)
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
		require.False(t, v.Principal().HasProcess, "%s: no chain, so no lease, grant or request", tc.account)
		require.Equal(t, tc.want, append([]string{}, p.RelayReachOf(rrLaptop, rrLaptopUIDs[tc.account], rsOpen, rsFenced)...),
			"%s: doctor's reach is the view's", tc.account)
	}
	require.Equal(t, len(rrLaptopUIDs), len(p.RelayRestrictedEntries(rrLaptop)), "every account of the relay has a row above")
}

// A rule is found by the relay and the uid together: one uid is a different
// account, under a different rule, on this machine, on one relay's computer
// and on another's.
func TestRelayRestricted_RuleIsKeyedByRelayAndUID(t *testing.T) {
	p := rrPolicy(t)
	reach := func(v View) []string {
		got, _ := v.WithReach(rsOpen, rsFenced).Allowed()
		return got
	}
	// uid 513: only [example-desk] here, plain on example-laptop, only
	// [example-vault] on example-studio.
	local, ok := p.PeerView(513)
	require.True(t, ok)
	require.Equal(t, []string{rsDesk}, reach(local))
	require.Equal(t, []string{rsOps, rsPipeline, rsVault}, reach(rrView(t, p, judgeLaptop, rrLaptop, 513)))
	require.False(t, rrView(t, p, judgeLaptop, rrLaptop, 513).ReachShaped())
	require.Equal(t, []string{rsVault}, reach(rrView(t, p, judgeDesk, rrStudio, 513)))

	// uid 511: two exclude lists, this machine's and the laptop's.
	local, ok = p.PeerView(511)
	require.True(t, ok)
	require.Equal(t, []string{rsOps}, reach(local))
	require.Equal(t, []string{rsPipeline, rsVault}, reach(rrView(t, p, judgeLaptop, rrLaptop, 511)))

	// uid 613: only [example-desk] on the laptop, plain on the studio, no
	// account at all here.
	require.Equal(t, []string{rsDesk}, reach(rrView(t, p, judgeLaptop, rrLaptop, 613)))
	studio := rrView(t, p, judgeDesk, rrStudio, 613)
	require.Equal(t, []string{rsOps, rsPipeline, rsVault}, reach(studio))
	require.False(t, studio.ReachShaped())
	_, ok = p.PeerView(613)
	require.False(t, ok)

	// The device id names no entry: the rule follows the enrolled name the
	// transport matched, whatever device presented it.
	require.Equal(t, []string{rsDesk}, reach(rrView(t, p, judgeDesk, rrLaptop, 613)))

	// A relay's view the policy never built -- no entry to read a rule from
	// -- reaches nothing, never the plain reach.
	for name, v := range map[string]View{
		"no relay name":     {policy: p, who: Principal{Kind: KindRestricted, UID: 613, HasUID: true, Socket: true, Relay: judgeLaptop}},
		"an unmapped relay": {policy: p, relayName: "example-gone", who: Principal{Kind: KindRestricted, UID: 613, HasUID: true, Socket: true, Relay: judgeLaptop}},
		"an unlisted uid":   {policy: p, relayName: rrLaptop, who: Principal{Kind: KindRestricted, UID: 999, HasUID: true, Socket: true, Relay: judgeLaptop}},
	} {
		require.Empty(t, reach(v), name)
		require.False(t, v.ReachShaped(), name)
	}
	require.Nil(t, p.RelayReachOf(rrLaptop, 999, rsOpen, rsFenced))
	require.Nil(t, p.RelayReachOf("example-gone", 613, rsOpen, rsFenced))
}

// The invariant that lets clearance stay out of restricted accounts holds
// behind a relay too: a reach that holds a confidential or sealed project
// holds nothing else, for every shape.
func TestRelayRestricted_ReachInvariant(t *testing.T) {
	p := rrPolicy(t)
	fenced := map[string]bool{}
	for _, s := range rsFenced {
		fenced[s] = true
	}
	for _, relay := range p.Relays() {
		for _, e := range p.RelayRestrictedEntries(relay) {
			got := p.RelayReachOf(relay, e.Account.UID, rsOpen, rsFenced)
			for _, slug := range got {
				if fenced[slug] {
					require.Len(t, got, 1, "%s's reach holds fenced %s and more: %v", e.Account.Name, slug, got)
				}
			}
		}
	}
}

// Reach follows the projects' states at the request (B-D6), behind a relay as
// on this socket.
func TestRelayRestricted_ReachFollowsStates(t *testing.T) {
	p := rrPolicy(t)
	view := func(account string) View {
		v, ok := p.RelayChainView(judgeLaptop, rrLaptop, rrLaptopUIDs[account], nil)
		require.True(t, ok)
		return v
	}
	got, _ := view("excl").WithReach([]string{rsVault}, []string{rsOps}).Allowed()
	require.Equal(t, []string{rsVault}, got, "an excluded project stays excluded once confidential")

	for _, states := range []struct{ open, fenced []string }{{[]string{rsDesk}, nil}, {nil, []string{rsDesk}}} {
		got, _ := view("desk").WithReach(states.open, states.fenced).Allowed()
		require.Equal(t, []string{rsDesk}, got)
	}
	got, _ = view("desk").WithReach(rsOpen, nil).Allowed()
	require.Empty(t, got, "a project no longer registered is not reached")

	got, _ = view("multi").WithReach([]string{rsOps}, []string{rsPipeline}).Allowed()
	require.Equal(t, []string{rsOps}, got, "a project that turned confidential leaves a multi-project only")

	// A transport that loaded only the open projects never reaches the desk's
	// confidential project: the reach needs the fenced ones too.
	got, _ = view("desk").WithOpenProjects(rsOpen).Allowed()
	require.Empty(t, got)
}

// Texts (B-D9), behind a relay: a plain entry keeps the texts it had before
// shapes, word for word; a shaped one gets the neutral set, under its label.
func TestRelayRestricted_ShapedTexts(t *testing.T) {
	p := rrPolicy(t)
	bare := rrView(t, p, judgeLaptop, rrLaptop, rrLaptopUIDs["bare"])
	require.EqualError(t, bare.Refusal(rsDesk), "project example-desk is not reachable from account bare@example-laptop: a restricted "+
		"account reaches open projects that are not locked, and reads the global scope; it cannot create a project either -- the "+
		"owner creates and maps them")
	require.EqualError(t, bare.Refusal(""), "the global scope is read-only for account bare@example-laptop: a restricted account reads "+
		"it and writes only into open projects -- name one with project=<slug>")
	require.Equal(t, RestrictedNoRequests("bare@example-laptop"),
		RestrictedNoRequestsFor(WithView(context.Background(), bare), "bare@example-laptop"))
	require.False(t, ReachShaped(WithView(context.Background(), bare)))

	excl := rrView(t, p, judgeLaptop, rrLaptop, rrLaptopUIDs["excl"])
	want := func(project string) string {
		return "project " + project + " is not reachable from account excl@example-laptop: the owner gives this account only the " +
			"projects project_list shows; it cannot create a project either -- the owner creates and maps them"
	}
	for _, slug := range []string{rsOps, rsDesk, lockFin, "example-nowhere"} { // excluded, confidential, locked, unregistered
		err := excl.Refusal(slug)
		require.EqualError(t, err, want(slug))
		require.True(t, errors.Is(err, ErrNotReachable))
	}
	require.EqualError(t, excl.Refusal(""), "account excl@example-laptop does not write the global scope: it writes only into the "+
		"projects project_list shows -- name one with project=<slug>")
	ctx := WithView(context.Background(), excl)
	require.True(t, ReachShaped(ctx))
	msg := RestrictedNoRequestsFor(ctx, "excl@example-laptop")
	require.Contains(t, msg, "the owner gives this account only the projects project_list shows")
	stateWord := regexp.MustCompile(`\b(open|confidential|sealed|locked)\b`)
	for _, text := range []string{msg, excl.Refusal(rsOps).Error(), excl.Refusal("").Error()} {
		require.False(t, stateWord.MatchString(text), "a shaped text names no isolation state: %s", text)
	}

	// The owner through the same relay is not shaped, whatever the relay maps.
	owner, ok := p.RelayChainView(judgeLaptop, rrLaptop, judgeRelayUID, []ProcessToken{judgeTok(2200)})
	require.True(t, ok)
	require.False(t, ReachShaped(WithView(context.Background(), owner)))
}

// A disabled entry (B-D7) behind a relay is refused at the door; every other
// account -- the same uid on this machine and on another relay included -- is
// served, and the policy it came from is unchanged.
func TestRelayRestricted_WithRelayDisabled(t *testing.T) {
	p := rrPolicy(t)
	d := p.WithRelayDisabled(rrLaptop, []uint32{rrLaptopUIDs["excl"], rrLaptopUIDs["desk"], 9999})
	for _, account := range []string{"excl", "desk"} {
		_, ok := d.RelayChainView(judgeLaptop, rrLaptop, rrLaptopUIDs[account], nil)
		require.False(t, ok, account)
		require.True(t, d.RelayRestrictedDisabled(rrLaptop, rrLaptopUIDs[account]), account)
		_, ok = p.RelayChainView(judgeLaptop, rrLaptop, rrLaptopUIDs[account], nil)
		require.True(t, ok, "WithRelayDisabled copies: %s", account)
		require.False(t, p.RelayRestrictedDisabled(rrLaptop, rrLaptopUIDs[account]))
	}
	require.False(t, d.RelayRestrictedDisabled(rrLaptop, 9999), "an unlisted uid is not a disabled entry")
	require.False(t, d.RelayRestrictedDisabled(rrLaptop, judgeRelayUID), "nor is the owner")

	_, ok := d.RelayChainView(judgeLaptop, rrLaptop, rrLaptopUIDs["bare"], nil)
	require.True(t, ok, "another account of that relay")
	_, ok = d.RelayChainView(judgeLaptop, rrLaptop, judgeRelayUID, nil)
	require.True(t, ok, "the owner through that relay")
	// uid 613 on the studio, and uid 511 here, are other accounts.
	_, ok = d.RelayChainView(judgeDesk, rrStudio, rrLaptopUIDs["desk"], nil)
	require.True(t, ok)
	require.False(t, d.RelayRestrictedDisabled(rrStudio, rrLaptopUIDs["desk"]))
	_, ok = d.PeerView(511)
	require.True(t, ok)
	require.False(t, d.RestrictedDisabled(511))
	// And this machine's WithDisabled never reaches a relay's account.
	local := p.WithDisabled([]uint32{511})
	_, ok = local.RelayChainView(judgeLaptop, rrLaptop, 511, nil)
	require.True(t, ok)
	_, ok = local.PeerView(511)
	require.False(t, ok)

	var disabled []string
	for _, e := range d.RelayRestrictedEntries(rrLaptop) {
		if e.Disabled {
			disabled = append(disabled, e.Account.Name)
		}
	}
	require.Equal(t, []string{"excl@example-laptop", "desk@example-laptop"}, disabled)
	require.Equal(t, p, p.WithRelayDisabled("example-gone", []uint32{613}), "a relay the policy does not map changes nothing")
}

// RelayRestrictedEntries and Relays, which doctor and the startup log read.
func TestRelayRestricted_Entries(t *testing.T) {
	p := rrPolicy(t)
	require.Equal(t, []string{rrLaptop, rrStudio}, p.Relays())
	byName := map[string]RestrictedEntry{}
	for _, e := range p.RelayRestrictedEntries(rrLaptop) {
		require.Equal(t, rrLaptop, e.Relay)
		byName[e.NameThere()] = e
	}
	require.False(t, byName["bare"].Shaped())
	require.Equal(t, Account{UID: 513, Name: "bare@example-laptop"}, byName["bare"].Account)
	require.Equal(t, []string{rsOps}, byName["excl"].Exclude)
	require.Equal(t, []string{rsDesk, rsOps}, byName["mixed"].Only, "sorted")
	require.Empty(t, p.RelayRestrictedEntries("example-gone"))

	// This machine's entries name no relay, and their name is the account's.
	for _, e := range p.RestrictedEntries() {
		require.Empty(t, e.Relay)
		require.Equal(t, e.Account.Name, e.NameThere())
	}
}

// A block built in code is held to the two rules whose breach would widen a
// relay's account: never exclude with only, never an empty only.
func TestRelayRestricted_NewRefusesWideningShapes(t *testing.T) {
	resolve := func(n string) (Account, error) { return rsAccounts[n], nil }
	for name, e := range map[string]config.RelayRestricted{
		"both":       {UID: 613, Exclude: []string{rsVault}, Only: []string{rsOps}},
		"empty only": {UID: 613, Only: []string{}},
	} {
		_, err := New(config.PeerAuth{Enabled: true, Owner: "owner",
			Relays: map[string]config.PeerRelay{rrLaptop: {OwnerUID: judgeRelayUID, Restricted: map[string]config.RelayRestricted{"desk": e}}}}, resolve)
		require.ErrorIs(t, err, ErrInvalidPolicy, name)
	}
}
