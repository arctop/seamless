package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// moveReq is a move from home into target by the fixture conversation, holding
// the target and holds once moved, writing to the target alone.
func moveReq(target, home string, holds ...string) MoveRequest {
	carry, err := CarryOf(home, append([]string{target}, holds...), []string{target})
	if err != nil {
		panic(err)
	}
	return MoveRequest{
		Project: target, From: home, Carry: carry, Lease: time.Hour, TTL: 10 * time.Minute,
		ConversationClient: "claude-code", ConversationID: "ext-conv-1",
		SessionID: "01SESSHOME", SessionName: "cc/home", CWD: "/work/" + home, Repo: home,
		Model: "claude-opus-5-5", Reason: "work on the ledger",
	}
}

// moveApproved files req and approves it into the child session 01SESSCHILD.
func moveApproved(t *testing.T, db *sql.DB, req MoveRequest) (AccessApproval, Move) {
	t.Helper()
	ctx := context.Background()
	a, err := MoveRequestCreate(ctx, db, req, migration039Now)
	require.NoError(t, err)
	m, err := MoveApprove(ctx, db, a.ID, a.Challenge, "bearer-key", "01SESSCHILD", migration039Now)
	require.NoError(t, err)
	return a, m
}

func TestMoves_RequestValidates(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*MoveRequest){
		"no project":           func(r *MoveRequest) { r.Project = "" },
		"into its home":        func(r *MoveRequest) { r.From = r.Project },
		"zero lease":           func(r *MoveRequest) { r.Lease = 0 },
		"no ttl":               func(r *MoveRequest) { r.TTL = 0 },
		"no session":           func(r *MoveRequest) { r.SessionID = "" },
		"a session alone":      func(r *MoveRequest) { r.ConversationClient, r.ConversationID = "", "" },
		"half a conversation":  func(r *MoveRequest) { r.ConversationClient = "" },
		"a slash in a client":  func(r *MoveRequest) { r.ConversationClient = "claude/code" },
		"a process, no pid":    func(r *MoveRequest) { r.ProcessKey = "boot-1:7:7" },
		"no carry":             func(r *MoveRequest) { r.Carry = Carry{} },
		"another home":         func(r *MoveRequest) { r.Carry.Home = "example-hr" },
		"not holding it":       func(r *MoveRequest) { r.Carry.Holds = []string{"example-legal"} },
		"writing elsewhere":    func(r *MoveRequest) { r.Carry.WriteTo = []string{"example-finance", "example-ops"} },
		"writing to the scope": func(r *MoveRequest) { r.Carry.WriteTo = []string{""} },
		"a long reason":        func(r *MoveRequest) { r.Reason = strings.Repeat("r", MaxAccessReasonRunes+1) },
	} {
		req := moveReq("example-finance", "example-ops")
		mutate(&req)
		_, err := MoveRequestCreate(ctx, db, req, migration039Now)
		require.ErrorIs(t, err, ErrInvalidAccessRequest, name)
	}

	// From the global scope, holding what it read before, unordered and twice.
	req := moveReq("example-vault", "", "example-legal", "example-legal")
	req.Carry.Holds = []string{"example-vault", "example-legal", "example-legal"}
	a, err := MoveRequestCreate(ctx, db, req, migration039Now)
	require.NoError(t, err)
	require.Equal(t, AccessKindMove, a.Kind)
	require.Equal(t, ApprovalPending, a.Status)
	require.Equal(t, "example-vault", a.Project)
	require.Empty(t, a.FromProject)
	require.Equal(t, Carry{Home: "", Holds: []string{"example-legal", "example-vault"}, WriteTo: []string{"example-vault"}}, a.Carry,
		"the carry is stored normalized")
	require.Equal(t, migration039Conversation(t), a.Holder())
	require.Equal(t, migration039Now.Add(10*time.Minute), a.ExpiresAt)

	// Under peer_auth the process is the holder, whatever else the row names.
	peer := moveReq("example-finance", "example-ops")
	peer.ProcessKey, peer.PeerPID, peer.PeerUID = "boot-1:4242:7", 4242, 501
	p, err := MoveRequestCreate(ctx, db, peer, migration039Now)
	require.NoError(t, err)
	require.Equal(t, HolderProcess, p.Holder().Kind())
}

func TestMoves_ApproveSingleUseChallengeBound(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a, err := MoveRequestCreate(ctx, db, moveReq("example-vault", "example-finance", "example-finance", "example-legal"), migration039Now)
	require.NoError(t, err)

	_, err = MoveApprove(ctx, db, a.ID, make([]byte, AccessChallengeBytes), "owner", "01SESSCHILD", migration039Now)
	require.ErrorIs(t, err, ErrApprovalChallenge)
	_, err = MoveApprove(ctx, db, a.ID, a.Challenge, "", "01SESSCHILD", migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a decision names its proof")
	_, err = MoveApprove(ctx, db, a.ID, a.Challenge, "owner", "", migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a move names its child")
	_, err = MoveApprove(ctx, db, a.ID, a.Challenge, "owner", "01SESSHOME", migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "the child is another session")
	_, err = MoveApprove(ctx, db, "01NOSUCHMOVE", a.Challenge, "owner", "01SESSCHILD", migration039Now)
	require.ErrorIs(t, err, ErrApprovalNotFound)

	later := migration039Now.Add(time.Minute)
	m, err := MoveApprove(ctx, db, a.ID, a.Challenge, "credential-1", "01SESSCHILD", later)
	require.NoError(t, err)
	require.Equal(t, Move{ID: m.ID, ApprovalID: a.ID, Holder: migration039Conversation(t), FromSessionID: "01SESSHOME",
		ToSessionID: "01SESSCHILD", FromProject: "example-finance", ToProject: "example-vault", DecidedBy: "credential-1",
		MovedAt: later}, m)
	stored, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalApproved, stored.Status)
	require.Equal(t, "credential-1", stored.DecidedBy)
	require.Empty(t, stored.LeaseID, "a move is no lease")

	// The flows its carry listed: each held project into the project moved into.
	flows, err := FlowsForHolder(ctx, db, []Holder{m.Holder})
	require.NoError(t, err)
	var pairs []FlowPair
	for _, f := range flows {
		require.Equal(t, a.ID, f.ApprovalID)
		pairs = append(pairs, FlowPair{From: f.From, To: f.To})
	}
	require.Equal(t, []FlowPair{{From: "example-finance", To: "example-vault"}, {From: "example-legal", To: "example-vault"}}, pairs)

	_, err = MoveApprove(ctx, db, a.ID, a.Challenge, "credential-1", "01SESSCHILD2", later)
	require.ErrorIs(t, err, ErrApprovalNotPending, "single use")
	moves, err := MovesForHolder(ctx, db, []Holder{m.Holder})
	require.NoError(t, err)
	require.Len(t, moves, 1)

	// Past its deadline a request is never approved; a denied one neither.
	b, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops"), migration039Now)
	require.NoError(t, err)
	_, err = MoveApprove(ctx, db, b.ID, b.Challenge, "owner", "01SESSCHILD3", migration039Now.Add(time.Hour))
	require.ErrorIs(t, err, ErrApprovalExpired)
	expired, err := AccessApprovalByID(ctx, db, b.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalExpired, expired.Status)
	c, err := MoveRequestCreate(ctx, db, moveReq("example-finance", "example-ops"), migration039Now)
	require.NoError(t, err)
	require.NoError(t, DenyAccess(ctx, db, c.ID, "owner", migration039Now))
	_, err = MoveApprove(ctx, db, c.ID, c.Challenge, "owner", "01SESSCHILD4", migration039Now)
	require.ErrorIs(t, err, ErrApprovalNotPending)
}

// A move request is never a lease, never a grant, and never answers a lock's
// lookups; no other request is ever approved as a move.
func TestMoves_NeverALeaseOrAGrant(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	req := moveReq("example-finance", "example-ops")
	req.ProcessKey, req.PeerPID, req.PeerUID = fenceProc.Key(), fenceProc.PID, 501
	a, err := MoveRequestCreate(ctx, db, req, migration039Now)
	require.NoError(t, err)

	_, err = ApproveAccessFor(ctx, db, a.ID, a.Challenge, "owner", 0, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	_, err = ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "owner", 0, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	_, err = GrantAccessApprove(ctx, db, a.ID, a.Challenge, "owner", 0, GrantAccessWrite, migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	require.Zero(t, migration039Count(t, db, "access_leases"))
	require.Zero(t, migration039Count(t, db, "access_grants"))

	_, ok, err := PendingAccessApproval(ctx, db, fenceProc.Key(), "example-finance", migration039Now)
	require.NoError(t, err)
	require.False(t, ok, "a lock's pending lookup never returns a move request")
	_, ok, err = LastAccessApproval(ctx, db, fenceProc.Key(), "example-finance")
	require.NoError(t, err)
	require.False(t, ok, "nor its last-request lookup")
	_, ok, err = PendingGrantRequest(ctx, db, fenceProc.Key(), "", "example-finance", "example-ops", migration039Now)
	require.NoError(t, err)
	require.False(t, ok, "nor a grant's")

	lock, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), migration039Now)
	require.NoError(t, err)
	_, err = MoveApprove(ctx, db, lock.ID, lock.Challenge, "owner", "01SESSCHILD", migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	g, err := CreateGrantRequest(ctx, db, grantReq("S1"), migration039Now)
	require.NoError(t, err)
	_, err = MoveApprove(ctx, db, g.ID, g.Challenge, "owner", "01SESSCHILD", migration039Now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	require.Zero(t, migration039Count(t, db, "access_moves"))

	// The lock's own lookups still find the lock's request beside the move.
	got, ok, err := PendingAccessApproval(ctx, db, fenceProc.Key(), "example-finance", migration039Now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, lock.ID, got.ID)
}

func TestMoves_ForHolder(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	_, conv := moveApproved(t, db, moveReq("example-finance", "example-ops"))

	peer := moveReq("example-legal", "example-ops")
	peer.ProcessKey, peer.PeerPID, peer.PeerUID = "boot-1:200:2000", 200, 501
	pa, err := MoveRequestCreate(ctx, db, peer, migration039Now)
	require.NoError(t, err)
	byProc, err := MoveApprove(ctx, db, pa.ID, pa.Challenge, "daemon-account", "01SESSCHILDP", migration039Now.Add(time.Second))
	require.NoError(t, err)

	got, err := MovesForHolder(ctx, db, []Holder{migration039Conversation(t)})
	require.NoError(t, err)
	require.Equal(t, []Move{conv}, got)

	chain := []Holder{}
	for _, k := range []string{"boot-1:300:3000", "boot-1:200:2000"} {
		h, err := HolderOfProcess(k)
		require.NoError(t, err)
		chain = append(chain, h)
	}
	got, err = MovesForHolder(ctx, db, chain)
	require.NoError(t, err)
	require.Equal(t, []Move{byProc}, got, "a process chain finds the move of its anchoring process, never the conversation's")

	other, err := HolderOfConversation("claude-code", "ext-other")
	require.NoError(t, err)
	got, err = MovesForHolder(ctx, db, []Holder{other, {}})
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = MovesForHolder(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, got)

	long := make([]Holder, 0, access.MaxChainHops+1)
	for i := range access.MaxChainHops + 1 {
		h, err := HolderOfSession("01S" + strings.Repeat("x", i+1))
		require.NoError(t, err)
		long = append(long, h)
	}
	_, err = MovesForHolder(ctx, db, long)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a longer list did not come from a request")
}
