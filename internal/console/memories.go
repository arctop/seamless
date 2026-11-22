package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
)

// memorySortKeys are the accepted ?sort values on the memories list. recent is
// the default (newest-updated first); name orders A-Z, reach by how often a
// memory surfaced, utility by the decayed demand score, and favorites floats
// starred rows first.
var memorySortKeys = []string{"recent", "name", "reach", "utility", "favorites"}

// memoriesListPath is the Memories screen's list URL.
const memoriesListPath = "/console/memories"

// memoryPageSize is how many rows the Memories list draws at a time across
// every project, and how many more its Show more link adds. A project's list
// is drawn whole.
const memoryPageSize = 50

// errNoFiles is returned when a write action needs the files subsystem but the
// console was built without it (should not happen in serve).
var errNoFiles = errors.New("console: files subsystem unavailable")

// memoryRow is a display projection of a memory for the browser.
type memoryRow struct {
	ID           string       `json:"id"`
	Kind         string       `json:"kind"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Project      string       `json:"project"`
	FilePath     string       `json:"filePath"`
	Updated      time.Time    `json:"updated"`
	Status       string       `json:"status"`            // active|superseded|archived
	Harness      string       `json:"harness,omitempty"` // producing client, resolved from the source session
	Model        string       `json:"model,omitempty"`   // producing model, verbatim
	Account      string       `json:"account,omitempty"` // restricted account that wrote the content ("" = owner)
	ReplacedBy   string       `json:"replacedBy,omitempty"`
	ReplacedByID string       `json:"replacedById,omitempty"`
	Favorite     bool         `json:"favorite,omitempty"`
	Injects      int          `json:"injects"`
	Reads        int          `json:"reads"`
	Utility      float64      `json:"utility,omitempty"`
	LastInjected *time.Time   `json:"lastInjected,omitempty"`
	AbsPath      string       `json:"absPath"`
	EditURL      template.URL `json:"-"` // vscode://file link; template.URL so it survives sanitization
}

// ScopeLabel names the row's scope as the list's project chip does.
func (m memoryRow) ScopeLabel() string {
	if m.Project == "" {
		return "Shared globally"
	}
	return m.Project
}

// kindGroup and projectGroup are the JSON answer's project -> kind tree.
type kindGroup struct {
	Kind     string      `json:"kind"`
	Memories []memoryRow `json:"memories"`
}

type projectGroup struct {
	Project string      `json:"project"`
	Count   int         `json:"count"`
	Kinds   []kindGroup `json:"kinds"`
}

// memoriesData is the payload for the Memories screen. The JSON answer is
// every match, unpaged, in its project and kind groups, with the archived and
// superseded ones beside them, whatever the page draws; List, the controls
// and the libView drive the HTML page only (plan:console-ux-pass UX 28).
type memoriesData struct {
	Groups        []projectGroup `json:"groups"`
	Inactive      []memoryRow    `json:"inactive"`
	ActiveCount   int            `json:"activeCount"`
	InactiveCount int            `json:"inactiveCount"`
	// Total is every active memory the request reads, whatever the
	// filters: the header's "3 of 128".
	Total int    `json:"total"`
	Query string `json:"query,omitempty"`
	Sort  string `json:"sort"`
	// The other filters, as the URL gave them (absent at their defaults).
	Group      string `json:"group,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Project    string `json:"project,omitempty"`
	Fav        bool   `json:"fav,omitempty"`
	Surfaced   string `json:"surfaced,omitempty"`
	CanArchive bool   `json:"-"`
	// View is the list's own state: which memories it lists and how many
	// rows it draws. RetiredTotal is every archived and superseded memory
	// the request reads, whatever the filters.
	View         memoryView `json:"-"`
	RetiredTotal int        `json:"-"`
	// List is what the page draws.
	List memoryList `json:"-"`
	// KindChips are the kind filter under the bar: All kinds, then each
	// kind with what choosing it would show. NeverLink is the Never
	// surfaced toggle, StatusLink the archived and superseded one.
	KindChips  []libLink `json:"-"`
	NeverLink  libLink   `json:"-"`
	StatusLink libLink   `json:"-"`
	libView    `json:"-"`
	// Item is a memory's own page (itempage.go): set, the screen draws it
	// instead of the list. HTML only.
	Item *itemPage `json:"-"`
}

// memoryView is the Memories list's own state beyond the library's
// (libQuery), read from the URL as strictly: ?status=inactive lists the
// archived and superseded memories instead of the active ones, and ?show=
// is how many rows the list across every project draws.
type memoryView struct {
	Inactive bool
	// Show is the rows to draw, 0 for memoryPageSize.
	Show int
}

// size is how many rows the list across every project draws.
func (v memoryView) size() int {
	if v.Show > 0 {
		return v.Show
	}
	return memoryPageSize
}

// parseMemoryView reads the list's own state from its URL. A value it cannot
// read is an error naming what it takes, as parseLibQuery's are.
func parseMemoryView(q url.Values) (memoryView, error) {
	var v memoryView
	switch st := q.Get("status"); st {
	case "":
	case "inactive":
		v.Inactive = true
	default:
		return v, fmt.Errorf("invalid status %q: valid values are inactive (or omit)", st)
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

// parseMemoryRequest reads the whole Memories state from a request: the
// library's and the list's own. ok=false means the 400 was already written.
func (s *Service) parseMemoryRequest(w http.ResponseWriter, r *http.Request) (libQuery, memoryView, bool) {
	lq, ok := s.parseLibRequest(w, r, memorySortKeys, true)
	if !ok {
		return libQuery{}, memoryView{}, false
	}
	v, err := parseMemoryView(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return libQuery{}, memoryView{}, false
	}
	return lq, v, true
}

// memoryValues is the whole list state as a query: the library's (forceSort
// names the sort even at its default), the status, and the rows to draw.
func memoryValues(lq libQuery, v memoryView, forceSort bool) url.Values {
	q := lq.values(forceSort)
	if v.Inactive {
		q.Set("status", "inactive")
	}
	if v.Show > 0 {
		q.Set("show", strconv.Itoa(v.Show))
	}
	return q
}

// memoryListHref is the list at state lq and v.
func memoryListHref(lq libQuery, v memoryView) string {
	return memoriesListPath + encodeQS(memoryValues(lq, v, false))
}

// memoryListQS is the query a row of the list carries to its memory's own
// page, so the page's place and its k / j follow this list: the list's
// state without its page size, and on the default list its sort, so the
// page knows it came from a list at all (namesMemoryList).
func memoryListQS(lq libQuery, v memoryView) string {
	v.Show = 0
	if qs := encodeQS(memoryValues(lq, v, false)); qs != "" {
		return qs
	}
	return encodeQS(memoryValues(lq, v, true))
}

// memoryListKeys are the query keys a Memories list puts on its rows' links
// (memoryListQS): a memory's own URL that carries any of them came from that
// list.
var memoryListKeys = []string{"sort", "q", "group", "kind", "project", "fav", "surfaced", "status"}

// namesMemoryList reports whether a memory's own URL names the list it came
// from.
func namesMemoryList(q url.Values) bool {
	for _, k := range memoryListKeys {
		if q.Has(k) {
			return true
		}
	}
	return false
}

// carryStatus is href, a link the library code drew from a libQuery, with
// the list's status kept: a filter, a sort or a project chosen in the
// archived and superseded list stays there. The page size is not kept: a
// new choice starts at the first rows.
func carryStatus(href string, v memoryView) string {
	if !v.Inactive {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	q := u.Query()
	q.Set("status", "inactive")
	u.RawQuery = q.Encode()
	return u.String()
}

// memoryList is what the Memories page draws: one list, in groups. Across
// every project it is a page of the matches in the sort's order, by local
// day under Recent; with a project chosen it is the whole project, by kind.
type memoryList struct {
	Groups []memoryListGroup
	// ByKind is a project's list, grouped by kind.
	ByKind bool
	// Shown is how many rows the page draws, Total how many match.
	Shown, Total int
	// More is the same list with MoreN more rows ("" when every match is
	// drawn).
	More  string
	MoreN int
	// Order says how the rows are ordered ("Newest first").
	Order string
	// RowQS is the query every row's link carries (memoryListQS).
	RowQS string
}

// memoryListGroup is one group of the list: a local day, a kind, or the one
// group of an order that has none.
type memoryListGroup struct {
	// Key is the group's element id after ml-g-: a day (today, yesterday,
	// week, month, older), a kind, or all.
	Key   string
	Label string
	// Kind is the kind a kind group holds (its accent), Gloss what that
	// kind holds (kinds.go).
	Kind, Gloss string
	// Note is the group's count in words: every match it holds, the ones
	// past this page included.
	Note string
	Rows []memoryRow
}

// The local days the list across every project groups its newest-first rows
// in. A day is counted as the Diary counts it: from the daemon's local
// midnight (dyMidnight), a day back with AddDate. This week is the seven days
// up to today, this month the calendar month.
const (
	dayToday = iota
	dayYesterday
	dayThisWeek
	dayThisMonth
	dayOlder
)

var memoryDays = [...]struct{ key, label string }{
	dayToday:     {"today", "Today"},
	dayYesterday: {"yesterday", "Yesterday"},
	dayThisWeek:  {"week", "Earlier this week"},
	dayThisMonth: {"month", "Earlier this month"},
	dayOlder:     {"older", "Older"},
}

// memoryDay is the day group of a memory updated at t, at now: today
// (a time after now included), yesterday, earlier this week, earlier this
// month, or older.
func memoryDay(t, now time.Time) int {
	today := dyMidnight(now)
	switch {
	case !t.Before(today):
		return dayToday
	case !t.Before(today.AddDate(0, 0, -1)):
		return dayYesterday
	case !t.Before(today.AddDate(0, 0, -6)):
		return dayThisWeek
	case !t.Before(dyMonthStart(now)):
		return dayThisMonth
	default:
		return dayOlder
	}
}

// memoryOrders say how the list orders its rows, by sort key.
var memoryOrders = map[string]string{
	"recent": "newest first", "name": "A to Z", "reach": "most surfaced first",
	"utility": "highest utility first", "favorites": "starred first",
}

// buildMemoryList draws rows, the matches of the list's status, as the page
// lists them at now: with a project chosen, every row by kind in core's
// order, each kind in the sort's order; across every project, the first
// v.size() rows in the sort's order, by local day under Recent. Every
// group's count is of all its matches. rows is reordered in place.
func buildMemoryList(rows []memoryRow, lq libQuery, v memoryView, now time.Time) memoryList {
	order := memoryOrders[lq.Sort]
	l := memoryList{Total: len(rows), ByKind: lq.Project != "", RowQS: memoryListQS(lq, v)}
	if l.ByKind {
		l.Order = "By kind, then " + order
		byKind := map[string][]memoryRow{}
		for _, r := range rows {
			byKind[r.Kind] = append(byKind[r.Kind], r)
		}
		for _, k := range core.MemoryKinds {
			kind := string(k)
			group := byKind[kind]
			if len(group) == 0 {
				continue
			}
			sortMemoryRows(group, lq.Sort)
			l.Groups = append(l.Groups, memoryListGroup{Key: kind, Label: memoryNoun(kind, 2), Kind: kind,
				Gloss: kindGloss(kind), Note: strconv.Itoa(len(group)), Rows: group})
			l.Shown += len(group)
		}
		return l
	}
	l.Order = strings.ToUpper(order[:1]) + order[1:]
	sortMemoryRows(rows, lq.Sort)
	shown := rows
	if size := v.size(); len(rows) > size {
		shown = rows[:size]
		more := v
		more.Show = size + memoryPageSize
		l.More, l.MoreN = memoryListHref(lq, more), min(memoryPageSize, len(rows)-size)
	}
	l.Shown = len(shown)
	if lq.Sort != libDefaultSort {
		if len(shown) > 0 {
			l.Groups = []memoryListGroup{{Key: "all", Label: l.Order, Note: strconv.Itoa(len(rows)), Rows: shown}}
		}
		return l
	}
	var counts [len(memoryDays)]int
	for _, r := range rows {
		counts[memoryDay(r.Updated, now)]++
	}
	for _, r := range shown {
		d := memoryDays[memoryDay(r.Updated, now)]
		if n := len(l.Groups); n == 0 || l.Groups[n-1].Key != d.key {
			l.Groups = append(l.Groups, memoryListGroup{Key: d.key, Label: d.label,
				Note: strconv.Itoa(counts[memoryDay(r.Updated, now)]) + " changed"})
		}
		g := &l.Groups[len(l.Groups)-1]
		g.Rows = append(g.Rows, r)
	}
	return l
}

// memoriesPage assembles the Memories screen for the given state, at the
// moment the request is served.
func (s *Service) memoriesPage(ctx context.Context, lq libQuery, v memoryView) (memoriesData, error) {
	return s.memoriesPageAt(ctx, lq, v, time.Now())
}

// memoriesPageAt assembles the Memories screen at now (the day groups'
// clock): the JSON answer's every match, and the page's list and controls.
// Every read goes through the request's handle, so a locked project's
// memories are absent from all of it.
func (s *Service) memoriesPageAt(ctx context.Context, lq libQuery, v memoryView, now time.Time) (memoriesData, error) {
	q := strings.ToLower(lq.Q)
	s.freshRetrievalStats(ctx)
	mems, err := store.AllMemoriesIncludingInvalid(ctx, s.db(ctx))
	if err != nil {
		return memoriesData{}, err
	}
	stats, err := store.AllRetrievalStats(ctx, s.db(ctx))
	if err != nil {
		return memoriesData{}, err
	}
	names, err := s.projectNames(ctx)
	if err != nil {
		return memoriesData{}, err
	}

	nameByID := make(map[string]string, len(mems))
	for _, m := range mems {
		nameByID[m.ID] = m.Name
	}

	// project -> kind -> rows (active only), and the same rows as one list;
	// inactive rows collected separately. Every filter applies to both sets.
	// The controls count the list's own status: what choosing them would
	// show here.
	resolve := s.sourceSessionResolver(ctx)
	active := map[string]map[string][]memoryRow{}
	var flat, inactive []memoryRow
	total, retired, favs, nevers := 0, 0, 0, 0
	kindCounts, projectCounts := map[string]int{}, map[string]int{}
	for _, m := range mems {
		st := stats[m.ID]
		text := memoryMatches(m, q, names[m.Project])
		kind := lq.Kind == "" || string(m.Kind) == lq.Kind
		scope := lq.inScope(m.Project)
		fav := !lq.Fav || m.Favorite
		never := lq.Surfaced == "" || st.InjectCount == 0
		if m.Active() {
			total++
		} else {
			retired++
		}
		if m.Active() != v.Inactive {
			// Each control counts what its own choice would show: the
			// other filters applied, its own left open.
			if text && scope && fav && never {
				kindCounts[string(m.Kind)]++
			}
			if text && kind && fav && never {
				projectCounts[m.Project]++
			}
			if text && kind && scope && never && m.Favorite {
				favs++
			}
			if text && kind && scope && fav && st.InjectCount == 0 {
				nevers++
			}
		}
		if !text || !kind || !scope || !fav || !never {
			continue
		}
		row := toMemoryRow(m, st, nameByID, s.cfg.DataDir)
		row.Harness = harnessOfSource(resolve, m.SourceSession)
		if !m.Active() {
			inactive = append(inactive, row)
			continue
		}
		if active[m.Project] == nil {
			active[m.Project] = map[string][]memoryRow{}
		}
		active[m.Project][string(m.Kind)] = append(active[m.Project][string(m.Kind)], row)
		flat = append(flat, row)
	}

	sortMemoryRows(inactive, lq.Sort)
	data := memoriesData{
		Groups:        buildGroups(active, lq.Sort),
		Inactive:      inactive,
		ActiveCount:   len(flat),
		InactiveCount: len(inactive),
		Total:         total,
		Query:         lq.Q,
		Sort:          lq.Sort,
		Group:         lq.Group, Kind: lq.Kind, Project: lq.Project, Fav: lq.Fav, Surfaced: lq.Surfaced,
		CanArchive:   s.cfg.Files != nil,
		View:         v,
		RetiredTotal: retired,
	}
	listed := flat
	if v.Inactive {
		listed = slices.Clone(inactive)
	}
	data.List = buildMemoryList(listed, lq, v, now)

	// The controls: the library's, each keeping the list's status, and the
	// list's own.
	data.libView = newLibView(lq, memoriesListPath, memoriesListPath, memorySortKeys)
	for key, href := range data.Sorts {
		data.Sorts[key] = carryStatus(href, v)
	}
	data.FavLink.Href = carryStatus(data.FavLink.Href, v)
	data.FavLink.Count = favs
	data.ClearHref = carryStatus(data.ClearHref, v)
	if v.Inactive {
		data.Keep = append(data.Keep, libField{Name: "status", Value: "inactive"})
		sort.Slice(data.Keep, func(i, j int) bool { return data.Keep[i].Name < data.Keep[j].Name })
		if data.SearchHref != "" {
			data.SearchHref += "&archived=1"
		}
	}
	data.Projects, data.ProjectLabel = libProjectMenu(lq, projectCounts, names, memoriesListPath, "Shared globally")
	for i := range data.Projects {
		data.Projects[i].Href = carryStatus(data.Projects[i].Href, v)
	}
	if data.Scope, err = s.projectSwitcher(ctx, data.Projects); err != nil {
		return memoriesData{}, err
	}
	data.KindChips = memoryKindChips(lq, v, kindCounts)
	never := lq
	never.Surfaced = ""
	if lq.Surfaced == "" {
		never.Surfaced = "never"
	}
	data.NeverLink = libLink{Label: "Never surfaced", Href: memoryListHref(never, memoryView{Inactive: v.Inactive}),
		Count: nevers, On: lq.Surfaced != ""}
	data.StatusLink = libLink{Label: "archived and superseded", Href: memoryListHref(lq, memoryView{Inactive: !v.Inactive}),
		Count: len(inactive), On: v.Inactive}
	return data, nil
}

// memoryKindChips lists the kind filter's chips: All kinds, then each kind in
// canonical order with how many memories match the other filters there, its
// glossary line as the title. A kind with none is left out, but the chosen
// one, which stays listed. Each chip's Value is its kind ("" for All kinds).
func memoryKindChips(lq libQuery, v memoryView, counts map[string]int) []libLink {
	all := lq
	all.Kind = ""
	n := 0
	for _, c := range counts {
		n += c
	}
	keep := memoryView{Inactive: v.Inactive}
	chips := []libLink{{Label: "All kinds", Href: memoryListHref(all, keep), Count: n, On: lq.Kind == ""}}
	for _, k := range core.MemoryKinds {
		kind := string(k)
		if counts[kind] == 0 && kind != lq.Kind {
			continue
		}
		o := lq
		o.Kind = kind
		chips = append(chips, libLink{Label: kind, Href: memoryListHref(o, keep), Title: kindGloss(kind),
			Count: counts[kind], On: kind == lq.Kind, Value: kind})
	}
	return chips
}

func (s *Service) memoriesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lq, v, ok := s.parseMemoryRequest(w, r)
	if !ok {
		return
	}
	data, err := s.memoriesPage(ctx, lq, v)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "memories", pageData{Title: "Memories", Active: "memories", Data: data})
}

// memoryLibraryPage answers a memory's own URL in HTML: the reader fragment
// (?reader=1), or the memory's own page (itempage.go), the list's state read
// from the URL either way. The page reads the memory and its context alone,
// never the list.
func (s *Service) memoryLibraryPage(w http.ResponseWriter, r *http.Request, d memoryDetail) {
	ctx := r.Context()
	lq, v, ok := s.parseMemoryRequest(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "memories", "memory-reader", s.memoryReaderOf(ctx, d, lq))
		return
	}
	s.memoryItemPage(w, r, d, lq, v)
}

// memoryMatches reports whether a memory satisfies the ?q text filter (empty q
// matches all): a case-insensitive substring of name, description, kind, any
// tag, or its project's slug or name (projectName).
func memoryMatches(m core.Memory, q, projectName string) bool {
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(m.Name), q) ||
		strings.Contains(strings.ToLower(m.Description), q) ||
		strings.Contains(strings.ToLower(string(m.Kind)), q) {
		return true
	}
	for _, t := range m.Tags {
		if strings.Contains(strings.ToLower(t), q) {
			return true
		}
	}
	return scopeMatches(m.Project, projectName, q)
}

func toMemoryRow(m core.Memory, stat store.RetrievalStat, nameByID map[string]string, dataDir string) memoryRow {
	status := "active"
	if !m.Active() {
		if m.SupersededBy != "" {
			status = "superseded"
		} else {
			status = "archived"
		}
	}
	abs, edit := absAndEditURL(dataDir, m.FilePath)
	return memoryRow{
		ID: m.ID, Kind: string(m.Kind), Name: m.Name, Description: m.Description,
		Project: m.Project, FilePath: m.FilePath, Updated: m.Updated, Status: status,
		Model:      m.Model, // Harness is filled by the caller (it needs a session resolver)
		Account:    m.Account,
		Favorite:   m.Favorite,
		ReplacedBy: nameByID[m.SupersededBy], ReplacedByID: m.SupersededBy,
		Injects: stat.InjectCount, Reads: stat.ReadCount, Utility: stat.Utility,
		LastInjected: stat.LastInjectedAt,
		AbsPath:      abs, EditURL: edit,
	}
}

// absAndEditURL resolves a data-dir-relative file path to its absolute path and a
// vscode:// editor link. An empty dataDir (or an already-absolute path) leaves
// the path as given. template.URL bypasses sanitization so the custom scheme
// survives; the input is a server-controlled file path, never user text.
func absAndEditURL(dataDir, relPath string) (string, template.URL) {
	abs := relPath
	if dataDir != "" && !filepath.IsAbs(abs) {
		abs = filepath.Join(dataDir, relPath)
	}
	return abs, template.URL("vscode://file" + abs)
}

// buildGroups turns the project->kind->rows map into an ordered slice: global
// ("") first then projects alphabetically, kinds in canonical order.
func buildGroups(active map[string]map[string][]memoryRow, sortKey string) []projectGroup {
	projects := make([]string, 0, len(active))
	for p := range active {
		projects = append(projects, p)
	}
	sortScopes(projects)

	var groups []projectGroup
	for _, p := range projects {
		byKind := active[p]
		var kinds []kindGroup
		count := 0
		for _, k := range core.MemoryKinds {
			rows := byKind[string(k)]
			if len(rows) == 0 {
				continue
			}
			sortMemoryRows(rows, sortKey)
			kinds = append(kinds, kindGroup{Kind: string(k), Memories: rows})
			count += len(rows)
		}
		groups = append(groups, projectGroup{Project: p, Count: count, Kinds: kinds})
	}
	return groups
}

// sortMemoryRows orders rows per the ?sort key: recent (newest-updated first,
// the default), name (A-Z), reach (most surfaced first), utility (highest
// decayed demand first), or favorites (starred first). Ties fall back to name
// for a stable, readable order.
func sortMemoryRows(rows []memoryRow, sortKey string) {
	switch sortKey {
	case "name":
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	case "favorites":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Favorite != rows[j].Favorite {
				return rows[i].Favorite
			}
			return rows[i].Name < rows[j].Name
		})
	case "recent":
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].Updated.Equal(rows[j].Updated) {
				return rows[i].Updated.After(rows[j].Updated)
			}
			return rows[i].Name < rows[j].Name
		})
	case "reach":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Injects != rows[j].Injects {
				return rows[i].Injects > rows[j].Injects
			}
			return rows[i].Name < rows[j].Name
		})
	case "utility":
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Utility != rows[j].Utility {
				return rows[i].Utility > rows[j].Utility
			}
			return rows[i].Name < rows[j].Name
		})
	}
}

// memoryReader is the reader pane's view of one memory: its detail plus what
// only the library draws -- the sessions it surfaced to, and the page state
// its star, its archive and its supersession links carry back.
type memoryReader struct {
	memoryDetail
	SurfacedTo *surfacedTo
	// QS is the library state (?q=&sort=&...) the reader's links carry.
	QS string
}

// memoryReaderOf builds the reader for d under library state lq.
func (s *Service) memoryReaderOf(ctx context.Context, d memoryDetail, lq libQuery) *memoryReader {
	return &memoryReader{memoryDetail: d, SurfacedTo: s.surfacedToOf(ctx, d.ID), QS: lq.QS()}
}

// The reader names the latest sessions a memory surfaced to, and lists more
// on request, up to surfacedToListed; past that it says how many are older.
const (
	surfacedToLatest = 5
	surfacedToListed = 50
)

// surfacedTo is the sessions a memory surfaced to, most recent first:
// Latest inline, More behind "all N", Earlier the ones past the list.
type surfacedTo struct {
	Latest  []surfacedSession
	More    []surfacedSession
	Total   int
	Earlier int
}

// surfacedSession is one session a memory surfaced to. ID "" is an injection
// that ran in no session.
type surfacedSession struct {
	ID    string
	Name  string
	Times int
	Last  time.Time
}

// surfacedToOf reads the sessions memory id surfaced to, through the
// request's handle: a locked project's sessions are absent from the read. nil
// when it never surfaced (or the read failed, which the log records).
func (s *Service) surfacedToOf(ctx context.Context, id string) *surfacedTo {
	rows, err := store.InjectedSessions(ctx, s.db(ctx), id, surfacedToListed)
	if err != nil {
		s.logger.Warn("console: memory surfaced to", "id", id, "error", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	out := &surfacedTo{Total: len(rows)}
	if len(rows) == surfacedToListed {
		if n, err := store.CountInjectedSessions(ctx, s.db(ctx), id); err != nil {
			s.logger.Warn("console: memory surfaced count", "id", id, "error", err)
		} else {
			out.Total = n
		}
	}
	for i, x := range rows {
		ss := surfacedSession{ID: x.SessionID, Name: x.Name, Times: x.Times, Last: x.Last}
		if ss.Name == "" && ss.ID != "" {
			ss.Name = shortID(ss.ID)
		}
		if i < surfacedToLatest {
			out.Latest = append(out.Latest, ss)
		} else {
			out.More = append(out.More, ss)
		}
	}
	out.Earlier = out.Total - len(rows)
	return out
}

// memoryArchive retires an active memory (the console's one write to memory
// state), then redirects back to the browser: to the Memories list the form
// names in next, its filters kept, else the plain list.
func (s *Service) memoryArchive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Files == nil {
		s.serverError(w, r, errNoFiles)
		return
	}
	id := r.PathValue("id")
	back := archiveBack(r.PostFormValue("next"), id)
	idx, ok, err := store.MemoryByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No memory with id "+id+".")
		return
	}
	if idx.InvalidAt != nil {
		// Already inactive; nothing to do.
		http.Redirect(w, r, back.RequestURI(), http.StatusSeeOther)
		return
	}
	// The read that feeds the tombstone write happens under the file's lock
	// (files.Manager.Mutate): read outside it, an agent's append landing in
	// between was undone by the rewrite, with no error anywhere.
	var updated core.Memory
	already := false
	err = s.cfg.Files.Mutate(ctx, idx.FilePath, func(ctx context.Context) error {
		mem, rerr := s.cfg.Files.Store().ReadMemory(idx.FilePath)
		if rerr != nil {
			return rerr
		}
		if mem.InvalidAt != nil {
			already = true // retired while this request waited
			return nil
		}
		u, aerr := lifecycle.Archive(ctx, s.cfg.Files, mem, "archived from console", time.Now().UTC())
		if aerr != nil {
			return aerr
		}
		updated = u
		return nil
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if already {
		http.Redirect(w, r, back.RequestURI(), http.StatusSeeOther)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind: core.EventMemoryArchived, ProjectSlug: updated.Project, ItemID: updated.ID,
			Payload: map[string]any{"name": updated.Name, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record archive event", "error", err)
		}
	}
	http.Redirect(w, r, withNotice(back, "Archived "+updated.Name+"."), http.StatusSeeOther)
}

// archiveBack is where an archive of memory id returns: next when it is the
// Memories list or that memory's own page (plan:console-ux-pass UX 26, which
// then shows it archived, with Restore), with the library state either
// carries; else the plain list. Any other page is not where the owner
// archived from.
func archiveBack(next, id string) *url.URL {
	if u, err := url.Parse(next); err == nil && id != "" && u.Path == memoriesListPath+"/"+id {
		return libBack(next, u.Path)
	}
	return libBack(next, memoriesListPath)
}

// libBack is next when its path is exactly path (a same-origin URL with no
// host, scheme or user), its query kept minus the one-shot flash; else path
// alone. Every library write returns through it, so a form can name its way
// back with the library state and never leave the page it belongs to.
func libBack(next, path string) *url.URL {
	u, err := url.Parse(next)
	if err != nil || u.Path != path || u.Host != "" || u.Scheme != "" || u.User != nil {
		return &url.URL{Path: path}
	}
	v := u.Query()
	v.Del("notice")
	v.Del("error")
	return &url.URL{Path: path, RawQuery: v.Encode()}
}

// libRefusal is a library write refused for a reason the owner can act on:
// the browser goes back to the page with msg as its error, a JSON caller gets
// a 409 with msg and extra. Returned from inside a files-layer mutation, it
// aborts the mutation before anything is written.
type libRefusal struct {
	msg   string
	extra map[string]any
}

func (e *libRefusal) Error() string { return e.msg }

// libRefuse answers a libRefusal; back is the page the browser returns to.
func libRefuse(w http.ResponseWriter, r *http.Request, back *url.URL, e *libRefusal) {
	if wantsJSON(r) {
		out := map[string]any{"error": e.msg}
		for k, v := range e.extra {
			out[k] = v
		}
		writeJSON(w, http.StatusConflict, out)
		return
	}
	q := back.Query()
	q.Del("notice")
	q.Set("error", e.msg)
	back.RawQuery = q.Encode()
	http.Redirect(w, r, back.RequestURI(), http.StatusSeeOther)
}

// withholdUnreadSuccessor drops the supersession line from d's body when this
// request cannot read what replaced the memory (d.ReplacedByID unresolved):
// the line names that memory's project, name and id, so a replacement in a
// project locked to this request would otherwise show through the body.
// memory_read withholds it the same way (lifecycle.WithoutTombstone). The
// file keeps the line; only this reader's copy loses it.
func (s *Service) withholdUnreadSuccessor(ctx context.Context, d *memoryDetail, m core.Memory) {
	if m.SupersededBy == "" || d.ReplacedByID != "" || !d.BodyLoaded {
		return
	}
	body := lifecycle.WithoutTombstone(d.BodyText, m.SupersededBy)
	if body == d.BodyText {
		return
	}
	d.BodyText = body
	d.Body = s.renderBody(ctx, body, m.Project)
	d.BodyIsEcho = bodyEchoesDescription(body, m.Description)
}

// restoreView is the inactive banner's restore control: Offered when Restore
// is drawn, else Live (the active memory that replaced this one, when this
// request reads it) or Blocked (why not, a sentence), with ToHeld when the
// Held screen is where it is settled. Zero for an active memory.
type restoreView struct {
	Offered bool
	Live    *memoryRef
	Blocked string
	ToHeld  bool
}

// restoreViewOf decides the restore control for m, reading through the
// request's handle only: a replacement in a project locked to this request is
// absent here, so Restore stays offered, and the POST, which checks on the
// write handle as every write does, refuses it without naming it.
func (s *Service) restoreViewOf(ctx context.Context, m core.Memory) restoreView {
	if m.Active() || s.cfg.Files == nil {
		return restoreView{}
	}
	if m.SupersededBy != "" {
		live, found, err := lifecycle.LiveSuccessor(ctx, memoryLookup(s.db(ctx)), m)
		if err != nil {
			s.logger.Warn("console: memory restore chain", "id", m.ID, "error", err)
			return restoreView{Blocked: "Its supersession chain cannot be followed to its end, so it cannot be restored here."}
		}
		if found {
			return restoreView{Live: &memoryRef{ID: live.ID, Name: live.Name, Kind: string(live.Kind), Project: live.Project}}
		}
	}
	if why := s.heldBlock(ctx, m.ID, m.FilePath, true); why != "" {
		return restoreView{Blocked: why, ToHeld: true}
	}
	return restoreView{Offered: true}
}

// memoryLookup resolves memory ids on db, for lifecycle's chain walks.
func memoryLookup(db *sql.DB) lifecycle.MemoryLookup {
	return func(ctx context.Context, id string) (core.Memory, bool, error) {
		return store.MemoryByID(ctx, db, id)
	}
}

// What the Held screen keeps a restore or a delete from doing here
// (heldBlock).
const (
	heldDrifted = "Its file changed outside Seamless and is quarantined: accept it on Held first."
	heldFlagged = "The review flagged it: mark it safe on Held before you restore it."
	heldPending = "An agent's change to it waits on Held: release or discard that first."
)

// heldBlock says why the Held screen keeps the item with this id and file
// from being restored (restore) or deleted here, "" when nothing does.
//
// With write screening on, a file in drift quarantine changed outside
// Seamless and has not been accepted: a delete would lose its only copy and
// drop the quarantine record (files.Manager.Remove), lifting the quarantine
// only the owner's Accept lifts, and a restore would bring unaccepted content
// back to every agent. A memory the review flagged is likewise restored only
// after Mark safe; deleting a flagged note only removes it. And while the
// held queue is served (screening on, or restricted accounts' proposals), a
// write to the item waiting there would turn stale, leaving only Discard, so
// it is settled first. A record that cannot be read blocks: Held is where to
// look.
func (s *Service) heldBlock(ctx context.Context, itemID, path string, restore bool) string {
	mode := s.heldModeFor(s.effectiveFeatures(ctx))
	if mode == heldOff {
		return ""
	}
	if mode == heldScreening {
		rec, ok, err := store.CorpusRecordByPath(ctx, s.db(ctx), path)
		if err != nil {
			s.logger.Warn("console: held check corpus record", "path", path, "error", err)
			return heldDrifted
		}
		if ok && rec.Drifted() {
			return heldDrifted
		}
		if ok && restore && rec.ReviewStatus == store.ReviewFlagged {
			return heldFlagged
		}
	}
	pending, err := store.HeldWrites(ctx, s.db(ctx), store.HeldStatusHeld, 0)
	if err != nil {
		s.logger.Warn("console: held check pending writes", "id", itemID, "error", err)
		return heldPending
	}
	for _, h := range pending {
		if h.ItemID == itemID {
			return heldPending
		}
	}
	return ""
}

// memoryRestore brings an archived or superseded memory back into the active
// set through lifecycle.Restore, under the file's lock, then returns to the
// memory's own page with the library state the form names in next. Nothing
// is written when a memory that replaced it is still active (the answer names
// it when this request reads it), or while the Held screen holds it
// (heldBlock). An active memory is left as it is. It records memory.written
// with restored and by=console, as the gardener's undo does.
func (s *Service) memoryRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Files == nil {
		s.serverError(w, r, errNoFiles)
		return
	}
	id := r.PathValue("id")
	back := libBack(r.PostFormValue("next"), memoriesListPath+"/"+id)
	idx, ok, err := store.MemoryByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No memory with id "+id+".")
		return
	}
	var restored core.Memory
	from := ""
	err = s.cfg.Files.Mutate(ctx, idx.FilePath, func(ctx context.Context) error {
		mem, rerr := s.cfg.Files.Store().ReadMemory(idx.FilePath)
		if rerr != nil {
			return rerr
		}
		if mem.ID != idx.ID {
			return errMemoryMoved
		}
		if mem.Active() {
			return nil // restored while this request waited
		}
		if why := s.heldBlock(ctx, mem.ID, mem.FilePath, true); why != "" {
			return &libRefusal{msg: why}
		}
		// The chain is read on the write handle: a replacement in a project
		// locked to this request still refuses; it is only not named.
		u, rerr := lifecycle.Restore(ctx, s.cfg.Files, memoryLookup(s.writeDB()), mem, time.Now().UTC())
		if rerr != nil {
			return rerr
		}
		from = "archive"
		if mem.SupersededBy != "" {
			from = "supersede"
		}
		restored = u
		return nil
	})
	var refusal *libRefusal
	var successor *lifecycle.SuccessorError
	switch {
	case errors.As(err, &successor):
		libRefuse(w, r, back, s.successorRefusal(ctx, successor.Successor))
		return
	case errors.As(err, &refusal):
		libRefuse(w, r, back, refusal)
		return
	case errors.Is(err, errMemoryMoved), errors.Is(err, fs.ErrNotExist):
		s.notFound(w, r, "No memory with id "+id+".")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if restored.ID == "" {
		// Already active: nothing to do, so a second press is harmless.
		if wantsJSON(r) {
			writeJSON(w, http.StatusOK, map[string]any{"id": idx.ID, "name": idx.Name, "status": "active"})
			return
		}
		http.Redirect(w, r, back.RequestURI(), http.StatusSeeOther)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind: core.EventMemoryWritten, ProjectSlug: restored.Project, ItemID: restored.ID,
			Payload: map[string]any{"name": restored.Name, "restored": from, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record restore event", "error", err)
		}
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": restored.ID, "name": restored.Name, "status": "active", "restored": from})
		return
	}
	http.Redirect(w, r, withNotice(back, "Restored "+restored.Name+": it reaches agents again."), http.StatusSeeOther)
}

// errMemoryMoved: the file an index row named now holds another memory.
var errMemoryMoved = errors.New("console: that file holds another memory now")

// successorRefusal is a restore refused because live replaced the memory and
// is still active. It names live, with its link, only when this request reads
// it: a replacement in a locked project refuses without a name.
func (s *Service) successorRefusal(ctx context.Context, live core.Memory) *libRefusal {
	seen, ok, err := store.MemoryByID(ctx, s.db(ctx), live.ID)
	if err != nil {
		s.logger.Warn("console: restore successor", "id", live.ID, "error", err)
	}
	if err != nil || !ok {
		return &libRefusal{msg: "Not restored: a memory that replaced it is still active."}
	}
	return &libRefusal{
		msg: "Not restored: " + seen.Name + " replaced it and is still active.",
		extra: map[string]any{"successor": map[string]any{
			"id": seen.ID, "name": seen.Name, "project": seen.Project, "url": memoriesListPath + "/" + seen.ID,
		}},
	}
}
