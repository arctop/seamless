package mcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	mcpserver "github.com/arctop/seamless/internal/mcp"
)

// The anchor rule at the tool surface (owner decision 2026-11-07): a call
// from a tmux pane with no agent in its process chain -- `seam call` run by
// hand -- names the tmux server as its agent process. Nothing is filed for
// it, on either path that files, and the caller reads why and what to do.

var (
	// lkPane is the seam process in the pane; lkTmux the tmux server above
	// it (its parent is pid 1, so the chain ends there).
	lkPane = access.ProcessToken{PID: 910, Version: 9100, Boot: lkBoot}
	lkTmux = access.ProcessToken{PID: 3698, Version: 36980, Boot: lkBoot}
)

const (
	tmuxRule = "Seamless files no access request for this process: it is tmux, a terminal multiplexer, not an agent, " +
		"and an approval would open the project to every program it started, other agent sessions included."
	anchorCure = "Remedy: run the work from an agent session (Claude Code or Codex), which asks for itself; for a " +
		"one-off, the owner approves that session's request with `seam approve` in their own terminal."
)

func TestAnchorRule_ATmuxPaneFilesNothing(t *testing.T) {
	r := newLockRig(t)
	svc := approval.New(approval.Config{DB: r.db, Events: r.rec, Host: lkHost, LeaseCap: 8 * time.Hour, Now: r.clock,
		Programs: func(t access.ProcessToken) string {
			if t == lkTmux {
				return "/opt/homebrew/Cellar/tmux/3.8/bin/tmux"
			}
			return ""
		}})
	r.srv = mcpserver.New(mcpserver.Config{
		DB: r.db, Files: r.mgr, Retrieve: r.ret, Events: r.rec, APIKey: testKey, LocalHost: lkHost,
		Features: config.Features{Research: true}, Now: r.clock, Approvals: svc, OfferAccessRequest: true,
	})
	cli := dialNaming(t, r.socket(lkPane, lkTmux), lkTmux)

	text := r.refused(cli, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.Equal(t, mcpserver.AccessRequestTool+": "+tmuxRule, ls[0])
	require.Equal(t, anchorCure, ls[1])

	text = r.refused(cli, "tasks_list", map[string]any{"project": lkFin})
	ls = lines(text)
	require.Len(t, ls, 2, text)
	require.Equal(t, "tasks_list: project "+lkFin+" is locked: even the owner's own agents need the owner's approval, "+
		"and "+tmuxRule, ls[0])
	require.Equal(t, anchorCure, ls[1])
	require.NotContains(t, text, "R-", "no request id: none was filed")
	require.Empty(t, r.pending(), "nothing was filed, on either path")
}

// dialNaming connects to base with the agent-process header naming proc.
func dialNaming(t *testing.T, base string, proc access.ProcessToken) *mcpclient.Client {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + testKey, mcpserver.HostHeader: lkHost,
		mcpserver.AgentProcessHeader: proc.AgentID()}
	cli, err := mcpclient.NewStreamableHttpClient(base, transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(context.Background()))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "pane", Version: "0"}
	_, err = cli.Initialize(context.Background(), initReq)
	require.NoError(t, err)
	return cli
}

// Every remedy offers only the approval paths the daemon has: no passkey page
// until a passkey is enrolled, and Touch ID only once a key is enrolled and
// only to a client that can show the approval dialog that carries it.
func TestRemedies_OfferOnlyThePathsThatExist(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: 400 * time.Millisecond, passkeys: true})
	d.session("cc/fin", lkFin)
	plain := d.dialOwner(d.agentSocket(), nil, nil) // no elicitation: no dialog, ever
	decliner := d.dialOwner(d.agentSocket(), newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil)), nil)

	remedy := func(cli *mcpclient.Client) string {
		t.Helper()
		ls := lines(d.refused(cli, "tasks_list", nil))
		require.Len(t, ls, 2, ls)
		return ls[1]
	}
	listed := func(cli *mcpclient.Client) string {
		t.Helper()
		out, _ := d.ok(cli, "project_list", nil)
		row := projectRow(t, out, lkFin)
		require.NotNil(t, row)
		return row["remedy"].(string)
	}

	// Passkeys are set up, but none is enrolled: no page is offered.
	got := remedy(plain)
	require.Contains(t, got, "-- the owner approves it in their own terminal with `seam approve R-")
	require.NotContains(t, got, "passkey")
	require.NotContains(t, got, "Touch ID")
	require.True(t, strings.HasSuffix(listed(plain), "explicitly; the owner approves it in their own terminal with `seam approve` "+
		"and the request's id."))

	// A Touch ID key: offered to the client that shows dialogs, never to the
	// one that cannot.
	d.enrollTouchID()
	require.NotContains(t, remedy(plain), "Touch ID")
	require.NotContains(t, listed(plain), "Touch ID")
	got = remedy(decliner)
	require.Contains(t, got, "-- the owner approves it with Touch ID in the approval dialog (where this agent's client "+
		"shows one), or in their own terminal with `seam approve R-")
	require.Contains(t, got, "The approval dialog was declined; access request R-")
	require.True(t, strings.HasSuffix(got, "in their own terminal, or Touch ID in the next approval dialog."), got)
	require.Contains(t, listed(decliner), "explicitly; the owner approves it with Touch ID in the approval dialog")

	// A passkey: its page, on every surface.
	d.enrollPasskey()
	pending := d.pending()
	require.Len(t, pending, 1)
	page := dlgOrigin + "/console/access/" + pending[0].ID
	require.True(t, strings.HasSuffix(remedy(plain), "or with a passkey at "+page+"; retry once it is approved, and keep to other "+
		"projects until then."))
	require.True(t, strings.HasSuffix(listed(plain), "or with a passkey on the request's console page."))
}
