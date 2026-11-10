package mcp_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func TestGardenerProposalsAndApply(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	// Create a memory through the tool so it exists on disk + index; an archive
	// apply then retires it.
	wrote := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "stale-thing", "kind": "gotcha",
		"description": "an old memory", "body": "body text", "project": "global",
	})
	memID, _ := wrote["id"].(string)
	require.NotEmpty(t, memID)

	// Seed archive + digest proposals directly.
	arch, err := store.CreateProposal(ctx, db, store.ProposalArchive, map[string]any{
		"key": "archive:" + memID, "id": memID, "name": "stale-thing",
	})
	require.NoError(t, err)
	dig, err := store.CreateProposal(ctx, db, store.ProposalDigest, map[string]any{
		"key": "digest:demo:2026-07", "project": "",
		"title": "Session digest -- 2026-07", "body": "- did work",
	})
	require.NoError(t, err)

	// gardener_proposals lists both; the kind filter narrows.
	all := callJSON(t, ctx, cli, "gardener_proposals", nil)
	require.Equal(t, float64(2), all["count"])
	archives := callJSON(t, ctx, cli, "gardener_proposals", map[string]any{"kind": "archive"})
	require.Equal(t, float64(1), archives["count"])

	// Apply the archive: the memory is retired (leaves the active index).
	applied := callJSON(t, ctx, cli, "gardener_apply", map[string]any{"id": arch.ID})
	require.Equal(t, "applied", applied["status"])
	require.Equal(t, "archive", applied["kind"])
	_, found, err := store.MemoryByName(ctx, db, "", "stale-thing")
	require.NoError(t, err)
	require.False(t, found, "archived memory must leave the active index")

	// Dismiss the digest: no side effect, just resolved.
	dismissed := callJSON(t, ctx, cli, "gardener_apply", map[string]any{"id": dig.ID, "action": "dismiss"})
	require.Equal(t, "dismissed", dismissed["status"])

	// Nothing pending now.
	none := callJSON(t, ctx, cli, "gardener_proposals", nil)
	require.Equal(t, float64(0), none["count"])

	// Applying an already-resolved proposal is a tool error, not a transport error.
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "gardener_apply", Arguments: map[string]any{"id": arch.ID},
	}})
	require.NoError(t, err)
	require.True(t, res.IsError)
}

// The agent surface carries both rejection tiers, and they resolve to different
// statuses. It also has to refuse an action it does not know rather than
// falling through to the apply default -- resolving a proposal is not something
// to guess at.
func TestGardenerApply_RejectionTiers(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	mk := func(key string) string {
		t.Helper()
		p, err := store.CreateProposal(ctx, db, store.ProposalToolError, map[string]any{
			"key": key, "project": "", "surface": "tool", "name": "tasks_add",
			"suggested_title": "tasks_add: " + key,
		})
		require.NoError(t, err)
		return p.ID
	}

	dismissed := callJSON(t, ctx, cli, "gardener_apply", map[string]any{"id": mk("err:a"), "action": "dismiss"})
	require.Equal(t, store.ProposalDismissed, dismissed["status"])

	hideID := mk("err:b")
	hidden := callJSON(t, ctx, cli, "gardener_apply", map[string]any{"id": hideID, "action": "hide"})
	require.Equal(t, store.ProposalHidden, hidden["status"])

	got, ok, err := store.ProposalByID(ctx, db, hideID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalHidden, got.Status)

	// An unknown action names the ones that exist instead of silently applying.
	isErr, txt := callErr(t, ctx, cli, "gardener_apply", map[string]any{"id": mk("err:c"), "action": "ignore"})
	require.True(t, isErr)
	require.Contains(t, txt, "hide")
	require.Equal(t, 1, len(mustPendingProposals(t, ctx, db)), "the refused proposal is untouched")
}

func mustPendingProposals(t *testing.T, ctx context.Context, db *sql.DB) []store.Proposal {
	t.Helper()
	ps, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	return ps
}

// TestGardenerRequest_NoChatIsToolError exercises the natural-language request
// tool on a server whose gardener has no chat client: it must surface a tool
// error rather than fabricate proposals. (A success-path test needs a
// chat-enabled server helper, which this fixture does not build.)
func TestGardenerRequest_NoChatIsToolError(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "gardener_request", Arguments: map[string]any{"request": "merge the duplicate memories"},
	}})
	require.NoError(t, err, "transport succeeds")
	require.True(t, res.IsError, "a gardener with no chat client returns a tool error")
}

// TestGardenerRequestScopeGuards covers the last tool argument that reached a
// service without passing the scope guards. Each rejection below was a SUCCESS
// before this: an unresolvable scope matched no rows, and "no active memories in
// scope" is indistinguishable from a genuinely empty project.
//
// The guards run before the interpretation, so the fixture's chat-less gardener
// is enough to exercise them: a rejection here means the scope was refused, and
// reaching the no-chat error means it was accepted.
func TestGardenerRequestScopeGuards(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	// A well-formed slug that is not a project. validate.Name only checks slug
	// SHAPE, so "typoed" passes every shared guard -- which is why the existence
	// check lives in this handler, and why the acceptance criterion needs it.
	isErr, txt := callErr(t, ctx, cli, "gardener_request", map[string]any{
		"request": "merge the duplicates", "project": "typoed",
	})
	require.True(t, isErr, "an unknown project must not report an empty scope as success")
	require.Contains(t, txt, `unknown project "typoed"`)

	// The traversal guard reaches this tool now too.
	isErr, txt = callErr(t, ctx, cli, "gardener_request", map[string]any{
		"request": "merge the duplicates", "project": "../notes/_global",
	})
	require.True(t, isErr, "an unsafe project slug must be rejected")
	require.Contains(t, txt, "invalid project")

	// Every accepted scope gets PAST the guards and fails on the missing chat
	// client instead -- which is what proves it was accepted rather than refused.
	//
	// "_global" is the sharpest of these: it is not a registered project, so it
	// can only clear the existence check if normalizeProject mapped it to the
	// global scope first. Unnormalized, it used to reach ActiveMemories("_global"),
	// match nothing, and report success. "demo" clears it because session_start
	// registers the project it resolves from the cwd (RegisterProjectForCWD
	// backfills EnsureProject), which is what keeps this check from ever
	// rejecting a real, session-reachable project.
	for _, project := range []string{"global", "_global", "all", "demo"} {
		isErr, txt := callErr(t, ctx, cli, "gardener_request", map[string]any{
			"request": "merge the duplicates", "project": project,
		})
		require.True(t, isErr, "the chat-less fixture always errors")
		require.NotContains(t, txt, "unknown project", "project %q must be accepted as a scope", project)
		require.NotContains(t, txt, "invalid project", "project %q must be accepted as a scope", project)
	}
}

// TestProjectCreateRejectsAllToken pins the reservation the widening token needs:
// without it a project could take the name "all" and then be permanently
// unreachable through gardener_request, which reads the token before it resolves
// anything as a slug.
func TestProjectCreateRejectsAllToken(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	isErr, txt := callErr(t, ctx, cli, "project_create", map[string]any{"name": "All", "slug": "all"})
	require.True(t, isErr, `slug "all" must be reserved`)
	require.Contains(t, txt, "reserved")
}

// TestGardenerSplit_NoChatIsToolError exercises the project-split tool on a
// server whose gardener has no chat client: it must surface a tool error rather
// than create any projects or proposals.
func TestGardenerSplit_NoChatIsToolError(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "gardener_split", Arguments: map[string]any{"source": "example-app", "instruction": "split into ios and android"},
	}})
	require.NoError(t, err, "transport succeeds")
	require.True(t, res.IsError, "a gardener with no chat client returns a tool error")
}

// seedProposal files a proposal row directly, standing in for a pass that ran
// before the fence question is asked.
func seedProposal(t *testing.T, ctx context.Context, db *sql.DB, kind string, payload map[string]any) string {
	t.Helper()
	payload["key"] = kind + ":" + t.Name() + ":" + payloadKeySuffix(payload)
	p, err := store.CreateProposal(ctx, db, kind, payload)
	require.NoError(t, err)
	return p.ID
}

// payloadKeySuffix keeps two seeded proposals in one test from colliding on the
// dedup key.
func payloadKeySuffix(payload map[string]any) string {
	if id, ok := payload["id"].(string); ok {
		return id
	}
	return "x"
}

// pendingIDs lists the still-pending proposal ids, for asserting that a refused
// resolve left the queue exactly as it found it.
func pendingIDs(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	ps, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// proposalIDs pulls the ids out of a gardener_proposals result.
func proposalIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	rows, ok := out["proposals"].([]any)
	require.True(t, ok, "proposals is a list")
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		require.True(t, ok, "each proposal is an object")
		id, _ := obj["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestGardenerProposalsOmitsWhatTheCallerMayNotRead is the listing half of the
// gardener fence. A proposal row carries no project column, so before this the
// whole pending queue -- payloads included, with every memory name, description
// and similarity score in them -- went to any caller that asked.
//
// The drop is silent by design: no "1 withheld" count, since that is itself a
// report of how much a fenced project has going on.
func TestGardenerProposalsOmitsWhatTheCallerMayNotRead(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "vault", core.IsolationSealed)

	vault := bindClient(t, ctx, db, url, "vault")
	vaultMem := callJSON(t, ctx, vault, "memory_write", map[string]any{
		"name": "vault-thing", "kind": "gotcha", "description": "fenced", "body": "b\n",
	})["id"].(string)

	outside := bindClient(t, ctx, db, url, "demo")
	openMem := callJSON(t, ctx, outside, "memory_write", map[string]any{
		"name": "open-thing", "kind": "gotcha", "description": "open", "body": "b\n",
	})["id"].(string)

	fenced := seedProposal(t, ctx, db, store.ProposalArchive,
		map[string]any{"id": vaultMem, "name": "vault-thing", "project": "vault"})
	open := seedProposal(t, ctx, db, store.ProposalArchive,
		map[string]any{"id": openMem, "name": "open-thing", "project": "demo"})
	require.Len(t, pendingIDs(t, ctx, db), 2, "both rows exist in the store")

	seen := callJSON(t, ctx, outside, "gardener_proposals", nil)
	require.Equal(t, float64(1), seen["count"])
	require.Equal(t, []string{open}, proposalIDs(t, seen), "the sealed project's row is not listed")

	// The kind filter does not become a way around it.
	filtered := callJSON(t, ctx, outside, "gardener_proposals", map[string]any{"kind": "archive"})
	require.Equal(t, []string{open}, proposalIDs(t, filtered))

	// The fenced project's own session still sees its own row -- and, being
	// sealed, nothing else.
	mine := callJSON(t, ctx, vault, "gardener_proposals", nil)
	require.Equal(t, []string{fenced}, proposalIDs(t, mine))
}

// TestGardenerApplyRefusesAcrossTheFence is the resolve half. Apply AND dismiss
// are both refused: dismissing settles a fenced project's review queue, and the
// call's own outcome would report that the proposal exists.
func TestGardenerApplyRefusesAcrossTheFence(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "vault", core.IsolationSealed)

	vault := bindClient(t, ctx, db, url, "vault")
	vaultMem := callJSON(t, ctx, vault, "memory_write", map[string]any{
		"name": "vault-thing", "kind": "gotcha", "description": "fenced", "body": "b\n",
	})["id"].(string)
	fenced := seedProposal(t, ctx, db, store.ProposalArchive,
		map[string]any{"id": vaultMem, "name": "vault-thing", "project": "vault"})

	outside := bindClient(t, ctx, db, url, "demo")
	for _, action := range []string{"apply", "dismiss"} {
		isErr, txt := callErr(t, ctx, outside, "gardener_apply", map[string]any{"id": fenced, "action": action})
		require.True(t, isErr, "%s across the fence must be refused", action)
		require.Contains(t, txt, "vault", "the refusal names the project, which it never hides")
		require.Contains(t, txt, string(core.IsolationSealed), "and the rule that refused")
		require.NotContains(t, txt, "fenced", "no proposal content is echoed")
	}
	require.Equal(t, []string{fenced}, pendingIDs(t, ctx, db), "a refused resolve leaves the queue untouched")

	// The project's own session resolves it normally: the fence is about who is
	// outside, not about disabling the proposal.
	applied := callJSON(t, ctx, vault, "gardener_apply", map[string]any{"id": fenced})
	require.Equal(t, "applied", applied["status"])
}

// TestGardenerApplyRechecksIsolationAtApplyTime is the stale-proposal case: a
// reproject raised while its source project was open, applied after the project
// was tightened. The proposal's evidence predates the fence, so the check that
// matters is the one made NOW -- otherwise a pre-tighten row is a standing
// instruction to carry a memory out of a project that has since closed.
func TestGardenerApplyRechecksIsolationAtApplyTime(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	_, err := store.EnsureProject(ctx, db, "client-work", "Client work")
	require.NoError(t, err)

	work := bindClient(t, ctx, db, url, "client-work")
	memID := callJSON(t, ctx, work, "memory_write", map[string]any{
		"name": "deploy-topology", "kind": "reference", "description": "how it deploys", "body": "b\n",
	})["id"].(string)
	stale := seedProposal(t, ctx, db, store.ProposalReproject, map[string]any{
		"id": memID, "name": "deploy-topology", "from": "client-work", "to": "demo",
	})

	// Before the tighten the outside caller sees it and could resolve it: without
	// this the test would pass on a proposal that was never visible at all.
	outside := bindClient(t, ctx, db, url, "demo")
	require.Equal(t, []string{stale}, proposalIDs(t, callJSON(t, ctx, outside, "gardener_proposals", nil)))

	require.NoError(t, store.SetProjectIsolation(ctx, db, "client-work", core.IsolationConfidential))

	require.Empty(t, proposalIDs(t, callJSON(t, ctx, outside, "gardener_proposals", nil)),
		"the tighten retroactively withdraws the proposal from outside callers")
	isErr, txt := callErr(t, ctx, outside, "gardener_apply", map[string]any{"id": stale})
	require.True(t, isErr, "a pre-tighten proposal must not still move a memory out of the fence")
	require.Contains(t, txt, "client-work")
	require.Contains(t, txt, string(core.IsolationConfidential))
	require.Equal(t, []string{stale}, pendingIDs(t, ctx, db))
}

// TestGardenerFailsClosedOnAnUnattributableProposal pins the direction an
// undecidable payload fails in. A digest with no project key is not "probably
// global" -- "" is a real scope, so an absent key leaves nothing to check a fence
// against, and the row leaves the agent surface entirely. The console is exempt
// and still shows it to the owner.
func TestGardenerFailsClosedOnAnUnattributableProposal(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	unattributable := seedProposal(t, ctx, db, store.ProposalDigest,
		map[string]any{"month": "2026-07", "title": "Session digest", "body": "- did work"})

	require.Empty(t, proposalIDs(t, callJSON(t, ctx, cli, "gardener_proposals", nil)),
		"an unattributable proposal is listed to nobody, the global scope included")
	isErr, txt := callErr(t, ctx, cli, "gardener_apply", map[string]any{"id": unattributable})
	require.True(t, isErr)
	require.Contains(t, txt, "names no project")
	require.Contains(t, txt, "console", "the refusal names the remedy")
	require.Equal(t, []string{unattributable}, pendingIDs(t, ctx, db))
}
