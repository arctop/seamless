package core

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Project
// ---------------------------------------------------------------------------

// Project groups memories, notes, sessions, tasks, and trials under a slug.
//
// ParentSlug, when set, points a child project (e.g. example-ios) at a shared
// parent (e.g. example-mobile-apps) whose active memories are injected into the
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
	// Account is the restricted local account (peer_auth.restricted) that
	// wrote the current content, as the kernel named it on the socket; "" for
	// the owner, background work and every install without restricted
	// accounts. It follows the content the way Model does: a content write
	// re-stamps it, metadata (tags, a star, a supersede pointer) leaves it.
	// Frontmatter `account`, mirrored to the index.
	Account     string `json:"account,omitempty"`
	ContentHash string `json:"contentHash"`
	// MayContain is the memory's provenance stamps (frontmatter may_contain,
	// mirrored into item_provenance; see Stamp): set from the writing
	// conversation's state, never from agent input, and never removed by a
	// write. Empty for every memory written with isolation.requests off.
	MayContain []Stamp `json:"mayContain,omitempty"`
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
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Project     string   `json:"project"` // empty = global
	Body        string   `json:"body"`
	FilePath    string   `json:"filePath"`
	Tags        []string `json:"tags"`
	SourceURL   string   `json:"sourceUrl"`
	Model       string   `json:"model"`              // model that produced the content, as the provider names it (e.g. "claude-fable-5")
	Favorite    bool     `json:"favorite,omitempty"` // owner/agent star; frontmatter is authoritative, never bumps Updated
	// Account is the restricted local account (peer_auth.restricted) that
	// wrote the current content, as the kernel named it on the socket; "" for
	// the owner, background work and every install without restricted
	// accounts. It follows the content the way Model does: a content write
	// re-stamps it, metadata (tags, a star, a supersede pointer) leaves it.
	// Frontmatter `account`, mirrored to the index.
	Account     string    `json:"account,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	ContentHash string    `json:"contentHash"`
	// MayContain is the note's provenance stamps, as Memory.MayContain.
	MayContain []Stamp `json:"mayContain,omitempty"`
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
	// SessionExpired is a session the reaper closed once it was no longer live
	// (Session.LiveAsOf: idle past SessionIdleTTL and holding no live claim)
	// without a graceful session_end (a crashed/killed agent, or an explicit
	// session_start whose agent never called session_end). Distinct from
	// completed so the console can tell a harvested session from an abandoned one.
	SessionExpired SessionStatus = "expired"
)

// SessionStatuses lists every valid session status.
var SessionStatuses = []SessionStatus{SessionActive, SessionCompleted, SessionExpired}

// Valid reports whether s is a recognized session status.
func (s SessionStatus) Valid() bool { return slices.Contains(SessionStatuses, s) }

// SessionVerdict is a session's flag: the whole session went unusually well
// (good) or unusually badly (bad), or is worth re-reading for another reason
// (notable). none is the argument that clears a flag; it is never stored.
type SessionVerdict string

const (
	VerdictGood    SessionVerdict = "good"
	VerdictBad     SessionVerdict = "bad"
	VerdictNotable SessionVerdict = "notable"
	VerdictNone    SessionVerdict = "none"
)

// SessionVerdicts lists every value a verdict argument takes: the three flags,
// then none, which clears one.
var SessionVerdicts = []SessionVerdict{VerdictGood, VerdictBad, VerdictNotable, VerdictNone}

// FlagVerdicts lists the verdicts a flagged session carries (SessionVerdicts
// less none).
var FlagVerdicts = []SessionVerdict{VerdictGood, VerdictBad, VerdictNotable}

// Valid reports whether v is a value a verdict argument takes.
func (v SessionVerdict) Valid() bool { return slices.Contains(SessionVerdicts, v) }

// Flags reports whether v sets a flag (a valid verdict other than none).
func (v SessionVerdict) Flags() bool { return slices.Contains(FlagVerdicts, v) }

// MaxVerdictReasonRunes bounds a flag's reason: one line, said briefly.
const MaxVerdictReasonRunes = 500

// TranscriptCopy is what became of a flagged session's transcript copy:
// copied, or the fixed word saying why none was made. "" means none was
// attempted yet: the copy is made at the session's end signal.
type TranscriptCopy string

const (
	// TranscriptCopied: the copy is kept.
	TranscriptCopied TranscriptCopy = "copied"
	// TranscriptNoPath: no hook named a transcript for the session (one from
	// session_start without hooks).
	TranscriptNoPath TranscriptCopy = "no_transcript"
	// TranscriptRemote: the agent runs on another computer, whose files the
	// daemon cannot read, and its hooks send none (they send one only to a
	// peer_auth socket).
	TranscriptRemote TranscriptCopy = "remote"
	// TranscriptNotPushed: under peer_auth the daemon opens no file an agent
	// names, and the agent's own hook sent none: it sends one only over the
	// peer_auth socket (an agent on the TCP key sends none). A later hook of
	// the same caller on the socket is asked for it, as for TranscriptPending.
	TranscriptNotPushed TranscriptCopy = "not_pushed"
	// TranscriptPending: under peer_auth, a flagged session holds no copy and
	// its end signal sent none (a crash, a closed terminal, a flag set after
	// the end), so a later hook of the same agent client, account and computer
	// is asked for it, at most TranscriptAskLimit times.
	TranscriptPending TranscriptCopy = "pending"
	// TranscriptRefused: the agent's hook would not read the path the daemon
	// recorded for the session: it is not a regular *.jsonl file inside its
	// agent client's own transcript folder.
	TranscriptRefused TranscriptCopy = "refused"
	// TranscriptNotSent: the agent's later hooks were asked for the
	// transcript TranscriptAskLimit times, and none sent it.
	TranscriptNotSent TranscriptCopy = "not_sent"
	// TranscriptUnreadable: the transcript file could not be read (gone, or
	// not a regular file).
	TranscriptUnreadable TranscriptCopy = "unreadable"
	// TranscriptTooLarge: the transcript is over the size a copy may have.
	TranscriptTooLarge TranscriptCopy = "too_large"
	// TranscriptFailed: the copy could not be written, or its upload was cut
	// off or did not decode.
	TranscriptFailed TranscriptCopy = "failed"
)

// TranscriptCopies lists every TranscriptCopy value.
var TranscriptCopies = []TranscriptCopy{
	TranscriptCopied, TranscriptNoPath, TranscriptRemote, TranscriptNotPushed,
	TranscriptUnreadable, TranscriptTooLarge, TranscriptFailed,
	TranscriptPending, TranscriptRefused, TranscriptNotSent,
}

// TranscriptAsked lists the states in which a flagged session that holds no
// copy is still asked for its transcript at the agent's later hooks
// (peer_auth): none attempted yet, pending, not pushed, and an upload that
// failed. Every other state stands until the session is flagged again.
var TranscriptAsked = []TranscriptCopy{"", TranscriptPending, TranscriptNotPushed, TranscriptFailed}

// TranscriptAskLimit is how many times the agent's later hooks are asked for
// a session's transcript before Seamless gives up on it (TranscriptNotSent).
const TranscriptAskLimit = 5

// Valid reports whether c is a TranscriptCopy value.
func (c TranscriptCopy) Valid() bool { return slices.Contains(TranscriptCopies, c) }

// Asked reports whether a flagged session in state c that holds no copy is
// still asked for its transcript (TranscriptAsked).
func (c TranscriptCopy) Asked() bool { return slices.Contains(TranscriptAsked, c) }

// Explain is the copy's state in words, for the owner's surfaces.
func (c TranscriptCopy) Explain() string {
	switch c {
	case "":
		return "no copy yet: it is made at the session's end"
	case TranscriptCopied:
		return "copy kept"
	case TranscriptNoPath:
		return "no copy: no hook named a transcript for this session"
	case TranscriptRemote:
		return "no copy: the agent ran on another computer, whose files this daemon cannot read"
	case TranscriptNotPushed:
		return "no copy yet: under peer_auth only the agent's own hook sends it, over the socket, and none came; " +
			"the agent's next hook on the socket is asked for it"
	case TranscriptPending:
		return "no copy yet: the session ended without sending it, so the agent's next hook on its computer is asked for it"
	case TranscriptRefused:
		return "no copy: the agent's hook would not read the recorded path, which is not a transcript " +
			"in its agent client's own transcript folder"
	case TranscriptNotSent:
		return "no copy: the agent's later hooks were asked for it " + strconv.Itoa(TranscriptAskLimit) +
			" times, and none sent it"
	case TranscriptUnreadable:
		return "no copy: the transcript file could not be read"
	case TranscriptTooLarge:
		return "no copy: the transcript is larger than a copy may be"
	case TranscriptFailed:
		return "no copy: it could not be written, or its upload did not arrive whole"
	}
	return "no copy: " + string(c)
}

// Flagged reports whether the session carries a flag.
func (s Session) Flagged() bool { return s.Verdict.Flags() }

// SessionSources lists every valid Session.Source. Callers must not invent
// values: retrieve/briefing.go branches on "compact" and "resume", so a
// near-miss like "compacted" silently yields the wrong briefing shape.
var SessionSources = []string{"startup", "resume", "clear", "compact", "explicit"}

// ValidSessionSource reports whether s is a recognized session source.
func ValidSessionSource(s string) bool { return slices.Contains(SessionSources, s) }

// SessionIdleTTL is the default no-activity age beyond which an active session
// that holds no live claim is considered dead. What Seamless sees bumps
// updated_at: a tool call credited to the session, and the ambient hooks of a
// cc/* or cx/* session (a prompt, a session start, a subagent's start or stop,
// a Codex turn's end, a plan capture). A shell command is not among them, nor
// is an edit outside the plans directory. A session quiet past this and holding
// no live claim is reaped to SessionExpired and shown as idle in the console
// (LiveAsOf). It does not bound how long work may go quiet: a holder running a
// long gate or waiting on CI keeps its session live by holding a claim, whose
// lease is at most MaxClaimLease. It is the liveness threshold shared by the
// gardener reaper, the console and the update gate.
// gardener.session_idle_minutes overrides it; every consumer takes the
// configured duration and falls back to this const when given <= 0.
const SessionIdleTTL = 45 * time.Minute

// LiveAsOf reports whether a session counts as live at now. It is the one
// liveness rule: the reaper expires exactly the active sessions it calls dead,
// and the console and the update gate count exactly the ones it calls live. A
// session is live while it is active and either updated within ttl (<= 0 falls
// back to SessionIdleTTL; updated exactly ttl ago still counts) or holding a
// claim that is live at now (HoldsAt). Completed and expired sessions are never
// live. Its SQL form, for the readers that count in the database, is the
// store's sessionLive, which must stay its exact equivalent.
func (s Session) LiveAsOf(now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = SessionIdleTTL
	}
	if s.Status != SessionActive {
		return false
	}
	return !s.UpdatedAt.Before(now.Add(-ttl)) || s.HoldsAt(now)
}

// HoldsAt reports whether the session holds a claim that is live at now:
// HeldUntil strictly after now, Task.ClaimLive's boundary, so a lease that
// lapses exactly at now keeps nothing live.
func (s Session) HoldsAt(now time.Time) bool {
	return s.HeldUntil != nil && s.HeldUntil.After(now)
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
	// Account is the restricted local account (peer_auth.restricted) that
	// started the session, "" for the owner and every install without one. It
	// is set once, at creation, and the session -- its findings included --
	// belongs to that principal alone (access.SessionAccountOK).
	Account string `json:"account,omitempty"`
	// ParentSessionID is the session this one continues: a move into a fenced
	// project (isolation.requests) opens a child session there whose parent is
	// the session the conversation left. "" for every other session. It is set
	// once, at creation, and nothing changes it afterwards.
	ParentSessionID string `json:"parentSessionId,omitempty"`
	// MayContain is the provenance stamps of the session's findings (see
	// Stamp), which live in item_provenance only: no sessions column holds
	// them and no store read fills them; a surface that shows them reads the
	// index and sets them (the console's session page). Empty for every
	// session of an install that never turned isolation.requests on.
	MayContain []Stamp `json:"mayContain,omitempty"`
	// Tokens is the real model token consumption harvested from the agent's
	// transcript (Claude Code at SessionEnd, Codex every Stop) -- the actual burn,
	// not the injection estimate. Zero until harvested, or for a client with no
	// transcript token record. See TokenUsage.
	Tokens TokenUsage `json:"tokens"`
	// TranscriptPath is the agent client's transcript file as its hooks name
	// it, recorded at every SessionStart; "" when no hook named one (a session
	// from session_start without hooks). A path on the agent's computer: under
	// peer_auth the daemon never opens it.
	TranscriptPath string `json:"transcriptPath,omitempty"`
	// The flag (session_update and session_end's verdict and reason): Verdict
	// is good, bad or notable, "" while the session is not flagged; FlaggedAt
	// is when it was last set. One flag per session, the last one set wins.
	Verdict       SessionVerdict `json:"verdict,omitempty"`
	VerdictReason string         `json:"verdictReason,omitempty"`
	FlaggedAt     *time.Time     `json:"flaggedAt,omitempty"`
	// The flagged session's transcript copy (internal/transcripts): Transcript
	// is copied, or the word saying why none was made, "" while none was
	// attempted; TranscriptBytes is the transcript's size when it was copied.
	// The record alone does not prove the file is on this disk (a restored or
	// merged database may name a copy made elsewhere): readers check the file.
	Transcript         TranscriptCopy `json:"transcriptCopy,omitempty"`
	TranscriptBytes    int64          `json:"transcriptBytes,omitempty"`
	TranscriptCopiedAt *time.Time     `json:"transcriptCopiedAt,omitempty"`
	// TranscriptAsks is how many times the agent's later hooks were asked for
	// the transcript of a flagged session holding no copy (peer_auth;
	// TranscriptAsked), and TranscriptAskedAt when last; a new flag starts
	// them over. They live in a table of their own, and only the pending
	// read (store.PendingTranscriptSessions) fills them: every other read
	// leaves them zero, and no JSON surface carries them.
	TranscriptAsks    int            `json:"-"`
	TranscriptAskedAt *time.Time     `json:"-"`
	Metadata          map[string]any `json:"metadata"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	// HeldUntil is when the furthest of the holds the session has lapses: the
	// latest lease among the in_progress tasks it claimed and the repo locks
	// it holds, lapsed or not; nil when it holds none. Every store read of a session fills it in the
	// same statement as the row, and LiveAsOf reads it. It is derived, never
	// written, and stays out of every JSON surface.
	HeldUntil *time.Time `json:"-"`
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

// TaskKind says what a task is: a step (work an agent claims and finishes) or
// a gate (a person's action, or an event outside Seamless, that the steps
// depending on it wait for). A gate is never ready and never claimed; a person
// clears it, which closes it and unblocks its dependents.
type TaskKind string

const (
	TaskKindStep TaskKind = "step"
	TaskKindGate TaskKind = "gate"
)

// TaskKinds lists every valid task kind.
var TaskKinds = []TaskKind{TaskKindStep, TaskKindGate}

// Valid reports whether k is a recognized task kind.
func (k TaskKind) Valid() bool { return slices.Contains(TaskKinds, k) }

// TaskGate is what a task waits on outside Seamless: who must act (the owner,
// reviewer:<name>, sitting:<name>, hardware, sealed:<project>, or free text)
// and what must happen, optionally the open question the person answers and
// when the task is ready. A task carries one gate at most. The gate is open
// while Who is set and ClearedAt is nil; clearing it records when, by whom and
// a note, and leaves the task's status alone (a gate-kind task closes).
type TaskGate struct {
	Who       string     `json:"who"`
	What      string     `json:"what"`
	Decision  string     `json:"decision,omitempty"`
	ReadyWhen string     `json:"readyWhen,omitempty"`
	ClearedAt *time.Time `json:"clearedAt,omitempty"`
	ClearedBy string     `json:"clearedBy,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// Set reports whether the task carries a gate, open or cleared.
func (g TaskGate) Set() bool { return g.Who != "" }

// Open reports whether the gate still holds the task: set and not cleared.
func (g TaskGate) Open() bool { return g.Who != "" && g.ClearedAt == nil }

// GateWhoMatches reports whether a gate's who answers a who filter: equal
// ignoring case, or the filter names the class before a colon ("reviewer"
// matches "reviewer:dana"). An empty filter matches nothing.
func GateWhoMatches(who, filter string) bool {
	who, filter = strings.TrimSpace(who), strings.TrimSpace(filter)
	if filter == "" || who == "" {
		return false
	}
	if strings.EqualFold(who, filter) {
		return true
	}
	class, _, found := strings.Cut(who, ":")
	return found && !strings.Contains(filter, ":") && strings.EqualFold(strings.TrimSpace(class), filter)
}

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
	// Kind is a step or a gate (TaskKind); a row read from the store always
	// carries one, and "" is a step to every writer. Gate is what the task
	// waits on outside Seamless, zero when nothing (a gate-kind task always
	// carries one: it describes the gate).
	Kind TaskKind `json:"kind,omitempty"`
	Gate TaskGate `json:"gate,omitzero"`
	// Account is the restricted local account (peer_auth.restricted) that
	// created the task, and UpdatedAccount the one whose rewrite of the title
	// or body the text may still carry (a status flip, a claim or a release
	// never moves it; store.UpdateTask's taskTextAccount); "" for the owner,
	// background work and every install without restricted accounts.
	Account        string     `json:"account,omitempty"`
	UpdatedAccount string     `json:"updatedAccount,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
}

// IsGate reports whether the task is a gate rather than a step.
func (t Task) IsGate() bool { return t.Kind == TaskKindGate }

// HeldByGate reports whether the task itself waits on people: a gate, or a
// step whose own gate is still open. Neither is ever ready or claimable.
// (A step can also wait on a gate it depends on; that is its blockers'
// business, store.GatedTasks.)
func (t Task) HeldByGate() bool { return t.IsGate() || t.Gate.Open() }

// ClaimLive reports whether the task is actively claimed as of now: in_progress,
// a non-empty holder, and an unexpired lease. An expired lease is not live (the
// task is reclaimable), matching ClaimTask's lazy expiry. A live claim also
// keeps its holder's session live, at the same boundary (Session.HoldsAt).
func (t Task) ClaimLive(now time.Time) bool {
	return t.ClaimedBy != "" && t.Status == TaskInProgress &&
		t.LeaseExpiresAt != nil && t.LeaseExpiresAt.After(now)
}

// MaxClaimLease is the ceiling on one claim's lease: a tasks_claim whose
// lease_seconds asks for more is refused, and the refusal names the ceiling.
// Work that needs longer re-claims (heartbeats) before the lease lapses. A live
// claim keeps its holder's session live (Session.LiveAsOf), so the ceiling also
// bounds how long the session of a holder that crashed outlives its last claim.
const MaxClaimLease = 4 * time.Hour

// TaskBodyChange is how a tasks_update changed a task's body: replaced it
// whole, or appended a line to it.
type TaskBodyChange string

const (
	TaskBodyReplaced TaskBodyChange = "replace"
	TaskBodyAppended TaskBodyChange = "append"
)

// TaskBodyChanges lists every valid body change (migration 054's CHECK).
var TaskBodyChanges = []TaskBodyChange{TaskBodyReplaced, TaskBodyAppended}

// TaskBodyRevision is a body a task had before a change replaced it: the text
// as it was, the change (a replacement, or an append, after which the body
// starts with this text), when it landed, and the session whose call made it
// ("" when the call had none). Revision numbers a task's revisions in the order
// the changes landed, from 1, and is never reused. UpdatedAccount is the task's
// updated_account when the change landed: the restricted account whose words
// Body may carry, "" for the owner.
type TaskBodyRevision struct {
	TaskID         string         `json:"taskId"`
	Revision       int            `json:"revision"`
	Body           string         `json:"body"`
	Change         TaskBodyChange `json:"change"`
	ReplacedAt     time.Time      `json:"replacedAt"`
	ReplacedBy     string         `json:"replacedBy,omitempty"`
	UpdatedAccount string         `json:"updatedAccount,omitempty"`
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
	// Account is the restricted local account (peer_auth.restricted) that
	// recorded the trial, "" for the owner.
	Account   string    `json:"account,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
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
	EventTaskTransition   EventKind = "task.transition"            // a task changed (payload: to; from, the status before, on every tasks_update; plus created, claimed_by/reclaimed/prior_holder, released, commits_recorded, removed_depends_on, kind (a task made a gate or a step) or gate (set, cleared or removed) as the call set them; receipt and reference for a moved conversation's receipt (isolation.requests))
	EventInjected         EventKind = "retrieval.injected"
	EventGardenerAction   EventKind = "gardener.action"
	EventToolCall         EventKind = "tool.call"    // MCP tool invocation, logged live by the middleware (also the shape used by import)
	EventHookPrompt       EventKind = "hook.prompt"  // a UserPromptSubmit that matched no memory (recall miss)
	EventRecallMiss       EventKind = "recall.miss"  // a recall tool call that returned zero hits (payload: query, scope, limit, source)
	EventHookError        EventKind = "hook.error"   // a hook-stage failure swallowed fail-open (payload: stage, error, client)
	EventAgentMishap      EventKind = "agent.mishap" // a mishap the agent self-reported at session_end (payload: description, plus item_ids of the memories the text names)

	// The session flag (session_update / session_end verdict) and the
	// transcript copy it keeps (internal/transcripts).
	EventSessionFlagged    EventKind = "session.flagged"    // a session was flagged, or its flag changed (session_id; payload: verdict, reason, by, retroactive)
	EventSessionUnflagged  EventKind = "session.unflagged"  // a session's flag was cleared and its transcript copy removed (session_id; payload: verdict -- the one cleared -- by, copy_removed)
	EventSessionTranscript EventKind = "session.transcript" // a flagged session's transcript was copied, or why not, or asked for again (session_id; payload: copy -- a core.TranscriptCopy word -- bytes, via: daemon, client or owner, asks)

	// Claude Code plan-mode capture (PostToolUse/PermissionRequest/SubagentStop hooks).
	EventPlanCaptured     EventKind = "plan.captured"     // a plan-file iteration landed as a cc-plan note
	EventPlanPresented    EventKind = "plan.presented"    // the user was prompted to review the plan
	EventPlanApproved     EventKind = "plan.approved"     // the user approved the plan (ExitPlanMode)
	EventPlanShipped      EventKind = "plan.shipped"      // momentum: a plan's final open step closed as done (payload: plan, project, steps); latched once per plan via RecordOnce
	EventSubagentCaptured EventKind = "subagent.captured" // a planning subagent's prompt+report landed as a cc-agent note

	// The owner changed the optional features (console Settings -> Features or
	// the Home welcome card; payload: features, by).
	EventFeaturesChanged EventKind = "settings.features_changed"

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
	EventAccessRefused   EventKind = "access.refused"    // a refused access: a fence, a peer account, a bad credential (session_id when bound; payload: reason, surface, tool, target, suppressed)
	EventCorpusDrift     EventKind = "corpus.drift"      // a memory or note file changed outside Seamless, appeared with no Seamless write, or went missing (item_id; payload: change, item_kind, verdicts, quarantined)
	EventCorpusReviewed  EventKind = "corpus.reviewed"   // the review read every memory and note file (payload: items, safe, flagged, by, baseline)
	EventCorpusConfirmed EventKind = "corpus.confirmed"  // the owner marked a flagged item safe or accepted a drifted one (item_id; payload: item_kind, via, by)
	EventLockdownChanged EventKind = "lockdown.changed"  // lockdown went on or off (payload: on, source, by)

	// Locked projects: the owner's approvals of an agent's
	// access. Payloads carry ids, codes and lengths -- never the agent's reason
	// or a path.
	EventAccessRequested    EventKind = "access.requested"            // an agent asked for a locked project (item_id: the request; session_id when known; payload: kind, via)
	EventAccessApproved     EventKind = "access.approved"             // the owner approved a request (item_id: the request; payload: lease, proof, seconds)
	EventAccessDenied       EventKind = "access.denied"               // the owner denied a request (item_id: the request; payload: proof)
	EventAccessRevoked      EventKind = "access.revoked"              // leases ended early (payload: reason, leases; session_id for a session end)
	EventAccessGranted      EventKind = "access.granted"              // the owner approved a read grant of a confidential project (isolation.requests; item_id: the request; session_id; project: the target; payload: grant, from_project, proof, seconds, duration)
	EventAccessConfined     EventKind = "access.confined"             // a session in an open project was write-confined by its read grant (item_id: the request; session_id; project: the confined project; payload: grant, target_project)
	EventConsoleLockChanged EventKind = "access.console_lock_changed" // a lock turned on or off, from the console or by an archive import (item_id: the proving credential, empty for an import; project: the project; payload: proof, action, by -- console -- for a passkey's, and leases, grants, unlocks: the access a lock ended)
	EventConsoleLockFailed  EventKind = "access.console_lock_failed"  // a console lock change failed after its ceremony began (item_id: the passkey, when an enrolled one answered; project: the ceremony's; payload: action, reason -- mismatch, assertion, credential, counter, race, state, error)
	// Device pairing (internal/devices): the computers a networked server
	// pairs. Payloads carry ids, kinds, a host name and fixed words -- never a
	// code, a key or a secret.
	EventDeviceCode    EventKind = "access.device_code"    // a pairing code was minted (payload: by -- console or terminal -- and minutes)
	EventDevicePending EventKind = "access.device_pending" // a pairing waits for the owner's passkey, peer_auth only (item_id: the device; payload: kind, name)
	EventDevicePaired  EventKind = "access.device_paired"  // a device was paired (item_id: the device; payload: kind, name, proof -- code or passkey -- and credential, the passkey)
	EventDeviceRevoked EventKind = "access.device_revoked" // a device was revoked, or a waiting pairing denied (item_id: the device; payload: kind, name, action -- revoke or deny -- by, requests)
	EventRelayDecision EventKind = "access.relay_decision" // an owner's decision made through a relay by the owner's own hands there (item_id: the relay's device; payload: name, uid, route, programs)

	// Background update check (internal/update). Payload values are parsed
	// X.Y.Z versions and fixed install-kind words, never release-API text.
	EventUpdateAvailable EventKind = "update.available" // a newer installable release was seen (item_id: the version; payload: version, current, kind); latched once per version via RecordOnce
	EventUpdateApplied   EventKind = "update.applied"   // the daemon started on a different version than the last one (item_id: the new version; payload: from, to, direction, plus codex_hooks_changed when Codex's hooks.json changed with it); recorded after the state file is written
	EventUpdateStarted   EventKind = "update.started"   // the daemon handed an update attempt to its updater (item_id: the version it installs; payload: from, to, why, path, attempt); recorded after the attempt is saved, before the spawn
	EventUpdateFailed    EventKind = "update.failed"    // an update attempt the daemon started did not apply (item_id: the version it tried; payload: from, to, outcome, stage, why, rolled_back, attempt, plus refusal -- a fixed word -- when the updater's gates refused it, and signature -- a fixed word -- when update.release_signers refused the release); never the updater's error text

	// Backups (internal/backup). Payloads carry file names, kinds, counts,
	// sizes and fixed reason words -- never a path's contents or error text.
	EventBackupCreated       EventKind = "backup.created"        // a backup was written and read back (item_id: the file name; payload: kind, where, bytes, memories, notes, projects)
	EventBackupFailed        EventKind = "backup.failed"         // a backup did not complete (payload: kind, where, reason -- a fixed word -- and failures, the automatic streak)
	EventBackupDeleted       EventKind = "backup.deleted"        // the owner deleted a backup, or pruning removed an old automatic one (item_id: the file name; payload: kind, where, by)
	EventBackupRestored      EventKind = "backup.restored"       // a restore was applied (item_id: the backup's file name; payload: mode, sha256, proof)
	EventBackupRestoreFailed EventKind = "backup.restore_failed" // a restore did not finish, or expired (item_id: the backup's file name; payload: mode, stage, reason -- a fixed word)
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
