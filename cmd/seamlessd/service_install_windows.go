//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// realServiceTaskEnv is install-service on this machine: the user from this
// process's token, this seamlessd.exe, and schtasks run with no console window
// (handoffProcAttr). The logon trigger names the account as DOMAIN\name, the
// form install.ps1's cmdlets gave it, and falls back to the SID when the name
// cannot be looked up.
func realServiceTaskEnv() (serviceTaskEnv, error) {
	sid, err := tokenUserSID()
	if err != nil {
		return serviceTaskEnv{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceTaskEnv{}, fmt.Errorf("find this seamlessd.exe: %w", err)
	}
	account := sid.String()
	if name, domain, _, lerr := sid.LookupAccount(""); lerr == nil && name != "" {
		account = trimAccount(domain, name)
	}
	return serviceTaskEnv{
		sid:           sid.String(),
		account:       account,
		exe:           exe,
		arguments:     taskArguments,
		splitArgs:     splitTaskArguments,
		queryXML:      queryTaskXML,
		isCurrentUser: isCurrentUser,
		write:         func(def []byte) (string, error) { return writeTaskFile("", "seamless-task-*.xml", def) },
		remove:        os.Remove,
		run: func(argv []string) ([]byte, error) {
			return runHandoff(context.Background(), handoffTimeout, argv[0], argv[1:]...)
		},
		started: taskStarted,
	}, nil
}

// schedTaskHasNotRun is SCHED_S_TASK_HAS_NOT_RUN, a task's last result until
// it first runs, as schtasks prints it.
const schedTaskHasNotRun = "267011"

// taskStarted reports whether Windows started the task name that /Run asked
// for. It can fail to only while the user is not signed in: the task runs as
// their InteractiveToken, so with no desktop session of theirs /Run succeeds
// and nothing starts, and the task's last result stays
// SCHED_S_TASK_HAS_NOT_RUN. So only a process outside every desktop session
// (session 0: SSH, a service) asks, for up to 5 seconds; one in a desktop
// session is that session's user, signed in. A task that cannot be read
// counts as started: the health check that follows has the last word.
func taskStarted(name string) bool {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil || session != 0 {
		return true
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for range 20 {
		if res, err := taskLastResult(name); err != nil || res != schedTaskHasNotRun {
			return true
		}
		<-tick.C
	}
	return false
}

// taskLastResult is the task's last result as schtasks prints it, a decimal
// number: the seventh field of its verbose CSV row, which no Windows language
// translates.
func taskLastResult(name string) (string, error) {
	out, err := runHandoff(context.Background(), handoffTimeout, "schtasks", "/Query", "/TN", name, "/V", "/FO", "CSV", "/NH")
	if err != nil {
		return "", err
	}
	row, err := csv.NewReader(bytes.NewReader(out)).Read()
	if err != nil {
		return "", err
	}
	if len(row) < 7 {
		return "", fmt.Errorf("schtasks /Query: %d fields, not the verbose row", len(row))
	}
	return strings.TrimSpace(row[6]), nil
}

// splitTaskArguments splits a task action's argument string the way Windows
// splits a command line, behind a stand-in program name: a command line's
// first word follows rules of its own.
func splitTaskArguments(arguments string) ([]string, error) {
	argv, err := windows.DecomposeCommandLine(`x.exe ` + arguments)
	if err != nil {
		return nil, err
	}
	return argv[1:], nil
}
