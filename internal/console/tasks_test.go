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
	"github.com/arctop/seamless/internal/store"
)

func TestTasksPage_ReadyBlockedClosed(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(title string, deps ...string) core.Task {
		id, err := core.NewID()
		require.NoError(t, err)
		task := core.Task{
			ID: id, ProjectSlug: "seamless", Title: title, Status: core.TaskOpen,
			DependsOn: deps, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, store.CreateTask(ctx, db, task))
		return task
	}

	a := mk("build the widget") // ready
	mk("ship the widget", a.ID) // blocked by a
	c := mk("write docs")       // will be marked done
	_, err := store.UpdateTask(ctx, db, c.ID, store.TaskPatch{Status: ptrStatus(core.TaskDone)}, "", now)
	require.NoError(t, err)

	var data tasksData
	getJSON(t, mux, "/console/tasks?format=json", &data)

	require.Len(t, data.Ready, 1)
	require.Equal(t, "build the widget", data.Ready[0].Title)
	require.Len(t, data.Blocked, 1)
	require.Equal(t, "ship the widget", data.Blocked[0].Title)
	require.Len(t, data.Blocked[0].Blockers, 1)
	require.Equal(t, "build the widget", data.Blocked[0].Blockers[0].Title)
	require.Len(t, data.Closed, 1)
	require.Equal(t, "write docs", data.Closed[0].Title)
	require.Equal(t, "done", data.Closed[0].Status)

	// HTML renders.
	req := httptest.NewRequest(http.MethodGet, "/console/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "ship the widget")
}

func TestTasksPage_RailGroupsSortOldestOrNewest(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)

	create := func(title string, status core.TaskStatus, created, updated time.Time, closed *time.Time, deps ...string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "seamless", Title: title, Status: status,
			DependsOn: deps, CreatedAt: created, UpdatedAt: updated, ClosedAt: closed,
		}))
		return id
	}

	// Give every older row a star. Stars remain visible metadata, but never
	// move a row in either sort.
	oldInProgress := create("old in progress", core.TaskInProgress, base, base.Add(8*time.Hour), nil)
	create("new in progress", core.TaskInProgress, base.Add(time.Hour), base.Add(time.Hour), nil)
	oldReady := create("old ready", core.TaskOpen, base, base, nil)
	create("new ready", core.TaskOpen, base.Add(time.Hour), base.Add(time.Hour), nil)
	oldBlocked := create("old blocked", core.TaskOpen, base, base, nil, oldInProgress)
	create("new blocked", core.TaskOpen, base.Add(time.Hour), base.Add(time.Hour), nil, oldInProgress)
	closedOld, closedNew := base.Add(2*time.Hour), base.Add(3*time.Hour)
	oldClosed := create("old closed", core.TaskDone, base, closedOld, &closedOld)
	create("new closed", core.TaskDone, base.Add(time.Hour), closedNew, &closedNew)
	for _, id := range []string{oldInProgress, oldReady, oldBlocked, oldClosed} {
		require.NoError(t, store.SetTaskFavorite(ctx, db, id, true))
	}

	titles := func(rows []taskRow) []string {
		out := make([]string, len(rows))
		for i := range rows {
			out[i] = rows[i].Title
		}
		return out
	}
	// The default is the order agents claim in: oldest first.
	var data tasksData
	getJSON(t, mux, "/console/tasks?format=json", &data)
	require.Equal(t, "oldest", data.Filter.Sort)
	require.Equal(t, []string{"old in progress", "new in progress"}, titles(data.InProgress))
	require.Equal(t, []string{"old ready", "new ready"}, titles(data.Ready))
	require.Equal(t, []string{"old blocked", "new blocked"}, titles(data.Blocked))
	require.Equal(t, []string{"new closed", "old closed"}, titles(data.Closed), "closed is newest first in either sort")
	require.True(t, data.Ready[0].Favorite)

	getJSON(t, mux, "/console/tasks?format=json&sort=newest", &data)
	require.Equal(t, []string{"new in progress", "old in progress"}, titles(data.InProgress))
	require.Equal(t, []string{"new ready", "old ready"}, titles(data.Ready))
	require.Equal(t, []string{"new blocked", "old blocked"}, titles(data.Blocked))
	require.Equal(t, []string{"new closed", "old closed"}, titles(data.Closed))
	require.True(t, data.Ready[1].Favorite, "the older star remains visible without changing recency order")
}

func ptrStatus(s core.TaskStatus) *core.TaskStatus { return &s }

// TestTaskRelease_OwnerOverride covers the console Release claim button: the peek
// surfaces the holder and offers the button, and POSTing the release reopens the
// task (clearing the claim) regardless of who held it.
func TestTaskRelease_OwnerOverride(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: id, ProjectSlug: "seamless", Title: "held task", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	_, err = store.ClaimTask(ctx, db, id, "cc/agent-x", time.Minute, now)
	require.NoError(t, err)

	// The peek surfaces the holder and the Release claim button.
	req := httptest.NewRequest(http.MethodGet, "/console/tasks/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "cc/agent-x")
	require.Contains(t, rr.Body.String(), "Release claim")
	require.NotContains(t, rr.Body.String(), rwOldRelease, "a task's claim is a claim, never a lock")

	// POST the release: a browser gets a redirect back to the task.
	rr = do(mux, postAuthed("/console/tasks/"+id+"/release"))
	require.Equal(t, http.StatusSeeOther, rr.Code)

	// The claim is cleared and the task is open again.
	got, err := store.TaskByID(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, got.Status)
	require.Empty(t, got.ClaimedBy)
	require.Nil(t, got.LeaseExpiresAt)

	// The button is gone now that nothing holds the task.
	req = httptest.NewRequest(http.MethodGet, "/console/tasks/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr = do(mux, req)
	require.NotContains(t, rr.Body.String(), "Release claim")

	// A JSON caller (the seam CLI --force path) gets 200 with the reopened task;
	// releasing an unclaimed task is an error, not a silent success.
	_, err = store.ClaimTask(ctx, db, id, "cc/agent-y", time.Minute, time.Now().UTC())
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, "/console/tasks/"+id+"/release", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "application/json")
	rr = do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"status": "open"`)
}

// postAuthed builds an authenticated POST request (no body) for a console action.
func postAuthed(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	return req
}

// The owner's release from the console tells the session that held the claim:
// one notice waits for its next hook injection or tool result.
func TestTaskRelease_TellsTheHolder(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: "seamless", Title: "held task",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	_, err = store.ClaimTask(ctx, db, id, "01CLHOLDERAAAAAAAAAAAAAAAA", time.Hour, now)
	require.NoError(t, err)

	rr := do(mux, postAuthed("/console/tasks/"+id+"/release"))
	require.Equal(t, http.StatusSeeOther, rr.Code)

	pending, err := store.PendingClaimNotices(ctx, db, "01CLHOLDERAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, store.ClaimLostReleased, pending[0].Reason)
	require.Equal(t, id, pending[0].ResourceID)
}

// A session idle past the threshold while it holds a live claim is live on the
// console's session surfaces: the Sessions screen, its own page, and the Now
// page, where its claim rides its card rather than the loose ends. Once the
// lease lapses it reads idle again.
func TestConsole_AQuietHolderOfALiveClaimIsLive(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	idle := now.Add(-2 * time.Hour)
	mk := func(name string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: "seamless",
			Status: core.SessionActive, CreatedAt: idle, UpdatedAt: idle}))
		return id
	}
	holder, quiet := mk("cc/gates"), mk("cc/quiet")
	task, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: task, ProjectSlug: "seamless", Title: "run the gates",
		Status: core.TaskOpen, CreatedAt: idle, UpdatedAt: idle}))
	_, err = store.ClaimTask(ctx, db, task, holder, time.Hour, now)
	require.NoError(t, err)

	liveOf := func() (active, idleN int, live map[string]bool) {
		t.Helper()
		var list sessionsData
		getJSON(t, mux, "/console/sessions?format=json", &list)
		live = map[string]bool{}
		for _, r := range list.Sessions {
			live[r.ID] = r.Live
		}
		return list.Active, list.Idle, live
	}
	active, idleN, live := liveOf()
	require.Equal(t, 1, active)
	require.Equal(t, 1, idleN)
	require.True(t, live[holder], "its live claim keeps the quiet holder live")
	require.False(t, live[quiet])

	var detail struct {
		Live bool `json:"live"`
	}
	getJSON(t, mux, "/console/sessions/"+holder+"?format=json", &detail)
	require.True(t, detail.Live)

	var page nowData
	getJSON(t, mux, "/console/now?format=json", &page)
	require.Equal(t, 1, page.AgentsLive)
	require.Equal(t, holder, page.Agents[0].ID)
	require.Len(t, page.Agents[0].Claims, 1)
	require.Equal(t, task, page.Agents[0].Claims[0].ID)
	require.Empty(t, page.Loose, "a live holder's claim is no loose end")

	_, err = db.ExecContext(ctx, `UPDATE tasks SET lease_expires_at = ? WHERE id = ?`,
		core.FormatTime(now.Add(-time.Minute)), task)
	require.NoError(t, err)
	active, idleN, live = liveOf()
	require.Zero(t, active, "the lease lapsed: nothing keeps it live")
	require.Equal(t, 2, idleN)
	require.False(t, live[holder])
}

// The task-claim action's old names, which no page may print: a claim is a
// claim, never a lock (the console's locks are repo locks and locked
// projects). Spelled in two halves so a grep of the tree for the old names
// finds no page, doc or test that still uses them.
const (
	rwOldRelease = "Release " + "lock"
	rwOldConfirm = "Release the " + "lock"
)

// rwFleet is one task in every state the console's words name, in project
// demo, under plan ("" for tasks outside a plan): ready, blocked (by blocker,
// itself ready), a gate and a step behind it, a live claim, a lapsed one, a
// claimless start, done and dropped. sess is the holder, created_by names it.
type rwFleet struct {
	sess, sessName                                                string
	ready, blocker, blocked, gate, gated, held, lapsed, claimless string
	done, dropped                                                 string
	all                                                           []string
}

func rwSeedFleet(t *testing.T, db *sql.DB, plan string) rwFleet {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	f := rwFleet{sess: mustID(t), sessName: "cc/rw-holder"}
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: f.sess, Name: f.sessName, ProjectSlug: "demo", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	n := 0
	mk := func(title string, mut func(*core.Task)) string {
		n++
		task := core.Task{ID: mustID(t), ProjectSlug: "demo", Title: title, Status: core.TaskOpen, PlanSlug: plan,
			CreatedBy: f.sessName, CreatedAt: now.Add(time.Duration(n) * time.Second), UpdatedAt: now}
		if mut != nil {
			mut(&task)
		}
		require.NoError(t, store.CreateTask(ctx, db, task))
		f.all = append(f.all, task.ID)
		return task.ID
	}
	f.ready = mk("rw ready", nil)
	f.blocker = mk("rw blocker", nil)
	f.blocked = mk("rw blocked", func(k *core.Task) { k.DependsOn = []string{f.blocker} })
	f.gate = mk("rw gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign off"}
	})
	f.gated = mk("rw gated", func(k *core.Task) { k.DependsOn = []string{f.gate} })
	f.held = mk("rw held", nil)
	f.lapsed = mk("rw lapsed", nil)
	f.claimless = mk("rw claimless", func(k *core.Task) { k.Status = core.TaskInProgress })
	f.done = mk("rw done", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &now })
	f.dropped = mk("rw dropped", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDropped, &now })
	_, err := store.ClaimTask(ctx, db, f.held, f.sess, 15*time.Minute, now)
	require.NoError(t, err)
	_, err = store.ClaimTask(ctx, db, f.lapsed, f.sess, time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)
	return f
}

// TestRwTasksPage_EveryRowCopiesItsWholeID: every row of the Tasks list, the
// closed ones included, shows its short id as a button that copies the
// 26-character id, and the in progress rows name their holder.
func TestRwTasksPage_EveryRowCopiesItsWholeID(t *testing.T) {
	db, mux := newConsole(t)
	f := rwSeedFleet(t, db, "")
	page := getHTMLBody(t, mux, "/console/tasks")
	for _, id := range f.all {
		require.Len(t, id, 26)
		require.Contains(t, tlRow(t, page, id),
			`<button type="button" class="copy-btn tl-id" data-copy="`+id+`" title="Copies the whole id" aria-label="Copy the whole id"><span class="mono">`+shortID(id)+`</span></button>`,
			"row %s copies its whole id from its short one", id)
	}
	held := tlRow(t, page, f.held)
	require.Contains(t, held, `data-mark="live"`)
	require.Contains(t, held, f.sessName, "the holder by name")
	require.Contains(t, tlRow(t, page, f.lapsed), `data-word="claim-lapsed"`)
	require.Contains(t, tlRow(t, page, f.claimless), `data-word="unclaimed"`)
	require.Contains(t, tlRow(t, page, f.done), `>done `)
	require.NotContains(t, page, ">in_progress<")
	require.Contains(t, page, `data-clock="title"`, "row times carry their instant for a local-time title")
}

// TestRwTaskReader_WordsHolderLease covers the reader and the peek: the
// task's word, its holder and creator by name linked to their session, the
// lease in words (never "expires now"), and Release claim, never a lock.
func TestRwTaskReader_WordsHolderLease(t *testing.T) {
	db, mux := newConsole(t)
	f := rwSeedFleet(t, db, "")
	sessLink := `<a class="mono" href="/console/sessions/` + f.sess + `">` + f.sessName + `</a>`

	for _, path := range []string{"/console/tasks/" + f.held, "/console/tasks/" + f.held + "?peek=1"} {
		body := getHTMLBody(t, mux, path)
		require.Contains(t, body, `data-word="in-progress"`, path)
		require.Contains(t, body, sessLink, "%s: the holder links to its session", path)
		require.Contains(t, body, "lease live, ", path)
		require.NotContains(t, body, "expires now", path)
		require.NotContains(t, body, "expires ", path)
		require.Regexp(t, `<time datetime="[^"]+" data-clock="title" title="[^"]+ UTC">lease live, 1[45]m left</time>`, body, path)
		require.Contains(t, strings.ToLower(body), "release claim", path)
		require.NotContains(t, strings.ToLower(body), strings.ToLower(rwOldRelease), path)
		require.NotContains(t, body, rwOldConfirm, path)
		require.Contains(t, body, "Release the claim on this task?", path)
	}
	reader := getHTMLBody(t, mux, "/console/tasks/"+f.held)
	require.Contains(t, reader, `created by `+sessLink, "the creator links to its session")

	lapsed := getHTMLBody(t, mux, "/console/tasks/"+f.lapsed+"?peek=1")
	require.Contains(t, lapsed, `data-word="claim-lapsed"`)
	require.Regexp(t, `class="task-lease is-lapsed"><time [^>]+>lapsed (59m|1h) ago</time>`, lapsed)

	for id, word := range map[string]string{
		f.ready: "ready", f.blocked: "blocked", f.gate: "waiting", f.gated: "waiting",
		f.claimless: "unclaimed", f.done: "done", f.dropped: "dropped",
	} {
		body := getHTMLBody(t, mux, "/console/tasks/"+id+"?peek=1")
		require.Contains(t, body, `<span class="badge`, id)
		require.Contains(t, body, `data-word="`+word+`"`, "task %s reads %s", id, word)
	}
	gated := getHTMLBody(t, mux, "/console/tasks/"+f.gated+"?peek=1")
	require.Contains(t, gated, ">waiting on owner</span>")
	// The dependency list words each blocker and copies its id.
	blocked := getHTMLBody(t, mux, "/console/tasks/"+f.blocked+"?peek=1")
	deps := blocked[strings.Index(blocked, "Depends on"):]
	require.Contains(t, deps, `data-word="ready"`, "the blocker is itself ready")
	require.Contains(t, deps, `data-copy="`+f.blocker+`"`)
	blocker := getHTMLBody(t, mux, "/console/tasks/"+f.blocker+"?peek=1")
	blocks := blocker[strings.Index(blocker, "Blocks"):]
	require.Contains(t, blocks, `data-word="blocked"`, "what it blocks reads blocked")

	// JSON callers get the word, the resolved holder and the lease line.
	var d taskDetail
	getJSON(t, mux, "/console/tasks/"+f.held+"?format=json", &d)
	require.Equal(t, wordInProgress, d.Word.Key)
	require.Equal(t, &taskSession{ID: f.sess, Name: f.sessName}, d.Holder)
	require.Equal(t, &taskSession{ID: f.sess, Name: f.sessName}, d.Creator)
	require.True(t, strings.HasPrefix(d.Lease, "lease live, "), d.Lease)
}

// TestRwTaskReader_UnresolvedSessionsAreNotLinked: a holder or creator that
// names no session the console reads keeps its text and gets no link, so the
// page never links to a session it cannot open; "console" is a creator too.
func TestRwTaskReader_UnresolvedSessionsAreNotLinked(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: "demo", Title: "orphan claim",
		Status: core.TaskOpen, CreatedBy: "console", CreatedAt: now, UpdatedAt: now}))
	_, err := store.ClaimTask(ctx, db, id, "cc/agent-gone", time.Minute, now)
	require.NoError(t, err)
	body := getHTMLBody(t, mux, "/console/tasks/"+id+"?peek=1")
	require.Contains(t, body, `<span class="mono">cc/agent-gone</span>`)
	require.Contains(t, body, `<span class="mono">console</span>`)
	require.NotContains(t, body, `href="/console/sessions/`)
}
