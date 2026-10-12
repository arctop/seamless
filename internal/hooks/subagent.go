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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
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

// Claude Code appends a subagent's final message to the transcript while
// SubagentStop is already in flight, so parsing the file the moment the hook
// fires reads it one message short: the capture stores the narration line
// before the report ("Now the console files.") as though it were the
// deliverable. 55 of this machine's first 80 captures landed that way, and the
// bimodal split -- one-line narration or a full report, nothing between --
// is the race, not truncation. A half-written trailing line reaches the same
// end by a second route, because the tolerant parser skips it and falls back to
// the previous assistant text. So wait for the transcript's terminal shape
// rather than trusting whatever is on disk at hook time. The budget fits inside
// captureTimeout with room for the note write, and an interrupted subagent --
// one that never emits a final message -- just spends it.
const (
	subagentReportSettle = 3 * time.Second
	subagentReportPoll   = 50 * time.Millisecond
)

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
	p := decodeSubagentStart(client, readHookBody(w, r))
	p.Identity = identityFromRequest(r)
	host := h.hostOf(p.Identity)

	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()

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
	// constraints, just without the prompt-matched RELEVANT section.
	prompt := ""
	if h.localDiskOK(ctx, "subagent-spawn-prompt", client, host) {
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
	p := decodeSubagentStop(client, readHookBody(w, r))
	p.Identity = identityFromRequest(r)

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

	if h.captureLocal(r.Context(), "subagent-transcript", p.Identity) {
		ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
		defer cancel()
		h.captureSubagent(ctx, p)
	}
	writeHookAck(w)
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
func (h *Handler) captureSubagent(ctx context.Context, p subagentPayload) {
	if p.AgentID == "" {
		return
	}
	meta, hasMeta := h.sessionPlanMeta(ctx, p.ParentSessionID)
	planning := hasMeta && (meta.Status == plans.StatusDraft || meta.Status == plans.StatusPresented)
	if !planning && p.PermissionMode != "plan" {
		return
	}
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
		return
	}
	prompt, report := h.awaitSubagentReport(ctx, path, p.AgentID)
	if prompt == "" && report == "" {
		h.logger.Warn("hooks: subagent transcript unreadable or empty",
			"agent_id", p.AgentID, "agent_type", p.AgentType, "path", path)
		return
	}

	project := h.resolveProject(ctx, h.hostOf(p.Identity), p.CWD)
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
			p.AgentID, h.gitHead(ctx, p.Identity, p.CWD), now,
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

// subagentTranscriptPath resolves the subagent's JSONL transcript: the
// agent_transcript_path Claude Code's SubagentStop names, else the payload
// transcript_path when it already names a subagents/agent-*.jsonl file, else
// constructed from the main transcript path and agent id (the verified layout:
// <proj-dir>/<session-id>/subagents/agent-<agent_id>.jsonl; SubagentStart
// carries no agent_transcript_path). An agent id carrying path separators or
// ".." is rejected rather than joined into the path -- it is a filename
// fragment, never a path.
func subagentTranscriptPath(p subagentPayload) string {
	if p.AgentTranscriptPath != "" {
		return p.AgentTranscriptPath
	}
	if p.TranscriptPath == "" {
		return ""
	}
	base := filepath.Base(p.TranscriptPath)
	if filepath.Base(filepath.Dir(p.TranscriptPath)) == "subagents" &&
		strings.HasPrefix(base, "agent-") && strings.HasSuffix(base, ".jsonl") {
		return p.TranscriptPath
	}
	if strings.ContainsAny(p.AgentID, `/\`) || strings.Contains(p.AgentID, "..") {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(p.TranscriptPath, ".jsonl"),
		"subagents", "agent-"+p.AgentID+".jsonl")
}

// awaitSubagentReport parses the transcript, waiting up to subagentReportSettle
// for the final assistant message to land. It returns the best values seen, so
// a transcript that never settles (an interrupted subagent) still yields its
// last narration rather than nothing -- that is the truest record available,
// and captureSubagent's own empty check decides whether it is worth a note.
func (h *Handler) awaitSubagentReport(ctx context.Context, path, agentID string) (prompt, report string) {
	prompt, report, complete := parseSubagentTranscript(path)
	if complete {
		return prompt, report
	}
	fi, err := os.Stat(path)
	if err != nil {
		return prompt, report // blank or absent path: nothing to wait for
	}
	size := fi.Size()

	settled := time.NewTimer(subagentReportSettle)
	defer settled.Stop()
	tick := time.NewTicker(subagentReportPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return prompt, report
		case <-settled.C:
			h.logger.Warn("hooks: subagent transcript never settled; capture may hold narration",
				"agent_id", agentID)
			return prompt, report
		case <-tick.C:
			// Re-parse only on growth: an interrupted subagent would otherwise
			// re-read a transcript that will never change once per tick.
			fi, err := os.Stat(path)
			if err != nil || fi.Size() == size {
				continue
			}
			size = fi.Size()
			p, r, done := parseSubagentTranscript(path)
			if p != "" {
				prompt = p
			}
			if r != "" {
				report = r
			}
			if done {
				return prompt, report
			}
		}
	}
}

// parseSubagentTranscript extracts the prompt (first user message) and final
// report (last assistant text) from a subagent transcript, and reports whether
// the transcript has reached its TERMINAL shape: a last line that parses, is an
// assistant message, carries text, and carries no tool_use. 371 of the 378
// subagent transcripts on the machine this was diagnosed on end exactly that
// way; the 7 that do not were interrupted mid-run. Anything else -- a trailing
// tool_use, a lone thinking block, a tool_result, a half-written line -- means
// the agent is still working or the writer is mid-flush, so the newest
// assistant text is narration rather than the report. Any problem yields empty
// strings and complete=false; capture is best-effort and never errors.
func parseSubagentTranscript(path string) (prompt, report string, complete bool) {
	if strings.TrimSpace(path) == "" {
		return "", "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// Same headroom as harvestFindings: transcript lines can be very large.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Every line clears the flag, so it describes the LAST one and not merely
		// some earlier line that happened to look final.
		complete = false
		var tl transcriptLine
		if err := json.Unmarshal(line, &tl); err != nil {
			continue // tolerant: skip malformed lines (a trailing one is a mid-flush write)
		}
		switch tl.Type {
		case "user":
			if prompt == "" {
				prompt = messageText(tl.Message.Content)
			}
		case "assistant":
			if txt := messageText(tl.Message.Content); txt != "" {
				report = txt
				complete = !hasToolUse(tl.Message.Content)
			}
		}
	}
	if sc.Err() != nil {
		complete = false // a truncated read cannot show which line is last
	}
	return strings.TrimSpace(prompt), strings.TrimSpace(report), complete
}

// hasToolUse reports whether a message's content carries a tool_use block, the
// marker that the turn continues. Text sitting beside a tool_use is the model
// narrating its next step, never its report. Content that is a plain string
// carries no blocks and so no tool_use.
func hasToolUse(raw json.RawMessage) bool {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	return slices.ContainsFunc(blocks, func(b contentBlock) bool { return b.Type == "tool_use" })
}
