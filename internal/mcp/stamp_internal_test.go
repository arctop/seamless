package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// A read names a stamp's project only when the caller's View may list it: a
// restricted account reaches open projects alone and is told how many stamps
// it may not be told about, never their names. The owner, and every install
// without peer_auth, sees them all.
func TestShownStamps_RestrictedAccountsAreNeverToldAnotherProjectsName(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent"}},
		func(name string) (access.Account, error) {
			switch name {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_example_agent":
				return access.Account{UID: 470, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("no account %s", name)
		})
	require.NoError(t, err)
	restricted, ok := policy.PeerView(470)
	require.True(t, ok)
	restricted = restricted.WithOpenProjects([]string{"example-ops", "example-docs"})
	stamps := []core.Stamp{
		{Project: "example-docs", Via: core.StampViaGrant, Ref: "01A"},
		{Project: "example-legal", Via: core.StampViaGrant, Ref: "01B"},
		{Project: "example-ops", Via: core.StampViaGrant, Ref: "01OWN"},
	}

	shown, withheld := shownStamps(access.WithView(context.Background(), restricted), stamps, "example-ops")
	require.Equal(t, []core.Stamp{stamps[0]}, shown)
	require.Equal(t, 1, withheld)
	out := map[string]any{}
	putStamps(access.WithView(context.Background(), restricted), out, stamps, "example-ops")
	require.Equal(t, map[string]any{mayContainKey: []core.Stamp{stamps[0]}, mayContainWithheldKey: 1}, out)

	for name, ctx := range map[string]context.Context{
		"no peer_auth": context.Background(),
		"the owner":    access.WithView(context.Background(), policy.OwnerView()),
	} {
		shown, withheld := shownStamps(ctx, stamps, "example-ops")
		require.Equal(t, stamps[:2], shown, name)
		require.Zero(t, withheld, name)
	}
	empty := map[string]any{}
	putStamps(context.Background(), empty, nil, "example-ops")
	require.Empty(t, empty, "no stamp, no key")
}

// core's short form of an approval is the request's own short id, as the
// Access requests page and seam approve name it.
func TestStampShortRef_IsTheRequestsShortID(t *testing.T) {
	for _, id := range []string{"01M7999M7MBPEKDYS4P1XBJC4D", "01M79FJBCPRF2WWPHWE592Y3C1", "ABC"} {
		for _, via := range []string{core.StampViaGrant, core.StampViaMove, ""} {
			require.Equal(t, approval.ShortID(id), core.Stamp{Project: "p", Via: via, Ref: id}.ShortRef(), id)
		}
	}
}
