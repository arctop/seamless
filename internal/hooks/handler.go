// Package hooks serves the Claude Code SessionStart and UserPromptSubmit hook
// endpoints and installs/removes their entries in a settings.json. Both handlers
// authenticate the same static bearer key as MCP, and both fail open: any
// internal error yields a 200 with empty additionalContext so a broken briefing
// can never block an agent. Only a bad key (401) or an unknown ?client=
// discriminator (400, an install bug rather than a runtime condition) returns
// non-2xx.
package hooks

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gitread"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// hookTimeout bounds briefing/recall assembly so a slow store never stalls the
// agent's turn.
const hookTimeout = 2 * time.Second

// captureTimeout bounds the plan/subagent capture endpoints. They read files
// and write notes (which may embed synchronously), so they get more headroom
// than the briefing path; the installed hook timeout is 10s.
const captureTimeout = 8 * time.Second

// maxHookBody caps every hook request body. PostToolUse payloads carry the
// full tool_input (a plan-file Write includes the whole file), so the cap is
// generous; anything larger is not a payload Seamless should buffer.
const maxHookBody = 8 << 20

// ambientPrefixLen is how many leading chars of the client's external session id
// remain visible in an ambient display name. Identity uses the full id + client;
// the display name adds a stable digest suffix to remain collision-resistant.
const ambientPrefixLen = 8

// ambientDigestBytes contributes 64 stable digest bits to a display name. The
// full external id remains the authoritative store key; this suffix prevents the
// UNIQUE human handle from reintroducing the old truncated-prefix collision.
const ambientDigestBytes = 8

// Config carries the Handler's dependencies. DB backs ambient sessions and the
// session-end harvest; Events may be nil (injection telemetry is then skipped);
// Files may be nil (plan/subagent capture is then skipped). MaxEventChars caps
// captured prompt/findings text (0 = unlimited); injected content is always
// stored in full (it is bounded by the client-aware context policy upstream).
// PlansDir is where Claude Code writes plan-mode files; empty defaults to
// ~/.claude/plans (tests override it). LocalHost is this daemon's own machine
// name (config.Hostname): every capture that reads the agent's filesystem is
// gated on the hook's host matching it, so a shared daemon never opens its own
// copy of a remote agent's path. Empty means "this machine has not named
// itself", which matches every hook that sends no host.
type Config struct {
	DB            *sql.DB
	Retrieve      *retrieve.Service
	Events        *events.Recorder
	Files         *files.Manager
	APIKey        string
	MaxEventChars int
	PlanCapture   config.PlanCapture
	PlansDir      string
	LocalHost     string
	Logger        *slog.Logger
}

// Handler serves the hook endpoints.
type Handler struct {
	db            *sql.DB
	retrieve      *retrieve.Service
	events        *events.Recorder
	files         *files.Manager
	apiKey        string
	maxEventChars int
	planCapture   config.PlanCapture
	plansDir      string
	localHost     string
	logger        *slog.Logger
}

// NewHandler builds a hook Handler from cfg.
func NewHandler(cfg Config) *Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PlansDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.PlansDir = filepath.Join(home, ".claude", "plans")
		}
	}
	return &Handler{
		db: cfg.DB, retrieve: cfg.Retrieve, events: cfg.Events, files: cfg.Files,
		apiKey: cfg.APIKey, maxEventChars: cfg.MaxEventChars,
		planCapture: cfg.PlanCapture, plansDir: cfg.PlansDir,
		localHost: strings.ToLower(strings.TrimSpace(cfg.LocalHost)), logger: cfg.Logger,
	}
}

// Register mounts the hook routes on mux at their full /api/hooks/* paths.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/hooks/session-start", h.sessionStart)
	mux.HandleFunc("POST /api/hooks/user-prompt-submit", h.userPromptSubmit)
	mux.HandleFunc("POST /api/hooks/session-end", h.sessionEnd)
	mux.HandleFunc("POST /api/hooks/stop", h.stop)
	mux.HandleFunc("POST /api/hooks/subagent-start", h.subagentStart)
	mux.HandleFunc("POST /api/hooks/post-tool-use", h.postToolUse)
	mux.HandleFunc("POST /api/hooks/subagent-stop", h.subagentStop)
	mux.HandleFunc("POST /api/hooks/permission-request", h.permissionRequest)
}

// hookPayload is the SessionStart request body (tolerant decode; unknown fields
// ignored, empty body OK). The client discriminator is not a body field: it rides
// on the request as the ?client= query param (see adapter.go).
type hookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Source         string `json:"source"`     // startup|resume|clear|compact
	AgentType      string `json:"agent_type"` // non-empty => subagent
	Model          string `json:"model"`      // Codex sends it; Claude Code does not (see setAmbientModel)
	// Identity travels on the query string, not in the body (see adapter.go);
	// the handler copies it in so everything downstream reads one payload.
	Identity hookIdentity `json:"-"`
}

type promptPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	UserPrompt     string `json:"user_prompt"` // Codex names this `prompt`; decodePrompt normalizes it
	HookEventName  string `json:"hook_event_name"`
	Model          string `json:"model"` // Codex sends it; Claude Code does not (see setAmbientModel)
	// Host is resolved by the handler, not the client: UserPromptSubmit stays an
	// http hook, so no seam process computes an identity for it. The daemon
	// attributes it through the ambient session it already owns.
	Host string `json:"-"`
}

// endPayload is the SessionEnd request body.
type endPayload struct {
	SessionID      string       `json:"session_id"`
	TranscriptPath string       `json:"transcript_path"`
	Reason         string       `json:"reason"` // clear|logout|prompt_input_exit|other
	Identity       hookIdentity `json:"-"`
}

// stopPayload is the Codex Stop request body. Stop fires at every turn end and,
// for Codex, stands in for the missing SessionEnd: it carries the turn's last
// agent message directly (LastAssistantMessage), with transcript_path as the
// rollout-file fallback when it is absent.
type stopPayload struct {
	SessionID            string       `json:"session_id"`
	CWD                  string       `json:"cwd"`
	TranscriptPath       string       `json:"transcript_path"`
	LastAssistantMessage string       `json:"last_assistant_message"`
	StopHookActive       bool         `json:"stop_hook_active"`
	Model                string       `json:"model"` // Codex sends it; Claude Code does not (see setAmbientModel)
	Identity             hookIdentity `json:"-"`
}

// toolPayload is the tolerant shape of the PostToolUse and PermissionRequest
// request bodies. ToolInput/ToolResponse stay raw: their shape is per-tool, and
// only the plan-capture paths decode the fields they need.
type toolPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"` // plan|default|acceptEdits|...
	ToolName       string          `json:"tool_name"`
	HookEventName  string          `json:"hook_event_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	Identity       hookIdentity    `json:"-"`
}

// subagentPayload is the normalized SubagentStart/SubagentStop shape. Both
// clients name session_id as the PARENT session on both events; the child is
// identified separately by agent_id. Both clients' SubagentStop distinguishes the
// parent transcript (transcript_path) from the child's (agent_transcript_path);
// Claude Code's smaller payload leaves the Codex-only fields (turn_id, model)
// empty, preserving its plan-capture path.
type subagentPayload struct {
	ParentSessionID      string       `json:"session_id"`
	TurnID               string       `json:"turn_id"`
	AgentID              string       `json:"agent_id"`
	AgentType            string       `json:"agent_type"`
	CWD                  string       `json:"cwd"`
	Model                string       `json:"model"`
	PermissionMode       string       `json:"permission_mode"`
	HookEventName        string       `json:"hook_event_name"`
	TranscriptPath       string       `json:"transcript_path"`
	AgentTranscriptPath  string       `json:"agent_transcript_path"`
	LastAssistantMessage string       `json:"last_assistant_message"`
	StopHookActive       bool         `json:"stop_hook_active"`
	Identity             hookIdentity `json:"-"`
}

// hookResponse is the Claude Code hook response envelope; the field names are
// load-bearing and case-sensitive.
type hookResponse struct {
	Continue           bool                `json:"continue"`
	SuppressOutput     bool                `json:"suppressOutput"`
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func (h *Handler) sessionStart(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	p := decodeSessionStart(client, readHookBody(w, r))
	p.Identity = identityFromRequest(r)
	host := h.hostOf(p.Identity)

	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	if h.foreignSession(ctx, client, p.SessionID) {
		p.SessionID = ""
	}

	// Grow the repo->project map when this agent is working in a not-yet-mapped
	// repo, so the briefing below (and later tool calls) resolve to a real
	// project. Best-effort by the package's never-block-the-agent contract: on a
	// failure the mapping is simply not written, and every later cwd resolution
	// falls back to the global scope. That fallback is not desirable, only
	// preferable to failing session start, so it is logged rather than hidden --
	// the explicit session_start tool resolves the same cwd and does surface the
	// error, and that is the path where a wrong binding actually sticks.
	//
	// A remote agent that sent no repo root is the one case with its own stage:
	// nothing was written and nothing could be, because deriving the roots would
	// mean reading THIS machine's disk for another machine's path.
	//
	// A configured peer_auth account never grows the map: it works only inside
	// its own projects, and a repository it opens must not mint a project.
	if !peerUser(ctx) {
		if _, moved, err := store.RegisterProjectForCWD(ctx, h.db, p.identity(host), h.localHost); err != nil {
			stage := "register-project"
			if errors.Is(err, store.ErrRemoteRootUnknown) {
				stage = "register-project-remote-root"
			}
			h.recordHookError(ctx, stage, client, err, "cwd", p.CWD, "host", host)
		} else if moved != nil {
			h.recordRepoMoved(ctx, client, moved)
		}
	}

	// peer_auth: the owner opening a repository whose project is sealed to
	// another OS account gets a notice instead of a briefing, and no session --
	// a session there would be the owner working inside the seal.
	if sealed, ok := h.sealedCWD(ctx, host, p.CWD); ok {
		h.writeContextResponse(ctx, w, "SessionStart", "session-start", client,
			p.SessionID, "", sealedNotice(sealed), false, nil)
		return
	}

	briefing, injectedIDs, err := h.retrieve.Briefing(ctx, retrieve.BriefingInput{
		CWD: p.CWD, Host: host, Source: p.Source, AgentType: p.AgentType,
	})
	if err != nil {
		h.recordHookError(ctx, "session-start-briefing", client, err)
		briefing, injectedIDs = "", nil // never block the agent
	}
	injected := briefing != ""
	// Ambient session: create or resume the client/full-id identity so work is recorded
	// without the agent calling session_start. Subagents share the parent's
	// session, so they get no ambient session of their own. Best-effort: a failure
	// just omits the ambient line.
	if p.AgentType == "" {
		if name := h.ensureAmbientSession(ctx, client, host, p); name != "" {
			briefing = injectAmbientLine(briefing, name, p.Identity.AgentProcess != "")
			h.setAmbientModel(ctx, client, host, p.SessionID, p.Model, p.TranscriptPath)
		}
	}
	// Cap after the ambient line is appended, then record and serialize the same
	// prepared bytes. A SessionStart carries no user prompt (prompt="").
	h.writeContextResponse(ctx, w, "SessionStart", "session-start", client,
		p.SessionID, "", briefing, injected, injectedIDs)
}

func (h *Handler) sessionEnd(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	p := decodeSessionEnd(client, readHookBody(w, r))
	p.Identity = identityFromRequest(r)

	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	if h.foreignSession(ctx, client, p.SessionID) {
		p.SessionID = ""
	}

	h.completeClaudeSessions(ctx, client, p)
	// SessionEnd has no hookSpecificOutput variant in Claude Code's schema, so
	// the response is a bare ack -- emitting one fails root validation.
	writeHookAck(w)
}

// stop serves the Stop hook. Stop fires at every turn end. For Codex -- which has
// no SessionEnd event (design D5) -- it is the only end-ish signal, so it both
// heartbeats the ambient session and provisionally harvests the turn's last agent
// message into that session's findings; the idle reaper later expires the session
// with the findings already in place. For any other client it only heartbeats
// (Claude Code harvests on its own SessionEnd, and installs no Stop hook), so a CC
// session's end path is untouched.
func (h *Handler) stop(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	p := decodeStop(client, readHookBody(w, r))
	p.Identity = identityFromRequest(r)
	host := h.hostOf(p.Identity)

	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	if h.foreignSession(ctx, client, p.SessionID) {
		p.SessionID = ""
	}

	// A Stop is proof the agent is alive this turn: heartbeat so the idle reaper
	// does not expire the session mid-work. Always, for every client.
	h.touchAmbient(ctx, client, p.SessionID)
	h.setAmbientModel(ctx, client, host, p.SessionID, p.Model, p.TranscriptPath)

	if client == ClientCodex {
		h.harvestCodexStop(ctx, host, p)
	}

	// Stop has no hookSpecificOutput variant in Codex's schema (it can only
	// continue / decision:block / systemMessage), so the response is a bare ack --
	// Stop cannot inject.
	writeHookAck(w)
}

// harvestCodexStop upserts the turn's last agent message onto the Codex ambient
// session as provisional findings. Best-effort by the package's never-block
// contract: a missing session id or DB, a turn with nothing to harvest, or a
// store error all leave the heartbeat above as the Stop's only effect, logged at
// debug rather than surfaced. Repeated Stops converge findings on the latest turn.
func (h *Handler) harvestCodexStop(ctx context.Context, host string, p stopPayload) {
	if h.db == nil || p.SessionID == "" {
		return
	}
	// Everything below reads the agent's rollout file. On another machine that
	// path either does not exist here or, worse, is a DIFFERENT session's
	// rollout under the same username and home layout -- so the turn is recorded
	// as skipped rather than harvested from the wrong file.
	if !h.localDiskOK(ctx, "codex-rollout", ClientCodex, host) {
		return
	}
	// Real token totals: the rollout carries a cumulative token_count every turn, so
	// re-reading the latest one each Stop and overwriting is idempotent (Codex has no
	// SessionEnd, so this is the only place its usage lands). Independent of findings
	// -- a turn may update one without the other.
	if usage, ok := codexRolloutTokens(p.TranscriptPath); ok {
		if _, err := store.SetAmbientSessionTokens(
			ctx, h.db, ClientCodex.externalIdentity(), p.SessionID, usage, time.Now().UTC(),
		); err != nil {
			h.recordHookError(ctx, "codex-stop-token-harvest", ClientCodex, err, "external_session_id", p.SessionID)
		}
	}

	findings := codexStopFindings(p.LastAssistantMessage, p.TranscriptPath)
	if findings == "" {
		return // nothing this turn; the heartbeat kept the session alive
	}
	if _, err := store.UpdateAmbientFindings(
		ctx, h.db, ClientCodex.externalIdentity(), p.SessionID, findings, time.Now().UTC(),
	); err != nil {
		h.recordHookError(ctx, "codex-stop-findings-harvest", ClientCodex, err, "external_session_id", p.SessionID)
	}
}

// harvestClaudeTokens parses the Claude Code transcript for the session's real
// model token totals and overwrites them on the ambient row. Best-effort by the
// package's never-block contract: a blank/unparseable transcript records nothing
// (never an error). It must run while the row is still active -- the caller invokes
// it before the SessionEnd completion flips the status, so SetAmbientSessionTokens'
// active-only guard matches.
func (h *Handler) harvestClaudeTokens(ctx context.Context, client Client, host, externalSessionID, transcriptPath string, now time.Time) {
	if h.db == nil || externalSessionID == "" {
		return
	}
	if !h.localDiskOK(ctx, "token-harvest", client, host) {
		return
	}
	usage, ok := claudeTranscriptTokens(transcriptPath)
	if !ok {
		return
	}
	if _, err := store.SetAmbientSessionTokens(
		ctx, h.db, client.externalIdentity(), externalSessionID, usage, now,
	); err != nil {
		h.recordHookError(ctx, "session-end-token-harvest", client, err, "external_session_id", externalSessionID)
	}
}

// ensureAmbientSession creates (source startup) or resumes (any source) the
// ambient session for the agent's full external session identity, scoped to the
// cwd-resolved project. It returns the row's display name, or "" when there is
// no session id, no DB, or a store error (best-effort; never blocks the hook).
// The resume path is a store-side targeted UPDATE of only the fields this hook
// owns (status, project scope, recency) -- never a full-row read-modify-write,
// which could clobber the findings a concurrent transcript harvest wrote to the
// same session between the read and the write-back.
func (h *Handler) ensureAmbientSession(ctx context.Context, client Client, host string, p hookPayload) string {
	if h.db == nil || p.SessionID == "" {
		return ""
	}
	externalClient := client.externalIdentity()
	project, err := store.ResolveProjectForCWD(ctx, h.db, host, p.CWD)
	if err != nil {
		h.recordHookError(ctx, "ambient-project-resolve", client, err, "cwd", p.CWD)
		project = ""
	}
	// peer_auth: a configured account's session lands in its own project; the
	// owner is never given a session inside a project sealed to another account
	// (sessionStart answered that case with a notice already -- this is the
	// backstop).
	placed, err := access.SessionScope(ctx, project)
	if err != nil {
		return ""
	}
	project = placed

	// Resume: reactivate and re-scope, so a compact/resume continues the same
	// ambient session rather than forking a new one.
	now := time.Now().UTC()
	resumed, found, err := store.ReactivateAmbientSession(
		ctx, h.db, externalClient, p.SessionID, project, now)
	if err != nil {
		h.recordHookError(ctx, "ambient-resume", client, err)
		return ""
	}
	if found {
		h.stampAgentProcess(ctx, client, resumed.ID, p.Identity.AgentProcess)
		return resumed.Name
	}

	name := ambientName(client, p.SessionID)
	id, err := core.NewID()
	if err != nil {
		return ""
	}
	sess := core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
		ExternalSessionID: p.SessionID, ExternalClient: externalClient,
		CWD: p.CWD, Host: host, Source: p.Source, Ambient: true,
		Metadata: map[string]any{
			"claude_session_id": p.SessionID, "external_client": externalClient,
			"cwd": p.CWD, "host": host, "source": p.Source,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateSession(ctx, h.db, sess); err != nil {
		// Two SessionStart hooks racing the same full identity can both miss the
		// resume and collide on its UNIQUE identity/name. The loser resumes the
		// winner's row instead of dropping its ambient line. A display-name
		// collision belonging to another identity remains an error.
		if errors.Is(err, store.ErrSessionIdentityExists) || errors.Is(err, store.ErrSessionNameExists) {
			if existing, ok, rerr := store.ReactivateAmbientSession(
				ctx, h.db, externalClient, p.SessionID, project, now,
			); rerr == nil && ok {
				h.stampAgentProcess(ctx, client, existing.ID, p.Identity.AgentProcess)
				return existing.Name
			}
		}
		h.recordHookError(ctx, "ambient-create", client, err)
		return ""
	}
	h.stampAgentProcess(ctx, client, id, p.Identity.AgentProcess)
	h.recordSession(ctx, core.EventSessionStarted, sess, map[string]any{"ambient": true})
	return name
}

// stampAgentProcess records which agent process now runs an ambient session, so
// that agent's MCP connection binds to it (store.ActiveAmbientByAgentProcess).
// It runs on every start, including an empty identity from an older client: a
// session resumed there must stop naming the process it ran in before.
// Best-effort like the rest of the hook: a failure only costs the automatic
// binding, and the connection falls back to the ambient inference it always had.
func (h *Handler) stampAgentProcess(ctx context.Context, client Client, sessionID, proc string) {
	if err := store.SetSessionAgentProcess(ctx, h.db, sessionID, proc); err != nil {
		h.recordHookError(ctx, "ambient-agent-process", client, err)
	}
}

// completeClaudeSessions completes every active session owned by the ending
// client's full external identity: its ambient row (findings and final model
// harvested from the transcript) plus any explicit session_start linked to that
// client/id pair. Because a graceful SessionEnd is a KNOWN end, these close
// immediately rather than waiting out the idle TTL -- the reaper is only for
// sessions we get no end signal for. Task claims are released. It is a no-op when
// nothing is active (an explicit session_end already ran, or a crash left it to
// the reaper). Never errors: the hook must always 200.
func (h *Handler) completeClaudeSessions(ctx context.Context, client Client, p endPayload) {
	if h.db == nil || p.SessionID == "" {
		return
	}
	sessions, err := store.ActiveSessionsByExternalIdentity(
		ctx, h.db, client.externalIdentity(), p.SessionID)
	if err != nil {
		h.recordHookError(ctx, "session-end-lookup", client, err)
		return
	}
	if len(sessions) == 0 {
		// Nothing active to complete (an explicit session_end already ran, or a
		// crash left it to the reaper). Returning here rather than falling into an
		// empty loop is what keeps a remote end from recording a skipped capture
		// for work there was none of.
		return
	}

	now := time.Now().UTC()
	// A one-turn Claude session can start before its transcript has an assistant
	// entry and end before another UserPromptSubmit gives setAmbientModel a chance
	// to sniff it. SessionEnd is the last reliable point at which the complete
	// transcript exists. Apply the final main-thread model to the ambient row and
	// any linked explicit session; an empty sniff preserves their prior values.
	// Every transcript read below is a read of the AGENT's disk. When the agent
	// is on another machine the daemon has no transcript to read, and the path it
	// would open belongs to someone else, so the session completes with whatever
	// the client already told us and the skips are recorded.
	host := h.hostOf(p.Identity)
	localDisk := h.localDiskOK(ctx, "session-end-transcript", client, host)
	finalModel := ""
	if client == ClientClaudeCode && localDisk {
		finalModel = transcriptModel(p.TranscriptPath)
	}
	harvested := "" // harvest the transcript once, lazily, only when an ambient needs it
	for _, sess := range sessions {
		// UpdateSession deliberately does not write model (the live model setter is
		// targeted so another lifecycle write cannot clobber it). Stamp while the
		// row is still active, then mirror the successful value into sess so the
		// session.ended event immediately projects the same attribution.
		if finalModel != "" {
			if err := store.SetSessionModel(ctx, h.db, sess.ID, finalModel); err != nil {
				h.recordHookError(ctx, "session-end-model", client, err, "session", sess.ID)
			} else {
				sess.Model = finalModel
			}
		}
		sess.Status = core.SessionCompleted
		sess.UpdatedAt = now
		if sess.Ambient && localDisk {
			if harvested == "" {
				harvested = harvestFindings(p.TranscriptPath)
			}
			sess.Findings = harvested // explicit sessions keep their session_update findings
			// Overwrite the real token totals while the row is STILL active, before the
			// UpdateSession below flips it to completed -- SetAmbientSessionTokens guards
			// on status = 'active'.
			h.harvestClaudeTokens(ctx, client, host, p.SessionID, p.TranscriptPath, now)
		}
		if err := store.UpdateSession(ctx, h.db, sess); err != nil {
			h.recordHookError(ctx, "session-end-complete", client, err, "session", sess.ID)
			continue
		}
		released, rerr := store.ReleaseClaimsForSession(ctx, h.db, sess.ID, now)
		if rerr != nil {
			h.recordHookError(ctx, "session-end-release-claims", client, rerr, "session", sess.ID)
		}
		h.recordSession(ctx, core.EventSessionEnded, sess, map[string]any{
			"ambient": sess.Ambient, "harvested": sess.Ambient, "linked": !sess.Ambient,
			"reason": p.Reason, "claims_released": released,
			"findings": events.Truncate(sess.Findings, h.maxEventChars),
		})
	}
}

// foreignSession reports whether the agent session a hook names belongs to a
// project the caller's OS account may not write (peer_auth). A hook identifies
// its session only by the client's session id, which travels in the body; under
// peer_auth that id must not be enough to act on -- resume, re-scope, complete,
// harvest into, or attribute to -- another account's session. The callers blank
// the id instead, so every session-touching step downstream no-ops and the hook
// still answers. A lookup failure fails closed. With peer_auth off it is false
// without touching the database.
func (h *Handler) foreignSession(ctx context.Context, client Client, externalID string) bool {
	v, ok := access.FromContext(ctx)
	if !ok || h.db == nil || strings.TrimSpace(externalID) == "" {
		return false
	}
	ambient, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID)
	if err != nil {
		h.recordHookError(ctx, "peer-auth-session-lookup", client, err)
		return true
	}
	if found && !v.Writable(ambient.ProjectSlug) {
		return true
	}
	linked, err := store.ActiveSessionsByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID)
	if err != nil {
		h.recordHookError(ctx, "peer-auth-session-lookup", client, err)
		return true
	}
	for _, sess := range linked {
		if !v.Writable(sess.ProjectSlug) {
			return true
		}
	}
	return false
}

// peerUser reports whether the request is a configured peer_auth account's (not
// the owner's, and not peer_auth off).
func peerUser(ctx context.Context) bool {
	v, ok := access.FromContext(ctx)
	return ok && v.Principal().Kind == access.KindUser
}

// sealedCWD reports the project a cwd resolves to when the caller may not be
// placed in it at all: the owner in a repository sealed to another OS account.
// A configured account is never refused here -- its session lands in its own
// project wherever it opens.
func (h *Handler) sealedCWD(ctx context.Context, host, cwd string) (string, bool) {
	if _, ok := access.FromContext(ctx); !ok || h.db == nil {
		return "", false
	}
	project, err := store.ResolveProjectForCWD(ctx, h.db, host, cwd)
	if err != nil {
		return "", false
	}
	if _, err := access.SessionScope(ctx, project); errors.Is(err, access.ErrNotReachable) {
		return project, true
	}
	return "", false
}

// sealedNotice is the SessionStart context for the owner inside a repository
// sealed to another OS account: what happened, and what to do instead. It names
// the project (the owner mapped this repository; the slug is not news) and
// nothing from inside it.
func sealedNotice(project string) string {
	return "<seam-briefing>\nSeamless: project " + project + " is sealed to its own system's OS account (peer_auth), " +
		"so this session is not recorded and Seamless tools from this account cannot reach that project. " +
		"To work on it with its memory, run the agent as that system's account (sudo -u).\n</seam-briefing>"
}

// ambientName constructs a collision-resistant display name for a NEW ambient
// session. It is never used to resolve lifecycle identity: store lookups use the
// full external id plus client. Existing legacy rows keep and return their
// historical names when resumed.
func ambientName(client Client, externalSessionID string) string {
	if externalSessionID == "" {
		return client.ambientPrefix()
	}
	prefix := externalSessionID
	if len(prefix) > ambientPrefixLen {
		prefix = prefix[:ambientPrefixLen]
	}
	digest := sha256.Sum256([]byte(externalSessionID))
	suffix := hex.EncodeToString(digest[:ambientDigestBytes])
	return client.ambientPrefix() + strings.ToLower(prefix) + "-" + suffix
}

// injectAmbientLine adds the "Seam session: cc/<handle> (ambient)" line to a briefing,
// placing it just before the closing tag, or wrapping a fresh minimal briefing
// when there was none. The line names the reading agent's own identity -- the
// value the briefing's "session=<your Seam session>" hints refer to -- and tells
// the agent the one thing it needs about binding.
//
// bound is whether the hook named the agent process. If it did, the agent's MCP
// connection names the same process and is bound to this session on its first
// call, so the line says no session_start is needed -- the misreading that sent
// agents into session_start with invented arguments. If it did not (an older
// client), the connection stays unbound and every call with several agents
// active is ambiguous, so the line gives the single call that fixes all of them:
// resuming this session by name binds the connection to it.
func injectAmbientLine(briefing, sessionName string, bound bool) string {
	line := "Seam session: " + sessionName + " (ambient) -- "
	if bound {
		line += "this agent's Seamless tool calls bind to it automatically; no session_start needed"
	} else {
		line += "if a tool reports an ambiguous scope, session, or agent, bind once with session_start name=" + sessionName
	}
	if briefing == "" {
		return "<seam-briefing>\n" + line + "\n</seam-briefing>"
	}
	const closeTag = "</seam-briefing>"
	if i := strings.LastIndex(briefing, closeTag); i >= 0 {
		head := strings.TrimRight(briefing[:i], "\n")
		return head + "\n" + line + "\n" + closeTag
	}
	return briefing + "\n" + line
}

func (h *Handler) userPromptSubmit(w http.ResponseWriter, r *http.Request) {
	if !verifyBearer(r, h.apiKey) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	client, ok := requireRequestClient(w, r)
	if !ok {
		return
	}
	p := decodePrompt(client, readHookBody(w, r))
	if p.UserPrompt == "" {
		h.writeContextResponse(r.Context(), w, "UserPromptSubmit", "user-prompt-submit",
			client, p.SessionID, "", "", false, nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hookTimeout)
	defer cancel()
	if h.foreignSession(ctx, client, p.SessionID) {
		p.SessionID = ""
	}

	// UserPromptSubmit is installed as an http hook (it is reliable mid-turn),
	// so no seam process computed an identity for it. The ambient session the
	// SessionStart hook already created carries the host, and that is the only
	// honest source here -- guessing "local" would read a remote agent's
	// transcript path off this machine.
	p.Host = h.ambientHost(ctx, client, p.SessionID)

	h.touchAmbient(ctx, client, p.SessionID)
	h.setAmbientModel(ctx, client, p.Host, p.SessionID, p.Model, p.TranscriptPath)

	out, injectedIDs, err := h.retrieve.PromptRecall(ctx, p.Host, p.CWD, p.UserPrompt)
	if err != nil {
		h.recordHookError(ctx, "prompt-recall", client, err)
		out, injectedIDs = "", nil
	}
	if out == "" {
		// A recall miss: capture the prompt as its own hook.prompt event so the
		// Interactions feed can show "why didn't recall fire?" cases.
		h.recordHookPrompt(ctx, client, p.SessionID, p.UserPrompt)
	}
	h.writeContextResponse(ctx, w, "UserPromptSubmit", "user-prompt-submit", client,
		p.SessionID, p.UserPrompt, out, out != "", injectedIDs)
}

// recordInjection logs a retrieval.injected event carrying the exact text that
// was injected, so the console can show what an agent actually received. itemIDs
// are the memory ULIDs surfaced by this injection; recording them (as the recall
// tool does) feeds the read-after-inject funnel so the auto-briefing counts, not
// just recall-tool hits. The event is stamped with the ambient session's seamless
// ULID and project (best-effort) so the Interactions feed can group it under the
// right agent; the Claude session id still rides in the payload. prompt (empty for
// SessionStart) is the user turn that triggered a recall injection; content is the
// full emitted block, stored verbatim with bounded cap metadata.
func (h *Handler) recordInjection(ctx context.Context, hook string, client Client, claudeSessionID, prompt string, prepared preparedHookContext, itemIDs []string) {
	if h.events == nil {
		return
	}
	// A post-assembly cap can remove memory lines without retaining a structured
	// id-to-line map, so on truncation the list is a superset of what reached the
	// model -- item_ids_exact records that. The ids are still credited, because
	// item_ids is the only source of last_injected_at (store.RebuildRetrievalStats),
	// which feeds store.StaleMemories and the gardener's archive proposals.
	// Dropping them makes an actively injected memory read as never injected and
	// proposes archiving it; over-crediting a trimmed line merely keeps it alive
	// one more staleness cycle. Err toward the recoverable direction.
	originalItemCount := len(itemIDs)
	itemIDsExact := !prepared.truncated || originalItemCount == 0
	sessionID, project := h.ambientRef(ctx, client, claudeSessionID)
	payload := map[string]any{
		"hook": hook, "claude_session_id": claudeSessionID,
		"external_client":           client.externalIdentity(),
		"content":                   prepared.content,
		"item_ids":                  itemIDs,
		"item_ids_exact":            itemIDsExact,
		"original_item_count":       originalItemCount,
		"original_estimated_tokens": prepared.originalEstimatedTokens,
		"emitted_estimated_tokens":  prepared.emittedEstimatedTokens,
		"truncated":                 prepared.truncated,
	}
	if prompt != "" {
		payload["prompt"] = events.Truncate(prompt, h.maxEventChars)
	}
	if _, err := h.events.Record(ctx, core.Event{
		Kind:        core.EventInjected,
		SessionID:   sessionID,
		ProjectSlug: project,
		Payload:     payload,
	}); err != nil {
		h.recordHookError(ctx, "record-injection", client, err)
	}
}

// recordHookPrompt logs a hook.prompt event for a UserPromptSubmit that matched
// no memory -- the recall-miss case. Matched prompts ride on the retrieval.injected
// event instead (no duplicate row), so BuildRetrievalReport (which counts injected
// rows) is unaffected. Session stamping mirrors recordInjection.
func (h *Handler) recordHookPrompt(ctx context.Context, client Client, claudeSessionID, prompt string) {
	if h.events == nil || prompt == "" {
		return
	}
	sessionID, project := h.ambientRef(ctx, client, claudeSessionID)
	if _, err := h.events.Record(ctx, core.Event{
		Kind:        core.EventHookPrompt,
		SessionID:   sessionID,
		ProjectSlug: project,
		Payload: map[string]any{
			"hook": "user-prompt-submit", "claude_session_id": claudeSessionID,
			"external_client": client.externalIdentity(),
			"prompt":          events.Truncate(prompt, h.maxEventChars), "matched": false,
		},
	}); err != nil {
		h.recordHookError(ctx, "record-hook-prompt", client, err)
	}
}

// touchAmbient heartbeats the ambient session for a client's full external
// identity, keeping it live for the idle reaper. Every hook that carries
// a session id is proof the agent is alive, so prompt/tool hooks bump updated_at
// between the MCP tool calls that heartbeat it via the connection binding --
// covering a long, quiet turn that makes no seamless MCP calls. Best-effort: a
// no-op when the DB or id is absent, or the session is not active.
func (h *Handler) touchAmbient(ctx context.Context, client Client, claudeSessionID string) {
	if h.db == nil || claudeSessionID == "" {
		return
	}
	if err := store.TouchAmbientSession(
		ctx, h.db, client.externalIdentity(), claudeSessionID, time.Now().UTC(),
	); err != nil {
		h.recordHookError(ctx, "ambient-heartbeat", client, err)
	}
}

// setAmbientModel records which LLM powers the ambient session's agent, stored
// verbatim as the provider names it ("claude-fable-5", "gpt-5.5"). Codex hook
// payloads carry the model directly; Claude Code's do not, so its sessions
// fall back to tail-sniffing the transcript for the last main-thread assistant
// entry's model id. Called on session-start, every prompt, and stop; SessionEnd
// performs one final transcript sniff before freezing the row. A mid-session
// model switch therefore updates the row (SetAmbientSessionModel no-ops when the
// value is unchanged). Best-effort by the package's never-block contract:
// nothing sniffable leaves the previous attribution in place, and a store error
// only logs.
func (h *Handler) setAmbientModel(ctx context.Context, client Client, host, claudeSessionID, model, transcriptPath string) {
	if h.db == nil || claudeSessionID == "" {
		return
	}
	model = strings.TrimSpace(model)
	// A model the client STATED is good from any machine; sniffing one out of a
	// transcript means opening a file, so that half is host-gated.
	if model == "" && client == ClientClaudeCode && h.localDiskOK(ctx, "model-sniff", client, host) {
		model = transcriptModel(transcriptPath)
	}
	if model == "" {
		return
	}
	if err := store.SetAmbientSessionModel(
		ctx, h.db, client.externalIdentity(), claudeSessionID, model,
	); err != nil {
		h.recordHookError(ctx, "ambient-model", client, err)
	}
}

// ambientRef resolves the ambient session for a client's full external identity
// to its Seamless ULID and project, best-effort. It returns
// empty strings when the DB is absent, the id is empty, or no such session exists
// -- the event then carries no session attribution rather than failing the hook.
func (h *Handler) ambientRef(ctx context.Context, client Client, claudeSessionID string) (sessionID, project string) {
	if h.db == nil || claudeSessionID == "" {
		return "", ""
	}
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, h.db, client.externalIdentity(), claudeSessionID)
	if err != nil {
		h.recordHookError(ctx, "ambient-ref-lookup", client, err)
		return "", ""
	}
	if !ok {
		return "", ""
	}
	return sess.ID, sess.ProjectSlug
}

// ambientModel returns the model recorded on the client's ambient session, or
// "" when unknown (no DB, no id, no session, or a lookup failure -- logged, per
// ambientRef). Plan capture stamps it onto the notes it writes.
func (h *Handler) ambientModel(ctx context.Context, client Client, claudeSessionID string) string {
	if h.db == nil || claudeSessionID == "" {
		return ""
	}
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, h.db, client.externalIdentity(), claudeSessionID)
	if err != nil {
		h.recordHookError(ctx, "ambient-model-lookup", client, err)
		return ""
	}
	if !ok {
		return ""
	}
	return sess.Model
}

// ambientDisplayName returns the stored human handle for an ambient identity,
// preserving a legacy pre-digest name in provenance. Before a row exists (or on
// a best-effort lookup failure), it falls back to the deterministic new-name
// constructor; neither path is used for lifecycle mutation.
func (h *Handler) ambientDisplayName(ctx context.Context, client Client, externalSessionID string) string {
	if externalSessionID == "" {
		return client.ambientPrefix() + "unknown"
	}
	if h.db != nil {
		sess, ok, err := store.AmbientSessionByExternalIdentity(
			ctx, h.db, client.externalIdentity(), externalSessionID)
		if err != nil {
			h.recordHookError(ctx, "ambient-display-name-lookup", client, err)
		} else if ok {
			return sess.Name
		}
	}
	return ambientName(client, externalSessionID)
}

// hostOf resolves the machine a hook call speaks for: the host the client sent,
// or this daemon's own when it sent none. An older `seam` sends no host and can
// only be talking to a daemon on its own box, so treating absent as local is
// what keeps every existing install byte-for-byte unchanged.
func (h *Handler) hostOf(id hookIdentity) string {
	if host := strings.ToLower(strings.TrimSpace(id.Host)); host != "" {
		return host
	}
	return h.localHost
}

// identity builds the store's placement input from a SessionStart payload. The
// roots come from the client, which is the only process that can see them; the
// daemon fills them in from its own disk only for its own host (see
// store.RegisterProjectForCWD).
func (p hookPayload) identity(host string) store.CWDIdentity {
	return store.CWDIdentity{
		Host:     host,
		CWD:      p.CWD,
		RepoRoot: p.Identity.RepoRoot,
		MainRoot: p.Identity.MainRoot,
		Origin:   p.Identity.Origin,
	}
}

// sameHost reports whether a hook's machine is this daemon's, which is the
// precondition for reading any path the agent named. An empty host is an older
// client (see hostOf) and counts as local.
//
// It is a HOST check and deliberately not a "does the file exist" check: two
// devices with the same username and home layout produce the same transcript and
// plan-file paths, so a missing-file test would sometimes find a real file --
// the wrong one -- and capture another machine's session as this one's.
func (h *Handler) sameHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "" || host == h.localHost
}

// localDiskOK reports whether the daemon may read the agent's filesystem for
// this hook, recording the skip when it may not. what is a stable curated label
// for the capture being skipped (the aggregation key, like a hook.error stage);
// the host rides in the payload.
//
// Under peer_auth the answer is always no. The daemon then runs as its own OS
// account, so every path a hook names is a path some OTHER account chose -- and
// the daemon can read its own data directory. A transcript_path, plan file or
// rollout pointing (or symlinked) into it would have the daemon read a sealed
// project's files and file the result as the caller's findings or notes: the
// confused deputy the whole boundary exists to rule out. Nothing is opened, and
// nothing is recorded per call either (it would be every call); doctor states
// the cost once.
func (h *Handler) localDiskOK(ctx context.Context, what string, client Client, host string) bool {
	if _, peerAuth := access.FromContext(ctx); peerAuth {
		h.logger.Debug("hooks: peer_auth, skipping agent-disk capture", "what", what)
		return false
	}
	if h.sameHost(host) {
		return true
	}
	h.recordRemoteSkip(ctx, what, client, host)
	return false
}

// recordRemoteSkip records that a daemon-side filesystem capture was skipped
// because the agent is on another machine. It is INFO, not a warning: on a
// shared daemon this is the design working, and the event exists so doctor and
// the console can say plainly what a remote device does not get.
func (h *Handler) recordRemoteSkip(ctx context.Context, what string, client Client, host string) {
	h.logger.Info("hooks: remote host, skipping local capture", "what", what, "host", host)
	if h.events == nil {
		return
	}
	payload := map[string]any{"stage": "remote-host-skip", "what": what, "host": host}
	if client != "" {
		payload["client"] = client.externalIdentity()
	}
	if _, err := h.events.Record(ctx, core.Event{
		Kind: core.EventHookError, Payload: payload,
	}); err != nil {
		h.logger.Debug("hooks: record remote-host skip", "what", what, "error", err)
	}
}

// gitHead is the capture stamp's commit hash, gated on the agent's machine. A
// remote agent's repo is not on this disk, and a same-named checkout here would
// stamp a commit from an unrelated history -- so the stamp is left empty, which
// renders as "unknown" (shortHead) and which plans.StampHead and the gardener's
// ship-evidence pass already treat as absent.
func (h *Handler) gitHead(ctx context.Context, id hookIdentity, cwd string) string {
	if !h.localDiskOK(ctx, "git-stamp", ClientClaudeCode, h.hostOf(id)) {
		return ""
	}
	return gitread.Head(cwd)
}

// ambientHost returns the machine recorded on the client's ambient session, for
// the hooks that carry no identity of their own (UserPromptSubmit is an http
// hook, so no seam process computes one). "" when there is no such session --
// which hostOf/sameHost read as local, the same as an older client.
func (h *Handler) ambientHost(ctx context.Context, client Client, claudeSessionID string) string {
	if h.db == nil || claudeSessionID == "" {
		return ""
	}
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, h.db, client.externalIdentity(), claudeSessionID)
	if err != nil {
		h.recordHookError(ctx, "ambient-host-lookup", client, err)
		return ""
	}
	if !ok {
		return ""
	}
	return sess.Host
}

// recordHookError logs a swallowed hook-stage failure (the package's fail-open
// contract: warn and continue) and, best-effort, emits a hook.error event so
// the gardener's tool-error pass can notice failures that recur. stage is a
// stable curated label -- it is the aggregation key, so it must never embed
// per-request values; those ride in attrs (log only) and the payload's error
// text. Emission never affects fail-open: with no recorder, or a recorder
// failure, the log line is all that happens. client may be zero (an internal
// caller with no request client in scope); the payload then omits it.
// recordRepoMoved surfaces the automatic remap RegisterProjectForCWD performs
// when a moved repo's new location adopts its existing project: an INFO log for
// the daemon and a repo.moved event for the console's activity feed, so the
// owner can see the map healed itself rather than minting a duplicate project.
func (h *Handler) recordRepoMoved(ctx context.Context, client Client, moved *store.RepoMapAdoption) {
	h.logger.Info("hooks: repo moved, adopted existing project",
		"project", moved.Slug, "new_path", moved.NewPath, "old_paths", moved.OldPaths)
	if h.events == nil {
		return
	}
	if _, err := h.events.Record(ctx, core.Event{
		Kind:        core.EventRepoMoved,
		ProjectSlug: moved.Slug,
		Payload: map[string]any{
			"slug": moved.Slug, "new_path": moved.NewPath, "old_paths": moved.OldPaths,
		},
	}); err != nil {
		h.recordHookError(ctx, "record-repo-moved", client, err)
	}
}

func (h *Handler) recordHookError(ctx context.Context, stage string, client Client, err error, attrs ...any) {
	h.logger.Warn("hooks: "+stage, append(attrs, "error", err)...)
	if h.events == nil {
		return
	}
	payload := map[string]any{"stage": stage, "error": firstLine(err.Error())}
	if client != "" {
		payload["client"] = client.externalIdentity()
	}
	if _, rerr := h.events.Record(ctx, core.Event{
		Kind: core.EventHookError, Payload: payload,
	}); rerr != nil {
		h.logger.Debug("hooks: record hook error", "stage", stage, "error", rerr)
	}
}

// firstLine returns the first line of s, trimmed -- the compact error text a
// hook.error payload carries (mirroring the mcp middleware's tool.call errors,
// which the same gardener pass consumes).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// recordSession appends a session lifecycle event for an ambient session.
func (h *Handler) recordSession(ctx context.Context, kind core.EventKind, sess core.Session, payload map[string]any) {
	if h.events == nil {
		return
	}
	if _, err := h.events.Record(ctx, core.Event{
		Kind: kind, SessionID: sess.ID, ProjectSlug: sess.ProjectSlug, Payload: payload,
	}); err != nil {
		h.recordHookError(ctx, "record-session-event", "", err, "kind", kind)
	}
}

func writePreparedHookResponse(w http.ResponseWriter, event string, prepared preparedHookContext) {
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck // the 200 is already committed; a failed write means the
	// agent hung up and there is no second channel to report it on.
	_ = json.NewEncoder(w).Encode(hookResponse{
		Continue:       true,
		SuppressOutput: true,
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName: event, AdditionalContext: prepared.content,
		},
	})
}

// writeHookAck confirms a hook that carries no additional context. SessionEnd
// (and other events without a hookSpecificOutput variant) must omit that field
// entirely, or Claude Code's schema validation rejects the whole response.
func writeHookAck(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	//nolint:errcheck // see writePreparedHookResponse: the response is already committed.
	_ = json.NewEncoder(w).Encode(hookResponse{Continue: true, SuppressOutput: true})
}

// verifyBearer constant-time-compares the request's bearer token to key. A
// peer_auth socket request needs no key: the kernel named its caller before it
// got here (cmd/seamlessd), and that is the authentication.
func verifyBearer(r *http.Request, key string) bool {
	if access.PeerAuthenticated(r.Context()) {
		return true
	}
	if key == "" {
		return false
	}
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(key)) == 1
}
