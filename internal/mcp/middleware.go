package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// errNoSchema rejects a dispatched tool this server never registered a schema
// for. It is unreachable through addTool -- the same statement records both --
// and is reachable only via mcp-go's per-session tools, which this server does
// not use. It fails closed anyway: an unvalidatable call is exactly the "present
// but uninterpretable" case the validator exists to refuse, and passing it
// through would silently restore the old behavior for that one tool.
var errNoSchema = errors.New("internal error: no input schema registered for this tool")

// errNonObjectArgs rejects an arguments payload that is present but is not a JSON
// object ("arguments": []). Absent arguments are legitimate -- gardener_proposals
// takes none -- but a non-object payload cannot be read as one, and reading it as
// empty would drop every argument the caller sent while reporting success.
var errNonObjectArgs = errors.New("invalid arguments: expected a JSON object")

// validateMiddleware normalizes and validates a call's arguments against the
// tool's declared input schema, so the handler only ever sees arguments of the
// declared type, under their canonical names. mcp.Required and mcp.Enum are
// advertising that mcp-go never enforces (see argspec.go); this is what enforces
// them.
//
// Ordering (registered [auth, log, validate] in New, applied in reverse):
// auth(log(validate(handler))). Auth is outermost so an unauthorized caller gets
// no schema feedback; validate is inside log so a rejected call still lands in
// the Interactions feed. logMiddleware's TouchSession heartbeat therefore fires
// for rejected calls too, which is right -- a malformed call is still proof the
// agent is alive.
//
// It REPLACES the arguments rather than mutating the map in place. mcp-go passes
// CallToolRequest by value, so assigning to req.Params.Arguments here rebinds
// only this frame's copy and hands the copy downstream; the map itself is still
// shared, which is the whole reason not to write into it:
//
//   - logMiddleware reads req.GetArguments() from ITS OWN copy after next()
//     returns, so replacement leaves it looking at the RAW arguments. That is
//     what we want -- the feed is evidence, not outcome. If it showed normalized
//     arguments, a call that sent depends_on:"a,b" would appear as ["a","b"] and
//     "this agent still emits CSV" would be undiagnosable from the feed.
//   - mcp-go hands the ORIGINAL request to its afterCallTool hooks, which
//     in-place mutation would leak normalized arguments into.
func (s *Server) validateMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		schema, ok := s.toolSchema(req.Params.Name)
		if !ok {
			return errResult(req.Params.Name, errNoSchema)
		}
		raw, ok := argumentsMap(req)
		if !ok {
			return errResult(req.Params.Name, errNonObjectArgs)
		}
		raw, err := dropRetired(req.Params.Name, raw)
		if err != nil {
			return errResult(req.Params.Name, err)
		}
		normalized, err := normalizeArgs(schema, raw)
		if err != nil {
			return errResult(req.Params.Name, err)
		}
		req.Params.Arguments = normalized
		return next(ctx, req)
	}
}

// argumentsMap returns a call's arguments as a map, reporting false for a payload
// that is present but is not a JSON object.
//
// Absent arguments, an explicit JSON null, and a typed-nil map all mean "no
// arguments" and yield a nil map, which normalizeArgs reads as empty while still
// running the required check -- calling gardener_proposals with no arguments is
// legitimate. Only a non-object payload is uninterpretable.
func argumentsMap(req mcp.CallToolRequest) (map[string]any, bool) {
	switch a := req.Params.Arguments.(type) {
	case nil:
		return nil, true
	case map[string]any:
		return a, true
	default:
		return nil, false
	}
}

// logMiddleware records one tool.call event per invocation for the console's
// Interactions feed: the tool name, its (truncated) arguments, the (truncated)
// result text or error, and the wall-clock duration. It runs INNER of
// authMiddleware -- registered after it in New(), and mcp-go applies middlewares
// in reverse registration order -- so an unauthorized call is rejected before it
// reaches here and is never logged. Attribution is read AFTER next() so that a
// handler's stashed target is visible: session_start's own event carries the
// session it just bound, session_end's the session it just completed. Logging is
// best-effort: a recorder failure never affects the tool's result.
func (s *Server) logMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Plant the pre-allocated attribution slot the session handlers write the
		// call's resolved target into (stashAttribution). It must go in BEFORE
		// next(): a context value set inside the handler could not propagate out,
		// so the slot is planted here and mutated in place.
		ctx = context.WithValue(ctx, attributionSlotKey{}, &attributionSlot{})
		// The per-call memo for the process binding (agent_binding.go): planted
		// here so the handler's scope/fence lookups and this middleware's own
		// attribution read share one query. session_end, the one handler that
		// ends a session mid-call, stashes its target, which attribution reads
		// first -- so a memo naming the session it just ended is never consulted.
		ctx = context.WithValue(ctx, processSessionSlotKey{}, &processSessionSlot{})
		// The session= handle's slot (session_handle.go), filled by
		// sessionHandleMiddleware further in, so the attribution read below
		// credits the session a call named as well as the one it was bound to.
		ctx = context.WithValue(ctx, sessionHandleSlotKey{}, &sessionHandleSlot{})
		// The tool's name, for what a handler records about the call itself (a
		// fence refusal's access.refused names the tool, never its arguments).
		ctx = context.WithValue(ctx, toolNameKey{}, req.Params.Name)
		start := time.Now()
		result, err := next(ctx, req)
		durMS := time.Since(start).Milliseconds()

		// Attribution doubles as the session heartbeat: any tool call is proof the
		// agent is alive, so bump the attributed session's updated_at to keep the
		// idle reaper from expiring it. Covers stashed targets, bound (explicit
		// session_start) and ambient-fallback sessions alike; the active-only guard
		// in TouchSession makes it a no-op for a just-ended one -- and because a
		// stashed target wins attribution, ending one agent's session never bumps a
		// surviving sibling's liveness. Best-effort.
		sessionID, project := s.attribution(ctx)
		if sessionID != "" {
			if terr := store.TouchSession(ctx, s.cfg.DB, sessionID, time.Now().UTC()); terr != nil {
				s.logger.Warn("mcp: session heartbeat", "session", sessionID, "error", terr)
			}
		}

		if s.cfg.Events == nil {
			return result, err
		}
		max := s.cfg.ToolEventMaxChars

		payload := map[string]any{
			"tool":        req.Params.Name,
			"duration_ms": durMS,
		}
		if args := truncateArgs(req.GetArguments(), max); len(args) > 0 {
			payload["args"] = args
		}
		switch {
		case err != nil:
			// Exceptional: a recovered panic surfaced as a Go error. Record the
			// error with no result body.
			payload["is_error"] = true
			payload["error"] = firstLine(err.Error())
		case result != nil:
			text := contentText(result)
			if t := events.Truncate(text, max); t != "" {
				payload["result"] = t
			}
			if result.IsError {
				payload["is_error"] = true
				payload["error"] = firstLine(text)
			}
		}
		s.record(ctx, core.EventToolCall, sessionID, project, "", payload)
		return result, err
	}
}

// toolNameKey keys the called tool's name, planted by logMiddleware.
type toolNameKey struct{}

// attributionSlotKey keys the pre-allocated attribution slot logMiddleware
// plants into the call context before running the handler.
type attributionSlotKey struct{}

// attributionSlot carries the session a handler resolved as the call's target
// out to logMiddleware's post-next() attribution read. The middleware
// pre-allocates it and the handler mutates it in place -- a context value set
// inside the handler could not propagate back out. The hand-off is
// same-goroutine (the middleware chain is synchronous), so no locking.
type attributionSlot struct {
	sessionID string
	project   string
}

// stashAttribution records the session a handler actually operated on, so the
// logged tool.call is attributed to it rather than to whatever the connection
// binding or ambient fallback would guess after the handler ran. session_end is
// the sharp case: the handler completes its target and evicts its bindings, so
// by the time logMiddleware reads attribution the target is invisible to both
// the binding and the active-only ambient lookup -- the stamp (and heartbeat)
// would land on a surviving sibling session instead. No-op outside the
// middleware chain (no slot in ctx).
func stashAttribution(ctx context.Context, sessionID, project string) {
	if slot, ok := ctx.Value(attributionSlotKey{}).(*attributionSlot); ok {
		slot.sessionID = sessionID
		slot.project = project
	}
}

// attribution resolves the (sessionID, project) a logged tool.call should carry:
// the session the handler stashed (the call's actual target -- see
// stashAttribution), else the connection's binding, else a single unambiguous
// ambient session, else empty. It never errors -- an unattributable call
// (stateless transport, or before any session_start) simply lands in the feed's
// "unattributed" tab. It is read after the handler runs so a stashed target is
// visible.
func (s *Server) attribution(ctx context.Context) (sessionID, project string) {
	if slot, ok := ctx.Value(attributionSlotKey{}).(*attributionSlot); ok && slot.sessionID != "" {
		return slot.sessionID, slot.project
	}
	if b, ok := s.getBinding(ctx); ok {
		return b.sessionID, b.project
	}
	if sess, ok, _, _ := s.ambientFallback(ctx); ok { //nolint:errcheck // provenance is best-effort: a fenced refusal leaves the call unattributed rather than misattributed, which is the outcome this branch already falls through to
		return sess.ID, sess.ProjectSlug
	}
	return "", ""
}

// truncateArgs copies args, truncating string values to max runes (keeping the
// stored JSON small yet valid) and leaving non-string values untouched. A nil or
// empty map yields nil.
func truncateArgs(args map[string]any, max int) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if sv, ok := v.(string); ok {
			out[k] = events.Truncate(sv, max)
		} else {
			out[k] = v
		}
	}
	return out
}

// contentText concatenates the text parts of a tool result (usually just one) --
// the request/response body the Interactions feed shows.
func contentText(result *mcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// firstLine returns the first line of s, trimmed -- a compact error label for the
// feed's summary while the full text stays in the result field.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
