//go:build darwin || linux

package access

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPeerUID_RealSocket dials a real Unix socket and reads the peer's uid back
// from the kernel: the test's own uid, since both ends are this process.
func TestPeerUID_RealSocket(t *testing.T) {
	// A short directory: macOS caps a socket path at 104 bytes, and t.TempDir()
	// nests deeply under $TMPDIR.
	dir, err := os.MkdirTemp("", "pc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			accepted <- c
		}
		close(accepted)
	}()
	client, err := net.Dial("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	server, ok := <-accepted
	require.True(t, ok)
	t.Cleanup(func() { _ = server.Close() })

	uid, err := PeerUID(server)
	require.NoError(t, err)
	require.Equal(t, uint32(os.Geteuid()), uid)
}

func TestPeerUID_RefusesNonUnixConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = PeerUID(c)
	require.ErrorIs(t, err, errNotUnixConn)
}
