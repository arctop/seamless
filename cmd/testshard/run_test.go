package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stream is a go test -json run of two tests, one of which fails.
const stream = `{"Action":"start","Package":"m/a"}
{"Action":"run","Package":"m/a","Test":"TestPass"}
{"Action":"output","Package":"m/a","Test":"TestPass","Output":"=== RUN   TestPass\n"}
{"Action":"output","Package":"m/a","Test":"TestPass","Output":"    a_test.go:5: chatter\n"}
{"Action":"output","Package":"m/a","Test":"TestPass","Output":"--- PASS: TestPass (1.50s)\n"}
{"Action":"pass","Package":"m/a","Test":"TestPass","Elapsed":1.5}
{"Action":"run","Package":"m/a","Test":"TestFail"}
{"Action":"output","Package":"m/a","Test":"TestFail","Output":"=== RUN   TestFail\n"}
{"Action":"run","Package":"m/a","Test":"TestFail/sub"}
{"Action":"output","Package":"m/a","Test":"TestFail/sub","Output":"    a_test.go:9: boom\n"}
{"Action":"fail","Package":"m/a","Test":"TestFail/sub","Elapsed":0.25}
{"Action":"output","Package":"m/a","Test":"TestFail","Output":"--- FAIL: TestFail (0.25s)\n"}
{"Action":"fail","Package":"m/a","Test":"TestFail","Elapsed":0.25}
not json at all
{"Action":"output","Package":"m/a","Output":"FAIL\n"}
{"Action":"output","Package":"m/a","Output":"FAIL\tm/a\t1.800s\n"}
{"Action":"fail","Package":"m/a","Elapsed":1.8}
`

func TestParseEventsAndReport_AFailingShardShowsOnlyWhatFailed(t *testing.T) {
	r := result{shard: shard{pkg: "m/a"}, testOut: map[string][]string{}, times: map[string]float64{}}
	parseEvents(strings.NewReader(stream), &r)
	r.err = fmt.Errorf("exit status 1")

	require.Equal(t, map[string]float64{"TestPass": 1.5, "TestFail": 0.25}, r.times, "top-level tests only")
	require.Equal(t, []string{"TestFail"}, r.failed)
	require.InDelta(t, 1.8, r.elapsed, 1e-9)

	var out bytes.Buffer
	require.False(t, report(&out, "m/a", []*result{&r}))
	require.Equal(t, "=== RUN   TestFail\n    a_test.go:9: boom\n--- FAIL: TestFail (0.25s)\n"+
		"not json at all\nFAIL\nFAIL\tm/a\t1.800s\n", out.String(),
		"the failing test with its subtests, then the package's own lines; no passing test's chatter")
}

func TestReport_ShardedAndWholePackages(t *testing.T) {
	whole := &result{shard: shard{pkg: "m/w", whole: true}, pkgOut: []string{"PASS\n", "ok  \tm/w\t0.5s\n"}}
	var out bytes.Buffer
	require.True(t, report(&out, "m/w", []*result{whole}))
	require.Equal(t, "ok  \tm/w\t0.5s\n", out.String(), "go test's own line")

	a := &result{shard: shard{pkg: "m/s"}, elapsed: 3.25}
	b := &result{shard: shard{pkg: "m/s", index: 1}, elapsed: 7.5}
	out.Reset()
	require.True(t, report(&out, "m/s", []*result{a, b}))
	require.Equal(t, "ok  \tm/s\t7.500s\t(longest of 2 shards)\n", out.String())

	a.cached, b.cached = true, true
	out.Reset()
	require.True(t, report(&out, "m/s", []*result{a, b}))
	require.Equal(t, "ok  \tm/s\t(cached, 2 shards)\n", out.String())

	// A shard that died without naming a failed test shows everything it said.
	dead := &result{shard: shard{pkg: "m/s", index: 1}, err: fmt.Errorf("exit status 2"),
		testOut: map[string][]string{"TestSlow": {"=== RUN   TestSlow\n"}},
		pkgOut:  []string{"panic: test timed out after 1s\n"}}
	out.Reset()
	require.False(t, report(&out, "m/s", []*result{a, dead}))
	require.Equal(t, "=== RUN   TestSlow\npanic: test timed out after 1s\n", out.String())
}

// TestRun_EndToEnd shards a real module's tests through the real go command:
// every test runs exactly once (each appends its name to a log), one the name
// scan missed included; a failure fails the run with its output; a build
// failure stops it; and the durations land in the times file.
func TestRun_EndToEnd(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	mod := t.TempDir()
	log := filepath.Join(t.TempDir(), "ran.log")
	write := func(name, body string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(mod, name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(mod, name), []byte(body), 0o600))
	}
	write("go.mod", "module example.com/m\n\ngo 1.21\n")
	write("a/a.go", "package a\n")
	var tests strings.Builder
	tests.WriteString("package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"func ran(t *testing.T) {\n\tf, err := os.OpenFile(os.Getenv(\"RANLOG\"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)\n" +
		"\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n\tdefer f.Close()\n\tif _, err := f.WriteString(t.Name() + \"\\n\"); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n\n")
	var want []string
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&tests, "func Test%02d(t *testing.T) { ran(t) }\n", i)
		want = append(want, fmt.Sprintf("Test%02d", i))
	}
	write("a/a_test.go", tests.String())
	// Listed by the scan (it ignores build tags), never compiled: a shard's
	// -run naming it matches nothing, harmlessly.
	write("a/tagged_test.go", "//go:build never\n\npackage a\n\nimport \"testing\"\n\nfunc TestNeverBuilt(t *testing.T) { ran(t) }\n")
	// And a test the scan misses: shard 0's -skip must still run it.
	scan := scanNames
	scanNames = func(dir string) []string {
		var kept []string
		for _, n := range scan(dir) {
			if n != "Test59" {
				kept = append(kept, n)
			}
		}
		return kept
	}
	t.Cleanup(func() { scanNames = scan })
	write("b/b.go", "package b\n")
	write("b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { t.Log(\"b ran\") }\n")
	write("c/c.go", "package c\n")

	timesPath := filepath.Join(t.TempDir(), "times.tsv")
	t.Setenv("RANLOG", log)
	t.Setenv("GO", gobin)
	t.Setenv("TESTSHARD_TIMES", timesPath)
	t.Setenv("TESTSHARD_JOBS", "4")
	t.Chdir(mod)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-count=1", "--", "./..."}, &stdout, &stderr)
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	require.Contains(t, stdout.String(), "ok  \texample.com/m/a\t")
	require.Contains(t, stdout.String(), "(longest of 6 shards)",
		"61 names over 3*4 shards is under minPerShard a shard, so ceil(60/10): 59 scanned and TestNeverBuilt")
	require.Contains(t, stdout.String(), "ok  \texample.com/m/b\t")
	require.Contains(t, stdout.String(), "?   \texample.com/m/c\t[no test files]")
	require.Less(t, strings.Index(stdout.String(), "m/a\t"), strings.Index(stdout.String(), "m/b\t"), "package order")
	require.Contains(t, stderr.String(), "testshard: 8 shards, 4 at a time")

	raw, err := os.ReadFile(log)
	require.NoError(t, err)
	got := strings.Fields(string(raw))
	sort.Strings(got)
	sort.Strings(want)
	require.Equal(t, want, got, "every test exactly once, the one the scan missed included")

	rec := loadTimes(timesPath)
	require.Len(t, rec["example.com/m/a"], 60, "every top-level test that ran is recorded")
	require.Contains(t, rec["example.com/m/b"], "TestB")

	// A planted failure fails the run and shows only the failing test.
	write("a/fail_test.go", "package a\n\nimport \"testing\"\n\nfunc TestPlantedFailure(t *testing.T) { t.Fatal(\"planted\") }\n")
	require.NoError(t, os.Remove(log))
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{"-count=1", "--", "./..."}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stdout.String(), "--- FAIL: TestPlantedFailure")
	require.Contains(t, stdout.String(), "planted")
	require.Contains(t, stdout.String(), "FAIL\texample.com/m/a")
	require.NotContains(t, stdout.String(), "Test07", "no passing test's output")
	require.True(t, strings.HasSuffix(stdout.String(), "FAIL\n"), stdout.String())
	require.Contains(t, stdout.String(), "ok  \texample.com/m/b\t", "the other packages still run and report")

	// A build failure stops the run before any shard.
	write("a/a.go", "package a\n\nfunc broken( {\n")
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{"-count=1", "--", "./..."}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stdout.String(), "FAIL\texample.com/m/a")
	require.Contains(t, stdout.String(), "syntax error")
	require.NotContains(t, stdout.String(), "ok  \t", "nothing ran")
}
