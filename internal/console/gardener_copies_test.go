package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
)

// Plan fenced-sessions P7 on the review gate: the stale-copy and copy-relocate
// proposals' cards, and the warning before a stamped memory moves into a less
// fenced project.

// p7Fence registers slug at state.
func p7Fence(t *testing.T, db *sql.DB, slug string, state core.Isolation) {
	t.Helper()
	seedIsolationProject(t, db, slug)
	require.NoError(t, store.SetProjectIsolation(context.Background(), db, slug, state))
}

// p7Memory writes a memory carrying stamps.
func p7Memory(t *testing.T, mgr *files.Manager, project, name string, stamps ...core.Stamp) core.Memory {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	m, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: core.KindGotcha, Name: name, Description: name + " described", Project: project,
		Body: name + "\n", Created: now, Updated: now, ValidFrom: now, MayContain: stamps,
	})
	require.NoError(t, err)
	return m
}

// p7Stamp is a stamp naming project.
func p7Stamp(t *testing.T, project string) core.Stamp {
	t.Helper()
	ref, err := core.NewID()
	require.NoError(t, err)
	return core.Stamp{Project: project, Via: core.StampViaGrant, Ref: ref}
}

// p7Propose files a proposal.
func p7Propose(t *testing.T, db *sql.DB, kind string, payload map[string]any) store.Proposal {
	t.Helper()
	p, err := store.CreateProposal(context.Background(), db, kind, payload)
	require.NoError(t, err)
	return p
}

const p7MoveWarning = `class="gardener-fenced-note gardener-move-warning"`

// The owner is warned before moving a stamped memory into a project less
// fenced than one it may hold content from -- its own, or a stamp's -- and the
// warning names those projects and the destination. It marks and never
// blocks: Apply is still offered, with no confirm.
func TestGardenerReader_WarnsBeforeAStampedMoveIntoALessFencedProject(t *testing.T) {
	ctx, db, mgr, mux := newConsoleWithGardener(t)
	p7Fence(t, db, "example-sealed", core.IsolationSealed)
	p7Fence(t, db, "example-legal", core.IsolationConfidential)
	p7Fence(t, db, "example-finance", core.IsolationConfidential)
	p7Fence(t, db, "example-open", core.IsolationOpen)

	// A copy written in a sealed home from example-legal's material, proposed
	// back into example-legal by the copy-relocate pass.
	carried := p7Memory(t, mgr, "example-sealed", "carried-terms", p7Stamp(t, "example-legal"))
	rel := p7Propose(t, db, store.ProposalRelocate, map[string]any{
		"key": "relocate:example-legal:" + carried.ID, "id": carried.ID, "name": carried.Name, "kind": "gotcha",
		"description": carried.Description, "from": "example-sealed", "to": "example-legal", "isolation": "confidential",
		"basis": gardener.BasisCitedSources, "cited": []any{"example-legal:" + carried.ID},
		"reason": "it may contain content from example-legal and no other project",
	})
	reader := getHTMLBody(t, mux, "/console/gardener/"+rel.ID+"?reader=1")
	require.Contains(t, reader, p7MoveWarning)
	require.Contains(t, reader, `This memory may contain content from <a href="/console/projects/example-sealed" data-peek>example-sealed</a> (sealed).`)
	require.Contains(t, reader, `Applying moves it into <a href="/console/projects/example-legal" data-peek>example-legal</a>, which is confidential`)
	require.Contains(t, reader, `action="/console/gardener/`+rel.ID+`/apply"`, "Apply is still offered")
	require.NotContains(t, reader, "This cannot be undone from the console", "no confirm: it marks only")
	require.NotContains(t, reader, `class="gardener-fenced-note"`, "the stale-evidence mark is not this proposal's")
	// Its card reads as a stamped copy's relocation, not the isolation audit's.
	require.Contains(t, reader, "Move a copy to the project its sources lie in")
	require.Contains(t, reader, "every item it cites lies in example-legal")
	require.Contains(t, reader, "example-legal:"+carried.ID)
	queue := getHTMLBody(t, mux, "/console/gardener")
	require.Contains(t, queue, "Relocate stamped copy")
	require.Contains(t, queue, "Moves a stamped memory into example-legal (confidential), less fenced than example-sealed (sealed)")

	// A request's reproject of a stamped memory into an open project names
	// both projects it may hold content from.
	written := p7Memory(t, mgr, "example-legal", "legal-from-finance", p7Stamp(t, "example-finance"))
	rep := p7Propose(t, db, store.ProposalReproject, map[string]any{
		"key": "reproject:" + written.ID, "id": written.ID, "name": written.Name,
		"from": "example-legal", "to": "example-open", "rationale": "it is general",
	})
	reader = getHTMLBody(t, mux, "/console/gardener/"+rep.ID+"?reader=1")
	require.Contains(t, reader, p7MoveWarning)
	require.Contains(t, reader, `<a href="/console/projects/example-legal" data-peek>example-legal</a> (confidential), `+
		`<a href="/console/projects/example-finance" data-peek>example-finance</a> (confidential).`)
	require.Contains(t, reader, "which is open")

	// The isolation audit's relocate pulls an unstamped global memory inside a
	// fence: no warning, and its card is as it was.
	leaked := p7Memory(t, mgr, "", "leaked-fact")
	audit := p7Propose(t, db, store.ProposalRelocate, map[string]any{
		"key": "relocate:example-legal:" + leaked.ID, "id": leaked.ID, "name": leaked.Name, "kind": "gotcha",
		"description": leaked.Description, "from": "", "to": "example-legal", "isolation": "confidential",
	})
	reader = getHTMLBody(t, mux, "/console/gardener/"+audit.ID+"?reader=1")
	require.NotContains(t, reader, p7MoveWarning)
	require.Contains(t, reader, "moving it puts it behind the fence")

	// An unstamped memory's reproject out of a fenced project is not this
	// warning's: the stale-evidence mark covers fenced projects as before.
	plain := p7Memory(t, mgr, "example-legal", "plain-legal")
	unstamped := p7Propose(t, db, store.ProposalReproject, map[string]any{
		"key": "reproject:" + plain.ID, "id": plain.ID, "name": plain.Name,
		"from": "example-legal", "to": "example-open",
	})
	reader = getHTMLBody(t, mux, "/console/gardener/"+unstamped.ID+"?reader=1")
	require.NotContains(t, reader, p7MoveWarning)
	require.Contains(t, reader, `class="gardener-fenced-note"`)

	// A stamped move into a project as fenced as every one it may hold
	// content from warns of nothing.
	sideways := p7Memory(t, mgr, "example-finance", "finance-from-legal", p7Stamp(t, "example-legal"))
	same := p7Propose(t, db, store.ProposalRelocate, map[string]any{
		"key": "relocate:example-legal:" + sideways.ID, "id": sideways.ID, "name": sideways.Name,
		"from": "example-finance", "to": "example-legal", "basis": gardener.BasisCitedSources,
	})
	require.NotContains(t, getHTMLBody(t, mux, "/console/gardener/"+same.ID+"?reader=1"), p7MoveWarning)

	// Read now: once the sealed home is loosened to confidential, the first
	// move warns no more.
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-sealed", core.IsolationConfidential))
	require.NotContains(t, getHTMLBody(t, mux, "/console/gardener/"+rel.ID+"?reader=1"), p7MoveWarning)

	// The owner applies it all the same: the check is informational.
	rr := post(mux, "/console/gardener/"+rel.ID+"/apply")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	moved, ok, err := store.MemoryByID(ctx, db, carried.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "example-legal", moved.Project)
}

// A stale copy's archive reads as one: its changed sources by reference, the
// copy's own project's fence marked as any archive's would be, the sources'
// projects not.
func TestGardenerReader_StaleCopyArchive(t *testing.T) {
	_, db, mgr, mux := newConsoleWithGardener(t)
	p7Fence(t, db, "example-ops", core.IsolationConfidential)
	p7Fence(t, db, "example-legal", core.IsolationConfidential)
	cp := p7Memory(t, mgr, "example-ops", "ops-renewal-copy", p7Stamp(t, "example-legal"))
	src, err := core.NewID()
	require.NoError(t, err)
	changed := time.Now().UTC().Add(-2 * time.Hour)

	p := p7Propose(t, db, store.ProposalArchive, map[string]any{
		"key": "archive:" + cp.ID, "id": cp.ID, "name": cp.Name, "project": "example-ops", "kind": "gotcha",
		"description": cp.Description, "reason": "its source example-legal:" + src + " changed after this copy was last written",
		"basis": gardener.BasisSourceChanged, "changed_sources": []any{map[string]any{
			"project": "example-legal", "id": src, "kind": "memory", "changed": core.FormatTime(changed),
		}},
	})
	reader := getHTMLBody(t, mux, "/console/gardener/"+p.ID+"?reader=1")
	require.Contains(t, reader, "Retire a stale copy")
	require.Contains(t, reader, "Sources that changed")
	require.Contains(t, reader, `<a href="/console/projects/example-legal" data-peek>example-legal:`+src+`</a> &middot; memory &middot; changed`)
	require.Contains(t, reader, "stop surfacing the stale copy")
	require.Contains(t, reader, `class="gardener-fenced-note"`, "the copy's own confidential project is marked")
	note := reader[strings.Index(reader, `class="gardener-fenced-note"`):]
	note = note[:strings.Index(note, "</div>")]
	require.Contains(t, note, "example-ops")
	require.NotContains(t, note, "example-legal", "the sources' projects are the premise, not news")

	queue := getHTMLBody(t, mux, "/console/gardener")
	require.Contains(t, queue, "Archive stale copy")

	// The CLI's JSON shape is the archive card's, unchanged.
	rr := getPeek(t, mux, "/console/gardener/"+p.ID+"?format=json")
	require.Equal(t, http.StatusOK, rr.Code)
	var card map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &card))
	require.Equal(t, cp.ID, card["archive"].(map[string]any)["id"])
	require.ElementsMatch(t, []string{"id", "kind", "label", "eyebrow", "icon", "tone", "createdAt", "archive", "reason"},
		slices.Collect(maps.Keys(card)))
}

// A digest folding stamped findings says what its note will record.
func TestGardenerReader_DigestNamesItsStamps(t *testing.T) {
	_, db, _, mux := newConsoleWithGardener(t)
	ref, err := core.NewID()
	require.NoError(t, err)
	stamped := p7Propose(t, db, store.ProposalDigest, map[string]any{
		"key": "digest:example-ops:2026-11", "project": "example-ops", "month": "2026-11", "session_count": 2.0,
		"title": "Session digest: example-ops -- 2026-11", "body": "## November\n- things",
		"may_contain": []any{map[string]any{"project": "example-legal", "grant": ref}},
	})
	plain := p7Propose(t, db, store.ProposalDigest, map[string]any{
		"key": "digest:example-hr:2026-11", "project": "example-hr", "month": "2026-11", "session_count": 2.0,
		"title": "Session digest: example-hr -- 2026-11", "body": "## November\n- other things",
	})
	reader := getHTMLBody(t, mux, "/console/gardener/"+stamped.ID+"?reader=1")
	require.Contains(t, reader, "The note records that it may contain content from example-legal (grant R-"+ref[len(ref)-4:]+")")
	require.NotContains(t, getHTMLBody(t, mux, "/console/gardener/"+plain.ID+"?reader=1"), "The note records that it")
}
