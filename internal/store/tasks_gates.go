// Task gates: a task that waits on a person or an event outside Seamless. A
// step's own gate (gate_who/what, open until cleared) and a gate-kind task (a
// person's action the steps depending on it wait for) both keep work off the
// ready queue; the queue's four groups partition the open tasks:
//
//	gates    a gate-kind task, open or in progress: the people's queue
//	gated    a step held by people: its own gate is open, or an unfinished
//	         blocker is a gate
//	blocked  any other step with an unfinished blocker
//	ready    the rest: open, no open gate, no unfinished blocker
//
// The predicates below are the one statement of that rule in SQL; every
// queue query, ClaimTask's compare-and-set, the plan rollups and the project
// board's counts (TaskCountsByProject) build on them.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/core"
)

// ErrTaskGate is returned when a write would leave a task's gate or kind
// breaking the gate rules (checkTaskGate), or asks for an impossible gate
// change: clearing a gate that is not open, or editing and clearing a gate in
// one patch.
var ErrTaskGate = errors.New("invalid task gate")

// ErrTaskGated is returned when a claim targets a task that waits on people: a
// gate (never claimed) or a step whose own gate is open. Nobody holds it; it
// becomes claimable once the gate is cleared.
var ErrTaskGated = errors.New("task gated")

// The gate's text limits: a gate is a line in a queue, not a second body.
const (
	gateWhoMaxRunes  = 100
	gateLineMaxRunes = 500
	gateNoteMaxRunes = 2000
)

// TaskGatedError is ClaimTask's ErrTaskGated refusal. It carries the gate so a
// surface can judge its words against the caller's read fence: who and what
// are the task's content, so a surface sets Withhold for a caller that may
// write the task but not read it, and the message then names no one.
type TaskGatedError struct {
	TaskID   string
	IsGate   bool
	Gate     core.TaskGate
	Withhold bool
}

func (e *TaskGatedError) Error() string {
	waits := "a person"
	if !e.Withhold {
		waits = e.Gate.Who + ": " + e.Gate.What
	}
	if e.IsGate {
		return fmt.Sprintf("store.ClaimTask: %s: task %q is a gate (%s), which nobody claims: once that has "+
			"happened, clear it (tasks_update gate_clear), which closes it and unblocks the tasks that depend on it",
			ErrTaskGated, e.TaskID, waits)
	}
	return fmt.Sprintf("store.ClaimTask: %s: task %q waits on %s: once that has happened, clear its gate "+
		"(tasks_update gate_clear) and claim it then", ErrTaskGated, e.TaskID, waits)
}

// Unwrap makes errors.Is(err, ErrTaskGated) hold.
func (e *TaskGatedError) Unwrap() error { return ErrTaskGated }

// TaskGatePatch edits a task's gate: each non-nil field replaces that field
// (an empty string clears an optional one). Any edit (re)opens the gate as it
// now reads, dropping a clearance record: editing a cleared gate sets a new
// one.
type TaskGatePatch struct {
	Who       *string
	What      *string
	Decision  *string
	ReadyWhen *string
}

// TaskGateClear clears a task's open gate: By is who cleared it (the acting
// session's name, or "owner" from an owner surface) and Note what they said.
// Clearing a step's gate leaves its status alone, so an open step becomes
// ready once nothing else blocks it; clearing a gate-kind task closes it as
// done, which unblocks its dependents.
type TaskGateClear struct {
	By   string
	Note string
}

// checkGate refuses a patch that both edits and clears the gate: the two
// orders mean different things, so neither is guessed.
func (p TaskPatch) checkGate() error {
	if p.Gate != nil && p.ClearGate != nil {
		return fmt.Errorf("%w: edit a gate or clear it, not both in one update", ErrTaskGate)
	}
	if p.Kind != nil && !p.Kind.Valid() {
		return fmt.Errorf("%w: kind %q: valid values are %s", ErrTaskGate, *p.Kind, kindList())
	}
	return nil
}

// applyGatePatch applies the patch's kind and gate changes to t and answers
// the status the update moves the task to (the patch's own, or done for a
// cleared gate-kind task; nil keeps the status). A gate-kind task moved back
// to open or in progress has its gate opened again: a gate's clearance is the
// record of its closing.
func applyGatePatch(t *core.Task, p TaskPatch, now time.Time) (*core.TaskStatus, error) {
	status := p.Status
	if p.Kind != nil {
		t.Kind = *p.Kind
	}
	if g := p.Gate; g != nil {
		for _, f := range []struct {
			dst *string
			src *string
		}{{&t.Gate.Who, g.Who}, {&t.Gate.What, g.What}, {&t.Gate.Decision, g.Decision}, {&t.Gate.ReadyWhen, g.ReadyWhen}} {
			if f.src != nil {
				*f.dst = strings.TrimSpace(*f.src)
			}
		}
		t.Gate.ClearedAt, t.Gate.ClearedBy, t.Gate.Note = nil, "", ""
	}
	if c := p.ClearGate; c != nil {
		if !t.Gate.Open() {
			return nil, fmt.Errorf("%w: the task has no open gate to clear", ErrTaskGate)
		}
		at := now.UTC()
		t.Gate.ClearedAt, t.Gate.ClearedBy, t.Gate.Note = &at, c.By, strings.TrimSpace(c.Note)
		if t.IsGate() && !t.Status.Closed() {
			if status != nil && *status != core.TaskDone {
				return nil, fmt.Errorf("%w: clearing a gate closes it as done, so it takes no status %q",
					ErrTaskGate, *status)
			}
			done := core.TaskDone
			status = &done
		}
	} else if t.IsGate() && t.Gate.ClearedAt != nil && t.Status.Closed() && status != nil && !status.Closed() {
		t.Gate.ClearedAt, t.Gate.ClearedBy, t.Gate.Note = nil, "", ""
	}
	if err := checkTaskGate(*t); err != nil {
		return nil, err
	}
	return status, nil
}

// gateTextAccount folds a gate's words into a task's updated_account
// (taskTextAccount's rule, which judges the title and body): before is the
// label the row had and label the one taskTextAccount answered. A restricted
// account's change of the gate's words -- who, what, the optional lines, a
// clearing's note -- stamps its name. The owner's change that would clear the
// label keeps it while the gate still carries words that change did not
// rewrite: the label names one account for the whole task, and keeping it a
// little too long is the error taskTextAccount chooses too.
func gateTextAccount(before, label, writer string, prev, next core.TaskGate) string {
	changed := gateWords(prev) != gateWords(next)
	switch {
	case writer != "" && changed:
		return writer
	case writer == "" && label == "" && before != "" && next.Set() && !changed:
		return before
	}
	return label
}

// gateWords is the text of a gate a writer's words can be in.
func gateWords(g core.TaskGate) [5]string {
	return [5]string{g.Who, g.What, g.Decision, g.ReadyWhen, g.Note}
}

// normalizeGate trims a gate's text fields.
func normalizeGate(g core.TaskGate) core.TaskGate {
	g.Who = strings.TrimSpace(g.Who)
	g.What = strings.TrimSpace(g.What)
	g.Decision = strings.TrimSpace(g.Decision)
	g.ReadyWhen = strings.TrimSpace(g.ReadyWhen)
	g.ClearedBy = strings.TrimSpace(g.ClearedBy)
	g.Note = strings.TrimSpace(g.Note)
	return g
}

// checkTaskGate is the gate rules a stored task satisfies: a known kind; a
// gate names both who and what, or carries nothing at all; a gate-kind task
// carries a gate (it describes the gate); and the gate's lines are single
// lines within their limits. Its errors name the MCP arguments, since they
// reach agents verbatim.
func checkTaskGate(t core.Task) error {
	if !t.Kind.Valid() {
		return fmt.Errorf("%w: kind %q: valid values are %s", ErrTaskGate, t.Kind, kindList())
	}
	g := t.Gate
	anyText := g.Who != "" || g.What != "" || g.Decision != "" || g.ReadyWhen != ""
	switch {
	case anyText && g.Who == "":
		return fmt.Errorf("%w: a gate names who must act (gate_who)", ErrTaskGate)
	case anyText && g.What == "":
		return fmt.Errorf("%w: a gate says what must happen (gate_what)", ErrTaskGate)
	case t.IsGate() && !g.Set():
		return fmt.Errorf("%w: a task of kind gate names who must act and what must happen (gate_who, gate_what)", ErrTaskGate)
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{
		{"gate_who", g.Who, gateWhoMaxRunes},
		{"gate_what", g.What, gateLineMaxRunes},
		{"gate_decision", g.Decision, gateLineMaxRunes},
		{"gate_ready_when", g.ReadyWhen, gateLineMaxRunes},
	} {
		if strings.ContainsAny(f.v, "\r\n") {
			return fmt.Errorf("%w: %s must be one line", ErrTaskGate, f.name)
		}
		if n := utf8.RuneCountInString(f.v); n > f.max {
			return fmt.Errorf("%w: %s is %d characters; at most %d", ErrTaskGate, f.name, n, f.max)
		}
	}
	if n := utf8.RuneCountInString(g.Note); n > gateNoteMaxRunes {
		return fmt.Errorf("%w: gate_note is %d characters; at most %d", ErrTaskGate, n, gateNoteMaxRunes)
	}
	return nil
}

// kindList renders the valid task kinds for an error.
func kindList() string {
	names := make([]string, len(core.TaskKinds))
	for i, k := range core.TaskKinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// sqlOwnGateOpen holds for a row of alias whose own gate is open: set and not
// cleared (core.TaskGate.Open).
func sqlOwnGateOpen(alias string) string {
	return "(" + alias + ".gate_who <> '' AND " + alias + ".gate_cleared_at IS NULL)"
}

// sqlHeldByGate holds for a row of alias that itself waits on people: a gate,
// or a step whose own gate is open (core.Task.HeldByGate).
func sqlHeldByGate(alias string) string {
	return "(" + alias + ".kind = 'gate' OR " + sqlOwnGateOpen(alias) + ")"
}

// sqlOpenBlocker holds for a row of alias with an unfinished (open or in
// progress) blocker; gatesOnly narrows it to blockers that are gates.
func sqlOpenBlocker(alias string, gatesOnly bool) string {
	kind := ""
	if gatesOnly {
		kind = " AND b.kind = 'gate'"
	}
	return `EXISTS (
		      SELECT 1 FROM task_deps d
		      JOIN tasks b ON b.id = d.depends_on
		      WHERE d.task_id = ` + alias + `.id AND b.status IN ('open','in_progress')` + kind + `)`
}

// The queue's groups as predicates over alias (the file comment's partition).
func sqlReady(alias string) string {
	return alias + ".status = 'open' AND NOT " + sqlHeldByGate(alias) + " AND NOT " + sqlOpenBlocker(alias, false)
}

func sqlBlocked(alias string) string {
	return alias + ".status = 'open' AND NOT " + sqlHeldByGate(alias) +
		" AND " + sqlOpenBlocker(alias, false) + " AND NOT " + sqlOpenBlocker(alias, true)
}

func sqlGated(alias string) string {
	return alias + ".status = 'open' AND " + alias + ".kind <> 'gate'" +
		" AND (" + sqlOwnGateOpen(alias) + " OR " + sqlOpenBlocker(alias, true) + ")"
}

func sqlGates(alias string) string {
	return alias + ".kind = 'gate' AND " + alias + ".status IN ('open','in_progress')"
}

// sqlGatesOpen holds for a row of alias that is an open gate of a plan's
// rollup (PlanRollup.GatesOpen): a gate-kind task open or in progress, or an
// unfinished step whose own gate is open.
func sqlGatesOpen(alias string) string {
	return "(" + alias + ".status IN ('open','in_progress') AND " + sqlHeldByGate(alias) + ")"
}

// sqlClaimLive holds for a row of alias that a live claim holds at the bound
// instant (one bind: now): core.Task.ClaimLive -- in progress, a holder, and
// a lease that runs out strictly after now. It is the exact negation of
// ClaimTask's in_progress arms (constraint
// claimtask-claimless-inprogress-and-lease-boundary): a lease ending at now is
// already lapsed.
func sqlClaimLive(alias string) string {
	return "(" + alias + ".status = 'in_progress' AND " + alias + ".claimed_by <> '' AND " +
		alias + ".lease_expires_at IS NOT NULL AND " + alias + ".lease_expires_at > ?)"
}

// TaskCounts is a project's unfinished tasks in the queue's words: the one
// tally the project board shares with the Tasks page. Ready, Blocked and
// Waiting count the Tasks page's rails -- ready (sqlReady), blocked
// (sqlBlocked: waiting on unfinished work and on no gate) and the people's two
// (sqlGates and sqlGated: the gates, and the steps an open gate holds) -- so
// the two screens cannot disagree on what is blocked and what waits on people.
//
// A step in progress counts once, in the console's status words
// (internal/console/taskwords.go): InProgress while a live claim holds it,
// Lapsed when its holder's lease ran out, Unclaimed when no claim names a
// holder at all. A step no live claim holds that an open gate holds (its own,
// or a gate it depends on) reads as waiting instead, so it counts in Waiting
// and in neither of the last two. Every unfinished task counts in exactly one
// of the six. GatesOpen is a plan rollup's GatesOpen (sqlGatesOpen) over the
// whole project: the gates, and the unfinished steps whose own gate is open.
type TaskCounts struct {
	Ready      int `json:"ready"`
	InProgress int `json:"inProgress"`
	Lapsed     int `json:"lapsed"`
	Unclaimed  int `json:"unclaimed"`
	Blocked    int `json:"blocked"`
	Waiting    int `json:"waiting"`
	GatesOpen  int `json:"gatesOpen"`
}

// TaskCountsByProject tallies every project's unfinished tasks (TaskCounts),
// plan steps included, in one grouped query, keyed by project slug ("" is the
// global scope). A project with no unfinished task is absent. now is the
// instant a claim is judged live at.
func TaskCountsByProject(ctx context.Context, db *sql.DB, now time.Time) (map[string]TaskCounts, error) {
	at := core.FormatTime(now.UTC())
	gateHeld := "(" + sqlHeldByGate("t") + " OR " + sqlOpenBlocker("t", true) + ")"
	started := "t.status = 'in_progress' AND t.kind <> 'gate' AND NOT " + sqlClaimLive("t")
	stale := started + " AND NOT " + gateHeld
	rows, err := db.QueryContext(ctx, `
		SELECT t.project_slug,
		       SUM(CASE WHEN `+sqlReady("t")+` THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.kind <> 'gate' AND `+sqlClaimLive("t")+` THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+stale+` AND t.claimed_by <> '' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+stale+` AND t.claimed_by = '' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+sqlBlocked("t")+` THEN 1 ELSE 0 END),
		       SUM(CASE WHEN (`+sqlGates("t")+`) OR (`+sqlGated("t")+`) OR (`+started+` AND `+gateHeld+`) THEN 1 ELSE 0 END),
		       SUM(CASE WHEN `+sqlGatesOpen("t")+` THEN 1 ELSE 0 END)
		  FROM tasks t
		 WHERE t.status IN ('open','in_progress')
		 GROUP BY t.project_slug`, at, at, at, at)
	if err != nil {
		return nil, fmt.Errorf("store.TaskCountsByProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]TaskCounts{}
	for rows.Next() {
		var (
			slug string
			c    TaskCounts
		)
		if err := rows.Scan(&slug, &c.Ready, &c.InProgress, &c.Lapsed, &c.Unclaimed, &c.Blocked, &c.Waiting, &c.GatesOpen); err != nil {
			return nil, fmt.Errorf("store.TaskCountsByProject: scan: %w", err)
		}
		out[slug] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.TaskCountsByProject: %w", err)
	}
	return out, nil
}

// ReadyTaskProjects maps every ready task (sqlReady), plan steps included, to
// its project: ids and slugs only, no content. The console's board asks it on
// the request's handle while a project is locked, to find the ready tasks a
// locked project's work may block (OpenBlockerProjects), as the Tasks page
// does for its rows.
func ReadyTaskProjects(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.id, t.project_slug FROM tasks t WHERE `+sqlReady("t"))
	if err != nil {
		return nil, fmt.Errorf("store.ReadyTaskProjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, fmt.Errorf("store.ReadyTaskProjects: scan: %w", err)
		}
		out[id] = project
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ReadyTaskProjects: %w", err)
	}
	return out, nil
}

// queueScope narrows a queue query: one project's non-plan tasks (the default
// queue), one plan's steps in a project, or every task of every project.
type queueScope struct {
	all     bool
	project string
	plan    string
}

// where renders the scope's conditions over alias, each led by AND.
func (q queueScope) where(alias string) (string, []any) {
	switch {
	case q.all:
		return "", nil
	case q.plan == "":
		return " AND " + alias + ".project_slug = ? AND " + alias + ".plan_slug = ''", []any{q.project}
	default:
		return " AND " + alias + ".project_slug = ? AND " + alias + ".plan_slug = ?", []any{q.project, q.plan}
	}
}

// queueTasks runs one group's query over a scope, ordered by order.
func queueTasks(ctx context.Context, db *sql.DB, group, order string, scope queueScope) (*sql.Rows, error) {
	where, args := scope.where("t")
	return db.QueryContext(ctx, `SELECT `+prefixCols("t", taskCols)+` FROM tasks t
		WHERE `+group+where+`
		ORDER BY `+order, args...)
}

// oldestFirst is the queue's order: oldest-created first, ties broken by id
// (ULID-monotonic), so the queue is stable and agent-predictable.
const oldestFirst = "t.created_at ASC, t.id ASC"

// byWho is the people's queue's order: by who must act, then oldest first.
const byWho = "lower(t.gate_who) ASC, t.created_at ASC, t.id ASC"

// GatedTasks returns a project's open steps that wait on people -- their own
// gate is open, or an unfinished blocker is a gate -- each with its unfinished
// blockers, oldest first. Plan steps are excluded, as in ReadyTasks.
func GatedTasks(ctx context.Context, db *sql.DB, project string) ([]BlockedTask, error) {
	return gatedTasks(ctx, db, queueScope{project: project})
}

// GatedTasksForPlan is GatedTasks over one plan's steps.
func GatedTasksForPlan(ctx context.Context, db *sql.DB, project, plan string) ([]BlockedTask, error) {
	return gatedTasks(ctx, db, queueScope{project: project, plan: plan})
}

// AllGatedTasks is GatedTasks across every project, plan steps included. It
// backs the console Tasks page.
func AllGatedTasks(ctx context.Context, db *sql.DB) ([]BlockedTask, error) {
	return gatedTasks(ctx, db, queueScope{all: true})
}

func gatedTasks(ctx context.Context, db *sql.DB, scope queueScope) ([]BlockedTask, error) {
	rows, err := queueTasks(ctx, db, sqlGated("t"), oldestFirst, scope)
	if err != nil {
		return nil, fmt.Errorf("store.GatedTasks: %w", err)
	}
	return attachBlockers(ctx, db, rows)
}

// GateTasks returns a project's gates still open or in progress -- the
// people's queue -- each with its unfinished blockers (a gate can wait on work
// too), ordered by who must act and then oldest first. Plan steps are
// excluded, as in ReadyTasks.
func GateTasks(ctx context.Context, db *sql.DB, project string) ([]BlockedTask, error) {
	return gateTasks(ctx, db, queueScope{project: project})
}

// GateTasksForPlan is GateTasks over one plan's steps.
func GateTasksForPlan(ctx context.Context, db *sql.DB, project, plan string) ([]BlockedTask, error) {
	return gateTasks(ctx, db, queueScope{project: project, plan: plan})
}

// AllGateTasks is GateTasks across every project, plan steps included. It
// backs the console Tasks page.
func AllGateTasks(ctx context.Context, db *sql.DB) ([]BlockedTask, error) {
	return gateTasks(ctx, db, queueScope{all: true})
}

func gateTasks(ctx context.Context, db *sql.DB, scope queueScope) ([]BlockedTask, error) {
	rows, err := queueTasks(ctx, db, sqlGates("t"), byWho, scope)
	if err != nil {
		return nil, fmt.Errorf("store.GateTasks: %w", err)
	}
	return attachBlockers(ctx, db, rows)
}
