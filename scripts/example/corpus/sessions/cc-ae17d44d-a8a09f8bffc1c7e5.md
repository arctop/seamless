---
id: 01M4H36388N77HSP2V1CHJ4GT3
name: cc/ae17d44d-a8a09f8bffc1c7e5
status: completed
started: 2026-10-09T19:45:33Z
---
(auto-harvested) The Windows spike is done on the test machine except for the interactive-desktop check you're deferring. The planned Windows updater design holds up, and the spike found one real bug in how seamlessd restarts its service on Windows.

The test machine is headless, so the daemon ran under an S4U principal standing in for `install.ps1`'s Interactive one. Your post-release check covers whether the real principal behaves the same. That's 7 trials in the `detached-updater-os-spikes` lab, on Windows 11 build 26200.

| Checklist item | Result |
|---|---|
| Is the daemon in a job, with which flags? | Yes, but it's one job shared across all tasks (it also held Windows' scheduled Defrag). Its only limit is "children leave the job automatically", and Task Scheduler switches even that on and off. Nothing kills the job's members when it closes. |
| Does stopping the task kill children? | **No.** `Stop-ScheduledTask`, `schtasks /End` and `taskkill /F` end only the daemon. Children survive however they're spawned: plain, detached, no-window or breakaway. |
| Can a standard user register the update task? | **Yes**, as an Interactive-logon task created with `schtasks /Create /XML`. This avoids the PowerShell task cmdlets and works even over SSH as a standard user. A run-whether-signed-in (S4U) task gets "Access is denied" for a standard user. |
| No window flashes | Not tested; it's your post-release check, filed as its own plan step. seamlessd has no code to hide its console, so the daemon task, or an Interactive update task, may show a console or Windows Terminal window. |

**The bug: restarting the service can leave it down.** `schtasks /End` returns almost at once, but Task Scheduler only kills the old process about 1.1s later, without sending it any shutdown signal. A start issued in that gap can fail to bind the port. The task's restart-on-failure setting doesn't retry an action that exits with code 1, so the daemon stays down until the next logon. That happened in 1 of 2 back-to-back restarts, and a
