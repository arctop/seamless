package events

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// A restricted local account has no home project, and the owner reads
// everything it does, so its events keep exactly the project they were
// recorded with -- the global scope included.
func TestRecord_LeavesARestrictedAccountsEventsAlone(t *testing.T) {
	r := newRecorder(t)
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent"}},
		func(name string) (access.Account, error) {
			switch name {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_example_agent":
				return access.Account{UID: 502, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", name)
		})
	require.NoError(t, err)
	v, ok := p.PeerView(502)
	require.True(t, ok)
	ctx := access.WithView(context.Background(), v.WithOpenProjects([]string{"example-ops"}))
	require.Equal(t, "_example_agent", access.AccountOf(ctx))

	for _, project := range []string{"", "example-ops", "example-finance"} {
		id, err := r.Record(ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: project, Payload: map[string]any{"tool": "recall"}})
		require.NoError(t, err)
		e, found, err := r.ByID(context.Background(), id)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, project, e.ProjectSlug, "project %q is kept", project)

		onceID, recorded, err := r.RecordOnce(ctx, core.Event{Kind: core.EventMemoryFirstReuse, ItemID: "item-" + id, ProjectSlug: project})
		require.NoError(t, err)
		require.True(t, recorded)
		e, _, err = r.ByID(context.Background(), onceID)
		require.NoError(t, err)
		require.Equal(t, project, e.ProjectSlug, "RecordOnce keeps it too")
	}
}
