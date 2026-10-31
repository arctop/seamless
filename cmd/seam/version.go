package main

// seam version -- the running daemon's version.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/update"
)

var versionCmd = spec("version", groupObservability, "the running daemon's version",
	noArgs(), bindNoOpts, runVersion).
	withLong(`Prints the version of the daemon this CLI is configured to talk to, in
the same form as ` + "`seamlessd version`" + `:

    seamlessd 0.3.8 (commit 6d664d2, built 2026-07-18T09:12:04Z, release build)

seam carries no version of its own. Both binaries ship from one tag and one
commit, so a number stamped into seam could only repeat this one or contradict
it, and only the daemon can report what is actually RUNNING -- an installed CLI
sitting next to a daemon nobody restarted would otherwise report the new version
for a process still serving the old one.

The version therefore comes from the daemon's /healthz, and an unreachable daemon
is a failure rather than a fallback: there is no second source to fall back to,
and printing seam's own build here is the exact confusion this avoids.`)

// runVersion prints the running daemon's version, in seamlessd's own phrasing.
//
// /healthz reports version as buildVersion() ("0.3.8+1a2b3c4") plus commit and
// built separately, which is everything `seamlessd version` prints -- so seam
// reassembles that one line rather than inventing a second format for the same
// fact.
func runVersion(_ context.Context, e *env, _ *noOpts, _ []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	base := cfg.ServerURL()

	client, err := cfg.HTTPClient(healthTimeout)
	if err != nil {
		return err
	}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return fmt.Errorf("server unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	var hz map[string]any
	if derr := json.NewDecoder(resp.Body).Decode(&hz); derr != nil {
		return fmt.Errorf("unreadable health response from %s: %w", cfg.DaemonAddress(), derr)
	}

	// A daemon new enough to report its distribution gets it in the line, as
	// `seamlessd version` prints it; an older one is printed as it always was.
	build := ""
	if d := str(hz["distribution"]); d == update.DistributionRelease || d == update.DistributionSource {
		build = ", " + d + " build"
	}
	fmt.Fprintf(e.stdout, "seamlessd %s (commit %s, built %s%s)\n",
		versionOf(str(hz["version"])), str(hz["commit"]), str(hz["built"]), build)
	if line := updateAvailableLine(hz); line != "" {
		fmt.Fprintln(e.stdout, line)
	}
	return nil
}

// updateAvailableLine is the one extra line version and status print when the
// daemon's background check has seen a newer release (/healthz
// "update_available"), or "" when it has not. The value is printed only once it
// parses as a release version: the line is for the owner, and the daemon is
// the one machine that knows how it was installed, so the line sends them
// there instead of guessing a command.
func updateAvailableLine(hz map[string]any) string {
	v, ok := update.Parse(str(hz["update_available"]))
	if !ok {
		return ""
	}
	return fmt.Sprintf("update available: v%s (seamlessd update --check on the daemon's machine shows how to update)", v)
}

// versionOf strips the "+commit" suffix from the daemon's buildVersion, since
// `seamlessd version` prints the bare version and the commit separately. A
// version without the suffix (an unlinked dev build) passes through unchanged.
func versionOf(s string) string {
	base, _, _ := strings.Cut(s, "+")
	return base
}
