package harvest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// usageLine is one synthetic Claude Code assistant line with a message id, a
// timestamp ("" for none), a model, the sidechain flag and a usage block.
func usageLine(id, ts, model string, sidechain bool, in, out, read, create int) string {
	stamp := ""
	if ts != "" {
		stamp = `"timestamp":"` + ts + `",`
	}
	return `{"type":"assistant",` + stamp + `"isSidechain":` + strconv.FormatBool(sidechain) +
		`,"message":{"id":"` + id + `","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"x"}],` +
		`"usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) +
		`,"cache_read_input_tokens":` + strconv.Itoa(read) + `,"cache_creation_input_tokens":` + strconv.Itoa(create) + `}}}`
}

func writeUsageTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// One message is several lines sharing an id and repeating its usage: it
// counts once, in the bucket, model and thread of its first line. The totals
// are the rows' sum and ClaudeTokens' figure.
func TestClaudeUsage_DedupsByMessageID(t *testing.T) {
	path := writeUsageTranscript(t,
		`{"type":"user","timestamp":"2026-11-20T09:00:00Z","message":{"role":"user","content":"go"}}`,
		usageLine("msg_A", "2026-11-20T09:01:10Z", "claude-x", false, 100, 50, 1000, 200),
		usageLine("msg_A", "2026-11-20T09:16:00Z", "claude-x", false, 100, 50, 1000, 200),
		usageLine("msg_A", "2026-11-20T09:16:01Z", "claude-x", false, 100, 50, 1000, 200),
		usageLine("msg_B", "2026-11-20T09:03:59Z", "claude-x", false, 10, 5, 0, 0),
	)
	totals, rows, ok := ClaudeUsage(path)
	require.True(t, ok)
	require.Equal(t, []core.UsageBucket{{
		Bucket: utc("2026-11-20T09:00:00Z"), Model: "claude-x", Thread: core.UsageThreadMain,
		Input: 110, Cached: 1000, CacheCreation: 200, Output: 55, Messages: 2, MinuteMask: 1<<1 | 1<<3,
	}}, rows, "msg_A once, at its first line's minute")
	require.Equal(t, core.TokenUsage{Input: 110, Cached: 1000, CacheCreation: 200, Output: 55, Total: 1365}, totals)
	require.Equal(t, core.SumUsage(rows), totals)
	tokens, ok := ClaudeTokens(path)
	require.True(t, ok)
	require.Equal(t, totals, tokens, "ClaudeTokens is the same pass's totals")
}

// A message lands in the quarter-hour (UTC) its timestamp falls in: the last
// instant of a quarter-hour is in it, the next quarter starts its own bucket,
// and an offset timestamp is placed by its UTC instant. Its minute within the
// bucket sets that bit; models and threads keep rows of their own, sorted.
func TestClaudeUsage_BucketEdges(t *testing.T) {
	path := writeUsageTranscript(t,
		usageLine("m1", "2026-11-20T09:14:59.999Z", "claude-x", false, 1, 0, 0, 0),
		usageLine("m2", "2026-11-20T09:15:00Z", "claude-x", false, 2, 0, 0, 0),
		usageLine("m3", "2026-11-20T11:29:30+02:00", "claude-x", false, 4, 0, 0, 0), // 09:29:30 UTC
		usageLine("m4", "2026-11-20T09:00:00Z", "claude-y", false, 8, 0, 0, 0),
		usageLine("m5", "2026-11-20T09:00:30Z", "claude-x", true, 16, 0, 0, 0),
	)
	_, rows, ok := ClaudeUsage(path)
	require.True(t, ok)
	nine, quarter := utc("2026-11-20T09:00:00Z"), utc("2026-11-20T09:15:00Z")
	require.Equal(t, []core.UsageBucket{
		{Bucket: nine, Model: "claude-x", Thread: core.UsageThreadMain, Input: 1, Messages: 1, MinuteMask: 1 << 14},
		{Bucket: nine, Model: "claude-x", Thread: core.UsageThreadSubagent, Input: 16, Messages: 1, MinuteMask: 1},
		{Bucket: nine, Model: "claude-y", Thread: core.UsageThreadMain, Input: 8, Messages: 1, MinuteMask: 1},
		{Bucket: quarter, Model: "claude-x", Thread: core.UsageThreadMain, Input: 6, Messages: 2, MinuteMask: 1 | 1<<14},
	}, rows)
	for _, r := range rows {
		require.True(t, r.Aligned())
		require.Equal(t, time.UTC, r.Bucket.Location())
	}
}

// A synthetic entry (Claude Code's placeholder) is no model call: it is not a
// message, takes no minute and makes no row. Nor does a line with no usage
// block, or a malformed one; a transcript of only those harvests nothing.
func TestClaudeUsage_SkipsSyntheticAndUsageFreeLines(t *testing.T) {
	path := writeUsageTranscript(t,
		usageLine("msg_syn", "2026-11-20T09:05:00Z", "<synthetic>", false, 0, 0, 0, 0),
		`{"type":"assistant","timestamp":"2026-11-20T09:06:00Z","message":{"id":"msg_text","role":"assistant","model":"claude-x","content":[{"type":"text","text":"no usage"}]}}`,
		`not even json "assistant"`,
		usageLine("msg_real", "2026-11-20T09:07:00Z", "claude-x", false, 3, 4, 0, 0),
	)
	_, rows, ok := ClaudeUsage(path)
	require.True(t, ok)
	require.Equal(t, []core.UsageBucket{{Bucket: utc("2026-11-20T09:00:00Z"), Model: "claude-x", Thread: core.UsageThreadMain,
		Input: 3, Output: 4, Messages: 1, MinuteMask: 1 << 7}}, rows)

	only := writeUsageTranscript(t, usageLine("msg_syn", "2026-11-20T09:05:00Z", "<synthetic>", false, 0, 0, 0, 0))
	_, rows, ok = ClaudeUsage(only)
	require.False(t, ok)
	require.Empty(t, rows)
	for _, p := range []string{"", filepath.Join(t.TempDir(), "absent.jsonl")} {
		_, _, ok := ClaudeUsage(p)
		require.False(t, ok, p)
	}
}

// The parser is stateless: the same file gives the same rows, and a grown file
// gives its earlier rows unchanged plus the new ones -- the rows the store
// replaces the session's with, so a re-harvest never counts twice.
func TestClaudeUsage_AGrownFileIsIdempotent(t *testing.T) {
	first := []string{
		usageLine("msg_A", "2026-11-20T09:01:00Z", "claude-x", false, 100, 50, 0, 0),
		usageLine("msg_B", "2026-11-20T09:20:00Z", "claude-x", false, 10, 5, 0, 0),
	}
	path := writeUsageTranscript(t, first...)
	t1, r1, ok := ClaudeUsage(path)
	require.True(t, ok)
	t1b, r1b, _ := ClaudeUsage(path)
	require.Equal(t, r1, r1b, "same file, same rows")
	require.Equal(t, t1, t1b)

	grown := append(first,
		usageLine("msg_B", "2026-11-20T09:31:00Z", "claude-x", false, 10, 5, 0, 0), // a repeated line of an old message
		usageLine("msg_C", "2026-11-20T09:44:00Z", "claude-x", false, 1, 1, 0, 0),
	)
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(grown, "\n")+"\n"), 0o600))
	t2, r2, ok := ClaudeUsage(path)
	require.True(t, ok)
	require.Equal(t, r1, r2[:len(r1)], "the earlier buckets are as they were")
	require.Equal(t, []core.UsageBucket{{Bucket: utc("2026-11-20T09:30:00Z"), Model: "claude-x", Thread: core.UsageThreadMain,
		Input: 1, Output: 1, Messages: 1, MinuteMask: 1 << 14}}, r2[len(r1):])
	require.Equal(t, t1.Total+2, t2.Total, "the grown file's absolute total, not the sum of two")
}

// A message whose line has no timestamp lands with the last earlier stamped
// one, or before any, with the first later one, and sets no minute; in a file
// with no timestamp at all, at the file's own time.
func TestClaudeUsage_UnstampedMessages(t *testing.T) {
	path := writeUsageTranscript(t,
		usageLine("m0", "", "claude-x", false, 1, 0, 0, 0),
		usageLine("m1", "2026-11-20T09:20:00Z", "claude-x", false, 2, 0, 0, 0),
		usageLine("m2", "", "claude-x", false, 4, 0, 0, 0),
		usageLine("m3", "2026-11-20T09:31:00Z", "claude-x", false, 8, 0, 0, 0),
		usageLine("m4", "not a time", "claude-x", false, 16, 0, 0, 0),
	)
	totals, rows, ok := ClaudeUsage(path)
	require.True(t, ok)
	require.Equal(t, []core.UsageBucket{
		{Bucket: utc("2026-11-20T09:15:00Z"), Model: "claude-x", Thread: core.UsageThreadMain, Input: 7, Messages: 3, MinuteMask: 1 << 5},
		{Bucket: utc("2026-11-20T09:30:00Z"), Model: "claude-x", Thread: core.UsageThreadMain, Input: 24, Messages: 2, MinuteMask: 1 << 1},
	}, rows)
	require.Equal(t, 31, totals.Total, "every message counts")

	bare := writeUsageTranscript(t, usageLine("m0", "", "claude-x", false, 5, 0, 0, 0))
	mtime := time.Date(2026, 11, 20, 13, 52, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(bare, mtime, mtime))
	_, rows, ok = ClaudeUsage(bare)
	require.True(t, ok)
	require.Equal(t, []core.UsageBucket{{Bucket: utc("2026-11-20T13:45:00Z"), Model: "claude-x", Thread: core.UsageThreadMain,
		Input: 5, Messages: 1}}, rows)
}
