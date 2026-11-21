package harvest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// ClaudeTokens sums the real model token usage of a Claude Code transcript:
// every assistant message's usage block, counted ONCE per message id. It is
// ClaudeUsage's totals, the sum of the ledger rows the same pass reads, so the
// two can never disagree; see ClaudeUsage for the rules (the dedup by
// message.id, the synthetic entries skipped).
//
// Subagent turns are INCLUDED -- unlike model attribution, which skips them,
// real token spend counts a subagent's burn against the session that launched
// it: the sidechain lines of the releases that kept subagent turns in the
// session's transcript, and the transcripts newer ones keep beside it, one per
// subagent (SubagentTranscripts).
//
// ok is false when nothing was harvested (blank path, unreadable file, no assistant
// usage). It never errors: the SessionEnd hook must always complete.
func ClaudeTokens(path string) (usage core.TokenUsage, ok bool) {
	usage, _, ok = ClaudeUsage(path)
	return usage, ok
}

// CodexTokens returns a Codex session's cumulative model token usage: the LAST
// token_count event in the rollout. Codex emits a token_count event every turn
// whose total_token_usage is the running session total, so the final one is the
// whole-session figure -- no summing, and reading it every Stop is idempotent:
// the store credits only what it grew by (store.CreditConversationUsage), so
// the same total read again adds nothing.
//
// Only the file's tail is read: the closing token_count of a turn is among its
// final lines, so a bounded tail finds the latest without parsing a multi-MB
// session. Codex's input_tokens INCLUDES its cached_input_tokens, so the fresh
// (uncached) input is input - cached; Codex has no cache-creation notion, so that
// field stays 0. Normalizing this way makes the computed Total match Codex's own
// total_tokens and stay comparable with the Claude Code figure.
//
// ok is false when the tail holds no token_count event (blank/unreadable path, or a
// session too young to have emitted one). It never errors.
func CodexTokens(path string) (usage core.TokenUsage, ok bool) {
	if strings.TrimSpace(path) == "" {
		return core.TokenUsage{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return core.TokenUsage{}, false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return core.TokenUsage{}, false
	}
	start := int64(0)
	if info.Size() > MaxRolloutTailBytes {
		start = info.Size() - MaxRolloutTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return core.TokenUsage{}, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return core.TokenUsage{}, false
	}

	lines := bytes.Split(data, []byte("\n"))
	// A mid-file seek almost always lands inside a line; drop that leading partial.
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	for _, line := range lines {
		if u, has := codexTokenCountLine(line); has {
			usage = u // keep the last: token_count is cumulative, later wins
			ok = true
		}
	}
	return usage, ok
}

// codexTokenCountLine extracts the cumulative token usage from a single rollout
// line if it is an event_msg/token_count carrying total_token_usage, normalized to
// core.TokenUsage. has is false for any other line. Tolerant: a malformed or
// unrelated line yields has=false.
func codexTokenCountLine(line []byte) (usage core.TokenUsage, has bool) {
	if !bytes.Contains(line, []byte(`"token_count"`)) {
		return core.TokenUsage{}, false // cheap prefilter
	}
	var ev codexRolloutEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return core.TokenUsage{}, false
	}
	if ev.Type != "event_msg" {
		return core.TokenUsage{}, false
	}
	var p struct {
		Type string `json:"type"`
		Info struct {
			Total *struct {
				Input  int `json:"input_tokens"`
				Cached int `json:"cached_input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"total_token_usage"`
		} `json:"info"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return core.TokenUsage{}, false
	}
	if p.Type != "token_count" || p.Info.Total == nil {
		return core.TokenUsage{}, false
	}
	t := p.Info.Total
	fresh := max(t.Input-t.Cached, 0) // Codex input_tokens includes the cached portion
	usage = core.TokenUsage{Input: fresh, Cached: t.Cached, Output: t.Output}
	usage.Normalize()
	return usage, true
}
