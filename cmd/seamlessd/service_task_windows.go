//go:build windows

package main

import (
	"os/exec"
	"sync"
)

// queryTaskXML reads the definition of the task called name. It opens no
// console window: a serve the task started has none to lend it
// (detachTaskConsole), so schtasks would open one of its own.
func queryTaskXML(name string) ([]byte, error) {
	cmd := exec.Command("schtasks", "/Query", "/TN", name, "/XML")
	cmd.SysProcAttr = handoffProcAttr()
	return cmd.Output()
}

// realTaskLookup is taskLookup for this process's user.
func realTaskLookup() (taskLookup, error) {
	sid, err := currentUserSID()
	if err != nil {
		return taskLookup{}, err
	}
	return taskLookup{sid: sid, queryXML: queryTaskXML, isCurrentUser: isCurrentUser}, nil
}

// ownServiceTasks names this user's Seamless tasks (taskLookup.own): none when
// this user's SID cannot be read.
func ownServiceTasks() []string {
	l, err := realTaskLookup()
	if err != nil {
		return nil
	}
	return taskNames(l.own())
}

// serviceTaskForControl is the task the service verbs act on: this user's own
// (the per-user one first), else the per-user name, which reads as not
// installed.
func serviceTaskForControl() string {
	l, err := realTaskLookup()
	if err != nil {
		return ""
	}
	if own := l.own(); len(own) > 0 {
		return own[0].name
	}
	return serviceTaskName(l.sid)
}

// onceTaskXML reads the install probe's task (findServiceTaskXML) once: the
// service probe and gate 9 both parse it. Without this user's SID only the
// legacy name can be read.
func onceTaskXML() func() (string, []byte, error) {
	type read struct {
		name string
		raw  []byte
		err  error
	}
	once := sync.OnceValue(func() read {
		sid, err := currentUserSID()
		if err != nil {
			sid = ""
		}
		name, raw, err := findServiceTaskXML(sid, queryTaskXML)
		return read{name, raw, err}
	})
	return func() (string, []byte, error) {
		r := once()
		return r.name, r.raw, r.err
	}
}
