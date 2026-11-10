package retrieve

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The briefing names no blocker: its ready line lists the briefing project's
// own ready tasks, and its plan lines are counts. A task in a locked or
// confidential project that blocks this project's work changes those counts
// (volume, not content) and appears nowhere by title or id.
func TestBriefing_BlockerInFencedProjectIsNeverNamed(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	const (
		ops = "example-ops"
		fin = "example-finance"
	)
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/ops":"`+ops+`"}`))
	setIsolation(t, db, ops, core.IsolationOpen)
	setIsolation(t, db, fin, core.IsolationConfidential)
	now := time.Now().UTC()
	newTask := func(project, plan, title string, deps ...string) string {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, PlanSlug: plan, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	const secret = "reconcile the quarter ledger"
	blocker := newTask(fin, "", secret)
	newTask(ops, "", "ops waits on finance", blocker)
	newTask(ops, "", "ops is ready")
	newTask(ops, "ops-rollout", "rollout waits on finance", blocker)
	newTask(ops, "ops-rollout", "rollout step two")

	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{fin}},
		func(n string) (access.Account, error) {
			if n == "owner" {
				return access.Account{UID: 501, Name: "owner"}, nil
			}
			return access.Account{}, fmt.Errorf("no account %s", n)
		})
	require.NoError(t, err)

	svc := New(db, nil, budgets(), nil)
	for name, c := range map[string]context.Context{
		"peer_auth off":             ctx,
		"the owner, finance locked": access.WithView(ctx, policy.OwnerView()),
	} {
		text, _, err := svc.Briefing(c, BriefingInput{CWD: "/work/ops"})
		require.NoError(t, err, name)
		require.NotContains(t, text, secret, name)
		require.NotContains(t, text, blocker, name)
		require.Contains(t, text, "Ready tasks (1):\n- ops is ready\n", name)
		require.Contains(t, text, "- ops-rollout -- 0/2 done, 1 claimable, 0 in flight", name)
	}
}
