package console

// The Settings briefing preview (plan:console-levels T12): the <seam-briefing> a
// new session in one project would start with, under the knobs the Briefing
// form holds right now -- saved or not -- so a preset or a knob has a picture
// instead of only a number.
//
// A preview is a read. It writes no settings row, and it records no event: no
// agent received it, and the utility signal counts only what agents asked for,
// so a preview logged as an injection would credit exposure that never happened
// (constraint closed-loop-utility-signal-contract).

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// Where a preview's knobs came from.
const (
	previewKnobsForm  = "form"  // POST: the Briefing form's values, saved or not
	previewKnobsSaved = "saved" // GET: the effective file/env + override values
)

// briefingPreview is the preview panel's payload: the fragment's dot, and the
// answer JSON callers get.
type briefingPreview struct {
	Project string `json:"project"`
	// Knobs is previewKnobsForm or previewKnobsSaved.
	Knobs string `json:"knobs"`
	// Text is the assembled briefing; "" means a new session in the project
	// gets no briefing at all (nothing is in scope).
	Text string `json:"briefing"`
	// Tokens is the estimate the assembler budgets with; Budget is what it packs
	// droppable rows into, and HardCap where it cuts the whole.
	Tokens  int `json:"tokens"`
	Budget  int `json:"budget"`
	HardCap int `json:"hardCap"`
}

// OverBudget reports a briefing past its packing budget. Only the pinned
// sections (constraints, pinned stages, starred rows) can put it there: they
// are never dropped for budget.
func (p briefingPreview) OverBudget() bool { return p.Tokens > p.Budget }

// Capped reports a briefing cut at the hard cap. hardTruncate returns anything
// under the cap unchanged, and what it cuts comes back one token over it.
func (p briefingPreview) Capped() bool { return p.Tokens > p.HardCap }

// FillPct is the budget meter's fill, 0-100.
func (p briefingPreview) FillPct() int {
	if p.Budget <= 0 {
		return 0
	}
	return min(100, p.Tokens*100/p.Budget)
}

// The meter's numbers, digit-grouped for reading ("1,212").
func (p briefingPreview) TokensLabel() string  { return groupDigits(p.Tokens) }
func (p briefingPreview) BudgetLabel() string  { return groupDigits(p.Budget) }
func (p briefingPreview) HardCapLabel() string { return groupDigits(p.HardCap) }

// groupDigits writes a non-negative count with thousands separators.
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	if head > 0 {
		b.WriteString(s[:head])
	}
	for i := head; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// settingsBriefingPreview answers GET and POST
// /console/settings/briefing/preview. POST previews the posted Briefing form --
// the owner's unsaved values -- through briefingFromForm, the save's own parse
// and validation, so a preview never accepts knobs a save would refuse; GET
// previews the saved, effective knobs. The project comes from the "project"
// field (POST) or query parameter (GET) and must name a registered, live
// project. One assembly per request. The panel gets an HTML fragment; JSON
// callers get the payload.
func (s *Service) settingsBriefingPreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.Retrieve == nil {
		s.previewError(w, r, http.StatusServiceUnavailable,
			"the briefing preview is unavailable: this console runs without the retrieval service")
		return
	}
	var knobs config.Briefing
	knobsFrom := previewKnobsSaved
	projectValues, projectPresent := r.URL.Query()["project"]
	if r.Method == http.MethodPost {
		b, err := briefingFromForm(r)
		if err != nil {
			s.previewError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		knobs, knobsFrom = b, previewKnobsForm
		projectValues, projectPresent = r.PostForm["project"]
	} else {
		b, _, err := store.BriefingConfig(ctx, s.db(ctx), s.cfg.BriefingCfg)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		knobs = b
	}
	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	project, err := pickPreviewProject(liveProjectSlugs(projects), projectValues, projectPresent)
	if err != nil {
		s.previewError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	text, err := s.cfg.Retrieve.PreviewBriefing(ctx, project, knobs)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	budget, hardCap := s.cfg.Retrieve.BriefingBudget(knobs)
	p := briefingPreview{
		Project: project, Knobs: knobsFrom, Text: text,
		Tokens: retrieve.EstimateTokens(text), Budget: budget, HardCap: hardCap,
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, p)
		return
	}
	s.writePreviewFragment(w, r, http.StatusOK, "briefing-preview", p)
}

// liveProjectSlugs lists the projects a preview can be for: registered and not
// retired, in the order given (store.ListProjects sorts by slug).
func liveProjectSlugs(projects []core.Project) []string {
	var live []string
	for _, p := range projects {
		if !p.Retired() {
			live = append(live, p.Slug)
		}
	}
	return live
}

// pickPreviewProject holds the project parameter to the input-boundary rule:
// given exactly once, naming a live project. Every refusal names the valid
// values; none falls back to a default.
func pickPreviewProject(live, values []string, present bool) (string, error) {
	if len(live) == 0 {
		return "", fmt.Errorf("no projects are registered yet: a preview needs a project an agent has worked in")
	}
	valid := strings.Join(live, ", ")
	switch {
	case !present:
		return "", fmt.Errorf("project is required: registered projects are %s", valid)
	case len(values) != 1:
		return "", fmt.Errorf("project must be given exactly once")
	case !slices.Contains(live, values[0]):
		return "", fmt.Errorf("unknown project %q: registered projects are %s", values[0], valid)
	}
	return values[0], nil
}

// previewError answers a refused preview: JSON callers get {"error": ...}, the
// panel a fragment carrying the same error flash the save shows.
func (s *Service) previewError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	s.writePreviewFragment(w, r, status, "briefing-preview-error", msg)
}

// writePreviewFragment writes one block of the settings template as a
// standalone fragment with the given status. A preview is computed from the
// request, so nothing may cache it.
func (s *Service) writePreviewFragment(w http.ResponseWriter, r *http.Request, status int, block string, data any) {
	tmpl, ok := s.pages["settings"]
	if !ok {
		s.serverError(w, r, fmt.Errorf("console: no settings page"))
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, block, data); err != nil {
		s.serverError(w, r, fmt.Errorf("console: render %s: %w", block, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
