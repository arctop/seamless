---
title: Networked Seamless
description: One computer keeps the data and the others connect to it - the Server and its Clients, the relay, pairing, approvals from any browser, and what stops when the Server is out of reach.
---

Seamless is **Standalone** by default: the daemon, the data and the agents that
use them, all on one computer. **Networked** is the other shape. One computer,
the **Server**, keeps the data, and every other computer is a **Client** of it,
so a laptop, a desktop and a build box share one corpus instead of three that
drift apart. [`seamlessd setup`](/reference/cli-seamlessd/#seamlessd_setup),
which the installer runs before it installs anything, asks which a computer
will be.

This page is the model. [Share one daemon across your
computers](/guides/network-install/) has the steps.

## Three kinds of computer {#three-kinds-of-computer}

| | Standalone | Networked: Server | Networked: Client |
|---|---|---|---|
| In setup's words | everything on this computer: the daemon, the data, and the agents that use them | this computer keeps the data, and the other computers connect to it | this computer connects to the Server and keeps no data of its own |
| The daemon | runs here | runs here | none: `seamlessd serve` refuses on a client |
| The data | here | here, for every computer | none; it is the Server's |
| Its agents reach | this daemon | this daemon | the Server, through a relay or with a key |
| Offered on | macOS, Linux, Windows | macOS, Linux | macOS, Linux, Windows |

Set up the Server first: every Client needs its address. A Client never starts
from a backup, since its data is the Server's. A Standalone install can become
the Server later. Setup does not make a computer that keeps its own data a
Client, or a Client anything else: those moves are by hand.

**Windows is never the Server.** Setup offers it Standalone and Client only,
and says why: "Windows has no peer_auth and no relay, so a Windows computer can
be a Client but not the Server". A Windows Client is always a key client: it
never runs the relay, and its agents never open a locked project.

## How a Client reaches the Server {#how-a-client-reaches-the-server}

There are two ways, and the difference is what the Server can tell apart.

- **Through a relay** (macOS and Linux). The relay is a small daemon on the
  Client, run by an account of its own. Agents dial a local socket, exactly as
  they would beside a [peer_auth](/guides/peer-auth/) daemon, and the computer
  holds no bearer key. The Server learns each request's local account and
  process chain, as its own socket does.
- **With a key** (Windows, and a computer where you have no root, such as a
  container or CI). The Client's config holds a key of its own, from its
  pairing, and dials the Server's address. The Server sees one caller: you, as
  the bearer key is.

| | Through a relay | With a key |
|---|---|---|
| Platforms | macOS, Linux | any; Windows always |
| Root on the Client | to install the relay | none |
| What the Client holds | the relay's private key, readable by its account alone | its own key in your config (`0600`) |
| What the Server learns of a caller | its uid and process chain, believed under peer_auth for a relay you map | nothing: every request is you |
| Locked projects | judged per agent, for a relay you map under peer_auth ([below](#locked-projects-across-computers)) | never |

Setup pairs a macOS or Linux Client as a relay unless you ask for a key, and
Windows always as a key device. A Client can still use the Server's own key
instead, which `seamlessd client-config` prints on the Server: that pairs
nothing, and the Devices page does not list it.

## The relay {#the-relay}

`seamlessd relay` is the same binary, run on the Client as a system account of
its own. It serves the socket agents dial beside a peer_auth daemon,
`/opt/seamless/run/seamlessd.sock`, so `seam`, its hooks, the `seam mcp-proxy`
bridge and the Touch ID helper reach the Server through it exactly as they
reach a daemon on their own computer. For each local connection it reads the
caller's uid and process chain from its own kernel, and forwards the request to
the Server over TLS with its own client certificate, carrying those two facts.
It runs on macOS and Linux only, the platforms whose kernels name a socket's
peer.

**Why it needs root.** The uid and chain it reports are believed only because no
process of yours can change what the relay says. So it runs as an account of
its own, pinned by root as the daemon's account is on a peer_auth server, with
its files where only that account and root can write: the binary, the socket's
directory, and its data directory, `/opt/seamless/relay` (`0700`), which holds
its private key. It refuses to run as root or as your account, and refuses any
layout another account could change. Root sets up the account and that layout;
the relay itself never runs as root.

**What it sees.** The uid and process chain of each local caller, and every
request and answer it forwards: it is a proxy, so whatever an agent behind it
reads passes through it. It reads none of your files. What a hook captures from
the agent's files, `seam hook` reads as you, beside the agent.

**What it never does: decide.** The Server judges every request. The relay says
who is asking, never that you said yes. It interprets no content, caches
nothing and answers nothing from memory, and it forwards only to the
certificate the Server presented when the relay paired.

**What the Server trusts it for.** Under peer_auth the Server judges a relay
only once you map it in `peer_auth.relays`, and refuses it until then. Through
a mapped relay your uid there is you, a restricted account you list there is
that account, labeled with the computer's name, and any other uid is refused
([how](/guides/peer-auth/#agents-on-another-computer-relays)). Root, or the
relay's account, on the Client can report any uid and chain, so every lease is
keyed on the relay: a forged report reaches only what that relay's agents were
approved. Without peer_auth on the Server, every request a relay forwards is
you, whatever uid it names, so the socket's mode and group are the only gate on
that computer. Setup's root step hands the socket to a group of you and the
relay alone (mode `0660`); `seamlessd doctor` on the Client warns when every
local account may connect (`relay.access`).

## Pairing {#pairing}

A computer pairs with the Server once, with a one-time code you get on the
Server, and from then on holds a credential of its own, which you can revoke: a
relay, a TLS certificate over its own key; a key device, a key of its own. A
Client that uses the Server's own key has not paired.

- **The code.** **Get a pairing code** on the console's Devices page, or
  `seamlessd devices code` on the Server. A code works once, for 10 minutes and
  for one computer; only its hash is stored, and at most 16 wait at once.
- **Without peer_auth, the code is the approval**: whoever enters it first
  pairs as you. Use it on your own computer, and get a new one for each
  computer.
- **Under peer_auth, the code alone pairs nothing.** The computer waits, for up
  to 10 minutes, until you approve it with your passkey on the Devices page,
  after checking that the fingerprint there is the one that computer shows.
- **Two fingerprints.** The Server's certificate fingerprint (its SHA-256, as
  `openssl x509 -noout -fingerprint -sha256` prints it): the Client shows it
  before it sends the code, for you to compare with the one the Server printed,
  and a relay pins that certificate from then on. A certificate the Client's
  own roots do not trust is trusted only once compared, and then as that
  certificate alone. The relay's key fingerprint: the relay prints it, and the
  Devices page shows it for your approval.
- **A paired computer speaks for its name.** The name it paired with, its host
  name by default, is the host every request of it is recorded under, whatever
  the request says. No two live devices share a name, and none takes the
  Server's own.
- **A paired computer is you, at most.** A key device reaches what the bearer
  key reaches. A relay does too without peer_auth; under it, the Server judges
  a relay you map as its own socket judges a request, and refuses one you have
  not mapped.

A relay needs TLS on the daemon's own listener. It proves itself with a client
certificate in the TLS handshake, so a proxy that ends TLS in front of the
daemon lets key devices pair but not relays.

Pairing is on only on a Server that serves other computers: TLS, a bind beyond
loopback, or a `server_url` naming another host. A default install has no
Devices page and no pairing endpoint, and asks no browser for a certificate.

## Revocation {#revocation}

Revoke a computer on the Devices page, or with
`seamlessd devices revoke <id|name>` on the Server. Its credential is refused from its next request, and
the requests it is making end: at once from the page, within 30 seconds from
the terminal. A relay's leases, read grants and waiting access requests end in
the same step. A pairing that waits is denied instead.

A revoked computer pairs again with a new code. A relay pairs again with
`seamlessd relay pair --replace`, after you revoke its old pairing: the Server
refuses a second live device of the same name.

## Locked projects across computers {#locked-projects-across-computers}

A locked project opens only to an agent whose process tree holds a lease you
approved, and only a kernel can name that tree.

- **Agents on the Server's own computer**, on its peer_auth socket, open a
  locked project as [Lock projects behind your approval](/guides/peer-auth/)
  describes.
- **Agents behind a relay `peer_auth.relays` maps** are judged as that socket
  judges them. The relay names the program of every process it forwards, so an
  agent there asks for access as it would on the Server: never a shell,
  terminal or multiplexer, only an agent program or a process an agent
  started. Every approval shows a `relay` warning naming the computer whose
  pid, uid and program they are, and the lease is keyed on the relay and the
  process. A relay from before program names files nothing: update Seamless on
  that computer.
- **Key devices, key clients of the Server's own key, and the bearer key**
  never hold a lease, so they never open a locked project. A Windows computer
  is always one of these. A relay you do not map gets nothing at all.

So run agents that need a locked project on the Server's computer, or behind a
relay the Server maps.

## Approvals from any browser {#approvals-from-any-browser}

Every passkey approval in the console is a ceremony the Server verifies,
whichever computer the browser runs on: an agent's access request, a console
unlock, a lock change, a device pairing under peer_auth, and a restore while a
project is locked. The browser must be signed in to the console, on the
Server's [passkey origin](/guides/peer-auth/#the-passkey-origin): the https
address of the Server's name.

Under peer_auth, terminal approvals run only on the Server's computer, in a
terminal there or over SSH to it: `seam approve`, `seamlessd approve`, the code
a console unlock shows (`seamlessd console-open --unlock`), and every
`seamlessd approvers` verb. Each proves itself by becoming the daemon's
account, which exists only on the Server; a Client's relay never vouches that
you said yes.

A browser on a Client signs in to the Server's console with the key, or with a
passkey under `console.sign_in: passkey`, so that no browser needs the key
([how](/guides/network-install/#console-sign-in)).

## When the Server is out of reach {#when-the-server-is-out-of-reach}

A Client keeps no data of its own, so nothing on it works without the Server.

- **Through a relay**, every request is refused at once, with the reason. The
  relay answers `502` and `seamless relay: the server at <address> could not be
  reached (...); nothing is answered from memory, so every call fails until it
  answers`. A Server that presents another certificate than the one the relay
  pinned gets the same kind of refusal, naming the fix. The relay starts even
  while the Server is down, and forwards again as soon as it answers.
- **With a key**, the connection fails.
- **Either way, the hooks fail open.** A session starts with no briefing, and
  what its hooks would have sent is lost: nothing is queued for later. MCP
  tools and `seam` commands fail with the reason.

**A renewed certificate is a new certificate.** A relay pins the exact
certificate the Server presented at pairing, so renewing it (a tailnet
certificate lasts 90 days) refuses every relay until it pairs again.
[Troubleshooting](/guides/troubleshooting/#an-agent-on-another-computer-gets-no-briefing)
has the fix.

## Related

- [Share one daemon across your computers](/guides/network-install/) - the
  Server's setup, each Client, the Devices page, and every step by hand.
- [Lock projects behind your approval](/guides/peer-auth/) - peer_auth, leases,
  and how the Server judges a relay you map.
- [seamlessd CLI](/reference/cli-seamlessd/#seamlessd_setup) - `setup`,
  [`devices`](/reference/cli-seamlessd/#seamlessd_devices) and
  [`relay`](/reference/cli-seamlessd/#seamlessd_relay).
