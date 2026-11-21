package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A bucket is the quarter-hour, in UTC, an instant falls in, whatever zone the
// instant is written in; its minute within the bucket is one bit of the mask.
func TestUsageBucketStartAndMinuteBit(t *testing.T) {
	for _, tc := range []struct {
		at     string
		bucket string
		bit    int
	}{
		{"2026-11-20T09:00:00Z", "2026-11-20T09:00:00Z", 1},
		{"2026-11-20T09:14:59.999Z", "2026-11-20T09:00:00Z", 1 << 14},
		{"2026-11-20T09:15:00Z", "2026-11-20T09:15:00Z", 1},
		{"2026-11-20T23:59:59Z", "2026-11-20T23:45:00Z", 1 << 14},
		{"2026-11-21T05:30:10+05:30", "2026-11-21T00:00:00Z", 1}, // a half-hour zone's midnight
		{"2026-11-20T18:52:00-05:45", "2026-11-21T00:30:00Z", 1 << 7},
	} {
		at, err := time.Parse(time.RFC3339Nano, tc.at)
		require.NoError(t, err)
		want, err := time.Parse(time.RFC3339, tc.bucket)
		require.NoError(t, err)
		got := UsageBucketStart(at)
		require.True(t, want.Equal(got), "%s: %s", tc.at, got)
		require.Equal(t, time.UTC, got.Location(), tc.at)
		require.Equal(t, tc.bit, UsageMinuteBit(at), tc.at)
		require.True(t, UsageBucket{Bucket: got}.Aligned())
	}
	require.False(t, UsageBucket{Bucket: time.Date(2026, 11, 20, 9, 1, 0, 0, time.UTC)}.Aligned())
	require.False(t, UsageBucket{Bucket: time.Date(2026, 11, 20, 9, 0, 0, 1, time.UTC)}.Aligned())
}

func TestUsageBucket_TokensMinutesAndSum(t *testing.T) {
	a := UsageBucket{Input: 1, Cached: 2, CacheCreation: 3, Output: 4, MinuteMask: 0b1011}
	b := UsageBucket{Input: 10, Output: 1, MinuteMask: UsageMinuteMaskAll}
	require.Equal(t, TokenUsage{Input: 1, Cached: 2, CacheCreation: 3, Output: 4, Total: 10}, a.Tokens())
	require.Equal(t, 3, a.ActiveMinutes())
	require.Equal(t, UsageMinutes, b.ActiveMinutes())
	require.Equal(t, TokenUsage{Input: 11, Cached: 2, CacheCreation: 3, Output: 5, Total: 21}, SumUsage([]UsageBucket{a, b}))
	require.True(t, SumUsage(nil).Empty())
	require.True(t, UsageThreadMain.Valid())
	require.True(t, UsageThreadSubagent.Valid())
	require.False(t, UsageThread("sidechain").Valid())
	require.False(t, UsageThread("").Valid())
}
