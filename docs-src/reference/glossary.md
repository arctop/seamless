---
title: Glossary
description: The vocabulary, with the distinctions that actually matter - memory vs note vs finding, briefing vs recall, archive vs supersede vs delete.
---

Terms are listed alphabetically. The ones worth reading even if you think you
know them are the four **disambiguation** entries at the end: each is a pair
people routinely use interchangeably, and each pair means genuinely different
things.

## A–Z

**Agent process** - the `claude` or `codex` process that runs an agent's hooks
and launches its MCP transports. The SessionStart hook stamps its identity on
the ambient session, and the agent's MCP requests name it, which binds the
agent's tool calls to that session with no `session_start`. See [Sessions &
briefings](/concepts/sessions/#process-binding).

**Ambient session** - a Seamless session opened automatically by a Claude Code
or Codex SessionStart hook, without the agent asking, displayed with an opaque
`cc/...` or `cx/...` handle. Lifecycle identity uses the full external session
ID plus client, not the display handle. Contrast *explicit session*.

**Approver** - a credential that signs your approval of an agent's access to a
*locked project*: a passkey, or on a Mac a Touch ID key kept by the Seamless
Approve app. Each is enrolled once, with a one-time code only the daemon's
account can mint. None is required: `seam approve` in your own terminal
approves with `sudo` as the proof. See [Enroll an
approver](/guides/peer-auth/#enroll-an-approver).

**Archive** - marking a Seamless memory invalid because it is no longer
relevant: it leaves the indexes and stays readable. The gardener's staleness
pass proposes it, and never for a `constraint` or a pinned `stage`.

**Backup** - one archive of a whole Seamless instance, of the kind `seamlessd
export` writes: every memory and note, a snapshot of the database, and a list
of what each project holds. A release install takes one every day and keeps
the newest 7 in `~/.seamless/backups`; **Settings > Backups** in the console
lists and restores them. See [Import, back up & restore](/guides/data/).

**Binding** - the association between an MCP connection and a Seamless session,
set by `session_start` or, for a Claude Code or Codex agent, automatically
through its *agent process*; an explicit `session_start` wins. Everything on
that connection inherits the bound session's project.

**Briefing** - the `<seam-briefing>` context block Seamless injects into an
agent at session start - constraints, pinned stages, plan rollups, the memory
index, recent findings - assembled inside a token budget. See [Sessions &
briefings](/concepts/sessions/).

**Claim** - an atomic, leased hold on a Seamless task, taken with `tasks_claim`;
exactly one agent can hold a live claim.

**Client** - in a networked Seamless setup, a computer whose agents use the
*Server*'s daemon and data. It runs no daemon and keeps no data of its own, and
reaches the Server through a *relay* or with a key. See [Networked
Seamless](/concepts/networked/).

**Confidential** - the *isolation* state in which nothing leaves a Seamless
project: sessions bound elsewhere cannot read it, and its own sessions cannot
write outside it. Its agents still read global memories and open projects.

**Console** - Seamless's read-mostly web UI at `/console`, an observability
surface for the human owner; agents use MCP.

**Constraint** - the Seamless memory kind for a rule the project cannot
violate; pinned into every briefing, never dropped for budget, never
staleness-archived.

**Description** - the one-line summary in a Seamless memory's frontmatter, the
**only** text shown in any index and therefore the entire retrieval surface.
See [Write memories that get recalled](/guides/write-good-memories/).

**Digest** - a note summarizing a Seamless project's recent activity, proposed
by the gardener.

**Explicit session** - a Seamless session opened by calling `session_start`.
Without a `name`, the call adopts the calling agent's existing ambient session
rather than opening a second one; a Claude Code or Codex agent that names a
different project gets a separate session there instead.

**Family** - a set of Seamless projects related by parent/child, so a child's
briefing can carry the parent's memories and a sibling's recent findings.

**Fail closed** - the Seamless rule that a durable write with no resolvable
scope is rejected rather than defaulted to global. See [Projects &
scope](/concepts/projects/).

**Fail open** - the Seamless rule that a hook never blocks an agent: an
internal error still returns success. The cost is that failure is silent.

**Finding** - what a Seamless session learned, passed to `session_end` and
surfaced in later briefings. Not a memory: a finding is what *happened*, a
memory is what is *true*.

**FTS5** - SQLite's built-in full-text search engine, the keyword half of
Seamless recall.

**Gardener** - the Seamless background pass that finds duplicates, staleness,
and drift and **proposes** fixes; it never acts on its own. See [The
gardener](/concepts/gardener/).

**Gate** - what a Seamless task waits on outside Seamless: who must act and
what must happen. A step whose own gate is open, or that a gate task (`kind:
gate`) blocks, is not ready; a person's action clears it. See [Tasks &
plans](/concepts/tasks-and-plans/#gates).

**Global** - the Seamless scope with no project, visible to every agent in
every repo. A write lands there when it passes `project: global`, or when it
comes from a session bound to the global scope: a `session_start` with neither
`project` nor `cwd`, or a session started outside any git repo. A write with no
scope at all is refused rather than defaulted here (see *fail closed*).

**Held write** - an agent write that *write screening* kept out of the corpus:
one that failed a screen, a change to a binding rule from a session that read
outside text, or a supersede, delete or demotion of a constraint. It waits in a
queue in the database, never under `memory/` or `notes/`, until you release or
discard it on the console's **Held** screen. The agent is told it is held.

**Isolation** - a Seamless project's fence: `open` (the default),
*confidential* (nothing leaves) or *sealed* (nothing enters or leaves). It
governs what flows in and out of a project; whether even its own agents need
your approval first is the separate *locked project* setting. See [Project
isolation](/concepts/project-isolation/).

**Kind** - a Seamless memory's type: `constraint`, `convention`, `runbook`,
`protocol`, `gotcha`, `decision`, `refuted`, `reference`, or `stage`. See
[Memory & notes](/concepts/memory/).

**Lab** - a shared Seamless workspace for a systematic investigation, holding
trials; opened with `lab_open`.

**Lease** - a time limit on something held in Seamless. A task claim's lease
(default 900 seconds) and a *repo lock*'s are refreshed by claiming or
acquiring again, and once one runs out the task or lock is free to take, so a
crashed agent strands nothing. Under *peer_auth*, your approval of an agent's
access to a *locked project* is a lease too: it covers that agent's process
tree until it expires, its session ends or you revoke it, and nothing extends
it. See [Lease-based task claiming](/concepts/lease-based-task-claiming/) and
[Leases and the process tree](/guides/peer-auth/#leases-and-the-process-tree).

**Locked project** - a Seamless project that stays closed to every agent of
yours until you approve one agent's access, for a limited time, with a proof
the agent cannot produce: Touch ID, a passkey, or your own `sudo`. It needs
*peer_auth* and is also confidential or sealed. You lock one from its Overview
with a passkey, or force it in the config file (`peer_auth.force_locked`). See
[Lock projects behind your approval](/guides/peer-auth/).

**Memory** - in Seamless, a markdown file with YAML frontmatter holding one
durable piece of knowledge; the unit that reaches briefings.

**Note** - in Seamless, a markdown file holding a work artifact - research
findings, a meeting summary, a design record; found via recall, never injected
into a briefing.

**Open** - the default *isolation* state: the project shares normally, global
knowledge flows in, and family surfaces may include it.

**Owner signature** - the second signature on a Seamless release,
`checksums.txt.sshsig`, made by the owner with a hardware security key before
the release is published. The installers check it on every release that has
one; the updater checks it only when `update.release_signers` pins the owner's
keys. See [Owner-signed releases](/updating/#owner-signed-releases).

**Pairing** - how a *Client* gets a credential of its own from the *Server*: a
one-time code from the console's Devices page (or `seamlessd devices code`),
after which the computer holds a key, or for a *relay* a TLS client
certificate, that you can revoke. Under *peer_auth* the code alone pairs
nothing until you approve the computer with a passkey. See
[Pairing](/concepts/networked/#pairing).

**peer_auth** - the optional Seamless deployment in which the daemon runs as an
OS account of its own and names each caller by the uid the kernel reports for
its Unix-socket connection, never by anything the caller sends. *Locked
projects* and *restricted local accounts* rest on it. macOS and Linux only, and
off by default. See [Lock projects behind your approval](/guides/peer-auth/).

**Plan** - in Seamless, not a primitive but a composition keyed by
`plan:<slug>`: a narrative note, supporting notes, and step tasks. See [Tasks &
plans](/concepts/tasks-and-plans/).

**Project** - the scope a Seamless memory, note, task, or session belongs to;
resolved from an explicit argument, a bound session, or the agent's cwd.

**Proposal** - the Seamless gardener's output: a suggestion for the owner to
review, applied only when someone decides to, with `gardener_apply` or from the
console's Gardener inbox.

**Provenance** - the Seamless record of where knowledge came from and what
replaced it: `source_session`, `superseded_by`, `invalid_at`.

**Ready** - a Seamless task with no unfinished blocker and no open gate;
`tasks_ready` returns exactly those.

**Recall** - Seamless's single search entry point, fusing FTS5 keyword matching
and vector similarity with RRF. Also, loosely, the `<seam-recall>` block
injected on prompt match - see the disambiguation below.

**Relay** - the small daemon on a macOS or Linux *Client*, run by an account of
its own, that serves the local socket its agents dial and forwards each request
to the *Server* over TLS with its own client certificate, carrying the caller's
uid and process chain. A computer with a relay holds no bearer key. See [The
relay](/concepts/networked/#the-relay).

**Repo lock** - a Seamless lock on a repository's refs (its main branch, or a
tag pattern), declared once per repository and name and held with a lease by
one session while it lands changes. It belongs to the repository, never to a
project, and is always named with it: `LOCK seamless main`. Sessions waiting
for a busy one queue for it and take it first come, first served
(`seam lock acquire --wait`). An optional feature, off by default.

**Reproject** - moving a Seamless memory to a different project that **already
exists**; moving it to one that does not is a *split*.

**Restricted local account** - a second OS account whose agents work beside
yours under *peer_auth*, listed in `peer_auth.restricted`. It reaches every
open project that is not locked, reads the global scope without writing it, and
everything it writes is labeled with its name. The console's Privacy screen
calls it a "Restricted agent account". See [Restricted local
accounts](/guides/peer-auth/#restricted-local-accounts).

**RRF (reciprocal rank fusion)** - the method Seamless recall uses to combine
the keyword and vector rankings so neither retriever gets a veto.

**Sealed** - the *isolation* state in which nothing enters or leaves a Seamless
project: *confidential* plus no global memories, no family, and no
cross-project reads in either direction. Its agents are told the mode in their
briefing.

**Server** - in a networked Seamless setup, the one computer that runs the
daemon and keeps the data for every computer; its *Clients* reach it over the
network. macOS or Linux, never Windows. See [Networked
Seamless](/concepts/networked/).

**Session** - one agent's stretch of work in Seamless. Sessions heartbeat; an
idle one is reaped and marked `expired`.

**Split** - dividing one Seamless project into new child projects, creating
them and a shared parent; planned as a unit by `gardener_split`.

**Stage** - the Seamless memory kind recording where multi-session work stands.
It is pinned into briefings while its `Status:` header names a live gate
(`open`, `in_progress` or `blocked`); `done` unpins it, and a stage with no
readable header leaves the briefing after a grace window. See [Memory &
notes](/concepts/memory/).

**Standalone** - the default Seamless setup: the daemon, the data and the
agents that use them, all on one computer. Contrast *Server* and *Client*.

**Supersede** - replacing an outdated Seamless memory with a new one: the old
is marked invalid, leaves the indexes, and stays readable pointing at its
replacement.

**Trial** - one attempt recorded in a Seamless lab: what was tried, what was
expected, what happened.

**ULID** - the id format Seamless uses everywhere, sortable by creation time.
Never UUID.

**Write screening** - the optional Seamless feature that screens every agent
write to a memory or note for secrets, injection markers, hidden characters,
encoded blobs and unlisted links, and keeps the suspicious ones as *held
writes* for you to release. Off by default. See [Screen writes and hold the
suspicious ones](/guides/write-screening/).

## Four distinctions worth getting right

**Memory vs. note vs. finding.** A *memory* is what is true (injected into
briefings). A *note* is what you produced (found by searching). A *finding* is
what a session learned (surfaced as recent activity). The test for the first
two: would a future agent need this injected before it starts? Then it is a
memory.

**Briefing vs. recall injection vs. recall call.** All three end with the agent
knowing something, which is why they blur. The *briefing* fires at session start
and is unconditional. A *recall injection* fires on prompt match, mid-turn, still
without the agent asking. A *recall call* is the agent choosing to search. The
first two are ambient; only the third is a decision. See
[Recall](/concepts/recall/).

**Ambient vs. explicit session.** *Ambient* is opened by the hook, per agent,
automatically, and the agent's tool calls are bound to it without asking.
*Explicit* is `session_start`, which **adopts** the agent's own ambient session
rather than creating a rival - so a Claude Code or Codex agent needs it only to
move to another project or resume a named session.

**Archive vs. supersede vs. delete.** *Archive*: no longer relevant, marked
invalid, still readable. *Supersede*: replaced by something specific, marked
invalid, still readable, **pointing at its replacement**. *Delete*: gone. The
rule of thumb - delete is for things that were never true; supersede is for
things that stopped being true; archive is for things that stopped mattering.
