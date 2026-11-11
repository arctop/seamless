package update

import (
	"fmt"
	"strings"
)

// Distribution values, stamped into seamlessd at link time (main.distribution).
// Only .goreleaser.yaml stamps DistributionRelease; every other build -- make
// install, go install, go build, the fixtures, CI -- is a source build
// (constraint dev-and-fixture-daemons-never-self-update).
const (
	DistributionRelease = "release"
	DistributionSource  = "source"
)

// IsReleaseBuild reports whether a build is a published release: stamped
// DistributionRelease by goreleaser AND carrying a clean X.Y.Z version. A
// goreleaser snapshot (`make release-snapshot`, version X.Y.Z-SNAPSHOT-<sha>)
// carries the stamp but is not a release, so it is treated like a source
// build everywhere the stamp matters.
func IsReleaseBuild(distribution, version string) bool {
	_, ok := Parse(version)
	return distribution == DistributionRelease && ok
}

// InstallerMarker is the comment the curl and PowerShell installers write into
// the service definition they own (the launchd plist, the systemd unit, and the
// Scheduled Task's description). `make install` renders its
// own plist without it, which is what keeps a developer's deploy from being
// mistaken for an installer install. The ".ps1" installer's marker shares
// this prefix.
const InstallerMarker = "Written by https://thereisnospoon.org/install"

// Kind is how this copy of Seamless was installed, which decides who may
// update it and with what command.
type Kind string

const (
	// KindInstaller is an install the curl/PowerShell installer made and
	// still manages: updated unattended by running the release's installer.
	KindInstaller Kind = "installer"
	// KindSystem is a peer_auth system daemon that updates itself: a
	// LaunchDaemon or system unit running as the daemon's own account, which
	// can write the directory its binaries live in, opted in with update.auto:
	// true in its config. Its updater swaps the release's binaries in place
	// and ends the daemon for the service manager to restart; no installer
	// runs, and no root process.
	KindSystem Kind = "system"
	// KindHomebrew is the Homebrew cask; brew owns its files.
	KindHomebrew Kind = "homebrew"
	// KindSource is a build from source: make install, go install, go build.
	KindSource Kind = "source"
	// KindClient is a client-role machine: it runs no daemon of its own, and
	// its binaries update when its pairing is re-run.
	KindClient Kind = "client"
	// KindUnknown is a release build whose layout cannot be vouched for: run
	// by hand, a binaries-only install, a moved service, an unusual config.
	KindUnknown Kind = "unknown"
)

// Install is the verdict of Detect.
type Install struct {
	Kind Kind `json:"kind"`
	// Reason says why the install does not update itself, for the owner;
	// empty for KindInstaller and KindSystem.
	Reason string `json:"reason,omitempty"`
	// Hint is the command the owner runs to update this kind of install.
	Hint string `json:"hint"`
}

// NotifyOnly reports whether this install is only ever told about updates:
// everything but KindInstaller and KindSystem.
func (i Install) NotifyOnly() bool { return i.Kind != KindInstaller && i.Kind != KindSystem }

// SystemHint is the update command of a peer_auth system daemon laid out as the
// peer_auth guide lays it out: the updater, run as the daemon's account.
const SystemHint = "sudo -u _seamless /opt/seamless/bin/seamlessd update --config /opt/seamless/etc/seamless.yaml"

// SystemManualHint is how a peer_auth daemon that does not update itself is
// updated: by an administrator, since `seamlessd update` refuses it.
const SystemManualHint = "replace the binaries in /opt/seamless/bin with sudo and restart the daemon"

// Hint is the owner's update command for an install of kind k.
func Hint(k Kind) string {
	switch k {
	case KindHomebrew:
		return "brew upgrade --cask arctop/tap/seamless"
	case KindSource:
		return "git pull && make install"
	case KindClient:
		return "re-run the pairing commands `seamlessd client-config` prints on the server"
	case KindSystem:
		return SystemHint
	default:
		return "seamlessd update"
	}
}

// Probe is what Detect decides from: every fact about this process and its
// surroundings, read by the caller (cmd/seamlessd/install_probe.go) so that
// Detect itself is a pure function a table test covers on any OS.
type Probe struct {
	// Distribution is the build stamp: DistributionRelease or anything else.
	Distribution string
	// Version is this binary's version, as `seamlessd version` prints it.
	Version string
	// Client is true on a role: client machine.
	Client bool
	// Root is true when running as root (euid 0). The installer refuses root.
	Root bool
	// Exe is this executable with symlinks resolved; "" when unknown.
	Exe string
	// ExeDirWritable reports whether Exe's directory accepts new files, which
	// replacing the binary in place needs.
	ExeDirWritable bool
	// ConfigIsInstallers reports whether the config this process loaded is
	// the installer's fixed ~/.config/seamless/seamless.yaml (os.SameFile).
	// ConfigPath names the loaded one, for the reason text.
	ConfigIsInstallers bool
	ConfigPath         string
	// ForeignEnv lists the SEAMLESS_* variables set in this process's
	// environment other than SEAMLESS_CONFIG. The installer rewrites the
	// service definition without them, so an update would silently drop them.
	ForeignEnv []string
	// Service is the installer-written service definition, as found.
	Service ServiceProbe
	// Supervised reports whether this process is the service's own instance
	// (launchd job, systemd unit, Scheduled Task); SupervisedReason explains
	// a false.
	Supervised       bool
	SupervisedReason string

	// PeerAuth is true when the loaded config turns peer_auth on. Such a
	// daemon runs as an account of its own under a system service, which the
	// installer's per-user service cannot host, so it is never KindInstaller:
	// it is KindSystem when System passes, else KindUnknown.
	PeerAuth bool
	// AutoOptIn reports whether the loaded config sets update.auto: true
	// explicitly, which a system daemon needs to update itself.
	AutoOptIn bool
	// System is what a peer_auth system daemon is judged by; read only with
	// PeerAuth on.
	System SystemProbe
}

// SystemProbe describes a peer_auth system daemon (KindSystem).
type SystemProbe struct {
	// Account reports whether this process runs as the account peer_auth
	// names as the daemon's: the pin at /opt/seamless/etc/daemon-account, or
	// with no pin a system account. AccountReason explains a false.
	Account       bool
	AccountReason string
	// Service is the system service definition: the LaunchDaemon plist or the
	// system unit. Marker is unused: an administrator writes it.
	Service ServiceProbe
	// Restarts reports whether that service starts the daemon again after it
	// exits cleanly (KeepAlive true; Restart=always or on-success), which is
	// how an in-place update starts the new binary: serve exits 0 on SIGTERM.
	Restarts bool
	// ForeignEnv (Linux only) lists the SEAMLESS_* variables besides
	// SEAMLESS_CONFIG in the daemon's environment. The updater runs as a unit
	// of the account's user manager, which never sees them, so it would read
	// another config than the daemon's.
	ForeignEnv []string
	// UserManagerProblem (Linux only) says why this account's systemd user
	// manager cannot host the updater -- it does not linger -- and is ""
	// when it can, and on every other OS. The updater runs as a transient
	// unit of that manager, because stopping the system unit kills every
	// process in the unit's cgroup.
	UserManagerProblem string
	// Supervised reports whether this process is the system service's own
	// instance; SupervisedReason explains a false.
	Supervised       bool
	SupervisedReason string
}

// ServiceProbe describes the service definition the installer would own.
type ServiceProbe struct {
	// Path is where the definition lives (plist, unit file) or its name (the
	// Scheduled Task); shown in reasons.
	Path string
	// Found reports whether the definition exists and could be read; Err
	// carries why not when it exists but could not be read.
	Found bool
	Err   string
	// Marker reports whether it carries InstallerMarker.
	Marker bool
	// Program is the executable it runs, and RunsThisExe whether that is
	// the same file as Probe.Exe (os.SameFile, never a string compare: APFS
	// and NTFS are case-insensitive).
	Program     string
	RunsThisExe bool
}

// Detect classifies an install from its probe. It is KindInstaller only when
// every gate passes, checked in this order (the first failure is the reason):
//
//  1. a release build (IsReleaseBuild) -- else KindSource, or KindUnknown
//     for a release-stamped snapshot;
//  2. not under Homebrew's /Caskroom/ -- else KindHomebrew;
//  3. not a client-role machine -- else KindClient;
//     (with peer_auth on, the system gates follow instead: systemReason)
//  4. not root;
//  5. the installer's service definition exists, carries InstallerMarker,
//     and runs this very executable;
//  6. the config is the installer's fixed path;
//  7. no SEAMLESS_* overrides beyond SEAMLESS_CONFIG in the environment;
//  8. the executable's directory is writable;
//  9. this process is the supervised instance of that service.
//
// Failing 4-9 leaves a release build of unknown layout: KindUnknown, notified
// about updates but never updated unattended.
func Detect(p Probe) Install {
	switch {
	case p.Distribution != DistributionRelease:
		return Install{Kind: KindSource, Reason: "built from source (make install, go install or go build)", Hint: Hint(KindSource)}
	case !IsReleaseBuild(p.Distribution, p.Version):
		return Install{Kind: KindUnknown, Reason: fmt.Sprintf("version %q is not a published release", p.Version), Hint: Hint(KindUnknown)}
	case isCaskroom(p.Exe):
		return Install{Kind: KindHomebrew, Reason: "installed by Homebrew, which owns its files", Hint: Hint(KindHomebrew)}
	case p.Client:
		return Install{Kind: KindClient, Reason: "a client machine (role: client) runs no daemon of its own", Hint: Hint(KindClient)}
	}
	if p.PeerAuth {
		if reason := systemReason(p); reason != "" {
			return Install{Kind: KindUnknown, Reason: reason, Hint: SystemManualHint}
		}
		return Install{Kind: KindSystem, Hint: Hint(KindSystem)}
	}
	if reason := unknownReason(p); reason != "" {
		return Install{Kind: KindUnknown, Reason: reason, Hint: Hint(KindUnknown)}
	}
	return Install{Kind: KindInstaller, Hint: Hint(KindInstaller)}
}

// unknownReason returns why a release build fails gates 4-9, or "".
func unknownReason(p Probe) string {
	svc := p.Service
	switch {
	case p.Root:
		return "running as root, which the installer refuses"
	case p.Exe == "":
		return "the running executable could not be located"
	case svc.Err != "":
		return fmt.Sprintf("the service definition %s could not be read: %s", svc.Path, svc.Err)
	case !svc.Found:
		return fmt.Sprintf("no installer-managed service found (%s)", svc.Path)
	case !svc.Marker:
		return fmt.Sprintf("the service definition %s was not written by the installer", svc.Path)
	case !svc.RunsThisExe:
		return fmt.Sprintf("the service runs %s, not this executable (%s)", svc.Program, p.Exe)
	case !p.ConfigIsInstallers:
		if p.ConfigPath == "" {
			return "no config file is loaded; the installer manages ~/.config/seamless/seamless.yaml"
		}
		return fmt.Sprintf("the config is %s, not the installer's ~/.config/seamless/seamless.yaml", p.ConfigPath)
	case len(p.ForeignEnv) > 0:
		return fmt.Sprintf("%s set in the environment; the installer rewrites the service without it",
			strings.Join(p.ForeignEnv, ", "))
	case !p.ExeDirWritable:
		return "the executable's directory is not writable"
	case !p.Supervised:
		if p.SupervisedReason != "" {
			return "not the service's own process: " + p.SupervisedReason
		}
		return "not the service's own process (started by hand?)"
	}
	return ""
}

// systemReason returns why a peer_auth daemon is not KindSystem, or "". In
// order: not root; the executable located; running as the daemon's account;
// the system service found, readable, running this executable and starting
// it again after a clean exit; no SEAMLESS_* overrides the updater would run
// without (Linux); a config
// file loaded (the updater is started with its path); update.auto: true in
// it; the executable's directory writable (the swap renames into it); on
// Linux, the account's user manager lingering; and this process the service's
// own instance.
func systemReason(p Probe) string {
	sys := p.System
	svc := sys.Service
	switch {
	case p.Root:
		return "running as root; a peer_auth daemon runs as its own account"
	case p.Exe == "":
		return "the running executable could not be located"
	case !sys.Account:
		if sys.AccountReason != "" {
			return "not running as the daemon's account: " + sys.AccountReason
		}
		return "not running as the daemon's account"
	case svc.Err != "":
		return fmt.Sprintf("the system service %s could not be read: %s", svc.Path, svc.Err)
	case !svc.Found:
		return fmt.Sprintf("no system service found (%s)", svc.Path)
	case !svc.RunsThisExe:
		return fmt.Sprintf("the system service runs %s, not this executable (%s)", svc.Program, p.Exe)
	case !sys.Restarts:
		return fmt.Sprintf("the system service %s does not start the daemon again after it exits cleanly "+
			"(launchd's KeepAlive true, or systemd's Restart=always), which an in-place update needs", svc.Path)
	case len(sys.ForeignEnv) > 0:
		return fmt.Sprintf("%s set in the system unit's environment; the updater runs without it, so move it into the config file",
			strings.Join(sys.ForeignEnv, ", "))
	case p.ConfigPath == "":
		return "no config file is loaded"
	case !p.AutoOptIn:
		return "a peer_auth daemon updates itself only once its config sets update.auto: true"
	case !p.ExeDirWritable:
		return "the daemon's account cannot write the executable's directory: give that account the directory and the binaries in it " +
			"for automatic updates, or replace them with sudo"
	case sys.UserManagerProblem != "":
		return sys.UserManagerProblem
	case !sys.Supervised:
		if sys.SupervisedReason != "" {
			return "not the system service's own process: " + sys.SupervisedReason
		}
		return "not the system service's own process (started by hand?)"
	}
	return ""
}

// isCaskroom reports whether exe lives in a Homebrew Caskroom (macOS
// /opt/homebrew/Caskroom, /usr/local/Caskroom, or Linuxbrew's). Homebrew
// links the binaries into its bin directory, so this needs the path with
// symlinks resolved, which Probe.Exe is.
func isCaskroom(exe string) bool {
	return strings.Contains(strings.ReplaceAll(exe, `\`, "/"), "/Caskroom/")
}
