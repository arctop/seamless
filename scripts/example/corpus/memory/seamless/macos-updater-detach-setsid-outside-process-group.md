---
id: 01M4HABDMKEJ76F519BR35R4J5
kind: decision
name: macos-updater-detach-setsid-outside-process-group
description: "2.01 spike (macOS 27): launchd only signals the job's process group, so a Setsid child updater survives bootout/kickstart -k/kill -9; self-check…"
project: seamless
created: 2026-10-09T21:50:47Z
invalid_at: null
superseded_by: null
source_session: 01M4H36388N77HSP2V1CHJ4GT3
tags: [macos, launchd, auto-update, plan:plan-background-update-check-automatic-updates-claude-code-style, lab:detached-updater-os-spikes]
---
Settled 2026-10-09 by lab `detached-updater-os-spikes` (task 2.01, macOS 27.0.1 / Darwin 27 on the owner's Mac; macOS 15 and 26 not yet run). Feeds task 2.09's darwin spawner and the darwin branch of `outsideServiceCheck` (cmd/seamlessd/update_engine.go).

## What launchd actually does to a LaunchAgent's children
- The job's main process is a process-group leader (pgid = pid) in launchd's session (**sid = 1**, not its own).
- On bootout, `kickstart -k` (which sends SIGTERM, not SIGKILL), `kill -9` of the main process, and the 5s exit-timeout SIGKILL alike, launchd sends **SIGTERM to the job's process group** when the main process goes. It never escalates to SIGKILL for leftover group members, and it does **not** kill by coalition or session, although each job has its own resource/jetsam coalition.
- A plain child (`exec.Command` with no SysProcAttr) is in that group, so it gets the SIGTERM. Go's default handling would end the updater mid-install.
- A child started with `Setpgid` or `Setsid` gets **no signal** and is reparented to pid 1.
- The orphan can run the installer's `launchctl bootout` + `bootstrap` on its own service. launchd logs "bootout initiated by: launchctl<-sh<-updater<-seamlessd(service)" and accepts it.

## Decision for 2.09 (darwin)
- Spawn the updater with `SysProcAttr{Setsid: true}` (the plan's choice; Setpgid alone would also survive) and reap it with `go cmd.Wait()`. The installer child keeps `installerProcAttr`'s Setpgid.
- `outsideServiceCheck` on darwin: **`unix.Getpgid(0) == os.Getpid()`**. Being a process-group leader means not being in the service's group, and that group is the only set launchd signals.
  - True for the Setsid child and for a transient job's main process; false for a plain child.
  - Do NOT use `XPC_SERVICE_NAME`: children inherit the service's label, and an orphan also has ppid 1, so both of Detect's gate-9 signals match an orphaned updater. `updaterProbe` already forces Supervised for that reason.
- Fallback (works, not needed on 27): a transient job whose plist lives in `<data_dir>/update/` (0600 in 0700), bootstrapped into `gui/<uid>`.
  - It runs as its own job: own XPC_SERVICE_NAME, ppid 1, pgid = pid. BTM never touches it.
  - It stays loaded as "not running" after it exits, so boot out any leftover label before the next bootstrap.

## Supporting trials
- macOS 27: external launchctl bootout -- children with no detach / Setpgid / Setsid
- macOS 27: launchctl kickstart -k -- children with no detach / Setpgid / Setsid
- macOS 27: kill -9 of the service's main process -- children with no detach / Setpgid / Setsid
- macOS 27: service hangs past the 5s exit timeout during the orphan's reload -- launchd SIGKILLs it, Setsid updater survives, bootstrap succeeds at try 5
- macOS 27: orphaned Setsid updater runs the installer's bootout+bootstrap on its own service (same binary)
- macOS 27: orphaned Setsid updater swaps the binary (rename) then bootout+bootstrap -- no AMFI kill
- macOS 27: transient launchd job from a plist outside ~/Library/LaunchAgents runs as its own job with zero BTM activity

## Rejected
- No detach: it receives the group SIGTERM.
- Relying on XPC_SERVICE_NAME or ppid for the self-check: both are inherited or shared by the orphan.
- `kickstart -k` after a swap: it was not killed on 27 (see memory [[macos-26-amfi-kills-first-launchd-spawn-after-binary-swap]]), but it stays wrong for 26.x.

Related: [[macos-btm-notice-per-adhoc-binary-update]], [[auto-update-architecture-and-policy]].

**Built 2026-10-09 in 172ce57** (task 2.09; update_spawn_darwin.go, update_spawn_unix.go, update_selfcheck_unix.go):
- Setsid child + `go cmd.Wait()`.
- stdin/stdout are /dev/null. stderr is the daemon's os.Stderr only when it is a regular file (launchd's ~/.seamless/seamlessd.log), so an updater that refuses before its own log opens still says why there; never a pipe.
- Self-check: `unix.Getpgid(0) == os.Getpid()`.
- The transient launchd-job fallback was deliberately NOT built: no failure class was found that a launchd job running the same exe would survive, and a launchd-spawned job is the one exposed to macOS 26's cached code-signing kill. darwin uninstall therefore has nothing new to remove.
