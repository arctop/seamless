//go:build darwin

package access

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The two raw calls (getsockopt for LOCAL_PEERTOKEN, and proc_info) go through
// unix.Syscall6, which reaches libSystem's syscall(2): x/sys exports no
// audit-token getter and no proc_info wrapper, and a cgo-free libSystem
// trampoline would be assembly for two calls. Both numbers have been stable for
// as long as the calls have existed, and a failure fails closed (no chain, no
// lease). staticcheck's SA1019 on the SYS_ constants is silenced at each site
// for that reason.
//
// macOS: the audit token names the peer and its pidversion; kern.proc.pid
// gives each process's parent and start time; proc_info's unique-identifier
// flavor gives the per-boot unique ids that verify every parent link and the
// pidversion that verifies the peer. None of these needs privilege over
// another account's processes (the xnu flavors are NO_CHECK_SAME_USER), so the
// daemon's own account can walk the owner's tree.

// auditToken is audit_token_t: eight 32-bit values. val[1] is the euid,
// val[5] the pid, val[7] the pidversion.
type auditToken [8]uint32

// procUniqIdentifierInfo is struct proc_uniqidentifierinfo (sys/proc_info.h).
type procUniqIdentifierInfo struct {
	UUID      [16]byte
	UniqueID  uint64
	PUniqueID uint64
	IDVersion int32
	_         uint32
	_         uint64
	_         uint64
}

const (
	procInfoCallPIDInfo       = 2  // PROC_INFO_CALL_PIDINFO
	procPIDUniqIdentifierInfo = 17 // PROC_PIDUNIQIDENTIFIERINFO
	procPIDPathInfo           = 11 // PROC_PIDPATHINFO
	procPIDPathInfoMaxSize    = 4 * 1024
	auditTokenEUID            = 1 // indexes into audit_token_t.val
	auditTokenPID             = 5
	auditTokenPIDVersion      = 7
)

// ReadPeer reads the process at the other end of a Unix-socket connection
// from the kernel: LOCAL_PEERTOKEN's audit token, cross-checked against
// LOCAL_PEERCRED's uid.
func ReadPeer(conn net.Conn) (Peer, error) {
	raw, err := rawConn(conn)
	if err != nil {
		return Peer{}, err
	}
	var (
		tok  auditToken
		cred *unix.Xucred
		serr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if serr != nil {
			return
		}
		size := uint32(unsafe.Sizeof(tok))
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.SOL_LOCAL, unix.LOCAL_PEERTOKEN, //nolint:staticcheck // SA1019: no libSystem wrapper takes a raw buffer; see the file comment
			uintptr(unsafe.Pointer(&tok)), uintptr(unsafe.Pointer(&size)), 0)
		switch {
		case errno != 0:
			serr = fmt.Errorf("LOCAL_PEERTOKEN: %w", errno)
		case size != uint32(unsafe.Sizeof(tok)):
			serr = fmt.Errorf("LOCAL_PEERTOKEN: %d bytes, want %d", size, unsafe.Sizeof(tok))
		}
	}); err != nil {
		return Peer{}, fmt.Errorf("access.ReadPeer: %w", err)
	}
	if serr != nil {
		return Peer{}, fmt.Errorf("access.ReadPeer: %w", serr)
	}
	if tok[auditTokenEUID] != cred.Uid {
		return Peer{}, fmt.Errorf("access.ReadPeer: the audit token's euid %d is not the peer credential's uid %d", tok[auditTokenEUID], cred.Uid)
	}
	pid := int32(tok[auditTokenPID])
	if pid <= 0 {
		return Peer{}, fmt.Errorf("access.ReadPeer: the audit token names pid %d", pid)
	}
	return Peer{UID: cred.Uid, PID: pid, pin: uint64(tok[auditTokenPIDVersion]), Pinned: true}, nil
}

// Chain walks the peer's parent chain, the peer first. The peer must still be
// the process that connected (its pidversion unchanged), or ErrPeerGone.
func (p Peer) Chain() ([]ChainHop, error) {
	boot, err := bootID()
	if err != nil {
		return nil, err
	}
	return walkChain(p.PID, p.pin, boot, lookupProc)
}

func lookupProc(pid int32) (procFacts, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil {
		return procFacts{}, err
	}
	if kp.Proc.P_pid != pid {
		return procFacts{}, errNoProcess
	}
	tv := kp.Proc.P_starttime
	if tv.Sec <= 0 {
		return procFacts{}, errNoProcess
	}
	u, err := procUniqInfo(pid)
	if err != nil {
		return procFacts{}, err
	}
	return procFacts{
		ppid:         kp.Eproc.Ppid,
		start:        uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), // agentproc's unit
		name:         unix.ByteSliceToString(kp.Proc.P_comm[:]),
		unique:       u.UniqueID,
		parentUnique: u.PUniqueID,
		pin:          uint64(uint32(u.IDVersion)),
	}, nil
}

func procUniqInfo(pid int32) (procUniqIdentifierInfo, error) {
	var u procUniqIdentifierInfo
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDUniqIdentifierInfo, 0, //nolint:staticcheck // SA1019: x/sys has no proc_info wrapper; see the file comment
		uintptr(unsafe.Pointer(&u)), unsafe.Sizeof(u))
	if errno != 0 {
		if errors.Is(errno, unix.ESRCH) {
			return u, errNoProcess
		}
		return u, fmt.Errorf("proc_info(%d): %w", pid, errno)
	}
	if n != unsafe.Sizeof(u) {
		return u, fmt.Errorf("proc_info(%d): %d bytes, want %d", pid, n, unsafe.Sizeof(u))
	}
	return u, nil
}

// ProgramPath returns the executable path of pid, for the approval surfaces to
// show. It is display only: a path is never part of an identity.
func ProgramPath(pid int32) (string, error) {
	buf := make([]byte, procPIDPathInfoMaxSize)
	// The call's return value is not the length (libproc's proc_pidpath takes
	// strlen of the buffer), so the path is read up to its NUL.
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0, //nolint:staticcheck // SA1019: x/sys has no proc_info wrapper; see the file comment
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return "", fmt.Errorf("access.ProgramPath(%d): %w", pid, errno)
	}
	path := unix.ByteSliceToString(buf)
	if path == "" {
		return "", fmt.Errorf("access.ProgramPath(%d): no path", pid)
	}
	return path, nil
}

var (
	bootOnce sync.Once
	bootVal  string
	bootErr  error
)

// bootID is kern.bootsessionuuid, read once: a new boot is a new namespace
// for pids and start times.
func bootID() (string, error) {
	bootOnce.Do(func() {
		s, err := unix.Sysctl("kern.bootsessionuuid")
		s = strings.TrimSpace(s)
		switch {
		case err != nil:
			bootErr = fmt.Errorf("access: kern.bootsessionuuid: %w", err)
		case !validBoot(s):
			bootErr = fmt.Errorf("access: kern.bootsessionuuid %q is not a boot id", s)
		default:
			bootVal = s
		}
	})
	return bootVal, bootErr
}
