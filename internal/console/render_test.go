package console

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// TestTS formats a title= tooltip timestamp as a clean minute-precision UTC
// stamp, and renders "" for a nil/zero time so the attribute stays empty.
func TestTS(t *testing.T) {
	tm := time.Date(2026, 7, 14, 7, 29, 45, 123456789, time.UTC)
	require.Equal(t, "2026-07-14 07:29 UTC", ts(tm))

	// A pointer is dereferenced; nil and zero render empty.
	require.Equal(t, "2026-07-14 07:29 UTC", ts(&tm))
	require.Equal(t, "", ts((*time.Time)(nil)))
	require.Equal(t, "", ts(time.Time{}))

	// A non-time value is ignored rather than panicking.
	require.Equal(t, "", ts("not a time"))

	// Non-UTC input is normalized to UTC.
	loc := time.FixedZone("PST", -8*3600)
	require.Equal(t, "2026-07-14 15:29 UTC", ts(time.Date(2026, 7, 14, 7, 29, 0, 0, loc)))
}

// TestClock renders a wall-clock time the browser localizes: the instant in
// datetime, the daemon's local wall clock as the text, and no zone named
// (owner decision 2026-11-08).
func TestClock(t *testing.T) {
	tm := time.Date(2026, 11, 8, 22, 5, 30, 0, time.UTC)
	require.Equal(t, `<time datetime="2026-11-08T22:05:30Z" data-clock="time">`+tm.Local().Format("15:04")+`</time>`,
		string(clock(tm)))
	require.Equal(t, `<time datetime="2026-11-08T22:05:30Z" data-clock="date">`+tm.Local().Format("2006-01-02 15:04")+`</time>`,
		string(clockDate(&tm)))
	require.Empty(t, clock((*time.Time)(nil)))
	require.Empty(t, clock(time.Time{}))
	require.Empty(t, clockDate("not a time"))
}

// TestShortID uses the last 8 chars so recent ULIDs (shared timestamp prefix)
// stay distinguishable, matching the Interactions client's id.slice(-8).
func TestShortID(t *testing.T) {
	require.Equal(t, "ABCDEFGH", shortID("01KXFM0000ABCDEFGH"))
	require.Equal(t, "short", shortID("short")) // <=8 returned as-is
}

func TestCompactNum(t *testing.T) {
	require.Equal(t, "0", compactNum(0))
	require.Equal(t, "999", compactNum(999))
	require.Equal(t, "1k", compactNum(1000))
	require.Equal(t, "12.4k", compactNum(12400))
	require.Equal(t, "999.9k", compactNum(999949))
	require.Equal(t, "1.2M", compactNum(1_200_000))
}

// Every event kind the log can record reads as words, and a kind added later
// without an entry is spelled out from its parts rather than shown raw.
func TestEvtLabel_HumanNames(t *testing.T) {
	for _, k := range []core.EventKind{
		core.EventSessionStarted, core.EventSessionEnded, core.EventMemoryWritten, core.EventMemoryRead,
		core.EventMemorySuperseded, core.EventMemoryArchived, core.EventMemoryMoved, core.EventRepoMoved,
		core.EventFavoriteChanged, core.EventNoteWritten, core.EventNoteRead, core.EventTrialRecorded,
		core.EventMemoryFirstReuse, core.EventProjectStage, core.EventRecordBroken, core.EventTaskTransition,
		core.EventInjected, core.EventGardenerAction, core.EventToolCall, core.EventHookPrompt,
		core.EventRecallMiss, core.EventHookError, core.EventAgentMishap, core.EventPlanCaptured,
		core.EventPlanPresented, core.EventPlanApproved, core.EventPlanShipped, core.EventSubagentCaptured,
		core.EventProjectIsolationChanged, eventFeaturesChanged, eventLevelChanged,
		core.EventUpdateAvailable, core.EventUpdateApplied, core.EventUpdateStarted, core.EventUpdateFailed,
		core.EventMemoryScreened, core.EventMemoryHeld, core.EventMemoryReleased, core.EventMemoryDiscarded,
		core.EventSessionTainted, core.EventSessionCleared,
	} {
		_, ok := eventLabels[string(k)]
		require.True(t, ok, "event kind %q has no human label", k)
	}
	require.Equal(t, "Project matured", evtLabel("project.stage_reached"))
	require.Equal(t, "Widget spun up", evtLabel("widget.spun_up"))
	require.Equal(t, "Event", evtLabel(""))
}
