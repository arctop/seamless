package console

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
)

// The advisory tier's in-session dialog (plan fenced-sessions P9): a grant
// approved there, and a request decided there, say so on the Access requests
// pages, so the owner can tell the dialog's decisions from their own. Every
// other decision reads as before.
func TestAccessConsole_DialogDecisionsSaySo(t *testing.T) {
	g, home := kindConsole(t, "example-hr")
	ctx := context.Background()
	a, err := g.grants.Ask(ctx, approval.AskInput{GrantInput: approval.GrantInput{Project: "example-payroll", SessionID: home}})
	require.NoError(t, err)
	f, err := g.grants.Facts(ctx, a.ID)
	require.NoError(t, err)
	_, err = g.grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: f.Digest,
		Proof: approval.Proof{Kind: approval.ProofDialog}})
	require.NoError(t, err)

	list := g.html(t, "/console/access")
	row := between(t, list, `data-grant="`, `<li class="access-item"`, "</li>")
	require.Contains(t, row, "example-payroll")
	require.Contains(t, row, " in the agent's approval dialog, ends ")
	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, "This request is <strong>approved</strong>: decided in the agent's approval dialog, which "+
		"without peer_auth is advisory.")

	var listed struct {
		Grants []struct {
			Project string `json:"project"`
			Dialog  bool   `json:"dialog"`
		} `json:"grants"`
	}
	g.getJSON(t, "/console/access?format=json", &listed)
	require.Len(t, listed.Grants, 1)
	require.True(t, listed.Grants[0].Dialog)
}

// A grant the bearer key approved (seam approve, the console) reads exactly as
// before: no word of a dialog.
func TestAccessConsole_KeyDecisionsReadAsBefore(t *testing.T) {
	g, home := kindConsole(t, "example-hr")
	ctx := context.Background()
	a, err := g.grants.Ask(ctx, approval.AskInput{GrantInput: approval.GrantInput{Project: "example-payroll", SessionID: home}})
	require.NoError(t, err)
	f, err := g.grants.Facts(ctx, a.ID)
	require.NoError(t, err)
	_, err = g.grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: f.Digest,
		Proof: approval.Proof{Kind: approval.ProofKey}})
	require.NoError(t, err)
	list := g.html(t, "/console/access")
	require.NotContains(t, list, "approval dialog")
	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, "This request is <strong>approved</strong>.")
	require.NotContains(t, page, "approval dialog")
}

// Denied in the dialog, the request's page says it was decided there.
func TestAccessConsole_DialogDenialSaysSo(t *testing.T) {
	g, home := kindConsole(t, "example-hr")
	ctx := context.Background()
	a, err := g.grants.Ask(ctx, approval.AskInput{GrantInput: approval.GrantInput{Project: "example-payroll", SessionID: home}})
	require.NoError(t, err)
	require.NoError(t, g.grants.DenyInDialog(ctx, a.ID))
	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, "This request is <strong>denied</strong>: decided in the agent's approval dialog, which "+
		"without peer_auth is advisory.")
}
