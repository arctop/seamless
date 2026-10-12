package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// eventRow is a display-ready projection of one event-log entry.
type eventRow struct {
	ID        string    `json:"id"`
	When      time.Time `json:"ts"`
	Kind      string    `json:"kind"`
	Project   string    `json:"project,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	ItemID    string    `json:"itemId,omitempty"`
	// ItemIDs are the memories an injection surfaced (capped), so a live client
	// can point at exactly what reached an agent -- the Overview's sky flares
	// those stars. Only injections carry them.
	ItemIDs []string `json:"itemIds,omitempty"`
	Summary string   `json:"summary"`
}

// maxRowItemIDs caps the ids an event row carries: a briefing can surface
// dozens of memories, and the sky only needs enough to light a burst.
const maxRowItemIDs = 48

// recentEvents returns the most recent events as display rows.
func (s *Service) recentEvents(ctx context.Context, limit int) ([]eventRow, error) {
	if s.cfg.Events == nil {
		return nil, nil
	}
	evs, err := s.cfg.Events.RecentExcluding(ctx, limit, s.ledgerExcludedKinds(ctx)...)
	if err != nil {
		return nil, err
	}
	out := make([]eventRow, 0, len(evs))
	for _, e := range evs {
		out = append(out, toEventRow(e))
	}
	return out, nil
}

// recentMishaps returns the latest agent-reported mishaps as recurrence-review
// rows for the overview rail, newest first. Each row is attributed to the
// reporting session's harness and model so the report traces to the exact task.
func (s *Service) recentMishaps(ctx context.Context, limit int) ([]mishapRow, error) {
	if s.cfg.Events == nil {
		return nil, nil
	}
	evs, err := s.cfg.Events.ByKinds(ctx, []core.EventKind{core.EventAgentMishap}, "", "", limit)
	if err != nil {
		return nil, err
	}
	sessOf := s.sourceSessionResolver(ctx)
	out := make([]mishapRow, 0, len(evs))
	for _, e := range evs {
		row := mishapRow{
			ID:          e.ID,
			When:        e.TS,
			Project:     e.ProjectSlug,
			SessionID:   e.SessionID,
			Description: snippet(payloadStr(e.Payload, "description"), 160),
		}
		if e.SessionID != "" {
			sess := sessOf(e.SessionID)
			row.Harness, row.Model = harnessOf(sess), sess.Model
		}
		out = append(out, row)
	}
	return out, nil
}

// ledgerExcludedKinds is the exclusion set for the business ledgers (the
// Overview feed, the Now wire, the project workspace's recent list):
// transport-level Interactions noise always -- tool.call and hook.prompt live
// on the Interactions screen instead -- plus milestone.reached while momentum
// is off. Milestones are the one gated kind consumption must filter itself:
// the events are only minted while the feature is on, but a row minted before
// a switch-off would otherwise keep celebrating in the ledger, and off means
// zero trace. The audit surfaces (the Interactions stream, the event detail
// page) keep the history, exactly like historical trial lines.
func (s *Service) ledgerExcludedKinds(ctx context.Context) []core.EventKind {
	kinds := []core.EventKind{core.EventToolCall, core.EventHookPrompt}
	if !features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		kinds = append(kinds, store.EventMilestoneReached)
	}
	return kinds
}

func toEventRow(e core.Event) eventRow {
	return eventRow{
		ID:        e.ID,
		When:      e.TS,
		Kind:      string(e.Kind),
		Project:   e.ProjectSlug,
		SessionID: e.SessionID,
		ItemID:    e.ItemID,
		ItemIDs:   rowItemIDs(e),
		Summary:   eventSummary(e),
	}
}

// rowItemIDs lists an injection's surfaced memory ids for the live row.
func rowItemIDs(e core.Event) []string {
	if e.Kind != core.EventInjected {
		return nil
	}
	ids := injectedEventItemIDs(e)
	if len(ids) > maxRowItemIDs {
		ids = ids[:maxRowItemIDs]
	}
	return ids
}

// eventSummary renders a one-line human description of an event from its payload.
func eventSummary(e core.Event) string {
	p := e.Payload
	switch e.Kind {
	case core.EventSessionStarted:
		if ambient, _ := p["ambient"].(bool); ambient {
			return "ambient session started"
		}
		return "session started"
	case core.EventSessionEnded:
		return "session ended"
	case core.EventMemoryWritten:
		return "wrote memory " + payloadStr(p, "name")
	case core.EventMemoryRead:
		return "read memory " + payloadStr(p, "name")
	case core.EventMemorySuperseded:
		return "superseded " + payloadStr(p, "name")
	case core.EventMemoryArchived:
		return "archived " + payloadStr(p, "name")
	case core.EventProjectIsolationChanged:
		line := "isolation " + payloadStr(p, "from") + " -> " + payloadStr(p, "to")
		if parent := payloadStr(p, "parent"); parent != "" {
			line += "; detached from " + parent
		}
		return line
	case core.EventRepoMoved:
		if path := payloadStr(p, "new_path"); path != "" {
			return "repo moved to " + path + "; project adopted"
		}
		return "repo moved; project adopted"
	case core.EventNoteWritten:
		return "wrote note " + payloadStr(p, "title")
	case core.EventNoteRead:
		// A read names its note by slug rather than the title the write side
		// records: the tool takes an id or a slug, so the title is not
		// necessarily in hand when the event is written.
		if slug := payloadStr(p, "slug"); slug != "" {
			return "read note " + slug
		}
		return "read note"
	case core.EventFavoriteChanged:
		verb := "starred"
		if fav, _ := p["favorite"].(bool); !fav {
			verb = "unstarred"
		}
		return verb + " " + payloadStr(p, "kind") + " " + payloadStr(p, "id")
	case core.EventTrialRecorded:
		return "recorded trial " + payloadStr(p, "title")
	case core.EventMemoryFirstReuse:
		// The momentum payoff line: the ideation copy, verbatim shape.
		line := strings.TrimSpace(payloadStr(p, "kind") + " " + payloadStr(p, "name"))
		if line == "" {
			line = "a memory"
		}
		return line + " just paid off for the first time"
	case core.EventProjectStage:
		if stage := payloadStr(p, "stage"); stage != "" {
			return "matured to " + stage
		}
		return "maturity stage reached"
	case store.EventMilestoneReached:
		// The milestone line is the payload's claim, verbatim: the mint wrote
		// the exact judged sentence ("100 memories written in seamless") and
		// the real count behind it, so rephrasing here would un-judge it.
		if claim := payloadStr(p, "claim"); claim != "" {
			return claim
		}
		return "milestone reached"
	case core.EventRecordBroken:
		if label := payloadStr(p, "label"); label != "" {
			if n, ok := p["n"].(float64); ok {
				return fmt.Sprintf("new record: %s -- %d", label, int(n))
			}
			return "new record: " + label
		}
		return "personal record broken"
	case eventFeaturesChanged:
		if reset, _ := p["reset"].(bool); reset {
			return "optional features reset to the file configuration"
		}
		return "optional features changed" + featureStateSuffix(p)
	case eventLevelChanged:
		return levelChangeSummary(p)
	case core.EventUpdateAvailable:
		return updateAvailableSummary(p)
	case core.EventUpdateApplied:
		return updateAppliedSummary(p)
	case core.EventUpdateStarted:
		return updateStartedSummary(p)
	case core.EventUpdateFailed:
		return updateFailedSummary(p)
	case core.EventTaskTransition:
		if to := payloadStr(p, "to"); to != "" {
			return "task -> " + to
		}
		return "task transition"
	case core.EventInjected:
		if hook := payloadStr(p, "hook"); hook != "" {
			return hook + " injection"
		}
		return "context injected"
	case core.EventGardenerAction:
		return gardenerSummary(p)
	case core.EventToolCall:
		label := "tool " + payloadStr(p, "tool")
		if isErr, _ := p["is_error"].(bool); isErr {
			label += " failed"
		}
		return label
	case core.EventHookPrompt:
		return "prompt (no recall match)"
	case core.EventAgentMishap:
		return "mishap: " + snippet(payloadStr(p, "description"), 120)
	case core.EventPlanCaptured:
		s := "captured plan " + payloadStr(p, "basename")
		if it, ok := p["iteration"].(float64); ok {
			s += fmt.Sprintf(" (iter %d)", int(it))
		}
		return s
	case core.EventPlanPresented:
		return "presented plan " + payloadStr(p, "basename")
	case core.EventPlanApproved:
		return "approved plan " + payloadStr(p, "basename")
	case core.EventPlanShipped:
		line := "shipped plan " + payloadStr(p, "plan")
		if n := payloadInt(p, "steps"); n > 0 {
			line += " (" + plural(n, "step", "steps") + ")"
		}
		return line
	case core.EventSubagentCaptured:
		if at := payloadStr(p, "agent_type"); at != "" {
			return "cached subagent (" + at + ")"
		}
		return "cached subagent"
	default:
		return string(e.Kind)
	}
}

func payloadStr(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// payloadInt reads a numeric payload field as an int, 0 when absent: a live
// SSE event carries the in-process int, a row read back from the DB carries
// JSON's float64.
func payloadInt(p map[string]any, key string) int {
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// levelChangeSummary renders a level_changed payload as one ledger line. A
// payload missing its levels says less rather than inventing one.
func levelChangeSummary(p map[string]any) string {
	from, to := payloadStr(p, "from"), payloadStr(p, "to")
	if reset, _ := p["reset"].(bool); reset {
		if to != "" {
			return "console level reset to the file configuration (" + to + ")"
		}
		return "console level reset to the file configuration"
	}
	switch {
	case from != "" && to != "":
		return "console level " + from + " -> " + to
	case to != "":
		return "console level set to " + to
	default:
		return "console level changed"
	}
}

// featureStateSuffix renders a features_changed payload's per-feature state as
// " (research on, ... off)", in registry order so the line reads the same way
// every time. An empty or unreadable map contributes nothing rather than an
// invented state.
func featureStateSuffix(p map[string]any) string {
	state := payloadMap(p, "features")
	if len(state) == 0 {
		return ""
	}
	var parts []string
	for _, f := range features.Registry() {
		on, ok := state[string(f.Key)].(bool)
		if !ok {
			continue
		}
		word := "off"
		if on {
			word = "on"
		}
		parts = append(parts, string(f.Key)+" "+word)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// payloadMap reads a nested object field from a payload map (nil if absent).
func payloadMap(p map[string]any, key string) map[string]any {
	if p == nil {
		return nil
	}
	if v, ok := p[key].(map[string]any); ok {
		return v
	}
	return nil
}

// payloadList reads an array-of-objects field from a payload map (nil if absent),
// e.g. a consolidate proposal's "sources".
func payloadList(p map[string]any, key string) []map[string]any {
	if p == nil {
		return nil
	}
	raw, ok := p[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if obj, ok := v.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

// proposalWork names what a gardener proposal kind would do, as the object of
// "proposed" / "applied" (store.ProposalKinds). An unlisted kind is spelled out.
var proposalWork = map[string]string{
	store.ProposalMerge:        "merging duplicate memories",
	store.ProposalArchive:      "archiving a memory",
	store.ProposalDigest:       "a digest",
	store.ProposalConsolidate:  "consolidating memories",
	store.ProposalReproject:    "moving a memory to another project",
	store.ProposalSplit:        "splitting a project",
	store.ProposalAbandonPlan:  "settling a stale plan as abandoned",
	store.ProposalMemoryWanted: "filling a knowledge gap",
	store.ProposalToolError:    "fixing a recurring tool error",
	store.ProposalRekind:       "re-kinding a memory",
	store.ProposalShipPlan:     "settling a plan as shipped",
	store.ProposalRelocate:     "relocating a memory",
	store.ProposalMergePlans:   "merging duplicate plans",
}

// gardenerSummary says what the gardener did, in words: the ledger reads
// "proposed archiving a memory", not "gardener propose (archive)".
func gardenerSummary(p map[string]any) string {
	kind := payloadStr(p, "kind")
	work, ok := proposalWork[kind]
	if !ok {
		work = "a proposal"
		if kind != "" {
			work = "a " + strings.ReplaceAll(kind, "_", " ") + " proposal"
		}
	}
	switch action := payloadStr(p, "action"); action {
	case "propose":
		return "proposed " + work
	case "apply":
		return "applied " + work
	case "dismiss":
		return "dismissed a proposal"
	case "settle":
		return "settled a moot proposal: " + payloadStr(p, "reason")
	case "hide":
		return "hid a proposal forever"
	case "unhide":
		return "lifted a hidden proposal pattern"
	case "undo_apply":
		return "undid " + work
	case "undo_reject":
		return "reopened a decided proposal"
	case "request":
		if text := payloadStr(p, "text"); text != "" {
			return "took a request: " + text
		}
		return "took a cleanup request"
	case "split":
		if src := payloadStr(p, "source"); src != "" {
			return "planned a split of " + src
		}
		return "planned a project split"
	case "utility_armed":
		if proj := payloadStr(p, "project"); proj != "" {
			return "armed utility ranking for " + proj
		}
		return "armed utility ranking"
	case "digest_note":
		return "wrote digest " + payloadStr(p, "title")
	case "undo_digest_note":
		return "withdrew digest " + payloadStr(p, "title")
	case "":
		return "gardener activity"
	default:
		return "gardener " + strings.ReplaceAll(action, "_", " ")
	}
}
