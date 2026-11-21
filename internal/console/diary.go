package console

// The Diary (plan:diary): one local day of work as a journal entry -- what was
// worked on, which agents did it, and what it took in agent time and model
// tokens -- beside a rail of its month's days. The day model is
// internal/diary's and knows no URLs; this file is the console's half:
//
//   - The links. Every href is built from an id the day model resolved through
//     the request's handle (linkEntry), the same href for the page and for
//     ?format=json. An item that no longer resolves is text marked "no longer
//     exists", never a link that answers 404.
//   - The optional features. With research off a trial appears nowhere, and
//     the counts that included one say so no longer (withoutTrials).
//   - The layout: the date block, the stat tiles, the day's rhythm, the
//     spotlight cards beside the usage panel, and the rail.
//
// One route, gated on the diary feature: GET /console/diary is today,
// ?day=YYYY-MM-DD another day, &m=YYYY-MM the rail's month (the day's own by
// default), ?format=json the day model plus hrefs, identical at every level.

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/diary"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// diaryPath is the Diary's route.
const diaryPath = "/console/diary"

// diaryLiveGapMS is the least time between two refreshes of today's entry,
// in milliseconds: a busy fleet sends many events a second, and one read of a
// day, its month and the usual day costs some tens of milliseconds through a
// filtered handle. The page script reads it from the entry.
const diaryLiveGapMS = 15000

// diaryData is the page's payload. The JSON answer is the day model's Entry,
// with every Ref's href filled in, plus the rail's month and the usual day the
// tiles compare against; Future marks a day that has not begun (an empty
// entry). View is the page's layout, HTML only.
type diaryData struct {
	diary.Entry
	Future bool            `json:"future,omitempty"`
	Month  diary.MonthView `json:"month"`
	Usual  diary.UsualDay  `json:"usual"`
	View   *diaryView      `json:"-"`
}

func (s *Service) diaryPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	now := time.Now()
	today := dyMidnight(now)
	day := today
	if raw := q.Get("day"); raw != "" {
		d, err := diary.ParseDay(raw)
		if err != nil {
			s.badRequest(w, r, fmt.Sprintf("invalid day %q: expected YYYY-MM-DD", raw))
			return
		}
		day = d
	}
	dayMonth := dyMonthStart(day)
	railMonth := dayMonth
	if raw := q.Get("m"); raw != "" {
		m, err := diary.ParseMonth(raw)
		if err != nil {
			s.badRequest(w, r, fmt.Sprintf("invalid m %q: expected YYYY-MM", raw))
			return
		}
		railMonth = m
	}
	research := features.Enabled(s.effectiveFeatures(ctx), features.Research)
	db := s.db(ctx)

	data := diaryData{}
	entry, err := diary.Day(ctx, db, day)
	switch {
	case errors.Is(err, diary.ErrFutureDay):
		data.Future = true
		entry = dyEmptyEntry(day)
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if !research {
		withoutTrials(&entry)
	}
	if data.Month, err = dyMonth(ctx, db, railMonth); err != nil {
		s.serverError(w, r, err)
		return
	}
	// The agents' colors follow the day's month, so an agent wears one color
	// on every day of it, whichever month the rail shows.
	slotMonth := data.Month
	if !dayMonth.Equal(railMonth) && !data.Future {
		if slotMonth, err = dyMonth(ctx, db, dayMonth); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	data.Usual = diary.UsualDay{Series: []diary.MonthDay{}}
	if !data.Future {
		if data.Usual, err = diary.Usual(ctx, db, day); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	linkEntry(&entry, research)
	data.Entry = entry
	if !wantsJSON(r) {
		data.View = buildDiaryView(data, slotMonth, dyViewContext{
			now: now, today: today, day: day, railMonth: railMonth, research: research, locked: lockedProjects(ctx),
		})
	}
	s.render(w, r, "diary", pageData{
		Title:  "Diary, " + day.Format("Mon Jan 2"),
		Active: "diary",
		Data:   data,
	})
}

// dyMonth is the month holding m, or an empty one when it has not begun.
func dyMonth(ctx context.Context, db *sql.DB, m time.Time) (diary.MonthView, error) {
	view, err := diary.Month(ctx, db, m)
	if errors.Is(err, diary.ErrFutureDay) {
		start := dyMonthStart(m)
		return diary.MonthView{Month: start.Format("2006-01"), Start: start, End: start.AddDate(0, 1, 0),
			Days: []diary.MonthDay{}, Agents: []diary.AgentUsage{}}, nil
	}
	return view, err
}

// dyMidnight is the local midnight of the day holding t, in the daemon's
// zone, as the day model counts days.
func dyMidnight(t time.Time) time.Time {
	l := t.In(time.Local)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.Local)
}

// dyMonthStart is the local midnight of the first day of t's month.
func dyMonthStart(t time.Time) time.Time {
	l := t.In(time.Local)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, time.Local)
}

// dyEmptyEntry is the entry of a day that has not begun: its bounds and
// nothing else, every list a list.
func dyEmptyEntry(day time.Time) diary.Entry {
	lo := dyMidnight(day)
	hi := lo.AddDate(0, 0, 1)
	return diary.Entry{
		Day: lo.Format("2006-01-02"), Start: lo, End: hi, Hours: int(hi.Sub(lo) / time.Hour),
		Headline: "This day has not begun.", Spotlight: []diary.Thread{}, Also: []diary.Thread{},
		Short: diary.Short{Sessions: []diary.SessionDay{}},
		Kept:  diary.Kept{Memories: []diary.KeptItem{}, Notes: []diary.KeptItem{}, Trials: []diary.KeptItem{}},
		Moments: diary.Moments{Shipped: []diary.Ref{}, Commits: []diary.Ref{}, Flags: []diary.Flag{},
			Mishaps: []diary.Mishap{}},
		Usage: diary.Usage{Unreported: []diary.Ref{}, Agents: []diary.AgentUsage{}, Projects: []diary.ProjectUsage{},
			Hours: []diary.HourUsage{}},
		Projects: []diary.Ref{},
	}
}

// ---------------------------------------------------------------------------
// The optional features
// ---------------------------------------------------------------------------

// withoutTrials takes every trial out of an entry while the research feature
// is off: off means no trace, so the kept counts lose them, each thread's
// kept list and its "why this is here" too, and the headline and character
// are composed again from the counts that remain.
func withoutTrials(e *diary.Entry) {
	for _, it := range e.Kept.Trials {
		if it.New {
			e.Kept.New--
		} else {
			e.Kept.Updated--
		}
	}
	e.Facts.Kept -= len(e.Kept.Trials)
	e.Kept.Trials = []diary.KeptItem{}
	for _, threads := range []*[]diary.Thread{&e.Spotlight, &e.Also} {
		for i := range *threads {
			t := &(*threads)[i]
			kept := make([]diary.Ref, 0, len(t.Kept))
			for _, ref := range t.Kept {
				if ref.Kind != diary.RefTrial {
					kept = append(kept, ref)
				}
			}
			if gone := len(t.Kept) - len(kept); gone > 0 {
				t.Kept = kept
				t.Reasons, t.Score = keptReasonWithout(t.Reasons, t.Score, len(kept))
			}
		}
	}
	e.Character = diary.Character(e.Facts)
	e.Headline = diary.Headline(*e)
}

// keptReasonWithout recounts a thread's items-kept reason as n items, its
// points as the day model scores them, and the thread's score by the points
// it lost; a reason left with nothing is dropped.
func keptReasonWithout(reasons []diary.Reason, score float64, n int) ([]diary.Reason, float64) {
	out := make([]diary.Reason, 0, len(reasons))
	for _, r := range reasons {
		if r.Kind != diary.ReasonKept {
			out = append(out, r)
			continue
		}
		points := float64(min(n*diary.PointsKept, diary.CapKept))
		score -= r.Points - points
		if n > 0 {
			out = append(out, diary.Reason{Kind: r.Kind, Count: n, Points: points})
		}
	}
	return out, score
}

// ---------------------------------------------------------------------------
// The links
// ---------------------------------------------------------------------------

// linkEntry fills every Ref's Href: the console page of an item the day
// model resolved through the request's handle, "" for one it did not. A
// trial links only while research is on (and is not there at all while it is
// off); a plan links to its page only when it has one in its thread's
// project, else to that project's Plans & tasks tab, else nowhere; a commit
// links to the task or session that recorded it.
func linkEntry(e *diary.Entry, research bool) {
	projects := map[string]bool{}
	for _, p := range e.Projects {
		projects[p.ID] = p.Resolved
	}
	link := func(ref *diary.Ref) {
		if ref.Owner != nil {
			ref.Owner.Href = refHref(*ref.Owner, projects, research)
		}
		ref.Href = refHref(*ref, projects, research)
	}
	for i := range e.Projects {
		link(&e.Projects[i])
	}
	for _, threads := range [][]diary.Thread{e.Spotlight, e.Also} {
		for i := range threads {
			t := &threads[i]
			if t.Plan != nil {
				link(t.Plan)
			}
			for j := range t.Moved {
				link(&t.Moved[j].Task)
			}
			for j := range t.Sessions {
				link(&t.Sessions[j].Session)
			}
			for j := range t.Kept {
				link(&t.Kept[j])
			}
			for j := range t.Commits {
				link(&t.Commits[j])
			}
		}
	}
	for i := range e.Short.Sessions {
		link(&e.Short.Sessions[i].Session)
	}
	for _, items := range [][]diary.KeptItem{e.Kept.Memories, e.Kept.Notes, e.Kept.Trials} {
		for i := range items {
			link(&items[i].Item)
		}
	}
	for i := range e.Moments.Shipped {
		link(&e.Moments.Shipped[i])
	}
	for i := range e.Moments.Commits {
		link(&e.Moments.Commits[i])
	}
	for i := range e.Moments.Flags {
		link(&e.Moments.Flags[i].Session)
	}
	for i := range e.Moments.Mishaps {
		link(&e.Moments.Mishaps[i].Event)
		link(&e.Moments.Mishaps[i].Session)
	}
	for i := range e.Usage.Unreported {
		link(&e.Usage.Unreported[i])
	}
}

// refHref is one Ref's console page, "" when it has none to offer.
// projects says which project slugs resolved.
func refHref(ref diary.Ref, projects map[string]bool, research bool) string {
	id := url.PathEscape(ref.ID)
	switch ref.Kind {
	case diary.RefPlan:
		// A plan ref is always resolved: its link depends on whether it has
		// a page in its thread's project.
		switch {
		case ref.HasPage:
			return "/console/plans/" + id
		case ref.Project != "" && projects[ref.Project]:
			return "/console/projects/" + url.PathEscape(ref.Project) + "?tab=tasks"
		}
		return ""
	case diary.RefCommit:
		// A commit has no page of its own: it is the page of what recorded it.
		if ref.Owner == nil {
			return ""
		}
		return refHref(*ref.Owner, projects, research)
	}
	if !ref.Resolved || ref.ID == "" {
		return ""
	}
	switch ref.Kind {
	case diary.RefTask:
		return "/console/tasks/" + id
	case diary.RefSession:
		return "/console/sessions/" + id
	case diary.RefMemory:
		return "/console/memories/" + id
	case diary.RefNote:
		return "/console/notes/" + id
	case diary.RefTrial:
		if !research {
			return ""
		}
		return "/console/trials/" + id
	case diary.RefEvent:
		return "/console/events/" + id
	case diary.RefProject:
		return "/console/projects/" + id
	}
	return ""
}

// diaryHref is the Diary's link to a day, naming the rail's month only when
// it is not the day's own.
func diaryHref(day, railMonth time.Time) string {
	h := diaryPath + "?day=" + day.Format("2006-01-02")
	if m := dyMonthStart(railMonth); !m.Equal(dyMonthStart(day)) {
		h += "&m=" + m.Format("2006-01")
	}
	return h
}

// refName is what a Ref shows: its title, else its short id (a task or a
// session that no longer resolves has no title to show).
func refName(ref diary.Ref) string {
	if ref.Title != "" {
		return ref.Title
	}
	return shortID(ref.ID)
}

// dyGone is the title an item that no longer resolves carries.
const dyGone = "No longer exists"

// refLink renders a Ref as the Diary shows it: a link to its page, opening in
// the detail pane when the page has a peek (every entity page does; a plan's
// fallback, its project's Plans & tasks tab, does not), or its name as text,
// marked when the item no longer exists. class goes on the element.
func refLink(ref diary.Ref, class string) template.HTML {
	return refLinkAs(ref, class, refName(ref), "")
}

// refLinkAs is refLink with its own text, and a title for a link (an item
// that no longer exists keeps that title instead).
func refLinkAs(ref diary.Ref, class, text, linkTitle string) template.HTML {
	name := template.HTMLEscapeString(text)
	cls := ""
	if class != "" {
		cls = ` class="` + template.HTMLEscapeString(class) + `"`
	}
	if href, ok := safeHref(ref.Href); ok && ref.Href != "" {
		peek := " data-peek"
		if ref.Kind == diary.RefPlan && !ref.HasPage {
			peek = ` title="Plans &amp; tasks in ` + template.HTMLEscapeString(ref.Project) + `"`
		} else if linkTitle != "" {
			peek += ` title="` + template.HTMLEscapeString(linkTitle) + `"`
		}
		return template.HTML(`<a` + cls + ` href="` + href + `"` + peek + `>` + name + `</a>`)
	}
	title := dyGone
	switch {
	case ref.Kind == diary.RefPlan && ref.Project == "":
		title = "A plan in the global scope, with no page of its own"
	case ref.Kind == diary.RefPlan:
		title = "A plan with no page of its own, in a project that no longer exists"
	case ref.Resolved:
		title = ""
	}
	if title != "" {
		title = ` title="` + template.HTMLEscapeString(title) + `"`
	}
	textCls := strings.TrimSpace(class + " dy-text")
	if !ref.Resolved {
		textCls += " dy-gone"
	}
	return template.HTML(`<span class="` + template.HTMLEscapeString(textCls) + `"` + title + `>` + name + `</span>`)
}

// projectChip renders a project slug as a chip: a link to its page when it
// resolved, the global scope (and a working directory) as plain text.
func projectChip(slug, label string, projects map[string]diary.Ref) template.HTML {
	if slug == "" {
		if label == "" {
			label = "global"
		}
		title := "The global scope"
		if label != "global" {
			title = "Sessions in no project, named by their working directory"
		}
		return template.HTML(`<span class="dy-proj dy-proj-none" title="` + template.HTMLEscapeString(title) + `">` +
			string(icon("folder-tree")) + template.HTMLEscapeString(label) + `</span>`)
	}
	ref, ok := projects[slug]
	if !ok {
		ref = diary.Ref{Kind: diary.RefProject, ID: slug, Title: slug}
	}
	inner := string(icon("folder-tree")) + template.HTMLEscapeString(slug)
	if href, ok := safeHref(ref.Href); ok && ref.Href != "" {
		retired := ""
		if ref.Retired {
			retired = ` title="Retired: its items moved to other projects"`
		}
		return template.HTML(`<a class="dy-proj" href="` + href + `" data-peek` + retired + `>` + inner + `</a>`)
	}
	return template.HTML(`<span class="dy-proj dy-gone" title="` + dyGone + `">` + inner + `</span>`)
}

// ---------------------------------------------------------------------------
// The layout
// ---------------------------------------------------------------------------

// dyViewContext is what laying the page out needs beside the payload.
type dyViewContext struct {
	now, today, day, railMonth time.Time
	research                   bool
	locked                     []string
}

// diaryView is the page as templates/diary.html draws it.
type diaryView struct {
	Day       string // YYYY-MM-DD
	Weekday   string // "Friday"
	Date      string // "November 20"
	Year      string // "2026"
	Today     bool
	Future    bool
	Quiet     bool // nothing recorded on the day
	FirstRun  bool // nothing recorded on it, before it or in its month
	Character diary.Tag
	CharTone  string // the tag's tone class
	Headline  string
	LiveGap   int // diaryLiveGapMS, for the page script

	PrevHref, NextHref, TodayHref string
	PrevLabel, NextLabel          string

	Sessions     int // session-days
	Projects     int
	Started      int
	StartedHref  string
	Approximate  bool
	ApproxNote   string
	Unreported   []template.HTML
	Locked       []string
	TodayIsShown bool // the page shows today: the Today link is the page itself

	Tiles  []template.HTML
	Rhythm template.HTML

	Spotlight []dyThread
	Also      []dyThread
	Short     dyShort

	Ring, Mix, Ranked template.HTML
	RankedBy          string // "agent time" or "model tokens"
	Tables            template.HTML
	Kept              []dyItem
	KeptSummary       string
	Closed            []dyItem
	Shipped           []dyItem
	Commits           []dyCommit
	Flags             []dyItem
	Mishaps           []dyItem
	Moments           bool // any of Shipped, Commits, Flags, Mishaps

	Rail dyRail
}

// dyThread is one thread as a card (the spotlight) or a line (Also that day).
type dyThread struct {
	Key      string
	ID       string // the element id: stable across renders, so a morph keeps the card (and its open disclosure)
	Kind     string // "plan" or "project"
	Icon     string
	Title    template.HTML
	Project  template.HTML // a plan thread's project chip; "" for a project thread, whose title is its project
	Clause   string        // what it did, in a few words (Also that day)
	Shipped  bool
	Pinned   bool
	Progress *dyProgress
	Moves    []dyMove
	Sessions []dySession
	Kept     []template.HTML
	Commits  []dyCommit
	Presence template.HTML
	Minutes  string
	Tokens   string
	Count    int    // its sessions
	Counted  string // "2 sessions"
	Shared   int
	Unreport int
	Reasons  []dyReason
}

// dyProgress is a plan's steps closed of all, before and after the day.
type dyProgress struct {
	Before, After, Total int
	BeforePct, DayPct    string // the bar's two tones' widths, percentages of Total
	Line                 string // "12 → 14 of 14 steps closed"
}

// dyMove is one task that changed.
type dyMove struct {
	Task     template.HTML
	From, To string
	Tone     string // the end state's badge tone
	What     string // "closed", "created", "claimed", or "" for a plain move
	Changed  bool   // its status at the end differs from the start
}

// dySession is one session's line: what it was about, linked to its page.
type dySession struct {
	Pill        template.HTML
	Link        template.HTML // the about sentence, a link to the session
	Flag        template.HTML // the flag badge, a link to the session
	FromPrompt  bool          // the sentence is the session's first prompt of the day
	Minutes     string
	Tokens      string
	Unreported  bool
	Approximate string // "counted on Nov 12" for a session from before the ledger, "" otherwise
	Shared      bool
	Started     bool
	Ended       bool
}

// dyReason is one fact that put a thread in the spotlight.
type dyReason struct {
	Text   string
	Points string // "+3", "" for a pin
}

// dyShort is the folded short sessions.
type dyShort struct {
	Count    int
	Minutes  string
	Sessions []dySession
}

// dyItem is one item line in the usage panel's lists.
type dyItem struct {
	Icon string
	Link template.HTML
	Mark string // "superseded", "archived", "new", a verdict, an outcome
	Tone string // the mark's badge tone
	Sub  string // a second, quieter phrase
	At   time.Time
}

// dyCommit is one commit recorded: its short id, linked to the page of the
// task or session that recorded it (a commit has no page of its own), the
// full id to copy, and that task or session.
type dyCommit struct {
	Link  template.HTML
	Full  string
	Owner template.HTML
}

// dyRail is the month beside the entry.
type dyRail struct {
	Label              string // "November 2026"
	PrevHref, NextHref string
	PrevLabel          string
	NextLabel          string
	Grid               template.HTML
	Rows               []dyRailRow
	Future             bool // the month has not begun
	Empty              bool // no day of it recorded anything
}

// dyRailRow is one day of the rail, or one run of quiet days.
type dyRailRow struct {
	Title    string // the row's tooltip: its character and sessions
	Quiet    bool
	Label    string // "Fri 20", or "Sat 14 – Sun 15" for a run
	Href     string
	Selected bool
	Today    bool
	Bar      int // 0..100: its agent time against the month's busiest day
	Minutes  string
	Word     string
	Rule     string
	Dots     []string // its top projects, as their labels
	Sessions int
}

// buildDiaryView lays a day out. slotMonth is the day's month, whose agents
// pick the colors.
func buildDiaryView(d diaryData, slotMonth diary.MonthView, c dyViewContext) *diaryView {
	e := d.Entry
	v := &diaryView{
		Day: e.Day, Weekday: c.day.Format("Monday"), Date: c.day.Format("January 2"), Year: c.day.Format("2006"),
		Today: e.Today, Future: d.Future, Character: e.Character, CharTone: characterTone(e.Character.Word),
		Headline: e.Headline, LiveGap: diaryLiveGapMS, Locked: c.locked,
		Sessions: e.Facts.Sessions, Projects: e.Facts.Projects, Started: e.SessionsStarted,
		Approximate: e.Usage.Approximate, TodayIsShown: c.day.Equal(c.today),
	}
	v.Quiet = !d.Future && e.Character.Word == diary.WordQuiet
	v.FirstRun = v.Quiet && !d.Usual.Has && d.Usual.Days == 0 && monthQuiet(slotMonth)
	prev := c.day.AddDate(0, 0, -1)
	v.PrevHref, v.PrevLabel = diaryHref(prev, prev), prev.Format("Jan 2")
	if next := c.day.AddDate(0, 0, 1); !next.After(c.today) {
		v.NextHref, v.NextLabel = diaryHref(next, next), next.Format("Jan 2")
	}
	v.TodayHref = diaryHref(c.today, c.today)
	if e.SessionsStarted > 0 {
		v.StartedHref = "/console/sessions?day=" + e.Day
	}
	if v.Approximate {
		v.ApproxNote = "Token totals are approximate: a session from before the usage ledger counts its whole total " +
			"on the day it ended, with no hour, agent time or thread."
	}
	for _, ref := range e.Usage.Unreported {
		v.Unreported = append(v.Unreported, refLink(ref, ""))
	}
	v.Rail = buildRail(d.Month, c)
	if d.Future || v.Quiet {
		return v
	}

	projects := map[string]diary.Ref{}
	for _, p := range e.Projects {
		projects[p.ID] = p
	}
	hours := make([]time.Time, len(e.Usage.Hours))
	for i, h := range e.Usage.Hours {
		hours[i] = h.Start.In(time.Local)
	}
	slots := agentSlots(slotMonth.Agents, e.Usage.Agents)
	v.Tiles = diaryTiles(e, d.Usual)
	rhythm := rhythmSpecOf(e, hours, slots)
	v.Rhythm = rhythmChart(rhythm)
	for _, t := range e.Spotlight {
		v.Spotlight = append(v.Spotlight, buildThread(t, hours, projects))
	}
	for _, t := range e.Also {
		v.Also = append(v.Also, buildThread(t, hours, projects))
	}
	v.Short = dyShort{Count: e.Short.Count, Minutes: diary.Duration(e.Short.Minutes)}
	for _, sd := range e.Short.Sessions {
		v.Short.Sessions = append(v.Short.Sessions, buildSession(sd))
	}
	usagePanel(v, e, rhythm, slots, projects)
	return v
}

// monthQuiet reports whether no day of a month recorded anything.
func monthQuiet(m diary.MonthView) bool {
	for _, md := range m.Days {
		if md.Character.Word != diary.WordQuiet {
			return false
		}
	}
	return true
}

// characterTone is the character tag's tone class: a hue per word, none of
// them a status color but Shipped's.
func characterTone(word string) string {
	switch word {
	case diary.WordShipped:
		return "shipped"
	case diary.WordClosing:
		return "closing"
	case diary.WordPlanning:
		return "planning"
	case diary.WordDeep:
		return "deep"
	case diary.WordBusy:
		return "busy"
	}
	return "calm"
}

// agentWeight orders agents for their colors: agent time first, then model
// tokens, as the day model ranks them.
func agentWeight(a diary.AgentUsage) int64 {
	const tokenRoom int64 = 1_000_000_000_000 // above any month's tokens
	return int64(a.Minutes)*tokenRoom + min(int64(a.Tokens.Total), tokenRoom-1)
}

// agentSlots picks each agent's color for the day: the slots its month's five
// busiest agents wear, the rest folded into Other. A month that names no agent
// at all leaves the day's own order to pick.
func agentSlots(month, day []diary.AgentUsage) map[string]int {
	totals := make(map[string]int64, len(month))
	for _, a := range month {
		totals[a.Key] = agentWeight(a)
	}
	if len(totals) == 0 {
		for _, a := range day {
			totals[a.Key] = agentWeight(a)
		}
	}
	return assignSlots(totals)
}

// agentName is an agent's short name, as its pill reads: "cc · opus-5-5".
func agentName(harness, model string) string {
	_, short, full := agentDisplay(harness)
	m := modelShort(model)
	switch {
	case short != "" && m != "":
		return short + " · " + m
	case m != "":
		return m
	case full != "":
		return full
	}
	return "unnamed agent"
}

// diaryTiles are the four headline numbers, each against the owner's usual
// day and each a jump down to the part of the entry that lists it.
func diaryTiles(e diary.Entry, u diary.UsualDay) []template.HTML {
	f := e.Facts
	soFar := ""
	if e.Today {
		// Part of a day against a whole usual day would read as a fall every
		// morning: today states the usual figure and draws no delta.
		soFar = "so far"
	}
	judged := u.Has && !e.Today
	usual := func(fig string) string {
		if !u.Has {
			return ""
		}
		return "usual day " + fig
	}
	series := func(of func(diary.Facts) int, now int) spark {
		pts := make([]store.TrendBucket, 0, len(u.Series)+1)
		for _, md := range u.Series {
			pts = append(pts, store.TrendBucket{Label: md.Day, Count: of(md.Facts)})
		}
		pts = append(pts, store.TrendBucket{Label: e.Day, Count: now})
		return spark{Points: pts, Label: fmt.Sprintf("The last %d active days, then this one", len(u.Series))}
	}
	minutes := tileSpec{Label: "Agent time", Icon: "clock", Value: chartMinutes(int64(f.Minutes)),
		Sub:   joinSub(soFar, usual(chartMinutes(int64(u.Minutes)))),
		Delta: volumeDelta(f.Minutes, u.Minutes, judged, noJudgment),
		Spark: series(func(x diary.Facts) int { return x.Minutes }, f.Minutes)}
	if f.Minutes > 0 {
		minutes.Anchor = "#diary-rhythm" // the rhythm draws only agent time
	}
	if f.Minutes == 0 && f.Approximate {
		minutes.Value, minutes.Sub, minutes.Delta = "", "not recorded before the usage ledger", delta{}
	}
	tokens := tileSpec{Label: "Model tokens", Icon: "zap", Value: compactNum(f.Tokens),
		Sub:    joinSub(soFar, usual(compactNum(u.Tokens))),
		Delta:  volumeDelta(f.Tokens, u.Tokens, judged, noJudgment),
		Spark:  series(func(x diary.Facts) int { return x.Tokens }, f.Tokens),
		Anchor: "#diary-usage"}
	switch {
	case f.Unreported > 0:
		tokens.Sub = joinSub(tokens.Sub, "("+strconv.Itoa(f.Unreported)+" unreported)")
	case f.Approximate:
		tokens.Sub = joinSub("approximate", usual(compactNum(u.Tokens)))
	}
	closed := tileSpec{Label: "Tasks closed", Icon: "list-checks", Value: strconv.Itoa(f.TasksClosed),
		Sub:    usual(strconv.Itoa(u.TasksClosed)),
		Delta:  volumeDelta(f.TasksClosed, u.TasksClosed, judged, noJudgment),
		Spark:  series(func(x diary.Facts) int { return x.TasksClosed }, f.TasksClosed),
		Anchor: "#diary-closed"}
	keptSub := ""
	if e.Kept.New+e.Kept.Updated > 0 {
		keptSub = fmt.Sprintf("%d new, %d updated", e.Kept.New, e.Kept.Updated)
	}
	kept := tileSpec{Label: "Kept", Icon: "pencil", Value: strconv.Itoa(f.Kept),
		Sub:    joinSub(keptSub, usual(strconv.Itoa(u.Kept))),
		Delta:  volumeDelta(f.Kept, u.Kept, judged, noJudgment),
		Spark:  series(func(x diary.Facts) int { return x.Kept }, f.Kept),
		Anchor: "#diary-kept"}
	return []template.HTML{statTile(minutes), statTile(tokens), statTile(closed), statTile(kept)}
}

// joinSub joins a tile's sub-line phrases with a middle dot, skipping the
// empty ones.
func joinSub(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// buildThread lays one thread out.
func buildThread(t diary.Thread, hours []time.Time, projects map[string]diary.Ref) dyThread {
	out := dyThread{
		Key: t.Key, ID: dyElementID(t.Key), Kind: string(t.Kind), Shipped: t.Shipped, Pinned: t.Pinned,
		Minutes: diary.Duration(t.Minutes), Tokens: compactNum(t.Tokens.Total),
		Count: len(t.Sessions), Counted: plural(len(t.Sessions), "session", "sessions"),
		Shared: t.Shared, Unreport: t.Unreported,
	}
	label := t.Label
	if label == "" {
		label = "global"
	}
	switch t.Kind {
	case diary.ThreadPlan:
		out.Icon = "map"
		if t.Plan != nil {
			out.Title = refLink(*t.Plan, "dy-card-name")
		} else {
			out.Title = template.HTML(`<span class="dy-card-name dy-text">` + template.HTMLEscapeString(label) + `</span>`)
		}
		out.Project = projectChip(t.Project, "", projects)
	default:
		out.Icon = "folder-tree"
		out.Title = projectTitle(t, label, projects)
	}
	if p := t.Progress; p != nil && p.End.Total > 0 {
		before := min(p.Start.Closed, p.End.Closed)
		out.Progress = &dyProgress{
			Before: p.Start.Closed, After: p.End.Closed, Total: p.End.Total,
			BeforePct: pctOf(before, p.End.Total), DayPct: pctOf(p.End.Closed-before, p.End.Total),
		}
		if p.Start.Closed == p.End.Closed {
			out.Progress.Line = fmt.Sprintf("%d of %d steps closed", p.End.Closed, p.End.Total)
		} else {
			out.Progress.Line = fmt.Sprintf("%d → %d of %d steps closed", p.Start.Closed, p.End.Closed, p.End.Total)
		}
	}
	for _, m := range t.Moved {
		mv := dyMove{Task: refLink(m.Task, "dy-move-name"), From: dyStatus(m.From), To: dyStatus(m.To),
			Tone: taskTone(m.To), Changed: m.From != m.To}
		switch {
		case m.Closed:
			mv.What = "closed"
		case m.Created:
			mv.What = "created"
		case m.Claimed:
			mv.What = "claimed"
		}
		out.Moves = append(out.Moves, mv)
	}
	for _, sd := range t.Sessions {
		out.Sessions = append(out.Sessions, buildSession(sd))
	}
	for _, ref := range t.Kept {
		out.Kept = append(out.Kept, refLink(ref, ""))
	}
	for _, c := range t.Commits {
		out.Commits = append(out.Commits, buildCommit(c))
	}
	spans := make([]presenceSpan, 0, len(t.Hours))
	for i, m := range t.Hours {
		if m > 0 && i < len(hours) {
			spans = append(spans, presenceSpan{From: hours[i], To: hours[i].Add(time.Hour)})
		}
	}
	out.Presence = presenceStrip(presenceSpec{Label: "Hours it was worked in", Hours: hours, Spans: spans})
	for _, r := range t.Reasons {
		out.Reasons = append(out.Reasons, reasonLine(r, t.Kind == diary.ThreadPlan))
	}
	out.Clause = threadClause(t)
	return out
}

// dyElementID is a thread's element id: its key hashed, since a key can hold
// a working directory's name, which may hold characters no id may.
func dyElementID(key string) string {
	return "dy-t" + strconv.FormatUint(uint64(skyHash(key)), 36)
}

// projectTitle is a project thread's title: its project, linked when it
// resolved; sessions in no project read as their directory or "global".
func projectTitle(t diary.Thread, label string, projects map[string]diary.Ref) template.HTML {
	if t.Project == "" {
		title := "The global scope"
		if strings.HasPrefix(t.Key, "cwd:") {
			title = "Sessions in no project, named by their working directory"
		}
		return template.HTML(`<span class="dy-card-name dy-text" title="` + template.HTMLEscapeString(title) + `">` +
			template.HTMLEscapeString(label) + `</span>`)
	}
	ref, ok := projects[t.Project]
	if !ok {
		ref = diary.Ref{Kind: diary.RefProject, ID: t.Project, Title: t.Project}
	}
	return refLink(ref, "dy-card-name")
}

// pctOf is part of whole as a percentage with two decimals, "0" for no
// whole: a width in a style attribute.
func pctOf(part, whole int) string {
	if whole <= 0 || part <= 0 {
		return "0"
	}
	return strconv.FormatFloat(float64(part)/float64(whole)*100, 'f', 2, 64)
}

// dyStatus words a task status for a line ("in progress"); "" is a task
// created on the day.
func dyStatus(s string) string {
	switch s {
	case "":
		return "new"
	case "in_progress":
		return "in progress"
	}
	return s
}

// threadClause says in a few words what a thread did, for its line under
// Also that day.
func threadClause(t diary.Thread) string {
	closed, moved := 0, 0
	for _, m := range t.Moved {
		if m.Closed {
			closed++
		}
		if m.From != m.To || m.Created {
			moved++
		}
	}
	noun := "task"
	if t.Kind == diary.ThreadPlan {
		noun = "step"
	}
	var parts []string
	switch {
	case closed > 0:
		parts = append(parts, plural(closed, noun+" closed", noun+"s closed"))
	case moved > 0:
		parts = append(parts, plural(moved, noun+" moved", noun+"s moved"))
	}
	if len(t.Kept) > 0 {
		parts = append(parts, plural(len(t.Kept), "item kept", "items kept"))
	}
	if t.Minutes > 0 {
		parts = append(parts, diary.Duration(t.Minutes))
	}
	if len(parts) == 0 {
		parts = append(parts, plural(len(t.Sessions), "session", "sessions"))
	}
	return strings.Join(parts, " · ")
}

// reasonLine words one reason a thread is in the spotlight.
func reasonLine(r diary.Reason, plan bool) dyReason {
	noun := "task"
	if plan {
		noun = "step"
	}
	pts := "+" + strconv.FormatFloat(math.Round(r.Points*10)/10, 'f', -1, 64)
	switch r.Kind {
	case diary.ReasonShipped:
		return dyReason{Text: "The plan shipped: its last open step closed as done", Points: pts}
	case diary.ReasonClosed:
		return dyReason{Text: plural(r.Count, noun+" closed as done", noun+"s closed as done"), Points: pts}
	case diary.ReasonStarted:
		return dyReason{Text: plural(r.Count, noun+" created or claimed", noun+"s created or claimed"), Points: pts}
	case diary.ReasonKept:
		return dyReason{Text: plural(r.Count, "item kept", "items kept"), Points: pts}
	case diary.ReasonCommits:
		return dyReason{Text: plural(r.Count, "commit recorded", "commits recorded"), Points: pts}
	case diary.ReasonFindings:
		return dyReason{Text: plural(r.Count, "session ended with findings it wrote", "sessions ended with findings they wrote"),
			Points: pts}
	case diary.ReasonMinutes:
		return dyReason{Text: diary.Duration(r.Count) + " of agent time", Points: pts}
	case diary.ReasonOutput:
		return dyReason{Text: compactNum(r.Count) + " output tokens", Points: pts}
	case diary.ReasonFlagged:
		return dyReason{Text: "A flagged session keeps it in the spotlight"}
	}
	return dyReason{Text: r.Kind, Points: pts}
}

// buildSession lays one session-day out.
func buildSession(sd diary.SessionDay) dySession {
	about := cmp.Or(sd.About, refName(sd.Session))
	title := refName(sd.Session)
	if sd.AboutFrom == diary.AboutPrompt {
		title += ": its first prompt of the day"
	}
	cls := "dy-sess-about"
	if sd.AboutFrom == diary.AboutPrompt {
		cls += " is-prompt"
	}
	out := dySession{
		Pill: agentPill(sd.Harness, sd.Model), Link: refLinkAs(sd.Session, cls, about, title),
		FromPrompt: sd.AboutFrom == diary.AboutPrompt,
		Minutes:    diary.Duration(sd.Minutes), Tokens: compactNum(sd.Tokens.Total),
		Unreported: sd.Unreported, Shared: sd.Shared, Started: sd.Started, Ended: sd.Ended,
	}
	if sd.Minutes == 0 && sd.Approximate {
		out.Minutes = "" // from before the ledger: no agent time was recorded
	}
	if sd.Approximate {
		out.Approximate = "approximate"
		if day, err := diary.ParseDay(sd.EndedOn); err == nil {
			out.Approximate = "counted on " + day.Format("Jan 2")
		}
	}
	if sd.Flag != "" {
		badge := `<span class="badge ` + verdictTone(core.SessionVerdict(sd.Flag)) + ` flag-badge">` + string(icon("flag")) +
			template.HTMLEscapeString(sd.Flag) + `</span>`
		if href, ok := safeHref(sd.Session.Href); ok && sd.Session.Href != "" {
			badge = `<a class="dy-flag" href="` + href + `" data-peek title="Flagged ` + template.HTMLEscapeString(sd.Flag) + `">` +
				badge + `</a>`
		}
		out.Flag = template.HTML(badge)
	}
	return out
}

// buildCommit lays one commit recorded out: its short id, the full id to
// copy, and the task or session that recorded it.
func buildCommit(c diary.Ref) dyCommit {
	out := dyCommit{Full: c.ID}
	short := `<code>` + template.HTMLEscapeString(c.Title) + `</code>`
	out.Link = template.HTML(short)
	if c.Owner == nil {
		return out
	}
	if href, ok := safeHref(c.Href); ok && c.Href != "" {
		out.Link = template.HTML(`<a class="dy-commit" href="` + href + `" data-peek title="Recorded by this ` +
			template.HTMLEscapeString(string(c.Owner.Kind)) + `">` + short + `</a>`)
	}
	out.Owner = refLink(*c.Owner, "")
	return out
}

// usagePanel fills the usage panel: the agents ring, the token mix, the
// projects, the tables (the rhythm's among them), and what the day kept and
// marked.
func usagePanel(v *diaryView, e diary.Entry, rhythm rhythmSpec, slots map[string]int, projects map[string]diary.Ref) {
	ring := ringSpec{Label: "Model tokens by agent", Unit: unitTokens, Core: "tokens"}
	for _, a := range e.Usage.Agents {
		ring.Slices = append(ring.Slices, ringSlice{Name: agentName(a.Harness, a.Model), Slot: slots[a.Key],
			Value: int64(a.Tokens.Total)})
	}
	v.Ring = ringChart(ring)
	tk := e.Usage.Tokens
	mix := mixSpec{Label: "Token mix", Unit: unitTokens, Parts: []mixPart{
		{Name: "Output", Value: int64(tk.Output)}, {Name: "Fresh input", Value: int64(tk.Input)},
		{Name: "Cache write", Value: int64(tk.CacheCreation)}, {Name: "Cache read", Value: int64(tk.Cached)},
	}}
	v.Mix = mixBar(mix)
	rank := rankSpec{Label: "Agent time by project", Unit: unitMinutes}
	v.RankedBy = "agent time"
	byTokens := e.Facts.Minutes == 0
	if byTokens {
		rank = rankSpec{Label: "Model tokens by project", Unit: unitTokens}
		v.RankedBy = "model tokens"
	}
	for _, p := range e.Usage.Projects {
		row := rankRow{Name: p.Label, Value: int64(p.Minutes)}
		if byTokens {
			row.Value = int64(p.Tokens.Total)
		}
		switch ref, ok := projects[p.Project]; {
		case p.Project == "" && p.Label != "global":
			row.Title = "Sessions in no project, named by their working directory"
		case p.Project == "":
			row.Title = "The global scope"
		case ok && ref.Href != "":
			row.Href, row.Peek = ref.Href, true
		default:
			row.Title = dyGone
		}
		rank.Rows = append(rank.Rows, row)
	}
	v.Ranked = rankBars(rank)
	v.Tables = template.HTML(string(rhythmTable(rhythm)) + string(ringTable(ring)) +
		string(mixTable(mix)) + string(rankTable(rank)))

	for _, it := range e.Kept.Memories {
		item := dyItem{Icon: "database", Link: refLink(it.Item, ""), At: it.At}
		switch it.Item.Status {
		case diary.MemorySuperseded:
			item.Mark, item.Tone = "superseded", "warn"
		case diary.MemoryArchived:
			item.Mark, item.Tone = "archived", ""
		}
		item.Sub = keptSub(it)
		v.Kept = append(v.Kept, item)
	}
	for _, it := range e.Kept.Notes {
		v.Kept = append(v.Kept, dyItem{Icon: "file-text", Link: refLink(it.Item, ""), Sub: keptSub(it), At: it.At})
	}
	for _, it := range e.Kept.Trials {
		item := dyItem{Icon: "test-tube", Link: refLink(it.Item, ""), Sub: keptSub(it), At: it.At}
		if it.Item.Status != "" {
			item.Mark, item.Tone = it.Item.Status, outcomeTone(it.Item.Status)
		}
		v.Kept = append(v.Kept, item)
	}
	v.KeptSummary = keptSummary(e.Kept)
	for _, threads := range [][]diary.Thread{e.Spotlight, e.Also} {
		for _, t := range threads {
			for _, m := range t.Moved {
				if m.Closed {
					v.Closed = append(v.Closed, dyItem{Icon: "list-checks", Link: refLink(m.Task, ""), At: m.At})
				}
			}
		}
	}
	slices.SortStableFunc(v.Closed, func(a, b dyItem) int { return a.At.Compare(b.At) })
	for _, p := range e.Moments.Shipped {
		v.Shipped = append(v.Shipped, dyItem{Icon: "flag", Link: refLink(p, ""), Sub: "shipped"})
	}
	for _, c := range e.Moments.Commits {
		v.Commits = append(v.Commits, buildCommit(c))
	}
	for _, f := range e.Moments.Flags {
		v.Flags = append(v.Flags, dyItem{Icon: "flag", Link: refLink(f.Session, ""), Mark: f.Verdict,
			Tone: verdictTone(core.SessionVerdict(f.Verdict)), Sub: f.Reason, At: f.At})
	}
	for _, m := range e.Moments.Mishaps {
		v.Mishaps = append(v.Mishaps, dyItem{Icon: "triangle-alert", Link: refLink(m.Event, ""), Sub: refName(m.Session), At: m.At})
	}
	v.Moments = len(v.Shipped)+len(v.Commits)+len(v.Flags)+len(v.Mishaps) > 0
}

// rhythmSpecOf is the day's agent time by hour, one column per local hour of
// it, stacked by agent in the agents' colors. Its full height stands for at
// least an hour of agent time, so a light day reads as light.
func rhythmSpecOf(e diary.Entry, hours []time.Time, slots map[string]int) rhythmSpec {
	spec := rhythmSpec{Label: "Agent time by hour", Unit: unitMinutes, Hours: hours, MinScale: 60}
	for _, a := range e.Usage.Agents {
		vals := make([]int64, len(e.Usage.Hours))
		for i, h := range e.Usage.Hours {
			for _, ah := range h.Agents {
				if ah.Key == a.Key {
					vals[i] = int64(ah.Minutes)
				}
			}
		}
		spec.Series = append(spec.Series, rhythmSeries{Name: agentName(a.Harness, a.Model), Slot: slots[a.Key], Values: vals})
	}
	return spec
}

// keptSub says how an item was written on the day.
func keptSub(it diary.KeptItem) string {
	verb := "updated"
	if it.New {
		verb = "new"
	}
	if it.Writes > 1 {
		return fmt.Sprintf("%s, %d writes", verb, it.Writes)
	}
	return verb
}

// keptSummary counts what the day kept, by kind.
func keptSummary(k diary.Kept) string {
	var parts []string
	if n := len(k.Memories); n > 0 {
		parts = append(parts, plural(n, "memory", "memories"))
	}
	if n := len(k.Notes); n > 0 {
		parts = append(parts, plural(n, "note", "notes"))
	}
	if n := len(k.Trials); n > 0 {
		parts = append(parts, plural(n, "trial", "trials"))
	}
	return joinWithAnd(parts)
}

// buildRail lays the rail out: the month's heat grid, then its days newest
// first, a run of quiet days as one thin row.
func buildRail(m diary.MonthView, c dyViewContext) dyRail {
	start := dyMonthStart(c.railMonth)
	rail := dyRail{Label: start.Format("January 2006")}
	prev := start.AddDate(0, -1, 0)
	rail.PrevHref, rail.PrevLabel = diaryHref(c.day, prev), prev.Format("January")
	if next := start.AddDate(0, 1, 0); !next.After(c.today) {
		rail.NextHref, rail.NextLabel = diaryHref(c.day, next), next.Format("January")
	}
	if len(m.Days) == 0 {
		rail.Future = start.After(c.today)
		rail.Empty = true
		return rail
	}
	byDay := map[string]diary.MonthDay{}
	maxMin := 0
	for _, md := range m.Days {
		byDay[md.Day] = md
		maxMin = max(maxMin, md.Facts.Minutes)
	}
	end := start.AddDate(0, 1, 0)
	todayKey, dayKey := c.today.Format("2006-01-02"), c.day.Format("2006-01-02")
	var cells []monthDay
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		md, ok := byDay[d.Format("2006-01-02")]
		cell := monthDay{Value: int64(md.Facts.Minutes)}
		if ok {
			cell.Href = diaryHref(d, d)
			cell.Note = railNote(md)
		}
		cells = append(cells, cell)
	}
	rail.Grid = monthGrid(monthSpec{Label: "Agent time per day", Unit: unitMinutes, Start: start, Days: cells,
		Today: todayKey, Selected: dayKey})

	rail.Empty = true
	for i := len(m.Days) - 1; i >= 0; i-- {
		md := m.Days[i]
		d, err := diary.ParseDay(md.Day)
		if err != nil {
			continue
		}
		quiet := md.Character.Word == diary.WordQuiet
		row := dyRailRow{Label: d.Format("Mon 2"), Href: diaryHref(d, d), Selected: md.Day == dayKey,
			Today: md.Day == todayKey, Quiet: quiet}
		if quiet {
			// A run of quiet days is one thin row: a pause, never a warning.
			if n := len(rail.Rows); n > 0 && rail.Rows[n-1].Quiet && !rail.Rows[n-1].Today && !row.Today {
				last := &rail.Rows[n-1]
				newest, _, _ := strings.Cut(last.Label, " – ")
				last.Label = d.Format("Mon 2") + " – " + newest
				last.Selected = last.Selected || row.Selected
				continue
			}
			rail.Rows = append(rail.Rows, row)
			continue
		}
		rail.Empty = false
		row.Title = railNote(md)
		row.Minutes = diary.Duration(md.Facts.Minutes)
		row.Word, row.Rule = md.Character.Word, md.Character.Rule
		row.Sessions = md.Facts.Sessions
		if maxMin > 0 {
			row.Bar = max(4, md.Facts.Minutes*100/maxMin)
		}
		if md.Facts.Minutes == 0 {
			row.Bar = 0
		}
		for _, p := range md.Projects {
			row.Dots = append(row.Dots, cmp.Or(p.Label, "global"))
		}
		rail.Rows = append(rail.Rows, row)
	}
	return rail
}

// railNote is a rail day's second line in the grid's readout.
func railNote(md diary.MonthDay) string {
	f := md.Facts
	if md.Character.Word == diary.WordQuiet {
		return "quiet"
	}
	parts := []string{plural(f.Sessions, "session", "sessions")}
	if f.TasksClosed > 0 {
		parts = append(parts, plural(f.TasksClosed, "task closed", "tasks closed"))
	}
	parts = append(parts, md.Character.Word)
	return strings.Join(parts, " · ")
}
