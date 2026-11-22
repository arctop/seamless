package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
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
	// Plan is the plan the task is a step of, "" for a task outside a plan:
	// the rail's plan chip.
	Plan string `json:"plan,omitempty"`
	// Updated is the task's last change. Untouched marks a ready task nothing
	// changed on for untouchedAfter or more (the Tasks page sets it).
	Updated   time.Time `json:"updated"`
	Untouched bool      `json:"untouched,omitempty"`
	// Lease says where a named claim's lease stands as of the read
	// (claimLease: "lease live, 12m left", "lapsed 3m ago"); LeaseExpiry is
	// the instant it names.
	Lease       string     `json:"lease,omitempty"`
	LeaseExpiry *time.Time `json:"leaseExpiry,omitempty"`
	// Since is when the task entered the state its word names (rowSince),
	// set where a list shows the time in state (the Tasks page).
	Since *time.Time `json:"since,omitempty"`
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

// LeaseLapsed reports a named claim whose lease ran out: the rail shows its
// lease line in the lapsed tone.
func (r taskRow) LeaseLapsed() bool { return r.Lease != "" && !r.ClaimLive }

// FirstBlocker is the row's oldest unfinished blocker it names, nil for none.
func (r taskRow) FirstBlocker() *blocker {
	if len(r.Blockers) == 0 {
		return nil
	}
	return &r.Blockers[0]
}

// InState is the row's time in its state as the rail shows it ("ready 12d",
// "in progress 2h"), a <time> whose title is the instant; "" when the list
// read no Since.
func (r taskRow) InState() template.HTML {
	if r.Since == nil || r.Since.IsZero() {
		return ""
	}
	word, ok := stateWords[r.Word().Key]
	if !ok {
		word = r.Word().Text
	}
	return timeAs(*r.Since, word+" "+agoAt(*r.Since, time.Now()))
}

// stateWords name each word's state for the time in it: a claim that lapsed
// or was never taken is a task started, not one in progress.
var stateWords = map[string]string{
	wordReady: "ready", wordInProgress: "in progress", wordLapsed: "started", wordUnclaimed: "started",
	wordBlocked: "blocked", wordWaiting: "waiting", wordDone: "done", wordDropped: "dropped",
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

// tasksData is the payload for the Tasks library screen. Selected and View
// drive the HTML page only.
//
// The rails are the queue's groups (store/tasks_gates.go) under the page's
// filters: Ready, InProgress (every step in progress: held by a live claim,
// lapsed, or started with no claim), Blocked, Gates (gate-kind tasks, open
// or in progress: the people's queue) and Gated (steps waiting on people),
// the last two present only when they hold something; then Closed, one page
// of the done and dropped tasks, newest first, with ClosedMore the closed
// tasks this page does not show and ClosedTotal all of them. Counts counts
// them in the board's words (taskCounts).
type tasksData struct {
	Ready      []taskRow `json:"ready"`
	InProgress []taskRow `json:"inProgress"`
	Blocked    []taskRow `json:"blocked"`
	Gates      []taskRow `json:"gates,omitempty"`
	Gated      []taskRow `json:"gated,omitempty"`
	Closed     []taskRow `json:"closed"`
	ClosedMore int       `json:"closedMore"`
	// ClosedTotal is every closed task under the filters; ClosedPage and
	// ClosedPages say which page of them Closed is (closed=all).
	ClosedTotal int `json:"closedTotal"`
	ClosedPage  int `json:"closedPage"`
	ClosedPages int `json:"closedPages"`
	// Counts is the header's tally; Projects the projects with unfinished
	// tasks under the other filters, for the project menu.
	Counts   taskCounts         `json:"counts"`
	Projects []taskProjectCount `json:"projects"`
	// Filter is the state the page was read with, as its URL names it.
	Filter taskFilterJSON `json:"filter"`
	// Selected is the task open in the reader: the requested one on a
	// /console/tasks/{id} page, or the most relevant one on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *taskDetail `json:"-"`
	SelectedAuto bool        `json:"-"`
	// View is the page's controls (taskView). HTML only.
	View *taskView `json:"-"`
}

// listsAny reports a row in any rail.
func (d tasksData) listsAny() bool {
	return len(d.Ready)+len(d.InProgress)+len(d.Blocked)+len(d.Gates)+len(d.Gated)+len(d.Closed) > 0
}

// Live is the in progress rows a live claim holds: the rail's in progress
// group.
func (d tasksData) Live() []taskRow {
	return slices.DeleteFunc(slices.Clone(d.InProgress), func(r taskRow) bool { return !r.ClaimLive })
}

// Unheld is the in progress rows no live claim holds (a lapsed lease, a
// start with no claim): any agent may claim them, so they are not in
// progress in the owner's words.
func (d tasksData) Unheld() []taskRow {
	return slices.DeleteFunc(slices.Clone(d.InProgress), func(r taskRow) bool { return r.ClaimLive })
}

// taskCounts is the Tasks header's tally, in the words the project board
// counts with (store.TaskCounts): InProgress is a live claim's alone, Lapsed
// and Unclaimed the steps in progress that none holds, Waiting the gates and
// the steps an open gate holds. Open is every unfinished task the rails show.
type taskCounts struct {
	Open       int `json:"open"`
	InProgress int `json:"inProgress"`
	Lapsed     int `json:"lapsed"`
	Unclaimed  int `json:"unclaimed"`
	Ready      int `json:"ready"`
	Blocked    int `json:"blocked"`
	Waiting    int `json:"waiting"`
}

// taskProjectCount is one entry of the project menu: a project ("" for the
// global scope) and its unfinished tasks under the other filters.
type taskProjectCount struct {
	Project string `json:"project"`
	Open    int    `json:"open"`
}

// taskFilterJSON is the page's state in its URL's words.
type taskFilterJSON struct {
	Project string `json:"project,omitempty"`
	Plan    string `json:"plan,omitempty"`
	Steps   bool   `json:"steps,omitempty"`
	Q       string `json:"q,omitempty"`
	Closed  string `json:"closed,omitempty"`
	Page    int    `json:"page,omitempty"`
	Sort    string `json:"sort"`
}

const closedLimit = 25

// untouchedAfter is how long a ready task sits with nothing changed on it
// before its rail row says so.
const untouchedAfter = 30 * 24 * time.Hour

// taskQuery is the Tasks page's state, read from its URL (parseTaskQuery).
// Every part is strict: a value the page cannot read is a 400 naming what it
// takes, never a quiet default.
type taskQuery struct {
	// Project is "" for every project, validate.GlobalProject for the global
	// scope, else a project's slug (?project=).
	Project string
	// Plan narrows to one plan's steps (?plan=). Steps includes the plan
	// steps beside the tasks outside a plan (?steps=1); the default list
	// leaves them out, which is what tasks_ready means for agents.
	Plan  string
	Steps bool
	// Q is the text filter, trimmed (?q=): title, body, or id, a pasted
	// short id included.
	Q string
	// ClosedAll pages through every closed task (?closed=all), Page (1-based,
	// ?page=) at a time; without it the rail shows the newest closedLimit.
	ClosedAll bool
	Page      int
	// Sort orders the open groups: taskSortOldest, the default, is the order
	// agents claim in; taskSortNewest the newest-created first.
	Sort string
}

// The Tasks page's sort keys.
const (
	taskSortOldest = "oldest"
	taskSortNewest = "newest"
)

var taskSortKeys = []string{taskSortOldest, taskSortNewest}

// parseTaskQuery reads the Tasks page's state from its URL. Parameters it
// does not know (format, reader, a flash) are left to their readers.
func parseTaskQuery(v url.Values) (taskQuery, error) {
	q := taskQuery{Q: strings.TrimSpace(v.Get("q")), Sort: taskSortOldest, Page: 1}
	if raw := strings.TrimSpace(v.Get("project")); raw != "" {
		slug, err := validate.Project(raw)
		if err != nil {
			return q, fmt.Errorf("invalid project %q: name a project's slug, or global for the global scope", raw)
		}
		q.Project = slug
		if slug == "" {
			q.Project = validate.GlobalProject
		}
	}
	if raw := strings.TrimSpace(v.Get("plan")); raw != "" {
		// A plan copies as plan:<slug>; pasted whole, it names the same plan.
		slug := strings.TrimPrefix(raw, "plan:")
		if validate.Name(slug) != nil {
			return q, fmt.Errorf("invalid plan %q: name a plan's slug (plan:<slug> works too)", raw)
		}
		q.Plan = slug
	}
	switch steps := v.Get("steps"); steps {
	case "":
	case "1":
		q.Steps = true
	default:
		return q, fmt.Errorf("invalid steps %q: valid values are 1 (or omit)", steps)
	}
	switch closed := v.Get("closed"); closed {
	case "":
	case "all":
		q.ClosedAll = true
	default:
		return q, fmt.Errorf("invalid closed %q: valid values are all (or omit)", closed)
	}
	if raw := v.Get("page"); raw != "" {
		if !q.ClosedAll {
			return q, fmt.Errorf("invalid page %q: page pages through the closed tasks, with closed=all", raw)
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return q, fmt.Errorf("invalid page %q: a page number from 1", raw)
		}
		q.Page = n
	}
	if sortKey := v.Get("sort"); sortKey != "" {
		if !slices.Contains(taskSortKeys, sortKey) {
			return q, fmt.Errorf("invalid sort %q: valid values are %s", sortKey, strings.Join(taskSortKeys, ", "))
		}
		q.Sort = sortKey
	}
	return q, nil
}

// values is the state's parts that are not their defaults.
func (q taskQuery) values() url.Values {
	v := url.Values{}
	if q.Project != "" {
		v.Set("project", q.Project)
	}
	if q.Plan != "" {
		v.Set("plan", q.Plan)
	}
	if q.Steps {
		v.Set("steps", "1")
	}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.ClosedAll {
		v.Set("closed", "all")
		if q.Page > 1 {
			v.Set("page", strconv.Itoa(q.Page))
		}
	}
	if q.Sort != taskSortOldest {
		v.Set("sort", q.Sort)
	}
	return v
}

// QS is the state as a ?query suffix, "" when every part is its default.
func (q taskQuery) QS() string { return encodeQS(q.values()) }

// href is path with the state's query.
func (q taskQuery) href(path string) string { return path + q.QS() }

// narrowed reports a filter that keeps some tasks out: a project, a plan or
// text. The steps toggle only lets more in.
func (q taskQuery) narrowed() bool { return q.Project != "" || q.Plan != "" || q.Q != "" }

// filter is the state as the store's filter.
func (q taskQuery) filter() store.TaskFilter {
	f := store.TaskFilter{Plan: q.Plan, Steps: q.Steps, Query: q.Q}
	switch q.Project {
	case "":
	case validate.GlobalProject:
		f.ByProject = true
	default:
		f.ByProject, f.Project = true, q.Project
	}
	return f
}

// json is the state in its URL's words, for the JSON model.
func (q taskQuery) json() taskFilterJSON {
	out := taskFilterJSON{Project: q.Project, Plan: q.Plan, Steps: q.Steps, Q: q.Q, Sort: q.Sort}
	if q.ClosedAll {
		out.Closed, out.Page = "all", q.Page
	}
	return out
}

// taskQueryOf reads the request's Tasks state and checks the project and
// the plan it names against what the request reads: one that names nothing
// here -- that does not exist, or sits in a locked project, whose tasks stay
// absent -- is a 400, never an empty list that reads as "no tasks". ok=false
// means the answer was written.
func (s *Service) taskQueryOf(w http.ResponseWriter, r *http.Request) (taskQuery, bool) {
	ctx := r.Context()
	q, err := parseTaskQuery(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return taskQuery{}, false
	}
	db := s.db(ctx)
	if q.Project != "" && q.Project != validate.GlobalProject {
		_, found, err := store.ProjectBySlug(ctx, db, q.Project)
		if err == nil && !found {
			found, err = store.TaskProjectExists(ctx, db, q.Project)
		}
		switch {
		case err != nil:
			s.serverError(w, r, err)
			return taskQuery{}, false
		case !found && lockedHere(ctx, q.Project):
			s.badRequest(w, r, fmt.Sprintf("invalid project %q: it is locked, and its tasks stay out of this view until this browser unlocks it", q.Project))
			return taskQuery{}, false
		case !found:
			s.badRequest(w, r, fmt.Sprintf("invalid project %q: no project has that slug", q.Project))
			return taskQuery{}, false
		}
	}
	if q.Plan != "" {
		found, err := store.TaskPlanExists(ctx, db, q.Plan)
		if err != nil {
			s.serverError(w, r, err)
			return taskQuery{}, false
		}
		if !found {
			s.badRequest(w, r, fmt.Sprintf("invalid plan %q: no task here is a step of that plan", q.Plan))
			return taskQuery{}, false
		}
	}
	return q, true
}

// closedPageError is tasksPage's answer to a closed=all page past the last:
// the page asked for and how many there are.
type closedPageError struct{ page, pages int }

func (e *closedPageError) Error() string {
	return fmt.Sprintf("no page %d of the closed tasks: they fill %d", e.page, e.pages)
}

// tasksPage assembles the Tasks page's rails under q, each through the
// request's handle (s.db), so a locked project's tasks are absent from the
// reads themselves.
func (s *Service) tasksPage(ctx context.Context, q taskQuery) (tasksData, error) {
	db, f := s.db(ctx), q.filter()
	ready, err := store.FilteredReadyTasks(ctx, db, f)
	if err != nil {
		return tasksData{}, err
	}
	started, err := store.FilteredStartedTasks(ctx, db, f)
	if err != nil {
		return tasksData{}, err
	}
	blocked, err := store.FilteredBlockedTasks(ctx, db, f)
	if err != nil {
		return tasksData{}, err
	}
	gated, err := store.FilteredGatedTasks(ctx, db, f)
	if err != nil {
		return tasksData{}, err
	}
	gates, err := store.FilteredGateTasks(ctx, db, f)
	if err != nil {
		return tasksData{}, err
	}
	page := 1
	if q.ClosedAll {
		page = q.Page
	}
	closed, total, err := store.FilteredClosedTasks(ctx, db, f, closedLimit, (page-1)*closedLimit)
	if err != nil {
		return tasksData{}, err
	}
	pages := max(1, (total+closedLimit-1)/closedLimit)
	if page > pages {
		return tasksData{}, &closedPageError{page: page, pages: pages}
	}
	namer := s.sessionNamer(ctx)
	data := tasksData{
		Ready:      taskRows(ready),
		InProgress: heldRows(taskRows(started), namer),
		Blocked:    blockedRows(blocked),
		Gates:      blockedRows(gates),
		Gated:      blockedRows(gated),
		Closed:     taskRows(closed),
		ClosedMore: total - len(closed), ClosedTotal: total, ClosedPage: page, ClosedPages: pages,
		Filter: q.json(),
	}
	if err := s.markLockedBlockers(ctx, &data); err != nil {
		return tasksData{}, err
	}
	now := time.Now().UTC()
	if err := s.stampTaskTimes(ctx, &data, now); err != nil {
		return tasksData{}, err
	}
	// The store reads each group oldest-created first, the order agents
	// claim in, which the default sort keeps; markLockedBlockers may have
	// moved a ready row to the blocked group's end. The people's queue keeps
	// its own order: by who must act.
	for _, bucket := range [][]taskRow{data.Ready, data.InProgress, data.Blocked, data.Gated} {
		if q.Sort == taskSortNewest {
			sortTaskRowsNewest(bucket)
		} else {
			sortTaskRowsOldest(bucket)
		}
	}
	data.Counts = countTaskRows(data)
	counts, err := store.OpenTaskCountsByProject(ctx, db, f)
	if err != nil {
		return tasksData{}, err
	}
	data.Projects = make([]taskProjectCount, 0, len(counts))
	for slug, n := range counts {
		data.Projects = append(data.Projects, taskProjectCount{Project: slug, Open: n})
	}
	sort.Slice(data.Projects, func(i, j int) bool {
		a, b := data.Projects[i].Project, data.Projects[j].Project
		if (a == "") != (b == "") {
			return a == ""
		}
		return a < b
	})
	return data, nil
}

// countTaskRows tallies the rails in the board's words (taskCounts).
func countTaskRows(d tasksData) taskCounts {
	c := taskCounts{Ready: len(d.Ready), Blocked: len(d.Blocked), Waiting: len(d.Gates) + len(d.Gated)}
	for _, r := range d.InProgress {
		switch r.Word().Key {
		case wordInProgress:
			c.InProgress++
		case wordLapsed:
			c.Lapsed++
		case wordUnclaimed:
			c.Unclaimed++
		case wordWaiting:
			c.Waiting++
		}
	}
	c.Open = len(d.Ready) + len(d.InProgress) + len(d.Blocked) + len(d.Gates) + len(d.Gated)
	return c
}

// stampTaskTimes sets each row's time in state (rowSince) from one read of
// when each task's state began, through the request's handle, and marks the
// ready rows nothing changed on for untouchedAfter.
func (s *Service) stampTaskTimes(ctx context.Context, data *tasksData, now time.Time) error {
	buckets := []*[]taskRow{&data.Ready, &data.InProgress, &data.Blocked, &data.Gates, &data.Gated, &data.Closed}
	var tasks []core.Task
	for _, b := range buckets {
		for _, r := range *b {
			tasks = append(tasks, core.Task{ID: r.ID, Status: core.TaskStatus(r.Status)})
		}
	}
	since, err := store.TaskStateSince(ctx, s.db(ctx), tasks)
	if err != nil {
		return err
	}
	for _, b := range buckets {
		rows := *b
		for i := range rows {
			if at := rowSince(rows[i], since[rows[i].ID]); !at.IsZero() {
				rows[i].Since = &at
			}
		}
	}
	for i := range data.Ready {
		data.Ready[i].Untouched = !data.Ready[i].Updated.IsZero() && now.Sub(data.Ready[i].Updated) >= untouchedAfter
	}
	return nil
}

// rowSince is when a row entered the state its word names, from what the
// record tells (store.TaskSince), falling back on the row's own times:
//
//	done, dropped      when it closed
//	in progress        when this run of being in progress began: its first
//	                   claim or start since it was last open (a heartbeat
//	                   continues it); its last change when nothing says
//	waiting            when it was last opened or its gate set, the later
//	blocked            when it was last opened (created, reopened, released)
//	ready              the latest of that, its gate's clearing, and its last
//	                   blocker's close
func rowSince(r taskRow, s store.TaskSince) time.Time {
	switch core.TaskStatus(r.Status) {
	case core.TaskDone, core.TaskDropped:
		if r.Closed != nil {
			return *r.Closed
		}
		if !s.Status.IsZero() {
			return s.Status
		}
		return r.Updated
	case core.TaskInProgress:
		if !s.Status.IsZero() {
			return s.Status
		}
		return r.Updated
	}
	at := r.Created
	if s.Status.After(at) {
		at = s.Status
	}
	switch {
	case r.Gate != nil && r.Gate.Open:
		if s.GateSet.After(at) {
			at = s.GateSet
		}
	case r.BlockedBy() > 0:
	default:
		if r.Gate != nil && r.Gate.ClearedAt != nil && r.Gate.ClearedAt.After(at) {
			at = *r.Gate.ClearedAt
		}
		if s.BlockersClosed.After(at) {
			at = s.BlockersClosed
		}
	}
	return at
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
	q, ok := s.taskQueryOf(w, r)
	if !ok {
		return
	}
	data, ok := s.tasksPageFor(w, r, q)
	if !ok {
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
				d.Next = q.listNext(d.ID)
				data.Selected = &d
				data.SelectedAuto = true
			}
		}
		data.View = newTaskView(q, data, "/console/tasks")
		if !data.listsAny() && !q.narrowed() && !q.Steps {
			steps, err := store.AnyPlanStep(ctx, s.db(ctx))
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			data.View.StepsOnly = steps
		}
	}
	s.render(w, r, "tasks", pageData{Title: "Tasks", Active: "tasks", Data: data})
}

// tasksPageFor is tasksPage for a request: a closed page past the last is a
// 404 naming how many there are, any other failure a 500. ok=false means the
// answer was written.
func (s *Service) tasksPageFor(w http.ResponseWriter, r *http.Request, q taskQuery) (tasksData, bool) {
	data, err := s.tasksPage(r.Context(), q)
	var past *closedPageError
	switch {
	case errors.As(err, &past):
		s.notFound(w, r, fmt.Sprintf("No page %d of the closed tasks: they fill %s.", past.page, plural(past.pages, "page", "pages")))
		return tasksData{}, false
	case err != nil:
		s.serverError(w, r, err)
		return tasksData{}, false
	}
	return data, true
}

// listNext is where the reader's actions on task id return to while the
// page carries a state: the task with that state, so a triage pass keeps
// its filters. "" with none, which leaves each action on its own default
// (the task's page).
func (q taskQuery) listNext(id string) string {
	if q.QS() == "" {
		return ""
	}
	return q.href("/console/tasks/" + id)
}

// defaultTaskID picks the reader's default selection on the list URL: the work
// happening now first -- held by a live claim, then started with none -- then
// the ready queue, then blocked, then the work waiting on people, then the
// newest closed ("" when the page lists no task at all).
func defaultTaskID(data tasksData) string {
	for _, bucket := range [][]taskRow{data.Live(), data.Unheld(), data.Ready, data.Blocked, data.Gates, data.Gated, data.Closed} {
		if len(bucket) > 0 {
			return bucket[0].ID
		}
	}
	return ""
}

// taskView is the Tasks page's controls under its state: the text filter's
// hidden fields, the project menu, the plan steps toggle, the sort menu and
// the closed pager. HTML only.
type taskView struct {
	Query taskQuery
	// QS is the state as a ?query suffix: every rail row's link carries it.
	QS string
	// Filtered is a filter that keeps tasks out (narrowed); ClearHref goes
	// back to the default list, the sort kept; SearchHref looks for the
	// text across everything, on Search.
	Filtered              bool
	ClearHref, SearchHref string
	// Keep is the text filter's hidden inputs.
	Keep []libField
	// Projects is the project menu, ProjectLabel its current choice.
	Projects     []libLink
	ProjectLabel string
	// Steps is the Plan steps toggle; PlanClear drops the plan filter.
	Steps     libLink
	PlanClear string
	// Sorts is the sort menu, on the task being read; SortLabel its choice.
	Sorts     []libLink
	SortLabel string
	// ClosedAllHref pages through every closed task; ClosedNewer and
	// ClosedOlder step a page of them.
	ClosedAllHref, ClosedNewer, ClosedOlder string
	// WaitingHref is where the waiting on people chip points: the gates,
	// else the steps waiting on people.
	WaitingHref string
	// StepsOnly says the default list is empty while plan steps exist, so
	// the page offers them rather than saying there are no tasks.
	StepsOnly bool
}

// newTaskView draws the controls for state q over data, on a page whose
// reader shows base (/console/tasks when nothing is asked for). A choice that
// changes what the list holds goes to the list, which opens its first task; a
// sort stays on the task being read.
func newTaskView(q taskQuery, data tasksData, base string) *taskView {
	v := &taskView{Query: q, QS: q.QS(), Filtered: q.narrowed(), WaitingHref: "#rg-gates"}
	if len(data.Gates) == 0 {
		v.WaitingHref = "#rg-gated"
	}
	const list = "/console/tasks"
	// A choice that changes the list starts its closed pages over.
	fresh := func(mut func(*taskQuery)) string {
		o := q
		o.Page = 1
		mut(&o)
		return o.href(list)
	}
	v.ClearHref = fresh(func(o *taskQuery) { *o = taskQuery{Sort: q.Sort, Page: 1} })
	if q.Q != "" {
		v.SearchHref = "/console/search?q=" + url.QueryEscape(q.Q)
	}
	keep := q.values()
	keep.Del("q")
	keep.Del("page")
	names := make([]string, 0, len(keep))
	for name := range keep {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v.Keep = append(v.Keep, libField{Name: name, Value: keep.Get(name)})
	}

	total := 0
	for _, p := range data.Projects {
		total += p.Open
	}
	v.Projects = []libLink{{Label: "All projects", Href: fresh(func(o *taskQuery) { o.Project = "" }),
		Count: total, On: q.Project == ""}}
	v.ProjectLabel = "All"
	chosen := q.Project
	if chosen == validate.GlobalProject {
		chosen = ""
	}
	listed := false
	for _, p := range data.Projects {
		listed = listed || (q.Project != "" && p.Project == chosen)
	}
	entries := data.Projects
	if q.Project != "" && !listed {
		entries = append(slices.Clone(entries), taskProjectCount{Project: chosen})
	}
	for _, p := range entries {
		slug, label := p.Project, p.Project
		if slug == "" {
			slug, label = validate.GlobalProject, "global"
		}
		on := q.Project != "" && p.Project == chosen
		if on {
			v.ProjectLabel = label
		}
		v.Projects = append(v.Projects, libLink{Label: label, Href: fresh(func(o *taskQuery) { o.Project = slug }),
			Count: p.Open, On: on})
	}

	v.Steps = libLink{Label: "Plan steps", On: q.Steps || q.Plan != "",
		Href: fresh(func(o *taskQuery) { o.Steps, o.Plan = !(q.Steps || q.Plan != ""), "" })}
	if q.Plan != "" {
		v.PlanClear = fresh(func(o *taskQuery) { o.Plan = "" })
	}
	for _, key := range taskSortKeys {
		o := q
		o.Sort = key
		label := "Oldest first"
		if key == taskSortNewest {
			label = "Newest first"
		}
		if key == q.Sort {
			v.SortLabel = label
		}
		v.Sorts = append(v.Sorts, libLink{Label: label, Href: o.href(base), On: key == q.Sort})
	}
	if !q.ClosedAll {
		if data.ClosedMore > 0 {
			v.ClosedAllHref = fresh(func(o *taskQuery) { o.ClosedAll = true }) + "#rg-closed"
		}
		return v
	}
	if data.ClosedPage > 1 {
		o := q
		o.Page = data.ClosedPage - 1
		v.ClosedNewer = o.href(list) + "#rg-closed"
	}
	if data.ClosedPage < data.ClosedPages {
		o := q
		o.Page = data.ClosedPage + 1
		v.ClosedOlder = o.href(list) + "#rg-closed"
	}
	return v
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

// gateArgWords are the tasks_update gate arguments the store's refusals
// name, each with the form's word for it, longest first so none is read
// inside another.
var gateArgWords = []struct{ arg, words string }{
	{"gate_ready_when", "ready when"},
	{"gate_decision", "the decision"},
	{"gate_what", "what"},
	{"gate_who", "who"},
}

// taskGateSet sets what a task waits on outside Seamless -- who must act and
// what must happen, with the optional decision and ready-when lines -- the
// way tasks_update gate_who and gate_what do (store.TaskGatePatch through
// store.UpdateTaskDetailed): a step whose gate is open leaves the ready queue
// for the people's, and stays there until the gate is marked as handled. A
// gate already cleared opens again as now set. It refuses a closed task, and
// one a live claim holds (as refusedHeld does), in its own words; setting the
// gate the task already has writes nothing. Each line is one line, as typed
// across lines or padded.
func (s *Service) taskGateSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.taskForAction(w, r)
	if !ok {
		return
	}
	next := r.PostFormValue("next")
	line := func(name string) string { return strings.Join(strings.Fields(r.PostFormValue(name)), " ") }
	who, what := line("who"), line("what")
	if who == "" || what == "" {
		s.taskAnswer(w, r, t.ID, next, http.StatusBadRequest, "error",
			"Say who the task waits on and what must happen: both are needed to set its gate.")
		return
	}
	if t.Status.Closed() {
		s.taskAnswer(w, r, t.ID, next, http.StatusConflict, "error",
			"The task is "+string(t.Status)+": reopen it first, then say what it waits on.")
		return
	}
	edit := store.TaskGatePatch{Who: &who, What: &what}
	decision, readyWhen := line("decision"), line("ready_when")
	edit.Decision, edit.ReadyWhen = &decision, &readyWhen
	if t.Gate.Open() && t.Gate.Who == who && t.Gate.What == what && t.Gate.Decision == decision && t.Gate.ReadyWhen == readyWhen {
		s.taskAnswer(w, r, t.ID, next, http.StatusOK, "notice", "The task already waits on "+who+".")
		return
	}
	held := func(task core.Task) {
		s.taskAnswer(w, r, task.ID, next, http.StatusConflict, "error",
			s.holderName(ctx, task)+" holds a live claim on this task: release the claim first, then say what it waits on.")
	}
	if t.ClaimLive(time.Now().UTC()) {
		held(t)
		return
	}
	res, err := store.UpdateTaskDetailed(ctx, s.writeDB(), t.ID, store.TaskPatch{Gate: &edit}, "", time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrTaskGate):
		s.taskAnswer(w, r, t.ID, next, http.StatusBadRequest, "error", "The gate was not set: "+gateRefusal(err)+".")
		return
	case errors.Is(err, store.ErrTaskClaimConflict):
		// Claimed between the read and the write.
		if now, rerr := store.TaskByID(ctx, s.db(ctx), t.ID); rerr == nil {
			t = now
		}
		held(t)
		return
	case err != nil:
		s.taskActionFailed(w, r, t.ID, next, err)
		return
	}
	s.recordTaskAction(ctx, res, map[string]any{"gate": "set"})
	s.taskActionDone(w, r, res.Task, next, "Waits on "+who+" now: it stays out of the ready queue until its gate is marked as handled.")
}

// gateRefusal is the store's gate refusal in the owner's words: why, with
// the form's field names for the tasks_update arguments.
func gateRefusal(err error) string {
	why := err.Error()
	if _, after, found := strings.Cut(why, store.ErrTaskGate.Error()+": "); found {
		why = after
	}
	for _, f := range gateArgWords {
		why = strings.ReplaceAll(why, "("+f.arg+")", "")
		why = strings.ReplaceAll(why, f.arg, f.words)
	}
	return strings.Join(strings.Fields(why), " ")
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
	// WaitOn: an open or in-progress task no live claim holds, whose own gate
	// is not open: Waits on someone sets one (who and what).
	WaitOn bool
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
		a.WaitOn = !a.Handle
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
	return s.holderName(ctx, t) + " holds a live claim on this task: release the claim first, then close it."
}

// holderName names who holds a task's claim: its session, or an agent.
func (s *Service) holderName(ctx context.Context, t core.Task) string {
	if h := holderLink(t.ClaimedBy, s.sessionNamer(ctx)); h != nil {
		return h.Name
	}
	return "An agent"
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

// sortTaskRowsNewest orders an open-state rail group newest-created first.
// ID is the deterministic newest-first tie-breaker.
func sortTaskRowsNewest(rows []taskRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Created.Equal(rows[j].Created) {
			return rows[i].Created.After(rows[j].Created)
		}
		return rows[i].ID > rows[j].ID
	})
}

// sortTaskRowsOldest orders an open-state rail group oldest-created first,
// ties by id: the order the store reads the queue in, and agents claim in.
func sortTaskRowsOldest(rows []taskRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Created.Equal(rows[j].Created) {
			return rows[i].Created.Before(rows[j].Created)
		}
		return rows[i].ID < rows[j].ID
	})
}

// taskRows projects tasks into rows. A row knows its blockers only when its
// list read them: an open row with dependencies reads them through
// readRowBlockers, or a list of ready tasks has none to read.
func taskRows(tasks []core.Task) []taskRow {
	now := time.Now().UTC()
	out := make([]taskRow, 0, len(tasks))
	for _, t := range tasks {
		row := rowOf(t, now)
		row.blockersRead = len(t.DependsOn) == 0
		out = append(out, row)
	}
	return out
}

// rowOf is the row every list projects a task into, as of now: its blockers
// are its list's to read.
func rowOf(t core.Task, now time.Time) taskRow {
	row := taskRow{
		ID: t.ID, Title: t.Title, Project: t.ProjectSlug, Status: string(t.Status),
		Favorite: t.Favorite, Deps: len(t.DependsOn), Account: t.Account, Created: t.CreatedAt, Closed: t.ClosedAt,
		Kind: taskKind(t), Gate: gateView(t),
		ClaimedBy: t.ClaimedBy, ClaimLive: t.ClaimLive(now),
		Plan: t.PlanSlug, Updated: t.UpdatedAt,
		dependsOn: t.DependsOn,
	}
	if t.ClaimedBy != "" && t.Status == core.TaskInProgress {
		row.Lease, row.LeaseExpiry = claimLease(t.LeaseExpiresAt, row.ClaimLive, now), t.LeaseExpiresAt
	}
	return row
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
	now := time.Now().UTC()
	out := make([]taskRow, 0, len(blocked))
	for _, b := range blocked {
		row := rowOf(b.Task, now)
		row.blockersRead = true
		for _, bl := range b.Blockers {
			row.Blockers = append(row.Blockers, blocker{ID: bl.ID, Title: bl.Title, Status: string(bl.Status),
				Kind: taskKind(bl), Gate: gateView(bl)})
		}
		out = append(out, row)
	}
	return out
}
