//go:build !darwin && !linux

package access

import (
	"errors"
	"net"
)

// ErrPeerCredUnsupported is PeerUID's answer on a platform with no peer
// credential call this package knows. serve refuses to enable peer_auth there:
// a socket whose callers cannot be told apart is no boundary.
var ErrPeerCredUnsupported = errors.New("access: Unix-socket peer credentials are not supported on this platform")

// PeerUID always fails here.
func PeerUID(net.Conn) (uint32, error) { return 0, ErrPeerCredUnsupported }

// PeerCredSupported reports whether PeerUID works on this platform.
const PeerCredSupported = false
