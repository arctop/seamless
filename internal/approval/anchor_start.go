package approval

import "github.com/arctop/seamless/internal/access"

// JudgeStart applies the anchor rule (anchor.go) to the agent process a
// SessionStart hook in a locked project's repository is about to stamp on
// the session it records there: agentProcess, a process of v's
// kernel-verified chain. The hook asks before it records anything (owner
// decision 2026-11-08): an agent started straight from a tmux pane, its hook
// naming the tmux server, could never have a request filed or a lease
// approved, so its session is not recorded at all, and the notice says why.
//
// The process is judged as filing judges it once the session is stamped
// with it (judgeAnchor, stamped): Claude Code installed with npm, which runs
// as node, passes exactly as it does at filing. Only an *AnchorError is an
// answer. nil covers everything else -- no claim, no chain, a claim outside
// the chain -- which the hook leaves to the filing, as before.
func (s *Service) JudgeStart(v access.View, agentProcess string) error {
	who := v.Principal()
	if agentProcess == "" || !who.Socket || len(who.Chain) == 0 {
		return nil
	}
	for i, t := range who.Chain {
		if t.AgentID() == agentProcess {
			_, err := judgeAnchor(who.Chain, i, s.cfg.Programs, true)
			return err
		}
	}
	return nil
}
