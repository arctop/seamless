package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// diaryEventAt inserts an event at a chosen time.
func diaryEventAt(t *testing.T, db *sql.DB, id string, at time.Time, kind core.EventKind, session, project, item string, payload map[string]any) {
	t.Helper()
	body := "{}"
	if payload != nil {
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		body = string(raw)
	}
	_, err := db.ExecContext(context.Background(), `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, core.FormatTime(at), string(kind), session, project, item, body)
	require.NoError(t, err)
}

// diarySession inserts a session created and updated at chosen times.
func diarySession(t *testing.T, db *sql.DB, id, project string, created, updated time.Time, total int) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO sessions (id, name, project_slug, status, cwd,
		created_at, updated_at, total_tokens, output_tokens) VALUES (?, ?, ?, 'completed', '/work/'||?, ?, ?, ?, ?)`,
		id, "cc/"+id, project, project, core.FormatTime(created), core.FormatTime(updated), total, total/10)
	require.NoError(t, err)
}

// The day model's event read extracts the payload fields it uses, in time
// order, for the kinds asked, in [from, to); a field holding another type
// reads as false or empty instead of failing.
func TestDiaryEvents_ExtractsThePayloadFields(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	diaryEventAt(t, db, "01EV0000000000000000000002", day.Add(2*time.Hour), core.EventTaskTransition, "s1", "example-ops", "t1",
		map[string]any{"to": "done", "from": "in_progress", "commits_recorded": 2, "diff": strings.Repeat("x", 4096)})
	diaryEventAt(t, db, "01EV0000000000000000000001", day.Add(time.Hour), core.EventTaskTransition, "s1", "example-ops", "t1",
		map[string]any{"to": "in_progress", "claimed_by": "s1", "created": "yes"})
	diaryEventAt(t, db, "01EV0000000000000000000003", day.Add(3*time.Hour), core.EventNoteWritten, "s1", "example-ops", "n1",
		map[string]any{"slug": "notes", "deleted": true})
	diaryEventAt(t, db, "01EV0000000000000000000004", day.Add(4*time.Hour), core.EventMemoryWritten, "s1", "example-ops", "m1",
		map[string]any{"name": "a-gotcha", "updated": true})
	diaryEventAt(t, db, "01EV0000000000000000000005", day.Add(5*time.Hour), core.EventToolCall, "s1", "example-ops", "", nil)
	diaryEventAt(t, db, "01EV0000000000000000000006", day.AddDate(0, 0, 1), core.EventMemoryWritten, "s1", "example-ops", "m2", nil)

	got, err := DiaryEvents(ctx, db, day, day.AddDate(0, 0, 1),
		[]core.EventKind{core.EventTaskTransition, core.EventNoteWritten, core.EventMemoryWritten})
	require.NoError(t, err)
	require.Len(t, got, 4, "the asked kinds, in the range: the next midnight is the next day's")
	require.Equal(t, "01EV0000000000000000000001", got[0].ID, "time order")
	require.Equal(t, DiaryEvent{ID: "01EV0000000000000000000001", At: day.Add(time.Hour), Kind: core.EventTaskTransition,
		SessionID: "s1", Project: "example-ops", ItemID: "t1", To: "in_progress", ClaimedBy: "s1"}, got[0],
		"created holding a string is not the JSON true")
	require.Equal(t, "in_progress", got[1].From)
	require.Equal(t, 2, got[1].Commits)
	require.True(t, got[2].Deleted)
	require.Equal(t, "notes", got[2].Slug)
	require.True(t, got[3].Updated)
	require.Equal(t, "a-gotcha", got[3].Name)
	none, err := DiaryEvents(ctx, db, day, day, []core.EventKind{core.EventTaskTransition})
	require.NoError(t, err)
	require.Empty(t, none)
}

// Sessions are found by a start or a last update in the range, and say
// whether the ledger holds any row of them.
func TestDiarySessionsBetween(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	diarySession(t, db, "01SE0000000000000000000001", "example-ops", day.Add(time.Hour), day.Add(2*time.Hour), 100)
	diarySession(t, db, "01SE0000000000000000000002", "example-ops", day.AddDate(0, 0, -1), day.Add(time.Hour), 0)
	diarySession(t, db, "01SE0000000000000000000003", "example-ops", day.AddDate(0, 0, -2), day.AddDate(0, 0, -1), 50)
	_, err := db.ExecContext(ctx, `INSERT INTO session_usage (session_id, bucket, model, thread, output_tokens)
		VALUES ('01SE0000000000000000000001', ?, 'm', 'main', 100)`, core.FormatTime(day.Add(time.Hour)))
	require.NoError(t, err)

	got, err := DiarySessionsBetween(ctx, db, day, day.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "01SE0000000000000000000002", got[0].ID, "by start")
	require.False(t, got[0].Ledger)
	require.Equal(t, "01SE0000000000000000000001", got[1].ID)
	require.True(t, got[1].Ledger)
	require.Equal(t, 100, got[1].Totals.Total)
	require.Equal(t, "/work/example-ops", got[1].CWD)
	byID, err := DiarySessionsByIDs(ctx, db, []string{"01SE0000000000000000000003", "absent", ""})
	require.NoError(t, err)
	require.Len(t, byID, 1)
	require.Equal(t, 50, byID["01SE0000000000000000000003"].Totals.Total)
}

// A plan's steps are its project's and slug's; the transitions come oldest
// first, before the bound, through the item index.
func TestDiaryPlanStepsAndTransitions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, task := range []struct{ id, project, plan string }{
		{"01TA0000000000000000000001", "example-ops", "rollout"},
		{"01TA0000000000000000000002", "example-ops", "rollout"},
		{"01TA0000000000000000000003", "example-finance", "rollout"},
		{"01TA0000000000000000000004", "example-ops", ""},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO tasks (id, project_slug, plan_slug, title, status, created_at, updated_at)
			VALUES (?, ?, ?, 'Step', 'open', ?, ?)`, task.id, task.project, task.plan, core.FormatTime(day), core.FormatTime(day))
		require.NoError(t, err)
	}
	steps, err := DiaryPlanSteps(ctx, db, []PlanRef{{Project: "example-ops", Slug: "rollout"}})
	require.NoError(t, err)
	var ids []string
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	require.ElementsMatch(t, []string{"01TA0000000000000000000001", "01TA0000000000000000000002"}, ids,
		"a slug is only unique within its project")
	diaryEventAt(t, db, "01EV0000000000000000000002", day.Add(2*time.Hour), core.EventTaskTransition, "", "example-ops",
		"01TA0000000000000000000001", map[string]any{"to": "done", "from": "in_progress"})
	diaryEventAt(t, db, "01EV0000000000000000000001", day.Add(time.Hour), core.EventTaskTransition, "", "example-ops",
		"01TA0000000000000000000001", map[string]any{"to": "in_progress", "claimed_by": "s1"})
	diaryEventAt(t, db, "01EV0000000000000000000003", day.Add(3*time.Hour), core.EventTaskTransition, "", "example-ops",
		"01TA0000000000000000000001", map[string]any{"to": "open", "released": true})
	history, err := DiaryTransitions(ctx, db, []string{"01TA0000000000000000000001", "01TA0000000000000000000002"}, day.Add(3*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []DiaryTransition{
		{TaskID: "01TA0000000000000000000001", At: day.Add(time.Hour), To: "in_progress", Claimed: true},
		{TaskID: "01TA0000000000000000000001", At: day.Add(2 * time.Hour), To: "done", From: "in_progress"},
	}, history, "oldest first, strictly before the bound")
	var plan strings.Builder
	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT item_id FROM events
		WHERE item_id IN (?, ?) AND +kind = ? AND ts < ?`, "a", "b", "task.transition", "z")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(t, rows.Err())
	require.Contains(t, plan.String(), "idx_events_item", "a task's history is read by its id, never by walking every transition")
}

// The first prompt of each session in the range: an injection with no prompt
// (a session start's briefing) is passed over, and so are other sessions.
func TestDiaryFirstPrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	big := strings.Repeat("y", 2048)
	diaryEventAt(t, db, "01EV0000000000000000000001", day.Add(time.Hour), core.EventInjected, "s1", "p", "",
		map[string]any{"hook": "session-start", "content": big})
	diaryEventAt(t, db, "01EV0000000000000000000002", day.Add(2*time.Hour), core.EventInjected, "s1", "p", "",
		map[string]any{"hook": "user-prompt-submit", "prompt": "Why is the build red?", "content": big})
	diaryEventAt(t, db, "01EV0000000000000000000003", day.Add(3*time.Hour), core.EventHookPrompt, "s1", "p", "",
		map[string]any{"prompt": "later prompt"})
	diaryEventAt(t, db, "01EV0000000000000000000004", day.Add(90*time.Minute), core.EventHookPrompt, "s2", "p", "",
		map[string]any{"prompt": "unmatched first"})
	diaryEventAt(t, db, "01EV0000000000000000000005", day.Add(time.Hour), core.EventHookPrompt, "s3", "p", "",
		map[string]any{"prompt": "not asked for"})
	diaryEventAt(t, db, "01EV0000000000000000000006", day.Add(-time.Hour), core.EventHookPrompt, "s2", "p", "",
		map[string]any{"prompt": "the day before"})
	got, err := DiaryFirstPrompts(ctx, db, day, day.AddDate(0, 0, 1), []string{"s1", "s2", "s4"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"s1": "Why is the build red?", "s2": "unmatched first"}, got)
}

// A plan's composition notes, by slug, in NotesByTag's order.
func TestDiaryPlanNotes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for i, n := range []struct {
		id, project, tags string
		updated           time.Time
	}{
		{"01NO0000000000000000000001", "example-ops", `["plan:a","cc-plan"]`, at},
		{"01NO0000000000000000000002", "example-ops", `["plan:a","plan:b"]`, at.Add(time.Hour)},
		{"01NO0000000000000000000003", "example-finance", `["plan:c"]`, at},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO notes_index (id, title, slug, project, file_path, tags, content_hash,
			created_at, updated_at) VALUES (?, 'T', ?, ?, ?, ?, 'h', ?, ?)`, n.id, n.id, n.project,
			filepath.Join("notes", n.id+".md"), n.tags, core.FormatTime(at), core.FormatTime(n.updated))
		require.NoError(t, err, i)
	}
	got, err := DiaryPlanNotes(ctx, db, []string{"a", "b", "absent"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, []string{"01NO0000000000000000000002", "01NO0000000000000000000001"},
		[]string{got["a"][0].ID, got["a"][1].ID}, "newest updated first")
	require.Equal(t, []string{"plan:a", "cc-plan"}, got["a"][1].Tags)
	require.Len(t, got["b"], 1)
	want, err := NotesByTag(ctx, db, "", "plan:a")
	require.NoError(t, err)
	require.Equal(t, want[0].ID, got["a"][0].ID)
	require.Equal(t, want[1].ID, got["a"][1].ID)
}

// Through the console's filtered handle a locked project's events, sessions,
// commits and prompts are not there.
func TestDiaryReads_TheViewHidesALockedProject(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	diarySession(t, db, "01SE0000000000000000000001", "example-ops", day.Add(time.Hour), day.Add(2*time.Hour), 10)
	diarySession(t, db, "01SE0000000000000000000002", "example-finance", day.Add(time.Hour), day.Add(2*time.Hour), 10)
	for i, s := range []string{"01SE0000000000000000000001", "01SE0000000000000000000002"} {
		project := []string{"example-ops", "example-finance"}[i]
		diaryEventAt(t, db, "01EV000000000000000000000"+string(rune('1'+i)), day.Add(time.Hour), core.EventHookPrompt, s, project, "",
			map[string]any{"prompt": "hello"})
		_, err := db.ExecContext(ctx, `INSERT INTO commit_records (owner_kind, owner_id, commit_id, recorded_by, recorded_at)
			VALUES ('session', ?, ?, ?, ?)`, s, strings.Repeat(string(rune('a'+i)), 40), s, core.FormatTime(day.Add(time.Hour)))
		require.NoError(t, err)
	}
	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	sessions, err := DiarySessionsBetween(ctx, view, day, day.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "example-ops", sessions[0].Project)
	events, err := DiaryEvents(ctx, view, day, day.AddDate(0, 0, 1), []core.EventKind{core.EventHookPrompt})
	require.NoError(t, err)
	require.Len(t, events, 1)
	commits, err := DiaryCommits(ctx, view, day, day.AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, commits, 1)
	prompts, err := DiaryFirstPrompts(ctx, view, day, day.AddDate(0, 0, 1),
		[]string{"01SE0000000000000000000001", "01SE0000000000000000000002"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"01SE0000000000000000000001": "hello"}, prompts)
}
