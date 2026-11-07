//go:build !darwin && !linux

package access

import (
	"fmt"
	"net"
)

// Peer is the kernel's account of a socket peer's process; this platform
// reads none.
type Peer struct {
	UID    uint32
	PID    int32
	Pinned bool
}

// ReadPeer is unsupported here: there is no peer credential this package can
// read, and peer_auth is refused on such a platform before any socket opens.
func ReadPeer(net.Conn) (Peer, error) { return Peer{}, ErrPeerCredUnsupported }

// Chain is unsupported here: no process tree is read, so no lease applies.
func (Peer) Chain() ([]ChainHop, error) { return nil, ErrPeerCredUnsupported }

// ProgramPath is unsupported here.
func ProgramPath(pid int32) (string, error) {
	return "", fmt.Errorf("access.ProgramPath(%d): %w", pid, ErrPeerCredUnsupported)
}

// ProgramOf is unsupported here: no process is named.
func ProgramOf(ProcessToken) string { return "" }
