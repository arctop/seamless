---
id: 01M4GMD7CW99V8Y60JMPJN2X2A
title: 2.01 OS spikes for the detached updater (macOS, Linux, Windows) as a research lab
status: done
plan: plan-background-update-check-automatic-updates-claude-code-style
project: seamless
created_by: cc/0dc79b88-a3fdc128d2c2757f
---
Human + devices. Spike checklist from section 2.3 of note cc-plan-temporal-yawning-cupcake: macOS 15 and 26 (Setsid child survives bootout, kickstart -k and kill -9; orphan can bootstrap; no AMFI OS_REASON_CODESIGNING; BTM notices; restart-to-healthy time); Linux (systemd-run --user from inside the unit on Ubuntu 22/24, Debian 12, Fedora, Arch, WSL2; survives restart; logout without linger; cgroup v1); Windows (is the daemon in a job and with which flags; does stopping the task kill children; can a standard user register the task; no window flashes). Record each result as a trial in a seam-research lab.

DONE 2026-10-09. Every half's trials are in lab `detached-updater-os-spikes`. Leftover coverage (macOS 15/26, WSL2, x86_64 Linux, systemd < 249) was split into task 01M4HJFFFPQ04JGHCW3R1KRXYK; it does not gate the plan. The interactive Windows desktop is task 01M4HCZR2W47BZAA5FRNRS1HT5 (post-release).

macOS (cc/ae17d44d, macOS 27.0.1, 15 trials):
- launchd only SIGTERMs the job's process group. A Setsid child survives bootout, kickstart -k, kill -9 and the 5s exit-timeout SIGKILL, and the orphan can bootout+bootstrap its own service.
- No AMFI kill on 27.
- BTM posts one Background Items notice per adhoc binary update; only Team-ID signing avoids it (owner decision task 01M4HAFE73Y4V844VKGEY7GMVX).
- Restart-to-healthy 0.84s with a 0.1s bootstrap loop (that loop landed in 416a090).
- Memories: decision macos-updater-detach-setsid-outside-process-group; gotcha macos-btm-notice-per-adhoc-binary-update.

Windows (cc/ae17d44d, Windows 11 26200, S4U stand-in, 7 trials):
- Task actions run in one shared job with only SILENT_BREAKAWAY_OK, which Task Scheduler toggles.
- Stopping the task ends only the action process; children of every creation-flag kind survive.
- A standard user can register its own InteractiveToken on-demand task via schtasks /Create /XML.
- schtasks /End returns ~1.1s before the process dies. Real seamlessd rides that out (it waits on seamlessd.lock before binding, since aba6c14). `seamlessd stop/restart` now wait on that lock (a457dcc).
- Memories: decision windows-updater-detach-under-task-scheduler; gotcha windows-schtasks-end-lag-and-the-serve-lock (supersedes windows-task-stop-returns-before-process-exits).

Linux (cc/90aaef39 fanout wave, Lima VMs on the owner's Mac, arm64, 24 "Linux ..." trials):
- Coverage: Ubuntu 22.04/24.04/26.04, Debian 12/13, Fedora 44, and Arch ARM (as a systemd-nspawn container, because the Lima archlinux image does not boot on VZ). systemd 249-261; cgroup v2, v1 hybrid and v1 legacy.
- The planned primary works everywhere: `systemd-run --user --unit=seamless-update-<ULID> --collect -p Type=exec -p RuntimeMaxSec=1800 -- <exe> update --auto ... --config <abs>`, run from inside seamless.service.
  - It returns in 3-25ms.
  - The updater lands in app.slice/seamless-update-<ULID>.service, survives restart AND stop of seamless.service with no signal, and can restart the service itself.
  - Real seamlessd restarts to healthy in 35-68ms on empty data dirs.
- Plain and Setsid children are SIGTERMed with the daemon on restart and stop (KillMode=control-group), then SIGKILLed at the stop timeout.
- No D-Bus is needed: systemd-run --user reaches $XDG_RUNTIME_DIR/systemd/private. Debian 12/13 ship no user bus and it still works. Only --wait/--pipe/--pty need the bus.
- The transient unit gets the user manager's env (HOME, PATH, XDG_RUNTIME_DIR), not the daemon's. There is no SEAMLESS_CONFIG, so --config is load-bearing. Proxy/CA vars must be passed as explicit --setenv=K=V; the bare --setenv=NAME form fails on systemd 249.
- The --scope fallback works but adds nothing, and inherits the daemon's env. Dropped: "transient service, else notify-only".
- inSystemdUnit, probeSupervised and outsideServiceCheck give correct verdicts on every captured sample, v1 legacy included (no 0:: line).
  - Two minor gaps: a sub-cgroup of the unit reads as outside, and a system-level /system.slice/seamless.service matches.
- Without linger, user@UID stops ~10s after the last session ends and kills the daemon and every updater. With linger, everything survives and spawning works with zero sessions.
  - Self-service linger works where polkit is installed; Fedora Cloud has none.
- BUG: 6 starts in 10s hit StartLimitBurst=5 ("start-limit-hit") and Restart=always never recovers. The installer's restart then dies, and a rollback restart can too.
  - Fix: StartLimitIntervalSec=0 in the unit, and/or reset-failed before the restart.
- WSL2 was not covered (it needs Windows).
- Memories:
  - decision linux-updater-detach-systemd-run-transient-service;
  - gotchas systemd-user-start-limit-leaves-seamless-down, systemd-run-setenv-inherit-form-fails-on-249, loginctl-enable-linger-explicit-name-and-polkit;
  - reference lima-linux-vm-testing-on-apple-silicon;
  - systemd-cgroup-kill-reaches-setsid-children (confirmation appended).
- VM seamspike-u24 (Ubuntu 24.04) is kept, stopped and clean, for 2.16.
