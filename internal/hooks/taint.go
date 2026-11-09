package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// The outside-text mark (the screening optional feature).
//
// An agent guard -- a PreToolUse/PostToolUse hook that knows which tools
// bring outside text into a session (a web fetch, a search, a mail reader) --
// POSTs here after one runs, and the session is marked: from then on its
// writes of a briefing-injected kind (constraint, convention, protocol, stage)
// are held for the owner instead of landing. It authenticates like every
// hook: the static key on TCP, or the kernel-named peer on the peer_auth
// socket, which is what lets a hook process call it with the credentials it
// already has.
//
// Setting the mark only lowers trust, so the session's own hook may set it.
// Clearing it is the owner's, on the console; there is no endpoint, flag or
// MCP tool that clears it.
//
// The body names the session one of two ways: the client's own session id
// (session_id, the field every client hook payload carries -- so a hook can
// forward its stdin unchanged) with ?client= naming the client, or a Seamless
// session (session: a ULID or a cc/... / cx/... name). reason is an optional
// short code (web_fetch, mail, ...); absent, it is the payload's tool_name
// ("tool:webfetch"), or "outside_text" when that is absent too. `seam hook
// session-taint` forwards a hook's stdin here with the usual config, key,
// socket and retry handling.

// defaultTaintReason is the mark's reason when the caller names none.
const defaultTaintReason = "outside_text"

// taintReasonRe bounds a reason to a short machine code: it is shown on the
// console and recorded in an event, and never carries free text.
var taintReasonRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,63}$`)

type taintPayload struct {
	SessionID string `json:"session_id"` // the client's own session id
	Session   string `json:"session"`    // a Seamless session ULID or name
	Reason    string `json:"reason"`
	// ToolName is the hook payload's own field: a guard forwarding its
	// PostToolUse stdin unchanged names the tool, which becomes the reason.
	ToolName string `json:"tool_name"`
}

// toolReason turns a hook payload's tool name into a reason code
// ("tool:webfetch"), or "" when it carries none that fits.
func toolReason(tool string) string {
	tool = strings.ToLower(strings.TrimSpace(tool))
	if tool == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("tool:")
	for _, r := range tool {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// TaintHeader carries a session-taint call's outcome. The body is always the
// plain hook acknowledgement, because `seam hook session-taint` copies it to
// the calling hook's stdout, which its client parses as hook output; the
// outcome rides here and in the status code instead.
const TaintHeader = "X-Seamless-Taint"

// Session-taint outcomes (TaintHeader values).
const (
	taintMarked  = "marked"          // this call set the mark (200)
	taintAlready = "already-marked"  // the session already carried it (200)
	taintOff     = "off"             // write screening is off: nothing marked (200)
	taintUnknown = "unknown-session" // no such session, or one this caller may not reach (404)
	taintInvalid = "invalid"         // a malformed request (400)
	taintFailed  = "failed"          // the daemon could not record it (500)
)

func (h *Handler) sessionTaint(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHookBody)
	var p taintPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeTaintOutcome(w, http.StatusBadRequest, taintInvalid)
		return
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		reason = toolReason(p.ToolName)
	}
	if reason == "" {
		reason = defaultTaintReason
	}
	extID, ref := strings.TrimSpace(p.SessionID), strings.TrimSpace(p.Session)
	// A reason is a short code, never free text; and the session is named
	// exactly one way.
	if !taintReasonRe.MatchString(reason) || (extID == "") == (ref == "") {
		writeTaintOutcome(w, http.StatusBadRequest, taintInvalid)
		return
	}
	// Both ways of naming a session read inside the caller's namespace: the
	// client's id here, a Seamless name in store.SessionByName.
	extID = namespacedExternalID(r, extID)
	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	if h.screening == nil || !h.screeningOn(ctx) {
		// Off means off: nothing is marked, and the caller is told so rather
		// than shown a success it did not get.
		writeTaintOutcome(w, http.StatusOK, taintOff)
		return
	}
	sess, found, err := h.taintTarget(ctx, client, extID, ref)
	if err != nil {
		h.logger.Warn("hooks: session taint lookup", "error", err)
		writeTaintOutcome(w, http.StatusInternalServerError, taintFailed)
		return
	}
	if !found {
		writeTaintOutcome(w, http.StatusNotFound, taintUnknown)
		return
	}
	first, err := h.screening.Taint(ctx, sess, reason, "hook")
	if err != nil {
		h.logger.Warn("hooks: session taint", "session_id", sess.ID, "error", err)
		writeTaintOutcome(w, http.StatusInternalServerError, taintFailed)
		return
	}
	if first {
		writeTaintOutcome(w, http.StatusOK, taintMarked)
		return
	}
	writeTaintOutcome(w, http.StatusOK, taintAlready)
}

// writeTaintOutcome answers a session-taint call: the outcome in TaintHeader
// and the status, the plain hook acknowledgement as the body.
func writeTaintOutcome(w http.ResponseWriter, status int, outcome string) {
	w.Header().Set(TaintHeader, outcome)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // the status line is sent: a failed body write cannot be reported.
	_ = json.NewEncoder(w).Encode(hookResponse{Continue: true, SuppressOutput: true})
}

// taintTarget resolves the session a taint call names, under the caller's
// peer_auth reach: another account's session is not found, exactly as every
// other hook treats it (foreignSession) -- a mark across the seal would let
// one account hold another's writes.
func (h *Handler) taintTarget(ctx context.Context, client Client, extID, ref string) (core.Session, bool, error) {
	if h.db == nil {
		return core.Session{}, false, errors.New("no database")
	}
	var sess core.Session
	var found bool
	var err error
	switch {
	case extID != "":
		if reach, _ := h.sessionReachOf(ctx, client, extID); reach == reachForeign {
			return core.Session{}, false, nil
		}
		sess, found, err = store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), extID)
	default:
		sess, found, err = store.SessionByID(ctx, h.db, ref)
		if err == nil && !found {
			sess, found, err = store.SessionByName(ctx, h.db, ref)
		}
	}
	if err != nil || !found {
		return core.Session{}, false, err
	}
	// A session in a project locked to the caller is reached the way every
	// hook reaches one (reachLocked): from the agent's own process chain. The
	// owner's agent in a locked project's repository before its lease
	// (lockedStart records the session there) is exactly the session whose
	// approval must show the mark, and its own guard runs with no lease. The
	// mark reads nothing from the project and writes nothing into it.
	if v, ok := access.FromContext(ctx); ok && !v.Writable(sess.ProjectSlug) &&
		(!v.Locked(sess.ProjectSlug) || !h.sessionInChain(ctx, v, sess.ID)) {
		return core.Session{}, false, nil
	}
	// A session another principal started is not found either, whichever way
	// it was named: a restricted account and the owner share the open
	// projects, and a mark on the other's session would hold its writes.
	if !access.SessionAccountOK(ctx, sess.Account) {
		return core.Session{}, false, nil
	}
	return sess, true, nil
}

// screeningOn resolves the screening feature live, like every other surface:
// the file/env base with the console's stored override over it. Failure-soft
// to the base.
func (h *Handler) screeningOn(ctx context.Context) bool {
	cfg, _, err := store.FeaturesConfig(ctx, h.db, h.features)
	if err != nil {
		h.logger.Warn("hooks: features override unreadable; using the file/env config", "error", err)
		cfg = h.features
	}
	return features.Enabled(cfg, features.Screening)
}
