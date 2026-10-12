package mcp_test

import (
	"context"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// memory_read accepts id as an alternative address: agents legitimately hold
// memory ULIDs from events, recall results, and gardener proposal payloads,
// and were sending them to memory_read (the gardener's tool-error evidence).
func TestMemoryRead_ByID(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "id-addressed", "kind": "reference", "description": "d",
		"body": "read me by id", "project": "global",
	})
	id, _ := w["id"].(string)
	require.NotEmpty(t, id)

	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"id": id})
	require.Equal(t, "id-addressed", r["name"])
	require.Contains(t, r["body"], "read me by id")

	// A missing id is a loud, specific error.
	isErr, txt := callErr(t, ctx, cli, "memory_read", map[string]any{"id": "01AAAAAAAAAAAAAAAAAAAAAAAA"})
	require.True(t, isErr)
	require.Contains(t, txt, "no memory with id")

	// name and id together are ambiguous, not a precedence guess.
	isErr, txt = callErr(t, ctx, cli, "memory_read", map[string]any{"id": id, "name": "id-addressed"})
	require.True(t, isErr)
	require.Contains(t, txt, "exactly one of name or id")

	// Neither name nor id names both addresses in the rejection.
	isErr, txt = callErr(t, ctx, cli, "memory_read", map[string]any{})
	require.True(t, isErr)
	require.Contains(t, txt, "name or id is required")
}

// A by-name miss answers the two causes behind the gardener's recurring
// memory_read error: a memory the agent knew but asked for in the wrong project
// gets that project named, and any other miss says that a name out of the
// client's own MEMORY.md is a local file, not a Seamless memory.
func TestMemoryRead_MissNamesTheProjectThatHasIt(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "client-work", core.IsolationConfidential)
	isolate(t, ctx, db, "vault", core.IsolationSealed)

	only := map[string]string{"site": "alpha", "docs": "beta", "client-work": "gamma", "vault": "delta"}
	clients := map[string]*mcpclient.Client{}
	for p, name := range only {
		w := bindClient(t, ctx, db, url, p)
		clients[p] = w
		callJSON(t, ctx, w, "memory_write", map[string]any{
			"name": name, "kind": "gotcha", "description": "d", "body": "b",
		})
		callJSON(t, ctx, w, "memory_write", map[string]any{
			"name": "everywhere", "kind": "gotcha", "description": "d", "body": "b",
		})
	}

	cli := bindClient(t, ctx, db, url, "raise")

	isErr, txt := callErr(t, ctx, cli, "memory_read", map[string]any{"name": "alpha"})
	require.True(t, isErr)
	require.Contains(t, txt, `no memory named "alpha" in project "raise"`)
	require.Contains(t, txt, "it is in project site: pass project=site")
	require.NotContains(t, txt, "MEMORY.md", "a found scope is the whole answer")
	require.Equal(t, "site", callJSON(t, ctx, cli, "memory_read",
		map[string]any{"name": "alpha", "project": "site"})["project"])

	isErr, txt = callErr(t, ctx, cli, "memory_read", map[string]any{"name": "everywhere"})
	require.True(t, isErr)
	require.Contains(t, txt, "that name is in projects docs, site: pass project=<slug>")

	// A fenced project's names never surface: from outside, its memory reads
	// as a plain miss with the generic advice.
	for _, fenced := range []string{"client-work", "vault"} {
		isErr, txt = callErr(t, ctx, cli, "memory_read", map[string]any{"name": only[fenced]})
		require.True(t, isErr)
		require.NotContains(t, txt, fenced, txt)
		require.Contains(t, txt, "MEMORY.md", txt)
	}

	// A name that is nowhere gets the client-memory hint.
	isErr, txt = callErr(t, ctx, cli, "memory_read", map[string]any{"name": "two-claude-accounts-design-on-3k"})
	require.True(t, isErr)
	require.Contains(t, txt, "check the name")
	require.Contains(t, txt, "a name listed in your client's own MEMORY.md (Claude Code's file-based auto-memory) is a local file, not a Seamless memory")

	// A sealed caller reads nothing outside itself, so it is never pointed out.
	isErr, txt = callErr(t, ctx, clients["vault"], "memory_read", map[string]any{"name": "alpha"})
	require.True(t, isErr)
	require.NotContains(t, txt, "project=site", txt)
	require.NotContains(t, txt, "project=global", txt)
	require.Contains(t, txt, "MEMORY.md", txt)
}

// query= on memory_read is an agent trying to search; the rejection points at
// recall instead of leaving it to guess which parameter to respell.
func TestMemoryRead_QueryRedirectsToRecall(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	isErr, txt := callErr(t, ctx, cli, "memory_read", map[string]any{"query": "anything"})
	require.True(t, isErr)
	require.Contains(t, txt, `unknown parameter "query"`)
	require.Contains(t, txt, "use the recall tool")
}

// notes_read accepts slug (with a name alias): briefings, plan compositions,
// and notes_create responses all name notes by slug, so slug is the vocabulary
// agents actually hold -- id-only was the asymmetry behind the recurring
// unknown-parameter errors.
func TestNotesRead_BySlug(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	nc := callJSON(t, ctx, cli, "notes_create", map[string]any{
		"title": "Slug Addressed Note", "body": "note body here", "project": "global",
	})
	slug, _ := nc["slug"].(string)
	require.NotEmpty(t, slug)

	r := callJSON(t, ctx, cli, "notes_read", map[string]any{"slug": slug, "project": "global"})
	require.Contains(t, r["body"], "note body here")

	// The name alias lands on the slug parameter.
	r = callJSON(t, ctx, cli, "notes_read", map[string]any{"name": slug, "project": "global"})
	require.Contains(t, r["body"], "note body here")

	// A miss names the scope searched and the other address forms.
	isErr, txt := callErr(t, ctx, cli, "notes_read", map[string]any{"slug": "no-such-note", "project": "global"})
	require.True(t, isErr)
	require.Contains(t, txt, "no note named")
	require.Contains(t, txt, "recall")

	// id and slug together are ambiguous, not a precedence guess.
	id, _ := nc["id"].(string)
	require.NotEmpty(t, id)
	isErr, txt = callErr(t, ctx, cli, "notes_read", map[string]any{"id": id, "slug": slug})
	require.True(t, isErr)
	require.Contains(t, txt, "exactly one of id or slug")

	// Neither id nor slug names both addresses in the rejection.
	isErr, txt = callErr(t, ctx, cli, "notes_read", map[string]any{})
	require.True(t, isErr)
	require.Contains(t, txt, "id or slug is required")
}
