package console

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
)

// Stamped copies on the review gate (isolation.requests, plan fenced-sessions
// P7). Two passes file proposals of existing kinds for memories that carry a
// provenance stamp: a stale copy's archive (gardener.BasisSourceChanged) and
// a copy's relocation into the one project its sources lie in
// (gardener.BasisCitedSources). The card renders each on its own terms, and
// any move of a stamped memory -- a relocate or a reproject -- warns before
// the owner applies it into a project less fenced than one the memory may
// hold content from. The warning marks and never blocks or confirms
// (console-fence-mark-warns-never-hides): the owner is exempt, and the
// apply-time check stays informational (gardener-apply-time-isolation-recheck).

// staleSourceView is one cited source of a stale copy that changed after the
// copy was last written: by reference and kind, and when. Never its content.
type staleSourceView struct {
	Ref     string
	Project string
	Kind    string
	Changed time.Time
}

// moveWarningView is what applying a move of a stamped memory carries past a
// fence: the destination with its isolation now, and the projects the memory
// may hold content from (its own, and every stamp's) that are more fenced.
type moveWarningView struct {
	To   store.ProjectIsolation
	Past []store.ProjectIsolation
}

// staleSources reads a stale copy's changed sources from its payload. An
// entry whose time does not parse is shown without one rather than dropped:
// the reference itself is the evidence.
func staleSources(p map[string]any) []staleSourceView {
	var out []staleSourceView
	for _, src := range payloadList(p, "changed_sources") {
		v := staleSourceView{
			Ref:     core.Citation{Project: payloadStr(src, "project"), ID: payloadStr(src, "id")}.String(),
			Project: payloadStr(src, "project"),
			Kind:    payloadStr(src, "kind"),
		}
		if t, err := core.ParseTime(payloadStr(src, "changed")); err == nil {
			v.Changed = t
		}
		out = append(out, v)
	}
	return out
}

// moveWarning is the warning a reproject or relocate proposal's apply step
// shows, or nil: nil for another kind, for a memory gone from the index, for
// one that carries no stamp, and for a move into a project at least as fenced
// as every project the memory may hold content from. Isolation is read now,
// not when the proposal was raised. A lookup failure costs the warning, never
// the page, and is logged.
func (s *Service) moveWarning(ctx context.Context, p store.Proposal) *moveWarningView {
	if p.Kind != store.ProposalReproject && p.Kind != store.ProposalRelocate {
		return nil
	}
	id, to := payloadStr(p.Payload, "id"), payloadStr(p.Payload, "to")
	if id == "" || to == "" {
		return nil
	}
	db := s.db(ctx)
	mem, found, err := store.MemoryByID(ctx, db, id)
	if err != nil {
		s.logger.Warn("console: move warning: memory lookup", "proposal", p.ID, "error", err)
		return nil
	}
	if !found {
		return nil
	}
	stamps := s.memoryStamps(ctx, mem)
	if len(stamps) == 0 {
		return nil
	}
	holds := []string{mem.Project}
	for _, st := range stamps {
		if !slices.Contains(holds, st.Project) {
			holds = append(holds, st.Project)
		}
	}
	toState, err := store.IsolationOf(ctx, db, to)
	if err != nil {
		s.logger.Warn("console: move warning: destination isolation", "proposal", p.ID, "error", err)
		return nil
	}
	fenced, err := store.IsolatedSlugs(ctx, db, holds)
	if err != nil {
		s.logger.Warn("console: move warning: isolation", "proposal", p.ID, "error", err)
		return nil
	}
	var past []store.ProjectIsolation
	for _, f := range fenced {
		if f.Slug != to && fenceRank(f.State) > fenceRank(toState) {
			past = append(past, f)
		}
	}
	if len(past) == 0 {
		return nil
	}
	return &moveWarningView{To: store.ProjectIsolation{Slug: to, State: toState}, Past: past}
}

// memoryStamps is a memory's provenance stamps as its file holds them (the
// source of truth, which keeps an entry for the memory's own project too), or
// -- when the file cannot be read -- as the index mirrors them.
func (s *Service) memoryStamps(ctx context.Context, mem core.Memory) []core.Stamp {
	if s.cfg.Files != nil {
		full, err := s.cfg.Files.Store().ReadMemory(mem.FilePath)
		if err == nil {
			return full.MayContain
		}
		s.logger.Warn("console: read a memory's provenance stamps from its file", "id", mem.ID, "error", err)
	}
	rows, err := store.ProvenanceForItem(ctx, s.db(ctx), core.ItemKindMemory, mem.ID)
	if err != nil {
		s.logger.Warn("console: read a memory's provenance stamps", "id", mem.ID, "error", err)
		return nil
	}
	out := make([]core.Stamp, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Stamp())
	}
	return out
}

// digestMayContain is the provenance line of a digest proposal: the stamps
// its sessions' findings carried when it was written (the payload's
// may_contain), as the note it writes will show them in project. "" for none,
// and for a payload whose stamps do not read -- its apply refuses it then, so
// nothing is written without them.
func digestMayContain(p map[string]any, project string) string {
	raw, ok := p["may_contain"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	var stamps []core.Stamp
	if err := json.Unmarshal(b, &stamps); err != nil {
		return ""
	}
	return core.MayContainLine(core.StampsShown(stamps, project))
}

// fenceRank orders isolation states from least fenced (open) to most
// (sealed): core.Isolations' order.
func fenceRank(i core.Isolation) int { return slices.Index(core.Isolations, i) }

// stampedCard fills in what the stamped-copy passes' proposals show beyond
// their kind's card: the basis, a stale copy's changed sources, and the
// outcome-first words for each.
func stampedCard(c *proposalCard, p store.Proposal) {
	c.Basis = payloadStr(p.Payload, "basis")
	switch {
	case p.Kind == store.ProposalArchive && c.Basis == gardener.BasisSourceChanged:
		c.Label, c.Eyebrow = "Retire a stale copy", "Source changed"
		c.StaleSources = staleSources(p.Payload)
	case p.Kind == store.ProposalRelocate && c.Basis == gardener.BasisCitedSources:
		c.Label, c.Eyebrow, c.Icon, c.Tone = "Move a copy to the project its sources lie in", "Placement", "folder-tree", "pop"
	}
}
