package access

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Requests through a relay (plan networked-relay): a relay peer_auth.relays
// maps is judged as this machine's socket is, and every key it carries names
// the relay, so nothing keyed on one relay's process ever applies to another
// relay's or to this machine's.

const (
	judgeLaptop   = "01JUDGELAPTOPRELAY0000000A" // the mapped relay's device id
	judgeDesk     = "01JUDGEDESKRELAY000000000B" // a second mapped relay
	judgeRelayUID = 501                          // the owner's uid on the relays' computers
)

// judgeRelayBoot is a relay computer's boot.
const judgeRelayBoot = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

func judgeTok(pid int32) ProcessToken {
	return ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: judgeRelayBoot}
}

// judgePolicy locks example-finance and maps two relays, the owner being uid
// 501 on both computers.
func judgePolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{
		Enabled: true, Owner: "owner", ForceLocked: []string{lockFin},
		Relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: judgeRelayUID}, "example-desk": {OwnerUID: judgeRelayUID}},
	}, lockResolve)
	require.NoError(t, err)
	return p
}

func TestProcessToken_ARelaysProcess(t *testing.T) {
	local := judgeTok(2200)
	relayed := local
	relayed.Relay = judgeLaptop
	require.Equal(t, judgeRelayBoot+":2200:2200000", local.Key(), "this machine's process, as a relay forwards its own")
	require.Equal(t, "relay/"+judgeLaptop+"/"+judgeRelayBoot+":2200:2200000", relayed.Key())
	require.True(t, strings.HasPrefix(relayed.Key(), RelayKeyPrefix(judgeLaptop)))
	require.Equal(t, local.AgentID(), relayed.AgentID(), "an agent process identity is the process's, on its own computer")
	require.Equal(t, judgeLaptop, RelayOfKey(relayed.Key()))
	require.Empty(t, RelayOfKey(local.Key()))
	require.Empty(t, RelayOfKey("relay/"+judgeLaptop), "a key that is no relay's process key names no relay")

	other := local
	other.Relay = judgeDesk
	require.NotEqual(t, relayed.Key(), other.Key(), "two relays' processes never share a key")
	require.NotEqual(t, local.Key(), relayed.Key())

	for _, bad := range []string{"a/b", "a b", "a:b", "-x", strings.Repeat("A", 65)} {
		tok := local
		tok.Relay = bad
		require.False(t, tok.Valid(), "relay %q", bad)
		require.Empty(t, tok.Key())
	}
	require.Empty(t, ProgramOf(relayed), "this kernel names no process of another computer")
}

func TestParseProcessKey(t *testing.T) {
	tok := judgeTok(2200)
	got, err := ParseProcessKey(tok.Key())
	require.NoError(t, err)
	require.Equal(t, tok, got)
	got, err = ParseProcessKey("ABC-def:1:0")
	require.NoError(t, err, "a version of zero is a version, and a boot may hold capitals")
	require.Equal(t, ProcessToken{PID: 1, Version: 0, Boot: "ABC-def"}, got)

	relayed := tok
	relayed.Relay = judgeLaptop
	for _, bad := range []string{
		"", "boot", "boot:1", "boot:1:2:3", ":1:2", "boot:0:2", "boot:-1:2", "boot:+1:2", "boot:01:2",
		"boot:1:02", "boot:1:+2", "boot:1:-2", "boot:2147483648:2", "boot:1:18446744073709551616",
		" boot:1:2", "boot:1:2 ", "bo ot:1:2", "bo/ot:1:2", relayed.Key(),
	} {
		_, err := ParseProcessKey(bad)
		require.ErrorIs(t, err, ErrProcessKey, "%q", bad)
	}
}

func TestPolicy_RelayChainView(t *testing.T) {
	p := judgePolicy(t)
	require.True(t, p.RelayMapped("example-laptop"))
	require.False(t, p.RelayMapped("example-studio"))

	chain := []ProcessToken{judgeTok(2210), judgeTok(2200), judgeTok(2100)}
	v, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, chain)
	require.True(t, ok)
	who := v.Principal()
	require.Equal(t, KindOwner, who.Kind)
	require.True(t, who.Socket, "a socket the relay's kernel authenticated")
	require.True(t, who.HasUID)
	require.EqualValues(t, judgeRelayUID, who.UID)
	require.Equal(t, judgeLaptop, who.Relay)
	require.True(t, who.HasProcess)
	require.Len(t, who.Chain, 3)
	for i, tok := range who.Chain {
		require.Equal(t, judgeLaptop, tok.Relay, "every process of the chain is the relay's")
		require.Equal(t, chain[i].Key(), strings.TrimPrefix(tok.Key(), RelayKeyPrefix(judgeLaptop)))
	}
	require.Empty(t, chain[0].Relay, "the caller's slice is left alone")
	require.True(t, PeerAuthenticated(WithView(context.Background(), v)))
	require.True(t, OwnerOnly(WithView(context.Background(), v)))

	for name, tc := range map[string]struct {
		relay, name string
		uid         uint32
	}{
		"a relay the policy does not map": {judgeLaptop, "example-studio", judgeRelayUID},
		"another uid on a mapped relay":   {judgeLaptop, "example-laptop", 502},
		"root on a mapped relay":          {judgeLaptop, "example-laptop", 0},
		"no relay id":                     {"", "example-laptop", judgeRelayUID},
		"a relay id that is no id":        {"a/b", "example-laptop", judgeRelayUID},
	} {
		_, ok := p.RelayChainView(tc.relay, tc.name, tc.uid, chain)
		require.False(t, ok, name)
	}

	long := make([]ProcessToken, MaxChainHops+1)
	for i := range long {
		long[i] = judgeTok(int32(3000 + i))
	}
	stolen := judgeTok(2200)
	stolen.Relay = judgeDesk
	for name, c := range map[string][]ProcessToken{
		"empty":                   nil,
		"too long":                long,
		"two boots":               {judgeTok(2210), {PID: 2200, Version: 1, Boot: "0f0e0d0c-0000-4000-8000-000000000001"}},
		"an invalid process":      {judgeTok(2210), {PID: 0, Version: 1, Boot: judgeRelayBoot}},
		"another relay's process": {judgeTok(2210), stolen},
	} {
		v, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, c)
		require.True(t, ok, name)
		require.False(t, v.Principal().HasProcess, "%s: not attached", name)
		require.Empty(t, v.Principal().Chain, name)
		require.Equal(t, judgeLaptop, v.Principal().Relay, name)
	}

	// This machine's socket never carries a relay's process.
	relayed := judgeTok(2200)
	relayed.Relay = judgeLaptop
	sv, ok := p.PeerChainView(501, []ProcessToken{relayed})
	require.True(t, ok)
	require.False(t, sv.Principal().HasProcess)
}

// A lease granted to one relay's process applies to that relay's chain alone:
// not to this machine's process of the same pid, start and boot, not to
// another relay forwarding the same chain, and not to a sibling on the same
// relay.
func TestRelayView_LeasesKeyOnTheRelayAndTheProcess(t *testing.T) {
	p := judgePolicy(t)
	agentA, agentB := judgeTok(2200), judgeTok(2300)
	granted := agentA
	granted.Relay = judgeLaptop
	lease := Lease{ID: "lease-a", Project: lockFin, Process: granted.Key(), Expires: lockNow.Add(time.Hour)}

	a, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, []ProcessToken{judgeTok(2210), agentA})
	require.True(t, ok)
	a = a.WithLeases(lockNow, []Lease{lease})
	require.True(t, a.Visible(lockFin), "the leased agent's tree, through its relay")
	require.True(t, a.Writable(lockFin))

	b, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, []ProcessToken{judgeTok(2310), agentB})
	require.True(t, ok)
	b = b.WithLeases(lockNow, []Lease{lease})
	require.False(t, b.Visible(lockFin), "a sibling agent on the same computer")
	require.True(t, b.Locked(lockFin))

	desk, ok := p.RelayChainView(judgeDesk, "example-desk", judgeRelayUID, []ProcessToken{judgeTok(2210), agentA})
	require.True(t, ok)
	require.False(t, desk.WithLeases(lockNow, []Lease{lease}).Visible(lockFin), "another relay forwarding the same chain")

	local, ok := p.PeerChainView(501, []ProcessToken{judgeTok(2210), agentA})
	require.True(t, ok)
	require.False(t, local.WithLeases(lockNow, []Lease{lease}).Visible(lockFin), "this machine's process of the same pid and start")
	localLease := Lease{ID: "lease-local", Project: lockFin, Process: agentA.Key(), Expires: lockNow.Add(time.Hour)}
	a2, _ := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, []ProcessToken{judgeTok(2210), agentA})
	require.False(t, a2.WithLeases(lockNow, []Lease{localLease}).Visible(lockFin), "nor a relay a local lease")
}

// A grant keyed on one relay's process holds for that relay's chain only.
func TestRelayView_GrantsKeyOnTheRelayAndTheProcess(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	p := judgePolicy(t)
	agent := judgeTok(2200)
	granted := agent
	granted.Relay = judgeLaptop
	rows := []Grant{
		{ID: "relay", Project: clLegal, From: clFin, Process: granted.Key(), Expires: now.Add(time.Hour)},
		{ID: "local", Project: clLegal, From: clFin, Process: agent.Key(), Expires: now.Add(time.Hour)},
	}
	c := Conversation{Home: clFin, Label: []string{clFin, clLegal}, Grants: rows}

	v, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, []ProcessToken{judgeTok(2210), agent})
	require.True(t, ok)
	g := JudgeConversation(WithView(context.Background(), v), now, c)
	require.Len(t, g.Live(), 1)
	require.Equal(t, "relay", g.Live()[0].ID)
	require.Len(t, JudgeGrants(WithView(context.Background(), v), "", now, rows).Live(), 1)

	local, ok := p.PeerChainView(501, []ProcessToken{judgeTok(2210), agent})
	require.True(t, ok)
	lg := JudgeConversation(WithView(context.Background(), local), now, c)
	require.Len(t, lg.Live(), 1)
	require.Equal(t, "local", lg.Live()[0].ID)

	desk, ok := p.RelayChainView(judgeDesk, "example-desk", judgeRelayUID, []ProcessToken{judgeTok(2210), agent})
	require.True(t, ok)
	require.Empty(t, JudgeConversation(WithView(context.Background(), desk), now, c).Live())
}

// At a lock, a relay's agent hears why nothing is filed for it, and its
// chain, which names no program, counts as an agent's for the owner's own
// decisions.
func TestRelayView_LockedRefusal(t *testing.T) {
	p := judgePolicy(t)
	v, ok := p.RelayChainView(judgeLaptop, "example-laptop", judgeRelayUID, []ProcessToken{judgeTok(2200)})
	require.True(t, ok)
	err := v.Refusal(lockFin)
	require.ErrorIs(t, err, ErrNotReachable)
	require.Contains(t, err.Error(), "project example-finance is locked")
	require.Contains(t, err.Error(), "an agent on another computer")
	require.Contains(t, err.Error(), "\nRemedy: work in example-finance from an agent session on the server's own computer")
	require.NotContains(t, err.Error(), "TCP connection", "a relay is no bearer request")

	ctx := WithAgentChain(WithView(context.Background(), v), func() bool { return true })
	require.True(t, FromAgent(ctx))
}

// A relays block that did not come through a config load is held to the
// load's shape.
func TestNew_RelaysShape(t *testing.T) {
	for name, relays := range map[string]map[string]config.PeerRelay{
		"root as the owner":   {"example-laptop": {OwnerUID: 0}},
		"an upper-case name":  {"Example-Laptop": {OwnerUID: 501}},
		"a name with a space": {"example-laptop ": {OwnerUID: 501}},
		"no name":             {"": {OwnerUID: 501}},
	} {
		_, err := New(config.PeerAuth{Enabled: true, Owner: "owner", Relays: relays}, lockResolve)
		require.ErrorIs(t, err, ErrInvalidPolicy, name)
	}
}
