package main

// The relay's one root step on a Client (plan networked-relay; task
// 01M73RGB4PQVSXCN4V8VT5XNBX): what root does once so that `seamlessd relay
// serve` runs on this computer as an account of its own -- the account and a
// group holding only the owner (and the relay, which hands the socket to it),
// the root-owned /opt/seamless layout, root's own copy of seamlessd (checked
// against the binary setup ran as), the daemon-account pin, the pairing made
// as the relay's account, and the LaunchDaemon or systemd unit. Setup prints
// it and runs none of it (setupRelayStep): the commands are what the owner
// runs as an administrator, read before they run, and doctor's relay rows
// check the result (doctor_relay.go). Nothing here touches the machine.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/access"
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

// clientRelayPlan is what the root step needs to know.
type clientRelayPlan struct {
	goos string
	// owner is the account the agents run as: the socket group's member.
	owner string
	// server, fingerprint and caFile are the pairing's: the server's https
	// address, the certificate fingerprint the owner compared, and the copy
	// of that certificate to trust it by ("" when the system trusts it).
	server, fingerprint, caFile string
	// source is the seamlessd root copies, and sha256 its hash as setup
	// read it ("" skips the check).
	source, sha256 string
	// accountExists and groupExists: the account or the group is there
	// already, and is not made again. uid and gid are the ids a macOS account
	// and group are made with.
	accountExists, groupExists bool
	uid, gid                   int
	// pinned: root's pin names the relay's account already.
	pinned bool
}

// clientRelayRootStep is the root step as numbered blocks of commands: each
// block one line of why, then its commands.
func clientRelayRootStep(p clientRelayPlan) [][]string {
	wheel := "wheel"
	if p.goos == "linux" {
		wheel = "root"
	}
	acct, grp := clientRelayAccount, clientRelayGroup
	var out [][]string

	block := []string{"its account, a system account of its own, and a group holding you and it alone (the socket's)"}
	switch p.goos {
	case "darwin":
		if !p.accountExists {
			id := strconv.Itoa(p.uid)
			block = append(block,
				"sudo dscl . -create /Groups/"+acct+" PrimaryGroupID "+id,
				"sudo dscl . -create /Users/"+acct+" UniqueID "+id,
				"sudo dscl . -create /Users/"+acct+" PrimaryGroupID "+id,
				"sudo dscl . -create /Users/"+acct+" UserShell /usr/bin/false",
				"sudo dscl . -create /Users/"+acct+" NFSHomeDirectory "+clientRelayHome,
				"sudo dscl . -create /Users/"+acct+" Password '*'",
				"sudo dscl . -create /Users/"+acct+" IsHidden 1")
		}
		if !p.groupExists {
			block = append(block, "sudo dscl . -create /Groups/"+grp+" PrimaryGroupID "+strconv.Itoa(p.gid))
		}
		block = append(block, "sudo dscl . -append /Groups/"+grp+" GroupMembership "+p.owner+" "+acct)
	default:
		if !p.accountExists {
			block = append(block, "sudo useradd --system --user-group --shell /usr/sbin/nologin --home-dir "+clientRelayHome+" "+acct)
		}
		if !p.groupExists {
			block = append(block, "sudo groupadd --system "+grp)
		}
		block = append(block, "sudo usermod -aG "+grp+" "+p.owner, "sudo usermod -aG "+grp+" "+acct)
	}
	out = append(out, block)

	sum := "shasum -a 256 -c"
	if p.goos == "linux" {
		sum = "sha256sum -c"
	}
	block = []string{"the layout, root's own copy of seamlessd (no account but root can replace it), and the pin",
		"sudo install -d -o root -g " + wheel + " -m 0755 /opt/seamless /opt/seamless/bin /opt/seamless/etc",
		"sudo install -d -o " + acct + " -g " + acct + " -m 0755 /opt/seamless/run",
		"sudo install -d -o " + acct + " -g " + acct + " -m 0700 " + clientRelayHome + " " + relay.DefaultDataDir,
		"sudo install -o root -g " + wheel + " -m 0755 " + p.source + " " + relay.DefaultBinary}
	if p.sha256 != "" {
		block = append(block, fmt.Sprintf("echo '%s  %s' | %s", p.sha256, relay.DefaultBinary, sum))
	}
	if p.caFile != "" {
		block = append(block, "sudo install -o root -g "+wheel+" -m 0644 "+p.caFile+" "+clientRelayCAFile)
	}
	if !p.pinned {
		block = append(block,
			fmt.Sprintf(`echo "%s $(id -u %s)" | sudo tee %s >/dev/null`, acct, acct, access.DaemonAccountPath),
			"sudo chmod 0644 "+access.DaemonAccountPath)
	}
	out = append(out, block)

	pair := relay.DefaultBinary + " relay pair --server " + p.server
	if p.fingerprint != "" {
		pair += " --fingerprint " + p.fingerprint
	}
	if p.caFile != "" {
		pair += " --ca-file " + clientRelayCAFile
	}
	pair = "sudo -u " + acct + " " + pair + " --code -"
	out = append(out, []string{"pair it, as its own account, pasting the code when it waits: the key it makes stays in " + relay.DefaultDataDir +
		", which no other account reads", pair})

	if p.goos == "darwin" {
		out = append(out, []string{"its service, a LaunchDaemon that runs it as that account",
			"sudo tee " + clientRelayPlistPath() + " >/dev/null <<'EOF'\n" + clientRelayPlist() + "EOF",
			"sudo launchctl bootstrap system " + clientRelayPlistPath()})
	} else {
		out = append(out, []string{"its service, a system unit that runs it as that account",
			"sudo tee " + clientRelayUnitPath() + " >/dev/null <<'EOF'\n" + clientRelayUnitFile() + "EOF",
			"sudo systemctl daemon-reload",
			"sudo systemctl enable --now " + strings.TrimSuffix(clientRelayUnit, ".service")})
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
