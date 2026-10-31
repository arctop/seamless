---
title: Seal projects to OS accounts
description: peer_auth - one daemon, a Unix socket that names each caller by its uid, projects only their own OS account can reach, and the deployment that makes the files hold too.
---

[Project isolation](/concepts/project-isolation/) keeps agents working on one
project out of another's knowledge. It is a policy fence: it trusts the session
binding an agent reports, every caller shares one bearer key, and the corpus is
files in your home directory that any process of yours can read.

**peer_auth** is the boundary underneath it, for when the threat is an *account*
rather than a stray agent: you run a sensitive system's agents under their own
macOS or Linux user, and nothing running as you - no agent, no script, no tool -
may read that system's memory, through Seamless or through a file. One daemon
serves everyone. It identifies each caller by the uid the kernel reports for its
Unix-socket connection, never by anything the caller sends, and it maps OS
accounts to the projects they may reach.

It is off by default, and off means exactly the behavior every release before it
had. It also asks for a different deployment: the daemon has to run as an account
of its own, or the files would not hold.

## How it decides

Every request carries a **principal**, set by the transport before any route
runs:

| Connection | Principal |
|---|---|
| TCP with the bearer key | the **owner** account (`peer_auth.owner`) |
| The socket, from the owner's uid | the owner |
| The socket, from a uid listed under `peer_auth.users` | that account |
| The socket, from any other uid | refused with `403` before any route runs |

Two rules follow, and they are the whole policy:

1. **A project sealed to an account is reachable only from that account's socket
   connections.** The owner - TCP key and socket alike - never reaches it.
2. **A configured account reaches only its own projects** (the ones sealed to it
   plus any it shares), and it **reads** the global scope but never **writes** it:
   a global memory is injected into every project's briefing, the owner's
   included.

| | A project sealed to this account | Sealed to another account | The owner's projects | Global, read | Global, write |
|---|---|---|---|---|---|
| Owner | - | no | yes | yes | yes |
| Configured account | yes | no | only if listed under `projects` | yes | no |

"Reach" means everything: recall, briefings, the prompt matcher, every MCP tool
by name or by id, tasks, trials, sessions, the gardener's proposals, the hooks,
A2A, the console and its live feed. A project an account may not reach is simply
absent from what it lists, and a call that names one is refused with the rule and
the remedy, never with any of its content.

A configured account works in its own projects whatever its working directory:
its sessions, and any call nothing else places, land in its **home** project - its
first sealed project, else its first shared one. It never grows the repo map. The
owner opening a repository whose project is sealed to another account gets a
one-line notice in place of a briefing, and no session.

The console belongs to the owner: a configured account gets `403`, and the
owner's console shows no project sealed to another account - not in a list, a
count's detail, a search, a chart, the event log or the live feed.

Sealing works with isolation rather than replacing it: every sealed project must
also be `confidential` or `sealed`, which keeps it standalone (no family, no
parent) and inside the existing fences between agents.

## Configure it

```yaml
peer_auth:
  enabled: true
  owner: alice                      # the account the TCP bearer key stands for
  socket:
    path: /opt/seamless/run/seamlessd.sock
    mode: "0666"                    # who may knock; the kernel still names each one
  users:
    _payroll:                       # an OS user name, or a numeric uid
      sealed: [payroll]             # reachable only from _payroll
    _ops_bot:
      projects: [ops]               # shared: _ops_bot and the owner both reach it
```

Checked when the file loads. The `users` map always, whether or not `enabled`
is set: a project sealed to two accounts, a project sealed to one account and
listed for another, a project listed twice for one account, the global scope or
the reserved `all` listed, and an account reaching nothing. With `enabled` set,
also: the owner is named and not listed under `users`, the socket path is
absolute and outside `data_dir`, and the mode is octal and keeps owner read and
write.

Checked when `serve` starts, and refused if any fails:

- every account resolves on this machine (macOS role accounts live in the
  directory service, and are found there), and no two entries share a uid;
- every project the block names exists - a misspelled seal would leave the
  project it meant open, which is exactly what this feature exists to prevent;
- every sealed project is `confidential` or `sealed`;
- the files would hold too (the next section).

`seamlessd doctor`, run as the daemon's account against the daemon's config,
prints the policy as it resolved and every reason `serve` would refuse.

## Deploy it: the daemon runs as its own account

The data directory holds every project's memory in plain files and a SQLite
database. If the daemon runs as you, every process of yours reads them. So with
`peer_auth` on, `serve` refuses to start unless:

- it runs as neither the owner nor root;
- `data_dir` belongs to the daemon's account and is mode `0700`;
- the socket's directory belongs to the daemon's account or root and is not group-
  or world-writable (whoever can write it can replace the socket and impersonate
  the daemon to every caller);
- every directory above the data, the socket, the config file and the binary
  belongs to root or the daemon's account and is not writable by anyone else -
  a directory you own anywhere above the data is one you can rename it out of;
- the config file and the `seamlessd` binary belong to root or the daemon's
  account and are not group- or world-writable: whoever can rewrite either can
  switch the boundary off at the next start.

A layout that satisfies all of it (macOS shown; Linux is the same with
`useradd --system` and a systemd unit):

| Path | Owner | Mode | Holds |
|---|---|---|---|
| `/opt/seamless` | `root:wheel` | `0755` | |
| `/opt/seamless/bin` | `root:wheel` | `0755` | `seamlessd`, `seam` (both `0755`, root's) |
| `/opt/seamless/etc` | `root:wheel` | `0755` | `seamless.yaml` (`root:_seamless`, `0640`) |
| `/opt/seamless/data` | `_seamless` | `0700` | the database and corpus |
| `/opt/seamless/run` | `_seamless` | `0755` | the socket |
| `/opt/seamless/home` | `_seamless` | `0700` | the account's home (caches, nothing else) |

Create the account (macOS, as an administrator; pick an unused uid in the role
range - `dscl . -list /Users UniqueID` lists the taken ones):

```bash
sudo dscl . -create /Groups/_seamless PrimaryGroupID 480
sudo dscl . -create /Users/_seamless UniqueID 480
sudo dscl . -create /Users/_seamless PrimaryGroupID 480
sudo dscl . -create /Users/_seamless UserShell /usr/bin/false
sudo dscl . -create /Users/_seamless NFSHomeDirectory /opt/seamless/home
sudo dscl . -create /Users/_seamless Password '*'
sudo dscl . -create /Users/_seamless IsHidden 1
```

The service is a LaunchDaemon that names the account (on Linux, a system unit
with `User=`), so it runs whether or not anyone is logged in. The plist, its
controls, and how to retire the per-user service first are in
[the service reference](/reference/service/#peer-auth-daemon).

## Connect the owner

Nothing changes for the owner's agents. They keep the TCP listener and the bearer
key, and they are the owner: every project except the sealed ones. Their config
becomes a client's, because the owner's account no longer runs the daemon - and a
server-role config there would have `seamlessd doctor` open (and create) a
database in the owner's home:

```yaml
role: client
server_url: "http://127.0.0.1:8081"
mcp:
  api_key: "<the same key as the daemon's config>"
```

The owner can use the socket instead (`server_url:
unix:///opt/seamless/run/seamlessd.sock`, no key): same principal, no key on disk.
The console still needs TCP - a browser cannot dial a socket.

## Connect a sealed account's agents

An agent for a sealed system runs **as that system's account**. That is also how
a person works on the system with its memory: they run the session as the
account, with `sudo -u`, and their own account's sessions cannot reach it.

The account's config, in its own home (`~/.config/seamless/seamless.yaml`, mode
`0600`):

```yaml
role: client
server_url: "unix:///opt/seamless/run/seamlessd.sock"
```

No key: the daemon names the caller by its uid. Then install its hooks and MCP
registration, as the account:

```bash
sudo -u _payroll -H /opt/seamless/bin/seamlessd install-hooks --client claude \
  --seam /opt/seamless/bin/seam
```

With a socket `server_url` every hook is a `seam hook <event>` command (Claude
Code's own http hook cannot dial a socket) and the MCP registration is the `seam
mcp-proxy` stdio bridge. If you write them by hand, the account's
`~/.claude/settings.json` hook entries look like:

```json
{"type": "command", "command": "/opt/seamless/bin/seam",
 "args": ["hook", "session-start", "--config", "/home/of/_payroll/.config/seamless/seamless.yaml"]}
```

one per event (session-start, user-prompt-submit, session-end, post-tool-use,
subagent-start, subagent-stop, permission-request), and its `~/.claude.json`
MCP server is:

```json
"seamless": {"type": "stdio", "command": "/opt/seamless/bin/seam",
             "args": ["mcp-proxy", "--config", "/home/of/_payroll/.config/seamless/seamless.yaml"]}
```

Then run the session as the account: `sudo -u _payroll -H claude`. The account
needs its own Claude Code sign-in, and its own copy of anything the session
reads - it cannot traverse your home directory, which is the point.

## Verify

```bash
# As root or the daemon's account: the policy, and nothing serve would refuse.
sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd doctor

# As the owner: the files are out of reach, and so is the sealed project.
ls /opt/seamless/data                         # Permission denied
seam recall something --project payroll      # refused: not reachable from this connection

# As the sealed account: its own project, and the global scope.
sudo -u _payroll -H /opt/seamless/bin/seam doctor
sudo -u _payroll -H /opt/seamless/bin/seam recall something

# As any other account: refused at the door.
curl --unix-socket /opt/seamless/run/seamlessd.sock http://localhost/healthz   # 403
```

## Move an existing install over

The data directory moves as a whole; the database stores paths relative to it,
so nothing inside needs rewriting. On one volume the move is a rename.

1. **Seal first, in the old world.** Make each sealed project `confidential` or
   `sealed` (`seam project isolation <slug> confidential --yes`) and run
   `seamlessd doctor` with the new config to catch a misspelled slug.
2. **Back up**, as the owner, while the old daemon still serves:
   `seamlessd export -o ~/seamless-before-peer-auth.tar.gz`. The archive holds
   every project, the sealed ones included - keep it where the sealed accounts'
   data would be kept.
3. **Create the account and the layout** above, and install root-owned copies of
   both binaries in `/opt/seamless/bin`.
4. **Write the daemon's config** at `/opt/seamless/etc/seamless.yaml`
   (`root:_seamless 0640`): the owner's old config with `data_dir:
   /opt/seamless/data`, the `peer_auth` block, and the same `mcp.api_key` and LLM
   keys.
5. **Stop the old service and move the data**: stop and remove the per-user
   service ([how](/reference/service/#peer-auth-daemon)), then
   `sudo mv ~/.seamless /opt/seamless/data`,
   `sudo chown -R _seamless:_seamless /opt/seamless/data` and
   `sudo chmod -R go-rwx /opt/seamless/data`.
6. **Start the daemon**: install and load
   [the LaunchDaemon](/reference/service/#peer-auth-daemon), then
   `curl -s http://127.0.0.1:8081/healthz` and the doctor line above.
7. **Switch the owner's config** to the client config above.
8. **Wire each sealed account** as described above, and verify.

### Roll back

Unload the LaunchDaemon ([how](/reference/service/#peer-auth-daemon)), move the
data back (`sudo mv /opt/seamless/data ~/.seamless` and `sudo chown -R
$(id -un):staff ~/.seamless`), restore the owner's server config without the
`peer_auth` block - a release that predates it refuses the key - and reinstall
the per-user service (`make install`, or the installer). The export from step 2
is the fallback if anything in the move went wrong.

## What changes with peer_auth on

- **The daemon reads no agent files.** Session-end findings and token totals
  harvested from transcripts, model sniffing, and plan-file and subagent capture
  are off: under peer_auth every path a hook names is one some other account
  chose, and the daemon can read its own data directory, so honoring one could
  make it read a sealed project's files on a caller's behalf. Agents still
  record findings with `session_end`. (The daemon could not read your
  transcripts anyway: it no longer runs as you.)
- **Updates are an administrator's step.** A LaunchDaemon install is not one the
  installer made, so the daemon only notifies; replace `/opt/seamless/bin` with
  `sudo` and restart it. Run `make install` only with a client-role config, so it
  updates your own `seam` and hooks and leaves the service alone.
- **Counts stay machine-wide in `usage_summary`**, as they are for isolation:
  volume, never content, and the same number for every caller.
- **The boundary is the OS account.** Root, and anyone who can become the
  daemon's account or a sealed account, reads everything; passwordless `sudo`
  voids the whole arrangement. Whoever holds the bearer key acts as the owner
  (which reaches no seal). Backups of the data directory, and exports, hold
  every project - treat them as the most sealed thing in them.
- **The gardener still runs over every project**, as the daemon, for no caller.
  Its proposals about a sealed project reach only that account's agents
  (`gardener_proposals`); the owner's console does not show them.
