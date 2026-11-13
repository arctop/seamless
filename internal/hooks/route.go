package hooks

// Where a conversation's own record goes. A Claude Code or Codex agent that
// binds itself to a confidential or sealed project with `session_start` goes
// on working from its own cwd, and every hook it fires still names its ambient
// row -- in the cwd's project, which may be open. Its SessionEnd harvest,
// Codex's Stop findings and its plan and subagent captures would carry the
// isolated project's content into that project. store.RecordDestinationOf
// decides where they go instead -- the ambient row as always (also for a
// conversation native to an isolated project, whatever it binds to), the
// isolated session the conversation bound itself to, or nowhere when its
// links reach more than one isolated project -- and the ambient row is then
// left a pointer, metadata only. The policy lives in
// internal/store/isolation.go: nothing here tests an isolation state. The View
// still has its word on every write into a link (access.Writable), as it has
// on every other hook write.

import (
	"context"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// captureRoute is where one plan or subagent capture lands (captureTarget):
// project takes the note. A routed capture's events and plan metadata are
// filed under link, the isolated session its conversation bound itself to; an
// unrouted one's go to the ambient row, as always.
type captureRoute struct {
	project string
	routed  bool
	link    core.Session
}

// captureTarget is where a plan or subagent capture for the conversation
// externalSessionID names lands, before the fences judge it: the project its
// cwd resolves to, as always, or -- once a conversation whose ambient row is in
// no isolated project bound itself to one -- that project, for the rest of the
// conversation, whatever it binds to afterwards. notice is non-empty when the
// capture lands nowhere: the conversation's links reach more than one isolated
// project, or where its record goes could not be read.
func (h *Handler) captureTarget(ctx context.Context, client Client, externalSessionID, host, cwd, what string) (captureRoute, string) {
	if h.db == nil {
		return captureRoute{}, "" // no capture runs without a database (captureEnabled)
	}
	if externalSessionID == "" {
		return captureRoute{project: h.resolveProject(ctx, host, cwd)}, ""
	}
	dest, err := store.RecordDestinationOf(ctx, h.db, client.externalIdentity(), externalSessionID)
	if err != nil {
		h.recordHookError(ctx, "capture-record-destination", client, err)
		return captureRoute{}, "Seamless did not record " + what + ": where this conversation's record goes could not be checked."
	}
	switch dest.Kind {
	case store.RecordDestLink:
		return captureRoute{project: dest.Session.ProjectSlug, routed: true, link: dest.Session}, ""
	case store.RecordDestWithheld:
		return captureRoute{}, withheldNotice(what, dest.Fenced)
	}
	return captureRoute{project: h.resolveProject(ctx, host, cwd)}, ""
}

// captureRef is the session and project a capture's events are filed under:
// the link a routed capture went to, else the ambient row (ambientRef), as
// always. A routed capture's events carry its content, so filing them under an
// open project's ambient row would carry that content out after all.
func (h *Handler) captureRef(ctx context.Context, client Client, externalSessionID string, route captureRoute) (sessionID, project string) {
	if route.routed {
		return route.link.ID, route.link.ProjectSlug
	}
	return h.ambientRef(ctx, client, externalSessionID)
}

// planSession is the session a capture's plan metadata lives on
// (plans_session.go): the link a routed capture went to, read again so the
// metadata is current, else the ambient row, as always. The metadata names the
// plan's composition slug, which a routed plan's title chose.
func (h *Handler) planSession(ctx context.Context, claudeSessionID string, route captureRoute) (core.Session, bool) {
	if !route.routed {
		return h.ambientSession(ctx, claudeSessionID)
	}
	if h.db == nil {
		return core.Session{}, false
	}
	sess, ok, err := store.SessionByID(ctx, h.db, route.link.ID)
	if err != nil {
		h.logger.Warn("hooks: linked session lookup", "error", err)
		return core.Session{}, false
	}
	return sess, ok
}

// routedPointer is what the ambient row's findings become once its
// conversation's record goes to an isolated session: where it goes, and
// nothing of what it holds.
func routedPointer(project string) string {
	return "Continued in project " + project + "; this conversation's findings go there, not here."
}

// withheldLine is the findings of a row that a conversation holding more than
// one isolated project ends with: why it records nothing, naming the projects
// and nothing they hold.
func withheldLine(fenced []string) string {
	return "Not recorded: this conversation was bound to more than one isolated project (" +
		strings.Join(fenced, ", ") + "), and its record goes to none of them."
}

// withheldNotice is the one-line notice (harvest.RefusedHeader) for what such
// a conversation's hook did not record.
func withheldNotice(what string, fenced []string) string {
	return "Seamless did not record " + what + ": this conversation was bound to more than one isolated project (" +
		strings.Join(fenced, ", ") + "), and its record goes to none of them."
}

// hookFindings reports whether findings are the hooks' own -- none yet, the
// no-summary sentinel, or an auto-harvest -- which the conversation's harvest
// may replace. Anything else the agent wrote itself, with session_update or
// session_end, and it is kept.
func hookFindings(findings string) bool {
	f := strings.TrimSpace(findings)
	return f == "" || f == harvestFallback || strings.HasPrefix(f, strings.TrimSpace(harvest.Prefix))
}

// endFindings is what sess's findings become when its conversation ends, given
// where the conversation's record goes and the harvest the end read: the
// harvest itself (took), a metadata-only line, or -- ok false -- the findings
// it already has.
//
//   - Nothing routed: the ambient row takes the harvest and every explicit
//     session keeps its own findings, as always.
//   - Routed to a link: that link takes the harvest when linkOK (linkTakes)
//     and the agent did not write its findings itself, and the ambient row
//     takes the pointer.
//   - Withheld: nothing takes it. The ambient row, and every isolated link
//     whose findings are the hooks' own, takes the line saying why.
func endFindings(dest store.RecordDestination, linkOK bool, sess core.Session, harvest func() string) (findings string, took, ok bool) {
	switch dest.Kind {
	case store.RecordDestLink:
		switch {
		case sess.Ambient:
			return routedPointer(dest.Session.ProjectSlug), false, true
		case linkOK && sess.ID == dest.Session.ID && hookFindings(sess.Findings):
			return harvest(), true, true
		}
	case store.RecordDestWithheld:
		if sess.Ambient || (dest.Holds(sess.ProjectSlug) && hookFindings(sess.Findings)) {
			return withheldLine(dest.Fenced), false, true
		}
	default:
		if sess.Ambient {
			return harvest(), true, true
		}
	}
	return "", false, false
}

// linkWritable reports whether the hook may write into link, an isolated
// session the conversation's record goes to: the link must be the principal's
// own and its project one the caller may write (the View; with peer_auth off
// both are always true). notice is the refusal line when it may not because the
// project is locked to the caller, "" otherwise -- nothing about another
// principal's session is the caller's to learn.
func linkWritable(ctx context.Context, link core.Session, what string) (ok bool, notice string) {
	if !access.SessionAccountOK(ctx, link.Account) {
		return false, ""
	}
	if access.Writable(ctx, link.ProjectSlug) {
		return true, ""
	}
	if access.Locked(ctx, link.ProjectSlug) {
		return false, captureRefusedNotice(what, link.ProjectSlug)
	}
	return false, ""
}

// linkTakes reports whether link, the isolated session the conversation's
// record goes to, may take the harvest: linkWritable, and no read grant
// confines the conversation's writes away from its project (captureConfined,
// isolation.requests), as a capture's own target is judged. A harvest it may
// not take lands nowhere: anywhere else would carry the link's project's
// content out of it. notice is the refusal line, "" when there is nothing the
// caller may learn.
func (h *Handler) linkTakes(ctx context.Context, client Client, externalSessionID string, link core.Session, what string) (ok bool, notice string) {
	if ok, notice := linkWritable(ctx, link, what); !ok {
		return false, notice
	}
	if refusal := h.captureConfined(ctx, client, externalSessionID, link.ProjectSlug, what); refusal != "" {
		return false, refusal
	}
	return true, ""
}
