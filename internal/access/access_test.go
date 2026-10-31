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
	"katata":          {UID: 501, Name: "katata", Home: "/Users/katata"},
	"501":             {UID: 501, Name: "katata", Home: "/Users/katata"},
	"_arctop_finance": {UID: 470, Name: "_arctop_finance"},
	"470":             {UID: 470, Name: "_arctop_finance"},
	"_arctop_legal":   {UID: 471, Name: "_arctop_legal"},
	"_arctop_hq":      {UID: 475, Name: "_arctop_hq"},
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
		Enabled: true,
		Owner:   "katata",
		Users: map[string]config.PeerUser{
			"_arctop_finance": {Sealed: []string{"arctop-finance"}},
			"_arctop_legal":   {Sealed: []string{"arctop-legal"}},
			"_arctop_hq":      {Projects: []string{"arctop-hq", "arctop-ops"}},
		},
	}, testResolve)
	require.NoError(t, err)
	return p
}

func TestView_Matrix(t *testing.T) {
	p := testPolicy(t)
	owner := p.OwnerView()
	ownerSock, ok := p.PeerView(501)
	require.True(t, ok)
	fin, ok := p.PeerView(470)
	require.True(t, ok)
	hq, ok := p.PeerView(475)
	require.True(t, ok)

	for _, tc := range []struct {
		name            string
		v               View
		project         string
		visible, writes bool
	}{
		{"owner reads an open project", owner, "arctop-ops", true, true},
		{"owner reads global", owner, "", true, true},
		{"owner never reaches a project sealed to another account", owner, "arctop-finance", false, false},
		{"owner on the socket is the same owner", ownerSock, "arctop-legal", false, false},
		{"owner reaches an unknown slug (not sealed)", owner, "brand-new", true, true},
		{"a sealed account reaches its own project", fin, "arctop-finance", true, true},
		{"a sealed account reads global", fin, "", true, false},
		{"a sealed account never reaches the owner's projects", fin, "arctop-ops", false, false},
		{"a sealed account never reaches another account's seal", fin, "arctop-legal", false, false},
		{"a shared account reaches its shared projects", hq, "arctop-ops", true, true},
		{"a shared account does not reach a seal", hq, "arctop-finance", false, false},
		{"no view reaches everything", View{}, "arctop-finance", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.visible, tc.v.Visible(tc.project), "visible")
			require.Equal(t, tc.writes, tc.v.Writable(tc.project), "writable")
		})
	}
}

func TestView_HiddenAllowedHome(t *testing.T) {
	p := testPolicy(t)
	owner := p.OwnerView()
	require.Equal(t, []string{"arctop-finance", "arctop-legal"}, owner.Hidden())
	_, bounded := owner.Allowed()
	require.False(t, bounded)
	require.True(t, owner.Restricted())
	require.False(t, owner.Principal().Socket, "the TCP owner is not kernel-authenticated")

	fin, _ := p.PeerView(470)
	allowed, bounded := fin.Allowed()
	require.True(t, bounded)
	require.Equal(t, []string{"arctop-finance"}, allowed)
	require.Nil(t, fin.Hidden())
	require.Equal(t, "arctop-finance", fin.Home())
	require.True(t, fin.Principal().Socket)
	require.Equal(t, KindUser, fin.Principal().Kind)

	hq, _ := p.PeerView(475)
	require.Equal(t, "arctop-hq", hq.Home(), "no seal: the first shared project is home")

	_, ok := p.PeerView(9999)
	require.False(t, ok, "an unconfigured uid has no view")
	require.False(t, View{}.Restricted())
}

func TestView_SessionScopeAndRefusal(t *testing.T) {
	p := testPolicy(t)
	fin, _ := p.PeerView(470)
	for _, in := range []string{"", "arctop-ops", "arctop-legal", "somewhere"} {
		got, err := fin.SessionScope(in)
		require.NoError(t, err)
		require.Equal(t, "arctop-finance", got, "a sealed account's session lands in its own project from %q", in)
	}
	got, err := fin.SessionScope("arctop-finance")
	require.NoError(t, err)
	require.Equal(t, "arctop-finance", got)

	owner := p.OwnerView()
	got, err = owner.SessionScope("arctop-ops")
	require.NoError(t, err)
	require.Equal(t, "arctop-ops", got)
	_, err = owner.SessionScope("arctop-finance")
	require.ErrorIs(t, err, ErrNotReachable)
	require.NotContains(t, err.Error(), "_arctop_finance", "a refusal names no other account")

	err = fin.Refusal("")
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "read-only")
	var re *RefusalError
	require.True(t, errors.As(fin.Refusal("arctop-ops"), &re))
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	require.True(t, Visible(ctx, "arctop-finance"), "no view in ctx reaches everything")
	require.True(t, Writable(ctx, ""))
	require.False(t, PeerAuthenticated(ctx))
	require.True(t, OwnerOnly(ctx))

	p := testPolicy(t)
	octx := WithView(ctx, p.OwnerView())
	require.False(t, Visible(octx, "arctop-finance"))
	require.False(t, PeerAuthenticated(octx), "the TCP owner still needs the key")
	require.True(t, OwnerOnly(octx))

	fin, _ := p.PeerView(470)
	fctx := WithView(ctx, fin)
	require.True(t, PeerAuthenticated(fctx))
	require.False(t, OwnerOnly(fctx))
	require.False(t, Writable(fctx, ""))
	got, err := SessionScope(fctx, "")
	require.NoError(t, err)
	require.Equal(t, "arctop-finance", got)
}

func TestNew_ResolutionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.PeerAuth
		want string
	}{
		{"owner does not resolve", config.PeerAuth{Owner: "nobody-here"}, "peer_auth.owner"},
		{"user does not resolve", config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{"ghost": {Sealed: []string{"x"}}}}, "peer_auth.users.ghost"},
		{"user resolves to the owner's uid", config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{"501": {Sealed: []string{"x"}}}}, "the owner's"},
		{"one uid listed twice", config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{
			"_arctop_finance": {Sealed: []string{"a"}}, "470": {Sealed: []string{"b"}},
		}}, "uid 470 twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, testResolve)
			require.ErrorIs(t, err, ErrInvalidPolicy)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestPolicy_Listings(t *testing.T) {
	p := testPolicy(t)
	require.Equal(t, []string{"arctop-finance", "arctop-legal"}, p.SealedProjects())
	require.Equal(t, []string{"arctop-finance", "arctop-hq", "arctop-legal", "arctop-ops"}, p.Projects())
	rows := p.Accounts()
	require.Len(t, rows, 3)
	require.Equal(t, uint32(470), rows[0].Account.UID)
	require.Equal(t, "katata", p.Owner().Name)
	require.Equal(t, "/Users/katata", p.OwnerView().HomeFor())
}
