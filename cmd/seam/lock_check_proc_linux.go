//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/agentproc"
)

// errLcProcStat is a /proc/<pid>/stat of another shape.
var errLcProcStat = errors.New("unreadable /proc stat")

// lcProcLookup reads a process's parent from /proc/<pid>/stat and its
// executable from /proc/<pid>/exe (agentproc.ExeName), or from stat's comm
// where exe cannot be read. comm alone names a hook script after itself
// ("reference-trans"), so the walk never saw the shell running it and stopped
// short of git.
func lcProcLookup(pid int) (int, string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", err
	}
	ppid, name, err := lcParseProcStat(b)
	if err != nil {
		return 0, "", err
	}
	if exe := agentproc.ExeName(pid); exe != "" {
		name = exe
	}
	return ppid, name, nil
}

// lcParseProcStat decodes "<pid> (<comm>) <state> <ppid> ...": comm sits in
// parentheses and may hold spaces and parentheses itself, so it runs to the
// last ')'.
func lcParseProcStat(b []byte) (int, string, error) {
	open, closing := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || closing < open {
		return 0, "", errLcProcStat
	}
	f := strings.Fields(string(b[closing+1:]))
	if len(f) < 2 {
		return 0, "", errLcProcStat
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, "", errLcProcStat
	}
	return ppid, string(b[open+1 : closing]), nil
}

// lcProcArgs reads a process's argv from /proc/<pid>/cmdline.
func lcProcArgs(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil, errLcProcStat
	}
	return strings.Split(string(b), "\x00"), nil
}
