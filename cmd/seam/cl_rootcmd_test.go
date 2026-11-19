package main

import (
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/shellcmd"
)

// Every command line seam runs or prints for the daemon's account names sudo,
// and the env sudo runs, by absolute path: the owner's PATH can lead through a
// directory the owner's processes write (owner decision 2026-11-18, task L4).

// clRootWordProblem says what is wrong with argv, a command line seam runs or
// prints: "" when it is not run through sudo, or when sudo is shellcmd.Sudo
// and an env it runs is shellcmd.Env; else why.
func clRootWordProblem(argv []string) string {
	if len(argv) == 0 || path.Base(argv[0]) != "sudo" {
		return ""
	}
	if argv[0] != shellcmd.Sudo {
		return fmt.Sprintf("sudo is %q, not %s", argv[0], shellcmd.Sudo)
	}
	i := 1
	for i < len(argv) && strings.HasPrefix(argv[i], "-") {
		if argv[i] == "-u" {
			i++
		}
		i++
	}
	if i < len(argv) && path.Base(argv[i]) == "env" && argv[i] != shellcmd.Env {
		return fmt.Sprintf("sudo runs env as %q, not %s", argv[i], shellcmd.Env)
	}
	return ""
}

func TestCLRootWordProblem_Judges(t *testing.T) {
	for argv, bad := range map[string]bool{
		"/usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/c /s approve x": false,
		"/usr/bin/sudo -n -u _seamless true":                                         false,
		"/usr/bin/sudo -k":                                                           false,
		"seamlessd approvers approve --code X":                                       false,
		"open https://example.org/console/access/x":                                  false,
		"sudo -k":                        true,
		"/home/owner/.local/bin/sudo -k": true,
		"/usr/bin/sudo -k -u _seamless env SEAMLESS_CONFIG=/c /s approve x": true,
		"/usr/bin/sudo -u _seamless /tmp/env SEAMLESS_CONFIG=/c /s":         true,
	} {
		require.Equal(t, bad, clRootWordProblem(strings.Fields(argv)) != "", argv)
	}
}

// Every program seam approve runs on the way to a terminal commit -- the
// passwordless probe and the commit itself, for an approval and a denial --
// names sudo and env by path, and there is one to judge.
func TestCLApprove_RunsSudoAndEnvByPath(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, typed := range []string{"\ny\n", "\nd\n"} {
		rig := newApproveRig(t, routes(t, byRef(t, a)), typed)
		require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
		calls := rig.runner.recorded()
		sudos := 0
		for _, argv := range calls {
			require.NotEqual(t, "sudo", argv[0], "%q: a bare sudo is found on the owner's PATH", typed)
			require.NotEqual(t, "env", argv[0], "%q", typed)
			if path.Base(argv[0]) == "sudo" {
				sudos++
			}
			require.Empty(t, clRootWordProblem(argv), "%q: %v", typed, argv)
		}
		require.Equal(t, 3, sudos, "%q: sudo -k, sudo -n and the commit: %v", typed, calls)
		require.Equal(t, shellcmd.Env, calls[len(calls)-1][4], "%q: the commit runs seamlessd through env", typed)
	}
}

// The command reportReplaced prints for an older key of this Mac's, to run as
// the daemon's account, names sudo and env by path.
func TestCLTouchID_PrintedRemoveNamesSudoAndEnvByPath(t *testing.T) {
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	older := approverKey{ID: "01KPREVKEY0000000000000002", Kind: approverKindTouchID, Name: "Touch ID on laptop", AddedAt: at}
	added := approverKey{ID: "01KTOUCHIDKEY0000000000001", Kind: approverKindTouchID, Name: "Touch ID on laptop", AddedAt: at.Add(time.Hour)}
	var out strings.Builder
	reportReplaced(&out, *testTerminal, []approverKey{older}, []approverKey{older, added}, []approverKey{added})
	var printed []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 && path.Base(f[0]) == "sudo" {
			printed = append(printed, line)
			require.Empty(t, clRootWordProblem(f), line)
		}
	}
	require.Equal(t, []string{"  /usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/opt/seamless/seamless.yaml " +
		"/opt/seamless/bin/seamlessd approvers remove 01KPREVKEY0000000000000002"}, printed)
	require.Empty(t, clRootWordProblem(removeApproverArgv(*testTerminal, older.ID)))
}
