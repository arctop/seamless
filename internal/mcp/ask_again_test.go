package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	mcpserver "github.com/arctop/seamless/internal/mcp"
)

// Asking again after a denial (owner decision 2026-11-08): the owner denies a
// wrong request, corrects the agent, and has it ask again. The agent hears
// the denial on its next refused call, which files nothing and shows no
// dialog -- no loop, nothing new for the owner -- and is told how to ask
// again; its explicit access_request files a fresh request with its reason.
func TestLockRefusal_AfterADenialNamesItAndFilesNothing(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	agent := d.dialOwner(d.agentSocket(), o, nil)

	d.refused(agent, "tasks_list", nil)
	require.Len(t, o.dialogs(), 1)
	pending := d.pending()
	require.Len(t, pending, 1)
	denied := pending[0]
	require.Empty(t, denied.Reason, "a refusal has no reason to give")
	require.NoError(t, d.svc.Deny(context.Background(), denied.ID, "owner"))

	text := d.refused(agent, "tasks_list", nil)
	short := approval.ShortID(denied.ID)
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.Contains(t, ls[0], "project "+lkFin+" is locked")
	require.True(t, strings.HasPrefix(ls[1], "Remedy: the owner denied access request "+short+" at "), ls[1])
	require.True(t, strings.HasSuffix(ls[1], ", so nothing new was filed; keep to other projects, and if the owner asks you "+
		"to request again, call access_request with a reason, or run `seam access request "+lkFin+" --reason \"...\"` "+
		"from your shell."), ls[1])
	require.Len(t, o.dialogs(), 1, "a denial offers no dialog")
	require.Empty(t, d.pending(), "nothing new for the owner")

	// The agent asks properly: a fresh request, with its reason.
	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin, "reason": "the ledger and its notes"})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.NotEqual(t, denied.ID, res["id"])
	require.Equal(t, "pending", res["status"])
	pending = d.pending()
	require.Len(t, pending, 1)
	require.Equal(t, "the ledger and its notes", pending[0].Reason)
}

// The denial the dialog itself saw: the call says who denied what, and how
// to ask again once the owner says so.
func TestDialog_DeniedSaysHowToAskAgain(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(func(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
		pending := d.pending()
		require.Len(t, pending, 1)
		require.NoError(t, d.svc.Deny(context.Background(), pending[0].ID, "owner"))
		return answerWith(mcp.ElicitationResponseActionDecline, nil)(ctx, req)
	})
	agent := d.dialOwner(d.agentSocket(), o, nil)
	text := d.refused(agent, "tasks_list", nil)
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.True(t, strings.HasPrefix(ls[1], "Remedy: the owner denied access request R-"), ls[1])
	require.True(t, strings.HasSuffix(ls[1], "; keep to other projects, and if the owner asks you to request again, call "+
		"access_request with a reason, or run `seam access request "+lkFin+" --reason \"...\"` from your shell."), ls[1])
}

// The read grant's fence names a denial the same way.
func TestGrantRefusal_AfterADenialNamesItAndFilesNothing(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	admin := d.dial(d.serve(d.lockRig.srv, nil), false)
	out, _ := d.ok(admin, "notes_create", map[string]any{"project": dlgLegal, "title": "contract notes", "body": "the vendor contract terms"})
	var note map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &note))
	d.session("cc/ops", lkOps)
	plain := d.dialOwner(d.agentSocket(), nil, nil) // no elicitation: no dialog

	first := d.refused(plain, "notes_read", map[string]any{"id": note["id"]})
	require.Contains(t, first, "\nRemedy: read request R-", first)
	pending := d.pending()
	require.Len(t, pending, 1)
	require.Equal(t, dlgLegal, pending[0].Project)
	require.NoError(t, d.grants.Deny(context.Background(), pending[0].ID, "owner"))

	text := d.refused(plain, "notes_read", map[string]any{"id": note["id"]})
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.True(t, strings.HasPrefix(ls[1], "Remedy: the owner denied read request "+approval.ShortID(pending[0].ID)+" at "), ls[1])
	require.True(t, strings.HasSuffix(ls[1], ", so nothing new was filed; keep to other projects, and if the owner asks you "+
		"to request again, call access_request with a reason, or run `seam access request "+dlgLegal+" --reason \"...\"` "+
		"from your shell."), ls[1])
	require.Empty(t, d.pending(), "nothing new for the owner")
}
