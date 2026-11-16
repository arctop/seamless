//go:build linux

package agentproc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// errMalformedStat is a /proc/<pid>/stat line that does not parse.
var errMalformedStat = errors.New("agentproc: malformed /proc stat line")

// newLookup reads the process table from /proc.
func newLookup() (func(int) (procInfo, error), error) {
	return lookupLinux, nil
}

func lookupLinux(pid int) (procInfo, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, fs.ErrNotExist) {
		return procInfo{}, errNoProcess
	}
	if err != nil {
		return procInfo{}, err
	}
	info, err := parseStat(string(b))
	if err != nil {
		return procInfo{}, err
	}
	if exe := ExeName(pid); exe != "" {
		info.name = exe
	}
	return info, nil
}

// ExeName is the path of the program pid runs, read from /proc/<pid>/exe, or
// "" where it cannot be read (another account's process, a kernel thread). A
// walk names a process by it, and by stat's comm only where it is "": comm is
// the base name of the file the process exec'd, so a script -- a git hook's
// #!/bin/sh -- is named after itself ("reference-trans") although a shell runs
// it, and a walk that took comm stopped at the hook instead of passing over it
// to the git and the agent above. macOS names the shell already: its /bin/sh
// execs the shell it selects. A program replaced since it started reads with
// a " (deleted)" suffix, which is dropped.
func ExeName(pid int) string {
	p, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(p, " (deleted)")
}

// parseStat reads the fields the walk needs from a /proc/<pid>/stat line:
// "pid (comm) state ppid ... starttime ...", where starttime is field 22 in
// clock ticks since boot. comm may itself contain spaces and parentheses, so the
// numbered fields are split from the LAST ')'.
func parseStat(line string) (procInfo, error) {
	open := strings.IndexByte(line, '(')
	end := strings.LastIndexByte(line, ')')
	if open < 0 || end < open {
		return procInfo{}, errMalformedStat
	}
	// fields[0] is field 3 (state), so field N sits at index N-3.
	fields := strings.Fields(line[end+1:])
	if len(fields) < 20 {
		return procInfo{}, errMalformedStat
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procInfo{}, errMalformedStat
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procInfo{}, errMalformedStat
	}
	if start == 0 {
		return procInfo{}, errNoStart
	}
	return procInfo{ppid: ppid, name: line[open+1 : end], start: start}, nil
}
