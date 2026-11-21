package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// mishapPayload is the slice of an agent.mishap event payload this package
// reads: the ids of memories the mishap's text named, matched once at
// ingestion (internal/mcp's session_end handler) and durable in the event log
// ever since -- nothing re-matches here.
type mishapPayload struct {
	ItemIDs []string `json:"item_ids"`
}

// RecentMishapItemIDs returns the ids of memories referenced by a project's
// agent.mishap events recorded within window of now, each mapped to the
// timestamp of its most recent referencing mishap. It feeds the briefing's
// mishap promotion -- an ordering signal like favorites -- and deliberately
// NOT the utility score: RebuildRetrievalStats keeps its query-gated classes
// (read/recall/prompt) per the closed-loop-utility-signal-contract. Mishaps
// whose payload names no memory contribute nothing; no references yields an
// empty map, not an error.
func RecentMishapItemIDs(ctx context.Context, db *sql.DB, project string, window time.Duration) (map[string]time.Time, error) {
	return recentMishapItemIDs(ctx, db, project, window, time.Now().UTC())
}

// recentMishapItemIDs is RecentMishapItemIDs with an injectable clock, so tests
// pin the window edge instead of sleeping. The cutoff comparison is a string
// >= on the fixed-width core.TimeFormat, which matches chronological order.
func recentMishapItemIDs(ctx context.Context, db *sql.DB, project string, window time.Duration, now time.Time) (map[string]time.Time, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, payload FROM events
		WHERE kind = ? AND project_slug = ? AND ts >= ?
		ORDER BY ts ASC, id ASC`,
		string(core.EventAgentMishap), project, core.FormatTime(now.Add(-window)))
	if err != nil {
		return nil, fmt.Errorf("store.RecentMishapItemIDs: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]time.Time{}
	for rows.Next() {
		var tsStr, payload string
		if err := rows.Scan(&tsStr, &payload); err != nil {
			return nil, fmt.Errorf("store.RecentMishapItemIDs: scan: %w", err)
		}
		if payload == "" || payload == "{}" {
			continue
		}
		var p mishapPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			// Best-effort, the injectedItemIDs precedent: one unreadable payload
			// drops that event's linkage, never the whole query.
			continue
		}
		if len(p.ItemIDs) == 0 {
			continue
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("store.RecentMishapItemIDs: parse ts: %w", err)
		}
		// Rows arrive oldest-first, so the last write per id is its most recent.
		for _, id := range p.ItemIDs {
			if id != "" {
				out[id] = ts
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.RecentMishapItemIDs: rows: %w", err)
	}
	return out, nil
}

// mishapListLimit is ListMishaps' default cap.
const mishapListLimit = 200

// MishapFilter selects agent.mishap events for the mishap listing (the
// console's Mishaps screen and seam mishaps).
type MishapFilter struct {
	// Type keeps one kind of report: a core.MishapTypes slug, core.MishapUntyped
	// for the reports that name no type, or "" for every report.
	Type string
	// Project, with ByProject set, keeps one scope's reports: a project slug,
	// or "" for the global scope's.
	Project   string
	ByProject bool
	// Since keeps the reports recorded at or after it; zero is all time.
	Since time.Time
	// Limit caps the rows ListMishaps returns (default 200). The tally ignores it.
	Limit int
}

// MishapReport is one agent.mishap event as the listing reads it.
type MishapReport struct {
	ID        string
	At        time.Time
	SessionID string
	Project   string
	// Type is the report's recorded type, "" for one that names none: every
	// report written before types existed. A payload type outside
	// core.MishapTypes reads as none too, here and in every filter.
	Type core.MishapType
	// Description is the report as the agent wrote it, type prefix and all.
	Description string
	// ItemIDs are the memories the report named, matched once at ingestion.
	ItemIDs []string
}

// MishapTally counts mishap reports for the listing's chips and its memories
// block.
type MishapTally struct {
	// ByType counts the reports of the filter's scope and window by type,
	// whatever its Type ("" counts the untyped ones), so every chip shows its
	// own number while one is selected.
	ByType map[core.MishapType]int
	// ByMemory counts, per memory id, the reports naming that memory among
	// those the filter selects, its Type included: which memories failed to
	// prevent the mishaps in view.
	ByMemory map[string]int
}

// mishapTypeSQL is a mishap event's recorded type in SQL: its payload's type,
// or the empty string for a payload with none or one that is not JSON.
const mishapTypeSQL = `COALESCE(CASE WHEN json_valid(payload) THEN json_extract(payload, '$.type') END, '')`

// mishapWhere builds the WHERE clause (without the keyword) and its arguments
// for f. withType false leaves f.Type out, for the counts by type.
func mishapWhere(f MishapFilter, withType bool) (string, []any, error) {
	where := `kind = ?`
	args := []any{string(core.EventAgentMishap)}
	if f.ByProject {
		where += ` AND project_slug = ?`
		args = append(args, f.Project)
	}
	if !f.Since.IsZero() {
		where += ` AND ts >= ?`
		args = append(args, core.FormatTime(f.Since))
	}
	if !withType || f.Type == "" {
		return where, args, nil
	}
	switch typ := core.MishapType(f.Type); {
	case typ.Valid():
		where += ` AND ` + mishapTypeSQL + ` = ?`
		args = append(args, string(typ))
	case f.Type == core.MishapUntyped:
		where += ` AND ` + mishapTypeSQL + ` NOT IN (` + placeholders(len(core.MishapTypes)) + `)`
		for _, t := range core.MishapTypes {
			args = append(args, string(t))
		}
	default:
		return "", nil, fmt.Errorf("invalid mishap type %q", f.Type)
	}
	return where, args, nil
}

// ListMishaps returns the agent.mishap events f selects, newest first (ties
// by id), at most f.Limit of them. It reads the handle it is given: on the
// console's filtered one a locked project's reports are simply absent. A
// payload that does not decode yields its row with no text, never a failed
// listing.
func ListMishaps(ctx context.Context, db *sql.DB, f MishapFilter) ([]MishapReport, error) {
	where, args, err := mishapWhere(f, true)
	if err != nil {
		return nil, fmt.Errorf("store.ListMishaps: %w", err)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = mishapListLimit
	}
	rows, err := db.QueryContext(ctx, `SELECT id, ts, session_id, project_slug, payload FROM events
		WHERE `+where+` ORDER BY ts DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("store.ListMishaps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MishapReport
	for rows.Next() {
		var (
			r           MishapReport
			ts, payload string
		)
		if err := rows.Scan(&r.ID, &ts, &r.SessionID, &r.Project, &payload); err != nil {
			return nil, fmt.Errorf("store.ListMishaps: scan: %w", err)
		}
		if r.At, err = core.ParseTime(ts); err != nil {
			return nil, fmt.Errorf("store.ListMishaps: event %s: %w", r.ID, err)
		}
		r.Type, r.Description, r.ItemIDs = decodeMishap(payload)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListMishaps: %w", err)
	}
	return out, nil
}

// decodeMishap reads a mishap payload's type, description and memory ids.
// Each field is read on its own, so one of an unexpected shape costs only
// itself; a type outside the vocabulary reads as none.
func decodeMishap(payload string) (core.MishapType, string, []string) {
	var p map[string]any
	if payload == "" || json.Unmarshal([]byte(payload), &p) != nil {
		return "", "", nil
	}
	typ, _ := p["type"].(string)
	if !core.MishapType(typ).Valid() {
		typ = ""
	}
	desc, _ := p["description"].(string)
	var ids []string
	if list, ok := p["item_ids"].([]any); ok {
		for _, v := range list {
			if id, ok := v.(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return core.MishapType(typ), desc, ids
}

// CountMishaps tallies the reports f selects: by type over its scope and
// window, and by the memories they named. Like ListMishaps, it reads the
// handle it is given.
func CountMishaps(ctx context.Context, db *sql.DB, f MishapFilter) (MishapTally, error) {
	byType, err := countMishapTypes(ctx, db, f)
	if err != nil {
		return MishapTally{}, fmt.Errorf("store.CountMishaps: %w", err)
	}
	byMemory, err := countMishapMemories(ctx, db, f)
	if err != nil {
		return MishapTally{}, fmt.Errorf("store.CountMishaps: %w", err)
	}
	return MishapTally{ByType: byType, ByMemory: byMemory}, nil
}

// countMishapTypes counts f's scope and window by type, f.Type aside.
func countMishapTypes(ctx context.Context, db *sql.DB, f MishapFilter) (map[core.MishapType]int, error) {
	where, args, err := mishapWhere(f, false)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+mishapTypeSQL+` AS t, COUNT(*) FROM events
		WHERE `+where+` GROUP BY t`, args...)
	if err != nil {
		return nil, fmt.Errorf("by type: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[core.MishapType]int{}
	for rows.Next() {
		var (
			t sql.NullString
			n int
		)
		if err := rows.Scan(&t, &n); err != nil {
			return nil, fmt.Errorf("by type: scan: %w", err)
		}
		typ := core.MishapType(t.String)
		if !typ.Valid() {
			typ = ""
		}
		out[typ] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("by type: %w", err)
	}
	return out, nil
}

// countMishapMemories counts, per memory id, the reports f selects that named
// it, reading item_ids only where it is a list, as ListMishaps does.
// COUNT(DISTINCT id): a report naming a memory twice names it once.
func countMishapMemories(ctx context.Context, db *sql.DB, f MishapFilter) (map[string]int, error) {
	where, args, err := mishapWhere(f, true)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT j.value, COUNT(DISTINCT events.id) FROM events,
		json_each(CASE WHEN json_valid(events.payload) AND json_type(events.payload, '$.item_ids') = 'array'
			THEN json_extract(events.payload, '$.item_ids') ELSE '[]' END) j
		WHERE `+where+` AND j.type = 'text' AND j.value != '' GROUP BY j.value`, args...)
	if err != nil {
		return nil, fmt.Errorf("by memory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("by memory: scan: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("by memory: %w", err)
	}
	return out, nil
}
