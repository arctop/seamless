package diary

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// MonthProjects is how many projects a month's day names: its top ones by
// agent time.
const MonthProjects = 3

// Month returns the local month holding month, in the daemon's zone, read
// through db: a row per day that has begun, oldest first. A month that has
// not begun is ErrFutureDay. It reads each source once for the whole month.
func Month(ctx context.Context, db *sql.DB, month time.Time) (MonthView, error) {
	return monthAt(ctx, db, month, time.Now(), time.Local)
}

// monthAt is Month at now, in loc.
func monthAt(ctx context.Context, db *sql.DB, month, now time.Time, loc *time.Location) (MonthView, error) {
	m := month.In(loc)
	lo := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, loc)
	hi := lo.AddDate(0, 1, 0)
	if lo.After(now) {
		return MonthView{}, ErrFutureDay
	}
	view := MonthView{Month: lo.Format(monthLayout), Start: lo, End: hi, Days: []MonthDay{}}
	// The days that have begun: today is the last.
	if _, tomorrow := dayBounds(now, loc); tomorrow.Before(hi) {
		hi = tomorrow
	}
	s, err := load(ctx, db, lo, hi, loc, true)
	if err != nil {
		return MonthView{}, err
	}
	agents := newAgentTotals()
	for _, d := range s.split() {
		t := s.count(d)
		view.Days = append(view.Days, monthDay(s, d, t))
		agents.add(s, d, t)
	}
	view.Agents = agents.list()
	return view, nil
}

// monthDay is one day's row, from its tally t: its Facts as Day counts them,
// its top projects and its character.
func monthDay(s *span, d dayRows, t *tally) MonthDay {
	f := t.facts()
	places := t.places()
	projects := slices.Clone(places[:min(len(places), MonthProjects)])
	if projects == nil {
		projects = []ProjectUsage{}
	}
	return MonthDay{Day: dayKey(d.lo, s.loc), Facts: f, Projects: projects, Character: Character(f)}
}

// agentTotals sums a month's days by agent the way Day's usage sums one day:
// the ledger rows by the session's harness and the row's model, split by
// thread, and the stored total of a session from before the ledger on the
// day it ended. Its sessions are counted once over the whole month.
type agentTotals struct {
	by       map[string]*AgentUsage
	sessions map[string]map[string]bool
}

func newAgentTotals() *agentTotals {
	return &agentTotals{by: map[string]*AgentUsage{}, sessions: map[string]map[string]bool{}}
}

// agent is the running total of one harness and model.
func (a *agentTotals) agent(harness, model string) *AgentUsage {
	key := harness + "/" + model
	u, ok := a.by[key]
	if !ok {
		u = &AgentUsage{Key: key, Harness: harness, Model: model}
		a.by[key] = u
		a.sessions[key] = map[string]bool{}
	}
	return u
}

// add counts day d of span s, its tally t.
func (a *agentTotals) add(s *span, d dayRows, t *tally) {
	for _, r := range d.usage {
		u := a.agent(harnessOfSpan(s.sessions[r.SessionID]), r.Model)
		m, tok := minutesOf(r), r.Tokens()
		u.Minutes += m
		u.Tokens = addTokens(u.Tokens, tok)
		part := &u.Main
		if r.Thread == core.UsageThreadSubagent {
			part = &u.Subagent
		}
		part.Minutes += m
		part.Tokens += tok.Total
		a.sessions[u.Key][r.SessionID] = true
	}
	for _, id := range t.sessions {
		su := t.use[id]
		if su.unreported || !su.approximate || !su.counted {
			continue
		}
		ss := s.sessions[id]
		u := a.agent(harnessOfSpan(ss), ss.Model)
		u.Tokens = addTokens(u.Tokens, ss.Totals)
		u.Approximate.Tokens += ss.Totals.Total
		a.sessions[u.Key][id] = true
	}
}

// list is the totals, most agent time first, as Usage.Agents orders a day.
func (a *agentTotals) list() []AgentUsage {
	out := make([]AgentUsage, 0, len(a.by))
	for key, u := range a.by {
		u.Sessions = len(a.sessions[key])
		out = append(out, *u)
	}
	slices.SortFunc(out, func(x, y AgentUsage) int {
		return cmp.Or(cmp.Compare(y.Minutes, x.Minutes), cmp.Compare(y.Tokens.Total, x.Tokens.Total), cmp.Compare(x.Key, y.Key))
	})
	return out
}

// harnessOfSpan is harnessOf for a session as a span holds it: its recorded
// client, else its ambient name's prefix; "" for a session the handle does
// not hold, as Day's agents read it.
func harnessOfSpan(s store.DiarySession) string {
	return harnessOf(core.Session{ExternalClient: s.Client, Name: s.Name})
}

// The usual day: the median of the trailing active days before a day.
const (
	// UsualDays is how many active days the comparison reads at most.
	UsualDays = 14
	// UsualMinDays is how many it needs, or there is no comparison.
	UsualMinDays = 3
	// UsualLookbackDays bounds how far back it looks for them.
	UsualLookbackDays = 90
)

// Usual returns the owner's usual day before the local day holding day, in
// the daemon's zone, read through db: the median agent time, tokens, tasks
// closed and items kept over the trailing active days (a day with a session,
// a task closed or an item kept), at most UsualDays of them within
// UsualLookbackDays. With fewer than UsualMinDays, Has is false.
func Usual(ctx context.Context, db *sql.DB, day time.Time) (UsualDay, error) {
	return usualAt(ctx, db, day, time.Local)
}

// usualAt is Usual in loc. It reads the lookback in widening windows, the
// nearest first, and stops once it has UsualDays active days.
func usualAt(ctx context.Context, db *sql.DB, day time.Time, loc *time.Location) (UsualDay, error) {
	end, _ := dayBounds(day, loc)
	var active []MonthDay // newest first
	for _, back := range []int{UsualDays + 7, 45, UsualLookbackDays} {
		active = active[:0]
		lo := end.AddDate(0, 0, -back)
		s, err := load(ctx, db, lo, end, loc, false)
		if err != nil {
			return UsualDay{}, err
		}
		days := s.split()
		for i := len(days) - 1; i >= 0 && len(active) < UsualDays; i-- {
			f := s.count(days[i]).facts()
			if f.Sessions > 0 || f.TasksClosed > 0 || f.Kept > 0 {
				active = append(active, MonthDay{Day: dayKey(days[i].lo, loc), Facts: f, Projects: []ProjectUsage{}})
			}
		}
		if len(active) >= UsualDays {
			break
		}
	}
	slices.Reverse(active)
	u := UsualDay{Days: len(active), Series: active}
	if u.Series == nil {
		u.Series = []MonthDay{}
	}
	if len(active) < UsualMinDays {
		return u, nil
	}
	u.Has = true
	pick := func(of func(Facts) int) int {
		vals := make([]int, len(active))
		for i, d := range active {
			vals[i] = of(d.Facts)
		}
		return median(vals)
	}
	u.Minutes = pick(func(f Facts) int { return f.Minutes })
	u.Tokens = pick(func(f Facts) int { return f.Tokens })
	u.TasksClosed = pick(func(f Facts) int { return f.TasksClosed })
	u.Kept = pick(func(f Facts) int { return f.Kept })
	return u, nil
}

// median is the middle of vals, the mean of the two middles (rounded half
// up) for an even count.
func median(vals []int) int {
	if len(vals) == 0 {
		return 0
	}
	s := slices.Clone(vals)
	slices.SortFunc(s, cmp.Compare[int])
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2] + 1) / 2
}
