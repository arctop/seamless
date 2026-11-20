---
id: 01M4HY2KR3N1CF0Q2K2ST7HYCX
kind: runbook
name: recover-from-failed-auto-update
description: "Recover when an automatic update or Update now did not apply: read the record, act by outcome, true-restore = stop, move data dir aside, import"
project: seamless
created: 2026-10-10T03:35:30Z
invalid_at: null
superseded_by: null
source_session: 01M4HW1QD1V5GJD4PJS4D05SFA
tags: [update, runbook, plan:plan-background-update-check-automatic-updates-claude-code-style]
---
How to recover when an automatic update (or Update now) did not apply. Written 2026-10-09 from code at a55011b+ (update_engine.go rollback/rollbackFailed/gates, fold.go, internal/archive/import.go, cmd/seamlessd/import.go); not yet run on a live install. User docs: /guides/troubleshooting/#an-automatic-update-failed.

1. **Read the record.** Three places: console Settings > Updates > Last attempt (outcome, updater error, log path); `seamlessd doctor` (`updates` row); `seamlessd update --check` (rows last/error/log/backup/fix). Full log: `~/.seamless/update/logs/<attempt>.log` (newest 20 kept). Backup: `~/.seamless/backups/pre-update-v<from>-<utc>.tar.gz` (newest 2 kept).
2. **Act by outcome:**
   - rolled back / failed / interrupted: From still runs, nothing is lost. A rollback or verify failure BLOCKS that release; the rest retry after a backoff (1h doubling to 24h). No action unless it repeats.
   - failed at `gates` with a refusal (the `fix` shows only while the daemon still runs the attempt's From): stale_binary or not_installer -> `seamlessd restart`; self_check -> update by hand with `seamlessd update`; auto_off -> nothing. Until you act, every retry is refused the same way.
   - not rolled back (broken; red banner; automatic updates paused): the daemon may be down. Go to 3.
3. **Broken recovery:**
   a. `seamlessd status`. If doctor warns `tls trust`, From is probably running and only the confirmation failed: the updater confirms through cfg.HTTPClient (system roots + tls.ca_file, dialing server_url), and so does the rollback (update_engine.go confirm), so a TLS install that cannot verify its own certificate ends every update "not rolled back". Fix the cert / ca_file / server_url first and do NOT restore data.
   b. Reinstall From pinned: `curl -fsSL https://thereisnospoon.org/install | SEAMLESS_VERSION=<from> sh` (Windows: `$env:SEAMLESS_VERSION='<from>'; irm https://thereisnospoon.org/install.ps1 | iex`).
   c. Only if the data must go back to its pre-update state, a TRUE restore. `seamlessd import --from <backup>` MERGES into a data dir that has seam.db or any memory/notes file (archive.DetectMode), and --force only bypasses the daemon-running guard for a fresh restore. So: `seamlessd stop`; `mv ~/.seamless ~/.seamless.broken`; `seamlessd import --from ~/.seamless.broken/backups/pre-update-v<from>-<utc>.tar.gz`; `seamlessd start`. The backup lives inside the data dir, so its path moves with it; since the 2.15 wave the attempt record's own recovery text names the moved path (`<data dir>.broken/backups/...`). Anything written after the backup stays in ~/.seamless.broken.
   d. Console Settings > Updates > Resume automatic updates: lifts the pause and any hold, resets the rollback count. Blocks stay; only a newer release clears them. Update now and `seamlessd update` ignore blocks.
4. **Gotchas:** `~/.seamless/update/test-fail-confirm` (the drill, [[drill-auto-update-rollback]]) forces every forward confirm to fail; doctor warns `update drill`; make sure it is gone. "not enough free space for the backup" stops before any change (backoff). "The updater never started": the daemon log line `update: could not start the updater` names why (per OS: /reference/service/#the-updater).
