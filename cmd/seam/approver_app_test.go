package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The app lives under the owner's ~/Applications, where seam touchid enroll
// installs it.
func TestApproverAppPath(t *testing.T) {
	home := filepath.Join(string(filepath.Separator)+"Users", "owner")
	require.Equal(t, filepath.Join(home, "Applications", "Seamless Approve.app"), approverAppPath(home))
}

// launchApprover hands open exactly the helper's approve command line, and
// refuses -- without running anything -- every argument that is not the shape
// the helper expects: a notification's id or duration becomes a program's
// argument, and one that began with "-" would be read as a flag.
func TestLaunchApprover(t *testing.T) {
	const (
		app    = "/Users/owner/Applications/Seamless Approve.app"
		socket = "/var/run/seamless/seamless.sock"
		id     = "01M6SBMC73SF2XSND2VAZ33QZJ"
	)
	self := func() (string, error) { return fakeSelf, nil }
	errBoom := errors.New("boom")
	for _, tc := range []struct {
		name              string
		goos, sock, id, d string
		code              int
		runErr            error
		wantRun           bool
		wantErr           string // "" for success
		wantIs            error
	}{
		{name: "launches", goos: "darwin", sock: socket, id: id, d: "1h", wantRun: true},
		{name: "the session duration", goos: "darwin", sock: socket, id: id, d: "session", wantRun: true},
		{name: "not macOS", goos: "linux", sock: socket, id: id, d: "1h", wantErr: "exists only on macOS"},
		{name: "no socket", goos: "darwin", sock: "", id: id, d: "1h", wantIs: errNoApproverSocket},
		{name: "a relative socket", goos: "darwin", sock: "run/seamless.sock", id: id, d: "1h", wantErr: "is not an absolute path"},
		{name: "an empty id", goos: "darwin", sock: socket, id: "", d: "1h", wantErr: "not a request id"},
		{name: "an id that is not a ULID", goos: "darwin", sock: socket, id: "req-1", d: "1h", wantErr: "not a request id"},
		{name: "a lowercase ULID", goos: "darwin", sock: socket, id: strings.ToLower(id), d: "1h", wantErr: "not a request id"},
		{name: "an id one short", goos: "darwin", sock: socket, id: id[:25], d: "1h", wantErr: "not a request id"},
		{name: "an id with a letter Crockford leaves out", goos: "darwin", sock: socket, id: "U" + id[1:], d: "1h", wantErr: "not a request id"},
		{name: "an id that starts with -", goos: "darwin", sock: socket, id: "-" + id[1:], d: "1h", wantErr: "not a request id"},
		{name: "an id that is a flag", goos: "darwin", sock: socket, id: "--socket", d: "1h", wantErr: "not a request id"},
		{name: "an empty duration", goos: "darwin", sock: socket, id: id, d: "", wantErr: "for duration"},
		{name: "a duration that starts with -", goos: "darwin", sock: socket, id: id, d: "-1h", wantErr: "for duration"},
		{name: "a duration with a space", goos: "darwin", sock: socket, id: id, d: "1h --for", wantErr: "for duration"},
		{name: "a duration too long", goos: "darwin", sock: socket, id: id, d: strings.Repeat("1", 17), wantErr: "for duration"},
		{name: "open exits non-zero", goos: "darwin", sock: socket, id: id, d: "1h", code: 1, wantRun: true,
			wantErr: "exit status 1 (is it installed? seam touchid enroll)"},
		{name: "open cannot be run", goos: "darwin", sock: socket, id: id, d: "1h", runErr: errBoom, wantRun: true,
			wantErr: "open the Touch ID approver", wantIs: errBoom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got [][]string
			signature := newTouchRunner()
			run := func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
				require.Nil(t, stdin, "open is given no stdin")
				if argv[0] == "codesign" { // the check before open: a good app here
					return signature.exec(ctx, argv, stdin, stdout, stderr)
				}
				got = append(got, argv)
				return tc.code, tc.runErr
			}
			err := launchApprover(context.Background(), run, self, tc.goos, app, tc.sock, tc.id, tc.d)
			if tc.wantRun {
				require.Equal(t, [][]string{{"open", "-n", "-a", app, "--args",
					"approve", "--socket", tc.sock, "--request", tc.id, "--for", tc.d}}, got)
			} else {
				require.Empty(t, got, "nothing is run for a refused launch")
			}
			if tc.wantErr == "" && tc.wantIs == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			}
			if tc.wantIs != nil {
				require.ErrorIs(t, err, tc.wantIs)
			}
		})
	}
}

// The app is checked right before open, the way an install checks it: a
// substitute under ~/Applications never runs, and the refusal names the command
// that puts a good one back. A seam with no team to pin (a source build, whose
// local app the owner confirmed at install) keeps working on the checks that
// need nobody to answer: a signature that verifies and names the helper.
func TestLaunchApprover_ChecksTheAppsSignatureBeforeOpen(t *testing.T) {
	const (
		app    = "/Users/owner/Applications/Seamless Approve.app"
		socket = "/var/run/seamless/seamless.sock"
		id     = "01M6SBMC73SF2XSND2VAZ33QZJ"
	)
	self := func() (string, error) { return fakeSelf, nil }
	pinned := "-R=" + approverRequirement(fakeTeam)
	for _, tc := range []struct {
		name     string
		mutate   func(*touchRunner)
		wantOpen bool
		wantPin  bool
		want     string
	}{
		{name: "a good app opens", wantOpen: true, wantPin: true},
		{name: "a signature that does not verify", mutate: func(r *touchRunner) { r.verifyFails = true },
			want: "code signature does not verify"},
		{name: "a wrong identifier", mutate: func(r *touchRunner) { r.identifier = "com.example.other" },
			want: `signed as "com.example.other"`},
		{name: "another team", mutate: func(r *touchRunner) { r.appTeam = otherTeam },
			want: "signed by team " + otherTeam + ", and this seam by team " + fakeTeam},
		{name: "ad hoc", mutate: func(r *touchRunner) { r.appTeam = "" }, want: "no Developer ID team"},
		{name: "a signature not chaining to Apple", mutate: func(r *touchRunner) { r.requirementBad = true }, wantPin: true,
			want: "does not chain to Apple for team " + fakeTeam},
		{name: "an unsigned seam opens a local build", mutate: func(r *touchRunner) { r.selfTeam, r.appTeam = "", otherTeam }, wantOpen: true},
		{name: "an unsigned seam still wants the helper", mutate: func(r *touchRunner) { r.selfTeam, r.identifier = "", "com.example.other" },
			want: `signed as "com.example.other"`},
		{name: "an unsigned seam still wants it intact", mutate: func(r *touchRunner) { r.selfTeam, r.verifyFails = "", true },
			want: "code signature does not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTouchRunner()
			if tc.mutate != nil {
				tc.mutate(r)
			}
			err := launchApprover(context.Background(), r.exec, self, "darwin", app, socket, id, "1h")
			calls := r.recorded()
			usedPin := slices.ContainsFunc(calls, func(c []string) bool { return slices.Contains(c, pinned) })
			require.Equal(t, tc.wantPin, usedPin, "%v", calls)
			last := calls[len(calls)-1]
			if tc.wantOpen {
				require.NoError(t, err)
				require.Equal(t, "open", last[0], "open comes last, after every check")
				require.Equal(t, app, last[3])
				require.Equal(t, []string{"codesign", "--verify", "--strict", "--deep", app}, calls[0])
				return
			}
			require.ErrorIs(t, err, errApproverRefused)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "seam touchid enroll")
			for _, c := range calls {
				require.NotEqual(t, "open", c[0], "a refused app never runs")
			}
		})
	}
}

// A check that could not run is a refusal, never a launch, and codesign's own
// words reach the bridge's one line of stderr as one line.
func TestLaunchApprover_RefusesWhenTheCheckCannotRun(t *testing.T) {
	errBoom := errors.New("boom")
	self := func() (string, error) { return fakeSelf, nil }
	opened := false
	run := func(_ context.Context, argv []string, _ io.Reader, _, _ io.Writer) (int, error) {
		if argv[0] == "open" {
			opened = true
		}
		return -1, errBoom
	}
	err := launchApprover(context.Background(), run, self, "darwin", "/x.app", "/s.sock", testRequestID, "1h")
	require.ErrorIs(t, err, errBoom)
	require.ErrorContains(t, err, "seam touchid enroll")
	require.False(t, opened)

	run = func(_ context.Context, argv []string, _ io.Reader, _, stderr io.Writer) (int, error) {
		if argv[0] == "open" {
			opened = true
		}
		_, _ = io.WriteString(stderr, "/x.app: a sealed resource is missing or invalid\nfile modified: /x.app/Contents/MacOS/x\n")
		return 1, nil
	}
	err = launchApprover(context.Background(), run, self, "darwin", "/x.app", "/s.sock", testRequestID, "1h")
	require.ErrorContains(t, err, "a sealed resource is missing or invalid file modified:")
	require.NotContains(t, err.Error(), "\n", "one line")
	require.False(t, opened)
}
