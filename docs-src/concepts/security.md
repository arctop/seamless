---
title: Security and privacy
description: What Seamless protects and from whom - what a fresh install does, what can leave your computer, the five layers you can add, and the limits, stated plainly.
---

Seamless holds what your agents learn about your work: its rules, its
decisions, sometimes the names of its clients. This page is the map of how that
is protected. It covers what a fresh install does, what can leave your
computer, the five layers you can add when the work calls for them, and what
none of them cover. Each layer has a guide of its own; this page says what each
one is for and links there.

## What Seamless defends against {#threat-model}

An agent is capable and usually well behaved, and it can still be tricked. A
web page, an issue, an email or a file it reads can carry instructions, and an
agent that follows them acts with everything it can reach. Seamless is built
around that case. Each layer narrows what a tricked agent can read, what it can
plant for the sessions that come after it, and what it can open without you.

The other risks are the usual ones for software that runs on your computer: a
program elsewhere on your network, another account on the same computer, a
release that is not what it claims to be, and lost data.

## A fresh install {#fresh-install}

A **Standalone** install, the installer's default, starts like this:

- **It answers this computer only.** The daemon listens on `127.0.0.1:8081`,
  and a request naming any host it does not serve gets `421`, which closes DNS
  rebinding.
- **A key guards it.** The first run writes a random 32-byte key into
  `~/.config/seamless/seamless.yaml` with `0600` permissions. The agent
  registrations read it from there when they connect; none copies it into the
  agent's own config ([how](/install/#security-posture)). The console signs in
  with it, or with a [passkey](/reference/console/#sign-in-with-a-passkey) once
  you switch to passkey sign-in.
- **Your knowledge is files on your disk.** Memories and notes are markdown
  under `~/.seamless`, and sessions, tasks and events live in a SQLite file
  beside them. There is no Seamless account and no hosted service.
- **It backs itself up.** A release install takes a backup every day and keeps
  the last seven, in `~/.seamless/backups`. A restore can be undone
  ([backups](/guides/data/)).
- **Updates prove where they came from** before they install
  ([below](#releases-and-updates)).

Every project starts `open`, and the five layers below start off.

## What leaves your computer {#outbound}

Seamless sends no usage data. On its own it makes one kind of request: a
release build asks GitHub's release list for new versions every six hours,
anonymously, and an install the installer made downloads the updates it
installs. `update.check: false` turns all of that off.

Everything else is something you set up or an agent asked for:

- **A model provider, once you add its key.** With OpenAI or Anthropic
  configured, the text of every memory and note is sent to the provider to be
  embedded, from every project, fenced and locked ones included, and so is
  each recall query. The gardener's monthly digests send a project's session
  findings to the chat model, and `gardener_request` and `gardener_split` call
  it when an agent invokes them. [Ollama](/reference/configuration/) keeps all
  of it on your computer. With no provider, recall runs on full-text search
  alone.
- **`capture_url`** fetches only the URLs an agent asks for, on ports 80 and
  443, and refuses private, loopback and link-local addresses.
- **Your own computers.** A networked Client talks to your Server.
- **The Touch ID approver**, if you choose to download it.

Two things are outside Seamless's reach: what it puts in an agent's context
travels to that agent's model provider with the rest of the conversation, and
the agent's own client keeps its transcripts.

## The five layers {#layers}

The console's [Privacy](/reference/console/#privacy) screen draws these for
your own server: which are on, which wait on an earlier one, and how far a
compromised agent gets in each arrangement.

<figure class="doc-figure" aria-labelledby="layers-caption">
  <span class="figure-kicker">From a fresh install inward</span>
  <div class="doc-stack">
    <div class="flow-node"><span class="flow-step">1 · Project fences</span><strong>open · confidential · sealed</strong><small>Keep one project's knowledge away from agents working anywhere else.</small></div>
    <div class="flow-node"><span class="flow-step">2 · Write screening</span><strong>held writes · provenance marks · quarantine</strong><small>Keep planted instructions and leaked secrets out of what agents read.</small></div>
    <div class="flow-node"><span class="flow-step">3 · Seamless in its own account</span><strong>peer_auth</strong><small>Move the data out of your agents' reach, and let the kernel name every caller.</small></div>
    <div class="flow-node emphasis"><span class="flow-step">4 · Locked projects</span><strong>Touch ID · passkey · seam approve</strong><small>A locked project opens only for an agent you approve, for as long as you choose.</small></div>
    <div class="flow-node"><span class="flow-step">5 · Restricted agent accounts</span><strong>peer_auth.restricted</strong><small>Agents run as an account of their own and reach only what you open to it.</small></div>
  </div>
  <figcaption id="layers-caption">Layers 1 and 2 work in any install, on every OS. Layers 3 to 5 build on each other and run on macOS and Linux.</figcaption>
</figure>

### 1. Project fences {#fences}

A project is `open` by default and shares the way a personal knowledge base
should. Make it `confidential` and nothing in it reaches an agent working in
another project, whether through briefings, recall, tool reads, family
surfaces or the gardener. Make it `sealed` and nothing outside reaches in
either: global memories and family stop at its edge. A refusal never echoes
the content it protects.

With access requests on, an agent that needs a fenced project asks, and you
approve a read-only or read-and-write grant, or a move of the whole
conversation into the project. What a conversation writes after reading fenced
content carries a provenance stamp
([project isolation](/concepts/project-isolation/)).

A fence binds what Seamless serves. Until layer 3 it does not stop an agent
that reads `~/.seamless` directly, and it trusts the session an agent says it
belongs to.

### 2. Write screening {#screening}

Briefings and recall make the shared memory the most leveraged place to plant
a prompt injection: a poisoned constraint reaches every session until someone
notices. Write screening checks every agent write to a memory or note, on your
computer, for secrets, instructions aimed at agents, hidden text and
characters, long encoded blobs and links outside an allowlist.

- A write that fails waits on the console's **Held** screen until you release
  or discard it, and the agent is told not to rephrase its way past the hold.
- A supersede or delete of a constraint is always held, and so is a change to
  a rule from a session that read outside text.
- Injected memories say whether you confirmed them or an agent wrote them.
- A memory or note file changed on disk by anything but Seamless is
  quarantined until you accept it, and a lockdown switch narrows briefings to
  what you confirmed.

It is off by default, and the console's Home welcome card offers it after
install ([write screening](/guides/write-screening/)). The screens are
heuristics: they catch the known shapes of an attack and surface the rest for
review.

### 3. Seamless in its own account {#own-account}

Out of the box Seamless runs as you, so any program of yours can read its
files and its key. `peer_auth` moves it into an operating-system account of its
own with a `0700` data directory, and serves agents on a Unix socket where the
kernel reports each caller's account. Your agents reach Seamless through that
socket, and its data directory is out of their reach. The daemon refuses to start
if another account could rewrite its data, its socket's directory, its config
or its binary ([peer_auth](/guides/peer-auth/)).

### 4. Locked projects {#locked}

Under `peer_auth` you can lock a project. A locked project is closed to every
agent, its own included, until you approve one:

- in a terminal of your own, with `seam approve` and your password;
- in the console, with a passkey;
- with Touch ID, through the optional Seamless Approve app on a Mac with Apple
  silicon.

An approval is a lease on that agent's process tree, as the kernel reports it,
for the session, an hour or four hours, capped at eight hours unless you
change the cap. The Touch ID prompt is built from the daemon's facts, never
from the agent's own words, so a request cannot dress itself up as something
else ([locked projects](/guides/peer-auth/)).

### 5. Restricted agent accounts {#restricted}

Run unattended agents as an operating-system account of their own and list it
under `peer_auth.restricted`. That account reaches the open, unlocked projects,
or only the ones you name. It reads global memories without writing them,
cannot create projects or ask for access, and its changes to binding rules wait
for you on the Held screen
([restricted local accounts](/guides/peer-auth/#restricted-local-accounts)).

## Your other computers {#networked}

Networked mode shares one daemon across your computers with no service in
between ([networked Seamless](/concepts/networked/)):

- The **Server** gets a name and a certificate at setup and serves https, TLS
  1.2 or later.
- A **Client** checks the Server's certificate fingerprint, then pairs with a
  one-time code: 80 bits, ten minutes, used once, stored hashed. Pairing over
  plain http is refused.
- On macOS and Linux the Client runs a **relay** that pins the Server's exact
  certificate, holds a client certificate of its own, and reports each local
  caller's account and process chain. A Windows Client holds a key of its own
  and never opens a locked project.
- Every paired device can be revoked on the console's **Devices** page.

Your own network or tailnet is the boundary. The public internet is not a
supported one.

## Releases and updates {#releases-and-updates}

- The installers check every archive against the release's `checksums.txt`,
  and that file against the maintainer's hardware-key signature
  (`checksums.txt.sshsig`, on every release after v0.8.3) using the keys they
  carry. With `cosign` installed they also verify its Sigstore signature.
- `seamlessd update`, and every automatic update, verifies the release's
  installer and `checksums.txt` against their Sigstore bundles in-process,
  pinned to this repository's release workflow at that exact tag and to a
  trusted root built into the binary, before it runs anything.
- macOS binaries are signed with a Developer ID and notarized by Apple.
- An automatic update waits a day after a release is published, backs up
  first, and rolls back if the new version does not come up.
- `update.release_signers` pins the maintainer's keys, so that every later
  update needs their signature as well
  ([owner-signed releases](/updating/#owner-signed-releases)).

A fresh install trusts the site that serves the installer. Read the script
first if that boundary matters to you.

## For a security monitor {#monitoring}

Two optional outputs let a separate tool watch what agents do without reading
what they wrote: a **metadata feed** of every event to a Unix socket you name,
and a **commit-matching snapshot** of the sessions and tasks that recorded each
commit ([security monitor](/guides/security-monitor/)). Both are off by
default.

## What Seamless does not claim {#limits}

- **No encryption at rest.** Memories and notes are plain markdown, the
  database is plain SQLite, and backups are not encrypted. Disk encryption is
  the operating system's job.
- **Your own account, without layer 3.** Any program running as you can read
  `~/.seamless` and the key, whatever the fences say.
- **Root.** Root, and anyone who can become the daemon's account, reads
  everything. A passwordless `sudo` to it voids the arrangement.
- **What a session already read.** An approval that ends does not unread what
  is in the agent's transcript.
- **Seamless guards its own surfaces.** An agent can still copy what it read
  into a file, a commit or a pull request.
- **Hooks fail open.** A daemon that is down never blocks an agent, so a
  session can start without its briefing.
- **Your model provider.** Text you send for embeddings goes to the provider
  you configure ([above](#outbound)).
- **Windows** has no `peer_auth`, relay or Server role.

## Choosing a setup {#choosing}

| You want | Turn on | Guide |
|---|---|---|
| One person, one computer, ordinary projects | Nothing: a Standalone install | [Quickstart](/quickstart/) |
| Client work that must not leak into other projects | `confidential` or `sealed` on those projects | [Project isolation](/concepts/project-isolation/) |
| Agents that read the web, issues or email | Write screening | [Write screening](/guides/write-screening/) |
| A project even your own agents open only with your approval | `peer_auth`, then lock the project | [peer_auth](/guides/peer-auth/) |
| Unattended agents with a narrower reach | A restricted agent account | [Restricted accounts](/guides/peer-auth/#restricted-local-accounts) |
| One memory across your computers | Networked mode | [Network install](/guides/network-install/) |

## Report a security problem {#report}

Report a vulnerability privately through
[GitHub's private vulnerability reporting](https://github.com/arctop/seamless/security/advisories/new),
not in a public issue. The repository's
[security policy](https://github.com/arctop/seamless/blob/main/SECURITY.md)
says what is in scope.
