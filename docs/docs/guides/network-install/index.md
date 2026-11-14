# Share one daemon across your computers

> Networked Seamless step by step - set up the Server with seamlessd setup, connect each Client, pair computers on the Devices page, sign in and approve from any browser, and every step by hand.

The default install is **Standalone**: one daemon per computer, bound to
loopback. This guide is the other shape, **Networked**: one computer, the
**Server**, runs the daemon and keeps the data, and every other computer is a
**Client** of it, so a laptop, a desktop and a build box share a single corpus
instead of three that drift apart. [Networked Seamless](https://thereisnospoon.org/docs/concepts/networked/)
explains the model - the relay, pairing, and what stops when the Server is out
of reach. This page is the steps.

It is an opt-in with real tradeoffs. The bearer key is you, and so is a
computer paired with the Server: either reaches every project but the locked
ones, except that under peer_auth a relay reaches only what the account it
names may. So the supported boundary is computers you trust on a network you
trust - see [the security posture](https://thereisnospoon.org/docs/install/#going-beyond-loopback-deliberately). Some
daemon-side captures stop working for agents on another computer, deliberately
and visibly; [what a remote device does not get](#what-a-remote-device-does-not-get)
is the honest list, and it is worth reading before you start.

Set up the Server first: every Client needs its address.

## The topology

| | Server | Client |
|---|---|---|
| `role` | `server` (the default) | `client` |
| Daemon | `seamlessd serve` | none - `serve` refuses outright |
| Service (launchd / systemd / Scheduled Task) | installed | none (a relay, if you install one, runs as a service of its own) |
| Data dir, database, corpus | here | none; `seam status` does not even print a data dir |
| Config keys that matter | `addr`, `server_url`, `allowed_hosts`, `tls.cert_file`, `tls.key_file`, `mcp.api_key` | `role`, `server_url`; a key device adds its own `mcp.api_key`, and `tls.ca_file` for a certificate it compared |
| Console, gardener, embeddings | here | the Server's |

A Client's config is deliberately tiny. `role: client` requires `server_url` - a
client that dials nowhere is a contradiction, and loading one is an error - and
`tls.cert_file` on a client is refused too: a client holds no server
certificate, it trusts one with `tls.ca_file`. Through a relay, `server_url`
names the relay's socket, `unix:///opt/seamless/run/seamlessd.sock`, and the
config holds no key at all.

## Set up the Server {#set-up-the-server}

### A new computer

On the computer that will keep the data, run the installer in a terminal:

```bash
curl -fsSL https://thereisnospoon.org/install | sh
```

Once it has downloaded and verified the release, and before it installs
anything, it asks [`seamlessd setup`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup)
how this computer will be used:

```text
How will you use Seamless?
  [1] Standalone  everything on this computer: the daemon, the data, and the agents that use them
  [2] Networked   one computer keeps the data, and the others connect to it
Enter 1 or 2 [1]: 2

Set up the Server first: each Client needs its address and a pairing code.
  [1] Server      this computer keeps the data, and the other computers connect to it
  [2] Client      this computer connects to the Server and keeps no data of its own
Enter 1 or 2: 1
```

Then it decides where the other computers reach this one:

```text
Where will the other computers reach this one?
  [1] Tailscale   <server name>: this computer's name on your tailnet, with a certificate from the tailnet
  [2] Domain      a name of yours, with its certificate and key
  [3] LAN         a name on your local network: setup shows how to make its certificate
Enter 1-3 [1]:
```

- **Tailscale** is offered, and is the default, when Tailscale runs here and the
  tailnet issues certificates for this computer's MagicDNS name. Setup gets the
  certificate with `tailscale cert`, into `~/.config/seamless/tls`. Otherwise a
  line says why Tailscale is not offered, and the choices are the other two.
- **Domain** is a name of yours with a certificate and key you already have:
  setup asks for the name, then the `Certificate file` and the `Key file`.
- **LAN** is a name on your local network; setup offers this computer's own,
  with `.local` added when it has no domain. It prints an `mkcert` and an
  `openssl` command that make the name's certificate in `~/.config/seamless/tls`,
  to run in another terminal, and then asks for the files. The other computers trust mkcert's `rootCA.pem`,
  or the openssl certificate itself ([how](#step-2-tls)).

Setup takes a name only when it resolves on this computer, A and AAAA, to
addresses this computer holds and none of them loopback. It takes a certificate
only when it loads as a pair with its key, names the host and is valid now, and
the key is readable by your account alone (`chmod 600`). It then binds the
narrowest address that holds every address of the name - one IPv4 address of
it, with the IPv6 addresses `serve` listens on beside it (a tailnet name's two),
else the wildcard - and says each choice in one line:

| Row | What it says |
|---|---|
| `address` | the `server_url` the other computers dial |
| `certificate` | the file, until when it is valid, and how to renew it |
| `bind` | the `addr`, and why it holds every address of the name |
| `passkeys` | the [passkey origin](https://thereisnospoon.org/docs/guides/peer-auth/#the-passkey-origin), derived from `server_url` |
| `allowed` | no other name: the daemon refuses (`421`) a request that names another host |
| `key` | who can read the TLS key, and what that means for the console's origin |

Next, **Start fresh, or from a backup?** Enter starts fresh. A backup - a
`seamlessd export` archive or one from Settings > Backups - is restored into the
empty data directory before anything is installed
([details](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup_import)). Only then does
setup write `~/.config/seamless/seamless.yaml`: `addr`, `server_url`,
`allowed_hosts` and `tls`, with the bearer key. The installer installs the
binaries, wires this computer's agents for https, starts the service and waits
for it to answer.

The install ends with what each Client needs, as `seamlessd devices code`
prints it:

```text
To connect another computer to this server:

  Address:                 https://<server name>:8081
  Certificate fingerprint: <SHA-256 of the certificate>
    (the other computer shows it before it pairs: they must match)
  Pairing code:            get one on the console's Devices page, or run `seamlessd devices code` here
    The code is the approval: whoever enters it first pairs as you.
```

It is followed by the one-liners for macOS and Linux and for Windows, which
carry the certificate's fingerprint, by a reminder that each code works once,
and by locked projects as a step setup does not do yet, with the
[guide that does it by hand](https://thereisnospoon.org/docs/guides/peer-auth/#move-an-existing-install-over).
Run a one-liner on each Client, with a code of its own
([below](#step-3-pair-a-client)).

A script answers the same questions in the environment: `SEAMLESS_SETUP=server`
with `SEAMLESS_SERVER_NAME`, plus `SEAMLESS_TLS_CERT` and `SEAMLESS_TLS_KEY`
unless the name is this computer's Tailscale name, whose certificate setup gets
from the tailnet:

```bash
curl -fsSL https://thereisnospoon.org/install | SEAMLESS_SETUP=server SEAMLESS_SERVER_NAME=<this computer's tailnet name> sh
```

Every knob is in [the CLI reference](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup).

### A computer already running Seamless

Run `seamlessd setup` on it. It says how the computer is set up and offers the
changes it can make:

```text
This computer is set up as Standalone.
  [1] Keep it as it is
  [2] Make it the Server, so other computers connect to it
  [3] Turn on locked projects (peer_auth)
Enter 1-3 [1]:
```

Choose **Make it the Server**. Setup asks the same address questions, but it
never edits a config that exists: it prints the lines to put in
`~/.config/seamless/seamless.yaml`, replacing any it has, and the installer
command to run again, which restarts the daemon and wires the hooks for https.
With locked projects on, the daemon's config and TLS pair are its own
account's, so setup prints the root steps that put them in place instead
([the Server's address](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup_address)).
Setup does not change the address of a computer that is the Server already:
that is [by hand](#step-1-widen-the-server).

### Check the Server

```bash
seamlessd doctor
```

Its transport rows are the ones to read:

| Row | What it tells you |
|---|---|
| `bind` | Loopback is OK. Non-loopback without TLS is a **warn** naming what travels in the clear; non-loopback with TLS is OK with the reminder that the bearer key is still the only authentication. |
| `bind.addresses` | Dials the bind and the IPv6 addresses `serve` listens on beside it. Another process answering at one is a **fail**: it would answer for the daemon there, and `serve` refuses to start while it holds one a name points at. |
| `server_url` | Fetches `/healthz` through the advertised URL. A `421` here is a **fail** that names the Host header it just refused - the allowlist and the advertised name disagree. |
| `tls` | Off, or the certificate's expiry and whether its SANs cover the advertised host. |
| `tls.key_file` | Who can read the TLS key: your account alone is **info**, group or world a **warn**. |
| `server_url.addresses` | Every address the `server_url` host resolves to, against what the daemon listens on. One the daemon does not hold is a **warn**. |
| `tailscale` | **info** when the tailnet issues certificates for the host: then the bind holding every address of the name, not the TLS key, keeps another page off the console's origin. |
| `device pairing` | How a computer pairs here, and how many are paired or waiting. No TLS is a **warn**: a relay cannot pair, and a pairing over plain http from another computer is refused; with a plain http `server_url` to another host no computer can pair, and no code is minted. |
| `peer_auth devices` | Under peer_auth only: a **warn** for each key device paired with a one-time code alone, before peer_auth, which is still you over TCP though no passkey approved it: revoke it and pair it again, so a passkey approves it. |

Every row is in [the doctor reference](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_doctor).

## Connect each Client {#step-3-pair-a-client}

On the Server, get a pairing code: **Get a pairing code** on the console's
[Devices page](#devices), or `seamlessd devices code` in the Server's terminal.
Either shows the code once, valid for 10 minutes and for one computer, with a
one-liner for each kind of Client that carries the Server certificate's
fingerprint. The code works once, so the line the Client's shell keeps in its
history is dead after use. On the Client, run its one-liner:

**macOS · Linux:**

```bash
curl -fsSL https://thereisnospoon.org/install | SEAMLESS_SETUP=client SEAMLESS_SERVER_URL=https://<server name>:8081 SEAMLESS_SERVER_FINGERPRINT=<fingerprint> SEAMLESS_PAIR_CODE=<code> sh
```


**Windows:**

```powershell
$env:SEAMLESS_SETUP='client'; $env:SEAMLESS_SERVER_URL='https://<server name>:8081'; $env:SEAMLESS_SERVER_FINGERPRINT='<fingerprint>'; $env:SEAMLESS_PAIR_CODE='<code>'; irm https://thereisnospoon.org/install.ps1 | iex
```


Setup then pairs the computer before anything is installed, and finishes once
the install is done:

1. **It shows the Server's certificate.** It reads the certificate in a
   handshake that sends nothing, and shows its SHA-256 fingerprint: compare it
   with the `Certificate fingerprint` the Server printed: the one-liner's
   `SEAMLESS_SERVER_FINGERPRINT` has setup compare it (at a terminal without
   it, setup asks you); on a mismatch nothing is sent. A certificate this computer's own
   roots do not trust - one made with mkcert or openssl - is trusted only once
   it was compared, and then as that certificate alone, kept in
   `~/.config/seamless/server-ca.crt`. Without the knob, such a run stops and
   names it. A plain `http://` address is refused unless it is this computer's
   own loopback: the code, and a key of this computer's, would cross the
   network in the clear, and the Server refuses such a pairing too.
2. **It pairs as a relay or a key device.** On macOS and Linux it pairs a
   [relay](https://thereisnospoon.org/docs/concepts/networked/#the-relay), unless `SEAMLESS_PAIR_AS=key` makes
   it a key device, for a computer where you have no root (a container, CI).
   [Windows](#windows-is-always-a-key-client) always pairs a key device.
3. **A key device pairs at once.** Its key, the pairing's answer, goes into a
   new `~/.config/seamless/seamless.yaml` (`0600`) with `role: client`, the
   Server's address and, for a certificate it had to compare, `tls.ca_file`.
   The key is never printed. Setup pairs one only where no config exists yet:
   move an existing one aside first. Under peer_auth it waits for your approval
   on the Devices page.
4. **A relay gets its config now and its root step at the end.** Setup writes a
   config of `role: client` and `server_url: unix:///opt/seamless/run/seamlessd.sock`,
   with no key, and the installer wires your agents to that socket. Once the
   install is done, setup prints the relay's one root step
   ([below](#the-relays-root-step)), with the pairing code on a row of its own.
   It never runs a command as root.
5. **It checks.** For a key device, `seam doctor`, then a session called
   `seamless-setup-check`, whose host the Server reports back: the name the
   Server records this computer's work under. For a relay that does not serve
   yet, it says to run `seam doctor` once it does.

A relay Client pairs again the same way, with a new code; a key device first
needs its config moved aside, since setup never edits one. A computer that runs
a daemon of its own (Standalone, or the Server) is refused: its data would have
to move first. Every knob is in
[the CLI reference](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup).

**At the terminal**, with no variable set, the installer asks instead. Choose
**Networked**, then **Client**:

```text
The Server's address: its setup printed it, and so does seamlessd devices code there.
Server address (https://host:8081): https://<server name>:8081

  fingerprint  <SHA-256 of the certificate>
               the Server's certificate, SHA-256: its setup printed it (Certificate fingerprint), and so does seamlessd devices code there
Does it match what the Server printed? [y/N]: y

How will this computer reach the Server?
  [1] Relay       a small service of its own account: your agents need no key, and locked projects can open to them (one root step)
  [2] Key         a key of this computer's own, for a computer where you have no root (a container, CI); it never opens a locked project
Enter 1 or 2 [1]:
```

Enter at the fingerprint question is no, and no sends nothing. Windows skips
the last question. Setup then asks for the pairing code: a key device at once,
a relay just before its root step.

**With the Server's own key**, `SEAMLESS_SERVER_URL` with `SEAMLESS_MCP_API_KEY`
installs a key client of the Server's shared key, as before: it pairs nothing,
and the Devices page does not list it ([by hand](#pair-with-the-servers-key)).

### The relay's root step {#the-relays-root-step}

The relay names each of your agents to the Server by the uid this computer's
kernel reports, so it runs as an account of its own, where no process of yours
can change what it says, and making that account takes root once. Setup prints
the commands with this computer's values, to read and then run as an
administrator, in four blocks:

1. **Its account and a group.** A system account, `_seamless` (on macOS made
   with `dscl`, on Linux with `useradd --system`), and a group,
   `_seamlessowner`, holding you and it alone: the socket's.
2. **The layout, root's copy of `seamlessd`, and the pin.** Root's
   `/opt/seamless`, `/opt/seamless/bin` and `/opt/seamless/etc`; the relay's
   `/opt/seamless/run`, its home, and `/opt/seamless/relay` (`0700`) for its
   key. Root's own copy of the `seamlessd` the installer just put in your
   `~/.local/bin` goes to `/opt/seamless/bin/seamlessd`, and a
   `shasum -a 256 -c` (Linux: `sha256sum -c`) line checks it is the binary
   setup ran as. A certificate you compared is copied to
   `/opt/seamless/etc/server-ca.crt`. Then the account is pinned in
   `/opt/seamless/etc/daemon-account`, as
   [Deploy it](https://thereisnospoon.org/docs/guides/peer-auth/#deploy-it-the-daemon-runs-as-its-own-account)
   pins the daemon's.
3. **The pairing, as its account:**

   ```bash
   sudo -u _seamless /opt/seamless/bin/seamlessd relay pair --server https://<server name>:8081 --fingerprint <fingerprint> --code -
   ```

   with `--ca-file /opt/seamless/etc/server-ca.crt` for a certificate you
   compared. It waits for you to paste the code, which setup shows on its own
   row and never puts in a command line, where other accounts and your shell
   history would keep it. The key it makes stays in `/opt/seamless/relay`.
   Under peer_auth it waits again, until you approve it on the Devices page.
4. **Its service**: on macOS the LaunchDaemon `com.arctop.seamless.relay`, on
   Linux the system unit `seamless-relay.service`, each running
   `/opt/seamless/bin/seamlessd relay serve --socket-mode 0660 --socket-group _seamlessowner`
   as `_seamless`. Only you, and the relay, reach its socket.

After the blocks, setup prints what the Server needs: under peer_auth it judges
each caller this relay names once you map it, and refuses it until then. Add
the line it prints to the daemon's config, `/opt/seamless/etc/seamless.yaml`,
and restart the daemon ([what it does](https://thereisnospoon.org/docs/guides/peer-auth/#agents-on-another-computer-relays)):

```yaml
peer_auth:
  relays:
    <this computer's name>:
      owner_uid: 501    # your uid on this computer
```

On Linux, log out and back in once: your agents join `_seamlessowner` at your
next login. Approvals then happen in the Server's console, with a passkey, from
any browser ([below](#approve-from-another-computer)).

### Windows is always a key client {#windows-is-always-a-key-client}

Setup offers a Windows computer Standalone and Client only:

```text
How will you use Seamless?
  [1] Standalone  everything on this computer: the daemon, the data, and the agents that use them
  [2] Networked   connect to a Server on another computer, as its Client
      Windows has no peer_auth and no relay, so a Windows computer can be a Client but not the Server.
Enter 1 or 2 [1]:
```

A Windows Client is a key device, always: `SEAMLESS_PAIR_AS=relay` is refused
there, and `seamlessd relay` refuses to run. Its pairing gives it a key of its
own, which the Server's Devices page lists and revokes, and setup says what
that means: a key device never opens a locked project, since the Server cannot
tell its agents apart, so they never hold a lease. Run agents that need a
locked project on the Server's computer, or on a macOS or Linux computer behind
a relay the Server maps ([below](#locked-projects-and-agents-on-other-computers)).
The same holds for a macOS or Linux computer paired as a key device.

### Check a Client

On the Client, any time:

```bash
seam doctor          # the server answers, and tools/list works
seamlessd doctor     # the client report: role, server_url, key, hooks, MCP
```

A client's `seamlessd doctor` is a deliberately short list: role and
`server_url` reachability, the API key, the MCP tool count, and the same
desired-state hook and MCP comparisons the server runs. The database, schema,
repo map, feature skills, gardener, LLM, and embedder checks are all absent,
because every one of them describes a machine that is somewhere else. The
`server_url` probe is a **fail** on a client rather than the server's info
line: until the server answers there are no briefings, no memories, and no
tools on this machine.

On a relay's Client the socket is the relay's: one that does not answer names
its service (`com.arctop.seamless.relay` on macOS, `seamless-relay.service` on
Linux) and how to start it, or, with none installed yet,
[its root step](#the-relays-root-step).

On a computer with a relay, it adds the relay's rows: the account the relay
runs as, its layout, its pairing, its socket, who may connect to it
(`relay.access`, a **warn** when every local account can), and whether the
Server answers it. The pairing is readable by the relay's account alone, so
run doctor as that account to see it
(`sudo -u _seamless -H /opt/seamless/bin/seamlessd doctor`).

## Pair computers on the Devices page {#devices}

A Server that serves other computers - TLS, a bind beyond loopback, or a
`server_url` naming another host - has a **Devices** page in its console,
`/console/devices`, and `seamlessd devices` does the same in the Server's
terminal ([reference](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_devices)). Elsewhere
neither exists.

- **Pair a computer.** The page shows the Server's address and its certificate
  fingerprint. **Get a pairing code** mints a code, shown once, valid for 10
  minutes and for one computer, with the one-liners. `seamlessd devices code`
  prints the same with a new code; under peer_auth, run it as the daemon's
  account:

  ```bash
  sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd devices code
  ```

- **Waiting for your approval**, under peer_auth only: each computer that used a
  code, with its fingerprint and until when it waits. **Approve with a passkey**
  only a computer whose fingerprint is the one its own screen shows; **Deny**
  one you do not recognize. Approving needs a signed-in browser on the
  [passkey origin](https://thereisnospoon.org/docs/guides/peer-auth/#the-passkey-origin) and a passkey
  [enrolled](https://thereisnospoon.org/docs/guides/peer-auth/#enroll-an-approver) there; the page says which
  is missing. Without peer_auth nothing waits: the code is the approval.
- **Paired**: each relay with its key's fingerprint and each key device with the
  start of its key, when it paired and whether your passkey or its one-time
  code approved it, and when it was last seen.
- **Revoked and expired**: what ended, and how.

`seamlessd devices list` prints the same list in the terminal. Without TLS on
the daemon's own listener a relay cannot pair, and the Server refuses a pairing
over plain http from another computer: its code, and a key device's key, would
cross the network in the clear. A key device can still pair through a TLS proxy
on the Server's own computer. Where `server_url` is plain http to another host
no computer can pair, so the page and `seamlessd devices code` say pairing
needs TLS there and mint no code; the devices paired before stay listed and
revocable.

### Revoke a computer {#revoke-a-computer}

**Revoke** on the Devices page, or in the Server's terminal (as the daemon's
account under peer_auth, like `devices code`):

```bash
seamlessd devices revoke <id or name>
```

The computer's credential is refused from its next request, and the requests
it is making end: at once from the page, within 30 seconds from the terminal. A
relay's leases, read grants and waiting access requests end with it. A pairing
that waits is denied instead.

To pair it again, get a new code. A relay pairs again with the root step's
pairing line plus `--replace`, once its old pairing is revoked (the Server
refuses a second live device of the same name), and then restarts:

```bash
sudo -u _seamless /opt/seamless/bin/seamlessd relay pair --replace --server https://<server name>:8081 --fingerprint <fingerprint> --code -
```

The same goes for a renewed certificate: a relay pins the exact certificate it
paired with, so a new one refuses it until it pairs again.

## Sign in to the console from another computer {#console-sign-in}

The console is the Server's: open `https://<server name>:8081/console/` in a
browser on any computer. It signs in one of two ways.

- **With the key**, the default: the login page takes the Server's bearer key,
  once per browser. A Client with a relay holds no key, so it comes from the
  Server's config.
- **With a passkey**, once `console.sign_in: passkey` is set in the Server's
  config (file only) and the daemon restarted. No browser needs the key then,
  the Server's included. Add each device's passkey from the device:
  `seamlessd approvers add`, on the Server, prints the Add a passkey address
  (`/console/passkeys/add` on the passkey origin), which is not a secret. Open it
  on the device and create the passkey there; the page shows a short code. On
  the Server, `seamlessd approvers approve --code <code>` - as the daemon's
  account under peer_auth - shows that passkey and asks before it adds it.
  Approve only a code your own device shows.

A passkey works only for the name it was made for, and needs that name over
https: the [passkey origin](https://thereisnospoon.org/docs/guides/peer-auth/#the-passkey-origin) is the host
of the Server's TLS `server_url`. One synced through iCloud Keychain, Google
Password Manager or another password manager is added once for every device it
syncs to, and a device without one can use your phone's through the browser's
QR prompt. A `localhost` passkey works only in a browser on the Server itself.
The rest is in [the console reference](https://thereisnospoon.org/docs/reference/console/#sign-in-with-a-passkey).

On a Client whose `server_url` is a relay's socket, `seamlessd console-open`
opens nothing - a browser cannot dial a socket - and names the console's address
instead.

## Approve from another computer {#approve-from-another-computer}

A browser on any computer approves with a passkey in the Server's console, and
the Server verifies the ceremony: an agent's access request (`/console/access`),
a console unlock on the project's unlock page, a lock change on the project's
Overview, a pairing on the Devices page under peer_auth, and a restore while a
project is locked. The browser must be signed in, on the passkey origin, with a
passkey [enrolled](https://thereisnospoon.org/docs/guides/peer-auth/#enroll-an-approver) for the Server's name.

Under peer_auth, terminal approvals run only on the Server's computer:
`seam approve`, the command a console unlock's **Approve from a terminal**
shows (`seamlessd console-open --unlock`), and `seamlessd approvers`. Each proves
itself by becoming the daemon's account, which exists only there, so a relay
never vouches that you said yes. A terminal on the Server counts wherever you
sit: SSH to it.

## Locked projects and agents on other computers {#locked-projects-and-agents-on-other-computers}

Run agents that need a locked project on the Server's computer, on its
peer_auth socket (a terminal there, or SSH to it), or on a macOS or Linux
computer behind a relay the Server maps:

- **Behind a relay the Server maps** in `peer_auth.relays`, an agent is judged
  as the Server's socket judges one: your uid on that computer is you, the
  relay names the program of every process it forwards, and an agent asks for
  access as it would on the Server. Your approval names that computer, and its
  lease is keyed on the relay and the process, so it opens the project to that
  agent's process tree there and nothing else. A relay from before program
  names files nothing: update Seamless on that computer.
- **Behind a relay the Server does not map**, every request is refused.
- **On a key device, or a key client of the Server's own key**, an agent never
  holds a lease, so it never opens a locked project. A Windows computer is
  always this case.

Mapping a relay, its restricted accounts, and what the Server then does with
their requests are in
[Agents on another computer: relays](https://thereisnospoon.org/docs/guides/peer-auth/#agents-on-another-computer-relays).

## When the Server is out of reach {#when-the-server-is-out-of-reach}

A Client keeps no data and answers nothing from memory, so its agents lose
Seamless until the Server answers again: a session starts with no briefing,
tools and `seam` commands fail with the reason, and what the hooks would have
sent is lost. A relay refuses every call at once, with `502` and the reason.
[Networked Seamless](https://thereisnospoon.org/docs/concepts/networked/#when-the-server-is-out-of-reach)
has what each path does, and
[Troubleshooting](https://thereisnospoon.org/docs/guides/troubleshooting/#an-agent-on-another-computer-gets-no-briefing)
the fixes.

## What a remote device does not get

Sessions, projects, memories, notes, tasks, trials and recall all work over the
network. What does not is every capture where the **daemon** reads the
**agent's** filesystem, because on a remote device those paths are not the
daemon's to read:

| Capture | Remote behavior |
|---|---|
| Claude Code plan-mode capture (plan-file saves, presentation, approval) | skipped |
| Subagent transcript capture and spawn-prompt matching | skipped |
| Git HEAD stamps on captured plans | left empty, which reads as `unknown` downstream |
| Session-end transcript harvest, token harvest, model sniff | skipped |
| Codex rollout harvest | skipped |
| The gardener's ship-evidence pass (git history behind a stale plan) | no evidence from repos mapped to another host |

This is a **host** check, not a "does the file exist" check, and that is the
point: two devices with the same username and home layout produce the same
transcript and plan-file paths, so a missing-file test would sometimes find a
real file - the wrong one - and capture another machine's session as this one's.
A paired computer's host is the name it paired with: the Server records every
request of it under that name, whatever the request says.

Under peer_auth on the Server, a Client with a relay gets these captures back,
except the gardener's ship evidence. Its `seam hook` reads the transcript, the
plan file and the subagent transcript itself, beside the agent, and sends what
it found, as hooks on the Server's own socket do
([what changes with peer_auth on](https://thereisnospoon.org/docs/guides/peer-auth/#what-changes-with-peer-auth-on)).
A key client, and a relay to a Server without peer_auth, get the table above.

The same host scoping protects project mapping. The repo map is keyed by
`(host, path)`, and the moved-repo heal that re-points a project when its
mapped path has vanished only ever stats **this** daemon's rows. A remote
client's path is never stat'd on the server's disk.

One consequence to know about: a session on another host that sends no
repository root cannot be placed in a project. The daemon will not derive one -
that would mean reading its own disk to answer a question about the client's -
so the session succeeds with **global** scope and says so, as a `warning` field
on `session_start` and as a `register-project-remote-root` hook error event.
Current `seam` resolves the roots locally and sends them, so this is the
signature of a client too old to do that.

### How doctor reports it

None of this is silent. On the **server**:

- **`remote sessions`** - an info line listing which other machines used this
  daemon in the last 24 hours and how many local captures were skipped for
  them. With no remote sessions it reads `none in 24h`, which is also how you
  notice a client that is not reaching you.
- **`repo map`** - counts local mapped paths and stats only those; rows
  belonging to other hosts are reported as present but "not verifiable from
  here" rather than treated as missing.

Every skip is also an event in the log (`hook.error`, stage
`remote-host-skip`, with the capture name and the host), recorded at INFO -
on a shared daemon a skip is the design working, not a fault.

## Updates on a shared daemon

The server's [automatic update check](https://thereisnospoon.org/docs/updating/#automatic-update-checks)
covers the server: its briefing notice reaches every session, and a session on
another machine is told that **the server** can update - an action for whoever
runs it, not for that machine. A server the installer made also
[updates itself](https://thereisnospoon.org/docs/updating/#automatic-updates) by the same rules as any other
install, and a live session on any paired machine counts: it waits for all of
them to go quiet, or for its deadline. Updating the server does not update the
clients: each paired machine keeps its own `seam` binary (the hooks and the MCP
bridge), and updates it by re-running the pairing commands
`seamlessd client-config` prints on the server. A client runs no daemon, so it
has no update check of its own and never updates itself.

A relay runs root's own copy of `seamlessd`, `/opt/seamless/bin/seamlessd`,
which nothing updates by itself yet: updating a Client leaves its relay on the
old binary. Update the relay's copy with the root step again: the second
block's `sudo install` line copies the `seamlessd` the installer just updated
in `~/.local/bin` into place. Then restart the relay's service.

## Windows: a second user on one box

The Windows install is per-user in where it writes (`%USERPROFILE%`) but **not**
isolated in what it registers: the Scheduled Task name (`Seamless`) is global
and the port is shared, so the last installer to run wins and a task belonging
to another account cannot be replaced without admin.

The client role is the clean way out of that collision, because **a client
install registers no Scheduled Task at all**. The first user runs the server;
every other user on the box pairs as a client against it. They do not even need
the steps above - `client-config` says so in its own loopback refusal - a
second user on the same machine can pair directly:

```powershell
seamlessd install-hooks --server-url http://127.0.0.1:8081 --api-key <key>
```

`seamlessd uninstall` on such a client reports the service as `not installed`
and leaves the running daemon alone, and `--purge` removes only the config
directory - never the data dir, which belongs to the server.

## By hand: widen the Server {#step-1-widen-the-server}

Setup's address step writes these keys on a first install. By hand - on a
Server set up before setup existed, to change its address, or with a config
you keep yourself - edit the server's `~/.config/seamless/seamless.yaml`:

```yaml
addr: "0.0.0.0:8081"                       # listen on every interface
server_url: "http://studio.local:8081"     # what clients dial
```

Both keys, not one. `addr` answers "where do I listen"; `server_url` answers
"where do clients reach me", and they stop having the same answer the moment
the bind is a wildcard. Leaving `server_url` unset on a wildcard bind derives
`http://127.0.0.1:8081` - an address that means "your own machine" on every
client that receives it - and the daemon warns about exactly that combination
at startup. Putting the wildcard in `server_url` instead is refused outright at
config load, in any spelling of the unspecified address: `http://0.0.0.0:8081`
reaches, at most, the machine doing the dialing, so it can never be the right
answer to "where do clients reach me".

A concrete IPv4 `addr` (one LAN or tailnet address instead of the wildcard)
also listens on its interface's IPv6 addresses that reach no further, so a
dual-stack name reaches the daemon over either family. Beside a private or
tailnet address that means only unique local addresses (`fd…`), never a global
IPv6 address the internet may reach. Doctor's `bind` row lists them.

Naming `server_url` also arms the **Host-header allowlist**: the daemon then
answers the loopback names, a concrete bind host, and the host of `server_url`,
and refuses everything else with `421 Misdirected Request`. Add any further
name the daemon is reached by - a short hostname, a raw IP - to
`allowed_hosts`. (A wildcard bind has no concrete host of its own, which is why
naming one somewhere is what turns the guard on there.)

Restart the daemon and check it:

```bash
seamlessd restart
seamlessd doctor
```

Doctor's transport rows are the ones [above](#check-the-server).

## By hand: TLS {#step-2-tls}

Plain http over a LAN is a legitimate choice for a key client and Seamless will
not stop you - `seamlessd client-config` prints a warning and continues. But
the bearer key and every memory it fetches cross the network unencrypted, so
prefer https. Pairing needs it: the Server refuses a pairing over plain http from another computer, a relay's and a key device's alike.

Both `tls.cert_file` and `tls.key_file` must be set; one without the other is
refused at load, because it cannot serve TLS. With both set the listener is
https (TLS 1.2 floor), the console session cookie is marked `Secure`, and
`server_url` derives an `https://` scheme.

The certificate must cover **the host of `server_url`** - bare host, no scheme,
no port, lower-cased. That is the name clients verify and the name doctor
checks the SANs against.

### mkcert (the friction-free path)

[mkcert](https://github.com/FiloSottile/mkcert) installs a local CA into your
system trust store and issues certificates from it:

```bash
mkcert -install                                       # once per machine
mkcert -cert-file ~/.config/seamless/seamless.crt \
       -key-file  ~/.config/seamless/seamless.key  studio.local
mkcert -CAROOT                                        # prints the CA directory
```

Server config:

```yaml
addr: "0.0.0.0:8081"
server_url: "https://studio.local:8081"
tls:
  cert_file: "~/.config/seamless/seamless.crt"
  key_file: "~/.config/seamless/seamless.key"
```

Copy `rootCA.pem` from the `mkcert -CAROOT` directory to each client and point
`tls.ca_file` at it there. That one file is what makes the `seam` CLI trust the
server; it is the only TLS key a client has. A relay takes the same file at
pairing (`--ca-file`).

### openssl (no extra tool)

A self-signed certificate works just as well as long as it carries the SAN and
is usable as its own root - `CA:TRUE`, which is why the second `-addext` is not
optional. Needs OpenSSL 1.1.1 or newer for `-addext`:

```bash
openssl req -x509 -newkey rsa:2048 -sha256 -days 825 -nodes \
  -keyout ~/.config/seamless/seamless.key \
  -out    ~/.config/seamless/seamless.crt \
  -subj   "/CN=studio.local" \
  -addext "subjectAltName=DNS:studio.local,IP:192.168.1.10" \
  -addext "basicConstraints=critical,CA:TRUE"
```

Include every name and address clients dial in `subjectAltName`; a client that
dials an IP the certificate does not list fails in the TLS handshake. Then copy
`seamless.crt` - the certificate, never the key - to each client as
`~/.config/seamless/server-ca.crt`, and write that client's whole config:

```yaml
role: "client"
server_url: "https://studio.local:8081"
tls:
  ca_file: "~/.config/seamless/server-ca.crt"
mcp:
  api_key: "<the server's key>"
```

Renewal is manual on both paths - nothing auto-renews - which is why doctor's
`tls` line warns from 30 days out. A renewed certificate is a new one to every
relay, which pins the exact certificate it paired with: pair each relay again
([how](#revoke-a-computer)).

### Why https changes the Claude Code wiring

Under an `https://` base URL, `install-hooks` writes a different shape on
purpose: **every** Claude Code hook becomes a command hook (including
`UserPromptSubmit`, normally the one http hook), and the MCP registration
becomes the `seam mcp-proxy` stdio bridge instead of a direct URL. Claude Code
performs http hooks and http MCP connections with its own client, which has
nowhere to be told about `tls.ca_file`; against a private CA that request dies
in the TLS handshake. Routing both through `seam` puts them on the CLI's trust
store, which does read `tls.ca_file`. Under plain http the shapes are
unchanged.

## By hand: connect a Client with the Server's key {#pair-with-the-servers-key}

On the **server**, ask for the pairing block:

```bash
seamlessd client-config            # add --redact to paste it into a ticket
```

It prints the server URL, the key, the minimum client version, and three
paste-able commands: the macOS/Linux installer one-liner, the PowerShell one,
and the manual form for a machine that already has the binaries:

```bash
seamlessd install-hooks --server-url https://studio.local:8081 --api-key <key>
```

That command writes `role: client`, `server_url`, and `mcp.api_key` into
`~/.config/seamless/seamless.yaml` on first run and wires the detected agent
clients against the server's URL. Like every other config bootstrap it never
edits a config file that already exists: it errors and names the exact lines to
add by hand. The one existing file that is not an error is one that already
says exactly this, so re-running the installer is safe.

The installer one-liners are the same thing with the binaries and the download
included - `SEAMLESS_SERVER_URL` plus `SEAMLESS_MCP_API_KEY`, which is a pair:
the URL without the key is a hard error. A client install skips the service
branch entirely - the run prints the service step as skipped, naming the server
it is a client of - and polls the *server's* `/healthz` instead of a local one.

`client-config` refuses rather than printing a command that cannot work:

| Refusal | Why |
|---|---|
| this install is `role: client` | it has no clients of its own to pair; run it on the server |
| `mcp.api_key` is empty | a client would have nothing to authenticate with |
| `server_url` is loopback | pasted on another machine it dials *that* machine's port 8081. It names the fix: `server_url` plus `addr: 0.0.0.0:8081`, then restart |

Check the Client as [above](#check-a-client).

## By hand: install a relay {#install-a-relay}

[The relay's root step](#the-relays-root-step), as setup prints it, is the
reference: the same commands with this computer's values filled in. These are
they with placeholders, for a Client set up another way (one paired with the
Server's key, say). The Server needs TLS on the daemon's own listener; on the
Client you need an administrator. `root`'s group is `wheel` on macOS, `root` on
Linux.

1. **On the Server, get a pairing code** on the Devices page, or with
   `seamlessd devices code`, and note the certificate fingerprint it shows.
2. **The account and the socket's group.** On macOS, make `_seamless` with the
   `dscl` lines in
   [Deploy it](https://thereisnospoon.org/docs/guides/peer-auth/#deploy-it-the-daemon-runs-as-its-own-account),
   then the group, with another unused id below 500, and its two members:

   ```bash
   sudo dscl . -create /Groups/_seamlessowner PrimaryGroupID <unused id>
   sudo dscl . -append /Groups/_seamlessowner GroupMembership <you> _seamless
   ```

   On Linux:

   ```bash
   sudo useradd --system --user-group --shell /usr/sbin/nologin --home-dir /opt/seamless/home _seamless
   sudo groupadd --system _seamlessowner
   sudo usermod -aG _seamlessowner <you>
   sudo usermod -aG _seamlessowner _seamless
   ```

3. **The layout, root's copy of `seamlessd`, and the pin:**

   ```bash
   sudo install -d -o root -g wheel -m 0755 /opt/seamless /opt/seamless/bin /opt/seamless/etc
   sudo install -d -o _seamless -g _seamless -m 0755 /opt/seamless/run
   sudo install -d -o _seamless -g _seamless -m 0700 /opt/seamless/home /opt/seamless/relay
   sudo install -o root -g wheel -m 0755 ~/.local/bin/seamlessd /opt/seamless/bin/seamlessd
   echo "_seamless $(id -u _seamless)" | sudo tee /opt/seamless/etc/daemon-account >/dev/null
   sudo chmod 0644 /opt/seamless/etc/daemon-account
   ```

   A certificate made with mkcert or openssl also goes in, for the pairing to
   trust: its root (mkcert's `rootCA.pem`, or the openssl certificate itself)
   as `/opt/seamless/etc/server-ca.crt`, root's, mode `0644`. The relay refuses
   to run with any part of this that another account could rewrite, and says
   which.
4. **Pair it, as its account**, pasting the code when it waits (`--code -`
   keeps it out of the process list and your shell history), and adding
   `--ca-file /opt/seamless/etc/server-ca.crt` for the certificate above:

   ```bash
   sudo -u _seamless /opt/seamless/bin/seamlessd relay pair --server https://<server name>:8081 --fingerprint <fingerprint> --code -
   ```

   `--fingerprint` makes sure the code goes only to that certificate. Under
   peer_auth the command waits until you approve the relay on the Devices page,
   and prints the key fingerprint the page shows for it. Once paired, it prints
   the certificate it pinned and the name it speaks for.
5. **Run it as a service**, as its account, never as root:
   `/opt/seamless/bin/seamlessd relay serve --socket-mode 0660 --socket-group _seamlessowner`,
   from the LaunchDaemon or the systemd unit in
   [the service reference](https://thereisnospoon.org/docs/reference/service/#relay). Without the socket flags
   every local account may connect, and a Server without peer_auth takes each
   one for you.
6. **Point your agents at it.** Your config on the Client names the socket and
   holds no key:

   ```yaml
   role: client
   server_url: "unix:///opt/seamless/run/seamlessd.sock"
   ```

   Then install the hooks and MCP registration against it:

   ```bash
   seamlessd install-hooks --client claude    # or codex, or all
   ```

7. **Under peer_auth, map it on the Server** in `peer_auth.relays`, by the name
   the Devices page shows, to your uid on the Client, and restart the daemon
   ([how](https://thereisnospoon.org/docs/guides/peer-auth/#agents-on-another-computer-relays)). Until then
   the daemon refuses it.
8. **Check it.** `seam doctor` on the Client reaches the Server through the
   relay. `seamlessd doctor` adds the relay's rows (`relay.account`,
   `relay.pairing`, `relay.socket` and the rest): run as you, they cover the
   account, the layout and the socket; run as the relay's account
   (`sudo -u _seamless -H /opt/seamless/bin/seamlessd doctor`), its pairing and
   whether the Server accepts it too.

## Related

- [Networked Seamless](https://thereisnospoon.org/docs/concepts/networked/) - the model: the relay, pairing,
  approvals, and what stops when the Server is out of reach.
- [Install & deploy](https://thereisnospoon.org/docs/install/#going-beyond-loopback-deliberately) - the
  security posture you are accepting, and the installer overrides.
- [Configuration](https://thereisnospoon.org/docs/reference/configuration/) - `role`, `server_url`,
  `allowed_hosts`, and the `tls:` block with every default.
- [seamlessd CLI](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_setup) - `setup`,
  [`client-config`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_client_config),
  [`devices`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_devices),
  [`relay`](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_relay), `install-hooks
  --server-url`, and `serve` under TLS.
- [Lock projects behind your approval](https://thereisnospoon.org/docs/guides/peer-auth/) - peer_auth on the
  Server, and the relays it maps.
- [Import, back up & restore](https://thereisnospoon.org/docs/guides/data/) - `seamlessd export` and a merge
  import, which is how you fold each device's existing local instance into the
  shared one before switching it to a client.
- [Hooks](https://thereisnospoon.org/docs/reference/hooks/) - the identity the hooks send, and what the
  daemon does with it.
