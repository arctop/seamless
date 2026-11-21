// The Diary fixture (plan:diary): three weeks of believable history for the
// console's Diary, laid on the real calendar around the seeding time
// (demoseed -diary). It is a mode of its own: the console fleet seed and the
// -scenes fixture share none of its code, data or RNG, so neither moves.
//
// The story (diary_story.go) is replayed in time order through the store,
// files, lifecycle and events calls a real day goes through: the hooks create
// a session's ambient row and record its prompts, its tool calls claim and
// close tasks and write memories, notes and trials, its end records findings,
// commits, a flag or a mishap, and its harvest writes the usage ledger. Each
// call takes the story's backdated time, so the database holds what those
// days would have left, the event log included. Three things have no store
// API and are written by the named primitives at the end of this file: the
// day older than the ledger (token totals with no ledger rows), the task
// deleted after it closed, and the flagged session's transcript copy.
//
// Deterministic apart from the anchor: the calendar follows the anchor's
// weekday, and each session's usage comes from an RNG seeded by its key, so a
// session's ledger rows are the same whatever day it lands on.

package demokit

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// DiaryLedgerDays is how many days the fixture's usage ledger covers, today
// included: three weeks.
const DiaryLedgerDays = 21

// diarySeed seeds the RNG behind the fixture's ids, external session ids and
// commit ids. Each session's usage has an RNG of its own (diaryUsageRNG).
const diarySeed = 20261120

// diaryTodaySpan is how many minutes before the anchor today's script starts
// at most: its earliest session began this long ago. An anchor closer to
// midnight squeezes today's script into the time since midnight.
const diaryTodaySpan = 195

// DiaryRole is what a covered day holds.
type DiaryRole string

// The roles of the covered days.
const (
	DiaryToday        DiaryRole = "today"          // the anchor's day: finished work and two live agents
	DiaryWorkday      DiaryRole = "workday"        // a weekday of plan steps, loose tasks and questions
	DiaryShortDay     DiaryRole = "short-sessions" // a weekday of short questions only
	DiaryQuietWeekend DiaryRole = "quiet-weekend"  // the newest whole weekend: no session at all
	DiaryWeekend      DiaryRole = "weekend"        // another weekend day, quiet too
	DiaryLightWeekend DiaryRole = "light-weekend"  // the one weekend day with a short question
	DiaryPreLedger    DiaryRole = "pre-ledger"     // older than the ledger: token totals, no ledger rows
)

// DiaryFixture is what SeedDiary seeded. The database is the record; this is
// its map, for the demoseed summary and the tests.
type DiaryFixture struct {
	// Zone is the time zone the days are local days of.
	Zone *time.Location
	// Anchor is the seeding time the calendar is laid around.
	Anchor time.Time
	// Days is every covered day, newest first: the ledger's DiaryLedgerDays
	// days ending today, then the day older than the ledger.
	Days []DiaryDay
	// Links names the item behind each case the Diary's links must handle.
	Links DiaryLinks
	// Sessions is every story session's id, by its key in the story.
	Sessions map[string]string
}

// DiaryDay is one covered day.
type DiaryDay struct {
	// Date is the day's local midnight.
	Date time.Time
	Role DiaryRole
	// Sessions are the ids of the sessions that started on the day.
	Sessions []string
	// LedgerRows is how many usage-ledger rows the day's quarter-hours hold.
	LedgerRows int
}

// DiaryLinks names the item behind each link case.
type DiaryLinks struct {
	// DeletedTask is a task that closed and whose row is gone; the
	// task.transition events naming it stay.
	DeletedTask string
	// TaskOnlyPlan is a plan with steps and no note carrying its tag, in
	// TaskOnlyPlanProject.
	TaskOnlyPlan, TaskOnlyPlanProject string
	// SharedSlug is a plan slug two projects hold steps of; the note carrying
	// its tag is in SharedSlugProjects[0], and SharedSlugProjects[1] has
	// steps only.
	SharedSlug         string
	SharedSlugProjects [2]string
	// ShippedPlan is the plan whose last step closed done, in
	// ShippedPlanProject.
	ShippedPlan, ShippedPlanProject string
	// SupersededMemory was superseded by SupersedingMemory.
	SupersededMemory, SupersedingMemory string
	// GlobalSession is a session in the global scope.
	GlobalSession string
	// CommitTask recorded commits when it closed; CommitSession recorded
	// commits at its session_end.
	CommitTask, CommitSession string
	// FlaggedSession is flagged, with its transcript copy kept.
	FlaggedSession string
	// MidnightSession started on one day and ended on the next.
	MidnightSession string
	// Mishap is the agent.mishap event a session reported.
	Mishap string
	// Trial is a recorded research trial.
	Trial string
	// UnreportedSession came from a client without hooks: no usage, so its
	// tokens are unreported.
	UnreportedSession string
}

// SeedDiary seeds the Diary fixture into the throwaway data dir, its days
// local days of loc around the Seeder's anchor (Now). It returns the map of
// what it seeded; any failure stops the seed and is returned.
func (s *Seeder) SeedDiary(loc *time.Location) (DiaryFixture, error) {
	if loc == nil {
		return DiaryFixture{}, errors.New("demokit: diary: no time zone")
	}
	d := &diarySeeder{
		s: s, ctx: s.ctx, rng: rand.New(rand.NewSource(diarySeed)),
		anchor: s.now.In(loc), loc: loc,
		tasks: map[string]*diaryTask{}, mems: map[string]*diaryMemory{}, notes: map[string]core.Note{},
		iters: map[string]int{}, trials: map[string]string{}, runs: map[string]*diaryRun{},
	}
	for _, t := range diaryTasks {
		d.tasks[t.key] = &diaryTask{spec: t}
	}
	for _, m := range diaryMemories {
		d.mems[m.key] = &diaryMemory{spec: m}
	}
	if err := d.seed(); err != nil {
		return DiaryFixture{}, fmt.Errorf("demokit: diary: %w", err)
	}
	return d.fx, nil
}

// diarySeeder is one SeedDiary run.
type diarySeeder struct {
	s      *Seeder
	ctx    context.Context
	rng    *rand.Rand
	anchor time.Time
	loc    *time.Location
	cal    diaryCalendar

	ops    []diaryOp
	opAt   time.Time // the running op's time
	opTick int       // how many times the running op asked the clock

	runs   map[string]*diaryRun
	tasks  map[string]*diaryTask
	mems   map[string]*diaryMemory
	memLog []string // memory keys in the order they were written
	notes  map[string]core.Note
	iters  map[string]int // a captured plan note's iteration
	trials map[string]string
	fx     DiaryFixture
}

// diaryTask is a story task and, once created, its id and status.
type diaryTask struct {
	spec   dTask
	id     string
	status core.TaskStatus
}

// diaryMemory is a story memory and, once written, the memory and when it was
// superseded.
type diaryMemory struct {
	spec       dMemory
	mem        core.Memory
	written    bool
	superseded *time.Time
}

// diaryOp is one step of the replay: run at at, ties in the order scheduled.
type diaryOp struct {
	at  time.Time
	seq int
	run func() error
}

func (d *diarySeeder) seed() error {
	cal, err := newDiaryCalendar(d.anchor, d.loc)
	if err != nil {
		return err
	}
	d.cal = cal
	d.fx = DiaryFixture{Zone: d.loc, Anchor: d.anchor}
	if err := d.projects(); err != nil {
		return err
	}
	if err := d.place(); err != nil {
		return err
	}
	if err := d.replay(); err != nil {
		return err
	}
	if err := d.removeDeletedTask(); err != nil {
		return err
	}
	return d.countLedger()
}

// ---------------------------------------------------------------------------
// the calendar
// ---------------------------------------------------------------------------

// diaryCalendar lays the story on the days around the anchor.
type diaryCalendar struct {
	days      []time.Time  // the ledger's days, newest first: days[0] is today
	workdays  []time.Time  // the weekdays among days[1:], newest first
	quiet     [2]time.Time // the newest whole weekend inside days[1:]: Saturday, Sunday
	light     time.Time    // the newest weekend day older than the quiet weekend
	preLedger time.Time    // the newest weekday older than the ledger's first day
	midnight  time.Time    // the weekday (Monday to Thursday) the midnight session starts on
}

func newDiaryCalendar(anchor time.Time, loc *time.Location) (diaryCalendar, error) {
	today := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, loc)
	back := func(n int) time.Time { return time.Date(today.Year(), today.Month(), today.Day()-n, 0, 0, 0, 0, loc) }
	var c diaryCalendar
	for i := range DiaryLedgerDays {
		c.days = append(c.days, back(i))
	}
	quietAt := -1
	for i := 1; i+1 < len(c.days); i++ {
		if c.days[i].Weekday() == time.Sunday && c.days[i+1].Weekday() == time.Saturday {
			c.quiet, quietAt = [2]time.Time{c.days[i+1], c.days[i]}, i+1
			break
		}
	}
	if quietAt < 0 {
		return c, errors.New("no whole weekend in the window")
	}
	for i := quietAt + 1; i < len(c.days); i++ {
		if weekend(c.days[i]) {
			c.light = c.days[i]
			break
		}
	}
	if c.light.IsZero() {
		return c, errors.New("no weekend day older than the quiet weekend")
	}
	for _, day := range c.days[1:] {
		if !weekend(day) {
			c.workdays = append(c.workdays, day)
		}
	}
	if n := len(c.workdays); n != len(diaryWorkdays) && n != len(diaryWorkdays)-1 {
		return c, fmt.Errorf("%d weekdays in the window, want %d or %d", n, len(diaryWorkdays)-1, len(diaryWorkdays))
	}
	for n := DiaryLedgerDays; c.preLedger.IsZero(); n++ {
		if day := back(n); !weekend(day) {
			c.preLedger = day
		}
	}
	for _, day := range c.workdays[1:] {
		if wd := day.Weekday(); wd >= time.Monday && wd <= time.Thursday {
			c.midnight = day
			break
		}
	}
	return c, nil
}

func weekend(day time.Time) bool {
	return day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
}

// script is the weekday script index the calendar lays on workdays[k]: the
// newest weekday takes the last script.
func (c diaryCalendar) script(k int) int { return len(diaryWorkdays) - 1 - k }

// role is a covered day's role.
func (c diaryCalendar) role(day time.Time) DiaryRole {
	switch {
	case day.Equal(c.days[0]):
		return DiaryToday
	case day.Equal(c.preLedger):
		return DiaryPreLedger
	case day.Equal(c.quiet[0]) || day.Equal(c.quiet[1]):
		return DiaryQuietWeekend
	case day.Equal(c.light):
		return DiaryLightWeekend
	case weekend(day):
		return DiaryWeekend
	}
	for k, wd := range c.workdays {
		if wd.Equal(day) && c.script(k) == diaryShortDay {
			return DiaryShortDay
		}
	}
	return DiaryWorkday
}

// clock is the local time hh:mm on day.
func clock(day time.Time, hhmm string) (time.Time, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, fmt.Errorf("start %q: %w", hhmm, err)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, day.Location()), nil
}

// ---------------------------------------------------------------------------
// projects and placement
// ---------------------------------------------------------------------------

func (d *diarySeeder) projects() error {
	created := d.cal.preLedger.AddDate(0, 0, -40).Add(10 * time.Hour)
	for _, p := range diaryProjects {
		id, err := d.id(created)
		if err != nil {
			return err
		}
		if err := store.CreateProject(d.ctx, d.s.db, core.Project{
			ID: id, Slug: p.slug, Name: p.name, Description: p.desc, CreatedAt: created, UpdatedAt: created,
		}); err != nil {
			return fmt.Errorf("project %s: %w", p.slug, err)
		}
	}
	return nil
}

// diaryRun is one placed session.
type diaryRun struct {
	spec  dSession
	id    string
	name  string
	ext   string // the client's own session id; "" for a client without hooks
	day   time.Time
	start time.Time
	last  time.Time // its end, or its last activity (reaped, live)
	pre   bool      // on the day older than the ledger
}

func (r *diaryRun) at(m int) time.Time { return r.start.Add(time.Duration(m) * time.Minute) }

// cwd is the directory the session ran in.
func (r *diaryRun) cwd() string {
	if r.spec.project == "" {
		return "/home/dev/scratch"
	}
	return "/home/dev/" + r.spec.project
}

// work is the session's main-thread activity: its spans, or one over the
// whole session.
func (r *diaryRun) work() []dSpan {
	if len(r.spec.work) > 0 {
		return r.spec.work
	}
	return []dSpan{{at: 0, mins: r.spec.mins}}
}

// place lays every session of the story on its day, mints its identity and
// schedules its replay.
func (d *diarySeeder) place() error {
	type placed struct {
		day   time.Time
		specs []dSession
		pre   bool
	}
	var all []placed
	all = append(all, placed{day: d.cal.preLedger, specs: diaryPreLedger, pre: true})
	for k := len(d.cal.workdays) - 1; k >= 0; k-- {
		all = append(all, placed{day: d.cal.workdays[k], specs: diaryWorkdays[d.cal.script(k)]})
	}
	all = append(all,
		placed{day: d.cal.midnight, specs: []dSession{diaryMidnight}},
		placed{day: d.cal.light, specs: []dSession{diaryLightWeekend}})
	for _, p := range all {
		for _, spec := range p.specs {
			start, err := clock(p.day, spec.at)
			if err != nil {
				return fmt.Errorf("session %s: %w", spec.key, err)
			}
			if err := d.schedule(spec, p.day, start, p.pre); err != nil {
				return err
			}
		}
	}
	today := d.cal.days[0]
	scale := todayScale(d.anchor.Sub(today))
	for i, spec := range diaryToday {
		spec = scaleSession(spec, scale)
		start := d.anchor.Add(-time.Duration(spec.ago) * time.Minute)
		if !start.After(today) {
			start = today.Add(time.Duration(i+1) * time.Minute)
		}
		if spec.end == endLive {
			spec.mins = int(d.anchor.Sub(start) / time.Minute)
		}
		if err := d.schedule(spec, today, start, false); err != nil {
			return err
		}
	}
	d.fx.Days = make([]DiaryDay, 0, len(d.cal.days)+1)
	for _, day := range append(slices.Clone(d.cal.days), d.cal.preLedger) {
		d.fx.Days = append(d.fx.Days, DiaryDay{Date: day, Role: d.cal.role(day)})
	}
	d.fx.Sessions = make(map[string]string, len(d.runs))
	for _, spec := range d.allSpecs() {
		r := d.runs[spec.key]
		d.fx.Sessions[spec.key] = r.id
		for i := range d.fx.Days {
			if d.fx.Days[i].Date.Equal(r.day) {
				d.fx.Days[i].Sessions = append(d.fx.Days[i].Sessions, r.id)
			}
		}
	}
	return nil
}

// allSpecs is every session of the story in placement order (the order
// place schedules them in).
func (d *diarySeeder) allSpecs() []dSession {
	out := slices.Clone(diaryPreLedger)
	for k := len(d.cal.workdays) - 1; k >= 0; k-- {
		out = append(out, diaryWorkdays[d.cal.script(k)]...)
	}
	out = append(out, diaryMidnight, diaryLightWeekend)
	return append(out, diaryToday...)
}

// todayScale is the factor today's script is squeezed by when the anchor is
// less than diaryTodaySpan minutes after midnight: 1 otherwise.
func todayScale(sinceMidnight time.Duration) float64 {
	avail := sinceMidnight.Minutes() - 5
	if avail >= diaryTodaySpan {
		return 1
	}
	return math.Max(avail, 1) / diaryTodaySpan
}

// scaleSession is spec with every minute value scaled by f: offsets round
// down and lengths up, so an act that came before the end still does
// (floor(a*f) < ceil(b*f) whenever a < b).
func scaleSession(spec dSession, f float64) dSession {
	if f == 1 {
		return spec
	}
	at := func(v int) int { return int(math.Floor(float64(v) * f)) }
	span := func(v int) int { return max(int(math.Ceil(float64(v)*f)), 1) }
	spec.ago, spec.mins = span(spec.ago), span(spec.mins)
	spans := func(in []dSpan) []dSpan {
		out := slices.Clone(in)
		for i := range out {
			out[i].at, out[i].mins = at(out[i].at), span(out[i].mins)
		}
		return out
	}
	spec.work, spec.subs = spans(spec.work), spans(spec.subs)
	spec.prompts = slices.Clone(spec.prompts)
	for i := range spec.prompts {
		spec.prompts[i].at = at(spec.prompts[i].at)
	}
	spec.acts = slices.Clone(spec.acts)
	for i := range spec.acts {
		spec.acts[i].at = at(spec.acts[i].at)
	}
	return spec
}

// schedule mints a session's identity and queues every step of its replay.
func (d *diarySeeder) schedule(spec dSession, day, start time.Time, pre bool) error {
	if _, dup := d.runs[spec.key]; dup {
		return fmt.Errorf("session %s placed twice", spec.key)
	}
	id, err := d.id(start)
	if err != nil {
		return err
	}
	r := &diaryRun{spec: spec, id: id, day: day, start: start, last: start.Add(time.Duration(spec.mins) * time.Minute), pre: pre}
	switch spec.client {
	case clientClaude, clientCodex:
		r.ext = d.uuid()
		r.name = ambientName(spec.client, r.ext)
	case clientNoHooks:
		r.name = "sess/" + strings.ToLower(id[len(id)-8:])
	default:
		return fmt.Errorf("session %s: unknown client %q", spec.key, spec.client)
	}
	if spec.end == endLive {
		r.last = d.anchor
	}
	d.runs[spec.key] = r
	if spec.end == endLive && (pre || !day.Equal(d.cal.days[0])) {
		return fmt.Errorf("session %s: only today's sessions are live", spec.key)
	}
	if spec.brief && (len(spec.acts) > 0 || spec.end == endExplicit || spec.mins >= 10) {
		return fmt.Errorf("session %s: a brief session has no outcome and lasts under 10 minutes", spec.key)
	}

	d.at(start, func() error { return d.startSession(r) })
	// A prompt reaches the hooks a few seconds into its minute (after the
	// SessionStart briefing), an act a few seconds after that.
	for _, p := range spec.prompts {
		d.at(r.at(p.at).Add(5*time.Second), func() error { return d.prompt(r, p) })
	}
	for _, a := range spec.acts {
		if a.at >= spec.mins {
			return fmt.Errorf("session %s: act at minute %d, not before its end (%d)", spec.key, a.at, spec.mins)
		}
		d.at(r.at(a.at).Add(10*time.Second), func() error { return d.act(r, a) })
	}
	if spec.client == clientCodex {
		until := r.last
		if spec.end == endLive {
			until = d.anchor.Add(-time.Minute)
		}
		turns := r.codexTurns(until)
		if len(turns) == 0 && spec.end != endLive {
			// A live one may have had no Stop yet (an anchor just after midnight).
			return fmt.Errorf("session %s: a Codex session with no turn", spec.key)
		}
		for i, tn := range turns {
			lastTurn := i == len(turns)-1
			d.at(tn.at, func() error { return d.codexStop(r, tn, lastTurn) })
		}
		if pre {
			d.at(r.last.Add(30*time.Second), func() error {
				return setPreLedgerTotals(d.ctx, d.s.db, r.id, turns[len(turns)-1].cumulative)
			})
		}
	}
	switch spec.end {
	case endExplicit:
		d.at(r.last, func() error { return d.endExplicit(r) })
		if spec.client == clientClaude {
			d.at(r.last.Add(30*time.Second), func() error { return d.harvestClaude(r, r.last) })
		}
		if spec.flag != nil {
			d.at(r.last.Add(31*time.Second), func() error { return d.keepTranscript(r) })
		}
	case endHook:
		if spec.client != clientClaude {
			return fmt.Errorf("session %s: only Claude Code ends by its SessionEnd hook", spec.key)
		}
		d.at(r.last, func() error { return d.endHook(r) })
		if pre {
			d.at(r.last.Add(30*time.Second), func() error {
				return setPreLedgerTotals(d.ctx, d.s.db, r.id, core.SumUsage(r.claudeRows(r.last)))
			})
		}
	case endReaped:
		reapAt := r.last.Add(core.SessionIdleTTL + time.Duration(5+d.rng.Intn(50))*time.Minute)
		if reapAt.Before(d.anchor) {
			d.at(reapAt, func() error { return d.reap(r) })
		}
	case endLive:
		if spec.client == clientClaude {
			at := d.anchor.Add(-2 * time.Minute)
			d.at(at, func() error { return d.harvestClaude(r, at) })
		}
	default:
		return fmt.Errorf("session %s: no end", spec.key)
	}
	if pre && spec.client == clientClaude && spec.end == endExplicit {
		d.at(r.last.Add(30*time.Second), func() error {
			return setPreLedgerTotals(d.ctx, d.s.db, r.id, core.SumUsage(r.claudeRows(r.last)))
		})
	}
	return nil
}

// at queues fn to run at t.
func (d *diarySeeder) at(t time.Time, fn func() error) {
	d.ops = append(d.ops, diaryOp{at: t, seq: len(d.ops), run: fn})
}

// replay runs the queued steps in time order.
func (d *diarySeeder) replay() error {
	slices.SortStableFunc(d.ops, func(a, b diaryOp) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	for _, op := range d.ops {
		if op.at.After(d.anchor) {
			return fmt.Errorf("a step at %s is after the anchor %s", op.at.Format(time.RFC3339), d.anchor.Format(time.RFC3339))
		}
		d.opAt, d.opTick = op.at, 0
		if err := op.run(); err != nil {
			return err
		}
	}
	return nil
}

// t is the running op's clock: its time, a millisecond later at each ask, so
// the events and writes of one step keep their order.
func (d *diarySeeder) t() time.Time {
	t := d.opAt.Add(time.Duration(d.opTick) * time.Millisecond).UTC()
	d.opTick++
	return t
}

// id mints a ULID at t from the fixture's RNG.
func (d *diarySeeder) id(t time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(t.UTC()), d.rng)
	if err != nil {
		return "", fmt.Errorf("mint ulid: %w", err)
	}
	return id.String(), nil
}

// uuid is a client session id shaped like the ones Claude Code and Codex send.
func (d *diarySeeder) uuid() string {
	return fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x", d.rng.Uint32(), d.rng.Uint32()&0xffff,
		d.rng.Uint32()&0xfff, d.rng.Uint32()&0x3fff|0x8000, d.rng.Uint64()&0xffffffffffff)
}

// commitIDs is n full commit ids.
func (d *diarySeeder) commitIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		b := make([]byte, 20)
		for j := range b {
			b[j] = byte(d.rng.Intn(256))
		}
		out[i] = hex.EncodeToString(b)
	}
	return out
}

// ambientName is the display name the hooks give a new ambient session
// (hooks.ambientName): the client's prefix, the external id's first eight
// characters and 64 bits of its digest.
func ambientName(client, ext string) string {
	prefix := "cc/"
	if client == clientCodex {
		prefix = "cx/"
	}
	digest := sha256.Sum256([]byte(ext))
	return prefix + strings.ToLower(ext[:8]) + "-" + hex.EncodeToString(digest[:8])
}

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

// diaryUsageRNG is the RNG a session's usage is drawn from: seeded by its key
// (FNV-1a), so its ledger rows do not depend on the day it lands on.
func diaryUsageRNG(key string) *rand.Rand {
	h := uint64(14695981039346656037)
	for i := range len(key) {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return rand.New(rand.NewSource(int64(h & math.MaxInt64)))
}

// claudeRows is a Claude Code session's usage until until, as its
// transcript's harvest reads it: one row per quarter-hour, model and thread.
func (r *diaryRun) claudeRows(until time.Time) []core.UsageBucket {
	rng := diaryUsageRNG(r.spec.key)
	type key struct {
		bucket int64
		model  string
		thread core.UsageThread
	}
	acc := map[key]*core.UsageBucket{}
	var order []key
	add := func(t time.Time, model string, thread core.UsageThread, tok core.TokenUsage) {
		b := core.UsageBucketStart(t)
		k := key{b.Unix(), model, thread}
		row, ok := acc[k]
		if !ok {
			row = &core.UsageBucket{Bucket: b, Model: model, Thread: thread}
			acc[k] = row
			order = append(order, k)
		}
		row.Input += tok.Input
		row.Cached += tok.Cached
		row.CacheCreation += tok.CacheCreation
		row.Output += tok.Output
		row.Messages++
		row.MinuteMask |= core.UsageMinuteBit(t)
	}
	idx := 0
	for _, sp := range r.work() {
		pct := cmp.Or(sp.pct, 62)
		if r.spec.brief {
			pct = cmp.Or(sp.pct, 70)
		}
		for m := range sp.mins {
			t := r.at(sp.at + m)
			if !t.Before(until) {
				break
			}
			if rng.Intn(100) >= pct {
				continue
			}
			n := 1 + rng.Intn(2)
			if r.spec.brief {
				n = 1
			}
			for k := range n {
				add(t.Add(time.Duration(5+k*19)*time.Second), r.spec.model, core.UsageThreadMain, mainMessage(rng, idx, r.spec.brief))
				idx++
			}
		}
	}
	for _, sp := range r.spec.subs {
		model := cmp.Or(sp.model, modelFable)
		j := 0
		for m := range sp.mins {
			t := r.at(sp.at + m)
			if !t.Before(until) {
				break
			}
			if rng.Intn(100) >= cmp.Or(sp.pct, 85) {
				continue
			}
			for k := range 1 + rng.Intn(3) {
				add(t.Add(time.Duration(9+k*14)*time.Second), model, core.UsageThreadSubagent, subMessage(rng, j))
				j++
			}
		}
	}
	slices.SortFunc(order, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.bucket, b.bucket), cmp.Compare(a.model, b.model), cmp.Compare(a.thread, b.thread))
	})
	rows := make([]core.UsageBucket, len(order))
	for i, k := range order {
		rows[i] = *acc[k]
	}
	return rows
}

// mainMessage is one assistant message of a session's own thread: the
// conversation's cache read grows with its length.
func mainMessage(rng *rand.Rand, idx int, brief bool) core.TokenUsage {
	cached := 14000 + 900*idx
	if cached > 160000 {
		cached = 150000 + rng.Intn(15000)
	}
	t := core.TokenUsage{
		Input: 1 + rng.Intn(45), Cached: cached * (90 + rng.Intn(21)) / 100,
		CacheCreation: 150 + rng.Intn(2400), Output: 80 + rng.Intn(800),
	}
	switch roll := rng.Intn(100); {
	case brief:
		// A short answer: under 420 output tokens a message.
		t.Output, t.CacheCreation = 60+rng.Intn(360), 100+rng.Intn(900)
	case roll < 7:
		t.Output = 1500 + rng.Intn(3000) // writing code
	case roll < 12:
		t.CacheCreation = 6000 + rng.Intn(12000) // a large file read into the context
	}
	t.Normalize()
	return t
}

// subMessage is one assistant message of a subagent: a fresh context that
// grows as it reads.
func subMessage(rng *rand.Rand, j int) core.TokenUsage {
	created := 120 + rng.Intn(1500)
	if j == 0 {
		created = 4000 + rng.Intn(8000)
	}
	t := core.TokenUsage{
		Input: 2 + rng.Intn(30), Cached: min(80000, 6000+2200*j) * (90 + rng.Intn(21)) / 100,
		CacheCreation: created, Output: 150 + rng.Intn(1200),
	}
	t.Normalize()
	return t
}

// diaryTurn is one Codex turn's Stop: when, and the rollout's cumulative
// counter then.
type diaryTurn struct {
	at         time.Time
	cumulative core.TokenUsage
}

// codexTurns is a Codex session's turns until until, every two to five
// minutes of its activity. A turn makes several model calls, each reading the
// conversation again, so its cache read is a multiple of the context's size.
func (r *diaryRun) codexTurns(until time.Time) []diaryTurn {
	rng := diaryUsageRNG(r.spec.key)
	var (
		cum core.TokenUsage
		out []diaryTurn
		idx int
	)
	for _, sp := range r.work() {
		for m := 1 + rng.Intn(3); m < sp.mins; m += 2 + rng.Intn(4) {
			t := r.at(sp.at + m).Add(25 * time.Second)
			if !t.Before(until) {
				break
			}
			g := core.TokenUsage{
				Input: 1500 + rng.Intn(7500), Cached: min(420000, 40000+9000*idx) * (85 + rng.Intn(31)) / 100,
				Output: 400 + rng.Intn(2600),
			}
			if r.spec.brief {
				// A short answer: one model call, under 620 output tokens.
				g.Cached, g.Output = g.Cached/3, 120+rng.Intn(500)
			}
			cum.Input += g.Input
			cum.Cached += g.Cached
			cum.Output += g.Output
			cum.Normalize()
			out = append(out, diaryTurn{at: t, cumulative: cum})
			idx++
		}
	}
	return out
}

// countLedger fills each covered day's ledger row count.
func (d *diarySeeder) countLedger() error {
	for i := range d.fx.Days {
		day := d.fx.Days[i].Date
		rows, err := store.UsageBetween(d.ctx, d.s.db, day, day.AddDate(0, 0, 1))
		if err != nil {
			return err
		}
		d.fx.Days[i].LedgerRows = len(rows)
	}
	return nil
}

// ---------------------------------------------------------------------------
// the session's life
// ---------------------------------------------------------------------------

func (d *diarySeeder) startSession(r *diaryRun) error {
	t := d.t()
	sess := core.Session{
		ID: r.id, Name: r.name, ProjectSlug: r.spec.project, Status: core.SessionActive,
		CWD: r.cwd(), Model: r.spec.model, CreatedAt: t, UpdatedAt: t,
	}
	hooked := r.spec.client != clientNoHooks
	if hooked {
		// What the SessionStart hook records (hooks.ensureAmbientSessionIn).
		sess.ExternalSessionID, sess.ExternalClient, sess.Source, sess.Ambient = r.ext, r.spec.client, "startup", true
		sess.Metadata = map[string]any{
			"claude_session_id": r.ext, "external_client": r.spec.client,
			"cwd": sess.CWD, "host": "", "source": "startup",
		}
	} else {
		sess.Source = "explicit" // session_start from a client without hooks
	}
	if err := store.CreateSession(d.ctx, d.s.db, sess); err != nil {
		return fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	if !hooked {
		if _, err := d.event(core.EventSessionStarted, r.id, r.spec.project, "", nil); err != nil {
			return err
		}
		return d.toolCall(r, "session_start", map[string]any{"project": r.spec.project, "model": r.spec.model},
			map[string]any{"session_id": r.id, "name": r.name, "project": r.spec.project})
	}
	if _, err := d.event(core.EventSessionStarted, r.id, r.spec.project, "", map[string]any{"ambient": true}); err != nil {
		return err
	}
	content, ids := d.briefing(r, t)
	_, err := d.event(core.EventInjected, r.id, r.spec.project, "", injection("session-start", r, content, ids))
	return err
}

// injection is a hook's retrieval.injected payload (hooks.recordInjection).
func injection(hook string, r *diaryRun, content string, ids []string) map[string]any {
	est := (len(content) + 3) / 4
	return map[string]any{
		"hook": hook, "claude_session_id": r.ext, "external_client": r.spec.client,
		"content": content, "item_ids": ids, "item_ids_exact": true, "original_item_count": len(ids),
		"original_estimated_tokens": est, "emitted_estimated_tokens": est, "truncated": false,
	}
}

// briefing is a short SessionStart briefing for r at t: its scope's active
// memories, constraints first, and its project's plans.
func (d *diarySeeder) briefing(r *diaryRun, t time.Time) (string, []string) {
	var (
		b               strings.Builder
		ids             []string
		rules, others   []string
		memories, binds int
	)
	for _, key := range d.memLog {
		m := d.mems[key]
		if m.superseded != nil && !m.superseded.After(t) {
			continue
		}
		if m.spec.project != r.spec.project && m.spec.project != "" {
			continue
		}
		memories++
		line := "- " + m.spec.key + ": " + m.spec.desc
		if m.spec.kind == core.KindConstraint {
			binds++
			rules = append(rules, line)
		} else {
			others = append(others, line)
		}
		ids = append(ids, m.mem.ID)
	}
	label := r.spec.project
	if label == "" {
		label = "(global)"
	}
	fmt.Fprintf(&b, "<seam-briefing>\nSeam project: %s -- %d memories (%d constraints).\nSeam session: %s\n", label, memories, binds, r.name)
	if len(rules) > 0 {
		b.WriteString("\nConstraints (binding for every session):\n" + strings.Join(rules, "\n") + "\n")
	}
	if len(others) > 0 {
		b.WriteString("\nMemories:\n" + strings.Join(others, "\n") + "\n")
	}
	if lines := d.planLines(r.spec.project); len(lines) > 0 {
		b.WriteString("\n" + strings.Join(lines, "\n") + "\n")
	}
	b.WriteString("</seam-briefing>")
	return b.String(), ids
}

// planLines are a project's plans with their progress, as the replay stands.
func (d *diarySeeder) planLines(project string) []string {
	type progress struct{ done, all int }
	var slugs []string
	seen := map[string]*progress{}
	for _, t := range diaryTasks {
		task := d.tasks[t.key]
		if task.id == "" || t.project != project || t.plan == "" {
			continue
		}
		p, ok := seen[t.plan]
		if !ok {
			p = &progress{}
			seen[t.plan] = p
			slugs = append(slugs, t.plan)
		}
		p.all++
		if task.status.Closed() {
			p.done++
		}
	}
	var out []string
	for _, slug := range slugs {
		if p := seen[slug]; p.done < p.all {
			out = append(out, fmt.Sprintf("PLAN: %s -- %d/%d done", slug, p.done, p.all))
		}
	}
	return out
}

// prompt records a user prompt as the UserPromptSubmit hook does: a
// retrieval.injected event when it matched memories, a hook.prompt when not.
func (d *diarySeeder) prompt(r *diaryRun, p dPrompt) error {
	if r.spec.client == clientNoHooks {
		return nil // no hook saw it
	}
	if len(p.recall) == 0 {
		_, err := d.event(core.EventHookPrompt, r.id, r.spec.project, "", map[string]any{
			"hook": "user-prompt-submit", "claude_session_id": r.ext, "external_client": r.spec.client,
			"prompt": p.text, "matched": false,
		})
		return err
	}
	var (
		lines []string
		ids   []string
	)
	for _, key := range p.recall {
		m, ok := d.mems[key]
		if !ok || !m.written || m.superseded != nil {
			return fmt.Errorf("session %s: prompt recalls %s, which is not an active memory yet", r.spec.key, key)
		}
		lines = append(lines, "- "+m.spec.key+": "+m.spec.desc)
		ids = append(ids, m.mem.ID)
	}
	content := "<seam-recall>\nPossibly relevant memories:\n" + strings.Join(lines, "\n") + "\n</seam-recall>"
	payload := injection("user-prompt-submit", r, content, ids)
	payload["prompt"] = p.text
	_, err := d.event(core.EventInjected, r.id, r.spec.project, "", payload)
	return err
}

// codexStop is one Codex turn's Stop hook: the growth of the cumulative
// counter is credited to the ledger, and the last turn leaves the session's
// harvested findings.
func (d *diarySeeder) codexStop(r *diaryRun, tn diaryTurn, lastTurn bool) error {
	t := d.t()
	if !r.pre {
		if _, err := store.CreditConversationUsage(d.ctx, d.s.db, clientCodex, r.ext, store.UsageCredit{
			Cumulative: tn.cumulative, Model: r.spec.model, At: t, Turn: true,
		}); err != nil {
			return fmt.Errorf("session %s: %w", r.spec.key, err)
		}
	}
	if !lastTurn || r.spec.findings == "" || r.spec.end == endExplicit {
		return nil
	}
	if _, err := store.UpdateAmbientFindings(d.ctx, d.s.db, clientCodex, r.ext, harvest.Prefix+r.spec.findings, t); err != nil {
		return fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	return nil
}

// harvestClaude replaces the session's ledger rows with its transcript's
// usage until until: the SessionEnd harvest, or for a live session the
// mid-session one.
func (d *diarySeeder) harvestClaude(r *diaryRun, until time.Time) error {
	if r.pre {
		return nil
	}
	rows := r.claudeRows(until)
	if _, err := store.ReplaceConversationUsage(d.ctx, d.s.db, clientClaude, r.ext, rows, d.t()); err != nil {
		return fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	return nil
}

// endExplicit is the agent's session_end: its commits, its flag, its findings,
// its claims released, its mishaps and the session.ended event
// (mcp.handleSessionEnd).
func (d *diarySeeder) endExplicit(r *diaryRun) error {
	t := d.t()
	args := map[string]any{"findings": r.spec.findings}
	result := map[string]any{"status": string(core.SessionCompleted), "session_id": r.id}
	if n := r.spec.commits; n > 0 {
		ids := d.commitIDs(n)
		if _, err := store.RecordSessionCommits(d.ctx, d.s.db, r.id, r.id, ids, t); err != nil {
			return fmt.Errorf("session %s: %w", r.spec.key, err)
		}
		args["commits"] = ids
		result["commits_recorded"] = n
	}
	if f := r.spec.flag; f != nil {
		if _, err := store.SetSessionFlag(d.ctx, d.s.db, r.id, f.verdict, f.reason, t); err != nil {
			return fmt.Errorf("session %s: %w", r.spec.key, err)
		}
		payload := map[string]any{"verdict": string(f.verdict), "by": "agent", "retroactive": false}
		if f.reason != "" {
			payload["reason"] = f.reason
		}
		if _, err := d.event(core.EventSessionFlagged, r.id, r.spec.project, "", payload); err != nil {
			return err
		}
		args["verdict"], args["reason"] = string(f.verdict), f.reason
		result["flag"] = map[string]any{"verdict": string(f.verdict), "transcript": "copied at the session's end"}
	}
	released, err := d.complete(r, core.SessionCompleted, r.spec.findings, t)
	if err != nil {
		return err
	}
	for _, m := range r.spec.mishaps {
		payload := map[string]any{"description": m}
		if ids := d.memoriesNamed(r, m); len(ids) > 0 {
			payload["item_ids"] = ids
		}
		id, err := d.event(core.EventAgentMishap, r.id, r.spec.project, "", payload)
		if err != nil {
			return err
		}
		d.fx.Links.Mishap = id
	}
	if len(r.spec.mishaps) > 0 {
		args["mishaps"] = r.spec.mishaps
	}
	ended := map[string]any{"claims_released": released, "findings": r.spec.findings}
	if n := r.spec.commits; n > 0 {
		ended["commits_recorded"] = n
	}
	if _, err := d.event(core.EventSessionEnded, r.id, r.spec.project, "", ended); err != nil {
		return err
	}
	result["claims_released"], result["mishaps_recorded"] = released, len(r.spec.mishaps)
	return d.toolCall(r, "session_end", args, result)
}

// endHook is Claude Code's SessionEnd with no session_end before it: the
// transcript's usage, then the session completes with its harvested findings
// (hooks.endSessions, endedSession).
func (d *diarySeeder) endHook(r *diaryRun) error {
	if err := d.harvestClaude(r, r.last); err != nil {
		return err
	}
	t := d.t()
	findings := harvest.Prefix + r.spec.findings
	released, err := d.complete(r, core.SessionCompleted, findings, t)
	if err != nil {
		return err
	}
	_, err = d.event(core.EventSessionEnded, r.id, r.spec.project, "", map[string]any{
		"ambient": true, "harvested": true, "linked": false,
		"reason": cmp.Or(r.spec.reason, "prompt_input_exit"), "claims_released": released, "findings": findings,
	})
	return err
}

// reap is the idle reaper expiring a session that sent no end signal: its
// updated_at stays at its last sign of life (gardener.reapStaleSessions).
func (d *diarySeeder) reap(r *diaryRun) error {
	t := d.t()
	sess, err := d.session(r)
	if err != nil {
		return err
	}
	sess.Status = core.SessionExpired
	if err := store.UpdateSession(d.ctx, d.s.db, sess); err != nil {
		return fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	released, err := store.ReleaseReapedClaims(d.ctx, d.s.db, r.id, t)
	if err != nil {
		return fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	_, err = d.event(core.EventSessionEnded, r.id, r.spec.project, "", map[string]any{
		"reason": "expired", "reaped": true, "claims_released": released,
	})
	return err
}

// complete moves the session to status with findings at t and releases the
// task claims it still holds.
func (d *diarySeeder) complete(r *diaryRun, status core.SessionStatus, findings string, t time.Time) (int, error) {
	sess, err := d.session(r)
	if err != nil {
		return 0, err
	}
	sess.Status, sess.Findings, sess.UpdatedAt = status, findings, t
	if err := store.UpdateSession(d.ctx, d.s.db, sess); err != nil {
		return 0, fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	released, err := store.ReleaseClaimsForSession(d.ctx, d.s.db, r.id, t)
	if err != nil {
		return 0, fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	return released, nil
}

// session reads r's row as it stands.
func (d *diarySeeder) session(r *diaryRun) (core.Session, error) {
	sess, found, err := store.SessionByID(d.ctx, d.s.db, r.id)
	if err != nil {
		return core.Session{}, fmt.Errorf("session %s: %w", r.spec.key, err)
	}
	if !found {
		return core.Session{}, fmt.Errorf("session %s: %w", r.spec.key, store.ErrSessionNotFound)
	}
	return sess, nil
}

// memoriesNamed is the ids of the active memories in r's scope whose name the
// text names (mcp.mishapMemoryIDs' linkage).
func (d *diarySeeder) memoriesNamed(r *diaryRun, text string) []string {
	var ids []string
	for _, key := range d.memLog {
		m := d.mems[key]
		if m.superseded == nil && (m.spec.project == r.spec.project || m.spec.project == "") && strings.Contains(text, key) {
			ids = append(ids, m.mem.ID)
		}
	}
	return ids
}

// ---------------------------------------------------------------------------
// acts: what the agents' tool calls and the plan-mode hooks do
// ---------------------------------------------------------------------------

func (d *diarySeeder) act(r *diaryRun, a dAct) error {
	var err error
	switch a.do {
	case doAdd:
		err = d.addTask(r, a.key)
	case doClaim:
		err = d.claimTask(r, a.key, a.n)
	case doDone:
		err = d.closeTask(r, a.key, core.TaskDone, a.n)
	case doDrop:
		err = d.closeTask(r, a.key, core.TaskDropped, 0)
	case doMemory:
		err = d.writeMemory(r, a.key, "")
	case doReplace:
		err = d.writeMemory(r, a.key, a.key2)
	case doNote:
		err = d.writeNote(r, a.key)
	case doTrial:
		err = d.recordTrial(r, a.key)
	case doCapture:
		err = d.capturePlan(r, a.key, a.n)
	case doPresent:
		err = d.retagPlan(r, a.key, plans.StatusPresented, core.EventPlanPresented)
	case doApprove:
		err = d.approvePlan(r, a.key, a.key2)
	default:
		err = errors.New("unknown act")
	}
	if err != nil {
		return fmt.Errorf("session %s: act %d %s: %w", r.spec.key, a.do, a.key, err)
	}
	return nil
}

func (d *diarySeeder) task(key string) (*diaryTask, error) {
	task, ok := d.tasks[key]
	if !ok {
		return nil, fmt.Errorf("no task %s", key)
	}
	if task.id == "" {
		return nil, fmt.Errorf("task %s is not created yet", key)
	}
	return task, nil
}

// addTask is tasks_add.
func (d *diarySeeder) addTask(r *diaryRun, key string) error {
	task, ok := d.tasks[key]
	if !ok || task.id != "" {
		return errors.New("unknown or already created")
	}
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return err
	}
	var deps []string
	for _, dep := range task.spec.deps {
		blocker, err := d.task(dep)
		if err != nil {
			return err
		}
		deps = append(deps, blocker.id)
	}
	if err := store.CreateTask(d.ctx, d.s.db, core.Task{
		ID: id, ProjectSlug: task.spec.project, Title: task.spec.title, Body: task.spec.body,
		Status: core.TaskOpen, CreatedBy: r.name, PlanSlug: task.spec.plan, DependsOn: deps,
		CreatedAt: t, UpdatedAt: t,
	}); err != nil {
		return err
	}
	task.id, task.status = id, core.TaskOpen
	if _, err := d.event(core.EventTaskTransition, r.id, task.spec.project, id,
		map[string]any{"to": string(core.TaskOpen), "created": true}); err != nil {
		return err
	}
	args := map[string]any{"title": task.spec.title}
	if task.spec.plan != "" {
		args["plan"] = task.spec.plan
	}
	if task.spec.project != r.spec.project {
		args["project"] = task.spec.project
	}
	if len(deps) > 0 {
		args["depends_on"] = deps
	}
	return d.toolCall(r, "tasks_add", args, map[string]any{
		"id": id, "title": task.spec.title, "status": string(core.TaskOpen), "project": task.spec.project, "created_by": r.name,
	})
}

// claimTask is tasks_claim, with a lease of leaseSeconds (0: the default).
func (d *diarySeeder) claimTask(r *diaryRun, key string, leaseSeconds int) error {
	task, err := d.task(key)
	if err != nil {
		return err
	}
	lease := 900 * time.Second
	if leaseSeconds > 0 {
		lease = time.Duration(leaseSeconds) * time.Second
	}
	res, err := store.ClaimTask(d.ctx, d.s.db, task.id, r.id, lease, d.t())
	if err != nil {
		return err
	}
	task.status = res.Task.Status
	payload := map[string]any{"to": string(res.Task.Status), "claimed_by": r.id}
	if res.Reclaimed {
		payload["reclaimed"], payload["prior_holder"] = true, res.PriorHolder
	}
	if _, err := d.event(core.EventTaskTransition, r.id, task.spec.project, task.id, payload); err != nil {
		return err
	}
	args := map[string]any{"id": task.id}
	if leaseSeconds > 0 {
		args["lease_seconds"] = leaseSeconds
	}
	result := map[string]any{"id": task.id, "status": string(res.Task.Status), "claimed_by": r.id}
	if res.Task.LeaseExpiresAt != nil {
		result["lease_expires_at"] = core.FormatTime(*res.Task.LeaseExpiresAt)
	}
	return d.toolCall(r, "tasks_claim", args, result)
}

// closeTask is tasks_update status=done or dropped, recording commits; the
// close that completes a plan with a step done mints its plan.shipped, as
// store.CheckMilestones does with momentum on.
func (d *diarySeeder) closeTask(r *diaryRun, key string, status core.TaskStatus, commits int) error {
	task, err := d.task(key)
	if err != nil {
		return err
	}
	patch := store.TaskPatch{Status: &status}
	if commits > 0 {
		patch.AddCommits = d.commitIDs(commits)
	}
	res, err := store.UpdateTaskDetailed(d.ctx, d.s.db, task.id, patch, r.id, d.t())
	if err != nil {
		return err
	}
	task.status = res.Task.Status
	transition := map[string]any{"to": string(res.Task.Status), "from": string(res.From)}
	args := map[string]any{"id": task.id, "status": string(status)}
	result := map[string]any{"id": task.id, "status": string(res.Task.Status), "project": task.spec.project}
	if commits > 0 {
		transition["commits_recorded"] = commits
		args["commits"] = patch.AddCommits
		result["commits_recorded"] = commits
	}
	if _, err := d.event(core.EventTaskTransition, r.id, task.spec.project, task.id, transition); err != nil {
		return err
	}
	if err := d.toolCall(r, "tasks_update", args, result); err != nil {
		return err
	}
	if status != core.TaskDone || task.spec.plan == "" {
		return nil
	}
	steps, err := store.ListTasksForPlan(d.ctx, d.s.db, task.spec.project, "", task.spec.plan)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if !step.Status.Closed() {
			return nil
		}
	}
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return err
	}
	if _, _, err := d.s.rec.RecordOnce(d.ctx, core.Event{
		ID: id, TS: t, Kind: core.EventPlanShipped, SessionID: r.id, ProjectSlug: task.spec.project, ItemID: task.spec.plan,
		Payload: map[string]any{"plan": task.spec.plan, "project": task.spec.project, "steps": len(steps)},
	}); err != nil {
		return err
	}
	d.fx.Links.ShippedPlan, d.fx.Links.ShippedPlanProject = task.spec.plan, task.spec.project
	return nil
}

// writeMemory is memory_write, superseding the memory supersedes names when
// one is given (lifecycle.Supersede, inside the old file's lock).
func (d *diarySeeder) writeMemory(r *diaryRun, key, supersedes string) error {
	m, ok := d.mems[key]
	if !ok || m.written {
		return errors.New("unknown or already written")
	}
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return err
	}
	written, err := d.s.mgr.WriteMemory(d.ctx, core.Memory{
		ID: id, Kind: m.spec.kind, Name: key, Description: m.spec.desc, Project: m.spec.project, Body: m.spec.body,
		Created: t, Updated: t, ValidFrom: t, SourceSession: r.id, Model: r.spec.model,
	})
	if err != nil {
		return err
	}
	m.mem, m.written = written, true
	d.memLog = append(d.memLog, key)
	if _, err := d.event(core.EventMemoryWritten, r.id, m.spec.project, written.ID,
		map[string]any{"name": key, "kind": string(m.spec.kind), "updated": false}); err != nil {
		return err
	}
	args := map[string]any{"name": key, "kind": string(m.spec.kind), "description": m.spec.desc}
	result := map[string]any{"id": written.ID, "name": key, "project": m.spec.project, "updated": false}
	if supersedes != "" {
		old, ok := d.mems[supersedes]
		if !ok || !old.written || old.superseded != nil {
			return fmt.Errorf("%s is not an active memory", supersedes)
		}
		at := d.t()
		var retired core.Memory
		if err := d.s.mgr.Mutate(d.ctx, old.mem.FilePath, func(ctx context.Context) error {
			full, err := d.s.mgr.Store().ReadMemory(old.mem.FilePath)
			if err != nil {
				return err
			}
			retired, err = lifecycle.Supersede(ctx, d.s.mgr, full, written, at)
			return err
		}); err != nil {
			return err
		}
		old.mem, old.superseded = retired, &at
		if _, err := d.event(core.EventMemorySuperseded, r.id, retired.Project, retired.ID,
			map[string]any{"name": retired.Name, "superseded_by": written.ID}); err != nil {
			return err
		}
		args["supersedes"] = supersedes
		result["superseded"] = lifecycle.MemoryRef(retired.Project, retired.Name)
		d.fx.Links.SupersededMemory, d.fx.Links.SupersedingMemory = retired.ID, written.ID
	}
	return d.toolCall(r, "memory_write", args, result)
}

func (d *diarySeeder) noteSpec(key string) (dNote, error) {
	for _, n := range diaryNotes {
		if n.key == key {
			return n, nil
		}
	}
	return dNote{}, fmt.Errorf("no note %s", key)
}

// writeNote is notes_create.
func (d *diarySeeder) writeNote(r *diaryRun, key string) error {
	n, err := d.noteSpec(key)
	if err != nil {
		return err
	}
	if _, done := d.notes[key]; done || n.capture != nil {
		return errors.New("already written, or a captured plan")
	}
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return err
	}
	written, err := d.s.mgr.WriteNote(d.ctx, core.Note{
		ID: id, Title: n.title, Slug: key, Description: n.desc, Project: n.project, Body: n.body,
		Tags: n.tags, Model: r.spec.model, Created: t, Updated: t,
	})
	if err != nil {
		return err
	}
	d.notes[key] = written
	if _, err := d.event(core.EventNoteWritten, r.id, n.project, written.ID, map[string]any{"title": n.title}); err != nil {
		return err
	}
	args := map[string]any{"title": n.title, "tags": n.tags}
	if n.project != r.spec.project {
		args["project"] = cmp.Or(n.project, "global")
	}
	return d.toolCall(r, "notes_create", args, map[string]any{
		"id": written.ID, "slug": key, "project": n.project, "title": n.title,
	})
}

// recordTrial is trial_record.
func (d *diarySeeder) recordTrial(r *diaryRun, key string) error {
	var spec *dTrial
	for i := range diaryTrials {
		if diaryTrials[i].key == key {
			spec = &diaryTrials[i]
		}
	}
	if spec == nil || d.trials[key] != "" {
		return errors.New("unknown or already recorded")
	}
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return err
	}
	if err := store.CreateTrial(d.ctx, d.s.db, core.Trial{
		ID: id, Lab: spec.lab, Title: spec.title, Changes: spec.changes, Expected: spec.expected, Actual: spec.actual,
		Outcome: spec.outcome, Metrics: spec.metrics, SessionID: r.id, ProjectSlug: spec.project, CreatedAt: t,
	}); err != nil {
		return err
	}
	d.trials[key] = id
	if _, err := d.event(core.EventTrialRecorded, r.id, spec.project, id,
		map[string]any{"lab": spec.lab, "title": spec.title, "outcome": string(spec.outcome)}); err != nil {
		return err
	}
	return d.toolCall(r, "trial_record", map[string]any{"lab": spec.lab, "title": spec.title, "outcome": string(spec.outcome)},
		map[string]any{"id": id, "lab": spec.lab, "title": spec.title, "outcome": string(spec.outcome)})
}

// capturePlan is plan mode writing iteration iter of the plan file behind the
// captured-plan note key (hooks.upsertPlanNote): the note's text is the
// iteration, stamped, and plan.captured carries it.
func (d *diarySeeder) capturePlan(r *diaryRun, key string, iter int) error {
	n, err := d.noteSpec(key)
	if err != nil {
		return err
	}
	c := n.capture
	if c == nil || iter < 1 || iter > len(c.iterations) || iter != d.iters[key]+1 {
		return errors.New("not a captured plan, or not its next iteration")
	}
	t := d.t()
	note, ok := d.notes[key]
	if !ok {
		id, err := d.id(t)
		if err != nil {
			return err
		}
		note = core.Note{ID: id, Slug: key, Project: n.project, Title: n.title, Created: t}
	}
	content := c.iterations[iter-1]
	note.Body = fmt.Sprintf("> captured from %s | %s.md | iter %d | git %s | %s",
		r.name, c.basename, iter, gitHead(r.spec.key), t.Format(time.RFC3339)) + "\n\n" + content
	note.Description = plans.NoteDescription(c.basename, iter, plans.StatusDraft)
	note.Tags = plans.SetStatusTag([]string{plans.SlugTag(c.plan), plans.TagPlan, "created-by:agent"}, plans.StatusDraft)
	note.Model, note.Updated = r.spec.model, t
	note.Extra = map[string]any{"plan_iteration": iter}
	written, err := d.s.mgr.WriteNote(d.ctx, note)
	if err != nil {
		return err
	}
	d.notes[key], d.iters[key] = written, iter
	_, err = d.event(core.EventPlanCaptured, r.id, n.project, written.ID, map[string]any{
		"basename": c.basename, "plan_slug": c.plan, "iteration": iter, "title": n.title, "content": content,
		"claude_session_id": r.ext, "external_client": clientClaude,
	})
	return err
}

// retagPlan moves a captured plan to status, recording kind
// (hooks.markPlanPresented, and the approval's retag).
func (d *diarySeeder) retagPlan(r *diaryRun, key, status string, kind core.EventKind) error {
	n, err := d.noteSpec(key)
	if err != nil {
		return err
	}
	note, ok := d.notes[key]
	if !ok || n.capture == nil {
		return errors.New("not captured yet")
	}
	t := d.t()
	iter := d.iters[key]
	note.Tags = plans.SetStatusTag(note.Tags, status)
	note.Description = plans.NoteDescription(n.capture.basename, iter, status)
	note.Updated = t
	written, err := d.s.mgr.WriteNote(d.ctx, note)
	if err != nil {
		return err
	}
	d.notes[key] = written
	payload := map[string]any{
		"basename": n.capture.basename, "plan_slug": n.capture.plan,
		"claude_session_id": r.ext, "external_client": clientClaude,
	}
	if kind == core.EventPlanApproved {
		payload["iteration"], payload["title"] = iter, n.title
	}
	_, err = d.event(kind, r.id, n.project, written.ID, payload)
	return err
}

// approvePlan is the owner approving a captured plan: the note is approved
// and its "Implement plan" tracking task is created (plans.EnsureTask, whose
// title and body it mirrors with the story's time).
func (d *diarySeeder) approvePlan(r *diaryRun, key, taskKey string) error {
	if err := d.retagPlan(r, key, plans.StatusApproved, core.EventPlanApproved); err != nil {
		return err
	}
	if _, dup := d.tasks[taskKey]; dup {
		return fmt.Errorf("task %s exists", taskKey)
	}
	note := d.notes[key]
	n, err := d.noteSpec(key)
	if err != nil {
		return err
	}
	slug := n.capture.plan
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return err
	}
	spec := dTask{key: taskKey, project: note.Project, plan: slug, title: "Implement plan: " + note.Title,
		body: fmt.Sprintf("Plan note: %s (id %s). Agent caches: notes tagged plan:%s. "+
			"Check git stamps for staleness before trusting caches.", note.Slug, note.ID, slug)}
	if err := store.CreateTask(d.ctx, d.s.db, core.Task{
		ID: id, ProjectSlug: spec.project, Title: spec.title, Body: spec.body, Status: core.TaskOpen,
		CreatedBy: r.name, PlanSlug: slug, CreatedAt: t, UpdatedAt: t,
	}); err != nil {
		return err
	}
	d.tasks[taskKey] = &diaryTask{spec: spec, id: id, status: core.TaskOpen}
	_, err = d.event(core.EventTaskTransition, r.id, spec.project, id, map[string]any{
		"to": string(core.TaskOpen), "created": true, "plan_slug": slug,
		"claude_session_id": r.ext, "external_client": clientClaude,
	})
	return err
}

// gitHead is a stable, made-up 12-digit commit head for a session's stamps.
func gitHead(key string) string {
	sum := sha256.Sum256([]byte("head:" + key))
	return hex.EncodeToString(sum[:6])
}

// ---------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------

// event records an event at the running op's clock, with an id minted at
// that time so the log's ids sort with its times.
func (d *diarySeeder) event(kind core.EventKind, sessionID, project, itemID string, payload map[string]any) (string, error) {
	t := d.t()
	id, err := d.id(t)
	if err != nil {
		return "", err
	}
	if _, err := d.s.rec.Record(d.ctx, core.Event{
		ID: id, TS: t, Kind: kind, SessionID: sessionID, ProjectSlug: project, ItemID: itemID, Payload: payload,
	}); err != nil {
		return "", fmt.Errorf("record %s: %w", kind, err)
	}
	return id, nil
}

// toolCall records the tool.call event the MCP middleware logs after a call
// (mcp.logMiddleware): the tool, its arguments and its JSON result.
func (d *diarySeeder) toolCall(r *diaryRun, tool string, args, result map[string]any) error {
	text, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("tool.call %s: %w", tool, err)
	}
	_, err = d.event(core.EventToolCall, r.id, r.spec.project, "", map[string]any{
		"tool": tool, "duration_ms": 4 + d.rng.Intn(60), "args": args, "result": string(text),
	})
	return err
}

// ---------------------------------------------------------------------------
// fixture primitives: the three writes no store API makes
// ---------------------------------------------------------------------------

// setPreLedgerTotals sets a session's five token columns directly: the
// fixture's day older than the ledger. Before migration 053 a harvest wrote
// the totals alone; since the ledger, the totals are always the sum of the
// session's ledger rows (store/ledger.go) and no store API writes them
// without rows, so a session of a pre-053 install -- totals, no rows -- can
// only be seeded by hand.
func setPreLedgerTotals(ctx context.Context, db *sql.DB, sessionID string, t core.TokenUsage) error {
	t.Normalize()
	res, err := db.ExecContext(ctx, `UPDATE sessions
		   SET input_tokens = ?, cached_input_tokens = ?, cache_creation_tokens = ?, output_tokens = ?, total_tokens = ?
		 WHERE id = ?`, t.Input, t.Cached, t.CacheCreation, t.Output, t.Total, sessionID)
	if err != nil {
		return fmt.Errorf("pre-ledger totals of %s: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pre-ledger totals of %s: rows affected: %w", sessionID, err)
	}
	if n == 0 {
		return fmt.Errorf("pre-ledger totals of %s: %w", sessionID, store.ErrSessionNotFound)
	}
	return nil
}

// removeTaskRow deletes a task's row and its search-index row: the fixture's
// task deleted after it closed. Seamless itself never deletes a task (no store
// API does; migration 030 says so), yet a row can go -- removed by hand, or
// lost with a restore -- while the task.transition events naming it stay, and
// the Diary must render those as plain text. Its dependency edges cascade.
func removeTaskRow(ctx context.Context, db *sql.DB, id string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove task %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove task %s: rows affected: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("remove task %s: %w", id, store.ErrTaskNotFound)
	}
	if err := store.DeleteWorkFTS(ctx, db, id); err != nil {
		return fmt.Errorf("remove task %s: %w", id, err)
	}
	return nil
}

// removeDeletedTask removes the story's deleted task once its history is in.
func (d *diarySeeder) removeDeletedTask() error {
	task, err := d.task(diaryDeletedTask)
	if err != nil {
		return err
	}
	if task.status != core.TaskDone {
		return fmt.Errorf("task %s is %s, not done", diaryDeletedTask, task.status)
	}
	if err := removeTaskRow(d.ctx, d.s.db, task.id); err != nil {
		return err
	}
	d.fx.Links.DeletedTask = task.id
	return d.links()
}

// keepTranscriptCopy writes a flagged session's transcript copy where the
// transcripts service keeps one (<data>/transcripts/<id>.jsonl.gz, directory
// 0700, file 0600) and records it. The service stamps a copy with the wall
// clock and its clock cannot be set, so the fixture writes the backdated copy
// itself, the way Service.commit records one.
func (d *diarySeeder) keepTranscript(r *diaryRun) error {
	raw, err := d.transcript(r)
	if err != nil {
		return err
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("transcript of %s: %w", r.spec.key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("transcript of %s: %w", r.spec.key, err)
	}
	dir := filepath.Join(d.s.mgr.Store().DataDir(), transcripts.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("transcript of %s: %w", r.spec.key, err)
	}
	if err := os.WriteFile(filepath.Join(dir, r.id+transcripts.Suffix), gz.Bytes(), 0o600); err != nil {
		return fmt.Errorf("transcript of %s: %w", r.spec.key, err)
	}
	t := d.t()
	recorded, err := store.SetTranscriptCopy(d.ctx, d.s.db, r.id, core.TranscriptCopied, int64(len(raw)), t)
	if err != nil {
		return fmt.Errorf("transcript of %s: %w", r.spec.key, err)
	}
	if !recorded {
		return fmt.Errorf("transcript of %s: the session is not flagged", r.spec.key)
	}
	_, err = d.event(core.EventSessionTranscript, r.id, r.spec.project, "", map[string]any{
		"copy": string(core.TranscriptCopied), "bytes": len(raw), "via": "daemon",
	})
	return err
}

// transcript is a short Claude Code transcript (JSONL) of the session: its
// first prompt, a few replies, and its findings as the last one.
func (d *diarySeeder) transcript(r *diaryRun) ([]byte, error) {
	replies := []string{
		"Reading the backup job's log from tonight's run.",
		"The upload step got 403 from the object store. Checking the credential the job uses.",
		"The credential expired at 22:00. Rotating it through the pipeline, then re-running the backup.",
		r.spec.findings,
	}
	prompt := ""
	if len(r.spec.prompts) > 0 {
		prompt = r.spec.prompts[0].text
	}
	var b bytes.Buffer
	line := func(v map[string]any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("transcript of %s: %w", r.spec.key, err)
		}
		b.Write(raw)
		b.WriteByte('\n')
		return nil
	}
	if err := line(map[string]any{
		"type": "user", "sessionId": r.ext, "cwd": r.cwd(), "isSidechain": false,
		"timestamp": r.start.UTC().Format(time.RFC3339), "message": map[string]any{"role": "user", "content": prompt},
	}); err != nil {
		return nil, err
	}
	span := r.last.Sub(r.start)
	for i, text := range replies {
		at := r.start.Add(span * time.Duration(i+1) / time.Duration(len(replies)+1))
		if err := line(map[string]any{
			"type": "assistant", "sessionId": r.ext, "cwd": r.cwd(), "isSidechain": false,
			"timestamp": at.UTC().Format(time.RFC3339),
			"message": map[string]any{
				"id": fmt.Sprintf("msg_%s_%02d", strings.ToLower(r.id[len(r.id)-10:]), i), "role": "assistant",
				"model": r.spec.model, "content": []map[string]any{{"type": "text", "text": text}},
				"usage": map[string]any{
					"input_tokens": 3 + i, "cache_read_input_tokens": 18000 + 9000*i,
					"cache_creation_input_tokens": 900 + 300*i, "output_tokens": 180 + 90*i,
				},
			},
		}); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// ---------------------------------------------------------------------------
// the link cases and the summary
// ---------------------------------------------------------------------------

// The story's items behind the link cases that are not found as they happen.
const (
	diaryGlobalSession = "w5-global"       // a session in the global scope, with an outcome
	diaryCommitSession = "w6-api"          // records a commit at its session_end
	diaryUnreported    = "w11-web-desktop" // a client without hooks
	diaryCommitTask    = "rl1"             // closes with two commits
	diaryLinkTrial     = "rl-load-1"
)

// links fills the fixture's link cases and checks that every one was seeded.
func (d *diarySeeder) links() error {
	l := &d.fx.Links
	for _, c := range []struct {
		dst *string
		key string
	}{
		{&l.GlobalSession, diaryGlobalSession}, {&l.CommitSession, diaryCommitSession},
		{&l.UnreportedSession, diaryUnreported}, {&l.FlaggedSession, diaryMidnight.key},
		{&l.MidnightSession, diaryMidnight.key},
	} {
		r, ok := d.runs[c.key]
		if !ok {
			return fmt.Errorf("no session %s", c.key)
		}
		*c.dst = r.id
	}
	task, err := d.task(diaryCommitTask)
	if err != nil {
		return err
	}
	l.CommitTask = task.id
	l.Trial = d.trials[diaryLinkTrial]
	l.TaskOnlyPlan, l.TaskOnlyPlanProject = planBackupRotation, "example-ops"
	l.SharedSlug, l.SharedSlugProjects = planDependencyRefresh, [2]string{"example-web", "example-api"}
	if l.Trial == "" || l.ShippedPlan == "" || l.SupersededMemory == "" || l.Mishap == "" || l.DeletedTask == "" {
		return fmt.Errorf("a link case was not seeded: %+v", *l)
	}
	return nil
}

// Summary is the fixture's map as text: a line per covered day, then the
// link cases.
func (fx DiaryFixture) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "demoseed: diary fixture seeded around %s\n", fx.Anchor.Format("Mon 2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "  %-14s %4s  %-15s %8s %12s\n", "day", "back", "role", "sessions", "ledger rows")
	for _, day := range fx.Days {
		back := int(math.Round(fx.Days[0].Date.Sub(day.Date).Hours() / 24))
		fmt.Fprintf(&b, "  %-14s %4d  %-15s %8d %12d\n", day.Date.Format("Mon 2006-01-02"), back, day.Role,
			len(day.Sessions), day.LedgerRows)
	}
	l := fx.Links
	for _, line := range [][2]string{
		{"deleted task", l.DeletedTask},
		{"task-only plan", l.TaskOnlyPlanProject + " plan:" + l.TaskOnlyPlan},
		{"shared slug", "plan:" + l.SharedSlug + " in " + l.SharedSlugProjects[0] + " (note) and " + l.SharedSlugProjects[1]},
		{"shipped plan", l.ShippedPlanProject + " plan:" + l.ShippedPlan},
		{"superseded memory", l.SupersededMemory + " by " + l.SupersedingMemory},
		{"global session", l.GlobalSession},
		{"commit records", "task " + l.CommitTask + ", session " + l.CommitSession},
		{"flagged session", l.FlaggedSession + " (spans midnight)"},
		{"mishap event", l.Mishap},
		{"trial", l.Trial},
		{"unreported session", l.UnreportedSession},
	} {
		fmt.Fprintf(&b, "  %-19s %s\n", line[0], line[1])
	}
	return b.String()
}
