package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLockCheck_ParseProcStat(t *testing.T) {
	ppid, name, err := lcParseProcStat([]byte("4242 (git) S 4241 4242 4241 0 -1 4194560 ..."))
	require.NoError(t, err)
	require.Equal(t, 4241, ppid)
	require.Equal(t, "git", name)

	ppid, name, err = lcParseProcStat([]byte("7 (a (b) c) R 6 7 7 0"))
	require.NoError(t, err)
	require.Equal(t, 6, ppid)
	require.Equal(t, "a (b) c", name, "comm runs to the last ')'")

	for _, bad := range []string{"", "7 git S 6", "7 (git) S", "7 (git) S x"} {
		_, _, err := lcParseProcStat([]byte(bad))
		require.Error(t, err, bad)
	}
}

// The live process table: this test's own argv reads back, and a check run by
// a process that is not git (the test runner) names no git command.
func TestLockCheck_TheLiveProcessTable(t *testing.T) {
	argv, err := lcProcArgs(os.Getpid())
	require.NoError(t, err)
	require.Equal(t, os.Args, argv)
	ppid, name, err := lcProcLookup(os.Getpid())
	require.NoError(t, err)
	require.Equal(t, os.Getppid(), ppid)
	require.NotEmpty(t, name)
	cmd, known := lcGitCommand()
	require.True(t, known)
	require.Empty(t, cmd)
}
