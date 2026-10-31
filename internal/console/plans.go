package console

// The Plans screen surfaces captured Claude Code plans (cc-plan notes): their
// lifecycle status, iteration count, cached subagent runs, and tracking tasks.
// It is one of the shared library screens: a phase-grouped rail beside a reader,
// windowed by ?w=. It also carries the owner escape hatch for Claude
// Code bug #20397 (an approval whose PostToolUse never fired): POST
// /console/plans/{slug}/approve flips the note to approved and creates the
// tracking task, exactly as the hook would have.

import (
	"context"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Plan sources: a "capture" row is a Claude Code plan-mode capture (cc-plan
// note, with an iteration/agents/basename); a "composed" row is a plain
// plans-as-composition plan (a note tagged plan:<slug> plus its tasks), which
// has none of the capture-only columns.
const (
	planSourceCapture  = "capture"
	planSourceComposed = "composed"
)

// Plan phases bucket a plan by the progress of its step tasks, driving the
// grouped list: in progress (a step is being worked), ready (open steps remain,
// or the plan has not started), done (all steps closed, or an abandoned
// capture).
const (
	planPhaseInProgress = "in_progress"
	planPhaseReady      = "ready"
	planPhaseDone       = "done"
)

// planRow is a display projection of one plan (capture or composed).
type planRow struct {
	NoteID     string `json:"noteId"`
	Slug       string `json:"slug"`     // plan:<slug> composition key
	Source     string `json:"source"`   // capture | composed
	Basename   string `json:"basename"` // CC plan file name without .md (captures only)
	Title      string `json:"title"`
	Project    string `json:"project,omitempty"`
	Status     string `json:"status"`
	Favorite   bool   `json:"favorite,omitempty"` // the primary note's star
	Iteration  int    `json:"iteration,omitempty"`
	Agents     int    `json:"agents"` // cached subagent notes in the composition
	TasksDone  int    `json:"tasksDone"`
	TasksOpen  int    `json:"tasksOpen"`       // open (not started, not closed) steps
	TasksWIP   int    `json:"tasksInProgress"` // steps currently being worked
	TasksTotal int    `json:"tasksTotal"`
	Phase      string `json:"phase"` // in_progress | ready | done
	// Updated is the COMPOSITION's last activity: the newest stamp across every
	// note tagged plan:<slug> and every plan-step task -- not the primary note's
	// own. A plan moves when any part of it moves, so keying recency to the
	// primary would drop an actively-worked plan out of the window (and sort it
	// low) merely because its narrative note had not been re-saved.
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

// plansData is the Plans library payload. Rows are one merged, newest-first
// list; the phase counts drive the three rail groups the template renders (in
// progress, then ready, then done). The Selected/QS fields drive the HTML
// reader pane only; JSON list callers get the same lean payload as before.
type plansData struct {
	Rows  []planRow `json:"rows"`
	Count int       `json:"count"` // rows inside the window
	// Total is every plan, all time -- what the sidebar badge counts. The headline
	// leads with it (window as qualifier) so the badge never appears to disagree
	// with the page it links to; Count scopes only the list.
	Total       int            `json:"total"`
	InProgress  int            `json:"inProgress"`
	Ready       int            `json:"ready"`
	Done        int            `json:"done"`
	Window      string         `json:"window"`
	WindowLabel string         `json:"windowLabel"`
	Windows     []windowOption `json:"-"`
	// Selected is the plan open in the reader: the requested one on a
	// /console/plans/{slug} page, or the first rail item on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *planDetailData `json:"-"`
	SelectedAuto bool            `json:"-"`
	// QS is the ?w= suffix rail links carry so the active window survives a
	// selection change ("" at the default).
	QS string `json:"-"`
	// ShippedMonth is the header's quiet momentum line: the real count of
	// plan.shipped settlements in the current LOCAL calendar month. nil while
	// the momentum feature is off -- zero trace, including in the JSON.
	ShippedMonth *planShippedMonth `json:"shippedThisMonth,omitempty"`
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

// planNoteRef is a compact pointer to a note attached to a plan composition.
type planNoteRef struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Slug    string    `json:"slug"`
	IsAgent bool      `json:"isAgent"` // an agent-cache note (vs a supporting note)
	Updated time.Time `json:"updated"`
}

// planTaskRef is a compact pointer to a plan step task.
type planTaskRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// planDetailData is the single-plan payload: the row, the rendered plan body,
// and the composition's attached notes and tasks.
type planDetailData struct {
	Row        planRow       `json:"plan"`
	Body       template.HTML `json:"-"`
	BodyText   string        `json:"body"`
	BodyLoaded bool          `json:"bodyAvailable"`
	Attached   []planNoteRef `json:"attached,omitempty"`
	Tasks      []planTaskRef `json:"tasks,omitempty"`
	CanApprove bool          `json:"canApprove"`
}

func (s *Service) plansList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), time.Now())
	data, err := s.plansPage(ctx, win)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the first rail item in the reader.
	if !wantsJSON(r) {
		if slug := defaultPlanSlug(data.Rows); slug != "" {
			d, found, derr := s.planDetailBySlug(ctx, slug)
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
	s.render(w, r, "plans", pageData{Title: "Plans", Active: "plans", Data: data})
}

// defaultPlanSlug picks the reader's default selection on the list URL: the
// first rail item in display order -- the newest in-progress plan, then ready,
// then done ("" when the window is empty).
func defaultPlanSlug(rows []planRow) string {
	for _, phase := range []string{planPhaseInProgress, planPhaseReady, planPhaseDone} {
		for _, r := range rows {
			if r.Phase == phase {
				return r.Slug
			}
		}
	}
	return ""
}

// planQS renders the ?w= suffix rail links carry so a selection change keeps
// the active window ("" at the 24h default).
func planQS(winKey string) string {
	if winKey == "24h" {
		return ""
	}
	return "?w=" + winKey
}

// plansPage assembles the merged, newest-first plan list for the given window.
func (s *Service) plansPage(ctx context.Context, win store.RetrievalWindow) (plansData, error) {
	agentCount, err := s.planAgentCounts(ctx)
	if err != nil {
		return plansData{}, err
	}
	tokens, err := store.PlanTokenRollups(ctx, s.cfg.DB)
	if err != nil {
		return plansData{}, err
	}
	// Every note carrying a plan:<slug> tag, both sources: composedPrimaries picks
	// the composed narratives out of this set, and the activity index spans all of
	// it (captures and agent caches carry the slug tag too).
	composed, err := store.NotesByTagPrefix(ctx, s.cfg.DB, "", plans.SlugTagPrefix())
	if err != nil {
		return plansData{}, err
	}
	activity := planNoteActivity(composed)
	// Captures are the primary of their slug. A cc-plan note also carries its
	// plan:<slug> tag, so track which (project, slug) pairs a capture already owns
	// and let composed plans fill only the rest.
	captures, err := store.NotesByTag(ctx, s.cfg.DB, "", plans.TagPlan)
	if err != nil {
		return plansData{}, err
	}
	rows := make([]planRow, 0, len(captures))
	owned := make(map[string]bool, len(captures))
	for _, n := range captures {
		row := s.planRow(ctx, n, agentCount, tokens, activity[planKey(n.Project, plans.SlugFromTags(n.Tags))])
		owned[planKey(n.Project, row.Slug)] = true
		rows = append(rows, row)
	}
	// Composed plans: any note carrying a plan:<slug> tag that is not itself a
	// capture. The earliest-created non-agent note is the narrative primary.
	for _, n := range composedPrimaries(composed, owned) {
		rows = append(rows, s.planRow(ctx, n, agentCount, tokens, activity[planKey(n.Project, plans.SlugFromTags(n.Tags))]))
	}
	// Total is every plan regardless of window -- the headline the sidebar badge
	// must agree with. Window-filter last so it applies uniformly across sources,
	// then sort newest-updated first. The template's phase grouping preserves that
	// order within each phase.
	total := len(rows)
	kept := rows[:0]
	for _, row := range rows {
		if win.Since.IsZero() || !row.Updated.Before(win.Since) {
			kept = append(kept, row)
		}
	}
	slices.SortFunc(kept, func(a, b planRow) int {
		if !a.Updated.Equal(b.Updated) {
			return b.Updated.Compare(a.Updated)
		}
		return strings.Compare(b.NoteID, a.NoteID)
	})
	data := plansData{
		Rows: kept, Count: len(kept), Total: total,
		Window: win.Key, WindowLabel: win.Label, Windows: windowOptions(win.Key),
		QS: planQS(win.Key),
	}
	for _, row := range kept {
		switch row.Phase {
		case planPhaseInProgress:
			data.InProgress++
		case planPhaseDone:
			data.Done++
		default:
			data.Ready++
		}
	}
	s.planShippedMomentum(ctx, &data)
	return data, nil
}

// planShippedMomentum fills the momentum surfaces of the Plans page: the
// header's monthly shipped count and the per-row settle marks, both judged
// from the plan.shipped settlements in the current local month. Gated on the
// feature (off = not computed, zero trace) and failure-soft the momentum way
// -- an error drops the whole surface rather than rendering a wrong zero.
func (s *Service) planShippedMomentum(ctx context.Context, data *plansData) {
	if !features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		return
	}
	now := time.Now().Local()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	refs, err := store.PlansShippedSince(ctx, s.cfg.DB, monthStart)
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
}

// composedPrimaries groups plan:<slug> notes by (project, slug) and returns the
// narrative primary of each composed plan: the earliest-created note that is
// neither an agent cache nor already owned by a capture. Groups with only agent
// caches, or whose slug a capture already represents, are skipped.
func composedPrimaries(notes []core.Note, owned map[string]bool) []core.Note {
	primary := make(map[string]core.Note)
	for _, n := range notes {
		if slices.Contains(n.Tags, plans.TagPlan) || slices.Contains(n.Tags, plans.TagAgent) {
			continue
		}
		slug := plans.SlugFromTags(n.Tags)
		if slug == "" {
			continue
		}
		key := planKey(n.Project, slug)
		if owned[key] {
			continue
		}
		if cur, ok := primary[key]; !ok || plans.EarlierPrimary(n, cur) {
			primary[key] = n
		}
	}
	out := make([]core.Note, 0, len(primary))
	for _, n := range primary {
		out = append(out, n)
	}
	return out
}

// planDetailBySlug assembles a plan's full detail payload: the row, the
// rendered plan body, and the composition's attached notes and step tasks.
// found=false means no note carries the plan:<slug> tag.
func (s *Service) planDetailBySlug(ctx context.Context, slug string) (planDetailData, bool, error) {
	planNote, attached, ok, err := s.planComposition(ctx, slug)
	if err != nil || !ok {
		return planDetailData{}, ok, err
	}
	agentCount, err := s.planAgentCounts(ctx)
	if err != nil {
		return planDetailData{}, false, err
	}
	tokens, err := store.PlanTokenRollups(ctx, s.cfg.DB)
	if err != nil {
		return planDetailData{}, false, err
	}
	// attached is every other note in the composition, so it carries the same
	// note-activity the list derives from its index -- keep the two in step.
	noteActivity := planNote.Updated
	for _, a := range attached {
		if a.Updated.After(noteActivity) {
			noteActivity = a.Updated
		}
	}
	d := planDetailData{Row: s.planRow(ctx, planNote, agentCount, tokens, noteActivity), Attached: attached}
	// The approve escape hatch is a CC-capture lifecycle action; composed plans
	// have no draft/presented/approved state to flip.
	d.CanApprove = d.Row.Source == planSourceCapture && d.Row.Status != plans.StatusApproved
	if s.cfg.Files != nil {
		if full, ferr := s.cfg.Files.Store().ReadNote(planNote.FilePath); ferr != nil {
			s.logger.Warn("console: read plan body", "slug", slug, "error", ferr)
		} else {
			d.BodyText = full.Body
			d.BodyLoaded = true
			d.Body = s.renderBody(ctx, full.Body, planNote.Project)
		}
	}
	tasks, err := store.ListTasksForPlan(ctx, s.cfg.DB, planNote.Project, "", slug)
	if err != nil {
		return planDetailData{}, false, err
	}
	for _, t := range tasks {
		d.Tasks = append(d.Tasks, planTaskRef{ID: t.ID, Title: t.Title, Status: string(t.Status)})
	}
	return d, true, nil
}

func (s *Service) planDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	d, found, err := s.planDetailBySlug(ctx, slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		s.notFound(w, r, "No plan with slug "+slug+".")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	// The peek fragment stays for the surfaces that open plans in a detail pane
	// (search results); the library screen itself swaps readers.
	if r.URL.Query().Get("peek") == "1" {
		s.renderFragment(w, r, "plan", d)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "plans", "plan-reader", d)
		return
	}
	// Default: the plans library page with this plan open in the reader.
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), time.Now())
	data, err := s.plansPage(ctx, win)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Selected = &d
	s.render(w, r, "plans", pageData{Title: "Plan " + d.Row.Title, Active: "plans", Data: data})
}

// planApprove is the owner escape hatch for Claude Code bug #20397 (approve +
// immediate /clear skips the ExitPlanMode PostToolUse): it flips the plan note
// to approved and ensures the tracking task, mirroring the hook path.
func (s *Service) planApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	planNote, _, ok, err := s.planComposition(ctx, slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
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
	http.Redirect(w, r, "/console/plans/"+slug, http.StatusSeeOther)
}

// planComposition resolves a plan slug to its primary note and the other notes
// tagged into the composition, projected for display. The selection rule lives
// in plans.Composition, shared with the favorite surfaces.
func (s *Service) planComposition(ctx context.Context, slug string) (core.Note, []planNoteRef, bool, error) {
	primary, others, ok, err := plans.Composition(ctx, s.cfg.DB, slug)
	if err != nil || !ok {
		return core.Note{}, nil, ok, err
	}
	var attached []planNoteRef
	for _, n := range others {
		attached = append(attached, planNoteRef{
			ID: n.ID, Title: n.Title, Slug: n.Slug,
			IsAgent: slices.Contains(n.Tags, plans.TagAgent), Updated: n.Updated,
		})
	}
	return primary, attached, true, nil
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
		key := n.Project + "\x00" + slug
		if cur, ok := act[key]; !ok || n.Updated.After(cur) {
			act[key] = n.Updated
		}
	}
	return act
}

// planAgentCounts counts agent-cache notes per plan slug, one query for the
// whole list.
func (s *Service) planAgentCounts(ctx context.Context) (map[string]int, error) {
	agents, err := store.NotesByTag(ctx, s.cfg.DB, "", plans.TagAgent)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(agents))
	for _, a := range agents {
		if slug := plans.SlugFromTags(a.Tags); slug != "" {
			counts[slug]++
		}
	}
	return counts, nil
}

// planRow projects a plan's primary note into a display row. The capture-only
// columns (basename, iteration) are read from the cc-plan note's frontmatter
// (best-effort); composed primaries carry neither.
//
// noteActivity is the newest stamp across the composition's OTHER notes (zero if
// unknown, which degrades to the primary's own); planRow folds in the step tasks
// itself, since it already loads them. See planRow.Updated.
func (s *Service) planRow(ctx context.Context, n core.Note, agentCount map[string]int, tokens map[store.PlanRef]store.PlanTokenRollup, noteActivity time.Time) planRow {
	slug := plans.SlugFromTags(n.Tags)
	isCapture := slices.Contains(n.Tags, plans.TagPlan)
	row := planRow{
		NoteID: n.ID, Slug: slug, Source: planSourceComposed,
		Title: n.Title, Project: n.Project,
		Status: plans.StatusFromTags(n.Tags), Favorite: n.Favorite,
		Agents:  agentCount[slug],
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
	if tasks, err := store.ListTasksForPlan(ctx, s.cfg.DB, n.Project, "", slug); err == nil {
		row.TasksTotal = len(tasks)
		for _, t := range tasks {
			switch t.Status {
			case core.TaskDone:
				row.TasksDone++
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
// closed with none open -- and ready when open steps remain or it has not
// started yet.
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

// phaseRows filters a newest-first plan list to one phase, preserving order, so
// the template can render each grouped section from the single merged slice.
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
	if _, err := s.cfg.Events.Record(ctx, core.Event{
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
