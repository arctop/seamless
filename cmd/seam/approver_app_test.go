package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
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
			run := func(_ context.Context, argv []string, stdin io.Reader, _, _ io.Writer) (int, error) {
				require.Nil(t, stdin, "open is given no stdin")
				got = append(got, argv)
				return tc.code, tc.runErr
			}
			err := launchApprover(context.Background(), run, tc.goos, app, tc.sock, tc.id, tc.d)
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
