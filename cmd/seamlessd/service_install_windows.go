//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

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
		sid:       sid.String(),
		account:   account,
		exe:       exe,
		arguments: taskArguments,
		splitArgs: splitTaskArguments,
		queryXML: func() ([]byte, error) {
			cmd := exec.Command("schtasks", "/Query", "/TN", scheduledTask, "/XML")
			cmd.SysProcAttr = handoffProcAttr()
			return cmd.Output()
		},
		isCurrentUser: isCurrentUser,
		write:         func(def []byte) (string, error) { return writeTaskFile("", "seamless-task-*.xml", def) },
		remove:        os.Remove,
		run: func(argv []string) ([]byte, error) {
			return runHandoff(context.Background(), handoffTimeout, argv[0], argv[1:]...)
		},
	}, nil
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
