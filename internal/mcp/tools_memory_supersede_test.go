package mcp_test

import (
	"context"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"
)

// TestMemoryWrite_SupersedeFailureIsToolError covers the write-succeeds,
// supersede-fails split: the call must surface an explicit tool error (not a
// success payload with an embedded error field an agent would read past), the
// new memory is kept rather than rolled back, and the still-active state of
// the target is spelled out. A retry with the corrected target then completes
// the supersession losslessly via the in-place update.
func TestMemoryWrite_SupersedeFailureIsToolError(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "old-truth", "kind": "gotcha",
		"description": "the old understanding",
		"body":        "old body\n",
	})

	// The supersede target does not exist: the write lands, the call errors.
	isErr, txt := callErr(t, ctx, cli, "memory_write", map[string]any{
		"name": "new-truth", "kind": "gotcha",
		"description": "the new understanding",
		"body":        "new body\n",
		"supersedes":  "no-such-memory",
	})
	require.True(t, isErr, "a failed supersede must be a tool error, not a field in a success payload")
	require.Contains(t, txt, "written and kept")
	require.Contains(t, txt, "no-such-memory")
	require.Contains(t, txt, "STILL ACTIVE")

	// The new memory was kept, not rolled back, and reads back active; the
	// error names its id so the agent keeps the reference.
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "new-truth"})
	require.Contains(t, r["body"], "new body")
	require.Nil(t, r["warning"])
	require.Contains(t, txt, r["id"].(string), "the error must carry the written memory's id")

	// Retrying with the corrected target is an in-place update (same id) that
	// completes the supersession.
	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "new-truth", "kind": "gotcha",
		"description": "the new understanding",
		"body":        "new body\n",
		"supersedes":  "old-truth",
	})
	require.Equal(t, true, w["updated"])
	require.Equal(t, r["id"], w["id"])
	require.Equal(t, "demo/old-truth", w["superseded"])

	old := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "old-truth"})
	require.Contains(t, old["warning"], "superseded by demo/new-truth")
}

// TestMemoryWrite_SupersedeRetryIdempotent pins the retry semantics the
// partial-failure error prescribes: re-issuing a memory_write whose supersede
// already landed succeeds again (the target being superseded by this exact
// replacement is the goal state), while a DIFFERENT memory claiming an
// already-superseded target is a tool error -- with that new memory still kept.
func TestMemoryWrite_SupersedeRetryIdempotent(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "old-truth", "kind": "gotcha",
		"description": "the old understanding",
		"body":        "old body\n",
	})
	args := map[string]any{
		"name": "new-truth", "kind": "gotcha",
		"description": "the new understanding",
		"body":        "new body\n",
		"supersedes":  "old-truth",
	}
	w := callJSON(t, ctx, cli, "memory_write", args)
	require.Equal(t, "demo/old-truth", w["superseded"])

	// Exact retry: already superseded by this same replacement -> success.
	w2 := callJSON(t, ctx, cli, "memory_write", args)
	require.Equal(t, true, w2["updated"])
	require.Equal(t, w["id"], w2["id"])
	require.Equal(t, "demo/old-truth", w2["superseded"])

	// A different memory claiming the same target fails explicitly.
	isErr, txt := callErr(t, ctx, cli, "memory_write", map[string]any{
		"name": "third-truth", "kind": "gotcha",
		"description": "a rival understanding",
		"body":        "third body\n",
		"supersedes":  "old-truth",
	})
	require.True(t, isErr)
	require.Contains(t, txt, "already superseded")

	// The rival write itself is still kept.
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "third-truth"})
	require.Equal(t, "third-truth", r["name"])
	require.Nil(t, r["warning"])
}

// A replacement since deleted has no row left to name its project, so it is
// judged like one the caller may not reach: the answer carries neither its id
// nor the tombstone line naming it, for any reader. The file keeps the line.
func TestMemoryRead_ReplacementSinceDeletedFailsClosed(t *testing.T) {
	r := newKARig(t, kaOpts{})
	r.write(r.owner, "old-rule", "gotcha", "Old body.\n", nil)
	written := r.write(r.owner, "new-rule", "gotcha", "New body.\n", map[string]any{"supersedes": "old-rule"})
	repl := written["id"].(string)
	r.call(r.owner, "memory_delete", map[string]any{"name": "new-rule", "project": kaOpen})

	idx, disk := r.memory("old-rule")
	require.Equal(t, repl, idx.SupersededBy, "the pointer dangles")
	require.Contains(t, disk.Body, "> Superseded by "+kaOpen+"/new-rule")

	for name, cli := range map[string]*mcpclient.Client{"the owner": r.owner, "a restricted account": r.agent, "no peer_auth": r.plain} {
		for _, args := range []map[string]any{{"name": "old-rule", "project": kaOpen}, {"id": idx.ID}} {
			read := r.call(cli, "memory_read", args)
			require.Regexp(t, `^superseded on \d{4}-\d{2}-\d{2}; the replacement is not reachable from here$`, read["warning"], name)
			require.Equal(t, "Old body.\n", read["body"], "%s: the tombstone line is gone, the memory's own words stay", name)
			raw := mustJSON(t, read)
			require.NotContains(t, raw, repl, name)
			require.NotContains(t, raw, "new-rule", name)
		}
	}
	_, after := r.memory("old-rule")
	require.Contains(t, after.Body, "> Superseded by "+kaOpen+"/new-rule", "the file is untouched")
}
