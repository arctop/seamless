package console

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/markdown"
	"github.com/arctop/seamless/internal/store"
)

// The detail handlers here serve a single entity four ways: JSON for the CLI,
// an HTML fragment for the in-page detail pane (?peek=1), a reader fragment
// for the library screens' in-place swap (?reader=1), and -- by default -- the
// entity's library
// page with that entity open in the reader (a shareable, no-JS URL). Entity
// references across the console link to these routes; on split list screens
// (sessions) a data-peek link loads the ?peek=1 fragment into the pane without
// a page navigation.

// memoryRef is a compact pointer to another memory (a supersession neighbor),
// enough to render a peek link.
type memoryRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Project string `json:"project,omitempty"`
}

// memoryDetail is the payload for a single memory's peek/detail. Body is the
// rendered markdown HTML the template shows; BodyText is the raw source (JSON +
// the no-files fallback), so the two never double-escape each other.
type memoryDetail struct {
	ID           string        `json:"id"`
	Kind         string        `json:"kind"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Project      string        `json:"project"`
	Status       string        `json:"status"` // active|superseded|archived
	Body         template.HTML `json:"-"`
	BodyText     string        `json:"body"`
	BodyLoaded   bool          `json:"bodyAvailable"`
	Tags         []string      `json:"tags,omitempty"`
	Created      time.Time     `json:"created"`
	Updated      time.Time     `json:"updated"`
	Injects      int           `json:"injects"`
	Reads        int           `json:"reads"`
	LastInjected *time.Time    `json:"lastInjected,omitempty"`
	LastRead     *time.Time    `json:"lastRead,omitempty"`
	// Utility is the normalized time-decayed demand score ranking feeds on;
	// UtilityParts is its per-signal breakdown so the number is inspectable.
	Utility      float64                  `json:"utility,omitempty"`
	UtilityParts *store.UtilityComponents `json:"utilityParts,omitempty"`
	// SurfacedAge is how long ago this memory last entered an agent context
	// ("4m", "47d", or "never"), and SurfacedTone the staleness verdict behind
	// its chip. These two -- utility and staleness -- are the decision signals
	// a reader needs first, so the reader leads with them and demotes the rest.
	SurfacedAge  string `json:"-"`
	SurfacedTone string `json:"-"` // "ok" (<=7d) | "" (7-45d) | "warn" (>45d or long-unseen)
	// BodyIsEcho reports that the stored body adds nothing to the description:
	// absent, or the same sentence again. The reader says so instead of
	// printing one sentence twice and calling the second one the body.
	BodyIsEcho   bool        `json:"-"`
	Source       string      `json:"sourceSession,omitempty"`   // session name
	SourceID     string      `json:"sourceSessionId,omitempty"` // resolved ULID, for a link
	Model        string      `json:"model,omitempty"`           // producing model, as the provider names it
	Harness      string      `json:"harness,omitempty"`         // producing client, resolved from the source session
	Account      string      `json:"account,omitempty"`         // restricted account that wrote the content ("" = owner)
	Favorite     bool        `json:"favorite,omitempty"`
	ReplacedBy   string      `json:"replacedBy,omitempty"` // name of the superseder
	ReplacedByID string      `json:"replacedById,omitempty"`
	Supersedes   []memoryRef `json:"supersedes,omitempty"` // reverse: memories this replaced
	FilePath     string      `json:"filePath"`
	AbsPath      string      `json:"absPath"`
	// Level is the console level, for the reader's one gate (the utility
	// score): the reader also renders standalone as a ?reader=1 fragment, with
	// no page around it to ask. Presentation only -- never in the JSON.
	Level      level        `json:"-"`
	EditURL    template.URL `json:"-"`
	CanArchive bool         `json:"-"`
}

// memoryDetailData projects an index row into the full detail payload: body
// from the file, retrieval stats, provenance, and supersession both ways.
func (s *Service) memoryDetailData(ctx context.Context, m core.Memory) (memoryDetail, error) {
	status := "active"
	if !m.Active() {
		if m.SupersededBy != "" {
			status = "superseded"
		} else {
			status = "archived"
		}
	}
	abs, edit := absAndEditURL(s.cfg.DataDir, m.FilePath)
	d := memoryDetail{
		Level: s.consoleLevel(ctx).Level,
		ID:    m.ID, Kind: string(m.Kind), Name: m.Name, Description: m.Description,
		Project: m.Project, Status: status, Tags: m.Tags,
		Created: m.Created, Updated: m.Updated, Source: m.SourceSession, Model: m.Model,
		Account: m.Account, Favorite: m.Favorite,
		FilePath: m.FilePath, AbsPath: abs, EditURL: edit,
		CanArchive: s.cfg.Files != nil && m.Active(),
	}

	// Body lives in the file, not the index. A nil Files subsystem or a read
	// error leaves the metadata intact with the body simply unavailable.
	if s.cfg.Files != nil {
		if full, ferr := s.cfg.Files.Store().ReadMemory(m.FilePath); ferr != nil {
			s.logger.Warn("console: read memory body", "id", m.ID, "error", ferr)
		} else {
			d.BodyText = full.Body
			d.BodyLoaded = true
			d.Body = s.renderBody(ctx, full.Body, m.Project)
		}
	}

	if stat, found, serr := store.GetRetrievalStat(ctx, s.db(ctx), m.ID); serr != nil {
		s.logger.Warn("console: memory retrieval stat", "id", m.ID, "error", serr)
	} else if found {
		d.Injects, d.Reads = stat.InjectCount, stat.ReadCount
		d.LastInjected, d.LastRead = stat.LastInjectedAt, stat.LastReadAt
		d.Utility = stat.Utility
		if !stat.Components.IsZero() {
			c := stat.Components
			d.UtilityParts = &c
		}
	}
	d.SurfacedAge, d.SurfacedTone = surfacedStaleness(d.LastInjected, m.Created, time.Now())
	d.BodyIsEcho = bodyEchoesDescription(d.BodyText, m.Description)

	// Provenance: SourceSession holds a session name (ambient stamps) or a
	// session ULID (bound stamps); resolve either to the row for the link and
	// the harness half of the attribution pill.
	resolve := s.sourceSessionResolver(ctx)
	if sess := resolve(m.SourceSession); sess.ID != "" {
		d.SourceID = sess.ID
	}
	d.Harness = harnessOfSource(resolve, m.SourceSession)

	// Supersession, both directions.
	if m.SupersededBy != "" {
		if by, found, serr := store.MemoryByID(ctx, s.db(ctx), m.SupersededBy); serr != nil {
			s.logger.Warn("console: memory superseder", "id", m.SupersededBy, "error", serr)
		} else if found {
			d.ReplacedBy, d.ReplacedByID = by.Name, by.ID
		}
	}
	superseded, err := store.MemoriesSuperseding(ctx, s.db(ctx), m.ID)
	if err != nil {
		return memoryDetail{}, err
	}
	for _, sm := range superseded {
		d.Supersedes = append(d.Supersedes, memoryRef{
			ID: sm.ID, Name: sm.Name, Kind: string(sm.Kind), Project: sm.Project,
		})
	}
	return d, nil
}

// memoryDetailByID loads a memory and projects it into its detail payload.
// found=false means no such memory.
func (s *Service) memoryDetailByID(ctx context.Context, id string) (memoryDetail, bool, error) {
	m, ok, err := store.MemoryByID(ctx, s.db(ctx), id)
	if err != nil || !ok {
		return memoryDetail{}, ok, err
	}
	d, err := s.memoryDetailData(ctx, m)
	if err != nil {
		return memoryDetail{}, false, err
	}
	return d, true, nil
}

func (s *Service) memoryDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	m, ok, err := store.MemoryByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No memory with id "+id+".")
		return
	}
	d, err := s.memoryDetailData(ctx, m)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	if r.URL.Query().Get("peek") == "1" {
		s.renderFragment(w, r, "memory", d)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "memories", "memory-reader", d)
		return
	}
	sortKey, query, ok2 := s.parseSortQuery(w, r, memorySortKeys, "recent")
	if !ok2 {
		return
	}
	data, err := s.memoriesPage(ctx, sortKey, query)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Selected = &d
	s.render(w, r, "memories", pageData{Title: "Memory " + m.Name, Active: "memories", Data: data})
}

// noteDetail is the payload for a single note's peek/detail.
type noteDetail struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Slug        string        `json:"slug"`
	Description string        `json:"description"`
	Project     string        `json:"project"`
	Body        template.HTML `json:"-"`
	BodyText    string        `json:"body"`
	BodyLoaded  bool          `json:"bodyAvailable"`
	Words       int           `json:"words,omitempty"`
	Tags        []string      `json:"tags,omitempty"`
	SourceURL   string        `json:"sourceUrl,omitempty"`
	Model       string        `json:"model,omitempty"`   // producing model, as the provider names it
	Account     string        `json:"account,omitempty"` // restricted account that wrote the content ("" = owner)
	Favorite    bool          `json:"favorite,omitempty"`
	Created     time.Time     `json:"created"`
	Updated     time.Time     `json:"updated"`
	FilePath    string        `json:"filePath"`
	AbsPath     string        `json:"absPath"`
	EditURL     template.URL  `json:"-"`
}

// noteDetailData projects an index row into the full detail payload, reading
// the body from the file (a nil Files subsystem degrades to metadata only).
func (s *Service) noteDetailData(ctx context.Context, n core.Note) noteDetail {
	abs, edit := absAndEditURL(s.cfg.DataDir, n.FilePath)
	d := noteDetail{
		ID: n.ID, Title: n.Title, Slug: n.Slug, Description: n.Description,
		Project: n.Project, Tags: n.Tags, SourceURL: n.SourceURL, Model: n.Model,
		Account: n.Account, Favorite: n.Favorite, Created: n.Created, Updated: n.Updated,
		FilePath: n.FilePath, AbsPath: abs, EditURL: edit,
	}
	if s.cfg.Files != nil {
		if full, ferr := s.cfg.Files.Store().ReadNote(n.FilePath); ferr != nil {
			s.logger.Warn("console: read note body", "id", n.ID, "error", ferr)
		} else {
			d.BodyText = full.Body
			d.BodyLoaded = true
			d.Words = len(strings.Fields(full.Body))
			d.Body = s.renderBody(ctx, full.Body, n.Project)
		}
	}
	return d
}

// noteDetailByID loads a note and projects it into its detail payload.
// found=false means no such note.
func (s *Service) noteDetailByID(ctx context.Context, id string) (noteDetail, bool, error) {
	n, ok, err := store.NoteByID(ctx, s.db(ctx), id)
	if err != nil || !ok {
		return noteDetail{}, ok, err
	}
	return s.noteDetailData(ctx, n), true, nil
}

func (s *Service) noteDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	n, ok, err := store.NoteByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No note with id "+id+".")
		return
	}
	d := s.noteDetailData(ctx, n)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	if r.URL.Query().Get("peek") == "1" {
		s.renderFragment(w, r, "note", d)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "notes", "note-reader", d)
		return
	}
	sortKey, query, ok2 := s.parseSortQuery(w, r, noteSortKeys, "recent")
	if !ok2 {
		return
	}
	data, err := s.notesPage(ctx, sortKey, query)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Selected = &d
	s.render(w, r, "notes", pageData{Title: "Note " + n.Title, Active: "notes", Data: data})
}

// taskRef is a compact pointer to a related task (a dependency or a dependent).
type taskRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// taskDetail is the payload for a single task's peek/detail. Unlike the Tasks
// list rows, it renders the task Body and resolves both dependency directions.
type taskDetail struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	Status   string `json:"status"`
	Favorite bool   `json:"favorite,omitempty"`
	// Body is the rendered markdown HTML the template shows; BodyText is the raw
	// source (JSON output), so the two never double-escape each other.
	Body        template.HTML `json:"-"`
	BodyText    string        `json:"body"`
	PlanSlug    string        `json:"planSlug,omitempty"`
	CreatedBy   string        `json:"createdBy,omitempty"`
	ClaimedBy   string        `json:"claimedBy,omitempty"`
	LeaseExpiry *time.Time    `json:"leaseExpiry,omitempty"`
	ClaimLive   bool          `json:"claimLive,omitempty"` // lease unexpired as of render time
	Deps        []taskRef     `json:"deps,omitempty"`      // tasks this one depends on
	Blocks      []taskRef     `json:"blocks,omitempty"`    // tasks that depend on this one
	Created     time.Time     `json:"created"`
	Updated     time.Time     `json:"updated"`
	Closed      *time.Time    `json:"closed,omitempty"`
	// Account is the restricted local account that created the task and
	// UpdatedAccount the one that last changed it; "" for the owner.
	Account        string `json:"account,omitempty"`
	UpdatedAccount string `json:"updatedAccount,omitempty"`
	// Next is where this fragment's owner actions (star, release) return to,
	// when the surface that fetched it asked for one. Empty leaves each action
	// on its own default -- the task page or the tasks library.
	Next string `json:"-"`
}

// taskDetailData projects a task into the full detail payload, resolving both
// dependency directions.
func (s *Service) taskDetailData(ctx context.Context, t core.Task) (taskDetail, error) {
	d := taskDetail{
		ID: t.ID, Title: t.Title, Project: t.ProjectSlug, Status: string(t.Status),
		Favorite: t.Favorite,
		BodyText: t.Body, PlanSlug: t.PlanSlug, CreatedBy: t.CreatedBy,
		ClaimedBy: t.ClaimedBy, LeaseExpiry: t.LeaseExpiresAt,
		Account: t.Account, UpdatedAccount: t.UpdatedAccount,
		ClaimLive: t.ClaimLive(time.Now().UTC()),
		Created:   t.CreatedAt, Updated: t.UpdatedAt, Closed: t.ClosedAt,
	}
	// Task bodies get the same markdown + wiki-link treatment as memory/note
	// bodies, scoped to the task's project.
	d.Body = s.renderBody(ctx, t.Body, t.ProjectSlug)
	for _, depID := range t.DependsOn {
		dep, derr := store.TaskByID(ctx, s.db(ctx), depID)
		if derr != nil {
			if errors.Is(derr, store.ErrTaskNotFound) {
				continue // a dangling dep edge should not 500 the page
			}
			return taskDetail{}, derr
		}
		d.Deps = append(d.Deps, taskRef{ID: dep.ID, Title: dep.Title, Status: string(dep.Status)})
	}
	blocks, err := store.TasksBlockedBy(ctx, s.db(ctx), t.ID)
	if err != nil {
		return taskDetail{}, err
	}
	for _, b := range blocks {
		d.Blocks = append(d.Blocks, taskRef{ID: b.ID, Title: b.Title, Status: string(b.Status)})
	}
	return d, nil
}

// taskDetailByID loads a task and projects it into its detail payload.
// found=false means no such task.
func (s *Service) taskDetailByID(ctx context.Context, id string) (taskDetail, bool, error) {
	t, err := store.TaskByID(ctx, s.db(ctx), id)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			return taskDetail{}, false, nil
		}
		return taskDetail{}, false, err
	}
	d, err := s.taskDetailData(ctx, t)
	if err != nil {
		return taskDetail{}, false, err
	}
	return d, true, nil
}

func (s *Service) taskDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	t, err := store.TaskByID(ctx, s.db(ctx), id)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			s.notFound(w, r, "No task with id "+id+".")
			return
		}
		s.serverError(w, r, err)
		return
	}
	d, err := s.taskDetailData(ctx, t)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	if r.URL.Query().Get("peek") == "1" {
		// An embedding surface (the project workspace's step dossier) passes the
		// view it wants the fragment's actions to land back on.
		if next := strings.TrimSpace(r.URL.Query().Get("next")); next != "" {
			d.Next = safeNext(next)
		}
		s.renderFragment(w, r, "task", d)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "tasks", "task-reader", d)
		return
	}
	data, err := s.tasksPage(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Selected = &d
	s.render(w, r, "tasks", pageData{Title: "Task " + shortID(t.ID), Active: "tasks", Data: data})
}

// projectDetail is the payload for a single project's peek/detail: its metadata
// plus live per-channel counts, each linking to the filtered screen.
type projectDetail struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Memories    int       `json:"memories"`
	Sessions    int       `json:"sessions"`
	OpenTasks   int       `json:"openTasks"`
	Notes       int       `json:"notes"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	// The agent-facing fence, so the CLI's read path (`seam project isolation
	// <slug>` with no state) can print the state and its promise without a POST.
	Isolation        core.Isolation `json:"isolation"`
	IsolationPromise string         `json:"isolationPromise"`
}

// The project detail handler (dispatcher + tabbed workspace) lives in
// project_detail.go; the thin projectDetail struct above is the peek/CLI summary
// it renders via projectSummary.

// renderBody renders a memory/note/task body -- or session findings -- as
// sanitized markdown HTML for the peek panel and the no-JS detail page. [[name]]
// references resolve to memory peek links (data-peek), scoped to the item's
// project with a global fallback (the same rule recall uses); unresolved
// references stay as literal text. The raw source is kept separately in each
// detail's BodyText field, so rendered and raw never double-escape each other.
func (s *Service) renderBody(ctx context.Context, body, project string) template.HTML {
	return markdown.Render(body, s.wikiResolver(ctx, project))
}

// wikiResolver maps a normalized [[name]] reference to a memory peek link,
// resolving the name against the store scoped to project with a global fallback.
func (s *Service) wikiResolver(ctx context.Context, project string) markdown.WikiResolver {
	return func(name string) (string, bool) {
		m, ok, err := store.MemoryByName(ctx, s.db(ctx), project, name)
		if err != nil {
			s.logger.Warn("console: resolve wiki-link", "name", name, "error", err)
			return "", false
		}
		if !ok && project != "" {
			m, ok, err = store.MemoryByName(ctx, s.db(ctx), "", name)
			if err != nil {
				s.logger.Warn("console: resolve wiki-link (global)", "name", name, "error", err)
				return "", false
			}
		}
		if !ok {
			return "", false
		}
		return "/console/memories/" + m.ID, true
	}
}
