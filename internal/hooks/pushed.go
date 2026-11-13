package hooks

// Pushed captures (peer_auth). Under peer_auth the daemon runs as its own OS
// account and opens no path a hook names (localDiskOK): a transcript_path, a
// plan file or a rollout is a path some other account chose, and the daemon
// can read its own data directory. So the five captures that read the agent's
// disk -- the session-end findings and token totals, the model sniff, plan and
// subagent capture, and the git stamp -- move to the client: `seam hook`,
// running as the owner beside the agent, reads the files with the same parsers
// (internal/harvest) and pushes what it found in the hook body
// (harvest.Field). The daemon still opens nothing.
//
// A push is the caller's own claim, like session_update's findings, and lands
// only through the fences every other hook write meets:
//
//   - The session the hook names decides first (sessionReachOf): another
//     account's session takes nothing -- nor does one another principal
//     started (a restricted local account's for the owner, the owner's for a
//     restricted account) -- and the owner's session in a locked project
//     takes metadata only -- the heartbeat, the model, the completion --
//     until the owner approves a lease for the pushing process's tree. A
//     capture refused there is answered with a one-line notice
//     (harvest.RefusedHeader, and the context of a hook that has one) and is
//     never retried: the next capture after the approval carries the current
//     state.
//   - A plan or subagent note lands in the project its cwd resolves to, placed
//     the way a session is (View.SessionScope): the owner's only where it may
//     write, a restricted account's only in an open project it
//     reaches (anywhere else it is dropped without a word, as a closed
//     project must read like a missing one), labeled with its account. Its
//     events are filed under the session's project, so a note in a leased
//     locked project is accepted only from a session recorded in that same
//     project: never a locked project's content in another project's
//     records.
//
// With peer_auth off no request carries a View, so pushedCapture never looks
// at the body, every payload's Capture stays nil, and every hook runs exactly
// the code it ran before.

import (
	"context"
	"net/http"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/harvest"
)

// pushedCapture is the capture `seam hook` pushed in body, read only on a
// request that carries a peer_auth View; nil otherwise -- with peer_auth off
// without the body being looked at. A push that does not decode or validate
// is dropped whole and recorded (stage capture-push-invalid), never applied in
// part; the hook still answers.
func (h *Handler) pushedCapture(ctx context.Context, client Client, body []byte) *harvest.Push {
	if _, on := access.FromContext(ctx); !on {
		return nil
	}
	push, ok, err := harvest.DecodePush(body)
	if err != nil {
		h.recordHookError(ctx, "capture-push-invalid", client, err)
		return nil
	}
	if !ok {
		return nil
	}
	return &push
}

// pushedModel is the model a hook stamps: the one the client stated (Codex
// sends it), else the one `seam hook` sniffed and pushed. A pushed model is
// metadata, exactly like a stated one, so a locked project's session takes it.
func pushedModel(stated string, push *harvest.Push) string {
	if stated == "" && push != nil {
		return push.Model
	}
	return stated
}

// pushCarries reports whether push holds anything a locked session refuses.
func pushCarries(push *harvest.Push) bool {
	return push != nil && push.Carries()
}

// pushVerdict is what the fences say about a pushed note capture.
type pushVerdict struct {
	ok bool // the capture lands
	// notice is the one-line refusal for a capture refused at a lock; "" for a
	// silent refusal (a session or project out of the caller's reach: nothing
	// about it is the caller's to learn).
	notice string
}

// fenceNotePush judges a pushed plan or subagent capture headed for
// route.project -- the project its hook's cwd resolves to, or the isolated
// project its conversation's record goes to (captureTarget) -- from the
// session the hook named: its reach (sessionReachOf, before any blanking), the
// locked project that reach found, and its id (blank when the reach is not
// reachOK). what names the capture in the notice. A capture that passes lands
// in project: the target, placed as a session would be (View.SessionScope).
func (h *Handler) fenceNotePush(ctx context.Context, client Client, reach sessionReach, lockedProject, sessionID string, route captureRoute, what string) (project string, verdict pushVerdict) {
	v, _ := access.FromContext(ctx)
	target := route.project
	switch {
	case reach == reachLocked:
		return "", pushVerdict{notice: captureRefusedNotice(what, lockedProject)}
	case reach == reachForeign:
		return "", pushVerdict{}
	case v.Locked(target):
		return "", pushVerdict{notice: captureRefusedNotice(what, target)}
	}
	// The View's own placement, never access.SessionScope: a capture files no
	// access request.
	project, err := v.SessionScope(target)
	if err != nil {
		return "", pushVerdict{} // out of reach: nothing about it is the caller's to learn
	}
	// The capture's events are filed under the session captureRef names: the
	// ambient row's, or a routed capture's own.
	if _, leased := v.Lease(project); leased {
		if _, sessionProject := h.captureRef(ctx, client, sessionID, route); sessionProject != project {
			return "", pushVerdict{notice: crossProjectNotice(what, project)}
		}
	}
	if refusal := h.captureConfined(ctx, client, sessionID, project, what); refusal != "" {
		return "", pushVerdict{notice: refusal}
	}
	return project, pushVerdict{ok: true}
}

// captureRefusedNotice is the line a capture refused at a lock is answered
// with: what was not recorded and why, then what to do. project is the locked
// one ("" when the refusal could not name it).
func captureRefusedNotice(what, project string) string {
	if project == "" {
		return "Seamless did not record " + what + ": this session's project is locked and the session holds no " +
			"approval; the next capture after the owner approves carries the current state."
	}
	return "Seamless did not record " + what + ": project " + project + " is locked and this session holds no " +
		"approval; the next capture after the owner approves carries the current state " +
		"(to ask: `seam access request " + project + "`)."
}

// crossProjectNotice is the line a pushed note capture into a leased locked
// project is refused with when the session is recorded elsewhere: the note's
// events would carry the locked project's content into that other project.
func crossProjectNotice(what, project string) string {
	return "Seamless did not record " + what + ": it would land in locked project " + project +
		" from a session recorded in another project; capture it from a session started in " + project + "."
}

// refuseCapture sets the refusal header on a hook response: one line, which
// `seam hook` prints on stderr. It must run before the response is written.
func (h *Handler) refuseCapture(w http.ResponseWriter, notice string) {
	w.Header().Set(harvest.RefusedHeader, notice)
}

// refusalContext is the refusal as the context of a hook that can carry one
// (PostToolUse).
func refusalContext(notice string) preparedHookContext {
	// Plan capture is Claude Code-only (Codex registers no plan-capture hooks).
	return prepareHookContext(ClientClaudeCode, "<seam-plan-context>\n"+notice+"\n</seam-plan-context>")
}
