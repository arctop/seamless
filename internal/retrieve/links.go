package retrieve

import (
	"context"

	"github.com/arctop/seamless/internal/core"
)

// expandLinks scans the top fused memory hits for [[name]] links and adds each
// referenced, in-scope, active memory to acc as a "linked" candidate, returning
// the neighbor ids added (so the caller can hydrate them). It contributes a
// modest RRF-style score by discovery order, so a linked neighbor competes as if
// it were another source's low-ranked hit -- surfaced, but below genuine matches.
// It is a no-op without a body reader (index rows carry no body to scan).
//
// globalVisible carries the caller's isolation fence: a sealed project may not
// read the global scope, so neither its already-fused hits nor a [[name]] that
// resolves only globally may enter through here. Link expansion is the one
// path that resolves a name the candidate queries never saw, so it has to
// carry the fence itself rather than inherit it.
func (s *Service) expandLinks(ctx context.Context, ordered []string, acc map[string]*fusedItem, mems map[string]core.Memory, project string, globalVisible bool) ([]string, error) {
	if s.bodyReader == nil {
		return nil, nil
	}
	var neighbors []string
	examined := 0
	for _, id := range ordered {
		if examined >= linkExpandFrom {
			break
		}
		f := acc[id]
		if f == nil || f.kind != "memory" {
			continue
		}
		m, ok := mems[id]
		if !ok || !m.Active() || !scopeVisible(m.Project, project, globalVisible) {
			continue
		}
		examined++

		full, err := s.bodyReader.ReadMemory(m.FilePath)
		if err != nil {
			s.logger.Warn("retrieve.expandLinks: read body", "id", id, "error", err)
			continue
		}
		for _, name := range core.WikiLinks(full.Body) {
			nb, ok, err := s.resolveLinkedMemory(ctx, project, name, globalVisible)
			if err != nil {
				// Link expansion is a bonus over results already fused from FTS
				// and cosine, and a failed neighbor lookup does not make those
				// wrong. Abandon the bonus and keep the recall: the contract is
				// already that no body reader means no links and no error, so a
				// lookup blip must not be the one path that fails the caller.
				// Stop rather than continue -- if the store is unhappy, the next
				// lookup is unlikely to differ and would only repeat the log.
				s.logger.Warn("retrieve.expandLinks: resolve link; skipping link expansion", "name", name, "error", err)
				return neighbors, nil
			}
			if !ok {
				continue
			}
			if _, exists := acc[nb.ID]; exists {
				continue // already a candidate (from semantic/fts or an earlier link)
			}
			rank := len(neighbors)
			acc[nb.ID] = &fusedItem{kind: "memory", linked: true, score: 1.0 / float64(rrfK+rank+1)}
			mems[nb.ID] = nb
			neighbors = append(neighbors, nb.ID)
		}
	}
	return neighbors, nil
}

// resolveLinkedMemory resolves a [[name]] reference to an active memory in the
// project scope, falling back to a global memory of the same name when the
// scope may read global at all (globalVisible; a sealed project may not).
// found is false when nothing matches (a dangling link is simply ignored).
//
// It is a by-name read like memory_read's, so a restricted account's project
// memory steps aside for the global rule it would shadow (MemoryByNameRead):
// a [[name]] link in a hit pulls in the rule, never the account's text in its
// place. The memory passed over is not added: it is in recall on its own
// merits, labeled, when the query reaches it.
func (s *Service) resolveLinkedMemory(ctx context.Context, project, name string, globalVisible bool) (core.Memory, bool, error) {
	r, err := MemoryByNameRead(ctx, s.db, project, name, globalVisible)
	if err != nil {
		return core.Memory{}, false, err
	}
	return r.Memory, r.Found, nil
}
