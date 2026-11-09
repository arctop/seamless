package access

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

var testAccounts = map[string]Account{
	"owner":          {UID: 501, Name: "owner", Home: "/home/owner"},
	"501":            {UID: 501, Name: "owner", Home: "/home/owner"},
	"_example_agent": {UID: 470, Name: "_example_agent"},
}

func testResolve(name string) (Account, error) {
	if a, ok := testAccounts[name]; ok {
		return a, nil
	}
	return Account{}, fmt.Errorf("unknown account %q", name)
}

func testPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled:    true,
		Owner:      "owner",
		Locked:     []string{"example-finance", "example-legal"},
		Restricted: []string{"_example_agent"},
	}, testResolve)
	require.NoError(t, err)
	return p
}

// testAgent is the restricted account's view with its reach filled in, the way
// the transport fills it: the locked slug handed in is dropped by the policy.
func testAgent(t *testing.T, p *Policy) View {
	t.Helper()
	v, ok := p.PeerView(470)
	require.True(t, ok)
	return v.WithOpenProjects([]string{"example-ops", "example-finance"})
}

func TestView_Matrix(t *testing.T) {
	p := testPolicy(t)
	owner := p.OwnerView()
	ownerSock, ok := p.PeerView(501)
	require.True(t, ok)
	agent := testAgent(t, p)

	for _, tc := range []struct {
		name            string
		v               View
		project         string
		visible, writes bool
	}{
		{"owner reads an open project", owner, "example-ops", true, true},
		{"owner reads global", owner, "", true, true},
		{"owner never reaches a locked project without a lease", owner, "example-finance", false, false},
		{"owner on the socket is the same owner", ownerSock, "example-legal", false, false},
		{"owner reaches an unknown slug (not locked)", owner, "brand-new", true, true},
		{"a restricted account reaches an open project", agent, "example-ops", true, true},
		{"a restricted account reads global and never writes it", agent, "", true, false},
		{"a restricted account never reaches a locked project", agent, "example-finance", false, false},
		{"a restricted account reaches no unknown slug", agent, "brand-new", false, false},
		{"no view reaches everything", View{}, "example-finance", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.visible, tc.v.Visible(tc.project), "visible")
			require.Equal(t, tc.writes, tc.v.Writable(tc.project), "writable")
		})
	}
}

func TestView_HiddenAllowed(t *testing.T) {
	p := testPolicy(t)
	owner := p.OwnerView()
	require.Equal(t, []string{"example-finance", "example-legal"}, owner.Hidden())
	_, bounded := owner.Allowed()
	require.False(t, bounded)
	require.True(t, owner.Restricted())
	require.False(t, owner.Principal().Socket, "the TCP owner is not kernel-authenticated")

	agent := testAgent(t, p)
	allowed, bounded := agent.Allowed()
	require.True(t, bounded)
	require.Equal(t, []string{"example-ops"}, allowed)
	require.Nil(t, agent.Hidden())
	require.True(t, agent.Principal().Socket)
	require.Equal(t, KindRestricted, agent.Principal().Kind)

	_, ok := p.PeerView(9999)
	require.False(t, ok, "a uid the policy does not list has no view: it is refused at the door")
	require.False(t, View{}.Restricted())
}

func TestView_SessionScopeAndRefusal(t *testing.T) {
	p := testPolicy(t)
	agent := testAgent(t, p)
	got, err := agent.SessionScope("example-ops")
	require.NoError(t, err)
	require.Equal(t, "example-ops", got)
	for _, in := range []string{"", "example-finance", "somewhere"} {
		_, err := agent.SessionScope(in)
		require.ErrorIs(t, err, ErrNotReachable, "a restricted account has no project to be placed in from %q", in)
	}

	owner := p.OwnerView()
	got, err = owner.SessionScope("example-ops")
	require.NoError(t, err)
	require.Equal(t, "example-ops", got)
	_, err = owner.SessionScope("example-finance")
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "is locked", "the owner is only ever refused at a lock")
	var re *RefusalError
	require.True(t, errors.As(owner.Refusal("example-finance"), &re))

	err = agent.Refusal("")
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "read-only")
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	require.True(t, Visible(ctx, "example-finance"), "no view in ctx reaches everything")
	require.True(t, Writable(ctx, ""))
	require.False(t, PeerAuthenticated(ctx))
	require.True(t, OwnerOnly(ctx))

	p := testPolicy(t)
	octx := WithView(ctx, p.OwnerView())
	require.False(t, Visible(octx, "example-finance"))
	require.False(t, PeerAuthenticated(octx), "the TCP owner still needs the key")
	require.True(t, OwnerOnly(octx))

	actx := WithView(ctx, testAgent(t, p))
	require.True(t, PeerAuthenticated(actx))
	require.False(t, OwnerOnly(actx))
	require.False(t, Writable(actx, ""))
	got, err := SessionScope(actx, "example-ops")
	require.NoError(t, err)
	require.Equal(t, "example-ops", got)
}

func TestNew_ResolutionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.PeerAuth
		want string
	}{
		{"owner does not resolve", config.PeerAuth{Owner: "nobody-here"}, "peer_auth.owner"},
		{"a filled-in users key", config.PeerAuth{Owner: "owner", Users: map[string]any{"_example_agent": nil}},
			"peer_auth.users was the 2026-10-31 design"},
		{"users beside locked", config.PeerAuth{Owner: "owner", Locked: []string{"x"}, Users: map[string]any{"_a": "x"}},
			"peer_auth.users was the 2026-10-31 design"},
		{"users beside restricted", config.PeerAuth{Owner: "owner", Restricted: []string{"_example_agent"}, Users: map[string]any{"_a": "x"}},
			"peer_auth.users was the 2026-10-31 design"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, testResolve)
			require.ErrorIs(t, err, ErrInvalidPolicy)
			require.Contains(t, err.Error(), tc.want)
			if len(tc.cfg.Users) > 0 {
				require.ErrorIs(t, err, config.ErrPeerAuthUsersRetired)
			}
		})
	}
}

func TestPolicy_Listings(t *testing.T) {
	p := testPolicy(t)
	require.Equal(t, []string{"example-finance", "example-legal"}, p.ConsoleHidden())
	require.Equal(t, p.OwnerLocked(), p.ConsoleHidden(), "the locked projects are the whole hidden set")
	require.Equal(t, "owner", p.Owner().Name)
	require.Equal(t, "/home/owner", p.OwnerView().HomeFor())
}
