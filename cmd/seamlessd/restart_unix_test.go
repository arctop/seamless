//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const reexecStageEnv = "SEAMLESS_TEST_REEXEC_STAGE"

// TestReexecHelper is not a test: it is the process TestReexecSelf re-executes.
func TestReexecHelper(t *testing.T) {
	switch os.Getenv(reexecStageEnv) {
	case "first":
		fmt.Printf("first pid=%d\n", os.Getpid())
		t.Setenv(reexecStageEnv, "second")
		t.Fatalf("reexecSelf returned: %v", reexecSelf())
	case "second":
		fmt.Printf("second pid=%d\n", os.Getpid())
	default:
		t.Skip("run by TestReexecSelf")
	}
}

// A restart keeps the pid, which is what lets launchd and systemd see one
// uninterrupted service, and starts a new image with the same arguments.
func TestReexecSelf(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestReexecHelper$", "-test.v") //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), reexecStageEnv+"=first")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	pid := func(stage string) string {
		for line := range strings.SplitSeq(string(out), "\n") {
			if rest, ok := strings.CutPrefix(line, stage+" pid="); ok {
				return rest
			}
		}
		t.Fatalf("no %q line in:\n%s", stage, out)
		return ""
	}
	require.Equal(t, pid("first"), pid("second"))
}
