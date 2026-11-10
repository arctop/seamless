package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Every proposal kind must land in a taxonomy section. A kind that is not
// mapped would be projected, counted, and then silently dropped out of the rail
// -- a proposal nobody can see is worse than one nobody wrote.
func TestQueueTaxonomy_CoversEveryProposalKind(t *testing.T) {
	labels := map[string]bool{}
	for _, sec := range queueTaxonomy {
		labels[sec.Key] = true
	}
	for _, kind := range store.ProposalKinds {
		if kind == store.ProposalSplit {
			continue // split setups lead their own plan batch, not a section
		}
		key := sectionOfKind[kind]
		require.NotEmpty(t, key, "proposal kind %q has no rail section", kind)
		require.True(t, labels[key], "kind %q maps to unknown section %q", kind, key)
	}
}

// seedQueue fills the store with one proposal per taxonomy section plus an
// owner-requested one, so the filters and grouping have something to sort.
func seedQueue(t *testing.T, ctx context.Context, db *sql.DB) map[string]store.Proposal {
	t.Helper()
	out := map[string]store.Proposal{}
	mk := func(kind string, payload map[string]any) {
		t.Helper()
		p, err := store.CreateProposal(ctx, db, kind, payload)
		require.NoError(t, err)
		out[kind] = p
	}
	mk(store.ProposalToolError, map[string]any{
		"key": "tool_error:seamless:abcd", "project": "seamless", "surface": "tool",
		"name": "tasks_add", "signature": "unknown parameter <v>",
		"examples":    []string{`tasks_add: unknown parameter "titel"`},
		"error_count": 3.0, "session_count": 2.0, "suggested_title": "tasks_add: unknown parameter <v>",
	})
	mk(store.ProposalArchive, map[string]any{
		"key": "archive:MISSING", "id": "MISSINGMEM", "name": "old-note",
		"project": "seamless", "kind": "reference", "description": "went stale",
	})
	mk(store.ProposalDigest, map[string]any{
		"key": "digest:seamless:2026-07", "project": "seamless", "month": "2026-07",
		"session_count": 4.0, "title": "seamless digest 2026-07", "body": "## Findings\n- did the thing",
		// The owner asked for this one, so the source filter has both cases.
		"source": "request", "request_text": "roll up last month",
	})
	mk(store.ProposalAbandonPlan, map[string]any{
		"key": "abandon_plan:P1", "id": "P1", "slug": "old-plan",
		"title": "Old Plan", "project": "seamless", "plan_status": "presented",
	})
	return out
}

// The queue screen wears the shared explorer chrome and files every pending
// proposal into a labeled rail section, with the propose-only contract and the
// undo affordance both visible.
func TestGardenerScreen_QueueChrome(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)

	// One decision, so "Recently decided" has something to offer an undo on.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+seeded[store.ProposalArchive].ID+"/dismiss").Code)

	body := getPeek(t, mux, "/console/gardener").Body.String()
	require.Contains(t, body, `class="lib-page gardener-page" data-library="gardener"`)
	require.Contains(t, body, `class="mv2-bar"`)
	require.Contains(t, body, `class="lib-layout"`)
	require.Contains(t, body, `data-base="/console/gardener"`)
	require.Contains(t, body, "Propose-only by design.")

	// Sections render by taxonomy key, labeled, with a bulk dismiss each.
	require.Contains(t, body, `id="rg-errors"`)
	require.Contains(t, body, `id="rg-digests"`)
	require.Contains(t, body, `id="rg-plans"`)
	require.NotContains(t, body, `id="rg-cleanup"`, "the only cleanup proposal was dismissed")
	require.Contains(t, body, "Errors &amp; gaps")
	require.Contains(t, body, "Plan settlements")
	require.Contains(t, body, `class="gardener-dismiss-all"`)

	// The one-line row summaries carry the concrete objects.
	require.Contains(t, body, "Fix recurring error")
	require.Contains(t, body, "tasks_add · 3×")
	require.Contains(t, body, "Retire stale plan")

	// Recently decided offers the undo.
	require.Contains(t, body, "Recently decided")
	require.Contains(t, body, "/console/gardener/"+seeded[store.ProposalArchive].ID+"/undo")
}

// The type and source filters narrow the rail, and an unknown value is a 400
// naming the valid ones rather than a silent default.
func TestGardenerQueue_Filters(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seedQueue(t, ctx, db)

	var all gardenerData
	getJSON(t, mux, "/console/gardener?format=json", &all)
	require.Equal(t, 4, all.PendingCount)

	var errorsOnly gardenerData
	getJSON(t, mux, "/console/gardener?group=errors&format=json", &errorsOnly)
	require.Len(t, errorsOnly.Cards, 1)
	require.Equal(t, store.ProposalToolError, errorsOnly.Cards[0].Kind)

	var asked gardenerData
	getJSON(t, mux, "/console/gardener?src=you&format=json", &asked)
	require.Len(t, asked.Cards, 1)
	require.Equal(t, store.ProposalDigest, asked.Cards[0].Kind)

	var background gardenerData
	getJSON(t, mux, "/console/gardener?src=auto&format=json", &background)
	require.Len(t, background.Cards, 3)

	// Combined filters intersect rather than override each other.
	var none gardenerData
	getJSON(t, mux, "/console/gardener?group=errors&src=you&format=json", &none)
	require.Empty(t, none.Cards)

	for _, bad := range []string{"/console/gardener?group=bogus", "/console/gardener?src=bogus"} {
		rr := getPeek(t, mux, bad)
		require.Equal(t, http.StatusBadRequest, rr.Code, bad)
		require.Contains(t, rr.Body.String(), "valid values are")
	}

	// A filter that matches nothing says so and offers a way out, rather than
	// looking like an empty garden.
	body := getPeek(t, mux, "/console/gardener?group=errors&src=you").Body.String()
	require.Contains(t, body, "No proposal matches this filter")
	require.Contains(t, body, `href="/console/gardener"`)
}

// The list URL auto-selects the top of the queue; /console/gardener/{id} opens
// a specific proposal; ?reader=1 returns just the review sheet; and a resolved
// id bounces back to the queue instead of 404ing.
func TestGardenerDetail_ReaderAndSelection(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)
	digest := seeded[store.ProposalDigest]

	// Auto-selection pins the chosen proposal into the URL for the client.
	list := getPeek(t, mux, "/console/gardener").Body.String()
	require.Contains(t, list, `data-auto-url="/console/gardener/`)

	full := getPeek(t, mux, "/console/gardener/"+digest.ID)
	require.Equal(t, http.StatusOK, full.Code)
	fullBody := full.Body.String()
	require.Contains(t, fullBody, `class="rail-item gardener-row tone-ok selected"`)
	require.Contains(t, fullBody, `aria-current="page"`)
	require.Contains(t, fullBody, "seamless digest 2026-07")

	frag := getPeek(t, mux, "/console/gardener/"+digest.ID+"?reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	fragBody := frag.Body.String()
	require.NotContains(t, fragBody, "<!doctype html>", "the reader fragment carries no page chrome")
	require.Contains(t, fragBody, `class="reader-sheet gardener-sheet`)
	require.Contains(t, fragBody, "/console/gardener/"+digest.ID+"/apply")
	require.Contains(t, fragBody, "/console/gardener/"+digest.ID+"/dismiss")
	require.Contains(t, fragBody, "Asked by you")

	// A stray reader=1 on the bare list URL is still the full page, not a 500.
	strayReader := getPeek(t, mux, "/console/gardener?reader=1")
	require.Equal(t, http.StatusOK, strayReader.Code)
	require.Contains(t, strayReader.Body.String(), "<!doctype html>")

	// A proposal someone already decided sends the owner back with a reason.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+digest.ID+"/dismiss").Code)
	stale := getPeek(t, mux, "/console/gardener/"+digest.ID)
	require.Equal(t, http.StatusSeeOther, stale.Code)
	require.Contains(t, stale.Header().Get("Location"), "/console/gardener?error=")
	require.Contains(t, stale.Header().Get("Location"), "already+resolved")
}

// Deciding a proposal advances the reader to the next one in the queue, so
// triage keeps moving instead of dropping into a blank pane.
func TestGardenerApply_RedirectsToNextSelection(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seedQueue(t, ctx, db)

	var data gardenerData
	getJSON(t, mux, "/console/gardener?format=json", &data)
	require.Len(t, data.Cards, 4)
	first, second := data.Cards[0], data.Cards[1]

	// The rail order the page renders is what the form posts back as `next`.
	page := getPeek(t, mux, "/console/gardener").Body.String()
	require.Contains(t, page, `<input type="hidden" name="next" value="`+second.ID+`">`)

	rr := postForm(mux, "/console/gardener/"+first.ID+"/dismiss", "next="+second.ID)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener/"+second.ID+"?notice="),
		"a decision advances to the next proposal, got %q", rr.Header().Get("Location"))

	// A `next` that another tab already decided degrades to the bare list,
	// which auto-selects -- never a redirect that bounces straight back out.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+second.ID+"/dismiss").Code)
	third := data.Cards[2]
	rr = postForm(mux, "/console/gardener/"+third.ID+"/dismiss", "next="+second.ID)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener?notice="),
		"a stale next falls back to the list, got %q", rr.Header().Get("Location"))
}

// Group dismiss closes one rail section at a time -- by taxonomy key or by plan
// slug -- and leaves every other section alone.
func TestGardenerGroupDismiss(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)

	rr := postForm(mux, "/console/gardener/group/dismiss", "group=errors")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc := rr.Header().Get("Location")
	require.Contains(t, loc, "/console/gardener?notice=")
	require.Contains(t, loc, "1+proposal")
	require.Contains(t, loc, "Errors")

	got, _, err := store.ProposalByID(ctx, db, seeded[store.ProposalToolError].ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalDismissed, got.Status)
	for _, kind := range []string{store.ProposalArchive, store.ProposalDigest, store.ProposalAbandonPlan} {
		got, _, err = store.ProposalByID(ctx, db, seeded[kind].ID)
		require.NoError(t, err)
		require.Equal(t, store.ProposalPending, got.Status, "%s was not in the dismissed section", kind)
	}

	// Re-dismissing an empty section is a flash, not a silent success.
	rr = postForm(mux, "/console/gardener/group/dismiss", "group=errors")
	require.Contains(t, rr.Header().Get("Location"), "error=")

	// Exactly one of group/plan, and only a known section.
	require.Equal(t, http.StatusBadRequest, postForm(mux, "/console/gardener/group/dismiss", "").Code)
	require.Equal(t, http.StatusBadRequest,
		postForm(mux, "/console/gardener/group/dismiss", "group=cleanup&plan=split-x").Code)
	require.Equal(t, http.StatusBadRequest,
		postForm(mux, "/console/gardener/group/dismiss", "group=bogus").Code)
}

// A plan batch dismisses as a unit, and every step lands in Recently decided as
// its own undoable row.
func TestGardenerGroupDismiss_PlanBatch(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/split", "source=example-app").Code)

	rr := postForm(mux, "/console/gardener/group/dismiss", "plan=split-example-app")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "3+proposals")

	pending, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, pending)

	recent, err := store.RecentResolvedProposals(ctx, db, 10)
	require.NoError(t, err)
	require.Len(t, recent, 3, "each step is separately undoable")
}

// Undoing a dismissal returns the proposal to the queue and selects it, so the
// owner lands on exactly what they took back.
func TestGardenerUndo_RestoresAndSelects(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)
	toolErr := seeded[store.ProposalToolError]

	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+toolErr.ID+"/dismiss").Code)
	rr := post(mux, "/console/gardener/"+toolErr.ID+"/undo")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/gardener/"+toolErr.ID+"?notice="),
		"undo selects the restored proposal, got %q", rr.Header().Get("Location"))

	got, _, err := store.ProposalByID(ctx, db, toolErr.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)
}

// Undoing an apply reverses its effect: the task a tool_error opened is dropped
// and the proposal is pending again.
func TestGardenerUndo_ApplyDropsTheOpenedTask(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)
	toolErr := seeded[store.ProposalToolError]

	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+toolErr.ID+"/apply").Code)
	open, err := store.ListTasks(ctx, db, "seamless", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, open, 1)

	rr := post(mux, "/console/gardener/"+toolErr.ID+"/undo")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")

	open, err = store.ListTasks(ctx, db, "seamless", core.TaskOpen)
	require.NoError(t, err)
	require.Empty(t, open, "the task the apply opened is dropped again")
	got, _, err := store.ProposalByID(ctx, db, toolErr.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)
}

// An applied split cannot be inverted, so its Recently-decided row offers no
// undo button and the endpoint refuses even if called directly.
func TestGardenerUndo_RefusedForAppliedSplit(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/split", "source=example-app").Code)
	splits, err := store.PendingProposals(ctx, db, store.ProposalSplit)
	require.NoError(t, err)
	require.Len(t, splits, 1)
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+splits[0].ID+"/apply").Code)

	rr := post(mux, "/console/gardener/"+splits[0].ID+"/undo")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")

	got, _, err := store.ProposalByID(ctx, db, splits[0].ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, got.Status)

	body := getPeek(t, mux, "/console/gardener").Body.String()
	require.NotContains(t, body, "/console/gardener/"+splits[0].ID+"/undo",
		"an irreversible apply offers no undo button")
	require.Contains(t, body, "final")
}

// Hiding is the second rejection tier. It resolves the proposal like a
// dismissal, keeps it undoable from Recently decided, and additionally lists it
// under Hidden forever -- the only durable surface naming a standing block --
// where unhiding lifts it without returning the proposal to the queue.
func TestGardenerHide_ListsAndUnhides(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seeded := seedQueue(t, ctx, db)
	toolErr := seeded[store.ProposalToolError]

	// The reader offers both strengths, neither behind a confirm.
	reader := getPeek(t, mux, "/console/gardener/"+toolErr.ID+"?reader=1").Body.String()
	require.Contains(t, reader, "/console/gardener/"+toolErr.ID+"/dismiss")
	require.Contains(t, reader, "/console/gardener/"+toolErr.ID+"/hide")
	require.Contains(t, reader, "Hide forever")
	require.NotContains(t, reader, "confirm(", "both rejections are reversible, so neither interstitial")

	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+toolErr.ID+"/hide").Code)
	got, _, err := store.ProposalByID(ctx, db, toolErr.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalHidden, got.Status)

	body := getPeek(t, mux, "/console/gardener").Body.String()
	require.Contains(t, body, `id="rg-hidden"`)
	require.Contains(t, body, "Hidden forever")
	require.Contains(t, body, "/console/gardener/"+toolErr.ID+"/unhide")
	require.Contains(t, body, "/console/gardener/"+toolErr.ID+"/undo",
		"a hide stays undoable from Recently decided too")

	// Unhide lifts the block without reopening the proposal.
	rr := post(mux, "/console/gardener/"+toolErr.ID+"/unhide")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")

	got, _, err = store.ProposalByID(ctx, db, toolErr.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalDismissed, got.Status, "unhide demotes, it does not reopen")

	body = getPeek(t, mux, "/console/gardener").Body.String()
	require.NotContains(t, body, `id="rg-hidden"`, "nothing is blocked any more")
}

// The hidden list has to survive an empty queue: it is the only place a
// standing block can be found, and a tidy garden is exactly when the owner
// wonders why a proposal stopped coming back.
func TestGardenerHidden_RendersWithAnEmptyQueue(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	p, err := store.CreateProposal(ctx, db, store.ProposalToolError, map[string]any{
		"key": "tool_error:seamless:abcd", "project": "seamless", "surface": "tool",
		"name": "tasks_add", "error_count": 3.0, "suggested_title": "tasks_add: unknown parameter <v>",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+p.ID+"/hide").Code)

	body := getPeek(t, mux, "/console/gardener").Body.String()
	require.Contains(t, body, `id="rg-hidden"`)
	require.Contains(t, body, "/console/gardener/"+p.ID+"/unhide")
	require.Contains(t, body, "The garden is tidy.", "the queue is still honestly empty")
}

// The confirm policy is a projection, not a per-template judgement call: only
// the two kinds that cannot be undone gate Apply behind a confirm.
func TestGardenerReader_ConfirmsOnlyIrreversibleApplies(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)

	undoable, err := store.CreateProposal(ctx, db, store.ProposalDigest, map[string]any{
		"key": "digest:seamless:2026-07", "project": "seamless", "month": "2026-07",
		"title": "seamless digest 2026-07", "body": "## Findings",
	})
	require.NoError(t, err)
	irreversible, err := store.CreateProposal(ctx, db, store.ProposalConsolidate, map[string]any{
		"key": "consolidate:a|b", "name": "unified", "kind": "runbook", "project": "seamless",
		"description": "unified", "body": "# Unified",
		"sources": []any{map[string]any{"id": "A", "name": "a"}},
	})
	require.NoError(t, err)

	ok := getPeek(t, mux, "/console/gardener/"+undoable.ID+"?reader=1").Body.String()
	require.NotContains(t, ok, "confirm(", "an undoable apply needs no interstitial")
	require.Contains(t, ok, "undoable from Recently decided")

	gated := getPeek(t, mux, "/console/gardener/"+irreversible.ID+"?reader=1").Body.String()
	require.Contains(t, gated, "This cannot be undone from the console.")
	require.NotContains(t, gated, "undoable from Recently decided")
}

// The ?format=json shape the seam CLI reads is fixed: the inbox projections are
// all json:"-", so a card carries exactly the fields it always did.
func TestGardenerJSON_ShapeIsUnchanged(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seedQueue(t, ctx, db)

	req := httptest.NewRequest(http.MethodGet, "/console/gardener?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	raw := do(mux, req).Body.String()

	for _, leaked := range []string{"rowTitle", "RowTitle", "rowDetail", "groupKey", "nextID", "NextID", "canUndoApply", "sections", "Sections", "recent", "Recent", "hidden", "Hidden"} {
		require.NotContains(t, raw, leaked, "%s must not reach the CLI payload", leaked)
	}
	require.Contains(t, raw, `"pendingCount"`)
	require.Contains(t, raw, `"cards"`)
}
