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
	// ClaimedBy is the session id the claim names ("" for none), ClaimLive
	// whether that claim is live as of the read (core.Task.ClaimLive, the
	// only authority on held), and Holder the session it resolved to, set
	// where a list names holders (the Tasks rail's in progress group).
	ClaimedBy string       `json:"claimedBy,omitempty"`
	ClaimLive bool         `json:"claimLive,omitempty"`
	Holder    *taskSession `json:"holder,omitempty"`
	// dependsOn is the task's own dependencies, for a list that reads their
	// blockers after the fact (readRowBlockers); blockersRead says Blockers
	// is the row's whole set of unfinished blockers.
	dependsOn    []string
	blockersRead bool
}

// Word is the row's state in the owner's words (taskwords.go).
func (r taskRow) Word() taskWord {
	return wordFor(taskFacts{
		Status: r.Status, Gate: r.Kind == string(core.TaskKindGate), WaitsOn: r.WaitsOn(),
		Blocked: r.BlockedBy(), Holder: r.ClaimedBy != "", Live: r.ClaimLive,
	})
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
	namer := s.sessionNamer(ctx)
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
		InProgress: heldRows(taskRows(inProgress), namer),
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
// claim (reopening the task for any agent to claim) regardless of who holds it,
// and whether it holds one at all: a claimless start or a lapsed lease is
// released the same way. It is reachable only from this owner surface (the
// console button and the bearer-authenticated `seam task release --force`),
// never the agent MCP tools. A task no longer in progress has nothing to
// release: the owner is told so, a JSON caller gets a 409, never a 500.
func (s *Service) taskRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.taskForAction(w, r)
	if !ok {
		return
	}
	next := r.PostFormValue("next")
	if t.Status != core.TaskInProgress {
		s.taskNothingToRelease(w, r, t.ID, next)
		return
	}
	released, err := store.ForceReleaseTask(ctx, s.writeDB(), t.ID, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTaskNotFound):
			s.notFound(w, r, "No task with id "+t.ID+".")
		case errors.Is(err, store.ErrTaskClaimConflict):
			// It left in progress between the read and the release.
			s.taskNothingToRelease(w, r, t.ID, next)
		default:
			s.serverError(w, r, err)
		}
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
	http.Redirect(w, r, releaseNext(next, released.ID), http.StatusSeeOther)
}

// taskNothingToRelease answers a release of a task that is not in progress:
// the owner's page says so, a JSON caller reads it as a 409.
func (s *Service) taskNothingToRelease(w http.ResponseWriter, r *http.Request, id, next string) {
	s.taskAnswer(w, r, id, next, http.StatusConflict, "notice",
		"Nothing to release: the task is not in progress.")
}

// releaseNext resolves where a force-release returns to (taskNext).
func releaseNext(raw, taskID string) string {
	return taskNext(raw, taskID, "notice", "Released the claim.")
}

// taskNext resolves where an owner's task action returns to, carrying its
// one-shot flash (param is "notice" or "error"). The task's page is the
// default, but the actions also live on surfaces that own their own view --
// the project workspace's step dossier, which posts its own URL -- and
// redirecting those to the library would morph the workspace into it.
// safeNext refuses anything off-console; a refused next falls back to the
// task rather than dumping the owner on the console root. A flash the next
// already carried is dropped, and a fragment stays last.
func taskNext(raw, taskID, param, msg string) string {
	dest := "/console/tasks/" + taskID
	if raw = strings.TrimSpace(raw); raw != "" {
		if next := safeNext(raw); next != "/console/" {
			dest = next
		}
	}
	dest, frag, hasFrag := strings.Cut(dest, "#")
	path, query, _ := strings.Cut(dest, "?")
	var kept []string
	for _, part := range strings.Split(query, "&") {
		if part == "" || strings.HasPrefix(part, "notice=") || strings.HasPrefix(part, "error=") {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, param+"="+url.QueryEscape(msg))
	out := path + "?" + strings.Join(kept, "&")
	if hasFrag {
		out += "#" + frag
	}
	return out
}

// The owner's task actions (plan:console-ux-pass UX 06): mark a task done,
// drop it, reopen it, and mark its gate as handled, beside the release above.
// Each goes through the store call tasks_update makes (store.UpdateTaskDetailed),
// so the holder rule, the unblocking of dependents and the plan settlement are
// the MCP path's own, and records the task.transition event it records, with
// "by": "console" and no session.

// gateClearedByOwner is who a gate the console clears was cleared by
// (store.TaskGateClear: "owner" from an owner surface).
const gateClearedByOwner = "owner"

// taskStatusMoves are the statuses POST /console/tasks/{id}/status moves a
// task to, with the notice each one leaves.
var taskStatusMoves = map[core.TaskStatus]string{
	core.TaskDone:    "Marked done.",
	core.TaskDropped: "Dropped.",
	core.TaskOpen:    "Reopened.",
}

// taskStatus moves a task to done, dropped or open (the form's to). It
// refuses to close a task a live claim holds -- the owner releases the claim
// first, which tells its holder -- and a reopen of a task in progress, which
// the release is. A move to the status the task already has writes nothing.
func (s *Service) taskStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.taskForAction(w, r)
	if !ok {
		return
	}
	next := r.PostFormValue("next")
	to := core.TaskStatus(strings.TrimSpace(r.PostFormValue("to")))
	notice, valid := taskStatusMoves[to]
	if !valid {
		s.formError(w, r, http.StatusBadRequest, "to must be done, dropped or open")
		return
	}
	switch {
	case t.Status == to:
		s.taskAnswer(w, r, t.ID, next, http.StatusOK, "notice", "The task is already "+string(to)+".")
		return
	case to == core.TaskOpen && !t.Status.Closed():
		s.taskAnswer(w, r, t.ID, next, http.StatusConflict, "error",
			"The task is in progress, not closed: Release claim reopens it.")
		return
	}
	if s.refusedHeld(w, r, t, next) {
		return
	}
	res, err := store.UpdateTaskDetailed(ctx, s.writeDB(), t.ID, store.TaskPatch{Status: &to}, "", time.Now().UTC())
	if err != nil {
		s.taskActionFailed(w, r, t.ID, next, err)
		return
	}
	s.recordTaskAction(ctx, res, nil)
	s.taskActionDone(w, r, res.Task, next, notice)
}

// taskGateClear marks a task's open gate as handled, with the owner's
// optional one-line note: a step's gate clears and the step goes ahead once
// nothing else blocks it; a gate clears and closes as done, which unblocks
// the tasks that depend on it. The clearance names the owner.
func (s *Service) taskGateClear(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.taskForAction(w, r)
	if !ok {
		return
	}
	next := r.PostFormValue("next")
	switch {
	case !t.Gate.Set():
		s.taskAnswer(w, r, t.ID, next, http.StatusConflict, "error", "The task has no gate to mark as handled.")
		return
	case !t.Gate.Open():
		s.taskAnswer(w, r, t.ID, next, http.StatusOK, "notice", "The gate is already handled.")
		return
	}
	if s.refusedHeld(w, r, t, next) {
		return
	}
	// One line: a note typed across lines, or padded, reads as one.
	note := strings.Join(strings.Fields(r.PostFormValue("note")), " ")
	res, err := store.UpdateTaskDetailed(ctx, s.writeDB(), t.ID, store.TaskPatch{
		ClearGate: &store.TaskGateClear{By: gateClearedByOwner, Note: note},
	}, "", time.Now().UTC())
	if err != nil {
		s.taskActionFailed(w, r, t.ID, next, err)
		return
	}
	s.recordTaskAction(ctx, res, map[string]any{"gate": "cleared"})
	msg := "Marked as handled: the step's gate is cleared."
	if res.Task.IsGate() {
		msg = "Marked as handled: the gate is done."
	}
	s.taskActionDone(w, r, res.Task, next, msg)
}

// taskActions are the owner's actions a task offers, as of the read: the
// buttons the reader and the peek render (task_shared.html's task-actions),
// each the route above that would take it.
type taskActions struct {
	// Release: the task is in progress, so there is a claim to take, live or
	// lapsed, or none at all (a claimless start). Offered holder or not.
	Release bool
	// Held: a live claim holds the task, so nothing closes it until the claim
	// is released; the page names the holder (taskDetail.Holder).
	Held bool
	// Done and Drop close an open or in-progress task no live claim holds.
	// A gate whose gate is still open closes by being marked as handled, so it
	// offers no Done.
	Done, Drop bool
	// Reopen: a done or dropped task.
	Reopen bool
	// Handle: the task's own gate is open and no live claim holds the task:
	// Mark as handled, with a note.
	Handle bool
}

// actionsFor is the set of actions t offers; live is core.Task.ClaimLive as
// of the read.
func actionsFor(t core.Task, live bool) taskActions {
	a := taskActions{Release: t.Status == core.TaskInProgress, Held: live}
	switch {
	case t.Status.Closed():
		a.Reopen = true
	case live:
	default:
		a.Handle = t.Gate.Open()
		a.Done = !(t.IsGate() && a.Handle)
		a.Drop = true
	}
	return a
}

// taskForAction reads the task an owner's action names through the request's
// handle, which hides a locked project's tasks: such a task, like one that
// does not exist, answers not found with no title. The action itself runs on
// the write handle (writeDB), which sees every project.
func (s *Service) taskForAction(w http.ResponseWriter, r *http.Request) (core.Task, bool) {
	id := r.PathValue("id")
	t, err := store.TaskByID(r.Context(), s.db(r.Context()), id)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			s.notFound(w, r, "No task with id "+id+".")
			return core.Task{}, false
		}
		s.serverError(w, r, err)
		return core.Task{}, false
	}
	return t, true
}

// refusedHeld refuses, and reports that it did, an action on a task a live
// claim holds: the console never closes a task over its holder's head (the
// holder rule UpdateTask keeps for every caller but the holder). The owner
// releases the claim first, which tells the holder.
func (s *Service) refusedHeld(w http.ResponseWriter, r *http.Request, t core.Task, next string) bool {
	if !t.ClaimLive(time.Now().UTC()) {
		return false
	}
	s.taskAnswer(w, r, t.ID, next, http.StatusConflict, "error", s.heldLine(r.Context(), t))
	return true
}

// heldLine says who holds a task's live claim and what the owner does first.
func (s *Service) heldLine(ctx context.Context, t core.Task) string {
	who := "An agent"
	if h := holderLink(t.ClaimedBy, s.sessionNamer(ctx)); h != nil {
		who = h.Name
	}
	return who + " holds a live claim on this task: release the claim first, then close it."
}

// taskActionFailed answers a store refusal of an owner's task action: a claim
// taken between the read and the write is the holder's line, a gate change
// the store refuses says why, and anything else is a server error.
func (s *Service) taskActionFailed(w http.ResponseWriter, r *http.Request, id, next string, err error) {
	ctx := r.Context()
	switch {
	case errors.Is(err, store.ErrTaskNotFound):
		s.notFound(w, r, "No task with id "+id+".")
	case errors.Is(err, store.ErrTaskClaimConflict):
		line := "An agent claimed this task a moment ago: release the claim first, then close it."
		if t, rerr := store.TaskByID(ctx, s.db(ctx), id); rerr == nil && t.ClaimLive(time.Now().UTC()) {
			line = s.heldLine(ctx, t)
		}
		s.taskAnswer(w, r, id, next, http.StatusConflict, "error", line)
	case errors.Is(err, store.ErrTaskGate):
		why := err.Error()
		if _, after, found := strings.Cut(why, store.ErrTaskGate.Error()+": "); found {
			why = after
		}
		s.taskAnswer(w, r, id, next, http.StatusBadRequest, "error", "The gate was not marked as handled: "+why+".")
	default:
		s.serverError(w, r, err)
	}
}

// recordTaskAction records an owner's task action as the task.transition
// event tasks_update records -- the status before and after, plus extra --
// with "by": "console" and no session: a console action attributes no
// session to a plan (plan-token-attribution-contract). The recorder mints
// plan.shipped from it when it closes a plan's last step, as it does for the
// MCP path's event.
func (s *Service) recordTaskAction(ctx context.Context, res store.TaskUpdateResult, extra map[string]any) {
	if s.cfg.Events == nil {
		return
	}
	payload := map[string]any{"to": string(res.Task.Status), "from": string(res.From), "by": "console"}
	for k, v := range extra {
		payload[k] = v
	}
	if _, err := s.eventLog(ctx).Record(ctx, core.Event{
		Kind: core.EventTaskTransition, ProjectSlug: res.Task.ProjectSlug, ItemID: res.Task.ID, Payload: payload,
	}); err != nil {
		s.logger.Warn("console: record task action event", "task", res.Task.ID, "error", err)
	}
}

// taskActionDone answers a landed action: the task as JSON, or the page the
// owner acted from with the action's notice.
func (s *Service) taskActionDone(w http.ResponseWriter, r *http.Request, t core.Task, next, notice string) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, taskDetailJSON(t))
		return
	}
	http.Redirect(w, r, taskNext(next, t.ID, "notice", notice), http.StatusSeeOther)
}

// taskAnswer answers an action that changed nothing: a JSON caller gets msg
// with status (an error unless status is 200), a browser the page it acted
// from with msg as its flash.
func (s *Service) taskAnswer(w http.ResponseWriter, r *http.Request, id, next string, status int, param, msg string) {
	if wantsJSON(r) {
		if status == http.StatusOK {
			writeJSON(w, status, map[string]string{"id": id, "notice": msg})
			return
		}
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	http.Redirect(w, r, taskNext(next, id, param, msg), http.StatusSeeOther)
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

// taskRows projects tasks into rows. A row knows its blockers only when its
// list read them: an open row with dependencies reads them through
// readRowBlockers, or a list of ready tasks has none to read.
func taskRows(tasks []core.Task) []taskRow {
	now := time.Now().UTC()
	out := make([]taskRow, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, taskRow{
			ID: t.ID, Title: t.Title, Project: t.ProjectSlug, Status: string(t.Status),
			Favorite: t.Favorite, Deps: len(t.DependsOn), Account: t.Account, Created: t.CreatedAt, Closed: t.ClosedAt,
			Kind: taskKind(t), Gate: gateView(t),
			ClaimedBy: t.ClaimedBy, ClaimLive: t.ClaimLive(now),
			dependsOn: t.DependsOn, blockersRead: len(t.DependsOn) == 0,
		})
	}
	return out
}

// heldRows names each row's holder, the session its claim names.
func heldRows(rows []taskRow, namer func(string) core.Session) []taskRow {
	for i := range rows {
		rows[i].Holder = holderLink(rows[i].ClaimedBy, namer)
	}
	return rows
}

// readRowBlockers reads the unfinished blockers of the open rows a list
// projected without them (taskRows), in one query through the request's
// handle, and the ones in locked projects by project only
// (markLockedBlockers' rule), so each row's word tells ready from blocked.
func (s *Service) readRowBlockers(ctx context.Context, rows []taskRow) error {
	var ids, open []string
	for _, r := range rows {
		if r.blockersRead || core.TaskStatus(r.Status).Closed() {
			continue
		}
		open = append(open, r.ID)
		for _, id := range r.dependsOn {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(open) == 0 {
		return nil
	}
	deps, err := store.TasksByIDs(ctx, s.db(ctx), ids)
	if err != nil {
		return err
	}
	var lockedBy map[string][]string
	if locked := lockedProjects(ctx); len(locked) > 0 {
		if lockedBy, err = store.OpenBlockerProjects(ctx, s.writeDB(), open, locked); err != nil {
			return err
		}
	}
	for i := range rows {
		r := &rows[i]
		if r.blockersRead || core.TaskStatus(r.Status).Closed() {
			continue
		}
		for _, id := range r.dependsOn {
			if d, ok := deps[id]; ok && !d.Status.Closed() {
				r.Blockers = append(r.Blockers, blocker{ID: d.ID, Title: d.Title, Status: string(d.Status),
					Kind: taskKind(d), Gate: gateView(d)})
			}
		}
		r.LockedBlockers = lockedBy[r.ID]
		r.blockersRead = true
	}
	return nil
}

func blockedRows(blocked []store.BlockedTask) []taskRow {
	out := make([]taskRow, 0, len(blocked))
	for _, b := range blocked {
		row := taskRow{
			ID: b.Task.ID, Title: b.Task.Title, Project: b.Task.ProjectSlug,
			Status: string(b.Task.Status), Favorite: b.Task.Favorite,
			Deps: len(b.Task.DependsOn), Account: b.Task.Account, Created: b.Task.CreatedAt,
			Kind: taskKind(b.Task), Gate: gateView(b.Task),
			ClaimedBy: b.Task.ClaimedBy, ClaimLive: b.Task.ClaimLive(time.Now().UTC()),
			dependsOn: b.Task.DependsOn, blockersRead: true,
		}
		for _, bl := range b.Blockers {
			row.Blockers = append(row.Blockers, blocker{ID: bl.ID, Title: bl.Title, Status: string(bl.Status),
				Kind: taskKind(bl), Gate: gateView(bl)})
		}
		out = append(out, row)
	}
	return out
}
