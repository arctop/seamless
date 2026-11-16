//go:build darwin

package main

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// errLcNoProcess is a pid the process table no longer lists.
var errLcNoProcess = errors.New("no such process")

// lcProcLookup reads a process's parent and executable name through
// sysctl(kern.proc.pid), the call ps makes.
func lcProcLookup(pid int) (int, string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, "", err
	}
	if int(kp.Proc.P_pid) != pid {
		return 0, "", errLcNoProcess
	}
	return int(kp.Eproc.Ppid), unix.ByteSliceToString(kp.Proc.P_comm[:]), nil
}

// lcProcArgs reads a process's argv through sysctl(kern.procargs2), which a
// process of the same account may read.
func lcProcArgs(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return lcParseProcargs2(b)
}

// errLcProcargs is a kern.procargs2 buffer of another shape.
var errLcProcargs = errors.New("unreadable kern.procargs2")

// lcParseProcargs2 decodes kern.procargs2: argc as a 32-bit integer, the
// executable's path, NUL padding, then argc NUL-terminated arguments (and the
// environment after them, which is not read).
func lcParseProcargs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errLcProcargs
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	rest := b[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, errLcProcargs
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	argv := make([]string, 0, argc)
	for len(argv) < argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, errLcProcargs
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, nil
}
