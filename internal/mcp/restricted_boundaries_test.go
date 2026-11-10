package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A restricted account may not take a project name that would newly shadow a
// global memory of a briefing-injected kind: by-name reads try the project
// first, so its text would answer for the owner's rule. Refused as a plain
// write and as a proposal, leaving nothing behind.
func TestRestricted_ShadowOfGlobalRuleRefused(t *testing.T) {
	for _, kind := range []string{"constraint", "convention", "protocol", "stage"} {
		t.Run(kind, func(t *testing.T) {
			r := newKARig(t, kaOpts{})
			r.call(r.owner, "memory_write", map[string]any{"name": "deploy-rule", "kind": kind,
				"description": "the deploy rule", "body": "Status: open\n\nTHE-OWNER-RULE\n", "project": "global"})
			before := r.corpus()

			for _, asKind := range []string{"gotcha", "constraint"} {
				text := r.refused(r.agent, "memory_write", map[string]any{"name": "deploy-rule", "kind": asKind,
					"description": "the deploy rule", "body": "SHADOW-TEXT\n", "project": kaOpen})
				require.Contains(t, text, "choose another name")
				require.Contains(t, text, "global "+kind)
				require.Contains(t, text, "nothing was written")
			}
			require.Equal(t, before, r.corpus(), "the refusal lands nothing")
			require.Empty(t, pendingHolds(t, r.db), "and files no proposal")

			r.call(r.owner, "session_start", map[string]any{"project": kaOpen})
			read := r.call(r.owner, "memory_read", map[string]any{"name": "deploy-rule"})
			require.Contains(t, read["body"], "THE-OWNER-RULE")
		})
	}
}

// Only a NEW shadow of a binding kind is refused: another name, a global
// memory of a kind no briefing injects, and a rewrite of the project memory
// the name already holds all land.
func TestRestricted_ShadowRefusalIsNarrow(t *testing.T) {
	r := newKARig(t, kaOpts{})
	r.call(r.owner, "memory_write", map[string]any{"name": "deploy-rule", "kind": "constraint",
		"description": "the rule", "body": "Rule.\n", "project": "global"})
	r.call(r.owner, "memory_write", map[string]any{"name": "cache-note", "kind": "gotcha",
		"description": "a note", "body": "Note.\n", "project": "global"})

	r.write(r.agent, "deploy-rule-local", "gotcha", "Mine.\n", nil)
	r.memAccount("deploy-rule-local", kaAccount)
	r.write(r.agent, "cache-note", "gotcha", "Mine too.\n", nil)
	r.memAccount("cache-note", kaAccount)

	// The account's memory came first; the owner's global rule arrived after.
	r.write(r.agent, "later-rule", "gotcha", "First.\n", nil)
	r.call(r.owner, "memory_write", map[string]any{"name": "later-rule", "kind": "constraint",
		"description": "the rule", "body": "Rule.\n", "project": "global"})
	out := r.write(r.agent, "later-rule", "gotcha", "Second.\n", nil)
	require.Equal(t, true, out["updated"], "over its own memory the shadow is not new")
	_, disk := r.memory("later-rule")
	require.Equal(t, "Second.\n", disk.Body)
}

// A superseded memory stays readable where its replacement may not be: the
// warning and the body then carry none of the replacement's project, name or
// id. The file keeps its tombstone, and a reader who may read the replacement
// sees what it always did.
func TestRestricted_SupersededReplacementFenced(t *testing.T) {
	r := newKARig(t, kaOpts{screening: true}) // access.refused is recorded only while it is on
	r.call(r.owner, "memory_write", map[string]any{"name": "old-rule", "kind": "gotcha",
		"description": "old", "body": "Old body.\n", "project": "global"})
	r.call(r.owner, "memory_write", map[string]any{"name": "acquisition-target-acme", "kind": "gotcha",
		"description": "secret", "body": "Secret body.\n", "project": kaConf, "supersedes": "old-rule"})
	idx, ok, err := store.MemoryByNameIncludingInvalid(r.ctx, r.db, "", "old-rule")
	require.NoError(t, err)
	require.True(t, ok)
	repl := idx.SupersededBy
	require.NotEmpty(t, repl)

	for _, args := range []map[string]any{{"name": "old-rule", "project": "global"}, {"id": idx.ID}} {
		read := r.call(r.agent, "memory_read", args)
		require.Regexp(t, `^superseded on \d{4}-\d{2}-\d{2}; the replacement is not reachable from here$`, read["warning"])
		require.Equal(t, "Old body.\n", read["body"], "the tombstone line is gone, the memory's own words stay")
		raw := mustJSON(t, read)
		for _, leak := range []string{kaConf, "acquisition-target-acme", repl} {
			require.NotContains(t, raw, leak)
		}
	}
	disk, err := r.mgr.Store().ReadMemory(idx.FilePath)
	require.NoError(t, err)
	require.Contains(t, disk.Body, "> Superseded by "+kaConf+"/acquisition-target-acme", "the file is untouched")
	require.Empty(t, eventsOf(t, r.db, core.EventAccessRefused), "the fence is a filter here: no refusal is recorded")

	bound := dialClient(t, r.ctx, r.ownerURL, testKey)
	r.call(bound, "session_start", map[string]any{"project": kaConf})
	read := r.call(bound, "memory_read", map[string]any{"name": "old-rule", "project": "global"})
	require.Contains(t, read["warning"], "superseded by "+kaConf+"/acquisition-target-acme")
	require.Contains(t, read["body"], "> Superseded by "+kaConf+"/acquisition-target-acme", "a readable replacement changes nothing")
}

// The other half: a replacement the account reaches is named, tombstone and all.
func TestRestricted_SupersededReplacementReachableIsNamed(t *testing.T) {
	r := newKARig(t, kaOpts{})
	r.write(r.owner, "old-note", "gotcha", "Old.\n", nil)
	r.write(r.owner, "new-note", "gotcha", "New.\n", map[string]any{"supersedes": "old-note"})

	read := r.call(r.agent, "memory_read", map[string]any{"name": "old-note", "project": kaOpen})
	require.Contains(t, read["warning"], "superseded by "+kaOpen+"/new-note")
	require.Contains(t, read["body"], "> Superseded by "+kaOpen+"/new-note")
}

// A star pins a memory into briefings and the account may not set one, so a
// change of a starred memory's content drops it, in the file and the index. A
// tags-only change leaves it, and so does anything the owner does.
func TestRestricted_ContentChangeDropsTheStar(t *testing.T) {
	star := func(r *kaRig) {
		r.t.Helper()
		r.write(r.owner, "pinned-note", "gotcha", "Owner body.\n", nil)
		r.call(r.owner, "favorite_set", map[string]any{"kind": "memory", "id": "pinned-note", "favorite": true, "project": kaOpen})
	}
	edit := func(extra map[string]any) map[string]any {
		args := map[string]any{"name": "pinned-note", "project": kaOpen}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	for _, tc := range []struct {
		name string
		call func(r *kaRig)
		want bool
	}{
		{"rewrite", func(r *kaRig) { r.write(r.agent, "pinned-note", "gotcha", "Agent body.\n", nil) }, false},
		{"append", func(r *kaRig) {
			r.call(r.agent, "memory_append", edit(map[string]any{"body": "Agent line.\n"}))
		}, false},
		{"body edit", func(r *kaRig) {
			r.call(r.agent, "memory_edit", edit(map[string]any{"edits": []any{map[string]any{"old_string": "Owner", "new_string": "Agent"}}}))
		}, false},
		{"description edit", func(r *kaRig) {
			r.call(r.agent, "memory_edit", edit(map[string]any{"description": "the agent's line"}))
		}, false},
		{"tags only", func(r *kaRig) {
			r.call(r.agent, "memory_edit", edit(map[string]any{"tags_add": []any{"x"}}))
		}, true},
		{"the owner's rewrite", func(r *kaRig) { r.write(r.owner, "pinned-note", "gotcha", "Owner again.\n", nil) }, true},
		{"the owner's append", func(r *kaRig) {
			r.call(r.owner, "memory_append", edit(map[string]any{"body": "Owner line.\n"}))
		}, true},
		{"with peer_auth off", func(r *kaRig) { r.write(r.plain, "pinned-note", "gotcha", "No auth.\n", nil) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newKARig(t, kaOpts{})
			star(r)
			tc.call(r)
			idx, disk := r.memory("pinned-note")
			require.Equal(t, tc.want, idx.Favorite, "index")
			require.Equal(t, tc.want, disk.Favorite, "file")
		})
	}
}

// A restricted account's rewrite of an owner's task names it on the briefing's
// Ready tasks line and in recall, and the owner's own task still carries nothing.
func TestRestricted_TaskRewriteLabeled(t *testing.T) {
	r := newKARig(t, kaOpts{})
	task := r.call(r.owner, "tasks_add", map[string]any{"project": kaOpen, "title": "owner task", "body": "owner body"})
	id := task["id"].(string)
	r.call(r.owner, "tasks_add", map[string]any{"project": kaOpen, "title": "untouched task", "body": "the owner's alone"})
	untouched := r.call(r.owner, "tasks_add", map[string]any{"project": kaOpen, "title": "claimed task", "body": "the owner's words"})
	out := r.call(r.agent, "tasks_update", map[string]any{"id": id, "title": "Xq7 rewritten", "body": "Xq7 rewritten body"})
	require.Equal(t, kaAccount, out["updated_account"])

	// The owner moving it through statuses keeps the label: the text is still
	// the account's.
	for _, st := range []string{"in_progress", "open"} {
		flip := r.call(r.owner, "tasks_update", map[string]any{"id": id, "status": st})
		require.Equal(t, kaAccount, flip["updated_account"], "status %s", st)
	}
	// The account claiming and releasing the owner's task writes none of it.
	r.call(r.agent, "session_start", map[string]any{"project": kaOpen})
	claim := r.call(r.agent, "tasks_claim", map[string]any{"id": untouched["id"]})
	require.NotContains(t, claim, "updated_account")
	released := r.call(r.agent, "tasks_release", map[string]any{"id": untouched["id"]})
	require.NotContains(t, released, "updated_account")

	mark := " [written by account " + kaAccount + "]"
	sess := r.call(r.owner, "session_start", map[string]any{"project": kaOpen})
	b, _ := sess["briefing"].(string)
	require.Contains(t, b, "- Xq7 rewritten"+mark+"\n")
	require.Contains(t, b, "- untouched task\n")
	require.Contains(t, b, "- claimed task\n")

	rec := r.call(r.owner, "recall", map[string]any{"query": "Xq7", "project": kaOpen})
	require.Contains(t, mustJSON(t, rec), `"account":"`+kaAccount+`"`)
}

// With no restricted account in play none of it applies: the owner's and a
// no-peer_auth caller's shadowing write lands, gardener_request still reaches
// the gardener, and the tool list reads the same either way.
func TestRestricted_NoneOfItAppliesToOthers(t *testing.T) {
	r := newKARig(t, kaOpts{})
	list := func(cli *mcpclient.Client) string {
		res, err := cli.ListTools(r.ctx, mcp.ListToolsRequest{})
		require.NoError(t, err)
		raw, err := json.Marshal(res.Tools)
		require.NoError(t, err)
		return string(raw)
	}
	require.Equal(t, list(r.owner), list(r.plain), "the owner's list is an install without peer_auth's")
	for _, promise := range []string{"The natural-language entry point for REORGANIZING memory", "Plan a project SPLIT"} {
		require.Contains(t, list(r.plain), promise)
		require.NotContains(t, list(r.agent), promise)
	}
	require.Contains(t, list(r.agent), "reorganizing memory is the owner's curation")

	r.call(r.owner, "memory_write", map[string]any{"name": "deploy-rule", "kind": "constraint",
		"description": "the rule", "body": "Rule.\n", "project": "global"})
	for _, cli := range []*mcpclient.Client{r.owner, r.plain} {
		out := r.write(cli, "deploy-rule", "gotcha", "A project's own.\n", nil)
		require.Contains(t, out, "id")
		r.call(cli, "memory_delete", map[string]any{"name": "deploy-rule", "project": kaOpen})

		_, text := callErr(t, r.ctx, cli, "gardener_request", map[string]any{"request": "tidy", "project": kaOpen})
		require.False(t, strings.Contains(text, "not open to account"), text)
	}
	r.chat.mu.Lock()
	defer r.chat.mu.Unlock()
	require.NotEmpty(t, r.chat.prompts, "the request reached the gardener")
}
