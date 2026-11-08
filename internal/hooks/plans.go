package hooks

// Claude Code plan-mode capture. Plan mode writes its plan to
// ~/.claude/plans/<basename>.md with the ordinary Write/Edit tools, so the
// PostToolUse hook sees every iteration; ExitPlanMode's PostToolUse fires on
// user approval; PermissionRequest[ExitPlanMode] fires when the user is shown
// the plan. Each plan becomes one upserted note (slug cc-plan-<basename>) whose
// lifecycle rides on a plan-status:draft|presented|approved tag, plus verbatim
// plan.captured/presented/approved events. The first capture and the approval
// each tell the agent the composition slug they filed the plan under, so it
// composes steps into that plan rather than minting a second one for the same
// work. Everything here is best-effort and fail-open: a capture problem is
// logged and the hook still acks 200.
//
// This file is the hook entry points and the capture flow. The note upsert lives
// in plans_note.go, session correlation in plans_session.go, and the plan-file
// vocabulary in plans_stamp.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
)

// postToolUse captures plan-file iterations (Write/Edit/MultiEdit under the
// plans dir) and plan approvals (ExitPlanMode). The seam CLI pre-filters other
// tools locally, but the daemon still dispatches defensively. A session's
// first captured iteration may return related prior knowledge as
// additionalContext; every other outcome is a bare ack.
func (h *Handler) postToolUse(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, ok := requireRequestClient(w, r); !ok {
		return
	}
	p := h.decodeToolPayload(w, r)
	reach, lockedProject := h.sessionReachOf(r.Context(), ClientClaudeCode, p.SessionID)
	if reach != reachOK {
		p.SessionID = "" // foreignSession: another account's, or locked
	}

	// Heartbeat the ambient session on any tool activity, so a long turn that never
	// calls a seamless MCP tool still keeps its cc/* session live for the reaper.
	// Plan/tool-capture hooks are Claude Code-only (Codex registers none).
	hbCtx, hbCancel := context.WithTimeout(r.Context(), hookTimeout)
	h.touchAmbient(hbCtx, ClientClaudeCode, p.SessionID)
	hbCancel()

	// The tool name is checked BEFORE the machine check, so a remote agent's
	// ordinary edits do not each record a skip: only a call that would actually
	// have captured something is worth reporting as skipped.
	//
	// Under peer_auth the plan file is never read here: `seam hook` read it and
	// pushed its text (pushed.go), and the push lands only through the fences.
	var extra preparedHookContext
	switch p.ToolName {
	case "Write", "Edit", "MultiEdit":
		if h.planCaptureRuns(r.Context(), w, "plan-capture", &p, reach, lockedProject, &extra) {
			ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
			defer cancel()
			extra = h.capturePlanIteration(ctx, p)
		}
	case "ExitPlanMode":
		if h.planCaptureRuns(r.Context(), w, "plan-approval", &p, reach, lockedProject, &extra) {
			ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
			defer cancel()
			extra = h.capturePlanApproval(ctx, p)
		}
	}
	if extra.content != "" {
		// The SAME prepared value that recordInjection already logged: prepare
		// once, so response and telemetry cannot drift apart.
		writePreparedHookResponse(w, "PostToolUse", extra)
		return
	}
	writeHookAck(w)
}

// permissionRequest marks the session's draft plan as presented when the user
// is prompted to review an ExitPlanMode call. The payload carries no plan
// content; correlation is via the session's plan_capture metadata. Claude Code
// support for this hook is optional -- nothing downstream depends on it.
func (h *Handler) permissionRequest(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, ok := requireRequestClient(w, r); !ok {
		return
	}
	p := h.decodeToolPayload(w, r)
	reach, lockedProject := h.sessionReachOf(r.Context(), ClientClaudeCode, p.SessionID)
	if reach != reachOK {
		p.SessionID = "" // foreignSession: another account's, or locked
	}

	// Presenting carries no plan text: a refusal here has nothing to name, so
	// it is silent (nil: no notice).
	if p.ToolName == "ExitPlanMode" && h.planCaptureRuns(r.Context(), w, "plan-presented", &p, reach, lockedProject, nil) {
		ctx, cancel := context.WithTimeout(r.Context(), captureTimeout)
		defer cancel()
		h.markPlanPresented(ctx, p)
	}
	writeHookAck(w)
}

// captureEnabled reports whether plan/subagent capture can run at all.
func (h *Handler) captureEnabled() bool {
	return h.planCapture.Enabled && h.db != nil && h.files != nil
}

// captureLocal is captureEnabled plus the machine check: every capture path
// below re-reads the plan file, the transcript, or the repo's git HEAD from
// disk, and on another machine those paths are not this agent's. The skip is
// recorded (INFO) so a remote device's missing captures are visible rather than
// simply absent.
func (h *Handler) captureLocal(ctx context.Context, what string, id hookIdentity) bool {
	if !h.captureEnabled() {
		return false
	}
	return h.localDiskOK(ctx, what, ClientClaudeCode, h.hostOf(id))
}

// planCaptureRuns decides whether a plan capture runs: the daemon's own read
// of the plan file (captureLocal) when no capture was pushed -- always, with
// peer_auth off -- or, under peer_auth, what `seam hook` pushed, once the
// fences pass it (fenceNotePush), which also place it (p.Project). When notice
// is non-nil, a push refused at a lock sets the notice header and the notice
// as the hook's context; nil refuses silently.
func (h *Handler) planCaptureRuns(ctx context.Context, w http.ResponseWriter, what string, p *toolPayload, reach sessionReach, lockedProject string, notice *preparedHookContext) bool {
	if p.Capture == nil {
		return h.captureLocal(ctx, what, p.Identity)
	}
	if !h.captureEnabled() || (p.Capture.Plan == nil && p.ToolName != "ExitPlanMode") {
		return false // an iteration whose plan file the client could not read: nothing to capture
	}
	target := h.resolveProject(ctx, h.hostOf(p.Identity), p.CWD)
	project, verdict := h.fenceNotePush(ctx, ClientClaudeCode, reach, lockedProject, p.SessionID, target, "this plan")
	if verdict.notice != "" && notice != nil {
		h.refuseCapture(w, verdict.notice)
		*notice = refusalContext(verdict.notice)
	}
	p.Project = project
	return verdict.ok
}

// captureProject is the project a plan capture lands in: where the fences
// placed a pushed one (under peer_auth), else the one the hook's cwd resolves
// to, as always.
func (h *Handler) captureProject(ctx context.Context, p toolPayload) string {
	if p.Capture != nil {
		return p.Project
	}
	return h.resolveProject(ctx, h.hostOf(p.Identity), p.CWD)
}

// decodeToolPayload decodes a PostToolUse or PermissionRequest body: tolerant,
// the first JSON value, as these hooks always have. Under peer_auth it keeps
// the bytes for the pushed capture (pushed.go); with it off the body is
// streamed into the decoder exactly as before.
func (h *Handler) decodeToolPayload(w http.ResponseWriter, r *http.Request) toolPayload {
	r.Body = http.MaxBytesReader(w, r.Body, maxHookBody)
	var p toolPayload
	if _, on := access.FromContext(r.Context()); !on {
		_ = json.NewDecoder(r.Body).Decode(&p) //nolint:errcheck // tolerant: a decode error just leaves p zero (no tool name -> no capture)
		p.Identity = identityFromRequest(r)
		return p
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		body = nil // over the cap or unreadable: the zero payload, as a decode error leaves it
	}
	_ = json.NewDecoder(bytes.NewReader(body)).Decode(&p) //nolint:errcheck // tolerant, as above
	p.SessionID = namespacedExternalID(r, p.SessionID)
	p.Identity = identityFromRequest(r)
	p.Capture = h.pushedCapture(r.Context(), ClientClaudeCode, body)
	return p
}

// capturePlanIteration re-reads the just-written plan file from disk (the hook
// runs on the same machine, and re-reading is authoritative for Write and Edit
// alike) and upserts the cc-plan note. On the session's first captured
// iteration it returns the prepared related-prior-knowledge block for
// injection (the zero value in every other case).
func (h *Handler) capturePlanIteration(ctx context.Context, p toolPayload) preparedHookContext {
	if p.Capture != nil {
		// Pushed (peer_auth): the client read the file under its own plans dir.
		if p.Capture.Plan == nil || p.Capture.Plan.Content == "" {
			return preparedHookContext{}
		}
		return h.planIterationCaptured(ctx, p, p.Capture.Plan.Basename, p.Capture.Plan.Content)
	}
	var in struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.FilePath == "" {
		return preparedHookContext{}
	}
	path, ok := h.planFilePath(in.FilePath)
	if !ok {
		return preparedHookContext{} // not a plan file
	}
	content, err := os.ReadFile(path)
	if err != nil {
		h.logger.Warn("hooks: plan capture read", "error", err)
		return preparedHookContext{}
	}
	return h.planIterationCaptured(ctx, p, planBasename(path), string(content))
}

// planIterationCaptured upserts one plan iteration's note and, on the
// session's first, returns the related-knowledge context.
func (h *Handler) planIterationCaptured(ctx context.Context, p toolPayload, basename, content string) preparedHookContext {
	up, ok := h.upsertPlanNote(ctx, p, basename, content, false)
	if !ok || !up.first {
		return preparedHookContext{}
	}
	return h.planCaptureContext(ctx, p, up.note, up.planSlug, true)
}

// capturePlanApproval handles PostToolUse[ExitPlanMode]: the tool_response
// carries the plan file path (and sometimes the plan text); the file is
// re-read as the authoritative final text. On success the note flips to
// approved and, when configured, a tracking task is created for the plan.
// It returns the composition line for injection: approval is the moment the
// agent stops planning and starts calling tasks_add, so naming the slug here
// lands one tool call before the step it would otherwise misfile. No related
// lookup -- that belongs to the first capture, and this is not the agent's
// first sight of the plan.
func (h *Handler) capturePlanApproval(ctx context.Context, p toolPayload) preparedHookContext {
	var resp struct {
		Plan     string `json:"plan"`
		FilePath string `json:"filePath"`
	}
	_ = json.Unmarshal(p.ToolResponse, &resp) //nolint:errcheck // tolerant: absent fields stay zero

	basename, content := "", ""
	switch {
	case p.Capture != nil:
		// Pushed (peer_auth): the client resolved the file under its own plans
		// dir and read it ("" when it could not).
		if pl := p.Capture.Plan; pl != nil {
			basename, content = pl.Basename, pl.Content
		}
	case resp.FilePath != "":
		if path, ok := h.planFilePath(resp.FilePath); ok {
			basename = planBasename(path)
			if b, err := os.ReadFile(path); err == nil {
				content = string(b)
			} else {
				h.logger.Warn("hooks: plan approval read", "error", err)
			}
		}
	}
	if content == "" {
		content = resp.Plan
	}
	if basename == "" {
		// No usable filePath: correlate via the session's captured draft.
		if meta, ok := h.sessionPlanMeta(ctx, p.SessionID); ok {
			basename = meta.Basename
		}
	}
	if basename == "" {
		h.logger.Warn("hooks: plan approval without correlation", "claude_session_id", p.SessionID)
		return preparedHookContext{}
	}
	up, ok := h.upsertPlanNote(ctx, p, basename, content, true)
	if !ok {
		return preparedHookContext{}
	}
	if h.planCapture.AutoTask {
		h.ensurePlanTask(ctx, p, up.note, up.planSlug)
	}
	return h.planCaptureContext(ctx, p, up.note, up.planSlug, false)
}

// markPlanPresented flips the session's draft plan note to presented.
func (h *Handler) markPlanPresented(ctx context.Context, p toolPayload) {
	meta, ok := h.sessionPlanMeta(ctx, p.SessionID)
	if !ok || meta.Basename == "" || meta.Status != plans.StatusDraft {
		return
	}
	project := h.captureProject(ctx, p)
	path, found := h.noteFileBySlug(ctx, project, plans.NotePrefix+meta.Basename)
	if !found {
		return
	}
	// Retag under the file's lock. The status tag and the description are both
	// derived from what the note currently carries (NoteIteration reads its
	// tags), and the write renders the WHOLE file from that read -- so a plan
	// iteration landing between the two used to be erased by this status flip,
	// silently reverting the plan body to its previous version.
	written, err := h.files.MutateNote(ctx, path, func(_ context.Context, note core.Note) (core.Note, error) {
		note.Tags = plans.SetStatusTag(note.Tags, plans.StatusPresented)
		note.Description = plans.NoteDescription(meta.Basename, plans.NoteIteration(note), plans.StatusPresented)
		note.Updated = time.Now().UTC()
		return note, nil
	})
	if err != nil {
		h.logger.Warn("hooks: plan presented write", "error", err)
		return
	}
	meta.Status = plans.StatusPresented
	h.setSessionPlanMeta(ctx, p.SessionID, meta)
	h.recordPlanEvent(ctx, core.EventPlanPresented, p.SessionID, written.ID, map[string]any{
		"basename": meta.Basename, "plan_slug": meta.PlanSlug,
	})
}
