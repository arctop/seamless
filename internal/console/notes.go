package console

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// noteSortKeys are the accepted ?sort values on the notes list.
var noteSortKeys = []string{"recent", "name", "favorites"}

// libDefaultSort is both library screens' default ?sort.
const libDefaultSort = "recent"

// libQuery is a library screen's state as its URL carries it: the sort, the
// text filter, the grouping and the filters. Every link the page draws -- a
// rail item, a sort, a filter, the star's and the archive's way back -- is
// this state with at most one part changed, so no choice drops another.
type libQuery struct {
	Sort string // a sort key the screen accepts
	Q    string // the text filter, trimmed, as typed
	// Group is "project" for the project tree, "" for one list in the sort's
	// order (the default). Sorting by name always draws the tree.
	Group string
	// Kind is a core memory kind, "" for every kind (Memories only).
	Kind string
	// Project is "", "global" (the shared scope), or a project slug.
	Project string
	// Fav keeps only starred items.
	Fav bool
	// Surfaced is "never" to keep only memories that never reached an
	// agent, "" for all (Memories only).
	Surfaced string
}

// parseLibQuery reads a library screen's state from its URL. Every part is
// strict, as ?sort always was: a value the screen cannot read is an error
// naming the values it takes, never a quiet default. memories adds the two
// filters only the Memories screen has (kind, surfaced); Notes ignores them.
func parseLibQuery(v url.Values, sortKeys []string, memories bool) (libQuery, error) {
	q := libQuery{Sort: v.Get("sort"), Q: strings.TrimSpace(v.Get("q"))}
	if q.Sort == "" {
		q.Sort = libDefaultSort
	}
	if !slices.Contains(sortKeys, q.Sort) {
		return q, fmt.Errorf("invalid sort %q: valid values are %s", q.Sort, strings.Join(sortKeys, ", "))
	}
	switch g := v.Get("group"); g {
	case "", "none":
	case "project":
		q.Group = g
	default:
		return q, fmt.Errorf("invalid group %q: valid values are none, project", g)
	}
	if p := strings.TrimSpace(v.Get("project")); p != "" {
		slug, err := validate.Project(p)
		if err != nil {
			return q, fmt.Errorf("invalid project %q: name a project's slug, or global for the global scope", p)
		}
		q.Project = slug
		if slug == "" {
			q.Project = validate.GlobalProject
		}
	}
	switch f := v.Get("fav"); f {
	case "":
	case "1":
		q.Fav = true
	default:
		return q, fmt.Errorf("invalid fav %q: valid values are 1 (or omit)", f)
	}
	if !memories {
		return q, nil
	}
	if k := v.Get("kind"); k != "" {
		if !core.MemoryKind(k).Valid() {
			kinds := make([]string, 0, len(core.MemoryKinds))
			for _, mk := range core.MemoryKinds {
				kinds = append(kinds, string(mk))
			}
			return q, fmt.Errorf("invalid kind %q: valid values are %s", k, strings.Join(kinds, ", "))
		}
		q.Kind = k
	}
	switch sf := v.Get("surfaced"); sf {
	case "":
	case "never":
		q.Surfaced = sf
	default:
		return q, fmt.Errorf("invalid surfaced %q: valid values are never (or omit)", sf)
	}
	return q, nil
}

// parseLibRequest is parseLibQuery for a request; ok=false means the 400 was
// already written.
func (s *Service) parseLibRequest(w http.ResponseWriter, r *http.Request, sortKeys []string, memories bool) (libQuery, bool) {
	q, err := parseLibQuery(r.URL.Query(), sortKeys, memories)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return libQuery{}, false
	}
	return q, true
}

// values is the state's non-default parts; forceSort names the sort even at
// its default (the sort menu's links say which sort they are).
func (q libQuery) values(forceSort bool) url.Values {
	v := url.Values{}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if forceSort || q.Sort != libDefaultSort {
		v.Set("sort", q.Sort)
	}
	if q.Group != "" {
		v.Set("group", q.Group)
	}
	if q.Kind != "" {
		v.Set("kind", q.Kind)
	}
	if q.Project != "" {
		v.Set("project", q.Project)
	}
	if q.Fav {
		v.Set("fav", "1")
	}
	if q.Surfaced != "" {
		v.Set("surfaced", q.Surfaced)
	}
	return v
}

// QS is the state as a ?query suffix, "" when every part is at its default.
func (q libQuery) QS() string { return encodeQS(q.values(false)) }

// href is path with the state's query.
func (q libQuery) href(path string) string { return path + q.QS() }

func encodeQS(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// tree reports whether the rail groups by project: asked for, or the Name
// sort, whose A to Z reads best inside each project.
func (q libQuery) tree() bool { return q.Sort == "name" || q.Group == "project" }

// narrowed reports a filter beyond the text one.
func (q libQuery) narrowed() bool {
	return q.Kind != "" || q.Project != "" || q.Fav || q.Surfaced != ""
}

// inScope reports whether an item in project passes the Project filter.
func (q libQuery) inScope(project string) bool {
	switch q.Project {
	case "":
		return true
	case validate.GlobalProject:
		return project == ""
	default:
		return project == q.Project
	}
}

// libLink is one choice a library control offers: a menu entry, a toggle.
type libLink struct {
	Label string
	Href  string
	Title string
	Count int
	On    bool
}

// libField is one hidden input the text filter's form carries, so typing
// keeps every other part of the state.
type libField struct{ Name, Value string }

// libView is what a library page draws beyond its list: its controls' links,
// the Project menu, and the words its header and rail use. HTML only.
type libView struct {
	// Flat is one list in the sort's order; else the project tree.
	Flat bool
	// AlwaysTree is the Name sort, which keeps the tree whatever the
	// grouping, so the Group control is not offered.
	AlwaysTree bool
	// Filtered is any filter, the text one included; Narrowed one beyond it.
	Filtered, Narrowed bool
	// OrderHint names the one list's order ("Newest first").
	OrderHint string
	// Sorts maps each sort key to its link, on the reader the page shows.
	Sorts map[string]string
	// GroupNone and GroupProject are the Group control's two links.
	GroupNone, GroupProject string
	// Projects is the Project menu, ProjectLabel its current choice.
	Projects     []libLink
	ProjectLabel string
	// FavLink is the Starred toggle.
	FavLink libLink
	// ClearHref drops every filter and keeps the sort and the grouping;
	// SearchHref looks for the text across everything, on Search.
	ClearHref, SearchHref string
	// Keep is the text filter's hidden inputs.
	Keep []libField
}

// libOrderHints name a flat rail's order, by sort key.
var libOrderHints = map[string]string{
	"recent": "Newest first", "reach": "Most surfaced first", "utility": "Highest utility first",
	"favorites": "Starred first",
}

// newLibView draws the controls for state q on a page whose list lives at
// list and whose reader shows base (list itself when nothing is open). A
// choice that changes what the list holds (a filter, Clear) goes to the list,
// which opens its first match; a choice that only reorders it (a sort, the
// grouping) stays on the item being read.
func newLibView(q libQuery, list, base string, sortKeys []string) libView {
	v := libView{
		Flat: !q.tree(), AlwaysTree: q.Sort == "name",
		Filtered: q.Q != "" || q.narrowed(), Narrowed: q.narrowed(),
		OrderHint: libOrderHints[q.Sort], Sorts: map[string]string{},
	}
	for _, key := range sortKeys {
		o := q
		o.Sort = key
		v.Sorts[key] = base + encodeQS(o.values(true))
	}
	none, proj := q, q
	none.Group, proj.Group = "", "project"
	v.GroupNone, v.GroupProject = none.href(base), proj.href(base)
	fav := q
	fav.Fav = !q.Fav
	v.FavLink = libLink{Label: "Starred", Href: fav.href(list), On: q.Fav}
	v.ClearHref = libQuery{Sort: q.Sort, Group: q.Group}.href(list)
	if q.Q != "" {
		v.SearchHref = "/console/search?q=" + url.QueryEscape(q.Q)
	}
	keep := q.values(false)
	keep.Del("q")
	names := make([]string, 0, len(keep))
	for name := range keep {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v.Keep = append(v.Keep, libField{Name: name, Value: keep.Get(name)})
	}
	return v
}

// libProjectMenu lists the Project filter's choices: every project, then each
// scope the screen holds (global first), with how many items match the other
// filters there. The chosen scope stays listed when nothing matches in it, so
// the menu always names the current choice. It returns the menu and that
// choice's label.
func libProjectMenu(q libQuery, counts map[string]int, names map[string]string, list, globalLabel string) ([]libLink, string) {
	all := q
	all.Project = ""
	menu := []libLink{{Label: "All projects", Href: all.href(list), On: q.Project == ""}}
	label := "All"
	chosen := q.Project
	if chosen == validate.GlobalProject {
		chosen = ""
	}
	scopes := make([]string, 0, len(counts)+1)
	for p := range counts {
		scopes = append(scopes, p)
	}
	if q.Project != "" && !slices.Contains(scopes, chosen) {
		scopes = append(scopes, chosen)
	}
	sortScopes(scopes)
	for _, p := range scopes {
		o := q
		o.Project = p
		lbl, title := p, names[p]
		if p == "" {
			o.Project, lbl, title = validate.GlobalProject, globalLabel, ""
		}
		if title == p {
			title = ""
		}
		on := q.Project != "" && p == chosen
		if on {
			label = lbl
		}
		menu = append(menu, libLink{Label: lbl, Href: o.href(list), Title: title, Count: counts[p], On: on})
	}
	return menu, label
}

// sortScopes orders project scopes: global ("") first, then by slug.
func sortScopes(scopes []string) {
	sort.Slice(scopes, func(i, j int) bool {
		if (scopes[i] == "") != (scopes[j] == "") {
			return scopes[i] == ""
		}
		return scopes[i] < scopes[j]
	})
}

// projectNames maps each project slug the request reads to its display name,
// for the text filter: typing a project's name narrows to it.
func (s *Service) projectNames(ctx context.Context) (map[string]string, error) {
	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.Slug] = p.Name
	}
	return names, nil
}

// scopeMatches reports whether a lowercased text filter names an item's
// project: its slug, its display name, or "global" for the shared scope.
func scopeMatches(project, name, q string) bool {
	if project == "" {
		return strings.Contains(validate.GlobalProject, q)
	}
	return strings.Contains(strings.ToLower(project), q) || (name != "" && strings.Contains(strings.ToLower(name), q))
}

// noteRow is a display projection of a note for the browser.
type noteRow struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Project     string    `json:"project"`
	Tags        []string  `json:"tags,omitempty"`
	Model       string    `json:"model,omitempty"`   // producing model, verbatim
	Account     string    `json:"account,omitempty"` // restricted account that wrote the content ("" = owner)
	Favorite    bool      `json:"favorite,omitempty"`
	Updated     time.Time `json:"updated"`
}

// ScopeLabel names the row's scope as the rail does.
func (n noteRow) ScopeLabel() string {
	if n.Project == "" {
		return "Global inbox"
	}
	return n.Project
}

// noteProjectGroup is one project's notes, for the grouped browser (project ""
// is global).
type noteProjectGroup struct {
	Project string    `json:"project"`
	Count   int       `json:"count"`
	Notes   []noteRow `json:"notes"`
	// All marks the one list the HTML rail draws when it does not group.
	All bool `json:"-"`
}

// notesData is the payload for the Notes library screen. The JSON answer
// keeps its project groups whatever the rail draws; Rail, the Selected/QS
// fields and the libView drive the HTML page only.
type notesData struct {
	Groups []noteProjectGroup `json:"groups"`
	Count  int                `json:"count"`
	// Total is every note the request reads, whatever the filters: the
	// header's "3 of 718".
	Total int    `json:"total"`
	Query string `json:"query,omitempty"`
	Sort  string `json:"sort"`
	// The other filters, as the URL gave them (absent at their defaults).
	Group   string `json:"group,omitempty"`
	Project string `json:"project,omitempty"`
	Fav     bool   `json:"fav,omitempty"`
	// Rail is what the HTML rail draws: Groups, or one list.
	Rail []noteProjectGroup `json:"-"`
	// Selected is the note open in the reader: the requested one on a
	// /console/notes/{id} page, or the first match on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *noteReader `json:"-"`
	SelectedAuto bool        `json:"-"`
	// QS is the library state rail links carry, so a selection change keeps
	// every filter ("" when all are at their defaults).
	QS      string `json:"-"`
	libView `json:"-"`
}

// notesPage assembles the notes list for the given library state.
func (s *Service) notesPage(ctx context.Context, lq libQuery) (notesData, error) {
	q := strings.ToLower(lq.Q)
	notes, err := store.ListNotes(ctx, s.db(ctx))
	if err != nil {
		return notesData{}, err
	}
	names, err := s.projectNames(ctx)
	if err != nil {
		return notesData{}, err
	}
	byProject := map[string][]noteRow{}
	var flat []noteRow
	projectCounts := map[string]int{}
	favs := 0
	for _, n := range notes {
		row := noteRow{
			ID: n.ID, Title: n.Title, Description: n.Description,
			Project: n.Project, Tags: n.Tags, Model: n.Model, Account: n.Account, Favorite: n.Favorite,
			Updated: n.Updated,
		}
		// Each menu counts what its own choice would show: the other
		// filters applied, its own left open.
		text, scope, fav := noteMatches(row, q, names[n.Project]), lq.inScope(n.Project), !lq.Fav || n.Favorite
		if text && fav {
			projectCounts[n.Project]++
		}
		if text && scope && n.Favorite {
			favs++
		}
		if !text || !scope || !fav {
			continue
		}
		byProject[n.Project] = append(byProject[n.Project], row)
		flat = append(flat, row)
	}
	data := notesData{
		Groups: buildNoteGroups(byProject, lq.Sort), Count: len(flat), Total: len(notes),
		Query: lq.Q, Sort: lq.Sort, Group: lq.Group, Project: lq.Project, Fav: lq.Fav,
		QS: lq.QS(),
	}
	data.Rail = data.Groups
	if !lq.tree() {
		data.Rail = nil
		if len(flat) > 0 {
			sortNoteRows(flat, lq.Sort)
			data.Rail = []noteProjectGroup{{Count: len(flat), Notes: flat, All: true}}
		}
	}
	data.libView = newLibView(lq, "/console/notes", "/console/notes", noteSortKeys)
	data.Projects, data.ProjectLabel = libProjectMenu(lq, projectCounts, names, "/console/notes", "Global inbox")
	data.FavLink.Count = favs
	return data, nil
}

// selectReader opens sel in the reader (auto: the list URL chose it) and
// redraws the controls that stay on the item being read.
func (d *notesData) selectReader(sel *noteReader, auto bool, lq libQuery) {
	d.Selected, d.SelectedAuto = sel, auto
	v := newLibView(lq, "/console/notes", "/console/notes/"+sel.ID, noteSortKeys)
	d.Sorts, d.GroupNone, d.GroupProject = v.Sorts, v.GroupNone, v.GroupProject
}

func (s *Service) notesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lq, ok := s.parseLibRequest(w, r, noteSortKeys, false)
	if !ok {
		return
	}
	data, err := s.notesPage(ctx, lq)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the first match in the reader.
	if !wantsJSON(r) {
		if id := firstNoteID(data); id != "" {
			d, found, derr := s.noteDetailByID(ctx, id)
			if derr != nil {
				s.serverError(w, r, derr)
				return
			}
			if found {
				data.selectReader(s.noteReaderOf(ctx, d, lq), true, lq)
			}
		}
	}
	s.render(w, r, "notes", pageData{Title: "Notes", Active: "notes", Data: data})
}

// noteLibraryPage answers a note's own URL in HTML: the reader fragment for
// the in-place swap (?reader=1), or the Notes screen with the note open, the
// library's state read from the URL either way.
func (s *Service) noteLibraryPage(w http.ResponseWriter, r *http.Request, d noteDetail) {
	ctx := r.Context()
	lq, ok := s.parseLibRequest(w, r, noteSortKeys, false)
	if !ok {
		return
	}
	sel := s.noteReaderOf(ctx, d, lq)
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "notes", "note-reader", sel)
		return
	}
	data, err := s.notesPage(ctx, lq)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.selectReader(sel, false, lq)
	s.render(w, r, "notes", pageData{Title: "Note " + d.Title, Active: "notes", Data: data})
}

// firstNoteID picks the reader's default selection on the list URL: the one
// list's first row, or in the tree the most recently updated note ("" when
// nothing matched).
func firstNoteID(data notesData) string {
	if len(data.Rail) == 1 && data.Rail[0].All {
		return data.Rail[0].Notes[0].ID
	}
	return newestNoteID(data.Groups)
}

// newestNoteID is the most recently updated note across every group ("" when
// the list is empty).
func newestNoteID(groups []noteProjectGroup) string {
	var id string
	var newest time.Time
	for _, g := range groups {
		for _, n := range g.Notes {
			if id == "" || n.Updated.After(newest) {
				id, newest = n.ID, n.Updated
			}
		}
	}
	return id
}

// noteMatches reports whether a note row satisfies the ?q text filter (empty q
// matches all): a case-insensitive substring of title, description, any tag,
// or its project's slug or name (projectName).
func noteMatches(row noteRow, q, projectName string) bool {
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(row.Title), q) ||
		strings.Contains(strings.ToLower(row.Description), q) {
		return true
	}
	for _, t := range row.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	return scopeMatches(row.Project, projectName, q)
}

// buildNoteGroups orders the project->notes map: global ("") first, then
// projects alphabetically, each group's notes in sortNoteRows' order.
func buildNoteGroups(byProject map[string][]noteRow, sortKey string) []noteProjectGroup {
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sortScopes(projects)
	groups := make([]noteProjectGroup, 0, len(projects))
	for _, p := range projects {
		rows := byProject[p]
		sortNoteRows(rows, sortKey)
		groups = append(groups, noteProjectGroup{Project: p, Count: len(rows), Notes: rows})
	}
	return groups
}

// sortNoteRows orders notes newest-updated first for sort=recent, by title for
// sort=name, or floats starred rows (keeping newest-first within each
// partition) for sort=favorites.
func sortNoteRows(rows []noteRow, sortKey string) {
	// Establish recency explicitly rather than relying on a store query's
	// incidental input order. The other modes are stable overrides of it.
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].Updated.Equal(rows[j].Updated) {
			return rows[i].Updated.After(rows[j].Updated)
		}
		return rows[i].ID > rows[j].ID
	})
	switch sortKey {
	case "name":
		sort.SliceStable(rows, func(i, j int) bool {
			return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title)
		})
	case "favorites":
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].Favorite && !rows[j].Favorite
		})
	}
}

// noteReader is the reader pane's view of one note: its detail plus what only
// the library draws -- its tags as a person reads them, each plan it belongs
// to linked with the plan's status, and the page state its star returns to.
type noteReader struct {
	noteDetail
	Tagged noteTags
	// QS is the library state the star carries back.
	QS string
}

// noteReaderOf builds the reader for d under library state lq.
func (s *Service) noteReaderOf(ctx context.Context, d noteDetail, lq libQuery) *noteReader {
	tagged := d.TagView()
	for i := range tagged.Plans {
		s.planStatusOf(ctx, &tagged.Plans[i])
	}
	return &noteReader{noteDetail: d, Tagged: tagged, QS: lq.QS()}
}

// noteTags is a note's tags as a person reads them: the plans it belongs to
// (plan:<slug>, each a link to the plan), the tags written for people, and
// the ones agents and hooks stamp for software, which fold behind "+N".
type noteTags struct {
	Plans   []notePlan
	Tags    []string
	Machine []string
}

// notePlan is one plan:<slug> tag as a link to the plan, with the plan's
// status when the reader looked it up ("" in a peek, which does not).
type notePlan struct {
	Slug   string
	Href   string
	Status string
	Tone   string
	Title  string
}

// machineTag reports a tag written for software rather than for a person:
// the provenance stamp every agent write carries (created-by:*), and a
// captured plan's lifecycle and marker tags (internal/plans).
func machineTag(tag string) bool {
	return strings.HasPrefix(tag, "created-by:") || plans.StatusFromTags([]string{tag}) != "" ||
		tag == plans.TagPlan || tag == plans.TagAgent
}

// TagView splits the note's tags for display (see noteTags). Pure, so the
// peek fragment can call it from its template.
func (d noteDetail) TagView() noteTags {
	var out noteTags
	for _, t := range d.Tags {
		if slug := plans.SlugFromTags([]string{t}); slug != "" {
			out.Plans = append(out.Plans, notePlan{Slug: slug, Href: "/console/plans/" + url.PathEscape(slug)})
			continue
		}
		if machineTag(t) {
			out.Machine = append(out.Machine, t)
			continue
		}
		out.Tags = append(out.Tags, t)
	}
	return out
}

// planPhaseWords name a plan's phase where it has no status of its own (a
// composed plan; a captured one carries plan-status:<v>).
var planPhaseWords = map[string]string{
	planPhaseInProgress: "in progress", planPhaseReady: "ready", planPhaseDone: "done",
}

// planStatusOf fills in a plan link's status the way the Plans screen reads
// it: a captured plan's own status, else its phase from its steps. A plan the
// request cannot resolve keeps its bare link.
func (s *Service) planStatusOf(ctx context.Context, p *notePlan) {
	primary, _, ok, err := plans.Composition(ctx, s.db(ctx), p.Slug)
	if err != nil {
		s.logger.Warn("console: note plan status", "slug", p.Slug, "error", err)
		return
	}
	if !ok {
		return
	}
	row := s.planRow(ctx, primary, nil, nil, time.Time{})
	p.Title = row.Title
	if row.TasksTotal > 0 {
		p.Title = fmt.Sprintf("%s: %d of %d steps done", row.Title, row.TasksDone, row.TasksTotal)
	}
	if row.Status != "" {
		p.Status, p.Tone = row.Status, planTone(row.Status)
		return
	}
	p.Status = planPhaseWords[row.Phase]
	switch row.Phase {
	case planPhaseDone:
		p.Tone = "ok"
	case planPhaseInProgress:
		p.Tone = "accent"
	}
}

// notesListPath is the Notes screen's list URL.
const notesListPath = "/console/notes"

// noteDeletion is the reader's delete control: Offered when Delete is drawn,
// else Blocked (why not, a sentence the Held screen settles). Narrative names
// the plans this note is the narrative of (narrativesOf): their confirmation
// is a box to tick, since a form posts on the first press without script.
type noteDeletion struct {
	Offered   bool
	Narrative []string
	Blocked   string
}

// deleteViewOf decides the delete control for n, through the request's
// handle. A narrative that cannot be resolved counts as one, so the reader
// asks for the confirmation rather than skip it.
func (s *Service) deleteViewOf(ctx context.Context, n core.Note) noteDeletion {
	if s.cfg.Files == nil {
		return noteDeletion{}
	}
	if why := s.heldBlock(ctx, n.ID, n.FilePath, false); why != "" {
		return noteDeletion{Blocked: why}
	}
	narr, err := s.narrativesOf(ctx, n)
	if err != nil {
		s.logger.Warn("console: note narrative", "id", n.ID, "error", err)
	}
	return noteDeletion{Offered: true, Narrative: narr}
}

// narrativesOf lists the plans n is the narrative of: each plan:<slug> tag
// whose composition's primary note is n (plans.Composition, the note the
// Plans screen and agents inheriting the plan read first). Read through the
// request's handle, which can only widen the list: a primary hidden in a
// locked project would have won over n. On a failed read the slug counts,
// alongside the error.
func (s *Service) narrativesOf(ctx context.Context, n core.Note) ([]string, error) {
	var out []string
	var errs []error
	for _, t := range n.Tags {
		slug := plans.SlugFromTags([]string{t})
		if slug == "" || slices.Contains(out, slug) {
			continue
		}
		primary, _, ok, err := plans.Composition(ctx, s.db(ctx), slug)
		if err != nil {
			errs = append(errs, err)
			out = append(out, slug)
			continue
		}
		if ok && primary.ID == n.ID {
			out = append(out, slug)
		}
	}
	return out, errors.Join(errs...)
}

// noteDelete deletes a note for good, as notes_delete does: files.Manager.Remove
// drops the file and its index rows, here under the file's lock so the read
// that decides the delete cannot race an agent's append, and note.written
// records it with deleted and by=console. The browser returns to the Notes
// list the form names in next, its state kept.
//
// Refused, with nothing removed: a plan's narrative without its explicit
// confirmation, narrative=<slug> for every plan it narrates (the in-page
// confirm cannot guard it, since the form posts on the first press without
// script), and a note the Held screen holds (heldBlock). A refusal returns to
// the note's own page.
func (s *Service) noteDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Files == nil {
		s.serverError(w, r, errNoFiles)
		return
	}
	id := r.PathValue("id")
	next := r.PostFormValue("next")
	back := libBack(next, notesListPath)
	idx, ok, err := store.NoteByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No note with id "+id+".")
		return
	}
	confirmed := r.PostForm["narrative"]
	err = s.cfg.Files.Mutate(ctx, idx.FilePath, func(ctx context.Context) error {
		note, rerr := s.cfg.Files.Store().ReadNote(idx.FilePath)
		if rerr != nil {
			return rerr
		}
		if note.ID != idx.ID {
			return errNoteMoved
		}
		if why := s.heldBlock(ctx, note.ID, note.FilePath, false); why != "" {
			return &libRefusal{msg: why}
		}
		narr, nerr := s.narrativesOf(ctx, note)
		if nerr != nil {
			return nerr
		}
		var unconfirmed []string
		for _, slug := range narr {
			if !slices.Contains(confirmed, slug) {
				unconfirmed = append(unconfirmed, slug)
			}
		}
		if len(unconfirmed) > 0 {
			return &libRefusal{
				msg:   narrativeRefusal(unconfirmed),
				extra: map[string]any{"narrativeOf": unconfirmed},
			}
		}
		return s.cfg.Files.Remove(ctx, idx.FilePath)
	})
	var refusal *libRefusal
	switch {
	case errors.As(err, &refusal):
		self := libBack(next, notesListPath)
		self.Path = notesListPath + "/" + idx.ID
		libRefuse(w, r, self, refusal)
		return
	case errors.Is(err, errNoteMoved), errors.Is(err, fs.ErrNotExist):
		s.notFound(w, r, "No note with id "+id+".")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	// idx.Project: the project the note sat in, as notes_delete records it.
	if s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind: core.EventNoteWritten, ProjectSlug: idx.Project, ItemID: idx.ID,
			Payload: map[string]any{"slug": idx.Slug, "deleted": true, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record note delete event", "error", err)
		}
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": idx.ID})
		return
	}
	http.Redirect(w, r, withNotice(back, "Deleted "+idx.Title+"."), http.StatusSeeOther)
}

// errNoteMoved: the file an index row named now holds another note.
var errNoteMoved = errors.New("console: that file holds another note now")

// narrativeRefusal says which plans' narrative a delete was not confirmed for.
func narrativeRefusal(slugs []string) string {
	named := make([]string, len(slugs))
	for i, s := range slugs {
		named[i] = "plan:" + s
	}
	return "Not deleted: this is " + strings.Join(named, " and ") + "'s narrative, which agents that pick up the plan read first. " +
		"Tick the box beside Delete to confirm it."
}
