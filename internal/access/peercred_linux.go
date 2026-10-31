//go:build linux

package access

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID returns the effective uid of the process at the other end of a
// Unix-socket connection, as the kernel recorded it at connect time
// (SO_PEERCRED). Nothing the client sends can change it.
func PeerUID(conn net.Conn) (uint32, error) {
	raw, err := rawConn(conn)
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Ucred
		serr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("access.PeerUID: %w", err)
	}
	if serr != nil {
		return 0, fmt.Errorf("access.PeerUID: SO_PEERCRED: %w", serr)
	}
	return cred.Uid, nil
}
