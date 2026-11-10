package access

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Restricted local accounts (peer_auth.restricted): open projects read-write,
// the global scope read-only, nothing else, and no lease ever.

var restrictedAccounts = map[string]Account{
	"owner":          {UID: 501, Name: "owner", Home: "/Users/owner"},
	"_example_agent": {UID: 502, Name: "_example_agent", Home: "/Users/_example_agent"},
	"502":            {UID: 502, Name: "_example_agent", Home: "/Users/_example_agent"},
	"_other_agent":   {UID: 503, Name: "_other_agent"},
	"501":            {UID: 501, Name: "owner"},
}

func restrictedResolve(name string) (Account, error) {
	if a, ok := restrictedAccounts[name]; ok {
		return a, nil
	}
	return Account{}, fmt.Errorf("unknown account %q", name)
}

func restrictedPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled:     true,
		Owner:       "owner",
		ForceLocked: []string{lockFin},
		Restricted:  []string{"_example_agent"},
	}, restrictedResolve)
	require.NoError(t, err)
	return p
}

func TestRestricted_PolicyResolves(t *testing.T) {
	p := restrictedPolicy(t)
	require.True(t, p.HasRestricted())
	require.Equal(t, []Account{restrictedAccounts["_example_agent"]}, p.RestrictedAccounts())

	for _, tc := range []struct {
		name string
		cfg  config.PeerAuth
		want string
	}{
		{"the owner by uid", config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"501"}}, "the owner's"},
		{"one account twice", config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent", "502"}}, "twice"},
		{"unknown", config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_nobody"}}, "unknown account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, restrictedResolve)
			require.ErrorIs(t, err, ErrInvalidPolicy)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRestricted_Reach(t *testing.T) {
	p := restrictedPolicy(t)
	plain, ok := p.PeerView(502)
	require.True(t, ok)
	require.Equal(t, KindRestricted, plain.Principal().Kind)
	require.Equal(t, "restricted", plain.Principal().Kind.String())
	require.True(t, plain.Principal().Socket)
	require.Equal(t, "_example_agent", plain.Account())
	require.Equal(t, "/Users/_example_agent", plain.HomeFor())

	// Unfilled, the view reaches the global scope read-only and nothing else.
	require.True(t, plain.Visible(""))
	require.False(t, plain.Writable(""))
	require.False(t, plain.Visible("example-open"))
	allowed, bounded := plain.Allowed()
	require.True(t, bounded)
	require.Empty(t, allowed)

	// The transport hands in every open project; the policy drops the locked one.
	v := plain.WithOpenProjects([]string{"example-open", "example-docs", lockFin, ""})
	allowed, bounded = v.Allowed()
	require.True(t, bounded)
	require.Equal(t, []string{"example-docs", "example-open"}, allowed)
	require.True(t, v.Restricted())
	require.Empty(t, v.Hidden(), "a restricted account is bounded by its reach, not a deny list")

	for _, tc := range []struct {
		project         string
		visible, writes bool
	}{
		{"example-open", true, true},
		{"example-docs", true, true},
		{"", true, false},                      // global: read-only
		{lockFin, false, false},                // locked, even though its isolation was open
		{"example-confidential", false, false}, // not among the open projects
		{"no-such-project", false, false},      // cannot create one
	} {
		require.Equal(t, tc.visible, v.Visible(tc.project), "visible %q", tc.project)
		require.Equal(t, tc.writes, v.Writable(tc.project), "writable %q", tc.project)
		require.False(t, v.Locked(tc.project), "a restricted account never sees a lock it could ask about: %q", tc.project)
		require.Equal(t, tc.visible, v.Listed(tc.project), "listed %q", tc.project)
	}

	// The refusal reads the same for a confidential project, a locked one and a
	// slug nobody registered.
	refuse := func(project string) string {
		err := v.Refusal(project)
		require.ErrorIs(t, err, ErrNotReachable)
		return err.Error()
	}
	want := func(project string) string {
		return "project " + project + " is not reachable from account _example_agent: a restricted account reaches open " +
			"projects that are not locked, and reads the global scope; it cannot create a project either -- the owner " +
			"creates and maps them"
	}
	require.Equal(t, want("example-confidential"), refuse("example-confidential"))
	require.Equal(t, want(lockFin), refuse(lockFin))
	require.Equal(t, want("no-such-project"), refuse("no-such-project"))
	require.Contains(t, refuse(""), "the global scope is read-only for account _example_agent")

	// Sessions land only in a project it reaches; global is refused, not kept.
	got, err := v.SessionScope("example-open")
	require.NoError(t, err)
	require.Equal(t, "example-open", got)
	_, err = v.SessionScope("")
	require.ErrorIs(t, err, ErrNotReachable)
	_, err = v.SessionScope("example-confidential")
	require.ErrorIs(t, err, ErrNotReachable)
}

func TestRestricted_NoLeaseNoGrantNoConsole(t *testing.T) {
	p := restrictedPolicy(t)
	tok := ProcessToken{PID: 900, Version: 1, Boot: "boot-a"}
	v, ok := p.PeerChainView(502, []ProcessToken{tok})
	require.True(t, ok)
	v = v.WithOpenProjects([]string{"example-open", lockFin})
	now := time.Now()
	leased := v.WithLeases(now, []Lease{{ID: "l1", Project: lockFin, Process: tok.Key(), Expires: now.Add(time.Hour)}})
	require.False(t, leased.Visible(lockFin), "a lease never opens a locked project for a restricted account")
	require.Empty(t, leased.Leases())

	ctx := WithView(context.Background(), v)
	g := JudgeGrants(ctx, "sess-1", now, []Grant{{ID: "g1", From: "example-open", Project: "example-confidential", Process: tok.Key(), Session: "sess-1",
		Expires: now.Add(time.Hour)}})
	require.Empty(t, g.Live(), "a restricted account holds no read grant")

	require.False(t, OwnerOnly(ctx), "a restricted account has no console")
	require.True(t, PeerAuthenticated(ctx))
	require.Equal(t, "_example_agent", AccountOf(ctx))

	owner := WithView(context.Background(), p.OwnerView())
	require.True(t, OwnerOnly(owner))
	require.Empty(t, AccountOf(owner), "the owner's writes carry no account label")
	require.Empty(t, AccountOf(context.Background()))

	// Sessions belong to the principal that started them.
	require.True(t, SessionAccountOK(ctx, "_example_agent"))
	require.False(t, SessionAccountOK(ctx, ""), "a restricted account never acts in the owner's session")
	require.False(t, SessionAccountOK(ctx, "_other_agent"))
	require.True(t, SessionAccountOK(owner, ""))
	require.False(t, SessionAccountOK(owner, "_example_agent"), "the owner never acts in a restricted account's session")
	require.True(t, SessionAccountOK(context.Background(), ""))
	require.ErrorIs(t, SessionAccountRefusal("cc/x"), ErrNotReachable)
}

func TestRestricted_UnlistedUIDStillRefused(t *testing.T) {
	p := restrictedPolicy(t)
	_, ok := p.PeerView(503)
	require.False(t, ok, "an unlisted uid is refused at the door")
	other := p.OwnerView().WithOpenProjects([]string{"example-open"})
	require.Empty(t, other.Account(), "WithOpenProjects leaves every other view unchanged")
	require.True(t, other.Visible("example-confidential"))
}

// A restricted account's session names live in a namespace of their own
// (SEAL A2); the owner's are read as given, and may not be started inside an
// account's namespace.
func TestRestricted_SessionNamespace(t *testing.T) {
	require.Equal(t, "_example_agent:sess/x", QualifiedSessionName("_example_agent", "sess/x"))
	require.Equal(t, "_example_agent:sess/x", QualifiedSessionName("_example_agent", "_example_agent:sess/x"),
		"qualifying twice changes nothing")
	require.Equal(t, "_example_agent:_other_agent:x", QualifiedSessionName("_example_agent", "_other_agent:x"),
		"another account's prefix is just part of the name")
	require.Equal(t, "sess/x", QualifiedSessionName("", "sess/x"), "the owner's names are as given")
	require.Empty(t, QualifiedSessionName("_example_agent", ""), "no name stays no name")

	p := restrictedPolicy(t)
	v, ok := p.PeerView(502)
	require.True(t, ok)
	acct := WithView(context.Background(), v.WithOpenProjects([]string{"example-open"}))
	owner := WithView(context.Background(), p.OwnerView())
	off := context.Background()

	require.Equal(t, "_example_agent:cc/abc", QualifySessionName(acct, "cc/abc"))
	require.Equal(t, "_example_agent:", SessionNamespace(acct))
	for _, ctx := range []context.Context{owner, off} {
		require.Equal(t, "cc/abc", QualifySessionName(ctx, "cc/abc"))
		require.Empty(t, SessionNamespace(ctx))
	}

	account, reserved := ReservedSessionName(owner, "_example_agent:sess/squat")
	require.True(t, reserved, "the owner may not start a session in the account's namespace")
	require.Equal(t, "_example_agent", account)
	err := ReservedSessionNameRefusal("_example_agent:sess/squat", account)
	require.ErrorIs(t, err, ErrNotReachable)
	require.Equal(t, "session name _example_agent:sess/squat is in restricted account _example_agent's namespace: "+
		"names beginning _example_agent: are its sessions -- choose another name", err.Error())
	for _, c := range []struct {
		ctx  context.Context
		name string
	}{
		{owner, "sess/mine"},
		{owner, "_other_agent:x"}, // not a restricted account
		{owner, "_example_agentx:y"},
		{acct, "_example_agent:sess/x"}, // its own, qualified already
		{off, "_example_agent:sess/x"},  // peer_auth off: no policy, no reservation
	} {
		_, reserved := ReservedSessionName(c.ctx, c.name)
		require.False(t, reserved, c.name)
	}
}
