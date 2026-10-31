//go:build darwin || linux

package access

import (
	"errors"
	"net"
	"syscall"
)

// PeerCredSupported reports whether PeerUID works on this platform.
const PeerCredSupported = true

// errNotUnixConn is PeerUID's refusal for a connection that is not a Unix
// socket: there is no peer credential to read, and guessing one would be the
// whole vulnerability.
var errNotUnixConn = errors.New("access.PeerUID: not a Unix-socket connection")

func rawConn(conn net.Conn) (syscall.RawConn, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errNotUnixConn
	}
	return uc.SyscallConn()
}
