---
id: 01M4HY2MMRHM3387K7J02PM0WS
kind: runbook
name: drill-auto-update-rollback
description: "Rehearse the update rollback on a real installer install: touch update/test-fail-confirm, run seamlessd update from a terminal, then delete the marker"
project: seamless
created: 2026-10-10T03:35:31Z
invalid_at: null
superseded_by: null
source_session: 01M4HW1QD1V5GJD4PJS4D05SFA
tags: [update, runbook, drill, plan:plan-background-update-check-automatic-updates-claude-code-style]
---
Rehearse the automatic-update rollback on a real installer install, release >= v0.7.0 (the first with checksums bundles, which preflight needs for the From release). Written 2026-10-09 from update_engine.go (confirm/confirmDrillName, install, rollback), update.go (attended path) and fold.go; not yet executed on a live install (task 2.16 does that).

1. **Turn the drill on:** `mkdir -p ~/.seamless/update && touch ~/.seamless/update/test-fail-confirm` (Windows: `New-Item -ItemType File -Force $HOME\.seamless\update\test-fail-confirm`). `seamlessd doctor` now warns `update drill`; `seamlessd update --check` shows a `drill` row.
2. **Trigger it from a terminal** with the attended updater: `seamlessd update` (needs a newer release), or `SEAMLESS_VERSION=<running version> seamlessd update` (a pinned reinstall, which also confirms and so also rolls back). Prefer this over console Update now: a manual (WhyManual) attempt is only shown -- no block, no backoff, no banner, no briefing line. An Update now (WhyNow) rollback BLOCKS that release for automatic updates, and only a newer release clears a block.
3. **Expect, in order:** verify, preflight, backup to `~/.seamless/backups/pre-update-v<from>-<utc>.tar.gz`, the installer runs, then at once (no polling) "confirm: v<to> not confirmed -- the rollback drill is on (test-fail-confirm exists)". Then "rollback: restoring v<from>"; the DB is put back only if the schema moved; then "v<from> serving again". The drill never fails the rollback's own confirm. The command exits non-zero with "v<to> not installed (stopped at rollback): ... rolled back to v<from>". `~/.seamless/update/attempt.json` has stage "rollback", rolled_back true; console Last attempt shows "rolled back" (seamlessd update, run by hand). The restart back to From sets no hold, because the attempt explains the downgrade.
4. **Turn the drill off:** delete `~/.seamless/update/test-fail-confirm`. While it exists EVERY update (automatic, Update now, seamlessd update) fails its confirmation and rolls back. Re-run `seamlessd doctor`; the `update drill` row must be gone.
5. **Retention:** each run writes one backup and one log. Backups keep the newest 2 (only pre-update-v* files are pruned); attempt logs keep the newest 20.

Recovery if anything goes wrong: [[recover-from-failed-auto-update]].
