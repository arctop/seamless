package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// The peer_auth tier of the fenced-sessions edge (plan fenced-sessions P4):
// the same rules, with the agent's process tree as the conversation and the
// approve core's proofs; a locked target is still the lock's.

const dlgVault = "example-vault"

// fpRig is a dialog rig with read grants on, a sealed project, a confidential
// home no lock covers, content and a session to name in every target, and the
// agent's conversation set up for origin o (fsOrigin's names).
type fpRig struct {
	*dialogRig
	o        *owner
	agent    *mcpclient.Client
	home     string
	homeName string
	notes    map[string]string
	sessions map[string]string
}

func newFPRig(t *testing.T, origin string, o *owner) *fpRig {
	t.Helper()
	ctx := context.Background()
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	d.confidential(dlgHR)
	_, err := store.EnsureProject(ctx, d.db, dlgVault, dlgVault)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, d.db, dlgVault, core.IsolationSealed))
	r := &fpRig{dialogRig: d, o: o, notes: map[string]string{}, sessions: map[string]string{}}
	base := d.serve(d.lockRig.srv, nil)
	for _, p := range []string{lkFin, dlgLegal, dlgHR, dlgVault} {
		// Each project's own owner session plants its content, as its own
		// agents would.
		admin := d.dial(base, false)
		name := "sess/" + p + "-owner"
		d.ok(admin, "session_start", map[string]any{"project": p, "name": name})
		out, _ := d.ok(admin, "notes_create", map[string]any{"title": p + " notes", "body": p + " planted notes"})
		var note map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &note))
		r.notes[p] = note["id"].(string)
		d.ok(admin, "memory_write", map[string]any{"name": p + "-rule", "kind": "constraint",
			"description": p + " house rule", "body": "The " + p + " house rule.\n"})
		r.sessions[p] = name
	}
	switch origin {
	case "native confidential":
		d.session("cc/hr", dlgHR)
		r.home, r.homeName = dlgHR, "cc/hr"
	default:
		d.session("cc/ops", lkOps)
		r.home, r.homeName = lkOps, "cc/ops"
	}
	r.agent = d.dialOwner(d.agentSocket(), o, nil)
	if origin == "moved" {
		text := d.refused(r.agent, "session_start", map[string]any{"project": dlgLegal})
		r.approveMove(gShort.FindString(text), approval.For1h)
		r.home, r.homeName = dlgLegal, "cc/ops@"+dlgLegal
		items, _ := d.call(r.agent, "project_list", nil)
		require.Len(t, items, 2, "%v", items)
		require.True(t, strings.HasPrefix(items[1], "This conversation moved into "+dlgLegal+": "), items[1])
		require.Contains(t, items[1], "until the agent process exits", "under peer_auth the label is the tree's")
	}
	return r
}

// approveMove decides a move request with the daemon account's proof, as
// seamlessd approve commits it.
func (r *fpRig) approveMove(ref, duration string) approval.MoveApproved {
	r.t.Helper()
	ctx := context.Background()
	a, err := r.grants.Resolve(ctx, ref)
	require.NoError(r.t, err)
	require.Equal(r.t, store.AccessKindMove, a.Kind)
	facts, err := r.grants.Facts(ctx, a.ID)
	require.NoError(r.t, err)
	done, err := r.grants.ApproveMove(ctx, approval.Decision{ID: a.ID, Duration: duration, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofDaemon}})
	require.NoError(r.t, err)
	return done
}

// fpKind is the request the peer tier files for target from home: the lock's
// request for a locked target, else the kind rule's.
func fpKind(home, target string) string {
	switch {
	case target == lkFin:
		return store.AccessKindRequest
	case target == dlgVault, home == lkOps:
		return store.AccessKindMove
	}
	return store.AccessKindGrant
}

// TestFenced_PeerMatrix: peer_auth x {confidential, sealed, locked} x {native
// open, native confidential, moved} x {rebind, session=, refusal, explicit
// request}. A locked target is refused at the lock and asks for its lease; a
// confidential or sealed one files exactly what the home and it call for.
func TestFenced_PeerMatrix(t *testing.T) {
	for _, origin := range []string{"native open", "native confidential", "moved"} {
		for _, target := range []string{"confidential", dlgVault, lkFin} {
			for _, action := range []string{"rebind", "session=", "refusal", "explicit request"} {
				t.Run(fmt.Sprintf("%s/%s/%s", origin, target, action), func(t *testing.T) {
					r := newFPRig(t, origin, nil)
					if target == "confidential" {
						target = dlgLegal
						if r.home == dlgLegal {
							target = dlgHR
						}
					}
					before := len(r.pending())
					kind := fpKind(r.home, target)
					var text string
					switch action {
					case "rebind":
						text = r.refused(r.agent, "session_start", map[string]any{"project": target})
					case "session=":
						text = r.refused(r.agent, "memory_read", map[string]any{"name": target + "-rule", "session": r.sessions[target]})
					case "refusal":
						text = r.refused(r.agent, "notes_read", map[string]any{"id": r.notes[target]})
					case "explicit request":
						out, _ := r.ok(r.agent, mcpserver.AccessRequestTool, map[string]any{"project": target})
						var res map[string]any
						require.NoError(t, json.Unmarshal([]byte(out), &res), out)
						require.Equal(t, kind, res["kind"], out)
						require.Equal(t, "pending", res["status"])
						text = out
					}
					require.NotContains(t, text, target+" planted notes")
					require.NotContains(t, text, target+" house rule")
					if kind == store.AccessKindMove {
						require.NotContains(t, text, "Touch ID", "the Touch ID approver words no move")
					}
					pending := r.pending()
					if action == "session=" && kind != store.AccessKindRequest {
						require.Contains(t, text, "so it may not act as that session")
						require.Len(t, pending, before, "a handle files nothing")
						return
					}
					if kind == store.AccessKindRequest && action != "explicit request" {
						require.Contains(t, text, target+" is locked")
					}
					require.Len(t, pending, before+1, "%s", text)
					var got store.AccessApproval
					for _, p := range pending {
						if p.Project == target {
							got = p
						}
					}
					require.Equal(t, kind, got.Kind, text)
					require.Equal(t, lkAgent.Key(), got.ProcessKey, "anchored to the agent")
					if kind != store.AccessKindRequest {
						require.Equal(t, r.home, got.FromProject)
					}
				})
			}
		}
	}
}

// A move's dialog never offers Touch ID: with a Touch ID key enrolled and a
// client that shows forms, a refusal that files a move answers at once, and
// its remedy names only the terminal. A grant's still offers it.
func TestFenced_PeerMoveOffersNoTouchID(t *testing.T) {
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	r := newFPRig(t, "native open", o)
	r.enrollTouchID()
	text := r.refused(r.agent, "notes_read", map[string]any{"id": r.notes[dlgLegal]})
	require.Contains(t, text, "\nRemedy: move request R-")
	require.Contains(t, text, "the owner approves it in their own terminal with `seam approve R-")
	require.NotContains(t, text, "Touch ID")
	require.Empty(t, o.dialogs(), "no dialog for a move without a passkey")
}

// A move into a locked project carries the project's lease (P3): the first
// result after it announces the move, says until when the lock is open, and
// carries the briefing exactly once -- the lease's own briefing is the same
// one, and is not sent again.
func TestFenced_PeerMoveIntoALockedProject(t *testing.T) {
	r := newFPRig(t, "native open", nil)
	// The approval service reads the lock from the store, as serve's does
	// (the console's lock table here; the config's force lock in serve).
	require.NoError(t, store.AddConsoleLock(context.Background(), r.db, lkFin, "test-passkey", r.clock()))
	out, _ := r.ok(r.agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin, "write": true})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	require.Equal(t, store.AccessKindMove, res["kind"])
	done := r.approveMove(res["short_id"].(string), approval.For1h)
	require.NotNil(t, done.Lease)
	require.Equal(t, lkFin, done.Child.ProjectSlug)

	items, isErr := r.call(r.agent, "notes_read", map[string]any{"id": r.notes[lkFin]})
	require.False(t, isErr, "%v", items)
	require.Contains(t, items[0], lkFin+" planted notes", "the move and its lease open the project")
	all := strings.Join(items[1:], "\n")
	require.Equal(t, 1, strings.Count(all, "briefing, sent once"), "%v", items)
	require.Contains(t, all, "This conversation moved into "+lkFin+": ")
	require.Contains(t, all, "Its lock is open to this agent until "+access.Clock(done.Lease.ExpiresAt))
	require.Contains(t, all, lkFin+" house rule")
	require.NotContains(t, all, "Access to "+lkFin+" was approved until", "the lease's notice is the move's")
	items, _ = r.call(r.agent, "notes_read", map[string]any{"id": r.notes[lkFin]})
	require.Len(t, items, 1, "once: %v", items)
}
