package demokit

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// fxSeeded is one seed the scale test made: its dir, its seeder and the maps
// it printed.
type fxSeeded struct {
	dir    string
	s      *Seeder
	review ReviewFixture
	scale  ScaleFixture
}

// fxSeed seeds a fresh dir at reviewTestAnchor: the review fixture, and the
// scale layer on top at volume percent of the owner's scale (none at 0).
func fxSeed(dir string, volume int) (fxSeeded, error) {
	s, err := New(dir)
	if err != nil {
		return fxSeeded{}, err
	}
	s.now = reviewTestAnchor
	out := fxSeeded{dir: dir, s: s}
	if volume > 0 {
		out.review, out.scale, err = s.seedReviewScale(volume)
	} else {
		out.review, err = s.SeedReview()
	}
	return out, err
}

// fxSeedAll seeds one fresh dir per volume, side by side, and closes each
// seeder when the test ends.
func fxSeedAll(t *testing.T, volumes ...int) []fxSeeded {
	t.Helper()
	seeds := make([]fxSeeded, len(volumes))
	errs := make([]error, len(volumes))
	var wg sync.WaitGroup
	began := time.Now()
	for i, v := range volumes {
		dir := t.TempDir()
		wg.Go(func() { seeds[i], errs[i] = fxSeed(dir, v) })
	}
	wg.Wait()
	t.Logf("seeded %d fixtures (volumes %v) in %s", len(volumes), volumes, time.Since(began).Round(time.Second))
	for i := range seeds {
		if seeds[i].s != nil {
			t.Cleanup(func() { require.NoError(t, seeds[i].s.Close()) })
		}
		require.NoError(t, errs[i], "seed %d", i)
	}
	return seeds
}

// fxShapeVolume is the shape test's volume: a quarter of the owner's scale,
// so every project still gets items, plans and retired memories, with the
// owner's-scale counts checked from the specs against what it seeded.
const fxShapeVolume = 25

// fxPlanned is what the layer plans at volume percent: its active memories,
// its retired ones (the most it may retire: a draw with no successor free
// skips one), and its notes before the plans' extra notes.
func fxPlanned(volume int) (memories, retired, notes int) {
	sc := &scaleSeeder{volume: volume}
	for _, spec := range slices.Concat(scaleGrownProjects, scaleNewProjects) {
		spec = sc.scaled(spec)
		memories += spec.memories
		retired += spec.memories*scaleSupersededPct/100 + spec.memories*scaleArchivedPct/100
		notes += spec.notes
	}
	for i := range reviewQuiet {
		m := quietMemories(i)
		memories += m
		retired += m*scaleSupersededPct/100 + m*scaleArchivedPct/100
		notes += quietNotes(i)
	}
	return memories, retired, notes
}

// fxDeterministicVolume is the determinism test's volume: every code path of
// the layer, at an eighth of its files. Each file costs the files layer an
// F_FULLFSYNC on macOS (about 10 ms) and two seeds do not overlap them, so the
// pair at full volume took one to two and a half minutes before -race (it
// passed there too, run by hand once).
const fxDeterministicVolume = 12

// fxUnstableEvents are the event kinds no two review seeds share: the held
// write's, which screening stamps with the wall clock, and the lock
// declaration's, whose item is an id the store mints from crypto randomness.
var fxUnstableEvents = []string{"'session.tainted'", "'memory.held'", "'memory.screened'", "'" + string(core.EventRepoLockChanged) + "'"}

// fxULID matches a ULID.
var fxULID = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)

// fxStable is a review summary with the two ids the review layer does not
// draw from its seeded RNG -- the held write's (screening mints it) and the
// lock declaration's (the store mints it) -- masked.
func fxStable(summary string) string {
	lines := strings.Split(summary, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "  "+string(ReviewHeldWrite)+" ") || strings.HasPrefix(line, "  "+string(ReviewLockRequested)+" ") {
			lines[i] = fxULID.ReplaceAllString(line, "<id>")
		}
	}
	return strings.Join(lines, "\n")
}

// fxRowQueries are the rows that matter: what the console and the agents read.
var fxRowQueries = []string{
	`SELECT * FROM projects`,
	`SELECT * FROM sessions`,
	`SELECT * FROM tasks`,
	`SELECT * FROM task_deps`,
	`SELECT * FROM memories_index`,
	`SELECT * FROM notes_index`,
	`SELECT * FROM session_usage`,
	`SELECT ts, kind, session_id, project_slug, item_id, payload FROM events WHERE kind NOT IN (` + strings.Join(fxUnstableEvents, ", ") + `)`,
}

// fxCorpus reads every file under dir's memory and notes trees, by path.
func fxCorpus(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tree := range []string{"memory", "notes"} {
		err := filepath.WalkDir(filepath.Join(dir, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(raw)
			return nil
		})
		require.NoError(t, err)
	}
	return out
}

// TestFxScaleDeterministic seeds the scale layer twice at one anchor, into two
// fresh dirs, and the review fixture alone into a third. The two scale seeds
// must match file for file and row for row, and the review layer must come
// out of the scale seed unchanged.
func TestFxScaleDeterministic(t *testing.T) {
	seeds := fxSeedAll(t, fxDeterministicVolume, fxDeterministicVolume, 0)
	a, b, reviewOnly := seeds[0], seeds[1], seeds[2]

	t.Run("every corpus file is byte-identical", func(t *testing.T) {
		ca, cb := fxCorpus(t, a.dir), fxCorpus(t, b.dir)
		require.Greater(t, len(ca), len(fxCorpus(t, reviewOnly.dir))+300, "the layer's files are there")
		pa, pb := fxKeys(ca), fxKeys(cb)
		require.Equal(t, pa, pb, "the same files")
		var differ []string
		for _, p := range pa {
			if ca[p] != cb[p] {
				differ = append(differ, p)
			}
		}
		require.Empty(t, differ, "files whose bytes differ between two seeds")
	})
	t.Run("the rows that matter are equal", func(t *testing.T) {
		for _, q := range fxRowQueries {
			ra, rb := reviewRows(t, a.s, q), reviewRows(t, b.s, q)
			require.NotEmpty(t, ra, q)
			require.Equal(t, len(ra), len(rb), q)
			differ := 0
			for row, n := range ra {
				if rb[row] != n {
					differ++
				}
			}
			require.Zero(t, differ, "%s: %d rows differ between two seeds", q, differ)
		}
	})
	t.Run("the summary is the same", func(t *testing.T) {
		require.Equal(t, a.scale.Summary(), b.scale.Summary())
		require.Equal(t, fxStable(a.review.Summary()), fxStable(b.review.Summary()))
	})
	t.Run("the review layer comes out unchanged", func(t *testing.T) {
		require.Equal(t, fxStable(reviewOnly.review.Summary()), fxStable(a.review.Summary()), "-review -scale prints the -review summary first, as -review does")
		for _, q := range fxRowQueries {
			want, got := reviewRows(t, reviewOnly.s, q), reviewRows(t, a.s, q)
			missing := 0
			for row, n := range want {
				if got[row] < n {
					missing++
				}
			}
			require.Zero(t, missing, "%s: %d review rows changed under the scale layer", q, missing)
		}
	})
}

// TestFxScale seeds the scale fixture at fxShapeVolume and checks its shape:
// the owner's-scale counts the full volume reaches, the spread of projects and
// kinds, the decay, the quiet projects, the stars and the project a lock
// needs, real successor links, plans and working copies, and every file
// parsing back into the same index. (A full-volume seed under -race took four
// minutes beside other gates; demoseed -review -scale is that seed.)
func TestFxScale(t *testing.T) {
	a := fxSeedAll(t, fxShapeVolume)[0]
	ctx, db, now := a.s.ctx, a.s.db, reviewTestAnchor
	added := a.scale.added

	t.Run("the layer seeds what it plans", func(t *testing.T) {
		memories, retired, notes := fxPlanned(fxShapeVolume)
		got := a.scale.Added
		require.Equal(t, memories, got.Memories)
		require.LessOrEqual(t, got.Superseded+got.Archived, retired)
		require.Greater(t, got.Superseded+got.Archived, retired*9/10, "few retirements skipped")
		require.Greater(t, got.Superseded, got.Archived)
		require.GreaterOrEqual(t, got.Notes, notes)
		require.Equal(t, len(scaleNewProjects), got.Projects)
		require.Positive(t, got.Sessions)
		require.Positive(t, got.Tasks)
	})
	t.Run("at full volume: about 1,500 active memories, 300 retired, 700 notes, 55 projects", func(t *testing.T) {
		tot, got := a.scale.Total, a.scale.Added
		require.Equal(t, 55, tot.Projects, "the project count does not scale")
		require.Equal(t, tot.Projects, tot.Active+tot.Quiet+tot.Retired)
		memories, retired, notes := fxPlanned(100)
		require.InDelta(t, 1500, tot.Memories-got.Memories+memories, 50)
		require.InDelta(t, 300, tot.Superseded+tot.Archived-got.Superseded-got.Archived+retired, 40)
		require.InDelta(t, 700, tot.Notes-got.Notes+notes, 40)
		big := 0
		for _, spec := range slices.Concat(scaleGrownProjects, scaleNewProjects) {
			if spec.memories >= 100 {
				big++
			}
		}
		require.GreaterOrEqual(t, big, 3, "projects with 100+ memories at full volume")
	})
	t.Run("a long tail", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, `SELECT project, COUNT(*) FROM memories_index WHERE invalid_at IS NULL AND project <> '' GROUP BY project`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		big, small := 0, 0
		for rows.Next() {
			var p string
			var n int
			require.NoError(t, rows.Scan(&p, &n))
			if n >= 25 {
				big++
			}
			if n < 10 {
				small++
			}
		}
		require.NoError(t, rows.Err())
		require.GreaterOrEqual(t, big, 3, "projects with 25+ memories, 100+ at full volume")
		require.GreaterOrEqual(t, small, 30, "the long tail")
	})
	t.Run("gotcha and constraint most common, refuted rare", func(t *testing.T) {
		require.Len(t, a.scale.Kinds, len(core.MemoryKinds))
		byKind := map[core.MemoryKind]int{}
		for id := range added.memories {
			m, ok, err := store.MemoryByID(ctx, db, id)
			require.NoError(t, err)
			require.True(t, ok)
			if m.Active() {
				byKind[m.Kind]++
			}
		}
		kinds := slices.Collect(maps.Keys(byKind))
		slices.SortFunc(kinds, func(x, y core.MemoryKind) int { return byKind[y] - byKind[x] })
		require.ElementsMatch(t, []core.MemoryKind{core.KindGotcha, core.KindConstraint}, kinds[:2], "%v", byKind)
		require.Less(t, byKind[core.KindRefuted]*100, a.scale.Added.Memories*5, "refuted is under 5%%")
	})
	t.Run("updated times decay over 200 days, filling today, this week and this month", func(t *testing.T) {
		fx := a.scale
		require.Positive(t, fx.Today)
		require.LessOrEqual(t, fx.Oldest, scaleDays)
		var day, week, month, old int // the layer's own, by age
		for id := range added.memories {
			m, ok, err := store.MemoryByID(ctx, db, id)
			require.NoError(t, err)
			require.True(t, ok)
			if !m.Active() {
				continue
			}
			switch age := now.Sub(m.Updated); {
			case age <= 24*time.Hour:
				day++
				fallthrough
			case age <= 7*24*time.Hour:
				week++
				fallthrough
			case age <= 30*24*time.Hour:
				month++
			case age > 100*24*time.Hour:
				old++
			}
		}
		require.Positive(t, day, "today")
		require.Greater(t, week, 2*day, "this week")
		require.Greater(t, month, 2*week, "this month")
		require.Less(t, month, fx.Added.Memories*3/4, "a tail older than a month")
		require.Positive(t, old, "older than 100 days")
		var future int
		anchor := core.FormatTime(now)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM memories_index WHERE updated_at > ? OR created_at > ?) +
			(SELECT COUNT(*) FROM notes_index WHERE updated_at > ? OR created_at > ?) +
			(SELECT COUNT(*) FROM sessions WHERE updated_at > ?) +
			(SELECT COUNT(*) FROM tasks WHERE updated_at > ?)`,
			anchor, anchor, anchor, anchor, anchor, anchor).Scan(&future))
		require.Zero(t, future, "nothing after the anchor")
	})
	t.Run("the quiet projects stay quiet", func(t *testing.T) {
		quiet := reviewItem(t, a.review, ReviewProjectsQuiet).IDs
		board, err := store.ProjectsWithCounts(ctx, db, store.ResolveRetrievalWindow("30d", now), now, 0)
		require.NoError(t, err)
		seen := 0
		for _, row := range board {
			if !slices.Contains(quiet, row.Project) {
				continue
			}
			seen++
			last := later(row.LastActive, row.LastTask)
			require.True(t, last.Before(now.AddDate(0, 0, -30)), "%s last active %s", row.Project, last)
			require.Zero(t, row.LiveSessions, row.Project)
		}
		require.Len(t, quiet, seen)
		require.Equal(t, len(quiet)-1, a.scale.Total.Quiet, "every quiet project but the retired one is in the Quiet band")
	})
	t.Run("stars and the project a lock needs", func(t *testing.T) {
		starred := a.scale.Starred
		require.GreaterOrEqual(t, len(starred), 5)
		require.Subset(t, starred, reviewItem(t, a.review, ReviewProjectsStarred).IDs)
		p, found, err := store.ProjectBySlug(ctx, db, ScaleLocked)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, core.IsolationConfidential, p.Isolation, "serve refuses to lock an open project")
		require.Equal(t, ScaleLocked, a.scale.Locked)
		require.Contains(t, a.scale.LockConfig, "force_locked: ["+ScaleLocked+"]")
	})
	t.Run("superseded memories point at active successors; archived ones carry the tombstone", func(t *testing.T) {
		// Read the rows first: the store holds one connection, which an open
		// cursor keeps from the lookups below.
		type old struct{ project, path, by string }
		var olds []old
		func() {
			rows, err := db.QueryContext(ctx, `SELECT id, project, file_path, COALESCE(superseded_by, '') FROM memories_index WHERE invalid_at IS NOT NULL`)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				var o old
				require.NoError(t, rows.Scan(&id, &o.project, &o.path, &o.by))
				if added.memories[id] {
					olds = append(olds, o)
				}
			}
			require.NoError(t, rows.Err())
		}()
		superseded, archived := 0, 0
		for _, o := range olds {
			project, path, by := o.project, o.path, o.by
			body := fxFile(t, a.dir, path)
			if by == "" {
				archived++
				require.Contains(t, body, "> Archived (gardener staleness) on ", path)
				continue
			}
			superseded++
			succ, ok, err := store.MemoryByID(ctx, db, by)
			require.NoError(t, err)
			require.True(t, ok, "%s: successor %s", path, by)
			require.True(t, succ.Active(), "%s: successor %s is active", path, by)
			require.Equal(t, project, succ.Project, path)
			require.Contains(t, body, "> Superseded by ", path)
			require.Contains(t, body, "("+by+")", path)
		}
		require.Equal(t, a.scale.Added.Superseded, superseded)
		require.Equal(t, a.scale.Added.Archived, archived)
	})
	t.Run("plan notes, plans with steps, and agent working copies", func(t *testing.T) {
		require.GreaterOrEqual(t, a.scale.Added.PlanNotes, 25)
		require.GreaterOrEqual(t, a.scale.Added.AgentNotes, 25)
		agents, err := store.NotesByTag(ctx, db, "", plans.TagAgent)
		require.NoError(t, err)
		withPlan, withoutPlan := 0, 0
		for _, n := range agents {
			require.True(t, strings.HasPrefix(n.Slug, plans.AgentNotePrefix), n.Slug)
			if plans.SlugFromTags(n.Tags) != "" {
				withPlan++
			} else {
				withoutPlan++
			}
		}
		require.Positive(t, withPlan)
		require.Positive(t, withoutPlan)
		rollups, err := store.AllPlanRollups(ctx, db)
		require.NoError(t, err)
		shipped, open := 0, 0
		for _, r := range rollups {
			if !added.plans[r.Project+"/"+r.Slug] {
				continue // the fleet's and the review's
			}
			if r.Done == r.Total {
				shipped++
			} else {
				open++
			}
		}
		require.GreaterOrEqual(t, shipped, 2)
		require.GreaterOrEqual(t, open, 2)
	})
	t.Run("every item names a session of its project", func(t *testing.T) {
		var orphans int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories_index m
			WHERE m.source_session LIKE '01%' AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = m.source_session AND s.project_slug = m.project)`).Scan(&orphans))
		require.Zero(t, orphans, "memories whose source session is missing or in another project")
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks t
			WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.name = t.created_by)`).Scan(&orphans))
		require.Zero(t, orphans, "tasks whose creator is no session")
	})
	t.Run("every file parses and indexes back the same from scratch", func(t *testing.T) {
		corpus := fxCorpus(t, a.dir)
		for path, content := range corpus {
			if strings.HasPrefix(path, "memory/") {
				m, err := files.ParseMemory(content, path)
				require.NoError(t, err, path)
				require.NoError(t, validate.Name(m.Name), path)
				require.LessOrEqual(t, len(m.Description), scaleMaxDesc, path)
				require.Equal(t, files.MemoryRelPath(m.Project, m.Name), path)
				if m.Kind == core.KindStage && added.memories[m.ID] {
					require.True(t, strings.HasPrefix(m.Body, "Status: "), "%s: a stage opens with its status", path)
				}
				continue
			}
			n, err := files.ParseNote(content, path)
			require.NoError(t, err, path)
			require.NoError(t, validate.Title(n.Title), path)
			require.Equal(t, files.NoteRelPath(n.Project, n.Slug), path)
		}

		fresh, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, fresh.Close()) })
		var warnings bytes.Buffer
		mgr, err := files.NewManager(a.dir, fresh, slog.New(slog.NewTextHandler(&warnings, &slog.HandlerOptions{Level: slog.LevelWarn})))
		require.NoError(t, err)
		require.NoError(t, mgr.Reconcile(context.Background()))
		require.NoError(t, mgr.Close())
		require.Empty(t, warnings.String(), "reindexing the corpus warns about nothing")
		for _, q := range []string{
			`SELECT id, kind, name, project, file_path, content_hash, invalid_at IS NULL, COALESCE(superseded_by, '') FROM memories_index`,
			`SELECT id, title, slug, project, file_path, content_hash, tags FROM notes_index`,
		} {
			want := reviewRows(t, a.s, q)
			got := reviewRows(t, &Seeder{ctx: ctx, db: fresh}, q)
			require.Equal(t, want, got, q)
		}
		var indexed int
		require.NoError(t, fresh.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM memories_index) + (SELECT COUNT(*) FROM notes_index)`).Scan(&indexed))
		require.Equal(t, len(corpus), indexed, "one index row per file")
	})
	t.Run("the summary prints the counts and five pages to measure", func(t *testing.T) {
		sum := a.scale.Summary()
		for _, want := range []string{"scale: +9 projects", "totals: 55 projects", "kinds: gotcha=", "updated: ", "locked: " + ScaleLocked} {
			require.Contains(t, sum, want)
		}
		pages := a.scale.Pages
		require.Len(t, pages, 5)
		for _, p := range pages {
			require.True(t, strings.HasPrefix(p.Path, "/console/"), p.Path)
			require.Contains(t, sum, "  "+p.Name+" ")
			require.Contains(t, sum, p.Path)
		}
		memID, ok := strings.CutPrefix(pages[0].Path, "/console/memories/")
		require.True(t, ok)
		m, found, err := store.MemoryByID(ctx, db, memID)
		require.NoError(t, err)
		require.True(t, found)
		require.True(t, m.Active())
		noteID, ok := strings.CutPrefix(pages[1].Path, "/console/notes/")
		require.True(t, ok)
		n, found, err := store.NoteByID(ctx, db, noteID)
		require.NoError(t, err)
		require.True(t, found)
		require.NotEmpty(t, plans.SlugFromTags(n.Tags), "the note page is a plan's narrative")
		require.Equal(t, []string{"/console/memories", "/console/notes", "/console/tasks"},
			[]string{pages[2].Path, pages[3].Path, pages[4].Path})
	})
}

func fxKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func fxFile(t *testing.T, dir, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(raw)
}
