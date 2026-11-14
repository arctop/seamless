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
has no Unix-socket peer credentials Seamless can read. A Windows computer can
still be a Client of a daemon with peer_auth on, as a key client: it never runs
the relay, and its agents never open a locked project
([Windows](https://thereisnospoon.org/docs/guides/network-install/#windows-is-always-a-key-client)).

## Two settings: isolation and the lock

Isolation says what flows in and out of a project. The lock says whether even
its own agents need a fresh approval from you before reaching it. They are two
settings, with two words: "sealed" is an isolation state, and the lock is never
called that.

| | Isolation | The lock |
|---|---|---|
| Question | What flows in and out of this project? | Must even this project's own agents get your approval first? |
| Values | `open`, `confidential`, `sealed` | locked or not |
| Set | on the console's project Overview, or with `seam project isolation` | on the console's project Overview with a passkey, or forced in the daemon's config file |
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

### Lock from the console, or force lock in the file

There are two ways to lock a project, and they differ in who can undo them.

**From the console.** On a confidential or sealed project, open the **Isolation**
card on its Overview and choose **Lock this project**. The change needs a passkey
you have already [enrolled](#enroll-an-approver); until you have one, the card
links to the Approvers page instead. Taking the lock off takes a passkey too, from
the same page or the project's unlock page. Each approval names one project and
one direction, so it cannot be replayed for anything else. Turning a lock on ends
every approval the project still had (agents' leases, read grants and browser
unlocks), so each agent and browser asks again. Seamless applies the
change by restarting, which briefly drops the console and agent connections.

**Force lock, in the file.** A project listed under `peer_auth.force_locked` is
force locked: the console shows it as force locked and cannot change it, even
with a passkey. Only editing the file and restarting removes it. Use this for
a project that should stay locked however the console is reached.

The console removes only the locks it set. A project in both places stays locked
until it leaves the file. While a project is locked, by either route, its
isolation cannot be set to `open`: remove the lock first.

Console lock changes are passkey-only for now. A Touch ID key enrolled with
`seam touchid enroll` approves agents' requests but cannot approve these; a
passkey saved on your Mac still asks for your fingerprint in the browser.

The key was called `peer_auth.locked` in earlier builds. That name still works,
and `seamlessd doctor` warns until you rename it.

## How it decides

Every request carries a **principal**, set by the transport before any route
runs:

| Connection | Principal |
|---|---|
| TCP with the bearer key | the **owner** account (`peer_auth.owner`) |
| The socket, from the owner's uid | the owner, with its kernel-reported process chain |
| The socket, from a uid listed under `peer_auth.restricted` | that [restricted account](#restricted-local-accounts) |
| The socket, from any other uid | refused with `403` before any route runs |
| TCP through a paired relay `peer_auth.relays` maps, from the owner's uid there | the owner, with the process chain the relay's kernel reported ([relays](#agents-on-another-computer-relays)) |
| TCP through such a relay, from a uid its `restricted` map names | that restricted account, labeled `<name>@<relay>` |
| TCP through such a relay, from any other uid | refused with `403` before any route runs |
| TCP through a paired relay `peer_auth.relays` does not map | refused with `403` before any route runs (map it, then restart) |
| TCP through a key device | the owner, as the bearer key is |

For the owner, the rule is simple. Every project that is not locked behaves
exactly as it does without peer_auth, isolation fences included. A locked
project is closed to every owner request: its name, description, isolation and
item counts show, nothing inside it does, and writes into it are refused,
unless the request rides a socket connection whose process holds a live
[lease](#leases-and-the-process-tree). A TCP request never holds one, because a
shared key cannot be tied to one process; a relay you map is judged as this
socket is ([relays](#agents-on-another-computer-relays)).

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
  force_locked: [example-finance]   # only this file can remove these; each must be confidential or sealed
  max_lease: 8h                     # optional: caps every approval, at most 24h
```

Every key is in [the configuration reference](https://thereisnospoon.org/docs/reference/configuration/#peer-auth).

Checked when the file loads, whether or not `enabled` is set: `force_locked` names
real projects, never the global scope, and none twice; `restricted` is listed
only with `enabled` set, and never names the owner or the same account twice;
`max_lease` and `passkeys` are well formed. With `enabled` set, also: the owner
is named, the socket path is absolute and outside `data_dir`, and the mode is
octal and keeps owner read and write. `relays` is listed only with `enabled` set; each key is a relay's name as the Devices page shows it, and each `owner_uid` is a uid other than 0.

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
config: peer_auth.users was the 2026-10-31 design, which sealed projects to other OS accounts, and is no longer supported: list the projects under peer_auth.force_locked and remove users
```

That map reserved projects for other OS accounts, and you could never reach them.
It is gone. List those projects under `peer_auth.force_locked`, keep each one
`confidential` or `sealed`, delete `users`, and restart. An account that was
listed there is then refused at the socket unless you list it under
`peer_auth.restricted`, which reaches only open projects that are not locked.

## Deploy it: the daemon runs as its own account

[`seamlessd setup`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup) does not turn
locked projects on yet: on a computer that runs the daemon it offers **Turn on
locked projects (peer_auth)** and points to
[the move below](#move-an-existing-install-over), which uses this section.
These are the steps, by hand.

The data directory holds every project's memory in plain files and a SQLite
database. If the daemon runs as you, every process of yours reads them. So with
`peer_auth` on, `serve` refuses to start unless:

- it runs as neither the owner nor root, and as the account the pin below
  names (with no pin, as a system account);
- `data_dir` belongs to the daemon's account and is mode `0700`;
- the socket's directory belongs to the daemon's account or root and is not group-
  or world-writable (whoever can write it can replace the socket and impersonate
  the daemon to every caller);
- every directory above the data, the socket, the config file and the binary
  belongs to root or the daemon's account and is not writable by anyone else -
  a directory you own anywhere above the data is one you can rename it out of;
- the config file and the `seamlessd` binary belong to root or the daemon's
  account and are not group- or world-writable: whoever can rewrite either can
  switch the boundary off at the next start;
- with TLS on, `tls.cert_file` and `tls.key_file` are held to the same rule, and
  no other account can read the key (its group may, only when that group is the
  daemon's own): whoever reads the key can serve the daemon's own name with its
  certificate, on another port or address.

A layout that satisfies all of it:

| Path | Owner | Mode | Holds |
|---|---|---|---|
| `/opt/seamless` | root | `0755` | |
| `/opt/seamless/bin` | `_seamless` | `0755` | `seamlessd`, `seam` (both `0755`, `_seamless`'s) |
| `/opt/seamless/etc` | root | `0755` | `seamless.yaml` (`root:_seamless`, `0640`), `daemon-account` (root's, `0644`), optionally `release_signers` (root's, `0644`; [owner-signed releases](#owner-signed-releases)) |
| `/opt/seamless/etc/tls` | `root:_seamless` | `0750` | with TLS on: the certificate (`0644`) and its key (`root:_seamless`, `0640`) |
| `/opt/seamless/data` | `_seamless` | `0700` | the database and corpus |
| `/opt/seamless/run` | `_seamless` | `0755` | the socket |
| `/opt/seamless/home` | `_seamless` | `0700` | the account's home (caches, nothing else) |
| `/opt/seamless/client` | root | `0755` | optionally, root's own `seam` (`0755`) for your hooks ([why](#client-copy)) |

The daemon's account holds `/opt/seamless/bin` so that the daemon can
[update itself](#updates); your account can write neither binary. Keep the
directory and both binaries root's instead to put them out of the daemon's
reach too, and it only tells you about a new release.

Create the account as a system account: on macOS a uid below 500, on Linux one
below `UID_MIN` in `/etc/login.defs` (1000 unless the file says otherwise). The
commands below make one. Until the account is pinned (after the layout),
`seam approve`, `seam touchid enroll`, the Touch ID approver and `serve` accept
any system account as the daemon's, and no other.

On macOS, as an administrator, pick an unused uid in the role range, below 500
(`dscl . -list /Users UniqueID` lists the taken ones):

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
sudo install -d -o root -g wheel -m 0755 /opt/seamless /opt/seamless/etc
sudo install -d -o _seamless -g _seamless -m 0700 /opt/seamless/home /opt/seamless/data
sudo install -d -o _seamless -g _seamless -m 0755 /opt/seamless/run /opt/seamless/bin
```

Then put a release's `seamlessd` and `seam` there, as the daemon's (a build
from source runs too, but never updates itself):

```bash
sudo install -o _seamless -g _seamless -m 0755 <seamlessd> <seam> /opt/seamless/bin/
```

A daemon that serves TLS keeps its certificate and key there too, out of your
account's reach: the daemon's account cannot read a key in your home, and every
process of yours can.

```bash
sudo install -d -o root -g _seamless -m 0750 /opt/seamless/etc/tls
```

Then pin the account: `seam approve`, `seam touchid enroll`, the Touch ID
approver and `serve` accept only the account this file names as the daemon's,
so no other account - a restricted one, another service's - can pass for it.
`sudo tee` writes it as root's:

```bash
echo "_seamless $(id -u _seamless)" | sudo tee /opt/seamless/etc/daemon-account >/dev/null
sudo chmod 0644 /opt/seamless/etc/daemon-account
```

It is one line, the account's name and uid (`_seamless 480`). A pin that is not
root's, that another account could rewrite (it or any directory above it), or
that is not that one line makes them refuse every daemon until root rewrites
it; `seamlessd doctor` says which.

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
sudo loginctl enable-linger _seamless
sudo systemctl daemon-reload
sudo systemctl enable --now seamless-daemon
```

Lingering keeps the account's own systemd user manager running with no one
logged in: the daemon's updater runs as a unit of it ([why](#updates)). Retire
any per-user service first, or it starts again at your next login
([how](https://thereisnospoon.org/docs/reference/service/#peer-auth-daemon)).

## Connect the owner

Your agents connect through the socket: a locked project opens only to a process
the kernel names, which a TCP connection never is. Your config becomes a
client's, because your account no longer runs the daemon - and a server-role
config there would have `seamlessd doctor` report on a database in your home
rather than the daemon's:

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
`seam mcp-proxy` stdio bridge. They run the `seam` beside the `seamlessd` that
installed them, a copy of your own; add `--seam /opt/seamless/bin/seam` to run
the daemon's instead, which no process of yours can replace and which moves
with the daemon when it [updates itself](#updates), or `--seam
/opt/seamless/client/seam` for [root's copy](#client-copy), which the daemon's
account cannot replace either. Under peer_auth the
[daemon reads no agent files](#what-changes-with-peer-auth-on), so `seam hook`
reads them and sends what it found.

An agent on another computer connects the same way, through the socket of that
computer's relay, at the same path
([how a Client pairs](https://thereisnospoon.org/docs/guides/network-install/#step-3-pair-a-client)). A
Windows computer, or one without a relay, is a key client and never opens a
locked project ([relays](#agents-on-another-computer-relays)).

The console still needs TCP: a browser cannot dial a socket, and
`seamlessd console-open` refuses a socket `server_url` with instructions. Keep
a second config of your own, used only for the console, that names the
daemon's TCP address and holds its key, and name it when you open the console:

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
the daemon names ([which one](#the-passkey-origin)) instead of `127.0.0.1`:
the console's cookie belongs to one host, and browsers refuse passkeys on an IP
address. On a TLS install, `server_url` in `console.yaml` is the daemon's https
address, as your other clients use it.

With `console.sign_in: passkey` in the daemon's config, a browser signs in with
a passkey instead, on this computer or any other, and the key signs no browser
in: `console-open` opens the sign-in page and writes nothing
([sign in with a passkey](https://thereisnospoon.org/docs/reference/console/#sign-in-with-a-passkey)).

The key in `console.yaml` is readable by every process of yours. To retire it,
run `seamlessd setup` and choose **Sign in with passkeys and retire the key**
([the steps](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup_retire)): the console
then takes only passkeys, TCP serves the console's sign-in, its pages and
`/healthz`, and MCP is the socket's alone. With every passkey lost, the sign-in
page's **Show a code** and `console-open --sign-in --code`, run as the daemon's
account, sign that one browser in.

If your agents would write scripts over the data directory, tell them to use the
tools or `seam ... --json | jq` instead:
[integrate your agent](https://thereisnospoon.org/docs/guides/integrate-your-agent/#optional-tell-your-agent-where-to-read-from)
has the line to paste. Under peer_auth the directory is out of their reach
either way.

## Enroll an approver

An approver credential is what signs your approval: a passkey, or a Touch ID
key. None is required: the terminal path below always works, and
`seamlessd doctor` warns only that it is the one path left. Enrolling needs the
daemon running under `peer_auth`, so do it after the
[cutover](#move-an-existing-install-over). A lock set from the console needs a
passkey enrolled first.

Every credential needs a one-time enrollment code, and only the daemon's
account can mint one; no route, over the socket or with the TCP key, can. A code
works once, for 10 minutes, and only its hash is stored. It is minted for one key,
named by its fingerprint, so a code someone else reads cannot enroll a different
key. A passkey you already enrolled never vouches for a new one: an enrolled
passkey can take a console lock off, so one touch you were tricked into must not
be enough to add a key an agent holds. `seamlessd approvers`
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
and enter the code there. Browsers refuse passkeys on `127.0.0.1`, so open the
page on [the passkey origin](#the-passkey-origin). Each passkey needs its own
authenticator: the browser refuses one that already holds a passkey enrolled here,
and devices that share synced passkeys, such as a phone on the same iCloud
Keychain, count as one.

**With console sign-in by passkey** a passkey is added from the device that will
hold it. `seamlessd approvers add`, run as the daemon's account, prints the plain
address; the device creates the passkey and shows a code.
`sudo -k -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd approvers approve --code 7KQ2-M9XD`
shows that passkey and asks before adding it.

Only the code crosses, and it names a waiting passkey without carrying a key, so
a code read off your screen cannot add another. Under peer_auth every new passkey
needs this approval, because an approver passkey now also signs in to the
console.

A passkey added while peer_auth was off was approved as your own account, which
every agent shares. When you turn peer_auth on, check `seamlessd approvers list`
and remove what you do not recognize, or `reset --yes` and add them again.

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

`scripts/approver/notarize.sh` notarizes and staples a Developer ID build, with
a notarytool keychain profile (`APPROVER_NOTARY_PROFILE`) or an App Store
Connect API key (`APPROVER_NOTARY_KEY`, `APPROVER_NOTARY_KEY_ID`,
`APPROVER_NOTARY_ISSUER_ID`). A
source or prerelease `seam` has no release of the app to fetch: pass `--app` with
a downloaded zip or a local build.

### The passkey origin

A passkey belongs to one relying party, named by its `rp_id`, and the browser
creates or uses it only on that relying party's origins. Unless
`peer_auth.passkeys` names them, the daemon picks:

- **A TLS install whose `server_url` names a host** (a tailnet name, your own
  domain) uses that exact host as `rp_id` and `https://<host>:<port>` as the
  origin, the address your clients already use. Its parent domain is never
  used, so no other host under it can run a ceremony for these passkeys.
- **Any other install** uses `localhost` and `http://localhost:<port>` (https
  with TLS on).

`seamlessd doctor` and the daemon's log at startup say which it is.

A passkey is bound to its `rp_id` for life. Changing `rp_id`, or the
`server_url` it came from, or turning TLS off, strands every passkey enrolled
under the old one. The daemon records each passkey's `rp_id`, so doctor and the
log name the ones a change strands. Passkeys enrolled before it recorded that
are listed as unknown.

A page on the origin can run passkey ceremonies with any passkey enrolled for it.
That is why doctor checks two things about every origin:

- **The daemon listens on every address its host resolves to, on its port.** A
  tailnet name can resolve to an IPv4 and an IPv6 address. An address of the
  name the daemon does not hold is one another process running as you can
  listen on and serve the same origin from, and a browser that picks that
  address runs its ceremonies there. So an IPv4 `addr` brings its interface's
  IPv6 addresses along: the tailnet's `fd7a:...` address beside its `100.x`
  one, `[::1]` beside `127.0.0.1`. Doctor's `bind` row lists them, and
  `bind.addresses` dials each to confirm this daemon answers there. Under
  peer_auth `serve` refuses to start while another process holds any of them,
  and the service starts it again once the address is free. It never
  pairs a private address with a global IPv6 one, which would widen a LAN bind
  to the internet. An address the pairing misses, such as one on another
  interface, still needs the wildcard (`addr: "0.0.0.0:8081"` holds every
  address of every interface, so the bearer key and TLS are then all that
  guard it on your LAN), or a name that resolves only to addresses the daemon
  holds.
- **The certificate names the host.** Under peer_auth only root and the daemon's
  account read the TLS key, which is what keeps a process of yours from serving
  the origin with a valid certificate. That holds only if your account cannot get
  a certificate for the name by itself: a tailnet whose cert command you may run
  (`tailscale cert`) hands any process of yours one. Then binding every address is
  what stands between them and the origin.

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
quoted and labeled as its own. A grant or a move (with
[access requests](#read-grants-and-the-advisory-tier) on) also states what it
carries: every fenced project the agent would hold once approved, and every
project it could write to. Warnings come first, most serious first:

1. the asking process is a broad process, on a request an older daemon filed (an
   approval will be refused);
2. the asking process is a JavaScript runtime (Claude Code installed with npm
   runs as `node`) that only its own hook calls an agent, so everything it
   started would share the approval;
3. the agent runs on another computer, named, whose
   [relay](#agents-on-another-computer-relays), not this daemon, reports the
   pid, the uid and the program;
4. for a move, that the agent works in the project from now on, what it may
   write there, and that it can no longer write where it worked before;
5. for a grant, where the project's content may be copied: the asking project
   first, then every other project the agent may write to;
6. the session read text from outside (a web page, a file it was handed), so its
   reason may not be its own;
7. the same agent is also asking for another project;
8. the session works outside the repository of the project it asks for (for a
   grant or a move, of the project it works in);
9. no session names the agent, so its cwd and model are unknown;
10. the agent already holds a lease on the project, and approving replaces it.

The facts carry a digest. When any of them changes before you decide (a reason
filled in, a new mark on the session, another request from the same agent), the
approval is refused and you look again.

## Approve: three paths, one core

Every path ends in the same check: the facts you saw still hold, the proof is
valid, and the lease is written once, bound to the request (with access requests
on, the grant or the move). Answering a dialog is never the approval, because any
process of your account, or a client hook, could answer one.

| Path | The proof | Where |
|---|---|---|
| The dialog in the agent's session | Touch ID, or a passkey on the request's page | the agent's client, on the socket |
| `seam approve` | `sudo -k` to the daemon's account, every time | your own terminal, on the daemon's computer |
| The passkey page | a passkey | the request's console page, in your browser on any computer |

The passkey page works from a browser on any computer that reaches the console
on [the passkey origin](#the-passkey-origin), and the daemon verifies the
ceremony wherever it ran. `seam approve` runs only on the daemon's own computer,
in a terminal there or over SSH to it: becoming the daemon's account is its
proof, and that account exists only there.

**The dialog.** When the agent's client declared MCP elicitation on the socket
and a proof can finish in the dialog, a refused call (or `access_request`) shows
you the request's facts, warnings first, in the client itself: Claude Code shows
it, and so do Codex builds that declare form elicitation. You choose a duration
and Touch ID or a passkey. Touch ID runs the Seamless Approve app, which reads the
facts from the daemon and signs over the request, the facts' digest, the access
and the duration. A passkey opens the request's page for the duration you chose,
and it signs the duration and the facts' digest too, so a tampered duration fails
verification. Where a passkey is the only way the dialog can finish, the dialog
is that page. A move's dialog never offers Touch ID, and a grant's approves it
read-only ([below](#read-grants-and-the-advisory-tier)).

The call waits up to 110 seconds from its start, then answers with the request
still pending for the paths below. Declining or dismissing denies nothing, and an
agent's connection cannot deny at all: the request waits for you. Canceling the
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
socket connections can hold a lease; a TCP request never can. Through a relay you map, they can ([relays](#agents-on-another-computer-relays)).

A lease ends at its expiry, when its session ends (`session_end`, or the client's
own session-end hook), when you Revoke it on `/console/access`, when you run
`seamlessd approvers reset --yes`, or when the database is restored from a
snapshot (a rollback or `seamlessd import`). Nothing extends one silently. From 30
minutes before it ends, every tool result and hook injection says when it ends and
how to ask again.

A console unlock is separate. A browser opens a locked project from its unlock
page, `/console/unlock/<project>`, for at most three hours (less when `max_lease`
is shorter), in one of two ways. **Unlock with a passkey** asks your
authenticator there. **Approve from a terminal** shows a code and the command
that approves it, for example:

```bash
sudo -k -u _seamless seamlessd console-open --unlock example-finance --code 7KQ2-M9XD
```

Run it in your own terminal. `sudo -k` asks for your password or Touch ID, and the
page updates by itself once the code is approved. The unlock opens the browser
that showed the code and no other, so the code is not a secret: reading it off
your screen or terminal gives nothing away. A code works once, within ten
minutes, and `--for 1h` makes the unlock shorter. Approve only a code your own
browser shows: one you were handed would unlock someone else's browser.

Either way the unlock lives in that one browser, never as a lease, and it never
opens the project to an agent. It is a cookie plus a key that the browser keeps
for the console's own address. A browser sends its cookies to every port of the
same host, so the cookie alone opens nothing. Before an unlock begins, the page
creates the key and installs a small service worker that signs each console
request with it. The browser lets only the console's own address use the key,
and the daemon counts the unlock only on a request the key signed. A browser that
cannot run the service worker can't hold an unlock, and the page says so. That
covers some private windows, and any page that is neither https nor localhost.
A locked plan or session page in the console takes you to that unlock page.

### Agents on another computer: relays {#agents-on-another-computer-relays}

On a server that serves other computers, a macOS or Linux client reaches the
daemon through its relay, `seamlessd relay`
([what it is](https://thereisnospoon.org/docs/concepts/networked/#the-relay),
[how to install one](https://thereisnospoon.org/docs/guides/network-install/#install-a-relay)). It runs as its
own system account on that computer and serves the socket path agents use
here. It reads each caller's uid and process chain from its own kernel and
forwards the request over TLS with its client certificate.

The server trusts what a relay forwards only under peer_auth, and only for a
relay you map, in the daemon's config, followed by a restart:

```yaml
peer_auth:
  relays:
    example-laptop:       # the relay's name, as the Devices page shows it
      owner_uid: 501      # your uid on that computer (id -u there)
      restricted:         # optional: restricted accounts there, name to uid
        _example_agent: 502
```

`seamlessd doctor`, run as the daemon's account, checks each mapping: a name no
paired relay holds, or a relay no passkey approved, is a **warn**, and a
mapping that holds names the restricted accounts there.

Through a mapped relay, your uid there is you, judged as this socket judges
you: with the chain the relay reported, and the leases that chain holds. A
lease is keyed on the relay and the process. It opens the project to that
agent's tree on that computer and to nothing else: not a sibling agent there,
not the same pid here, not another relay.

Any other uid gets 403, and a request forwarding no caller gets 400. A relay
paired with the code alone, while peer_auth was off, is refused until it pairs
again and you approve it with a passkey. A relay you do not map is refused,
whatever uid it forwards: every account on that computer can reach its socket,
and none is known to be you until you map it. A key client is you over TCP, as
the bearer key is, and never holds a lease, and a Windows computer is always
one: it never runs the relay, and its agents never open a locked project.

A second account on that computer can be a
[restricted account](#restricted-local-accounts), as on this one: map it under
the relay, by its name and uid there (`restricted` above). It reaches the open
projects that are not locked, reads the global scope, and files no requests.
Its writes carry the label `_example_agent@example-laptop`, the account and the
host, and its constraints, conventions, protocols and stages wait on the Held
screen for you. Root and your own uid there are never restricted, and the relay
never forwards its own account.

A relay also names the program of every process it forwards, so the server
files an agent's request through it as it would here: never for a shell,
terminal or multiplexer, only for an agent program or a process an agent
started. Every approval path - your terminal, a passkey, Touch ID - shows a
`relay` warning naming the computer whose pid, uid and program they are. A
relay from before program names files nothing: update Seamless on that
computer.

Passkeys approve from any browser. The Touch ID helper on a client Mac enrolls
and approves through its relay, and the server verifies its signature.
`seamlessd approve` stays a server command, as does every terminal path:
`seam approve`, a console unlock's code and `seamlessd approvers` run on this
computer, in a terminal here or over SSH. Your own decisions (a pairing code,
revoking, denying, approving without Touch ID or a passkey, held writes,
features and backups) are refused through a relay or a device key, with or
without peer_auth: make them in the console of the server you sign in to, or in
a terminal on the server.

Revoking a relay ends its leases, read grants and waiting requests at once. A
console lock turned back on, and a restore, end a relay's leases as they end
this machine's.

## Read grants and the advisory tier

[Access requests](https://thereisnospoon.org/docs/concepts/project-isolation/#asking-for-access)
(`isolation.requests: true`) turn a call refused at a confidential or sealed
project's fence into a request. What is asked follows from where the agent's
conversation works, never from the agent's choice: from an open project, a
**move** into the project for good; between confidential projects, a **grant**,
read-only unless you choose read and write; into a sealed project, a move, from
anywhere. Nothing ever grants a sealed project.

### Under peer_auth

- **The conversation is the agent's process tree.** A grant or a move is bound
  to the asking agent's process tree, as a lease is, and everything the
  conversation holds belongs to that tree for the life of the process, `/clear`
  included. Its home is the project of the agent's own session, believed only
  when that session is stamped with a process of the tree.
- **The same proofs as a lease**: `seam approve` in your terminal (through
  `sudo` to the daemon's account), a passkey, or a Touch ID key. Touch ID
  approves a grant, never a move: Seamless Approve words every request as
  access for a while, which a move is not, so a move's dialog is its passkey
  page or nothing, and its remedies name only your terminal and that page. The
  dialog approves a grant read-only; read and write is your choice in your
  terminal (`seam approve <id> --access read-write`) or on the request's passkey
  page.
- **A locked target's move or grant is also its lease.** A call refused at the
  lock still files the lock's request, and that lease opens the project only to
  the conversation that works in it - an agent started in the project's
  repository. An agent working elsewhere asks with `access_request` and
  `write=true` (or `seam access request <project> --write`): that files the move
  from an open project, or the read-and-write grant from a confidential one,
  and approving it opens the lock too, in the same approval, for the duration
  you choose. After a move into a locked project, the agent's first tool result
  says until when the lock is open and carries the project's briefing, once.

What a conversation holds binds the asking agent's process tree. A process that
detaches from that tree, or a call over TCP with the bearer key, holds nothing
and is not confined.

### Without peer_auth: the advisory tier

Without peer_auth the approval is **advisory**. `seam approve` asks for a y/N in
a terminal and commits over the console with the bearer key, and the console's
Approve does the same, so any process that holds the key can approve a request,
its own included.

In a client that shows MCP forms (Claude Code does, and so do Codex builds that
declare form elicitation), a refusal that files a grant or a move,
`session_start` into a fenced project, and `access_request` - which this tier
registers too - open a dialog in the agent's session, and **Approve or Deny
there decides the request**, with no other proof. It is advisory like the rest:
Seamless cannot tell who answers a dialog, any more than who holds the key, so
the form says so, and the record names the dialog (`in-session-dialog`). The
form shows the facts, warnings first, what the request carries, and what
read-only and read and write each let happen; a grant asks for the access
(read-only by default) and how long, and a move only for the decision.
Declining, canceling, or no answer within 110 seconds decides nothing: the
request stays pending for your terminal, the console, or the next dialog. The
daemon keeps the client's declared dialog modes across a restart.

The holder of an advisory grant or move is the conversation the agent's process
identity names, which is self-reported; a client that sends none is held by the
session a call speaks for, so one that rebinds with `session_start`, or names
another session with `session=`, writes where that session may. Under peer_auth
the holder is the process tree, and those moves change nothing it holds. Use
the advisory tier to keep an agent from wandering into a confidential project,
not against one set on it.

### What the switch changes about the binding limit

Without access requests, only a lock stops an agent running as you from
reading a `confidential` or `sealed` project, with peer_auth or without it. A
session's project is the agent's word, so `session_start project=<slug>`, a hook
naming that repository, or `session=` on a tool reads the project with no grant
at all, and the console answers your account for every project that is not
locked.

With them on, an agent whose conversation Seamless identifies - a Claude Code
or Codex agent running the hooks with a current `seam` - can no longer name a
fenced project as its own: `session_start` files a move or grant request
instead of binding, and `session=` acts as another project's session only where
the conversation's home may read and write. Two ways in remain, in both tiers:
an agent started in the project's repository is that project's own, and a
client that sends no agent-process identity binds as before.

So the fence keeps a careful agent out, and the lock holds against one set on
it, because it asks the kernel who is calling: lock a project that must stay
shut. A restricted account has none of these ways in.

A2A speaks for no session, so under peer_auth it refuses a `confidential` or
`sealed` project unless the caller holds a lease on it.

## Restricted local accounts

A **restricted local account** is a second OS account on the machine whose
agents work beside yours: a separate login for unattended agents, say, so that
nothing they run can read your home directory. With
[the daemon's account pinned](#deploy-it-the-daemon-runs-as-its-own-account), it
may be any account other than the daemon's; without the pin it must be an
ordinary account, the kind a person logs in with, since the clients then accept
any system account as the daemon's. List it under `peer_auth.restricted`, by user
name or numeric uid:

```yaml
peer_auth:
  enabled: true
  owner: alice
  socket:
    path: /opt/seamless/run/seamlessd.sock
  force_locked: [example-finance]
  restricted: [_example_agent]
```

What such an account reaches:

- **Every open project that is not locked**, reading and writing. A project
  whose isolation is `confidential` or `sealed`, and every locked
  project, is out of its reach and is not listed for it. Its refusal
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
- **No approvals.** It files no access requests, gets no grants, is never
  moved and holds no leases, so nothing it does can open a locked or
  confidential project.

It is known only through the socket. The kernel names it by its uid, exactly as
it names you; it never holds the bearer key, and a TCP request is always yours.
Its config can live in its own home (`~/.config/seamless/seamless.yaml`, mode `0600`),
or in one file every account shares (below):

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

**One client config for several accounts.** The client config holds no secret,
so one file can serve every account on the machine. When the hooks live in
Claude Code's [managed settings](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_install_hooks_managed),
which no account can edit, this is the shape to use: one root-owned file outside
every home, named with `--config` by every hook, so no account's home holds it
and no account can redirect it. As an administrator:

```bash
printf 'role: client\nserver_url: "unix:///opt/seamless/run/seamlessd.sock"\n' |
  sudo tee /opt/seamless/etc/client.yaml >/dev/null
sudo chmod 0644 /opt/seamless/etc/client.yaml
```

Then write the hooks into the managed settings, each naming that file. This is
macOS's path; on Linux it is `/etc/claude-code/managed-settings.json`. An
existing file keeps everything else in it, and re-running the command leaves
its hooks in place:

```bash
sudo /opt/seamless/bin/seamlessd install-hooks --client claude \
  --settings "/Library/Application Support/ClaudeCode/managed-settings.json" \
  --config /opt/seamless/etc/client.yaml --seam /opt/seamless/bin/seam \
  --mcp=false --skills=false
```

Each account then registers its own MCP bridge and skills, against the same
file. Its hook events are left to the managed settings (`managed`), and the
bridge is `seam mcp-proxy --config /opt/seamless/etc/client.yaml`:

```bash
sudo -u _example_agent -H /opt/seamless/bin/seamlessd install-hooks --client claude \
  --config /opt/seamless/etc/client.yaml --seam /opt/seamless/bin/seam
```

The hooks and the bridge read the shared file because they name it. A `seam`
command the account runs itself, or its agents run from a shell, reads
`$SEAMLESS_CONFIG`, then `~/.config/seamless/seamless.yaml`, and never a
`seamless.yaml` in the repository it runs in. So set
`SEAMLESS_CONFIG=/opt/seamless/etc/client.yaml` in the environment the
account's sessions start with, or give the account a home config of its own.
Check the file as the account:

```bash
sudo -u _example_agent -H env SEAMLESS_CONFIG=/opt/seamless/etc/client.yaml \
  /opt/seamless/bin/seamlessd doctor
```

Its `config permissions` line is **info**: the file holds no secret, and only
root can rewrite it. One that another account could rewrite, or that sits under
a directory another account owns, is a **warn** saying which. A client config
that holds a key keeps the owner-only rule, so never share one.

**Everything it writes is labeled.** Memories, notes, tasks and its rewrites of
yours, plan steps, sessions and their findings, and trials record the account that
wrote them, and every surface shows it: "[written by account _example_agent]"
at the end of briefing, prompt recall (`<seam-recall>`), A2A and
`seam task list` lines, an `account` field in tool results, and a "written by
account _example_agent" chip on console rows. Your own writes look exactly as
they always have. A label stays while the account's words may still be there. A
status change, a claim or release, tags or a star never move it, and neither
does your append, partial edit or rewrite of one field. It clears once you have
rewritten all of the item's text in new words: a memory's body and description,
a note's body, title and description, or a task's title and body. An empty
description or task body counts as yours.

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
a read by that name would then answer with its text. A project memory it wrote
before you created a global rule of that name does not answer either: a read by
the bare name answers with your rule and names the account's memory, which
stays readable by id or with `project=<slug>`. A write by that bare name (an
append, edit, delete, star or supersede) is refused until it says
`project=<slug>` or `project=global`. It cannot ask the gardener to
plan a change (`gardener_request`, `gardener_split`), and it still sees proposals
with `gardener_proposals`. A memory or note whose content it changes loses its star.

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

When the idle reaper expires one of its sessions, the account's agent gets it
back on its next call, exactly as yours does - but a call naming another
account's agent process, or yours, reopens nothing, and a session in a project
the account no longer reaches stays expired.

**A security monitor can tell its work from yours.** With the
[metadata feed](https://thereisnospoon.org/docs/guides/security-monitor/#what-a-frame-carries) on, every frame of its
requests - tool calls, hooks and A2A, writes, proposals, task changes and refused
reaches, its refusal at the console's door included - carries `actor.account`
`_example_agent`, and `actor.session` its qualified session name. The
[commit-matching snapshot](https://thereisnospoon.org/docs/guides/security-monitor/#the-commit-matching-snapshot-version-1)
lists its sessions by that name with `account`, and gives `account` to the tasks it
filed, rewrote or recorded a commit on. Your own frames and entries carry no `account`
and look exactly as before.

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
  captures; point their `server_url` at the socket. On another computer, an
  agent whose `server_url` names its relay's socket makes them the same way.
  Agents can still record findings with `session_end`.
- **It updates itself, as its own account.** The installer's per-user service
  cannot host peer_auth, so neither the installer nor a `seamlessd update` run
  as you touches this install. The daemon installs new releases by itself
  instead ([how](#updates)). Run `make install` only with a client-role config,
  so it updates your own `seam` and hooks and leaves the service alone.
- **Counts stay machine-wide in `usage_summary`**, as they are for isolation:
  volume, never content, and the same number for every caller.
- **The gardener still runs over every project**, as the daemon, for no caller.
  Its proposals about a locked project reach a client only through a lease, and
  the console shows them only in a browser that has unlocked the project.

### Updates {#updates}

A peer_auth daemon installs new releases by itself, with no installer and no
root process, like any install that updates itself: automatic updates are on
until you turn them off. The layout above is all it needs:

- its account holds `/opt/seamless/bin` and both binaries, which the updater
  swaps in place;
- the service starts the daemon again after a clean exit, as the plist and the
  unit above do (`KeepAlive` true, `Restart=always`); `Restart=on-failure`
  would leave it down after an update;
- on Linux, the account lingers (`sudo loginctl enable-linger _seamless`): the
  updater runs as a unit of its user manager, because stopping the system unit
  stops every process in it. Keep every setting in the config file there too: a
  `SEAMLESS_*` variable on an `Environment=` line of the unit, other than
  `SEAMLESS_CONFIG`, never reaches the updater, so it makes the daemon only
  notify.

**Settings -> Updates** (its **Mode** row) reads "installs new releases by
itself", or names what is missing. A daemon set up with root-owned binaries
needs `sudo chown -R _seamless:_seamless /opt/seamless/bin`, on Linux the
linger, and a restart. Turn automatic updates off with the toggle there, or for
good with `update.auto: false` in its config, `/opt/seamless/etc/seamless.yaml`,
which the console cannot override.

The [usual rules](https://thereisnospoon.org/docs/updating/#automatic-updates) hold: the soak,
the quiet moment, the backup, the confirmation and the rollback. Only the
install step differs. Running as the daemon's account, the updater verifies the
release's `checksums.txt` against its Sigstore bundle for the exact tag, checks
the archive against it, keeps the `seamlessd` and `seam` it replaces beside the
new ones, renames the new ones into place, and ends the daemon with `SIGTERM`.
launchd's `KeepAlive` or systemd's `Restart=always` then starts the new binary.
A rollback renames the kept ones back the same way. Hooks installed with
`--seam /opt/seamless/bin/seam` ([Connect the owner](#connect-the-owner)) move
with the daemon. To update by hand:

```bash
sudo -u _seamless /opt/seamless/bin/seamlessd update --config /opt/seamless/etc/seamless.yaml
```

The trade-off: the daemon's account can rewrite its own binaries, so a
compromised daemon could keep itself in place across restarts, and could replace
the `seam` that hooks installed with `--seam /opt/seamless/bin/seam` run. It
already holds every project's data, and your account still cannot write either
binary, so the boundary against your agents is unchanged. To keep the hooks'
`seam` out of its reach while it still updates `seamlessd`, give your hooks
[root's copy](#client-copy). To keep both binaries out of the daemon's reach,
leave `/opt/seamless/bin` root's (`sudo chown -R root /opt/seamless/bin`): the
daemon then only tells you about a new release, and you replace the binaries
with `sudo` and restart it.

#### Owner-signed releases {#owner-signed-releases}

A daemon that updates itself installs whatever release GitHub serves that
passes its Sigstore check, and whoever can push a release tag can produce one
of those. Pin the owner's release keys and it installs only a release the
owner signed with a hardware key ([how that works](https://thereisnospoon.org/docs/updating/#owner-signed-releases)).
As root, put the keys where neither your account nor the daemon's can change
them, and name the file in the daemon's config:

```bash
curl -fsSLo /tmp/release_signers https://raw.githubusercontent.com/arctop/seamless/main/release_signers
ssh-keygen -l -f /tmp/release_signers    # check each fingerprint against the owner's keys
sudo install -o root -g wheel -m 0644 /tmp/release_signers /opt/seamless/etc/release_signers
```

```yaml
# /opt/seamless/etc/seamless.yaml
update:
  release_signers: /opt/seamless/etc/release_signers
```

(`root`'s group is `root` on Linux.) Then restart the daemon. Under peer_auth
`serve` refuses to start when the file is not root's, when a directory above it
is writable by any account but root - the daemon's included, since the pin binds
its own updater - or when it names no key for `seamless-release`; `seamlessd
doctor`, run as the daemon's account, says which. Every update after that, by
itself or by `sudo -u _seamless ... seamlessd update`, verifies
`checksums.txt.sshsig` before it swaps anything, and so does the rollback to the
release it replaced. The pin does not stop a compromised daemon from rewriting
its own binaries; it stops a release the owner did not sign from reaching this
machine through the update path at all.

#### Keep the hooks' seam out of the daemon's reach {#client-copy}

Your hooks run `seam` as you, beside every agent, so whoever can rewrite that
file runs code as you at the next hook. There are three places it can be:

| Hooks installed with | Who can replace it | It follows updates |
|---|---|---|
| no `--seam` (the copy beside your `seamlessd`) | your account, and so every agent you run | when you update it |
| `--seam /opt/seamless/bin/seam` | the daemon's account | at once: the daemon updates it |
| `--seam /opt/seamless/client/seam` | root only | when an administrator refreshes it |

The last keeps it out of both your agents' and the daemon's reach. Make the
directory root's, put a release's `seam` there from a verified download - never
a copy of `/opt/seamless/bin/seam`, which the daemon's account could have
replaced - and point the hooks at it:

```bash
sudo install -d -o root -g wheel -m 0755 /opt/seamless/client
seamlessd install-hooks --client claude --seam /opt/seamless/client/seam    # or codex, or all
```

To refresh it after each update the daemon installs, an administrator runs
this for the release the daemon now runs (`seam version`, or Settings ->
Updates), with the system's own tools, none of them the daemon's. It stops at
the first check that fails:

```bash
sudo sh -eu <<'END'
v=0.9.1        # the daemon's release
os=darwin      # or linux
arch=arm64     # or amd64
d=$(mktemp -d); cd "$d"
base=https://github.com/arctop/seamless/releases/download/v$v
tarball=seamless_${v}_${os}_${arch}.tar.gz
curl -fsSLO "$base/checksums.txt" -O "$base/checksums.txt.sshsig" -O "$base/$tarball"
ok=
for p in $(awk '!/^#/ && NF { print $1 }' /opt/seamless/etc/release_signers); do
  if ssh-keygen -Y verify -f /opt/seamless/etc/release_signers -I "$p" -n seamless-release \
    -s checksums.txt.sshsig <checksums.txt; then ok=1; break; fi
done
[ -n "$ok" ] || { echo "checksums.txt is not signed by the owner's keys" >&2; exit 1; }
want=$(awk -v f="$tarball" '$2 == f { print $1 }' checksums.txt)
got=$(shasum -a 256 "$tarball" 2>/dev/null || sha256sum "$tarball")
[ -n "$want" ] && [ "${got%% *}" = "$want" ] || { echo "$tarball does not match checksums.txt" >&2; exit 1; }
tar -xzf "$tarball" seam
install -o root -m 0755 seam /opt/seamless/client/seam
echo "/opt/seamless/client/seam is v$v's"
END
```

It checks the release against the [pinned keys](#owner-signed-releases), so
the copy is the owner's release. `ssh-keygen` checks the signature, the
namespace and the key, but not whether the hardware key was unlocked with its
PIN (it reads the flags without requiring them); the daemon's pinned updater
checked that too when it installed the release. `seamlessd doctor`, run as you,
has a `hooks' seam` row: who can replace the `seam` your hooks run, and, for
root's copy, whether it is the daemon's release.

**While the client copy is a release behind**, everything keeps working: a
daemon accepts what the previous release's `seam` sends - hooks, the MCP
bridge, `seam` commands - as it does for the [network clients](https://thereisnospoon.org/docs/guides/network-install/)
that run their own release. What waits for the refresh is whatever the new
release changed in `seam` itself: a fix in `seam hook` or `seam mcp-proxy`, a
capture a hook newly reads and sends, a new `seam` subcommand or flag. Refresh
the copy before you run the new release's `install-hooks`: its definitions may
pass the hooks an argument the old `seam` does not know.

### Backups and restore {#restore}

[Scheduled backups](https://thereisnospoon.org/docs/guides/data/#backups-in-the-console) run as the daemon's
account, like everything else it does, so the folder rules are stricter here:

- the backup folder, and every folder above it, must be writable by nobody but
  root and the daemon's account, the rule serve applies to the data directory;
- every backup must stay owned by the daemon's account with mode `0600`. A disk
  that cannot keep that - exFAT, and many network shares - fails the run, and
  the file is removed.

A backup holds every project, the locked ones too, so keep the folder as
carefully as the most sensitive project in it.

**A console restore needs a passkey** while any project is locked - force locked
in the file, or locked from the console. A restore could roll a locked project
back, and the bearer key is not proof of the owner, so **Restore with
passkey** runs a WebAuthn ceremony first. Only a signed-in browser can begin it;
a bearer key cannot. The challenge is bound to the backup file's SHA-256, which
the daemon reads from the file itself, and to the mode, so an approval to bring
back what is missing can never replace everything, or be lifted onto another
file. Each ceremony is single use and lasts five minutes, and the file's digest
is read again before the restore is staged. Like a console lock change, it is
passkey-only for now.

Without a usable passkey - none enrolled, passkeys not configured, or the page
not on the [passkey origin](#the-passkey-origin) - the confirm card says why and
shows the terminal's way instead, with a copy button:

```bash
sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd restore [--merge] <path>
```

[`seamlessd restore`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_restore) refuses to
run as root or as any account but the daemon's: running as that account is its
proof, as for `seamlessd approve`. Either way, the restore keeps today's locks,
approvers and fences, adds the backup's locks, and ends every lease, read grant
and console unlock the backup held.

## Limits

- **A console sign-in is the owner's console for open projects** for 7 days or
  until you sign out; locked projects still need an unlock or a lease. A program
  running as you can still drive the signed-in browser. The Add a passkey page is
  open to anyone who reaches the console: creating a passkey there adds nothing,
  but at most 32 wait at once, so someone looping on it can hold yours off for up
  to 10 minutes.
- **A key device is a TCP caller.** A computer paired with a key of its own
  (the console's Devices page) is you, as the bearer key is, but the daemon
  reads no process chain behind it, so it never holds a lease and never opens a
  locked project. A relay is judged only where `peer_auth.relays` maps it, and
  refused anywhere else ([relays](#agents-on-another-computer-relays)). Pairing
  under peer_auth needs your passkey on the Devices page: the code alone pairs
  nothing.
- **A Windows computer is always a key client.** Windows has no Unix-socket peer
  credentials, so it never runs the relay: its agents reach the daemon as a TCP
  caller, never hold a lease, and never open a locked project. Run agents that
  need one on the daemon's own computer.
- **A relay is trusted for its computer.** Root or the relay's account there
  can report any uid and chain. Every lease is keyed on the relay, so a forged
  report reaches only what that relay's agents were approved.
- **A relay's socket reaches every local account by default (mode `0666`).**
  The daemon refuses a relay you have not mapped. Through a mapped one, only
  your uid there and the restricted accounts you listed get anything. Setup's
  [root step](https://thereisnospoon.org/docs/guides/network-install/#the-relays-root-step) restricts the
  socket anyway, to a group of you and the relay alone
  (`--socket-mode 0660 --socket-group _seamlessowner`); a relay you run by hand
  on a computer with other accounts needs the same.
- **A relay pins the daemon's exact certificate.** Renewing the certificate (a
  tailnet certificate lasts 90 days) refuses every relay until it pairs again.
  It fails closed: no relay forwards to a certificate it did not pin.
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
  project but the locked ones. Backups of the data directory, scheduled
  backups and exports hold every project: keep them as carefully as the most
  sensitive project in them. A backup records the locks, a restore keeps them
  ([how](#restore)), and importing one [checks for a passkey that can open
  them](https://thereisnospoon.org/docs/guides/data/#locked-projects).
- **Anything running as you can watch your terminal.** A `tmux` server your agents
  share lets them capture the pane, and your clipboard is reachable too. Nothing
  the approval commands show is a credential: a console unlock's code opens only
  the browser that showed it, and an enrollment code enrolls only the key it was
  minted for, so being read gives neither away. Check that the code you approve
  is the one your browser shows, and that the key the enrollment command prints
  is the one on your screen.
- **An unlocked browser shows the project to whatever controls that browser.**
  A program running as you can receive the console's cookies by opening your
  browser on another port, but without the browser's key they open nothing. What
  it can still do is drive the browser itself, capture your screen, or read the
  browser's profile files, which hold the key. Safari wraps the key with one in
  your keychain; other browsers may store it as is. Lock the project again on the
  access page once you are done, rather than wait out the three hours.
- **Taking a lock off is one passkey touch, and it holds for every agent.**
  Removing a lock set from the console takes a single passkey approval, and from
  the restart that follows every agent of yours reaches the project as its
  isolation allows, with no approval, until you lock it again. A project listed
  under `peer_auth.force_locked` keeps its lock: the console cannot take it off,
  even with a passkey, though a console unlock still opens it in one browser for
  a while.
- **Your shell is yours to guard.** Under peer_auth `seam approve` works only over
  the socket, and refuses one that the daemon's account does not serve, so a changed
  `server_url` cannot show it false facts. An agent running as you can still edit your shell's startup files, or
  replace a `seam` you own. Run `/opt/seamless/bin/seam approve`, which your
  account cannot write, by its full path if you doubt yours.
- **Some decisions refuse a process an agent started**: bringing a fence down,
  releasing or discarding a held write, clearing a session's mark, and changing
  which features are on. Make them in your own terminal or the console. This stops
  an agent's ordinary tool calls, not one that detaches a process from its tree or
  reads the key in your `console.yaml`, until you
  [retire the key](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup_retire).
- **A terminal sign-in's code queue is open to anyone who reaches the console.**
  At most 32 terminal sign-ins wait at once, so someone looping on **Show a
  code** can hold yours off for up to 10 minutes; the code itself signs in only
  the browser that showed it.

## Move an existing install over

This is where `seamlessd setup` sends you for locked projects: it does not turn
them on itself yet. A first Standalone or Server install lists them as a step to
do by hand, and a re-run offers **Turn on locked projects (peer_auth)** on a
computer that runs the daemon, naming this section. Two of setup's steps come
first:

- **A Server's address.** Set it up first, with setup
  ([the Server's address](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup_address)):
  step 4 below then moves its TLS pair out of your home.
- **A backup to start from.** A new computer imports it with setup
  ([starting from a backup](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup_import)).
  Installed with `SEAMLESS_IMPORT_LOCKS=keep` and `SEAMLESS_NO_SERVICE=1`, no
  daemon serves the backup's locked projects before the move, which carries
  them whole, with their locks, fences and passkeys. Then follow the steps
  below from step 3. The backup's locks come back as console locks
  ([how](https://thereisnospoon.org/docs/guides/data/#locked-projects)), so they need no `force_locked`
  entry, and step 5 has no service to stop.

The data directory moves as a whole; the database stores paths relative to it,
so nothing inside needs rewriting. On one volume the move is a rename.

1. **Fence first, in the old world.** Make each project you will lock
   `confidential` or `sealed` (`seam project isolation <slug> confidential --yes`).
2. **Back up**, as the owner, while the old daemon still serves:
   `seamlessd export -o ~/seamless-before-peer-auth.tar.gz`. The archive holds
   every project, the locked ones included: keep it as carefully as they are.
3. **Create the account, the layout with both binaries, and the pin** above,
   leaving out `/opt/seamless/data`. Keep a
   copy of the current `mcp.api_key` for the console.
4. **Write the daemon's config** at `/opt/seamless/etc/seamless.yaml`
   (`root:_seamless 0640`): your old config with `data_dir: /opt/seamless/data`,
   the `peer_auth` block listing the projects to lock, and the same `mcp.api_key`
   and LLM keys. Check the slugs against your project list now: a misspelled
   entry is refused at startup, and `doctor` names it.

   If the daemon serves TLS, its certificate and key move too: the old config
   points into your home, which the daemon's account cannot read and every
   process of yours can. Copy them into `/opt/seamless/etc/tls`
   (`sudo install -o root -g _seamless -m 0644 <cert> /opt/seamless/etc/tls/`
   and `sudo install -o root -g _seamless -m 0640 <key> /opt/seamless/etc/tls/`),
   point `tls.cert_file` and `tls.key_file` at those copies, and delete the key
   from your home once the new daemon serves. Whatever renews the certificate
   (`tailscale cert`, `certbot`) must from then on write there, as root: `serve`
   refuses a key another account can read, and `doctor` names the file.
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
installer). A TLS key you deleted from your home in step 4 comes back the same
way (`sudo install -o $(id -un) -m 0600 /opt/seamless/etc/tls/<key> <the old path>`),
or the per-user daemon cannot load its certificate. The export from step 2,
wherever step 10 put it, is the fallback if anything in the move went wrong.
