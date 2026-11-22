package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// nwRecord records one event and returns its id.
func nwRecord(t *testing.T, db *sql.DB, ev core.Event) string {
	t.Helper()
	id, err := events.NewRecorder(db).Record(context.Background(), ev)
	require.NoError(t, err)
	return id
}

// nwEventPage fetches an event's full page and its JSON model.
func nwEventPage(t *testing.T, mux *http.ServeMux, id string) (string, eventDetailData) {
	t.Helper()
	rr := getPeek(t, mux, "/console/events/"+id)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var d eventDetailData
	getJSON(t, mux, "/console/events/"+id+"?format=json", &d)
	return rr.Body.String(), d
}

// An event about a memory links that memory by name; one about a task links
// the task with its state in the task words; neither falls back to "no
// captured body" or an unlinked short id.
func TestNwEventSubject_MemoryAndTaskLinkTheirItem(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mem := writeMemory(t, mgr, core.KindRunbook, "orbital", "dedupe-backfill-runbook", "Backfill the dedupe store from the outbox.")
	taskID := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: taskID, ProjectSlug: "orbital", Title: "Benchmark the new storage pool", PlanSlug: "storage",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now,
	}))

	memEv := nwRecord(t, db, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "orbital", ItemID: mem.ID,
		Payload: map[string]any{"name": mem.Name, "kind": "runbook"}})
	body, d := nwEventPage(t, mux, memEv)
	require.Len(t, d.Subjects, 1)
	sub := d.Subjects[0]
	require.Equal(t, subjectMemory, sub.Kind)
	require.Equal(t, "dedupe-backfill-runbook", sub.Name)
	require.Equal(t, "/console/memories/"+mem.ID, sub.Href)
	require.False(t, sub.Missing)
	require.Contains(t, body, `<a href="/console/memories/`+mem.ID+`" data-peek>dedupe-backfill-runbook</a>`)
	require.Contains(t, body, "Backfill the dedupe store from the outbox.")
	require.NotContains(t, body, "No captured body")
	require.NotContains(t, body, "Nothing more was recorded", "the item card is the event's body")
	// The inspector's Item row links the same item.
	item := body[strings.Index(body, "<dt>Item</dt>"):]
	item = item[:strings.Index(item, "</dd>")]
	require.Contains(t, item, `href="/console/memories/`+mem.ID+`"`)

	taskEv := nwRecord(t, db, core.Event{Kind: core.EventTaskTransition, ProjectSlug: "orbital", ItemID: taskID,
		Payload: map[string]any{"created": true, "to": "open"}})
	body, d = nwEventPage(t, mux, taskEv)
	require.Len(t, d.Subjects, 1)
	require.Equal(t, "Benchmark the new storage pool", d.Subjects[0].Name)
	require.Equal(t, "/console/tasks/"+taskID, d.Subjects[0].Href)
	require.NotNil(t, d.Subjects[0].Word)
	require.Equal(t, wordReady, d.Subjects[0].Word.Key)
	require.Contains(t, body, `<a href="/console/tasks/`+taskID+`" data-peek>Benchmark the new storage pool</a>`)
	require.Contains(t, body, `data-word="ready"`)
	require.Contains(t, body, "a step of plan:storage")
}

// A gardener event names what its proposal is about: the page's title and
// heading say "proposed archiving a memory: <name>", the proposal links to the
// Gardener while it waits and to the decision history once decided, and the
// memory links to its page.
func TestNwEventSubject_ProposalNamesItsMemory(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	mem := writeMemory(t, mgr, core.KindGotcha, "orbital", "billing-replay-landed", "Replay protection landed.")
	p, err := store.CreateProposal(ctx, db, store.ProposalArchive, map[string]any{
		"id": mem.ID, "name": mem.Name, "project": "orbital", "kind": "gotcha", "reason": "unused for 120 days",
	})
	require.NoError(t, err)
	evID := nwRecord(t, db, core.Event{Kind: core.EventGardenerAction, ItemID: p.ID,
		Payload: map[string]any{"action": "propose", "kind": store.ProposalArchive, "key": "archive:" + mem.ID}})

	body, d := nwEventPage(t, mux, evID)
	require.Equal(t, "proposed archiving a memory: billing-replay-landed", d.Headline)
	require.Contains(t, body, "<title>proposed archiving a memory: billing-replay-landed &middot; Seamless</title>",
		"the tab is titled by the event's headline, not its short id")
	require.Contains(t, body, "<h1>proposed archiving a memory: billing-replay-landed</h1>")
	require.Len(t, d.Subjects, 2)
	require.Equal(t, subjectProposal, d.Subjects[0].Kind)
	require.Equal(t, "/console/gardener/"+p.ID, d.Subjects[0].Href)
	require.Equal(t, store.ProposalPending, d.Subjects[0].Word.Key)
	require.Equal(t, "unused for 120 days", d.Subjects[0].Detail)
	require.Equal(t, "/console/memories/"+mem.ID, d.Subjects[1].Href)
	require.Equal(t, "the memory to archive", d.Subjects[1].Role)
	require.Contains(t, body, `<a href="/console/gardener/`+p.ID+`">Archiving a memory</a>`, "a proposal has no peek fragment")
	require.Contains(t, body, `<a href="/console/memories/`+mem.ID+`" data-peek>billing-replay-landed</a>`)
	require.Contains(t, body, "waiting for review")

	// Decided, it links to the decision history instead.
	require.NoError(t, store.ResolveProposal(ctx, db, p.ID, store.ProposalApplied, "console", time.Now().UTC()))
	body, d = nwEventPage(t, mux, evID)
	require.Equal(t, "/console/gardener/decisions", d.Subjects[0].Href)
	require.Equal(t, store.ProposalApplied, d.Subjects[0].Word.Key)
	require.Contains(t, body, `data-word="applied"`)

	// A merge names both sides, in the order the proposal uses them.
	other := writeMemory(t, mgr, core.KindGotcha, "orbital", "replay-dedupe-index", "The unique index.")
	merge, err := store.CreateProposal(ctx, db, store.ProposalMerge, map[string]any{
		"keep": map[string]any{"id": mem.ID, "name": mem.Name, "project": "orbital"},
		"drop": map[string]any{"id": other.ID, "name": other.Name, "project": "orbital"},
	})
	require.NoError(t, err)
	mergeEv := nwRecord(t, db, core.Event{Kind: core.EventGardenerAction, ItemID: merge.ID,
		Payload: map[string]any{"action": "propose", "kind": store.ProposalMerge}})
	_, d = nwEventPage(t, mux, mergeEv)
	require.Equal(t, "proposed merging duplicate memories: billing-replay-landed and replay-dedupe-index", d.Headline)
	require.Equal(t, []string{"the memory to keep", "the memory to retire into it"}, []string{d.Subjects[1].Role, d.Subjects[2].Role})
}

// Each kind's item resolves to its own page: a note, a session, a plan with a
// page and one without, a trial (whose page is the research feature's), and a
// starred item. An id that names nothing is a card with its kind and id, and
// no name.
func TestNwEventSubject_EachKindResolves(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	note, err := mgr.WriteNote(ctx, core.Note{ID: mustID(t), Slug: "nw-narrative", Title: "The plan narrative",
		Project: "demo", Body: "# Narrative\n", Tags: []string{"plan:nw-plan", "created-by:agent"}, Created: now, Updated: now})
	require.NoError(t, err)
	sessID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: sessID, Name: "cc/nwsess", ProjectSlug: "demo",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: mustID(t), ProjectSlug: "demo", Title: "a step",
		PlanSlug: "nw-steps", Status: core.TaskDone, CreatedAt: now, UpdatedAt: now}))
	trialID := mustID(t)
	require.NoError(t, store.CreateTrial(ctx, db, core.Trial{ID: trialID, Lab: "nw-lab", Title: "try the cache",
		Outcome: core.OutcomePass, ProjectSlug: "demo", CreatedAt: now}))

	for _, tc := range []struct {
		name string
		ev   core.Event
		kind string
		want string // the subject's name, "" for an unresolved one
		href string
	}{
		{"note written", core.Event{Kind: core.EventNoteWritten, ProjectSlug: "demo", ItemID: note.ID,
			Payload: map[string]any{"title": note.Title}}, subjectNote, "The plan narrative", "/console/notes/" + note.ID},
		{"session started", core.Event{Kind: core.EventSessionStarted, ProjectSlug: "demo", SessionID: sessID},
			subjectSession, "cc/nwsess", "/console/sessions/" + sessID},
		{"plan shipped, with a page", core.Event{Kind: core.EventPlanShipped, ProjectSlug: "demo", ItemID: "nw-plan",
			Payload: map[string]any{"plan": "nw-plan"}}, subjectPlan, "plan:nw-plan", "/console/plans/nw-plan?project=demo"},
		{"plan shipped, steps only", core.Event{Kind: core.EventPlanShipped, ProjectSlug: "demo", ItemID: "nw-steps",
			Payload: map[string]any{"plan": "nw-steps"}}, subjectPlan, "plan:nw-steps", "/console/projects/demo?tab=tasks"},
		{"trial, research off", core.Event{Kind: core.EventTrialRecorded, ProjectSlug: "demo", ItemID: trialID,
			Payload: map[string]any{"title": "try the cache"}}, subjectTrial, "try the cache", ""},
		{"starred session", core.Event{Kind: core.EventFavoriteChanged, ProjectSlug: "demo", ItemID: sessID,
			Payload: map[string]any{"kind": "session", "id": sessID, "favorite": true}}, subjectSession, "cc/nwsess", "/console/sessions/" + sessID},
		{"deleted memory", core.Event{Kind: core.EventMemoryRead, ProjectSlug: "demo", ItemID: "01NOSUCHMEMORY0000000000000",
			Payload: map[string]any{"name": "gone"}}, subjectMemory, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := nwRecord(t, db, tc.ev)
			body, d := nwEventPage(t, mux, id)
			require.Len(t, d.Subjects, 1)
			sub := d.Subjects[0]
			require.Equal(t, tc.kind, sub.Kind)
			require.Equal(t, tc.want, sub.Name)
			require.Equal(t, tc.href, sub.Href)
			require.Equal(t, tc.want == "", sub.Missing)
			require.Contains(t, body, `data-subject="`+tc.kind+`"`)
			if sub.Missing {
				require.Contains(t, body, ">not found</span>")
				require.Contains(t, body, "Not here any more, or in a project this console does not show.")
			}
		})
	}

	// An event about no item (a hook run) keeps the plain empty card.
	id := nwRecord(t, db, core.Event{Kind: core.EventRecallMiss, ProjectSlug: "demo", Payload: map[string]any{"source": "hook"}})
	body, d := nwEventPage(t, mux, id)
	require.Empty(t, d.Subjects)
	require.Contains(t, body, "Nothing more was recorded")
}

// An event's item in a locked project is neither resolved nor named. The
// event view already hides an event about a locked item, so its page is a
// plain miss; and resolution itself reads through the request's handle, so
// even an event read some other way resolves the locked item to nothing --
// while the open project's item resolves on the same console.
func TestNwEventSubject_LockedItemIsNeitherResolvedNorNamed(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	open := writeMemory(t, lc.mgr, core.KindGotcha, openSlug, "ops-open-gotcha", "an open gotcha "+openMarker)
	bearer := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		return lc.do(req)
	}

	// The page: recorded under the open project, about the locked memory.
	evID, err := lc.rec.Record(ctx, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: openSlug, ItemID: lc.memID,
		Payload: map[string]any{"name": "ledger-close"}})
	require.NoError(t, err)
	rr := bearer("/console/events/" + evID)
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.NotContains(t, rr.Body.String(), "ledger-close")
	require.NotContains(t, rr.Body.String(), lockedSecret)

	// A proposal naming the locked memory is hidden with it, and so is the
	// gardener's event about the proposal.
	p, err := store.CreateProposal(ctx, lc.db, store.ProposalArchive, map[string]any{
		"id": lc.memID, "name": "ledger-close", "project": lockedSlug,
	})
	require.NoError(t, err)
	gardenerEv, err := lc.rec.Record(ctx, core.Event{Kind: core.EventGardenerAction, ItemID: p.ID,
		Payload: map[string]any{"action": "propose", "kind": store.ProposalArchive}})
	require.NoError(t, err)
	rr = bearer("/console/events/" + gardenerEv)
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.NotContains(t, rr.Body.String(), "ledger-close")

	// Resolution, given the events directly, reads the same handle.
	for _, ev := range []core.Event{
		{Kind: core.EventMemoryWritten, ProjectSlug: openSlug, ItemID: lc.memID},
		{Kind: core.EventNoteWritten, ProjectSlug: openSlug, ItemID: lc.noteID},
		{Kind: core.EventTaskTransition, ProjectSlug: openSlug, ItemID: lc.taskID},
		{Kind: core.EventSessionEnded, ProjectSlug: openSlug, SessionID: lc.sessID},
		{Kind: core.EventPlanShipped, ProjectSlug: lockedSlug, ItemID: lc.plan},
		{Kind: core.EventGardenerAction, ItemID: p.ID, Payload: map[string]any{"action": "propose"}},
	} {
		subs, err := lc.cs.resolveEventSubjects(ctx, ev)
		require.NoError(t, err, ev.Kind)
		require.Len(t, subs, 1, ev.Kind)
		require.True(t, subs[0].Missing, "%s: a locked item does not resolve", ev.Kind)
		require.Empty(t, subs[0].Name, ev.Kind)
		require.Empty(t, subs[0].Href, ev.Kind)
		require.Empty(t, subs[0].Project, ev.Kind)
		require.Equal(t, eventHeadline(ev, "proposed archiving a memory", subs), "proposed archiving a memory",
			"%s: the headline names nothing it could not read", ev.Kind)
	}

	// The control: the open project's memory resolves on the same console.
	openEv, err := lc.rec.Record(ctx, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: openSlug, ItemID: open.ID,
		Payload: map[string]any{"name": open.Name}})
	require.NoError(t, err)
	rr = bearer("/console/events/" + openEv)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `<a href="/console/memories/`+open.ID+`" data-peek>ops-open-gotcha</a>`)
	require.NotContains(t, rr.Body.String(), lockedSecret)
}

// The event page has one <main>, the layout's, and its fragment none; its
// times are the shared local-time helpers, not a printed UTC stamp.
func TestNwEventPage_OneMainAndLocalTimes(t *testing.T) {
	db, mux := newConsole(t)
	id := nwRecord(t, db, core.Event{Kind: core.EventHookError, ProjectSlug: "demo",
		Payload: map[string]any{"stage": "session-start", "error": "briefing assembly failed"}})

	body, _ := nwEventPage(t, mux, id)
	require.Equal(t, 1, strings.Count(body, "<main"), "one main per page")
	require.Contains(t, body, `class="event-primary"`)
	require.Contains(t, body, `data-clock="date"`, "the recorded time is clockDate's, which shell.js localizes")
	require.Contains(t, body, `data-clock="title"`, "the age is agoTime's")
	require.NotRegexp(t, `>\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC<`, body, "no UTC stamp is printed as text")

	peek := getPeek(t, mux, "/console/events/"+id+"?peek=1").Body.String()
	require.NotContains(t, peek, "<main", "the fragment lands inside a page that has one")
	require.Contains(t, peek, `<span class="event-peek-time"><time`)
}
