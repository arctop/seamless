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
			Passkeys: v, Origins: []string{dlgOrigin}, UserName: "owner"})
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

// enrolTouchID enrols a key with a one-time code, as `seamlessd approvers
// enroll-code` and the helper's enrolment do.
func (d *dialogRig) enrolTouchID() touchKey {
	d.t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(d.t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(d.t, err)
	code, _, err := store.MintEnrolCode(ctx, d.db, store.ApproverTouchID, d.clock())
	require.NoError(d.t, err)
	cred, err := d.svc.EnrolTouchID(ctx, "this Mac", code, pub, sign(d.t, key, approval.TouchIDEnrolMessage(code, pub)))
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
	key := d.enrolTouchID()
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
	require.True(t, strings.HasPrefix(msg, "Seamless access request "+short+"\n"), msg)
	require.Contains(t, msg, "Project: "+lkFin+" (confidential, locked)")
	require.Contains(t, msg, "Session: cc/fin (")
	require.Contains(t, msg, "Working directory: /work/"+lkFin)
	require.Contains(t, msg, "Request: "+short+" ("+a.ID+"), request")
	require.Contains(t, msg, "the session works outside "+lkFin+"'s repository")
	require.Less(t, strings.Index(msg, "Warnings:"), strings.Index(msg, "Project:"), "warnings come first")
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
	d.enrolTouchID()
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
	require.True(t, strings.HasPrefix(ls[1], "Remedy: access request "+short+" is waiting"), ls[1])
	require.Contains(t, ls[1], "The approval dialog stopped waiting at ")
	require.True(t, strings.HasSuffix(ls[1], "without a decision; access request "+short+
		" stays pending for the owner: `seam approve "+short+"` in their own terminal, or the console."), ls[1])
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
			d.enrolTouchID()
			o := newOwner(answerWith(c.action, nil))
			agent := d.dialOwner(d.agentSocket(), o, nil)

			text := d.refused(agent, "tasks_list", nil)
			require.Len(t, o.dialogs(), 1)
			pending := d.pending()
			require.Len(t, pending, 1, "declining denies nothing: an agent's connection may not deny")
			short := approval.ShortID(pending[0].ID)
			require.Contains(t, text, " The approval dialog was "+c.word+"; access request "+short+" stays pending for the owner")
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
	d.enrolTouchID()
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
		if strings.Contains(text, "The approval dialog was declined") {
			break
		}
	}
	require.Contains(t, text, "The approval dialog was declined", "the cancelled call's dialog was released")
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
		d.enrolTouchID()
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
		d.enrolPasskey()
		o := newOwner(accepting(approval.ForSession, approval.ProofPasskey))
		plainRefusal(t, d.refused(d.dialOwner(d.agentSocket(), o, nil), "tasks_list", nil))
		require.Zero(t, d.tap.count())
	})
	t.Run("the TCP key", func(t *testing.T) {
		d := newDialogRig(t, dialogOpts{wait: dlgLong})
		d.session("cc/fin", lkFin)
		d.enrolTouchID()
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
	d.enrolTouchID()
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
	require.Contains(t, text, "The approval dialog stopped waiting at ")
	require.Empty(t, d.liveLeases(), "an answer is not a proof")
	require.Len(t, d.pending(), 1)
}

// enrolPasskey enrols a virtual authenticator as the owner's passkey.
func (d *dialogRig) enrolPasskey() *passkeytest.Authenticator {
	d.t.Helper()
	ctx := context.Background()
	a, err := passkeytest.New(true)
	require.NoError(d.t, err)
	code, _, err := store.MintEnrolCode(ctx, d.db, store.ApproverPasskey, d.clock())
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

// provePasskey approves request id with the authenticator's assertion over
// the request's own challenge, as the passkey page does.
func (d *dialogRig) provePasskey(a *passkeytest.Authenticator, id, duration string) error {
	ctx := context.Background()
	opts, err := d.svc.AssertionOptions(ctx, id)
	if err != nil {
		return err
	}
	resp, err := a.Get(opts, dlgOrigin)
	if err != nil {
		return err
	}
	facts, err := d.svc.Facts(ctx, id)
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
// offers both and the owner picks the passkey; without, the page is the
// dialog.
func TestDialog_PasskeyPage(t *testing.T) {
	for _, withTouchID := range []bool{false, true} {
		t.Run(fmt.Sprintf("touch id enrolled %v", withTouchID), func(t *testing.T) {
			d := newDialogRig(t, dialogOpts{wait: dlgLong, passkeys: true})
			d.seed()
			d.session("cc/fin", lkFin)
			auth := d.enrolPasskey()
			if withTouchID {
				d.enrolTouchID()
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

			proved := make(chan error, 1)
			var page opened
			go func() {
				select {
				case page = <-pages:
					proved <- d.provePasskey(auth, path.Base(page.url), approval.For4h)
				case <-time.After(20 * time.Second):
					proved <- fmt.Errorf("no passkey page")
				}
			}()
			text := d.refused(agent, "tasks_list", nil)
			require.NoError(t, within(t, proved, "the passkey proof"))

			id := path.Base(page.url)
			require.Equal(t, d.svc.Page(id), page.url, "the request's own passkey page")
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
			require.Empty(t, o.asks, "no Touch ID ask")

			leases := d.liveLeases()
			require.Len(t, leases, 1)
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
	key := d.enrolTouchID()
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
	d.enrolTouchID()
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
