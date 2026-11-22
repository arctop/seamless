package demokit

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// reviewTestAnchor is the review tests' anchor: Friday 2026-11-20, 18:30 UTC.
var reviewTestAnchor = time.Date(2026, 11, 20, 18, 30, 0, 0, time.UTC)

// reviewFleetSummary is the console fleet's summary at reviewTestAnchor, as
// the default mode prints it. The landing page's console shots come from that
// seed, so its summary must not move when the review layer does.
const reviewFleetSummary = "seeded: 212 sessions, 128 active memories (131 total), 57 notes, 29 tasks (9 open/wip), 2342 events, 2 proposals\n" +
	"kinds: gotcha=31 decision=23 constraint=20 runbook=16 reference=16 protocol=12 stage=8 refuted=2\n"

// reviewNew is a Seeder over a fresh data dir, anchored at reviewTestAnchor.
func reviewNew(t *testing.T) *Seeder {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.now = reviewTestAnchor
	return s
}

// reviewSeedAt seeds the review fixture at reviewTestAnchor.
func reviewSeedAt(t *testing.T) (*Seeder, ReviewFixture) {
	t.Helper()
	s := reviewNew(t)
	fx, err := s.SeedReview()
	require.NoError(t, err)
	return s, fx
}

// reviewItem is the fixture's item of a state.
func reviewItem(t *testing.T, fx ReviewFixture, state ReviewState) ReviewItem {
	t.Helper()
	it, ok := fx.Item(state)
	require.True(t, ok, "state %s has no item", state)
	return it
}

// reviewTask is the task an item names first.
func reviewTask(t *testing.T, s *Seeder, fx ReviewFixture, state ReviewState) core.Task {
	t.Helper()
	it := reviewItem(t, fx, state)
	require.NotEmpty(t, it.IDs, "state %s", state)
	task, err := store.TaskByID(s.ctx, s.db, it.IDs[0])
	require.NoError(t, err)
	return task
}

// reviewRollup is one plan's rollup in one project.
func reviewRollup(t *testing.T, s *Seeder, project, plan string) store.ProjectPlan {
	t.Helper()
	all, err := store.AllPlanRollups(s.ctx, s.db)
	require.NoError(t, err)
	i := slices.IndexFunc(all, func(p store.ProjectPlan) bool { return p.Project == project && p.Slug == plan })
	require.GreaterOrEqual(t, i, 0, "plan %s in %s", plan, project)
	return all[i]
}

// reviewBlockedIDs is the ids of a BlockedTask list.
func reviewBlockedIDs(list []store.BlockedTask) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.Task.ID
	}
	return out
}

// TestSeedReview seeds the review fixture once and asserts every state the
// task asks for by querying it the way the console reads it.
func TestSeedReview(t *testing.T) {
	s, fx := reviewSeedAt(t)
	ctx, db, now := s.ctx, s.db, reviewTestAnchor

	t.Run("every state has an item, in order", func(t *testing.T) {
		got := make([]ReviewState, len(fx.Items))
		for i, it := range fx.Items {
			got[i] = it.State
			require.NotEmpty(t, it.Page, "state %s", it.State)
			require.NotEmpty(t, it.Detail, "state %s", it.State)
		}
		require.Equal(t, reviewStates, got)
		sum := fx.Summary()
		require.True(t, strings.HasPrefix(sum, "seeded: "), sum)
		for _, st := range reviewStates {
			require.Contains(t, sum, "  "+string(st)+" ", "the summary names %s", st)
		}
	})

	// Off-plan tasks in one project.
	t.Run("off-plan: in progress with a live claim", func(t *testing.T) {
		task := reviewTask(t, s, fx, ReviewTaskClaimLive)
		require.Equal(t, ReviewOps, task.ProjectSlug)
		require.Empty(t, task.PlanSlug)
		require.True(t, task.ClaimLive(now))
		holder, found, err := store.SessionByID(ctx, db, task.ClaimedBy)
		require.NoError(t, err)
		require.True(t, found)
		require.True(t, holder.LiveAsOf(now, 0), "the holder is live")
	})
	t.Run("off-plan: in progress with a lapsed lease", func(t *testing.T) {
		task := reviewTask(t, s, fx, ReviewTaskClaimLapsed)
		require.Empty(t, task.PlanSlug)
		require.Equal(t, core.TaskInProgress, task.Status)
		require.NotEmpty(t, task.ClaimedBy)
		require.NotNil(t, task.LeaseExpiresAt)
		require.True(t, task.LeaseExpiresAt.Before(now))
		require.False(t, task.ClaimLive(now))
	})
	t.Run("off-plan: in progress with no holder", func(t *testing.T) {
		task := reviewTask(t, s, fx, ReviewTaskClaimless)
		require.Empty(t, task.PlanSlug)
		require.Equal(t, core.TaskInProgress, task.Status)
		require.Empty(t, task.ClaimedBy)
		require.Nil(t, task.LeaseExpiresAt)
	})
	t.Run("off-plan: blocked by an open task", func(t *testing.T) {
		it := reviewItem(t, fx, ReviewTaskBlocked)
		blocked, err := store.BlockedTasks(ctx, db, ReviewOps)
		require.NoError(t, err)
		i := slices.IndexFunc(blocked, func(b store.BlockedTask) bool { return b.Task.ID == it.IDs[0] })
		require.GreaterOrEqual(t, i, 0)
		require.Len(t, blocked[i].Blockers, 1)
		require.Equal(t, it.IDs[1], blocked[i].Blockers[0].ID)
		require.Equal(t, core.TaskOpen, blocked[i].Blockers[0].Status)
	})
	t.Run("off-plan: an open gate waits on the owner", func(t *testing.T) {
		task := reviewTask(t, s, fx, ReviewTaskGated)
		require.False(t, task.IsGate())
		require.True(t, task.Gate.Open())
		require.Equal(t, "owner", task.Gate.Who)
		gated, err := store.GatedTasks(ctx, db, ReviewOps)
		require.NoError(t, err)
		require.Contains(t, reviewBlockedIDs(gated), task.ID)
	})
	t.Run("off-plan: a gate two steps depend on", func(t *testing.T) {
		it := reviewItem(t, fx, ReviewTaskGate)
		gate := reviewTask(t, s, fx, ReviewTaskGate)
		require.True(t, gate.IsGate())
		require.True(t, gate.Gate.Open())
		gates, err := store.GateTasks(ctx, db, ReviewOps)
		require.NoError(t, err)
		require.Contains(t, reviewBlockedIDs(gates), gate.ID)
		dependents, err := store.TasksBlockedBy(ctx, db, gate.ID)
		require.NoError(t, err)
		require.Len(t, dependents, 2)
		gated, err := store.GatedTasks(ctx, db, ReviewOps)
		require.NoError(t, err)
		for _, id := range it.IDs[1:] {
			require.Contains(t, reviewBlockedIDs(gated), id, "a dependent waits on the gate")
		}
	})
	t.Run("off-plan: several closed, done and dropped", func(t *testing.T) {
		done, err := store.ListTasks(ctx, db, ReviewOps, core.TaskDone)
		require.NoError(t, err)
		dropped, err := store.ListTasks(ctx, db, ReviewOps, core.TaskDropped)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(done), 3)
		require.GreaterOrEqual(t, len(dropped), 2)
		require.Len(t, reviewItem(t, fx, ReviewTaskClosed).IDs, len(done)+len(dropped))
	})

	// Plans.
	t.Run("plan: complete except for a dropped step", func(t *testing.T) {
		p := reviewRollup(t, s, ReviewOps, rPlanDNS)
		require.Equal(t, p.Total, p.Done, "every step closed")
		steps, err := store.ListTasksForPlan(ctx, db, ReviewOps, "", rPlanDNS)
		require.NoError(t, err)
		var dropped int
		for _, st := range steps {
			if st.Status == core.TaskDropped {
				dropped++
			}
		}
		require.Equal(t, 1, dropped)
	})
	t.Run("plan: steps and no narrative note", func(t *testing.T) {
		p := reviewRollup(t, s, ReviewOps, rPlanRunbooks)
		require.Positive(t, p.Total)
		notes, err := store.NotesByTag(ctx, db, "", "plan:"+rPlanRunbooks)
		require.NoError(t, err)
		require.Empty(t, notes)
	})
	t.Run("plan: stalled 10+ days with a claimable step", func(t *testing.T) {
		p := reviewRollup(t, s, ReviewOps, rPlanMetrics)
		require.True(t, p.LastActivity.Before(now.AddDate(0, 0, -10)), "last moved %s", p.LastActivity)
		require.Zero(t, p.InFlight)
		require.Positive(t, p.Claimable)
		require.Less(t, p.Done, p.Total)
		notes, err := store.NotesByTag(ctx, db, ReviewOps, "plan:"+rPlanMetrics)
		require.NoError(t, err)
		require.Len(t, notes, 1)
	})
	t.Run("plan: a step waits on an open gate", func(t *testing.T) {
		p := reviewRollup(t, s, ReviewOps, rPlanStorage)
		require.Positive(t, p.GatesOpen)
		gated, err := store.GatedTasksForPlan(ctx, db, ReviewOps, rPlanStorage)
		require.NoError(t, err)
		it := reviewItem(t, fx, ReviewPlanGated)
		i := slices.IndexFunc(gated, func(b store.BlockedTask) bool { return b.Task.ID == it.IDs[1] })
		require.GreaterOrEqual(t, i, 0)
		require.True(t, slices.ContainsFunc(gated[i].Blockers, func(b core.Task) bool {
			return b.ID == it.IDs[2] && b.IsGate() && b.Gate.Open()
		}))
	})
	t.Run("plan: the same slug in two projects", func(t *testing.T) {
		reviewRollup(t, s, ReviewOps, rPlanShared)
		reviewRollup(t, s, ReviewWeb, rPlanShared)
	})
	t.Run("plan: the captured plan awaiting approval stays", func(t *testing.T) {
		presented, err := store.NotesByTag(ctx, db, "orbital", "plan-status:presented")
		require.NoError(t, err)
		require.True(t, slices.ContainsFunc(presented, func(n core.Note) bool { return n.Slug == "cc-plan-api-rate-limiting" }))
	})

	// Projects.
	board, err := store.ProjectsWithCounts(ctx, db, store.ResolveRetrievalWindow("30d", now), now, 0)
	require.NoError(t, err)
	projects, err := store.ListProjects(ctx, db)
	require.NoError(t, err)
	t.Run("projects: about 40 quiet, a few with open tasks", func(t *testing.T) {
		it := reviewItem(t, fx, ReviewProjectsQuiet)
		require.Len(t, it.IDs, 40)
		withTasks := 0
		for _, row := range board {
			if !slices.Contains(it.IDs, row.Project) {
				continue
			}
			require.False(t, row.LastActive.IsZero(), "%s has a session", row.Project)
			require.True(t, row.LastActive.Before(now.AddDate(0, 0, -30)), "%s last active %s", row.Project, row.LastActive)
			if row.OpenTasks > 0 {
				withTasks++
			}
		}
		require.GreaterOrEqual(t, withTasks, 3)
		require.GreaterOrEqual(t, len(projects), 44, "the board at the owner's scale")
	})
	t.Run("projects: 3 starred, one retired, one confidential", func(t *testing.T) {
		var starred, retired, confidential []string
		for _, p := range projects {
			if p.Favorite {
				starred = append(starred, p.Slug)
			}
			if p.Retired() {
				retired = append(retired, p.Slug)
			}
			if p.Isolation == core.IsolationConfidential {
				confidential = append(confidential, p.Slug)
			}
		}
		require.ElementsMatch(t, reviewItem(t, fx, ReviewProjectsStarred).IDs, starred)
		require.Len(t, starred, 3)
		require.Equal(t, reviewItem(t, fx, ReviewProjectRetired).IDs, retired)
		require.Equal(t, reviewItem(t, fx, ReviewProjectConfidential).IDs, confidential)
	})

	// Mishaps, the held write, the lock request, sessions and usage.
	t.Run("8 mishap reports", func(t *testing.T) {
		reports, err := store.ListMishaps(ctx, db, store.MishapFilter{Project: ReviewOps, ByProject: true})
		require.NoError(t, err)
		require.Len(t, reports, 8)
		types := map[core.MishapType]bool{}
		named := 0
		for _, r := range reports {
			types[r.Type] = true
			if len(r.ItemIDs) > 0 {
				named++
			}
		}
		require.Len(t, types, len(core.MishapTypes)+1, "every type, and untyped")
		require.Equal(t, 1, named)
	})
	t.Run("one held write", func(t *testing.T) {
		held, err := store.HeldWrites(ctx, db, store.HeldStatusHeld, 0)
		require.NoError(t, err)
		require.Len(t, held, 1)
		require.Equal(t, reviewItem(t, fx, ReviewHeldWrite).IDs, []string{held[0].ID})
		require.Equal(t, []string{"session.tainted"}, held[0].Reasons)
		_, tainted, err := store.SessionTaintOf(ctx, db, held[0].SessionID)
		require.NoError(t, err)
		require.True(t, tainted)
	})
	t.Run("one lock declaration waiting for the owner", func(t *testing.T) {
		locks, err := store.ListRepoLocks(ctx, db)
		require.NoError(t, err)
		require.Len(t, locks, 1)
		require.Equal(t, core.RepoLockRequested, locks[0].State)
		require.Equal(t, ReviewAccount, locks[0].Account)
		require.Equal(t, reviewItem(t, fx, ReviewLockRequested).IDs, []string{locks[0].ID})
	})
	t.Run("live sessions holding different claims", func(t *testing.T) {
		it := reviewItem(t, fx, ReviewSessionsLive)
		require.Len(t, it.IDs, 4)
		inProgress, err := store.AllTasksByStatus(ctx, db, core.TaskInProgress)
		require.NoError(t, err)
		held := map[string][]string{}
		for _, task := range inProgress {
			if task.ClaimLive(now) {
				held[task.ClaimedBy] = append(held[task.ClaimedBy], task.ID)
			}
		}
		seen := map[string]bool{}
		for _, id := range it.IDs {
			sess, found, err := store.SessionByID(ctx, db, id)
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, sess.LiveAsOf(now, 0), "%s is live", sess.Name)
			require.Len(t, held[id], 1, "%s holds one live claim", sess.Name)
			require.False(t, seen[held[id][0]], "each holds a different task")
			seen[held[id][0]] = true
		}
		lapsed := reviewTask(t, s, fx, ReviewTaskClaimLapsed)
		require.Contains(t, it.IDs, lapsed.ClaimedBy, "the lapsed claim's holder is a live session")
	})
	t.Run("250+ sessions in one project", func(t *testing.T) {
		sessions, err := store.ListSessionsForProject(ctx, db, ReviewOps, "", time.Time{}, 1000)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(sessions), 250)
		inWindow, err := store.ListSessionsForProject(ctx, db, ReviewOps, "", now.AddDate(0, 0, -30), 1000)
		require.NoError(t, err)
		require.Greater(t, len(inWindow), 200, "more than the sessions list shows in its 30-day window")
	})
	t.Run("usage-ledger rows make token and time figures non-zero", func(t *testing.T) {
		rows, err := store.UsageBetween(ctx, db, now.AddDate(0, 0, -DiaryLedgerDays), now)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		today, err := store.UsageBetween(ctx, db, now.Add(-24*time.Hour), now)
		require.NoError(t, err)
		require.NotEmpty(t, today)
		minutes := 0
		for _, r := range today {
			for m := r.MinuteMask; m != 0; m &= m - 1 {
				minutes++
			}
		}
		require.Positive(t, minutes, "time figures")
		for _, row := range board {
			if row.Project == ReviewOps {
				require.Positive(t, row.TokensTotal, "token figures")
			}
		}
	})
}

// TestSeedReview_DefaultSeedUnchanged pins the default seed: its summary at
// the fixed anchor is the one the base printed, and a review seed holds every
// row the default seed writes, unchanged.
func TestSeedReview_DefaultSeedUnchanged(t *testing.T) {
	fleet := reviewNew(t)
	fleet.consoleFleet()
	require.Equal(t, reviewFleetSummary, fleet.fleetSummary())

	review, fx := reviewSeedAt(t)
	require.True(t, strings.HasPrefix(fx.Summary(), "seeded: "))
	for _, q := range []string{
		`SELECT * FROM projects`,
		`SELECT * FROM sessions`,
		`SELECT * FROM tasks`,
		`SELECT * FROM task_deps`,
		`SELECT * FROM memories_index`,
		`SELECT * FROM notes_index`,
		`SELECT * FROM trials`,
		`SELECT * FROM gardener_proposals`,
		`SELECT ts, kind, session_id, project_slug, item_id, payload FROM events`,
	} {
		want, got := reviewRows(t, fleet, q), reviewRows(t, review, q)
		require.NotEmpty(t, want, q)
		missing := 0
		for row, n := range want {
			if got[row] < n {
				missing++
			}
		}
		require.Zero(t, missing, "%s: %d fleet rows differ in the review seed", q, missing)
	}
}

// reviewRows is a query's rows, each rendered as one string, counted.
func reviewRows(t *testing.T, s *Seeder, query string) map[string]int {
	t.Helper()
	rows, err := s.db.QueryContext(s.ctx, query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	out := map[string]int{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		var b strings.Builder
		for _, v := range vals {
			if raw, ok := v.([]byte); ok {
				v = string(raw)
			}
			fmt.Fprintf(&b, "%v\x1f", v)
		}
		out[b.String()]++
	}
	require.NoError(t, rows.Err())
	return out
}
