---
id: 01M4HJDG4Q0JGSMEWHFYFF1QBK
kind: reference
name: lima-linux-vm-testing-on-apple-silicon
description: "Lima on the owner's Mac for Linux tests: VM seamspike-u24 kept for 2.16; uid 501 breaks journalctl --user; archlinux template won't boot on VZ"
project: seamless
created: 2026-10-10T00:11:44Z
invalid_at: null
superseded_by: null
source_session: 01M4HDW50R4DMHKDFGGTX8EZXA
tags: [linux, lima, testing, plan:plan-background-update-check-automatic-updates-claude-code-style]
---
Set up 2026-10-09 (session cc/90aaef39) for spike 2.01's Linux half. Lima 2.2.1 (`brew install lima`), VMType vz, arm64.

- Kept: VM `seamspike-u24` (Ubuntu 24.04, systemd 255), stopped, cleaned (no test users, units, binaries or linger), for the live E2E task 2.16. Start it with `limactl start seamspike-u24`; delete it with `limactl delete seamspike-u24`.
- Images are cached in ~/Library/Caches/lima (~14 GB after the spike); `limactl prune` frees them.
- Templates: `limactl start --list-templates`; Lima 2.x syntax is `template:ubuntu-24.04`. Useful: ubuntu-22.04/24.04/26.04, debian-12/13, fedora-44.
- uid 501: Lima mirrors the macOS uid, and 501 < 1000 is a system uid to journald. Its user logs go to system.journal and `journalctl --user` fails. For journal-dependent tests, create a uid >= 1000 user.
- The archlinux template (2022 aarch64 image) stops about 1s after boot under VZ, and QEMU is not installed. Arch ran as a systemd-nspawn container inside the Fedora VM (`machinectl shell`).
- The host home is mounted read-only in the VM. /private/tmp (the scratchpad) is not mounted, so copy files in with `limactl copy <file> <vm>:/tmp/`.
- The spike's harness, scripts and samples live in the cc/90aaef39 scratchpad (agent-linux/), which is not durable. The trials in lab detached-updater-os-spikes are the record.

Related: [[linux-updater-detach-systemd-run-transient-service]].
