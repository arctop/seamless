package mcp_test

import (
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"
)

// A restricted account's project memory that was there BEFORE the owner wrote
// a global rule of the same name (refuseGlobalShadow judges only a new name)
// does not answer memory_read by name in the rule's place: the rule answers,
// and project_memory points at the account's memory by id. By id, or with the
// project named, the account's memory still answers, labeled, and an explicit
// read is pointed at the rule.
func TestRestricted_PreexistingShadowReadsTheRule(t *testing.T) {
	for _, kind := range []string{"constraint", "convention", "protocol", "stage"} {
		t.Run(kind, func(t *testing.T) {
			r := newKARig(t, kaOpts{})
			mine := r.write(r.agent, "deploy-rule", "gotcha", "SHADOW-TEXT\n", nil)
			mineID := mine["id"].(string)
			r.memAccount("deploy-rule", kaAccount)
			rule := r.call(r.owner, "memory_write", map[string]any{"name": "deploy-rule", "kind": kind,
				"description": "the deploy rule", "body": "Status: open\n\nTHE-OWNER-RULE\n", "project": "global"})
			ruleID := rule["id"].(string)

			// Every reader the briefing sends with a bare name: the owner's
			// agent, the account's own, and a connection with no peer_auth view.
			r.call(r.owner, "session_start", map[string]any{"project": kaOpen})
			r.call(r.agent, "session_start", map[string]any{"project": kaOpen})
			r.call(r.plain, "session_start", map[string]any{"project": kaOpen})
			for label, cli := range map[string]*mcpclient.Client{"owner": r.owner, "account": r.agent, "no peer_auth": r.plain} {
				read := r.call(cli, "memory_read", map[string]any{"name": "deploy-rule"})
				require.Equal(t, ruleID, read["id"], label)
				require.Contains(t, read["body"], "THE-OWNER-RULE", label)
				require.Equal(t, "", read["project"], label)
				require.NotContains(t, read, "account", "%s: the rule is the owner's", label)
				require.NotContains(t, read, "global_memory", label)
				pm, ok := read["project_memory"].(map[string]any)
				require.True(t, ok, "%s: the project memory is named: %v", label, read)
				require.Equal(t, mineID, pm["id"], label)
				require.Equal(t, kaOpen, pm["project"], label)
				require.Equal(t, kaAccount, pm["account"], label)
				require.Equal(t, "project "+kaOpen+" also has a memory named deploy-rule (written by account "+kaAccount+
					"); a restricted account's project memory does not answer by-name reads in place of a global "+kind+
					", so this answer is the global one -- read the project memory with memory_read id="+mineID, pm["note"], label)
				require.NotContains(t, mustJSON(t, read), "SHADOW-TEXT", label)
			}

			// By id: the account's memory, labeled as always, with no pointer.
			byID := r.call(r.owner, "memory_read", map[string]any{"id": mineID})
			require.Equal(t, "SHADOW-TEXT\n", byID["body"])
			require.Equal(t, kaAccount, byID["account"])
			require.NotContains(t, byID, "project_memory")
			require.NotContains(t, byID, "global_memory")

			// The project named: the account's memory, pointed at the rule.
			named := r.call(r.owner, "memory_read", map[string]any{"name": "deploy-rule", "project": kaOpen})
			require.Equal(t, mineID, named["id"])
			require.Equal(t, "SHADOW-TEXT\n", named["body"])
			require.Equal(t, kaAccount, named["account"])
			require.NotContains(t, named, "project_memory")
			gm, ok := named["global_memory"].(map[string]any)
			require.True(t, ok, "the explicit read is pointed at the rule: %v", named)
			require.Equal(t, ruleID, gm["id"])
			require.Equal(t, kind, gm["kind"])
			require.Equal(t, "a global "+kind+" named deploy-rule also exists, and a by-name read that names no project "+
				"answers with it rather than with this project memory (written by account "+kaAccount+"); read it with "+
				"memory_read name=deploy-rule project=global", gm["note"])

			// project=global reads the rule, and has nothing to point at.
			global := r.call(r.agent, "memory_read", map[string]any{"name": "deploy-rule", "project": "global"})
			require.Equal(t, ruleID, global["id"])
			require.NotContains(t, global, "project_memory")
			require.NotContains(t, global, "global_memory")
		})
	}
}

// What stays as it was: an owner's project memory is a project-local override
// of the global rule, and a global memory of a kind no briefing injects is no
// rule to protect. Neither read grows a key.
func TestRestricted_ReadShadowIsNarrow(t *testing.T) {
	r := newKARig(t, kaOpts{})
	own := r.write(r.owner, "local-rule", "gotcha", "OWNER-OVERRIDE\n", nil)
	r.call(r.owner, "memory_write", map[string]any{"name": "local-rule", "kind": "constraint",
		"description": "the rule", "body": "Rule.\n", "project": "global"})
	mine := r.write(r.agent, "cache-note", "gotcha", "ACCOUNT-NOTE\n", nil)
	r.call(r.owner, "memory_write", map[string]any{"name": "cache-note", "kind": "gotcha",
		"description": "a global gotcha", "body": "Global gotcha.\n", "project": "global"})

	r.call(r.owner, "session_start", map[string]any{"project": kaOpen})
	read := r.call(r.owner, "memory_read", map[string]any{"name": "local-rule"})
	require.Equal(t, own["id"], read["id"], "the owner's project memory still shadows")
	require.Equal(t, "OWNER-OVERRIDE\n", read["body"])
	require.ElementsMatch(t, []string{"id", "kind", "name", "description", "project", "body", "tags",
		"source_session", "content_hash"}, wkKeys(read), "the owner's read is the old shape")

	read = r.call(r.owner, "memory_read", map[string]any{"name": "cache-note"})
	require.Equal(t, mine["id"], read["id"], "a non-briefing global kind does not trigger it")
	require.Equal(t, kaAccount, read["account"], "labeled as always")
	require.NotContains(t, read, "project_memory")
	require.NotContains(t, read, "global_memory")
}
