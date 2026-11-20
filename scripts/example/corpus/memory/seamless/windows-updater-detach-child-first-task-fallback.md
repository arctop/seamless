---
id: 01M4HQS7HYP0QW5XS6AVM6G98M
kind: decision
name: windows-updater-detach-child-first-task-fallback
description: "Windows updater: a CREATE_NO_WINDOW breakaway child first, the per-user on-demand Scheduled Task only as fallback (owner, 2026-10-09)"
project: seamless
created: 2026-10-10T01:45:31Z
invalid_at: null
superseded_by: null
source_session: 01M4HPJ03NSMG6HN75GWYQYHBM
model: claude-opus-5-5
tags: [windows, task-scheduler, auto-update, plan:plan-background-update-check-automatic-updates-claude-code-style, lab:detached-updater-os-spikes]
---
Owner decision 2026-10-09 (session cc/90aaef39): "flip it according to the evidence". The detached child is the PRIMARY way the daemon starts the Windows updater, and the per-user on-demand Scheduled Task is the fallback. This supersedes the task-first order the spike's decision recorded and 2.09 built (172ce57). The flip itself is in cmd/seamlessd/update_spawn_windows.go (windowsSpawner) and update_spawn.go (childElseTask).

## What Task Scheduler does (lab detached-updater-os-spikes, a Windows 11 test machine, build 26200, S4U stand-in)
- Task actions run under the Schedule service's svchost.exe, in ONE job shared across tasks. Its only limit flag is SILENT_BREAKAWAY_OK, and Task Scheduler toggles even that. No KILL_ON_JOB_CLOSE.
- `Stop-ScheduledTask`, `schtasks /End` and `taskkill /F` of the daemon end ONLY the action process. Children survive whatever their creation flags (plain, DETACHED, NO_WINDOW, BREAKAWAY).
- The stop is a hard kill ~1.1s after the call returns; see [[windows-schtasks-end-lag-and-the-serve-lock]].
- A standard user can register its own InteractiveToken on-demand task with `schtasks /Create /XML` (S4U is "Access is denied").

## Why child first
- It came back healthy in 0.81s, against 5.81s for the task path.
- It is windowless: CREATE_NO_WINDOW children showed no console window. An InteractiveToken task launching the console-subsystem seamlessd.exe likely shows one; that is unverified, and the post-release check covers it.
- Its start is certain: CreateProcess succeeds or fails synchronously. `schtasks /Run` rc=0 does not prove a start (0x41303, "has not yet run"), and a task that never runs never reaches a fallback.
- The task path gains nothing on the job: a task-launched updater lands in the same shared job as the daemon anyway.

## As built
- Child: `CREATE_NO_WINDOW|CREATE_NEW_PROCESS_GROUP|CREATE_BREAKAWAY_FROM_JOB`, NUL stdio, `go cmd.Wait()`. One retry without breakaway on ERROR_ACCESS_DENIED: the job has no KILL_ON_JOB_CLOSE, so a child inside it survives too.
- Fallback, only when the child cannot start and the daemon's ctx is not done: task SeamlessUpdate-<SID> (UTF-16 LE + BOM XML in <data_dir>/update/, InteractiveToken, LeastPrivilege, Hidden, PT1H, IgnoreNew), via `schtasks /Create /XML /F` + `/Run`. A Warn names the child's error.
- Both failing: `seamlessd.spawn: a detached process: <err>; the update task: <err>`. A failed child during shutdown is final (nothing has started).
- Self-check (outsideServiceCheck, Windows): refuse only inside a job with KILL_ON_JOB_CLOSE.
- Detect gate 9: the daemon's parent is svchost.exe AND the Seamless task's Principal UserId is this user.
- Uninstall still deletes SeamlessUpdate-<SID>, which can exist once the fallback has run.

## Still open
The post-release check with a signed-in Interactive principal: windows on screen, the job flags, gate 9 (task 01M4HCZR2W47BZAA5FRNRS1HT5).

Related: [[macos-updater-detach-setsid-outside-process-group]], [[linux-updater-detach-systemd-run-transient-service]].
