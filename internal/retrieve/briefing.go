package retrieve

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/agentguide"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// BriefingInput carries the SessionStart hook fields the briefing depends on.
type BriefingInput struct {
	CWD string // agent working directory; resolved to a project slug
	// Host is the machine that CWD belongs to. The same absolute path maps to
	// different projects on different machines once a daemon is shared, so the
	// cwd is only half of the question. "" is the daemon's own map (the caller
	// did not say which machine it is on).
	Host      string
	Source    string // startup|resume|clear|compact
	AgentType string // non-empty => subagent => constraints-only briefing
	// Prompt is the child's spawn prompt, resolved best-effort at SubagentStart
	// (empty on the main-session path and whenever resolution fails). The
	// subagent path matches it against the project's memories and renders the
	// hits as the briefing's RELEVANT section (subagentRelevant); the
	// main-session path never reads it, so main briefings stay byte-identical
	// whether it is set or empty.
	Prompt string
}

// Briefing assembles the SessionStart briefing for an agent: constraints (always
// included), a newest-first memory index, and recent sibling findings, budgeted
// by estimated tokens and wrapped in <seam-briefing> tags. It returns "" when
// there is nothing worth injecting (unmapped cwd with no global memories), which
// the hook forwards as an empty additionalContext. The second return value is the
// ids of the memories actually rendered (after budget dropping), so the caller
// can record them as a retrieval.injected event and feed the read-after-inject
// funnel -- the same telemetry the recall tool emits.
func (s *Service) Briefing(ctx context.Context, in BriefingInput) (string, []string, error) {
	cfg := s.effectiveBriefing(ctx)
	project, err := s.cwdProject(ctx, in.Host, in.CWD)
	if err != nil {
		return "", nil, err
	}
	return s.briefingFor(ctx, project, in, cfg)
}

// cwdProject resolves the project a cwd places an agent in, under the caller's
// peer_auth reach: a configured OS account working outside its own projects is
// placed in its own project, and the owner in a repository sealed to another
// account gets the refusal (access.ErrNotReachable) rather than that project.
func (s *Service) cwdProject(ctx context.Context, host, cwd string) (string, error) {
	project, err := store.ResolveProjectForCWD(ctx, s.db, host, cwd)
	if err != nil {
		return "", err
	}
	return access.SessionScope(ctx, project)
}

// ProjectBriefing is Briefing for a project the caller already resolved, so the
// input's cwd is not consulted. session_start binds a project by routes a cwd
// does not capture -- a project argument, a resumed session, the agent's own
// session -- and the briefing it returns must describe the project the session
// actually got, not whatever the cwd (often none) resolves to.
func (s *Service) ProjectBriefing(ctx context.Context, project string, in BriefingInput) (string, []string, error) {
	return s.briefingFor(ctx, project, in, s.effectiveBriefing(ctx))
}

// PreviewBriefing assembles the briefing a new session in project would start
// with under cfg rather than the effective knobs: the console's Settings
// preview of values the owner may not have saved. It is the session-start
// assembly exactly (Source "startup", not a subagent), and a pure read, like
// Briefing itself. What differs is the caller's side: a hook records the ids
// Briefing returns as an injection, and a preview must record nothing, because
// no agent received it and counting it would feed the utility signal exposure
// that never happened (constraint closed-loop-utility-signal-contract). So it
// returns no ids to record.
func (s *Service) PreviewBriefing(ctx context.Context, project string, cfg config.Briefing) (string, error) {
	text, _, err := s.briefingFor(ctx, project, BriefingInput{Source: "startup"}, cfg)
	return text, err
}

// BriefingBudget reports the token budget an assembly under cfg packs its
// droppable rows into and the hard cap it truncates at, with the assembler's
// own fallbacks, so a preview can state both without restating them.
func (s *Service) BriefingBudget(cfg config.Briefing) (budget, hardCap int) {
	return s.packingBudget(), s.briefingHardCap(cfg)
}

// briefingFor is Briefing past project resolution, under explicit knobs.
func (s *Service) briefingFor(ctx context.Context, project string, in BriefingInput, cfg config.Briefing) (string, []string, error) {
	// peer_auth: the one funnel every briefing (hook, session_start, console
	// preview) passes through refuses a project the caller's OS account may not
	// read, before anything is loaded.
	if !access.Visible(ctx, project) {
		return "", nil, access.Refusal(ctx, project)
	}
	// Isolation is resolved once per briefing, for the header line that tells the
	// agent which world it is in. The narrowing itself is not decided here: the
	// scope query (scopedActiveMemories) and every family surface (familyPeers)
	// each ask store.CanRead for themselves.
	iso, err := store.IsolationOf(ctx, s.db, project)
	if err != nil {
		return "", nil, err
	}
	// A child project (one with a parent) sees its shared parent's active memories
	// in-briefing too, so cross-platform knowledge kept in the parent surfaces in
	// each child without being duplicated (see the arctop-app split).
	extra, err := s.familyMemoryScope(ctx, project, cfg.IncludeParentMemories)
	if err != nil {
		return "", nil, err
	}
	mems, err := s.scopedActiveMemories(ctx, project, extra)
	if err != nil {
		return "", nil, err
	}
	var constraints, stageMems, favorites, conventions []core.Memory
	index := make([]core.Memory, 0, len(mems))
	for _, m := range mems {
		switch {
		case m.Kind == core.KindConstraint:
			constraints = append(constraints, m)
		case m.Kind == core.KindStage:
			stageMems = append(stageMems, m)
		case m.Kind == core.KindConvention:
			// Project-local choices/facts: their own budget-competing section,
			// out of the index and its recency/count trims (the section's own
			// ConventionMaxFull tier + count line bound it instead). A starred
			// convention stays here -- there is no separate starred section --
			// but keeps the pin: it sorts to the section head and is exempt
			// from the tier split and the budget check (see the assembler).
			conventions = append(conventions, m)
		case m.Favorite:
			// Starred index-kind memories keep the favorite pin without a
			// section of their own: they render as the head of the Memories
			// list, pulled out of the index before the recency/count trims and
			// never budget-dropped. A starred constraint or stage keeps its own
			// pinned section (the cases above win), so nothing renders twice.
			favorites = append(favorites, m)
		default:
			index = append(index, m)
		}
	}

	// Constraints are ranked ahead of the tier split (ConstraintMaxFull):
	// starred first, then constraints a recent mishap referenced (most recent
	// mishap first), then the blended recency+utility order. With tiering
	// disabled (0) the order stays untouched -- the legacy all-full rendering.
	constraints = s.rankConstraints(ctx, project, constraints, cfg)

	// Subagents get constraints plus, when a spawn prompt resolved, a
	// prompt-matched RELEVANT section (they inherit the parent's task context,
	// so no index/findings/tasks sections). The matcher is skipped entirely
	// when no constraints are in scope: the child briefing renders only with
	// constraints >= 1, so there would be nothing to attach the section to. The
	// update notice is never asked for here: it is an owner action the parent
	// session already carries, not news a child can act on.
	if in.AgentType != "" {
		var relevant []promptHit
		if len(constraints) > 0 {
			relevant = s.subagentRelevant(ctx, project, in.Prompt, constraints)
		}
		text, ids := s.assembleSubagent(project, iso, constraints, relevant, cfg)
		return text, ids, nil
	}

	// Utility blend runs after the partition (pinned sections are already out)
	// and before the trims, so the MemoryMaxItems head-slice and the budget
	// drop order both follow the blended ranking rather than raw recency.
	// Conventions take the same blend ahead of their ConventionMaxFull tier
	// split, so the full lines are the ones agents actually use.
	index = s.utilityRankedMemories(ctx, project, index, cfg)
	conventions = s.utilityRankedMemories(ctx, project, conventions, cfg)
	// Starred conventions sort to the section head ahead of the tier split:
	// the favorite pin relocated into the section. The assembler renders them
	// full-tier and budget-exempt, so a star still guarantees rendering.
	slices.SortStableFunc(conventions, func(a, b core.Memory) int {
		switch {
		case a.Favorite == b.Favorite:
			return 0
		case a.Favorite:
			return -1
		default:
			return 1
		}
	})

	// Recency/count trims run AFTER the kind partition above, so constraints and
	// pinned stages are exempt (the never-drop invariant).
	index, omitted := trimMemoryIndex(index, cfg, time.Now())

	var findings []core.Session
	if cfg.FindingsCount > 0 {
		findings, err = store.RecentFindings(ctx, s.db, project, cfg.FindingsCount)
		if err != nil {
			return "", nil, err
		}
		findings = filterFindingsByAge(findings, cfg.FindingsMaxAgeDays, time.Now())
	}
	var ready []core.Task
	if cfg.ReadyTasksShown > 0 {
		ready, err = store.ReadyTasks(ctx, s.db, project)
		if err != nil {
			return "", nil, err
		}
	}
	siblings, err := s.siblingFindings(ctx, project, cfg.SiblingFindingsCount)
	if err != nil {
		return "", nil, err
	}
	siblingMems, err := s.siblingMemories(ctx, project, extra, cfg)
	if err != nil {
		return "", nil, err
	}
	rollups, err := store.ActivePlans(ctx, s.db, project)
	if err != nil {
		return "", nil, err
	}
	pending, err := s.pendingPlans(ctx, project, cfg.PendingPlanMaxDays)
	if err != nil {
		return "", nil, err
	}
	stages := s.pinnedStages(stageMems, cfg.StageUnknownMaxAgeDays, time.Now())
	if len(constraints) == 0 && len(favorites) == 0 && len(conventions) == 0 &&
		len(index) == 0 && omitted == 0 &&
		len(findings) == 0 && len(ready) == 0 && len(siblings) == 0 && len(siblingMems) == 0 &&
		len(stages) == 0 && len(rollups) == 0 && len(pending) == 0 {
		return "", nil, nil
	}
	text, ids := s.assembleBriefing(project, in.Source, briefingSections{
		constraints: constraints, favorites: favorites, conventions: conventions,
		index: index, indexOmitted: omitted,
		findings: findings, ready: ready, siblings: siblings,
		siblingMems: siblingMems, stages: stages, plans: rollups,
		pendingPlans: pending, isolation: iso,
		// Asked for only past the nothing-to-inject return above: the notice
		// rides on a briefing, it never makes an empty one non-empty.
		notice:   s.updateNoticeLine(ctx, in.Host),
		momentum: features.Enabled(s.effectiveFeatures(ctx), features.Momentum),
	}, cfg)
	return text, ids, nil
}

// updateNoticeMaxRunes caps the update-notice line. A notice is one sentence
// and a command, far below it; the cap is defense in depth against a provider
// that returns a paragraph.
const updateNoticeMaxRunes = 300

// updateNoticeLine asks the update-notice provider for the line a main-session
// briefing on host carries: the sanitized text plus its newline, or "" when no
// provider is installed or it has nothing to say. The text is untrusted to the
// briefing whoever builds it, so it goes through sanitizeField -- flattened to
// one line, injection phrases stripped, capped -- and a result the scrub
// empties renders nothing rather than a blank line.
func (s *Service) updateNoticeLine(ctx context.Context, host string) string {
	if s.updateNotice == nil {
		return ""
	}
	text := sanitizeField(s.updateNotice(ctx, host), updateNoticeMaxRunes)
	if text == "" {
		return ""
	}
	return text + "\n"
}

// effectiveBriefing resolves the briefing knobs for one assembly: the file/env
// base layered with the console's runtime override row, so a console save takes
// effect on the next session start without a daemon restart. Failure-soft: an
// unreadable override degrades to the base config rather than blocking the
// agent's session start.
func (s *Service) effectiveBriefing(ctx context.Context) config.Briefing {
	cfg, _, err := store.BriefingConfig(ctx, s.db, s.briefing)
	if err != nil {
		s.logger.Warn("retrieve: briefing override unavailable, using base config", "error", err)
		return s.briefing
	}
	return cfg
}

// effectiveFeatures resolves the optional-features state for one assembly the
// same way: file/env base, console override row on top, failure-soft. The
// briefing checks the same effective config the console gates on, so "momentum
// off" means off everywhere at once.
func (s *Service) effectiveFeatures(ctx context.Context) config.Features {
	cfg, _, err := store.FeaturesConfig(ctx, s.db, s.features)
	if err != nil {
		s.logger.Warn("retrieve: features override unavailable, using base config", "error", err)
		return s.features
	}
	return cfg
}

// briefingRecencyHalfLifeDays is the half-life of the recency term in the
// blended index sort key. It matches the utility score's own half-life (store
// side) so the two terms decay on the same clock.
const briefingRecencyHalfLifeDays = 14.0

// utilityRankedMemories re-orders a briefing memory slice (the index, and the
// constraints ahead of their tier split) by the blended recency+utility key
// when utility ranking is active for the project (mode "on", or "auto" with
// the gardener's readiness latch / owner force). Both state and score reads
// are failure-soft: the briefing must never fail -- or change composition --
// because a stats read did. With weight 0 or inactive state the slice is
// returned untouched (the legacy recency order).
func (s *Service) utilityRankedMemories(ctx context.Context, project string, mems []core.Memory, cfg config.Briefing) []core.Memory {
	if cfg.UtilityWeight <= 0 || len(mems) < 2 {
		return mems
	}
	activation, err := store.GetUtilityActivation(ctx, s.db)
	if err != nil {
		s.logger.Warn("retrieve: utility activation unavailable, briefing keeps recency order", "error", err)
		return mems
	}
	if !activation.Active(project, cfg.UtilityMode) {
		return mems
	}
	utility, err := store.UtilityScores(ctx, s.db)
	if err != nil {
		s.logger.Warn("retrieve: utility scores unavailable, briefing keeps recency order", "error", err)
		return mems
	}
	return rankMemories(mems, utility, cfg.UtilityWeight, time.Now())
}

// rankMemories sorts memories by (1-w)*recency + w*utility, both in [0,1)
// (recency decays with briefingRecencyHalfLifeDays). Ties break Updated desc
// then ID desc, replicating ActiveMemoriesForScope's SQL order so w=0 or
// all-zero utility reproduces the legacy ordering exactly.
func rankMemories(index []core.Memory, utility map[string]float64, weight float64, now time.Time) []core.Memory {
	key := func(m core.Memory) float64 {
		days := now.Sub(m.Updated).Hours() / 24
		if days < 0 {
			days = 0
		}
		recency := math.Exp2(-days / briefingRecencyHalfLifeDays)
		return (1-weight)*recency + weight*utility[m.ID]
	}
	out := make([]core.Memory, len(index))
	copy(out, index)
	sort.Slice(out, func(i, j int) bool {
		ki, kj := key(out[i]), key(out[j])
		if ki != kj {
			return ki > kj
		}
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// mishapPinWindowDays bounds the briefing's mishap promotion: a constraint
// referenced by an agent.mishap event recorded within this many days is
// promoted to the head of the full tier (store.RecentMishapItemIDs supplies
// the references). Older mishaps stop promoting -- the signal is "this rule
// was violated recently", not a permanent demerit.
const mishapPinWindowDays = 30

// recentMishapRefs reads the project's mishap-referenced memory ids inside the
// promotion window. Failure-soft, the utilityRankedMemories posture: an
// unreadable query costs the promotion, never the briefing. Promotion is a
// briefing-side ordering signal only, like favorites -- it never feeds utility
// scores, recall, or search (the closed-loop-utility-signal-contract).
func (s *Service) recentMishapRefs(ctx context.Context, project string) map[string]time.Time {
	mishaps, err := store.RecentMishapItemIDs(ctx, s.db, project, mishapPinWindowDays*24*time.Hour)
	if err != nil {
		s.logger.Warn("retrieve: recent mishaps unavailable, briefing skips promotion", "error", err)
		return nil
	}
	return mishaps
}

// rankConstraints orders constraints for the tiered rendering: priority class
// first (constraintPriority -- starred constraints claim full-tier slots
// before mishap-promoted ones, which claim them before anything else), then
// within a class the blended recency+utility order -- the same
// utilityRankedMemories gate and key as the memory index, degrading to pure
// recency (the SQL updated_at DESC order) when utility is inactive or weighted
// 0. The mishap class alone re-sorts internally, most recent mishap first.
// ConstraintMaxFull 0 disables tiering, so the input order is returned
// untouched: the legacy all-full rendering stays exactly as it was, favorites
// and mishaps included -- promotion is part of the tier ranking only, and the
// mishap query is skipped entirely.
func (s *Service) rankConstraints(ctx context.Context, project string, constraints []core.Memory, cfg config.Briefing) []core.Memory {
	if cfg.ConstraintMaxFull <= 0 || len(constraints) < 2 {
		return constraints
	}
	mishaps := s.recentMishapRefs(ctx, project)
	ranked := s.utilityRankedMemories(ctx, project, constraints, cfg)
	out := make([]core.Memory, len(ranked))
	copy(out, ranked)
	slices.SortStableFunc(out, func(a, b core.Memory) int {
		ca, cb := constraintPriority(a, mishaps), constraintPriority(b, mishaps)
		if c := cmp.Compare(ca, cb); c != 0 {
			return c
		}
		if ca == constraintClassMishap {
			// Most recent mishap first; equal stamps keep the blended order
			// (the sort is stable).
			return mishaps[b.ID].Compare(mishaps[a.ID])
		}
		return 0
	})
	return out
}

// Constraint priority classes for the tiered ordering: a lower class claims
// full-tier slots first, and the blended order (rankConstraints) is preserved
// within a class -- except the mishap class, which orders by most recent
// mishap. A star outranks a mishap deliberately: the star is an explicit
// owner signal, the mishap an implicit one, so a starred constraint sorts as
// a favorite even when a recent mishap also references it.
const (
	constraintClassFavorite = iota // starred by the owner or an agent
	constraintClassMishap          // referenced by an agent.mishap event within the window
	constraintClassBlended         // everything else: the blended recency+utility order
)

// constraintPriority is a constraint's class in the tiered ordering. mishaps
// maps memory ids to their most recent referencing agent.mishap timestamp
// (store.RecentMishapItemIDs); nil or empty means no promotion.
func constraintPriority(m core.Memory, mishaps map[string]time.Time) int {
	if m.Favorite {
		return constraintClassFavorite
	}
	if _, ok := mishaps[m.ID]; ok {
		return constraintClassMishap
	}
	return constraintClassBlended
}

// constraintTiers splits ranked constraints at the full-tier boundary: the top
// maxFull render as full CONSTRAINT lines, the rest collapse into the compact
// "Also binding" line. maxFull 0 disables tiering (everything renders full,
// the legacy behavior).
func constraintTiers(constraints []core.Memory, maxFull int) (full, compact []core.Memory) {
	if maxFull <= 0 || len(constraints) <= maxFull {
		return constraints, nil
	}
	return constraints[:maxFull], constraints[maxFull:]
}

// alsoBindingLine collapses the constraints past the full tier into one
// compact pinned line that still names every one -- the name is what
// memory_read takes, so the never-drop invariant holds in compact form.
// Returns "" when the compact tier is empty.
func alsoBindingLine(compact []core.Memory) string {
	if len(compact) == 0 {
		return ""
	}
	names := make([]string, 0, len(compact))
	for _, c := range compact {
		names = append(names, sanitizeName(c.Name, 80))
	}
	return fmt.Sprintf("- +%d more, equally binding -- memory_read name=<name> before working near one: %s\n",
		len(compact), strings.Join(names, ", "))
}

// conventionCountLine closes the Conventions section with the pool size and the
// premade filter for whatever the tier or budget left unrendered. Unlike
// alsoBindingLine it carries no names -- a count is the demotion -- so it is
// cheap enough to render unconditionally, keeping the section discoverable
// even when every full line was budget-dropped. The kind=convention hint is a
// real command: recall with kind and no query lists the kind newest-first.
func conventionCountLine(total, shown int) string {
	if shown < total {
		return fmt.Sprintf("(%d total, %d shown -- recall kind=convention for the rest)\n", total, shown)
	}
	return fmt.Sprintf("(%d total -- recall kind=convention lists them)\n", total)
}

// conventionTiers splits ranked conventions at the full-tier boundary like
// constraintTiers, except starred conventions -- sorted to the head by the
// caller -- always make the full tier even past maxFull: the star's render
// guarantee survives the section fold. maxFull 0 disables tiering.
func conventionTiers(conventions []core.Memory, maxFull int) (full, compact []core.Memory) {
	if maxFull <= 0 || len(conventions) <= maxFull {
		return conventions, nil
	}
	starred := 0
	for starred < len(conventions) && conventions[starred].Favorite {
		starred++
	}
	return conventions[:max(maxFull, starred)], conventions[max(maxFull, starred):]
}

// countNoun renders "1 constraint" / "3 constraints" for the header breakdown.
func countNoun(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// kindBreakdown renders the header's parenthesized kind counts, naming only
// the nonzero pinned/sectioned kinds ("" when all are zero) -- the shape of
// the store at a glance without a row of noisy zeros.
func kindBreakdown(constraints, conventions, stages int) string {
	var parts []string
	if constraints > 0 {
		parts = append(parts, countNoun(constraints, "constraint", "constraints"))
	}
	if conventions > 0 {
		parts = append(parts, countNoun(conventions, "convention", "conventions"))
	}
	if stages > 0 {
		parts = append(parts, countNoun(stages, "stage", "stages"))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// trimMemoryIndex applies the recency filter (MemoryMaxAgeDays) and item cap
// (MemoryMaxItems) to the briefing's memory index, returning the kept lines and
// how many were omitted. The omitted count folds into the assembler's
// "(+N older -- use recall)" trailer so trimmed knowledge stays discoverable.
// Zero-valued knobs disable their trim (the default: budget-only).
func trimMemoryIndex(index []core.Memory, cfg config.Briefing, now time.Time) ([]core.Memory, int) {
	kept := index
	if cfg.MemoryMaxAgeDays > 0 {
		cutoff := now.AddDate(0, 0, -cfg.MemoryMaxAgeDays)
		fresh := make([]core.Memory, 0, len(kept))
		for _, m := range kept {
			if m.Updated.After(cutoff) {
				fresh = append(fresh, m)
			}
		}
		kept = fresh
	}
	if cfg.MemoryMaxItems > 0 && len(kept) > cfg.MemoryMaxItems {
		kept = kept[:cfg.MemoryMaxItems]
	}
	return kept, len(index) - len(kept)
}

// filterFindingsByAge drops findings older than maxAgeDays (0 = keep all).
// Findings arrive newest-first, so only the tail is ever dropped.
func filterFindingsByAge(findings []core.Session, maxAgeDays int, now time.Time) []core.Session {
	if maxAgeDays <= 0 {
		return findings
	}
	cutoff := now.AddDate(0, 0, -maxAgeDays)
	kept := make([]core.Session, 0, len(findings))
	for _, f := range findings {
		if f.UpdatedAt.After(cutoff) {
			kept = append(kept, f)
		}
	}
	return kept
}

// pendingPlans returns the project's recently captured, not-yet-approved
// Claude Code plans (plan-status draft/presented updated within maxDays days;
// 0 = no age cutoff), newest first. Anything older is presumed stale (the
// gardener proposes abandoning it) and stops earning briefing space. These earn
// budget-participating "awaiting approval" lines -- unlike the pinned
// active-plan rollups, a pending plan is a hint, not a commitment.
func (s *Service) pendingPlans(ctx context.Context, project string, maxDays int) ([]core.Note, error) {
	if project == "" {
		return nil, nil // the global scope aggregates too much to be useful
	}
	notes, err := store.NotesByTag(ctx, s.db, project, plans.TagPlan)
	if err != nil {
		return nil, err
	}
	var cutoff time.Time
	if maxDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -maxDays)
	}
	var out []core.Note
	for _, n := range notes {
		switch plans.StatusFromTags(n.Tags) {
		case plans.StatusDraft, plans.StatusPresented:
			if n.Updated.After(cutoff) {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// familyMemoryScope returns the extra project slugs whose active memories should
// be folded into project's briefing: its shared parent, when set and the
// IncludeParentMemories knob is on (the default). It is a no-op (nil) for the
// global scope or a project with no parent. Kept small on purpose -- a child
// inherits its parent's shared memories, not its siblings' platform-specific
// ones (those cross over only via siblingFindings/siblingMemories).
func (s *Service) familyMemoryScope(ctx context.Context, project string, includeParent bool) ([]string, error) {
	if project == "" || !includeParent {
		return nil, nil
	}
	p, ok, err := store.ProjectBySlug(ctx, s.db, project)
	if err != nil || !ok || p.ParentSlug == "" {
		return nil, err
	}
	// The isolation fence applies to the parent link like every other family
	// surface: an isolated project neither folds a parent's memories into its
	// own scope nor lends its own to one.
	return s.familyPeers(ctx, project, []string{p.ParentSlug})
}

// siblingFindings gathers up to limit recent findings from a project's family
// members (see store.SiblingProjects/SiblingFindings), for the briefing's
// "Sibling projects" section. It is a no-op for the global scope, a project
// with no configured family, limit 0 (the section disabled), or a family the
// isolation fence empties (familyPeers).
func (s *Service) siblingFindings(ctx context.Context, project string, limit int) ([]core.Session, error) {
	if limit <= 0 {
		return nil, nil
	}
	siblings, err := store.SiblingProjects(ctx, s.db, project)
	if err != nil || len(siblings) == 0 {
		return nil, err
	}
	siblings, err = s.familyPeers(ctx, project, siblings)
	if err != nil || len(siblings) == 0 {
		return nil, err
	}
	return store.SiblingFindings(ctx, s.db, siblings, limit)
}

// siblingMemories gathers family members' active memories for the opt-in
// "Sibling memories" cross-over (IncludeSiblingMemories, default off).
// Constraints and stages are excluded -- a sibling's gates bind its own
// sessions, not this one -- slugs already folded into the main scope (the
// shared parent) are skipped so nothing renders twice, and the isolation fence
// drops family members in either direction (familyPeers). The memory recency
// filter applies here too; the item cap does not (the section already packs
// after the own-project index, so budget drops it first).
func (s *Service) siblingMemories(ctx context.Context, project string, already []string, cfg config.Briefing) ([]core.Memory, error) {
	if !cfg.IncludeSiblingMemories {
		return nil, nil
	}
	siblings, err := store.SiblingProjects(ctx, s.db, project)
	if err != nil || len(siblings) == 0 {
		return nil, err
	}
	siblings, err = s.familyPeers(ctx, project, siblings)
	if err != nil || len(siblings) == 0 {
		return nil, err
	}
	skip := make(map[string]bool, len(already))
	for _, slug := range already {
		skip[slug] = true
	}
	fetch := make([]string, 0, len(siblings))
	for _, slug := range siblings {
		if !skip[slug] {
			fetch = append(fetch, slug)
		}
	}
	mems, err := store.ActiveMemoriesForProjects(ctx, s.db, fetch)
	if err != nil {
		return nil, err
	}
	var cutoff time.Time
	if cfg.MemoryMaxAgeDays > 0 {
		cutoff = time.Now().AddDate(0, 0, -cfg.MemoryMaxAgeDays)
	}
	var out []core.Memory
	for _, m := range mems {
		if m.Kind == core.KindConstraint || m.Kind == core.KindStage {
			continue
		}
		if !m.Updated.After(cutoff) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func projectLabel(project string) string {
	if project == "" {
		return "(global)"
	}
	return project
}

// briefingSections carries the assembled, pre-filtered content of a full
// briefing (subagents use assembleSubagent instead). Grouping the sections keeps
// the assembler signature stable as new sections are added.
type briefingSections struct {
	constraints  []core.Memory // ranked (rankConstraints); tier-split by ConstraintMaxFull at render
	favorites    []core.Memory // starred non-constraint/stage memories, pinned after plans
	conventions  []core.Memory // ranked; tier-split by ConventionMaxFull at render, budget-competing
	index        []core.Memory
	indexOmitted int // index lines cut by the recency/count trims, for the "+N older" trailer
	findings     []core.Session
	ready        []core.Task
	siblings     []core.Session     // recent findings from family-member projects
	siblingMems  []core.Memory      // family members' memories (opt-in cross-over)
	stages       []stageLine        // non-done stage memories, pinned after constraints
	plans        []store.PlanRollup // active plans (a plan-tagged task set), pinned after stages
	pendingPlans []core.Note        // captured, not-yet-approved CC plans (budget-participating)
	isolation    core.Isolation     // the project's fence state; open renders no line
	notice       string             // sanitized update-notice line ("" = none), pinned under the isolation line
	momentum     bool               // momentum feature on: near-done plan lines carry the finish-line emphasis
}

// assembleBriefing packs the grouped sections against the token budget. The
// pinned content -- the header line, the isolation line, the update notice,
// the Constraints section (both tiers), the Stages section, the active-plan
// rollups with their trailer, and the footer -- is counted first and never
// dropped.
// Budget-competing rows then pack in render order -- situation before library:
// pending plan lines > conventions > recent findings > ready tasks > memory
// index > sibling findings > sibling memories -- so budget priority and render
// priority agree: a fat memory index can no longer evict the findings that
// render above it, and the sibling sections, last, are dropped first. Starred
// rows carry the favorite pin into their sections: starred conventions and the
// starred head of the Memories list render exempt from the budget check. The
// header counts the findings actually rendered (headLine defers for that), and
// findings cut by budget leave a "+N more" trailer, mirroring the memory index.
// The whole is hard-capped. The second return value is the ids of the memories
// actually rendered (constraints, pinned stages, starred rows, and the
// convention/index/sibling lines that survived budgeting) -- for retrieval
// instrumentation. Findings, siblings, and ready tasks are sessions/tasks, not
// memory_read-able memories, so they are omitted from the funnel.
func (s *Service) assembleBriefing(project, source string, sec briefingSections, cfg config.Briefing) (string, []string) {
	constraints, index := sec.constraints, sec.index
	findings, ready := sec.findings, sec.ready
	label := projectLabel(project)
	budget := s.packingBudget()
	hardCap := s.briefingHardCap(cfg)

	ids := make([]string, 0, len(constraints)+len(sec.stages)+len(index))

	// The header line carries the count of findings that actually render, which
	// is only known after the findings section packs below; headLine defers its
	// composition. Estimating the budget with the pre-packing count is fine --
	// the two strings differ by at most a digit.
	//
	// Constraints, conventions, and pinned stages ARE memories, just sectioned,
	// so the header reports one memory total with the kind counts called out as
	// its subsets. Reporting them as disjoint pools ("6 constraints, 76
	// memories") would contradict the body, where the sections, the index, and
	// the "+N older" trailer sum to the total.
	totalMems := len(constraints) + len(sec.favorites) + len(sec.conventions) + len(index) + len(sec.stages)
	breakdown := kindBreakdown(len(constraints), len(sec.conventions), len(sec.stages))
	headLine := func(renderedFindings int) string {
		return fmt.Sprintf("<seam-briefing>\nSeam project: %s -- %d memories%s, %d recent findings.\n",
			sanitizeName(label, 80), totalMems, breakdown, renderedFindings)
	}

	// Pinned sections are composed up front so their cost is reserved before
	// any budget-competing row packs: a droppable line can never squeeze out
	// pinned content that renders after it.
	var constraintsSec strings.Builder
	if len(constraints) > 0 {
		constraintsSec.WriteString("\nConstraints (binding for every session):\n")
		full, compact := constraintTiers(constraints, cfg.ConstraintMaxFull)
		for _, c := range full {
			constraintsSec.WriteString("- " + sanitizeName(c.Name, 80) + ": " + sanitizeField(c.Description, 160) + "\n")
			ids = append(ids, c.ID)
		}
		// The compact tier is part of the same pinned section -- budget can drop
		// neither tier -- and its ids are recorded like the full lines' so the
		// read-after-inject funnel keeps seeing every constraint.
		constraintsSec.WriteString(alsoBindingLine(compact))
		for _, c := range compact {
			ids = append(ids, c.ID)
		}
	}
	// The pinned Stages section sits right after constraints and, like them, is
	// never dropped for budget -- a gated stage's status is load-bearing for
	// the whole session.
	stagesSec := stagesSection(sec.stages)
	for _, st := range sec.stages {
		ids = append(ids, st.id)
	}
	// Active-plan rollups follow stages, also pinned: a plan's
	// claimable/in-flight counts tell the next agent what work is available to
	// pick up right now. The section header and trailer are pinned with them;
	// the pending (awaiting approval) lines interleave between rollups and
	// trailer below but budget-compete.
	const plansHeader = "\nPlans:\n"
	var rollups strings.Builder
	plansTrailer := ""
	if len(sec.plans) > 0 {
		for _, p := range sec.plans {
			fmt.Fprintf(&rollups, "- %s -- %d/%d done, %d claimable, %d in flight",
				sanitizeName(p.Slug, 80), p.Done, p.Total, p.Claimable, p.InFlight)
			// Momentum's finish-line emphasis: the same qualification and phrase
			// the Overview card derives (store.PlanRollup), gated by the same
			// effective feature state, so an agent and the owner read one story.
			if sec.momentum && p.AtFinishLine() {
				rollups.WriteString(" -- " + p.FinishLinePhrase())
			}
			rollups.WriteString("\n")
		}
		plansTrailer = "(steps: tasks_ready plan=<slug>; claim: tasks_claim id=<task id> session=<your Seam session>; attach work via the plan:<slug> tag)\n"
	}

	var tail strings.Builder
	tail.WriteString(agentguide.BriefingFooter)
	if source == "compact" || source == "resume" {
		tail.WriteString("(resumed session -- earlier context may be summarized; recall to re-ground.)\n")
	}
	tail.WriteString("</seam-briefing>")

	pinnedPlans := ""
	if len(sec.plans) > 0 {
		pinnedPlans = plansHeader + rollups.String() + plansTrailer
	}
	// The isolation line is pinned directly under the header: an agent that
	// cannot see WHY its briefing carries no global memories or no family would
	// go looking for both. Open projects render nothing and pay nothing.
	isoLine := isolationLine(sec.isolation)
	// The update notice follows it, pinned the same way: its cost is reserved
	// before any budget-competing row packs, so none can squeeze it out, and
	// sitting at the head keeps it inside the prefix both hardTruncate and the
	// Codex hook cap preserve. No provider, or nothing to say, renders nothing
	// and pays nothing (estTokens("") is 0).
	used := estTokens(headLine(len(findings))) + estTokens(isoLine) + estTokens(sec.notice) +
		estTokens(constraintsSec.String()) +
		estTokens(stagesSec) + estTokens(pinnedPlans) + estTokens(tail.String())

	var body strings.Builder
	body.WriteString(isoLine)
	body.WriteString(sec.notice)
	body.WriteString(constraintsSec.String())
	body.WriteString(stagesSec)

	// Pending (unapproved) plan lines pack first among the budget-competing
	// rows -- deliberate: they are few and PendingPlanMaxDays-bounded, so they
	// cannot crowd out what follows the way a fat index could. With no active
	// rollups the section header is not pinned; it packs with the first
	// pending line instead, so a pending-only section still budget-competes as
	// a whole.
	if len(sec.plans) > 0 {
		body.WriteString(plansHeader)
		body.WriteString(rollups.String())
		for _, n := range sec.pendingPlans {
			line := pendingPlanLine(n)
			if used+estTokens(line) > budget {
				break
			}
			body.WriteString(line)
			used += estTokens(line)
		}
		body.WriteString(plansTrailer)
	} else if len(sec.pendingPlans) > 0 {
		wroteHeader := false
		for _, n := range sec.pendingPlans {
			line := pendingPlanLine(n)
			cost := estTokens(line)
			if !wroteHeader {
				cost += estTokens(plansHeader)
			}
			if used+cost > budget {
				break
			}
			if !wroteHeader {
				body.WriteString(plansHeader)
				wroteHeader = true
			}
			body.WriteString(line)
			used += cost
		}
	}

	// Conventions -- project-local choices/facts, the constraint head's demoted
	// sibling tier -- render right below the plans: rules-adjacent, but
	// budget-COMPETING, unlike constraints. Starred conventions (sorted to the
	// section head) render exempt from the budget check -- the favorite pin
	// folded into the section -- then up to ConventionMaxFull full lines pack
	// against the budget (0 = all full, matching ConstraintMaxFull); the count
	// line then always renders, exempt too, so the section is never invisible
	// and the rest stay one kind-filtered recall away. Only rendered lines'
	// ids are recorded: the count line names nothing, so counting its hidden
	// conventions as surfaced would poison the read-after-inject funnel with
	// exposure that never happened.
	if len(sec.conventions) > 0 {
		lead := "\nConventions (project-local choices):\n"
		body.WriteString(lead)
		used += estTokens(lead)
		full, _ := conventionTiers(sec.conventions, cfg.ConventionMaxFull)
		shown := 0
		for _, m := range full {
			line := "- " + sanitizeName(m.Name, 80) + ": " + sanitizeField(m.Description, 160) + "\n"
			if !m.Favorite && used+estTokens(line) > budget {
				break
			}
			body.WriteString(line)
			used += estTokens(line)
			ids = append(ids, m.ID)
			shown++
		}
		line := conventionCountLine(len(sec.conventions), shown)
		body.WriteString(line)
		used += estTokens(line)
	}

	// Recent findings pack before the memory index: they are few
	// (FindingsCount-capped) and say what just happened here, so a fat index
	// can neither out-render nor out-budget them.
	findingsRendered := 0
	if len(findings) > 0 {
		lead := "\nRecent findings:\n"
		if used+estTokens(lead) <= budget {
			body.WriteString(lead)
			used += estTokens(lead)
			for _, f := range findings {
				line := "- " + sanitizeName(f.Name, 80) + " (" + humanAge(f.UpdatedAt) + "): " + clipWords(sanitizeField(f.Findings, 0), 200) + "\n"
				if used+estTokens(line) > budget {
					break
				}
				body.WriteString(line)
				used += estTokens(line)
				findingsRendered++
			}
			if cut := len(findings) - findingsRendered; cut > 0 {
				// No retrieval hint: session findings are not recallable, so
				// naming a mechanism here would point at a command that cannot
				// reach them.
				extra := fmt.Sprintf("- (+%d more)\n", cut)
				body.WriteString(extra)
				used += estTokens(extra)
			}
		}
	}

	// The ready-tasks section follows the findings: the queue is part of the
	// situation, not the library. It packs all-or-nothing, like the single
	// line it replaces -- a header with no rows underneath would read as an
	// empty queue.
	if section := readyTasksSection(ready, cfg.ReadyTasksShown); section != "" && used+estTokens(section) <= budget {
		body.WriteString(section)
		used += estTokens(section)
	}

	dropped := 0
	if len(sec.favorites) > 0 || len(index) > 0 || sec.indexOmitted > 0 {
		lead := "\nMemories (" + sanitizeName(label, 80) + "):\n"
		body.WriteString(lead)
		used += estTokens(lead)
		// Starred memories head the list, exempt from the budget check: the
		// favorite pin folded into the section (partitioned out before the
		// recency/count trims, so those never saw them either).
		for _, m := range sec.favorites {
			line := "- " + sanitizeName(m.Name, 80) + ": " + sanitizeField(m.Description, 160) + "\n"
			body.WriteString(line)
			used += estTokens(line)
			ids = append(ids, m.ID)
		}
		for i, m := range index {
			line := "- " + sanitizeName(m.Name, 80) + ": " + sanitizeField(m.Description, 160) + "\n"
			if used+estTokens(line) > budget && (i > 0 || len(sec.favorites) > 0) {
				dropped = len(index) - i
				break
			}
			body.WriteString(line)
			used += estTokens(line)
			ids = append(ids, m.ID)
		}
		// The trailer counts both budget-dropped lines and the recency/count trims,
		// so filtered-out knowledge stays discoverable via recall.
		if dropped+sec.indexOmitted > 0 {
			extra := fmt.Sprintf("- (+%d older -- recall query=<topic>, optionally kind=<kind>)\n", dropped+sec.indexOmitted)
			body.WriteString(extra)
			used += estTokens(extra)
		}
	}

	if len(sec.siblings) > 0 {
		lead := "\nSibling projects:\n"
		if used+estTokens(lead) <= budget {
			body.WriteString(lead)
			used += estTokens(lead)
			for _, f := range sec.siblings {
				line := "- " + sanitizeName(f.ProjectSlug, 60) + " (" + humanAge(f.UpdatedAt) + "): " + clipWords(sanitizeField(f.Findings, 0), 150) + "\n"
				if used+estTokens(line) > budget {
					break
				}
				body.WriteString(line)
				used += estTokens(line)
			}
		}
	}

	// Sibling memories pack right after the sibling findings: durable cross-over
	// knowledge, deliberately below the own-project index so budget drops it first.
	if len(sec.siblingMems) > 0 {
		lead := "\nSibling memories:\n"
		if used+estTokens(lead) <= budget {
			body.WriteString(lead)
			used += estTokens(lead)
			for _, m := range sec.siblingMems {
				line := "- " + sanitizeName(m.Project, 60) + "/" + sanitizeName(m.Name, 80) + ": " + sanitizeField(m.Description, 160) + "\n"
				if used+estTokens(line) > budget {
					break
				}
				body.WriteString(line)
				used += estTokens(line)
				ids = append(ids, m.ID)
			}
		}
	}

	return hardTruncate(headLine(findingsRendered)+body.String()+tail.String(), hardCap), ids
}

// pendingPlanLine renders one captured, not-yet-approved CC plan as a row of
// the Plans section -- a hint, not a commitment, so unlike the rollup rows it
// budget-competes (see assembleBriefing).
func pendingPlanLine(n core.Note) string {
	return fmt.Sprintf("- %s -- awaiting approval: %s (%s, %s)\n",
		sanitizeName(plans.SlugFromTags(n.Tags), 80), sanitizeField(n.Title, 80),
		plans.StatusFromTags(n.Tags), humanAge(n.Updated))
}

// readyTasksSection renders the briefing's ready-queue section ("Ready tasks
// (N):" with up to shown newest-first title bullets and the premade-filter
// trailer), or "" when none are ready or the section is disabled (shown 0).
// Display-only recency sort: store.ReadyTasks (and the tasks_ready tool the
// trailer points at) stays oldest-first because claiming is intentionally FIFO.
func readyTasksSection(ready []core.Task, shown int) string {
	if len(ready) == 0 || shown <= 0 {
		return ""
	}
	newestFirst := slices.Clone(ready)
	slices.SortStableFunc(newestFirst, func(a, b core.Task) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "\nReady tasks (%d):\n", len(newestFirst))
	for i, t := range newestFirst {
		if i == shown {
			break
		}
		b.WriteString("- " + sanitizeField(t.Title, 60) + "\n")
	}
	b.WriteString("(full queue with ids: tasks_ready; claim: tasks_claim id=<id> session=<your Seam session>)\n")
	return b.String()
}

// subagentFooter is the always-on tool-guidance line closing every non-empty
// subagent briefing: the first sentence of agentguide.BriefingFooter (a test
// pins the prefix relation so the vocabulary cannot drift). Without it a child
// in a project with few constraints would get no hint that recall/memory_read
// exist -- alsoBindingLine names memory_read only past the tier cap. The mild
// redundancy between the two lines when both render is accepted by design.
const subagentFooter = "Recall on demand with recall; read a memory with memory_read.\n"

// subagentRelevantMax caps the briefing's RELEVANT section. Prompt-matched
// memories are a pointer, not a second memory index -- the child pulls more
// with recall (the footer says how).
const subagentRelevantMax = 3

// subagentRelevant matches a child's spawn prompt against the project scope's
// memories -- all kinds, via the same corpus the <seam-recall> engine uses --
// and returns up to subagentRelevantMax hits not already rendered as
// constraints. The dedupe runs over the UNCAPPED ranking (scorePromptAll), so
// a constraint the child already sees never costs the section a genuinely-new
// memory ranked below it. Failure-soft throughout: an empty prompt, an
// unavailable corpus, or no qualifying hits yield nil, never a failed
// briefing.
//
// Utility boundary (the closed-loop-utility-signal-contract memory): these are
// briefing injects, weight 0. The hook records them under the subagent-start
// surface -- never as prompt-class demand, even though the prompt matcher
// produced them: spawn-prompt matching is automated supply, not the child
// choosing to ask. Demand stays the child's own memory_read/recall calls.
func (s *Service) subagentRelevant(ctx context.Context, project, prompt string, constraints []core.Memory) []promptHit {
	tokens := promptTokenize(prompt)
	if len(tokens) == 0 {
		return nil
	}
	corpus, err := s.promptCorpusFor(ctx, project)
	if err != nil {
		s.logger.Warn("retrieve: subagent prompt corpus unavailable, briefing omits the RELEVANT section", "error", err)
		return nil
	}
	pinned := make(map[string]struct{}, len(constraints))
	for _, c := range constraints {
		pinned[c.ID] = struct{}{}
	}
	var out []promptHit
	for _, h := range scorePromptAll(tokens, corpus) {
		if _, ok := pinned[h.id]; ok {
			continue
		}
		out = append(out, h)
		if len(out) == subagentRelevantMax {
			break
		}
	}
	return out
}

// packingBudget is the token budget assembleBriefing packs droppable rows
// into: budgets.max_briefing_tokens, or 1500 when unset.
func (s *Service) packingBudget() int {
	if budget := s.budgets.MaxBriefingTokens; budget > 0 {
		return budget
	}
	return 1500
}

// briefingHardCap is the absolute ceiling hardTruncate enforces on an
// assembled briefing: the packing budget times cfg.HardCapMultiplier (2 when
// unset).
func (s *Service) briefingHardCap(cfg config.Briefing) int {
	mult := cfg.HardCapMultiplier
	if mult <= 0 {
		mult = 2
	}
	return s.packingBudget() * mult
}

// assembleSubagent renders the briefing for a subagent, or "" if there are no
// constraints in scope (relevant hits alone never render -- the child briefing
// exists only where constraints do). Constraints arrive already ranked
// (rankConstraints) and get the same grouped Constraints section and
// ConstraintMaxFull tier split as the full briefing -- a subagent drowns in an
// all-full wall just the same. The prompt-matched "Relevant to this task"
// section sits between the Constraints section and the footer; whenever the
// briefing renders it closes with subagentFooter, independent of the tier
// split, and the whole is hard-capped like the main briefing. The isolation
// line sits directly under the header, exactly as in assembleBriefing: a child
// is an agent inside the fence too, and its scope is already narrowed by the
// time it gets here (Briefing resolves the scope before the subagent branch),
// so without the line its world is silently smaller than it can explain. Being
// the first line after the header also pins it: hardTruncate cuts the tail.
// The second return value is the ids of the rendered memories (both constraint
// tiers plus the relevant hits) for retrieval instrumentation; like
// assembleBriefing's, it is a superset when the hard cap truncates lines.
func (s *Service) assembleSubagent(project string, iso core.Isolation, constraints []core.Memory, relevant []promptHit, cfg config.Briefing) (string, []string) {
	if len(constraints) == 0 {
		return "", nil
	}
	label := projectLabel(project)
	ids := make([]string, 0, len(constraints)+len(relevant))
	var b strings.Builder
	b.WriteString("<seam-briefing>\n")
	fmt.Fprintf(&b, "Seam project: %s -- %s (subagent scope).\n", sanitizeName(label, 80),
		countNoun(len(constraints), "constraint", "constraints"))
	b.WriteString(isolationLine(iso))
	b.WriteString("\nConstraints (binding for every session):\n")
	full, compact := constraintTiers(constraints, cfg.ConstraintMaxFull)
	for _, c := range full {
		b.WriteString("- " + sanitizeName(c.Name, 80) + ": " + sanitizeField(c.Description, 160) + "\n")
		ids = append(ids, c.ID)
	}
	b.WriteString(alsoBindingLine(compact))
	for _, c := range compact {
		ids = append(ids, c.ID)
	}
	if len(relevant) > 0 {
		b.WriteString("\nRelevant to this task:\n")
		for _, h := range relevant {
			b.WriteString("- " + sanitizeName(h.name, 80) + ": " + sanitizeField(h.description, 160) + "\n")
			ids = append(ids, h.id)
		}
	}
	b.WriteString(subagentFooter)
	b.WriteString("</seam-briefing>")
	return hardTruncate(b.String(), s.briefingHardCap(cfg)), ids
}

// hardTruncate caps s at hardCapTokens estimated tokens while preserving the
// closing </seam-briefing> tag, so a truncated briefing is still well-formed.
func hardTruncate(s string, hardCapTokens int) string {
	if estTokens(s) <= hardCapTokens {
		return s
	}
	const closeTag = "\n</seam-briefing>"
	maxChars := hardCapTokens * 4
	if maxChars <= len(closeTag) {
		return s
	}
	body := strings.TrimSuffix(s, "</seam-briefing>")
	body = strings.TrimRight(body, "\n")
	if cut := maxChars - len(closeTag); len(body) > cut {
		// Back off to a rune boundary so truncation never emits invalid UTF-8.
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut] + "..."
	}
	return body + closeTag
}
