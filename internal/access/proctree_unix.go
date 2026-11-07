//go:build darwin || linux

package access

// Peer is the kernel's account of the process that opened one socket
// connection, read once at accept (ReadPeer). It names a process; the chain
// above it is walked per request (Chain), so a lease follows the tree as it
// is, not as it was when the connection opened.
type Peer struct {
	UID uint32
	PID int32
	// pin ties PID to the process that connected: macOS's pidversion from the
	// audit token, Linux's start time read at accept.
	pin uint64
	// Pinned reports that the pin was taken while the kernel guaranteed the
	// pid named the connecting process: always on macOS (the audit token
	// carries it), on Linux only with SO_PEERPIDFD (6.5 and later). Without
	// it, a peer that exits between connect and accept, and whose pid is
	// reused in that window, could be taken for the process that connected --
	// the documented residual race.
	Pinned bool
}

// validBoot reports whether s can be a boot id in a ProcessToken.
func validBoot(s string) bool {
	return ProcessToken{PID: 1, Boot: s}.Valid()
}

// ProgramOf names the process t names, for the approval surfaces and the
// approval core's anchor checks: its executable path when the kernel reveals
// it, else the kernel's name for it (Linux keeps another account's
// /proc/<pid>/exe behind ptrace access, and its stat's name is all the
// daemon's account may read). It is "" when t names no live process of this
// boot: the process exited, its pid now belongs to another, or t was never a
// process the kernel reported here. The name is read only while the pid
// still names t's process -- the start time is checked before and after --
// so it can never be a newer process's that reused the pid.
func ProgramOf(t ProcessToken) string {
	if !t.Valid() {
		return ""
	}
	boot, err := bootID()
	if err != nil || boot != t.Boot {
		return ""
	}
	before, err := lookupProc(t.PID)
	if err != nil || before.start != t.Version {
		return ""
	}
	name := before.name
	if path, err := ProgramPath(t.PID); err == nil && path != "" {
		name = path
	}
	after, err := lookupProc(t.PID)
	if err != nil || after.start != t.Version {
		return ""
	}
	return name
}
