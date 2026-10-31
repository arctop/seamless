// Gamification: the day tape and the personal-records latch behind the Now
// screen's arcade layer.
//
// Every number here is judged from recorded activity -- the tasks table and the
// event log -- never invented. Day boundaries follow the localDay conventions
// momentum established (local midnights advanced with AddDate, never 24h
// arithmetic). Records only ever move forward, like the maturity latch: a
// quiet day is an empty tape, not a lost record, and deleting the settings row
// merely resets the ledger to be re-earned.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// SettingGamificationRecords is the settings key holding the personal-records
// ledger, as EvaluateGamificationRecords maintains it. Deleting the row resets
// the records; nothing else references it.
const SettingGamificationRecords = "gamification_records"

// DayCounts is one day of fleet output, judged from the tasks table and the
// event log. The day tape renders today's; the trailing week average gives its
// comparison.
type DayCounts struct {
	TasksClosed     int `json:"tasksClosed"`     // tasks whose closed_at landed in the window, status done
	MemoriesWritten int `json:"memoriesWritten"` // memory.written events
	NotesWritten    int `json:"notesWritten"`    // note.written events
	PlansTouched    int `json:"plansTouched"`    // distinct (project, plan) whose steps moved
	Sessions        int `json:"sessions"`        // sessions started
}

// GamificationDayCounts judges the fleet's output between from (inclusive) and
// to (exclusive) in one round trip. A gamification-only surface: callers gate
// on the feature before asking, so a disabled install never runs the query.
func GamificationDayCounts(ctx context.Context, db *sql.DB, from, to time.Time) (DayCounts, error) {
	var c DayCounts
	lo, hi := core.FormatTime(from), core.FormatTime(to)
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM tasks WHERE status = 'done' AND closed_at >= ? AND closed_at < ?),
			(SELECT COUNT(*) FROM events WHERE kind = ? AND ts >= ? AND ts < ?),
			(SELECT COUNT(*) FROM events WHERE kind = ? AND ts >= ? AND ts < ?),
			(SELECT COUNT(*) FROM (SELECT DISTINCT project_slug, plan_slug FROM tasks
				WHERE plan_slug <> '' AND updated_at >= ? AND updated_at < ?)),
			(SELECT COUNT(*) FROM sessions WHERE created_at >= ? AND created_at < ?)`,
		lo, hi,
		string(core.EventMemoryWritten), lo, hi,
		string(core.EventNoteWritten), lo, hi,
		lo, hi,
		lo, hi,
	).Scan(&c.TasksClosed, &c.MemoriesWritten, &c.NotesWritten, &c.PlansTouched, &c.Sessions)
	if err != nil {
		return DayCounts{}, fmt.Errorf("store.GamificationDayCounts: %w", err)
	}
	return c, nil
}

// RecordMark is one personal best: the value and the local day it was set.
type RecordMark struct {
	N   int    `json:"n"`
	Day string `json:"day"` // bucketKey-formatted local day
}

// GamificationRecords is the stored personal-bests ledger.
type GamificationRecords struct {
	TasksDay    RecordMark `json:"tasksDay"`    // most tasks closed in one day
	MemoriesDay RecordMark `json:"memoriesDay"` // most memories written in one day
	LiveAgents  RecordMark `json:"liveAgents"`  // most agents live at one moment
}

// RecordCrossing is one record advancing: which record, the new value, and the
// value it beat. The console mints the once-only celebration event from it.
type RecordCrossing struct {
	Key   string // "tasks_day" | "memories_day" | "live_agents"
	Label string // owner-facing phrasing of the record
	N     int
	Prev  int
}

// EvaluateGamificationRecords compares today's judged counts (and the live
// agent count) against the stored ledger, latches any new bests forward, and
// returns the crossings. Forward-only, like the maturity latch: a slower day
// never regresses a record, and re-rendering with unchanged values persists
// nothing. A corrupt or absent row reads as an empty ledger and is simply
// re-earned. A gamification-only surface: callers gate on the feature, so a
// disabled install neither reads nor writes the row.
func EvaluateGamificationRecords(ctx context.Context, db *sql.DB, today DayCounts, liveAgents int, now time.Time) (GamificationRecords, []RecordCrossing, error) {
	return EvaluateGamificationRecordsWith(ctx, db, db, today, liveAgents, now)
}

// EvaluateGamificationRecordsWith is EvaluateGamificationRecords reading the
// stored records through db and writing them through cacheDB (see
// CaptureStreakWith).
func EvaluateGamificationRecordsWith(ctx context.Context, db, cacheDB *sql.DB, today DayCounts, liveAgents int, now time.Time) (GamificationRecords, []RecordCrossing, error) {
	recs, err := gamificationRecordsStored(ctx, db)
	if err != nil {
		return GamificationRecords{}, nil, err
	}
	day := bucketKey(localDay(now), false)
	var crossings []RecordCrossing
	latch := func(mark *RecordMark, n int, key, label string) {
		if n <= mark.N {
			return
		}
		crossings = append(crossings, RecordCrossing{Key: key, Label: label, N: n, Prev: mark.N})
		mark.N, mark.Day = n, day
	}
	latch(&recs.TasksDay, today.TasksClosed, "tasks_day", "most tasks closed in a day")
	latch(&recs.MemoriesDay, today.MemoriesWritten, "memories_day", "most memories written in a day")
	latch(&recs.LiveAgents, liveAgents, "live_agents", "most agents live at once")
	if len(crossings) > 0 {
		raw, merr := json.Marshal(recs)
		if merr != nil {
			return GamificationRecords{}, nil, fmt.Errorf("store.EvaluateGamificationRecords: encode: %w", merr)
		}
		if serr := SetSetting(ctx, cacheDB, SettingGamificationRecords, string(raw)); serr != nil {
			return GamificationRecords{}, nil, serr
		}
	}
	return recs, crossings, nil
}

// gamificationRecordsStored loads the ledger row. A corrupt row reads as empty
// (the records are re-earned) rather than failing the page.
func gamificationRecordsStored(ctx context.Context, db *sql.DB) (GamificationRecords, error) {
	var recs GamificationRecords
	raw, found, err := GetSetting(ctx, db, SettingGamificationRecords)
	if err != nil {
		return recs, fmt.Errorf("store.gamificationRecords: %w", err)
	}
	if !found {
		return recs, nil
	}
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		return GamificationRecords{}, nil
	}
	return recs, nil
}
