package hooks

// Locked projects at the hooks (peer_auth.locked): the SessionStart notice in
// a locked project's repository, with the access request filed for the
// session it records there, and the lease expiry warnings in the briefing and
// the prompt injections. Every path here runs only while the daemon locks a
// project (Config.Approvals set, or a View that holds a lock); with peer_auth
// off the hooks answer exactly as before.

import (
	"context"
	"net/http"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// lockedStart answers a SessionStart in the repository of a project the
// caller is locked out of: the owner's agent, with no lease on it yet. The
// session is recorded in that project, metadata only (owner decision Q4): its
// name, cwd, host and model, and the agent process its MCP connection binds
// by. Nothing is read from the project and nothing is written into it until
// the owner approves; the findings tools refuse it at the lock. An access
// request is filed for the session here, so the notice can name it and the
// owner has it waiting; the project's briefing arrives with the first tool
// result after the approval (internal/mcp).
func (h *Handler) lockedStart(ctx context.Context, w http.ResponseWriter, client Client, host string, p hookPayload, project string) {
	v, _ := access.FromContext(ctx)
	var sessionID, name string
	if p.AgentType == "" {
		sessionID, name = h.ensureAmbientSessionIn(ctx, client, host, p, project)
		if name != "" {
			h.setAmbientModel(ctx, client, host, p.SessionID, pushedModel(p.Model, p.Capture), p.TranscriptPath)
		}
	}
	var req access.LockRequest
	if h.approvals != nil {
		a, err := h.approvals.Request(ctx, approval.RequestInput{
			Project: project, View: v, SessionID: sessionID, AgentProcess: p.Identity.AgentProcess,
			Host: host, Via: "refusal",
		})
		if err != nil {
			// No request (the TCP key, no verified process): the notice
			// carries the lock's plain remedy instead.
			h.logger.Debug("hooks: access request not filed", "project", project, "error", err)
		} else {
			req = access.LockRequest{ID: a.ID, Short: approval.ShortID(a.ID),
				Refresh: a.Kind == store.AccessKindRefresh, Page: h.approvals.Page(a.ID)}
		}
	}
	notice := lockedNotice(project, v.RefusalFor(project, req), sessionID != "")
	if name != "" {
		notice = injectAmbientLine(notice, name, p.Identity.AgentProcess != "")
	}
	// A lease this session holds on another locked project, near its end.
	notice = injectNotice(notice, h.expiryNotice(ctx, sessionID))
	h.writeContextResponse(ctx, w, "SessionStart", "session-start", client,
		p.SessionID, "", notice, false, nil)
}

// lockedNotice is the SessionStart context in a locked project's repository:
// the lock's rule and remedy (refusal, which names the request when one was
// filed), what happened to this session, and when the briefing comes. It
// names the project and nothing from inside it.
func lockedNotice(project string, refusal error, recorded bool) string {
	var b strings.Builder
	b.WriteString("<seam-briefing>\nSeamless: ")
	b.WriteString(refusal.Error())
	b.WriteString("\n")
	if recorded {
		b.WriteString("This session is recorded in " + project + " (its name, cwd and model only); until the owner " +
			"approves, Seamless tools cannot read or write " + project + ", and its findings are refused. ")
	} else {
		b.WriteString("Until the owner approves, Seamless tools cannot read or write " + project + ". ")
	}
	b.WriteString("The project's briefing arrives with the first Seamless tool result after the approval.\n</seam-briefing>")
	return b.String()
}

// injectNotice puts a notice at the head of a briefing, just inside its
// opening tag (where a truncating client keeps it), or wraps it as a briefing
// of its own when there is none. An empty notice changes nothing.
func injectNotice(briefing, notice string) string {
	if notice == "" {
		return briefing
	}
	const openTag = "<seam-briefing>\n"
	if briefing == "" {
		return openTag + "Seamless: " + notice + "\n</seam-briefing>"
	}
	if rest, ok := strings.CutPrefix(briefing, openTag); ok {
		return openTag + "Seamless: " + notice + "\n" + rest
	}
	return injectNotice("", notice) + "\n" + briefing
}

// expiryNotice is the lease expiry warning for a session (access.ExpiryNotice):
// the leases granted to its requests (store.AccessLeasesForSession) and the
// ones this request's own View holds, for every one ending within
// access.ExpiryWarning. "" when none does -- and without a database lookup
// at all unless the daemon locks a project.
func (h *Handler) expiryNotice(ctx context.Context, sessionID string) string {
	if h.approvals == nil || h.db == nil {
		return ""
	}
	now := h.now()
	seen := map[string]bool{}
	var leases []access.Lease
	add := func(ls []access.Lease) {
		for _, l := range ls {
			if !seen[l.ID] {
				seen[l.ID] = true
				leases = append(leases, l)
			}
		}
	}
	if v, ok := access.FromContext(ctx); ok {
		add(v.Leases())
	}
	if sessionID != "" {
		rows, err := store.AccessLeasesForSession(ctx, h.db, sessionID, now)
		if err != nil {
			h.logger.Warn("hooks: session leases", "session_id", sessionID, "error", err)
		} else {
			add(store.AccessLeases(rows))
		}
	}
	return access.ExpiryNotice(leases, now)
}

// ambientSessionID is the Seamless id of the ambient session a client's id
// names, "" when none (or no lookup is needed: nothing is locked).
func (h *Handler) ambientSessionID(ctx context.Context, client Client, externalID string) string {
	if h.approvals == nil {
		return ""
	}
	id, _ := h.ambientRef(ctx, client, externalID)
	return id
}
