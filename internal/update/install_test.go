package update

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// installerProbe is a probe for which every gate passes.
func installerProbe() Probe {
	return Probe{
		Distribution:       DistributionRelease,
		Version:            "0.7.2",
		Exe:                "/Users/me/.local/bin/seamlessd",
		ExeDirWritable:     true,
		ConfigIsInstallers: true,
		ConfigPath:         "/Users/me/.config/seamless/seamless.yaml",
		Service: ServiceProbe{
			Path:        "/Users/me/Library/LaunchAgents/com.arctop.seamless.plist",
			Found:       true,
			Marker:      true,
			Program:     "/Users/me/.local/bin/seamlessd",
			RunsThisExe: true,
		},
		Supervised: true,
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(p *Probe)
		wantKind   Kind
		wantReason string // substring; "" for the installer kind
		wantHint   string
	}{
		{"every gate passes", func(*Probe) {}, KindInstaller, "", "seamlessd update"},
		{"source build", func(p *Probe) { p.Distribution = DistributionSource }, KindSource, "built from source", "git pull && make install"},
		{"unstamped build", func(p *Probe) { p.Distribution = "" }, KindSource, "built from source", "git pull && make install"},
		{"release-stamped snapshot", func(p *Probe) { p.Version = "0.7.3-SNAPSHOT-abc1234" }, KindUnknown, "not a published release", "seamlessd update"},
		{"homebrew on macOS", func(p *Probe) { p.Exe = "/opt/homebrew/Caskroom/seamless/0.7.2/seamlessd" }, KindHomebrew, "Homebrew", "brew upgrade --cask arctop/tap/seamless"},
		{"linuxbrew", func(p *Probe) { p.Exe = "/home/linuxbrew/.linuxbrew/Caskroom/seamless/0.7.2/seamlessd" }, KindHomebrew, "Homebrew", "brew upgrade --cask arctop/tap/seamless"},
		{"client role", func(p *Probe) { p.Client = true }, KindClient, "role: client", "client-config"},
		{"root", func(p *Probe) { p.Root = true }, KindUnknown, "root", "seamlessd update"},
		{"exe unknown", func(p *Probe) { p.Exe = "" }, KindUnknown, "could not be located", "seamlessd update"},
		{"service unreadable", func(p *Probe) { p.Service.Err = "permission denied" }, KindUnknown, "could not be read", "seamlessd update"},
		{"no service (binaries only)", func(p *Probe) { p.Service.Found = false }, KindUnknown, "no installer-managed service", "seamlessd update"},
		{"make install plist (no marker)", func(p *Probe) { p.Service.Marker = false }, KindUnknown, "not written by the installer", "seamlessd update"},
		{"service runs another binary", func(p *Probe) {
			p.Service.RunsThisExe = false
			p.Service.Program = "/opt/other/seamlessd"
		}, KindUnknown, "/opt/other/seamlessd", "seamlessd update"},
		{"custom config", func(p *Probe) {
			p.ConfigIsInstallers = false
			p.ConfigPath = "/srv/seamless.yaml"
		}, KindUnknown, "/srv/seamless.yaml", "seamlessd update"},
		{"no config loaded", func(p *Probe) {
			p.ConfigIsInstallers = false
			p.ConfigPath = ""
		}, KindUnknown, "no config file", "seamlessd update"},
		{"env overrides the installer would drop", func(p *Probe) {
			p.ForeignEnv = []string{"SEAMLESS_ADDR", "SEAMLESS_DATA_DIR"}
		}, KindUnknown, "SEAMLESS_ADDR, SEAMLESS_DATA_DIR", "seamlessd update"},
		{"read-only exe dir", func(p *Probe) { p.ExeDirWritable = false }, KindUnknown, "not writable", "seamlessd update"},
		{"started by hand", func(p *Probe) { p.Supervised = false }, KindUnknown, "started by hand", "seamlessd update"},
		{"started by hand with a reason", func(p *Probe) {
			p.Supervised = false
			p.SupervisedReason = "parent is not launchd"
		}, KindUnknown, "parent is not launchd", "seamlessd update"},
		// Gate order: the first failing gate names the reason.
		{"source wins over homebrew", func(p *Probe) {
			p.Distribution = DistributionSource
			p.Exe = "/opt/homebrew/Caskroom/seamless/0.7.2/seamlessd"
		}, KindSource, "built from source", "git pull && make install"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := installerProbe()
			tt.mutate(&p)
			got := Detect(p)
			require.Equal(t, tt.wantKind, got.Kind)
			require.Contains(t, got.Hint, tt.wantHint)
			if tt.wantReason == "" {
				require.Empty(t, got.Reason)
				require.False(t, got.NotifyOnly())
				return
			}
			require.Contains(t, got.Reason, tt.wantReason)
			require.True(t, got.NotifyOnly())
		})
	}
}

// systemProbe is a peer_auth system daemon's probe for which every gate passes.
func systemProbe() Probe {
	return Probe{
		Distribution:   DistributionRelease,
		Version:        "0.8.1",
		Exe:            "/opt/seamless/bin/seamlessd",
		ExeDirWritable: true,
		ConfigPath:     "/opt/seamless/etc/seamless.yaml",
		PeerAuth:       true,
		AutoOptIn:      true,
		System: SystemProbe{
			Account: true,
			Service: ServiceProbe{
				Path:        "/Library/LaunchDaemons/com.arctop.seamless.daemon.plist",
				Found:       true,
				Program:     "/opt/seamless/bin/seamlessd",
				RunsThisExe: true,
			},
			Restarts:   true,
			Supervised: true,
		},
	}
}

func TestDetectSystem(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(p *Probe)
		wantKind   Kind
		wantReason string // substring; "" for the system kind
	}{
		{"every gate passes", func(*Probe) {}, KindSystem, ""},
		// The build gates come first, as for every install.
		{"source build", func(p *Probe) { p.Distribution = DistributionSource }, KindSource, "built from source"},
		{"release-stamped snapshot", func(p *Probe) { p.Version = "0.8.1-SNAPSHOT-abc1234" }, KindUnknown, "not a published release"},
		{"client role", func(p *Probe) { p.Client = true }, KindClient, "role: client"},
		// With peer_auth on, the installer's gates never apply: no marker, no
		// installer config, and SEAMLESS_* in the unit's environment are all fine.
		{"installer facts are irrelevant", func(p *Probe) {
			p.ForeignEnv = []string{"SEAMLESS_CONFIG_EXTRA"}
			p.Service = ServiceProbe{}
		}, KindSystem, ""},
		{"root", func(p *Probe) { p.Root = true }, KindUnknown, "root"},
		{"exe unknown", func(p *Probe) { p.Exe = "" }, KindUnknown, "could not be located"},
		{"not the daemon's account", func(p *Probe) {
			p.System.Account = false
			p.System.AccountReason = "uid 501 is not the pinned _seamless (uid 480)"
		}, KindUnknown, "not running as the daemon's account: uid 501"},
		{"service unreadable", func(p *Probe) { p.System.Service.Err = "permission denied" }, KindUnknown, "could not be read"},
		{"no system service", func(p *Probe) { p.System.Service.Found = false }, KindUnknown, "no system service found"},
		{"service runs another binary", func(p *Probe) {
			p.System.Service.RunsThisExe = false
			p.System.Service.Program = "/usr/local/bin/seamlessd"
		}, KindUnknown, "/usr/local/bin/seamlessd"},
		{"no restart after a clean exit", func(p *Probe) { p.System.Restarts = false }, KindUnknown, "does not start the daemon again"},
		{"env overrides the updater would run without", func(p *Probe) {
			p.System.ForeignEnv = []string{"SEAMLESS_ADDR"}
		}, KindUnknown, "SEAMLESS_ADDR set in the system unit's environment"},
		{"no config loaded", func(p *Probe) { p.ConfigPath = "" }, KindUnknown, "no config file"},
		{"not opted in", func(p *Probe) { p.AutoOptIn = false }, KindUnknown, "update.auto: true"},
		{"root-owned bin dir", func(p *Probe) { p.ExeDirWritable = false }, KindUnknown, "cannot write the executable's directory"},
		{"no lingering user manager", func(p *Probe) {
			p.System.UserManagerProblem = "the user manager of _seamless does not linger"
		}, KindUnknown, "does not linger"},
		{"started by hand", func(p *Probe) { p.System.Supervised = false }, KindUnknown, "started by hand"},
		{"started by hand with a reason", func(p *Probe) {
			p.System.Supervised = false
			p.System.SupervisedReason = "not running as the launchd job com.arctop.seamless.daemon"
		}, KindUnknown, "com.arctop.seamless.daemon"},
		// An installer layout with peer_auth switched on is no installer install.
		{"installer layout with peer_auth on", func(p *Probe) {
			*p = installerProbe()
			p.PeerAuth = true
		}, KindUnknown, "not running as the daemon's account"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := systemProbe()
			tt.mutate(&p)
			got := Detect(p)
			require.Equal(t, tt.wantKind, got.Kind)
			if tt.wantReason == "" {
				require.Empty(t, got.Reason)
				require.False(t, got.NotifyOnly())
				require.Equal(t, SystemHint, got.Hint)
				return
			}
			require.Contains(t, got.Reason, tt.wantReason)
			require.True(t, got.NotifyOnly())
			if p.PeerAuth && got.Kind == KindUnknown && IsReleaseBuild(p.Distribution, p.Version) {
				require.Equal(t, SystemManualHint, got.Hint, "seamlessd update refuses a peer_auth layout")
			}
		})
	}
}

func TestIsReleaseBuild(t *testing.T) {
	require.True(t, IsReleaseBuild(DistributionRelease, "0.7.2"))
	require.False(t, IsReleaseBuild(DistributionRelease, "0.7.2-SNAPSHOT-abc"))
	require.False(t, IsReleaseBuild(DistributionRelease, "0.0.0-dev"))
	require.False(t, IsReleaseBuild(DistributionSource, "0.7.2"))
	require.False(t, IsReleaseBuild("", "0.7.2"))
}

func TestInstallerMarkerMatchesBothInstallers(t *testing.T) {
	// The marker docs/install writes into the plist and the unit, and the
	// PowerShell installer's spelling, both carry the prefix Detect looks for.
	require.Contains(t, "<!-- Written by https://thereisnospoon.org/install. Re-run it to update. -->", InstallerMarker)
	require.Contains(t, "# Written by https://thereisnospoon.org/install. Re-run it to update.", InstallerMarker)
	require.Contains(t, "Written by https://thereisnospoon.org/install.ps1. Re-run it to update.", InstallerMarker)
}
