# Share one daemon across a LAN

> Run one seamlessd for several devices - the server config, TLS with mkcert or openssl, the client-config pairing command, and the captures a remote device honestly does not get.

The default install is one daemon per machine, bound to loopback. This guide is
the other shape: **one daemon, several devices**, so a laptop, a desktop, and a
build box share a single corpus instead of three that drift apart.

It is an opt-in with real tradeoffs. The bearer key is still the only
authentication and there is still no per-client authorization, so the supported
boundary is a trusted LAN - see
[the security posture](https://thereisnospoon.org/docs/install/#going-beyond-loopback-deliberately). Some
daemon-side captures stop working for remote devices, deliberately and visibly;
[what a remote device does not get](#what-a-remote-device-does-not-get) is the
honest list, and it is worth reading before you start.

## The topology

One machine is the **server**: it runs `seamlessd`, owns `~/.seamless` (the
SQLite database and the markdown corpus), and is the only machine that needs a
service. Every other machine is a **client**: it installs the two binaries and
wires its agent clients' hooks and MCP registrations at the server's URL, and
it runs no daemon of its own.

| | Server | Client |
|---|---|---|
| `role` | `server` (the default) | `client` |
| Daemon | `seamlessd serve` | none - `serve` refuses outright |
| Service (launchd / systemd / Scheduled Task) | installed | none |
| Data dir, database, corpus | here | none; `seam status` does not even print a data dir |
| Config keys that matter | `addr`, `server_url`, `allowed_hosts`, `tls.cert_file`, `tls.key_file`, `mcp.api_key` | `role`, `server_url`, `mcp.api_key`, `tls.ca_file` |
| Console, gardener, embeddings | here | the server's |

A client's config is deliberately tiny. `role: client` requires `server_url` -
a client that dials nowhere is a contradiction, and loading one is an error -
and `tls.cert_file` on a client is refused too: a client holds no server
certificate, it trusts one with `tls.ca_file`.

## Step 1: widen the server

Edit the server's `~/.config/seamless/seamless.yaml`:

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

Doctor's transport lines are the ones to read:

| Line | What it tells you |
|---|---|
| `bind` | Loopback is OK. Non-loopback without TLS is a **warn** naming what travels in the clear; non-loopback with TLS is OK with the reminder that the bearer key is still the only authentication. |
| `server_url` | Fetches `/healthz` through the advertised URL. A `421` here is a **fail** that names the Host header it just refused - the allowlist and the advertised name disagree. |
| `tls` | Off, or the certificate's expiry and whether its SANs cover the advertised host. |

## Step 2: TLS

Plain http over a LAN is a legitimate choice and Seamless will not stop you -
`seamlessd client-config` prints a warning and continues. But the bearer key
and every memory it fetches cross the network unencrypted, so prefer https.

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
server; it is the only TLS key a client has.

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
`tls` line warns from 30 days out.

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

## Step 3: pair a client

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

On the client, confirm:

```bash
seam doctor          # resolved URL, server reachable, key accepted, tools/list count
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

## Windows: a second user on one box

The Windows install is per-user in where it writes (`%USERPROFILE%`) but **not**
isolated in what it registers: the Scheduled Task name (`Seamless`) is global
and the port is shared, so the last installer to run wins and a task belonging
to another account cannot be replaced without admin.

The client role is the clean way out of that collision, because **a client
install registers no Scheduled Task at all**. The first user runs the server;
every other user on the box pairs as a client against it. They do not even need
the LAN setup above - `client-config` says so in its own loopback refusal - a
second user on the same machine can pair directly:

```powershell
seamlessd install-hooks --server-url http://127.0.0.1:8081 --api-key <key>
```

`seamlessd uninstall` on such a client reports the service as `not installed`
and leaves the running daemon alone, and `--purge` removes only the config
directory - never the data dir, which belongs to the server.

## Related

- [Install & deploy](https://thereisnospoon.org/docs/install/#going-beyond-loopback-deliberately) - the
  security posture you are accepting, and the installer overrides.
- [Configuration](https://thereisnospoon.org/docs/reference/configuration/) - `role`, `server_url`,
  `allowed_hosts`, and the `tls:` block with every default.
- [seamlessd CLI](https://thereisnospoon.org/docs/reference/cli-seamlessd/#seamlessd_client_config) -
  `client-config`, `install-hooks --server-url`, and `serve` under TLS.
- [Import, back up & restore](https://thereisnospoon.org/docs/guides/data/) - `seamlessd export` and a merge
  import, which is how you fold each device's existing local instance into the
  shared one before switching it to a client.
- [Hooks](https://thereisnospoon.org/docs/reference/hooks/) - the identity the hooks send, and what the
  daemon does with it.
