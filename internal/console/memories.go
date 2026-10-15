package console

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
)

// memorySortKeys are the accepted ?sort values on the memories list. recent is
// the default (newest-updated within each kind group); name orders A-Z, reach by
// injection count, utility by the decayed demand score, and favorites floats
// starred rows first.
var memorySortKeys = []string{"recent", "name", "reach", "utility", "favorites"}

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

type kindGroup struct {
	Kind     string      `json:"kind"`
	Memories []memoryRow `json:"memories"`
}

type projectGroup struct {
	Project string      `json:"project"`
	Count   int         `json:"count"`
	Kinds   []kindGroup `json:"kinds"`
}

// memoriesData is the payload for the Memories library screen. The Selected/QS
// fields drive the HTML reader pane only; JSON list callers get the same lean
// payload as before.
type memoriesData struct {
	Groups        []projectGroup `json:"groups"`
	Inactive      []memoryRow    `json:"inactive"`
	ActiveCount   int            `json:"activeCount"`
	InactiveCount int            `json:"inactiveCount"`
	Query         string         `json:"query,omitempty"`
	Sort          string         `json:"sort"`
	CanArchive    bool           `json:"-"`
	// Selected is the memory open in the reader: the requested one on a
	// /console/memories/{id} page, or the newest match on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *memoryDetail `json:"-"`
	SelectedAuto bool          `json:"-"`
	// QS is the ?q=&sort= suffix rail links carry so the active filter
	// survives a selection change ("" when both are at their defaults).
	QS string `json:"-"`
}

// memoriesPage assembles the grouped memories list for the given sort + filter.
func (s *Service) memoriesPage(ctx context.Context, sortKey, query string) (memoriesData, error) {
	q := strings.ToLower(query)
	s.freshRetrievalStats(ctx)
	mems, err := store.AllMemoriesIncludingInvalid(ctx, s.cfg.DB)
	if err != nil {
		return memoriesData{}, err
	}
	stats, err := store.AllRetrievalStats(ctx, s.cfg.DB)
	if err != nil {
		return memoriesData{}, err
	}

	nameByID := make(map[string]string, len(mems))
	for _, m := range mems {
		nameByID[m.ID] = m.Name
	}

	// project -> kind -> rows (active only); inactive collected separately. The
	// ?q filter applies to both sets.
	resolve := s.sourceSessionResolver(ctx)
	active := map[string]map[string][]memoryRow{}
	var inactive []memoryRow
	activeCount := 0
	for _, m := range mems {
		if !memoryMatches(m, q) {
			continue
		}
		row := toMemoryRow(m, stats[m.ID], nameByID, s.cfg.DataDir)
		row.Harness = harnessOfSource(resolve, m.SourceSession)
		if m.Active() {
			activeCount++
			if active[m.Project] == nil {
				active[m.Project] = map[string][]memoryRow{}
			}
			active[m.Project][string(m.Kind)] = append(active[m.Project][string(m.Kind)], row)
		} else {
			inactive = append(inactive, row)
		}
	}

	sortMemoryRows(inactive, sortKey)
	return memoriesData{
		Groups:        buildGroups(active, sortKey),
		Inactive:      inactive,
		ActiveCount:   activeCount,
		InactiveCount: len(inactive),
		Query:         query,
		Sort:          sortKey,
		CanArchive:    s.cfg.Files != nil,
		QS:            listQS(query, sortKey, "recent"),
	}, nil
}

func (s *Service) memoriesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sortKey, query, ok := s.parseSortQuery(w, r, memorySortKeys, "recent")
	if !ok {
		return
	}
	data, err := s.memoriesPage(ctx, sortKey, query)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the most relevant match in the reader.
	if !wantsJSON(r) {
		if id := defaultMemoryID(data); id != "" {
			d, found, derr := s.memoryDetailByID(ctx, id)
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
	s.render(w, r, "memories", pageData{Title: "Memories", Active: "memories", Data: data})
}

// defaultMemoryID picks the reader's default selection on the list URL: the
// most recently updated active match, falling back to the newest inactive one
// ("" when nothing matched).
func defaultMemoryID(data memoriesData) string {
	var id string
	var newest time.Time
	for _, g := range data.Groups {
		for _, k := range g.Kinds {
			for _, m := range k.Memories {
				if id == "" || m.Updated.After(newest) {
					id, newest = m.ID, m.Updated
				}
			}
		}
	}
	if id != "" {
		return id
	}
	for _, m := range data.Inactive {
		if id == "" || m.Updated.After(newest) {
			id, newest = m.ID, m.Updated
		}
	}
	return id
}

// memoryMatches reports whether a memory satisfies the ?q text filter (empty q
// matches all): a case-insensitive substring of name, description, kind, or any
// tag.
func memoryMatches(m core.Memory, q string) bool {
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
	return false
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
	sort.Slice(projects, func(i, j int) bool {
		if (projects[i] == "") != (projects[j] == "") {
			return projects[i] == "" // global first
		}
		return projects[i] < projects[j]
	})

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

// sortMemoryRows orders the rows within a kind group per the ?sort key: recent
// (newest-updated first, the default), name (A-Z), reach (most-injected
// first), utility (highest decayed demand first), or favorites (starred first).
// Ties fall back to name for a stable, readable order.
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

// memoryArchive retires an active memory (the console's one write to memory
// state), then redirects back to the browser.
func (s *Service) memoryArchive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Files == nil {
		s.serverError(w, r, errNoFiles)
		return
	}
	id := r.PathValue("id")
	idx, ok, err := store.MemoryByID(ctx, s.cfg.DB, id)
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
		http.Redirect(w, r, "/console/memories", http.StatusSeeOther)
		return
	}
	mem, err := s.cfg.Files.Store().ReadMemory(idx.FilePath)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	updated, err := lifecycle.Archive(ctx, s.cfg.Files, mem, "archived from console", time.Now().UTC())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.cfg.Events.Record(ctx, core.Event{
			Kind: core.EventMemoryArchived, ProjectSlug: updated.Project, ItemID: updated.ID,
			Payload: map[string]any{"name": updated.Name, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record archive event", "error", err)
		}
	}
	http.Redirect(w, r, "/console/memories?notice="+url.QueryEscape("Archived "+updated.Name+"."), http.StatusSeeOther)
}
