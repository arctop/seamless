---
id: 01M4GMHS10DFM2WCG177YFCY9C
kind: decision
name: auto-update-architecture-and-policy
description: "2026-10-09 owner decisions for Claude Code-style auto-update: in-daemon GitHub check, auto-apply on installer installs only, idle+deadline restart…"
project: seamless
created: 2026-10-09T15:29:47Z
invalid_at: null
superseded_by: null
source_session: 01M4GM7FZR3W5KHMNZE67XWCNE
tags: [update, auto-update, plan:plan-background-update-check-automatic-updates-claude-code-style]
---
Owner decisions, 2026-10-09 (plan:plan-background-update-check-automatic-updates-claude-code-style; full design in note cc-plan-temporal-yawning-cupcake).

1. **Where the check runs:** inside `seamlessd serve` (first check 2-10 min after start, then every `update.check_interval`, default 6h +-10%, scheduled by wall clock so laptop sleep cannot stall it) against the GitHub releases LIST endpoint (`/repos/arctop/seamless/releases?per_page=20`, ETag). No OS-scheduled job, no self-hosted manifest.
2. **Who auto-applies:** only installs made by the curl/PowerShell installer (`update.Detect` kind Installer: release build + installer-written service running this exe + installer marker + installer config path + supervised instance). Homebrew, `make install`/`go install` source builds, client-only machines and unknown layouts are notify-only, with the right command as the hint.
3. **When it restarts:** when idle (no live agent session, nothing in flight); once pending longer than `update.max_defer` (24h), at the next 90s lull; forced at 1.5x.
4. **Soak:** auto-apply only takes releases at least `update.min_age` (24h) old by GitHub's clock, so a bad release can be pulled first by flipping it to prerelease. Manual `seamlessd update` and console "Update now" skip the soak.
5. **LAN-shared servers** follow the same rules; the idle gate already counts remote sessions.

Design pillars:
- ONE upgrade implementation: apply = verify the release's installer Sigstore bundle in-process, then run the installer ([[seamlessd-update-command]]). No fetch/swap/service logic in Go.
- `update.check: false` = zero update traffic; `update.auto: false` keeps check + notify. An explicit file/env false locks the console toggle off (the more restrictive setting wins -- unlike the briefing override, where the DB row wins).
- No telemetry: one anonymous GET, fixed UA `seamlessd-update-check`, never `GITHUB_TOKEN`.
- Phase 1 (check + notify) ships alone; Phase 2 (unattended apply, detached updater, rollback) needs OS spikes first. The first release carrying Phase 2 is the floor every auto-update can roll back to, so never delete releases at or above it.

Related: [[dev-and-fixture-daemons-never-self-update]], [[update-surfaces-render-parsed-versions-only]], [[github-releases-latest-is-not-max-version]].
