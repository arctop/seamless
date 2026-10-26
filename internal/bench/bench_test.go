package bench

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/demokit"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestScenarios_TableShape(t *testing.T) {
	scs := Scenarios()
	require.NotEmpty(t, scs)
	seen := map[string]bool{}
	for _, sc := range scs {
		require.NotEmpty(t, sc.Name, "scenario with empty name")
		require.NotNil(t, sc.Seed, "scenario %s: nil seed", sc.Name)
		require.NotNil(t, sc.Grader, "scenario %s: nil grader", sc.Name)
		require.False(t, seen[sc.Name], "duplicate scenario name %s", sc.Name)
		seen[sc.Name] = true

		// Prompt is sugar for a single step; setting both is a table error.
		require.False(t, sc.Prompt != "" && len(sc.Steps) > 0,
			"scenario %s sets both Prompt and Steps", sc.Name)
		steps := sc.Sessions()
		require.NotEmpty(t, steps, "scenario %s has no sessions", sc.Name)
		for i, st := range steps {
			require.NotEmpty(t, st.Prompt, "scenario %s: step %d has no prompt", sc.Name, i+1)
			for path := range st.Evidence {
				require.True(t, filepath.IsLocal(path),
					"scenario %s: step %d evidence path %q escapes the repo", sc.Name, i+1, path)
			}
		}
		if sc.RequiresRecall {
			require.Len(t, steps, 1,
				"scenario %s: RequiresRecall is a component check and cannot be multi-step", sc.Name)
		}
	}
}

// The suite is five scenarios, one per mechanism of value. Growing it is
// expected -- update the list here when it happens.
func TestScenarios_FiveGradedScenarios(t *testing.T) {
	names := make([]string, 0, len(Scenarios()))
	for _, sc := range Scenarios() {
		names = append(names, sc.Name)
	}
	require.ElementsMatch(t, []string{
		cookieHardeningName, staleAssetsName, deployDrainName, restartLogoutsName, refreshGraceName,
	}, names)
}

// The two-session scenario's control-blinding is carried by exactly two
// flags. Losing either silently un-blinds the vanilla arm -- the evidence
// (or the step-1 working tree) would leak into session B -- and the uplift
// number would quietly stop measuring the handoff. Pinned here for that
// reason.
func TestRestartLogouts_BoundaryPins(t *testing.T) {
	sc, ok := ScenarioByName(restartLogoutsName)
	require.True(t, ok)
	require.Len(t, sc.Steps, 2)

	investigate, fix := sc.Steps[0], sc.Steps[1]
	require.Contains(t, investigate.Evidence, restartLogoutsLogPath)
	require.Contains(t, investigate.Prompt, restartLogoutsLogPath,
		"the prompt must point the agent at the evidence it was given")
	require.NotContains(t, investigate.Prompt, "restart",
		"the prompt must not leak the root cause")

	require.True(t, fix.FreshRepo, "session B must start from a clean tree")
	require.Empty(t, fix.Evidence, "session B must not get the evidence back")

	// The log's line shapes must match what the fixture app actually logs,
	// or a careful agent will notice the evidence is synthetic.
	for _, shape := range []string{
		"myapp 1.4.2 listening on :8080",
		"auth: refresh failed: unknown refresh token",
		"auth: refresh failed: refresh token reuse detected; family revoked",
		"POST /auth/refresh 401",
	} {
		require.Contains(t, restartLogoutsLog, shape)
	}
}

func TestScenarioByName(t *testing.T) {
	sc, ok := ScenarioByName(cookieHardeningName)
	require.True(t, ok)
	require.Equal(t, cookieHardeningName, sc.Name)
	require.False(t, sc.RequiresRecall, "cookie-hardening is briefing-surfaced and must stay headless-runnable")

	_, ok = ScenarioByName("no-such-scenario")
	require.False(t, ok)
}

// seedExpect is one scenario's expected fixture state, asserted against DB
// and file state -- expected memories by name/kind, plan topology, backdated
// finding age -- never against briefing text: the briefing layout churns by
// design and is itself the benchmark's regression surface.
type seedExpect struct {
	scenario      string
	loadBearing   string // memory name; "" for a scenario whose record is the trials
	descContains  string // the briefing's index line must carry the constraint
	extraMemories map[string]core.MemoryKind
	plan          string
	readyTitle    string
	blockedTitle  string
	doneCount     int
	session       string
	findingAgoH   float64
	// findingMustNotContain guards the leak: the finding sets the work up,
	// the memory (or the trials) carries the constraint.
	findingMustNotContain []string
	findingMustContain    []string
	trialLab              string
	trials                int
}

// commonNoiseKinds is the pool subset every scenario seeds, by kind.
func kindsOf(names ...string) map[string]core.MemoryKind {
	out := map[string]core.MemoryKind{}
	for _, n := range names {
		m, ok := commonMemories[n]
		if !ok {
			panic("unknown common memory " + n)
		}
		out[n] = m.kind
	}
	return out
}

// newSeeder is demokit.New over a data dir whose database is already a
// storetest.OpenFresh copy, so each seed opens a migrated file instead of
// running every migration itself. What these tests check is the seeded state,
// not how the database came to exist.
func newSeeder(t *testing.T, dataDir string) *demokit.Seeder {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	s, err := demokit.New(dataDir)
	require.NoError(t, err)
	return s
}

func TestSeeds_State(t *testing.T) {
	expects := []seedExpect{
		{
			scenario:     cookieHardeningName,
			loadBearing:  cookieHardeningMemory,
			descContains: "never strict",
			extraMemories: kindsOf("refresh-token-single-use", "auth-cookies-httponly-secure",
				"persist-refresh-tokens", "dashboard-latency-budget", "deploy-runbook",
				"postgres-timeouts", "chroma-boot-race"),
			plan: cookieHardeningPlan, readyTitle: cookieHardeningStep,
			blockedTitle: "Send the resolution write-up back to security", doneCount: 1,
			session: cookieHardeningSession, findingAgoH: 20,
			findingMustNotContain: []string{"external link", "logged out", "__host"},
			trialLab:              "scanner-findings", trials: 1,
		},
		{
			scenario:     staleAssetsName,
			loadBearing:  staleAssetsMemory,
			descContains: "query strings are ignored",
			extraMemories: kindsOf("refresh-token-single-use", "auth-cookies-httponly-secure",
				"auth-cookies-samesite-lax", "dashboard-latency-budget", "cdn-purge-by-tag",
				"deploy-runbook", "postgres-timeouts", "chroma-boot-race"),
			plan: staleAssetsPlan, readyTitle: staleAssetsStep,
			blockedTitle: "Raise the asset max-age once the URLs are immutable", doneCount: 1,
			session: staleAssetsSession, findingAgoH: 20,
			findingMustNotContain: []string{"query", "?v", "hash", "path-only", "fingerprint"},
			trialLab:              "stale-assets", trials: 1,
		},
		{
			scenario:     deployDrainName,
			loadBearing:  deployDrainMemory,
			descContains: "fail healthz first",
			extraMemories: kindsOf("refresh-token-single-use", "auth-cookies-httponly-secure",
				"auth-cookies-samesite-lax", "persist-refresh-tokens", "dashboard-latency-budget",
				"deploy-runbook", "postgres-timeouts", "chroma-boot-race"),
			plan: deployDrainPlan, readyTitle: deployDrainStep,
			blockedTitle: "Rely on the drain window in the deploy flow and drop the sleep hack", doneCount: 1,
			session: deployDrainSession, findingAgoH: 18,
			findingMustNotContain: []string{"healthz", "poll", "503", "consecutive", "drain"},
			trialLab:              "deploy-drops", trials: 1,
		},
		{
			scenario: restartLogoutsName,
			extraMemories: kindsOf("persist-refresh-tokens", "refresh-token-single-use",
				"auth-cookies-httponly-secure", "auth-cookies-samesite-lax",
				"dashboard-latency-budget", "deploy-runbook", "postgres-timeouts", "chroma-boot-race"),
			plan: restartLogoutsPlan, readyTitle: restartLogoutsRootStep,
			blockedTitle: restartLogoutsFixStep, doneCount: 1,
			session: restartLogoutsSession, findingAgoH: 44,
			findingMustNotContain: []string{"restart", "recycle", "persist", "in-memory", "unknown refresh"},
		},
		{
			scenario: refreshGraceName,
			extraMemories: kindsOf("refresh-token-single-use", "auth-cookies-httponly-secure",
				"auth-cookies-samesite-lax", "persist-refresh-tokens", "dashboard-latency-budget",
				"deploy-runbook", "postgres-timeouts", "chroma-boot-race"),
			plan: refreshGracePlan, readyTitle: refreshGraceStep,
			blockedTitle: "Add a counter and alert for family revocations", doneCount: 1,
			session: refreshGraceSession, findingAgoH: 18,
			// This finding deliberately PRESCRIBES: the investigation record
			// is the scenario's channel.
			findingMustContain: []string{"grace window", "already-minted pair"},
			trialLab:           refreshGraceLab, trials: 2,
		},
	}

	for _, want := range expects {
		t.Run(want.scenario, func(t *testing.T) {
			dataDir := t.TempDir()
			repo := t.TempDir()

			s := newSeeder(t, dataDir)
			sc, ok := ScenarioByName(want.scenario)
			require.True(t, ok)
			require.NoError(t, sc.Seed(s, repo))
			require.NoError(t, s.Close())

			ctx := context.Background()
			db, err := store.Open(filepath.Join(dataDir, "seam.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()

			// Memories: active, correctly named, kinded, scoped, backdated,
			// and on disk (files are the source of truth).
			wantKinds := maps.Clone(want.extraMemories)
			if want.loadBearing != "" {
				if m, ok := commonMemories[want.loadBearing]; ok {
					wantKinds[want.loadBearing] = m.kind
				} else {
					wantKinds[want.loadBearing] = core.KindGotcha
				}
			}
			mems, err := store.ActiveMemories(ctx, db, benchProject)
			require.NoError(t, err)
			require.Len(t, mems, len(wantKinds))
			now := time.Now().UTC()
			for _, m := range mems {
				kind, ok := wantKinds[m.Name]
				require.True(t, ok, "unexpected memory %s", m.Name)
				require.Equal(t, kind, m.Kind, "memory %s", m.Name)
				require.Equal(t, benchProject, m.Project, "memory %s", m.Name)
				require.True(t, m.Created.Before(now.Add(-24*time.Hour)), "memory %s not backdated: %s", m.Name, m.Created)
				_, err := os.Stat(filepath.Join(dataDir, "memory", benchProject, m.Name+".md"))
				require.NoError(t, err, "memory file %s missing", m.Name)
			}

			// The load-bearing memory has to carry the constraint in the ONE
			// line the briefing's memory index shows, or the mechanism under
			// test never fires.
			if want.loadBearing != "" {
				lb, ok, err := store.MemoryByName(ctx, db, benchProject, want.loadBearing)
				require.NoError(t, err)
				require.True(t, ok)
				require.Contains(t, strings.ToLower(lb.Description), want.descContains)
			}

			// Plan topology: exactly one claimable step -- the work the run
			// is graded on -- with the follow-up blocked behind it.
			ready, err := store.ReadyTasksForPlan(ctx, db, benchProject, want.plan)
			require.NoError(t, err)
			require.Len(t, ready, 1)
			require.Equal(t, want.readyTitle, ready[0].Title)

			blocked, err := store.BlockedTasksForPlan(ctx, db, benchProject, want.plan)
			require.NoError(t, err)
			require.Len(t, blocked, 1)
			require.Equal(t, want.blockedTitle, blocked[0].Task.Title)
			require.Len(t, blocked[0].Blockers, 1)
			require.Equal(t, ready[0].ID, blocked[0].Blockers[0].ID)

			done, err := store.ListTasksForPlan(ctx, db, benchProject, core.TaskDone, want.plan)
			require.NoError(t, err)
			require.Len(t, done, want.doneCount)

			// The backdated finding, in RecentFindings' reach, carrying the
			// setup but not the answer (or, where noted, deliberately the
			// answer).
			sess, ok, err := store.SessionByName(ctx, db, want.session)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, core.SessionCompleted, sess.Status)
			age := now.Sub(sess.UpdatedAt)
			require.InDelta(t, want.findingAgoH*60, age.Minutes(), 30,
				"finding age drifted from ~%vh: %s", want.findingAgoH, age)
			for _, leak := range want.findingMustNotContain {
				require.NotContains(t, strings.ToLower(sess.Findings), leak,
					"the finding leaks what the memory/trials are supposed to carry")
			}
			for _, must := range want.findingMustContain {
				require.Contains(t, strings.ToLower(sess.Findings), must)
			}

			recents, err := store.RecentFindings(ctx, db, benchProject, 5)
			require.NoError(t, err)
			var found bool
			for _, r := range recents {
				found = found || r.ID == sess.ID
			}
			require.True(t, found, "the finding is not in RecentFindings")

			// The trials, tied to that session and all failed.
			if want.trialLab != "" {
				trials, err := store.QueryTrials(ctx, db, store.TrialFilter{Lab: want.trialLab})
				require.NoError(t, err)
				require.Len(t, trials, want.trials)
				for _, tr := range trials {
					require.Equal(t, core.OutcomeFail, tr.Outcome)
					require.Equal(t, sess.ID, tr.SessionID)
					require.Equal(t, benchProject, tr.ProjectSlug)
				}
			}

			// The repo mapping binds sessions starting in the demo repo. The
			// key is the PHYSICAL path -- the form the agent's client reports
			// (see TestScenarioSeed_BindsRepoThroughASymlinkedPath) -- so
			// resolve before looking it up, or this passes only on a
			// symlink-free temp dir.
			repoMap, err := store.RepoProjectMap(ctx, db)
			require.NoError(t, err)
			physical, err := filepath.EvalSymlinks(repo)
			require.NoError(t, err)
			require.Equal(t, benchProject, repoMap[physical])
		})
	}
}

// The "" repo path skips the mapping -- what newScenarioRunDir relies on.
func TestSeed_NoRepoMapping(t *testing.T) {
	dataDir := t.TempDir()
	s := newSeeder(t, dataDir)
	sc, ok := ScenarioByName(cookieHardeningName)
	require.True(t, ok)
	require.NoError(t, sc.Seed(s, ""))
	require.NoError(t, s.Close())

	ctx := context.Background()
	db, err := store.Open(filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	repoMap, err := store.RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Empty(t, repoMap)
}

// noiseMemories must refuse an unknown pool name instead of silently seeding
// a thinner corpus.
func TestNoiseMemories_UnknownNameIsAnError(t *testing.T) {
	_, err := noiseMemories("refresh-token-single-use", "no-such-memory")
	require.ErrorContains(t, err, "no-such-memory")
}
