package gardener

// Stamped copies and their sources (isolation.requests, plan fenced-sessions
// P7; owner decision N2 (a) and placement rule 3).
//
// A memory written while its conversation held content from a fenced project
// other than its own carries a provenance stamp (core.Stamp, frontmatter
// may_contain), and may name the items it copied from in its body by the one
// fixed form core.Citations reads, "<project>:<id>". Two passes read them:
//
//   - stale-copy (proposeStaleCopies) flags a copy whose cited source changed
//     after the copy was last written, as an archive proposal for the copy, in
//     the copy's project;
//   - copy-relocate (proposeSourceRelocations) proposes moving a copy whose
//     sources all lie in one other project into that project: the relocation
//     placement rule 3 asks the gardener for, once an agent wrote in its home
//     what is about a project it could not write to, citing the sources.
//
// Both read only memories that carry a stamp, and stamps exist only where
// isolation.requests wrote them, so on an install that never turned the
// switch on neither pass finds anything to read.
//
// Neither ever reads a source's content. A proposal names the copy (its own
// project's item) and the sources by project slug and id -- the references
// the copy's own body makes -- and, for a stale copy, when a source changed.
// That time is a fact about the source's project, so a stale-copy proposal's
// scope spans the sources' projects too (ResolveProposalScope): an agent is
// shown it only when it may read the copy's project and every source's
// project (gardener_proposals, through ProposalFence), and may apply it only
// where mcp.fenceProposal finds both readable and writable. The console is
// exempt, as for every proposal, and its views hide a proposal that names a
// locked project's slug or item.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The bases a proposal of an existing kind carries when one of these passes
// filed it (payload "basis"): what the console renders it as, and what tells
// it apart from the same kind's other passes.
const (
	// BasisSourceChanged marks an archive proposal for a stamped copy whose
	// cited source changed after the copy was last written (stale-copy).
	BasisSourceChanged = "source_changed"
	// BasisCitedSources marks a relocate proposal for a stamped copy whose
	// sources all lie in its destination (copy-relocate). The isolation
	// audit's relocate proposals carry no basis.
	BasisCitedSources = "cited_sources"
)

// stampedCopy is one active memory that may contain content from a project
// other than its own: the memory as its file holds it, the stamps it shows in
// its project, and the references its body makes to other projects' items.
type stampedCopy struct {
	mem    core.Memory
	stamps []core.Stamp
	cites  []core.Citation
}

// holds reports whether the copy carries a stamp naming project.
func (c stampedCopy) holds(project string) bool {
	return slices.ContainsFunc(c.stamps, func(st core.Stamp) bool { return st.Project == project })
}

// stampedCopies reads every active memory whose index carries a stamp, from
// its file (the stamps' source of truth). A memory whose file cannot be read
// is skipped with a warning naming its id: its references are not judged this
// pass, which proposes nothing about it rather than guessing.
func (s *Service) stampedCopies(ctx context.Context) ([]stampedCopy, error) {
	mems, err := store.AllActiveMemories(ctx, s.db)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	stamped, err := store.ProvenanceForItems(ctx, s.db, core.ItemKindMemory, ids)
	if err != nil {
		return nil, err
	}
	var out []stampedCopy
	for _, m := range mems {
		if len(stamped[m.ID]) == 0 {
			continue
		}
		full, err := s.files.Store().ReadMemory(m.FilePath)
		if err != nil {
			s.logger.Warn("gardener: read a stamped memory; its source references are not checked this pass",
				"id", m.ID, "error", err)
			continue
		}
		shown := core.StampsShown(full.MayContain, full.Project)
		if len(shown) == 0 {
			continue
		}
		var cites []core.Citation
		for _, c := range core.Citations(full.Body) {
			if c.Project != full.Project {
				cites = append(cites, c)
			}
		}
		out = append(out, stampedCopy{mem: full, stamps: shown, cites: cites})
	}
	return out, nil
}

// copySource is the item a reference names, as far as the gardener reads
// it: its kind, the project it lives in now, and when it last changed. Never
// its content.
type copySource struct {
	kind    string
	project string
	changed time.Time
}

// resolveSource finds the item with id among the kinds a copy can cite -- a
// memory, a note or a task -- and reports found=false for an id that names
// none of them (deleted, never written, or a trial's or a session's).
//
// A memory's change is its last write or its retirement, whichever is later:
// a source archived or superseded after the copy was made changed after it.
// A task's is its last update of any kind, its status included, since a copy
// of a task's details goes out of date when the task does.
func (s *Service) resolveSource(ctx context.Context, id string) (copySource, bool, error) {
	if m, ok, err := store.MemoryByID(ctx, s.db, id); err != nil {
		return copySource{}, false, err
	} else if ok {
		changed := m.Updated
		if m.InvalidAt != nil && m.InvalidAt.After(changed) {
			changed = *m.InvalidAt
		}
		return copySource{kind: core.ItemKindMemory, project: m.Project, changed: changed}, true, nil
	}
	if n, ok, err := store.NoteByID(ctx, s.db, id); err != nil {
		return copySource{}, false, err
	} else if ok {
		return copySource{kind: core.ItemKindNote, project: n.Project, changed: n.Updated}, true, nil
	}
	t, err := store.TaskByID(ctx, s.db, id)
	switch {
	case errors.Is(err, store.ErrTaskNotFound):
		return copySource{}, false, nil
	case err != nil:
		return copySource{}, false, err
	}
	return copySource{kind: core.ItemKindTask, project: t.ProjectSlug, changed: t.UpdatedAt}, true, nil
}

// sourceJudge decides, once per pair of projects, whether the stale-copy pass
// may judge a copy in one project against a source in another.
type sourceJudge struct {
	db    *sql.DB
	gates map[string]*scopeGate
	iso   map[string]core.Isolation
}

func newSourceJudge(db *sql.DB) *sourceJudge {
	return &sourceJudge{db: db, gates: map[string]*scopeGate{}, iso: map[string]core.Isolation{}}
}

// readable reports whether the pass may compare copy c with a source in
// project: the copy's project reads it (store.CanRead, as a session bound
// there would, with no grant: an open project, read from anything but a
// sealed one), or the copy carries a stamp naming it and it is confidential
// now. The stamp is the record of the approved flow that let the source's
// content into the copy, and CanRead's grant rule opens a confidential target
// alone: a source in a project sealed since, or in a fenced project the copy
// never held, is not judged.
func (j *sourceJudge) readable(ctx context.Context, c stampedCopy, project string) (bool, error) {
	gate, ok := j.gates[c.mem.Project]
	if !ok {
		gate = newScopeGate(j.db, c.mem.Project)
		j.gates[c.mem.Project] = gate
	}
	if ok, err := gate.readable(ctx, project); err != nil || ok {
		return ok, err
	}
	if !c.holds(project) {
		return false, nil
	}
	iso, ok := j.iso[project]
	if !ok {
		var err error
		if iso, err = store.IsolationOf(ctx, j.db, project); err != nil {
			return false, err
		}
		j.iso[project] = iso
	}
	return iso == core.IsolationConfidential, nil
}

// changedSource is one cited source that changed after the copy.
type changedSource struct {
	cite    core.Citation
	kind    string
	changed time.Time
}

// changedSources returns the sources copy c cites that changed after it was
// last written, in the order its body cites them.
//
// A reference the pass cannot judge is skipped and leaves no trace: one whose
// id names no memory, note or task, one whose item lives in another project
// than the reference names (moved since, or mistyped), and one in a project
// the copy may not read (sourceJudge.readable). "Cannot be checked" is not
// "stale", and a proposal saying a source is gone would state a fact about
// the source's project.
//
// The copy's last write stands for when it was made: a write to the copy
// counts as a refresh, since nothing records which version of a source a copy
// last matched.
func (s *Service) changedSources(ctx context.Context, judge *sourceJudge, c stampedCopy) ([]changedSource, error) {
	var out []changedSource
	for _, cite := range c.cites {
		src, found, err := s.resolveSource(ctx, cite.ID)
		if err != nil {
			return nil, err
		}
		if !found || src.project != cite.Project {
			continue
		}
		ok, err := judge.readable(ctx, c, cite.Project)
		if err != nil {
			return nil, err
		}
		if ok && src.changed.After(c.mem.Updated) {
			out = append(out, changedSource{cite: cite, kind: src.kind, changed: src.changed})
		}
	}
	return out, nil
}

// proposeStaleCopies files an archive proposal for each stamped memory whose
// cited sources changed after it was last written: applied, the stale copy
// leaves briefings and recall (still readable, and undoable); dismissed, it
// stays as it is. The proposal is the copy's project's, and names no content
// of the sources.
//
// It shares the archive passes' key, one archive question per memory, so a
// pending or hidden archive proposal for the memory holds it back. A
// dismissal (of this pass's proposal or another archive pass's) holds until a
// cited source changes after it: the evidence clock is the newest change.
// Unlike the staleness pass it spares no kind: a stale copy of a constraint
// or a stage is the copy most worth the owner's eye.
func (s *Service) proposeStaleCopies(ctx context.Context, seen seenKeys) (int, error) {
	copies, err := s.stampedCopies(ctx)
	if err != nil {
		return 0, err
	}
	judge := newSourceJudge(s.db)
	created := 0
	for _, c := range copies {
		changed, err := s.changedSources(ctx, judge, c)
		if err != nil {
			return created, err
		}
		if len(changed) == 0 {
			continue
		}
		newest := changed[0].changed
		for _, ch := range changed[1:] {
			if ch.changed.After(newest) {
				newest = ch.changed
			}
		}
		key := "archive:" + c.mem.ID
		if seen.blockedSince(key, newest) {
			continue
		}
		if _, err := s.createProposal(ctx, store.ProposalArchive, key, staleCopyPayload(c.mem, changed), seen); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// staleCopyPayload is a stale copy's archive proposal: the archive kind's
// fields for the copy, and the sources that changed by project, id, kind and
// time ("changed_sources", which ResolveProposalScope adds to its scope).
func staleCopyPayload(m core.Memory, changed []changedSource) map[string]any {
	refs := make([]string, 0, len(changed))
	sources := make([]map[string]any, 0, len(changed))
	for _, ch := range changed {
		refs = append(refs, ch.cite.String())
		sources = append(sources, map[string]any{
			"project": ch.cite.Project, "id": ch.cite.ID, "kind": ch.kind, "changed": core.FormatTime(ch.changed),
		})
	}
	reason := "its source " + refs[0] + " changed after this copy was last written"
	if len(refs) > 1 {
		reason = "its sources " + joinAnd(refs) + " changed after this copy was last written"
	}
	return map[string]any{
		"id": m.ID, "name": m.Name, "project": m.Project, "kind": string(m.Kind),
		"description": m.Description, "reason": reason, "last_activity": core.FormatTime(m.Updated),
		"basis": BasisSourceChanged, "copied_at": core.FormatTime(m.Updated), "changed_sources": sources,
	}
}

// soleSourceProject is the one project other than its own that copy c's
// sources lie in, or ok=false: every stamp it shows names that project, it
// cites at least one item, and every item it cites is that project's by its
// reference. A stamp naming any other project means moving the copy there
// would carry that project's content along, and no citation means nothing
// says which project the copy is about.
func (c stampedCopy) soleSourceProject() (string, bool) {
	if len(c.cites) == 0 {
		return "", false
	}
	to := c.stamps[0].Project
	for _, st := range c.stamps {
		if st.Project != to {
			return "", false
		}
	}
	for _, cite := range c.cites {
		if cite.Project != to {
			return "", false
		}
	}
	return to, to != c.mem.Project
}

// proposeSourceRelocations files a relocate proposal for each stamped memory
// whose sources all lie in one other project (soleSourceProject), once every
// item it cites is found there: the move placement rule 3 leaves to the
// gardener, for a memory its writer kept in its home because the project it
// is about was not cleared for it. The destination must be a registered
// project. A memory the isolation audit already asked about (relocateKey) is
// not asked about twice, and a dismissal holds: the key names the move, and
// nothing new about the memory makes a different one.
//
// Moving a copy into a project less fenced than one it may hold content from
// (its own, or another stamp's) is the owner's call alone: the console warns
// before the apply, and the apply-time check stays informational
// (gardener-apply-time-isolation-recheck).
func (s *Service) proposeSourceRelocations(ctx context.Context, seen seenKeys) (int, error) {
	copies, err := s.stampedCopies(ctx)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, c := range copies {
		to, ok := c.soleSourceProject()
		if !ok {
			continue
		}
		key := relocateKey(to, c.mem.ID)
		if seen.blocked(key) {
			continue
		}
		placed, err := s.citesLieIn(ctx, c, to)
		if err != nil {
			return created, err
		}
		if !placed {
			continue
		}
		if _, found, err := store.ProjectBySlug(ctx, s.db, to); err != nil {
			return created, err
		} else if !found {
			continue
		}
		iso, err := store.IsolationOf(ctx, s.db, to)
		if err != nil {
			return created, err
		}
		if _, err := s.createProposal(ctx, store.ProposalRelocate, key, sourceRelocatePayload(c, to, iso), seen); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// citesLieIn reports whether every item copy c cites is found in project.
func (s *Service) citesLieIn(ctx context.Context, c stampedCopy, project string) (bool, error) {
	for _, cite := range c.cites {
		src, found, err := s.resolveSource(ctx, cite.ID)
		if err != nil {
			return false, err
		}
		if !found || src.project != project {
			return false, nil
		}
	}
	return true, nil
}

// sourceRelocatePayload is a stamped copy's relocate proposal: the move
// applyReproject reads ("id", "to") with the copy's project as "from", and
// the evidence the review gate renders -- the references the copy's own body
// makes ("cited") and the destination's isolation now.
func sourceRelocatePayload(c stampedCopy, to string, state core.Isolation) map[string]any {
	cited := make([]string, 0, len(c.cites))
	for _, cite := range c.cites {
		cited = append(cited, cite.String())
	}
	return map[string]any{
		"id": c.mem.ID, "name": c.mem.Name, "kind": string(c.mem.Kind), "description": c.mem.Description,
		"from": c.mem.Project, "to": to, "isolation": string(state),
		"basis": BasisCitedSources, "cited": cited,
		"reason": "it may contain content from " + to + " and no other project, and every item it cites lies in " + to,
	}
}

// joinAnd joins names as a sentence lists them: "a", "a and b", "a, b and c".
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
