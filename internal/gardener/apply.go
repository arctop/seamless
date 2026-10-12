package gardener

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Decision names one way to resolve a pending proposal. This is the canonical
// set: derive the MCP action enum from it rather than transcribing, so a tier
// cannot exist in the service while staying invisible at the boundary. Undo and
// unhide are deliberately absent -- both act on an already-resolved proposal,
// and both are owner surfaces rather than agent decisions.
const (
	DecisionApply   = "apply"
	DecisionDismiss = "dismiss"
	DecisionHide    = "hide"
)

// Decisions lists every way to resolve a pending proposal, weakest commitment
// first.
var Decisions = []string{DecisionApply, DecisionDismiss, DecisionHide}

// Apply carries out a pending proposal and marks it applied. The effect depends
// on the kind: an archive retires the memory (invalid, but still readable), a
// merge supersedes the "drop" memory by the "keep" memory, a digest writes the
// summary as a note, a memory_wanted opens a task to write the missing
// knowledge, and a rekind reclassifies the memory's kind in place. A proposal
// whose memory went inactive since it was raised is moot: it is settled as a
// dismissal instead, and the result says so under "settled" (see moot.go). If
// the effect cannot be carried out for another reason (e.g. a referenced memory
// has since been deleted), the proposal is left pending and an error is
// returned, so the owner can retry or dismiss.
func (s *Service) Apply(ctx context.Context, id string) (map[string]any, error) {
	p, ok, err := store.ProposalByID(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no proposal with id %q", id)
	}
	if p.Status != store.ProposalPending {
		return nil, fmt.Errorf("proposal %q is already %s", id, p.Status)
	}
	reason, err := s.mootReason(ctx, p)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		if err := s.settleMoot(ctx, p, reason); err != nil {
			return nil, err
		}
		return map[string]any{"status": store.ProposalDismissed, "kind": p.Kind,
			"settled": reason + " -- nothing left to apply"}, nil
	}
	now := s.now().UTC()

	var result map[string]any
	switch p.Kind {
	case store.ProposalArchive:
		result, err = s.applyArchive(ctx, p, now)
	case store.ProposalMerge:
		result, err = s.applyMerge(ctx, p, now)
	case store.ProposalDigest:
		result, err = s.applyDigest(ctx, p, now)
	case store.ProposalConsolidate:
		result, err = s.applyConsolidate(ctx, p, now)
	case store.ProposalReproject, store.ProposalRelocate:
		// One effect, two decisions: a relocate is a reproject out of the global
		// scope, so it shares the move, the name-clash guard, the idempotence, and
		// the inverse. Only the evidence the owner reviewed differs.
		result, err = s.applyReproject(ctx, p, now)
	case store.ProposalSplit:
		result, err = s.applySplit(ctx, p, now)
	case store.ProposalAbandonPlan:
		result, err = s.applySettlePlan(ctx, p, plans.StatusAbandoned, now)
	case store.ProposalShipPlan:
		result, err = s.applySettlePlan(ctx, p, plans.StatusShipped, now)
	case store.ProposalMergePlans:
		// The one settlement that moves rather than retags in place: the whole
		// composition follows the capture onto the surviving plan's slug.
		result, err = s.applyMergePlans(ctx, p, now)
	case store.ProposalMemoryWanted:
		result, err = s.applyMemoryWanted(ctx, p, now)
	case store.ProposalToolError:
		result, err = s.applyToolError(ctx, p, now)
	case store.ProposalRekind:
		result, err = s.applyRekind(ctx, p, now)
	default:
		return nil, fmt.Errorf("unknown proposal kind %q", p.Kind)
	}
	if err != nil {
		return nil, err // leave the proposal pending; the effect did not happen
	}
	if err := store.ResolveProposal(ctx, s.db, id, store.ProposalApplied, now); err != nil {
		return nil, err
	}
	// Persist what the apply produced so Undo can find the artifact again (the
	// note it wrote, the task it opened, the project it moved a memory out of).
	// Best-effort: the change already landed, so a failure here costs the undo
	// affordance, not the apply.
	if err := store.RecordProposalResult(ctx, s.db, id, result); err != nil {
		s.logger.Warn("gardener: record apply result", "id", id, "error", err)
	}
	s.record(ctx, id, map[string]any{"action": "apply", "kind": p.Kind})
	result["status"] = "applied"
	result["kind"] = p.Kind
	return result, nil
}

// Dismiss is the regular rejection: it marks a pending proposal dismissed
// without any side effect. The suggestion stops being raised, but only for the
// evidence behind it -- a pattern whose evidence recurs after the dismissal is
// proposed again, because the owner answered what they were shown and not
// everything the pattern might do next. Hide is the tier that answers forever.
func (s *Service) Dismiss(ctx context.Context, id string) error {
	return s.reject(ctx, id, store.ProposalDismissed, "dismiss")
}

// Hide is the strong rejection: the proposal is resolved like a dismissal and
// its key is blocked permanently, so no recurrence re-raises it. It is
// reversible twice over -- Undo returns this very proposal to the queue, and
// Unhide lifts the block without doing so.
func (s *Service) Hide(ctx context.Context, id string) error {
	return s.reject(ctx, id, store.ProposalHidden, "hide")
}

// reject resolves a pending proposal into one of the two rejection tiers and
// records the decision.
func (s *Service) reject(ctx context.Context, id, status, action string) error {
	if err := store.ResolveProposal(ctx, s.db, id, status, s.now().UTC()); err != nil {
		return err
	}
	s.record(ctx, id, map[string]any{"action": action})
	return nil
}

// Unhide lifts a forever block. The proposal stays resolved -- this is not an
// undo and nothing returns to the queue -- but its key drops to a regular
// dismissal, so the next pass may raise the pattern again once evidence for it
// recurs. For a pattern that has stopped occurring, that is nothing at all,
// which is the honest outcome: unhiding does not manufacture a proposal.
func (s *Service) Unhide(ctx context.Context, id string) error {
	if err := store.UnhideProposal(ctx, s.db, id); err != nil {
		return err
	}
	s.record(ctx, id, map[string]any{"action": "unhide"})
	return nil
}

// Hidden lists the proposals blocked forever, newest decision first. It backs
// the console's hidden list, the only surface that names a standing block.
func (s *Service) Hidden(ctx context.Context) ([]store.Proposal, error) {
	return store.HiddenProposals(ctx, s.db)
}

func (s *Service) applyArchive(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	// Archive appends a tombstone to the body and rewrites the whole file, so the
	// read it works from has to be the one under the lock -- see
	// mutateActiveMemory. Everything after (the event, the result) is outside it.
	var updated core.Memory
	if err := s.mutateActiveMemory(ctx, payloadString(p.Payload, "id"), func(ctx context.Context, mem core.Memory) error {
		archived, err := lifecycle.Archive(ctx, s.files, mem, "gardener staleness", now)
		if err != nil {
			return err
		}
		updated = archived
		return nil
	}); err != nil {
		return nil, err
	}
	s.recordMemory(ctx, core.EventMemoryArchived, updated, map[string]any{"name": updated.Name, "by": "gardener"})
	return map[string]any{"archived": lifecycle.MemoryRef(updated.Project, updated.Name)}, nil
}

func (s *Service) applyMerge(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	keepID := payloadString(payloadMap(p.Payload, "keep"), "id")
	dropID := payloadString(payloadMap(p.Payload, "drop"), "id")
	if keepID == "" || dropID == "" {
		return nil, errors.New("merge proposal missing keep/drop ids")
	}
	if keepID == dropID {
		return nil, errors.New("merge proposal keep and drop are the same memory")
	}
	// The kept memory is only read from -- it supplies the tombstone's target --
	// so it needs no lock. The dropped one is rewritten wholesale by Supersede,
	// so its read moves inside the lock; the index lookup outside is what names
	// the file to lock, and it keeps the per-side error prefixes intact.
	keep, err := s.loadActiveMemory(ctx, keepID)
	if err != nil {
		return nil, fmt.Errorf("keep memory: %w", err)
	}
	dropIdx, err := s.activeMemoryIndex(ctx, dropID)
	if err != nil {
		return nil, fmt.Errorf("drop memory: %w", err)
	}
	var updated core.Memory
	if err := s.files.Mutate(ctx, dropIdx.FilePath, func(ctx context.Context) error {
		drop, rerr := s.loadActiveMemory(ctx, dropID)
		if rerr != nil {
			return fmt.Errorf("drop memory: %w", rerr)
		}
		superseded, serr := lifecycle.Supersede(ctx, s.files, drop, keep, now)
		if serr != nil {
			return serr
		}
		updated = superseded
		return nil
	}); err != nil {
		return nil, err
	}
	s.recordMemory(ctx, core.EventMemorySuperseded, updated, map[string]any{
		"name": updated.Name, "superseded_by": keep.ID, "by": "gardener",
	})
	return map[string]any{
		"kept":    lifecycle.MemoryRef(keep.Project, keep.Name),
		"dropped": lifecycle.MemoryRef(updated.Project, updated.Name),
	}, nil
}

func (s *Service) applyDigest(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	title := payloadString(p.Payload, "title")
	body := payloadString(p.Payload, "body")
	if title == "" || body == "" {
		return nil, errors.New("digest proposal missing title/body")
	}
	id, err := core.NewID()
	if err != nil {
		return nil, err
	}
	project := payloadString(p.Payload, "project")
	note := core.Note{
		ID: id, Title: title, Slug: core.Slugify(title), Description: "Monthly session digest",
		Project: project, Body: body, Tags: []string{"created-by:gardener", "digest"},
		Model:   payloadString(p.Payload, "model"),
		Created: now, Updated: now,
	}
	written, err := s.files.WriteNote(ctx, note)
	if err != nil {
		return nil, err
	}
	if s.events != nil {
		s.record(ctx, written.ID, map[string]any{"action": "digest_note", "title": title})
	}
	return map[string]any{"note_id": written.ID, "title": written.Title}, nil
}

// applyConsolidate writes a new unified memory (or reuses an existing one with
// the same project+name) and supersedes every named source by it. It is
// idempotent: a retry after a partial apply reuses the already-written target
// and skips sources that were already superseded, so it converges rather than
// duplicating -- important because Apply leaves the proposal pending on failure.
func (s *Service) applyConsolidate(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	name := payloadString(p.Payload, "name")
	project := payloadString(p.Payload, "project")
	body := payloadString(p.Payload, "body")
	if name == "" || body == "" {
		return nil, errors.New("consolidate proposal missing name/body")
	}

	// Resolve-or-create the unified target. An already-active (project,name)
	// memory is reused, so a retry does not write a second copy; on a first run
	// this also means a name matching an existing memory folds the sources into it.
	target, ok, err := store.MemoryByName(ctx, s.db, project, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		id, nerr := core.NewID()
		if nerr != nil {
			return nil, nerr
		}
		written, werr := s.files.WriteMemory(ctx, core.Memory{
			ID: id, Kind: core.MemoryKind(payloadString(p.Payload, "kind")),
			Name: name, Description: payloadString(p.Payload, "description"),
			Project: project, Body: body,
			Model:   payloadString(p.Payload, "model"),
			Created: now, Updated: now, ValidFrom: now,
		})
		if werr != nil {
			return nil, werr
		}
		target = written
		s.recordMemory(ctx, core.EventMemoryWritten, target, map[string]any{"name": target.Name, "by": "gardener"})
	}

	// Supersede each still-active source by the target. Sources already inactive
	// (superseded on a prior partial apply) and the target itself are skipped.
	// Each source is superseded under its own file's lock, one at a time: the
	// tombstone write renders the whole file, so the body it appends to must be
	// read inside. Locking them one by one rather than all at once is deliberate
	// -- the sources are independent writes, and the loop already converges on a
	// retry, so holding every lock for the whole fold would block unrelated
	// mutations for no added safety.
	superseded := make([]string, 0)
	for _, src := range payloadList(p.Payload, "sources") {
		srcID := payloadString(src, "id")
		if srcID == "" || srcID == target.ID {
			continue
		}
		idx, found, ierr := store.MemoryByID(ctx, s.db, srcID)
		if ierr != nil {
			return nil, ierr
		}
		if !found || idx.InvalidAt != nil {
			continue // already gone or already superseded
		}
		var updated core.Memory
		if merr := s.files.Mutate(ctx, idx.FilePath, func(ctx context.Context) error {
			full, rerr := s.files.Store().ReadMemory(idx.FilePath)
			if rerr != nil {
				return rerr
			}
			u, serr := lifecycle.Supersede(ctx, s.files, full, target, now)
			if serr != nil {
				return serr
			}
			updated = u
			return nil
		}); merr != nil {
			return nil, merr
		}
		s.recordMemory(ctx, core.EventMemorySuperseded, updated, map[string]any{
			"name": updated.Name, "superseded_by": target.ID, "by": "gardener",
		})
		superseded = append(superseded, lifecycle.MemoryRef(updated.Project, updated.Name))
	}

	return map[string]any{
		"created":    lifecycle.MemoryRef(target.Project, target.Name),
		"superseded": superseded,
	}, nil
}

// applyReproject relocates a memory to another project. Unlike archive/merge this
// is a relocation, not an invalidation: the memory keeps its ULID and body and
// stays active, only its project (and file path) change. It is idempotent -- a
// memory already at the target is a success, so a retry after a partial apply
// converges. A name already taken by a different active memory in the target is a
// hard error (the proposal stays pending) so the owner resolves the clash.
//
// It also carries the relocate kind (the isolation provenance audit), whose
// payload names the same "id" and "to".
func (s *Service) applyReproject(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	to := payloadString(p.Payload, "to")
	if to == "" {
		return nil, fmt.Errorf("%s proposal missing target project", p.Kind)
	}
	idx, err := s.activeMemoryIndex(ctx, payloadString(p.Payload, "id"))
	if err != nil {
		return nil, err
	}
	// A move spans two files -- MoveMemory writes the new path before removing the
	// old one -- so both are locked for the whole read-decide-write. The clash
	// guard is inside because it is the check that authorizes the write into the
	// target path, and holding that path's lock is what stops a second writer from
	// taking the name between the check and the move.
	var moved core.Memory
	var from string
	noop := false
	if err := s.files.MutatePaths(ctx, []string{idx.FilePath, files.MemoryRelPath(to, idx.Name)}, func(ctx context.Context) error {
		mem, rerr := s.loadActiveMemory(ctx, payloadString(p.Payload, "id"))
		if rerr != nil {
			return rerr
		}
		from = mem.Project
		if from == to {
			noop = true // already relocated (a retry, or a no-op target)
			return nil
		}
		if clash, found, cerr := store.MemoryByName(ctx, s.db, to, mem.Name); cerr != nil {
			return cerr
		} else if found && clash.ID != mem.ID {
			return fmt.Errorf("target project %q already has an active memory named %q", to, mem.Name)
		}
		mem.Updated = now
		m, merr := s.files.MoveMemory(ctx, mem, to)
		if merr != nil {
			return merr
		}
		moved = m
		return nil
	}); err != nil {
		return nil, err
	}
	if noop {
		return map[string]any{"moved": lifecycle.MemoryRef(to, idx.Name), "from": from, "to": to, "noop": true}, nil
	}
	s.recordMemory(ctx, core.EventMemoryMoved, moved, map[string]any{
		"name": moved.Name, "from": from, "to": to, "by": "gardener",
	})
	return map[string]any{"moved": lifecycle.MemoryRef(to, moved.Name), "from": from, "to": to}, nil
}

// applyRekind reclassifies a memory in place: same ULID, same project, same
// body -- only the kind (and the updated stamp) change, so briefings and recall
// start tiering it under its corrected kind. It is idempotent: a memory already
// at the target kind is a no-op success, so a retry after a partial apply
// converges. An unknown target kind is a hard error (the proposal stays pending)
// rather than a silent default -- the owner approved a specific classification.
func (s *Service) applyRekind(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	to := core.MemoryKind(payloadString(p.Payload, "to"))
	if !slices.Contains(core.MemoryKinds, to) {
		return nil, fmt.Errorf("rekind proposal has unknown target kind %q", to)
	}
	// The kind lives in the frontmatter, which WriteMemory re-renders along with
	// the whole file, so the read that supplies the body has to be under the lock.
	var written core.Memory
	var from core.MemoryKind
	var ref string
	noop := false
	if err := s.mutateActiveMemory(ctx, payloadString(p.Payload, "id"), func(ctx context.Context, mem core.Memory) error {
		from = mem.Kind
		ref = lifecycle.MemoryRef(mem.Project, mem.Name)
		if from == to {
			noop = true
			return nil
		}
		mem.Kind = to
		mem.Updated = now
		w, werr := s.files.WriteMemory(ctx, mem)
		if werr != nil {
			return werr
		}
		written = w
		return nil
	}); err != nil {
		return nil, err
	}
	if noop {
		return map[string]any{"rekinded": ref, "from": string(from), "to": string(to), "noop": true}, nil
	}
	s.recordMemory(ctx, core.EventMemoryWritten, written, map[string]any{
		"name": written.Name, "kind_from": string(from), "kind_to": string(to), "by": "gardener",
	})
	return map[string]any{"rekinded": ref, "from": string(from), "to": string(to)}, nil
}

// applySplit sets up the topology for a project split: it creates the child and
// shared-parent projects, links them as a family, points each child at the shared
// parent, and (optionally) retires the emptied source. It moves no memories --
// each memory is a separate reproject proposal in the same plan, so the owner can
// retarget or veto per memory. Every step is idempotent (ensure/add/set are
// upserts), so a retry after a partial apply converges.
func (s *Service) applySplit(ctx context.Context, p store.Proposal, now time.Time) (map[string]any, error) {
	source := payloadString(p.Payload, "source_project")
	shared := payloadMap(p.Payload, "shared")
	sharedSlug := payloadString(shared, "slug")

	// Collect the target project slugs to create: each child plus the shared parent.
	type projSpec struct{ slug, label string }
	var specs []projSpec
	for _, c := range payloadList(p.Payload, "children") {
		if slug := payloadString(c, "slug"); slug != "" {
			specs = append(specs, projSpec{slug, payloadString(c, "label")})
		}
	}
	if sharedSlug != "" {
		specs = append(specs, projSpec{sharedSlug, payloadString(shared, "label")})
	}
	if len(specs) == 0 {
		return nil, errors.New("split proposal has no child or shared projects")
	}

	created := make([]string, 0, len(specs))
	familyMembers := make([]string, 0, len(specs))
	for _, sp := range specs {
		if _, err := store.EnsureProject(ctx, s.db, sp.slug, sp.label); err != nil {
			return nil, fmt.Errorf("ensure project %q: %w", sp.slug, err)
		}
		created = append(created, sp.slug)
		familyMembers = append(familyMembers, sp.slug)
	}

	// Link the new projects as a family so a child's briefing surfaces its
	// siblings' recent findings; name it after the shared parent (or the source).
	family := payloadString(p.Payload, "family")
	if family == "" {
		family = sharedSlug
	}
	if family == "" {
		family = source + "-split"
	}
	if _, err := store.AddFamilyMembers(ctx, s.db, family, familyMembers); err != nil {
		return nil, fmt.Errorf("link family: %w", err)
	}

	// Point each child at the shared parent, whose active memories are injected
	// into the child's briefing.
	parented := make([]string, 0)
	if sharedSlug != "" {
		for _, sp := range specs {
			if sp.slug == sharedSlug {
				continue
			}
			if err := store.SetProjectParent(ctx, s.db, sp.slug, sharedSlug, now); err != nil {
				return nil, fmt.Errorf("parent %q: %w", sp.slug, err)
			}
			parented = append(parented, sp.slug)
		}
	}

	retired := ""
	if payloadBool(p.Payload, "retire_source") && source != "" {
		if err := store.RetireProject(ctx, s.db, source, now); err != nil {
			return nil, fmt.Errorf("retire %q: %w", source, err)
		}
		retired = source
	}

	return map[string]any{
		"created": created, "family": family, "parented": parented, "retired": retired,
	}, nil
}

// activeMemoryIndex resolves a memory id to its INDEX row, erroring if the
// memory no longer exists or is already inactive (archived/superseded) -- in
// either case the proposal's effect no longer applies. The row carries no body:
// its job is to name the file, which is all a caller needs before taking that
// file's mutation lock.
func (s *Service) activeMemoryIndex(ctx context.Context, id string) (core.Memory, error) {
	if id == "" {
		return core.Memory{}, errors.New("empty memory id")
	}
	idx, ok, err := store.MemoryByID(ctx, s.db, id)
	if err != nil {
		return core.Memory{}, err
	}
	if !ok {
		return core.Memory{}, fmt.Errorf("memory %q no longer exists", id)
	}
	if idx.InvalidAt != nil {
		return core.Memory{}, fmt.Errorf("memory %q is already inactive", id)
	}
	return idx, nil
}

// loadActiveMemory resolves a memory id to its full on-disk content, subject to
// the same liveness guards. A caller that goes on to WRITE the memory must call
// this INSIDE the file's mutation lock (mutateActiveMemory does): every apply
// rewrites the whole file from what this returned, so a read taken before the
// lock renders content another writer has already replaced -- and the loser's
// write vanishes with no error anywhere, since the index upsert is keyed by id.
func (s *Service) loadActiveMemory(ctx context.Context, id string) (core.Memory, error) {
	idx, err := s.activeMemoryIndex(ctx, id)
	if err != nil {
		return core.Memory{}, err
	}
	return s.files.Store().ReadMemory(idx.FilePath)
}

// mutateActiveMemory runs fn on an active memory with its file's mutation lock
// held, handing fn the memory as re-read (and re-checked for liveness) inside
// the lock. The resolve outside only names the file to lock; the resolve inside
// is the one the write is entitled to act on, which is why the liveness guard
// runs there too -- a memory archived by hand while this apply waited must be
// refused, not overwritten.
func (s *Service) mutateActiveMemory(ctx context.Context, id string, fn func(context.Context, core.Memory) error) error {
	idx, err := s.activeMemoryIndex(ctx, id)
	if err != nil {
		return err
	}
	return s.files.Mutate(ctx, idx.FilePath, func(ctx context.Context) error {
		mem, rerr := s.loadActiveMemory(ctx, id)
		if rerr != nil {
			return rerr
		}
		return fn(ctx, mem)
	})
}

// recordMemory appends a memory lifecycle event best-effort.
func (s *Service) recordMemory(ctx context.Context, kind core.EventKind, m core.Memory, payload map[string]any) {
	if s.events == nil {
		return
	}
	if _, err := s.events.Record(ctx, core.Event{
		Kind: kind, ProjectSlug: m.Project, ItemID: m.ID, Payload: payload,
	}); err != nil {
		s.logger.Warn("gardener: record memory event", "kind", kind, "error", err)
	}
}

// payloadString reads a string field from a proposal payload map ("" if absent
// or not a string).
func payloadString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// payloadMap reads a nested object field from a proposal payload (nil if absent).
func payloadMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// payloadBool reads a boolean field from a proposal payload (false if absent or
// not a bool).
func payloadBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

// payloadList reads an array-of-objects field from a proposal payload (nil if
// absent), e.g. a consolidate proposal's "sources".
func payloadList(m map[string]any, key string) []map[string]any {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if obj, ok := v.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}
