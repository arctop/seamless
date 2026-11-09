package main

// The approval proofs and the anchor rule, end to end through the daemon's
// real routes (step 10). An approval is a lease on the requesting agent's
// process tree; three paths reach the one approve core (approval.Approve) --
// the daemon account's commit behind `seam approve`, a passkey assertion on
// the console's page, a Touch ID signature posted on the socket after the
// in-session dialog -- and nothing else grants. The anchor rows hold the rule
// from 4dac68e2: no request names a shell, multiplexer, terminal or login
// process, on any surface, and the approve core never approves a stored one.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

func TestPeerAuthMatrix_Proofs(t *testing.T) {
	ctx := context.Background()

	// --- Each proof grants: a lease on exactly the asking agent's tree, for
	// the duration approved; the tree then reads the locked project while a
	// sibling tree stays locked.

	t.Run("grants/daemon-account commit (seamlessd approve)", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		d.plant()
		sess := d.session("cc/fin", pmLocked, pmAgent)
		d.session("cc/sib", pmLocked, pmSibling)
		agent, sibling := d.agent(nil, nil), d.sibling()

		a := d.file(agent)
		short := approval.ShortID(a.ID)
		require.Equal(t, pmAgent.Key(), a.ProcessKey, "anchored at the agent the session is stamped with")
		require.Equal(t, sess, a.SessionID)
		// seam approve reads the facts over the console with the key; the
		// challenge is for the socket only.
		one := d.readAccess(d.tcp.URL, pmKey, short)
		require.Equal(t, store.ApprovalPending, one.Status)
		require.Empty(t, one.Challenge, "the TCP key's read carries no challenge")
		require.Equal(t, "Claude Code 2.1.319", one.Request.Agent)

		var out bytes.Buffer
		require.NoError(t, approve(ctx, d.svc, &out, []string{short, "--for", approval.For1h, "--digest", one.Request.Digest}))
		require.True(t, strings.HasPrefix(out.String(), "Approved "+short+": Claude Code 2.1.319 (pid 200) may read and write "+pmLocked+" until "), out.String())
		row := d.row(a.ID)
		require.Equal(t, store.ApprovalApproved, row.Status)
		require.Equal(t, "daemon-account", row.DecidedBy)
		d.requireLease(pmAgent, sess, time.Hour)
		d.requireTreeOpen(agent, sibling)
	})

	t.Run("grants/passkey assertion on the console's passkey page", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		d.plant()
		sess := d.session("cc/fin", pmLocked, pmAgent)
		d.session("cc/sib", pmLocked, pmSibling)
		auth := d.enrollPasskey()
		agent, sibling := d.agent(nil, nil), d.sibling()

		text := d.refused(agent, "tasks_list", map[string]any{"project": pmLocked})
		short := pmShortRe.FindString(text)
		require.NotEmpty(t, short, text)
		a, err := d.svc.Resolve(ctx, short)
		require.NoError(t, err)
		page := pmOrigin + "/console/access/" + a.ID
		require.Contains(t, text, "with a passkey at "+page, "the remedy names the request's own page")

		one, assertion := d.assertion(auth, a.ID, approval.For4h)
		require.Equal(t, page, one.Page)
		status, body := d.passkeyPost(a.ID, assertion, approval.For4h, one.Request.Digest)
		require.Equal(t, http.StatusOK, status, body)
		var ok struct {
			Approved  string `json:"approved"`
			Project   string `json:"project"`
			ExpiresAt string `json:"expires_at"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &ok))
		require.Equal(t, short, ok.Approved)
		require.Equal(t, pmLocked, ok.Project)
		row := d.row(a.ID)
		require.Equal(t, store.ApprovalApproved, row.Status)
		creds, err := store.ApproverCredentials(ctx, d.db, store.ApproverPasskey)
		require.NoError(t, err)
		require.Len(t, creds, 1)
		require.Equal(t, creds[0].ID, row.DecidedBy, "decided by the passkey that signed")
		d.requireLease(pmAgent, sess, 4*time.Hour)
		d.requireTreeOpen(agent, sibling)
	})

	t.Run("grants/touch id signature on the socket, from the in-session dialog", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		d.plant()
		sess := d.session("cc/fin", pmLocked, pmAgent)
		d.session("cc/sib", pmLocked, pmSibling)
		helper := d.socket(pmHelper)
		key := d.enrollTouchID()
		o := newPMOwner(pmAccepting(approval.ForSession, approval.ProofTouchID))
		agent, sibling := d.agent(o, nil), d.sibling()

		// The bridge's half: the ask arrives on the call's own reply, and the
		// helper -- its own process, on its own socket connection -- reads the
		// request, signs the decision and posts it.
		done := make(chan error, 1)
		var ask core.ApproverLaunch
		go func() {
			select {
			case got := <-o.asks:
				ask = got
				done <- pmHelperApprove(helper, key, got.RequestID, got.Duration)
			case <-time.After(20 * time.Second):
				done <- context.DeadlineExceeded
			}
		}()
		text := d.refused(agent, "tasks_list", map[string]any{"project": pmLocked})
		require.NoError(t, pmWithin(t, done, "the helper's approval"))
		require.Equal(t, approval.ForSession, ask.Duration, "the duration chosen in the dialog")
		row := d.row(ask.RequestID)
		require.Equal(t, store.ApprovalApproved, row.Status)
		require.Equal(t, key.id, row.DecidedBy, "decided by the Touch ID key")
		lease := d.requireLease(pmAgent, sess, 8*time.Hour) // until the session ends: the cap
		ls := pmLines(text)
		require.Len(t, ls, 2, text)
		require.Equal(t, "tasks_list: the owner approved access request "+approval.ShortID(row.ID)+": project "+pmLocked+
			" is open to this agent until "+access.Clock(lease.ExpiresAt)+".", ls[0])
		require.Equal(t, "Remedy: this call ran before the approval and did nothing -- retry it now.", ls[1])
		shown := o.dialogs()
		require.Len(t, shown, 1)
		require.True(t, strings.HasPrefix(shown[0].Params.Message, "No warnings. Claude Code 2.1.319 asks for read-write access to "+pmLocked+
			".\nUntil its session ends (8 hours at most), unless you pick less below."), shown[0].Params.Message)
		d.requireTreeOpen(agent, sibling)

		// "Until the session ends" means it: session_end revokes the lease.
		text, isErr := paCall(t, agent, "session_end", map[string]any{"findings": "closed the quarter", "session": sess})
		require.False(t, isErr, text)
		require.Empty(t, d.leases(), "session_end revoked the session's lease")
		// (A client without the dialog: this one's would wait for a second
		// approval nobody gives.)
		text = d.refused(d.agent(nil, nil), "tasks_list", map[string]any{"project": pmLocked})
		require.Contains(t, text, "project "+pmLocked+" is locked")
		require.NotContains(t, text, pmSecret)
	})

	// --- Each of these grants nothing.

	t.Run("nothing/an elicitation acceptance without a proof", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{dialogWait: 700 * time.Millisecond})
		d.session("cc/fin", pmLocked, pmAgent)
		d.enrollTouchID()
		d.enrollPasskey()

		// Touch ID chosen: the ask goes out, nobody signs.
		o := newPMOwner(pmAccepting(approval.For4h, approval.ProofTouchID))
		agent := d.agent(o, nil)
		text := d.refused(agent, "tasks_list", map[string]any{"project": pmLocked})
		ask := pmWithin(t, o.asks, "the approver ask")
		short := approval.ShortID(ask.RequestID)
		page := pmOrigin + "/console/access/" + ask.RequestID
		d.nothing("an accepted dialog", map[string]string{ask.RequestID: store.ApprovalPending})
		ls := pmLines(text)
		require.Len(t, ls, 2, text)
		require.Equal(t, "tasks_list: project "+pmLocked+" is locked: even the owner's own agents need the owner's approval, "+
			"and this session holds none.", ls[0])
		require.True(t, strings.HasPrefix(ls[1], "Remedy: the approval dialog stopped waiting at "), ls[1])
		require.NotContains(t, ls[1], " is waiting -- ", "one remedy, as the dialog left it")
		require.True(t, strings.HasSuffix(ls[1], " without a decision; access request "+short+" stays pending for the owner: `seam approve "+
			short+"` in their own terminal, a passkey at "+page+", or Touch ID in the next approval dialog; retry once it is "+
			"approved, and keep to other projects until then."), ls[1])

		// access_request, accepted the same way: still pending.
		text, isErr := paCall(t, agent, "access_request", map[string]any{"project": pmLocked, "reason": "close the quarter"})
		require.False(t, isErr, text)
		var res struct {
			ShortID string `json:"short_id"`
			Status  string `json:"status"`
			Dialog  string `json:"dialog"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &res))
		require.Equal(t, short, res.ShortID, "the same request, found again")
		require.Equal(t, store.ApprovalPending, res.Status)
		require.Equal(t, "accepted", res.Dialog)
		d.nothing("an accepted access_request dialog", map[string]string{ask.RequestID: store.ApprovalPending})

		// The passkey chosen: the page is opened (a URL elicitation, accepted),
		// and no assertion arrives.
		po := newPMOwner(pmAccepting(approval.For1h, approval.ProofPasskey))
		pagent := d.dial(d.socket(pmBridge, pmAgent, pmShell), pmAgent, po, pmURLCaps)
		text = d.refused(pagent, "tasks_list", map[string]any{"project": pmLocked})
		shown := po.dialogs()
		require.Len(t, shown, 2, "the form, then the page")
		require.Equal(t, mcpgo.ElicitationModeURL, shown[1].Params.Mode)
		require.Equal(t, page+"?for=1h", shown[1].Params.URL, "the page for the duration the form chose")
		require.Contains(t, text, "\nRemedy: the approval dialog stopped waiting at ")
		require.Empty(t, po.asks, "no Touch ID ask for a passkey")
		d.nothing("an opened passkey page", map[string]string{ask.RequestID: store.ApprovalPending})
	})

	t.Run("nothing/a forged proof", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		d.session("cc/fin", pmLocked, pmAgent)
		helper := d.socket(pmHelper)
		key := d.enrollTouchID()
		auth := d.enrollPasskey()
		a := d.file(d.agent(nil, nil))
		one := d.readAccess(helper, "", a.ID)
		require.NotEmpty(t, one.Challenge, "the helper's read on the socket carries the challenge")
		pending := map[string]string{a.ID: store.ApprovalPending}

		// Touch ID: the right message signed by a key never enrolled.
		wrong, err := pmTouchIDSignature(pmNewKey(t), one, approval.For1h)
		require.NoError(t, err)
		status, body := d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, wrong, approval.For1h, one.Request.Digest))
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, pmTouchIDBad, pmErrorText(t, body))
		d.nothing("a wrong key's signature", pending)

		// Touch ID: the enrolled key's signature with one bit flipped.
		good, err := pmTouchIDSignature(key.key, one, approval.For1h)
		require.NoError(t, err)
		for _, at := range []int{len(good) - 1, len(good) / 2} {
			flipped := bytes.Clone(good)
			flipped[at] ^= 0x01
			status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, flipped, approval.For1h, one.Request.Digest))
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Equal(t, pmTouchIDBad, pmErrorText(t, body))
		}
		// A credential that is no Touch ID key: unknown, and the passkey's.
		creds, err := store.ApproverCredentials(ctx, d.db, store.ApproverPasskey)
		require.NoError(t, err)
		require.Len(t, creds, 1)
		for _, cred := range []string{"01NOSUCHKEY", creds[0].ID} {
			status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(cred, good, approval.For1h, one.Request.Digest))
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Equal(t, pmTouchIDBad, pmErrorText(t, body))
		}
		d.nothing("a bit-flipped signature, an unknown key", pending)

		// Off the socket, even a good signature is refused unread.
		status, body = d.do(d.tcp.URL, pmKey, http.MethodPost, "/console/access/"+a.ID+"/approve",
			pmTouchIDForm(key.id, good, approval.For1h, one.Request.Digest), nil)
		require.Equal(t, http.StatusForbidden, status, body)
		require.Contains(t, pmErrorText(t, body), "a Touch ID approval is accepted on the peer_auth socket only")
		d.nothing("a Touch ID proof over TCP", pending)

		// Passkey: an authenticator never enrolled answers the page's options
		// (ignoring their allow list, as a forger would).
		page, _ := d.assertion(auth, a.ID, approval.For1h)
		var opts map[string]map[string]any
		require.NoError(t, json.Unmarshal(page.PasskeyOptions, &opts))
		delete(opts["publicKey"], "allowCredentials")
		open, err := json.Marshal(opts)
		require.NoError(t, err)
		stranger := pmNewAuthenticator(t)
		forged, err := stranger.Get(open, pmOrigin)
		require.NoError(t, err)
		status, body = d.passkeyPost(a.ID, forged, approval.For1h, page.Request.Digest)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, pmPasskeyBad, pmErrorText(t, body))

		// Passkey: the enrolled passkey's assertion with its signature bit-flipped.
		_, assertion := d.assertion(auth, a.ID, approval.For1h)
		status, body = d.passkeyPost(a.ID, pmFlipPasskeySignature(t, assertion), approval.For1h, page.Request.Digest)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, pmPasskeyBad, pmErrorText(t, body))
		// No proof at all, and a proof the routes do not construct.
		status, body = d.passkeyPost(a.ID, nil, approval.For1h, page.Request.Digest)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, pmPasskeyBad, pmErrorText(t, body))
		for _, kind := range []string{"daemon", ""} {
			status, body = d.do(helper, "", http.MethodPost, "/console/access/"+a.ID+"/approve", url.Values{
				"proof": {kind}, "duration": {approval.For1h}, "digest": {page.Request.Digest},
			}, nil)
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Contains(t, pmErrorText(t, body), "invalid proof \""+kind+"\": the valid value is touchid")
		}
		d.nothing("a forged passkey assertion, no proof, a claimed daemon proof", pending)
	})

	t.Run("nothing/a replay", func(t *testing.T) {
		t.Run("touch id", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			sess := d.session("cc/fin", pmLocked, pmAgent)
			d.session("cc/sib", pmLocked, pmSibling)
			helper := d.socket(pmHelper)
			key := d.enrollTouchID()
			a := d.file(d.agent(nil, nil))
			b := d.file(d.sibling())

			// A valid proof, submitted twice: the second grants nothing.
			one := d.readAccess(helper, "", a.ID)
			sig, err := pmTouchIDSignature(key.key, one, approval.For1h)
			require.NoError(t, err)
			form := pmTouchIDForm(key.id, sig, approval.For1h, one.Request.Digest)
			status, body := d.touchIDPost(helper, a.ID, form)
			require.Equal(t, http.StatusOK, status, body)
			first := d.requireLease(pmAgent, sess, time.Hour)
			status, body = d.touchIDPost(helper, a.ID, form)
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmDecided, pmErrorText(t, body))
			require.Equal(t, first.ID, d.requireLease(pmAgent, sess, time.Hour).ID, "the replay neither added nor replaced a lease")

			// A proof captured for B, replayed once B is decided (denied).
			oneB := d.readAccess(helper, "", b.ID)
			sigB, err := pmTouchIDSignature(key.key, oneB, approval.For4h)
			require.NoError(t, err)
			status, body = d.do(d.tcp.URL, pmKey, http.MethodPost, "/console/access/"+b.ID+"/deny", url.Values{}, nil)
			require.Equal(t, http.StatusOK, status, body)
			status, body = d.touchIDPost(helper, b.ID, pmTouchIDForm(key.id, sigB, approval.For4h, oneB.Request.Digest))
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmDecided, pmErrorText(t, body))
			require.Equal(t, store.ApprovalDenied, d.row(b.ID).Status)
			require.Equal(t, first.ID, d.requireLease(pmAgent, sess, time.Hour).ID, "still only A's lease")
		})
		t.Run("passkey", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			sess := d.session("cc/fin", pmLocked, pmAgent)
			d.session("cc/sib", pmLocked, pmSibling)
			auth := d.enrollPasskey()
			a := d.file(d.agent(nil, nil))
			b := d.file(d.sibling())

			one, assertion := d.assertion(auth, a.ID, approval.For1h)
			status, body := d.passkeyPost(a.ID, assertion, approval.For1h, one.Request.Digest)
			require.Equal(t, http.StatusOK, status, body)
			first := d.requireLease(pmAgent, sess, time.Hour)
			status, body = d.passkeyPost(a.ID, assertion, approval.For1h, one.Request.Digest)
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmDecided, pmErrorText(t, body))
			require.Equal(t, first.ID, d.requireLease(pmAgent, sess, time.Hour).ID)

			oneB, assertionB := d.assertion(auth, b.ID, approval.For1h)
			status, body = d.do(d.tcp.URL, pmKey, http.MethodPost, "/console/access/"+b.ID+"/deny", url.Values{}, nil)
			require.Equal(t, http.StatusOK, status, body)
			status, body = d.passkeyPost(b.ID, assertionB, approval.For1h, oneB.Request.Digest)
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmDecided, pmErrorText(t, body))
			require.Equal(t, store.ApprovalDenied, d.row(b.ID).Status)
			require.Equal(t, first.ID, d.requireLease(pmAgent, sess, time.Hour).ID)
		})
		t.Run("daemon-account commit", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			sess := d.session("cc/fin", pmLocked, pmAgent)
			a := d.file(d.agent(nil, nil))
			short := approval.ShortID(a.ID)
			digest := d.readAccess(d.tcp.URL, pmKey, short).Request.Digest
			var out bytes.Buffer
			require.NoError(t, approve(ctx, d.svc, &out, []string{a.ID, "--for", approval.For1h, "--digest", digest}))
			first := d.requireLease(pmAgent, sess, time.Hour)
			err := approve(ctx, d.svc, &out, []string{a.ID, "--for", approval.For4h, "--digest", digest})
			require.ErrorIs(t, err, store.ErrApprovalNotPending, "single use")
			require.Equal(t, first.ID, d.requireLease(pmAgent, sess, time.Hour).ID)
		})
	})

	t.Run("nothing/a proof over another request", func(t *testing.T) {
		t.Run("touch id", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			d.session("cc/fin", pmLocked, pmAgent)
			sib := d.session("cc/sib", pmLocked, pmSibling)
			helper := d.socket(pmHelper)
			key := d.enrollTouchID()
			a := d.file(d.agent(nil, nil))
			b := d.file(d.sibling())
			oneA, oneB := d.readAccess(helper, "", a.ID), d.readAccess(helper, "", b.ID)
			sigB, err := pmTouchIDSignature(key.key, oneB, approval.For1h)
			require.NoError(t, err)

			// Signed for B, submitted for A: with A's facts, the signature is
			// over the wrong request; with B's, the facts are not A's.
			status, body := d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sigB, approval.For1h, oneA.Request.Digest))
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Equal(t, pmTouchIDBad, pmErrorText(t, body))
			status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sigB, approval.For1h, oneB.Request.Digest))
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmTouchIDFactsChanged, pmErrorText(t, body))
			d.nothing("B's signature on A", map[string]string{a.ID: store.ApprovalPending, b.ID: store.ApprovalPending})

			// On its own request it is a proof: B's tree is leased, A's is not.
			status, body = d.touchIDPost(helper, b.ID, pmTouchIDForm(key.id, sigB, approval.For1h, oneB.Request.Digest))
			require.Equal(t, http.StatusOK, status, body)
			d.requireLease(pmSibling, sib, time.Hour)
			require.Equal(t, store.ApprovalPending, d.row(a.ID).Status)
		})
		t.Run("passkey", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			d.session("cc/fin", pmLocked, pmAgent)
			sib := d.session("cc/sib", pmLocked, pmSibling)
			auth := d.enrollPasskey()
			a := d.file(d.agent(nil, nil))
			b := d.file(d.sibling())
			oneA := d.readAccess(d.tcp.URL, pmKey, a.ID)
			oneB, assertionB := d.assertion(auth, b.ID, approval.For1h)

			status, body := d.passkeyPost(a.ID, assertionB, approval.For1h, oneA.Request.Digest)
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Equal(t, pmPasskeyBad, pmErrorText(t, body))
			d.nothing("B's assertion on A", map[string]string{a.ID: store.ApprovalPending, b.ID: store.ApprovalPending})

			status, body = d.passkeyPost(b.ID, assertionB, approval.For1h, oneB.Request.Digest)
			require.Equal(t, http.StatusOK, status, body)
			d.requireLease(pmSibling, sib, time.Hour)
			require.Equal(t, store.ApprovalPending, d.row(a.ID).Status)
		})
	})

	t.Run("nothing/a changed duration", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		sess := d.session("cc/fin", pmLocked, pmAgent)
		helper := d.socket(pmHelper)
		key := d.enrollTouchID()
		a := d.file(d.agent(nil, nil))
		one := d.readAccess(helper, "", a.ID)
		sig, err := pmTouchIDSignature(key.key, one, approval.For1h)
		require.NoError(t, err)
		for _, changed := range []string{approval.For4h, approval.ForSession} {
			status, body := d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, changed, one.Request.Digest))
			require.Equal(t, http.StatusBadRequest, status, "signed for 1h, sent as %s: %s", changed, body)
			require.Equal(t, pmTouchIDBad, pmErrorText(t, body))
		}
		status, body := d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, "8d", one.Request.Digest))
		require.Equal(t, http.StatusConflict, status, body)
		require.Equal(t, "choose a duration: until the session ends, 1 hour or 4 hours", pmErrorText(t, body))
		d.nothing("a changed duration", map[string]string{a.ID: store.ApprovalPending})
		// The duration it was signed for is the one that approves.
		status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, approval.For1h, one.Request.Digest))
		require.Equal(t, http.StatusOK, status, body)
		d.requireLease(pmAgent, sess, time.Hour)
	})

	t.Run("nothing/a changed facts digest", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		sess := d.session("cc/fin", pmLocked, pmAgent)
		helper := d.socket(pmHelper)
		key := d.enrollTouchID()
		auth := d.enrollPasskey()
		a := d.file(d.agent(nil, nil))
		short := approval.ShortID(a.ID)
		pending := map[string]string{a.ID: store.ApprovalPending}
		one := d.readAccess(helper, "", a.ID)
		sig, err := pmTouchIDSignature(key.key, one, approval.For1h)
		require.NoError(t, err)
		_, assertion := d.assertion(auth, a.ID, approval.For1h)
		garbage := strings.Repeat("0", len(one.Request.Digest))

		// A digest that is not the facts': every path refuses it.
		status, body := d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, approval.For1h, garbage))
		require.Equal(t, http.StatusConflict, status, body)
		require.Equal(t, pmTouchIDFactsChanged, pmErrorText(t, body))
		status, body = d.passkeyPost(a.ID, assertion, approval.For1h, garbage)
		require.Equal(t, http.StatusConflict, status, body)
		require.Equal(t, pmFactsChanged, pmErrorText(t, body))
		var out bytes.Buffer
		err = approve(ctx, d.svc, &out, []string{short, "--for", approval.For1h, "--digest", garbage})
		require.EqualError(t, err, "the facts of "+short+" changed since they were shown: run seam approve again and look at them first")
		d.nothing("a digest that is not the facts'", pending)

		// The facts change after the owner looked (the session read outside
		// text): the digest that was shown, and signed, no longer approves.
		_, err = store.SetSessionTaint(ctx, d.db, sess, "web", "tool", time.Now())
		require.NoError(t, err)
		fresh := d.readAccess(helper, "", a.ID)
		require.NotEqual(t, one.Request.Digest, fresh.Request.Digest)
		require.Equal(t, approval.WarnTainted, fresh.Request.Warnings[0].Code)
		status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, approval.For1h, one.Request.Digest))
		require.Equal(t, http.StatusConflict, status, body)
		require.Equal(t, pmTouchIDFactsChanged, pmErrorText(t, body))
		status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, approval.For1h, fresh.Request.Digest))
		require.Equal(t, http.StatusBadRequest, status, "the fresh digest, under a signature over the stale one: %s", body)
		require.Equal(t, pmTouchIDBad, pmErrorText(t, body))
		status, body = d.passkeyPost(a.ID, assertion, approval.For1h, one.Request.Digest)
		require.Equal(t, http.StatusConflict, status, body)
		require.Equal(t, pmFactsChanged, pmErrorText(t, body))
		err = approve(ctx, d.svc, &out, []string{short, "--for", approval.For1h, "--digest", one.Request.Digest})
		require.EqualError(t, err, "the facts of "+short+" changed since they were shown: run seam approve again and look at them first")
		require.ErrorContains(t, approve(ctx, d.svc, &out, []string{short, "--for", approval.For1h}), "an approval needs --for and --digest")
		require.Empty(t, out.String(), "nothing was approved")
		d.nothing("the facts changed since they were shown", pending)
	})

	t.Run("nothing/an expired request", func(t *testing.T) {
		late := approval.RequestTTL + time.Minute
		t.Run("touch id", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			d.session("cc/fin", pmLocked, pmAgent)
			helper := d.socket(pmHelper)
			key := d.enrollTouchID()
			a := d.file(d.agent(nil, nil))
			one := d.readAccess(helper, "", a.ID)
			sig, err := pmTouchIDSignature(key.key, one, approval.For1h)
			require.NoError(t, err)
			d.runAhead(late)
			status, body := d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, approval.For1h, one.Request.Digest))
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmExpired, pmErrorText(t, body))
			d.nothing("an expired request", map[string]string{a.ID: store.ApprovalExpired})
		})
		t.Run("passkey", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			d.session("cc/fin", pmLocked, pmAgent)
			auth := d.enrollPasskey()
			a := d.file(d.agent(nil, nil))
			one, assertion := d.assertion(auth, a.ID, approval.For1h)
			d.runAhead(late)
			status, body := d.passkeyPost(a.ID, assertion, approval.For1h, one.Request.Digest)
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmExpired, pmErrorText(t, body))
			d.nothing("an expired request", map[string]string{a.ID: store.ApprovalExpired})
		})
		t.Run("daemon-account commit", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			d.session("cc/fin", pmLocked, pmAgent)
			a := d.file(d.agent(nil, nil))
			short := approval.ShortID(a.ID)
			digest := d.readAccess(d.tcp.URL, pmKey, short).Request.Digest
			d.runAhead(late)
			var out bytes.Buffer
			// seam approve names the short id, which still names the request
			// after it stopped waiting (approval.ShortIDMemory): the core
			// expires it. Asked again by the full id, it was expired already.
			require.ErrorIs(t, approve(ctx, d.svc, &out, []string{short, "--for", approval.For1h, "--digest", digest}), store.ErrApprovalExpired)
			again := approve(ctx, d.svc, &out, []string{a.ID, "--for", approval.For1h, "--digest", digest})
			require.ErrorIs(t, again, store.ErrApprovalNotPending)
			require.ErrorContains(t, again, short+" expired at ", "the owner reads that it expired, by either id")
			require.Empty(t, out.String())
			d.nothing("an expired request", map[string]string{a.ID: store.ApprovalExpired})
		})
	})

	// The passkey proof signs its duration, as Touch ID's does (decision
	// D1): the page issues its options for the duration chosen (?for=), and
	// the assertion over them approves that duration only. Sent with another
	// one it approves nothing, refused as a changed decision.
	t.Run("contract/the passkey assertion signs its duration", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{})
		sess := d.session("cc/fin", pmLocked, pmAgent)
		auth := d.enrollPasskey()
		a := d.file(d.agent(nil, nil))
		require.Empty(t, d.readAccess(d.tcp.URL, pmKey, a.ID).PasskeyOptions, "no options before a duration is chosen")
		one, assertion := d.assertion(auth, a.ID, approval.For1h)
		for _, changed := range []string{approval.For4h, approval.ForSession} {
			status, body := d.passkeyPost(a.ID, assertion, changed, one.Request.Digest)
			require.Equal(t, http.StatusConflict, status, "signed for 1h, sent as %s: %s", changed, body)
			require.Equal(t, pmPasskeyRebound, pmErrorText(t, body))
		}
		d.nothing("a passkey assertion sent with another duration", map[string]string{a.ID: store.ApprovalPending})
		status, body := d.passkeyPost(a.ID, assertion, approval.For1h, one.Request.Digest)
		require.Equal(t, http.StatusOK, status, body)
		d.requireLease(pmAgent, sess, time.Hour)
	})
}

func pmFlipPasskeySignature(t *testing.T, assertion []byte) []byte {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(assertion, &resp))
	inner := resp["response"].(map[string]any)
	sig, err := base64.RawURLEncoding.DecodeString(inner["signature"].(string))
	require.NoError(t, err)
	sig[len(sig)-1] ^= 0x01
	inner["signature"] = base64.RawURLEncoding.EncodeToString(sig)
	out, err := json.Marshal(resp)
	require.NoError(t, err)
	return out
}

// Rows 1-4's tokens: a seam run by hand in a tmux pane, whose server was
// reparented to pid 1 (the walk stops below pid 1, so tmux ends the chain).
var (
	pmSeamProc = access.ProcessToken{PID: 900, Version: 9000, Boot: pmBoot}
	pmTmuxProc = access.ProcessToken{PID: 700, Version: 7000, Boot: pmBoot}
)

// pmHop is one process of a row-8 chain: its token and the program the
// kernel names for it.
type pmHop struct {
	tok  access.ProcessToken
	prog string
}

// pmBroadRule is the anchor refusal's first line for a broad ancestor
// (approval.AnchorError.Rule), what being "tmux, a terminal multiplexer".
func pmBroadRule(what string) string {
	return "Seamless files no access request for this process: it is " + what + ", not an agent, and an approval " +
		"would open the project to every program it started, other agent sessions included"
}

// pmUnstartedRule is the same line for a program no agent started.
func pmUnstartedRule(base string) string {
	return "Seamless files no access request for this process: it is " + base + ", which no agent session " +
		"started, and an approval would open the project to every program it started"
}

// pmBroadApprove is the console's 409 for a stored broad anchor.
const pmBroadApprove = "this request names a process that is not an agent (a shell, terminal, multiplexer or login process): " +
	"approving it would open the project to every program it started, so it is never approved; deny it"

func TestPeerAuthMatrix_Anchors(t *testing.T) {
	ctx := context.Background()
	tmuxNames := map[access.ProcessToken]string{pmSeamProc: pmSeamPath, pmTmuxProc: pmTmuxPath, pmHelper: pmHelperPath}
	rule := pmBroadRule("tmux, a terminal multiplexer")

	// Rows 1-4: the owner's socket, chain [seam, tmux (ppid 1)], the
	// agent-process header naming tmux. Every filing surface answers the two
	// lines and files nothing.
	t.Run("1-4/a tmux pane files nothing on any surface", func(t *testing.T) {
		d := newPMDaemon(t, pmOpts{programs: tmuxNames})
		sock := d.socket(pmSeamProc, pmTmuxProc)
		cli := d.dial(sock, pmTmuxProc, nil, nil)
		none := func(row string) {
			t.Helper()
			require.Zero(t, d.count(`SELECT COUNT(*) FROM access_approvals`), "%s: no request row", row)
			require.Zero(t, d.count(`SELECT COUNT(*) FROM access_leases`), "%s: no lease", row)
		}

		// 1. access_request: the anchor refusal, two lines.
		text, isErr := paCall(t, cli, "access_request", map[string]any{"project": pmLocked, "reason": "close the quarter"})
		require.True(t, isErr, text)
		require.Equal(t, []string{"access_request: " + rule + ".", pmAnchorRemedy}, pmLines(text))
		none("access_request")

		// 2. A call refused at the lock: the unfiled lock refusal.
		text, isErr = paCall(t, cli, "tasks_list", map[string]any{"project": pmLocked})
		require.True(t, isErr, text)
		require.Equal(t, []string{"tasks_list: project " + pmLocked + " is locked: even the owner's own agents need the owner's " +
			"approval, and " + rule + ".", pmAnchorRemedy}, pmLines(text))
		require.Empty(t, pmShortRe.FindString(text), "no request id: none was filed")
		none("tasks_list")

		// 3. seam access request: POST /console/access/request.
		status, body := d.do(sock, "", http.MethodPost, "/console/access/request",
			url.Values{"project": {pmLocked}, "reason": {"close the quarter"}},
			map[string]string{mcp.AgentProcessHeader: pmTmuxProc.AgentID(), mcp.HostHeader: pmHost})
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Equal(t, rule+".\n"+pmAnchorRemedy, pmErrorText(t, body))
		none("POST /console/access/request")

		// 4. The SessionStart hook run in the locked project's repository.
		q := url.Values{"host": {pmHost}, "agent_process": {pmTmuxProc.AgentID()}, "repo_root": {pmCWD}, "main_root": {pmCWD}}
		raw, err := json.Marshal(map[string]any{"session_id": "tmux-pane-1", "cwd": pmCWD, "source": "startup"})
		require.NoError(t, err)
		notice := pmHook(t, sock, "session-start?"+q.Encode(), raw)
		require.Contains(t, notice, "Seamless: project "+pmLocked+" is locked: even the owner's own agents need the owner's "+
			"approval, and "+rule+".\n"+pmAnchorRemedy+"\n")
		require.Empty(t, pmShortRe.FindString(notice), "no request id: none was filed")
		require.NotContains(t, notice, "sealed", "the lock is not called sealed")
		require.Contains(t, notice, "\nThis session is not recorded, and Seamless tools cannot read or write "+pmLocked+" from this process.\n")
		require.Zero(t, d.count(`SELECT COUNT(*) FROM sessions`), "SessionStart: refused before recording")
		none("SessionStart")
	})

	// Rows 6-7: rows an older daemon filed for a broad anchor, or for pid 1.
	// Every approval path refuses them, with a valid proof in hand.
	for _, c := range []struct {
		name    string
		pid     int32
		program string
		what    string
	}{
		{"6/a stored row whose program is tmux", 3698, pmTmuxPath, "tmux, a terminal multiplexer"},
		{"7/a stored row naming pid 1, whatever its name", 1, "/usr/local/bin/claude", "pid 1, the system's process manager"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			helper := d.socket(pmHelper)
			key := d.enrollTouchID()
			auth := d.enrollPasskey()
			tok := access.ProcessToken{PID: c.pid, Version: uint64(c.pid) * 10, Boot: pmBoot}
			a, err := store.CreateAccessApproval(ctx, d.db, store.AccessRequest{Project: pmLocked, Kind: store.AccessKindRequest,
				Lease: 8 * time.Hour, TTL: 10 * time.Minute, PeerUID: pmUID, PeerPID: c.pid, ProcessKey: tok.Key(), Program: c.program},
				time.Now())
			require.NoError(t, err)
			pending := map[string]string{a.ID: store.ApprovalPending}

			one, assertion := d.assertion(auth, a.ID, approval.For1h)
			require.NotEmpty(t, one.Request.Warnings)
			require.Equal(t, approval.WarnAnchorIsShell, one.Request.Warnings[0].Code, "the broadest reach comes first")
			require.Contains(t, one.Request.Warnings[0].Text, "the process asking is "+c.what+", not an agent")
			status, body := d.passkeyPost(a.ID, assertion, approval.For1h, one.Request.Digest)
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmBroadApprove, pmErrorText(t, body))
			d.nothing("a passkey on a broad anchor", pending)

			hone := d.readAccess(helper, "", a.ID)
			sig, err := pmTouchIDSignature(key.key, hone, approval.For1h)
			require.NoError(t, err)
			status, body = d.touchIDPost(helper, a.ID, pmTouchIDForm(key.id, sig, approval.For1h, hone.Request.Digest))
			require.Equal(t, http.StatusConflict, status, body)
			require.Equal(t, pmBroadApprove, pmErrorText(t, body))
			d.nothing("Touch ID on a broad anchor", pending)

			var out bytes.Buffer
			err = approve(ctx, d.svc, &out, []string{approval.ShortID(a.ID), "--for", approval.For1h, "--digest", one.Request.Digest})
			require.ErrorIs(t, err, approval.ErrBroadAnchor)
			require.ErrorContains(t, err, "it is "+c.what+", and approving it would open the project to every program it started")
			require.Empty(t, out.String())
			d.nothing("seamlessd approve on a broad anchor", pending)
		})
	}

	// Row 8: the chains a request comes from, the seam process first, and
	// the process agentproc names (the header). Broad or unstarted anchors
	// are refused; an agent, or a process inside an agent's tree, is filed.
	const (
		loginPath   = "/usr/bin/login"
		sshdPath    = "/usr/sbin/sshd"
		ghosttyPath = "/Applications/Ghostty.app/Contents/MacOS/ghostty"
		someterm    = "/opt/someterm/bin/someterm"
		pythonPath  = "/usr/bin/python3"
		nodePath    = "/opt/homebrew/bin/node"
		bashPath    = "/bin/bash"
		shPath      = "/bin/sh"
		disclaimer  = "/Applications/Claude.app/Contents/Helpers/disclaimer"
		claudeApp   = "/Applications/Claude.app/Contents/MacOS/Claude"
	)
	hop := func(pid int32, prog string) pmHop {
		return pmHop{access.ProcessToken{PID: pid, Version: uint64(pid) * 10, Boot: pmBoot}, prog}
	}
	seam := hop(900, pmSeamPath)
	for _, c := range []struct {
		name    string
		chain   []pmHop
		header  int
		stamped bool
		// refused is the rule's first line (without the tool prefix); "" files.
		refused string
		agent   string
	}{
		{"refused/Terminal -> login -> zsh -> seam", []pmHop{seam, hop(800, pmZshPath), hop(750, loginPath), hop(700, pmTerminalPath)}, 2, false,
			pmBroadRule("login, a login session"), ""},
		{"refused/sshd -> zsh -> seam", []pmHop{seam, hop(800, pmZshPath), hop(700, sshdPath)}, 2, false, pmBroadRule("sshd, a login session"), ""},
		{"refused/Ghostty -> zsh -> seam", []pmHop{seam, hop(800, pmZshPath), hop(700, ghosttyPath)}, 2, false, pmBroadRule("ghostty, a terminal"), ""},
		{"refused/an unlisted terminal -> zsh -> seam", []pmHop{seam, hop(800, pmZshPath), hop(700, someterm)}, 2, false, pmUnstartedRule("someterm"), ""},
		{"refused/node no session stamped", []pmHop{seam, hop(700, nodePath), hop(600, pmZshPath), hop(500, loginPath)}, 1, false, pmUnstartedRule("node"), ""},
		{"filed/claude -> zsh -> seam", []pmHop{seam, hop(800, pmZshPath), hop(700, pmClaudePath), hop(600, pmZshPath), hop(500, loginPath)}, 2, true, "",
			"Claude Code 2.1.319"},
		{"filed/codex -> bash -> seam", []pmHop{seam, hop(800, bashPath), hop(700, pmCodexPath), hop(600, pmZshPath)}, 2, true, "", "Codex"},
		{"filed/a script under claude", []pmHop{seam, hop(800, pythonPath), hop(750, pmZshPath), hop(700, pmClaudePath)}, 1, false, "", ""},
		{"filed/a stamped node (Claude Code installed with npm)", []pmHop{seam, hop(800, shPath), hop(700, nodePath), hop(600, pmZshPath), hop(500, loginPath)}, 2, true, "", ""},
		{"filed/the Claude app's per-server wrapper", []pmHop{seam, hop(700, disclaimer), hop(600, claudeApp)}, 1, false, "", "Claude app"},
	} {
		t.Run("8/"+c.name, func(t *testing.T) {
			names := map[access.ProcessToken]string{}
			chain := make([]access.ProcessToken, len(c.chain))
			for i, h := range c.chain {
				chain[i], names[h.tok] = h.tok, h.prog
			}
			d := newPMDaemon(t, pmOpts{programs: names})
			anchor := chain[c.header]
			if c.stamped {
				d.session("cc/anchor", pmOpen, anchor)
			}
			cli := d.dial(d.socket(chain...), anchor, nil, nil)
			text, isErr := paCall(t, cli, "access_request", map[string]any{"project": pmLocked})
			if c.refused != "" {
				require.True(t, isErr, text)
				require.Equal(t, []string{"access_request: " + c.refused + ".", pmAnchorRemedy}, pmLines(text))
				require.Zero(t, d.count(`SELECT COUNT(*) FROM access_approvals`), "nothing was filed")
				return
			}
			require.False(t, isErr, text)
			var res struct {
				ID     string `json:"id"`
				Kind   string `json:"kind"`
				Status string `json:"status"`
			}
			require.NoError(t, json.Unmarshal([]byte(text), &res))
			require.Equal(t, store.AccessKindRequest, res.Kind)
			require.Equal(t, store.ApprovalPending, res.Status)
			row := d.row(res.ID)
			require.Equal(t, anchor.Key(), row.ProcessKey, "anchored where the header points, inside the chain")
			require.Equal(t, c.chain[c.header].prog, row.Program, "the kernel's program, from the injected Programs")
			facts, err := d.svc.Facts(ctx, res.ID)
			require.NoError(t, err)
			require.Equal(t, c.agent, facts.Agent)
			for _, w := range facts.Warnings {
				require.NotEqual(t, approval.WarnAnchorIsShell, w.Code)
			}
		})
	}

	// Row 9: the remedy offers only the approval paths that exist -- the
	// passkey page only with a passkey enrolled, Touch ID only with a key
	// enrolled and a client that can show the form dialog that carries it;
	// the owner's terminal always.
	for _, c := range []struct {
		name             string
		passkey, touchID bool
	}{
		{"none", false, false}, {"passkey only", true, false}, {"touch id only", false, true}, {"both", true, true},
	} {
		t.Run("9/remedy with "+c.name+" enrolled", func(t *testing.T) {
			d := newPMDaemon(t, pmOpts{})
			d.session("cc/fin", pmLocked, pmAgent)
			if c.passkey {
				d.enrollPasskey()
			}
			if c.touchID {
				d.enrollTouchID()
			}
			plain := d.agent(nil, nil)
			text := d.refused(plain, "tasks_list", map[string]any{"project": pmLocked})
			short := pmShortRe.FindString(text)
			require.NotEmpty(t, short, text)
			a, err := d.svc.Resolve(ctx, short)
			require.NoError(t, err)
			page := pmOrigin + "/console/access/" + a.ID
			// Touch ID is never offered to a client with no elicitation; a
			// form client that shows the dialog gets the dialog's remedy.
			remedy := func() string {
				ways := []string{"in their own terminal with `seam approve " + short + "`"}
				if c.passkey {
					ways = append(ways, "with a passkey at "+page)
				}
				return "Remedy: access request " + short + " is waiting -- the owner approves it " + pmOrList(ways) +
					"; retry once it is approved, and keep to other projects until then."
			}
			rule := "tasks_list: project " + pmLocked + " is locked: even the owner's own agents need the owner's approval, " +
				"and this session holds none."

			// A client with no elicitation: never Touch ID.
			require.Equal(t, []string{rule, remedy()}, pmLines(text))
			res, isErr := paCall(t, plain, "access_request", map[string]any{"project": pmLocked})
			require.False(t, isErr, res)
			var out struct {
				Page string `json:"page"`
				Next string `json:"next"`
			}
			require.NoError(t, json.Unmarshal([]byte(res), &out))
			if c.passkey {
				require.Equal(t, page, out.Page)
			} else {
				require.Empty(t, out.Page, "no passkey enrolled: no page")
			}
			require.NotContains(t, out.Next, "Touch ID")
			status, body := d.do(d.socket(pmBridge, pmAgent, pmShell), "", http.MethodPost, "/console/access/request",
				url.Values{"project": {pmLocked}}, map[string]string{mcp.AgentProcessHeader: pmAgent.AgentID(), mcp.HostHeader: pmHost})
			require.Equal(t, http.StatusOK, status, body)
			var filed struct {
				ShortID string `json:"short_id"`
				Page    string `json:"page"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &filed))
			require.Equal(t, short, filed.ShortID)
			if c.passkey {
				require.Equal(t, page, filed.Page)
			} else {
				require.Empty(t, filed.Page)
			}

			// A form-elicitation client, which declines the dialog when one is
			// shown: Touch ID appears exactly when a key is enrolled.
			o := newPMOwner(pmAnswer(mcpgo.ElicitationResponseActionDecline, nil))
			form := d.agent(o, nil)
			text = d.refused(form, "tasks_list", map[string]any{"project": pmLocked})
			if !c.touchID {
				require.Equal(t, []string{rule, remedy()}, pmLines(text), "no proof finishes in a form: no dialog")
				require.Empty(t, o.dialogs())
				return
			}
			require.Len(t, o.dialogs(), 1)
			still := []string{"`seam approve " + short + "` in their own terminal"}
			if c.passkey {
				still = append(still, "a passkey at "+page)
			}
			still = append(still, "Touch ID in the next approval dialog")
			require.Equal(t, []string{rule, "Remedy: the approval dialog was declined; access request " + short +
				" stays pending for the owner: " + pmOrList(still) + "; retry once it is approved, and keep to other " +
				"projects until then."}, pmLines(text))
			d.nothing("a declined dialog", map[string]string{a.ID: store.ApprovalPending})
		})
	}
}

// pmHook posts a hook body as `seam hook` does on the socket and returns the
// additional context it answered.
func pmHook(t *testing.T, base, endpoint string, body []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/hooks/"+endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.HookSpecificOutput.AdditionalContext
}
