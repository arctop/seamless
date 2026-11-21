package diary

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A month's agents are its days' agents summed: the same keys, the same
// minutes, tokens and parts, each agent's sessions counted once over the
// month, in the same order a day uses. The screen colors an agent from them,
// so a key the days use and the month does not would leave that agent
// without its color.
func TestMonth_AgentsAgreeWithEveryDay(t *testing.T) {
	loc := dmZone(t, "America/Los_Angeles")
	f := dmOpen(t, loc)
	f.inTx(func() {
		dmGenerate(f, f.at(2026, 10, 20, 0, 0), 26, 11, dmScale{sessions: 8, ledger: f.at(2026, 11, 4, 0, 0)})
	})
	// Two sessions with no recorded client: one named like a Codex ambient
	// session, which reads as codex, and one explicit session, which names no
	// harness at all.
	named := f.session(dmSession{Name: "cx/named-only", Project: "example-ops", Model: "gpt-c", Explicit: true,
		Created: f.at(2026, 11, 10, 9, 0), Updated: f.at(2026, 11, 10, 10, 0)})
	f.work(named, f.at(2026, 11, 10, 9, 0), 40, 40000, "gpt-c", core.UsageThreadMain)
	explicit := f.session(dmSession{Project: "example-web", Model: "claude-a", Explicit: true,
		Created: f.at(2026, 11, 11, 14, 0), Updated: f.at(2026, 11, 11, 15, 0)})
	f.work(explicit, f.at(2026, 11, 11, 14, 0), 25, 30000, "claude-a", core.UsageThreadMain)

	now := f.at(2026, 11, 14, 15, 0)
	m, err := monthAt(f.ctx, f.db, f.at(2026, 11, 3, 0, 0), now, loc)
	require.NoError(t, err)
	require.NotEmpty(t, m.Agents)

	want := map[string]AgentUsage{}
	daily := map[string][]int{} // each day's session count, by agent
	for _, md := range m.Days {
		day, err := ParseDay(md.Day)
		require.NoError(t, err)
		d, err := dayAt(f.ctx, f.db, time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc), now, loc)
		require.NoError(t, err)
		for _, a := range d.Usage.Agents {
			w := want[a.Key]
			w.Key, w.Harness, w.Model = a.Key, a.Harness, a.Model
			w.Minutes += a.Minutes
			w.Tokens = addTokens(w.Tokens, a.Tokens)
			w.Main = Part{Minutes: w.Main.Minutes + a.Main.Minutes, Tokens: w.Main.Tokens + a.Main.Tokens}
			w.Subagent = Part{Minutes: w.Subagent.Minutes + a.Subagent.Minutes, Tokens: w.Subagent.Tokens + a.Subagent.Tokens}
			w.Approximate = Part{Minutes: w.Approximate.Minutes + a.Approximate.Minutes,
				Tokens: w.Approximate.Tokens + a.Approximate.Tokens}
			want[a.Key] = w
			daily[a.Key] = append(daily[a.Key], a.Sessions)
		}
	}
	require.Len(t, m.Agents, len(want), "the month names every agent its days name, and no other")
	approx := 0
	for _, a := range m.Agents {
		w, ok := want[a.Key]
		require.True(t, ok, "%s is an agent of no day", a.Key)
		got := a
		got.Sessions = 0
		require.Equal(t, w, got, a.Key)
		sum := 0
		for _, n := range daily[a.Key] {
			sum += n
		}
		require.GreaterOrEqual(t, a.Sessions, slices.Max(daily[a.Key]), "%s: no fewer sessions than its busiest day", a.Key)
		require.LessOrEqual(t, a.Sessions, sum, "%s: a session working two days counts once", a.Key)
		if a.Approximate.Tokens > 0 {
			approx++
		}
	}
	require.Positive(t, approx, "the days before the ledger count their stored totals by agent")
	require.True(t, slices.IsSortedFunc(m.Agents, func(x, y AgentUsage) int {
		return cmp.Or(cmp.Compare(y.Minutes, x.Minutes), cmp.Compare(y.Tokens.Total, x.Tokens.Total), cmp.Compare(x.Key, y.Key))
	}), "most agent time first, as a day orders them")

	keys := map[string]bool{}
	for _, a := range m.Agents {
		keys[a.Key] = true
	}
	require.True(t, keys["codex/gpt-c"], "a session's ambient name names its harness when no client was recorded")
	require.True(t, keys["/claude-a"], "an explicit session names no harness")
}
