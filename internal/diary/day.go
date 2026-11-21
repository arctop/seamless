package diary

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Day returns the entry of the local day holding day, in the daemon's zone,
// read through db. A day that has not begun is ErrFutureDay.
func Day(ctx context.Context, db *sql.DB, day time.Time) (Entry, error) {
	return dayAt(ctx, db, day, time.Now(), time.Local)
}

// dayAt is Day at now, in loc.
func dayAt(ctx context.Context, db *sql.DB, day, now time.Time, loc *time.Location) (Entry, error) {
	lo, hi := dayBounds(day, loc)
	if lo.After(now) {
		return Entry{}, ErrFutureDay
	}
	s, err := load(ctx, db, lo, hi, loc, true)
	if err != nil {
		return Entry{}, err
	}
	d := s.split()[0]
	b := &builder{s: s, d: d, t: s.count(d), hours: hourStarts(lo, hi)}
	if err := b.resolve(ctx, db); err != nil {
		return Entry{}, err
	}
	e := Entry{
		Day: dayKey(lo, loc), Start: lo, End: hi, Hours: len(b.hours),
		Today:           !now.Before(lo) && now.Before(hi),
		SessionsStarted: len(d.started),
	}
	e.Facts = b.t.facts()
	b.sessionDays()
	threads := b.threads()
	spot, also := spotlight(threads)
	e.Spotlight, e.Also = derefThreads(spot), derefThreads(also)
	e.Short = b.short()
	e.Usage = b.usage()
	e.Kept = b.kept()
	e.Moments = b.moments()
	e.Projects = b.projectRefs()
	e.Character = Character(e.Facts)
	e.Headline = Headline(e)
	return e, nil
}

// builder composes one day's entry from its span, tally and resolved items.
type builder struct {
	s     *span
	d     dayRows
	t     *tally
	hours []time.Time

	// The items the day names, resolved through the handle: one batched read
	// per kind.
	sessions  map[string]core.Session
	memories  map[string]core.Memory
	notes     map[string]core.Note
	trials    map[string]core.Trial
	projects  map[string]core.Project
	planNotes map[string][]core.Note
	prompts   map[string]string
	commits   []store.DiaryCommit

	// days is the session-days by session id; member each one's threads.
	days   map[string]*SessionDay
	member map[string][]string
	named  map[string]bool // every project slug the entry names
}

// resolve reads every item the day names.
func (b *builder) resolve(ctx context.Context, db *sql.DB) error {
	var err error
	b.commits = b.d.commits
	sessionIDs := slices.Clone(b.t.sessions)
	var memoryIDs, noteIDs, trialIDs, extraTasks []string
	for _, e := range b.d.events {
		if e.SessionID != "" {
			sessionIDs = append(sessionIDs, e.SessionID)
		}
		switch e.Kind {
		case core.EventMemoryWritten:
			memoryIDs = append(memoryIDs, e.ItemID)
		case core.EventNoteWritten:
			noteIDs = append(noteIDs, e.ItemID)
		case core.EventTrialRecorded:
			trialIDs = append(trialIDs, e.ItemID)
		}
	}
	for _, c := range b.commits {
		switch c.Owner {
		case store.CommitOwnerSession:
			sessionIDs = append(sessionIDs, c.OwnerID)
		case store.CommitOwnerTask:
			if _, ok := b.s.tasks[c.OwnerID]; !ok {
				extraTasks = append(extraTasks, c.OwnerID)
			}
		}
		if c.RecordedBy != "" {
			sessionIDs = append(sessionIDs, c.RecordedBy)
		}
	}
	if b.sessions, err = store.SessionsByIDs(ctx, db, diaryIDs(sessionIDs)); err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	if len(extraTasks) > 0 {
		more, err := store.DiaryTasksByIDs(ctx, db, extraTasks)
		if err != nil {
			return fmt.Errorf("diary: %w", err)
		}
		maps.Copy(b.s.tasks, more)
	}
	if b.memories, err = store.MemoriesByIDs(ctx, db, diaryIDs(memoryIDs)); err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	if b.notes, err = store.NotesByIDs(ctx, db, diaryIDs(noteIDs)); err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	if b.trials, err = store.TrialsByIDs(ctx, db, diaryIDs(trialIDs)); err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	projects, err := store.ListProjects(ctx, db)
	if err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	b.projects = make(map[string]core.Project, len(projects))
	for _, p := range projects {
		b.projects[p.Slug] = p
	}
	var slugs []string
	for p := range b.s.steps {
		slugs = append(slugs, p.Slug)
	}
	for _, e := range b.d.events {
		if e.PlanSlug != "" {
			slugs = append(slugs, e.PlanSlug)
		}
	}
	if b.planNotes, err = store.DiaryPlanNotes(ctx, db, slugs); err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	// Only a session with no findings of its own needs its first prompt.
	var asking []string
	for _, id := range b.t.sessions {
		if !explicitFindings(b.s.sessions[id].Findings) {
			asking = append(asking, id)
		}
	}
	if b.prompts, err = store.DiaryFirstPrompts(ctx, db, b.d.lo, b.d.hi, asking); err != nil {
		return fmt.Errorf("diary: %w", err)
	}
	return nil
}

// diaryIDs is ids without blanks and repeats, sorted.
func diaryIDs(ids []string) []string {
	out := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

// Client discriminators as the hooks persist them (the console's
// harnessOf reads them the same way).
const (
	harnessClaudeCode = "claude-code"
	harnessCodex      = "codex"
)

// harnessOf is a session's client: its recorded ExternalClient, else its
// ambient name's prefix (cc/, cx/); "" for an explicit session that names
// none.
func harnessOf(s core.Session) string {
	switch {
	case s.ExternalClient != "":
		return s.ExternalClient
	case strings.HasPrefix(s.Name, "cc/"):
		return harnessClaudeCode
	case strings.HasPrefix(s.Name, "cx/"):
		return harnessCodex
	}
	return ""
}

// in reports whether t falls on the day.
func (b *builder) in(t time.Time) bool { return !t.Before(b.d.lo) && t.Before(b.d.hi) }

// sessionRef is a session's Ref.
func (b *builder) sessionRef(id string) Ref {
	s, ok := b.sessions[id]
	if !ok {
		return Ref{Kind: RefSession, ID: id}
	}
	b.name(s.ProjectSlug)
	return Ref{Kind: RefSession, ID: id, Title: s.Name, Project: s.ProjectSlug, Resolved: true, Status: string(s.Status)}
}

// taskRef is a task's Ref. For a task the handle no longer holds, project and
// title are what the event that named it recorded: where it was recorded,
// and a title only if it carried one (no task.transition does today).
func (b *builder) taskRef(id, project, title string) Ref {
	t, ok := b.s.tasks[id]
	if !ok {
		b.name(project)
		return Ref{Kind: RefTask, ID: id, Title: title, Project: project}
	}
	b.name(t.Project)
	return Ref{Kind: RefTask, ID: id, Title: t.Title, Project: t.Project, Resolved: true, Status: string(t.Status)}
}

// itemRef is the Ref of the item a write event names, resolved, or kept with
// what the event recorded.
func (b *builder) itemRef(e store.DiaryEvent) Ref {
	switch e.Kind {
	case core.EventMemoryWritten:
		if m, ok := b.memories[e.ItemID]; ok {
			b.name(m.Project)
			status := MemoryActive
			switch {
			case m.InvalidAt != nil && m.SupersededBy != "":
				status = MemorySuperseded
			case m.InvalidAt != nil:
				status = MemoryArchived
			}
			return Ref{Kind: RefMemory, ID: m.ID, Title: m.Name, Project: m.Project, Resolved: true, Status: status}
		}
		b.name(e.Project)
		return Ref{Kind: RefMemory, ID: e.ItemID, Title: e.Name, Project: e.Project}
	case core.EventNoteWritten:
		if n, ok := b.notes[e.ItemID]; ok {
			b.name(n.Project)
			return Ref{Kind: RefNote, ID: n.ID, Title: cmp.Or(n.Title, n.Slug), Project: n.Project, Resolved: true}
		}
		b.name(e.Project)
		return Ref{Kind: RefNote, ID: e.ItemID, Title: cmp.Or(e.Title, e.Slug), Project: e.Project}
	case core.EventTrialRecorded:
		if tr, ok := b.trials[e.ItemID]; ok {
			b.name(tr.ProjectSlug)
			return Ref{Kind: RefTrial, ID: tr.ID, Title: cmp.Or(tr.Title, tr.Lab), Project: tr.ProjectSlug, Resolved: true,
				Status: string(tr.Outcome)}
		}
		b.name(e.Project)
		return Ref{Kind: RefTrial, ID: e.ItemID, Title: cmp.Or(e.Title, e.Lab), Project: e.Project}
	}
	return Ref{}
}

// planRef is a plan's Ref in its project.
func (b *builder) planRef(p store.PlanRef) Ref {
	b.name(p.Project)
	primary, ok := planPrimary(b.planNotes[p.Slug])
	return Ref{Kind: RefPlan, ID: p.Slug, Title: p.Slug, Project: p.Project, Resolved: true,
		HasPage: ok && primary.Project == p.Project}
}

// commitRef is a commit's Ref, with its owner's.
func (b *builder) commitRef(c store.DiaryCommit) Ref {
	var owner Ref
	switch c.Owner {
	case store.CommitOwnerTask:
		owner = b.taskRef(c.OwnerID, "", "")
	default:
		owner = b.sessionRef(c.OwnerID)
	}
	title := c.Commit
	if len(title) > 12 {
		title = title[:12]
	}
	return Ref{Kind: RefCommit, ID: c.Commit, Title: title, Project: owner.Project, Resolved: true, Owner: &owner}
}

// name notes a project slug the entry names.
func (b *builder) name(project string) {
	if project == "" {
		return
	}
	if b.named == nil {
		b.named = map[string]bool{}
	}
	b.named[project] = true
}

// sessionDays builds every session-day of the day, judged by the tally (its
// outcome and the minor test are the tally's, which Month counts with too).
func (b *builder) sessionDays() {
	b.days = make(map[string]*SessionDay, len(b.t.sessions))
	n := len(b.hours)
	for _, id := range b.t.sessions {
		u := b.t.use[id]
		sess, ok := b.sessions[id]
		known := b.s.sessions[id]
		sd := &SessionDay{
			Session: b.sessionRef(id), Harness: harnessOf(sess), Model: sess.Model,
			Minutes: u.minutes, Tokens: u.tokens, Hours: make([]int, n),
			Unreported: u.unreported, Approximate: u.approximate,
			Started: u.started, Ended: u.ended, Flag: string(known.Verdict), Minor: u.minor,
		}
		if u.approximate {
			sd.EndedOn = u.endedOn
		}
		for _, r := range u.rows {
			sd.Hours[hourOf(r.Bucket, b.d.lo, n)] += minutesOf(r)
		}
		label := placeOf(known.Project, known.CWD).label
		if ok {
			label = placeOf(sess.ProjectSlug, sess.CWD).label
		}
		sd.About, sd.AboutFrom = about(known.Findings, b.prompts[id], label)
		b.days[id] = sd
	}
}

// threadOfTask is the key and shape of the thread a task belongs to: its
// plan's, or its project's.
func (b *builder) threadOfTask(e store.DiaryEvent) (string, *Thread) {
	if p, ok := b.s.planOf(e); ok {
		return planKey(p), &Thread{Key: planKey(p), Kind: ThreadPlan, Label: p.Slug, Project: p.Project}
	}
	project := e.Project
	if t, ok := b.s.tasks[e.ItemID]; ok {
		project = t.Project
	}
	pl := placeOf(project, "")
	return pl.key, &Thread{Key: pl.key, Kind: ThreadProject, Label: pl.label, Project: pl.project}
}

// planKey is a plan thread's key.
func planKey(p store.PlanRef) string { return "plan:" + p.Project + ":" + p.Slug }

// threads builds the day's threads: each task that changed and each plan
// event in its thread, each session in the threads of the work it did, or in
// its own project's when it did none that names one. Minor session-days go to
// no thread.
func (b *builder) threads() []*Thread {
	threads := map[string]*Thread{}
	open := func(key string, shape *Thread) *Thread {
		t, ok := threads[key]
		if !ok {
			t = shape
			threads[key] = t
		}
		return t
	}
	b.member = map[string][]string{}
	join := func(session, key string) {
		if session != "" && !slices.Contains(b.member[session], key) {
			b.member[session] = append(b.member[session], key)
		}
	}
	// The day's task changes, by task, in order.
	type taskDay struct {
		key     string
		first   store.DiaryEvent
		last    store.DiaryEvent
		title   string // the last title an event recorded
		claimed bool
		created bool
		by      []string
	}
	tasks := map[string]*taskDay{}
	var order []string
	for _, e := range b.d.events {
		switch e.Kind {
		case core.EventTaskTransition:
			if e.ItemID == "" {
				continue
			}
			key, shape := b.threadOfTask(e)
			open(key, shape)
			if e.SessionID != "" && !b.days[e.SessionID].isMinor() {
				join(e.SessionID, key)
			}
			td, ok := tasks[e.ItemID]
			if !ok {
				td = &taskDay{key: key, first: e}
				tasks[e.ItemID] = td
				order = append(order, e.ItemID)
			}
			td.last = e
			td.title = cmp.Or(e.Title, td.title)
			td.claimed = td.claimed || e.ClaimedBy != ""
			td.created = td.created || e.Created
			if e.SessionID != "" && !slices.Contains(td.by, e.SessionID) {
				td.by = append(td.by, e.SessionID)
			}
		case core.EventPlanCaptured, core.EventPlanPresented, core.EventPlanApproved:
			if e.PlanSlug == "" {
				continue
			}
			p := store.PlanRef{Project: e.Project, Slug: e.PlanSlug}
			open(planKey(p), &Thread{Key: planKey(p), Kind: ThreadPlan, Label: p.Slug, Project: p.Project})
			if e.SessionID != "" && !b.days[e.SessionID].isMinor() {
				join(e.SessionID, planKey(p))
			}
		}
	}
	// A session whose work named no thread is in its own project's.
	for _, id := range b.t.sessions {
		sd := b.days[id]
		if sd.Minor || len(b.member[id]) > 0 {
			continue
		}
		sess, ok := b.sessions[id]
		pl := placeOf(sess.ProjectSlug, sess.CWD)
		if !ok {
			known := b.s.sessions[id]
			pl = placeOf(known.Project, known.CWD)
		}
		open(pl.key, &Thread{Key: pl.key, Kind: ThreadProject, Label: pl.label, Project: pl.project})
		join(id, pl.key)
	}
	closed := map[string]bool{}
	for _, id := range b.t.closed {
		closed[id] = true
	}
	shipped := map[string]bool{}
	for _, p := range b.t.shipped {
		shipped[planKey(p)] = true
	}
	for _, id := range order {
		td := tasks[id]
		t := threads[td.key]
		from := ""
		if !td.created {
			if task, ok := b.s.tasks[id]; ok {
				from = statusBefore(task, b.s.history[id], b.d.lo)
			}
			if from == "" {
				from = statusBeforeFirst(store.DiaryTransition{From: td.first.From, Claimed: td.first.ClaimedBy != "",
					Released: td.first.Released})
			}
		}
		slices.Sort(td.by)
		if td.by == nil {
			td.by = []string{}
		}
		t.Moved = append(t.Moved, Move{
			Task: b.taskRef(id, td.last.Project, td.title), From: from, To: cmp.Or(td.last.To, from), At: td.last.At,
			Created: td.created, Claimed: td.claimed, Closed: closed[id], Sessions: td.by,
		})
	}
	out := make([]*Thread, 0, len(threads))
	for _, key := range slices.Sorted(maps.Keys(threads)) {
		t := threads[key]
		b.name(t.Project)
		slices.SortStableFunc(t.Moved, func(x, y Move) int {
			return cmp.Or(x.At.Compare(y.At), cmp.Compare(x.Task.ID, y.Task.ID))
		})
		if t.Kind == ThreadPlan {
			p := store.PlanRef{Project: t.Project, Slug: t.Label}
			ref := b.planRef(p)
			t.Plan = &ref
			if steps := b.s.steps[p]; len(steps) > 0 {
				t.Progress = &Progress{Start: b.s.progressAt(steps, b.d.lo), End: b.s.progressAt(steps, b.d.hi)}
			}
			t.Shipped = shipped[key]
		}
		b.fill(t)
		out = append(out, t)
	}
	return out
}

// isMinor reports whether a session-day is minor; a session the day does not
// hold (a nil day) is not.
func (sd *SessionDay) isMinor() bool { return sd != nil && sd.Minor }

// fill sums a thread's sessions, gathers what they kept and recorded, and
// scores it.
func (b *builder) fill(t *Thread) {
	t.Hours = make([]int, len(b.hours))
	var f threadFacts
	members := map[string]bool{}
	for id, keys := range b.member {
		if slices.Contains(keys, t.Key) {
			members[id] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(members)) {
		sd := *b.days[id]
		sd.Shared = len(b.member[id]) > 1
		if sd.Shared {
			t.Shared++
		}
		if sd.Unreported {
			t.Unreported++
		}
		t.Minutes += sd.Minutes
		t.Tokens = addTokens(t.Tokens, sd.Tokens)
		for i, m := range sd.Hours {
			t.Hours[i] += m
		}
		if sd.Ended && explicitFindings(b.s.sessions[id].Findings) {
			f.findings++
		}
		if sd.Flag != "" {
			t.Pinned = true
		}
		t.Sessions = append(t.Sessions, sd)
	}
	slices.SortStableFunc(t.Sessions, func(x, y SessionDay) int {
		return cmp.Or(cmp.Compare(y.Minutes, x.Minutes), cmp.Compare(y.Tokens.Total, x.Tokens.Total),
			cmp.Compare(x.Session.ID, y.Session.ID))
	})
	kept := map[keptKey]Ref{}
	for _, e := range b.d.events {
		if !members[e.SessionID] || e.ItemID == "" {
			continue
		}
		switch {
		case e.Kind == core.EventMemoryWritten, e.Kind == core.EventTrialRecorded,
			e.Kind == core.EventNoteWritten && !e.Deleted:
			kept[keptKey{e.Kind, e.ItemID}] = b.itemRef(e)
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(kept), compareKept) {
		t.Kept = append(t.Kept, kept[k])
	}
	moved := map[string]bool{}
	for _, m := range t.Moved {
		moved[m.Task.ID] = true
		if m.Closed {
			f.closed++
		}
		if m.Created || m.Claimed {
			f.started++
		}
	}
	seen := map[string]bool{}
	for _, c := range b.commits {
		mine := false
		switch c.Owner {
		case store.CommitOwnerTask:
			key, _ := b.threadOfTask(store.DiaryEvent{ItemID: c.OwnerID})
			mine = key == t.Key
		case store.CommitOwnerSession:
			mine = members[c.OwnerID]
		}
		if mine && !seen[c.Commit] {
			seen[c.Commit] = true
			t.Commits = append(t.Commits, b.commitRef(c))
		}
	}
	f.shipped, f.kept, f.commits = t.Shipped, len(t.Kept), len(t.Commits)
	f.minutes, f.output, f.pinned = t.Minutes, t.Tokens.Output, t.Pinned
	t.Score, t.Reasons = score(f)
	if t.Moved == nil {
		t.Moved = []Move{}
	}
	if t.Sessions == nil {
		t.Sessions = []SessionDay{}
	}
	if t.Kept == nil {
		t.Kept = []Ref{}
	}
	if t.Commits == nil {
		t.Commits = []Ref{}
	}
	if t.Reasons == nil {
		t.Reasons = []Reason{}
	}
}

// compareKept orders kept items by kind, then id.
func compareKept(a, b keptKey) int {
	return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.id, b.id))
}

// derefThreads copies threads out of their pointers, never nil.
func derefThreads(ts []*Thread) []Thread {
	out := make([]Thread, 0, len(ts))
	for _, t := range ts {
		out = append(out, *t)
	}
	return out
}

// short is the day's minor session-days, most agent time first.
func (b *builder) short() Short {
	out := Short{Sessions: []SessionDay{}}
	for _, id := range b.t.sessions {
		if sd := b.days[id]; sd.Minor {
			out.Sessions = append(out.Sessions, *sd)
			out.Minutes += sd.Minutes
		}
	}
	out.Count = len(out.Sessions)
	slices.SortStableFunc(out.Sessions, func(x, y SessionDay) int {
		return cmp.Or(cmp.Compare(y.Minutes, x.Minutes), cmp.Compare(x.Session.ID, y.Session.ID))
	})
	return out
}

// usage is the day's usage: the ledger's rows with each session counted once,
// and the stored totals of the sessions from before it that ended on the day.
func (b *builder) usage() Usage {
	u := Usage{
		Minutes: b.t.minutes, Tokens: b.t.tokens, Sessions: len(b.t.sessions),
		Unreported: []Ref{}, Approximate: b.t.approx, Projects: b.t.places(),
	}
	type agent struct {
		AgentUsage
		sessions map[string]bool
	}
	agents := map[string]*agent{}
	agentOf := func(harness, model string) *agent {
		key := harness + "/" + model
		a, ok := agents[key]
		if !ok {
			a = &agent{AgentUsage: AgentUsage{Key: key, Harness: harness, Model: model}, sessions: map[string]bool{}}
			agents[key] = a
		}
		return a
	}
	n := len(b.hours)
	type hourAgent struct{ minutes, tokens int }
	byHour := make([]map[string]*hourAgent, n)
	u.Hours = make([]HourUsage, n)
	for i, start := range b.hours {
		u.Hours[i] = HourUsage{Start: start, Label: start.In(b.s.loc).Format("15:04"), Agents: []AgentHour{}}
		byHour[i] = map[string]*hourAgent{}
	}
	for _, r := range b.d.usage {
		a := agentOf(harnessOf(b.sessions[r.SessionID]), r.Model)
		m, tok := minutesOf(r), r.Tokens()
		a.Minutes += m
		a.Tokens = addTokens(a.Tokens, tok)
		a.sessions[r.SessionID] = true
		part := &a.Main
		if r.Thread == core.UsageThreadSubagent {
			part = &a.Subagent
		}
		part.Minutes += m
		part.Tokens += tok.Total
		h := hourOf(r.Bucket, b.d.lo, n)
		u.Hours[h].Minutes += m
		u.Hours[h].Tokens += tok.Total
		ha, ok := byHour[h][a.Key]
		if !ok {
			ha = &hourAgent{}
			byHour[h][a.Key] = ha
		}
		ha.minutes += m
		ha.tokens += tok.Total
	}
	for _, id := range b.t.sessions {
		su := b.t.use[id]
		switch {
		case su.unreported:
			u.Unreported = append(u.Unreported, b.days[id].Session)
		case su.approximate && su.counted:
			sess := b.sessions[id]
			a := agentOf(harnessOf(sess), sess.Model)
			total := b.s.sessions[id].Totals
			a.Tokens = addTokens(a.Tokens, total)
			a.Approximate.Tokens += total.Total
			a.sessions[id] = true
		}
	}
	u.Reported = u.Sessions - len(u.Unreported)
	for _, a := range agents {
		a.Sessions = len(a.sessions)
		u.Agents = append(u.Agents, a.AgentUsage)
	}
	slices.SortFunc(u.Agents, func(x, y AgentUsage) int {
		return cmp.Or(cmp.Compare(y.Minutes, x.Minutes), cmp.Compare(y.Tokens.Total, x.Tokens.Total), cmp.Compare(x.Key, y.Key))
	})
	if u.Agents == nil {
		u.Agents = []AgentUsage{}
	}
	for i := range u.Hours {
		for _, a := range u.Agents {
			if ha, ok := byHour[i][a.Key]; ok {
				u.Hours[i].Agents = append(u.Hours[i].Agents, AgentHour{Key: a.Key, Minutes: ha.minutes, Tokens: ha.tokens})
			}
		}
	}
	for _, p := range u.Projects {
		b.name(p.Project)
	}
	if u.Projects == nil {
		u.Projects = []ProjectUsage{}
	}
	return u
}

// kept is every distinct item written on the day.
func (b *builder) kept() Kept {
	type item struct {
		first, last store.DiaryEvent
		writes      int
		by          []string
	}
	items := map[keptKey]*item{}
	for _, e := range b.d.events {
		switch {
		case e.ItemID == "":
			continue
		case e.Kind == core.EventMemoryWritten, e.Kind == core.EventTrialRecorded,
			e.Kind == core.EventNoteWritten && !e.Deleted:
		default:
			continue
		}
		k := keptKey{e.Kind, e.ItemID}
		it, ok := items[k]
		if !ok {
			it = &item{first: e}
			items[k] = it
		}
		it.last = e
		it.writes++
		if e.SessionID != "" && !slices.Contains(it.by, e.SessionID) {
			it.by = append(it.by, e.SessionID)
		}
	}
	out := Kept{Memories: []KeptItem{}, Notes: []KeptItem{}, Trials: []KeptItem{}}
	for _, k := range slices.SortedFunc(maps.Keys(items), compareKept) {
		it := items[k]
		ref := b.itemRef(it.last)
		slices.Sort(it.by)
		ki := KeptItem{Item: ref, New: b.isNew(k, it.first), Writes: it.writes, At: it.last.At, Sessions: it.by}
		if ki.Sessions == nil {
			ki.Sessions = []string{}
		}
		if ki.New {
			out.New++
		} else {
			out.Updated++
		}
		switch k.kind {
		case core.EventMemoryWritten:
			out.Memories = append(out.Memories, ki)
		case core.EventNoteWritten:
			out.Notes = append(out.Notes, ki)
		case core.EventTrialRecorded:
			out.Trials = append(out.Trials, ki)
		}
	}
	return out
}

// isNew reports whether an item written on the day was created on it: its
// creation time, or for an item the handle no longer holds, a first write of
// the day that was no update, append or edit.
func (b *builder) isNew(k keptKey, first store.DiaryEvent) bool {
	switch k.kind {
	case core.EventMemoryWritten:
		if m, ok := b.memories[k.id]; ok {
			return b.in(m.Created)
		}
	case core.EventNoteWritten:
		if n, ok := b.notes[k.id]; ok {
			return b.in(n.Created)
		}
	case core.EventTrialRecorded:
		return true
	}
	return !first.Updated && !first.Appended && !first.Edited
}

// moments is the day's shipped plans, commits, flags and mishaps.
func (b *builder) moments() Moments {
	m := Moments{Shipped: []Ref{}, Commits: []Ref{}, Flags: []Flag{}, Mishaps: []Mishap{}}
	for _, p := range b.t.shipped {
		m.Shipped = append(m.Shipped, b.planRef(p))
	}
	for _, c := range b.commits {
		m.Commits = append(m.Commits, b.commitRef(c))
	}
	for _, e := range b.d.events {
		switch e.Kind {
		case core.EventSessionFlagged:
			if e.SessionID == "" {
				continue
			}
			m.Flags = append(m.Flags, Flag{Session: b.sessionRef(e.SessionID), Verdict: e.Verdict, Reason: e.Reason, At: e.At})
		case core.EventAgentMishap:
			b.name(e.Project)
			typ := core.MishapType(e.MishapType)
			if !typ.Valid() {
				typ = ""
			}
			m.Mishaps = append(m.Mishaps, Mishap{
				Event: Ref{Kind: RefEvent, ID: e.ID, Title: firstLine(core.MishapText(e.Description, typ)),
					Project: e.Project, Resolved: true},
				Session: b.sessionRef(e.SessionID), At: e.At, Type: string(typ),
			})
		}
	}
	return m
}

// projectRefs is every project the entry names, by slug.
func (b *builder) projectRefs() []Ref {
	out := []Ref{}
	for _, slug := range slices.Sorted(maps.Keys(b.named)) {
		p, ok := b.projects[slug]
		out = append(out, Ref{Kind: RefProject, ID: slug, Title: slug, Project: slug, Resolved: ok, Retired: ok && p.Retired()})
	}
	return out
}
