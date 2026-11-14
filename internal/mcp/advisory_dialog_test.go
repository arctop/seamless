package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// The advisory tier's in-session dialog (plan fenced-sessions P9, owner
// decision D7): with read grants on and peer_auth off, a call that files a
// move or grant request -- a refusal at a fenced project, session_start into
// one, an explicit access_request -- shows the owner the request in the
// agent's client, and Approve or Deny there decides it, recorded as the
// dialog's. Declining, cancelling and the dialog's bound decide nothing.

// advRig is an advisory fenced-sessions rig (fsRig) whose second server, over
// the same database, offers the dialog: access_request registered as serve
// registers it in this tier, dialogs bounded at wait and polled every few
// milliseconds.
type advRig struct {
	*fsRig
	url string
}

func newAdvRig(t *testing.T, o fsOrigin, wait time.Duration) *advRig {
	t.Helper()
	r := newFSRig(t, o)
	r.cfg.DialogWait, r.cfg.DialogPoll, r.cfg.OfferAccessRequest = wait, 5*time.Millisecond, true
	return &advRig{fsRig: r, url: r.restart(t)}
}

// advDial dials base as the conversation's agent -- its process identity on
// every request -- with o answering its dialogs (nil: a client that declares
// no elicitation) and caps overlaid on what the handler declares (nil: the
// bare `elicitation: {}`, form mode).
func advDial(t *testing.T, base string, o *owner, caps *mcp.ElicitationCapability) *mcpclient.Client {
	t.Helper()
	return advDialAs(t, base, fsAgent, o, caps)
}

// advDialAs is advDial naming process proc ("" names none).
func advDialAs(t *testing.T, base, proc string, o *owner, caps *mcp.ElicitationCapability) *mcpclient.Client {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + testKey}
	if proc != "" {
		headers[mcpserver.AgentProcessHeader] = proc
	}
	trans, err := transport.NewStreamableHTTP(base, transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	var opts []mcpclient.ClientOption
	if o != nil {
		opts = append(opts, mcpclient.WithElicitationHandler(o))
	}
	cli := mcpclient.NewClient(trans, opts...)
	t.Cleanup(func() { _ = cli.Close() })
	ctx := context.Background()
	require.NoError(t, cli.Start(ctx))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "advisory-dialog", Version: "0"}
	if caps != nil {
		initReq.Params.Capabilities.Elicitation = caps
	}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	return cli
}

// advAnswer is an owner who accepts the form with decision, and with access
// and duration when they are not "".
func advAnswer(decision, accessChoice, duration string) func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	content := map[string]any{"decision": decision}
	if accessChoice != "" {
		content["access"] = accessChoice
	}
	if duration != "" {
		content["duration"] = duration
	}
	return answerWith(mcp.ElicitationResponseActionAccept, content)
}

// advNever is an owner who never answers: the dialog waits out its bound.
func advNever(ctx context.Context, _ mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *advRig) advRow(id string) store.AccessApproval {
	r.t.Helper()
	a, err := store.AccessApprovalByID(context.Background(), r.db, id)
	require.NoError(r.t, err)
	return a
}

// advRequest is the one request for target, filed since the rig was set up.
func (r *advRig) advRequest(target string) store.AccessApproval {
	r.t.Helper()
	var id string
	require.NoError(r.t, r.db.QueryRowContext(context.Background(),
		`SELECT id FROM access_approvals WHERE project_slug = ? ORDER BY requested_at DESC, id DESC LIMIT 1`, target).Scan(&id))
	return r.advRow(id)
}

// advSchema is the form's properties, as the client received them.
func advSchema(t *testing.T, req mcp.ElicitationRequest) map[string]any {
	t.Helper()
	schema, ok := req.Params.RequestedSchema.(map[string]any)
	require.True(t, ok, "%T", req.Params.RequestedSchema)
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	return props
}

// A refusal that files a move: the dialog shows the move, asks only for the
// decision (a move has no access choice and, nothing being locked, no
// duration), and its answer decides -- Approve moves the conversation, Deny
// denies it, and the bound, declining or cancelling leave it pending.
func TestAdvisoryDialog_RefusalFiledMove(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[0], dlgLong)
		o := newOwner(advAnswer("approve", "", ""))
		cli := advDial(t, r.url, o, nil)

		text := r.refused(cli, "notes_read", map[string]any{"id": r.finNote})
		a := r.advRequest(gFin)
		short := approval.ShortID(a.ID)
		require.Equal(t, store.AccessKindMove, a.Kind)
		require.Equal(t, "notes_read: the owner approved move request "+short+": this conversation works in project "+gFin+
			" from now on.\nRemedy: this call ran before the approval and did nothing -- retry it now.", text)
		require.NotContains(t, text, "quarterly ledger", "the call that asked read nothing")
		require.Equal(t, store.ApprovalApproved, a.Status)
		require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
		require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM access_moves WHERE approval_id = ? AND decided_by = ?`, a.ID,
			approval.DecidedByDialog), "the move is the dialog's")
		require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.approved' AND item_id = ?
			AND json_extract(payload, '$.proof') = 'dialog'`, a.ID))

		shown := o.dialogs()
		require.Len(t, shown, 1)
		msg := shown[0].Params.Message
		lead, _, _ := strings.Cut(msg, "\n\n")
		require.Equal(t, "1 warning. Session cc/fs-agent asks to move into "+gFin+".\n"+
			"For good: a move does not end. Approve below decides it.", lead)
		require.Contains(t, msg, "\nWarnings:\n- This agent has worked in "+gOps+". If you approve, it works in "+gFin+
			" from now on, and what it saw may be written into "+gFin+". It can no longer write to "+gOps+
			" or the global scope until the conversation ends.\n")
		for _, want := range []string{
			"Project: " + gFin + " (confidential), moving from project " + gOps + "\n",
			"Holds once approved: " + gFin + "\n",
			"May write to once approved: " + gFin + "\n",
			"Session: cc/fs-agent\n",
			"Asking: a session (without peer_auth the daemon knows no process)\n",
			"Working directory: /work/" + gOps + "\n",
			approval.SelfReportedNote + "\n",
			"Request: " + short + ", a move request\n",
			"This dialog stops waiting at: ",
			"The request expires at: ",
		} {
			require.Contains(t, msg, want)
		}
		require.Less(t, strings.Index(msg, "Warnings:"), strings.Index(msg, "Project:"), "warnings come first")
		require.True(t, strings.HasSuffix(msg, "Choosing Approve decides the request now, with no other proof: without "+
			"peer_auth this approval is advisory, since Seamless cannot tell who answers this dialog, and any process "+
			"holding the bearer key could make the same decision. Choosing Deny denies it. Declining or cancelling decides "+
			"nothing: the request stays pending for `seam approve "+short+"` in your own terminal, or the console's Access "+
			"requests page."), msg)
		require.NotContains(t, msg, "Touch ID")
		require.NotContains(t, msg, "grants nothing by itself", "the peer_auth tier's words are not this tier's")
		props := advSchema(t, shown[0])
		require.Len(t, props, 1, "a move asks for the decision alone: %v", props)
		decision := props["decision"].(map[string]any)
		require.Equal(t, []any{"approve", "deny"}, decision["enum"])
		require.Equal(t, []any{"Approve", "Deny"}, decision["enumNames"])
		require.NotContains(t, decision, "default", "nothing chosen decides nothing")
		require.Contains(t, decision["description"], "advisory")
		schema := shown[0].Params.RequestedSchema.(map[string]any)
		require.Equal(t, []any{"decision"}, schema["required"])

		// The retry works in the new home, and its first result announces the
		// move.
		items, isErr := r.call(cli, "notes_read", map[string]any{"id": r.finNote})
		require.False(t, isErr, "%v", items)
		require.Contains(t, items[0], "quarterly ledger reconciliation notes")
		require.Len(t, items, 2, "%v", items)
		require.True(t, strings.HasPrefix(items[1], "This conversation moved into "+gFin+": the owner approved move request "+short), items[1])
	})

	t.Run("deny", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[0], dlgLong)
		o := newOwner(advAnswer("deny", "", ""))
		cli := advDial(t, r.url, o, nil)

		text := r.refused(cli, "notes_read", map[string]any{"id": r.finNote})
		a := r.advRequest(gFin)
		short := approval.ShortID(a.ID)
		require.Equal(t, store.ApprovalDenied, a.Status)
		require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
		rule, remedy, _ := strings.Cut(text, "\nRemedy: ")
		require.True(t, strings.HasPrefix(rule, "notes_read: project "+gFin+" is confidential: "), rule)
		require.Equal(t, "the owner denied move request "+short+"; keep to other projects, and if the owner asks you to "+
			"request again, call access_request with a reason, or run `seam access request "+gFin+" --reason \"...\"` with "+
			"your own shell tool.", remedy)
		require.Zero(t, r.count(`SELECT COUNT(*) FROM access_moves`))

		// The next refusal names the denial and files nothing; no dialog is
		// shown for a request that was not filed.
		again := r.refused(cli, "notes_read", map[string]any{"id": r.finNote})
		require.Contains(t, again, "the owner denied move request "+short+" at ")
		require.Contains(t, again, "so nothing new was filed")
		require.Len(t, o.dialogs(), 1)
		require.Empty(t, r.pending())
	})

	for name, answer := range map[string]func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error){
		"timeout": advNever,
		"decline": answerWith(mcp.ElicitationResponseActionDecline, nil),
		"cancel":  answerWith(mcp.ElicitationResponseActionCancel, nil),
		// Accepted with no decision chosen: nothing is decided.
		"no decision": answerWith(mcp.ElicitationResponseActionAccept, map[string]any{}),
	} {
		t.Run(name, func(t *testing.T) {
			wait := dlgLong
			if name == "timeout" {
				wait = 400 * time.Millisecond
			}
			r := newAdvRig(t, fsOrigins[0], wait)
			o := newOwner(answer)
			cli := advDial(t, r.url, o, nil)

			text := r.refused(cli, "notes_read", map[string]any{"id": r.finNote})
			require.Len(t, o.dialogs(), 1)
			a := r.advRequest(gFin)
			short := approval.ShortID(a.ID)
			require.Equal(t, store.ApprovalPending, a.Status, "%s decides nothing: the request waits for the owner", name)
			require.Empty(t, a.DecidedBy)
			ls := lines(text)
			require.Len(t, ls, 2, text)
			ended := map[string]string{
				"timeout":     "the approval dialog stopped waiting at ",
				"decline":     "the approval dialog was declined",
				"cancel":      "the approval dialog was cancelled",
				"no decision": "the approval dialog's answer was not one it offered",
			}[name]
			require.True(t, strings.HasPrefix(ls[1], "Remedy: "+ended), ls[1])
			require.Contains(t, ls[1], "; move request "+short+" stays pending for the owner: `seam approve "+short+
				"` in their own terminal, Approve on the console's Access requests page, or Approve in the next approval "+
				"dialog; retry once it is approved. Record what belongs in "+gOps+" now: ")
			require.NotContains(t, ls[1], " is waiting -- ", "one remedy, not the original with a second after it")

			// Decidable elsewhere: seam approve's bearer-key decision still
			// approves it.
			done := r.approveMove(short)
			require.Equal(t, gFin, done.Child.ProjectSlug)
			require.Equal(t, "bearer-key", r.advRow(a.ID).DecidedBy)
		})
	}
}

// A refused write from a confidential home files a read-and-write grant
// request: the dialog shows the carry and what each access lets happen, and
// asks for the access (read-only by default) and how long. Approve with read
// and write opens the project to writes; Approve with nothing chosen is
// read-only; Deny and the bound decide as for a move.
func TestAdvisoryDialog_RefusalFiledGrantReadAndWrite(t *testing.T) {
	ask := map[string]any{"project": gFin, "title": "reconcile", "body": "the visiting conversation's task body"}
	for _, c := range []struct {
		name               string
		answer             func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error)
		wait               time.Duration
		status, access     string
		expires            time.Duration
		writesAfterwards   bool
		pendingRemedyStart string
	}{
		{name: "approve read and write", answer: advAnswer("approve", "read-write", approval.For1h), wait: dlgLong,
			status: store.ApprovalApproved, access: store.GrantAccessWrite, expires: time.Hour, writesAfterwards: true},
		{name: "approve with the defaults", answer: advAnswer("approve", "", ""), wait: dlgLong,
			status: store.ApprovalApproved, access: store.GrantAccessRead, expires: 8 * time.Hour},
		{name: "deny", answer: advAnswer("deny", "read-write", approval.For1h), wait: dlgLong, status: store.ApprovalDenied},
		{name: "timeout", answer: advNever, wait: 400 * time.Millisecond, status: store.ApprovalPending,
			pendingRemedyStart: "Remedy: the approval dialog stopped waiting at "},
		{name: "an access it did not offer", answer: advAnswer("approve", "admin", approval.For1h), wait: dlgLong,
			status: store.ApprovalPending, pendingRemedyStart: "Remedy: the approval dialog's answer was not one it offered"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newAdvRig(t, fsOrigins[1], c.wait)
			o := newOwner(c.answer)
			cli := advDial(t, r.url, o, nil)

			text := r.refused(cli, "tasks_add", ask)
			a := r.advRequest(gFin)
			short := approval.ShortID(a.ID)
			require.Equal(t, store.AccessKindGrant, a.Kind)
			require.Equal(t, store.GrantAccessWrite, a.RequestedAccess)
			require.Equal(t, gLegal, a.FromProject)
			require.Equal(t, c.status, a.Status)

			shown := o.dialogs()
			require.Len(t, shown, 1)
			msg := shown[0].Params.Message
			lead, _, _ := strings.Cut(msg, "\n\n")
			require.Equal(t, "1 warning. Session cc/fs-agent asks for read-write access to "+gFin+" from project "+gLegal+".\n"+
				"Until its session ends (8 hours at most), unless you pick less below. Approve below decides it.", lead)
			for _, want := range []string{
				"- content from " + gFin + " may be copied into " + gLegal + "\n",
				"Project: " + gFin + " (confidential), read from project " + gLegal + "\n",
				"Access: read-only unless you choose read and write below\n",
				"Asked for: read and write\n",
				"Holds once approved: " + gFin + " and " + gLegal + "\n",
				"May write to once approved: " + gLegal + " (approved read-only)\n",
				"Read-only lets: content from " + gFin + " may be copied into " + gLegal + "\n",
				"Read and write lets: content from " + gFin + " may be copied into " + gLegal + ", and it may change " + gFin +
					"'s tasks, memories and notes; content from " + gLegal + " may be copied into " + gFin + "\n",
				"Request: " + short + ", a read-and-write request\n",
			} {
				require.Contains(t, msg, want)
			}
			props := advSchema(t, shown[0])
			require.Len(t, props, 3, "%v", props)
			accessField := props["access"].(map[string]any)
			require.Equal(t, []any{"read-only", "read-write"}, accessField["enum"])
			require.Equal(t, []any{"Read-only", "Read and write"}, accessField["enumNames"])
			require.Equal(t, "read-only", accessField["default"], "read-only by default")
			durationField := props["duration"].(map[string]any)
			require.Equal(t, []any{"session", "1h", "4h"}, durationField["enum"])
			require.Equal(t, "session", durationField["default"])

			switch c.status {
			case store.ApprovalApproved:
				require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
				var g store.AccessGrant
				require.NoError(t, r.db.QueryRowContext(context.Background(),
					`SELECT access, decided_by, expires_at FROM access_grants WHERE approval_id = ?`, a.ID).Scan(&g.Access, &g.DecidedBy, new(string)))
				require.Equal(t, c.access, g.Access)
				require.Equal(t, approval.DecidedByDialog, g.DecidedBy)
				live, err := r.grants.Live(context.Background())
				require.NoError(t, err)
				require.Len(t, live, 1)
				require.Equal(t, r.clock().Add(c.expires), live[0].ExpiresAt, "the duration chosen in the dialog")
				how := "readable from project " + gLegal
				if c.access == store.GrantAccessWrite {
					how = "readable and writable from project " + gLegal
				}
				if c.access == store.GrantAccessWrite {
					require.Equal(t, "tasks_add: the owner approved read-and-write request "+short+": project "+gFin+" is "+how+
						", nothing else until "+access.Clock(live[0].ExpiresAt)+".\nRemedy: this call ran before the approval and did "+
						"nothing -- retry it now.", text)
				} else {
					// Approved read-only, the write stays refused: no "retry it now",
					// which would only file another request (P13, 2026-11-14).
					require.Equal(t, "tasks_add: the owner approved read-and-write request "+short+" as read-only: project "+gFin+
						" is "+how+", nothing else until "+access.Clock(live[0].ExpiresAt)+".\nRemedy: a read grant clears no "+
						"write, so this write stays refused. Write in "+gLegal+" instead; if the task needs to change "+gFin+
						", ask the owner first whether to make it again: that files a new request, which they approve as read "+
						"and write by choosing read and write in its dialog, or with `seam approve <its id> --access read-write`.", text)
				}
				items, isErr := r.call(cli, "tasks_add", ask)
				require.Equal(t, !c.writesAfterwards, isErr, "%v", items)
			case store.ApprovalDenied:
				require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
				require.Contains(t, text, "\nRemedy: the owner denied read-and-write request "+short+"; keep to other projects")
				require.Zero(t, r.count(`SELECT COUNT(*) FROM access_grants`))
			default:
				require.Empty(t, a.DecidedBy)
				ls := lines(text)
				require.Len(t, ls, 2, text)
				require.True(t, strings.HasPrefix(ls[1], c.pendingRemedyStart), ls[1])
				require.Contains(t, ls[1], "; read-and-write request "+short+" stays pending for the owner: `seam approve "+
					short+"` in their own terminal, Approve on the console's Access requests page, or Approve in the next "+
					"approval dialog; retry once it is approved. Approved read and write, this session changes "+gFin)
				require.Zero(t, r.count(`SELECT COUNT(*) FROM access_grants`))
			}
		})
	}
}

// The explicit request: access_request, registered in this tier, files what
// the conversation's home and the project call for and answers with the
// dialog's decision -- approved, denied, or still pending when the dialog
// stops waiting -- and with the agent's reason shown to the owner as its own
// words.
func TestAdvisoryDialog_ExplicitRequest(t *testing.T) {
	type result struct {
		Kind, Status, Dialog, Next, OpenUntil, Access, Granted, ShortID, Page, Reason string
		Filed                                                                         bool
	}
	decode := func(t *testing.T, out map[string]any) result {
		t.Helper()
		b, err := json.Marshal(out)
		require.NoError(t, err)
		var res struct {
			Kind      string `json:"kind"`
			Status    string `json:"status"`
			Dialog    string `json:"dialog"`
			Next      string `json:"next"`
			OpenUntil string `json:"open_until"`
			Access    string `json:"access"`
			Granted   string `json:"granted"`
			ShortID   string `json:"short_id"`
			Page      string `json:"page"`
			Reason    string `json:"reason"`
			Filed     bool   `json:"filed"`
		}
		require.NoError(t, json.Unmarshal(b, &res))
		return result{res.Kind, res.Status, res.Dialog, res.Next, res.OpenUntil, res.Access, res.Granted, res.ShortID, res.Page,
			res.Reason, res.Filed}
	}

	t.Run("a move, approved", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[0], dlgLong)
		o := newOwner(advAnswer("approve", "", ""))
		cli := advDial(t, r.url, o, nil)
		res := decode(t, r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": gFin, "reason": "reconcile the quarter"}))
		require.Equal(t, store.AccessKindMove, res.Kind)
		require.Equal(t, store.ApprovalApproved, res.Status)
		require.Equal(t, "accepted", res.Dialog)
		require.True(t, res.Filed)
		require.Equal(t, approval.ReasonAdded, res.Reason)
		require.Empty(t, res.Page)
		require.Equal(t, "the owner approved it: this conversation works in project "+gFin+" from now on; retry the work "+
			"that needs it", res.Next)
		a := r.advRow(r.advRequest(gFin).ID)
		require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
		msg := o.dialogs()[0].Params.Message
		require.Contains(t, msg, "\nThe agent's reason, in its own words (Seamless did not check it):\n\"reconcile the quarter\"\n")
		require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE payload LIKE '%reconcile the quarter%'`),
			"the reason reaches no event")
	})

	t.Run("read and write between confidential projects, approved", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[1], dlgLong)
		o := newOwner(advAnswer("approve", "read-write", approval.For4h))
		cli := advDial(t, r.url, o, nil)
		res := decode(t, r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": gFin, "write": true}))
		require.Equal(t, store.AccessKindGrant, res.Kind)
		require.Equal(t, store.GrantAccessWrite, res.Access)
		require.Equal(t, store.GrantAccessWrite, res.Granted)
		require.Equal(t, store.ApprovalApproved, res.Status)
		require.Equal(t, r.clock().Add(4*time.Hour).UTC().Format(time.RFC3339), res.OpenUntil)
		require.True(t, strings.HasPrefix(res.Next, "the owner approved it: project "+gFin+" is readable and writable from project "+
			gLegal+", nothing else until "), res.Next)
		got := r.ok(cli, "tasks_add", map[string]any{"project": gFin, "title": "reconcile", "body": "written under the grant"})
		require.Equal(t, gFin, got["project"])
	})

	// Asked for read and write, approved with the defaults (read-only): the
	// answer says what was granted and never "retry the work" (P13,
	// 2026-11-14: "access":"write" beside a read-only approval misled).
	t.Run("read and write asked, approved read-only", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[1], dlgLong)
		o := newOwner(advAnswer("approve", "", ""))
		cli := advDial(t, r.url, o, nil)
		res := decode(t, r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": gFin, "write": true}))
		require.Equal(t, store.GrantAccessWrite, res.Access, "what was asked")
		require.Equal(t, store.GrantAccessRead, res.Granted, "what was granted")
		require.Equal(t, store.ApprovalApproved, res.Status)
		require.True(t, strings.HasPrefix(res.Next, "the owner approved it as read-only: project "+gFin+" is readable from project "+
			gLegal+", nothing else until "), res.Next)
		require.True(t, strings.HasSuffix(res.Next, "; a read grant clears no write, so write in "+gLegal+
			", and ask the owner before asking for read and write again"), res.Next)
		require.NotContains(t, res.Next, "retry")
	})

	t.Run("denied", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[1], dlgLong)
		o := newOwner(advAnswer("deny", "", ""))
		cli := advDial(t, r.url, o, nil)
		res := decode(t, r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": gFin}))
		require.Equal(t, store.AccessKindGrant, res.Kind)
		require.Equal(t, store.ApprovalDenied, res.Status)
		require.Equal(t, "the owner denied it: keep to other projects, and if the owner asks you to request again, call "+
			"access_request with a reason, or run `seam access request "+gFin+" --reason \"...\"` with your own shell tool", res.Next)
		require.Equal(t, approval.DecidedByDialog, r.advRequest(gFin).DecidedBy)
	})

	t.Run("the dialog stops waiting", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[0], 400*time.Millisecond)
		o := newOwner(advNever)
		cli := advDial(t, r.url, o, nil)
		res := decode(t, r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": gVault}))
		require.Equal(t, store.AccessKindMove, res.Kind, "a sealed project is reached only by moving in")
		require.Equal(t, store.ApprovalPending, res.Status)
		require.Equal(t, "unanswered", res.Dialog)
		require.Equal(t, "Record what belongs in "+gOps+" now: once the owner approves the move, this conversation can no "+
			"longer write there. The owner approves it in their own terminal with `seam approve "+res.ShortID+"`, or with "+
			"Approve on the console's Access requests page; keep to other projects until it is approved, then retry the work "+
			"that needs "+gVault, res.Next)
		require.Equal(t, store.ApprovalPending, r.advRequest(gVault).Status)
	})

	t.Run("a client that shows no form answers at once", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[1], dlgLong)
		for _, caps := range []*mcp.ElicitationCapability{nil, {URL: &struct{}{}}} {
			var o *owner
			if caps != nil {
				o = newOwner(advAnswer("approve", "", ""))
			}
			cli := advDial(t, r.url, o, caps)
			res := decode(t, r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": gFin}))
			require.Equal(t, store.ApprovalPending, res.Status)
			require.Empty(t, res.Dialog, "no dialog was shown")
			require.True(t, strings.HasPrefix(res.Next, "the owner approves it in their own terminal with `seam approve "+
				res.ShortID+"`, or with Approve on the console's Access requests page; "), res.Next)
			if o != nil {
				require.Empty(t, o.dialogs(), "the advisory dialog is a form: a URL-only client gets none")
			}
		}
	})

	t.Run("the home itself", func(t *testing.T) {
		r := newAdvRig(t, fsOrigins[1], dlgLong)
		cli := advDial(t, r.url, nil, nil)
		for _, write := range []bool{false, true} {
			text := r.refused(cli, mcpserver.AccessRequestTool, map[string]any{"project": gLegal, "write": write})
			require.Equal(t, "access_request: this conversation already works in "+gLegal+": nothing to request", text)
		}
		text := r.refused(cli, mcpserver.AccessRequestTool, map[string]any{"project": gHR})
		require.Equal(t, "access_request: that project is neither confidential nor sealed: nothing to request", text)
		require.Empty(t, r.pending())
	})
}

// A client that names no agent process is held by the session it is bound
// to (the documented limit): its refused read files the read grant it always
// did, and the dialog decides that too -- no carry to show, no access choice,
// only how long.
func TestAdvisoryDialog_ASessionWithoutIdentity(t *testing.T) {
	r := newAdvRig(t, fsOrigins[0], dlgLong)
	o := newOwner(advAnswer("approve", "", approval.For1h))
	plain := advDialAs(t, r.url, "", o, nil)
	start := r.ok(plain, "session_start", map[string]any{"project": gHR, "name": "sess/plain"})
	require.Equal(t, gHR, start["project"])

	text := r.refused(plain, "notes_read", map[string]any{"id": r.finNote})
	a := r.advRequest(gFin)
	short := approval.ShortID(a.ID)
	require.Equal(t, store.AccessKindGrant, a.Kind)
	require.Equal(t, gHR, a.FromProject)
	require.True(t, a.Carry.IsZero(), "an open project's read grant states no carry, as before")
	require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
	live, err := r.grants.Live(context.Background())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, store.GrantAccessRead, live[0].Access)
	require.Equal(t, r.clock().Add(time.Hour), live[0].ExpiresAt)
	require.Equal(t, "notes_read: the owner approved read request "+short+": project "+gFin+" is readable from project "+gHR+
		", nothing else until "+access.Clock(live[0].ExpiresAt)+".\nRemedy: this call ran before the approval and did "+
		"nothing -- retry it now.", text)

	shown := o.dialogs()
	require.Len(t, shown, 1)
	props := advSchema(t, shown[0])
	require.Contains(t, props, "decision")
	require.Contains(t, props, "duration")
	require.NotContains(t, props, "access", "a read grant from an open project offers no access choice")
	msg := shown[0].Params.Message
	require.Contains(t, msg, "- content from "+gFin+" may be copied into "+gHR+"'s memories\n")
	require.NotContains(t, msg, "Holds once approved", "no carry stated, none shown")

	items, isErr := r.call(plain, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "%v", items)
	require.Contains(t, items[0], "quarterly ledger reconciliation notes")
}

// D4's session_start into a fenced project files the request, and the dialog
// answers it like any refusal's: approved, the conversation works in the
// project from now on.
func TestAdvisoryDialog_SessionStartFiledMove(t *testing.T) {
	r := newAdvRig(t, fsOrigins[0], dlgLong)
	o := newOwner(advAnswer("approve", "", ""))
	cli := advDial(t, r.url, o, nil)
	text := r.refused(cli, "session_start", map[string]any{"project": gVault})
	a := r.advRequest(gVault)
	require.Equal(t, store.AccessKindMove, a.Kind)
	require.Equal(t, store.ApprovalApproved, a.Status)
	require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
	require.Equal(t, "session_start: the owner approved move request "+approval.ShortID(a.ID)+": this conversation works in "+
		"project "+gVault+" from now on.\nRemedy: this call ran before the approval and did nothing -- retry it now.", text)
	require.Contains(t, o.dialogs()[0].Params.Message, gVault+" is sealed, so nothing enters it otherwise: approving lets in "+
		"what this agent carries from "+gOps+".", "the carry-in a seal otherwise forbids")
	start := r.ok(cli, "session_start", map[string]any{"project": gVault})
	require.Equal(t, gVault, start["project"], "retried, it binds in its new home")
}

// Every origin, target and way of asking: the dialog's Approve decides the
// request the kind rule filed, as the dialog's.
func TestFenced_AdvisoryDialogMatrix(t *testing.T) {
	for _, o := range fsOrigins {
		for _, target := range []string{o.conf, gVault} {
			for _, action := range []string{"rebind", "refusal", "explicit request"} {
				t.Run(fmt.Sprintf("%s/%s/%s", o.name, target, action), func(t *testing.T) {
					r := newAdvRig(t, o, dlgLong)
					owner := newOwner(advAnswer("approve", "", ""))
					cli := advDial(t, r.url, owner, nil)
					kind := kindOf(r.home, target)
					switch action {
					case "rebind":
						text := r.refused(cli, "session_start", map[string]any{"project": target})
						require.True(t, strings.HasPrefix(text, "session_start: the owner approved "), text)
					case "refusal":
						text := r.refused(cli, "notes_read", map[string]any{"id": r.notes[target]})
						require.True(t, strings.HasPrefix(text, "notes_read: the owner approved "), text)
						require.NotContains(t, text, target+" planted notes", "the call that asked read nothing")
					case "explicit request":
						out := r.ok(cli, mcpserver.AccessRequestTool, map[string]any{"project": target})
						require.Equal(t, kind, out["kind"])
						require.Equal(t, store.ApprovalApproved, out["status"])
					}
					a := r.advRequest(target)
					require.Equal(t, kind, a.Kind)
					require.Equal(t, r.home, a.FromProject)
					require.Equal(t, store.ApprovalApproved, a.Status)
					require.Equal(t, approval.DecidedByDialog, a.DecidedBy)
					require.Len(t, owner.dialogs(), 1)
					items, isErr := r.call(cli, "notes_read", map[string]any{"id": r.notes[target]})
					require.False(t, isErr, "approved, the read goes through: %v", items)
					body := target + " planted notes"
					if target == gFin {
						body = "quarterly ledger reconciliation notes"
					}
					require.Contains(t, items[0], body)
				})
			}
		}
	}
}

// Approve is over the facts the dialog showed: a request that changed while
// it was open -- here the same conversation asked for another project, a new
// warning -- is not approved, and stays pending.
func TestAdvisoryDialog_FactsChangedApprovesNothing(t *testing.T) {
	r := newAdvRig(t, fsOrigins[0], dlgLong)
	o := newOwner(func(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
		if _, err := r.grants.Ask(ctx, approval.AskInput{GrantInput: approval.GrantInput{Project: gVault, SessionID: r.ambient}}); err != nil {
			return nil, err
		}
		return advAnswer("approve", "", "")(ctx, req)
	})
	cli := advDial(t, r.url, o, nil)
	text := r.refused(cli, "notes_read", map[string]any{"id": r.finNote})
	a := r.advRequest(gFin)
	short := approval.ShortID(a.ID)
	require.Equal(t, store.ApprovalPending, a.Status)
	require.Empty(t, a.DecidedBy)
	require.Contains(t, text, "\nRemedy: the request changed while the approval dialog showed it (a new warning, or the "+
		"agent's reason filled in), so nothing was approved; move request "+short+" stays pending for the owner: ")
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_moves`))
}

// After a daemon restart under a live connection the advisory dialog is still
// there: the capabilities the client declared at initialize are kept over TCP
// in this tier (mcp_sessions), as on the peer_auth socket. With read grants
// off nothing is kept.
func TestAdvisoryDialog_SurvivesARestart(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	r.cfg.DialogWait, r.cfg.DialogPoll, r.cfg.OfferAccessRequest = dlgLong, 5*time.Millisecond, true
	var handler atomic.Pointer[http.Handler]
	serve := func() {
		h := mcpserver.New(r.cfg).Handler()
		handler.Store(&h)
	}
	serve()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		(*handler.Load()).ServeHTTP(w, req)
	}))
	t.Cleanup(ts.Close)
	o := newOwner(advAnswer("approve", "", ""))
	cli := advDial(t, ts.URL, o, nil)
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM mcp_sessions`), "initialize kept what the client declared")

	serve() // the new process: nothing of the old one's memory is left

	text := r.refused(cli, "notes_read", map[string]any{"id": r.finNote})
	require.Len(t, o.dialogs(), 1, "the restored form was shown")
	require.True(t, strings.HasPrefix(text, "notes_read: the owner approved move request "), text)
	require.Equal(t, approval.DecidedByDialog, func() string {
		var by string
		require.NoError(t, r.db.QueryRowContext(context.Background(),
			`SELECT decided_by FROM access_approvals WHERE project_slug = ?`, gFin).Scan(&by))
		return by
	}())

	// Off: a server without read grants keeps nothing, whatever the client
	// declared, and shows no dialog.
	off := newGrantRig(t, false)
	ops := seedAgentAmbient(t, context.Background(), off.db, "cc/off-agent", gOps, "/work/"+gOps, fsAgent)
	require.NotEmpty(t, ops)
	plain := newOwner(advAnswer("approve", "", ""))
	offCli := advDial(t, off.url, plain, nil)
	off.refused(offCli, "notes_read", map[string]any{"id": off.finNote})
	require.Zero(t, off.count(`SELECT COUNT(*) FROM mcp_sessions`))
	require.Empty(t, plain.dialogs())
	require.Zero(t, off.count(`SELECT COUNT(*) FROM access_approvals`))
}

// Under peer_auth nothing changes: the dialog still only starts a proof. With
// a Touch ID key enrolled, a refusal that files a grant shows the proof
// dialog, which offers no decision; an answer carrying one approves nothing.
// With no proof on hand no dialog is shown at all.
func TestAdvisoryDialog_PeerAuthOffersNoAdvisoryApprove(t *testing.T) {
	t.Run("a proof on hand", func(t *testing.T) {
		o := newOwner(answerWith(mcp.ElicitationResponseActionAccept, map[string]any{"decision": "approve",
			"duration": approval.ForSession, "method": approval.ProofTouchID}))
		r := newFPRig(t, "native confidential", o)
		r.enrollTouchID()
		// A short bound: nothing proves the Touch ID ask, so the dialog waits it
		// out.
		r.cfg.DialogWait = 600 * time.Millisecond
		r.srv = mcpserver.New(r.cfg)
		agent := r.dialOwner(r.agentSocket(), o, nil)
		text := r.refused(agent, "notes_read", map[string]any{"id": r.notes[dlgLegal]})
		shown := o.dialogs()
		require.Len(t, shown, 1)
		props := advSchema(t, shown[0])
		require.NotContains(t, props, "decision", "the peer_auth dialog offers no Approve of its own")
		require.Contains(t, props, "method")
		require.Contains(t, shown[0].Params.Message, "Accepting here grants nothing by itself")
		require.NotContains(t, shown[0].Params.Message, "advisory")
		ask := within(t, o.asks, "the approver ask")
		pending := r.pending()
		require.Len(t, pending, 1, "the answer started a proof and decided nothing: %s", text)
		require.Equal(t, pending[0].ID, ask.RequestID)
		require.Empty(t, pending[0].DecidedBy)
		live, err := r.grants.Live(context.Background())
		require.NoError(t, err)
		require.Empty(t, live)
	})
	t.Run("no proof on hand", func(t *testing.T) {
		o := newOwner(answerWith(mcp.ElicitationResponseActionAccept, map[string]any{"decision": "approve"}))
		r := newFPRig(t, "native confidential", o)
		text := r.refused(r.agent, "notes_read", map[string]any{"id": r.notes[dlgLegal]})
		require.Empty(t, o.dialogs(), "no dialog: nothing in it could approve")
		require.Zero(t, r.tap.count())
		require.Contains(t, text, "\nRemedy: read request R-")
		require.Len(t, r.pending(), 1)
	})
}

// access_request in the advisory tier is registered in words of its own: the
// same name, arguments and hints, worded for a daemon without peer_auth;
// under peer_auth it is the Catalog's, word for word. OffersAccessRequest is
// when serve registers it.
func TestAccessRequest_AdvisoryRegistration(t *testing.T) {
	for _, c := range []struct {
		locks, peer, grants, want bool
	}{
		{false, false, false, false}, // a default install
		{false, false, true, true},   // the advisory tier
		{false, true, false, false},  // peer_auth, nothing locked
		{false, true, true, false},   // peer_auth with read grants, nothing locked
		{true, true, false, true},    // peer_auth locks a project
		{true, true, true, true},
	} {
		require.Equal(t, c.want, mcpserver.OffersAccessRequest(c.locks, c.peer, c.grants), "%+v", c)
	}

	var catalog mcp.Tool
	for _, tool := range mcpserver.Catalog() {
		if tool.Name == mcpserver.AccessRequestTool {
			catalog = tool
		}
	}
	listed := func(t *testing.T, cli *mcpclient.Client) mcp.Tool {
		t.Helper()
		tools, err := cli.ListTools(context.Background(), mcp.ListToolsRequest{})
		require.NoError(t, err)
		require.Len(t, tools.Tools, mcpserver.LockedToolCount)
		for _, tool := range tools.Tools {
			if tool.Name == mcpserver.AccessRequestTool {
				return tool
			}
		}
		require.FailNow(t, "access_request is not registered")
		return mcp.Tool{}
	}

	r := newAdvRig(t, fsOrigins[0], dlgLong)
	advisory := listed(t, advDial(t, r.url, nil, nil))
	require.NotEqual(t, catalog.Description, advisory.Description)
	require.True(t, strings.HasPrefix(advisory.Description, "Ask the owner for a confidential or sealed project. Present with "+
		"read grants on (isolation.requests) on a server without peer_auth."), advisory.Description)
	for _, never := range []string{"locked", "Touch ID", "passkey", "peer_auth socket"} {
		require.NotContains(t, advisory.Description, never, "no word of a lock or a proof this tier lacks")
	}
	require.Equal(t, catalog.Annotations, advisory.Annotations)
	require.Equal(t, advPropNames(catalog.InputSchema.Properties), advPropNames(advisory.InputSchema.Properties),
		"the same arguments (session= aside, which every registered tool gains)")
	require.Equal(t, catalog.InputSchema.Required, advisory.InputSchema.Required)

	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	peer := listed(t, d.dialOwner(d.agentSocket(), nil, nil))
	require.Equal(t, catalog.Description, peer.Description, "under peer_auth, word for word")
}

// advPropNames is a schema's property names but session (which every
// registered tool gains), sorted.
func advPropNames(props map[string]any) []string {
	out := []string{}
	for k := range props {
		if k != "session" {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
