package console

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/markdown"
	"github.com/arctop/seamless/internal/store"
)

// sessionSortKeys are the accepted ?sort values on the sessions list.
var sessionSortKeys = []string{"recent", "name"}

// sessionStatusNames widens core.SessionStatuses for an error message.
//
// Derived, not transcribed: the canonical set is the authority, and this exact
// set is where transcription already drifted. seam's --status help said
// "active|completed" while this handler has always accepted "expired" too, so a
// list copied from the help text would have silently rejected a status that
// works.
func sessionStatusNames() []string {
	out := make([]string, len(core.SessionStatuses))
	for i, st := range core.SessionStatuses {
		out[i] = string(st)
	}
	return out
}

// sessionRow is a display projection of a session for the list page. Live is true
// only for an active session whose last activity is within core.SessionIdleTTL;
// an active-but-stale (idle) row has Live false and is awaiting the reaper.
type sessionRow struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Project  string    `json:"project"`
	Status   string    `json:"status"`
	Source   string    `json:"source"`
	Ambient  bool      `json:"ambient"`
	Harness  string    `json:"harness,omitempty"` // client discriminator (claude-code|codex)
	Model    string    `json:"model,omitempty"`   // model powering the session, verbatim
	Host     string    `json:"host,omitempty"`    // machine the agent ran on ("" = before host scoping)
	Live     bool      `json:"live"`
	Findings string    `json:"findings"`
	Tokens   int       `json:"tokens"` // real model tokens harvested from the transcript (0 = none)
	Updated  time.Time `json:"updated"`
}

// sessionsData is the payload for the sessions list page. Active counts genuinely
// live sessions (active + heartbeated within the idle TTL); Idle counts active
// sessions gone quiet past it (the reaper will expire them); Completed and Expired
// count those terminal states. All are windowed; Total is the all-time count.
type sessionsData struct {
	Filter      string         `json:"filter"`
	Query       string         `json:"query,omitempty"`
	Sort        string         `json:"sort"`
	Active      int            `json:"active"`
	Idle        int            `json:"idle"`
	Completed   int            `json:"completed"`
	Expired     int            `json:"expired"`
	Total       int            `json:"total"`
	Window      string         `json:"window"`
	WindowLabel string         `json:"windowLabel"`
	Windows     []windowOption `json:"-"`
	Sessions    []sessionRow   `json:"sessions"`
	// Day and DayLabel carry the ?day drill-down: a single local day the list
	// is focused on (clicking a capture-calendar cell mints these URLs), empty
	// when the list answers about a window instead.
	Day      string `json:"day,omitempty"`
	DayLabel string `json:"-"`
	// Retained carries the ?retained drill-down: "yes" keeps only sessions
	// that left a durable artifact behind (GetSessionCoverage's covered-ness
	// test), "no" only those that retained nothing, "" applies no filter. The
	// Overview's Knowledge continuity vital mints ?retained=no links.
	Retained string `json:"retained,omitempty"`
	// Calendar and Streak are the momentum capture calendar (a year of daily
	// activity with covered-day marks) and its two quiet numbers. Both are nil
	// while the momentum feature is off -- zero trace, including in the JSON.
	Calendar template.HTML  `json:"-"`
	Streak   *captureStreak `json:"captureStreak,omitempty"`
}

// captureStreak carries the calendar's two quiet numbers: the current run of
// covered days and the longest ever (store.CaptureStreak).
type captureStreak struct {
	Current int `json:"current"`
	Longest int `json:"longest"`
}

// StreakEmberFloor is the current-streak length, in covered days, at which the
// calendar header lights its ember. A stated judgment threshold like the
// targets in atoms.go -- which is why it is a named const, not config.
const StreakEmberFloor = 7

// Ember reports whether the current streak has reached StreakEmberFloor. Pure
// presentation on the already-judged number: below the floor the template
// renders nothing, not a grey ember.
func (c captureStreak) Ember() bool { return c.Current >= StreakEmberFloor }

// EmberTitle is the ember's tooltip: the verbatim run beside the floor it
// cleared, so the glyph's claim is verifiable on the surface.
func (c captureStreak) EmberTitle() string {
	return fmt.Sprintf("%d covered days running -- the ember lights at %d", c.Current, StreakEmberFloor)
}

func (s *Service) sessionsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// An ABSENT status is a legitimate default (no filter, list everything). A
	// status that is present but uninterpretable is not: the `default: filter = ""`
	// this replaces silently listed EVERY session for `?status=bogus`, and the
	// caller cannot tell that from a filter that matched everything. Note the
	// inconsistency it leaves behind: a bad ?sort has always been a loud 400 twenty
	// lines down.
	filter := r.URL.Query().Get("status") // "", active, completed, expired
	var statusFilter core.SessionStatus
	if filter != "" {
		statusFilter = core.SessionStatus(filter)
		if !statusFilter.Valid() {
			s.badRequest(w, r, fmt.Sprintf("invalid status %q: valid values are %s",
				filter, strings.Join(sessionStatusNames(), ", ")))
			return
		}
	}
	sortKey := r.URL.Query().Get("sort")
	if sortKey == "" {
		sortKey = "recent"
	}
	if !slices.Contains(sessionSortKeys, sortKey) {
		s.badRequest(w, r, fmt.Sprintf("invalid sort %q: valid values are %s", sortKey, strings.Join(sessionSortKeys, ", ")))
		return
	}
	// The ?retained filter: the Overview's Knowledge continuity vital links
	// here with retained=no, so the card's click lands on the sessions that
	// dropped knowledge. Present-but-uninterpretable is a loud 400 like a bad
	// sort or day.
	retained := r.URL.Query().Get("retained") // "", yes, no
	if retained != "" && retained != "yes" && retained != "no" {
		s.badRequest(w, r, fmt.Sprintf("invalid retained %q: valid values are yes, no", retained))
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	q := strings.ToLower(query)
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), time.Now())

	// The ?day drill-down: clicking a capture-calendar cell focuses the list on
	// the sessions CREATED that local day -- created, not updated, because that
	// is what the calendar counted for the cell. A day present but
	// uninterpretable is a loud 400 like a bad sort, never a silent
	// list-everything. A focused day bypasses the ?w window (two time filters
	// would compose to confusion -- a day outside the window would show a lit
	// cell over an empty list; the template's window links drop ?day for the
	// same reason): the fetch bound becomes the day itself, sound because
	// updated_at never precedes created_at, and the list cap trades up so every
	// session the calendar counted for that day stays reachable.
	dayParam := r.URL.Query().Get("day")
	var dayStart, dayEnd time.Time
	var dayLabel string
	if dayParam != "" {
		var derr error
		dayStart, derr = time.ParseInLocation("2006-01-02", dayParam, time.Local)
		if derr != nil {
			s.badRequest(w, r, fmt.Sprintf("invalid day %q: expected YYYY-MM-DD", dayParam))
			return
		}
		dayEnd = dayStart.AddDate(0, 0, 1)
		dayLabel = dayStart.Format("Mon, Jan 02")
	}
	since, limit := win.Since, 200
	if !dayStart.IsZero() {
		since, limit = dayStart, 1000
	}

	sessions, err := store.ListSessions(ctx, s.cfg.DB, statusFilter, since, limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts, err := store.GetNavCounts(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The event half of the covered-ness test, loaded only when the filter is
	// on; the findings half lives on the rows already fetched.
	var artifacts map[string]struct{}
	if retained != "" {
		artifacts, err = store.SessionsWithArtifacts(ctx, s.cfg.DB)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	now := time.Now()
	rows := make([]sessionRow, 0, len(sessions))
	active, idle, completed, expired := 0, 0, 0, 0
	for _, sess := range sessions {
		if !dayStart.IsZero() {
			if c := sess.CreatedAt.Local(); c.Before(dayStart) || !c.Before(dayEnd) {
				continue
			}
		}
		plain := markdown.PlainText(sess.Findings)
		if !sessionMatches(sess.Name, sess.ProjectSlug, plain, sess.ID, q) {
			continue
		}
		// Covered-ness exactly as GetSessionCoverage computes it: non-empty
		// findings on the row, or a durable-artifact event in the log.
		if retained != "" {
			_, artifact := artifacts[sess.ID]
			if (sess.Findings != "" || artifact) != (retained == "yes") {
				continue
			}
		}
		live := sess.LiveAsOf(now, s.cfg.SessionIdleTTL)
		switch sess.Status {
		case core.SessionActive:
			if live {
				active++
			} else {
				idle++
			}
		case core.SessionExpired:
			expired++
		default:
			completed++
		}
		rows = append(rows, sessionRow{
			ID: sess.ID, Name: sess.Name, Project: sess.ProjectSlug,
			Status: string(sess.Status), Source: sess.Source, Ambient: sess.Ambient,
			Harness: harnessOf(sess), Model: sess.Model, Host: sess.Host,
			Live: live, Findings: snippet(plain, 120), Tokens: sess.Tokens.Total,
			Updated: sess.UpdatedAt,
		})
	}
	if sortKey == "name" {
		sort.SliceStable(rows, func(i, j int) bool {
			return sessionSortName(rows[i]) < sessionSortName(rows[j])
		})
	}
	calendar, streak := s.captureCalendar(ctx, now, dayParam)
	s.render(w, r, "sessions", pageData{
		Title:  "Sessions",
		Active: "sessions",
		Data: sessionsData{
			Filter: filter, Query: query, Sort: sortKey,
			Active: active, Idle: idle, Completed: completed, Expired: expired,
			Total:  counts.Sessions,
			Window: win.Key, WindowLabel: win.Label, Windows: windowOptions(win.Key),
			Day: dayParam, DayLabel: dayLabel, Retained: retained,
			Sessions: rows,
			Calendar: calendar, Streak: streak,
		},
	})
}

// captureCalendar assembles the momentum capture calendar for the Sessions
// page: a full year of daily session activity with covered-day marks, plus the
// streak numbers. Sessions is the screen about daily rhythm, so the full-year
// grid lives here rather than as a compact strip on Overview (the placement
// call the plan left to the implementer). selected is the active ?day filter,
// so the grid can outline the cell the list is currently answering about.
// Gated on the momentum feature -- off, neither query runs and the page is
// byte-identical to before -- and failure-soft: a store error costs the
// calendar, never the page.
func (s *Service) captureCalendar(ctx context.Context, now time.Time, selected string) (template.HTML, *captureStreak) {
	if !features.Enabled(s.effectiveFeatures(ctx), features.Momentum) {
		return "", nil
	}
	// A year back from today's local midnight, extended to the previous Sunday
	// so the grid's first column is a full week (AddDate day arithmetic, per
	// the localBucketAxis conventions -- never 24h multiples across DST).
	l := now.Local()
	start := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, l.Location()).AddDate(0, 0, -364)
	start = start.AddDate(0, 0, -int(start.Weekday()))
	buckets, err := store.SessionCoverageBuckets(ctx, s.cfg.DB,
		store.RetrievalWindow{Key: "calendar", Since: start}, now)
	if err != nil {
		s.logger.Warn("console: capture calendar buckets", "error", err)
		return "", nil
	}
	if len(buckets) == 0 {
		return "", nil // no sessions in the year: absence is the empty state
	}
	current, longest, err := store.CaptureStreakWith(ctx, s.cfg.DB, s.writeDB(), now)
	if err != nil {
		s.logger.Warn("console: capture streak", "error", err)
		return calendarGrid(buckets, start, selected), nil
	}
	return calendarGrid(buckets, start, selected), &captureStreak{Current: current, Longest: longest}
}

// sessionMatches reports whether a session row satisfies the ?q text filter
// (empty q matches all): a case-insensitive substring of name, project, findings,
// or id.
func sessionMatches(name, project, findings, id, q string) bool {
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), q) ||
		strings.Contains(strings.ToLower(project), q) ||
		strings.Contains(strings.ToLower(findings), q) ||
		strings.Contains(strings.ToLower(id), q)
}

// sessionSortName is the key for the "name" sort: the display name, or the id
// when a session is unnamed, lowercased.
func sessionSortName(row sessionRow) string {
	if row.Name != "" {
		return strings.ToLower(row.Name)
	}
	return strings.ToLower(row.ID)
}

// sessionDetail is the payload for a single session's page. Findings is the raw
// markdown (JSON output); FindingsHTML is the rendered, sanitized version the
// template shows.
type sessionDetail struct {
	Session      core.Session     `json:"session"`
	Harness      string           `json:"harness,omitempty"` // client discriminator (claude-code|codex)
	Live         bool             `json:"live"`              // active and heartbeated within the configured idle window
	Duration     string           `json:"duration"`          // compact wall-clock span for review surfaces
	Findings     string           `json:"findings"`
	FindingsHTML template.HTML    `json:"-"`
	Timeline     []eventRow       `json:"timeline"`
	Interactions []interactionRow `json:"interactions"` // the timeline as shared IX feed rows
	Strip        sessionTimeline  `json:"-"`            // proportional wall-clock strip over the session's interactions
	ToolCalls    int              `json:"toolCalls"`
	Reads        int              `json:"memoryReads"`
	Writes       int              `json:"memoryWrites"`
	Injected     int              `json:"injectedItems"`
	ReadBack     int              `json:"readAfterInject"`
	ByKind       []kindCount      `json:"eventsByKind"`
	ClaimedTasks []claimedTaskVM  `json:"claimedTasks"`
	Memories     []sessMemVM      `json:"memoriesWritten"`
	Trials       []sessTrialVM    `json:"trialsRecorded"`
	// ShowTrials gates the trials surfaces (the "Trials recorded" section and
	// the peek footer's trial count) on the research feature. When it is false
	// Trials is not even queried, so the section cannot half-render.
	ShowTrials bool `json:"-"`
}

// claimedTaskVM is a task the session currently holds (a live claim), shown on
// the session detail's right rail.
type claimedTaskVM struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	PlanSlug  string `json:"planSlug,omitempty"`
	LeaseLeft string `json:"leaseLeft,omitempty"`
}

// sessMemVM is a memory the session produced (its source_session is the session
// name), shown on the session detail's "Memories written" list.
type sessMemVM struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// sessTrialVM is a trial the session recorded, shown on the session detail's
// "Trials recorded" list.
type sessTrialVM struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Lab     string `json:"lab"`
	Outcome string `json:"outcome,omitempty"`
}

func (s *Service) sessionDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	sess, ok, err := store.SessionByID(ctx, s.cfg.DB, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "No session with id "+id+".")
		return
	}

	var timeline []eventRow
	var interactions []interactionRow
	byKind := map[string]int{}
	toolCalls, reads, writes := 0, 0, 0
	injected := map[string]struct{}{}
	readItems := map[string]struct{}{}

	if s.cfg.Events != nil {
		evs, eerr := s.cfg.Events.BySession(ctx, id, 500)
		if eerr != nil {
			s.serverError(w, r, eerr)
			return
		}
		for _, e := range evs {
			timeline = append(timeline, toEventRow(e))
			byKind[string(e.Kind)]++
			switch e.Kind {
			case core.EventToolCall:
				toolCalls++
			case core.EventMemoryRead:
				reads++
				if e.ItemID != "" {
					readItems[e.ItemID] = struct{}{}
				}
			case core.EventNoteRead:
				// Read-back evidence: recall surfaces notes as well as
				// memories, so a note opened after it was injected is context
				// genuinely used. It stays out of Reads, which the detail page
				// pairs with Writes under "Memory activity" -- the same reason
				// note.written is not counted there.
				if e.ItemID != "" {
					readItems[e.ItemID] = struct{}{}
				}
			case core.EventMemoryWritten:
				writes++
			case core.EventInjected:
				for _, itemID := range injectedEventItemIDs(e) {
					injected[itemID] = struct{}{}
				}
			}
		}
		// Interactions surface: the session's events projected into the shared IX
		// feed rows (newest first). Scoped to the feed's interaction kinds so the
		// rows, the wall-clock strip, and the kind filter all describe one set; the
		// right-rail cards cover the non-interaction detail (reads/writes, produced
		// memories, claimed tasks). BySession returns oldest-first, so we walk it in
		// reverse to build newest-first.
		namer := func(string) core.Session { return sess }
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			if !isInteraction(e) || skipInteraction(e) {
				continue
			}
			interactions = append(interactions, toInteractionRow(e, namer))
		}
	}
	readBack := 0
	for itemID := range injected {
		if _, ok := readItems[itemID]; ok {
			readBack++
		}
	}

	now := time.Now().UTC()
	data := sessionDetail{
		Session: sess, Harness: harnessOf(sess), Live: sess.LiveAsOf(now, s.cfg.SessionIdleTTL),
		Duration: sessionDuration(sess, now), Findings: sess.Findings, Timeline: timeline,
		Interactions: interactions,
		Strip:        buildSessionTimeline(interactions),
		ToolCalls:    toolCalls, Reads: reads, Writes: writes,
		Injected: len(injected), ReadBack: readBack, ByKind: sortedKinds(byKind),
	}
	data.FindingsHTML = s.renderBody(ctx, sess.Findings, sess.ProjectSlug)

	// Claimed tasks (live claims this session holds) + memories it produced --
	// the T2b relation joins. Best-effort: a legacy non-ULID session id or empty
	// name (the joins guard against mis-keyed calls) leaves the panel empty
	// rather than failing the whole detail page.
	if held, herr := store.TasksClaimedBy(ctx, s.cfg.DB, sess.ID); herr == nil {
		for _, t := range held {
			if !t.ClaimLive(now) {
				continue
			}
			vm := claimedTaskVM{ID: t.ID, Title: t.Title, PlanSlug: t.PlanSlug}
			if t.LeaseExpiresAt != nil {
				vm.LeaseLeft = durUntil(*t.LeaseExpiresAt, now)
			}
			data.ClaimedTasks = append(data.ClaimedTasks, vm)
		}
	} else {
		s.logger.Warn("console: session claimed tasks", "session", sess.ID, "error", herr)
	}
	if mems, merr := store.MemoriesForSession(ctx, s.cfg.DB, sess); merr == nil {
		for _, m := range mems {
			data.Memories = append(data.Memories, sessMemVM{ID: m.ID, Name: m.Name, Kind: string(m.Kind)})
		}
	} else {
		s.logger.Warn("console: session memories written", "session", sess.ID, "error", merr)
	}
	// Trials recorded: a research surface. While that feature is off the query
	// is skipped entirely -- the rows are still there, the console just does not
	// offer a way into a gated screen.
	data.ShowTrials = features.Enabled(s.effectiveFeatures(ctx), features.Research)
	if data.ShowTrials {
		if trials, terr := store.QueryTrials(ctx, s.cfg.DB, store.TrialFilter{SessionID: sess.ID, Limit: 50}); terr == nil {
			for _, tr := range trials {
				data.Trials = append(data.Trials, sessTrialVM{
					ID: tr.ID, Title: tr.Title, Lab: tr.Lab, Outcome: string(tr.Outcome),
				})
			}
		} else {
			s.logger.Warn("console: session trials recorded", "session", sess.ID, "error", terr)
		}
	}

	s.renderDetail(w, r, "session", pageData{
		Title:  "Session " + shortID(sess.ID),
		Active: "sessions",
		Data:   data,
	})
}

// sessionDuration returns the compact wall-clock span shown on session review
// surfaces. Active sessions run through now; terminal sessions stop at their
// final update. Seconds are intentionally suppressed because heartbeat timing
// is operational noise at this level.
func sessionDuration(sess core.Session, now time.Time) string {
	end := sess.UpdatedAt
	if sess.Status == core.SessionActive {
		end = now
	}
	if sess.CreatedAt.IsZero() || end.IsZero() || end.Before(sess.CreatedAt) {
		return ""
	}
	d := end.Sub(sess.CreatedAt)
	if d < time.Minute {
		return "<1m"
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	minutes := int(d/time.Minute) % 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// injectedEventItemIDs pulls the item ids an injection event surfaced (recall
// records them in the payload's item_ids array; single-item events use ItemID).
func injectedEventItemIDs(e core.Event) []string {
	var ids []string
	if e.ItemID != "" {
		ids = append(ids, e.ItemID)
	}
	// Two shapes reach here: an event read back from the log carries the
	// JSON-decoded []any, while one delivered live by the recorder still holds
	// the []string its producer built. Reading only the first made every live
	// injection look empty (no item count on the feed, nothing for the sky).
	switch raw := e.Payload["item_ids"].(type) {
	case []string:
		for _, str := range raw {
			if str != "" {
				ids = append(ids, str)
			}
		}
	case []any:
		for _, v := range raw {
			if str, ok := v.(string); ok && str != "" {
				ids = append(ids, str)
			}
		}
	}
	return ids
}

// snippet trims s to a single line no longer than n runes, appending an ellipsis
// when it was cut.
func snippet(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// sortedKinds returns event-kind counts as ordered pairs (descending count).
func sortedKinds(byKind map[string]int) []kindCount {
	out := make([]kindCount, 0, len(byKind))
	for k, n := range byKind {
		out = append(out, kindCount{Kind: k, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
