package console

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// noteSortKeys are the accepted ?sort values on the notes list.
var noteSortKeys = []string{"recent", "name", "favorites"}

// notePageSize is how many rows the Notes list draws at a time, and how many
// more its Show more link adds (plan:console-ux-pass UX 29).
const notePageSize = 50

// noteChipLimit is how many tag chips the bar offers: the tags most notes in
// view carry. A tag past them is still found by typing it in the filter.
const noteChipLimit = 12

// noteRowLabels is how many of a note's tags written for people its row
// shows; the rest are counted.
const noteRowLabels = 2

// noteRow is a display projection of a note for the browser. The JSON
// answer carries the fields with a json name; the rest are the list row's,
// filled for the rows a page draws (fillNoteRows).
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
	// Plans are the plans the note is part of, each with its status;
	// Labels up to noteRowLabels of its tags written for people, and
	// MoreLabels how many more it has; Cache marks an agent working copy;
	// Words is its body's length (0 when its file cannot be read).
	Plans      []notePlan `json:"-"`
	Labels     []string   `json:"-"`
	MoreLabels int        `json:"-"`
	Cache      bool       `json:"-"`
	Words      int        `json:"-"`
	file       string
}

// ScopeLabel names the row's scope as the list's project chip does.
func (n noteRow) ScopeLabel() string {
	if n.Project == "" {
		return "Global inbox"
	}
	return n.Project
}

// WordsLabel is the row's length in words ("1,180 words"), "" when unknown.
func (n noteRow) WordsLabel() string {
	if n.Words <= 0 {
		return ""
	}
	return groupDigits(n.Words) + " " + nounOf(n.Words, "word", "words")
}

// noteProjectGroup is one project's notes, for the JSON answer (project ""
// is global).
type noteProjectGroup struct {
	Project string    `json:"project"`
	Count   int       `json:"count"`
	Notes   []noteRow `json:"notes"`
}

// notesData is the payload for the Notes screen. The JSON answer is every
// match of the filters, unpaged, in its project groups, the agent working
// copies included, whatever the page draws; the view, the list and the
// controls drive the HTML page only (plan:console-ux-pass UX 29).
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
	// Tag and Plans are the tag chips' filters (noteView).
	Tag   string `json:"tag,omitempty"`
	Plans bool   `json:"plans,omitempty"`
	// List is what the page draws; TagChips are the chips under the bar and
	// Fold the agent working copies' line.
	List     noteList  `json:"-"`
	TagChips []libLink `json:"-"`
	Fold     noteFold  `json:"-"`
	libView  `json:"-"`
	// Item is a note's own page (itempage_note.go): set, the screen draws it
	// instead of the list. HTML only.
	Item *itemPage `json:"-"`
}

// noteView is the Notes list's own state beyond the library's (libQuery),
// read from the URL as strictly: ?tag= keeps the notes with that tag (a tag
// chip), ?plans=1 the notes tagged plan:<slug> (the "plan narratives" chip),
// ?caches=1 lists the agent working copies the list folds away by default,
// and ?show= is how many rows it draws.
type noteView struct {
	Tag    string
	Plans  bool
	Caches bool
	// Show is the rows to draw, 0 for notePageSize.
	Show int
}

// size is how many rows the list draws.
func (v noteView) size() int {
	if v.Show > 0 {
		return v.Show
	}
	return notePageSize
}

// parseNoteView reads the list's own state from its URL. A value it cannot
// read is an error naming what it takes, as parseLibQuery's are.
func parseNoteView(q url.Values) (noteView, error) {
	v := noteView{Tag: strings.TrimSpace(q.Get("tag"))}
	for _, f := range []struct {
		key string
		on  *bool
	}{{"plans", &v.Plans}, {"caches", &v.Caches}} {
		switch raw := q.Get(f.key); raw {
		case "":
		case "1":
			*f.on = true
		default:
			return v, fmt.Errorf("invalid %s %q: valid values are 1 (or omit)", f.key, raw)
		}
	}
	if raw := q.Get("show"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return v, fmt.Errorf("invalid show %q: a number of rows, 1 or more", raw)
		}
		v.Show = n
	}
	return v, nil
}

// parseNoteRequest reads the whole Notes state from a request: the library's
// and the list's own. ok=false means the 400 was already written.
func (s *Service) parseNoteRequest(w http.ResponseWriter, r *http.Request) (libQuery, noteView, bool) {
	lq, ok := s.parseLibRequest(w, r, noteSortKeys, false)
	if !ok {
		return libQuery{}, noteView{}, false
	}
	v, err := parseNoteView(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return libQuery{}, noteView{}, false
	}
	return lq, v, true
}

// noteValues is the whole list state as a query: the library's (forceSort
// names the sort even at its default), the tag chip, the working copies and
// the rows to draw.
func noteValues(lq libQuery, v noteView, forceSort bool) url.Values {
	q := lq.values(forceSort)
	carryNoteValues(q, v)
	if v.Show > 0 {
		q.Set("show", strconv.Itoa(v.Show))
	}
	return q
}

// carryNoteValues sets the list's own filters on q: the tag chip and the
// working copies.
func carryNoteValues(q url.Values, v noteView) {
	if v.Tag != "" {
		q.Set("tag", v.Tag)
	}
	if v.Plans {
		q.Set("plans", "1")
	}
	if v.Caches {
		q.Set("caches", "1")
	}
}

// noteListHref is the list at state lq and v.
func noteListHref(lq libQuery, v noteView) string {
	return notesListPath + encodeQS(noteValues(lq, v, false))
}

// noteListQS is the query a row of the list carries to its note's own page,
// so the page's place and its k / j follow this list: the list's state
// without its page size, and on the default list its sort, so the page knows
// it came from a list at all (namesNoteList).
func noteListQS(lq libQuery, v noteView) string {
	v.Show = 0
	if qs := encodeQS(noteValues(lq, v, false)); qs != "" {
		return qs
	}
	return encodeQS(noteValues(lq, v, true))
}

// noteListKeys are the query keys a Notes list puts on its rows' links
// (noteListQS): a note's own URL that carries any of them came from that
// list.
var noteListKeys = []string{"sort", "q", "group", "project", "fav", "tag", "plans", "caches"}

// namesNoteList reports whether a note's own URL names the list it came from.
func namesNoteList(q url.Values) bool {
	for _, k := range noteListKeys {
		if q.Has(k) {
			return true
		}
	}
	return false
}

// carryNoteView is href, a link the library code drew from a libQuery, with
// the list's own filters kept (v's tag chip and working copies). The page
// size is not kept: a new choice starts at the first rows.
func carryNoteView(href string, v noteView) string {
	if v.Tag == "" && !v.Plans && !v.Caches {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	q := u.Query()
	carryNoteValues(q, v)
	u.RawQuery = q.Encode()
	return u.String()
}

// noteList is what the Notes page draws: one list, in groups. It is a page of
// the matches in the sort's order, by local day under Recent (memoryDay, the
// Diary's days), across every project or in the one chosen.
type noteList struct {
	Groups []noteListGroup
	// ByProject is one project's list: its rows name no project.
	ByProject bool
	// Shown is how many rows the page draws, Total how many match.
	Shown, Total int
	// More is the same list with MoreN more rows ("" when every match is
	// drawn); Rest says those are all the rows left.
	More  string
	MoreN int
	Rest  bool
	// Order says how the rows are ordered ("Newest first").
	Order string
	// RowQS is the query every row's link carries (noteListQS).
	RowQS string
}

// noteListGroup is one group of the list: a local day, or the one group of
// an order that has none.
type noteListGroup struct {
	// Key is the group's element id after ml-g-: a day (today, yesterday,
	// week, month, older), or all.
	Key   string
	Label string
	// Note is the group's count in words: every match it holds, the ones
	// past this page included.
	Note string
	Rows []noteRow
}

// noteOrders say how the list orders its rows, by sort key.
var noteOrders = map[string]string{"recent": "newest first", "name": "A to Z", "favorites": "starred first"}

// buildNoteList draws rows, the list's matches, as the page lists them at
// now: the first v.size() rows in the sort's order, by local day under
// Recent. Every group's count is of all its matches. rows is reordered in
// place.
func buildNoteList(rows []noteRow, lq libQuery, v noteView, now time.Time) noteList {
	order := noteOrders[lq.Sort]
	l := noteList{Total: len(rows), ByProject: lq.Project != "", RowQS: noteListQS(lq, v),
		Order: strings.ToUpper(order[:1]) + order[1:]}
	sortNoteRows(rows, lq.Sort)
	shown := rows
	if size := v.size(); len(rows) > size {
		shown = rows[:size]
		more := v
		more.Show = size + notePageSize
		left := len(rows) - size
		l.More, l.MoreN, l.Rest = noteListHref(lq, more), min(notePageSize, left), left <= notePageSize
	}
	l.Shown = len(shown)
	if lq.Sort != libDefaultSort {
		if len(shown) > 0 {
			l.Groups = []noteListGroup{{Key: "all", Label: l.Order, Note: strconv.Itoa(len(rows)), Rows: shown}}
		}
		return l
	}
	var counts [len(memoryDays)]int
	for _, r := range rows {
		counts[memoryDay(r.Updated, now)]++
	}
	for _, r := range shown {
		day := memoryDay(r.Updated, now)
		d := memoryDays[day]
		if n := len(l.Groups); n == 0 || l.Groups[n-1].Key != d.key {
			l.Groups = append(l.Groups, noteListGroup{Key: d.key, Label: d.label,
				Note: strconv.Itoa(counts[day]) + " changed"})
		}
		g := &l.Groups[len(l.Groups)-1]
		g.Rows = append(g.Rows, r)
	}
	return l
}

// noteFold is the agent working copies' line under the chips: N copies
// match the other filters; On when the list shows them; Href turns them the
// other way.
type noteFold struct {
	N    int
	On   bool
	Href string
}

// isAgentCache reports a planning subagent's cached run (the agent-cache
// tag): an agent's working copy, which the list folds away by default.
func isAgentCache(tags []string) bool { return slices.Contains(tags, plans.TagAgent) }

// inSomePlan reports a note tagged plan:<slug>, the "plan narratives" chip.
func inSomePlan(tags []string) bool {
	for _, t := range tags {
		if plans.SlugFromTags([]string{t}) != "" {
			return true
		}
	}
	return false
}

// notesPage assembles the Notes screen for the given state, at the moment
// the request is served.
func (s *Service) notesPage(ctx context.Context, lq libQuery, v noteView) (notesData, error) {
	return s.notesPageAt(ctx, lq, v, time.Now())
}

// notesPageAt assembles the Notes screen at now (the day groups' clock): the
// JSON answer's every match, and the page's list and controls. Every read
// goes through the request's handle, so a locked project's notes are absent
// from all of it.
func (s *Service) notesPageAt(ctx context.Context, lq libQuery, v noteView, now time.Time) (notesData, error) {
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
	var listed []noteRow
	count, favs, folded, everything, inPlans, chosen := 0, 0, 0, 0, 0, 0
	projectCounts, tagCounts := map[string]int{}, map[string]int{}
	for _, n := range notes {
		row := noteRow{
			ID: n.ID, Title: n.Title, Description: n.Description,
			Project: n.Project, Tags: n.Tags, Model: n.Model, Account: n.Account, Favorite: n.Favorite,
			Updated: n.Updated, Cache: isAgentCache(n.Tags), file: n.FilePath,
		}
		// Each control counts what its own choice would show: the other
		// filters applied, its own left open. A folded working copy counts
		// nowhere but in the fold's line.
		text, scope, fav := noteMatches(row, q, names[n.Project]), lq.inScope(n.Project), !lq.Fav || n.Favorite
		plan := inSomePlan(n.Tags)
		chip := (v.Tag == "" || slices.Contains(n.Tags, v.Tag)) && (!v.Plans || plan)
		drawn := !row.Cache || v.Caches
		if text && fav && chip && drawn {
			projectCounts[n.Project]++
		}
		if text && scope && chip && drawn && n.Favorite {
			favs++
		}
		if text && scope && fav && chip && row.Cache {
			folded++
		}
		if text && scope && fav && drawn {
			everything++
			if plan {
				inPlans++
			}
			if v.Tag != "" && slices.Contains(n.Tags, v.Tag) {
				chosen++
			}
			for _, t := range slices.Compact(slices.Sorted(slices.Values(splitNoteTags(n.Project, n.Tags).Tags))) {
				tagCounts[t]++
			}
		}
		if !text || !scope || !fav || !chip {
			continue
		}
		byProject[n.Project] = append(byProject[n.Project], row)
		count++
		if drawn {
			listed = append(listed, row)
		}
	}
	data := notesData{
		Groups: buildNoteGroups(byProject, lq.Sort), Count: count, Total: len(notes),
		Query: lq.Q, Sort: lq.Sort, Group: lq.Group, Project: lq.Project, Fav: lq.Fav,
		Tag: v.Tag, Plans: v.Plans,
	}
	data.List = buildNoteList(listed, lq, v, now)

	// The controls: the library's, each keeping the list's own filters, and
	// the list's own.
	data.libView = newLibView(lq, notesListPath, notesListPath, noteSortKeys)
	if v.Tag != "" || v.Plans {
		data.Filtered, data.Narrowed = true, true
	}
	for key, href := range data.Sorts {
		data.Sorts[key] = carryNoteView(href, v)
	}
	data.FavLink.Href = carryNoteView(data.FavLink.Href, v)
	data.FavLink.Count = favs
	data.ClearHref = carryNoteView(data.ClearHref, noteView{Caches: v.Caches})
	extra := url.Values{}
	carryNoteValues(extra, v)
	for name := range extra {
		data.Keep = append(data.Keep, libField{Name: name, Value: extra.Get(name)})
	}
	sort.Slice(data.Keep, func(i, j int) bool { return data.Keep[i].Name < data.Keep[j].Name })
	data.Projects, data.ProjectLabel = libProjectMenu(lq, projectCounts, names, notesListPath, "Global inbox")
	for i := range data.Projects {
		data.Projects[i].Href = carryNoteView(data.Projects[i].Href, v)
	}
	if data.Scope, err = s.projectSwitcher(ctx, data.Projects); err != nil {
		return notesData{}, err
	}
	data.TagChips = noteTagChips(lq, v, everything, inPlans, tagCounts, chosen)
	fold := v
	fold.Caches, fold.Show = !v.Caches, 0
	data.Fold = noteFold{N: folded, On: v.Caches, Href: noteListHref(lq, fold)}
	return data, nil
}

// noteTagChips lists the tag chips under the bar: Everything, then "plan
// narratives" (the notes tagged plan:<slug>) when any note in view has one,
// then the noteChipLimit tags written for people most notes in view carry,
// most first, each with what choosing it would show. Tags software stamps
// (machineTag) never become chips. The chosen tag stays listed, whatever its
// count. Each tag chip's Value is its tag.
func noteTagChips(lq libQuery, v noteView, everything, inPlans int, counts map[string]int, chosen int) []libLink {
	keep := noteView{Caches: v.Caches}
	chips := []libLink{{Label: "Everything", Href: noteListHref(lq, keep), Count: everything, On: v.Tag == "" && !v.Plans}}
	if inPlans > 0 || v.Plans {
		p := keep
		p.Plans = true
		chips = append(chips, libLink{Label: "plan narratives", Href: noteListHref(lq, p), Count: inPlans, On: v.Plans,
			Title: "Notes that belong to a plan: tagged plan:<slug>"})
	}
	tags := make([]string, 0, len(counts))
	for t := range counts {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if counts[tags[i]] != counts[tags[j]] {
			return counts[tags[i]] > counts[tags[j]]
		}
		return tags[i] < tags[j]
	})
	if len(tags) > noteChipLimit {
		tags = tags[:noteChipLimit]
	}
	if v.Tag != "" && !slices.Contains(tags, v.Tag) {
		tags = append(tags, v.Tag)
		counts = maps.Clone(counts)
		counts[v.Tag] = chosen
	}
	for _, t := range tags {
		o := keep
		o.Tag = t
		chips = append(chips, libLink{Label: t, Href: noteListHref(lq, o), Count: counts[t], On: v.Tag == t, Value: t})
	}
	return chips
}

// fillNoteRows completes the rows the page draws: each plan a note is part
// of, with the plan's status as its chip says it (one read of every plan,
// shared by the rows, and only when a row names one); up to noteRowLabels of
// its tags written for people; and its length in words, read from its file.
func (s *Service) fillNoteRows(ctx context.Context, l *noteList) {
	var statuses map[string]notePlan
	loaded := false
	for gi := range l.Groups {
		for ri := range l.Groups[gi].Rows {
			row := &l.Groups[gi].Rows[ri]
			tagged := splitNoteTags(row.Project, row.Tags)
			if len(tagged.Plans) > 0 && !loaded {
				statuses, loaded = s.notePlanStatuses(ctx), true
			}
			for _, p := range tagged.Plans {
				if st, ok := statuses[planKey(row.Project, p.Slug)]; ok {
					p.Status, p.Tone, p.Title = st.Status, st.Tone, st.Title
				}
				row.Plans = append(row.Plans, p)
			}
			row.Labels = tagged.Tags[:min(noteRowLabels, len(tagged.Tags))]
			row.MoreLabels = len(tagged.Tags) - len(row.Labels)
			row.Words = s.noteWords(row.file)
		}
	}
}

// notePlanStatuses reads every plan the request's handle holds (the Plans
// screen's planEntries) as a note's plan chip says it: its status words and
// tone, a captured plan's own status else its phase from its steps, and a
// title naming the steps done; keyed by planKey. A failed read leaves the
// chips bare.
func (s *Service) notePlanStatuses(ctx context.Context) map[string]notePlan {
	entries, err := s.planEntries(ctx)
	if err != nil {
		s.logger.Warn("console: note list plan statuses", "error", err)
		return nil
	}
	out := make(map[string]notePlan, len(entries))
	for _, e := range entries {
		row := planRow{Title: e.title(), Status: e.status()}
		if pl := e.Rollup; pl != nil {
			row.TasksTotal, row.TasksDone, row.TasksWIP = pl.Total, pl.Done, pl.InFlight
			row.TasksOpen = pl.Total - pl.Done - pl.InFlight
		}
		row.Phase = planPhase(row)
		p := notePlan{Title: row.Title}
		if row.TasksTotal > 0 {
			p.Title = fmt.Sprintf("%s: %d of %d steps done", row.Title, row.TasksDone, row.TasksTotal)
		}
		p.Status, p.Tone = planStatusWords(row)
		out[e.key()] = p
	}
	return out
}

// noteWords is the length in words of the note at path (data-dir relative),
// as its page counts it; 0 when it cannot be read.
func (s *Service) noteWords(path string) int {
	if s.cfg.Files == nil || path == "" {
		return 0
	}
	full, err := s.cfg.Files.Store().ReadNote(path)
	if err != nil {
		s.logger.Debug("console: note list words", "path", path, "error", err)
		return 0
	}
	return len(strings.Fields(full.Body))
}

func (s *Service) notesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lq, v, ok := s.parseNoteRequest(w, r)
	if !ok {
		return
	}
	data, err := s.notesPage(ctx, lq, v)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !wantsJSON(r) {
		s.fillNoteRows(ctx, &data.List)
	}
	s.render(w, r, "notes", pageData{Title: "Notes", Active: "notes", Data: data})
}

// noteLibraryPage answers a note's own URL in HTML: the reader fragment
// (?reader=1), or the note's own page (itempage_note.go), the list's state
// read from the URL either way. The page reads the note and its context
// alone, never the list.
func (s *Service) noteLibraryPage(w http.ResponseWriter, r *http.Request, d noteDetail) {
	ctx := r.Context()
	lq, v, ok := s.parseNoteRequest(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "notes", "note-reader", s.noteReaderOf(ctx, d, lq))
		return
	}
	s.noteItemPage(w, r, d, lq, v)
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
		s.planStatusOf(ctx, d.Project, &tagged.Plans[i])
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
func (d noteDetail) TagView() noteTags { return splitNoteTags(d.Project, d.Tags) }

// splitNoteTags splits the tags of a note in project for display: each plan
// it belongs to as a link to that project's plan, the tags written for
// people, and the ones software stamps (machineTag).
func splitNoteTags(project string, tags []string) noteTags {
	var out noteTags
	for _, t := range tags {
		if slug := plans.SlugFromTags([]string{t}); slug != "" {
			out.Plans = append(out.Plans, notePlan{Slug: slug, Href: planHref(project, slug)})
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
// it: a captured plan's own status, else its phase from its steps. The plan
// is the slug in project, the one the link opens (a plan is keyed by project
// and slug since plan:console-ux-pass UX 07), so the chip agrees with its
// page and with the Notes list's row. A plan the request cannot resolve
// keeps its bare link.
func (s *Service) planStatusOf(ctx context.Context, project string, p *notePlan) {
	primary, _, ok, err := plans.CompositionIn(ctx, s.db(ctx), project, p.Slug)
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
	p.Status, p.Tone = planStatusWords(row)
}

// planStatusWords is a plan row's status as a note's plan chip says it, with
// its tone: a captured plan's own status, else its phase from its steps.
func planStatusWords(row planRow) (status, tone string) {
	if row.Status != "" {
		return row.Status, planTone(row.Status)
	}
	switch row.Phase {
	case planPhaseDone:
		tone = "ok"
	case planPhaseInProgress:
		tone = "accent"
	}
	return planPhaseWords[row.Phase], tone
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
