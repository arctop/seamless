package mcp

import (
	"context"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Process binding: how a connection reaches the session its own agent's hook
// created, without a session_start call.
//
// A Claude Code or Codex session reaches the daemon on two channels that share
// nothing on the wire. The SessionStart hook creates the ambient session; the
// MCP connection carries the agent's tool calls under a transport id that names
// no client (memory mcp-session-id-opaque-and-restart-unstable). Until this
// binding existed every connection therefore started unbound, and any call that
// needed a project or a session had to infer one from whichever agents were
// active -- an inference that refuses, rightly, as soon as agents work in two
// repos at once. Most of the agents' tool errors were that refusal, and their
// attempts to cure it with session_start were most of the rest.
//
// What the two channels share is the process that spawned them. `seam hook`
// names it on session-start and the daemon stamps it on the ambient session;
// `seam mcp-proxy` / `seam mcp-headers` / the CLI dial name it on every request
// (see internal/agentproc). A request naming the process that exactly one live
// ambient session is stamped with belongs to that session.
//
// This is identification, not inference: nothing is chosen from circumstantial
// evidence such as recency, so it binds like session_start does -- the isolation
// fence judges the call against this session's project, and a confidential
// project's own agents can read it without a ritual. It is also durable in a way
// the in-memory binding is not: it lives in the session row and on every
// request, so it survives a daemon restart and a client re-initialize alike.

// AgentProcessHeader names the agent process (agentproc.Anchor) that launched
// the MCP client: Claude Code's headersHelper output, the mcp-proxy bridge and
// the seam CLI's dial all send it. A client that sends none -- an older seam, or
// any client Seamless did not launch -- binds only through session_start, as
// before.
const AgentProcessHeader = "X-Seamless-Agent-Process"

type agentProcessKey struct{}

// callerAgentProcess is the agent process identity this call's connection
// named, or "". Handler stores only a well-formed one.
func callerAgentProcess(ctx context.Context) string {
	proc, _ := ctx.Value(agentProcessKey{}).(string)
	return proc
}

// processSessionSlotKey keys the per-call memo logMiddleware plants.
type processSessionSlotKey struct{}

// processSessionSlot memoizes processSession for one tool call. Scope and
// isolation resolution consult the binding several times per call, and each
// lookup is a query; the answer cannot change within a call that has not
// changed it, so it is asked once. Same-goroutine hand-off, like
// attributionSlot, so no lock.
type processSessionSlot struct {
	done bool
	sess core.Session
	ok   bool
}

// processSession returns the live ambient session the calling agent process
// owns: exactly one active ambient session on the caller's host stamped with the
// identity this connection named -- or, when there is none, the one the idle
// reaper expired while the agent was away, revived (reviveProcessSession). No
// row (no identity sent, a session already ended, an older hook) and several
// rows (a process hosting more than one live session -- a Claude Code process
// runs one at a time and ends the previous one on /clear and /resume, so this is
// some other host whose calls cannot be pinned) both report false, and the
// caller falls back to everything that applied before. A lookup failure is
// logged and reports false for the same reason: the binding is an improvement on
// the fallback, never a new way to fail.
func (s *Server) processSession(ctx context.Context) (core.Session, bool) {
	proc := callerAgentProcess(ctx)
	if proc == "" || s.cfg.DB == nil {
		return core.Session{}, false
	}
	slot, memo := ctx.Value(processSessionSlotKey{}).(*processSessionSlot)
	if memo && slot.done {
		return slot.sess, slot.ok
	}
	sess, ok := s.lookupProcessSession(ctx, proc)
	// The agent-process header is the client's own claim. Under peer_auth it may
	// name only a session this connection's OS account may reach: a forged
	// identity pointing at another account's agent binds nothing.
	if ok && !access.Visible(ctx, sess.ProjectSlug) {
		sess, ok = core.Session{}, false
	}
	if memo {
		slot.done, slot.sess, slot.ok = true, sess, ok
	}
	return sess, ok
}

func (s *Server) lookupProcessSession(ctx context.Context, proc string) (core.Session, bool) {
	host := s.callerHost(ctx)
	sessions, err := store.ActiveAmbientByAgentProcess(ctx, s.cfg.DB, host, proc)
	if err != nil {
		s.logger.Warn("mcp: process binding lookup", "error", err)
		return core.Session{}, false
	}
	switch len(sessions) {
	case 1:
		return sessions[0], true
	case 0:
		return s.reviveProcessSession(ctx, host, proc)
	default:
		return core.Session{}, false
	}
}

// reviveProcessSession is the process binding for an agent whose session the
// idle reaper expired while it was away: this call, from the same live process,
// is proof of life, so the session is reactivated and bound
// (store.ReviveExpiredAgentSession says which sessions qualify). The revival is
// recorded as the mirror of the reaper's session.ended, so the console's
// timeline shows the session coming back rather than a heartbeat from nowhere.
// If a concurrent call revived it first, the live row is simply read again.
func (s *Server) reviveProcessSession(ctx context.Context, host, proc string) (core.Session, bool) {
	sess, ok, err := store.ReviveExpiredAgentSession(ctx, s.cfg.DB, host, proc, time.Now().UTC())
	if err != nil {
		s.logger.Warn("mcp: process binding revive", "error", err)
		return core.Session{}, false
	}
	if ok {
		s.record(ctx, core.EventSessionStarted, sess.ID, sess.ProjectSlug, "",
			map[string]any{"resumed": true, "revived": true, "agent_process": true})
		return sess, true
	}
	sessions, err := store.ActiveAmbientByAgentProcess(ctx, s.cfg.DB, host, proc)
	if err != nil || len(sessions) != 1 {
		return core.Session{}, false
	}
	return sessions[0], true
}
