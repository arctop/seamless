package hooks

// The transcript, rollout and subagent parsers live in internal/harvest, a leaf
// the daemon and `seam hook` share: under peer_auth the hook reads the agent's
// files and pushes what it found (pushed.go), and one set of parsers is what
// keeps the two sides from reading the same file differently. These names keep
// the daemon's call sites -- and the tests that pin each format -- reading as
// they always have.

import (
	"context"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
)

// harvestFallback is the finding recorded when no assistant text can be
// harvested (harvest.Fallback).
const harvestFallback = harvest.Fallback

// maxHarvestRunes caps auto-harvested findings (harvest.MaxRunes).
const maxHarvestRunes = harvest.MaxRunes

// The rollout read windows (harvest.MaxRolloutTailBytes, MaxRolloutHeadBytes).
const (
	maxRolloutTailBytes = harvest.MaxRolloutTailBytes
	maxRolloutHeadBytes = harvest.MaxRolloutHeadBytes
)

// subagentReportSettle is how long a subagent capture waits for the final
// report to land (harvest.SubagentReportSettle).
const subagentReportSettle = harvest.SubagentReportSettle

// harvestFindings is a Claude Code transcript's last assistant message as
// findings (harvest.Findings).
func harvestFindings(path string) string { return harvest.Findings(path) }

// claudeTranscriptTokens is a Claude Code transcript's token totals
// (harvest.ClaudeTokens).
func claudeTranscriptTokens(path string) (core.TokenUsage, bool) { return harvest.ClaudeTokens(path) }

// codexRolloutTokens is a Codex rollout's cumulative token totals
// (harvest.CodexTokens).
func codexRolloutTokens(path string) (core.TokenUsage, bool) { return harvest.CodexTokens(path) }

// transcriptModel is a Claude Code transcript's last main-thread model
// (harvest.TranscriptModel).
func transcriptModel(path string) string { return harvest.TranscriptModel(path) }

// codexStopFindings is one Codex Stop's findings (harvest.CodexStopFindings).
func codexStopFindings(lastAssistantMessage, transcriptPath string) string {
	return harvest.CodexStopFindings(lastAssistantMessage, transcriptPath)
}

// tailCodexRollout is a rollout's last agent message (harvest.CodexLastMessage).
func tailCodexRollout(path string) string { return harvest.CodexLastMessage(path) }

// headCodexRollout is a rollout's first user message
// (harvest.CodexFirstUserMessage).
func headCodexRollout(path string) string { return harvest.CodexFirstUserMessage(path) }

// codexRolloutLineMessage is one rollout line's agent message
// (harvest.CodexLineMessage).
func codexRolloutLineMessage(line []byte) string { return harvest.CodexLineMessage(line) }

// codexRolloutLineUserMessage is one rollout line's user message
// (harvest.CodexLineUserMessage).
func codexRolloutLineUserMessage(line []byte) string { return harvest.CodexLineUserMessage(line) }

// parseSubagentTranscript is a subagent transcript's prompt, report and
// whether it has settled (harvest.SubagentTranscript).
func parseSubagentTranscript(path string) (prompt, report string, complete bool) {
	return harvest.SubagentTranscript(path)
}

// subagentTranscriptPath resolves a Claude Code subagent's transcript from a
// SubagentStart/SubagentStop payload (harvest.SubagentTranscriptPath).
func subagentTranscriptPath(p subagentPayload) string {
	return harvest.SubagentTranscriptPath(p.AgentTranscriptPath, p.TranscriptPath, p.AgentID)
}

// awaitSubagentReport parses the subagent transcript, waiting for its final
// report to land (harvest.AwaitSubagentReport), and says so when the wait ran
// out: the capture may then hold narration rather than the report.
func (h *Handler) awaitSubagentReport(ctx context.Context, path, agentID string) (prompt, report string) {
	prompt, report, unsettled := harvest.AwaitSubagentReport(ctx, path)
	if unsettled {
		h.logger.Warn("hooks: subagent transcript never settled; capture may hold narration",
			"agent_id", agentID)
	}
	return prompt, report
}

// capRunes truncates s to at most n runes (harvest.CapRunes).
func capRunes(s string, n int) string { return harvest.CapRunes(s, n) }
