# Concepts

> The mental model behind Seamless - memory with a lifecycle, ambient sessions, one search entry point, a dependency-aware task queue, and a gardener that only proposes.

Seamless is built around a small number of load-bearing ideas, and everything
else follows from them. Knowledge lives in markdown files that are the source
of truth, with SQLite as a rebuildable index over them. Memory has a lifecycle:
a memory is never silently rewritten - it is superseded, with provenance
pointing back at what it replaced. And agents do not ask for context; context
arrives ambiently, injected at session start and on matching prompts, before
the agent has done anything at all.

Start with [How Seamless works](https://thereisnospoon.org/docs/concepts/how-it-works/) for the shape of the
system - one daemon, the surfaces it serves, and the line between files and
database. Then [Memory & notes](https://thereisnospoon.org/docs/concepts/memory/) explains what actually gets
stored: the eight memory kinds, when a note is the better vehicle, and how
supersession keeps a long-lived store honest instead of letting it silt up.

The middle of the story is how knowledge reaches an agent.
[Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/) covers the ambient side - what a
session is, how the briefing is packed, and what never gets dropped from it.
[Recall](https://thereisnospoon.org/docs/concepts/recall/) covers the on-demand side: one search entry point
that fuses keyword and vector results, and the three distinct paths by which a
stored fact ends up in front of a model. [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/)
explains which knowledge is even in play for a given call - the scope
precedence chain, the fail-closed rule, and project families.

The last two pages are about work and upkeep rather than knowledge.
[Tasks & plans](https://thereisnospoon.org/docs/concepts/tasks-and-plans/) describes the dependency-aware
ready queue, lease-based claiming that lets parallel agents divide work without
colliding, and plans as compositions of notes and tasks rather than a separate
primitive. [The gardener](https://thereisnospoon.org/docs/concepts/gardener/) is the background pass that
finds duplicates, staleness, and drift - and only ever proposes, because
nothing in Seamless rewrites your knowledge behind your back.

[Networked Seamless](https://thereisnospoon.org/docs/concepts/networked/) is the shape for more than one
computer: one Server keeps the data, and the others connect to it as Clients,
through a relay or with a key.

- [How Seamless works](https://thereisnospoon.org/docs/concepts/how-it-works/): One daemon, three surfaces, and a hard line between the files that are truth and the database that is an index.
- [Memory & notes](https://thereisnospoon.org/docs/concepts/memory/): What a memory is, the nine kinds, how supersession keeps the store honest, and when to write a note instead.
- [Sessions & briefings](https://thereisnospoon.org/docs/concepts/sessions/): How an agent gets your knowledge injected before it does anything - ambient sessions, how its tool calls bind to them, the briefing's packing order, and what never gets dropped.
- [Recall](https://thereisnospoon.org/docs/concepts/recall/): One search entry point fusing keyword and vector search - and the three different ways your knowledge actually reaches an agent.
- [Tasks & plans](https://thereisnospoon.org/docs/concepts/tasks-and-plans/): The dependency-aware ready queue, lease-based claiming that lets parallel agents divide work, and plans as compositions rather than primitives.
- [Projects & scope](https://thereisnospoon.org/docs/concepts/projects/): How Seamless decides which project a call belongs to - the precedence chain, the fail-closed rule, and project families.
- [Project isolation](https://thereisnospoon.org/docs/concepts/project-isolation/): Confidential and sealed projects - what each state promises, how tightening works, and the limits of the fence.
- [Networked Seamless](https://thereisnospoon.org/docs/concepts/networked/): One computer keeps the data and the others connect to it - the Server and its Clients, the relay, pairing, approvals from any browser, and what stops when the Server is out of reach.
- [The gardener](https://thereisnospoon.org/docs/concepts/gardener/): The background pass that finds duplicates, staleness, and drift - and proposes, because nothing rewrites your knowledge behind your back.
- [Memory supersession](https://thereisnospoon.org/docs/concepts/memory-supersession/): How an agent memory store stays true instead of just growing - new knowledge explicitly replaces old, with provenance, unlike decay scores or append-only logs.
- [Lease-based task claiming](https://thereisnospoon.org/docs/concepts/lease-based-task-claiming/): The coordination primitive that lets parallel AI agents share one task queue - an atomic claim with an expiry, so two agents never work the same task and a crashed agent never strands one.
- [Reciprocal rank fusion for agent recall](https://thereisnospoon.org/docs/concepts/reciprocal-rank-fusion/): How RRF merges keyword and vector search into one ranked list by rank, not score - and how Seamless runs both legs in a single SQLite file with no vector database.
- [What is a coordination substrate?](https://thereisnospoon.org/docs/concepts/coordination-substrate/): A definition - the durable, shared layer underneath a fleet of AI agents where memory, tasks, and plans live, so agents in different processes, sessions, and clients can act like one team.
