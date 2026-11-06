package console

// Access requests for locked projects (peer_auth.locked): the pending list,
// one request's facts with its passkey approval, and the JSON the terminal
// path (seam approve) reads. Every route here is a 404 unless a project is
// locked (Config.Approvals set).
//
// What the routes never do: approve without a proof. The passkey approval is
// an assertion over the request's own challenge; the terminal approval is not
// a route at all (seamlessd approve, run as the daemon's account). Denying
// needs no proof -- it only ever closes a request -- but an agent's socket
// connection may not deny: only the owner's browser (the console cookie) or
// the TCP key can.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// TerminalApprover tells `seam approve` how to reach the terminal path's
// commit: the daemon's account, its seamlessd, and the config it loads.
type TerminalApprover struct {
	Account   string `json:"account"`
	Seamlessd string `json:"seamlessd"`
	Config    string `json:"config"`
}

type accessListData struct {
	Requests  []approval.Facts  `json:"requests"`
	Terminal  *TerminalApprover `json:"terminal,omitempty"`
	Durations []string          `json:"durations"`
	MaxLease  string            `json:"max_lease"`
}

type accessOneData struct {
	Request        approval.Facts    `json:"request"`
	Status         string            `json:"status"`
	Page           string            `json:"page,omitempty"`
	PasskeyOptions json.RawMessage   `json:"passkey_options,omitempty"`
	Terminal       *TerminalApprover `json:"terminal,omitempty"`
	Durations      []string          `json:"durations"`
	MaxLease       string            `json:"max_lease"`
	// OnOrigin and Origin are for the page: a passkey ceremony runs only on
	// the relying party's origin.
	OnOrigin bool   `json:"-"`
	Origin   string `json:"-"`
}

func (s *Service) maxLease() string {
	d, err := s.cfg.Approvals.LeaseLength(approval.ForSession)
	if err != nil {
		return ""
	}
	return d.String()
}

func (s *Service) accessList(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	pending, err := s.cfg.Approvals.Pending(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if pending == nil {
		pending = []approval.Facts{}
	}
	d := accessListData{Requests: pending, Terminal: s.cfg.Terminal, Durations: approval.Durations, MaxLease: s.maxLease()}
	s.render(w, r, "access", pageData{Title: "Access requests", Data: d})
}

func (s *Service) accessOne(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	a, err := s.cfg.Approvals.Resolve(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrApprovalNotFound) {
		s.notFound(w, r, "no such access request")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	facts, err := s.cfg.Approvals.Facts(r.Context(), a.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := accessOneData{Request: facts, Status: a.Status, Page: s.cfg.Approvals.Page(a.ID), Terminal: s.cfg.Terminal,
		Durations: approval.Durations, MaxLease: s.maxLease()}
	if origins := s.cfg.Approvals.Origins(); len(origins) > 0 {
		d.Origin = origins[0]
		d.OnOrigin = containsString(origins, requestOrigin(r))
	}
	if a.Status == store.ApprovalPending && s.cfg.Approvals.PasskeysOn() {
		if opts, err := s.cfg.Approvals.AssertionOptions(r.Context(), a.ID); err == nil {
			d.PasskeyOptions = opts
		}
	}
	s.render(w, r, "access_one", pageData{Title: "Access request " + facts.Short, Data: d})
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// accessRequest files a request explicitly: `seam access request`, run from
// the agent's own shell, so the kernel's chain and the agent-process header
// name the agent.
func (s *Service) accessRequest(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	v, _ := access.FromContext(r.Context())
	proc := strings.TrimSpace(r.Header.Get("X-Seamless-Agent-Process"))
	if !agentproc.Valid(proc) {
		proc = ""
	}
	a, err := s.cfg.Approvals.Request(r.Context(), approval.RequestInput{
		Project: strings.TrimSpace(r.PostForm.Get("project")), View: v, AgentProcess: proc,
		Host:   strings.ToLower(strings.TrimSpace(r.Header.Get("X-Seamless-Host"))),
		Reason: r.PostForm.Get("reason"), Via: "request",
	})
	if err != nil {
		msg := err.Error()
		switch {
		case errors.Is(err, approval.ErrNoProcess):
			msg = "this connection has no verified process: request access from an agent session whose seam dials the peer_auth socket"
		case errors.Is(err, approval.ErrNoAnchor), errors.Is(err, approval.ErrAnchorNotInChain):
			msg = "no agent session names this process: run it from the agent's own shell, where seam names the agent that started it"
		case errors.Is(err, approval.ErrNotLocked):
			msg = "that project is not locked: nothing to request"
		case errors.Is(err, store.ErrInvalidAccessRequest):
			msg = "the reason is limited to 200 characters"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": a.ID, "short_id": approval.ShortID(a.ID), "project": a.Project, "kind": a.Kind, "page": s.cfg.Approvals.Page(a.ID),
	})
}

// accessApprove approves a request with a passkey assertion over its own
// challenge, for the duration and over the facts digest the page showed.
func (s *Service) accessApprove(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	a, err := s.cfg.Approvals.Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such access request"})
		return
	}
	lease, err := s.cfg.Approvals.Approve(r.Context(), approval.Decision{
		ID: a.ID, Duration: r.PostForm.Get("duration"), Digest: r.PostForm.Get("digest"),
		Proof: approval.Proof{Kind: approval.ProofPasskey, Assertion: []byte(r.PostForm.Get("response"))},
	})
	if err != nil {
		s.accessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approved": approval.ShortID(a.ID), "project": a.Project, "expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// accessDeny closes a request without a lease. An agent's socket connection
// may not deny: the owner's browser or the TCP key only.
func (s *Service) accessDeny(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	if access.PeerAuthenticated(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "deny from the console, or with seam approve in the owner's terminal"})
		return
	}
	a, err := s.cfg.Approvals.Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such access request"})
		return
	}
	if err := s.cfg.Approvals.Deny(r.Context(), a.ID, "console"); err != nil {
		s.accessError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"denied": approval.ShortID(a.ID), "project": a.Project})
}

func (s *Service) accessError(w http.ResponseWriter, r *http.Request, err error) {
	msg := ""
	switch {
	case errors.Is(err, approval.ErrFactsChanged):
		msg = "the request's facts changed since this page loaded: reload and look at them again"
	case errors.Is(err, store.ErrApprovalNotPending):
		msg = "this request was already decided"
	case errors.Is(err, store.ErrApprovalExpired):
		msg = "this request expired: the agent can ask again"
	case errors.Is(err, approval.ErrDuration):
		msg = "choose a duration: until the session ends, 1 hour or 4 hours"
	case errors.Is(err, approval.ErrNoProof), errors.Is(err, approval.ErrPasskeysOff):
		msg = "approving needs a passkey here, or seam approve in the owner's terminal"
	}
	if msg != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}
	s.approverError(w, r, err)
}
