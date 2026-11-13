package feed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// A restricted local account's events name it on the frame's actor: the
// account that started the event's session, or, with no session, the one its
// request spoke for (the payload key the recorders stamp). Its session is
// named by its qualified name, "<account>:<name>", which may start with "_".
// The owner's frames are untouched (TestFrames_OwnerGolden).
func TestFrames_NameARestrictedAccount(t *testing.T) {
	m := testMapper()
	ts := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	const acct = "_example_agent"
	qualified := acct + ":cc/0a1b2c3d-4e5f6a7b"
	restricted := session{name: qualified, agentProcess: "4343.1730000001", account: acct}
	marshal := func(f Frame) string {
		t.Helper()
		require.NoError(t, f.validate())
		raw, err := json.Marshal(f)
		require.NoError(t, err)
		return string(raw)
	}

	t.Run("in its own session", func(t *testing.T) {
		f := m.frame(core.Event{ID: "01KX0000000000000000000R01", TS: ts, Kind: core.EventMemoryWritten, SessionID: "S2",
			ProjectSlug: "myapp", ItemID: "01KX0000000000000000000M02", Payload: map[string]any{"kind": "gotcha", "updated": false}},
			restricted)
		require.Equal(t, `{"time":"2026-11-12T09:00:00.000Z","host":"testhost","class":"agent","activity":"memory_written",`+
			`"severity":"info","outcome":"success","actor":{"kind":"agent","id":"4343.1730000001",`+
			`"session":"_example_agent:cc/0a1b2c3d-4e5f6a7b","account":"_example_agent"},`+
			`"target":{"system":"seamless","asset":"myapp"},"fields":{"event_id":"01KX0000000000000000000R01",`+
			`"item_id":"01KX0000000000000000000M02","kind":"gotcha","updated":false}}`, marshal(f))
	})

	t.Run("with no session: the request's account", func(t *testing.T) {
		f := m.frame(core.Event{ID: "01KX0000000000000000000R02", TS: ts, Kind: core.EventAccessRefused, ProjectSlug: "myapp",
			Payload: map[string]any{"reason": "peer.unreachable", "surface": "mcp", "tool": "memory_read",
				"target": "vault-project", events.PayloadAccount: acct}}, session{})
		require.Equal(t, &Actor{Kind: "agent", Account: acct}, f.Actor)
		require.NotContains(t, f.Fields, "account", "the account is the actor's, never a field")
		require.Equal(t, "peer.unreachable", f.Fields["reason"])
		require.True(t, strings.HasPrefix(f.Fields["target_project"].(string), "h:"))
		marshal(f)
	})

	t.Run("never the owner, whatever the payload says", func(t *testing.T) {
		f := m.frame(core.Event{ID: "01KX0000000000000000000R03", TS: ts, Kind: core.EventMemoryReleased,
			Payload: map[string]any{"by": "owner", events.PayloadAccount: acct}}, session{})
		require.Equal(t, &Actor{Kind: "agent", Account: acct}, f.Actor)
		f = m.frame(core.Event{ID: "01KX0000000000000000000R04", TS: ts, Kind: core.EventToolCall,
			Payload: map[string]any{"tool": "tasks_list", events.PayloadAccount: 42}}, session{})
		require.Equal(t, &Actor{Kind: "system", ID: targetSystem}, f.Actor, "a payload account that is no name is none")
	})

	t.Run("its session's account wins over the payload's", func(t *testing.T) {
		f := m.frame(core.Event{ID: "01KX0000000000000000000R05", TS: ts, Kind: core.EventToolCall, SessionID: "S2",
			Payload: map[string]any{"tool": "tasks_list", events.PayloadAccount: "_other"}}, restricted)
		require.Equal(t, acct, f.Actor.Account)
		require.Equal(t, qualified, f.Actor.Session)
	})

	t.Run("a session whose lookup failed keeps the request's account", func(t *testing.T) {
		f := m.frame(core.Event{ID: "01KX0000000000000000000R06", TS: ts, Kind: core.EventToolCall, SessionID: "S2",
			Payload: map[string]any{"tool": "tasks_list", events.PayloadAccount: acct}}, session{})
		require.Equal(t, &Actor{Kind: "agent", Account: acct}, f.Actor)
	})

	t.Run("a name outside the pattern travels as its hash", func(t *testing.T) {
		odd := "agent$box"
		f := m.frame(core.Event{ID: "01KX0000000000000000000R07", TS: ts, Kind: core.EventMemoryWritten, SessionID: "S3",
			Payload: map[string]any{"kind": "gotcha"}}, session{name: odd + ":cc/1111-2222", account: odd})
		require.Regexp(t, `^h:[0-9a-f]{24}$`, f.Actor.Account, "never dropped: the frame must not read as the owner's")
		require.Equal(t, accountRef(odd), f.Actor.Account, "stable")
		require.Empty(t, f.Actor.Session, "nor is the qualified name carried with an account off the pattern")
		marshal(f)
	})
}

// sessionName carries a session's name only on the pattern: a bare name for
// anyone, the qualified form only for the session of the account it names --
// so an owner's session whose name merely looks qualified is dropped exactly
// as before.
func TestSessionName(t *testing.T) {
	long := "_example_agent:cc/" + strings.Repeat("a", 128-len("_example_agent:cc/"))
	for _, tc := range []struct {
		name, account string
		ok            bool
	}{
		{"cc/0a1b2c3d-4e5f6a7b", "", true},
		{"cx/0f0f0f0f-1e1e1e1e", "", true},
		{"sess/01KX0000000000000000000000", "", true},
		{"example_agent:cc/abc", "example_agent", true}, // already on the bare pattern
		{"_example_agent:cc/abc", "_example_agent", true},
		{"_example_agent:sess/my-run.1", "_example_agent", true},
		{long, "_example_agent", true},
		{long + "a", "_example_agent", false},      // past 128 in all
		{"_example_agent:cc/abc", "", false},       // an owner's session: the bare pattern alone
		{"_example_agent:cc/abc", "_other", false}, // another account's prefix
		{"_example_agent:", "_example_agent", false},
		{"_example_agent:_x", "_example_agent", false}, // the rest is held to the bare pattern
		{"_example_agent:cc/a b", "_example_agent", false},
		{"a name with spaces", "", false},
		{"", "", false},
	} {
		got, ok := sessionName(tc.name, tc.account)
		require.Equal(t, tc.ok, ok, "%q (account %q)", tc.name, tc.account)
		if ok {
			require.Equal(t, tc.name, got)
		}
	}
}

// The contract's rule on actor.account: a name on the account pattern or the
// hash that stands in for one; anything else refuses the frame.
func TestFrameValidate_Account(t *testing.T) {
	f := hbFrame(1)
	for _, ok := range []string{"_example_agent", "example-agent", "a", "svc.agent_2", "h:0123456789abcdef01234567"} {
		f.Actor = &Actor{Kind: "agent", Account: ok}
		require.NoError(t, f.validate(), ok)
	}
	for _, bad := range []string{"-agent", ".agent", "agent name", "agent:x", "h:short", strings.Repeat("a", 65)} {
		f.Actor = &Actor{Kind: "agent", Account: bad}
		require.Error(t, f.validate(), bad)
	}
}

// The session lookup reads a session's account, so a restricted account's
// events reach the frame named through the whole path.
func TestFeed_SessionsOfReadsTheAccount(t *testing.T) {
	ctx := context.Background()
	db, rec := feedDB(t)
	ts := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	const id = "01KX0000000000000000000S02"
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: "cc/0a1b2c3d-4e5f6a7b", ProjectSlug: "myapp",
		Account: "_example_agent", Status: core.SessionActive, CreatedAt: ts, UpdatedAt: ts}))
	_, err := rec.Record(ctx, core.Event{Kind: core.EventSessionStarted, SessionID: id, ProjectSlug: "myapp", TS: ts})
	require.NoError(t, err)
	logged, err := rec.Tail(ctx, 0, 10)
	require.NoError(t, err)
	f := New(Config{DB: db, Events: rec})
	sessions := f.sessionsOf(ctx, logged)
	require.Equal(t, session{name: "_example_agent:cc/0a1b2c3d-4e5f6a7b", account: "_example_agent"}, sessions[id],
		"stored inside the account's namespace, and read with its account")
	fr := f.mapper.frame(logged[0].Event, sessions[id])
	require.Equal(t, &Actor{Kind: "agent", Session: "_example_agent:cc/0a1b2c3d-4e5f6a7b", Account: "_example_agent"}, fr.Actor)
}
