package approval

import "github.com/arctop/seamless/internal/access"

// JudgeStart applies the anchor rule (anchor.go) to the agent process a
// SessionStart hook in a locked project's repository is about to stamp on
// the session it records there: agentProcess, a process of v's
// kernel-verified chain. The hook asks before it records anything (owner
// decision 2026-11-08): a hook with no agent above it, run from a tmux pane
// and naming the tmux server say, could never have a request filed or a lease
// approved, so its session is not recorded at all, and the notice says why.
//
// The process is judged as filing judges it once the session is stamped
// with it (judgeAnchor, stamped): Claude Code installed with npm, which runs
// as node, passes exactly as it does at filing, and a relay's process is
// named by its relay's report (wire version 2) as filing names it
// (programNamer). Only an *AnchorError is an answer. nil covers everything
// else -- no claim, no chain, a claim outside the chain, a relay's chain no
// report names -- which the hook leaves to the filing, as before.
func (s *Service) JudgeStart(v access.View, agentProcess string) error {
	who := v.Principal()
	if agentProcess == "" || !who.Socket || len(who.Chain) == 0 {
		return nil
	}
	name, named := s.programNamer(who, who.Chain)
	if !named {
		return nil
	}
	for i, t := range who.Chain {
		if t.AgentID() == agentProcess {
			_, err := judgeAnchor(who.Chain, i, name, true)
			return err
		}
	}
	return nil
}
