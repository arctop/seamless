package main

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLockCheck_ParseProcargs2(t *testing.T) {
	buf := binary.LittleEndian.AppendUint32(nil, 3)
	buf = append(buf, "/usr/bin/git\x00\x00\x00\x00git\x00fetch\x00-q\x00HOME=/x\x00"...)
	argv, err := lcParseProcargs2(buf)
	require.NoError(t, err)
	require.Equal(t, []string{"git", "fetch", "-q"}, argv, "the environment after argv is not read")

	for _, bad := range [][]byte{nil, {1, 0}, binary.LittleEndian.AppendUint32(nil, 2), append(binary.LittleEndian.AppendUint32(nil, 2), "/x\x00a\x00"...)} {
		_, err := lcParseProcargs2(bad)
		require.Error(t, err, "%q", bad)
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
