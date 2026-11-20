---
id: 01M4HJDC7PC273P6DZ6A6KM3BD
kind: decision
name: linux-updater-detach-systemd-run-transient-service
description: "2.01 spike (Linux, systemd 249-261): spawn the updater as a transient user service via systemd-run --user (Type=exec, --collect); no Setsid, no…"
project: seamless
created: 2026-10-10T00:11:40Z
invalid_at: null
superseded_by: null
source_session: 01M4HDW50R4DMHKDFGGTX8EZXA
tags: [linux, systemd, auto-update, plan:plan-background-update-check-automatic-updates-claude-code-style, lab:detached-updater-os-spikes]
---
Built 2026-10-09 in 172ce57 (update_spawn_linux.go; task 2.09). Settled 2026-10-09 by lab `detached-updater-os-spikes` (24 "Linux ..." trials, session cc/90aaef39) on Lima VMs (arm64): Ubuntu 22.04/24.04/26.04, Debian 12/13, Fedora 44, Arch ARM (as an nspawn container); systemd 249-261; cgroup v2 and v1 hybrid and legacy. Feeds task 2.09's Linux spawner. Not covered: WSL2, x86_64, systemd < 249.

## Spawn (from inside seamless.service)
```
systemd-run --user --unit=seamless-update-<ULID> --collect -p Type=exec -p RuntimeMaxSec=1800 [--setenv=K=V ...] -- <exe> update --auto --attempt <ULID> --from X.Y.Z --to X.Y.Z --why auto --config <absolute path>
```
- Run systemd-run as a child, buffer its output, Wait with a ~30s timeout (it returns in 3-25ms). rc=0 means the unit was created and exec'd. Whether the update worked comes only from the attempt record.
- rc != 0 -> notify-only. stderr classes (substring match):
  - "Failed to connect to": the manager is unreachable (XDG_RUNTIME_DIR unset or bogus). Wording varies by version.
  - "already loaded or has a fragment file": this attempt's unit exists; never respawn under that ULID.
  - "Failed to find executable": the binary is missing.
  - "Job for ... failed": exec failed (with -p Type=exec, systemd 252+).
- The updater lands in `.../user@UID.service/app.slice/seamless-update-<ULID>.service`. It survives `systemctl --user restart` AND `stop` of seamless.service with no signal, and can restart the service itself (the installer's step).
- A 26-char ULID is a valid unit name as is. systemd-run silently escapes invalid names (rc=0), so only ever pass the validated ULID.
- `--collect` removes the unit whatever the outcome. Without it, a failed run blocks the name until reset-failed. RuntimeMaxSec is enforced (SIGTERM, then SIGKILL after TimeoutStopSec); 1800s is twice the installer timeout. KillMode=control-group also reaps the installer's leftovers when the updater exits.

## Environment
- The transient unit gets the USER MANAGER's environment, never the daemon's: HOME, PATH (as short as /usr/local/bin:/usr/bin), XDG_RUNTIME_DIR, LANG, USER, LOGNAME, SHELL, plus DBUS_SESSION_BUS_ADDRESS only where a user bus exists.
- No SEAMLESS_CONFIG, hence `--config` is load-bearing. Nothing from seamless.service's Environment= or drop-ins arrives either (a proxy, for one).
- Pass what the updater needs explicitly, and only for values the daemon has set and non-empty: `--setenv=HTTPS_PROXY=...` etc. (HTTP(S)_PROXY, NO_PROXY, ALL_PROXY in both cases, SSL_CERT_FILE, SSL_CERT_DIR).
- Never the bare `--setenv=NAME` form ([[systemd-run-setenv-inherit-form-fails-on-249]]).
- No D-Bus is needed: systemd-run --user reaches the manager's private socket ($XDG_RUNTIME_DIR/systemd/private), and works on Debian, which ships no user bus. Only --wait/--pipe/--pty need the bus; never use them, nor --no-block.

## Rejected
- Setsid or plain child: KillMode=control-group SIGTERMs the whole service cgroup on restart and stop, then SIGKILLs at the stop timeout ([[systemd-cgroup-kill-reaches-setsid-children]]).
- The plan's `--scope` fallback: it works, but it gave no extra resilience against any measured failure (same manager connection, same exe). It also keeps the daemon's env (SEAMLESS_*, stale JOURNAL_STREAM) and pgid/sid. "Transient service, else notify-only."

## Self-checks
- outsideServiceCheck and probeSupervised (cmd/seamlessd/install_probe.go inSystemdUnit) give the right verdicts on every captured sample, v1 legacy included (no "0::" line; the name=systemd branch carries it).
- The spike found two minor gaps, both fixed in 172ce57: a sub-cgroup of the unit (`.../seamless.service/x`) read as outside, and a SYSTEM unit `/system.slice/seamless.service` matched too. inSystemdUnit now wants `user@<uid>.service` followed by `seamless.service` at any depth.

## Preconditions
- Linger. Without it, user@UID stops ~10s after the last session ends and kills the daemon and any updater, whatever the spawn method. With linger, everything survives and spawning works with zero sessions. See [[loginctl-enable-linger-explicit-name-and-polkit]].
- The start limit: [[systemd-user-start-limit-leaves-seamless-down]].

Related: [[macos-updater-detach-setsid-outside-process-group]], [[windows-updater-detach-under-task-scheduler]], [[auto-update-architecture-and-policy]].
