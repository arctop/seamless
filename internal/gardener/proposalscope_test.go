package gardener

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// scopeFixture is the corpus every resolver case reads: one memory per scope
// (an open project, a second open project, the global scope) plus a captured
// plan note, so an anchor can be resolved through the store rather than only
// from the payload.
type scopeFixture struct {
	ctx                        context.Context
	db                         *sql.DB
	mgr                        *files.Manager
	alphaMem, betaMem, globMem string
	planNote                   string
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	return scopeFixture{
		ctx:      ctx,
		db:       db,
		mgr:      mgr,
		alphaMem: seedScopeMemory(t, mgr, "alpha-thing", "alpha"),
		betaMem:  seedScopeMemory(t, mgr, "beta-thing", "beta"),
		globMem:  seedScopeMemory(t, mgr, "global-thing", ""),
		planNote: seedScopeNote(t, mgr, "Captured plan", "alpha"),
	}
}

// seedScopeMemory writes a memory and returns its id (writeMem does not).
func seedScopeMemory(t *testing.T, mgr *files.Manager, name, project string) string {
	t.Helper()
	id := mustID(t)
	now := time.Now().UTC()
	_, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: core.KindReference, Name: name, Description: name + " description",
		Project: project, Body: "body text\n",
		Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)
	return id
}

func seedScopeNote(t *testing.T, mgr *files.Manager, title, project string) string {
	t.Helper()
	id := mustID(t)
	now := time.Now().UTC()
	_, err := mgr.WriteNote(context.Background(), core.Note{
		ID: id, Title: title, Slug: core.Slugify(title), Description: "a captured plan",
		Project: project, Body: "plan body\n", Created: now, Updated: now,
	})
	require.NoError(t, err)
	return id
}

// TestResolveProposalScope_EveryKind is the kind -> payload-key table, executable.
// Every kind in store.ProposalKinds appears; a new kind that lands without a rule
// falls into the default branch and is caught by the fail-closed test below.
func TestResolveProposalScope_EveryKind(t *testing.T) {
	f := newScopeFixture(t)

	for _, tc := range []struct {
		name    string
		kind    string
		payload map[string]any
		want    []string
	}{
		{
			name:    "archive: id + project",
			kind:    store.ProposalArchive,
			payload: map[string]any{"id": f.alphaMem, "name": "alpha-thing", "project": "alpha"},
			want:    []string{"alpha"},
		},
		{
			// The staleness passes always stamp "project", but a hand-seeded or
			// older payload may not -- the anchor id still attributes it.
			name:    "archive: id alone still attributes the proposal",
			kind:    store.ProposalArchive,
			payload: map[string]any{"id": f.alphaMem},
			want:    []string{"alpha"},
		},
		{
			// The union that matters: the payload froze "beta" at proposal time,
			// the memory lives in "alpha" now. Both are in scope -- the payload
			// discloses one, the apply would touch the other.
			name:    "archive: a memory that moved since spans both readings",
			kind:    store.ProposalArchive,
			payload: map[string]any{"id": f.alphaMem, "project": "beta"},
			want:    []string{"alpha", "beta"},
		},
		{
			name:    "rekind: id + project",
			kind:    store.ProposalRekind,
			payload: map[string]any{"id": f.betaMem, "from": "reference", "to": "gotcha", "project": "beta"},
			want:    []string{"beta"},
		},
		{
			name: "merge: both briefs count",
			kind: store.ProposalMerge,
			payload: map[string]any{
				"score": 0.91,
				"keep":  map[string]any{"id": f.alphaMem, "name": "alpha-thing", "project": "alpha"},
				"drop":  map[string]any{"id": f.betaMem, "name": "beta-thing", "project": "beta"},
			},
			want: []string{"alpha", "beta"},
		},
		{
			name: "consolidate: the unified target plus every source",
			kind: store.ProposalConsolidate,
			payload: map[string]any{
				"name": "unified", "project": "alpha", "body": "b",
				"sources": []any{
					map[string]any{"id": f.alphaMem, "name": "alpha-thing"},
					map[string]any{"id": f.betaMem, "name": "beta-thing"},
				},
			},
			want: []string{"alpha", "beta"},
		},
		{
			name:    "reproject: from and to",
			kind:    store.ProposalReproject,
			payload: map[string]any{"id": f.alphaMem, "name": "alpha-thing", "from": "alpha", "to": "beta"},
			want:    []string{"alpha", "beta"},
		},
		{
			// The provenance audit: a global memory pulled back inside a fence.
			// The global scope is a real resolved answer, not a missing one.
			name:    "relocate: the global source and the fenced destination",
			kind:    store.ProposalRelocate,
			payload: map[string]any{"id": f.globMem, "name": "global-thing", "from": "", "to": "vault"},
			want:    []string{"", "vault"},
		},
		{
			name: "split: the source plus every project it would create",
			kind: store.ProposalSplit,
			payload: map[string]any{
				"source_project": "alpha",
				"children": []any{
					map[string]any{"slug": "alpha-ios", "label": "iOS"},
					map[string]any{"slug": "alpha-android", "label": "Android"},
				},
				"shared": map[string]any{"slug": "alpha-mobile", "label": "Shared"},
			},
			want: []string{"alpha", "alpha-android", "alpha-ios", "alpha-mobile"},
		},
		{
			name:    "abandon_plan: the note's project",
			kind:    store.ProposalAbandonPlan,
			payload: map[string]any{"id": f.planNote, "title": "Captured plan", "project": "alpha"},
			want:    []string{"alpha"},
		},
		{
			name:    "ship_plan: the note id alone attributes it",
			kind:    store.ProposalShipPlan,
			payload: map[string]any{"id": f.planNote, "title": "Captured plan"},
			want:    []string{"alpha"},
		},
		{
			name:    "digest: the project it summarizes",
			kind:    store.ProposalDigest,
			payload: map[string]any{"project": "beta", "month": "2026-07", "title": "t", "body": "b"},
			want:    []string{"beta"},
		},
		{
			name:    "digest: a global digest resolves to the global scope",
			kind:    store.ProposalDigest,
			payload: map[string]any{"project": "", "month": "2026-07", "title": "t", "body": "b"},
			want:    []string{""},
		},
		{
			name:    "memory_wanted: the project whose recalls missed",
			kind:    store.ProposalMemoryWanted,
			payload: map[string]any{"project": "alpha", "signature": "dfu-mode", "suggested_title": "dfu mode"},
			want:    []string{"alpha"},
		},
		{
			name:    "tool_error: the project whose agents hit the error",
			kind:    store.ProposalToolError,
			payload: map[string]any{"project": "beta", "surface": "tool", "name": "tasks_claim"},
			want:    []string{"beta"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, err := ResolveProposalScope(f.ctx, f.db, store.Proposal{Kind: tc.kind, Payload: tc.payload})
			require.NoError(t, err)
			require.True(t, scope.Resolved, "the kind must be attributable")
			require.Equal(t, tc.want, scope.Projects)
		})
	}
}

// TestResolveProposalScope_FailsClosed pins the direction an unattributable
// payload has to fail in. Every case here used to be a proposal streamed to any
// caller with its payload intact; now it resolves to nothing, and nothing is
// readable at the tool surface (the console is exempt and still shows it).
func TestResolveProposalScope_FailsClosed(t *testing.T) {
	f := newScopeFixture(t)
	gone := mustID(t) // a well-formed id that indexes nothing

	for _, tc := range []struct {
		name    string
		kind    string
		payload map[string]any
	}{
		{
			// The case that matters most: a kind added later without a rule in
			// the switch must vanish from the agent surface, not stream an
			// unattributed payload to every caller.
			name:    "an unknown kind",
			kind:    "teleport",
			payload: map[string]any{"project": "alpha"},
		},
		{
			name:    "archive whose memory is gone and whose payload named no project",
			kind:    store.ProposalArchive,
			payload: map[string]any{"id": gone},
		},
		{
			name:    "merge missing one side of the pair",
			kind:    store.ProposalMerge,
			payload: map[string]any{"keep": map[string]any{"id": f.alphaMem, "project": "alpha"}},
		},
		{
			name:    "merge whose second memory is gone and unattributed",
			kind:    store.ProposalMerge,
			payload: map[string]any{"keep": map[string]any{"id": f.alphaMem, "project": "alpha"}, "drop": map[string]any{"id": gone}},
		},
		{
			name:    "consolidate with no sources",
			kind:    store.ProposalConsolidate,
			payload: map[string]any{"name": "unified", "project": "alpha", "body": "b"},
		},
		{
			name:    "reproject with no destination",
			kind:    store.ProposalReproject,
			payload: map[string]any{"id": f.alphaMem, "from": "alpha"},
		},
		{
			name:    "relocate whose destination is empty",
			kind:    store.ProposalRelocate,
			payload: map[string]any{"id": f.globMem, "from": "", "to": ""},
		},
		{
			name:    "split with no source project",
			kind:    store.ProposalSplit,
			payload: map[string]any{"children": []any{map[string]any{"slug": "a"}, map[string]any{"slug": "b"}}},
		},
		{
			// Presence, not emptiness: "" is the global scope, so an absent key
			// cannot be read as one.
			name:    "digest with no project key at all",
			kind:    store.ProposalDigest,
			payload: map[string]any{"month": "2026-07", "title": "t", "body": "b"},
		},
		{
			name:    "memory_wanted with no project key",
			kind:    store.ProposalMemoryWanted,
			payload: map[string]any{"signature": "dfu-mode"},
		},
		{
			name:    "tool_error with no project key",
			kind:    store.ProposalToolError,
			payload: map[string]any{"surface": "tool", "name": "tasks_claim"},
		},
		{
			name:    "abandon_plan whose note is gone and unattributed",
			kind:    store.ProposalAbandonPlan,
			payload: map[string]any{"id": gone},
		},
		{
			name:    "an empty payload",
			kind:    store.ProposalArchive,
			payload: map[string]any{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := store.Proposal{ID: mustID(t), Kind: tc.kind, Payload: tc.payload}
			scope, err := ResolveProposalScope(f.ctx, f.db, p)
			require.NoError(t, err)
			require.False(t, scope.Resolved, "an unattributable payload must not resolve")

			// Fail closed all the way to the fence: even the global scope, which
			// reads every open project, is not shown a row nothing can attribute.
			readable, err := NewProposalFence(f.db, "").Readable(f.ctx, p)
			require.NoError(t, err)
			require.False(t, readable, "an unresolved scope is readable by nobody")
		})
	}
}

// TestProposalFence_SpanningProposalNeedsEverySideReadable is the either-vs-both
// decision, pinned. A proposal naming two projects is shown only to a caller that
// may read BOTH: the payload is one object carrying both memories' names and
// descriptions, so showing it on the strength of the readable side alone would
// tell an outsider what lives inside the fence.
//
// "Both" is not "nobody": the confidential project's own session reads the open
// project too, so it still sees the spanning row. Only the outside caller loses it.
func TestProposalFence_SpanningProposalNeedsEverySideReadable(t *testing.T) {
	f := newScopeFixture(t)
	confMem := seedScopeMemory(t, f.mgr, "conf-thing", "conf")
	isolate(t, f.db, "conf", core.IsolationConfidential)
	isolate(t, f.db, "vault", core.IsolationSealed)

	merge := func(a, aProject, b, bProject string) store.Proposal {
		return store.Proposal{ID: mustID(t), Kind: store.ProposalMerge, Payload: map[string]any{
			"keep": map[string]any{"id": a, "project": aProject},
			"drop": map[string]any{"id": b, "project": bProject},
		}}
	}
	openPair := merge(f.alphaMem, "alpha", f.betaMem, "beta")
	spanning := merge(f.alphaMem, "alpha", confMem, "conf")
	inside := merge(confMem, "conf", confMem, "conf")
	intoVault := store.Proposal{ID: mustID(t), Kind: store.ProposalReproject, Payload: map[string]any{
		"id": f.alphaMem, "from": "alpha", "to": "vault",
	}}

	for _, tc := range []struct {
		name   string
		caller string
		want   map[string]bool // proposal label -> visible
	}{
		{
			name: "the global scope loses everything touching a fence", caller: "",
			want: map[string]bool{"open": true, "spanning": false, "inside": false, "vault": false},
		},
		{
			name: "an unrelated open project sees exactly the same", caller: "alpha",
			want: map[string]bool{"open": true, "spanning": false, "inside": false, "vault": false},
		},
		{
			// It reads its own project AND the open ones, so the spanning row is
			// legitimately its to see -- nothing leaves the fence by showing it here.
			name: "the confidential project's own session sees its side and the spanning row", caller: "conf",
			want: map[string]bool{"open": true, "spanning": true, "inside": true, "vault": false},
		},
		{
			// Sealed fences inbound too, so even a proposal naming its own project
			// alongside an outside one is not shown.
			name: "a sealed session reads nothing outside itself", caller: "vault",
			want: map[string]bool{"open": false, "spanning": false, "inside": false, "vault": false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fence := NewProposalFence(f.db, tc.caller)
			for label, p := range map[string]store.Proposal{
				"open": openPair, "spanning": spanning, "inside": inside, "vault": intoVault,
			} {
				got, err := fence.Readable(f.ctx, p)
				require.NoError(t, err)
				require.Equal(t, tc.want[label], got, "proposal %q", label)
			}
		})
	}
}

// TestProposalFence_FilterDropsSilently pins the listing contract: the withheld
// rows simply are not there. No marker, no count -- a "2 withheld" would itself
// report how much a fenced project has going on, which is the shape-of-the-corpus
// fact the outbound fence exists to withhold.
func TestProposalFence_FilterDropsSilently(t *testing.T) {
	f := newScopeFixture(t)
	confMem := seedScopeMemory(t, f.mgr, "conf-thing", "conf")
	isolate(t, f.db, "conf", core.IsolationConfidential)

	archive := func(id, project string) store.Proposal {
		return store.Proposal{ID: mustID(t), Kind: store.ProposalArchive,
			Payload: map[string]any{"id": id, "project": project}}
	}
	all := []store.Proposal{
		archive(f.alphaMem, "alpha"),
		archive(confMem, "conf"),
		archive(f.globMem, ""),
	}

	kept, err := NewProposalFence(f.db, "").Filter(f.ctx, all)
	require.NoError(t, err)
	require.Len(t, kept, 2, "the confidential row is dropped, and nothing marks its absence")
	require.Equal(t, []string{all[0].ID, all[2].ID}, []string{kept[0].ID, kept[1].ID}, "order is preserved")

	own, err := NewProposalFence(f.db, "conf").Filter(f.ctx, all)
	require.NoError(t, err)
	require.Len(t, own, 3, "the project's own session still sees all three")
}

// TestFencedProjects_MarksAPreTightenProposal is the owner-side half of the
// apply-time re-check decision: the agent surface REFUSES across the fence, while
// the console (exempt by design) is handed the current isolation state so it can
// mark a proposal whose evidence predates the tighten. The gardener never decides
// for the owner; it just stops the choice from being uninformed.
func TestFencedProjects_MarksAPreTightenProposal(t *testing.T) {
	f := newScopeFixture(t)
	stale := store.Proposal{ID: mustID(t), Kind: store.ProposalReproject, Payload: map[string]any{
		"id": f.alphaMem, "name": "alpha-thing", "from": "alpha", "to": "beta",
	}}

	fenced, err := FencedProjects(f.ctx, f.db, stale)
	require.NoError(t, err)
	require.Empty(t, fenced, "nothing it touches is fenced while both projects are open")

	// The tighten lands after the proposal was raised: applying it now would carry
	// a memory out of a project that has since become confidential.
	isolate(t, f.db, "alpha", core.IsolationConfidential)

	fenced, err = FencedProjects(f.ctx, f.db, stale)
	require.NoError(t, err)
	require.Equal(t, []store.ProjectIsolation{{Slug: "alpha", State: core.IsolationConfidential}}, fenced)

	// And the agent surface's half of the same fact: no caller outside alpha may
	// be shown it any more.
	readable, err := NewProposalFence(f.db, "").Readable(f.ctx, stale)
	require.NoError(t, err)
	require.False(t, readable)
}
