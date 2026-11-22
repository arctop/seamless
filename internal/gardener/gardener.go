// Package gardener runs the propose-only maintenance passes over the memory
// corpus: it finds near-duplicate memories (dedup), memories that have gone
// untouched for too long (staleness), stage memories that stopped carrying a
// live gate (stale-stage), memories briefings keep injecting without any
// demand (dead-weight), captured plans that were never approved (stale-plan),
// recall queries that keep missing because nobody wrote the memory
// (memory-wanted), errors agents keep hitting on tool calls or hook stages
// (tool-error), stamped copies whose cited source changed since (stale-copy)
// or whose sources all lie in one other project (copy-relocate; both in
// copies.go), and rolls recent sessions into a monthly digest. Every pass
// only ever writes a gardener_proposals row for the owner to apply or dismiss
// -- it never mutates a memory on its own. The one exception is the owner's
// opt-in (gardener.auto_apply, auto.go): a new proposal of a kind the owner
// named, from the four AutoSafe ones, is applied at the end of the pass that
// raised it, in the name "auto", and stays undoable.
//
// The same schedule also does the daemon's upkeep (Housekeep): it refreshes
// retrieval stats, latches utility ranking, prunes old tool events, reaps idle
// sessions and settles moot proposals. Upkeep runs whatever gardener.enabled
// says; the proposal passes (Propose) run only while it is on, on the schedule
// or when the owner asks for one (RunNow).
package gardener

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// Default pass parameters, used when a Config field is non-positive.
const (
	defaultDedupThreshold = 0.88
	defaultStalenessDays  = 90
	defaultDigestDays     = 30
	defaultInterval       = time.Hour
	// minDigestSessions is the fewest completed sessions in a project's window
	// worth rolling into a digest (a single session is already its own finding).
	minDigestSessions = 2
)

// Config parameterizes the gardener passes.
type Config struct {
	// Propose runs the proposal passes on the schedule (gardener.enabled).
	// Upkeep (Housekeep) runs on the same schedule either way.
	Propose        bool
	DedupThreshold float64
	StalenessDays  int
	DigestDays     int
	Interval       time.Duration
	// ToolEventRetentionDays prunes transport-level Interactions events
	// (tool.call, hook.prompt, recall.miss, hook.error) older than this many
	// days on each pass. 0 disables pruning -- deliberately not defaulted,
	// since 0 means "keep forever".
	ToolEventRetentionDays int
	// StalePlanDays proposes abandoning captured Claude Code plans still in
	// draft/presented after this many days. 0 disables the pass -- deliberately
	// not defaulted, mirroring ToolEventRetentionDays.
	StalePlanDays int
	// StaleStageDays proposes archiving stage memories whose Status header is
	// not a live gate (done, missing, or unrecognized) after this many days
	// without an update. 0 disables the pass -- same contract as StalePlanDays.
	StaleStageDays int
	// SessionIdle is the no-activity age past which the reaper expires an
	// active session that holds no live claim. Non-positive falls back to
	// core.SessionIdleTTL, keeping the reaper aligned with the console's
	// live/idle derivation (core.Session.LiveAsOf).
	SessionIdle time.Duration
	// LocalHost is this daemon's own machine name (config.Hostname). The
	// stale-plan pass opens repositories on disk for its ship evidence, so it
	// reads only the mappings THIS machine made: a remote device's repo path
	// either is not here, or is a different repository with the same path.
	LocalHost string
	// AgentsMayApply and AutoApply are the file/env values of the gardener's
	// two switches (gardener.agents_may_apply, gardener.auto_apply). They are
	// only the BASE: Switches layers the console's stored override over them
	// each time a check decides.
	AgentsMayApply *bool
	AutoApply      []string
}

// withDefaults fills non-positive fields from the package defaults.
func (c Config) withDefaults() Config {
	if c.DedupThreshold <= 0 {
		c.DedupThreshold = defaultDedupThreshold
	}
	if c.StalenessDays <= 0 {
		c.StalenessDays = defaultStalenessDays
	}
	if c.DigestDays <= 0 {
		c.DigestDays = defaultDigestDays
	}
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.SessionIdle <= 0 {
		c.SessionIdle = core.SessionIdleTTL
	}
	return c
}

// FromConfig adapts the server config's gardener block to the pass Config.
func FromConfig(g config.Gardener) Config {
	return Config{
		Propose:                g.Enabled,
		DedupThreshold:         g.DedupThreshold,
		StalenessDays:          g.StalenessDays,
		DigestDays:             g.DigestDays,
		Interval:               time.Duration(g.IntervalMinutes) * time.Minute,
		ToolEventRetentionDays: g.ToolEventRetentionDays,
		StalePlanDays:          g.StalePlanDays,
		StaleStageDays:         g.StaleStageDays,
		SessionIdle:            time.Duration(g.SessionIdleMinutes) * time.Minute,
		LocalHost:              config.Hostname(),
		AgentsMayApply:         g.AgentsMayApply,
		AutoApply:              slices.Clone(g.AutoApply),
	}
}

// Service runs the gardener passes over one store.
type Service struct {
	db       *sql.DB
	files    *files.Manager
	embedder llm.Embedder // nil => dedup pass is skipped (no vectors to compare)
	chat     llm.Chat     // nil => digest pass is skipped (no summarizer)
	events   *events.Recorder
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time // injectable clock (tests)
	done     chan struct{}    // closed when the Start goroutine exits; nil until Start
	// transcripts settles flagged sessions' transcript copies after the
	// reaper's pass (SetTranscripts); nil settles none.
	transcripts TranscriptKeeper

	// The schedule (ticker.go). firstDelay is the wait before the first tick
	// (initialDelay; tests shorten it); kick carries the owner's RunNow to the
	// loop, nil until Start; scheduled is whether the loop is running (under
	// mu).
	firstDelay time.Duration
	kick       chan runRequest
	scheduled  bool
	// beforePasses runs at the start of every proposal pass (tests hold a pass
	// open with it); nil in production.
	beforePasses func(context.Context)
	// guard is what automatic apply asks before it applies a new proposal by
	// itself (SetAutoGuard, auto.go); the zero value asks nothing.
	guard AutoGuard

	// mu guards the pass bookkeeping the status line reads (Status).
	mu sync.Mutex
	// pass is the proposal pass running or queued now, nil when none is.
	pass *Run
	// last is the latest proposal pass to finish, nil before the first.
	last *Run
	// next is when the schedule's next tick is due; housekept is when upkeep
	// last finished. Both zero until the loop gets there.
	next, housekept time.Time
}

// TranscriptKeeper is what the reaper needs of the session flag's transcript
// copies (*transcripts.Service): KeepEnded settles the flagged sessions that
// ended before cutoff without a copy, their end signal never having made one
// -- copied where the daemon reads the file, pending for the agent's later
// hooks under peer_auth.
type TranscriptKeeper interface {
	KeepEnded(ctx context.Context, cutoff time.Time)
}

// SetTranscripts hands the reaper the transcript copies to settle after each
// pass. Call it before Start.
func (s *Service) SetTranscripts(t TranscriptKeeper) { s.transcripts = t }

// New builds a gardener Service. embedder and chat may each be nil, disabling
// the pass that needs them (dedup and digest respectively); staleness always
// runs. events may be nil (proposal telemetry is then skipped).
func New(db *sql.DB, mgr *files.Manager, embedder llm.Embedder, chat llm.Chat, rec *events.Recorder, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db: db, files: mgr, embedder: embedder, chat: chat, events: rec,
		cfg: cfg.withDefaults(), logger: logger, now: time.Now, firstDelay: initialDelay,
	}
}

// Proposing reports whether the proposal passes run on the schedule
// (gardener.enabled). Upkeep runs either way.
func (s *Service) Proposing() bool { return s.cfg.Propose }

// Interval is the schedule's period: upkeep, and the proposal passes when they
// are on, run this often.
func (s *Service) Interval() time.Duration { return s.cfg.Interval }

// Pass names, as PassResult.Failed and PassResult.Skipped spell them.
const (
	PassDedup        = "dedup"
	PassStaleness    = "staleness"
	PassStaleStage   = "stale-stage"
	PassDeadWeight   = "dead-weight"
	PassStaleCopy    = "stale-copy"
	PassDigest       = "digest"
	PassStalePlan    = "stale-plan"
	PassMemoryWanted = "memory-wanted"
	PassToolError    = "tool-error"
	PassCopyRelocate = "copy-relocate"
)

// SkippedPass is a pass that did not run, and why, in the owner's words.
type SkippedPass struct {
	Pass   string `json:"pass"`
	Reason string `json:"reason"`
}

// Skips lists the passes this service cannot run, or was told not to, in run
// order: dedup without an embedder, digest without a chat model, and a pass
// its config turns off. A skipped pass proposes nothing, which looks exactly
// like a pass that found nothing; this is what tells them apart before a pass
// has run.
func (s *Service) Skips() []SkippedPass {
	var out []SkippedPass
	for _, name := range []string{PassDedup, PassStaleStage, PassDigest, PassStalePlan} {
		if why := s.skipReason(name); why != "" {
			out = append(out, SkippedPass{Pass: name, Reason: why})
		}
	}
	return out
}

// skipReason is why the named pass cannot run, "" when it can. Each pass also
// guards itself (a pass called directly finds nothing); this is the reason
// the run records.
func (s *Service) skipReason(name string) string {
	switch name {
	case PassDedup:
		if s.embedder == nil {
			return "no embeddings"
		}
	case PassDigest:
		if s.chat == nil {
			return "no chat model"
		}
	case PassStalePlan:
		if s.cfg.StalePlanDays <= 0 {
			return "off in the config"
		}
	case PassStaleStage:
		if s.cfg.StaleStageDays <= 0 {
			return "off in the config"
		}
	}
	return ""
}

// PassResult counts what a single proposal pass (Propose) produced.
type PassResult struct {
	Merges       int `json:"merges"`
	Archives     int `json:"archives"`
	Digests      int `json:"digests"`
	StalePlans   int `json:"stalePlans"`
	MemoryWanted int `json:"memoryWanted"`
	ToolError    int `json:"toolError"`
	// Relocations counts the copy-relocate pass's relocate proposals (a stale
	// copy's archive proposal counts under Archives).
	Relocations int `json:"relocations"`
	// AutoApplied counts the new proposals this pass applied by itself
	// (gardener.auto_apply, auto.go), and KeptForReview the new ones of an
	// enabled kind it left pending for the owner instead. Both are among the
	// counts above: a proposal applied by itself was still raised.
	AutoApplied   int `json:"autoApplied,omitempty"`
	KeptForReview int `json:"keptForReview,omitempty"`
	// Failed names the passes that errored, in run order. A failing pass leaves
	// its count at 0, which is shaped exactly like "nothing to propose"; this is
	// the only thing that tells the two apart. Read it before trusting a zero.
	Failed []string `json:"failed,omitempty"`
	// Skipped names the passes that did not run, and why (Skips), in run
	// order. A skipped pass's zero is no finding either.
	Skipped []SkippedPass `json:"skipped,omitempty"`
}

// Total is the number of proposals created across all passes.
func (r PassResult) Total() int {
	return r.Merges + r.Archives + r.Digests + r.StalePlans + r.MemoryWanted + r.ToolError + r.Relocations
}

// OK reports whether every pass ran to completion. A zero Total means "nothing
// to propose" only when OK is true.
func (r PassResult) OK() bool { return len(r.Failed) == 0 }

// Housekeep is the daemon's upkeep, run on the schedule whether or not the
// proposal passes are on: it refreshes retrieval stats, latches utility
// ranking, prunes old tool events, reaps idle sessions and settles the pending
// proposals that went moot. Each step is best-effort: a failure is logged and
// the next step runs.
func (s *Service) Housekeep(ctx context.Context) {
	// Staleness reads last-injected/last-read from retrieval_stats, a projection
	// of the event log, and so do the console and usage_summary; refresh it
	// first so the latch below and a proposal pass after it see current
	// activity.
	s.refreshStats(ctx)

	// Latch utility ranking on for projects whose demand history just matured;
	// runs against the same event state the stats above were built from.
	s.evaluateUtilityActivation(ctx)

	// Bound the event log: prune transport-level Interactions events past the
	// retention window. It never touches domain events, so retrieval stats
	// (built above) are unaffected.
	s.pruneToolEvents(ctx)

	// Reap sessions that went idle without a graceful session_end (crashed agents,
	// explicit session_starts whose agent never ended them). Keeps the "active" set
	// honest -- the only reliable close for these, since no hook fires on a kill.
	s.reapStaleSessions(ctx)

	// Settle the pending proposals whose memory went inactive by another route
	// since they were raised, so the queue only holds what can still be applied.
	// The gardener decides these itself.
	s.settleMootProposals(WithActor(ctx, ByGardener))

	s.mu.Lock()
	s.housekept = s.now().UTC()
	s.mu.Unlock()
}

// refreshStats rebuilds retrieval_stats from the event log, best-effort.
func (s *Service) refreshStats(ctx context.Context) {
	if err := store.RebuildRetrievalStats(ctx, s.db); err != nil {
		s.logger.Warn("gardener: rebuild retrieval stats", "error", err)
	}
}

// Propose refreshes retrieval stats, then runs the proposal passes and returns
// what each created. A pass that fails is logged and named in Failed, one that
// cannot run is named in Skipped; neither aborts the others. It is the pass
// itself, with no bookkeeping: the schedule and RunNow run it through runPass,
// which records it for Status.
func (s *Service) Propose(ctx context.Context) (PassResult, error) {
	s.refreshStats(ctx)
	return s.propose(ctx)
}

// propose runs the proposal passes over stats a caller just refreshed.
func (s *Service) propose(ctx context.Context) (PassResult, error) {
	if s.beforePasses != nil {
		s.beforePasses(ctx)
	}
	existing, err := loadSeen(ctx, s.db)
	if err != nil {
		return PassResult{}, err
	}
	// Every proposal the passes raise is noted, so automatic apply takes this
	// pass's new ones and nothing older: an undone apply, which returns to the
	// queue, is never applied again by itself.
	ctx, created := withCreatedLog(ctx)

	var res PassResult
	// run records a failing pass by name instead of letting its zero count pass
	// for "nothing to propose", and a pass that cannot run by name and reason.
	run := func(name string, pass func(context.Context, seenKeys) (int, error)) int {
		if why := s.skipReason(name); why != "" {
			res.Skipped = append(res.Skipped, SkippedPass{Pass: name, Reason: why})
			return 0
		}
		n, err := pass(ctx, existing)
		if err != nil {
			s.logger.Warn("gardener: "+name+" pass", "error", err)
			res.Failed = append(res.Failed, name)
			return 0
		}
		return n
	}
	res.Merges = run(PassDedup, s.proposeMerges)
	res.Archives = run(PassStaleness, s.proposeArchives)
	// Stale-stage and dead-weight proposals are archive proposals too (same
	// kind, same key namespace, so the passes never double-propose one memory);
	// they fold into the Archives count and are told apart in Failed by name.
	res.Archives += run(PassStaleStage, s.proposeStaleStages)
	res.Archives += run(PassDeadWeight, s.proposeDeadWeight)
	// A stale copy's proposal is an archive proposal too, after the other
	// archive passes so a memory they asked about this run is not asked about
	// twice. It and copy-relocate read only memories that carry a provenance
	// stamp (isolation.requests): with no stamps they find nothing.
	res.Archives += run(PassStaleCopy, s.proposeStaleCopies)
	res.Digests = run(PassDigest, s.proposeDigests)
	res.StalePlans = run(PassStalePlan, s.proposeStalePlans)
	res.MemoryWanted = run(PassMemoryWanted, s.proposeMemoryWanted)
	res.ToolError = run(PassToolError, s.proposeToolError)
	res.Relocations = run(PassCopyRelocate, s.proposeSourceRelocations)
	// After every pass, so no pass reads a world an automatic apply changed
	// mid-run (a task opened for one gap reading as coverage for the next).
	res.AutoApplied, res.KeptForReview = s.autoApplyNew(ctx, created.list())

	switch {
	case !res.OK():
		// Every failure was warned above with its cause; this says how much of
		// the run's output to trust, which no single pass warning can convey.
		s.logger.Warn("gardener pass incomplete", "failed", res.Failed, "merges", res.Merges,
			"archives", res.Archives, "digests", res.Digests, "stale_plans", res.StalePlans,
			"memory_wanted", res.MemoryWanted, "tool_errors", res.ToolError, "relocations", res.Relocations)
	case res.Total() > 0:
		s.logger.Info("gardener pass complete", "merges", res.Merges, "archives", res.Archives,
			"digests", res.Digests, "stale_plans", res.StalePlans, "memory_wanted", res.MemoryWanted,
			"tool_errors", res.ToolError, "relocations", res.Relocations,
			"auto_applied", res.AutoApplied, "kept_for_review", res.KeptForReview)
	}
	return res, nil
}

// pruneToolEvents deletes transport-level Interactions events (tool.call,
// hook.prompt, recall.miss, hook.error) older than the retention window,
// keeping the event log bounded. Domain events are never touched. A
// non-positive retention or nil recorder disables it. Best-effort: a failure
// is logged, not returned.
func (s *Service) pruneToolEvents(ctx context.Context) {
	if s.events == nil || s.cfg.ToolEventRetentionDays <= 0 {
		return
	}
	cutoff := s.now().UTC().AddDate(0, 0, -s.cfg.ToolEventRetentionDays)
	n, err := s.events.PruneKinds(ctx, []core.EventKind{core.EventToolCall, core.EventHookPrompt, core.EventRecallMiss, core.EventHookError}, cutoff)
	if err != nil {
		s.logger.Warn("gardener: prune tool events", "error", err)
		return
	}
	if n > 0 {
		s.logger.Info("gardener pruned tool events", "deleted", n, "older_than_days", s.cfg.ToolEventRetentionDays)
	}
}

// reapStaleSessions closes the sessions that are no longer live
// (core.Session.LiveAsOf): quiet past the configured SessionIdle threshold
// (gardener.session_idle_minutes) and holding no live claim or repo lock. A
// session idle past the threshold while it holds a live one is left alone; the
// first pass after its last lease lapses reaps it. It flips each to expired,
// returns the lapsed task claims it still held to the queue and frees the
// repo locks it still held, with a notice to the session for each
// (store.ReleaseReapedClaims, store.ReleaseReapedRepoLocks), and records a
// session.ended event stamped reason=expired. Best-effort: a failure is logged, not returned, so a
// reap problem never aborts the pass. This is the backstop for the fact that an
// active session is only otherwise cleared by an explicit session_end or the
// SessionEnd hook, neither of which fires on a crash/kill.
//
// A flagged session among them, or any other that ended without its end
// signal making its transcript copy, is then settled (TranscriptKeeper): the
// daemon copies it where it may read the file, and under peer_auth it waits,
// pending, for the agent's later hooks to send it.
func (s *Service) reapStaleSessions(ctx context.Context) {
	now := s.now().UTC()
	stale, err := store.ExpireIdleSessions(ctx, s.db, now, s.cfg.SessionIdle)
	if err != nil {
		s.logger.Warn("gardener: reap stale sessions", "error", err)
		return
	}
	if s.transcripts != nil {
		// After the flips above, so the sessions just expired are among the
		// ended ones; the idle threshold gives a completed session's end signal
		// its time to come first.
		defer s.transcripts.KeepEnded(ctx, now.Add(-s.cfg.SessionIdle))
	}
	if len(stale) == 0 {
		return
	}
	for _, sess := range stale {
		released, rerr := store.ReleaseReapedClaims(ctx, s.db, sess.ID, now)
		if rerr != nil {
			s.logger.Warn("gardener: reap release claims", "session", sess.ID, "error", rerr)
		}
		locks, lerr := store.ReleaseReapedRepoLocks(ctx, s.db, sess.ID, now)
		if lerr != nil {
			s.logger.Warn("gardener: reap release repo locks", "session", sess.ID, "error", lerr)
		}
		payload := map[string]any{"reason": "expired", "reaped": true, "claims_released": released}
		if locks > 0 {
			payload["locks_released"] = locks
		}
		if s.events != nil {
			if _, eerr := s.events.Record(ctx, core.Event{
				Kind: core.EventSessionEnded, SessionID: sess.ID, ProjectSlug: sess.ProjectSlug,
				Payload: payload,
			}); eerr != nil {
				s.logger.Warn("gardener: reap record event", "session", sess.ID, "error", eerr)
			}
		}
	}
	s.logger.Info("gardener reaped idle sessions", "count", len(stale), "idle_ttl", s.cfg.SessionIdle)
}

// seenKeys is the suppression set a pass consults before proposing: payload key
// -> why that key is blocked. It exists because a rejection has two strengths.
// A hidden (or pending, or applied) key is blocked outright; a regularly
// dismissed one is blocked only until evidence for it recurs, which is what
// makes "dismiss" a decision about the evidence so far rather than a permanent
// verdict on the pattern.
type seenKeys map[string]store.ProposalBlock

// loadSeen reads the suppression set for the whole run.
func loadSeen(ctx context.Context, db *sql.DB) (seenKeys, error) {
	blocks, err := store.ProposalKeyBlocks(ctx, db)
	if err != nil {
		return nil, err
	}
	return seenKeys(blocks), nil
}

// blocked reports whether key is suppressed for a pass whose proposals carry no
// evidence clock. Merge, archive, digest and the request-driven kinds are all
// like this: their key already encodes the content, so a changed world mints a
// new key rather than re-raising the old one, and any block at all holds.
func (s seenKeys) blocked(key string) bool { return s.blockedSince(key, time.Time{}) }

// blockedSince is blocked for a pass that DOES have an evidence clock -- the
// error/gap passes, whose key is the pattern's stable identity and whose
// evidence keeps accumulating under it. A hard block still holds; a regular
// dismissal lapses once the group's newest evidence postdates it, so a pattern
// that keeps happening is raised again rather than silently accruing forever.
func (s seenKeys) blockedSince(key string, evidence time.Time) bool {
	b, ok := s[key]
	if !ok {
		return false
	}
	if b.Hard {
		return true
	}
	return !evidence.After(b.DismissedAt)
}

// mark blocks the key for the rest of the run, so one pass cannot propose the
// same thing twice and a later pass sharing the key namespace (the three
// archive passes) cannot double-propose one memory.
func (s seenKeys) mark(key string) { s[key] = store.ProposalBlock{Hard: true} }

// createProposal persists a proposal and records a gardener.action event. The
// key is marked seen so a single run does not propose the same thing twice.
// It returns the new proposal's id.
func (s *Service) createProposal(ctx context.Context, kind, key string, payload map[string]any, seen seenKeys) (string, error) {
	payload["key"] = key
	p, err := store.CreateProposal(ctx, s.db, kind, payload)
	if err != nil {
		return "", err
	}
	seen.mark(key)
	s.record(ctx, p.ID, map[string]any{"action": "propose", "kind": kind, "key": key})
	noteCreated(ctx, p.ID, kind)
	return p.ID, nil
}

// record appends a gardener.action event best-effort. When ctx names who is
// deciding (WithActor), the payload carries it as by (eventBy).
func (s *Service) record(ctx context.Context, itemID string, payload map[string]any) {
	if s.events == nil {
		return
	}
	if by := ActorOf(ctx); by != "" {
		payload["by"] = eventBy(by)
	}
	if _, err := s.events.Record(ctx, core.Event{
		Kind: core.EventGardenerAction, ItemID: itemID, Payload: payload,
	}); err != nil {
		s.logger.Warn("gardener: record event", "error", err)
	}
}
