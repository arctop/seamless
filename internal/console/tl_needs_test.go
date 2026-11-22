package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/demokit"
	"github.com/arctop/seamless/internal/store"
)

// tlNeedCards is the Overview's needs items for tasks (needs.go's
// needOpenGates and needClaims), by kind: each one's count and link.
func tlNeedCards(t *testing.T, mux *http.ServeMux) map[string]attnCard {
	t.Helper()
	var ov struct{ Attention []attnCard }
	getJSON(t, mux, "/console/?format=json", &ov)
	out := map[string]attnCard{}
	for _, c := range ov.Attention {
		switch c.Kind {
		case needGates, needLapsed, needUnclaimed:
			out[c.Kind] = c
		}
	}
	return out
}

// tlBadgeRe is the sidebar's Tasks badge.
var tlBadgeRe = regexp.MustCompile(`title="Gates waiting on you, lapsed claims and tasks in progress with no claim">(\d+)<`)

// tlNeedsMatch checks the Tasks list's Needs you against the needs model on
// mux: as many of each kind as the Overview's item counts (gates on the
// owner, lapsed claims, claimless starts), a lone item's link the row's
// task, and the sidebar's Tasks badge their sum.
func tlNeedsMatch(t *testing.T, mux *http.ServeMux) []taskRow {
	t.Helper()
	cards := tlNeedCards(t, mux)
	data := bkPage(t, mux, "/console/tasks?format=json")
	now := time.Now().UTC()
	byKind := map[string][]taskRow{}
	for _, r := range data.NeedsYou {
		need := taskNeed(r, now)
		require.NotEmpty(t, need, "%s waits on the owner", r.Title)
		kind := map[string]string{rowNeedGate: needGates, rowNeedLapsed: needLapsed, rowNeedClaimless: needUnclaimed}[need]
		byKind[kind] = append(byKind[kind], r)
	}
	for _, kind := range []string{needGates, needLapsed, needUnclaimed} {
		card, rows := cards[kind], byKind[kind]
		require.Equal(t, card.Count, len(rows), "%s: Needs you holds what the needs item counts", kind)
		if len(rows) == 1 {
			require.Equal(t, "/console/tasks/"+rows[0].ID, card.Href, "%s: the lone item is the row's task", kind)
		}
	}
	page := getHTMLBody(t, mux, "/console/tasks")
	m := tlBadgeRe.FindStringSubmatch(page)
	require.NotNil(t, m, "the Tasks badge")
	require.Equal(t, strconv.Itoa(len(data.NeedsYou)), m[1], "the badge counts Needs you")
	ids := make([]string, 0, len(data.NeedsYou))
	for _, r := range data.NeedsYou {
		ids = append(ids, r.ID)
	}
	require.Equal(t, ids, tlGroupIDs(page, "needs"), "the page draws the JSON's group")
	return data.NeedsYou
}

// The Tasks list on the review fixture (demoseed -review), seeded once.
func TestTlOnTheReviewFixture(t *testing.T) {
	mux, _, fx := scReview(t)
	t.Run("needs you matches the needs model", func(t *testing.T) { tlNeedsYouMatchesTheNeedsModel(t, mux) })
	t.Run("every state in its group", func(t *testing.T) { tlReviewStatesInTheirGroups(t, mux, fx) })
}

// Needs you is the needs model's task items (needs.go needsFor, through
// ownerGateOpen and looseClaim): on the review fixture every gate on the
// owner, lapsed claim and claimless start, plan steps included though the
// default list leaves the other steps out, and nothing else.
func tlNeedsYouMatchesTheNeedsModel(t *testing.T, mux *http.ServeMux) {
	rows := tlNeedsMatch(t, mux)
	require.NotEmpty(t, rows)
	steps := 0
	for _, r := range rows {
		if r.Plan != "" {
			steps++
		}
	}
	require.Positive(t, steps, "the review fixture's plan steps that wait on the owner are among them")
}

// The edges of the needs model on a fixture of its own: a gate on the
// owner's class (owner:<who>), any case, in progress or open; a step whose
// own gate names the owner; a step only a gate holds is no item (its gate
// is); a gate on someone else, a live claim whose holder went quiet, and a
// gate someone started are no claim items.
func TestTlNeedsYouEdges(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	live, lapsed := now.Add(10*time.Minute), now.Add(-10*time.Minute)
	gate := tlTask(t, db, "example-ops", "", "class gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "OWNER:legal", What: "sign"}
	})
	tlTask(t, db, "example-ops", "", "behind the class gate", func(k *core.Task) { k.DependsOn = []string{gate.ID} })
	tlTask(t, db, "example-ops", "", "started gate", func(k *core.Task) {
		k.Kind, k.Status, k.Gate = core.TaskKindGate, core.TaskInProgress, core.TaskGate{Who: "owner", What: "decide"}
	})
	tlTask(t, db, "example-ops", "", "own owner gate", func(k *core.Task) { k.Gate = core.TaskGate{Who: "owner", What: "x"} })
	tlTask(t, db, "example-ops", "", "gate on bob", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "bob", What: "y"}
	})
	tlTask(t, db, "example-ops", "", "quiet holder", func(k *core.Task) {
		k.Status, k.ClaimedBy, k.LeaseExpiresAt = core.TaskInProgress, "gone-session", &live
	})
	tlTask(t, db, "example-ops", "", "lapsed one", func(k *core.Task) {
		k.Status, k.ClaimedBy, k.LeaseExpiresAt = core.TaskInProgress, "gone-session", &lapsed
	})
	tlTask(t, db, "example-ops", "ship-plan", "claimless step", func(k *core.Task) { k.Status = core.TaskInProgress })
	rows := tlNeedsMatch(t, mux)
	require.Equal(t, []string{"class gate", "started gate", "own owner gate", "lapsed one", "claimless step"}, bkTitles(rows))

	// A filter narrows Needs you as it narrows the rest.
	data := bkPage(t, mux, "/console/tasks?format=json&q=step")
	require.Equal(t, []string{"claimless step"}, bkTitles(data.NeedsYou))
	data = bkPage(t, mux, "/console/tasks?format=json&project=global")
	require.Empty(t, data.NeedsYou)
	require.NotNil(t, data.NeedsYou, "an empty group is [] in the JSON")
}

// A locked project's tasks are absent from the list, Needs you, the counts
// and the reads the list adds (what a gate holds, the recently closed), as
// from the rails: the request's handle hides them.
func TestTlLockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	closed := now.Add(-time.Hour)
	for _, task := range []core.Task{
		{ID: mustID(t), ProjectSlug: lockedSlug, Title: "ledger gate " + lockedSecret, Status: core.TaskOpen,
			Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "owner", What: "sign " + lockedSecret}},
		{ID: mustID(t), ProjectSlug: lockedSlug, Title: "ledger claimless " + lockedSecret, Status: core.TaskInProgress},
		{ID: mustID(t), ProjectSlug: lockedSlug, PlanSlug: lc.plan, Title: "ledger step " + lockedSecret, Status: core.TaskInProgress},
		{ID: mustID(t), ProjectSlug: lockedSlug, Title: "ledger closed " + lockedSecret, Status: core.TaskDone, ClosedAt: &closed},
	} {
		task.CreatedAt, task.UpdatedAt = now, now
		require.NoError(t, store.CreateTask(ctx, lc.db, task))
	}
	gate := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: gate, ProjectSlug: openSlug, Title: "ops gate",
		Status: core.TaskOpen, Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "owner", What: "approve"}, CreatedAt: now, UpdatedAt: now}))
	// A locked task depends on the open gate: what the gate holds counts it
	// on the main handle only.
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: mustID(t), ProjectSlug: lockedSlug, Title: "ledger behind " + lockedSecret,
		Status: core.TaskOpen, DependsOn: []string{gate}, CreatedAt: now, UpdatedAt: now}))
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, path)
		return rr.Body.String()
	}
	for _, path := range []string{"/console/tasks", "/console/tasks?steps=1", "/console/tasks?format=json", "/console/tasks?format=json&steps=1&closed=all"} {
		body := get(path)
		require.NotContains(t, body, lockedSecret, path)
		require.NotContains(t, body, "ledger", path)
	}
	page := get("/console/tasks")
	require.Equal(t, []string{gate}, tlGroupIDs(page, "needs"))
	require.NotContains(t, tlRow(t, page, gate), "holds", "the locked task it holds is not counted")
	var data tasksData
	require.NoError(t, json.Unmarshal([]byte(get("/console/tasks?format=json")), &data))
	require.Zero(t, data.ClosedRecent)
	require.Zero(t, data.ClosedTotal)
	require.Len(t, data.NeedsYou, 1)
}

// On the review fixture (demoseed -review) every task state UX 01 seeds
// shows in its group with its line: a live claim in progress with its
// holder and lease, a lapsed claim and a claimless start in Needs you with
// Release claim and Mark done, a blocked task with its first blocker, a step
// whose own gate waits on the owner and a gate two steps depend on in Needs
// you with Mark as handled, those two steps blocked by the gate, and the
// closed tasks under Recently closed, the last week's, or a link away.
func tlReviewStatesInTheirGroups(t *testing.T, mux *http.ServeMux, fx demokit.ReviewFixture) {
	page := getHTMLBody(t, mux, "/console/tasks")
	item := func(state demokit.ReviewState) demokit.ReviewItem {
		t.Helper()
		it, ok := fx.Item(state)
		require.True(t, ok, state)
		return it
	}
	in := func(group, id string) string {
		t.Helper()
		require.Contains(t, tlGroupIDs(page, group), id, "%s in %s", id, group)
		return tlRow(t, page, id)
	}
	live := in("inprogress", item(demokit.ReviewTaskClaimLive).IDs[0])
	require.Contains(t, live, " left</time>")
	require.NotContains(t, live, "data-task-act")
	lapsed := in("needs", item(demokit.ReviewTaskClaimLapsed).IDs[0])
	require.Contains(t, lapsed, ">claim lapsed ")
	require.Contains(t, lapsed, `data-task-act="release"`)
	claimless := in("needs", item(demokit.ReviewTaskClaimless).IDs[0])
	require.Contains(t, claimless, "in progress, but nobody holds a claim")
	require.Contains(t, claimless, `data-task-act="done"`)
	blocked := item(demokit.ReviewTaskBlocked)
	require.Contains(t, in("blocked", blocked.IDs[0]), `data-first-blocker="`+blocked.IDs[1]+`"`)
	gated := in("needs", item(demokit.ReviewTaskGated).IDs[0])
	require.Contains(t, gated, "waits on you: ")
	require.Contains(t, gated, `data-task-act="handle"`)
	gate := item(demokit.ReviewTaskGate)
	require.Contains(t, in("needs", gate.IDs[0]), "holds 2 tasks")
	for _, step := range gate.IDs[1:] {
		require.Contains(t, in("blocked", step), `data-first-blocker="`+gate.IDs[0]+`"`)
	}
	recent := tlGroupIDs(page, "closed")
	all := tlGroupIDs(getHTMLBody(t, mux, "/console/tasks?closed=all"), "closed")
	shown := 0
	for _, id := range item(demokit.ReviewTaskClosed).IDs {
		require.Contains(t, all, id)
		if slices.Contains(recent, id) {
			shown++
		}
	}
	require.Positive(t, shown, "the last week's closes are folded under Recently closed")
	require.Contains(t, page, "closed tasks", "the older ones are a link away")
}
