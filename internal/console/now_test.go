package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// seedNowFleet stands up one live agent holding a plan step, one lapsed claim
// whose holder is gone, one claimable task, and one business event -- the
// smallest world in which every Now zone has something to say.
func seedNowFleet(t *testing.T, db *sql.DB) (agentID, claimedID, looseID, readyID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	agentID = mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: agentID, Name: "cc/alpha", ProjectSlug: "demo", Status: core.SessionActive,
		ExternalClient: "claude-code", Model: "claude-fable-5",
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
	}))

	claimedID = mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: claimedID, ProjectSlug: "demo", Title: "wire the guided demo", PlanSlug: "ship-it",
		Status: core.TaskOpen, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}))
	_, err := store.ClaimTask(ctx, db, claimedID, agentID, 15*time.Minute, now)
	require.NoError(t, err)

	// A lapsed claim: the holder session is long gone and so is the lease.
	looseID = mustID(t)
	past := now.Add(-30 * time.Minute)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: looseID, ProjectSlug: "demo", Title: "abandoned migration step", PlanSlug: "ship-it",
		Status: core.TaskInProgress, ClaimedBy: mustID(t), LeaseExpiresAt: &past,
		CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: past,
	}))

	readyID = mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: readyID, ProjectSlug: "demo", Title: "polish the empty states",
		Status: core.TaskOpen, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}))

	_, err = events.NewRecorder(db).Record(ctx, core.Event{
		Kind: core.EventMemoryWritten, SessionID: agentID, ProjectSlug: "demo",
		Payload: map[string]any{"name": "demo-gotcha"},
	})
	require.NoError(t, err)
	return agentID, claimedID, looseID, readyID
}

// justAfterLocalMidnight moves a synctest bubble's clock, the one the Now page
// reads, to 00:00:30 local on the bubble's next local day. A blocked timer is
// how a bubble's clock moves, and it takes no real time. Call it first: the
// clock advances only while nothing else in the bubble is running.
func justAfterLocalMidnight(t *testing.T) {
	t.Helper()
	l := time.Now().Local()
	target := time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 30, 0, l.Location())
	<-time.After(target.Sub(l))
	require.Equal(t, "00:00:30", time.Now().Local().Format("15:04:05"))
}

func TestNowPage_ShowsTheFleet(t *testing.T) {
	db, mux := newConsole(t)
	agentID, claimedID, looseID, readyID := seedNowFleet(t, db)

	rr := getPeek(t, mux, "/console/now")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// The sidebar offers the page and its badge counts the live fleet.
	require.Contains(t, body, `href="/console/now"`)

	// The agent card: identity, harness pill, claim with its lease ticker data.
	require.Contains(t, body, `id="ag-`+agentID+`"`, "live rows need stable ids for the morph")
	require.Contains(t, body, "cc/alpha")
	require.Contains(t, body, `data-fresh="hot"`)
	require.Contains(t, body, `id="cl-`+claimedID+`"`)
	require.Contains(t, body, `data-lease-exp=`)
	require.Contains(t, body, "plan:ship-it")

	// The loose end: lapsed, with the no-confirm release action.
	require.Contains(t, body, `id="lo-`+looseID+`"`)
	require.Contains(t, body, `data-state="lapsed"`)
	require.Contains(t, body, `action="/console/tasks/`+looseID+`/release"`)

	// The plans rail and the ready queue.
	require.Contains(t, body, `id="pl-demo-ship-it"`)
	require.Contains(t, body, `id="up-`+readyID+`"`)

	// The wire carries the business event.
	require.Contains(t, body, "wrote memory demo-gotcha")

	// Scoping to another project empties the fleet without erroring.
	scoped := getPeek(t, mux, "/console/now?scope=elsewhere").Body.String()
	require.NotContains(t, scoped, `id="ag-`+agentID+`"`)
	require.Contains(t, scoped, "All quiet")

	var data nowData
	getJSON(t, mux, "/console/now?format=json", &data)
	require.Equal(t, 1, data.AgentsLive)
	require.Equal(t, 2, data.InFlight)
	require.Len(t, data.Loose, 1)
	require.Equal(t, "lapsed", data.Loose[0].State)
	require.Len(t, data.UpNext, 1)
	require.Equal(t, readyID, data.UpNext[0].ID)
}

// The arcade is a gamification surface: off (the shipped default), no query
// runs, no latch is written, no event is minted, and the page carries zero
// trace in HTML and JSON alike; on, the tape and records render and a record
// crossing is celebrated exactly once. Every run sits in the first minute of the
// local day, where "today" is easiest to get wrong.
func TestNowPage_ArcadeFollowsTheGamificationFeature(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		justAfterLocalMidnight(t)
		db, mux, _ := newGatedConsole(t)
		ctx := context.Background()
		now := time.Now().UTC()

		agentID, _, _, _ := seedNowFleet(t, db)
		closed := now.Add(-10 * time.Second) // still today; a minute back is yesterday
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: mustID(t), ProjectSlug: "demo", Title: "already shipped today",
			Status: core.TaskDone, ClosedAt: &closed,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: closed,
		}))
		recordEvents := func() int {
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`,
				string(core.EventRecordBroken)).Scan(&n))
			return n
		}
		latchRows := func() int {
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = ?`,
				store.SettingGamificationRecords).Scan(&n))
			return n
		}

		off := getPeek(t, mux, "/console/now").Body.String()
		require.Contains(t, off, `id="ag-`+agentID+`"`, "the core page renders regardless of the feature")
		for _, marker := range []string{"now-tape", "now-record", "now-moment", "now-hot", "data-moment"} {
			require.NotContains(t, off, marker, "off means zero trace")
		}
		require.NotContains(t, getJSON2(t, mux, "/console/now?format=json"), "arcade")
		require.Zero(t, latchRows(), "off means no computation and no latch")
		require.Zero(t, recordEvents())

		require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Gamification: true}))
		on := getPeek(t, mux, "/console/now").Body.String()
		require.Contains(t, on, "now-tape")
		require.Contains(t, on, "tasks closed")
		require.Contains(t, on, "most tasks closed in a day")
		require.Contains(t, on, `data-moment="record-tasks_day-`, "a fresh record is a moment")
		require.Equal(t, 1, latchRows(), "the ledger latched")
		require.Equal(t, 3, recordEvents(), "each crossing (tasks, memories, live agents) is minted exactly once")

		again := getPeek(t, mux, "/console/now").Body.String()
		require.Contains(t, again, "now-record")
		require.Equal(t, 3, recordEvents(), "re-rendering mints nothing new")
		require.Contains(t, getJSON2(t, mux, "/console/now?format=json"), "arcade")
	})
}

// nwFleet is the world the plain-words Now tests read: three live agents --
// one holding a live claim and one it let lapse, one holding only a lapsed
// claim, one holding nothing -- plus a plan with a narrative note and a plan
// with steps only.
type nwFleet struct {
	mux                              *http.ServeMux
	holder, lapser, idle             string // live sessions
	held, heldLapsed, lapsed         string // tasks
	notedPlan, taskOnlyPlan, project string
}

func nwSeedFleet(t *testing.T) nwFleet {
	t.Helper()
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := nwFleet{mux: mux, notedPlan: "nw-noted", taskOnlyPlan: "nw-steps-only", project: "demo"}

	session := func(name string) string {
		id := mustID(t)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: f.project, Status: core.SessionActive,
			ExternalClient: "claude-code", CreatedAt: now.Add(-40 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
		}))
		return id
	}
	f.holder, f.lapser, f.idle = session("cc/nwholder"), session("cc/nwlapser"), session("cc/nwidle")

	f.held = mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: f.held, ProjectSlug: f.project, Title: "Rotate the backup encryption key with a title long enough to need a second line",
		PlanSlug: f.notedPlan, Status: core.TaskOpen, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}))
	_, err := store.ClaimTask(ctx, db, f.held, f.holder, 15*time.Minute, now)
	require.NoError(t, err)

	lapsedTask := func(title, holder, plan string) string {
		id := mustID(t)
		exp := now.Add(-12 * time.Minute)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: f.project, Title: title, PlanSlug: plan,
			Status: core.TaskInProgress, ClaimedBy: holder, LeaseExpiresAt: &exp,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-20 * time.Minute),
		}))
		return id
	}
	f.heldLapsed = lapsedTask("Move the metrics exporter to its new port", f.holder, f.taskOnlyPlan)
	f.lapsed = lapsedTask("Replay protection and the dedupe store", f.lapser, "")

	_, err = mgr.WriteNote(ctx, core.Note{
		ID: mustID(t), Slug: "nw-noted-narrative", Title: "The noted plan", Project: f.project,
		Body: "# The noted plan\n", Tags: []string{plans.SlugTag(f.notedPlan), "created-by:agent"},
		Created: now, Updated: now,
	})
	require.NoError(t, err)
	return f
}

// The owner reads one story per agent: a claim it let lapse says so on its
// card in the words Loose ends uses for the same task, each row says its state
// once, and an agent that holds nothing says that plainly.
func TestNwNow_LapsedClaimReadsTheSameOnTheCardAndInLooseEnds(t *testing.T) {
	f := nwSeedFleet(t)
	body := getPeek(t, f.mux, "/console/now").Body.String()

	card := func(id string) string {
		at := strings.Index(body, `id="ag-`+id+`"`)
		require.GreaterOrEqual(t, at, 0, "no card for %s", id)
		end := strings.Index(body[at:], "</article>")
		require.Greater(t, end, 0)
		return body[at : at+end]
	}
	loose := func(id string) string {
		at := strings.Index(body, `id="lo-`+id+`"`)
		require.GreaterOrEqual(t, at, 0, "no loose end for %s", id)
		end := strings.Index(body[at:], "</form>")
		require.Greater(t, end, 0)
		return body[at : at+end]
	}

	for _, tc := range []struct{ agent, task string }{{f.holder, f.heldLapsed}, {f.lapser, f.lapsed}} {
		c, l := card(tc.agent), loose(tc.task)
		require.Contains(t, c, `id="cl-lapsed-`+tc.task+`"`, "the card carries its lapsed claim")
		for _, row := range []string{c[strings.Index(c, `id="cl-lapsed-`+tc.task+`"`):], l} {
			require.Contains(t, row, `data-word="claim-lapsed"`)
			require.Contains(t, row, ">claim lapsed</span>")
			require.Contains(t, row, `data-clock="title"`, "when it lapsed, as a local time on hover")
			require.Contains(t, row, " ago</time>")
		}
		require.NotContains(t, c, "roaming")
		require.NotContains(t, c, "holds no claim", "an agent with a lapsed claim does not read as holding nothing")
		// One state phrase: the lapsed row carries no second "lapsed" from a
		// lease countdown.
		require.NotContains(t, l, "data-lease-exp", "a lapsed claim counts nothing down")
		require.NotContains(t, l, ">lapsed<")
		require.NotContains(t, l, "expired")
		require.Contains(t, l, "claimed by <a href=\"/console/sessions/"+tc.agent+"\">")
	}
	require.Contains(t, card(f.idle), "holds no claim")
	require.NotContains(t, body, "roaming")

	var data nowData
	getJSON(t, f.mux, "/console/now?format=json", &data)
	byID := map[string]nowAgentVM{}
	for _, a := range data.Agents {
		byID[a.ID] = a
	}
	require.Len(t, byID[f.holder].Lapsed, 1)
	require.Equal(t, f.heldLapsed, byID[f.holder].Lapsed[0].ID)
	require.Len(t, byID[f.lapser].Lapsed, 1)
	require.Empty(t, byID[f.idle].Lapsed)
	for _, l := range data.Loose {
		require.Equal(t, "lapsed", l.State)
		require.Equal(t, wordLapsed, l.Word.Key)
		require.NotNil(t, l.LapsedAt)
	}
}

// A card is headed by the task its agent holds; the agent's name, harness and
// account sit under it. One that holds nothing keeps its name as its title.
// Last-heartbeat and wall-clock read as words.
func TestNwNow_CardTitleAndPlainTimes(t *testing.T) {
	f := nwSeedFleet(t)
	var data nowData
	getJSON(t, f.mux, "/console/now?format=json", &data)
	byID := map[string]nowAgentVM{}
	for _, a := range data.Agents {
		byID[a.ID] = a
	}
	require.Equal(t, "Rotate the backup encryption key with a title long enough to need a second line", byID[f.holder].Title)
	require.Equal(t, "cc/nwlapser", byID[f.lapser].Title, "a lapsed claim is not held: the name stays the title")
	require.Equal(t, "cc/nwidle", byID[f.idle].Title)

	body := getPeek(t, f.mux, "/console/now").Body.String()
	require.Contains(t, body, `<span class="now-agent-handle mono" title="cc/nwholder">cc/nwholder</span>`)
	require.NotContains(t, body, `now-agent-handle mono" title="cc/nwidle"`, "a name that is the title is not repeated")
	require.Contains(t, body, ">last seen 2m ago</time>")
	require.Contains(t, body, "running 40m")
	require.NotContains(t, body, ">beat ")
	require.NotContains(t, body, ">up ")
}

// No Now title is cut without a way to read it: each line the CSS clamps to
// two lines carries its whole text in its title.
func TestNwNow_ClampedTitlesKeepTheirWholeText(t *testing.T) {
	f := nwSeedFleet(t)
	body := getPeek(t, f.mux, "/console/now").Body.String()
	title := "Rotate the backup encryption key with a title long enough to need a second line"
	require.Contains(t, body, `<a class="now-agent-name" href="/console/sessions/`+f.holder+`" title="`+title+`">`+title+`</a>`)
	require.Contains(t, body, `<span class="t" title="`+title+`">`+title+`</span>`)
	require.Contains(t, body, `<span class="t" title="Move the metrics exporter to its new port">`)
	require.Contains(t, body, `<span class="mono nm" title="plan:`+f.notedPlan+`">`)
	require.Contains(t, body, `<span class="now-chip plan" title="plan:`+f.notedPlan+`">`)

	css := string(consoleCSS)
	require.Contains(t, css, ".now-agent-name, .now-claim .t, .now-lapsed .t, .now-trail span, .now-plan-top .nm, .now-evt p {")
	require.Contains(t, css, "-webkit-line-clamp: 2; line-clamp: 2;")
}

// A plan card opens the plan's page when it has one in its project, and its
// project's Plans & tasks tab when it has none (a plan of steps only), so no
// card links to a 404.
func TestNwNow_PlanCardsLinkToThePlan(t *testing.T) {
	f := nwSeedFleet(t)
	body := getPeek(t, f.mux, "/console/now").Body.String()
	require.Contains(t, body, `id="pl-demo-`+f.notedPlan+`" href="/console/plans/`+f.notedPlan+`"`)
	require.Contains(t, body, `id="pl-demo-`+f.taskOnlyPlan+`" href="/console/projects/demo?tab=tasks"`)

	rr := getPeek(t, f.mux, "/console/plans/"+f.notedPlan)
	require.Equal(t, http.StatusOK, rr.Code, "the linked plan page opens")
	rr = getPeek(t, f.mux, "/console/plans/"+f.taskOnlyPlan)
	require.Equal(t, http.StatusNotFound, rr.Code, "a plan of steps only has no page to link")
}

// Now reads through the request's handle: a locked project's live agent, its
// claims and its plan are absent from the page, its plan's page included.
func TestNwNow_LockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	exp := now.Add(-5 * time.Minute)
	stepID := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{
		ID: stepID, ProjectSlug: lockedSlug, Title: "close the books " + lockedSecret, PlanSlug: lc.plan,
		Status: core.TaskInProgress, ClaimedBy: lc.sessID, LeaseExpiresAt: &exp,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-10 * time.Minute),
	}))
	openID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, lc.db, core.Session{
		ID: openID, Name: "cc/opsagent", ProjectSlug: openSlug, Status: core.SessionActive,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}))

	req := httptest.NewRequest(http.MethodGet, "/console/now", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, `id="ag-`+openID+`"`, "the open project's agent is on duty")
	for _, hidden := range []string{lockedSecret, lc.sessID, "cc/ledger", stepID, lc.plan} {
		require.NotContains(t, body, hidden)
	}
}

// Each loose end says its state once, in the task words: a claimless start is
// "unclaimed" with nothing counting down, and a live claim whose holder went
// quiet is "in progress", names its quiet holder, and keeps its countdown.
func TestNwNow_EachLooseEndSaysItsStateOnce(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	claimless := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: claimless, ProjectSlug: "demo", Title: "Tidy the alert routing table",
		Status: core.TaskInProgress, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}))
	gone := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: gone, Name: "cc/nwgone", ProjectSlug: "demo", Status: core.SessionCompleted,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}))
	quiet := mustID(t)
	exp := now.Add(20 * time.Minute)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: quiet, ProjectSlug: "demo", Title: "Benchmark the new storage pool",
		Status: core.TaskInProgress, ClaimedBy: gone, LeaseExpiresAt: &exp,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-5 * time.Minute),
	}))

	body := getPeek(t, mux, "/console/now").Body.String()
	row := func(id string) string {
		at := strings.Index(body, `id="lo-`+id+`"`)
		require.GreaterOrEqual(t, at, 0)
		end := strings.Index(body[at:], "\n      </div>")
		require.Greater(t, end, 0)
		return body[at : at+end]
	}
	c := row(claimless)
	require.Contains(t, c, `data-word="unclaimed"`)
	require.NotContains(t, c, "data-lease-exp")
	require.NotContains(t, c, "claimed by")
	require.NotContains(t, c, ">claimless<", "the state key is not printed as a word")

	require.Contains(t, body, `data-state="quiet-holder" id="lo-`+quiet+`"`)
	q := row(quiet)
	require.Contains(t, q, `data-word="in-progress"`)
	require.Contains(t, q, "claimed by <a href=\"/console/sessions/"+gone+"\">cc/nwgone</a>, which went quiet")
	require.Contains(t, q, "data-lease-exp=", "a live claim still counts down")
	require.NotContains(t, q, "release claim", "a live claim is not released without a confirm")
}
