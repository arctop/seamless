// Gamification: the day tape and the personal-records latch behind the Now
// screen's arcade layer.
//
// Every number here is judged from recorded activity -- the tasks table and the
// event log -- never invented. Day boundaries follow the localDay conventions
// momentum established (local midnights advanced with AddDate, never 24h
// arithmetic). Records only ever move forward, like the maturity latch: a
// quiet day is an empty tape, not a lost record. The ledger starts from the
// history rather than from nothing (EvaluateGamificationRecords), so deleting
// the settings row merely seeds it again.
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
// to (exclusive) in one round trip.
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
	// Seen marks a best the ledger found when it began rather than one it saw
	// fall: the agents live at that moment, a count with no history to judge
	// it by. Day is the day it was seen. Beating it is a record; the seed is
	// not, so it is never celebrated.
	Seen bool `json:"seen,omitempty"`
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
// nothing.
//
// With no ledger yet -- none stored, or a corrupt row -- it is seeded before
// today is judged: the best day for tasks closed and memories written comes
// from the tasks table and the event log through yesterday, with its real day,
// and the agents live now are the live-agent mark, Seen. So the first look at
// the records is not a day of records broken against an empty ledger: today
// is a record only if it beats the history.
func EvaluateGamificationRecords(ctx context.Context, db *sql.DB, today DayCounts, liveAgents int, now time.Time) (GamificationRecords, []RecordCrossing, error) {
	return EvaluateGamificationRecordsWith(ctx, db, db, today, liveAgents, now)
}

// EvaluateGamificationRecordsWith is EvaluateGamificationRecords reading the
// stored records through db and writing them through cacheDB (see
// CaptureStreakWith).
func EvaluateGamificationRecordsWith(ctx context.Context, db, cacheDB *sql.DB, today DayCounts, liveAgents int, now time.Time) (GamificationRecords, []RecordCrossing, error) {
	recs, found, err := gamificationRecordsStored(ctx, db)
	if err != nil {
		return GamificationRecords{}, nil, err
	}
	day := bucketKey(localDay(now), false)
	if !found {
		if recs, err = gamificationRecordsSeed(ctx, db, localDay(now)); err != nil {
			return GamificationRecords{}, nil, err
		}
		if liveAgents > 0 {
			recs.LiveAgents = RecordMark{N: liveAgents, Day: day, Seen: true}
		}
	}
	var crossings []RecordCrossing
	latch := func(mark *RecordMark, n int, key, label string) {
		if n <= mark.N {
			return
		}
		crossings = append(crossings, RecordCrossing{Key: key, Label: label, N: n, Prev: mark.N})
		*mark = RecordMark{N: n, Day: day}
	}
	latch(&recs.TasksDay, today.TasksClosed, "tasks_day", "most tasks closed in a day")
	latch(&recs.MemoriesDay, today.MemoriesWritten, "memories_day", "most memories written in a day")
	latch(&recs.LiveAgents, liveAgents, "live_agents", "most agents live at once")
	if len(crossings) > 0 || !found {
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

// gamificationRecordsSeed judges the day records from the history before
// today (a local midnight): the most tasks closed done, by their closed_at,
// and the most memory.written events on any one local day, each with the
// earliest day that held it. Tasks reopened and closed again count on their
// last close, so the seed can only under-report. The live-agent record has no
// history and is left to the caller.
func gamificationRecordsSeed(ctx context.Context, db *sql.DB, today time.Time) (GamificationRecords, error) {
	before := core.FormatTime(today)
	var recs GamificationRecords
	var err error
	if recs.TasksDay, err = bestDay(ctx, db,
		`SELECT closed_at FROM tasks WHERE status = ? AND closed_at IS NOT NULL AND closed_at <> '' AND closed_at < ?`,
		string(core.TaskDone), before); err != nil {
		return GamificationRecords{}, fmt.Errorf("store.gamificationRecordsSeed: tasks: %w", err)
	}
	if recs.MemoriesDay, err = bestDay(ctx, db,
		`SELECT ts FROM events WHERE kind = ? AND ts < ?`,
		string(core.EventMemoryWritten), before); err != nil {
		return GamificationRecords{}, fmt.Errorf("store.gamificationRecordsSeed: memories: %w", err)
	}
	return recs, nil
}

// bestDay buckets the timestamps a one-column query returns by local day and
// returns the fullest day: its count and key, the earliest on a tie. No rows
// is the zero mark, an unset record. A timestamp that does not parse is not
// counted rather than failing the seed, which a failure would retry on every
// render: the seed under-reports, never invents.
func bestDay(ctx context.Context, db *sql.DB, query string, args ...any) (RecordMark, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return RecordMark{}, err
	}
	defer func() { _ = rows.Close() }()
	perDay := map[string]int{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return RecordMark{}, err
		}
		ts, err := core.ParseTime(raw)
		if err != nil {
			continue
		}
		perDay[bucketKey(localDay(ts), false)]++
	}
	if err := rows.Err(); err != nil {
		return RecordMark{}, err
	}
	var best RecordMark
	for day, n := range perDay {
		if n > best.N || (n == best.N && day < best.Day) {
			best = RecordMark{N: n, Day: day}
		}
	}
	return best, nil
}

// gamificationRecordsStored loads the ledger row, and whether there is one to
// go by. A corrupt row is none (the ledger is seeded again) rather than a
// failed page.
func gamificationRecordsStored(ctx context.Context, db *sql.DB) (GamificationRecords, bool, error) {
	var recs GamificationRecords
	raw, found, err := GetSetting(ctx, db, SettingGamificationRecords)
	if err != nil {
		return recs, false, fmt.Errorf("store.gamificationRecords: %w", err)
	}
	if !found {
		return recs, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		return GamificationRecords{}, false, nil
	}
	return recs, true, nil
}
