package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// shardsPerJob is how many shards a run aims for per concurrent job: enough
// that the longest ones finishing late still leave the others something to
// do, few enough that the per-process cost stays small. Every shard pays a
// go test start, a test binary start and its own copy of whatever its tests
// set up once per process (a migrated template database, a seeded Diary): on
// this repository, 241 shards cost about 1,200 CPU seconds more than the
// unsharded run did, which a 24-core Mac absorbs and CI's 4 cores would not.
const shardsPerJob = 3

// minPerShard is the fewest unrecorded tests worth a shard of their own.
const minPerShard = 10

// maxShards caps one package's shards.
const maxShards = 32

// minShardSecs is the least work worth its own shard once durations are
// known: below it, the start-up costs more than the split saves.
const minShardSecs = 5.0

// testNames returns the top-level Test, Example and Fuzz functions declared in
// dir's *_test.go files, sorted, without regard to build constraints. A file
// that does not parse is skipped: shard 0's -skip runs whatever this misses.
func testNames(dir string) []string {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if isTestName(fn.Name.Name) {
				seen[fn.Name.Name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// isTestName is go test's own rule: the prefix, then nothing or a rune that is
// not lower case (TestX, Test_x and Test are tests; Testify is not).
func isTestName(name string) bool {
	for _, prefix := range []string{"Test", "Example", "Fuzz"} {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if rest == "" {
			return true
		}
		r, _ := utf8.DecodeRuneInString(rest)
		return !unicode.IsLower(r)
	}
	return false
}

// times is the last recorded duration, in seconds, of each package's
// top-level tests.
type times map[string]map[string]float64

// loadTimes reads a times file: one "package<TAB>test<TAB>seconds" per line.
// A missing or unreadable file is no record at all; a malformed line is
// skipped.
func loadTimes(path string) times {
	t := times{}
	if path == "" {
		return t
	}
	f, err := os.Open(path)
	if err != nil {
		return t
	}
	defer func() { _ = f.Close() }() // read-only
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 3 {
			continue
		}
		secs, err := strconv.ParseFloat(fields[2], 64)
		if err != nil || secs < 0 {
			continue
		}
		if t[fields[0]] == nil {
			t[fields[0]] = map[string]float64{}
		}
		t[fields[0]][fields[1]] = secs
	}
	return t
}

// saveTimes writes t through a temp file and a rename, so a reader (another
// checkout's run) sees the old file or the new one, never half of one. Two
// runs that finish together each write their own view, and the later one
// wins: the file is a scheduling hint, never a result.
func saveTimes(path string, t times) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".testshard-*")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tmp)
	pkgs := make([]string, 0, len(t))
	for p := range t {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		tests := make([]string, 0, len(t[p]))
		for n := range t[p] {
			tests = append(tests, n)
		}
		sort.Strings(tests)
		for _, n := range tests {
			fmt.Fprintf(w, "%s\t%s\t%.3f\n", p, n, t[p][n])
		}
	}
	err = w.Flush()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) // best effort: the temp file is ours and useless now
	}
	return err
}

// pkg is one package to test: its import path, directory and the test names
// this run runs. In one part of a split run (a slice), others are the
// package's scanned names the other parts run, and noCatchAll marks a
// package whose catch-all (the shard that runs whatever the scan missed) is
// another part's. Outside a slice both are zero: the run runs every name.
type pkg struct {
	path       string
	dir        string
	names      []string
	others     []string
	noCatchAll bool
}

// slice is one part of a run split over several machines
// (TESTSHARD_SLICE=i/n): part i of n, i counted from 0 here.
type slice struct{ i, n int }

// take is the part's share of pkgs. The parts are cut by count from the
// package list and the scanned names alone, never from the times file, so
// every machine cuts the same parts whatever its record says: test k of the
// run (packages in go list's order, each one's names sorted) is part k mod n's.
// Each package's catch-all goes with its first name; a package with no names
// counts as one test. A package the part has neither a name nor the catch-all
// of is left out, and so is never built there.
func (s slice) take(pkgs []pkg) []pkg {
	if s.n <= 1 {
		return pkgs
	}
	var out []pkg
	k := 0
	for _, p := range pkgs {
		q := pkg{path: p.path, dir: p.dir, noCatchAll: k%s.n != s.i}
		if len(p.names) == 0 {
			k++
		}
		for _, n := range p.names {
			if k%s.n == s.i {
				q.names = append(q.names, n)
			} else {
				q.others = append(q.others, n)
			}
			k++
		}
		if !q.noCatchAll || len(q.names) > 0 {
			out = append(out, q)
		}
	}
	return out
}

// shard is one go test process: a package and the names it runs. skip marks
// the package's first shard, which runs everything but the other shards'
// names. est is its expected duration in seconds, for ordering.
type shard struct {
	pkg   string
	index int
	names []string
	skip  []string
	est   float64
	whole bool // the package's only shard: no -run, no -skip
}

// args is the shard's go test arguments after the shared flags.
func (s shard) args() []string {
	switch {
	case s.whole:
		return []string{s.pkg}
	case s.skip != nil:
		return []string{"-skip", anchored(s.skip), s.pkg}
	default:
		return []string{"-run", anchored(s.names), s.pkg}
	}
}

// anchored is a -run or -skip pattern matching exactly these top-level names
// (and so all their subtests). Go identifiers need no escaping.
func anchored(names []string) string {
	return "^(" + strings.Join(names, "|") + ")$"
}

// plan packs every package's tests into shards and orders them longest first.
//
// A test's estimate is its recorded duration, or the median recorded one when
// it has none (1 s when nothing is recorded). The run aims at shardsPerJob
// shards per job: a package gets enough shards for its recorded work to come
// to about known/(shardsPerJob*jobs) seconds a shard (but not less than
// minShardSecs), and for its unrecorded tests to come to about
// unknown/(shardsPerJob*jobs) a shard (but not less than minPerShard), where
// known and unknown are the whole run's; at most maxShards, and never more
// than it has tests. So with no record at all it shards by count. Its tests
// then go longest first onto the least loaded shard, so a long test ends up
// alone.
func plan(pkgs []pkg, rec times, jobs int) []shard {
	median := medianTime(rec)
	known, unknown := 0.0, 0
	for _, p := range pkgs {
		for _, n := range p.names {
			if s, ok := rec[p.path][n]; ok {
				known += s
			} else {
				unknown++
			}
		}
	}
	aim := float64(shardsPerJob * max(jobs, 1))
	target := max(known/aim, minShardSecs)
	per := max(int(math.Ceil(float64(unknown)/aim)), minPerShard)

	var shards []shard
	for _, p := range pkgs {
		shards = append(shards, planPackage(p, rec, median, target, per)...)
	}
	sort.SliceStable(shards, func(i, j int) bool { return shards[i].est > shards[j].est })
	return shards
}

func planPackage(p pkg, rec times, median, target float64, per int) []shard {
	// full: this run runs the whole package, so one shard can run it plainly.
	full := !p.noCatchAll && len(p.others) == 0
	if len(p.names) == 0 && !full {
		// The catch-all alone: whatever the scan missed.
		return []shard{{pkg: p.path, skip: p.others}}
	}
	if len(p.names) <= 1 && full {
		est := 0.0
		for _, n := range p.names {
			est += estimate(rec, p.path, n, median)
		}
		return []shard{{pkg: p.path, est: est, whole: true}}
	}
	type test struct {
		name string
		est  float64
	}
	tests := make([]test, len(p.names))
	sum, knownSum, unknown := 0.0, 0.0, 0
	for i, n := range p.names {
		tests[i] = test{n, estimate(rec, p.path, n, median)}
		sum += tests[i].est
		if s, ok := rec[p.path][n]; ok {
			knownSum += s
		} else {
			unknown++
		}
	}
	k := int(math.Ceil(knownSum / target))
	k = max(k, (unknown+per-1)/per, 1)
	k = min(k, maxShards, len(tests))
	if k == 1 && full {
		return []shard{{pkg: p.path, est: sum, whole: true}}
	}

	sort.SliceStable(tests, func(i, j int) bool {
		if tests[i].est != tests[j].est {
			return tests[i].est > tests[j].est
		}
		return tests[i].name < tests[j].name
	})
	shards := make([]shard, k)
	for i := range shards {
		shards[i] = shard{pkg: p.path, index: i}
	}
	for _, t := range tests {
		least := 0
		for i := range shards {
			if shards[i].est < shards[least].est {
				least = i
			}
		}
		shards[least].names = append(shards[least].names, t.name)
		shards[least].est += t.est
	}
	for i := range shards {
		sort.Strings(shards[i].names)
	}
	// Shard 0 runs everything the others do not: its own names, and any test
	// the scan missed -- unless another part of a split run holds the
	// catch-all, and then it runs its names like the rest.
	if !p.noCatchAll {
		shards[0].skip = append(shards[0].skip, p.others...)
		for i := 1; i < k; i++ {
			shards[0].skip = append(shards[0].skip, shards[i].names...)
		}
		sort.Strings(shards[0].skip)
	}
	return shards
}

func estimate(rec times, pkg, name string, median float64) float64 {
	if s, ok := rec[pkg][name]; ok {
		return s
	}
	return median
}

// medianTime is the median recorded duration, or 1 s with none recorded.
func medianTime(rec times) float64 {
	var all []float64
	for _, tests := range rec {
		for _, s := range tests {
			all = append(all, s)
		}
	}
	if len(all) == 0 {
		return 1
	}
	sort.Float64s(all)
	return all[len(all)/2]
}
