package access

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const treeBoot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"

// procTable is a fake process table for walkChain.
type procTable map[int32]procFacts

func (tb procTable) look(pid int32) (procFacts, error) {
	f, ok := tb[pid]
	if !ok {
		return procFacts{}, errNoProcess
	}
	return f, nil
}

// tree is one owner's processes: a terminal shell, the agent under it, the
// agent's MCP bridge and a script it ran (with a grandchild), plus a sibling
// of the agent -- another program the same shell started.
//
//	1 launchd
//	└─ 100 zsh
//	   ├─ 200 claude            (the anchor)
//	   │  ├─ 300 seam (mcp-proxy)
//	   │  └─ 310 bash
//	   │     └─ 320 seam (a CLI call from a script)
//	   └─ 210 python            (a sibling: same uid, not in the tree)
var tree = procTable{
	100: {ppid: 1, start: 1000, name: "zsh", unique: 10, parentUnique: 1},
	200: {ppid: 100, start: 2000, name: "claude", unique: 20, parentUnique: 10},
	210: {ppid: 100, start: 2100, name: "python", unique: 21, parentUnique: 10},
	300: {ppid: 200, start: 3000, name: "seam", unique: 30, parentUnique: 20},
	310: {ppid: 200, start: 3100, name: "bash", unique: 31, parentUnique: 20},
	320: {ppid: 310, start: 3200, name: "seam", unique: 32, parentUnique: 31},
}

func walk(t *testing.T, tb procTable, pid int32) []ChainHop {
	t.Helper()
	hops, err := walkChain(pid, tb[pid].pin, treeBoot, tb.look)
	require.NoError(t, err)
	return hops
}

func pids(hops []ChainHop) []int32 {
	out := make([]int32, len(hops))
	for i, h := range hops {
		out[i] = h.Token.PID
	}
	return out
}

func TestWalkChain_Tree(t *testing.T) {
	require.Equal(t, []int32{300, 200, 100}, pids(walk(t, tree, 300)), "the bridge: up to the shell, never pid 1")
	require.Equal(t, []int32{320, 310, 200, 100}, pids(walk(t, tree, 320)), "a script's CLI call")
	require.Equal(t, []int32{210, 100}, pids(walk(t, tree, 210)), "the sibling never passes through the agent")

	hops := walk(t, tree, 320)
	require.Equal(t, ProcessToken{PID: 200, Version: 2000, Boot: treeBoot}, hops[2].Token)
	require.Equal(t, "claude", hops[2].Name)
	require.Equal(t, "200.2000", hops[2].Token.AgentID(), "agentproc's rendering, for comparing with a session's stamp")
}

// The lease is bound to the agent process: everything below it shares the
// lease; nothing beside or above it does.
func TestWalkChain_LeaseCoversTheAgentsTree(t *testing.T) {
	p := lockPolicy(t)
	anchor := ProcessToken{PID: 200, Version: 2000, Boot: treeBoot}
	lease := lockLease(lockFin, anchor, lockNow.Add(time.Hour))

	for name, tc := range map[string]struct {
		pid  int32
		open bool
	}{
		"the agent itself":                   {200, true},
		"a child (the MCP bridge)":           {300, true},
		"a grandchild (a script's CLI call)": {320, true},
		"a sibling of the same uid":          {210, false},
		"the shell above the agent":          {100, false},
	} {
		t.Run(name, func(t *testing.T) {
			v, ok := p.PeerChainView(501, Tokens(walk(t, tree, tc.pid)))
			require.True(t, ok)
			v = v.WithLeases(lockNow, []Lease{lease})
			require.Equal(t, tc.open, v.Visible(lockFin))
			require.Equal(t, !tc.open, v.Locked(lockFin))
		})
	}
}

func TestWalkChain_LinksThatDoNotVerifyEndTheChain(t *testing.T) {
	// An orphan: the agent exited and its bridge was reparented to launchd.
	orphan := procTable{
		100: tree[100],
		300: {ppid: 1, start: 3000, name: "seam", unique: 30, parentUnique: 20},
	}
	require.Equal(t, []int32{300}, pids(walk(t, orphan, 300)), "an orphan's chain no longer reaches the agent")

	// The agent exited and its pid was reused by a process that started
	// after the child: a reused pid is never taken for the parent.
	reused := procTable{
		300: tree[300],
		200: {ppid: 100, start: 9000, name: "claude", unique: 90, parentUnique: 10},
		100: tree[100],
	}
	require.Equal(t, []int32{300}, pids(walk(t, reused, 300)))

	// macOS: the parent's unique id is not the one the child records, even
	// though the start times look right.
	impostor := procTable{
		300: tree[300],
		200: {ppid: 100, start: 2000, name: "claude", unique: 77, parentUnique: 10},
		100: tree[100],
	}
	require.Equal(t, []int32{300}, pids(walk(t, impostor, 300)))

	// Linux keeps no unique ids (0): the start-time check alone decides.
	linux := procTable{
		300: {ppid: 200, start: 3000, name: "seam"},
		200: {ppid: 100, start: 2000, name: "claude"},
		100: {ppid: 1, start: 1000, name: "zsh"},
	}
	require.Equal(t, []int32{300, 200, 100}, pids(walk(t, linux, 300)))

	// A parent that cannot be read (it exited mid-walk) ends the chain.
	gone := procTable{300: tree[300], 100: tree[100]}
	require.Equal(t, []int32{300}, pids(walk(t, gone, 300)))

	// A parent with no start time ends it too.
	nostart := procTable{300: tree[300], 200: {ppid: 100, name: "claude", unique: 20, parentUnique: 10}}
	require.Equal(t, []int32{300}, pids(walk(t, nostart, 300)))
}

func TestWalkChain_ThePeerMustStillBeTheProcessThatConnected(t *testing.T) {
	_, err := walkChain(300, 2999, treeBoot, tree.look)
	require.ErrorIs(t, err, ErrPeerGone, "a mismatched pin (start time or pidversion) is refused")

	_, err = walkChain(999, 0, treeBoot, tree.look)
	require.ErrorIs(t, err, errNoProcess, "a peer that is gone entirely")

	_, err = walkChain(0, 0, treeBoot, tree.look)
	require.Error(t, err)

	_, err = walkChain(300, tree[300].pin, "not a boot id", tree.look)
	require.Error(t, err, "no boot id, no token")

	broken := procTable{300: {ppid: 200, name: "seam"}}
	_, err = walkChain(300, 0, treeBoot, broken.look)
	require.ErrorIs(t, err, ErrPeerGone, "a peer with no start time cannot be pinned")

	failing := func(int32) (procFacts, error) { return procFacts{}, errors.New("sysctl failed") }
	_, err = walkChain(300, 0, treeBoot, failing)
	require.ErrorContains(t, err, "sysctl failed")
}

func TestWalkChain_DepthBound(t *testing.T) {
	deep := procTable{}
	for pid := int32(2); pid <= 60; pid++ {
		deep[pid] = procFacts{ppid: pid - 1, start: uint64(pid), name: "p"}
	}
	hops := walk(t, deep, 60)
	require.Len(t, hops, MaxChainHops)
	require.Equal(t, int32(60), hops[0].Token.PID)
	require.Equal(t, int32(60-MaxChainHops+1), hops[MaxChainHops-1].Token.PID)
}

func TestPeerChainView_RefusesAMalformedChain(t *testing.T) {
	p := lockPolicy(t)
	good := Tokens(walk(t, tree, 320))
	other := ProcessToken{PID: 200, Version: 2000, Boot: "0f0e0d0c-0000-4000-8000-000000000001"}
	long := make([]ProcessToken, MaxChainHops+1)
	for i := range long {
		long[i] = ProcessToken{PID: int32(i + 2), Version: 1, Boot: treeBoot}
	}
	for name, chain := range map[string][]ProcessToken{
		"empty":                  nil,
		"an invalid token":       {good[0], {PID: 0, Version: 1, Boot: treeBoot}},
		"two boots":              {good[0], other},
		"longer than a chain is": long,
	} {
		t.Run(name, func(t *testing.T) {
			v, ok := p.PeerChainView(501, chain)
			require.True(t, ok)
			require.False(t, v.Principal().HasProcess)
			require.Empty(t, v.Principal().Chain)
			lease := lockLease(lockFin, good[0], lockNow.Add(time.Hour))
			require.Empty(t, v.WithLeases(lockNow, []Lease{lease}).Leases(), "no chain, no lease")
		})
	}
	v, ok := p.PeerChainView(501, good)
	require.True(t, ok)
	require.Equal(t, good[0], v.Principal().Process, "the peer is the chain's first process")
	require.Equal(t, good, v.Principal().Chain)
	good[1].PID = 999
	require.NotEqual(t, good, v.Principal().Chain, "the view keeps its own copy")

	_, ok = p.PeerChainView(9999, Tokens(walk(t, tree, 320)))
	require.False(t, ok, "an unknown uid has no view, chain or not")
}

func TestParseProcStat(t *testing.T) {
	// comm may hold spaces and parentheses; the fields split from the last ')'.
	line := "4242 (my (odd) proc) S 4100 4242 4100 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10"
	f, err := parseProcStat(line)
	require.NoError(t, err)
	require.Equal(t, procFacts{ppid: 4100, start: 987654, name: "my (odd) proc"}, f)

	for _, bad := range []string{"", "4242 no parens", "4242 (x) S", "4242 (x) S nope 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19",
		"4242 (x) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 notanumber"} {
		_, err := parseProcStat(bad)
		require.ErrorIs(t, err, errMalformedStat, "%q", bad)
	}
	require.True(t, strings.HasPrefix(ProcessToken{PID: 1, Version: 2, Boot: treeBoot}.Key(), treeBoot))
}
