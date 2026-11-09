package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// A daemon restart under a running agent: everything in the old process's
// memory is gone (the transport's sessions, the capabilities initialize left
// on them), and only the database is left. The client keeps presenting the
// Mcp-Session-Id it was given, which is what a `seam mcp-proxy` bridge does (the
// bridge-level test is cmd/seam's TestBridge_RestartKeepsTheApprovalDialog).

// restartRig serves a dialogRig's server behind a handler the test swaps for
// a fresh server over the same database, under a live client connection.
type restartRig struct {
	*dialogRig
	handler atomic.Pointer[http.Handler]
}

func newRestartRig(t *testing.T, o dialogOpts) *restartRig {
	t.Helper()
	return &restartRig{dialogRig: newDialogRig(t, o)}
}

// listenRestartable serves the current server through the tap, with view
// attached to every request as the transport edge does.
func (r *restartRig) listenRestartable(view func() (access.View, bool)) string {
	h := r.tap.wrap(r.srv.Handler())
	r.handler.Store(&h)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if view != nil {
			if v, ok := view(); ok {
				req = req.WithContext(access.WithView(req.Context(), v))
			}
		}
		(*r.handler.Load()).ServeHTTP(w, req)
	}))
	r.t.Cleanup(ts.Close)
	return ts.URL
}

// restart replaces the server, as the new process of a real restart does.
func (r *restartRig) restart() {
	r.srv = mcpserver.New(r.cfg)
	h := r.tap.wrap(r.srv.Handler())
	r.handler.Store(&h)
}

func (r *restartRig) agentSocket() string {
	return r.listenRestartable(r.socketView(lkBridge, lkAgent))
}

// keptSessions is the rows the daemon kept for restarts.
func (r *restartRig) keptSessions() int {
	r.t.Helper()
	var n int
	require.NoError(r.t, r.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM mcp_sessions`).Scan(&n))
	return n
}

// proved runs the bridge's half after a Touch ID dialog's ask: take the ask off
// the call's reply and prove it as the helper does, through the approve core.
func (r *restartRig) proved(key touchKey, o *owner) <-chan error {
	done := make(chan error, 1)
	go func() {
		select {
		case ask := <-o.asks:
			done <- r.proveTouchID(key, ask.RequestID, ask.Duration)
		case <-time.After(20 * time.Second):
			done <- fmt.Errorf("no approver ask")
		}
	}()
	return done
}

// A client that declared elicitation keeps the dialog across a restart under
// the live connection, and the dialog works: the form reaches the client, the
// answer reaches the waiting call, and the approver launch reaches the bridge.
func TestRestart_DialogSurvivesUnderALiveConnection(t *testing.T) {
	for name, caps := range map[string]*mcp.ElicitationCapability{
		"bare elicitation {}": nil,
		"form":                {Form: &struct{}{}},
		"form and url":        {Form: &struct{}{}, URL: &struct{}{}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRestartRig(t, dialogOpts{wait: dlgLong})
			r.seed()
			r.session("cc/fin", lkFin)
			key := r.enrollTouchID()
			o := newOwner(accepting(approval.For1h, approval.ProofTouchID))
			agent := r.dialOwner(r.agentSocket(), o, caps)
			require.Equal(t, 1, r.keptSessions(), "initialize kept what the client declared")

			r.restart()

			proof := r.proved(key, o)
			text := r.refused(agent, "tasks_list", nil)
			require.NoError(t, within(t, proof, "the proof"))
			require.Len(t, o.dialogs(), 1, "the restored form was shown")
			require.Contains(t, text, "the owner approved access request")

			leases := r.liveLeases()
			require.Len(t, leases, 1)
			first, _ := r.ok(agent, "tasks_list", nil)
			require.Contains(t, first, "reconcile the ledger")
		})
	}
}

// A connection presents its id again on every POST, and each POST is a new
// session object after a restart, so every call restores what it needs.
func TestRestart_EveryCallRestoresTheDialog(t *testing.T) {
	r := newRestartRig(t, dialogOpts{wait: dlgLong})
	r.seed()
	r.session("cc/fin", lkFin)
	r.enrollTouchID()
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	agent := r.dialOwner(r.agentSocket(), o, nil)

	r.restart()

	first := r.refused(agent, "tasks_list", nil)
	require.Contains(t, first, "the approval dialog was declined")
	second := r.refused(agent, "tasks_list", nil)
	require.Contains(t, second, "the approval dialog was declined")
	require.Len(t, o.dialogs(), 2, "the second call's own session object had to be restored too")
	require.Len(t, r.pending(), 1, "one request, still pending")
}

// The restored URL mode reaches the passkey path: the methods the form offers
// after a restart are what the client declared at initialize.
func TestRestart_URLModeIsRestored(t *testing.T) {
	r := newRestartRig(t, dialogOpts{wait: dlgLong, passkeys: true})
	r.seed()
	r.session("cc/fin", lkFin)
	r.enrollPasskey()
	r.enrollTouchID()
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	agent := r.dialOwner(r.agentSocket(), o, &mcp.ElicitationCapability{Form: &struct{}{}, URL: &struct{}{}})

	r.restart()

	r.refused(agent, "tasks_list", nil)
	shown := o.dialogs()
	require.Len(t, shown, 1)
	props := shown[0].Params.RequestedSchema.(map[string]any)["properties"].(map[string]any)
	require.Equal(t, []any{"touchid", "passkey"}, props["method"].(map[string]any)["enum"],
		"a passkey needs the url mode, which only the restored capability can say")
}

// A connection in use refreshes its row about once a day, so a long-lived agent
// is not pruned (store.MCPSessionRetention) just because it never restarted.
func TestClientCaps_CallsKeepTheConnectionFromBeingPruned(t *testing.T) {
	r := newRestartRig(t, dialogOpts{wait: dlgLong})
	r.session("cc/fin", lkFin)
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	agent := r.dialOwner(r.agentSocket(), o, nil)
	var id string
	require.NoError(t, r.db.QueryRowContext(context.Background(), `SELECT id FROM mcp_sessions`).Scan(&id))
	seenAt := func() time.Time {
		t.Helper()
		_, seen, ok, err := store.MCPSessionCapabilities(context.Background(), r.db, id)
		require.NoError(t, err)
		require.True(t, ok)
		return seen
	}
	start := r.clock()
	require.True(t, seenAt().Equal(start))

	r.set(start.Add(time.Hour))
	r.ok(agent, "project_list", nil)
	require.True(t, seenAt().Equal(start), "a call inside the day writes nothing")

	r.set(start.Add(25 * time.Hour))
	r.ok(agent, "project_list", nil)
	require.True(t, seenAt().Equal(start.Add(25*time.Hour)), "a call a day later refreshes it")
}

// Nothing is kept unless the dialog is possible: with no approval service and
// no grants a server has no initialize hook at all, and off the peer_auth
// socket or from a client that declared no elicitation nothing is written. The
// row count is the feature's whole footprint.
func TestRestart_NothingKeptWithoutTheDialog(t *testing.T) {
	t.Run("default install: no approvals, no grants", func(t *testing.T) {
		r := newRestartRig(t, dialogOpts{wait: dlgLong})
		r.cfg.Approvals, r.cfg.Grants = nil, nil
		r.srv = mcpserver.New(r.cfg)
		o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
		cli := r.dialOwner(r.agentSocket(), o, nil)
		r.ok(cli, "project_list", nil)
		require.Zero(t, r.keptSessions())
	})
	t.Run("off the socket", func(t *testing.T) {
		r := newRestartRig(t, dialogOpts{wait: dlgLong})
		o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
		cli := r.dialOwner(r.listenRestartable(func() (access.View, bool) { return r.policy.OwnerView(), true }), o, nil)
		r.ok(cli, "project_list", nil)
		require.Zero(t, r.keptSessions())
	})
	t.Run("a client that declared no elicitation", func(t *testing.T) {
		r := newRestartRig(t, dialogOpts{wait: dlgLong})
		r.seed()
		r.session("cc/fin", lkFin)
		r.enrollTouchID()
		agent := r.dialOwner(r.agentSocket(), nil, nil)
		require.Zero(t, r.keptSessions())

		r.restart()
		text := r.refused(agent, "tasks_list", nil)
		require.Zero(t, r.tap.count(), "no dialog")
		require.NotContains(t, text, "Touch ID", "this connection never offered it")
	})
}

// The one-time briefing is sent once per lease however many times the daemon
// restarts, whether the restart falls before or after it was sent.
func TestRestart_BriefingOncePerLease(t *testing.T) {
	briefingsIn := func(items []string) int {
		n := 0
		for _, it := range items {
			if strings.Contains(it, "This is its briefing, sent once:") {
				n++
			}
		}
		return n
	}
	t.Run("restart after it was sent", func(t *testing.T) {
		r := newRestartRig(t, dialogOpts{wait: dlgLong})
		r.seed()
		r.session("cc/fin", lkFin)
		agent := r.dial(r.agentSocket(), true)
		r.refused(agent, "tasks_list", nil)
		r.approve(r.pending()[0].ID, time.Hour)

		_, items := r.ok(agent, "project_list", nil)
		require.Equal(t, 1, briefingsIn(items), "the first result after the lease carries it")

		r.restart()
		_, items = r.ok(agent, "project_list", nil)
		require.Zero(t, briefingsIn(items), "sent once, across the restart")
		r.restart()
		_, items = r.ok(agent, "project_list", nil)
		require.Zero(t, briefingsIn(items))
	})
	t.Run("restart before it was sent", func(t *testing.T) {
		r := newRestartRig(t, dialogOpts{wait: dlgLong})
		r.seed()
		r.session("cc/fin", lkFin)
		agent := r.dial(r.agentSocket(), true)
		r.refused(agent, "tasks_list", nil)
		r.approve(r.pending()[0].ID, time.Hour)

		r.restart()
		_, items := r.ok(agent, "project_list", nil)
		require.Equal(t, 1, briefingsIn(items), "nothing was sent before the restart, so this is the one")
		_, items = r.ok(agent, "project_list", nil)
		require.Zero(t, briefingsIn(items))
	})
	t.Run("a refresh is briefed once again", func(t *testing.T) {
		r := newRestartRig(t, dialogOpts{wait: dlgLong})
		r.seed()
		r.session("cc/fin", lkFin)
		agent := r.dial(r.agentSocket(), true)
		r.refused(agent, "tasks_list", nil)
		r.approve(r.pending()[0].ID, time.Hour)
		_, items := r.ok(agent, "project_list", nil)
		require.Equal(t, 1, briefingsIn(items))

		// A new lease on the same project is a new lease to brief: a refresh
		// may follow a /clear, whose new session has no briefing.
		r.set(r.clock().Add(40 * time.Minute))
		r.ok(agent, "access_request", map[string]any{"project": lkFin})
		pending := r.pending()
		require.Len(t, pending, 1)
		require.Equal(t, store.AccessKindRefresh, pending[0].Kind)
		r.approve(pending[0].ID, time.Hour)

		r.restart()
		_, items = r.ok(agent, "project_list", nil)
		require.Equal(t, 1, briefingsIn(items), "the refresh's new lease")
		_, items = r.ok(agent, "project_list", nil)
		require.Zero(t, briefingsIn(items))
	})
}

// A refresh the owner approves inside the call replaces the lease the call's
// View was taken with: the result reports the new lease, and no expiry notice
// about the old one comes with it.
func TestDialog_RefreshInTheCallDropsTheOldExpiryNotice(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.seed()
	d.session("cc/fin", lkFin)
	key := d.enrollTouchID()
	base := d.agentSocket()

	// The first lease, through a connection that shows no dialog.
	d.refused(d.dial(base, true), "tasks_list", nil)
	first := d.approve(d.pending()[0].ID, time.Hour)
	d.set(first.ExpiresAt.Add(-18 * time.Minute))

	// The refresh, through one that does, approved inside the call.
	o := newOwner(accepting(approval.For1h, approval.ProofTouchID))
	agent := d.dialOwner(base, o, nil)
	proof := make(chan error, 1)
	go func() {
		select {
		case ask := <-o.asks:
			proof <- d.proveTouchID(key, ask.RequestID, ask.Duration)
		case <-time.After(20 * time.Second):
			proof <- fmt.Errorf("no approver ask")
		}
	}()

	res, err := agent.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: mcpserver.AccessRequestTool, Arguments: map[string]any{"project": lkFin},
	}})
	require.NoError(t, err)
	require.False(t, res.IsError, "the approval reads as a success: %v", res.Content)
	require.NoError(t, within(t, proof, "the proof"))

	leases := d.liveLeases()
	require.Len(t, leases, 1, "the refresh replaced the lease, it did not add one")
	require.NotEqual(t, first.ID, leases[0].ID)
	require.Greater(t, leases[0].ExpiresAt.Sub(d.clock()), 30*time.Minute, "the new lease is not expiring")

	oldEnd := access.Clock(first.ExpiresAt)
	for _, c := range res.Content {
		tc, ok := c.(mcp.TextContent)
		require.True(t, ok)
		require.NotContains(t, tc.Text, "ends at", "no expiry notice about the replaced lease")
		require.NotContains(t, tc.Text, oldEnd+" (", "not the replaced lease's end")
	}
}

// With a lease still expiring when the call ends (nothing replaced it), the
// notice is recomputed from the live row: same words as ever.
func TestLeaseNotice_RecomputedStillWarnsWhenNothingChanged(t *testing.T) {
	r := newRestartRig(t, dialogOpts{wait: dlgLong})
	r.seed()
	r.session("cc/fin", lkFin)
	agent := r.dial(r.agentSocket(), true)
	r.refused(agent, "tasks_list", nil)
	lease := r.approve(r.pending()[0].ID, time.Hour)
	r.ok(agent, "project_list", nil)

	r.set(lease.ExpiresAt.Add(-20 * time.Minute))
	_, items := r.ok(agent, "project_list", nil)
	last := items[len(items)-1]
	require.True(t, strings.HasPrefix(last, "Access to "+lkFin+" ends at "+access.Clock(lease.ExpiresAt)+" (20 minutes left)."),
		"the notice as it always read: %v", items)
}
