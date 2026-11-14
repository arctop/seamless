package hooks

// Records follow the home (isolation.requests on; plan fenced-sessions). The
// conversation a hook names -- its client and the session id it issued -- has
// one home: its ambient row's project, which after an approved move is the
// child's (store.MoveApproveWith). With the switch on, everything the hooks
// record for it goes there, wherever the hook's cwd is, once the clearance
// rule clears it:
//
//   - the SessionEnd harvest, Codex Stop findings and pushed findings land on
//     its ambient row (store.RecordDestinationOf already says so); a home the
//     clearance refuses -- an open home holding fenced content -- records
//     none of them: the row keeps the not-recorded line
//     (store.UnrecordedFindings) and the hook answers with the notice
//     (homeRefused);
//   - plan, subagent and pushed captures land in the home's project
//     (captureTarget), dropped with the notice only when the home itself is
//     not cleared (captureConfined);
//   - prompt recall and a subagent's briefing read the home's scope plus the
//     targets of the conversation's live grants (conversationScope);
//   - a resumed conversation resumes in its home and is briefed from it
//     (startHome): never re-scoped into a project its home may not write
//     (resumeScope), a move's child never at all, and a locked home whose
//     lease ended answers with the locked notice (lockedStart);
//   - the events a hook files about the conversation go where its record goes,
//     and carry none of its text when the record goes nowhere (eventRef).
//
// P0's routing (route.go) still decides first: a conversation that bound
// itself to one isolated project with session_start keeps its record there,
// and one bound to several keeps none. A native fenced conversation is its own
// home, so nothing changes for it. The decisions live in the store
// (RecordDestinationOf, CanWrite) and in internal/access (the clearance);
// nothing here tests an isolation state. With the switch off none of this
// runs, and every hook is P0's.

import (
	"context"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// clearanceOf judges the clearance of the conversation a hook names
// (store.ConversationClearance): its client and session id -- under peer_auth
// the caller's kernel chain, which the conversation only gives a home.
func (h *Handler) clearanceOf(ctx context.Context, client Client, externalSessionID string) (access.Grants, error) {
	ref := store.ConversationRef{}
	if externalSessionID != "" {
		ref.Client, ref.ExternalID = client.externalIdentity(), externalSessionID
	}
	return store.ConversationClearance(ctx, h.db, ref, h.now())
}

// homeRefused reports a conversation's findings that may not land on its
// ambient row in home (isolation.requests): notice is the hook's one line,
// line what the row's findings become instead (store.UnrecordedFindings),
// naming projects only. Both are "" when the clearance clears home. A
// clearance that cannot be read records nothing either: notice says so, and
// line is "" -- the row's findings stay as they are.
func (h *Handler) homeRefused(ctx context.Context, client Client, externalSessionID, home, what string) (notice, line string) {
	g, err := h.clearanceOf(ctx, client, externalSessionID)
	if err != nil {
		h.logger.Warn("hooks: load the conversation's clearance", "error", err)
		return unreadClearanceNotice(what), ""
	}
	if notice = clearanceRefusal(ctx, g, home, what); notice == "" {
		return "", ""
	}
	return notice, store.UnrecordedFindings(holderWord(ctx), g.Label(), whereWords(home))
}

// unrecordedFindings is what an ambient row's findings become when its
// conversation's record may not land there: the not-recorded line in place of
// the hooks' own findings (none yet, the no-summary sentinel, an
// auto-harvest, an earlier metadata-only line), or after findings the agent
// wrote itself, which were cleared when it wrote them and are kept. It is
// idempotent: a Codex conversation's every refused turn leaves one line.
func unrecordedFindings(current, line string) string {
	kept := strings.TrimRight(current, "\n")
	if hookFindings(kept) || store.IsRecordLine(kept) {
		return line
	}
	if i := strings.LastIndex(kept, "\n\n"); i >= 0 && store.IsRecordLine(kept[i+2:]) {
		kept = kept[:i]
	}
	return kept + "\n\n" + line
}

// conversationScope is what a conversation's prompt recall and its subagents'
// briefings read with isolation.requests on: its home -- the ambient row's
// project -- and the targets of its live grants approved from there that the
// caller may read (store.CanRead over the conversation's clearance), as the
// MCP edge's recall reads them (grantedScope). ok is false with the switch
// off, for a hook that names no session, and for a conversation with no
// ambient row of the caller's own: those read by cwd, as always. A clearance
// that cannot be read leaves the home alone.
func (h *Handler) conversationScope(ctx context.Context, client Client, externalSessionID string) (home string, also []string, ok bool) {
	if !h.readGrants || h.db == nil || externalSessionID == "" {
		return "", nil, false
	}
	amb, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), externalSessionID)
	if err != nil {
		h.recordHookError(ctx, "home-lookup", client, err)
		return "", nil, false
	}
	if !found || !access.SessionAccountOK(ctx, amb.Account) {
		return "", nil, false
	}
	g, err := h.clearanceOf(ctx, client, externalSessionID)
	if err != nil {
		h.logger.Warn("hooks: load the conversation's clearance", "error", err)
		return amb.ProjectSlug, nil, true
	}
	gctx := access.WithGrants(ctx, g)
	for _, target := range access.GrantedTargets(gctx, amb.ProjectSlug) {
		readable, err := store.CanRead(gctx, h.db, amb.ProjectSlug, target)
		if err != nil {
			h.logger.Warn("hooks: judge a grant's target", "project", target, "error", err)
			continue
		}
		if readable {
			also = append(also, target)
		}
	}
	return amb.ProjectSlug, also, true
}

// eventRef is the session and project a hook's events about the conversation
// are filed under -- its injections and prompts, which carry the
// conversation's own text and what was injected into it -- and whether they
// must carry none of that text (bare). Off, it is the ambient row, as always
// (ambientRef). On, it is where the conversation's record goes
// (RecordDestinationOf): the isolated session it bound itself to, else its
// ambient row; and when the record goes nowhere -- withheld, a destination the
// View or the clearance refuses, or one that could not be read -- the events
// are filed under the ambient row, bare.
func (h *Handler) eventRef(ctx context.Context, client Client, externalSessionID string) (sessionID, project string, bare bool) {
	sessionID, project = h.ambientRef(ctx, client, externalSessionID)
	if !h.readGrants || h.db == nil || externalSessionID == "" {
		return sessionID, project, false
	}
	dest, err := store.RecordDestinationOf(ctx, h.db, client.externalIdentity(), externalSessionID)
	if err != nil {
		h.recordHookError(ctx, "event-record-destination", client, err)
		return sessionID, project, true
	}
	switch dest.Kind {
	case store.RecordDestWithheld:
		return sessionID, project, true
	case store.RecordDestLink:
		if ok, _ := linkWritable(ctx, dest.Session, ""); !ok || !h.cleared(ctx, client, externalSessionID, dest.Session.ProjectSlug) {
			return sessionID, project, true
		}
		return dest.Session.ID, dest.Session.ProjectSlug, false
	}
	if home, ok := dest.Home(); ok && !h.cleared(ctx, client, externalSessionID, home) {
		return sessionID, project, true
	}
	return sessionID, project, false
}

// cleared reports whether the conversation's clearance clears a write into
// dest; one that cannot be read clears nothing.
func (h *Handler) cleared(ctx context.Context, client Client, externalSessionID, dest string) bool {
	g, err := h.clearanceOf(ctx, client, externalSessionID)
	if err != nil {
		h.logger.Warn("hooks: load the conversation's clearance", "error", err)
		return false
	}
	return access.WriteCleared(access.WithGrants(ctx, g), dest)
}

// resumeScope is the project a resumed ambient row is re-scoped to
// (store.ReactivateAmbientSession: "" keeps its project). Off, it is project,
// as always. On, a resume carries the conversation, and everything it holds,
// into the cwd's project, so it re-scopes only where its home may write
// (store.CanWrite over its clearance): a conversation holding fenced content
// stays in its home, as session_start could not bind it elsewhere, and a
// conversation from an open home reaches a sealed project only by moving in. A
// lookup that fails keeps the home.
func (h *Handler) resumeScope(ctx context.Context, client Client, externalSessionID, project string) string {
	if !h.readGrants || h.db == nil || project == "" || externalSessionID == "" {
		return project
	}
	amb, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), externalSessionID)
	if err != nil {
		h.recordHookError(ctx, "home-lookup", client, err)
		return ""
	}
	if !found || amb.ProjectSlug == project {
		return project
	}
	g, err := h.clearanceOf(ctx, client, externalSessionID)
	if err != nil {
		h.logger.Warn("hooks: load the conversation's clearance", "error", err)
		return ""
	}
	ok, err := store.CanWrite(access.WithGrants(ctx, g), h.db, amb.ProjectSlug, project)
	if err != nil {
		h.logger.Warn("hooks: judge the resume's project", "project", project, "error", err)
		return ""
	}
	if !ok {
		return ""
	}
	return project
}

// startHome is the project a SessionStart's briefing describes with
// isolation.requests on, when it is not the cwd's: the home of the
// conversation the hook resumes (its ambient row, keyed on the client's
// session id) that the resume keeps there -- a move's child, or a
// conversation resumeScope does not re-scope -- and, for a subagent's start,
// which resumes nothing, the home it shares. ok is false with the switch off,
// for a new conversation, and for a resume into the cwd's project, whose
// briefing is the cwd's as always.
func (h *Handler) startHome(ctx context.Context, client Client, host string, p hookPayload) (string, bool) {
	if !h.readGrants || h.db == nil || p.SessionID == "" {
		return "", false
	}
	amb, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), p.SessionID)
	if err != nil {
		h.recordHookError(ctx, "home-lookup", client, err)
		return "", false
	}
	if !found || !access.SessionAccountOK(ctx, amb.Account) {
		return "", false
	}
	project, ok := h.startProject(ctx, host, p.CWD)
	if !ok || amb.ProjectSlug == project {
		return "", false
	}
	if p.AgentType == "" && amb.ParentSessionID == "" && project != "" &&
		h.resumeScope(ctx, client, p.SessionID, project) == project {
		return "", false // re-scoped to the cwd's project: its briefing
	}
	return amb.ProjectSlug, true
}

// startProject is the project a SessionStart places its session in, as
// ensureAmbientSessionIn does: the cwd's, under the caller's reach
// (access.SessionScope). ok is false where no session would be placed.
func (h *Handler) startProject(ctx context.Context, host, cwd string) (string, bool) {
	project, err := store.ResolveProjectForCWD(ctx, h.db, host, cwd)
	if err != nil {
		project = ""
	}
	placed, err := access.SessionScope(ctx, project)
	if err != nil {
		return "", false
	}
	return placed, true
}

// holderWord names who holds a conversation's content in a refusal: the
// agent's process tree under peer_auth, else the session.
func holderWord(ctx context.Context) string {
	if _, hasView := access.FromContext(ctx); hasView {
		return "agent"
	}
	return "session"
}

// whereWords names a destination in a refusal: "project <slug>", or the
// global scope.
func whereWords(project string) string {
	if project == "" {
		return "the global scope"
	}
	return "project " + project
}

// unreadClearanceNotice is the line for what was not recorded because the
// conversation's clearance could not be read.
func unreadClearanceNotice(what string) string {
	return "Seamless did not record " + what + ": the read grants of this connection could not be checked."
}
