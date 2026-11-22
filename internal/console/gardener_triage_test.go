package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// gtFixture is a console over a real gardener and files, with the handles
// the triage tests reach for. locked, when set, is a project the console's
// read handle hides (a peer_auth view), writes going to the main handle.
type gtFixture struct {
	ctx    context.Context
	db     *sql.DB
	mgr    *files.Manager
	garden *gardener.Service
	svc    *Service
	mux    *http.ServeMux
}

func gtConsole(t *testing.T, locked ...string) gtFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.FromConfig(config.Gardener{}), slog.Default())
	cfg := Config{DB: db, Files: mgr, Gardener: garden, Events: rec, DataDir: dataDir, APIKey: testKey, Level: "advanced"}
	if len(locked) > 0 {
		view, err := store.OpenView(path, locked)
		require.NoError(t, err)
		t.Cleanup(func() { _ = view.Close() })
		cfg.DB, cfg.WriteDB, cfg.Events = view, db, rec.ReadingFrom(view)
	}
	svc, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return gtFixture{ctx: context.Background(), db: db, mgr: mgr, garden: garden, svc: svc, mux: mux}
}

// gtMemory writes an active memory with a body, written by session, and
// returns it.
func gtMemory(t *testing.T, f gtFixture, project, name, body, session string, updated time.Time) core.Memory {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	m, err := f.mgr.WriteMemory(f.ctx, core.Memory{
		ID: id, Kind: core.KindGotcha, Name: name, Description: name + " in one line",
		Project: project, Body: body, Created: updated, Updated: updated, ValidFrom: updated,
		SourceSession: session, Model: "example-model",
	})
	require.NoError(t, err)
	return m
}

// gtDeliver records n briefing deliveries of id, the last at last, and
// rebuilds the retrieval stats the evidence reads.
func gtDeliver(t *testing.T, f gtFixture, id string, n int, last time.Time) {
	t.Helper()
	rec := events.NewRecorder(f.db)
	for i := range n {
		_, err := rec.Record(f.ctx, core.Event{
			Kind: core.EventInjected, TS: last.Add(-time.Duration(i) * time.Hour),
			Payload: map[string]any{"item_ids": []string{id}, "hook": "session-start"},
		})
		require.NoError(t, err)
	}
	require.NoError(t, store.RebuildRetrievalStats(f.ctx, f.db))
}

// gtMerge files a background merge proposal keeping keep and retiring drop.
func gtMerge(t *testing.T, f gtFixture, keep, drop core.Memory) store.Proposal {
	t.Helper()
	brief := func(m core.Memory) map[string]any {
		return map[string]any{"id": m.ID, "name": m.Name, "project": m.Project, "kind": string(m.Kind), "description": m.Description}
	}
	p, err := store.CreateProposal(f.ctx, f.db, store.ProposalMerge, map[string]any{
		"key": "merge:" + keep.ID + "|" + drop.ID, "score": 0.93, "keep": brief(keep), "drop": brief(drop),
	})
	require.NoError(t, err)
	return p
}

// gtEvent is the payload of the newest gardener.action event about id whose
// action is action, nil when there is none.
func gtEvent(t *testing.T, f gtFixture, id, action string) map[string]any {
	t.Helper()
	var raw string
	err := f.db.QueryRowContext(f.ctx, `SELECT payload FROM events WHERE kind = ? AND item_id = ?
		AND json_extract(payload, '$.action') = ? ORDER BY ts DESC, id DESC LIMIT 1`,
		string(core.EventGardenerAction), id, action).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

// gtLocation is where a redirect goes and its flash.
func gtLocation(t *testing.T, rr interface{ Header() http.Header }) (path, param, msg string) {
	t.Helper()
	u, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	if m := u.Query().Get("error"); m != "" {
		return u.Path, "error", m
	}
	return u.Path, "notice", u.Query().Get("notice")
}

// A merge card shows both memories as they are now -- each body, when it
// last changed, when it was last delivered and how often, and who wrote it --
// and Swap changes which one survives: the payload is rewritten, the swap is
// recorded as the owner's, and applying then retires the memory the merge
// first kept.
func TestGtMerge_BothBodiesAndSwap(t *testing.T) {
	f := gtConsole(t)
	now := time.Now().UTC()
	sessID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(f.ctx, f.db, core.Session{ID: sessID, Name: "cc/gt-writer", ProjectSlug: "example-ops",
		Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now}))
	keep := gtMemory(t, f, "example-ops", "deploy-window", "Deploys go out on **Tuesdays** only.", "cc/gt-writer", now.Add(-2*time.Hour))
	drop := gtMemory(t, f, "example-ops", "deploy-days", "Ship on Tuesday and *never* on Friday.", "", now.Add(-72*time.Hour))
	gtDeliver(t, f, drop.ID, 3, now.Add(-5*time.Hour))
	p := gtMerge(t, f, keep, drop)

	reader := getPeek(t, f.mux, "/console/gardener/"+p.ID+"?reader=1").Body.String()
	require.Contains(t, reader, `class="gardener-ev-pair"`)
	require.Contains(t, reader, "<strong>Tuesdays</strong>", "the kept memory's body, rendered")
	require.Contains(t, reader, "<em>never</em>", "the retired memory's body, rendered")
	require.Contains(t, reader, "Stays active")
	require.Contains(t, reader, "Retired, pointing at the kept one")
	require.Contains(t, reader, "Last delivered")
	require.Contains(t, reader, "<dd>3 times</dd>", "how often agents were given the retired one")
	require.Contains(t, reader, "<dd>never</dd>", "the kept one was never delivered")
	require.Contains(t, reader, `href="/console/sessions/`+sessID+`"`, "who wrote it, linked")
	require.Contains(t, reader, "example-model")
	require.Contains(t, reader, "A background pass found these two memories 93% alike")
	require.Contains(t, reader, `action="/console/gardener/`+p.ID+`/swap"`)
	require.Contains(t, reader, `name="keep" value="`+drop.ID+`"`, "Swap names the memory to keep")

	// Swap: the payload now keeps the other one, and the reader says so.
	rr := postForm(f.mux, "/console/gardener/"+p.ID+"/swap", "keep="+drop.ID)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	path, param, msg := gtLocation(t, rr)
	require.Equal(t, "/console/gardener/"+p.ID, path, "the swapped proposal stays selected")
	require.Equal(t, "notice", param)
	require.Equal(t, "Swapped. Applying now keeps deploy-days and retires deploy-window.", msg)
	got, _, err := store.ProposalByID(f.ctx, f.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status, "a swap decides nothing")
	require.Equal(t, drop.ID, payloadStr(payloadMap(got.Payload, "keep"), "id"))
	require.Equal(t, keep.ID, payloadStr(payloadMap(got.Payload, "drop"), "id"))
	require.Equal(t, p.Payload["key"], got.Payload["key"], "the pair's key is unchanged")
	ev := gtEvent(t, f, p.ID, "swap")
	require.NotNil(t, ev, "a swap records its gardener.action event")
	require.Equal(t, map[string]any{"action": "swap", "kind": "merge", "by": "console", "keep": drop.ID, "drop": keep.ID}, ev)

	// The same post again lands on the same choice, not back where it was.
	_, param, msg = gtLocation(t, postForm(f.mux, "/console/gardener/"+p.ID+"/swap", "keep="+drop.ID))
	require.Equal(t, "notice", param)
	require.Equal(t, "This merge already keeps deploy-days.", msg)
	got, _, err = store.ProposalByID(f.ctx, f.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, drop.ID, payloadStr(payloadMap(got.Payload, "keep"), "id"))

	// Refusals: no keep, a memory the merge does not name, not a merge.
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/"+p.ID+"/swap", "").Code)
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/"+p.ID+"/swap", "keep=SOMEOTHER").Code)
	wanted, err := store.CreateProposal(f.ctx, f.db, store.ProposalMemoryWanted, map[string]any{
		"key": "memory_wanted:example-ops:x", "project": "example-ops", "suggested_title": "x", "miss_count": 2.0,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/"+wanted.ID+"/swap", "keep="+drop.ID).Code)

	// Apply now supersedes the memory the merge first kept.
	require.Equal(t, http.StatusSeeOther, post(f.mux, "/console/gardener/"+p.ID+"/apply").Code)
	first, _, err := store.MemoryByID(f.ctx, f.db, keep.ID)
	require.NoError(t, err)
	require.Equal(t, drop.ID, first.SupersededBy, "the swap decided which survives")
	second, _, err := store.MemoryByID(f.ctx, f.db, drop.ID)
	require.NoError(t, err)
	require.True(t, second.Active())

	// A resolved merge cannot be swapped.
	_, param, _ = gtLocation(t, postForm(f.mux, "/console/gardener/"+p.ID+"/swap", "keep="+keep.ID))
	require.Equal(t, "error", param)
}

// An archive card shows the memory's body, who wrote it, when it last
// changed and when agents were last given it, and says why it was proposed
// in a plain sentence.
func TestGtArchive_EvidenceAndAPlainReason(t *testing.T) {
	f := gtConsole(t)
	now := time.Now().UTC()
	m := gtMemory(t, f, "example-ops", "old-runbook", "Restart the **queue** by hand.", "cc/gt-gone", now.Add(-100*24*time.Hour))
	gtDeliver(t, f, m.ID, 1, now.Add(-40*24*time.Hour))
	p, err := store.CreateProposal(f.ctx, f.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + m.ID, "id": m.ID, "name": m.Name, "project": m.Project, "kind": string(m.Kind),
		"description": m.Description, "reason": "no activity in 90d", "last_activity": core.FormatTime(m.Updated),
	})
	require.NoError(t, err)

	reader := getPeek(t, f.mux, "/console/gardener/"+p.ID+"?reader=1").Body.String()
	require.Contains(t, reader, "<strong>queue</strong>", "the body, rendered")
	require.Contains(t, reader, "cc/gt-gone", "who wrote it, by the stamp when the session is gone")
	require.Contains(t, reader, "<dd>once</dd>")
	require.Contains(t, reader, "Last updated")
	require.Contains(t, reader, "Why it was proposed")
	require.Contains(t, reader, "Nothing has updated, delivered or read it in 90 days, and no other memory links to it.")
	require.NotContains(t, reader, "no activity in 90d", "the pass's shorthand stays in the JSON")

	// The page's own selection (the bare list) carries the evidence too.
	page := getPeek(t, f.mux, "/console/gardener").Body.String()
	require.Contains(t, page, "<strong>queue</strong>")

	// The JSON keeps the pass's reason and gains no evidence.
	var card map[string]any
	require.NoError(t, json.Unmarshal(getPeek(t, f.mux, "/console/gardener/"+p.ID+"?format=json").Body.Bytes(), &card))
	require.Equal(t, "no activity in 90d", card["reason"])
	for _, k := range []string{"Why", "why", "ArchiveEv", "archiveEv", "BulkBlock"} {
		require.NotContains(t, card, k)
	}
}

// A body from a project the request may not read is not shown: the evidence
// reads through the request's handle, on which a locked project's memories
// do not exist. Without the lock the same reader shows it.
func TestGtEvidence_LockedProjectBodyIsAbsent(t *testing.T) {
	const locked = "example-finance"
	f := gtConsole(t, locked)
	now := time.Now().UTC()
	hidden := gtMemory(t, f, locked, "ledger-close", "Close the ledger on the **fifth**.", "", now)
	shown := gtMemory(t, f, "example-ops", "ops-close", "Close tickets on *Fridays*.", "", now)
	p, err := store.CreateProposal(f.ctx, f.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + hidden.ID, "id": hidden.ID, "name": hidden.Name, "project": locked, "kind": "gotcha",
	})
	require.NoError(t, err)
	q, err := store.CreateProposal(f.ctx, f.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + shown.ID, "id": shown.ID, "name": shown.Name, "project": "example-ops", "kind": "gotcha",
	})
	require.NoError(t, err)

	require.Nil(t, f.svc.memoryEvidence(f.ctx, hidden.ID), "the locked memory does not exist on the request's handle")
	require.NotNil(t, f.svc.memoryEvidence(f.ctx, shown.ID))
	for _, path := range []string{"/console/gardener", "/console/gardener/" + q.ID + "?reader=1", "/console/gardener/" + p.ID + "?reader=1"} {
		body := getPeek(t, f.mux, path).Body.String()
		for _, gone := range []string{"fifth", "ledger-close", hidden.ID} {
			require.NotContains(t, body, gone, "%s shows the locked project's %s", path, gone)
		}
	}
	require.Contains(t, getPeek(t, f.mux, "/console/gardener/"+q.ID+"?reader=1").Body.String(), "<em>Fridays</em>")

	open := gtConsole(t)
	m := gtMemory(t, open, locked, "ledger-close", "Close the ledger on the **fifth**.", "", now)
	r, err := store.CreateProposal(open.ctx, open.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + m.ID, "id": m.ID, "name": m.Name, "project": locked, "kind": "gotcha",
	})
	require.NoError(t, err)
	require.Contains(t, getPeek(t, open.mux, "/console/gardener/"+r.ID+"?reader=1").Body.String(), "<strong>fifth</strong>",
		"without the lock the reader shows it: the absence is the view's")
}

// gtQueue files an Errors & gaps section of two undoable proposals and a
// Memory cleanup section holding a consolidation beside an archive.
func gtQueue(t *testing.T, f gtFixture) (toolErr, wanted, consolidate, archive store.Proposal) {
	t.Helper()
	mk := func(kind string, payload map[string]any) store.Proposal {
		p, err := store.CreateProposal(f.ctx, f.db, kind, payload)
		require.NoError(t, err)
		return p
	}
	toolErr = mk(store.ProposalToolError, map[string]any{
		"key": "tool_error:example-ops:1", "project": "example-ops", "surface": "tool", "name": "tasks_add",
		"error_count": 3.0, "session_count": 2.0, "suggested_title": "tasks_add: unknown parameter <v>",
	})
	wanted = mk(store.ProposalMemoryWanted, map[string]any{
		"key": "memory_wanted:example-ops:boot", "project": "example-ops", "signature": "boot",
		"queries": []string{"boot race"}, "miss_count": 3.0, "session_count": 2.0, "suggested_title": "boot race",
	})
	consolidate = mk(store.ProposalConsolidate, map[string]any{
		"key": "consolidate:a|b", "name": "unified", "kind": "runbook", "project": "example-ops",
		"description": "unified", "body": "# Unified", "sources": []any{map[string]any{"id": "A", "name": "a"}},
	})
	archive = mk(store.ProposalArchive, map[string]any{
		"key": "archive:MISSING", "id": "MISSING", "name": "old-note", "project": "example-ops", "kind": "reference",
	})
	return toolErr, wanted, consolidate, archive
}

// Apply all appears only on a section whose every proposal can be undone,
// applies exactly the proposals the section showed, one at a time, and each
// lands in the decision history with its own Undo. A section holding a
// consolidation has none, and the route refuses one whole, before anything
// is applied.
func TestGtApplyAll_UndoableSectionsOnly(t *testing.T) {
	f := gtConsole(t)
	toolErr, wanted, consolidate, archive := gtQueue(t, f)

	page := getPeek(t, f.mux, "/console/gardener").Body.String()
	errSec := gtSection(t, page, "errors")
	require.Contains(t, errSec, `action="/console/gardener/group/apply"`)
	require.Contains(t, errSec, "Apply all 2")
	require.Contains(t, errSec, `name="id" value="`+toolErr.ID+`"`)
	require.Contains(t, errSec, `name="id" value="`+wanted.ID+`"`)
	require.Contains(t, errSec, `data-confirm="Applies all 2 proposals in Errors &amp; gaps; each can be undone from the decision history."`,
		"Apply all asks in the page")
	cleanup := gtSection(t, page, "cleanup")
	require.NotContains(t, cleanup, "/console/gardener/group/apply", "a section with a consolidation offers no Apply all")
	require.Contains(t, cleanup, "Dismiss all 2", "it can still be dismissed")

	// A pass raises another error after the page rendered: it is not applied.
	late, err := store.CreateProposal(f.ctx, f.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:example-ops:2", "project": "example-ops", "surface": "tool", "name": "notes_read",
		"error_count": 3.0, "session_count": 2.0, "suggested_title": "notes_read: no such note",
	})
	require.NoError(t, err)

	rr := postForm(f.mux, "/console/gardener/group/apply", "group=errors&id="+toolErr.ID+"&id="+wanted.ID)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	_, param, msg := gtLocation(t, rr)
	require.Equal(t, "notice", param, msg)
	require.Equal(t, "Applied 2 proposals from Errors & gaps. Undo any of them from the decision history.", msg)
	for _, p := range []store.Proposal{toolErr, wanted} {
		got, _, err := store.ProposalByID(f.ctx, f.db, p.ID)
		require.NoError(t, err)
		require.Equal(t, store.ProposalApplied, got.Status, p.Kind)
		require.Equal(t, gardener.ByOwner, got.ResolvedBy, "the owner applied it")
		require.NotNil(t, gtEvent(t, f, p.ID, "apply"), "each apply keeps its own event")
	}
	stillPending, _, err := store.ProposalByID(f.ctx, f.db, late.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, stillPending.Status, "a proposal the owner did not see is never applied")
	tasks, err := store.ListTasks(f.ctx, f.db, "example-ops", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, tasks, 2, "each apply opened its task")

	// Each lands in the history with its own Undo, and Undo takes it back.
	history := getPeek(t, f.mux, "/console/gardener/decisions").Body.String()
	for _, p := range []store.Proposal{toolErr, wanted} {
		require.Contains(t, history, `action="/console/gardener/`+p.ID+`/undo"`)
	}
	require.Equal(t, http.StatusSeeOther, post(f.mux, "/console/gardener/"+toolErr.ID+"/undo").Code)
	got, _, err := store.ProposalByID(f.ctx, f.db, toolErr.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)
	tasks, err = store.ListTasks(f.ctx, f.db, "example-ops", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, tasks, 1, "the undo dropped the task its apply opened")

	// A batch holding a consolidation is refused whole: nothing is applied.
	rr = postForm(f.mux, "/console/gardener/group/apply", "group=cleanup&id="+archive.ID+"&id="+consolidate.ID)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	path, param, msg := gtLocation(t, rr)
	require.Equal(t, "error", param)
	require.Equal(t, "/console/gardener/"+consolidate.ID, path, "the refusal opens the proposal to decide alone")
	require.Contains(t, msg, "Nothing was applied: Consolidate 1 memory (→ unified)")
	require.Contains(t, msg, "because applying it cannot be undone")
	for _, p := range []store.Proposal{archive, consolidate} {
		got, _, err := store.ProposalByID(f.ctx, f.db, p.ID)
		require.NoError(t, err)
		require.Equal(t, store.ProposalPending, got.Status, p.Kind)
	}

	// Refusals: no section, a split plan, no ids, an id from another section,
	// an unknown id. An id decided meanwhile is skipped and counted.
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/group/apply", "id="+late.ID).Code)
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/group/apply", "group=split&id="+late.ID).Code)
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/group/apply", "group=errors").Code)
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/group/apply", "group=cleanup&id="+late.ID).Code)
	require.Equal(t, http.StatusNotFound, postForm(f.mux, "/console/gardener/group/apply", "group=errors&id=NOSUCH").Code)
	_, param, msg = gtLocation(t, postForm(f.mux, "/console/gardener/group/apply", "group=errors&id="+wanted.ID+"&id="+late.ID))
	require.Equal(t, "notice", param, msg)
	require.Equal(t, "Applied 1 proposal from Errors & gaps. Skipped 1 already decided. Undo any of them from the decision history.", msg)
}

// gtSection is one rail section's markup, by taxonomy key.
func gtSection(t *testing.T, page, key string) string {
	t.Helper()
	start := strings.Index(page, `<section class="rail-group" id="rg-`+key+`">`)
	require.NotEqual(t, -1, start, "no %s section", key)
	end := strings.Index(page[start:], "</section>")
	require.NotEqual(t, -1, end)
	return page[start : start+end]
}

// A proposal whose reader asks to be read first -- one touching a project
// fenced since it was proposed -- keeps its section out of Apply all, and the
// route refuses it in a batch.
func TestGtApplyAll_LeavesOutWhatAsksToBeReadFirst(t *testing.T) {
	f := gtConsole(t)
	_, err := store.EnsureProject(f.ctx, f.db, "example-legal", "example-legal")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(f.ctx, f.db, "example-legal", core.IsolationConfidential))
	p, err := store.CreateProposal(f.ctx, f.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:example-legal:1", "project": "example-legal", "surface": "tool", "name": "tasks_add",
		"error_count": 3.0, "session_count": 2.0, "suggested_title": "tasks_add: unknown parameter <v>",
	})
	require.NoError(t, err)
	c := f.svc.toProposalCard(f.ctx, p)
	require.NotEmpty(t, c.Fenced)
	require.Equal(t, "it touches a project fenced since it was proposed", c.BulkBlock)

	page := getPeek(t, f.mux, "/console/gardener").Body.String()
	require.NotContains(t, gtSection(t, page, "errors"), "/console/gardener/group/apply")
	_, param, msg := gtLocation(t, postForm(f.mux, "/console/gardener/group/apply", "group=errors&id="+p.ID))
	require.Equal(t, "error", param)
	require.Contains(t, msg, "Nothing was applied")
	got, _, err := store.ProposalByID(f.ctx, f.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)
}

// Dismiss all dismisses the rows the rail shows: under the Source filter, the
// form carries src=, and the route leaves the other source's rows pending.
func TestGtGroupDismiss_HonorsTheSourceFilter(t *testing.T) {
	f := gtConsole(t)
	asked, err := store.CreateProposal(f.ctx, f.db, store.ProposalDigest, map[string]any{
		"key": "digest:example-ops:2026-07", "project": "example-ops", "month": "2026-07", "title": "asked",
		"body": "x", "source": "request", "request_text": "roll up July",
	})
	require.NoError(t, err)
	found, err := store.CreateProposal(f.ctx, f.db, store.ProposalDigest, map[string]any{
		"key": "digest:example-ops:2026-08", "project": "example-ops", "month": "2026-08", "title": "found", "body": "y",
	})
	require.NoError(t, err)

	page := getPeek(t, f.mux, "/console/gardener?src=you").Body.String()
	digests := gtSection(t, page, "digests")
	require.Contains(t, digests, `<input type="hidden" name="src" value="you">`)
	require.Contains(t, digests, "Dismiss all 1")
	require.NotContains(t, gtSection(t, getPeek(t, f.mux, "/console/gardener").Body.String(), "digests"), `name="src"`,
		"unfiltered, the form names no source")

	_, param, msg := gtLocation(t, postForm(f.mux, "/console/gardener/group/dismiss", "group=digests&src=you"))
	require.Equal(t, "notice", param, msg)
	require.Contains(t, msg, "Dismissed 1 proposal from Digests.")
	got, _, err := store.ProposalByID(f.ctx, f.db, asked.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalDismissed, got.Status)
	got, _, err = store.ProposalByID(f.ctx, f.db, found.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status, "the background row was not shown, so it stays")
	require.Equal(t, http.StatusBadRequest, postForm(f.mux, "/console/gardener/group/dismiss", "group=digests&src=bogus").Code)
}

// Retarget records its gardener.action event like every other Gardener
// decision: the same kind, the action, who decided as by, plus the
// destination before (from) and after (to). A retarget to where it already
// goes writes and records nothing.
func TestGtRetarget_RecordsItsEvent(t *testing.T) {
	ctx, db, mux := newConsoleForSplit(t, stubChat{out: splitConsoleJSON})
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/split", "source=example-app").Code)
	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.NotEmpty(t, reps)
	p := reps[0]
	from := payloadStr(p.Payload, "to")
	to := "example-android"
	require.NotEqual(t, from, to)
	require.Len(t, reps, 2, "the split proposes one move per memory")

	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/gardener/"+p.ID+"/retarget", "to="+to).Code)
	f := gtFixture{ctx: ctx, db: db}
	ev := gtEvent(t, f, p.ID, "retarget")
	require.NotNil(t, ev, "a retarget records its event")
	require.Equal(t, map[string]any{"action": "retarget", "kind": "reproject", "by": "console", "from": from, "to": to}, ev)

	// The same shape as an apply's: action, kind and by, from one recorder.
	require.Equal(t, http.StatusSeeOther, post(mux, "/console/gardener/"+p.ID+"/dismiss").Code)
	dismiss := gtEvent(t, f, p.ID, "dismiss")
	require.Equal(t, "console", dismiss["by"], "the same by as a dismissal's")

	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind = ? AND json_extract(payload, '$.action') = 'retarget'`,
		string(core.EventGardenerAction)).Scan(&n))
	require.Equal(t, 1, n)
	other := reps[len(reps)-1]
	same := payloadStr(other.Payload, "to")
	_, param, msg := gtLocation(t, postForm(mux, "/console/gardener/"+other.ID+"/retarget", "to="+same))
	require.Equal(t, "notice", param)
	require.Contains(t, msg, "already moves to "+same)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind = ? AND json_extract(payload, '$.action') = 'retarget'`,
		string(core.EventGardenerAction)).Scan(&n))
	require.Equal(t, 1, n, "no change, no event")
}

// The decision history opens with Applied automatically: every proposal
// automatic apply applied that is still applied, uncapped and first, each
// with its Undo; every other decision is paged below it, so each appears
// once. Undo takes an automatic apply back and it leaves the section.
func TestGtDecisions_AppliedAutomaticallyFirst(t *testing.T) {
	f := gtConsole(t)
	at := time.Now().UTC().Add(-time.Hour)
	var autoIDs []string
	for i := range 23 { // more than a page: the section is uncapped
		p, err := store.CreateProposal(f.ctx, f.db, store.ProposalShipPlan, map[string]any{
			"key": fmt.Sprintf("ship_plan:P%02d", i), "id": fmt.Sprintf("P%02d", i), "slug": fmt.Sprintf("plan-%02d", i),
			"title": fmt.Sprintf("Plan %02d", i), "project": "example-ops", "plan_status": "presented",
		})
		require.NoError(t, err)
		require.NoError(t, store.ResolveProposal(f.ctx, f.db, p.ID, store.ProposalApplied, gardener.ByAuto, at.Add(time.Duration(i)*time.Second)))
		autoIDs = append(autoIDs, p.ID)
	}
	// One applied for real under the automatic actor, so its Undo has
	// something to reverse.
	real, err := store.CreateProposal(f.ctx, f.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:example-ops:auto", "project": "example-ops", "surface": "tool", "name": "tasks_add",
		"error_count": 3.0, "session_count": 2.0, "suggested_title": "tasks_add: auto",
	})
	require.NoError(t, err)
	_, err = f.garden.Apply(gardener.WithActor(f.ctx, gardener.ByAuto), real.ID)
	require.NoError(t, err)
	// And decisions by others: 21 dismissals by the owner, past one page.
	var owner []store.Proposal
	for i := range 21 {
		owner = append(owner, gdDecide(t, f.ctx, f.garden, f.db, i, gardener.ByOwner, "dismiss"))
	}

	page := getPeek(t, f.mux, "/console/gardener/decisions").Body.String()
	autoAt := strings.Index(page, `id="gardener-auto"`)
	othersAt := strings.Index(page, "Every other decision")
	require.NotEqual(t, -1, autoAt)
	require.NotEqual(t, -1, othersAt)
	require.Less(t, autoAt, othersAt, "Applied automatically comes first")
	autoPart := page[autoAt:othersAt]
	require.Contains(t, autoPart, "Applied automatically")
	require.Equal(t, 24, strings.Count(autoPart, `class="gardener-decision tone-`), "uncapped: every automatic apply")
	require.Contains(t, autoPart, `action="/console/gardener/`+real.ID+`/undo"`)
	require.Contains(t, autoPart, ">automatic apply</span>")
	rest := page[othersAt:]
	require.Equal(t, 20, strings.Count(rest, `class="gardener-decision tone-`), "the rest pages, a page at a time")
	for _, id := range append(autoIDs, real.ID) {
		require.NotContains(t, rest, `id="gd-`+id+`"`, "each decision appears once")
	}
	require.Contains(t, page, "<strong>45</strong>decided", "the head counts both")

	var model decisionsView
	require.NoError(t, json.Unmarshal(getPeek(t, f.mux, "/console/gardener/decisions?format=json").Body.Bytes(), &model))
	require.Len(t, model.Auto, 24)
	require.Equal(t, 24, model.AutoCount)
	require.Equal(t, 21, model.Total)
	require.Equal(t, 2, model.Pages)
	var page2 decisionsView
	require.NoError(t, json.Unmarshal(getPeek(t, f.mux, "/console/gardener/decisions?page=2&format=json").Body.Bytes(), &page2))
	require.Empty(t, page2.Auto, "the section leads page 1 only")
	require.Equal(t, 24, page2.AutoCount)
	require.Len(t, page2.Rows, 1)
	require.Equal(t, owner[0].ID, page2.Rows[0].ID)
	require.NotContains(t, getPeek(t, f.mux, "/console/gardener/decisions?page=2").Body.String(), `id="gardener-auto"`)

	// Undo the real one: the task it opened is gone, it is pending again, and
	// it leaves the section.
	tasks, err := store.ListTasks(f.ctx, f.db, "example-ops", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, http.StatusSeeOther, post(f.mux, "/console/gardener/"+real.ID+"/undo").Code)
	tasks, err = store.ListTasks(f.ctx, f.db, "example-ops", core.TaskOpen)
	require.NoError(t, err)
	require.Empty(t, tasks)
	require.NoError(t, json.Unmarshal(getPeek(t, f.mux, "/console/gardener/decisions?format=json").Body.Bytes(), &model))
	require.Len(t, model.Auto, 23)
	for _, r := range model.Auto {
		require.NotEqual(t, real.ID, r.ID)
	}

	// With nothing applied automatically there is no section and no
	// "every other" heading.
	plain := gtConsole(t)
	gdDecide(t, plain.ctx, plain.garden, plain.db, 0, gardener.ByOwner, "dismiss")
	body := getPeek(t, plain.mux, "/console/gardener/decisions").Body.String()
	require.NotContains(t, body, `id="gardener-auto"`)
	require.NotContains(t, body, "Every other decision")
}

// The Applied automatically section reads through the request's handle: an
// automatic apply about a locked project's memory is absent from it.
func TestGtDecisions_AppliedAutomaticallyLockedProjectIsAbsent(t *testing.T) {
	const locked = "example-finance"
	f := gtConsole(t, locked)
	now := time.Now().UTC()
	ids := map[string]string{}
	for _, proj := range []string{locked, "example-ops"} {
		p, err := store.CreateProposal(f.ctx, f.db, store.ProposalMemoryWanted, map[string]any{
			"key": "memory_wanted:" + proj + ":x", "project": proj, "suggested_title": proj + " gap", "miss_count": 2.0,
		})
		require.NoError(t, err)
		require.NoError(t, store.ResolveProposal(f.ctx, f.db, p.ID, store.ProposalApplied, gardener.ByAuto, now))
		ids[proj] = p.ID
	}
	body := getPeek(t, f.mux, "/console/gardener/decisions").Body.String()
	require.Contains(t, body, `id="gardener-auto"`)
	require.Contains(t, body, `id="gd-`+ids["example-ops"]+`"`)
	require.NotContains(t, body, ids[locked], "the locked project's automatic apply is absent")
	require.NotContains(t, body, locked)
	var model decisionsView
	require.NoError(t, json.Unmarshal(getPeek(t, f.mux, "/console/gardener/decisions?format=json").Body.Bytes(), &model))
	require.Len(t, model.Auto, 1)
	require.Equal(t, 1, model.AutoCount, "the count reads through the same handle")

	open := gtConsole(t)
	p, err := store.CreateProposal(open.ctx, open.db, store.ProposalMemoryWanted, map[string]any{
		"key": "memory_wanted:" + locked + ":x", "project": locked, "suggested_title": locked + " gap", "miss_count": 2.0,
	})
	require.NoError(t, err)
	require.NoError(t, store.ResolveProposal(open.ctx, open.db, p.ID, store.ProposalApplied, gardener.ByAuto, now))
	require.Contains(t, getPeek(t, open.mux, "/console/gardener/decisions").Body.String(), `id="gd-`+p.ID+`"`,
		"without the lock it is listed: the absence is the view's")
}

// The screen's own sentence says what the switches say: whether agents may
// apply proposals, and which kinds apply automatically, with the way to
// Settings.
func TestGtContract_SaysWhatTheSwitchesSay(t *testing.T) {
	def := gardenerContract(gardenerSwitches{AgentsMayApply: true})
	require.False(t, def.Auto)
	require.Equal(t, "Proposes only", def.Badge)
	require.Equal(t, "A proposal changes nothing until it is applied. You and your agents may apply one, and the decision history shows who did. None applies automatically.", def.Copy)
	require.Equal(t, gardenerSettingsHref, def.Href)

	barred := gardenerContract(gardenerSwitches{AgentsMayApply: false})
	require.Contains(t, barred.Copy, "Only you may apply one; agents are refused")
	require.NotContains(t, barred.Copy, "your agents may")

	auto := gardenerContract(gardenerSwitches{AgentsMayApply: true,
		AutoApply: []string{store.ProposalAbandonPlan, store.ProposalMemoryWanted, store.ProposalToolError}})
	require.True(t, auto.Auto)
	require.Equal(t, "Some apply automatically", auto.Badge)
	require.Contains(t, auto.Copy, "Stale plans, missing memories and recurring errors apply automatically, as soon as a pass finds them.")
	require.NotContains(t, auto.Copy, "None applies")

	// The page renders whatever the switches in force say.
	f := gtConsole(t)
	want := gardenerContract(f.svc.gardenerSwitches(f.ctx))
	page := getPeek(t, f.mux, "/console/gardener").Body.String()
	require.Contains(t, page, want.Copy)
	require.Contains(t, page, `<a href="`+gardenerSettingsHref+`">Settings &rsaquo; Gardener</a>`)
	require.NotContains(t, page, "Propose-only by design.")
}

// Every proposal kind says why it was raised in a plain sentence, and each
// reason a background pass writes reads as words, not shorthand. A reason
// none of the shapes knows is kept, as a sentence.
func TestGtProposalWhy_EveryKindReadsAsASentence(t *testing.T) {
	sentence := func(t *testing.T, s string) {
		t.Helper()
		require.NotEmpty(t, s)
		first := []rune(s)[0]
		require.True(t, unicode.IsUpper(first) || unicode.IsDigit(first), "%q starts a sentence", s)
		require.True(t, strings.HasSuffix(s, "."), "%q ends one", s)
		require.NotContains(t, s, "_")
	}
	for _, kind := range store.ProposalKinds {
		for _, src := range []string{"", "request"} {
			c := proposalCard{Kind: kind, Source: src, ScorePercent: 91, SessionCount: 1, Project: "example-ops", Month: "2026-07"}
			sentence(t, proposalWhy(kind, map[string]any{}, c))
		}
	}

	for reason, want := range map[string]string{
		// staleness.go
		"no activity in 90d": "Nothing has updated, delivered or read it in 90 days, and no other memory links to it.",
		// stages.go
		"stage with no parsable Status header, no update in 14d": "It is a stage with no Status line Seamless can read, so it gates nothing, and it has not changed in 14 days.",
		"stage with status done, no update in 1d":                "It is a stage whose Status line reads done, so it gates nothing, and it has not changed in 1 day.",
		// utility.go
		"dead weight: briefing-injected 24x in 30d, zero recall/prompt/read demand": "Briefings gave it to agents 24 times in 30 days, and not one search, prompt or read asked for it.",
		// wanted.go
		"knowledge gap: recall found nothing 3x across 2 sessions in 14d": "Agents searched for this 3 times across 2 sessions in 14 days, and recall found nothing.",
		// errwanted.go
		"recurring tool error: tasks_add returned this error 5x across 3 sessions in 14d":               "Agents' calls to tasks_add returned this error 5 times across 3 sessions in 14 days.",
		`recurring hook failure: stage "session-start: briefing" failed 4x in 14d, swallowed fail-open`: "The session-start: briefing hook stage failed 4 times in 14 days; a hook carries on past a failure, so no agent saw it.",
		// plans.go, mergeplans.go
		"never approved in 14d":                                                                                         "Nobody approved it in 14 days.",
		"never approved in 14d; repo unchanged since the capture":                                                       "Nobody approved it in 14 days, and its repository has not changed since it was captured.",
		"never approved in 14d; 6 commit(s) since the capture, none matching the plan":                                  "Nobody approved it in 14 days, and none of the 6 commits since it was captured match the plan.",
		"unapproved for 14d, but 3 of 8 commit(s) since its capture match the plan -- the work appears to have shipped": "Nobody approved it in 14 days, but 3 of the 8 commits since it was captured match the plan, so the work looks shipped.",
		`unapproved for 14d and carries no steps of its own, while plan "ux-pass" in the same project holds the steps for what looks like the same work (3 shared title words)`: "Nobody approved it in 14 days and it has no steps of its own, while plan ux-pass in the same project holds the steps for what looks like the same work: their titles share 3 words.",
		// relocate.go
		"written by a session bound to example-legal, which is now confidential -- this memory stayed in the global scope, outside the fence": "A session bound to example-legal wrote it, and example-legal is now confidential, but the memory stayed in the global scope, outside the fence.",
		// copies.go: already a clause; kept and sentence-cased
		"its source example-ops:01ABC changed after this copy was last written": "Its source example-ops:01ABC changed after this copy was last written.",
		// a request's own words, kept
		"superseded by the newer auth memory": "Superseded by the newer auth memory.",
	} {
		got := proposalWhy(store.ProposalArchive, map[string]any{"reason": reason}, proposalCard{})
		require.Equal(t, want, got, reason)
	}
	// A request with no reason of its own says it was asked for.
	require.Equal(t, "You asked for it to be archived.",
		proposalWhy(store.ProposalArchive, map[string]any{"reason": "requested"}, proposalCard{Source: "request"}))
}

// The reader names where a proposal came from in words, not the raw kind
// or "auto"; its decision buttons carry a and x; and no Gardener confirm is
// a native confirm() any more.
func TestGtReader_WordsKeysAndInPageConfirms(t *testing.T) {
	f := gtConsole(t)
	toolErr, _, consolidate, _ := gtQueue(t, f)
	asked, err := store.CreateProposal(f.ctx, f.db, store.ProposalDigest, map[string]any{
		"key": "digest:example-ops:2026-07", "project": "example-ops", "month": "2026-07", "title": "asked",
		"body": "x", "source": "request", "request_text": "roll up July",
	})
	require.NoError(t, err)

	reader := getPeek(t, f.mux, "/console/gardener/"+toolErr.ID+"?reader=1").Body.String()
	require.Contains(t, reader, `<span class="kind">found by a background pass</span>`)
	require.NotContains(t, reader, `<span class="kind">tool_error</span>`)
	require.Contains(t, reader, `data-reader-key="a"`)
	require.Contains(t, reader, `data-reader-key="x"`)
	require.NotContains(t, reader, "data-confirm=", "an undoable apply and a dismissal ask nothing")
	require.Contains(t, getPeek(t, f.mux, "/console/gardener/"+asked.ID+"?reader=1").Body.String(), `<span class="kind">asked by you</span>`)

	gated := getPeek(t, f.mux, "/console/gardener/"+consolidate.ID+"?reader=1").Body.String()
	require.Contains(t, gated, `data-confirm="This cannot be undone from the console."`)

	page := getPeek(t, f.mux, "/console/gardener").Body.String()
	require.Contains(t, page, `<span class="gardener-src-chip you" title="Asked by you">asked by you</span>`)
	require.Contains(t, page, `<span class="gardener-src-chip" title="Found by a background pass">background</span>`)
	require.NotContains(t, page, ">auto</span>")

	source, err := templateFS.ReadFile("templates/gardener.html")
	require.NoError(t, err)
	require.NotContains(t, string(source), "confirm(", "every Gardener confirmation asks in the page")

	js := getPeek(t, f.mux, "/console/static/library.js").Body.String()
	require.Contains(t, js, `button[data-reader-key="`, "library.js presses the reader's key buttons")
	require.Contains(t, js, "chord-hint", "and steps aside while a g-chord waits for its key")
	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	require.Contains(t, string(layout), "<dd>Apply what the reader shows (press again to confirm a change that cannot be undone)</dd>", "the ? sheet lists a")
	require.Contains(t, string(layout), "<dd>Dismiss what the reader shows</dd>", "and x")
}

// The queue comes first: under the head sit the status line and one line of
// tools, the ask box folded into a button (open only to finish a split
// request), and with no chat model, one line pointing at Settings.
func TestGtQueue_ComesFirst(t *testing.T) {
	ctx, _, mux := newConsoleWithChat(t, `{"ops":[]}`)
	_ = ctx
	page := getPeek(t, mux, "/console/gardener").Body.String()
	require.Contains(t, page, `<details class="gardener-composer" id="gardener-ask">`, "folded: no open attribute")
	require.Contains(t, page, `<summary class="btn small gardener-ask-toggle" id="gardener-ask-title">`)
	require.NotContains(t, page, "gardener-request-unavailable")
	split := getPeek(t, mux, "/console/gardener?split=split+the+app").Body.String()
	require.Contains(t, split, `<details class="gardener-composer" id="gardener-ask" open>`, "a split follow-up opens it")

	f := gtConsole(t)
	gtQueue(t, f)
	body := getPeek(t, f.mux, "/console/gardener").Body.String()
	require.Contains(t, body, `class="gardener-ask-off"`)
	require.NotContains(t, body, "Natural-language cleanup is not connected.")
	tools := strings.Index(body, `class="gardener-tools"`)
	queue := strings.Index(body, `class="lib-layout"`)
	require.NotEqual(t, -1, tools)
	require.NotEqual(t, -1, queue)
	between := body[tools:queue]
	for _, block := range []string{"<form", "<article", "<section", "<textarea"} {
		require.NotContains(t, between, block, "nothing but the tools line sits between the head and the queue")
	}
	require.Equal(t, 2, strings.Count(between, "<p "), "the contract line and the one line about asking")
}
