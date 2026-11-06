//go:build linux

package access

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Linux: SO_PEERCRED names the peer's pid; SO_PEERPIDFD (6.5 and later) hands
// over a pidfd that holds the connecting process, so the start time read
// while it is alive is that process's. /proc/<pid>/stat gives each process's
// parent and start time. A parent link is verified by start time alone (a
// parent never starts after its child, and a pid reused by a new process
// always does). Without a pidfd the start time is read at accept, and a peer
// that exits and has its pid reused between connect and that read is the
// documented residual race (Peer.Pinned is false).

// ReadPeer reads the process at the other end of a Unix-socket connection
// from the kernel: SO_PEERCRED's pid and uid, pinned to the connecting process
// by its start time.
func ReadPeer(conn net.Conn) (Peer, error) {
	raw, err := rawConn(conn)
	if err != nil {
		return Peer{}, err
	}
	var (
		cred   *unix.Ucred
		pidfd  = -1
		serr   error
		pfdErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if serr != nil {
			return
		}
		pidfd, pfdErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	}); err != nil {
		return Peer{}, fmt.Errorf("access.ReadPeer: %w", err)
	}
	if serr != nil {
		return Peer{}, fmt.Errorf("access.ReadPeer: SO_PEERCRED: %w", serr)
	}
	if cred.Pid <= 0 {
		return Peer{}, fmt.Errorf("access.ReadPeer: SO_PEERCRED names pid %d", cred.Pid)
	}
	pinned := pfdErr == nil && pidfd >= 0
	if pinned {
		defer func() { _ = unix.Close(pidfd) }()
	}
	f, err := lookupProc(cred.Pid)
	if err != nil {
		return Peer{}, fmt.Errorf("access.ReadPeer: %w", err)
	}
	// With a pidfd, the process it holds must still be alive AFTER the start
	// time was read: then the pid could not have been reused in between, and
	// the start time is the connecting process's.
	if pinned {
		if err := unix.PidfdSendSignal(pidfd, 0, nil, 0); err != nil {
			return Peer{}, fmt.Errorf("access.ReadPeer: %w", ErrPeerGone)
		}
	}
	return Peer{UID: cred.Uid, PID: cred.Pid, pin: f.start, Pinned: pinned}, nil
}

// Chain walks the peer's parent chain, the peer first. The peer must still be
// the process that connected (its start time unchanged), or ErrPeerGone.
func (p Peer) Chain() ([]ChainHop, error) {
	boot, err := bootID()
	if err != nil {
		return nil, err
	}
	return walkChain(p.PID, p.pin, boot, lookupProc)
}

func lookupProc(pid int32) (procFacts, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.FormatInt(int64(pid), 10), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return procFacts{}, errNoProcess
	}
	if err != nil {
		return procFacts{}, err
	}
	f, err := parseProcStat(string(b))
	if err != nil {
		return procFacts{}, err
	}
	f.pin = f.start
	return f, nil
}

// ProgramPath returns the executable path of pid, for the approval surfaces to
// show. It is display only: a path is never part of an identity. Reading
// another account's /proc/<pid>/exe needs ptrace access, so the daemon's own
// account may be refused; the caller shows the process name instead.
func ProgramPath(pid int32) (string, error) {
	p, err := os.Readlink(filepath.Join("/proc", strconv.FormatInt(int64(pid), 10), "exe"))
	if err != nil {
		return "", fmt.Errorf("access.ProgramPath(%d): %w", pid, err)
	}
	return p, nil
}

var (
	bootOnce sync.Once
	bootVal  string
	bootErr  error
)

// bootID is /proc/sys/kernel/random/boot_id, read once: a new boot is a new
// namespace for pids and start times.
func bootID() (string, error) {
	bootOnce.Do(func() {
		b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		s := strings.TrimSpace(string(b))
		switch {
		case err != nil:
			bootErr = fmt.Errorf("access: boot_id: %w", err)
		case !validBoot(s):
			bootErr = fmt.Errorf("access: boot_id %q is not a boot id", s)
		default:
			bootVal = s
		}
	})
	return bootVal, bootErr
}
