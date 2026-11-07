package hooks

// Subagent lifecycle handling. SubagentStart fires for both clients and
// injects the same child briefing -- pinned constraints, up to three RELEVANT
// memories matched from the child's spawn prompt, and the recall/memory_read
// footer -- while sharing its parent's ambient session. Claude Code emits no
// SessionStart for Task subagents, so this is a CC child's only injection
// point.
// Codex SubagentStop is deliberately limited to a
// parent heartbeat: it never harvests child output into the parent's findings or
// creates durable notes. Claude Code's established planning-subagent capture is
// kept separate and unchanged: during plan-mode activity its transcript (first
// user message = prompt, last assistant text = final report, once the
// transcript has settled) is cached as a cc-agent-<agent_id> note in the
// session's plan composition.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/retrieve"
)

// maxAgentTitleRunes caps the prompt-derived part of an agent-cache note title.
const maxAgentTitleRunes = 120

// subagentStart injects the child briefing (project constraints plus
// spawn-prompt-matched RELEVANT memories) without running any ambient-session
// ensure/reactivation path. Both contracts (the captured Codex fixtures and
// Claude Code's documented SubagentStart payload) name session_id as the
// parent id, so a heartbeat is safe; model attribution is intentionally not
// updated because a child may run a different model.
func (h *Handler) subagentStart(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	body := readHookBody(w, r)
	p := decodeSubagentStart(client, body)
	p.Identity = identityFromRequest(r)
	host := h.hostOf(p.Identity)

	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	p.Capture = h.pushedCapture(ctx, client, body)
	if h.foreignSession(ctx, client, p.ParentSessionID) {
		p.ParentSessionID = ""
	}

	// The released contract requires these fields. A malformed request gets an
	// empty, correctly shaped response; never guess a parent, child, or scope.
	if strings.TrimSpace(p.ParentSessionID) == "" || strings.TrimSpace(p.AgentID) == "" ||
		strings.TrimSpace(p.AgentType) == "" || strings.TrimSpace(p.CWD) == "" {
		h.writeContextResponse(ctx, w, "SubagentStart", "subagent-start", client,
			p.ParentSessionID, "", "", false, nil)
		return
	}
	// The spawn prompt is read out of the child's transcript, so it is resolved
	// only for an agent on this machine; a remote child still gets the project's
	// constraints, just without the prompt-matched RELEVANT section. Under
	// peer_auth the transcript is never opened here: `seam hook` read it and
	// pushed the prompt (pushed.go).
	prompt := ""
	switch {
	case p.Capture != nil:
		prompt = p.Capture.Prompt
	case h.localDiskOK(ctx, "subagent-spawn-prompt", client, host):
		prompt = subagentSpawnPrompt(client, p)
	}
	briefing, injectedIDs, err := h.retrieve.Briefing(ctx, subagentBriefingInput(p, host, prompt))
	if err != nil {
		h.logger.Warn("hooks: subagent-start briefing failed", "error", err)
		briefing, injectedIDs = "", nil
	}

	// This is the only parent-session mutation: no ensure, project registration,
	// re-scope, rename, model update, findings harvest, or completion occurs here.
	h.touchAmbient(ctx, client, p.ParentSessionID)
	h.writeContextResponse(ctx, w, "SubagentStart", "subagent-start", client,
		p.ParentSessionID, "", briefing, briefing != "", injectedIDs)
}

func (h *Handler) subagentStop(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	body := readHookBody(w, r)
	p := decodeSubagentStop(client, body)
	p.Identity = identityFromRequest(r)
	p.Capture = h.pushedCapture(r.Context(), client, body)
	reach, lockedProject := h.sessionReachOf(r.Context(), client, p.ParentSessionID)
	if reach != reachOK {
		p.ParentSessionID = "" // foreignSession: another account's, or locked
	}

	if client == ClientCodex {
		// The Codex contract shares the parent session_id. Keep it alive, but do
		// not apply the child model/final message to the parent and do not enter
		// Claude's plan-note capture path.
		ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
		h.touchAmbient(ctx, client, p.ParentSessionID)
		cancel()
		writeHookAck(w)
		return
	}

	// Under peer_auth the transcript is never opened here: `seam hook` waited
	// for it to settle and pushed the prompt and report (pushed.go).
	if h.subagentCaptureRuns(r.Context(), p) {
		ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
		defer cancel()
		if notice := h.captureSubagent(ctx, p, reach, lockedProject); notice != "" {
			h.refuseCapture(w, notice)
		}
	}
	writeHookAck(w)
}

// subagentCaptureRuns decides whether a subagent capture runs at all: the
// daemon's own read of the transcript (captureLocal) when nothing was pushed --
// always, with peer_auth off -- or, under peer_auth, the push (whose fences
// captureSubagent applies).
func (h *Handler) subagentCaptureRuns(ctx context.Context, p subagentPayload) bool {
	if p.Capture != nil {
		return h.captureEnabled()
	}
	return h.captureLocal(ctx, "subagent-transcript", p.Identity)
}

// errAgentCaptureDropped is the fail-open signal out of the locked upsert, the
// twin of errPlanCaptureDropped: this capture cannot be recorded (no id, the
// write failed), so the hook stays silent rather than failing the agent's tool
// call. It never leaves captureSubagent.
var errAgentCaptureDropped = errors.New("agent capture dropped")

// captureSubagent caches a completed Claude Code subagent's prompt and report as
// a note.
// Gate: only while the session has an unapproved plan capture or is in plan
// mode -- otherwise every subagent machine-wide would produce a note.
//
// Under peer_auth the prompt and report are the ones `seam hook` pushed, and
// they land only once the fences pass them (fenceNotePush, judged with the
// parent session's reach); the notice for one refused at a lock is returned
// for the response. It is "" in every other case.
func (h *Handler) captureSubagent(ctx context.Context, p subagentPayload, reach sessionReach, lockedProject string) (notice string) {
	if p.AgentID == "" {
		return ""
	}
	meta, hasMeta := h.sessionPlanMeta(ctx, p.ParentSessionID)
	planning := hasMeta && (meta.Status == plans.StatusDraft || meta.Status == plans.StatusPresented)
	if !planning && p.PermissionMode != "plan" {
		return ""
	}
	if p.Capture == nil {
		prompt, report, ok := h.subagentReportFromDisk(ctx, p)
		if ok {
			h.writeSubagentNote(ctx, p, meta, h.resolveProject(ctx, h.hostOf(p.Identity), p.CWD), prompt, report)
		}
		return ""
	}
	if p.Capture.Prompt == "" && p.Capture.Report == "" {
		return "" // the client found no transcript, or nothing in it
	}
	target := h.resolveProject(ctx, h.hostOf(p.Identity), p.CWD)
	project, verdict := h.fenceNotePush(ctx, ClientClaudeCode, reach, lockedProject, p.ParentSessionID, target, "this subagent's report")
	if !verdict.ok {
		return verdict.notice
	}
	h.writeSubagentNote(ctx, p, meta, project, p.Capture.Prompt, p.Capture.Report)
	return ""
}

// subagentReportFromDisk reads a subagent's prompt and report out of its
// transcript on this machine, waiting for the report to settle. ok is false
// when there is nothing to capture.
func (h *Handler) subagentReportFromDisk(ctx context.Context, p subagentPayload) (prompt, report string, ok bool) {
	path := subagentTranscriptPath(p)
	// Claude Code also fires SubagentStop for agents that never write a
	// transcript: no SubagentStart, no file anywhere, at turn cadence for as
	// long as a plan stays unapproved (204 of them across two sessions on the
	// machine this was diagnosed on). There is nothing to capture, and a WARN per
	// stop buried the log, so only a transcript that exists and yields nothing
	// earns a warning.
	if _, err := os.Stat(path); path != "" && errors.Is(err, fs.ErrNotExist) {
		h.logger.Debug("hooks: subagent left no transcript; nothing to capture",
			"agent_id", p.AgentID, "agent_type", p.AgentType)
		return "", "", false
	}
	prompt, report = h.awaitSubagentReport(ctx, path, p.AgentID)
	if prompt == "" && report == "" {
		h.logger.Warn("hooks: subagent transcript unreadable or empty",
			"agent_id", p.AgentID, "agent_type", p.AgentType, "path", path)
		return "", "", false
	}
	return prompt, report, true
}

// writeSubagentNote upserts the cc-agent-<agent_id> note for one subagent run
// in project, parks it on the session when no plan slug exists yet, and
// records the capture.
func (h *Handler) writeSubagentNote(ctx context.Context, p subagentPayload, meta planCaptureMeta, project, prompt, report string) {
	noteSlug := plans.AgentNotePrefix + core.Slugify(p.AgentID)
	now := time.Now().UTC()

	// Create-or-update under the note's lock, the same shape upsertPlanNote uses:
	// the slug is deterministic, so the path can be locked before it is known
	// whether the note exists. The read has to be inside because it decides
	// identity -- an existing note keeps its id and created time -- and because
	// the write renders the whole file: two subagents of one session finishing at
	// once, or an adoption retagging the note between the read and the write,
	// used to lose one of the two with no error anywhere.
	var written core.Note
	err := h.files.Mutate(ctx, files.NoteRelPath(project, noteSlug), func(ctx context.Context) error {
		note, found := h.loadNoteBySlug(ctx, project, noteSlug)
		if !found {
			id, err := core.NewID()
			if err != nil {
				h.logger.Warn("hooks: agent note id", "error", err)
				return errAgentCaptureDropped
			}
			note = core.Note{ID: id, Slug: noteSlug, Project: project, Created: now}
		}
		note.Title = agentNoteTitle(p.AgentType, prompt)
		note.Description = fmt.Sprintf("Cached planning-subagent run (%s) -- prompt + final report", p.AgentType)
		note.Body = agentStamp(
			h.ambientDisplayName(ctx, ClientClaudeCode, p.ParentSessionID),
			p.AgentID, h.gitHead(ctx, p.Identity, p.CWD, p.Capture), now,
		) +
			"\n\n## Prompt\n\n" + prompt + "\n\n## Report\n\n" + report
		note.Tags = agentNoteTags(meta.PlanSlug, p.AgentType)
		note.Updated = now

		w, werr := h.files.WriteNote(ctx, note)
		if werr != nil {
			h.logger.Warn("hooks: agent note write", "slug", noteSlug, "error", werr)
			return errAgentCaptureDropped
		}
		written = w
		return nil
	})
	if err != nil {
		return // already logged inside; the hook stays silent rather than failing the tool call
	}
	// No plan slug yet (the explore-first pattern: subagents finish before the
	// first plan-file write): park the note slug on the session so the first
	// plan capture adopts it into the composition.
	if meta.PlanSlug == "" && !slices.Contains(meta.PendingAgents, noteSlug) {
		meta.PendingAgents = append(meta.PendingAgents, noteSlug)
		h.setSessionPlanMeta(ctx, p.ParentSessionID, meta)
	}
	h.recordPlanEvent(ctx, core.EventSubagentCaptured, p.ParentSessionID, written.ID, map[string]any{
		"content":  report, // verbatim, unbounded by design
		"prompt":   events.Truncate(prompt, h.maxEventChars),
		"agent_id": p.AgentID, "agent_type": p.AgentType, "plan_slug": meta.PlanSlug,
	})
}

// agentNoteTags composes the agent-cache note's tag set; the plan tag is
// omitted when the session has no correlated plan (pure plan-mode gate).
func agentNoteTags(planSlug, agentType string) []string {
	tags := make([]string, 0, 4)
	if planSlug != "" {
		tags = append(tags, plans.SlugTag(planSlug))
	}
	tags = append(tags, plans.TagAgent)
	if agentType != "" {
		tags = append(tags, "agent:"+agentType)
	}
	return append(tags, "created-by:agent")
}

// agentNoteTitle is "[<type>] <first prompt line>", capped.
func agentNoteTitle(agentType, prompt string) string {
	line := prompt
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = "subagent run"
	}
	line = capRunes(line, maxAgentTitleRunes)
	if agentType == "" {
		return line
	}
	return "[" + agentType + "] " + line
}

// agentStamp is the provenance blockquote prepended to an agent-cache note.
func agentStamp(sessionName, agentID, head string, now time.Time) string {
	return fmt.Sprintf("> captured from %s | agent %s | git %s | %s",
		stampSession(sessionName), agentID, shortHead(head), now.UTC().Format(time.RFC3339))
}

// subagentBriefingInput builds the child-briefing input from a SubagentStart
// payload, carrying the spawn prompt when it can be resolved. The briefing
// matches the prompt against the project's memories and renders the hits as
// its RELEVANT section; an unresolved (empty) prompt just means no section.
func subagentBriefingInput(p subagentPayload, host, prompt string) retrieve.BriefingInput {
	return retrieve.BriefingInput{
		CWD:       p.CWD,
		Host:      host,
		AgentType: p.AgentType,
		Prompt:    prompt,
	}
}

// subagentSpawnPrompt best-effort resolves the child's spawn prompt at
// SubagentStart, per client: Claude Code = the first user message of the child
// transcript (which may not exist yet when the hook fires); Codex = the first
// user event of the child rollout that the Start payload's transcript_path
// names (Stop differs: there transcript_path is the parent rollout and
// agent_transcript_path the child). Every failure mode -- absent, empty, or
// unparseable file, prompt not yet flushed, oversized line -- yields ""
// silently; reads are size-bounded and never turn the hook response into an
// error.
func subagentSpawnPrompt(client Client, p subagentPayload) string {
	switch client {
	case ClientClaudeCode:
		// No settle wait here: at SubagentStart the child has produced nothing to
		// settle to, and the prompt is the FIRST line rather than the last.
		prompt, _, _ := parseSubagentTranscript(subagentTranscriptPath(p))
		return prompt
	case ClientCodex:
		return headCodexRollout(p.TranscriptPath)
	}
	return ""
}
