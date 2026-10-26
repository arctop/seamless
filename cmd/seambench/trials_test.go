// The live-write half, tested against a THROWAWAY instance that this test
// stands up itself. Nothing here may reach the owner's real store: every test
// passes an explicit --trials-url/--trials-key-file target, and
// isolateLiveConfig points the config fallback at a dead port besides.

package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/bench"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const throwawayKey = "throwaway-bearer-key"

// throwawayInstance stands up a full MCP surface over a temp data dir: a real
// instance in every respect except that it is this test's and dies with it.
func throwawayInstance(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	rec := events.NewRecorder(db)
	srv := mcpserver.New(mcpserver.Config{
		DB:       db,
		Files:    mgr,
		Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events:   rec,
		Gardener: gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil),
		APIKey:   throwawayKey,
		// seambench records its results as a trial, so this throwaway instance
		// must expose the research tools: they are an optional feature and ship
		// off, and a fresh test database is never grandfathered on. The real
		// fixture does the same thing through scripts/fixture/harness.sh.
		Features: config.Features{Research: true},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, db
}

// trialTree writes a run tree whose verdicts are already cached, so collecting
// it needs no repo fixtures: one pass, one fail, one that never ran.
func trialTree(t *testing.T) (string, bench.Results) {
	t.Helper()
	root := t.TempDir()
	conds := bench.DefaultConditions()
	write := func(cond bench.Condition, i int, rec bench.RunRecord, g bench.Grade) {
		dir := filepath.Join(root, "cookie-hardening", cond.Name, fmt.Sprintf("run-%02d", i))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		rec.Scenario, rec.Condition, rec.Run = "cookie-hardening", cond, i
		rec.Version, rec.Model = "v9.9.9-test", "claude-opus-5"
		require.NoError(t, bench.WriteRunRecord(dir, rec))
		g.Schema = bench.GradeSchema
		require.NoError(t, bench.WriteGrade(dir, g))
	}
	write(conds[0], 1, bench.RunRecord{Metrics: bench.Metrics{Turns: 7, InputTokens: 900, CostUSD: 0.21}},
		bench.Grade{Status: bench.StatusGraded, Pass: false,
			Details: []string{"repo/gate: refresh limiter present -- FAIL: no limiter on /auth/refresh"}})
	write(conds[1], 1, bench.RunRecord{Metrics: bench.Metrics{Turns: 9, InputTokens: 1200, CostUSD: 0.42}},
		bench.Grade{Status: bench.StatusGraded, Pass: true, Metrics: bench.Metrics{ToolCalls: 6, Injections: 1}})
	write(conds[1], 2, bench.RunRecord{Error: "agent timed out"},
		bench.Grade{Status: bench.StatusFailedToRun, Error: "agent timed out"})

	res, err := bench.Collect(context.Background(), root, bench.CollectOptions{})
	require.NoError(t, err)
	require.Len(t, res.Runs, 3)
	return root, res
}

func TestRecordTrials_OneTrialPerRunAndIdempotent(t *testing.T) {
	isolateLiveConfig(t)
	ctx := context.Background()
	url, db := throwawayInstance(t)
	root, res := trialTree(t)

	opts := &trialOpts{url: url, keyFile: writeKeyFile(t), project: "benchtest"}
	var out bytes.Buffer
	require.NoError(t, recordTrialsForReport(ctx, opts, root, res, &out))
	require.Contains(t, out.String(), "trials: 3 recorded in lab \"seambench\"")
	require.NotContains(t, out.String(), "WARNING")

	trials, err := store.QueryTrials(ctx, db, store.TrialFilter{Lab: trialLab, Limit: 50})
	require.NoError(t, err)
	require.Len(t, trials, 3)

	byTitle := map[string]core.Trial{}
	for _, tr := range trials {
		require.Equal(t, "benchtest", tr.ProjectSlug, "trials are scoped explicitly, never guessed")
		require.Equal(t, trialLab, tr.Lab)
		byTitle[tr.Title] = tr
	}

	pass := byTitle["cookie-hardening / mechanism @ v9.9.9-test run 1"]
	require.Equal(t, core.TrialOutcome("pass"), pass.Outcome)
	require.Equal(t, "PASS", pass.Actual)
	require.Contains(t, pass.Changes, "profile mechanism")
	require.Contains(t, pass.Changes, "model claude-opus-5")
	require.Contains(t, pass.Changes, "control arm is vanilla")
	require.Contains(t, pass.Expected, "cookie-hardening")
	// The {version, condition, scenario, run} tags live in metrics so
	// trial_query can filter on them exactly.
	require.Equal(t, "cookie-hardening", pass.Metrics["scenario"])
	require.Equal(t, "mechanism", pass.Metrics["condition"])
	require.Equal(t, "v9.9.9-test", pass.Metrics["version"])
	require.EqualValues(t, 1, pass.Metrics["run"])
	require.Equal(t, "graded", pass.Metrics["status"])
	require.Equal(t, true, pass.Metrics["pass"])
	require.EqualValues(t, 9, pass.Metrics["turns"])
	require.EqualValues(t, 6, pass.Metrics["toolCalls"], "the grader's half of the metrics rides along")

	fail := byTitle["cookie-hardening / vanilla @ v9.9.9-test run 1"]
	require.Equal(t, core.TrialOutcome("fail"), fail.Outcome)
	require.Contains(t, fail.Actual, "FAIL -- failing gates: repo/gate: refresh limiter present")

	// A run that never produced a verdict is INCONCLUSIVE. Recording it as a
	// failure is how a later reader mistakes an infrastructure flake for a
	// regression.
	crashed := byTitle["cookie-hardening / mechanism @ v9.9.9-test run 2"]
	require.Equal(t, core.TrialOutcome("inconclusive"), crashed.Outcome)
	require.Contains(t, crashed.Actual, "the run itself failed")
	require.Equal(t, "failed_to_run", crashed.Metrics["status"])
	require.Equal(t, false, crashed.Metrics["pass"])

	// The trial id is stamped back into the run dir, so re-reporting the same
	// tree records nothing twice.
	for _, run := range res.Runs {
		g, ok, err := bench.ReadGrade(filepath.Join(root, filepath.FromSlash(run.Dir)))
		require.NoError(t, err)
		require.True(t, ok)
		require.NotEmpty(t, g.TrialID)
	}

	again, err := bench.Collect(ctx, root, bench.CollectOptions{})
	require.NoError(t, err)
	out.Reset()
	require.NoError(t, recordTrialsForReport(ctx, opts, root, again, &out))
	require.Contains(t, out.String(), "trials: 0 recorded")
	require.Contains(t, out.String(), "3 already recorded")

	trials, err = store.QueryTrials(ctx, db, store.TrialFilter{Lab: trialLab, Limit: 50})
	require.NoError(t, err)
	require.Len(t, trials, 3, "a second report must not duplicate the results")
}

// An instance that is down must never cost a run that already spent tokens:
// the report says so loudly, tells the operator how to retry, and exits fine.
func TestRecordTrials_UnreachableInstanceWarnsWithoutFailing(t *testing.T) {
	isolateLiveConfig(t)
	root, res := trialTree(t)

	opts := &trialOpts{url: "http://127.0.0.1:1", keyFile: writeKeyFile(t), project: "benchtest"}
	var out bytes.Buffer
	require.NoError(t, recordTrialsForReport(context.Background(), opts, root, res, &out))
	require.Contains(t, out.String(), "trials: WARNING -- results were NOT recorded")
	require.Contains(t, out.String(), "nothing was lost")
	require.Contains(t, out.String(), "seambench report --out "+root)

	// No stamp was written, so a retry records everything.
	g, ok, err := bench.ReadGrade(filepath.Join(root, filepath.FromSlash(res.Runs[0].Dir)))
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, g.TrialID)
}

func TestRecordTrials_BadKeyIsAWarningNotACrash(t *testing.T) {
	isolateLiveConfig(t)
	url, _ := throwawayInstance(t)
	root, res := trialTree(t)

	bad := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(bad, []byte("not-the-key"), 0o600))
	var out bytes.Buffer
	require.NoError(t, recordTrialsForReport(context.Background(),
		&trialOpts{url: url, keyFile: bad, project: "benchtest"}, root, res, &out))
	require.Contains(t, out.String(), "WARNING")
}

func TestTrialOpts_Resolve(t *testing.T) {
	isolateLiveConfig(t)
	key := writeKeyFile(t)

	t.Run("explicit target needs no config", func(t *testing.T) {
		got, err := (&trialOpts{url: "http://127.0.0.1:9/", keyFile: key, project: "p"}).resolve()
		require.NoError(t, err)
		require.Equal(t, trialTarget{url: "http://127.0.0.1:9", key: throwawayKey, project: "p"}, got)
	})

	t.Run("falls back to the configured instance", func(t *testing.T) {
		got, err := (&trialOpts{project: defaultTrialProject}).resolve()
		require.NoError(t, err)
		require.Equal(t, "http://127.0.0.1:1", got.url, "the isolated test config, never the live 8081")
		require.Equal(t, "test-key-not-a-real-one", got.key)
	})

	t.Run("an empty project is refused rather than defaulted to global", func(t *testing.T) {
		_, err := (&trialOpts{url: "http://x", keyFile: key, project: "  "}).resolve()
		require.ErrorContains(t, err, "trials need a scope")
	})

	t.Run("an unreadable key file is an error", func(t *testing.T) {
		_, err := (&trialOpts{url: "http://x", keyFile: filepath.Join(t.TempDir(), "nope"), project: "p"}).resolve()
		require.ErrorContains(t, err, "read --trials-key-file")
	})
}

// The live instance a trial is recorded against is the one the config points
// at. The derivation is config.ServerURL's (and tested there); what this pins
// is that resolve() reaches for it rather than reinventing a base URL.
func TestResolveFallsBackToTheConfiguredServerURL(t *testing.T) {
	tests := []struct{ addr, want string }{
		{"127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"0.0.0.0:8099", "http://127.0.0.1:8099"},
		{":8099", "http://127.0.0.1:8099"},
		{"garbage", "http://127.0.0.1:8081"},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			require.Equal(t, tt.want, config.Config{Addr: tt.addr}.ServerURL())
		})
	}
}

func TestTrialOutcome(t *testing.T) {
	tests := []struct {
		name  string
		grade bench.Grade
		want  string
	}{
		{"passed", bench.Grade{Status: bench.StatusGraded, Pass: true}, "pass"},
		{"failed", bench.Grade{Status: bench.StatusGraded}, "fail"},
		{"never ran", bench.Grade{Status: bench.StatusFailedToRun}, "inconclusive"},
		{"no evidence", bench.Grade{Status: bench.StatusUngradeable}, "inconclusive"},
		// A pass flag on a run that never produced a verdict is meaningless and
		// must not become a pass.
		{"pass flag on a crashed run", bench.Grade{Status: bench.StatusFailedToRun, Pass: true}, "inconclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, trialOutcome(bench.RunResult{Grade: tt.grade}))
		})
	}
}

func TestTrialActual_TruncatesALongGateList(t *testing.T) {
	var details []string
	for i := range 8 {
		details = append(details, fmt.Sprintf("repo/gate: check %d -- FAIL: nope", i))
	}
	details = append(details, "repo/obs: observed thing -- FAIL: not a gate")
	actual := trialActual(bench.RunResult{Grade: bench.Grade{Status: bench.StatusGraded, Details: details}})
	require.Contains(t, actual, "(+3 more)")
	require.NotContains(t, actual, "not a gate", "only gating checks explain a verdict")
}

func writeKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte(throwawayKey+"\n"), 0o600))
	return path
}
