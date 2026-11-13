package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The approvals of a move and of a leased grant (approvalkind.go): one owner
// decision, one transaction, and every lookup that expects one active
// ambient row per conversation still finding exactly one.

// moveHome opens the fixture conversation's ambient row in home, stamped with
// the agent process "agent-1", as the SessionStart hook does, with findings.
func moveHome(t *testing.T, db *sql.DB, home, findings string) core.Session {
	t.Helper()
	ctx := context.Background()
	s := core.Session{
		ID: "01SESSHOME", Name: "cc/ext-conv-1-abc", ProjectSlug: home, Status: core.SessionActive, Findings: findings,
		ExternalSessionID: "ext-conv-1", ExternalClient: "claude-code", CWD: "/work/" + home, Host: "laptop",
		Source: "startup", Model: "claude-opus-5-5", Ambient: true,
		Metadata:  map[string]any{"claude_session_id": "ext-conv-1", "external_client": "claude-code"},
		CreatedAt: migration039Now, UpdatedAt: migration039Now,
	}
	require.NoError(t, CreateSession(ctx, db, s))
	require.NoError(t, SetSessionAgentProcess(ctx, db, s.ID, "agent-1"))
	return s
}

func agentProcessOf(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var p string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT agent_process FROM sessions WHERE id = ?`, id).Scan(&p))
	return p
}

func TestMoveApproveWith_TheChildIsTheConversationsOneAmbientRow(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen,
		"example-finance": core.IsolationConfidential, "example-legal": core.IsolationConfidential} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, SetProjectIsolation(ctx, db, slug, state))
	}
	left := moveHome(t, db, "example-ops", "")
	a, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops", "example-legal"), migration039Now)
	require.NoError(t, err)

	// The identity's ambient index admits one ambient row, whatever its
	// status: the child could never be inserted beside the row it leaves.
	clash := core.Session{ID: "01SESSCLASH", Name: "cc/clash", ProjectSlug: "example-finance", Status: core.SessionActive,
		ExternalSessionID: "ext-conv-1", ExternalClient: "claude-code", Ambient: true, CreatedAt: migration039Now, UpdatedAt: migration039Now}
	require.ErrorIs(t, CreateSession(ctx, db, clash), ErrSessionIdentityExists, "why the flip comes first")

	later := migration039Now.Add(time.Minute)
	done, err := MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, later)
	require.NoError(t, err)
	require.Nil(t, done.Lease, "no lock, no lease")

	child := done.Child
	require.Equal(t, "example-finance", child.ProjectSlug)
	require.Equal(t, left.ID, child.ParentSessionID)
	require.Equal(t, "cc/ext-conv-1-abc@example-finance", child.Name)
	require.True(t, child.Ambient)
	require.Equal(t, core.SessionActive, child.Status)
	require.Equal(t, "agent-1", agentProcessOf(t, db, child.ID), "stamped with the agent process")
	stored, ok, err := SessionByID(ctx, db, child.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, left.ID, stored.ParentSessionID)
	require.Equal(t, "ext-conv-1", stored.ExternalSessionID)
	require.Equal(t, "claude-code", stored.ExternalClient)
	require.Equal(t, a.ID, stored.Metadata["move_approval"])

	// The session it left ends, metadata only, and is no longer ambient.
	gone, ok, err := SessionByID(ctx, db, left.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, gone.Ambient)
	require.Equal(t, core.SessionCompleted, gone.Status)
	require.Equal(t, MovePointer("example-finance", child.Name), gone.Findings)
	require.Equal(t, child.ID, gone.Metadata["moved_to"])
	require.Empty(t, agentProcessOf(t, db, left.ID), "the process runs the child now")

	// Every lookup that expects one ambient row per conversation finds the
	// child, and only it.
	amb, found, err := AmbientSessionByExternalIdentity(ctx, db, "claude-code", "ext-conv-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, child.ID, amb.ID)
	byProc, err := ActiveAmbientByAgentProcess(ctx, db, "laptop", "agent-1")
	require.NoError(t, err)
	require.Len(t, byProc, 1)
	require.Equal(t, child.ID, byProc[0].ID)
	active, err := ActiveSessionsByExternalIdentity(ctx, db, "claude-code", "ext-conv-1")
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, child.ID, active[0].ID)

	// P0's routing sends the conversation's own record to the child, in the
	// project it moved into: its ambient row is there.
	dest, err := RecordDestinationOf(ctx, db, "claude-code", "ext-conv-1")
	require.NoError(t, err)
	require.Equal(t, RecordDestAmbient, dest.Kind)
	require.Equal(t, child.ID, dest.Ambient.ID)
	require.Equal(t, []string{"example-finance"}, dest.Fenced)

	// The move row, the flows the carry listed, and the request approved.
	require.Equal(t, child.ID, done.Move.ToSessionID)
	require.Equal(t, left.ID, done.Move.FromSessionID)
	moves, err := MovesForHolder(ctx, db, []Holder{migration039Conversation(t)})
	require.NoError(t, err)
	require.Len(t, moves, 1)
	flows, err := FlowsForHolder(ctx, db, []Holder{migration039Conversation(t)})
	require.NoError(t, err)
	require.Len(t, flows, 1)
	require.Equal(t, FlowPair{From: "example-legal", To: "example-finance"}, FlowPair{From: flows[0].From, To: flows[0].To})
	row, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalApproved, row.Status)
	require.Empty(t, row.LeaseID)

	_, err = MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, later)
	require.ErrorIs(t, err, ErrApprovalNotPending, "single use")

	// A resume or compact from the repository it left keeps the child home.
	resumed, found, err := ReactivateAmbientSession(ctx, db, "claude-code", "ext-conv-1", "example-ops", later.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, child.ID, resumed.ID)
	require.Equal(t, "example-finance", resumed.ProjectSlug, "a move's child is never re-scoped")
}

// A session with no parent still re-scopes on resume, as it always did.
func TestReactivateAmbientSession_RescopesASessionWithoutAParent(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	moveHome(t, db, "example-ops", "")
	got, found, err := ReactivateAmbientSession(ctx, db, "claude-code", "ext-conv-1", "example-docs", migration039Now)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "example-docs", got.ProjectSlug)
}

func TestMoveApproveWith_KeepsTheAgentsOwnFindings(t *testing.T) {
	for name, tc := range map[string]struct{ findings, want string }{
		"none":            {"", MovePointer("example-finance", "cc/ext-conv-1-abc@example-finance")},
		"the sentinel":    {core.FindingNoSummary, MovePointer("example-finance", "cc/ext-conv-1-abc@example-finance")},
		"the agent's own": {"Shipped the docs fix.\n", "Shipped the docs fix.\n\n" + MovePointer("example-finance", "cc/ext-conv-1-abc@example-finance")},
	} {
		t.Run(name, func(t *testing.T) {
			db := leaseDB(t)
			ctx := context.Background()
			moveHome(t, db, "example-ops", tc.findings)
			a, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops"), migration039Now)
			require.NoError(t, err)
			done, err := MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, migration039Now)
			require.NoError(t, err)
			require.Equal(t, tc.want, done.Left.Findings)
			gone, _, err := SessionByID(ctx, db, done.Left.ID)
			require.NoError(t, err)
			require.Equal(t, tc.want, gone.Findings)
		})
	}
}

// Under peer_auth a locked target's move carries its lease, in the same
// approval: bound to the process, the child session and the request.
func TestMoveApproveWith_LeaseForALockedTarget(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	moveHome(t, db, "example-ops", "")
	req := moveReq("example-finance", "example-ops")
	req.ProcessKey, req.PeerPID, req.PeerUID = "boot-1:4242:7", 4242, 501
	a, err := MoveRequestCreate(ctx, db, req, migration039Now)
	require.NoError(t, err)

	_, err = MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "daemon-account", Lease: 1500 * time.Millisecond}, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a lease is whole seconds")
	done, err := MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "daemon-account", Lease: time.Hour}, migration039Now)
	require.NoError(t, err)
	require.NotNil(t, done.Lease)
	require.Equal(t, done.Child.ID, done.Lease.SessionID, "the child's end revokes it")
	require.Equal(t, "boot-1:4242:7", done.Lease.ProcessKey)
	live, err := ActiveAccessLeasesFor(ctx, db, []string{"boot-1:4242:7"}, migration039Now)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "example-finance", live[0].Project)
	require.Equal(t, migration039Now.Add(time.Hour), live[0].ExpiresAt)
	row, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, live[0].ID, row.LeaseID)
	n, err := RevokeAccessLeasesForSession(ctx, db, done.Child.ID, LeaseRevokedSessionEnded, migration039Now)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	// A conversation's move (no process) carries no lease.
	db2 := leaseDB(t)
	moveHome(t, db2, "example-ops", "")
	b, err := MoveRequestCreate(ctx, db2, moveReq("example-finance", "example-ops"), migration039Now)
	require.NoError(t, err)
	_, err = MoveApproveWith(ctx, db2, MoveDecision{ID: b.ID, Challenge: b.Challenge, DecidedBy: "bearer-key", Lease: time.Hour}, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	still, err := AccessApprovalByID(ctx, db2, b.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalPending, still.Status, "a refused approval changes nothing")
}

// The session a move leaves must still be the conversation's ambient row,
// in the home the request leaves; anything else changes nothing.
func TestMoveApproveWith_RefusesASessionItCannotLeave(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, db *sql.DB)
	}{
		"gone": {func(t *testing.T, db *sql.DB) {}},
		"ended": {func(t *testing.T, db *sql.DB) {
			s := moveHome(t, db, "example-ops", "")
			s.Status = core.SessionCompleted
			require.NoError(t, UpdateSession(context.Background(), db, s))
		}},
		"moved elsewhere meanwhile": {func(t *testing.T, db *sql.DB) {
			s := moveHome(t, db, "example-ops", "")
			s.ProjectSlug = "example-docs"
			require.NoError(t, UpdateSession(context.Background(), db, s))
		}},
		"another conversation's": {func(t *testing.T, db *sql.DB) {
			s := moveHome(t, db, "example-ops", "")
			_, err := db.ExecContext(context.Background(), `UPDATE sessions SET claude_session_id = 'ext-conv-2' WHERE id = ?`, s.ID)
			require.NoError(t, err)
		}},
		"not ambient": {func(t *testing.T, db *sql.DB) {
			s := moveHome(t, db, "example-ops", "")
			_, err := db.ExecContext(context.Background(), `UPDATE sessions SET ambient = 0 WHERE id = ?`, s.ID)
			require.NoError(t, err)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			db := leaseDB(t)
			ctx := context.Background()
			tc.setup(t, db)
			a, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops"), migration039Now)
			require.NoError(t, err)
			_, err = MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, migration039Now)
			require.ErrorIs(t, err, ErrInvalidAccessRequest)
			row, err := AccessApprovalByID(ctx, db, a.ID)
			require.NoError(t, err)
			require.Equal(t, ApprovalPending, row.Status)
			moves, err := MovesForHolder(ctx, db, []Holder{migration039Conversation(t)})
			require.NoError(t, err)
			require.Empty(t, moves)
		})
	}
}

// MoveApprove, which P1 froze, still approves with a child its caller opens.
func TestMoveApprove_StillTakesTheCallersChild(t *testing.T) {
	db := leaseDB(t)
	_, m := moveApproved(t, db, moveReq("example-finance", "example-ops"))
	require.Equal(t, "01SESSCHILD", m.ToSessionID)
}

// A grant into a locked target carries its lease: the grant's length, the
// request's process, named on the request.
func TestGrantAccessApproveWith_Lease(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	carry, err := CarryOf("example-ops", []string{"example-ops", "example-finance"}, []string{"example-ops"})
	require.NoError(t, err)
	req := GrantRequest{Project: "example-finance", From: "example-ops", Lease: 8 * time.Hour, TTL: 10 * time.Minute,
		PeerUID: 501, PeerPID: 4242, ProcessKey: "boot-1:4242:7", SessionID: "01SESSHOME", Carry: carry}
	a, err := CreateGrantRequest(ctx, db, req, migration039Now)
	require.NoError(t, err)
	g, lease, err := GrantAccessApproveWith(ctx, db, GrantDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "daemon-account",
		Length: time.Hour, Access: GrantAccessWrite, Lease: true}, migration039Now)
	require.NoError(t, err)
	require.Equal(t, GrantAccessWrite, g.Access)
	require.NotNil(t, lease)
	require.Equal(t, g.ExpiresAt, lease.ExpiresAt)
	row, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, lease.ID, row.LeaseID)
	live, err := ActiveAccessLeasesFor(ctx, db, []string{"boot-1:4242:7"}, migration039Now)
	require.NoError(t, err)
	require.Len(t, live, 1)

	// Without a process there is nothing to lease to: refused, nothing written.
	plain := req
	plain.ProcessKey, plain.PeerPID = "", 0
	b, err := CreateGrantRequest(ctx, db, plain, migration039Now)
	require.NoError(t, err)
	_, _, err = GrantAccessApproveWith(ctx, db, GrantDecision{ID: b.ID, Challenge: b.Challenge, DecidedBy: "bearer-key",
		Access: GrantAccessRead, Lease: true}, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	still, err := AccessApprovalByID(ctx, db, b.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalPending, still.Status)
}

func TestRevokeAccessGrantsForHolder(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	conv := migration039Conversation(t)
	file := func(session string) AccessGrant {
		a, err := CreateGrantRequest(ctx, db, GrantRequest{Project: "example-finance", From: "example-ops", Lease: time.Hour,
			TTL: 10 * time.Minute, SessionID: session, ConversationClient: "claude-code", ConversationID: "ext-conv-1"}, migration039Now)
		require.NoError(t, err)
		g, err := ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "bearer-key", 0, migration039Now)
		require.NoError(t, err)
		return g
	}
	file("01SESSLINK")
	other, err := CreateGrantRequest(ctx, db, GrantRequest{Project: "example-legal", From: "example-ops", Lease: time.Hour,
		TTL: 10 * time.Minute, SessionID: "01SESSOTHER"}, migration039Now)
	require.NoError(t, err)
	_, err = ApproveAccessGrant(ctx, db, other.ID, other.Challenge, "bearer-key", 0, migration039Now)
	require.NoError(t, err)

	_, err = RevokeAccessGrantsForHolder(ctx, db, Holder{}, LeaseRevokedSessionEnded, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	n, err := RevokeAccessGrantsForHolder(ctx, db, conv, LeaseRevokedSessionEnded, migration039Now)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "the conversation's grant, whichever session asked")
	live, err := LiveAccessGrants(ctx, db, migration039Now)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "01SESSOTHER", live[0].SessionID, "another holder's grant stays")
}

// A move ends the session it leaves without releasing what that session
// claimed: a moved conversation closes a task it claimed before moving with a
// receipt (store.JudgeReceipt matches the task's claim against the move's
// FromSessionID), so the claim must still name the pre-move session.
func TestMoveApproveWith_KeepsThePreMoveSessionsClaims(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	left := moveHome(t, db, "example-ops", "")
	require.NoError(t, CreateTask(ctx, db, core.Task{ID: "01KTASKBUILDSTEP0000000001", ProjectSlug: "example-ops", Title: "build step",
		Status: core.TaskOpen, CreatedAt: migration039Now, UpdatedAt: migration039Now}))
	_, err := ClaimTask(ctx, db, "01KTASKBUILDSTEP0000000001", left.ID, time.Hour, migration039Now)
	require.NoError(t, err)
	a, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops"), migration039Now)
	require.NoError(t, err)
	done, err := MoveApproveWith(ctx, db, MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "bearer-key"}, migration039Now.Add(time.Minute))
	require.NoError(t, err)
	task, err := TaskByID(ctx, db, "01KTASKBUILDSTEP0000000001")
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, task.Status)
	require.Equal(t, left.ID, task.ClaimedBy, "still the pre-move session's")
	require.Equal(t, left.ID, done.Move.FromSessionID)
	require.True(t, task.ClaimLive(migration039Now.Add(time.Minute)))
}
