---
title: Concepts
description: The mental model behind Seamless - memory with a lifecycle, ambient sessions, one search entry point, a dependency-aware task queue, and a gardener that only proposes.
---

Seamless is built around a small number of load-bearing ideas, and everything
else follows from them. Knowledge lives in markdown files that are the source
of truth, with SQLite as a rebuildable index over them. Memory has a lifecycle:
a memory is never silently rewritten - it is superseded, with provenance
pointing back at what it replaced. And agents do not ask for context; context
arrives ambiently, injected at session start and on matching prompts, before
the agent has done anything at all.

Start with [How Seamless works](/concepts/how-it-works/) for the shape of the
system - one daemon, the surfaces it serves, and the line between files and
database. Then [Memory & notes](/concepts/memory/) explains what actually gets
stored: the eight memory kinds, when a note is the better vehicle, and how
supersession keeps a long-lived store honest instead of letting it silt up.

The middle of the story is how knowledge reaches an agent.
[Sessions & briefings](/concepts/sessions/) covers the ambient side - what a
session is, how the briefing is packed, and what never gets dropped from it.
[Recall](/concepts/recall/) covers the on-demand side: one search entry point
that fuses keyword and vector results, and the three distinct paths by which a
stored fact ends up in front of a model. [Projects & scope](/concepts/projects/)
explains which knowledge is even in play for a given call - the scope
precedence chain, the fail-closed rule, and project families.

The last two pages are about work and upkeep rather than knowledge.
[Tasks & plans](/concepts/tasks-and-plans/) describes the dependency-aware
ready queue, lease-based claiming that lets parallel agents divide work without
colliding, and plans as compositions of notes and tasks rather than a separate
primitive. [The gardener](/concepts/gardener/) is the background pass that
finds duplicates, staleness, and drift - and only ever proposes, because
nothing in Seamless rewrites your knowledge behind your back.

[Networked Seamless](/concepts/networked/) is the shape for more than one
computer: one Server keeps the data, and the others connect to it as Clients,
through a relay or with a key.
