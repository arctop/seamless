package gardener

import (
	"context"
	"fmt"

	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
)

// A proposal can go moot while it waits: the memory it would archive, rekind,
// move or merge is archived or superseded by some other route. Its apply then
// can only fail ("memory ... is already inactive"), and before this it stayed
// pending until the owner noticed -- one archive proposal sat in the queue for a
// month after its memory was archived by hand, and was clicked Apply three times
// before being dismissed. A moot proposal is now settled as a dismissal, on the
// next gardener pass or at the apply that finds it, with the reason recorded in
// its result and its gardener.action event.
//
// Only inactivity settles. A memory missing from the index altogether might be
// mid-reindex rather than deleted, and a settled archive proposal is never
// raised again for that memory, so an absence keeps the apply's error and leaves
// the owner to decide.

// mootSubjects lists the memories a proposal acts on, for the kinds whose
// whole effect is on existing memories. Consolidate is absent: its apply
// already skips sources that went inactive, and its target is resolve-or-create.
func mootSubjects(p store.Proposal) []string {
	switch p.Kind {
	case store.ProposalArchive, store.ProposalRekind, store.ProposalReproject, store.ProposalRelocate:
		return []string{payloadString(p.Payload, "id")}
	case store.ProposalMerge:
		return []string{
			payloadString(payloadMap(p.Payload, "keep"), "id"),
			payloadString(payloadMap(p.Payload, "drop"), "id"),
		}
	}
	return nil
}

// mootReason says why a pending proposal can no longer be applied as raised,
// or "" when it still stands.
func (s *Service) mootReason(ctx context.Context, p store.Proposal) (string, error) {
	for _, id := range mootSubjects(p) {
		if id == "" {
			continue
		}
		m, ok, err := store.MemoryByID(ctx, s.db, id)
		if err != nil {
			return "", err
		}
		if ok && m.InvalidAt != nil {
			return fmt.Sprintf("memory %s is already inactive", lifecycle.MemoryRef(m.Project, m.Name)), nil
		}
	}
	return "", nil
}

// settleMoot resolves a moot proposal as a dismissal, the weakest resolution.
// Its key goes on blocking a re-proposal for that memory, as any dismissal's
// does, and Undo reopens it should the memory come back.
func (s *Service) settleMoot(ctx context.Context, p store.Proposal, reason string) error {
	if err := store.ResolveProposal(ctx, s.db, p.ID, store.ProposalDismissed, s.now().UTC()); err != nil {
		return err
	}
	if err := store.RecordProposalResult(ctx, s.db, p.ID, map[string]any{"settled": reason}); err != nil {
		s.logger.Warn("gardener: record settle reason", "id", p.ID, "error", err)
	}
	s.record(ctx, p.ID, map[string]any{"action": "settle", "kind": p.Kind, "reason": reason})
	return nil
}

// settleMootProposals settles every pending proposal that has gone moot.
// Best-effort, like the session reaper: a failure is logged, never returned.
func (s *Service) settleMootProposals(ctx context.Context) {
	pending, err := store.PendingProposals(ctx, s.db, "")
	if err != nil {
		s.logger.Warn("gardener: settle moot proposals", "error", err)
		return
	}
	settled := 0
	for _, p := range pending {
		reason, err := s.mootReason(ctx, p)
		if err == nil && reason != "" {
			err = s.settleMoot(ctx, p, reason)
			if err == nil {
				settled++
			}
		}
		if err != nil {
			s.logger.Warn("gardener: settle moot proposal", "id", p.ID, "error", err)
		}
	}
	if settled > 0 {
		s.logger.Info("gardener settled moot proposals", "settled", settled)
	}
}
