package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsTestName(t *testing.T) {
	for name, want := range map[string]bool{
		"Test": true, "TestX": true, "Test_x": true, "Test1": true,
		"Example": true, "ExampleFoo_bar": true, "Example_x": true,
		"Fuzz": true, "FuzzParse": true,
		"Testify": false, "Examples": false, "Fuzzy": false,
		"test": false, "helperTest": false, "BenchmarkX": false,
	} {
		require.Equal(t, want, isTestName(name), name)
	}
}

func TestTestNames_ReadsEveryTestFileWhateverItsBuildTags(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\nfunc helper() {}\nfunc (s suite) TestMethod(t *testing.T) {}\nfunc ExampleA() {}\n")
	write("b_windows_test.go", "//go:build windows\n\npackage a\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\nfunc TestA(t *testing.T) {}\n")
	write("c_test.go", "package a_test\n\nimport \"testing\"\n\nfunc FuzzC(f *testing.F) {}\n")
	write("broken_test.go", "package a\n\nfunc TestBroken( {\n")
	write("d.go", "package a\n\nfunc TestNotInATestFile() {}\n")

	require.Equal(t, []string{"ExampleA", "FuzzC", "TestA", "TestB"}, testNames(dir),
		"one name once, tagged files included, methods, non-test files and unparsable files out")
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("Test%03d", i)
	}
	return out
}

// assertCovers checks the property the whole tool rests on: each package's
// shards run every name exactly once, and its first shard skips exactly the
// others' names.
func assertCovers(t *testing.T, pkgs []pkg, shards []shard) {
	t.Helper()
	for _, p := range pkgs {
		var mine []shard
		for _, s := range shards {
			if s.pkg == p.path {
				mine = append(mine, s)
			}
		}
		require.NotEmpty(t, mine, p.path)
		if len(mine) == 1 {
			require.True(t, mine[0].whole, "%s: a lone shard runs the package whole", p.path)
			continue
		}
		sort.Slice(mine, func(i, j int) bool { return mine[i].index < mine[j].index })
		var all, others []string
		for i, s := range mine {
			require.False(t, s.whole, p.path)
			require.NotEmpty(t, s.names, "%s shard %d is empty", p.path, i)
			all = append(all, s.names...)
			if i > 0 {
				require.Nil(t, s.skip, "%s: only shard 0 skips", p.path)
				others = append(others, s.names...)
				require.Equal(t, []string{"-run", anchored(s.names), p.path}, s.args())
			}
		}
		sort.Strings(all)
		sort.Strings(others)
		require.Equal(t, p.names, all, "%s: every name in exactly one shard", p.path)
		require.Equal(t, others, mine[0].skip, "%s: shard 0 skips what the others run", p.path)
		require.Equal(t, []string{"-skip", anchored(mine[0].skip), p.path}, mine[0].args())
	}
}

func TestPlan_WithNoRecordShardsByCount(t *testing.T) {
	pkgs := []pkg{
		{path: "m/big", names: names(110)},
		{path: "m/small", names: names(3)},
		{path: "m/one", names: names(1)},
		{path: "m/none"},
	}
	count := func(shards []shard) map[string]int {
		c := map[string]int{}
		for _, s := range shards {
			c[s.pkg]++
		}
		return c
	}

	shards := plan(pkgs, times{}, 8)
	assertCovers(t, pkgs, shards)
	require.Equal(t, map[string]int{"m/big": 11, "m/small": 1, "m/one": 1, "m/none": 1}, count(shards),
		"114 tests over 3*8 shards is under minPerShard a shard, so 10 a shard: ceil(110/10); a small package runs whole")
	require.Equal(t, "m/big", shards[0].pkg, "the most work first")

	shards = plan(pkgs, times{}, 1)
	assertCovers(t, pkgs, shards)
	require.Equal(t, map[string]int{"m/big": 3, "m/small": 1, "m/one": 1, "m/none": 1}, count(shards),
		"one job: about three shards in all, ceil(114/3) = 38 tests a shard")
}

func TestPlan_ALongTestGetsAShardOfItsOwnAndStartsFirst(t *testing.T) {
	pkgs := []pkg{
		{path: "m/a", names: names(60)},
		{path: "m/b", names: names(40)},
	}
	rec := times{"m/a": {}, "m/b": {}}
	for _, n := range pkgs[0].names {
		rec["m/a"][n] = 1
	}
	for _, n := range pkgs[1].names {
		rec["m/b"][n] = 2
	}
	rec["m/b"]["Test007"] = 120

	shards := plan(pkgs, rec, 4)
	assertCovers(t, pkgs, shards)
	require.Equal(t, "m/b", shards[0].pkg)
	require.Equal(t, []string{"Test007"}, shards[0].names, "the long test alone, first in line")
	for i := 1; i < len(shards); i++ {
		require.GreaterOrEqual(t, shards[i-1].est, shards[i].est, "longest expected first")
	}
	// The rest of m/b's 78 s spreads over its other shards.
	for _, s := range shards {
		if s.pkg == "m/b" && len(s.names) > 1 {
			require.LessOrEqual(t, s.est, 30.0, "%v", s.names)
		}
	}
}

func TestPlan_AnUnrecordedTestCountsAsTheMedian(t *testing.T) {
	p := pkg{path: "m/a", names: names(50)}
	rec := times{"m/a": {}}
	for _, n := range p.names[:40] {
		rec["m/a"][n] = 0.5
	}
	shards := plan([]pkg{p}, rec, 2)
	assertCovers(t, []pkg{p}, shards)
	sum := 0.0
	for _, s := range shards {
		sum += s.est
	}
	require.InDelta(t, 25.0, sum, 1e-9, "40 recorded at 0.5 s and 10 new at the median 0.5 s")
}

func TestTimes_RoundTripAndMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "times.tsv")
	require.Empty(t, loadTimes(path), "no file is no record")

	rec := times{"m/a": {"TestA": 1.5, "TestB": 0}, "m/b": {"TestC": 120.25}}
	require.NoError(t, saveTimes(path, rec))
	require.Equal(t, rec, loadTimes(path))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	junk := string(raw) + "m/a\tTestD\n" + "m/a\tTestE\tsoon\n" + "m/a\tTestF\t-1\n" + "\n"
	require.NoError(t, os.WriteFile(path, []byte(junk), 0o600))
	require.Equal(t, rec, loadTimes(path), "malformed lines are skipped")

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file is left behind")
}

func TestMergeTimes(t *testing.T) {
	old := times{
		"m/a": {"TestGone": 9, "TestKept": 1},
		"m/b": {"TestB": 4},
		"m/c": {"TestC": 2},
	}
	results := []result{
		{shard: shard{pkg: "m/a"}, times: map[string]float64{"TestKept": 3, "TestNew": 1}},
		// m/b: one shard reported, another died before saying anything.
		{shard: shard{pkg: "m/b"}, times: map[string]float64{"TestB2": 5}},
		{shard: shard{pkg: "m/b", index: 1}, times: map[string]float64{}, err: fmt.Errorf("signal: killed")},
	}
	got := mergeTimes(old, results)
	require.Equal(t, map[string]float64{"TestKept": 3, "TestNew": 1}, got["m/a"], "a package that ran whole is replaced")
	require.Equal(t, map[string]float64{"TestB": 4, "TestB2": 5}, got["m/b"], "a package with a silent shard keeps its old record")
	require.Equal(t, map[string]float64{"TestC": 2}, got["m/c"], "a package not run is untouched")
}

func TestParseArgs(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}

	_, err := parseArgs([]string{"-race", "./..."}, env())
	require.ErrorContains(t, err, "usage", "the packages follow --")

	o, err := parseArgs([]string{"-race", "-timeout", "20m", "--"}, env("TESTSHARD_TIMES", "/x/t.tsv", "GO", "go9"))
	require.NoError(t, err)
	require.False(t, o.plain)
	require.Equal(t, []string{"-race", "-timeout", "20m"}, o.flags)
	require.Equal(t, []string{"./..."}, o.pkgs)
	require.Equal(t, "/x/t.tsv", o.timesPath)
	require.Equal(t, "go9", o.gocmd)

	for _, picker := range []string{"-run", "-run=X", "--skip", "-list", "-bench=.", "-fuzz", "-json", "-v", "-test.run"} {
		o, err := parseArgs([]string{picker, "--", "./a"}, env())
		require.NoError(t, err)
		require.True(t, o.plain, picker)
	}
	o, err = parseArgs([]string{"-tags", "v", "--", "./a"}, env())
	require.NoError(t, err)
	require.False(t, o.plain, "a flag's value is not a flag")

	o, err = parseArgs([]string{"--"}, env("TESTSHARD", "0"))
	require.NoError(t, err)
	require.True(t, o.plain)

	o, err = parseArgs([]string{"--"}, env("TESTSHARD_JOBS", "3"))
	require.NoError(t, err)
	require.Equal(t, 3, o.jobs)
	_, err = parseArgs([]string{"--"}, env("TESTSHARD_JOBS", "0"))
	require.ErrorContains(t, err, "TESTSHARD_JOBS")

	race, err := parseArgs([]string{"-race", "--"}, env())
	require.NoError(t, err)
	plainRun, err := parseArgs([]string{"--"}, env())
	require.NoError(t, err)
	if race.timesPath != "" {
		require.True(t, strings.HasSuffix(race.timesPath, "testshard-race.tsv"), race.timesPath)
		require.True(t, strings.HasSuffix(plainRun.timesPath, "testshard-plain.tsv"), plainRun.timesPath)
	}
}
