package gardener

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Provenance stamps on what the gardener writes (plan fenced-sessions P6 and
// P7). A gardener write is the gardener's own content, so it carries no
// applying agent's label; but a write that folds stamped items -- a merge or a
// consolidation superseding stamped memories (lifecycle.UnionStamps), a digest
// summarizing stamped findings -- may contain what they may contain, and
// carries the union of their stamps.

// payloadStampsKey is the proposal payload key that records, at proposal
// time, the stamps of the items the proposal's text was written from: the
// stamps its apply writes onto what it creates. Absent when they carry none.
const payloadStampsKey = "may_contain"

// foldedStamps is the union of the stamps the items of kind with ids carry
// (store.ProvenanceForItems), as an item in project shows them: none for
// project itself, each source project once, the first approval named for it
// kept (core.MergeStamps).
func (s *Service) foldedStamps(ctx context.Context, kind string, ids []string, project string) ([]core.Stamp, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID, err := store.ProvenanceForItems(ctx, s.db, kind, ids)
	if err != nil {
		return nil, err
	}
	var out []core.Stamp
	for _, id := range ids {
		out, _ = core.MergeStamps(out, byID[id], project)
	}
	return out, nil
}

// payloadStamps reads the stamps a payload recorded under key: a proposal
// still in memory from CreateProposal holds []core.Stamp, one read back from
// the store the JSON objects core.Stamp marshals to. A value that does not
// decode is an error, never "no stamps": writing the item without them would
// drop the very warning they carry.
func payloadStamps(m map[string]any, key string) ([]core.Stamp, error) {
	switch v := m[key].(type) {
	case nil:
		return nil, nil
	case []core.Stamp:
		return v, nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("proposal %s: %w", key, err)
		}
		var out []core.Stamp
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("proposal %s does not read as provenance stamps: %w", key, err)
		}
		return out, nil
	}
}
