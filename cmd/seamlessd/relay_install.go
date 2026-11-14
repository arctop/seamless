package main

// The relay's one root step on a Client (plan networked-relay; task
// 01M73RGB4PQVSXCN4V8VT5XNBX): what root does once so that `seamlessd relay
// serve` runs on this computer as an account of its own -- the account and a
// group holding only the owner (and the relay, which hands the socket to it),
// the root-owned /opt/seamless layout, root's own copy of seamlessd (staged
// and checked against the binary setup ran as before it goes into place), the
// daemon-account pin, the pairing made as the relay's account, and the
// LaunchDaemon or systemd unit. Setup prints it and runs none of it
// (setupRelayStep): the commands are what the owner runs as an administrator,
// read before they run, so they hold even against a process of the owner's
// that rewrote every file the owner can write (rootstep_commands.go), and
// doctor's relay rows check the result (doctor_relay.go). Nothing here
// touches the machine.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/relay"
)

// The relay's names on a Client: its account (the name the peer_auth guide
// gives the daemon's, pinned as the daemon's on this computer, where no
// daemon runs), the group its socket is handed to, and its service.
const (
	clientRelayAccount = "_seamless"
	clientRelayGroup   = "_seamlessowner"
	clientRelayLabel   = launchdLabel + ".relay"
	clientRelayUnit    = "seamless-relay.service"
	clientRelayHome    = "/opt/seamless/home"
	clientRelayCAFile  = "/opt/seamless/etc/server-ca.crt"
	// clientRelayUID is where the search for a free macOS role id starts
	// (the guide's example), going down.
	clientRelayUID = 480
)

// clientRelayPlistPath and clientRelayUnitPath are where the service is
// written.
func clientRelayPlistPath() string { return systemDaemonPlistDir + "/" + clientRelayLabel + ".plist" }
func clientRelayUnitPath() string  { return "/etc/systemd/system/" + clientRelayUnit }

// clientRelayServeArgs is the relay's command line: the restrictive socket of
// task 01M7CPC4D1AQXBDTE7G399PNXM's recommendation (a): 0660, handed to a
// group of the owner's alone, so no other account on this computer reaches
// the server as the owner through it, whether or not the server judges uids.
func clientRelayServeArgs() []string {
	return []string{relay.DefaultBinary, "relay", "serve", "--socket-mode", "0660", "--socket-group", clientRelayGroup}
}

// clientRelayStaged is where root's copy of seamlessd is staged and checked
// before it goes into place: in root's own directory, which no other account
// can write or make a file in.
const clientRelayStaged = "/opt/seamless/bin/.seamlessd.new"

// clientRelayPlan is what the root step needs to know.
type clientRelayPlan struct {
	goos string
	// owner is the account the agents run as: the socket group's member.
	owner string
	// server and fingerprint are the pairing's: the server's https address and
	// the certificate fingerprint the owner compared.
	server, fingerprint string
	// caPEM is that certificate, as PEM, when this computer's roots do not
	// vouch for it ("" when they do): the step prints it, and root writes its
	// own copy from there, reading no file of the owner's.
	caPEM string
	// source is the seamlessd root's copy is made from, opened by the owner's
	// shell, and sha256 its hash as setup read it: the staged copy must match.
	source, sha256 string
	// accountExists and groupExists: the account or the group is there
	// already, and is not made again. uid and gid are the ids a macOS account
	// and group are made with.
	accountExists, groupExists bool
	uid, gid                   int
	// pinned: root's pin names the relay's account already.
	pinned bool
}

// problem says why p cannot be printed as a root step, "" when it can: every
// value root's commands carry has its format, whatever the state file said.
// The commands quote each value as well; this keeps a wrong one from being
// printed at all.
func (p clientRelayPlan) problem() string {
	switch {
	case p.goos != "darwin" && p.goos != "linux":
		return "the relay runs on macOS and Linux only"
	case p.owner == "" || strings.HasPrefix(p.owner, "-") || strings.ContainsFunc(p.owner, setupControl) || strings.ContainsAny(p.owner, " \t"):
		return fmt.Sprintf("%q is not an account name", p.owner)
	case !strings.HasPrefix(p.server, "https://"):
		return fmt.Sprintf("the server address %q is not an https address", config.RedactedURL(p.server))
	}
	if why := setupServerURLProblem(p.server); why != "" {
		return fmt.Sprintf("the server address %q: %s", config.RedactedURL(p.server), why)
	}
	switch {
	case !devices.ValidCertFingerprint(p.fingerprint):
		return fmt.Sprintf("%q is not a certificate's SHA-256 fingerprint", p.fingerprint)
	case !strings.HasPrefix(p.source, "/") || strings.ContainsFunc(p.source, setupControl):
		return fmt.Sprintf("the seamlessd to copy, %q, is not a full path", p.source)
	case !rootstepSHA256(p.sha256):
		return fmt.Sprintf("%q is not the SHA-256 of the seamlessd setup ran as", p.sha256)
	case p.goos == "darwin" && !p.accountExists && (p.uid <= 200 || p.uid >= 500):
		return "no id below 500 is free for the relay's account"
	case p.goos == "darwin" && !p.groupExists && (p.gid <= 200 || p.gid >= 500):
		return "no id below 500 is free for the socket's group"
	}
	return ""
}

// clientRelayRootStep is the root step as numbered blocks of commands: each
// block one line of why, then its commands, which rootstepChain joins into
// one AND-list to paste whole. Every tool is named by its absolute path and
// every value quoted (rootstep_commands.go). Root opens no file the owner can
// write: the owner's shell opens the seamlessd to copy, root stages it in its
// own directory and checks it there before it goes into place, and the
// certificate the owner compared is printed for root to write.
func clientRelayRootStep(p clientRelayPlan) [][]string {
	t := rootstepToolsFor(p.goos)
	wheel := "wheel"
	if p.goos == "linux" {
		wheel = "root"
	}
	acct, grp, owner := clientRelayAccount, clientRelayGroup, posixShellArg(p.owner)
	var out [][]string

	block := []string{"its account, a system account of its own, and a group holding you and it alone (the socket's)"}
	switch p.goos {
	case "darwin":
		if !p.accountExists {
			id := strconv.Itoa(p.uid)
			block = append(block,
				t.asRoot(t.dscl, ".", "-create", "/Groups/"+acct, "PrimaryGroupID", id),
				t.asRoot(t.dscl, ".", "-create", "/Users/"+acct, "UniqueID", id),
				t.asRoot(t.dscl, ".", "-create", "/Users/"+acct, "PrimaryGroupID", id),
				t.asRoot(t.dscl, ".", "-create", "/Users/"+acct, "UserShell", "/usr/bin/false"),
				t.asRoot(t.dscl, ".", "-create", "/Users/"+acct, "NFSHomeDirectory", clientRelayHome),
				t.asRoot(t.dscl, ".", "-create", "/Users/"+acct, "Password", "'*'"),
				t.asRoot(t.dscl, ".", "-create", "/Users/"+acct, "IsHidden", "1"))
		}
		if !p.groupExists {
			block = append(block, t.asRoot(t.dscl, ".", "-create", "/Groups/"+grp, "PrimaryGroupID", strconv.Itoa(p.gid)))
		}
		block = append(block, t.asRoot(t.dscl, ".", "-append", "/Groups/"+grp, "GroupMembership", owner, acct))
	default:
		if !p.accountExists {
			block = append(block, t.asRoot(t.useradd, "--system", "--user-group", "--shell", "/usr/sbin/nologin", "--home-dir", clientRelayHome, acct))
		}
		if !p.groupExists {
			block = append(block, t.asRoot(t.groupadd, "--system", grp))
		}
		block = append(block, t.asRoot(t.usermod, "-aG", grp, owner), t.asRoot(t.usermod, "-aG", grp, acct))
	}
	out = append(out, block)

	block = []string{"the layout, root's own copy of seamlessd (no account but root can replace it), and the pin",
		t.asRoot(t.install, "-d", "-o", "root", "-g", wheel, "-m", "0755", "/opt/seamless", "/opt/seamless/bin", "/opt/seamless/etc"),
		t.asRoot(t.install, "-d", "-o", acct, "-g", acct, "-m", "0755", "/opt/seamless/run"),
		t.asRoot(t.install, "-d", "-o", acct, "-g", acct, "-m", "0700", clientRelayHome, relay.DefaultDataDir)}
	// The owner's shell opens the source and root only writes what it reads,
	// into a copy that is root's alone until it passes its check.
	block = append(block, rootstepCopyCommands(t, rootstepCopy{source: p.source, sha256: p.sha256, staged: clientRelayStaged,
		dest: relay.DefaultBinary, owner: "root", group: wheel, mode: "0755"})...)
	if p.caPEM != "" {
		block = append(block, rootstepHeredoc(t.asRoot(t.tee, clientRelayCAFile, ">/dev/null"), p.caPEM),
			t.asRoot(t.chmod, "0644", clientRelayCAFile))
	}
	if !p.pinned {
		block = append(block, rootstepPinCommands(t, acct)...)
	}
	out = append(out, block)

	pair := []string{t.sudo, "-u", acct, "-H", relay.DefaultBinary, "relay", "pair", "--server", posixShellArg(p.server)}
	if p.fingerprint != "" {
		pair = append(pair, "--fingerprint", posixShellArg(p.fingerprint))
	}
	if p.caPEM != "" {
		pair = append(pair, "--ca-file", clientRelayCAFile)
	}
	pair = append(pair, "--code", "-")
	out = append(out, []string{"pair it, as its own account, pasting the code when it waits: the key it makes stays in " + relay.DefaultDataDir +
		", which no other account reads", strings.Join(pair, " ")})

	if p.goos == "darwin" {
		out = append(out, []string{"its service, a LaunchDaemon that runs it as that account",
			rootstepHeredoc(t.asRoot(t.tee, clientRelayPlistPath(), ">/dev/null"), clientRelayPlist()),
			t.asRoot(t.launchctl, "bootstrap", "system", clientRelayPlistPath())})
	} else {
		out = append(out, []string{"its service, a system unit that runs it as that account",
			rootstepHeredoc(t.asRoot(t.tee, clientRelayUnitPath(), ">/dev/null"), clientRelayUnitFile()),
			t.asRoot(t.systemctl, "daemon-reload"),
			t.asRoot(t.systemctl, "enable", "--now", strings.TrimSuffix(clientRelayUnit, ".service"))})
	}
	return out
}

// clientRelayPlist is the relay's LaunchDaemon (the peer_auth daemon's, run
// as relay serve): its account, its groups (the socket's among them), umask
// 077, kept alive.
func clientRelayPlist() string {
	var args strings.Builder
	for _, a := range clientRelayServeArgs() {
		args.WriteString("    <string>" + a + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>` + clientRelayLabel + `</string>
  <key>UserName</key>          <string>` + clientRelayAccount + `</string>
  <key>GroupName</key>         <string>` + clientRelayAccount + `</string>
  <key>InitGroups</key>        <true/>
  <key>ProgramArguments</key>
  <array>
` + args.String() + `  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>            <string>` + clientRelayHome + `</string>
  </dict>
  <key>Umask</key>             <integer>63</integer>
  <key>RunAtLoad</key>         <true/>
  <key>KeepAlive</key>         <true/>
  <key>ThrottleInterval</key>  <integer>10</integer>
  <key>StandardOutPath</key>   <string>` + clientRelayHome + `/relay.log</string>
  <key>StandardErrorPath</key> <string>` + clientRelayHome + `/relay.log</string>
</dict>
</plist>
`
}

// clientRelayUnitFile is the relay's systemd system unit.
func clientRelayUnitFile() string {
	return `[Unit]
Description=Seamless relay
After=network-online.target
Wants=network-online.target

[Service]
User=` + clientRelayAccount + `
Group=` + clientRelayAccount + `
UMask=0077
Environment=HOME=` + clientRelayHome + `
ExecStart=` + strings.Join(clientRelayServeArgs(), " ") + `
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`
}

// clientRelayFreeIDs picks a macOS role id for the relay's account (free as a
// user id and as a group id, both being made) and another for the socket's
// group, searching down from clientRelayUID; taken reports an id in use.
func clientRelayFreeIDs(taken func(id int) bool) (uid, gid int) {
	uid, gid = -1, -1
	for id := clientRelayUID; id > 200 && (uid < 0 || gid < 0); id-- {
		if taken(id) {
			continue
		}
		if uid < 0 {
			uid = id
			continue
		}
		gid = id
	}
	return uid, gid
}
