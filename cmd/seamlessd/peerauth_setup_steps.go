package main

// The steps of seamlessd peer-auth setup and of its undo (peerauth_setup.go):
// each one's commands, built when it runs from the plan and the world as it
// is then, so a step a stopped run half did skips what is there already.
// Every root command names its tool by absolute path and quotes every value
// (rootstep_commands.go); root opens no file the owner can write -- the
// owner's shell opens each source and root writes what it reads into a
// staged copy it checks before the copy goes into place.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/update"
)

// pasSteps is the move, in the guide's order: back up, the account and the
// layout, the binaries, the daemon's config, the per-user service retired,
// the data moved, the system service, its health and doctor, then the
// owner's side: the client config and the hooks.
var pasSteps = []pasStepDef{
	{id: "backup", build: pasBackupCommands,
		title: func(pasPlan) string { return "export everything into " + pasBackupPath + ", root's alone" }},
	{id: "account", build: pasAccountCommands,
		title: func(p pasPlan) string {
			return "the account " + p.Account + ", the " + pasRoot + " layout, and the pin naming it as the daemon's"
		}},
	{id: "binaries", build: pasBinaryCommands,
		title: func(p pasPlan) string {
			return "the binaries into " + pasBin + ", each checked in root's staging first"
		}},
	{id: "config", build: pasConfigCommands,
		title: func(p pasPlan) string { return "the daemon's config " + pasConfig + pasTLSWords(p) }},
	{id: "retire", build: pasRetireCommands, after: pasWaitOldDaemonGone,
		afterWords: func(p pasPlan) string { return "wait until no daemon answers at " + p.ServerURL },
		title:      func(p pasPlan) string { return "stop and retire the per-user service" }},
	{id: "data", build: pasMoveDataCommands,
		title: func(p pasPlan) string {
			return "move " + pasHomeWords(p, p.OldDataDir) + " to " + pasData + ", the daemon's account's"
		}},
	{id: "service", build: pasServiceCommands,
		title: func(p pasPlan) string {
			if p.GOOS == "darwin" {
				return "install and start the LaunchDaemon " + systemDaemonLabel
			}
			return "install and start the system unit " + systemDaemonUnit
		}},
	{id: "health", after: pasWaitHealthy,
		afterWords: func(pasPlan) string { return "wait until the daemon answers on " + pasSocket },
		title:      func(pasPlan) string { return "wait until the daemon serves, on the socket" }},
	{id: "doctor", build: pasDoctorCommands,
		title: func(p pasPlan) string { return "seamlessd doctor, as " + p.Account }},
	{id: "client", after: pasSwitchClientConfig,
		afterWords: func(p pasPlan) string {
			return "set " + pasHomeWords(p, p.OldConfig) + " aside as " + filepath.Base(p.OldConfig) + pasKeptSuffix +
				", write the client config there and " + pasHomeWords(p, pasConsoleConfig(p))
		},
		title: func(p pasPlan) string {
			return "your config becomes a client's, on the socket; " + filepath.Base(pasConsoleConfig(p)) + " keeps the key for the console"
		}},
	{id: "hooks", after: pasReinstallHooks,
		afterWords: func(p pasPlan) string { return "seamlessd install-hooks --seam " + pasBin + "/seam" },
		title:      func(pasPlan) string { return "reinstall the hooks and the MCP bridge to run " + pasBin + "/seam" }},
}

// pasUndoSteps is the undo, in the guide's "Roll back" order, for the steps
// it reverses. The backup, the account and layout, the binaries and the
// daemon's config stay: inert without the service, and the next setup reuses
// them.
var pasUndoSteps = []pasStepDef{
	{id: "service", build: pasUndoServiceCommands, after: pasWaitDaemonGone,
		title: func(p pasPlan) string {
			if p.GOOS == "darwin" {
				return "stop and remove the LaunchDaemon " + systemDaemonLabel
			}
			return "stop and remove the system unit " + systemDaemonUnit
		}},
	{id: "data", build: pasUndoDataCommands,
		title: func(p pasPlan) string {
			return "move " + pasData + " back to " + pasHomeWords(p, p.OldDataDir) + ", yours again"
		}},
	{id: "retire", build: pasUndoRetireCommands,
		title: func(pasPlan) string { return "put the per-user service back and start it" }},
	{id: "client", after: pasUndoClientConfig,
		title: func(p pasPlan) string { return "put your server config back at " + pasHomeWords(p, p.OldConfig) }},
	{id: "hooks", after: pasUndoHooks,
		title: func(pasPlan) string { return "reinstall the hooks to run the seam beside your seamlessd" }},
}

// pasBackupCommands export the whole instance, as the owner and while the old
// daemon still serves, into a file root made empty and its own alone first;
// tar then reads the copy root holds, so a failed export stops the step
// rather than leaving an empty backup behind it.
func pasBackupCommands(x *pasRun) ([]string, error) {
	t := x.t
	export := t.Env + " " + shellcmd.PosixArg("SEAMLESS_CONFIG="+x.plan.OldConfig) + " " + shellcmd.PosixArg(x.h.exe) + " export -o -"
	return []string{
		t.AsRoot(t.Install, "-m", "0600", "/dev/null", pasBackupPath),
		export + " | " + t.AsRoot(t.Tee, pasBackupPath, ">/dev/null"),
		t.AsRoot(t.Tar, "-tzf", pasBackupPath, ">/dev/null"),
	}, nil
}

// pasAccountCommands make the daemon's account (unless it is there), the
// layout, and the pin. The bin directory is the account's, so the daemon
// updates itself, unless the plan keeps it root's.
func pasAccountCommands(x *pasRun) ([]string, error) {
	t, p, acct := x.t, x.plan, x.plan.Account
	var cmds []string
	if _, _, exists := x.h.world.lookupUser(acct); !exists {
		switch p.GOOS {
		case "darwin":
			if p.UID == 0 {
				return nil, fmt.Errorf("the account %s was there when the move was planned and is gone: make it again, or undo and plan afresh", acct)
			}
			id := strconv.Itoa(p.UID)
			cmds = append(cmds,
				t.AsRoot(t.Dscl, ".", "-create", "/Groups/"+acct, "PrimaryGroupID", id),
				t.AsRoot(t.Dscl, ".", "-create", "/Users/"+acct, "UniqueID", id),
				t.AsRoot(t.Dscl, ".", "-create", "/Users/"+acct, "PrimaryGroupID", id),
				t.AsRoot(t.Dscl, ".", "-create", "/Users/"+acct, "UserShell", "/usr/bin/false"),
				t.AsRoot(t.Dscl, ".", "-create", "/Users/"+acct, "NFSHomeDirectory", pasHome),
				t.AsRoot(t.Dscl, ".", "-create", "/Users/"+acct, "Password", "'*'"),
				t.AsRoot(t.Dscl, ".", "-create", "/Users/"+acct, "IsHidden", "1"))
		default:
			cmds = append(cmds, t.AsRoot(t.Useradd, "--system", "--user-group", "--shell", "/usr/sbin/nologin", "--home-dir", pasHome, acct))
		}
	}
	binOwner, binGroup := acct, acct
	if p.RootOwnedBinaries {
		binOwner, binGroup = "root", x.wheel()
	}
	cmds = append(cmds,
		t.AsRoot(t.Install, "-d", "-o", "root", "-g", x.wheel(), "-m", "0755", pasRoot, pasEtc),
		t.AsRoot(t.Install, "-d", "-o", "root", "-g", x.wheel(), "-m", "0700", pasStage),
		t.AsRoot(t.Install, "-d", "-o", acct, "-g", acct, "-m", "0700", pasHome),
		t.AsRoot(t.Install, "-d", "-o", acct, "-g", acct, "-m", "0755", pasRunDir),
		t.AsRoot(t.Install, "-d", "-o", binOwner, "-g", binGroup, "-m", "0755", pasBin))
	pin, pinned, err := x.h.world.pin()
	uid, _, exists := x.h.world.lookupUser(acct)
	if err != nil || !pinned || pin.Name != acct || !exists || int(pin.UID) != uid {
		cmds = append(cmds, rootstepPinCommands(t, acct)...)
	}
	return cmds, nil
}

// pasBinaryCommands put the binaries in place: each staged in root's own
// directory, checked there against the hash this run took of the verified
// release (or of the source build), on macOS against Apple's notarization
// and the release's Developer ID team as well, and only then made the
// daemon's (or root's) and renamed into place. seam goes first, seamlessd
// last (systemBinaries).
func pasBinaryCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	type bin struct{ source, sha string }
	bins := map[string]bin{}
	team := p.Team
	switch {
	case p.Source != "":
		for _, name := range systemBinaries {
			src := filepath.Join(p.Source, name)
			data, err := x.h.readFile(src)
			if err != nil {
				return nil, fmt.Errorf("read the source build's %s: %w", name, err)
			}
			sum := sha256.Sum256(data)
			bins[name] = bin{src, hex.EncodeToString(sum[:])}
		}
	case x.dryRun:
		for _, name := range systemBinaries {
			bins[name] = bin{"<this run's download>/" + name, "<sha256 of v" + p.Release + "'s " + name + ">"}
		}
		if team == "" {
			team = "<the team that signed v" + p.Release + ">"
		}
	default:
		v, _ := update.Parse(p.Release) // problem() checked it
		files, err := x.h.fetch(x.ctx, v)
		if err != nil {
			return nil, fmt.Errorf("download and check release v%s: %w", p.Release, err)
		}
		for _, name := range systemBinaries {
			data, ok := files[name]
			if !ok {
				return nil, fmt.Errorf("release v%s has no %s", p.Release, name)
			}
			src := filepath.Join(x.tmp, name)
			if err := os.WriteFile(src, data, 0o600); err != nil {
				return nil, err
			}
			sum := sha256.Sum256(data)
			bins[name] = bin{src, hex.EncodeToString(sum[:])}
		}
		if p.GOOS == "darwin" && team == "" {
			// This seamlessd names no team (a source build installing a
			// release): the release's Sigstore signature vouches for the
			// archive, so its binaries are held to the team that signed them.
			if team = x.h.team(x.ctx, bins["seamlessd"].source); !pasTeam.MatchString(team) {
				return nil, fmt.Errorf("release v%s's seamlessd names no Developer ID team", p.Release)
			}
		}
	}
	owner, group := p.Account, p.Account
	if p.RootOwnedBinaries {
		owner, group = "root", x.wheel()
	}
	var cmds []string
	for _, name := range systemBinaries {
		b := bins[name]
		staged := filepath.Join(pasStage, name)
		copyCmds := rootstepCopyCommands(t, rootstepCopy{source: b.source, sha256: b.sha, staged: staged,
			dest: filepath.Join(pasBin, name), owner: owner, group: group, mode: "0755"})
		if p.GOOS == "darwin" && p.Release != "" {
			// After the hash check (the third command), before anything makes
			// the copy anyone's but root's.
			req := `anchor apple generic and certificate leaf[subject.OU] = "` + team + `" and notarized`
			check := t.AsRoot(t.Codesign, "--verify", "--strict", "--check-notarization", shellcmd.PosixArg("-R="+req), staged)
			copyCmds = append(copyCmds[:3], append([]string{check}, copyCmds[3:]...)...)
		}
		cmds = append(cmds, copyCmds...)
	}
	return cmds, nil
}

// pasConfigCommands write the daemon's config, and with TLS on its pair,
// each a copy root checks against the hash of what this run prepared (the
// config) or read (the pair) before it goes into place.
func pasConfigCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	raw, err := x.h.readFile(p.OldConfig)
	if err != nil {
		return nil, fmt.Errorf("read your config %s: %w", p.OldConfig, err)
	}
	daemon, err := pasDaemonConfig(raw, p, x.h.now())
	if err != nil {
		return nil, err
	}
	src := "<this run's copy>/seamless.yaml"
	if !x.dryRun {
		src = filepath.Join(x.tmp, "seamless.yaml")
		if err := os.WriteFile(src, daemon, 0o600); err != nil {
			return nil, err
		}
		if _, err := config.LoadFrom(src); err != nil {
			return nil, fmt.Errorf("the daemon's config this run wrote does not load: %w", err)
		}
	}
	var cmds []string
	if p.TLSCert != "" {
		cmds = append(cmds, t.AsRoot(t.Install, "-d", "-o", "root", "-g", p.Account, "-m", "0750", pasTLSDir))
		for _, f := range []struct {
			src, group, mode string
		}{{p.TLSCert, x.wheel(), "0644"}, {p.TLSKey, p.Account, "0640"}} {
			data, err := x.h.readFile(f.src)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", f.src, err)
			}
			sum := sha256.Sum256(data)
			base := filepath.Base(f.src)
			cmds = append(cmds, rootstepCopyCommands(t, rootstepCopy{source: f.src, sha256: hex.EncodeToString(sum[:]),
				staged: filepath.Join(pasTLSDir, "."+base+".new"), dest: filepath.Join(pasTLSDir, base),
				owner: "root", group: f.group, mode: f.mode})...)
		}
	}
	sum := sha256.Sum256(daemon)
	cmds = append(cmds, rootstepCopyCommands(t, rootstepCopy{source: src, sha256: hex.EncodeToString(sum[:]),
		staged: filepath.Join(pasEtc, ".seamless.yaml.new"), dest: pasConfig, owner: "root", group: p.Account, mode: "0640"})...)
	return cmds, nil
}

// pasLaunchdWait is a loop that waits up to ten seconds until target is no
// longer loaded, then fails if it still is: bootout returns before the job
// has let go. It never exits the shell it runs in, pasted or not.
func pasLaunchdWait(t shellcmd.Tools, probe string) []string {
	return []string{
		"seamless_i=0",
		"while " + probe + " >/dev/null 2>&1; do [ \"$seamless_i\" -lt 100 ] || break; seamless_i=$((seamless_i + 1)); " + t.Sleep + " 0.1; done",
		"if " + probe + " >/dev/null 2>&1; then false; fi",
	}
}

// pasRetireCommands stop each per-user service and set its definition aside,
// so the next login does not start it again: a LaunchAgent's plist is kept
// beside itself (pasKeptSuffix, which launchd does not load); a systemd user
// unit is disabled.
func pasRetireCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	var cmds []string
	for _, svc := range p.Services {
		if p.GOOS == "darwin" {
			target := "gui/" + strconv.Itoa(p.OwnerUID) + "/" + svc
			if x.h.world.launchdLoaded(target) {
				cmds = append(cmds, t.Launchctl+" bootout "+target)
				cmds = append(cmds, pasLaunchdWait(t, t.Launchctl+" print "+target)...)
			}
			plist := launchdPlist(p.Home, svc)
			kept := plist + pasKeptSuffix
			_, perr := x.h.world.inspect(plist)
			_, kerr := x.h.world.inspect(kept)
			switch {
			case perr == nil && kerr == nil:
				return nil, fmt.Errorf("both %s and %s are there: remove the one you do not need", plist, kept)
			case perr == nil:
				cmds = append(cmds, t.Mv+" "+shellcmd.PosixArg(plist)+" "+shellcmd.PosixArg(kept))
			}
			continue
		}
		unit := filepath.Join(p.Home, ".config", "systemd", "user", svc)
		if _, err := x.h.world.inspect(unit); err == nil {
			cmds = append(cmds, t.Systemctl+" --user disable --now "+svc)
		}
	}
	return cmds, nil
}

// pasHostPort is the host:port of an http(s) address.
func pasHostPort(raw string) string {
	hp := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexByte(hp, '/'); i >= 0 {
		hp = hp[:i]
	}
	return hp
}

// pasPoll calls ok every h.wait until it reports true or tries run out.
func pasPoll(x *pasRun, tries int, ok func() bool) bool {
	for i := 0; i < tries; i++ {
		if ok() {
			return true
		}
		select {
		case <-x.ctx.Done():
			return false
		case <-time.After(x.h.wait):
		}
	}
	return ok()
}

// pasWaitOldDaemonGone waits until nothing answers at the old address: the
// data directory moves only once no daemon holds it.
func pasWaitOldDaemonGone(x *pasRun) error {
	hp := pasHostPort(x.plan.ServerURL)
	if pasPoll(x, 30, func() bool { return !x.h.reachable(hp) }) {
		return nil
	}
	return fmt.Errorf("a daemon still answers at %s: stop it (a daemon you run by hand, or a service this move does not know), then run setup again", hp)
}

// pasMoveDataCommands move the data directory into the layout and make it
// the daemon's account's alone. The move is a rename on one volume; the
// target must not be there, or mv would move the directory into it.
func pasMoveDataCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	_, oerr := x.h.world.inspect(p.OldDataDir)
	_, nerr := x.h.world.inspect(pasData)
	var cmds []string
	switch {
	case oerr == nil && nerr == nil:
		return nil, fmt.Errorf("both %s and %s are there: one of them has to go before the data can move", p.OldDataDir, pasData)
	case oerr == nil:
		cmds = append(cmds, "[ ! -e "+pasData+" ]", "[ ! -h "+pasData+" ]", t.AsRoot(t.Mv, shellcmd.PosixArg(p.OldDataDir), pasData))
	case nerr != nil:
		return nil, fmt.Errorf("neither %s nor %s is there", p.OldDataDir, pasData)
	}
	return append(cmds,
		"[ -d "+pasData+" ]", "[ ! -h "+pasData+" ]",
		t.AsRoot(t.Chown, "-R", shellcmd.PosixArg(p.Account+":"+p.Account), pasData),
		t.AsRoot(t.Chmod, "-R", "go-rwx", pasData)), nil
}

// pasServiceCommands install and start the system service.
func pasServiceCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	if p.GOOS == "darwin" {
		plist := pasDaemonPlistPath()
		return []string{
			rootstepHeredoc(t.AsRoot(t.Tee, plist, ">/dev/null"), pasDaemonPlist(p)),
			t.AsRoot(t.Chown, "root:wheel", plist),
			t.AsRoot(t.Chmod, "0644", plist),
			t.Plutil + " -lint " + plist + " >/dev/null",
			"{ " + t.AsRoot(t.Launchctl, "print", "system/"+systemDaemonLabel) + " >/dev/null 2>&1 || " +
				t.AsRoot(t.Launchctl, "bootstrap", "system", plist) + "; }",
		}, nil
	}
	unit := pasDaemonUnitPath()
	cmds := []string{
		rootstepHeredoc(t.AsRoot(t.Tee, unit, ">/dev/null"), pasDaemonUnit(p)),
		t.AsRoot(t.Chmod, "0644", unit),
	}
	if pasSelfUpdating(p) {
		// The updater runs as a unit of the account's own user manager, which
		// runs with no one logged in only while the account lingers.
		cmds = append(cmds, t.AsRoot(t.Loginctl, "enable-linger", p.Account))
	}
	return append(cmds,
		t.AsRoot(t.Systemctl, "daemon-reload"),
		t.AsRoot(t.Systemctl, "enable", "--now", strings.TrimSuffix(systemDaemonUnit, ".service"))), nil
}

// pasSelfUpdating reports whether the daemon the plan installs updates
// itself: a release's binaries, held by its account.
func pasSelfUpdating(p pasPlan) bool { return p.Release != "" && !p.RootOwnedBinaries }

func pasDaemonPlistPath() string { return systemDaemonPlistDir + "/" + systemDaemonLabel + ".plist" }
func pasDaemonUnitPath() string  { return "/etc/systemd/system/" + systemDaemonUnit }

// pasDaemonPlist is the LaunchDaemon (docs-src/reference/service.md, "A
// system service for peer_auth"): the account, umask 077, kept alive, so it
// starts again after the clean exit an update ends it with.
func pasDaemonPlist(p pasPlan) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>` + systemDaemonLabel + `</string>
  <key>UserName</key>          <string>` + p.Account + `</string>
  <key>GroupName</key>         <string>` + p.Account + `</string>
  <key>InitGroups</key>        <true/>
  <key>ProgramArguments</key>
  <array>
    <string>` + pasBin + `/seamlessd</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>SEAMLESS_CONFIG</key> <string>` + pasConfig + `</string>
    <key>HOME</key>            <string>` + pasHome + `</string>
  </dict>
  <key>Umask</key>             <integer>63</integer>
  <key>RunAtLoad</key>         <true/>
  <key>KeepAlive</key>         <true/>
  <key>ThrottleInterval</key>  <integer>10</integer>
  <key>StandardOutPath</key>   <string>` + pasData + `/seamlessd.log</string>
  <key>StandardErrorPath</key> <string>` + pasData + `/seamlessd.log</string>
</dict>
</plist>
`
}

// pasDaemonUnit is the system unit (the guide's): Restart=always, so it
// starts again after an update's clean exit, and no SEAMLESS_* but the
// config's path, which the updater would not see.
func pasDaemonUnit(p pasPlan) string {
	return `[Unit]
Description=Seamless daemon (peer_auth)
After=network.target

[Service]
User=` + p.Account + `
Group=` + p.Account + `
UMask=0077
Environment=SEAMLESS_CONFIG=` + pasConfig + `
Environment=HOME=` + pasHome + `
ExecStart=` + pasBin + `/seamlessd serve
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`
}

// pasWaitHealthy waits until the daemon answers on the socket: launchd and
// systemd retry a refused start every ten seconds, and a first start may
// migrate the database.
func pasWaitHealthy(x *pasRun) error {
	var last error
	if pasPoll(x, 90, func() bool { last = x.h.health(x.ctx); return last == nil }) {
		return nil
	}
	where := "sudo /bin/launchctl print system/" + systemDaemonLabel + " and " + pasData + "/seamlessd.log"
	if x.plan.GOOS == "linux" {
		where = "sudo journalctl -u " + systemDaemonUnit
	}
	return fmt.Errorf("the daemon does not answer on %s (%w): see %s", pasSocket, last, where)
}

// pasWaitDaemonGone waits until the daemon no longer answers on the socket.
func pasWaitDaemonGone(x *pasRun) error {
	if pasPoll(x, 30, func() bool { return x.h.health(x.ctx) != nil }) {
		return nil
	}
	return fmt.Errorf("the daemon still answers on %s", pasSocket)
}

// pasDoctorCommands run doctor as the daemon's account against its config:
// the file and account checks describe whoever runs them. A failing check
// stops the step; the owner's agents still reach the daemon over TCP with
// the key until the next steps switch them to the socket.
func pasDoctorCommands(x *pasRun) ([]string, error) {
	t := x.t
	return []string{
		"cd /",
		strings.Join([]string{t.Sudo, "-u", x.plan.Account, t.Env, "SEAMLESS_CONFIG=" + pasConfig, "HOME=" + pasHome,
			pasBin + "/seamlessd", "doctor"}, " "),
	}, nil
}

// pasSwitchClientConfig sets the owner's server config aside and writes the
// client config in its place, and the console's own config beside it. The
// server config is never edited: it is kept whole, and undo puts it back.
func pasSwitchClientConfig(x *pasRun) error {
	p := x.plan
	kept := p.OldConfig + pasKeptSuffix
	if _, err := os.Lstat(kept); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(p.OldConfig, kept); err != nil {
			return fmt.Errorf("set your config aside: %w", err)
		}
	} else if err != nil {
		return err
	}
	raw, err := x.h.readFile(kept)
	if err != nil {
		return err
	}
	key, caFile, err := pasFileKey(raw)
	if err != nil {
		return err
	}
	if err := pasWriteOwnerConfig(p.OldConfig, pasClientConfig(), func(c config.Config) bool {
		return c.IsClient() && c.SocketPath() == pasSocket
	}); err != nil {
		return err
	}
	return pasWriteOwnerConfig(pasConsoleConfig(p), pasConsoleConfigContent(p.ServerURL, key, caFile), func(c config.Config) bool {
		return c.IsClient() && strings.TrimSuffix(c.AdvertisedURL, "/") == p.ServerURL && c.MCP.APIKey == key
	})
}

// pasWriteOwnerConfig creates path (0600, O_EXCL) with content, or adopts the
// file there when same says it is this one; any other is refused, never
// overwritten.
func pasWriteOwnerConfig(path, content string, same func(config.Config) bool) error {
	if _, err := os.Lstat(path); err == nil {
		if c, lerr := config.LoadFrom(path); lerr == nil && same(c) {
			return nil
		}
		return fmt.Errorf("%s is there and is not the config this step writes: move it aside, then run setup again", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := config.LoadFrom(path); err != nil {
		return fmt.Errorf("the config just written to %s does not load: %w", path, err)
	}
	return nil
}

// pasReinstallHooks points the hooks and the MCP bridge at the daemon's own
// seam, which no process of the owner's can replace and which moves with
// the daemon when it updates itself.
func pasReinstallHooks(x *pasRun) error {
	return x.h.runOwner(x.ctx, []string{x.h.exe, "install-hooks", "--config", x.plan.OldConfig, "--seam", pasBin + "/seam"}, nil)
}

// pasUndoServiceCommands stop the system service and remove it.
func pasUndoServiceCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	if p.GOOS == "darwin" {
		probe := t.AsRoot(t.Launchctl, "print", "system/"+systemDaemonLabel)
		cmds := []string{"if " + probe + " >/dev/null 2>&1; then " + t.AsRoot(t.Launchctl, "bootout", "system/"+systemDaemonLabel) + "; fi"}
		cmds = append(cmds, pasLaunchdWait(t, probe)...)
		return append(cmds, t.AsRoot(t.Rm, "-f", pasDaemonPlistPath())), nil
	}
	var cmds []string
	if _, err := x.h.world.inspect(pasDaemonUnitPath()); err == nil {
		cmds = append(cmds, t.AsRoot(t.Systemctl, "disable", "--now", strings.TrimSuffix(systemDaemonUnit, ".service")),
			t.AsRoot(t.Rm, "-f", pasDaemonUnitPath()))
	}
	return append(cmds, t.AsRoot(t.Systemctl, "daemon-reload"), t.AsRoot(t.Loginctl, "disable-linger", p.Account)), nil
}

// pasUndoDataCommands move the data back and make it the owner's again. The
// old path must be free, and after the move it must hold the database
// itself: a directory made there meanwhile would have taken the data inside.
func pasUndoDataCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	_, oerr := x.h.world.inspect(p.OldDataDir)
	_, nerr := x.h.world.inspect(pasData)
	switch {
	case oerr == nil && nerr == nil:
		return nil, fmt.Errorf("both %s and %s are there: one of them has to go before the data can move back", p.OldDataDir, pasData)
	case nerr != nil:
		return nil, nil // back already
	}
	old := shellcmd.PosixArg(p.OldDataDir)
	return []string{
		"[ ! -e " + old + " ]", "[ ! -h " + old + " ]",
		t.AsRoot(t.Mv, pasData, old),
		"[ -e " + shellcmd.PosixArg(filepath.Join(p.OldDataDir, "seam.db")) + " ]",
		t.AsRoot(t.Chown, "-R", shellcmd.PosixArg(p.Owner+":"+p.OwnerGroup), old),
	}, nil
}

// pasUndoRetireCommands put each per-user service back and start it.
func pasUndoRetireCommands(x *pasRun) ([]string, error) {
	p, t := x.plan, x.t
	var cmds []string
	for _, svc := range p.Services {
		if p.GOOS == "darwin" {
			plist := launchdPlist(p.Home, svc)
			kept := plist + pasKeptSuffix
			_, perr := x.h.world.inspect(plist)
			if _, kerr := x.h.world.inspect(kept); kerr == nil && perr != nil {
				cmds = append(cmds, t.Mv+" "+shellcmd.PosixArg(kept)+" "+shellcmd.PosixArg(plist))
			} else if perr != nil {
				continue
			}
			target := "gui/" + strconv.Itoa(p.OwnerUID) + "/" + svc
			if !x.h.world.launchdLoaded(target) {
				cmds = append(cmds, t.Launchctl+" bootstrap gui/"+strconv.Itoa(p.OwnerUID)+" "+shellcmd.PosixArg(plist))
			}
			continue
		}
		cmds = append(cmds, t.Systemctl+" --user enable --now "+svc)
	}
	return cmds, nil
}

// pasUndoClientConfig puts the server config back where it was; the client
// config the move wrote is kept beside it, and so is the console's.
func pasUndoClientConfig(x *pasRun) error {
	p := x.plan
	kept := p.OldConfig + pasKeptSuffix
	if _, err := os.Lstat(kept); errors.Is(err, fs.ErrNotExist) {
		return nil // back already
	}
	if _, err := os.Lstat(p.OldConfig); err == nil {
		if err := os.Rename(p.OldConfig, p.OldConfig+".peer-auth-client"); err != nil {
			return err
		}
	}
	return os.Rename(kept, p.OldConfig)
}

// pasUndoHooks reinstall the hooks against the restored server config, with
// the seam beside this seamlessd.
func pasUndoHooks(x *pasRun) error {
	return x.h.runOwner(x.ctx, []string{x.h.exe, "install-hooks", "--config", x.plan.OldConfig}, nil)
}
