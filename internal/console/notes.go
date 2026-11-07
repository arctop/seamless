package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// noteSortKeys are the accepted ?sort values on the notes list.
var noteSortKeys = []string{"recent", "name", "favorites"}

// noteRow is a display projection of a note for the browser.
type noteRow struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Project     string    `json:"project"`
	Tags        []string  `json:"tags,omitempty"`
	Model       string    `json:"model,omitempty"` // producing model, verbatim
	Favorite    bool      `json:"favorite,omitempty"`
	Updated     time.Time `json:"updated"`
}

// noteProjectGroup is one project's notes, for the grouped browser (project ""
// is global).
type noteProjectGroup struct {
	Project string    `json:"project"`
	Count   int       `json:"count"`
	Notes   []noteRow `json:"notes"`
}

// notesData is the payload for the Notes library screen. The Selected/QS
// fields drive the HTML reader pane only; JSON list callers get the same lean
// payload as before.
type notesData struct {
	Groups []noteProjectGroup `json:"groups"`
	Count  int                `json:"count"`
	Query  string             `json:"query,omitempty"`
	Sort   string             `json:"sort"`
	// Selected is the note open in the reader: the requested one on a
	// /console/notes/{id} page, or the newest match on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *noteDetail `json:"-"`
	SelectedAuto bool        `json:"-"`
	// QS is the ?q=&sort= suffix rail links carry so the active filter
	// survives a selection change ("" when both are at their defaults).
	QS string `json:"-"`
}

// parseSortQuery validates ?sort against keys (empty -> def) and returns the
// trimmed ?q. ok=false means the 400 response was already written.
func (s *Service) parseSortQuery(w http.ResponseWriter, r *http.Request, keys []string, def string) (sortKey, query string, ok bool) {
	sortKey = r.URL.Query().Get("sort")
	if sortKey == "" {
		sortKey = def
	}
	if !slices.Contains(keys, sortKey) {
		s.badRequest(w, r, fmt.Sprintf("invalid sort %q: valid values are %s", sortKey, strings.Join(keys, ", ")))
		return "", "", false
	}
	return sortKey, strings.TrimSpace(r.URL.Query().Get("q")), true
}

// listQS renders the ?q=&sort= suffix for library rail links ("" when both are
// at their defaults), so changing the selection keeps the active filter.
func listQS(query, sortKey, defSort string) string {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if sortKey != defSort {
		v.Set("sort", sortKey)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// notesPage assembles the grouped notes list for the given sort + filter.
func (s *Service) notesPage(ctx context.Context, sortKey, query string) (notesData, error) {
	q := strings.ToLower(query)
	notes, err := store.ListNotes(ctx, s.db(ctx))
	if err != nil {
		return notesData{}, err
	}
	byProject := map[string][]noteRow{}
	matched := 0
	for _, n := range notes {
		row := noteRow{
			ID: n.ID, Title: n.Title, Description: n.Description,
			Project: n.Project, Tags: n.Tags, Model: n.Model, Favorite: n.Favorite,
			Updated: n.Updated,
		}
		if !noteMatches(row, q) {
			continue
		}
		byProject[n.Project] = append(byProject[n.Project], row)
		matched++
	}
	return notesData{
		Groups: buildNoteGroups(byProject, sortKey), Count: matched,
		Query: query, Sort: sortKey, QS: listQS(query, sortKey, "recent"),
	}, nil
}

func (s *Service) notesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sortKey, query, ok := s.parseSortQuery(w, r, noteSortKeys, "recent")
	if !ok {
		return
	}
	data, err := s.notesPage(ctx, sortKey, query)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the newest match in the reader.
	if !wantsJSON(r) {
		if id := newestNoteID(data.Groups); id != "" {
			d, found, derr := s.noteDetailByID(ctx, id)
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
	s.render(w, r, "notes", pageData{Title: "Notes", Active: "notes", Data: data})
}

// newestNoteID picks the reader's default selection on the list URL: the most
// recently updated note across every group ("" when the list is empty).
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
// matches all): a case-insensitive substring of title, description, or any tag.
func noteMatches(row noteRow, q string) bool {
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
	return false
}

// buildNoteGroups orders the project->notes map: global ("") first, then
// projects alphabetically. Within a group notes sort newest-updated first for
// sort=recent, by title for sort=name, or float starred rows (keeping
// newest-first within each partition) for sort=favorites.
func buildNoteGroups(byProject map[string][]noteRow, sortKey string) []noteProjectGroup {
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool {
		if (projects[i] == "") != (projects[j] == "") {
			return projects[i] == "" // global first
		}
		return projects[i] < projects[j]
	})
	groups := make([]noteProjectGroup, 0, len(projects))
	for _, p := range projects {
		rows := byProject[p]
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
		groups = append(groups, noteProjectGroup{Project: p, Count: len(rows), Notes: rows})
	}
	return groups
}
