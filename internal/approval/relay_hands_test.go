package approval

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// rhTok is one process of a relay's computer, as the policy attaches it.
func rhTok(pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 10, Boot: "boot-relay", Relay: "01RELAYEXAMPLE"}
}

// rhOwner is the owner's view through the relay of a chain whose programs
// are programs, the peer first (one process per program, pids from 3000).
func rhOwner(programs ...string) access.Principal {
	chain := make([]access.ProcessToken, len(programs))
	for i := range programs {
		chain[i] = rhTok(int32(3000 + i))
	}
	return access.Principal{Kind: access.KindOwner, UID: 501, HasUID: true, User: "owner", Socket: true,
		Relay: "01RELAYEXAMPLE", Process: chain[0], HasProcess: true, Chain: chain, Programs: programs}
}

const (
	rhSeam     = "/Users/owner/.local/bin/seam"
	rhZsh      = "/bin/zsh"
	rhLogin    = "/usr/bin/login"
	rhTerminal = "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"
	rhClaude   = "/Users/owner/.local/share/claude/versions/2.1.319"
)

// The owner's own hands through a relay (owner decision R-D3): every process
// of the chain named, none an agent by the socket's own judgment, the chain
// whole by its evidence, and the pairing's uid agreeing with the mapping's.
// The refusals come first; each names why.
func TestJudgeRelayHands(t *testing.T) {
	long := make([]string, access.MaxChainHops)
	for i := range long {
		long[i] = rhZsh
	}
	long[0], long[len(long)-1] = rhSeam, rhTerminal
	restricted := rhOwner(rhSeam, rhZsh, rhLogin, rhTerminal)
	restricted.Kind = access.KindRestricted
	local := rhOwner(rhSeam, rhZsh, rhLogin, rhTerminal)
	local.Relay = ""
	v1 := rhOwner(rhSeam, rhZsh, rhLogin, rhTerminal)
	v1.Programs = nil
	unattached := rhOwner(rhSeam)
	unattached.HasProcess, unattached.Chain, unattached.Programs = false, nil, nil

	for name, tc := range map[string]struct {
		who      access.Principal
		recorded uint32
		why      string // "" for the owner's hands
	}{
		"a restricted account":            {who: restricted, recorded: 501, why: "not the owner's account"},
		"this machine's socket":           {who: local, recorded: 501, why: "not the owner's account"},
		"no chain attached":               {who: unattached, recorded: 501, why: "not one this server can attach"},
		"a relay of wire version 1":       {who: v1, recorded: 501, why: "wire version 1"},
		"an agent in the chain":           {who: rhOwner(rhSeam, rhClaude, rhZsh, rhLogin, rhTerminal), recorded: 501, why: "Claude Code 2.1.319 (pid 3001 on example-laptop) is in its process chain: an agent started it"},
		"an agent as the peer":            {who: rhOwner("/opt/homebrew/bin/codex", rhZsh, rhLogin, rhTerminal), recorded: 501, why: "Codex"},
		"a JS runtime in the chain":       {who: rhOwner(rhSeam, "/usr/local/bin/node", rhZsh, rhTerminal), recorded: 501, why: "node (pid 3001 on example-laptop) is in its process chain, and an agent may run as it"},
		"Linux's kernel name for Claude":  {who: rhOwner("seam", "2.1.319", "bash", "sshd"), recorded: 501, why: "Claude Code 2.1.319"},
		"an unnamed process":              {who: rhOwner(rhSeam, rhZsh, "", rhTerminal), recorded: 501, why: "no program for pid 3002"},
		"a chain as long as the bound":    {who: rhOwner(long...), recorded: 501, why: "may have been cut short"},
		"a chain that ends at a shell":    {who: rhOwner(rhSeam, rhZsh), recorded: 501, why: "ends at zsh (pid 3001 on example-laptop)"},
		"a chain that ends at sudo":       {who: rhOwner(rhSeam, rhZsh, "/usr/bin/sudo"), recorded: 501, why: "ends at sudo"},
		"a chain that ends at a script":   {who: rhOwner(rhSeam, "/usr/bin/python3"), recorded: 501, why: "ends at python3"},
		"the peer alone":                  {who: rhOwner(rhSeam), recorded: 501, why: "ends at seam"},
		"a pairing that recorded no uid":  {who: rhOwner(rhSeam, rhZsh, rhLogin, rhTerminal), recorded: 0, why: "recorded no owner's uid there"},
		"a pairing that recorded another": {who: rhOwner(rhSeam, rhZsh, rhLogin, rhTerminal), recorded: 502, why: "recorded uid 502 there as the owner's, and peer_auth.relays maps owner_uid 501"},

		"the owner's terminal":            {who: rhOwner(rhSeam, rhZsh, rhLogin, rhTerminal), recorded: 501},
		"iTerm2's server":                 {who: rhOwner(rhSeam, rhZsh, rhLogin, "/Users/owner/Library/Application Support/iTerm2/iTermServer-3.5.4"), recorded: 501},
		"a multiplexer":                   {who: rhOwner(rhSeam, rhZsh, "tmux: server"), recorded: 501},
		"a remote login, Linux names":     {who: rhOwner("seam", "bash", "sshd-session", "sshd"), recorded: 501},
		"a desktop session, Linux names":  {who: rhOwner("seam", "bash", "gnome-terminal-", "systemd"), recorded: 501},
		"an editor's terminal":            {who: rhOwner(rhSeam, rhZsh, "/Applications/Visual Studio Code.app/Contents/MacOS/Electron"), recorded: 501},
		"sudo below the session's root":   {who: rhOwner(rhSeam, "/usr/bin/sudo", rhZsh, rhLogin, rhTerminal), recorded: 501},
		"a script below the session root": {who: rhOwner(rhSeam, "/usr/bin/python3", rhZsh, rhLogin, rhTerminal), recorded: 501},
	} {
		t.Run(name, func(t *testing.T) {
			got := JudgeRelayHands(tc.who, "example-laptop", tc.recorded)
			require.Equal(t, tc.who.Relay, got.Relay)
			require.Equal(t, "example-laptop", got.Name)
			require.Equal(t, tc.who.Programs, got.Programs)
			if tc.why == "" {
				require.Empty(t, got.Why)
				require.True(t, got.Owner())
				return
			}
			require.False(t, got.Owner())
			require.Contains(t, got.Why, tc.why)
		})
	}
}

// The predicate is the socket's own: every program chainHasAgent counts as
// an agent's (IsAgentProgram) refuses the owner's hands through a relay.
func TestJudgeRelayHands_AgentIsTheSocketsPredicate(t *testing.T) {
	for _, prog := range []string{rhClaude, "claude", "2.1.319", "codex", "codex-aarch64-apple-darwin", "node", "bun", "deno",
		"/Applications/Claude.app/Contents/Helpers/disclaimer"} {
		require.True(t, IsAgentProgram(prog), prog)
		got := JudgeRelayHands(rhOwner(rhSeam, prog, rhZsh, rhLogin, rhTerminal), "example-laptop", 501)
		require.False(t, got.Owner(), prog)
		require.Contains(t, got.Why, "is in its process chain", prog)
	}
}
