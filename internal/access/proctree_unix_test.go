//go:build darwin || linux

package access

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/agentproc"
)

// The kernel paths, on real processes: the peer of a real socket, real
// children and grandchildren, a real orphan.

func chainPIDs(t *testing.T, pid int32) []int32 {
	t.Helper()
	boot, err := bootID()
	require.NoError(t, err)
	f, err := lookupProc(pid)
	require.NoError(t, err)
	hops, err := walkChain(pid, f.pin, boot, lookupProc)
	require.NoError(t, err)
	return pids(hops)
}

// spawn starts `sh -c script` with the test as its parent and returns the sh
// process and the pid the script echoes first (a background job's $!).
func spawn(t *testing.T, script string) (*exec.Cmd, int32) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	pid, err := strconv.ParseInt(strings.TrimSpace(line), 10, 32)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(int(pid), syscall.SIGKILL) })
	return cmd, int32(pid)
}

func TestReadPeer_RealSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "p.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, derr := net.Dial("unix", path)
		if derr == nil {
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	conn, err := ln.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	peer, err := ReadPeer(conn)
	require.NoError(t, err)
	require.Equal(t, int32(os.Getpid()), peer.PID, "the test dialled itself")
	require.Equal(t, uint32(os.Geteuid()), peer.UID)

	hops, err := peer.Chain()
	require.NoError(t, err)
	require.Equal(t, int32(os.Getpid()), hops[0].Token.PID)
	if os.Getppid() > 1 {
		require.Greater(t, len(hops), 1)
		require.Equal(t, int32(os.Getppid()), hops[1].Token.PID)
	}
	self, err := lookupProc(int32(os.Getpid()))
	require.NoError(t, err)
	require.Equal(t, self.start, hops[0].Token.Version)

	// The pin is what makes the peer the process that connected.
	moved := peer
	moved.pin++
	_, err = moved.Chain()
	require.ErrorIs(t, err, ErrPeerGone, "a mismatched pin is refused")

	// A TCP connection has no process at all.
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = tcp.Close() })
	go func() {
		c, derr := net.Dial("tcp", tcp.Addr().String())
		if derr == nil {
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	tc, err := tcp.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.Close() })
	_, err = ReadPeer(tc)
	require.ErrorIs(t, err, errNotUnixConn)
}

func TestWalkChain_RealProcesses(t *testing.T) {
	self := int32(os.Getpid())

	// A grandchild: sh -> sleep, both below the test.
	sh, grandchild := spawn(t, "sleep 60 >/dev/null 2>&1 & echo $!; wait")
	child := int32(sh.Process.Pid)
	require.Equal(t, []int32{grandchild, child, self}, chainPIDs(t, grandchild)[:3], "a grandchild reaches the test through its parent")
	require.Equal(t, []int32{child, self}, chainPIDs(t, child)[:2])

	// A sibling of the child: another child of the test, never passing
	// through the first.
	sib, sibling := spawn(t, "echo $$; exec sleep 60")
	_ = sib
	require.NotContains(t, chainPIDs(t, sibling), child, "a sibling of the same uid is not in the child's tree")
	require.Contains(t, chainPIDs(t, sibling), self)

	// An orphan: its shell exits at once, and the kernel reparents the
	// background sleep (to launchd, init or a subreaper). Its chain no longer
	// reaches the test.
	orphanSh, orphan := spawn(t, "sleep 60 >/dev/null 2>&1 & echo $!")
	require.NoError(t, orphanSh.Wait())
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := lookupProc(orphan)
		require.NoError(t, err)
		if f.ppid != int32(orphanSh.Process.Pid) {
			break
		}
		require.True(t, time.Now().Before(deadline), "the orphan was never reparented")
		<-time.After(5 * time.Millisecond)
	}
	require.NotContains(t, chainPIDs(t, orphan), self, "an orphan loses the tree it left")
}

// The chain's tokens render exactly as agentproc renders the agent process a
// session is stamped with, on this platform: the approval core compares the
// two for equality.
func TestChain_AgentIDMatchesAgentproc(t *testing.T) {
	anchor, ok := agentproc.Anchor()
	if !ok {
		t.Skip("no non-shell ancestor within agentproc's reach")
	}
	boot, err := bootID()
	require.NoError(t, err)
	self, err := lookupProc(int32(os.Getpid()))
	require.NoError(t, err)
	hops, err := walkChain(int32(os.Getpid()), self.pin, boot, lookupProc)
	require.NoError(t, err)
	var ids []string
	for _, h := range hops {
		ids = append(ids, h.Token.AgentID())
	}
	require.Contains(t, ids, anchor)
}

func TestProgramPath_Self(t *testing.T) {
	got, err := ProgramPath(int32(os.Getpid()))
	require.NoError(t, err)
	exe, err := os.Executable()
	require.NoError(t, err)
	a, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	b, err := filepath.EvalSymlinks(exe)
	require.NoError(t, err)
	require.Equal(t, b, a)
}

// shortTempDir is a temp dir short enough for a socket path (104 bytes on
// macOS), which t.TempDir under $TMPDIR is not.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "acc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
