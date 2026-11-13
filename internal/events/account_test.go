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

// accountCtxs are a request of the restricted account _example_agent and one of
// the owner, under one peer_auth policy.
func accountCtxs(t *testing.T) (restricted, owner context.Context) {
	t.Helper()
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
	return access.WithView(context.Background(), v.WithOpenProjects([]string{"example-ops"})),
		access.WithView(context.Background(), p.OwnerView())
}

// Every event a restricted account's request records names the account in its
// payload -- through Record and RecordOnce, stored and published alike, on any
// surface -- and no other caller's event changes: the owner's, or background
// work's with no View.
func TestRecord_NamesARestrictedAccount(t *testing.T) {
	restricted, owner := accountCtxs(t)
	r := newRecorder(t)
	live, unsub := r.Subscribe()
	defer unsub()
	stored := func(id string) map[string]any {
		t.Helper()
		e, found, err := r.ByID(context.Background(), id)
		require.NoError(t, err)
		require.True(t, found)
		return e.Payload
	}

	payload := map[string]any{"stage": "session-start"}
	id, err := r.Record(restricted, core.Event{Kind: core.EventHookError, Payload: payload})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"stage": "session-start", PayloadAccount: "_example_agent"}, stored(id),
		"a session-less event names the account")
	require.Equal(t, map[string]any{"stage": "session-start"}, payload, "the caller's map is never written")
	require.Equal(t, "_example_agent", (<-live).Payload[PayloadAccount], "the live fan-out carries it too")

	id, err = r.Record(restricted, core.Event{Kind: core.EventSessionStarted, SessionID: "S1"})
	require.NoError(t, err)
	require.Equal(t, map[string]any{PayloadAccount: "_example_agent"}, stored(id), "a nil payload gains one")
	<-live

	id, err = r.Record(restricted, core.Event{Kind: core.EventAccessRefused, Payload: map[string]any{
		"reason": "peer.owner_only", PayloadAccount: "_example_other"}})
	require.NoError(t, err)
	require.Equal(t, "_example_other", stored(id)[PayloadAccount], "an account the payload already names stands")
	<-live

	onceID, recorded, err := r.RecordOnce(restricted, core.Event{Kind: core.EventMemoryFirstReuse, ItemID: "item-1"})
	require.NoError(t, err)
	require.True(t, recorded)
	require.Equal(t, map[string]any{PayloadAccount: "_example_agent"}, stored(onceID), "RecordOnce names it too")
	<-live

	for name, ctx := range map[string]context.Context{"the owner": owner, "background work": context.Background()} {
		id, err := r.Record(ctx, core.Event{Kind: core.EventHookError, Payload: map[string]any{"stage": "x"}})
		require.NoError(t, err)
		require.Equal(t, map[string]any{"stage": "x"}, stored(id), "%s: unchanged", name)
		<-live
		id, err = r.Record(ctx, core.Event{Kind: core.EventSessionStarted})
		require.NoError(t, err)
		require.Empty(t, stored(id), "%s: no payload, none added", name)
		<-live
	}
}

// stampAccount passes every payload but a restricted account's through as the
// very map it was given; a restricted account's is a copy.
func TestStampAccount(t *testing.T) {
	restricted, owner := accountCtxs(t)
	payload := map[string]any{"tool": "memory_write"}
	for _, ctx := range []context.Context{owner, context.Background()} {
		got := stampAccount(ctx, payload)
		got["probe"] = true
		require.Contains(t, payload, "probe", "passed through, not copied")
		delete(payload, "probe")
		require.Nil(t, stampAccount(ctx, nil))
	}
	got := stampAccount(restricted, payload)
	require.Equal(t, map[string]any{"tool": "memory_write", PayloadAccount: "_example_agent"}, got)
	require.Equal(t, map[string]any{"tool": "memory_write"}, payload)
	require.Equal(t, map[string]any{PayloadAccount: "_example_agent"}, stampAccount(restricted, map[string]any{PayloadAccount: ""}),
		"an empty name is no name")
}
