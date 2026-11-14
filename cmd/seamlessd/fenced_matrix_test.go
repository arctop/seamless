package main

// The fenced-sessions matrix, end to end (plan fenced-sessions P12): every
// cell of tier x target x conversation x action on one daemon wired as serve
// wires it (newGMRig), through the surfaces an agent really uses -- its hooks
// and its MCP connection -- with the approvals the owner really makes.
//
//   - tier: peer_auth off (the advisory tier: the conversation is the
//     client's session, named by the agent-process identity; the bearer key
//     approves over the console) or on (the agent's kernel-verified process
//     tree; seamlessd approve's daemon-account proof);
//   - target: the project the conversation works in -- confidential
//     (example-conf), sealed (example-sealed) or locked (example-finance:
//     confidential, and locked under peer_auth; without peer_auth there is
//     no lock, and it is a confidential project like any other);
//   - conversation: native (it started in the target's repository) or moved
//     (it started in an open project's repository and the owner approved its
//     move into the target);
//   - action: rebind (session_start), session=, receipt, write grant, stamps,
//     subagent, captures, harvest, resume and /clear, in that order on one
//     conversation, since the last three end it.
//
// The rows hold the design (memory fenced-sessions-design, the plan's
// narrative): the conversation works in its home and its record goes there
// wherever its hooks fire from; there is no way back to an open project or
// the global scope; a receipt is the one content-free write into the home a
// move left, never from a sealed home; a grant is between confidential
// projects only, read and write when the owner chooses it, and what is
// written under it is stamped; a resume resumes the home, and a lock whose
// lease ended answers with its notice; /clear starts a new conversation
// without peer_auth, and under it the label stays with the process tree.

import (
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// fmCase is one setup of the matrix: the tier, the project the conversation
// works in, and whether it moved there.
type fmCase struct {
	peer   bool
	target string
	moved  bool
}

func (c fmCase) String() string {
	tier, origin := "peer_auth off", "native"
	if c.peer {
		tier = "peer_auth on"
	}
	if c.moved {
		origin = "moved"
	}
	return tier + "/" + map[string]string{gmConf: "confidential", gmSealed: "sealed", gmFin: "locked"}[c.target] + "/" + origin
}

// id is the conversation's client session id for c (a client's ids hold no
// space).
func (c fmCase) id() string {
	return "fm-" + strings.NewReplacer(" ", "-", "/", "-", "_", "-").Replace(c.String())
}

// locked reports whether the target is locked: under peer_auth only.
func (c fmCase) locked() bool { return c.peer && c.target == gmFin }

// sealed reports whether the target is sealed.
func (c fmCase) sealed() bool { return c.target == gmSealed }

// holder is how a refusal names who holds the content: the agent's process
// tree under peer_auth, the session without it.
func (c fmCase) holder() string {
	if c.peer {
		return "agent"
	}
	return "session"
}

// fmRow is one set-up conversation of the matrix.
type fmRow struct {
	c    fmCase
	r    *gmRig
	conv *fmConv
	// cli is the conversation's MCP connection.
	cli *mcpclient.Client
	// origin is where it started: the target, or for a move the open project.
	origin string
	// start is the row its SessionStart recorded -- after a move, the row it
	// left, ended with its pointer -- and step, for a move, a task it claimed
	// in the open home before moving.
	start core.Session
	step  string
	// openStep is a step another agent claimed in the open project.
	openStep string
	// grant is the read-and-write grant's request, once the write grant row
	// approved it.
	grant string
	// note is a note in the target carrying its secret.
	note string
}

// fmSetUp plants the daemon's content and sets the conversation up for c:
// its SessionStart in its repository (a locked project's asks for the lease,
// which the owner approves), then, for a move, a step claimed in the open
// home, the move request session_start files (a locked target's through
// access_request with write, which carries the lease) and the owner's
// approval.
func fmSetUp(t *testing.T, c fmCase) *fmRow {
	t.Helper()
	r := newGMRig(t, c.peer, true)
	row := &fmRow{c: c, r: r, origin: c.target, note: r.fmPlant(c.target)}
	if c.moved {
		row.origin = gmOpen
	}
	other := paClient(t, r.tcp.URL, gmKey)
	gmOK(t, other, "session_start", map[string]any{"project": gmOpen, "name": "open-builder"})
	row.openStep = gmOK(t, other, "tasks_add", map[string]any{"title": "the builder's step"})["id"].(string)
	gmOK(t, other, "tasks_claim", map[string]any{"id": row.openStep})

	row.conv = r.fmConversation(c.id(), row.origin)
	a := row.conv.start("startup")
	if c.locked() && !c.moved {
		require.Contains(t, a.context, "project "+gmFin+" is locked", a.context)
		r.fmApprove(fmShortOf(t, a.context), "")
	} else {
		require.Contains(t, a.context, row.origin+" house rule", "the start is briefed from its repository's project")
	}
	row.start = row.conv.ambient()
	require.Equal(t, row.origin, row.start.ProjectSlug)
	row.cli = row.conv.dial()
	if !c.moved {
		return row
	}
	row.step = gmOK(t, row.cli, "tasks_add", map[string]any{"title": "the moved agent's step", "body": "the step's body"})["id"].(string)
	gmOK(t, row.cli, "tasks_claim", map[string]any{"id": row.step})
	var short string
	if c.locked() {
		asked := gmOK(t, row.cli, "access_request", map[string]any{"project": gmFin, "write": true})
		require.Equal(t, store.AccessKindMove, asked["kind"], "%v", asked)
		short = asked["short_id"].(string)
	} else {
		text := fmRefused(t, row.cli, "session_start", map[string]any{"project": c.target})
		require.Contains(t, text, "\nRemedy: move request R-", text)
		short = fmShortOf(t, text)
	}
	f := r.fmApprove(short, "")
	require.Equal(t, store.AccessKindMove, f.Kind)
	items, isErr := gmCall(t, row.cli, "project_list", nil)
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 2, "the first result after the move announces it: %v", items)
	require.True(t, strings.HasPrefix(items[1], "This conversation moved into "+c.target+": the owner approved move request "+
		short+". "), items[1])
	require.Contains(t, items[1], c.target+" house rule", "and carries the new home's briefing")
	require.NotContains(t, items[1], gmOpen+" house rule")
	return row
}

// home is the conversation's home session now.
func (row *fmRow) home() core.Session { return row.conv.ambient() }

func TestFencedMatrix(t *testing.T) {
	for _, peer := range []bool{false, true} {
		for _, target := range []string{gmConf, gmSealed, gmFin} {
			for _, moved := range []bool{false, true} {
				c := fmCase{peer: peer, target: target, moved: moved}
				t.Run(c.String(), func(t *testing.T) { fmRun(t, c) })
			}
		}
	}
}

// fmRun runs every action of the matrix on one conversation set up for c.
func fmRun(t *testing.T, c fmCase) {
	row := fmSetUp(t, c)
	if c.moved {
		left, ok, err := store.SessionByID(row.r.ctx, row.r.db, row.start.ID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, core.SessionCompleted, left.Status, "the move ended the row it left")
		require.False(t, left.Ambient)
		require.Equal(t, store.MovePointer(c.target, row.home().Name), left.Findings, "a metadata-only pointer")
		require.Equal(t, left.ID, row.home().ParentSessionID, "the home is the move's child")
	}
	if !c.peer {
		defer func() {
			require.Zero(t, row.r.count(`SELECT COUNT(*) FROM access_approvals WHERE kind = 'request'`),
				"without peer_auth there is no lock, and nothing asks for a lease")
		}()
	}
	for _, a := range []struct {
		name string
		run  func(t *testing.T, row *fmRow)
	}{
		{"rebind", fmRebind},
		{"session=", fmHandle},
		{"receipt", fmReceipt},
		{"write grant", fmWriteGrant},
		{"stamps", fmStampRows},
		{"subagent", fmSubagent},
		{"captures", fmCaptures},
		{"harvest", fmHarvest},
		{"resume", fmResume},
		{"/clear", fmClear},
	} {
		t.Run(a.name, func(t *testing.T) {
			r := *row
			r.r = row.r.at(t)
			r.conv = row.conv.at(t)
			a.run(t, &r)
			row.grant = r.grant
		})
	}
}
