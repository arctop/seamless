package core

import (
	"slices"
	"time"
)

// ---------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------

// Project groups memories, notes, sessions, tasks, and trials under a slug.
//
// ParentSlug, when set, points a child project (e.g. arctop-ios) at a shared
// parent (e.g. arctop-mobile-apps) whose active memories are injected into the
// child's briefing -- how a split keeps cross-platform knowledge shared without
// duplicating it. RetiredAt marks a project emptied by a split: kept for
// provenance and still readable, but flagged as no longer the live home.
type Project struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	ParentSlug  string     `json:"parentSlug,omitempty"`
	RetiredAt   *time.Time `json:"retiredAt,omitempty"`
	Favorite    bool       `json:"favorite,omitempty"`  // owner/agent star; never bumps UpdatedAt
	Isolation   Isolation  `json:"isolation,omitempty"` // agent-facing fence; "" reads as open, never bumps UpdatedAt
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// Retired reports whether the project has been retired (emptied by a split).
func (p Project) Retired() bool { return p.RetiredAt != nil }

// Isolation is a project's agent-facing isolation state. The fence is
// directional: confidential stops knowledge leaving the project, sealed
// additionally stops outside knowledge entering it. The global scope ("") is
// never isolated, and the console is exempt -- isolation fences agent-to-agent
// leakage, not the owner.
type Isolation string

const (
	IsolationOpen         Isolation = "open"         // shares normally (the default)
	IsolationConfidential Isolation = "confidential" // nothing leaves
	IsolationSealed       Isolation = "sealed"       // nothing in or out
)

// Isolations lists every valid isolation state, loosest first.
var Isolations = []Isolation{IsolationOpen, IsolationConfidential, IsolationSealed}

// Valid reports whether i is a recognized isolation state.
func (i Isolation) Valid() bool { return slices.Contains(Isolations, i) }

// FencesOutbound reports whether nothing may leave the project: sessions bound
// elsewhere never read it, and sessions bound to it never write outside it --
// an agent-initiated write to another scope (global included) is a leak too.
func (i Isolation) FencesOutbound() bool {
	return i == IsolationConfidential || i == IsolationSealed
}

// FencesInbound reports whether nothing may enter the project: its sessions
// see only its own scope (no global memories, no family, no cross-project
// reads) and no outside session writes into it.
func (i Isolation) FencesInbound() bool { return i == IsolationSealed }

// ---------------------------------------------------------------------------
// Indexed item kinds
// ---------------------------------------------------------------------------

// Item kinds, as stored in the fts.kind and embeddings.kind columns. They name
// what an indexed row IS, which is a different axis from MemoryKind (the
// frontmatter kind of a memory) -- do not conflate the two.
//
// The first two are the durable-knowledge kinds: file-backed, embedded, indexed
// from the files layer. The rest are the work record: DB-native rows indexed
// from the store layer, lexical-only (see store.IndexTaskFTS).
const (
	ItemKindMemory  = "memory"
	ItemKindNote    = "note"
	ItemKindTask    = "task"
	ItemKindTrial   = "trial"
	ItemKindSession = "session"
)

// KnowledgeItemKinds are the file-backed kinds: the source of truth is a
// markdown file, and both retrieval legs (FTS and cosine) cover them.
var KnowledgeItemKinds = []string{ItemKindMemory, ItemKindNote}

// WorkItemKinds are the DB-native kinds mirrored into FTS by the store layer.
// They carry no embedding: there is no content-hash reconcile loop behind them,
// so vectorizing would put a provider round-trip on every task/trial/session
// write. Lexical-only is the deliberate trade (see store/index_work.go).
var WorkItemKinds = []string{ItemKindTask, ItemKindTrial, ItemKindSession}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

// MemoryKind classifies a memory. It is the frontmatter `kind` field and is
// pinned/filtered differently per kind during briefing assembly.
type MemoryKind string

const (
	KindConstraint MemoryKind = "constraint" // hard rule that must hold on any task
	KindConvention MemoryKind = "convention" // project-local choice or layout fact
	KindRunbook    MemoryKind = "runbook"    // procedure to follow
	KindProtocol   MemoryKind = "protocol"   // interaction/coordination contract
	KindGotcha     MemoryKind = "gotcha"     // surprising pitfall
	KindDecision   MemoryKind = "decision"   // a choice and its rationale
	KindRefuted    MemoryKind = "refuted"    // a claim investigated and found false
	KindReference  MemoryKind = "reference"  // a durable pointer/fact
	KindStage      MemoryKind = "stage"      // a gated stage with status + gate lines
)

// MemoryKinds lists every valid kind, in briefing-priority-ish order.
var MemoryKinds = []MemoryKind{
	KindConstraint, KindConvention, KindRunbook, KindProtocol, KindGotcha,
	KindDecision, KindRefuted, KindReference, KindStage,
}

// Valid reports whether k is a recognized memory kind.
func (k MemoryKind) Valid() bool { return slices.Contains(MemoryKinds, k) }

// Memory is a single durable knowledge item, stored one-per-file with YAML
// frontmatter; this struct mirrors that frontmatter plus the body.
type Memory struct {
	ID            string     `json:"id"`
	Kind          MemoryKind `json:"kind"`
	Name          string     `json:"name"`
	Description   string     `json:"description"` // <=150 chars; the only text shown in indexes
	Project       string     `json:"project"`     // empty = global
	Body          string     `json:"body"`
	FilePath      string     `json:"filePath"`
	Tags          []string   `json:"tags"`
	Created       time.Time  `json:"created"`
	Updated       time.Time  `json:"updated"`
	ValidFrom     time.Time  `json:"validFrom"`
	InvalidAt     *time.Time `json:"invalidAt"`          // nil = still valid
	SupersededBy  string     `json:"supersededBy"`       // ULID of the replacement, "" = none
	SourceSession string     `json:"sourceSession"`      // provenance
	Model         string     `json:"model"`              // model that produced the content, as the provider names it (e.g. "claude-fable-5")
	Favorite      bool       `json:"favorite,omitempty"` // owner/agent star; frontmatter is authoritative, never bumps Updated
	ContentHash   string     `json:"contentHash"`
	// Extra preserves unknown frontmatter keys (e.g. Obsidian plugin fields) so
	// a parse -> render round-trip is lossless. Not mirrored to the index.
	Extra map[string]any `json:"extra,omitempty"`
}

// Active reports whether the memory is still valid (not superseded or archived).
// Inactive memories leave the briefing/prompt/recall indexes but remain readable.
func (m Memory) Active() bool { return m.InvalidAt == nil }

// ---------------------------------------------------------------------------
// Note
// ---------------------------------------------------------------------------

// Note is a work artifact (research finding, decision record, meeting summary),
// stored one-per-file with YAML frontmatter; this struct mirrors that
// frontmatter plus the body. Unlike a Memory it has no lifecycle/validity.
type Note struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	Project     string    `json:"project"` // empty = global
	Body        string    `json:"body"`
	FilePath    string    `json:"filePath"`
	Tags        []string  `json:"tags"`
	SourceURL   string    `json:"sourceUrl"`
	Model       string    `json:"model"`              // model that produced the content, as the provider names it (e.g. "claude-fable-5")
	Favorite    bool      `json:"favorite,omitempty"` // owner/agent star; frontmatter is authoritative, never bumps Updated
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	ContentHash string    `json:"contentHash"`
	// Extra preserves unknown frontmatter keys for a lossless round-trip.
	Extra map[string]any `json:"extra,omitempty"`
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// SessionStatus is the lifecycle state of an agent session.
type SessionStatus string

const (
	SessionActive    SessionStatus = "active"
	SessionCompleted SessionStatus = "completed"
	// SessionExpired is a session the reaper closed after it went idle past
	// SessionIdleTTL without a graceful session_end (a crashed/killed agent, or an
	// explicit session_start whose agent never called session_end). Distinct from
	// completed so the console can tell a harvested session from an abandoned one.
	SessionExpired SessionStatus = "expired"
)

// SessionStatuses lists every valid session status.
var SessionStatuses = []SessionStatus{SessionActive, SessionCompleted, SessionExpired}

// Valid reports whether s is a recognized session status.
func (s SessionStatus) Valid() bool { return slices.Contains(SessionStatuses, s) }

// SessionSources lists every valid Session.Source. Callers must not invent
// values: retrieve/briefing.go branches on "compact" and "resume", so a
// near-miss like "compacted" silently yields the wrong briefing shape.
var SessionSources = []string{"startup", "resume", "clear", "compact", "explicit"}

// ValidSessionSource reports whether s is a recognized session source.
func ValidSessionSource(s string) bool { return slices.Contains(SessionSources, s) }

// SessionIdleTTL is the default no-activity age beyond which an active session
// is considered dead: heartbeats (MCP tool calls for bound sessions, the ambient
// hooks for cc/* or cx/* sessions) bump updated_at, and anything quiet past this is
// reaped to SessionExpired and shown as idle in the console. It is the canonical
// liveness threshold shared by the gardener reaper and the console; it must
// comfortably exceed a long single agent turn so live work is never reaped.
// gardener.session_idle_minutes overrides it; every consumer takes the
// configured duration and falls back to this const when given <= 0.
const SessionIdleTTL = 45 * time.Minute

// LiveAsOf reports whether an active session counts as live at now: still
// active and updated within ttl (<= 0 falls back to SessionIdleTTL).
// Completed/expired sessions are never live.
func (s Session) LiveAsOf(now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = SessionIdleTTL
	}
	return s.Status == SessionActive && now.Sub(s.UpdatedAt) < ttl
}

// Session is one agent work session. Ambient sessions are created by a hook
// (readable {cc|cx}/ names with a stable full-id digest); explicit ones by
// session_start.
type Session struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	ProjectSlug string        `json:"projectSlug"`
	Status      SessionStatus `json:"status"`
	Findings    string        `json:"findings"`
	// ExternalSessionID is the client's own session id (Claude Code's session_id,
	// Codex's session id, ...) -- the key a client's SessionEnd/Stop hook uses to
	// find the ambient session it owns. The DB column, the session metadata key,
	// and the event payload key all remain "claude_session_id" for historical
	// continuity (it long predated Codex); only this Go field was generalized.
	ExternalSessionID string `json:"externalSessionId"`
	// ExternalClient disambiguates identical external session ids issued by
	// different agent clients. It is the authoritative client half of an ambient
	// session identity; Name remains a human-readable display handle.
	ExternalClient string `json:"externalClient"`
	CWD            string `json:"cwd"`
	// Host is the machine the agent is running on, lower-cased, as the client
	// reported it (config.Hostname on the daemon's own box). It is what makes
	// CWD meaningful once several devices share one daemon: the same absolute
	// path names different repositories on different machines, and any work the
	// daemon does on the agent's behalf that touches a filesystem -- plan
	// capture, git stamps, transcript harvest -- is only valid when this equals
	// the daemon's own host. "" means the client did not say, which the daemon
	// reads as its own host (an older seam CLI).
	Host   string `json:"host,omitempty"`
	Source string `json:"source"` // startup|resume|compact|clear|explicit
	// Model is the LLM currently powering the session's agent, stored verbatim
	// as the provider names it ("claude-fable-5", "gpt-5.5"). Sources, in
	// arrival order: the session_start tool's model arg, the Codex hook
	// payloads' model field, or a tail-sniff of the Claude Code transcript.
	// Updated in place when the agent switches models, and stamped onto
	// memories/notes at write time.
	Model    string `json:"model"`
	Ambient  bool   `json:"ambient"`
	Favorite bool   `json:"favorite,omitempty"` // owner/agent star; never bumps UpdatedAt
	// Tokens is the real model token consumption harvested from the agent's
	// transcript (Claude Code at SessionEnd, Codex every Stop) -- the actual burn,
	// not the injection estimate. Zero until harvested, or for a client with no
	// transcript token record. See TokenUsage.
	Tokens    TokenUsage     `json:"tokens"`
	Metadata  map[string]any `json:"metadata"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// TokenUsage is a session's cumulative model token consumption, normalized across
// agent clients so a per-project sum is meaningful. Every field is an absolute
// cumulative total for the session -- OVERWRITTEN on each harvest, never
// accumulated -- so re-harvesting a resumed or compacted session's grown
// transcript cannot double-count.
//
// Claude Code fills all four leaf fields from the per-message usage blocks (input
// and cache-read/cache-creation are distinct there). Codex has no cache-creation
// notion, so CacheCreation is always 0 and Cached carries its cached_input_tokens.
// Total is computed the same way for both (Input+Cached+CacheCreation+Output), so
// the number is comparable regardless of which client produced it.
type TokenUsage struct {
	Input         int `json:"input"`         // fresh (uncached) input processed
	Cached        int `json:"cached"`        // input served from the prompt cache (read)
	CacheCreation int `json:"cacheCreation"` // input written to the prompt cache (Claude Code only)
	Output        int `json:"output"`        // tokens the model produced
	Total         int `json:"total"`         // Input + Cached + CacheCreation + Output
}

// Empty reports whether no token usage has been harvested (all fields zero).
func (t TokenUsage) Empty() bool {
	return t.Input == 0 && t.Cached == 0 && t.CacheCreation == 0 && t.Output == 0 && t.Total == 0
}

// Normalize sets Total to the sum of the leaf fields, the single definition of
// "total tokens" shared by both clients.
func (t *TokenUsage) Normalize() {
	t.Total = t.Input + t.Cached + t.CacheCreation + t.Output
}

// FindingNoSummary is the sentinel findings value recorded when a session ends
// with nothing to harvest (missing/empty/unreadable transcript). It marks the
// session ended-without-a-summary rather than leaving findings blank -- but it
// carries no knowledge, so the briefing's findings queries exclude it: a
// content-free line is not worth an agent's context window.
const FindingNoSummary = "(auto) session ended, no summary harvested"

// ---------------------------------------------------------------------------
// Task
// ---------------------------------------------------------------------------

// TaskStatus is the state of a task in the ready-queue.
type TaskStatus string

const (
	TaskOpen       TaskStatus = "open"
	TaskInProgress TaskStatus = "in_progress"
	TaskDone       TaskStatus = "done"
	TaskDropped    TaskStatus = "dropped"
)

// TaskStatuses lists every valid task status.
var TaskStatuses = []TaskStatus{TaskOpen, TaskInProgress, TaskDone, TaskDropped}

// Valid reports whether s is a recognized task status.
func (s TaskStatus) Valid() bool { return slices.Contains(TaskStatuses, s) }

// Closed reports whether the status is terminal (done or dropped).
func (s TaskStatus) Closed() bool { return s == TaskDone || s == TaskDropped }

// Task is a unit of work with optional dependency edges. It is "ready" when all
// of its dependencies are closed.
//
// PlanSlug composes a task into a plan (see the plan:<slug> convention): a
// non-empty PlanSlug marks the task as a plan step, excluded from the default
// ready-queue but surfaced under a plan filter. ClaimedBy holds the session ULID
// that currently owns the task (empty when unclaimed); LeaseExpiresAt is when
// that claim lapses, after which the task is claimable again (lazy expiry, no
// sweeper).
type Task struct {
	ID             string     `json:"id"`
	ProjectSlug    string     `json:"projectSlug"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Status         TaskStatus `json:"status"`
	CreatedBy      string     `json:"createdBy"`
	PlanSlug       string     `json:"planSlug,omitempty"`
	ClaimedBy      string     `json:"claimedBy,omitempty"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt,omitempty"`
	Favorite       bool       `json:"favorite,omitempty"` // owner/agent star; never bumps UpdatedAt
	DependsOn      []string   `json:"dependsOn,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
}

// ClaimLive reports whether the task is actively claimed as of now: in_progress,
// a non-empty holder, and an unexpired lease. An expired lease is not live (the
// task is reclaimable), matching ClaimTask's lazy expiry.
func (t Task) ClaimLive(now time.Time) bool {
	return t.ClaimedBy != "" && t.Status == TaskInProgress &&
		t.LeaseExpiresAt != nil && t.LeaseExpiresAt.After(now)
}

// ---------------------------------------------------------------------------
// Trial (research lab)
// ---------------------------------------------------------------------------

// TrialOutcome summarizes a trial result. Free-form by design (the store does
// not constrain it); these are the conventional values.
type TrialOutcome string

const (
	OutcomePass         TrialOutcome = "pass"
	OutcomeFail         TrialOutcome = "fail"
	OutcomePartial      TrialOutcome = "partial"
	OutcomeInconclusive TrialOutcome = "inconclusive"
)

// Trial records one expected-vs-actual experiment inside a lab, with optional
// structured metrics for native querying.
type Trial struct {
	ID          string         `json:"id"`
	Lab         string         `json:"lab"`
	Title       string         `json:"title"`
	Changes     string         `json:"changes"`
	Expected    string         `json:"expected"`
	Actual      string         `json:"actual"`
	Outcome     TrialOutcome   `json:"outcome"`
	Metrics     map[string]any `json:"metrics"`
	SessionID   string         `json:"sessionId"`
	ProjectSlug string         `json:"projectSlug"`
	Favorite    bool           `json:"favorite,omitempty"` // owner/agent star
	CreatedAt   time.Time      `json:"createdAt"`
}

// ---------------------------------------------------------------------------
// Event (append-only log)
// ---------------------------------------------------------------------------

// EventKind identifies a kind of logged event. The append-only event log is the
// source for telemetry, retrieval stats, and the console feed.
type EventKind string

const (
	EventSessionStarted   EventKind = "session.started"
	EventSessionEnded     EventKind = "session.ended"
	EventMemoryWritten    EventKind = "memory.written"
	EventMemoryRead       EventKind = "memory.read"
	EventMemorySuperseded EventKind = "memory.superseded"
	EventMemoryArchived   EventKind = "memory.archived"
	EventMemoryMoved      EventKind = "memory.moved"     // relocated to another project (reproject/split)
	EventRepoMoved        EventKind = "repo.moved"       // a moved repo's new path adopted its existing project (payload: slug, new_path, old_paths)
	EventFavoriteChanged  EventKind = "favorite.changed" // an item was starred/unstarred (payload: kind, id, name, favorite, by)
	EventNoteWritten      EventKind = "note.written"
	EventNoteRead         EventKind = "note.read" // an agent read a note by id or slug (payload: slug); the note-side twin of memory.read
	EventTrialRecorded    EventKind = "trial.recorded"
	EventMemoryFirstReuse EventKind = "memory.first_reuse"         // momentum: a memory's first read by a session other than its writer (payload: name, kind, writer, reader); latched once per memory ever
	EventProjectStage     EventKind = "project.stage_reached"      // momentum: a project's latched maturity stage advanced (payload: stage, from); minted once per stage per project
	EventRecordBroken     EventKind = "gamification.record_broken" // gamification: a latched personal record advanced (payload: record, label, n, prev); minted at most once per record per local day
	EventTaskTransition   EventKind = "task.transition"
	EventInjected         EventKind = "retrieval.injected"
	EventGardenerAction   EventKind = "gardener.action"
	EventToolCall         EventKind = "tool.call"    // MCP tool invocation, logged live by the middleware (also the shape used by import)
	EventHookPrompt       EventKind = "hook.prompt"  // a UserPromptSubmit that matched no memory (recall miss)
	EventRecallMiss       EventKind = "recall.miss"  // a recall tool call that returned zero hits (payload: query, scope, limit, source)
	EventHookError        EventKind = "hook.error"   // a hook-stage failure swallowed fail-open (payload: stage, error, client)
	EventAgentMishap      EventKind = "agent.mishap" // a mishap the agent self-reported at session_end (payload: description, plus item_ids of the memories the text names)

	// Claude Code plan-mode capture (PostToolUse/PermissionRequest/SubagentStop hooks).
	EventPlanCaptured     EventKind = "plan.captured"     // a plan-file iteration landed as a cc-plan note
	EventPlanPresented    EventKind = "plan.presented"    // the user was prompted to review the plan
	EventPlanApproved     EventKind = "plan.approved"     // the user approved the plan (ExitPlanMode)
	EventPlanShipped      EventKind = "plan.shipped"      // momentum: a plan's final open step closed as done (payload: plan, project, steps); latched once per plan via RecordOnce
	EventSubagentCaptured EventKind = "subagent.captured" // a planning subagent's prompt+report landed as a cc-agent note

	// Project isolation: the owner moved a project's agent-facing fence.
	EventProjectIsolationChanged EventKind = "project.isolation.changed" // (payload: slug, from, to, families + parent detached by a tighten, by)

	// Write screening (the screening optional feature, internal/screening).
	// Payloads carry verdict and reason codes, ids, kinds and sizes -- never a
	// body, a description, or the text a screen matched.
	EventMemoryScreened  EventKind = "memory.screened"   // a screened write (item_id: the memory or note; payload: item_kind, kind, tool, size, verdicts, held)
	EventMemoryHeld      EventKind = "memory.held"       // a write was held out of the corpus for the owner (item_id: the item it lands on; payload: hold, op, item_kind, kind, tool, reasons)
	EventMemoryReleased  EventKind = "memory.released"   // the owner released a held write and it landed (item_id; payload: hold, op, item_kind, kind, by)
	EventMemoryDiscarded EventKind = "memory.discarded"  // the owner discarded a held write; its text is erased (item_id; payload: hold, op, item_kind, kind, by)
	EventSessionTainted  EventKind = "session.tainted"   // a session was marked as having read outside text (session_id; payload: reason, via); recorded once per session
	EventSessionCleared  EventKind = "session.untainted" // the owner cleared a session's outside-text mark (session_id; payload: by)

	// Background update check (internal/update). Payload values are parsed
	// X.Y.Z versions and fixed install-kind words, never release-API text.
	EventUpdateAvailable EventKind = "update.available" // a newer installable release was seen (item_id: the version; payload: version, current, kind); latched once per version via RecordOnce
	EventUpdateApplied   EventKind = "update.applied"   // the daemon started on a different version than the last one (item_id: the new version; payload: from, to, direction, plus codex_hooks_changed when Codex's hooks.json changed with it); recorded after the state file is written
	EventUpdateStarted   EventKind = "update.started"   // the daemon handed an update attempt to its updater (item_id: the version it installs; payload: from, to, why, path, attempt); recorded after the attempt is saved, before the spawn
	EventUpdateFailed    EventKind = "update.failed"    // an update attempt the daemon started did not apply (item_id: the version it tried; payload: from, to, outcome, stage, why, rolled_back, attempt, plus refusal -- a fixed word -- when the updater's gates refused it); never the updater's error text
)

// Event is one entry in the append-only log. Payload carries kind-specific
// detail (e.g. which memory names were injected).
type Event struct {
	ID          string         `json:"id"`
	TS          time.Time      `json:"ts"`
	Kind        EventKind      `json:"kind"`
	SessionID   string         `json:"sessionId,omitempty"`
	ProjectSlug string         `json:"projectSlug,omitempty"`
	ItemID      string         `json:"itemId,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
}
