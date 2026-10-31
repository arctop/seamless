---
title: The service & where things live
description: Everything OS-specific in one place - the per-user service (launchd, systemd, or a Scheduled Task), its native controls and logs, the detached updater behind automatic updates, and every path Seamless touches.
---

The installer registers `seamlessd` as a per-user service, so the daemon
survives reboots without you supervising it. It runs as **your** user, not
root: it reads your config, writes your files, and should die with your login
session, not the machine.

By default there is **one instance per machine**: port `8081`, data dir
`~/.seamless` - one daemon, one database, one set of files. Both are config
keys, not fixed facts: set `addr:` and `data_dir:` in
`~/.config/seamless/seamless.yaml` (or the `SEAMLESS_ADDR` /
`SEAMLESS_DATA_DIR` env overrides) and restart the service. The config is the
single source of truth for the bind address - the installer and the Makefile
both read the port back out of it, so their health checks follow your change
rather than assuming `8081`, and nothing bakes the address into the service
itself.

One instance per machine is the default, not a law. Several devices can share
**one** daemon: the server keeps the service, the database and the corpus, and
every other machine installs with `role: client` plus `server_url`, which runs
no daemon and registers no service at all (`seamlessd serve` refuses on a
client outright). Everything below describes the server's install;
[Share one daemon across a LAN](/guides/network-install/) is the setup, and
the tradeoffs.

## Control it from anywhere

Whatever the platform, one set of verbs controls the service - they resolve
your OS's service manager for you:

```bash
seamlessd start       # start | stop | restart | status
make start            # the same, from a clone (start | stop | restart | status)
```

These act on the already-installed service and print a hint if it was never
installed. The platform-native commands below are what they wrap - you need
them only when you want to talk to the service manager directly.

## The service on your OS

::: when os=macos

A user LaunchAgent labelled `org.thereisnospoon.seamless` in
`~/Library/LaunchAgents/`, logging to `~/.seamless/seamlessd.log` (`make logs`
follows it from a clone). The universal verbs wrap:

```bash
launchctl print gui/$(id -u)/org.thereisnospoon.seamless        # state
launchctl kickstart -k gui/$(id -u)/org.thereisnospoon.seamless # restart
launchctl bootout gui/$(id -u)/org.thereisnospoon.seamless      # stop
```

:::

::: when os=linux

The installer writes a systemd user unit to
`~/.config/systemd/user/seamless.service` and enables lingering, so the daemon
starts at boot rather than at your next login:

```bash
systemctl --user status seamless      # state, pid, last exit
journalctl --user -u seamless -f      # follow the log
systemctl --user restart seamless
systemctl --user stop seamless
```

No systemd user session (some containers, WSL1)? The installer says so and
skips the step; run `seamlessd serve` under whatever supervises processes
there.

:::

::: when os=windows

An at-logon Scheduled Task named `Seamless`, running as you (`LogonType
Interactive`, no admin), logging to `~/.seamless/seamlessd.log`. The task
action is a bare exec - `seamlessd.exe serve --config <path> --log-file
<path>` - because a task cannot carry the `SEAMLESS_CONFIG` env prefix a plist
or systemd unit does; the two flags pass exactly what that prefix would have:

```powershell
Get-ScheduledTask Seamless | Get-ScheduledTaskInfo   # state, last run, last result
Get-Content ~/.seamless/seamlessd.log -Wait          # follow the log
Restart-ScheduledTask Seamless                        # stop + start
Stop-ScheduledTask Seamless                           # stop (it restarts at next logon)
```

It restarts on failure and never hits the default execution time limit, so it
behaves like launchd's `KeepAlive`. Because it triggers at logon, it runs while
you are signed in and stops when you sign out - a single-user desktop, which is
the shape Seamless is built for.

Windows wiring ships in every release and its hook command forms are
unit-tested, but it has fewer live-verified runs than macOS and Linux; the
[Codex compatibility matrix](/reference/codex-compatibility/) records exactly
which combinations have been observed working.

:::

## The updater on your OS {#the-updater}

On an install the installer made, the daemon installs new releases by itself
([Automatic updates](/updating/#automatic-updates) covers which installs, and
when). It never installs anything in-process. It starts the updater,
`seamlessd update --auto`, which verifies the new release and runs that
release's own installer, and the installer restarts the service. So the daemon
starts the updater where stopping the service cannot reach it, which is a
different place on each OS.

The updater runs the `seamlessd` on disk and refuses unless that binary is the
release the daemon is updating from. It also refuses to run inside the
service's own process tree. It holds `~/.seamless/update/update.lock` for its
whole run, so only one update runs at a time, and records itself in
`~/.seamless/update/attempt.json`. Every step, and the installer's output,
goes to `~/.seamless/update/logs/<attempt>.log`.

::: when os=macos

A child process in a session of its own (`setsid`). launchd stops a
LaunchAgent by signalling its process group (on `bootout`, on `kickstart -k`,
and when the daemon is killed), and the updater has left that group, so the
installer's reload of the service does not reach it. The installer reloads
with `launchctl bootout` and `bootstrap`, never by re-executing the loaded job
in place: macOS 26 kills a job re-executed in place after its binary was
replaced. Nothing is registered with launchd for the updater, so there is
nothing to remove. Its standard error goes to `~/.seamless/seamlessd.log`. An
updater that refuses before its own log opens says why there
(`command failed cmd=update`).

```bash
pgrep -fl 'seamlessd update --auto'   # an update in progress
```

From v0.7.0 on, the macOS release binaries are signed with the project's
Developer ID Application certificate and notarized by Apple. macOS Background
Items should therefore treat an updated `seamlessd` as the same background
item. The first update away from an ad-hoc-signed binary (v0.6.0 or older, or
a build from source) may still show one "seamlessd can run in the background"
notice, possibly hours after the update.

:::

::: when os=linux

A transient user unit of its own, `seamless-update-<attempt>.service`, started
with `systemd-run --user`. Stopping or restarting `seamless.service` kills
every process in that unit's cgroup, a `setsid` child included, so the updater
needs a unit of its own. systemd drops the unit when the updater exits
(`--collect`) and stops it after 30 minutes at most (`RuntimeMaxSec`), along
with anything the installer left running. The unit starts from your user
manager's environment, not the daemon's. The daemon passes along only its
proxy and CA settings: `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` and `ALL_PROXY`
in either case, `SSL_CERT_FILE` and `SSL_CERT_DIR`. No user D-Bus session is
needed.

```bash
systemctl --user list-units 'seamless-update-*'   # an update in progress
journalctl --user -u 'seamless-update-*'          # what it printed, if it refused before its log opened
```

Two things about the service itself matter here:

- **Lingering.** Without it, your user manager stops when your last session
  ends, and it takes the daemon and any updater with it, possibly mid-install.
  The installer runs `loginctl enable-linger`. Where it cannot (a standard user
  on a system without polkit), it warns, and
  `sudo loginctl enable-linger $(id -un)` is the one-time fix.
  `loginctl show-user $(id -un) -p Linger` shows the setting.
- **No start limit.** The unit the installer writes sets
  `StartLimitIntervalSec=0`, and the installer runs
  `systemctl --user reset-failed seamless.service` before it restarts the
  service. A burst of restarts (a crash loop, the installer's own restart, a
  rollback) therefore never leaves the service down for good. A unit written by
  the v0.6.0 installer or an older one keeps systemd's default limit until the
  next install or update rewrites it. Until then,
  `systemctl --user reset-failed seamless.service && systemctl --user restart seamless.service`
  brings back a service that `systemctl --user status seamless` reports as
  `start-limit-hit`.

:::

::: when os=windows

A detached process with no console window and a process group of its own,
started outside the `Seamless` task's job where Windows allows it
(`CREATE_NO_WINDOW`, `CREATE_NEW_PROCESS_GROUP`, `CREATE_BREAKAWAY_FROM_JOB`).
When the job refuses the breakaway, the daemon starts it again without that
flag. Task Scheduler's stop (`Stop-ScheduledTask`, `schtasks /End`) ends only
the task's own process, the daemon, so the updater keeps running.

The daemon falls back only when that process cannot be started at all. The
fallback is a hidden, on-demand Scheduled Task named `SeamlessUpdate-<your SID>`:
it has no trigger, runs as you and only while you are signed in
(`InteractiveToken`, least privilege, no admin), and has a one-hour limit. The
daemon registers it again for each update with `schtasks /Create /XML` and
starts it with `schtasks /Run`. The task then stays registered, hidden, until
`seamlessd uninstall` removes it.

```powershell
Get-CimInstance Win32_Process -Filter "Name = 'seamlessd.exe'" | Where-Object CommandLine -like '*update --auto*'   # an update in progress
Get-ScheduledTask -TaskName 'SeamlessUpdate-*'                                                                   # only once the fallback has run
```

Only the daemon that the `Seamless` task started updates itself. On Windows,
the last of the checks that decide whether this install updates itself asks
whether this process is the service's own instance. It needs two things: the
daemon's parent process is the Task Scheduler (`svchost.exe`), and the
`Seamless` task runs as you. A `seamlessd serve` you started by hand only
notifies, and its reason reads `not the service's own process: the parent
process is ..., not the Task Scheduler (svchost.exe)`. Stop it and run
`Start-ScheduledTask Seamless`. An install that fell back to a Startup-folder
shortcut, because the installer could not register the task, has no task at
all, so it only notifies too.

:::

### When an update goes wrong {#updater-recovery}

Every attempt leaves a record. `seamlessd doctor` (its `updates` row) and
`seamlessd update --check` print the last attempt with the updater's own error,
its log and the backup it took. [An automatic update failed](/guides/troubleshooting/#an-automatic-update-failed)
walks through the outcomes. What the service manager can tell you:

- **An update that never started** is recorded at the `spawn` stage, and the
  daemon tries again after a backoff. When the daemon could not start the
  updater, its log says why (`could not start the updater`). On Linux the
  reason is one of `systemd-run is not installed`, `the systemd user manager is
  unreachable`, `this attempt's unit already exists`, `the seamlessd binary is
  missing or not executable` and `the updater could not be executed`. On
  Windows the error names why the detached process failed and why the update
  task failed. When the updater started but refused before it recorded
  anything, it said why on its standard error: in `~/.seamless/seamlessd.log` on
  macOS, and in `journalctl --user -u 'seamless-update-*'` on Linux. On Windows
  that output goes nowhere.
- **An update the updater's gates refused** names the owner's action in
  doctor's `updates` row, and every retry is refused the same way until you
  act. A release installed without a restart (the seamlessd on disk is not
  the one the daemon runs), or an install that no longer looks installer-made,
  needs `seamlessd restart`. An updater that could not make sure it runs
  outside the service needs `seamlessd update`, run by hand from a terminal.
- **An updater that hangs** is stopped by a limit: the installer's own
  15-minute timeout on every OS, the unit's 30 minutes on Linux, and the
  update task's hour on Windows. You can also stop it by hand: on Linux with
  `systemctl --user stop 'seamless-update-*'`, and on macOS and Windows by
  ending the `seamlessd update --auto` process. The OS releases the update lock
  however the updater dies, and the daemon settles the attempt from the release
  that is running. An attempt stopped while the old release still runs counts
  as interrupted and is tried again after a backoff. That run reinstalls the
  new release and confirms it, or rolls back.
- **A service left down** by an update that could not be rolled back cleanly:
  the attempt's error carries the updater's recovery steps. Reinstalling the
  release you had is the installer pinned to it:
  `curl -fsSL https://thereisnospoon.org/install | SEAMLESS_VERSION=<version> sh`,
  or `$env:SEAMLESS_VERSION='<version>'; irm https://thereisnospoon.org/install.ps1 | iex`
  on Windows. Then run `seamlessd doctor`.
- **Putting the whole instance back** from the pre-update backup is a restore,
  and `seamlessd import` restores only into an empty data directory (into a
  populated one it merges). Run `seamlessd stop`, then move `~/.seamless` aside
  (to `~/.seamless.broken`, say; the backup moves with it), then run
  `seamlessd import --from ~/.seamless.broken/backups/pre-update-v<old>-<time>.tar.gz`,
  then `seamlessd start`.

## A system service for peer_auth {#peer-auth-daemon}

[Sealing projects to OS accounts](/guides/peer-auth/) needs the daemon to run as
an account of its own, so the per-user service above cannot host it. On macOS it
is a LaunchDaemon that names the account; it runs whether or not anyone is
logged in. Install it as `/Library/LaunchDaemons/org.thereisnospoon.seamless.daemon.plist`
(`root:wheel`, `0644`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>             <string>org.thereisnospoon.seamless.daemon</string>
  <key>UserName</key>          <string>_seamless</string>
  <key>GroupName</key>         <string>_seamless</string>
  <key>InitGroups</key>        <true/>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/seamless/bin/seamlessd</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>SEAMLESS_CONFIG</key> <string>/opt/seamless/etc/seamless.yaml</string>
    <key>HOME</key>            <string>/opt/seamless/home</string>
  </dict>
  <key>Umask</key>             <integer>63</integer>
  <key>RunAtLoad</key>         <true/>
  <key>KeepAlive</key>         <true/>
  <key>ThrottleInterval</key>  <integer>10</integer>
  <key>StandardOutPath</key>   <string>/opt/seamless/data/seamlessd.log</string>
  <key>StandardErrorPath</key> <string>/opt/seamless/data/seamlessd.log</string>
</dict>
</plist>
```

`Umask` 63 is `077`. The controls are the system domain's, all with `sudo`:

```bash
sudo plutil -lint /Library/LaunchDaemons/org.thereisnospoon.seamless.daemon.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/org.thereisnospoon.seamless.daemon.plist
sudo launchctl kickstart -k system/org.thereisnospoon.seamless.daemon   # restart
sudo launchctl print system/org.thereisnospoon.seamless.daemon          # state, pid, last exit
sudo launchctl bootout system/org.thereisnospoon.seamless.daemon        # stop and unload
```

`seamlessd start|stop|restart|status` manage the per-user service, not this one.
Before loading it, stop and remove the per-user service, or it starts again at
your next login: `seamlessd stop`, then
`launchctl bootout gui/$(id -u)/org.thereisnospoon.seamless` and delete
`~/Library/LaunchAgents/org.thereisnospoon.seamless.plist`.

On Linux the same daemon is a system unit with `User=` and `Group=` set to the
account, `UMask=0077`, `Environment=SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml`
and `ExecStart=/opt/seamless/bin/seamlessd serve`, enabled with
`sudo systemctl enable --now`.

Automatic updates do not manage this install (the daemon only notifies): replace
the binaries in `/opt/seamless/bin` with `sudo` and restart it.

## Where things live {#where-things-live}

Every `~` path resolves under `%USERPROFILE%` on Windows - the daemon searches
the same relative locations on every OS.

| What | Where | Notes |
|---|---|---|
| Binaries | `~/.local/bin/seamlessd`, `~/.local/bin/seam` | `SEAMLESS_INSTALL_DIR` retargets them - see [Install & deploy](/install/) |
| Config + bearer key | `~/.config/seamless/seamless.yaml` | mode `0600`; every key in [Configuration](/reference/configuration/) |
| Knowledge + database | `~/.seamless/` | markdown memories and notes, plus `seam.db` - see [Storage](/reference/storage/) |
| Update records + backups | `~/.seamless/update/`, `~/.seamless/backups/` | the update check's state, each update attempt and its log, the pre-update archives - see [Storage](/reference/storage/#the-update-records) |
| Daemon log | `~/.seamless/seamlessd.log` (macOS, Windows); journald (Linux) | `make logs` from a clone, `journalctl --user -u seamless -f` on Linux |
| Claude Code hooks | `~/.claude/settings.json` | exactly what is written: [Hooks](/reference/hooks/) |
| Codex hooks | `${CODEX_HOME:-~/.codex}/hooks.json` | same reference, [shell-string forms](/reference/hooks/#the-codex-profile-is-shell-strings) |
| Skills | `~/.claude/skills/`, `${CODEX_HOME:-~/.codex}/skills/` | `seam-onboard` always; `seam-research` while its [optional feature](/reference/console/#optional-features) is on. Delivered per client |
| Claude app chat MCP | `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS); `%APPDATA%\Claude\claude_desktop_config.json` (Windows) | the chat surface's only artifact - [Claude app chat setup](/claude-app/) |

## Removing the service by hand {#removing-the-service-by-hand}

[Update & uninstall](/updating/) covers `seamlessd uninstall`, which removes
the service along with everything else, including what the updater leaves
behind. On Linux it first stops any update unit still loaded, so an updater
caught mid-update cannot restart the service it is removing. On Windows it
deletes your `SeamlessUpdate-<SID>` task. On macOS the updater leaves nothing
to remove. For a bare binary you never registered a service for, or a machine
you are cleaning by hand, the native teardown is:

```bash
launchctl bootout gui/$(id -u)/org.thereisnospoon.seamless   # macOS
systemctl --user stop 'seamless-update-*'                    # Linux: an update still running
systemctl --user disable --now seamless                      # Linux
```

```powershell
Unregister-ScheduledTask -TaskName Seamless                  # Windows
Unregister-ScheduledTask -TaskName "SeamlessUpdate-$([Security.Principal.WindowsIdentity]::GetCurrent().User.Value)"   # Windows: the update task, if it exists
```
