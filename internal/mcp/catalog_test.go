package mcp

import (
	"context"
	"slices"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/agentguide"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// TestCatalogMatchesRegistration is the contract that keeps the generated docs
// honest: Catalog must list exactly the tools registerTools registers, in the
// same order. A new tool wired into only one of the two trips this rather than
// quietly going undocumented (or documenting a tool the server never serves).
// It is a same-package test so it can read the server's private toolNames --
// the registration record itself, not a second hand-maintained list.
func TestCatalogMatchesRegistration(t *testing.T) {
	names := make([]string, 0, len(Catalog()))
	for _, tool := range Catalog() {
		names = append(names, tool.Name)
	}

	// A server that locks a project registers the whole catalog.
	locked := New(Config{OfferAccessRequest: true})
	require.Equal(t, locked.toolNames, names, "Catalog order/content must mirror registerTools")
	require.Len(t, names, LockedToolCount)
	require.Equal(t, LockedToolCount, ToolCountWith(true))

	// Every other one -- every default install -- registers the catalog minus
	// the locked-only tools, in the same order, and ToolCount counts that.
	defaults := New(Config{})
	want := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(LockedOnlyTools(), n) })
	require.Equal(t, want, defaults.toolNames)
	require.Len(t, defaults.toolNames, ToolCount)
	require.Equal(t, ToolCount, ToolCountWith(false))
	require.Equal(t, LockedOnlyTools(), names[len(want):], "the locked-only tools come last in Catalog")
}

// TestAddToolRecordsEverySchema pins what makes validation impossible to forget:
// addTool records the schema in the same statement that registers the tool, so a
// registered tool cannot lack one. Without this, a tool added through some other
// path would dispatch unvalidated -- silently restoring the pre-validator
// behavior for exactly that tool, which is the failure mode hardest to notice.
//
// Same-package so it reads the registration record itself rather than a second
// hand-maintained list.
func TestAddToolRecordsEverySchema(t *testing.T) {
	srv := New(Config{})

	require.Len(t, srv.toolSchemas, ToolCount)
	for _, name := range srv.toolNames {
		schema, ok := srv.toolSchema(name)
		require.True(t, ok, "%s: registered with no input schema -- it would dispatch unvalidated", name)
		require.Equal(t, "object", schema.Type, "%s: an input schema is a JSON object", name)
	}
}

func TestResultLimitsAreIntegerAndBounded(t *testing.T) {
	wantMax := map[string]int{
		"recall":      retrieve.MaxRecallLimit,
		"trial_query": store.MaxTrialQueryLimit,
	}
	for _, tool := range Catalog() {
		max, ok := wantMax[tool.Name]
		if !ok {
			continue
		}
		prop, ok := tool.InputSchema.Properties["limit"].(map[string]any)
		require.True(t, ok, "%s limit schema", tool.Name)
		require.Equal(t, "integer", prop["type"], "%s must advertise true integer semantics", tool.Name)
		require.EqualValues(t, 1, prop["minimum"])
		require.EqualValues(t, max, prop["maximum"])
		delete(wantMax, tool.Name)
	}
	require.Empty(t, wantMax, "every bounded result tool must be present in the catalog")
}

// A tool the server never registered fails closed rather than dispatching
// unvalidated. Unreachable through addTool by construction (the test above), so
// this pins the fallback itself: mcp-go's per-session tools are the only path
// that could produce one, and an unvalidatable call must not proceed.
func TestValidateMiddlewareFailsClosedWithoutASchema(t *testing.T) {
	srv := New(Config{})

	var reached bool
	h := srv.validateMiddleware(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reached = true
		return mcp.NewToolResultText("ok"), nil
	})
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "never_registered"},
	})

	require.NoError(t, err)
	require.True(t, res.IsError)
	require.False(t, reached, "an unvalidatable call must not reach its handler")
}

// TestCatalogToolsAreDocumentable guards the inputs docsgen renders: a tool with
// no description would emit an empty docs section, and a duplicate name would
// collide on the page anchor (/docs/reference/mcp/tasks/#tasks_claim).
func TestCatalogToolsAreDocumentable(t *testing.T) {
	seen := make(map[string]bool)
	for _, tool := range Catalog() {
		require.NotEmpty(t, tool.Name, "every catalog tool has a name")
		require.NotEmpty(t, tool.Description, "%s: description is the docs body", tool.Name)
		require.False(t, seen[tool.Name], "duplicate tool name %q", tool.Name)
		seen[tool.Name] = true
	}
}

// Server-wide guidance must never teach a workflow around a tool that was
// renamed or removed. The canonical names live with the guidance; Catalog is
// the served/help-text surface that proves each one still exists.
func TestAgentGuidanceNamesRealTools(t *testing.T) {
	served := make(map[string]bool, len(Catalog()))
	for _, tool := range Catalog() {
		served[tool.Name] = true
	}
	for _, name := range agentguide.RequiredToolNames() {
		require.True(t, served[name], "agent guidance names missing MCP tool %q", name)
	}
}

// The constraint-vs-convention call and the stage-header contract are both
// applied at write time, so the kind parameter must carry the shared strings
// -- not a paraphrase that can drift from the guidance agentguide serves
// elsewhere.
func TestMemoryWriteKindTeachesTheDiscriminator(t *testing.T) {
	prop, err := propSchema(memoryWriteTool().InputSchema, "kind")
	require.NoError(t, err)

	desc, _ := prop["description"].(string)
	require.Contains(t, desc, agentguide.KindDiscriminator)
	require.Contains(t, desc, agentguide.StageContract)
}

// asStrings widens a canonical set declared over a named string type, so the
// expectation below is the set itself rather than a second transcription of it.
func asStrings[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// TestArgsEnumsDeriveFromCanonicalSets is the anti-drift gate: every enum a tool
// advertises must BE its canonical set, exactly and in order.
//
// It is distinct from argspec_test's TestEnumOf_DerivesFromCanonicalSets, which
// proves enumOf builds a correct enum from a set. That one passes even if no
// tool calls enumOf -- it tests the helper. This one tests the declarations, so
// a tool that goes back to hand-listing fails the build.
//
// It exists because transcribing a set is a silent failure with a live example:
// tools_gardener hand-listed 6 of store.ProposalKinds' 7 kinds, so abandon_plan
// -- which migration 005, the gardener, the applier, and the console all handle
// -- was undiscoverable at the boundary for months. Nothing errored; the schema
// simply lied. Now a 9th MemoryKind, or an 8th proposal kind, fails here unless
// it reaches the schema too.
//
// Since validateMiddleware enforces the declared enum, this also pins that the
// values enforced at the boundary are the ones the handlers' Valid() checks
// accept: a transcription slip would otherwise reject a value the store takes,
// which is exactly how enforcing the old hand-written list would have regressed
// abandon_plan.
func TestArgsEnumsDeriveFromCanonicalSets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  func() mcp.Tool
		param string
		want  []string
	}{
		{"memory_write.kind", memoryWriteTool, "kind", asStrings(core.MemoryKinds)},
		{"project_create.isolation", projectCreateTool, "isolation", asStrings(core.Isolations)},
		{"tasks_update.status", tasksUpdateTool, "status", asStrings(core.TaskStatuses)},
		{"tasks_list.status", tasksListTool, "status", asStrings(core.TaskStatuses)},
		{"gardener_proposals.kind", gardenerProposalsTool, "kind", store.ProposalKinds},
		{"gardener_apply.action", gardenerApplyTool, "action", gardener.Decisions},
		{"recall.scope", recallTool, "scope", retrieve.RecallScopes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prop, err := propSchema(tc.tool().InputSchema, tc.param)
			require.NoError(t, err)

			got, ok, err := enumValues(prop, tc.param)
			require.NoError(t, err)
			require.True(t, ok, "%s: declares no enum -- an undeclared enum is not enforced", tc.name)
			require.Equal(t, tc.want, got, "%s: the schema's enum must BE the canonical set, not a copy of it", tc.name)
		})
	}
}
