package diary

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// place is where a session or a task belongs on a day: its project, or for a
// session in no project its working directory's last path element.
type place struct {
	project string // the slug, "" for none
	label   string // the slug, the directory, or "global"
	key     string // project:<slug>, cwd:<directory>, or "project:" for global
}

// globalLabel names the global scope, as every console screen does.
const globalLabel = "global"

// placeOf is the place of a project and working directory.
func placeOf(project, cwd string) place {
	if project != "" {
		return place{project: project, label: project, key: "project:" + project}
	}
	if base := lastElement(cwd); base != "" {
		return place{label: base, key: "cwd:" + base}
	}
	return place{label: globalLabel, key: "project:"}
}

// lastElement is a path's last element, POSIX or Windows, "" for none.
func lastElement(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// sessionUse is one session's part of one day.
type sessionUse struct {
	id      string
	minutes int
	tokens  core.TokenUsage
	rows    []store.UsageRow // the day's ledger rows of it
	// unreported: no token record at all. approximate: from before the
	// ledger, its stored total counted on endedOn alone (counted: today).
	unreported, approximate, counted bool
	endedOn                          string
	// started and ended: it began, or ended, on the day. outcome: it did
	// something the record keeps (a credited event, a commit recorded, or an
	// end with findings it wrote). minor: the fold's test (minor).
	started, ended, outcome, minor bool
}

// keptKey is one distinct item written on a day.
type keptKey struct {
	kind core.EventKind
	id   string
}

// tally is one day counted: the session-days and the rest of Facts' inputs,
// from the span's rules alone. Day and Month both count with it.
type tally struct {
	sessions []string // session-days, by id ascending
	use      map[string]*sessionUse
	short    int // the minor session-days
	minutes  int
	tokens   core.TokenUsage
	closed   []string // tasks closed as done, by id ascending
	created  []string
	kept     []keptKey
	drafted  []store.PlanRef
	shipped  []store.PlanRef
	projects map[string]bool // slugs of the session-days and the tasks that moved
	working  map[string]bool // slugs of the session-days that are not minor and the tasks that moved
	// byPlace is the day's usage by place key; places the places.
	byPlace map[string]*ProjectUsage
	approx  bool
}

// minutesOf is a ledger row's active minutes.
func minutesOf(r store.UsageRow) int { return r.ActiveMinutes() }

// sessionOwner is the session a commit record belongs to, "" for a task's.
func sessionOwner(c store.DiaryCommit) string {
	if c.Owner == store.CommitOwnerSession {
		return c.OwnerID
	}
	return ""
}

// count tallies day d of span s.
func (s *span) count(d dayRows) *tally {
	t := &tally{use: map[string]*sessionUse{}, projects: map[string]bool{}, working: map[string]bool{},
		byPlace: map[string]*ProjectUsage{}}
	use := func(id string) *sessionUse {
		u, ok := t.use[id]
		if !ok {
			u = &sessionUse{id: id}
			t.use[id] = u
		}
		return u
	}
	for _, r := range d.usage {
		u := use(r.SessionID)
		m := minutesOf(r)
		u.minutes += m
		u.tokens = addTokens(u.tokens, r.Tokens())
		u.rows = append(u.rows, r)
		t.minutes += m
	}
	for _, e := range d.events {
		if credits(e) {
			use(e.SessionID).outcome = true
		}
	}
	for _, id := range d.started {
		use(id).started = true
	}
	for _, id := range d.updated {
		use(id)
	}
	for _, c := range d.commits {
		for _, id := range []string{c.RecordedBy, sessionOwner(c)} {
			if u, ok := t.use[id]; ok {
				u.outcome = true
			}
		}
	}
	day := dayKey(d.lo, s.loc)
	in := func(at time.Time) bool { return !at.Before(d.lo) && at.Before(d.hi) }
	for id, u := range t.use {
		ss, known := s.sessions[id]
		if !known {
			continue
		}
		u.ended = ss.Status != core.SessionActive && in(ss.Updated)
		if u.ended && explicitFindings(ss.Findings) {
			u.outcome = true
		}
		if ss.Totals.Total == 0 {
			u.unreported = true
		} else if !ss.Ledger {
			u.approximate, u.endedOn = true, dayKey(ss.Updated, s.loc)
			if u.endedOn == day {
				u.counted = true
				u.tokens = addTokens(u.tokens, ss.Totals)
				t.approx = true
			}
		}
		if ss.Project != "" {
			t.projects[ss.Project] = true
		}
	}
	t.sessions = slices.Sorted(maps.Keys(t.use))
	for _, id := range t.sessions {
		u := t.use[id]
		t.tokens = addTokens(t.tokens, u.tokens)
		ss := s.sessions[id]
		u.minor = minor(u.outcome, ss.Verdict != "", u.minutes, u.tokens.Output)
		if u.minor {
			t.short++
		} else if ss.Project != "" {
			t.working[ss.Project] = true
		}
		pl := placeOf(ss.Project, ss.CWD)
		pu, ok := t.byPlace[pl.key]
		if !ok {
			pu = &ProjectUsage{Project: pl.project, Label: pl.label}
			t.byPlace[pl.key] = pu
		}
		pu.Minutes += u.minutes
		pu.Tokens = addTokens(pu.Tokens, u.tokens)
		pu.Sessions++
	}
	t.countWork(s, d)
	return t
}

// countWork tallies the day's tasks, items and plans.
func (t *tally) countWork(s *span, d dayRows) {
	last := map[string]string{} // task -> the status its last transition moved it to
	closing := map[string]bool{}
	created := map[string]bool{}
	kept := map[keptKey]bool{}
	drafted := map[store.PlanRef]bool{}
	plans := map[store.PlanRef]bool{}
	for _, e := range d.events {
		switch e.Kind {
		case core.EventTaskTransition:
			if e.ItemID == "" {
				continue
			}
			if e.To != "" {
				last[e.ItemID] = e.To
			}
			if e.To == string(core.TaskDone) && e.From != string(core.TaskDone) {
				closing[e.ItemID] = true
			}
			if e.Created {
				created[e.ItemID] = true
			}
			project := e.Project
			if task, ok := s.tasks[e.ItemID]; ok {
				project = task.Project
			}
			if project != "" {
				t.projects[project] = true
				t.working[project] = true
			}
			if p, ok := s.planOf(e); ok && e.To == string(core.TaskDone) {
				plans[p] = true
			}
		case core.EventMemoryWritten, core.EventTrialRecorded:
			if e.ItemID != "" {
				kept[keptKey{e.Kind, e.ItemID}] = true
			}
		case core.EventNoteWritten:
			if e.ItemID != "" && !e.Deleted {
				kept[keptKey{e.Kind, e.ItemID}] = true
			}
		case core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved:
			if e.PlanSlug != "" {
				drafted[store.PlanRef{Project: e.Project, Slug: e.PlanSlug}] = true
			}
		}
	}
	for id := range closing {
		if last[id] == string(core.TaskDone) {
			t.closed = append(t.closed, id)
		}
	}
	slices.Sort(t.closed)
	t.created = slices.Sorted(maps.Keys(created))
	t.kept = slices.SortedFunc(maps.Keys(kept), func(a, b keptKey) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.id, b.id))
	})
	t.drafted = slices.SortedFunc(maps.Keys(drafted), comparePlans)
	if s.history != nil {
		for _, p := range slices.SortedFunc(maps.Keys(plans), comparePlans) {
			if s.shippedOn(p, d.events, d.lo) {
				t.shipped = append(t.shipped, p)
			}
		}
	}
}

// places is the day's usage by place, most agent time first, then most
// tokens, most sessions, and label.
func (t *tally) places() []ProjectUsage {
	out := make([]ProjectUsage, 0, len(t.byPlace))
	for _, pu := range t.byPlace {
		out = append(out, *pu)
	}
	slices.SortFunc(out, func(a, b ProjectUsage) int {
		return cmp.Or(cmp.Compare(b.Minutes, a.Minutes), cmp.Compare(b.Tokens.Total, a.Tokens.Total),
			cmp.Compare(b.Sessions, a.Sessions), cmp.Compare(a.Label, b.Label), cmp.Compare(a.Project, b.Project))
	})
	return out
}

// facts is the tally as Facts.
func (t *tally) facts() Facts {
	f := Facts{
		Minutes: t.minutes, Tokens: t.tokens.Total, Output: t.tokens.Output,
		Sessions: len(t.sessions), Short: t.short, Projects: len(t.projects), WorkProjects: len(t.working),
		TasksClosed: len(t.closed), TasksCreated: len(t.created),
		PlansShipped: len(t.shipped), PlansDrafted: len(t.drafted),
		Kept: len(t.kept), Approximate: t.approx,
	}
	for _, u := range t.use {
		if u.unreported {
			f.Unreported++
		}
	}
	if places := t.places(); len(places) > 0 && t.minutes > 0 {
		f.TopShare = places[0].Minutes * 100 / t.minutes
	}
	return f
}

// addTokens is a plus b, field by field, its total the sum of its parts.
func addTokens(a, b core.TokenUsage) core.TokenUsage {
	out := core.TokenUsage{
		Input: a.Input + b.Input, Cached: a.Cached + b.Cached,
		CacheCreation: a.CacheCreation + b.CacheCreation, Output: a.Output + b.Output,
	}
	out.Normalize()
	return out
}
