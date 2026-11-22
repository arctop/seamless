package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// UX 18 (plan:console-ux-pass): restore an archived or superseded memory and
// delete a note from the library readers.

// lrGet answers a bearer GET.
func lrGet(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	return do(mux, req)
}

// lrPost answers a bearer form POST, as JSON when asJSON.
func lrPost(mux *http.ServeMux, path string, form url.Values, asJSON bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	return do(mux, req)
}

// lrJSON decodes a JSON answer.
func lrJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return out
}

// lrFile reads a corpus file's bytes.
func lrFile(t *testing.T, mgr *files.Manager, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mgr.Store().DataDir(), rel))
	require.NoError(t, err)
	return string(raw)
}

// lrEvents lists the kind and payload of every event recorded on an item.
func lrEvents(t *testing.T, db *sql.DB, itemID string) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT kind, payload FROM events WHERE item_id = ? ORDER BY ts, id`, itemID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var kind, payload string
		require.NoError(t, rows.Scan(&kind, &payload))
		p := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(payload), &p))
		p["_kind"] = kind
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

// lrSupersede retires old in favor of replacement through the lifecycle
// package, as memory_write supersedes does.
func lrSupersede(t *testing.T, mgr *files.Manager, old, replacement core.Memory) core.Memory {
	t.Helper()
	var out core.Memory
	require.NoError(t, mgr.Mutate(context.Background(), old.FilePath, func(ctx context.Context) error {
		full, err := mgr.Store().ReadMemory(old.FilePath)
		if err != nil {
			return err
		}
		out, err = lifecycle.Supersede(ctx, mgr, full, replacement, time.Now().UTC())
		return err
	}))
	return out
}

// lrReaderSheet is the reader article of a library page.
func lrReaderSheet(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<article class="reader-sheet"`)
	require.NotEqual(t, -1, i, "no reader on the page")
	return page[i:]
}

var lrRestoreForm = regexp.MustCompile(`<form class="reader-status-act" method="post" action="/console/memories/([0-9A-Z]+)/restore">`)

// Restore brings an archived memory back: it is active in the index again,
// its body is the one it had before the archive, the reader trades Restore
// for Archive, and memory.written records it with by=console. A second press
// changes nothing.
func TestLrRestoreBringsAnArchivedMemoryBack(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	m := writeMemory(t, mgr, core.KindGotcha, "example-ops", "pager-loop", "the pager loops on restart")
	before := lrFile(t, mgr, m.FilePath)

	reader := lrReaderSheet(t, lrGet(t, mux, "/console/memories/"+m.ID).Body.String())
	require.Contains(t, reader, `action="/console/memories/`+m.ID+`/archive"`)
	require.NotRegexp(t, lrRestoreForm, reader, "an active memory has nothing to restore")

	rr := lrPost(mux, "/console/memories/"+m.ID+"/archive", url.Values{"next": {"/console/memories"}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	const qs = "?kind=gotcha&q=pager"
	page := lrGet(t, mux, "/console/memories/"+m.ID+qs).Body.String()
	reader = lrReaderSheet(t, page)
	require.Contains(t, reader, "This memory is archived.")
	require.Contains(t, reader, "It no longer reaches agents: briefings and recall leave it out, and its history stays readable here.")
	require.Equal(t, m.ID, lrRestoreForm.FindStringSubmatch(reader)[1], "the archived banner carries Restore")
	require.Contains(t, reader, `<input type="hidden" name="next" value="/console/memories/`+m.ID+`?kind=gotcha&amp;q=pager">`,
		"Restore returns to this memory with the library state")
	require.NotContains(t, reader, `action="/console/memories/`+m.ID+`/archive"`)

	rr = lrPost(mux, "/console/memories/"+m.ID+"/restore", url.Values{"next": {"/console/memories/" + m.ID + qs + "&notice=old"}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/memories/"+m.ID, loc.Path)
	require.Equal(t, url.Values{"kind": {"gotcha"}, "q": {"pager"}, "notice": {"Restored pager-loop: it reaches agents again."}}, loc.Query())

	idx, ok, err := store.MemoryByID(ctx, db, m.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, idx.InvalidAt, "back in the active index")
	var data memoriesData
	getJSON(t, mux, "/console/memories?format=json", &data)
	require.Equal(t, 1, data.ActiveCount)
	require.Equal(t, 0, data.InactiveCount)
	after, err := mgr.Store().ReadMemory(m.FilePath)
	require.NoError(t, err)
	require.Equal(t, m.Body+"\n", after.Body, "the archive's line comes off the body, and nothing else changes")
	require.NotContains(t, after.Body, "Archived")
	stamp := regexp.MustCompile(`(?m)^updated: .*$`)
	require.Equal(t, stamp.ReplaceAllString(before, ""), stamp.ReplaceAllString(lrFile(t, mgr, m.FilePath), ""),
		"the file is what it was before the archive, but for when it was updated")

	reader = lrReaderSheet(t, lrGet(t, mux, "/console/memories/"+m.ID).Body.String())
	require.NotContains(t, reader, "reader-status-banner")
	require.Contains(t, reader, `action="/console/memories/`+m.ID+`/archive"`)

	evs := lrEvents(t, db, m.ID)
	require.Len(t, evs, 2)
	require.Equal(t, "memory.archived", evs[0]["_kind"])
	require.Equal(t, map[string]any{"_kind": "memory.written", "name": "pager-loop", "restored": "archive", "by": "console"}, evs[1])

	// A second press, and a restore of a memory that was never retired, change nothing.
	rr = lrPost(mux, "/console/memories/"+m.ID+"/restore", nil, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/memories/"+m.ID, rr.Header().Get("Location"))
	require.Len(t, lrEvents(t, db, m.ID), 2)
	// A next that is not this memory's page returns to it.
	for _, next := range []string{"https://example.com/console/memories/" + m.ID, "/console/memories", "/console/settings"} {
		rr = lrPost(mux, "/console/memories/"+m.ID+"/restore", url.Values{"next": {next}}, false)
		require.Equal(t, "/console/memories/"+m.ID, rr.Header().Get("Location"), next)
	}
	require.Equal(t, http.StatusNotFound, lrPost(mux, "/console/memories/01NOSUCHMEMORY0000000000AA/restore", nil, false).Code)
}

// A superseded memory comes back only once nothing that replaced it is
// active: while one is, the reader links it and offers no Restore, and the
// POST is refused, naming it, with nothing written. Once it retires, Restore
// clears the edge and its line.
func TestLrRestoreRefusedWhileASuccessorIsActive(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	old := writeMemory(t, mgr, core.KindDecision, "example-ops", "dns-ttl-300", "keep the TTL at 300")
	mid := writeMemory(t, mgr, core.KindDecision, "example-ops", "dns-ttl-120", "lower the TTL to 120")
	head := writeMemory(t, mgr, core.KindDecision, "example-ops", "dns-ttl-60", "lower the TTL to 60")
	old = lrSupersede(t, mgr, old, mid)
	mid = lrSupersede(t, mgr, mid, head)
	before := lrFile(t, mgr, old.FilePath)

	reader := lrReaderSheet(t, lrGet(t, mux, "/console/memories/"+old.ID).Body.String())
	require.Contains(t, reader, `continue with <a href="/console/memories/`+head.ID+`">dns-ttl-60</a>, the active memory that replaced it.`,
		"the banner links the active end of the chain, not the retired link between")
	require.Contains(t, reader, "It can be restored once that one is no longer active.")
	require.NotRegexp(t, lrRestoreForm, reader)

	rr := lrPost(mux, "/console/memories/"+old.ID+"/restore", url.Values{"next": {"/console/memories/" + old.ID + "?q=dns"}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/memories/"+old.ID, loc.Path)
	require.Equal(t, "Not restored: dns-ttl-60 replaced it and is still active.", loc.Query().Get("error"))
	require.Equal(t, "dns", loc.Query().Get("q"))
	page := lrGet(t, mux, loc.RequestURI()).Body.String()
	require.Contains(t, page, "Not restored: dns-ttl-60 replaced it and is still active.", "the page shows why")
	require.Contains(t, page, `href="/console/memories/`+head.ID+`?q=dns"`, "and links the successor")

	rr = lrPost(mux, "/console/memories/"+old.ID+"/restore", nil, true)
	require.Equal(t, http.StatusConflict, rr.Code)
	out := lrJSON(t, rr)
	require.Equal(t, map[string]any{"id": head.ID, "name": "dns-ttl-60", "project": "example-ops", "url": "/console/memories/" + head.ID},
		out["successor"])

	require.Equal(t, before, lrFile(t, mgr, old.FilePath), "nothing changes on disk")
	idx, _, err := store.MemoryByID(ctx, db, old.ID)
	require.NoError(t, err)
	require.NotNil(t, idx.InvalidAt)
	require.Equal(t, mid.ID, idx.SupersededBy)
	for _, ev := range lrEvents(t, db, old.ID) {
		require.NotEqual(t, "memory.written", ev["_kind"], "a refused restore records nothing")
	}

	// The head retires: now nothing active replaced it, and it can come back.
	require.Equal(t, http.StatusSeeOther, lrPost(mux, "/console/memories/"+head.ID+"/archive", nil, false).Code)
	reader = lrReaderSheet(t, lrGet(t, mux, "/console/memories/"+old.ID).Body.String())
	require.Contains(t, reader, `<a href="/console/memories/`+mid.ID+`">dns-ttl-120</a> replaced it and is no longer active either.`)
	require.Regexp(t, lrRestoreForm, reader)
	rr = lrPost(mux, "/console/memories/"+old.ID+"/restore", nil, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, "supersede", lrJSON(t, rr)["restored"])
	restored, err := mgr.Store().ReadMemory(old.FilePath)
	require.NoError(t, err)
	require.True(t, restored.Active())
	require.Empty(t, restored.SupersededBy)
	require.Equal(t, old.Body[:strings.Index(old.Body, "\n\n> Superseded by")]+"\n", restored.Body)
}

// A note delete removes the file and its index rows, as notes_delete does,
// records note.written with deleted and by=console, and returns to the list
// with its state.
func TestLrNoteDeleteRemovesTheFileAndItsIndexRows(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	n := libNote(t, mgr, "example-ops", "scratch-dump", "Scratch dump", time.Now().UTC(), []string{"scratch"})
	keep := libNote(t, mgr, "example-ops", "runbook-notes", "Runbook notes", time.Now().UTC(), nil)
	abs := filepath.Join(mgr.Store().DataDir(), n.FilePath)
	var fts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM fts WHERE item_id = ?`, n.ID).Scan(&fts))
	require.Positive(t, fts, "sanity: the note is in the text index")

	reader := lrReaderSheet(t, lrGet(t, mux, "/console/notes/"+n.ID+"?q=s").Body.String())
	require.Contains(t, reader, `<form method="post" action="/console/notes/`+n.ID+`/delete">`)
	require.Contains(t, reader, `<input type="hidden" name="next" value="/console/notes?q=s">`)
	require.Contains(t, reader, `data-confirm="Delete Scratch dump for good? Its file and index rows go, and nothing brings it back."`)
	require.NotContains(t, reader, `name="narrative"`, "a note outside any plan asks nothing more")

	rr := lrPost(mux, "/console/notes/"+n.ID+"/delete", url.Values{"next": {"/console/notes?q=s&error=old"}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/notes", loc.Path)
	require.Equal(t, url.Values{"q": {"s"}, "notice": {"Deleted Scratch dump."}}, loc.Query())

	_, err = os.Stat(abs)
	require.ErrorIs(t, err, os.ErrNotExist, "the file is gone")
	_, ok, err := store.NoteByID(ctx, db, n.ID)
	require.NoError(t, err)
	require.False(t, ok, "and its index row")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM fts WHERE item_id = ?`, n.ID).Scan(&fts))
	require.Zero(t, fts, "and its text index rows")
	_, ok, err = store.NoteByID(ctx, db, keep.ID)
	require.NoError(t, err)
	require.True(t, ok, "only that note")
	require.Equal(t, []map[string]any{{"_kind": "note.written", "slug": "scratch-dump", "deleted": true, "by": "console"}},
		lrEvents(t, db, n.ID))

	require.Equal(t, http.StatusNotFound, lrPost(mux, "/console/notes/"+n.ID+"/delete", nil, false).Code, "a second press finds nothing")
	rr = lrPost(mux, "/console/notes/"+keep.ID+"/delete", url.Values{"next": {"https://example.com/console/notes"}}, true)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, map[string]any{"status": "deleted", "id": keep.ID}, lrJSON(t, rr), "the answer notes_delete gives")
}

// A plan's narrative says so in the question, needs its box ticked, and a
// delete without narrative=<slug> is refused with nothing removed, since the
// form posts on the first press without script. A note that only supports
// the plan asks nothing more.
func TestLrNarrativeDeleteNeedsItsConfirmation(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	narr := libNote(t, mgr, "example-ops", "plan-dns-cutover", "Plan: DNS cutover", at, []string{"plan:dns-cutover", "created-by:agent"})
	support := libNote(t, mgr, "example-ops", "dns-zone-inventory", "DNS zone inventory", at.Add(time.Minute), []string{"plan:dns-cutover"})

	reader := lrReaderSheet(t, lrGet(t, mux, "/console/notes/"+narr.ID).Body.String())
	require.Contains(t, reader, `<input type="checkbox" name="narrative" value="dns-cutover" required>Delete the narrative of plan:dns-cutover</label>`)
	require.Contains(t, reader, `nothing brings it back. This is plan:dns-cutover's narrative."`)
	peek := lrGet(t, mux, "/console/notes/"+narr.ID+"?peek=1").Body.String()
	require.Contains(t, peek, `name="narrative" value="dns-cutover" required`, "the peek asks the same")

	for _, form := range []url.Values{nil, {"narrative": {"other-plan"}}, {"narrative": {""}}} {
		rr := lrPost(mux, "/console/notes/"+narr.ID+"/delete", form, true)
		require.Equal(t, http.StatusConflict, rr.Code, "%v", form)
		out := lrJSON(t, rr)
		require.Equal(t, []any{"dns-cutover"}, out["narrativeOf"])
		require.Contains(t, out["error"], "this is plan:dns-cutover's narrative")
	}
	rr := lrPost(mux, "/console/notes/"+narr.ID+"/delete", url.Values{"next": {"/console/notes?q=dns"}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/notes/"+narr.ID, loc.Path, "a refusal returns to the note")
	require.Equal(t, "dns", loc.Query().Get("q"))
	require.Contains(t, loc.Query().Get("error"), "Tick the box beside Delete")
	require.FileExists(t, filepath.Join(mgr.Store().DataDir(), narr.FilePath), "nothing is removed")

	supportReader := lrReaderSheet(t, lrGet(t, mux, "/console/notes/"+support.ID).Body.String())
	require.NotContains(t, supportReader, `name="narrative"`, "a supporting note is not the narrative")
	require.Equal(t, http.StatusOK, lrPost(mux, "/console/notes/"+support.ID+"/delete", nil, true).Code)

	rr = lrPost(mux, "/console/notes/"+narr.ID+"/delete", url.Values{"narrative": {"dns-cutover"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoFileExists(t, filepath.Join(mgr.Store().DataDir(), narr.FilePath))
}

// With write screening on, what Held keeps is settled there first: a file in
// drift quarantine is neither restored nor deleted, a flagged memory is not
// restored (a flagged note may still go), and an item an agent's held write
// waits on is left alone. With screening off none of it applies.
func TestLrHeldKeepsRestoreAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newHeldConsole(t, config.Features{Screening: true}, "advanced")
	f.scr = screening.New(screening.Config{DB: f.db, Files: f.mgr, Events: events.NewRecorder(f.db),
		Enabled: func(context.Context) bool { return true }})
	mux := lrRewire(t, f, config.Features{Screening: true})
	off := lrRewire(t, f, config.Features{})
	now := time.Now().UTC()
	write := func(name, body string) core.Memory {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		m, err := f.mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: core.KindReference, Name: name, Project: "app",
			Description: name, Body: body, Created: now, Updated: now, ValidFrom: now})
		require.NoError(t, err)
		return m
	}
	drifted := write("drifted-ref", "plain text\n")
	flagged := write("leaked-token", "token ghp_"+strings.Repeat("Q1w2", 9)+"\n")
	free := write("plain-ref", "plain text\n")
	waiting := libNote(t, f.mgr, "app", "waiting-note", "Waiting note", now, nil)
	flaggedNote, err := f.mgr.WriteNote(ctx, core.Note{ID: "01FLAGGEDNOTE00000000000AA", Title: "Token note", Slug: "token-note",
		Project: "app", Body: "token ghp_" + strings.Repeat("Q1w2", 9) + "\n", Created: now, Updated: now})
	require.NoError(t, err)
	require.NoError(t, f.scr.Sync(ctx))
	for _, m := range []core.Memory{drifted, flagged, free} {
		require.Equal(t, http.StatusSeeOther, lrPost(mux, "/console/memories/"+m.ID+"/archive", nil, false).Code)
	}
	abs := filepath.Join(f.mgr.Store().DataDir(), drifted.FilePath)
	raw, err := os.ReadFile(abs)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(abs, append(raw, []byte("edited by hand\n")...), 0o600))
	_, err = f.scr.CheckDrift(ctx)
	require.NoError(t, err)
	for _, p := range []string{flagged.FilePath, flaggedNote.FilePath} {
		rec, ok, err := store.CorpusRecordByPath(ctx, f.db, p)
		require.NoError(t, err)
		require.True(t, ok && rec.ReviewStatus == store.ReviewFlagged, "sanity: the review flagged %s", p)
	}
	_, err = f.scr.Hold(ctx, store.HeldWrite{Op: store.HeldOpWrite, Tool: "notes_update", ItemKind: core.ItemKindNote,
		ItemID: waiting.ID, Project: "app", Name: waiting.Slug, Content: "x",
		Detail: map[string]string{"path": waiting.FilePath}})
	require.NoError(t, err)

	for _, tc := range []struct {
		name, path, why string
	}{
		{"a drifted memory", "/console/memories/" + drifted.ID, heldDrifted},
		{"a flagged memory", "/console/memories/" + flagged.ID, heldFlagged},
		{"a note a held write waits on", "/console/notes/" + waiting.ID, heldPending},
	} {
		page := lrReaderSheet(t, lrGet(t, mux, tc.path).Body.String())
		require.Contains(t, page, html2(tc.why), tc.name)
		require.Contains(t, page, `<a href="/console/held">Open Held</a>`, tc.name)
		require.NotContains(t, page, `/restore">`, tc.name)
		require.NotContains(t, page, `/delete">`, tc.name)
		action := "/restore"
		if strings.Contains(tc.path, "/notes/") {
			action = "/delete"
		}
		before := lrGet(t, mux, tc.path+"?format=json").Body.String()
		rr := lrPost(mux, tc.path+action, nil, true)
		require.Equal(t, http.StatusConflict, rr.Code, tc.name)
		require.Equal(t, tc.why, lrJSON(t, rr)["error"], tc.name)
		require.Equal(t, before, lrGet(t, mux, tc.path+"?format=json").Body.String(), "%s: nothing changes", tc.name)

		// Screening off: Held is not served, and nothing keeps it.
		require.NotContains(t, lrReaderSheet(t, lrGet(t, off, tc.path).Body.String()), "Open Held", tc.name)
	}
	rec, ok, err := store.CorpusRecordByPath(ctx, f.db, drifted.FilePath)
	require.NoError(t, err)
	require.True(t, ok && rec.Drifted(), "the quarantine stands")

	require.Regexp(t, lrRestoreForm, lrReaderSheet(t, lrGet(t, mux, "/console/memories/"+free.ID).Body.String()))
	require.Equal(t, http.StatusOK, lrPost(mux, "/console/memories/"+free.ID+"/restore", nil, true).Code)
	require.Equal(t, http.StatusOK, lrPost(mux, "/console/notes/"+flaggedNote.ID+"/delete", nil, true).Code,
		"deleting a flagged note only removes it")
}

// html2 is s as html/template escapes it in text.
func html2(s string) string { return strings.ReplaceAll(s, "'", "&#39;") }

// lrRewire builds a second console over f's stores with feats.
func lrRewire(t *testing.T, f heldFixture, feats config.Features) *http.ServeMux {
	t.Helper()
	svc, err := New(Config{DB: f.db, Events: events.NewRecorder(f.db), APIKey: testKey, Files: f.mgr,
		Screening: f.scr, Features: feats, Level: "advanced"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return mux
}

// A locked project stays absent: the reader of a memory a locked project's
// memory replaced names nothing of it and still offers Restore, which the
// POST, checking on the write handle, refuses without a name. The locked
// project's own memories and notes are not found from here.
func TestLrLockedProjectIsAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	view, err := store.OpenView(path, []string{"example-locked"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	console := func(read *sql.DB) *http.ServeMux {
		svc, err := New(Config{DB: read, WriteDB: db, Events: events.NewRecorder(db).ReadingFrom(read), Files: mgr,
			DataDir: dataDir, APIKey: testKey, Level: "advanced"})
		require.NoError(t, err)
		mux := http.NewServeMux()
		svc.Register(mux)
		return mux
	}
	locked, open := console(view), console(db)

	old := writeMemory(t, mgr, core.KindDecision, "example-open", "open-rule", "the open rule")
	hidden := writeMemory(t, mgr, core.KindDecision, "example-locked", "locked-rule", "the locked rule")
	old = lrSupersede(t, mgr, old, hidden)
	before := lrFile(t, mgr, old.FilePath)
	note := libNote(t, mgr, "example-locked", "locked-note", "Locked note", time.Now().UTC(), nil)

	reader := lrReaderSheet(t, lrGet(t, locked, "/console/memories/"+old.ID).Body.String())
	for _, gone := range []string{hidden.ID, "locked-rule", "example-locked"} {
		require.NotContains(t, reader, gone)
	}
	require.Regexp(t, lrRestoreForm, reader, "nothing this request reads replaced it")
	rr := lrPost(locked, "/console/memories/"+old.ID+"/restore", nil, true)
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Equal(t, map[string]any{"error": "Not restored: a memory that replaced it is still active."}, lrJSON(t, rr))
	require.Equal(t, before, lrFile(t, mgr, old.FilePath))

	require.Contains(t, lrReaderSheet(t, lrGet(t, open, "/console/memories/"+old.ID).Body.String()), ">locked-rule</a>, the active memory that replaced it.",
		"without the lock the reader names it: the absence is the view's")
	require.Equal(t, http.StatusNotFound, lrPost(locked, "/console/memories/"+hidden.ID+"/restore", nil, true).Code)
	require.Equal(t, http.StatusNotFound, lrPost(locked, "/console/notes/"+note.ID+"/delete", nil, true).Code)
	require.FileExists(t, filepath.Join(dataDir, note.FilePath))
}

// The library speaks of what happens, never of a tombstone, and asks in the
// page: no native confirm() is left in its templates, and Archive says the
// memory leaves briefings and recall, its history stays, and it can come back.
func TestLrArchiveWordsAndConfirmsAskInThePage(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	m := writeMemory(t, mgr, core.KindGotcha, "example-ops", "cron-drift", "cron drifts")
	gone := writeMemory(t, mgr, core.KindGotcha, "example-ops", "cron-old", "cron was old")
	require.Equal(t, http.StatusSeeOther, lrPost(mux, "/console/memories/"+gone.ID+"/archive", nil, false).Code)
	n := libNote(t, mgr, "example-ops", "cron-notes", "Cron notes", time.Now().UTC(), nil)

	const ask = `data-confirm="Archive cron-drift? It leaves briefings and recall; its history stays readable here, and you can restore it."`
	pages := map[string]string{}
	for _, p := range []string{"/console/memories/" + m.ID, "/console/memories/" + m.ID + "?peek=1",
		"/console/memories/" + gone.ID, "/console/memories/" + gone.ID + "?peek=1", "/console/memories",
		"/console/notes/" + n.ID, "/console/notes/" + n.ID + "?peek=1"} {
		rr := lrGet(t, mux, p)
		require.Equal(t, http.StatusOK, rr.Code, p)
		pages[p] = rr.Body.String()
		require.NotContains(t, strings.ToLower(pages[p]), "tombstone", p)
		require.NotContains(t, pages[p], "return confirm(", p)
	}
	require.Contains(t, pages["/console/memories/"+m.ID], ask)
	require.Contains(t, pages["/console/memories/"+m.ID+"?peek=1"], ask)
	require.Contains(t, pages["/console/memories/"+gone.ID+"?peek=1"], `action="/console/memories/`+gone.ID+`/restore"`,
		"the peek trades archive for restore too")
	require.Contains(t, pages["/console/notes/"+n.ID+"?peek=1"], `action="/console/notes/`+n.ID+`/delete"`)
	for _, name := range []string{"memories", "notes", "peek_memory", "peek_note"} {
		src, err := templateFS.ReadFile("templates/" + name + ".html")
		require.NoError(t, err)
		require.NotContains(t, string(src), "confirm(", name)
	}
}

// Both readers draw the same actions in the same order: Edit in editor, then
// the one that changes the item's state, in the eyebrow; the star in the
// title.
func TestLrReadersShareTheActionOrder(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	m := writeMemory(t, mgr, core.KindGotcha, "example-ops", "disk-alarm", "disk alarm")
	n := libNote(t, mgr, "example-ops", "disk-notes", "Disk notes", time.Now().UTC(), nil)
	order := regexp.MustCompile(`(?s)<span class="reader-actions">\s*<a class="ract" href="vscode://file[^"]*"[^>]*>.*?Edit in editor</a>\s*<form method="post" action="/console/(memories|notes)/[0-9A-Z]+/(archive|delete)">.*?</span>.*?<h2 class="reader-title[^"]*">.*?<form class="star-form"`)
	for _, p := range []string{"/console/memories/" + m.ID, "/console/notes/" + n.ID} {
		require.Regexp(t, order, lrReaderSheet(t, lrGet(t, mux, p).Body.String()), p)
	}
}

// The activity line says what a restore and a delete did; notes_delete's
// event carries no title, so the delete names the slug.
func TestLrActivityNamesRestoreAndDelete(t *testing.T) {
	require.Equal(t, "restored memory dns-ttl-300", eventSummary(core.Event{Kind: core.EventMemoryWritten,
		Payload: map[string]any{"name": "dns-ttl-300", "restored": "supersede", "by": "console"}}))
	require.Equal(t, "wrote memory dns-ttl-300", eventSummary(core.Event{Kind: core.EventMemoryWritten,
		Payload: map[string]any{"name": "dns-ttl-300", "kind": "decision"}}))
	require.Equal(t, "deleted note scratch-dump", eventSummary(core.Event{Kind: core.EventNoteWritten,
		Payload: map[string]any{"slug": "scratch-dump", "deleted": true}}))
	require.Equal(t, "wrote note Scratch dump", eventSummary(core.Event{Kind: core.EventNoteWritten,
		Payload: map[string]any{"title": "Scratch dump"}}))
}
