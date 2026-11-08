package hooks

// The cc-plan note upsert: one note per plan file, carrying the plan across its
// draft -> presented -> approved lifecycle without losing the composition slug,
// the owner's tags, or the iteration count.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/validate"
)

// errPlanCaptureDropped is the fail-open signal out of the locked upsert: this
// capture cannot be recorded (nothing to write, no id, the write failed), so the
// hook stays silent rather than failing the agent's tool call. It never leaves
// upsertPlanNote -- the caller sees the same "not captured" it always did.
var errPlanCaptureDropped = errors.New("plan capture dropped")

// planUpsert is upsertPlanNote's result: the written note, its composition
// slug, and whether this was the session's first plan capture (no plan_capture
// metadata existed yet) -- the trigger for the related-knowledge injection.
type planUpsert struct {
	note     core.Note
	planSlug string
	first    bool
}

// upsertPlanNote creates or updates the cc-plan-<basename> note for one plan
// iteration or approval. On update the note id, created time, plan:<slug> tag
// (the composition slug is minted once, at first capture), and any tags the
// owner or another agent added are preserved; the title, body, and the managed
// tags follow the latest content. An approval with
// no readable content flips the status of an existing note without touching
// its body; with no existing note it is dropped (fail-open). Agent-cache notes
// captured before the plan existed (pending on the session) are adopted into
// the composition here, once the slug is minted.
func (h *Handler) upsertPlanNote(ctx context.Context, p toolPayload, basename, content string, approve bool) (planUpsert, bool) {
	project := h.captureProject(ctx, p)
	noteSlug := plans.NotePrefix + basename
	trimmed := strings.TrimSpace(content)

	var written core.Note
	var planSlug, status string
	var iter int
	// A capture is a read-modify-write of one note file, and its racers are the
	// ordinary case rather than the pathological one: successive plan-file saves
	// fire this hook back to back, and an approval can land while the previous
	// save is still being written. Unserialized, both sides rendered the whole
	// file from the same pre-capture note and the second rename erased the first
	// -- iteration count, composition slug, owner tags and all -- with no error
	// anywhere. The lock spans the lookup, the read and the write; the session
	// metadata, the agent adoption and the event below are DB work on other rows
	// and stay outside it.
	err := h.files.Mutate(ctx, files.NoteRelPath(project, noteSlug), func(ctx context.Context) error {
		existing, found := h.loadNoteBySlug(ctx, project, noteSlug)
		if trimmed == "" && !(approve && found) {
			return errPlanCaptureDropped
		}

		now := time.Now().UTC()
		note := existing
		iter = 1
		if found {
			iter = plans.NoteIteration(existing)
			if !approve && trimmed != "" {
				iter++
			}
		} else {
			id, err := core.NewID()
			if err != nil {
				h.logger.Warn("hooks: plan note id", "error", err)
				return errPlanCaptureDropped
			}
			note = core.Note{ID: id, Slug: noteSlug, Project: project, Created: now}
		}

		status = plans.StatusDraft
		if found && plans.StatusFromTags(existing.Tags) == plans.StatusApproved {
			status = plans.StatusApproved // an approved plan never regresses to draft
		}
		if approve {
			status = plans.StatusApproved
		}

		planSlug = plans.SlugFromTags(note.Tags)
		if trimmed != "" {
			title := firstHeading(content)
			if title == "" || validate.Title(title) != nil {
				title = basename
			}
			note.Title = title
			note.Body = planStamp(
				h.ambientDisplayName(ctx, ClientClaudeCode, p.SessionID),
				basename, iter, h.gitHead(ctx, p.Identity, p.CWD, p.Capture), now,
			) + "\n\n" + content
			// New plan content is attributed to the capturing session's model; an
			// unknown model keeps the note's prior attribution.
			if m := h.ambientModel(ctx, ClientClaudeCode, p.SessionID); m != "" {
				note.Model = m
			}
			// The account label follows the content the way the model does,
			// except that it is always re-stamped: a restricted account's
			// capture is labeled with it, and the owner's own capture over
			// that content clears it. A status flip below (an approval with no
			// text, a retag) is metadata and keeps it.
			note.Account = access.AccountOf(ctx)
		}
		if planSlug == "" {
			planSlug = core.Slugify(note.Title)
		}
		note.Description = plans.NoteDescription(basename, iter, status)
		note.Tags = plans.SetStatusTag(mergePlanTags(note.Tags, planSlug), status)
		note.Updated = now
		if note.Extra == nil {
			note.Extra = map[string]any{}
		}
		note.Extra["plan_iteration"] = iter

		w, werr := h.files.WriteNote(ctx, note)
		if werr != nil {
			h.logger.Warn("hooks: plan note write", "slug", noteSlug, "error", werr)
			return errPlanCaptureDropped
		}
		written = w
		return nil
	})
	if err != nil {
		// Every dropped capture already logged its own reason (or is a deliberate
		// silent skip); anything else is the lock itself failing -- a cancelled
		// request or a path that cannot be locked -- which nothing downstream has
		// reported yet.
		if !errors.Is(err, errPlanCaptureDropped) {
			h.logger.Warn("hooks: plan note lock", "slug", noteSlug, "error", err)
		}
		return planUpsert{}, false
	}

	// Adopt agent-cache notes that completed before this plan existed: the
	// session accrued their slugs while no plan slug was known; tag them into
	// the now-minted composition and clear the pending list (the fresh meta
	// below carries none).
	prior, _ := h.sessionPlanMeta(ctx, p.SessionID)
	adopted := h.adoptPendingAgents(ctx, project, planSlug, prior.PendingAgents)
	h.setSessionPlanMeta(ctx, p.SessionID, planCaptureMeta{Basename: basename, PlanSlug: planSlug, Status: status})

	kind := core.EventPlanCaptured
	if approve {
		kind = core.EventPlanApproved
	}
	payload := map[string]any{
		"basename": basename, "plan_slug": planSlug, "iteration": iter,
		"title": events.Truncate(written.Title, h.maxEventChars),
	}
	if trimmed != "" {
		payload["content"] = content // verbatim, unbounded by design
	}
	if adopted > 0 {
		payload["adopted_agents"] = adopted
	}
	h.recordPlanEvent(ctx, kind, p.SessionID, written.ID, payload)
	return planUpsert{note: written, planSlug: planSlug, first: prior.Basename == ""}, true
}

// mergePlanTags rebuilds a captured-plan note's tag set for an upsert. The
// hook-managed tags stay authoritative and deduplicated -- plan:<slug> (the
// composition tag, replacing any other plan:* tag), cc-plan, and
// created-by:agent -- while every other tag already on the note (owner- or
// agent-added) is preserved in order, so a re-captured iteration never wipes
// them. The plan-status:* tag is managed separately via plans.SetStatusTag.
func mergePlanTags(existing []string, planSlug string) []string {
	managed := []string{plans.SlugTag(planSlug), plans.TagPlan, "created-by:agent"}
	out := make([]string, 0, len(managed)+len(existing))
	out = append(out, managed...)
	for _, t := range existing {
		if strings.HasPrefix(t, plans.SlugTagPrefix()) || slices.Contains(managed, t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// adoptPendingAgents adds the plan:<slug> tag to agent-cache notes captured
// before the plan's first iteration (the explore-first pattern: subagents
// finish before any plan file exists). Best-effort per note; one that vanished
// or already belongs to a plan is skipped. Returns how many were adopted.
func (h *Handler) adoptPendingAgents(ctx context.Context, project, planSlug string, slugs []string) int {
	adopted := 0
	for _, slug := range slugs {
		path, found := h.noteFileBySlug(ctx, project, slug)
		if !found {
			continue
		}
		// captureSubagent writes these same files, so the tag goes on under the
		// note's lock: the write renders the whole note, and a subagent report
		// landing between an unlocked read and this write would be erased by it.
		// A note that already belongs to a composition reports ErrNoChange, which
		// skips the write rather than re-rendering an unchanged file -- and is why
		// the adoption count is taken from the callback rather than from err.
		tagged := false
		if _, err := h.files.MutateNote(ctx, path, func(_ context.Context, note core.Note) (core.Note, error) {
			if plans.SlugFromTags(note.Tags) != "" {
				return note, files.ErrNoChange
			}
			note.Tags = append([]string{plans.SlugTag(planSlug)}, note.Tags...)
			note.Updated = time.Now().UTC()
			tagged = true
			return note, nil
		}); err != nil {
			h.logger.Warn("hooks: adopt pending agent note", "slug", slug, "error", err)
			continue
		}
		if tagged {
			adopted++
		}
	}
	return adopted
}

// ensurePlanTask creates the "Implement plan" tracking task for an approved
// plan unless the plan already has an open or in-progress step (idempotent on
// re-approval).
func (h *Handler) ensurePlanTask(ctx context.Context, p toolPayload, note core.Note, planSlug string) {
	createdBy := ""
	if p.SessionID != "" {
		// Plan capture is Claude Code-only (Codex registers no plan-capture hooks).
		createdBy = h.ambientDisplayName(ctx, ClientClaudeCode, p.SessionID)
	}
	task, created, err := plans.EnsureTask(ctx, h.db, note, planSlug, createdBy)
	if err != nil {
		h.logger.Warn("hooks: plan task", "error", err)
		return
	}
	if !created {
		return
	}
	h.recordPlanEvent(ctx, core.EventTaskTransition, p.SessionID, task.ID, map[string]any{
		"to": string(core.TaskOpen), "created": true, "plan_slug": planSlug,
	})
}

// relatedPlanHits caps how many recall hits the first-capture injection lists.
const relatedPlanHits = 5

// planCompositionLine names the composition a captured plan was filed under.
//
// Without it the agent never learns the slug upsertPlanNote just minted from
// its own H1: the `PLAN: <slug>` briefing line appears only in the NEXT
// session, long after this one has invented a second slug for the same work via
// tasks_add. Approval used to paper the gap over -- ensurePlanTask wires the
// tracking task onto the captured slug -- which is exactly why the stranded
// captures are concentrated in the plans that were presented and never
// approved: no approval, no wiring, and the capture is left with no steps while
// the real steps accrue under a slug it has never heard of.
func planCompositionLine(planSlug string) string {
	return "Seamless filed this plan as composition plan:" + planSlug +
		". Attach its steps with tasks_add plan=" + planSlug + " and supporting notes with the" +
		" tag plan:" + planSlug + " -- do not mint a second slug for this work."
}

// planCaptureContext builds the additionalContext returned to a capturing
// agent. The composition line is always present: it is mechanism rather than
// retrieval, so it is gated only by capture being on at all, not by
// InjectRelated (which the owner sets to control prior-knowledge lookups).
// The related-knowledge list is appended when that IS on and recall found
// something beyond the plan's own note.
func (h *Handler) planCaptureContext(ctx context.Context, p toolPayload, note core.Note, planSlug string, lookUpRelated bool) preparedHookContext {
	var b strings.Builder
	b.WriteString("<seam-plan-context>\n")
	b.WriteString(planCompositionLine(planSlug))

	var ids []string
	if lookUpRelated && h.planCapture.InjectRelated {
		var related string
		related, ids = h.relatedPlanKnowledge(ctx, note)
		if related != "" {
			b.WriteString("\nSeamless has prior knowledge related to this plan; check before finalizing:")
			b.WriteString(related)
		}
	}
	b.WriteString("\n</seam-plan-context>")

	// Plan capture is Claude Code-only (Codex registers no plan-capture hooks).
	prepared := prepareHookContext(ClientClaudeCode, b.String())
	// Only the recall half is a retrieval. With no hits there are no item ids to
	// attribute, and recording an injection anyway would add an event to the
	// injected-then-read funnel that surfaced nothing to read. When there ARE
	// hits, telemetry and response consume the same prepared value, so the two
	// cannot drift apart.
	if len(ids) > 0 {
		h.recordInjection(ctx, "post-tool-use", ClientClaudeCode, p.SessionID, "", prepared, ids)
	}
	return prepared
}

// relatedPlanKnowledge renders the top recall hits for a plan's title (prior
// plans, constraints, related notes) so the planning agent sees prior art
// before the plan is finalized, with the ids for injection telemetry. Both are
// empty when recall is unavailable, errors, or finds nothing beyond the plan's
// own note.
func (h *Handler) relatedPlanKnowledge(ctx context.Context, note core.Note) (string, []string) {
	if h.retrieve == nil || strings.TrimSpace(note.Title) == "" {
		return "", nil
	}
	hits, err := h.retrieve.Recall(ctx, retrieve.RecallInput{
		Query: note.Title, Project: note.Project, Limit: relatedPlanHits + 1,
	})
	if err != nil {
		h.logger.Warn("hooks: related plan recall", "error", err)
		return "", nil
	}
	var b strings.Builder
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.ID == note.ID {
			continue // the plan's own freshly-written note
		}
		if len(ids) == relatedPlanHits {
			break
		}
		read := "memory_read name=" + hit.Name
		if hit.Kind == "note" {
			read = "notes_read id=" + hit.ID
		}
		fmt.Fprintf(&b, "\n- [%s] %s (%s): %s -- %s", hit.Kind, hit.Title, hit.Age, hit.Description, read)
		ids = append(ids, hit.ID)
	}
	if len(ids) == 0 {
		return "", nil
	}
	return b.String(), ids
}

// recordPlanEvent appends a plan-capture event, attributed to the ambient
// session (best-effort) with the Claude session id riding in the payload.
func (h *Handler) recordPlanEvent(ctx context.Context, kind core.EventKind, claudeSessionID, itemID string, payload map[string]any) {
	if h.events == nil {
		return
	}
	// Plan capture is Claude Code-only (Codex registers no plan-capture hooks).
	sessionID, project := h.ambientRef(ctx, ClientClaudeCode, claudeSessionID)
	payload["claude_session_id"] = claudeSessionID
	payload["external_client"] = ClientClaudeCode.externalIdentity()
	if _, err := h.events.Record(ctx, core.Event{
		Kind: kind, SessionID: sessionID, ProjectSlug: project, ItemID: itemID, Payload: payload,
	}); err != nil {
		h.logger.Warn("hooks: record plan event", "kind", kind, "error", err)
	}
}
