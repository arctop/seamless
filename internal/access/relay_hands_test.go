package access

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// rhPolicy maps the relay example-laptop, the owner uid 501 there and a
// restricted account 502.
func rhPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New(config.PeerAuth{Owner: "owner", Relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501,
		Restricted: config.BareRelayRestricted(map[string]uint32{"example-agent": 502})}}},
		func(n string) (Account, error) { return Account{UID: 501, Name: n}, nil })
	require.NoError(t, err)
	return p
}

// The console asks OwnerHandsThrough before it lets a relay's request make
// the owner's decision: only a judgment for that relay that said owner, on
// the owner's socket view through that same relay, with the programs the
// relay named, passes.
func TestOwnerHandsThrough(t *testing.T) {
	const relay, other = "01RELAYEXAMPLE", "01OTHERRELAY"
	p := rhPolicy(t)
	chain := []ProcessToken{{PID: 4100, Version: 7, Boot: "b"}, {PID: 4000, Version: 3, Boot: "b"}}
	programs := []string{"/Users/owner/.local/bin/seam", "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"}
	owner, ok := p.RelayChainViewNamed(relay, "example-laptop", 501, chain, programs)
	require.True(t, ok)
	v1, ok := p.RelayChainView(relay, "example-laptop", 501, chain)
	require.True(t, ok)
	restricted, ok := p.RelayChainViewNamed(relay, "example-laptop", 502, chain, programs)
	require.True(t, ok)
	yes := RelayHands{Relay: relay, Name: "example-laptop", Programs: programs}
	no := RelayHands{Relay: relay, Name: "example-laptop", Programs: programs, Why: "an agent started it"}

	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"no judgment":                 {ctx: WithView(context.Background(), owner)},
		"a judgment that says no":     {ctx: WithRelayHands(WithView(context.Background(), owner), no)},
		"another relay's judgment":    {ctx: WithRelayHands(WithView(context.Background(), owner), RelayHands{Relay: other, Name: "example-desk"})},
		"no view (peer_auth off)":     {ctx: WithRelayHands(context.Background(), yes)},
		"the TCP owner's view":        {ctx: WithRelayHands(WithView(context.Background(), p.OwnerView()), yes)},
		"a restricted account's view": {ctx: WithRelayHands(WithView(context.Background(), restricted), yes)},
		"a version 1 relay's view":    {ctx: WithRelayHands(WithView(context.Background(), v1), yes)},
		"the owner through the relay": {ctx: WithRelayHands(WithView(context.Background(), owner), yes), want: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := OwnerHandsThrough(tc.ctx, relay)
			require.Equal(t, tc.want, ok)
			if tc.want {
				require.Equal(t, yes, got)
			}
			_, ok = OwnerHandsThrough(tc.ctx, other)
			require.False(t, ok, "never for another relay")
			_, ok = OwnerHandsThrough(tc.ctx, "")
			require.False(t, ok, "never for no relay")
		})
	}

	// The judgment the context carries is its own copy.
	ctx := WithRelayHands(WithView(context.Background(), owner), yes)
	programs[0] = "/Users/owner/.local/share/claude/versions/2.1.319"
	got, ok := OwnerHandsThrough(ctx, relay)
	require.True(t, ok)
	require.Equal(t, "/Users/owner/.local/bin/seam", got.Programs[0])
	require.False(t, RelayHands{}.Owner(), "an empty judgment names no relay")
}
