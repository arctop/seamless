package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// scanNames is testNames; a test replaces it to make the scan miss a name.
var scanNames = testNames

// sharded is the whole sharded run. It returns the exit status and a one-line
// summary for stderr.
func sharded(ctx context.Context, o options, stdout, stderr io.Writer) (int, string) {
	// 1. Build every test binary once, through one go command. A binary that
	// does not build, or whose TestMain fails with no test to run, stops the
	// run here.
	pre := exec.CommandContext(ctx, o.gocmd, append(append(append([]string{"test"}, o.flags...), "-run", "^$"), o.pkgs...)...)
	var preOut bytes.Buffer
	pre.Stdout, pre.Stderr = &preOut, &preOut
	if err := pre.Run(); err != nil {
		for _, line := range strings.SplitAfter(preOut.String(), "\n") {
			if !strings.HasPrefix(line, "ok  ") && !strings.HasPrefix(line, "?   ") &&
				!strings.HasPrefix(line, "testing: warning: no tests to run") {
				fmt.Fprint(stdout, line)
			}
		}
		fmt.Fprintln(stdout, "FAIL")
		return max(exitCode(err, stderr), 1), "the test binaries did not build or start"
	}

	// 2. The packages, as go test sees them, and their tests.
	list := exec.CommandContext(ctx, o.gocmd, append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}, o.pkgs...)...)
	list.Stderr = stderr
	out, err := list.Output()
	if err != nil {
		return max(exitCode(err, stderr), 1), ""
	}
	var pkgs []pkg
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, dir, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		pkgs = append(pkgs, pkg{path: path, dir: dir, names: scanNames(dir)})
	}

	// 3. Pack and order the shards; 4. run them.
	rec := loadTimes(o.timesPath)
	shards := plan(pkgs, rec, o.jobs)
	results := runShards(ctx, o, shards)

	// 5. Report in package order, and record the durations.
	byPkg := map[string][]*result{}
	for i := range results {
		byPkg[results[i].shard.pkg] = append(byPkg[results[i].shard.pkg], &results[i])
	}
	code := 0
	for _, p := range pkgs {
		rs := byPkg[p.path]
		sort.Slice(rs, func(i, j int) bool { return rs[i].shard.index < rs[j].shard.index })
		if !report(stdout, p.path, rs) {
			code = 1
		}
	}
	if code != 0 {
		fmt.Fprintln(stdout, "FAIL")
	}
	if ctx.Err() == nil {
		merged := mergeTimes(loadTimes(o.timesPath), results)
		if err := saveTimes(o.timesPath, merged); err != nil {
			fmt.Fprintf(stderr, "testshard: could not record test durations in %s: %v\n", o.timesPath, err)
		}
	}
	return code, summarize(results, o.jobs)
}

// result is one shard's run: its exit, what go test -json said, and the
// top-level tests' durations and verdicts.
type result struct {
	shard   shard
	err     error
	stderr  []byte
	elapsed float64             // the package's own pass/fail Elapsed
	pkgOut  []string            // output with no test: ok/FAIL lines, a panic outside a test
	testOut map[string][]string // output by top-level test, subtests included
	times   map[string]float64  // top-level test -> seconds
	failed  []string            // top-level tests that failed
	cached  bool
}

func runShards(ctx context.Context, o options, shards []shard) []result {
	results := make([]result, len(shards))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < max(o.jobs, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				results[i] = runShard(ctx, o, shards[i])
			}
		}()
	}
	for i := range shards {
		work <- i
	}
	close(work)
	wg.Wait()
	return results
}

// event is one line of go test -json (cmd/test2json's TestEvent).
type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

func runShard(ctx context.Context, o options, s shard) result {
	r := result{shard: s, testOut: map[string][]string{}, times: map[string]float64{}}
	args := append(append([]string{"test", "-json"}, o.flags...), s.args()...)
	cmd := exec.CommandContext(ctx, o.gocmd, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.err = err
		return r
	}
	if err := cmd.Start(); err != nil {
		r.err = err
		return r
	}
	parseEvents(stdout, &r)
	r.err = cmd.Wait()
	r.stderr = errBuf.Bytes()
	return r
}

// parseEvents reads a go test -json stream into r. A line that is not an
// event (go test can print one before the stream starts) is kept as package
// output, so a failure never loses what explains it.
func parseEvents(stream io.Reader, r *result) {
	br := bufio.NewReader(stream)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var e event
			if jerr := json.Unmarshal(line, &e); jerr != nil || e.Action == "" {
				r.pkgOut = append(r.pkgOut, string(line))
			} else {
				r.add(e)
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *result) add(e event) {
	top, _, _ := strings.Cut(e.Test, "/")
	switch e.Action {
	case "output", "build-output":
		if e.Test == "" {
			r.pkgOut = append(r.pkgOut, e.Output)
			if strings.HasPrefix(e.Output, "ok  ") && strings.Contains(e.Output, "(cached)") {
				r.cached = true
			}
		} else {
			r.testOut[top] = append(r.testOut[top], e.Output)
		}
	case "pass", "fail", "skip":
		switch {
		case e.Test == "":
			r.elapsed = e.Elapsed
		case top == e.Test:
			r.times[e.Test] = e.Elapsed
			if e.Action == "fail" {
				r.failed = append(r.failed, e.Test)
			}
		}
	}
}

// ok reports whether the shard passed.
func (r *result) ok() bool { return r.err == nil }

// report prints one package's verdict and reports whether it passed: go
// test's own line for a package run whole, one ok line for a sharded package,
// or each failing shard's failing tests and package output.
func report(w io.Writer, path string, rs []*result) bool {
	passed := true
	for _, r := range rs {
		if r.ok() {
			continue
		}
		passed = false
		if len(r.failed) == 0 {
			// No test failed by name (a panic or timeout outside one, a
			// TestMain that exited, a build step): show all of it.
			names := make([]string, 0, len(r.testOut))
			for n := range r.testOut {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				writeAll(w, r.testOut[n])
			}
		}
		for _, n := range r.failed {
			writeAll(w, r.testOut[n])
		}
		writeAll(w, r.pkgOut)
		if len(r.stderr) > 0 {
			fmt.Fprintf(w, "%s", r.stderr)
		}
		if r.err != nil && len(r.pkgOut) == 0 && len(r.stderr) == 0 {
			fmt.Fprintf(w, "FAIL\t%s\t(%v)\n", path, r.err)
		}
	}
	if !passed {
		return false
	}
	if len(rs) == 1 && rs[0].shard.whole {
		for _, line := range rs[0].pkgOut {
			if strings.HasPrefix(line, "ok  ") || strings.HasPrefix(line, "?   ") {
				fmt.Fprint(w, line)
			}
		}
		return true
	}
	longest, cached := 0.0, 0
	for _, r := range rs {
		longest = max(longest, r.elapsed)
		if r.cached {
			cached++
		}
	}
	if cached == len(rs) {
		fmt.Fprintf(w, "ok  \t%s\t(cached, %d shards)\n", path, len(rs))
	} else {
		fmt.Fprintf(w, "ok  \t%s\t%.3fs\t(longest of %d shards)\n", path, longest, len(rs))
	}
	return true
}

func writeAll(w io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprint(w, l)
	}
}

// mergeTimes replaces each package's recorded durations with this run's, for
// every package whose shards all reported: a test renamed or removed leaves
// the record with its package's next run. A cached shard's elapsed is the
// original run's, so it counts too.
func mergeTimes(old times, results []result) times {
	fresh := times{}
	incomplete := map[string]bool{}
	for _, r := range results {
		if len(r.times) == 0 && !r.ok() {
			incomplete[r.shard.pkg] = true
		}
		for n, s := range r.times {
			if fresh[r.shard.pkg] == nil {
				fresh[r.shard.pkg] = map[string]float64{}
			}
			fresh[r.shard.pkg][n] = s
		}
	}
	for p, tests := range fresh {
		if incomplete[p] {
			for n, s := range tests {
				if old[p] == nil {
					old[p] = map[string]float64{}
				}
				old[p][n] = s
			}
			continue
		}
		old[p] = tests
	}
	return old
}

// summarize is the run in one line: how many shards, and the slowest test.
func summarize(results []result, jobs int) string {
	slowest, slowPkg, secs := "", "", 0.0
	for _, r := range results {
		for n, s := range r.times {
			if s > secs {
				slowest, slowPkg, secs = n, r.shard.pkg, s
			}
		}
	}
	s := fmt.Sprintf("%d shards, %d at a time", len(results), max(jobs, 1))
	if slowest != "" {
		s += fmt.Sprintf("; slowest test %s (%s) %.0fs", slowest, slowPkg, secs)
	}
	return s
}
