// Package demokit is the shared seeding core that fills a THROWAWAY Seamless
// data dir with backdated fixtures. It backs the demoseed CLI (the console-fleet
// history for landing-page screenshots, the -review fixture layered on it, and
// the -scenes terminal-scene fixture) and is importable so other tooling -- the agent-scenario benchmark's scenario
// seeder -- can build fixture state in Go without shelling out or reimplementing
// the time-backdating logic. Point it only at a fresh dir while no daemon holds
// the DB (single-writer SQLite).
//
// Timestamps are backdated through the store/files/events APIs, which write
// caller-supplied times verbatim; nothing here goes through MCP (which stamps
// time.Now). Deterministic apart from the anchor "now".
package demokit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

const (
	historicalSessions = 209 // + 3 live = 212, matching the landing-page sketch
	daysOfHistory      = 42
)

type Seeder struct {
	ctx  context.Context
	db   *sql.DB
	mgr  *files.Manager
	rec  *events.Recorder
	rng  *rand.Rand
	now  time.Time
	used map[string]bool // session name uniqueness

	injected map[string]bool // memory ids that have surfaced at least once
}

type sessRec struct {
	id, name, project string
	start, end        time.Time
	live              bool
}

type memRec struct {
	id, project, kind, name string
	created                 time.Time
	hot                     int
}

// New opens the throwaway data dir -- creating the memory/ and notes/ subdirs
// and the SQLite DB -- and returns a Seeder ready to seed. The RNG is seeded
// from a fixed constant and "now" is captured once, so a run is deterministic
// apart from the wall-clock anchor. Never point this at a live instance.
func New(dataDir string) (*Seeder, error) {
	ctx := context.Background()
	for _, sub := range []string{"memory", "notes"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("demokit: mkdir %s: %w", sub, err)
		}
	}
	db, err := store.Open(filepath.Join(dataDir, "seam.db"))
	if err != nil {
		return nil, fmt.Errorf("demokit: open db: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr, err := files.NewManager(dataDir, db, logger)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("demokit: files manager: %w", err)
	}
	return &Seeder{
		ctx:      ctx,
		db:       db,
		mgr:      mgr,
		rec:      events.NewRecorder(db),
		rng:      rand.New(rand.NewSource(42)),
		now:      time.Now().UTC(),
		used:     map[string]bool{},
		injected: map[string]bool{},
	}, nil
}

// Close releases the seeder's database handle.
func (s *Seeder) Close() error { return s.db.Close() }

// SeedConsoleFleet seeds the fictional six-week console fleet history (the
// branding surface: landing-page screenshots) and prints a summary. This is the
// demoseed default mode.
func (s *Seeder) SeedConsoleFleet() {
	s.consoleFleet()
	s.summary()
}

// consoleFleet seeds the console fleet history: SeedConsoleFleet without its
// summary, and the base SeedReview lays its layer on.
func (s *Seeder) consoleFleet() {
	s.projects()
	sessions := s.sessions()
	mems := s.memories(sessions)
	s.superseded(sessions, mems)
	s.injections(sessions, mems)
	s.notes(sessions)
	s.plansAndTasks(sessions)
	s.trials(sessions)
	s.gardener(mems)
	s.recentBurst(sessions, mems)
	s.coverage(sessions, mems)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Seeder) IdAt(t time.Time) string {
	id, err := ulid.New(ulid.Timestamp(t.UTC()), s.rng)
	if err != nil {
		log.Fatalf("demoseed: mint ulid: %v", err)
	}
	return id.String()
}

func (s *Seeder) DaysAgo(d int) time.Time { return s.now.AddDate(0, 0, -d) }

func (s *Seeder) Event(t time.Time, kind core.EventKind, sessionID, project, itemID string, payload map[string]any) {
	if _, err := s.rec.Record(s.ctx, core.Event{
		TS: t.UTC(), Kind: kind, SessionID: sessionID, ProjectSlug: project,
		ItemID: itemID, Payload: payload,
	}); err != nil {
		log.Fatalf("demoseed: record %s: %v", kind, err)
	}
}

func (s *Seeder) ToolCall(t time.Time, sessionID, project, tool string, args map[string]any, result string, durMS int) {
	s.Event(t, core.EventToolCall, sessionID, project, "", map[string]any{
		"tool": tool, "args": args, "result": result, "duration_ms": durMS, "is_error": false,
	})
}

func (s *Seeder) hexName() string {
	for {
		n := fmt.Sprintf("cc/%08x", s.rng.Uint32())
		if !s.used[n] {
			s.used[n] = true
			return n
		}
	}
}

func Pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.Intn(len(xs))] }

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

func (s *Seeder) projects() {
	specs := []struct {
		slug, name, desc string
		daysAgo          int
	}{
		{"orbital", "orbital", "Go SaaS backend: payments, webhooks, public API", 42},
		{"orbital-web", "orbital-web", "Next.js frontend for orbital", 40},
		{"homelab", "homelab", "Proxmox cluster, backups, network", 38},
		{"dotfiles", "dotfiles", "Shell, editor, and machine setup", 36},
	}
	for _, p := range specs {
		t := s.DaysAgo(p.daysAgo)
		if err := store.CreateProject(s.ctx, s.db, core.Project{
			ID: s.IdAt(t), Slug: p.slug, Name: p.name, Description: p.desc,
			CreatedAt: t, UpdatedAt: t,
		}); err != nil {
			log.Fatalf("demoseed: project %s: %v", p.slug, err)
		}
	}
	if err := store.SetProjectParent(s.ctx, s.db, "orbital-web", "orbital", s.DaysAgo(40)); err != nil {
		log.Fatalf("demoseed: parent: %v", err)
	}
	if err := store.SetProjectFamilies(s.ctx, s.db, map[string][]string{
		"orbital-suite": {"orbital", "orbital-web"},
	}); err != nil {
		log.Fatalf("demoseed: family: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

func (s *Seeder) sessions() []sessRec {
	var out []sessRec
	projWeights := []string{
		"orbital", "orbital", "orbital", "orbital", "orbital",
		"orbital-web", "orbital-web", "orbital-web",
		"homelab", "homelab", "dotfiles",
	}
	sources := []string{"startup", "startup", "startup", "startup", "startup", "startup", "startup", "resume", "resume", "clear", "explicit"}

	for day := daysOfHistory; day >= 1; day-- {
		date := s.DaysAgo(day)
		ramp := (daysOfHistory - day) / 9 // 0..4: the fleet ramps up over the weeks
		var n int
		if wd := date.Weekday(); wd == time.Saturday || wd == time.Sunday {
			n = s.rng.Intn(3)
		} else {
			n = 2 + ramp/2 + s.rng.Intn(4)
		}
		for i := 0; i < n; i++ {
			start := time.Date(date.Year(), date.Month(), date.Day(), 8+s.rng.Intn(14), s.rng.Intn(60), s.rng.Intn(60), 0, time.UTC)
			out = append(out, s.makeSession(start, Pick(s.rng, projWeights), Pick(s.rng, sources)))
		}
	}
	// Trim or pad to the exact historical count.
	for len(out) > historicalSessions {
		i := s.rng.Intn(len(out) / 2) // drop from the older half
		out = append(out[:i], out[i+1:]...)
	}
	for len(out) < historicalSessions {
		day := 1 + s.rng.Intn(10)
		date := s.DaysAgo(day)
		start := time.Date(date.Year(), date.Month(), date.Day(), 8+s.rng.Intn(14), s.rng.Intn(60), 0, 0, time.UTC)
		out = append(out, s.makeSession(start, Pick(s.rng, projWeights), "startup"))
	}

	for i := range out {
		s.persistSession(&out[i], false)
	}

	// Three live sessions for the "right now" story: two claiming plan/tasks,
	// one that just started. Claim wiring happens in plansAndTasks.
	live := []struct {
		name, project string
		startedAgo    time.Duration
	}{
		{"cc/7c31a9e2", "orbital", 25 * time.Minute},     // replay-protection step
		{"cc/f3d19b2c", "orbital", 40 * time.Minute},     // signing key rotation
		{"cc/9d24e7b0", "orbital-web", 65 * time.Minute}, // token migration
	}
	for _, l := range live {
		s.used[l.name] = true
		r := sessRec{
			name: l.name, project: l.project,
			start: s.now.Add(-l.startedAgo), end: s.now.Add(-time.Duration(2+s.rng.Intn(6)) * time.Minute),
			live: true,
		}
		r.id = s.IdAt(r.start)
		s.persistSession(&r, true)
		out = append(out, r)
	}
	return out
}

func (s *Seeder) makeSession(start time.Time, project, source string) sessRec {
	r := sessRec{
		name: s.hexName(), project: project,
		start: start, end: start.Add(time.Duration(15+s.rng.Intn(105)) * time.Minute),
	}
	r.id = s.IdAt(r.start)
	_ = source // recorded in persistSession via metadata-free fields; kept simple
	return r
}

func (s *Seeder) persistSession(r *sessRec, live bool) {
	status := core.SessionCompleted
	findings := ""
	updated := r.end
	switch {
	case live:
		status = core.SessionActive
	case s.rng.Float64() < 0.12:
		status = core.SessionExpired
	default:
		if s.rng.Float64() < 0.85 {
			findings = Pick(s.rng, findingsPool)
		}
	}
	sess := core.Session{
		ID: r.id, Name: r.name, ProjectSlug: r.project, Status: status,
		Findings: findings, ExternalSessionID: fmt.Sprintf("%08x-%04x-%04x", s.rng.Uint32(), s.rng.Uint32()&0xffff, s.rng.Uint32()&0xffff),
		CWD: "/home/dev/" + r.project, Source: "startup", Ambient: s.rng.Float64() < 0.85,
		CreatedAt: r.start, UpdatedAt: updated,
	}
	if err := store.CreateSession(s.ctx, s.db, sess); err != nil {
		log.Fatalf("demoseed: session %s: %v", r.name, err)
	}
	s.Event(r.start, core.EventSessionStarted, r.id, r.project, "", map[string]any{
		"name": r.name, "ambient": sess.Ambient, "source": "startup",
	})
	if !live && status == core.SessionCompleted {
		s.Event(r.end, core.EventSessionEnded, r.id, r.project, "", map[string]any{
			"findings": findings,
		})
	}
}

// ---------------------------------------------------------------------------
// memories
// ---------------------------------------------------------------------------

// sessionFor picks a session matching project (any project for global "") whose
// window the item can plausibly fall into.
func (s *Seeder) sessionFor(sessions []sessRec, project string, notBefore int) sessRec {
	for tries := 0; tries < 200; tries++ {
		c := sessions[s.rng.Intn(len(sessions))]
		if c.live {
			continue
		}
		if project != "" && c.project != project {
			continue
		}
		if notBefore > 0 && c.start.After(s.DaysAgo(notBefore)) {
			continue
		}
		return c
	}
	return sessions[0]
}

// pinnedMemories anchor the "right now" story: memories written minutes ago by
// the live claiming sessions keep project workspaces and plan timelines tied to
// the agents actively executing them.
var pinnedMemories = map[string]struct {
	sess string
	ago  time.Duration
}{
	"dedupe-store-schema-decision":        {"cc/7c31a9e2", 18 * time.Minute},
	"replay-dedupe-event-id-unique-index": {"cc/7c31a9e2", 12 * time.Minute},
	"dedupe-backfill-runbook":             {"cc/7c31a9e2", 6 * time.Minute},
	"tokens-ts-consts-pattern":            {"cc/9d24e7b0", 30 * time.Minute},
	"design-tokens-refresh-generate-done": {"cc/9d24e7b0", 50 * time.Minute},
}

func (s *Seeder) memories(sessions []sessRec) []memRec {
	byName := map[string]sessRec{}
	for _, r := range sessions {
		byName[r.name] = r
	}
	out := make([]memRec, 0, len(memSpecs))
	for _, m := range memSpecs {
		sess := s.sessionFor(sessions, m.project, 0)
		created := sess.start.Add(time.Duration(s.rng.Intn(30)+2) * time.Minute)
		if pin, ok := pinnedMemories[m.name]; ok {
			sess = byName[pin.sess]
			created = s.now.Add(-pin.ago)
		}
		updated := created
		body := m.body
		if body == "" {
			body = m.desc + "\n\nCaptured during a working session; see the source session for the full context."
		}
		id := s.IdAt(created)
		mem := core.Memory{
			ID: id, Kind: core.MemoryKind(m.kind), Name: m.name, Description: m.desc,
			Project: m.project, Body: body, Created: created, Updated: updated,
			ValidFrom: created, SourceSession: sess.name,
		}
		// A quarter of memories were touched again later by another session.
		if _, pinned := pinnedMemories[m.name]; !pinned && s.rng.Float64() < 0.25 {
			later := s.sessionFor(sessions, m.project, 0)
			if later.start.After(created) {
				mem.Updated = later.start.Add(10 * time.Minute)
				s.Event(mem.Updated, core.EventMemoryWritten, later.id, m.project, id, map[string]any{
					"name": m.name, "kind": m.kind, "project": m.project,
				})
			}
		}
		if _, err := s.mgr.WriteMemory(s.ctx, mem); err != nil {
			log.Fatalf("demoseed: memory %s: %v", m.name, err)
		}
		s.Event(created, core.EventMemoryWritten, sess.id, m.project, id, map[string]any{
			"name": m.name, "kind": m.kind, "project": m.project,
		})
		s.ToolCall(created.Add(500*time.Millisecond), sess.id, m.project, "memory_write",
			map[string]any{"name": m.name, "kind": m.kind}, "stored "+m.name, 8+s.rng.Intn(20))
		out = append(out, memRec{id: id, project: m.project, kind: m.kind, name: m.name, created: created, hot: m.hot})
	}
	return out
}

func (s *Seeder) superseded(sessions []sessRec, mems []memRec) {
	byName := map[string]memRec{}
	for _, m := range mems {
		byName[m.name] = m
	}
	for _, sp := range supersededSpecs {
		successor, ok := byName[sp.successor]
		if !ok {
			log.Fatalf("demoseed: superseded %s: successor %s missing", sp.name, sp.successor)
		}
		supAt := s.DaysAgo(sp.daysAgoSup)
		if sp.daysAgoSup == 0 {
			supAt = s.now.Add(-4 * time.Minute) // the live demo beat: superseded minutes ago
		}
		created := supAt.AddDate(0, 0, -(5 + s.rng.Intn(15)))
		sess := s.sessionFor(sessions, sp.project, 0)
		invalid := supAt
		mem := core.Memory{
			ID: s.IdAt(created), Kind: core.MemoryKind(sp.kind), Name: sp.name, Description: sp.desc,
			Project: sp.project, Body: sp.desc, Created: created, Updated: supAt,
			ValidFrom: created, InvalidAt: &invalid, SupersededBy: successor.id, SourceSession: sess.name,
		}
		if _, err := s.mgr.WriteMemory(s.ctx, mem); err != nil {
			log.Fatalf("demoseed: superseded %s: %v", sp.name, err)
		}
		s.Event(supAt, core.EventMemorySuperseded, "", sp.project, mem.ID, map[string]any{
			"name": sp.name, "superseded_by": sp.successor,
		})
	}
}

// ---------------------------------------------------------------------------
// injections + ambient traffic (retrieval health, interactions volume)
// ---------------------------------------------------------------------------

// hotPool returns the memories that ever get injected: the top-hotness slice,
// capped so distinct-injected matches the landing-page sketch (78 of 128).
func hotPool(mems []memRec) []memRec {
	var hot []memRec
	for _, m := range mems {
		if m.hot > 0 {
			hot = append(hot, m)
		}
	}
	slices.SortStableFunc(hot, func(a, b memRec) int { return b.hot - a.hot })
	if len(hot) > 78 {
		hot = hot[:78]
	}
	return hot
}

func (s *Seeder) pickInjected(pool []memRec, project string, before time.Time, n int) []string {
	var ids []string
	seen := map[string]bool{}
	for tries := 0; len(ids) < n && tries < 400; tries++ {
		m := pool[s.rng.Intn(len(pool))]
		if m.created.After(before) || seen[m.id] {
			continue
		}
		// Project affinity: same-project or global memories dominate a briefing.
		if m.project != project && m.project != "" && s.rng.Float64() < 0.8 {
			continue
		}
		if s.rng.Intn(10) >= m.hot {
			continue
		}
		seen[m.id] = true
		ids = append(ids, m.id)
	}
	return ids
}

func (s *Seeder) injections(sessions []sessRec, mems []memRec) {
	pool := hotPool(mems)
	fillers := []struct {
		tool   string
		result string
	}{
		{"tasks_ready", "2 ready"},
		{"tasks_list", "9 tasks"},
		{"notes_read", "note body returned"},
		{"session_update", "checkpoint saved"},
		{"project_list", "4 projects"},
		{"trial_query", "3 trials match"},
		{"usage_summary", "summary returned"},
	}

	injected := s.injected
	for _, sess := range sessions {
		// SessionStart briefing injection.
		ids := s.pickInjected(pool, sess.project, sess.start, 3+s.rng.Intn(6))
		if len(ids) > 0 {
			s.Event(sess.start.Add(2*time.Second), core.EventInjected, sess.id, sess.project, "", map[string]any{
				"hook": "SessionStart", "item_ids": ids, "content": Pick(s.rng, briefingPreviews),
			})
			for _, id := range ids {
				injected[id] = true
			}
		}
		dur := sess.end.Sub(sess.start)
		if dur <= 0 {
			dur = 20 * time.Minute
		}
		at := func() time.Time {
			return sess.start.Add(time.Duration(s.rng.Int63n(int64(dur))))
		}

		// Recall calls (tool.call + twin retrieval.injected with source=recall).
		for i := 0; i < s.rng.Intn(3); i++ {
			t := at()
			q := Pick(s.rng, recallQueries)
			rids := s.pickInjected(pool, sess.project, t, 2+s.rng.Intn(4))
			if len(rids) == 0 {
				continue
			}
			s.ToolCall(t, sess.id, sess.project, "recall",
				map[string]any{"query": q}, fmt.Sprintf("%d hits", len(rids)), 40+s.rng.Intn(160))
			s.Event(t.Add(100*time.Millisecond), core.EventInjected, sess.id, sess.project, "", map[string]any{
				"query": q, "item_ids": rids, "source": "recall",
			})
			for _, id := range rids {
				injected[id] = true
			}
		}

		// Prompt-matched injections and no-match prompts.
		if s.rng.Float64() < 0.35 {
			t := at()
			prompt := Pick(s.rng, promptsPool)
			if s.rng.Float64() < 0.5 {
				rids := s.pickInjected(pool, sess.project, t, 1+s.rng.Intn(3))
				if len(rids) > 0 {
					s.Event(t, core.EventInjected, sess.id, sess.project, "", map[string]any{
						"hook": "UserPromptSubmit", "prompt": prompt, "item_ids": rids,
					})
					for _, id := range rids {
						injected[id] = true
					}
				}
			} else {
				s.Event(t, core.EventHookPrompt, sess.id, sess.project, "", map[string]any{
					"hook": "UserPromptSubmit", "prompt": prompt,
				})
			}
		}

		// memory_read traffic.
		for i := 0; i < s.rng.Intn(3); i++ {
			t := at()
			ids := s.pickInjected(pool, sess.project, t, 1)
			if len(ids) == 0 {
				continue
			}
			var name string
			for _, m := range mems {
				if m.id == ids[0] {
					name = m.name
					break
				}
			}
			s.Event(t, core.EventMemoryRead, sess.id, sess.project, ids[0], map[string]any{"name": name})
			s.ToolCall(t, sess.id, sess.project, "memory_read",
				map[string]any{"name": name}, "body returned", 4+s.rng.Intn(10))
		}

		// Plain tool chatter.
		for i := 0; i < 1+s.rng.Intn(3); i++ {
			f := Pick(s.rng, fillers)
			s.ToolCall(at(), sess.id, sess.project, f.tool, map[string]any{}, f.result, 2+s.rng.Intn(18))
		}
	}

}

// coverage guarantees full hot-pool coverage as the LAST injection pass:
// anything hot that never surfaced gets one injection from a session that
// postdates its creation (live sessions cover the just-written pins).
func (s *Seeder) coverage(sessions []sessRec, mems []memRec) {
	pool := hotPool(mems)
	for _, m := range pool {
		if s.injected[m.id] {
			continue
		}
		var covered bool
		for tries := 0; tries < 400 && !covered; tries++ {
			r := sessions[s.rng.Intn(len(sessions))]
			at := r.start.Add(3 * time.Second)
			if r.live {
				at = s.now.Add(-time.Duration(1+s.rng.Intn(3)) * time.Minute)
			}
			if m.created.After(at) {
				continue
			}
			s.Event(at, core.EventInjected, r.id, r.project, "", map[string]any{
				"hook": "SessionStart", "item_ids": []string{m.id}, "content": Pick(s.rng, briefingPreviews),
			})
			s.injected[m.id] = true
			covered = true
		}
		if !covered {
			log.Printf("demoseed: WARNING: could not cover %s", m.name)
		}
	}
	log.Printf("demoseed: distinct memories injected: %d of %d active", len(s.injected), len(memSpecs))
}

// ---------------------------------------------------------------------------
// notes
// ---------------------------------------------------------------------------

func (s *Seeder) writeNote(sessions []sessRec, n noteSpec) core.Note {
	sess := s.sessionFor(sessions, n.project, n.daysAgo)
	created := s.DaysAgo(n.daysAgo).Add(time.Duration(s.rng.Intn(10)) * time.Hour)
	if n.daysAgo == 0 {
		created = s.now.Add(-time.Duration(5+s.rng.Intn(4)) * time.Hour)
	}
	body := n.body
	if body == "" {
		body = n.desc + "\n\nFull working notes captured with the session; kept as an artifact for the next agent."
	}
	note := core.Note{
		ID: s.IdAt(created), Title: n.title, Slug: n.slug, Description: n.desc,
		Project: n.project, Body: body, Tags: n.tags, Created: created, Updated: created,
	}
	if n.iter > 0 {
		note.Extra = map[string]any{"plan_iteration": n.iter}
	}
	if _, err := s.mgr.WriteNote(s.ctx, note); err != nil {
		log.Fatalf("demoseed: note %s: %v", n.slug, err)
	}
	s.Event(created, core.EventNoteWritten, sess.id, n.project, note.ID, map[string]any{
		"title": n.title, "slug": n.slug,
	})
	s.ToolCall(created.Add(time.Second), sess.id, n.project, "notes_create",
		map[string]any{"title": n.title}, "created "+n.slug, 10+s.rng.Intn(25))
	return note
}

func (s *Seeder) notes(sessions []sessRec) {
	for _, n := range planNarrativeNotes {
		s.writeNote(sessions, n)
	}
	for _, n := range miscNotes {
		s.writeNote(sessions, n)
	}
	for _, n := range agentCacheNotes {
		note := s.writeNote(sessions, n)
		s.Event(note.Created.Add(2*time.Second), core.EventSubagentCaptured, "", n.project, note.ID, map[string]any{
			"agent_type": "Plan", "prompt": "survey prior art for " + n.title, "content": n.desc,
		})
	}
	// Captured CC plans get their capture/present/approve event trail.
	for _, n := range ccPlanCaptureNotes {
		note := s.writeNote(sessions, n)
		base := planBasename(n)
		for it := 1; it <= n.iter; it++ {
			s.Event(note.Created.Add(time.Duration(it-n.iter)*45*time.Minute), core.EventPlanCaptured, "", n.project, note.ID, map[string]any{
				"basename": base, "iteration": it,
			})
		}
		s.Event(note.Created.Add(30*time.Minute), core.EventPlanPresented, "", n.project, note.ID, map[string]any{
			"basename": base,
		})
		for _, tag := range n.tags {
			if tag == "plan-status:approved" {
				s.Event(note.Created.Add(50*time.Minute), core.EventPlanApproved, "", n.project, note.ID, map[string]any{
					"basename": base,
				})
			}
		}
	}
}

func planBasename(n noteSpec) string {
	const prefix = "cc-plan-"
	if len(n.slug) > len(prefix) {
		return n.slug[len(prefix):] + ".md"
	}
	return n.slug + ".md"
}

// ---------------------------------------------------------------------------
// plans (step tasks) + standalone tasks
// ---------------------------------------------------------------------------

// liveSessionID finds one of the live sessions by name.
func liveSessionID(sessions []sessRec, name string) string {
	for _, r := range sessions {
		if r.name == name {
			return r.id
		}
	}
	log.Fatalf("demoseed: live session %s missing", name)
	return ""
}

func (s *Seeder) plansAndTasks(sessions []sessRec) {
	claimants := map[string]string{
		"billing-webhooks-v2":   liveSessionID(sessions, "cc/7c31a9e2"),
		"design-tokens-refresh": liveSessionID(sessions, "cc/9d24e7b0"),
	}
	for _, p := range planSpecs {
		var prevID string
		for _, st := range p.steps {
			created := s.DaysAgo(st.daysAgo).Add(time.Duration(9+s.rng.Intn(8)) * time.Hour)
			creator := s.sessionFor(sessions, p.project, st.daysAgo)
			t := core.Task{
				ID: s.IdAt(created), ProjectSlug: p.project, Title: st.title,
				Body:      "Step of plan:" + p.slug + ". See the plan narrative note for acceptance criteria.",
				Status:    core.TaskStatus(st.status),
				CreatedBy: creator.name, PlanSlug: p.slug,
				CreatedAt: created, UpdatedAt: created,
			}
			if st.depPrev && prevID != "" {
				t.DependsOn = []string{prevID}
			}
			switch st.status {
			case "done":
				closed := created.Add(time.Duration(4+s.rng.Intn(30)) * time.Hour)
				t.ClosedAt = &closed
				t.UpdatedAt = closed
			case "in_progress":
				if st.claimed {
					lease := s.now.Add(time.Duration(7+s.rng.Intn(8)) * time.Minute)
					t.ClaimedBy = claimants[p.slug]
					t.LeaseExpiresAt = &lease
					t.UpdatedAt = s.now.Add(-time.Duration(2+s.rng.Intn(10)) * time.Minute)
				}
			}
			if err := store.CreateTask(s.ctx, s.db, t); err != nil {
				log.Fatalf("demoseed: task %q: %v", st.title, err)
			}
			prevID = t.ID
			if st.status == "done" && st.daysAgo <= 12 {
				s.Event(*t.ClosedAt, core.EventTaskTransition, "", p.project, t.ID, map[string]any{
					"to": "done", "title": st.title,
				})
			}
		}
	}

	rotator := liveSessionID(sessions, "cc/f3d19b2c")
	ids := make([]string, len(standaloneTasks))
	for i, ts := range standaloneTasks {
		created := s.DaysAgo(ts.daysAgo).Add(time.Duration(9+s.rng.Intn(9)) * time.Hour)
		creator := s.sessionFor(sessions, ts.project, ts.daysAgo)
		t := core.Task{
			ID: s.IdAt(created), ProjectSlug: ts.project, Title: ts.title, Body: ts.body,
			Status: core.TaskStatus(ts.status), CreatedBy: creator.name,
			CreatedAt: created, UpdatedAt: created,
		}
		if ts.dependsI >= 0 {
			t.DependsOn = []string{ids[ts.dependsI]}
		}
		switch {
		case ts.status == "done":
			closed := created.Add(time.Duration(6+s.rng.Intn(40)) * time.Hour)
			t.ClosedAt = &closed
			t.UpdatedAt = closed
		case ts.claimed:
			lease := s.now.Add(9 * time.Minute)
			t.ClaimedBy = rotator
			t.LeaseExpiresAt = &lease
			t.UpdatedAt = s.now.Add(-14 * time.Minute)
		}
		if err := store.CreateTask(s.ctx, s.db, t); err != nil {
			log.Fatalf("demoseed: task %q: %v", ts.title, err)
		}
		ids[i] = t.ID
	}
}

// ---------------------------------------------------------------------------
// trials
// ---------------------------------------------------------------------------

func (s *Seeder) trials(sessions []sessRec) {
	for _, tr := range trialSpecs {
		sess := s.sessionFor(sessions, "orbital", tr.daysAgo)
		at := s.DaysAgo(tr.daysAgo).Add(time.Duration(10+s.rng.Intn(8)) * time.Hour)
		t := core.Trial{
			ID: s.IdAt(at), Lab: trialLab, Title: tr.title, Changes: tr.changes,
			Expected: tr.expected, Actual: tr.actual, Outcome: core.TrialOutcome(tr.outcome),
			SessionID: sess.id, ProjectSlug: "orbital", CreatedAt: at,
		}
		if err := store.CreateTrial(s.ctx, s.db, t); err != nil {
			log.Fatalf("demoseed: trial %q: %v", tr.title, err)
		}
		s.Event(at, core.EventTrialRecorded, sess.id, "orbital", t.ID, map[string]any{
			"title": tr.title, "lab": trialLab, "outcome": tr.outcome,
		})
		s.ToolCall(at.Add(time.Second), sess.id, "orbital", "trial_record",
			map[string]any{"lab": trialLab, "title": tr.title}, "recorded ("+tr.outcome+")", 6+s.rng.Intn(12))
	}
}

// ---------------------------------------------------------------------------
// gardener proposals (backdated via direct insert; CreateProposal stamps now)
// ---------------------------------------------------------------------------

func (s *Seeder) gardener(mems []memRec) {
	byName := map[string]memRec{}
	for _, m := range mems {
		byName[m.name] = m
	}
	brief := func(m memRec, desc string) map[string]any {
		return map[string]any{"id": m.id, "name": m.name, "project": m.project, "description": desc, "kind": m.kind}
	}
	keep, drop := byName["edge-cache-vary-cookie"], byName["cdn-cookie-cache-miss"]
	stale := byName["storybook-6-migration-notes"]

	proposals := []struct {
		kind    string
		payload map[string]any
		agoH    int
	}{
		{"merge", map[string]any{
			"key":   "merge:" + keep.id + "|" + drop.id,
			"score": 0.91,
			"keep":  brief(keep, "The edge cache treats Vary: Cookie as uncacheable -- session cookie on static assets nuked the hit rate to 4%."),
			"drop":  brief(drop, "CDN misses on every asset when the session cookie rides along; static routes must stay cookieless."),
		}, 3},
		{"archive", map[string]any{
			"key": "archive:" + stale.id, "id": stale.id, "name": stale.name,
			"project": stale.project, "kind": stale.kind,
			"description":   "Old Storybook 6 migration notes -- kept for the addon compatibility table.",
			"reason":        "no activity in 30d",
			"last_activity": core.FormatTime(stale.created),
		}, 26},
	}
	for _, p := range proposals {
		raw, err := json.Marshal(p.payload)
		if err != nil {
			log.Fatalf("demoseed: proposal payload: %v", err)
		}
		at := s.now.Add(-time.Duration(p.agoH) * time.Hour)
		if _, err := s.db.ExecContext(s.ctx, `
			INSERT INTO gardener_proposals (id, kind, payload, status, created_at)
			VALUES (?, ?, ?, 'pending', ?)`,
			s.IdAt(at), p.kind, string(raw), core.FormatTime(at)); err != nil {
			log.Fatalf("demoseed: proposal insert: %v", err)
		}
		s.Event(at, core.EventGardenerAction, "", "", "", map[string]any{
			"action": "propose", "kind": p.kind, "key": p.payload["key"],
		})
	}
}

// ---------------------------------------------------------------------------
// the recent burst: a choreographed last hour so the feed's first screen tells
// the product story (briefing -> claim -> supersede -> recall)
// ---------------------------------------------------------------------------

func (s *Seeder) recentBurst(sessions []sessRec, mems []memRec) {
	byName := map[string]memRec{}
	for _, m := range mems {
		byName[m.name] = m
	}
	replayIDs := []string{
		byName["stripe-webhook-replay-window"].id,
		byName["payments-idempotent-by-event-id"].id,
		byName["webhook-replay-dedupe-store"].id,
		byName["replay-dedupe-event-id-unique-index"].id,
		byName["billing-webhooks-v2-replay-landed"].id,
	}
	worker := liveSessionID(sessions, "cc/7c31a9e2")
	rotator := liveSessionID(sessions, "cc/f3d19b2c")
	tokens := liveSessionID(sessions, "cc/9d24e7b0")
	mark := func(ids []string) []string {
		for _, id := range ids {
			s.injected[id] = true
		}
		return ids
	}

	beats := []func(){
		func() { // 25m ago: the replay-step session started with a briefing
			t := s.now.Add(-25 * time.Minute)
			s.Event(t.Add(2*time.Second), core.EventInjected, worker, "orbital", "", map[string]any{
				"hook": "SessionStart", "item_ids": mark(replayIDs), "content": briefingPreviews[0],
			})
		},
		func() { // 24m ago: it claimed the replay-protection step
			t := s.now.Add(-24 * time.Minute)
			s.ToolCall(t, worker, "orbital", "tasks_claim",
				map[string]any{"id": "replay-protection + dedupe store"}, "claimed; lease 900s", 12)
			s.Event(t.Add(time.Second), core.EventTaskTransition, worker, "orbital", "", map[string]any{
				"to": "in_progress", "title": "Replay protection + dedupe store",
			})
		},
		func() { // 16m ago: recall while working
			t := s.now.Add(-16 * time.Minute)
			s.ToolCall(t, worker, "orbital", "recall",
				map[string]any{"query": "dedupe store unique index burst"}, "4 hits", 96)
			s.Event(t.Add(100*time.Millisecond), core.EventInjected, worker, "orbital", "", map[string]any{
				"query": "dedupe store unique index burst", "item_ids": mark(replayIDs[:3]), "source": "recall",
			})
		},
		func() { // 14m ago: the rotation session heartbeats its claim
			t := s.now.Add(-14 * time.Minute)
			s.ToolCall(t, rotator, "orbital", "tasks_claim",
				map[string]any{"id": "rotate stripe webhook signing key"}, "lease refreshed", 9)
		},
		func() { // 9m ago: token migration appends progress to the plan note
			t := s.now.Add(-9 * time.Minute)
			s.ToolCall(t, tokens, "orbital-web", "notes_append",
				map[string]any{"note": "plan-design-tokens-refresh"}, "appended 14 lines", 11)
		},
		func() { // 8m ago: a prompt with no recall match
			t := s.now.Add(-8 * time.Minute)
			s.Event(t, core.EventHookPrompt, tokens, "orbital-web", "", map[string]any{
				"hook": "UserPromptSubmit", "prompt": "migrate the data-table component to generated tokens",
			})
		},
		func() { // 4m ago: memory_write that supersedes edge-cache-gotcha (v1)
			t := s.now.Add(-4 * time.Minute)
			m := byName["edge-cache-vary-cookie"]
			s.ToolCall(t, tokens, "orbital", "memory_write",
				map[string]any{"name": m.name, "supersedes": "edge-cache-gotcha"}, "stored; superseded edge-cache-gotcha", 18)
			s.Event(t.Add(time.Second), core.EventMemoryWritten, tokens, "orbital", m.id, map[string]any{
				"name": m.name, "kind": "gotcha", "project": "orbital",
			})
		},
		func() { // 20m ago: token session recalls its own fresh groundwork
			t := s.now.Add(-20 * time.Minute)
			ids := []string{byName["tokens-ts-consts-pattern"].id, byName["design-tokens-refresh-generate-done"].id, byName["tokens-generated-never-edited"].id}
			s.ToolCall(t, tokens, "orbital-web", "recall",
				map[string]any{"query": "generated token consts import surface"}, "3 hits", 71)
			s.Event(t.Add(100*time.Millisecond), core.EventInjected, tokens, "orbital-web", "", map[string]any{
				"query": "generated token consts import surface", "item_ids": mark(ids), "source": "recall",
			})
		},
		func() { // 2m ago: replay worker recalls the memories it just wrote
			t := s.now.Add(-2 * time.Minute)
			ids := []string{byName["dedupe-store-schema-decision"].id, byName["replay-dedupe-event-id-unique-index"].id, byName["dedupe-backfill-runbook"].id}
			s.ToolCall(t, worker, "orbital", "recall",
				map[string]any{"query": "dedupe store backfill after outage"}, "3 hits", 88)
			s.Event(t.Add(100*time.Millisecond), core.EventInjected, worker, "orbital", "", map[string]any{
				"query": "dedupe store backfill after outage", "item_ids": mark(ids), "source": "recall",
			})
		},
		func() { // 90s ago: fresh session checks the ready queue
			t := s.now.Add(-90 * time.Second)
			s.ToolCall(t, worker, "orbital", "tasks_ready", map[string]any{}, "1 ready (load test at 50x burst blocked)", 5)
		},
	}
	for _, beat := range beats {
		beat()
	}

	// Fill the last hour with working chatter from the live sessions so the 1h
	// interactions histogram reads as a fleet at work, not a demo at rest.
	chatter := []struct {
		sid, project, tool, result string
	}{
		{worker, "orbital", "tasks_list", "5 tasks"},
		{worker, "orbital", "notes_read", "note body returned"},
		{worker, "orbital", "memory_read", "body returned"},
		{worker, "orbital", "session_update", "checkpoint saved"},
		{worker, "orbital", "notes_append", "appended 9 lines"},
		{rotator, "orbital", "tasks_list", "9 tasks"},
		{rotator, "orbital", "memory_read", "body returned"},
		{rotator, "orbital", "notes_read", "note body returned"},
		{rotator, "orbital", "session_update", "checkpoint saved"},
		{tokens, "orbital-web", "notes_read", "note body returned"},
		{tokens, "orbital-web", "tasks_list", "3 tasks"},
		{tokens, "orbital-web", "memory_read", "body returned"},
		{tokens, "orbital-web", "session_update", "checkpoint saved"},
		{tokens, "orbital-web", "usage_summary", "summary returned"},
	}
	for i, c := range chatter {
		back := time.Duration(3+((i*257)%52)) * time.Minute // deterministic spread over the hour
		s.ToolCall(s.now.Add(-back), c.sid, c.project, c.tool, map[string]any{}, c.result, 3+s.rng.Intn(22))
	}
}

// ---------------------------------------------------------------------------
// summary
// ---------------------------------------------------------------------------

func (s *Seeder) summary() { fmt.Print(s.fleetSummary()) }

// fleetSummary is the console fleet's summary: its counts line and its
// memory-kinds line, counted from the database as it stands.
func (s *Seeder) fleetSummary() string {
	q := func(query string) int {
		var n int
		if err := s.db.QueryRowContext(s.ctx, query).Scan(&n); err != nil {
			log.Fatalf("demoseed: summary: %v", err)
		}
		return n
	}
	var b strings.Builder
	fmt.Fprintf(&b, "seeded: %d sessions, %d active memories (%d total), %d notes, %d tasks (%d open/wip), %d events, %d proposals\n",
		q(`SELECT COUNT(*) FROM sessions`),
		q(`SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL`),
		q(`SELECT COUNT(*) FROM memories_index`),
		q(`SELECT COUNT(*) FROM notes_index`),
		q(`SELECT COUNT(*) FROM tasks`),
		q(`SELECT COUNT(*) FROM tasks WHERE status IN ('open','in_progress')`),
		q(`SELECT COUNT(*) FROM events`),
		q(`SELECT COUNT(*) FROM gardener_proposals WHERE status='pending'`))
	kinds, err := s.db.QueryContext(s.ctx, `SELECT kind, COUNT(*) FROM memories_index WHERE invalid_at IS NULL GROUP BY kind ORDER BY 2 DESC`)
	if err != nil {
		log.Fatalf("demoseed: kinds: %v", err)
	}
	defer func() { _ = kinds.Close() }()
	b.WriteString("kinds:")
	for kinds.Next() {
		var k string
		var n int
		if err := kinds.Scan(&k, &n); err != nil {
			log.Fatalf("demoseed: kinds scan: %v", err)
		}
		fmt.Fprintf(&b, " %s=%d", k, n)
	}
	b.WriteString("\n")
	if err := kinds.Err(); err != nil {
		log.Fatalf("demoseed: kinds rows: %v", err)
	}
	return b.String()
}
