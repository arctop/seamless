package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// The shared client config. Under peer_auth a client config holds no secret --
// `role: client` and the socket's server_url, since the kernel names the caller
// and no key is presented -- so the reason the config permissions row asks for
// an owner-only file is gone. What is left is who can REWRITE it: whoever can
// redirects every hook and bridge that reads it. A machine with several
// accounts therefore wants one file owned by root (or the daemon's account),
// readable by all and writable by no one else, outside every home -- the
// peer_auth guide's /opt/seamless/etc/client.yaml, which the hooks in Claude
// Code's managed settings name with --config. This file is the row's rule for
// it; every other config keeps configPermissionsCheck's.

// sharedConfigEnv is what the rule reads of the machine, injectable so a test
// can play a root-owned file without root.
type sharedConfigEnv struct {
	// inspect reports a path's owner and mode without following a final
	// symlink (inspectPath).
	inspect func(string) (fileFacts, error)
	// evalSymlinks resolves every symlink in a path (filepath.EvalSymlinks).
	evalSymlinks func(string) (string, error)
	// daemonAccount reads root's pin of the daemon's account
	// (access.DaemonAccount).
	daemonAccount func() (access.DaemonPin, bool, error)
}

func defaultSharedConfigEnv() sharedConfigEnv {
	return sharedConfigEnv{inspect: inspectPath, evalSymlinks: filepath.EvalSymlinks, daemonAccount: access.DaemonAccount}
}

// configFileCheck is doctor's "config permissions" row for the file cfg was
// loaded from.
//
//   - No file, Windows, a server-role config, or a file holding a secret
//     (config.FileSecrets: an API key, or a URL with userinfo): today's rule,
//     configPermissionsCheck -- owner-only, since whoever reads it holds the key.
//   - A server-role config with peer_auth on: an info row deferring to the
//     peer_auth rows, which hold it to serve's own rule (trustedFileProblem).
//     The owner-only rule would warn on the guide's root:_seamless 0640 layout
//     and offer a chmod 600 / chmod 700 that locks the daemon out of its config.
//   - A client config holding no secret, a regular file owned by root or the
//     pinned daemon account: info when it and every directory above it are
//     writable by no other account, else a warn naming what lets another
//     account rewrite it.
//   - A client config holding no secret owned by anyone else (the caller's own
//     ~/.config/seamless/seamless.yaml, say): today's rule, and a warning there
//     names the shared shape as the other way out.
func configFileCheck(cfg config.Config, goos string, env sharedConfigEnv) check {
	path := cfg.SourcePath()
	if path != "" && goos != "windows" && !cfg.IsClient() && cfg.PeerAuth.Enabled {
		return check{statusInfo, "config permissions", path + " is a peer_auth daemon's config: the peer_auth rows below hold it to serve's rule " +
			"(owned by root or the daemon's account, writable by no other account, and not readable by every account), not to owner-only"}
	}
	if path == "" || goos == "windows" || !cfg.IsClient() {
		return configPermissionsCheck(path, goos)
	}
	secrets, err := config.FileSecrets(path)
	if err != nil {
		// Not judged, so held to the strict rule, and the reason said.
		c := configPermissionsCheck(path, goos)
		c.detail += fmt.Sprintf(" (held to the owner-only rule: whether it holds a secret could not be read: %v)", err)
		return c
	}
	if len(secrets) > 0 {
		return configPermissionsCheck(path, goos)
	}
	if c, ok := sharedClientConfigCheck(path, env); ok {
		return c
	}
	c := configPermissionsCheck(path, goos)
	if c.status == statusWarn {
		c.detail += "; it holds no secret, so it may instead be a shared client config: owned by root, mode 0644, under directories only root can write (see the peer_auth guide's Restricted local accounts)"
	}
	return c
}

// sharedClientConfigCheck judges a secret-free client config at path as a
// shared one. ok is false when it is not one: not a regular file (a symlink
// included), or owned by neither root nor the pinned daemon account, or not
// inspectable on this platform. Those keep the owner-only rule.
func sharedClientConfigCheck(path string, env sharedConfigEnv) (check, bool) {
	const name = "config permissions"
	facts, err := env.inspect(path)
	if err != nil || facts.symlink || !facts.mode.IsRegular() {
		return check{}, false
	}
	// A pin that is there but not believed names no account: every client
	// refuses on it, and so does this rule (root still qualifies).
	pin, pinned, pinErr := env.daemonAccount()
	daemon := pinned && pinErr == nil
	trusted := func(uid uint32) bool { return uid == 0 || (daemon && uid == pin.UID) }
	if !trusted(facts.uid) {
		return check{}, false
	}
	owner, writers := "root", "root"
	if facts.uid != 0 {
		owner = "the daemon's account " + pin.String()
	}
	if daemon {
		writers = "root and the daemon's account"
	}

	lead := path + " holds no secret (a client config with no key)"
	perm := facts.mode.Perm()
	if perm&0o022 != 0 {
		return check{statusWarn, name, fmt.Sprintf(
			"%s, but mode %04o lets accounts other than %s rewrite it, and with it redirect every hook and bridge that reads it; run: sudo %s",
			lead, perm, owner, chmodRepairCommand("644", path))}, true
	}
	if p := sharedConfigDirProblem(path, trusted, env); p != "" {
		return check{statusWarn, name, lead + ", but " + p +
			": keep a shared client config under directories only " + writers + " can write, such as /opt/seamless/etc"}, true
	}
	return check{statusInfo, name, fmt.Sprintf(
		"%s and is owned by %s, mode %04o, under directories only %s can write: a shared client config, safe for every account to read, that no other account can redirect",
		lead, owner, perm, writers)}, true
}

// sharedConfigDirProblem walks every directory a shared config is reached
// through -- on its path as written, and on the path its directory resolves
// to -- and says why one could let another account replace the file, or ""
// when none could. Each must belong to a trusted account and be writable by no
// other, unless it is sticky (where only an entry's owner may rename or remove
// it). A symlink on the written path is judged by its owner alone: its mode
// bits mean nothing, and its directory is checked as the walk goes up.
func sharedConfigDirProblem(path string, trusted func(uint32) bool, env sharedConfigEnv) string {
	dir := filepath.Dir(filepath.Clean(path))
	starts := []string{dir}
	real, err := env.evalSymlinks(dir)
	if err != nil {
		return fmt.Sprintf("its directory %s cannot be resolved: %v", dir, err)
	}
	if real != dir {
		starts = append(starts, real)
	}
	for _, start := range starts {
		for d := start; ; d = filepath.Dir(d) {
			facts, err := env.inspect(d)
			switch {
			case err != nil:
				return fmt.Sprintf("directory %s above it cannot be inspected: %v", d, err)
			case !trusted(facts.uid):
				return fmt.Sprintf("directory %s above it is owned by uid %d, who could rename the file away and put another in its place", d, facts.uid)
			case !facts.symlink && facts.mode.Perm()&0o022 != 0 && facts.mode&os.ModeSticky == 0:
				return fmt.Sprintf("directory %s above it is mode %04o, so other accounts could rename the file away and put another in its place", d, facts.mode.Perm())
			}
			if next := filepath.Dir(d); next == d {
				break
			}
		}
	}
	return ""
}
