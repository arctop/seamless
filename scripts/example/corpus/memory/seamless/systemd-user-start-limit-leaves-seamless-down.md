---
id: 01M4HJDE5QXCHG6EFBMP98M9TV
kind: gotcha
name: systemd-user-start-limit-leaves-seamless-down
description: "6 starts of seamless.service in 10s hit systemd's StartLimitBurst=5: restart fails and Restart=always never recovers; reset-failed or…"
project: seamless
created: 2026-10-10T00:11:42Z
invalid_at: null
superseded_by: null
source_session: 01M4HDW50R4DMHKDFGGTX8EZXA
tags: [linux, systemd, installer, auto-update, lab:detached-updater-os-spikes]
---
Measured 2026-10-09 (lab detached-updater-os-spikes; trials 01M4HG3THG6GDBBP8WMNXFMP9K Ubuntu 24.04, 01M4HHJQFHM7SMS020S5XM3S71 Arch, 01M4HHTWA9CRRZ9YNED2NW00WR the fix).

- systemd's default start rate limit is 5 starts per 10s on every distro measured. docs/install's seamless.service (service_systemd) sets neither StartLimitIntervalSec nor StartLimitBurst.
- The 6th start inside 10s ends in "start-limit-hit". Restart=always does NOT recover it: the service stays down until a start after the window, or `systemctl --user reset-failed seamless.service`.
- systemctl's text: "Job for seamless.service failed." on 255; "...because start of the service was attempted too often." plus a reset-failed hint on 261.
- Who hits it:
  - a crash-looping daemon (RestartSec=2 gives ~5 starts in 10s);
  - the installer's own `systemctl --user restart seamless.service || die` (docs/install ~:757), which then dies;
  - an updater's restart or rollback right after a crash loop.
  It also happened unprompted on Arch after an aborted run left the unit enabled.
- Fix, either or both (both measured): `StartLimitIntervalSec=0` under [Unit] (8 of 8 rapid restarts succeeded; matches launchd's KeepAlive, which throttles but never gives up), and `systemctl --user reset-failed seamless.service` before the restart (recovers at once).
- A release's unit change reaches an install only when that release's installer rewrites the unit. Rollback targets are at or above the Phase 2 floor, so the floor release must carry the fix.

Related: [[linux-updater-detach-systemd-run-transient-service]].

**Fixed 2026-10-09 in 172ce57** (task 2.09): docs/install's unit sets `StartLimitIntervalSec=0`, and the installer runs `systemctl --user reset-failed seamless.service` before its restart. The fix was verified in the Ubuntu 24.04 VM (systemd 255):
- old unit: 8 rapid restarts gave rc "0 0 0 0 0 1 1 1" (start-limit-hit);
- new unit: 16 of 16 succeeded;
- after rewriting the unit plus daemon-reload, a restart succeeded even with the limit already hit, so the reset-failed is defensive (it covers a rewrite that did not load).
Installs made before this release keep the limit until their installer runs again; the manual recovery is `systemctl --user reset-failed seamless.service && systemctl --user restart seamless.service`.
