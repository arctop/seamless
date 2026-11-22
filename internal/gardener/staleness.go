package gardener

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// errProtectionIncomplete fails a pass whose safety depends on reading a name's
// absence from the reference set as "nothing links here": with any body
// unreadable, that absence proves nothing. Shared by the staleness and
// stale-stage passes.
var errProtectionIncomplete = errors.New("protection set incomplete: some memory bodies were unreadable, so a name's absence no longer proves it is unreferenced")

// proposeArchives proposes archiving active memories that have seen no activity
// (update, injection, or read) for StalenessDays, subject to two protections:
//
//   - Kind: constraints and stages are never archived by staleness. A constraint
//     is a load-bearing rule and a stage carries gated status; neither becomes
//     irrelevant merely by going unread. (A stage that stopped carrying a live
//     gate is retired by the stale-stage pass instead -- see stages.go --
//     because pinning re-injects it every session, so it never looks inactive
//     to this pass's activity metric.)
//   - References: a memory named by a [[link]] in another memory's body is kept,
//     since something still points at it.
func (s *Service) proposeArchives(ctx context.Context, seen seenKeys) (int, error) {
	cutoff := s.now().UTC().Add(-time.Duration(s.cfg.StalenessDays) * 24 * time.Hour)
	stale, err := store.StaleMemories(ctx, s.db, cutoff)
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}
	referenced, complete, err := s.referencedNames(ctx)
	if err != nil {
		return 0, err
	}
	if !complete {
		// Archiving keys off a name being ABSENT from the protection set, so an
		// incomplete scan cannot tell "nothing links here" from "the body that
		// links here was unreadable" -- and proposing on that basis is how a
		// live, referenced memory gets archived. Fail the pass instead: the
		// unreadable bodies are logged above, and Propose reports staleness as
		// failed rather than as a clean zero.
		return 0, errProtectionIncomplete
	}

	created := 0
	for _, m := range stale {
		if m.Kind == core.KindConstraint || m.Kind == core.KindStage {
			continue // protected kinds
		}
		if _, ref := referenced[m.Name]; ref {
			continue // still referenced by another memory
		}
		key := "archive:" + m.ID
		if seen.blocked(key) {
			continue
		}
		payload := map[string]any{
			"id": m.ID, "name": m.Name, "project": m.Project, "kind": string(m.Kind),
			"description": m.Description, "reason": "no activity in " + strconv.Itoa(s.cfg.StalenessDays) + "d",
			"last_activity": core.FormatTime(m.Updated),
		}
		if _, err := s.createProposal(ctx, store.ProposalArchive, key, payload, seen); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}
