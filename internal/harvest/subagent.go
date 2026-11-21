package harvest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Claude Code appends a subagent's final message to the transcript while
// SubagentStop is already in flight, so parsing the file the moment the hook
// fires reads it one message short: the capture stores the narration line
// before the report ("Now the console files.") as though it were the
// deliverable. 55 of one machine's first 80 captures landed that way, and the
// bimodal split -- one-line narration or a full report, nothing between -- is
// the race, not truncation. A half-written trailing line reaches the same end
// by a second route, because the tolerant parser skips it and falls back to
// the previous assistant text. So wait for the transcript's terminal shape
// rather than trusting whatever is on disk at hook time. The budget fits inside
// the hook's timeout with room for the note write, and an interrupted subagent
// -- one that never emits a final message -- just spends it.
const (
	SubagentReportSettle = 3 * time.Second
	SubagentReportPoll   = 50 * time.Millisecond
)

// SubagentTranscriptPath resolves a Claude Code subagent's JSONL transcript:
// the agent_transcript_path SubagentStop names, else the payload
// transcript_path when it already names a subagents/agent-*.jsonl file, else
// constructed from the main transcript path and agent id (the verified layout:
// <proj-dir>/<session-id>/subagents/agent-<agent_id>.jsonl; SubagentStart
// carries no agent_transcript_path). An agent id carrying path separators or
// ".." is rejected rather than joined into the path -- it is a filename
// fragment, never a path.
func SubagentTranscriptPath(agentTranscriptPath, transcriptPath, agentID string) string {
	if agentTranscriptPath != "" {
		return agentTranscriptPath
	}
	if transcriptPath == "" {
		return ""
	}
	base := filepath.Base(transcriptPath)
	if filepath.Base(filepath.Dir(transcriptPath)) == "subagents" &&
		strings.HasPrefix(base, "agent-") && strings.HasSuffix(base, ".jsonl") {
		return transcriptPath
	}
	if strings.ContainsAny(agentID, `/\`) || strings.Contains(agentID, "..") {
		return ""
	}
	return filepath.Join(subagentsDir(transcriptPath), "agent-"+agentID+".jsonl")
}

// subagentsDir is the folder Claude Code keeps a session's subagent
// transcripts in: <transcript minus .jsonl>/subagents.
func subagentsDir(transcriptPath string) string {
	return filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents")
}

// SubagentFile is one subagent transcript SubagentTranscripts listed: its path
// and what Lstat said of it, which the reader holds the file it opens to.
type SubagentFile struct {
	Path string
	Info os.FileInfo
}

// SubagentTranscripts lists the subagent transcripts Claude Code keeps beside
// the session transcript at transcriptPath (a *.jsonl file): the
// agent-*.jsonl files in its subagents folder, in name order. Nothing is
// followed: the session's folder and its subagents folder must be folders,
// not links, and an entry is listed only when Lstat finds a regular file, so
// a link, a folder or a device of that name is skipped. The other files there
// (agent-*.meta.json and the like) are not transcripts. A folder that is
// missing or unreadable lists nothing; it never errors.
func SubagentTranscripts(transcriptPath string) []SubagentFile {
	if !strings.HasSuffix(transcriptPath, ".jsonl") {
		return nil
	}
	dir := subagentsDir(transcriptPath)
	for _, d := range []string{filepath.Dir(dir), dir} {
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
			return nil
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SubagentFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// os.ReadDir's Info is Lstat's: a link reports itself, not its target.
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, SubagentFile{Path: filepath.Join(dir, name), Info: fi})
	}
	return out
}

// AwaitSubagentReport parses the transcript, waiting up to SubagentReportSettle
// for the final assistant message to land. It returns the best values seen, so
// a transcript that never settles (an interrupted subagent) still yields its
// last narration rather than nothing -- that is the truest record available,
// and the caller's own empty check decides whether it is worth a note.
// unsettled reports that the whole budget ran out with the transcript still
// unfinished (the capture may hold narration); a cancelled ctx or a blank or
// absent path returns at once with unsettled false.
func AwaitSubagentReport(ctx context.Context, path string) (prompt, report string, unsettled bool) {
	// The size is taken before the first parse, never after it: a report
	// appended between the parse and a later stat would be counted in the
	// size already, so the poll below would never see the file grow, and the
	// capture would wait out the whole budget and keep the narration.
	fi, statErr := os.Stat(path)
	prompt, report, complete := SubagentTranscript(path)
	if complete {
		return prompt, report, false
	}
	var size int64
	if statErr == nil {
		size = fi.Size()
	} else if _, err := os.Stat(path); err != nil {
		return prompt, report, false // blank or absent path: nothing to wait for
	}
	// A transcript that appeared only after the first stat counts from empty,
	// so everything in it is growth the poll reads.

	settled := time.NewTimer(SubagentReportSettle)
	defer settled.Stop()
	tick := time.NewTicker(SubagentReportPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return prompt, report, false
		case <-settled.C:
			return prompt, report, true
		case <-tick.C:
			// Re-parse only on growth: an interrupted subagent would otherwise
			// re-read a transcript that will never change once per tick.
			fi, err := os.Stat(path)
			if err != nil || fi.Size() == size {
				continue
			}
			size = fi.Size()
			p, r, done := SubagentTranscript(path)
			if p != "" {
				prompt = p
			}
			if r != "" {
				report = r
			}
			if done {
				return prompt, report, false
			}
		}
	}
}

// SubagentTranscript extracts the prompt (first user message) and final report
// (last assistant text) from a subagent transcript, and reports whether the
// transcript has reached its TERMINAL shape: a last line that parses, is an
// assistant message, carries text, and carries no tool_use. 371 of the 378
// subagent transcripts on the machine this was diagnosed on end exactly that
// way; the 7 that do not were interrupted mid-run. Anything else -- a trailing
// tool_use, a lone thinking block, a tool_result, a half-written line -- means
// the agent is still working or the writer is mid-flush, so the newest
// assistant text is narration rather than the report. Any problem yields empty
// strings and complete=false; capture is best-effort and never errors.
func SubagentTranscript(path string) (prompt, report string, complete bool) {
	if strings.TrimSpace(path) == "" {
		return "", "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// Same headroom as Findings: transcript lines can be very large.
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Every line clears the flag, so it describes the LAST one and not merely
		// some earlier line that happened to look final.
		complete = false
		var tl transcriptLine
		if err := json.Unmarshal(line, &tl); err != nil {
			continue // tolerant: skip malformed lines (a trailing one is a mid-flush write)
		}
		switch tl.Type {
		case "user":
			if prompt == "" {
				prompt = MessageText(tl.Message.Content)
			}
		case "assistant":
			if txt := MessageText(tl.Message.Content); txt != "" {
				report = txt
				complete = !hasToolUse(tl.Message.Content)
			}
		}
	}
	if sc.Err() != nil {
		complete = false // a truncated read cannot show which line is last
	}
	return strings.TrimSpace(prompt), strings.TrimSpace(report), complete
}

// hasToolUse reports whether a message's content carries a tool_use block, the
// marker that the turn continues. Text sitting beside a tool_use is the model
// narrating its next step, never its report. Content that is a plain string
// carries no blocks and so no tool_use.
func hasToolUse(raw json.RawMessage) bool {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	return slices.ContainsFunc(blocks, func(b contentBlock) bool { return b.Type == "tool_use" })
}
