package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// The words the switch-on surfaces give an agent about a move, and the tool
// descriptions only isolation.requests changes (plan fenced-sessions P11):
// each promise they make is checked against what the server then does.

// p11Tool is the tool named name as cli's server lists it.
func p11Tool(t *testing.T, cli *mcpclient.Client, name string) mcp.Tool {
	t.Helper()
	tools, err := cli.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name == name {
			return tool
		}
	}
	require.FailNow(t, "not registered", name)
	return mcp.Tool{}
}

// p11CatalogTool is the Catalog's definition of name: the words the docs
// render, and the words every server without the switch registers.
func p11CatalogTool(t *testing.T, name string) mcp.Tool {
	t.Helper()
	for _, tool := range mcpserver.Catalog() {
		if tool.Name == name {
			return tool
		}
	}
	require.FailNow(t, "not in the catalog", name)
	return mcp.Tool{}
}

// A move out of an open project into a confidential one: its remedy says the
// conversation may still close a task it claimed there with a receipt, its
// notice names the session whose claims count and the receipt's one
// reference, and the receipt it describes closes the task -- while every
// other write into the home it left is refused.
func TestFenced_P11MoveWordsPromiseAReceiptThatWorks(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	task := r.ok(r.cli, "tasks_add", map[string]any{"title": "build step"})["id"].(string)
	claimed := r.ok(r.cli, "tasks_claim", map[string]any{"id": task})
	require.Equal(t, "in_progress", claimed["status"])

	text := r.refused(r.cli, "notes_read", map[string]any{"id": r.finNote})
	require.Contains(t, text, "until the conversation ends. After the move it can still close a task it claimed there, with "+
		"a receipt: tasks_update with status done or dropped, and no text. Its first result after the approval")
	r.approveMove(gShort.FindString(text))

	items, isErr := r.call(r.cli, "project_list", nil)
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 2, "%v", items)
	require.Contains(t, items[1], "until the conversation ends. It can still close a task its session cc/fs-agent claimed "+
		"before the move, with a receipt: tasks_update with status done or dropped and no text, optionally with "+
		"reference=example-finance:<id> naming one item in example-finance. This is its briefing, sent once:\n")
	require.NotContains(t, items[1], "citing", "an open home's move says nothing of citing it")

	// The receipt the notice describes closes the step...
	got := r.ok(r.cli, "tasks_update", map[string]any{"id": task, "status": "done", "reference": gFin + ":" + r.finNote})
	require.Equal(t, true, got["receipt"], "%v", got)
	require.Equal(t, "done", got["status"])
	// ... and nothing else is written where the conversation came from.
	refusal := r.refused(r.cli, "memory_write", map[string]any{"project": gOps, "name": "after-move", "kind": "reference",
		"description": "d", "body": "b\n"})
	require.Contains(t, refusal, "a write into project "+gOps+" is refused")
}

// A move into a sealed project offers no receipt, in its remedy or in its
// notice, because a conversation whose home is sealed sends none: the close
// the words would have promised is refused.
func TestFenced_P11NoReceiptOfferedIntoASealedProject(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	task := r.ok(r.cli, "tasks_add", map[string]any{"title": "build step"})["id"].(string)
	r.ok(r.cli, "tasks_claim", map[string]any{"id": task})

	text := r.refused(r.cli, "session_start", map[string]any{"project": gVault})
	require.Contains(t, text, "Record what belongs in "+gOps+" now: ")
	require.NotContains(t, text, "receipt")
	r.approveMove(gShort.FindString(text))

	items, isErr := r.call(r.cli, "project_list", nil)
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 2, "%v", items)
	require.True(t, strings.HasPrefix(items[1], "This conversation moved into "+gVault+": "), items[1])
	require.NotContains(t, items[1], "receipt")
	require.NotContains(t, items[1], "citing", "an open home's move says nothing of citing it")

	refusal := r.refused(r.cli, "tasks_update", map[string]any{"id": task, "status": "done"})
	require.Contains(t, refusal, "a conversation whose home is sealed project "+gVault+" sends no receipts")
}

// A move out of a fenced home -- always into a sealed project -- tells the
// conversation where what it writes about that home goes, citing its items
// in the one form the gardener reads; and that is where it can write.
func TestFenced_P11MoveOutOfAFencedHomeSaysWhereItsWritingGoes(t *testing.T) {
	r := newFSRig(t, fsOrigins[1])
	text := r.refused(r.cli, "session_start", map[string]any{"project": gVault})
	require.NotContains(t, text, "receipt", "a sealed home sends none")
	r.approveMove(gShort.FindString(text))

	items, isErr := r.call(r.cli, "project_list", nil)
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 2, "%v", items)
	require.True(t, strings.HasSuffix(items[1], "until the conversation ends. What it writes about "+gLegal+" goes in "+
		gVault+", citing "+gLegal+"'s items by reference, each written "+gLegal+":<id> with the item's full id."), items[1])
	require.NotContains(t, items[1], "receipt")

	r.refused(r.cli, "memory_write", map[string]any{"project": gLegal, "name": "about-legal", "kind": "reference",
		"description": "d", "body": "b\n"})
	mem := r.ok(r.cli, "memory_write", map[string]any{"name": "about-legal", "kind": "reference", "description": "d",
		"body": "Cites " + gLegal + ":01M7998NT13A8CHQSE5XCS05WR.\n"})
	require.Equal(t, gVault, mem["project"])
}

// A conversation whose home is sealed may not act as an open project's
// session: not through a session= handle, and not through a session_start
// binding made before it moved in. The refusal names the fence that refused,
// the home's own -- every other conversation reads an open project -- never
// the open project's.
func TestFenced_P11ASealedHomeIsKeptInByItsOwnFence(t *testing.T) {
	sealedAt := func(t *testing.T, moved bool) *fsRig {
		t.Helper()
		return newFSRig(t, fsOrigin{name: "sealed", home: gVault, conf: gFin, moved: moved})
	}
	for _, moved := range []bool{false, true} {
		r := sealedAt(t, moved)
		r.bind(gHR, "cc/hr")
		text := r.refused(r.cli, "memory_write", map[string]any{"session": "cc/hr", "name": "x", "kind": "reference",
			"description": "d", "body": "b\n"})
		require.Equal(t, "memory_write: session cc/hr works in project "+gHR+", and this conversation works in project "+
			gVault+": its home's fence keeps its reads out of project "+gHR+", so it may not act as that session\n"+
			"Remedy: leave session= off to act as your own session "+r.homeName+" in project "+gVault, text)
		require.NotContains(t, text, gHR+" is open")
	}

	// A binding made from an open home, before the move into the sealed one.
	r := newFSRig(t, fsOrigins[0])
	bound := r.ok(r.cli, "session_start", map[string]any{"project": gHR})
	require.Equal(t, gHR, bound["project"])
	text := r.refused(r.cli, "session_start", map[string]any{"project": gVault})
	done := r.approveMove(gShort.FindString(text))
	items, isErr := r.call(r.cli, "memory_write", map[string]any{"name": "y", "kind": "reference", "description": "d",
		"body": "b\n"})
	require.True(t, isErr, "%v", items)
	require.Equal(t, "memory_write: this connection is bound to session "+bound["name"].(string)+" in project "+gHR+
		", and this conversation works in project "+gVault+": its home's fence keeps its reads out of project "+gHR+
		", so it may no longer act as that session\nRemedy: call session_start with no arguments to hand this connection "+
		"back to your own session "+done.Child.Name+" in project "+gVault, items[0])
}

// session_start is registered in the Catalog's words on a server without
// isolation.requests, and with them on adds what the switch changes -- which
// is what session_start then does for an identified conversation.
func TestFenced_P11SessionStartWordsFollowTheSwitch(t *testing.T) {
	catalog := p11CatalogTool(t, "session_start")

	off := newGrantRig(t, false)
	plain := p11Tool(t, dialAgent(t, context.Background(), off.url, fsAgent), "session_start")
	require.Equal(t, catalog.Description, plain.Description, "off, word for word")

	r := newFSRig(t, fsOrigins[0])
	on := p11Tool(t, r.cli, "session_start")
	require.Equal(t, catalog.Description+" With isolation.requests on, as on this server, a Claude Code or Codex agent's "+
		"conversation keeps one home, the project its own session is in: session_start into a confidential or sealed "+
		"project binds nothing and asks the owner instead -- to move this conversation into it for good (from an open "+
		"project, or into a sealed one), or for a grant of it (from a confidential project) -- and the refusal names the "+
		"request; into an open project or the global scope it is refused once the conversation holds a fenced project's "+
		"content.", on.Description)
	require.ElementsMatch(t, catalog.InputSchema.Required, on.InputSchema.Required)
	require.Equal(t, advPropNames(catalog.InputSchema.Properties), advPropNames(on.InputSchema.Properties))

	text := r.refused(r.cli, "session_start", map[string]any{"project": gFin})
	require.True(t, strings.HasPrefix(text, "session_start: nothing was bound: "), text)
	require.Contains(t, text, "\nRemedy: move request R-")
	c := newFSRig(t, fsOrigins[1])
	text = c.refused(c.cli, "session_start", map[string]any{"project": gFin})
	require.Contains(t, text, "\nRemedy: read-and-write request R-", "from a confidential home, a grant")
}

// access_request under peer_auth with isolation.requests on is registered in
// words that say what the switch asks for; and what they say a lock's lease
// alone does not do -- open a locked project to a conversation working
// elsewhere -- it does not.
func TestFenced_P11AccessRequestWordsUnderPeerAuth(t *testing.T) {
	catalog := p11CatalogTool(t, mcpserver.AccessRequestTool)
	r := newFPRig(t, "native open", nil)
	listed := p11Tool(t, r.agent, mcpserver.AccessRequestTool)
	require.NotEqual(t, catalog.Description, listed.Description)
	require.True(t, strings.HasPrefix(listed.Description, "Ask the owner for a locked, confidential or sealed project. "+
		"Present only on a server that locks projects (peer_auth), and answered only on its peer_auth socket; this server "+
		"also has isolation.requests on."), listed.Description)
	require.Contains(t, listed.Description, "a lease alone opens a locked project only to the conversation that works in it")
	require.Equal(t, catalog.Annotations, listed.Annotations)
	require.Equal(t, advPropNames(catalog.InputSchema.Properties), advPropNames(listed.InputSchema.Properties))
	require.Equal(t, catalog.InputSchema.Required, listed.InputSchema.Required)

	// Without write, a locked project is asked for its lease...
	out, _ := r.ok(r.agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	require.Equal(t, store.AccessKindRequest, res["kind"], out)
	r.approve(res["id"].(string), time.Hour)
	// ... and the lease alone opens the lock, not the fence: the conversation
	// working in example-ops is still refused, and asked to move in.
	text := r.refused(r.agent, "notes_read", map[string]any{"id": r.notes[lkFin]})
	require.NotContains(t, text, lkFin+" planted notes")
	require.True(t, strings.HasPrefix(text, "notes_read: project "+lkFin+" is confidential: only a conversation working in "+
		"it reads it"), text)
	require.Contains(t, text, "\nRemedy: move request R-")
}
