package gardener

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

// Plan fenced-sessions P7: the stale-copy and copy-relocate passes over
// stamped memories and the references their bodies make.

const (
	p7Ops     = "example-ops"
	p7Legal   = "example-legal"
	p7Finance = "example-finance"
)

// p7Env is a gardener over a fresh store with three confidential projects.
type p7Env struct {
	t   *testing.T
	g   *Service
	mgr *files.Manager
	ctx context.Context
	now time.Time
}

func p7NewEnv(t *testing.T) *p7Env {
	t.Helper()
	g, mgr, cx := newApplyFixture(t)
	e := &p7Env{t: t, g: g, mgr: mgr, ctx: cx(), now: time.Now().UTC().Truncate(time.Second)}
	for _, p := range []string{p7Ops, p7Legal, p7Finance} {
		isolate(t, g.db, p, core.IsolationConfidential)
	}
	return e
}

// p7Stamp is a grant stamp naming project.
func (e *p7Env) p7Stamp(project string) core.Stamp {
	return core.Stamp{Project: project, Via: core.StampViaGrant, Ref: mustID(e.t)}
}

// p7Memory writes a memory last written at updated.
func (e *p7Env) p7Memory(project, name string, updated time.Time, body string, stamps ...core.Stamp) core.Memory {
	e.t.Helper()
	m, err := e.mgr.WriteMemory(e.ctx, core.Memory{
		ID: mustID(e.t), Kind: core.KindGotcha, Name: name, Description: name + " described",
		Project: project, Body: body + "\n", Created: updated, Updated: updated, ValidFrom: updated,
		MayContain: stamps,
	})
	require.NoError(e.t, err)
	return m
}

// p7Source writes a source memory in project with content that must never
// reach a proposal.
func (e *p7Env) p7Source(project, name string, updated time.Time) core.Memory {
	e.t.Helper()
	return e.p7Memory(project, name, updated, "SOURCE-BODY-SECRET of "+name)
}

// p7Proposals lists the pending proposals of kind.
func (e *p7Env) p7Proposals(kind string) []store.Proposal {
	e.t.Helper()
	ps, err := store.PendingProposals(e.ctx, e.g.db, kind)
	require.NoError(e.t, err)
	return ps
}

// p7Run runs the whole gardener and requires every pass to finish.
func (e *p7Env) p7Run() PassResult {
	e.t.Helper()
	res, err := e.g.RunOnce(e.ctx)
	require.NoError(e.t, err)
	require.True(e.t, res.OK(), "failed passes: %v", res.Failed)
	return res
}

func p7Cite(m core.Memory) string { return core.Citation{Project: m.Project, ID: m.ID}.String() }

func TestStaleCopy_ChangedSourceIsFlaggedInTheCopysProject(t *testing.T) {
	e := p7NewEnv(t)
	copied := e.now.Add(-48 * time.Hour)
	src := e.p7Source(p7Legal, "renewal-terms", e.now.Add(-24*time.Hour)) // changed after the copy
	cp := e.p7Memory(p7Ops, "ops-renewal-copy", copied, "Renewal follows "+p7Cite(src)+".", e.p7Stamp(p7Legal))

	res := e.p7Run()
	require.Equal(t, 1, res.Archives, "the stale copy's archive proposal counts as an archive")

	ps := e.p7Proposals(store.ProposalArchive)
	require.Len(t, ps, 1)
	p := ps[0].Payload
	require.Equal(t, cp.ID, p["id"])
	require.Equal(t, p7Ops, p["project"], "filed in the copy's project")
	require.Equal(t, BasisSourceChanged, p["basis"])
	require.Equal(t, "archive:"+cp.ID, p["key"], "one archive question per memory")
	require.Equal(t, "its source "+p7Cite(src)+" changed after this copy was last written", p["reason"])
	sources := payloadObjects(p, "changed_sources")
	require.Len(t, sources, 1)
	require.Equal(t, p7Legal, sources[0]["project"])
	require.Equal(t, src.ID, sources[0]["id"])
	require.Equal(t, core.ItemKindMemory, sources[0]["kind"])

	// Names and ids only: nothing of the source's content reaches the proposal.
	raw, err := json.Marshal(ps[0].Payload)
	require.NoError(t, err)
	for _, secret := range []string{"SOURCE-BODY-SECRET", src.Name, src.Description} {
		require.NotContains(t, string(raw), secret)
	}

	// The proposal spans the source's project: an agent in the copy's project
	// is shown it only while it may read the source's too.
	scope, err := ResolveProposalScope(e.ctx, e.g.db, ps[0])
	require.NoError(t, err)
	require.True(t, scope.Resolved)
	require.Equal(t, []string{p7Legal, p7Ops}, scope.Projects)
	ok, err := NewProposalFence(e.g.db, p7Ops).Readable(e.ctx, ps[0])
	require.NoError(t, err)
	require.False(t, ok, "without a grant of the source's project, its change is not the copy's agents' to see")
	granted := access.WithGrants(e.ctx, access.JudgeGrants(e.ctx, "S1", e.now, []access.Grant{
		{ID: "G1", Project: p7Legal, From: p7Ops, Session: "S1", Expires: e.now.Add(time.Hour)},
	}))
	ok, err = NewProposalFence(e.g.db, p7Ops).Readable(granted, ps[0])
	require.NoError(t, err)
	require.True(t, ok, "an agent holding a live grant of the source's project sees it")

	// Applied, the stale copy is archived like any archive proposal's memory.
	_, err = e.g.Apply(e.ctx, ps[0].ID)
	require.NoError(t, err)
	after, found, err := store.MemoryByID(e.ctx, e.g.db, cp.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, after.InvalidAt)
}

func TestStaleCopy_UnchangedSourceProposesNothing(t *testing.T) {
	e := p7NewEnv(t)
	src := e.p7Source(p7Legal, "renewal-terms", e.now.Add(-72*time.Hour))
	e.p7Memory(p7Ops, "ops-renewal-copy", e.now.Add(-48*time.Hour), "Renewal follows "+p7Cite(src)+".", e.p7Stamp(p7Legal))
	// A source written in the same instant as its copy did not change after it.
	same := e.now.Add(-36 * time.Hour)
	src2 := e.p7Source(p7Legal, "notice-period", same)
	e.p7Memory(p7Ops, "ops-notice-copy", same, "Notice: "+p7Cite(src2), e.p7Stamp(p7Legal))

	res := e.p7Run()
	require.Zero(t, res.Archives)
	require.Empty(t, e.p7Proposals(store.ProposalArchive))
}

// A reference the pass cannot judge -- its source missing, moved out of the
// project it names, in a project sealed since, or in a fenced project the copy
// holds no stamp for -- files nothing, though every source here changed after
// its copy.
func TestStaleCopy_UnreadableSourceProposesNothing(t *testing.T) {
	e := p7NewEnv(t)
	copied := e.now.Add(-48 * time.Hour)
	changed := e.now.Add(-24 * time.Hour)
	isolate(t, e.g.db, "example-sealed", core.IsolationConfidential)

	missing := core.Citation{Project: p7Legal, ID: mustID(t)}.String()
	elsewhere := e.p7Source(p7Finance, "moved-away", changed) // cited as example-legal's
	sealed := e.p7Source("example-sealed", "vault-terms", changed)
	unheld := e.p7Source(p7Finance, "finance-terms", changed)

	e.p7Memory(p7Ops, "copy-of-missing", copied, "From "+missing, e.p7Stamp(p7Legal))
	e.p7Memory(p7Ops, "copy-of-moved", copied, "From "+core.Citation{Project: p7Legal, ID: elsewhere.ID}.String(),
		e.p7Stamp(p7Legal))
	e.p7Memory(p7Ops, "copy-of-sealed", copied, "From "+p7Cite(sealed), e.p7Stamp("example-sealed"))
	e.p7Memory(p7Ops, "copy-of-unheld", copied, "From "+p7Cite(unheld), e.p7Stamp(p7Legal))
	require.NoError(t, store.SetProjectIsolation(e.ctx, e.g.db, "example-sealed", core.IsolationSealed))

	res := e.p7Run()
	require.Zero(t, res.Archives)
	require.Empty(t, e.p7Proposals(store.ProposalArchive))
}

// Notes and tasks are sources too; an open project's item is readable from a
// confidential copy without any stamp.
func TestStaleCopy_NoteTaskAndOpenSources(t *testing.T) {
	e := p7NewEnv(t)
	copied := e.now.Add(-48 * time.Hour)
	changed := e.now.Add(-24 * time.Hour)
	_, err := store.EnsureProject(e.ctx, e.g.db, "example-open", "example-open")
	require.NoError(t, err)

	note, err := e.mgr.WriteNote(e.ctx, core.Note{ID: mustID(t), Title: "Legal write-up", Slug: "legal-write-up",
		Project: p7Legal, Body: "SOURCE-BODY-SECRET\n", Created: copied, Updated: changed})
	require.NoError(t, err)
	task := core.Task{ID: mustID(t), ProjectSlug: p7Legal, Title: "SOURCE-TITLE-SECRET", Status: core.TaskOpen,
		CreatedBy: "test", CreatedAt: copied, UpdatedAt: changed}
	require.NoError(t, store.CreateTask(e.ctx, e.g.db, task))
	open := e.p7Source("example-open", "open-fact", changed)

	cp := e.p7Memory(p7Ops, "ops-digest-copy", copied,
		"See "+core.Citation{Project: p7Legal, ID: note.ID}.String()+", "+
			core.Citation{Project: p7Legal, ID: task.ID}.String()+" and "+p7Cite(open), e.p7Stamp(p7Legal))

	e.p7Run()
	ps := e.p7Proposals(store.ProposalArchive)
	require.Len(t, ps, 1)
	require.Equal(t, cp.ID, ps[0].Payload["id"])
	var kinds []any
	for _, src := range payloadObjects(ps[0].Payload, "changed_sources") {
		kinds = append(kinds, src["kind"])
	}
	require.Equal(t, []any{core.ItemKindNote, core.ItemKindTask, core.ItemKindMemory}, kinds)
	raw, err := json.Marshal(ps[0].Payload)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SOURCE-")
}

// A dismissal answers the evidence so far: the copy is asked about again only
// once a source changes after the dismissal. A refreshed copy is not stale.
func TestStaleCopy_DismissHoldsUntilTheSourceChangesAgain(t *testing.T) {
	e := p7NewEnv(t)
	src := e.p7Source(p7Legal, "renewal-terms", e.now.Add(-24*time.Hour))
	e.p7Memory(p7Ops, "ops-renewal-copy", e.now.Add(-48*time.Hour), "Renewal follows "+p7Cite(src)+".", e.p7Stamp(p7Legal))

	e.p7Run()
	ps := e.p7Proposals(store.ProposalArchive)
	require.Len(t, ps, 1)
	e.g.now = func() time.Time { return e.now.Add(-12 * time.Hour) }
	require.NoError(t, e.g.Dismiss(e.ctx, ps[0].ID))
	e.g.now = time.Now

	e.p7Run()
	require.Empty(t, e.p7Proposals(store.ProposalArchive), "nothing changed since the dismissal")

	src.Updated = e.now.Add(-time.Hour)
	_, err := e.mgr.WriteMemory(e.ctx, src)
	require.NoError(t, err)
	e.p7Run()
	require.Len(t, e.p7Proposals(store.ProposalArchive), 1, "the source changed again after the dismissal")
}

// The passes read only stamped memories: with isolation.requests off nothing
// is stamped, so a memory citing a changed source is left alone.
func TestStaleCopy_AnUnstampedMemoryIsNoCopy(t *testing.T) {
	e := p7NewEnv(t)
	src := e.p7Source(p7Legal, "renewal-terms", e.now.Add(-24*time.Hour))
	e.p7Memory(p7Ops, "ops-note", e.now.Add(-48*time.Hour), "Renewal follows "+p7Cite(src)+".")

	res := e.p7Run()
	require.Zero(t, res.Total())
}

func TestCopyRelocate_SourcesAllInOneProject(t *testing.T) {
	e := p7NewEnv(t)
	at := e.now.Add(-time.Hour)
	srcA := e.p7Source(p7Legal, "term-a", e.now.Add(-48*time.Hour))
	srcB := e.p7Source(p7Legal, "term-b", e.now.Add(-48*time.Hour))
	cp := e.p7Memory(p7Ops, "legal-terms-digest", at, "From "+p7Cite(srcA)+" and "+p7Cite(srcB)+".", e.p7Stamp(p7Legal))

	res := e.p7Run()
	require.Equal(t, 1, res.Relocations)
	require.Zero(t, res.Archives, "unchanged sources: not stale")
	ps := e.p7Proposals(store.ProposalRelocate)
	require.Len(t, ps, 1)
	p := ps[0].Payload
	require.Equal(t, relocateKey(p7Legal, cp.ID), p["key"])
	require.Equal(t, BasisCitedSources, p["basis"])
	require.Equal(t, p7Ops, p["from"])
	require.Equal(t, p7Legal, p["to"])
	require.Equal(t, string(core.IsolationConfidential), p["isolation"])
	require.Equal(t, []string{p7Cite(srcA), p7Cite(srcB)}, payloadStrings(p, "cited"))
	scope, err := ResolveProposalScope(e.ctx, e.g.db, ps[0])
	require.NoError(t, err)
	require.Equal(t, []string{p7Legal, p7Ops}, scope.Projects)

	// Asked once: the next pass files nothing new.
	require.Zero(t, e.p7Run().Relocations)

	// Applied, the copy moves with its stamps; undone, it moves back.
	_, err = e.g.Apply(e.ctx, ps[0].ID)
	require.NoError(t, err)
	moved, found, err := store.MemoryByID(e.ctx, e.g.db, cp.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, p7Legal, moved.Project)
	full, err := e.mgr.Store().ReadMemory(moved.FilePath)
	require.NoError(t, err)
	require.Equal(t, []string{p7Legal}, []string{full.MayContain[0].Project}, "the file keeps its stamp")
	require.NoError(t, e.g.Undo(e.ctx, ps[0].ID))
	back, _, err := store.MemoryByID(e.ctx, e.g.db, cp.ID)
	require.NoError(t, err)
	require.Equal(t, p7Ops, back.Project)
}

func TestCopyRelocate_NotWhenSourcesSpanProjects(t *testing.T) {
	e := p7NewEnv(t)
	at := e.now.Add(-time.Hour)
	old := e.now.Add(-48 * time.Hour)
	legal := e.p7Source(p7Legal, "term-a", old)
	finance := e.p7Source(p7Finance, "ledger-a", old)

	// Cites two projects.
	e.p7Memory(p7Ops, "two-cited", at, p7Cite(legal)+" "+p7Cite(finance), e.p7Stamp(p7Legal), e.p7Stamp(p7Finance))
	// Cites one, but may hold another's content: moving it would carry that along.
	e.p7Memory(p7Ops, "two-stamped", at, p7Cite(legal), e.p7Stamp(p7Legal), e.p7Stamp(p7Finance))
	// Stamped, cites nothing: nothing says what it is about.
	e.p7Memory(p7Ops, "no-cites", at, "plain words", e.p7Stamp(p7Legal))
	// Cites an id example-legal does not hold.
	e.p7Memory(p7Ops, "missing-cite", at, core.Citation{Project: p7Legal, ID: mustID(t)}.String(), e.p7Stamp(p7Legal))
	// Stamped for a project its citation is not in.
	e.p7Memory(p7Ops, "stamp-elsewhere", at, p7Cite(finance), e.p7Stamp(p7Legal))

	require.Zero(t, e.p7Run().Relocations)
	require.Empty(t, e.p7Proposals(store.ProposalRelocate))
}

// A copy written in a sealed home from a confidential project's material is
// proposed back into that project too: the move is less fenced, so the console
// warns before the owner applies it.
func TestCopyRelocate_OutOfASealedHome(t *testing.T) {
	e := p7NewEnv(t)
	isolate(t, e.g.db, "example-sealed", core.IsolationSealed)
	src := e.p7Source(p7Legal, "term-a", e.now.Add(-48*time.Hour))
	cp := e.p7Memory("example-sealed", "carried-terms", e.now.Add(-time.Hour), "From "+p7Cite(src),
		core.Stamp{Project: p7Legal, Via: core.StampViaMove, Ref: mustID(t)})

	require.Equal(t, 1, e.p7Run().Relocations)
	ps := e.p7Proposals(store.ProposalRelocate)
	require.Len(t, ps, 1)
	require.Equal(t, cp.ID, ps[0].Payload["id"])
	require.Equal(t, "example-sealed", ps[0].Payload["from"])
}

// A digest folding stamped findings writes a note carrying their stamps; a
// digest of unstamped sessions carries no key at all.
func TestDigest_UnionsTheFoldedFindingsStamps(t *testing.T) {
	e := p7NewEnv(t)
	e.g.chat = fakeChat{out: "## Digest\n- did things"}
	// The stamp names the conversation's own session in example-legal (a
	// fenced home's native flow), so the index derives its kind as a stamp
	// read back from item_provenance always does.
	home := mustID(t)
	require.NoError(t, store.CreateSession(e.ctx, e.g.db, core.Session{
		ID: home, Name: "cc/p7-home", ProjectSlug: p7Legal, Status: core.SessionActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}))
	legal := core.Stamp{Project: p7Legal, Via: core.StampViaSession, Ref: home}
	var stamped string
	for i, project := range []string{p7Ops, p7Ops, p7Finance, p7Finance} {
		id := mustID(t)
		require.NoError(t, store.CreateSession(e.ctx, e.g.db, core.Session{
			ID: id, Name: "cc/p7-" + string(rune('a'+i)), ProjectSlug: project, Status: core.SessionCompleted,
			Findings: "a finding", CreatedAt: e.now, UpdatedAt: e.now,
		}))
		if i == 0 {
			stamped = id
		}
	}
	require.NoError(t, store.StampWorkItem(e.ctx, e.g.db, core.ItemKindSession, stamped, p7Ops, []core.Stamp{legal}, e.now))

	require.Equal(t, 2, e.p7Run().Digests)
	var opsDigest store.Proposal
	for _, p := range e.p7Proposals(store.ProposalDigest) {
		switch p.Payload["project"] {
		case p7Ops:
			opsDigest = p
		case p7Finance:
			_, has := p.Payload[payloadStampsKey]
			require.False(t, has, "a digest of unstamped findings carries no stamps key")
		}
	}
	require.NotEmpty(t, opsDigest.ID)
	res, err := e.g.Apply(e.ctx, opsDigest.ID)
	require.NoError(t, err)
	note, ok, err := store.NoteByID(e.ctx, e.g.db, res["note_id"].(string))
	require.NoError(t, err)
	require.True(t, ok)
	full, err := e.mgr.Store().ReadNote(note.FilePath)
	require.NoError(t, err)
	require.Equal(t, []core.Stamp{legal}, full.MayContain)
	stamps, err := store.ProvenanceForItem(e.ctx, e.g.db, core.ItemKindNote, note.ID)
	require.NoError(t, err)
	require.Len(t, stamps, 1, "the note's stamp is mirrored into the index")
	require.Equal(t, p7Legal, stamps[0].Source)
}

// A digest proposal whose recorded stamps do not read is refused rather than
// applied without them.
func TestDigest_UnreadableStampsRefuseTheApply(t *testing.T) {
	e := p7NewEnv(t)
	p, err := store.CreateProposal(e.ctx, e.g.db, store.ProposalDigest, map[string]any{
		"key": "digest:example-ops:2026-11", "project": p7Ops, "title": "Session digest: example-ops -- 2026-11",
		"body": "## November\n- things", payloadStampsKey: "not a list",
	})
	require.NoError(t, err)
	_, err = e.g.Apply(e.ctx, p.ID)
	require.ErrorContains(t, err, "does not read as provenance stamps")
	still, _, err := store.ProposalByID(e.ctx, e.g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, still.Status)
}

// A stale copy's archive spans its sources' projects, and fails closed when it
// names a source it cannot attribute.
func TestResolveProposalScope_StaleCopySpansItsSources(t *testing.T) {
	f := newScopeFixture(t)
	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    []string
	}{
		{"the copy's project and every source's", map[string]any{
			"id": f.alphaMem, "project": "alpha", "basis": BasisSourceChanged,
			"changed_sources": []any{
				map[string]any{"project": "example-legal", "id": f.betaMem},
				map[string]any{"project": "", "id": f.globMem},
			},
		}, []string{"", "alpha", "example-legal"}},
		{"a stale copy naming no source", map[string]any{
			"id": f.alphaMem, "project": "alpha", "basis": BasisSourceChanged,
		}, nil},
		{"a source without its project", map[string]any{
			"id": f.alphaMem, "project": "alpha", "basis": BasisSourceChanged,
			"changed_sources": []any{map[string]any{"id": f.betaMem}},
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, err := ResolveProposalScope(f.ctx, f.db, store.Proposal{Kind: store.ProposalArchive, Payload: tc.payload})
			require.NoError(t, err)
			if tc.want == nil {
				require.False(t, scope.Resolved, "unattributable is unreadable")
				return
			}
			require.True(t, scope.Resolved)
			require.Equal(t, tc.want, scope.Projects)
		})
	}
}

// A consolidation whose source went inactive before the apply still carries
// that source's stamps: the unified body was written from it.
func TestApplyConsolidate_CarriesARetiredSourcesStamps(t *testing.T) {
	e := p7NewEnv(t)
	at := e.now.Add(-time.Hour)
	a := e.p7Memory(p7Ops, "fold-a", at, "a", e.p7Stamp(p7Legal))
	b := e.p7Memory(p7Ops, "fold-b", at, "b", e.p7Stamp(p7Finance))
	archiveByHand(t, e.g, e.mgr, b.ID)

	c, err := store.CreateProposal(e.ctx, e.g.db, store.ProposalConsolidate, map[string]any{
		"key": "consolidate:" + a.ID + "|" + b.ID, "name": "fold-unified", "kind": "runbook",
		"project": p7Ops, "description": "the unified fold", "body": "# Fold\ncombined",
		"sources": []any{map[string]any{"id": a.ID, "name": a.Name}, map[string]any{"id": b.ID, "name": b.Name}},
	})
	require.NoError(t, err)
	_, err = e.g.Apply(e.ctx, c.ID)
	require.NoError(t, err)
	unified, ok, err := store.MemoryByName(e.ctx, e.g.db, p7Ops, "fold-unified")
	require.NoError(t, err)
	require.True(t, ok)
	full, err := e.mgr.Store().ReadMemory(unified.FilePath)
	require.NoError(t, err)
	var projects []string
	for _, st := range full.MayContain {
		projects = append(projects, st.Project)
	}
	require.ElementsMatch(t, []string{p7Legal, p7Finance}, projects)
}
