// The scale layer (demoseed -review -scale, plan:console-ux-pass UX 25): the
// review fixture grown to the owner's size -- about 1,500 active memories, 300
// archived and superseded ones, 700 notes and 55 projects -- so the steps that
// change the library screens can measure page weight where the pain shows. It
// runs after the review layer, changes none of that layer's rows, and draws
// from an RNG of its own, so a seed at a fixed anchor is byte-identical from
// run to run.
//
// Updated times decay exponentially over scaleDays back from the seeder's
// anchor, the same "now" the review layer uses, so the today, this week and
// this month groups all fill. The quiet projects keep their quiet: what the
// layer gives them predates their last session. Items go in through the files
// layer (which quotes every YAML scalar that needs it), the store and the
// events recorder; supersession and archiving go through lifecycle, as the
// real tools do.

package demokit

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// scaleSeed seeds the scale layer's RNG.
const scaleSeed = 20261122

const (
	// scaleDays is how far back the layer's updated times reach.
	scaleDays = 200
	// scaleMeanDays is the mean of their exponential decay.
	scaleMeanDays = 40
	// scaleMaxDesc is the longest description the layer writes: the index
	// line's budget.
	scaleMaxDesc = 150
	// scaleSupersededPct and scaleArchivedPct are how many retired memories
	// a project gets beside its active ones, in percent of them.
	scaleSupersededPct = 15
	scaleArchivedPct   = 9
)

// ScaleFixture is what SeedReviewScale's scale layer seeded: what it added,
// the whole seed's totals after it, and the pages to measure.
type ScaleFixture struct {
	Added ScaleCounts
	Total ScaleCounts
	// Kinds counts the active memories by kind, the most common first.
	Kinds []ScaleKind
	// Today, Week and Month count the active memories updated in the last
	// day, 7 days and 30 days before the anchor; Oldest is the oldest one's
	// age in whole days.
	Today, Week, Month, Oldest int
	// Starred are the starred projects, sorted.
	Starred []string
	// Locked is the confidential project LockConfig locks under peer_auth.
	Locked, LockConfig string
	// Pages are the console paths to measure, relative to the console's
	// origin.
	Pages []ScalePage

	// added holds the ids the layer wrote and the plans it made, for the
	// package's tests.
	added scaleAdded
}

// scaleAdded is what one scale layer run wrote, by id.
type scaleAdded struct {
	memories, notes map[string]bool
	plans           map[string]bool // project + "/" + plan slug
}

// ScaleCounts counts one side of a scale seed. Active, Quiet and Retired are
// the Projects screen's bands; the quiet ones had no live agent and no session
// or task change for 30 days.
type ScaleCounts struct {
	Projects, Active, Quiet, Retired, Confidential int
	Memories, Superseded, Archived                 int // memories: active, superseded, archived
	Notes, PlanNotes, AgentNotes                   int
	Sessions, Tasks, OpenTasks                     int
}

// ScaleKind is one memory kind and how many active memories carry it.
type ScaleKind struct {
	Kind core.MemoryKind
	N    int
}

// ScalePage is one page to measure: what it is and its path.
type ScalePage struct{ Name, Path string }

// Summary is the scale layer's part of the demoseed -review -scale summary.
func (fx ScaleFixture) Summary() string {
	var b strings.Builder
	a, t := fx.Added, fx.Total
	fmt.Fprintf(&b, "scale: +%d projects, +%d memories (+%d superseded, +%d archived), +%d notes (%d tagged with a plan, %d agent working copies), +%d sessions, +%d tasks\n",
		a.Projects, a.Memories, a.Superseded, a.Archived, a.Notes, a.PlanNotes, a.AgentNotes, a.Sessions, a.Tasks)
	fmt.Fprintf(&b, "totals: %d projects (%d active, %d quiet for 30+ days, %d retired; %d confidential; %d starred: %s)\n",
		t.Projects, t.Active, t.Quiet, t.Retired, t.Confidential, len(fx.Starred), strings.Join(fx.Starred, ", "))
	fmt.Fprintf(&b, "        %d active memories (%d in all: %d superseded, %d archived), %d notes (%d tagged with a plan, %d agent working copies), %d sessions, %d tasks (%d open)\n",
		t.Memories, t.Memories+t.Superseded+t.Archived, t.Superseded, t.Archived, t.Notes, t.PlanNotes, t.AgentNotes, t.Sessions, t.Tasks, t.OpenTasks)
	kinds := make([]string, len(fx.Kinds))
	for i, k := range fx.Kinds {
		kinds[i] = fmt.Sprintf("%s=%d", k.Kind, k.N)
	}
	fmt.Fprintf(&b, "kinds: %s\n", strings.Join(kinds, " "))
	fmt.Fprintf(&b, "updated: %d active memories in the last day, %d in the last 7 days, %d in the last 30 days, the oldest %d days back\n",
		fx.Today, fx.Week, fx.Month, fx.Oldest)
	fmt.Fprintf(&b, "locked: %s is confidential and locks only under peer_auth, from the daemon's config: %s\n", fx.Locked, fx.LockConfig)
	b.WriteString("measure (paths under the console, any port):\n")
	for _, p := range fx.Pages {
		fmt.Fprintf(&b, "  %-14s %s\n", p.Name, p.Path)
	}
	return b.String()
}

// SeedReviewScale seeds the review fixture (SeedReview), then the scale layer
// on top of it, and returns both maps. Like SeedReview it stops the process
// on a broken fleet; a failure in either layer is returned.
func (s *Seeder) SeedReviewScale() (ReviewFixture, ScaleFixture, error) {
	return s.seedReviewScale(100)
}

// seedReviewScale is SeedReviewScale with the grown and new projects' item
// counts at volume percent of the owner's scale; the tests that only need the
// layer's code paths seed less of it.
func (s *Seeder) seedReviewScale(volume int) (ReviewFixture, ScaleFixture, error) {
	fx, err := s.SeedReview()
	if err != nil {
		return ReviewFixture{}, ScaleFixture{}, err
	}
	sc := &scaleSeeder{s: s, ctx: s.ctx, rng: rand.New(rand.NewSource(scaleSeed)), now: s.now, volume: volume,
		planSlugs: map[string]bool{}}
	sc.fx.added = scaleAdded{memories: map[string]bool{}, notes: map[string]bool{}, plans: map[string]bool{}}
	if err := sc.seed(); err != nil {
		return ReviewFixture{}, ScaleFixture{}, fmt.Errorf("demokit: scale: %w", err)
	}
	return fx, sc.fx, nil
}

// scaleSeeder is one scale layer run.
type scaleSeeder struct {
	s   *Seeder
	ctx context.Context
	rng *rand.Rand
	now time.Time
	// volume is the grown and new projects' item counts, in percent of the
	// owner's scale.
	volume int

	projects  []*scaleProject
	planSlugs map[string]bool // every plan slug in use, the review's included
	plans     int             // plans drawn so far, for their states' turn
	fx        ScaleFixture
}

// scaleProject is one project the layer seeds into, with the window its items
// fall in.
type scaleProject struct {
	spec     scaleProjectSpec
	isNew    bool
	active   bool      // a grown or new project, which must not go quiet
	created  time.Time // a new project's creation
	from, to time.Time
	names    map[string]bool // memory names in use
	slugs    map[string]bool // note slugs in use

	slots   []time.Time // session starts on the decay, oldest first
	recent  time.Time   // an active project's session in the last five days
	moments []*scaleMoment
	mems    []*scaleMem // active
	olds    []*scaleMem // superseded and archived
	notes   []*scaleNote
	tasks   []*scaleTask
}

// scaleMoment is a write some session made; grouping the moments makes the
// sessions.
type scaleMoment struct {
	at   time.Time
	sess *core.Session
}

type scaleMem struct {
	mem        core.Memory
	topic      string
	wrote      *scaleMoment
	edited     *scaleMoment // nil when never edited after its write
	successor  *scaleMem    // superseded: what replaced it
	archivedAt time.Time    // archived: when
}

type scaleNote struct {
	note   core.Note
	wrote  *scaleMoment
	edited *scaleMoment
	agent  string // a working copy's agent type
	prompt string
}

type scaleTask struct {
	task   core.Task
	wrote  *scaleMoment
	deps   []*scaleTask
	close  core.TaskStatus
	closed time.Time
}

func (sc *scaleSeeder) seed() error {
	if err := sc.windows(); err != nil {
		return err
	}
	for _, p := range sc.projects {
		if err := sc.plan(p); err != nil {
			return err
		}
	}
	for _, step := range []func() error{sc.writeProjects, sc.writeSessions, sc.writeMemories, sc.writeOlds, sc.writeNotes, sc.writeTasks, sc.marks} {
		if err := step(); err != nil {
			return err
		}
	}
	return sc.summarize()
}

// ---------------------------------------------------------------------------
// windows
// ---------------------------------------------------------------------------

// windows lays out the projects the layer seeds into, each with its window:
// the grown ones from their creation (the global scope from scaleDays back),
// the new ones from theirs, and the review's quiet ones up to a day before
// their last session, so they stay quiet.
func (sc *scaleSeeder) windows() error {
	to := sc.now.Add(-3 * time.Minute).Truncate(time.Second)
	floor := sc.now.AddDate(0, 0, -scaleDays).Truncate(time.Second)
	if err := sc.existingPlanSlugs(); err != nil {
		return err
	}
	for _, spec := range scaleGrownProjects {
		p := &scaleProject{spec: sc.scaled(spec), active: true, from: floor, to: to}
		if spec.slug != "" {
			proj, found, err := store.ProjectBySlug(sc.ctx, sc.s.db, spec.slug)
			if err != nil {
				return fmt.Errorf("project %s: %w", spec.slug, err)
			}
			if !found {
				return fmt.Errorf("project %s: not seeded by the review fixture", spec.slug)
			}
			p.from = later(floor, proj.CreatedAt.Add(time.Hour).Truncate(time.Second))
		}
		sc.add(p)
	}
	for _, spec := range scaleNewProjects {
		created := sc.now.AddDate(0, 0, -spec.age).Add(-time.Duration(sc.rng.Intn(8*60)) * time.Minute).Truncate(time.Second)
		sc.add(&scaleProject{spec: sc.scaled(spec), isNew: true, active: true, created: created, from: later(floor, created.Add(time.Hour)), to: to})
	}
	for i, q := range reviewQuiet {
		proj, found, err := store.ProjectBySlug(sc.ctx, sc.s.db, q.slug)
		if err != nil {
			return fmt.Errorf("quiet project %s: %w", q.slug, err)
		}
		if !found {
			return fmt.Errorf("quiet project %s: not seeded by the review fixture", q.slug)
		}
		var last sql.NullString
		if err := sc.s.db.QueryRowContext(sc.ctx, `SELECT MAX(updated_at) FROM sessions WHERE project_slug = ?`, q.slug).Scan(&last); err != nil {
			return fmt.Errorf("quiet project %s: %w", q.slug, err)
		}
		if !last.Valid {
			return fmt.Errorf("quiet project %s: no session", q.slug)
		}
		lastAt, err := core.ParseTime(last.String)
		if err != nil {
			return fmt.Errorf("quiet project %s: %w", q.slug, err)
		}
		spec := scaleProjectSpec{slug: q.slug, memories: quietMemories(i), notes: quietNotes(i), things: quietThings(q.desc)}
		sc.add(&scaleProject{spec: spec, from: later(floor, proj.CreatedAt.Add(time.Hour).Truncate(time.Second)), to: lastAt.Add(-24 * time.Hour).Truncate(time.Second)})
	}
	for _, p := range sc.projects {
		if err := sc.taken(p); err != nil {
			return err
		}
	}
	return nil
}

// taken records the memory names and note slugs p already holds, so none of
// the layer's lands on a path the fleet or the review wrote.
func (sc *scaleSeeder) taken(p *scaleProject) error {
	for _, q := range []struct {
		query string
		into  map[string]bool
	}{
		{`SELECT name FROM memories_index WHERE project = ?`, p.names},
		{`SELECT slug FROM notes_index WHERE project = ?`, p.slugs},
	} {
		if err := func() error {
			rows, err := sc.s.db.QueryContext(sc.ctx, q.query, p.spec.slug)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					return err
				}
				q.into[v] = true
			}
			return rows.Err()
		}(); err != nil {
			return fmt.Errorf("project %q: names in use: %w", p.spec.slug, err)
		}
	}
	return nil
}

// scaled is spec with its item counts at sc.volume percent, each kept to at
// least one.
func (sc *scaleSeeder) scaled(spec scaleProjectSpec) scaleProjectSpec {
	at := func(n int) int {
		if n == 0 {
			return 0
		}
		return max(1, n*sc.volume/100)
	}
	spec.memories, spec.notes, spec.plans, spec.tasks = at(spec.memories), at(spec.notes), at(spec.plans), at(spec.tasks)
	return spec
}

func (sc *scaleSeeder) add(p *scaleProject) {
	p.names, p.slugs = map[string]bool{}, map[string]bool{}
	sc.projects = append(sc.projects, p)
}

// existingPlanSlugs records the plan slugs the fleet and the review use, so no
// scale plan shares one by accident.
func (sc *scaleSeeder) existingPlanSlugs() error {
	rows, err := sc.s.db.QueryContext(sc.ctx, `SELECT DISTINCT plan_slug FROM tasks WHERE plan_slug <> ''`)
	if err != nil {
		return fmt.Errorf("plan slugs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return fmt.Errorf("plan slugs: %w", err)
		}
		sc.planSlugs[slug] = true
	}
	return rows.Err()
}

// quietMemories and quietNotes are what the i-th quiet project gains: one to
// four memories and one to three notes, the long tail.
func quietMemories(i int) int { return 1 + (i*5)%4 }

func quietNotes(i int) int { return 1 + (i*7)%3 }

// quietThings are a quiet project's things, beside the generic ones: its
// description's last two words and its last word ("analytics pipeline",
// "pipeline"), a leading capital dropped unless the word is an acronym.
func quietThings(desc string) []string {
	words := strings.Fields(desc)
	if len(words) > 2 {
		words = words[len(words)-2:]
	}
	if w := words[0]; len(w) > 1 && unicode.IsLower(rune(w[1])) {
		words[0] = strings.ToLower(w[:1]) + w[1:]
	}
	out := []string{strings.Join(words, " ")}
	if len(words) == 2 {
		out = append(out, words[1])
	}
	return out
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// ago is how long before a window's end an item falls: an exponential decay
// with mean scaleMeanDays, cut at span.
func (sc *scaleSeeder) ago(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	m := float64(scaleMeanDays * 24 * time.Hour)
	u := sc.rng.Float64()
	x := -m * math.Log(1-u*(1-math.Exp(-float64(span)/m)))
	return min(time.Duration(x), span).Truncate(time.Second)
}

// sessionSlots samples p's session starts on the decay: about one for every
// four items, so a session writes a handful of them. An active project also
// gets one session in the last five days, apart from the decay, so it never
// reads as quiet.
func (sc *scaleSeeder) sessionSlots(p *scaleProject) {
	n := max(3, (p.spec.memories+p.spec.notes+p.spec.tasks+4*p.spec.plans)/4)
	p.slots = make([]time.Time, n)
	for i := range p.slots {
		p.slots[i] = p.to.Add(-sc.ago(p.to.Sub(p.from)))
	}
	slices.SortFunc(p.slots, func(a, b time.Time) int { return a.Compare(b) })
	if p.active {
		p.recent = later(p.from, sc.between(p.to.Add(-5*24*time.Hour), p.to.Add(-time.Hour)))
	}
}

// at is an item's time in p's window: within the hour after one of its
// session starts, so its times follow the decay and cluster into sessions.
func (sc *scaleSeeder) at(p *scaleProject) time.Time {
	start := p.slots[sc.rng.Intn(len(p.slots))]
	return earlier(p.to, start.Add(time.Duration(60+sc.rng.Intn(55*60))*time.Second))
}

// snap moves t into the session that started last before it, when one started
// within three days; t otherwise. It never moves t later.
func (sc *scaleSeeder) snap(p *scaleProject, t time.Time) time.Time {
	i, _ := slices.BinarySearchFunc(p.slots, t, func(a, b time.Time) int { return a.Compare(b) })
	if i == len(p.slots) || p.slots[i].After(t) {
		i--
	}
	if i < 0 || t.Sub(p.slots[i]) > 3*24*time.Hour {
		return t
	}
	return earlier(t, p.slots[i].Add(time.Duration(60+sc.rng.Intn(55*60))*time.Second))
}

// between is a time in [a, b], uniform, to the second.
func (sc *scaleSeeder) between(a, b time.Time) time.Time {
	if !b.After(a) {
		return a
	}
	return a.Add(time.Duration(sc.rng.Int63n(int64(b.Sub(a)))).Truncate(time.Second))
}

func (sc *scaleSeeder) id(t time.Time) (string, error) {
	id, err := ulid.New(ulid.Timestamp(t.UTC()), sc.rng)
	if err != nil {
		return "", fmt.Errorf("mint ulid: %w", err)
	}
	return id.String(), nil
}

func (sc *scaleSeeder) pick(xs []string) string { return xs[sc.rng.Intn(len(xs))] }

func (p *scaleProject) moment(at time.Time) *scaleMoment {
	m := &scaleMoment{at: at}
	p.moments = append(p.moments, m)
	return m
}

// ---------------------------------------------------------------------------
// words
// ---------------------------------------------------------------------------

// scaleSlotNames are the template slots, in the order they are filled.
var scaleSlotNames = []string{"{cond}", "{fix}", "{why}", "{reason}", "{when}", "{purpose}", "{opnoun}",
	"{op}", "{A}", "{a}", "{as}", "{au}", "{b}", "{c}", "{v}", "{vb}", "{n}", "{m}", "{k}", "{date}"}

// scaleSlots is one item's slot values.
type scaleSlots map[string]string

func (s scaleSlots) fill(t string) string {
	pairs := make([]string, 0, 2*len(scaleSlotNames))
	for _, k := range scaleSlotNames {
		if v, ok := s[k]; ok {
			pairs = append(pairs, k, v)
		}
	}
	return strings.NewReplacer(pairs...).Replace(t)
}

// slots draws a fresh set of slot values for an item of p, about topic when
// it is not empty, dated at.
func (sc *scaleSeeder) slots(p *scaleProject, topic string, at time.Time) scaleSlots {
	own := 3 // in four, one of p's own things; in two for a project with few
	if len(p.spec.things) < 4 {
		own = 2
	}
	thing := func() string {
		if len(p.spec.things) > 0 && sc.rng.Intn(4) < own {
			return sc.pick(p.spec.things)
		}
		return sc.pick(scaleGeneric)
	}
	a := topic
	if a == "" {
		a = thing()
	}
	b, c := thing(), thing()
	for range 6 {
		if b != a {
			break
		}
		b = thing()
	}
	for range 6 {
		if c != a && c != b {
			break
		}
		c = thing()
	}
	verb := scaleVerbs[sc.rng.Intn(len(scaleVerbs))]
	s := scaleSlots{
		"{a}": a, "{A}": capFirst(a), "{as}": core.Slugify(a), "{au}": strings.ReplaceAll(core.Slugify(a), "-", "_"),
		"{b}": b, "{c}": c, "{v}": verb[0], "{vb}": verb[1], "{op}": sc.pick(scaleOps), "{opnoun}": sc.pick(scaleOpNouns),
		"{n}": strconv.Itoa(2 + sc.rng.Intn(97)), "{m}": strconv.Itoa(10 + sc.rng.Intn(990)), "{k}": strconv.Itoa(50 + sc.rng.Intn(1950)),
		"{date}": at.UTC().Format("2006-01-02"), "{purpose}": sc.pick(scalePurposes),
	}
	inner := scaleSlots{"{c}": c}
	s["{cond}"] = sc.pick(scaleConds)
	s["{fix}"] = inner.fill(sc.pick(scaleFixes))
	s["{why}"] = inner.fill(sc.pick(scaleWhys))
	s["{reason}"] = inner.fill(sc.pick(scaleReasons))
	s["{when}"] = inner.fill(sc.pick(scaleWhens))
	return s
}

// doubledWord reports whether a slug repeats a word back to back, as a
// template filled with its own word does ("migration-migration-window").
func doubledWord(slug string) bool {
	words := strings.Split(slug, "-")
	for i := 1; i < len(words); i++ {
		if words[i] == words[i-1] {
			return true
		}
	}
	return false
}

func capFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// sentence fills one of the body sentences, capitalized.
func (sc *scaleSeeder) sentence(s scaleSlots) string {
	return capFirst(s.fill(sc.pick(scaleSentences)))
}

// paragraph is n sentences of fresh slots for p.
func (sc *scaleSeeder) paragraph(p *scaleProject, topic string, at time.Time, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = sc.sentence(sc.slots(p, topic, at))
	}
	return strings.Join(out, " ")
}

// ---------------------------------------------------------------------------
// planning: every item and its time, before anything is written
// ---------------------------------------------------------------------------

func (sc *scaleSeeder) plan(p *scaleProject) error {
	if !p.to.After(p.from) {
		return nil
	}
	sc.sessionSlots(p)
	for i := range p.spec.memories {
		updated := sc.at(p)
		if i == 0 && p.active { // the recent session writes this one
			updated = earlier(p.to, p.recent.Add(time.Duration(60+sc.rng.Intn(55*60))*time.Second))
		}
		created := updated
		if sc.rng.Intn(100) < 35 { // written earlier, edited since
			created = sc.snap(p, later(p.from, updated.Add(-time.Duration(1+sc.rng.Intn(40*24))*time.Hour)))
		}
		m, err := sc.memorySpec(p, "", "", created, updated)
		if err != nil {
			return err
		}
		m.wrote = p.moment(created)
		if updated.After(created) {
			m.edited = p.moment(updated)
		}
		p.mems = append(p.mems, m)
	}
	if err := sc.superseded(p); err != nil {
		return err
	}
	if err := sc.archived(p); err != nil {
		return err
	}
	if err := sc.planNotes(p); err != nil {
		return err
	}
	sc.ordinaryNotes(p)
	sc.offPlanTasks(p)
	return nil
}

// memorySpec draws one memory of p written at created and last changed at
// updated: its kind (kind, or drawn when empty), a name no memory of p has, a
// description within the index line's budget, and its body, about topic when
// it is not empty. Its id is minted when it is written.
func (sc *scaleSeeder) memorySpec(p *scaleProject, kind core.MemoryKind, topic string, created, updated time.Time) (*scaleMem, error) {
	if kind == "" {
		kind = sc.kind(p)
	}
	for i := range 60 {
		if i == 20 {
			topic = "" // the topic's names are taken: any topic will do
		}
		s := sc.slots(p, topic, created)
		tpl := scaleMemoryTemplates[kind][sc.rng.Intn(len(scaleMemoryTemplates[kind]))]
		name := core.Slugify(s.fill(tpl.name))
		desc := capFirst(s.fill(tpl.desc))
		if sc.rng.Intn(12) == 0 {
			desc = s.fill(sc.pick(scaleTricky))
		}
		if p.names[name] || len(desc) > scaleMaxDesc || doubledWord(name) {
			continue
		}
		p.names[name] = true
		return &scaleMem{topic: s["{a}"], mem: core.Memory{
			Kind: kind, Name: name, Description: desc, Project: p.spec.slug, Body: sc.memoryBody(p, kind, desc, s, created),
			Created: created, Updated: updated, ValidFrom: created,
		}}, nil
	}
	return nil, fmt.Errorf("project %q: no free %s name after 60 draws", p.spec.slug, kind)
}

// kind draws a memory kind by p's weights.
func (sc *scaleSeeder) kind(p *scaleProject) core.MemoryKind {
	weights := scaleKindWeights
	if p.spec.slug == "" {
		weights = scaleGlobalKindWeights
	}
	total := 0
	for _, w := range weights {
		total += w.weight
	}
	r := sc.rng.Intn(total)
	for _, w := range weights {
		if r < w.weight {
			return w.kind
		}
		r -= w.weight
	}
	return weights[0].kind
}

// stageStatuses are a stage memory's statuses: most stages are done.
var stageStatuses = []string{"done", "done", "done", "done", "done", "done", "done", "in_progress", "blocked", "open"}

func (sc *scaleSeeder) memoryBody(p *scaleProject, kind core.MemoryKind, desc string, s scaleSlots, at time.Time) string {
	var b strings.Builder
	if kind == core.KindStage {
		fmt.Fprintf(&b, "Status: %s\n\n", sc.pick(stageStatuses))
	}
	b.WriteString(desc)
	b.WriteString("\n\n")
	b.WriteString(sc.paragraph(p, s["{a}"], at, 1+sc.rng.Intn(3)))
	if sc.rng.Intn(10) < 6 {
		b.WriteString("\n\n## How to apply\n")
		for range 2 + sc.rng.Intn(3) {
			b.WriteString("\n- " + sc.slots(p, s["{a}"], at).fill(sc.pick(scaleBullets)))
		}
	}
	if sc.rng.Intn(5) == 0 {
		b.WriteString("\n\n```sh\n" + s.fill(sc.pick(scaleCommands)) + "\n```")
	}
	b.WriteString("\n")
	return b.String()
}

// superseded draws p's superseded memories: each an earlier take on one of
// p's active memories, replaced when that memory was written.
func (sc *scaleSeeder) superseded(p *scaleProject) error {
	n := len(p.mems) * scaleSupersededPct / 100
	used := map[*scaleMem]bool{}
	for range n {
		var succ *scaleMem
		for range 20 {
			c := p.mems[sc.rng.Intn(len(p.mems))]
			if !used[c] && c.mem.Created.Sub(p.from) > 3*24*time.Hour {
				succ = c
				break
			}
		}
		if succ == nil {
			continue
		}
		used[succ] = true
		created := sc.snap(p, later(p.from, succ.mem.Created.Add(-time.Duration(2*24+sc.rng.Intn(58*24))*time.Hour)))
		old, err := sc.memorySpec(p, succ.mem.Kind, succ.topic, created, created)
		if err != nil {
			return err
		}
		old.successor = succ
		old.wrote = p.moment(created)
		p.olds = append(p.olds, old)
	}
	return nil
}

// archived draws p's archived memories: written, left alone, then archived
// by the gardener's staleness pass.
func (sc *scaleSeeder) archived(p *scaleProject) error {
	n := len(p.mems) * scaleArchivedPct / 100
	end := p.to.Add(-6 * 24 * time.Hour)
	if !end.After(p.from) {
		return nil
	}
	for range n {
		created := sc.snap(p, end.Add(-sc.ago(end.Sub(p.from))))
		m, err := sc.memorySpec(p, "", "", created, created)
		if err != nil {
			return err
		}
		first := created.Add(24 * time.Hour)
		m.archivedAt = sc.between(first, later(first, earlier(p.to.Add(-time.Hour), created.Add(90*24*time.Hour))))
		m.wrote = p.moment(created)
		p.olds = append(p.olds, m)
	}
	return nil
}

// scalePlanStates are the states the layer's plans take, in turn: three in
// seven shipped, two moving, one stalled and one with a note and no steps, so
// the Plans board fills every group at any volume.
var scalePlanStates = []string{"shipped", "moving", "shipped", "note-only", "stalled", "shipped", "moving"}

// scalePlan is one plan the layer seeds: its narrative note, its supporting
// notes and working copies, and (unless it is a note-only plan) its steps.
type scalePlan struct {
	slug, a, b, phrase string
	start              time.Time
	state              string // shipped, moving, stalled or note-only
}

// planNotes draws p's plans: each a narrative note, up to two supporting
// notes, working copies of the subagents that planned it, and its steps.
func (sc *scaleSeeder) planNotes(p *scaleProject) error {
	span := p.to.Sub(p.from)
	for range p.spec.plans {
		pl, err := sc.newPlan(p)
		if err != nil {
			return err
		}
		state := scalePlanStates[sc.plans%len(scalePlanStates)]
		sc.plans++
		switch {
		case state == "shipped" && span > 20*24*time.Hour:
			pl.state, pl.start = state, sc.snap(p, p.to.Add(-12*24*time.Hour).Add(-sc.ago(span-12*24*time.Hour)))
		case state == "moving" && span > 10*24*time.Hour:
			pl.state, pl.start = state, sc.snap(p, sc.between(later(p.from, p.to.Add(-40*24*time.Hour)), p.to.Add(-8*24*time.Hour)))
		case state == "stalled" && span > 35*24*time.Hour:
			pl.state, pl.start = state, sc.snap(p, sc.between(later(p.from, p.to.Add(-80*24*time.Hour)), p.to.Add(-30*24*time.Hour)))
		default:
			pl.state, pl.start = "note-only", sc.at(p)
		}
		if pl.start.Before(p.from) {
			pl.start = p.from
		}
		s := sc.slots(p, pl.a, pl.start)
		s["{b}"] = pl.b
		title := fmt.Sprintf("PLAN: %s -- %s", pl.slug, s.fill(pl.phrase))
		desc := capFirst(s.fill("Why the {a} changes and in what order: {reason}. Each step lands on its own; the last one retires the old {a}."))
		if len(desc) > scaleMaxDesc {
			desc = capFirst(s.fill("Why the {a} changes and in what order; the last step retires the old {a}."))
		}
		body := desc + "\n\n## Why\n\n" + sc.paragraph(p, pl.a, pl.start, 3) + "\n\n## Steps\n\n" +
			"Steps are the tasks tagged plan:" + pl.slug + "; claim one before working it.\n\n## Risks\n\n" +
			sc.paragraph(p, pl.a, pl.start, 2) + "\n"
		narrative := sc.note(p, "plan-"+pl.slug, title, desc, body, []string{plans.SlugTag(pl.slug), "created-by:agent"}, pl.start.Add(-30*time.Minute))
		if pl.state != "note-only" && sc.rng.Intn(10) < 4 {
			if at := sc.snap(p, sc.between(pl.start, p.to)); at.After(narrative.note.Created) {
				narrative.edited = p.moment(at)
				narrative.note.Updated = at
			}
		}
		for range sc.rng.Intn(3) {
			k := scaleNoteKinds[sc.rng.Intn(2)] // research or design
			sc.ordinaryNote(p, k, pl.a, sc.snap(p, later(p.from, sc.between(pl.start.Add(-3*24*time.Hour), pl.start))), []string{plans.SlugTag(pl.slug)})
		}
		for range 1 + sc.rng.Intn(4) {
			sc.agentNote(p, pl.slug, pl.a, sc.snap(p, later(p.from, sc.between(pl.start.Add(-2*24*time.Hour), pl.start))))
		}
		if pl.state != "note-only" {
			if err := sc.steps(p, pl); err != nil {
				return err
			}
		}
	}
	return nil
}

// newPlan draws a plan slug no plan uses: the project's short name, a thing
// and a goal.
func (sc *scaleSeeder) newPlan(p *scaleProject) (*scalePlan, error) {
	short := strings.TrimPrefix(p.spec.slug, "example-")
	for range 40 {
		s := sc.slots(p, "", sc.now)
		goal := scalePlanGoals[sc.rng.Intn(len(scalePlanGoals))]
		slug := short + "-" + s["{as}"] + "-" + goal.slug
		if sc.planSlugs[slug] {
			continue
		}
		sc.planSlugs[slug] = true
		return &scalePlan{slug: slug, a: s["{a}"], b: s["{b}"], phrase: goal.phrase}, nil
	}
	return nil, fmt.Errorf("project %s: no free plan slug", p.spec.slug)
}

// steps draws a plan's steps: three to seven of scaleStepTitles in order, each
// blocked by the one before. A shipped plan closed them all; a moving one
// closed the first and moved within days; a stalled one stopped weeks ago.
func (sc *scaleSeeder) steps(p *scaleProject, pl *scalePlan) error {
	n := 3 + sc.rng.Intn(5)
	idx := sc.rng.Perm(len(scaleStepTitles))[:n]
	slices.Sort(idx)
	s := sc.slots(p, pl.a, pl.start)
	s["{b}"] = pl.b
	created := pl.start.Add(time.Duration(5+sc.rng.Intn(20)) * time.Minute)
	var closeUntil, lastClose time.Time
	closed := 0
	switch pl.state {
	case "shipped":
		closed, lastClose = n, later(created.Add(time.Hour), earlier(p.to.Add(-3*24*time.Hour), created.Add(time.Duration(5+sc.rng.Intn(25))*24*time.Hour)))
	case "moving":
		closed, lastClose = 1+sc.rng.Intn(n-1), sc.between(p.to.Add(-5*24*time.Hour), p.to.Add(-time.Hour))
	case "stalled":
		closed, lastClose = 1+sc.rng.Intn(n-1), sc.between(later(created.Add(time.Hour), p.to.Add(-40*24*time.Hour)), p.to.Add(-12*24*time.Hour))
	}
	closeUntil = later(created.Add(time.Hour), lastClose)
	wrote := p.moment(created)
	var prev *scaleTask
	times := make([]time.Time, closed)
	for i := range times {
		times[i] = sc.between(created.Add(time.Hour), closeUntil)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	if closed > 0 {
		times[closed-1] = closeUntil
	}
	for i, ti := range idx {
		t := &scaleTask{wrote: wrote, task: core.Task{
			ProjectSlug: p.spec.slug, PlanSlug: pl.slug, Title: s.fill(scaleStepTitles[ti]),
			Body: capFirst(sc.slots(p, pl.a, created).fill(sc.pick(scaleSentences))), Status: core.TaskOpen,
			CreatedAt: created, UpdatedAt: created,
		}}
		if prev != nil {
			t.deps = []*scaleTask{prev}
		}
		if i < closed {
			t.close, t.closed = core.TaskDone, times[i]
			if sc.rng.Intn(10) == 0 && i > 0 {
				t.close = core.TaskDropped
			}
		}
		p.tasks = append(p.tasks, t)
		prev = t
	}
	return nil
}

// note plans one note of p written at at.
func (sc *scaleSeeder) note(p *scaleProject, slug, title, desc, body string, tags []string, at time.Time) *scaleNote {
	base := slug
	for i := 2; p.slugs[slug]; i++ {
		slug = base + "-" + strconv.Itoa(i)
	}
	p.slugs[slug] = true
	n := &scaleNote{wrote: p.moment(at), note: core.Note{
		Title: title, Slug: slug, Description: desc, Project: p.spec.slug, Body: body, Tags: tags, Created: at, Updated: at,
	}}
	p.notes = append(p.notes, n)
	return n
}

// agentNote plans a working copy of a planning subagent's run: the cc-agent
// note the capture hook writes, under plan when it is not empty.
func (sc *scaleSeeder) agentNote(p *scaleProject, plan, topic string, at time.Time) {
	agent := sc.pick(scaleAgentTypes)
	s := sc.slots(p, topic, at)
	prompt := s.fill("Survey what the {a} does when {cond}, and what the {b} needs from it; report the options with their cost.")
	agentID := fmt.Sprintf("%08x%08x", sc.rng.Uint32(), sc.rng.Uint32())
	var tags []string
	if plan != "" {
		tags = append(tags, plans.SlugTag(plan))
	}
	tags = append(tags, plans.TagAgent, "agent:"+agent, "created-by:agent")
	title := "[" + agent + "] " + capFirst(s.fill("survey the {a} and the {b}"))
	desc := "Cached planning-subagent run (" + agent + ") -- prompt + final report"
	body := fmt.Sprintf("> captured from SESSION | agent %s | git %07x | %s\n\n## Prompt\n\n%s\n\n## Report\n\n%s\n\n%s\n",
		agentID, sc.rng.Uint32()&0xfffffff, at.UTC().Format(time.RFC3339), prompt,
		sc.paragraph(p, topic, at, 3+sc.rng.Intn(4)), sc.paragraph(p, topic, at, 2+sc.rng.Intn(3)))
	n := sc.note(p, plans.AgentNotePrefix+agentID, title, desc, body, tags, at)
	n.agent, n.prompt = agent, prompt
}

// ordinaryNotes fills p's note quota after its plans: about one in six a
// working copy with no plan yet, the rest notes of every kind.
func (sc *scaleSeeder) ordinaryNotes(p *scaleProject) {
	for len(p.notes) < p.spec.notes {
		at := sc.at(p)
		if p.spec.slug != "" && sc.rng.Intn(6) == 0 {
			sc.agentNote(p, "", "", at)
			continue
		}
		var tags []string
		if sc.rng.Intn(10) < 7 {
			tags = []string{"created-by:agent"}
		}
		n := sc.ordinaryNote(p, scaleNoteKinds[sc.rng.Intn(len(scaleNoteKinds))], "", at, tags)
		if sc.rng.Intn(10) < 3 {
			if edit := sc.snap(p, sc.between(at, p.to)); edit.After(at) {
				n.edited = p.moment(edit)
				n.note.Updated = edit
			}
		}
	}
}

// ordinaryNote plans one note of a kind, its kind's tag first.
func (sc *scaleSeeder) ordinaryNote(p *scaleProject, k scaleNoteKind, topic string, at time.Time, tags []string) *scaleNote {
	s := sc.slots(p, topic, at)
	title := s.fill(k.title)
	desc := capFirst(s.fill(k.desc))
	if len(desc) > scaleMaxDesc {
		desc = capFirst(s.fill("Working notes on the {a}."))
	}
	sections := len(k.sections)
	long := sc.rng.Intn(30) == 0 // a long one now and then
	var b strings.Builder
	b.WriteString(desc + "\n")
	for i := range sections {
		fmt.Fprintf(&b, "\n## %s\n\n", k.sections[i])
		reps := 1
		if long {
			reps = 4 + sc.rng.Intn(4)
		}
		for r := range reps {
			if r > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(sc.paragraph(p, s["{a}"], at, 2+sc.rng.Intn(5)))
		}
		switch sc.rng.Intn(6) {
		case 0:
			b.WriteString("\n")
			for range 2 + sc.rng.Intn(4) {
				b.WriteString("\n- " + sc.slots(p, s["{a}"], at).fill(sc.pick(scaleBullets)))
			}
		case 1:
			b.WriteString("\n\n| Option | Cost | Risk |\n|---|---|---|\n")
			for range 2 + sc.rng.Intn(3) {
				o := sc.slots(p, s["{a}"], at)
				fmt.Fprintf(&b, "| %s | %s | %s |\n", o.fill("keep the {b}"), sc.pick([]string{"low", "medium", "high"}), o.fill("the {c} {v} the {a}"))
			}
		}
		b.WriteString("\n")
	}
	return sc.note(p, core.Slugify(title), title, desc, b.String(), append([]string{k.tag}, tags...), at)
}

// offPlanTasks draws p's off-plan tasks: most closed, the rest open.
func (sc *scaleSeeder) offPlanTasks(p *scaleProject) {
	for range p.spec.tasks {
		created := sc.at(p)
		s := sc.slots(p, "", created)
		t := &scaleTask{wrote: p.moment(created), task: core.Task{
			ProjectSlug: p.spec.slug, Title: s.fill(sc.pick(scaleTaskTitles)),
			Body:   capFirst(sc.slots(p, s["{a}"], created).fill(sc.pick(scaleSentences))),
			Status: core.TaskOpen, CreatedAt: created, UpdatedAt: created,
		}}
		switch r := sc.rng.Intn(100); {
		case r < 55:
			t.close = core.TaskDone
		case r < 65:
			t.close = core.TaskDropped
		}
		if t.close != "" {
			t.closed = sc.between(created.Add(30*time.Minute), later(created.Add(30*time.Minute), earlier(p.to, created.Add(20*24*time.Hour))))
		}
		p.tasks = append(p.tasks, t)
	}
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

func (sc *scaleSeeder) writeProjects() error {
	for _, p := range sc.projects {
		if !p.isNew {
			continue
		}
		id, err := sc.id(p.created)
		if err != nil {
			return err
		}
		if err := store.CreateProject(sc.ctx, sc.s.db, core.Project{
			ID: id, Slug: p.spec.slug, Name: p.spec.slug, Description: p.spec.desc, CreatedAt: p.created, UpdatedAt: p.created,
		}); err != nil {
			return fmt.Errorf("project %s: %w", p.spec.slug, err)
		}
		sc.fx.Added.Projects++
	}
	return nil
}

// writeSessions groups each project's moments into sessions -- a session
// takes the writes of up to three hours and twelve writes -- and creates them,
// each with its start and end events.
func (sc *scaleSeeder) writeSessions() error {
	for _, p := range sc.projects {
		slices.SortStableFunc(p.moments, func(a, b *scaleMoment) int { return a.at.Compare(b.at) })
		var cur *core.Session
		var group []*scaleMoment
		flush := func() error {
			if cur == nil {
				return nil
			}
			end := group[len(group)-1].at.Add(time.Duration(2+sc.rng.Intn(11)) * time.Minute)
			if limit := sc.now.Add(-time.Minute); end.After(limit) {
				end = limit
			}
			cur.UpdatedAt = end
			return sc.session(p, cur)
		}
		for _, m := range p.moments {
			if cur == nil || m.at.Sub(cur.CreatedAt) > 3*time.Hour || len(group) >= 12 {
				if err := flush(); err != nil {
					return err
				}
				start := m.at.Add(-time.Duration(4+sc.rng.Intn(22)) * time.Minute)
				id, err := sc.id(start)
				if err != nil {
					return err
				}
				cur = &core.Session{
					ID: id, Name: sc.sessionName(), ProjectSlug: p.spec.slug, Status: core.SessionCompleted,
					CWD: "/home/dev/" + cmp.Or(p.spec.slug, "notes"), Source: "explicit", Model: sc.pick(scaleModels),
					CreatedAt: start,
				}
				group = group[:0]
			}
			m.sess = cur
			group = append(group, m)
		}
		if err := flush(); err != nil {
			return err
		}
	}
	return nil
}

// sessionName is a session display name no other session has.
func (sc *scaleSeeder) sessionName() string {
	for {
		n := fmt.Sprintf("cc/%08x", sc.rng.Uint32())
		if !sc.s.used[n] {
			sc.s.used[n] = true
			return n
		}
	}
}

func (sc *scaleSeeder) session(p *scaleProject, sess *core.Session) error {
	sess.Findings = capFirst(sc.slots(p, "", sess.CreatedAt).fill(sc.pick(scaleFindings)))
	if err := store.CreateSession(sc.ctx, sc.s.db, *sess); err != nil {
		return fmt.Errorf("session %s: %w", sess.Name, err)
	}
	if err := sc.event(sess.CreatedAt, core.EventSessionStarted, sess.ID, p.spec.slug, "", map[string]any{
		"name": sess.Name, "ambient": false, "source": sess.Source,
	}); err != nil {
		return err
	}
	if err := sc.event(sess.UpdatedAt, core.EventSessionEnded, sess.ID, p.spec.slug, "", map[string]any{
		"findings": sess.Findings, "claims_released": 0,
	}); err != nil {
		return err
	}
	sc.fx.Added.Sessions++
	return nil
}

func (sc *scaleSeeder) event(t time.Time, kind core.EventKind, sessionID, project, itemID string, payload map[string]any) error {
	if _, err := sc.s.rec.Record(sc.ctx, core.Event{
		TS: t.UTC(), Kind: kind, SessionID: sessionID, ProjectSlug: project, ItemID: itemID, Payload: payload,
	}); err != nil {
		return fmt.Errorf("record %s: %w", kind, err)
	}
	return nil
}

// writeMemory writes one planned memory as its session wrote it, and its
// memory.written events.
func (sc *scaleSeeder) writeMemory(m *scaleMem) error {
	id, err := sc.id(m.mem.Created)
	if err != nil {
		return err
	}
	m.mem.ID, m.mem.SourceSession, m.mem.Model = id, m.wrote.sess.ID, m.wrote.sess.Model
	if m.edited != nil {
		m.mem.Model = m.edited.sess.Model
	}
	written, err := sc.s.mgr.WriteMemory(sc.ctx, m.mem)
	if err != nil {
		return fmt.Errorf("memory %s/%s: %w", m.mem.Project, m.mem.Name, err)
	}
	m.mem = written
	sc.fx.added.memories[id] = true
	payload := func() map[string]any {
		return map[string]any{"name": m.mem.Name, "kind": string(m.mem.Kind), "project": m.mem.Project}
	}
	if err := sc.event(m.mem.Created, core.EventMemoryWritten, m.wrote.sess.ID, m.mem.Project, id, payload()); err != nil {
		return err
	}
	if m.edited != nil {
		if err := sc.event(m.mem.Updated, core.EventMemoryWritten, m.edited.sess.ID, m.mem.Project, id, payload()); err != nil {
			return err
		}
	}
	return nil
}

func (sc *scaleSeeder) writeMemories() error {
	for _, p := range sc.projects {
		for _, m := range p.mems {
			if err := sc.writeMemory(m); err != nil {
				return err
			}
			sc.fx.Added.Memories++
		}
	}
	return nil
}

// writeOlds writes the superseded and archived memories, each first as it
// was written and then retired through lifecycle under the file's lock, as
// memory_write's supersedes and the gardener's archive do.
func (sc *scaleSeeder) writeOlds() error {
	for _, p := range sc.projects {
		for _, m := range p.olds {
			if err := sc.writeMemory(m); err != nil {
				return err
			}
			err := sc.s.mgr.Mutate(sc.ctx, m.mem.FilePath, func(ctx context.Context) error {
				full, err := sc.s.mgr.Store().ReadMemory(m.mem.FilePath)
				if err != nil {
					return err
				}
				if m.successor != nil {
					m.mem, err = lifecycle.Supersede(ctx, sc.s.mgr, full, m.successor.mem, m.successor.mem.Created)
				} else {
					m.mem, err = lifecycle.Archive(ctx, sc.s.mgr, full, "gardener staleness", m.archivedAt)
				}
				return err
			})
			if err != nil {
				return fmt.Errorf("retire %s/%s: %w", p.spec.slug, m.mem.Name, err)
			}
			if m.successor != nil {
				err = sc.event(m.successor.mem.Created, core.EventMemorySuperseded, m.successor.wrote.sess.ID, p.spec.slug, m.mem.ID,
					map[string]any{"name": m.mem.Name, "superseded_by": m.successor.mem.ID})
				sc.fx.Added.Superseded++
			} else {
				err = sc.event(m.archivedAt, core.EventMemoryArchived, "", p.spec.slug, m.mem.ID, map[string]any{"name": m.mem.Name, "by": "gardener"})
				sc.fx.Added.Archived++
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (sc *scaleSeeder) writeNotes() error {
	for _, p := range sc.projects {
		for _, n := range p.notes {
			id, err := sc.id(n.note.Created)
			if err != nil {
				return err
			}
			n.note.ID, n.note.Model = id, n.wrote.sess.Model
			if n.edited != nil {
				n.note.Model = n.edited.sess.Model
			}
			n.note.Body = strings.Replace(n.note.Body, "SESSION", n.wrote.sess.Name, 1)
			written, err := sc.s.mgr.WriteNote(sc.ctx, n.note)
			if err != nil {
				return fmt.Errorf("note %s/%s: %w", p.spec.slug, n.note.Slug, err)
			}
			n.note = written
			sc.fx.added.notes[id] = true
			payload := map[string]any{"title": n.note.Title, "slug": n.note.Slug}
			if err := sc.event(n.note.Created, core.EventNoteWritten, n.wrote.sess.ID, p.spec.slug, id, payload); err != nil {
				return err
			}
			if n.edited != nil {
				if err := sc.event(n.note.Updated, core.EventNoteWritten, n.edited.sess.ID, p.spec.slug, id, payload); err != nil {
					return err
				}
			}
			if n.agent != "" {
				if err := sc.event(n.note.Created.Add(2*time.Second), core.EventSubagentCaptured, "", p.spec.slug, id, map[string]any{
					"agent_type": n.agent, "prompt": n.prompt, "content": n.note.Description,
				}); err != nil {
					return err
				}
				sc.fx.Added.AgentNotes++
			}
			if plans.SlugFromTags(n.note.Tags) != "" {
				sc.fx.Added.PlanNotes++
			}
			sc.fx.Added.Notes++
		}
	}
	return nil
}

// writeTasks creates every planned task, blockers first (a plan's steps are
// planned in order), and closes the closed ones at their time.
func (sc *scaleSeeder) writeTasks() error {
	for _, p := range sc.projects {
		for _, t := range p.tasks {
			id, err := sc.id(t.task.CreatedAt)
			if err != nil {
				return err
			}
			t.task.ID, t.task.CreatedBy = id, t.wrote.sess.Name
			for _, d := range t.deps {
				t.task.DependsOn = append(t.task.DependsOn, d.task.ID)
			}
			if err := store.CreateTask(sc.ctx, sc.s.db, t.task); err != nil {
				return fmt.Errorf("task %q: %w", t.task.Title, err)
			}
			if t.task.PlanSlug != "" {
				sc.fx.added.plans[p.spec.slug+"/"+t.task.PlanSlug] = true
			}
			if t.close != "" {
				status := t.close
				if _, err := store.UpdateTask(sc.ctx, sc.s.db, id, store.TaskPatch{Status: &status}, "", t.closed); err != nil {
					return fmt.Errorf("close %q: %w", t.task.Title, err)
				}
				if err := sc.event(t.closed, core.EventTaskTransition, "", p.spec.slug, id, map[string]any{
					"from": string(core.TaskOpen), "to": string(status), "title": t.task.Title,
				}); err != nil {
					return err
				}
			}
			sc.fx.Added.Tasks++
		}
	}
	return nil
}

// marks stars the new projects the spec stars, through the favorites call
// the review layer uses, and fences ScaleLocked confidential: the state a
// lock needs (seamlessd serve refuses to lock an open project).
func (sc *scaleSeeder) marks() error {
	for _, p := range sc.projects {
		if p.spec.starred {
			if err := store.SetProjectFavorite(sc.ctx, sc.s.db, p.spec.slug, true); err != nil {
				return fmt.Errorf("star %s: %w", p.spec.slug, err)
			}
		}
		if p.spec.confidential {
			if _, err := store.TightenProjectIsolation(sc.ctx, sc.s.db, p.spec.slug, core.IsolationConfidential, p.created.Add(24*time.Hour)); err != nil {
				return fmt.Errorf("fence %s: %w", p.spec.slug, err)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the summary
// ---------------------------------------------------------------------------

// summarize counts the whole seed after the layer, the way the console counts
// it, and picks the pages to measure.
func (sc *scaleSeeder) summarize() error {
	ctx, db, now := sc.ctx, sc.s.db, sc.now
	t := &sc.fx.Total
	count := func(dst *int, query string, args ...any) error {
		if err := db.QueryRowContext(ctx, query, args...).Scan(dst); err != nil {
			return fmt.Errorf("count: %w", err)
		}
		return nil
	}
	ago := func(d time.Duration) string { return core.FormatTime(now.Add(-d)) }
	day := 24 * time.Hour
	for _, c := range []struct {
		dst   *int
		query string
		args  []any
	}{
		{&t.Memories, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL`, nil},
		{&t.Superseded, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NOT NULL AND COALESCE(superseded_by, '') <> ''`, nil},
		{&t.Archived, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NOT NULL AND COALESCE(superseded_by, '') = ''`, nil},
		{&t.Notes, `SELECT COUNT(*) FROM notes_index`, nil},
		{&t.PlanNotes, `SELECT COUNT(*) FROM notes_index WHERE tags LIKE '%"plan:%'`, nil},
		{&t.AgentNotes, `SELECT COUNT(*) FROM notes_index WHERE tags LIKE '%"` + plans.TagAgent + `"%'`, nil},
		{&t.Sessions, `SELECT COUNT(*) FROM sessions`, nil},
		{&t.Tasks, `SELECT COUNT(*) FROM tasks`, nil},
		{&t.OpenTasks, `SELECT COUNT(*) FROM tasks WHERE status IN ('open', 'in_progress')`, nil},
		{&sc.fx.Today, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL AND updated_at >= ?`, []any{ago(day)}},
		{&sc.fx.Week, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL AND updated_at >= ?`, []any{ago(7 * day)}},
		{&sc.fx.Month, `SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL AND updated_at >= ?`, []any{ago(30 * day)}},
	} {
		if err := count(c.dst, c.query, c.args...); err != nil {
			return err
		}
	}
	var oldest string
	if err := db.QueryRowContext(ctx, `SELECT MIN(updated_at) FROM memories_index WHERE invalid_at IS NULL`).Scan(&oldest); err != nil {
		return fmt.Errorf("oldest: %w", err)
	}
	oldestAt, err := core.ParseTime(oldest)
	if err != nil {
		return fmt.Errorf("oldest: %w", err)
	}
	sc.fx.Oldest = int(now.Sub(oldestAt) / day)
	if err := sc.kinds(); err != nil {
		return err
	}
	if err := sc.bands(); err != nil {
		return err
	}
	sc.fx.Locked = ScaleLocked
	sc.fx.LockConfig = "peer_auth: {force_locked: [" + ScaleLocked + "]}"
	return sc.pages()
}

func (sc *scaleSeeder) kinds() error {
	rows, err := sc.s.db.QueryContext(sc.ctx, `SELECT kind, COUNT(*) FROM memories_index WHERE invalid_at IS NULL GROUP BY kind ORDER BY COUNT(*) DESC, kind`)
	if err != nil {
		return fmt.Errorf("kinds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k ScaleKind
		if err := rows.Scan(&k.Kind, &k.N); err != nil {
			return fmt.Errorf("kinds: %w", err)
		}
		sc.fx.Kinds = append(sc.fx.Kinds, k)
	}
	return rows.Err()
}

// bands sorts every project into the Projects screen's bands
// (console.activityGroups): retired; active with a live agent or a session or
// task change in the last 30 days; quiet otherwise.
func (sc *scaleSeeder) bands() error {
	projects, err := store.ListProjects(sc.ctx, sc.s.db)
	if err != nil {
		return fmt.Errorf("projects: %w", err)
	}
	board, err := store.ProjectsWithCounts(sc.ctx, sc.s.db, store.ResolveRetrievalWindow("30d", sc.now), sc.now, 0)
	if err != nil {
		return fmt.Errorf("board: %w", err)
	}
	rows := map[string]store.ProjectBoardRow{}
	for _, r := range board {
		rows[r.Project] = r
	}
	t := &sc.fx.Total
	for _, p := range projects {
		t.Projects++
		if p.Favorite {
			sc.fx.Starred = append(sc.fx.Starred, p.Slug)
		}
		if p.Isolation == core.IsolationConfidential {
			t.Confidential++
		}
		r := rows[p.Slug]
		last := later(r.LastActive, r.LastTask)
		switch {
		case p.Retired():
			t.Retired++
		case r.LiveSessions > 0 || (!last.IsZero() && sc.now.Sub(last) <= 30*24*time.Hour):
			t.Active++
		default:
			t.Quiet++
		}
	}
	slices.Sort(sc.fx.Starred)
	return nil
}

// pages picks the pages to measure: the newest active memory and plan note of
// the biggest new project, and the three lists.
func (sc *scaleSeeder) pages() error {
	var big *scaleProject
	for _, p := range sc.projects {
		if p.isNew && (big == nil || p.spec.memories > big.spec.memories) {
			big = p
		}
	}
	if big == nil || len(big.mems) == 0 {
		return errors.New("pages: no new project with memories")
	}
	mem := slices.MaxFunc(big.mems, func(a, b *scaleMem) int { return a.mem.Updated.Compare(b.mem.Updated) })
	var note *scaleNote
	for _, n := range big.notes {
		if strings.HasPrefix(n.note.Slug, "plan-") && (note == nil || n.note.Updated.After(note.note.Updated)) {
			note = n
		}
	}
	if note == nil {
		return errors.New("pages: no plan note in " + big.spec.slug)
	}
	sc.fx.Pages = []ScalePage{
		{"memory page", "/console/memories/" + mem.mem.ID},
		{"note page", "/console/notes/" + note.note.ID},
		{"memories list", "/console/memories"},
		{"notes list", "/console/notes"},
		{"tasks list", "/console/tasks"},
	}
	return nil
}
