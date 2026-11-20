---
id: 01M4GW0J6068NKD5GG5PHMNJM4
kind: stage
name: phase2-prereqs-on-local-main-release-hold
description: "DONE 2026-10-10: main pushed and released as v0.7.0 (Phase 1 + Phase 2 together); history of the hold kept below"
project: seamless
created: 2026-10-09T17:40:12Z
source_session: 01M4GRW9WKM0CRYZ6ZQXC1YFPD
model: claude-opus-5-5
tags: [update, release, plan:plan-background-update-check-automatic-updates-claude-code-style]
---
Status: done
Gate: human

DONE 2026-10-10 (cc/448e543f): the owner chose main's HEAD as the release point. Pushed 25e7924 over SSH, tagged and released v0.7.0 (signed and notarized), changelog refreshed in 88d46a4. Main = origin/main. What follows is history.

Since 2026-10-09 (session cc/8234de8d), local `main` carries eight Phase 2 prerequisite commits on top of a54feff, the last Phase-1-only (check + notify) commit: 741c5c4, 5f2fbdc, 20d3169, ad8aa50, c21297a, aba6c14, 0bad23c, 11286f3. The cc/47575b9c wave (same day) added four more:
- e4da8d9: WhyManual in the attempt contract.
- 74e2e8b: per-event `seam hook` dial budgets.
- b0bc49b: 2.11, the daemon's Decide/Target/fold, inert until 2.13 wires a spawner.
- d8519bb: 2.08, the updater engine.
None of it is pushed: 18 commits ahead of origin/main as of d8519bb.

While this holds, do NOT push main or tag a release from main's HEAD without the owner choosing the release point:
- Pushing main deploys docs/ through GitHub Pages at once, including docs/install and docs/install.ps1 (0bad23c, the live curl/irm installers) and docs pages that describe update.auto/max_defer/min_age. Those keys do nothing until tasks 2.11 and 2.13 land.
- ad8aa50 edits .github/workflows/release.yml, so the push must go over SSH ([[push-workflow-change-needs-ssh-not-gh-https]]).
- From d8519bb on, a release CHANGES what a manual `seamlessd update` does. Unlike the auto-update docs, this is not inert:
  - it targets the newest release by semver;
  - it verifies the installer and checksums.txt under the exact tag (Sigstore);
  - on an installer-made install it backs up to <data_dir>/backups, confirms via /healthz and rolls back on failure;
  - already up to date, it changes nothing.
  The docs pages pushing main would publish describe exactly this. `seamlessd update --auto` stays unspawned until 2.09 and 2.13.
- The options are recorded on the plan's gate task 01M4GMFBTK1D934E678HB4AKM3: push and tag a54feff alone for the Phase 1 release, or push everything.

Flip to done once the owner has pushed or tagged per their decision.

UPDATE 2026-10-09 (session cc/90aaef39, fanout wave): six more commits, still unpushed. Main is 24 ahead of origin/main as of 267ed0a.
- 416a090 + 267ed0a: docs/install and the Makefile retry the launchd bootstrap every 0.1s. Live make install restart is now 1.42s (was 2.6s).
- a457dcc: Windows `seamlessd stop`/`restart` wait on seamlessd.lock.
- 4ca66ae: doctor and `update --check` automatic-update rows. LIVE at once in a release: the new doctor checks "update drill" and "tls trust" (tls trust prints OK/WARN on any TLS install).
- 00274a7: console Settings > Updates controls and banners. Inert until 2.13: they need CanApply.
- 172ce57: the per-OS updater spawner (not wired until 2.13).
  - Also LIVE in a release: Detect's Windows gate 9 can now pass, so Windows installs become KindInstaller (still notify-only while no Spawner is wired), and inSystemdUnit is stricter.
  - docs/install's systemd unit gains StartLimitIntervalSec=0 plus a reset-failed before restart.
What a push would now also publish via Pages: docs/install with the 0.1s launchd loop and the systemd start-limit fix. Both are backward-compatible installer improvements. The release-point options on gate task 01M4GMFBTK1D934E678HB4AKM3 still apply, with these commits added to "everything".

UPDATE 2026-10-10 (sess/gwyqyhbm): two more unpushed commits; main is 26 ahead of origin/main as of aaefd58.
- d5da89d: the Windows updater starts as a detached child first; the Scheduled Task is the fallback (owner decision).
- aaefd58: Developer ID signing + notarization of the darwin binaries in the release.
  - It edits .github/workflows/release.yml, so the push must go over SSH ([[push-workflow-change-needs-ssh-not-gh-https]]).
  - A release of arctop/seamless now REQUIRES the five MACOS_* secrets (set 2026-10-10) and stops at its first step without them.
  - A release cut from a54feff (option (a) on the gate task) predates this and ships adhoc binaries.

UPDATE 2026-10-09 (cc/095a86b1, fanout wave): four more unpushed commits. Main is 30 ahead of origin/main as of 39d8be9.
- 1d0246e: 2.13 wires the spawner, activity, LiveSessions and probes into the daemon. NOT INERT ANY MORE: a release cut from main's HEAD AUTOMATICALLY INSTALLS later releases on curl/PowerShell installer installs (macOS, Linux systemd user service, Windows), subject to auto/min_age/max_defer. The console's Auto / Update now / Resume controls and doctor's automatic rows go live with it. Source builds and snapshots stay notify-only.
- fef3d15: shared update wording (three CLI/console strings changed slightly).
- 03426a5: Windows-portable cmd/seamlessd tests.
- 39d8be9: CI gains os-smoke (macos-latest, windows-latest) and config-lint. It edits .github/workflows/ci.yml, so the push must go over SSH ([[push-workflow-change-needs-ssh-not-gh-https]]). The first push is also the first real run of those jobs.
Docs for automatic updates (2.15) have NOT landed yet: a release from HEAD would ship live auto-update with docs that still say "nothing installs itself" in places (console.md, troubleshooting.md, updating.md).

UPDATE 2026-10-09 (cc/448e543f, fanout wave + release prep): four more unpushed commits. Main is 34 ahead of origin/main as of 25e7924. The full `make check` is green on the tree.
- a55011b: typed gate refusal (attempt.Refusal) with the owner action in the console, doctor and --check.
- 5e4b3b4: the failed-rollback recovery text names the moved backup; doctor's tls trust consequence is corrected.
- a2b5f29: 2.15 docs (the "nothing installs itself" lines are gone), AGENTS.md/CLAUDE.md release-discipline rules, the landing-page phone-home FAQ.
- 25e7924: "docs: prepare v0.7.0" (server.json 0.7.0).
The docs blocker named in the cc/095a86b1 update is resolved: a release from 25e7924 ships live auto-update WITH matching docs.
Pre-push test: the new docs/install at HEAD was run in Lima VM seamspike-u24 (Ubuntu 24.04 arm64) against GitHub's latest release, v0.6.0. Exit 0; the systemd unit has StartLimitIntervalSec=0; healthz ok. So curl installs stay safe in the window between pushing main and the v0.7.0 tag. The VM is left on v0.6.0 with linger on, as the by-hand upgrade test target.
Still waiting on the owner's say-so to push (over SSH) and tag.

> Archived (gardener staleness) on 2026-11-20
