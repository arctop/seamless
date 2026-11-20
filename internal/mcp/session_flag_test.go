package mcp_test

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// newFlagServer is newServer with a transcript copy service behind it.
func newFlagServer(t *testing.T) (string, *sql.DB, *transcripts.Service) {
	t.Helper()
	var svc *transcripts.Service
	url, db, _, _ := newServerFiles(t, func(cfg *mcpserver.Config) {
		var err error
		svc, err = transcripts.New(transcripts.Config{DataDir: t.TempDir(), DB: cfg.DB, Events: events.NewRecorder(cfg.DB), LocalHost: "alpha"})
		require.NoError(t, err)
		cfg.Transcripts = svc
	})
	return url, db, svc
}

func sessionRow(t *testing.T, db *sql.DB, id string) core.Session {
	t.Helper()
	sess, ok, err := store.SessionByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return sess
}

// verdict and reason on session_update: a flag alone leaves the findings and
// updated_at as they were, the last flag wins, and none clears it.
func TestSessionUpdate_Verdict(t *testing.T) {
	ctx := context.Background()
	url, db, _ := newFlagServer(t)
	cli := dialClient(t, ctx, url, testKey)
	id := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})["session_id"].(string)
	callJSON(t, ctx, cli, "session_update", map[string]any{"findings": "halfway there"})
	before := sessionRow(t, db, id)

	out := callJSON(t, ctx, cli, "session_update", map[string]any{"verdict": "good", "reason": "clean\n  first try"})
	flag := out["flag"].(map[string]any)
	require.Equal(t, "good", flag["verdict"])
	// session_start without hooks: no hook ever names a transcript for it.
	require.Equal(t, core.TranscriptNoPath.Explain(), flag["transcript"])
	got := sessionRow(t, db, id)
	require.Equal(t, core.VerdictGood, got.Verdict)
	require.Equal(t, "clean first try", got.VerdictReason, "a reason is folded to one line")
	require.Equal(t, "halfway there", got.Findings, "a flag alone leaves the findings")
	require.Equal(t, before.Status, got.Status)

	callJSON(t, ctx, cli, "session_update", map[string]any{"verdict": "bad"})
	got = sessionRow(t, db, id)
	require.Equal(t, core.VerdictBad, got.Verdict, "the last flag wins")
	require.Empty(t, got.VerdictReason)

	out = callJSON(t, ctx, cli, "session_update", map[string]any{"verdict": "none"})
	require.Equal(t, true, out["flag"].(map[string]any)["cleared"])
	require.False(t, sessionRow(t, db, id).Flagged())
	out = callJSON(t, ctx, cli, "session_update", map[string]any{"verdict": "none"})
	require.Equal(t, false, out["flag"].(map[string]any)["cleared"], "nothing to clear")

	var flagged, unflagged int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND session_id = ?`, core.EventSessionFlagged, id).Scan(&flagged))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND session_id = ?`, core.EventSessionUnflagged, id).Scan(&unflagged))
	require.Equal(t, 2, flagged)
	require.Equal(t, 1, unflagged)
}

// The refusals, on both tools: what the enum cannot say is refused before
// anything changes.
func TestSessionFlag_Validation(t *testing.T) {
	ctx := context.Background()
	url, db, _ := newFlagServer(t)
	cli := dialClient(t, ctx, url, testKey)
	id := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})["session_id"].(string)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"no findings, no verdict", map[string]any{}, "findings is required, unless you pass verdict"},
		{"unknown verdict", map[string]any{"verdict": "great"}, `invalid verdict "great"`},
		{"reason without verdict", map[string]any{"findings": "x", "reason": "why"}, "reason explains a verdict"},
		{"reason on none", map[string]any{"verdict": "none", "reason": "why"}, "verdict none clears the flag and takes no reason"},
		{"reason too long", map[string]any{"verdict": "notable", "reason": strings.Repeat("r", core.MaxVerdictReasonRunes+1)}, "keep it to one line"},
	} {
		for _, tool := range []string{"session_update", "session_end"} {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				args := map[string]any{}
				for k, v := range tc.args {
					args[k] = v
				}
				if tool == "session_end" {
					if _, ok := args["findings"]; !ok {
						args["findings"] = "done"
					}
					if tc.name == "no findings, no verdict" {
						return // session_end's findings are always required
					}
				}
				isErr, txt := callErr(t, ctx, cli, tool, args)
				require.True(t, isErr, txt)
				require.Contains(t, txt, tc.want)
			})
		}
	}
	got := sessionRow(t, db, id)
	require.Equal(t, core.SessionActive, got.Status, "a refused session_end ends nothing")
	require.False(t, got.Flagged())
}

// session_end with a verdict flags the session it ends; the copy waits for
// the end signal that follows, after which the transcript is whole.
func TestSessionEnd_Verdict(t *testing.T) {
	ctx := context.Background()
	url, db, _ := newFlagServer(t)
	cli := dialClient(t, ctx, url, testKey)
	id := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})["session_id"].(string)
	out := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done", "verdict": "notable", "reason": "odd race"})
	require.Equal(t, "completed", out["status"])
	flag := out["flag"].(map[string]any)
	require.Equal(t, "notable", flag["verdict"])
	// An explicit session from session_start without hooks names no transcript.
	require.Equal(t, core.TranscriptNoPath.Explain(), flag["transcript"])
	got := sessionRow(t, db, id)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Equal(t, core.VerdictNotable, got.Verdict)
	require.Equal(t, "odd race", got.VerdictReason)
	require.Equal(t, core.TranscriptNoPath, got.Transcript)
}

// A session flagged after it ended is copied at once when the daemon may read
// its transcript, and clearing the flag deletes the copy.
func TestSessionUpdate_RetroactiveFlagCopiesNow(t *testing.T) {
	ctx := context.Background()
	url, db, svc := newFlagServer(t)
	cli := dialClient(t, ctx, url, testKey)
	transcript := filepath.Join(t.TempDir(), "conv.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"user"}`+"\n"), 0o600))
	now := time.Now().UTC()
	sess := core.Session{ID: "01MRETROAAAAAAAAAAAAAAAAAA", Name: "cc/retro", ProjectSlug: "demo", Status: core.SessionCompleted,
		Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "retro-ext", Host: "alpha", Source: "startup",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	require.NoError(t, store.CreateSession(ctx, db, sess))
	require.NoError(t, store.SetSessionTranscriptPath(ctx, db, sess.ID, transcript))
	before := sessionRow(t, db, sess.ID)

	out := callJSON(t, ctx, cli, "session_update", map[string]any{"session_id": sess.ID, "verdict": "good"})
	require.Equal(t, "being copied now", out["flag"].(map[string]any)["transcript"])
	svc.Wait()
	require.True(t, sessionRow(t, db, sess.ID).UpdatedAt.Equal(before.UpdatedAt), "a flag is metadata: an ended session's updated_at stays")
	rc, err := svc.Open(sess.ID)
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, `{"type":"user"}`+"\n", string(b))
	var payload string
	require.NoError(t, db.QueryRow(`SELECT payload FROM events WHERE kind = ? AND session_id = ?`, core.EventSessionFlagged, sess.ID).Scan(&payload))
	require.Contains(t, payload, `"retroactive":true`)

	out = callJSON(t, ctx, cli, "session_update", map[string]any{"session_id": sess.ID, "verdict": "none"})
	require.Equal(t, "copy deleted", out["flag"].(map[string]any)["transcript"])
	require.False(t, svc.Kept(sess.ID))
}

// Fields, not a tool: the flag adds no tool, and both tools' enums are the
// canonical verdict set.
func TestSessionFlag_NoNewTool(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range mcpserver.Catalog() {
		names[tool.Name] = true
		if tool.Name != "session_update" && tool.Name != "session_end" {
			continue
		}
		prop, ok := tool.InputSchema.Properties["verdict"].(map[string]any)
		require.True(t, ok, "%s declares verdict", tool.Name)
		require.ElementsMatch(t, []any{"good", "bad", "notable", "none"}, toAny(prop["enum"]), tool.Name)
		_, ok = tool.InputSchema.Properties["reason"]
		require.True(t, ok, "%s declares reason", tool.Name)
		desc, _ := prop["description"].(string)
		require.Contains(t, desc, "mishaps", "the verdict says how it relates to mishaps")
		require.Contains(t, desc, "favorite_set", "and to favorite_set")
	}
	require.False(t, names["session_flag"])
	require.Len(t, mcpserver.Catalog(), mcpserver.LockedToolCount, "the catalog lists the locked-only tool too")
	require.Equal(t, 37, mcpserver.ToolCount, "ToolCount is unchanged by the flag")
}

func toAny(v any) []any {
	switch vs := v.(type) {
	case []any:
		return vs
	case []string:
		out := make([]any, len(vs))
		for i, s := range vs {
			out[i] = s
		}
		return out
	}
	return nil
}
