package store

// The day model's reads (internal/diary: the console's Diary). Every read
// takes the handle it is given -- the console passes its filtered one
// (OpenView), on which a locked project's sessions, tasks, events and ledger
// rows are simply not there -- and a UTC range [from, to) the caller computed
// from a local day: nothing here knows a zone. The few payload fields the
// model uses are extracted in SQL and the payload itself is never read whole,
// because an edit's carries its diff and a plan capture's the whole plan.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// diaryChunk bounds the ids one IN list binds, far below SQLite's limit on a
// statement's parameters, so a long month never builds a statement it refuses.
const diaryChunk = 500

// DiaryEvent is one event as the day model reads it: its identity, time,
// session, project and item, and the payload fields it uses ("" or false
// where the payload has none). The vocabulary is internal/core's (EventKind)
// and the recorders': task.transition carries to, from, created, claimed_by,
// released, commits_recorded and, for a plan's tracking task, plan_slug;
// plan.captured, plan.presented and plan.approved carry plan_slug (captured
// and approved a title); plan.shipped names its plan in item_id;
// memory.written carries name and one of updated, appended or edited;
// note.written carries title, or slug for an append or a delete, and one of
// updated, appended, edited or deleted; trial.recorded carries title and lab;
// session.flagged verdict and reason; agent.mishap description. No
// task.transition carries the task's title: a task's title is the task's.
type DiaryEvent struct {
	ID        string
	At        time.Time
	Kind      core.EventKind
	SessionID string
	Project   string
	ItemID    string

	To, From, ClaimedBy, PlanSlug      string
	Created, Released                  bool
	Commits                            int
	Name, Title, Slug                  string
	Updated, Appended, Edited, Deleted bool
	Verdict, Reason, Description, Lab  string
}

// diaryEventCols is the column list DiaryEvents scans, in order. A boolean
// field is the JSON true and nothing else (IS 1), so a payload holding some
// other value there reads as false rather than failing the scan.
const diaryEventCols = `id, ts, kind, session_id, project_slug, item_id,
	COALESCE(json_extract(payload, '$.to'), ''), COALESCE(json_extract(payload, '$.from'), ''),
	COALESCE(json_extract(payload, '$.claimed_by'), ''), COALESCE(json_extract(payload, '$.plan_slug'), ''),
	json_extract(payload, '$.created') IS 1, json_extract(payload, '$.released') IS 1,
	CAST(COALESCE(json_extract(payload, '$.commits_recorded'), 0) AS INTEGER),
	COALESCE(json_extract(payload, '$.name'), ''), COALESCE(json_extract(payload, '$.title'), ''),
	COALESCE(json_extract(payload, '$.slug'), ''),
	json_extract(payload, '$.updated') IS 1, json_extract(payload, '$.appended') IS 1,
	json_extract(payload, '$.edited') IS 1, json_extract(payload, '$.deleted') IS 1,
	COALESCE(json_extract(payload, '$.verdict'), ''), COALESCE(json_extract(payload, '$.reason'), ''),
	COALESCE(json_extract(payload, '$.description'), ''), COALESCE(json_extract(payload, '$.lab'), '')`

// DiaryEvents returns the events of the given kinds recorded in [from, to),
// oldest first (ties by id). Nothing is returned unless from is before to and
// a kind is named.
func DiaryEvents(ctx context.Context, db *sql.DB, from, to time.Time, kinds []core.EventKind) ([]DiaryEvent, error) {
	if !from.Before(to) || len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(kinds)+2)
	for _, k := range kinds {
		args = append(args, string(k))
	}
	args = append(args, core.FormatTime(from), core.FormatTime(to))
	rows, err := db.QueryContext(ctx, `SELECT `+diaryEventCols+` FROM events
		WHERE kind IN (`+placeholders(len(kinds))+`) AND ts >= ? AND ts < ?
		ORDER BY ts, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.DiaryEvents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DiaryEvent
	for rows.Next() {
		var (
			e        DiaryEvent
			ts, kind string
		)
		if err := rows.Scan(&e.ID, &ts, &kind, &e.SessionID, &e.Project, &e.ItemID,
			&e.To, &e.From, &e.ClaimedBy, &e.PlanSlug, &e.Created, &e.Released, &e.Commits,
			&e.Name, &e.Title, &e.Slug, &e.Updated, &e.Appended, &e.Edited, &e.Deleted,
			&e.Verdict, &e.Reason, &e.Description, &e.Lab); err != nil {
			return nil, fmt.Errorf("store.DiaryEvents: scan: %w", err)
		}
		if e.At, err = core.ParseTime(ts); err != nil {
			return nil, fmt.Errorf("store.DiaryEvents: event %s: %w", e.ID, err)
		}
		e.Kind = core.EventKind(kind)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.DiaryEvents: %w", err)
	}
	return out, nil
}

// DiarySession is what the day model reads of a session to place it on its
// days, judge it and count its tokens: its project and working directory,
// status, findings and flag, when it began and was last updated (where an
// ended session ended), its stored token totals, and whether the usage ledger
// holds any row of it at all. A session with totals and no ledger row
// predates the ledger.
type DiarySession struct {
	ID       string
	Project  string
	CWD      string
	Status   core.SessionStatus
	Findings string
	Verdict  core.SessionVerdict
	Created  time.Time
	Updated  time.Time
	Totals   core.TokenUsage
	Ledger   bool
}

// diarySessionCols is the column list scanDiarySessions reads, in order.
const diarySessionCols = `id, project_slug, cwd, status, findings, COALESCE(verdict, ''), created_at, updated_at,
	input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, total_tokens,
	EXISTS (SELECT 1 FROM session_usage u WHERE u.session_id = sessions.id)`

// DiarySessionsBetween returns the sessions created, or last updated, in
// [from, to): the sessions that began or ended (or were last seen) in the
// range. Nothing is returned unless from is before to.
func DiarySessionsBetween(ctx context.Context, db *sql.DB, from, to time.Time) ([]DiarySession, error) {
	if !from.Before(to) {
		return nil, nil
	}
	lo, hi := core.FormatTime(from), core.FormatTime(to)
	rows, err := db.QueryContext(ctx, `SELECT `+diarySessionCols+` FROM sessions
		WHERE (created_at >= ? AND created_at < ?) OR (updated_at >= ? AND updated_at < ?)
		ORDER BY created_at, id`, lo, hi, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("store.DiarySessionsBetween: %w", err)
	}
	out, err := scanDiarySessions(rows)
	if err != nil {
		return nil, fmt.Errorf("store.DiarySessionsBetween: %w", err)
	}
	return out, nil
}

// DiarySessionsByIDs returns DiarySessionsBetween's facts for the sessions
// with the given ids, keyed by id; an id the handle does not hold is absent.
func DiarySessionsByIDs(ctx context.Context, db *sql.DB, ids []string) (map[string]DiarySession, error) {
	out := make(map[string]DiarySession, len(ids))
	for chunk := range slices.Chunk(diaryDistinct(ids), diaryChunk) {
		rows, err := db.QueryContext(ctx, `SELECT `+diarySessionCols+` FROM sessions
			WHERE id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("store.DiarySessionsByIDs: %w", err)
		}
		got, err := scanDiarySessions(rows)
		if err != nil {
			return nil, fmt.Errorf("store.DiarySessionsByIDs: %w", err)
		}
		for _, s := range got {
			out[s.ID] = s
		}
	}
	return out, nil
}

// scanDiarySessions scans diarySessionCols rows, closing rows.
func scanDiarySessions(rows *sql.Rows) ([]DiarySession, error) {
	defer func() { _ = rows.Close() }()
	var out []DiarySession
	for rows.Next() {
		var (
			s                DiarySession
			status, verdict  string
			created, updated string
		)
		if err := rows.Scan(&s.ID, &s.Project, &s.CWD, &status, &s.Findings, &verdict, &created, &updated,
			&s.Totals.Input, &s.Totals.Cached, &s.Totals.CacheCreation, &s.Totals.Output, &s.Totals.Total,
			&s.Ledger); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		var err error
		if s.Created, err = core.ParseTime(created); err != nil {
			return nil, fmt.Errorf("session %s created: %w", s.ID, err)
		}
		if s.Updated, err = core.ParseTime(updated); err != nil {
			return nil, fmt.Errorf("session %s updated: %w", s.ID, err)
		}
		s.Status, s.Verdict = core.SessionStatus(status), core.SessionVerdict(verdict)
		out = append(out, s)
	}
	return out, rows.Err()
}

// DiaryTask is what the day model reads of a task: where it is now (project
// and plan), its title, its status now, and when it was created and closed.
type DiaryTask struct {
	ID      string
	Project string
	Plan    string
	Title   string
	Status  core.TaskStatus
	Created time.Time
	Closed  *time.Time
}

// diaryTaskCols is the column list scanDiaryTasks reads, in order.
const diaryTaskCols = `id, project_slug, plan_slug, title, status, created_at, closed_at`

// DiaryTasksByIDs returns the tasks with the given ids, keyed by id; an id the
// handle does not hold is absent.
func DiaryTasksByIDs(ctx context.Context, db *sql.DB, ids []string) (map[string]DiaryTask, error) {
	out := make(map[string]DiaryTask, len(ids))
	for chunk := range slices.Chunk(diaryDistinct(ids), diaryChunk) {
		rows, err := db.QueryContext(ctx, `SELECT `+diaryTaskCols+` FROM tasks
			WHERE id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryTasksByIDs: %w", err)
		}
		got, err := scanDiaryTasks(rows)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryTasksByIDs: %w", err)
		}
		for _, t := range got {
			out[t.ID] = t
		}
	}
	return out, nil
}

// DiaryPlanSteps returns every step of the given plans as the tasks table
// holds them now (a plan is its project and slug: PlanRef), in no particular
// order.
func DiaryPlanSteps(ctx context.Context, db *sql.DB, plans []PlanRef) ([]DiaryTask, error) {
	var out []DiaryTask
	for chunk := range slices.Chunk(plans, diaryChunk/2) {
		terms := make([]string, len(chunk))
		args := make([]any, 0, 2*len(chunk))
		for i, p := range chunk {
			terms[i] = `(project_slug = ? AND plan_slug = ?)`
			args = append(args, p.Project, p.Slug)
		}
		rows, err := db.QueryContext(ctx, `SELECT `+diaryTaskCols+` FROM tasks
			WHERE plan_slug <> '' AND (`+strings.Join(terms, " OR ")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryPlanSteps: %w", err)
		}
		got, err := scanDiaryTasks(rows)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryPlanSteps: %w", err)
		}
		out = append(out, got...)
	}
	return out, nil
}

// scanDiaryTasks scans diaryTaskCols rows, closing rows.
func scanDiaryTasks(rows *sql.Rows) ([]DiaryTask, error) {
	defer func() { _ = rows.Close() }()
	var out []DiaryTask
	for rows.Next() {
		var (
			t       DiaryTask
			status  string
			created string
			closed  sql.NullString
		)
		if err := rows.Scan(&t.ID, &t.Project, &t.Plan, &t.Title, &status, &created, &closed); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		var err error
		if t.Created, err = core.ParseTime(created); err != nil {
			return nil, fmt.Errorf("task %s created: %w", t.ID, err)
		}
		if t.Closed, err = nullTimePtr(closed); err != nil {
			return nil, fmt.Errorf("task %s closed: %w", t.ID, err)
		}
		t.Status = core.TaskStatus(status)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DiaryTransition is one task.transition of a task: when, the status it moved
// to and, where the event recorded it, the status before (every tasks_update
// records from; a claim, a release and a creation do not).
type DiaryTransition struct {
	TaskID   string
	At       time.Time
	To, From string
	Created  bool
	Claimed  bool
	Released bool
}

// DiaryTransitions returns the task.transition events of the given tasks
// recorded before before, oldest first (ties by id): each task's status
// history up to a day's end.
func DiaryTransitions(ctx context.Context, db *sql.DB, taskIDs []string, before time.Time) ([]DiaryTransition, error) {
	var out []DiaryTransition
	for chunk := range slices.Chunk(diaryDistinct(taskIDs), diaryChunk) {
		args := append(anySlice(chunk), string(core.EventTaskTransition), core.FormatTime(before))
		// +kind keeps the planner on the item index: the kind index would
		// walk every transition ever recorded before the bound.
		rows, err := db.QueryContext(ctx, `SELECT item_id, ts,
			COALESCE(json_extract(payload, '$.to'), ''), COALESCE(json_extract(payload, '$.from'), ''),
			json_extract(payload, '$.created') IS 1,
			COALESCE(json_extract(payload, '$.claimed_by'), '') <> '',
			json_extract(payload, '$.released') IS 1
			FROM events
			WHERE item_id IN (`+placeholders(len(chunk))+`) AND +kind = ? AND ts < ?
			ORDER BY ts, id`, args...)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryTransitions: %w", err)
		}
		got, err := scanDiaryTransitions(rows)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryTransitions: %w", err)
		}
		out = append(out, got...)
	}
	if len(out) > 0 && len(taskIDs) > diaryChunk {
		slices.SortStableFunc(out, func(a, b DiaryTransition) int { return a.At.Compare(b.At) })
	}
	return out, nil
}

// scanDiaryTransitions scans DiaryTransitions' rows, closing rows.
func scanDiaryTransitions(rows *sql.Rows) ([]DiaryTransition, error) {
	defer func() { _ = rows.Close() }()
	var out []DiaryTransition
	for rows.Next() {
		var (
			t  DiaryTransition
			ts string
		)
		if err := rows.Scan(&t.TaskID, &ts, &t.To, &t.From, &t.Created, &t.Claimed, &t.Released); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		var err error
		if t.At, err = core.ParseTime(ts); err != nil {
			return nil, fmt.Errorf("transition of %s: %w", t.TaskID, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DiaryCommit is one commit record (the commits.record option): the session
// or task that recorded it, the commit, the session whose call recorded it
// and when.
type DiaryCommit struct {
	Owner      CommitOwner
	OwnerID    string
	Commit     string
	RecordedBy string
	At         time.Time
}

// DiaryCommits returns the commits recorded in [from, to), oldest first. The
// table is empty while commits.record is off.
func DiaryCommits(ctx context.Context, db *sql.DB, from, to time.Time) ([]DiaryCommit, error) {
	if !from.Before(to) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT owner_kind, owner_id, commit_id, recorded_by, recorded_at
		FROM commit_records WHERE recorded_at >= ? AND recorded_at < ?
		ORDER BY recorded_at, owner_kind, owner_id, commit_id`, core.FormatTime(from), core.FormatTime(to))
	if err != nil {
		return nil, fmt.Errorf("store.DiaryCommits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DiaryCommit
	for rows.Next() {
		var (
			c         DiaryCommit
			owner, at string
		)
		if err := rows.Scan(&owner, &c.OwnerID, &c.Commit, &c.RecordedBy, &at); err != nil {
			return nil, fmt.Errorf("store.DiaryCommits: scan: %w", err)
		}
		if c.At, err = core.ParseTime(at); err != nil {
			return nil, fmt.Errorf("store.DiaryCommits: commit %s: %w", c.Commit, err)
		}
		c.Owner = CommitOwner(owner)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.DiaryCommits: %w", err)
	}
	return out, nil
}

// diaryPromptKinds are the events that carry the owner's prompt: a prompt that
// matched no memory (hook.prompt, pruned with the transport events after
// gardener.tool_event_retention_days) and one that did (retrieval.injected,
// kept). The payload key is prompt in both, absent when there was none (a
// session start's briefing) or the text was withheld.
var diaryPromptKinds = []core.EventKind{core.EventHookPrompt, core.EventInjected}

// DiaryFirstPrompts returns, for each of the given sessions, the first prompt
// it recorded in [from, to), keyed by session id; a session with none is
// absent. The prompt-bearing events are found without parsing their payloads
// (an injection's carries the injected text), and only each session's first
// one is parsed: the CROSS JOIN keeps the sessions' firsts the outer loop and
// +kind keeps the kind index, which holds every prompt ever recorded, out of
// the lookup back.
func DiaryFirstPrompts(ctx context.Context, db *sql.DB, from, to time.Time, sessionIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if !from.Before(to) {
		return out, nil
	}
	kinds := placeholders(len(diaryPromptKinds))
	kindArgs := make([]any, len(diaryPromptKinds))
	for i, k := range diaryPromptKinds {
		kindArgs[i] = string(k)
	}
	for chunk := range slices.Chunk(diaryDistinct(sessionIDs), diaryChunk) {
		args := slices.Concat(anySlice(chunk), kindArgs, []any{core.FormatTime(from), core.FormatTime(to)}, kindArgs)
		rows, err := db.QueryContext(ctx, `WITH firsts AS (
				SELECT session_id, MIN(ts) AS ts FROM events
				 WHERE session_id IN (`+placeholders(len(chunk))+`) AND kind IN (`+kinds+`)
				   AND ts >= ? AND ts < ? AND instr(payload, '"prompt":') > 0
				 GROUP BY session_id)
			SELECT f.session_id, COALESCE(json_extract(e.payload, '$.prompt'), '')
			  FROM firsts f CROSS JOIN events e ON e.session_id = f.session_id AND e.ts = f.ts
			 WHERE +e.kind IN (`+kinds+`)
			 ORDER BY e.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryFirstPrompts: %w", err)
		}
		if err := scanDiaryPrompts(rows, out); err != nil {
			return nil, fmt.Errorf("store.DiaryFirstPrompts: %w", err)
		}
	}
	return out, nil
}

// scanDiaryPrompts scans DiaryFirstPrompts' rows into out, the first prompt
// with words in it per session, closing rows.
func scanDiaryPrompts(rows *sql.Rows, out map[string]string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, prompt string
		if err := rows.Scan(&id, &prompt); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if _, seen := out[id]; !seen && strings.TrimSpace(prompt) != "" {
			out[id] = prompt
		}
	}
	return rows.Err()
}

// DiaryPlanNotes returns the notes tagged into each of the given plans'
// compositions (the plan:<slug> tag), keyed by slug, each list in
// NotesByTag's order (newest updated first, ties by id descending) -- the
// order plans.Composition reads, so a caller choosing the primary as it does
// chooses the same note. A slug no note carries is absent. Each note carries
// what choosing the primary reads and a link shows: its id, title, slug,
// project, tags and times, and nothing else.
func DiaryPlanNotes(ctx context.Context, db *sql.DB, slugs []string) (map[string][]core.Note, error) {
	out := map[string][]core.Note{}
	for chunk := range slices.Chunk(diaryDistinct(slugs), diaryChunk) {
		tags := make([]any, len(chunk))
		for i, s := range chunk {
			tags[i] = "plan:" + s
		}
		rows, err := db.QueryContext(ctx, `SELECT je.value, n.id, n.title, n.slug, n.project, n.tags,
			n.created_at, n.updated_at
			FROM notes_index n, json_each(n.tags) je
			WHERE je.value IN (`+placeholders(len(chunk))+`)
			ORDER BY n.updated_at DESC, n.id DESC`, tags...)
		if err != nil {
			return nil, fmt.Errorf("store.DiaryPlanNotes: %w", err)
		}
		if err := scanDiaryPlanNotes(rows, out); err != nil {
			return nil, fmt.Errorf("store.DiaryPlanNotes: %w", err)
		}
	}
	return out, nil
}

// scanDiaryPlanNotes scans DiaryPlanNotes' rows into out, closing rows.
func scanDiaryPlanNotes(rows *sql.Rows, out map[string][]core.Note) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			tag, tags        string
			created, updated string
			n                core.Note
		)
		if err := rows.Scan(&tag, &n.ID, &n.Title, &n.Slug, &n.Project, &tags, &created, &updated); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		n.Tags = decodeTags(tags)
		var err error
		if n.Created, err = core.ParseTime(created); err != nil {
			return fmt.Errorf("note %s created: %w", n.ID, err)
		}
		if n.Updated, err = core.ParseTime(updated); err != nil {
			return fmt.Errorf("note %s updated: %w", n.ID, err)
		}
		slug := strings.TrimPrefix(tag, "plan:")
		out[slug] = append(out[slug], n)
	}
	return rows.Err()
}

// diaryDistinct returns ids without blanks and repeats, sorted, as a copy.
func diaryDistinct(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
