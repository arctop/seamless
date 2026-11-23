package console

// Settings > Knowledge engine: recall (the embedder and the stored vectors),
// ranking by use (the utility activation per project) and storage. The
// section's markup is templates/settings_engine.html.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// utilityProjectRow is one project's utility-activation status for the
// Settings table: where its demand history stands against the readiness
// thresholds, whether the gardener latch has tripped, and any owner force.
type utilityProjectRow struct {
	Project        string     `json:"project"` // "" = global scope
	Status         string     `json:"status"`  // active|armed|building|forced-off
	Forced         string     `json:"forced,omitempty"`
	ReadyAt        *time.Time `json:"readyAt,omitempty"`
	RecentEvents   int        `json:"recentEvents"`
	RecentMemories int        `json:"recentMemories"`
	AgeDays        int        `json:"ageDays"` // days since the first demand event
	EventsOK       bool       `json:"eventsOK"`
	MemoriesOK     bool       `json:"memoriesOK"`
	AgeOK          bool       `json:"ageOK"`
	// Remaining spells out what still separates this scope from arming
	// ("needs 7 more events, 3d more history") so the table answers "when
	// does auto kick in" without mental math against the threshold prose.
	Remaining string `json:"remaining,omitempty"`
}

// Label names the row's scope: its project, or the global scope's name.
func (r utilityProjectRow) Label() string {
	if r.Project == "" {
		return "Global context"
	}
	return r.Project
}

// Anchor is the row's element id, the one a force's answer returns to. It is
// made from the slug (a slug can hold characters an id should not), so it
// stays the row's id however the rows reorder.
func (r utilityProjectRow) Anchor() string {
	if r.Project == "" {
		return "ur-global"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(r.Project))
	return fmt.Sprintf("ur-%08x", h.Sum32())
}

// Choice is the row's Auto / On / Off segment: the owner's force, or auto.
func (r utilityProjectRow) Choice() string {
	if r.Forced == "" {
		return "auto"
	}
	return r.Forced
}

// Counts says the row's demand in a line: "102 uses · 61 memories · 40 days"
// (uses and memories over the last 30 days, days since the first use), or
// "no use yet".
func (r utilityProjectRow) Counts() string {
	if r.RecentEvents == 0 && r.AgeDays == 0 {
		return "no use yet"
	}
	return plural(r.RecentEvents, "use", "uses") + " · " +
		plural(r.RecentMemories, "memory", "memories") + " · " + plural(r.AgeDays, "day", "days")
}

// Why is the row's sub line before any time note: the force, or what the
// gardener does next, then the counts; a project still building says what it
// still needs.
func (r utilityProjectRow) Why() string {
	switch {
	case r.Forced != "":
		return "forced " + r.Forced + " · " + r.Counts()
	case r.Status == "armed":
		return "ready: ranks by use from the gardener’s next pass · " + r.Counts()
	case r.Status == "building" && r.Remaining != "":
		return r.Remaining
	}
	return r.Counts()
}

// utilityBar is one bar of a row's meter: how far one readiness gate is met.
type utilityBar struct {
	Pct int
	Met bool
}

// Meter is the row's three bars, uses, memories used and days of history,
// each against its readiness threshold (full at the threshold).
func (r utilityProjectRow) Meter() []utilityBar {
	bar := func(v, of int, met bool) utilityBar { return utilityBar{Pct: min(100, percent(v, of)), Met: met} }
	return []utilityBar{
		bar(r.RecentEvents, store.UtilityReadyMinEvents, r.EventsOK),
		bar(r.RecentMemories, store.UtilityReadyMinMemories, r.MemoriesOK),
		bar(r.AgeDays, store.UtilityReadyMinAgeDays, r.AgeOK),
	}
}

// MeterLabel reads the meter out: each count against its threshold.
func (r utilityProjectRow) MeterLabel() string {
	return fmt.Sprintf("%d of %d uses, %d of %d memories used, %d of %d days of history",
		r.RecentEvents, store.UtilityReadyMinEvents, r.RecentMemories, store.UtilityReadyMinMemories,
		r.AgeDays, store.UtilityReadyMinAgeDays)
}

// utilityRanking is Knowledge engine's Ranking card: the rows it lists (the
// projects that rank by use, are ready to, or are forced either way) and the
// rows it folds (still building their history), with the counts its chips
// show.
type utilityRanking struct {
	Rows     []utilityRankingRow
	Building []utilityRankingRow
	// Ranking counts the rows whose briefings rank by use now (latched or
	// forced on), Forced the rows the owner forced either way, and Ready the
	// rows that rank from the gardener's next pass.
	Ranking, Forced, Ready int
	// Mode is briefing.utility_mode. Anything but auto decides for every
	// project, and the per-project states wait until it is auto again.
	Mode string
}

// utilityRankingRow is a row as the Ranking card draws it.
type utilityRankingRow struct {
	utilityProjectRow
	// Momentum words a latched row's time note as the Momentum feature does
	// ("unlocked <day>"); without it the note is plain ("since <ago>").
	Momentum bool
}

// Hint is the card's hint: when a project starts ranking by use, with the
// readiness thresholds the gardener latches on.
func (u utilityRanking) Hint() string {
	return fmt.Sprintf("On Per project, a project's briefing starts ranking memories by use once agents have used "+
		"its memories %d times, %d different memories, within 30 days, with %d days of history. The gardener turns "+
		"it on at its next pass; On and Off here override that, until set back to Auto. Uses are reads and recall "+
		"hits that agents asked for, never what a briefing handed them.",
		store.UtilityReadyMinEvents, store.UtilityReadyMinMemories, store.UtilityReadyMinAgeDays)
}

// BuildingNoun names what the fold holds: projects, or scopes when the
// global scope is among them.
func (u utilityRanking) BuildingNoun() string {
	for _, r := range u.Building {
		if r.Project == "" {
			return "scopes"
		}
	}
	return "projects"
}

// utilityRankingOf groups the activation rows for the Ranking card, keeping
// their order. A forced row stays listed until it is set back to auto. The
// global scope is folded only when it has some history: with none it can
// neither rank nor need anything, and every project's briefing already
// carries its memories.
func utilityRankingOf(rows []utilityProjectRow, mode string, momentum bool) utilityRanking {
	if mode == "" {
		mode = "auto"
	}
	u := utilityRanking{Mode: mode}
	for _, row := range rows {
		r := utilityRankingRow{utilityProjectRow: row, Momentum: momentum}
		if r.Forced != "" {
			u.Forced++
		}
		switch r.Status {
		case "active":
			u.Ranking++
		case "armed":
			u.Ready++
		}
		if r.Forced != "" || r.Status == "active" || r.Status == "armed" {
			u.Rows = append(u.Rows, r)
			continue
		}
		if r.Project == "" && r.RecentEvents == 0 && r.AgeDays == 0 {
			continue
		}
		u.Building = append(u.Building, r)
	}
	return u
}

// EmbeddingRuntime describes the embedder the daemon resolved at serve start.
// Enabled means vectors are being written and searched this process. When
// disabled, Reason says why in the owner's terms: their off switch, a missing
// credential, or a config mistake -- three states that call for three different
// next actions.
type EmbeddingRuntime struct {
	Enabled bool `json:"enabled"`
	// Provider is the configured llm.provider, shown even when disabled.
	Provider string `json:"provider"`
	// Model is the active embedding model when enabled, else the configured one.
	Model string `json:"model"`
	// Reason is the human-readable cause when disabled; empty when enabled.
	Reason string `json:"reason,omitempty"`
	// Misconfigured marks Reason as a local config error (llm.ErrConfig class)
	// rather than a deliberate opt-out, so the panel can escalate its tone.
	Misconfigured bool `json:"misconfigured,omitempty"`
	// OverriddenOff means this process started with the console off switch set.
	OverriddenOff bool `json:"overriddenOff,omitempty"`
}

// embeddingModelRow is one (model, dims) group of stored vectors, flagged
// stale when it is not the model the running embedder writes -- those vectors
// are invisible to semantic search until re-embedded.
type embeddingModelRow struct {
	Model    string    `json:"model"`
	Dims     int       `json:"dims"`
	Count    int       `json:"count"`
	Memories int       `json:"memories"`
	Notes    int       `json:"notes"`
	Updated  time.Time `json:"updated"`
	Stale    bool      `json:"stale"`
}

// embeddingsPanel is the Settings page's semantic-index section: the runtime
// embedder state, the stored corpus grouped by model, the owner's override as
// currently stored, and the re-embed pass state.
type embeddingsPanel struct {
	EmbeddingRuntime
	// ModeOff is the stored override at render time; RestartNeeded is true when
	// it disagrees with what this process resolved at startup.
	ModeOff       bool                  `json:"modeOff"`
	RestartNeeded bool                  `json:"restartNeeded"`
	Total         int                   `json:"total"`
	Missing       int                   `json:"missing"`
	Stale         int                   `json:"stale"`
	Models        []embeddingModelRow   `json:"models"`
	Reembed       files.ReembedProgress `json:"reembed"`
}

// engineRecall is Knowledge engine's Semantic recall row: its state chip, the
// reason in its sub line, and what to do about it in its hint.
type engineRecall struct {
	Word, Tone string
	// Sub is the sub line; Detail, when set, the provider's own error after
	// it, drawn as code. Only a config mistake shows one: off for want of a
	// provider, the daemon's error stays in its log, and the hint says how to
	// add one in Your setup's words (embedOffFix).
	Sub, Detail string
	Hint        string
}

// Recall builds the Semantic recall row from the panel.
func (p embeddingsPanel) Recall() engineRecall {
	switch {
	case p.Enabled:
		return engineRecall{Word: "semantic", Tone: "ok", Sub: "By meaning and by keywords, through " + p.Provider + ".",
			Hint: "Recall and search find knowledge by meaning, through embeddings, and by keywords. Switched off, they match keywords only, from the next restart."}
	case p.OverriddenOff:
		return engineRecall{Word: "keywords only", Tone: "warn", Sub: "Switched off from this console.",
			Hint: "Recall and search match keywords only. Enable it to match by meaning again: it applies at the next restart."}
	case p.Misconfigured:
		return engineRecall{Word: "misconfigured", Tone: "danger", Sub: "The LLM configuration needs a fix in the config file:", Detail: p.Reason,
			Hint: "Recall and search match keywords only until the llm settings in the config file are fixed and the daemon restarts."}
	}
	return engineRecall{Word: "keywords only", Tone: "warn", Sub: "No embedding provider is set up.", Hint: embedOffFix}
}

// VectorsLine counts the stored vectors in one line: "2,208 vectors · 3
// items not embedded · 0 on an old model".
func (p embeddingsPanel) VectorsLine() string {
	words := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return groupDigits(n) + " " + many
	}
	return words(p.Total, "vector", "vectors") + " · " + words(p.Missing, "item", "items") + " not embedded · " +
		groupDigits(p.Stale) + " on an old model"
}

// ModelsFolded reports whether the per-model table is worth its fold: more
// than one model, or vectors the running embedder no longer writes. One
// current model is already the row's own label.
func (p embeddingsPanel) ModelsFolded() bool { return len(p.Models) > 1 || p.Stale > 0 }

// StorageHint is the Storage card's hint: where the values come from, what
// the database holds, and the budgets' meaning, with the tool-event cap the
// chips leave out.
func (d settingsData) StorageHint() string {
	db := "the database"
	if d.Database.Path != "" {
		db += " (" + d.Database.Path + ")"
	}
	tools := "Tool events are kept in full."
	if n := d.Budgets.ToolEventMaxChars; n > 0 {
		tools = "A tool event keeps " + groupDigits(n) + " characters of each field."
	}
	return "Set in the config file or the environment, and read at start. Durable knowledge stays in markdown files " +
		"in the data folder; " + db + ", which holds sessions, tasks, events and the vector index, can be rebuilt " +
		"from them. A briefing packs into its token budget, and a recall answer into its own. " + tools
}

// databasePanel is the Settings page's SQLite block: where the database lives,
// how big it is on disk (main file plus WAL), and its schema version.
type databasePanel struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	WalBytes      int64  `json:"walBytes"`
	SizeHuman     string `json:"sizeHuman"` // main + WAL, formatted
	SchemaVersion int    `json:"schemaVersion"`
}

// utilityActivationRows joins the activation state with each scope's demand
// progress so the Settings table can say WHY a project is or is not ranked by
// utility yet. Every registered project appears, plus the global scope and any
// slug that exists only in the demand or activation maps.
func (s *Service) utilityActivationRows(ctx context.Context, projects []core.Project) ([]utilityProjectRow, error) {
	now := time.Now().UTC()
	activation, err := store.GetUtilityActivation(ctx, s.db(ctx))
	if err != nil {
		return nil, err
	}
	demand, err := store.UtilityDemandByProject(ctx, s.db(ctx), now, store.UtilityReadyWindow)
	if err != nil {
		return nil, err
	}

	slugs := map[string]struct{}{"": {}}
	for _, p := range projects {
		if !p.Retired() {
			slugs[p.Slug] = struct{}{}
		}
	}
	for slug := range demand {
		slugs[slug] = struct{}{}
	}
	for slug := range activation.Projects {
		slugs[slug] = struct{}{}
	}

	rows := make([]utilityProjectRow, 0, len(slugs))
	for slug := range slugs {
		st := activation.Projects[slug]
		d := demand[slug]
		row := utilityProjectRow{
			Project: slug, Forced: st.Forced, ReadyAt: st.ReadyAt,
			RecentEvents: d.RecentEvents, RecentMemories: d.RecentMemories,
		}
		if !d.Earliest.IsZero() {
			row.AgeDays = int(now.Sub(d.Earliest).Hours() / 24)
		}
		row.EventsOK = d.RecentEvents >= store.UtilityReadyMinEvents
		row.MemoriesOK = d.RecentMemories >= store.UtilityReadyMinMemories
		row.AgeOK = !d.Earliest.IsZero() && now.Sub(d.Earliest) >= store.UtilityReadyMinAgeDays*24*time.Hour
		switch {
		case st.Forced == "off":
			row.Status = "forced-off"
		case st.Forced == "on" || st.ReadyAt != nil:
			row.Status = "active"
		case d.Ready(now):
			row.Status = "armed" // latches on the next gardener pass
			row.Remaining = "arms on the next gardener pass"
		default:
			row.Status = "building"
			row.Remaining = utilityRemaining(row, d)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if (rows[i].Status == "active") != (rows[j].Status == "active") {
			return rows[i].Status == "active"
		}
		if rows[i].RecentEvents != rows[j].RecentEvents {
			return rows[i].RecentEvents > rows[j].RecentEvents
		}
		return rows[i].Project < rows[j].Project
	})
	return rows, nil
}

// utilityRemaining phrases the unmet readiness gates for a building scope. A
// scope with no demand at all gets nothing -- the table already says "no
// demand yet" and enumerating every gate there would just restate the header.
func utilityRemaining(row utilityProjectRow, d store.UtilityProjectDemand) string {
	if d.Earliest.IsZero() {
		return ""
	}
	var needs []string
	if !row.EventsOK {
		needs = append(needs, fmt.Sprintf("%d more events", store.UtilityReadyMinEvents-d.RecentEvents))
	}
	if !row.MemoriesOK {
		needs = append(needs, fmt.Sprintf("%d more memories", store.UtilityReadyMinMemories-d.RecentMemories))
	}
	if !row.AgeOK {
		needs = append(needs, fmt.Sprintf("%dd more history", store.UtilityReadyMinAgeDays-row.AgeDays))
	}
	if len(needs) == 0 {
		return ""
	}
	return "needs " + strings.Join(needs, ", ")
}

// settingsUtilityForce sets or clears the owner's per-project force: "on" and
// "off" win over the gardener latch; "auto" clears the force and defers to it.
func (s *Service) settingsUtilityForce(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PostFormValue("project"))
	if project != "" {
		if err := validate.Name(project); err != nil {
			settingsUtilityFlash(w, r, "project: "+err.Error())
			return
		}
	}
	force := r.PostFormValue("force")
	switch force {
	case "on", "off", "auto":
	default:
		settingsUtilityFlash(w, r, fmt.Sprintf("force invalid %q: valid values are on, off, auto", force))
		return
	}

	ctx := r.Context()
	activation, err := store.GetUtilityActivation(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st := activation.Projects[project]
	if force == "auto" {
		st.Forced = ""
	} else {
		st.Forced = force
	}
	activation.Projects[project] = st
	if err := store.SetUtilityActivation(ctx, s.writeDB(), activation); err != nil {
		s.serverError(w, r, err)
		return
	}
	scope := project
	if scope == "" {
		scope = "the global scope"
	}
	// Back to the row the change was made on: it is there whether the change
	// keeps it listed or folds it with the projects still building.
	settingsBack(w, r, "engine", "notice", fmt.Sprintf("Utility ranking for %s set to %s.", scope, force),
		utilityProjectRow{Project: project}.Anchor())
}

// embeddingsPanel assembles the semantic-index section: the startup runtime
// state from Config, the stored override and vector stats read live, and the
// re-embed pass state from the files manager (when wired).
func (s *Service) embeddingsPanel(ctx context.Context) (embeddingsPanel, error) {
	mode, err := store.EmbedderMode(ctx, s.db(ctx))
	if err != nil {
		return embeddingsPanel{}, err
	}
	stats, err := store.GetEmbeddingStats(ctx, s.db(ctx))
	if err != nil {
		return embeddingsPanel{}, err
	}

	p := embeddingsPanel{
		EmbeddingRuntime: s.cfg.Embedding,
		ModeOff:          mode == store.EmbedderModeOff,
		Total:            stats.Total,
		Missing:          stats.Missing,
	}
	// The override applies at serve start, so the page owes the owner a restart
	// note exactly when the stored switch disagrees with what this process did:
	// running but switched off, or held off by an override that is now cleared.
	p.RestartNeeded = (p.Enabled && p.ModeOff) || (p.OverriddenOff && !p.ModeOff)
	for _, m := range stats.Models {
		row := embeddingModelRow{
			Model: m.Model, Dims: m.Dims, Count: m.Count,
			Memories: m.Memories, Notes: m.Notes, Updated: m.Updated,
			Stale: p.Enabled && m.Model != p.Model,
		}
		if row.Stale {
			p.Stale += m.Count
		}
		p.Models = append(p.Models, row)
	}
	if s.cfg.Files != nil {
		p.Reembed = s.cfg.Files.ReembedStatus()
	}
	return p, nil
}

// databasePanel reads the SQLite block: schema version from the live
// connection, sizes from disk. Best-effort on the filesystem side -- a missing
// WAL (checkpointed) or an unreadable stat renders as zero, not an error.
func (s *Service) databasePanel() databasePanel {
	p := databasePanel{Path: s.cfg.DBPath}
	if v, err := store.SchemaVersion(s.cfg.DB); err == nil {
		p.SchemaVersion = v
	}
	if p.Path == "" {
		return p
	}
	if info, err := os.Stat(p.Path); err == nil {
		p.SizeBytes = info.Size()
	}
	if info, err := os.Stat(p.Path + "-wal"); err == nil {
		p.WalBytes = info.Size()
	}
	p.SizeHuman = humanBytes(p.SizeBytes + p.WalBytes)
	return p
}

// humanBytes formats a byte count for display (binary units, one decimal).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// settingsEmbeddingsMode stores or clears the embedder off switch. The switch
// is read once at serve start, so the flash says when a restart is what makes
// the change real.
func (s *Service) settingsEmbeddingsMode(w http.ResponseWriter, r *http.Request) {
	mode := r.PostFormValue("mode")
	switch mode {
	case store.EmbedderModeAuto, store.EmbedderModeOff:
	default:
		settingsEmbeddingsFlash(w, r, fmt.Sprintf("mode invalid %q: valid values are %s, %s",
			mode, store.EmbedderModeAuto, store.EmbedderModeOff))
		return
	}
	if err := store.SetEmbedderMode(r.Context(), s.writeDB(), mode); err != nil {
		s.serverError(w, r, err)
		return
	}
	switch {
	case mode == store.EmbedderModeOff && s.cfg.Embedding.Enabled:
		settingsEmbeddingsNotice(w, r, "Embeddings switched off. Restart the daemon to apply.")
	case mode == store.EmbedderModeOff:
		settingsEmbeddingsNotice(w, r, "Embeddings switched off.")
	case s.cfg.Embedding.OverriddenOff:
		settingsEmbeddingsNotice(w, r, "Embeddings switch cleared. Restart the daemon to re-enable them.")
	default:
		settingsEmbeddingsNotice(w, r, "Embeddings switch cleared: back to the configured provider.")
	}
}

// settingsEmbeddingsReembed starts the background re-embed pass. The redirect
// returns immediately; progress renders on the page and the pass survives the
// request (the files manager owns its lifecycle).
func (s *Service) settingsEmbeddingsReembed(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Files == nil {
		settingsEmbeddingsFlash(w, r, "the files subsystem is not available")
		return
	}
	total, err := s.cfg.Files.StartReembed(r.Context())
	switch {
	case errors.Is(err, files.ErrNoEmbedder):
		settingsEmbeddingsFlash(w, r, "embeddings are disabled; configure a provider (and restart) before re-embedding")
		return
	case errors.Is(err, files.ErrReembedRunning):
		settingsEmbeddingsFlash(w, r, "a re-embed pass is already running")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	settingsEmbeddingsNotice(w, r, fmt.Sprintf("Re-embedding %d items in the background with %s.",
		total, s.cfg.Embedding.Model))
}

func settingsUtilityFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "error", msg, "utility-activation")
}

func settingsEmbeddingsFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "error", msg, "semantic-index")
}

func settingsEmbeddingsNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "notice", msg, "semantic-index")
}
