package console

// The Mishaps screen (/console/mishaps): every mishap agents reported at
// session_end, newest first, filtered by type, project and window, with the
// count of each type and of each memory the reports named -- a recurrence
// review's two questions: which kinds keep happening, and which memories
// fail to prevent their mishap. seam mishaps reads its JSON.

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// mishapsPath is the screen's route.
const mishapsPath = "/console/mishaps"

// mishapsListLimit caps the rows the page lists; the counts cover every report
// in view.
const mishapsListLimit = 200

// mishapsDefaultWindow is the page's window when the request names none: all
// time, because mishaps are rare and a review counts them across weeks.
const mishapsDefaultWindow = "all"

// mishapsData is the Mishaps page and its JSON answer.
type mishapsData struct {
	// Type is the ?type= filter: a core.MishapTypes slug, "untyped", or ""
	// for every report.
	Type string `json:"type,omitempty"`
	// Project is the ?project= filter as the request named it ("global" for
	// the global scope's reports), "" for every scope.
	Project string `json:"project,omitempty"`
	// Window is the window's key (store.RetrievalWindowKeys), "" when ?days=
	// named one; Days is that count. Since is the window's lower bound (zero:
	// all time).
	Window      string    `json:"window,omitempty"`
	Days        int       `json:"days,omitempty"`
	WindowLabel string    `json:"windowLabel"`
	Since       time.Time `json:"since,omitzero"`
	// Total is every report in the scope and window, whatever its type.
	Total int `json:"total"`
	// Types counts the scope and window by type, every type in order and then
	// the untyped reports, zeros included.
	Types []mishapTypeCount `json:"types"`
	// Mishaps are the reports the filters select, newest first, at most
	// mishapsListLimit; Shown is how many the filters select in all.
	Mishaps []mishapListRow `json:"mishaps"`
	Shown   int             `json:"shown"`
	// Memories are the memories the selected reports named, most named first.
	Memories []mishapMemoryCount `json:"memories"`
	// Locked names the locked projects this request does not read, whose
	// reports the page leaves out: names only. nil while nothing is locked.
	Locked []string `json:"locked,omitempty"`
	// Review is where the owner's review stands: when the reports were last
	// marked reviewed and how many came after, whatever the filters. nil
	// while no report exists at all.
	Review *mishapsReview `json:"review,omitempty"`

	Chips        []mishapLink `json:"-"`
	Windows      []mishapLink `json:"-"`
	ClearProject string       `json:"-"`
}

// Limited reports whether the list stops short of what the filters select.
func (d mishapsData) Limited() bool { return d.Shown > len(d.Mishaps) }

// ShownLabel counts what the filters select ("3 mishaps").
func (d mishapsData) ShownLabel() string { return plural(d.Shown, "mishap", "mishaps") }

// TypeLabel is the selected filter as a phrase, "" for every report.
func (d mishapsData) TypeLabel() string { return d.Type }

// mishapsReview is the review strip: the stamp (nil before the first review),
// the reports recorded after it (every report before then), and, for the Mark
// reviewed form, the newest report's time and the page to come back to.
type mishapsReview struct {
	At      *time.Time `json:"at,omitempty"`
	New     int        `json:"new"`
	Through string     `json:"-"`
	Return  string     `json:"-"`
}

// mishapTypeCount is one type's count; Type "untyped" counts the reports that
// named no type.
type mishapTypeCount struct {
	Type    string `json:"type"`
	Meaning string `json:"meaning"`
	Count   int    `json:"count"`
}

// mishapListRow is one report: its text with the type prefix taken off (Text)
// and as recorded (Description), its type, where it was reported and by
// which session, and the memories it named.
type mishapListRow struct {
	ID          string            `json:"id"`
	When        time.Time         `json:"ts"`
	Type        string            `json:"type,omitempty"`
	Meaning     string            `json:"-"`
	Text        string            `json:"text"`
	Description string            `json:"description"`
	Project     string            `json:"project,omitempty"`
	SessionID   string            `json:"sessionId,omitempty"`
	Session     string            `json:"session,omitempty"`
	Harness     string            `json:"harness,omitempty"`
	Model       string            `json:"model,omitempty"`
	Memories    []mishapMemoryRef `json:"memories,omitempty"`
	// New marks a report recorded after the owner last marked the reports
	// reviewed (never before the first review).
	New bool `json:"new,omitempty"`
	// ProjectHref filters the page to the row's scope, keeping the other filters.
	ProjectHref string `json:"-"`
}

// mishapMemoryRef is a memory a report named: its name and scope while the
// index holds it, Missing once it does not.
type mishapMemoryRef struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Project string `json:"project,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// mishapMemoryCount is a memory and the number of selected reports naming it.
type mishapMemoryCount struct {
	mishapMemoryRef
	Count int `json:"count"`
}

// mishapLink is one chip or window choice: its label, its count where it has
// one, and the page with it applied.
type mishapLink struct {
	Key, Label, Title string
	Count             int
	Active            bool
	Href              string
}

// mishapsQuery is the page's parsed filters. project is the ?project= value
// as the page links it ("global" for the global scope); slug is the scope it
// names ("" for the global scope).
type mishapsQuery struct {
	typ, project, slug, window string
	days                       int
}

// href is the page with these filters, omitting each one at its default.
func (q mishapsQuery) href() string {
	v := url.Values{}
	if q.typ != "" {
		v.Set("type", q.typ)
	}
	if q.project != "" {
		v.Set("project", q.project)
	}
	switch {
	case q.days > 0:
		v.Set("days", strconv.Itoa(q.days))
	case q.window != "" && q.window != mishapsDefaultWindow:
		v.Set("w", q.window)
	}
	if len(v) == 0 {
		return mishapsPath
	}
	return mishapsPath + "?" + v.Encode()
}

// mishapTypeFilters are the values ?type= takes.
func mishapTypeFilters() []string {
	out := make([]string, 0, len(core.MishapTypes)+1)
	for _, t := range core.MishapTypes {
		out = append(out, string(t))
	}
	return append(out, core.MishapUntyped)
}

// parseMishapsQuery reads the page's filters. A filter absent (or empty, as a
// form sends it) is its default; one present that the page cannot read is an
// error naming the values it takes, never a quiet default.
func parseMishapsQuery(v url.Values) (mishapsQuery, error) {
	q := mishapsQuery{typ: strings.TrimSpace(v.Get("type")), project: strings.TrimSpace(v.Get("project"))}
	if q.typ != "" && !slices.Contains(mishapTypeFilters(), q.typ) {
		return q, fmt.Errorf("invalid type %q: valid values are %s", q.typ, strings.Join(mishapTypeFilters(), ", "))
	}
	if q.project != "" {
		slug, err := validate.Project(q.project)
		if err != nil {
			return q, fmt.Errorf("invalid project %q: name a project's slug, or global for the global scope", q.project)
		}
		q.slug = slug
		if slug == "" {
			q.project = validate.GlobalProject
		}
	}
	q.window = strings.TrimSpace(v.Get("w"))
	if q.window != "" && !slices.Contains(store.RetrievalWindowKeys, q.window) {
		return q, fmt.Errorf("invalid w %q: valid values are %s", q.window, strings.Join(store.RetrievalWindowKeys, ", "))
	}
	if days := strings.TrimSpace(v.Get("days")); days != "" {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("invalid days %q: a positive number of days", days)
		}
		if q.window != "" {
			return q, fmt.Errorf("w %q and days %q name two windows: pass one", q.window, days)
		}
		q.days = n
	}
	return q, nil
}

func (s *Service) mishapsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q, err := parseMishapsQuery(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	now := time.Now()
	data := mishapsData{Type: q.typ, Project: q.project, Locked: lockedProjects(ctx)}
	if q.days > 0 {
		data.Days, data.Since = q.days, now.AddDate(0, 0, -q.days)
		data.WindowLabel = "last " + plural(q.days, "day", "days")
	} else {
		win := store.ResolveRetrievalWindow(cmp.Or(q.window, mishapsDefaultWindow), now)
		data.Window, data.Since = win.Key, win.Since
		data.WindowLabel = win.Label
		if win.Key != "all" {
			data.WindowLabel = "last " + win.Label
		}
	}
	filter := store.MishapFilter{Type: q.typ, Project: q.slug, ByProject: q.project != "",
		Since: data.Since, Limit: mishapsListLimit}
	reports, err := store.ListMishaps(ctx, s.db(ctx), filter)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	tally, err := store.CountMishaps(ctx, s.db(ctx), filter)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mems, err := s.mishapMemories(ctx, reports, tally.ByMemory)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	data.Types, data.Total = mishapTypeCounts(tally.ByType)
	data.Shown = data.Total
	if q.typ != "" {
		key := core.MishapType(q.typ)
		if q.typ == core.MishapUntyped {
			key = ""
		}
		data.Shown = tally.ByType[key]
	}
	review, err := s.mishapsReviewOf(ctx, r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Review = review
	sessOf := s.sourceSessionResolver(ctx)
	data.Mishaps = make([]mishapListRow, 0, len(reports))
	for _, rep := range reports {
		row := mishapListRow{
			ID: rep.ID, When: rep.At, Type: string(rep.Type), Meaning: rep.Type.Meaning(),
			Text:        core.MishapText(rep.Description, rep.Type),
			Description: rep.Description, Project: rep.Project, SessionID: rep.SessionID,
			ProjectHref: mishapsQuery{typ: q.typ, project: cmp.Or(rep.Project, validate.GlobalProject),
				window: q.window, days: q.days}.href(),
		}
		if rep.SessionID != "" {
			sess := sessOf(rep.SessionID)
			row.Session, row.Harness, row.Model = sess.Name, harnessOf(sess), sess.Model
		}
		for _, id := range uniqueIDs(rep.ItemIDs) {
			row.Memories = append(row.Memories, mems[id])
		}
		row.New = review != nil && review.At != nil && rep.At.After(*review.At)
		data.Mishaps = append(data.Mishaps, row)
	}
	data.Memories = make([]mishapMemoryCount, 0, len(tally.ByMemory))
	for id, n := range tally.ByMemory {
		data.Memories = append(data.Memories, mishapMemoryCount{mishapMemoryRef: mems[id], Count: n})
	}
	slices.SortFunc(data.Memories, func(a, b mishapMemoryCount) int {
		return cmp.Or(b.Count-a.Count, cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	data.Chips = mishapChips(q, data.Types, data.Total)
	data.Windows = mishapWindows(q)
	if q.project != "" {
		data.ClearProject = mishapsQuery{typ: q.typ, window: q.window, days: q.days}.href()
	}
	s.render(w, r, "mishaps", pageData{Title: "Mishaps", Active: "mishaps", Data: data})
}

// unreviewedMishaps counts the agent-reported mishaps recorded after the
// owner last marked them reviewed (every report before the first review),
// through db, the request's handle, so a locked project's reports are absent
// from the count itself. It is the one definition of "mishaps to review": the
// Overview's mishap item, the Mishaps screen's review line, its Mark reviewed
// and the sidebar's Mishaps badge all read it. A stamp that does not read
// counts every report: more, never fewer. at and reviewed are the stamp.
func (s *Service) unreviewedMishaps(ctx context.Context, db *sql.DB) (n int, at time.Time, reviewed bool, err error) {
	at, reviewed, serr := store.MishapsReviewedAt(ctx, db)
	if serr != nil {
		s.logger.Warn("console: mishaps reviewed stamp", "error", serr)
		at, reviewed = time.Time{}, false
	}
	n, err = store.CountMishapsAfter(ctx, db, at)
	return n, at, reviewed, err
}

// mishapsReviewOf reads where the review stands, through the request's
// handle, whatever the page's filters: the stamp, the reports after it, and
// the newest report, which a Mark reviewed marks the reports reviewed through.
// nil when no report exists.
func (s *Service) mishapsReviewOf(ctx context.Context, r *http.Request) (*mishapsReview, error) {
	db := s.db(ctx)
	newest, err := store.ListMishaps(ctx, db, store.MishapFilter{Limit: 1})
	if err != nil || len(newest) == 0 {
		return nil, err
	}
	rv := &mishapsReview{Through: core.FormatTime(newest[0].At), Return: returnPath(r)}
	n, at, reviewed, err := s.unreviewedMishaps(ctx, db)
	if err != nil {
		return nil, err
	}
	if reviewed {
		rv.At = &at
	}
	rv.New = n
	return rv, nil
}

// mishapsReviewed is POST /console/mishaps/reviewed: the owner marks the
// reports reviewed through the newest one the page showed (through, its time
// as the page sent it; now when absent, and never later than now), and the
// Overview's mishap item counts only the reports after it. The stamp never
// moves back: a mark through an older report than the stamp writes nothing
// and says so. Records mishaps.reviewed with the stamp, how many reports the
// mark covered, and by: console. Wrapped in ownerHands in Register: clearing
// the owner's queue is the owner's acknowledgement, so an agent's process
// cannot hide its own reports from the owner.
func (s *Service) mishapsReviewed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	through := now
	if raw := strings.TrimSpace(r.PostFormValue("through")); raw != "" {
		t, err := core.ParseTime(raw)
		if err != nil {
			s.formError(w, r, http.StatusBadRequest, "through must be a report's time, as the Mishaps page sends it")
			return
		}
		if t.Before(now) {
			through = t
		}
	}
	back := func(msg string) string {
		dest := safeNext(strings.TrimSpace(r.PostFormValue("return")))
		if dest == "/console/" {
			dest = mishapsPath
		}
		target, err := url.Parse(dest)
		if err != nil {
			target = &url.URL{Path: mishapsPath}
		}
		return withNotice(target, msg)
	}
	db := s.db(ctx)
	before, prev, _, err := s.unreviewedMishaps(ctx, db)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stamp, err := store.MarkMishapsReviewed(ctx, s.writeDB(), through)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	after, err := store.CountMishapsAfter(ctx, db, stamp)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	covered := max(before-after, 0)
	moved := !stamp.Equal(prev)
	if moved && s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind:    store.EventMishapsReviewed,
			Payload: map[string]any{"through": core.FormatTime(stamp), "reports": covered, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record mishaps reviewed", "error", err)
		}
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"reviewedAt": stamp, "reviewed": covered, "new": after})
		return
	}
	msg := "Marked " + plural(covered, "report", "reports") + " reviewed."
	if !moved {
		msg = "Already reviewed through that report."
	}
	http.Redirect(w, r, back(msg), http.StatusSeeOther)
}

// mishapMemories resolves every memory the reports and the tally name, by
// id, through the request's handle. One the index no longer holds is kept,
// marked Missing.
func (s *Service) mishapMemories(ctx context.Context, reports []store.MishapReport, byMemory map[string]int) (map[string]mishapMemoryRef, error) {
	var ids []string
	for _, rep := range reports {
		ids = append(ids, rep.ItemIDs...)
	}
	for id := range byMemory {
		ids = append(ids, id)
	}
	ids = uniqueIDs(ids)
	found, err := store.MemoriesByIDs(ctx, s.db(ctx), ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]mishapMemoryRef, len(ids))
	for _, id := range ids {
		if m, ok := found[id]; ok {
			out[id] = mishapMemoryRef{ID: id, Name: m.Name, Kind: string(m.Kind), Project: m.Project}
			continue
		}
		out[id] = mishapMemoryRef{ID: id, Missing: true}
	}
	return out, nil
}

// uniqueIDs returns ids without repeats or blanks, in first-seen order.
func uniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// mishapTypeCounts lays the counts by type out in vocabulary order, then the
// untyped reports, and totals them.
func mishapTypeCounts(byType map[core.MishapType]int) ([]mishapTypeCount, int) {
	out := make([]mishapTypeCount, 0, len(core.MishapTypes)+1)
	total := 0
	for _, t := range core.MishapTypes {
		out = append(out, mishapTypeCount{Type: string(t), Meaning: t.Meaning(), Count: byType[t]})
		total += byType[t]
	}
	out = append(out, mishapTypeCount{Type: core.MishapUntyped, Meaning: mishapUntypedMeaning, Count: byType[""]})
	return out, total + byType[""]
}

// mishapUntypedMeaning is the untyped chip's meaning.
const mishapUntypedMeaning = "Reports that named no type, among them every report written before types existed."

// mishapChips are the type filter's choices: every report, each type, and
// the untyped reports, each with its count.
func mishapChips(q mishapsQuery, types []mishapTypeCount, total int) []mishapLink {
	all := q
	all.typ = ""
	out := []mishapLink{{Key: "", Label: "All", Title: "Every report, whatever its type", Count: total,
		Active: q.typ == "", Href: all.href()}}
	for _, tc := range types {
		sel := q
		sel.typ = tc.Type
		out = append(out, mishapLink{Key: tc.Type, Label: tc.Type, Title: tc.Meaning, Count: tc.Count,
			Active: q.typ == tc.Type, Href: sel.href()})
	}
	return out
}

// mishapWindows are the window choices, keeping the other filters. A ?days=
// window marks none of them.
func mishapWindows(q mishapsQuery) []mishapLink {
	active := cmp.Or(q.window, mishapsDefaultWindow)
	out := make([]mishapLink, 0, len(store.RetrievalWindowKeys))
	for _, key := range store.RetrievalWindowKeys {
		sel := q
		sel.window, sel.days = key, 0
		out = append(out, mishapLink{Key: key, Label: windowLabels[key], Active: q.days == 0 && key == active, Href: sel.href()})
	}
	return out
}
