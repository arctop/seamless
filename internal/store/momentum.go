// Momentum: the capture streak behind the activity calendar.
//
// The streak counts covered local days -- days on which at least one session
// left a durable artifact (the same covered-ness test as SessionCoverage), so
// it rewards knowledge capture, not raw usage. Day boundaries follow the
// localBucketAxis conventions: local midnights advanced with AddDate, never
// 24h arithmetic, so DST transitions cannot split or merge a day.
//
// The longest-ever run is cached in a settings row and extended incrementally,
// finalized through yesterday; only the days since the cached watermark are
// ever queried, so a load never rewalks the full session history. Today is
// always computed live and never persisted: a day is finalized only once it is
// over, which also gives a session created late yesterday until the first load
// of today for its artifact to land. (An artifact landing two or more days
// after its session's creation day can still be missed by an already-finalized
// day -- accepted: the cache only ever underreports, and longest never
// regresses.)
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// SettingCaptureStreak is the settings key caching the momentum capture
// streak: {"longest":N,"run":R,"through":"2006-01-02"}, finalized through the
// named local day. Deleting the row is safe -- the next load rebuilds it from
// the session table.
const SettingCaptureStreak = "momentum_capture_streak"

// captureStreakCache is the stored shape behind SettingCaptureStreak.
type captureStreakCache struct {
	// Longest is the longest run of consecutive covered days ever finalized.
	Longest int `json:"longest"`
	// Run is the covered-day run length ending at Through (0 when Through was
	// not covered) -- what a still-growing streak continues from.
	Run int `json:"run"`
	// Through is the last finalized local day, as bucketKey formats it.
	Through string `json:"through"`
}

// CaptureStreak reports the momentum capture streak: the current run of
// consecutive covered local days and the longest run ever. Today counts into
// the current run as soon as it is covered; an uncovered today is a pause, not
// a break -- the run through yesterday stands until the day is actually over.
// A momentum-only surface: callers gate on the feature, so a disabled install
// neither queries nor writes the cache.
func CaptureStreak(ctx context.Context, db *sql.DB, now time.Time) (current, longest int, err error) {
	today := localDay(now)
	yesterday := today.AddDate(0, 0, -1)

	cache, found, err := captureStreakCached(ctx, db)
	if err != nil {
		return 0, 0, err
	}
	through, usable := parseDay(cache.Through)
	if !found || !usable || through.After(yesterday) {
		// No usable watermark: a fresh cache, a malformed row, or a clock that
		// moved backwards past it. Rebuild from the whole session table, once.
		cache = captureStreakCache{}
		through = time.Time{}
	}

	if !through.Equal(yesterday) {
		// Finalize the days after the watermark, through yesterday.
		var since time.Time // zero = all sessions (first build)
		if !through.IsZero() {
			since = through.AddDate(0, 0, 1)
		}
		covered, earliest, cerr := coveredDays(ctx, db, since)
		if cerr != nil {
			return 0, 0, cerr
		}
		start := since
		if start.IsZero() {
			if earliest.IsZero() {
				// No sessions at all: nothing to finalize, nothing to store.
				return 0, 0, nil
			}
			start = localDay(earliest)
		}
		for day := start; !day.After(yesterday); day = day.AddDate(0, 0, 1) {
			if covered[bucketKey(day, false)] {
				cache.Run++
				cache.Longest = max(cache.Longest, cache.Run)
			} else {
				cache.Run = 0
			}
		}
		cache.Through = bucketKey(yesterday, false)
		raw, merr := json.Marshal(cache)
		if merr != nil {
			return 0, 0, fmt.Errorf("store.CaptureStreak: encode cache: %w", merr)
		}
		if serr := SetSetting(ctx, db, SettingCaptureStreak, string(raw)); serr != nil {
			return 0, 0, serr
		}
	}

	// Today, live and unpersisted.
	todayCovered, _, err := coveredDays(ctx, db, today)
	if err != nil {
		return 0, 0, err
	}
	current = cache.Run
	if todayCovered[bucketKey(today, false)] {
		current++
	}
	return current, max(cache.Longest, current), nil
}

// captureStreakCached loads the cache row. A corrupt row reads as absent (the
// next finalize rebuilds and rewrites it) rather than failing the page.
func captureStreakCached(ctx context.Context, db *sql.DB) (captureStreakCache, bool, error) {
	var cache captureStreakCache
	raw, found, err := GetSetting(ctx, db, SettingCaptureStreak)
	if err != nil {
		return cache, false, fmt.Errorf("store.CaptureStreak: %w", err)
	}
	if !found {
		return cache, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &cache); err != nil {
		return captureStreakCache{}, false, nil
	}
	return cache, true, nil
}

// coveredDays reports, per local day key, whether any session created since
// the given instant (zero = all time) is covered -- the SessionCoverage test:
// findings, a memory write, a note write, or a recorded trial. It also returns
// the earliest session creation time seen, for the first full build.
func coveredDays(ctx context.Context, db *sql.DB, since time.Time) (map[string]bool, time.Time, error) {
	args := []any{string(core.EventMemoryWritten), string(core.EventNoteWritten), string(core.EventTrialRecorded)}
	q := `
		SELECT
			s.created_at,
			CASE WHEN
				s.findings <> ''
				OR EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?)
				OR EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?)
				OR EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = ?)
			THEN 1 ELSE 0 END AS covered
		FROM sessions s`
	if !since.IsZero() {
		q += ` WHERE s.created_at >= ?`
		args = append(args, core.FormatTime(since))
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store.coveredDays: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	var earliest time.Time
	for rows.Next() {
		var createdAt string
		var covered int
		if err := rows.Scan(&createdAt, &covered); err != nil {
			return nil, time.Time{}, fmt.Errorf("store.coveredDays: scan: %w", err)
		}
		ts, err := core.ParseTime(createdAt)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("store.coveredDays: parse ts: %w", err)
		}
		if earliest.IsZero() || ts.Before(earliest) {
			earliest = ts
		}
		if covered == 1 {
			out[bucketKey(ts, false)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("store.coveredDays: %w", err)
	}
	return out, earliest, nil
}

// SpotlightMemory is the momentum "memory of the month": the active memory
// with the highest utility gain inside the spotlight window, with the counts
// that back the claim up.
type SpotlightMemory struct {
	ItemID  string  `json:"itemId"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Project string  `json:"project"`
	Utility float64 `json:"utility"` // windowed utility gain (house weights, per-session dedup, decay)
	Reads   int     `json:"reads"`   // memory.read events in the window
	Readers int     `json:"readers"` // distinct sessions that read or pulled it
	Injects int     `json:"injects"` // query-gated injections in the window
}

// MemorySpotlight returns the active memory that gained the most utility since
// the given instant -- the same signal classes, weights, per-session dedup,
// and decay as RebuildRetrievalStats, restricted to the window -- with its
// windowed read and reader counts. found is false when no active memory earned
// any query-gated utility in the window: the honest empty state. A
// momentum-only surface: callers gate on the feature before asking.
func MemorySpotlight(ctx context.Context, db *sql.DB, since, now time.Time) (SpotlightMemory, bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, kind, session_id, item_id, payload FROM events
		WHERE kind IN (?, ?) AND ts >= ?
		ORDER BY ts ASC, id ASC`,
		string(core.EventInjected), string(core.EventMemoryRead), core.FormatTime(since))
	if err != nil {
		return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type gain struct {
		utility float64
		reads   int
		injects int
		readers map[string]struct{}
	}
	gains := map[string]*gain{}
	get := func(id string) *gain {
		g := gains[id]
		if g == nil {
			g = &gain{readers: map[string]struct{}{}}
			gains[id] = g
		}
		return g
	}
	seen := map[string]struct{}{}
	credit := func(g *gain, id, class, session string, weight float64, ts time.Time) {
		if weight == 0 {
			return
		}
		if session != "" {
			g.readers[session] = struct{}{}
			key := class + "\x00" + session + "\x00" + id
			if _, dup := seen[key]; dup {
				return
			}
			seen[key] = struct{}{}
		}
		g.utility += weight * utilityDecay(now.Sub(ts))
	}
	for rows.Next() {
		var tsStr, kind, sessionID, itemID, payload string
		if err := rows.Scan(&tsStr, &kind, &sessionID, &itemID, &payload); err != nil {
			return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: parse ts: %w", err)
		}
		switch core.EventKind(kind) {
		case core.EventInjected:
			p, _ := decodeInjectedPayload(payload)
			weight, class := injectedUtilityWeight(p)
			if weight == 0 {
				continue
			}
			session := injectedSessionKey(sessionID, p)
			for _, id := range injectedItemIDs(itemID, p) {
				g := get(id)
				g.injects++
				credit(g, id, class, session, weight, ts)
			}
		case core.EventMemoryRead:
			if itemID == "" {
				continue
			}
			g := get(itemID)
			g.reads++
			credit(g, itemID, "read", sessionID, utilityWeightRead, ts)
		}
	}
	if err := rows.Err(); err != nil {
		return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: rows: %w", err)
	}
	if len(gains) == 0 {
		return SpotlightMemory{}, false, nil
	}

	// Resolve winners against the ACTIVE memory index: a superseded or archived
	// memory is history, not a spotlight, and note ids fall away here too.
	ids := make([]string, 0, len(gains))
	args := make([]any, 0, len(gains))
	for id := range gains {
		ids = append(ids, "?")
		args = append(args, id)
	}
	mrows, err := db.QueryContext(ctx, `
		SELECT id, name, kind, project FROM memories_index
		WHERE invalid_at IS NULL AND id IN (`+strings.Join(ids, ",")+`)`, args...)
	if err != nil {
		return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: resolve: %w", err)
	}
	defer func() { _ = mrows.Close() }()
	best := SpotlightMemory{}
	found := false
	for mrows.Next() {
		var id, name, kind, project string
		if err := mrows.Scan(&id, &name, &kind, &project); err != nil {
			return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: scan memory: %w", err)
		}
		g := gains[id]
		if g == nil || g.utility <= 0 {
			continue
		}
		cand := SpotlightMemory{
			ItemID: id, Name: name, Kind: kind, Project: project,
			Utility: g.utility, Reads: g.reads, Readers: len(g.readers), Injects: g.injects,
		}
		if !found || cand.Utility > best.Utility ||
			(cand.Utility == best.Utility && (cand.Reads > best.Reads ||
				(cand.Reads == best.Reads && cand.Name < best.Name))) {
			best, found = cand, true
		}
	}
	if err := mrows.Err(); err != nil {
		return SpotlightMemory{}, false, fmt.Errorf("store.MemorySpotlight: rows: %w", err)
	}
	return best, found, nil
}

// PlanShippedRef identifies one shipped plan: the plan.shipped settlement
// event's project and plan slug (its item_id).
type PlanShippedRef struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
}

// PlansShippedSince returns the plan.shipped settlements recorded at or after
// since, oldest first -- one per plan, because the settlement is latched
// once-ever per plan slug at mint time. It backs the Plans page's monthly
// shipped count and settle marks; a momentum-only surface, so callers gate on
// the feature before asking and a disabled install never runs the query.
func PlansShippedSince(ctx context.Context, db *sql.DB, since time.Time) ([]PlanShippedRef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT project_slug, item_id FROM events
		 WHERE kind = ? AND ts >= ?
		 ORDER BY ts ASC, id ASC`,
		string(core.EventPlanShipped), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.PlansShippedSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PlanShippedRef
	for rows.Next() {
		var ref PlanShippedRef
		if err := rows.Scan(&ref.Project, &ref.Slug); err != nil {
			return nil, fmt.Errorf("store.PlansShippedSince: scan: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PlansShippedSince: %w", err)
	}
	return out, nil
}

// localDay floors t to its local midnight, the day-boundary convention every
// momentum surface shares with localBucketAxis.
func localDay(t time.Time) time.Time {
	l := t.Local()
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location())
}

// parseDay parses a bucketKey-formatted local day back to its local midnight.
// ok is false for an empty or malformed key.
func parseDay(key string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", key, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
