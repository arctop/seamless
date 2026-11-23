package console

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/diary"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/markdown"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
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

// sessionRow is a display projection of a session for the list page. Live is
// core.Session.LiveAsOf: true for an active session whose last activity is
// within the idle TTL, or that holds a live claim; an active row that is
// neither (idle) has Live false and is awaiting the reaper.
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
	Account  string    `json:"account,omitempty"` // restricted account that started it ("" = owner)
	Live     bool      `json:"live"`
	Findings string    `json:"findings"`
	Tokens   int       `json:"tokens"` // real model tokens harvested from the transcript (0 = none)
	Updated  time.Time `json:"updated"`
	// The flag (session_update / session_end verdict): Verdict and its
	// Reason, "" when not flagged; Tone is the badge's class. Kept is whether
	// its transcript copy is on this disk, Copy the copy's record.
	Verdict string `json:"verdict,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Tone    string `json:"-"`
	Copy    string `json:"transcriptCopy,omitempty"`
	Kept    bool   `json:"transcriptKept,omitempty"`
	// TranscriptPath is the file the agent client wrote, on its computer
	// (shown for flagged sessions only).
	TranscriptPath string `json:"transcriptPath,omitempty"`
	// Headline is what the row is titled by, what the session is doing
	// (sessionHeadlines): the task it holds, else its first prompt; "" when it
	// has neither, and the row keeps its name as the title. HeadlineFrom says
	// which (headlineTask, headlinePrompt). With a headline the name is the
	// row's second line.
	Headline     string `json:"headline,omitempty"`
	HeadlineFrom string `json:"headlineFrom,omitempty"`
}

// Heading is the row's title: its headline, else its name, else its short id.
func (r sessionRow) Heading() string {
	if r.Headline != "" {
		return r.Headline
	}
	return r.NameOrID()
}

// NameOrID is the session's name, or its short id when it has none.
func (r sessionRow) NameOrID() string {
	if r.Name != "" {
		return r.Name
	}
	return shortID(r.ID)
}

// sessionsData is the payload for the sessions list page. Active counts genuinely
// live sessions (core.Session.LiveAsOf: heartbeated within the idle TTL, or
// holding a live claim); Idle counts active sessions gone quiet past it with no
// live claim (the reaper will expire them); Completed and Expired count those
// terminal states. All are windowed; Total is the all-time count.
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
	// WindowParam is ?w= as the request named it, "" when it named none: the
	// links the page draws carry the window only once it was chosen, so a
	// verdict filter picked from the default view still spans all time.
	WindowParam string       `json:"-"`
	Sessions    []sessionRow `json:"sessions"`
	// Day and DayLabel carry the ?day drill-down: a single local day the list
	// is focused on (clicking a capture-calendar cell mints these URLs), empty
	// when the list answers about a window instead.
	Day      string `json:"day,omitempty"`
	DayLabel string `json:"-"`
	// DiaryHref is the focused day's page in the Diary, set only while the
	// diary feature is on and a day is focused (HTML only): off, the page
	// carries no trace of the Diary.
	DiaryHref string `json:"-"`
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
	// Locked names the locked projects this request does
	// not read, whose sessions the list leaves out: names only, never a count.
	// nil while nothing is locked -- absent from the JSON.
	Locked []string `json:"locked,omitempty"`
	// Verdict carries the ?verdict= filter (flagged, or one verdict); "" for
	// none. Flagged counts every flagged session this request reads, and the
	// page offers the filter only once there is one (or it is in use).
	Verdict string `json:"verdict,omitempty"`
	Flagged int    `json:"flagged,omitempty"`
	// Project carries the ?project= filter as the request named it: a
	// project's slug, or "global" for the global scope; "" for none.
	Project string `json:"project,omitempty"`
	// Matched counts every session the page's filters keep, its cap aside;
	// Sessions holds the newest Cap of them. Capped is Matched > Cap, and the
	// page then says "newest Cap of Matched".
	Matched int  `json:"matched"`
	Cap     int  `json:"cap"`
	Capped  bool `json:"capped,omitempty"`
}

// ShowFlagFilter reports whether the page offers the flag filter.
func (d sessionsData) ShowFlagFilter() bool { return d.Flagged > 0 || d.Verdict != "" }

// sessionsListCap is how many sessions the list shows, the newest first; a
// focused day shows up to sessionsDayCap, so every session the capture
// calendar counted for the day stays reachable.
const (
	sessionsListCap = 200
	sessionsDayCap  = 1000
)

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
	return fmt.Sprintf("%d covered days running; the ember lights at %d", c.Current, StreakEmberFloor)
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
	// The ?verdict= filter: flagged sessions, or one verdict's. Loud on a
	// value it cannot read, like every other filter here.
	verdict := r.URL.Query().Get("verdict")
	listFilter := store.SessionFilter{Status: statusFilter}
	if !verdictFilter(verdict, &listFilter) {
		s.badRequest(w, r, fmt.Sprintf("invalid verdict %q: valid values are %s", verdict, strings.Join(verdictFilterValues(), ", ")))
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	// The ?project= filter (one project's sessions, "global" for the global
	// scope's): the project page's Sessions tab links here. Strict like every
	// other filter: a value that names no project this request reads -- one
	// that does not exist, or a locked one, whose sessions stay absent -- is a
	// loud 400, never an empty list that reads as "no sessions".
	project, byProject, refusal, err := s.sessionsProject(ctx, r.URL.Query().Get("project"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if refusal != "" {
		s.badRequest(w, r, refusal)
		return
	}
	if byProject {
		listFilter.ByProject, listFilter.Project = true, project
	}
	projectParam := ""
	if byProject {
		projectParam = cmp.Or(project, validate.GlobalProject)
	}
	// The ?w window. Absent, it is the page's default -- the last 24h, or all
	// time once a verdict or a project filter is set: a flag is metadata that
	// moves no updated_at and a flagged session is worth finding however old
	// (seam sessions --flagged lists all time too), and a project's sessions
	// are what its page links here for. A window the owner chose applies to
	// those filters as to every other. Present but none of the windows is a
	// loud 400, never a quiet default: it would read as the window chosen.
	chosen := "" // the window the request named, "" for none
	if ws, ok := r.URL.Query()["w"]; ok {
		chosen = ws[0]
		if !slices.Contains(store.RetrievalWindowKeys, chosen) {
			s.badRequest(w, r, fmt.Sprintf("invalid w %q: valid values are %s", chosen, strings.Join(store.RetrievalWindowKeys, ", ")))
			return
		}
	}
	windowKey := chosen
	if windowKey == "" && (verdict != "" || byProject) {
		windowKey = "all"
	}
	win := store.ResolveRetrievalWindow(windowKey, time.Now())

	// The ?day drill-down: clicking a capture-calendar cell focuses the list on
	// the sessions CREATED that local day -- created, not updated, because that
	// is what the calendar counted for the cell. A day present but
	// uninterpretable is a loud 400 like a bad sort, never a silent
	// list-everything. A focused day bypasses the ?w window (two time filters
	// would compose to confusion -- a day outside the window would show a lit
	// cell over an empty list; the template's window links drop ?day for the
	// same reason), and the list cap trades up so every session the calendar
	// counted for that day stays reachable.
	dayParam := r.URL.Query().Get("day")
	var dayStart time.Time
	var dayLabel string
	if dayParam != "" {
		var derr error
		dayStart, derr = time.ParseInLocation("2006-01-02", dayParam, time.Local)
		if derr != nil {
			s.badRequest(w, r, fmt.Sprintf("invalid day %q: expected YYYY-MM-DD", dayParam))
			return
		}
		dayLabel = dayStart.Format("Mon, Jan 02")
	}
	listFilter.Since, listFilter.Limit = win.Since, sessionsListCap
	if !dayStart.IsZero() {
		listFilter.Since, listFilter.Limit = time.Time{}, sessionsDayCap
		listFilter.CreatedFrom, listFilter.CreatedTo = dayStart, dayStart.AddDate(0, 0, 1)
	}
	// Every filter is the query's, the search and the retained test included,
	// so the cap cuts the matches and not the window: a session past the
	// newest 200 is still found by its name, and the count says how many the
	// cap left out.
	listFilter.Retained, listFilter.Query = store.SessionRetained(retained), query

	sessions, err := store.ListSessionsWhere(ctx, s.db(ctx), listFilter)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	matched := len(sessions)
	if matched >= listFilter.Limit {
		if matched, err = store.CountSessionsWhere(ctx, s.db(ctx), listFilter); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	flagged, err := store.CountFlaggedSessions(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts, err := store.GetNavCounts(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	now := time.Now()
	heads := s.sessionHeadlines(ctx, sessions, now)
	rows := make([]sessionRow, 0, len(sessions))
	active, idle, completed, expired := 0, 0, 0, 0
	for _, sess := range sessions {
		plain := markdown.PlainText(sess.Findings)
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
		row := sessionRow{
			ID: sess.ID, Name: sess.Name, Project: sess.ProjectSlug,
			Status: string(sess.Status), Source: sess.Source, Ambient: sess.Ambient,
			Harness: harnessOf(sess), Model: sess.Model, Host: sess.Host, Account: sess.Account,
			Live: live, Findings: snippet(plain, 120), Tokens: sess.Tokens.Total,
			Updated: sess.UpdatedAt,
		}
		if h, ok := heads[sess.ID]; ok {
			row.Headline, row.HeadlineFrom = h.Text, h.From
		}
		if sess.Flagged() {
			row.Verdict, row.Reason, row.Tone = string(sess.Verdict), sess.VerdictReason, verdictTone(sess.Verdict)
			row.Copy, row.TranscriptPath = string(sess.Transcript), sess.TranscriptPath
			row.Kept = s.cfg.Transcripts != nil && s.cfg.Transcripts.Kept(sess.ID)
		}
		rows = append(rows, row)
	}
	if sortKey == "name" {
		sort.SliceStable(rows, func(i, j int) bool {
			return sessionSortName(rows[i]) < sessionSortName(rows[j])
		})
	}
	calendar, streak := s.captureCalendar(ctx, now, dayParam)
	// A focused day links to its page in the Diary, while that feature is
	// on. The link is all it adds: a calendar click still focuses this list.
	diaryHref := ""
	if dayParam != "" && features.Enabled(s.effectiveFeatures(ctx), features.Diary) {
		diaryHref = diaryPath + "?day=" + dayStart.Format("2006-01-02")
	}
	s.render(w, r, "sessions", pageData{
		Title:  "Sessions",
		Active: "sessions",
		Data: sessionsData{
			Filter: filter, Query: query, Sort: sortKey,
			Active: active, Idle: idle, Completed: completed, Expired: expired,
			Total:  counts.Sessions,
			Window: win.Key, WindowLabel: win.Label, Windows: windowOptions(win.Key),
			WindowParam: chosen,
			Day:         dayParam, DayLabel: dayLabel, Retained: retained, DiaryHref: diaryHref,
			Sessions: rows,
			Calendar: calendar, Streak: streak,
			Locked:  lockedProjects(ctx),
			Verdict: verdict, Flagged: flagged,
			Project: projectParam,
			Matched: matched, Cap: listFilter.Limit, Capped: matched > len(rows),
		},
	})
}

// sessionsProject reads the Sessions list's ?project= value: ("", false) for
// none, the slug and true for one ("" and true for the global scope, named
// "global"), or the refusal the page answers with. The project must be one
// this request reads -- a projects row, or sessions, through the request's
// handle -- so an unknown slug and a locked project's are both refused, the
// locked one naming its lock (the page names the locked projects already).
func (s *Service) sessionsProject(ctx context.Context, raw string) (slug string, ok bool, refusal string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, "", nil
	}
	slug, verr := validate.Project(raw)
	if verr != nil {
		return "", false, fmt.Sprintf("invalid project %q: name a project's slug, or global for the global scope", raw), nil
	}
	if slug == "" {
		return "", true, "", nil
	}
	db := s.db(ctx)
	if _, found, err := store.ProjectBySlug(ctx, db, slug); err != nil || found {
		return slug, found, "", err
	}
	some, err := store.ListSessionsWhere(ctx, db, store.SessionFilter{ByProject: true, Project: slug, Limit: 1})
	if err != nil || len(some) > 0 {
		return slug, len(some) > 0, "", err
	}
	if lockedHere(ctx, slug) {
		return "", false, fmt.Sprintf("invalid project %q: it is locked, and its sessions stay out of this view until this browser unlocks it", raw), nil
	}
	return "", false, fmt.Sprintf("invalid project %q: no project has that slug", raw), nil
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
	buckets, err := store.SessionCoverageBuckets(ctx, s.db(ctx),
		store.RetrievalWindow{Key: "calendar", Since: start}, now)
	if err != nil {
		s.logger.Warn("console: capture calendar buckets", "error", err)
		return "", nil
	}
	if len(buckets) == 0 {
		return "", nil // no sessions in the year: absence is the empty state
	}
	current, longest, err := store.CaptureStreakWith(ctx, s.db(ctx), s.writeDB(), now)
	if err != nil {
		s.logger.Warn("console: capture streak", "error", err)
		return calendarGrid(buckets, start, selected), nil
	}
	return calendarGrid(buckets, start, selected), &captureStreak{Current: current, Longest: longest}
}

// The sources of a session row's headline (sessionHeadline.From).
const (
	headlineTask   = "task"   // the task it holds with a live claim
	headlinePrompt = "prompt" // its first prompt, as the Diary's about line reads one
)

// sessionHeadline is what a session row is titled by: what the session is
// doing, in words, rather than its generated name.
type sessionHeadline struct {
	// Text is the title; "" when the session holds no task and has no prompt
	// to show, and its row keeps its name.
	Text string
	// From is where Text came from: headlineTask or headlinePrompt.
	From string
	// Held is the tasks the session holds with a live claim, oldest-created
	// first (ClaimsHeldBy's order), for a list that also marks its claims.
	Held []core.Task
}

// sessionHeadlines titles a list of sessions by what each is doing, in two
// reads for the whole list through the request's handle, so a locked
// project's tasks and prompts are absent from both:
//
//   - an active session holding a task with a live claim is titled by the
//     task (store.ClaimsHeldBy), and by how many more it holds;
//   - any other session whose findings are not its agent's own is titled by
//     its first prompt (store.SessionFirstPrompts: DiaryFirstPrompts' events,
//     over the session's life), by the rules the Diary's about line follows:
//     findings the agent wrote come first, and a prompt shows as its first
//     line with words, markdown stripped, cut on a word (diary.ExplicitFindings,
//     diary.FirstLine);
//   - a session whose agent wrote findings is not titled: its row shows them.
//
// A session absent from the map has no headline. A read that fails costs the
// headlines, never the page: the rows keep their names.
func (s *Service) sessionHeadlines(ctx context.Context, sessions []core.Session, now time.Time) map[string]sessionHeadline {
	out := make(map[string]sessionHeadline, len(sessions))
	db := s.db(ctx)
	var active []string
	for _, sess := range sessions {
		if sess.Status == core.SessionActive {
			active = append(active, sess.ID)
		}
	}
	held := map[string][]core.Task{}
	if len(active) > 0 {
		h, err := store.ClaimsHeldBy(ctx, db, active)
		if err != nil {
			s.logger.Warn("console: session headlines: claims", "error", err)
		} else {
			held = h
		}
	}
	var asking []string
	for _, sess := range sessions {
		var live []core.Task
		if sess.Status == core.SessionActive {
			for _, t := range held[sess.ID] {
				if t.ClaimLive(now) {
					live = append(live, t)
				}
			}
		}
		if len(live) > 0 {
			text := core.TruncateWords(shownTitle(live[0].Title), diary.AboutMaxRunes)
			if len(live) > 1 {
				text += ", and " + plural(len(live)-1, "more task", "more tasks")
			}
			out[sess.ID] = sessionHeadline{Text: text, From: headlineTask, Held: live}
			continue
		}
		if !diary.ExplicitFindings(sess.Findings) {
			asking = append(asking, sess.ID)
		}
	}
	if len(asking) == 0 {
		return out
	}
	// A session's first prompt of all its life, not of a day.
	prompts, err := store.SessionFirstPrompts(ctx, db, asking)
	if err != nil {
		s.logger.Warn("console: session headlines: first prompts", "error", err)
		return out
	}
	for _, id := range asking {
		if line := diary.FirstLine(prompts[id]); line != "" {
			out[id] = sessionHeadline{Text: line, From: headlinePrompt}
		}
	}
	return out
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
	Live         bool             `json:"live"`              // core.Session.LiveAsOf: heartbeated within the configured idle window, or holding a live claim
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
	TasksMoved   []sessTaskMoveVM `json:"tasksMoved"`
	Notes        []sessNoteVM     `json:"notesWritten"`
	Memories     []sessMemVM      `json:"memoriesWritten"`
	Trials       []sessTrialVM    `json:"trialsRecorded"`
	// Events counts every event the session recorded; Timeline holds at most
	// sessionTimelineCap of them: the first, or with ?timeline=latest
	// (TimelineLatest) the latest. TimelineCapped says it left some out.
	Events         int  `json:"events"`
	TimelineLatest bool `json:"timelineLatest,omitempty"`
	// ShowTrials gates the trials surfaces (the "Trials recorded" section and
	// the peek footer's trial count) on the research feature. When it is false
	// Trials is not even queried, so the section cannot half-render.
	ShowTrials bool `json:"-"`
	// Conversation is the session's conversation, one card on every page of
	// it (conversation.go): its home chain, every project it reached with the
	// approval and who decided it, and what it wrote in each. nil -- absent
	// from the JSON, no card -- for a conversation that never moved, asked for
	// nothing and wrote nothing stamped.
	Conversation *conversationView `json:"conversation,omitempty"`
	// Flag is the session's flag and its transcript copy; nil -- absent from
	// the JSON, no card -- for a session that is not flagged.
	Flag *sessionFlagView `json:"flag,omitempty"`
}

// TimelineCapped reports whether the timeline holds fewer events than the
// session recorded.
func (d sessionDetail) TimelineCapped() bool { return d.Events > len(d.Timeline) }

// sessionTimelineCap is how many events a session's timeline holds.
const sessionTimelineCap = 500

// sessTaskMoveVM is a task the session moved -- created, claimed, released,
// closed or reopened -- from its own task.transition events, shown on the
// session detail's "Tasks moved" list.
type sessTaskMoveVM struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	PlanSlug string `json:"planSlug,omitempty"`
	// Moves are the verbs (transitionVerb's) in the order they happened, a
	// verb repeated in a row folded into one.
	Moves []string  `json:"moves"`
	Last  time.Time `json:"last"`
}

// MovesText is the moves as the list shows them.
func (m sessTaskMoveVM) MovesText() string { return strings.Join(m.Moves, " · ") }

// sessNoteVM is a note the session wrote, from its own write events (the
// Conversation card's kinds, convWrittenKind), shown on the session detail's
// "Notes written" list. What says how: a note, a plan it captured, or a
// subagent's report.
type sessNoteVM struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	What  string `json:"what"`
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
	sess, ok, err := store.SessionByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		if s.answerLockedSession(w, r, id) {
			return
		}
		s.notFound(w, r, "No session with id "+id+".")
		return
	}

	// ?timeline=latest reads the session's latest events where the default
	// reads its first: a long session's timeline holds sessionTimelineCap of
	// them, and the page links between the two. Strict like every parameter.
	latest := false
	switch tl := r.URL.Query().Get("timeline"); tl {
	case "":
	case "latest":
		latest = true
	default:
		s.badRequest(w, r, fmt.Sprintf("invalid timeline %q: valid values are latest (or omit)", tl))
		return
	}

	var timeline []eventRow
	var interactions []interactionRow
	byKind := map[string]int{}
	toolCalls, reads, writes := 0, 0, 0
	injected := map[string]struct{}{}
	readItems := map[string]struct{}{}
	var evs []core.Event
	total := 0

	if s.cfg.Events != nil {
		elog := s.eventLog(ctx)
		var eerr error
		if latest {
			evs, eerr = elog.LatestBySession(ctx, id, nil, sessionTimelineCap)
			slices.Reverse(evs) // newest first; the timeline reads oldest first
		} else {
			evs, eerr = elog.BySession(ctx, id, sessionTimelineCap)
		}
		if eerr != nil {
			s.serverError(w, r, eerr)
			return
		}
		total = len(evs)
		if total >= sessionTimelineCap {
			if total, eerr = elog.CountBySession(ctx, id); eerr != nil {
				s.serverError(w, r, eerr)
				return
			}
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
	// Its findings' provenance stamps (item_provenance only; console stamp.go).
	sess.MayContain = s.itemStamps(ctx, core.ItemKindSession, sess.ID, sess.ProjectSlug)
	data := sessionDetail{
		Session: sess, Harness: harnessOf(sess), Live: sess.LiveAsOf(now, s.cfg.SessionIdleTTL),
		Duration: sessionDuration(sess, now), Findings: sess.Findings, Timeline: timeline,
		Interactions: interactions,
		Strip:        buildSessionTimeline(interactions),
		ToolCalls:    toolCalls, Reads: reads, Writes: writes,
		Injected: len(injected), ReadBack: readBack, ByKind: sortedKinds(byKind),
		Events: total, TimelineLatest: latest,
	}
	data.FindingsHTML = s.renderBody(ctx, sess.Findings, sess.ProjectSlug)
	data.Flag = s.flagView(sess)

	// Claimed tasks (live claims this session holds) + memories it produced --
	// the T2b relation joins. Best-effort: a legacy non-ULID session id or empty
	// name (the joins guard against mis-keyed calls) leaves the panel empty
	// rather than failing the whole detail page.
	if held, herr := store.TasksClaimedBy(ctx, s.db(ctx), sess.ID); herr == nil {
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
	// What it moved and wrote, from its own events: the timeline's when it
	// holds every one, else a read of just those kinds.
	data.TasksMoved, data.Notes = s.sessionWork(ctx, sess.ID, evs, total > len(evs))
	if mems, merr := store.MemoriesForSession(ctx, s.db(ctx), sess); merr == nil {
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
		if trials, terr := store.QueryTrials(ctx, s.db(ctx), store.TrialFilter{SessionID: sess.ID, Limit: 50}); terr == nil {
			for _, tr := range trials {
				data.Trials = append(data.Trials, sessTrialVM{
					ID: tr.ID, Title: tr.Title, Lab: tr.Lab, Outcome: string(tr.Outcome),
				})
			}
		} else {
			s.logger.Warn("console: session trials recorded", "session", sess.ID, "error", terr)
		}
	}
	// The conversation's card (the page and the JSON; the peek fragment is a
	// compact summary that shows none). A record that cannot be read costs the
	// card its content, never the page, and the card says so rather than
	// showing part of the record.
	if r.URL.Query().Get("peek") != "1" {
		if conv, cerr := s.conversationFor(ctx, sess, data.ShowTrials); cerr != nil {
			s.logger.Warn("console: session conversation", "session", sess.ID, "error", cerr)
			data.Conversation = &conversationView{Err: "This conversation's record could not be read; the daemon's log has the error."}
		} else {
			data.Conversation = conv
		}
	}

	s.renderDetail(w, r, "session", pageData{
		Title:  sessionPageTitle(sess),
		Active: "sessions",
		Data:   data,
	})
}

// sessionPageTitle is a session page's title, the browser tab's and the
// palette's Recent row's: the session's name (its short id when it has none)
// and its project.
func sessionPageTitle(sess core.Session) string {
	name := sess.Name
	if name == "" {
		name = shortID(sess.ID)
	}
	return "Session " + name + " · " + cmp.Or(sess.ProjectSlug, validate.GlobalProject)
}

// sessionWorkKinds are the events that say what a session moved and wrote
// beyond its memories (MemoriesForSession reads those by their stamp): the
// task transitions it made and the notes it wrote, the Conversation card's
// kinds (convWrittenKind).
var sessionWorkKinds = []core.EventKind{
	core.EventTaskTransition, core.EventNoteWritten, core.EventPlanCaptured, core.EventSubagentCaptured,
}

// sessionWork lists the tasks a session moved and the notes it wrote, from
// its own events: evs, the timeline's, when they are all of them, else a
// read of just these kinds (partial). Only the items that resolve through
// the request's handle are listed, each linking to its page; one deleted
// since, or in a project this request does not read, is left out, never a
// link that answers 404. A read that fails costs the lists, never the page.
func (s *Service) sessionWork(ctx context.Context, sessionID string, evs []core.Event, partial bool) ([]sessTaskMoveVM, []sessNoteVM) {
	if partial {
		elog := s.eventLog(ctx)
		if elog == nil {
			return nil, nil
		}
		latest, err := elog.LatestBySession(ctx, sessionID, sessionWorkKinds, sessionTimelineCap)
		if err != nil {
			s.logger.Warn("console: session work", "session", sessionID, "error", err)
			return nil, nil
		}
		slices.Reverse(latest)
		evs = latest
	}
	type move struct {
		verbs []string
		last  time.Time
	}
	moves := map[string]*move{}
	var taskIDs []string
	noteWhat := map[string]string{}
	var noteIDs []string
	for _, e := range evs {
		if e.ItemID == "" || e.SessionID != sessionID {
			continue
		}
		switch e.Kind {
		case core.EventTaskTransition:
			verb, _, _ := transitionVerb(e.Payload)
			if !sessionTaskMoved(verb) {
				continue
			}
			m := moves[e.ItemID]
			if m == nil {
				m = &move{}
				moves[e.ItemID] = m
				taskIDs = append(taskIDs, e.ItemID)
			}
			if n := len(m.verbs); n == 0 || m.verbs[n-1] != verb {
				m.verbs = append(m.verbs, verb)
			}
			m.last = e.TS
		case core.EventNoteWritten, core.EventPlanCaptured, core.EventSubagentCaptured:
			if _, seen := noteWhat[e.ItemID]; !seen {
				noteIDs = append(noteIDs, e.ItemID)
			}
			// The latest says what the note is now: a capture approved
			// into a plan, say.
			noteWhat[e.ItemID] = sessionNoteWhat(e.Kind)
		}
	}
	db := s.db(ctx)
	var tasks []sessTaskMoveVM
	if len(taskIDs) > 0 {
		got, err := store.TasksByIDs(ctx, db, taskIDs)
		if err != nil {
			s.logger.Warn("console: session tasks moved", "session", sessionID, "error", err)
		}
		for _, id := range taskIDs {
			t, ok := got[id]
			if !ok {
				continue
			}
			m := moves[id]
			tasks = append(tasks, sessTaskMoveVM{ID: id, Title: t.Title, PlanSlug: t.PlanSlug, Moves: m.verbs, Last: m.last})
		}
		// Newest move first, as the other lists of the card read.
		slices.SortStableFunc(tasks, func(a, b sessTaskMoveVM) int { return b.Last.Compare(a.Last) })
	}
	var notes []sessNoteVM
	if len(noteIDs) > 0 {
		got, err := store.NotesByIDs(ctx, db, noteIDs)
		if err != nil {
			s.logger.Warn("console: session notes written", "session", sessionID, "error", err)
		}
		for i := len(noteIDs) - 1; i >= 0; i-- { // newest first
			if n, ok := got[noteIDs[i]]; ok {
				notes = append(notes, sessNoteVM{ID: n.ID, Title: n.Title, What: noteWhat[n.ID]})
			}
		}
	}
	return tasks, notes
}

// sessionTaskMoved reports whether a transition verb moved its task: its
// status or its holder. An edit, a commit or a blocker removed moved
// nothing, and a session that only did those moved no task.
func sessionTaskMoved(verb string) bool {
	switch verb {
	case "updated", "blocker removed", "blockers removed":
		return false
	}
	return true
}

// sessionNoteWhat is how the "Notes written" list says a note came to be.
func sessionNoteWhat(kind core.EventKind) string {
	switch kind {
	case core.EventPlanCaptured:
		return "plan"
	case core.EventSubagentCaptured:
		return "subagent report"
	}
	return "note"
}

// answerLockedSession is answerLockedPlan for a session: a lookup of a
// session in a locked project this request does not read names the lock (the
// JSON refusal, or the locked page). It reports whether it answered (a failed
// lookup as the error it is); nothing runs while nothing is locked.
func (s *Service) answerLockedSession(w http.ResponseWriter, r *http.Request, id string) bool {
	ctx := r.Context()
	if len(lockedProjects(ctx)) == 0 {
		return false
	}
	sess, ok, err := store.SessionByID(ctx, s.writeDB(), id)
	if err != nil {
		s.serverError(w, r, err)
		return true
	}
	if !ok || !lockedHere(ctx, sess.ProjectSlug) {
		return false
	}
	s.answerLocked(w, r, sess.ProjectSlug, lockedItemSession)
	return true
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
