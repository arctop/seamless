package demokit

import (
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the daylight-saving row loads America/Los_Angeles on any runner

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/transcripts"
)

// diaryTestZone is the zone the shape tests seed in: eight hours behind UTC,
// with no daylight-saving change.
var diaryTestZone = time.FixedZone("UTC-8", -8*60*60)

// diaryTestAnchor is the shape tests' anchor: Friday 2026-11-20, 21:00.
var diaryTestAnchor = time.Date(2026, 11, 20, 21, 0, 0, 0, diaryTestZone)

// diarySeedAt seeds the Diary fixture around anchor, in loc, into a fresh
// data dir.
func diarySeedAt(t *testing.T, anchor time.Time, loc *time.Location) (*Seeder, DiaryFixture) {
	t.Helper()
	s, err := New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.now = anchor.UTC()
	fx, err := s.SeedDiary(loc)
	require.NoError(t, err)
	return s, fx
}

// diaryCount runs a COUNT query.
func diaryCount(t *testing.T, s *Seeder, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRowContext(s.ctx, query, args...).Scan(&n))
	return n
}

// diaryStrings runs a query of one text column.
func diaryStrings(t *testing.T, s *Seeder, query string, args ...any) []string {
	t.Helper()
	rows, err := s.db.QueryContext(s.ctx, query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

// diaryBounds is a local day's [start, end) in the database's time format.
func diaryBounds(day time.Time) (string, string) {
	return core.FormatTime(day), core.FormatTime(day.AddDate(0, 0, 1))
}

// diaryStarted is the ids of the sessions created on a local day.
func diaryStarted(t *testing.T, s *Seeder, day time.Time) []string {
	t.Helper()
	lo, hi := diaryBounds(day)
	return diaryStrings(t, s, `SELECT id FROM sessions WHERE created_at >= ? AND created_at < ? ORDER BY id`, lo, hi)
}

// diaryDayRows is the ledger rows whose quarter-hour starts on a local day.
func diaryDayRows(t *testing.T, s *Seeder, day time.Time) []store.UsageRow {
	t.Helper()
	rows, err := store.UsageBetween(s.ctx, s.db, day, day.AddDate(0, 0, 1))
	require.NoError(t, err)
	return rows
}

// diarySessionRows is one session's ledger rows.
func diarySessionRows(t *testing.T, s *Seeder, id string) []store.UsageRow {
	t.Helper()
	rows, err := store.SessionUsage(s.ctx, s.db, id)
	require.NoError(t, err)
	return rows
}

// diarySession is one session's row.
func diarySession(t *testing.T, s *Seeder, id string) core.Session {
	t.Helper()
	sess, found, err := store.SessionByID(s.ctx, s.db, id)
	require.NoError(t, err)
	require.True(t, found, "session %s", id)
	return sess
}

// diaryRole is the fixture's days of a role.
func diaryRole(fx DiaryFixture, role DiaryRole) []DiaryDay {
	var out []DiaryDay
	for _, day := range fx.Days {
		if day.Role == role {
			out = append(out, day)
		}
	}
	return out
}

// diaryWantDays is the fixture's shape at diaryTestAnchor, newest first: each
// covered day's role, the sessions that started on it, and its ledger rows
// (the quarter-hours that start on it). The weekday scripts land on the
// newest weekday first; 2026-11-18 holds the midnight session's start and
// 2026-11-19 its rows after midnight.
var diaryWantDays = []string{
	"2026-11-20 today 4 sessions 18 rows",
	"2026-11-19 workday 3 sessions 33 rows",
	"2026-11-18 workday 3 sessions 18 rows",
	"2026-11-17 seven-agents 6 sessions 39 rows",
	"2026-11-16 workday 3 sessions 18 rows",
	"2026-11-15 quiet-weekend 0 sessions 0 rows",
	"2026-11-14 quiet-weekend 0 sessions 0 rows",
	"2026-11-13 workday 4 sessions 24 rows",
	"2026-11-12 short-sessions 5 sessions 6 rows",
	"2026-11-11 workday 3 sessions 28 rows",
	"2026-11-10 workday 4 sessions 28 rows",
	"2026-11-09 workday 3 sessions 25 rows",
	"2026-11-08 light-weekend 1 sessions 1 rows",
	"2026-11-07 weekend 0 sessions 0 rows",
	"2026-11-06 workday 3 sessions 17 rows",
	"2026-11-05 workday 3 sessions 24 rows",
	"2026-11-04 workday 4 sessions 32 rows",
	"2026-11-03 workday 3 sessions 29 rows",
	"2026-11-02 workday 3 sessions 24 rows",
	"2026-11-01 weekend 0 sessions 0 rows",
	"2026-10-31 weekend 0 sessions 0 rows",
	"2026-10-30 pre-ledger 3 sessions 0 rows",
}

// TestSeedDiary seeds the fixture once at the fixed anchor and checks its
// shape, its link cases, its special days and its usage, then seeds it around
// other anchors and once more around the same one.
func TestSeedDiary(t *testing.T) {
	s, fx := diarySeedAt(t, diaryTestAnchor, diaryTestZone)
	t.Run("days", func(t *testing.T) { diaryCheckDays(t, s, fx) })
	t.Run("link cases", func(t *testing.T) { diaryCheckLinks(t, s, fx) })
	t.Run("special days", func(t *testing.T) { diaryCheckSpecialDays(t, s, fx) })
	t.Run("usage mix", func(t *testing.T) { diaryCheckUsageMix(t, s, fx) })
	t.Run("other anchors", func(t *testing.T) { diaryCheckOtherAnchors(t, s, fx) })
	t.Run("deterministic", func(t *testing.T) { diaryCheckDeterministic(t, s, fx) })
}

func diaryCheckDays(t *testing.T, s *Seeder, fx DiaryFixture) {
	require.Len(t, fx.Days, DiaryLedgerDays+1, "three weeks of ledger days, then the day older than the ledger")
	today := time.Date(2026, 11, 20, 0, 0, 0, 0, diaryTestZone)
	for i, day := range fx.Days[:DiaryLedgerDays] {
		require.True(t, day.Date.Equal(today.AddDate(0, 0, -i)), "day %d is %s", i, day.Date)
	}
	got := make([]string, len(fx.Days))
	for i, day := range fx.Days {
		got[i] = fmt.Sprintf("%s %s %d sessions %d rows", day.Date.Format("2006-01-02"), day.Role,
			len(diaryStarted(t, s, day.Date)), len(diaryDayRows(t, s, day.Date)))
	}
	require.Equal(t, diaryWantDays, got)

	// The map the summary prints is the database's.
	for _, day := range fx.Days {
		require.ElementsMatch(t, diaryStarted(t, s, day.Date), day.Sessions, "sessions of %s", day.Date)
		require.Len(t, diaryDayRows(t, s, day.Date), day.LedgerRows, "ledger rows of %s", day.Date)
	}
	require.Equal(t, 58, diaryCount(t, s, `SELECT COUNT(*) FROM sessions`))
	require.Len(t, fx.Sessions, 58)
}

func diaryCheckLinks(t *testing.T, s *Seeder, fx DiaryFixture) {
	ctx, db, l := s.ctx, s.db, fx.Links

	t.Run("a task deleted after it closed", func(t *testing.T) {
		got, err := store.TasksByIDs(ctx, db, []string{l.DeletedTask})
		require.NoError(t, err)
		require.Empty(t, got, "the row is gone")
		require.Equal(t, []string{"open", "in_progress", "in_progress", "done"}, diaryStrings(t, s,
			`SELECT json_extract(payload, '$.to') FROM events WHERE kind = ? AND item_id = ? ORDER BY ts`,
			core.EventTaskTransition, l.DeletedTask), "its history stays")
		require.Zero(t, diaryCount(t, s, `SELECT COUNT(*) FROM fts WHERE item_id = ?`, l.DeletedTask))
	})

	t.Run("a task-only plan", func(t *testing.T) {
		steps, err := store.ListTasksForPlan(ctx, db, l.TaskOnlyPlanProject, "", l.TaskOnlyPlan)
		require.NoError(t, err)
		require.Len(t, steps, 3)
		_, _, ok, err := plans.Composition(ctx, db, l.TaskOnlyPlan)
		require.NoError(t, err)
		require.False(t, ok, "no note carries plan:%s", l.TaskOnlyPlan)
	})

	t.Run("one plan slug in two projects", func(t *testing.T) {
		for _, project := range l.SharedSlugProjects {
			steps, err := store.ListTasksForPlan(ctx, db, project, "", l.SharedSlug)
			require.NoError(t, err)
			require.NotEmpty(t, steps, "steps of plan:%s in %s", l.SharedSlug, project)
		}
		primary, _, ok, err := plans.Composition(ctx, db, l.SharedSlug)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, l.SharedSlugProjects[0], primary.Project, "the plan's page is in the project its note is in")
		other, err := store.NotesByTag(ctx, db, l.SharedSlugProjects[1], plans.SlugTag(l.SharedSlug))
		require.NoError(t, err)
		require.Empty(t, other)
	})

	t.Run("a shipped plan", func(t *testing.T) {
		steps, err := store.ListTasksForPlan(ctx, db, l.ShippedPlanProject, "", l.ShippedPlan)
		require.NoError(t, err)
		require.Len(t, steps, 5)
		for _, step := range steps {
			require.Equal(t, core.TaskDone, step.Status, step.Title)
		}
		require.Equal(t, 1, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ? AND project_slug = ?`,
			core.EventPlanShipped, l.ShippedPlan, l.ShippedPlanProject))
		require.Equal(t, 1, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ?`, core.EventPlanShipped),
			"one plan ships")
	})

	t.Run("a superseded memory", func(t *testing.T) {
		mems, err := store.MemoriesByIDs(ctx, db, []string{l.SupersededMemory, l.SupersedingMemory})
		require.NoError(t, err)
		old, repl := mems[l.SupersededMemory], mems[l.SupersedingMemory]
		require.NotNil(t, old.InvalidAt)
		require.Equal(t, l.SupersedingMemory, old.SupersededBy)
		require.Nil(t, repl.InvalidAt)
		full, err := s.mgr.Store().ReadMemory(old.FilePath)
		require.NoError(t, err)
		require.Contains(t, full.Body, "> Superseded by "+repl.Project+"/"+repl.Name+" ("+repl.ID+")")
		require.Equal(t, 1, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ?
			AND json_extract(payload, '$.superseded_by') = ?`, core.EventMemorySuperseded, old.ID, repl.ID))
	})

	t.Run("a session in the global scope", func(t *testing.T) {
		require.Empty(t, diarySession(t, s, l.GlobalSession).ProjectSlug)
		require.NotEmpty(t, diarySessionRows(t, s, l.GlobalSession))
		require.Positive(t, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ? AND session_id = ?`,
			core.EventNoteWritten, l.GlobalSession), "it kept something")
	})

	t.Run("commit records", func(t *testing.T) {
		for owner, id := range map[store.CommitOwner]string{store.CommitOwnerTask: l.CommitTask, store.CommitOwnerSession: l.CommitSession} {
			got, err := store.RecordedCommits(ctx, db, owner, []string{id})
			require.NoError(t, err)
			require.NotEmpty(t, got[id], "%s %s", owner, id)
			for _, c := range got[id] {
				require.True(t, core.IsCommitID(c), c)
			}
		}
		require.Equal(t, 1, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ?
			AND json_extract(payload, '$.commits_recorded') = 2`, core.EventTaskTransition, l.CommitTask))
		require.Equal(t, 1, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ? AND session_id = ?
			AND json_extract(payload, '$.commits_recorded') = 1`, core.EventSessionEnded, l.CommitSession))
	})

	t.Run("a flagged session with its transcript kept", func(t *testing.T) {
		sess := diarySession(t, s, l.FlaggedSession)
		require.Equal(t, core.VerdictNotable, sess.Verdict)
		require.NotEmpty(t, sess.VerdictReason)
		require.Equal(t, core.TranscriptCopied, sess.Transcript)
		fi, err := os.Stat(filepath.Join(s.mgr.Store().DataDir(), transcripts.DirName, sess.ID+transcripts.Suffix))
		require.NoError(t, err)
		require.Positive(t, fi.Size())
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		}
		require.Equal(t, 1, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE kind = ? AND session_id = ?`,
			core.EventSessionFlagged, sess.ID))
	})

	t.Run("a self-reported mishap naming a memory", func(t *testing.T) {
		e, found, err := s.rec.ByID(ctx, l.Mishap)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, core.EventAgentMishap, e.Kind)
		named := diaryStrings(t, s, `SELECT id FROM memories_index WHERE name = 'prune-targets-staging-first'`)
		require.Equal(t, []any{named[0]}, e.Payload["item_ids"])
	})

	t.Run("a trial", func(t *testing.T) {
		got, err := store.TrialsByIDs(ctx, db, []string{l.Trial})
		require.NoError(t, err)
		require.Contains(t, got, l.Trial)
	})

	t.Run("an unreported session", func(t *testing.T) {
		sess := diarySession(t, s, l.UnreportedSession)
		require.False(t, sess.Ambient, "a client without hooks")
		require.Zero(t, sess.Tokens.Total)
		require.Empty(t, diarySessionRows(t, s, sess.ID))
		require.NotEmpty(t, sess.Findings)
	})
}

func diaryCheckSpecialDays(t *testing.T, s *Seeder, fx DiaryFixture) {
	t.Run("a quiet weekend", func(t *testing.T) {
		quiet := diaryRole(fx, DiaryQuietWeekend)
		require.Len(t, quiet, 2)
		require.Equal(t, time.Sunday, quiet[0].Date.Weekday())
		require.Equal(t, time.Saturday, quiet[1].Date.Weekday())
		for _, day := range quiet {
			lo, hi := diaryBounds(day.Date)
			require.Empty(t, diaryDayRows(t, s, day.Date))
			require.Zero(t, diaryCount(t, s, `SELECT COUNT(*) FROM sessions WHERE (created_at >= ? AND created_at < ?)
				OR (updated_at >= ? AND updated_at < ?)`, lo, hi, lo, hi))
			require.Zero(t, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE ts >= ? AND ts < ?`, lo, hi))
		}
	})

	t.Run("the day older than the ledger", func(t *testing.T) {
		pre := diaryRole(fx, DiaryPreLedger)
		require.Len(t, pre, 1)
		require.Equal(t, pre[0], fx.Days[len(fx.Days)-1])
		require.Len(t, pre[0].Sessions, 3)
		for _, id := range pre[0].Sessions {
			require.Positive(t, diarySession(t, s, id).Tokens.Total, "stored totals")
			require.Empty(t, diarySessionRows(t, s, id), "no ledger rows")
		}
		first := diaryStrings(t, s, `SELECT MIN(bucket) FROM session_usage`)
		require.Greater(t, first[0], core.FormatTime(pre[0].Date.AddDate(0, 0, 1)), "the ledger starts after it")
	})

	t.Run("a day of only short sessions", func(t *testing.T) {
		short := diaryRole(fx, DiaryShortDay)
		require.Len(t, short, 1)
		require.Len(t, short[0].Sessions, 5)
		for _, id := range short[0].Sessions {
			var active, output int
			for _, r := range diarySessionRows(t, s, id) {
				active += bits.OnesCount(uint(r.MinuteMask))
				output += r.Output
			}
			require.Positive(t, active)
			require.Less(t, active, 10, "under 10 active minutes")
			require.Less(t, output, 4000, "under 4k output tokens")
			require.Zero(t, diaryCount(t, s, `SELECT COUNT(*) FROM events WHERE session_id = ? AND kind IN
				('task.transition', 'memory.written', 'memory.superseded', 'note.written', 'trial.recorded',
				 'plan.captured', 'plan.presented', 'plan.approved', 'plan.shipped', 'session.flagged', 'agent.mishap')`, id),
				"no outcome")
			require.True(t, strings.HasPrefix(diarySession(t, s, id).Findings, harvest.Prefix), "only harvested findings")
		}
	})

	t.Run("a day of seven agents", func(t *testing.T) {
		many := diaryRole(fx, DiaryManyAgents)
		require.Len(t, many, 1)
		lo, hi := diaryBounds(many[0].Date)
		agents := `SELECT DISTINCT s.external_client || '/' || u.model FROM session_usage u
			JOIN sessions s ON s.id = u.session_id WHERE u.bucket >= ? AND u.bucket < ? ORDER BY 1`
		require.Equal(t, []string{
			clientClaude + "/" + modelFable, clientClaude + "/" + modelHaiku, clientClaude + "/" + modelOpus,
			clientClaude + "/" + modelSonnet, clientCodex + "/" + modelGPT, clientCodex + "/" + modelGPTMini,
			clientCodex + "/" + modelGPTNext,
		}, diaryStrings(t, s, agents, lo, hi), "seven agents, each a harness and a model")
		for _, day := range fx.Days {
			if day.Role == DiaryManyAgents {
				continue
			}
			lo, hi := diaryBounds(day.Date)
			require.LessOrEqual(t, len(diaryStrings(t, s, agents, lo, hi)), 3, "only the one day has more than three agents: %s", day.Date)
		}
	})

	t.Run("a session spanning midnight", func(t *testing.T) {
		sess := diarySession(t, s, fx.Links.MidnightSession)
		start, end := sess.CreatedAt.In(diaryTestZone), sess.UpdatedAt.In(diaryTestZone)
		require.Equal(t, start.AddDate(0, 0, 1).Format(time.DateOnly), end.Format(time.DateOnly))
		days := map[string]int{}
		for _, r := range diarySessionRows(t, s, sess.ID) {
			days[r.Bucket.In(diaryTestZone).Format(time.DateOnly)]++
		}
		require.Len(t, days, 2, "its ledger rows fall on two local days: %v", days)
	})

	t.Run("today is live", func(t *testing.T) {
		today := fx.Days[0]
		require.Equal(t, DiaryToday, today.Role)
		var live int
		for _, id := range today.Sessions {
			sess := diarySession(t, s, id)
			require.NotEmpty(t, diaryDayRows(t, s, today.Date))
			if sess.Status == core.SessionActive {
				live++
				require.NotEmpty(t, diarySessionRows(t, s, id), "a live session's usage so far")
			}
		}
		require.Equal(t, 2, live)
		require.Equal(t, 2, diaryCount(t, s, `SELECT COUNT(*) FROM tasks t JOIN sessions s ON s.id = t.claimed_by
			WHERE t.status = 'in_progress' AND s.status = 'active' AND t.lease_expires_at > ?`,
			core.FormatTime(diaryTestAnchor.UTC())), "two claims leased past the anchor")
	})
}

func diaryCheckUsageMix(t *testing.T, s *Seeder, fx DiaryFixture) {
	require.Equal(t, []string{
		clientClaude + " " + modelFable + " main", clientClaude + " " + modelFable + " subagent",
		clientClaude + " " + modelHaiku + " subagent", clientClaude + " " + modelOpus + " main",
		clientClaude + " " + modelSonnet + " main", clientCodex + " " + modelGPT + " main",
		clientCodex + " " + modelGPTMini + " main", clientCodex + " " + modelGPTNext + " main",
	}, diaryStrings(t, s, `SELECT DISTINCT s.external_client || ' ' || u.model || ' ' || u.thread
		FROM session_usage u JOIN sessions s ON s.id = u.session_id ORDER BY 1`),
		"two harnesses, three models every day and four more on the day of seven agents, a subagent share")

	// Every plan the agents moved has its sessions' tokens (store.PlanTokenRollups,
	// which the plan pages read).
	rollups, err := store.PlanTokenRollups(s.ctx, s.db)
	require.NoError(t, err)
	for _, ref := range []store.PlanRef{
		{Project: "example-api", Slug: planRateLimits}, {Project: "example-api", Slug: planSearchV2},
		{Project: "example-web", Slug: planDependencyRefresh}, {Project: "example-api", Slug: planDependencyRefresh},
		{Project: "example-ops", Slug: planBackupRotation},
	} {
		require.Positive(t, rollups[ref].Sessions, "%+v", ref)
		require.Positive(t, rollups[ref].Tokens, "%+v", ref)
	}

	ledgerStart := core.FormatTime(fx.Days[DiaryLedgerDays-1].Date)
	require.Zero(t, diaryCount(t, s, `SELECT COUNT(*) FROM sessions s WHERE s.created_at >= ? AND s.ambient = 1
		AND NOT EXISTS (SELECT 1 FROM session_usage u WHERE u.session_id = s.id)`, ledgerStart),
		"every hooked session in the ledger's window has ledger rows")
	require.Zero(t, diaryCount(t, s, `SELECT COUNT(*) FROM sessions s WHERE s.total_tokens <> (SELECT
		COALESCE(SUM(input_tokens + cached_input_tokens + cache_creation_tokens + output_tokens), 0)
		FROM session_usage u WHERE u.session_id = s.id) AND s.created_at >= ?`, ledgerStart),
		"a session's totals are its rows' sum")

	// Parallel agents in the afternoon: two sessions' rows in one quarter-hour
	// between 12:00 and 18:00 local, on most weekdays.
	parallel := 0
	for _, day := range fx.Days {
		held := map[int64]map[string]bool{}
		for _, r := range diaryDayRows(t, s, day.Date) {
			if h := r.Bucket.In(diaryTestZone).Hour(); h < 12 || h >= 18 {
				continue
			}
			if held[r.Bucket.Unix()] == nil {
				held[r.Bucket.Unix()] = map[string]bool{}
			}
			held[r.Bucket.Unix()][r.SessionID] = true
		}
		for _, sessions := range held {
			if len(sessions) >= 2 {
				parallel++
				break
			}
		}
	}
	require.GreaterOrEqual(t, parallel, 8, "days with parallel afternoon agents")
}

// diaryCheckOtherAnchors seeds the fixture around other anchors and checks
// the shape every anchor must give, and that each session's usage is the
// same wherever the calendar puts it. The calendar itself is checked for
// every weekday by TestDiaryCalendar_EveryDay; these seeds cover what only a
// replay shows: a 15-weekday window (its first script is used), a window
// across a daylight-saving change, a zone 45 minutes off the hour, and
// anchors so close to midnight that today's script is squeezed, down to
// seconds after it (squeezed by whole minutes, the fixture failed to seed
// for the first eight minutes of every day).
func diaryCheckOtherAnchors(t *testing.T, base *Seeder, baseFx DiaryFixture) {
	baseRows := map[string]int{}
	for key, id := range baseFx.Sessions {
		baseRows[key] = len(diarySessionRows(t, base, id))
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	odd := time.FixedZone("UTC+5:45", 5*60*60+45*60)
	cases := map[string]struct {
		anchor time.Time
		loc    *time.Location
	}{
		"saturday, 15 weekdays":     {time.Date(2026, 11, 21, 21, 0, 0, 0, diaryTestZone), diaryTestZone},
		"monday, daylight saving":   {time.Date(2026, 11, 16, 21, 0, 0, 0, la), la},
		"wednesday, 45-minute zone": {time.Date(2026, 11, 18, 21, 0, 0, 0, odd), odd},
		"tuesday, after midnight":   {time.Date(2026, 11, 17, 0, 40, 0, 0, diaryTestZone), diaryTestZone},
		"sunday, 7 minutes in":      {time.Date(2026, 11, 22, 0, 7, 0, 0, diaryTestZone), diaryTestZone},
		"sunday, 3 minutes in":      {time.Date(2026, 11, 22, 0, 3, 0, 0, diaryTestZone), diaryTestZone},
		"sunday, 30 seconds in":     {time.Date(2026, 11, 22, 0, 0, 30, 0, diaryTestZone), diaryTestZone},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, fx := diarySeedAt(t, c.anchor, c.loc)
			today := time.Date(c.anchor.Year(), c.anchor.Month(), c.anchor.Day(), 0, 0, 0, 0, c.loc)

			require.Len(t, fx.Days, DiaryLedgerDays+1)
			for i, day := range fx.Days[:DiaryLedgerDays] {
				require.True(t, day.Date.Equal(today.AddDate(0, 0, -i)), "day %d is %s", i, day.Date)
			}
			pre := fx.Days[DiaryLedgerDays]
			require.Equal(t, DiaryPreLedger, pre.Role)
			require.False(t, weekend(pre.Date))
			require.True(t, pre.Date.Before(fx.Days[DiaryLedgerDays-1].Date))
			roles := map[DiaryRole]int{}
			for _, day := range fx.Days {
				roles[day.Role]++
				switch day.Role {
				case DiaryWorkday, DiaryShortDay, DiaryManyAgents, DiaryPreLedger:
					require.False(t, weekend(day.Date), "%s on %s", day.Role, day.Date.Weekday())
				case DiaryQuietWeekend, DiaryWeekend, DiaryLightWeekend:
					require.True(t, weekend(day.Date), "%s on %s", day.Role, day.Date.Weekday())
				}
				if day.Role == DiaryQuietWeekend || day.Role == DiaryWeekend {
					require.Empty(t, diaryStarted(t, s, day.Date), "%s %s", day.Role, day.Date)
					require.Empty(t, diaryDayRows(t, s, day.Date), "%s %s", day.Role, day.Date)
				}
			}
			for role, n := range map[DiaryRole]int{DiaryToday: 1, DiaryQuietWeekend: 2, DiaryShortDay: 1,
				DiaryManyAgents: 1, DiaryLightWeekend: 1, DiaryPreLedger: 1} {
				require.Equal(t, n, roles[role], "days of role %s", role)
			}
			var live int
			for _, spec := range diaryToday {
				sess := diarySession(t, s, fx.Sessions[spec.key])
				require.Equal(t, today.Format(time.DateOnly), sess.CreatedAt.In(c.loc).Format(time.DateOnly),
					"%s starts today", spec.key)
				if sess.Status == core.SessionActive {
					live++
				}
			}
			require.Equal(t, 2, live)
			l := fx.Links
			for _, v := range []string{l.DeletedTask, l.TaskOnlyPlan, l.SharedSlug, l.ShippedPlan, l.SupersededMemory,
				l.GlobalSession, l.CommitTask, l.CommitSession, l.FlaggedSession, l.MidnightSession, l.Mishap,
				l.Trial, l.UnreportedSession} {
				require.NotEmpty(t, v)
			}
			// A session's usage does not depend on the day it lands on.
			for key, id := range fx.Sessions {
				if strings.HasPrefix(key, "today-") {
					continue
				}
				want, ok := baseRows[key]
				if !ok {
					continue // the weekday script used only with 15 weekdays
				}
				require.Len(t, diarySessionRows(t, s, id), want, "ledger rows of %s", key)
			}
		})
	}
}

// diaryCheckDeterministic seeds once more around a's anchor: the database
// comes out the same.
func diaryCheckDeterministic(t *testing.T, a *Seeder, fxA DiaryFixture) {
	b, fxB := diarySeedAt(t, diaryTestAnchor, diaryTestZone)
	require.Equal(t, fxA, fxB)
	dump := func(s *Seeder) []string {
		return slices.Concat(
			diaryStrings(t, s, `SELECT id || ' ' || ts || ' ' || kind || ' ' || payload FROM events ORDER BY id`),
			diaryStrings(t, s, `SELECT session_id || ' ' || bucket || ' ' || model || ' ' || thread || ' ' ||
				(input_tokens + cached_input_tokens + cache_creation_tokens + output_tokens) || ' ' || minute_mask
				FROM session_usage ORDER BY 1`),
			diaryStrings(t, s, `SELECT id || ' ' || status || ' ' || updated_at FROM tasks ORDER BY id`))
	}
	require.Equal(t, dump(a), dump(b), "the same anchor seeds the same database")
}

// TestDiaryCalendar_EveryDay lays the calendar around anchors seven hours
// apart over four weeks, in a fixed zone and a daylight-saving one, and checks
// what the story relies on whatever the weekday: every weekday script on a
// weekday of the window; the quiet weekend whole and before today; the light
// weekend day older than it; the midnight session on Monday to Thursday, the
// next weekday in the window and neither day the short day; and the day older
// than the ledger a weekday, with only weekend days between it and the ledger.
func TestDiaryCalendar_EveryDay(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	for _, loc := range []*time.Location{diaryTestZone, la} {
		start := time.Date(2026, 10, 19, 0, 30, 0, 0, loc)
		for h := 0; h < 28*24; h += 7 {
			anchor := start.Add(time.Duration(h) * time.Hour)
			c, err := newDiaryCalendar(anchor, loc)
			require.NoError(t, err, anchor)
			today := time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, loc)
			require.Len(t, c.days, DiaryLedgerDays)
			require.True(t, c.days[0].Equal(today), anchor)
			require.Contains(t, []int{len(diaryWorkdays) - 1, len(diaryWorkdays)}, len(c.workdays), anchor)
			for _, day := range c.workdays {
				require.False(t, weekend(day), anchor)
				require.True(t, day.Before(today), anchor)
			}
			require.Equal(t, time.Saturday, c.quiet[0].Weekday(), anchor)
			require.True(t, c.quiet[1].Equal(c.quiet[0].AddDate(0, 0, 1)), anchor)
			require.True(t, c.quiet[1].Before(today), anchor)
			require.True(t, weekend(c.light) && c.light.Before(c.quiet[0]), anchor)

			wd := c.midnight.Weekday()
			require.True(t, wd >= time.Monday && wd <= time.Thursday, "%s: midnight on %s", anchor, wd)
			j := slices.IndexFunc(c.workdays, c.midnight.Equal)
			require.GreaterOrEqual(t, j, 1, anchor)
			require.True(t, c.workdays[j-1].Equal(c.midnight.AddDate(0, 0, 1)), "%s: the night ends on a weekday", anchor)
			short := c.workdays[len(diaryWorkdays)-1-diaryShortDay]
			require.Equal(t, DiaryShortDay, c.role(short), anchor)
			many := c.workdays[len(diaryWorkdays)-1-diaryAgentsDay]
			require.Equal(t, DiaryManyAgents, c.role(many), anchor)
			require.False(t, weekend(many), anchor)
			require.False(t, short.Equal(c.midnight) || short.Equal(c.workdays[j-1]), "%s: the short day stays short", anchor)

			require.False(t, weekend(c.preLedger), anchor)
			oldest := c.days[DiaryLedgerDays-1]
			require.True(t, c.preLedger.Before(oldest), anchor)
			for day := c.preLedger.AddDate(0, 0, 1); day.Before(oldest); day = day.AddDate(0, 0, 1) {
				require.True(t, weekend(day), "%s: %s between the ledger and the day older than it", anchor, day)
			}
		}
	}
}
