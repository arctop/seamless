package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// Requests through a relay (plan networked-relay): the wire names no program,
// so the anchor rule cannot judge a relay's process. Nothing is filed for one,
// and the approve core never turns a stored one nobody named into a lease.

const judgeRelayBoot = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

// judgeRelay pairs a relay named name in f's database and returns its id.
func judgeRelay(t *testing.T, f *reqFixture, name string) string {
	t.Helper()
	ctx := context.Background()
	code, _, err := store.MintPairingCode(ctx, f.db, store.DeviceByConsole, f.now)
	require.NoError(t, err)
	d, err := store.PairDevice(ctx, f.db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: store.ApproverKeyFingerprint([]byte(name)), PublicKey: []byte(name),
		KeyHash: judgeHex("key:" + name), SecretHash: judgeHex("secret:" + name)}, f.now)
	require.NoError(t, err)
	return d.ID
}

// judgeHex is the hex SHA-256 of s: a stand-in key or secret hash.
func judgeHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// judgeRelayView is the owner's view through the relay, as the daemon's relay
// judge builds it, with the leases the store holds for its chain.
func judgeRelayView(t *testing.T, f *reqFixture, relay string, chain ...access.ProcessToken) access.View {
	t.Helper()
	p, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{lockFin, lockLeg},
		Relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501}}}, func(string) (access.Account, error) {
		return access.Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	v, ok := p.RelayChainView(relay, "example-laptop", 501, chain)
	require.True(t, ok)
	rows, err := store.ActiveAccessLeasesFor(context.Background(), f.db, v.Principal().ChainKeys(), f.now)
	require.NoError(t, err)
	return v.WithLeases(f.now, store.AccessLeases(rows))
}

func judgeRelayTok(pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: judgeRelayBoot}
}

// A relay's agent files nothing, for a lock's request, a read grant or a
// move alike: the server cannot tell its agent from a shell there.
func TestRequest_ARelaysProcessFilesNothing(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	relay := judgeRelay(t, f, "example-laptop")
	agentTok, bridgeTok := judgeRelayTok(2200), judgeRelayTok(2210)
	// Even with a session stamped with the relay's agent, as its SessionStart
	// hook stamps one.
	sess := f.session(t, "cc/relay", "/work/ops", agentTok)
	v := judgeRelayView(t, f, relay, bridgeTok, agentTok)

	_, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: v, SessionID: sess, Reason: "reconcile", Via: ViaRequest})
	require.ErrorIs(t, err, ErrRelayUnnamed)
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	_, err = g.RequestGrant(access.WithView(ctx, v), GrantInput{Project: confHR, SessionID: sess})
	require.ErrorIs(t, err, ErrRelayUnnamed)
	pending, err := store.PendingAccessApprovals(ctx, f.db, f.now)
	require.NoError(t, err)
	require.Empty(t, pending, "no request exists")

	// This machine's agent of the same pid and start files as before.
	local := f.request(t, lockFin, sess, judgeRelayTok(2210), agentTok)
	require.Equal(t, agentTok.Key(), local.ProcessKey)
}

// A relay's stored request whose program nobody named is never approved, by
// any proof; one whose program was named is judged as this machine's are.
func TestApprove_RefusesARelaysUnnamedRequest(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	relay := judgeRelay(t, f, "example-laptop")
	tok := judgeRelayTok(2200)
	tok.Relay = relay
	row, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockFin, Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: tok.PID, ProcessKey: tok.Key(),
	}, f.now)
	require.NoError(t, err)
	facts, err := f.svc.Facts(ctx, row.ID)
	require.NoError(t, err)
	_, err = f.svc.Approve(ctx, Decision{ID: row.ID, Duration: ForSession, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrRelayUnnamed)
	after, err := store.AccessApprovalByID(ctx, f.db, row.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalPending, after.Status, "refused before any proof: nothing decided")

	// A relay's request whose program was named is judged like this
	// machine's: approved, its lease names the relay.
	named, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{
		Project: lockFin, Kind: store.AccessKindRequest, Lease: 8 * time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: tok.PID, ProcessKey: tok.Key(), Program: "/Users/owner/.local/share/claude/versions/2.1.319",
	}, f.now)
	require.NoError(t, err)
	lease := f.approveDaemon(t, named.ID, ForSession)
	require.Equal(t, relay, lease.RelayID)
}
