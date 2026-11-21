//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The detached spawn is a session of its own, so the agent client killing
// the hook's process group at its timeout does not reach it, and the hook
// does not wait for it.
func TestSpawnDetached_StartsANewSession(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	start := time.Now()
	require.NoError(t, spawnDetached("/bin/sh", []string{"-c", `echo $$ > "$1.tmp" && mv "$1.tmp" "$1" && sleep 30`, "sh", pidFile}))
	require.Less(t, time.Since(start), 5*time.Second, "the spawn does not wait for the child")
	var pid int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 0
	}, 10*time.Second, 10*time.Millisecond)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	sid, err := unix.Getsid(pid) // syscall has no Getsid on linux
	require.NoError(t, err)
	require.Equal(t, pid, sid, "the child leads a session of its own")
	own, err := unix.Getsid(os.Getpid())
	require.NoError(t, err)
	require.NotEqual(t, own, sid)
}
