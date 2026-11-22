package console

// UX 09: sessions you can tell apart, and that say what they did. The list
// titles a row by what the session is doing (sessionHeadlines), filters by
// project (?project=), and says what its cap left out; a session's page lists
// the tasks it moved and the notes it wrote, and says what its timeline's cap
// left out; tab titles and the palette's Recent rows are readable.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// snSession creates a session updated ago before now.
func snSession(t *testing.T, db *sql.DB, name, project string, status core.SessionStatus, findings string, ago time.Duration) core.Session {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	at := time.Now().UTC().Add(-ago)
	s := core.Session{ID: id, Name: name, ProjectSlug: project, Status: status, Findings: findings,
		CreatedAt: at.Add(-time.Minute), UpdatedAt: at}
	require.NoError(t, store.CreateSession(context.Background(), db, s))
	return s
}

// snTask creates an open task.
func snTask(t *testing.T, db *sql.DB, project, title string) core.Task {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	task := core.Task{ID: id, ProjectSlug: project, Title: title, Status: core.TaskOpen, CreatedBy: "test",
		CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task
}

// snClaim claims task for sess with an hour's lease.
func snClaim(t *testing.T, db *sql.DB, task core.Task, sess core.Session) {
	t.Helper()
	_, err := store.ClaimTask(context.Background(), db, task.ID, sess.ID, time.Hour, time.Now().UTC())
	require.NoError(t, err)
}

// snPrompt records the owner's prompt in sess, as the UserPromptSubmit hook does.
func snPrompt(t *testing.T, rec *events.Recorder, sess core.Session, prompt string) {
	t.Helper()
	_, err := rec.Record(context.Background(), core.Event{Kind: core.EventHookPrompt, SessionID: sess.ID,
		ProjectSlug: sess.ProjectSlug, Payload: map[string]any{"prompt": prompt}})
	require.NoError(t, err)
}

// snGet is an authenticated GET's status and body.
func snGet(t *testing.T, mux http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func snRow(t *testing.T, rows []sessionRow, id string) sessionRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no row for %s", id)
	return sessionRow{}
}

// Three live sessions read differently: each is titled by the task it holds,
// or by its first prompt under the Diary's rules (its first line with words,
// markdown stripped, cut on a word), and its generated name is the second
// line. A session whose agent wrote findings keeps its name: its row shows
// them. A finished session with no findings of its own is titled by its
// first prompt too.
func TestSnSessionsList_LiveRowsReadDifferently(t *testing.T) {
	db, mux := newConsole(t)
	rec := events.NewRecorder(db)
	holder := snSession(t, db, "cc/7c31a9e2", "example-ops", core.SessionActive, "", time.Minute)
	snClaim(t, db, snTask(t, db, "example-ops", "Fix the watcher race"), holder)
	busy := snSession(t, db, "cc/7c31a9e3", "example-ops", core.SessionActive, "", 2*time.Minute)
	snClaim(t, db, snTask(t, db, "example-ops", "Rotate the signing keys"), busy)
	snClaim(t, db, snTask(t, db, "example-ops", "Prune the old backups"), busy)
	snPrompt(t, rec, busy, "the claim wins over this prompt")
	asker := snSession(t, db, "cc/7c31a9e4", "example-ops", core.SessionActive, "", 3*time.Minute)
	snPrompt(t, rec, asker, "\n\n  Add a **retry** to the `uploader`\nand a test for it")
	snPrompt(t, rec, asker, "a later prompt")
	quiet := snSession(t, db, "cc/7c31a9e5", "example-ops", core.SessionActive, "", 4*time.Minute)
	wrote := snSession(t, db, "cc/7c31a9e6", "example-ops", core.SessionCompleted, "Fixed the race in the watcher.", 5*time.Minute)
	snPrompt(t, rec, wrote, "fix the race")
	harvested := snSession(t, db, "cc/7c31a9e7", "example-ops", core.SessionCompleted, "(auto-harvested) I looked at the logs.", 6*time.Minute)
	snPrompt(t, rec, harvested, "why is the nightly job slow? "+strings.Repeat("and then some more words ", 12))

	var list sessionsData
	getJSON(t, mux, "/console/sessions?format=json", &list)
	cases := []struct {
		sess          core.Session
		headline, src string
	}{
		{holder, "Fix the watcher race", headlineTask},
		{busy, "Rotate the signing keys, and 1 more task", headlineTask},
		{asker, "Add a retry to the uploader", headlinePrompt},
		{quiet, "", ""},
		{wrote, "", ""},
	}
	for _, c := range cases {
		row := snRow(t, list.Sessions, c.sess.ID)
		require.Equal(t, c.headline, row.Headline, c.sess.Name)
		require.Equal(t, c.src, row.HeadlineFrom, c.sess.Name)
	}
	long := snRow(t, list.Sessions, harvested.ID)
	require.Equal(t, headlinePrompt, long.HeadlineFrom, "harvested findings are not the agent's own: the prompt comes first")
	require.True(t, strings.HasPrefix(long.Headline, "why is the nightly job slow?"))
	require.True(t, strings.HasSuffix(long.Headline, "…"), "cut on a word, as the Diary cuts it")

	headings := map[string]bool{}
	for _, s := range []core.Session{holder, busy, asker} {
		headings[snRow(t, list.Sessions, s.ID).Heading()] = true
	}
	require.Len(t, headings, 3, "three live sessions read differently")

	code, page := snGet(t, mux, "/console/sessions")
	require.Equal(t, http.StatusOK, code)
	page = html.UnescapeString(page)
	require.Contains(t, page, `<a class="sess-headline" href="/console/sessions/`+holder.ID+`" data-peek>Fix the watcher race</a>`)
	require.Contains(t, page, `<a class="sess-headline is-prompt" href="/console/sessions/`+asker.ID+`" data-peek>Add a retry to the uploader</a>`)
	require.Contains(t, page, `<span class="mono">cc/7c31a9e2</span> · holds this task`, "the name is the second line")
	require.Contains(t, page, `<span class="mono">cc/7c31a9e4</span> · its first prompt`)
	require.Contains(t, page, `<a href="/console/sessions/`+quiet.ID+`" data-peek>cc/7c31a9e5</a>`, "no headline: the name stays the title")
	require.Equal(t, 1, strings.Count(page, "Work in progress — no findings yet"), "only the untitled live row says it")
}

// A locked project's claims and prompts are absent from the headline reads
// themselves, not only from the rows: given the locked session, the helper
// reading through the request's handle finds nothing to title it by.
func TestSnSessionHeadlines_LockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	locked, ok, err := store.SessionByID(ctx, lc.db, lc.sessID)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = store.ClaimTask(ctx, lc.db, lc.taskID, lc.sessID, time.Hour, time.Now().UTC())
	require.NoError(t, err)
	snPrompt(t, lc.rec, locked, "reconcile "+lockedSecret)
	open := snSession(t, lc.db, "cc/ops-live", openSlug, core.SessionActive, "", time.Minute)
	snPrompt(t, lc.rec, open, "check the "+openMarker)
	locked, _, err = store.SessionByID(ctx, lc.db, lc.sessID)
	require.NoError(t, err)

	base := context.WithValue(ctx, reachKey{}, &reach{db: lc.cs.cfg.DB, lockable: []string{lockedSlug}})
	heads := lc.cs.sessionHeadlines(base, []core.Session{locked, open}, time.Now())
	require.NotContains(t, heads, locked.ID, "the locked session's claim and prompt are not read")
	require.Equal(t, "check the "+openMarker, heads[open.ID].Text)

	rr := lc.do(lc.browser(http.MethodGet, "/console/sessions?format=json", "", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), lockedSecret)
	require.NotContains(t, rr.Body.String(), lc.sessID)
	require.Contains(t, rr.Body.String(), "check the "+openMarker)
	rr = lc.do(lc.browser(http.MethodGet, "/console/sessions", "", nil))
	require.NotContains(t, rr.Body.String(), lockedSecret)
}

// ?project= keeps one project's sessions ("global" the global scope's), over
// all time unless a window is chosen, and every link the page draws keeps it.
// A value that names no project this request reads is refused like every
// other strict parameter.
func TestSnSessionsList_ProjectFilter(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	for _, slug := range []string{"example-ops", "example-web", "example-quiet"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	ops := snSession(t, db, "cc/ops-old", "example-ops", core.SessionCompleted, "", 72*time.Hour)
	opsNew := snSession(t, db, "cc/ops-new", "example-ops", core.SessionCompleted, "", time.Hour)
	web := snSession(t, db, "cc/web", "example-web", core.SessionCompleted, "", time.Hour)
	global := snSession(t, db, "cc/global", "", core.SessionCompleted, "", time.Hour)
	legacy := snSession(t, db, "cc/legacy", "example-gone", core.SessionCompleted, "", time.Hour)

	var got sessionsData
	getJSON(t, mux, "/console/sessions?project=example-ops&format=json", &got)
	require.Equal(t, []string{opsNew.ID, ops.ID}, snRowIDs(got.Sessions), "all time when no window is chosen")
	require.Equal(t, "example-ops", got.Project)
	require.Equal(t, "all", got.Window)
	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?project=example-ops&w=24h&format=json", &got)
	require.Equal(t, []string{opsNew.ID}, snRowIDs(got.Sessions), "a chosen window applies")
	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?project=global&format=json", &got)
	require.Equal(t, []string{global.ID}, snRowIDs(got.Sessions))
	require.Equal(t, "global", got.Project)
	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?project=example-gone&format=json", &got)
	require.Equal(t, []string{legacy.ID}, snRowIDs(got.Sessions), "a project known by its sessions only")
	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?project=example-quiet&format=json", &got)
	require.Empty(t, got.Sessions, "a project with no sessions is an empty list, not a refusal")
	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?format=json", &got)
	require.Len(t, got.Sessions, 4, "no filter: every project (the 24h default)")
	require.NotContains(t, snRowIDs(got.Sessions), ops.ID)
	_ = web

	for value, want := range map[string]string{
		"example-nope": `invalid project "example-nope": no project has that slug`,
		"all":          `invalid project "all": name a project's slug, or global for the global scope`,
		"..%2Fops":     `invalid project "../ops": name a project's slug, or global for the global scope`,
	} {
		code, body := snGet(t, mux, "/console/sessions?format=json&project="+value)
		require.Equal(t, http.StatusBadRequest, code, value)
		var e map[string]string
		require.NoError(t, json.Unmarshal([]byte(body), &e))
		require.Equal(t, want, e["error"], value)
	}

	code, page := snGet(t, mux, "/console/sessions?project=example-ops&status=completed")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, page, `data-project-filter="example-ops"`, "the page names the filter")
	require.Contains(t, page, `<input type="hidden" name="project" value="example-ops">`, "the search keeps it")
	require.Contains(t, page, `href="/console/sessions?status=completed&amp;sort=recent&amp;q=&amp;day=&amp;retained=&amp;project=example-ops">Completed</a>`)
	require.Contains(t, page, `href="/console/sessions?status=completed&amp;sort=recent&amp;q=&amp;day=&amp;retained=" data-seam-query>Clear</a>`, "Clear drops it")
	require.NotContains(t, page, web.ID)
}

// A locked project is refused as a filter, naming its lock, and its sessions
// stay absent; an open project's filter works beside it.
func TestSnSessionsList_ProjectFilterLockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	open := snSession(t, lc.db, "cc/ops", openSlug, core.SessionCompleted, "ran it", time.Minute)
	rr := lc.do(lc.browser(http.MethodGet, "/console/sessions?format=json&project="+lockedSlug, "", nil))
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), `it is locked, and its sessions stay out of this view until this browser unlocks it`)
	require.NotContains(t, rr.Body.String(), lockedSecret)
	require.NotContains(t, rr.Body.String(), lc.sessID)

	rr = lc.do(lc.browser(http.MethodGet, "/console/sessions?format=json&project="+openSlug, "", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var got sessionsData
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, []string{open.ID}, snRowIDs(got.Sessions))
	require.Equal(t, []string{lockedSlug}, got.Locked)
}

// The capture calendar keeps the ?project= filter: a cell's click builds its
// ?day URL from the page's own (charts.js open(), which sets day and nothing
// else), and the day focus's clear link carries the project as every other
// list link does.
func TestFd_CalendarDayFocusKeepsTheProjectFilter(t *testing.T) {
	db, mux := newConsole(t)
	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Momentum: true}))
	snSession(t, db, "cc/ops", "example-ops", core.SessionCompleted, "ran it", time.Minute)
	day := time.Now().Local().Format("2006-01-02")

	code, page := snGet(t, mux, "/console/sessions?project=example-ops&day="+day)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, page, `class="mom-cal-grid"`, "the calendar renders")
	require.Contains(t, page, `data-day="`+day+`"`)
	require.Contains(t, page, `<a class="mom-cal-day-x" href="/console/sessions?status=&amp;sort=recent&amp;q=&amp;retained=&amp;project=example-ops"`,
		"clearing the day keeps the project")

	src, err := os.ReadFile("static/charts.js")
	require.NoError(t, err)
	js := string(src)
	at := strings.Index(js, "function open(cell) {")
	require.GreaterOrEqual(t, at, 0)
	end := strings.Index(js[at:], "\n  }\n")
	require.Greater(t, end, 0)
	open := js[at : at+end]
	require.Contains(t, open, "var url = new URL(location.href);", "the click starts from the page's own URL")
	require.Equal(t, 2, strings.Count(open, "url.searchParams."), "and touches day alone")
	require.Contains(t, open, "url.searchParams.delete('day')")
	require.Contains(t, open, "url.searchParams.set('day', ")
}

func snRowIDs(rows []sessionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// The list shows the newest 200 and says "newest 200 of N"; the search is the
// query's, so it finds a session past the first 200.
func TestSnSessionsList_CapSaysNewestOfNAndSearchReachesPast(t *testing.T) {
	db, mux := newConsole(t)
	oldest := snSession(t, db, "cc/oldest", "example-ops", core.SessionCompleted, "Found the kestrel race", 30*time.Hour)
	for i := range 204 {
		ago := time.Duration(i+1) * time.Minute // 100 in the last 24h, the rest a day back
		if i >= 100 {
			ago += 25 * time.Hour
		}
		snSession(t, db, fmt.Sprintf("cc/run-%03d", i), "example-ops", core.SessionCompleted, "routine", ago)
	}

	var got sessionsData
	getJSON(t, mux, "/console/sessions?w=all&format=json", &got)
	require.Len(t, got.Sessions, 200)
	require.Equal(t, 205, got.Matched)
	require.True(t, got.Capped)
	require.Equal(t, 200, got.Cap)
	require.NotContains(t, snRowIDs(got.Sessions), oldest.ID)
	code, page := snGet(t, mux, "/console/sessions?w=all")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, page, "Newest 200 of 205 runs in this view")
	require.Contains(t, page, "The newest 200 of 205 sessions in this view are listed. The search and the filters look through all 205.")

	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?w=all&q=Kestrel&format=json", &got)
	require.Equal(t, []string{oldest.ID}, snRowIDs(got.Sessions), "the search reaches past the newest 200")
	require.False(t, got.Capped)
	require.Equal(t, 1, got.Matched)

	got = sessionsData{}
	getJSON(t, mux, "/console/sessions?format=json", &got)
	require.Len(t, got.Sessions, 100, "the 24h default holds fewer than the cap")
	require.False(t, got.Capped)
	require.Equal(t, 100, got.Matched)
	_, page = snGet(t, mux, "/console/sessions")
	require.NotContains(t, page, "data-capped")
	require.Contains(t, page, "100 of 205 sessions were active in the last 24h.")
}

// A completed session lists the tasks it moved -- claimed, closed, released,
// created -- from its own task.transition events, and the notes it wrote; an
// edit that moved nothing, a task that no longer resolves and another
// session's moves are left out. The empty text follows the session's status.
func TestSnSessionDetail_ListsTheTasksItMoved(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	rec := events.NewRecorder(db)
	ctx := context.Background()
	sess := snSession(t, db, "cc/mover", "example-ops", core.SessionCompleted, "Shipped the retry.", time.Minute)
	other := snSession(t, db, "cc/other", "example-ops", core.SessionCompleted, "", time.Minute)
	done := snTask(t, db, "example-ops", "Add the retry")
	released := snTask(t, db, "example-ops", "Look at the flaky test")
	created := snTask(t, db, "example-ops", "Write the follow-up")
	edited := snTask(t, db, "example-ops", "Only edited")
	theirs := snTask(t, db, "example-ops", "Someone else's")
	note := writeNote(t, mgr, "example-ops", "retry-design", "Retry design", "the design")
	move := func(by core.Session, task string, payload map[string]any) {
		t.Helper()
		_, err := rec.Record(ctx, core.Event{Kind: core.EventTaskTransition, SessionID: by.ID, ProjectSlug: "example-ops",
			ItemID: task, Payload: payload})
		require.NoError(t, err)
	}
	move(sess, done.ID, map[string]any{"to": "in_progress", "claimed_by": sess.ID})
	move(sess, done.ID, map[string]any{"to": "in_progress", "claimed_by": sess.ID}) // a heartbeat: one "claimed"
	move(sess, done.ID, map[string]any{"to": "done", "from": "in_progress"})
	move(sess, released.ID, map[string]any{"to": "in_progress", "claimed_by": sess.ID})
	move(sess, released.ID, map[string]any{"to": "open", "released": true})
	move(sess, created.ID, map[string]any{"to": "open", "created": true})
	move(sess, edited.ID, map[string]any{"to": "open", "from": "open"})
	move(sess, "01GONEGONEGONEGONEGONEGONE", map[string]any{"to": "done", "from": "open"})
	move(other, theirs.ID, map[string]any{"to": "done", "from": "open"})
	_, err := rec.Record(ctx, core.Event{Kind: core.EventNoteWritten, SessionID: sess.ID, ProjectSlug: "example-ops", ItemID: note.ID})
	require.NoError(t, err)

	var d sessionDetail
	getJSON(t, mux, "/console/sessions/"+sess.ID+"?format=json", &d)
	moved := map[string]string{}
	for _, m := range d.TasksMoved {
		moved[m.Title] = m.MovesText()
	}
	require.Equal(t, map[string]string{
		"Add the retry": "claimed · done", "Look at the flaky test": "claimed · released", "Write the follow-up": "created",
	}, moved)
	require.Equal(t, []sessNoteVM{{ID: note.ID, Title: "Retry design", What: "note"}}, d.Notes)

	code, page := snGet(t, mux, "/console/sessions/"+sess.ID)
	require.Equal(t, http.StatusOK, code)
	page = html.UnescapeString(page)
	require.Contains(t, page, "Tasks moved <span>3</span>")
	require.Contains(t, page, `href="/console/tasks/`+done.ID+`" data-peek title="claimed · done"><span>Add the retry</span><small>claimed · done</small></a>`)
	require.Contains(t, page, `href="/console/notes/`+note.ID+`" data-peek><span>Retry design</span><small>note</small></a>`)
	require.NotContains(t, page, "Nothing durable yet", "a finished session with work never says it has none")
	require.NotContains(t, page, "01GONEGONEGONEGONEGONEGONE", "a task that no longer resolves is not linked")

	_, page = snGet(t, mux, "/console/sessions/"+other.ID)
	require.Contains(t, html.UnescapeString(page), "Someone else's", "the other session lists its own")
	empty := snSession(t, db, "cc/empty", "example-ops", core.SessionCompleted, "", time.Minute)
	_, page = snGet(t, mux, "/console/sessions/"+empty.ID)
	require.Contains(t, page, "It ended without moving a task or writing a note or memory.")
	found := snSession(t, db, "cc/found", "example-ops", core.SessionExpired, "Learned a thing.", time.Minute)
	_, page = snGet(t, mux, "/console/sessions/"+found.ID)
	require.Contains(t, page, "It moved no task and wrote no note or memory; what it found is in its findings.")
	live := snSession(t, db, "cc/live", "example-ops", core.SessionActive, "", time.Minute)
	_, page = snGet(t, mux, "/console/sessions/"+live.ID)
	require.Contains(t, page, "Nothing durable yet. Findings and memories are usually harvested when the session ends.")
}

// A session page lists only what the request's handle reads: a task moved and
// a note written in a locked project are absent, never a link that answers 404.
func TestSnSessionDetail_LockedProjectWorkIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	sess := snSession(t, lc.db, "cc/ops-crossing", openSlug, core.SessionCompleted, "crossed", time.Minute)
	mine := snTask(t, lc.db, openSlug, "the ops task")
	for _, e := range []core.Event{
		{Kind: core.EventTaskTransition, SessionID: sess.ID, ProjectSlug: lockedSlug, ItemID: lc.taskID, Payload: map[string]any{"to": "done", "from": "open"}},
		{Kind: core.EventNoteWritten, SessionID: sess.ID, ProjectSlug: lockedSlug, ItemID: lc.noteID},
		{Kind: core.EventTaskTransition, SessionID: sess.ID, ProjectSlug: openSlug, ItemID: mine.ID, Payload: map[string]any{"to": "done", "from": "open"}},
	} {
		_, err := lc.rec.Record(ctx, e)
		require.NoError(t, err)
	}
	rr := lc.do(lc.browser(http.MethodGet, "/console/sessions/"+sess.ID+"?format=json", "", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var d sessionDetail
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &d))
	require.Len(t, d.TasksMoved, 1)
	require.Equal(t, mine.ID, d.TasksMoved[0].ID)
	require.Empty(t, d.Notes)
	for _, body := range []string{rr.Body.String(), lc.do(lc.browser(http.MethodGet, "/console/sessions/"+sess.ID, "", nil)).Body.String()} {
		require.NotContains(t, body, lockedSecret)
		require.NotContains(t, body, lc.taskID)
		require.NotContains(t, body, lc.noteID)
	}
}

// A timeline past its cap says "the first 500 of N events" and links to the
// latest 500; that page says so and links back. The tasks moved come from
// every event, not only the 500 shown. Strict: any other ?timeline= is a 400.
func TestSnSessionDetail_TimelineSaysFirstOfN(t *testing.T) {
	db, mux := newConsole(t)
	rec := events.NewRecorder(db)
	ctx := context.Background()
	sess := snSession(t, db, "cc/long", "example-ops", core.SessionCompleted, "long", time.Minute)
	late := snTask(t, db, "example-ops", "Closed at the very end")
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 503 {
		e := core.Event{Kind: core.EventToolCall, SessionID: sess.ID, TS: base.Add(time.Duration(i) * time.Second),
			Payload: map[string]any{"tool": "recall"}}
		if i == 502 {
			e = core.Event{Kind: core.EventTaskTransition, SessionID: sess.ID, ItemID: late.ID, TS: e.TS,
				Payload: map[string]any{"to": "done", "from": "in_progress"}}
		}
		_, err := rec.Record(ctx, e)
		require.NoError(t, err)
	}

	var d sessionDetail
	getJSON(t, mux, "/console/sessions/"+sess.ID+"?format=json", &d)
	require.Equal(t, 503, d.Events)
	require.Len(t, d.Timeline, 500)
	require.True(t, d.TimelineCapped())
	require.False(t, d.TimelineLatest)
	require.Len(t, d.TasksMoved, 1, "a move past the first 500 is still listed")
	require.Equal(t, "Closed at the very end", d.TasksMoved[0].Title)
	_, page := snGet(t, mux, "/console/sessions/"+sess.ID)
	require.Contains(t, page, "The first 500 of 503 events.")
	require.Contains(t, page, `<a href="/console/sessions/`+sess.ID+`?timeline=latest">The latest 500</a>`)

	d = sessionDetail{}
	getJSON(t, mux, "/console/sessions/"+sess.ID+"?timeline=latest&format=json", &d)
	require.True(t, d.TimelineLatest)
	require.Len(t, d.Timeline, 500)
	require.Equal(t, string(core.EventTaskTransition), d.Timeline[len(d.Timeline)-1].Kind, "oldest first, ending at the latest")
	_, page = snGet(t, mux, "/console/sessions/"+sess.ID+"?timeline=latest")
	require.Contains(t, page, "The latest 500 of 503 events.")
	require.Contains(t, page, `<a href="/console/sessions/`+sess.ID+`">The first 500</a>`)

	code, body := snGet(t, mux, "/console/sessions/"+sess.ID+"?timeline=newest&format=json")
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, body, `invalid timeline \"newest\": valid values are latest (or omit)`)

	short := snSession(t, db, "cc/short", "example-ops", core.SessionCompleted, "short", time.Minute)
	d = sessionDetail{}
	getJSON(t, mux, "/console/sessions/"+short.ID+"?format=json", &d)
	require.False(t, d.TimelineCapped())
	_, page = snGet(t, mux, "/console/sessions/"+short.ID)
	require.NotContains(t, page, "session-timeline-cap")
}

// A session's tab title is its name and its project, not its id.
func TestSnSessionDetail_TitleNamesTheSessionAndProject(t *testing.T) {
	db, mux := newConsole(t)
	named := snSession(t, db, "cc/abcd1234", "example-ops", core.SessionCompleted, "", time.Minute)
	bare := snSession(t, db, "", "", core.SessionCompleted, "", time.Minute)
	_, page := snGet(t, mux, "/console/sessions/"+named.ID)
	require.Contains(t, page, "<title>Session cc/abcd1234 · example-ops &middot; Seamless</title>")
	_, page = snGet(t, mux, "/console/sessions/"+bare.ID)
	require.Contains(t, page, "<title>Session "+shortID(bare.ID)+" · global &middot; Seamless</title>")
}

// The palette's Recent list (shell.js): an event is not recorded, and a list
// kept from before is tidied of them on every page; a title that is only a
// kind and a short id never replaces a readable one kept for the same page.
func TestSnShellRecents_SkipEventsAndKeepTitles(t *testing.T) {
	js := string(shellJS)
	at := strings.Index(js, "/* ---- Recents for the palette")
	end := strings.Index(js, "/* ---- Clock times in this browser's zone")
	require.Greater(t, at, 0)
	require.Greater(t, end, at)
	recents := js[at:end]
	require.Contains(t, recents, `var ENTITY = /^\/console\/(memories|notes|tasks|plans|sessions|projects|labs|trials)\/[^/]+/;`)
	require.NotContains(t, recents, "|events", "an event page is not offered again")
	require.NotContains(t, recents, "events:")
	require.Contains(t, recents, `return list.filter(function (r) { return r && typeof r.href === 'string' && ENTITY.test(r.href); });`)
	require.Contains(t, recents, `if (kept && kept.title && BARE.test(title) && !BARE.test(kept.title)) title = kept.title;`)
	require.Contains(t, recents, `(function tidy() {`)
	require.Contains(t, recents, `remember(location.pathname, document.title);`)
	require.Contains(t, recents, "if (!a || e.target.closest('.copy-btn')) return;")
}

// A session's page has one <main>, the layout's: its timeline column is a
// plain block, so the page's main landmark is not nested inside itself. The
// peek fragment, drawn into another page's panel, has none.
func TestSnSessionDetail_OneMain(t *testing.T) {
	db, mux := newConsole(t)
	sess := snSession(t, db, "cc/landmark", "example-ops", core.SessionActive, "", time.Minute)
	code, page := snGet(t, mux, "/console/sessions/"+sess.ID)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, strings.Count(page, "<main"), "one main landmark")
	require.Contains(t, page, `<div class="session-primary">`)
	code, peek := snGet(t, mux, "/console/sessions/"+sess.ID+"?peek=1")
	require.Equal(t, http.StatusOK, code)
	require.Zero(t, strings.Count(peek, "<main"))
}
