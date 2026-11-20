---
id: 01M4HCZR20Q34NZYNHJNVBAK6X
title: "Windows serviceControl restart races the old process: wait for the serve process to exit between schtasks /End and /Run"
status: done
plan: plan-background-update-check-automatic-updates-claude-code-style
project: seamless
created_by: cc/ae17d44d-a8a09f8bffc1c7e5
---
Owns: cmd/seamlessd/service.go (the windows actionRestart plan, plus actionStop if callers start right after it) and its tests.

The bug (lab detached-updater-os-spikes, a Windows 11 test machine, build 26200, 2026-10-09):
- `schtasks /End` returns in ~0.04s, but Task Scheduler hard-kills the task's process only ~1.1s later (1.06 / 1.18s measured).
- No console control event reaches the daemon, so seamlessd gets no graceful shutdown.
- `seamlessd restart` on Windows runs `/End` then `/Run` immediately. The new serve instance can start while the old one still holds the port and die with `bind: Only one usage of each socket address`.
- The task's RestartInterval/RestartCount does NOT retry an action that exits 1. The task sits Ready with LastTaskResult 0x1, and the daemon stays down until the next logon.
- Reproduced 1 of 2 immediate restarts in the restart-race trial, and in the update-task trial via Stop-ScheduledTask + Start-ScheduledTask.
- Waiting for the old process to exit first: healthy 2.2s / 3.9s after /End.

Who is exposed:
- `seamlessd restart`;
- the updater's rollback and unswapped-failure restarts, which go through serviceControl (plan 2.2 step 8, 2.5).

install.ps1's Stop-Daemon already does it right: Stop-ScheduledTask, then Stop-Process on `seamlessd.exe serve` processes, then Wait-Process -Timeout 15.

Fix: between /End and /Run, wait (<=15s) for the old serve process to exit, then /Run.
- Find the process by state.json running.pid, or by the exe + `serve` command-line match install.ps1 uses.
- If it is still alive at the deadline, kill it and wait again.
- Test the plan shape with fakes. Keep the darwin and linux branches unchanged.

OUTCOME 2026-10-09 (cc/90aaef39 fanout wave): DONE in a457dcc (not pushed).

Premises refuted by measurement (verified by the orchestrator):
- Real seamlessd does not hit the bind race. Since aba6c14 (in no release; the latest release is v0.6.0), serve takes <data_dir>/seamlessd.lock (main.go:242) BEFORE net.Listen (main.go:475) and waits up to 30s for it (filelock.go dataDirLockWait). A restarted serve therefore waits for the old process instead of failing to bind.
- The spike's race came from its mock daemon (spikew.exe), which bound at once.
- Remaining exposure: data dirs on file systems without locking (serve runs unguarded, main.go:247-251), and pre-aba6c14 binaries.
- The updater is not exposed. Rollback stops, then waits on the same lock (waitDaemonGone, update_engine.go:729-738, 780-797). The unswapped-failure path only calls start, after install.ps1's Stop-Daemon has waited. Nothing calls actionRestart.

[…] What landed, row by row.
