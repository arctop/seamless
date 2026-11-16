package mcp

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The read-only surface: tools that answer from the store and write nothing.
// lab_open is deliberately absent -- it binds the lab to the connection before
// it reads, so it is a setter that happens to return history.
var readOnlyTools = []string{
	"gardener_proposals",
	"locks_list",
	"memory_read",
	"notes_read",
	"project_list",
	"recall",
	"tasks_list",
	"tasks_ready",
	"trial_query",
	"usage_summary",
}

// The destructive surface: calls that can delete or overwrite durable content
// -- a memory or note removed, rewritten in place, or superseded. Updates to
// sessions, tasks, trials, and favorites are transitions over high-churn DB
// state and do not belong here (see annotations.go).
var destructiveTools = []string{
	"gardener_apply",
	"memory_delete",
	"memory_edit",
	"memory_write",
	"notes_delete",
	"notes_edit",
	"notes_update",
}

// TestEveryToolDeclaresItsHints is the regression guard for the bug that
// started this: a constructor that passes no annotation does not serve "no
// opinion", it serves the MCP spec's pessimistic defaults (readOnlyHint false,
// destructiveHint true). Every tool must say all four out loud, so a new tool
// added without a hint fails here rather than shipping as "may destroy things".
func TestEveryToolDeclaresItsHints(t *testing.T) {
	for _, tool := range Catalog() {
		ann := tool.Annotations
		require.NotNil(t, ann.ReadOnlyHint, "%s: no readOnlyHint -- the spec default (false) would ship instead", tool.Name)
		require.NotNil(t, ann.DestructiveHint, "%s: no destructiveHint -- the spec default (TRUE) would ship instead", tool.Name)
		require.NotNil(t, ann.IdempotentHint, "%s: no idempotentHint", tool.Name)
		require.NotNil(t, ann.OpenWorldHint, "%s: no openWorldHint", tool.Name)
	}
}

// A read-only tool that also advertises destructiveHint is the exact
// contradiction Glama's tool grader caught in gardener_proposals: a single
// SELECT whose description ended "Read-only." while its annotations said it
// might destroy things. A client gating confirmation on the hint cannot resolve
// that, so it must be unrepresentable.
func TestReadOnlyToolsAreNeverDestructive(t *testing.T) {
	for _, tool := range Catalog() {
		if !*tool.Annotations.ReadOnlyHint {
			continue
		}
		require.False(t, *tool.Annotations.DestructiveHint,
			"%s: reads nothing but advertises destructive updates", tool.Name)
		require.True(t, *tool.Annotations.IdempotentHint,
			"%s: a call that writes nothing is idempotent by construction", tool.Name)
	}
}

// The prose and the hints describe the same tool to the same reader, so a
// description that calls itself read-only must be annotated that way. This
// generalizes the specific contradiction rather than pinning one tool: any
// future "Read-only." added to a writer's description fails here.
func TestDescriptionsClaimingReadOnlyAreAnnotatedReadOnly(t *testing.T) {
	for _, tool := range Catalog() {
		if !strings.Contains(strings.ToLower(tool.Description), "read-only") {
			continue
		}
		require.True(t, *tool.Annotations.ReadOnlyHint,
			"%s: description says read-only, annotations say otherwise", tool.Name)
	}
}

// The classification itself. The invariants above cannot tell a misfiled tool
// from a correct one -- only an explicit expectation can -- and these three
// sets are what a client acts on: what it may call freely, what it must gate,
// and what leaves the machine.
func TestToolHintClassification(t *testing.T) {
	var readOnly, destructive, openWorld []string
	for _, tool := range Catalog() {
		if *tool.Annotations.ReadOnlyHint {
			readOnly = append(readOnly, tool.Name)
		}
		if *tool.Annotations.DestructiveHint {
			destructive = append(destructive, tool.Name)
		}
		if *tool.Annotations.OpenWorldHint {
			openWorld = append(openWorld, tool.Name)
		}
	}
	sort.Strings(readOnly)
	sort.Strings(destructive)

	require.Equal(t, readOnlyTools, readOnly)
	require.Equal(t, destructiveTools, destructive)
	require.Equal(t, []string{"capture_url"}, openWorld,
		"capture_url is the only tool whose domain is the internet")
}
