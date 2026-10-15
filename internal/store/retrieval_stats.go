package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// Utility scoring constants. Utility is a time-decayed sum of QUERY-GATED demand
// signals -- events where the agent (or its prompt) asked and this memory
// answered -- normalized to [0,1). Passive briefing (session-start) injections
// deliberately carry no weight: they are exposure, not demand, and crediting
// them would let the briefing reinforce its own ranking. All values are
// retroactively retunable -- utility is rebuilt from the full event log, so a
// changed weight or half-life recomputes history on the next pass.
const (
	utilityHalfLifeDays = 14.0 // decay half-life for every signal class
	utilitySaturation   = 4.0  // raw score at which utility reaches 0.5
	utilityWeightRead   = 3.0  // explicit memory_read/notes_read: deliberate full-body pull
	utilityWeightRecall = 1.5  // recall-tool hit: agent-initiated query match
	utilityWeightPrompt = 1.0  // prompt-recall match: query-gated but automated
)

// UtilityComponents is the decayed raw demand per signal class, kept alongside
// the normalized score so the console can show WHY a memory ranks where it does.
type UtilityComponents struct {
	Read   float64 `json:"read,omitempty"`
	Recall float64 `json:"recall,omitempty"`
	Prompt float64 `json:"prompt,omitempty"`
}

// IsZero reports no demand in any class (exported for display-layer callers).
func (c UtilityComponents) IsZero() bool { return c.Read == 0 && c.Recall == 0 && c.Prompt == 0 }

// raw is the un-normalized decayed demand total.
func (c UtilityComponents) raw() float64 { return c.Read + c.Recall + c.Prompt }

// RetrievalStat mirrors one retrieval_stats row: how often an item has been
// surfaced (injected) to an agent and read back, when last, and its time-decayed
// utility score. It is a materialized projection of the append-only event log,
// rebuilt by RebuildRetrievalStats -- the events table is the source of truth.
type RetrievalStat struct {
	ItemID         string
	InjectCount    int
	ReadCount      int
	LastInjectedAt *time.Time
	LastReadAt     *time.Time
	Utility        float64
	Components     UtilityComponents
}

// injectedPayload is the shape RebuildRetrievalStats reads out of a
// retrieval.injected event's payload: recall records the surfaced item ids here
// (the item_id column is reserved for single-item events like memory.read).
// Hook and Source classify the injection for utility scoring: source "recall"
// and hook "user-prompt-submit" are query-gated demand; hook "session-start"
// (and anything unrecognized) is passive exposure.
type injectedPayload struct {
	ItemIDs         []string `json:"item_ids"`
	ClaudeSessionID string   `json:"claude_session_id"`
	ExternalClient  string   `json:"external_client"`
	Hook            string   `json:"hook"`
	Source          string   `json:"source"`
	Query           string   `json:"query"` // recall's search text; hit-signature suppression reads it
}

// injectedUtilityWeight maps an injection payload to its utility weight.
func injectedUtilityWeight(p injectedPayload) (weight float64, class string) {
	switch {
	case p.Source == "recall":
		return utilityWeightRecall, "recall"
	case p.Hook == "user-prompt-submit":
		return utilityWeightPrompt, "prompt"
	default:
		return 0, ""
	}
}

// utilityDecay is the exponential decay factor for a signal age days old.
func utilityDecay(age time.Duration) float64 {
	days := age.Hours() / 24
	if days < 0 {
		days = 0
	}
	return math.Exp2(-days / utilityHalfLifeDays)
}

// RebuildRetrievalStats recomputes the entire retrieval_stats table from the
// event log. It is the canonical maintenance path (the gardener calls it at the
// top of each pass, and it is safe to call after an import): retrieval.injected
// events contribute inject_count + last_injected_at (per item id, whether the id
// is in the item_id column or the payload's item_ids array), and memory.read /
// note.read events contribute read_count + last_read_at. Events are read oldest-first so
// the last-seen timestamp wins. The same walk accumulates the utility score:
// each query-gated signal adds its class weight decayed by the event's age.
func RebuildRetrievalStats(ctx context.Context, db *sql.DB) error {
	return rebuildRetrievalStats(ctx, db, time.Now().UTC())
}

// rebuildRetrievalStats is RebuildRetrievalStats with an injectable decay
// anchor, so tests can pin `now` instead of sleeping.
func rebuildRetrievalStats(ctx context.Context, db *sql.DB, now time.Time) error {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, kind, session_id, item_id, payload FROM events
		WHERE kind IN (?, ?, ?)
		ORDER BY ts ASC, id ASC`,
		string(core.EventInjected), string(core.EventMemoryRead), string(core.EventNoteRead))
	if err != nil {
		return fmt.Errorf("store.RebuildRetrievalStats: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	stats := make(map[string]*RetrievalStat)
	stat := func(id string) *RetrievalStat {
		s := stats[id]
		if s == nil {
			s = &RetrievalStat{ItemID: id}
			stats[id] = s
		}
		return s
	}
	// Utility counts each (item, session, signal class) once per session, so a
	// topic hammered eight times in one session is one unit of demand, not
	// eight. Events without a resolvable session (old rows) count individually.
	seen := map[string]struct{}{}
	credit := func(s *RetrievalStat, class, session string, weight float64, ts time.Time) {
		if weight == 0 {
			return
		}
		if session != "" {
			key := class + "\x00" + session + "\x00" + s.ItemID
			if _, dup := seen[key]; dup {
				return
			}
			seen[key] = struct{}{}
		}
		w := weight * utilityDecay(now.Sub(ts))
		switch class {
		case "read":
			s.Components.Read += w
		case "recall":
			s.Components.Recall += w
		case "prompt":
			s.Components.Prompt += w
		}
	}

	for rows.Next() {
		var tsStr, kind, sessionID, itemID, payload string
		if err := rows.Scan(&tsStr, &kind, &sessionID, &itemID, &payload); err != nil {
			return fmt.Errorf("store.RebuildRetrievalStats: scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return fmt.Errorf("store.RebuildRetrievalStats: parse ts: %w", err)
		}
		switch core.EventKind(kind) {
		case core.EventInjected:
			p, _ := decodeInjectedPayload(payload)
			weight, class := injectedUtilityWeight(p)
			session := injectedSessionKey(sessionID, p)
			for _, id := range injectedItemIDs(itemID, p) {
				s := stat(id)
				s.InjectCount++
				t := ts
				s.LastInjectedAt = &t
				credit(s, class, session, weight, ts)
			}
		// Notes are first-class recall results, so opening one is the same
		// deliberate query-gated pull as a memory read and earns the same class
		// and weight -- a note an agent keeps coming back to would otherwise
		// earn no demand credit at all. retrieval_stats is keyed by item id
		// alone (no kind column), so the note's row simply sits beside the
		// memories' with nothing to migrate.
		case core.EventMemoryRead, core.EventNoteRead:
			if itemID == "" {
				continue
			}
			s := stat(itemID)
			s.ReadCount++
			t := ts
			s.LastReadAt = &t
			credit(s, "read", sessionID, utilityWeightRead, ts)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store.RebuildRetrievalStats: rows: %w", err)
	}

	// Normalize: saturating ratio keeps utility in [0,1) and outlier-robust --
	// the one memory with hundreds of hits cannot dominate the blend.
	for _, s := range stats {
		if raw := s.Components.raw(); raw > 0 {
			s.Utility = raw / (raw + utilitySaturation)
		}
	}

	return replaceRetrievalStats(ctx, db, stats)
}

// injectedItemIDs returns every item id an injection event refers to: the
// item_id column (when set) plus the payload's item_ids array (recall's form).
func injectedItemIDs(itemID string, p injectedPayload) []string {
	var ids []string
	if itemID != "" {
		ids = append(ids, itemID)
	}
	for _, id := range p.ItemIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// decodeInjectedPayload decodes a retrieval.injected payload. Decoding is
// best-effort: an empty or unparseable payload is the zero payload (ok false),
// so the event still counts through its item_id column, it just cannot be
// classified for utility or tied to a session.
//
// A reader decodes each payload once and hands the result to
// injectedUtilityWeight, injectedSessionKey and injectedItemIDs: a hook
// injection's payload carries the verbatim context it injected, kilobytes per
// event, and the readers replay every injection in their window on each
// console render.
func decodeInjectedPayload(payload string) (p injectedPayload, ok bool) {
	if payload == "" || payload == "{}" {
		return injectedPayload{}, false
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return injectedPayload{}, false
	}
	return p, true
}

// RetrievalStatsInputs fingerprints the event-log rows RebuildRetrievalStats
// reads: how many there are, and the newest one's timestamp. While the
// fingerprint holds, a rebuild would read the same events and only move the
// utility decay's anchor, so a caller that rebuilds on demand can skip it.
// Events are append-only and an import rebuilds by itself (archive.Merge), so
// a change to the inputs always moves one half or the other.
func RetrievalStatsInputs(ctx context.Context, db *sql.DB) (string, error) {
	var (
		n      int
		newest sql.NullString
	)
	err := db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(ts) FROM events WHERE kind IN (?, ?, ?)`,
		string(core.EventInjected), string(core.EventMemoryRead), string(core.EventNoteRead)).Scan(&n, &newest)
	if err != nil {
		return "", fmt.Errorf("store.RetrievalStatsInputs: %w", err)
	}
	return fmt.Sprintf("%d@%s", n, newest.String), nil
}

// replaceRetrievalStats atomically swaps the retrieval_stats table for the given
// aggregates (clear + bulk insert in one transaction).
func replaceRetrievalStats(ctx context.Context, db *sql.DB, stats map[string]*RetrievalStat) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.RebuildRetrievalStats: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM retrieval_stats`); err != nil {
		return fmt.Errorf("store.RebuildRetrievalStats: clear: %w", err)
	}
	for _, s := range stats {
		var components any
		if !s.Components.IsZero() {
			b, err := json.Marshal(s.Components)
			if err != nil {
				return fmt.Errorf("store.RebuildRetrievalStats: marshal components %s: %w", s.ItemID, err)
			}
			components = string(b)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO retrieval_stats (item_id, inject_count, read_count, last_injected_at, last_read_at, utility, utility_components)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			s.ItemID, s.InjectCount, s.ReadCount,
			formatTimePtr(s.LastInjectedAt), formatTimePtr(s.LastReadAt),
			s.Utility, components); err != nil {
			return fmt.Errorf("store.RebuildRetrievalStats: insert %s: %w", s.ItemID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.RebuildRetrievalStats: commit: %w", err)
	}
	return nil
}

// GetRetrievalStat returns the stats row for an item. found is false when the
// item has never been injected or read.
func GetRetrievalStat(ctx context.Context, db *sql.DB, itemID string) (RetrievalStat, bool, error) {
	var (
		s                      RetrievalStat
		lastInjected, lastRead sql.NullString
		components             sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT item_id, inject_count, read_count, last_injected_at, last_read_at, utility, utility_components
		FROM retrieval_stats WHERE item_id = ?`, itemID).
		Scan(&s.ItemID, &s.InjectCount, &s.ReadCount, &lastInjected, &lastRead, &s.Utility, &components)
	if errors.Is(err, sql.ErrNoRows) {
		return RetrievalStat{}, false, nil
	}
	if err != nil {
		return RetrievalStat{}, false, fmt.Errorf("store.GetRetrievalStat: %w", err)
	}
	var perr error
	if s.LastInjectedAt, perr = nullTimePtr(lastInjected); perr != nil {
		return RetrievalStat{}, false, fmt.Errorf("store.GetRetrievalStat: last_injected_at: %w", perr)
	}
	if s.LastReadAt, perr = nullTimePtr(lastRead); perr != nil {
		return RetrievalStat{}, false, fmt.Errorf("store.GetRetrievalStat: last_read_at: %w", perr)
	}
	if components.Valid && components.String != "" {
		if err := json.Unmarshal([]byte(components.String), &s.Components); err != nil {
			return RetrievalStat{}, false, fmt.Errorf("store.GetRetrievalStat: utility_components: %w", err)
		}
	}
	return s, true, nil
}

// AllRetrievalStats loads the whole retrieval_stats table into a map keyed by
// item id, so a caller (the console) can annotate many memories with one query
// instead of N GetRetrievalStat calls.
func AllRetrievalStats(ctx context.Context, db *sql.DB) (map[string]RetrievalStat, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT item_id, inject_count, read_count, last_injected_at, last_read_at, utility, utility_components
		FROM retrieval_stats`)
	if err != nil {
		return nil, fmt.Errorf("store.AllRetrievalStats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]RetrievalStat)
	for rows.Next() {
		var (
			s                      RetrievalStat
			lastInjected, lastRead sql.NullString
			components             sql.NullString
		)
		if err := rows.Scan(&s.ItemID, &s.InjectCount, &s.ReadCount, &lastInjected, &lastRead, &s.Utility, &components); err != nil {
			return nil, fmt.Errorf("store.AllRetrievalStats: scan: %w", err)
		}
		var perr error
		if s.LastInjectedAt, perr = nullTimePtr(lastInjected); perr != nil {
			return nil, fmt.Errorf("store.AllRetrievalStats: last_injected_at: %w", perr)
		}
		if s.LastReadAt, perr = nullTimePtr(lastRead); perr != nil {
			return nil, fmt.Errorf("store.AllRetrievalStats: last_read_at: %w", perr)
		}
		if components.Valid && components.String != "" {
			if err := json.Unmarshal([]byte(components.String), &s.Components); err != nil {
				return nil, fmt.Errorf("store.AllRetrievalStats: utility_components: %w", err)
			}
		}
		out[s.ItemID] = s
	}
	return out, rows.Err()
}

// UtilityScores loads item id -> normalized utility for every item with nonzero
// demand. It is the single wiring point from the stats projection into the
// ranking paths (briefing, prompt-recall, recall), which read the stored score
// as-is -- rebuilt hourly by the gardener and on console loads -- and never
// replay the event log themselves.
func UtilityScores(ctx context.Context, db *sql.DB) (map[string]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT item_id, utility FROM retrieval_stats WHERE utility > 0`)
	if err != nil {
		return nil, fmt.Errorf("store.UtilityScores: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]float64{}
	for rows.Next() {
		var id string
		var u float64
		if err := rows.Scan(&id, &u); err != nil {
			return nil, fmt.Errorf("store.UtilityScores: scan: %w", err)
		}
		out[id] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.UtilityScores: %w", err)
	}
	return out, nil
}

// StaleMemories returns active memories that have seen no activity since cutoff:
// neither updated, injected, nor read on or after that instant. It LEFT JOINs
// retrieval_stats (a memory with no stats row counts as never injected/read, so
// only its updated_at matters), oldest-updated first. It backs the gardener's
// staleness pass.
func StaleMemories(ctx context.Context, db *sql.DB, cutoff time.Time) ([]core.Memory, error) {
	c := core.FormatTime(cutoff.UTC())
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		LEFT JOIN retrieval_stats ON retrieval_stats.item_id = memories_index.id
		WHERE invalid_at IS NULL
		  AND updated_at < ?
		  AND (last_injected_at IS NULL OR last_injected_at < ?)
		  AND (last_read_at IS NULL OR last_read_at < ?)
		ORDER BY updated_at ASC, id ASC`, c, c, c)
	if err != nil {
		return nil, fmt.Errorf("store.StaleMemories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.StaleMemories: %w", err)
	}
	return mems, nil
}

// CountMemoriesUnsurfacedSince counts active memories that have not entered an
// agent context since cutoff: never injected, or last injected before it. It is
// the Overview's "going stale" bucket, and it is deliberately narrower than
// StaleMemories -- that pass also weighs updates and reads, which answers "is
// anyone touching this", while this one answers only "is this still reaching
// agents".
//
// A memory created after the cutoff is excluded: something written yesterday has
// not had 45 days in which to surface, and counting it would make every burst of
// writing look like decay.
func CountMemoriesUnsurfacedSince(ctx context.Context, db *sql.DB, cutoff time.Time) (int, error) {
	c := core.FormatTime(cutoff.UTC())
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memories_index
		LEFT JOIN retrieval_stats ON retrieval_stats.item_id = memories_index.id
		WHERE invalid_at IS NULL
		  AND created_at < ?
		  AND (last_injected_at IS NULL OR last_injected_at < ?)`, c, c).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store.CountMemoriesUnsurfacedSince: %w", err)
	}
	return n, nil
}

// formatTimePtr renders a nullable timestamp for a nullable column: nil -> NULL.
func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return core.FormatTime(t.UTC())
}
