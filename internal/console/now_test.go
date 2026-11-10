package console

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
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
