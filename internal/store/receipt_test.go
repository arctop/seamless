package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// Receipts (D5): the content-free write a moved conversation may make to
// close a task its pre-move session claimed.

// rcTask is a task in project, claimed by claimedBy until lease from crNow.
func rcTask(t *testing.T, c *crConv, project, claimedBy string, lease time.Duration) core.Task {
	t.Helper()
	until := crNow.Add(lease)
	task := core.Task{ID: crID(t), ProjectSlug: project, Title: "build step", Status: core.TaskInProgress,
		ClaimedBy: claimedBy, LeaseExpiresAt: &until, CreatedAt: crNow, UpdatedAt: crNow}
	require.NoError(t, CreateTask(context.Background(), c.db, task))
	return task
}

func TestJudgeReceipt_ClosesWhatThePreMoveSessionClaimed(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crOps)
	step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
	other := rcTask(t, c, crOps, crID(t), time.Hour)
	unclaimed := rcTask(t, c, crOps, "", 0)
	left := c.move(crLegal)
	require.Equal(t, step.ClaimedBy, left.ID)

	// The clearance refuses every ordinary write into the home it left.
	require.False(t, crCanWrite(t, c.judged(ctx), db, crLegal, crOps))

	ref := ReceiptRef{Project: crLegal, ID: crID(t)}
	for _, status := range []core.TaskStatus{core.TaskDone, core.TaskDropped} {
		got, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: status, Ref: ref})
		require.NoError(t, err, status)
		require.Equal(t, step.ID, got.ID)
	}
	_, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
	require.NoError(t, err, "the reference is optional")

	// The one write it allows: the status, nothing else.
	r := Receipt{TaskID: step.ID, Status: core.TaskDone, Ref: ref}
	patch := r.Patch()
	require.Equal(t, TaskPatch{Status: &r.Status}, patch)
	done, err := UpdateTask(ctx, db, step.ID, patch, step.ClaimedBy, crNow)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, done.Status)
	require.Equal(t, step.Title, done.Title)
	require.Equal(t, crLegal+":"+ref.ID, ref.String())

	for name, tc := range map[string]struct {
		r   Receipt
		why string
	}{
		"a task claimed by someone else": {Receipt{TaskID: other.ID, Status: core.TaskDone}, "claimed before it moved"},
		"an unclaimed task":              {Receipt{TaskID: unclaimed.ID, Status: core.TaskDone}, "claimed before it moved"},
		"another status":                 {Receipt{TaskID: step.ID, Status: core.TaskInProgress}, "done or dropped"},
		"no status":                      {Receipt{TaskID: step.ID}, "done or dropped"},
		"a reference outside the home":   {Receipt{TaskID: step.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crOps, ID: crID(t)}}, "in the conversation's home"},
		"a reference that is text":       {Receipt{TaskID: step.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal, ID: "finished the review"}}, "one item id"},
		"a reference with no id":         {Receipt{TaskID: step.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal}}, "one item id"},
	} {
		_, err := JudgeReceipt(ctx, db, c.ref(), tc.r)
		require.ErrorIs(t, err, ErrReceiptRefused, name)
		require.ErrorContains(t, err, tc.why, name)
	}
	_, err = JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: crID(t), Status: core.TaskDone})
	require.ErrorIs(t, err, ErrTaskNotFound)
}

// A lapsed claim is still the pre-move session's until someone else takes
// it.
func TestJudgeReceipt_LapsedClaim(t *testing.T) {
	db := crDB(t)
	c := crStart(t, db, crOps)
	step := rcTask(t, c, crOps, c.amb.ID, -time.Minute)
	c.move(crLegal)
	_, err := JudgeReceipt(context.Background(), db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDropped})
	require.NoError(t, err)
}

func TestJudgeReceipt_Refusals(t *testing.T) {
	ctx := context.Background()

	t.Run("a sealed home sends none", func(t *testing.T) {
		db := crDB(t)
		c := crStart(t, db, crOps)
		step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
		c.move(crSafe)
		_, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
		require.ErrorIs(t, err, ErrReceiptRefused)
		require.ErrorContains(t, err, "sealed project "+crSafe+" sends no receipts")
	})

	t.Run("a conversation that never moved", func(t *testing.T) {
		db := crDB(t)
		c := crStart(t, db, crData)
		step := rcTask(t, c, crData, c.amb.ID, time.Hour)
		_, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
		require.ErrorIs(t, err, ErrReceiptRefused)
		require.ErrorContains(t, err, "claimed before it moved")
	})

	t.Run("no conversation", func(t *testing.T) {
		db := crDB(t)
		c := crStart(t, db, crOps)
		step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
		c.move(crLegal)
		_, err := JudgeReceipt(ctx, db, ConversationRef{}, Receipt{TaskID: step.ID, Status: core.TaskDone})
		require.ErrorIs(t, err, ErrReceiptRefused)
		other := crStart(t, db, crLegal)
		_, err = JudgeReceipt(ctx, db, other.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
		require.ErrorIs(t, err, ErrReceiptRefused, "another conversation never claimed it")
	})

	t.Run("never into a sealed project", func(t *testing.T) {
		db := crDB(t)
		c := crStart(t, db, crOps)
		step := rcTask(t, c, crVault, c.amb.ID, time.Hour)
		c.move(crLegal)
		_, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
		require.ErrorIs(t, err, ErrReceiptRefused)
		require.ErrorContains(t, err, "sealed project "+crVault)
	})
}

// Under peer_auth the conversation is the agent's tree: its moves are the
// process's, and the View has the first word on the task's project.
func TestJudgeReceipt_PeerAuth(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{crHR}}, func(n string) (access.Account, error) {
		return access.Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: crBoot}
	v, ok := policy.PeerChainView(501, []access.ProcessToken{agent})
	require.True(t, ok)
	tree := access.WithView(ctx, v)

	c := crStart(t, db, crOps)
	c.process, c.pid = agent.Key(), agent.PID
	step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
	locked := rcTask(t, c, crHR, c.amb.ID, time.Hour)
	c.move(crLegal)
	require.NoError(t, SetSessionAgentProcess(ctx, db, c.amb.ID, agent.AgentID()))

	_, err = JudgeReceipt(tree, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
	require.NoError(t, err)
	_, err = JudgeReceipt(tree, db, c.ref(), Receipt{TaskID: locked.ID, Status: core.TaskDone})
	require.ErrorIs(t, err, access.ErrNotReachable, "a locked project is closed without a lease")
	_, err = JudgeReceipt(access.WithView(ctx, policy.OwnerView()), db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
	require.ErrorIs(t, err, ErrReceiptRefused, "the TCP key is no conversation")
}
