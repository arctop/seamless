package mcp_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/agentguide"
	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
)

// An entry that opens with its type records the type beside the description,
// which stays the entry as written; an entry with no type, an unknown word or
// a colon far in is recorded exactly as before, and the answer counts it. The
// memory scan runs over the whole entry, prefix or not.
func TestSessionEnd_RecordsMishapTypes(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	mem := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "zsh-equals-expansion", "kind": "gotcha",
		"description": "mishap type fixture", "body": "body\n",
	})
	memID := mem["id"].(string)

	typed := "shell-trap: a bare == in a zsh command died, despite zsh-equals-expansion"
	spaced := "Wrong target: edited the installed config instead of the repo copy"
	plain := "pkill -f matched the live daemon as well as the scratch one"
	unknown := "note: the build was slow"
	lock := "LOCK seamless main: held without a claim"
	end := callJSON(t, ctx, cli, "session_end", map[string]any{
		"findings": "done",
		"mishaps":  []string{typed, spaced, plain, unknown, lock},
	})
	require.Equal(t, float64(5), end["mishaps_recorded"])
	require.Equal(t, float64(3), end["mishaps_untyped"], "the answer counts the entries that named no type")

	byDesc := map[string]map[string]any{}
	for _, m := range mishapEvents(t, db) {
		byDesc[m["description"].(string)] = m
	}
	require.Len(t, byDesc, 5, "every description is the entry as written, prefix and all")

	require.Equal(t, "shell-trap", byDesc[typed]["type"])
	require.Equal(t, []any{memID}, byDesc[typed]["item_ids"], "the scan covers the text after the prefix")
	require.Equal(t, "wrong-target", byDesc[spaced]["type"])
	require.NotContains(t, byDesc[spaced], "item_ids")
	for _, desc := range []string{plain, unknown, lock} {
		require.NotContains(t, byDesc[desc], "type", "%q names no type, so its payload is the old one", desc)
		require.NotContains(t, byDesc[desc], "item_ids")
	}
}

// Every entry typed: the answer carries no mishaps_untyped key at all.
func TestSessionEnd_AllTypedMishapsOmitTheUntypedCount(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	end := callJSON(t, ctx, cli, "session_end", map[string]any{
		"findings": "done",
		"mishaps":  []string{"live-state: restarted the running daemon", "fabricated: passed a flag that does not exist"},
	})
	require.Equal(t, float64(2), end["mishaps_recorded"])
	require.NotContains(t, end, "mishaps_untyped")
	types := []any{}
	for _, m := range mishapEvents(t, db) {
		types = append(types, m["type"])
	}
	require.ElementsMatch(t, []any{"live-state", "fabricated"}, types)
}

// session_end's mishaps argument asks for the prefix in the guidance's own
// words, which name core's vocabulary, and keeps the exact-slug instruction.
func TestSessionEndTool_MishapsAskForTheTypePrefix(t *testing.T) {
	var desc string
	for _, tool := range mcpserver.Catalog() {
		if tool.Name != "session_end" {
			continue
		}
		prop, ok := tool.InputSchema.Properties["mishaps"].(map[string]any)
		require.True(t, ok, "session_end declares mishaps")
		desc, _ = prop["description"].(string)
	}
	require.Contains(t, desc, agentguide.MishapPrefix)
	for _, typ := range core.MishapTypes {
		require.Contains(t, desc, string(typ))
	}
	require.Contains(t, desc, "exact slug")
	require.True(t, strings.Contains(desc, `"ignored-warning: violated chroma-boot-race by ..."`),
		"the example shows a typed entry naming its memory")
}
