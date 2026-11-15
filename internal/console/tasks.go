package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// taskRow is a display projection of a task.
type taskRow struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Project  string     `json:"project"`
	Status   string     `json:"status"`
	Favorite bool       `json:"favorite,omitempty"`
	Deps     int        `json:"deps"`
	Account  string     `json:"account,omitempty"` // restricted account that created it ("" = owner)
	Created  time.Time  `json:"created"`
	Closed   *time.Time `json:"closed,omitempty"`
	Blockers []blocker  `json:"blockers,omitempty"`
	// LockedBlockers are the unfinished blockers this request's handle does
	// not hold because they sit in a locked project it does not read:
	// that project's slug, once per blocker, never the
	// blocker itself. nil while nothing is locked -- absent from the JSON.
	LockedBlockers []string `json:"lockedBlockers,omitempty"`
	// MayContain is the task's provenance stamps, set only where a list shows
	// them (a project page's came-from filter, camefrom.go).
	MayContain []core.Stamp `json:"mayContain,omitempty"`
	// Kind is "gate" for a gate-kind task and "" for a step; Gate is what the
	// task waits on outside Seamless, nil when nothing.
	Kind string        `json:"kind,omitempty"`
	Gate *taskGateView `json:"gate,omitempty"`
}

// taskGateView is a task's gate as the console shows it (core.TaskGate).
type taskGateView struct {
	Who       string     `json:"who"`
	What      string     `json:"what"`
	Decision  string     `json:"decision,omitempty"`
	ReadyWhen string     `json:"readyWhen,omitempty"`
	Open      bool       `json:"open"`
	ClearedAt *time.Time `json:"clearedAt,omitempty"`
	ClearedBy string     `json:"clearedBy,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// gateView projects a task's gate, nil when it has none.
func gateView(t core.Task) *taskGateView {
	if !t.Gate.Set() {
		return nil
	}
	g := t.Gate
	return &taskGateView{
		Who: g.Who, What: g.What, Decision: g.Decision, ReadyWhen: g.ReadyWhen, Open: g.Open(),
		ClearedAt: g.ClearedAt, ClearedBy: g.ClearedBy, Note: g.Note,
	}
}

// taskKind is a task's kind as the console carries it: "gate", or "" for a
// step (a step reads as every task always has).
func taskKind(t core.Task) string {
	if t.IsGate() {
		return string(core.TaskKindGate)
	}
	return ""
}

// BlockedBy counts every unfinished blocker of the row: the ones it names and
// the ones in locked projects.
func (r taskRow) BlockedBy() int { return len(r.Blockers) + len(r.LockedBlockers) }

// LockedIn is the locked projects the row's locked blockers sit in, once each.
func (r taskRow) LockedIn() []string { return slices.Compact(slices.Clone(r.LockedBlockers)) }

type blocker struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Status string        `json:"status"`
	Kind   string        `json:"kind,omitempty"`
	Gate   *taskGateView `json:"gate,omitempty"`
}

// WaitsOn is who an entry of the people's rails waits on: its own open
// gate's who, else the who of each open gate blocking it, once each.
func (r taskRow) WaitsOn() []string {
	var who []string
	if r.Gate != nil && r.Gate.Open {
		who = append(who, r.Gate.Who)
	}
	for _, b := range r.Blockers {
		if b.Kind == string(core.TaskKindGate) && b.Gate != nil && !slices.Contains(who, b.Gate.Who) {
			who = append(who, b.Gate.Who)
		}
	}
	return who
}

// tasksData is the payload for the Tasks library screen. Selected drives the
// HTML reader pane only; JSON callers get the same lean payload as before.
// Gates (gate-kind tasks, open or in progress: the people's queue) and Gated
// (steps waiting on people) are present only when they hold something.
type tasksData struct {
	Ready      []taskRow `json:"ready"`
	InProgress []taskRow `json:"inProgress"`
	Blocked    []taskRow `json:"blocked"`
	Gates      []taskRow `json:"gates,omitempty"`
	Gated      []taskRow `json:"gated,omitempty"`
	Closed     []taskRow `json:"closed"`
	ClosedMore int       `json:"closedMore"`
	// Selected is the task open in the reader: the requested one on a
	// /console/tasks/{id} page, or the most relevant one on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *taskDetail `json:"-"`
	SelectedAuto bool        `json:"-"`
}

const closedLimit = 25

// tasksPage assembles the four status buckets of the tasks list.
func (s *Service) tasksPage(ctx context.Context) (tasksData, error) {
	ready, err := store.AllReadyTasks(ctx, s.db(ctx))
	if err != nil {
		return tasksData{}, err
	}
	inProgress, err := store.AllTasksByStatus(ctx, s.db(ctx), core.TaskInProgress)
	if err != nil {
		return tasksData{}, err
	}
	blocked, err := store.AllBlockedTasks(ctx, s.db(ctx))
	if err != nil {
		return tasksData{}, err
	}
	gated, err := store.AllGatedTasks(ctx, s.db(ctx))
	if err != nil {
		return tasksData{}, err
	}
	gates, err := store.AllGateTasks(ctx, s.db(ctx))
	if err != nil {
		return tasksData{}, err
	}
	// A gate someone marked in progress stays in the people's queue, once.
	inProgress = slices.DeleteFunc(inProgress, core.Task.IsGate)
	done, err := store.AllTasksByStatus(ctx, s.db(ctx), core.TaskDone)
	if err != nil {
		return tasksData{}, err
	}
	dropped, err := store.AllTasksByStatus(ctx, s.db(ctx), core.TaskDropped)
	if err != nil {
		return tasksData{}, err
	}

	closed := append(done, dropped...)
	sort.Slice(closed, func(i, j int) bool {
		a, b := closedAt(closed[i]), closedAt(closed[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return closed[i].ID > closed[j].ID
	})
	closedMore := 0
	if len(closed) > closedLimit {
		closedMore = len(closed) - closedLimit
		closed = closed[:closedLimit]
	}

	data := tasksData{
		Ready:      taskRows(ready),
		InProgress: taskRows(inProgress),
		Blocked:    blockedRows(blocked),
		Gates:      blockedRows(gates),
		Gated:      blockedRows(gated),
		Closed:     taskRows(closed),
		ClosedMore: closedMore,
	}
	if err := s.markLockedBlockers(ctx, &data); err != nil {
		return tasksData{}, err
	}
	// The agent-facing ready/blocked queries are deliberately oldest-first for
	// fair claiming. The console is a review surface: within each open-state rail
	// group, match the timestamp shown on the row and present newest-created first.
	// The people's queue keeps its order: by who must act.
	for _, bucket := range [][]taskRow{data.Ready, data.InProgress, data.Blocked, data.Gated} {
		sortTaskRowsNewest(bucket)
	}
	return data, nil
}

// markLockedBlockers tells the owner which open tasks a locked project
// blocks. The request's handle hides a locked project it
// does not read -- its tasks and their dependency edges both -- so a task one
// of them blocks reads as ready on it, and a blocked one as blocked by fewer.
// This asks the main handle for those blockers' projects alone, moves such a
// ready task to the blocked rail, and records each blocker by its locked
// project (LockedBlockers): the console is the owner's surface, which lists
// locked projects by name, and their content stays out. Nothing runs while
// nothing is locked.
func (s *Service) markLockedBlockers(ctx context.Context, data *tasksData) error {
	locked := lockedProjects(ctx)
	if len(locked) == 0 {
		return nil
	}
	ids := make([]string, 0, len(data.Ready)+len(data.Blocked)+len(data.Gated)+len(data.Gates))
	for _, bucket := range [][]taskRow{data.Ready, data.Blocked, data.Gated, data.Gates} {
		for _, row := range bucket {
			ids = append(ids, row.ID)
		}
	}
	by, err := store.OpenBlockerProjects(ctx, s.writeDB(), ids, locked)
	if err != nil || len(by) == 0 {
		return err
	}
	for _, bucket := range [][]taskRow{data.Blocked, data.Gated, data.Gates} {
		for i := range bucket {
			bucket[i].LockedBlockers = by[bucket[i].ID]
		}
	}
	ready := data.Ready[:0]
	for _, row := range data.Ready {
		if projects := by[row.ID]; len(projects) > 0 {
			row.LockedBlockers = projects
			data.Blocked = append(data.Blocked, row)
			continue
		}
		ready = append(ready, row)
	}
	data.Ready = ready
	return nil
}

func (s *Service) tasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := s.tasksPage(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the most relevant task in the reader.
	if !wantsJSON(r) {
		if id := defaultTaskID(data); id != "" {
			d, found, derr := s.taskDetailByID(ctx, id)
			if derr != nil {
				s.serverError(w, r, derr)
				return
			}
			if found {
				data.Selected = &d
				data.SelectedAuto = true
			}
		}
	}
	s.render(w, r, "tasks", pageData{Title: "Tasks", Active: "tasks", Data: data})
}

// defaultTaskID picks the reader's default selection on the list URL: the work
// happening now first, then the ready queue, then blocked, then the work
// waiting on people, then the newest closed ("" when there are no tasks at
// all).
func defaultTaskID(data tasksData) string {
	for _, bucket := range [][]taskRow{data.InProgress, data.Ready, data.Blocked, data.Gates, data.Gated, data.Closed} {
		if len(bucket) > 0 {
			return bucket[0].ID
		}
	}
	return ""
}

// taskRelease is the owner override for a claimed task: it force-releases the
// lock (reopening the task for any agent to claim) regardless of who holds it.
// It is reachable only from this owner surface (the console button and the
// bearer-authenticated `seam task release --force`), never the agent MCP tools.
func (s *Service) taskRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	// Read through the console's (possibly filtered) handle first: the release
	// itself runs on the write handle, which sees every project (see writeDB).
	if s.cfg.WriteDB != nil {
		if _, err := store.TaskByID(ctx, s.db(ctx), id); err != nil {
			if errors.Is(err, store.ErrTaskNotFound) {
				s.notFound(w, r, "No task with id "+id+".")
				return
			}
			s.serverError(w, r, err)
			return
		}
	}
	released, err := store.ForceReleaseTask(ctx, s.writeDB(), id, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			s.notFound(w, r, "No task with id "+id+".")
			return
		}
		// Not in progress (nothing to release) or a store error: surface it
		// rather than pretend success.
		s.serverError(w, r, err)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind: core.EventTaskTransition, ProjectSlug: released.ProjectSlug, ItemID: released.ID,
			Payload: map[string]any{"to": string(released.Status), "released": true, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record task release event", "error", err)
		}
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, taskDetailJSON(released))
		return
	}
	http.Redirect(w, r, releaseNext(r.PostFormValue("next"), released.ID), http.StatusSeeOther)
}

// releaseNext resolves where a force-release returns to. The tasks library is
// the default, but the release button also lives on surfaces that own their own
// view -- the project workspace's plan timeline, which posts its own URL -- and
// redirecting those to the library would morph the workspace into it. safeNext
// refuses anything off-console; a refused next falls back to the task rather
// than dumping the owner on the console root.
func releaseNext(raw, taskID string) string {
	dest := "/console/tasks/" + taskID
	if raw = strings.TrimSpace(raw); raw != "" {
		if next := safeNext(raw); next != "/console/" {
			dest = next
		}
	}
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	return dest + sep + "notice=" + url.QueryEscape("Released the claim.")
}

// taskDetailJSON is the minimal task view returned to a CLI/JSON caller of
// taskRelease, so `seam task release --force` can print the reopened task.
func taskDetailJSON(t core.Task) map[string]any {
	return map[string]any{
		"id": t.ID, "title": t.Title, "project": t.ProjectSlug, "status": string(t.Status),
	}
}

// closedAt returns the rail timestamp for a terminal task. Legacy rows without
// closed_at fall back to their final update rather than sorting at time zero.
func closedAt(t core.Task) time.Time {
	if t.ClosedAt != nil {
		return *t.ClosedAt
	}
	return t.UpdatedAt
}

// sortTaskRowsNewest orders an open-state rail group by the creation timestamp
// displayed on each row. ID is the deterministic newest-first tie-breaker.
func sortTaskRowsNewest(rows []taskRow) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Created.Equal(rows[j].Created) {
			return rows[i].Created.After(rows[j].Created)
		}
		return rows[i].ID > rows[j].ID
	})
}

func taskRows(tasks []core.Task) []taskRow {
	out := make([]taskRow, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskRow{
			ID: t.ID, Title: t.Title, Project: t.ProjectSlug, Status: string(t.Status),
			Favorite: t.Favorite, Deps: len(t.DependsOn), Account: t.Account, Created: t.CreatedAt, Closed: t.ClosedAt,
			Kind: taskKind(t), Gate: gateView(t),
		})
	}
	return out
}

func blockedRows(blocked []store.BlockedTask) []taskRow {
	out := make([]taskRow, 0, len(blocked))
	for _, b := range blocked {
		row := taskRow{
			ID: b.Task.ID, Title: b.Task.Title, Project: b.Task.ProjectSlug,
			Status: string(b.Task.Status), Favorite: b.Task.Favorite,
			Deps: len(b.Task.DependsOn), Account: b.Task.Account, Created: b.Task.CreatedAt,
			Kind: taskKind(b.Task), Gate: gateView(b.Task),
		}
		for _, bl := range b.Blockers {
			row.Blockers = append(row.Blockers, blocker{ID: bl.ID, Title: bl.Title, Status: string(bl.Status),
				Kind: taskKind(bl), Gate: gateView(bl)})
		}
		out = append(out, row)
	}
	return out
}
