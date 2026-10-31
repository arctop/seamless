---
title: Configuration
description: Every configuration key, its type and default, plus the annotated example file and the four layers that resolve them.
generate: config
---

Seamless reads a single YAML file. Every key also has a `SEAMLESS_*` environment
override.

## Where the config comes from

The file is looked up in this order, first hit wins:

1. `$SEAMLESS_CONFIG`
2. `~/.config/seamless/seamless.yaml`
3. `./seamless.yaml`

## Precedence

Four layers resolve each key. Later layers win:

1. **Defaults** - the built-in values in the table below.
2. **File** - whatever the YAML sets.
3. **Environment** - `SEAMLESS_*` overrides the file.
4. **Runtime override (DB)** - the console's Settings forms store some blocks in
   the database. They win over file *and* env, take effect without a daemon
   restart, and stay until reset.

That fourth layer covers three blocks where the stored row simply wins:

| Block | Written by | When it takes effect |
|---|---|---|
| `briefing:` | Settings → [Briefing](/reference/console/#briefing) | From the next session start. |
| `features:` | Settings → [Features](/reference/console/#optional-features) | Immediately in the console; an agent sees it from its next session (tool lists and briefings alike). |
| `console.level` | Settings → [Experience](/reference/console/#experience), or the Home welcome card | Immediately, in the console only. |

The `update:` block's two switches, `update.check` and `update.auto`, are merged
the other way round: the **more restrictive** value wins. Settings →
[Updates](/reference/console/#updates) can always turn either off, and can turn
it on when file and env do not say `false` - but an explicit `check: false` or
`auto: false` in the file (or `SEAMLESS_UPDATE_CHECK=false`,
`SEAMLESS_UPDATE_AUTO=false`) is final, and the console shows that switch locked
with where it was set. `check: false` promises no update traffic at all, so it
is never one click from untrue, and with checks off nothing installs by itself
either. See [the update block](#the-update-check).

It exists so you can change what agents get injected - and what they can reach -
while they are running, and it is the one place where the config file is not the
last word: check the console before concluding a `briefing:`, `features:`, or
`console.level` setting is being ignored. No form ever writes your config file,
and **Reset** on each clears the stored row and hands that block back to
file/env.

Overrides can be in force without you having set them. Upgrading an
installation that already holds trial data seeds a stored `features:` override
with research on, so a feature that now ships off does not disappear from under
data you were already using. The console labels it a stored override, not your
choice; **Reset** clears it like any other.

### The console level

`console.level` (`SEAMLESS_CONSOLE_LEVEL`) is how much of the
[console](/reference/console/#choose-how-much-you-see) you see: `basic`,
`standard`, or `advanced`. It is presentation only - briefings, MCP tools,
hooks, recall, and the gardener are identical at every level - and a screen a
level leaves out of the sidebar still opens from a link. A value outside those
three stops the daemon at startup with an error naming them; an empty value
means the default, `basic`.

A fresh installation starts at `basic`. Upgrading an installation that had
already recorded sessions stores `advanced` once, as the same kind of runtime
override (labeled "set by the upgrade" in Settings → Experience), so an upgrade
never hides a screen you were using. Like any override, it wins over this key
until **Reset** hands the level back to file/env.

### The update check {#the-update-check}

The `update:` block drives the daemon's
[automatic update checks](/updating/#automatic-update-checks) and
[automatic updates](/updating/#automatic-updates):

- `check` (`SEAMLESS_UPDATE_CHECK`) is optional. Left unset, a release build
  checks and a build from source does not; `true` or `false` decides either
  way. `false` means no request to GitHub at all, and so no automatic update
  either.
- `check_interval` (`SEAMLESS_UPDATE_CHECK_INTERVAL`) is the time between
  checks: a Go duration from `1h` to `720h`, default `6h`. Write the unit - a
  bare number other than `0` is refused rather than guessed at, since `6` could
  mean six seconds or six hours.
- `auto` (`SEAMLESS_UPDATE_AUTO`) is optional, and unset means on: an install
  made by the curl or PowerShell installer installs a newer release by itself.
  Homebrew, builds from source, client machines and layouts the daemon cannot
  vouch for are only told, whatever `auto` says
  ([who updates itself](/updating/#who-updates-itself)). `false` keeps the
  check and its notices and leaves installing to you (`seamlessd update`, or
  **Update now** in the console). Like `check: false`, it is final, and the
  console cannot turn it back on. With checks off, `auto` is off too.
- `max_defer` (`SEAMLESS_UPDATE_MAX_DEFER`) is how long a pending automatic
  update waits for a quiet daemon - no live agent session, no request in
  flight - before it takes the next lull in requests instead: a Go duration from
  `1m` to `720h`, default `24h`. `0` is refused. After 1.5 times `max_defer` it
  installs as soon as no request is in flight, and after twice `max_defer`
  whatever is in flight ([when it installs](/updating/#when-it-installs)).
- `min_age` (`SEAMLESS_UPDATE_MIN_AGE`) is the soak: how long a release must have
  been published before an automatic update takes it, measured on GitHub's
  clock. A Go duration from `0` (no wait) to `720h`, default `24h`. Installing by
  hand ignores it.

Set `auto`, `max_defer` and `min_age` in the file, not in the daemon's
environment. Any `SEAMLESS_*` variable besides `SEAMLESS_CONFIG` in the
daemon's environment makes the install notify-only - the installer rewrites
the service without it - so `SEAMLESS_UPDATE_AUTO=true` can never turn
automatic updates on, and the two durations set there switch them off rather
than tune them.

All five keys are new in v0.7.0. seamlessd releases before it refuse config
keys they do not know, so a file that sets one runs only with v0.7.0 or newer.
The example file spells out the keys whose default has a value and leaves
`check` and `auto`, whose default is unset, commented out.

### Peer-user authentication {#peer-auth}

The `peer_auth:` block turns on a Unix-socket listener whose callers the daemon
identifies by the uid the kernel reports, and maps OS accounts to the projects
they may reach. It is off by default, and off is exactly the behavior of every
earlier release. The guide is [Seal projects to OS accounts](/guides/peer-auth/);
in short:

- `enabled` (`SEAMLESS_PEER_AUTH_ENABLED`) switches it on.
- `owner` (`SEAMLESS_PEER_AUTH_OWNER`) is the OS account - a user name or a
  numeric uid - the TCP bearer key stands for, and whose socket connections act
  as the owner. Required when enabled.
- `socket.path` (`SEAMLESS_PEER_AUTH_SOCKET_PATH`) is the socket's absolute path,
  outside `data_dir`. Required when enabled.
- `socket.mode` (`SEAMLESS_PEER_AUTH_SOCKET_MODE`) is its permission bits in
  octal, default `0666`: every local account may knock, and the kernel still
  names each one. `socket.group` (`SEAMLESS_PEER_AUTH_SOCKET_GROUP`) hands the
  socket to a group, for a mode such as `0660`.
- `users` maps an OS account (name or uid) to `sealed` - projects reachable only
  from that account - and `projects` - projects it shares with the owner. It is
  file-only: a map of lists has no single-variable spelling.

A client of such a daemon names the socket as its `server_url`:
`unix:///absolute/path.sock`. It needs no `mcp.api_key`, and its hooks and MCP
registration go through the `seam` CLI (`install-hooks` does that by itself for
a socket URL).

The block is new after v0.7.4; releases before it refuse config keys they do
not know, so the example file leaves it commented out.

## Generating a key

`mcp.api_key` guards `/api/mcp` and the console. On a true first run - no
config file anywhere in the search order and no `SEAMLESS_MCP_API_KEY` in the
environment - `seamlessd serve` (or `install-hooks`) generates one and writes
it to `~/.config/seamless/seamless.yaml`, so a fresh install never handles the
key by hand. An existing config file is never edited, even when its key is
empty; set one yourself:

```bash
openssl rand -hex 32
```

The daemon still starts with an empty key, but every MCP and hook request is
rejected until one is set - `seamlessd doctor` reports it as a warning.
