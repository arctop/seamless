// Package harvest reads an agent's own files -- a Claude Code transcript, a
// Codex rollout, a subagent transcript, a plan file -- for the captures
// Seamless keeps: the session-end findings and token totals, the model, the
// subagent spawn prompt and report, and the plan text. It is a leaf (core and
// validate only) so both sides can share it: the daemon parses with it when it
// may read the agent's disk (internal/hooks), and `seam hook` parses with it
// when it may not -- under peer_auth the daemon runs as its own OS account and
// opens no path a hook names, so the hook, running as the owner beside the
// agent, reads the files itself and pushes what it found (push.go). One set of
// parsers is what keeps the two from reading the same file differently.
//
// Every parser is best-effort and never errors: a hook must always complete,
// so a blank path, an unreadable file or a malformed line yields the zero
// value (or the documented fallback) rather than a failure.
package harvest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// Fallback is the finding recorded when no assistant text can be harvested
// (missing/unreadable/empty transcript). It marks the session ended without a
// summary rather than leaving findings blank. The briefing filters it back out
// (core.FindingNoSummary) so a content-free line never reaches an agent.
const Fallback = core.FindingNoSummary

// MaxRunes caps auto-harvested findings so a long final message does not
// bloat the session record or later briefings.
const MaxRunes = 2000

// Prefix opens every auto-harvested finding, so the console and briefing can
// tell a harvested summary from one an agent wrote.
const Prefix = "(auto-harvested) "

// maxScanLine caps a single transcript line the scanners accept: assistant
// turns and tool results can be large, so grow well past bufio's 64 KiB
// default so a long line is not dropped.
const maxScanLine = 16 * 1024 * 1024

// transcriptLine is the tolerant shape of one Claude Code transcript JSONL line.
// Only assistant lines carry the text harvested; message.content is either a
// string or an array of typed blocks.
type transcriptLine struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Findings extracts draft findings from a Claude Code transcript: it returns
// the text of the LAST assistant message (its text blocks concatenated),
// prefixed with Prefix and capped at MaxRunes. Any problem -- blank path,
// unreadable file, no assistant text -- yields Fallback. It never errors; the
// session-end hook must always complete the session.
func Findings(path string) string {
	if strings.TrimSpace(path) == "" {
		return Fallback
	}
	f, err := os.Open(path)
	if err != nil {
		return Fallback
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)

	last := ""
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal(line, &tl); err != nil {
			continue // tolerant: skip malformed lines
		}
		if tl.Type != "assistant" {
			continue
		}
		if txt := MessageText(tl.Message.Content); txt != "" {
			last = txt
		}
	}

	last = strings.TrimSpace(last)
	if last == "" {
		return Fallback
	}
	return Prefix + CapRunes(last, MaxRunes)
}

// MessageText concatenates the text blocks of a transcript message's content
// (assistant or user), which is either a plain string or an array of typed blocks.
func MessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// CapRunes truncates s to at most n runes (never splitting a multi-byte rune).
func CapRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
