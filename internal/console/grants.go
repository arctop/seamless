package console

// Read grants on the access pages (isolation.requests; Config.Grants set):
// `seam access request` for a confidential project, the approval of a grant
// request, and Revoke for a live grant. Every route answers 404 with read
// grants off (accessOff, or the revoke's own check), so nothing here changes a
// console that never turned them on.
//
// Under peer_auth a grant needs the approve core's proofs: here, a passkey
// assertion (the terminal commit is seamlessd approve's). Without peer_auth --
// the advisory tier -- a decision without a passkey response is the bearer
// key's (seam approve's y/N, or this page's Approve under the console
// cookie), and any holder of the key can make it.

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// grantRequest files a read grant request explicitly: `seam access request
// <confidential project>`, run from the agent's own shell, whose session the
// agent-process header names.
func (s *Service) grantRequest(w http.ResponseWriter, r *http.Request, project, proc, host string) {
	a, err := s.cfg.Grants.RequestGrant(r.Context(), approval.GrantInput{
		Project: project, AgentProcess: proc, Host: host, Reason: r.PostForm.Get("reason"), Via: "request",
	})
	if err != nil {
		msg := err.Error()
		var anchor *approval.AnchorError
		var live *approval.AccessLiveError
		switch {
		case errors.As(err, &anchor):
			msg = anchor.Error()
		case errors.As(err, &live):
			msg = live.Answer()
		case errors.Is(err, approval.ErrNotConfidential) && s.cfg.Approvals != nil:
			msg = "that project is neither locked nor confidential: nothing to request"
		case errors.Is(err, approval.ErrNotConfidential):
			msg = "that project is not confidential: nothing to request"
		case errors.Is(err, approval.ErrSealed):
			msg = "a sealed project is never granted, and a sealed project's session is never granted another: nothing to request"
		case errors.Is(err, approval.ErrNoProject):
			msg = "the asking session works in no project: a read grant opens a project to another project, never to the global scope"
		case errors.Is(err, approval.ErrNoProcess):
			msg = "this connection has no verified process: request access from an agent session whose seam dials the peer_auth Unix socket"
		case errors.Is(err, approval.ErrNoSession), errors.Is(err, approval.ErrNoAnchor), errors.Is(err, approval.ErrAnchorNotInChain):
			msg = "no agent session names this process: run it from the agent's own shell, where seam names the agent that started it"
		case errors.Is(err, access.ErrNotReachable):
			msg = "that project is not reachable from this connection"
		case errors.Is(err, store.ErrInvalidAccessRequest) && utf8.RuneCountInString(strings.TrimSpace(r.PostForm.Get("reason"))) > store.MaxAccessReasonRunes:
			msg = "the reason is limited to 200 characters"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": a.ID, "short_id": approval.ShortID(a.ID), "project": a.Project, "kind": a.Kind, "from": a.FromProject,
		"page": s.cfg.Grants.Ways(r.Context(), a.ID).Page,
	})
}

// grantApprove decides a read grant request: a passkey assertion when the
// form carries one, else -- in the advisory tier only -- the bearer key's
// decision. JSON for seam approve and the page's script, a redirect with a
// notice for the advisory page's plain form.
func (s *Service) grantApprove(w http.ResponseWriter, r *http.Request, a store.AccessApproval) {
	if s.cfg.Grants == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "read grants are off (isolation.requests)"})
		return
	}
	proof := approval.Proof{Kind: approval.ProofKey}
	if resp := r.PostForm.Get("response"); resp != "" {
		proof = approval.Proof{Kind: approval.ProofPasskey, Assertion: []byte(resp)}
	}
	grant, err := s.cfg.Grants.Approve(r.Context(), approval.Decision{
		ID: a.ID, Duration: r.PostForm.Get("duration"), Digest: r.PostForm.Get("digest"), Proof: proof,
	})
	if err != nil {
		s.accessError(w, r, err)
		return
	}
	afterAccessWrite(w, r, map[string]any{
		"approved": approval.ShortID(a.ID), "project": a.Project, "from": a.FromProject, "grant": grant.ID,
		"expires_at": grant.ExpiresAt.UTC().Format(time.RFC3339),
	}, "Approved "+approval.ShortID(a.ID)+": "+a.Project+" is readable from "+a.FromProject+", read-only, until "+
		grant.ExpiresAt.Local().Format("15:04")+".")
}

// grantRevoke ends a live read grant before its expiry. Its holder stays
// write-confined: a revoke does not unread what it read. An agent's socket
// connection may not revoke: the owner's browser or the TCP key only.
func (s *Service) grantRevoke(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Grants == nil {
		s.notFound(w, r, "read grants are off (isolation.requests), so there are none to revoke")
		return
	}
	if !ownerDecision(w, r) {
		return
	}
	g, err := s.cfg.Grants.RevokeGrant(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrGrantNotLive) || errors.Is(err, store.ErrGrantNotFound) {
		s.notFound(w, r, "that grant already ended")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	afterAccessWrite(w, r, map[string]any{"revoked": g.ID, "project": g.Project, "from": g.FromProject},
		"Revoked the read grant of "+g.Project+" to "+g.FromProject+".")
}
