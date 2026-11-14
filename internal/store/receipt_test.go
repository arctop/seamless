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
	task := core.Task{ID: crID(t), ProjectSlug: project, Title: "build step", Body: "build and verify the release",
		Status: core.TaskInProgress, ClaimedBy: claimedBy, LeaseExpiresAt: &until, CreatedAt: crNow, UpdatedAt: crNow}
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

	// The reference: an item in the home, here the conversation's own session
	// there.
	ref := ReceiptRef{Project: crLegal, ID: c.amb.ID}
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

	next := rcTask(t, c, crOps, left.ID, time.Hour)
	for name, tc := range map[string]struct {
		r   Receipt
		why string
	}{
		"a task claimed by someone else": {Receipt{TaskID: other.ID, Status: core.TaskDone}, "claimed before it moved"},
		"an unclaimed task":              {Receipt{TaskID: unclaimed.ID, Status: core.TaskDone}, "claimed before it moved"},
		"another status":                 {Receipt{TaskID: next.ID, Status: core.TaskInProgress}, "done or dropped"},
		"no status":                      {Receipt{TaskID: next.ID}, "done or dropped"},
		"a text field": {Receipt{TaskID: next.ID, Status: core.TaskDone, Other: []string{"body"}},
			"a receipt carries no text: it sets the status alone, with at most one reference to an item in the " +
				"conversation's home, " + crLegal + ", so body is refused"},
		"several": {Receipt{TaskID: next.ID, Status: core.TaskDropped, Other: []string{"title", "project"}},
			"so title, project are refused"},
		"a reference outside the home": {Receipt{TaskID: next.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crOps, ID: left.ID}},
			"in the conversation's home"},
		"a reference that is text": {Receipt{TaskID: next.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal, ID: "finished the review"}},
			"one item id"},
		"a reference with no id": {Receipt{TaskID: next.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal}}, "one item id"},
		"a reference that names nothing": {Receipt{TaskID: next.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal, ID: crID(t)}},
			"names none there"},
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
		// The seal is the first word: a receipt that breaks another rule too
		// is told so before anything else.
		_, err = JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone, Other: []string{"body"}})
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
	claimed, err := ClaimedBeforeMove(tree, db, c.ref(), step)
	require.NoError(t, err)
	require.True(t, claimed, "the tree's move left the session that claimed it")
	_, err = JudgeReceipt(tree, db, c.ref(), Receipt{TaskID: locked.ID, Status: core.TaskDone})
	require.ErrorIs(t, err, access.ErrNotReachable, "a locked project is closed without a lease")
	owner := access.WithView(ctx, policy.OwnerView())
	_, err = JudgeReceipt(owner, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone})
	require.ErrorIs(t, err, ErrReceiptRefused, "the TCP key is no conversation")
	claimed, err = ClaimedBeforeMove(owner, db, c.ref(), step)
	require.NoError(t, err)
	require.False(t, claimed, "nor does it hold one's claims")
}

// The reference names an item in the home of any kind the conversation's
// record holds -- and only one that is there: an id of another project's
// item, or of none, is refused, so a reference cannot carry text dressed up as
// an id.
func TestJudgeReceipt_TheReferenceNamesAnItemInTheHome(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crOps)
	step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
	c.move(crLegal)
	at := core.FormatTime(crNow)

	memory, note, trial := crID(t), crID(t), crID(t)
	insertMemory(t, db, memory, "reference", "ledger-map", "d", crLegal, "b", at, "")
	insertNote(t, db, note, "review notes", "review-notes", crLegal, at)
	require.NoError(t, CreateTrial(ctx, db, core.Trial{ID: trial, Lab: "review", Title: "run", Outcome: core.OutcomePass,
		SessionID: c.amb.ID, ProjectSlug: crLegal, CreatedAt: crNow}))
	task := rcTask(t, c, crLegal, "", 0)
	for kind, id := range map[string]string{"memory": memory, "note": note, "trial": trial, "task": task.ID, "session": c.amb.ID} {
		_, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal, ID: id}})
		require.NoError(t, err, kind)
	}

	elsewhere := crID(t)
	insertNote(t, db, elsewhere, "ops notes", "ops-notes", crOps, at)
	_, err := JudgeReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone, Ref: ReceiptRef{Project: crLegal, ID: elsewhere}})
	require.ErrorIs(t, err, ErrReceiptRefused)
	require.ErrorContains(t, err, "a receipt's reference names an item in the conversation's home, "+crLegal+", and "+elsewhere+
		" names none there")
}

func TestParseReceiptRef(t *testing.T) {
	id := "01KTASKBUILDSTEP0000000001"
	got, err := ParseReceiptRef(" " + crLegal + ":" + id + " ")
	require.NoError(t, err)
	require.Equal(t, ReceiptRef{Project: crLegal, ID: id}, got)
	require.Equal(t, crLegal+":"+id, got.String(), "String renders the form it reads")
	lower, err := ParseReceiptRef(crLegal + ":01ktaskbuildstep0000000001")
	require.NoError(t, err)
	require.Equal(t, id, lower.ID, "a ULID reads in either case and is kept as Seamless mints it")
	for _, bad := range []string{"", "finished the review", crLegal, crLegal + ":", ":" + id, ":"} {
		_, err := ParseReceiptRef(bad)
		require.ErrorIs(t, err, ErrReceiptRefused, "%q", bad)
		require.ErrorContains(t, err, "written <project>:<id>", "%q", bad)
	}
	_, err = ParseReceiptRef("revenue was up twelve percent")
	require.NotContains(t, err.Error(), "twelve", "the text a receipt may not carry is not echoed")
}

// ClaimedBeforeMove names the class: the tasks whose claim a session the
// conversation left by a move holds, and no other.
func TestClaimedBeforeMove(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crOps)
	step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
	other := rcTask(t, c, crOps, crID(t), time.Hour)
	unclaimed := rcTask(t, c, crOps, "", 0)
	before, err := ClaimedBeforeMove(ctx, db, c.ref(), step)
	require.NoError(t, err)
	require.False(t, before, "nothing before the move")
	c.move(crLegal)
	for task, want := range map[*core.Task]bool{&step: true, &other: false, &unclaimed: false} {
		got, err := ClaimedBeforeMove(ctx, db, c.ref(), *task)
		require.NoError(t, err)
		require.Equal(t, want, got, task.ID)
	}
	got, err := ClaimedBeforeMove(ctx, db, ConversationRef{}, step)
	require.NoError(t, err)
	require.False(t, got, "no conversation holds nothing")
	stranger := crStart(t, db, crLegal)
	got, err = ClaimedBeforeMove(ctx, db, stranger.ref(), step)
	require.NoError(t, err)
	require.False(t, got, "another conversation's claim is not this one's")
}

// SendReceipt makes the receipt's one write as the pre-move session: the
// status, and the claim released with it. Title and body stay as they were.
func TestSendReceipt_ClosesTheStepAndReleasesTheClaim(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crOps)
	step := rcTask(t, c, crOps, c.amb.ID, time.Hour)
	left := c.move(crLegal)

	sent, err := SendReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDone,
		Ref: ReceiptRef{Project: crLegal, ID: c.amb.ID}}, crNow.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, left.ID, sent.Holder)
	require.Equal(t, core.TaskInProgress, sent.Update.From)
	got := sent.Update.Task
	require.Equal(t, core.TaskDone, got.Status)
	require.Empty(t, got.ClaimedBy, "the claim is released")
	require.Nil(t, got.LeaseExpiresAt)
	require.NotNil(t, got.ClosedAt)
	require.Equal(t, step.Title, got.Title)
	require.Equal(t, step.Body, got.Body)
	require.Equal(t, crOps, got.ProjectSlug)

	_, err = SendReceipt(ctx, db, c.ref(), Receipt{TaskID: step.ID, Status: core.TaskDropped}, crNow.Add(3*time.Minute))
	require.ErrorIs(t, err, ErrReceiptRefused, "closed, it holds no claim to close again")
}

// The receipt is written only while the claim it judged is the pre-move
// session's: one another agent took once the lease lapsed, or one the owner
// released, is no receipt's to close.
func TestSendReceipt_OnlyWhileThePreMoveSessionHoldsTheClaim(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crOps)
	taken := rcTask(t, c, crOps, c.amb.ID, -time.Minute)
	released := rcTask(t, c, crOps, c.amb.ID, time.Hour)
	c.move(crLegal)

	_, err := ClaimTask(ctx, db, taken.ID, crID(t), time.Hour, crNow)
	require.NoError(t, err)
	_, err = SendReceipt(ctx, db, c.ref(), Receipt{TaskID: taken.ID, Status: core.TaskDone}, crNow)
	require.ErrorIs(t, err, ErrReceiptRefused)
	require.ErrorContains(t, err, "claimed before it moved")

	_, err = ForceReleaseTask(ctx, db, released.ID, crNow)
	require.NoError(t, err)
	_, err = SendReceipt(ctx, db, c.ref(), Receipt{TaskID: released.ID, Status: core.TaskDone}, crNow)
	require.ErrorIs(t, err, ErrReceiptRefused)
	still, err := TaskByID(ctx, db, released.ID)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, still.Status, "the owner's release stands")
}

// IfClaimedBy is the precondition the receipt's write rides: the patch
// applies only while the named session holds the task's claim, live or
// lapsed, read inside the update's transaction.
func TestUpdateTask_IfClaimedBy(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	c := crStart(t, db, crOps)
	holder, someone := crID(t), crID(t)
	done := core.TaskDone
	mine := rcTask(t, c, crOps, holder, -time.Minute)
	theirs := rcTask(t, c, crOps, someone, -time.Minute)
	nobody := rcTask(t, c, crOps, "", 0)

	_, err := UpdateTask(ctx, db, theirs.ID, TaskPatch{Status: &done, IfClaimedBy: &holder}, holder, crNow)
	require.ErrorIs(t, err, ErrTaskClaimConflict)
	require.ErrorContains(t, err, "is no longer held by \""+holder+"\"")
	_, err = UpdateTask(ctx, db, nobody.ID, TaskPatch{Status: &done, IfClaimedBy: &holder}, holder, crNow)
	require.ErrorIs(t, err, ErrTaskClaimConflict)
	for _, id := range []string{theirs.ID, nobody.ID} {
		got, err := TaskByID(ctx, db, id)
		require.NoError(t, err)
		require.Equal(t, core.TaskInProgress, got.Status, "a refused precondition writes nothing")
	}
	got, err := UpdateTask(ctx, db, mine.ID, TaskPatch{Status: &done, IfClaimedBy: &holder}, holder, crNow)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, got.Status)
	require.Empty(t, got.ClaimedBy)
}
