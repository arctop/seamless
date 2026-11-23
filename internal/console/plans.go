package console

// The Plans screen is one board of plan cards for every project
// (plan:console-ux-pass UX 07): the card the project tab shows
// (templates/plan_shared.html), grouped by the state the plan is in -- Needs
// you, In progress, Stalled, Ready, Shipped -- top to bottom. Only Shipped is
// windowed (?w=) and folded: an active plan is never hidden by a window. The
// board filters by project (?project=, chosen with the project switcher the
// other lists draw, scope.go) and by text over title and slug (?q=).
//
// A plan is keyed by its project and slug: /console/plans/{slug}?project=
// opens one plan on its own page, in the reading frame a memory, a note and
// a task open in (itempage_plan.go): its steps first in dependency order as
// the card's step rows, then its narrative, with its narrative note, its
// attached notes and its project beside them. A slug that is a plan in
// more than one project, asked for without ?project=, answers a chooser,
// never one page holding both. The screen also carries the owner escape
// hatch for Claude Code bug #20397 (an approval whose PostToolUse never
// fired): POST /console/plans/{slug}/approve flips the note to approved and
// creates the tracking task, exactly as the hook would have.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// Plan sources: a "capture" row is a Claude Code plan-mode capture (cc-plan
// note, with an iteration/agents/basename); a "composed" row is a plain
// plans-as-composition plan (a note tagged plan:<slug>, or only its tasks),
// which has none of the capture-only columns. The words the owner reads are
// planSourceText's.
const (
	planSourceCapture  = "capture"
	planSourceComposed = "composed"
)

// Plan phases bucket a plan by the progress of its step tasks: in progress
// (a step is being worked), ready (open steps remain, or the plan has not
// started), done (all steps closed, or a settled capture). The JSON keeps
// them for its readers; the board groups by the plan's state.
const (
	planPhaseInProgress = "in_progress"
	planPhaseReady      = "ready"
	planPhaseDone       = "done"
)

// Plan states, the board's groups top to bottom (the plan note's five
// states, planStateOf): Needs you, a capture the owner can approve, a step
// waiting on people, or a step in progress that no live claim holds; In
// progress, a live claim holds a step; Stalled, open steps, none in progress,
// nothing moved for planStalledAfter; Ready, everything else still open;
// Shipped, every step closed (done or dropped), or a settled capture.
const (
	planStateNeeds      = "needs_you"
	planStateInProgress = "in_progress"
	planStateStalled    = "stalled"
	planStateReady      = "ready"
	planStateShipped    = "shipped"
)

// planGroupDefs are the board's open groups in reading order. Needs you and
// In progress cards open with their steps; Stalled and Ready cards show
// their head line and load their steps when opened (planTimelineVM.Lazy).
var planGroupDefs = []planGroupVM{
	{Key: planStateNeeds, ID: "pg-needs", Label: "Needs you", Open: true,
		Hint: "A captured plan you can approve, a step waiting on people (a gate), or a step in progress that no live claim holds."},
	{Key: planStateInProgress, ID: "pg-inprogress", Label: "In progress", Open: true,
		Hint: "A live claim holds a step."},
	{Key: planStateStalled, ID: "pg-stalled", Label: "Stalled",
		Hint: "Open steps, none in progress, and nothing moved for 7 days."},
	{Key: planStateReady, ID: "pg-ready", Label: "Ready",
		Hint: "Open steps that moved in the last 7 days, or a plan with no steps yet."},
}

// planGroupVM is one group of the board: its state, anchor, words and cards.
// Open groups render their cards' steps.
type planGroupVM struct {
	Key   string
	ID    string
	Label string
	Hint  string
	Open  bool
	Cards []planTimelineVM
}

// planRow is the JSON projection of one plan (capture or composed), the
// model `seam plan list` reads (cmd/seam/plan.go cliPlanRow).
type planRow struct {
	NoteID   string `json:"noteId"`
	Slug     string `json:"slug"`     // plan:<slug> composition key
	Source   string `json:"source"`   // capture | composed
	Basename string `json:"basename"` // CC plan file name without .md (captures only)
	Title    string `json:"title"`
	Project  string `json:"project,omitempty"`
	Status   string `json:"status"`
	Favorite bool   `json:"favorite,omitempty"` // the primary note's star
	// Href is the plan's page, its project named (a slug is a plan per
	// project).
	Href      string `json:"href,omitempty"`
	Iteration int    `json:"iteration,omitempty"`
	Agents    int    `json:"agents"` // cached subagent notes in the composition
	// TasksDone is the closed steps, done and dropped alike (a plan whose
	// every step closed is complete); TasksDropped how many of them were
	// dropped.
	TasksDone    int    `json:"tasksDone"`
	TasksDropped int    `json:"tasksDropped,omitempty"`
	TasksOpen    int    `json:"tasksOpen"`       // open (not started, not closed) steps
	TasksWIP     int    `json:"tasksInProgress"` // steps currently being worked
	TasksTotal   int    `json:"tasksTotal"`
	GatesOpen    int    `json:"gatesOpen,omitempty"`
	Phase        string `json:"phase"` // in_progress | ready | done
	// State is the board's group (planStateOf); StalledDays how long a
	// stalled plan has not moved.
	State       string `json:"state,omitempty"`
	StalledDays int    `json:"stalledDays,omitempty"`
	// Updated is the COMPOSITION's last activity: the newest stamp across every
	// note tagged plan:<slug> and every plan-step task -- not the primary note's
	// own. A plan moves when any part of it moves.
	Updated time.Time `json:"updated"`
	// Shipped marks a plan with a plan.shipped settlement in the current local
	// month -- the mom-settle hook the live-arrival wash keys on. A momentum
	// surface: never set while the feature is off (zero trace, JSON included).
	Shipped bool `json:"shipped,omitempty"`
	// Token fields project store.PlanTokenRollup: the cumulative model tokens
	// of the sessions attributed to this plan (whole-session transcript totals,
	// a session counted once however many steps it moved). The qualifiers
	// travel with the number so no surface renders it bare: TokenSessions is
	// the attributed-session count, TokenUnreported the sessions whose tokens
	// have not landed (live, or ended without a clean SessionEnd), TokenShared
	// the sessions also attributed to another plan -- which is why plan tokens
	// must never be summed across plans.
	Tokens          int `json:"tokens,omitempty"`
	TokenSessions   int `json:"tokenSessions,omitempty"`
	TokenUnreported int `json:"tokenUnreported,omitempty"`
	TokenShared     int `json:"tokenShared,omitempty"`
}

// plansData is the board's payload. Rows are every plan the board lists, in
// its order (group by group); the counts are its groups'. Total is every plan
// under the project and text filters, all time; Count the ones listed: every
// active plan, and the shipped ones inside the window.
type plansData struct {
	Rows       []planRow `json:"rows"`
	Count      int       `json:"count"`
	Total      int       `json:"total"`
	Active     int       `json:"active"`
	NeedsYou   int       `json:"needsYou"`
	InProgress int       `json:"inProgress"`
	Stalled    int       `json:"stalled"`
	Ready      int       `json:"ready"`
	// Done is the shipped plans the window lists; ShippedAll every shipped
	// plan, all time.
	Done        int            `json:"done"`
	ShippedAll  int            `json:"shippedAll"`
	Window      string         `json:"window"`
	WindowLabel string         `json:"windowLabel"`
	Windows     []windowOption `json:"-"`
	// Filter is the board's state in its URL's words; Projects the project
	// menu: each project with a plan the board would list, by slug ("" is
	// the global scope), and how many.
	Filter   planFilterJSON     `json:"filter"`
	Projects []planProjectCount `json:"projects"`
	// ShippedMonth is the header's quiet momentum line: the real count of
	// plan.shipped settlements in the current LOCAL calendar month. nil while
	// the momentum feature is off -- zero trace, including in the JSON.
	ShippedMonth *planShippedMonth `json:"shippedThisMonth,omitempty"`
	// Locked names the locked projects this request does not read, whose
	// plans the board leaves out: their names only, never a count of what
	// they hold. nil while nothing is locked -- absent from the JSON.
	Locked []string `json:"locked,omitempty"`

	// The HTML board: its groups, the shipped lines, its controls, and the
	// URL its own actions return to. A plan's own page (itempage_plan.go)
	// and the chooser render in the same screen instead (Item, Chooser).
	Groups  []planGroupVM    `json:"-"`
	Shipped []planTimelineVM `json:"-"`
	View    *planView        `json:"-"`
	Return  string           `json:"-"`
	Item    *planItemPage    `json:"-"`
	Chooser *planChooserVM   `json:"-"`
}

// planFilterJSON is the board's state as its URL names it.
type planFilterJSON struct {
	Project string `json:"project,omitempty"`
	Q       string `json:"q,omitempty"`
}

// planProjectCount is one entry of the board's project menu.
type planProjectCount struct {
	Project string `json:"project"`
	Plans   int    `json:"plans"`
}

// planShippedMonth carries the Plans header's monthly shipped count, a
// verbatim count of plan.shipped events (each latched once-ever per plan).
type planShippedMonth struct {
	Count int `json:"count"`
}

// planKey identifies a plan by the (project, slug) pair rather than the slug
// alone: the same plan name may run in more than one project, and those are
// distinct plans with distinct steps.
func planKey(project, slug string) string { return project + "\x00" + slug }

// planProjectParam is a project as the console's URLs name it: its slug, or
// global for the global scope.
func planProjectParam(project string) string {
	if project == "" {
		return validate.GlobalProject
	}
	return project
}

// planProjectLabel is a project as the board shows it.
func planProjectLabel(project string) string {
	if project == "" {
		return "global"
	}
	return project
}

// planHref is a plan's page: its slug, its project named.
func planHref(project, slug string) string {
	return "/console/plans/" + url.PathEscape(slug) + "?project=" + url.QueryEscape(planProjectParam(project))
}

// planApproveHref is the escape hatch's route for a plan, its project named,
// so Approve never has to guess between two projects' captures of one slug.
func planApproveHref(project, slug string) string {
	return "/console/plans/" + url.PathEscape(slug) + "/approve?project=" + url.QueryEscape(planProjectParam(project))
}

// planAnchor is a plan's card id on the board, unique per project and slug:
// neither a project slug nor a plan slug may hold a colon (validate.Name).
// The project tab keeps the card's plan-<slug>.
func planAnchor(project, slug string) string {
	return "plan-" + planProjectParam(project) + ":" + slug
}

// planTitleHTML is a plan title for display: escaped, with each " -- " shown
// as a dash that stays with the word before it (a no-break space, then an en
// dash) so a wrap never splits it in two. The stored title is untouched.
func planTitleHTML(title string) template.HTML {
	esc := template.HTMLEscapeString(title)
	// Escaped first, so the only markup is the fixed entity pair.
	return template.HTML(strings.ReplaceAll(esc, " -- ", ": "))
}

// planSourceText names where a plan came from, in the owner's words.
func planSourceText(source string) string {
	if source == planSourceCapture {
		return "captured from plan mode"
	}
	return "from tasks"
}

// planNoteRef is a compact pointer to a note attached to a plan composition.
type planNoteRef struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Slug    string    `json:"slug"`
	IsAgent bool      `json:"isAgent"` // an agent-cache note (vs a supporting note)
	Updated time.Time `json:"updated"`
}

// planTaskRef is a compact pointer to a plan step task, with its state in the
// owner's words (taskwords.go), who holds its claim, and the gates it waits on.
type planTaskRef struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Status string        `json:"status"`
	Word   taskWord      `json:"word"`
	Holder string        `json:"holder,omitempty"`
	Waits  []planWaitRef `json:"waits,omitempty"`
}

// planWaitRef is one open gate a step waits on: who must act, and what must
// happen.
type planWaitRef struct {
	Who  string `json:"who"`
	What string `json:"what"`
}

// planDetailData is the single-plan payload: the row, the rendered plan body,
// the composition's attached notes, and its steps in the card's order. NoNote
// marks a plan whose steps exist with no note tagged plan:<slug> in its
// project: no body, no note to link or star, nothing to approve. Card is the
// plan's card with every step row, which the page and the reader render.
// Lead is a composed plan's narrative description, the page's lead (a
// capture's description only restates its facts); HTML only.
type planDetailData struct {
	Row        planRow        `json:"plan"`
	Body       template.HTML  `json:"-"`
	BodyText   string         `json:"body"`
	BodyLoaded bool           `json:"bodyAvailable"`
	Attached   []planNoteRef  `json:"attached,omitempty"`
	Tasks      []planTaskRef  `json:"tasks,omitempty"`
	CanApprove bool           `json:"canApprove"`
	NoNote     bool           `json:"noNote,omitempty"`
	Card       planTimelineVM `json:"-"`
	Return     string         `json:"-"`
	Lead       string         `json:"-"`
}

// SourceText is the plan's source in the owner's words.
func (d planDetailData) SourceText() string { return planSourceText(d.Row.Source) }

// planChooserVM answers a slug that is a plan in more than one project,
// asked for without ?project=: one choice per project, never a merge.
type planChooserVM struct {
	Slug    string
	Choices []planChoiceVM
	Error   string
}

// planChoiceVM is one project's plan of the slug.
type planChoiceVM struct {
	Project string `json:"project"`
	Title   string `json:"title"`
	Href    string `json:"href"`
}

// TitleHTML is the choice's title for display (planTitleHTML).
func (c planChoiceVM) TitleHTML() template.HTML { return planTitleHTML(c.Title) }

// planQuery is the board's state from its URL: the project filter (?project=,
// validate.GlobalProject for the global scope), the text filter (?q=), and
// the window over the shipped plans (?w=, 24h by default).
type planQuery struct {
	Project string
	Q       string
	Window  store.RetrievalWindow
}

// parsePlanQuery reads the board's state from its URL, strictly: a project
// that is no slug, or a window outside 24h, 7d, 30d and all, is a 400, never
// a silent default. Parameters it does not know are left to their readers.
func parsePlanQuery(v url.Values, now time.Time) (planQuery, error) {
	q := planQuery{Q: strings.TrimSpace(v.Get("q"))}
	if raw := strings.TrimSpace(v.Get("project")); raw != "" {
		slug, err := validate.Project(raw)
		if err != nil {
			return q, fmt.Errorf("invalid project %q: name a project's slug, or global for the global scope", raw)
		}
		q.Project = planProjectParam(slug)
	}
	w := v.Get("w")
	if w != "" && !slices.Contains(store.RetrievalWindowKeys, w) {
		return q, fmt.Errorf("invalid w %q: valid values are %s", w, strings.Join(store.RetrievalWindowKeys, ", "))
	}
	q.Window = store.ResolveRetrievalWindow(w, now)
	return q, nil
}

// scope is the project filter as a project slug ("" is the global scope),
// and whether one is set.
func (q planQuery) scope() (string, bool) {
	if q.Project == "" {
		return "", false
	}
	if q.Project == validate.GlobalProject {
		return "", true
	}
	return q.Project, true
}

// values is the state's parts that are not their defaults.
func (q planQuery) values() url.Values {
	v := url.Values{}
	if q.Project != "" {
		v.Set("project", q.Project)
	}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.Window.Key != "24h" {
		v.Set("w", q.Window.Key)
	}
	return v
}

// href is the board under the state.
func (q planQuery) href() string { return "/console/plans" + encodeQS(q.values()) }

// matches reports whether a plan's title or slug holds the text filter, in
// any case ("plan:<slug>", the form a plan copies as, works too).
func (q planQuery) matches(title, slug string) bool {
	if q.Q == "" {
		return true
	}
	needle := strings.ToLower(strings.TrimPrefix(q.Q, "plan:"))
	return strings.Contains(strings.ToLower(title), needle) || strings.Contains(strings.ToLower(slug), needle)
}

// planView is the board's controls under its state: the text filter's
// hidden fields, the project menu and the switcher drawn from it, the
// shipped window and the clear link. HTML only.
type planView struct {
	Query        planQuery
	Filtered     bool
	ClearHref    string
	Keep         []libField
	Projects     []libLink
	ProjectLabel string
	// Scope is the project switcher (scope.go) built from Projects: the
	// board draws it in the place of a Project menu (plan:console-ux-pass
	// UX 32), as Memories and Notes do.
	Scope   projectSwitcher
	Windows []libLink
	// Wider is the windows past the current one, for the shipped fold's
	// "shipped before this window" line.
	Wider []libLink
}

// newPlanView draws the board's controls for state q over the menu counts.
// The project menu is the switcher's input (projectSwitcher, scope.go): All
// projects, then each scope with a plan on the board, its ?project= value
// (Value), what choosing it shows (Count) and the project's display name
// where it differs from its slug (Title, which typing in the switcher
// matches); the switcher orders and marks them. The chosen scope stays
// listed when it holds no plan, so the button always names it.
func newPlanView(q planQuery, counts []planProjectCount, names map[string]string) *planView {
	v := &planView{Query: q, Filtered: q.Project != "" || q.Q != ""}
	with := func(mut func(*planQuery)) string {
		o := q
		mut(&o)
		return o.href()
	}
	v.ClearHref = with(func(o *planQuery) { o.Project, o.Q = "", "" })
	keep := q.values()
	keep.Del("q")
	for _, name := range []string{"project", "w"} {
		if val := keep.Get(name); val != "" {
			v.Keep = append(v.Keep, libField{Name: name, Value: val})
		}
	}
	total := 0
	for _, c := range counts {
		total += c.Plans
	}
	v.Projects = []libLink{{Label: "All projects", Href: with(func(o *planQuery) { o.Project = "" }), Count: total, On: q.Project == ""}}
	v.ProjectLabel = "All"
	chosen, scoped := q.scope()
	entries := slices.Clone(counts)
	if scoped && !slices.ContainsFunc(entries, func(c planProjectCount) bool { return c.Project == chosen }) {
		entries = append(entries, planProjectCount{Project: chosen})
	}
	slices.SortStableFunc(entries, func(a, b planProjectCount) int { return strings.Compare(a.Project, b.Project) })
	for _, c := range entries {
		param, label := planProjectParam(c.Project), planProjectLabel(c.Project)
		on := scoped && c.Project == chosen
		if on {
			v.ProjectLabel = label
		}
		title := names[c.Project]
		if title == c.Project {
			title = ""
		}
		v.Projects = append(v.Projects, libLink{Label: label, Href: with(func(o *planQuery) { o.Project = param }),
			Title: title, Count: c.Plans, On: on, Value: param})
	}
	past := false
	for _, key := range store.RetrievalWindowKeys {
		link := libLink{Label: windowLabels[key], Href: with(func(o *planQuery) { o.Window = store.ResolveRetrievalWindow(key, time.Now()) }) + "#pg-shipped",
			On: key == q.Window.Key}
		v.Windows = append(v.Windows, link)
		if past {
			v.Wider = append(v.Wider, link)
		}
		past = past || key == q.Window.Key
	}
	return v
}

// planEntry is one plan before it is a card: its key, its steps' rollup (nil
// with no step), its primary note (nil with none in its project), and its
// composition's newest note stamp.
type planEntry struct {
	Project      string
	Slug         string
	Rollup       *store.PlanRollup
	Note         *core.Note
	NoteActivity time.Time
}

// key is the plan's (project, slug) key.
func (e planEntry) key() string { return planKey(e.Project, e.Slug) }

// ref is the plan as the store keys it.
func (e planEntry) ref() store.PlanRef { return store.PlanRef{Project: e.Project, Slug: e.Slug} }

// title is the narrative note's title, the slug without one.
func (e planEntry) title() string {
	if e.Note != nil && strings.TrimSpace(e.Note.Title) != "" {
		return e.Note.Title
	}
	return e.Slug
}

// capture reports a Claude Code plan-mode capture.
func (e planEntry) capture() bool {
	return e.Note != nil && slices.Contains(e.Note.Tags, plans.TagPlan)
}

// status is the capture's lifecycle status ("" for a composed plan).
func (e planEntry) status() string {
	if e.Note == nil {
		return ""
	}
	return plans.StatusFromTags(e.Note.Tags)
}

// canApprove reports a capture still a draft or presented: the card offers
// Approve (withNote's rule).
func (e planEntry) canApprove() bool {
	st := e.status()
	return e.capture() && (st == plans.StatusDraft || st == plans.StatusPresented)
}

// shipped reports a plan whose every step closed, or a capture the gardener
// settled with no step of its own.
func (e planEntry) shipped() bool {
	if e.Rollup != nil {
		return e.Rollup.Total > 0 && e.Rollup.Done >= e.Rollup.Total
	}
	switch e.status() {
	case plans.StatusAbandoned, plans.StatusShipped, plans.StatusMerged:
		return e.capture()
	}
	return false
}

// activity is the composition's last activity: its steps' and its notes'.
func (e planEntry) activity() time.Time {
	at := e.NoteActivity
	if e.Note != nil && e.Note.Updated.After(at) {
		at = e.Note.Updated
	}
	if e.Rollup != nil && e.Rollup.LastActivity.After(at) {
		at = e.Rollup.LastActivity
	}
	return at
}

// planEntries reads every plan the request's handle holds: one per (project,
// slug) with a step (store.AllPlanRollups) or a primary note (a capture wins,
// else the earliest non-agent note: betterPrimary), each with its
// composition's newest note stamp. A capture with no plan:<slug> tag names
// no plan. Ordered by key, so every caller's order is its own choice.
func (s *Service) planEntries(ctx context.Context) ([]planEntry, error) {
	db := s.db(ctx)
	rollups, err := store.AllPlanRollups(ctx, db)
	if err != nil {
		return nil, err
	}
	notes, err := store.NotesByTagPrefix(ctx, db, "", plans.SlugTagPrefix())
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*planEntry, len(rollups))
	for i := range rollups {
		pl := rollups[i].PlanRollup
		byKey[planKey(rollups[i].Project, pl.Slug)] = &planEntry{Project: rollups[i].Project, Slug: pl.Slug, Rollup: &pl}
	}
	activity := planNoteActivity(notes)
	primary := map[string]core.Note{}
	for _, n := range notes {
		slug := plans.SlugFromTags(n.Tags)
		if slug == "" || slices.Contains(n.Tags, plans.TagAgent) {
			continue
		}
		k := planKey(n.Project, slug)
		if cur, ok := primary[k]; !ok || betterPrimary(n, cur) {
			primary[k] = n
		}
	}
	for k, n := range primary {
		e, ok := byKey[k]
		if !ok {
			e = &planEntry{Project: n.Project, Slug: plans.SlugFromTags(n.Tags)}
			byKey[k] = e
		}
		note := n
		e.Note = &note
	}
	out := make([]planEntry, 0, len(byKey))
	for k, e := range byKey {
		e.NoteActivity = activity[k]
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b planEntry) int { return strings.Compare(a.key(), b.key()) })
	return out, nil
}

// planInputs is what every card and row of one page reads once: the token
// rollups, the agent caches, the dropped steps, the pending settlements, the
// projects with a workspace and their display names, and the session namer.
type planInputs struct {
	tokens     map[store.PlanRef]store.PlanTokenRollup
	agents     map[string]int
	dropped    map[store.PlanRef]int
	stale      planStaleSet
	registered map[string]bool
	names      map[string]string
	namer      func(string) core.Session
	now        time.Time
}

// planInputsOf reads a page's planInputs through the request's handle.
func (s *Service) planInputsOf(ctx context.Context, now time.Time) (*planInputs, error) {
	db := s.db(ctx)
	in := &planInputs{now: now, namer: s.sessionNamer(ctx), registered: map[string]bool{}, names: map[string]string{}}
	var err error
	if in.tokens, err = store.PlanTokenRollups(ctx, db); err != nil {
		return nil, err
	}
	if in.agents, err = s.planAgentCounts(ctx); err != nil {
		return nil, err
	}
	if in.dropped, err = store.PlanDroppedCounts(ctx, db); err != nil {
		return nil, err
	}
	in.stale = s.stalePlans(ctx)
	projects, err := store.ListProjects(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		in.registered[p.Slug] = true
		in.names[p.Slug] = p.Name
	}
	return in, nil
}

// projectTab is the project tab at a plan's card, "" for a plan with no
// project workspace to open (the global scope, an unregistered slug).
func (in *planInputs) projectTab(project, slug string) string {
	if project == "" || !in.registered[project] {
		return ""
	}
	return "/console/projects/" + url.PathEscape(project) + "?tab=tasks#plan-" + slug
}

// planStaleSet is the plans a pending gardener settlement names: by project
// and slug, and by slug alone for a settlement whose note is global (it
// settles a plan whose steps live anywhere: stalePlanSlugs' rule).
type planStaleSet struct {
	byKey  map[string]bool
	bySlug map[string]bool
}

// has reports a pending settlement for the plan.
func (st planStaleSet) has(project, slug string) bool {
	return st.byKey[planKey(project, slug)] || st.bySlug[slug]
}

// stalePlans reads the pending ship, abandon and merge settlements once. The
// badge is decoration, so an error costs it and nothing else.
func (s *Service) stalePlans(ctx context.Context) planStaleSet {
	out := planStaleSet{byKey: map[string]bool{}, bySlug: map[string]bool{}}
	proposals, err := store.PendingProposals(ctx, s.db(ctx), "")
	if err != nil {
		s.logger.Warn("console: pending plan settlements", "error", err)
		return out
	}
	for _, p := range proposals {
		switch p.Kind {
		case store.ProposalShipPlan, store.ProposalAbandonPlan, store.ProposalMergePlans:
		default:
			continue
		}
		slug := payloadStr(p.Payload, "slug")
		if slug == "" {
			continue
		}
		if proj := payloadStr(p.Payload, "project"); proj != "" {
			out.byKey[planKey(proj, slug)] = true
		} else {
			out.bySlug[slug] = true
		}
	}
	return out
}

// planRowOf projects a plan into its JSON row. The capture-only columns
// (basename, iteration) are read from the cc-plan note; the iteration from
// its file, so only when withIteration (the JSON asks for it).
func (s *Service) planRowOf(e planEntry, in *planInputs, withIteration bool) planRow {
	row := planRow{Slug: e.Slug, Source: planSourceComposed, Title: e.title(), Project: e.Project,
		Href: planHref(e.Project, e.Slug), Agents: in.agents[e.key()], Updated: e.activity()}
	if e.Note != nil {
		row.NoteID, row.Status, row.Favorite = e.Note.ID, e.status(), e.Note.Favorite
		if e.capture() {
			row.Source, row.Basename = planSourceCapture, plans.Basename(e.Note.Slug)
			if withIteration && s.cfg.Files != nil {
				if full, err := s.cfg.Files.Store().ReadNote(e.Note.FilePath); err == nil {
					row.Iteration = plans.NoteIteration(full)
				}
			}
		}
	}
	if tr, ok := in.tokens[e.ref()]; ok {
		row.Tokens, row.TokenSessions, row.TokenUnreported, row.TokenShared = tr.Tokens, tr.Sessions, tr.Unreported, tr.Shared
	}
	if pl := e.Rollup; pl != nil {
		row.TasksTotal, row.TasksDone, row.TasksWIP = pl.Total, pl.Done, pl.InFlight
		row.TasksOpen = pl.Total - pl.Done - pl.InFlight
		row.GatesOpen = pl.GatesOpen
		row.TasksDropped = in.dropped[e.ref()]
	}
	row.Phase = planPhase(row)
	return row
}

// planCardOf builds a plan's card (planTimelineVM) and its step rows: the
// head from its narrative note and rollup, its project chip, its tokens, the
// rows in their words, the head's counts and the stalled mark. The rows'
// groups, gate holds and ledger are the caller's (finishCard), once it knows
// the card opens with its steps.
func (s *Service) planCardOf(ctx context.Context, e planEntry, steps []core.Task, pc planStepCtx, in *planInputs) (planTimelineVM, []planStepVM) {
	tl := planTimelineVM{Slug: e.Slug, Project: e.Project, Title: e.Slug, Anchor: planAnchor(e.Project, e.Slug),
		ShowProject: true, ProjectHref: in.projectTab(e.Project, e.Slug), Stale: in.stale.has(e.Project, e.Slug)}
	if e.Note != nil {
		tl.withNote(*e.Note)
	}
	if tr, ok := in.tokens[e.ref()]; ok {
		tl.Tokens, tl.TokenSessions, tl.TokenUnreported, tl.TokenShared = tr.Tokens, tr.Sessions, tr.Unreported, tr.Shared
	}
	if e.Rollup == nil {
		tl.NoteOnly, tl.LastActivity = true, e.activity()
		return tl, nil
	}
	pl := e.Rollup
	tl.Total, tl.Done, tl.InFlight = pl.Total, pl.Done, pl.InFlight
	tl.Open = pl.Total - pl.Done - pl.InFlight
	tl.LastActivity, tl.GatesOpen = pl.LastActivity, pl.GatesOpen
	tl.DonePct, tl.DoingPct = percent(pl.Done, pl.Total), percent(pl.InFlight, pl.Total)
	tl.Dropped = in.dropped[e.ref()]
	rows := make([]planStepVM, 0, len(steps))
	for _, st := range steps {
		rows = append(rows, s.planStep(ctx, st, pc))
	}
	// The head counts the rows, not the rollup: a step a locked project
	// blocks reads as ready on the request's handle, and its row knows better.
	tl.Counts = planCounts(rows)
	held := 0
	for _, row := range rows {
		if row.Claimable {
			tl.Claimable++
		}
		if row.Word.Key == wordInProgress {
			held++
		}
	}
	tl.Stalled, tl.StalledDays = planStalled(pl.LastActivity, in.now, pl.Total-pl.Done, held)
	return tl, rows
}

// planStateOf is a plan's group on the board, first match wins: Needs you
// (a capture the owner can approve, a step waiting on people, a step in
// progress no live claim holds), Shipped (every step closed), In progress (a
// live claim holds a step), Stalled, then Ready.
func planStateOf(tl planTimelineVM, rows []planStepVM, shipped bool) string {
	if tl.CanApprove {
		return planStateNeeds
	}
	for _, r := range rows {
		switch r.Word.Key {
		case wordWaiting, wordLapsed, wordUnclaimed:
			return planStateNeeds
		}
	}
	if shipped {
		return planStateShipped
	}
	for _, r := range rows {
		if r.Word.Key == wordInProgress {
			return planStateInProgress
		}
	}
	if tl.Stalled {
		return planStateStalled
	}
	return planStateReady
}

// planPageCtx is the step context of one card: the page's shared task cache
// and locked-blocker index, and the card's own rows as the ones a chip jumps
// to (a chip to another plan's step opens the task).
func planPageCtx(steps []core.Task, cache map[string]core.Task, lockedBy map[string][]string, in *planInputs, ret string) planStepCtx {
	own := make(map[string]core.Task, len(steps))
	onPage := make(map[string]bool, len(steps))
	for _, t := range steps {
		own[t.ID], onPage[t.ID] = t, true
	}
	return planStepCtx{cache: cache, onPage: onPage, unblocks: reverseEdges(own), namer: in.namer, now: in.now,
		lockedBy: lockedBy, ret: ret}
}

// finishCard fills what an open card renders beyond its head: what each of
// its gates holds (one query per open gate) and its rows grouped so a long
// run of closed steps folds away, in dependency order (groupSteps).
func (s *Service) finishCard(ctx context.Context, tl *planTimelineVM, rows []planStepVM, pc planStepCtx) error {
	if err := s.gateHolds(ctx, rows, pc); err != nil {
		return err
	}
	tl.Groups = groupSteps(rows)
	return nil
}

func (s *Service) plansList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	q, err := parsePlanQuery(r.URL.Query(), now)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	if project, scoped := q.scope(); scoped && project != "" {
		if ok, err := s.planProjectKnown(ctx, project); err != nil {
			s.serverError(w, r, err)
			return
		} else if !ok {
			msg := fmt.Sprintf("invalid project %q: no project has that slug", project)
			if lockedHere(ctx, project) {
				msg = fmt.Sprintf("invalid project %q: it is locked, and its plans stay out of this view until this browser unlocks it", project)
			}
			s.badRequest(w, r, msg)
			return
		}
	}
	data, err := s.plansBoard(ctx, q, now, wantsJSON(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "plans", pageData{Title: "Plans", Active: "plans", Data: data})
}

// planProjectKnown reports a project the request reads: registered, or
// holding a task or a note, through the request's handle.
func (s *Service) planProjectKnown(ctx context.Context, project string) (bool, error) {
	db := s.db(ctx)
	if _, ok, err := store.ProjectBySlug(ctx, db, project); err != nil || ok {
		return ok, err
	}
	if ok, err := store.TaskProjectExists(ctx, db, project); err != nil || ok {
		return ok, err
	}
	notes, err := store.NotesByTagPrefix(ctx, db, project, plans.SlugTagPrefix())
	return len(notes) > 0, err
}

// plansBoard assembles the board for state q as of now. Query budget: one
// rollup query, one note-tag query, the page inputs (tokens, agent caches,
// dropped steps, pending settlements, projects), one ListTasksForPlan per
// ACTIVE plan the filters keep (a shipped plan renders from its rollup), the
// dependency walk's cache misses, one query per open gate on an open card,
// and one transition-ledger read per project with an open card.
func (s *Service) plansBoard(ctx context.Context, q planQuery, now time.Time, withIteration bool) (plansData, error) {
	entries, err := s.planEntries(ctx)
	if err != nil {
		return plansData{}, err
	}
	in, err := s.planInputsOf(ctx, now)
	if err != nil {
		return plansData{}, err
	}
	ret := q.href()
	data := plansData{
		Window: q.Window.Key, WindowLabel: q.Window.Label, Windows: windowOptions(q.Window.Key),
		Filter: planFilterJSON{Project: q.Project, Q: q.Q}, Locked: lockedProjects(ctx), Return: ret,
	}
	inWindow := func(e planEntry) bool { return q.Window.Since.IsZero() || !e.activity().Before(q.Window.Since) }
	project, scoped := q.scope()
	menu := map[string]int{}
	var active, shipped []planEntry
	for _, e := range entries {
		if !q.matches(e.title(), e.Slug) {
			continue
		}
		isShipped := e.shipped() && !e.canApprove()
		if !isShipped || inWindow(e) {
			menu[e.Project]++
		}
		if scoped && e.Project != project {
			continue
		}
		data.Total++
		switch {
		case !isShipped:
			active = append(active, e)
		case inWindow(e):
			shipped = append(shipped, e)
			data.ShippedAll++
		default:
			data.ShippedAll++
		}
	}
	for p, n := range menu {
		data.Projects = append(data.Projects, planProjectCount{Project: p, Plans: n})
	}
	slices.SortFunc(data.Projects, func(a, b planProjectCount) int { return strings.Compare(a.Project, b.Project) })
	data.View = newPlanView(q, data.Projects, in.names)
	if data.View.Scope, err = s.projectSwitcher(ctx, data.View.Projects); err != nil {
		return plansData{}, err
	}

	// Every active plan's steps first: the dependency walk and the
	// locked-blocker index read the same in-memory set.
	steps := make(map[string][]core.Task, len(active))
	cache := map[string]core.Task{}
	for _, e := range active {
		if e.Rollup == nil {
			continue
		}
		list, err := store.ListTasksForPlan(ctx, s.db(ctx), e.Project, "", e.Slug)
		if err != nil {
			return plansData{}, err
		}
		steps[e.key()] = list
		for _, t := range list {
			cache[t.ID] = t
		}
	}
	lockedBy, err := s.lockedBlockersOf(ctx, cache)
	if err != nil {
		return plansData{}, err
	}
	groups := slices.Clone(planGroupDefs)
	for _, e := range active {
		pc := planPageCtx(steps[e.key()], cache, lockedBy, in, ret)
		tl, rows := s.planCardOf(ctx, e, steps[e.key()], pc, in)
		tl.Return = ret
		tl.State = planStateOf(tl, rows, false)
		gi := slices.IndexFunc(groups, func(g planGroupVM) bool { return g.Key == tl.State })
		switch {
		case tl.NoteOnly:
		case groups[gi].Open:
			// An open card renders its rows: what its gates hold, and its
			// groups in dependency order. On the board a row names its
			// nearest blocker only (the rows above it are the rest of the
			// chain); the plan's page names the whole chain.
			for i := range rows {
				rows[i].Chain = rows[i].Chain[:min(len(rows[i].Chain), 1)]
			}
			if err := s.finishCard(ctx, &tl, rows, pc); err != nil {
				return plansData{}, err
			}
		default:
			tl.Lazy = true
			tl.StepsURL = planHref(e.Project, e.Slug) + "&steps=1"
		}
		groups[gi].Cards = append(groups[gi].Cards, tl)
	}
	// The open cards' recent transitions: one ledger read per project.
	ledgerSteps := map[string]map[string]core.Task{}
	for _, g := range groups {
		if !g.Open {
			continue
		}
		for _, tl := range g.Cards {
			if tl.NoteOnly {
				continue
			}
			if ledgerSteps[tl.Project] == nil {
				ledgerSteps[tl.Project] = map[string]core.Task{}
			}
			for _, t := range steps[planKey(tl.Project, tl.Slug)] {
				ledgerSteps[tl.Project][t.ID] = t
			}
		}
	}
	ledgers := map[string]map[string][]planLedgerVM{}
	for p, set := range ledgerSteps {
		ledgers[p] = s.planLedger(ctx, p, set, in.namer)
	}
	byKey := make(map[string]planEntry, len(entries))
	for _, e := range entries {
		byKey[e.key()] = e
	}
	for gi := range groups {
		cards := groups[gi].Cards
		for i := range cards {
			if groups[gi].Open && !cards[i].NoteOnly {
				cards[i].Recent = ledgers[cards[i].Project][cards[i].Slug]
			}
		}
		sortPlanCards(cards)
		for _, tl := range cards {
			row := s.planRowOf(byKey[planKey(tl.Project, tl.Slug)], in, withIteration)
			row.State, row.StalledDays = tl.State, tl.StalledDays
			data.Rows = append(data.Rows, row)
		}
		switch groups[gi].Key {
		case planStateNeeds:
			data.NeedsYou = len(cards)
		case planStateInProgress:
			data.InProgress = len(cards)
		case planStateStalled:
			data.Stalled = len(cards)
		case planStateReady:
			data.Ready = len(cards)
		}
		data.Active += len(cards)
		if len(cards) > 0 {
			data.Groups = append(data.Groups, groups[gi])
		}
	}
	for _, e := range shipped {
		tl := planTimelineVM{Slug: e.Slug, Project: e.Project, Title: e.Slug, Anchor: planAnchor(e.Project, e.Slug),
			ShowProject: true, ProjectHref: in.projectTab(e.Project, e.Slug), State: planStateShipped, Return: ret,
			LastActivity: e.activity(), Dropped: in.dropped[e.ref()]}
		if e.Note != nil {
			tl.withNote(*e.Note)
		}
		if e.Rollup != nil {
			tl.Total, tl.Done = e.Rollup.Total, e.Rollup.Done
		} else {
			tl.NoteOnly = true
		}
		data.Shipped = append(data.Shipped, tl)
	}
	sortPlanCards(data.Shipped)
	for _, tl := range data.Shipped {
		row := s.planRowOf(byKey[planKey(tl.Project, tl.Slug)], in, withIteration)
		row.State = planStateShipped
		data.Rows = append(data.Rows, row)
	}
	if data.Rows == nil {
		data.Rows = []planRow{}
	}
	data.Done = len(data.Shipped)
	data.Count = len(data.Rows)
	s.planShippedMomentum(ctx, &data)
	return data, nil
}

// sortPlanCards orders a group newest activity first, by project and then
// slug on a tie, so a morph never reorders two renders of the same data. A
// star marks a card; it does not move it ahead of newer work.
func sortPlanCards(cards []planTimelineVM) {
	slices.SortStableFunc(cards, func(a, b planTimelineVM) int {
		if c := b.LastActivity.Compare(a.LastActivity); c != 0 {
			return c
		}
		if c := strings.Compare(a.Project, b.Project); c != 0 {
			return c
		}
		return strings.Compare(a.Slug, b.Slug)
	})
}

// planShippedMomentum fills the momentum surfaces of the Plans page: the
// header's monthly shipped count and the per-plan settle marks, both judged
// from the plan.shipped settlements in the current local month. Gated on the
// feature (off = not computed, zero trace) and failure-soft the momentum way
// -- an error drops the whole surface rather than rendering a wrong zero.
func (s *Service) planShippedMomentum(ctx context.Context, data *plansData) {
	if !features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		return
	}
	now := time.Now().Local()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	refs, err := store.PlansShippedSince(ctx, s.db(ctx), monthStart)
	if err != nil {
		s.logger.Warn("console: plans shipped this month", "error", err)
		return
	}
	data.ShippedMonth = &planShippedMonth{Count: len(refs)}
	shipped := make(map[string]bool, len(refs))
	for _, ref := range refs {
		shipped[planKey(ref.Project, ref.Slug)] = true
	}
	for i := range data.Rows {
		data.Rows[i].Shipped = shipped[planKey(data.Rows[i].Project, data.Rows[i].Slug)]
	}
	for i := range data.Shipped {
		data.Shipped[i].MomSettle = shipped[planKey(data.Shipped[i].Project, data.Shipped[i].Slug)]
	}
}

// planProjectsOf names the projects in which slug is a plan on db: a
// composition there (plans.CompositionProjects) or a step. Sorted, each once.
func planProjectsOf(ctx context.Context, db *sql.DB, slug string) ([]string, error) {
	notes, err := plans.CompositionProjects(ctx, db, slug)
	if err != nil {
		return nil, err
	}
	steps, err := store.PlanStepProjects(ctx, db, slug)
	if err != nil {
		return nil, err
	}
	out := slices.Concat(notes, steps)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// planDetailIn assembles one plan's page as of now: its row, its card with
// every step row (in dependency order, with the gates' holds and the recent
// transitions), the rendered body, the attached notes, and the steps as
// refs. found=false means the project holds neither a note tagged
// plan:<slug> nor a step of it on the request's handle.
func (s *Service) planDetailIn(ctx context.Context, project, slug string, now time.Time) (planDetailData, bool, error) {
	db := s.db(ctx)
	primary, others, hasNote, err := plans.CompositionIn(ctx, db, project, slug)
	if err != nil {
		return planDetailData{}, false, err
	}
	tasks, err := store.ListTasksForPlan(ctx, db, project, "", slug)
	if err != nil {
		return planDetailData{}, false, err
	}
	if !hasNote && len(tasks) == 0 {
		return planDetailData{}, false, nil
	}
	in, err := s.planInputsOf(ctx, now)
	if err != nil {
		return planDetailData{}, false, err
	}
	e := planEntry{Project: project, Slug: slug}
	if hasNote {
		e.Note = &primary
		e.NoteActivity = primary.Updated
		for _, n := range others {
			if n.Updated.After(e.NoteActivity) {
				e.NoteActivity = n.Updated
			}
		}
	}
	if len(tasks) > 0 {
		rollups, err := store.PlanRollupsForProject(ctx, db, project)
		if err != nil {
			return planDetailData{}, false, err
		}
		if i := slices.IndexFunc(rollups, func(p store.PlanRollup) bool { return p.Slug == slug }); i >= 0 {
			e.Rollup = &rollups[i]
		}
	}
	ret := planHref(project, slug)
	cache := make(map[string]core.Task, len(tasks))
	for _, t := range tasks {
		cache[t.ID] = t
	}
	lockedBy, err := s.lockedBlockersOf(ctx, cache)
	if err != nil {
		return planDetailData{}, false, err
	}
	pc := planPageCtx(tasks, cache, lockedBy, in, ret)
	tl, rows := s.planCardOf(ctx, e, tasks, pc, in)
	tl.Return = ret
	tl.State = planStateOf(tl, rows, e.shipped())
	if !tl.NoteOnly {
		if err := s.finishCard(ctx, &tl, rows, pc); err != nil {
			return planDetailData{}, false, err
		}
		tl.Recent = s.planLedger(ctx, project, cache, in.namer)[slug]
	}
	d := planDetailData{Row: s.planRowOf(e, in, true), Card: tl, CanApprove: tl.CanApprove, NoNote: !hasNote, Return: ret}
	if hasNote && !e.capture() {
		d.Lead = strings.TrimSpace(primary.Description)
	}
	d.Row.State, d.Row.StalledDays = tl.State, tl.StalledDays
	for _, n := range others {
		d.Attached = append(d.Attached, planNoteRef{
			ID: n.ID, Title: n.Title, Slug: n.Slug,
			IsAgent: slices.Contains(n.Tags, plans.TagAgent), Updated: n.Updated,
		})
	}
	if hasNote && s.cfg.Files != nil {
		if full, ferr := s.cfg.Files.Store().ReadNote(primary.FilePath); ferr != nil {
			s.logger.Warn("console: read plan body", "slug", slug, "error", ferr)
		} else {
			d.BodyText = full.Body
			d.BodyLoaded = true
			d.Body = s.renderBody(ctx, full.Body, project)
		}
	}
	for _, g := range tl.Groups {
		for _, st := range g.Steps {
			ref := planTaskRef{ID: st.ID, Title: st.Title, Status: st.Status, Word: st.Word, Holder: st.ClaimedBy}
			for _, wt := range st.Waits {
				ref.Waits = append(ref.Waits, planWaitRef(wt))
			}
			d.Tasks = append(d.Tasks, ref)
		}
	}
	return d, true, nil
}

// planScope reads a plan route's ?project=: the project ("" is the global
// scope) and whether one was named. A value that is no slug is an error.
func planScope(r *http.Request) (string, bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("project"))
	if raw == "" {
		return "", false, nil
	}
	slug, err := validate.Project(raw)
	if err != nil {
		return "", false, fmt.Errorf("invalid project %q: name a project's slug, or global for the global scope", raw)
	}
	return slug, true, nil
}

func (s *Service) planDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	project, scoped, err := planScope(r)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	query := r.URL.Query()
	switch query.Get("steps") {
	case "", "1":
	default:
		s.badRequest(w, r, fmt.Sprintf("invalid steps %q: valid values are 1 (or omit)", query.Get("steps")))
		return
	}
	if !scoped {
		where, err := planProjectsOf(ctx, s.db(ctx), slug)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		switch len(where) {
		case 0:
			if s.answerLockedPlan(w, r, slug, "", false) {
				return
			}
			s.notFound(w, r, "No plan with slug "+slug+".")
			return
		case 1:
			project = where[0]
		default:
			s.planChooser(w, r, slug, where)
			return
		}
	}
	d, found, err := s.planDetailIn(ctx, project, slug, time.Now().UTC())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		if s.answerLockedPlan(w, r, slug, project, true) {
			return
		}
		s.notFound(w, r, "No plan with slug "+slug+" in "+planProjectLabel(project)+".")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	switch {
	case query.Get("steps") == "1":
		// The board's lazy card: its steps, loaded when it is opened.
		s.renderReader(w, r, "plans", "plan-card-steps", d.Card)
		return
	case query.Get("peek") == "1":
		// The peek fragment stays for the surfaces that open plans in a
		// detail pane (search results).
		s.renderFragment(w, r, "plan", d)
		return
	case query.Get("reader") == "1":
		s.renderReader(w, r, "plans", "plan-reader", d)
		return
	}
	s.planPage(w, r, d)
}

// planChooser answers a slug that is a plan in more than one project, asked
// for without ?project=: a page (or fragment) that names each project's plan
// and links to it, and for JSON a 409 that names them, so `seam plan show`
// says which to pick. Never one page holding both plans.
func (s *Service) planChooser(w http.ResponseWriter, r *http.Request, slug string, projects []string) {
	ctx := r.Context()
	ch := planChooserVM{Slug: slug}
	labels := make([]string, 0, len(projects))
	for _, p := range projects {
		title := slug
		if n, _, ok, err := plans.CompositionIn(ctx, s.db(ctx), p, slug); err != nil {
			s.serverError(w, r, err)
			return
		} else if ok && strings.TrimSpace(n.Title) != "" {
			title = n.Title
		}
		ch.Choices = append(ch.Choices, planChoiceVM{Project: planProjectParam(p), Title: title, Href: planHref(p, slug)})
		labels = append(labels, planProjectLabel(p))
	}
	ch.Error = fmt.Sprintf("plan:%s is a plan in %d projects (%s): name one with ?project=<slug> "+
		"(seam plan show %s --project <slug>)", slug, len(projects), strings.Join(labels, ", "), slug)
	if wantsJSON(r) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": ch.Error, "ambiguous": true, "slug": slug, "choices": ch.Choices})
		return
	}
	if q := r.URL.Query(); q.Get("peek") == "1" || q.Get("reader") == "1" || q.Get("steps") == "1" {
		s.renderReader(w, r, "plans", "plan-chooser", ch)
		return
	}
	s.render(w, r, "plans", pageData{Title: "Plan " + slug, Active: "plans", Data: plansData{Chooser: &ch}})
}

// planApprove is the owner escape hatch for Claude Code bug #20397 (approve +
// immediate /clear skips the ExitPlanMode PostToolUse): it flips the plan note
// to approved and ensures the tracking task, mirroring the hook path. The
// capture is the one in ?project= when the action names it; without it, the
// only capture of the slug, and a slug captured in two projects is refused
// with both named rather than guessed.
func (s *Service) planApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	project, scoped, err := planScope(r)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	var (
		planNote core.Note
		ok       bool
	)
	if scoped {
		planNote, _, ok, err = plans.CompositionIn(ctx, s.db(ctx), project, slug)
	} else {
		planNote, ok, err = s.planToApprove(w, r, slug)
		if errors.Is(err, errPlanAnswered) {
			return
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		if s.answerLockedPlan(w, r, slug, project, scoped) {
			return
		}
		s.notFound(w, r, "No plan with slug "+slug+".")
		return
	}
	// The escape hatch only means anything for a CC capture; a composed plan has
	// no capture lifecycle to flip.
	if !slices.Contains(planNote.Tags, plans.TagPlan) {
		s.notFound(w, r, "Plan "+slug+" is not a Claude Code capture; nothing to approve.")
		return
	}
	if s.cfg.Files == nil {
		http.Error(w, "files layer unavailable", http.StatusServiceUnavailable)
		return
	}
	// Read the note and retag it under the file's lock. The capture hook rewrites
	// this same file on every plan-file save, and the retag renders the whole file
	// from what the read returned -- unserialized, an approval racing a save
	// dropped one of the two writes silently (the index upsert is by id, so
	// nothing complained). The status check is the read that decides the write, so
	// it belongs inside; an already-approved plan reports ErrNoChange, which is
	// also what keeps the event below from firing on a second approval.
	approved := false
	note, err := s.cfg.Files.MutateNote(ctx, planNote.FilePath, func(_ context.Context, note core.Note) (core.Note, error) {
		if plans.StatusFromTags(note.Tags) == plans.StatusApproved {
			return note, files.ErrNoChange
		}
		note.Tags = plans.SetStatusTag(note.Tags, plans.StatusApproved)
		note.Description = plans.NoteDescription(plans.Basename(note.Slug), plans.NoteIteration(note), plans.StatusApproved)
		note.Updated = time.Now().UTC()
		approved = true
		return note, nil
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if approved {
		s.recordPlanAction(ctx, core.EventPlanApproved, note.Project, note.ID, map[string]any{
			"basename": plans.Basename(note.Slug), "plan_slug": slug, "by": "console",
		})
	}
	task, created, err := plans.EnsureTask(ctx, s.writeDB(), note, slug, "console")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if created {
		s.recordPlanAction(ctx, core.EventTaskTransition, note.Project, task.ID, map[string]any{
			"to": string(core.TaskOpen), "created": true, "plan_slug": slug, "by": "console",
		})
	}
	if wantsJSON(r) {
		out := map[string]any{"slug": slug, "status": plans.StatusApproved, "taskCreated": created}
		if created {
			out["taskId"] = task.ID
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	// A form that names a next (the Overview's inline Approve, a card's
	// Approve) goes back there with a notice; without one, the plan's page,
	// as always (ownerNext) -- its project named when the action named it.
	msg := "Approved plan:" + slug + "."
	if !approved {
		msg = "plan:" + slug + " was already approved."
	}
	fallback := "/console/plans/" + slug
	if scoped {
		fallback = planHref(project, slug)
	}
	http.Redirect(w, r, ownerNext(r.PostFormValue("next"), fallback, "notice", msg), http.StatusSeeOther)
}

// errPlanAnswered marks a lookup that already wrote its answer.
var errPlanAnswered = errors.New("console: plan lookup answered")

// planToApprove resolves an approve that names no project: the slug's only
// capture on the request's handle. With none, the slug's composition (so a
// composed plan is told it has nothing to approve); with captures in two
// projects, a 409 naming them (errPlanAnswered).
func (s *Service) planToApprove(w http.ResponseWriter, r *http.Request, slug string) (core.Note, bool, error) {
	ctx := r.Context()
	tagged, err := store.NotesByTag(ctx, s.db(ctx), "", plans.SlugTag(slug))
	if err != nil {
		return core.Note{}, false, err
	}
	var where []string
	for _, n := range tagged {
		if slices.Contains(n.Tags, plans.TagPlan) && !slices.Contains(where, n.Project) {
			where = append(where, n.Project)
		}
	}
	switch len(where) {
	case 0:
		n, _, ok, err := plans.Composition(ctx, s.db(ctx), slug)
		return n, ok, err
	case 1:
		n, _, ok, err := plans.CompositionIn(ctx, s.db(ctx), where[0], slug)
		return n, ok, err
	}
	slices.Sort(where)
	labels := make([]string, len(where))
	for i, p := range where {
		labels[i] = planProjectLabel(p)
	}
	msg := fmt.Sprintf("plan:%s is captured in %d projects (%s): approve it from its card, or name its project "+
		"(?project=<slug>; seam plan approve %s --project <slug>)", slug, len(where), strings.Join(labels, ", "), slug)
	if wantsJSON(r) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": msg, "ambiguous": true, "slug": slug})
	} else {
		s.renderErrorPage(w, r, http.StatusConflict, "Which plan?", msg)
	}
	return core.Note{}, false, errPlanAnswered
}

// answerLockedPlan answers a lookup of a plan this request's handle does not
// hold, when the plan is in a locked project the request does not read: the
// lock, named (answerLocked), instead of a "not found" that would send the
// owner looking for a plan that exists -- the JSON refusal, or for a browser
// page the locked page (lockedItem). A plan is there when a note tagged
// plan:<slug> or a step of it is; scoped narrows the lookup to project, the
// one the request named. It reports whether it answered (a failed lookup is
// answered too, as the error it is). Nothing runs while nothing is locked,
// and a plan in no locked project falls through to the 404 as before.
func (s *Service) answerLockedPlan(w http.ResponseWriter, r *http.Request, slug, project string, scoped bool) bool {
	ctx := r.Context()
	if len(lockedProjects(ctx)) == 0 {
		return false
	}
	// The base handle hides every locked project, so the composition's notes
	// and the plan's steps are read through the write handle -- for their
	// projects alone.
	tagged, err := store.NotesByTag(ctx, s.writeDB(), "", plans.SlugTag(slug))
	if err != nil {
		s.serverError(w, r, err)
		return true
	}
	where, err := store.PlanStepProjects(ctx, s.writeDB(), slug)
	if err != nil {
		s.serverError(w, r, err)
		return true
	}
	for _, n := range tagged {
		where = append(where, n.Project)
	}
	var locked []string
	for _, p := range where {
		if (!scoped || p == project) && lockedHere(ctx, p) && !slices.Contains(locked, p) {
			locked = append(locked, p)
		}
	}
	if len(locked) == 0 {
		return false
	}
	slices.Sort(locked)
	s.answerLocked(w, r, locked[0], lockedItemPlan)
	return true
}

// answerLocked is the answer for an item -- item names its kind, a plan or a
// session -- that lives in project, a locked project this request does not
// read. A browser page gets the locked page (lockedItem). The JSON answer is
// 403, {"error", "locked": true, "project"}: the error is the lock refusal
// the tool-backed seam commands and the MCP tools carry (access.View.Refusal:
// the rule on one line, the remedy on the next, worded for the socket or the
// TCP key), so a console-backed command says what they say. Nothing is
// filed: a console read is not an access request. A browser's own JSON gets
// the console unlock instead (lockedProject), the remedy a browser has.
func (s *Service) answerLocked(w http.ResponseWriter, r *http.Request, project, item string) {
	if !wantsJSON(r) {
		s.lockedItem(w, r, project, item)
		return
	}
	if s.classify(r) == credCookie {
		s.lockedProject(w, r, project)
		return
	}
	msg := access.Refusal(access.WithLockRequester(r.Context(), nil), project).Error()
	writeJSON(w, http.StatusForbidden, map[string]any{"error": msg, "locked": true, "project": project})
}

// planNoteActivity indexes the newest note stamp per (project, slug) across a
// set of plan-tagged notes, so each row's recency can reflect its whole
// composition. Keyed by (project, slug) because that pair -- not the slug alone
// -- identifies a plan: two projects may each run a plan of the same name.
func planNoteActivity(notes []core.Note) map[string]time.Time {
	act := make(map[string]time.Time, len(notes))
	for _, n := range notes {
		slug := plans.SlugFromTags(n.Tags)
		if slug == "" {
			continue
		}
		key := planKey(n.Project, slug)
		if cur, ok := act[key]; !ok || n.Updated.After(cur) {
			act[key] = n.Updated
		}
	}
	return act
}

// planAgentCounts counts agent-cache notes per plan, by (project, slug)
// (planKey), one query for the whole list.
func (s *Service) planAgentCounts(ctx context.Context) (map[string]int, error) {
	agents, err := store.NotesByTag(ctx, s.db(ctx), "", plans.TagAgent)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(agents))
	for _, a := range agents {
		if slug := plans.SlugFromTags(a.Tags); slug != "" {
			counts[planKey(a.Project, slug)]++
		}
	}
	return counts, nil
}

// planRow projects a plan's primary note into a display row, its steps read
// from the note's own project. The capture-only columns (basename,
// iteration) are read from the cc-plan note's frontmatter (best-effort);
// composed primaries carry neither. agentCount is keyed by planKey.
//
// noteActivity is the newest stamp across the composition's OTHER notes (zero if
// unknown, which degrades to the primary's own); planRow folds in the step tasks
// itself, since it already loads them. See planRow.Updated.
func (s *Service) planRow(ctx context.Context, n core.Note, agentCount map[string]int, tokens map[store.PlanRef]store.PlanTokenRollup, noteActivity time.Time) planRow {
	slug := plans.SlugFromTags(n.Tags)
	isCapture := slices.Contains(n.Tags, plans.TagPlan)
	row := planRow{
		NoteID: n.ID, Slug: slug, Source: planSourceComposed,
		Title: n.Title, Project: n.Project, Href: planHref(n.Project, slug),
		Status: plans.StatusFromTags(n.Tags), Favorite: n.Favorite,
		Agents:  agentCount[planKey(n.Project, slug)],
		Updated: n.Updated,
	}
	if tr, ok := tokens[store.PlanRef{Project: n.Project, Slug: slug}]; ok {
		row.Tokens = tr.Tokens
		row.TokenSessions = tr.Sessions
		row.TokenUnreported = tr.Unreported
		row.TokenShared = tr.Shared
	}
	if noteActivity.After(row.Updated) {
		row.Updated = noteActivity
	}
	if isCapture {
		row.Source = planSourceCapture
		row.Basename = plans.Basename(n.Slug)
		if s.cfg.Files != nil {
			if full, err := s.cfg.Files.Store().ReadNote(n.FilePath); err == nil {
				row.Iteration = plans.NoteIteration(full)
			}
		}
	}
	if tasks, err := store.ListTasksForPlan(ctx, s.db(ctx), n.Project, "", slug); err == nil {
		row.TasksTotal = len(tasks)
		for _, t := range tasks {
			switch t.Status {
			case core.TaskDone:
				row.TasksDone++
			case core.TaskDropped:
				// A dropped step is closed: the plan it ends is complete.
				row.TasksDone++
				row.TasksDropped++
			case core.TaskInProgress:
				row.TasksWIP++
			case core.TaskOpen:
				row.TasksOpen++
			}
			// Claiming or closing a step is activity even when no note moves.
			if t.UpdatedAt.After(row.Updated) {
				row.Updated = t.UpdatedAt
			}
		}
	}
	row.Phase = planPhase(row)
	return row
}

// planPhase buckets a plan by the progress of its step tasks. A step in progress
// wins outright (matching "has an in-progress task"). Otherwise the plan is done
// when it is terminal -- an abandoned, shipped or merged capture, or every step
// closed (done or dropped) with none open -- and ready when open steps remain
// or it has not started yet.
func planPhase(r planRow) string {
	switch {
	case r.TasksWIP > 0:
		return planPhaseInProgress
	case r.Status == plans.StatusAbandoned, r.Status == plans.StatusShipped, r.Status == plans.StatusMerged:
		return planPhaseDone
	case r.TasksTotal > 0 && r.TasksOpen == 0:
		return planPhaseDone
	default:
		return planPhaseReady
	}
}

// phaseRows filters a newest-first plan list to one phase, preserving order.
func phaseRows(rows []planRow, phase string) []planRow {
	out := make([]planRow, 0, len(rows))
	for _, r := range rows {
		if r.Phase == phase {
			out = append(out, r)
		}
	}
	return out
}

// recordPlanAction appends a console-attributed plan event (best-effort).
func (s *Service) recordPlanAction(ctx context.Context, kind core.EventKind, project, itemID string, payload map[string]any) {
	if s.cfg.Events == nil {
		return
	}
	if _, err := s.eventLog(ctx).Record(ctx, core.Event{
		Kind: kind, ProjectSlug: project, ItemID: itemID, Payload: payload,
	}); err != nil {
		s.logger.Warn("console: record plan action", "kind", kind, "error", err)
	}
}

// planTone maps a plan status to a badge tone: approved and shipped green,
// presented brand-ish accent, draft amber, abandoned and merged neutral -- a
// merged capture reached no verdict of its own, it was filed under another
// plan, so it reads as settled rather than as an outcome.
func planTone(status string) string {
	switch status {
	case plans.StatusApproved, plans.StatusShipped:
		return "ok"
	case plans.StatusPresented:
		return "accent"
	case plans.StatusDraft:
		return "warn"
	default: // abandoned, merged
		return ""
	}
}
