//go:build darwin

package access

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID returns the effective uid of the process at the other end of a
// Unix-socket connection, as the kernel recorded it at connect time
// (LOCAL_PEERCRED). Nothing the client sends can change it.
func PeerUID(conn net.Conn) (uint32, error) {
	raw, err := rawConn(conn)
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Xucred
		serr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("access.PeerUID: %w", err)
	}
	if serr != nil {
		return 0, fmt.Errorf("access.PeerUID: LOCAL_PEERCRED: %w", serr)
	}
	return cred.Uid, nil
}
