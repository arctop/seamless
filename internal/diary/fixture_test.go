package diary

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store/storetest"
)

// dmFixture is a fresh database the day-model tests write rows into
// directly, with every timestamp chosen: sessions, ledger rows, tasks,
// events, memories, notes, trials, commit records and projects.
type dmFixture struct {
	tb   testing.TB
	ctx  context.Context
	db   *sql.DB
	ex   dmExecer // db, or the transaction inTx opened
	path string
	loc  *time.Location
	n    int
}

// dmExecer is what the fixture writes through: the database or a
// transaction.
type dmExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inTx runs fn with every write in one transaction, for a large fixture.
func (f *dmFixture) inTx(fn func()) {
	f.tb.Helper()
	tx, err := f.db.BeginTx(f.ctx, nil)
	require.NoError(f.tb, err)
	f.ex = tx
	defer func() { f.ex = f.db }()
	fn()
	require.NoError(f.tb, tx.Commit())
}

// dmOpen opens a fresh fixture whose days are local days in loc.
func dmOpen(tb testing.TB, loc *time.Location) *dmFixture {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(tb, path)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = db.Close() })
	return &dmFixture{tb: tb, ctx: context.Background(), db: db, ex: db, path: path, loc: loc}
}

// dmZone loads a zone the tests need, or skips where the host has no zone
// database.
func dmZone(tb testing.TB, name string) *time.Location {
	tb.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		tb.Skipf("no zone %s: %v", name, err)
	}
	return loc
}

// at is a local time on the fixture's zone.
func (f *dmFixture) at(y int, m time.Month, d, h, minute int) time.Time {
	return time.Date(y, m, d, h, minute, 0, 0, f.loc)
}

// id mints a readable 26-character id: tag, then a counter.
func (f *dmFixture) id(tag string) string {
	f.n++
	return fmt.Sprintf("01%s%0*d", tag, 24-len(tag), f.n)
}

func (f *dmFixture) exec(q string, args ...any) {
	f.tb.Helper()
	_, err := f.ex.ExecContext(f.ctx, q, args...)
	require.NoError(f.tb, err)
}

// dmSession is a session row the fixture writes.
type dmSession struct {
	ID, Name, Project, CWD, Findings, Model, Client string
	Status                                          core.SessionStatus
	Created, Updated                                time.Time
	Totals                                          core.TokenUsage // stored totals, for a session with no ledger rows
	Verdict                                         string
	Explicit                                        bool // a session_start session: no client
}

// session writes s, filling an id, a name, a status and a client.
func (f *dmFixture) session(s dmSession) string {
	f.tb.Helper()
	if s.ID == "" {
		s.ID = f.id("SESS")
	}
	if s.Status == "" {
		s.Status = core.SessionCompleted
	}
	if s.Updated.IsZero() {
		s.Updated = s.Created
	}
	ambient, client, ext := 1, s.Client, s.ID
	if s.Explicit {
		ambient, client, ext = 0, "", ""
	} else if client == "" {
		client = "claude-code"
	}
	if s.Name == "" {
		switch {
		case s.Explicit:
			s.Name = "explicit-" + strings.ToLower(s.ID[len(s.ID)-6:])
		case client == "codex":
			s.Name = "cx/" + strings.ToLower(s.ID)
		default:
			s.Name = "cc/" + strings.ToLower(s.ID)
		}
	}
	s.Totals.Normalize()
	var verdict, flagged any
	if s.Verdict != "" {
		verdict, flagged = s.Verdict, core.FormatTime(s.Updated)
	}
	f.exec(`INSERT INTO sessions (id, name, project_slug, status, findings, claude_session_id, external_client,
		cwd, source, model, ambient, created_at, updated_at, input_tokens, cached_input_tokens,
		cache_creation_tokens, output_tokens, total_tokens, verdict, flagged_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'startup', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.Project, string(s.Status), s.Findings, ext, client, s.CWD, s.Model, ambient,
		core.FormatTime(s.Created), core.FormatTime(s.Updated), s.Totals.Input, s.Totals.Cached,
		s.Totals.CacheCreation, s.Totals.Output, s.Totals.Total, verdict, flagged)
	return s.ID
}

// work writes a session's ledger: one row per quarter-hour from start for
// minutes of activity (every minute active), output tokens spread evenly and
// ten times as much cache read, on model and thread. The session's totals
// become the sum of its rows, as the ledger's writers keep them.
func (f *dmFixture) work(session string, start time.Time, minutes, output int, model string, thread core.UsageThread) {
	f.tb.Helper()
	if minutes <= 0 {
		return
	}
	at := start.UTC()
	left, perMinute := minutes, output/minutes
	for left > 0 {
		bucket := core.UsageBucketStart(at)
		first := int(at.Sub(bucket) / time.Minute)
		n := min(core.UsageMinutes-first, left)
		mask := 0
		for i := first; i < first+n; i++ {
			mask |= 1 << i
		}
		out := perMinute * n
		f.exec(`INSERT INTO session_usage (session_id, bucket, model, thread, input_tokens, cached_input_tokens,
			cache_creation_tokens, output_tokens, messages, minute_mask) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (session_id, bucket, model, thread) DO UPDATE SET
			  input_tokens = input_tokens + excluded.input_tokens,
			  cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
			  output_tokens = output_tokens + excluded.output_tokens,
			  messages = messages + excluded.messages, minute_mask = minute_mask | excluded.minute_mask`,
			session, core.FormatTime(bucket), model, string(thread), out/2, out*10, 0, out, n, mask)
		left -= n
		at = bucket.Add(core.UsageBucketWidth)
	}
	f.exec(`UPDATE sessions SET
		input_tokens = (SELECT COALESCE(SUM(input_tokens), 0) FROM session_usage WHERE session_id = sessions.id),
		cached_input_tokens = (SELECT COALESCE(SUM(cached_input_tokens), 0) FROM session_usage WHERE session_id = sessions.id),
		cache_creation_tokens = (SELECT COALESCE(SUM(cache_creation_tokens), 0) FROM session_usage WHERE session_id = sessions.id),
		output_tokens = (SELECT COALESCE(SUM(output_tokens), 0) FROM session_usage WHERE session_id = sessions.id)
		WHERE id = ?`, session)
	f.exec(`UPDATE sessions SET total_tokens = input_tokens + cached_input_tokens + cache_creation_tokens + output_tokens
		WHERE id = ?`, session)
}

// event writes one event.
func (f *dmFixture) event(kind core.EventKind, at time.Time, session, project, item string, payload map[string]any) string {
	f.tb.Helper()
	id := f.id("EVNT")
	body := []byte("{}")
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		require.NoError(f.tb, err)
	}
	f.exec(`INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, core.FormatTime(at), string(kind), session, project, item, string(body))
	return id
}

// project writes a project row.
func (f *dmFixture) project(slug string, retired bool) {
	f.tb.Helper()
	now := core.FormatTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var retiredAt any
	if retired {
		retiredAt = now
	}
	f.exec(`INSERT INTO projects (id, slug, name, created_at, updated_at, retired_at) VALUES (?, ?, ?, ?, ?, ?)`,
		f.id("PROJ"), slug, slug, now, now, retiredAt)
}

// task writes a task row, open, created at created (and a task.transition
// creating it, by session, when session is not "-").
func (f *dmFixture) task(project, plan, title string, created time.Time, session string) string {
	f.tb.Helper()
	id := f.id("TASK")
	f.exec(`INSERT INTO tasks (id, project_slug, plan_slug, title, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'open', ?, ?)`, id, project, plan, title, core.FormatTime(created), core.FormatTime(created))
	if session != "-" {
		f.event(core.EventTaskTransition, created, session, project, id, map[string]any{"to": "open", "created": true})
	}
	return id
}

// move records a task.transition by session at at and sets the task's row
// to match: from is the status before (an update's), "" for a claim.
func (f *dmFixture) move(task string, at time.Time, session, from, to string) {
	f.tb.Helper()
	var project string
	require.NoError(f.tb, f.ex.QueryRowContext(f.ctx, `SELECT project_slug FROM tasks WHERE id = ?`, task).Scan(&project))
	payload := map[string]any{"to": to}
	if from != "" {
		payload["from"] = from
	} else if to == "in_progress" {
		payload["claimed_by"] = session
	}
	f.event(core.EventTaskTransition, at, session, project, task, payload)
	var closedAt any
	if core.TaskStatus(to).Closed() {
		closedAt = core.FormatTime(at)
	}
	f.exec(`UPDATE tasks SET status = ?, updated_at = ?, closed_at = ? WHERE id = ?`, to, core.FormatTime(at), closedAt, task)
}

// memory writes a memories_index row.
func (f *dmFixture) memory(name, project string, created time.Time) string {
	f.tb.Helper()
	id := f.id("MEMO")
	f.exec(`INSERT INTO memories_index (id, kind, name, project, file_path, content_hash, created_at, updated_at, valid_from)
		VALUES (?, 'gotcha', ?, ?, ?, 'h', ?, ?, ?)`, id, name, project, "memory/"+project+"/"+name+".md",
		core.FormatTime(created), core.FormatTime(created), core.FormatTime(created))
	return id
}

// note writes a notes_index row with tags.
func (f *dmFixture) note(title, project string, tags []string, created time.Time) string {
	f.tb.Helper()
	id := f.id("NOTE")
	if tags == nil {
		tags = []string{}
	}
	raw, err := json.Marshal(tags)
	require.NoError(f.tb, err)
	slug := strings.ReplaceAll(strings.ToLower(title), " ", "-")
	f.exec(`INSERT INTO notes_index (id, title, slug, project, file_path, tags, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'h', ?, ?)`, id, title, slug, project, "notes/"+project+"/"+id+".md", string(raw),
		core.FormatTime(created), core.FormatTime(created))
	return id
}

// trial writes a trial row and its trial.recorded event.
func (f *dmFixture) trial(title, lab, project, session string, at time.Time) string {
	f.tb.Helper()
	id := f.id("TRIA")
	f.exec(`INSERT INTO trials (id, lab, title, outcome, session_id, project_slug, created_at)
		VALUES (?, ?, ?, 'confirmed', ?, ?, ?)`, id, lab, title, session, project, core.FormatTime(at))
	f.event(core.EventTrialRecorded, at, session, project, id, map[string]any{"title": title, "lab": lab, "outcome": "confirmed"})
	return id
}

// commit writes a commit record.
func (f *dmFixture) commit(ownerKind, ownerID, recordedBy string, at time.Time) string {
	f.tb.Helper()
	f.n++
	full := fmt.Sprintf("%040x", f.n)
	f.exec(`INSERT INTO commit_records (owner_kind, owner_id, commit_id, recorded_by, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		ownerKind, ownerID, full, recordedBy, core.FormatTime(at))
	return full
}

// day reads the fixture's day holding at, as of a moment long after it.
func (f *dmFixture) day(at time.Time) Entry {
	f.tb.Helper()
	e, err := dayAt(f.ctx, f.db, at, at.AddDate(1, 0, 0), f.loc)
	require.NoError(f.tb, err)
	return e
}

// thread finds a thread of an entry by key, in the spotlight or after it.
func dmThread(tb testing.TB, e Entry, key string) Thread {
	tb.Helper()
	for _, ts := range [][]Thread{e.Spotlight, e.Also} {
		for _, t := range ts {
			if t.Key == key {
				return t
			}
		}
	}
	require.Failf(tb, "no thread", "%s in %v", key, dmKeys(e))
	return Thread{}
}

// dmKeys lists an entry's thread keys, spotlight first.
func dmKeys(e Entry) []string {
	var out []string
	for _, ts := range [][]Thread{e.Spotlight, e.Also} {
		for _, t := range ts {
			out = append(out, t.Key)
		}
	}
	return out
}

// dmSpotKeys lists the spotlight's keys.
func dmSpotKeys(e Entry) []string {
	out := []string{}
	for _, t := range e.Spotlight {
		out = append(out, t.Key)
	}
	return out
}
