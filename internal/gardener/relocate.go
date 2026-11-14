// The provenance audit: the one gardener pass a fence cannot do for itself.
//
// Every other isolation fence is forward-looking -- once a project is
// confidential or sealed, the read funnel, the write funnel, the briefing and
// the cross-project passes all refuse to move its knowledge. None of that
// reaches knowledge that ALREADY left: global memories written by sessions bound
// to the project, back when writing globally was allowed. Those memories are
// global, so no scope check will ever fence them, and no owner can be expected
// to remember which of their global memories came out of which project.
//
// So a tighten asks the question once, per memory, in the inbox: should this
// come back inside the fence? Propose-only like every other pass -- the audit
// files proposals and moves nothing.
package gardener

import (
	"context"
	"errors"
	"fmt"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// ErrNotIsolated is returned by ProposeIsolationRelocations for a project that
// is not fenced. The audit exists to repair a leak past a fence; running it on
// an open project would propose moving perfectly ordinary global knowledge into
// one project for no stated reason.
var ErrNotIsolated = errors.New("gardener: project is not isolated")

// ProposeIsolationRelocations files one relocate proposal per global memory
// whose source session was bound to project, and returns how many it created.
// It is called from the tighten flow (the console's confirm step and the CLI's
// --yes path) AFTER the isolation change has committed, never inside that
// transaction: the fence must land atomically whatever the audit does, and a
// failure here costs the proposals, not the fence.
//
// There is no background pass for this, on purpose. Once the fence is up the
// write funnel refuses this project's sessions any write outside it, so no new
// global memory can originate here -- the leak set is closed at tighten time,
// and re-scanning every isolated project on every tick would find the same
// already-proposed memories forever.
//
// Every proposal carries the key relocate:<project>:<memory id>, so a second
// tighten (confidential -> sealed) never re-asks a question the owner already
// applied or dismissed. The copy-relocate pass (copies.go) files the same kind
// under the same key for a memory carrying provenance stamps, so one move is
// asked about once whichever of the two finds it.
func (s *Service) ProposeIsolationRelocations(ctx context.Context, project string) (int, error) {
	iso, err := store.IsolationOf(ctx, s.db, project)
	if err != nil {
		return 0, fmt.Errorf("gardener.ProposeIsolationRelocations: %w", err)
	}
	if !iso.FencesOutbound() {
		return 0, fmt.Errorf("gardener.ProposeIsolationRelocations: %w: %s is %s", ErrNotIsolated, project, iso)
	}

	// No scopeGate here, and that is deliberate. The gate answers "may this pass
	// carry knowledge across a fence?"; nothing crosses one here. The memories
	// read are global (readable from every scope), and the destination is the
	// project they came out of -- the write funnel's inbound fence exists to keep
	// OTHER projects' knowledge out of a sealed project, not to lock a project out
	// of its own provenance. Gating on writability would silently skip the audit
	// for sealed projects, which are the ones that most need the leak repaired.
	leaked, err := store.GlobalMemoriesFromProjectSessions(ctx, s.db, project)
	if err != nil {
		return 0, fmt.Errorf("gardener.ProposeIsolationRelocations: %w", err)
	}
	if len(leaked) == 0 {
		return 0, nil
	}
	seen, err := loadSeen(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("gardener.ProposeIsolationRelocations: %w", err)
	}

	created := 0
	for _, mem := range leaked {
		key := relocateKey(project, mem.ID)
		if seen.blocked(key) {
			continue
		}
		if _, err := s.createProposal(ctx, store.ProposalRelocate, key,
			relocatePayload(mem, project, iso), seen); err != nil {
			return created, fmt.Errorf("gardener.ProposeIsolationRelocations: %w", err)
		}
		created++
	}
	return created, nil
}

// relocateKey is the dedup key: one standing question per (project, memory).
func relocateKey(project, memoryID string) string {
	return "relocate:" + project + ":" + memoryID
}

// relocatePayload describes the move and the provenance that justifies it. The
// "id"/"to" pair is exactly what applyReproject reads, so the two kinds share one
// apply and one inverse; the rest is the evidence the review gate renders.
func relocatePayload(mem core.Memory, project string, state core.Isolation) map[string]any {
	return map[string]any{
		"id":          mem.ID,
		"name":        mem.Name,
		"kind":        string(mem.Kind),
		"description": mem.Description,
		"from":        "", // always the global scope: that is what makes it a leak
		"to":          project,
		"session":     mem.SourceSession,
		"isolation":   string(state),
		"reason": "written by a session bound to " + project + ", which is now " +
			string(state) + " -- this memory stayed in the global scope, outside the fence",
	}
}
