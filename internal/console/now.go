// The Now screen: the full exploded view of what every agent is doing right
// now, across every project and plan at once. It inverts the project
// workspace's task-centric lens -- here the AGENT is the unit: one card per
// live session carrying its claims, its lease countdowns, and its freshest
// output, with the loose ends (claims nobody live is holding), the plans in
// motion, the cross-project ready queue, and the live wire below.
//
// The arcade (gamification) rides on top: the day tape, the personal-records
// rail, the hot-streak pulse, and celebration moments, every number judged
// from recorded activity.
package console

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// nowFresh* are the presentation thresholds for an agent card's heartbeat
// freshness. They are display judgments only -- liveness itself stays
// core.Session.LiveAsOf with the configured idle TTL.
const (
	nowFreshHot  = 5 * time.Minute  // heartbeat moments ago: the agent is mid-flight
	nowFreshWarm = 15 * time.Minute // recently active; older-but-live renders quiet
)

// nowHotStreakFloor is how many events the last ten minutes need before the
// pulse reads "hot". The count is always shown verbatim; the floor only picks
// the styling, so the claim stays verifiable either way.
const nowHotStreakFloor = 15

// nowTrailKinds are the business-level kinds an agent card's trail shows --
// what the agent produced, not its transport noise.
var nowTrailKinds = []core.EventKind{
	core.EventSessionStarted, core.EventMemoryWritten, core.EventMemorySuperseded,
	core.EventNoteWritten, core.EventTaskTransition, core.EventTrialRecorded,
	core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved,
	core.EventSubagentCaptured,
}

// nowData is the page payload.
type nowData struct {
	Generated  time.Time    `json:"generated"`
	AgentsLive int          `json:"agentsLive"`
	InFlight   int          `json:"tasksInFlight"`
	Moving     int          `json:"plansMoving"`
	EventsHour int          `json:"eventsLastHour"`
	Pulse      spark        `json:"-"`
	Scope      string       `json:"scope,omitempty"`
	Scopes     []nowScopeVM `json:"scopes,omitempty"`
	Agents     []nowAgentVM `json:"agents"`
	Loose      []nowLooseVM `json:"looseEnds"`
	Plans      []nowPlanVM  `json:"plans"`
	UpNext     []nowReadyVM `json:"upNext"`
	// ReadyTotal is every task claimable this instant in scope; UpNext lists
	// the first of them.
	ReadyTotal int        `json:"readyTotal"`
	Wire       []eventRow `json:"wire"`

	// Arcade is the gamification layer: nil only when it could not be judged
	// (nowArcade is failure-soft), so the page drops it rather than show wrong
	// numbers.
	Arcade *nowArcadeVM `json:"arcade,omitempty"`

	// liveAll counts every live session whatever the scope, and shippedToday
	// the plans whose last step closed today in scope: what the arcade reads
	// after nowWork (never in the JSON answer).
	liveAll      int
	shippedToday []store.ProjectPlan
}

// nowScopeVM is one entry of the project filter strip.
type nowScopeVM struct {
	Slug   string `json:"slug"`
	Live   int    `json:"live"`
	Active bool   `json:"active"`
}

// nowAgentVM is one live agent card.
type nowAgentVM struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Title heads the card: the title of the task the agent holds (its first
	// claim), else its name -- what Now showed before sessions had titles.
	Title    string       `json:"title"`
	Project  string       `json:"project"` // "global" for the empty scope
	Harness  string       `json:"harness,omitempty"`
	Model    string       `json:"model,omitempty"`
	Account  string       `json:"account,omitempty"` // restricted account that started it ("" = owner)
	Fresh    string       `json:"fresh"`             // "hot" | "warm" | "quiet"
	Beat     time.Time    `json:"lastBeat"`
	Dur      string       `json:"duration,omitempty"`
	Tokens   int          `json:"tokens,omitempty"`
	Favorite bool         `json:"favorite,omitempty"`
	Claims   []nowClaimVM `json:"claims"`
	// Lapsed are the claims this agent held whose lease ran out: the same rows
	// Loose ends lists, in the same words, so the card and the list agree.
	Lapsed []nowLooseVM `json:"lapsedClaims,omitempty"`
	Trail  []eventRow   `json:"trail"`

	// Contrib is the agent's today line, a gamification surface: nil when the
	// card's trail could not be read.
	Contrib *nowContribVM `json:"contrib,omitempty"`
}

// nowClaimVM is a live claim on an agent card, with the data the client-side
// lease ticker reads.
type nowClaimVM struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	PlanSlug  string `json:"planSlug,omitempty"`
	LeaseExp  string `json:"leaseExp,omitempty"` // RFC3339 for the countdown ticker
	LeaseLeft string `json:"leaseLeft,omitempty"`
}

// nowContribVM is what one agent shipped today (judged from its own events).
type nowContribVM struct {
	Memories int `json:"memories"`
	Notes    int `json:"notes"`
	Closed   int `json:"tasksClosed"`
}

// nowLooseVM is an in_progress task no live agent card is carrying: a lapsed
// lease, a claimless start, or a holder that went quiet mid-lease.
type nowLooseVM struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	PlanSlug string `json:"planSlug,omitempty"`
	Holder   string `json:"holder,omitempty"` // session name when it resolves, else the short id
	HolderID string `json:"holderId,omitempty"`
	// HolderLink is the holder linked to its session page when it resolved to
	// one this request reads (holderLink), else its raw id and no link.
	HolderLink *taskSession `json:"-"`
	State      string       `json:"state"` // "lapsed" | "claimless" | "quiet-holder"
	// Word is the row's one state phrase, in the task vocabulary (wordFor):
	// claim lapsed, unclaimed, or in progress for a quiet holder's live claim.
	Word taskWord `json:"word"`
	// LapsedAt is when the claim lapsed (its lease's end), set only while Word
	// is claim lapsed, so the phrase reads "claim lapsed 12m ago".
	LapsedAt  *time.Time `json:"lapsedAt,omitempty"`
	LeaseExp  string     `json:"leaseExp,omitempty"`
	LeaseLeft string     `json:"leaseLeft,omitempty"`
	Since     time.Time  `json:"since"` // last movement (updated_at)
	// Releasable marks the states whose claim can be cleared without a confirm:
	// the lease is already dead, so there is no live holder to interrupt.
	Releasable bool `json:"releasable"`
}

// nowPlanVM is one moving-plans rail card.
type nowPlanVM struct {
	Project   string    `json:"project"`
	Slug      string    `json:"slug"`
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	InFlight  int       `json:"inFlight"`
	Claimable int       `json:"claimable"`
	DonePct   int       `json:"donePct"`
	DoingPct  int       `json:"doingPct"`
	Moving    bool      `json:"moving"` // a step moved within the last 24h
	Last      time.Time `json:"lastActivity"`
	// Href is the plan's page when it has one in this project (planPageHref),
	// else the project's Plans & tasks tab: a card never links to a 404.
	Href string `json:"href"`
	// Holders are the live agents holding a step of the plan with a live
	// claim, in the agent cards' order: the plan is in progress while any
	// does.
	Holders []nowHolderVM `json:"holders,omitempty"`
}

// nowHolderVM is a live agent holding a plan step: its session and name.
type nowHolderVM struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// nowReadyVM is one cross-project ready-queue row: claimable this instant.
type nowReadyVM struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Project  string    `json:"project"`
	PlanSlug string    `json:"planSlug,omitempty"`
	Unblocks int       `json:"unblocks"`
	Created  time.Time `json:"created"`
}

// nowArcadeVM is the gamification layer's payload.
type nowArcadeVM struct {
	Tape    []nowTapeCellVM `json:"tape"`
	Records []nowRecordVM   `json:"records"`
	Hot     nowHotVM        `json:"hotStreak"`
	Moments []nowMomentVM   `json:"moments,omitempty"`
}

// nowTapeCellVM is one day-tape cell: today's judged count against the
// trailing week's daily average.
type nowTapeCellVM struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	N     int    `json:"n"`
	Avg   string `json:"avg"`  // "2.3" -- the trailing 7-day daily average
	Tone  string `json:"tone"` // "up" | "down" | ""
}

// nowRecordVM is one personal best on the records rail.
type nowRecordVM struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	N     int    `json:"n"`
	Day   string `json:"day,omitempty"`
	Today bool   `json:"today,omitempty"` // set today -- worth a glow
	// Seen is a best the ledger found when it began, on Day, rather than one
	// it saw fall (store.RecordMark): never a record set today.
	Seen bool `json:"seen,omitempty"`
}

// nowHotVM is the ten-minute pulse: the verbatim count, and whether it clears
// the hot floor.
type nowHotVM struct {
	Count int  `json:"count"`
	Hot   bool `json:"hot"`
}

// nowMomentVM is one celebration moment: a record broken or a plan shipped
// today. The client bursts only when a moment NEWLY appears during a live
// morph -- a moment already on screen at load renders as a plain chip.
type nowMomentVM struct {
	ID   string `json:"id"`
	Copy string `json:"copy"`
}

func (s *Service) nowPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	data, err := s.buildNow(ctx, now, scope)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "now", pageData{Title: "Now", Active: "now", Data: data})
}

// buildNow assembles the page: the work in motion (nowWork), then each agent
// card's trail, the wire, the pulse and the arcade. The primary queries fail
// the page; per-card trails and the arcade degrade soft, like every other
// strip.
func (s *Service) buildNow(ctx context.Context, now time.Time, scope string) (nowData, error) {
	data, err := s.nowWork(ctx, now, scope)
	if err != nil {
		return nowData{}, err
	}
	dayStart := localMidnight(now)

	// Trail + today's contribution from one newest-first read per card.
	// Best-effort: a trail failure leaves the card, not the page.
	if s.cfg.Events != nil {
		for i := range data.Agents {
			vm := &data.Agents[i]
			evs, terr := s.eventLog(ctx).LatestBySession(ctx, vm.ID, nowTrailKinds, 100)
			if terr != nil {
				s.logger.Warn("console: now agent trail", "session", vm.ID, "error", terr)
			}
			for _, e := range evs {
				if len(vm.Trail) < 3 {
					vm.Trail = append(vm.Trail, toEventRow(e))
				}
			}
			if terr == nil {
				vm.Contrib = nowContribution(evs, dayStart)
			}
		}
	}

	// The wire: the freshest business events, scope-filtered. Best-effort.
	if wire, werr := s.recentEvents(ctx, 40); werr != nil {
		s.logger.Warn("console: now wire", "error", werr)
	} else {
		for _, row := range wire {
			if !nowInScope(scope, row.Project) {
				continue
			}
			if len(data.Wire) < 12 {
				data.Wire = append(data.Wire, row)
			}
		}
	}

	// The pulse: fleet-wide event volume over the last hour, all kinds -- the
	// system's heartbeat, deliberately unfiltered by scope. Best-effort.
	var pulseTimes []time.Time
	if s.cfg.Events != nil {
		if ts, perr := s.eventLog(ctx).TimestampsSince(ctx, now.Add(-time.Hour), 4000); perr != nil {
			s.logger.Warn("console: now pulse", "error", perr)
		} else {
			pulseTimes = ts
		}
	}
	data.EventsHour = len(pulseTimes)
	data.Pulse = nowPulseSpark(pulseTimes, now)

	data.Arcade = s.nowArcade(ctx, now, dayStart, data.liveAll, data.shippedToday, pulseTimes)
	return data, nil
}

// nowInScope reports whether a project's work is in a Now scope: "" is every
// scope, "global" the empty project slug (a real scope, displayed as
// "global" so the chip value round-trips), anything else that project.
func nowInScope(scope, project string) bool {
	switch scope {
	case "":
		return true
	case "global":
		return project == ""
	default:
		return project == scope
	}
}

// nowWork is the work in motion, the model Now draws and the Overview's Work
// in motion row and live strip summarize (plan:console-ux-pass UX 12): the
// live fleet with the claims it holds, the loose ends, the unfinished plans
// with the agents holding their steps, and the ready queue, read through the
// request's handle, so a locked project's work is absent. It writes nothing
// and reads no event trail.
func (s *Service) nowWork(ctx context.Context, now time.Time, scope string) (nowData, error) {
	ttl := s.cfg.SessionIdleTTL
	if ttl <= 0 {
		ttl = core.SessionIdleTTL
	}

	// The live fleet: every session the one rule calls live
	// (core.Session.LiveAsOf), a quiet holder of a live claim included -- the
	// list's idle cutoff keeps those too.
	sessions, err := store.ListSessions(ctx, s.db(ctx), core.SessionActive, now.Add(-ttl), 200)
	if err != nil {
		return nowData{}, fmt.Errorf("now: sessions: %w", err)
	}
	var live []core.Session
	liveByID := map[string]core.Session{}
	for _, sess := range sessions {
		if !sess.LiveAsOf(now, ttl) {
			continue
		}
		live = append(live, sess)
		liveByID[sess.ID] = sess
	}

	inProg, err := store.AllTasksByStatus(ctx, s.db(ctx), core.TaskInProgress)
	if err != nil {
		return nowData{}, fmt.Errorf("now: in-progress tasks: %w", err)
	}

	inScope := func(project string) bool { return nowInScope(scope, project) }
	scopeName := func(project string) string {
		if project == "" {
			return "global"
		}
		return project
	}

	data := nowData{Generated: now, Scope: scope}

	// Claims held by a live session ride that agent's card; everything else in
	// flight is a loose end.
	namer := s.sessionNamer(ctx)
	claims := map[string][]core.Task{}
	var looseTasks []core.Task
	for _, t := range inProg {
		// A gate someone marked in progress is no agent's work in flight: it
		// waits on a person, and stays with the gates (looseClaim).
		if !inScope(t.ProjectSlug) || t.IsGate() {
			continue
		}
		data.InFlight++
		holder, holderLive := liveByID[t.ClaimedBy]
		if t.ClaimLive(now) && holderLive {
			claims[holder.ID] = append(claims[holder.ID], t)
			continue
		}
		looseTasks = append(looseTasks, t)
	}
	// Each loose end reads in the task vocabulary, the words every other task
	// surface prints for the same row.
	words, err := s.wordTasks(ctx, looseTasks, now)
	if err != nil {
		return nowData{}, fmt.Errorf("now: loose-end words: %w", err)
	}
	// A live agent's own lapsed claims ride its card too, worded as here.
	lapsedBy := map[string][]nowLooseVM{}
	for _, t := range looseTasks {
		loose := nowLooseVM{
			ID: t.ID, Title: t.Title, Project: t.ProjectSlug, PlanSlug: t.PlanSlug,
			Since: t.UpdatedAt, Word: words[t.ID],
		}
		// looseClaim (console.go) is the one rule the Tasks badge counts by too.
		switch looseClaim(t, now) {
		case "claimless":
			loose.State = "claimless"
		case "lapsed":
			loose.State = "lapsed"
			loose.Releasable = true
		default:
			// Lease live, but its holder is not among the live sessions above. A
			// live claim keeps its session live, so the holder has ended, is not
			// active, or lies outside this view or the list's cap.
			loose.State = "quiet-holder"
		}
		if t.ClaimedBy != "" {
			loose.HolderID = t.ClaimedBy
			loose.Holder = shortID(t.ClaimedBy)
			loose.HolderLink = holderLink(t.ClaimedBy, namer)
			if loose.HolderLink.ID != "" {
				loose.Holder = loose.HolderLink.Name
			} else {
				loose.HolderLink.Name = loose.Holder
			}
		}
		if t.LeaseExpiresAt != nil {
			loose.LeaseExp = t.LeaseExpiresAt.UTC().Format(time.RFC3339)
			loose.LeaseLeft = durUntil(*t.LeaseExpiresAt, now)
			if loose.Word.Key == wordLapsed {
				at := t.LeaseExpiresAt.UTC()
				loose.LapsedAt = &at
			}
		}
		data.Loose = append(data.Loose, loose)
		if _, live := liveByID[t.ClaimedBy]; live && loose.State == "lapsed" {
			lapsedBy[t.ClaimedBy] = append(lapsedBy[t.ClaimedBy], loose)
		}
	}
	sort.SliceStable(data.Loose, func(i, j int) bool { return data.Loose[i].Since.After(data.Loose[j].Since) })

	dayStart := localMidnight(now)
	data.liveAll = len(live)

	// One card per live agent, freshest heartbeat first.
	sort.SliceStable(live, func(i, j int) bool {
		if !live[i].UpdatedAt.Equal(live[j].UpdatedAt) {
			return live[i].UpdatedAt.After(live[j].UpdatedAt)
		}
		return live[i].ID > live[j].ID
	})
	// The Sessions screen's headline (sessionHeadlines) titles a card that
	// holds no claim by its first prompt; a held claim still heads it below.
	heads := s.sessionHeadlines(ctx, live, now)
	holders := map[string][]nowHolderVM{}
	for _, sess := range live {
		if !inScope(sess.ProjectSlug) {
			continue
		}
		vm := nowAgentVM{
			ID: sess.ID, Name: sess.Name, Project: scopeName(sess.ProjectSlug),
			Harness: harnessOf(sess), Model: sess.Model, Account: sess.Account,
			Fresh: nowFreshness(now.Sub(sess.UpdatedAt)), Beat: sess.UpdatedAt,
			Dur: sessionDuration(sess, now), Tokens: sess.Tokens.Total, Favorite: sess.Favorite,
		}
		if vm.Name == "" {
			vm.Name = shortID(sess.ID)
		}
		vm.Title = cmp.Or(heads[sess.ID].Text, vm.Name)
		for _, t := range claims[sess.ID] {
			claim := nowClaimVM{ID: t.ID, Title: t.Title, Project: t.ProjectSlug, PlanSlug: t.PlanSlug}
			if t.LeaseExpiresAt != nil {
				claim.LeaseExp = t.LeaseExpiresAt.UTC().Format(time.RFC3339)
				claim.LeaseLeft = durUntil(*t.LeaseExpiresAt, now)
			}
			vm.Claims = append(vm.Claims, claim)
		}
		if len(vm.Claims) > 0 && vm.Claims[0].Title != "" {
			vm.Title = vm.Claims[0].Title
		}
		vm.Lapsed = lapsedBy[sess.ID]
		for _, c := range vm.Claims {
			if c.PlanSlug != "" {
				key := nowPlanKey(c.Project, c.PlanSlug)
				if !slices.ContainsFunc(holders[key], func(h nowHolderVM) bool { return h.ID == vm.ID }) {
					holders[key] = append(holders[key], nowHolderVM{ID: vm.ID, Name: vm.Name})
				}
			}
		}
		data.Agents = append(data.Agents, vm)
	}
	data.AgentsLive = len(data.Agents)

	// Scope chips, from every live agent regardless of the current filter, so
	// the strip always offers the way back out.
	perScope := map[string]int{}
	for _, sess := range live {
		perScope[scopeName(sess.ProjectSlug)]++
	}
	slugs := make([]string, 0, len(perScope))
	for slug := range perScope {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	if scope != "" && perScope[scope] == 0 {
		slugs = append(slugs, scope) // keep the active filter visible even when it matches nothing
		sort.Strings(slugs)
	}
	for _, slug := range slugs {
		data.Scopes = append(data.Scopes, nowScopeVM{Slug: slug, Live: perScope[slug], Active: slug == scope})
	}

	// Moving plans: every incomplete plan across projects, freshest first.
	rollups, err := store.AllPlanRollups(ctx, s.db(ctx))
	if err != nil {
		return nowData{}, fmt.Errorf("now: plan rollups: %w", err)
	}
	data.shippedToday = []store.ProjectPlan{}
	for _, p := range rollups {
		if !inScope(p.Project) {
			continue
		}
		if p.Done >= p.Total {
			if p.Total > 0 && !p.LastActivity.Before(dayStart) {
				data.shippedToday = append(data.shippedToday, p)
			}
			continue
		}
		vm := nowPlanVM{
			Project: p.Project, Slug: p.Slug, Total: p.Total, Done: p.Done,
			InFlight: p.InFlight, Claimable: p.Claimable,
			DonePct: percent(p.Done, p.Total), DoingPct: percent(p.InFlight, p.Total),
			Moving: now.Sub(p.LastActivity) < 24*time.Hour, Last: p.LastActivity,
			Holders: holders[nowPlanKey(p.Project, p.Slug)],
		}
		if vm.Moving {
			data.Moving++
		}
		if len(data.Plans) < 12 {
			href, err := s.planPageHref(ctx, p.Project, p.Slug)
			if err != nil {
				return nowData{}, fmt.Errorf("now: plan page: %w", err)
			}
			if href == "" {
				href = projectTasksHref(p.Project)
			}
			vm.Href = href
			data.Plans = append(data.Plans, vm)
		}
	}

	// Up next: claimable this instant, across every project and plan.
	ready, err := store.AllReadyTasks(ctx, s.db(ctx))
	if err != nil {
		return nowData{}, fmt.Errorf("now: ready tasks: %w", err)
	}
	for _, t := range ready {
		if !inScope(t.ProjectSlug) {
			continue
		}
		data.ReadyTotal++
		if len(data.UpNext) >= 12 {
			continue
		}
		vm := nowReadyVM{ID: t.ID, Title: t.Title, Project: t.ProjectSlug, PlanSlug: t.PlanSlug, Created: t.CreatedAt}
		if unblocked, uerr := store.TasksBlockedBy(ctx, s.db(ctx), t.ID); uerr == nil {
			vm.Unblocks = len(unblocked)
		}
		data.UpNext = append(data.UpNext, vm)
	}
	return data, nil
}

// nowPlanKey keys a plan by its project and slug: one slug may name plans in
// two projects.
func nowPlanKey(project, slug string) string { return project + "\x00" + slug }

// planPageHref is a plan's page in project, its project named (planHref),
// when the plan has a narrative there: plans.CompositionIn resolves the slug
// in that project through the request's handle. "" means link elsewhere (the
// project's steps), never to a 404.
func (s *Service) planPageHref(ctx context.Context, project, slug string) (string, error) {
	if slug == "" {
		return "", nil
	}
	_, _, ok, err := plans.CompositionIn(ctx, s.db(ctx), project, slug)
	if err != nil || !ok {
		return "", err
	}
	return planHref(project, slug), nil
}

// projectTasksHref is a project's Plans & tasks tab, where a plan without a
// page of its own is read. The global scope has no project page, so its plans
// fall back to the Plans screen.
func projectTasksHref(project string) string {
	if project == "" {
		return "/console/plans"
	}
	return "/console/projects/" + url.PathEscape(project) + "?tab=tasks"
}

// nowFreshness buckets a heartbeat age for card presentation.
func nowFreshness(age time.Duration) string {
	switch {
	case age < nowFreshHot:
		return "hot"
	case age < nowFreshWarm:
		return "warm"
	default:
		return "quiet"
	}
}

// nowContribution counts what an agent shipped today from its own event trail.
func nowContribution(evs []core.Event, dayStart time.Time) *nowContribVM {
	c := &nowContribVM{}
	for _, e := range evs {
		if e.TS.Before(dayStart) {
			continue
		}
		switch e.Kind {
		case core.EventMemoryWritten:
			c.Memories++
		case core.EventNoteWritten:
			c.Notes++
		case core.EventTaskTransition:
			// An update of a task already done (from == to) closed nothing; an
			// event without from counts by its to, as it always has.
			if to, _ := e.Payload["to"].(string); to == string(core.TaskDone) && !statusUnmoved(e.Payload) {
				c.Closed++
			}
		}
	}
	return c
}

// nowPulseSpark buckets event timestamps into twelve five-minute cells over
// the last hour.
func nowPulseSpark(times []time.Time, now time.Time) spark {
	const buckets = 12
	step := time.Hour / buckets
	points := make([]store.TrendBucket, buckets)
	start := now.Add(-time.Hour)
	for i := range points {
		points[i].Label = fmt.Sprintf("-%dm", int((time.Duration(buckets-i) * step).Minutes()))
	}
	for _, t := range times {
		i := max(0, min(int(t.Sub(start)/step), buckets-1))
		points[i].Count++
	}
	return spark{Points: points, Label: "Events per five minutes over the last hour"}
}

// nowArcade assembles the gamification layer. Failure-soft: an arcade error is
// logged and drops the layer, never the page.
func (s *Service) nowArcade(ctx context.Context, now, dayStart time.Time, liveAgents int, shippedToday []store.ProjectPlan, pulseTimes []time.Time) *nowArcadeVM {
	dayKey := dayStart.Format("2006-01-02")
	today, err := store.GamificationDayCounts(ctx, s.db(ctx), dayStart, dayStart.AddDate(0, 0, 1))
	if err != nil {
		s.logger.Warn("console: now arcade day counts", "error", err)
		return nil
	}
	week, err := store.GamificationDayCounts(ctx, s.db(ctx), dayStart.AddDate(0, 0, -7), dayStart)
	if err != nil {
		s.logger.Warn("console: now arcade week counts", "error", err)
		return nil
	}

	arc := &nowArcadeVM{}
	cell := func(key, label string, n, weekTotal int) {
		avg := float64(weekTotal) / 7
		tone := ""
		switch {
		case float64(n) > avg && n > 0:
			tone = "up"
		case float64(n) < avg:
			tone = "down"
		}
		arc.Tape = append(arc.Tape, nowTapeCellVM{
			Key: key, Label: label, N: n, Avg: strings.TrimSuffix(fmt.Sprintf("%.1f", avg), ".0"), Tone: tone,
		})
	}
	cell("tasks", "tasks closed", today.TasksClosed, week.TasksClosed)
	cell("memories", "memories written", today.MemoriesWritten, week.MemoriesWritten)
	cell("notes", "notes written", today.NotesWritten, week.NotesWritten)
	cell("plans", "plans touched", today.PlansTouched, week.PlansTouched)
	cell("sessions", "sessions started", today.Sessions, week.Sessions)

	recs, crossings, err := store.EvaluateGamificationRecordsWith(ctx, s.db(ctx), s.writeDB(), today, liveAgents, now)
	if err != nil {
		s.logger.Warn("console: now arcade records", "error", err)
		return nil
	}
	record := func(key, label string, mark store.RecordMark) {
		arc.Records = append(arc.Records, nowRecordVM{
			Key: key, Label: label, N: mark.N, Day: mark.Day, Seen: mark.Seen,
			Today: mark.Day == dayKey && mark.N > 0 && !mark.Seen,
		})
	}
	record("tasks_day", "most tasks closed in a day", recs.TasksDay)
	record("memories_day", "most memories written in a day", recs.MemoriesDay)
	record("live_agents", "most agents live at once", recs.LiveAgents)

	// Celebration ledger: a crossing mints its event at most once per record
	// per local day (RecordOnce on the record+day key), so a record climbing
	// all afternoon celebrates once, not on every render.
	for _, c := range crossings {
		if s.cfg.Events == nil {
			break
		}
		_, _, rerr := s.eventLog(ctx).RecordOnce(ctx, core.Event{
			Kind:   core.EventRecordBroken,
			ItemID: c.Key + ":" + dayKey,
			Payload: map[string]any{
				"record": c.Key, "label": c.Label, "n": c.N, "prev": c.Prev,
			},
		})
		if rerr != nil {
			s.logger.Warn("console: now arcade record event", "record", c.Key, "error", rerr)
		}
	}

	// Moments: facts that deserve a burst, keyed to the local day so they stand
	// for the WHOLE day rather than blinking away on the next morph. The client
	// animates a moment only when it newly appears -- which is exactly the
	// render where the record actually broke or the plan actually shipped.
	for _, r := range arc.Records {
		if !r.Today {
			continue
		}
		arc.Moments = append(arc.Moments, nowMomentVM{
			ID:   "record-" + r.Key + "-" + dayKey,
			Copy: fmt.Sprintf("New record today: %s (%d)", r.Label, r.N),
		})
	}
	for _, p := range shippedToday {
		arc.Moments = append(arc.Moments, nowMomentVM{
			ID:   "plan-shipped-" + p.Project + "-" + p.Slug + "-" + dayKey,
			Copy: "plan:" + p.Slug + " shipped its last step today",
		})
	}

	tenAgo := now.Add(-10 * time.Minute)
	for _, t := range pulseTimes {
		if !t.Before(tenAgo) {
			arc.Hot.Count++
		}
	}
	arc.Hot.Hot = arc.Hot.Count >= nowHotStreakFloor
	return arc
}

// localMidnight floors now to the local day boundary the arcade judges by --
// the same convention as the momentum surfaces (store.localDay).
func localMidnight(now time.Time) time.Time {
	l := now.Local()
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location())
}
