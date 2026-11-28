package mcp

import (
	"context"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// markFirstReuse records the momentum "first reuse" moment: the first time a
// memory is read by a session other than the one that wrote it -- the point
// where stored knowledge visibly pays off. Latched once per memory ever via
// events.RecordOnce. Best-effort by design: a failure here must never fail
// the read that triggered it.
//
// The read history decides, not the latch alone: memory_read runs this
// BEFORE it records its own memory.read, and a memory another session has
// already read was reused then, so this read is not its first and mints
// nothing. That keeps the claim true for reads made before first reuse was
// recorded at all (an install from before momentum was core), and spares
// every later read the latch's write. Two first foreign reads racing both
// find no history; RecordOnce lets one of them mint.
func (s *Server) markFirstReuse(ctx context.Context, mem core.Memory) {
	if s.cfg.Events == nil || s.cfg.DB == nil {
		return
	}
	// Reuse needs a known writer and a different, known reader. source_session
	// carries the writer's session ULID as this server stamps it -- but v1
	// imports carry session NAMES there, so the reader matches on either
	// identity before the moment counts as someone else's.
	if mem.SourceSession == "" {
		return
	}
	readerID := s.boundSession(ctx)
	if readerID == "" || readerID == mem.SourceSession {
		return
	}
	reader := s.boundSessionName(ctx)
	if reader == mem.SourceSession {
		return
	}
	if reader == "" {
		reader = readerID
	}
	reused, err := store.MemoryReadByOthers(ctx, s.cfg.DB, mem.ID, mem.SourceSession)
	if err != nil {
		s.logger.Warn("momentum: first-reuse history", "memory", mem.Name, "error", err)
		return
	}
	if reused {
		return
	}
	_, recorded, err := s.cfg.Events.RecordOnce(ctx, core.Event{
		Kind: core.EventMemoryFirstReuse, SessionID: readerID,
		ProjectSlug: mem.Project, ItemID: mem.ID,
		Payload: map[string]any{
			"name": mem.Name, "kind": string(mem.Kind),
			"writer": mem.SourceSession, "reader": reader,
		},
	})
	if err != nil {
		s.logger.Warn("momentum: first-reuse mark", "memory", mem.Name, "error", err)
		return
	}
	if recorded {
		s.logger.Info("momentum: first reuse", "memory", mem.Name, "reader", reader)
	}
}
