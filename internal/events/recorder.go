// Package events is the append-only event log: the single write path for the
// record of everything (session lifecycle, memory write/read/supersede,
// injection, task transition, gardener action). Telemetry, retrieval stats, and
// the console feed all derive from this log. SSE fan-out is added in a later phase.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// truncationMarker is appended to a captured field that Truncate had to trim.
const truncationMarker = "\n… [truncated]"

// Truncate caps s to maxRunes runes, appending truncationMarker when it trims.
// A non-positive maxRunes disables truncation (returns s unchanged) -- the
// default for captured Interactions content, where retention pruning rather than
// truncation is the growth control. Rune-safe: never splits a multibyte rune.
func Truncate(s string, maxRunes int) string {
	if maxRunes <= 0 || len(s) <= maxRunes {
		// len (bytes) >= rune count, so a byte-length under the cap is safe.
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + truncationMarker
}

// kindArgs builds the "?,?,?" placeholder list and the matching []any args for
// an IN/NOT IN clause over kinds. It returns ("", nil) for an empty slice.
func kindArgs(kinds []core.EventKind) (string, []any) {
	if len(kinds) == 0 {
		return "", nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = string(k)
	}
	return ph, args
}

// subBuffer is the per-subscriber channel depth. A subscriber that falls behind
// simply drops events (the console's live feed is best-effort; the DB remains the
// source of truth), so a stalled SSE client never blocks the write path.
const subBuffer = 32

// Recorder appends events to the log, reads them back, and fans successfully
// recorded events out to live subscribers (the console SSE feed).
type Recorder struct {
	db *sql.DB
	// reader, when set, serves every read (ByID, ByKinds, ...) instead of db:
	// the console's peer_auth-filtered handle (ReadingFrom). Writes and the live
	// fan-out always stay on db and the shared hub.
	reader *sql.DB
	*hub
}

// hub is the state every Recorder sharing one event log shares: the live
// subscribers and the milestone layer's arming. ReadingFrom copies a Recorder
// but never its hub, so an event recorded through either reaches both's
// subscribers.
type hub struct {
	mu     sync.Mutex
	subs   map[int]chan core.Event
	nextID int

	// features is the file/env optional-features base the milestone layer
	// gates on, and featuresArmed whether SetFeatures has supplied it. Until
	// then the recorder appends events exactly as before -- no gate read, no
	// milestone checks -- so unwired recorders (tests, benchmarks, one-shot
	// tools) keep their behavior and cost unchanged.
	features      config.Features
	featuresArmed bool
}

// NewRecorder returns a Recorder backed by db.
func NewRecorder(db *sql.DB) *Recorder { return &Recorder{db: db, hub: &hub{}} }

// ReadingFrom returns a Recorder that answers every read from db -- a filtered
// handle (store.OpenView) on the same database -- while recording, milestones
// and the live fan-out stay this Recorder's. The console reads the event log
// through one, so an event about a project sealed to another OS account never
// reaches its pages.
func (r *Recorder) ReadingFrom(db *sql.DB) *Recorder {
	return &Recorder{db: r.db, reader: db, hub: r.hub}
}

// rdb is the handle reads go to.
func (r *Recorder) rdb() *sql.DB {
	if r.reader != nil {
		return r.reader
	}
	return r.db
}

// refile files an event recorded on behalf of a configured peer_auth account
// under that account's own project whenever it named another scope -- the
// global scope, typically, for an unattributed tool call. A sealed system's
// tool calls, prompts and reads are its content as much as its memories are,
// and the event log is read by the owner's console: an event filed under the
// global scope would carry a sealed agent's arguments to the one account the
// seal exists to keep them from. The owner's own events, and every event
// recorded for no caller, are left exactly as given.
func refile(ctx context.Context, e core.Event) core.Event {
	v, ok := access.FromContext(ctx)
	if !ok || v.Principal().Kind != access.KindUser {
		return e
	}
	if !v.Writable(e.ProjectSlug) {
		e.ProjectSlug = v.Home()
	}
	return e
}

// The milestone layer mints through RecordOnce (store.OnceRecorder).
var _ store.OnceRecorder = (*Recorder)(nil)

// SetFeatures arms the momentum milestone layer with the file/env features
// base. From then on every successfully recorded event runs
// store.CheckMilestones, which resolves the effective features live (base
// overlaid with the console's stored override) and does nothing while
// momentum is off -- so the console toggle applies immediately, with no
// daemon restart. Call it once at wiring time, before serving.
func (r *Recorder) SetFeatures(base config.Features) {
	r.mu.Lock()
	r.features = base
	r.featuresArmed = true
	r.mu.Unlock()
}

// mintMilestones runs the store milestone checks against an event that just
// landed. Best-effort by design: the event is already durably recorded, so a
// milestone failure is logged and never surfaces to the recording caller. The
// re-entrant call this makes through RecordOnce terminates immediately:
// milestone.reached is not a kind CheckMilestones watches.
func (r *Recorder) mintMilestones(ctx context.Context, e core.Event) {
	r.mu.Lock()
	armed, base := r.featuresArmed, r.features
	r.mu.Unlock()
	if !armed {
		return
	}
	if err := store.CheckMilestones(ctx, r.db, r, base, e); err != nil {
		slog.Warn("events: milestone check", "kind", string(e.Kind), "error", err)
	}
}

// Subscribe registers a live-event channel and returns it with an unsubscribe
// func the caller must invoke when done (idempotent). Events are delivered
// best-effort: a full channel drops rather than blocks.
func (r *Recorder) Subscribe() (<-chan core.Event, func()) {
	ch := make(chan core.Event, subBuffer)
	r.mu.Lock()
	if r.subs == nil {
		r.subs = make(map[int]chan core.Event)
	}
	id := r.nextID
	r.nextID++
	r.subs[id] = ch
	r.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			if c, ok := r.subs[id]; ok {
				delete(r.subs, id)
				close(c)
			}
			r.mu.Unlock()
		})
	}
}

// publish fans an event out to current subscribers, dropping to any that are
// full. It holds the lock so it cannot race an unsubscribe's close.
func (r *Recorder) publish(e core.Event) {
	r.mu.Lock()
	for _, ch := range r.subs {
		select {
		case ch <- e:
		default:
		}
	}
	r.mu.Unlock()
}

// Record appends an event, stamping a ULID id and UTC timestamp when absent and
// serializing Payload to JSON. It returns the stored event's id. A non-fatal
// logging call site may ignore the id but should not ignore the error.
func (r *Recorder) Record(ctx context.Context, e core.Event) (string, error) {
	if e.Kind == "" {
		return "", fmt.Errorf("events.Record: empty kind")
	}
	e = refile(ctx, e)
	if e.ID == "" {
		id, err := core.NewID()
		if err != nil {
			return "", fmt.Errorf("events.Record: %w", err)
		}
		e.ID = id
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}

	payload := "{}"
	if len(e.Payload) > 0 {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return "", fmt.Errorf("events.Record: marshal payload: %w", err)
		}
		payload = string(b)
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, core.FormatTime(e.TS), string(e.Kind),
		e.SessionID, e.ProjectSlug, e.ItemID, payload,
	)
	if err != nil {
		return "", fmt.Errorf("events.Record: insert: %w", err)
	}
	r.publish(e)
	r.mintMilestones(ctx, e)
	return e.ID, nil
}

// RecordOnce appends an event only if no event of its kind exists for its item
// -- the once-per-item latch behind moments like the momentum first-reuse
// mark. The guard and the insert are a single statement, so two concurrent
// recorders cannot both mint the moment; recorded is false when the latch had
// already been set, and subscribers see the event only when it actually
// landed.
func (r *Recorder) RecordOnce(ctx context.Context, e core.Event) (id string, recorded bool, err error) {
	if e.Kind == "" {
		return "", false, fmt.Errorf("events.RecordOnce: empty kind")
	}
	if e.ItemID == "" {
		return "", false, fmt.Errorf("events.RecordOnce: empty item id (the latch is per item)")
	}
	e = refile(ctx, e)
	if e.ID == "" {
		nid, err := core.NewID()
		if err != nil {
			return "", false, fmt.Errorf("events.RecordOnce: %w", err)
		}
		e.ID = nid
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	payload := "{}"
	if len(e.Payload) > 0 {
		b, err := json.Marshal(e.Payload)
		if err != nil {
			return "", false, fmt.Errorf("events.RecordOnce: marshal payload: %w", err)
		}
		payload = string(b)
	}
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		 SELECT ?, ?, ?, ?, ?, ?, ?
		 WHERE NOT EXISTS (SELECT 1 FROM events WHERE kind = ? AND item_id = ?)`,
		e.ID, core.FormatTime(e.TS), string(e.Kind),
		e.SessionID, e.ProjectSlug, e.ItemID, payload,
		string(e.Kind), e.ItemID,
	)
	if err != nil {
		return "", false, fmt.Errorf("events.RecordOnce: insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("events.RecordOnce: rows affected: %w", err)
	}
	if n == 0 {
		return "", false, nil
	}
	r.publish(e)
	r.mintMilestones(ctx, e)
	return e.ID, true, nil
}

// ByID returns a single event by its id. ok is false (with a nil error) when no
// event has that id.
func (r *Recorder) ByID(ctx context.Context, id string) (core.Event, bool, error) {
	if id == "" {
		return core.Event{}, false, nil
	}
	rows, err := r.rdb().QueryContext(ctx,
		`SELECT id, ts, kind, session_id, project_slug, item_id, payload
		 FROM events WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return core.Event{}, false, fmt.Errorf("events.ByID: %w", err)
	}
	evs, err := scanEvents(rows)
	if err != nil {
		return core.Event{}, false, err
	}
	if len(evs) == 0 {
		return core.Event{}, false, nil
	}
	return evs[0], true, nil
}

// BySession returns a session's events in chronological order (oldest first), so
// a caller can render the session's timeline. A non-positive limit defaults to
// 500.
func (r *Recorder) BySession(ctx context.Context, sessionID string, limit int) ([]core.Event, error) {
	if sessionID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.rdb().QueryContext(ctx,
		`SELECT id, ts, kind, session_id, project_slug, item_id, payload
		 FROM events WHERE session_id = ? ORDER BY ts ASC, id ASC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("events.BySession: %w", err)
	}
	return scanEvents(rows)
}

// LatestBySession returns a session's most recent events of the given kinds,
// newest first, capped at limit (default 50). Unlike BySession -- whose limit
// truncates from the oldest side, for timelines -- this reads from the newest
// side, for "what did this agent just do" trails. An empty kinds slice spans
// all kinds.
func (r *Recorder) LatestBySession(ctx context.Context, sessionID string, kinds []core.EventKind, limit int) ([]core.Event, error) {
	if sessionID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, ts, kind, session_id, project_slug, item_id, payload
	      FROM events WHERE session_id = ?`
	args := []any{sessionID}
	if ph, kArgs := kindArgs(kinds); ph != "" {
		q += ` AND kind IN (` + ph + `)`
		args = append(args, kArgs...)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.LatestBySession: %w", err)
	}
	return scanEvents(rows)
}

// ByKinds returns events of any of the given kinds, newest first, strictly older
// than the compound (beforeTS, beforeID) cursor when it is set -- so a caller can
// page backwards through the Interactions feed without missing or repeating rows
// across a ts tie (ids are ULIDs, lexically ordered like ts). An empty kinds
// slice returns nil; a non-positive limit defaults to 200.
func (r *Recorder) ByKinds(ctx context.Context, kinds []core.EventKind, beforeTS, beforeID string, limit int) ([]core.Event, error) {
	ph, args := kindArgs(kinds)
	if ph == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, ts, kind, session_id, project_slug, item_id, payload
	      FROM events WHERE kind IN (` + ph + `)`
	if beforeTS != "" {
		q += ` AND (ts < ? OR (ts = ? AND id < ?))`
		args = append(args, beforeTS, beforeTS, beforeID)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.ByKinds: %w", err)
	}
	return scanEvents(rows)
}

// ByKindsAfter returns events of any of the given kinds at or after afterTS,
// newest first. The optional compound (beforeTS, beforeID) cursor pages farther
// back without crossing the lower bound. It is the bounded-history query behind
// the Interactions screen's explicit "add recent" action; keeping the bound in
// SQL avoids reading an unbounded tail only to discard it in the console layer.
// An empty kinds slice returns nil; a non-positive limit defaults to 200.
func (r *Recorder) ByKindsAfter(ctx context.Context, kinds []core.EventKind, afterTS, beforeTS, beforeID string, limit int) ([]core.Event, error) {
	ph, args := kindArgs(kinds)
	if ph == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, ts, kind, session_id, project_slug, item_id, payload
	      FROM events WHERE kind IN (` + ph + `)`
	if afterTS != "" {
		q += ` AND ts >= ?`
		args = append(args, afterTS)
	}
	if beforeTS != "" {
		q += ` AND (ts < ? OR (ts = ? AND id < ?))`
		args = append(args, beforeTS, beforeTS, beforeID)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.ByKindsAfter: %w", err)
	}
	return scanEvents(rows)
}

// ByKindsSince returns events of any of the given kinds strictly newer than the
// (sinceTS, sinceID) cursor, oldest first -- the gap-fill query an SSE client
// runs after a reconnect to recover rows it missed. An empty kinds slice returns
// nil; a non-positive limit defaults to 200.
func (r *Recorder) ByKindsSince(ctx context.Context, kinds []core.EventKind, sinceTS, sinceID string, limit int) ([]core.Event, error) {
	ph, args := kindArgs(kinds)
	if ph == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id, ts, kind, session_id, project_slug, item_id, payload
	      FROM events WHERE kind IN (` + ph + `)`
	if sinceTS != "" {
		q += ` AND (ts > ? OR (ts = ? AND id > ?))`
		args = append(args, sinceTS, sinceTS, sinceID)
	}
	q += ` ORDER BY ts ASC, id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.ByKindsSince: %w", err)
	}
	return scanEvents(rows)
}

// KindTick is one event's id, timestamp, and kind -- the minimal projection the
// console buckets into the interaction-volume histogram. ID lets an interactive
// bucket identify its newest represented row without fetching wide event
// payloads for the chart.
type KindTick struct {
	ID   string
	TS   time.Time
	Kind string
}

// KindTimeline returns the (id, ts, kind) of every event of the given kinds at
// or after sinceTS, newest first and capped at limit. project scopes to one
// project slug when non-empty. Only three narrow columns are read, so a wide
// window stays cheap.
// An empty sinceTS spans all history (bounded by limit); an empty kinds slice or
// non-positive limit returns nil.
func (r *Recorder) KindTimeline(ctx context.Context, kinds []core.EventKind, project, sinceTS string, limit int) ([]KindTick, error) {
	ph, args := kindArgs(kinds)
	if ph == "" || limit <= 0 {
		return nil, nil
	}
	q := `SELECT id, ts, kind FROM events WHERE kind IN (` + ph + `)`
	if project != "" {
		q += ` AND project_slug = ?`
		args = append(args, project)
	}
	if sinceTS != "" {
		q += ` AND ts >= ?`
		args = append(args, sinceTS)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.KindTimeline: %w", err)
	}
	defer rows.Close()
	var out []KindTick
	for rows.Next() {
		var id, tsStr, kind string
		if err := rows.Scan(&id, &tsStr, &kind); err != nil {
			return nil, fmt.Errorf("events.KindTimeline scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("events.KindTimeline time: %w", err)
		}
		out = append(out, KindTick{ID: id, TS: ts, Kind: kind})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events.KindTimeline: %w", err)
	}
	return out, nil
}

// TimestampsSince returns the timestamps of every event at or after since,
// oldest first, capped at limit (default 2000) -- the minimal projection behind
// the Now screen's activity pulse, which buckets them client-blind into a
// per-interval histogram. All kinds count: the pulse measures how hard the
// fleet is running, and a tool call is exactly that.
func (r *Recorder) TimestampsSince(ctx context.Context, since time.Time, limit int) ([]time.Time, error) {
	if limit <= 0 {
		limit = 2000
	}
	rows, err := r.rdb().QueryContext(ctx,
		`SELECT ts FROM events WHERE ts >= ? ORDER BY ts ASC, id ASC LIMIT ?`,
		core.FormatTime(since), limit)
	if err != nil {
		return nil, fmt.Errorf("events.TimestampsSince: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var tsStr string
		if err := rows.Scan(&tsStr); err != nil {
			return nil, fmt.Errorf("events.TimestampsSince scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("events.TimestampsSince time: %w", err)
		}
		out = append(out, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events.TimestampsSince: %w", err)
	}
	return out, nil
}

// RecentExcluding returns the most recent events, newest first, omitting the
// given kinds -- the overview's business-level feed, which hides transport-level
// tool.call / hook.prompt noise. A non-positive limit defaults to 50.
func (r *Recorder) RecentExcluding(ctx context.Context, limit int, exclude ...core.EventKind) ([]core.Event, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, ts, kind, session_id, project_slug, item_id, payload FROM events`
	var args []any
	if ph, exArgs := kindArgs(exclude); ph != "" {
		q += ` WHERE kind NOT IN (` + ph + `)`
		args = append(args, exArgs...)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.rdb().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.RecentExcluding: %w", err)
	}
	return scanEvents(rows)
}

// PruneKinds deletes events of the given kinds recorded strictly before cutoff,
// returning the number removed. It is the retention path for transport-level
// Interactions events (tool.call, hook.prompt); domain events are never passed
// in. An empty kinds slice or zero cutoff deletes nothing.
func (r *Recorder) PruneKinds(ctx context.Context, kinds []core.EventKind, before time.Time) (int64, error) {
	ph, args := kindArgs(kinds)
	if ph == "" || before.IsZero() {
		return 0, nil
	}
	args = append(args, core.FormatTime(before))
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM events WHERE kind IN (`+ph+`) AND ts < ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("events.PruneKinds: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("events.PruneKinds: rows affected: %w", err)
	}
	return n, nil
}

// scanEvents drains an events query into core.Event values.
func scanEvents(rows *sql.Rows) ([]core.Event, error) {
	defer func() { _ = rows.Close() }()
	var out []core.Event
	for rows.Next() {
		var (
			e                 core.Event
			ts, kind, payload string
		)
		if err := rows.Scan(&e.ID, &ts, &kind, &e.SessionID, &e.ProjectSlug, &e.ItemID, &payload); err != nil {
			return nil, fmt.Errorf("events: scan: %w", err)
		}
		t, err := core.ParseTime(ts)
		if err != nil {
			return nil, fmt.Errorf("events: parse ts: %w", err)
		}
		e.TS = t
		e.Kind = core.EventKind(kind)
		if payload != "" && payload != "{}" {
			if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
				return nil, fmt.Errorf("events: unmarshal payload: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: rows: %w", err)
	}
	return out, nil
}
