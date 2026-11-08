package main

import (
	"fmt"
	"os/exec"
)

// launchdChecks is doctor's line for the macOS LaunchAgent: which label the
// installed job runs under. Seamless renamed the label (plan:move-to-arctop),
// and a Mac moves to the new one only when an installer or `make install`
// that knows it runs there, so the line says which one this Mac has, and
// warns when both are loaded, which leaves two daemons racing for one data
// dir and port. Nothing on other OSes, and no line on a Mac with no
// LaunchAgent at all (Homebrew, or serve run by hand).
func launchdChecks(goos, home string, uid int) []check {
	if goos != "darwin" {
		return nil
	}
	loaded := func(label string) bool {
		return exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", uid, label)).Run() == nil
	}
	if c, ok := launchdCheckFor(home, fileExists, loaded); ok {
		return []check{c}
	}
	return nil
}

// launchdCheckFor is launchdChecks over an injected stat and a launchctl
// query: ok is false when neither label is installed or loaded.
func launchdCheckFor(home string, exists, loaded func(string) bool) (check, bool) {
	cur := launchdState{plist: exists(launchdPlist(home, launchdLabel)), loaded: loaded(launchdLabel)}
	old := launchdState{plist: exists(launchdPlist(home, legacyLaunchdLabel)), loaded: loaded(legacyLaunchdLabel)}
	const rerun = "re-run the installer (or `make install` from a clone), which boots out " +
		legacyLaunchdLabel + " and deletes its plist"
	switch {
	case cur.loaded && old.loaded:
		return check{statusWarn, "launchd", fmt.Sprintf("two jobs are loaded, %s and %s, each running seamlessd serve; %s",
			launchdLabel, legacyLaunchdLabel, rerun)}, true
	case cur.present() && old.present():
		return check{statusWarn, "launchd", fmt.Sprintf("%s %s, and the legacy %s is still installed; %s",
			launchdLabel, cur.word(), legacyLaunchdLabel, rerun)}, true
	case cur.present():
		if !cur.loaded {
			return check{statusInfo, "launchd", launchdLabel + " installed, not loaded (seamlessd start loads it)"}, true
		}
		return check{statusOK, "launchd", launchdLabel + " (loaded)"}, true
	case old.present():
		return check{statusInfo, "launchd", fmt.Sprintf("%s (%s), the label before the rename to %s; %s to move it",
			legacyLaunchdLabel, old.word(), launchdLabel, "re-run the installer or `make install`")}, true
	}
	return check{}, false
}

// launchdState is one label's LaunchAgent: its plist on disk, its job in
// launchd.
type launchdState struct{ plist, loaded bool }

func (s launchdState) present() bool { return s.plist || s.loaded }

func (s launchdState) word() string {
	if s.loaded {
		return "loaded"
	}
	return "not loaded"
}
