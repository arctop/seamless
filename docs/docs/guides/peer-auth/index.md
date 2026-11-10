# Lock projects behind your approval

> peer_auth - the daemon in its own OS account, a Unix socket that names each caller by its uid, and locked projects your own agents reach only with an approval you prove.

[Project isolation](https://thereisnospoon.org/docs/concepts/project-isolation/) keeps agents working on one
project out of another's knowledge. It is a policy fence: it trusts the session
binding an agent reports, every caller shares one bearer key, and the corpus is
files in your home directory that any process of yours can read.

**peer_auth** is the boundary underneath it, for when the threat is your own
account: an agent of yours reading a project it should not, through Seamless or
through a file. One daemon serves everyone, running as an OS account of its
own. It identifies each caller by the uid the kernel reports for its
Unix-socket connection, never by anything the caller sends.

On that boundary you can **lock** a project. A locked project stays closed to
every agent of yours until you approve one agent's access, for a limited time,
with a proof the agent cannot produce: Touch ID, a passkey, or your own `sudo`.
The approval covers that agent and the programs it started, and nothing else.

It is off by default, and off means exactly the behavior every release before
it had. It asks for a different deployment (the daemon needs an account of its
own, or the files would not hold), and it runs on macOS and Linux only: Windows
has no Unix-socket peer credentials Seamless can read.

## Two settings: isolation and the lock

Isolation says what flows in and out of a project. The lock says whether even
its own agents need a fresh approval from you before reaching it. They are two
settings, with two words: "sealed" is an isolation state, and the lock is never
called that.

| | Isolation | The lock |
|---|---|---|
| Question | What flows in and out of this project? | Must even this project's own agents get your approval first? |
| Values | `open`, `confidential`, `sealed` | locked (`peer_auth.locked`) or not |
| Set | on the console's project Overview, or with `seam project isolation` | in the daemon's config file |
| Needs peer_auth | no | yes |

- **`confidential`** is for sensitive work your agents should still do freely:
  nothing leaves the project, and its sessions still read open projects and the
  global scope. It needs no extra deployment.
- **`sealed`** is a clean room: nothing in or out.
- **Locked** is for what no agent should touch without you saying so now:
  money, legal matters, credentials. Reaching one takes your approval, session
  by session.

Sensitive projects are normally `confidential` and locked: once you approve an
agent, its session still reads your open projects and the global scope. `serve`
refuses a locked project that is `open`, because the isolation fence is what
keeps it out of the family, parent and cross-project gardener surfaces the lock
does not reach on its own.

## How it decides

Every request carries a **principal**, set by the transport before any route
runs:

| Connection | Principal |
|---|---|
| TCP with the bearer key | the **owner** account (`peer_auth.owner`) |
| The socket, from the owner's uid | the owner, with its kernel-reported process chain |
| The socket, from a uid listed under `peer_auth.restricted` | that [restricted account](#restricted-local-accounts) |
| The socket, from any other uid | refused with `403` before any route runs |

For the owner, the rule is simple. Every project that is not locked behaves
exactly as it does without peer_auth, isolation fences included. A locked
project is closed to every owner request: its name, description, isolation and
item counts show, nothing inside it does, and writes into it are refused,
unless the request rides a socket connection whose process holds a live
[lease](#leases-and-the-process-tree). A TCP request never holds one, because a
shared key cannot be tied to one process.

"Closed" means every surface: recall, briefings, the prompt matcher, every MCP
tool by name or by id, tasks, trials, sessions, the gardener's proposals, the
hooks, A2A, the console and its live feed. A call that names a locked project is
refused with the rule and the remedy, never with any of its content. A `recall`
that names no project answers from the rest of its scope and says
`<project> (locked) may hold results`; `project_list` lists the project with the
way in.

## Configure it

```yaml
peer_auth:
  enabled: true
  owner: alice                      # the account the TCP bearer key stands for
  socket:
    path: /opt/seamless/run/seamlessd.sock
    mode: "0666"                    # who may knock; the kernel still names each one
  locked: [example-finance]         # each must be confidential or sealed
  max_lease: 8h                     # optional: caps every approval, at most 24h
```

Every key is in [the configuration reference](https://thereisnospoon.org/docs/reference/configuration/#peer-auth).

Checked when the file loads, whether or not `enabled` is set: `locked` names
real projects, never the global scope, and none twice; `restricted` is listed
only with `enabled` set, and never names the owner or the same account twice;
`max_lease` and `passkeys` are well formed. With `enabled` set, also: the owner
is named, the socket path is absolute and outside `data_dir`, and the mode is
octal and keeps owner read and write.

Checked when `serve` starts, and refused if any fails:

- every account resolves on this machine (macOS role accounts live in the
  directory service, and are found there), and no two entries share a uid;
- every project the block names exists - a misspelled lock would leave the
  project it meant open, which is exactly what this feature exists to prevent;
- every locked project is `confidential` or `sealed`;
- the files would hold too (the next section).

`seamlessd doctor`, run as the daemon's account against the daemon's config,
prints the policy as it resolved and every reason `serve` would refuse.

**Coming from `peer_auth.users`.** A config with a non-empty `users` map is
refused when it loads, even while peer_auth is disabled, with:

```text
config: peer_auth.users was the 2026-10-31 design, which sealed projects to other OS accounts, and is no longer supported: list the projects under peer_auth.locked and remove users
```

That map reserved projects for other OS accounts, and you could never reach them.
It is gone. List those projects under `peer_auth.locked`, keep each one
`confidential` or `sealed`, delete `users`, and restart. An account that was
listed there is then refused at the socket unless you list it under
`peer_auth.restricted`, which reaches only open projects that are not locked.

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

A layout that satisfies all of it:

| Path | Owner | Mode | Holds |
|---|---|---|---|
| `/opt/seamless` | root | `0755` | |
| `/opt/seamless/bin` | root | `0755` | `seamlessd`, `seam` (both `0755`, root's) |
| `/opt/seamless/etc` | root | `0755` | `seamless.yaml` (`root:_seamless`, `0640`) |
| `/opt/seamless/data` | `_seamless` | `0700` | the database and corpus |
| `/opt/seamless/run` | `_seamless` | `0755` | the socket |
| `/opt/seamless/home` | `_seamless` | `0700` | the account's home (caches, nothing else) |

Create the account. On macOS, as an administrator, pick an unused uid in the
role range (`dscl . -list /Users UniqueID` lists the taken ones):

```bash
sudo dscl . -create /Groups/_seamless PrimaryGroupID 480
sudo dscl . -create /Users/_seamless UniqueID 480
sudo dscl . -create /Users/_seamless PrimaryGroupID 480
sudo dscl . -create /Users/_seamless UserShell /usr/bin/false
sudo dscl . -create /Users/_seamless NFSHomeDirectory /opt/seamless/home
sudo dscl . -create /Users/_seamless Password '*'
sudo dscl . -create /Users/_seamless IsHidden 1
```

On Linux:

```bash
sudo useradd --system --user-group --shell /usr/sbin/nologin --home-dir /opt/seamless/home _seamless
```

Then the layout (`root`'s group is `wheel` on macOS, `root` on Linux). Leave
out `/opt/seamless/data` when you are [moving an existing install](#move-an-existing-install-over),
whose data directory arrives there as a whole:

```bash
sudo install -d -o root -g wheel -m 0755 /opt/seamless /opt/seamless/bin /opt/seamless/etc
sudo install -d -o _seamless -g _seamless -m 0700 /opt/seamless/home /opt/seamless/data
sudo install -d -o _seamless -g _seamless -m 0755 /opt/seamless/run
```

The service runs as that account whether or not anyone is logged in. On macOS
it is a LaunchDaemon: the plist, its controls, and how to retire the per-user
service first are in [the service reference](https://thereisnospoon.org/docs/reference/service/#peer-auth-daemon).
On Linux it is a system unit, `/etc/systemd/system/seamless-daemon.service`:

```ini
[Unit]
Description=Seamless daemon (peer_auth)
After=network.target

[Service]
User=_seamless
Group=_seamless
UMask=0077
Environment=SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml
Environment=HOME=/opt/seamless/home
ExecStart=/opt/seamless/bin/seamlessd serve
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now seamless-daemon
```

Retire any per-user service first, or it starts again at your next login
([how](https://thereisnospoon.org/docs/reference/service/#peer-auth-daemon)).

## Connect the owner

Your agents connect through the socket: a locked project opens only to a process
the kernel names, which a TCP connection never is. Your config becomes a
client's, because your account no longer runs the daemon - and a server-role
config there would have `seamlessd doctor` open (and create) a database in your
home:

```yaml
role: client
server_url: "unix:///opt/seamless/run/seamlessd.sock"
```

No key: the daemon names the caller by its uid. Then install the hooks and MCP
registration:

```bash
seamlessd install-hooks --client claude    # or codex, or all
```

With a socket `server_url` every hook is a `seam hook <event>` command (Claude
Code's own http hook cannot dial a socket), and the MCP registration is the
`seam mcp-proxy` stdio bridge. Under peer_auth the
[daemon reads no agent files](#what-changes-with-peer-auth-on), so `seam hook`
reads them and sends what it found.

The console still needs TCP and the bearer key: a browser cannot dial a socket,
and `seamlessd console-open` refuses a socket `server_url` with instructions.
Keep the daemon's key in a second config of your own, used only for the console,
and name it when you open the console:

```yaml
# ~/.config/seamless/console.yaml, mode 0600
role: client
server_url: "http://127.0.0.1:8081"
mcp:
  api_key: "<the same key as the daemon's config>"
```

```bash
SEAMLESS_CONFIG=~/.config/seamless/console.yaml seamlessd console-open
```

On a daemon that locks a project, `console-open` signs in on the passkey origin
the daemon names (`http://localhost:<port>` by default) instead of `127.0.0.1`:
the console's cookie belongs to one host, and browsers refuse passkeys on an IP
address.

If your agents would write scripts over the data directory, tell them to use the
tools or `seam ... --json | jq` instead:
[integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/#optional-tell-your-agent-where-to-read-from)
has the line to paste. Under peer_auth the directory is out of their reach
either way.

## Enroll an approver

An approver credential is what signs your approval: a passkey, or a Touch ID
key. None is required: the terminal path below always works, and
`seamlessd doctor` warns only that it is the one path left. Enrolling needs the
daemon running with a project locked (or read grants on), so do it after the
[cutover](#move-an-existing-install-over).

A first credential needs a one-time enrollment code, and only the daemon's
account can mint one; no route, over the socket or with the TCP key, can. A code
works once, for 10 minutes, and only its hash is stored. It is minted for one key,
named by its fingerprint, so a code someone else reads cannot enroll a different
key. `seamlessd approvers`
also lists credentials and removes them, and `reset --yes` drops every credential
and revokes every live lease
([reference](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_approvers)).

**Passkeys** work with hardware keys, phones and password managers. Open
`/console/approvers` on the passkey origin in your browser and create the passkey.
The page then shows the command that mints a code for it:

```bash
sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml \
  /opt/seamless/bin/seamlessd approvers enroll-code passkey --key XXXX-XXXX-XXXX-XXXX
```

Run it in your own terminal, check that the key it prints is the one on the page,
and enter the code there. The origin is `http://localhost:<port>` unless `peer_auth.passkeys`
says otherwise, and browsers refuse passkeys on `127.0.0.1`. A later passkey can
be approved by one already enrolled instead of a code. Each passkey needs its own
authenticator: the browser refuses one that already holds a passkey enrolled here,
and devices that share synced passkeys, such as a phone on the same iCloud
Keychain, count as one. A passkey is bound to its `rp_id` for life: changing
`rp_id` later strands every passkey enrolled under the old one.

**Touch ID on a Mac** (optional) goes through Seamless Approve, a small macOS app
that approves a request with Touch ID, an Apple Watch or the login password. A
default install neither contains nor launches it, and Linux and Windows never
see it. In your own terminal, with `server_url` naming the socket:

```bash
seam touchid enroll
```

It installs your `seam`'s own release of the app
(`seamless-approve_<version>_darwin.zip`, checked against the release's signed
`checksums.txt` and against Apple's signature) at
`~/Applications/Seamless Approve.app` and opens it. Its window shows the new key's
fingerprint and the command that mints a code for that key, to run in another
terminal. Details, including how the app is checked and
how a Mac re-enrolls, are in the
[CLI reference](https://thereisnospoon.org/docs/reference/cli-seam/#seam_touchid_enroll).

The app takes nothing from its command line but the socket and the request. It
reads the request's facts and challenge from the daemon over the socket, refuses
a socket served by your own account or by any account but the one the daemon
names, and shows only the daemon's facts in the Touch ID prompt: the grant first,
then the first three warnings. The agent's own reason is left out of that prompt
on purpose. Its key is made in the Mac's Secure Enclave and kept in the data
protection keychain under an access group only the signed app can claim, so
another program running as you cannot sign with it, even behind a prompt of its
own.

To build the app yourself you need macOS and Xcode's command-line tools.
`make approver-test` runs its unit tests (no keychain, no prompt); `make approver`
builds `bin/approver/Seamless Approve.app`, ad-hoc signed, and that build has no
key store and says so. A usable build needs an Apple Developer account, an
explicit macOS App ID `com.arctop.seamless.approve`, and a Developer ID
provisioning profile for it whose keychain group covers the app's:

```bash
APPROVER_SIGN_IDENTITY=<identity hash> APPROVER_TEAM_ID=<team id> APPROVER_PROFILE=<file> make approver
seam touchid enroll --app "bin/approver/Seamless Approve.app"
```

`scripts/approver/notarize.sh` notarizes and staples a Developer ID build
(`APPROVER_NOTARY_KEY`, `APPROVER_NOTARY_KEY_ID`, `APPROVER_NOTARY_ISSUER_ID`). A
source or prerelease `seam` has no release of the app to fetch: pass `--app` with
a downloaded zip or a local build.

## Requests

When an agent reaches a locked project without a lease, Seamless files an access
request, and the refusal says so. Asking needs no privilege, and the daemon, not
the agent, supplies the facts you will see.

- **A call refused at the lock** files one by itself, on the socket: a tool call
  that names the project, or that resolves its scope or session through a session
  recorded in it.
- **A session starting in the project's repository** is recorded there, its name,
  cwd and model only, and the SessionStart hook files the request and answers with
  the [locked notice](https://thereisnospoon.org/docs/reference/hooks/) instead of a briefing. The project's
  briefing arrives with the first Seamless tool result after the approval, once
  per lease, even across a daemon restart.
- **The agent can ask ahead**, with the `access_request` tool (present only on a
  daemon that locks a project) or `seam access request <project> --reason "..."`
  from its own shell. The reason is at most 200 characters, and you see it quoted
  and labeled as the agent's own words.

`access_request`, and `seam access request --json`, answer with `filed` (true
when this call filed a new request, false when it found one waiting or the agent
already holds the access) and `reason` (`added`, `kept` or `none`). The rest of
the behavior is the same for every path:

- A request waits 15 minutes for your decision. One request waits per agent and
  project: asking again finds it, and an explicit reason fills in a waiting
  request's missing one.
- After you deny one, the agent's next refused call says you denied it, and when,
  and files nothing: a refused call never asks you again behind your back. Its
  explicit request always files a new one, which is how it asks once you have
  told it what to ask for.
- An agent that already holds the access it asks for is answered with it, and
  nothing is filed, until 30 minutes before it ends. From then on an explicit
  request files a **refresh**. A refresh carries the reason it was given; given
  none it shows "(no reason given)", and a later explicit reason fills that blank.
  Approving a refresh replaces the lease.

**Requests name agents, not shells.** A lease covers a process and everything it
started, so a lease on a tmux server would open the project to every program in
every pane, other agent sessions included. Seamless therefore files a request
only for an agent: Claude Code, Codex, the Claude app's per-server wrapper, a
process with one of those above it in the kernel-verified chain, or the JavaScript
runtime a live session's hook stamped (Claude Code installed with npm runs as
`node`). A process with no agent above it - `seam` run by hand in a tmux pane, a
script from a login shell - is refused with the rule and this remedy: run the
work from an agent session, which asks for itself. A shell, a multiplexer, a
terminal or an editor that hosts terminals, a login process, the system's
process manager, or any other application's own executable is never named by a
request and never approved, on any path; deny one an older daemon filed. A
session starting in a locked repository under such a process is not recorded at
all.

**What you are shown.** Every approval surface shows the daemon's facts - the
project, its isolation and the access asked for, the agent the daemon names, its
pid and uid, the session and model, the working directory, whether this is a
first request or a refresh, and until when it waits - with the agent's reason
quoted and labeled as its own. Warnings come first, most serious first:

1. the asking process is a broad process, on a request an older daemon filed (an
   approval will be refused);
2. for a read grant, the project's content may be copied into the asking
   project's memories;
3. the session read text from outside (a web page, a file it was handed), so its
   reason may not be its own;
4. the same agent is also asking for another project;
5. the session works outside the project's repository;
6. no session names the agent, so its cwd and model are unknown;
7. the agent already holds a lease on the project, and approving replaces it.

The facts carry a digest. When any of them changes before you decide (a reason
filled in, a new mark on the session, another request from the same agent), the
approval is refused and you look again.

## Approve: three paths, one core

Every path ends in the same check: the facts you saw still hold, the proof is
valid, and the lease is written once, bound to the request. Answering a dialog is
never the approval, because any process of your account, or a client hook, could
answer one.

| Path | The proof | Where |
|---|---|---|
| The dialog in the agent's session | Touch ID, or a passkey on the request's page | the agent's client, on the socket |
| `seam approve` | `sudo -k` to the daemon's account, every time | your own terminal |
| The passkey page | a passkey | the request's console page, in your browser |

**The dialog.** When the agent's client declared MCP elicitation on the socket
and a proof can finish in the dialog, a refused call (or `access_request`) shows
you the request's facts, warnings first, in the client itself: Claude Code shows
it, and so do Codex builds that declare form elicitation. You choose a duration
and Touch ID or a passkey. Touch ID runs the Seamless Approve app, which reads the
facts from the daemon and signs over the request, the facts' digest, the access
and the duration. A passkey opens the request's page for the duration you chose,
and it signs the duration and the facts' digest too, so a tampered duration fails
verification. Where a passkey is the only way the dialog can finish, the dialog
is that page.

The call waits up to 110 seconds from its start, then answers with the request
still pending for the paths below. Declining or dismissing denies nothing, and an
agent's connection cannot deny at all: the request waits for you. Cancelling the
Touch ID prompt, or the helper closing or being killed after it was seen, ends the
wait at once, the request still pending. There is at most one dialog per request
per connection, and it survives a daemon restart: the daemon keeps a connection's
declared dialog modes, for clients on the socket only and only while a lock or
read grants are configured, until the connection has gone unused for 30 days. An
agent connected before the release that keeps them shows the plain refusal until
its next initialize.

**`seam approve`.** In your own terminal (it refuses without one, so an agent
cannot use it), `seam approve` lists what is waiting and shows the facts, asks for
a duration, and commits the decision with `sudo -k -u <daemon account> seamlessd
approve`. The `sudo -k` asks for your Touch ID or password every time, and
becoming the daemon's account is the proof. Typing `deny` in full at the
duration question, or `d` at the confirmation, denies. `seam approve --watch`
prints each new request and raises a notification. Every flag and message is in
[the CLI reference](https://thereisnospoon.org/docs/reference/cli-seam/#seam_approve).

**The passkey page.** `/console/access/<id>` shows the request's facts; choose a
duration and sign with an enrolled passkey (`seam approve --passkey` opens it, and
`--for` chooses the duration, which the page then shows read-only). The page
denies without a passkey. `/console/access` lists every waiting request, every
live lease with **Revoke**, and every console unlock with **Lock again**.

**Which paths a refusal offers, and when.** Every remedy offers only the paths this
daemon has for this call:

| Path | Offered when |
|---|---|
| `seam approve <id>` in your terminal | always |
| Touch ID in the approval dialog | a Touch ID key is enrolled, and the client declared form elicitation on the socket. The SessionStart notice runs before MCP connects, so it offers Touch ID to Claude Code, and to Codex with "if this Codex shows one" |
| A passkey at the request's page | passkeys are set up and at least one is enrolled |

A call over the TCP key, whose bearer key can never hold a lease, is told to work
from an agent session on the socket instead, and no request is filed. Once a dialog
ends with the request still pending, the remedy says how it ended and which paths
are left.

The durations are: until the session ends (the default, at most
`peer_auth.max_lease`), 1 hour, or 4 hours, each capped by `max_lease`.

## Leases and the process tree

An approval is a **lease** on one agent process. It covers that process and every
process it started: the MCP bridge, `seam` commands, the hooks and the agent's own
scripts share one approval, and nothing outside the tree does.

The daemon learns the tree from the kernel alone. At accept it reads which process
holds the socket connection (the audit token on macOS, which names the last process
to use it, and `SO_PEERCRED` and a pidfd on Linux) and pins that pid to the process, so a later walk can tell it from one
that reused the pid. Per request it walks the parent chain from there, verifying
every link. A link that does not verify ends the chain, so an orphan reparented to
init, or a reused pid, reaches nothing above it. Nothing the client sends, the
self-reported agent-process header included, enters the chain. Only the owner's
socket connections can hold a lease; a TCP request never can.

A lease ends at its expiry, when its session ends (`session_end`, or the client's
own session-end hook), when you Revoke it on `/console/access`, when you run
`seamlessd approvers reset --yes`, or when the database is restored from a
snapshot (a rollback or `seamlessd import`). Nothing extends one silently. From 30
minutes before it ends, every tool result and hook injection says when it ends and
how to ask again.

A console unlock is separate. A browser opens a locked project with
`/console/unlock/<project>` and a passkey, or with
`sudo -k -u _seamless seamlessd console-open --unlock <project>` from the terminal,
for at most three hours (less when `max_lease` is shorter). It is a cookie in that
one browser, never a lease, and it never opens the project to an agent. A locked
plan or session page in the console takes you to that unlock page.

## Read grants and the advisory tier

[Read grants](https://thereisnospoon.org/docs/concepts/project-isolation/#asking-for-access)
(`isolation.requests: true`) turn a read refused at a confidential project's fence
into a request. A grant is read-only, one project, for a limited time, and never
involves a `sealed` project on either side.

Under peer_auth a grant is bound to the asking agent's process tree, as a lease is,
and takes the same proofs: `seam approve`, a passkey or a Touch ID key. Without
peer_auth the approval is **advisory**. `seam approve` asks for a y/N in a terminal
and commits over the console with the bearer key, and the console's Approve does
the same, so any process that holds the key can approve a request, its own
included. The holder of an advisory grant is also the session a call speaks for,
which is self-reported: an agent that rebinds with `session_start`, or names another
session with `session=`, writes where that session may, past the grant's write
confinement. Under peer_auth the holder is the process tree, and the same moves are
refused. Use the advisory tier to keep an agent from wandering into a confidential
project, not against one set on it.

Neither tier stops an agent running as you from reading a `confidential` or
`sealed` project. A session's project is the agent's word, so `session_start
project=<slug>`, a hook naming that repository, or `session=` on a tool reads the
project with no grant at all, and the console answers your account for every
project that is not locked. The fence keeps a careful agent out. The lock holds
against one set on it, because it asks the kernel who is calling, so lock a
project that must stay shut. A restricted account has none of these ways in.

A grant's write confinement binds the asking agent's process tree. A process that
detaches from that tree, or a call over TCP with the bearer key, is not confined.

A2A speaks for no session, so under peer_auth it refuses a `confidential` or
`sealed` project unless the caller holds a lease on it.

## Restricted local accounts

A **restricted local account** is a second OS account on the machine whose
agents work beside yours: a separate login for unattended agents, say, so that
nothing they run can read your home directory. List it under
`peer_auth.restricted`, by user name or numeric uid:

```yaml
peer_auth:
  enabled: true
  owner: alice
  socket:
    path: /opt/seamless/run/seamlessd.sock
  locked: [example-finance]
  restricted: [_example_agent]
```

What such an account reaches:

- **Every open project that is not locked**, reading and writing. A project
  whose isolation is `confidential` or `sealed`, and every project under
  `peer_auth.locked`, is out of its reach and is not listed for it. Its refusal
  reads the same for those projects and for a slug nobody registered, so it
  never learns which closed projects exist.
- **The global scope, read-only.** Its briefing, recall and reads include
  global memories and notes; every global write is refused.
- **No new projects.** A write or a session naming a project that does not
  exist is refused rather than creating it. You create the projects and map
  the account's clones of your repositories to them, as the daemon's account:

  ```bash
  sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml \
    /opt/seamless/bin/seamlessd map-repo --path /Users/_example_agent/repos/example-app --project example-app
  ```

  A session started anywhere else - an unmapped directory, a closed project's
  repository - is not recorded.
- **No approvals.** It files no access requests, gets no read grants and holds
  no leases, so nothing it does can open a locked or confidential project.

It is known only through the socket. The kernel names it by its uid, exactly as
it names you; it never holds the bearer key, and a TCP request is always yours.
Its config lives in its own home (`~/.config/seamless/seamless.yaml`, mode `0600`):

```yaml
role: client
server_url: "unix:///opt/seamless/run/seamlessd.sock"
```

No key. Then install its hooks and MCP bridge as the account, and run the session
as the account (`sudo -u _example_agent -H claude`). The account needs its own
Claude Code sign-in and its own copy of anything the session reads:

```bash
sudo -u _example_agent -H /opt/seamless/bin/seamlessd install-hooks --client claude \
  --seam /opt/seamless/bin/seam
```

Keep the socket outside every home directory: the account cannot traverse yours.

**Everything it writes is labeled.** Memories, notes, tasks and their updates,
plan steps, sessions and their findings, and trials record the account that
wrote them, and every surface shows it: "[written by account _example_agent]"
at the end of briefing, prompt recall (`<seam-recall>`), A2A and
`seam task list` lines, an `account` field in tool results, and a "written by
account _example_agent" chip on console rows. Your own writes look exactly as
they always have.

**Binding rules are proposals.** A constraint, convention, protocol or stage
steers every session's briefing, so one from a restricted account does not land
on its own. When the account writes one, or edits, appends to, supersedes,
overwrites or deletes one (yours or its own), the write waits on the console's
Held screen as a proposal, "proposed by account _example_agent", and nothing in
the corpus changes. It is in no briefing or recall, the account's own included,
until you promote it:

```bash
seam proposals              # pending proposals, newest first
seam promote <id>           # land it as written, labeled with the account
seam proposals discard <id> # drop it
```

Promoting is yours alone, on the console or in your terminal; the account can
neither promote a proposal nor undo a promotion. Every other kind of memory,
and notes, tasks and findings, lands at once, labeled.

Three more rules keep its text from passing as yours. It cannot give a project
memory the name of a live global constraint, convention, protocol or stage, because
a read by that name would then answer with its text. It cannot ask the gardener to
plan a change (`gardener_request`, `gardener_split`), and it still sees proposals
with `gardener_proposals`. A memory it rewrites loses its star.

Sessions belong to the account that started them: a restricted account's agents
cannot resume, bind to or write findings into your sessions, and yours cannot
act in theirs. Its sessions also have names of their own: every one is stored
as `_example_agent:<name>` (`_example_agent:cc/...` for its Claude Code
sessions), and every session name its agents give is read inside that
namespace. A name you already use, in any project, is simply free for it, so
the account cannot learn which session names exist where it cannot look. The
console and `seam` show its sessions by the qualified name, and that is the name
you use for them. You cannot start a session of your own under a name beginning
`_example_agent:`.

## Verify

```bash
# As root or the daemon's account: the policy, and nothing serve would refuse.
sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd doctor

# As the owner: the files are out of reach, and the lock holds.
ls /opt/seamless/data                               # Permission denied
seam recall something --project example-finance     # refused at the lock

# As any other account: refused at the door.
curl --unix-socket /opt/seamless/run/seamlessd.sock http://localhost/healthz   # 403
```

Then start an agent in the locked project's repository: its session starts with
the locked notice, and `seam approve` in your own terminal shows its request.

## What changes with peer_auth on

- **The daemon reads no agent files; your hooks do.** Under peer_auth every
  path a hook names was chosen by another account, and the daemon can read its
  own data directory, so it opens none of them. When `server_url` names the
  peer_auth socket, `seam hook` (running as you, beside the agent) reads the
  transcript, the plan file and the subagent transcript itself and sends what
  it found: session-end findings and token totals, the model, plan and
  subagent capture, and the git head. The daemon treats this as your agent's
  claim and applies the usual rules: in a locked project it lands only once
  you approve a lease for that agent. Until then the hook says in one line on
  stderr (and, for a plan, in the agent's context) that the capture was not
  recorded; nothing is retried, and the next capture after the approval
  carries the current state. Agents on the TCP key make none of these
  captures; point their `server_url` at the socket. Agents can still record
  findings with `session_end`.
- **Updates are an administrator's step.** A LaunchDaemon or system-unit install
  is not one the installer made, so the daemon only notifies; replace
  `/opt/seamless/bin` with `sudo` and restart it. Run `make install` only with a
  client-role config, so it updates your own `seam` and hooks and leaves the
  service alone.
- **Counts stay machine-wide in `usage_summary`**, as they are for isolation:
  volume, never content, and the same number for every caller.
- **The gardener still runs over every project**, as the daemon, for no caller.
  Its proposals about a locked project reach a client only through a lease, and
  the console shows them only in a browser that has unlocked the project.

## Limits

- **The transcript holds what a session read.** Once you approve an agent, its
  project's content is in that session's transcript and any tool result it kept.
  A lease ending does not unread it.
- **An approval is only as strong as its proof.** `seam approve` proves itself
  only through `sudo -k`, so a passwordless or `seamlessd`-only sudoers rule voids
  it. `seam approve` warns when `sudo -n` shows it needs no authentication, which
  a rule scoped to `seamlessd` alone does not trigger. Touch ID and passkeys are
  as strong as your device and the user verification they require.
- **A debugger running as you can act inside an approval.** It can attach to any
  process in the agent's tree. macOS limits this with the hardened runtime and SIP,
  and Linux with yama's `ptrace_scope` (1 or higher); neither stops root.
- **The boundary is the OS account.** Root, and anyone who can become the daemon's
  account, reads everything; a passwordless `sudo` to it voids the whole
  arrangement. Whoever holds the bearer key acts as the owner, and reaches every
  project but the locked ones. Backups of the data directory, and exports, hold
  every project: keep them as carefully as the most sensitive project in them.
- **Anything running as you can watch your terminal.** A `console-open --unlock`
  link is shown once, and whoever uses it first gets what it opens, so make it in a
  terminal your agents cannot read: a `tmux` server they share lets them capture
  the pane. An enrollment code enrolls only the key it was minted for, so being
  read does not give it away, but your clipboard is reachable too: check that the
  key the command prints is the one on your screen.
- **Your shell is yours to guard.** Under peer_auth `seam approve` works only over
  the socket, and refuses one that the daemon's account does not serve, so a changed
  `server_url` cannot show it false facts. An agent running as you can still edit your shell's startup files, or
  replace a `seam` you own. Run the root-owned `/opt/seamless/bin/seam approve` by
  its full path if you doubt yours.
- **Some decisions refuse a process an agent started**: bringing a fence down,
  releasing or discarding a held write, clearing a session's mark, and changing
  which features are on. Make them in your own terminal or the console. This stops
  an agent's ordinary tool calls, not one that detaches a process from its tree or
  reads the key in your `console.yaml`.

## Move an existing install over

The data directory moves as a whole; the database stores paths relative to it,
so nothing inside needs rewriting. On one volume the move is a rename.

1. **Fence first, in the old world.** Make each project you will lock
   `confidential` or `sealed` (`seam project isolation <slug> confidential --yes`).
2. **Back up**, as the owner, while the old daemon still serves:
   `seamlessd export -o ~/seamless-before-peer-auth.tar.gz`. The archive holds
   every project, the locked ones included: keep it as carefully as they are.
3. **Create the account and the layout** above, leaving out `/opt/seamless/data`,
   and install root-owned copies of both binaries in `/opt/seamless/bin`
   (`sudo install -o root -m 0755 <seamlessd> <seam> /opt/seamless/bin/`). Keep a
   copy of the current `mcp.api_key` for the console.
4. **Write the daemon's config** at `/opt/seamless/etc/seamless.yaml`
   (`root:_seamless 0640`): your old config with `data_dir: /opt/seamless/data`,
   the `peer_auth` block listing the projects to lock, and the same `mcp.api_key`
   and LLM keys. Check the slugs against your project list now: a misspelled
   entry is refused at startup, and `doctor` names it.
5. **Stop the old service and move the data**: stop and remove the per-user
   service ([how](https://thereisnospoon.org/docs/reference/service/#peer-auth-daemon)), then
   `sudo mv ~/.seamless /opt/seamless/data`,
   `sudo chown -R _seamless:_seamless /opt/seamless/data` and
   `sudo chmod -R go-rwx /opt/seamless/data`.
6. **Start the daemon**: install and load
   [the LaunchDaemon](https://thereisnospoon.org/docs/reference/service/#peer-auth-daemon) or the system unit
   above, then `curl -s http://127.0.0.1:8081/healthz` and the doctor line in
   [Verify](#verify).
7. **Switch your own config to the socket** and reinstall the hooks and MCP
   registration ([Connect the owner](#connect-the-owner)). This is the step that
   puts your agents on `seam mcp-proxy`.
8. **Enroll an approver** ([how](#enroll-an-approver)). Until you do, the terminal
   path still approves.
9. **Verify** ([above](#verify)), then start an agent in a locked repository and
   approve its request end to end.
10. **Put the export out of your agents' reach.** The archive from step 2 sits in
    your home, readable by anything that runs as you, with every locked project in
    it. Move it now (`sudo mv ~/seamless-before-peer-auth.tar.gz /opt/seamless/` and
    `sudo chown root` and `sudo chmod 600` the file there),
    and delete it once the new install has proved itself.
11. **Optional: list a restricted account.** Add it to `peer_auth.restricted` and
    restart the daemon. Give the account its own client config (socket
    `server_url`, no key), run `install-hooks` as the account, and map each of its
    clones with `seamlessd map-repo --path <its clone> --project <slug>` as the
    daemon's account ([details](#restricted-local-accounts)).

### Roll back

Unload the LaunchDaemon or disable the system unit
([how](https://thereisnospoon.org/docs/reference/service/#peer-auth-daemon)), move the data back
(`sudo mv /opt/seamless/data ~/.seamless` and `sudo chown -R $(id -un) ~/.seamless`),
restore your server config without the `peer_auth` block - a release that predates
it refuses the key - and reinstall the per-user service (`make install`, or the
installer). The export from step 2, wherever step 10 put it, is the fallback if
anything in the move went wrong.
