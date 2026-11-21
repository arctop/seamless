package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// seedTokenSession inserts a session with a harvested total_tokens value (0 =
// never harvested, as a live Claude Code session is until its SessionEnd).
func seedTokenSession(t *testing.T, db *sql.DB, id string, status core.SessionStatus, tokens int) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, CreateSession(context.Background(), db, core.Session{
		ID: id, Name: "cc/" + id, Status: status, CreatedAt: now, UpdatedAt: now,
	}))
	if tokens > 0 {
		_, err := db.ExecContext(context.Background(),
			`UPDATE sessions SET total_tokens = ? WHERE id = ?`, tokens, id)
		require.NoError(t, err)
	}
}

// seedEvent inserts an event row directly: the rollup reads the log, so the
// test writes the log rather than replaying the recorder.
func seedEvent(t *testing.T, db *sql.DB, kind core.EventKind, session, project, item string, payload map[string]any) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	body := []byte("{}")
	if payload != nil {
		body, err = json.Marshal(payload)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, core.FormatTime(time.Now().UTC()), string(kind), session, project, item, string(body))
	require.NoError(t, err)
}

func TestPlanTokenRollups(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	step1 := seedPlanTask(t, db, "demo", "p", core.TaskDone)
	step2 := seedPlanTask(t, db, "demo", "p", core.TaskInProgress)
	step3 := seedPlanTask(t, db, "demo", "q", core.TaskOpen)
	loose := seedPlanTask(t, db, "demo", "", core.TaskOpen)

	// A worked both steps of p: one session, counted once.
	seedTokenSession(t, db, "aaaaaaaaaaaaaaaaaaaaaaaaaa", core.SessionCompleted, 5000)
	seedEvent(t, db, core.EventTaskTransition, "aaaaaaaaaaaaaaaaaaaaaaaaaa", "demo", step1, nil)
	seedEvent(t, db, core.EventTaskTransition, "aaaaaaaaaaaaaaaaaaaaaaaaaa", "demo", step2, nil)
	// B straddled p and q (plus a loose non-plan task, which attributes nothing):
	// shared in both plans, whole-session total counted in each.
	seedTokenSession(t, db, "bbbbbbbbbbbbbbbbbbbbbbbbbb", core.SessionCompleted, 2000)
	seedEvent(t, db, core.EventTaskTransition, "bbbbbbbbbbbbbbbbbbbbbbbbbb", "demo", step2, nil)
	seedEvent(t, db, core.EventTaskTransition, "bbbbbbbbbbbbbbbbbbbbbbbbbb", "demo", step3, nil)
	seedEvent(t, db, core.EventTaskTransition, "bbbbbbbbbbbbbbbbbbbbbbbbbb", "demo", loose, nil)
	// C is live: attributed, but its tokens are unreported until SessionEnd.
	seedTokenSession(t, db, "cccccccccccccccccccccccccc", core.SessionActive, 0)
	seedEvent(t, db, core.EventTaskTransition, "cccccccccccccccccccccccccc", "demo", step1, nil)
	// D captured plan r: the planning session is attributed before any step exists.
	seedTokenSession(t, db, "dddddddddddddddddddddddddd", core.SessionCompleted, 700)
	seedEvent(t, db, core.EventPlanCaptured, "dddddddddddddddddddddddddd", "demo", "note-r",
		map[string]any{"plan_slug": "r"})
	// E both captured p and worked a step of it: still one session for p.
	seedTokenSession(t, db, "eeeeeeeeeeeeeeeeeeeeeeeeee", core.SessionCompleted, 100)
	seedEvent(t, db, core.EventPlanCaptured, "eeeeeeeeeeeeeeeeeeeeeeeeee", "demo", "note-p",
		map[string]any{"plan_slug": "p"})
	seedEvent(t, db, core.EventTaskTransition, "eeeeeeeeeeeeeeeeeeeeeeeeee", "demo", step1, nil)
	// Console-attributed events carry no session and attribute nothing.
	seedEvent(t, db, core.EventPlanApproved, "", "demo", "note-p", map[string]any{"plan_slug": "p"})

	got, err := PlanTokenRollups(ctx, db)
	require.NoError(t, err)

	require.Equal(t, PlanTokenRollup{Sessions: 4, Unreported: 1, Shared: 1, Tokens: 7100},
		got[PlanRef{Project: "demo", Slug: "p"}])
	require.Equal(t, PlanTokenRollup{Sessions: 1, Shared: 1, Tokens: 2000},
		got[PlanRef{Project: "demo", Slug: "q"}])
	require.Equal(t, PlanTokenRollup{Sessions: 1, Tokens: 700},
		got[PlanRef{Project: "demo", Slug: "r"}])
	_, ok := got[PlanRef{Project: "demo", Slug: ""}]
	require.False(t, ok, "a non-plan task must not mint a rollup under the empty slug")
}

func TestPlanTokenRollups_MissingSessionRowIsUnreported(t *testing.T) {
	db := openTestDB(t)
	step := seedPlanTask(t, db, "demo", "p", core.TaskDone)
	seedEvent(t, db, core.EventTaskTransition, "01234567890123456789012345", "demo", step, nil)

	got, err := PlanTokenRollups(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, PlanTokenRollup{Sessions: 1, Unreported: 1},
		got[PlanRef{Project: "demo", Slug: "p"}])
}
