// The review fixture (demoseed -review, plan:console-ux-pass): the states the
// console must show that the console-fleet seed has none of, so each one can
// be checked by eye. It seeds the fleet first, exactly as the default mode
// does, and then lays a layer of its own on top: new projects and the rows
// that live in them. It changes no row of the fleet's, and the fleet's RNG has
// drawn its last number before this layer draws its first, so the fleet part
// of a review seed is the default seed.
//
// Every state has a stable name (ReviewState) and the console page that shows
// it; ReviewFixture maps each to the items that carry it, and Summary prints
// that map. The rows go in through the store, files, screening and events
// calls the real surfaces use, each with its backdated time.
//
// Live claims carry three-hour leases, inside core.MaxClaimLease, so the live
// sessions and the lapsed claim beside them hold for about two hours after
// the seed; a claimless, blocked or gated task holds until someone moves it.

package demokit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// reviewSeed seeds the review layer's RNG: its ids, session names, client
// session ids and schedule. Each session's usage has an RNG of its own, keyed
// by the session (diaryUsageRNG).
const reviewSeed = 20261121

// The review layer's two working projects.
const (
	// ReviewOps holds the off-plan task states, four of the plans, the long
	// session list, the mishaps, the held write and the lock request.
	ReviewOps = "example-ops"
	// ReviewWeb holds the second half of the plan slug two projects share.
	ReviewWeb = "example-web"
)

// ReviewSessions is how many finished sessions ReviewOps holds, more than
// any session list shows at once; the live ones come on top.
const ReviewSessions = 264

// reviewSessionDays is how many days back ReviewOps's finished sessions
// reach: past the oldest plan step, so every step has a creator.
const reviewSessionDays = 24

// ReviewAccount is the restricted local account whose lock declaration waits
// for the owner.
const ReviewAccount = "example-agent"

// reviewLockOrigin is the repository the waiting lock declaration names.
const reviewLockOrigin = "https://git.example.com/example/example-ops.git"

// ReviewState names one state the review fixture seeds.
type ReviewState string

// The review fixture's states, in the order Summary prints them.
const (
	ReviewTaskClaimLive   ReviewState = "task.claim-live"   // in progress, held by a live claim
	ReviewTaskClaimLapsed ReviewState = "task.claim-lapsed" // in progress, its holder's lease lapsed
	ReviewTaskClaimless   ReviewState = "task.claimless"    // in progress with no holder (seam task start)
	ReviewTaskBlocked     ReviewState = "task.blocked"      // waits on an open task
	ReviewTaskGated       ReviewState = "task.gated"        // its own gate waits on the owner
	ReviewTaskGate        ReviewState = "task.gate"         // a gate two steps depend on
	ReviewTaskClosed      ReviewState = "task.closed"       // done and dropped off-plan tasks

	ReviewPlanDroppedStep ReviewState = "plan.dropped-step"      // every step closed, one of them dropped
	ReviewPlanNoNote      ReviewState = "plan.no-note"           // steps and no narrative note
	ReviewPlanStalled     ReviewState = "plan.stalled"           // nothing moved for 10+ days, a step claimable
	ReviewPlanGated       ReviewState = "plan.gated"             // a step waits on an open gate
	ReviewPlanSharedSlug  ReviewState = "plan.shared-slug"       // one slug, steps in two projects
	ReviewPlanAwaiting    ReviewState = "plan.awaiting-approval" // the fleet's captured plan, presented

	ReviewProjectsQuiet       ReviewState = "projects.quiet"       // no activity for 30+ days
	ReviewProjectsStarred     ReviewState = "projects.starred"     // starred by the owner
	ReviewProjectRetired      ReviewState = "project.retired"      // retired
	ReviewProjectConfidential ReviewState = "project.confidential" // isolation confidential

	ReviewSessionsLive  ReviewState = "sessions.live-claims" // live sessions, each holding a different claim
	ReviewSessionsMany  ReviewState = "sessions.many"        // ReviewSessions finished sessions in one project
	ReviewMishaps       ReviewState = "mishaps"              // agent.mishap reports
	ReviewHeldWrite     ReviewState = "held.write"           // a write held for the owner
	ReviewLockRequested ReviewState = "lock.requested"       // a lock declaration waiting for the owner
	ReviewUsageLedger   ReviewState = "usage.ledger"         // ledger rows behind the token and time figures
)

// ReviewItem is one seeded state: the console page that shows it, the ids or
// slugs that carry it, and one line saying what it is.
type ReviewItem struct {
	State  ReviewState
	Page   string
	IDs    []string
	Detail string
}

// ReviewFixture is what SeedReview seeded. The database is the record; this
// is its map, for the demoseed summary and the tests.
type ReviewFixture struct {
	// Fleet is the console-fleet seed's summary, counted after the review
	// layer: the same two lines the default mode prints.
	Fleet string
	// Items is every state, in the order of the ReviewState constants.
	Items []ReviewItem
}

// Item returns the item of a state.
func (fx ReviewFixture) Item(state ReviewState) (ReviewItem, bool) {
	i := slices.IndexFunc(fx.Items, func(it ReviewItem) bool { return it.State == state })
	if i < 0 {
		return ReviewItem{}, false
	}
	return fx.Items[i], true
}

// Summary is the demoseed -review summary: the fleet's two lines, then one
// line per state naming the page that shows it and what carries it.
func (fx ReviewFixture) Summary() string {
	var b strings.Builder
	b.WriteString(fx.Fleet)
	fmt.Fprintf(&b, "review: %d states on top of the console fleet\n", len(fx.Items))
	for _, it := range fx.Items {
		fmt.Fprintf(&b, "  %-22s %-18s %s\n", it.State, it.Page, it.Detail)
	}
	return b.String()
}

// SeedReview seeds the console fleet, then the review layer on top of it, and
// returns the map of what the layer seeded. Like SeedConsoleFleet it stops the
// process on a broken fleet; a failure in the layer is returned.
func (s *Seeder) SeedReview() (ReviewFixture, error) {
	s.consoleFleet()
	r := &reviewSeeder{
		s: s, ctx: s.ctx, rng: rand.New(rand.NewSource(reviewSeed)), now: s.now,
		sessions: map[string]core.Session{}, tasks: map[string]core.Task{}, mems: map[string]core.Memory{},
	}
	if err := r.seed(); err != nil {
		return ReviewFixture{}, fmt.Errorf("demokit: review: %w", err)
	}
	r.fx.Fleet = s.fleetSummary()
	return r.fx, nil
}

// reviewSeeder is one SeedReview run.
type reviewSeeder struct {
	s   *Seeder
	ctx context.Context
	rng *rand.Rand
	now time.Time

	sessions map[string]core.Session // by key
	finished []core.Session          // ReviewOps's finished sessions, oldest first
	tasks    map[string]core.Task    // by key
	mems     map[string]core.Memory  // by name
	fx       ReviewFixture
}

func (r *reviewSeeder) seed() error {
	for _, step := range []func() error{
		r.projects, r.liveSessions, r.opsSessions, r.memories, r.quiet,
		r.offPlanTasks, r.plans, r.mishaps, r.heldWrite, r.lockRequest, r.ledgerItem,
	} {
		if err := step(); err != nil {
			return err
		}
	}
	slices.SortStableFunc(r.fx.Items, func(a, b ReviewItem) int {
		return cmp.Compare(slices.Index(reviewStates, a.State), slices.Index(reviewStates, b.State))
	})
	return nil
}

// reviewStates is every state, in Summary's order.
var reviewStates = []ReviewState{
	ReviewTaskClaimLive, ReviewTaskClaimLapsed, ReviewTaskClaimless, ReviewTaskBlocked, ReviewTaskGated,
	ReviewTaskGate, ReviewTaskClosed,
	ReviewPlanDroppedStep, ReviewPlanNoNote, ReviewPlanStalled, ReviewPlanGated, ReviewPlanSharedSlug,
	ReviewPlanAwaiting,
	ReviewProjectsQuiet, ReviewProjectsStarred, ReviewProjectRetired, ReviewProjectConfidential,
	ReviewSessionsLive, ReviewSessionsMany, ReviewMishaps, ReviewHeldWrite, ReviewLockRequested,
	ReviewUsageLedger,
}

func (r *reviewSeeder) add(state ReviewState, page, detail string, ids ...string) {
	r.fx.Items = append(r.fx.Items, ReviewItem{State: state, Page: page, IDs: ids, Detail: detail})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (r *reviewSeeder) id(t time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(t.UTC()), r.rng)
	if err != nil {
		return "", fmt.Errorf("mint ulid: %w", err)
	}
	return id.String(), nil
}

// name is a session display name no fleet or review session has.
func (r *reviewSeeder) name() string {
	for {
		n := fmt.Sprintf("cc/%08x", r.rng.Uint32())
		if !r.s.used[n] {
			r.s.used[n] = true
			return n
		}
	}
}

// uuid is a client session id shaped like the ones Claude Code sends.
func (r *reviewSeeder) uuid() string {
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", r.rng.Uint32(), r.rng.Uint32()&0xffff,
		r.rng.Uint32()&0xfff, r.rng.Uint32()&0x3fff|0x8000, r.rng.Uint64()&0xffffffffffff)
}

func (r *reviewSeeder) ago(d time.Duration) time.Time { return r.now.Add(-d) }

func (r *reviewSeeder) event(t time.Time, kind core.EventKind, sessionID, project, itemID string, payload map[string]any) error {
	if _, err := r.s.rec.Record(r.ctx, core.Event{
		TS: t.UTC(), Kind: kind, SessionID: sessionID, ProjectSlug: project, ItemID: itemID, Payload: payload,
	}); err != nil {
		return fmt.Errorf("record %s: %w", kind, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

func (r *reviewSeeder) projects() error {
	for _, p := range []struct {
		slug, desc string
		days       int
	}{
		{ReviewOps, "Operations: backups, alerting, storage and the internal network", 70},
		{ReviewWeb, "Public website and its services", 60},
	} {
		if err := r.project(p.slug, p.desc, r.ago(time.Duration(p.days)*24*time.Hour)); err != nil {
			return err
		}
	}
	return nil
}

func (r *reviewSeeder) project(slug, desc string, at time.Time) error {
	id, err := r.id(at)
	if err != nil {
		return err
	}
	if err := store.CreateProject(r.ctx, r.s.db, core.Project{
		ID: id, Slug: slug, Name: slug, Description: desc, CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		return fmt.Errorf("project %s: %w", slug, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// sessions and their usage
// ---------------------------------------------------------------------------

// rSession is one review session to create.
type rSession struct {
	key, project, model string
	start, last         time.Time // last: its end, or its last activity
	status              core.SessionStatus
	findings            string
	account             string
	ledger              bool // a Claude Code session whose transcript's usage lands in the ledger
	subagent            bool
}

// session creates one session with its start (and end) events and, for a
// Claude Code session, its ledger rows.
func (r *reviewSeeder) session(spec rSession) (core.Session, error) {
	id, err := r.id(spec.start)
	if err != nil {
		return core.Session{}, err
	}
	sess := core.Session{
		ID: id, Name: r.name(), ProjectSlug: spec.project, Status: spec.status, Findings: spec.findings,
		CWD: "/home/dev/" + spec.project, Source: "startup", Model: spec.model, Account: spec.account,
		CreatedAt: spec.start, UpdatedAt: spec.last,
	}
	if spec.ledger {
		sess.ExternalSessionID, sess.ExternalClient, sess.Ambient = r.uuid(), clientClaude, true
		sess.Metadata = map[string]any{
			"claude_session_id": sess.ExternalSessionID, "external_client": clientClaude,
			"cwd": sess.CWD, "host": "", "source": "startup",
		}
	} else {
		sess.Source = "explicit"
	}
	if err := store.CreateSession(r.ctx, r.s.db, sess); err != nil {
		return core.Session{}, fmt.Errorf("session %s: %w", spec.key, err)
	}
	if err := r.event(spec.start, core.EventSessionStarted, id, spec.project, "", map[string]any{
		"name": sess.Name, "ambient": sess.Ambient, "source": sess.Source,
	}); err != nil {
		return core.Session{}, err
	}
	if spec.status == core.SessionCompleted {
		if err := r.event(spec.last, core.EventSessionEnded, id, spec.project, "", map[string]any{
			"findings": spec.findings, "claims_released": 0,
		}); err != nil {
			return core.Session{}, err
		}
	}
	if spec.ledger {
		if err := r.usage(sess, spec); err != nil {
			return core.Session{}, err
		}
	}
	r.sessions[spec.key] = sess
	return sess, nil
}

// usage writes a Claude Code session's ledger rows: the Diary fixture's own
// usage model (diaryRun.claudeRows) over the session's span, harvested the way
// SessionEnd harvests a transcript (store.ReplaceConversationUsage). A live
// session's harvest is its heartbeat, so it keeps its last activity.
func (r *reviewSeeder) usage(sess core.Session, spec rSession) error {
	run := &diaryRun{
		spec:  dSession{key: "review/" + spec.key, model: spec.model, mins: max(1, int(spec.last.Sub(spec.start)/time.Minute))},
		start: spec.start,
	}
	if spec.subagent {
		run.spec.subs = []dSpan{{at: 3, mins: 11, model: modelFable}}
	}
	rows := run.claudeRows(spec.last)
	if len(rows) == 0 {
		return nil
	}
	ok, err := store.ReplaceConversationUsage(r.ctx, r.s.db, clientClaude, sess.ExternalSessionID, rows, spec.last)
	if err != nil {
		return fmt.Errorf("session %s: %w", spec.key, err)
	}
	if !ok {
		return fmt.Errorf("session %s: the ledger found no ambient row", spec.key)
	}
	return nil
}

// The live sessions: each holds a different claim (offPlanTasks, plans).
// rLiveLapsed also holds the lapsed claim; its live claim keeps it live, so
// the reaper leaves both alone while that lease runs.
const (
	rLiveOffPlan = "live-off-plan" // the off-plan task's live claim
	rLiveNoNote  = "live-no-note"  // a step of the plan with no note
	rLiveWeb     = "live-web"      // the shared slug's step in ReviewWeb
	rLiveLapsed  = "live-lapsed"   // a storage step, and the lapsed off-plan claim
	rRestricted  = "restricted"    // the restricted account's session that asked for the lock
)

func (r *reviewSeeder) liveSessions() error {
	for _, l := range []struct {
		key, project, model string
		started, last       time.Duration
	}{
		{rLiveOffPlan, ReviewOps, modelOpus, 35 * time.Minute, 3 * time.Minute},
		{rLiveNoNote, ReviewOps, modelFable, 52 * time.Minute, 6 * time.Minute},
		{rLiveWeb, ReviewWeb, modelOpus, 22 * time.Minute, time.Minute},
		{rLiveLapsed, ReviewOps, modelOpus, 80 * time.Minute, 4 * time.Minute},
	} {
		if _, err := r.session(rSession{
			key: l.key, project: l.project, model: l.model, start: r.ago(l.started), last: r.ago(l.last),
			status: core.SessionActive, ledger: true, subagent: l.key == rLiveLapsed,
		}); err != nil {
			return err
		}
	}
	_, err := r.session(rSession{
		key: rRestricted, project: ReviewOps, model: modelFable, start: r.ago(3 * time.Hour),
		last: r.ago(2*time.Hour + 41*time.Minute), status: core.SessionCompleted, account: ReviewAccount,
		findings: "Asked the owner for a lock on main before the runbook branch merges.",
	})
	return err
}

// opsSessions seeds ReviewOps's finished sessions: ReviewSessions of them over
// the last reviewSessionDays days, denser toward today, Claude Code sessions
// whose usage lands in the ledger.
func (r *reviewSeeder) opsSessions() error {
	span := float64(reviewSessionDays * 24 * 60)
	for i := range ReviewSessions {
		back := time.Duration(span*math.Pow(float64(i)/ReviewSessions, 1.5)) * time.Minute
		start := r.ago(50*time.Minute + back + time.Duration(r.rng.Intn(50))*time.Minute)
		end := start.Add(time.Duration(8+r.rng.Intn(85)) * time.Minute)
		if limit := r.ago(10 * time.Minute); end.After(limit) {
			end = limit
		}
		status, findings := core.SessionCompleted, Pick(r.rng, reviewFindings)
		if r.rng.Intn(100) < 14 {
			status, findings = core.SessionExpired, ""
		}
		model := modelOpus
		if r.rng.Intn(3) == 0 {
			model = modelFable
		}
		sess, err := r.session(rSession{
			key: fmt.Sprintf("ops-%03d", i), project: ReviewOps, model: model, start: start, last: end,
			status: status, findings: findings, ledger: true, subagent: r.rng.Intn(5) == 0,
		})
		if err != nil {
			return err
		}
		r.finished = append(r.finished, sess)
	}
	slices.SortFunc(r.finished, func(a, b core.Session) int { return a.CreatedAt.Compare(b.CreatedAt) })
	n, err := r.count(`SELECT COUNT(*) FROM sessions WHERE project_slug = ?`, ReviewOps)
	if err != nil {
		return err
	}
	r.add(ReviewSessionsMany, "/console/sessions",
		fmt.Sprintf("%s holds %d sessions: %d finished over %d days, the live ones and the restricted account's", ReviewOps, n, ReviewSessions, reviewSessionDays))
	return nil
}

// recentFinished is the newest of ReviewOps's finished sessions that ended
// with a session_end, skip from the newest.
func (r *reviewSeeder) recentFinished(skip int) (core.Session, error) {
	for i := len(r.finished) - 1; i >= 0; i-- {
		if r.finished[i].Status != core.SessionCompleted {
			continue
		}
		if skip == 0 {
			return r.finished[i], nil
		}
		skip--
	}
	return core.Session{}, errors.New("not enough finished sessions")
}

// creatorFor is the ReviewOps session working at t: the newest finished one
// that started before it, or the oldest when none did.
func (r *reviewSeeder) creatorFor(t time.Time) core.Session {
	for i := len(r.finished) - 1; i >= 0; i-- {
		if r.finished[i].CreatedAt.Before(t) {
			return r.finished[i]
		}
	}
	return r.finished[0]
}

func (r *reviewSeeder) count(query string, args ...any) (int, error) {
	var n int
	if err := r.s.db.QueryRowContext(r.ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// memories
// ---------------------------------------------------------------------------

// memories writes the working projects' memories, each by one of the finished
// sessions; the mishaps name one of them.
func (r *reviewSeeder) memories() error {
	for i, m := range reviewMemories {
		by, err := r.recentFinished(3 + 7*i)
		if err != nil {
			return err
		}
		if m.project == ReviewWeb {
			by = r.sessions[rLiveWeb]
		}
		at := by.CreatedAt.Add(9 * time.Minute)
		mem, err := r.memory(m.project, m.kind, m.name, m.desc, by, at)
		if err != nil {
			return err
		}
		r.mems[m.name] = mem
	}
	return nil
}

func (r *reviewSeeder) memory(project string, kind core.MemoryKind, name, desc string, by core.Session, at time.Time) (core.Memory, error) {
	id, err := r.id(at)
	if err != nil {
		return core.Memory{}, err
	}
	mem, err := r.s.mgr.WriteMemory(r.ctx, core.Memory{
		ID: id, Kind: kind, Name: name, Description: desc, Project: project,
		Body:    desc + "\n\nWritten during a working session; the session's findings hold the context.",
		Created: at, Updated: at, ValidFrom: at, SourceSession: by.ID, Model: by.Model,
	})
	if err != nil {
		return core.Memory{}, fmt.Errorf("memory %s: %w", name, err)
	}
	if err := r.event(at, core.EventMemoryWritten, by.ID, project, id, map[string]any{
		"name": name, "kind": string(kind), "project": project,
	}); err != nil {
		return core.Memory{}, err
	}
	return mem, nil
}

// ---------------------------------------------------------------------------
// quiet projects
// ---------------------------------------------------------------------------

// quiet seeds the projects nobody touched for 30+ days, a few with open
// tasks, the stars, the retired project and the confidential one.
func (r *reviewSeeder) quiet() error {
	var slugs, withTasks []string
	for i, q := range reviewQuiet {
		lastDays := 31 + (i*37)%55 // 31..85 days: quiet, and younger than the staleness pass's 90
		if q.retired {
			lastDays = 104
		}
		last := r.ago(time.Duration(lastDays)*24*time.Hour + time.Duration(r.rng.Intn(10*60))*time.Minute)
		if err := r.project(q.slug, q.desc, last.AddDate(0, 0, -(30+r.rng.Intn(240)))); err != nil {
			return err
		}
		var sess core.Session
		for k := range 1 + r.rng.Intn(3) {
			end := last.AddDate(0, 0, -9*k)
			var err error
			sess, err = r.session(rSession{
				key: fmt.Sprintf("quiet-%s-%d", q.slug, k), project: q.slug, model: modelFable,
				start: end.Add(-time.Duration(12+r.rng.Intn(50)) * time.Minute), last: end,
				status: core.SessionCompleted, findings: q.finding,
			})
			if err != nil {
				return err
			}
			if k == 0 && q.memory != "" {
				name := strings.TrimPrefix(q.slug, "example-") + "-" + q.memoryName
				if _, err := r.memory(q.slug, q.kind, name, q.memory, sess, end.Add(-5*time.Minute)); err != nil {
					return err
				}
			}
		}
		for k, title := range q.tasks {
			if _, err := r.task(rTask{
				key: fmt.Sprintf("quiet-%s-%d", q.slug, k), project: q.slug, title: title,
				body: "Left open when the project went quiet.", created: r.now.Sub(last) + time.Duration(k+1)*time.Hour,
				by: sess.Name,
			}); err != nil {
				return err
			}
		}
		if len(q.tasks) > 0 {
			withTasks = append(withTasks, q.slug)
		}
		if q.retired {
			if err := store.RetireProject(r.ctx, r.s.db, q.slug, r.ago(45*24*time.Hour)); err != nil {
				return fmt.Errorf("retire %s: %w", q.slug, err)
			}
			r.add(ReviewProjectRetired, "/console/projects", q.slug+", retired 45 days ago", q.slug)
		}
		if q.confidential {
			if _, err := store.TightenProjectIsolation(r.ctx, r.s.db, q.slug, core.IsolationConfidential, r.ago(50*24*time.Hour)); err != nil {
				return fmt.Errorf("fence %s: %w", q.slug, err)
			}
			r.add(ReviewProjectConfidential, "/console/projects", q.slug+", isolation confidential", q.slug)
		}
		slugs = append(slugs, q.slug)
	}
	r.add(ReviewProjectsQuiet, "/console/projects", fmt.Sprintf("%d projects with no activity for 30+ days; open tasks in %s",
		len(slugs), strings.Join(withTasks, ", ")), slugs...)

	starred := []string{ReviewOps, "example-ios", "example-reports"}
	for _, slug := range starred {
		if err := store.SetProjectFavorite(r.ctx, r.s.db, slug, true); err != nil {
			return fmt.Errorf("star %s: %w", slug, err)
		}
	}
	r.add(ReviewProjectsStarred, "/console/projects", strings.Join(starred, ", "), starred...)
	return nil
}

// ---------------------------------------------------------------------------
// tasks
// ---------------------------------------------------------------------------

// rTask is one review task. Times are durations before the anchor. A closed
// task is created open and closed through UpdateTask; a claimed one is claimed
// through ClaimTask; a claimless one is started through UpdateTask with no
// actor, the shape `seam task start` leaves.
type rTask struct {
	key, project, plan, title, body string
	kind                            core.TaskKind
	gate                            core.TaskGate
	deps                            []string // keys
	created                         time.Duration
	by                              string // the creating session's name

	close     core.TaskStatus // done or dropped, at closed
	closed    time.Duration
	holder    string        // session key: claimed at claimedAt for lease
	claimedAt time.Duration // before the anchor
	lease     time.Duration
	claimless time.Duration // started with no holder this long ago (0: not)
}

func (r *reviewSeeder) task(spec rTask) (core.Task, error) {
	created := r.ago(spec.created)
	id, err := r.id(created)
	if err != nil {
		return core.Task{}, err
	}
	t := core.Task{
		ID: id, ProjectSlug: spec.project, Title: spec.title, Body: spec.body, Status: core.TaskOpen,
		CreatedBy: spec.by, PlanSlug: spec.plan, Kind: spec.kind, Gate: spec.gate,
		CreatedAt: created, UpdatedAt: created,
	}
	for _, k := range spec.deps {
		dep, ok := r.tasks[k]
		if !ok {
			return core.Task{}, fmt.Errorf("task %s: blocker %s not seeded yet", spec.key, k)
		}
		t.DependsOn = append(t.DependsOn, dep.ID)
	}
	if err := store.CreateTask(r.ctx, r.s.db, t); err != nil {
		return core.Task{}, fmt.Errorf("task %s: %w", spec.key, err)
	}
	switch {
	case spec.close != "":
		at := r.ago(spec.closed)
		status := spec.close
		if t, err = store.UpdateTask(r.ctx, r.s.db, id, store.TaskPatch{Status: &status}, "", at); err != nil {
			return core.Task{}, fmt.Errorf("close %s: %w", spec.key, err)
		}
		if err := r.event(at, core.EventTaskTransition, "", spec.project, id, map[string]any{
			"from": string(core.TaskOpen), "to": string(status), "title": spec.title,
		}); err != nil {
			return core.Task{}, err
		}
	case spec.holder != "":
		holder, ok := r.sessions[spec.holder]
		if !ok {
			return core.Task{}, fmt.Errorf("task %s: holder %s not seeded", spec.key, spec.holder)
		}
		at := r.ago(spec.claimedAt)
		res, err := store.ClaimTask(r.ctx, r.s.db, id, holder.ID, spec.lease, at)
		if err != nil {
			return core.Task{}, fmt.Errorf("claim %s: %w", spec.key, err)
		}
		t = res.Task
		if err := r.event(at, core.EventToolCall, holder.ID, spec.project, "", map[string]any{
			"tool": "tasks_claim", "args": map[string]any{"id": id, "lease_seconds": int(spec.lease / time.Second)},
			"result": "claimed", "duration_ms": 11, "is_error": false,
		}); err != nil {
			return core.Task{}, err
		}
		if err := r.event(at, core.EventTaskTransition, holder.ID, spec.project, id, map[string]any{
			"from": string(core.TaskOpen), "to": string(core.TaskInProgress), "claimed_by": holder.ID, "title": spec.title,
		}); err != nil {
			return core.Task{}, err
		}
	case spec.claimless > 0:
		at := r.ago(spec.claimless)
		status := core.TaskInProgress
		if t, err = store.UpdateTask(r.ctx, r.s.db, id, store.TaskPatch{Status: &status}, "", at); err != nil {
			return core.Task{}, fmt.Errorf("start %s: %w", spec.key, err)
		}
		if err := r.event(at, core.EventTaskTransition, "", spec.project, id, map[string]any{
			"from": string(core.TaskOpen), "to": string(core.TaskInProgress), "title": spec.title,
		}); err != nil {
			return core.Task{}, err
		}
	}
	r.tasks[spec.key] = t
	return t, nil
}

func (r *reviewSeeder) tasksOf(specs []rTask) error {
	for _, spec := range specs {
		if spec.by == "" {
			spec.by = r.creatorFor(r.ago(spec.created)).Name
		}
		if _, err := r.task(spec); err != nil {
			return err
		}
	}
	return nil
}

func (r *reviewSeeder) holderName(key string) string { return r.sessions[key].Name }

// offPlanTasks seeds ReviewOps's off-plan tasks: one in each state.
func (r *reviewSeeder) offPlanTasks() error {
	if err := r.tasksOf(reviewOffPlan); err != nil {
		return err
	}
	t := r.tasks
	r.add(ReviewTaskClaimLive, "/console/tasks", fmt.Sprintf("%s %q, held by %s",
		t["claim-live"].ID, t["claim-live"].Title, r.holderName(rLiveOffPlan)), t["claim-live"].ID)
	r.add(ReviewTaskClaimLapsed, "/console/tasks", fmt.Sprintf("%s %q, %s's lease lapsed 25m ago",
		t["claim-lapsed"].ID, t["claim-lapsed"].Title, r.holderName(rLiveLapsed)), t["claim-lapsed"].ID)
	r.add(ReviewTaskClaimless, "/console/tasks", fmt.Sprintf("%s %q, in progress with no holder",
		t["claimless"].ID, t["claimless"].Title), t["claimless"].ID)
	r.add(ReviewTaskBlocked, "/console/tasks", fmt.Sprintf("%s %q, blocked by %q",
		t["blocked"].ID, t["blocked"].Title, t["blocker"].Title), t["blocked"].ID, t["blocker"].ID)
	r.add(ReviewTaskGated, "/console/tasks", fmt.Sprintf("%s %q, its gate waits on the owner",
		t["gated"].ID, t["gated"].Title), t["gated"].ID)
	r.add(ReviewTaskGate, "/console/tasks", fmt.Sprintf("%s %q (gate), %q and %q depend on it",
		t["gate"].ID, t["gate"].Title, t["gate-a"].Title, t["gate-b"].Title), t["gate"].ID, t["gate-a"].ID, t["gate-b"].ID)
	var closed []string
	done, dropped := 0, 0
	for _, spec := range reviewOffPlan {
		switch spec.close {
		case core.TaskDone:
			done++
		case core.TaskDropped:
			dropped++
		default:
			continue
		}
		closed = append(closed, t[spec.key].ID)
	}
	r.add(ReviewTaskClosed, "/console/tasks", fmt.Sprintf("%d done and %d dropped off-plan tasks in %s", done, dropped, ReviewOps), closed...)
	return nil
}

// ---------------------------------------------------------------------------
// plans
// ---------------------------------------------------------------------------

func (r *reviewSeeder) plans() error {
	if err := r.tasksOf(reviewPlanSteps); err != nil {
		return err
	}
	for _, n := range reviewPlanNotes {
		at := r.ago(n.ago)
		by := r.creatorFor(at)
		id, err := r.id(at)
		if err != nil {
			return err
		}
		note, err := r.s.mgr.WriteNote(r.ctx, core.Note{
			ID: id, Title: n.title, Slug: "plan-" + n.plan, Description: n.desc, Project: n.project,
			Body: n.desc + "\n\nSteps are the tasks tagged plan:" + n.plan + "; claim one before working it.",
			Tags: []string{"plan:" + n.plan, "created-by:agent"}, Model: by.Model, Created: at, Updated: at,
		})
		if err != nil {
			return fmt.Errorf("note %s: %w", n.plan, err)
		}
		if err := r.event(at, core.EventNoteWritten, by.ID, n.project, note.ID, map[string]any{
			"title": n.title, "slug": note.Slug,
		}); err != nil {
			return err
		}
	}
	t := r.tasks
	r.add(ReviewPlanDroppedStep, "/console/plans", fmt.Sprintf("plan:%s (%s): 3 steps done, %q dropped",
		rPlanDNS, ReviewOps, t["dns-3"].Title), rPlanDNS)
	r.add(ReviewPlanNoNote, "/console/plans", fmt.Sprintf("plan:%s (%s): 3 steps, no note tagged with it, a step held by %s",
		rPlanRunbooks, ReviewOps, r.holderName(rLiveNoNote)), rPlanRunbooks)
	r.add(ReviewPlanStalled, "/console/plans", fmt.Sprintf("plan:%s (%s): nothing moved for 14 days, %q claimable",
		rPlanMetrics, ReviewOps, t["metrics-3"].Title), rPlanMetrics, t["metrics-3"].ID)
	r.add(ReviewPlanGated, "/console/plans", fmt.Sprintf("plan:%s (%s): %q waits on the gate %q",
		rPlanStorage, ReviewOps, t["storage-4"].Title, t["storage-3"].Title), rPlanStorage, t["storage-4"].ID, t["storage-3"].ID)
	r.add(ReviewPlanSharedSlug, "/console/plans", fmt.Sprintf("plan:%s: steps in %s (with the note) and in %s",
		rPlanShared, ReviewOps, ReviewWeb), rPlanShared)
	r.add(ReviewPlanAwaiting, "/console/plans", "plan:api-rate-limiting (orbital): the fleet's captured plan, presented and awaiting approval",
		"api-rate-limiting")

	live := []struct{ session, task string }{
		{rLiveOffPlan, "claim-live"}, {rLiveNoNote, "runbooks-2"}, {rLiveWeb, "shared-web-1"}, {rLiveLapsed, "storage-2"},
	}
	var parts, ids []string
	for _, l := range live {
		parts = append(parts, fmt.Sprintf("%s holds %q", r.holderName(l.session), t[l.task].Title))
		ids = append(ids, r.sessions[l.session].ID)
	}
	r.add(ReviewSessionsLive, "/console/now", strings.Join(parts, "; ")+"; "+r.holderName(rLiveLapsed)+" also holds the lapsed claim", ids...)
	return nil
}

// ---------------------------------------------------------------------------
// mishaps, the held write and the lock request
// ---------------------------------------------------------------------------

// mishaps records the agent.mishap reports, each from a finished session of
// ReviewOps at its end, as session_end records them (mcp.mishapPayload): the
// type the text opens with, and the memories it names by slug.
func (r *reviewSeeder) mishaps() error {
	var ids []string
	types := map[core.MishapType]bool{}
	for i, text := range reviewMishaps {
		by, err := r.recentFinished(1 + 9*i)
		if err != nil {
			return err
		}
		payload := map[string]any{"description": text}
		typ, _ := core.ParseMishap(text)
		if typ != "" {
			payload["type"] = string(typ)
			types[typ] = true
		}
		var named []string
		for name, m := range r.mems {
			if strings.Contains(text, name) {
				named = append(named, m.ID)
			}
		}
		if len(named) > 0 {
			slices.Sort(named)
			payload["item_ids"] = named
		}
		at := by.UpdatedAt.Add(-time.Second)
		if err := r.event(at, core.EventAgentMishap, by.ID, ReviewOps, "", payload); err != nil {
			return err
		}
		ids = append(ids, by.ID)
	}
	r.add(ReviewMishaps, "/console/mishaps", fmt.Sprintf("%d reports in %s, %d types and %d untyped, one names %s",
		len(reviewMishaps), ReviewOps, len(types), len(reviewMishaps)-countTyped(), reviewMishapMemory), ids...)
	return nil
}

func countTyped() int {
	n := 0
	for _, text := range reviewMishaps {
		if typ, _ := core.ParseMishap(text); typ != "" {
			n++
		}
	}
	return n
}

// heldWrite holds one memory write for the owner through the screening
// service, as memory_write does: a live session that read outside text writes
// a convention, and the write waits on the Held screen. The service stamps
// its rows with the clock it is given and its events with the wall clock, so
// the hold falls minutes before the anchor, where the two agree.
func (r *reviewSeeder) heldWrite() error {
	by := r.sessions[rLiveOffPlan]
	tainted := r.ago(12 * time.Minute)
	held := r.ago(5 * time.Minute)
	clock := tainted
	svc := screening.New(screening.Config{
		DB: r.s.db, Files: r.s.mgr, Events: r.s.rec, Now: func() time.Time { return clock },
	})
	if _, err := svc.Taint(r.ctx, by, "tool:webfetch", "hook"); err != nil {
		return err
	}
	clock = held
	id, err := r.id(held)
	if err != nil {
		return err
	}
	mem := core.Memory{
		ID: id, Kind: core.KindConvention, Name: "ops-paging-routes-first", Project: ReviewOps,
		Description: "Alert routes match top to bottom: keep the paging routes above every catch-all route.",
		Body:        "Copied from a vendor page the session fetched: the router stops at the first matching route.",
		Created:     held, Updated: held, ValidFrom: held, SourceSession: by.ID, Model: by.Model,
	}
	content, err := files.RenderMemory(mem)
	if err != nil {
		return err
	}
	reasons := screening.Strings([]screening.Code{screening.ReasonTaintedSession})
	h, err := svc.Hold(r.ctx, store.HeldWrite{
		Op: store.HeldOpWrite, Tool: "memory_write", ItemKind: core.ItemKindMemory, ItemID: mem.ID,
		Project: mem.Project, Name: mem.Name, MemoryKind: mem.Kind, SessionID: by.ID, Reasons: reasons,
		Content: content, Detail: map[string]string{"path": files.MemoryRelPath(mem.Project, mem.Name)},
	})
	if err != nil {
		return err
	}
	svc.RecordScreened(r.ctx, screening.Screened{
		ItemKind: core.ItemKindMemory, ItemID: mem.ID, Project: mem.Project, SessionID: by.ID, Kind: mem.Kind,
		Tool: "memory_write", Size: len(screening.MemoryText(mem)), Held: true,
	})
	r.add(ReviewHeldWrite, "/console/held", fmt.Sprintf("%s: the convention %s from %s, which read outside text",
		h.ID, mem.Name, by.Name), h.ID)
	return nil
}

// lockRequest stores the restricted account's lock declaration, which waits
// for the owner's approval on the Repo locks screen.
func (r *reviewSeeder) lockRequest() error {
	by := r.sessions[rRestricted]
	aliases, err := store.RepoLockAliasesFor(store.CWDIdentity{Origin: reviewLockOrigin}, "")
	if err != nil {
		return err
	}
	at := by.CreatedAt.Add(12 * time.Minute)
	res, err := store.DeclareRepoLock(r.ctx, r.s.db, store.RepoLockDeclaration{
		Name: "main", Aliases: aliases, Projects: []string{ReviewOps}, State: core.RepoLockRequested,
		CreatedBy: by.ID, Account: ReviewAccount,
	}, at)
	if err != nil {
		return err
	}
	if err := r.event(at, core.EventRepoLockChanged, by.ID, "", res.Lock.ID, map[string]any{
		"action": core.RepoLockActionRequested, "name": res.Lock.Name, "aliases": len(res.Added), "account": ReviewAccount,
	}); err != nil {
		return err
	}
	r.add(ReviewLockRequested, "/console/locks", fmt.Sprintf("%s: main on %s, declared by %s (%s)",
		res.Lock.ID, aliases[0].Alias, ReviewAccount, by.Name), res.Lock.ID)
	return nil
}

// ledgerItem tallies the review layer's ledger rows.
func (r *reviewSeeder) ledgerItem() error {
	rows, err := r.count(`SELECT COUNT(*) FROM session_usage`)
	if err != nil {
		return err
	}
	sessions, err := r.count(`SELECT COUNT(DISTINCT session_id) FROM session_usage`)
	if err != nil {
		return err
	}
	var tokens int
	if err := r.s.db.QueryRowContext(r.ctx, `SELECT COALESCE(SUM(total_tokens), 0) FROM sessions`).Scan(&tokens); err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	r.add(ReviewUsageLedger, "/console/diary", fmt.Sprintf("%d ledger rows from %d Claude Code sessions, %d tokens", rows, sessions, tokens))
	return nil
}

// ---------------------------------------------------------------------------
// the review layer's data
// ---------------------------------------------------------------------------

// reviewMemories are the working projects' memories.
var reviewMemories = []struct {
	project    string
	kind       core.MemoryKind
	name, desc string
}{
	{ReviewOps, core.KindConstraint, reviewMishapMemory, "Never run a schema migration while the nightly backup runs (01:00 to 03:00 UTC): the snapshot holds the table locks."},
	{ReviewOps, core.KindGotcha, "ops-alert-router-reload-drops-silences", "Reloading the alert router drops every active silence; re-apply them from the silences file after each reload."},
	{ReviewOps, core.KindRunbook, "ops-restore-from-snapshot", "Restore a volume from its newest snapshot: stop the writer, mount read-only, verify the checksum, then swap."},
	{ReviewOps, core.KindConvention, "ops-runbook-naming", "Runbooks are named for their system and action (ops-backup-restore), one action per runbook."},
	{ReviewOps, core.KindDecision, "ops-resolver-pair", "Internal DNS runs on a resolver pair with native zone transfer; the zone sync job was dropped."},
	{ReviewWeb, core.KindGotcha, "web-cache-purge-lag", "A CDN purge takes up to two minutes to reach every edge; wait that long before checking a fix."},
}

// reviewMishapMemory is the memory one mishap report names.
const reviewMishapMemory = "ops-no-migrations-during-backups"

// reviewMishaps are the agent.mishap reports: six typed, two untyped. The
// Overview's card shows six.
var reviewMishaps = []string{
	"shell-trap: an unquoted == in zsh stopped the cleanup script halfway; reran it with the argument quoted.",
	"wrong-target: restarted the staging alert router instead of the scratch one; it was back within a minute.",
	"live-state: wrote a test row into the live metrics database; deleted it right after.",
	"ignored-warning: ran the index migration although " + reviewMishapMemory + " says not to; the backup had already finished.",
	"fabricated: reported the disk inventory as verified before the health check had run; corrected it in the findings.",
	"other: left a scratch daemon running on a shared port for an hour.",
	"Forgot to release the claim on the alert routing task before ending the session.",
	"Pushed the runbook edit to the wrong branch and moved it back.",
}

// reviewFindings feed the finished sessions' findings.
var reviewFindings = []string{
	"Backup verification passed for every volume; one snapshot was 9 minutes late and is now alerted on.",
	"Alert routes reordered so paging routes match first; the catch-all no longer swallows disk alerts.",
	"Resolver pair answers for every internal zone; client switch-over is done for the build machines.",
	"Container image prune freed 41 GB; the prune job now keeps the last three tags per image.",
	"Restore runbook rewritten against the new snapshot tool; the old mount step is gone.",
	"Metrics volume measured at 2.1M series; the downsampling tiers are drafted in the plan note.",
	"Disk health inventory done: two disks show reallocated sectors and are on the replacement list.",
	"On-call handoff checklist written and linked from the rotation calendar.",
	"Log retention runbook drafted; the change itself waits on it.",
	"Firewall profile tested on the spare node; the owner picks the change window.",
}

// rPlan* are the review plans' slugs.
const (
	rPlanDNS      = "ops-dns-cutover"        // complete but for a dropped step
	rPlanRunbooks = "ops-runbook-refresh"    // steps and no note
	rPlanMetrics  = "ops-metrics-retention"  // stalled with a claimable step
	rPlanStorage  = "ops-storage-migration"  // a step waits on an open gate
	rPlanShared   = "observability-baseline" // steps in two projects
)

// rDay is one day, for the task and note specs' ages.
const rDay = 24 * time.Hour

// reviewOffPlan are ReviewOps's off-plan tasks, one per state.
var reviewOffPlan = []rTask{
	{key: "claim-live", project: ReviewOps, title: "Rotate the backup encryption key",
		body: "Rotate per ops-restore-from-snapshot's key section; keep the old key readable for 7 days.", created: 30 * time.Hour,
		holder: rLiveOffPlan, claimedAt: 31 * time.Minute, lease: 3 * time.Hour},
	{key: "claim-lapsed", project: ReviewOps, title: "Move the metrics exporter to its new port",
		body: "The exporter moves off the port the new proxy takes; update the scrape config in the same change.", created: 26 * time.Hour,
		holder: rLiveLapsed, claimedAt: 40 * time.Minute, lease: 15 * time.Minute},
	{key: "claimless", project: ReviewOps, title: "Tidy the alert routing table",
		body: "Started with seam task start: in progress, and nobody holds a claim on it.", created: 50 * time.Hour,
		claimless: 47 * time.Hour},
	{key: "blocker", project: ReviewOps, title: "Write the log retention runbook",
		body: "The runbook the retention change follows: what is kept, for how long, and how to restore a pruned day.", created: 70 * time.Hour},
	{key: "blocked", project: ReviewOps, title: "Apply the log retention change", deps: []string{"blocker"},
		body: "Waits on the runbook: the change is applied by following it.", created: 69 * time.Hour},
	{key: "gated", project: ReviewOps, title: "Turn on the stricter firewall profile",
		body: "Tested on the spare node; the switch drops idle connections, so it runs in a change window.", created: 45 * time.Hour,
		gate: core.TaskGate{Who: "owner", What: "pick the firewall change window", Decision: "Which night this week can the change run?"}},
	{key: "gate", project: ReviewOps, title: "Sign off the storage vendor renewal", kind: core.TaskKindGate,
		body: "The renewal covers the replacement disks; nothing is ordered before it is signed.", created: 90 * time.Hour,
		gate: core.TaskGate{Who: "owner", What: "sign the storage vendor renewal", ReadyWhen: "the signed renewal is in the shared folder"}},
	{key: "gate-a", project: ReviewOps, title: "Order the replacement disks", deps: []string{"gate"},
		body: "Two disks from the health inventory, plus one spare.", created: 89 * time.Hour},
	{key: "gate-b", project: ReviewOps, title: "Schedule the disk swap", deps: []string{"gate"},
		body: "One disk per night, with a scrub between swaps.", created: 89 * time.Hour},
	{key: "done-1", project: ReviewOps, title: "Renew the internal CA certificate", body: "Renewed; every service picked up the new chain.",
		created: 200 * time.Hour, close: core.TaskDone, closed: 180 * time.Hour},
	{key: "done-2", project: ReviewOps, title: "Prune the stale container images", body: "Pruned; the job keeps the last three tags.",
		created: 120 * time.Hour, close: core.TaskDone, closed: 100 * time.Hour},
	{key: "done-3", project: ReviewOps, title: "Write the on-call handoff checklist", body: "Written and linked from the rotation calendar.",
		created: 60 * time.Hour, close: core.TaskDone, closed: 20 * time.Hour},
	{key: "dropped-1", project: ReviewOps, title: "Evaluate a second log shipper", body: "Dropped: the current shipper's new release covers the gap.",
		created: 300 * time.Hour, close: core.TaskDropped, closed: 150 * time.Hour},
	{key: "dropped-2", project: ReviewOps, title: "Move the wiki to a new server", body: "Dropped: the wiki moves into the docs site instead.",
		created: 250 * time.Hour, close: core.TaskDropped, closed: 40 * time.Hour},
}

// reviewPlanSteps are the review plans' steps, blockers first.
var reviewPlanSteps = []rTask{
	// Complete but for a dropped step.
	{key: "dns-1", project: ReviewOps, plan: rPlanDNS, title: "Inventory every internal zone", created: 16 * rDay,
		close: core.TaskDone, closed: 15 * rDay},
	{key: "dns-2", project: ReviewOps, plan: rPlanDNS, title: "Stand up the new resolver pair", deps: []string{"dns-1"}, created: 16 * rDay,
		close: core.TaskDone, closed: 12 * rDay},
	{key: "dns-3", project: ReviewOps, plan: rPlanDNS, title: "Mirror the zones with a sync job", deps: []string{"dns-2"}, created: 16 * rDay,
		body: "Dropped: the new resolvers transfer zones natively.", close: core.TaskDropped, closed: 9 * rDay},
	{key: "dns-4", project: ReviewOps, plan: rPlanDNS, title: "Switch the clients to the new resolvers", deps: []string{"dns-2"}, created: 16 * rDay,
		close: core.TaskDone, closed: 6 * rDay},
	// Steps and no note.
	{key: "runbooks-1", project: ReviewOps, plan: rPlanRunbooks, title: "List the runbooks older than a year", created: 5 * rDay,
		close: core.TaskDone, closed: 4 * rDay},
	{key: "runbooks-2", project: ReviewOps, plan: rPlanRunbooks, title: "Rewrite the restore runbook", deps: []string{"runbooks-1"}, created: 5 * rDay,
		holder: rLiveNoNote, claimedAt: 44 * time.Minute, lease: 3 * time.Hour},
	{key: "runbooks-3", project: ReviewOps, plan: rPlanRunbooks, title: "Rewrite the failover runbook", deps: []string{"runbooks-2"}, created: 5 * rDay},
	// Stalled: nothing moved since the second step closed 14 days ago.
	{key: "metrics-1", project: ReviewOps, plan: rPlanMetrics, title: "Measure the current metrics volume", created: 22 * rDay,
		close: core.TaskDone, closed: 20 * rDay},
	{key: "metrics-2", project: ReviewOps, plan: rPlanMetrics, title: "Pick the downsampling tiers", deps: []string{"metrics-1"}, created: 22 * rDay,
		close: core.TaskDone, closed: 14 * rDay},
	{key: "metrics-3", project: ReviewOps, plan: rPlanMetrics, title: "Apply the downsampling rules", deps: []string{"metrics-2"}, created: 22 * rDay},
	{key: "metrics-4", project: ReviewOps, plan: rPlanMetrics, title: "Drop the raw series older than 90 days", deps: []string{"metrics-3"}, created: 22 * rDay},
	// A step waits on an open gate.
	{key: "storage-1", project: ReviewOps, plan: rPlanStorage, title: "Inventory the disks and their health", created: 8 * rDay,
		close: core.TaskDone, closed: 5 * rDay},
	{key: "storage-2", project: ReviewOps, plan: rPlanStorage, title: "Benchmark the new storage pool", deps: []string{"storage-1"}, created: 8 * rDay,
		holder: rLiveLapsed, claimedAt: 9 * time.Minute, lease: 3 * time.Hour},
	{key: "storage-3", project: ReviewOps, plan: rPlanStorage, title: "Owner approves the maintenance window", kind: core.TaskKindGate, created: 8 * rDay,
		gate: core.TaskGate{Who: "owner", What: "approve the storage maintenance window", Decision: "Saturday night or Sunday morning?"}},
	{key: "storage-4", project: ReviewOps, plan: rPlanStorage, title: "Run the storage migration", deps: []string{"storage-2", "storage-3"}, created: 8 * rDay},
	{key: "storage-5", project: ReviewOps, plan: rPlanStorage, title: "Verify a restore from the new pool", deps: []string{"storage-4"}, created: 8 * rDay},
	// One slug, steps in two projects.
	{key: "shared-ops-1", project: ReviewOps, plan: rPlanShared, title: "Agree the baseline panels", created: 10 * rDay,
		close: core.TaskDone, closed: 8 * rDay},
	{key: "shared-ops-2", project: ReviewOps, plan: rPlanShared, title: "Add the baseline dashboard to the ops services", deps: []string{"shared-ops-1"}, created: 10 * rDay},
	{key: "shared-web-1", project: ReviewWeb, plan: rPlanShared, title: "Add the baseline dashboard to the web services", created: 10 * rDay,
		holder: rLiveWeb, claimedAt: 18 * time.Minute, lease: 3 * time.Hour},
	{key: "shared-web-2", project: ReviewWeb, plan: rPlanShared, title: "Alert on the web error budget", deps: []string{"shared-web-1"}, created: 10 * rDay},
}

// reviewPlanNotes are the narrative notes; rPlanRunbooks has none.
var reviewPlanNotes = []struct {
	plan, project, title, desc string
	ago                        time.Duration
}{
	{rPlanDNS, ReviewOps, "PLAN: ops-dns-cutover -- internal DNS on a resolver pair", "Move every internal zone to the new resolver pair and switch the clients; the old resolvers retire after.", 16*rDay + 2*time.Hour},
	{rPlanMetrics, ReviewOps, "PLAN: ops-metrics-retention -- downsample, then drop raw series", "Measure the metrics volume, pick downsampling tiers, apply them, then drop raw series older than 90 days.", 22*rDay + 3*time.Hour},
	{rPlanStorage, ReviewOps, "PLAN: ops-storage-migration -- move the volumes to the new pool", "Inventory the disks, benchmark the new pool, migrate in the window the owner approves, then verify a restore.", 8*rDay + time.Hour},
	{rPlanShared, ReviewOps, "PLAN: observability-baseline -- one dashboard per service", "Agree the baseline panels once, then give every ops and web service the same dashboard and an error-budget alert.", 10*rDay + 2*time.Hour},
}

// reviewQuiet are the projects nobody touched for 30+ days.
var reviewQuiet = []struct {
	slug, desc, finding   string
	kind                  core.MemoryKind
	memoryName, memory    string
	tasks                 []string
	retired, confidential bool
}{
	{slug: "example-analytics", desc: "Product analytics pipeline", finding: "Event schema frozen for the quarter.", kind: core.KindDecision, memoryName: "schema-freeze", memory: "The event schema is frozen until the quarter ends; new fields wait for the next version."},
	{slug: "example-android", desc: "Android app", finding: "Release build signed and uploaded for review."},
	{slug: "example-api-docs", desc: "Public API reference site", finding: "Reference pages regenerated from the spec.", kind: core.KindConvention, memoryName: "spec-is-source", memory: "The API spec is the source of the reference pages; never edit a generated page by hand."},
	{slug: "example-archive", desc: "Cold storage of finished project files", finding: "Archive index rebuilt."},
	{slug: "example-audit", desc: "Internal audit evidence and findings", finding: "Evidence folder reorganized by control.", kind: core.KindConstraint, memoryName: "evidence-read-only", memory: "Evidence files are read-only once filed; corrections go in a new file that names the old one.", confidential: true},
	{slug: "example-billing-legacy", desc: "The billing system before the rewrite", finding: "Last invoices reconciled against the new system."},
	{slug: "example-blog", desc: "Engineering blog", finding: "Draft on the resolver migration published."},
	{slug: "example-bots", desc: "Chat bots for the team channels", finding: "Standup bot moved to the new token.", kind: core.KindGotcha, memoryName: "token-scope", memory: "The bot token needs the history scope or replies in threads fail silently."},
	{slug: "example-cli", desc: "Command-line client", finding: "Completion scripts regenerated.", tasks: []string{"Add a --json flag to the list command", "Ship shell completion for fish"}},
	{slug: "example-crm-sync", desc: "Sync between the CRM and the warehouse", finding: "Nightly sync verified after the field rename."},
	{slug: "example-data-lake", desc: "Raw event storage", finding: "Partition layout documented.", kind: core.KindReference, memoryName: "partition-layout", memory: "Raw events are partitioned by day and source; a partition is immutable once its day closes."},
	{slug: "example-design-system", desc: "Shared UI components", finding: "Button and input tokens aligned."},
	{slug: "example-desktop", desc: "Desktop client", finding: "Auto-update channel switched to stable."},
	{slug: "example-email", desc: "Transactional email templates", finding: "Templates moved to the shared layout.", kind: core.KindGotcha, memoryName: "preheader-length", memory: "Keep the preheader under 90 characters or some clients show the body's first line instead."},
	{slug: "example-etl", desc: "Nightly ETL jobs", finding: "Job retries capped at three."},
	{slug: "example-experiments", desc: "Short experiments and spikes", finding: "Spike on streaming exports closed."},
	{slug: "example-feature-flags", desc: "Feature flag service", finding: "Stale flags removed.", kind: core.KindConvention, memoryName: "flag-expiry", memory: "Every flag carries an expiry date; the weekly report lists the expired ones."},
	{slug: "example-forms", desc: "Form builder", finding: "Validation messages translated."},
	{slug: "example-geo", desc: "Geocoding helpers", finding: "Cache warmed for the new regions."},
	{slug: "example-graphql", desc: "GraphQL gateway", finding: "Query cost limits applied.", kind: core.KindDecision, memoryName: "cost-limits", memory: "Queries are refused above a cost of 1000; persisted queries are exempt."},
	{slug: "example-i18n", desc: "Translations", finding: "String freeze lifted."},
	{slug: "example-infra-old", desc: "Infrastructure before the cluster move", finding: "Last service moved off the old machines."},
	{slug: "example-ios", desc: "iOS app", finding: "TestFlight build shared with the beta group.", kind: core.KindGotcha, memoryName: "build-number", memory: "The build number must grow with every upload, even when the version stays the same.", tasks: []string{"Fix the share sheet crash on older devices"}},
	{slug: "example-legacy-site", desc: "The old marketing site", finding: "Redirects to the new site verified.", retired: true},
	{slug: "example-load-tests", desc: "Load test scenarios", finding: "Checkout scenario updated for the new flow."},
	{slug: "example-maps", desc: "Map tiles", finding: "Tile cache size reviewed."},
	{slug: "example-ml-models", desc: "Model training scripts", finding: "Training data snapshot tagged.", kind: core.KindRunbook, memoryName: "retrain", memory: "Retrain from the tagged snapshot only; record the tag in the model card."},
	{slug: "example-mobile-api", desc: "Backend for the mobile apps", finding: "Deprecated endpoints logged for a month."},
	{slug: "example-notebooks", desc: "Data notebooks", finding: "Notebook outputs stripped before commit."},
	{slug: "example-payments-sandbox", desc: "Payment provider sandbox", finding: "Sandbox keys rotated."},
	{slug: "example-pdf", desc: "PDF rendering service", finding: "Font fallback fixed for long names.", kind: core.KindGotcha, memoryName: "font-subset", memory: "Embedded fonts are subset per document; a missing glyph renders as a box, never an error."},
	{slug: "example-plugins", desc: "Editor plugins", finding: "Plugin manifest bumped to the new format."},
	{slug: "example-reports", desc: "Monthly reports", finding: "October report sent.", kind: core.KindConvention, memoryName: "report-day", memory: "Reports go out on the second working day of the month."},
	{slug: "example-scheduler", desc: "Job scheduler", finding: "Timezone handling covered by tests.", tasks: []string{"Retry jobs that time out once before alerting"}},
	{slug: "example-search", desc: "Search index", finding: "Index rebuilt with the new analyzer.", kind: core.KindDecision, memoryName: "analyzer", memory: "The index uses the language-aware analyzer; exact-match fields keep the keyword type.", tasks: []string{"Boost recent documents in the ranking", "Reindex the archived posts"}},
	{slug: "example-sdk-js", desc: "JavaScript SDK", finding: "Types published with the package."},
	{slug: "example-sdk-python", desc: "Python SDK", finding: "Wheels built for the new runtime.", tasks: []string{"Drop the deprecated sync client"}},
	{slug: "example-status-page", desc: "Status page", finding: "Incident template shortened."},
	{slug: "example-support", desc: "Support tooling", finding: "Macros reviewed with the support lead."},
	{slug: "example-wiki", desc: "Team wiki", finding: "Stale pages flagged for review."},
}
