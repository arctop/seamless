package mcp_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// The in-session approval dialog (plan step E): a call refused at a lock, or
// at a confidential fence with a read request, or an access_request, shows
// the owner the request in the agent's own client when it can, and waits for
// the approve core's decision. The dialog's answer alone never grants.

const (
	dlgOrigin = "http://localhost:8081"
	dlgLegal  = "example-legal"
	// dlgLong is a bound no test reaches: a dialog that must not wait.
	dlgLong = time.Minute
)

// dialogRig is a lockRig whose server offers the dialog: access_request
// registered, dialogs bounded at wait and polled every few milliseconds, and
// a tap on every reply that sees the elicitations the server writes.
type dialogRig struct {
	*lockRig
	svc    *approval.Service
	grants *approval.Grants
	srv    *mcpserver.Server
	tap    *elicitTap
}

type dialogOpts struct {
	wait time.Duration
	// passkeys sets up the relying party (the passkey page).
	passkeys bool
	// grants turns read grants on (isolation.requests), with a confidential
	// project no lock covers.
	grants bool
}

func newDialogRig(t *testing.T, o dialogOpts) *dialogRig {
	t.Helper()
	r := newLockRig(t)
	d := &dialogRig{lockRig: r, svc: r.svc, tap: &elicitTap{}}
	if o.passkeys {
		v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{dlgOrigin}})
		require.NoError(t, err)
		d.svc = approval.New(approval.Config{DB: r.db, Events: r.rec, Host: lkHost, LeaseCap: 8 * time.Hour, Now: r.clock,
			Passkeys: v, Origins: []string{dlgOrigin}, UserName: "owner", Programs: lkPrograms})
	}
	cfg := mcpserver.Config{
		DB: r.db, Files: r.mgr, Retrieve: r.ret, Events: r.rec, APIKey: testKey, LocalHost: lkHost,
		Features: config.Features{Research: true}, Now: r.clock, Approvals: d.svc, OfferAccessRequest: true,
		DialogWait: o.wait, DialogPoll: 5 * time.Millisecond,
	}
	if o.grants {
		ctx := context.Background()
		_, err := store.EnsureProject(ctx, r.db, dlgLegal, dlgLegal)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, r.db, dlgLegal, core.IsolationConfidential))
		d.grants = approval.NewGrants(approval.GrantsConfig{Approvals: d.svc})
		cfg.Grants = d.grants
	}
	d.srv = mcpserver.New(cfg)
	return d
}

// listen serves the dialog server through the tap, with view attached to
// every request as the transport edge does.
func (d *dialogRig) listen(view func() (access.View, bool)) string {
	h := d.tap.wrap(d.srv.Handler())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if v, ok := view(); ok {
			req = req.WithContext(access.WithView(req.Context(), v))
		}
		h.ServeHTTP(w, req)
	}))
	d.t.Cleanup(ts.Close)
	return ts.URL
}

// socketView is the owner's socket view of a process chain, with its live
// leases at the rig's clock (peerHandler's leasedView).
func (d *dialogRig) socketView(chain ...access.ProcessToken) func() (access.View, bool) {
	return func() (access.View, bool) {
		v, ok := d.policy.PeerChainView(501, chain)
		if !ok {
			return access.View{}, false
		}
		keys := make([]string, len(chain))
		for i, c := range chain {
			keys[i] = c.Key()
		}
		now := d.clock()
		rows, err := store.ActiveAccessLeasesFor(context.Background(), d.db, keys, now)
		if err != nil {
			return access.View{}, false
		}
		return v.WithLeases(now, store.AccessLeases(rows)), true
	}
}

// agentSocket is the agent's own socket transport (bridge, then agent).
func (d *dialogRig) agentSocket() string { return d.listen(d.socketView(lkBridge, lkAgent)) }

// owner is the agent's client standing in for the owner at the dialog: it
// records what it was shown and answers with answer; asks and completions
// collect the notifications the server sent.
type owner struct {
	answer func(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error)

	mu    sync.Mutex
	shown []mcp.ElicitationRequest
	seen  chan mcp.ElicitationRequest

	asks        chan core.ApproverLaunch
	completions chan string
}

func newOwner(answer func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error)) *owner {
	return &owner{answer: answer, seen: make(chan mcp.ElicitationRequest, 8),
		asks: make(chan core.ApproverLaunch, 8), completions: make(chan string, 8)}
}

func (o *owner) Elicit(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	o.mu.Lock()
	o.shown = append(o.shown, req)
	o.mu.Unlock()
	o.seen <- req
	return o.answer(ctx, req)
}

func (o *owner) dialogs() []mcp.ElicitationRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]mcp.ElicitationRequest(nil), o.shown...)
}

func answerWith(action mcp.ElicitationResponseAction, content any) func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	return func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
		return &mcp.ElicitationResult{ElicitationResponse: mcp.ElicitationResponse{Action: action, Content: content}}, nil
	}
}

func accepting(duration, method string) func(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	return answerWith(mcp.ElicitationResponseActionAccept, map[string]any{"duration": duration, "method": method})
}

// dialOwner connects to base as the agent, with o answering its dialogs (nil:
// a client that declares no elicitation) and caps overlaid on what the
// handler declares (nil: the bare `elicitation: {}`, form mode).
func (d *dialogRig) dialOwner(base string, o *owner, caps *mcp.ElicitationCapability) *mcpclient.Client {
	d.t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + testKey, mcpserver.HostHeader: lkHost,
		mcpserver.AgentProcessHeader: lkAgent.AgentID()}
	trans, err := transport.NewStreamableHTTP(base, transport.WithHTTPHeaders(headers))
	require.NoError(d.t, err)
	var opts []mcpclient.ClientOption
	if o != nil {
		opts = append(opts, mcpclient.WithElicitationHandler(o))
	}
	cli := mcpclient.NewClient(trans, opts...)
	d.t.Cleanup(func() { _ = cli.Close() })
	if o != nil {
		cli.OnNotification(func(n mcp.JSONRPCNotification) {
			switch n.Method {
			case core.ApproverNotification:
				b, err := json.Marshal(n.Params.AdditionalFields)
				if err != nil {
					return
				}
				var ask core.ApproverLaunch
				if json.Unmarshal(b, &ask) == nil {
					o.asks <- ask
				}
			case string(mcp.MethodNotificationElicitationComplete):
				id, _ := n.Params.AdditionalFields["elicitationId"].(string)
				o.completions <- id
			}
		})
	}
	ctx := context.Background()
	require.NoError(d.t, cli.Start(ctx))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "dialog", Version: "0"}
	if caps != nil {
		initReq.Params.Capabilities.Elicitation = caps
	}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(d.t, err)
	return cli
}

// touchKey is a software P-256 key enrolled as a Touch ID approver: the
// helper's Secure Enclave key, played in the test.
type touchKey struct {
	key  *ecdsa.PrivateKey
	cred store.ApproverCredential
}

func sign(t *testing.T, key *ecdsa.PrivateKey, msg []byte) []byte {
	t.Helper()
	d := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, key, d[:])
	require.NoError(t, err)
	return sig
}

// enrollTouchID enrolls a key with a one-time code, as `seamlessd approvers
// enroll-code` and the helper's enrollment do.
func (d *dialogRig) enrollTouchID() touchKey {
	d.t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(d.t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(d.t, err)
	code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverTouchID, d.clock())
	require.NoError(d.t, err)
	cred, err := d.svc.EnrollTouchID(ctx, "this Mac", code, pub, sign(d.t, key, approval.TouchIDEnrollMessage(code, pub)))
	require.NoError(d.t, err)
	return touchKey{key: key, cred: cred}
}

// proveTouchID signs request id over duration as the helper would and hands
// the proof to the approve core, through the lease path or the grant path.
func (d *dialogRig) proveTouchID(k touchKey, id, duration string) error {
	ctx := context.Background()
	a, err := store.AccessApprovalByID(ctx, d.db, id)
	if err != nil {
		return err
	}
	facts, err := d.svc.Facts(ctx, id)
	if err != nil {
		return err
	}
	d.t.Helper()
	dgst := sha256.Sum256(approval.TouchIDApproveMessage(a.ID, a.Challenge, facts.Digest, facts.Access, duration))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, dgst[:])
	if err != nil {
		return err
	}
	dec := approval.Decision{ID: id, Duration: duration, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofTouchID, CredentialID: k.cred.ID, Signature: sig}}
	if a.Kind == store.AccessKindGrant {
		_, err = d.grants.Approve(ctx, dec)
		return err
	}
	_, err = d.svc.Approve(ctx, dec)
	return err
}

func (d *dialogRig) liveLeases() []store.AccessLease {
	d.t.Helper()
	rows, err := store.LiveAccessLeases(context.Background(), d.db, d.clock())
	require.NoError(d.t, err)
	return rows
}

// elicitTap sees every elicitation/create a server writes on its replies.
type elicitTap struct {
	mu  sync.Mutex
	ids []json.RawMessage
}

func (tp *elicitTap) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&tapWriter{ResponseWriter: w, tap: tp}, r)
	})
}

func (tp *elicitTap) count() int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return len(tp.ids)
}

func (tp *elicitTap) lastID() json.RawMessage {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if len(tp.ids) == 0 {
		return nil
	}
	return tp.ids[len(tp.ids)-1]
}

type tapWriter struct {
	http.ResponseWriter
	tap *elicitTap
	buf []byte
}

func (w *tapWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Method == string(mcp.MethodElicitationCreate) {
			w.tap.mu.Lock()
			w.tap.ids = append(w.tap.ids, msg.ID)
			w.tap.mu.Unlock()
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *tapWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *tapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// within receives from ch, failing the test after a generous bound.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		require.FailNow(t, "timed out waiting for "+what)
		var zero T
		return zero
	}
}

func TestDialog_TouchIDProofApproves(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.seed()
	d.session("cc/fin", lkFin)
	key := d.enrollTouchID()
	o := newOwner(accepting(approval.For1h, approval.ProofTouchID))
	agent := d.dialOwner(d.agentSocket(), o, nil)

	// The bridge's half, played here: take the ask off the call's reply and
	// prove it as the helper does, through the approve core.
	type proved struct {
		ask core.ApproverLaunch
		err error
	}
	done := make(chan proved, 1)
	go func() {
		select {
		case ask := <-o.asks:
			done <- proved{ask, d.proveTouchID(key, ask.RequestID, ask.Duration)}
		case <-time.After(20 * time.Second):
			done <- proved{err: fmt.Errorf("no approver ask")}
		}
	}()
	text := d.refused(agent, "tasks_list", nil)
	p := within(t, done, "the proof")
	require.NoError(t, p.err)

	a, err := store.AccessApprovalByID(context.Background(), d.db, p.ask.RequestID)
	require.NoError(t, err, "the ask names the request by its full id")
	require.Equal(t, store.ApprovalApproved, a.Status)
	require.Equal(t, approval.For1h, p.ask.Duration, "the duration chosen in the dialog")
	short := approval.ShortID(a.ID)
	leases := d.liveLeases()
	require.Len(t, leases, 1)
	require.Equal(t, a.ID, leases[0].ApprovalID)

	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.Equal(t, "tasks_list: the owner approved access request "+short+": project "+lkFin+
		" is open to this agent until "+access.Clock(leases[0].ExpiresAt)+".", ls[0])
	require.Equal(t, "Remedy: this call ran before the approval and did nothing -- retry it now.", ls[1])

	// What the dialog showed: the daemon's facts, warnings first.
	shown := o.dialogs()
	require.Len(t, shown, 1)
	msg := shown[0].Params.Message
	// Claude Code collapses the dialog to its first lines, cut at the
	// terminal's edge: the warning count first, then what is asked.
	lead, _, _ := strings.Cut(msg, "\n\n")
	require.Equal(t, "1 warning. Claude Code 2.1.319 asks for read-write access to "+lkFin+".\n"+
		"Until its session ends (8 hours at most), unless you pick less below.", lead)
	require.NotContains(t, lead, short, "the request's id is not the lead")
	require.Contains(t, msg, "Project: "+lkFin+" (confidential, locked)")
	require.Contains(t, msg, "Session: cc/fin\n")
	require.Contains(t, msg, "Working directory: /work/"+lkFin)
	require.Contains(t, msg, "Agent process: Claude Code 2.1.319\n")
	require.Contains(t, msg, "Program: "+lkClaude+"\n", "the full path, so a borrowed name shows")
	require.Contains(t, msg, "Request: "+short+", a new request\n")
	require.NotContains(t, msg, a.ID, "no full ids")
	require.Contains(t, msg, "the session works outside "+lkFin+"'s repository")
	require.Less(t, strings.Index(msg, "Warnings:"), strings.Index(msg, "Project:"), "warnings come first")
	require.True(t, strings.HasSuffix(msg, "Declining leaves the request pending for `seam approve "+short+"` in your own terminal."), msg)
	require.NotContains(t, msg, "reason", "a refusal's request carries no reason")
	schema, ok := shown[0].Params.RequestedSchema.(map[string]any)
	require.True(t, ok)
	props := schema["properties"].(map[string]any)
	require.Equal(t, []any{"touchid"}, props["method"].(map[string]any)["enum"], "only the proofs on hand")
	require.Equal(t, []any{"session", "1h", "4h"}, props["duration"].(map[string]any)["enum"])
	require.Equal(t, "session", props["duration"].(map[string]any)["default"])

	// The retry runs under the lease.
	first, _ := d.ok(agent, "tasks_list", nil)
	require.Contains(t, first, "reconcile the ledger")
}

func TestDialog_AcceptWithoutProofGrantsNothing(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: 800 * time.Millisecond})
	d.seed()
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(accepting(approval.ForSession, approval.ProofTouchID))
	agent := d.dialOwner(d.agentSocket(), o, nil)

	text := d.refused(agent, "tasks_list", nil)
	ask := within(t, o.asks, "the approver ask")
	pending := d.pending()
	require.Len(t, pending, 1)
	require.Equal(t, pending[0].ID, ask.RequestID)
	short := approval.ShortID(pending[0].ID)
	require.Empty(t, d.liveLeases(), "an accepted dialog is not an approval")

	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.Contains(t, ls[0], "project "+lkFin+" is locked")
	require.True(t, strings.HasPrefix(ls[1], "Remedy: the approval dialog stopped waiting at "), ls[1])
	require.NotContains(t, ls[1], " is waiting -- ", "one remedy, not the original with a second after it")
	require.NotContains(t, ls[1], "passkey", "no passkey is enrolled: none is offered")
	require.True(t, strings.HasSuffix(ls[1], " without a decision; access request "+short+
		" stays pending for the owner: `seam approve "+short+"` in their own terminal, or Touch ID in the next approval "+
		"dialog; retry once it is approved, and keep to other projects until then."), ls[1], "Touch ID is offered: a key is enrolled")
}

func TestDialog_DeclineOrCancelLeavesThePendingRequest(t *testing.T) {
	for _, c := range []struct {
		action mcp.ElicitationResponseAction
		word   string
	}{
		{mcp.ElicitationResponseActionDecline, "declined"},
		{mcp.ElicitationResponseActionCancel, "cancelled"},
	} {
		t.Run(c.word, func(t *testing.T) {
			d := newDialogRig(t, dialogOpts{wait: dlgLong})
			d.session("cc/fin", lkFin)
			d.enrollTouchID()
			o := newOwner(answerWith(c.action, nil))
			agent := d.dialOwner(d.agentSocket(), o, nil)

			text := d.refused(agent, "tasks_list", nil)
			require.Len(t, o.dialogs(), 1)
			pending := d.pending()
			require.Len(t, pending, 1, "declining denies nothing: an agent's connection may not deny")
			short := approval.ShortID(pending[0].ID)
			require.Contains(t, text, "\nRemedy: the approval dialog was "+c.word+"; access request "+short+" stays pending for the owner")
			require.Len(t, lines(text), 2)
			require.Empty(t, o.asks, "no proof was asked for")
			require.Empty(t, d.liveLeases())
		})
	}
}

// A call cancelled mid-wait ends its dialog with it: the same connection is
// offered a fresh dialog for the same request at once (one per request per
// connection at a time, and the first is gone).
func TestDialog_ClientCancelEndsTheWait(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	var n atomic.Int32
	o := newOwner(func(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
		if n.Add(1) == 1 {
			return accepting(approval.ForSession, approval.ProofTouchID)(ctx, req)
		}
		return answerWith(mcp.ElicitationResponseActionDecline, nil)(ctx, req)
	})
	agent := d.dialOwner(d.agentSocket(), o, nil)

	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-o.asks:
		case <-time.After(20 * time.Second):
		}
		cancel()
	}()
	_, err := agent.CallTool(callCtx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "tasks_list"}})
	require.Error(t, err, "the call was cancelled")

	deadline := time.Now().Add(20 * time.Second)
	var text string
	for time.Now().Before(deadline) {
		text = d.refused(agent, "tasks_list", nil)
		if strings.Contains(text, "the approval dialog was declined") {
			break
		}
	}
	require.Contains(t, text, "the approval dialog was declined", "the cancelled call's dialog was released")
	require.Len(t, d.pending(), 1, "the same request, found again")
	require.Empty(t, d.liveLeases())
}

// Without every precondition the call answers at once, as before: no
// elicitation is written at all.
func TestDialog_PreconditionsElseTheTextPath(t *testing.T) {
	plainRefusal := func(t *testing.T, text string) {
		t.Helper()
		ls := lines(text)
		require.Len(t, ls, 2, text)
		require.Contains(t, ls[0], "project "+lkFin+" is locked")
		require.True(t, strings.HasSuffix(ls[1], "; retry once it is approved, and keep to other projects until then."), ls[1])
		require.NotContains(t, text, "approval dialog")
	}
	t.Run("client without the capability", func(t *testing.T) {
		d := newDialogRig(t, dialogOpts{wait: dlgLong})
		d.session("cc/fin", lkFin)
		d.enrollTouchID()
		plainRefusal(t, d.refused(d.dialOwner(d.agentSocket(), nil, nil), "tasks_list", nil))
		require.Zero(t, d.tap.count())
	})
	t.Run("no proof can finish in the dialog", func(t *testing.T) {
		d := newDialogRig(t, dialogOpts{wait: dlgLong})
		d.session("cc/fin", lkFin)
		o := newOwner(accepting(approval.ForSession, approval.ProofTouchID))
		plainRefusal(t, d.refused(d.dialOwner(d.agentSocket(), o, nil), "tasks_list", nil))
		require.Zero(t, d.tap.count())
		require.Empty(t, o.dialogs())
	})
	t.Run("a passkey needs a client that opens URLs", func(t *testing.T) {
		d := newDialogRig(t, dialogOpts{wait: dlgLong, passkeys: true})
		d.session("cc/fin", lkFin)
		d.enrollPasskey()
		o := newOwner(accepting(approval.ForSession, approval.ProofPasskey))
		plainRefusal(t, d.refused(d.dialOwner(d.agentSocket(), o, nil), "tasks_list", nil))
		require.Zero(t, d.tap.count())
	})
	t.Run("the TCP key", func(t *testing.T) {
		d := newDialogRig(t, dialogOpts{wait: dlgLong})
		d.session("cc/fin", lkFin)
		d.enrollTouchID()
		o := newOwner(accepting(approval.ForSession, approval.ProofTouchID))
		tcp := d.dialOwner(d.listen(func() (access.View, bool) { return d.policy.OwnerView(), true }), o, nil)
		text := d.refused(tcp, "tasks_list", nil)
		require.Contains(t, text, "is locked")
		require.NotContains(t, text, "approval dialog")
		require.Zero(t, d.tap.count())
		require.Empty(t, d.pending(), "the TCP key files nothing")
	})
}

// Any process of the owner's account can answer a dialog -- here a second
// connection, posting an accept for the first one's elicitation. The answer
// starts a proof and nothing more: without one, nothing is granted.
func TestDialog_AnotherConnectionsAnswerGrantsNothing(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: 1500 * time.Millisecond})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(func(ctx context.Context, _ mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
		<-ctx.Done() // the agent's own client never answers
		return nil, ctx.Err()
	})
	base := d.agentSocket()
	agent := d.dialOwner(base, o, nil)

	type result struct {
		res *mcp.CallToolResult
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := agent.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "tasks_list"}})
		got <- result{res, err}
	}()
	within(t, o.seen, "the dialog")
	id := d.tap.lastID()
	require.NotNil(t, id)

	forged := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"action":"accept","content":{"duration":"4h","method":"touchid"}}}`, id)
	req, err := http.NewRequest(http.MethodPost, base, strings.NewReader(forged))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", agent.GetSessionId())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Less(t, resp.StatusCode, 300, "the forged answer reached the dialog")

	ask := within(t, o.asks, "the approver ask")
	require.Equal(t, approval.For4h, ask.Duration, "the forged answer was taken as the dialog's")
	r := within(t, got, "the call's result")
	require.NoError(t, r.err)
	require.True(t, r.res.IsError)
	text := r.res.Content[0].(mcp.TextContent).Text
	require.Contains(t, text, "\nRemedy: the approval dialog stopped waiting at ")
	require.Empty(t, d.liveLeases(), "an answer is not a proof")
	require.Len(t, d.pending(), 1)
}

// enrollPasskey enrolls a virtual authenticator as the owner's passkey.
func (d *dialogRig) enrollPasskey() *passkeytest.Authenticator {
	d.t.Helper()
	ctx := context.Background()
	a, err := passkeytest.New(true)
	require.NoError(d.t, err)
	code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverPasskey, d.clock())
	require.NoError(d.t, err)
	begun, err := d.svc.BeginPasskey(ctx, "security key", code)
	require.NoError(d.t, err)
	resp, err := a.Create(begun.Options, dlgOrigin)
	require.NoError(d.t, err)
	fin, err := d.svc.FinishPasskey(ctx, begun.ID, resp)
	require.NoError(d.t, err)
	require.NotNil(d.t, fin.Credential)
	return a
}

// provePasskey approves request id for duration with the authenticator's
// assertion over the options the passkey page issues for that duration.
func (d *dialogRig) provePasskey(a *passkeytest.Authenticator, id, duration string) error {
	ctx := context.Background()
	facts, err := d.svc.Facts(ctx, id)
	if err != nil {
		return err
	}
	opts, err := d.svc.AssertionOptionsFor(ctx, id, duration, facts.Digest)
	if err != nil {
		return err
	}
	resp, err := a.Get(opts, dlgOrigin)
	if err != nil {
		return err
	}
	_, err = d.svc.Approve(ctx, approval.Decision{ID: id, Duration: duration, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofPasskey, Assertion: resp}})
	return err
}

// The passkey path: the owner is sent to the request's passkey page (a URL
// elicitation), and when the approval lands there the interaction is closed
// (notifications/elicitation/complete). With Touch ID enrolled too, the form
// offers both, the owner picks the passkey and a duration, and the page is
// opened for that duration (?for=); without, the page is the dialog and asks
// the duration itself.
func TestDialog_PasskeyPage(t *testing.T) {
	for _, withTouchID := range []bool{false, true} {
		t.Run(fmt.Sprintf("touch id enrolled %v", withTouchID), func(t *testing.T) {
			d := newDialogRig(t, dialogOpts{wait: dlgLong, passkeys: true})
			d.seed()
			d.session("cc/fin", lkFin)
			auth := d.enrollPasskey()
			if withTouchID {
				d.enrollTouchID()
			}
			type opened struct{ url, id string }
			pages := make(chan opened, 1)
			o := newOwner(func(ctx context.Context, req mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
				if req.Params.Mode == mcp.ElicitationModeURL {
					pages <- opened{req.Params.URL, req.Params.ElicitationID}
					return answerWith(mcp.ElicitationResponseActionAccept, nil)(ctx, req)
				}
				return accepting(approval.For1h, approval.ProofPasskey)(ctx, req)
			})
			agent := d.dialOwner(d.agentSocket(), o, &mcp.ElicitationCapability{Form: &struct{}{}, URL: &struct{}{}})

			// The duration the page approves: the form's choice when the form
			// asked one, else the page's own.
			want := approval.For4h
			if withTouchID {
				want = approval.For1h
			}
			proved := make(chan error, 1)
			var page opened
			go func() {
				select {
				case page = <-pages:
					u, err := url.Parse(page.url)
					if err != nil {
						proved <- err
						return
					}
					proved <- d.provePasskey(auth, path.Base(u.Path), want)
				case <-time.After(20 * time.Second):
					proved <- fmt.Errorf("no passkey page")
				}
			}()
			text := d.refused(agent, "tasks_list", nil)
			require.NoError(t, within(t, proved, "the passkey proof"))

			u, err := url.Parse(page.url)
			require.NoError(t, err)
			id := path.Base(u.Path)
			if withTouchID {
				require.Equal(t, d.svc.Page(id)+"?for=1h", page.url, "the request's own passkey page, for the form's duration")
			} else {
				require.Equal(t, d.svc.Page(id), page.url, "the request's own passkey page; it asks the duration")
			}
			require.Equal(t, page.id, within(t, o.completions, "the elicitation's completion"))
			shown := o.dialogs()
			if withTouchID {
				require.Len(t, shown, 2, "the form, then the page")
				props := shown[0].Params.RequestedSchema.(map[string]any)["properties"].(map[string]any)
				require.Equal(t, []any{"touchid", "passkey"}, props["method"].(map[string]any)["enum"])
			} else {
				require.Len(t, shown, 1, "the page is the dialog")
			}
			require.Equal(t, mcp.ElicitationModeURL, shown[len(shown)-1].Params.Mode)
			last := shown[len(shown)-1].Params.Message
			require.True(t, strings.HasPrefix(last, "1 warning. Claude Code 2.1.319 asks for read-write "+
				"access to "+lkFin+".\n"), "the warning count and what is asked come first: %s", last)
			if withTouchID {
				require.Contains(t, last, ".\nApprove it for 1 hour with your passkey, or deny it, on the page this opens")
				props := shown[0].Params.RequestedSchema.(map[string]any)["properties"].(map[string]any)
				require.Equal(t, []any{"Touch ID", "Passkey (opens the approval page for the duration chosen here)"},
					props["method"].(map[string]any)["enumNames"])
			} else {
				require.Contains(t, last, ".\nApprove it with your passkey, or deny it, on the page this opens")
			}
			require.Empty(t, o.asks, "no Touch ID ask")

			leases := d.liveLeases()
			require.Len(t, leases, 1)
			length := map[string]time.Duration{approval.For1h: time.Hour, approval.For4h: 4 * time.Hour}[want]
			require.Equal(t, d.clock().Add(length), leases[0].ExpiresAt, "the duration the passkey signed")
			require.Equal(t, "tasks_list: the owner approved access request "+approval.ShortID(id)+": project "+lkFin+
				" is open to this agent until "+access.Clock(leases[0].ExpiresAt)+".", lines(text)[0])
			first, _ := d.ok(agent, "tasks_list", nil)
			require.Contains(t, first, "reconcile the ledger")
		})
	}
}

// A read refused at a confidential project's fence (isolation.requests)
// offers the same dialog for its read request, decided by the read grants'
// approve core.
func TestDialog_ReadGrantRequest(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	admin := d.dial(d.serve(d.lockRig.srv, nil), false)
	out, _ := d.ok(admin, "notes_create", map[string]any{"project": dlgLegal, "title": "contract notes", "body": "the vendor contract terms"})
	var note map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &note))
	d.session("cc/ops", lkOps)
	key := d.enrollTouchID()
	o := newOwner(accepting(approval.ForSession, approval.ProofTouchID))
	agent := d.dialOwner(d.agentSocket(), o, nil)

	done := make(chan error, 1)
	go func() {
		select {
		case ask := <-o.asks:
			done <- d.proveTouchID(key, ask.RequestID, ask.Duration)
		case <-time.After(20 * time.Second):
			done <- fmt.Errorf("no approver ask")
		}
	}()
	text := d.refused(agent, "notes_read", map[string]any{"id": note["id"]})
	require.NoError(t, within(t, done, "the proof"))
	msg := o.dialogs()[0].Params.Message
	require.Contains(t, msg, "Project: "+dlgLegal+" (confidential), read from project "+lkOps)
	require.Contains(t, msg, "Access: read-only")
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.True(t, strings.HasPrefix(ls[0], "notes_read: the owner approved read request R-"), ls[0])
	require.Contains(t, ls[0], ": project "+dlgLegal+" is readable from project "+lkOps+", nothing else until ")
	require.Empty(t, d.liveLeases(), "a read grant is never a lease")

	out, _ = d.ok(agent, "notes_read", map[string]any{"id": note["id"]})
	require.Contains(t, out, "the vendor contract terms")
}

func TestAccessRequest_RegisteredOnlyWhenLocked(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	listed, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.NotContains(t, names, mcpserver.AccessRequestTool, "a default install has no access_request")
	_, err = cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: mcpserver.AccessRequestTool,
		Arguments: map[string]any{"project": lkFin}}})
	require.Error(t, err, "and refuses it as an unknown tool")

	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	locked := d.dialOwner(d.agentSocket(), nil, nil)
	listed, err = locked.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Tools, mcpserver.LockedToolCount)
	names = names[:0]
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, mcpserver.AccessRequestTool)
}

// With nothing locked, a client that can show dialogs is answered exactly as
// one that cannot.
func TestDialog_NothingLockedChangesNothing(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	capable := d.dialOwner(url, newOwner(accepting(approval.ForSession, approval.ProofTouchID)), nil)
	bare := dialClient(t, ctx, url, testKey)
	args := map[string]any{"name": "x", "kind": "gotcha", "description": "d", "body": "b\n"}
	call := func(cli *mcpclient.Client) string {
		res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "memory_write", Arguments: args}})
		require.NoError(t, err)
		require.True(t, res.IsError)
		return resultText(t, res)
	}
	require.Equal(t, call(bare), call(capable))
}

func TestAccessRequest_TextPathAndBoundary(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	sessID := d.session("cc/fin", lkFin)
	agent := d.dialOwner(d.agentSocket(), nil, nil)

	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin, "reason": "reconcile the quarter"})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	pending := d.pending()
	require.Len(t, pending, 1)
	short := approval.ShortID(pending[0].ID)
	require.Equal(t, pending[0].ID, res["id"])
	require.Equal(t, short, res["short_id"])
	require.Equal(t, lkFin, res["project"])
	require.Equal(t, "request", res["kind"])
	require.Equal(t, "pending", res["status"])
	require.NotContains(t, res, "dialog", "no dialog was shown")
	require.Contains(t, res["next"], "`seam approve "+short+"`")
	require.Equal(t, "reconcile the quarter", pending[0].Reason)
	require.Equal(t, sessID, pending[0].SessionID)

	// Asking again finds the same request.
	out, _ = d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
	require.Contains(t, out, pending[0].ID)
	require.Len(t, d.pending(), 1)

	// The reason is shown to the owner and kept out of every event.
	var n int
	require.NoError(t, d.db.QueryRow(`SELECT COUNT(*) FROM events WHERE payload LIKE '%reconcile the quarter%'`).Scan(&n))
	require.Zero(t, n)
	ev := findToolCall(t, d.db, mcpserver.AccessRequestTool)
	args, _ := ev.Payload["args"].(map[string]any)
	require.NotContains(t, args, "reason")

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"project": lkFin, "reason": strings.Repeat("r", store.MaxAccessReasonRunes+1)}, "the reason is limited to 200 characters"},
		{map[string]any{"project": "global"}, "name a project: the global scope is never locked"},
		{map[string]any{"project": "../notes/_global"}, "access_request: "},
		{map[string]any{"project": lkOps}, "that project is not locked: nothing to request"},
		{map[string]any{}, "project"},
	} {
		require.Contains(t, d.refused(agent, mcpserver.AccessRequestTool, c.args), c.want, "%v", c.args)
	}
	tcp := d.dialOwner(d.listen(func() (access.View, bool) { return d.policy.OwnerView(), true }), nil, nil)
	require.Contains(t, d.refused(tcp, mcpserver.AccessRequestTool, map[string]any{"project": lkFin}),
		"this connection has no verified process")
	require.Len(t, d.pending(), 1)
}

func TestAccessRequest_DialogShowsTheReasonAsTheAgents(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	agent := d.dialOwner(d.agentSocket(), o, nil)

	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin,
		"reason": "reconcile\u202e the\u2066 ledger\u200b"})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Equal(t, "pending", res["status"])
	require.Equal(t, "declined", res["dialog"])
	require.Contains(t, res["next"], "`seam approve ")
	shown := o.dialogs()
	require.Len(t, shown, 1)
	msg := shown[0].Params.Message
	require.Contains(t, msg, "\nThe agent's reason, in its own words (Seamless did not check it):\n\"reconcile the ledger\"\n")
	for _, r := range []string{"\u202e", "\u2066", "\u200b"} {
		require.NotContains(t, msg, r, "control and bidi characters are stripped")
	}
	require.Len(t, d.pending(), 1)
}

// approverEnded posts the bridge's word that the Touch ID helper it launched
// for request id ended (outcome), on MCP session sid of base.
func (d *dialogRig) approverEnded(base, sid, id, outcome string) {
	d.t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":{"request_id":%q,"outcome":%q}}`,
		approverEndedMethod, id, outcome)
	req, err := http.NewRequest(http.MethodPost, base, strings.NewReader(body))
	require.NoError(d.t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(d.t, err)
	_ = resp.Body.Close()
	require.Equal(d.t, http.StatusAccepted, resp.StatusCode)
}

// approverEndedMethod is the bridge's notification, pinned here as the bridge
// pins it (cmd/seam/mcpproxy_approver_test.go).
const approverEndedMethod = "notifications/seamless/approver_ended"

// Accepted with Touch ID, then the helper ended without an approval: the
// bridge says so and the call answers at once, the request still pending,
// in words that say what happened. Nothing is denied or approved.
func TestDialog_TouchIDEndAnswersAtOnce(t *testing.T) {
	for _, c := range []struct {
		outcome, answer, words, next string
	}{
		{"cancelled", "touchid_cancelled", "Remedy: Touch ID was cancelled in the approval dialog, so nothing was approved; ",
			"Touch ID was cancelled, so nothing was approved and access request "},
		{"closed", "touchid_closed", "Remedy: the Touch ID approver closed without approving; ",
			"the Touch ID approver closed without approving, so access request "},
	} {
		t.Run(c.outcome, func(t *testing.T) {
			d := newDialogRig(t, dialogOpts{wait: dlgLong})
			d.session("cc/fin", lkFin)
			d.enrollTouchID()
			o := newOwner(accepting(approval.For1h, approval.ProofTouchID))
			base := d.agentSocket()
			agent := d.dialOwner(base, o, nil)
			// The bridge's word is a POST of its own, from the agent's tree;
			// it reaches the dialog through any listener of the socket.
			own := d.agentSocket()
			bridge := func() {
				select {
				case ask := <-o.asks:
					d.approverEnded(own, agent.GetSessionId(), ask.RequestID, c.outcome)
				case <-time.After(20 * time.Second):
				}
			}

			go bridge()
			start := time.Now()
			text := d.refused(agent, "tasks_list", nil)
			require.Less(t, time.Since(start), dlgLong/2, "the call did not wait out its dialog")
			pending := d.pending()
			require.Len(t, pending, 1, "the request stays pending")
			short := approval.ShortID(pending[0].ID)
			ls := lines(text)
			require.Len(t, ls, 2, text)
			require.Equal(t, c.words+"access request "+short+" stays pending for the owner: `seam approve "+short+
				"` in their own terminal, or Touch ID in the next approval dialog; retry once it is approved, and keep to "+
				"other projects until then.", ls[1])
			require.Empty(t, d.liveLeases())

			// access_request, the same way.
			go bridge()
			start = time.Now()
			out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
			require.Less(t, time.Since(start), dlgLong/2)
			var res map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &res))
			require.Equal(t, pending[0].ID, res["id"])
			require.Equal(t, "pending", res["status"])
			require.Equal(t, c.answer, res["dialog"])
			require.True(t, strings.HasPrefix(res["next"].(string), c.next+short+" is still pending: the owner approves it "), res["next"])
			require.Len(t, d.pending(), 1)
			require.Empty(t, d.liveLeases())
		})
	}
}

// The bridge's word ends only the wait it is about: from another agent's
// tree, or with an outcome it does not know, it is dropped and the dialog
// waits out its bound as before.
func TestDialog_TouchIDEndFromElsewhereIsDropped(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: 1500 * time.Millisecond})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(accepting(approval.For1h, approval.ProofTouchID))
	base := d.agentSocket()
	stranger := d.listen(d.socketView(lkBridge, lkStranger))
	agent := d.dialOwner(base, o, nil)
	go func() {
		select {
		case ask := <-o.asks:
			d.approverEnded(stranger, agent.GetSessionId(), ask.RequestID, "cancelled")
			d.approverEnded(base, agent.GetSessionId(), ask.RequestID, "maybe")
			d.approverEnded(base, agent.GetSessionId(), "01M6SBKTYM04X9QS4P6YQPBRMG", "cancelled")
		case <-time.After(20 * time.Second):
		}
	}()
	text := d.refused(agent, "tasks_list", nil)
	require.Contains(t, text, "\nRemedy: the approval dialog stopped waiting at ")
	require.NotContains(t, text, "Touch ID was cancelled")
	require.Len(t, d.pending(), 1)
}

// A tree that holds a live lease asks again: the lease answers, nothing is
// filed and no dialog is shown, until the expiry warning starts; from then
// on, asking again files a refresh (a new approval).
func TestAccessRequest_LiveLeaseAnswersUntilTheExpiryWarning(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong})
	d.session("cc/fin", lkFin)
	d.enrollTouchID()
	o := newOwner(answerWith(mcp.ElicitationResponseActionDecline, nil))
	agent := d.dialOwner(d.agentSocket(), o, nil)

	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	id := res["id"].(string)
	short := approval.ShortID(id)
	lease := d.approve(id, 4*time.Hour)
	shown := len(o.dialogs())

	out, _ = d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin, "reason": "one more look"})
	res = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Equal(t, map[string]any{
		"id": id, "short_id": short, "project": lkFin, "kind": "request", "status": "approved",
		"open_until": lease.ExpiresAt.UTC().Format(time.RFC3339),
		"next": "project " + lkFin + " is already open to this agent until " + access.Clock(lease.ExpiresAt) + " (approved as " +
			short + "), so nothing was filed. Nothing extends it silently: from " + access.Clock(lease.ExpiresAt.Add(-30*time.Minute)) +
			", 30 minutes before it ends, asking again files a new approval; keep working there until then",
	}, res)
	require.Empty(t, d.pending(), "nothing filed")
	require.Len(t, o.dialogs(), shown, "no dialog")

	d.set(lease.ExpiresAt.Add(-30 * time.Minute))
	out, _ = d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
	res = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Equal(t, "refresh", res["kind"])
	require.Equal(t, "pending", res["status"])
	require.NotEqual(t, id, res["id"])
	require.Len(t, d.pending(), 1)
}

// With read grants on (isolation.requests), access_request for a
// confidential project no lock covers files its read grant request, as
// `seam access request` does; with them off, the answer is the lock's "not
// locked", word for word.
func TestAccessRequest_ReadGrantOfAConfidentialProject(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	sess := d.session("cc/ops", lkOps)
	agent := d.dialOwner(d.agentSocket(), nil, nil)

	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": dlgLegal, "reason": "read the vendor contract"})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	pending := d.pending()
	require.Len(t, pending, 1)
	short := approval.ShortID(pending[0].ID)
	require.Equal(t, store.AccessKindGrant, pending[0].Kind)
	require.Equal(t, lkOps, pending[0].FromProject)
	require.Equal(t, sess, pending[0].SessionID)
	require.Equal(t, "read the vendor contract", pending[0].Reason)
	require.Equal(t, pending[0].ID, res["id"])
	require.Equal(t, "grant", res["kind"])
	require.Equal(t, lkOps, res["from"])
	require.Equal(t, dlgLegal, res["project"])
	require.Equal(t, "pending", res["status"])
	require.Contains(t, res["next"], "`seam approve "+short+"`")

	// Asking again finds it; once approved, the live grant answers and
	// nothing is filed; a lock's project is still the lock's.
	out, _ = d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": dlgLegal})
	require.Contains(t, out, pending[0].ID)
	facts, err := d.svc.Facts(context.Background(), pending[0].ID)
	require.NoError(t, err)
	grant, err := d.grants.Approve(context.Background(), approval.Decision{ID: pending[0].ID, Duration: approval.For4h,
		Digest: facts.Digest, Proof: approval.Proof{Kind: approval.ProofDaemon}})
	require.NoError(t, err)
	out, _ = d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": dlgLegal})
	res = map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Equal(t, "approved", res["status"])
	require.Equal(t, "grant", res["kind"])
	require.Equal(t, lkOps, res["from"])
	require.Equal(t, pending[0].ID, res["id"])
	require.Equal(t, grant.ExpiresAt.UTC().Format(time.RFC3339), res["open_until"])
	require.True(t, strings.HasPrefix(res["next"].(string), "project "+dlgLegal+" is already readable from project "+lkOps+" until "), res["next"])
	require.Empty(t, d.pending(), "the live grant answers: nothing filed")
	out, _ = d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin})
	require.Contains(t, out, `"kind":"request"`)
	require.Len(t, d.pending(), 1)
	require.Contains(t, d.refused(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkOps}),
		"that project is neither locked nor confidential: nothing to request")

	// Off: the same project is not locked, and nothing is filed.
	off := newDialogRig(t, dialogOpts{wait: dlgLong})
	ctx := context.Background()
	_, err = store.EnsureProject(ctx, off.db, dlgLegal, dlgLegal)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, off.db, dlgLegal, core.IsolationConfidential))
	off.session("cc/ops", lkOps)
	offAgent := off.dialOwner(off.agentSocket(), nil, nil)
	require.Equal(t, "access_request: that project is not locked: nothing to request",
		off.refused(offAgent, mcpserver.AccessRequestTool, map[string]any{"project": dlgLegal}))
	require.Empty(t, off.pending())
}
