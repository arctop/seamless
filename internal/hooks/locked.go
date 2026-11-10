package hooks

// Locked projects at the hooks: the SessionStart notice in
// a locked project's repository, with the access request filed for the
// session it records there, and the lease expiry warnings in the briefing and
// the prompt injections. Every path here runs only while the daemon locks a
// project (Config.Approvals set, or a View that holds a lock); with peer_auth
// off the hooks answer exactly as before.

import (
	"context"
	"errors"
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
//
// The agent process the session would be stamped with is judged first
// (approval's anchor rule, owner decision 2026-11-08): one no approval may
// name -- a shell, a terminal, a multiplexer, which is what the walk finds when
// `seam` runs with no agent above it, in a tmux pane say -- could never have a
// request filed or a lease approved, so nothing is recorded, and the notice
// says why and what to do instead.
//
// The notice offers Touch ID only to an agent host that may show the approval
// dialog's form (formDialogClient), hedged for a host the hook cannot judge
// (touchIDIf), as MCP offers it only to a client that declared form
// elicitation (internal/mcp callWays).
func (h *Handler) lockedStart(ctx context.Context, w http.ResponseWriter, client Client, host string, p hookPayload, project string) {
	v, _ := access.FromContext(ctx)
	var refused *approval.AnchorError
	if h.approvals != nil && p.AgentType == "" {
		if err := h.approvals.JudgeStart(v, p.Identity.AgentProcess); errors.As(err, &refused) {
			h.logger.Debug("hooks: locked session not recorded", "project", project, "error", err)
		}
	}
	var sessionID, name string
	if p.AgentType == "" && refused == nil {
		sessionID, name = h.ensureAmbientSessionIn(ctx, client, host, p, project)
		if name != "" {
			h.setAmbientModel(ctx, client, host, p.SessionID, pushedModel(p.Model, p.Capture), p.TranscriptPath)
		}
	}
	var req access.LockRequest
	switch {
	case refused != nil:
		req = access.LockRequest{Unfiled: refused.Rule(), UnfiledRemedy: refused.Remedy()}
	case h.approvals != nil:
		a, err := h.approvals.Request(ctx, approval.RequestInput{
			Project: project, View: v, SessionID: sessionID, AgentProcess: p.Identity.AgentProcess,
			Host: host, Via: approval.ViaRefusal,
		})
		var anchor *approval.AnchorError
		var denied *approval.DeniedError
		switch {
		case errors.As(err, &anchor):
			// A process no approval may name: the notice says why nothing
			// was filed, and what to do instead.
			h.logger.Debug("hooks: access request not filed", "project", project, "error", err)
			req = access.LockRequest{Unfiled: anchor.Rule(), UnfiledRemedy: anchor.Remedy()}
		case errors.As(err, &denied):
			// The owner denied this agent's last request (a compact or a
			// clear in the same process): the notice says so and files nothing.
			req = denied.LockRequest()
		case err != nil:
			// No request (the TCP key, no verified process): the notice
			// carries the lock's plain remedy instead.
			h.logger.Debug("hooks: access request not filed", "project", project, "error", err)
		default:
			ways := h.approvals.Ways(ctx, a.ID)
			req = access.LockRequest{ID: a.ID, Short: approval.ShortID(a.ID),
				Refresh: a.Kind == store.AccessKindRefresh, Page: ways.Page,
				TouchID: ways.TouchID && formDialogClient(client), TouchIDIf: touchIDIf(client)}
		}
	}
	outcome := lockedUnrecorded
	switch {
	case refused != nil:
		outcome = lockedRefused
	case sessionID != "":
		outcome = lockedRecorded
	}
	notice := lockedNotice(project, v.RefusalFor(project, req), outcome)
	if name != "" {
		notice = injectAmbientLine(notice, name, p.Identity.AgentProcess != "")
	}
	// A lease this session holds on another locked project, near its end.
	notice = injectNotice(notice, h.expiryNotice(ctx, sessionID))
	h.writeContextResponse(ctx, w, "SessionStart", "session-start", client,
		p.SessionID, "", notice, false, nil)
}

// formDialogClient reports whether an agent host may show an MCP form dialog
// (elicitation), the one Touch ID approval runs through: Claude Code does (the
// owner approved in it on 2026-11-07), and so does Codex (owner decision
// 2026-11-09: 0.163.0-alpha.10.2 renders the whole form and completed Touch ID
// through it) -- though older captured builds (internal/hooks/testdata/codex)
// declare no form elicitation, so its offer is hedged (touchIDIf). The client
// is what the hook already knows of the agent, and unlike the program's name
// it holds for Claude Code installed with npm, which runs as node.
func formDialogClient(c Client) bool { return c == ClientClaudeCode || c == ClientCodex }

// touchIDIf is the condition a host's Touch ID offer is hedged with: the
// SessionStart hook runs before MCP connects, so it has no client version and
// cannot see whether this Codex declares form elicitation (the tool-call
// answers can: callWays gates on the declared capability). "" for a host that
// needs no hedge.
func touchIDIf(c Client) string {
	if c == ClientCodex {
		return "if this Codex shows one"
	}
	return ""
}

// lockedOutcome is what a locked project's SessionStart did with the session.
type lockedOutcome int

const (
	// lockedUnrecorded: no session to record (a subagent's start, no session
	// id, or a session kept in another locked project).
	lockedUnrecorded lockedOutcome = iota
	// lockedRecorded: recorded in the project, metadata only.
	lockedRecorded
	// lockedRefused: not recorded, because no approval may name the agent
	// process (approval.JudgeStart).
	lockedRefused
)

// lockedNotice is the SessionStart context in a locked project's repository:
// the lock's rule and remedy (refusal, which names the request when one was
// filed), what happened to this session, and when the briefing comes. It
// names the project and nothing from inside it.
func lockedNotice(project string, refusal error, outcome lockedOutcome) string {
	var b strings.Builder
	b.WriteString("<seam-briefing>\nSeamless: ")
	b.WriteString(refusal.Error())
	b.WriteString("\n")
	switch outcome {
	case lockedRefused:
		b.WriteString("This session is not recorded, and Seamless tools cannot read or write " + project +
			" from this process.\n</seam-briefing>")
		return b.String()
	case lockedRecorded:
		b.WriteString("This session is recorded in " + project + " (its name, cwd and model only); until the owner " +
			"approves, Seamless tools cannot read or write " + project + ", and its findings are refused. ")
	default:
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
