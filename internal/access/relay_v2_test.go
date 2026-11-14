package access

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Wire version 2 and restricted accounts on a relay's computer (plan
// networked-relay): the programs a relay names ride the owner's chain, and a
// uid the relay's restricted list names is that account, as on this socket.

// relay2Policy locks example-finance and maps example-laptop with the owner
// at 501 and one restricted account, example-agent at 502; example-desk maps
// the owner alone.
func relay2Policy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled: true, Owner: "owner", ForceLocked: []string{lockFin}, Restricted: []string{"_example_agent"},
		Relays: map[string]config.PeerRelay{
			"example-laptop": {OwnerUID: judgeRelayUID, Restricted: map[string]uint32{"example-agent": 502}},
			"example-desk":   {OwnerUID: judgeRelayUID},
		},
	}, restrictedResolve)
	require.NoError(t, err)
	return p
}

func TestRelayV2_ProgramsRideTheOwnersChain(t *testing.T) {
	p := relay2Policy(t)
	chain := []ProcessToken{judgeTok(2210), judgeTok(2200), judgeTok(2100)}
	programs := []string{"/Users/owner/.local/bin/seam", "/Users/owner/.local/share/claude/versions/2.1.319", ""}
	v, ok := p.RelayChainViewNamed(judgeLaptop, "example-laptop", judgeRelayUID, chain, programs)
	require.True(t, ok)
	who := v.Principal()
	require.True(t, who.Named())
	require.Equal(t, programs, who.Programs)
	got, ok := who.ReportedProgram(who.Chain[1])
	require.True(t, ok)
	require.Equal(t, programs[1], got)
	got, ok = who.ReportedProgram(who.Chain[2])
	require.True(t, ok, "named: and the kernel named nothing for it")
	require.Empty(t, got)
	_, ok = who.ReportedProgram(judgeTok(2200))
	require.False(t, ok, "this machine's process of the same pid is not the relay's")
	programs[0] = "changed"
	require.Equal(t, "/Users/owner/.local/bin/seam", v.Principal().Programs[0], "the caller's slice is copied")

	// Version 1: no programs, so nothing is named.
	v1, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, chain)
	require.True(t, ok)
	require.False(t, v1.Principal().Named())
	_, ok = v1.Principal().ReportedProgram(v1.Principal().Chain[0])
	require.False(t, ok)

	// Programs that do not match the chain one for one are not attached.
	short, ok := p.RelayChainViewNamed(judgeLaptop, "example-laptop", judgeRelayUID, chain, programs[:2])
	require.True(t, ok)
	require.True(t, short.Principal().HasProcess, "the chain is the relay's report, attached as before")
	require.False(t, short.Principal().Named())

	// This machine's socket names nothing here: its kernel is the daemon's own.
	local, ok := p.PeerChainView(501, []ProcessToken{judgeTok(2200)})
	require.True(t, ok)
	require.False(t, local.Principal().Named())
}

// At a lock, a relay that names programs is a socket the agent asks through
// as it would here: the refusal's remedy is the socket's, not the version 1
// relay's.
func TestRelayV2_LockedRefusalOfANamingRelay(t *testing.T) {
	p := relay2Policy(t)
	v, ok := p.RelayChainViewNamed(judgeLaptop, "example-laptop", judgeRelayUID, []ProcessToken{judgeTok(2200)},
		[]string{"/usr/local/bin/claude"})
	require.True(t, ok)
	err := v.Refusal(lockFin)
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "Remedy: run `seam access request example-finance` from the agent's shell")
	require.NotContains(t, err.Error(), "wire version 1")
}

// A restricted account on a relay's computer is judged as one on this socket:
// the open projects that are not locked, read and write; the global scope
// read-only; nothing else; labeled with its name and the relay's; no chain,
// so no lease, grant or request; its session namespace reserved from the
// owner.
func TestRelayV2_ARestrictedAccountOnAClientComputer(t *testing.T) {
	p := relay2Policy(t)
	require.True(t, p.HasRestricted())
	require.Equal(t, []Account{{UID: 502, Name: "example-agent@example-laptop"}}, p.RelayRestrictedAccounts("example-laptop"))
	require.Empty(t, p.RelayRestrictedAccounts("example-desk"))

	chain := []ProcessToken{judgeTok(2210), judgeTok(2200)}
	v, ok := p.RelayChainViewNamed(judgeLaptop, "example-laptop", 502, chain, []string{"/x/seam", "/x/claude"})
	require.True(t, ok)
	who := v.Principal()
	require.Equal(t, KindRestricted, who.Kind)
	require.Equal(t, "example-agent@example-laptop", who.User)
	require.True(t, who.Socket)
	require.Equal(t, judgeLaptop, who.Relay)
	require.False(t, who.HasProcess, "no chain: no lease, grant or request")
	require.Empty(t, who.Chain)
	require.False(t, who.Named())
	require.Equal(t, "example-agent@example-laptop", v.Account(), "the label every write carries: the account and the host")

	v = v.WithOpenProjects([]string{lockOps, lockFin, "example-open"})
	require.True(t, v.Writable(lockOps))
	require.True(t, v.Writable("example-open"))
	require.False(t, v.Visible(lockFin), "a locked project is never in its reach")
	require.False(t, v.Visible(lockLegal), "a project that is not open")
	require.True(t, v.Visible(""), "the global scope reads")
	require.False(t, v.Writable(""), "and never writes")
	err := v.Refusal(lockLegal)
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "project example-legal is not reachable from account example-agent@example-laptop")
	require.Equal(t, v, v.WithLeases(lockNow, []Lease{{ID: "x", Project: lockFin, Process: judgeTok(2200).Key(), Expires: lockNow.Add(1)}}),
		"no lease attaches to it")

	ctx := WithView(context.Background(), v)
	require.Equal(t, "example-agent@example-laptop", AccountOf(ctx))
	require.Equal(t, "example-agent@example-laptop:cc/abc", QualifySessionName(ctx, "cc/abc"))
	require.False(t, OwnerOnly(ctx))
	require.True(t, SessionAccountOK(ctx, "example-agent@example-laptop"))
	require.False(t, SessionAccountOK(ctx, "_example_agent"), "the account of the same uid on this machine is another account")
	require.False(t, SessionAccountOK(ctx, ""), "nor the owner's sessions")

	// The owner may not start a session in its namespace.
	owner, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, chain)
	require.True(t, ok)
	acct, reserved := ReservedSessionName(WithView(context.Background(), owner), "example-agent@example-laptop:cc/abc")
	require.True(t, reserved)
	require.Equal(t, "example-agent@example-laptop", acct)
	_, reserved = ReservedSessionName(WithView(context.Background(), owner), "example-agent:cc/abc")
	require.False(t, reserved)

	// The same uid on another relay's computer, or one no list names, is
	// refused; this machine's 502 is this machine's account.
	_, ok = p.RelayChainView(judgeDesk, "example-desk", 502, chain)
	require.False(t, ok, "example-desk maps no restricted account")
	_, ok = p.RelayChainView(judgeLaptop, "example-laptop", 503, chain)
	require.False(t, ok)
	local, ok := p.PeerView(502)
	require.True(t, ok)
	require.Equal(t, "_example_agent", local.Account())
}

// A relay's restricted list that did not come through a config load is held
// to the load's shape, and no label is two accounts'.
func TestRelayV2_RestrictedShape(t *testing.T) {
	for name, tc := range map[string]struct {
		relays     map[string]config.PeerRelay
		restricted []string
	}{
		"root":              {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x": 0}}}},
		"the owner's uid":   {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x": 501}}}},
		"one uid twice":     {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x": 502, "y": 502}}}},
		"a name with @":     {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x@y": 502}}}},
		"a name with :":     {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x:y": 502}}}},
		"a name with space": {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x y": 502}}}},
		"no name":           {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"": 502}}}},
		"a dash first":      {relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"-x": 502}}}},
		"a local account labeled like it": {
			relays:     map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501, Restricted: map[string]uint32{"x": 502}}},
			restricted: []string{"x@example-laptop"},
		},
	} {
		resolve := func(n string) (Account, error) {
			if n == "x@example-laptop" {
				return Account{UID: 600, Name: n}, nil
			}
			return restrictedResolve(n)
		}
		_, err := New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: tc.restricted, Relays: tc.relays}, resolve)
		require.ErrorIs(t, err, ErrInvalidPolicy, name)
	}
}
