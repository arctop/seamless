package console

// Momentum's console-side helpers for project maturity stages: gated
// evaluation (the computation and its latch run only while the feature is
// on), crossing events, and the glyph the board rows and the project detail
// header share.

import (
	"context"
	"fmt"
	"html/template"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// projectStages evaluates the momentum maturity stages for the given board
// rows: nil while the feature is off (no computation, no latch writes), and
// failure-soft -- an error costs the glyphs, never the page. Latch advances
// are recorded as project.stage_reached events, once per crossing by
// construction (the latch only moves forward).
func (s *Service) projectStages(ctx context.Context, rows []store.ProjectBoardRow, now time.Time) map[string]store.ProjectStageInfo {
	if !features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		return nil
	}
	stages, crossings, err := store.EvaluateProjectStagesWith(ctx, s.cfg.DB, s.writeDB(), rows, now)
	if err != nil {
		s.logger.Warn("console: project stages", "error", err)
		return nil
	}
	if s.cfg.Events != nil {
		for _, c := range crossings {
			if _, err := s.cfg.Events.Record(ctx, core.Event{
				Kind: core.EventProjectStage, ProjectSlug: c.Project,
				Payload: map[string]any{"stage": string(c.To), "from": string(c.From)},
			}); err != nil {
				s.logger.Warn("console: record stage crossing", "project", c.Project, "error", err)
			}
		}
	}
	return stages
}

// finishBar renders the finish-line card's thin progress bar: the plan's real
// done/total share drawn in with the area-draw idiom (a pathLength-normalized
// stroke, the chart draw-in register; reduced motion lands it at its final
// width via the global rule). The claim stays verifiable on the surface: the
// SVG title names done, total, and the integer percent -- floor math, the same
// judgment AtFinishLine applies.
func finishBar(p store.PlanRollup) template.HTML {
	if p.Total <= 0 {
		return ""
	}
	pct := p.Done * 100 / p.Total
	title := template.HTMLEscapeString(fmt.Sprintf("%d of %d steps done -- %d%%", p.Done, p.Total, pct))
	return template.HTML(fmt.Sprintf(
		`<svg class="mom-fin-bar" viewBox="0 0 100 4" preserveAspectRatio="none" role="img" aria-label="%s"><title>%s</title><line class="mom-fin-track" x1="0" x2="100" y1="2" y2="2"/><line class="mom-fin-fill" x1="0" x2="%d" y1="2" y2="2" pathLength="1"/></svg>`,
		title, title, pct))
}

// glintKind reports whether an event kind is one of the momentum-born
// celebration kinds whose Overview ledger row wears the mom-glint class -- the
// hook the live-arrival wash rides (see console.css). Keyed on the kind, not
// the feature: all three kinds are minted only while momentum is on
// (projectStages here, the first-reuse latch and the milestone ledger in
// store), and a stale milestone row is filtered out of the feeds while it is
// off (ledgerExcludedKinds), so the class never appears on a page the feature
// did not already touch.
func glintKind(kind string) bool {
	return kind == string(core.EventMemoryFirstReuse) || kind == string(core.EventProjectStage) ||
		kind == string(store.EventMilestoneReached)
}

// stageIcons maps each stage to its Lucide glyph.
var stageIcons = map[store.ProjectStage]string{
	store.StageSeedling:    "circle",
	store.StageSprouting:   "sprout",
	store.StageEstablished: "leaf",
	store.StageDeepRooted:  "tree-pine",
}

// stageGlyph renders a maturity stage as the small pill both surfaces share:
// the stage's glyph and name, with the "why not the next stage yet" progress
// in the title so the owner can see what the project is growing toward.
func stageGlyph(info store.ProjectStageInfo, now time.Time) template.HTML {
	icon := icon(stageIcons[info.Stage])
	return template.HTML(fmt.Sprintf(`<span class="mom-stage" data-stage="%s" title="%s">%s%s</span>`,
		template.HTMLEscapeString(string(info.Stage)),
		template.HTMLEscapeString(stageTitle(info, now)),
		icon, template.HTMLEscapeString(string(info.Stage))))
}

// stageTitle composes the tooltip: the stage held, and -- below the top stage
// -- the exact bars the next one asks for beside where the project stands,
// from the same exported constants the computation reads.
func stageTitle(info store.ProjectStageInfo, now time.Time) string {
	age := info.Inputs.AgeDays(now)
	state := fmt.Sprintf("now %dd, %d memories, %d events", age, info.Inputs.Memories, info.Inputs.Events)
	if info.Inputs.HasReach {
		state += fmt.Sprintf(", %d%% reach", info.Inputs.ReachRate)
	}
	switch info.Stage {
	case store.StageDeepRooted:
		return "deep-rooted -- " + state
	case store.StageEstablished:
		return fmt.Sprintf("established -- deep-rooted at %dd, %d memories, %d events, %d%%+ reach (%s)",
			store.StageDeepRootedMinAgeDays, store.StageDeepRootedMinMemories,
			store.StageDeepRootedMinEvents, store.StageDeepRootedMinReachPct, state)
	case store.StageSprouting:
		return fmt.Sprintf("sprouting -- established at %dd, %d memories, %d events (%s)",
			store.StageEstablishedMinAgeDays, store.StageEstablishedMinMemories,
			store.StageEstablishedMinEvents, state)
	default:
		return fmt.Sprintf("seedling -- sprouting at %dd, %d memories, %d events (%s)",
			store.StageSproutingMinAgeDays, store.StageSproutingMinMemories,
			store.StageSproutingMinEvents, state)
	}
}
