package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// Wire version 2: a relay names each process's program, so the anchor rule
// judges a relay's chain as it judges this machine's, the stored request
// carries the program, and the facts say which relay asked.

const (
	relay2Claude = "/Users/owner/.local/share/claude/versions/2.1.319"
	relay2Seam   = "/Users/owner/.local/bin/seam"
)

// relay2View is the owner's view through the relay, naming each process of
// chain by programs (wire version 2; nil is version 1), with the leases the
// store holds for it.
func relay2View(t *testing.T, f *reqFixture, relay string, chain []access.ProcessToken, programs []string) access.View {
	t.Helper()
	p, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{lockFin, lockLeg},
		Relays: map[string]config.PeerRelay{"example-laptop": {OwnerUID: 501}}}, func(string) (access.Account, error) {
		return access.Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	v, ok := p.RelayChainViewNamed(relay, "example-laptop", 501, chain, programs)
	require.True(t, ok)
	rows, err := store.ActiveAccessLeasesFor(context.Background(), f.db, v.Principal().ChainKeys(), f.now)
	require.NoError(t, err)
	return v.WithLeases(f.now, store.AccessLeases(rows))
}

func TestRequestV2_ARelaysAgentFilesAndIsApprovedAsThisMachinesIs(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	relay := judgeRelay(t, f, "example-laptop")
	agentTok, bridgeTok, termTok := judgeRelayTok(2200), judgeRelayTok(2210), judgeRelayTok(2100)
	sess := f.session(t, "cc/relay2", "/work/ops", agentTok)
	chain := []access.ProcessToken{bridgeTok, agentTok, termTok}
	v := relay2View(t, f, relay, chain, []string{relay2Seam, relay2Claude, "/Applications/iTerm.app/Contents/MacOS/iTerm2"})

	a, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: v, SessionID: sess, Reason: "reconcile", Via: ViaRequest})
	require.NoError(t, err, "a relay that names its programs files as this machine's socket does")
	want := agentTok
	want.Relay = relay
	require.Equal(t, want.Key(), a.ProcessKey)
	require.Equal(t, relay2Claude, a.Program)

	facts, err := f.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "Claude Code 2.1.319", facts.Agent)
	require.Equal(t, WarnRelay, facts.Warnings[0].Code)
	require.Equal(t, "the agent runs on example-laptop, another computer: its relay, not this server, reports the pid, the uid "+
		"and the program", facts.Warnings[0].Text)
	require.LessOrEqual(t, len(facts.Warnings[0].Text), 140, "the Touch ID prompt shows a warning whole")

	lease := f.approveDaemon(t, a.ID, ForSession)
	require.Equal(t, relay, lease.RelayID)
	require.Equal(t, want.Key(), lease.ProcessKey)

	// The agent's grant (isolation.requests) is filed the same way.
	g := NewGrants(GrantsConfig{Approvals: f.svc})
	ga, err := g.RequestGrant(access.WithView(ctx, relay2View(t, f, relay, chain,
		[]string{relay2Seam, relay2Claude, ""})), GrantInput{Project: confHR, SessionID: sess})
	require.NoError(t, err)
	require.Equal(t, want.Key(), ga.ProcessKey)
	require.Equal(t, relay2Claude, ga.Program)
}

// The cwd of a relay's agent is a path on that computer: it is judged against
// that computer's repositories, never this one's.
func TestFactsV2_ARelaysCWDIsJudgedOnItsOwnComputer(t *testing.T) {
	f := newReqFixture(t)
	ctx := context.Background()
	f.svc.cfg.Host = "example-server"
	require.NoError(t, store.AdoptLocalHost(ctx, f.db, "example-server"))
	require.NoError(t, store.AddRepoMapping(ctx, f.db, "/srv/finance", lockFin))
	relay := judgeRelay(t, f, "example-laptop")
	relayed := judgeRelayTok(2200)
	relayed.Relay = relay
	for name, tc := range map[string]struct {
		tok     access.ProcessToken
		outside bool
	}{
		"this machine's agent in this machine's repository": {judgeRelayTok(2300), false},
		"a relay's agent at the same path, on its computer": {relayed, true},
	} {
		a, err := store.CreateAccessApproval(ctx, f.db, store.AccessRequest{Project: lockFin, Kind: store.AccessKindRequest,
			Lease: time.Hour, TTL: RequestTTL, PeerUID: 501, PeerPID: tc.tok.PID, ProcessKey: tc.tok.Key(), Program: relay2Claude,
			SessionID: "01SESSIONEXAMPLE", CWD: "/srv/finance/ledger"}, f.now)
		require.NoError(t, err)
		facts, err := f.svc.Facts(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, tc.outside, contains(warningCodes(facts), WarnCWDOutside), name)
		require.Equal(t, tc.tok.Relay != "", contains(warningCodes(facts), WarnRelay), name)
		require.NoError(t, store.DenyAccess(ctx, f.db, a.ID, "test", f.now))
	}
}

func contains(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}

// The anchor rule judges a relay's process by its relay's name for it: a
// shell or a multiplexer is never filed, an unnamed one is not filed, and
// version 1 still files nothing.
func TestRequestV2_TheAnchorRuleJudgesARelaysChain(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	relay := judgeRelay(t, f, "example-laptop")
	bridge, shellTok, tmux := judgeRelayTok(2510), judgeRelayTok(2500), judgeRelayTok(2400)
	chain := []access.ProcessToken{bridge, shellTok, tmux}
	for name, tc := range map[string]struct {
		programs []string
		anchor   access.ProcessToken
		want     error
		what     string
	}{
		"a shell":                 {[]string{relay2Seam, "/bin/zsh", "/opt/homebrew/bin/tmux"}, shellTok, ErrBroadAnchor, "zsh, a shell"},
		"a multiplexer":           {[]string{relay2Seam, "/bin/zsh", "/opt/homebrew/bin/tmux"}, tmux, ErrBroadAnchor, "tmux, a terminal multiplexer"},
		"a program no agent ran":  {[]string{relay2Seam, "/usr/bin/python3", "/sbin/launchd"}, shellTok, ErrBroadAnchor, "python3"},
		"an unnamed anchor":       {[]string{relay2Seam, "", "/opt/homebrew/bin/tmux"}, shellTok, ErrRelayUnnamed, ""},
		"version 1, no names":     {nil, shellTok, ErrRelayUnnamed, ""},
		"names for another chain": {[]string{relay2Seam}, shellTok, ErrRelayUnnamed, ""},
	} {
		sess := f.session(t, "cc/"+name, "/work/ops", tc.anchor)
		v := relay2View(t, f, relay, chain, tc.programs)
		_, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: v, SessionID: sess, Via: ViaRequest})
		require.ErrorIs(t, err, tc.want, name)
		if tc.what != "" {
			require.Contains(t, err.Error(), tc.what, name)
		}
		require.Equal(t, errors.Is(tc.want, ErrBroadAnchor), f.svc.JudgeStart(v, tc.anchor.AgentID()) != nil,
			"%s: the SessionStart hook judges it as filing does", name)
	}
	pending, err := store.PendingAccessApprovals(ctx, f.db, f.now)
	require.NoError(t, err)
	require.Empty(t, pending, "no request exists")

	// An agent above a program no agent is: filed, as on this machine.
	agent := judgeRelayTok(2600)
	script := judgeRelayTok(2610)
	sess := f.session(t, "cc/script", "/work/ops", script)
	v := relay2View(t, f, relay, []access.ProcessToken{script, agent}, []string{"/usr/bin/python3", relay2Claude})
	a, err := f.svc.Request(ctx, RequestInput{Project: lockFin, View: v, SessionID: sess, Via: ViaRequest})
	require.NoError(t, err)
	require.Equal(t, "/usr/bin/python3", a.Program)
	require.NoError(t, f.svc.JudgeStart(v, script.AgentID()))
}

// The grant's and the move's approve core (P11) refuse a stored relay row
// nobody named, as the lease's does: on every proof.
func TestApproveV2_GrantsAndMovesRefuseARelaysUnnamedRow(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	relay := judgeRelay(t, f, "example-laptop")
	tok := judgeRelayTok(2200)
	tok.Relay = relay
	sess := f.sessionIn(t, openOps, "cc/move")
	grant, err := store.CreateGrantRequest(ctx, f.db, store.GrantRequest{Project: confHR, From: lockLeg, Lease: time.Hour,
		TTL: RequestTTL, PeerUID: 501, PeerPID: tok.PID, ProcessKey: tok.Key(), SessionID: sess}, f.now)
	require.NoError(t, err)
	move, err := store.MoveRequestCreate(ctx, f.db, store.MoveRequest{Project: confHR, From: openOps,
		Carry: store.Carry{Home: openOps, Holds: []string{confHR}, WriteTo: []string{confHR}}, Lease: time.Hour, TTL: RequestTTL,
		PeerUID: 501, PeerPID: tok.PID, ProcessKey: tok.Key(), SessionID: sess}, f.now)
	require.NoError(t, err)
	g := NewGrants(GrantsConfig{Approvals: f.svc})

	facts, err := f.svc.Facts(ctx, grant.ID)
	require.NoError(t, err)
	_, err = g.Approve(ctx, Decision{ID: grant.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrRelayUnnamed)
	facts, err = f.svc.Facts(ctx, move.ID)
	require.NoError(t, err)
	_, err = g.ApproveMove(ctx, Decision{ID: move.ID, Duration: For1h, Digest: facts.Digest, Proof: Proof{Kind: ProofDaemon}})
	require.ErrorIs(t, err, ErrRelayUnnamed)
	for _, id := range []string{grant.ID, move.ID} {
		row, err := store.AccessApprovalByID(ctx, f.db, id)
		require.NoError(t, err)
		require.Equal(t, store.ApprovalPending, row.Status, "nothing decided")
	}
}

// The console names a relay's leased agent with its computer.
func TestAgentLabelOn(t *testing.T) {
	require.Equal(t, "Claude Code 2.1.319 (pid 4242 on example-laptop)", AgentLabelOn(relay2Claude, 4242, "example-laptop"))
	require.Equal(t, "Claude Code 2.1.319 (pid 4242)", AgentLabelOn(relay2Claude, 4242, ""))
	require.Equal(t, "pid 4242 on example-laptop", AgentLabelOn("", 4242, "example-laptop"))
	require.Empty(t, AgentLabelOn(relay2Claude, 0, "example-laptop"))
	require.Equal(t, AgentLabel(relay2Claude, 4242), AgentLabelOn(relay2Claude, 4242, ""))

	f := newReqFixture(t)
	relay := judgeRelay(t, f, "example-laptop")
	require.Equal(t, "example-laptop", RelayName(context.Background(), f.db, relay))
	require.Empty(t, RelayName(context.Background(), f.db, ""))
	require.Empty(t, RelayName(context.Background(), f.db, "01NOSUCHRELAY"))
}
