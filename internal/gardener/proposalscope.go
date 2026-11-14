// Payload -> project resolution for proposals, and the isolation fence that
// rides on it.
//
// A gardener_proposals row has no project column: the project a proposal is
// ABOUT lives inside its untyped payload, spelled differently per kind -- a
// memory id to load, a slug carried directly, a from/to pair for a move, a whole
// set of not-yet-created slugs for a split. The agent-facing tool surface cannot
// fence what it cannot attribute, so that spelling is decoded here, once, for
// every kind in store.ProposalKinds.
//
// Two rules make this a fence rather than a convenience:
//
//  1. A proposal spanning two projects is readable only when the caller may read
//     BOTH of them, never either. See ProposalFence.Readable.
//  2. A payload this cannot attribute FAILS CLOSED -- an unresolved scope is
//     readable by nobody at the tool surface. See ProposalScope.Resolved.
//
// The console reaches the gardener directly (store.PendingProposals and
// Service.Apply), never through this, and stays exempt by design: the threat
// model is agent-to-agent leakage, not hiding from the owner.
package gardener

import (
	"context"
	"database/sql"
	"slices"

	"github.com/arctop/seamless/internal/store"
)

// ProposalScope is every project scope one proposal touches: the projects its
// payload names and the projects applying it would change. "" is the global
// scope -- a real answer, not a missing one.
type ProposalScope struct {
	// Projects holds the distinct scopes, sorted: one for most kinds, two for a
	// move (its source and its destination), and the whole target set for a split.
	Projects []string
	// Resolved is false when the payload cannot be attributed at all: an unknown
	// kind, a payload missing the key its kind is defined by, or an anchor whose
	// memory/note no longer exists. It never degrades to "probably global" -- a
	// surface that cannot prove which project a row is about must not show it.
	Resolved bool
}

// ResolveProposalScope decodes which project(s) a proposal is about.
//
// Memory- and note-anchored kinds resolve from BOTH readings: the project the
// payload recorded when the proposal was raised, and the project the item sits in
// NOW. They differ exactly when the item moved since -- and their union is the
// conservative answer, because the payload still discloses the old project while
// an apply would touch the new one.
func ResolveProposalScope(ctx context.Context, db *sql.DB, p store.Proposal) (ProposalScope, error) {
	set := newScopeSet()
	switch p.Kind {
	case store.ProposalArchive, store.ProposalRekind:
		// One memory, named by id with the project it was in alongside.
		// (staleness, stale-stage, dead-weight, and the request ops.)
		if err := set.addAnchored(ctx, db, p.Payload, "id", "project", memoryAnchor); err != nil {
			return ProposalScope{}, err
		}
		// A stale copy's archive also says when each cited source changed: a
		// fact about the source's project, so the scope spans every one of
		// them (stale-copy, copies.go). Each needs its project key.
		for _, src := range payloadObjects(p.Payload, "changed_sources") {
			set.addKey(src, "project")
		}
		if p.Payload["basis"] == BasisSourceChanged && len(payloadObjects(p.Payload, "changed_sources")) == 0 {
			set.fail() // a stale copy that names no source names nothing knowable
		}

	case store.ProposalMerge:
		// Two memories, each a brief object. Both sides count: the payload names
		// both, and the apply supersedes one by the other.
		for _, side := range []string{"keep", "drop"} {
			brief := payloadMap(p.Payload, side)
			if brief == nil {
				set.fail()
				continue
			}
			if err := set.addAnchored(ctx, db, brief, "id", "project", memoryAnchor); err != nil {
				return ProposalScope{}, err
			}
		}

	case store.ProposalConsolidate:
		// The unified memory's own project plus every source it supersedes -- a
		// consolidate is a many-to-one merge and spans every one of them.
		set.addKey(p.Payload, "project")
		sources := payloadObjects(p.Payload, "sources")
		if len(sources) == 0 {
			set.fail() // a consolidate with no sources supersedes nothing knowable
		}
		for _, src := range sources {
			if err := set.addAnchored(ctx, db, src, "id", "project", memoryAnchor); err != nil {
				return ProposalScope{}, err
			}
		}

	case store.ProposalReproject, store.ProposalRelocate:
		// A move spans two scopes by construction. "to" is the destination the
		// apply writes into and is required (applyReproject errors without it);
		// "from" plus the memory's current project is the source side.
		if to, ok := payloadProject(p.Payload, "to"); ok && to != "" {
			set.add(to)
		} else {
			set.fail()
		}
		if err := set.addAnchored(ctx, db, p.Payload, "id", "from", memoryAnchor); err != nil {
			return ProposalScope{}, err
		}

	case store.ProposalSplit:
		// The source being emptied plus every project the setup would create.
		// The new slugs do not exist yet, so they are open by construction -- they
		// are still in scope because applying creates and parents them.
		if source := payloadString(p.Payload, "source_project"); source != "" {
			set.add(source)
		} else {
			set.fail()
		}
		for _, child := range payloadObjects(p.Payload, "children") {
			if slug := payloadString(child, "slug"); slug != "" {
				set.add(slug)
			}
		}
		if slug := payloadString(payloadMap(p.Payload, "shared"), "slug"); slug != "" {
			set.add(slug)
		}

	case store.ProposalAbandonPlan, store.ProposalShipPlan:
		// A captured plan note, named by id with its project alongside.
		if err := set.addAnchored(ctx, db, p.Payload, "id", "project", noteAnchor); err != nil {
			return ProposalScope{}, err
		}

	case store.ProposalDigest, store.ProposalMemoryWanted, store.ProposalToolError:
		// Derived from a project's own sessions, recall misses, or tool errors,
		// and each carries that project directly. The key must be PRESENT: "" is
		// the global scope and a legitimate value, so an absent key cannot be read
		// as one.
		set.addKey(p.Payload, "project")

	default:
		// An unknown kind is the fail-closed case that matters most: a kind added
		// later without a rule here must disappear from the agent surface rather
		// than stream an unattributed payload to every caller.
		set.fail()
	}
	return set.result(), nil
}

// ProposalFence answers "may this caller be shown that proposal?" for one
// caller, reusing the passes' scopeGate so each project's verdict is asked once.
// store.CanRead owns the matrix; nothing here restates it.
type ProposalFence struct {
	db   *sql.DB
	gate *scopeGate
}

// NewProposalFence builds a fence for a caller bound to callerProject ("" is the
// global scope: it sees everything except what a fenced project keeps to itself).
func NewProposalFence(db *sql.DB, callerProject string) *ProposalFence {
	return &ProposalFence{db: db, gate: newScopeGate(db, callerProject)}
}

// Readable reports whether the caller may be shown p.
//
// A proposal spanning two projects needs BOTH readable, never either. The
// payload is one object naming every side -- a merge carries both memories'
// names, descriptions and similarity score, a move carries its source and its
// destination -- and the tool surface renders it whole or not at all. Showing it
// on the strength of the readable side alone would tell an outsider that a
// memory by that name exists inside the fence, and a dedup merge would even
// score how close it is to one of their own. The outbound promise is that an
// outsider learns NOTHING about what a confidential project knows, so a spanning
// proposal collapses to its least permissive side.
//
// An unresolved scope is unreadable, full stop. Nothing is lost by that: the
// console is exempt and still shows the row to the owner.
func (f *ProposalFence) Readable(ctx context.Context, p store.Proposal) (bool, error) {
	scope, err := ResolveProposalScope(ctx, f.db, p)
	if err != nil {
		return false, err
	}
	if !scope.Resolved {
		return false, nil
	}
	for _, project := range scope.Projects {
		ok, err := f.gate.readable(ctx, project)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// Filter drops the proposals the caller may not read, preserving order.
//
// The drop is silent on purpose. A "3 withheld" count would be a probe in its
// own right: it reports how much a fenced project has going on, which is exactly
// the shape-of-the-corpus fact the outbound fence exists to withhold.
func (f *ProposalFence) Filter(ctx context.Context, ps []store.Proposal) ([]store.Proposal, error) {
	out := make([]store.Proposal, 0, len(ps))
	for _, p := range ps {
		ok, err := f.Readable(ctx, p)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// FencedProjects returns the projects a proposal touches that are isolated NOW,
// each with the state fencing it. Empty means nothing it touches is fenced.
//
// This is the apply-time isolation re-check in the only form that fits an owner
// surface. Agents are BLOCKED across the fence (the tool surface refuses); the
// owner is not -- the console is exempt by design -- but a proposal raised before
// a tighten carries evidence that predates the fence, so applying it blind is not
// an informed choice. Reporting the current state lets the console mark the row
// (e.g. "moves a memory out of vault, now sealed") without the gardener deciding
// for the owner.
//
// It reports on whatever the scope resolved; an unattributable payload simply has
// no projects to report, which is safe here because this only ever adds a warning
// to a surface that already shows everything.
func FencedProjects(ctx context.Context, db *sql.DB, p store.Proposal) ([]store.ProjectIsolation, error) {
	scope, err := ResolveProposalScope(ctx, db, p)
	if err != nil {
		return nil, err
	}
	return store.IsolatedSlugs(ctx, db, scope.Projects)
}

// scopeSet accumulates the projects one proposal touches, and remembers whether
// anything it needed was missing.
type scopeSet struct {
	projects map[string]struct{}
	ok       bool
}

func newScopeSet() *scopeSet {
	return &scopeSet{projects: map[string]struct{}{}, ok: true}
}

// add records one project scope ("" is global).
func (s *scopeSet) add(project string) { s.projects[project] = struct{}{} }

// fail marks the scope unresolvable. It is never reversed: one unattributable
// anchor makes the whole proposal unattributable, since the payload names every
// side whether or not this could resolve them all.
func (s *scopeSet) fail() { s.ok = false }

// addKey records the project a payload carries under key, failing the scope when
// the key is absent. Presence rather than emptiness is the test: "" is the global
// scope, so payloadString cannot tell a global proposal from a malformed one.
func (s *scopeSet) addKey(payload map[string]any, key string) {
	project, ok := payloadProject(payload, key)
	if !ok {
		s.fail()
		return
	}
	s.add(project)
}

// addAnchored records both readings of an item-anchored payload: the project the
// payload recorded under projectKey, and the project the item at idKey lives in
// now. Either alone is enough; neither fails the scope.
func (s *scopeSet) addAnchored(ctx context.Context, db *sql.DB, payload map[string]any, idKey, projectKey string, lookup anchorLookup) error {
	got := false
	if project, ok := payloadProject(payload, projectKey); ok {
		s.add(project)
		got = true
	}
	if id := payloadString(payload, idKey); id != "" {
		project, found, err := lookup(ctx, db, id)
		if err != nil {
			return err
		}
		if found {
			s.add(project)
			got = true
		}
	}
	if !got {
		s.fail()
	}
	return nil
}

// result renders the accumulated scope. An empty project set is unresolved
// however it got there: there is nothing to check a fence against.
func (s *scopeSet) result() ProposalScope {
	out := make([]string, 0, len(s.projects))
	for project := range s.projects {
		out = append(out, project)
	}
	slices.Sort(out)
	return ProposalScope{Projects: out, Resolved: s.ok && len(out) > 0}
}

// anchorLookup resolves an item id to the project it currently lives in. found
// is false when the item is gone (deleted, or never indexed).
type anchorLookup func(ctx context.Context, db *sql.DB, id string) (string, bool, error)

// memoryAnchor resolves a memory id. Invalid (archived/superseded) memories still
// resolve: a proposal naming one still names its project.
func memoryAnchor(ctx context.Context, db *sql.DB, id string) (string, bool, error) {
	m, ok, err := store.MemoryByID(ctx, db, id)
	if err != nil {
		return "", false, err
	}
	return m.Project, ok, nil
}

// noteAnchor resolves a note id (the captured-plan kinds).
func noteAnchor(ctx context.Context, db *sql.DB, id string) (string, bool, error) {
	n, ok, err := store.NoteByID(ctx, db, id)
	if err != nil {
		return "", false, err
	}
	return n.Project, ok, nil
}

// payloadProject reads a project-slug field, reporting PRESENCE separately from
// value. Every other payload reader collapses absent onto "", which is exactly
// the distinction a fence cannot afford: "" is the global scope.
func payloadProject(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok
}

// payloadObjects reads an array-of-objects payload field, tolerating both shapes
// a payload can hold: []any, which is what the JSON round trip through the store
// produces, and []map[string]any, which a proposal still in memory from
// CreateProposal keeps. payloadList (the apply path) only ever sees the former.
func payloadObjects(m map[string]any, key string) []map[string]any {
	if m == nil {
		return nil
	}
	switch raw := m[key].(type) {
	case []any:
		out := make([]map[string]any, 0, len(raw))
		for _, v := range raw {
			if obj, ok := v.(map[string]any); ok {
				out = append(out, obj)
			}
		}
		return out
	case []map[string]any:
		return raw
	default:
		return nil
	}
}
