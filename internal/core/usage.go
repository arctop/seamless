package core

import (
	"math/bits"
	"slices"
	"time"
)

// The usage ledger (store: session_usage, migration 053): a session's model
// token usage, one row per quarter-hour (UTC), model and thread. It is what
// can say which day, hour or model a session's tokens went to; the session's
// token totals (TokenUsage) are the sum of its rows. A row holds ids, a model
// id, counts and a time, and no text.

// UsageBucketWidth is the ledger's resolution. Every real UTC offset is a
// whole number of quarter-hours, so any local day or hour is an exact union of
// buckets, and a zone or daylight-saving change needs no rewrite.
const UsageBucketWidth = 15 * time.Minute

// UsageMinutes is how many minutes a bucket spans: the bits of
// UsageBucket.MinuteMask that are used.
const UsageMinutes = 15

// UsageMinuteMaskAll is a minute mask with every minute of a bucket set.
const UsageMinuteMaskAll = 1<<UsageMinutes - 1

// UsageThread is whose turns a ledger row counts.
type UsageThread string

const (
	// UsageThreadMain is the agent's own conversation.
	UsageThreadMain UsageThread = "main"
	// UsageThreadSubagent is the subagents it launched (Claude Code's
	// sidechain turns).
	UsageThreadSubagent UsageThread = "subagent"
)

// UsageThreads lists every valid thread.
var UsageThreads = []UsageThread{UsageThreadMain, UsageThreadSubagent}

// Valid reports whether t is a recognized thread.
func (t UsageThread) Valid() bool { return slices.Contains(UsageThreads, t) }

// UsageBucketStart is the start of the quarter-hour bucket holding t, in UTC.
func UsageBucketStart(t time.Time) time.Time { return t.UTC().Truncate(UsageBucketWidth) }

// UsageMinuteBit is the MinuteMask bit of the minute of its bucket that t
// falls in.
func UsageMinuteBit(t time.Time) int { return 1 << (t.UTC().Minute() % UsageMinutes) }

// UsageBucket is one row of a session's usage ledger as a harvest reports it:
// what one model used in one quarter-hour of one thread. The four counts
// follow TokenUsage's fields and meaning (fresh input, cache read, cache
// creation, output).
type UsageBucket struct {
	// Bucket is the quarter-hour's start, in UTC (UsageBucketStart).
	Bucket time.Time `json:"bucket"`
	// Model is the model id verbatim as the provider names it; "" when the
	// harvest could not name one.
	Model  string      `json:"model,omitempty"`
	Thread UsageThread `json:"thread"`

	Input         int `json:"input,omitempty"`
	Cached        int `json:"cached,omitempty"`
	CacheCreation int `json:"cacheCreation,omitempty"`
	Output        int `json:"output,omitempty"`
	// Messages is how many model messages the row counts: Claude Code's
	// assistant messages, one per message id; for Codex, one per turn whose
	// growth the row was credited.
	Messages int `json:"messages,omitempty"`
	// MinuteMask has bit i set when a message landed in minute i of the
	// bucket (UsageMinuteBit). Its population count is the bucket's active
	// minutes.
	MinuteMask int `json:"minuteMask,omitempty"`
}

// Tokens is the row's counts as a TokenUsage, its total the sum of its parts.
func (b UsageBucket) Tokens() TokenUsage {
	t := TokenUsage{Input: b.Input, Cached: b.Cached, CacheCreation: b.CacheCreation, Output: b.Output}
	t.Normalize()
	return t
}

// ActiveMinutes is how many minutes of the bucket a message landed in.
func (b UsageBucket) ActiveMinutes() int {
	return bits.OnesCount(uint(b.MinuteMask & UsageMinuteMaskAll))
}

// Aligned reports whether Bucket is the start of a quarter-hour.
func (b UsageBucket) Aligned() bool { return b.Bucket.Equal(UsageBucketStart(b.Bucket)) }

// SumUsage is the total of rows' counts, its total the sum of its parts.
func SumUsage(rows []UsageBucket) TokenUsage {
	var t TokenUsage
	for _, r := range rows {
		t.Input += r.Input
		t.Cached += r.Cached
		t.CacheCreation += r.CacheCreation
		t.Output += r.Output
	}
	t.Normalize()
	return t
}
