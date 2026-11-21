package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The day model reads a mishap's recorded type beside its description; a
// report from before types reads none, and another kind's "type" field is
// never read as one.
func TestDiaryEvents_ReadTheMishapType(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	diaryEventAt(t, db, "01EV0000000000000000000001", day.Add(time.Hour), core.EventAgentMishap, "S1", "example-ops", "",
		map[string]any{"description": "shell-trap: a bare == died", "type": "shell-trap"})
	diaryEventAt(t, db, "01EV0000000000000000000002", day.Add(2*time.Hour), core.EventAgentMishap, "S1", "example-ops", "",
		map[string]any{"description": "pkill matched the live daemon"})
	diaryEventAt(t, db, "01EV0000000000000000000003", day.Add(3*time.Hour), core.EventAgentMishap, "S1", "example-ops", "",
		map[string]any{"description": "odd", "type": 7})
	diaryEventAt(t, db, "01EV0000000000000000000004", day.Add(4*time.Hour), core.EventSessionFlagged, "S1", "example-ops", "",
		map[string]any{"verdict": "bad", "type": "shell-trap"})

	evs, err := DiaryEvents(ctx, db, day, day.AddDate(0, 0, 1), []core.EventKind{core.EventAgentMishap, core.EventSessionFlagged})
	require.NoError(t, err)
	require.Len(t, evs, 4)
	require.Equal(t, "shell-trap", evs[0].MishapType)
	require.Equal(t, "shell-trap: a bare == died", evs[0].Description, "the description keeps its prefix")
	require.Empty(t, evs[1].MishapType, "an old report names no type")
	require.Equal(t, "pkill matched the live daemon", evs[1].Description)
	require.Equal(t, "7", evs[2].MishapType, "read as stored: the day model holds it to the vocabulary")
	require.Empty(t, evs[3].MishapType, "only a mishap carries a mishap type")
}
