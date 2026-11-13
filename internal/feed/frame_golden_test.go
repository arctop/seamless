package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// goldenOwnerFrames are the owner's frames for goldenOwnerEvents, byte for
// byte, as the feed sent them before frames could name a restricted account.
// The owner's frames must never change shape: a monitor tells a restricted
// account's events from the owner's by the account alone.
var goldenOwnerFrames = []string{
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"session_started","severity":"info","outcome":"success","actor":{"kind":"agent","id":"4242.1730000000","session":"cc/0a1b2c3d-4e5f6a7b"},"target":{"system":"seamless","asset":"myapp"},"fields":{"event_id":"01KX0000000000000000000G01"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"memory_written","severity":"info","outcome":"success","actor":{"kind":"agent","id":"4242.1730000000","session":"cc/0a1b2c3d-4e5f6a7b"},"target":{"system":"seamless","asset":"myapp"},"fields":{"event_id":"01KX0000000000000000000G02","item_id":"01KX0000000000000000000M01","kind":"gotcha","updated":false}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"memory_held","severity":"medium","outcome":"blocked","actor":{"kind":"agent","id":"4242.1730000000","session":"cc/0a1b2c3d-4e5f6a7b"},"target":{"system":"seamless","asset":"myapp","path_class":"memory"},"fields":{"event_id":"01KX0000000000000000000G03","hold_id":"01KX0000000000000000000H01","item_kind":"memory","kind":"constraint","op":"write","reasons":"instruction.override","tool":"memory_write"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"task_transition","severity":"info","outcome":"success","actor":{"kind":"agent","id":"4242.1730000000","session":"cc/0a1b2c3d-4e5f6a7b"},"target":{"system":"seamless","asset":"myapp"},"fields":{"event_id":"01KX0000000000000000000G04","from":"open","item_id":"01KX0000000000000000000T01","to":"done"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"auth","activity":"access_refused","severity":"medium","outcome":"blocked","actor":{"kind":"agent","id":"4242.1730000000","session":"cc/0a1b2c3d-4e5f6a7b"},"target":{"system":"seamless","asset":"myapp"},"fields":{"event_id":"01KX0000000000000000000G05","reason":"fence.write","surface":"mcp","target_project":"h:c2100dc57dab34afdd60a73f","tool":"memory_write"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"session_ended","severity":"info","outcome":"success","actor":{"kind":"agent","id":"4242.1730000000","session":"cc/0a1b2c3d-4e5f6a7b"},"target":{"system":"seamless","asset":"myapp"},"fields":{"event_id":"01KX0000000000000000000G06"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"auth","activity":"access_refused","severity":"medium","outcome":"blocked","actor":{"kind":"system","id":"seamless"},"target":{"system":"seamless","asset":"global"},"fields":{"event_id":"01KX0000000000000000000G07","reason":"auth.failed","suppressed":2,"surface":"mcp"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"memory_released","severity":"low","outcome":"success","actor":{"kind":"user","id":"owner"},"target":{"system":"seamless","asset":"myapp","path_class":"memory"},"fields":{"by":"owner","event_id":"01KX0000000000000000000G08","hold_id":"01KX0000000000000000000H01","item_kind":"memory","kind":"constraint","op":"write","tool":"memory_write"}}`,
	`{"time":"2026-11-02T01:16:54.977Z","host":"build-host","class":"agent","activity":"tool_call","severity":"info","outcome":"success","actor":{"kind":"system","id":"seamless"},"target":{"system":"seamless","asset":"myapp"},"fields":{"duration_ms":7,"event_id":"01KX0000000000000000000G09","tool":"tasks_list"}}`,
}

// goldenOwnerEvents is one of each event an agent's work and its refusals
// produce, all of them the owner's: in a bound session, with no session, and
// at the console.
func goldenOwnerEvents(sessionID string) []core.Event {
	ts := time.Date(2026, 11, 2, 1, 16, 54, 977000000, time.UTC)
	id := func(s string) string { return "01KX00000000000000000" + s }
	return []core.Event{
		{ID: id("00G01"), TS: ts, Kind: core.EventSessionStarted, SessionID: sessionID, ProjectSlug: "myapp"},
		{ID: id("00G02"), TS: ts, Kind: core.EventMemoryWritten, SessionID: sessionID, ProjectSlug: "myapp", ItemID: id("00M01"),
			Payload: map[string]any{"name": "a-name", "kind": "gotcha", "updated": false}},
		{ID: id("00G03"), TS: ts, Kind: core.EventMemoryHeld, SessionID: sessionID, ProjectSlug: "myapp", Payload: map[string]any{
			"hold": id("00H01"), "op": "write", "item_kind": "memory", "kind": "constraint", "tool": "memory_write",
			"reasons": []any{"instruction.override"}}},
		{ID: id("00G04"), TS: ts, Kind: core.EventTaskTransition, SessionID: sessionID, ProjectSlug: "myapp", ItemID: id("00T01"),
			Payload: map[string]any{"from": "open", "to": "done", "commits_recorded": 1}},
		{ID: id("00G05"), TS: ts, Kind: core.EventAccessRefused, SessionID: sessionID, ProjectSlug: "myapp", Payload: map[string]any{
			"reason": "fence.write", "surface": "mcp", "tool": "memory_write", "target": "vault-project"}},
		{ID: id("00G06"), TS: ts, Kind: core.EventSessionEnded, SessionID: sessionID, ProjectSlug: "myapp",
			Payload: map[string]any{"findings": "done", "commits_recorded": 1}},
		{ID: id("00G07"), TS: ts, Kind: core.EventAccessRefused, Payload: map[string]any{
			"reason": "auth.failed", "surface": "mcp", "suppressed": 2}},
		{ID: id("00G08"), TS: ts, Kind: core.EventMemoryReleased, ProjectSlug: "myapp", Payload: map[string]any{
			"hold": id("00H01"), "op": "write", "item_kind": "memory", "kind": "constraint", "tool": "memory_write", "by": "owner"}},
		{ID: id("00G09"), TS: ts, Kind: core.EventToolCall, ProjectSlug: "myapp", Payload: map[string]any{
			"tool": "tasks_list", "duration_ms": 7}},
	}
}

// The owner's frames, through the whole path -- the event log, the session
// lookup, the mapper -- are the golden ones.
func TestFrames_OwnerGolden(t *testing.T) {
	ctx := context.Background()
	db, rec := feedDB(t)
	_, err := store.EnsureProject(ctx, db, "vault-project", "vault-project")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "vault-project", core.IsolationConfidential))
	ts := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	const sess = "01KX0000000000000000000S01"
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: sess, Name: "cc/0a1b2c3d-4e5f6a7b", ProjectSlug: "myapp",
		Status: core.SessionActive, CreatedAt: ts, UpdatedAt: ts}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, db, sess, "4242.1730000000"))
	for _, e := range goldenOwnerEvents(sess) {
		_, err := rec.Record(ctx, e)
		require.NoError(t, err)
	}

	f := New(Config{DB: db, Events: rec, Host: "build-host"})
	f.mapper.key = bytes.Repeat([]byte{7}, 32)
	logged, err := rec.Tail(ctx, 0, 100)
	require.NoError(t, err)
	sessions := f.sessionsOf(ctx, logged)
	got := make([]string, 0, len(logged))
	for _, e := range logged {
		fr := f.mapper.frame(e.Event, sessions[e.SessionID])
		require.NoError(t, fr.validate(), "%s", e.Kind)
		raw, err := json.Marshal(fr)
		require.NoError(t, err)
		got = append(got, string(raw))
	}
	require.Equal(t, strings.Join(goldenOwnerFrames, "\n"), strings.Join(got, "\n"))
}
