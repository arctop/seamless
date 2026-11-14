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
3. `./seamless.yaml`, for `seamlessd` only

The third step is the daemon's: `seamlessd serve` and its one-shot commands
read a `seamless.yaml` in the working directory (the pre-install layout, where
`make run` serves a checkout's gitignored `seamless.yaml`). The `seam` CLI never does - not its commands, not `seam hook`, not
`seam mcp-proxy` or `seam mcp-headers`. Your agent client runs those in the
agent's repository, and a `seamless.yaml` committed there would otherwise
decide which server the session's hooks and bridge talk to. A `seam` command
that finds neither of the first two runs on the defaults and the environment.

To point `seam` at a file anywhere else, name it: `SEAMLESS_CONFIG=/path/to/seamless.yaml seam ...`,
or `--config` on `seam hook`, `seam mcp-proxy` and `seam mcp-headers`. The hooks
and MCP registration `seamlessd install-hooks` writes carry `--config` with the
absolute path of the file it loaded (or the one its own `--config` names), so
they read that file from any directory. `seam doctor` names the file it loaded,
and notes a `./seamless.yaml` it did not read.

## Precedence

Four layers resolve each key. Later layers win:

1. **Defaults** - the built-in values in the table below.
2. **File** - whatever the YAML sets.
3. **Environment** - `SEAMLESS_*` overrides the file.
4. **Runtime override (DB)** - the console's Settings forms store some blocks in
   the database. They win over file *and* env, take effect without a daemon
   restart, and stay until reset.

That fourth layer covers four blocks where the stored row simply wins:

| Block | Written by | When it takes effect |
|---|---|---|
| `briefing:` | Settings → [Briefing](/reference/console/#briefing) | From the next session start. |
| `features:` | Settings → [Features](/reference/console/#optional-features), or the Home welcome card's write-screening box | Immediately in the console; an agent sees it from its next session (tool lists and briefings alike), and write screening from its next write. |
| `console.level` | Settings → [Experience](/reference/console/#experience), or the Home welcome card | Immediately, in the console only. |
| `backup:` | Settings → [Backups](/reference/console/#backups) | From the next scheduled backup. |

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
last word: check the console before concluding a `briefing:`, `features:`,
`console.level` or `backup:` setting is being ignored. No form ever writes your config file,
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
- `release_signers` (`SEAMLESS_UPDATE_RELEASE_SIGNERS`) is optional and unset by
  default: the absolute path (or `~/...`) of an OpenSSH allowed_signers file of
  the owner's release keys. With it set, every update installs only a release
  whose `checksums.txt.sshsig` verifies against a key the file lists
  ([owner-signed releases](/updating/#owner-signed-releases)). The console can
  neither set nor clear it. Under peer_auth the file must be root's, through
  directories only root can write, or `serve` refuses to start. New after
  v0.8.3, and left commented out in the example file.

Set `auto`, `max_defer`, `min_age` and `release_signers` in the file, not in
the daemon's environment. Any `SEAMLESS_*` variable besides `SEAMLESS_CONFIG`
in the daemon's environment makes the install notify-only - the installer
rewrites the service without it - so `SEAMLESS_UPDATE_AUTO=true` can never
turn automatic updates on, the two durations set there switch them off rather
than tune them, and `SEAMLESS_UPDATE_RELEASE_SIGNERS` there pins only what you
install by hand.

The first five keys are new in v0.7.0, and `release_signers` after v0.8.3.
seamlessd releases before a key refuse config keys they do not know, so a file
that sets one runs only with a release that knows it. The example file spells
out the keys whose default has a value and leaves `check`, `auto` and
`release_signers`, whose default is unset, commented out.

### Scheduled backups {#backups}

The `backup:` block drives [scheduled backups](/guides/data/#backups-in-the-console):

- `auto` (`SEAMLESS_BACKUP_AUTO`) is optional. Left unset, a release build
  backs up on a schedule and a build from source does not, so a throwaway
  daemon never writes backups. `true` or `false` decides either way.
- `every` (`SEAMLESS_BACKUP_EVERY`) is the time between scheduled backups: a Go
  duration from `1h` to `720h`, default `24h`.
- `keep` (`SEAMLESS_BACKUP_KEEP`) is how many automatic backups to keep, from 1
  to 100, default 7. Pruning removes only this install's automatic backups past
  the newest `keep`, after the next one is written: never a backup you made, a
  safety backup taken before a restore, an updater's pre-update backup, or
  another install's backup in a shared folder.
- `dir` (`SEAMLESS_BACKUP_DIR`) is the folder backups go to: an absolute path,
  or one starting with `~/`. Empty means `<data_dir>/backups`. A folder inside
  the data directory other than that one is refused. The safety backup taken
  before a restore always stays in `<data_dir>/backups`.

The console's **Settings → Backups** is the recommended place for all four: it
checks a folder before it is used, and what it saves wins over the file and the
environment until you reset it there. The daemon's environment is a poor place
for them: any `SEAMLESS_*` variable there besides `SEAMLESS_CONFIG` makes an
install that updates itself [only notify](#the-update-check).

The block is new after v0.8.1. seamlessd releases before it refuse config keys
they do not know, so a file that sets any of these runs only with a release that
knows them, and a rollback to an older release would refuse it. That is why the
example file leaves the block commented out.

### Peer-user authentication {#peer-auth}

The `peer_auth:` block turns on a Unix-socket listener whose callers the daemon
identifies by the uid the kernel reports, and locks the projects you list until
you approve an agent's access. It is off by default, and off is exactly the
behavior of every earlier release. The guide is
[Lock projects behind your approval](/guides/peer-auth/); in short:

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
- `force_locked` lists force-locked projects: locks only this file can remove.
  The console can set and remove locks of its own with a passkey, but it can
  never change one listed here. Each locked project is closed to every owner
  request - its name shows, its content does not, writes are refused - unless the
  request rides a socket connection whose process holds a lease the owner
  approved. The TCP bearer key never holds one. The lock is not the `sealed`
  isolation state (which says what flows in and out of a project): a locked
  project is normally `confidential`, and `serve` refuses a force-locked one that
  is `open`. File-only. `locked`, this key's earlier name, still loads, and
  `seamlessd doctor` warns until it is renamed.
- `users` is a retired key. A config that sets it to a non-empty map is refused
  when it loads, even while the block is disabled, with a message that says
  to list the projects under `force_locked` and remove `users`
  ([the guide](/guides/peer-auth/#configure-it) has it word for word).
- `max_lease` caps every lease the owner approves on a locked project, "until
  this session ends" included: at most `24h`, unset `8h`. File-only.
- `restricted` lists restricted local accounts, each an OS user name or a
  numeric uid: a second account on the machine whose agents work beside
  yours. It connects over the socket only - the kernel names it, and it never
  holds the bearer key - and reaches every project that is `open` and not
  locked, reading and writing, plus the global scope read-only. It cannot
  create a project, files no access requests and holds no leases. Its writes
  are labeled with its name, and a constraint, convention, protocol or stage it
  writes waits for you to promote it
  ([Restricted local accounts](/guides/peer-auth/#restricted-local-accounts)).
  Refused while `enabled` is false, and for the owner. File-only.
- `relays` maps paired relays ([the guide](/guides/peer-auth/#agents-on-another-computer-relays)):
  each relay's name as the Devices page shows it, to `owner_uid`, your uid on
  that computer. Through a mapped relay that uid is you, judged as the socket
  is; any other uid is refused. An unmapped relay is you over TCP and holds no
  lease. Refused while `enabled` is false. File-only.
- `passkeys.rp_id` and `passkeys.origins` name the WebAuthn relying party of
  the owner's approver passkeys ([seamlessd approvers](/reference/cli-seamlessd/#seamlessd_approvers)).
  Unset, a TLS install whose `server_url` names a host uses that exact host as
  `rp_id` and its https address as the origin; any other install uses
  `localhost` and the console's own address on localhost. `rp_id` is a host
  name, never an IP address; each origin is https, or http on localhost, on
  `rp_id` or a subdomain of it. A passkey is bound to its `rp_id` for life:
  changing it, or the `server_url` it was derived from, strands every passkey
  enrolled under the old one, and `seamlessd doctor` and the daemon's log name
  the ones it strands. File-only.

A client of such a daemon names the socket as its `server_url`:
`unix:///absolute/path.sock`. It needs no `mcp.api_key`, and its hooks and MCP
registration go through the `seam` CLI (`install-hooks` does that by itself for
a socket URL).

The block is new after v0.7.4; releases before it refuse config keys they do
not know, so the example file leaves it commented out.

### Access requests at a confidential fence {#isolation-requests}

The `isolation:` block is what [project isolation](/concepts/project-isolation/)
does beyond each project's own state. It holds one key, and it is off by
default; off is exactly the behavior of every earlier release.

- `requests` (`SEAMLESS_ISOLATION_REQUESTS`, default `false`) lets a confidential
  or sealed project's fence ask instead of only refusing, and makes the fence
  judge the agent's **conversation** - its client session without `peer_auth`,
  its process tree under it - instead of the session a connection is bound to.
  [Asking for access](/concepts/project-isolation/#asking-for-access) has the
  whole model. Turning it on changes these, and nothing else:
  - **One home, one rule.** A conversation has one home, the project its own
    session is in, and a label: every confidential or sealed project whose
    content it may hold. Every write must be cleared for everything it holds:
    once the label holds anything, it writes only into the projects your
    approvals cleared, never an open project or the global scope.
  - **Refusals file requests.** A read or write refused at a fenced project,
    and `session_start` into one, file what the home and the project call for:
    a **move** into the project for good (from an open home, or into a sealed
    project), or a **grant** of it (between confidential projects, read-only
    unless you choose read and write). Nothing is ever granted to or from a
    sealed project. A locked project's refusal still files the lock's request.
  - **Bindings stop at the home.** For a Claude Code or Codex agent,
    `session_start` binds nothing outside what its home may read and write, and
    `session=` acts as a session outside the home only there.
  - **Approvals.** You decide each request with `seam approve`, on the
    console's Access requests page or, in a client that shows MCP forms, in
    the agent's in-session dialog. Without `peer_auth` that decision is
    advisory, and `access_request` is registered so the agent can ask ahead;
    under it, a grant or a move takes the same proofs as a lease, and one into
    a locked project opens its lock too.
  - **The record follows the home.** The SessionEnd harvest, Codex `Stop`
    findings, plan and subagent captures, prompt recall and a resume's
    briefing go to and read from the home; after a move, the project it moved
    into.
  - **Provenance stamps** on what a conversation writes while it holds another
    fenced project's content (frontmatter `may_contain` on memories and
    notes), which the gardener's stale-copy and copy-relocate passes read.
  - **Receipts**: `tasks_update` gains `reference`, and a moved conversation
    may close a task it claimed before the move.
  - **The console** shows a conversation's moves, grants and writes on its
    sessions' pages, and a **Came from** filter on a project's page.

  A grant lasts until the conversation's session ends, 1 hour or 4 hours,
  capped by `peer_auth.max_lease` (`8h` when unset); a move lasts for good.

Without `peer_auth` the approval is **advisory**: `seam approve` and the
console's Approve commit it with the bearer key, and so does Approve in the
agent's in-session dialog, so any process that holds the key can approve a
request, its own included. Under `peer_auth` a grant or a move is bound to the
asking agent's process tree, as a locked project's lease is, and needs the same
proofs: `seam approve` in your terminal (through `sudo` to the daemon's
account), a passkey, or a Touch ID key, which approves no move. Locks work
without this switch; grants and moves do not.

The block is new after v0.7.4; releases before it refuse config keys they do
not know, so the example file leaves it commented out.

### Feeding a security monitor {#security-monitor}

`screening.feed_socket` and `screening.feed_spool` deliver a metadata feed of
events to a Unix socket, and the `commits:` block records the commits agents
made and writes a commit-matching snapshot for a monitor to read. Both are off
by default, and off is exactly the earlier behavior.
[Feeding a security monitor](/guides/security-monitor/) has each contract and
how to turn each on.

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
