package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The read verbs' text rendering, and the exact tool call behind each: these are
// thin clients, so what they send is the contract as much as what they print.
func TestKnowledgeVerbs_RenderAndCall(t *testing.T) {
	fx := newGoldenFixture(t)
	for _, tc := range []struct {
		name     string
		argv     []string
		tool     string
		args     map[string]any
		wantText string
	}{
		{
			"memory read by name",
			[]string{"memory", "read", "lease-steal-window", "--project", "example-project"},
			"memory_read", map[string]any{"name": "lease-steal-window", "project": "example-project"},
			"lease-steal-window  [gotcha]  example-project\n" +
				"id:          01KAAAAAAAAAAAAAAAAAAAAAAA\n" +
				"description: a claim can be stolen inside the lease grace\n" +
				"tags:        leases, claims\n" +
				"model:       example-model-1\n" +
				"\n" +
				"The lease grace lets a second claimer in.\n\nWait for expiry.\n",
		},
		{
			// A superseded memory still reads, under the warning naming what
			// replaced it; empty fields print no line at all.
			"memory read superseded",
			[]string{"memory", "read", "old-lease"},
			"memory_read", map[string]any{"name": "old-lease"},
			"old-lease  [decision]  -\n" +
				"id:          01KRRRRRRRRRRRRRRRRRRRRRRR\n" +
				"description: the old lease rule\n" +
				"warning:     superseded by lease-steal-window; read that instead\n",
		},
		{
			"memory read by id",
			[]string{"memory", "read", "--id", "01KAAAAAAAAAAAAAAAAAAAAAAA"},
			"memory_read", map[string]any{"id": "01KAAAAAAAAAAAAAAAAAAAAAAA"},
			"",
		},
		{
			// recall's browse mode: a kind and no query.
			"memory list by kind",
			[]string{"memory", "list", "--kind", "gotcha"},
			"recall", map[string]any{"scope": "memories", "kind": "gotcha", "limit": float64(10)},
			"lease-steal-window (example-project, 3d)\n" +
				"    a claim can be stolen inside the lease grace\n" +
				"global-rule (-, 2w)\n",
		},
		{
			"memory list by words",
			[]string{"memory", "list", "lease", "grace", "--limit", "25", "--project", "example-project"},
			"recall", map[string]any{"scope": "memories", "query": "lease grace", "limit": float64(25), "project": "example-project"},
			"",
		},
		{
			"memory list words within a kind",
			[]string{"memory", "list", "--kind", "gotcha", "lease"},
			"recall", map[string]any{"scope": "memories", "query": "lease", "kind": "gotcha", "limit": float64(10)},
			"",
		},
		{
			"note read by slug",
			[]string{"note", "read", "lease-design"},
			"notes_read", map[string]any{"slug": "lease-design"},
			"lease-design  example-project\n" +
				"title:       Lease design\n" +
				"id:          01KCCCCCCCCCCCCCCCCCCCCCCC\n" +
				"description: how leases work\n" +
				"tags:        plan:lease-rework\n" +
				"source:      https://example.com/leases\n" +
				"\n" +
				"# Lease design\n\nLeases expire.\n",
		},
		{
			"note read by id",
			[]string{"note", "read", "--id", "01KCCCCCCCCCCCCCCCCCCCCCCC"},
			"notes_read", map[string]any{"id": "01KCCCCCCCCCCCCCCCCCCCCCCC"},
			"",
		},
		{
			"note list",
			[]string{"note", "list", "lease", "design", "--project", "example-project"},
			"recall", map[string]any{"scope": "notes", "query": "lease design", "limit": float64(10), "project": "example-project"},
			"lease-design  Lease design (example-project, 2w)\n" +
				"    how leases work\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := fx.run(t, tc.argv...)
			require.Equal(t, 0, code, stderr)
			got := fx.lastCall(t)
			require.Equal(t, tc.tool, got.name)
			require.Equal(t, tc.args, got.args)
			if tc.wantText != "" {
				require.Equal(t, tc.wantText, stdout)
			}
		})
	}
}

func TestKnowledgeVerbs_EmptyLists(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{"recall": fixedReply(fxRecallEmpty)}, nil, fxHealthz)
	stdout, _, code := fx.run(t, "memory", "list", "--kind", "convention")
	require.Equal(t, 0, code)
	require.Equal(t, "(no memories)\n", stdout)

	stdout, _, code = fx.run(t, "note", "list", "nothing")
	require.Equal(t, 0, code)
	require.Equal(t, "(no notes)\n", stdout)
}

// Each refusal names its fix and happens before a connection is opened.
func TestKnowledgeVerbs_RefuseBeforeDialing(t *testing.T) {
	fx := newGoldenFixture(t)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"memory", "read"}, "nothing to read: pass a memory name, or --id ID"},
		{[]string{"memory", "read", "a", "--id", "01KAAAAAAAAAAAAAAAAAAAAAAA"}, "pass a memory name or --id, not both"},
		// --project means nothing to an id read; accepting it would be a silently
		// ignored flag.
		{[]string{"memory", "read", "--id", "01KAAAAAAAAAAAAAAAAAAAAAAA", "--project", "p"}, "--project scopes a name lookup"},
		{[]string{"note", "read"}, "nothing to read: pass a note slug, or --id ID"},
		{[]string{"note", "read", "s", "--id", "01KCCCCCCCCCCCCCCCCCCCCCCC"}, "pass a note slug or --id, not both"},
		{[]string{"note", "read", "--id", "01KCCCCCCCCCCCCCCCCCCCCCCC", "--project", "p"}, "--project scopes a slug lookup"},
		// recall's own rule: a query, or a kind to browse. There is no unfiltered
		// listing to fall back to.
		{[]string{"memory", "list"}, "pass --kind to list one kind newest first, or words to search memories"},
		{[]string{"note", "list", " "}, "query is empty"},
	} {
		e, out, errb := noDialEnv(t, fx)
		require.Equal(t, 1, dispatch(context.Background(), e, tc.argv), "%v", tc.argv)
		require.Empty(t, out(), "%v", tc.argv)
		require.Contains(t, errb(), tc.want, "%v", tc.argv)
	}
	require.Zero(t, fx.callCount())
}

// Usage errors are the parser's, exit 2: a kind outside the canonical set, a
// second name, a note list with nothing to search for.
func TestKnowledgeVerbs_ParseErrors(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"memory", "list", "--kind", "bogus"}, "valid values are constraint"},
		{[]string{"memory", "read", "a", "b"}, `unexpected argument "b"`},
		{[]string{"note", "read", "a", "b"}, `unexpected argument "b"`},
		{[]string{"note", "list"}, "missing argument: seam note list requires <word>..."},
		{[]string{"memory", "list", "--limit", "0", "--kind", "gotcha"}, "must be a positive integer"},
		{[]string{"memory", "bogus"}, `unknown command "memory bogus": valid values are memory read, memory list`},
	} {
		e, _, errb := stubEnv()
		require.Equal(t, 2, dispatch(context.Background(), e, tc.argv), "%v", tc.argv)
		require.Contains(t, errb.String(), tc.want, "%v", tc.argv)
	}
}
