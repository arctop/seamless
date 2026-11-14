package main

// What a re-run of setup finds installed: the config's role, peer_auth, and
// the service -- read through an injected host, so a test stands any layout up
// from a scratch home.

import (
	"errors"
	"io/fs"
	"net"
	"path/filepath"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

// setupHost is the slice of the machine setup reads: the service probes
// install_probe.go already has (the per-user service an installer writes, a
// peer_auth system service), and a stat.
type setupHost struct {
	probe  installProbeEnv
	exists func(path string) bool
}

// realSetupHost reads this machine.
func realSetupHost() setupHost {
	return setupHost{probe: realInstallProbeEnv(), exists: fileExists}
}

// setupFacts is what setup knows about this computer before it asks anything.
type setupFacts struct {
	// configPath is the config the installers manage: $SEAMLESS_CONFIG when set
	// (the installers set it to that file, or leave it unset), else
	// ~/.config/seamless/seamless.yaml.
	configPath  string
	configFound bool
	// role, serverURL and addr are the config's own top-level values, read
	// the way the installers read them (installerConfigValue): the file, never
	// an environment override.
	role, serverURL, addr string
	// userService: the per-user service an installer registers (a LaunchAgent,
	// a systemd user unit, the Scheduled Task). systemService: a peer_auth
	// daemon's (a LaunchDaemon, a system unit).
	userService, systemService bool
	// localData: the default data dir holds a database.
	localData bool
}

// setupFactsOf reads the facts from h.
func setupFactsOf(h setupHost) setupFacts {
	env := h.probe
	var f setupFacts
	f.configPath = envValue(env.environ, "SEAMLESS_CONFIG")
	if f.configPath == "" && env.home != "" {
		f.configPath = filepath.Join(env.home, ".config", "seamless", "seamless.yaml")
	}
	if f.configPath != "" {
		data, err := env.readFile(f.configPath)
		// A config that is there but cannot be read reads as one with no
		// values, as the installers' sed does: role unset is today's install.
		f.configFound = err == nil || !errors.Is(err, fs.ErrNotExist)
		if err == nil {
			f.role = installerConfigValue(data, "role")
			f.serverURL = installerConfigValue(data, "server_url")
			f.addr = installerConfigValue(data, "addr")
		}
	}
	if env.home != "" {
		f.userService = probeService(env).Found && setupServiceMine(env)
		f.localData = h.exists(filepath.Join(env.home, ".seamless", "seam.db"))
	}
	if setupPeerAuthOS(env.goos) {
		sp, _ := probeSystemService(env)
		f.systemService = sp.Found
	}
	return f
}

// setupServiceMine reports whether the per-user service probeService found is
// this user's. A LaunchAgent and a systemd user unit live under the home; the
// Scheduled Task's name is the whole machine's, and on a box another user
// installed first it is theirs -- the box where this user is to be a Client
// (docs-src/guides/network-install.md, "Windows: a second user on one box").
func setupServiceMine(env installProbeEnv) bool {
	if env.goos != "windows" {
		return true
	}
	raw, err := env.taskXML()
	if err != nil || env.isCurrentUser == nil {
		return false
	}
	def, err := parseTaskXML(raw)
	if err != nil || def.UserID == "" {
		return false
	}
	mine, err := env.isCurrentUser(def.UserID)
	return err == nil && mine
}

// installerConfigValue is a top-level scalar of a config file, read exactly as
// docs/install's config_value (and install.ps1's Get-ConfigValue) reads it:
// the first line that starts with key and a colon; blanks after the colon
// skipped, then one optional double quote; the value runs to the next double
// quote or blank. "" when no line names it. setup decides what the installer
// acts on from these, so it must read the same answer the installer does --
// not the YAML parse, which would read role: 'client' as a client the
// installer does not see. installerConfigValue_test.go holds it to the sh.
func installerConfigValue(data []byte, key string) string {
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(line, key+":")
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " \t\r\v\f")
		rest = strings.TrimPrefix(rest, `"`)
		if end := strings.IndexAny(rest, "\" \t\r\v\f"); end >= 0 {
			rest = rest[:end]
		}
		return rest
	}
	return ""
}

// setupInstall is the kind setup finds installed, and what the installer acts
// on for it.
type setupInstall struct {
	kind setupKind
	// locked: locked projects are on -- peer_auth runs this computer's daemon.
	locked bool
	// role and serverURL are what the installer acts on, as it reads them
	// itself: a role: client config dials its server_url (one trailing slash
	// dropped, as the installers drop it); anything else runs the daemon here.
	role, serverURL string
}

// setupInstalledOf classifies the facts:
//
//   - a role: client config is a Client of its server -- unless the server is
//     a Unix socket: that is the owner's half of a peer_auth install, whose
//     daemon runs on this computer under an account of its own (Standalone,
//     with locked projects);
//   - any other config runs the daemon here: the Server when it binds, or
//     names itself at, an address other computers reach; else Standalone;
//   - no config, and still a service or a database: Standalone, the config
//     lost (the installer writes a new one, as on a first install);
//   - nothing at all: a first install.
//
// A peer_auth install that also serves other computers reads as Standalone:
// the daemon's config is its own account's, and the owner's cannot read it.
func setupInstalledOf(f setupFacts) setupInstall {
	in := setupInstall{role: config.RoleServer}
	switch {
	case f.configFound && f.role == config.RoleClient:
		in.role, in.serverURL = config.RoleClient, strings.TrimSuffix(f.serverURL, "/")
		in.kind = setupClient
		if strings.HasPrefix(strings.ToLower(in.serverURL), "unix:") {
			in.kind, in.locked = setupStandalone, true
		}
	case f.configFound:
		in.kind, in.locked = setupStandalone, f.systemService
		if setupNetworkedConfig(f.addr, f.serverURL) {
			in.kind = setupServer
		}
	case f.systemService:
		in.kind, in.locked = setupStandalone, true
	case f.userService || f.localData:
		in.kind = setupStandalone
	default:
		in.kind = setupNone
	}
	return in
}

// setupNetworkedConfig reports whether a server config is reached from other
// computers: its addr binds every interface or one that is not loopback, or
// its server_url names a host that is not loopback. An unset addr is the
// default loopback bind.
func setupNetworkedConfig(addr, serverURL string) bool {
	if addr != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && (config.IsWildcardHost(host) || !isLoopbackServerHost(host)) {
			return true
		}
	}
	if serverURL == "" || strings.HasPrefix(strings.ToLower(serverURL), "unix:") {
		return false
	}
	cfg := config.Config{AdvertisedURL: serverURL}
	host := cfg.ServerHost()
	return host != "" && !isLoopbackServerHost(host)
}
