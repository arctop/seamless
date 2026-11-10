package approval

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// JudgeStart judges the process a SessionStart hook would stamp as filing
// judges it once stamped: a broad ancestor or a process no agent started is
// an *AnchorError, an agent's process (node included, since it is the one the
// hook stamps) is not, and anything the rule cannot judge is left to filing.
func TestJudgeStart(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{"example-finance"}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	const claudePath = "/Users/owner/.local/share/claude/versions/2.1.319"
	seam := hopAt(900, "/Users/owner/.local/bin/seam")
	for _, c := range []struct {
		name string
		// chain is the seam hook process first, then its ancestors.
		chain []hop
		// claim is the index of the process the hook names (-1: none).
		claim int
		// refused is the start the hook must not record.
		refused bool
	}{
		{"Claude Code", []hop{seam, hopAt(700, claudePath), hopAt(600, "/bin/zsh")}, 1, false},
		{"Codex", []hop{seam, hopAt(700, "/opt/homebrew/bin/codex"), hopAt(600, "/bin/zsh")}, 1, false},
		{"Claude Code run by node (npm): the process the hook stamps",
			[]hop{seam, hopAt(700, "/opt/homebrew/bin/node"), hopAt(600, "/bin/zsh"), hopAt(500, "/usr/bin/login")}, 1, false},
		{"the hook run by hand in a tmux pane: the tmux server",
			[]hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/opt/homebrew/Cellar/tmux/3.8/bin/tmux")}, 2, true},
		{"a login shell", []hop{seam, hopAt(800, "/bin/zsh"), hopAt(700, "/usr/sbin/sshd")}, 2, true},
		{"a program no agent started", []hop{seam, hopAt(700, "/opt/someterm/bin/someterm")}, 1, true},
		{"no claim: left to filing", []hop{seam, hopAt(700, "/opt/homebrew/Cellar/tmux/3.8/bin/tmux")}, -1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			names := map[access.ProcessToken]string{}
			chain := make([]access.ProcessToken, len(c.chain))
			for i, h := range c.chain {
				chain[i], names[h.tok] = h.tok, h.prog
			}
			svc := New(Config{Programs: func(t access.ProcessToken) string { return names[t] }})
			v, ok := policy.PeerChainView(501, chain)
			require.True(t, ok)
			claim := ""
			if c.claim >= 0 {
				claim = chain[c.claim].AgentID()
			}
			err := svc.JudgeStart(v, claim)
			if !c.refused {
				require.NoError(t, err)
				return
			}
			var ae *AnchorError
			require.ErrorAs(t, err, &ae)
			require.ErrorIs(t, err, ErrBroadAnchor)
			require.Contains(t, ae.Rule(), "Seamless files no access request for this process")
		})
	}

	// A claim outside the kernel's chain, or a view with no chain (the TCP
	// key), is not the anchor rule's to answer: filing refuses it as before.
	svc := New(Config{Programs: func(access.ProcessToken) string { return "/opt/homebrew/Cellar/tmux/3.8/bin/tmux" }})
	v, ok := policy.PeerChainView(501, []access.ProcessToken{seam.tok})
	require.True(t, ok)
	require.NoError(t, svc.JudgeStart(v, hopAt(42, "").tok.AgentID()))
	require.NoError(t, svc.JudgeStart(policy.OwnerView(), seam.tok.AgentID()))
}
