// testshard runs `go test` over packages with each big package's top-level
// tests split across several go test processes, so a test run uses every core
// instead of a handful. `make test` and `make test-race` (and so `make check`)
// run it:
//
//	go run ./cmd/testshard [go test flags] -- [packages]   (default ./...)
//
// Why: go test runs packages in parallel but the tests inside one package one
// after another -- almost nothing here calls t.Parallel, and most tests could
// not (t.Setenv, t.Chdir, package-level hooks a test swaps out). So `go test
// -race ./...` spent most of its time with the small packages long done and
// the few big ones (cmd/seamlessd, internal/store, internal/console,
// internal/approval, internal/mcp) each grinding through hundreds of tests on
// one core: 640 s on a 24-core Mac at an average of 3.3 cores busy.
//
// Sharding by process keeps each test's world as it was. A shard is an
// ordinary `go test -run '^(names)$' <pkg>`: its own process, globals,
// environment and working directory, running its tests one after another as
// before. What is new is that two of one package's tests can run at the same
// moment in different processes, which shares exactly what two packages'
// tests already shared: the file system outside t.TempDir, and the machine.
//
// How:
//  1. One `go test -run '^$'` over every package builds each test binary once,
//     so the shards find them in the build cache instead of racing to compile
//     the same packages. A build failure stops the run there.
//  2. Each package's top-level Test, Example and Fuzz names are read from its
//     *_test.go files (build constraints ignored) and packed into shards by
//     their durations in the last run (the times file; a test with no record
//     counts as the median one), longest first onto the least loaded shard,
//     about three shards per job in all (plan).
//  3. Every shard but one runs `-run '^(its names)$'`; the first runs `-skip`
//     of all the other shards' names, so a test the scan missed (a build-tagged
//     file, a name it reads differently) still runs, there. Whether every test
//     runs never depends on the scan -- only how evenly they spread.
//  4. -jobs shards run at a time, the longest expected first, each as
//     `go test -json`, which is how the run learns each test's duration.
//  5. Results print in package order: go test's own ok line per package (its
//     longest shard's time), or every failing shard's failing tests' output
//     and its package-level output. The exit status is 0 only if every shard
//     passed. The new durations replace the package's old ones in the times
//     file.
//
// Each shard is a real go test run, so go test's vet pass, test caching, PATH
// and working directory apply unchanged; only its output is gathered through
// -json. Flags that choose the tests or the output themselves (-run, -skip,
// -list, -bench, -fuzz, -json, -v) run plain go test instead, as does
// TESTSHARD=0.
//
// One run can also be split over several machines, CI's runners:
// TESTSHARD_SLICE=i/n runs part i of n (slice.take). The parts are cut from
// the package list and the test names alone, so every machine cuts the same
// ones and every test runs in exactly one; the times file only orders the
// shards inside a part, and a part adds to it without dropping the names the
// other parts ran.
//
// Environment: GO (the go command, default go), TESTSHARD=0 (plain go test),
// TESTSHARD_JOBS (shards at once, default the CPU count), TESTSHARD_TIMES
// (the times file, default <user cache dir>/seamless/testshard-<race|plain>.tsv,
// shared by every checkout on the machine), TESTSHARD_SLICE (i/n: one part of
// a run split over n machines).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// options is one invocation: go test's flags, the packages, and the knobs.
type options struct {
	gocmd     string
	flags     []string
	pkgs      []string
	jobs      int
	timesPath string
	plain     bool
	slice     slice // n == 0: the whole run
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "testshard: %v\n", err)
		return 2
	}
	if o.plain {
		return plainGoTest(ctx, o, stdout, stderr)
	}
	start := time.Now()
	code, summary := sharded(ctx, o, stdout, stderr)
	if summary != "" {
		fmt.Fprintf(stderr, "testshard: %s in %s\n", summary, time.Since(start).Round(time.Second))
	}
	return code
}

// pickers are the go test flags that choose the tests, or the output, themselves
// (-v asks for every test's output, which a sharded run does not print).
var pickers = []string{"run", "skip", "list", "bench", "fuzz", "json", "v"}

func parseArgs(args []string, getenv func(string) string) (options, error) {
	o := options{gocmd: "go", jobs: runtime.NumCPU()}
	if g := getenv("GO"); g != "" {
		o.gocmd = g
	}
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		return o, errors.New("usage: testshard [go test flags] -- [packages]")
	}
	o.flags = append([]string(nil), args[:sep]...)
	o.pkgs = append([]string(nil), args[sep+1:]...)
	if len(o.pkgs) == 0 {
		o.pkgs = []string{"./..."}
	}
	for _, f := range o.flags {
		name := strings.TrimLeft(f, "-")
		if name == f {
			continue // a flag's value, not a flag
		}
		name, _, _ = strings.Cut(name, "=")
		name = strings.TrimPrefix(name, "test.")
		for _, p := range pickers {
			if name == p {
				o.plain = true
			}
		}
	}
	if getenv("TESTSHARD") == "0" {
		o.plain = true
	}
	if v := getenv("TESTSHARD_SLICE"); v != "" {
		i, n, ok := strings.Cut(v, "/")
		part, err1 := strconv.Atoi(i)
		parts, err2 := strconv.Atoi(n)
		if !ok || err1 != nil || err2 != nil || parts < 1 || part < 1 || part > parts {
			return o, fmt.Errorf("TESTSHARD_SLICE=%q: want i/n, 1 <= i <= n", v)
		}
		if o.plain {
			// Plain go test runs every test: each part would run them all.
			return o, fmt.Errorf("TESTSHARD_SLICE=%q needs a sharded run: no TESTSHARD=0 and no flag that picks the tests (%s)",
				v, strings.Join(pickers, ", "))
		}
		o.slice = slice{i: part - 1, n: parts}
	}
	if j := getenv("TESTSHARD_JOBS"); j != "" {
		n, err := strconv.Atoi(j)
		if err != nil || n < 1 {
			return o, fmt.Errorf("TESTSHARD_JOBS=%q: want a positive number", j)
		}
		o.jobs = n
	}
	o.timesPath = getenv("TESTSHARD_TIMES")
	if o.timesPath == "" {
		mode := "plain"
		for _, f := range o.flags {
			if f == "-race" || f == "--race" || f == "-race=true" {
				mode = "race"
			}
		}
		if dir, err := os.UserCacheDir(); err == nil {
			o.timesPath = filepath.Join(dir, "seamless", "testshard-"+mode+".tsv")
		}
	}
	return o, nil
}

// plainGoTest is `go test <flags> <packages>`, unsharded, with its exit status.
func plainGoTest(ctx context.Context, o options, stdout, stderr io.Writer) int {
	cmd := exec.CommandContext(ctx, o.gocmd, append(append([]string{"test"}, o.flags...), o.pkgs...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	return exitCode(cmd.Run(), stderr)
}

// exitCode is a finished command's exit status; a command that never ran is 1.
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}
	fmt.Fprintf(stderr, "testshard: %v\n", err)
	return 1
}
