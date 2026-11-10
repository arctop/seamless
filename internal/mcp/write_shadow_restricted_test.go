package mcp_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// A by-name write that names no scope, on a name a restricted account's
// project memory shares with a global rule, is refused: nothing is written,
// and the error names both memories and asks for project=<slug> or
// project=global. With the scope named, the write lands on the memory it
// names, as before.
func TestRestricted_ShadowedNameWriteNeedsAScope(t *testing.T) {
	writes := []struct {
		tool string
		args map[string]any
	}{
		{"memory_append", map[string]any{"name": "deploy-rule", "body": "More.\n"}},
		{"memory_edit", map[string]any{"name": "deploy-rule", "tags_add": []any{"x"}}},
		{"memory_delete", map[string]any{"name": "deploy-rule"}},
		{"favorite_set", map[string]any{"kind": "memory", "id": "deploy-rule", "favorite": true}},
		{"memory_write", map[string]any{"name": "new-rule", "kind": "gotcha", "description": "the replacement",
			"body": "Replacement.\n", "supersedes": "deploy-rule"}},
	}
	with := func(args map[string]any, project string) map[string]any {
		out := map[string]any{}
		for k, v := range args {
			out[k] = v
		}
		if project != "" {
			out["project"] = project
		}
		return out
	}

	for _, w := range writes {
		t.Run(w.tool, func(t *testing.T) {
			r, mineID, ruleID := newShadowRig(t)
			before := r.corpus()
			mine, rule := r.memState(mineID), r.memState(ruleID)

			text := r.refused(r.owner, w.tool, w.args)
			require.Contains(t, text, "the name deploy-rule is two memories: project "+kaOpen+"'s (id "+mineID+
				", written by account "+kaAccount+") and a global constraint (id "+ruleID+")")
			require.Contains(t, text, "nothing was written; pass project="+kaOpen+
				" for the project memory, or project=global for the global constraint")
			require.Equal(t, before, r.corpus(), "nothing was written")

			// The account's own agent is asked the same.
			r.call(r.agent, "session_start", map[string]any{"project": kaOpen})
			if w.tool != "favorite_set" { // refused to it outright, as always
				require.Contains(t, r.refused(r.agent, w.tool, w.args), "is two memories")
			}
			require.Equal(t, before, r.corpus())

			// project=<slug>: the project memory, and only it.
			r.call(r.owner, w.tool, with(w.args, kaOpen))
			require.NotEqual(t, mine, r.memState(mineID), "the project memory took the write")
			require.Equal(t, rule, r.memState(ruleID), "the rule is untouched")
		})
		t.Run(w.tool+" project=global", func(t *testing.T) {
			r, mineID, ruleID := newShadowRig(t)
			mine, rule := r.memState(mineID), r.memState(ruleID)
			r.call(r.owner, w.tool, with(w.args, "global"))
			require.NotEqual(t, rule, r.memState(ruleID), "the rule took the write")
			require.Equal(t, mine, r.memState(mineID), "the project memory is untouched")
		})
	}
}

// Nothing changes where the pair does not exist: the owner's own project
// memory still answers an unscoped write, and so does the account's when the
// global memory of its name is of a kind no briefing injects.
func TestRestricted_ShadowedNameWriteIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ownerLocal bool
		globalKind string
	}{
		{"the owner's project memory", true, "constraint"},
		{"a non-briefing global kind", false, "gotcha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newKARig(t, kaOpts{})
			writer := r.agent
			if tc.ownerLocal {
				writer = r.owner
			}
			local := r.write(writer, "deploy-rule", "gotcha", "LOCAL\n", nil)
			global := r.call(r.owner, "memory_write", map[string]any{"name": "deploy-rule", "kind": tc.globalKind,
				"description": "the global one", "body": "GLOBAL\n", "project": "global"})
			r.call(r.owner, "session_start", map[string]any{"project": kaOpen})
			localState, globalState := r.memState(local["id"].(string)), r.memState(global["id"].(string))
			r.call(r.owner, "memory_append", map[string]any{"name": "deploy-rule", "body": "More.\n"})
			require.NotEqual(t, localState, r.memState(local["id"].(string)), "the project memory answers, as before")
			require.Equal(t, globalState, r.memState(global["id"].(string)))
		})
	}
}

// newShadowRig is a kaRig holding the pair: the account's project memory
// written first, then the owner's global constraint of the same name, with
// the owner's connection bound to the project.
func newShadowRig(t *testing.T) (*kaRig, string, string) {
	t.Helper()
	r := newKARig(t, kaOpts{})
	mine := r.write(r.agent, "deploy-rule", "gotcha", "SHADOW-TEXT\n", nil)
	rule := r.call(r.owner, "memory_write", map[string]any{"name": "deploy-rule", "kind": "constraint",
		"description": "the deploy rule", "body": "THE-OWNER-RULE\n", "project": "global"})
	r.call(r.owner, "session_start", map[string]any{"project": kaOpen})
	return r, mine["id"].(string), rule["id"].(string)
}

// memState is what a write to the memory would change: whether it exists, is
// active, and its file's hash.
func (r *kaRig) memState(id string) string {
	r.t.Helper()
	m, ok, err := store.MemoryByID(r.ctx, r.db, id)
	require.NoError(r.t, err)
	return fmt.Sprintf("found=%v active=%v hash=%s", ok, ok && m.Active(), m.ContentHash)
}
