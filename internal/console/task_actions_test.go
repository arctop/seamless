package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The owner's task actions (plan:console-ux-pass UX 06): Mark done, Drop,
// Reopen, Mark as handled and Release claim, in the task reader and the peek.

// tkConsole is a console whose event recorder has its milestones armed, the
// way the daemon arms it (seamlessd's rec.ArmMilestones), so the milestone
// layer -- plan.shipped among it -- runs on the console's own events.
func tkConsole(t *testing.T, feats config.Features) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	rec.ArmMilestones()
	svc, err := New(Config{
		DB: db, Events: rec, APIKey: testKey,
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Features: feats, Level: "advanced",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// tkTask creates a task in project demo; mut adjusts it first.
func tkTask(t *testing.T, db *sql.DB, title string, mut func(*core.Task)) string {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: mustID(t), ProjectSlug: "demo", Title: title, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}
	if mut != nil {
		mut(&task)
	}
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task.ID
}

// tkPost posts form to path with the bearer key, as a browser's plain form
// post (no script) or, with asJSON, as the CLI does.
func tkPost(t *testing.T, mux *http.ServeMux, path string, form url.Values, asJSON bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+testKey)
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	return do(mux, req)
}

// tkFlash reads a redirect's target: its path and query, and its one-shot
// flash (param "notice" or "error", and the message).
func tkFlash(t *testing.T, rr *httptest.ResponseRecorder) (target *url.URL, param, msg string) {
	t.Helper()
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	u, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	if m := u.Query().Get("error"); m != "" {
		return u, "error", m
	}
	return u, "notice", u.Query().Get("notice")
}

// tkTransition is one task.transition event as recorded.
type tkTransition struct {
	Session string
	Payload map[string]any
}

// tkTransitions reads a task's task.transition events, oldest first.
func tkTransitions(t *testing.T, db *sql.DB, id string) []tkTransition {
	t.Helper()
	rows, err := db.Query(`SELECT session_id, payload FROM events WHERE kind = ? AND item_id = ? ORDER BY ts, id`,
		string(core.EventTaskTransition), id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []tkTransition
	for rows.Next() {
		var ev tkTransition
		var payload string
		require.NoError(t, rows.Scan(&ev.Session, &payload))
		require.NoError(t, json.Unmarshal([]byte(payload), &ev.Payload))
		out = append(out, ev)
	}
	require.NoError(t, rows.Err())
	return out
}

// tkCount counts the events of kind.
func tkCount(t *testing.T, db *sql.DB, kind core.EventKind) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, string(kind)).Scan(&n))
	return n
}

// tkTaskByID reads a task through the main handle.
func tkTaskByID(t *testing.T, db *sql.DB, id string) core.Task {
	t.Helper()
	task, err := store.TaskByID(context.Background(), db, id)
	require.NoError(t, err)
	return task
}

// tkReadyIDs is the Tasks screen's ready rail, by id.
func tkReadyIDs(t *testing.T, mux *http.ServeMux) []string {
	t.Helper()
	var data tasksData
	getJSON(t, mux, "/console/tasks?format=json", &data)
	ids := make([]string, 0, len(data.Ready))
	for _, row := range data.Ready {
		ids = append(ids, row.ID)
	}
	return ids
}

// Mark done closes the task through the update tasks_update makes: its
// dependent goes ahead, the event says it came from the console (with the
// status before, and no session), and the owner lands back on the task with
// a notice.
func TestTkMarkDone_UnblocksADependentAndRecordsByConsole(t *testing.T) {
	db, mux := newConsole(t)
	blocker := tkTask(t, db, "tk blocker", nil)
	dependent := tkTask(t, db, "tk dependent", func(k *core.Task) { k.DependsOn = []string{blocker} })
	require.NotContains(t, tkReadyIDs(t, mux), dependent, "the dependent waits on its blocker")

	target, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+blocker+"/status", url.Values{"to": {"done"}}, false))
	require.Equal(t, "/console/tasks/"+blocker, target.Path, "the owner stays on the task")
	require.Equal(t, "notice", param)
	require.Equal(t, "Marked done.", msg)

	got := tkTaskByID(t, db, blocker)
	require.Equal(t, core.TaskDone, got.Status)
	require.NotNil(t, got.ClosedAt)
	require.Contains(t, tkReadyIDs(t, mux), dependent, "its dependent is ready now")

	evs := tkTransitions(t, db, blocker)
	require.Len(t, evs, 1)
	require.Equal(t, map[string]any{"to": "done", "from": "open", "by": "console"}, evs[0].Payload)
	require.Empty(t, evs[0].Session, "a console action attributes no session (plan-token-attribution-contract)")

	// A JSON caller gets the task.
	other := tkTask(t, db, "tk json", nil)
	rr := tkPost(t, mux, "/console/tasks/"+other+"/status", url.Values{"to": {"done"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, "done", out["status"])
	require.Equal(t, other, out["id"])
}

// Drop closes without doing, and also unblocks; Reopen opens a closed task
// again, so its dependent waits once more. A move to the status a task has
// writes nothing, a reopen of a task in progress is the release's, and a
// status the route does not take is refused.
func TestTkDropAndReopen(t *testing.T) {
	db, mux := newConsole(t)
	blocker := tkTask(t, db, "tk blocker", nil)
	dependent := tkTask(t, db, "tk dependent", func(k *core.Task) { k.DependsOn = []string{blocker} })

	_, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+blocker+"/status", url.Values{"to": {"dropped"}}, false))
	require.Equal(t, "notice", param)
	require.Equal(t, "Dropped.", msg)
	require.Equal(t, core.TaskDropped, tkTaskByID(t, db, blocker).Status)
	require.Contains(t, tkReadyIDs(t, mux), dependent, "a dropped blocker unblocks too")

	_, _, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+blocker+"/status", url.Values{"to": {"open"}}, false))
	require.Equal(t, "Reopened.", msg)
	got := tkTaskByID(t, db, blocker)
	require.Equal(t, core.TaskOpen, got.Status)
	require.Nil(t, got.ClosedAt)
	require.NotContains(t, tkReadyIDs(t, mux), dependent, "the reopened blocker holds its dependent again")

	evs := tkTransitions(t, db, blocker)
	require.Len(t, evs, 2)
	require.Equal(t, map[string]any{"to": "dropped", "from": "open", "by": "console"}, evs[0].Payload)
	require.Equal(t, map[string]any{"to": "open", "from": "dropped", "by": "console"}, evs[1].Payload)

	// Already open: a notice, and no event.
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+blocker+"/status", url.Values{"to": {"open"}}, false))
	require.Equal(t, "notice", param)
	require.Equal(t, "The task is already open.", msg)
	require.Len(t, tkTransitions(t, db, blocker), 2)

	// In progress, a reopen is the release's.
	started := tkTask(t, db, "tk started", func(k *core.Task) { k.Status = core.TaskInProgress })
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+started+"/status", url.Values{"to": {"open"}}, false))
	require.Equal(t, "error", param)
	require.Contains(t, msg, "Release claim reopens it")
	require.Equal(t, core.TaskInProgress, tkTaskByID(t, db, started).Status)

	for _, bad := range []string{"", "in_progress", "DONE", "closed"} {
		rr := tkPost(t, mux, "/console/tasks/"+blocker+"/status", url.Values{"to": {bad}}, true)
		require.Equal(t, http.StatusBadRequest, rr.Code, bad)
		require.Contains(t, rr.Body.String(), "to must be done, dropped or open", bad)
	}
	require.Equal(t, core.TaskOpen, tkTaskByID(t, db, blocker).Status)
}

// A task a live claim holds is never closed over its holder's head: the
// page names the holder and offers Release claim, not Mark done or Drop; a
// close posted anyway is refused with the holder's name (409 for JSON) and
// writes nothing. Released, the task offers its close again.
func TestTkLiveClaim_ShowsTheHolderAndNoSilentClose(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sess := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: sess, Name: "cc/tk-holder", ProjectSlug: "demo",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	id := tkTask(t, db, "tk held", nil)
	_, err := store.ClaimTask(ctx, db, id, sess, 15*time.Minute, now)
	require.NoError(t, err)

	for _, path := range []string{"/console/tasks/" + id, "/console/tasks/" + id + "?peek=1", "/console/tasks/" + id + "?reader=1"} {
		body := getHTMLBody(t, mux, path)
		held := between(t, body, "data-held-live", "<p", "</p>")
		require.Contains(t, held, `<a class="mono" href="/console/sessions/`+sess+`">cc/tk-holder</a>`, path)
		require.Contains(t, held, "holds a live claim on this task", path)
		require.Contains(t, held, "lease live, ", path)
		require.Contains(t, body, `data-task-act="release"`, path)
		require.NotContains(t, body, `data-task-act="done"`, "%s: no close over the holder's head", path)
		require.NotContains(t, body, `data-task-act="drop"`, path)
	}

	for _, to := range []string{"done", "dropped"} {
		_, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+id+"/status", url.Values{"to": {to}}, false))
		require.Equal(t, "error", param, to)
		require.Equal(t, "cc/tk-holder holds a live claim on this task: release the claim first, then close it.", msg, to)
		rr := tkPost(t, mux, "/console/tasks/"+id+"/status", url.Values{"to": {to}}, true)
		require.Equal(t, http.StatusConflict, rr.Code, to)
		require.Contains(t, rr.Body.String(), "cc/tk-holder holds a live claim", to)
	}
	got := tkTaskByID(t, db, id)
	require.Equal(t, core.TaskInProgress, got.Status, "nothing closed it")
	require.Equal(t, sess, got.ClaimedBy)
	require.Empty(t, tkTransitions(t, db, id), "a refusal records nothing")

	_, _, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+id+"/release", nil, false))
	require.Equal(t, "Released the claim.", msg)
	body := getHTMLBody(t, mux, "/console/tasks/"+id)
	require.NotContains(t, body, "data-held-live")
	require.Contains(t, body, `data-task-act="done"`)
	require.Contains(t, body, `data-task-act="drop"`)
	require.NotContains(t, body, `data-task-act="release"`, "an open task has no claim to release")
}

// Release claim is offered whenever the task is in progress, holder or not:
// a claimless start and a lapsed lease are released like a live claim, and
// with no live claim the task also offers its close. A release of a task no
// longer in progress says there is nothing to release -- a notice, a 409 for
// JSON -- never a 500.
func TestTkRelease_ClaimlessLapsedAndNotInProgress(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	claimless := tkTask(t, db, "tk claimless", func(k *core.Task) { k.Status = core.TaskInProgress })
	lapsed := tkTask(t, db, "tk lapsed", nil)
	_, err := store.ClaimTask(ctx, db, lapsed, "cc/tk-gone", time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)

	for _, id := range []string{claimless, lapsed} {
		for _, path := range []string{"/console/tasks/" + id, "/console/tasks/" + id + "?peek=1"} {
			body := getHTMLBody(t, mux, path)
			require.Contains(t, body, `data-task-act="release"`, path)
			require.Contains(t, body, `data-task-act="done"`, "%s: no live claim holds it", path)
			require.NotContains(t, body, "data-held-live", path)
		}
	}
	_, _, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+claimless+"/release", nil, false))
	require.Equal(t, "Released the claim.", msg)
	got := tkTaskByID(t, db, claimless)
	require.Equal(t, core.TaskOpen, got.Status)
	require.Empty(t, got.ClaimedBy)

	// A lapsed claim closes without a release.
	_, _, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+lapsed+"/status", url.Values{"to": {"done"}}, false))
	require.Equal(t, "Marked done.", msg)
	got = tkTaskByID(t, db, lapsed)
	require.Equal(t, core.TaskDone, got.Status)
	require.Empty(t, got.ClaimedBy, "a closed task carries no claim")

	// Nothing to release: open or done.
	for _, id := range []string{claimless, lapsed} {
		target, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+id+"/release",
			url.Values{"next": {"/console/projects/demo?tab=tasks"}}, false))
		require.Equal(t, "notice", param)
		require.Equal(t, "Nothing to release: the task is not in progress.", msg)
		require.Equal(t, "/console/projects/demo", target.Path, "the owner stays where they were")
		rr := tkPost(t, mux, "/console/tasks/"+id+"/release", nil, true)
		require.Equal(t, http.StatusConflict, rr.Code)
		require.Contains(t, rr.Body.String(), "Nothing to release")
	}
}

// Mark as handled clears an open gate with the owner's note, through the
// update tasks_update gate_clear makes: a gate closes as done and the step
// waiting on it goes ahead; a step's own gate clears and the step is ready.
// The clearance names the owner, the event says the gate was cleared from
// the console, a second press changes nothing, and a gate change the store
// refuses says why.
func TestTkMarkAsHandled_GateAndStep(t *testing.T) {
	db, mux := newConsole(t)
	gate := tkTask(t, db, "tk sign-off", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign the release"}
	})
	behind := tkTask(t, db, "tk ship it", func(k *core.Task) { k.DependsOn = []string{gate} })
	own := tkTask(t, db, "tk drill", func(k *core.Task) {
		k.Gate = core.TaskGate{Who: "owner", What: "run the drill"}
	})
	plain := tkTask(t, db, "tk plain", nil)

	// The gate offers Mark as handled, never Mark done; the step offers both.
	gatePage := getHTMLBody(t, mux, "/console/tasks/"+gate)
	form := between(t, gatePage, `data-gate-handle`, "<form", "</form>")
	require.Contains(t, form, `action="/console/tasks/`+gate+`/gate/clear"`)
	require.Contains(t, form, `name="note"`)
	require.Contains(t, form, `id="gate-note-`+gate+`"`)
	require.Contains(t, form, "data-live-skip", "a typed note survives a live refresh")
	require.Contains(t, form, "Mark as handled")
	require.NotContains(t, gatePage, `data-task-act="done"`)
	require.Contains(t, gatePage, `data-task-act="drop"`)
	ownPeek := getHTMLBody(t, mux, "/console/tasks/"+own+"?peek=1")
	require.Contains(t, ownPeek, `data-task-act="handle"`)
	require.Contains(t, ownPeek, `data-task-act="done"`)
	require.NotContains(t, getHTMLBody(t, mux, "/console/tasks/"+plain), `data-task-act="handle"`)

	_, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+gate+"/gate/clear", url.Values{"note": {"signed it"}}, false))
	require.Equal(t, "notice", param)
	require.Equal(t, "Marked as handled: the gate is done.", msg)
	got := tkTaskByID(t, db, gate)
	require.Equal(t, core.TaskDone, got.Status, "clearing a gate closes it")
	require.False(t, got.Gate.Open())
	require.Equal(t, "owner", got.Gate.ClearedBy)
	require.Equal(t, "signed it", got.Gate.Note)
	require.Contains(t, tkReadyIDs(t, mux), behind, "the step waiting on the gate goes ahead")
	evs := tkTransitions(t, db, gate)
	require.Len(t, evs, 1)
	require.Equal(t, map[string]any{"to": "done", "from": "open", "gate": "cleared", "by": "console"}, evs[0].Payload)

	// A note typed across lines reads as one line.
	_, _, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+own+"/gate/clear", url.Values{"note": {"  ran it\r\n on  Tuesday "}}, false))
	require.Equal(t, "Marked as handled: the step's gate is cleared.", msg)
	got = tkTaskByID(t, db, own)
	require.Equal(t, core.TaskOpen, got.Status, "a step's gate clears and the step stays open")
	require.Equal(t, "ran it on Tuesday", got.Gate.Note)
	require.Equal(t, "owner", got.Gate.ClearedBy)
	require.Contains(t, tkReadyIDs(t, mux), own, "the waiting step is ready")
	require.Equal(t, map[string]any{"to": "open", "from": "open", "gate": "cleared", "by": "console"},
		tkTransitions(t, db, own)[0].Payload)
	cleared := getHTMLBody(t, mux, "/console/tasks/"+own)
	require.Contains(t, cleared, `data-gate="cleared"`)
	require.Contains(t, cleared, `by <span class="mono">owner</span>`)
	require.NotContains(t, cleared, `data-task-act="handle"`)

	// Again: already handled, nothing written.
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+own+"/gate/clear", nil, false))
	require.Equal(t, "notice", param)
	require.Equal(t, "The gate is already handled.", msg)
	require.Len(t, tkTransitions(t, db, own), 1)

	// No gate at all.
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+plain+"/gate/clear", nil, false))
	require.Equal(t, "error", param)
	require.Equal(t, "The task has no gate to mark as handled.", msg)
	require.Equal(t, http.StatusConflict, tkPost(t, mux, "/console/tasks/"+plain+"/gate/clear", nil, true).Code)

	// A note past the store's limit: the store's reason, and the gate stays open.
	long := tkTask(t, db, "tk long note", func(k *core.Task) { k.Gate = core.TaskGate{Who: "owner", What: "decide"} })
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+long+"/gate/clear", url.Values{"note": {strings.Repeat("n", 2001)}}, false))
	require.Equal(t, "error", param)
	require.Equal(t, "The gate was not marked as handled: gate_note is 2001 characters; at most 2000.", msg)
	require.True(t, tkTaskByID(t, db, long).Gate.Open())
	require.Empty(t, tkTransitions(t, db, long))
}

// Clearing a gate records the owner as who cleared it, so it is the owner's
// own hands: an agent's process on the peer_auth socket and a paired device
// are refused, and the gate stays open; the owner's callers clear it.
func TestTkMarkAsHandled_IsTheOwnersHands(t *testing.T) {
	for _, c := range uohCallers(t) {
		t.Run(c.name, func(t *testing.T) {
			db, mux := newConsole(t)
			id := tkTask(t, db, "tk gate", func(k *core.Task) {
				k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "approve"}
			})
			rr := uohPost(t, mux, c, "/console/tasks/"+id+"/gate/clear", "note=ok")
			if !c.owner {
				uohRequireRefused(t, rr, c, "gate clear")
				require.True(t, tkTaskByID(t, db, id).Gate.Open(), "the gate stays open")
				require.Empty(t, tkTransitions(t, db, id))
				return
			}
			_, param, msg := tkFlash(t, rr)
			require.Equal(t, "notice", param, msg)
			require.Equal(t, "owner", tkTaskByID(t, db, id).Gate.ClearedBy)
		})
	}
}

// A console close of a plan's last step mints plan.shipped exactly as the
// MCP path's close does: the recorder mints it from the task.transition
// event the console records (store.CheckMilestones), once per
// plan whatever closes it again. Mark as handled closing a plan's last step,
// a gate, settles its plan too.
func TestTkPlanShipped_ConsoleClosesThePlansLastStep(t *testing.T) {
	db, mux := tkConsole(t, config.Features{})
	const plan = "tk-plan"
	first := tkTask(t, db, "tk step one", func(k *core.Task) { k.PlanSlug = plan })
	last := tkTask(t, db, "tk step two", func(k *core.Task) { k.PlanSlug = plan; k.DependsOn = []string{first} })
	shipped := func() []core.Event {
		t.Helper()
		rows, err := db.Query(`SELECT session_id, project_slug, item_id FROM events WHERE kind = ?`, string(core.EventPlanShipped))
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []core.Event
		for rows.Next() {
			var e core.Event
			require.NoError(t, rows.Scan(&e.SessionID, &e.ProjectSlug, &e.ItemID))
			out = append(out, e)
		}
		require.NoError(t, rows.Err())
		return out
	}

	tkFlash(t, tkPost(t, mux, "/console/tasks/"+first+"/status", url.Values{"to": {"done"}}, false))
	require.Empty(t, shipped(), "a step remains open")
	tkFlash(t, tkPost(t, mux, "/console/tasks/"+last+"/status", url.Values{"to": {"done"}}, false))
	got := shipped()
	require.Len(t, got, 1, "the last step closed: the plan shipped")
	require.Equal(t, core.Event{ProjectSlug: "demo", ItemID: plan}, got[0], "no session: the console's close")

	// Reopened and closed again, the plan shipped once.
	tkFlash(t, tkPost(t, mux, "/console/tasks/"+last+"/status", url.Values{"to": {"open"}}, false))
	tkFlash(t, tkPost(t, mux, "/console/tasks/"+last+"/status", url.Values{"to": {"done"}}, false))
	require.Len(t, shipped(), 1, "once per plan")

	// A plan whose last step is a gate ships when the owner marks it handled.
	const gated = "tk-gated-plan"
	step := tkTask(t, db, "tk gated step", func(k *core.Task) { k.PlanSlug = gated })
	signoff := tkTask(t, db, "tk gated sign-off", func(k *core.Task) {
		k.PlanSlug, k.Kind, k.Gate = gated, core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign off"}
	})
	tkFlash(t, tkPost(t, mux, "/console/tasks/"+step+"/status", url.Values{"to": {"dropped"}}, false))
	require.Len(t, shipped(), 1, "a drop is no ship, and the gate is open")
	tkFlash(t, tkPost(t, mux, "/console/tasks/"+signoff+"/gate/clear", url.Values{"note": {"signed"}}, false))
	got = shipped()
	require.Len(t, got, 2)
	require.Equal(t, gated, got[1].ItemID)
	require.Equal(t, 2, tkCount(t, db, core.EventPlanShipped))
}

// A locked project's task is not found to every action, with no title: the
// action reads through the request's handle (console-reads-through-the-
// request-handle), and nothing is written.
func TestTkActions_LockedProjectTaskIsNotFound(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	who, what := "owner", "close the books"
	_, err := store.UpdateTask(ctx, lc.db, lc.taskID, store.TaskPatch{Gate: &store.TaskGatePatch{Who: &who, What: &what}},
		"", time.Now().UTC())
	require.NoError(t, err)
	before, err := store.TaskByID(ctx, lc.db, lc.taskID)
	require.NoError(t, err)
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/console/tasks/" + lc.taskID + "/status", url.Values{"to": {"done"}}},
		{"/console/tasks/" + lc.taskID + "/status", url.Values{"to": {"bogus"}}},
		{"/console/tasks/" + lc.taskID + "/gate/clear", url.Values{"note": {"x"}}},
		{"/console/tasks/" + lc.taskID + "/release", nil},
	} {
		for _, asJSON := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", "Bearer "+testKey)
			if asJSON {
				req.Header.Set("Accept", "application/json")
			}
			rr := lc.do(req)
			require.Equal(t, http.StatusNotFound, rr.Code, "%s json=%v: %s", tc.path, asJSON, rr.Body.String())
			require.NotContains(t, rr.Body.String(), lockedSecret, tc.path)
			require.NotContains(t, rr.Body.String(), "reconcile", "%s: no title", tc.path)
		}
	}
	after, err := store.TaskByID(ctx, lc.db, lc.taskID)
	require.NoError(t, err)
	require.Equal(t, before, after, "nothing changed")

	// The project the owner reads keeps its actions.
	id := mustID(t)
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: id, ProjectSlug: openSlug, Title: "ops step",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	req := httptest.NewRequest(http.MethodPost, "/console/tasks/"+id+"/status", strings.NewReader("to=done"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := lc.do(req)
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	done, err := store.TaskByID(ctx, lc.db, id)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, done.Status)
}

// The actions ride the owner's surfaces' contracts: a cookie post must come
// from the console's own origin (console-csrf-origin-check-contract), and the
// in-place client's post is answered 204 with where to go, the notice kept
// (console-mutation-follow-contract).
func TestTkActions_OriginCheckAndFollow(t *testing.T) {
	db, mux := newConsole(t)
	id := tkTask(t, db, "tk followed", nil)

	cross := httptest.NewRequest(http.MethodPost, "http://localhost:8081/console/tasks/"+id+"/status", strings.NewReader("to=done"))
	cross.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cross.Header.Set("Sec-Fetch-Site", "same-site")
	cross.AddCookie(consoleCookie())
	require.Equal(t, http.StatusForbidden, do(mux, cross).Code)
	require.Equal(t, core.TaskOpen, tkTaskByID(t, db, id).Status)

	req := httptest.NewRequest(http.MethodPost, "http://localhost:8081/console/tasks/"+id+"/status",
		strings.NewReader("to=done&next="+url.QueryEscape("/console/projects/demo?tab=tasks")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Seam-Follow", "1")
	req.AddCookie(consoleCookie())
	rr := do(mux, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.Equal(t, "/console/projects/demo?tab=tasks&notice=Marked+done.", rr.Header().Get("X-Seam-Location"))
	require.Equal(t, core.TaskDone, tkTaskByID(t, db, id).Status)
}

// The peek is also the project tab's step dossier: every action it offers
// posts back to the surface that opened it, and lands there.
func TestTkPeek_ActionsCarryTheCallersNext(t *testing.T) {
	db, mux := newConsole(t)
	next := "/console/projects/demo?tab=tasks"
	gate := tkTask(t, db, "tk gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "decide"}
	})
	closed := tkTask(t, db, "tk closed", func(k *core.Task) {
		now := time.Now().UTC()
		k.Status, k.ClosedAt = core.TaskDone, &now
	})
	started := tkTask(t, db, "tk started", func(k *core.Task) { k.Status = core.TaskInProgress })
	for id, acts := range map[string][]string{gate: {"handle", "drop"}, closed: {"reopen"}, started: {"done", "drop", "release"}} {
		body := getHTMLBody(t, mux, "/console/tasks/"+id+"?peek=1&next="+url.QueryEscape(next))
		for _, act := range acts {
			at := strings.Index(body, `data-task-act="`+act+`"`)
			require.GreaterOrEqual(t, at, 0, "%s offers %s", id, act)
			form := body[strings.LastIndex(body[:at], "<form"):at]
			require.Contains(t, form, `name="next" value="`+next+`"`, "%s's %s returns to the dossier's surface", id, act)
		}
		// Without a next, the forms carry none and land on the task.
		require.NotContains(t, between(t, getHTMLBody(t, mux, "/console/tasks/"+id+"?peek=1"), "data-task-actions", "<div", "</div>"),
			`name="next"`)
	}
	target, _, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+closed+"/status", url.Values{"to": {"open"}, "next": {next}}, false))
	require.Equal(t, "Reopened.", msg)
	require.Equal(t, "/console/projects/demo", target.Path)
	require.Equal(t, "tasks", target.Query().Get("tab"))
}

// Drop and Release claim confirm in the page (data-confirm, shell.js), and no
// native confirm() is left in the task templates.
func TestTkTemplates_ConfirmInThePage(t *testing.T) {
	for _, name := range []string{"tasks", "peek_task", "task_shared"} {
		b, err := fs.ReadFile(templateFS, "templates/"+name+".html")
		require.NoError(t, err)
		require.NotContains(t, string(b), "confirm(", "%s.html asks through a native confirm()", name)
		require.NotContains(t, string(b), "onsubmit", name)
	}
	db, mux := newConsole(t)
	id := tkTask(t, db, "tk confirmed", func(k *core.Task) { k.Status = core.TaskInProgress })
	for _, path := range []string{"/console/tasks/" + id, "/console/tasks/" + id + "?peek=1"} {
		body := getHTMLBody(t, mux, path)
		for act, sentence := range map[string]string{
			"drop":    `data-confirm="Drop this task? It closes without being done, and the tasks waiting on it go ahead." data-confirm-label="Drop it"`,
			"release": `data-confirm="Release the claim on this task? It reopens for any agent to claim." data-confirm-label="Release it"`,
		} {
			at := strings.Index(body, `data-task-act="`+act+`"`)
			require.GreaterOrEqual(t, at, 0, path)
			button := body[at : at+strings.Index(body[at:], ">")]
			require.Contains(t, button, sentence, "%s: %s asks in the page", path, act)
		}
		done := strings.Index(body, `data-task-act="done"`)
		require.NotContains(t, body[done:done+strings.Index(body[done:], ">")], "data-confirm", "Mark done is undone by Reopen: no confirm")
	}
}

// A task's page is titled by the task, so two tabs tell their tasks apart.
func TestTkTaskPage_TabTitleIsTheTasks(t *testing.T) {
	db, mux := newConsole(t)
	id := tkTask(t, db, "tk renew the certificate", nil)
	require.Contains(t, getHTMLBody(t, mux, "/console/tasks/"+id), "<title>tk renew the certificate &middot; Seamless</title>")
	require.Equal(t, "Task "+shortID("01KTKTKTKTKTKTKTKTKTKTKTKT"), taskPageTitle(core.Task{ID: "01KTKTKTKTKTKTKTKTKTKTKTKT", Title: "  "}))
}

// taskNext lands an action back on the surface it came from, with its flash
// before any fragment, never off the console, and never with a stale flash.
func TestTkTaskNext(t *testing.T) {
	const id = "01KTKTKTKTKTKTKTKTKTKTKTKT"
	for _, tc := range []struct{ raw, want string }{
		{"", "/console/tasks/" + id + "?notice=Done."},
		{"  ", "/console/tasks/" + id + "?notice=Done."},
		{"/console/projects/demo?tab=tasks", "/console/projects/demo?tab=tasks&notice=Done."},
		{"/console/projects/demo?tab=tasks#step-1", "/console/projects/demo?tab=tasks&notice=Done.#step-1"},
		{"/console/tasks?notice=Old&error=Older&x=1", "/console/tasks?x=1&notice=Done."},
		{"/console/tasks?", "/console/tasks?notice=Done."},
		{"https://evil.example/steal", "/console/tasks/" + id + "?notice=Done."},
		{"//evil.example/steal", "/console/tasks/" + id + "?notice=Done."},
		{"/console//evil.example", "/console/tasks/" + id + "?notice=Done."},
	} {
		require.Equal(t, tc.want, taskNext(tc.raw, id, "notice", "Done."), tc.raw)
	}
	require.Equal(t, "/console/tasks/"+id+"?error=No+way.", taskNext("", id, "error", "No way."))
}

// actionsFor: what each kind of task offers.
func TestTkActionsFor(t *testing.T) {
	gate := core.TaskGate{Who: "owner", What: "decide"}
	at := time.Now().UTC()
	cleared := core.TaskGate{Who: "owner", What: "decide", ClearedAt: &at}
	for _, tc := range []struct {
		name string
		task core.Task
		live bool
		want taskActions
	}{
		{"open", core.Task{Status: core.TaskOpen}, false, taskActions{Done: true, Drop: true, WaitOn: true}},
		{"claimless start", core.Task{Status: core.TaskInProgress}, false, taskActions{Release: true, Done: true, Drop: true, WaitOn: true}},
		{"lapsed claim", core.Task{Status: core.TaskInProgress, ClaimedBy: "s"}, false, taskActions{Release: true, Done: true, Drop: true, WaitOn: true}},
		{"live claim", core.Task{Status: core.TaskInProgress, ClaimedBy: "s"}, true, taskActions{Release: true, Held: true}},
		{"done", core.Task{Status: core.TaskDone}, false, taskActions{Reopen: true}},
		{"dropped", core.Task{Status: core.TaskDropped}, false, taskActions{Reopen: true}},
		{"open gate", core.Task{Status: core.TaskOpen, Kind: core.TaskKindGate, Gate: gate}, false, taskActions{Handle: true, Drop: true}},
		{"a step's open gate", core.Task{Status: core.TaskOpen, Gate: gate}, false, taskActions{Handle: true, Done: true, Drop: true}},
		{"a step's cleared gate", core.Task{Status: core.TaskOpen, Gate: cleared}, false, taskActions{Done: true, Drop: true, WaitOn: true}},
		{"a gate held live", core.Task{Status: core.TaskInProgress, Gate: gate, ClaimedBy: "s"}, true, taskActions{Release: true, Held: true}},
		{"a closed gate", core.Task{Status: core.TaskDone, Kind: core.TaskKindGate, Gate: cleared}, false, taskActions{Reopen: true}},
	} {
		require.Equal(t, tc.want, actionsFor(tc.task, tc.live), tc.name)
	}
}
