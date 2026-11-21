package harvest

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// MaxUsageRows bounds the usage ledger rows one push may carry. A row is one
// quarter-hour of one model and one thread, and a session is active in only
// some of its quarter-hours: the longest session on the machine this was
// measured on ran 844 minutes and harvested 47 rows. 10000 rows is over
// 2500 hours of a single model's continuous work, and at the widest a row can
// be (the longest model id a push accepts) the push still fits well inside
// the hook body's cap (internal/hooks pins the two). A transcript past it
// pushes its totals alone, which the daemon credits as a client that names no
// buckets.
const MaxUsageRows = 10000

// usageEntry is the tolerant shape of a Claude Code assistant line, as the
// usage harvest reads it.
type usageEntry struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	Message     struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int `json:"input_tokens"`
			Output        int `json:"output_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ClaudeUsage reads a Claude Code transcript once, with the transcripts of the
// subagents it launched, and returns the session's token totals and its usage
// ledger rows, the totals being the rows' sum. Every assistant message counts
// ONCE, by message.id: one message is written as several lines that share an
// id and repeat the same usage block (not a per-line delta), so the first line
// for an id contributes and the rest are skipped.
//
// A message lands in the quarter-hour (UTC) of its line's timestamp, under
// message.model ("" when the line names none) and its thread: a sidechain
// line (isSidechain) is a subagent's turn, every other line the agent's own.
// Its minute sets that minute's bit in the row's mask. A message whose line
// has no timestamp lands with the last earlier one in its file that had a
// timestamp (or, before any, the first later one; in a file with none, the
// file's modification time) and sets no minute: the mask holds only minutes a
// message is known to have landed in. A synthetic entry (Claude Code's
// placeholder, model <synthetic>) is no model call and is skipped; its usage
// is zero.
//
// A subagent's burn counts against the session that launched it. Claude Code
// releases that kept subagent turns in the session's transcript wrote them as
// sidechain lines; current ones keep each subagent in a transcript of its own
// beside it (SubagentTranscripts), and every message read there is a
// subagent's, sidechain line or not. One set of message ids spans the session's
// transcript and all of its subagents', so a message that appears in two of
// them (a forked subagent opens with a copy of its parent's last message)
// counts once, where it was read first: the session's own transcript, then the
// subagents' in name order. A subagent transcript that is missing, unreadable
// or not a regular file is skipped, never an error.
//
// Every file is read whole because the rows are the session's usage to date
// and the store replaces the session's rows with them: re-parsing a resumed or
// compacted session's grown transcripts yields its new rows, never a double
// count (store.ReplaceConversationUsage).
//
// ok is false when nothing was harvested (blank path, unreadable transcript,
// no assistant usage in it or its subagents'). It never errors: the SessionEnd
// hook must always complete.
func ClaudeUsage(path string) (totals core.TokenUsage, rows []core.UsageBucket, ok bool) {
	if strings.TrimSpace(path) == "" {
		return core.TokenUsage{}, nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return core.TokenUsage{}, nil, false
	}
	defer func() { _ = f.Close() }()

	fold := usageFold{rows: map[usageKey]*core.UsageBucket{}, seen: map[string]struct{}{}}
	fold.file(f, false)
	for _, sub := range SubagentTranscripts(path) {
		fold.subagent(sub)
	}
	if len(fold.rows) == 0 {
		return core.TokenUsage{}, nil, false
	}
	rows = make([]core.UsageBucket, 0, len(fold.rows))
	for _, r := range fold.rows {
		rows = append(rows, *r)
	}
	slices.SortFunc(rows, func(a, b core.UsageBucket) int {
		return cmp.Or(a.Bucket.Compare(b.Bucket), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Thread, b.Thread))
	})
	return core.SumUsage(rows), rows, true
}

// usageFold is one session's usage as ClaudeUsage reads it: the ledger rows
// and the message ids counted so far, across every file it folds in.
type usageFold struct {
	rows map[usageKey]*core.UsageBucket
	seen map[string]struct{}
}

// subagent folds in the subagent transcript SubagentTranscripts listed, when
// the file opened is still that regular file (it was not swapped for a link
// since it was listed).
func (u usageFold) subagent(entry SubagentFile) {
	f, err := os.Open(entry.Path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	if opened, err := f.Stat(); err != nil || !os.SameFile(entry.Info, opened) {
		return
	}
	u.file(f, true)
}

// file folds in one transcript file. subagent counts every message in it as a
// subagent's turn. A line longer than maxScanLine ends the file's read there,
// as it always has; what was read before it counts.
func (u usageFold) file(f *os.File, subagent bool) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLine)

	// The clock is the file's own: an unstamped line takes a time from its
	// own file, never from another transcript.
	acc := usageAccumulator{rows: u.rows}
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue // cheap prefilter before a full JSON parse
		}
		var e usageEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerant: skip malformed lines
		}
		// An assistant line with no usage block (a text-only or synthetic entry)
		// is not a billed message.
		if e.Type != "assistant" || e.Message.ID == "" || e.Message.Usage == nil || e.Message.Model == syntheticModel {
			continue
		}
		if _, dup := u.seen[e.Message.ID]; dup {
			continue // same message split across lines, or files -- its usage is repeated, not additive
		}
		u.seen[e.Message.ID] = struct{}{}
		msg := core.UsageBucket{Model: strings.TrimSpace(e.Message.Model), Thread: core.UsageThreadMain,
			Input: e.Message.Usage.Input, Cached: e.Message.Usage.CacheRead,
			CacheCreation: e.Message.Usage.CacheCreation, Output: e.Message.Usage.Output, Messages: 1}
		if e.IsSidechain || subagent {
			msg.Thread = core.UsageThreadSubagent
		}
		at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		acc.add(msg, at, err == nil)
	}
	if len(acc.pending) > 0 {
		// No line had a timestamp: the file's own time is the one there is.
		at := time.Now()
		if info, err := f.Stat(); err == nil {
			at = info.ModTime()
		}
		acc.flush(at)
	}
}

// usageKey is a ledger row's key within one session.
type usageKey struct {
	bucket int64
	model  string
	thread core.UsageThread
}

// usageAccumulator folds one file's messages into ledger rows.
type usageAccumulator struct {
	rows map[usageKey]*core.UsageBucket
	// last is the latest timestamp a message line of the file carried;
	// pending, the messages read before any line carried one.
	last    time.Time
	pending []core.UsageBucket
}

// add folds one message in: at its own time when stamped (has), else at the
// last time a line carried.
func (a *usageAccumulator) add(msg core.UsageBucket, at time.Time, has bool) {
	switch {
	case has:
		a.last = at
		a.flush(at)
		a.fold(msg, at, true)
	case a.last.IsZero():
		a.pending = append(a.pending, msg)
	default:
		a.fold(msg, a.last, false)
	}
}

// flush folds the messages waiting for a time in at at, setting no minute.
func (a *usageAccumulator) flush(at time.Time) {
	for _, msg := range a.pending {
		a.fold(msg, at, false)
	}
	a.pending = nil
}

// fold adds msg to the row of at's bucket, setting at's minute when stamped.
func (a *usageAccumulator) fold(msg core.UsageBucket, at time.Time, stamped bool) {
	bucket := core.UsageBucketStart(at)
	k := usageKey{bucket.Unix(), msg.Model, msg.Thread}
	r := a.rows[k]
	if r == nil {
		r = &core.UsageBucket{Bucket: bucket, Model: msg.Model, Thread: msg.Thread}
		a.rows[k] = r
	}
	r.Input += msg.Input
	r.Cached += msg.Cached
	r.CacheCreation += msg.CacheCreation
	r.Output += msg.Output
	r.Messages += msg.Messages
	if stamped {
		r.MinuteMask |= core.UsageMinuteBit(at)
	}
}
