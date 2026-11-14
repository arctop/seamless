package mcp

// The in-session approval dialog of the advisory tier: read grants on
// (isolation.requests) without peer_auth. Plan fenced-sessions P9, owner
// decision D7 of the fenced-sessions design.
//
// Without peer_auth the bearer key decides every grant and move request --
// seam approve's y/N, the console's Approve and Deny -- and D7 lets the
// owner's answer in the agent's own client count too. A call that files (or
// finds) a move or grant request -- a refusal at a fenced project,
// session_start into one, an explicit access_request -- shows the owner the
// request as a form, and Approve or Deny there decides it
// (approval.ProofDialog, Grants.DenyInDialog). Like every decision of this
// tier it is advisory: Seamless cannot tell who answers a dialog any more than
// who holds the key, so the form says so, and the record names the dialog
// (approval.DecidedByDialog).
//
// The form shows what the console's Access requests page and seam approve
// show: the warnings first, the project, what the request carries, the
// session and the agent's own claims labeled as such, the agent's reason as
// its own words. It asks for the decision (Approve or Deny, no default: a
// form sent back without one decides nothing), the access where the request
// offers read-only or read and write (read-only by default), and for a grant
// how long. A move has no access choice and, nothing being locked without
// peer_auth, no duration: it lasts for good. Declining, cancelling and the
// dialog's bound (DialogWait, from the start of the call) decide nothing: the
// request stays pending, for the owner to decide in their terminal or the
// console, or in the next dialog.
//
// It runs only in the advisory tier, on a call that carries no View (with
// peer_auth on every request carries one), for a client that declared form
// elicitation. Under peer_auth the dialog is elicit.go's, which only ever
// starts a proof, and the approval core refuses this one's decisions
// (approval.ErrDialogPeerAuth).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// The advisory form's decisions.
const (
	decisionApprove = "approve"
	decisionDeny    = "deny"
)

// How an advisory dialog's accepted answer failed to decide its request; the
// request stays pending either way.
const (
	// answerChanged: the request's facts changed while the dialog showed them
	// (a new warning, the agent's reason filled in), so the Approve, which is
	// over the facts shown, was refused.
	answerChanged = "changed"
	// answerFailed: the approval core refused the decision for another
	// reason, such as a fence that changed while the request waited.
	answerFailed = "failed"
)

// advisoryTier reports read grants without peer_auth: the bearer key decides
// every request, and the in-session dialog's Approve with it (D7).
func (s *Server) advisoryTier() bool {
	return s.cfg.Approvals == nil && s.cfg.Grants != nil && s.cfg.Grants.Advisory()
}

// advisoryCall reports a call of the advisory tier: one that carries no View,
// since with peer_auth on every request carries one.
func (s *Server) advisoryCall(ctx context.Context) bool {
	if !s.advisoryTier() {
		return false
	}
	_, hasView := access.FromContext(ctx)
	return !hasView
}

// advisoryDecider is what the advisory dialog needs of the read grants
// (*approval.Grants): asserted, so the Grants interface the rest of the server
// holds keeps its shape.
type advisoryDecider interface {
	Facts(ctx context.Context, id string) (approval.Facts, error)
	LeaseLength(choice string) (time.Duration, error)
	Approve(ctx context.Context, d approval.Decision) (store.AccessGrant, error)
	ApproveMove(ctx context.Context, d approval.Decision) (approval.MoveApproved, error)
	DenyInDialog(ctx context.Context, id string) error
}

// advisoryDialog runs the advisory tier's dialog for request t, filed by the
// call in ctx that started at start, and decides the request with the owner's
// answer. It reports false -- show nothing, answer as before -- unless the
// call is the advisory tier's, its client declared form elicitation, the
// request is a pending grant or move, and this connection has no dialog open
// for it already.
func (s *Server) advisoryDialog(ctx context.Context, start time.Time, t dialogTarget) (dialogEnd, bool) {
	decider, ok := s.cfg.Grants.(advisoryDecider)
	if !ok || t.ID == "" || !s.advisoryCall(ctx) {
		return dialogEnd{}, false
	}
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil || session.SessionID() == "" {
		return dialogEnd{}, false
	}
	if _, ok := session.(mcpserver.SessionWithElicitation); !ok {
		return dialogEnd{}, false
	}
	if form, _ := clientElicitation(session); !form {
		return dialogEnd{}, false
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, t.ID)
	if err != nil || s.stateOf(a) != statePending || (a.Kind != store.AccessKindGrant && a.Kind != store.AccessKindMove) {
		return dialogEnd{}, false
	}
	key := session.SessionID() + "\x00" + t.ID
	if !s.claimDialog(key) {
		return dialogEnd{}, false
	}
	defer s.releaseDialog(key)

	facts, err := decider.Facts(ctx, t.ID)
	if err != nil {
		s.logger.Debug("advisory dialog: facts", "request", t.Short, "error", err)
		return dialogEnd{}, false
	}
	capped, err := decider.LeaseLength(approval.ForSession)
	if err != nil {
		s.logger.Debug("advisory dialog: lease cap", "request", t.Short, "error", err)
		return dialogEnd{}, false
	}
	stopAt := start.Add(s.dialogWait())
	form := newAdvisoryForm(facts, capped)
	req, err := form.request(stopAt)
	if err != nil {
		s.logger.Debug("advisory dialog: form", "request", t.Short, "error", err)
		return dialogEnd{}, false
	}
	dctx, cancel := context.WithDeadline(ctx, stopAt)
	defer cancel()
	end := dialogEnd{stopAt: stopAt, advisory: true}
	res, err := s.mcp.RequestElicitation(dctx, req)
	if err != nil {
		if dctx.Err() == nil {
			// The client did not take it: nothing was shown.
			s.logger.Debug("advisory dialog: not shown", "request", t.Short, "error", err)
			return dialogEnd{}, false
		}
		// The bound (or the call's end) came first: nothing was decided.
		return s.settle(ctx, dctx, t, end), true
	}
	switch res.Action {
	case mcp.ElicitationResponseActionAccept:
	case mcp.ElicitationResponseActionDecline:
		end.answer = answerDeclined
		return s.settle(ctx, dctx, t, end), true
	case mcp.ElicitationResponseActionCancel:
		end.answer = answerCancelled
		return s.settle(ctx, dctx, t, end), true
	default:
		end.answer = answerUnoffered
		return s.settle(ctx, dctx, t, end), true
	}
	choice, ok := form.choice(res.Content)
	if !ok {
		end.answer = answerUnoffered
		return s.settle(ctx, dctx, t, end), true
	}
	end.answer = answerAccepted
	if err := advisoryDecide(ctx, decider, a, facts, choice); err != nil {
		end.answer = answerFailed
		if errors.Is(err, approval.ErrFactsChanged) {
			end.answer = answerChanged
		}
		s.logger.Debug("advisory dialog: not decided", "request", t.Short, "decision", choice.decision, "error", err)
	} else {
		s.logger.Debug("advisory dialog: decided", "request", t.Short, "decision", choice.decision)
	}
	return s.settle(ctx, dctx, t, end), true
}

// advisoryDecide records the owner's answer to request a, whose facts the
// dialog showed: Approve through the approve core over those facts' digest
// (a grant's with the access and duration chosen; a move's for good), Deny as
// the dialog's denial. The answer is the owner's once it arrived, so the call
// ending meanwhile does not undo it.
func advisoryDecide(ctx context.Context, d advisoryDecider, a store.AccessApproval, f approval.Facts, c advisoryChoice) error {
	ctx = context.WithoutCancel(ctx)
	if c.decision == decisionDeny {
		return d.DenyInDialog(ctx, a.ID)
	}
	dec := approval.Decision{ID: a.ID, Duration: c.duration, Digest: f.Digest, Access: c.access,
		Proof: approval.Proof{Kind: approval.ProofDialog}}
	if a.Kind == store.AccessKindMove {
		_, err := d.ApproveMove(ctx, dec)
		return err
	}
	_, err := d.Approve(ctx, dec)
	return err
}

// advisoryForm is the advisory dialog for one request: its facts, the lease
// cap its durations are worded against, the durations it offers (none for a
// move no lock covers, which lasts for good) and the accesses (none where the
// facts offer no choice).
type advisoryForm struct {
	facts         approval.Facts
	capped        time.Duration
	durations     []string
	durationNames []string
	accesses      []string
}

func newAdvisoryForm(f approval.Facts, capped time.Duration) advisoryForm {
	form := advisoryForm{facts: f, capped: capped}
	if f.Kind != store.AccessKindMove || f.Locked {
		form.durations, form.durationNames = durationChoices(capped)
	}
	if len(f.AccessChoices) > 1 {
		form.accesses = slices.Clone(f.AccessChoices)
	}
	return form
}

// advisoryChoice is an accepted advisory form: the decision, and for an
// Approve the duration and the access ("" for the facts' own).
type advisoryChoice struct {
	decision, duration, access string
}

// accessTitles are the words the form shows for each access choice.
var accessTitles = map[string]string{
	"read-only":  "Read-only",
	"read-write": "Read and write",
}

// request is the form's elicitation, worded for the dialog that stops waiting
// at stopAt.
func (af advisoryForm) request(stopAt time.Time) (mcp.ElicitationRequest, error) {
	schema, err := af.schema()
	if err != nil {
		return mcp.ElicitationRequest{}, err
	}
	return mcp.ElicitationRequest{
		Request: mcp.Request{Method: string(mcp.MethodElicitationCreate)},
		Params:  mcp.ElicitationParams{Message: advisoryDialogMessage(af, stopAt), RequestedSchema: schema},
	}, nil
}

// schema is the form's fields, in the order the owner reads them: the
// decision, then -- for an Approve -- the access and how long.
func (af advisoryForm) schema() (json.RawMessage, error) {
	fields := []schemaField{{name: "decision", spec: map[string]any{
		"type": "string", "title": "Decision",
		"description": "Approve decides the request now, with no other proof (advisory: Seamless runs without peer_auth); " +
			"Deny denies it",
		"enum": []string{decisionApprove, decisionDeny}, "enumNames": []string{"Approve", "Deny"},
	}}}
	required := []string{"decision"}
	if len(af.accesses) > 0 {
		fields = append(fields, schemaField{name: "access", spec: map[string]any{
			"type": "string", "title": "Access",
			"description": "read-only is the default; what each lets happen is above",
			"enum":        af.accesses, "enumNames": titles(af.accesses, accessTitles),
			"default": af.accesses[0],
		}})
		required = append(required, "access")
	}
	if len(af.durations) > 0 {
		fields = append(fields, schemaField{name: "duration", spec: map[string]any{
			"type": "string", "title": "Approve for",
			"description": "how long the access lasts",
			"enum":        af.durations, "enumNames": af.durationNames,
			"default": approval.ForSession,
		}})
		required = append(required, "duration")
	}
	return orderedSchema(fields, required)
}

// choice reads the accepted form: a decision it offered, and for an Approve
// one of the durations and accesses it offered (the default when the form came
// back without one). ok is false for anything else, which decides nothing.
func (af advisoryForm) choice(content any) (advisoryChoice, bool) {
	fields, _ := content.(map[string]any)
	decision, _ := fields["decision"].(string)
	switch decision {
	case decisionDeny:
		return advisoryChoice{decision: decisionDeny}, true
	case decisionApprove:
	default:
		return advisoryChoice{}, false
	}
	c := advisoryChoice{decision: decisionApprove, duration: approval.ForSession}
	if len(af.durations) > 0 {
		if d, _ := fields["duration"].(string); d != "" {
			c.duration = d
		}
		if !slices.Contains(af.durations, c.duration) {
			return advisoryChoice{}, false
		}
	}
	if len(af.accesses) > 0 {
		c.access = af.accesses[0]
		if v, _ := fields["access"].(string); v != "" {
			c.access = v
		}
		if !slices.Contains(af.accesses, c.access) {
			return advisoryChoice{}, false
		}
	}
	return c, true
}

// schemaField is one property of a form's schema.
type schemaField struct {
	name string
	spec map[string]any
}

// orderedSchema is an object schema whose properties keep the order given:
// encoding/json writes a map's keys sorted, and a client shows a form's
// fields in the order they arrive.
func orderedSchema(fields []schemaField, required []string) (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteString(`{"type":"object","properties":{`)
	for i, f := range fields {
		name, err := json.Marshal(f.name)
		if err != nil {
			return nil, err
		}
		spec, err := json.Marshal(f.spec)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(spec)
	}
	req, err := json.Marshal(required)
	if err != nil {
		return nil, err
	}
	b.WriteString(`},"required":`)
	b.Write(req)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// advisoryDialogMessage renders request facts for the advisory dialog that
// stops waiting at stopAt, as the console's Access requests page and seam
// approve show them. The first two lines say what is asked, warning count
// first (Claude Code collapses a dialog to its first lines); the warnings
// follow, before every other fact; then the project, what the request carries
// and what each access lets happen, the session with the agent's own claims
// labeled as such, the request and its times, the agent's reason as its own
// words, and what the answer does. Every value is stripped of control and
// format characters (plain).
func advisoryDialogMessage(af advisoryForm, stopAt time.Time) string {
	f := af.facts
	var b strings.Builder
	b.WriteString(advisoryLead(f, af.capped) + "\n")
	if len(f.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range f.Warnings {
			b.WriteString("- " + plain(w.Text) + "\n")
		}
	}
	b.WriteString("\n")
	line := func(label, value string) {
		if value = plain(value); value != "" {
			b.WriteString(label + ": " + value + "\n")
		}
	}
	switch {
	case f.Kind == store.AccessKindMove && f.From == "":
		line("Project", f.Project+" ("+f.Isolation+"), moving from the global scope")
	case f.Kind == store.AccessKindMove:
		line("Project", f.Project+" ("+f.Isolation+"), moving from project "+f.From)
	default:
		line("Project", f.Project+" ("+f.Isolation+"), read from project "+f.From)
	}
	accessWords := f.Access
	if len(af.accesses) > 0 {
		accessWords = f.Access + " unless you choose read and write below"
	}
	line("Access", accessWords)
	if f.AskedAccess == "read-write" {
		line("Asked for", "read and write")
	}
	if c := f.Carry; c != nil {
		line("Holds once approved", approval.JoinAnd(c.Holds))
		writes := "nowhere"
		if len(c.WriteTo) > 0 {
			writes = approval.JoinAnd(c.WriteTo)
		}
		if len(af.accesses) > 0 {
			writes += " (approved read-only)"
		}
		line("May write to once approved", writes)
	}
	if len(af.accesses) > 0 {
		line("Read-only lets", copyLineOf(f))
		line("Read and write lets", f.ReadWrite)
	}
	if f.Locked {
		line("Lock", "approving also opens the lock to this agent, for the duration chosen")
	}
	line("Session", f.Session)
	if f.PID == 0 {
		line("Asking", "a session (without peer_auth the daemon knows no process)")
	}
	line("Repository", f.Repo)
	line("Working directory", f.CWD)
	line("Model", f.Model)
	if plain(f.Repo)+plain(f.CWD)+plain(f.Model) != "" {
		b.WriteString(approval.SelfReportedNote + "\n")
	}
	line("Agent process", agentProcessName(f))
	if strings.ContainsAny(f.Program, `/\`) {
		line("Program", f.Program)
	}
	line("Request", f.Short+", "+requestKindWords(f))
	line("This dialog stops waiting at", access.Clock(stopAt))
	line("The request expires at", access.Clock(f.ExpiresAt))
	if reason := plain(f.Reason); reason != "" {
		b.WriteString("\nThe agent's reason, in its own words (Seamless did not check it):\n\"" + reason + "\"\n")
	}
	b.WriteString("\nChoosing Approve decides the request now, with no other proof: without peer_auth this approval is " +
		"advisory, since Seamless cannot tell who answers this dialog, and any process holding the bearer key could " +
		"make the same decision. Choosing Deny denies it. Declining or cancelling decides nothing: the request stays " +
		"pending for `seam approve " + plain(f.Short) + "` in your own terminal, or the console's Access requests page.")
	return b.String()
}

// advisoryLead is the advisory dialog's first two lines: the warning count,
// who asks for what, then how long and that Approve below decides it.
//
//	1 warning. Session cc/fs-agent asks to move into example-finance.
//	For good: a move does not end. Approve below decides it.
func advisoryLead(f approval.Facts, capped time.Duration) string {
	if f.Kind == store.AccessKindMove && !f.Locked {
		return warningCount(len(f.Warnings)) + ". " + plain(askerName(f)) + " asks to move into " + plain(f.Project) + ".\n" +
			"For good: a move does not end. Approve below decides it."
	}
	if f.Kind == store.AccessKindMove {
		return warningCount(len(f.Warnings)) + ". " + plain(askerName(f)) + " asks to move into " + plain(f.Project) + ".\n" +
			"For good, and its lock opens until its session ends (" + spanWords(capped) + " at most), unless you pick " +
			"less below. Approve below decides it."
	}
	asked := f.Access
	if f.AskedAccess != "" {
		asked = f.AskedAccess
	}
	long := "Until its session ends (" + spanWords(capped) + " at most)"
	if f.SessionID == "" {
		long = "For " + spanWords(capped)
	}
	return warningCount(len(f.Warnings)) + ". " + plain(askerName(f)) + " asks for " + plain(asked) + " access to " +
		plain(f.Project) + " from project " + plain(f.From) + ".\n" + long + ", unless you pick less below. Approve below decides it."
}

// copyLineOf is a grant's copy line, the facts' copy_into warning.
func copyLineOf(f approval.Facts) string {
	for _, w := range f.Warnings {
		if w.Code == approval.WarnCopyInto {
			return w.Text
		}
	}
	return ""
}

// advisoryApproves says how the owner approves request short in the advisory
// tier: their own terminal, or the console's Access requests page.
func advisoryApproves(short string) string {
	return "the owner approves it in their own terminal with `seam approve " + short + "`, or with Approve on the " +
		"console's Access requests page"
}

// advisoryStillPending says how the owner may still decide request short
// once the advisory dialog ended without a decision: their own terminal, the
// console, or the next dialog (the call that needs the project again, retried,
// shows one).
func advisoryStillPending(short string) string {
	return "`seam approve " + short + "` in their own terminal, Approve on the console's Access requests page, or " +
		"Approve in the next approval dialog"
}
