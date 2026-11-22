package console

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
)

// The project page's Sessions tab titles its rows the way the Sessions
// screen does (sessionHeadlines): a live session by the task it holds, any
// other by its first prompt, with its name on a second line; a session that
// has neither keeps its name.
func TestFuProjectSessionsTab_RowsWearTheHeadline(t *testing.T) {
	db, mux := newConsole(t)
	rec := events.NewRecorder(db)
	pvProject(t, db, "demo", "Demo")
	holder := snSession(t, db, "cc/fu000001", "demo", core.SessionActive, "", time.Minute)
	held := snTask(t, db, "demo", "Fix the watcher race")
	snClaim(t, db, held, holder)
	asker := snSession(t, db, "cc/fu000002", "demo", core.SessionActive, "", 2*time.Minute)
	snPrompt(t, rec, asker, "Add a **retry** to the `uploader`")
	snSession(t, db, "cc/fu000003", "demo", core.SessionActive, "", 3*time.Minute)

	body := getHTMLBody(t, mux, "/console/projects/demo?tab=sessions")
	require.Contains(t, body, `class="sess-headline" href="/console/sessions/`+holder.ID+`">Fix the watcher race</a>`)
	require.Contains(t, body, `<span class="mono">cc/fu000001</span> &middot; holds this task`)
	require.Contains(t, body, `class="sess-headline is-prompt" href="/console/sessions/`+asker.ID+`">Add a retry to the uploader</a>`)
	require.Contains(t, body, `<span class="mono">cc/fu000002</span> &middot; its first prompt`)
	require.Contains(t, body, `">cc/fu000003</a>`, "a session with no task and no prompt keeps its name")
	require.Contains(t, body, `<a href="/console/tasks/`+held.ID+`">Fix the watcher race</a>`, "the held task is still linked from its claim")
}

// A claim on a Now card wears the Tasks icon: a claim is never a lock.
func TestFuNow_ClaimRowIsNotALock(t *testing.T) {
	db, mux := newConsole(t)
	holder := snSession(t, db, "cc/fu000021", "demo", core.SessionActive, "", time.Minute)
	task := snTask(t, db, "demo", "Rotate the signing keys")
	snClaim(t, db, task, holder)
	body := getHTMLBody(t, mux, "/console/now")
	start := strings.Index(body, `id="cl-`+task.ID+`"`)
	require.Positive(t, start)
	row := body[start:]
	row = row[:strings.Index(row, "</a>")]
	require.NotContains(t, row, "lock", "the claim row's icon")
}

// A Now card whose agent holds no claim is titled by the session's first
// prompt; one that holds a claim is still titled by the claim.
func TestFuNow_CardTitledByTheFirstPrompt(t *testing.T) {
	db, mux := newConsole(t)
	rec := events.NewRecorder(db)
	asker := snSession(t, db, "cc/fu000011", "demo", core.SessionActive, "", time.Minute)
	snPrompt(t, rec, asker, "Why is the nightly job slow?")
	holder := snSession(t, db, "cc/fu000012", "demo", core.SessionActive, "", 2*time.Minute)
	snClaim(t, db, snTask(t, db, "demo", "Rotate the signing keys"), holder)
	quiet := snSession(t, db, "cc/fu000013", "demo", core.SessionActive, "", 3*time.Minute)

	var data nowData
	getJSON(t, mux, "/console/now?format=json", &data)
	titles := map[string]string{}
	for _, a := range data.Agents {
		titles[a.ID] = a.Title
	}
	require.Equal(t, "Why is the nightly job slow?", titles[asker.ID])
	require.Equal(t, "Rotate the signing keys", titles[holder.ID])
	require.Equal(t, "cc/fu000013", titles[quiet.ID])
}

// The project bar's open tasks name what waits on people apart from what is
// blocked: an open gate is never counted as blocked (UX 11's one meaning).
func TestFuProjectBar_WaitingOnPeopleApartFromBlocked(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	bdTask(t, db, "demo", "", "Owner signs off", core.TaskKindGate, "owner", "sign the renewal")

	body := getHTMLBody(t, mux, "/console/projects/demo?tab=overview")
	require.Contains(t, body, "1 waiting on people")
	require.NotContains(t, body, " blocked</small>")
}
