package diary

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A day's mishap carries the type its report recorded, and its title is the
// text without the prefix; a report from before types (a description alone,
// prefix-shaped or not) reads untyped with its text as written, and a stored
// type outside the vocabulary reads as none.
func TestDay_MishapsCarryTheirType(t *testing.T) {
	f := dmOpen(t, time.UTC)
	day := f.at(2026, 11, 18, 10, 0)
	s := f.session(dmSession{Project: "example-ops", Created: day, Updated: day.Add(time.Hour)})
	f.work(s, day, 30, 30_000, "claude-x", core.UsageThreadMain)
	typed := f.event(core.EventAgentMishap, day.Add(10*time.Minute), s, "example-ops", "",
		map[string]any{"description": "shell-trap: a bare == in zsh died", "type": "shell-trap"})
	old := f.event(core.EventAgentMishap, day.Add(20*time.Minute), s, "example-ops", "",
		map[string]any{"description": "Ran the wrong target"})
	shaped := f.event(core.EventAgentMishap, day.Add(30*time.Minute), s, "example-ops", "",
		map[string]any{"description": "other: written before types", "item_ids": []string{"01MEM"}})
	odd := f.event(core.EventAgentMishap, day.Add(40*time.Minute), s, "example-ops", "",
		map[string]any{"description": "shell-trap: odd", "type": "not-a-type"})

	e := f.day(day)
	require.Len(t, e.Moments.Mishaps, 4)
	byID := map[string]Mishap{}
	for _, m := range e.Moments.Mishaps {
		byID[m.Event.ID] = m
	}
	require.Equal(t, "shell-trap", byID[typed].Type)
	require.Equal(t, "a bare == in zsh died", byID[typed].Event.Title)
	require.Empty(t, byID[old].Type)
	require.Equal(t, "Ran the wrong target", byID[old].Event.Title)
	require.Empty(t, byID[shaped].Type, "a prefix in old text is no recorded type")
	require.Equal(t, "other: written before types", byID[shaped].Event.Title)
	require.Empty(t, byID[odd].Type)
	require.Equal(t, "shell-trap: odd", byID[odd].Event.Title)
	require.Equal(t, s, byID[typed].Session.ID)
}
