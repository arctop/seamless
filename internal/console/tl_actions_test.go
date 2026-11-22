package console

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// tlFormRe reads a row's one action: its route, its way back and its button.
var tlFormRe = regexp.MustCompile(`(?s)<form method="post" action="([^"]+)">\s*<input type="hidden" name="next" value="([^"]+)">(.*?)</form>`)

// tlRowAction is the row's action form: where it posts, the next it carries
// (unescaped), and its button's markup. ok is false for a row with none.
func tlRowAction(row string) (action, next, button string, ok bool) {
	m := tlFormRe.FindStringSubmatch(row[strings.Index(row, `<span class="tl-act">`):])
	if m == nil {
		return "", "", "", false
	}
	return m[1], html.UnescapeString(m[2]), m[3], true
}

// Each row offers the one action it needs, posted to the route the task's
// own page posts it to, and lands back on the list at the row, its filters
// kept, wherever its new group puts it: Mark as handled on a gate on the
// owner, Release claim on a lapsed claim (asked in the page first), Mark
// done on a ready task and on a claimless start. A row a live claim holds,
// a blocked one, one waiting on others and a closed one offer none.
func TestTlRowActionsPostAndReturnToTheRow(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	sess := bkSession(t, db, "example-ops", "cc/tl-holder")
	live, lapsed := now.Add(20*time.Minute), now.Add(-20*time.Minute)
	gate := tlTask(t, db, "example-ops", "", "tl gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign"}
	})
	behind := tlTask(t, db, "example-ops", "", "tl behind the gate", func(k *core.Task) { k.DependsOn = []string{gate.ID} })
	ownGate := tlTask(t, db, "example-ops", "", "tl own gate", func(k *core.Task) { k.Gate = core.TaskGate{Who: "owner", What: "pick"} })
	ready := tlTask(t, db, "example-ops", "", "tl ready", nil)
	claimless := tlTask(t, db, "example-ops", "", "tl claimless", func(k *core.Task) { k.Status = core.TaskInProgress })
	lapsedTask := tlTask(t, db, "example-ops", "", "tl lapsed", func(k *core.Task) {
		k.Status, k.ClaimedBy, k.LeaseExpiresAt = core.TaskInProgress, sess, &lapsed
	})
	held := tlTask(t, db, "example-ops", "", "tl held", func(k *core.Task) {
		k.Status, k.ClaimedBy, k.LeaseExpiresAt = core.TaskInProgress, sess, &live
	})
	waiting := tlTask(t, db, "example-ops", "", "tl waits on bob", func(k *core.Task) { k.Gate = core.TaskGate{Who: "bob", What: "z"} })
	done := tlTask(t, db, "example-ops", "", "tl done", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &now })

	const list = "/console/tasks?project=example-ops&sort=newest"
	page := getHTMLBody(t, mux, list)
	for _, id := range []string{held.ID, behind.ID, waiting.ID, done.ID} {
		_, _, _, ok := tlRowAction(tlRow(t, page, id))
		require.False(t, ok, "%s offers no action on its row", id)
	}
	require.NotContains(t, tlRow(t, page, held.ID), `data-task-act="done"`, "a live claim offers no Mark done")

	for _, tc := range []struct {
		task          core.Task
		act, route    string
		from, landsIn string
		confirm       bool
	}{
		{gate, "handle", "/gate/clear", "needs", "closed", false},
		{ownGate, "handle", "/gate/clear", "needs", "ready", false},
		{lapsedTask, "release", "/release", "needs", "ready", true},
		{claimless, "done", "/status", "needs", "closed", false},
		{ready, "done", "/status", "ready", "closed", false},
	} {
		page := getHTMLBody(t, mux, list)
		require.Contains(t, tlGroupIDs(page, tc.from), tc.task.ID, tc.task.Title)
		action, next, button, ok := tlRowAction(tlRow(t, page, tc.task.ID))
		require.True(t, ok, tc.task.Title)
		require.Equal(t, "/console/tasks/"+tc.task.ID+tc.route, action, tc.task.Title)
		require.Equal(t, list+"#tl-"+tc.task.ID, next, "%s: back to the list at the row", tc.task.Title)
		require.Contains(t, button, `data-task-act="`+tc.act+`"`, tc.task.Title)
		require.Equal(t, tc.confirm, strings.Contains(button, "data-confirm="), "%s: asks in the page first", tc.task.Title)

		form := url.Values{"next": {next}}
		if tc.act == "done" {
			form.Set("to", "done")
		}
		req := httptest.NewRequest(http.MethodPost, action, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set(followHeader, "1")
		rr := do(mux, req)
		require.Equal(t, http.StatusNoContent, rr.Code, "%s: %s", tc.task.Title, rr.Body.String())
		landing, err := url.Parse(rr.Header().Get(locationHeader))
		require.NoError(t, err)
		require.Equal(t, "/console/tasks", landing.Path)
		require.Equal(t, "tl-"+tc.task.ID, landing.Fragment, "%s: lands on the row", tc.task.Title)
		require.Equal(t, "example-ops", landing.Query().Get("project"), "the filters are kept")
		require.Equal(t, "newest", landing.Query().Get("sort"))
		require.NotEmpty(t, landing.Query().Get("notice"), "%s: says what it did", tc.task.Title)
		after := getHTMLBody(t, mux, landing.Path+"?"+landing.RawQuery)
		require.Contains(t, tlGroupIDs(after, tc.landsIn), tc.task.ID, "%s: the row is in its new group", tc.task.Title)
	}
	require.Equal(t, core.TaskOpen, tkTaskByID(t, db, behind.ID).Status)
	require.Contains(t, tlGroupIDs(getHTMLBody(t, mux, list), "ready"), behind.ID, "the handled gate let the step behind it go ahead")
	require.Equal(t, sess, tkTaskByID(t, db, held.ID).ClaimedBy, "nothing touched the live claim")
}

// The cmd/seam release answers as before: a JSON post to a row's route gets
// the task, a 409 when there is nothing to release.
func TestTlReleaseJSONAnswersAsBefore(t *testing.T) {
	db, mux := newConsole(t)
	id := tkTask(t, db, "tl release", func(k *core.Task) { k.Status = core.TaskInProgress })
	rr := tkPost(t, mux, "/console/tasks/"+id+"/release?format=json", url.Values{}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.JSONEq(t, `{"id":"`+id+`","title":"tl release","project":"demo","status":"open"}`, rr.Body.String())
	rr = tkPost(t, mux, "/console/tasks/"+id+"/release?format=json", url.Values{}, true)
	require.Equal(t, http.StatusConflict, rr.Code)
}
