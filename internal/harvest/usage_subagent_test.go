package harvest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// subagentSession writes a session transcript where Claude Code keeps one, in
// a folder of its own, and returns its path and its subagents folder (not yet
// made).
func subagentSession(t *testing.T, lines ...string) (transcript, subagents string) {
	t.Helper()
	dir := t.TempDir()
	transcript = filepath.Join(dir, "0b9c51e7-session.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return transcript, filepath.Join(dir, "0b9c51e7-session", "subagents")
}

// writeSubagent writes one file into the subagents folder, making it.
func writeSubagent(t *testing.T, subagents, name string, lines ...string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(subagents, 0o700))
	path := filepath.Join(subagents, name)
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

// A subagent's transcript beside the session's is folded into the session's
// usage: its messages are the subagent thread's, sidechain lines or not, under
// their own model, in their own quarter-hours, and the totals are the sum of
// every file. The files beside them that are not transcripts are not read.
func TestClaudeUsage_FoldsSubagentTranscripts(t *testing.T) {
	transcript, subagents := subagentSession(t,
		usageLine("m1", "2026-11-20T09:01:00Z", "claude-x", false, 100, 50, 1000, 200),
		usageLine("m2", "2026-11-20T09:20:00Z", "claude-x", false, 10, 5, 0, 0),
	)
	writeSubagent(t, subagents, "agent-a1.jsonl",
		`{"type":"user","isSidechain":true,"timestamp":"2026-11-20T09:02:00Z","message":{"role":"user","content":"explore"}}`,
		usageLine("s1", "2026-11-20T09:02:30Z", "claude-small", true, 7, 3, 70, 0),
		usageLine("s1", "2026-11-20T09:02:31Z", "claude-small", true, 7, 3, 70, 0), // one message, two lines
		usageLine("s2", "2026-11-20T09:16:00Z", "claude-small", true, 1, 1, 0, 0),
	)
	// A line without the sidechain flag is still the subagent's.
	writeSubagent(t, subagents, "agent-b2.jsonl",
		usageLine("s3", "2026-11-20T09:05:00Z", "claude-x", false, 2, 2, 0, 0))
	writeSubagent(t, subagents, "agent-a1.meta.json",
		usageLine("not-a-transcript", "2026-11-20T09:05:00Z", "claude-x", false, 9999, 0, 0, 0))
	writeSubagent(t, subagents, "agent-c3.forked-skill.json",
		usageLine("not-a-transcript-either", "2026-11-20T09:05:00Z", "claude-x", false, 9999, 0, 0, 0))

	totals, rows, ok := ClaudeUsage(transcript)
	require.True(t, ok)
	nine, quarter := utc("2026-11-20T09:00:00Z"), utc("2026-11-20T09:15:00Z")
	require.Equal(t, []core.UsageBucket{
		{Bucket: nine, Model: "claude-small", Thread: core.UsageThreadSubagent, Input: 7, Cached: 70, Output: 3,
			Messages: 1, MinuteMask: 1 << 2},
		{Bucket: nine, Model: "claude-x", Thread: core.UsageThreadMain, Input: 100, Cached: 1000, CacheCreation: 200,
			Output: 50, Messages: 1, MinuteMask: 1 << 1},
		{Bucket: nine, Model: "claude-x", Thread: core.UsageThreadSubagent, Input: 2, Output: 2, Messages: 1, MinuteMask: 1 << 5},
		{Bucket: quarter, Model: "claude-small", Thread: core.UsageThreadSubagent, Input: 1, Output: 1, Messages: 1, MinuteMask: 1 << 1},
		{Bucket: quarter, Model: "claude-x", Thread: core.UsageThreadMain, Input: 10, Output: 5, Messages: 1, MinuteMask: 1 << 5},
	}, rows)
	require.Equal(t, core.TokenUsage{Input: 120, Cached: 1070, CacheCreation: 200, Output: 61, Total: 1451}, totals)
	require.Equal(t, core.SumUsage(rows), totals)
	tokens, ok := ClaudeTokens(transcript)
	require.True(t, ok)
	require.Equal(t, totals, tokens, "ClaudeTokens counts the subagents too")
	require.Len(t, SubagentTranscripts(transcript), 2, "the two agent-*.jsonl files, nothing else")
}

// One set of message ids spans the session's transcript and its subagents': a
// forked subagent opens with a copy of its parent's last message, and two
// forks of one parent may both carry it. It counts once, where it was read
// first -- the session's own transcript, then the subagents' in name order --
// and a message two subagent files share counts once too.
func TestClaudeUsage_DedupsAcrossTheSubagentFiles(t *testing.T) {
	transcript, subagents := subagentSession(t,
		usageLine("shared-with-parent", "2026-11-20T09:01:00Z", "claude-x", false, 100, 10, 0, 0),
	)
	writeSubagent(t, subagents, "agent-fork1.jsonl",
		`{"type":"fork-context-ref"}`,
		usageLine("shared-with-parent", "2026-11-20T09:40:00Z", "claude-x", true, 100, 10, 0, 0),
		usageLine("shared-by-forks", "2026-11-20T09:41:00Z", "claude-x", true, 5, 5, 0, 0),
		usageLine("fork1-own", "2026-11-20T09:42:00Z", "claude-x", true, 1, 0, 0, 0),
	)
	writeSubagent(t, subagents, "agent-fork2.jsonl",
		`{"type":"fork-context-ref"}`,
		usageLine("shared-with-parent", "2026-11-20T09:50:00Z", "claude-x", true, 100, 10, 0, 0),
		usageLine("shared-by-forks", "2026-11-20T09:51:00Z", "claude-x", true, 5, 5, 0, 0),
		usageLine("fork2-own", "2026-11-20T09:52:00Z", "claude-x", true, 2, 0, 0, 0),
	)
	totals, rows, ok := ClaudeUsage(transcript)
	require.True(t, ok)
	require.Equal(t, []core.UsageBucket{
		{Bucket: utc("2026-11-20T09:00:00Z"), Model: "claude-x", Thread: core.UsageThreadMain, Input: 100, Output: 10,
			Messages: 1, MinuteMask: 1 << 1},
		{Bucket: utc("2026-11-20T09:30:00Z"), Model: "claude-x", Thread: core.UsageThreadSubagent, Input: 6, Output: 5,
			Messages: 2, MinuteMask: 1<<11 | 1<<12},
		{Bucket: utc("2026-11-20T09:45:00Z"), Model: "claude-x", Thread: core.UsageThreadSubagent, Input: 2,
			Messages: 1, MinuteMask: 1 << 7},
	}, rows, "the parent's copy stays the parent's; the forks' shared message is fork1's")
	require.Equal(t, 100+10+5+5+1+2, totals.Total)
}

// Nothing in the subagents folder is followed: a link named like a transcript,
// pointing at one, is skipped, and so is a folder of that name. A subagents
// folder that is itself a link lists nothing.
func TestClaudeUsage_SkipsLinksAndNonRegularEntries(t *testing.T) {
	transcript, subagents := subagentSession(t,
		usageLine("m1", "2026-11-20T09:01:00Z", "claude-x", false, 1, 0, 0, 0))
	writeSubagent(t, subagents, "agent-real.jsonl", usageLine("s1", "2026-11-20T09:02:00Z", "claude-x", true, 10, 0, 0, 0))
	outside := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	require.NoError(t, os.WriteFile(outside, []byte(usageLine("x1", "2026-11-20T09:03:00Z", "claude-x", true, 1000, 0, 0, 0)+"\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(subagents, "agent-folder.jsonl"), 0o700))
	linked := os.Symlink(outside, filepath.Join(subagents, "agent-link.jsonl")) == nil

	totals, _, ok := ClaudeUsage(transcript)
	require.True(t, ok)
	require.Equal(t, 11, totals.Total, "the session's own and the real subagent's: no link, no folder")
	names := []string{}
	for _, s := range SubagentTranscripts(transcript) {
		names = append(names, filepath.Base(s.Path))
	}
	require.Equal(t, []string{"agent-real.jsonl"}, names)
	if !linked {
		t.Skip("this system makes no symlinks here; the folder entry was still skipped")
	}

	// The subagents folder itself a link to a folder of transcripts: not read.
	other, otherSubagents := subagentSession(t,
		usageLine("m1", "2026-11-20T09:01:00Z", "claude-x", false, 1, 0, 0, 0))
	require.NoError(t, os.MkdirAll(filepath.Dir(otherSubagents), 0o700))
	require.NoError(t, os.Symlink(subagents, otherSubagents))
	require.Empty(t, SubagentTranscripts(other))
	totals, _, ok = ClaudeUsage(other)
	require.True(t, ok)
	require.Equal(t, 1, totals.Total, "a linked subagents folder is not followed")
}

// A session with no subagents folder -- or a transcript that is not a .jsonl
// file, whose folder Claude Code does not keep -- reads exactly as before, and
// an unreadable subagent transcript is skipped, never an error.
func TestClaudeUsage_NoSubagentsFolder(t *testing.T) {
	transcript, subagents := subagentSession(t,
		usageLine("m1", "2026-11-20T09:01:00Z", "claude-x", false, 3, 4, 0, 0))
	require.Empty(t, SubagentTranscripts(transcript))
	totals, rows, ok := ClaudeUsage(transcript)
	require.True(t, ok)
	require.Len(t, rows, 1)
	require.Equal(t, 7, totals.Total)

	// An empty folder, and a file the reader may not open.
	require.NoError(t, os.MkdirAll(subagents, 0o700))
	locked := writeSubagent(t, subagents, "agent-locked.jsonl", usageLine("s1", "2026-11-20T09:02:00Z", "claude-x", true, 50, 0, 0, 0))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	if f, err := os.Open(locked); err == nil {
		_ = f.Close() // the superuser, or a system without modes: the file is readable after all
	} else {
		totals, _, ok = ClaudeUsage(transcript)
		require.True(t, ok)
		require.Equal(t, 7, totals.Total, "the unreadable subagent transcript is skipped")
	}

	require.Empty(t, SubagentTranscripts(strings.TrimSuffix(transcript, ".jsonl")+".txt"))
	require.Empty(t, SubagentTranscripts(""))
	_, _, ok = ClaudeUsage(filepath.Join(filepath.Dir(transcript), "absent.jsonl"))
	require.False(t, ok, "no session transcript: nothing, whatever lies beside it")
}

// An unstamped subagent line takes its time from its own file -- the first
// later stamped line there, or the file's modification time -- never from the
// session's transcript.
func TestClaudeUsage_SubagentClockIsItsOwn(t *testing.T) {
	transcript, subagents := subagentSession(t,
		usageLine("m1", "2026-11-20T09:01:00Z", "claude-x", false, 1, 0, 0, 0))
	writeSubagent(t, subagents, "agent-a.jsonl",
		usageLine("s1", "", "claude-x", true, 2, 0, 0, 0),
		usageLine("s2", "2026-11-20T11:31:00Z", "claude-x", true, 4, 0, 0, 0))
	bare := writeSubagent(t, subagents, "agent-b.jsonl", usageLine("s3", "", "claude-x", true, 8, 0, 0, 0))
	mtime := time.Date(2026, 11, 20, 13, 52, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(bare, mtime, mtime))

	_, rows, ok := ClaudeUsage(transcript)
	require.True(t, ok)
	require.Equal(t, []core.UsageBucket{
		{Bucket: utc("2026-11-20T09:00:00Z"), Model: "claude-x", Thread: core.UsageThreadMain, Input: 1, Messages: 1, MinuteMask: 1 << 1},
		{Bucket: utc("2026-11-20T11:30:00Z"), Model: "claude-x", Thread: core.UsageThreadSubagent, Input: 6, Messages: 2, MinuteMask: 1 << 1},
		{Bucket: utc("2026-11-20T13:45:00Z"), Model: "claude-x", Thread: core.UsageThreadSubagent, Input: 8, Messages: 1},
	}, rows)
}

// A subagent transcript swapped between the listing and the open -- for a link,
// or another file -- is not read: the file opened must be the one listed.
func TestClaudeUsage_ASwappedSubagentFileIsNotRead(t *testing.T) {
	transcript, subagents := subagentSession(t,
		usageLine("m1", "2026-11-20T09:01:00Z", "claude-x", false, 1, 0, 0, 0))
	writeSubagent(t, subagents, "agent-a.jsonl", usageLine("s1", "2026-11-20T09:02:00Z", "claude-x", true, 10, 0, 0, 0))
	listed := SubagentTranscripts(transcript)
	require.Len(t, listed, 1)
	other := writeSubagent(t, t.TempDir(), "agent-other.jsonl", usageLine("x1", "2026-11-20T09:03:00Z", "claude-x", true, 100, 0, 0, 0))

	fold := usageFold{rows: map[usageKey]*core.UsageBucket{}, seen: map[string]struct{}{}}
	fold.subagent(SubagentFile{Path: other, Info: listed[0].Info})
	require.Empty(t, fold.rows, "the file opened is not the one listed")
	fold.subagent(listed[0])
	require.Len(t, fold.rows, 1, "the one listed is read")
}
