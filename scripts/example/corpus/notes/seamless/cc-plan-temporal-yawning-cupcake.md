---
id: 01M4GHBSMX5152D2Y4TGBEA15V
slug: cc-plan-temporal-yawning-cupcake
title: "Plan: Background update check + automatic updates (Claude Code-style)"
project: seamless
tags: [plan:plan-background-update-check-automatic-updates-claude-code-style, plan-status:approved]
---
> captured from cc/8c95fc6d-ef1b5619af1c9370 | temporal-yawning-cupcake.md | iter 38 | git b2fdbcf27c7b | 2026-10-09T15:22:27Z

# Plan: Background update check + automatic updates (Claude Code-style)

## Context

Today Seamless updates only when someone runs `seamlessd update`, which re-runs
the Sigstore-verified installer, or remembers `seamlessd update --check`. The
docs ask users to update weekly because releases land often, so installs
drift. Goal: Seamless keeps itself current the way Claude Code's native
install does:
- it checks by itself;
- it applies in the background and takes effect with a ~2s restart at a quiet
  moment;
- it says so afterwards;
- it can be turned off;
- package-manager and source builds are told about updates, never touched.

**Owner decisions (2026-10-09)**
1. The check runs inside the daemon (startup, then every few hours) against
   the GitHub releases API. There is no separate OS-scheduled job and no
   self-hosted manifest.
2. Auto-apply is on by default for installs made by the curl/PowerShell
   installer. These are notify-only: Homebrew, `make install`/`go install`
   source builds, client-only machines (no daemon) and unknown layouts.
3. Restart when idle, with a deadline: no live agent session and nothing in
   flight. If an update has been pending more than 24h, take the next lull in
   requests.
4. A 24h soak. Auto-apply only takes releases at least `update.min_age` (24h)
   old, so a bad release can be pulled first by marking it prerelease. Manual
   `seamlessd update` and console "Update now" ignore the soak.
5. LAN-shared servers follow the same rules. The idle gate already counts
   remote sessions.

**Constraints carried in**
- **One upgrade implementation.** Apply = the existing path: verify the
  installer's Sigstore bundle in-process (`update_verify.go`), then run the
  installer. No fetch/swap/service logic in Go (memory
  `seamlessd-update-command`).
- **macOS needs a full reload after a binary swap.** A swapped binary needs
  `launchctl bootout` + `bootstrap`, never `kickstart -k` (memory
  `macos-26-amfi-...`). `docs/install` still kickstarts after bootstrap
  (install:611); that gets fixed here.
- **No telemetry.** The check is one anonymous GET: fixed UA, no token, no
  identifiers. `update.check: false` means zero update traffic.
- **Config files are never edited.** Present-but-invalid values are errors.
  Old binaries reject unknown keys (`KnownFields`).
- **Console rules.** Cookie POSTs pass the CSRF origin check, and pages morph
  in place without a reload.
- **The signer pin is arctop-only.** Every release auto-update can target or
  roll back to is at or above the floor release (the first release carrying
  Phase 2), so it is arctop-signed.

## How it maps to Claude Code

| Claude Code (native) | Seamless |
|---|---|
| Checks on startup and periodically | Daemon checker: first check 2-10 min after start, then every `check_interval` (6h ±10%), scheduled by wall clock so laptop sleep can't stall it |
| Downloads and installs in the background | Daemon spawns a **detached** `seamlessd update --auto`, outside the service's process tree. It runs the signed installer for the chosen tag |
| Takes effect at next start | The installer restarts the service; the daemon only spawns the updater when idle or at the deadline |
| Notice once installed | Briefing line + console banner + `update.applied` event, keyed off a version change |
| `claude doctor` shows the last attempt | `seamlessd doctor` "updates" row, and `update --check` |
| `DISABLE_AUTOUPDATER` | `update.auto: false` (check + notify continue). `update.check: false` means no network. Manual `seamlessd update` always works |
| Homebrew/WinGet: notice with the command | Homebrew, source, client-only and unknown installs: notify-only, with the right command |
| `stable` channel, `minimumVersion` | 24h soak via `update.min_age`. Named channels are out of scope |
| Keeps old versions | Pre-update backup (`archive.Export`), automatic rollback on failed health, and the DB restored when the schema advanced |

[…] The plan goes on for another 32,073 characters: the architecture, phase 1 (check and notify), phase 2 (unattended apply), the failure modes, verification, rollout and the task breakdown.
