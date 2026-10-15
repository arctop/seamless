package console

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
)

//go:embed templates
var templateFS embed.FS

//go:embed static/console.css
var consoleCSS []byte

//go:embed static/interactions.js
var interactionsJS []byte

//go:embed static/search.js
var searchJS []byte

//go:embed static/library.js
var libraryJS []byte

//go:embed static/charts.js
var chartsJS []byte

//go:embed static/navigation.js
var navigationJS []byte

//go:embed static/shell.js
var shellJS []byte

//go:embed static/sky.js
var skyJS []byte

//go:embed static/favicon.svg
var faviconSVG []byte

// pageNames are the page templates; each is parsed together with the shared
// layout so it can supply the "content" (and optional "scripts") blocks. Pages
// are added here as their handlers land, phase by phase.
var pageNames = []string{
	"login", "overview", "now", "search", "interactions", "projects", "projectdetail", "context", "sessions", "session",
	"memories", "notes", "retrieval", "tasks", "plans", "labs", "trials", "gardener", "settings", "event", "error",
	"feature_off",
}

// peekNames are the entity detail templates. Each templates/peek_<name>.html
// defines a "peek-body" block (the standalone ?peek=1 fragment) wrapping a
// "detail-body" block (the entity's one body source).
var peekNames = []string{"memory", "note", "task", "project", "session", "event", "plan", "trial"}

// entityPeekPages are the entities whose bespoke full page composes the shared
// "detail-body" block from templates/peek_<name>.html, so the peek fragment and
// the full page render one source (page chrome aside). session and project are
// absent on purpose: their fragments are deliberate compact summaries of much
// richer bespoke surfaces (the session workspace, the tabbed project workspace);
// memory, note, task, and plan are absent because their full pages are the
// library screens, whose "<entity>-reader" blocks render the richer document view.
var entityPeekPages = map[string]bool{"event": true}

// pageData is the envelope every rendered page receives. Data holds the
// page-specific payload; Nav/Active/Title drive the shared chrome.
type pageData struct {
	Title  string
	Active string // nav key to highlight
	// Screen is the registry screen this page IS, for the level banner, when
	// that differs from the nav entry it highlights (Context highlights
	// Projects but is its own, advanced, screen). Empty means Active.
	Screen   string
	Nav      navCounts
	Host     string // machine this console runs on, for the sidebar account row
	HostName string // the same machine's bare hostname, for sameHost comparisons
	Notice   string // positive flash banner (from ?notice=)
	FlashErr string // error flash banner (from ?error=)
	// Features is the effective optional-feature state for this request; page
	// templates reach it as $.Features to hide a feature's copy.
	Features config.Features
	// Level is the console level in force for this request; page templates
	// gate level-dependent markup with {{if $.Level.AtLeast "standard"}}.
	Level level
	// LevelOverridden reports a stored level row in force (else file/env).
	LevelOverridden bool
	// Screens are the screens offered at this level and feature state, in
	// registry order; NavGroups lays the ones with a nav row out as sidebar
	// sections. A screen missing here keeps working by URL.
	Screens   []screen
	NavGroups []navGroup
	// LevelBanner is set when this page's screen is above the current level:
	// it still renders in full, under a note offering the switch.
	LevelBanner *levelBanner
	// UpdateBanner is set on every page for a day after the daemon upgraded
	// (updates.go); nil otherwise, and whenever no update check runs here.
	UpdateBanner *updateBanner
	// UpdateAlert is set on every page while automatic updates are in
	// trouble -- paused, or the last update the daemon started did not apply
	// (updates_auto.go); nil otherwise, and whenever no update check runs here.
	UpdateAlert *updateAlert
	// ReturnPath is this page's own URL (flash params stripped), for forms that
	// change presentation state and come back here -- the banner's switch.
	ReturnPath string
	// VisibleScopes are the search scopes this level offers, space-separated,
	// for the palette to narrow its server results to (the JSON itself is
	// level-blind).
	VisibleScopes string
	// Jumps are the palette's Jump to rows that have no sidebar link: the
	// screens without a nav row and the Settings sections this level offers.
	Jumps []jumpEntry
	Data  any
}

// jumpEntry is one hidden palette row the layout renders inside the sidebar
// nav, so it re-renders (and search.js re-reads it) whenever the level changes
// what the sidebar offers.
type jumpEntry struct {
	Title string
	Hint  string
	Href  string
	Icon  string
}

// jumpEntries lists the palette rows with no sidebar link at this level: the
// visible screens without a nav row (Search, Context), then the visible
// Settings sections as "Settings > <section>".
func jumpEntries(visible []screen, lvl level) []jumpEntry {
	var out []jumpEntry
	for _, sc := range visible {
		if !sc.NavRow {
			out = append(out, jumpEntry{Title: sc.Label, Hint: sc.Hint, Href: sc.Href, Icon: sc.Icon})
		}
	}
	for _, sec := range visibleSettingsSections(lvl) {
		out = append(out, jumpEntry{Title: "Settings \u203a " + sec.Label, Hint: sec.Blurb, Href: sec.Href(), Icon: sec.Icon})
	}
	return out
}

// levelBanner is the soft note a below-level screen carries: which screen, the
// level whose sidebar shows it, and the level in force. It is information, not
// an error -- the page beneath renders in full.
type levelBanner struct {
	// ID names what the banner is about (a screen id, or settings:<section>):
	// the per-browser "Got it" remembers it.
	ID      string
	Label   string
	Min     level
	Current level
	// Kind and Place word the note: a "screen" missing from "your sidebar",
	// or a "Settings section" missing from "your Settings menu".
	Kind  string
	Place string
}

// MinArticle is the indefinite article for the minimum level's label ("an
// Advanced screen", "a Standard screen").
func (b levelBanner) MinArticle() string {
	if strings.ContainsRune("AEIOU", rune(b.Min.Label()[0])) {
		return "an"
	}
	return "a"
}

// levelBannerFor returns the banner for the screen id at the current level, or
// nil when the level already shows it (or the id names no screen).
func levelBannerFor(id string, current level) *levelBanner {
	sc, ok := screenByID(id)
	if !ok || current >= sc.Min {
		return nil
	}
	return &levelBanner{
		ID: sc.ID, Label: sc.Label, Min: sc.Min, Current: current,
		Kind: "screen", Place: "your sidebar",
	}
}

// withChrome fills the per-request chrome every layout-wrapped page shares: the
// effective features, the level and the screens it offers, the sidebar
// sections and badges, the host, the one-day "updated" banner and the
// automatic-update trouble banner.
func (s *Service) withChrome(ctx context.Context, pd pageData) pageData {
	pd.Features = s.effectiveFeatures(ctx)
	lvl := s.consoleLevel(ctx)
	pd.Level, pd.LevelOverridden = lvl.Level, lvl.Overridden
	pd.Screens = visibleScreens(pd.Features, pd.Level)
	pd.Nav = s.navCounts(ctx, pd.Features)
	pd.NavGroups = navGroups(pd.Screens, pd.Active, pd.Nav)
	pd.VisibleScopes = strings.Join(slices.DeleteFunc(searchScopesFor(pd.Features, pd.Level),
		func(scope string) bool { return scope == "all" }), " ")
	pd.Jumps = jumpEntries(pd.Screens, pd.Level)
	if pd.LevelBanner == nil {
		id := pd.Screen
		if id == "" {
			id = pd.Active
		}
		pd.LevelBanner = levelBannerFor(id, pd.Level)
	}
	pd.UpdateBanner, pd.UpdateAlert = s.updateChrome(time.Now())
	pd.Host = s.host
	pd.HostName = s.hostName
	return pd
}

// withFlash populates the banner fields from the ?notice= / ?error= query params
// a redirect-after-mutate handler set, so any full page renders the flash without
// per-page plumbing.
func withFlash(r *http.Request, pd pageData) pageData {
	q := r.URL.Query()
	pd.Notice = q.Get("notice")
	pd.FlashErr = q.Get("error")
	return pd
}

// returnPath is the request's own path and query minus the one-shot flash
// params, so a form that comes back here does not replay a stale message.
func returnPath(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del("notice")
	q.Del("error")
	u.RawQuery = q.Encode()
	return safeNext(u.RequestURI())
}

// funcs are the template helpers shared by every page.
var funcs = template.FuncMap{
	"ago":           ago,
	"agoPhrase":     agoPhrase,
	"ts":            ts,
	"day":           day,
	"shortID":       shortID,
	"pct":           func(n, d int) int { return percent(n, d) },
	"add":           func(a, b int) int { return a + b },
	"sub":           func(a, b int) int { return a - b },
	"hasPrefix":     strings.HasPrefix,
	"copyBtn":       copyBtn,
	"agentPill":     agentPill,
	"isoPill":       isolationPill,
	"evtTone":       evtTone,
	"evtIcon":       evtIcon,
	"evtSev":        evtSev,
	"glint":         glintKind,
	"rowSev":        rowSev,
	"deltaChip":     deltaChip,
	"bandLine":      bandLine,
	"taskTone":      taskTone,
	"planTone":      planTone,
	"outcomeTone":   outcomeTone,
	"outcomeSegs":   outcomeSegs,
	"labPath":       labPath,
	"phaseRows":     phaseRows,
	"icon":          icon,
	"compactNum":    compactNum,
	"kindLegend":    kindLegend,
	"kindBars":      kindBars,
	"kindBar":       kindBar,
	"sparkLine":     sparkLine,
	"ptlTrack":      ptlTrack,
	"areaChart":     areaChart,
	"stackedBar":    stackedBar,
	"coverageTrend": coverageTrend,
	"evtLabel":      evtLabel,
	"about":         pageAbout,
	"sameHost":      sameHost,
	"sortLabel":     sortLabel,
}

// pageAbout renders a page's one-line purpose as an (i) disclosure beside its
// h1. The compact title bar keeps the purpose off the screen until asked for,
// but a title attribute was the only way to ask -- invisible to touch, and to
// anyone who never hovers a heading. A <details> needs no script, survives the
// live morph open (navigation.js keeps a DETAILS element's open state), and is
// announced as a button by assistive tech.
func pageAbout(text string) template.HTML {
	esc := template.HTMLEscapeString(text)
	return template.HTML(`<details class="page-about"><summary aria-label="About this page" title="About this page">` +
		string(icon("info")) + `</summary><p class="page-about-pop" role="note">` + esc + `</p></details>`)
}

// normHost folds a hostname to the form two machines' reports can be compared
// in: lowercase, no trailing ".local" (macOS reports both spellings).
func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".local")
}

// sameHost reports whether a session's recorded host is this console's own
// machine. An unknown side (either empty) counts as the same, so a missing host
// never earns a tag it cannot back up.
func sameHost(a, b string) bool {
	a, b = normHost(a), normHost(b)
	return a == "" || b == "" || a == b
}

// sortLabels names every sort key a console page accepts, so a compact sort
// menu can show the active choice ("Sort: Recent") without each template
// spelling out an if-chain. An unknown key renders as itself.
var sortLabels = map[string]string{
	"recent": "Recent", "name": "Name", "title": "Title", "reach": "Reach", "coverage": "Reach",
	"utility": "Utility", "favorites": "Starred", "relevance": "Relevance", "newest": "Newest",
	"oldest": "Oldest", "project": "Project", "confidence": "Confidence",
}

func sortLabel(key string) string {
	if l, ok := sortLabels[key]; ok {
		return l
	}
	return key
}

// evtTone maps an event kind to a chip tone class (see console.css .kind.*), so
// the activity/timeline tables read at a glance: injections brand, reads/writes
// green, gardener coral, supersede/archive amber. Empty string = neutral.
func evtTone(kind string) string {
	switch {
	case strings.HasPrefix(kind, "retrieval"):
		return "brand"
	case strings.HasPrefix(kind, "gardener"):
		return "pop"
	case kind == "memory.superseded" || kind == "memory.archived":
		return "warn"
	case kind == "project.isolation.changed":
		return "accent"
	case kind == "plan.approved":
		return "ok"
	case strings.HasPrefix(kind, "plan."), kind == "subagent.captured":
		return "accent"
	case kind == "update.available":
		// The chip palette has no info tone; amber matches the health strip,
		// which marks the same fact warn: an owner action is waiting.
		return "warn"
	case kind == "update.applied":
		return "ok"
	case kind == "update.failed":
		return "warn"
	case strings.HasPrefix(kind, "memory.read"), strings.HasPrefix(kind, "memory.written"), strings.HasPrefix(kind, "note."):
		return "ok"
	default:
		return ""
	}
}

// eventLabels is the console's human vocabulary for event kinds: what a ledger
// row, a trace chip, or the live feed says happened. The raw kind is the
// machine's name for it and stays one hover away (a title attribute) and on the
// event page itself, so nothing that depends on the exact string is lost.
var eventLabels = map[string]string{
	"session.started":            "Session started",
	"session.ended":              "Session ended",
	"memory.written":             "Memory written",
	"memory.read":                "Memory read",
	"memory.superseded":          "Memory superseded",
	"memory.archived":            "Memory archived",
	"memory.moved":               "Memory moved",
	"memory.first_reuse":         "First reuse",
	"repo.moved":                 "Repo moved",
	"favorite.changed":           "Star changed",
	"note.written":               "Note written",
	"note.read":                  "Note read",
	"trial.recorded":             "Trial recorded",
	"task.transition":            "Task moved",
	"retrieval.injected":         "Context injected",
	"gardener.action":            "Gardener",
	"tool.call":                  "Tool call",
	"hook.prompt":                "Unmatched prompt",
	"recall.miss":                "Search miss",
	"hook.error":                 "Hook error",
	"agent.mishap":               "Mishap reported",
	"plan.captured":              "Plan captured",
	"plan.presented":             "Plan presented",
	"plan.approved":              "Plan approved",
	"plan.shipped":               "Plan shipped",
	"subagent.captured":          "Subagent captured",
	"project.isolation.changed":  "Isolation changed",
	"project.stage_reached":      "Project matured",
	"gamification.record_broken": "New record",
	"milestone.reached":          "Milestone",
	"settings.features_changed":  "Features changed",
	"settings.level_changed":     "Console level changed",
	"update.available":           "Update available",
	"update.applied":             "Version changed", // a downgrade too; the summary says which
	"update.started":             "Update started",
	"update.failed":              "Update failed",
}

// evtLabel names an event kind for a reader. An unlisted kind is spelled out
// from its own parts ("foo.bar_baz" -> "Foo bar baz") rather than dropped, so a
// kind added later still reads as words until it earns an entry above.
func evtLabel(kind string) string {
	if l, ok := eventLabels[kind]; ok {
		return l
	}
	words := strings.TrimSpace(strings.NewReplacer(".", " ", "_", " ").Replace(kind))
	if words == "" {
		return "Event"
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// evtIcon maps an event kind to a lucide icon name for the Interactions feed's
// type glyph, so tool calls, injections, sessions, and plans read at a glance.
// Mirrors the JS icon map in interactions.js. Unknown kinds get the activity mark.
func evtIcon(kind string) string {
	switch {
	case kind == "tool.call":
		return "terminal"
	case kind == "retrieval.injected":
		return "brain"
	case kind == "hook.prompt":
		return "search"
	case strings.HasPrefix(kind, "session."):
		return "circle"
	case kind == "subagent.captured":
		return "git-fork"
	case kind == "project.isolation.changed":
		return "lock"
	case kind == "memory.first_reuse":
		return "zap"
	case kind == "project.stage_reached":
		return "trending-up"
	case kind == "milestone.reached":
		// The milestone ledger's own glyph -- a medal, distinct from the
		// gamification records' trophy.
		return "award"
	case kind == "plan.shipped":
		// The settlement's own glyph, distinct from the plan-capture map.
		return "flag"
	case strings.HasPrefix(kind, "plan."):
		return "map"
	case kind == "gamification.record_broken":
		return "trophy"
	case kind == "agent.mishap", kind == "hook.error", kind == "update.failed":
		return "triangle-alert"
	case kind == "memory.written", kind == "note.written":
		return "pencil"
	case kind == "memory.read", kind == "note.read":
		return "eye"
	case kind == "memory.archived":
		return "archive"
	case kind == "memory.superseded", strings.HasPrefix(kind, "update."):
		return "refresh-cw"
	case kind == "memory.moved", kind == "repo.moved":
		return "folder-open"
	case kind == "favorite.changed":
		return "star"
	case kind == "trial.recorded":
		return "test-tube"
	case kind == "task.transition":
		return "list-checks"
	case kind == "gardener.action":
		return "sprout"
	case kind == "recall.miss":
		return "search"
	default:
		return "activity"
	}
}

// taskTone maps a task status to a badge tone class (see console.css .badge.*),
// so status chips read at a glance: done green, in-progress brand, open amber,
// dropped neutral.
func taskTone(status string) string {
	switch status {
	case "done":
		return "ok"
	case "in_progress":
		return "accent"
	case "open":
		return "warn"
	default: // dropped
		return ""
	}
}

// parseTemplates parses the layout + every page into its own template set (an
// entity page also parses its peek_<name>.html companion so its content block
// can compose the shared "detail-body"), plus a standalone body fragment per
// peekNames entry.
func parseTemplates() (pages, fragments map[string]*template.Template, err error) {
	pages = make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t := template.New("layout").Funcs(funcs)
		files := []string{"templates/layout.html", "templates/" + name + ".html"}
		if name == "context" || name == "projectdetail" {
			files = append(files, "templates/context_shared.html")
		}
		if entityPeekPages[name] {
			files = append(files, "templates/peek_"+name+".html")
		}
		t, perr := t.ParseFS(templateFS, files...)
		if perr != nil {
			return nil, nil, fmt.Errorf("console: parse %s: %w", name, perr)
		}
		pages[name] = t
	}
	// Standalone fragments: just the peek body, executed into the detail pane.
	fragments = make(map[string]*template.Template, len(peekNames))
	for _, name := range peekNames {
		t := template.New("peek_" + name).Funcs(funcs)
		t, perr := t.ParseFS(templateFS, "templates/peek_"+name+".html")
		if perr != nil {
			return nil, nil, fmt.Errorf("console: parse peek fragment %s: %w", name, perr)
		}
		fragments[name] = t
	}
	return pages, fragments, nil
}

// renderFragment writes an entity's peek body -- the "peek-body" block of
// templates/peek_<name>.html -- as a standalone HTML fragment, for a detail
// pane fetch (?peek=1). data is the entity payload (the fragment's dot).
func (s *Service) renderFragment(w http.ResponseWriter, r *http.Request, name string, data any) {
	tmpl, ok := s.fragments[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("console: no peek fragment %q", name))
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "peek-body", data); err != nil {
		s.serverError(w, r, fmt.Errorf("console: render peek %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// renderReader writes a library entity's reader fragment -- the
// "<entity>-reader" block of its library page template -- as a standalone HTML
// fragment, for the in-place reader swap (?reader=1). page names the library
// page ("notes", "memories", "tasks", "plans"); block its reader template.
func (s *Service) renderReader(w http.ResponseWriter, r *http.Request, page, block string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		s.serverError(w, r, fmt.Errorf("console: no such page %q", page))
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, block, data); err != nil {
		s.serverError(w, r, fmt.Errorf("console: render reader %s: %w", block, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// renderDetail serves an entity detail three ways: JSON for the CLI (wantsJSON),
// an HTML fragment for a detail-pane fetch (?peek=1), or -- by default -- a full
// layout-wrapped page (a shareable, no-JS fallback URL). name is the peek entity
// ("event"); pd.Data is its payload. The library entities (memory, note, task,
// plan) branch by hand in their handlers instead, because their default page is
// the shared library screen rather than a bespoke one.
func (s *Service) renderDetail(w http.ResponseWriter, r *http.Request, name string, pd pageData) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, pd.Data)
		return
	}
	if r.URL.Query().Get("peek") == "1" {
		s.renderFragment(w, r, name, pd.Data)
		return
	}
	// Default: the entity's bespoke full page, whose content block composes the
	// same "detail-body" the fragment renders. Every full-page caller has one
	// (project routes non-peek requests to its workspace before reaching here);
	// render 500s on an unregistered page.
	s.render(w, r, name, pd)
}

// render writes a page as HTML, or -- when the caller asked for JSON (the CLI) --
// encodes just the page's Data payload. Nav counts are filled in best-effort.
func (s *Service) render(w http.ResponseWriter, r *http.Request, page string, pd pageData) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, pd.Data)
		return
	}
	pd = s.withChrome(r.Context(), pd)
	pd = withFlash(r, pd)
	pd.ReturnPath = returnPath(r)
	tmpl, ok := s.pages[page]
	if !ok {
		s.serverError(w, r, fmt.Errorf("console: no such page %q", page))
		return
	}
	layout := "layout"
	if patchRequest(r) {
		layout = "patch"
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, layout, pd); err != nil {
		s.serverError(w, r, fmt.Errorf("console: render %s: %w", page, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if layout == "patch" {
		// A patch is not a page. It must never be stored: a back or forward
		// navigation may reuse a stale cache entry for the same URL, and would
		// render the patch instead of the page.
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = buf.WriteTo(w)
}

// The console's own fetches of a page it is showing -- the live refresh in
// layout.html and the in-place loads in static/navigation.js -- carry one of
// these headers. Both morph only the view and the sidebar's nav and account
// copy into the open document, so they are answered with the "patch" layout:
// just those parts, without the head, the shell, the dialogs, or the scripts.
const (
	liveHeader    = "X-Seam-Live"
	partialHeader = "X-Seam-Partial"
)

// patchRequest reports whether r is one of the console's own fetches of a page
// it is showing.
func patchRequest(r *http.Request) bool {
	return r.Header.Get(liveHeader) == "1" || r.Header.Get(partialHeader) == "1"
}

// wantsJSON reports whether the request prefers a JSON response (the seam CLI
// sets ?format=json; a browser never does).
func wantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("format") == "json" {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// writeJSON renders v as the entire response. It encodes into a buffer first,
// like renderPage: encoding straight into w would commit the status line and
// then discover a failure mid-body, leaving the client a truncated document that
// is neither valid JSON nor an error it can detect. Buffering means the status
// is only chosen once the body is known to exist.
func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		http.Error(w, "internal error: encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

// errorData is the payload for the styled in-console error page.
type errorData struct {
	Status  int
	Heading string
	Message string
	// Links are the places a lost reader most often means, drawn from the
	// screens this level offers; SearchNoun names what the search box covers.
	Links      []screen
	SearchNoun string
}

// errorDestinations are the screens the styled error page offers, in order,
// by id: the places a lost reader most often means.
var errorDestinations = []string{"overview", "memories", "sessions", "projects", "notes"}

// errorLinks picks up to four destinations the level offers, so a Basic owner
// is never sent to a screen their sidebar does not have.
func errorLinks(visible []screen) []screen {
	var out []screen
	for _, id := range errorDestinations {
		for _, sc := range visible {
			if sc.ID == id && sc.NavRow {
				out = append(out, sc)
			}
		}
		if len(out) == 4 {
			break
		}
	}
	return out
}

// renderErrorPage renders a full, layout-wrapped error page (sidebar + a way
// back) at the given HTTP status, so a bad/stale URL keeps the observer inside
// the console instead of dropping them on a bare "404 page not found". Only for
// browsers; JSON/CLI callers are answered before reaching here. Falls back to
// plaintext if the template is somehow unavailable.
func (s *Service) renderErrorPage(w http.ResponseWriter, r *http.Request, status int, heading, msg string) {
	// A ?peek=1 fetch is injected verbatim into the in-page detail pane; a
	// layout-wrapped page there nests the whole console inside the pane. Answer
	// fragment fetches (e.g. a stale row whose entity is gone) with a
	// fragment-shaped error instead.
	if r.URL.Query().Get("peek") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w,
			`<article class="peek-entity"><div class="peek-head"><span class="badge danger">%d</span></div><h2 class="peek-title">%s</h2><p class="peek-desc">%s</p><p class="peek-note">The row may be stale &mdash; reload the list.</p></article>`,
			status, template.HTMLEscapeString(heading), template.HTMLEscapeString(msg))
		return
	}
	tmpl, ok := s.pages["error"]
	if !ok {
		http.Error(w, msg, status)
		return
	}
	pd := s.withChrome(r.Context(), pageData{Title: heading})
	pd.Data = errorData{
		Status: status, Heading: heading, Message: msg,
		Links:      errorLinks(pd.Screens),
		SearchNoun: searchScopeNoun(searchScopesFor(pd.Features, pd.Level)),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", pd); err != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// badRequest reports a 400 in the caller's preferred format, for a strictly
// validated query param (an unknown enum value). The message names the bad
// param and lists the valid values so an agent driving the console by URL sees
// the fix rather than a silent default; a browser gets the styled error page.
func (s *Service) badRequest(w http.ResponseWriter, r *http.Request, msg string) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	s.renderErrorPage(w, r, http.StatusBadRequest, "Bad request", msg)
}

// notFound reports a 404 in the caller's preferred format, naming the missing
// entity so an agent driving the console by URL sees exactly what was not found;
// a browser gets the styled error page with a way back.
func (s *Service) notFound(w http.ResponseWriter, r *http.Request, msg string) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": msg})
		return
	}
	s.renderErrorPage(w, r, http.StatusNotFound, "Not found", msg)
}

// serverError logs and reports a 500 in the caller's preferred format. The
// browser page stays generic (the detail is in the log, not the response). A
// failure that is only the browser giving up logs at debug (clientGoneHandler).
func (s *Service) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("console: request failed", "path", r.URL.Path, "error", err)
	if wantsJSON(r) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.renderErrorPage(w, r, http.StatusInternalServerError, "Something went wrong",
		"An internal error occurred and has been logged.")
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

// ago renders a compact relative age ("3d", "5h", "just now"). It accepts a
// time.Time or *time.Time (nil/zero -> "-") so templates can pass nullable
// timestamps directly.
func ago(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "-"
		}
		t = *x
	default:
		return "-"
	}
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/(24*365)))
	}
}

// ts formats a timestamp for a title= tooltip: a clean minute-precision UTC
// stamp ("2026-07-14 07:29 UTC"), instead of Go's default time.Time String()
// with microseconds and a "+0000 UTC" suffix. Accepts time.Time or *time.Time;
// a nil or zero time renders "" so the attribute is simply empty.
func ts(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return ""
		}
		t = *x
	default:
		return ""
	}
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

// day formats a timestamp as its calendar date ("2026-08-07"), for lines that
// witness when something happened rather than how long ago. Accepts time.Time
// or *time.Time like ts; a nil or zero time renders "".
func day(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return ""
		}
		t = *x
	default:
		return ""
	}
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

// copyBtn renders a small quiet copy-to-clipboard button carrying the FULL
// value in a data-copy attribute; the global handler in layout.html copies it
// on click. Pass the untruncated id/name/slug/path -- not the shortID display
// text. The value is HTML-attribute-escaped; an empty value renders nothing so
// callers can drop it inline next to conditionally-shown fields. It carries
// both the copy and check glyphs; CSS toggles them via the .copied class.
func copyBtn(value string) template.HTML {
	if value == "" {
		return ""
	}
	esc := template.HTMLEscapeString(value)
	return template.HTML(`<button type="button" class="copy-btn" data-copy="` + esc +
		`" aria-label="Copy" title="Copy">` + string(icon("copy")) + string(icon("check")) + `</button>`)
}

// shortID returns the last 8 chars of a ULID for compact display. The last 8
// distinguish ULIDs better than the first 8 (ULID prefixes are the timestamp,
// so recent ids share them); the Interactions client does the same (id.slice(-8))
// so a given id renders identically server-side and client-side.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[len(id)-8:]
}

// compactNum renders a count for dense stat rows: plain below 1000, then one
// decimal with a k/M suffix ("12.4k", "1.2M"), dropping a trailing ".0".
func compactNum(n int) string {
	format := func(v float64, suffix string) string {
		s := strconv.FormatFloat(v, 'f', 1, 64)
		s = strings.TrimSuffix(s, ".0")
		return s + suffix
	}
	switch {
	case n >= 1_000_000:
		return format(float64(n)/1_000_000, "M")
	case n >= 1000:
		return format(float64(n)/1000, "k")
	default:
		return strconv.Itoa(n)
	}
}

// percent computes n/d as a rounded whole percentage (0 when d == 0).
func percent(n, d int) int {
	if d == 0 {
		return 0
	}
	return int(float64(n)/float64(d)*100 + 0.5)
}

// durUntil renders the remaining time until t as a compact "22m left" / "1h left".
// It is the countdown half of ago(): both the project workspace and the sessions
// screen render task leases with it.
func durUntil(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "expired"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm left", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh left", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd left", int(d.Hours()/24))
	}
}
