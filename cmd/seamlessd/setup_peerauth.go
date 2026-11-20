package main

// seamlessd setup's locked-projects step: it runs `seamlessd peer-auth setup`
// (peerauth_setup.go), the move of the daemon to an account of its own, once
// the install is done. The owner opts in: by choosing "Turn on locked
// projects (peer_auth)" on a re-run, or with SEAMLESS_PEER_AUTH=1, the
// installers' --peer-auth. A run that does neither lists the step with the
// by-hand ones, saying how.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// setupLockedProjectsWanted reports whether this run turns locked projects
// on: the change was chosen, or the knob asks for it.
func setupLockedProjectsWanted(r *setupRun, st setupState) bool {
	return st.change == "locked-projects" || r.env(setupKnobPeerAuth) == "1"
}

// setupLockedProjectsStep runs the move. With the knob the owner consented in
// the environment, so peer-auth setup does not ask again (--yes); a change
// chosen at the terminal shows the move's plan and asks once there. sudo asks
// for the password on the terminal either way. A move that stops says where it
// stands, and runs on from that step when run again.
func setupLockedProjectsStep(r *setupRun, st *setupState) error {
	args := []string{"peer-auth", "setup"}
	if r.env(setupKnobPeerAuth) == "1" {
		args = append(args, "--yes")
	}
	if r.peerAuthSetup == nil {
		return errors.New("this setup cannot run seamlessd peer-auth setup; run it yourself")
	}
	fmt.Fprintln(r.out)
	setupRow(r.out, "locked", "turning on locked projects: seamlessd "+strings.Join(args, " "))
	if err := r.peerAuthSetup(args); err != nil {
		return fmt.Errorf("seamlessd peer-auth setup: %w; seamlessd peer-auth status says where it stands, and seamlessd peer-auth setup continues from there", err)
	}
	st.locked = true
	return nil
}

// setupPeerAuthKnobs refuses SEAMLESS_PEER_AUTH where it cannot be done,
// before anything is installed: a value other than 1, Windows (no peer_auth),
// a Client (no daemon here to move), and SEAMLESS_NO_SERVICE (no per-user
// service to move to a system one).
func setupPeerAuthKnobs(r *setupRun, in setupInstall, want setupKind) error {
	v := r.env(setupKnobPeerAuth)
	switch {
	case v == "":
		return nil
	case v != "1":
		return setupRefusal("invalid %s=%q: set it to 1 to turn locked projects on, or leave it unset", setupKnobPeerAuth, v)
	case !setupPeerAuthOS(r.goos):
		return setupRefusal("%s: Windows has no peer_auth, so no locked projects; turn them on on a macOS or Linux {server}", setupKnobPeerAuth)
	case want == setupClient || r.serverKnob() != "" || in.kind == setupClient:
		return setupRefusal("%s: a {client} runs no daemon, so its projects are locked on the {server}: set %s=1 there", setupKnobPeerAuth, setupKnobPeerAuth)
	case r.env("SEAMLESS_NO_SERVICE") != "":
		return setupRefusal("%s: locked projects move the per-user service to a system one, and SEAMLESS_NO_SERVICE installs none; drop one of them",
			setupKnobPeerAuth)
	}
	return nil
}

// runPeerAuthSetupChild runs `seamlessd peer-auth setup` as a child of this
// binary, on this process's terminal: its plan, its question and sudo's
// password prompts go there.
func runPeerAuthSetupChild(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this seamlessd: %w", err)
	}
	cmd := exec.Command(exe, args...) //nolint:gosec // this binary, with fixed arguments
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
