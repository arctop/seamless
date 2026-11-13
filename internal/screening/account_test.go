package screening_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/screening"
)

// restrictedCtx is a request of the restricted account _example_agent, and
// ownerCtx one of the owner, under one peer_auth policy.
func restrictedCtx(t *testing.T) (restricted, owner context.Context) {
	t.Helper()
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent"}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_example_agent":
				return access.Account{UID: 502, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	v, ok := policy.PeerView(502)
	require.True(t, ok)
	return access.WithView(context.Background(), v), access.WithView(context.Background(), policy.OwnerView())
}

// access.refused names the restricted account that was refused, from its
// request or given outright, and one account's refusals are never folded
// into another's (or the owner's) count.
func TestRefused_NamesTheRestrictedAccount(t *testing.T) {
	restricted, owner := restrictedCtx(t)
	f := newCorpus(t)
	f.on.Store(true)
	reach := screening.Refusal{Reason: screening.RefusedPeerUnreach, Surface: "mcp", Tool: "memory_read", Target: "example-conf"}
	f.scr.RecordRefused(restricted, reach)
	f.scr.RecordRefused(owner, reach)
	f.scr.RecordRefused(restricted, reach)
	given := screening.Refusal{Reason: screening.RefusedOwnerOnly, Surface: "console", Account: "_example_agent"}
	f.scr.RecordRefused(context.Background(), given)

	ev := f.events(t, core.EventAccessRefused)
	require.Len(t, ev, 3, "the owner's refusal is its own key; the account's second is suppressed")
	require.Equal(t, "_example_agent", ev[0][events.PayloadAccount])
	require.Equal(t, "peer.unreachable", ev[0]["reason"])
	require.NotContains(t, ev[1], events.PayloadAccount, "the owner's refusal names no account")
	require.Equal(t, "peer.owner_only", ev[2]["reason"])
	require.Equal(t, "_example_agent", ev[2][events.PayloadAccount], "given outright, for a ctx without the View")

	f.scr.RecordScreened(restricted, screening.Screened{ItemKind: "memory", Kind: core.KindGotcha, Tool: "memory_write", Size: 3})
	f.scr.RecordScreened(owner, screening.Screened{ItemKind: "memory", Kind: core.KindGotcha, Tool: "memory_write", Size: 3})
	sc := f.events(t, core.EventMemoryScreened)
	require.Len(t, sc, 2)
	require.Equal(t, "_example_agent", sc[0][events.PayloadAccount], "the recorder names the account on every event")
	require.NotContains(t, sc[1], events.PayloadAccount)
}
