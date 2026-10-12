// Package gardener runs the propose-only maintenance passes over the memory
// corpus: it finds near-duplicate memories (dedup), memories that have gone
// untouched for too long (staleness), stage memories that stopped carrying a
// live gate (stale-stage), memories briefings keep injecting without any
// demand (dead-weight), captured plans that were never approved (stale-plan),
// recall queries that keep missing because nobody wrote the memory
// (memory-wanted), errors agents keep hitting on tool calls or hook stages
// (tool-error), and rolls recent sessions into a monthly digest. Every pass
// only ever writes a gardener_proposals row for the owner to apply or dismiss
// -- it never mutates a memory on its own. The passes run on a ticker and are
// also invokable on demand (RunOnce).
package gardener

import (
	"context"
	"database/sql"
	"log/slog"
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
	// active session. Non-positive falls back to core.SessionIdleTTL, keeping
	// the reaper cutoff aligned with the console's live/idle derivation.
	SessionIdle time.Duration
	// LocalHost is this daemon's own machine name (config.Hostname). The
	// stale-plan pass opens repositories on disk for its ship evidence, so it
	// reads only the mappings THIS machine made: a remote device's repo path
	// either is not here, or is a different repository with the same path.
	LocalHost string
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
		DedupThreshold:         g.DedupThreshold,
		StalenessDays:          g.StalenessDays,
		DigestDays:             g.DigestDays,
		Interval:               time.Duration(g.IntervalMinutes) * time.Minute,
		ToolEventRetentionDays: g.ToolEventRetentionDays,
		StalePlanDays:          g.StalePlanDays,
		StaleStageDays:         g.StaleStageDays,
		SessionIdle:            time.Duration(g.SessionIdleMinutes) * time.Minute,
		LocalHost:              config.Hostname(),
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
}

// New builds a gardener Service. embedder and chat may each be nil, disabling
// the pass that needs them (dedup and digest respectively); staleness always
// runs. events may be nil (proposal telemetry is then skipped).
func New(db *sql.DB, mgr *files.Manager, embedder llm.Embedder, chat llm.Chat, rec *events.Recorder, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db: db, files: mgr, embedder: embedder, chat: chat, events: rec,
		cfg: cfg.withDefaults(), logger: logger, now: time.Now,
	}
}

// PassResult counts what a single RunOnce produced.
type PassResult struct {
	Merges       int `json:"merges"`
	Archives     int `json:"archives"`
	Digests      int `json:"digests"`
	StalePlans   int `json:"stalePlans"`
	MemoryWanted int `json:"memoryWanted"`
	ToolError    int `json:"toolError"`
	// Failed names the passes that errored, in run order. A failing pass leaves
	// its count at 0, which is shaped exactly like "nothing to propose"; this is
	// the only thing that tells the two apart. Read it before trusting a zero.
	Failed []string `json:"failed,omitempty"`
}

// Total is the number of proposals created across all passes.
func (r PassResult) Total() int {
	return r.Merges + r.Archives + r.Digests + r.StalePlans + r.MemoryWanted + r.ToolError
}

// OK reports whether every pass ran to completion. A zero Total means "nothing
// to propose" only when OK is true.
func (r PassResult) OK() bool { return len(r.Failed) == 0 }

// RunOnce refreshes retrieval stats, then runs the propose-only passes and
// returns what each created. A pass that fails is logged and skipped; a single
// failing pass never aborts the others.
func (s *Service) RunOnce(ctx context.Context) (PassResult, error) {
	// Staleness reads last-injected/last-read from retrieval_stats, a projection
	// of the event log; refresh it first so this pass sees current activity.
	if err := store.RebuildRetrievalStats(ctx, s.db); err != nil {
		s.logger.Warn("gardener: rebuild retrieval stats", "error", err)
	}

	// Latch utility ranking on for projects whose demand history just matured;
	// runs against the same event state the stats above were built from.
	s.evaluateUtilityActivation(ctx)

	// Bound the event log: prune transport-level Interactions events past the
	// retention window. Runs before the stat-independent passes and never touches
	// domain events, so retrieval stats (built above) are unaffected.
	s.pruneToolEvents(ctx)

	// Reap sessions that went idle without a graceful session_end (crashed agents,
	// explicit session_starts whose agent never ended them). Keeps the "active" set
	// honest -- the only reliable close for these, since no hook fires on a kill.
	s.reapStaleSessions(ctx)

	// Settle the pending proposals whose memory went inactive by another route
	// since they were raised, so the queue only holds what can still be applied.
	s.settleMootProposals(ctx)

	existing, err := loadSeen(ctx, s.db)
	if err != nil {
		return PassResult{}, err
	}

	var res PassResult
	// run records a failing pass by name instead of letting its zero count pass
	// for "nothing to propose".
	run := func(name string, pass func(context.Context, seenKeys) (int, error)) int {
		n, err := pass(ctx, existing)
		if err != nil {
			s.logger.Warn("gardener: "+name+" pass", "error", err)
			res.Failed = append(res.Failed, name)
			return 0
		}
		return n
	}
	res.Merges = run("dedup", s.proposeMerges)
	res.Archives = run("staleness", s.proposeArchives)
	// Stale-stage and dead-weight proposals are archive proposals too (same
	// kind, same key namespace, so the passes never double-propose one memory);
	// they fold into the Archives count and are told apart in Failed by name.
	res.Archives += run("stale-stage", s.proposeStaleStages)
	res.Archives += run("dead-weight", s.proposeDeadWeight)
	res.Digests = run("digest", s.proposeDigests)
	res.StalePlans = run("stale-plan", s.proposeStalePlans)
	res.MemoryWanted = run("memory-wanted", s.proposeMemoryWanted)
	res.ToolError = run("tool-error", s.proposeToolError)

	switch {
	case !res.OK():
		// Every failure was warned above with its cause; this says how much of
		// the run's output to trust, which no single pass warning can convey.
		s.logger.Warn("gardener pass incomplete", "failed", res.Failed, "merges", res.Merges,
			"archives", res.Archives, "digests", res.Digests, "stale_plans", res.StalePlans,
			"memory_wanted", res.MemoryWanted, "tool_errors", res.ToolError)
	case res.Total() > 0:
		s.logger.Info("gardener pass complete", "merges", res.Merges, "archives", res.Archives,
			"digests", res.Digests, "stale_plans", res.StalePlans, "memory_wanted", res.MemoryWanted,
			"tool_errors", res.ToolError)
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

// reapStaleSessions closes sessions idle past the configured SessionIdle
// threshold (gardener.session_idle_minutes): it flips each
// to expired, returns any task claims it still held to the queue, and records a
// session.ended event stamped reason=expired. Best-effort: a failure is logged,
// not returned, so a reap problem never aborts the pass. This is the backstop for
// the fact that an active session is only otherwise cleared by an explicit
// session_end or the SessionEnd hook, neither of which fires on a crash/kill.
func (s *Service) reapStaleSessions(ctx context.Context) {
	now := s.now().UTC()
	cutoff := now.Add(-s.cfg.SessionIdle)
	stale, err := store.ExpireStaleSessions(ctx, s.db, cutoff)
	if err != nil {
		s.logger.Warn("gardener: reap stale sessions", "error", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	for _, sess := range stale {
		released, rerr := store.ReleaseClaimsForSession(ctx, s.db, sess.ID, now)
		if rerr != nil {
			s.logger.Warn("gardener: reap release claims", "session", sess.ID, "error", rerr)
		}
		if s.events != nil {
			if _, eerr := s.events.Record(ctx, core.Event{
				Kind: core.EventSessionEnded, SessionID: sess.ID, ProjectSlug: sess.ProjectSlug,
				Payload: map[string]any{"reason": "expired", "reaped": true, "claims_released": released},
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
	return p.ID, nil
}

// record appends a gardener.action event best-effort.
func (s *Service) record(ctx context.Context, itemID string, payload map[string]any) {
	if s.events == nil {
		return
	}
	if _, err := s.events.Record(ctx, core.Event{
		Kind: core.EventGardenerAction, ItemID: itemID, Payload: payload,
	}); err != nil {
		s.logger.Warn("gardener: record event", "error", err)
	}
}
