package console

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
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
	row := between(t, list, `data-grant="`, `<li class="ax-open-row`, "</li>")
	require.Contains(t, row, "example-payroll")
	require.Regexp(t, `approved <time[^>]*>[^<]*</time> in the agent&#39;s approval dialog`, row)
	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, `data-result="approved"`)
	require.Contains(t, page, "Decided in the agent's approval dialog, which without peer_auth is advisory.")

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
	require.Contains(t, list, "with the bearer key", "the open list says what decided it")
	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, `data-result="approved"`)
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
	require.Contains(t, page, `data-result="denied"`)
	require.Contains(t, page, "The agent hears it was denied, and when; it can ask again with a new request. "+
		"Decided in the agent's approval dialog, which without peer_auth is advisory.")
}

// An approved grant's page says what the approval opened: the facts are the
// request as asked, whose access is the default, read-only, so a grant
// approved as read and write read "read-only" there with nothing to say
// otherwise (fenced owner check, 2026-11-15). Once the grant ends, the line
// says when.
func TestAccessConsole_ApprovedGrantSaysWhatItOpened(t *testing.T) {
	g, home := kindConsole(t, "example-hr")
	ctx := context.Background()
	a, err := g.grants.Ask(ctx, approval.AskInput{GrantInput: approval.GrantInput{Project: "example-payroll", SessionID: home}})
	require.NoError(t, err)
	f, err := g.grants.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Contains(t, f.AccessChoices, "read-write")
	before := g.html(t, "/console/access/"+a.ID)
	require.NotContains(t, before, "data-granted")
	require.NotContains(t, before, "(approved)")

	grant, err := g.grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: f.Digest,
		Access: "read-write", Proof: approval.Proof{Kind: approval.ProofKey}})
	require.NoError(t, err)
	short := approval.ShortID(a.ID)
	until := grant.ExpiresAt.Local().Format("15:04")

	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, "<dt>Opens</dt><dd>read and write <span class=\"faint\">(approved)</span></dd>")
	require.Contains(t, page, `data-result="approved"`)
	require.Contains(t, page, `<p class="ax-result-line" data-granted="write">Approved `+short+
		`: example-payroll is readable and writable from example-hr, until `+until+`.</p>`)

	var one struct {
		Request struct {
			Access string `json:"access"`
		} `json:"request"`
		Granted struct {
			Access string `json:"access"`
		} `json:"granted"`
	}
	g.getJSON(t, "/console/access/"+a.ID+"?format=json", &one)
	require.Equal(t, "read-only", one.Request.Access, "the facts stay the request as asked")
	require.Equal(t, "write", one.Granted.Access)

	require.NoError(t, store.RevokeAccessGrant(ctx, g.db, grant.ID, store.LeaseRevokedOwner, time.Now()))
	ended := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, ended, `data-granted="write">Approved `+short+` as read and write for example-hr; the grant ended at `)
}

// A grant approved as the default reads read-only, approved.
func TestAccessConsole_ApprovedReadGrantSaysReadOnly(t *testing.T) {
	g, home := kindConsole(t, "example-hr")
	ctx := context.Background()
	a, err := g.grants.Ask(ctx, approval.AskInput{GrantInput: approval.GrantInput{Project: "example-payroll", SessionID: home}})
	require.NoError(t, err)
	f, err := g.grants.Facts(ctx, a.ID)
	require.NoError(t, err)
	_, err = g.grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: f.Digest,
		Proof: approval.Proof{Kind: approval.ProofKey}})
	require.NoError(t, err)
	page := g.html(t, "/console/access/"+a.ID)
	require.Contains(t, page, "<dt>Opens</dt><dd>read-only <span class=\"faint\">(approved)</span></dd>")
	require.Contains(t, page, `data-granted="read">Approved `+approval.ShortID(a.ID)+
		`: example-payroll is readable from example-hr, read-only, until `)
}
